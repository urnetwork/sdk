package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func peerPinDeviceSettings(target ByteCount) *DeviceLocalSettings {
	settings := DefaultDeviceLocalSettings()
	settings.MemoryTargetByteCount = target
	settings.DisableLogging = true
	settings.EncryptionSettings = nil // nil is a supported default, not opt-out.
	return settings
}

func newPeerPinTestDevice(t *testing.T, fixture *testingAuthClientShape, settings *DeviceLocalSettings, jwt string) *DeviceLocal {
	t.Helper()
	device, err := newDeviceLocalWithOverridesForPlatform(fixture.networkSpace, jwt, "pin-test", "test", "0",
		fixture.instanceId, settings, connect.NewId(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := device.CloseAndWait(ctx); err != nil {
			t.Error(err)
		}
	})
	if device.provider != nil {
		device.provider.platformTransport.Close()
	}
	return device
}

func assertPeerPinDevicePropagation(t *testing.T, device *DeviceLocal, want connect.PeerClientKeyPinStore) {
	t.Helper()
	if device.settings.EncryptionSettings == nil || device.settings.EncryptionSettings.PeerClientKeyPinStore != want {
		t.Fatal("device lost store identity")
	}
	if device.provider != nil && device.provider.Client().EncryptionSessionManager().Settings().PeerClientKeyPinStore != want {
		t.Fatal("provider did not receive the pre-admitted store")
	}
	upgrade := connect.DefaultUpgradeMuxSettings()
	upgrade.Dns = nil
	device.SetUpgradeMuxSettings(upgrade)
	var previous *connect.ApiMultiClientGenerator
	for range 2 {
		device.SetConnectLocation(&ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}})
		device.stateLock.Lock()
		generator := device.apiMultiClientGenerator
		device.stateLock.Unlock()
		if generator == nil || generator == previous {
			t.Fatal("destination did not create its real settings generator")
		}
		client := generator.NewClientSettings()
		if client.EncryptionSettings == nil || client.EncryptionSettings.PeerClientKeyPinStore != want {
			t.Fatal("destination generation lost the exact shared store")
		}
		previous = generator
		device.SetConnectLocation(nil)
	}
}

func TestDeviceLocalPeerPinStoreProfilesPropagationAndJoinedRelease(t *testing.T) {
	for _, target := range []ByteCount{20, 28} {
		t.Run(fmt.Sprint(target), func(t *testing.T) {
			fixture := testingAuthClientShapeSpace(t)
			settings := peerPinDeviceSettings(target * 1024 * 1024)
			device := newPeerPinTestDevice(t, fixture, settings, "")
			store, memory := device.peerKeyPinStore, device.transferMemory
			if store == nil || memory == nil || store.budget != memory.peerKeyPins || memory.peerKeyPins.Parent() != memory.client {
				t.Fatal("pin store escaped the device client parent")
			}
			if settings.EncryptionSettings != nil {
				t.Fatal("constructor mutated reusable nil caller settings")
			}
			assertPeerPinDevicePropagation(t, device, store)
			peer, pin := testBoundedPin(1)
			if err := store.CommitPeerClientKeyPin(peer, pin); err != nil {
				t.Fatal(err)
			}
			usage := device.MemoryUsed()
			if usage.PeerKeyPinCount != 1 || usage.PeerKeyPinUsedByteCount != peerPinStoreMemoryByteCount ||
				usage.PeerKeyPinBudgetByteCount != peerPinStoreMemoryByteCount || usage.TransferRootUsedByteCount < usage.PeerKeyPinUsedByteCount ||
				usage.TotalByteCount > settings.MemoryTargetByteCount {
				t.Fatalf("missing/double-counted pin diagnostics: %+v", usage)
			}
			diagnostic := transferDiagTransferBudget(usage, 1)
			encoded, err := json.Marshal(diagnostic)
			if err != nil || len(encoded) > 3000 || diagnostic.PinCount != 1 || diagnostic.PinUsedByteCount != peerPinStoreMemoryByteCount || diagnostic.RootUsedByteCount < diagnostic.PinUsedByteCount {
				t.Fatal("bounded evidence lost the named pin child")
			}
			_, clientShare, _, providerShare := deviceMemoryShares(device.settings)
			if usage.TransferRootBudgetByteCount != clientShare+providerShare {
				t.Fatal("pin envelope increased the existing root")
			}
			release := make(chan struct{})
			device.lifecycleWorkers.Add(1)
			go func() { defer device.lifecycleWorkers.Done(); <-release }()
			device.Close()
			if store.budget.UsedByteCount() != peerPinStoreMemoryByteCount {
				t.Fatal("pin owner released before retiring client work joined")
			}
			close(release)
			if err := device.CloseAndWait(t.Context()); err != nil {
				t.Fatal(err)
			}
			usage = device.MemoryUsed()
			if usage.PeerKeyPinUsedByteCount != 0 || usage.PeerKeyPinReservedByteCount != usage.PeerKeyPinReleasedByteCount || usage.PeerKeyPinCount != 0 {
				t.Fatalf("joined teardown leaked pins: %+v", usage)
			}
		})
	}
}

