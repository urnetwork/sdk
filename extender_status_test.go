package sdk

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The sdk half of the extender surface (EXTENDER.md D5, F1, F2).

// The derived extender dns name and gossip url follow the env prefix rule and
// the migration host, exactly as ServiceUrl does (F1).
func TestExtenderNetworkSpaceValueDefaults(t *testing.T) {
	cases := []struct {
		hostName          string
		envName           string
		migrationHostName string
		envSecret         string
		values            NetworkSpaceValues
		expectDnsName     string
		expectGossipUrl   string
	}{
		{
			hostName:        "space.example",
			envName:         "main",
			expectDnsName:   "extender.space.example",
			expectGossipUrl: "wss://gossip.space.example",
		},
		{
			hostName:        "space.example",
			envName:         "",
			expectDnsName:   "extender.space.example",
			expectGossipUrl: "wss://gossip.space.example",
		},
		{
			hostName:        "space.example",
			envName:         "g2",
			expectDnsName:   "g2-extender.space.example",
			expectGossipUrl: "wss://g2-gossip.space.example",
		},
		{
			hostName:          "old.example",
			envName:           "main",
			migrationHostName: "new.example",
			expectDnsName:     "extender.new.example",
			expectGossipUrl:   "wss://gossip.new.example",
		},
		{
			// the env secret rides the api and platform urls but never the
			// gossip url: it becomes a multiaddr, which carries no path (F1)
			hostName:        "space.example",
			envName:         "g2",
			envSecret:       "sekret",
			expectDnsName:   "g2-extender.space.example",
			expectGossipUrl: "wss://g2-gossip.space.example",
		},
		{
			hostName:        "space.example",
			envName:         "main",
			values:          NetworkSpaceValues{ExtenderDnsName: "x.example", GossipUrl: "wss://g.example/"},
			expectDnsName:   "x.example",
			expectGossipUrl: "wss://g.example",
		},
	}
	for _, c := range cases {
		key := NewNetworkSpaceKey(c.hostName, c.envName)
		values := c.values
		values.MigrationHostName = c.migrationHostName
		values.EnvSecret = c.envSecret
		if extenderDnsName := ExtenderDnsName(key, &values); extenderDnsName != c.expectDnsName {
			t.Errorf("extender dns name = %q, expected %q", extenderDnsName, c.expectDnsName)
		}
		if gossipUrl := GossipUrl(key, &values); gossipUrl != c.expectGossipUrl {
			t.Errorf("gossip url = %q, expected %q", gossipUrl, c.expectGossipUrl)
		}
	}
}

// The gossip url never carries the env secret path, whatever the api url does
// (F1): the url becomes a multiaddr, which has no path, and the gossip service
// serves none.
func TestGossipUrlNeverCarriesTheEnvSecret(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	key := NewNetworkSpaceKey("space.example", "g2")
	values := NetworkSpaceValues{EnvSecret: "sekret"}
	networkSpace := newNetworkSpace(ctx, *key, values, "")
	t.Cleanup(networkSpace.close)

	if gossipUrl := networkSpace.GetGossipUrl(); gossipUrl != "wss://g2-gossip.space.example" {
		t.Fatalf("gossip url = %q, expected no env secret path", gossipUrl)
	}
	// the api url of the same space still carries it, so this is the gossip
	// url's own rule rather than a space without a secret
	if apiUrl := networkSpace.GetApiUrl(); apiUrl != "https://g2-api.space.example/sekret" {
		t.Fatalf("api url = %q, expected the env secret path", apiUrl)
	}
}

