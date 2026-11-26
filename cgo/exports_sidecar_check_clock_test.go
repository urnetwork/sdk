// The app's sidecar role's clock check through the real c abi (owner decision 803, QD-1427; sidecar §18.5's API list):
// a C host's urnet_sidecar_check_clock on a local Sidecar it opened through urnet_new_sidecar_local reaches the core's
// clock check, which hands each component that keeps a clock its check, once a call, before the export returns; on a
// closed Sidecar's handle, a released one, 0, a handle never issued and a handle of another type it reaches nothing and
// returns, nothing panicking into C.
package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/sdk"
	"github.com/urnetwork/sdk/cgo/abitest"
)

// A component the core lists that counts its clock checks (sidecar's SidecarClockComponent). The cgo package imports
// nothing of the sidecar module (the sdk's import rule, TestSdkImportsTheSidecarModuleByRole), so the probe takes its
// Attach's env type from a witness, the Attach of the core's own dns component, rather than by name.
type testingAbiClockProbe[E any] struct {
	checks atomic.Int64
}

// A probe whose Attach takes what attachWitness takes; the witness is never called.
func newTestingAbiClockProbe[E any](attachWitness func(env E) error) *testingAbiClockProbe[E] {
	return &testingAbiClockProbe[E]{}
}

func (self *testingAbiClockProbe[E]) Name() string {
	return "clock-probe"
}

func (self *testingAbiClockProbe[E]) Attach(env E) error {
	return nil
}

func (self *testingAbiClockProbe[E]) Close() {}

func (self *testingAbiClockProbe[E]) CheckClock() {
	self.checks.Add(1)
}

// urnet_sidecar_check_clock calls through to the Sidecar's core, once a call, and on every handle that names no live
// Sidecar does nothing.
func TestSidecarCheckClockCallsThroughTheAbi(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace := sdk.NewNetworkSpaceWithUrls(ctx, "https://api.sdktest.example", "wss://connect.sdktest.example", testingOfflineClientStrategySettings())
	defer networkSpace.Close()
	// no Sidecar here reaches an api
	if err := networkSpace.GetApi().CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	spaceHandle := newHandle(networkSpace)
	defer handleRelease(spaceHandle)
	store := abitest.NewSidecarKeyStore()
	t.Cleanup(store.Free)
	store.SetMode(abitest.SidecarKeyStoreWorking)

	settingsBytes, err := json.Marshal(map[string]any{"state_dir": t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	settings := cString(string(settingsBytes))
	defer cStringFree(settings)
	outError := socketABIOut(urnet_new_sidecar_local, 9)
	sidecarHandle := socketABICall(
		urnet_new_sidecar_local,
		spaceHandle,
		testingAbiFunction(urnet_new_sidecar_local, 1, store.DeleteSecretFunction()),
		testingAbiFunction(urnet_new_sidecar_local, 2, store.GetSecretFunction()),
		testingAbiFunction(urnet_new_sidecar_local, 3, store.PutSecretFunction()),
		store.UserData(),
		nil,
		nil,
		nil,
		settings,
		outError,
	)[0].Uint()
	if sidecarHandle == 0 {
		t.Fatalf("no Sidecar opened: %q", socketABIError(outError))
	}
	local, ok := resolveHandle[*sdk.Sidecar](sidecarHandle, "the test")
	if !ok {
		t.Fatal("the handle is no Sidecar")
	}
	released := false
	// closed and waited for, its handle released, before the store is freed
	t.Cleanup(func() {
		local.Close()
		closeCtx, closeCancel := context.WithTimeout(context.Background(), testingSidecarAbiTimeout)
		defer closeCancel()
		if err := local.CloseAndWait(closeCtx); err != nil {
			t.Errorf("close: %v", err)
		}
		if !released {
			handleRelease(sidecarHandle)
		}
	})
	core := local.Core()
	if core == nil {
		t.Fatal("a local Sidecar holds no core")
	}
	probe := newTestingAbiClockProbe(core.NewDnsComponent().Attach)
	if _, err := core.AddComponent(probe); err != nil {
		t.Fatal(err)
	}

	socketABICall(urnet_sidecar_check_clock, sidecarHandle)
	if checks := probe.checks.Load(); checks != 1 {
		t.Fatalf("one call reached the core's clock component %d times", checks)
	}
	socketABICall(urnet_sidecar_check_clock, sidecarHandle)
	if checks := probe.checks.Load(); checks != 2 {
		t.Fatalf("two calls reached the core's clock component %d times", checks)
	}
	// 0, a handle never issued and the network space's name no Sidecar
	for _, handle := range []uint64{0, ^uint64(0), spaceHandle} {
		socketABICall(urnet_sidecar_check_clock, handle)
	}
	if checks := probe.checks.Load(); checks != 2 {
		t.Errorf("a handle that names no Sidecar reached the core: %d checks", checks)
	}

	// closed through the abi, its handle still held, and then released
	socketABICall(urnet_sidecar_close, sidecarHandle)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), testingSidecarAbiTimeout)
	defer closeCancel()
	if err := local.CloseAndWait(closeCtx); err != nil {
		t.Fatalf("close: %v", err)
	}
	socketABICall(urnet_sidecar_check_clock, sidecarHandle)
	if checks := probe.checks.Load(); checks != 2 {
		t.Errorf("a closed Sidecar's check reached the core: %d checks", checks)
	}
	if !handleRelease(sidecarHandle) {
		t.Fatal("the Sidecar's handle was not held")
	}
	released = true
	socketABICall(urnet_sidecar_check_clock, sidecarHandle)
	if checks := probe.checks.Load(); checks != 2 {
		t.Errorf("a released handle's check reached the core: %d checks", checks)
	}
}
