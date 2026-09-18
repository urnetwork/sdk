package evm

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// ErrUnreachable wraps transport failures on every configured endpoint.
var ErrUnreachable = errors.New("chain rpc unreachable")

// RpcError is a JSON-RPC level error (the endpoint answered, the call failed).
type RpcError struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *RpcError) Error() string {
	if len(e.Data) > 0 {
		return fmt.Sprintf("rpc error %d: %s (%s)", e.Code, e.Message, string(e.Data))
	}
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// RevertData extracts the revert bytes an endpoint attached to an error, if any.
func (e *RpcError) RevertData() []byte {
	if len(e.Data) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(e.Data, &s); err == nil {
		if b, err := ParseHexBytes(s); err == nil {
			return b
		}
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(e.Data, &obj); err == nil {
		for _, key := range []string{"data", "result", "return"} {
			if s, ok := obj[key].(string); ok {
				if b, err := ParseHexBytes(s); err == nil {
					return b
				}
			}
		}
	}
	return nil
}

// Reason renders the error for a user: decoded revert data when present.
func (e *RpcError) Reason() string {
	if data := e.RevertData(); len(data) > 0 {
		return DecodeRevert(data)
	}
	return e.Message
}

// Client is a JSON-RPC client over one or more endpoints tried in order.
type Client struct {
	urls   []string
	http   *http.Client
	nextId atomic.Int64
}

// NewClient builds a client for the endpoints (first is preferred).
func NewClient(urls []string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		urls: append([]string(nil), urls...),
		http: &http.Client{Timeout: timeout},
	}
}

// Urls returns the configured endpoints.
func (c *Client) Urls() []string {
	return append([]string(nil), c.urls...)
}

type rpcRequest struct {
	Jsonrpc string `json:"jsonrpc"`
	Id      int64  `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	Id     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

func (c *Client) post(ctx context.Context, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	out, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		return nil, fmt.Errorf("http %d from %s: %.200s", res.StatusCode, url, string(out))
	}
	return out, nil
}

// Call performs one JSON-RPC call, failing over to the next endpoint only on
// transport errors. A JSON-RPC error is authoritative and returned as *RpcError.
func (c *Client) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	if len(c.urls) == 0 {
		return nil, fmt.Errorf("%w: no rpc endpoints configured", ErrUnreachable)
	}
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(rpcRequest{Jsonrpc: "2.0", Id: c.nextId.Add(1), Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, url := range c.urls {
		out, err := c.post(ctx, url, body)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		var res rpcResponse
		if err := json.Unmarshal(out, &res); err != nil {
			lastErr = fmt.Errorf("bad rpc response from %s: %w", url, err)
			continue
		}
		if res.Error != nil {
			return nil, &RpcError{Code: res.Error.Code, Message: res.Error.Message, Data: res.Error.Data}
		}
		return res.Result, nil
	}
	return nil, fmt.Errorf("%w: %v", ErrUnreachable, lastErr)
}

// BatchCall is one entry of a JSON-RPC batch.
type BatchCall struct {
	Method string
	Params []any
}

// BatchResult carries one entry's outcome.
type BatchResult struct {
	Result json.RawMessage
	Err    error
}

// Batch sends the calls as one JSON-RPC batch; results align with calls.
func (c *Client) Batch(ctx context.Context, calls []BatchCall) ([]BatchResult, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	if len(c.urls) == 0 {
		return nil, fmt.Errorf("%w: no rpc endpoints configured", ErrUnreachable)
	}
	reqs := make([]rpcRequest, len(calls))
	byId := map[int64]int{}
	for i, call := range calls {
		id := c.nextId.Add(1)
		params := call.Params
		if params == nil {
			params = []any{}
		}
		reqs[i] = rpcRequest{Jsonrpc: "2.0", Id: id, Method: call.Method, Params: params}
		byId[id] = i
	}
	body, err := json.Marshal(reqs)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, url := range c.urls {
		out, err := c.post(ctx, url, body)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		var responses []rpcResponse
		if err := json.Unmarshal(out, &responses); err != nil {
			lastErr = fmt.Errorf("bad rpc batch response from %s: %w", url, err)
			continue
		}
		results := make([]BatchResult, len(calls))
		for i := range results {
			results[i].Err = errors.New("missing batch response")
		}
		for _, res := range responses {
			i, ok := byId[res.Id]
			if !ok {
				continue
			}
			if res.Error != nil {
				results[i] = BatchResult{Err: &RpcError{Code: res.Error.Code, Message: res.Error.Message, Data: res.Error.Data}}
			} else {
				results[i] = BatchResult{Result: res.Result}
			}
		}
		return results, nil
	}
	return nil, fmt.Errorf("%w: %v", ErrUnreachable, lastErr)
}

// HexQuantity renders a JSON-RPC quantity (no leading zeros).
func HexQuantity(v *big.Int) string {
	if v == nil || v.Sign() == 0 {
		return "0x0"
	}
	return "0x" + v.Text(16)
}

// ParseQuantity parses a JSON-RPC quantity string result.
func ParseQuantity(raw json.RawMessage) (*big.Int, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("quantity is not a string: %s", string(raw))
	}
	return ParseUint256(s)
}

func parseBytesResult(raw json.RawMessage) ([]byte, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("data is not a string: %s", string(raw))
	}
	return ParseHexBytes(s)
}

// ChainId calls eth_chainId.
func (c *Client) ChainId(ctx context.Context) (*big.Int, error) {
	raw, err := c.Call(ctx, "eth_chainId")
	if err != nil {
		return nil, err
	}
	return ParseQuantity(raw)
}

// BlockNumber calls eth_blockNumber.
func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	raw, err := c.Call(ctx, "eth_blockNumber")
	if err != nil {
		return 0, err
	}
	v, err := ParseQuantity(raw)
	if err != nil {
		return 0, err
	}
	return v.Uint64(), nil
}

// Balance calls eth_getBalance(address, latest).
func (c *Client) Balance(ctx context.Context, addr [20]byte) (*big.Int, error) {
	raw, err := c.Call(ctx, "eth_getBalance", AddressHex(addr), "latest")
	if err != nil {
		return nil, err
	}
	return ParseQuantity(raw)
}

// Nonce calls eth_getTransactionCount(address, pending).
func (c *Client) Nonce(ctx context.Context, addr [20]byte) (uint64, error) {
	raw, err := c.Call(ctx, "eth_getTransactionCount", AddressHex(addr), "pending")
	if err != nil {
		return 0, err
	}
	v, err := ParseQuantity(raw)
	if err != nil {
		return 0, err
	}
	return v.Uint64(), nil
}

// GasPrice calls eth_gasPrice.
func (c *Client) GasPrice(ctx context.Context) (*big.Int, error) {
	raw, err := c.Call(ctx, "eth_gasPrice")
	if err != nil {
		return nil, err
	}
	return ParseQuantity(raw)
}

// MaxPriorityFeePerGas calls eth_maxPriorityFeePerGas.
func (c *Client) MaxPriorityFeePerGas(ctx context.Context) (*big.Int, error) {
	raw, err := c.Call(ctx, "eth_maxPriorityFeePerGas")
	if err != nil {
		return nil, err
	}
	return ParseQuantity(raw)
}

// LatestBaseFee returns the latest block's baseFeePerGas, or nil when the
// chain does not expose one (legacy pricing only).
func (c *Client) LatestBaseFee(ctx context.Context) (*big.Int, error) {
	raw, err := c.Call(ctx, "eth_getBlockByNumber", "latest", false)
	if err != nil {
		return nil, err
	}
	var block struct {
		BaseFeePerGas string `json:"baseFeePerGas"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, err
	}
	if block.BaseFeePerGas == "" {
		return nil, nil
	}
	return ParseUint256(block.BaseFeePerGas)
}

