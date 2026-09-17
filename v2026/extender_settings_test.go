package sdk

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// The manual hosts and the read-only store (EXTENDER.md K5, K6).

// A synthetic ed25519 public key, 32 bytes, for the root key value. It only
// has to be the right shape: nothing here verifies a signature with it.
const testExtenderRootPublicKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// The normalizer drops the blanks a text field leaves behind, so an edit that
// only adds whitespace is not a change.
func TestExtenderHostsNormalize(t *testing.T) {
	values := NetworkSpaceValues{
		ExtenderHosts: []string{" 192.0.2.1 ", "", "   ", "bootstrap.example"},
	}
	expected := []string{"192.0.2.1", "bootstrap.example"}
	if extenderHosts := ExtenderHosts(&values); !slices.Equal(extenderHosts, expected) {
		t.Fatalf("hosts = %v, expected %v", extenderHosts, expected)
	}
	empty := NetworkSpaceValues{}
	if extenderHosts := ExtenderHosts(&empty); len(extenderHosts) != 0 {
		t.Fatalf("hosts = %v, expected none", extenderHosts)
	}
}

// testExtenderManualHostsNetwork turns the network client on with an
// in-process resolver and hello, and records the settings of every client a
// space builds, so a test can see what the refresh loop was configured with.
type testExtenderNetworkClientSettingsRecorder struct {
	stateLock sync.Mutex
	manual    [][]string
}

func (self *testExtenderNetworkClientSettingsRecorder) record(hosts []string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.manual = append(self.manual, slices.Clone(hosts))
}

func (self *testExtenderNetworkClientSettingsRecorder) last() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.manual) == 0 {
		return nil
	}
	return slices.Clone(self.manual[len(self.manual)-1])
}

func (self *testExtenderNetworkClientSettingsRecorder) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.manual)
}

func testEnableExtenderManualHostsNetwork(t *testing.T) *testExtenderNetworkClientSettingsRecorder {
	t.Helper()
	recorder := &testExtenderNetworkClientSettingsRecorder{}
	extenderNetworkClientEnabled = true
	extenderNetworkClientConfigure = func(settings *connect.ExtenderNetworkClientSettings) {
		recorder.record(settings.ManualHosts)
		settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
			return nil, nil
		}
		settings.Hello = func(ctx context.Context) (*connect.ExtenderHelloResult, error) {
			return &connect.ExtenderHelloResult{}, nil
		}
	}
	t.Cleanup(func() {
		extenderNetworkClientEnabled = false
		extenderNetworkClientConfigure = nil
	})
	return recorder
}

