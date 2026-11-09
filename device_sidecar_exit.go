// What the device role's bridge (device_sidecar.go) asks of the rest of DeviceLocal beyond its seam (sidecar §17.6,
// BRIDGE-M4-21): its hook on the exit NAT, and on that NAT alone.
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

package sdk

import (
	"github.com/urnetwork/connect"
)

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
