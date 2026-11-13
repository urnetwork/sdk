// The limited window under the low-memory policy (server FP2-PROTOCOL.md
// §7.4, D8, D11.5): connect's fraction of the largest normal window until the
// mobile steady-memory gate fails, then at most one client, on mobile and
// hosted devices only.
package sdk

import (
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// The bound is installed only under the low-memory policy, and only when the
// gate failed; the fraction is always connect's.
func TestMobileLimitedWindowPolicyBoundsTheWindowWhenTheGateFails(t *testing.T) {
	defaultSettings := connect.DefaultMultiClientSettings()
	cases := []struct {
		name               string
		limitedSizeMax     int
		target             ByteCount
		mobile             bool
		expectedLimitedMax int
	}{
		{name: "gate failed, ios", limitedSizeMax: 1, target: 32 * 1024 * 1024, mobile: true, expectedLimitedMax: 1},
		{name: "gate failed, android", limitedSizeMax: 1, target: 64 * 1024 * 1024, mobile: true, expectedLimitedMax: 1},
		{name: "gate failed, calibrated target", limitedSizeMax: 1, target: mobileSteadyMemoryTargetByteCount, mobile: true, expectedLimitedMax: 1},
		{name: "gate not failed", limitedSizeMax: 0, target: 32 * 1024 * 1024, mobile: true, expectedLimitedMax: defaultSettings.LimitedWindowSizeMax},
		{name: "negative is no bound", limitedSizeMax: -1, target: 32 * 1024 * 1024, mobile: true, expectedLimitedMax: defaultSettings.LimitedWindowSizeMax},
		{name: "desktop", limitedSizeMax: 1, target: 32 * 1024 * 1024, mobile: false, expectedLimitedMax: defaultSettings.LimitedWindowSizeMax},
		{name: "no memory target", limitedSizeMax: 1, target: 0, mobile: true, expectedLimitedMax: defaultSettings.LimitedWindowSizeMax},
	}
	for _, c := range cases {
		settings := connect.DefaultMultiClientSettings()
		applyMobileLowMemoryMultiClientSettingsForPlatform(settings, c.target, c.mobile)
		applyMobileLimitedWindowSettingsForPlatform(settings, c.limitedSizeMax, c.target, c.mobile)
		if settings.LimitedWindowSizeMax != c.expectedLimitedMax {
			t.Errorf("%s: limited window max = %d, want %d", c.name, settings.LimitedWindowSizeMax, c.expectedLimitedMax)
		}
		if settings.LimitedWindowSizeFraction != defaultSettings.LimitedWindowSizeFraction {
			t.Errorf("%s: limited window fraction = %v, want connect's %v", c.name, settings.LimitedWindowSizeFraction, defaultSettings.LimitedWindowSizeFraction)
		}
	}
	// no settings to bound
	applyMobileLimitedWindowSettingsForPlatform(nil, 1, mobileSteadyMemoryTargetByteCount, true)
}

// Every platform's default leaves the window to the fraction: the gate has
// not failed. Connect's fraction is the owner's half (D8), and its default
// bounds nothing.
func TestDefaultDeviceLocalSettingsLeaveTheLimitedWindowToTheFraction(t *testing.T) {
	for _, goos := range []string{"ios", "android", "darwin", "windows", "linux"} {
		if limitedSizeMax := defaultDeviceLocalSettingsForPlatform(goos).lowMemoryLimitedWindowSizeMax; limitedSizeMax != 0 {
			t.Errorf("%s: limited window max = %d, want 0", goos, limitedSizeMax)
		}
	}
	if limitedSizeMax := DefaultLowMemoryDeviceLocalSettings().lowMemoryLimitedWindowSizeMax; limitedSizeMax != 0 {
		t.Errorf("low-memory profile: limited window max = %d, want 0", limitedSizeMax)
	}
	defaultSettings := connect.DefaultMultiClientSettings()
	if defaultSettings.LimitedWindowSizeFraction != 0.5 {
		t.Errorf("connect's limited window fraction = %v, want 0.5 (D8)", defaultSettings.LimitedWindowSizeFraction)
	}
	if defaultSettings.LimitedWindowSizeMax != 0 {
		t.Errorf("connect's limited window max = %d, want 0", defaultSettings.LimitedWindowSizeMax)
	}
}

// The sizes this gives each low-memory device, by connect's rule for the
// limited window's target, min(max(1, floor(fraction × S)), max) with S the
// largest normal target, here the policy's quality window. Connect's own tests
// pin the rule; this pins what the sdk's sizes make of it: 2 on iOS and
// hosted devices and 5 on Android until the gate fails, then 1 on each.
func TestMobileLimitedWindowSizesFollowTheQualityWindow(t *testing.T) {
	limitedTarget := func(settings *connect.MultiClientSettings) int {
		largestNormalTarget := 0
		for _, windowType := range []connect.WindowType{connect.WindowTypeQuality, connect.WindowTypeSpeed} {
			largestNormalTarget = max(largestNormalTarget, settings.WindowSizes[windowType].WindowSizeMax)
		}
		target := max(1, int(settings.LimitedWindowSizeFraction*float64(largestNormalTarget)))
		if 0 < settings.LimitedWindowSizeMax {
			target = min(target, settings.LimitedWindowSizeMax)
		}
		return target
	}
	cases := []struct {
		name           string
		target         ByteCount
		limitedSizeMax int
		expectedSize   int
	}{
		{name: "ios and hosted", target: LowMemoryDeviceTargetByteCount, limitedSizeMax: 0, expectedSize: 2},
		{name: "android", target: 64 * 1024 * 1024, limitedSizeMax: 0, expectedSize: 5},
		{name: "calibrated target", target: mobileSteadyMemoryTargetByteCount, limitedSizeMax: 0, expectedSize: 2},
		{name: "ios and hosted, gate failed", target: LowMemoryDeviceTargetByteCount, limitedSizeMax: 1, expectedSize: 1},
		{name: "android, gate failed", target: 64 * 1024 * 1024, limitedSizeMax: 1, expectedSize: 1},
	}
	for _, c := range cases {
		settings := connect.DefaultMultiClientSettings()
		applyMobileLowMemoryMultiClientSettingsForPlatform(settings, c.target, true)
		applyMobileLimitedWindowSettingsForPlatform(settings, c.limitedSizeMax, c.target, true)
		if size := limitedTarget(settings); size != c.expectedSize {
			t.Errorf("%s: limited window = %d, want %d", c.name, size, c.expectedSize)
		}
	}
}
