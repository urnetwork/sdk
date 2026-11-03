//go:build !ios_extension && !js

package sdk

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/blake2b"

	"github.com/urnetwork/sdk/internal/subtensor"
	"github.com/urnetwork/sdk/sn/evm"
	"github.com/urnetwork/sdk/sn/ss58"
)

// A fake chain answering the sdk's JSON-RPC from the real runtime 486 metadata:
// no network, no clock, no randomness. Every test starts with a key that never
// held TAO and has 5 SN25α on the escrow hotkey.
type convertFakeChain struct {
	t        *testing.T
	metadata []byte

	genesis [32]byte
	escrow  [32]byte
	head    uint64
	spec    uint32

	stake        uint64
	free         uint64
	frozen       uint64
	nonce        uint32
	price        uint64
	nominatorMin uint64
	fee          uint64
	registerFee  uint64
	burn         uint64
	// the pool's own fee, in parts of 65535, and how deep it is for slippage
	poolFeeParts uint64
	poolDepth    uint64
	simulateZero bool
	// the same, from this amount up
	simulateZeroFrom uint64

	submitted [][]byte
	refuse    error
	// what the block after submission holds for the transaction
	landEvents   func(index uint32) string
	noLanding    bool
	eventsAnswer map[uint64]string
	landedAt     map[uint64][]string
	reads        map[string]int
}

