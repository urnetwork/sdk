// Unit coverage for the echo bookkeeping of device_remote_connect_state.go.
package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

// A device coalesces its pushes, last value wins, so an echo may stand for
// several writes. A report equal to no expected echo is a change of its own.
func TestConsumeDeviceRemoteConnectEcho(t *testing.T) {
	equal := func(a bool, b bool) bool { return a == b }

	echoes, echo := consumeDeviceRemoteConnectEcho([]bool{true, false, true}, true, equal)
	connect.AssertEqual(t, echo, true)
	connect.AssertEqual(t, echoes, []bool{false, true})

	// the coalesced push of the last write consumes the earlier writes
	echoes, echo = consumeDeviceRemoteConnectEcho([]bool{true, false}, false, equal)
	connect.AssertEqual(t, echo, true)
	connect.AssertEqual(t, len(echoes), 0)

	echoes, echo = consumeDeviceRemoteConnectEcho([]bool{true}, false, equal)
	connect.AssertEqual(t, echo, false)
	connect.AssertEqual(t, len(echoes), 0)

	echoes, echo = consumeDeviceRemoteConnectEcho([]bool(nil), true, equal)
	connect.AssertEqual(t, echo, false)
	connect.AssertEqual(t, len(echoes), 0)

	// a device that never reports some writes cannot grow the echoes
	var bounded []bool
	for i := 0; i < 3*deviceRemoteConnectEchoLimit; i += 1 {
		bounded = appendDeviceRemoteConnectEcho(bounded, i%2 == 0)
	}
	connect.AssertEqual(t, len(bounded), deviceRemoteConnectEchoLimit)
}