// The configured hosts reach the refresh loop at construction, a change
// restarts the loop in place -- the space, and everything bound to it,
// survives the save -- and the new list is what the replacement carries (K6).
func TestExtenderHostsReachTheNetworkClientAndRestartIt(t *testing.T) {
	recorder := testEnableExtenderManualHostsNetwork(t)

	storagePath, err := os.MkdirTemp("", "test_extender_hosts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	t.Cleanup(networkSpaceManager.Close)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.ExtenderHosts = []string{"192.0.2.1"}
	})
	if networkSpace.getExtenderNetworkClient() == nil {
		t.Fatal("the space ran no extender network client")
	}
	if manual := recorder.last(); !slices.Equal(manual, []string{"192.0.2.1"}) {
		t.Fatalf("manual hosts = %v, expected the configured one", manual)
	}
	if hosts := networkSpace.GetExtenderHosts(); hosts.Len() != 1 || hosts.Get(0) != "192.0.2.1" {
		t.Fatalf("hosts = %v", hosts.getAll())
	}
	// an ip literal is added to the directory as a manual address, which the
	// removal policy never takes away
	deadline := time.Now().Add(30 * time.Second)
	for {
		manual := false
		for _, entry := range networkSpace.extenderDirectory.Snapshot().Entries {
			if entry.Ip.String() == "192.0.2.1" && entry.Source == connect.ExtenderSourceManual {
				manual = true
			}
		}
		if manual {
			break
		}
		if deadline.Before(time.Now()) {
			t.Fatal("the manual host never reached the directory")
		}
		time.Sleep(10 * time.Millisecond)
	}

	previousNetworkClient := networkSpace.getExtenderNetworkClient()
	previousCount := recorder.count()
	updated := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.ExtenderHosts = []string{"192.0.2.1", "bootstrap.example"}
	})
	// the space itself survives: a rebuild would hand the device and the view
	// controller that saved a closed space
	if updated != networkSpace {
		t.Fatal("an extender settings change rebuilt the space instead of restarting it in place")
	}
	if networkSpace.getExtenderNetworkClient() == previousNetworkClient {
		t.Fatal("the network client was not restarted")
	}
	if recorder.count() != previousCount+1 {
		t.Fatalf("client builds = %d, expected exactly one more", recorder.count())
	}
	expected := []string{"192.0.2.1", "bootstrap.example"}
	if manual := recorder.last(); !slices.Equal(manual, expected) {
		t.Fatalf("manual hosts = %v, expected %v", manual, expected)
	}
	if hosts := networkSpace.GetExtenderHosts(); !slices.Equal(hosts.getAll(), expected) {
		t.Fatalf("hosts = %v, expected %v", hosts.getAll(), expected)
	}

	// The one write path a settings screen uses stops at an edit that resolves
	// to nothing. `updateNetworkSpace` produces a new space generation
	// whatever it is handed -- the manager's stale-generation rules depend on
	// that -- so saving an unchanged form has to stop before it, not inside it.
	previousNetworkClient = networkSpace.getExtenderNetworkClient()
	previousCount = recorder.count()
	if networkSpace.updateExtenderValues(func(values *NetworkSpaceValues) {
		values.ExtenderHosts = []string{" 192.0.2.1 ", "bootstrap.example", ""}
	}) {
		t.Fatal("a whitespace-only edit reported a change")
	}
	if networkSpace.getExtenderNetworkClient() != previousNetworkClient {
		t.Fatal("a whitespace-only edit restarted the network client")
	}
	if recorder.count() != previousCount {
		t.Fatalf("client builds = %d, expected none", recorder.count())
	}
	if networkSpaceManager.GetNetworkSpace(key) != networkSpace {
		t.Fatal("a whitespace-only edit replaced the space")
	}
	// a real change through the same path does restart it
	if !networkSpace.updateExtenderValues(func(values *NetworkSpaceValues) {
		values.ExtenderHosts = []string{"192.0.2.1", "bootstrap.example", "198.51.100.7"}
	}) {
		t.Fatal("a real change reported none")
	}
	if networkSpace.getExtenderNetworkClient() == previousNetworkClient {
		t.Fatal("a real change did not restart the network client")
	}
	if networkSpaceManager.GetNetworkSpace(key) != networkSpace {
		t.Fatal("a real change replaced the space")
	}
	expected = append(expected, "198.51.100.7")

	// and the value survives a restart of the whole manager
	networkSpaceManager.Close()
	restored := NewNetworkSpaceManager(storagePath)
	t.Cleanup(restored.Close)
	restoredSpace := restored.GetNetworkSpace(key)
	if restoredSpace == nil {
		t.Fatal("the space did not survive the restart")
	}
	if hosts := restoredSpace.GetExtenderHosts(); !slices.Equal(hosts.getAll(), expected) {
		t.Fatalf("restored hosts = %v, expected %v", hosts.getAll(), expected)
	}
	spaceJson, err := restoredSpace.ToJson()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spaceJson, `"extender_hosts":["192.0.2.1","bootstrap.example","198.51.100.7"]`) {
		t.Fatalf("exported json = %s", spaceJson)
	}
}

