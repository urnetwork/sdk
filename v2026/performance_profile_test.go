package sdk

import (
	"testing"

	"github.com/urnetwork/connect/v2026"
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
		WindowSize: &WindowSizeSettings{WindowSizeMin: 2},
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
		},
	})

	device.SetPerformanceProfile(&PerformanceProfile{
		WindowType: WindowTypeAuto,
		WindowSize: &WindowSizeSettings{
			WindowSizeMin: 9,
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
		WindowSize: &WindowSizeSettings{WindowSizeMin: 2},
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
