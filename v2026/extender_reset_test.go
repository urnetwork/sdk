package sdk

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// The reset of the account screen's extender section (EXTENDER.md E7): a
// space's extender state back to a fresh install's, in this process and in
// every other process that holds the space -- by the device rpc at once, or by
// the next import of the space.

// Records every network client a space builds and every hello it reads, so a
// test sees a client relearn without waiting on time.
type testExtenderResetNetwork struct {
	stateLock         sync.Mutex
	clientManualHosts [][]string
	helloCount        int
	helloCountMonitor *connect.Monitor
}

func (self *testExtenderResetNetwork) recordClient(manualHosts []string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clientManualHosts = append(self.clientManualHosts, slices.Clone(manualHosts))
}

func (self *testExtenderResetNetwork) recordHello() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.helloCount += 1
	self.helloCountMonitor.NotifyAll()
}

func (self *testExtenderResetNetwork) clientCount() (int, []string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.clientManualHosts) == 0 {
		return 0, nil
	}
	return len(self.clientManualHosts), slices.Clone(self.clientManualHosts[len(self.clientManualHosts)-1])
}

func (self *testExtenderResetNetwork) helloCountValue() (int, chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.helloCount, self.helloCountMonitor.NotifyChannel()
}

// Waits until hello has been read more than `count` times.
func (self *testExtenderResetNetwork) waitHelloAfter(t *testing.T, count int) {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		helloCount, update := self.helloCountValue()
		if count < helloCount {
			return
		}
		select {
		case <-update:
		case <-timeout:
			t.Fatalf("hello read %d times, expected a read after the first %d", helloCount, count)
		}
	}
}

// Runs every space's network client in process, as the other extender tests
// do, and records it.
func testEnableExtenderResetNetwork(t *testing.T) *testExtenderResetNetwork {
	t.Helper()
	network := &testExtenderResetNetwork{
		helloCountMonitor: connect.NewMonitor(),
	}
	extenderNetworkClientEnabled = true
	extenderNetworkClientConfigure = func(settings *connect.ExtenderNetworkClientSettings) {
		testConfigureInProcessExtenderNetworkClient(settings)
		network.recordClient(settings.ManualHosts)
		settings.Hello = func(ctx context.Context) (*connect.ExtenderHelloResult, error) {
			network.recordHello()
			return &connect.ExtenderHelloResult{}, nil
		}
	}
	t.Cleanup(func() {
		extenderNetworkClientEnabled = false
		extenderNetworkClientConfigure = nil
	})
	return network
}

// One manager over its own storage, the way a process keeps one, and its
// space for the test host with the values `configure` sets.
func testExtenderResetSpace(
	t *testing.T,
	storagePath string,
	configure func(values *NetworkSpaceValues),
) (*NetworkSpaceManager, *NetworkSpace) {
	t.Helper()
	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	t.Cleanup(networkSpaceManager.Close)
	if configure == nil {
		configure = func(values *NetworkSpaceValues) {}
	}
	networkSpace := networkSpaceManager.updateNetworkSpace(NewNetworkSpaceKey("space.example", "main"), configure)
	if networkSpace == nil || networkSpace.extenderDirectory == nil {
		t.Fatal("the space has no extender directory")
	}
	return networkSpaceManager, networkSpace
}

// Every extender value a user can set, so a reset has something to clear.
func testExtenderResetUserValues(values *NetworkSpaceValues) {
	values.ExtenderHosts = []string{"192.0.2.1"}
	values.ExtenderDnsName = "x.example"
	values.GossipUrl = "wss://g.example"
	values.ExtenderRootPublicKeys = []string{testExtenderRootPublicKeyHex}
	values.NetExtender = &NetExtender{Ip: "192.0.2.9", Secret: "test-secret"}
}

