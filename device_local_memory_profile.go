package sdk

import "runtime"

// The iOS and hosted-proxy low profile share one per-device target. Runtime
// memory limits and allocator pools remain separate process-owned resources.
const LowMemoryDeviceTargetByteCount int64 = 32 * 1024 * 1024

// Select the low-memory ownership and queue policy on any host without
// changing its Go runtime, allocator pools, or mobile-only sampling.
func DefaultLowMemoryDeviceLocalSettings() *DeviceLocalSettings {
	return defaultDeviceLocalSettingsWithProfile(LowMemoryDeviceTargetByteCount, true)
}

// Platform defaults select policy, not a process-wide memory limit.
func DefaultDeviceLocalSettings() *DeviceLocalSettings {
	return defaultDeviceLocalSettingsForPlatform(runtime.GOOS)
}

// Android retains its existing default; the Android app's explicit 64-MiB
// target is rederived at device construction using the same scaled policy.
func defaultDeviceLocalSettingsForPlatform(goos string) *DeviceLocalSettings {
	if goos == "ios" {
		return DefaultLowMemoryDeviceLocalSettings()
	}
	mobile := goos == "android"
	return defaultDeviceLocalSettingsWithProfile(
		defaultDeviceLocalMemoryTargetByteCountForPlatform(mobile), mobile,
	)
}

// The explicit host-facing constructor chooses its final low-profile target
// before any process-derived default is captured. Other profiles preserve
// their historical provisional defaults and targetless compatibility.
func defaultDeviceLocalSettingsWithMemoryTargetForPlatform(memoryTargetByteCount ByteCount, goos string) *DeviceLocalSettings {
	settings := defaultDeviceLocalSettingsForPlatform(goos)
	if settings.LowMemoryProfile && 0 < memoryTargetByteCount {
		return defaultDeviceLocalSettingsWithProfile(memoryTargetByteCount, true)
	}
	settings.MemoryTargetByteCount = memoryTargetByteCount
	return settings
}

// Keep platform selection at the public constructor boundary.
func defaultDeviceLocalSettingsWithMemoryTarget(memoryTargetByteCount ByteCount) *DeviceLocalSettings {
	return defaultDeviceLocalSettingsWithMemoryTargetForPlatform(memoryTargetByteCount, runtime.GOOS)
}
