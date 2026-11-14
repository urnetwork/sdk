// The limited window under the low-memory policy (server FP2-PROTOCOL.md
// §7.4, D8, D11.5): connect's fraction of the largest normal window, the
// normal window's size, except at or below the 32-MiB target of iOS and the
// hosted proxies, which hold half until the owner decides; and at most one
// client once the mobile steady-memory gate fails, on mobile and hosted
// devices only.
package sdk

import (
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// The bound is installed only under the low-memory policy, and only when the
// gate failed. The fraction is connect's, the normal window's size, except at
// or below the 32-MiB target, which takes the device's half.
func TestMobileLimitedWindowPolicyBoundsTheWindowWhenTheGateFails(t *testing.T) {
	defaultSettings := connect.DefaultMultiClientSettings()
	half := DefaultLowMemoryDeviceLocalSettings().lowMemoryLimitedWindowSizeFraction
	cases := []struct {
		name               string
		limitedSizeMax     int
		target             ByteCount
		mobile             bool
		expectedLimitedMax int
		expectedFraction   float64
	}{
		{name: "gate failed, ios", limitedSizeMax: 1, target: 32 * 1024 * 1024, mobile: true, expectedLimitedMax: 1, expectedFraction: half},
		{name: "gate failed, android", limitedSizeMax: 1, target: 64 * 1024 * 1024, mobile: true, expectedLimitedMax: 1, expectedFraction: defaultSettings.LimitedWindowSizeFraction},
		{name: "gate failed, calibrated target", limitedSizeMax: 1, target: mobileSteadyMemoryTargetByteCount, mobile: true, expectedLimitedMax: 1, expectedFraction: half},
		{name: "gate not failed", limitedSizeMax: 0, target: 32 * 1024 * 1024, mobile: true, expectedLimitedMax: defaultSettings.LimitedWindowSizeMax, expectedFraction: half},
		{name: "negative is no bound", limitedSizeMax: -1, target: 32 * 1024 * 1024, mobile: true, expectedLimitedMax: defaultSettings.LimitedWindowSizeMax, expectedFraction: half},
		{name: "android, gate not failed", limitedSizeMax: 0, target: 64 * 1024 * 1024, mobile: true, expectedLimitedMax: defaultSettings.LimitedWindowSizeMax, expectedFraction: defaultSettings.LimitedWindowSizeFraction},
		{name: "desktop", limitedSizeMax: 1, target: 32 * 1024 * 1024, mobile: false, expectedLimitedMax: defaultSettings.LimitedWindowSizeMax, expectedFraction: defaultSettings.LimitedWindowSizeFraction},
		{name: "no memory target", limitedSizeMax: 1, target: 0, mobile: true, expectedLimitedMax: defaultSettings.LimitedWindowSizeMax, expectedFraction: defaultSettings.LimitedWindowSizeFraction},
	}
	for _, c := range cases {
		settings := connect.DefaultMultiClientSettings()
		applyMobileLowMemoryMultiClientSettingsForPlatform(settings, c.target, c.mobile)
		applyMobileLimitedWindowSettingsForPlatform(settings, c.limitedSizeMax, half, c.target, c.mobile)
		if settings.LimitedWindowSizeMax != c.expectedLimitedMax {
			t.Errorf("%s: limited window max = %d, want %d", c.name, settings.LimitedWindowSizeMax, c.expectedLimitedMax)
		}
		if settings.LimitedWindowSizeFraction != c.expectedFraction {
			t.Errorf("%s: limited window fraction = %v, want %v", c.name, settings.LimitedWindowSizeFraction, c.expectedFraction)
		}
	}
	// a non-positive device fraction leaves connect's at 32 MiB too
	settings := connect.DefaultMultiClientSettings()
	applyMobileLimitedWindowSettingsForPlatform(settings, 0, 0, LowMemoryDeviceTargetByteCount, true)
	if settings.LimitedWindowSizeFraction != defaultSettings.LimitedWindowSizeFraction {
		t.Errorf("no device fraction: limited window fraction = %v, want connect's %v", settings.LimitedWindowSizeFraction, defaultSettings.LimitedWindowSizeFraction)
	}
	// no settings to bound
	applyMobileLimitedWindowSettingsForPlatform(nil, 1, half, mobileSteadyMemoryTargetByteCount, true)
}

// Every platform's default leaves the window to the fraction: the gate has
// not failed. Connect's fraction is 1, the owner's "same size as the normal
// window" (D8, 2026-11-13), and its default bounds nothing. Every device
// carries the 32-MiB tier's half, which only that tier reads.
func TestDefaultDeviceLocalSettingsLeaveTheLimitedWindowToTheFraction(t *testing.T) {
	for _, goos := range []string{"ios", "android", "darwin", "windows", "linux"} {
		settings := defaultDeviceLocalSettingsForPlatform(goos)
		if settings.lowMemoryLimitedWindowSizeMax != 0 {
			t.Errorf("%s: limited window max = %d, want 0", goos, settings.lowMemoryLimitedWindowSizeMax)
		}
		if settings.lowMemoryLimitedWindowSizeFraction != 0.5 {
			t.Errorf("%s: low-memory limited window fraction = %v, want 0.5", goos, settings.lowMemoryLimitedWindowSizeFraction)
		}
	}
	lowMemory := DefaultLowMemoryDeviceLocalSettings()
	if lowMemory.lowMemoryLimitedWindowSizeMax != 0 || lowMemory.lowMemoryLimitedWindowSizeFraction != 0.5 {
		t.Errorf("low-memory profile: limited window max = %d fraction = %v, want 0 and 0.5", lowMemory.lowMemoryLimitedWindowSizeMax, lowMemory.lowMemoryLimitedWindowSizeFraction)
	}
	defaultSettings := connect.DefaultMultiClientSettings()
	if defaultSettings.LimitedWindowSizeFraction != 1 {
		t.Errorf("connect's limited window fraction = %v, want 1 (D8)", defaultSettings.LimitedWindowSizeFraction)
	}
	if defaultSettings.LimitedWindowSizeMax != 0 {
		t.Errorf("connect's limited window max = %d, want 0", defaultSettings.LimitedWindowSizeMax)
	}
}

// The sizes this gives each low-memory device, by connect's rule for the
// limited window's target, min(max(1, floor(fraction × S)), max) with S the
// largest normal target, here the policy's quality window. Connect's own tests
// pin the rule; this pins what the sdk's sizes make of it until the gate
// fails, then 1 on each: Android's quality window's size, 10, and on iOS and
// hosted devices, which hold half, 2 of their 5. Before the hold the 32-MiB
// tier took connect's full size, 5.
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
		{name: "android", target: 64 * 1024 * 1024, limitedSizeMax: 0, expectedSize: 10},
		{name: "calibrated target", target: mobileSteadyMemoryTargetByteCount, limitedSizeMax: 0, expectedSize: 2},
		{name: "ios and hosted, gate failed", target: LowMemoryDeviceTargetByteCount, limitedSizeMax: 1, expectedSize: 1},
		{name: "android, gate failed", target: 64 * 1024 * 1024, limitedSizeMax: 1, expectedSize: 1},
	}
	for _, c := range cases {
		settings := connect.DefaultMultiClientSettings()
		applyMobileLowMemoryMultiClientSettingsForPlatform(settings, c.target, true)
		applyMobileLimitedWindowSettingsForPlatform(settings, c.limitedSizeMax, DefaultLowMemoryDeviceLocalSettings().lowMemoryLimitedWindowSizeFraction, c.target, true)
		if size := limitedTarget(settings); size != c.expectedSize {
			t.Errorf("%s: limited window = %d, want %d", c.name, size, c.expectedSize)
		}
	}
}
