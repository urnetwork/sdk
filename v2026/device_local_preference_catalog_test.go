// Catalog controls cross actual DeviceLocal mutation, atomic LocalState files,
// cold manager reconstruction and production RPC; no native listener saves.
package sdk

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Each case invokes an existing public setter and observes its live getter.
// No helper boolean substitutes for the actual storage/application boundary.
type testingCatalogPreference struct {
	name string
	set  func(*DeviceLocal)
	get  func(*DeviceLocal) any
}

// Values exercise every agreed record while avoiding external provider work.
func testingCatalogPreferences() []testingCatalogPreference {
	profile := &PerformanceProfile{WindowType: WindowTypeAuto, AllowDirect: true, PostQuantumEncryption: true}
	clientTransport := DefaultTransportSettings()
	clientTransport.Mode = TransportModeH3
	providerTransport := DefaultProviderTransportSettings()
	providerTransport.Mode = TransportModeH1
	dns := GetDefaultDnsResolverSettings()
	dns.EnableFallback = false
	overrides := NewBlockActionOverrideList()
	overrides.Add(&BlockActionOverride{OverrideId: NewId(), BlockOverride: &BlockOverride{Block: true}})
	return []testingCatalogPreference{
		{name: "log-verbosity", set: func(d *DeviceLocal) { d.SetLogVerbosity(1) }, get: func(d *DeviceLocal) any { return d.GetLogVerbosity() }},
		{name: "control-ip-family-policy", set: func(d *DeviceLocal) { d.SetControlIpFamilyPolicy(IpFamilyPolicyAuto) }, get: func(d *DeviceLocal) any { return d.GetControlIpFamilyPolicy() }},
		{name: "transport-settings", set: func(d *DeviceLocal) { d.SetTransportSettings(clientTransport) }, get: func(d *DeviceLocal) any { return d.GetTransportSettings() }},
		{name: "provider-transport-settings", set: func(d *DeviceLocal) { d.SetProviderTransportSettings(providerTransport) }, get: func(d *DeviceLocal) any { return d.GetProviderTransportSettings() }},
		{name: "performance-profile", set: func(d *DeviceLocal) { d.SetPerformanceProfile(profile) }, get: func(d *DeviceLocal) any { return d.GetPerformanceProfile() }},
		{name: "route-local", set: func(d *DeviceLocal) { d.SetRouteLocal(true) }, get: func(d *DeviceLocal) any { return d.GetRouteLocal() }},
		{name: "blocker-enabled", set: func(d *DeviceLocal) { d.SetBlockerEnabled(true) }, get: func(d *DeviceLocal) any { return d.GetBlockerEnabled() }},
		{name: "block-action-overrides", set: func(d *DeviceLocal) { d.SetBlockActionOverrides(overrides) }, get: func(d *DeviceLocal) any { return d.GetBlockActionOverrides() }},
		{name: "dns-resolver-settings", set: func(d *DeviceLocal) { d.SetDnsResolverSettings(dns) }, get: func(d *DeviceLocal) any { return d.GetDnsResolverSettings() }},
		{name: "routing-tier", set: func(d *DeviceLocal) { d.SetRoutingTier(RoutingTierLight) }, get: func(d *DeviceLocal) any { d.stateLock.Lock(); defer d.stateLock.Unlock(); return d.routingTier }},
		{name: "vpn-interface-while-offline", set: func(d *DeviceLocal) { d.SetVpnInterfaceWhileOffline(true) }, get: func(d *DeviceLocal) any { return d.GetVpnInterfaceWhileOffline() }},
		{name: "allow-foreground", set: func(d *DeviceLocal) { d.SetAllowForeground(true) }, get: func(d *DeviceLocal) any { return d.GetAllowForeground() }},
		{name: "can-show-rating-dialog", set: func(d *DeviceLocal) { d.SetCanShowRatingDialog(false) }, get: func(d *DeviceLocal) any { return d.GetCanShowRatingDialog() }},
		{name: "can-prompt-intro-funnel", set: func(d *DeviceLocal) { d.SetCanPromptIntroFunnel(false) }, get: func(d *DeviceLocal) any { return d.GetCanPromptIntroFunnel() }},
		{name: "can-refer", set: func(d *DeviceLocal) { d.SetCanRefer(true) }, get: func(d *DeviceLocal) any { return d.GetCanRefer() }},
		{name: "provide-network-mode", set: func(d *DeviceLocal) { d.SetProvideNetworkMode(ProvideNetworkModeAll) }, get: func(d *DeviceLocal) any { return d.GetProvideNetworkMode() }},
		{name: "provide-mode", set: func(d *DeviceLocal) { d.SetProvideMode(ProvideModeNone) }, get: func(d *DeviceLocal) any { return d.GetProvideMode() }},
		{name: "provide-control-mode", set: func(d *DeviceLocal) { d.SetProvideControlMode(ProvideControlModeManual) }, get: func(d *DeviceLocal) any { return d.GetProvideControlMode() }},
	}
}

