package sdk

// vless_settings.go — the VLESS server of a network space (connect vless.go).
//
// A space stores one VLESS server in its values. When the settings are
// enabled and valid, the space's client strategy carries its connections
// through the server as one more dialer beside the direct, resilient and
// extender ones. The settings apply in place, like the extender settings
// (K6), so a device bound to the space survives a save. On ios the packet
// tunnel extension reads the stored values at its next start, and the
// desktop services import the space at the next tunnel start.
//
// `VlessSettings` is the settings form: plain strings, ints and bools, so it
// crosses gomobile and the desktop c abi unchanged. The list and binary
// values of `connect.VlessConfig` travel in their share link forms: the alpn
// list comma separated, the reality public key as base64url and the short id
// as hex.
//
// The app-facing helpers are in vless_settings_ui.go.

import (
	"encoding/hex"
	"strings"

	"github.com/urnetwork/connect"
)

// The error ids of a VLESS form. Each is a localization key id, so the apps
// map them to their own strings. A function that answers one names its result
// `errorId`, empty on success, which tells the C ABI to answer
// URNET_ERROR_ID_INTERNAL rather than empty when a call cannot run there
// (cgo/gen/gen.go errorIdResult).
const (
	VlessErrorLinkInvalid            = "vless_error_link_invalid"
	VlessErrorLinkUnsupported        = "vless_error_link_unsupported"
	VlessErrorAddressInvalid         = "vless_error_address_invalid"
	VlessErrorPortInvalid            = "vless_error_port_invalid"
	VlessErrorIdInvalid              = "vless_error_id_invalid"
	VlessErrorNetworkUnsupported     = "vless_error_network_unsupported"
	VlessErrorSecurityUnsupported    = "vless_error_security_unsupported"
	VlessErrorFlowInvalid            = "vless_error_flow_invalid"
	VlessErrorServerNameRequired     = "vless_error_server_name_required"
	VlessErrorFingerprintUnsupported = "vless_error_fingerprint_unsupported"
	VlessErrorPublicKeyInvalid       = "vless_error_public_key_invalid"
	VlessErrorShortIdInvalid         = "vless_error_short_id_invalid"
)

// One VLESS server as the settings screens edit it and the space stores it.
type VlessSettings struct {
	// The client strategy dials through the server only while this is on. A
	// space keeps the rest of the settings while it is off.
	Enabled bool `json:"enabled,omitempty"`
	// a label for people, the share link's fragment
	Name string `json:"name,omitempty"`
	// host name or ip literal of the server
	Address string `json:"address,omitempty"`
	Port    int    `json:"port,omitempty"`
	// the user id, a uuid or a custom id of up to 30 bytes
	Id string `json:"id,omitempty"`
	// "" or "xtls-rprx-vision"
	Flow string `json:"flow,omitempty"`
	// "tcp", "ws" or "httpupgrade"
	Network string `json:"network,omitempty"`
	// "none", "tls" or "reality"
	Security string `json:"security,omitempty"`
	// the tls server name (tls, reality)
	ServerName string `json:"server_name,omitempty"`
	// the imitated tls client hello, one of `VlessFingerprints`
	Fingerprint string `json:"fingerprint,omitempty"`
	// the outer tls alpn list, comma separated (tls)
	Alpn string `json:"alpn,omitempty"`
	// skip the outer certificate check (tls)
	AllowInsecure bool `json:"allow_insecure,omitempty"`
	// the server's x25519 public key, base64url (reality)
	PublicKey string `json:"public_key,omitempty"`
	// up to 16 hex digits (reality)
	ShortId string `json:"short_id,omitempty"`
	// kept so a link round-trips (reality)
	SpiderX string `json:"spider_x,omitempty"`
	// the http path (ws, httpupgrade)
	Path string `json:"path,omitempty"`
	// the http host header (ws, httpupgrade)
	Host string `json:"host,omitempty"`
}

// Every field is a value, so a copy is a struct copy.
func (self *VlessSettings) copy() *VlessSettings {
	if self == nil {
		return nil
	}
	copied := *self
	return &copied
}

