package evm

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testRpcServer answers each JSON-RPC call with the result answer gives,
// recording the params of each call.
func testRpcServer(t *testing.T, answer func(method string, params []json.RawMessage) any) (*Client, *[][]json.RawMessage) {
	t.Helper()
	var calls [][]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Id     int64             `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("request: %v", err)
		}
		calls = append(calls, req.Params)
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.Id, "result": answer(req.Method, req.Params)})
	}))
	t.Cleanup(server.Close)
	return NewClient([]string{server.URL}, 5*time.Second), &calls
}

// APPS-SPEC claim rule 3: the preflight is an eth_call from the sender at
// latest, and its return data comes back.
func TestEthCallFrom(t *testing.T) {
	client, calls := testRpcServer(t, func(method string, params []json.RawMessage) any {
		if method != "eth_call" {
			t.Errorf("method %s", method)
		}
		return "0x" + strings.Repeat("01", 32)
	})
	from, to := [20]byte{19: 1}, [20]byte{19: 2}
	ret, err := client.EthCallFrom(context.Background(), from, to, []byte{0xb7, 0xa6})
	if err != nil || hex.EncodeToString(ret) != strings.Repeat("01", 32) {
		t.Fatalf("return %x, %v", ret, err)
	}
	var msg map[string]string
	if err := json.Unmarshal((*calls)[0][0], &msg); err != nil {
		t.Fatal(err)
	}
	if msg["from"] != AddressHex(from) || msg["to"] != AddressHex(to) || msg["data"] != "0xb7a6" || string((*calls)[0][1]) != `"latest"` {
		t.Fatalf("call %s %s", (*calls)[0][0], (*calls)[0][1])
	}

	failing, _ := testRpcServer(t, func(string, []json.RawMessage) any { return 7 })
	if _, err := failing.EthCallFrom(context.Background(), from, to, nil); err == nil {
		t.Fatal("a return that is not a hex string was accepted")
	}
	unreachable := NewClient(nil, time.Second)
	if _, err := unreachable.EthCallFrom(context.Background(), from, to, nil); err == nil {
		t.Fatal("a call without endpoints succeeded")
	}
}

// APPS-SPEC claim rule 5: a receipt carries its logs with their address,
// topics and data; a log whose fields do not parse is left out.
func TestTransactionReceiptLogs(t *testing.T) {
	topic := "0x" + strings.Repeat("ea", 32)
	client, _ := testRpcServer(t, func(string, []json.RawMessage) any {
		return map[string]any{
			"transactionHash": "0xAB", "status": "0x1", "blockNumber": "0x10", "gasUsed": "0x5208",
			"logs": []any{
				map[string]any{"address": "0x" + strings.Repeat("ab", 20), "topics": []string{topic, "0x" + strings.Repeat("00", 31) + "07"}, "data": "0x0102"},
				map[string]any{"address": "nope", "topics": []string{topic}, "data": "0x"},
				map[string]any{"address": "0x" + strings.Repeat("ab", 20), "topics": []string{"0x1234"}, "data": "0x"},
				map[string]any{"address": "0x" + strings.Repeat("ab", 20), "topics": []string{topic}, "data": "0xzz"},
				map[string]any{"address": "0x" + strings.Repeat("cd", 20), "topics": []string{}, "data": "0x"},
			},
		}
	})
	receipt, err := client.TransactionReceipt(context.Background(), "0xab")
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Status || receipt.TxHash != "0xab" || len(receipt.Logs) != 2 {
		t.Fatalf("receipt %+v", receipt)
	}
	first := receipt.Logs[0]
	if first.Address != [20]byte(mustHexBytes(t, strings.Repeat("ab", 20))) || len(first.Topics) != 2 || first.Topics[0] != [32]byte(mustHexBytes(t, topic)) ||
		first.Topics[1] != uint64Word(7) || hex.EncodeToString(first.Data) != "0102" {
		t.Fatalf("first log %+v", first)
	}
	if second := receipt.Logs[1]; second.Address != [20]byte(mustHexBytes(t, strings.Repeat("cd", 20))) || len(second.Topics) != 0 || len(second.Data) != 0 {
		t.Fatalf("second log %+v", second)
	}
}
