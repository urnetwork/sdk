//go:build !ios_extension

package sdk

// vless_settings_ui.go — what the VLESS settings screens call: Account >
// Settings > VLESS and the network settings of the login screen. Both edit
// the network space through `GetVlessSettings` and `SetVlessSettings`, which
// persist through the space's manager like the extender settings (K6).
// Excluded from the ios packet tunnel extension, which only reads the stored
// values (vless_settings.go).

import (
	"github.com/urnetwork/connect"
)

// The settings a new form starts from: a reality server over raw tcp with the
// vision flow and a chrome hello on 443, which is how most VLESS servers are
// shared. Not enabled.
func NewVlessSettings() *VlessSettings {
	return &VlessSettings{
		Port:        443,
		Flow:        connect.VlessFlowVision,
		Network:     connect.VlessNetworkTcp,
		Security:    connect.VlessSecurityReality,
		Fingerprint: "chrome",
	}
}

// What a pasted share link reads as.
type VlessLinkResult struct {
	// the settings of the link, enabled; nil when Error is set
	Settings *VlessSettings
	// one of the VlessError ids, empty when the link was read
	Error string
}

// ParseVlessLink reads a `vless://` share link into settings.
func ParseVlessLink(link string) *VlessLinkResult {
	config, err := connect.ParseVlessLink(link)
	if err != nil {
		return &VlessLinkResult{Error: vlessErrorId(err)}
	}
	return &VlessLinkResult{Settings: vlessSettingsFromConfig(config)}
}

// The share link of the settings, for copying to another device or app.
// Empty when they do not validate.
func VlessSettingsLink(settings *VlessSettings) string {
	config, errorId := settings.connectConfig()
	if errorId != "" {
		return ""
	}
	return config.Link()
}

// The error id of the first problem with the settings, or empty when they can
// be dialed. Whether they are enabled does not matter here.
func ValidateVlessSettings(settings *VlessSettings) (errorId string) {
	_, errorId = settings.connectConfig()
	return errorId
}

// The transports a form offers.
func VlessNetworks() *StringList {
	networks := NewStringList()
	networks.addAll(connect.VlessNetworkTcp, connect.VlessNetworkWs, connect.VlessNetworkHttpUpgrade)
	return networks
}

// The securities a form offers.
func VlessSecurities() *StringList {
	securities := NewStringList()
	securities.addAll(connect.VlessSecurityNone, connect.VlessSecurityTls, connect.VlessSecurityReality)
	return securities
}

// The flows a form offers; the empty flow is none.
func VlessFlows() *StringList {
	flows := NewStringList()
	flows.addAll(connect.VlessFlowNone, connect.VlessFlowVision)
	return flows
}

// The tls client hellos a form offers; the empty fingerprint is the Go tls
// client for tls, and chrome for reality.
func VlessFingerprints() *StringList {
	fingerprints := NewStringList()
	fingerprints.addAll("", "chrome", "firefox", "safari", "ios", "android", "edge", "360", "qq", "random", "randomized")
	return fingerprints
}

// The space's VLESS settings, or the new-form defaults (not enabled) when it
// has none. Never nil; the caller owns the copy.
func (self *NetworkSpace) GetVlessSettings() *VlessSettings {
	values := self.valuesCopy()
	if values.Vless == nil {
		return NewVlessSettings()
	}
	return values.Vless.copy()
}

// SetVlessSettings saves the space's VLESS settings and replaces the client
// strategy's VLESS dialer in place: the space, a device bound to it and the
// screen that saved all stay valid. Enabled settings must validate, and
// nothing is saved when they do not; settings that are off are kept as they
// are and remove the dialer. Nil, or settings that are off and name no
// server, clear them.
//
// Returns the error id of enabled settings that do not validate, else empty.
// On ios this writes the app group values the packet tunnel extension reads
// at its next start, and the desktop services take them at the next tunnel
// start, which is what the apps tell the user.
//
// The space a cloud host shares among its hosted devices
// (NewPlatformNetworkSpace) refuses VLESS, which is not cloud safe: it saves
// nothing and returns empty, the hosted-incompatible no-op.
func (self *NetworkSpace) SetVlessSettings(settings *VlessSettings) (errorId string) {
	if self.hostedIncompatible {
		self.logger().Infof("[ns]hosted incompatible: SetVlessSettings ignored\n")
		return ""
	}
	stored := settings.normalized()
	if stored != nil && stored.Enabled {
		if _, errorId = stored.connectConfig(); errorId != "" {
			return errorId
		}
	}
	if stored.empty() {
		stored = nil
	}
	self.updateInPlaceValues(func(values *NetworkSpaceValues) {
		values.Vless = stored.copy()
	})
	return ""
}
