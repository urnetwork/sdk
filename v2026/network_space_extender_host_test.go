package sdk

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

// Spaces on an extender host (NewNetworkSpaceWithUrlsOnExtenderHost): one
// process that runs many providers on one host, each through its own proxy --
// the sn miner's slots -- runs one extender stack, the host's, and the other
// spaces read it (connect#227).

// Counts the extender directories, the root of every stack, that spaces build
// until the test ends. The suite runs no test in parallel, so every build
// counted is this test's.
func testCountExtenderDirectoryBuilds(t *testing.T) *atomic.Int64 {
	t.Helper()
	count := &atomic.Int64{}
	hook := func() {
		count.Add(1)
	}
	testingExtenderDirectoryBuilt.Store(&hook)
	t.Cleanup(func() {
		testingExtenderDirectoryBuilt.Store(nil)
	})
	return count
}

// A url-only space's settings that resolve and dial in the process.
func testExtenderHostStrategySettings() *connect.ClientStrategySettings {
	settings := testOfflineClientStrategySettings()
	settings.Log = connect.NewNoopLogger()
	return settings
}

// Whether a space runs any part of an extender stack of its own.
func testRunsAnExtenderStack(networkSpace *NetworkSpace) bool {
	return networkSpace.extenderDirectory != nil ||
		networkSpace.getExtenderNetworkClient() != nil ||
		networkSpace.getExtenderNode() != nil ||
		networkSpace.extenderRelease.fetcher.Load() != nil ||
		networkSpace.extenderRelease.blockReporter.Load() != nil ||
		networkSpace.extenderCamouflageReport.reporter.Load() != nil ||
		networkSpace.getExtenderSeed() != nil ||
		networkSpace.extenderWebRtcDial != nil
}

// Of a host and the spaces built on it, the host alone runs a stack: one
// directory, network client, node, release fetch, camouflage report, seed
// bootstrap, webrtc dial side and status watch for all of them. Each space on
// the host reads the host's directory through its strategy, read only, and
// closing one leaves the host's stack running.
func TestNetworkSpacesOnAnExtenderHostRunOneStack(t *testing.T) {
	testEnableUrlSpaceExtenderNetwork(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	directoryBuilds := testCountExtenderDirectoryBuilds(t)

	host := NewNetworkSpaceWithUrls(ctx, testUrlSpaceApiUrl, testUrlSpacePlatformUrl, testExtenderHostStrategySettings())
	defer host.close()
	if host.extenderDirectory == nil || host.getExtenderNetworkClient() == nil || host.getExtenderNode() == nil ||
		host.extenderRelease.fetcher.Load() == nil || host.extenderCamouflageReport.reporter.Load() == nil ||
		host.getExtenderSeed() == nil || host.extenderWebRtcDial == nil {
		t.Fatal("the host does not run the whole stack a url-only space runs")
	}
	spaces := []*NetworkSpace{}
	for range 3 {
		networkSpace := NewNetworkSpaceWithUrlsOnExtenderHost(ctx, testUrlSpaceApiUrl, testUrlSpacePlatformUrl, testExtenderHostStrategySettings(), host)
		defer networkSpace.close()
		spaces = append(spaces, networkSpace)
	}

	if count := directoryBuilds.Load(); count != 1 {
		t.Fatalf("a host and %d spaces on it built %d extender stacks, expected one", len(spaces), count)
	}
	if host.clientStrategySettings.ExtenderDirectoryReadOnly {
		t.Fatal("the host only reads its own directory")
	}
	for _, networkSpace := range spaces {
		if !networkSpace.readsExtenderHost() || networkSpace.extenderHost != host {
			t.Fatal("a space built on the host does not read it")
		}
		if testRunsAnExtenderStack(networkSpace) {
			t.Fatal("a space on the host runs a stack part of its own")
		}
		if networkSpace.clientStrategy.ExtenderDirectory() != host.extenderDirectory {
			t.Fatal("a space on the host draws its extenders from another directory")
		}
		if !networkSpace.clientStrategySettings.ExtenderDirectoryReadOnly {
			t.Fatal("a space on the host writes the outcomes of its dials into the host's directory")
		}
		if networkSpace.clientStrategySettings.ConnectSettings.WebRtcExtenderCarrier != nil {
			t.Fatal("a space on the host has a webrtc carrier of its own")
		}
	}

	spaces[0].close()
	if host.getExtenderNetworkClient() == nil || host.getExtenderNode() == nil {
		t.Fatal("closing a space on the host took the host's stack down")
	}
}

// A space on a host has no extender identity: it runs no node and no role,
// so it makes none, takes none an embedder offers, and leaves the host's alone.
func TestNetworkSpaceOnAnExtenderHostPresentsNoExtenderIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	host := NewNetworkSpaceWithUrls(ctx, testUrlSpaceApiUrl, testUrlSpacePlatformUrl, testExtenderHostStrategySettings())
	defer host.close()
	hostKeySeed := host.extenderIdentityKeySeed()
	if len(hostKeySeed) == 0 {
		t.Fatal("the host has no extender identity")
	}
	networkSpace := NewNetworkSpaceWithUrlsOnExtenderHost(ctx, testUrlSpaceApiUrl, testUrlSpacePlatformUrl, testExtenderHostStrategySettings(), host)
	defer networkSpace.close()

	if seed := networkSpace.extenderIdentityKeySeed(); seed != nil {
		t.Fatal("a space on the host made an extender identity")
	}
	offeredKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	networkSpace.setExtenderKeySeed(offeredKeySeed)
	networkSpace.setExtenderMeasurementKeyFile("/synthetic/measurement.key")
	if seed := networkSpace.extenderIdentityKeySeed(); seed != nil {
		t.Fatal("a space on the host took an embedder's extender identity")
	}
	if file := networkSpace.suppliedExtenderMeasurementKeyFile(); file != "" {
		t.Fatal("a space on the host took an embedder's measurement key file")
	}
	if !slices.Equal(host.extenderIdentityKeySeed(), hostKeySeed) {
		t.Fatal("a space on the host changed the host's identity")
	}
}

