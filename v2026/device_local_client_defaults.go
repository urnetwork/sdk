// Low-profile defaults follow one device target, independently of other
// devices and the host process. Desktop and targetless callers keep legacy
// process-default sizing.
package sdk

import "github.com/urnetwork/connect/v2026"

// Used both before device ownership is attached and for every later window
// generation. The explicit Connect factory applies the complete same policy
// as its process-default counterpart without changing global state.
func newDeviceLocalDefaultClientSettings(bufferSize int, memoryTargetByteCount ByteCount, lowMemoryProfile bool) *connect.ClientSettings {
	if lowMemoryProfile && 0 < memoryTargetByteCount {
		return connect.DefaultClientSettingsWithBufferSizeAndMemoryTarget(bufferSize, memoryTargetByteCount)
	}
	return connect.DefaultClientSettingsWithBufferSize(bufferSize)
}