type callMsg struct {
	From  string `json:"from,omitempty"`
	To    string `json:"to"`
	Data  string `json:"data"`
	Value string `json:"value,omitempty"`
}

// EthCall executes a read-only call at latest.
func (c *Client) EthCall(ctx context.Context, to [20]byte, data []byte) ([]byte, error) {
	raw, err := c.Call(ctx, "eth_call", callMsg{To: AddressHex(to), Data: "0x" + hex.EncodeToString(data)}, "latest")
	if err != nil {
		return nil, err
	}
	return parseBytesResult(raw)
}

// EthCallBatch runs several read-only calls in one round trip.
func (c *Client) EthCallBatch(ctx context.Context, to [20]byte, datas [][]byte) ([][]byte, []error, error) {
	calls := make([]BatchCall, len(datas))
	for i, data := range datas {
		calls[i] = BatchCall{Method: "eth_call", Params: []any{callMsg{To: AddressHex(to), Data: "0x" + hex.EncodeToString(data)}, "latest"}}
	}
	results, err := c.Batch(ctx, calls)
	if err != nil {
		return nil, nil, err
	}
	outs := make([][]byte, len(datas))
	errs := make([]error, len(datas))
	for i, r := range results {
		if r.Err != nil {
			errs[i] = r.Err
			continue
		}
		outs[i], errs[i] = parseBytesResult(r.Result)
	}
	return outs, errs, nil
}