// A space runs its own stack, as NewNetworkSpaceWithUrls builds it, when its
// host cannot host it: no host, a closed one, or one for another extender
// network. A space built on a space that is itself on a host reads that host.
func TestNetworkSpaceOnAnExtenderHostRunsItsOwnWithoutAHost(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	host := NewNetworkSpaceWithUrls(ctx, testUrlSpaceApiUrl, testUrlSpacePlatformUrl, testExtenderHostStrategySettings())
	defer host.close()

	onHost := NewNetworkSpaceWithUrlsOnExtenderHost(ctx, testUrlSpaceApiUrl, testUrlSpacePlatformUrl, testExtenderHostStrategySettings(), host)
	defer onHost.close()
	chained := NewNetworkSpaceWithUrlsOnExtenderHost(ctx, testUrlSpaceApiUrl, testUrlSpacePlatformUrl, testExtenderHostStrategySettings(), onHost)
	defer chained.close()
	if chained.extenderHost != host || testRunsAnExtenderStack(chained) {
		t.Fatal("a space built on a space on a host does not read that host")
	}

	otherNetwork := NewNetworkSpaceWithUrlsOnExtenderHost(ctx, "https://api.other.example", "wss://platform.other.example", testExtenderHostStrategySettings(), host)
	defer otherNetwork.close()
	closedHost := NewNetworkSpaceWithUrls(ctx, testUrlSpaceApiUrl, testUrlSpacePlatformUrl, testExtenderHostStrategySettings())
	closedHost.close()
	afterClose := NewNetworkSpaceWithUrlsOnExtenderHost(ctx, testUrlSpaceApiUrl, testUrlSpacePlatformUrl, testExtenderHostStrategySettings(), closedHost)
	defer afterClose.close()
	noHost := NewNetworkSpaceWithUrlsOnExtenderHost(ctx, testUrlSpaceApiUrl, testUrlSpacePlatformUrl, testExtenderHostStrategySettings(), nil)
	defer noHost.close()
	for name, networkSpace := range map[string]*NetworkSpace{"another network": otherNetwork, "a closed host": afterClose, "no host": noHost} {
		if networkSpace.readsExtenderHost() || networkSpace.extenderDirectory == nil {
			t.Errorf("with %s the space runs no stack of its own", name)
		}
		if networkSpace.clientStrategySettings.ExtenderDirectoryReadOnly {
			t.Errorf("with %s the space only reads its own directory", name)
		}
	}
}

// A device whose space is on a host attests no probes: its space probes
// nothing, and the host's own device attests the host's probes. A device in
// the host attests as every provider does.
func TestDeviceOnAnExtenderHostAttestsNoProbes(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	host := NewNetworkSpaceWithUrls(ctx, testUrlSpaceApiUrl, testUrlSpacePlatformUrl, testExtenderHostStrategySettings())
	defer host.close()
	onHost := NewNetworkSpaceWithUrlsOnExtenderHost(ctx, testUrlSpaceApiUrl, testUrlSpacePlatformUrl, testExtenderHostStrategySettings(), host)
	defer onHost.close()

	attests := func(networkSpace *NetworkSpace) bool {
		settings := testExtenderStatusDeviceSettings()
		settings.AllowProvider = true
		settings.ProvideExtenderEnabled = false
		device, err := newDeviceLocalWithOverrides(networkSpace, "", "", "", "", NewId(), settings, connect.NewId())
		if err != nil {
			t.Fatal(err)
		}
		defer device.Close()
		provider := func() *deviceLocalProvider {
			device.stateLock.Lock()
			defer device.stateLock.Unlock()
			return device.provider
		}()
		if provider == nil {
			t.Fatal("the device runs no provider")
		}
		provider.stateLock.Lock()
		defer provider.stateLock.Unlock()
		return provider.probeAttestorEnabled
	}
	if !attests(host) {
		t.Fatal("a provider in the host attests no probes")
	}
	if attests(onHost) {
		t.Fatal("a provider in a space on the host attests probes its space never makes")
	}
}