// The operator patterns a space's extender forwards to are the space host and
// one wildcard level under it, for the key host and the migration host (A5,
// G2). A spoof domain is never among them: it is on the whitelist for the
// reverse proxy, never as a destination.
func TestExtenderAllowedHostsCoverTheMigrationHost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	key := NewNetworkSpaceKey("old.example", "main")
	values := NetworkSpaceValues{MigrationHostName: "new.example"}
	networkSpace := newNetworkSpace(ctx, *key, values, "")
	t.Cleanup(networkSpace.close)

	expected := []string{"old.example", "*.old.example", "new.example", "*.new.example"}
	if allowedHosts := networkSpace.extenderAllowedHosts(); !slices.Equal(allowedHosts, expected) {
		t.Fatalf("allowed hosts = %v, expected %v", allowedHosts, expected)
	}

	plainKey := NewNetworkSpaceKey("space.example", "g2")
	plainSpace := newNetworkSpace(ctx, *plainKey, NetworkSpaceValues{}, "")
	t.Cleanup(plainSpace.close)
	// the env prefix names services, not the space host, so the patterns do
	// not carry it
	plainExpected := []string{"space.example", "*.space.example"}
	if allowedHosts := plainSpace.extenderAllowedHosts(); !slices.Equal(allowedHosts, plainExpected) {
		t.Fatalf("allowed hosts = %v, expected %v", allowedHosts, plainExpected)
	}
}

// The bundled root key table is the anchor when a space configures none, and
// it ships empty until operations fill it (F1, B4).
func TestExtenderRootPublicKeysFallBackToTheBundledTable(t *testing.T) {
	key := NewNetworkSpaceKey("space.example", "main")
	values := NetworkSpaceValues{
		ExtenderRootPublicKeys: []string{" aabb ", ""},
	}
	rootPublicKeys := ExtenderRootPublicKeys(key, &values)
	if len(rootPublicKeys) != 1 || rootPublicKeys[0] != "aabb" {
		t.Fatalf("root keys = %v, expected the configured one", rootPublicKeys)
	}

	empty := NetworkSpaceValues{}
	if rootPublicKeys := ExtenderRootPublicKeys(key, &empty); len(rootPublicKeys) != 0 {
		t.Fatalf("root keys = %v, expected the bundled table to be empty", rootPublicKeys)
	}
	if bundled := bundledExtenderRootPublicKeys("SPACE.EXAMPLE."); len(bundled) != 0 {
		t.Fatalf("bundled = %v, expected none", bundled)
	}
}

// The new values round-trip through the manager's json, and the removed
// auto-configure value is gone from the exported document (F1).
func TestExtenderNetworkSpaceValuesRoundTrip(t *testing.T) {
	storagePath, err := os.MkdirTemp("", "test_extender_values")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.ExtenderDnsName = "x.example"
		values.GossipUrl = "wss://g.example"
		values.ExtenderRootPublicKeys = []string{"aabbcc"}
	})
	if networkSpace.GetExtenderDnsName() != "x.example" {
		t.Fatalf("extender dns name = %q", networkSpace.GetExtenderDnsName())
	}
	if networkSpace.GetGossipUrl() != "wss://g.example" {
		t.Fatalf("gossip url = %q", networkSpace.GetGossipUrl())
	}
	rootPublicKeys := networkSpace.GetExtenderRootPublicKeys()
	if rootPublicKeys.Len() != 1 || rootPublicKeys.Get(0) != "aabbcc" {
		t.Fatalf("root keys = %v", rootPublicKeys.getAll())
	}

	spaceJson, err := networkSpace.ToJson()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spaceJson, `"extender_dns_name":"x.example"`) {
		t.Fatalf("exported json = %s", spaceJson)
	}
	if strings.Contains(spaceJson, "net_extender_auto_configure") {
		t.Fatalf("the removed auto configure value is still exported: %s", spaceJson)
	}
	networkSpaceManager.Close()

	// the manager's own document carries them across a restart
	stateBytes, err := os.ReadFile(filepath.Join(storagePath, ".network_spaces"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateBytes), "net_extender_auto_configure") {
		t.Fatal("the removed auto configure value is still persisted")
	}
	restored := NewNetworkSpaceManager(storagePath)
	t.Cleanup(restored.Close)
	restoredSpace := restored.GetNetworkSpace(key)
	if restoredSpace == nil {
		t.Fatal("the space did not survive the restart")
	}
	if restoredSpace.GetExtenderDnsName() != "x.example" {
		t.Fatalf("restored extender dns name = %q", restoredSpace.GetExtenderDnsName())
	}
	if restoredSpace.GetGossipUrl() != "wss://g.example" {
		t.Fatalf("restored gossip url = %q", restoredSpace.GetGossipUrl())
	}
	if keys := restoredSpace.GetExtenderRootPublicKeys(); keys.Len() != 1 {
		t.Fatalf("restored root keys = %v", keys.getAll())
	}
}

