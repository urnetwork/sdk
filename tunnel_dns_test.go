package sdk

import (
	"context"
	"slices"
	"testing"

	"github.com/urnetwork/connect"
)

func TestTunnelDnsAddresses(t *testing.T) {
	defaultSetting := DefaultTunnelDnsSetting()

	assertAddresses := func(name string, actual []string, expected []string) {
		if !slices.Equal(actual, expected) {
			t.Fatalf("%s = %v, expected %v", name, actual, expected)
		}
	}

	// no resolver settings: the default tunnel dns, ipv4 only
	assertAddresses(
		"nil resolver ipv4",
		tunnelDnsAddresses(nil, defaultSetting, false),
		[]string{"9.9.9.9", "1.1.1.1"},
	)
	assertAddresses(
		"nil resolver ipv6",
		tunnelDnsAddresses(nil, defaultSetting, true),
		[]string{},
	)

	// local dns disabled: the addresses are not "set", use the default
	disabled := &connect.DnsResolverSettings{
		EnableLocalDns: false,
		LocalDnsIpv4:   []string{"223.5.5.5"},
	}
	assertAddresses(
		"disabled local dns",
		tunnelDnsAddresses(disabled, defaultSetting, false),
		[]string{"9.9.9.9", "1.1.1.1"},
	)

	// unencrypted local addresses set: use them
	local := &connect.DnsResolverSettings{
		EnableLocalDns: true,
		LocalDnsIpv4:   []string{"223.5.5.5", " 119.29.29.29 "},
		LocalDnsIpv6:   []string{"2400:3200::1"},
	}
	assertAddresses(
		"local dns ipv4",
		tunnelDnsAddresses(local, defaultSetting, false),
		[]string{"223.5.5.5", "119.29.29.29"},
	)
	assertAddresses(
		"local dns ipv6",
		tunnelDnsAddresses(local, defaultSetting, true),
		[]string{"2400:3200::1"},
	)

	// a misfiled entry lands in its actual family
	misfiled := &connect.DnsResolverSettings{
		EnableLocalDns: true,
		LocalDnsIpv4:   []string{"2400:3200::1"},
	}
	assertAddresses(
		"misfiled ipv6 entry",
		tunnelDnsAddresses(misfiled, defaultSetting, true),
		[]string{"2400:3200::1"},
	)
	assertAddresses(
		"misfiled leaves ipv4 to the default",
		tunnelDnsAddresses(misfiled, defaultSetting, false),
		[]string{"9.9.9.9", "1.1.1.1"},
	)

	// entries that do not parse as ips are dropped; all-invalid uses the default
	invalid := &connect.DnsResolverSettings{
		EnableLocalDns: true,
		LocalDnsIpv4:   []string{"not-an-ip", "dns.example.com", ""},
	}
	assertAddresses(
		"invalid local entries",
		tunnelDnsAddresses(invalid, defaultSetting, false),
		[]string{"9.9.9.9", "1.1.1.1"},
	)

	// local dns enabled with no addresses: use the default
	empty := &connect.DnsResolverSettings{
		EnableLocalDns: true,
	}
	assertAddresses(
		"enabled with no addresses",
		tunnelDnsAddresses(empty, defaultSetting, false),
		[]string{"9.9.9.9", "1.1.1.1"},
	)

	// an explicit single-server override narrows the tunnel to that resolver
	// instead of the default list
	override := &TunnelDnsSetting{Server: "223.5.5.5"}
	assertAddresses(
		"single-server override ipv4",
		tunnelDnsAddresses(nil, override, false),
		[]string{"223.5.5.5"},
	)
	// a v4-only override contributes nothing to the ipv6 list
	assertAddresses(
		"single-server override ipv6",
		tunnelDnsAddresses(nil, override, true),
		[]string{},
	)

	// no default setting and nothing set: no addresses
	assertAddresses(
		"no default setting",
		tunnelDnsAddresses(empty, nil, false),
		[]string{},
	)
}

func TestDeviceLocalTunnelDnsAddresses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device, _ := testing_newBlockDevice(ctx, t, false)
	defer device.Close()

	// no unencrypted local dns is enabled by default, so the tunnel dns comes from
	// the default tunnel dns setting: the default plain-DNS resolver list
	if addresses := device.TunnelDnsAddressesIpv4().getAll(); !slices.Equal(addresses, []string{"9.9.9.9", "1.1.1.1"}) {
		t.Fatalf("default tunnel dns ipv4 = %v", addresses)
	}
	if addresses := device.TunnelDnsAddressesIpv6().getAll(); len(addresses) != 0 {
		t.Fatalf("default tunnel dns ipv6 = %v", addresses)
	}

	// setting unencrypted local servers changes the tunnel dns
	settings := device.GetDnsResolverSettings()
	settings.EnableLocalDns = true
	settings.LocalDnsIpv4 = testing_stringList("223.5.5.5", "119.29.29.29")
	device.SetDnsResolverSettings(settings)
	if addresses := device.TunnelDnsAddressesIpv4().getAll(); !slices.Equal(addresses, []string{"223.5.5.5", "119.29.29.29"}) {
		t.Fatalf("tunnel dns ipv4 = %v", addresses)
	}

	// disabling local dns falls back to the default tunnel dns setting
	settings.EnableLocalDns = false
	device.SetDnsResolverSettings(settings)
	if addresses := device.TunnelDnsAddressesIpv4().getAll(); !slices.Equal(addresses, []string{"9.9.9.9", "1.1.1.1"}) {
		t.Fatalf("tunnel dns ipv4 = %v", addresses)
	}
}
