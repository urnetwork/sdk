// The connection region and bootstrap DoH servers of a running tunnel.
//
// A space resolves its own names (api, connect, extender) over the ingress
// dns: the DoH servers of its connection region (the ingress country) with
// its bootstrap DoH servers ahead of them (control_doh.go). Both apply in
// place, from the next lookup, in the space that saves them.
//
// The device of a tunnel does not always run in that space. The ios and macOS
// packet tunnel runs its device in its own copy of the space, imported when
// the tunnel starts, and the windows service and the linux daemon do the same
// for each session. So a region or servers the app saved while the tunnel ran
// reached the device only at its next start: a user in mainland China whose
// connection stalled, and who chose China from the "Can't reach URnetwork"
// prompt raised over the stall, stayed on the blocked default resolvers until
// a manual disconnect and connect (support inbox 8051). The windows app hands
// the values to its service itself (windows e1e53638, set_ingress_dns).
//
// A DeviceRemote now does it for every app, as it does the settings it saves
// for its device (device_remote_saved_settings.go):
//
//   - At construction it queues the space's ingress dns for the device, so the
//     first sync, also on an attach to a tunnel that is already up, gives the
//     device the app's values.
//   - It registers for the space's in-place changes, and on each one hands the
//     space's values to the device at once over the rpc, and queues them for
//     the next sync when the rpc is down.
//   - The values stay queued across syncs, as the reliability override does,
//     so an rpc reconnect or a new device gets them again. A device that has
//     them already changes nothing.
//
// The device applies them to its copy of the space in place and persists
// nothing. Its process takes the app's values at its next start anyway, and
// writing through that process's network space manager would store its whole
// copy of the values: on ios that is the app group document the app writes
// too, so every other value the app changed since the tunnel started would go
// back. The values are read by the space's own rules on both sides, so a value
// the app could not have saved changes nothing.
//
// A remote to a hosted device neither queues nor hands off, and a hosted device
// takes none: its space is the platform's. An older device has no
// SetIngressDns and ignores the queued field; it keeps the values it imported
// until its next start, as before.
package sdk

import (
	"slices"

	"github.com/urnetwork/connect"
)

// The ingress dns of a space for the device's own copy of it: the connection
// region (ingress country, empty for undisclosed) and the bootstrap DoH
// servers of each family, as the space reads its values (control_doh.go).
//
//gomobile:noexport
type DeviceRemoteIngressDns struct {
	CountryCode        string
	ControlDohUrlsIpv4 []string
	ControlDohUrlsIpv6 []string
}

// The ingress dns a space's values name, read by the space's rules: a region
// with a recommendation, else undisclosed, and the urls the url rule reads.
func newDeviceRemoteIngressDns(values *NetworkSpaceValues) *DeviceRemoteIngressDns {
	dohUrlsIpv4, dohUrlsIpv6 := spaceControlDohUrls(values)
	return &DeviceRemoteIngressDns{
		CountryCode:        spaceIngressCountryCode(values),
		ControlDohUrlsIpv4: dohUrlsIpv4,
		ControlDohUrlsIpv6: dohUrlsIpv6,
	}
}

// Registers `callback` for the space's in-place ingress dns changes
// (applyControlDohValues). It runs on the goroutine that made the change,
// outside the space's lock, and must not block. Returns the unsubscribe.
func (self *NetworkSpace) addIngressDnsChangeCallback(callback func()) func() {
	if self.ingressDnsChangeCallbacks == nil {
		return func() {}
	}
	callbackId := self.ingressDnsChangeCallbacks.Add(callback)
	return func() {
		self.ingressDnsChangeCallbacks.Remove(callbackId)
	}
}

func (self *NetworkSpace) ingressDnsChanged() {
	if self.ingressDnsChangeCallbacks == nil {
		return
	}
	for _, callback := range self.ingressDnsChangeCallbacks.Get() {
		callback()
	}
}

