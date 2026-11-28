package hotspot

// interfaces.go — which addresses the proxy may listen on. The platform
// reports facts about its interfaces (HotspotInterface); the choice is made
// here, so every platform binds by the same rules.

import (
	"cmp"
	"net/netip"
	"slices"

	"github.com/urnetwork/sdk/v2026"
)

// The narrowest prefix accepted as a hotspot subnet. The subnet is also the
// set of sources the proxy admits, so a wider one, up to the whole IPv4 space,
// is refused rather than trusted.
const minHotspotPrefixBits = 8

// One network interface as the platform reports it. The platform reports
// facts only and never decides what to bind; see hotspotProxyBindAddresses.
type HotspotInterface struct {
	// the interface name, e.g. wlan1, ap0, rndis0, ncm0 or bt-pan
	Name string
	// the interface's addresses in prefix form, e.g. 192.168.43.1/24
	Addresses *sdk.StringList
	// the platform reports it as a tethering downstream (android: an
	// interface in the active tether list of TETHER_STATE_CHANGED)
	Tethered bool
	// the interface carries one of the device's own networks, such as
	// cellular, wi-fi, ethernet or a vpn (android: the interface of a
	// ConnectivityManager network)
	Upstream bool
}

func NewHotspotInterface() *HotspotInterface {
	return &HotspotInterface{
		Addresses: sdk.NewStringList(),
	}
}

// A list of interfaces, since gomobile binds no slice of structs.
type HotspotInterfaceList struct {
	interfaces []*HotspotInterface
}

func NewHotspotInterfaceList() *HotspotInterfaceList {
	return &HotspotInterfaceList{}
}

func (self *HotspotInterfaceList) Len() int {
	return len(self.interfaces)
}

func (self *HotspotInterfaceList) Get(i int) *HotspotInterface {
	return self.interfaces[i]
}

func (self *HotspotInterfaceList) Add(hotspotInterface *HotspotInterface) {
	self.interfaces = append(self.interfaces, hotspotInterface)
}

// The proxy's copy of one reported interface, so a caller that changes its
// gomobile objects later cannot change what the proxy chose from.
type hotspotProxyInterface struct {
	name      string
	addresses []string
	tethered  bool
	upstream  bool
}

func copyHotspotInterfaces(interfaceList *HotspotInterfaceList) []hotspotProxyInterface {
	if interfaceList == nil {
		return nil
	}
	interfaces := []hotspotProxyInterface{}
	for _, hotspotInterface := range interfaceList.interfaces {
		if hotspotInterface == nil {
			continue
		}
		addresses := []string{}
		if hotspotInterface.Addresses != nil {
			for i := range hotspotInterface.Addresses.Len() {
				addresses = append(addresses, hotspotInterface.Addresses.Get(i))
			}
		}
		interfaces = append(interfaces, hotspotProxyInterface{
			name:      hotspotInterface.Name,
			addresses: addresses,
			tethered:  hotspotInterface.Tethered,
			upstream:  hotspotInterface.Upstream,
		})
	}
	return interfaces
}

// One address the proxy listens on: the interface's own address with its
// subnet, which is also the set of sources it admits there.
type hotspotProxyBindAddress struct {
	interfaceName string
	prefix        netip.Prefix
}

// The addresses the proxy may listen on: each private IPv4 address of an
// interface that the platform reports as a tethering downstream and not as
// one of the device's own networks. Nothing else is ever bound. Refused are:
//   - an interface that is not tethered, or that carries a device network
//     (the cellular WAN, a wi-fi the phone joined, a vpn), even if tethered;
//   - the wildcard, loopback, link-local, multicast and public addresses;
//   - IPv6, whose hotspot addresses can be globally routed;
//   - a subnet wider than minHotspotPrefixBits.
//
// Sorted by interface name, then address, so the endpoints read the same on
// every report.
func hotspotProxyBindAddresses(interfaces []hotspotProxyInterface) []hotspotProxyBindAddress {
	bindAddresses := []hotspotProxyBindAddress{}
	seen := map[netip.Addr]bool{}
	for _, hotspotInterface := range interfaces {
		if hotspotInterface.name == "" || !hotspotInterface.tethered || hotspotInterface.upstream {
			continue
		}
		for _, address := range hotspotInterface.addresses {
			prefix, err := netip.ParsePrefix(address)
			if err != nil {
				continue
			}
			addr := prefix.Addr()
			if !addr.Is4() || !addr.IsPrivate() {
				// IsPrivate is 10/8, 172.16/12 and 192.168/16 for IPv4, which
				// excludes the wildcard, loopback, link-local and multicast.
				// An IPv4-mapped IPv6 form is not Is4 and is refused too.
				continue
			}
			if prefix.Bits() < minHotspotPrefixBits {
				continue
			}
			if seen[addr] {
				continue
			}
			seen[addr] = true
			bindAddresses = append(bindAddresses, hotspotProxyBindAddress{
				interfaceName: hotspotInterface.name,
				prefix:        netip.PrefixFrom(addr, prefix.Bits()),
			})
		}
	}
	slices.SortFunc(bindAddresses, func(a hotspotProxyBindAddress, b hotspotProxyBindAddress) int {
		if c := cmp.Compare(a.interfaceName, b.interfaceName); c != 0 {
			return c
		}
		return a.prefix.Addr().Compare(b.prefix.Addr())
	})
	return bindAddresses
}
