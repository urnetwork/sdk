package sdk

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect"
)

// TestToConnectPerformanceProfileWindowType pins the window type mapping:
// quality and speed fix a window; auto and unset map to the connect auto
// window type (the same as no profile), not a silently fixed quality window.
func TestToConnectPerformanceProfileWindowType(t *testing.T) {
	if toConnectPerformanceProfile(nil) != nil {
		t.Fatalf("nil profile must map to nil")
	}

	quality := toConnectPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeQuality})
	if quality.WindowType != connect.WindowTypeQuality {
		t.Fatalf("quality window type = %v", quality.WindowType)
	}
	if _, _, ok := quality.FixedWindow(); !ok {
		t.Fatalf("quality profile must fix a window")
	}

	speed := toConnectPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeSpeed})
	if speed.WindowType != connect.WindowTypeSpeed {
		t.Fatalf("speed window type = %v", speed.WindowType)
	}

	auto := toConnectPerformanceProfile(&PerformanceProfile{
		WindowType:  WindowTypeAuto,
		AllowDirect: true,
	})
	if auto.WindowType != connect.WindowTypeAuto {
		t.Fatalf("auto window type = %v", auto.WindowType)
	}
	if _, _, ok := auto.FixedWindow(); ok {
		t.Fatalf("auto profile must not fix a window")
	}
	// the orthogonal settings carry through under auto
	if !auto.AllowDirect {
		t.Fatalf("auto profile must carry allow direct")
	}
	pqe := toConnectPerformanceProfile(&PerformanceProfile{
		WindowType:            WindowTypeAuto,
		PostQuantumEncryption: true,
	})
	if !pqe.PostQuantumEncryption {
		t.Fatalf("auto profile must carry post quantum encryption")
	}

	// unset means auto, not quality
	unset := toConnectPerformanceProfile(&PerformanceProfile{})
	if unset.WindowType != connect.WindowTypeAuto {
		t.Fatalf("unset window type = %v", unset.WindowType)
	}
}

// TestPerformanceProfilesEqualAuto verifies change detection compares the
// installed behavior: nil, unset, and explicit auto are equivalent when the
// orthogonal settings match, and auto ignores its unused window-size value.
func TestPerformanceProfilesEqualAuto(t *testing.T) {
	autoProfile := &PerformanceProfile{WindowType: WindowTypeAuto}

	if !performanceProfilesEqual(autoProfile, &PerformanceProfile{WindowType: WindowTypeAuto}) {
		t.Fatalf("identical auto profiles must be equal")
	}
	if !performanceProfilesEqual(autoProfile, nil) {
		t.Fatalf("auto profile and nil install the same behavior")
	}
	if !performanceProfilesEqual(autoProfile, &PerformanceProfile{
		WindowSize: &WindowSizeSettings{
			WindowSizeMin: 17,
			WindowSizeMax: 23,
		},
	}) {
		t.Fatalf("auto profile must ignore its unused window size")
	}
	if performanceProfilesEqual(autoProfile, &PerformanceProfile{WindowType: WindowTypeQuality}) {
		t.Fatalf("auto and quality profiles must differ")
	}
	if performanceProfilesEqual(
		autoProfile,
		&PerformanceProfile{WindowType: WindowTypeAuto, AllowDirect: true},
	) {
		t.Fatalf("allow direct must be part of profile equality")
	}
	if performanceProfilesEqual(
		autoProfile,
		&PerformanceProfile{WindowType: WindowTypeAuto, PostQuantumEncryption: true},
	) {
		t.Fatalf("post quantum encryption must be part of profile equality")
	}
}

func TestPerformanceProfilesEqualOmittedFixedWindowUsesEffectiveDefault(t *testing.T) {
	omitted := &PerformanceProfile{WindowType: WindowTypeQuality}
	explicit := &PerformanceProfile{
		WindowType: WindowTypeQuality,
		WindowSize: &WindowSizeSettings{
			WindowSizeMin:            1,
			WindowSizeMax:            1,
			WindowSizeHardMax:        4,
			WindowSizeReconnectScale: 1.0,
			KeepHealthiestCount:      1,
		},
	}
	if !performanceProfilesEqual(omitted, explicit) {
		t.Fatalf("omitted and explicit effective default windows must be equal")
	}
	explicit.WindowSize.WindowSizeMax = 2
	if performanceProfilesEqual(omitted, explicit) {
		t.Fatalf("a changed effective fixed window must not be equal")
	}
}