// Puts learned state in a space's directory: a dns bootstrap address, an
// imported one with a failure held against it, a verified record from the
// feed, the continent hint and the operator's country.
func testExtenderResetLearn(t *testing.T, networkSpace *NetworkSpace) {
	t.Helper()
	directory := networkSpace.extenderDirectory
	directory.AddBootstrap(netip.MustParseAddr("198.51.100.1"), connect.ExtenderSourceDns)
	directory.AddBootstrap(netip.MustParseAddr("198.51.100.2"), connect.ExtenderSourceImport)
	directory.RecordFailure(netip.MustParseAddr("198.51.100.2"), connect.ExtenderConnectModeTcpTls)
	if _, err := directory.ApplyRecord(testExtenderStatusRecord(t, networkSpace, "198.51.100.3"), connect.ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	directory.SetContinentHint("EU")
	directory.SetCountryHint("de")
	if directory.Snapshot().KnownCount == 0 {
		t.Fatal("the test learned nothing")
	}
}

// The addresses a directory knows, by ip.
func testExtenderResetIps(networkSpace *NetworkSpace) []string {
	ips := []string{}
	for _, entry := range networkSpace.extenderDirectory.Snapshot().Entries {
		ips = append(ips, entry.Ip.String())
	}
	return ips
}

// Fails unless the space's directory is a fresh install's.
func testExtenderResetRequireFresh(t *testing.T, what string, networkSpace *NetworkSpace) {
	t.Helper()
	directory := networkSpace.extenderDirectory
	if ips := testExtenderResetIps(networkSpace); len(ips) != 0 {
		t.Fatalf("%s: the directory still knows %v", what, ips)
	}
	if continentHint := directory.ContinentHint(); continentHint != "" {
		t.Fatalf("%s: continent hint = %q", what, continentHint)
	}
	if connect.NetworkCountryCode() == "" {
		if countryCode := directory.SpoofCountryCode(); countryCode != "" {
			t.Fatalf("%s: spoof country = %q", what, countryCode)
		}
	}
}

// The directory envelope a space's storage holds, decoded.
func testExtenderResetStoredDirectory(t *testing.T, networkSpace *NetworkSpace) map[string]json.RawMessage {
	t.Helper()
	stateBytes, err := os.ReadFile(filepath.Join(networkSpace.asyncLocalState.GetLocalState().localStorageDir, extenderStoreFileName))
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]json.RawMessage{}
	if err := json.Unmarshal(stateBytes, &state); err != nil {
		t.Fatal(err)
	}
	return state
}

