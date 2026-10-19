package sdk

import (
	"os"
	"slices"
	"testing"

	"github.com/urnetwork/connect"
)

// The connection region (ingress country) picks the ingress dns of the space's
// own names from the per-country recommendations (support inbox 7956, 7957,
// 7973): China the AliDNS/DNSPod presets, undisclosed and an unknown country
// the defaults. It applies in place, survives a restart of the manager, is
// stored apart from the bootstrap DoH servers, and never reaches a hosted
// device's strategy.
func TestNetworkSpaceSetIngressCountryCode(t *testing.T) {
	storagePath, err := os.MkdirTemp("", "test_ingress_dns")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	t.Cleanup(networkSpaceManager.Close)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	strategy := networkSpace.clientStrategy

	if countryCode := networkSpace.GetIngressCountryCode(); countryCode != "" {
		t.Fatalf("a new space's region = %q, expected undisclosed", countryCode)
	}
	if countryCode := strategy.DohSettings().IngressCountryCode; countryCode != "" {
		t.Fatalf("a new space's ingress country = %q", countryCode)
	}

	if countryCode := networkSpace.SetIngressCountryCode(" CN "); countryCode != "cn" {
		t.Fatalf("stored region = %q, expected cn", countryCode)
	}
	if networkSpaceManager.GetNetworkSpace(key) != networkSpace || networkSpace.clientStrategy != strategy {
		t.Fatal("saving the region replaced the space")
	}
	if countryCode := strategy.DohSettings().IngressCountryCode; countryCode != "cn" {
		t.Fatalf("the strategy's ingress country = %q, expected cn", countryCode)
	}
	if dohUrls := networkSpace.GetControlDohUrls(); dohUrls.Len() != 0 {
		t.Fatalf("the region changed the bootstrap DoH servers to %v", dohUrls.getAll())
	}
	hostedStrategy := networkSpace.newHostedClientStrategy(nil)
	if countryCode := hostedStrategy.DohSettings().IngressCountryCode; countryCode != "" {
		t.Fatalf("a hosted strategy's ingress country = %q", countryCode)
	}
	hostedStrategy.Close()

	networkSpaceManager.Close()
	restored := NewNetworkSpaceManager(storagePath)
	t.Cleanup(restored.Close)
	restoredSpace := restored.GetNetworkSpace(key)
	if restoredSpace.GetIngressCountryCode() != "cn" || restoredSpace.clientStrategy.DohSettings().IngressCountryCode != "cn" {
		t.Fatal("the region did not survive the restart")
	}

	// a country without a recommendation is undisclosed
	if countryCode := restoredSpace.SetIngressCountryCode("us"); countryCode != "" {
		t.Fatalf("us = %q, expected undisclosed", countryCode)
	}
	if countryCode := restoredSpace.clientStrategy.DohSettings().IngressCountryCode; countryCode != "" {
		t.Fatalf("the strategy's ingress country = %q after undisclosed", countryCode)
	}
}

// A region change alone applies in place.
func TestOnlyInPlaceValuesChangedIncludesIngressCountry(t *testing.T) {
	key := NewNetworkSpaceKey("space.example", "main")
	previous := NetworkSpaceValues{ApiUrl: "https://api.space.example"}
	next := previous
	next.IngressCountryCode = "cn"
	if !onlyInPlaceValuesChanged(key, &previous, &next) {
		t.Fatal("a region change alone must apply in place")
	}
	unknown := previous
	unknown.IngressCountryCode = "us"
	if inPlaceValuesChanged(key, &previous, &unknown) {
		t.Fatal("an unknown region is undisclosed, no change")
	}
}

// The picker lists only regions with a recommendation and shows the resolvers
// each selects.
func TestIngressCountryPicker(t *testing.T) {
	countryCodes := IngressCountryCodes().getAll()
	if !slices.Equal(countryCodes, connect.IngressCountryCodes()) || !slices.Contains(countryCodes, "cn") || slices.Contains(countryCodes, "us") {
		t.Fatalf("regions = %v", countryCodes)
	}
	presetsIpv4, _ := connect.RegionalControlDohUrls("cn")
	if !slices.Equal(IngressDohUrls("cn").getAll(), presetsIpv4) {
		t.Fatalf("cn doh = %v", IngressDohUrls("cn").getAll())
	}
	if IngressPlainDnsServers("cn").Len() == 0 {
		t.Fatal("cn has no plain servers")
	}
	if IngressDohUrls("").Len() != 0 || IngressPlainDnsServers("").Len() != 0 {
		t.Fatal("undisclosed selects resolvers beyond the defaults")
	}
}