// TestClonePerformanceProfileOwnsNestedWindowSize prevents a caller-owned
// gomobile model from silently mutating stored or callback state.
func TestClonePerformanceProfileOwnsNestedWindowSize(t *testing.T) {
	source := &PerformanceProfile{
		WindowType: WindowTypeSpeed,
		WindowSize: &WindowSizeSettings{
			WindowSizeMin: 2,
			WindowSizeMax: 8,
		},
		AllowDirect: true,
	}
	cloned := clonePerformanceProfile(source)
	source.WindowType = WindowTypeQuality
	source.WindowSize.WindowSizeMin = 5

	if cloned.WindowType != WindowTypeSpeed {
		t.Fatalf("clone window type changed with source: %v", cloned.WindowType)
	}
	if cloned.WindowSize == nil || cloned.WindowSize.WindowSizeMin != 2 {
		t.Fatalf("clone nested window size changed with source: %+v", cloned.WindowSize)
	}
	if !cloned.AllowDirect {
		t.Fatalf("clone lost allow-direct setting")
	}
}

func TestDeviceLocalPerformanceProfileOwnsSetAndGetValues(t *testing.T) {
	device := &DeviceLocal{
		settings:                          DefaultDeviceLocalSettings(),
		performanceProfileChangeListeners: connect.NewCallbackList[PerformanceProfileChangeListener](),
	}
	source := &PerformanceProfile{
		WindowType: WindowTypeSpeed,
		WindowSize: &WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4},
	}
	device.SetPerformanceProfile(source)
	source.WindowSize.WindowSizeMin = 9

	first := device.GetPerformanceProfile()
	if first.WindowSize == nil || first.WindowSize.WindowSizeMin != 2 {
		t.Fatalf("stored profile followed caller mutation: %+v", first)
	}
	first.WindowSize.WindowSizeMin = 7
	second := device.GetPerformanceProfile()
	if second.WindowSize == nil || second.WindowSize.WindowSizeMin != 2 {
		t.Fatalf("stored profile followed getter mutation: %+v", second)
	}
}

func TestDeviceLocalPerformanceProfileListenersReceiveIndependentValues(t *testing.T) {
	device := &DeviceLocal{
		performanceProfileChangeListeners: connect.NewCallbackList[PerformanceProfileChangeListener](),
	}
	first := &testing_performanceProfileChangeListener{}
	second := &testing_performanceProfileChangeListener{}
	device.performanceProfileChangeListeners.Add(first)
	device.performanceProfileChangeListeners.Add(second)
	source := &PerformanceProfile{
		WindowType: WindowTypeSpeed,
		WindowSize: &WindowSizeSettings{WindowSizeMin: 2},
	}

	device.performanceProfileChanged(source)
	first.performanceProfile.WindowSize.WindowSizeMin = 9

	if source.WindowSize.WindowSizeMin != 2 {
		t.Fatalf("listener mutated callback source: %+v", source.WindowSize)
	}
	if second.performanceProfile.WindowSize.WindowSizeMin != 2 {
		t.Fatalf("one listener mutated another listener's value: %+v", second.performanceProfile.WindowSize)
	}
}

func TestDeviceRemoteExactKnownPerformanceProfileIsNoOp(t *testing.T) {
	device := &DeviceRemote{}
	device.lastKnownState.PerformanceProfile.Set(&PerformanceProfile{
		WindowType: WindowTypeAuto,
		WindowSize: &WindowSizeSettings{
			WindowSizeMin: 9,
			WindowSizeMax: 9,
		},
	})

	device.SetPerformanceProfile(&PerformanceProfile{
		WindowType: WindowTypeAuto,
		WindowSize: &WindowSizeSettings{
			WindowSizeMin: 9,
			WindowSizeMax: 9,
		},
	})

	if device.state.PerformanceProfile.IsSet {
		t.Fatalf("exact known profile was queued for rpc")
	}
}