// The space's ingress dns.
func (self *NetworkSpace) ingressDns() *DeviceRemoteIngressDns {
	values := self.valuesCopy()
	return newDeviceRemoteIngressDns(&values)
}

// Applies an app's ingress dns to this copy of the space in place, persisting
// nothing (see the package comment). The values are read by the space's rules
// first. Returns whether anything changed.
func (self *NetworkSpace) applyIngressDns(ingressDns *DeviceRemoteIngressDns) bool {
	if ingressDns == nil || self.hostedIncompatibleGuarded("applyIngressDns") {
		return false
	}
	values := self.valuesCopy()
	values.IngressCountryCode = ingressDns.CountryCode
	values.ControlDohUrlsIpv4 = slices.Clone(ingressDns.ControlDohUrlsIpv4)
	values.ControlDohUrlsIpv6 = slices.Clone(ingressDns.ControlDohUrlsIpv6)
	read := newDeviceRemoteIngressDns(&values)
	values.IngressCountryCode = read.CountryCode
	values.ControlDohUrlsIpv4 = read.ControlDohUrlsIpv4
	values.ControlDohUrlsIpv6 = read.ControlDohUrlsIpv6
	return self.applyControlDohValues(&values)
}

// Queues the space's ingress dns for the device and registers for its
// changes. The constructor calls this before the run loop starts, so it sets
// the state without stateLock, as queueSavedSettings does.
func (self *DeviceRemote) queueIngressDns() {
	if self.settings == nil || self.settings.DisableHostedIncompatible ||
		self.networkSpace == nil || self.networkSpace.hostedIncompatible {
		return
	}
	self.state.IngressDns.Set(self.networkSpace.ingressDns())
	self.ingressDnsUnsub = self.networkSpace.addIngressDnsChangeCallback(self.ingressDnsChangedInSpace)
}

// The space's ingress dns changed in place. The space calls this on the
// goroutine that changed it, the app's settings screen, so the hand-off runs
// on a goroutine of its own: it takes the remote's lock and waits on the rpc,
// and a device that applies the values in a process sharing this space calls
// back here from inside its sync.
func (self *DeviceRemote) ingressDnsChangedInSpace() {
	go connect.HandleError(self.handOffIngressDns)
}

// Hands the space's ingress dns to the device, and queues it for the next
// sync. The values are read under the remote's lock, so of changes made close
// together the hand-off that runs last carries the newest.
func (self *DeviceRemote) handOffIngressDns() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if self.closed {
		return
	}
	ingressDns := self.networkSpace.ingressDns()
	// queued whether or not the call below lands, and kept across syncs: a
	// new device, or this one after an rpc reconnect, takes it at its first
	// sync
	self.state.IngressDns.Set(ingressDns)
	if self.service == nil {
		// the tunnel is not running, or the rpc is reconnecting
		return
	}
	// an older device without the method keeps its session; it takes the
	// values at its next start
	rpcCallVoidAllowMissingMethod(
		self.service,
		"DeviceLocalRpc.SetIngressDns",
		ingressDns,
		self.closeService,
	)
}

// Applies the app's ingress dns to this device's network space in place. A
// hosted device takes none: its space is the platform's.
func (self *DeviceLocal) applyIngressDns(ingressDns *DeviceRemoteIngressDns) {
	if ingressDns == nil || self.networkSpace == nil || self.hostedIncompatibleGuarded("applyIngressDns") {
		return
	}
	self.networkSpace.applyIngressDns(ingressDns)
}

// SetIngressDns applies the app's connection region and bootstrap DoH servers
// to the device's copy of the network space, in place and at once
// (DeviceRemote.handOffIngressDns).
func (self *DeviceLocalRpc) SetIngressDns(ingressDns *DeviceRemoteIngressDns, _ RpcVoid) error {
	if self.hostedIncompatibleRpcGuarded("SetIngressDns") {
		return nil
	}
	self.deviceLocal.applyIngressDns(ingressDns)
	return nil
}
