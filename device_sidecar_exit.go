// What the device role's bridge (device_sidecar.go) asks of the rest of DeviceLocal beyond its seam (sidecar §17.6,
// §17.7, BRIDGE-M4-21): its hook on the exit NAT, its block and approved prefixes on the tunnel address's avoid list,
// and the devices that run no role at all.
//
//	exit NAT      remoteUserNatProviderLocalUserNat, which egresses other clients' flows, holds the held bridge's hook,
//	              the policy table of BridgePolicyProfileSdkProvider until C16, since an Opportunistic provider's
//	              sources may be plaintext (§17.6's sdk exit NAT). A NAT built while a bridge is held takes the hook at
//	              construction; a live NAT takes it through SetEgressHook when a bridge is first held, and gives it up
//	              when none is, so the hook follows the holds and nothing else. A publish changes what the hook decides,
//	              never the hook: installing a hook again would close every flow it decided.
//	fallback NAT  deviceLocalProvider.localUserNat carries this device's own route-local flows as
//	              ProvideMode_Network, which are no provider source, so it never holds a hook: a pending posture would
//	              deny them and cut the device off from its own local route (plan-BRIDGE-small-readings #21).
//	avoid list    the block in force, the block it replaced and the prefixes the bridge routes join the local networks
//	              RandomLocalIpv4 avoids, so a block chosen in 10/8 or a granted 10/8 route never holds the tunnel's
//	              own address. The tunnel address is chosen once, while the device is built, from the bridge the
//	              restore built: a later change applies to the next tunnel address and never moves a live one. A
//	              subnet route a local network wins is no route of the bridge's (Bridge.Routes, §17.7's sender
//	              conflicts), and the local network itself is avoided already.
//	no role       besides an off or hosted device (device_sidecar.go), a browser's: the extension worker and the
//	              page run no raw sockets, so js builds no bridge (D-BRIDGE-11). URnetwork-run proxies, WireGuard's
//	              included, are hosted devices, and the extension worker otherwise drives a DeviceRemote, which holds
//	              no bridge.

package sdk

import (
	"errors"
	"net/netip"
	"slices"

	"github.com/urnetwork/connect"
)

// Why a browser's device runs no device role.
var errDeviceBridgeBrowser = errors.New("a browser device runs no device role: js has no raw sockets (D-BRIDGE-11)")

// Why a device built on settings for goos runs no device role, nil when it runs one: off, hosted or a browser's, in
// that order.
func deviceBridgeRefusal(settings *DeviceLocalSettings, goos string) error {
	switch {
	case !settings.EnableBridge:
		return errDeviceBridgeOff
	case settings.HostedIncompatible:
		return errDeviceBridgeHosted
	case goos == "js":
		return errDeviceBridgeBrowser
	default:
		return nil
	}
}

// The hook the exit NAT holds while bridge is held: the bridge's own, the same value on every call; nil for none,
// which keeps today's behavior.
func deviceBridgeExitHook(bridge deviceBridge) connect.EgressHook {
	if bridge == nil {
		return nil
	}
	return bridge.EgressHook()
}

// holdBridge, then the held bridge's hook on the live exit NAT (the file's comment). The device role's owner calls it
// under its lock after every call that may move the posture, which orders the installs as it orders the holds.
func (self *DeviceLocal) holdBridgeOnExitNat(bridge deviceBridge) {
	self.holdBridge(bridge)
	self.installExitNatHook()
}

// Puts the held bridge's hook on the live exit NAT, or takes it off when none is held, unless that NAT holds it
// already: one built after the hold took it at construction, and a hold repeated after a publish holds the same
// bridge, while a second install would close every flow the hook decided. SetEgressHook scans every flow and may answer
// a source, so it runs outside stateLock. A NAT the device lets go of in between may take the hook as it ends, and its
// end lets go of it.
func (self *DeviceLocal) installExitNatHook() {
	localUserNat, hook := func() (*connect.LocalUserNat, connect.EgressHook) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.remoteUserNatProviderLocalUserNat == nil || self.remoteUserNatProviderEgressBridge == self.bridge {
			return nil, nil
		}
		self.remoteUserNatProviderEgressBridge = self.bridge
		return self.remoteUserNatProviderLocalUserNat, deviceBridgeExitHook(self.bridge)
	}()
	if localUserNat == nil {
		return
	}
	if self.log != nil && self.log.V(1).Enabled() {
		self.log.Infof("[device]exit nat hook installed=%t\n", hook != nil)
	}
	localUserNat.SetEgressHook(hook)
}

// What the tunnel address avoids beside the device's local networks (the file's comment): the block in force and the
// block it replaced, and the prefixes the bridge routes, from the bridge the restore built. Nil with no bridge: off,
// hosted, a browser's, legacy or closed.
func (self *deviceLocalBridgeOwner) tunnelAvoidPrefixes() []netip.Prefix {
	self.operationLock.Lock()
	defer self.operationLock.Unlock()
	if self.bridge == nil {
		return nil
	}
	status := self.bridge.Status()
	prefixes := []netip.Prefix{}
	for _, block := range []netip.Prefix{status.Block, status.PreviousBlock} {
		if block.IsValid() {
			prefixes = append(prefixes, block)
		}
	}
	return append(prefixes, self.bridge.Routes()...)
}

// The tunnel's address in 10/8 (UseExperimentalTunnelAddress): connect.RandomLocalIpv4 over this device's local
// networks and what its device role avoids, read once, now.
func deviceLocalTunnelAddress(settings *DeviceLocalSettings, bridgeOwner *deviceLocalBridgeOwner) netip.Addr {
	avoid := slices.Concat(connect.LocalIpv4Networks(), bridgeOwner.tunnelAvoidPrefixes())
	if settings.testingRandomLocalIpv4 != nil {
		return settings.testingRandomLocalIpv4(avoid)
	}
	return connect.RandomLocalIpv4(avoid)
}
