// Actual providers, checked storage and held ownership boundaries exercise
// explicit key saves independently of native locks or preference autosave.
package sdk

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Provides a real key-producing client even though providing and consumer
// routing are disabled. Synthetic values are never included in diagnostics.
func testingProviderKeySaveDevice(t *testing.T, fixture *testingAuthClientShape, marker byte) *DeviceLocal {
	t.Helper()
	seed := bytes.Repeat([]byte{marker}, 32)
	device := testingDeviceKeyLoadConstructor(t, fixture, NewDeviceLocalKeyMaterial(seed, nil, nil))
	device.SetProvideControlMode(ProvideControlModeNever)
	device.SetProvidePaused(true)
	secrets := NewProvideSecretKeyList()
	secrets.Add(&ProvideSecretKey{ProvideMode: ProvideModePublic, ProvideSecretKey: string(bytes.Repeat([]byte{marker}, 32))})
	device.LoadProvideSecretKeys(secrets)
	return device
}

// Positive completion bound; an absent result is not a semantic old-red.
func testingProviderKeySaveResult(t *testing.T, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("provider key operation did not complete")
		return nil
	}
}

// Uses fresh disk observation without displaying identity or secret bytes.
func testingRequireProviderKeyState(t *testing.T, state *LocalState, material *DeviceLocalKeyMaterial, secrets *ProvideSecretKeyList) {
	t.Helper()
	stored, err := state.LoadDeviceLocalKeyMaterial()
	if err != nil || stored == nil || !testingDeviceKeyLoadEqual(stored, material) {
		t.Fatal("durable key material does not match its accepted owner")
	}
	storedSecrets, err := state.LoadProvideSecretKeys()
	if err != nil || storedSecrets == nil || storedSecrets.Len() != secrets.Len() {
		t.Fatal("durable provider secrets do not match their accepted owner")
	}
	provideModeSecretKeys := map[ProvideMode]string{}
	for _, secret := range secrets.values {
		provideModeSecretKeys[secret.ProvideMode] = secret.ProvideSecretKey
	}
	for _, secret := range storedSecrets.values {
		if expected, ok := provideModeSecretKeys[secret.ProvideMode]; !ok || expected != secret.ProvideSecretKey {
			t.Fatal("durable provider secrets do not match their accepted owner")
		}
	}
}

func TestDeviceLocalExplicitKeySavesRemainSeparateAndWorkWithProvidingDisabled(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	device := testingProviderKeySaveDevice(t, fixture, 7)
	if device.GetProvideEnabled() || device.GetConnectEnabled() || device.GetAutoSave() {
		t.Fatal("key fixture unexpectedly enabled providing, routing or autosave")
	}
	beforeSave := device.GetLastLocalStateSaveResult()
	if beforeSave == nil || beforeSave.GetPreference() != "provide-control-mode" ||
		beforeSave.GetAutoSaveEnabled() || beforeSave.GetSaved() || beforeSave.GetError() != "" {
		t.Fatal("key fixture did not retain its completed off-mode preference result")
	}
	beforeSaveValue := *beforeSave
	var preferenceNotifications atomic.Int64
	sub := device.AddLocalStateSaveListener(testingPreferenceRpcSaveListener(func(*DeviceLocalSaveResult) {
		preferenceNotifications.Add(1)
	}))
	t.Cleanup(sub.Close)
	secretPath := filepath.Join(fixture.localState.localStorageDir, ".provide_secret_keys")
	if err := device.SaveKeyMaterial(); err != nil {
		t.Fatal("key-only explicit save failed")
	}
	if _, err := os.Lstat(secretPath); !os.IsNotExist(err) {
		t.Fatal("key-only persistence implicitly wrote provider secrets")
	}
	keyPath := filepath.Join(fixture.localState.localStorageDir, ".device_local_key_material")
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil || len(keyBytes) == 0 {
		t.Fatal("key-only explicit save did not commit")
	}
	if err := device.SaveProvideSecretKeys(); err != nil {
		t.Fatal("explicit secret save failed with providing disabled")
	}
	after, err := os.ReadFile(keyPath)
	if err != nil || !bytes.Equal(keyBytes, after) {
		t.Fatal("secret-only persistence changed the key file")
	}
	testingRequireProviderKeyState(t, fixture.localState, device.GetKeyMaterial(), device.GetProvideSecretKeys())
	afterSave := device.GetLastLocalStateSaveResult()
	if device.GetAutoSave() || afterSave != beforeSave || *afterSave != beforeSaveValue || preferenceNotifications.Load() != 0 {
		t.Fatal("explicit key save changed preference mode, result or notifications")
	}
}

