package sdk

// device_local_move.go — the device's part in the platform's connection moves
// (connect/resident_move.go, server/connect/MOVE.md §5.2, D11).
//
// The platform may advise a client to move its platform transport to another
// edge. A mobile device declines such an advisory move while its app is in
// the background or its active network is metered: the move costs a second
// connection and its data, which the platform's balance never needs urgently.
// A compulsory move still applies. The apps report both states from their OS
// signals; until they do, the device declines nothing for this reason. The
// policy is the device's `MoveDeclineBackgroundOrMetered`, and it reaches every
// platform transport the device builds -- the provider's and each window's --
// through `PlatformTransportSettings.MoveDecline`.
//
// Concurrency: the state is atomics, read on the transports' receive path
// without blocking.

import (
	"sync/atomic"
)

// The app and network state a device reports, which decides whether it
// declines the platform's advisory moves.
type deviceLocalMoveDecline struct {
	// MoveDeclineBackgroundOrMetered, fixed at construction
	enabled         bool
	appBackgrounded atomic.Bool
	networkMetered  atomic.Bool
}

func newDeviceLocalMoveDecline(enabled bool) *deviceLocalMoveDecline {
	return &deviceLocalMoveDecline{
		enabled: enabled,
	}
}

// Whether an advisory move is declined now. Never blocks.
func (self *deviceLocalMoveDecline) declines() bool {
	if self == nil || !self.enabled {
		return false
	}
	return self.appBackgrounded.Load() || self.networkMetered.Load()
}

// The decline hook for platform transport settings, nil when the policy is
// off so the transports skip the call.
func (self *deviceLocalMoveDecline) hook() func() bool {
	if self == nil || !self.enabled {
		return nil
	}
	return self.declines
}

// The device's decline hook for the platform transports it builds.
func (self *DeviceLocal) moveDeclineHook() func() bool {
	return self.moveDecline.hook()
}

// Reports whether the app is in the background. While it is, and the
// device's `MoveDeclineBackgroundOrMetered` is set, the device declines the
// platform's advisory connection moves; a compulsory move still applies.
// Apps call it from the OS lifecycle signal (android ProcessLifecycleOwner,
// the ios scene phase); cheap and safe to call on every change.
func (self *DeviceLocal) SetAppBackgrounded(backgrounded bool) {
	if self.moveDecline != nil {
		self.moveDecline.appBackgrounded.Store(backgrounded)
	}
}

// Reports whether the active network is metered (android's NOT_METERED
// capability absent, an expensive or constrained ios path). While it is, and
// the device's `MoveDeclineBackgroundOrMetered` is set, the device declines
// the platform's advisory connection moves; a compulsory move still applies.
// Apps call it on every default network change; cheap and safe to call on
// every change.
func (self *DeviceLocal) SetNetworkMetered(metered bool) {
	if self.moveDecline != nil {
		self.moveDecline.networkMetered.Store(metered)
	}
}