// A reset clears everything learned and everything a user added, persists the
// cleared values with the reset's id through the manager, writes the fresh
// directory, keeps the space and everything bound to it, and starts a new
// network client that relearns as on a first run.
func TestNetworkSpaceResetExtendersReturnsTheFreshState(t *testing.T) {
	network := testEnableExtenderResetNetwork(t)
	storagePath := t.TempDir()
	networkSpaceManager, networkSpace := testExtenderResetSpace(t, storagePath, testExtenderResetUserValues)
	testExtenderResetLearn(t, networkSpace)
	previousNetworkClient := networkSpace.getExtenderNetworkClient()
	if previousNetworkClient == nil {
		t.Fatal("the space ran no network client")
	}
	network.waitHelloAfter(t, 0)
	helloCount, _ := network.helloCountValue()
	clientCount, _ := network.clientCount()

	resetId := networkSpace.ResetExtenders()

	if _, err := connect.ParseId(resetId); err != nil {
		t.Fatalf("reset id %q: %v", resetId, err)
	}
	if networkSpaceManager.GetNetworkSpace(networkSpace.GetKey()) != networkSpace {
		t.Fatal("the reset replaced the space")
	}
	connect.AssertEqual(t, networkSpace.GetExtenderResetId(), resetId)
	connect.AssertEqual(t, networkSpace.GetExtenderHosts().Len(), 0)
	connect.AssertEqual(t, networkSpace.GetExtenderDnsName(), "extender.space.example")
	connect.AssertEqual(t, networkSpace.GetGossipUrl(), "wss://gossip.space.example")
	// the bundled table names no key for this host
	connect.AssertEqual(t, networkSpace.GetExtenderRootPublicKeys().Len(), 0)
	if netExtender := networkSpace.GetNetExtender(); netExtender != nil && netExtender.Ip != "" {
		t.Fatalf("the private extender %q survived the reset", netExtender.Ip)
	}
	if customExtenders := networkSpace.clientStrategy.CustomExtenders(); len(customExtenders) != 0 {
		t.Fatalf("the strategy kept the custom extenders %v", customExtenders)
	}
	testExtenderResetRequireFresh(t, "after the reset", networkSpace)
	if networkSpace.extenderDirectory.RootKeys().Len() != 0 {
		t.Fatal("the directory kept root keys the bundled table does not name")
	}

	// a new client, configured with no manual host, reads hello again
	nextNetworkClient := networkSpace.getExtenderNetworkClient()
	if nextNetworkClient == nil || nextNetworkClient == previousNetworkClient {
		t.Fatal("the reset did not start a new network client")
	}
	if count, manualHosts := network.clientCount(); count <= clientCount || len(manualHosts) != 0 {
		t.Fatalf("clients = %d (was %d), the last with manual hosts %v", count, clientCount, manualHosts)
	}
	network.waitHelloAfter(t, helloCount)

	// what reaches the disk: the fresh directory, the reset it applied, and
	// the cleared values with its id
	stored := testExtenderResetStoredDirectory(t, networkSpace)
	if string(stored["addresses"]) != "[]" || string(stored["records"]) != "[]" || len(stored["country_hint"]) != 0 {
		t.Fatalf("the stored directory after the reset = %v", stored)
	}
	connect.AssertEqual(t, networkSpace.asyncLocalState.GetLocalState().getExtenderResetId(), resetId)
	reloadedManager := NewNetworkSpaceManager(storagePath)
	defer reloadedManager.Close()
	reloaded := reloadedManager.GetNetworkSpace(networkSpace.GetKey())
	if reloaded == nil {
		t.Fatal("the reloaded manager lost the space")
	}
	connect.AssertEqual(t, reloaded.GetExtenderResetId(), resetId)
	connect.AssertEqual(t, reloaded.GetExtenderHosts().Len(), 0)
	connect.AssertEqual(t, reloaded.GetExtenderDnsName(), "extender.space.example")
}

// A reset made in the app process reaches the space a tunnel process imports:
// the import resets the directory there once, the values it brings replace the
// user's, and another import of the same reset -- or an older one -- resets
// nothing.
func TestNetworkSpaceImportAppliesAResetOnce(t *testing.T) {
	testEnableExtenderResetNetwork(t)
	_, appSpace := testExtenderResetSpace(t, t.TempDir(), testExtenderResetUserValues)
	tunnelManager := NewNetworkSpaceManager(t.TempDir())
	t.Cleanup(tunnelManager.Close)

	importAppSpace := func() *NetworkSpace {
		t.Helper()
		appSpaceJson, err := appSpace.ToJson()
		if err != nil {
			t.Fatal(err)
		}
		tunnelSpace, err := tunnelManager.ImportNetworkSpaceFromJson(appSpaceJson)
		if err != nil {
			t.Fatal(err)
		}
		return tunnelSpace
	}
	tunnelSpace := importAppSpace()
	testExtenderResetLearn(t, tunnelSpace)
	olderResetId := connect.NewId().String()

	resetId := appSpace.ResetExtenders()
	if tunnelManager.GetNetworkSpace(tunnelSpace.GetKey()) != tunnelSpace {
		t.Fatal("the test expected the tunnel space to be applied in place")
	}
	if importAppSpace() != tunnelSpace {
		t.Fatal("the import replaced a space whose values changed in place only")
	}
	testExtenderResetRequireFresh(t, "after the import", tunnelSpace)
	connect.AssertEqual(t, tunnelSpace.GetExtenderResetId(), resetId)
	connect.AssertEqual(t, tunnelSpace.GetExtenderHosts().Len(), 0)
	connect.AssertEqual(t, tunnelSpace.asyncLocalState.GetLocalState().getExtenderResetId(), resetId)

	// learned since, and kept by every later arrival of this reset or an older
	// one: the user adds a host after the reset, and the space that brings it
	// carries the same reset
	tunnelSpace.extenderDirectory.AddBootstrap(netip.MustParseAddr("198.51.100.10"), connect.ExtenderSourceDns)
	appSpace.updateInPlaceValues(func(values *NetworkSpaceValues) {
		values.ExtenderHosts = []string{"192.0.2.2"}
	})
	if importAppSpace() != tunnelSpace {
		t.Fatal("the import replaced a space whose values changed in place only")
	}
	connect.AssertEqual(t, tunnelSpace.GetExtenderHosts().Len(), 1)
	if tunnelSpace.ApplyExtenderReset(resetId) || tunnelSpace.ApplyExtenderReset(olderResetId) {
		t.Fatal("a reset the tunnel space had applied, or an older one, applied again")
	}
	if ips := testExtenderResetIps(tunnelSpace); !slices.Contains(ips, "198.51.100.10") {
		t.Fatalf("a later arrival of the reset reset the directory again: %v", ips)
	}
}