// Global preferences restore the test process's prior controls on return.
func testingPreserveCatalogGlobals(t *testing.T) {
	t.Helper()
	level, policy := GetLogVerbosity(), GetControlIpFamilyPolicy()
	t.Cleanup(func() {
		if err := SetLogVerbosity(level); err != nil {
			t.Error(err)
		}
		SetControlIpFamilyPolicy(policy)
	})
}

// Canonical public values avoid pointer-identity assertions after cold decode.
func testingCatalogValue(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal("catalog value did not encode")
	}
	return data
}

func TestDeviceLocalPreferenceCatalogDefaultOffEqualSaveAndColdLoad(t *testing.T) {
	testingPreserveCatalogGlobals(t)
	directory := t.TempDir()
	manager, fixture := testingPreferenceSpaceAt(t, directory)
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	device.SetUpgradeMuxSettings(connect.DefaultUpgradeMuxSettings())
	preferences := testingCatalogPreferences()
	for _, preference := range preferences {
		preference.set(device)
		file, _ := localPreferenceFile(preference.name)
		if _, err := os.Stat(filepath.Join(fixture.localState.localStorageDir, file)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("default-off setter persisted %s", preference.name)
		}
	}
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	values := map[string][]byte{}
	records := map[string][]byte{}
	for _, preference := range preferences {
		file, _ := localPreferenceFile(preference.name)
		path := filepath.Join(fixture.localState.localStorageDir, file)
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("enable implicitly saved a catalog snapshot")
		}
		// Every explicit equal mutation must repair the missing durable record.
		preference.set(device)
		result := device.GetLastLocalStateSaveResult()
		if result == nil || result.GetPreference() != preference.name || !result.GetSaved() || result.GetError() != "" {
			t.Fatalf("catalog mutation did not report durable success: %s", preference.name)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("catalog first save was not committed: %s", preference.name)
		}
		records[preference.name] = data
		values[preference.name] = testingCatalogValue(t, preference.get(device))
	}
	testingJoinPreferenceDevice(t, device)
	manager.Close()
	_, fresh := testingPreferenceSpaceAt(t, directory)
	restored := testingPreferenceDevice(t, fresh)
	restored.SetUpgradeMuxSettings(connect.DefaultUpgradeMuxSettings())
	if restored.GetBlockerEnabled() || restored.GetTransportSettings().Mode != TransportModeAuto || restored.GetPerformanceProfile() != nil {
		t.Fatal("constructor implicitly restored catalog preferences")
	}
	loaded, err := restored.Load()
	if err != nil || loaded == nil || restored.GetAutoSave() || testingPreferenceConsumer(restored) != nil {
		t.Fatal("explicit catalog load failed or selected a consumer without current intent")
	}
	for _, preference := range preferences {
		if !loaded.GetHasPreference(preference.name) || loaded.GetPreferenceError(preference.name) != "" ||
			!bytes.Equal(testingCatalogValue(t, preference.get(restored)), values[preference.name]) {
			t.Fatalf("cold Load did not apply the saved catalog value: %s", preference.name)
		}
		file, _ := localPreferenceFile(preference.name)
		data, err := os.ReadFile(filepath.Join(fresh.localState.localStorageDir, file))
		if err != nil || !bytes.Equal(data, records[preference.name]) {
			t.Fatal("close/load changed a catalog record")
		}
	}
	if loaded.GetHasPreference("private-path/token") || loaded.GetPreferenceError("private-path/token") != localPreferenceUnknownMessage {
		t.Fatal("unknown preference looked like healthy absence or echoed caller input")
	}
	if auth, err := fresh.localState.GetAuthStateSnapshot(); err != nil ||
		auth.GetByJwt() != fixture.adminJwt || auth.GetByClientJwt() != fixture.initialJwt || auth.GetInstanceId().Cmp(fixture.instanceId) != 0 {
		t.Fatal("catalog restoration changed authentication")
	}
}

