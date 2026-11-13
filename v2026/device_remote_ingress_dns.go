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
//   - The space calls it on each in-place change with a snapshot of the values
//     and their revision, taken with the change. It queues the newest for the
//     next sync on the caller's goroutine, without i/o, and its hand-off loop
//     sends the newest to the device over the rpc at once when the rpc is up.
//     Stale snapshots, which a slower caller can deliver after a newer one,
//     are dropped by revision.
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
//
// Locks: the space calls its callbacks with no lock held, and the remote takes
// only its own stateLock to queue; the hand-off loop holds no lock while it
// waits on the rpc.
package sdk

import (
	"slices"

	"github.com/urnetwork/connect/v2026"
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

// Called after a space's ingress dns changed in place, with the snapshot and
// revision taken with the change. It runs on the goroutine that made the
// change, with no lock held, and must not block.
type ingressDnsChangeCallback func(ingressDns *DeviceRemoteIngressDns, revision uint64)

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
// (applyControlDohValues). Returns the unsubscribe.
func (self *NetworkSpace) addIngressDnsChangeCallback(callback ingressDnsChangeCallback) func() {
	if self.ingressDnsChangeCallbacks == nil {
		return func() {}
	}
	callbackId := self.ingressDnsChangeCallbacks.Add(callback)
	return func() {
		self.ingressDnsChangeCallbacks.Remove(callbackId)
	}
}

// Runs the ingress dns callbacks for one change. Called with no lock held.
func (self *NetworkSpace) ingressDnsChanged(ingressDns *DeviceRemoteIngressDns, revision uint64) {
	if self.ingressDnsChangeCallbacks == nil {
		return
	}
	for _, callback := range self.ingressDnsChangeCallbacks.Get() {
		callback(ingressDns, revision)
	}
}

// The space's ingress dns and its revision, read together.
func (self *NetworkSpace) ingressDnsAndRevision() (*DeviceRemoteIngressDns, uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return newDeviceRemoteIngressDns(&self.values), self.ingressDnsRevision
}

// Applies an app's ingress dns to this copy of the space in place, persisting
// nothing (see the file comment). The values are read by the space's rules
// first. Returns whether anything changed.
func (self *NetworkSpace) applyIngressDns(ingressDns *DeviceRemoteIngressDns) bool {
	if ingressDns == nil || self.hostedIncompatibleGuarded("applyIngressDns") {
		return false
	}
	values := self.valuesCopy()
	values.IngressCountryCode = ingressDns.CountryCode
	values.ControlDohUrlsIpv4 = slices.Clone(ingressDns.ControlDohUrlsIpv4)
	values.ControlDohUrlsIpv6 = slices.Clone(ingressDns.ControlDohUrlsIpv6)
	readIngressDns := newDeviceRemoteIngressDns(&values)
	values.IngressCountryCode = readIngressDns.CountryCode
	values.ControlDohUrlsIpv4 = readIngressDns.ControlDohUrlsIpv4
	values.ControlDohUrlsIpv6 = readIngressDns.ControlDohUrlsIpv6
	return self.applyControlDohValues(&values)
}

// Queues the space's ingress dns for the device, registers for its changes
// and starts the hand-off loop. The constructor calls this before the run loop
// starts, so it sets the state without stateLock, as queueSavedSettings does.
func (self *DeviceRemote) queueIngressDns() {
	if self.settings == nil || self.settings.DisableHostedIncompatible ||
		self.networkSpace == nil || self.networkSpace.hostedIncompatible {
		return
	}
	ingressDns, revision := self.networkSpace.ingressDnsAndRevision()
	self.ingressDns = ingressDns
	self.ingressDnsRevision = revision
	self.state.IngressDns.Set(ingressDns)
	// the first sync carries the constructor's value, so the loop starts at
	// its revision and sends only what changes after
	self.ingressDnsRevisionMonitor = connect.NewMonitorValue(revision)
	self.ingressDnsUnsub = self.networkSpace.addIngressDnsChangeCallback(self.ingressDnsChangedInSpace)

	self.backgroundWorkers.Add(1)
	go connect.HandleError(func() {
		defer self.backgroundWorkers.Done()
		self.runIngressDnsHandOff(revision)
	})
}

// Queues the space's newer ingress dns for the next sync and wakes the
// hand-off loop. A snapshot older than the one queued is dropped.
func (self *DeviceRemote) ingressDnsChangedInSpace(ingressDns *DeviceRemoteIngressDns, revision uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed || revision <= self.ingressDnsRevision {
		return
	}
	self.ingressDns = ingressDns
	self.ingressDnsRevision = revision
	// queued whether or not the hand-off lands, and kept across syncs: a new
	// device, or this one after an rpc reconnect, takes it at its first sync
	self.state.IngressDns.Set(ingressDns)
	self.ingressDnsRevisionMonitor.Set(revision)
}

// Sends each newer queued ingress dns to the device at once while the rpc is
// up; with the rpc down the queued value waits for the next sync. Ends with
// the remote's context. `sentRevision` is the revision the first sync carries.
func (self *DeviceRemote) runIngressDnsHandOff(sentRevision uint64) {
	for {
		revision, update := self.ingressDnsRevisionMonitor.Get()
		if sentRevision < revision {
			ingressDns, queuedRevision, service := func() (*DeviceRemoteIngressDns, uint64, *rpcClient) {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				return self.ingressDns, self.ingressDnsRevision, self.service
			}()
			if service != nil {
				// an older device without the method keeps its session; it
				// takes the values at its next start
				rpcCallVoidAllowMissingMethod(
					service,
					"DeviceLocalRpc.SetIngressDns",
					ingressDns,
					func() { self.closeServiceInstance(service) },
				)
			}
			sentRevision = queuedRevision
		}
		select {
		case <-self.ctx.Done():
			return
		case <-update:
		}
	}
}

// Applies the app's ingress dns to this device's network space in place. A
// hosted device takes none: its space is the platform's.
func (self *DeviceLocal) applyIngressDns(ingressDns *DeviceRemoteIngressDns) {
	if ingressDns == nil || self.networkSpace == nil || self.hostedIncompatibleGuarded("applyIngressDns") {
		return
	}
	self.networkSpace.applyIngressDns(ingressDns)
}

// Applies the app's connection region and bootstrap DoH servers to the
// device's copy of the network space, in place and at once
// (DeviceRemote.runIngressDnsHandOff).
func (self *DeviceLocalRpc) SetIngressDns(ingressDns *DeviceRemoteIngressDns, _ RpcVoid) error {
	if self.hostedIncompatibleRpcGuarded("SetIngressDns") {
		return nil
	}
	self.deviceLocal.applyIngressDns(ingressDns)
	return nil
}
