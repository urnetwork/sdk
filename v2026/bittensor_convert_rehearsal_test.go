//go:build !ios_extension && !js

package sdk

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/sdk/v2026/internal/subtensor"
	"github.com/urnetwork/sdk/v2026/sn/evm"
	"github.com/urnetwork/sdk/v2026/sn/ss58"
)

// The testnet genesis: pinned here, so this reaches nothing else.
const convertRehearsalTestnetGenesis = "8f9cf856bf558a14440e75569c9e58594757048d7b3a84b5d25f6bd978263105"

// There is no vault on testnet: the hotkey comes from whoever runs this, and the
// read of it is answered here. Everything else goes to the chain.
type convertRehearsalRpc struct {
	client *evm.Client
	hotkey [32]byte
}

func (self *convertRehearsalRpc) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	if method == "eth_call" {
		return json.Marshal(convertHexOf(self.hotkey[:]))
	}
	return self.client.Call(ctx, method, params...)
}

func (self *convertRehearsalRpc) Batch(ctx context.Context, calls []evm.BatchCall) ([]evm.BatchResult, error) {
	results := make([]evm.BatchResult, len(calls))
	for i, call := range calls {
		results[i].Result, results[i].Err = self.Call(ctx, call.Method, call.Params...)
	}
	return results, nil
}

func convertRehearsalEnv(t *testing.T, name string) uint64 {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return 0
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return parsed
}

// TestConvertTestnetRehearsal is the real path on testnet, for a throwaway key:
// internal/subtensor/TESTNET.md is the checklist. It is skipped unless asked for,
// and the only chain it can write to is testnet. Without
// BT_CONVERT_TESTNET_ACTION it reads, and asks the runtime to validate the signed
// transaction without sending it.
func TestConvertTestnetRehearsal(t *testing.T) {
	phrase := os.Getenv("BT_CONVERT_TESTNET_PHRASE")
	if phrase == "" {
		t.Skip("set BT_CONVERT_TESTNET_PHRASE to a throwaway key's phrase: internal/subtensor/TESTNET.md")
	}
	hotkey, err := ss58.DecodeWithPrefix(os.Getenv("BT_CONVERT_TESTNET_HOTKEY"), SnSs58Prefix)
	if err != nil {
		t.Fatalf("BT_CONVERT_TESTNET_HOTKEY: %v", err)
	}
	netuid := uint16(convertRehearsalEnv(t, "BT_CONVERT_TESTNET_NETUID"))
	if netuid == 0 {
		t.Fatal("BT_CONVERT_TESTNET_NETUID is required")
	}
	amount := convertRehearsalEnv(t, "BT_CONVERT_TESTNET_AMOUNT_RAO")
	action := os.Getenv("BT_CONVERT_TESTNET_ACTION")

	settings := SnTestnetChainSettings()
	genesis, err := hex.DecodeString(convertRehearsalTestnetGenesis)
	if err != nil {
		t.Fatal(err)
	}
	pins := bittensorChainPins{chainId: SnChainIdTestnet, genesisHash: [32]byte(genesis), escrowHotkey: hotkey, netuid: netuid}
	engine := newBittensorChainEngineWithRpc(&convertRehearsalRpc{client: settings.chainClient(), hotkey: hotkey}, pins)

	signer, err := bittensorSignerFromPhrase(phrase)
	if err != nil {
		t.Fatal(err)
	}
	coldkey := signer.PublicKey()
	address, _ := ss58.Encode(coldkey, SnSs58Prefix)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	show := func(label string) {
		view, err := engine.signingView(ctx)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		balance, err := engine.freeBalance(ctx, view, coldkey)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		stake, err := engine.chain.Stake(ctx, hotkey, coldkey, netuid)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		nonce, err := engine.chain.AccountNonce(ctx, coldkey)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		fmt.Printf("%s: key %s spec %d head %d: free %d rao, stake %d rao, nonce %d, providers %d, sufficients %d\n", label, address, view.version.SpecVersion, view.head.Number, balance.FreeRao, stake, nonce, balance.Providers, balance.Sufficients)
	}
	show("before")

	estimate := engine.convertEstimate(ctx, coldkey, int64(amount))
	encoded, _ := json.MarshalIndent(estimate, "", "  ")
	fmt.Println(string(encoded))

	switch action {
	case "":
		// read only: the runtime's own validation of the signed transaction
		limit := uint64(estimate.LimitPriceRao)
		if amount == 0 || limit == 0 {
			fmt.Println("no amount or no limit: nothing to validate")
			return
		}
		call, err := subtensor.NewRemoveStakeLimit(hotkey, netuid, amount, limit)
		if err != nil {
			t.Fatal(err)
		}
		signed, err := engine.signCall(ctx, signer, call)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("the runtime's validation, nothing sent:", convertValidate(ctx, t, engine, signed))
	case "convert":
		if !estimate.CanConvert {
			t.Fatalf("the sdk refuses this amount: %+v", estimate.Error)
		}
		submission := engine.convertSubmit(ctx, signer, estimate)
		encoded, _ := json.MarshalIndent(submission, "", "  ")
		fmt.Println("submission", string(encoded))
		if submission.TxHash != "" {
			receipt := engine.convertWait(ctx, submission)
			encoded, _ := json.MarshalIndent(receipt, "", "  ")
			fmt.Println("receipt", string(encoded))
		}
	case "raw":
		// the chain's own refusals: the sdk would not sign these, and they keep the fee
		limit := convertRehearsalEnv(t, "BT_CONVERT_TESTNET_LIMIT_RAO")
		if limit == 0 {
			t.Fatal("BT_CONVERT_TESTNET_LIMIT_RAO is required")
		}
		call, err := subtensor.NewRemoveStakeLimit(hotkey, netuid, amount, limit)
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := engine.submitCall(ctx, signer, call, func(hash string) { fmt.Println("sent", hash) })
		if err != nil {
			t.Fatal(err)
		}
		fmt.Printf("block %d extrinsic %d success %v failure %q sales %d alpha fee %+v tao fee %d\n", outcome.Block.Number, outcome.ExtrinsicIndex, outcome.Success, outcome.Failure, len(outcome.Sales), outcome.AlphaFee, outcome.TaoFeeRao)
	default:
		t.Fatalf("no action %q", action)
	}
	show("after")
}