func convertTestMetadata(t *testing.T) []byte {
	t.Helper()
	encoded, err := os.ReadFile("internal/subtensor/testdata/runtime486-metadata.scale.gz.base64")
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := base64.StdEncoding.DecodeString(string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newConvertFakeChain(t *testing.T) *convertFakeChain {
	t.Helper()
	return &convertFakeChain{
		t:            t,
		metadata:     convertTestMetadata(t),
		genesis:      bittensorMainnetPins.genesisHash,
		escrow:       bittensorMainnetPins.escrowHotkey,
		head:         9_422_016,
		spec:         486,
		stake:        5_000_000_000,
		price:        4_153_679,
		nominatorMin: 10_000_000,
		fee:          629_362,
		registerFee:  1_100_000,
		burn:         500_000,
		poolFeeParts: 33,
		poolDepth:    2_500_000_000_000_000,
		eventsAnswer: map[uint64]string{},
		landedAt:     map[uint64][]string{},
		reads:        map[string]int{},
	}
}

func convertBlockHash(number uint64) [32]byte {
	var hash [32]byte
	hash[0] = 0xb1
	binary.BigEndian.PutUint64(hash[24:], number)
	return hash
}

func convertHexOf(data []byte) string {
	return "0x" + hex.EncodeToString(data)
}

func (self *convertFakeChain) simulate(alpha uint64) []byte {
	out := []byte{}
	if self.simulateZero || (self.simulateZeroFrom != 0 && self.simulateZeroFrom <= alpha) {
		return make([]byte, 48)
	}
	poolFee := alpha * self.poolFeeParts / 65535
	into := alpha - poolFee
	withoutImpact, _ := bittensorMulDiv(into, self.price, 1_000_000_000)
	slippage, _ := bittensorMulDiv(withoutImpact, into, 2*self.poolDepth)
	tao := withoutImpact - slippage
	for _, value := range []uint64{tao, into, 0, poolFee, slippage, 0} {
		out = binary.LittleEndian.AppendUint64(out, value)
	}
	return out
}

func (self *convertFakeChain) stateCall(method string, args []byte) (any, error) {
	self.reads[method] += 1
	switch method {
	case "AccountNonceApi_account_nonce":
		return convertHexOf(binary.LittleEndian.AppendUint32(nil, self.nonce)), nil
	case "StakeInfoRuntimeApi_get_stake_info_for_hotkey_coldkey_netuid":
		out := []byte{1}
		out = append(out, args[:64]...)
		out = convertCompact(out, uint64(binary.LittleEndian.Uint16(args[64:66])))
		out = convertCompact(out, self.stake)
		for range 4 {
			out = convertCompact(out, 0)
		}
		out = append(out, 1)
		return convertHexOf(out), nil
	case "SwapRuntimeApi_current_alpha_price":
		return convertHexOf(binary.LittleEndian.AppendUint64(nil, self.price)), nil
	case "SwapRuntimeApi_sim_swap_alpha_for_tao":
		return convertHexOf(self.simulate(binary.LittleEndian.Uint64(args[2:10]))), nil
	case "TransactionPaymentApi_query_info":
		out := convertCompact(nil, 2_472_000_000)
		out = convertCompact(out, 70_000)
		out = append(out, 0)
		fee := self.fee
		// the call starts after the compact length, version, address, signature and extras
		if bytes.Contains(args, []byte{7, 134, 25, 0}) {
			fee = self.registerFee
		}
		return convertHexOf(binary.LittleEndian.AppendUint64(out, fee)), nil
	default:
		return nil, fmt.Errorf("no answer for state_call %s", method)
	}
}

// Enough of compact integers for a fake, written apart from the sdk's.
func convertCompact(out []byte, value uint64) []byte {
	switch {
	case value < 64:
		return append(out, byte(value<<2))
	case value < 1<<14:
		return binary.LittleEndian.AppendUint16(out, uint16(value<<2|1))
	case value < 1<<30:
		return binary.LittleEndian.AppendUint32(out, uint32(value<<2|2))
	}
	raw := binary.LittleEndian.AppendUint64(nil, value)
	for len(raw) > 4 && raw[len(raw)-1] == 0 {
		raw = raw[:len(raw)-1]
	}
	out = append(out, byte((len(raw)-4)<<2|3))
	return append(out, raw...)
}

func (self *convertFakeChain) account() any {
	if self.free == 0 && self.frozen == 0 {
		return nil
	}
	out := []byte{}
	for _, value := range []uint32{self.nonce, 0, 1, 0} {
		out = binary.LittleEndian.AppendUint32(out, value)
	}
	for _, value := range []uint64{self.free, 0, self.frozen} {
		out = binary.LittleEndian.AppendUint64(out, value)
	}
	out = append(out, make([]byte, 16)...)
	return convertHexOf(out)
}

func (self *convertFakeChain) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	value, err := self.answer(method, params)
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func (self *convertFakeChain) answer(method string, params []any) (any, error) {
	self.reads[method] += 1
	blockNumber := func(value any) uint64 {
		data, err := evm.ParseHexBytes(value.(string))
		if err != nil || len(data) != 32 {
			self.t.Fatalf("bad block hash %v", value)
		}
		return binary.BigEndian.Uint64(data[24:])
	}
	switch method {
	case "chain_getBlockHash":
		number := fmt.Sprintf("%v", params[0])
		if number == "0" {
			return convertHexOf(self.genesis[:]), nil
		}
		var parsed uint64
		fmt.Sscan(number, &parsed)
		hash := convertBlockHash(parsed)
		return convertHexOf(hash[:]), nil
	case "chain_getFinalizedHead":
		hash := convertBlockHash(self.head)
		return convertHexOf(hash[:]), nil
	case "chain_getHeader":
		return map[string]string{"number": fmt.Sprintf("0x%x", blockNumber(params[0]))}, nil
	case "state_getRuntimeVersion":
		return map[string]any{"specName": "node-subtensor", "specVersion": self.spec, "transactionVersion": 1}, nil
	case "state_getMetadata":
		return convertHexOf(self.metadata), nil
	case "state_call":
		args, err := evm.ParseHexBytes(params[1].(string))
		if err != nil {
			return nil, err
		}
		return self.stateCall(params[0].(string), args)
	case "state_getStorage":
		key := params[0].(string)
		switch {
		case strings.HasPrefix(key, "0x26aa394eea5630e07c48ae0c9558cef7b99d880ec681799c0cf30e8886371da9"):
			return self.account(), nil
		case strings.HasPrefix(key, "0x26aa394eea5630e07c48ae0c9558cef780d41e5e16056765bc8461851072c9d7"):
			return self.eventsAnswer[blockNumber(params[1])], nil
		case key == "0x658faa385070e074c85bf6b568cf0555600828b736cfd977b24a0d2528a8a2e8":
			return convertHexOf(binary.LittleEndian.AppendUint64(nil, self.nominatorMin)), nil
		case key == "0x658faa385070e074c85bf6b568cf055501be1755d08418802946bca51b6863251900":
			return convertHexOf(binary.LittleEndian.AppendUint64(nil, self.burn)), nil
		}
		return nil, fmt.Errorf("no answer for storage %s", key)
	case "eth_call":
		call := params[0].(map[string]string)
		if call["to"] != evm.AddressHex(bittensorMainnetPins.vault2) || call["data"] != "0x164bf360" {
			return nil, fmt.Errorf("eth_call %v", call)
		}
		return convertHexOf(self.escrow[:]), nil
	case "author_submitExtrinsic":
		raw, err := evm.ParseHexBytes(params[0].(string))
		if err != nil {
			return nil, err
		}
		if self.refuse != nil {
			return nil, self.refuse
		}
		self.submitted = append(self.submitted, raw)
		if !self.noLanding {
			landing := self.head + 1
			self.landedAt[landing] = []string{convertHexOf([]byte{9}), convertHexOf(raw)}
			self.head = landing
			if self.landEvents != nil {
				self.eventsAnswer[landing] = self.landEvents(1)
			}
		}
		hash := blake2b.Sum256(raw)
		return convertHexOf(hash[:]), nil
	case "chain_getBlock":
		return map[string]any{"block": map[string]any{"extrinsics": self.landedAt[blockNumber(params[0])]}}, nil
	default:
		return nil, fmt.Errorf("no answer for %s", method)
	}
}

func (self *convertFakeChain) Batch(ctx context.Context, calls []evm.BatchCall) ([]evm.BatchResult, error) {
	self.reads["batch"] += 1
	results := make([]evm.BatchResult, len(calls))
	for i, call := range calls {
		results[i].Result, results[i].Err = self.Call(ctx, call.Method, call.Params...)
	}
	return results, nil
}

func convertEngine(chain *convertFakeChain) *bittensorChainEngine {
	engine := newBittensorChainEngineWithRpc(chain, bittensorMainnetPins)
	engine.sleep = func(ctx context.Context, wait time.Duration) error {
		chain.head += 1
		return ctx.Err()
	}
	return engine
}

// A key that signs with a fixed signature and remembers what it was asked. The
// real key's signature is covered in bittensor_chain_signer_test.go.
type convertTestSigner struct {
	public   [32]byte
	payloads [][]byte
}

func (self *convertTestSigner) PublicKey() [32]byte {
	return self.public
}

func (self *convertTestSigner) Sign(payload []byte) ([64]byte, error) {
	self.payloads = append(self.payloads, slices.Clone(payload))
	return [64]byte(bytes.Repeat([]byte{0x5a}, 64)), nil
}

// A sheet's run: submit, then wait if it was sent.
type convertTestCallback struct {
	sent      []string
	confirmed []*BittensorConvertReceipt
	failed    [][2]string
}

func (self *convertTestCallback) run(ctx context.Context, engine *bittensorChainEngine, signer subtensor.Signer, estimate *BittensorConvertEstimateResult) {
	submission := engine.convertSubmit(ctx, signer, estimate)
	if submission.Error != nil && submission.TxHash == "" {
		self.failed = append(self.failed, [2]string{submission.Error.Code, submission.Error.Message})
		return
	}
	if submission.Error != nil {
		self.failed = append(self.failed, [2]string{submission.Error.Code, submission.Error.Message})
	}
	self.sent = append(self.sent, submission.TxHash)
	receipt := engine.convertWait(ctx, submission)
	if receipt.Error != nil {
		if submission.Error == nil {
			self.failed = append(self.failed, [2]string{receipt.Error.Code, receipt.Error.Message})
		}
		return
	}
	self.confirmed = append(self.confirmed, receipt)
}

var convertTestColdkey = [32]byte{0xd4, 0x35, 0x93}

func convertEstimateOf(t *testing.T, chain *convertFakeChain, amount int64) *BittensorConvertEstimateResult {
	t.Helper()
	return convertEngine(chain).convertEstimate(context.Background(), convertTestColdkey, amount)
}

func convertWantCode(t *testing.T, result *BittensorConvertEstimateResult, code string) {
	t.Helper()
	if result.Error == nil || result.Error.Code != code {
		t.Fatalf("estimate error %+v, want %s", result.Error, code)
	}
	if result.CanConvert {
		t.Fatal("an estimate with an error can convert")
	}
	if strings.Contains(result.Error.Message, "rpc") || strings.Contains(result.Error.Message, "http") {
		t.Fatalf("a user would read %q", result.Error.Message)
	}
}

// The values below were worked out by hand from the price, the fee and the
// runtime's rules, apart from the code.
func TestConvertEstimateForAKeyWithNoTao(t *testing.T) {
	chain := newConvertFakeChain(t)
	result := convertEstimateOf(t, chain, 0)
	if result.Error != nil {
		t.Fatalf("limits: %+v", result.Error)
	}
	if !result.FeeInAlpha {
		t.Fatal("a key with no TAO pays its fee in alpha")
	}
	if result.CanConvert {
		t.Fatal("an amount of 0 can convert")
	}
	if result.StakeRao != 5_000_000_000 || result.FreeTaoRao != 0 || result.PriceRao != 4_153_679 || result.FeeTaoRao != 629_362 {
		t.Fatalf("read %+v", result)
	}
	// 629,362 rao at the price
	if result.FeeAlphaRao != 151_519_172 {
		t.Fatalf("fee in alpha %d", result.FeeAlphaRao)
	}
	// the stake less the fee at the lowest price an accepted sale can run at,
	// so the fee the runtime takes first can never make the sale fail
	if result.MaxAmountRao != 4_817_446_751 {
		t.Fatalf("max %d", result.MaxAmountRao)
	}
	// pays the runtime's 0.002 TAO floor even at the price limit
	if result.MinAmountRao < 491_477_897 || result.MinAmountRao > 492_000_000 {
		t.Fatalf("min %d", result.MinAmountRao)
	}
	if want, _ := ss58.Encode(convertTestColdkey, 42); result.ColdkeySs58 != want {
		t.Fatalf("coldkey %s", result.ColdkeySs58)
	}
	if want, _ := ss58.Encode(bittensorMainnetPins.escrowHotkey, 42); result.HotkeySs58 != want {
		t.Fatalf("hotkey %s", result.HotkeySs58)
	}

	// enough stake that what the sale leaves is over the nominator minimum
	chain.stake = 10_000_000_000
	amount := convertEstimateOf(t, chain, 1_000_000_000)
	if !amount.CanConvert || amount.Error != nil {
		t.Fatalf("amount: %+v", amount.Error)
	}
	// the amount less the pool's fee, at the price, with a sale too small to move it
	if amount.ExpectedTaoRao != 4_151_587 || amount.ExpectedNetTaoRao != 4_151_587 {
		t.Fatalf("expected %d net %d", amount.ExpectedTaoRao, amount.ExpectedNetTaoRao)
	}
	if amount.PoolFeeAlphaRao != 503_547 {
		t.Fatalf("pool fee %d", amount.PoolFeeAlphaRao)
	}
	// 2% for the price to move
	if amount.PriceImpactBps != 0 || amount.LimitPriceRao != 4_070_605 {
		t.Fatalf("impact %d limit %d", amount.PriceImpactBps, amount.LimitPriceRao)
	}
	if amount.MinTaoRao != 4_068_555 {
		t.Fatalf("min tao %d", amount.MinTaoRao)
	}
	if amount.RemainderSweptAlphaRao != 0 {
		t.Fatalf("swept %d with 8.8 SN25α left, worth more than the nominator minimum", amount.RemainderSweptAlphaRao)
	}
}

// A sheet shows these as they are.
func TestConvertEstimateNotices(t *testing.T) {
	chain := newConvertFakeChain(t)
	chain.stake = 10_000_000_000
	result := convertEstimateOf(t, chain, 1_000_000_000)
	if result.Notice != "This key has no TAO, so the network fee of about 0.1515 SN25α comes out of its SN25α." {
		t.Fatalf("notice %q", result.Notice)
	}
	chain.stake = 3_000_000_000
	result = convertEstimateOf(t, chain, 1_000_000_000)
	if !strings.Contains(result.Notice, "The other 1.8485 SN25α converts too, because the network doesn't keep stake worth less than 0.020000 TAO.") {
		t.Fatalf("notice %q", result.Notice)
	}
	chain.free = 10_000_000
	chain.stake = 10_000_000_000
	if result := convertEstimateOf(t, chain, 1_000_000_000); result.Notice != "" {
		t.Fatalf("notice %q with nothing to say", result.Notice)
	}
	if result := convertEstimateOf(t, chain, 0); result.Notice != "" {
		t.Fatalf("notice %q for the limits alone", result.Notice)
	}
}

func TestConvertEstimateForAKeyThatPaysInTao(t *testing.T) {
	chain := newConvertFakeChain(t)
	chain.free = 10_000_000
	result := convertEstimateOf(t, chain, 5_000_000_000)
	if result.Error != nil {
		t.Fatalf("%+v", result.Error)
	}
	if result.FeeInAlpha {
		t.Fatal("a key with TAO for the fee pays it in TAO")
	}
	if result.MaxAmountRao != 5_000_000_000 {
		t.Fatalf("max %d", result.MaxAmountRao)
	}
	if !result.CanConvert || result.ExpectedNetTaoRao != result.ExpectedTaoRao-629_362 {
		t.Fatalf("%+v", result)
	}
	if result.RemainderSweptAlphaRao != 0 {
		t.Fatalf("nothing is left to sweep: %d", result.RemainderSweptAlphaRao)
	}
}

// The runtime takes a fee in TAO only if the key keeps its deposit, and if it
// cannot it tries the alpha.
func TestConvertFeeIsPaidInAlphaUnlessTheKeyKeepsItsDeposit(t *testing.T) {
	for _, row := range []struct {
		name       string
		free       uint64
		frozen     uint64
		feeInAlpha bool
	}{
		{"nothing", 0, 0, true},
		{"less than the deposit", 499, 0, true},
		{"the fee but not the deposit too", 629_362 + 499, 0, true},
		{"the fee and the deposit", 629_362 + 500, 0, false},
		{"frozen beyond the fee", 1_000_000, 900_000, true},
		{"enough over what is frozen", 1_629_362, 1_000_000, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			chain := newConvertFakeChain(t)
			chain.free, chain.frozen = row.free, row.frozen
			if result := convertEstimateOf(t, chain, 0); result.Error != nil || result.FeeInAlpha != row.feeInAlpha {
				t.Fatalf("fee in alpha %v, want %v, %+v", result.FeeInAlpha, row.feeInAlpha, result.Error)
			}
		})
	}
}

// What a sale leaves under the nominator minimum the runtime sells with it, at
// the market. The estimate says so, and counts it.
func TestConvertEstimateSaysWhatIsSweptAlong(t *testing.T) {
	chain := newConvertFakeChain(t)
	chain.stake = 3_000_000_000
	// the nominator minimum, 0.02 TAO of value, is 4.8150 SN25α at this price
	result := convertEstimateOf(t, chain, 1_000_000_000)
	if result.RemainderSweptAlphaRao != 3_000_000_000-1_000_000_000-151_519_172 {
		t.Fatalf("swept %d", result.RemainderSweptAlphaRao)
	}
	if result.RemainderSweptTaoRao == 0 || result.ExpectedTaoRao <= 4_151_587 {
		t.Fatalf("swept %d, expected %d", result.RemainderSweptTaoRao, result.ExpectedTaoRao)
	}
	if result.ExpectedTaoRao != 4_151_587+result.RemainderSweptTaoRao {
		t.Fatalf("expected %d", result.ExpectedTaoRao)
	}
	if result.MinTaoRao >= 4_151_587 {
		t.Fatalf("the limit covers the sale, not the sweep: %d", result.MinTaoRao)
	}
	if !result.CanConvert {
		t.Fatalf("%+v", result.Error)
	}

	// exactly at the minimum nothing is swept
	chain = newConvertFakeChain(t)
	// 20,000,000 rao at the price, worked out apart from the code
	const minimumLeft = 4_815_008_574
	chain.stake = 1_000_000_000 + 151_519_172 + minimumLeft
	if result := convertEstimateOf(t, chain, 1_000_000_000); result.RemainderSweptAlphaRao != 0 {
		t.Fatalf("swept %d at the minimum", result.RemainderSweptAlphaRao)
	}
	chain.stake -= 1
	if result := convertEstimateOf(t, chain, 1_000_000_000); result.RemainderSweptAlphaRao != minimumLeft-1 {
		t.Fatalf("swept %d just under the minimum", result.RemainderSweptAlphaRao)
	}
}

func TestConvertEstimateRefusals(t *testing.T) {
	t.Run("no stake", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		chain.stake = 0
		convertWantCode(t, convertEstimateOf(t, chain, 0), BittensorConvertErrorNothingToConvert)
		convertWantCode(t, convertEstimateOf(t, chain, 1_000), BittensorConvertErrorNothingToConvert)
	})
	t.Run("less than the fee", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		chain.stake = 100_000_000
		result := convertEstimateOf(t, chain, 0)
		convertWantCode(t, result, BittensorConvertErrorBalanceTooLow)
		if result.MaxAmountRao != 0 || result.MinAmountRao != 0 {
			t.Fatalf("limits %d %d", result.MinAmountRao, result.MaxAmountRao)
		}
	})
	t.Run("too little for the fee and the floor", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		chain.stake = 600_000_000
		convertWantCode(t, convertEstimateOf(t, chain, 0), BittensorConvertErrorBalanceTooLow)
	})
	t.Run("a key with TAO can empty a small position", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		chain.free = 10_000_000
		chain.stake = 100_000_000
		result := convertEstimateOf(t, chain, 100_000_000)
		if !result.CanConvert || result.MinAmountRao != 100_000_000 || result.MaxAmountRao != 100_000_000 {
			t.Fatalf("%+v %+v", result, result.Error)
		}
		convertWantCode(t, convertEstimateOf(t, chain, 50_000_000), BittensorConvertErrorAmountTooLow)
	})
	t.Run("under the minimum", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		result := convertEstimateOf(t, chain, 1_000)
		convertWantCode(t, result, BittensorConvertErrorAmountTooLow)
		if result.MinAmountRao == 0 {
			t.Fatal("a refused amount still shows the limits")
		}
		convertWantCode(t, convertEstimateOf(t, chain, -1), BittensorConvertErrorAmountTooLow)
	})
	t.Run("over what the fee leaves", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		result := convertEstimateOf(t, chain, 5_000_000_000)
		convertWantCode(t, result, BittensorConvertErrorAmountTooHigh)
		if !strings.Contains(result.Error.Message, "4.8174") {
			t.Fatalf("message %q", result.Error.Message)
		}
		convertWantCode(t, convertEstimateOf(t, chain, 4_817_446_752), BittensorConvertErrorAmountTooHigh)
		if amount := convertEstimateOf(t, chain, 4_817_446_751); !amount.CanConvert {
			t.Fatalf("the most it says it can convert: %+v", amount.Error)
		}
	})
	t.Run("over what the key holds", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		chain.free = 10_000_000
		convertWantCode(t, convertEstimateOf(t, chain, 5_000_000_001), BittensorConvertErrorAmountTooHigh)
	})
	t.Run("moves the price too far", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		chain.stake = 1_000_000_000_000_000
		chain.poolDepth = 2_500_000_000_000
		result := convertEstimateOf(t, chain, 900_000_000_000)
		convertWantCode(t, result, BittensorConvertErrorPriceImpact)
		if result.PriceImpactBps <= 500 {
			t.Fatalf("impact %d", result.PriceImpactBps)
		}
	})
	t.Run("the pool cannot run it", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		chain.simulateZero = true
		convertWantCode(t, convertEstimateOf(t, chain, 1_000_000_000), BittensorConvertErrorPriceMoved)
	})
	t.Run("the pool cannot run this much", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		chain.simulateZeroFrom = 2_000_000_000
		if result := convertEstimateOf(t, chain, 1_000_000_000); !result.CanConvert {
			t.Fatalf("%+v", result.Error)
		}
		convertWantCode(t, convertEstimateOf(t, chain, 2_000_000_000), BittensorConvertErrorPriceImpact)
	})
}