// The real device method is held after getters and before its paired admission.
// A replacement commits actual different material while the old callback waits.
func testingProviderKeySaveRejectsReset(t *testing.T, part string) {
	t.Helper()
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	old := testingProviderKeySaveDevice(t, fixture, 11)
	if old.SaveKeyMaterial() != nil || old.SaveProvideSecretKeys() != nil {
		t.Fatal("could not seed the first provider owner")
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	t.Cleanup(resume)
	old.testingBeforeProviderKeySaveAdmission = func(string) {
		close(entered)
		<-release
	}
	done := make(chan error, 1)
	go func() {
		if part == "key-material" {
			done <- old.SaveKeyMaterial()
		} else {
			done <- old.SaveProvideSecretKeys()
		}
	}()
	testingAwaitAuthBoundary(t, entered)
	reset, err := fixture.networkSpace.ResetLocalStateIfCurrent(testingPairedAuthSnapshot(t, fixture))
	if err != nil || reset == nil || !reset.GetReset() {
		t.Fatal("actual provider-owner reset failed")
	}
	fixture.initialJwt = testingRefreshableJwtWithMarker(t, "replacement-provider-key-owner")
	fixture.seedDistinctLogin(t)
	replacement := testingProviderKeySaveDevice(t, fixture, 23)
	if replacement.SaveKeyMaterial() != nil || replacement.SaveProvideSecretKeys() != nil {
		t.Fatal("replacement provider state could not commit")
	}
	resume()
	writeErr := testingProviderKeySaveResult(t, done)
	// Check actual final files before interpreting the returned error. The
	// counterfactual must fail for stale durable mutation, not only a boolean.
	stored, err := fixture.localState.LoadDeviceLocalKeyMaterial()
	secrets, secretErr := fixture.localState.LoadProvideSecretKeys()
	if err != nil || secretErr != nil || stored == nil || secrets == nil ||
		!testingDeviceKeyLoadEqual(stored, replacement.GetKeyMaterial()) ||
		secrets.Len() != 1 || *secrets.Get(0) != *replacement.GetProvideSecretKeys().Get(0) {
		t.Fatal("late provider key save overwrote the replacement's durable state")
	}
	if writeErr == nil || writeErr.Error() != localAuthSnapshotSupersededMessage {
		t.Fatal("late provider key save did not report supersession")
	}
}

func TestDeviceLocalKeyMaterialSaveRejectsHeldOldOwnerAfterReset(t *testing.T) {
	testingProviderKeySaveRejectsReset(t, "key-material")
}

func TestDeviceLocalProvideSecretSaveRejectsHeldOldOwnerAfterReset(t *testing.T) {
	testingProviderKeySaveRejectsReset(t, "provide-secret-keys")
}

func TestDeviceLocalKeySaveResetPreservesActuallyCommittedMaterial(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	device := testingProviderKeySaveDevice(t, fixture, 31)
	if device.SaveKeyMaterial() != nil || device.SaveProvideSecretKeys() != nil {
		t.Fatal("could not seed provider state")
	}
	device.SetKeyMaterial(NewDeviceLocalKeyMaterial(bytes.Repeat([]byte{37}, 32), nil, nil))
	material := device.GetKeyMaterial()
	snapshot := testingPairedAuthSnapshot(t, fixture)
	committed := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	t.Cleanup(resume)
	device.testingAfterProviderKeySaveCommit = func(string) {
		close(committed)
		<-release
	}
	written := make(chan error, 1)
	go func() { written <- device.SaveKeyMaterial() }()
	testingAwaitAuthBoundary(t, committed)
	if fixture.api.authMutationLock.TryLock() {
		fixture.api.authMutationLock.Unlock()
		t.Fatal("provider key commit did not hold paired cleanup serialization")
	}
	// A separate LocalState observes the actual completed file while the writer
	// remains admitted. This is not an assertion over an in-memory save result.
	fresh := newLocalState(context.Background(), filepath.Dir(fixture.localState.localStorageDir))
	t.Cleanup(fresh.Close)
	stored, err := fresh.LoadDeviceLocalKeyMaterial()
	if err != nil || stored == nil || !testingDeviceKeyLoadEqual(stored, material) {
		t.Fatal("held provider save had not actually committed its current material")
	}
	resetStarted := make(chan struct{})
	resetDone := make(chan *LocalStateResetResult, 1)
	resetErrors := make(chan error, 1)
	go func() {
		close(resetStarted)
		result, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot)
		resetDone <- result
		resetErrors <- err
	}()
	testingAwaitAuthBoundary(t, resetStarted)
	resume()
	if testingProviderKeySaveResult(t, written) != nil || testingProviderKeySaveResult(t, resetErrors) != nil {
		t.Fatal("admitted save or following reset failed")
	}
	result := <-resetDone // resetErrors completion follows this buffered send.
	if result == nil || !result.GetReset() || !testingDeviceKeyLoadEqual(result.GetDeviceLocalKeyMaterial(), material) {
		t.Fatal("reset returned stale pre-write key material")
	}
	stored, err = fresh.LoadDeviceLocalKeyMaterial()
	if err != nil || stored == nil || !testingDeviceKeyLoadEqual(stored, material) {
		t.Fatal("reset did not preserve the committed material in place")
	}
	if secrets, err := fresh.LoadProvideSecretKeys(); err != nil || secrets != nil {
		t.Fatal("genuine reset retained old provider secrets")
	}
}

