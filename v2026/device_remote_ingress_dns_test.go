package sdk

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// The parts of the ingress dns hand-off the tunnel tests can't see
// (device_remote_ingress_dns.go): what the tunnel's copy of the space stores,
// the queue while the tunnel is down and across syncs, the stale snapshot
// guard, the callback's lifetime, and the hosted guard.

// The bytes of every file under `dir`, by path.
func ingressDnsTestPathContents(t *testing.T, dir string) map[string]string {
	t.Helper()
	pathContents := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			pathContents[path] = string(content)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return pathContents
}

// The tunnel's copy of the space takes the app's values in place, read by the
// space's own rules, and stores nothing: on ios the tunnel's network space
// manager writes the app group document the app writes too, so storing its
// whole copy would put every other value back to what the tunnel started with.
func TestTunnelSpaceTakesTheIngressDnsInPlaceAndStoresNothing(t *testing.T) {
	storagePath := t.TempDir()
	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	t.Cleanup(networkSpaceManager.Close)
	key := NewNetworkSpaceKey("space.example", "main")
	tunnelSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	strategy := tunnelSpace.clientStrategy
	storedPathContents := ingressDnsTestPathContents(t, storagePath)
	if len(storedPathContents) == 0 {
		t.Fatal("the manager stored nothing to compare against")
	}

	// as an app could not have saved them: an upper-case region, an http url,
	// a host name, a repeat and blanks
	changed := tunnelSpace.applyIngressDns(&DeviceRemoteIngressDns{
		CountryCode: " CN ",
		ControlDohUrlsIpv4: []string{
			"https://192.0.2.1/dns-query", "http://192.0.2.9/dns-query", "https://dns.example/dns-query",
			"https://192.0.2.1/dns-query", " ",
		},
		ControlDohUrlsIpv6: []string{"https://[2001:db8::1]/dns-query"},
	})
	if !changed {
		t.Fatal("the values changed nothing")
	}
	if countryCode := tunnelSpace.GetIngressCountryCode(); countryCode != "cn" {
		t.Fatalf("region = %q, expected cn", countryCode)
	}
	if dohUrls := tunnelSpace.GetControlDohUrlsIpv4().getAll(); !slices.Equal(dohUrls, []string{"https://192.0.2.1/dns-query"}) {
		t.Fatalf("v4 servers = %v, expected the one valid url once", dohUrls)
	}
	if dohUrls := tunnelSpace.GetControlDohUrlsIpv6().getAll(); !slices.Equal(dohUrls, []string{"https://[2001:db8::1]/dns-query"}) {
		t.Fatalf("v6 servers = %v", dohUrls)
	}
	if countryCode := strategy.DohSettings().IngressCountryCode; countryCode != "cn" {
		t.Fatalf("the strategy's ingress country = %q", countryCode)
	}
	if networkSpaceManager.GetNetworkSpace(key) != tunnelSpace || tunnelSpace.clientStrategy != strategy {
		t.Fatal("the values replaced the space or its strategy")
	}
	if afterPathContents := ingressDnsTestPathContents(t, storagePath); !maps.Equal(storedPathContents, afterPathContents) {
		t.Fatal("taking the app's values stored the tunnel's copy of the space")
	}

	// the same values again are a no-op, and a region without a
	// recommendation is undisclosed
	ingressDns, _ := tunnelSpace.ingressDnsAndRevision()
	if tunnelSpace.applyIngressDns(ingressDns) {
		t.Fatal("the values the space has changed something")
	}
	tunnelSpace.applyIngressDns(&DeviceRemoteIngressDns{CountryCode: "zz"})
	if countryCode := tunnelSpace.GetIngressCountryCode(); countryCode != "" {
		t.Fatalf("an unknown region read as %q, expected undisclosed", countryCode)
	}
}

// While the tunnel is down the change is queued on the caller's goroutine, so
// the next device's first sync carries it as soon as the setter returns.
func TestDeviceRemoteQueuesTheIngressDnsWhileTheTunnelIsDown(t *testing.T) {
	appSpace, byJwt := newSavedSettingsTestSpace(t)
	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, appSpace, byJwt)

	appSpace.SetIngressCountryCode("cn")
	request := deviceRemote.takeSyncRequest()
	if !request.State.IngressDns.IsSet || request.State.IngressDns.Value.CountryCode != "cn" {
		t.Fatal("the next sync does not carry the app's region")
	}
}