// A sale that lands after the price falls still pays the floor: refused there,
// it would keep the fee.
func TestConvertMinimumPaysTheFloorAtTheLimit(t *testing.T) {
	chain := newConvertFakeChain(t)
	result := convertEstimateOf(t, chain, 0)
	if result.Error != nil {
		t.Fatal(result.Error)
	}
	minimum := convertEstimateOf(t, chain, result.MinAmountRao)
	if !minimum.CanConvert {
		t.Fatalf("%+v", minimum.Error)
	}
	if minimum.MinTaoRao < 2_000_000 {
		t.Fatalf("the smallest sale pays %d at its limit, under the 2,000,000 floor", minimum.MinTaoRao)
	}
	below := convertEstimateOf(t, chain, result.MinAmountRao-1)
	convertWantCode(t, below, BittensorConvertErrorAmountTooLow)
}

func TestConvertBurnEstimate(t *testing.T) {
	t.Run("a key with alpha and no TAO converts just enough", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		// a floor under the burn, so the burn decides
		chain.burn = 3_000_000
		result := convertEngine(chain).convertEstimateForBurn(context.Background(), convertTestColdkey)
		if result.Error != nil {
			t.Fatal(result.Error)
		}
		// (burn + register fee) x 1.1, and the deposit
		if result.BurnRao != 3_000_000 || result.RegisterFeeRao != 1_100_000 || result.BurnTaoNeededRao != 4_510_500 {
			t.Fatalf("%+v", result)
		}
		if !result.CanConvert || result.BurnAlreadyCovered {
			t.Fatalf("%+v %+v", result, result.Error)
		}
		// even at its limit the sale pays what the registration needs, and little more
		if result.MinTaoRao < 4_510_500 {
			t.Fatalf("at its limit it pays %d", result.MinTaoRao)
		}
		if result.MinTaoRao > 4_510_500+50_000 {
			t.Fatalf("more than just enough: %d", result.MinTaoRao)
		}
		if result.LimitPriceRao == 0 || result.AmountRao == 0 {
			t.Fatalf("%+v", result)
		}
	})
	t.Run("the burn alone is under the sale floor", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		chain.registerFee = 100_000
		result := convertEngine(chain).convertEstimateForBurn(context.Background(), convertTestColdkey)
		if result.Error != nil || !result.CanConvert {
			t.Fatalf("%+v", result.Error)
		}
		// today's burn is about 0.12 SN25α, but the sale has to pay the 0.002 TAO floor
		if result.MinTaoRao < 2_000_000 || result.AmountRao < 480_000_000 {
			t.Fatalf("amount %d pays %d at its limit", result.AmountRao, result.MinTaoRao)
		}
	})
	t.Run("a key that pays the sale's fee in TAO counts it", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		chain.burn = 3_000_000
		chain.free = 2_000_000
		result := convertEngine(chain).convertEstimateForBurn(context.Background(), convertTestColdkey)
		if result.Error != nil || result.FeeInAlpha {
			t.Fatalf("%+v %+v", result, result.Error)
		}
		// 4,510,500 needed, 2,000,000 held, and the sale's fee on top
		if result.MinTaoRao < 4_510_500-2_000_000+629_362 {
			t.Fatalf("at its limit it pays %d", result.MinTaoRao)
		}
	})
	t.Run("a key that holds enough converts nothing", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		chain.free = 10_000_000
		result := convertEngine(chain).convertEstimateForBurn(context.Background(), convertTestColdkey)
		if result.Error != nil || !result.BurnAlreadyCovered || result.CanConvert || result.AmountRao != 0 {
			t.Fatalf("%+v %+v", result, result.Error)
		}
	})
	t.Run("a key with too little says so", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		chain.stake = 650_000_000
		chain.burn = 3_000_000
		result := convertEngine(chain).convertEstimateForBurn(context.Background(), convertTestColdkey)
		convertWantCode(t, result, BittensorConvertErrorBalanceTooLow)
	})
	t.Run("no alpha", func(t *testing.T) {
		chain := newConvertFakeChain(t)
		chain.stake = 0
		convertWantCode(t, convertEngine(chain).convertEstimateForBurn(context.Background(), convertTestColdkey), BittensorConvertErrorNothingToConvert)
	})
}

