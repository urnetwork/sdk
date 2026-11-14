package sdk

// The exit timers of an app's windows (owner, 2026-11-14: "go ahead and fix
// all 5"; connect ip_remote_multi_client_exit_stall.go).
//
// An exit that takes packets and acknowledges none of them holds every flow
// on it until its window client's send-ack lifetime closes the sequence, and
// a hole in an exit's return stream freezes every flow on it until the
// receive sequence's GapTimeout. Connect's window defaults, 30 s and 60 s,
// are what the owner's iOS logs of 2026-11-13 measured: 15 exits held their
// flows for the whole 30 s (70% of the flows reset), and four exits held a
// hole for the whole 60 s, one of them carrying 65 flows.
//
// Every app platform shortens both: iOS, Android, and the macOS, Windows and
// Linux desktops build their DeviceLocal windows here, and a stalled exit is
// the same stall on each. A hosted device (DeviceLocalSettings
// .HostedIncompatible, the proxy service) keeps connect's values, and so does
// the device's own provider client, whose settings these never touch.
//
// Neither timer cuts an exit that is merely slow. The send-ack lifetime
// renews on every advance of the cumulative head (connect
// SendSequence.ackProgressDeadline), so it ends only an exit whose head has
// stopped; and a hole in a working, lossy stream fills within a few resend
// rounds, far inside the gap timeout. A gap timeout evicts its exit only
// while the tunnel receives return traffic (connect receiveGapTimedOut), so a
// phone between networks keeps its exits.

import (
	"time"

	"github.com/urnetwork/connect/v2026"
)

const (
	// The send-ack lifetime of an app window client: about four send-stall
	// bars (3 s), and outside every transport loss in the owner's logs of
	// 2026-11-13 (64 of 65 were under 5 s, the other under 10 s).
	appWindowAckTimeout = 12 * time.Second
	// The receive gap timeout of an app window client: a hole in a working,
	// lossy stream fills within a few resend rounds, so one still open at
	// 15 s is an exit that has stopped resending it. Above the send-ack
	// lifetime, so an exit whose acks stopped too leaves by the lifetime.
	appWindowGapTimeout = 15 * time.Second
)

// Gives the windows of an app device the app's send-ack lifetime. A hosted
// device keeps connect's.
func applyAppWindowAckTimeout(settings *connect.MultiClientSettings, deviceLocalSettings *DeviceLocalSettings) {
	if settings == nil || deviceLocalSettings == nil || deviceLocalSettings.HostedIncompatible {
		return
	}
	settings.AckTimeout = appWindowAckTimeout
}

// Gives one window client of an app device the app's receive gap timeout. The
// settings are the window client's own, built fresh for it; a hosted device
// keeps connect's.
func applyAppWindowGapTimeout(clientSettings *connect.ClientSettings, deviceLocalSettings *DeviceLocalSettings) {
	if clientSettings == nil || clientSettings.ReceiveBufferSettings == nil ||
		deviceLocalSettings == nil || deviceLocalSettings.HostedIncompatible {
		return
	}
	clientSettings.ReceiveBufferSettings.GapTimeout = appWindowGapTimeout
}
