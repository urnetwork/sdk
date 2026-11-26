// The device role's bridge settings by platform, and §17.9's egress spec on app devices (BRIDGE-M8-02; R-BRIDGE-4,
// R-BRIDGE-5, R-BRIDGE-7, D-BRIDGE-5, D-BRIDGE-13, §17.5's memory row, §18.17; owner decisions 100 and 197). Untagged,
// as device_sidecar.go is.
//
//	pins, links   a phone's (device.MobileBridgeSettings) on iOS, on Android and on any low-memory device: 1,024 pin
//	              slots and 8 links before an idle one closes for room; §17's desktop values elsewhere
//	LinkCountMax  per platform: 0 in the iOS tunnel until §22.9 measures a link there (owner decision 100), so every
//	              link entry and every exit answers with a reject and no window client is built; a phone's 8; a
//	              desktop's 64. It is no cap: a busy link is never closed for room (R-BRIDGE-11)
//	clock         on iOS and Android the waits are uncapped, the VPN component calling DeviceLocal.CheckClock at each
//	              wake and clock change; elsewhere, a low-memory device included, a desktop's ClockCheckInterval caps them
//	              (owner decision 789, sidecar_clock_check.go)
//	egress spec   off unless EnableBridgeEgress: the VPN component runs the device's IP and port forwarding alone (owner
//	              decision 197), and §17.9's egress spec on app devices follows M8 (§18.17), so until an app turns it on
//	              a packet no entry holds takes the device's own route whatever the state's spec says
//	              (BridgeSettings.EgressSpecOff). On, a subject's default route goes to an approved exit or nowhere:
//	              under exits over the forwarding role's link to the exit, a fixed window of one Required window client
//	              with PostQuantumEncryption in ProvideMode_Network, F's child under the forwarding key's
//	              WindowDelegation (SDK-M8-08), which is §17.9's NetworkPeer with PostQuantumEncryption on an app; under
//	              none a reject into the tun. With LinkCountMax 0 exits reject too
//
// A sidecar's own Public contracts stay §8.10's gate's to refuse under exits or none (D-BRIDGE-13), switch or none.

package sdk

import (
	sidecardevice "github.com/urnetwork/sidecar/device"
)

// The device role's bridge settings for a device of goos built on settings (the file's comment), with the sdk exit
// NAT's table until C16, which the device client's network's forwarding role replaces where the device hosts
// (deviceBridgeHostingSettings).
func deviceLocalBridgeSettingsFor(settings *DeviceLocalSettings, goos string) *sidecardevice.BridgeSettings {
	var bridgeSettings *sidecardevice.BridgeSettings
	phone := goos == "ios" || goos == "android"
	if settings.LowMemoryProfile || phone {
		bridgeSettings = sidecardevice.MobileBridgeSettings()
		if !phone {
			// a low-memory desktop takes a phone's sizes, not its wake: no platform call checks its clock, so its
			// waits keep a desktop's cap (sidecar_clock_check.go; owner decision 789)
			bridgeSettings.ClockCheckInterval = sidecardevice.DefaultBridgeSettings().ClockCheckInterval
		}
	} else {
		bridgeSettings = sidecardevice.DefaultBridgeSettings()
	}
	bridgeSettings.PolicyProfile = sidecardevice.BridgePolicyProfileSdkProvider
	if goos == "ios" {
		bridgeSettings.LinkCountMax = 0
	}
	bridgeSettings.EgressSpecOff = !settings.EnableBridgeEgress
	return bridgeSettings
}