// Each of the five extender values restarts the network in place, and a value
// outside them still rebuilds the whole space (K6).
func TestExtenderValuesRestartInPlaceAndOthersRebuild(t *testing.T) {
	storagePath, err := os.MkdirTemp("", "test_extender_value_changes")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	networkSpaceManager := NewNetworkSpaceManager(storagePath)
	t.Cleanup(networkSpaceManager.Close)
	key := NewNetworkSpaceKey("space.example", "main")
	networkSpace := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})

	inPlace := []struct {
		name  string
		apply func(values *NetworkSpaceValues)
	}{
		{name: "dns name", apply: func(v *NetworkSpaceValues) { v.ExtenderDnsName = "x.example" }},
		{name: "gossip url", apply: func(v *NetworkSpaceValues) { v.GossipUrl = "wss://g.example" }},
		{name: "hosts", apply: func(v *NetworkSpaceValues) { v.ExtenderHosts = []string{"192.0.2.1"} }},
		{name: "root keys", apply: func(v *NetworkSpaceValues) { v.ExtenderRootPublicKeys = []string{testExtenderRootPublicKeyHex} }},
		{name: "net extender", apply: func(v *NetworkSpaceValues) {
			v.NetExtender = &NetExtender{Ip: "192.0.2.2", Secret: "s"}
		}},
	}
	for _, c := range inPlace {
		updated := networkSpaceManager.updateNetworkSpace(key, c.apply)
		if updated != networkSpace {
			t.Fatalf("%s rebuilt the space instead of restarting it in place", c.name)
		}
	}
	// the directory anchor followed the root keys
	rootKeys := networkSpace.extenderDirectory.RootKeys()
	if rootKeys == nil || rootKeys.Len() != 1 {
		t.Fatalf("root keys = %v, expected the configured one", rootKeys)
	}

	// a value outside the extender set is a rebuild, which is what a changed
	// api host or env secret needs
	rebuilt := networkSpaceManager.updateNetworkSpace(key, func(values *NetworkSpaceValues) {
		values.LinkHostName = "link.example"
	})
	if rebuilt == networkSpace {
		t.Fatal("a non extender value was applied in place")
	}
	// the extender values it was carrying came with it
	if rebuilt.GetExtenderDnsName() != "x.example" {
		t.Fatalf("rebuilt dns name = %q", rebuilt.GetExtenderDnsName())
	}
	if hosts := rebuilt.GetExtenderHosts(); hosts.Len() != 1 {
		t.Fatalf("rebuilt hosts = %v", hosts.getAll())
	}
}

// On ios the app process and the tunnel extension share one app group
// directory. The app loads the shared `.extenders` file and never writes it,
// so the extension's directory is the only writer (K5).
func TestExtenderStoreReadOnlyLoadsAndNeverWrites(t *testing.T) {
	storagePath, err := os.MkdirTemp("", "test_extender_read_only")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	key := NewNetworkSpaceKey("space.example", "main")
	storePath := filepath.Join(
		storagePath,
		"network_spaces",
		"space.example",
		"main",
		".by",
		extenderStoreFileName,
	)

	// the writing process -- the tunnel extension -- seeds the file
	writer := NewNetworkSpaceManager(storagePath)
	writerSpace := writer.updateNetworkSpace(key, func(values *NetworkSpaceValues) {})
	writerSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("198.51.100.60"),
		connect.ExtenderSourceDns,
	)
	writer.Close()

	seeded, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	seededInfo, err := os.Stat(storePath)
	if err != nil {
		t.Fatal(err)
	}

	// the app process opens the same directory read-only
	SetExtenderStoreReadOnly(true)
	t.Cleanup(func() { SetExtenderStoreReadOnly(false) })
	if !GetExtenderStoreReadOnly() {
		t.Fatal("the read-only switch did not take")
	}

	reader := NewNetworkSpaceManager(storagePath)
	readerSpace := reader.GetNetworkSpace(key)
	if readerSpace == nil {
		t.Fatal("the read-only process did not load the space")
	}
	// it LOADS: the app starts warm on what the extension discovered
	status := readerSpace.GetExtenderStatus()
	if status.KnownCount != 1 || status.Extenders.Get(0).Ip != "198.51.100.60" {
		t.Fatalf("read-only status = %+v", status)
	}
	// and it never writes, not even through the final save that Close forces
	readerSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("203.0.113.42"),
		connect.ExtenderSourceDns,
	)
	readerSpace.extenderDirectory.AddManual(netip.MustParseAddr("192.0.2.1"))
	readerSpace.extenderDirectory.RecordFailure(
		netip.MustParseAddr("198.51.100.60"),
		connect.ExtenderConnectModeTcpTls,
	)
	reader.Close()

	after, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(seeded) {
		t.Fatalf("the read-only process rewrote the store:\n%s\n%s", seeded, after)
	}
	if !afterInfo.ModTime().Equal(seededInfo.ModTime()) {
		t.Fatalf(
			"the read-only process touched the store: %s then %s",
			seededInfo.ModTime(),
			afterInfo.ModTime(),
		)
	}

	// the same sequence with the switch off DOES write, so the assertion above
	// is about the switch and not about a directory that never changed
	SetExtenderStoreReadOnly(false)
	writeAgain := NewNetworkSpaceManager(storagePath)
	writeAgainSpace := writeAgain.GetNetworkSpace(key)
	writeAgainSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr("203.0.113.42"),
		connect.ExtenderSourceDns,
	)
	writeAgain.Close()
	rewritten, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(rewritten) == string(seeded) {
		t.Fatal("the read-write process did not write the store, so the read-only proof is vacuous")
	}
}