// The events of a transaction, as the runtime writes them.
func convertEvents(extrinsic uint32, events ...[]byte) string {
	out := convertCompact(nil, uint64(len(events)))
	for _, event := range events {
		out = append(out, 0)
		out = binary.LittleEndian.AppendUint32(out, extrinsic)
		out = append(out, event...)
		out = append(out, 0)
	}
	return convertHexOf(out)
}

func convertDispatchInfo() []byte {
	out := convertCompact(nil, 1_000)
	out = convertCompact(out, 1_000)
	return append(out, 0, 0)
}

func convertSuccessEvent() []byte {
	return append([]byte{0, 0}, convertDispatchInfo()...)
}

func convertFailedEvent(palletIndex byte, errorIndex byte) []byte {
	out := []byte{0, 1, 3, palletIndex, errorIndex, 0, 0, 0}
	return append(out, convertDispatchInfo()...)
}

func convertStakeRemovedEvent(tao uint64, alpha uint64, poolFee uint64) []byte {
	out := []byte{7, 3}
	out = append(out, convertTestColdkey[:]...)
	out = append(out, bittensorMainnetPins.escrowHotkey[:]...)
	out = binary.LittleEndian.AppendUint64(out, tao)
	out = binary.LittleEndian.AppendUint64(out, alpha)
	out = binary.LittleEndian.AppendUint16(out, 25)
	return binary.LittleEndian.AppendUint64(out, poolFee)
}

func convertFeePaidWithAlphaEvent(alpha uint64, tao uint64) []byte {
	out := []byte{7, 128}
	out = append(out, convertTestColdkey[:]...)
	out = binary.LittleEndian.AppendUint16(out, 25)
	out = binary.LittleEndian.AppendUint64(out, alpha)
	return binary.LittleEndian.AppendUint64(out, tao)
}

func convertTaoFeePaidEvent(fee uint64) []byte {
	out := []byte{6, 0}
	out = append(out, convertTestColdkey[:]...)
	out = binary.LittleEndian.AppendUint64(out, fee)
	return binary.LittleEndian.AppendUint64(out, 0)
}

func convertRun(t *testing.T, chain *convertFakeChain, estimate *BittensorConvertEstimateResult) (*convertTestSigner, *convertTestCallback) {
	t.Helper()
	signer := &convertTestSigner{public: convertTestColdkey}
	callback := &convertTestCallback{}
	callback.run(context.Background(), convertEngine(chain), signer, estimate)
	if len(callback.confirmed)+len(callback.failed) != 1 {
		t.Fatalf("a run ended %d confirmed and %d failed", len(callback.confirmed), len(callback.failed))
	}
	return signer, callback
}

func convertLandSuccess(chain *convertFakeChain) {
	chain.landEvents = func(index uint32) string {
		return convertEvents(index,
			convertStakeRemovedEvent(652_113, 151_519_172, 0),
			convertStakeRemovedEvent(4_151_587, 999_496_453, 503_547),
			convertFeePaidWithAlphaEvent(151_519_172, 640_000),
			convertSuccessEvent(),
		)
	}
}

// What gets signed and sent is exactly the one call, by the exact bytes.
func TestConvertSubmitSignsTheExpectedBytes(t *testing.T) {
	chain := newConvertFakeChain(t)
	convertLandSuccess(chain)
	chain.nonce = 0
	estimate := convertEstimateOf(t, chain, 1_000_000_000)
	birth := chain.head
	birthHash := convertBlockHash(birth)
	signer, callback := convertRun(t, chain, estimate)
	if len(callback.failed) != 0 {
		t.Fatalf("failed %v", callback.failed)
	}

	wantCall := []byte{7, 89}
	wantCall = append(wantCall, bittensorMainnetPins.escrowHotkey[:]...)
	wantCall = append(wantCall, 25, 0)
	wantCall = binary.LittleEndian.AppendUint64(wantCall, 1_000_000_000)
	wantCall = binary.LittleEndian.AppendUint64(wantCall, uint64(estimate.LimitPriceRao))
	wantCall = append(wantCall, 0)

	// a mortal era of 64 at this block's phase, nonce 0, no tip, metadata hash off
	phase := birth % 64
	era := binary.LittleEndian.AppendUint16(nil, uint16(5|phase<<4))
	wantExtra := append(slices.Clone(era), 0, 0, 0)
	wantPayload := slices.Clone(wantCall)
	wantPayload = append(wantPayload, wantExtra...)
	wantPayload = binary.LittleEndian.AppendUint32(wantPayload, 486)
	wantPayload = binary.LittleEndian.AppendUint32(wantPayload, 1)
	wantPayload = append(wantPayload, bittensorMainnetPins.genesisHash[:]...)
	wantPayload = append(wantPayload, birthHash[:]...)
	wantPayload = append(wantPayload, 0)

	if len(signer.payloads) != 1 || !bytes.Equal(signer.payloads[0], wantPayload) {
		t.Fatalf("signed\n %x\nwant\n %x", signer.payloads, wantPayload)
	}

	body := []byte{0x84, 0}
	body = append(body, convertTestColdkey[:]...)
	body = append(body, 1)
	body = append(body, bytes.Repeat([]byte{0x5a}, 64)...)
	body = append(body, wantExtra...)
	body = append(body, wantCall...)
	wantRaw := append(convertCompact(nil, uint64(len(body))), body...)
	if len(chain.submitted) != 1 || !bytes.Equal(chain.submitted[0], wantRaw) {
		t.Fatalf("sent\n %x\nwant\n %x", chain.submitted, wantRaw)
	}

	hash := blake2b.Sum256(wantRaw)
	if len(callback.sent) != 1 || callback.sent[0] != convertHexOf(hash[:]) {
		t.Fatalf("sent callback %v", callback.sent)
	}
	receipt := callback.confirmed[0]
	if receipt.TxHash != callback.sent[0] || receipt.BlockNumber != int64(birth+1) || receipt.ExtrinsicIndex != 1 {
		t.Fatalf("receipt %+v", receipt)
	}
	if receipt.AlphaSoldRao != 1_000_000_000 {
		t.Fatalf("sold %d", receipt.AlphaSoldRao)
	}
	// the sale, and what the fee's sale raised over the final fee
	if receipt.TaoReceivedRao != 4_151_587+12_113 {
		t.Fatalf("received %d", receipt.TaoReceivedRao)
	}
	if receipt.FeeAlphaRao != 151_519_172 || receipt.FeeTaoRao != 640_000 {
		t.Fatalf("fee %+v", receipt)
	}
}