func TestDeviceRemoteEquivalentBehaviorQueuesDifferentStoredValue(t *testing.T) {
	device := &DeviceRemote{}
	device.lastKnownState.PerformanceProfile.Set(nil)

	device.SetPerformanceProfile(&PerformanceProfile{
		WindowType: WindowTypeAuto,
	})

	if !device.state.PerformanceProfile.IsSet {
		t.Fatal("explicit auto value was not queued over a known nil value")
	}
	if device.state.PerformanceProfile.Value == nil {
		t.Fatal("queued explicit auto value was collapsed to nil")
	}
}

func TestDeviceRemoteChangedPerformanceProfileIsQueued(t *testing.T) {
	device := &DeviceRemote{}
	device.lastKnownState.PerformanceProfile.Set(&PerformanceProfile{
		WindowType: WindowTypeAuto,
	})
	source := &PerformanceProfile{
		WindowType: WindowTypeSpeed,
		WindowSize: &WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4},
	}

	device.SetPerformanceProfile(source)
	source.WindowSize.WindowSizeMin = 9

	if !device.state.PerformanceProfile.IsSet {
		t.Fatalf("changed profile was not queued for rpc")
	}
	queued := device.state.PerformanceProfile.Value
	if queued.WindowSize == nil || queued.WindowSize.WindowSizeMin != 2 {
		t.Fatalf("queued profile followed caller mutation: %+v", queued)
	}
}

func TestDeviceLocalEquivalentBehaviorPreservesStoredProfileValue(t *testing.T) {
	device := &DeviceLocal{
		settings:                          DefaultDeviceLocalSettings(),
		performanceProfileChangeListeners: connect.NewCallbackList[PerformanceProfileChangeListener](),
	}
	listener := &testing_performanceProfileChangeListener{}
	device.performanceProfileChangeListeners.Add(listener)

	device.SetPerformanceProfile(&PerformanceProfile{
		WindowType: WindowTypeAuto,
	})

	if profile := device.GetPerformanceProfile(); profile == nil || profile.WindowType != WindowTypeAuto {
		t.Fatalf("stored profile did not preserve explicit auto: %+v", profile)
	}
	listener.with(func() {
		if !listener.event || listener.performanceProfile == nil {
			t.Fatalf("representation change did not reach listeners: %+v", listener.performanceProfile)
		}
	})
}

type testingRefusedPerformanceProfile struct {
	name    string
	profile *PerformanceProfile
}

// testingRefusedPerformanceProfiles are profiles any caller can send (a
// device-rpc client, a page, a native app) whose window the multi client
// cannot install.
func testingRefusedPerformanceProfiles() []testingRefusedPerformanceProfile {
	fixed := func(windowSize *WindowSizeSettings) *PerformanceProfile {
		return &PerformanceProfile{
			WindowType:            WindowTypeQuality,
			WindowSize:            windowSize,
			PostQuantumEncryption: true,
		}
	}
	return []testingRefusedPerformanceProfile{
		{"max below min", fixed(&WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 1})},
		{"negative max", fixed(&WindowSizeSettings{WindowSizeMin: 0, WindowSizeMax: -1})},
		{"negative window", fixed(&WindowSizeSettings{WindowSizeMin: -1, WindowSizeMax: -1})},
		{"negative min", fixed(&WindowSizeSettings{WindowSizeMin: -3, WindowSizeMax: 2})},
		{"negative min p2p only", fixed(&WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4, WindowSizeMinP2pOnly: -1})},
		{"negative hard max", fixed(&WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4, WindowSizeHardMax: -1})},
		{"negative keep healthiest count", fixed(&WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4, KeepHealthiestCount: -1})},
		{"negative ulimit", fixed(&WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4, Ulimit: -1})},
		{"negative reconnect scale", fixed(&WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4, WindowSizeReconnectScale: -1})},
		{"nan reconnect scale", fixed(&WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4, WindowSizeReconnectScale: math.NaN()})},
		{"fixed window with no exit", fixed(&WindowSizeSettings{})},
		{"speed max below min", &PerformanceProfile{
			WindowType: WindowTypeSpeed,
			WindowSize: &WindowSizeSettings{WindowSizeMin: 4, WindowSizeMax: 2},
		}},
		{"auto with an invalid window", &PerformanceProfile{
			WindowType: WindowTypeAuto,
			WindowSize: &WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 1},
		}},
	}
}

