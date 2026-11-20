// Constructor pin activation shares the auth commit boundary. Fixtures change
// only private temporary files after initial admission and before publication.
package sdk

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Explicit low-memory settings select the production bounded pin-store path on
// desktop test hosts too; ordinary desktop defaults use the legacy pin store.
func testingStartPinPublicationLocal(t *testing.T, fixture *testingAuthClientShape) {
	t.Helper()
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	device, err := newDeviceLocalWithOverrides(
		fixture.networkSpace, fixture.initialJwt, "pin-publication-serving", "test", "0.0.0",
		fixture.instanceId, settings, connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.localDevice = device
	fixture.deviceJwt = func() string {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		return device.byJwt
	}
	fixture.joinDevice = device.CloseAndWait
	fixture.installCleanup(t)
	if device.peerKeyPinStore == nil || device.transferMemory == nil ||
		device.peerKeyPinStore.budget != device.transferMemory.peerKeyPins ||
		device.settings.EncryptionSettings == nil ||
		device.settings.EncryptionSettings.PeerClientKeyPinStore != device.peerKeyPinStore {
		t.Fatal("low-memory constructor did not install its admitted pin store")
	}
}

// A late pin read failure must retain the serving auth and pin owners, including
// their next real refresh/commit. The address hook follows initial pin loading.
func testingLocalConstructorPinFailure(t *testing.T, replaceInstance bool, pinFailure error) {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = testingStartupClientJwt(t, "pin-failure-serving", nil)
	fixture.seedDistinctLogin(t)
	testingStartPinPublicationLocal(t, fixture)
	if err := fixture.localState.SetBlockerEnabled(true); err != nil {
		t.Fatal(err)
	}
	servingStore := fixture.localDevice.peerKeyPinStore
	if servingStore == nil {
		t.Fatal("serving constructor did not install a bounded pin store")
	}
	peer, pin := testBoundedPin(1)
	if err := servingStore.CommitPeerClientKeyPin(peer, pin); err != nil {
		t.Fatal(err)
	}
	pinPath := filepath.Join(fixture.localState.localStorageDir, peerClientKeyPinsFileName)
	savedPins, err := os.ReadFile(pinPath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	_, apiGeneration := fixture.api.authCredentialSnapshot()
	fixture.localState.authStateLock.Lock()
	localGeneration := fixture.localState.deviceAuthGeneration
	pinGeneration := fixture.localState.peerPinStorePublishedGeneration
	fixture.localState.authStateLock.Unlock()
	assertServing := func() {
		t.Helper()
		after, err := fixture.localState.loadAuthState()
		if err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Error("failed pin activation committed speculative durable auth")
		}
		jwt, generation := fixture.api.authCredentialSnapshot()
		if jwt != fixture.initialJwt || generation != apiGeneration ||
			!fixture.api.deviceOwnsAuth(fixture.localDevice.authPublication) {
			t.Error("failed pin activation changed the serving API credential or owner")
		}
		fixture.localState.authStateLock.Lock()
		retained := fixture.localState.deviceAuthOwner == fixture.localDevice.authPublication &&
			fixture.localState.deviceAuthGeneration == localGeneration &&
			fixture.localState.peerPinStoreOwner == servingStore &&
			fixture.localState.peerPinStorePublishedGeneration == pinGeneration
		fixture.localState.authStateLock.Unlock()
		if !retained {
			t.Error("failed pin activation changed the serving durable or pin publication owner")
		}
		if fixture.deviceJwt() != fixture.initialJwt || fixture.localDevice.ctx.Err() != nil ||
			!fixture.localState.GetBlockerEnabled() {
			t.Error("failed pin activation disturbed the serving device or routing preference")
		}
		if got, found, err := servingStore.GetPeerClientKeyPinChecked(peer); err != nil || !found || got != pin {
			t.Error("serving pin owner stopped answering after a candidate's failed activation")
		}
	}
	proposedJwt := testingStartupClientJwt(t, "pin-failure-proposed", map[string]any{
		"iat": testingClientStartupNow - 50,
	})
	proposedInstance := fixture.instanceId
	if replaceInstance {
		proposedInstance = NewId()
	}
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.UseExperimentalTunnelAddress = false
	var memory *deviceLocalTransferMemory
	settings.testingBeforePeerPinStoreAdmission = func(owner *deviceLocalTransferMemory) { memory = owner }
	addressCalls := 0
	changedPins := false
	restorePins := func() {
		t.Helper()
		if changedPins {
			if err := os.Remove(pinPath); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(pinPath, savedPins, 0600); err != nil {
				t.Fatal(err)
			}
			changedPins = false
		}
	}
	t.Cleanup(restorePins)
	settings.testingTakeLocalAddress = func() (netip.Addr, bool) {
		addressCalls++
		assertServing()
		if memory == nil || memory.peerKeyPins.UsedByteCount() != peerPinStoreMemoryByteCount {
			t.Fatal("late failure hook ran before candidate pin admission")
		}
		if pinFailure == errPeerPinStoreIO {
			if err := os.Remove(pinPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(pinPath, 0700); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(pinPath, []byte("malformed synthetic pin store"), 0600); err != nil {
			t.Fatal(err)
		}
		changedPins = true
		return connect.TakeLocalIpv4Address()
	}
	device, err := newDeviceLocalWithOverrides(
		fixture.networkSpace, proposedJwt, "pin-failure-candidate", "test", "0.0.0",
		proposedInstance, settings, connect.NewId(),
	)
	if device != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = device.CloseAndWait(ctx)
		t.Fatal("pin activation failure returned a replacement device")
	}
	if !errors.Is(err, pinFailure) || addressCalls != 1 {
		t.Fatalf("constructor did not reach the late pin failure: calls=%d error=%v", addressCalls, err)
	}
	assertServing()
	if len(fixture.api.jwtRefreshListeners.Get()) != 1 || len(fixture.api.authLogoutListeners.Get()) != 1 {
		t.Error("failed constructor retained API listeners or detached the serving owner's listeners")
	}
	stats := memory.root.Stats()
	if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Error("failed constructor retained the candidate's pin memory")
	}
	restorePins()
	if err := servingStore.CommitPeerClientKeyPin(peer, pin); err != nil {
		t.Errorf("failed candidate stranded the serving pin writer: %v", err)
	}
	nextJwt := testingStartupClientJwt(t, "pin-failure-serving-refreshed", map[string]any{
		"iat": testingClientStartupNow - 10,
	})
	if !fixture.api.setRefreshedByJwt(fixture.initialJwt, nextJwt) {
		t.Fatal("failed candidate stranded the serving API refresh")
	}
	afterRefresh, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if afterRefresh.ByClientJwt != nextJwt || fixture.deviceJwt() != nextJwt ||
		afterRefresh.ByJwt != fixture.adminJwt || afterRefresh.InstanceId != fixture.instanceId.String() {
		t.Error("serving refresh lost its durable owner after the candidate's pin failure")
	}
}

// Malformed pins after preparation cannot commit a proposed client credential.
func TestLocalConstructorPinCorruptionDoesNotCommitClient(t *testing.T) {
	testingLocalConstructorPinFailure(t, false, errPeerPinStoreCorrupt)
}

// A changed instance is speculative through the same final pin reread.
func TestLocalConstructorPinCorruptionDoesNotCommitInstance(t *testing.T) {
	testingLocalConstructorPinFailure(t, true, errPeerPinStoreCorrupt)
}

// Replacing the leaf with a directory deterministically exercises read refusal.
func TestLocalConstructorPinReadFailurePreservesServingOwner(t *testing.T) {
	testingLocalConstructorPinFailure(t, false, errPeerPinStoreIO)
}

// A healthy replacement must include the serving store's commit after its first
// pin snapshot. This covers both authenticated and anonymous store ownership.
func testingLocalConstructorPinCommit(t *testing.T, authenticated bool) {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = ""
	if authenticated {
		fixture.initialJwt = testingStartupClientJwt(t, "pin-commit-serving", nil)
		fixture.seedDistinctLogin(t)
	}
	testingStartPinPublicationLocal(t, fixture)
	servingStore := fixture.localDevice.peerKeyPinStore
	if servingStore == nil {
		t.Fatal("serving constructor did not install a bounded pin store")
	}
	peer, pin := testBoundedPin(1)
	if err := servingStore.CommitPeerClientKeyPin(peer, pin); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	proposedJwt := ""
	proposedInstance := fixture.instanceId
	if authenticated {
		proposedJwt = testingStartupClientJwt(t, "pin-commit-proposed", map[string]any{
			"iat": testingClientStartupNow - 50,
		})
		proposedInstance = NewId()
	}
	latePeer, latePin := testBoundedPin(2)
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.UseExperimentalTunnelAddress = false
	addressCalls := 0
	settings.testingTakeLocalAddress = func() (netip.Addr, bool) {
		addressCalls++
		prepared, err := fixture.localState.loadAuthState()
		if err != nil {
			t.Fatal(err)
		}
		if prepared != before || fixture.api.GetByJwt() != fixture.initialJwt ||
			!fixture.api.deviceOwnsAuth(fixture.localDevice.authPublication) {
			t.Fatal("preparation published candidate auth before its final pin read")
		}
		if err := servingStore.CommitPeerClientKeyPin(latePeer, latePin); err != nil {
			t.Fatalf("preparation revoked the serving pin writer: %v", err)
		}
		return connect.TakeLocalIpv4Address()
	}
	device, err := newDeviceLocalWithOverrides(
		fixture.networkSpace, proposedJwt, "pin-commit-candidate", "test", "0.0.0",
		proposedInstance, settings, connect.NewId(),
	)
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
	if addressCalls != 1 || device.peerKeyPinStore == nil ||
		!fixture.api.deviceOwnsByJwt(device.authPublication, proposedJwt) {
		t.Fatal("healthy constructor did not publish its API and pin owner")
	}
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if authenticated {
		if after.ByJwt != fixture.adminJwt || after.ByClientJwt != proposedJwt ||
			after.InstanceId != proposedInstance.String() || after.Generation != before.Generation+1 {
			t.Error("healthy pin activation did not commit the complete proposed auth once")
		}
	} else if after != before {
		t.Error("anonymous pin activation claimed durable auth storage")
	}
	for _, expected := range []struct {
		peer connect.Id
		pin  connect.ClientKeyPin
	}{{peer: peer, pin: pin}, {peer: latePeer, pin: latePin}} {
		if got, found, err := device.peerKeyPinStore.GetPeerClientKeyPinChecked(expected.peer); err != nil || !found || got != expected.pin {
			t.Error("replacement omitted a pin committed before its publication")
		}
	}
	if err := servingStore.CommitPeerClientKeyPin(peer, pin); !errors.Is(err, errPeerPinStoreSuperseded) {
		t.Error("published replacement did not retire the old pin writer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fixture.localDevice.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	if !fixture.api.deviceOwnsByJwt(device.authPublication, proposedJwt) {
		t.Error("old close cleared the new API owner")
	}
	if err := device.peerKeyPinStore.CommitPeerClientKeyPin(latePeer, latePin); err != nil {
		t.Errorf("old close revoked the new pin writer: %v", err)
	}
	if authenticated {
		nextJwt := testingStartupClientJwt(t, "pin-commit-refreshed", map[string]any{
			"iat": testingClientStartupNow - 10,
		})
		if !fixture.api.setRefreshedByJwt(proposedJwt, nextJwt) {
			t.Fatal("healthy replacement did not own its next API refresh")
		}
		refreshed, err := fixture.localState.loadAuthState()
		if err != nil {
			t.Fatal(err)
		}
		device.stateLock.Lock()
		deviceJwt := device.byJwt
		device.stateLock.Unlock()
		if refreshed.ByClientJwt != nextJwt || refreshed.InstanceId != proposedInstance.String() || deviceJwt != nextJwt {
			t.Error("healthy replacement did not retain durable refresh ownership")
		}
	}
}

// New credentials and late serving pins become visible in one accepted commit.
func TestLocalConstructorPinCommitIncludesLateServingPins(t *testing.T) {
	testingLocalConstructorPinCommit(t, true)
}

// Anonymous devices also reread and transfer pins without claiming auth storage.
func TestLocalConstructorAnonymousPinCommitIncludesLateServingPins(t *testing.T) {
	testingLocalConstructorPinCommit(t, false)
}

// Checking pins early cannot transfer their owner before auth publication is
// accepted. An explicit API login deterministically supersedes preparation.
func TestLocalConstructorSupersededAuthDoesNotPublishPins(t *testing.T) {
	fixture := testingAuthClientShapeSpace(t)
	fixture.initialJwt = testingStartupClientJwt(t, "pin-superseded-serving", nil)
	fixture.seedDistinctLogin(t)
	testingStartPinPublicationLocal(t, fixture)
	servingStore := fixture.localDevice.peerKeyPinStore
	if servingStore == nil {
		t.Fatal("serving constructor did not install a bounded pin store")
	}
	peer, pin := testBoundedPin(1)
	if err := servingStore.CommitPeerClientKeyPin(peer, pin); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.UseExperimentalTunnelAddress = false
	var apiGeneration uint64
	addressCalls := 0
	settings.testingTakeLocalAddress = func() (netip.Addr, bool) {
		addressCalls++
		fixture.api.SetByJwt(fixture.adminJwt)
		_, apiGeneration = fixture.api.authCredentialSnapshot()
		return connect.TakeLocalIpv4Address()
	}
	device, err := newDeviceLocalWithOverrides(
		fixture.networkSpace, fixture.initialJwt, "pin-superseded-candidate", "test", "0.0.0",
		fixture.instanceId, settings, connect.NewId(),
	)
	if device != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = device.CloseAndWait(ctx)
		t.Fatal("superseded constructor returned a replacement device")
	}
	if err == nil || addressCalls != 1 {
		t.Fatal("constructor did not reach the superseded API boundary")
	}
	after, err := fixture.localState.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	jwt, generation := fixture.api.authCredentialSnapshot()
	if after != before || jwt != fixture.adminJwt || generation != apiGeneration {
		t.Error("failed publication changed durable auth or the explicit API login")
	}
	if err := servingStore.CommitPeerClientKeyPin(peer, pin); err != nil {
		t.Errorf("auth publication refusal retired the serving pin writer: %v", err)
	}
}