// A space built with values that bring a reset its storage has not applied --
// a process that starts from them -- resets its directory before anything
// reads it, and records the reset, so the next start does not reset again.
func TestNetworkSpaceConstructionAppliesAStoredReset(t *testing.T) {
	storagePath := t.TempDir()
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	t.Cleanup(networkSpaceManager.Close)
	learning := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	testExtenderResetLearn(t, learning)
	envStoragePath := networkSpaceManager.envStoragePath(key)
	// the directory's close writes what it learned
	networkSpaceManager.Close()

	resetId := connect.NewId().String()
	values := NetworkSpaceValues{
		ExtenderResetId: resetId,
	}
	networkSpace := newNetworkSpace(context.Background(), *key, values, envStoragePath)
	testExtenderResetRequireFresh(t, "a space built from the reset", networkSpace)
	connect.AssertEqual(t, networkSpace.asyncLocalState.GetLocalState().getExtenderResetId(), resetId)
	networkSpace.extenderDirectory.AddBootstrap(netip.MustParseAddr("198.51.100.11"), connect.ExtenderSourceDns)
	networkSpace.close()

	restarted := newNetworkSpace(context.Background(), *key, values, envStoragePath)
	defer restarted.close()
	if ips := testExtenderResetIps(restarted); !slices.Equal(ips, []string{"198.51.100.11"}) {
		t.Fatalf("a restart from the same values reset again: the directory = %v", ips)
	}
}

// A reset that comes with values a running space cannot take in place is
// applied to the space being replaced before its replacement loads the store
// they share: the old directory is reset, so neither its learning since nor
// its close can put back what the reset cleared.
func TestNetworkSpaceRebuildAppliesTheResetToTheReplacedSpaceFirst(t *testing.T) {
	_, replaced := testExtenderResetSpace(t, t.TempDir(), testExtenderResetUserValues)
	networkSpaceManager := replaced.getNetworkSpaceManager()
	testExtenderResetLearn(t, replaced)

	resetId := connect.NewId().String()
	networkSpace := networkSpaceManager.updateNetworkSpace(replaced.GetKey(), func(values *NetworkSpaceValues) {
		values.LinkHostName = "link.example"
		resetExtenderValues(values, resetId)
	})
	if networkSpace == replaced {
		t.Fatal("the test expected a rebuild")
	}
	testExtenderResetRequireFresh(t, "the replaced space", replaced)
	testExtenderResetRequireFresh(t, "the replacement", networkSpace)
	connect.AssertEqual(t, networkSpace.asyncLocalState.GetLocalState().getExtenderResetId(), resetId)
	if stored := testExtenderResetStoredDirectory(t, networkSpace); string(stored["addresses"]) != "[]" {
		t.Fatalf("the stored directory after the rebuild = %s", stored["addresses"])
	}
}