// The role rule of D5, with every platform input explicit.
func TestExtenderRoleForPlatform(t *testing.T) {
	const lowMemory ByteCount = 16 * 1024 * 1024
	const desktopMemory ByteCount = 512 * 1024 * 1024
	cases := []struct {
		name          string
		mode          string
		feedOnlyBuild bool
		mobile        bool
		memoryBudget  ByteCount
		expect        string
	}{
		{name: "desktop", mode: ExtenderGossipModeAuto, memoryBudget: desktopMemory, expect: ExtenderRoleMember},
		{name: "desktop no budget", mode: ExtenderGossipModeAuto, expect: ExtenderRoleMember},
		{name: "js", mode: ExtenderGossipModeAuto, feedOnlyBuild: true, memoryBudget: desktopMemory, expect: ExtenderRoleFeed},
		{name: "mobile low memory", mode: ExtenderGossipModeAuto, mobile: true, memoryBudget: lowMemory, expect: ExtenderRoleFeed},
		{name: "mobile at the target", mode: ExtenderGossipModeAuto, mobile: true, memoryBudget: mobileSteadyMemoryTargetByteCount, expect: ExtenderRoleFeed},
		{name: "mobile above the target", mode: ExtenderGossipModeAuto, mobile: true, memoryBudget: desktopMemory, expect: ExtenderRoleMember},
		{name: "mobile no budget", mode: ExtenderGossipModeAuto, mobile: true, expect: ExtenderRoleMember},
		{name: "forced member on js", mode: ExtenderGossipModeMember, feedOnlyBuild: true, expect: ExtenderRoleMember},
		{name: "forced feed on desktop", mode: ExtenderGossipModeFeed, memoryBudget: desktopMemory, expect: ExtenderRoleFeed},
		{name: "unknown mode is auto", mode: "nonsense", memoryBudget: desktopMemory, expect: ExtenderRoleMember},
	}
	for _, c := range cases {
		role := extenderRoleForPlatform(c.mode, c.feedOnlyBuild, c.mobile, c.memoryBudget)
		if role != c.expect {
			t.Errorf("%s: role = %s, expected %s", c.name, role, c.expect)
		}
	}
}

// The gossip mode persists and reaches the status role (D5).
func TestExtenderGossipModePersists(t *testing.T) {
	storagePath, err := os.MkdirTemp("", "test_extender_mode")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	if mode := networkSpace.GetExtenderGossipMode(); mode != ExtenderGossipModeAuto {
		t.Fatalf("mode = %q, expected auto", mode)
	}
	networkSpace.SetExtenderGossipMode(ExtenderGossipModeFeed)
	// the write is serialized behind the local state worker; the read below is
	// the barrier that proves it landed
	deadline := time.Now().Add(10 * time.Second)
	for networkSpace.GetExtenderGossipMode() != ExtenderGossipModeFeed {
		if deadline.Before(time.Now()) {
			t.Fatal("the gossip mode was never persisted")
		}
		time.Sleep(time.Millisecond)
	}
	if role := networkSpace.GetExtenderStatus().Role; role != ExtenderRoleFeed {
		t.Fatalf("role = %s, expected the forced feed role", role)
	}
	networkSpaceManager.Close()

	restored := NewNetworkSpaceManager(storagePath)
	t.Cleanup(restored.Close)
	restoredSpace := restored.GetNetworkSpace(key)
	if mode := restoredSpace.GetExtenderGossipMode(); mode != ExtenderGossipModeFeed {
		t.Fatalf("restored mode = %q, expected feed", mode)
	}
	// an unknown mode degrades to auto rather than sticking
	restoredSpace.SetExtenderGossipMode("nonsense")
	deadline = time.Now().Add(10 * time.Second)
	for restoredSpace.GetExtenderGossipMode() != ExtenderGossipModeAuto {
		if deadline.Before(time.Now()) {
			t.Fatal("an unknown mode did not degrade to auto")
		}
		time.Sleep(time.Millisecond)
	}
}

