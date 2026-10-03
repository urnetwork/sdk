package sdk

// provider_family_transport_status.go — the per-family readout of the
// provider's platform transports (connect/IPV6.md A4), for the apps' connect
// drawer and developer screens.
//
// A provider runs a v4-pinned, a v6-pinned and a family-agnostic standby
// platform transport as one group (connect.FamilyPlatformTransportGroup). The
// states here are connect.PlatformTransportState strings: "connecting",
// "connected", "disabled", "sleeping" (the device has no path of that
// family), "idle-policy" (the control family policy forbids that family).
// "unknown" is reported when the device cannot be reached.
//
// Not to be confused with TransportStatus, which is the memory-budget
// eligibility of the transport modes (GetProviderTransportStatus).

import (
	"github.com/urnetwork/connect"
)

// ProviderFamilyTransportStateUnknown is the state reported for a transport
// the device cannot describe: a remote device out of contact, or no provider.
const ProviderFamilyTransportStateUnknown = "unknown"

// ProviderFamilyTransportStatus is one snapshot of the provider's transports.
// Plain bools and strings so gomobile and the cgo/js bindings carry it as is.
type ProviderFamilyTransportStatus struct {
	// HasIpv4 is false when no v4-pinned transport is configured (the
	// network space has no family urls, or a legacy single transport).
	HasIpv4   bool
	Ipv4State string
	HasIpv6   bool
	Ipv6State string
	// StandbyState is the family-agnostic transport. With no pinned
	// transports it is the provider's only transport.
	StandbyState string
	// StandbyActive is true while the standby is released to dial, whether
	// or not it has connected.
	StandbyActive bool
}

// newProviderFamilyTransportStatus maps a group readout to the app form.
func newProviderFamilyTransportStatus(status connect.FamilyPlatformTransportGroupStatus) *ProviderFamilyTransportStatus {
	out := &ProviderFamilyTransportStatus{
		HasIpv4:       status.HasIpv4,
		HasIpv6:       status.HasIpv6,
		StandbyState:  status.Standby.String(),
		StandbyActive: status.StandbyActive,
	}
	if status.HasIpv4 {
		out.Ipv4State = status.Ipv4.String()
	} else {
		out.Ipv4State = ProviderFamilyTransportStateUnknown
	}
	if status.HasIpv6 {
		out.Ipv6State = status.Ipv6.String()
	} else {
		out.Ipv6State = ProviderFamilyTransportStateUnknown
	}
	return out
}

// legacyProviderFamilyTransportStatus describes a single family-agnostic
// transport (no pinned transports) from its connected bit.
func legacyProviderFamilyTransportStatus(connected bool) *ProviderFamilyTransportStatus {
	standby := connect.PlatformTransportStateConnecting
	if connected {
		standby = connect.PlatformTransportStateConnected
	}
	return &ProviderFamilyTransportStatus{
		Ipv4State:     ProviderFamilyTransportStateUnknown,
		Ipv6State:     ProviderFamilyTransportStateUnknown,
		StandbyState:  standby.String(),
		StandbyActive: true,
	}
}

// unknownProviderFamilyTransportStatus is the readout when there is no
// provider to describe.
func unknownProviderFamilyTransportStatus() *ProviderFamilyTransportStatus {
	return &ProviderFamilyTransportStatus{
		Ipv4State:    ProviderFamilyTransportStateUnknown,
		Ipv6State:    ProviderFamilyTransportStateUnknown,
		StandbyState: ProviderFamilyTransportStateUnknown,
	}
}

func cloneProviderFamilyTransportStatus(status *ProviderFamilyTransportStatus) *ProviderFamilyTransportStatus {
	if status == nil {
		return nil
	}
	copied := *status
	return &copied
}
