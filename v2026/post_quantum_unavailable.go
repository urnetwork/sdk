package sdk

// post_quantum_unavailable.go -- the notice that the selected location has
// no post-quantum exits right now (owner, 2026-11-24: "Tell them, let them
// choose"). With post-quantum encryption on, discovery draws only providers
// that negotiate it, so a location whose providers none do never connects;
// instead of a connection that stays connecting, the apps say "No
// post-quantum exits in <location> right now" and offer two actions: pick
// another location, or turn post-quantum encryption off, saved as the
// option's switch saves it. Nothing changes without the user: the device
// keeps the selected location and the setting, and keeps trying.
//
// The device decides the notice (device_local_post_quantum.go), so every
// surface reads the same one: the connect screen through
// ConnectViewController, and the surfaces that run while it is closed (a VPN
// notification, a quick settings tile, widgets, a tray) through
// Device.GetPostQuantumUnavailable. A DeviceRemote mirrors the device
// process's notice over the device rpc.

import (
	"time"
)

// Immutable.
type PostQuantumUnavailable struct {
	// the selected location with no post-quantum exits; name it with its
	// Name, as the location list shows it
	Location *ConnectLocation `json:"location"`
}

// fires on every change of `Device.GetPostQuantumUnavailable`, with the new
// notice, nil when there is none. A DeviceRemote also fires it with the
// device process's notice each time it syncs with that process. Called on an
// sdk goroutine.
type PostQuantumUnavailableChangeListener interface {
	PostQuantumUnavailableChanged(postQuantumUnavailable *PostQuantumUnavailable)
}

// The tunables of the device's post-quantum supply check
// (device_local_post_quantum.go).
type postQuantumSupplySettings struct {
	// The longest one check's two find-providers2 requests may take together.
	// A check that does not answer in time failed: no notice, and a recheck
	// after RecheckInterval. Non-positive bounds it by the device's lifetime
	// only.
	CheckTimeout time.Duration
	// How long after a check that found no post-quantum exits, or that failed,
	// the location is checked again, so the notice clears when supply returns
	// without the user acting. Non-positive checks once.
	RecheckInterval time.Duration
	// The least time between the starts of two checks of one location that
	// the connection alone asks for, as it leaves connected, so a window that
	// drops in and out of connected costs at most one check per interval. A
	// new selection, and post-quantum encryption turned on, check at once.
	// Non-positive does not space them.
	MinCheckInterval time.Duration
	// The providers each request asks for. One answers the question; the
	// server reads its usual minimum block either way.
	ProviderCount int
}

func defaultPostQuantumSupplySettings() *postQuantumSupplySettings {
	return &postQuantumSupplySettings{
		CheckTimeout: 15 * time.Second,
		// about the time a window gives up on a location with no provider
		// (connect's window outcome watchdog, twice 45s)
		RecheckInterval: 90 * time.Second,
		// a third of the recheck: a window recovering in a loop stays far
		// below one request a second, and a real drop is checked promptly
		MinCheckInterval: 30 * time.Second,
		ProviderCount:    1,
	}
}

// Whether the selection is a target whose supply a find-providers2 request
// can check: a location or a group. Best available always has post-quantum
// exits, and a device of the caller's own network is answered without the
// requirement.
func postQuantumSupplyCheckable(location *ConnectLocation) bool {
	if location == nil || location.ConnectLocationId == nil {
		return false
	}
	connectLocationId := location.ConnectLocationId
	if connectLocationId.BestAvailable || connectLocationId.ClientId != nil {
		return false
	}
	return connectLocationId.LocationId != nil || connectLocationId.LocationGroupId != nil
}

// Whether two notices are the same: both none, or both for the same
// location.
func postQuantumUnavailableEqual(a *PostQuantumUnavailable, b *PostQuantumUnavailable) bool {
	if a == nil || b == nil {
		return a == b
	}
	return connectLocationTransportEqual(a.Location, b.Location)
}

// Whether a connection is connected: its window reached its minimum, or its
// active providers cover the location's provider count, since then there is
// no provider left to connect to (see gridConnectionStatus). A location
// without a provider count (best available, a group, a client id) relies on
// the window minimum alone. The one rule for the connect screen's status and
// the device's post-quantum notice.
func connectionSatisfied(minSatisfied bool, activeProviderCount int, locationProviderCount int32) bool {
	if minSatisfied {
		return true
	}
	return 0 < locationProviderCount && 0 < activeProviderCount &&
		int(locationProviderCount) <= activeProviderCount
}
