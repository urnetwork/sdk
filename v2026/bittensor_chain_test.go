//go:build !ios_extension && !js

package sdk

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/sdk/v2026/internal/subtensor"
)

// The one way a transaction leaves the sdk, for each call a key on the device
// signs: what the Top 200 flow builds on as well as convert.
func bittensorChainTestCalls(t *testing.T) map[string]*subtensor.Call {
	t.Helper()
	remove, err := subtensor.NewRemoveStakeLimit(bittensorMainnetPins.escrowHotkey, 25, 1_000_000_000, 4_070_605)
	if err != nil {
		t.Fatal(err)
	}
	register, err := subtensor.NewRegisterLimit(25, [32]byte{7, 7, 7}, 550_000)
	if err != nil {
		t.Fatal(err)
	}
	commitment, err := subtensor.NewSetFleetCommitment(25, [32]byte{0xcd, 0xcd})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*subtensor.Call{
		"remove_stake_limit": remove,
		"register_limit":     register,
		"set_commitment":     commitment,
	}
}

// Each call is signed under what the chain says now: the pinned genesis, the
// runtime's versions, a mortal era from the finalized head, and the signer's
// nonce. The signature in the bytes sent verifies over that payload, and the
// chain's outcome comes back.
func TestSignCallSignsEachFixedCall(t *testing.T) {
	signer, err := bittensorSignerFromPhrase(convertTestDevPhrase)
	if err != nil {
		t.Fatal(err)
	}
	for name, call := range bittensorChainTestCalls(t) {
		t.Run(name, func(t *testing.T) {
			chain := newConvertFakeChain(t)
			chain.nonce = 7
			chain.landEvents = func(index uint32) string {
				return convertEvents(index, convertSuccessEvent())
			}
			engine := convertEngine(chain)
			ctx := context.Background()
			recording := &convertRecordingSigner{Signer: signer}
			birth := chain.head
			signed, err := engine.signCall(ctx, recording, call)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := subtensor.ParseMetadata(chain.metadata)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := subtensor.Prepare(meta, subtensor.SigningContext{
				GenesisHash:        bittensorMainnetPins.genesisHash,
				SpecVersion:        486,
				TransactionVersion: 1,
				BirthBlockNumber:   birth,
				BirthBlockHash:     convertBlockHash(birth),
				Nonce:              7,
			}, signer.PublicKey(), call)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(recording.payload, prepared.SigningPayload()) {
				t.Fatalf("signed\n %x\nwant\n %x", recording.payload, prepared.SigningPayload())
			}
			// compact length (2), version (1), address (33), signature type (1)
			signatureAt := 2 + 1 + 33 + 1
			if !convertVerifySr25519(t, signer.PublicKey(), recording.payload, [64]byte(signed.signed.Raw[signatureAt:signatureAt+64])) {
				t.Fatal("the signature in the transaction does not verify over its payload")
			}
			if first, last := signed.landingBlocks(); first != birth+1 || last != birth+64 {
				t.Fatalf("landing blocks %d to %d from %d", first, last, birth)
			}
			sent := ""
			outcome, err := engine.sendCall(ctx, signed, func(txHash string) {
				sent = txHash
			})
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.Success || len(chain.submitted) != 1 || !bytes.Equal(chain.submitted[0], signed.signed.Raw) || sent == "" {
				t.Fatalf("outcome %+v, sent %q, %d submitted", outcome, sent, len(chain.submitted))
			}
		})
	}
}

// Signing needs nothing from the signer's balance: a hotkey with no TAO signs
// set_commitment, which the runtime charges nothing for.
func TestSignCallReadsNoBalance(t *testing.T) {
	signer, err := bittensorSignerFromSeed([32]byte{0x42})
	if err != nil {
		t.Fatal(err)
	}
	chain := newConvertFakeChain(t)
	chain.free, chain.stake = 0, 0
	engine := convertEngine(chain)
	if _, err := engine.signCall(context.Background(), signer, bittensorChainTestCalls(t)["set_commitment"]); err != nil {
		t.Fatal(err)
	}
	if chain.reads["state_getStorage"] != 0 || chain.reads["StakeInfoRuntimeApi_get_stake_info_for_hotkey_coldkey_netuid"] != 0 {
		t.Fatalf("signing read balances: %v", chain.reads)
	}
}

// Another chain, or a runtime whose call or extensions this sdk does not know,
// refuses before the key is asked for anything.
func TestSignCallRefusesBeforeSigning(t *testing.T) {
	for _, row := range []struct {
		name   string
		call   string
		change func(chain *convertFakeChain)
		check  func(err error) bool
	}{
		{"another chain", "register_limit", func(chain *convertFakeChain) { chain.genesis = [32]byte{1} }, func(err error) bool {
			return errors.Is(err, errBittensorChainWrongChain)
		}},
		{"register_limit renamed", "register_limit", func(chain *convertFakeChain) {
			chain.metadata = bytes.ReplaceAll(chain.metadata, []byte("register_limit"), []byte("register_limiz"))
		}, subtensor.IsRuntimeChanged},
		{"set_commitment renamed", "set_commitment", func(chain *convertFakeChain) {
			chain.metadata = bytes.ReplaceAll(chain.metadata, []byte("set_commitment"), []byte("set_commitmenz"))
		}, subtensor.IsRuntimeChanged},
		{"Raw40 renamed", "set_commitment", func(chain *convertFakeChain) {
			chain.metadata = bytes.ReplaceAll(chain.metadata, []byte("\x14Raw40"), []byte("\x14Raw4z"))
		}, subtensor.IsRuntimeChanged},
		{"the nonce's extension renamed", "set_commitment", func(chain *convertFakeChain) {
			chain.metadata = bytes.ReplaceAll(chain.metadata, []byte("\x28CheckNonce"), []byte("\x28CheckNonse"))
		}, subtensor.IsRuntimeChanged},
		{"a runtime version the metadata is not", "remove_stake_limit", func(chain *convertFakeChain) { chain.spec = 487 }, func(err error) bool {
			return err != nil
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			chain := newConvertFakeChain(t)
			row.change(chain)
			signer := &convertTestSigner{public: convertTestColdkey}
			signed, err := convertEngine(chain).signCall(context.Background(), signer, bittensorChainTestCalls(t)[row.call])
			if signed != nil || !row.check(err) {
				t.Fatalf("signed %v, %v", signed != nil, err)
			}
			if len(signer.payloads) != 0 {
				t.Fatal("the key was asked to sign")
			}
		})
	}
}