func TestDeviceLocalPreferenceCatalogFailedCommitPreservesEveryLiveValue(t *testing.T) {
	testingPreserveCatalogGlobals(t)
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	device.SetUpgradeMuxSettings(connect.DefaultUpgradeMuxSettings())
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	fixture.localState.testingBeforePreferenceCommit = func(string) error { return errors.New("private path and credential must not escape") }
	for _, preference := range testingCatalogPreferences() {
		before := testingCatalogValue(t, preference.get(device))
		preference.set(device)
		result := device.GetLastLocalStateSaveResult()
		if result == nil || result.GetSaved() || result.GetError() != "save "+preference.name ||
			!bytes.Equal(before, testingCatalogValue(t, preference.get(device))) {
			t.Fatalf("failed catalog commit changed live policy or reported success: %s", preference.name)
		}
		file, _ := localPreferenceFile(preference.name)
		if _, err := os.Stat(filepath.Join(fixture.localState.localStorageDir, file)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed atomic catalog commit replaced its absent final leaf")
		}
	}
}

func TestDeviceLocalPreferenceCatalogCriticalNullIsNotAbsent(t *testing.T) {
	for _, name := range []string{"performance-profile", "dns-resolver-settings", "transport-settings", "provider-transport-settings", "block-action-overrides", "routing-tier", "provide-control-mode", "vpn-interface-while-offline", "allow-foreground"} {
		manager, fixture := testingPreferenceSpaceAt(t, t.TempDir())
		fixture.seedDistinctLogin(t)
		device := testingPreferenceDevice(t, fixture)
		file, _ := localPreferenceFile(name)
		path := filepath.Join(fixture.localState.localStorageDir, file)
		if err := os.WriteFile(path, []byte("null"), LocalStorageFilePermissions); err != nil {
			t.Fatal(err)
		}
		// No current consumer does not make security/permission policy optional.
		if result, err := device.Load(); result != nil || err == nil || err.Error() != "load "+name {
			t.Fatalf("null critical policy became healthy absence: %s", name)
		}
		if device.GetConnectLocation() != nil || testingPreferenceConsumer(device) != nil {
			t.Fatal("failed policy load constructed a consumer")
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "null" {
			t.Fatal("failed policy load deleted its evidence")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		result, err := device.Load()
		if err != nil || result == nil || result.GetHasPreference(name) || result.GetPreferenceError(name) != "" {
			t.Fatal("genuine absence was not healthy")
		}
		testingJoinPreferenceDevice(t, device)
		manager.Close()
	}
}

func TestDeviceLocalPreferenceCatalogRequiredReadFailurePrecedesAllAdoption(t *testing.T) {
	for _, preference := range localPreferenceCatalog {
		if preference.optional {
			continue
		}
		manager, fixture := testingPreferenceSpaceAt(t, t.TempDir())
		fixture.seedDistinctLogin(t)
		device := testingPreferenceDevice(t, fixture)
		target := testingSpecificPreferenceLocation()
		if err := fixture.localState.SetConnectLocation(target); err != nil {
			t.Fatal(err)
		}
		if err := fixture.localState.SetCanRefer(true); err != nil {
			t.Fatal(err)
		}
		// A directory is a real type/read failure for every record format.
		path := filepath.Join(fixture.localState.localStorageDir, preference.file)
		if err := os.Mkdir(path, LocalStorageDirectoryPermissions); err != nil {
			t.Fatal(err)
		}
		result, err := device.Load()
		if result != nil || err == nil || err.Error() != "load "+preference.name ||
			device.GetCanRefer() || testingPreferenceConsumer(device) != nil || device.GetConnectLocation() != nil {
			t.Fatalf("required catalog failure partially adopted startup preferences: %s", preference.name)
		}
		if stored, err := fixture.localState.LoadConnectLocation(); err != nil || !connectLocationValuesEqual(stored, target) {
			t.Fatal("failed policy load changed current intent")
		}
		testingJoinPreferenceDevice(t, device)
		manager.Close()
	}
}

func TestDeviceLocalPreferenceCatalogOptionalErrorsKeepSpecificDestination(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	target := testingSpecificPreferenceLocation()
	if err := fixture.localState.SetConnectLocation(target); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{".default_location", ".can_refer", ".can_prompt_intro_funnel", ".can_show_rating_dialog", ".log_verbosity"} {
		if err := os.WriteFile(filepath.Join(fixture.localState.localStorageDir, file), []byte("malformed"), LocalStorageFilePermissions); err != nil {
			t.Fatal(err)
		}
	}
	device := testingPreferenceDevice(t, fixture)
	result, err := device.Load()
	if err != nil || result == nil || testingPreferenceConsumer(device) == nil || !connectLocationValuesEqual(device.GetConnectLocation(), target) {
		t.Fatal("optional unused preference failure denied the actual specific destination")
	}
	for _, name := range []string{"default-location", "can-refer", "can-prompt-intro-funnel", "can-show-rating-dialog", "log-verbosity"} {
		if result.GetPreferenceError(name) == "" || result.GetHasPreference(name) {
			t.Fatal("optional failure became absence")
		}
	}
}

func TestDeviceLocalPreferenceCatalogLoadWithAutoSaveNeverWritesReplay(t *testing.T) {
	testingPreserveCatalogGlobals(t)
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	device.SetUpgradeMuxSettings(connect.DefaultUpgradeMuxSettings())
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	for _, preference := range testingCatalogPreferences() {
		preference.set(device)
	}
	last := device.GetLastLocalStateSaveResult()
	var writes atomic.Int64
	fixture.localState.testingBeforePreferenceCommit = func(string) error { writes.Add(1); return errors.New("load attempted a preference write") }
	fixture.localState.testingBeforeLocationCommit = fixture.localState.testingBeforePreferenceCommit
	if result, err := device.Load(); err != nil || result == nil || !device.GetAutoSave() {
		t.Fatal("Load changed autosave or failed its read-only replay")
	}
	if writes.Load() != 0 || device.GetLastLocalStateSaveResult() != last {
		t.Fatal("Load autosaved its catalog replay")
	}
	testingJoinPreferenceDevice(t, device)
	if writes.Load() != 0 {
		t.Fatal("Close persisted transient catalog state")
	}
}

func TestDeviceLocalPreferenceCatalogLegacyFunnelAndEmptyOverrides(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	for _, lastPrompted := range []time.Time{time.Now().Add(-6 * 24 * time.Hour), time.Now().Add(-24 * time.Hour)} {
		data, err := json.Marshal(lastPrompted)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(fixture.localState.localStorageDir, ".can_prompt_intro_funnel")
		if err := os.WriteFile(path, data, LocalStorageFilePermissions); err != nil {
			t.Fatal(err)
		}
		if err := fixture.localState.SetBlockActionOverrides(NewBlockActionOverrideList()); err != nil {
			t.Fatal(err)
		}
		result, err := device.Load()
		if err != nil || result == nil || !result.GetHasPreference("can-prompt-intro-funnel") ||
			device.GetCanPromptIntroFunnel() != (time.Since(lastPrompted) > 5*24*time.Hour) || device.GetBlockActionOverrides().Len() != 0 {
			t.Fatal("legacy funnel timestamp or explicit empty override list lost its meaning")
		}
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, data) {
			t.Fatal("Load migrated a legacy record by writing")
		}
	}
}

func TestDeviceLocalPreferenceCatalogRpcFailureAndCallbackReentry(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	server, client := testingPreferenceRpc(t, device)
	fixture.localState.testingBeforePreferenceCommit = func(name string) error {
		if name == ".blocker_enabled" {
			return errors.New("private forced failure")
		}
		return nil
	}
	results := make(chan *DeviceLocalSaveResult, 2)
	sub := device.AddLocalStateSaveListener(testingPreferenceRpcSaveListener(func(result *DeviceLocalSaveResult) {
		for _, stateLock := range []*sync.Mutex{&server.stateLock, &device.preferenceMutationLock, &fixture.api.authMutationLock, &fixture.localState.authStateLock} {
			if !stateLock.TryLock() {
				t.Error("catalog Sync notified under a service/owner lock")
				return
			}
			stateLock.Unlock()
		}
		var response *DeviceRemoteSyncResponse
		if err := server.Sync(&DeviceRemoteSyncRequest{InstanceId: device.instanceId, RpcVersion: DeviceRpcVersion}, &response); err != nil {
			t.Error("catalog callback could not reenter actual Sync")
		}
		select {
		case results <- result:
		default:
			t.Error("catalog Sync published an extra mutation result")
		}
	}))
	defer sub.Close()
	request := &DeviceRemoteSyncRequest{InstanceId: device.instanceId, RpcVersion: DeviceRpcVersion}
	request.State.CanRefer.Set(true)
	request.State.BlockerEnabled.Set(true)
	if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.Sync", request, &DeviceRemoteSyncResponse{}); err == nil || err.Error() != "save blocker-enabled" {
		t.Fatal("actual Sync discarded the catalog operation's fixed storage error")
	}
	receiveResult := func() *DeviceLocalSaveResult {
		select {
		case result := <-results:
			return result
		case <-time.After(5 * time.Second):
			t.Fatal("catalog Sync did not publish its completed mutation result")
			return nil
		}
	}
	first, second := receiveResult(), receiveResult()
	if first.GetPreference() != "can-refer" || !first.GetSaved() || second.GetPreference() != "blocker-enabled" ||
		second.GetSaved() || second.GetSequence() != first.GetSequence()+1 || !device.GetCanRefer() || device.GetBlockerEnabled() {
		t.Fatal("partial catalog Sync did not expose ordered real results")
	}
}