// A snapshot older than the one queued is dropped: a slower caller can deliver
// it after a newer change, and the device must end on the newest.
func TestDeviceRemoteDropsAStaleIngressDnsSnapshot(t *testing.T) {
	appSpace, byJwt := newSavedSettingsTestSpace(t)
	deviceRemote := newTestDeviceRemoteWithNoServiceInSpace(t, appSpace, byJwt)
	_, revision := appSpace.ingressDnsAndRevision()

	deviceRemote.ingressDnsChangedInSpace(&DeviceRemoteIngressDns{CountryCode: "cn"}, revision+2)
	deviceRemote.ingressDnsChangedInSpace(&DeviceRemoteIngressDns{CountryCode: ""}, revision+1)
	request := deviceRemote.takeSyncRequest()
	if !request.State.IngressDns.IsSet || request.State.IngressDns.Value.CountryCode != "cn" {
		t.Fatal("the stale snapshot replaced the newer one")
	}
	if queuedRevision := deviceRemote.ingressDnsRevisionMonitor.Value(); queuedRevision != revision+2 {
		t.Fatalf("the hand-off loop watches revision %d, expected %d", queuedRevision, revision+2)
	}
}

// The values stay queued after a sync, so the next device, or this one after
// an rpc reconnect, gets them again; and closing the remote removes its
// callback from the app's space.
func TestDeviceRemoteKeepsTheIngressDnsAcrossSyncsAndUnsubscribesOnClose(t *testing.T) {
	appSpace, _, deviceRemote := newIngressDnsTestTunnel(t, nil)

	kept := func() bool {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()
		return deviceRemote.state.IngressDns.IsSet
	}()
	if !kept {
		t.Fatal("the sync dropped the app's ingress dns from the queue")
	}
	if callbacks := appSpace.ingressDnsChangeCallbacks.Get(); len(callbacks) != 1 {
		t.Fatalf("the app's space has %d ingress dns callbacks, expected the remote's one", len(callbacks))
	}

	if err := deviceRemote.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if callbacks := appSpace.ingressDnsChangeCallbacks.Get(); len(callbacks) != 0 {
		t.Fatalf("the closed remote left %d ingress dns callbacks", len(callbacks))
	}
	// a change after the close reaches nothing and breaks nothing
	appSpace.SetIngressCountryCode("cn")
}

// A remote to a hosted device neither queues nor registers, and a hosted
// device takes none: its space is the platform's.
func TestHostedDevicesTakeNoIngressDns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	appSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultDeviceRpcSettings()
	settings.Address = requireRemoteAddress(testing_freeHostPort())
	settings.DisableHostedIncompatible = true
	deviceRemote, err := newDeviceRemoteWithOverrides(
		appSpace, byJwt, NewId(), settings, connect.NewId(), testing_deviceRpcDialer(settings),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceRemote.Close)
	if deviceRemote.takeSyncRequest().State.IngressDns.IsSet {
		t.Fatal("a remote to a hosted device queued the app's ingress dns")
	}
	if callbacks := appSpace.ingressDnsChangeCallbacks.Get(); len(callbacks) != 0 {
		t.Fatal("a remote to a hosted device registered for the app's ingress dns")
	}

	tunnelSpace, _, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deviceLocalSettings := DefaultDeviceLocalSettings()
	deviceLocalSettings.Verbose = false
	deviceLocalSettings.HostedIncompatible = true
	deviceLocal, err := newDeviceLocalWithOverrides(
		tunnelSpace, byJwt, "", "", "", NewId(), deviceLocalSettings, connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceLocal.Close)
	deviceLocal.applyIngressDns(&DeviceRemoteIngressDns{CountryCode: "cn"})
	if countryCode := tunnelSpace.GetIngressCountryCode(); countryCode != "" {
		t.Fatalf("a hosted device took region %q", countryCode)
	}
}
