//go:build js

// The dns settings a page reads and writes carry the selection the device
// derives the lists from (ADBLOCK3RD E3), and the dns resolver status and the
// blocked-lookups count reach the page as the sdk reads them (C5, I3).
package main

import (
	"syscall/js"
	"testing"

	"github.com/urnetwork/sdk/v2026"
)

// The settings' selection crosses to the page and back with the lists.
func TestDnsResolverSettingsWasmCarriesTheSelection(t *testing.T) {
	settings := sdk.DnsResolverSettingsWithPreset(sdk.GetDefaultDnsResolverSettings(), "nextdns-personal", "abc123")
	if settings == nil {
		t.Fatal("the personal preset could not be selected")
	}
	page := jsDnsResolverSettings(settings)
	if page.Get("presetId").String() != "nextdns-personal" || page.Get("presetPersonalId").String() != "abc123" {
		t.Fatalf("the page reads presetId %q presetPersonalId %q", page.Get("presetId").String(), page.Get("presetPersonalId").String())
	}
	back := parseDnsResolverSettings(page)
	if back.PresetId != settings.PresetId || back.PresetPersonalId != settings.PresetPersonalId ||
		back.RemoteDohUrlsIpv4.Len() != 1 || back.RemoteDohUrlsIpv4.Get(0) != settings.RemoteDohUrlsIpv4.Get(0) {
		t.Fatalf("the page's settings parsed as %+v", back)
	}
	// a page that predates the selection writes custom settings
	legacy := js.Global().Get("Object").New()
	legacy.Set("enableRemoteDoh", true)
	if parsed := parseDnsResolverSettings(legacy); parsed.PresetId != "" || parsed.PresetPersonalId != "" {
		t.Fatalf("a page without a selection parsed as %q", parsed.PresetId)
	}
}

// The binding has the status and count methods, and an unreachable device
// reads an unknown status and no count.
func TestDnsResolverStatusWasmBinding(t *testing.T) {
	_, device := newUnopenedExtensionDeviceRemote(t)
	for _, method := range []string{"getDnsResolverStatus", "addDnsResolverStatusChangeListener", "getDnsBlockedCount"} {
		if device.Get(method).Type() != js.TypeFunction {
			t.Fatalf("the DeviceRemote binding has no %s", method)
		}
	}
	status := device.Call("getDnsResolverStatus")
	if status.Get("state").String() != sdk.DnsResolverStateUnknown || status.Get("verification").String() != sdk.DnsResolverVerificationNone {
		t.Fatalf("an unreachable device reads state %q verification %q", status.Get("state").String(), status.Get("verification").String())
	}
	if count := device.Call("getDnsBlockedCount").Int(); count != 0 {
		t.Fatalf("an unreachable device reads %d blocked lookups", count)
	}
	page := jsDnsResolverStatus(&sdk.DnsResolverStatus{
		State:        sdk.DnsResolverStateFailedOver,
		Verification: sdk.DnsResolverVerificationConfirmed,
		PresetId:     "adguard",
		SinceTime:    1794200000000,
		BlockedCount: 12,
	})
	if page.Get("state").String() != "failedOver" || page.Get("verification").String() != "confirmed" ||
		page.Get("presetId").String() != "adguard" || page.Get("sinceTime").Float() != 1794200000000 ||
		page.Get("blockedCount").Int() != 12 {
		t.Fatal("the page reads a different status")
	}
}