func TestDeviceLocalPreferenceCatalogRpcDirectSetterReturnsActualError(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	_, client := testingPreferenceRpc(t, device)
	fixture.localState.testingBeforePreferenceCommit = func(string) error { return errors.New("private path") }
	var response any
	err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.SetBlockerEnabled", true, &response)
	if err == nil || err.Error() != "save blocker-enabled" || strings.Contains(err.Error(), "private") || device.GetBlockerEnabled() {
		t.Fatal("direct catalog RPC reported false success or changed live policy")
	}
}

func TestDeviceLocalPreferenceCatalogOverrideMutationIsOwnedAndAtomic(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	override := &BlockActionOverride{OverrideId: NewId(), BlockOverride: &BlockOverride{Block: true}}
	device.AddBlockActionOverride(override)
	stored := fixture.localState.GetBlockActionOverrides()
	if stored == nil || stored.Len() != 1 || !stored.Get(0).BlockOverride.Block {
		t.Fatal("add did not persist the actual override")
	}
	override.BlockOverride.Block = false
	if !device.GetBlockActionOverrides().Get(0).BlockOverride.Block {
		t.Fatal("caller mutation changed the owned live preference")
	}
	fixture.localState.testingBeforePreferenceCommit = func(string) error { return errors.New("remove interruption") }
	device.RemoveBlockActionOverride(override.OverrideId)
	if result := device.GetLastLocalStateSaveResult(); result.GetSaved() || result.GetError() != "save block-action-overrides" {
		t.Fatal("failed remove claimed durability")
	}
	if !reflect.DeepEqual(newBlockActionOverridesRpc(device.GetBlockActionOverrides()), newBlockActionOverridesRpc(stored)) {
		t.Fatal("failed override removal changed the installed rules")
	}
	fixture.localState.testingBeforePreferenceCommit = nil
	device.RemoveBlockActionOverride(override.OverrideId)
	if stored := fixture.localState.GetBlockActionOverrides(); stored == nil || stored.Len() != 0 || device.GetBlockActionOverrides().Len() != 0 {
		t.Fatal("healthy remove did not durably clear rules")
	}
}