// A remote device resets the space of its own process and hands the reset to
// the device process at once; while that process cannot be reached the reset
// is queued and applied at the next sync.
func TestDeviceRemoteResetExtendersReachesTheDeviceProcess(t *testing.T) {
	_, appSpace := testExtenderResetSpace(t, t.TempDir(), testExtenderResetUserValues)
	_, tunnelSpace := testExtenderResetSpace(t, t.TempDir(), testExtenderResetUserValues)

	clientId := connect.NewId()
	instanceId := NewId()
	settings := defaultDeviceRpcSettings()
	deviceRemote, err := newDeviceRemoteWithOverrides(
		appSpace, "", instanceId, settings, clientId, testing_deviceRpcDialer(settings),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceRemote.Close)
	bringUp := func() *DeviceLocal {
		t.Helper()
		localSettings := testExtenderStatusDeviceSettings()
		localSettings.EnableRpc = true
		deviceLocal, err := newDeviceLocalWithOverrides(
			tunnelSpace, "", "", "", "", instanceId, localSettings, clientId,
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(deviceLocal.Close)
		deviceRemote.Sync()
		if !deviceRemote.waitForSync(30 * time.Second) {
			t.Fatal("the device remote did not sync after the device came up")
		}
		return deviceLocal
	}
	queuedReset := func() deviceRemoteValue[string] {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()
		return deviceRemote.state.ExtenderReset
	}

	// unreachable: the app's space resets at once and the reset is queued
	testExtenderResetLearn(t, appSpace)
	testExtenderResetLearn(t, tunnelSpace)
	deviceRemote.ResetExtenders()
	testExtenderResetRequireFresh(t, "the app space", appSpace)
	queued := queuedReset()
	if !queued.IsSet || queued.Value != appSpace.GetExtenderResetId() {
		t.Fatalf("queued = %+v, expected the app's reset %s", queued, appSpace.GetExtenderResetId())
	}
	if len(testExtenderResetIps(tunnelSpace)) == 0 {
		t.Fatal("the tunnel space reset before the device process could be told")
	}

	bringUp()
	testExtenderResetRequireFresh(t, "the tunnel space after the sync", tunnelSpace)
	connect.AssertEqual(t, tunnelSpace.GetExtenderResetId(), appSpace.GetExtenderResetId())
	connect.AssertEqual(t, tunnelSpace.GetExtenderHosts().Len(), 0)
	if queued := queuedReset(); queued.IsSet {
		t.Fatalf("queued = %+v after the sync, expected nothing left to replay", queued)
	}

	// reachable: the device process applies it inside the call
	testExtenderResetLearn(t, tunnelSpace)
	deviceRemote.ResetExtenders()
	testExtenderResetRequireFresh(t, "the tunnel space after a connected reset", tunnelSpace)
	connect.AssertEqual(t, tunnelSpace.GetExtenderResetId(), appSpace.GetExtenderResetId())
	if queued := queuedReset(); queued.IsSet {
		t.Fatalf("queued = %+v after a reset the device took", queued)
	}
}

// A hosted device's space is the proxy host's, shared by unrelated customers:
// neither the device nor a remote of it resets anything, and nothing is queued.
func TestHostedDeviceNeverResetsExtenders(t *testing.T) {
	_, networkSpace := testExtenderResetSpace(t, t.TempDir(), nil)
	testExtenderResetLearn(t, networkSpace)
	ips := testExtenderResetIps(networkSpace)

	localSettings := testExtenderStatusDeviceSettings()
	localSettings.HostedIncompatible = true
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), localSettings, connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceLocal.Close)
	deviceLocal.ResetExtenders()
	deviceLocal.applyExtenderReset(connect.NewId().String())

	settings := defaultDeviceRpcSettings()
	settings.DisableHostedIncompatible = true
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace, "", NewId(), settings, connect.NewId(), testing_deviceRpcDialer(settings),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceRemote.Close)
	deviceRemote.ResetExtenders()

	if after := testExtenderResetIps(networkSpace); !slices.Equal(after, ips) {
		t.Fatalf("a hosted device reset the space: %v, was %v", after, ips)
	}
	connect.AssertEqual(t, networkSpace.GetExtenderResetId(), "")
	deviceRemote.stateLock.Lock()
	queued := deviceRemote.state.ExtenderReset
	deviceRemote.stateLock.Unlock()
	if queued.IsSet {
		t.Fatalf("a hosted remote queued the reset %q", queued.Value)
	}
}