// The status reflects the directory, and the listener is coalesced to one
// callback per epoch (F2).
func TestExtenderStatusAndListener(t *testing.T) {
	storagePath, err := os.MkdirTemp("", "test_extender_status")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	t.Cleanup(networkSpaceManager.Close)
	networkSpace := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey("space.example", "main"),
		func(values *NetworkSpaceValues) {},
	)
	if networkSpace.extenderDirectory == nil {
		t.Fatal("the space has no extender directory")
	}
	if networkSpace.extenderNetworkClient != nil {
		t.Fatal("the suite disabled the network client, but the space started one")
	}

	statuses := make(chan *ExtenderStatus, 16)
	sub := networkSpace.AddExtenderStatusChangeListener(
		extenderStatusChangeListenerFunc(func(status *ExtenderStatus) {
			select {
			case statuses <- status:
			default:
			}
		}),
	)
	t.Cleanup(sub.Close)

	// a burst of directory changes is one callback
	for i := range 8 {
		networkSpace.extenderDirectory.AddBootstrap(
			netip.MustParseAddr(fmt.Sprintf("198.51.100.%d", 10+i)),
			connect.ExtenderSourceDns,
		)
	}

	var status *ExtenderStatus
	select {
	case status = <-statuses:
	case <-time.After(30 * time.Second):
		t.Fatal("the extender status listener was never called")
	}
	if status.KnownCount < 8 {
		t.Fatalf("known = %d, expected the whole burst in one callback", status.KnownCount)
	}
	if status.Extenders.Len() != status.KnownCount {
		t.Fatalf("entries = %d, known = %d", status.Extenders.Len(), status.KnownCount)
	}
	select {
	case extra := <-statuses:
		t.Fatalf("the burst produced a second callback: known = %d", extra.KnownCount)
	case <-time.After(100 * time.Millisecond):
	}

	// the rows carry the directory state
	entry := status.Extenders.Get(0)
	if entry.Source != connect.ExtenderSourceDns {
		t.Fatalf("source = %q, expected dns", entry.Source)
	}
	if entry.State != connect.ExtenderStateUnverified {
		t.Fatalf("state = %q, expected unverified", entry.State)
	}
	if entry.Id != "" {
		t.Fatalf("id = %q, expected none for an unverified address", entry.Id)
	}
	if entry.IpVersion != 4 {
		t.Fatalf("ip version = %d, expected 4", entry.IpVersion)
	}
	if entry.Carriers != "tcp,quic,dns" {
		t.Fatalf("carriers = %q, expected the carrier defaults", entry.Carriers)
	}

	// a failure moves the row, and the next epoch carries it
	networkSpace.extenderDirectory.RecordFailure(
		netip.MustParseAddr("198.51.100.10"),
		connect.ExtenderConnectModeTcpTls,
	)
	select {
	case status = <-statuses:
	case <-time.After(30 * time.Second):
		t.Fatal("a directory change did not reach the listener")
	}
	if status.HoldCount != 1 {
		t.Fatalf("hold = %d, expected the failed address", status.HoldCount)
	}
}