// TestValidatePerformanceProfile pins what the device accepts: nil and the
// profiles the apps send (auto; Fixed IP, a window of exactly one exit; a
// window of two to four; a fixed type with the default window) are valid,
// and every profile in testingRefusedPerformanceProfiles is refused.
func TestValidatePerformanceProfile(t *testing.T) {
	valid := []*PerformanceProfile{
		nil,
		{},
		{WindowType: WindowTypeAuto, AllowDirect: true, PostQuantumEncryption: true},
		{WindowType: WindowTypeQuality, WindowSize: &WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1}},
		{WindowType: WindowTypeSpeed, WindowSize: &WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4}},
		{WindowType: WindowTypeQuality},
	}
	for _, performanceProfile := range valid {
		if err := validatePerformanceProfile(performanceProfile); err != nil {
			t.Fatalf("valid profile %+v refused: %v", performanceProfile, err)
		}
	}
	for _, refused := range testingRefusedPerformanceProfiles() {
		if validatePerformanceProfile(refused.profile) == nil {
			t.Fatalf("%s: profile was accepted", refused.name)
		}
	}
}

// TestNormalizeSavedPerformanceProfile pins how a saved profile reads back: a
// valid one is unchanged, and one whose window the multi client cannot
// install reads back in auto mode with its direct and post-quantum choices.
func TestNormalizeSavedPerformanceProfile(t *testing.T) {
	valid := &PerformanceProfile{
		WindowType: WindowTypeQuality,
		WindowSize: &WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1},
	}
	if normalizeSavedPerformanceProfile(valid) != valid {
		t.Fatal("a valid saved profile was changed")
	}
	for _, refused := range testingRefusedPerformanceProfiles() {
		saved := clonePerformanceProfile(refused.profile)
		saved.AllowDirect = true
		normalized := normalizeSavedPerformanceProfile(saved)
		if normalized.WindowType != WindowTypeAuto || normalized.WindowSize != nil ||
			!normalized.AllowDirect || normalized.PostQuantumEncryption != refused.profile.PostQuantumEncryption {
			t.Fatalf("%s: saved profile read back as %+v", refused.name, normalized)
		}
	}
}

// testingPerformanceProfileDevice is an accepted device with autosave on, a
// live multi client, and a valid profile in force and saved.
func testingPerformanceProfileDevice(t *testing.T) (*DeviceLocal, string, *PerformanceProfile) {
	t.Helper()
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	device := testingPreferenceDevice(t, fixture)
	if err := device.SetAutoSave(true); err != nil {
		t.Fatal(err)
	}
	device.SetConnectLocation(testingSpecificPreferenceLocation())
	if _, ok := testingPreferenceConsumer(device).(*connect.RemoteUserNatMultiClient); !ok {
		t.Fatal("the device has no multi client to apply a profile to")
	}
	previous := &PerformanceProfile{
		WindowType: WindowTypeQuality,
		WindowSize: &WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1},
	}
	device.SetPerformanceProfile(previous)
	if !performanceProfileValuesEqual(device.GetPerformanceProfile(), previous) {
		t.Fatal("a valid profile was not applied")
	}
	path := filepath.Join(fixture.localState.localStorageDir, ".performance_profile")
	return device, path, previous
}

// TestDeviceLocalRefusesInvalidPerformanceProfile is the direct API path (the
// mobile and C APIs). With a multi client, an invalid window panicked in the
// caller's goroutine after the device had stored the profile, and with
// autosave saved it, so a later re-apply of it panicked again. The device now
// refuses it without a panic, and the previous profile stays in force: in the
// getter, in the saved record, and with no change notification.
func TestDeviceLocalRefusesInvalidPerformanceProfile(t *testing.T) {
	device, path, previous := testingPerformanceProfileDevice(t)
	savedBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	listener := &testing_performanceProfileChangeListener{}
	sub := device.AddPerformanceProfileChangeListener(listener)
	defer sub.Close()

	for _, refused := range testingRefusedPerformanceProfiles() {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s: SetPerformanceProfile panicked: %v", refused.name, r)
				}
			}()
			device.SetPerformanceProfile(refused.profile)
		}()
		if performanceProfile := device.GetPerformanceProfile(); !performanceProfileValuesEqual(performanceProfile, previous) {
			t.Fatalf("%s: the device holds %+v", refused.name, performanceProfile)
		}
		if saved, err := os.ReadFile(path); err != nil || !bytes.Equal(saved, savedBefore) {
			t.Fatalf("%s: the saved profile changed: %s", refused.name, saved)
		}
	}
	listener.with(func() {
		if listener.event {
			t.Fatalf("a refused profile reached the listeners: %+v", listener.performanceProfile)
		}
	})

	// the getter only ever returns a profile that applies, so changing one
	// option on the profile in force works after the refusals
	changed := device.GetPerformanceProfile()
	changed.PostQuantumEncryption = true
	device.SetPerformanceProfile(changed)
	if !device.GetPerformanceProfile().PostQuantumEncryption {
		t.Fatal("a valid change after the refusals was not applied")
	}
}

