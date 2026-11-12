package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

// Root cause: a phone's webrtc extender dials (connect EXTENDER.md S8) took the
// carrier's defaults, whose SCTP receive window follows the process memory
// target, 256-512 KiB, not the device's p2p budget. Observable: on a phone a
// space's dial settings bound the window to the device's p2p window,
// deviceLocalP2pReceiveBufferByteCount (128 KiB), and the largest message to
// the window, and keep the rest of the carrier's settings and the space's
// log; a desktop keeps the carrier's defaults; and the policy leaves nil
// alone.
func TestExtenderWebRtcDialTakesThePhonesP2pWindow(t *testing.T) {
	defaults := connect.DefaultWebRtcExtenderSettings()
	// the carrier's own window is past the phone's, whatever the process
	// memory target, so the bound below is one
	if defaults.ReceiveBufferSize <= deviceLocalP2pReceiveBufferByteCount {
		t.Fatalf("the carrier's window %d is already within the phone's %d", defaults.ReceiveBufferSize, deviceLocalP2pReceiveBufferByteCount)
	}
	log := connect.NewNoopLogger()

	phone := newSpaceExtenderWebRtcDialSettings(log, true)
	connect.AssertEqual(t, phone.ReceiveBufferSize, deviceLocalP2pReceiveBufferByteCount)
	connect.AssertEqual(t, phone.MaxMessageSize, min(defaults.MaxMessageSize, deviceLocalP2pReceiveBufferByteCount))
	if phone.ReceiveBufferSize < phone.MaxMessageSize {
		t.Fatalf("a message of %d does not fit the window of %d", phone.MaxMessageSize, phone.ReceiveBufferSize)
	}
	// the carrier's other settings stand: the browser dtls hello, no datagram
	// fast path, and the space's log
	connect.AssertEqual(t, phone.DtlsClientHelloMimicry, defaults.DtlsClientHelloMimicry)
	connect.AssertEqual(t, phone.EnableDatagramFastPath, defaults.EnableDatagramFastPath)
	connect.AssertEqual(t, phone.ExtenderCarrierOpenTimeout, defaults.ExtenderCarrierOpenTimeout)
	if phone.Log != log {
		t.Fatal("the dial settings lost the space's log")
	}

	desktop := newSpaceExtenderWebRtcDialSettings(log, false)
	connect.AssertEqual(t, desktop.ReceiveBufferSize, defaults.ReceiveBufferSize)
	connect.AssertEqual(t, desktop.MaxMessageSize, defaults.MaxMessageSize)

	// a window already within the phone's is kept
	small := connect.DefaultWebRtcExtenderSettings()
	small.ReceiveBufferSize = 64 * 1024
	small.MaxMessageSize = 16 * 1024
	applyMobileExtenderWebRtcDialSettingsForPlatform(small, true)
	connect.AssertEqual(t, small.ReceiveBufferSize, connect.ByteCount(64*1024))
	connect.AssertEqual(t, small.MaxMessageSize, connect.ByteCount(16*1024))

	applyMobileExtenderWebRtcDialSettingsForPlatform(nil, true)
}
