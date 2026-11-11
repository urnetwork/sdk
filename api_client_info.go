package sdk

import (
	"context"
	"github.com/urnetwork/connect"
)

// Embedding apps supply their own version, device type and distribution before
// making requests. The api sends them as X-UR-ClientInfo with its requests and
// in the connect auth.
type ClientInfo struct {
	Version    int    `json:"v"`
	DeviceType string `json:"device_type"`
	AppVersion string `json:"app_version"`
	SdkVersion string `json:"sdk_version,omitempty"`
	// how the app's build reached the device (ClientDistribution*); "" is
	// unknown, and an unknown one is not sent
	Distribution string `json:"distribution,omitempty"`
}

// The distributions an app's build reports, the values the server reads
// (connect.ClientDistribution*). Each build passes its own to
// NewClientInfoWithDistribution, from what it was built as: its Android flavor
// (a BuildConfig field of the flavor), Apple's DIRECT_DOWNLOAD flag, its Linux
// package (-Ddistribution), its browser target. The sdk keeps a value only
// with the device type it pairs with, and an app that names none, or one this
// sdk does not know, reports an unknown distribution, as every app before the
// field did.
//
// The server tells the App Store Mac from the direct-download Mac by it
// alone: the two builds send the same device type, spec and version.
const (
	// the App Store builds for iOS, iPadOS and macOS, their beta builds
	// included (device types ios and macos)
	ClientDistributionAppStore = connect.ClientDistributionAppStore
	// the direct-download macOS build: the Developer ID DMG, DIRECT_DOWNLOAD
	ClientDistributionMacDirect = connect.ClientDistributionMacDirect
	// the Android play flavor
	ClientDistributionPlay = connect.ClientDistributionPlay
	// the Android github flavor: the GitHub APK and the F-Droid build
	ClientDistributionAndroidDirect = connect.ClientDistributionAndroidDirect
	// the Android solana_dapp flavor
	ClientDistributionSolanaDapp = connect.ClientDistributionSolanaDapp
	// the Android ethos_dapp flavor
	ClientDistributionEthosDapp = connect.ClientDistributionEthosDapp
	// the Windows app (the MSI and the portable zip carry one build)
	ClientDistributionWindows = connect.ClientDistributionWindows
	// a Linux build whose package is not known (the daemon, a source build)
	ClientDistributionLinux = connect.ClientDistributionLinux
	// the Linux app's AppImage
	ClientDistributionLinuxAppimage = connect.ClientDistributionLinuxAppimage
	// the Linux app's Flatpak
	ClientDistributionLinuxFlatpak = connect.ClientDistributionLinuxFlatpak
	// ur.io in a browser (device type web)
	ClientDistributionWeb = connect.ClientDistributionWeb
	// the browser extension's Chrome build (device type web)
	ClientDistributionExtensionChrome = connect.ClientDistributionExtensionChrome
	// the browser extension's Firefox build (device type web)
	ClientDistributionExtensionFirefox = connect.ClientDistributionExtensionFirefox
)

// The client info of an app that does not report its distribution.
func NewClientInfo(deviceType, appVersion string) *ClientInfo {
	return NewClientInfoWithDistribution(deviceType, appVersion, "")
}

// The client info of an app's build: its device type, version and
// distribution (ClientDistribution*). SetClientInfo drops a distribution that
// does not pair with the device type.
func NewClientInfoWithDistribution(deviceType, appVersion, distribution string) *ClientInfo {
	return &ClientInfo{Version: 1, DeviceType: deviceType, AppVersion: appVersion, SdkVersion: Version, Distribution: distribution}
}

// Normalized through the server's own parser (connect.ParseClientInfo), so
// what the api stores and sends is what the server reads: invalid metadata is
// unknown, and an unknown or mismatched distribution is "".
func (self *Api) SetClientInfo(info *ClientInfo) {
	value := connect.UnknownClientInfo()
	if info != nil {
		value = connect.ClientInfo{Version: 1, DeviceType: info.DeviceType, AppVersion: info.AppVersion, SdkVersion: info.SdkVersion, Distribution: info.Distribution}
		if value.SdkVersion == "" {
			value.SdkVersion = Version
		}
		value = connect.ParseClientInfo(value.Json(), "")
	}
	self.mutex.Lock()
	self.clientInfo = value
	self.mutex.Unlock()
}
func (self *Api) GetClientInfo() *ClientInfo {
	self.mutex.Lock()
	value := self.clientInfo
	self.mutex.Unlock()
	if value.Version == 0 {
		value = connect.UnknownClientInfo()
	}
	return &ClientInfo{Version: value.Version, DeviceType: value.DeviceType, AppVersion: value.AppVersion, SdkVersion: value.SdkVersion, Distribution: value.Distribution}
}
func (self *Api) clientInfoContext(ctx context.Context) context.Context {
	return connect.WithClientInfo(ctx, self.connectClientInfo())
}
func (self *Api) connectClientInfo() connect.ClientInfo {
	info := self.GetClientInfo()
	return connect.ClientInfo{Version: info.Version, DeviceType: info.DeviceType, AppVersion: info.AppVersion, SdkVersion: info.SdkVersion, Distribution: info.Distribution}
}
