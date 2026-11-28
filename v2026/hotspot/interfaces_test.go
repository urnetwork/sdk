package hotspot

import (
	"net/netip"
	"slices"
	"testing"
)

func newTestHotspotInterface(name string, tethered bool, upstream bool, addresses ...string) *HotspotInterface {
	hotspotInterface := NewHotspotInterface()
	hotspotInterface.Name = name
	hotspotInterface.Tethered = tethered
	hotspotInterface.Upstream = upstream
	for _, address := range addresses {
		hotspotInterface.Addresses.Add(address)
	}
	return hotspotInterface
}

func newTestHotspotInterfaceList(interfaces ...*HotspotInterface) *HotspotInterfaceList {
	interfaceList := NewHotspotInterfaceList()
	for _, hotspotInterface := range interfaces {
		interfaceList.Add(hotspotInterface)
	}
	return interfaceList
}

func bindAddressStrings(bindAddresses []hotspotProxyBindAddress) []string {
	values := []string{}
	for _, bindAddress := range bindAddresses {
		values = append(values, bindAddress.interfaceName+" "+bindAddress.prefix.String())
	}
	return values
}

// Only the private IPv4 address of a tethering downstream that carries none
// of the device's own networks is ever chosen. The addresses are synthetic.
func TestHotspotProxyBindAddressesOnlyTetheredPrivateIpv4(t *testing.T) {
	interfaces := copyHotspotInterfaces(newTestHotspotInterfaceList(
		// the cellular wan: an upstream, not tethered
		newTestHotspotInterface("rmnet_data0", false, true, "10.20.30.40/24", "2001:db8:1::40/64"),
		// a wi-fi the phone joined, misreported as tethered: still an upstream
		newTestHotspotInterface("wlan0", true, true, "192.168.1.20/24"),
		// the vpn's own interface
		newTestHotspotInterface("tun0", true, true, "10.0.0.2/32"),
		// the wi-fi hotspot, with ipv6 addresses that are never bound
		newTestHotspotInterface("wlan1", true, false, "192.168.43.1/24", "fe80::1/64", "2001:db8:2::1/64", "fd00::1/64"),
		// usb tethering
		newTestHotspotInterface("rndis0", true, false, "192.168.42.129/24"),
		// tethered, but no address that may be bound
		newTestHotspotInterface("bt-pan", true, false,
			"0.0.0.0/0",
			"127.0.0.1/8",
			"169.254.10.1/16",
			"203.0.113.7/24",
			"224.0.0.1/4",
			"10.0.0.1/7",
			"::ffff:192.168.44.1/120",
			"192.168.44.1",
			"not an address",
		),
		// an interface that is not tethered and not an upstream
		newTestHotspotInterface("eth1", false, false, "172.16.5.1/24"),
		// a tethered interface with no name
		newTestHotspotInterface("", true, false, "172.16.6.1/24"),
		// a second tethering interface on a 10/8 and 172.16/12 subnet
		newTestHotspotInterface("ncm0", true, false, "10.97.10.1/24", "172.20.1.1/16"),
		// the hotspot address reported twice binds once
		newTestHotspotInterface("ap0", true, false, "192.168.43.1/24"),
	))

	got := bindAddressStrings(hotspotProxyBindAddresses(interfaces))
	want := []string{
		"ncm0 10.97.10.1/24",
		"ncm0 172.20.1.1/16",
		"rndis0 192.168.42.129/24",
		"wlan1 192.168.43.1/24",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("bind addresses = %v, want %v", got, want)
	}
}

func TestHotspotProxyBindAddressesNoneWithoutAHotspot(t *testing.T) {
	interfaces := copyHotspotInterfaces(newTestHotspotInterfaceList(
		newTestHotspotInterface("rmnet_data0", false, true, "10.20.30.40/24"),
		newTestHotspotInterface("wlan0", false, true, "192.168.1.20/24"),
	))
	if got := hotspotProxyBindAddresses(interfaces); len(got) != 0 {
		t.Fatalf("bind addresses = %v, want none", bindAddressStrings(got))
	}
	if got := hotspotProxyBindAddresses(nil); len(got) != 0 {
		t.Fatalf("bind addresses of no report = %v, want none", bindAddressStrings(got))
	}
}

// The proxy reads a copy: a caller that changes its objects after the report
// does not change what was reported.
func TestHotspotProxyInterfacesAreCopiedFromTheReport(t *testing.T) {
	hotspotInterface := newTestHotspotInterface("wlan1", true, false, "192.168.43.1/24")
	interfaceList := newTestHotspotInterfaceList(hotspotInterface)
	interfaces := copyHotspotInterfaces(interfaceList)

	hotspotInterface.Upstream = true
	hotspotInterface.Addresses.Add("192.168.50.1/24")
	interfaceList.Add(newTestHotspotInterface("rndis0", true, false, "192.168.42.129/24"))

	got := bindAddressStrings(hotspotProxyBindAddresses(interfaces))
	if want := []string{"wlan1 192.168.43.1/24"}; !slices.Equal(got, want) {
		t.Fatalf("bind addresses = %v, want %v", got, want)
	}
}

// A report with nil entries and nil address lists reads as no addresses.
func TestHotspotProxyInterfacesToleratesEmptyEntries(t *testing.T) {
	interfaceList := NewHotspotInterfaceList()
	interfaceList.Add(nil)
	interfaceList.Add(&HotspotInterface{Name: "wlan1", Tethered: true})
	interfaces := copyHotspotInterfaces(interfaceList)
	if len(interfaces) != 1 || len(interfaces[0].addresses) != 0 {
		t.Fatalf("copied interfaces = %+v, want one with no addresses", interfaces)
	}
	if got := hotspotProxyBindAddresses(interfaces); len(got) != 0 {
		t.Fatalf("bind addresses = %v, want none", bindAddressStrings(got))
	}
	if got := copyHotspotInterfaces(nil); got != nil {
		t.Fatalf("copy of no list = %v, want nil", got)
	}
}

// The subnet the screen admits is the interface's subnet, masked.
func TestHotspotProxyBindAddressKeepsTheInterfaceSubnet(t *testing.T) {
	interfaces := copyHotspotInterfaces(newTestHotspotInterfaceList(
		newTestHotspotInterface("wlan1", true, false, "192.168.43.1/24"),
	))
	bindAddresses := hotspotProxyBindAddresses(interfaces)
	if len(bindAddresses) != 1 {
		t.Fatalf("bind addresses = %v, want one", bindAddressStrings(bindAddresses))
	}
	if got, want := bindAddresses[0].prefix.Addr(), netip.MustParseAddr("192.168.43.1"); got != want {
		t.Fatalf("bind address = %v, want %v", got, want)
	}
	if got, want := bindAddresses[0].prefix.Masked(), netip.MustParsePrefix("192.168.43.0/24"); got != want {
		t.Fatalf("admitted subnet = %v, want %v", got, want)
	}
}