// A key that never held TAO has no account on chain: its nonce is 0 and nothing
// has to exist for this to run.
func TestConvertSubmitFromAKeyWithNoAccount(t *testing.T) {
	chain := newConvertFakeChain(t)
	convertLandSuccess(chain)
	chain.free, chain.frozen, chain.nonce = 0, 0, 0
	estimate := convertEstimateOf(t, chain, 1_000_000_000)
	if !estimate.FeeInAlpha || !estimate.CanConvert {
		t.Fatalf("%+v %+v", estimate, estimate.Error)
	}
	_, callback := convertRun(t, chain, estimate)
	if len(callback.confirmed) != 1 {
		t.Fatalf("failed %v", callback.failed)
	}
}

func TestConvertSubmitWithTaoFee(t *testing.T) {
	chain := newConvertFakeChain(t)
	chain.free = 10_000_000
	chain.nonce = 154
	chain.landEvents = func(index uint32) string {
		return convertEvents(index,
			convertStakeRemovedEvent(4_151_587, 999_496_453, 503_547),
			convertTaoFeePaidEvent(620_000),
			convertSuccessEvent(),
		)
	}
	estimate := convertEstimateOf(t, chain, 1_000_000_000)
	signer, callback := convertRun(t, chain, estimate)
	if len(callback.confirmed) != 1 {
		t.Fatalf("failed %v", callback.failed)
	}
	if receipt := callback.confirmed[0]; receipt.TaoReceivedRao != 4_151_587 || receipt.FeeTaoRao != 620_000 || receipt.FeeAlphaRao != 0 {
		t.Fatalf("receipt %+v", receipt)
	}
	// nonce 154 in two bytes, after the era
	if !bytes.Contains(signer.payloads[0], []byte{0x69, 0x02, 0, 0}) {
		t.Fatalf("payload %x", signer.payloads[0])
	}
}

// The limit a user was shown is the limit on chain, and if the price has already
// passed it nothing is signed: the chain would refuse and keep the fee.
func TestConvertPriceLimitHolds(t *testing.T) {
	chain := newConvertFakeChain(t)
	convertLandSuccess(chain)
	estimate := convertEstimateOf(t, chain, 1_000_000_000)

	t.Run("the price fell past the limit", func(t *testing.T) {
		moved := newConvertFakeChain(t)
		moved.price = uint64(estimate.LimitPriceRao) - 1
		signer, callback := convertRun(t, moved, estimate)
		if len(signer.payloads) != 0 || len(moved.submitted) != 0 {
			t.Fatal("signed or sent past the limit")
		}
		if callback.failed[0][0] != BittensorConvertErrorPriceMoved {
			t.Fatalf("failed %v", callback.failed)
		}
	})
	t.Run("the price fell past the limit and the sale would still pay enough", func(t *testing.T) {
		// the chain refuses by the price, whatever the sale would pay, and keeps the fee
		moved := newConvertFakeChain(t)
		moved.price = uint64(estimate.LimitPriceRao) - 1
		moved.poolFeeParts = 0
		signer, callback := convertRun(t, moved, estimate)
		if len(signer.payloads) != 0 || len(moved.submitted) != 0 {
			t.Fatal("signed or sent past the limit")
		}
		if callback.failed[0][0] != BittensorConvertErrorPriceMoved {
			t.Fatalf("failed %v", callback.failed)
		}
	})
	t.Run("the sale would pay less than was promised", func(t *testing.T) {
		moved := newConvertFakeChain(t)
		moved.price = uint64(estimate.LimitPriceRao)
		// the pool's fee went up, and so the sale pays under the minimum at this price
		moved.poolFeeParts = 6_000
		signer, callback := convertRun(t, moved, estimate)
		if len(signer.payloads) != 0 {
			t.Fatal("signed under the promised minimum")
		}
		if callback.failed[0][0] != BittensorConvertErrorPriceMoved {
			t.Fatalf("failed %v", callback.failed)
		}
	})
	t.Run("the price rose and the limit stays what was shown", func(t *testing.T) {
		moved := newConvertFakeChain(t)
		convertLandSuccess(moved)
		moved.price = uint64(estimate.PriceRao) * 11 / 10
		signer, callback := convertRun(t, moved, estimate)
		if len(callback.confirmed) != 1 {
			t.Fatalf("failed %v", callback.failed)
		}
		limit := binary.LittleEndian.Uint64(signer.payloads[0][2+32+2+8 : 2+32+2+8+8])
		if limit != uint64(estimate.LimitPriceRao) {
			t.Fatalf("signed limit %d, shown %d", limit, estimate.LimitPriceRao)
		}
	})
	t.Run("a limit no estimate would ask for", func(t *testing.T) {
		altered := *estimate
		altered.LimitPriceRao = 1
		fresh := newConvertFakeChain(t)
		signer, callback := convertRun(t, fresh, &altered)
		if len(signer.payloads) != 0 {
			t.Fatal("signed a limit of 1")
		}
		if callback.failed[0][0] != BittensorConvertErrorPriceMoved {
			t.Fatalf("failed %v", callback.failed)
		}
	})
}

// Each of these refuses with nothing signed and nothing sent.
func TestConvertRefusesBeforeSigning(t *testing.T) {
	estimateChain := newConvertFakeChain(t)
	good := convertEstimateOf(t, estimateChain, 1_000_000_000)
	for _, row := range []struct {
		name     string
		estimate func() *BittensorConvertEstimateResult
		change   func(chain *convertFakeChain)
		code     string
	}{
		{name: "no estimate", estimate: func() *BittensorConvertEstimateResult { return nil }, code: BittensorConvertErrorPriceMoved},
		{name: "an estimate that said no", estimate: func() *BittensorConvertEstimateResult {
			altered := *good
			altered.CanConvert = false
			return &altered
		}, code: BittensorConvertErrorPriceMoved},
		{name: "an estimate for another key", estimate: func() *BittensorConvertEstimateResult {
			altered := *good
			altered.ColdkeySs58, _ = ss58.Encode([32]byte{9}, 42)
			return &altered
		}, code: BittensorConvertErrorKeyMismatch},
		{name: "an amount over what can be converted", estimate: func() *BittensorConvertEstimateResult {
			altered := *good
			altered.AmountRao = 5_000_000_000
			return &altered
		}, code: BittensorConvertErrorAmountTooHigh},
		{name: "the stake has gone", change: func(chain *convertFakeChain) { chain.stake = 0 }, code: BittensorConvertErrorNothingToConvert},
		{name: "another chain", change: func(chain *convertFakeChain) { chain.genesis = [32]byte{1} }, code: BittensorChainErrorWrongChain},
		{name: "the vault names another hotkey", change: func(chain *convertFakeChain) { chain.escrow = [32]byte{1} }, code: BittensorChainErrorWrongChain},
		{name: "the runtime renamed the call", change: func(chain *convertFakeChain) {
			chain.metadata = bytes.ReplaceAll(chain.metadata, []byte("remove_stake_limit"), []byte("remove_stake_limiz"))
		}, code: BittensorConvertErrorRuntimeChanged},
		{name: "the runtime renamed an extension", change: func(chain *convertFakeChain) {
			// a shape this sdk has not seen under this name: the nonce's extension is gone
			chain.metadata = bytes.ReplaceAll(chain.metadata, []byte("\x28CheckNonce"), []byte("\x28CheckNonse"))
		}, code: BittensorConvertErrorRuntimeChanged},
	} {
		t.Run(row.name, func(t *testing.T) {
			chain := newConvertFakeChain(t)
			if row.change != nil {
				row.change(chain)
			}
			estimate := good
			if row.estimate != nil {
				estimate = row.estimate()
			}
			signer, callback := convertRun(t, chain, estimate)
			if len(signer.payloads) != 0 || len(chain.submitted) != 0 {
				t.Fatalf("signed %d, sent %d", len(signer.payloads), len(chain.submitted))
			}
			if callback.failed[0][0] != row.code {
				t.Fatalf("failed %v, want %s", callback.failed, row.code)
			}
			if len(callback.sent) != 0 {
				t.Fatal("said sent")
			}
		})
	}
}