// The names of InvalidTransaction, in the runtime's order.
var convertInvalidTransactions = []string{
	"Call", "Payment", "Future", "Stale", "BadProof", "AncientBirthBlock", "ExhaustsResources", "Custom", "BadMandatory", "MandatoryValidation", "BadSigner", "IndeterminateImplicit", "UnknownOrigin",
}

// convertValidate runs TaggedTransactionQueue_validate_transaction. A state_call:
// it reads, and changes nothing anywhere.
func convertValidate(ctx context.Context, t *testing.T, engine *bittensorChainEngine, signed *bittensorSignedCall) string {
	t.Helper()
	args := []byte{2}
	args = append(args, signed.signed.Raw...)
	args = append(args, signed.view.head.Hash[:]...)
	raw, err := engine.rpc.Call(ctx, "state_call", "TaggedTransactionQueue_validate_transaction", convertHexOf(args))
	if err != nil {
		t.Fatal(err)
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		t.Fatal(err)
	}
	reply, err := evm.ParseHexBytes(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return convertValidity(reply)
}

func convertValidity(reply []byte) string {
	switch {
	case len(reply) == 0:
		return "no answer"
	case reply[0] == 0:
		return "valid"
	case len(reply) >= 3 && reply[1] == 0 && int(reply[2]) < len(convertInvalidTransactions):
		name := convertInvalidTransactions[reply[2]]
		if name == "Custom" && len(reply) >= 4 {
			return fmt.Sprintf("invalid: Custom(%d)", reply[3])
		}
		return "invalid: " + name
	default:
		return fmt.Sprintf("refused: %x", reply)
	}
}
