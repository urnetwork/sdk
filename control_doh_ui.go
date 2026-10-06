//go:build !ios_extension

// What the extender settings screens call for the bootstrap DoH servers
// (control_doh.go): the url check a form runs on each line, the presets, and
// the getters and setter of the space's list, which persists through the
// space's manager like the extender settings (K6). Excluded from the ios
// packet tunnel extension, which only reads the stored values.
package sdk

import (
	"slices"
	"strings"

	"github.com/urnetwork/connect"
)

// Returns the error id of one bootstrap DoH url, one of the ControlDohError
// ids, or empty when the space can query it. A form checks each line with this
// as it is typed.
func ValidateControlDohUrl(dohUrl string) (errorId string) {
	if _, _, err := connect.ParseControlDohUrl(dohUrl); err != nil {
		return controlDohErrorId(err)
	}
	return ""
}

// The preset list of bootstrap DoH servers for a country, v4 first, ready for
// `SetControlDohUrls`. "cn" is the "Use China resolvers" preset. Empty when
// the country has none.
func RegionalControlDohUrls(countryCode string) *StringList {
	dohUrls := NewStringList()
	dohUrlsIpv4, dohUrlsIpv6 := connect.RegionalControlDohUrls(countryCode)
	dohUrls.addAll(dohUrlsIpv4...)
	dohUrls.addAll(dohUrlsIpv6...)
	return dohUrls
}

// The space's bootstrap DoH servers, v4 then v6, as the form shows them. Empty
// when it names none, which is the default servers alone.
func (self *NetworkSpace) GetControlDohUrls() *StringList {
	values := self.valuesCopy()
	dohUrlsIpv4, dohUrlsIpv6 := spaceControlDohUrls(&values)
	dohUrls := NewStringList()
	dohUrls.addAll(dohUrlsIpv4...)
	dohUrls.addAll(dohUrlsIpv6...)
	return dohUrls
}

// The space's v4 bootstrap DoH servers, in the order they are tried.
func (self *NetworkSpace) GetControlDohUrlsIpv4() *StringList {
	values := self.valuesCopy()
	dohUrlsIpv4, _ := spaceControlDohUrls(&values)
	dohUrls := NewStringList()
	dohUrls.addAll(dohUrlsIpv4...)
	return dohUrls
}

// The space's v6 bootstrap DoH servers, in the order they are tried.
func (self *NetworkSpace) GetControlDohUrlsIpv6() *StringList {
	values := self.valuesCopy()
	_, dohUrlsIpv6 := spaceControlDohUrls(&values)
	dohUrls := NewStringList()
	dohUrls.addAll(dohUrlsIpv6...)
	return dohUrls
}

// Saves the space's bootstrap DoH servers and replaces the client strategy's
// DoH settings in place: the space, a device bound to it and the screen that
// saved all stay valid. The urls are one per entry, of either family, in the
// order they should be tried. Blank entries and repeats are dropped and each
// url goes in the list of its family. Every url must validate, and nothing is
// saved when one does not. Nil or empty clears the list, which leaves the
// default servers alone.
//
// Returns the error id of the first url that does not validate, or
// `ControlDohErrorTooMany` for more than `connect.ControlDohMaxUrlCount` of one
// family, else empty. On ios this writes the app group values the packet
// tunnel extension reads at its next start, and the desktop services take
// them at the next tunnel start, which is what the apps tell the user.
//
// The space a cloud host shares among its hosted devices
// (NewPlatformNetworkSpace) refuses bootstrap DoH servers, which its host
// would query: it saves nothing and returns empty, the hosted-incompatible
// no-op.
func (self *NetworkSpace) SetControlDohUrls(dohUrls *StringList) (errorId string) {
	if self.hostedIncompatibleGuarded("SetControlDohUrls") {
		return ""
	}
	var dohUrlsIpv4 []string
	var dohUrlsIpv6 []string
	if dohUrls != nil {
		for _, dohUrl := range dohUrls.getAll() {
			if strings.TrimSpace(dohUrl) == "" {
				continue
			}
			parsedUrl, addr, err := connect.ParseControlDohUrl(dohUrl)
			if err != nil {
				return controlDohErrorId(err)
			}
			if addr.Is4() {
				if !slices.Contains(dohUrlsIpv4, parsedUrl) {
					dohUrlsIpv4 = append(dohUrlsIpv4, parsedUrl)
				}
			} else if !slices.Contains(dohUrlsIpv6, parsedUrl) {
				dohUrlsIpv6 = append(dohUrlsIpv6, parsedUrl)
			}
		}
	}
	if connect.ControlDohMaxUrlCount < len(dohUrlsIpv4) || connect.ControlDohMaxUrlCount < len(dohUrlsIpv6) {
		return ControlDohErrorTooMany
	}
	self.updateInPlaceValues(func(values *NetworkSpaceValues) {
		values.ControlDohUrlsIpv4 = dohUrlsIpv4
		values.ControlDohUrlsIpv6 = dohUrlsIpv6
	})
	return ""
}