// Interrupted replacement keeps the old complete policy for a fresh manager.
func TestDeviceLocalPreferenceCatalogInterruptedReplaceColdLoadKeepsOriginal(t *testing.T) {
	directory := t.TempDir()
	manager, fixture := testingPreferenceSpaceAt(t, directory)
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	device.SetBlockerEnabled(true)
	path := filepath.Join(fixture.localState.localStorageDir, ".blocker_enabled")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fixture.localState.testingBeforePreferenceCommit = func(string) error { return errors.New("interrupted before rename") }
	device.SetBlockerEnabled(false)
	if result := device.GetLastLocalStateSaveResult(); result.GetSaved() || result.GetError() != "save blocker-enabled" || !device.GetBlockerEnabled() {
		t.Fatal("interrupted policy write changed live truth")
	}
	testingJoinPreferenceDevice(t, device)
	manager.Close()
	_, fresh := testingPreferenceSpaceAt(t, directory)
	restored := testingPreferenceDevice(t, fresh)
	if result, err := restored.Load(); err != nil || result == nil || !restored.GetBlockerEnabled() {
		t.Fatal("fresh manager lost the original policy after interrupted replacement")
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, original) {
		t.Fatal("interruption truncated the previously committed record")
	}
}

// The same-owner API storage callback renews credentials without discarding
// policy records; the still-accepted device may continue saving afterwards.
func TestDeviceLocalPreferenceCatalogSameOwnerRenewalPreservesAllRecords(t *testing.T) {
	testingPreserveCatalogGlobals(t)
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	device.SetUpgradeMuxSettings(connect.DefaultUpgradeMuxSettings())
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	records := map[string][]byte{}
	for _, preference := range testingCatalogPreferences() {
		preference.set(device)
		file, _ := localPreferenceFile(preference.name)
		data, err := os.ReadFile(filepath.Join(fixture.localState.localStorageDir, file))
		if err != nil {
			t.Fatal(err)
		}
		records[file] = data
	}
	renewed := testingRefreshableJwtWithMarker(t, "catalog-renewed")
	if !fixture.api.setRefreshedByJwt(fixture.initialJwt, renewed) {
		t.Fatal("same-owner API renewal failed")
	}
	if result, err := device.Load(); err != nil || result == nil {
		t.Fatal("settled same-owner rotation invalidated Load")
	}
	for file, original := range records {
		if data, err := os.ReadFile(filepath.Join(fixture.localState.localStorageDir, file)); err != nil || !bytes.Equal(data, original) {
			t.Fatal("token renewal or read-only Load changed a policy record")
		}
	}
	device.SetBlockerEnabled(false)
	if result := device.GetLastLocalStateSaveResult(); result == nil || !result.GetSaved() || result.GetError() != "" || device.GetBlockerEnabled() {
		t.Fatal("settled renewal prevented an owned catalog mutation")
	}
	if auth, err := fixture.localState.GetAuthStateSnapshot(); err != nil || auth.GetByJwt() != fixture.adminJwt || auth.GetByClientJwt() != renewed {
		t.Fatal("catalog renewal combined auth roles")
	}
}