// A runtime version that does not match the metadata it comes with is no reason
// to sign, and no reason to report the wrong thing either.
func TestConvertRuntimeVersionMismatchSignsNothing(t *testing.T) {
	good := convertEstimateOf(t, newConvertFakeChain(t), 1_000_000_000)
	chain := newConvertFakeChain(t)
	chain.spec = 487
	signer, callback := convertRun(t, chain, good)
	if len(signer.payloads) != 0 {
		t.Fatal("signed under a mismatched runtime")
	}
	if len(callback.failed) != 1 {
		t.Fatalf("%v", callback.failed)
	}
}

// The settings can name the endpoint, not the chain.
func TestConvertSettingsCannotChooseTheChain(t *testing.T) {
	base := func() *SnChainSettings {
		return DefaultSnChainSettings()
	}
	if _, snErr := newBittensorChainEngine(base()); snErr != nil {
		t.Fatalf("the release's settings: %+v", snErr)
	}
	for _, row := range []struct {
		name     string
		settings func() *SnChainSettings
		code     string
	}{
		{"nil", func() *SnChainSettings { return nil }, SnErrorCodeChainNotConfigured},
		{"no endpoints", func() *SnChainSettings {
			settings := base()
			settings.ClearRpcUrls()
			return settings
		}, SnErrorCodeChainNotConfigured},
		{"testnet", func() *SnChainSettings { return SnTestnetChainSettings() }, BittensorChainErrorWrongChain},
		{"another chain id", func() *SnChainSettings {
			settings := base()
			settings.ChainId = 1
			return settings
		}, BittensorChainErrorWrongChain},
		{"another subnet", func() *SnChainSettings {
			settings := base()
			settings.Netuid = 26
			return settings
		}, BittensorChainErrorWrongChain},
		{"another vault", func() *SnChainSettings {
			settings := base()
			settings.Vault2Address = "0x0000000000000000000000000000000000000001"
			return settings
		}, BittensorChainErrorWrongChain},
		{"no vault", func() *SnChainSettings {
			settings := base()
			settings.Vault2Address = ""
			return settings
		}, BittensorChainErrorWrongChain},
	} {
		t.Run(row.name, func(t *testing.T) {
			engine, snErr := newBittensorChainEngine(row.settings())
			if engine != nil || snErr == nil || snErr.Code != row.code {
				t.Fatalf("%v, %+v, want %s", engine != nil, snErr, row.code)
			}
		})
	}
	settings := base()
	settings.ClearRpcUrls()
	settings.AddRpcUrl("https://another.endpoint.example")
	engine, snErr := newBittensorChainEngine(settings)
	if snErr != nil || engine.pins != bittensorMainnetPins {
		t.Fatalf("another endpoint: %+v", snErr)
	}
}

func TestConvertOutcomes(t *testing.T) {
	for _, row := range []struct {
		name   string
		change func(chain *convertFakeChain)
		code   string
		sent   bool
	}{
		{"the sale hit the limit", func(chain *convertFakeChain) {
			chain.landEvents = func(index uint32) string {
				return convertEvents(index, convertFailedEvent(7, 84))
			}
		}, BittensorConvertErrorPriceMoved, true},
		{"the swap hit the limit", func(chain *convertFakeChain) {
			chain.landEvents = func(index uint32) string {
				return convertEvents(index, convertFailedEvent(28, 3))
			}
		}, BittensorConvertErrorPriceMoved, true},
		{"the fee left too little", func(chain *convertFakeChain) {
			chain.landEvents = func(index uint32) string {
				return convertEvents(index, convertFailedEvent(7, 9))
			}
		}, BittensorConvertErrorAmountTooHigh, true},
		{"under the floor when it landed", func(chain *convertFakeChain) {
			chain.landEvents = func(index uint32) string {
				return convertEvents(index, convertFailedEvent(7, 82))
			}
		}, BittensorConvertErrorAmountTooLow, true},
		{"any other failure", func(chain *convertFakeChain) {
			chain.landEvents = func(index uint32) string {
				return convertEvents(index, convertFailedEvent(7, 83))
			}
		}, BittensorConvertErrorFailed, true},
		{"the era ended", func(chain *convertFakeChain) {
			chain.noLanding = true
		}, BittensorConvertErrorExpired, true},
		{"its events cannot be read", func(chain *convertFakeChain) {
			chain.landEvents = func(index uint32) string {
				return "0x0401"
			}
		}, BittensorConvertErrorOutcomeUnknown, true},
		{"the fee cannot be paid", func(chain *convertFakeChain) {
			chain.refuse = &evm.RpcError{Code: 1010, Message: "Invalid Transaction", Data: json.RawMessage(`"Inability to pay some fees (e.g. account balance too low)"`)}
		}, BittensorConvertErrorBalanceTooLow, false},
		{"the nonce was used", func(chain *convertFakeChain) {
			chain.refuse = &evm.RpcError{Code: 1010, Message: "Invalid Transaction", Data: json.RawMessage(`"Transaction is outdated"`)}
		}, BittensorConvertErrorPending, false},
		{"the pool already has it", func(chain *convertFakeChain) {
			chain.refuse = &evm.RpcError{Code: 1013, Message: "Transaction Already Imported"}
		}, BittensorConvertErrorPending, false},
		{"the pool has one with this nonce", func(chain *convertFakeChain) {
			chain.refuse = &evm.RpcError{Code: 1014, Message: "Priority is too low"}
		}, BittensorConvertErrorPending, false},
		{"the signature does not match the runtime", func(chain *convertFakeChain) {
			chain.refuse = &evm.RpcError{Code: 1010, Message: "Invalid Transaction", Data: json.RawMessage(`"Transaction has a bad signature"`)}
		}, BittensorConvertErrorRuntimeChanged, false},
		{"another refusal", func(chain *convertFakeChain) {
			chain.refuse = &evm.RpcError{Code: 1010, Message: "Invalid Transaction", Data: json.RawMessage(`"Custom error: 3"`)}
		}, BittensorConvertErrorRefused, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			chain := newConvertFakeChain(t)
			row.change(chain)
			estimate := convertEstimateOf(t, chain, 1_000_000_000)
			_, callback := convertRun(t, chain, estimate)
			if len(callback.failed) != 1 || callback.failed[0][0] != row.code {
				t.Fatalf("failed %v, want %s", callback.failed, row.code)
			}
			if (len(callback.sent) == 1) != row.sent {
				t.Fatalf("sent %v, want %v", callback.sent, row.sent)
			}
		})
	}
}