func TestDeviceLocalKeySavesRejectEqualTokenReplacementAndClosedOwner(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	old := testingProviderKeySaveDevice(t, fixture, 41)
	replacement := testingProviderKeySaveDevice(t, fixture, 43) // Same token and instance, different owner.
	if replacement.SaveKeyMaterial() != nil || replacement.SaveProvideSecretKeys() != nil {
		t.Fatal("equal-token replacement could not save its own state")
	}
	if old.SaveKeyMaterial() == nil || old.SaveProvideSecretKeys() == nil {
		t.Fatal("equal token bytes admitted the previous device owner")
	}
	material, secrets := replacement.GetKeyMaterial(), replacement.GetProvideSecretKeys()
	testingDeviceKeyLoadJoin(t, replacement)
	if replacement.SaveKeyMaterial() == nil || replacement.SaveProvideSecretKeys() == nil {
		t.Fatal("closed owner admitted a key save")
	}
	testingRequireProviderKeyState(t, fixture.localState, material, secrets)
}

func TestDeviceLocalKeySavesWaitForActualSameOwnerPublication(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	device := testingProviderKeySaveDevice(t, fixture, 47)
	if device.SaveKeyMaterial() != nil || device.SaveProvideSecretKeys() != nil {
		t.Fatal("could not seed provider state")
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	t.Cleanup(resume)
	var admissions atomic.Int64
	device.authPublication.testingAfterAdmission = func() {
		if admissions.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	renewed := testingRefreshableJwtWithMarker(t, "provider-key-renewal")
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		fixture.api.setRefreshedByJwt(fixture.initialJwt, renewed)
	}()
	testingAwaitAuthBoundary(t, entered)
	for _, save := range []func() error{device.SaveKeyMaterial, device.SaveProvideSecretKeys} {
		if err := save(); err == nil || err.Error() != localAuthSnapshotSupersededMessage {
			t.Fatal("API-ahead storage admitted a provider key save")
		}
	}
	resume()
	testingAwaitAuthBoundary(t, refreshDone)
	if device.GetClientJwt() != renewed || fixture.localState.GetByClientJwt() != renewed ||
		device.SaveKeyMaterial() != nil || device.SaveProvideSecretKeys() != nil {
		t.Fatal("settled same-owner renewal did not admit explicit key saves")
	}
	if fixture.localState.GetByJwt() != fixture.adminJwt || device.GetAutoSave() {
		t.Fatal("key-save renewal changed auth roles or autosave policy")
	}
}

func TestDeviceLocalKeySaveFailureReportsPartialTwoOperationEffects(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	device := testingProviderKeySaveDevice(t, fixture, 53)
	if device.SaveKeyMaterial() != nil {
		t.Fatal("could not seed the retained material")
	}
	keyPath := filepath.Join(fixture.localState.localStorageDir, ".device_local_key_material")
	retained := keyPath + ".retained-test"
	if err := os.Rename(keyPath, retained); err != nil {
		t.Fatal("could not hold the original key file")
	}
	before, err := os.ReadFile(retained)
	if err != nil || os.Mkdir(keyPath, LocalStorageDirectoryPermissions) != nil {
		t.Fatal("could not install the exact key-write failure")
	}
	if err := device.SaveProvideSecretKeys(); err != nil {
		t.Fatal("first independent secret operation did not commit")
	}
	if err := device.SaveKeyMaterial(); err == nil || err.Error() != "save device key material" {
		t.Fatal("failed second key operation claimed success or exposed raw error")
	}
	secrets, err := fixture.localState.LoadProvideSecretKeys()
	if err != nil || secrets == nil || secrets.Len() != 1 {
		t.Fatal("second-operation failure erased the committed first operation")
	}
	after, err := os.ReadFile(retained)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed key save changed the retained original file")
	}
	if _, err := fixture.localState.LoadDeviceLocalKeyMaterial(); err == nil {
		t.Fatal("key read failure after partial operations became absence")
	}
}