// The store writes and reads the directory under the space's storage (E1, F1).
func TestExtenderDirectoryStoreUnderTheSpaceStorage(t *testing.T) {
	storagePath, err := os.MkdirTemp("", "test_extender_store")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("198.51.100.60"),
		connect.ExtenderSourceDns,
	)
	// closing the manager closes the space, which writes anything the
	// coalescing window still held
	networkSpaceManager.Close()

	// the local state lives under `.by` of the space's env storage path
	storeBytes, err := os.ReadFile(filepath.Join(
		storagePath,
		"network_spaces",
		"space.example",
		"main",
		".by",
		extenderStoreFileName,
	))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(storeBytes), "198.51.100.60") {
		t.Fatalf("stored directory = %s", storeBytes)
	}

	restored := NewNetworkSpaceManager(storagePath)
	t.Cleanup(restored.Close)
	restoredSpace := restored.GetNetworkSpace(key)
	status := restoredSpace.GetExtenderStatus()
	if status.KnownCount != 1 {
		t.Fatalf("restored known = %d, expected the stored address", status.KnownCount)
	}
	if status.Extenders.Get(0).Ip != "198.51.100.60" {
		t.Fatalf("restored entry = %+v", status.Extenders.Get(0))
	}
}

// A url-only space runs no network client and reports an empty status rather
// than failing (F1, F2).
func TestExtenderUrlOnlySpaceRunsNoNetworkClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace := Testing_NewNetworkSpaceWithUrls(
		ctx,
		"https://127.0.0.1:8083",
		"wss://127.0.0.1:8080",
		connect.DefaultConnectSettings(),
	)
	defer networkSpace.Close()

	if networkSpace.extenderNetworkClient != nil {
		t.Fatal("a url-only space started a network client")
	}
	if networkSpace.extenderDirectory != nil {
		t.Fatal("a url-only space built a directory")
	}
	status := networkSpace.GetExtenderStatus()
	if status == nil || status.Extenders == nil {
		t.Fatal("a url-only space reported no status")
	}
	if status.KnownCount != 0 || status.FeedConnected {
		t.Fatalf("status = %+v, expected an empty one", status)
	}
	// the listener is still subscribable and simply never fires
	sub := networkSpace.AddExtenderStatusChangeListener(
		extenderStatusChangeListenerFunc(func(status *ExtenderStatus) {}),
	)
	sub.Close()
}

// A space whose host is not a real dns name runs no network client (F1).
func TestExtenderNetworkClientRuns(t *testing.T) {
	cases := []struct {
		hostName string
		envName  string
		values   NetworkSpaceValues
		expect   bool
	}{
		{hostName: "space.example", envName: "main", expect: true},
		{hostName: "space.example", envName: "g2", expect: true},
		{hostName: "test", envName: "test", expect: false},
		{hostName: "custom", envName: "custom", expect: false},
		{
			hostName: "test",
			envName:  "test",
			values:   NetworkSpaceValues{ExtenderDnsName: "x.example"},
			expect:   false,
		},
		{
			hostName: "old.example",
			envName:  "main",
			values:   NetworkSpaceValues{MigrationHostName: "new.example"},
			expect:   true,
		},
	}
	for _, c := range cases {
		key := NewNetworkSpaceKey(c.hostName, c.envName)
		values := c.values
		if runs := extenderNetworkClientRuns(key, &values); runs != c.expect {
			t.Errorf("%s/%s runs = %v, expected %v", c.hostName, c.envName, runs, c.expect)
		}
	}
}

// A single-label host derives no dns name worth resolving (F1).
func TestExtenderIsDottedHostName(t *testing.T) {
	cases := []struct {
		hostName string
		expect   bool
	}{
		{hostName: "space.example", expect: true},
		{hostName: "extender.space.example", expect: true},
		{hostName: "extender.space.example.", expect: true},
		{hostName: "test", expect: false},
		{hostName: "custom", expect: false},
		{hostName: "", expect: false},
		{hostName: "192.0.2.1", expect: false},
		{hostName: "2001:db8::1", expect: false},
		{hostName: "space..example", expect: false},
		{hostName: "space example", expect: false},
	}
	for _, c := range cases {
		if dotted := isDottedHostName(c.hostName); dotted != c.expect {
			t.Errorf("%q dotted = %v, expected %v", c.hostName, dotted, c.expect)
		}
	}
}

// extenderStatusChangeListenerFunc adapts a closure to the listener interface.
type extenderStatusChangeListenerFunc func(status *ExtenderStatus)

func (self extenderStatusChangeListenerFunc) ExtenderStatusChanged(status *ExtenderStatus) {
	self(status)
}