// TestDeviceLocalRefusesInvalidInitialPerformanceProfile mirrors the hosted
// proxy, which sets the initial profile a client sent (saved on the server)
// before the device connects: an invalid one is refused, so the device
// starts in auto instead of building its multi client from it.
func TestDeviceLocalRefusesInvalidInitialPerformanceProfile(t *testing.T) {
	settings := DefaultDeviceLocalSettings()
	settings.HostedIncompatible = true
	device := &DeviceLocal{
		settings:                          settings,
		performanceProfileChangeListeners: connect.NewCallbackList[PerformanceProfileChangeListener](),
	}
	for _, refused := range testingRefusedPerformanceProfiles() {
		device.SetPerformanceProfile(refused.profile)
		if performanceProfile := device.GetPerformanceProfile(); performanceProfile != nil {
			t.Fatalf("%s: the device holds %+v", refused.name, performanceProfile)
		}
	}
}

// TestDeviceLocalRpcRefusesInvalidPerformanceProfile is the device-rpc path,
// for any rpc client. The handler panicked inside the multi client; the rpc
// recovered it but never answered, and the device had already stored and
// saved the profile. The rpc now answers with the fixed refusal and the
// previous profile stays in force. A sync that carries an invalid profile (one
// a remote from before the refusal queued) completes and reports the profile
// in force, instead of failing that sync and every later one.
func TestDeviceLocalRpcRefusesInvalidPerformanceProfile(t *testing.T) {
	device, path, previous := testingPerformanceProfileDevice(t)
	savedBefore, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, client := testingPreferenceRpc(t, device)

	for _, refused := range testingRefusedPerformanceProfiles() {
		var response RpcVoid
		err := testingPreferenceRpcCall(
			t,
			client,
			"DeviceLocalRpc.SetPerformanceProfile",
			&DevicePerformanceProfile{PerformanceProfile: refused.profile},
			&response,
		)
		if err == nil || err.Error() != "invalid performance-profile" {
			t.Fatalf("%s: the rpc did not report the refusal: %v", refused.name, err)
		}
		var current *DevicePerformanceProfile
		if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.GetPerformanceProfile", RpcNoArg(0), &current); err != nil {
			t.Fatalf("%s: the rpc stopped serving after the refusal: %v", refused.name, err)
		}
		if !performanceProfileValuesEqual(current.PerformanceProfile, previous) {
			t.Fatalf("%s: the device holds %+v", refused.name, current.PerformanceProfile)
		}

		target := testingSpecificPreferenceLocation()
		request := &DeviceRemoteSyncRequest{InstanceId: device.instanceId, RpcVersion: DeviceRpcVersion}
		request.State.PerformanceProfile.Set(refused.profile)
		// applied after the profile, so it shows the rest of the sync still runs
		request.State.Location.Set(newDeviceRemoteConnectLocation(target))
		syncResponse := &DeviceRemoteSyncResponse{}
		if err := testingPreferenceRpcCall(t, client, "DeviceLocalRpc.Sync", request, syncResponse); err != nil {
			t.Fatalf("%s: a sync carrying the profile failed: %v", refused.name, err)
		}
		if syncResponse.Error != "" || !syncResponse.State.PerformanceProfile.IsSet ||
			!performanceProfileValuesEqual(syncResponse.State.PerformanceProfile.Value, previous) {
			t.Fatalf("%s: the sync did not report the profile in force: %+v", refused.name, syncResponse.State.PerformanceProfile)
		}
		if !connectLocationValuesEqual(device.GetConnectLocation(), target) {
			t.Fatalf("%s: the sync stopped at the refused profile", refused.name)
		}
		if performanceProfile := device.GetPerformanceProfile(); !performanceProfileValuesEqual(performanceProfile, previous) {
			t.Fatalf("%s: the device holds %+v after the sync", refused.name, performanceProfile)
		}
		if saved, err := os.ReadFile(path); err != nil || !bytes.Equal(saved, savedBefore) {
			t.Fatalf("%s: the saved profile changed: %s", refused.name, saved)
		}
	}
}