// EstimateGas calls eth_estimateGas for a transaction from `from`.
func (c *Client) EstimateGas(ctx context.Context, from, to [20]byte, data []byte, value *big.Int) (uint64, error) {
	msg := callMsg{From: AddressHex(from), To: AddressHex(to), Data: "0x" + hex.EncodeToString(data)}
	if value != nil && value.Sign() > 0 {
		msg.Value = HexQuantity(value)
	}
	raw, err := c.Call(ctx, "eth_estimateGas", msg)
	if err != nil {
		return 0, err
	}
	v, err := ParseQuantity(raw)
	if err != nil {
		return 0, err
	}
	if !v.IsUint64() {
		return 0, errors.New("gas estimate overflows uint64")
	}
	return v.Uint64(), nil
}

// SendRawTransaction submits a signed transaction and returns its hash.
func (c *Client) SendRawTransaction(ctx context.Context, raw []byte) (string, error) {
	res, err := c.Call(ctx, "eth_sendRawTransaction", "0x"+hex.EncodeToString(raw))
	if err != nil {
		return "", err
	}
	var hash string
	if err := json.Unmarshal(res, &hash); err != nil {
		return "", fmt.Errorf("tx hash is not a string: %s", string(res))
	}
	return strings.ToLower(hash), nil
}

// Receipt is the subset of a transaction receipt the SDK reports.
type Receipt struct {
	TxHash      string
	Status      bool
	BlockNumber uint64
	GasUsed     uint64
}

// TransactionReceipt returns nil, nil while the transaction is pending.
func (c *Client) TransactionReceipt(ctx context.Context, txHash string) (*Receipt, error) {
	raw, err := c.Call(ctx, "eth_getTransactionReceipt", txHash)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var r struct {
		TransactionHash string `json:"transactionHash"`
		Status          string `json:"status"`
		BlockNumber     string `json:"blockNumber"`
		GasUsed         string `json:"gasUsed"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	out := &Receipt{TxHash: strings.ToLower(r.TransactionHash)}
	if r.Status != "" {
		status, err := ParseUint256(r.Status)
		if err != nil {
			return nil, err
		}
		out.Status = status.Sign() != 0
	}
	if r.BlockNumber != "" {
		if n, err := ParseUint256(r.BlockNumber); err == nil && n.IsUint64() {
			out.BlockNumber = n.Uint64()
		}
	}
	if r.GasUsed != "" {
		if n, err := ParseUint256(r.GasUsed); err == nil && n.IsUint64() {
			out.GasUsed = n.Uint64()
		}
	}
	return out, nil
}

// WaitReceipt polls until the receipt lands, the timeout elapses or ctx ends.
func (c *Client) WaitReceipt(ctx context.Context, txHash string, poll, timeout time.Duration) (*Receipt, error) {
	if poll <= 0 {
		poll = 3 * time.Second
	}
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for {
		receipt, err := c.TransactionReceipt(ctx, txHash)
		if err != nil && !errors.Is(err, ErrUnreachable) {
			return nil, err
		}
		if receipt != nil {
			return receipt, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("timed out waiting for the transaction receipt")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}