func TestDeviceLocalKeySavesRejectNoProviderAndUnsupportedHostedStore(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture) // Actual provider creation is disabled here.
	if device.SaveKeyMaterial() == nil || device.SaveProvideSecretKeys() == nil {
		t.Fatal("device without a key-producing provider wrote key state")
	}
	// Construct an actual hosted/private-API device. Its shared space does not
	// become a per-tenant persistence authority just because a store exists.
	settings := DefaultDeviceLocalSettings()
	settings.HostedIncompatible = true
	hosted, err := NewPlatformDeviceLocal(
		func([]*connect.ProviderSpec) connect.MultiClientGenerator { return &testingDnsOwnerGenerator{} },
		fixture.networkSpace, fixture.initialJwt, "hosted-key-test", "test", "0", NewId(), settings,
	)
	if err != nil {
		t.Fatal("hosted key-store control could not construct its actual device")
	}
	t.Cleanup(func() { testingJoinPreferenceDevice(t, hosted) })
	if err := hosted.SaveKeyMaterial(); err == nil || err.Error() != providerKeyStoreUnsupportedMessage {
		t.Fatal("unsupported hosted storage admitted a key save")
	}
	if keys, err := fixture.localState.LoadDeviceLocalKeyMaterial(); err != nil || keys != nil {
		t.Fatal("refused key operation modified the local store")
	}
}

func TestDeviceLocalCloseRetiresHeldKeySaveAndJoinsItsLease(t *testing.T) {
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	device := testingProviderKeySaveDevice(t, fixture, 59)
	if device.SaveKeyMaterial() != nil {
		t.Fatal("could not seed the retained material")
	}
	retained := device.GetKeyMaterial()
	device.SetKeyMaterial(NewDeviceLocalKeyMaterial(bytes.Repeat([]byte{61}, 32), nil, nil))
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	t.Cleanup(resume)
	device.testingBeforeProviderKeySaveAdmission = func(string) {
		close(entered)
		<-release
	}
	done := make(chan error, 1)
	go func() { done <- device.SaveKeyMaterial() }()
	testingAwaitAuthBoundary(t, entered)
	device.Close()
	select {
	case <-device.authPublication.Done():
		t.Fatal("Close lost ownership of the held key-save lease")
	default:
	}
	resume()
	writeErr := testingProviderKeySaveResult(t, done)
	stored, err := fixture.localState.LoadDeviceLocalKeyMaterial()
	if err != nil || stored == nil || !testingDeviceKeyLoadEqual(stored, retained) {
		t.Fatal("closed provider key save changed the retained durable identity")
	}
	if writeErr == nil {
		t.Fatal("closed provider key save claimed success")
	}
	testingDeviceKeyLoadJoin(t, device)
}