// A sent transaction whose result cannot be read never says it failed.
func TestConvertTransportFailureAfterSendingIsUnknown(t *testing.T) {
	chain := newConvertFakeChain(t)
	chain.noLanding = true
	estimate := convertEstimateOf(t, chain, 1_000_000_000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signer := &convertTestSigner{public: convertTestColdkey}
	callback := &convertTestCallback{}
	engine := newBittensorChainEngineWithRpc(chain, bittensorMainnetPins)
	engine.sleep = func(ctx context.Context, wait time.Duration) error {
		cancel()
		return ctx.Err()
	}
	callback.run(ctx, engine, signer, estimate)
	if len(callback.sent) != 1 || len(callback.failed) != 1 || callback.failed[0][0] != BittensorConvertErrorOutcomeUnknown {
		t.Fatalf("sent %v, failed %v", callback.sent, callback.failed)
	}
}

// An answer lost on the way back from the node does not say it was not sent.
func TestConvertSubmitWhoseAnswerWasLost(t *testing.T) {
	chain := newConvertFakeChain(t)
	convertLandSuccess(chain)
	estimate := convertEstimateOf(t, chain, 1_000_000_000)
	lost := &convertLostAnswerRpc{convertFakeChain: chain}
	engine := newBittensorChainEngineWithRpc(lost, bittensorMainnetPins)
	engine.sleep = convertEngine(chain).sleep
	ctx := context.Background()
	submission := engine.convertSubmit(ctx, &convertTestSigner{public: convertTestColdkey}, estimate)
	if submission.Error == nil || submission.Error.Code != BittensorConvertErrorOutcomeUnknown || submission.TxHash == "" {
		t.Fatalf("submission %+v", submission)
	}
	// the node had taken it, and waiting finds it
	receipt := engine.convertWait(ctx, submission)
	if receipt.Error != nil || receipt.BlockNumber == 0 {
		t.Fatalf("receipt %+v %+v", receipt, receipt.Error)
	}
}

type convertLostAnswerRpc struct {
	*convertFakeChain
}

func (self *convertLostAnswerRpc) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	answer, err := self.convertFakeChain.Call(ctx, method, params...)
	if err == nil && method == "author_submitExtrinsic" {
		return nil, fmt.Errorf("%w: the connection dropped", evm.ErrUnreachable)
	}
	return answer, err
}

// A submission is all an app has to keep to find out later what happened, and what
// is not one refuses.
func TestConvertWaitAfterTheAppWasAway(t *testing.T) {
	chain := newConvertFakeChain(t)
	convertLandSuccess(chain)
	engine := convertEngine(chain)
	ctx := context.Background()
	estimate := engine.convertEstimate(ctx, convertTestColdkey, 1_000_000_000)
	submission := engine.convertSubmit(ctx, &convertTestSigner{public: convertTestColdkey}, estimate)
	if submission.Error != nil {
		t.Fatal(submission.Error)
	}
	encoded, err := json.Marshal(submission)
	if err != nil {
		t.Fatal(err)
	}
	kept := &BittensorConvertSubmission{}
	if err := json.Unmarshal(encoded, kept); err != nil {
		t.Fatal(err)
	}
	fresh := convertEngine(chain)
	for range 2 {
		if receipt := fresh.convertWait(ctx, kept); receipt.Error != nil || receipt.TaoReceivedRao == 0 {
			t.Fatalf("receipt %+v %+v", receipt, receipt.Error)
		}
	}
	for name, bad := range map[string]*BittensorConvertSubmission{
		"nil":             nil,
		"not sent":        {Error: newSnError(BittensorConvertErrorPriceMoved, "no")},
		"a bad hash":      {TxHash: "0x12", ColdkeySs58: kept.ColdkeySs58, FirstBlock: 1, LastBlock: 2},
		"a bad key":       {TxHash: kept.TxHash, ColdkeySs58: "no", FirstBlock: 1, LastBlock: 2},
		"no blocks":       {TxHash: kept.TxHash, ColdkeySs58: kept.ColdkeySs58},
		"backwards":       {TxHash: kept.TxHash, ColdkeySs58: kept.ColdkeySs58, FirstBlock: 5, LastBlock: 4},
		"longer than era": {TxHash: kept.TxHash, ColdkeySs58: kept.ColdkeySs58, FirstBlock: 5, LastBlock: 5 + 65},
	} {
		receipt := fresh.convertWait(ctx, bad)
		if receipt.Error == nil || receipt.Error.Code != BittensorConvertErrorRefused {
			t.Errorf("%s: %+v", name, receipt)
		}
	}
}

// Typing an amount reads the rest once, and signing never trusts what was kept.
func TestConvertReadsAreKeptForATypedAmountOnly(t *testing.T) {
	chain := newConvertFakeChain(t)
	convertLandSuccess(chain)
	engine := convertEngine(chain)
	engine.cacheKey = t.Name()
	now := time.Unix(1_800_000_000, 0)
	engine.now = func() time.Time {
		return now
	}
	ctx := context.Background()
	if result := engine.convertEstimate(ctx, convertTestColdkey, 1_000_000_000); result.Error != nil {
		t.Fatal(result.Error)
	}
	metadataReads, stateReads := chain.reads["state_getMetadata"], chain.reads["batch"]
	if metadataReads != 1 {
		t.Fatalf("metadata read %d times", metadataReads)
	}
	if result := engine.convertEstimate(ctx, convertTestColdkey, 2_000_000_000); result.Error != nil || result.AmountRao != 2_000_000_000 {
		t.Fatalf("%+v", result)
	}
	if chain.reads["state_getMetadata"] != 1 || chain.reads["batch"] != stateReads {
		t.Fatalf("a typed amount read metadata %d, batches %d", chain.reads["state_getMetadata"], chain.reads["batch"])
	}
	// another key reads its own
	if result := engine.convertEstimate(ctx, [32]byte{9}, 1_000_000_000); result.Error != nil {
		t.Fatal(result.Error)
	}
	if chain.reads["batch"] <= stateReads {
		t.Fatal("another key used the first key's state")
	}
	stateReads = chain.reads["batch"]
	if chain.reads["state_getMetadata"] != 1 {
		t.Fatal("another key read the metadata again")
	}
	// a new block's worth of time reads again
	now = now.Add(bittensorConvertStateTtl)
	if result := engine.convertEstimate(ctx, [32]byte{9}, 1_000_000_000); result.Error != nil {
		t.Fatal(result.Error)
	}
	if chain.reads["batch"] <= stateReads {
		t.Fatal("old state was kept past its time")
	}
	// a new runtime reads its metadata, and a mismatch is an error, not old metadata
	chain.spec = 487
	if result := engine.convertEstimateWith(ctx, [32]byte{9}, 1_000_000_000, false); result.Error == nil {
		t.Fatal("old metadata answered for a new runtime")
	}
	if chain.reads["state_getMetadata"] != 2 {
		t.Fatalf("metadata read %d times", chain.reads["state_getMetadata"])
	}
	chain.spec = 486

	// signing reads the metadata itself, cached or not
	estimate := engine.convertEstimate(ctx, convertTestColdkey, 1_000_000_000)
	before := chain.reads["state_getMetadata"]
	callback := &convertTestCallback{}
	callback.run(ctx, engine, &convertTestSigner{public: convertTestColdkey}, estimate)
	if len(callback.confirmed) != 1 {
		t.Fatalf("failed %v", callback.failed)
	}
	if chain.reads["state_getMetadata"] <= before {
		t.Fatal("signed under kept metadata")
	}
	// and what comes next reads what the conversion changed, right away
	chain.stake = 1
	if result := engine.convertEstimate(ctx, convertTestColdkey, 0); result.StakeRao != 1 {
		t.Fatalf("a conversion left the old stake in place: %d", result.StakeRao)
	}
}

// Every sdk file that can reach the encoder, now and later: no exported function
// or method there takes bytes, a call or a pallet, so nothing outside the sdk can
// choose what gets signed.
func TestNothingExportedChoosesACall(t *testing.T) {
	const encoder = `"github.com/urnetwork/sdk/internal/subtensor"`
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	scanned := []string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		importsEncoder := false
		for _, spec := range file.Imports {
			if spec.Path.Value == encoder {
				importsEncoder = true
			}
		}
		if !importsEncoder {
			continue
		}
		scanned = append(scanned, path)
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !function.Name.IsExported() || function.Type.Params == nil {
				continue
			}
			// a signer's own methods, on an unexported type
			if function.Recv != nil {
				if star, ok := function.Recv.List[0].Type.(*ast.StarExpr); ok {
					if name, ok := star.X.(*ast.Ident); ok && !ast.IsExported(name.Name) {
						continue
					}
				}
			}
			for _, param := range function.Type.Params.List {
				if _, isSlice := param.Type.(*ast.ArrayType); isSlice {
					t.Errorf("%s: %s takes a slice", path, function.Name.Name)
				}
				ast.Inspect(param.Type, func(node ast.Node) bool {
					if selector, ok := node.(*ast.SelectorExpr); ok {
						if name, ok := selector.X.(*ast.Ident); ok && name.Name == "subtensor" {
							t.Errorf("%s: %s takes a subtensor.%s", path, function.Name.Name, selector.Sel.Name)
						}
					}
					return true
				})
				for _, name := range param.Names {
					lower := strings.ToLower(name.Name)
					if strings.Contains(lower, "call") && !strings.Contains(lower, "callback") || strings.Contains(lower, "pallet") || strings.Contains(lower, "payload") {
						t.Errorf("%s: %s takes %s", path, function.Name.Name, name.Name)
					}
				}
			}
		}
	}
	for _, want := range []string{"bittensor_chain.go", "bittensor_chain_signer.go", "bittensor_convert.go"} {
		if !slices.Contains(scanned, want) {
			t.Errorf("CONTROL FAILED: %s is not among the files scanned, %v", want, scanned)
		}
	}
}