// TestDeviceRemoteRefusesInvalidPerformanceProfile keeps a profile the device
// would refuse out of the remote's sync queue, and leaves the previous
// profile in force in the remote's getter.
func TestDeviceRemoteRefusesInvalidPerformanceProfile(t *testing.T) {
	previous := &PerformanceProfile{
		WindowType: WindowTypeQuality,
		WindowSize: &WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1},
	}
	device := &DeviceRemote{}
	device.lastKnownState.PerformanceProfile.Set(previous)
	for _, refused := range testingRefusedPerformanceProfiles() {
		device.SetPerformanceProfile(refused.profile)
		if device.state.PerformanceProfile.IsSet {
			t.Fatalf("%s: the remote queued the profile for its next sync", refused.name)
		}
		if performanceProfile := device.GetPerformanceProfile(); !performanceProfileValuesEqual(performanceProfile, previous) {
			t.Fatalf("%s: the remote reports %+v", refused.name, performanceProfile)
		}
	}
}

// TestDeviceLocalLoadsInvalidSavedPerformanceProfileAsAuto covers a profile an
// older build saved from any caller. Load adopted it, the next connect built
// its multi client from it, and a later re-apply of it with one option
// changed panicked. It now reads back in auto mode with its direct and
// post-quantum choices, through Load and through the LocalState getter the
// apps restore from, and neither rewrites the saved record. The LocalState
// setter refuses such a profile and keeps the saved one.
func TestDeviceLocalLoadsInvalidSavedPerformanceProfileAsAuto(t *testing.T) {
	_, fixture := testingPreferenceSpaceAt(t, t.TempDir())
	fixture.seedDistinctLogin(t)
	path := filepath.Join(fixture.localState.localStorageDir, ".performance_profile")
	record := []byte(`{"window_type":"quality","window_size":{"window_size_min":2,"window_size_max":1},"allow_direct":true,"post_quantum_encryption":true}`)
	if err := os.WriteFile(path, record, LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	fallback := &PerformanceProfile{
		WindowType:            WindowTypeAuto,
		AllowDirect:           true,
		PostQuantumEncryption: true,
	}
	if restored := fixture.localState.GetPerformanceProfile(); !performanceProfileValuesEqual(restored, fallback) {
		t.Fatalf("LocalState restored %+v", restored)
	}

	device := testingPreferenceDevice(t, fixture)
	result, err := device.Load()
	if err != nil || result == nil || !result.GetHasPreference("performance-profile") {
		t.Fatalf("load did not adopt the saved profile: %v", err)
	}
	if performanceProfile := device.GetPerformanceProfile(); !performanceProfileValuesEqual(performanceProfile, fallback) {
		t.Fatalf("load adopted %+v", performanceProfile)
	}
	device.SetConnectLocation(testingSpecificPreferenceLocation())
	if _, ok := testingPreferenceConsumer(device).(*connect.RemoteUserNatMultiClient); !ok {
		t.Fatal("the device built no multi client from the loaded profile")
	}
	changed := device.GetPerformanceProfile()
	changed.PostQuantumEncryption = false
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("re-applying the loaded profile panicked: %v", r)
			}
		}()
		device.SetPerformanceProfile(changed)
	}()
	if device.GetPerformanceProfile().PostQuantumEncryption {
		t.Fatal("a change to the loaded profile was not applied")
	}

	invalid := &PerformanceProfile{
		WindowType: WindowTypeSpeed,
		WindowSize: &WindowSizeSettings{WindowSizeMin: 0, WindowSizeMax: -1},
	}
	if err := fixture.localState.SetPerformanceProfile(invalid); err == nil || err.Error() != "invalid performance-profile" {
		t.Fatalf("LocalState saved a profile the device refuses: %v", err)
	}
	if saved, err := os.ReadFile(path); err != nil || !bytes.Equal(saved, record) {
		t.Fatalf("the saved record was rewritten: %s", saved)
	}
}