// The account screen's action answers the settings the reset leaves, every
// one the default, and the directory behind them is fresh.
func TestExtenderViewControllerResetExtenders(t *testing.T) {
	vc, networkSpace, networkSpaceManager := testExtenderViewController(t)
	hosts := NewStringList()
	hosts.Add("192.0.2.1")
	vc.SetSettings("x.example", "wss://g.example", hosts)
	testExtenderResetLearn(t, networkSpace)

	settings := vc.ResetExtenders()
	connect.AssertEqual(t, settings.DnsName, "extender.space.example")
	connect.AssertEqual(t, settings.DnsNameDefault, true)
	connect.AssertEqual(t, settings.GossipUrl, "wss://gossip.space.example")
	connect.AssertEqual(t, settings.GossipUrlDefault, true)
	connect.AssertEqual(t, settings.Hosts.Len(), 0)
	connect.AssertEqual(t, settings.RootPublicKeysDefault, true)
	testExtenderResetRequireFresh(t, "after the controller's reset", networkSpace)
	if networkSpaceManager.GetNetworkSpace(networkSpace.GetKey()) != networkSpace {
		t.Fatal("the reset replaced the space the controller holds")
	}
	if vc.GetStatus() == nil {
		t.Fatal("the controller has no status after the reset")
	}
}

// Reset ids are ordered by when they were minted, so a reset newer than the
// applied one applies and an older or equal one does not; nothing is no reset.
func TestExtenderResetIdOrder(t *testing.T) {
	older := connect.NewId().String()
	newer := connect.NewId().String()
	cases := []struct {
		resetId        string
		appliedResetId string
		expect         bool
	}{
		{resetId: newer, appliedResetId: "", expect: true},
		{resetId: newer, appliedResetId: older, expect: true},
		{resetId: older, appliedResetId: newer, expect: false},
		{resetId: newer, appliedResetId: newer, expect: false},
		{resetId: "", appliedResetId: older, expect: false},
		{resetId: "not an id", appliedResetId: "", expect: false},
		{resetId: newer, appliedResetId: "not an id", expect: true},
	}
	for _, c := range cases {
		if newerReset := extenderResetIdNewer(c.resetId, c.appliedResetId); newerReset != c.expect {
			t.Errorf("newer(%q, %q) = %t, expected %t", c.resetId, c.appliedResetId, newerReset, c.expect)
		}
	}
}

// A reset pressed in this process applies whatever the ids say: with a clock
// set back since the last reset, the new id is older than the applied one,
// and the press must not be skipped.
func TestNetworkSpaceResetExtendersAppliesAfterAClockSetBack(t *testing.T) {
	_, networkSpace := testExtenderResetSpace(t, t.TempDir(), testExtenderResetUserValues)
	// a reset applied from a clock far ahead of this one
	futureResetId, err := connect.ParseId("ffffffff-ffff-ffff-ffff-ffffffffffff")
	if err != nil {
		t.Fatal(err)
	}
	func() {
		networkSpace.stateLock.Lock()
		defer networkSpace.stateLock.Unlock()
		networkSpace.appliedExtenderResetId = futureResetId.String()
	}()
	testExtenderResetLearn(t, networkSpace)

	resetId := networkSpace.ResetExtenders()
	testExtenderResetRequireFresh(t, "a reset minted behind the applied one", networkSpace)
	connect.AssertEqual(t, networkSpace.GetExtenderResetId(), resetId)
	connect.AssertEqual(t, networkSpace.GetExtenderHosts().Len(), 0)
}
