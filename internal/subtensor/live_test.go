package subtensor

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/sdk/sn/evm"
)

// TestLiveRuntimeShape asks a live chain, read-only, whether this client would
// still sign under its current runtime: run it when a runtime upgrade lands.
//
//	SUBTENSOR_LIVE_RPC=https://lite.chain.opentensor.ai go test -run TestLiveRuntimeShape -v ./internal/subtensor
//
// Skipped without the variable, so no ordinary run touches a network.
func TestLiveRuntimeShape(t *testing.T) {
	url := os.Getenv("SUBTENSOR_LIVE_RPC")
	if url == "" {
		t.Skip("SUBTENSOR_LIVE_RPC is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := NewChain(evm.NewClient([]string{url}, time.Minute))
	head, err := chain.FinalizedHead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	version, err := chain.RuntimeVersionAt(ctx, head.Hash)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := chain.MetadataAt(ctx, head.Hash, version)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("finalized block %d, %s %d, transaction version %d\n", head.Number, version.SpecName, version.SpecVersion, version.TransactionVersion)
	hotkey, hash := [32]byte{1}, [32]byte{2}
	remove, err := NewRemoveStakeLimit(hotkey, 25, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	register, err := NewRegisterLimit(25, hotkey, 1)
	if err != nil {
		t.Fatal(err)
	}
	commitment, err := NewSetFleetCommitment(25, hash)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []*Call{remove, register, commitment} {
		if err := meta.CheckRuntime(call); err != nil {
			t.Errorf("%s: %v", call.Name(), err)
		}
	}
	for _, name := range []string{"ExistentialDeposit", "MinStakeRao"} {
		var value uint64
		var err error
		if name == "ExistentialDeposit" {
			value, err = meta.ExistentialDeposit()
		} else {
			value, err = meta.MinStakeRao()
		}
		if err != nil {
			t.Error(err)
		}
		fmt.Printf("%s %d rao\n", name, value)
	}
}
