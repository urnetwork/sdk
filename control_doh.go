// The bootstrap DoH servers of a network space (connect
// net_http_doh_control.go).
//
// The space resolves its own names (api, connect, extender) only over DoH,
// through the client strategy's internal DoH cache, and the extender
// bootstrap queries the same servers. A user on a network that blocks the
// default DoH servers can name servers that work there:
// `control_doh_urls_ipv4` and `control_doh_urls_ipv6`, `https://<ip
// literal>/<path>` urls only. They go ahead of the default servers, which stay
// behind them. The chosen servers see the lookups of the space's names, which
// the apps say where the setting is edited.
//
// The settings apply in place, like the extender and VLESS settings (K6): the
// strategy's DoH cache is swapped and the space, a device bound to it and the
// screen that saved stay valid. On ios the packet tunnel extension reads the
// stored values at its next start, and the desktop services import the space
// at the next tunnel start.
//
// Ingress and egress dns are separate. Ingress dns resolves the space's own
// names, the platform the app dials (api, connect, extender): these settings,
// picked by the space's ingress country (`IngressCountryCode`, the "Connection
// region" the user chooses on the login screen or in the dns settings).
// Egress dns resolves user traffic through the tunnel, the device's dns
// settings, which never read the ingress country. The ingress country is
// local only. Undisclosed takes the default ingress dns; when every encrypted
// resolver fails, the space's own names fall back to plain dns (connect
// net_http_doh_bootstrap.go).
//
// The app-facing helpers are in control_doh_ui.go.
package sdk

import (
	"slices"

	"github.com/urnetwork/connect"
)

// The error ids of a bootstrap DoH url. Each is a localization key id, so the
// apps map them to their own strings. A function that answers one names its
// result `errorId`, as the VLESS ones do (vless_settings.go).
const (
	ControlDohErrorUrlInvalid    = "control_doh_error_url_invalid"
	ControlDohErrorHttpsRequired = "control_doh_error_https_required"
	ControlDohErrorIpRequired    = "control_doh_error_ip_required"
	ControlDohErrorTooMany       = "control_doh_error_too_many"
)

// When set, adjusts the DoH settings a space's strategy is given. Production
// never sets it; the tests install the trust of an in-process DoH server and a
// dial that black-holes the default servers through it, so nothing resolves
// for real.
var controlDohSettingsConfigure func(settings *connect.DohSettings)

// The error id of a connect bootstrap DoH url error.
func controlDohErrorId(err error) string {
	if code := connect.ControlDohUrlErrorCode(err); code != "" {
		return "control_doh_error_" + code
	}
	return ControlDohErrorUrlInvalid
}

// The bootstrap DoH servers a space's values name, each read by the url rule
// and put in the list of its family, without blanks or repeats and at most
// `connect.ControlDohMaxUrlCount` of a family. A stored url the rule refuses
// is left out rather than queried: the values may have been written by hand.
func spaceControlDohUrls(values *NetworkSpaceValues) (dohUrlsIpv4 []string, dohUrlsIpv6 []string) {
	for _, dohUrl := range slices.Concat(values.ControlDohUrlsIpv4, values.ControlDohUrlsIpv6) {
		parsedUrl, addr, err := connect.ParseControlDohUrl(dohUrl)
		if err != nil {
			continue
		}
		if addr.Is4() {
			if !slices.Contains(dohUrlsIpv4, parsedUrl) && len(dohUrlsIpv4) < connect.ControlDohMaxUrlCount {
				dohUrlsIpv4 = append(dohUrlsIpv4, parsedUrl)
			}
		} else if !slices.Contains(dohUrlsIpv6, parsedUrl) && len(dohUrlsIpv6) < connect.ControlDohMaxUrlCount {
			dohUrlsIpv6 = append(dohUrlsIpv6, parsedUrl)
		}
	}
	return
}

// The ingress country of a space's values: a country with a recommendation
// (`connect.IngressCountryCodes`), else undisclosed (""). A stored value the
// table does not know is left out: the values may have been written by hand.
func spaceIngressCountryCode(values *NetworkSpaceValues) string {
	countryCode := connect.NormalizeIngressCountryCode(values.IngressCountryCode)
	if !slices.Contains(connect.IngressCountryCodes(), countryCode) {
		return ""
	}
	return countryCode
}

// The DoH settings of a space's client strategy: the ingress (control-name)
// settings of the space's ingress country, with the space's bootstrap DoH
// servers ahead of the defaults when it names any.
func spaceControlDohSettings(values *NetworkSpaceValues) *connect.DohSettings {
	dohUrlsIpv4, dohUrlsIpv6 := spaceControlDohUrls(values)
	settings := connect.IngressDohSettings(spaceIngressCountryCode(values), dohUrlsIpv4, dohUrlsIpv6)
	if controlDohSettingsConfigure != nil {
		controlDohSettingsConfigure(settings)
	}
	return settings
}

// Reports whether two value sets name different bootstrap DoH servers or
// ingress countries. The
// comparison is of the urls the rule reads, so an edit that only adds
// whitespace, a blank line or a repeat changes nothing.
func controlDohValuesChanged(previous *NetworkSpaceValues, next *NetworkSpaceValues) bool {
	previousDohUrlsIpv4, previousDohUrlsIpv6 := spaceControlDohUrls(previous)
	nextDohUrlsIpv4, nextDohUrlsIpv6 := spaceControlDohUrls(next)
	return !slices.Equal(previousDohUrlsIpv4, nextDohUrlsIpv4) || !slices.Equal(previousDohUrlsIpv6, nextDohUrlsIpv6) ||
		spaceIngressCountryCode(previous) != spaceIngressCountryCode(next)
}