// A delayed producer from a genuinely reset owner cannot reclaim any record,
// including a non-routing permission/UI preference, through its old setter.
func TestDeviceLocalPreferenceCatalogRetiredOwnerCannotRecreateRecords(t *testing.T) {
	testingPreserveCatalogGlobals(t)
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	oldDevice := testingPreferenceDevice(t, fixture)
	if err := oldDevice.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fixture.networkSpace.GetAuthStateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	reset, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot)
	if err != nil || reset == nil || !reset.GetReset() {
		t.Fatal("actual owner reset failed")
	}
	fixture.seedDistinctLogin(t)
	current := testingPreferenceDevice(t, fixture)
	if err := current.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	current.SetBlockerEnabled(false)
	for _, preference := range testingCatalogPreferences() {
		preference.set(oldDevice)
		if result := oldDevice.GetLastLocalStateSaveResult(); result == nil || result.GetSaved() || result.GetError() == "" {
			t.Fatal("retired catalog producer reported success")
		}
		file, _ := localPreferenceFile(preference.name)
		data, err := os.ReadFile(filepath.Join(fixture.localState.localStorageDir, file))
		if preference.name == "blocker-enabled" {
			if err != nil || string(data) != "false" || current.GetBlockerEnabled() {
				t.Fatal("retired setter overwrote the new owner's explicit policy")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retired setter recreated %s after reset", preference.name)
		}
	}
}

func TestDeviceLocalPreferenceCatalogDisablingAutoSavePreservesDurablePolicy(t *testing.T) {
	directory := t.TempDir()
	manager, fixture := testingPreferenceSpaceAt(t, directory)
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	device.SetBlockerEnabled(true)
	if err := device.SetAutoSave(false); err != nil {
		t.Fatal(err)
	}
	device.SetBlockerEnabled(false)
	if device.GetBlockerEnabled() {
		t.Fatal("disabled persistence prevented a live-only preference mutation")
	}
	testingJoinPreferenceDevice(t, device)
	manager.Close()
	_, fresh := testingPreferenceSpaceAt(t, directory)
	restored := testingPreferenceDevice(t, fresh)
	if result, err := restored.Load(); err != nil || result == nil || !restored.GetBlockerEnabled() {
		t.Fatal("disabled autosave or Close rewrote the last durable policy")
	}
}