// One thing can be converted, one way, and the list of what is exported for it is
// here on purpose.
func TestConvertExportsOnlyTypedCalls(t *testing.T) {
	want := map[string]string{
		"BittensorConvertEstimate":        "settings,coldkeySs58,amountRao,callback",
		"BittensorConvertEstimateForBurn": "settings,coldkeySs58,callback",
		"BittensorConvertSubmit":          "settings,mnemonic,estimate,callback",
		"BittensorConvertWait":            "settings,submission,callback",
		"FormatTao":                       "rao",
		"FormatTaoAmount":                 "rao",
	}
	got := map[string]string{}
	types := map[string]string{}
	fileSet := token.NewFileSet()
	for _, path := range []string{"bittensor_convert.go", "bittensor_convert_submit.go", "bittensor_chain.go", "bittensor_chain_signer.go"} {
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !function.Name.IsExported() {
				continue
			}
			if function.Recv != nil {
				switch receiver := function.Recv.List[0].Type.(type) {
				case *ast.StarExpr:
					// methods that make the unexported signer a subtensor.Signer
					if name, ok := receiver.X.(*ast.Ident); ok && !ast.IsExported(name.Name) {
						continue
					}
				case *ast.Ident:
					if !ast.IsExported(receiver.Name) {
						continue
					}
				}
				t.Errorf("exported method %s", function.Name.Name)
				continue
			}
			names := []string{}
			for _, param := range function.Type.Params.List {
				for _, name := range param.Names {
					names = append(names, name.Name)
					types[function.Name.Name+"."+name.Name] = fmt.Sprintf("%T", param.Type)
					if _, isSlice := param.Type.(*ast.ArrayType); isSlice {
						t.Errorf("%s takes a slice: %s", function.Name.Name, name.Name)
					}
				}
			}
			got[function.Name.Name] = strings.Join(names, ",")
		}
	}
	if len(got) != len(want) {
		t.Errorf("exported %v, want %v", got, want)
	}
	for name, params := range want {
		if got[name] != params {
			t.Errorf("%s takes (%s), want (%s)", name, got[name], params)
		}
	}
	if len(types) == 0 {
		t.Fatal("CONTROL FAILED: the scan saw no parameters")
	}
}

// Nothing here may reach the network extension or the browser, and the apps'
// bindings have to have it.
func TestConvertStaysOutOfTheExtensionAndTheBrowser(t *testing.T) {
	const encoder = "github.com/urnetwork/sdk/internal/subtensor"
	for _, row := range []struct {
		name        string
		directory   string
		environment []string
		tags        string
		links       bool
	}{
		{"ios extension", ".", []string{"GOOS=ios", "GOARCH=arm64", "CGO_ENABLED=1"}, "sdk_mobile_bind,ios_extension", false},
		{"macos extension", ".", []string{"GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=1"}, "sdk_mobile_bind,ios_extension", false},
		{"browser", "js", []string{"GOOS=js", "GOARCH=wasm", "CGO_ENABLED=0"}, "", false},
		{"ios app", ".", []string{"GOOS=ios", "GOARCH=arm64", "CGO_ENABLED=1"}, "sdk_mobile_bind", true},
		{"macos app", ".", []string{"GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=1"}, "sdk_mobile_bind", true},
		{"android", ".", []string{"GOOS=android", "GOARCH=arm64", "CGO_ENABLED=1"}, "sdk_mobile_bind", true},
		{"c library", "cgo", []string{"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=1"}, "", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			arguments := []string{"list", "-deps"}
			if row.tags != "" {
				arguments = append(arguments, "-tags="+row.tags)
			}
			command := exec.CommandContext(t.Context(), "go", append(arguments, ".")...)
			command.Dir = row.directory
			command.Env = append(os.Environ(), row.environment...)
			command.Env = append(command.Env, "GOWORK=off")
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("go list: %v\n%s", err, output)
			}
			packages := strings.Fields(string(output))
			if !slices.Contains(packages, "github.com/urnetwork/sdk") {
				t.Fatal("CONTROL FAILED: the closure does not hold the sdk, so this reads nothing real")
			}
			if linked := slices.Contains(packages, encoder); linked != row.links {
				t.Fatalf("%s linked: %v, want %v", encoder, linked, row.links)
			}
		})
	}
}

func TestFormatTao(t *testing.T) {
	for rao, want := range map[int64]string{
		0:               "0.000000",
		500_000:         "0.000500",
		499:             "0.000000",
		500:             "0.000001",
		1_000_000_000:   "1.000000",
		1_999_999_999:   "2.000000",
		-1_500_000_000:  "-1.500000",
		123_456_789_012: "123.456789",
	} {
		if got := FormatTaoAmount(rao); got != want {
			t.Errorf("FormatTaoAmount(%d) = %s, want %s", rao, got, want)
		}
	}
	if got := FormatTao(500_000); got != "0.000500 TAO" {
		t.Errorf("FormatTao = %s", got)
	}
}

// The exported calls answer through their callbacks, and a bad address or bad
// settings needs no chain to say so.
func TestConvertExportsAnswerThroughTheirCallbacks(t *testing.T) {
	type answer struct {
		result *BittensorConvertEstimateResult
	}
	run := func(call func(callback BittensorConvertEstimateCallback)) *BittensorConvertEstimateResult {
		t.Helper()
		results := make(chan answer, 1)
		call(convertEstimateCallbackFunc(func(result *BittensorConvertEstimateResult) {
			results <- answer{result: result}
		}))
		select {
		case got := <-results:
			return got.result
		case <-time.After(30 * time.Second):
			t.Fatal("no answer")
			return nil
		}
	}
	result := run(func(callback BittensorConvertEstimateCallback) {
		BittensorConvertEstimate(DefaultSnChainSettings(), "not an address", 1, callback)
	})
	if result.Error == nil || result.Error.Code != SnErrorCodeInvalidAddress {
		t.Fatalf("%+v", result.Error)
	}
	address, _ := ss58.Encode(convertTestColdkey, 42)
	result = run(func(callback BittensorConvertEstimateCallback) {
		BittensorConvertEstimateForBurn(SnTestnetChainSettings(), address, callback)
	})
	if result.Error == nil || result.Error.Code != BittensorChainErrorWrongChain {
		t.Fatalf("%+v", result.Error)
	}
}

type convertEstimateCallbackFunc func(result *BittensorConvertEstimateResult)

func (self convertEstimateCallbackFunc) Result(result *BittensorConvertEstimateResult, err error) {
	self(result)
}

// What the engine reports for the reads that fail.
func TestConvertReadFailuresAreWordedForAUser(t *testing.T) {
	for _, row := range []struct {
		err  error
		code string
	}{
		{fmt.Errorf("%w: http 429 from https://endpoint.example", evm.ErrUnreachable), SnErrorCodeChainRpcUnreachable},
		{&evm.RpcError{Code: -32000, Message: "something internal at the node"}, SnErrorCodeChainRpcError},
		{&subtensor.RuntimeChangedError{Detail: "the call moved"}, BittensorConvertErrorRuntimeChanged},
		{errBittensorChainWrongChain, BittensorChainErrorWrongChain},
		{errors.New("anything else"), SnErrorCodeChainRpcError},
	} {
		failure := bittensorConvertErrorFromErr(row.err)
		if failure.Code != row.code {
			t.Errorf("%v: code %s, want %s", row.err, failure.Code, row.code)
		}
		for _, detail := range []string{"endpoint.example", "429", "internal", "moved"} {
			if strings.Contains(failure.Message, detail) {
				t.Errorf("%v: a user would read %q", row.err, failure.Message)
			}
		}
	}
}