// A copy with the whitespace a form leaves trimmed, the enumerations lower
// case, an address typed in ipv6 brackets unbracketed and the alpn list
// without blanks.
func (self *VlessSettings) normalized() *VlessSettings {
	if self == nil {
		return nil
	}
	settings := *self
	settings.Name = strings.TrimSpace(settings.Name)
	settings.Address = strings.TrimSpace(settings.Address)
	if strings.HasPrefix(settings.Address, "[") && strings.HasSuffix(settings.Address, "]") {
		settings.Address = settings.Address[1 : len(settings.Address)-1]
	}
	settings.Id = strings.TrimSpace(settings.Id)
	settings.Flow = strings.ToLower(strings.TrimSpace(settings.Flow))
	settings.Network = strings.ToLower(strings.TrimSpace(settings.Network))
	settings.Security = strings.ToLower(strings.TrimSpace(settings.Security))
	settings.ServerName = strings.TrimSpace(settings.ServerName)
	settings.Fingerprint = strings.ToLower(strings.TrimSpace(settings.Fingerprint))
	alpns := []string{}
	for _, alpn := range strings.Split(settings.Alpn, ",") {
		if alpn = strings.TrimSpace(alpn); alpn != "" {
			alpns = append(alpns, alpn)
		}
	}
	settings.Alpn = strings.Join(alpns, ",")
	settings.PublicKey = strings.TrimSpace(settings.PublicKey)
	settings.ShortId = strings.ToLower(strings.TrimSpace(settings.ShortId))
	settings.SpiderX = strings.TrimSpace(settings.SpiderX)
	settings.Path = strings.TrimSpace(settings.Path)
	settings.Host = strings.TrimSpace(settings.Host)
	return &settings
}

// Off and naming no server: what a space with no VLESS stores, which is
// nothing.
func (self *VlessSettings) empty() bool {
	return self == nil || (!self.Enabled && self.Address == "" && self.Id == "")
}

// The connect configuration the settings describe, or the error id of the
// first problem, in the order a form is filled in.
func (self *VlessSettings) connectConfig() (*connect.VlessConfig, string) {
	settings := self.normalized()
	if settings == nil {
		return nil, VlessErrorLinkInvalid
	}
	config := &connect.VlessConfig{
		Name:          settings.Name,
		Address:       settings.Address,
		Port:          settings.Port,
		Id:            settings.Id,
		Flow:          settings.Flow,
		Network:       settings.Network,
		Security:      settings.Security,
		ServerName:    settings.ServerName,
		Fingerprint:   settings.Fingerprint,
		AllowInsecure: settings.AllowInsecure,
		SpiderX:       settings.SpiderX,
		Path:          settings.Path,
		Host:          settings.Host,
	}
	if settings.Alpn != "" {
		config.Alpns = strings.Split(settings.Alpn, ",")
	}
	shortIdInvalid := false
	if settings.Security == connect.VlessSecurityReality {
		// an undecodable key is left out, which validation reports after the
		// fields before it
		if publicKey, err := connect.DecodeVlessPublicKey(settings.PublicKey); err == nil {
			config.PublicKey = publicKey
		}
		if shortId, err := connect.DecodeVlessShortId(settings.ShortId); err == nil {
			config.ShortId = shortId
		} else {
			shortIdInvalid = true
		}
	}
	if err := config.Validate(); err != nil {
		return nil, vlessErrorId(err)
	}
	if shortIdInvalid {
		return nil, VlessErrorShortIdInvalid
	}
	return config, ""
}

// The settings of a configuration read from a link, enabled.
func vlessSettingsFromConfig(config *connect.VlessConfig) *VlessSettings {
	settings := &VlessSettings{
		Enabled:       true,
		Name:          config.Name,
		Address:       config.Address,
		Port:          config.Port,
		Id:            config.Id,
		Flow:          config.Flow,
		Network:       config.Network,
		Security:      config.Security,
		ServerName:    config.ServerName,
		Fingerprint:   config.Fingerprint,
		Alpn:          strings.Join(config.Alpns, ","),
		AllowInsecure: config.AllowInsecure,
		SpiderX:       config.SpiderX,
		Path:          config.Path,
		Host:          config.Host,
	}
	if config.Security == connect.VlessSecurityReality {
		settings.PublicKey = connect.EncodeVlessPublicKey(config.PublicKey)
		settings.ShortId = hex.EncodeToString(config.ShortId)
	}
	return settings
}

// The error id of a connect configuration error.
func vlessErrorId(err error) string {
	if code := connect.VlessConfigErrorCode(err); code != "" {
		return "vless_error_" + code
	}
	return VlessErrorLinkInvalid
}

// The strategy configurations of a space's stored settings: the server when
// they are enabled and valid, none otherwise.
func spaceVlessConfigs(settings *VlessSettings) []*connect.VlessConfig {
	if settings == nil || !settings.Enabled {
		return nil
	}
	config, errorId := settings.connectConfig()
	if errorId != "" {
		return nil
	}
	return []*connect.VlessConfig{config}
}

func vlessSettingsEqual(previous *VlessSettings, next *VlessSettings) bool {
	if previous == nil || next == nil {
		return previous == next
	}
	return *previous == *next
}

func vlessValuesChanged(previous *NetworkSpaceValues, next *NetworkSpaceValues) bool {
	return !vlessSettingsEqual(previous.Vless, next.Vless)
}