// A held read validates the actual complete owner again before any catalog
// application. Replacing only the API/storage owner cannot publish old policy.
func TestDeviceLocalPreferenceCatalogHeldLoadRejectsOwnerReset(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	if err := fixture.localState.SetBlockerEnabled(true); err != nil {
		t.Fatal(err)
	}
	device := testingPreferenceDevice(t, fixture)
	read := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	defer resumeOnce.Do(func() { close(resume) })
	device.testingBeforePreferenceApply = func() { close(read); <-resume }
	done := make(chan error, 1)
	go func() { _, err := device.Load(); done <- err }()
	testingAwaitAuthBoundary(t, read)
	snapshot, err := fixture.networkSpace.GetAuthStateSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	reset, err := fixture.networkSpace.ResetLocalStateIfCurrent(snapshot)
	if err != nil || reset == nil || !reset.GetReset() {
		t.Fatal("held catalog's actual owner reset failed")
	}
	resumeOnce.Do(func() { close(resume) })
	select {
	case err := <-done:
		if err == nil || device.GetBlockerEnabled() {
			t.Fatal("old policy was applied after final owner admission failed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held catalog Load did not finish after reset")
	}
}

// Receives the production setter's mode event, which is generated only when
// the real provider's contract mode actually changes, even if later reversed.
type testingCatalogProvideModeListener func(ProvideMode)

func (self testingCatalogProvideModeListener) ProvideModeChanged(mode ProvideMode) { self(mode) }

func TestDeviceLocalPreferenceCatalogLoadNeverTransientlyProvidesAndManualRestoresRaw(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	if err := fixture.localState.SetProvideMode(ProvideModePublic); err != nil {
		t.Fatal(err)
	}
	if err := fixture.localState.SetProvideControlMode(ProvideControlModeNever); err != nil {
		t.Fatal(err)
	}
	var writes atomic.Int64
	fixture.localState.testingBeforePreferenceCommit = func(string) error { writes.Add(1); return errors.New("unexpected replay write") }
	settings := DefaultDeviceLocalSettings()
	settings.EnableRpc = false
	settings.DisableLogging = true
	settings.AllowProvider = true
	settings.GeneratorFunc = func([]*connect.ProviderSpec) connect.MultiClientGenerator { return &testingDnsOwnerGenerator{} }
	device, err := newDeviceLocalWithOverrides(fixture.networkSpace, fixture.initialJwt, "catalog", "test", "0", fixture.instanceId, settings, connect.NewId())
	if err != nil {
		t.Fatal("actual provider device construction failed")
	}
	t.Cleanup(func() { testingJoinPreferenceDevice(t, device) })
	if device.GetProvideEnabled() || device.GetProvideMode() != ProvideModeNone {
		t.Fatal("constructor implicitly restored raw provide mode")
	}
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	var modes []ProvideMode
	sub := device.AddProvideModeChangeListener(testingCatalogProvideModeListener(func(mode ProvideMode) { modes = append(modes, mode) }))
	defer sub.Close()
	if result, err := device.Load(); err != nil || result == nil {
		t.Fatal("checked Never load failed")
	}
	if device.GetProvideEnabled() || device.GetProvideMode() != ProvideModeNone || len(modes) != 0 {
		t.Fatal("Load transiently enabled a stale raw provider mode under Never")
	}
	if err := fixture.localState.SetProvideControlMode(ProvideControlModeManual); err != nil {
		t.Fatal(err)
	}
	if result, err := device.Load(); err != nil || result == nil {
		t.Fatal("checked Manual load failed")
	}
	if !device.GetProvideEnabled() || device.GetProvideMode() != ProvideModePublic || !reflect.DeepEqual(modes, []ProvideMode{ProvideModePublic}) {
		t.Fatal("Manual did not restore the actual explicitly saved provider mode")
	}
	if writes.Load() != 0 || fixture.localState.GetProvideMode() != ProvideModePublic ||
		device.GetLastLocalStateSaveResult() != nil || !device.GetAutoSave() {
		t.Fatal("constructor or Load persisted a derived provider mode")
	}
}