func TestDeviceLocalPeerPinStoreRefusalBeforeProviderAndRollback(t *testing.T) {
	for _, kind := range []string{"capacity", "oversize", "corrupt", "activation corrupt"} {
		t.Run(kind, func(t *testing.T) {
			fixture := testingAuthClientShapeSpace(t)
			settings := peerPinDeviceSettings(20 * 1024 * 1024)
			var memory *deviceLocalTransferMemory
			var held ByteCount
			settings.testingBeforePeerPinStoreAdmission = func(owner *deviceLocalTransferMemory) {
				memory = owner
				if kind == "capacity" {
					held = owner.root.TotalByteCount()
					if !owner.root.TryReserve(held) {
						t.Fatal("test fill refused")
					}
				}
			}
			providerCalls := 0
			settings.testingBeforeProviderConstruction = func() {
				providerCalls++
				if kind == "activation corrupt" {
					if err := os.WriteFile(filepath.Join(fixture.localState.localStorageDir, peerClientKeyPinsFileName), []byte("corrupt"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			want := errPeerPinStoreBudget
			wantProviderCalls := 0
			if kind == "activation corrupt" {
				want, wantProviderCalls = errPeerPinStoreCorrupt, 1
			} else if kind != "capacity" {
				data := []byte("corrupt")
				want = errPeerPinStoreCorrupt
				if kind == "oversize" {
					data = make([]byte, peerPinStoreMaxFileByteCount+1)
					want = errPeerPinStoreOversize
				}
				if err := os.WriteFile(filepath.Join(fixture.localState.localStorageDir, peerClientKeyPinsFileName), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			device, err := newDeviceLocalWithOverridesForPlatform(fixture.networkSpace, "", "pin-test", "test", "0",
				fixture.instanceId, settings, connect.NewId(), true)
			if device != nil || !errors.Is(err, want) || providerCalls != wantProviderCalls || memory == nil {
				t.Fatalf("refusal crossed ownership gate: device=%v err=%v providers=%d", device, err, providerCalls)
			}
			memory.root.Release(held)
			stats := memory.root.Stats()
			if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount || fixture.localState.peerPinStoreOwner != nil {
				t.Fatal("failed constructor retained store ownership")
			}
		})
	}
}

func TestDeviceLocalPeerPinStoreAuthenticatedReplacement(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	settings := peerPinDeviceSettings(20 * 1024 * 1024)
	settings.AllowProvider = false
	first := newPeerPinTestDevice(t, fixture, settings, fixture.initialJwt)
	peer, pin := testBoundedPin(1)
	if err := first.peerKeyPinStore.CommitPeerClientKeyPin(peer, pin); err != nil {
		t.Fatal(err)
	}
	second := newPeerPinTestDevice(t, fixture, settings, fixture.initialJwt)
	if err := first.peerKeyPinStore.CommitPeerClientKeyPin(peer, pin); err != errPeerPinStoreSuperseded {
		t.Fatalf("superseded authenticated store accepted commit: %v", err)
	}
	if got, found, err := second.peerKeyPinStore.GetPeerClientKeyPinChecked(peer); err != nil || !found || got != pin {
		t.Fatal("replacement lost persisted pin")
	}
	if err := first.CloseAndWait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := second.peerKeyPinStore.CommitPeerClientKeyPin(peer, pin); err != nil {
		t.Fatalf("old close revoked new owner: %v", err)
	}
}

type callerCheckedPinStore struct{ connect.PeerClientKeyPinStore }

func (self *callerCheckedPinStore) GetPeerClientKeyPinChecked(peer connect.Id) (connect.ClientKeyPin, bool, error) {
	pin, ok := self.GetPeerClientKeyPin(peer)
	return pin, ok, nil
}
func (self *callerCheckedPinStore) CommitPeerClientKeyPin(peer connect.Id, pin connect.ClientKeyPin) error {
	self.SetPeerClientKeyPin(peer, pin)
	self.SetSignedHistorySeen()
	return nil
}

func TestDeviceLocalPeerPinStorePreservesCallerStores(t *testing.T) {
	for _, checked := range []bool{false, true} {
		t.Run(fmt.Sprint(checked), func(t *testing.T) {
			fixture := testingAuthClientShapeSpace(t)
			settings := peerPinDeviceSettings(20 * 1024 * 1024)
			var store connect.PeerClientKeyPinStore = fixture.localState.peerClientKeyPinStore()
			if checked {
				store = &callerCheckedPinStore{store}
			}
			settings.EncryptionSettings = connect.DefaultEncryptionSettings()
			settings.EncryptionSettings.PeerClientKeyPinStore = store
			device := newPeerPinTestDevice(t, fixture, settings, "")
			assertPeerPinDevicePropagation(t, device, store)
			if device.peerKeyPinStore != nil || device.MemoryUsed().PeerKeyPinUsedByteCount != 0 {
				t.Fatal("device acquired caller-owned store lifecycle")
			}
			if err := device.CloseAndWait(t.Context()); err != nil {
				t.Fatal(err)
			}
			peer, pin := testBoundedPin(1)
			store.SetPeerClientKeyPin(peer, pin)
			if got, ok := store.GetPeerClientKeyPin(peer); !ok || got != pin {
				t.Fatal("device close invalidated caller-owned store")
			}
		})
	}
}

func TestDeviceLocalPeerPinStoreCommitSnapshotCloseRace(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	settings := peerPinDeviceSettings(20 * 1024 * 1024)
	settings.AllowProvider = false
	device := newPeerPinTestDevice(t, fixture, settings, "")
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := range 32 {
			peer, pin := testBoundedPin(i)
			err := device.peerKeyPinStore.CommitPeerClientKeyPin(peer, pin)
			if err != nil && err != errPeerPinStoreClosed && err != errPeerPinStoreSuperseded {
				t.Errorf("concurrent commit: %v", err)
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for range 128 {
			usage := device.MemoryUsed()
			if usage.TransferRootUsedByteCount < usage.PeerKeyPinUsedByteCount || usage.TotalByteCount > usage.TargetByteCount {
				t.Error("incoherent pin/root byte sample")
				return
			}
		}
	}()
	device.Close()
	workers.Wait()
	if err := device.CloseAndWait(t.Context()); err != nil {
		t.Fatal(err)
	}
	stats := device.transferMemory.root.Stats()
	if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatalf("pin/close claim leak: %+v", stats)
	}
}
