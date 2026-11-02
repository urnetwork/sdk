package sdk

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
)

// The mirrors of a space's seed bundle (extender_seed_mirrors.go): a setting
// of the space, else the bundled table for its host, read into the entries a
// pass dials; applied in place, and kept by a reset.

func TestExtenderSeedMirrorEntries(t *testing.T) {
	key := &NetworkSpaceKey{HostName: testSeedNetworkHost, EnvName: "main"}
	type entry struct {
		url            string
		address        string
		dnsIndependent bool
	}
	cases := []struct {
		name    string
		mirrors []ExtenderSeedMirrorValue
		entries []entry
	}{
		{
			name:    "a host takes the fixed path",
			mirrors: []ExtenderSeedMirrorValue{{Url: "https://Mirror.Seed.Example."}},
			entries: []entry{{url: "https://mirror.seed.example/v1/seed.bin"}},
		},
		{
			name:    "a path and an address, which takes the url's port",
			mirrors: []ExtenderSeedMirrorValue{{Url: " https://mirror.seed.example:8443/a/seed.bin ", Address: " 203.0.113.7 "}},
			entries: []entry{{url: "https://mirror.seed.example:8443/a/seed.bin", address: "203.0.113.7:8443", dnsIndependent: true}},
		},
		{
			name: "a dual-stack mirror is two entries with one url",
			mirrors: []ExtenderSeedMirrorValue{
				{Url: "https://mirror.seed.example", Address: "203.0.113.7"},
				{Url: "https://mirror.seed.example", Address: "2001:db8::7"},
			},
			entries: []entry{
				{url: "https://mirror.seed.example/v1/seed.bin", address: "203.0.113.7:443", dnsIndependent: true},
				{url: "https://mirror.seed.example/v1/seed.bin", address: "[2001:db8::7]:443", dnsIndependent: true},
			},
		},
		{
			name: "v6 addresses, with and without a port",
			mirrors: []ExtenderSeedMirrorValue{
				{Url: "https://mirror.seed.example", Address: "[2001:db8::7]:9443"},
				{Url: "https://mirror.seed.example", Address: "2001:db8::8"},
			},
			entries: []entry{
				{url: "https://mirror.seed.example/v1/seed.bin", address: "[2001:db8::7]:9443", dnsIndependent: true},
				{url: "https://mirror.seed.example/v1/seed.bin", address: "[2001:db8::8]:443", dnsIndependent: true},
			},
		},
		{
			name: "entries that verify nothing or dial nowhere are left out",
			mirrors: []ExtenderSeedMirrorValue{
				{},
				{Url: "http://mirror.seed.example"},
				{Url: "https://203.0.113.7/v1/seed.bin"},
				{Url: "https://user@mirror.seed.example"},
				{Url: "https://mirror.seed.example/?sequence=1"},
				{Url: "https://mirror.seed.example/#seed"},
				{Url: "https://mirror"},
				{Url: "https://mirror.seed.example:0"},
				{Url: "https://mirror.seed.example", Address: "0.0.0.0"},
				{Url: "https://mirror.seed.example", Address: "224.0.0.1"},
				{Url: "https://mirror.seed.example", Address: "169.254.1.1"},
				{Url: "https://mirror.seed.example", Address: "fe80::1"},
				{Url: "https://mirror.seed.example", Address: "203.0.113.7:0"},
				{Url: "https://mirror.seed.example", Address: "mirror.seed.example"},
				{Url: "https://mirror.seed.example", Address: "198.51.100.1"},
			},
			entries: []entry{{url: "https://mirror.seed.example/v1/seed.bin", address: "198.51.100.1:443", dnsIndependent: true}},
		},
		{
			name: "a repeat is one entry",
			mirrors: []ExtenderSeedMirrorValue{
				{Url: "https://mirror.seed.example", Address: "203.0.113.7"},
				{Url: "https://mirror.seed.example/v1/seed.bin", Address: "203.0.113.7:443"},
			},
			entries: []entry{{url: "https://mirror.seed.example/v1/seed.bin", address: "203.0.113.7:443", dnsIndependent: true}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			values := &NetworkSpaceValues{ExtenderSeedMirrors: c.mirrors}
			entries := []entry{}
			for _, mirror := range spaceExtenderSeedMirrors(key, values) {
				entries = append(entries, entry{
					url:            mirror.url,
					address:        mirror.addressText(),
					dnsIndependent: mirror.dnsIndependent(),
				})
			}
			if !slices.Equal(entries, c.entries) {
				t.Errorf("the entries are %+v, not %+v", entries, c.entries)
			}
		})
	}

	// a pass fetches from at most extenderSeedMaxMirrorCount
	many := []ExtenderSeedMirrorValue{}
	for i := range extenderSeedMaxMirrorCount + 3 {
		many = append(many, ExtenderSeedMirrorValue{Url: fmt.Sprintf("https://mirror%d.seed.example", i)})
	}
	if mirrors := spaceExtenderSeedMirrors(key, &NetworkSpaceValues{ExtenderSeedMirrors: many}); len(mirrors) != extenderSeedMaxMirrorCount {
		t.Errorf("%d configured mirrors gave %d entries", len(many), len(mirrors))
	}
}

// The bundled table is the default for the host the space's extender network
// is keyed by, empty in this build; configured mirrors replace it.
func TestExtenderSeedMirrorDefaults(t *testing.T) {
	if 0 < len(bundledExtenderSeedMirrorValues) {
		t.Errorf("the build carries default mirrors %v; they are an owner decision", bundledExtenderSeedMirrorValues)
	}
	key := &NetworkSpaceKey{HostName: testSeedNetworkHost, EnvName: "main"}
	if mirrors := extenderSeedMirrorValues(key, &NetworkSpaceValues{}); len(mirrors) != 0 {
		t.Errorf("an unconfigured space has mirrors %v", mirrors)
	}

	bundled := ExtenderSeedMirrorValue{Url: "https://bundled.seed.example", Address: "203.0.113.9"}
	bundledExtenderSeedMirrorValues[testSeedNetworkHost] = []ExtenderSeedMirrorValue{bundled}
	t.Cleanup(func() {
		delete(bundledExtenderSeedMirrorValues, testSeedNetworkHost)
	})
	mirrors := extenderSeedMirrorValues(key, &NetworkSpaceValues{})
	if len(mirrors) != 1 || mirrors[0] != bundled {
		t.Fatalf("an unconfigured space reads %v, not the bundled table", mirrors)
	}
	// a copy: the caller cannot edit the table through it
	mirrors[0].Url = "https://edited.example"
	if bundledExtenderSeedMirrorValues[testSeedNetworkHost][0] != bundled {
		t.Error("the bundled table was edited through its copy")
	}
	// another host does not read this host's table
	if mirrors := extenderSeedMirrorValues(&NetworkSpaceKey{HostName: "other.example", EnvName: "main"}, &NetworkSpaceValues{}); len(mirrors) != 0 {
		t.Errorf("another host reads %v", mirrors)
	}
	configured := ExtenderSeedMirrorValue{Url: "https://configured.seed.example"}
	mirrors = extenderSeedMirrorValues(key, &NetworkSpaceValues{
		ExtenderSeedMirrors: []ExtenderSeedMirrorValue{{Url: "  "}, configured},
	})
	if len(mirrors) != 1 || mirrors[0] != configured {
		t.Errorf("a configured space reads %v, not its own mirrors", mirrors)
	}
}

// A change of the mirrors alone is applied in place: the extender network does
// not restart, the status counts the new mirrors, the backoff the old ones
// built up starts over, and the next pass fetches from the new ones. A reset
// keeps them; an edit that changes only whitespace changes nothing.
func TestExtenderSeedMirrorValuesApplyInPlace(t *testing.T) {
	var requestLock sync.Mutex
	var requestUrls [][]string
	mirrorPass := &testSeedMirrorPass{answer: func(ctx context.Context, request *extenderSeedMirrorRequest) *extenderSeedMirrorResult {
		urls := []string{}
		for _, mirror := range request.mirrors {
			urls = append(urls, mirror.url)
		}
		requestLock.Lock()
		defer requestLock.Unlock()
		requestUrls = append(requestUrls, urls)
		return &extenderSeedMirrorResult{code: connect.ExtenderSeedCodeFetch}
	}}
	space, _ := newTestSeedLifecycleSpace(t, mirrorPass, nil)
	seed := space.getExtenderSeed()
	if status := space.GetExtenderSeedStatus(); status.MirrorCount != 1 || status.DnsIndependentMirrorCount != 1 {
		t.Fatalf("the status counts the mirrors as %+v", status)
	}
	seed.awaitPass(context.Background())
	seed.awaitPass(context.Background())
	if calls := mirrorPass.calls.Load(); calls != 1 {
		t.Fatalf("a failed fetch left %d fetches, not a backoff", calls)
	}

	nodeChanged := space.extenderNodeMonitor.NotifyChannel()
	mirrorValues := []ExtenderSeedMirrorValue{
		{Url: "https://other.seed.example"},
		{Url: testSeedMirrorConfig.Url, Address: testSeedMirrorConfig.Address},
	}
	if !space.updateInPlaceValues(func(values *NetworkSpaceValues) {
		values.ExtenderSeedMirrors = mirrorValues
	}) {
		t.Fatal("the new mirrors were not applied")
	}
	select {
	case <-nodeChanged:
		t.Error("a change of the mirrors restarted the extender network")
	default:
	}
	// the space keeps its own copy
	mirrorValues[0].Url = "https://edited.example"
	if status := space.GetExtenderSeedStatus(); status.MirrorCount != 2 || status.DnsIndependentMirrorCount != 1 {
		t.Errorf("the status counts the new mirrors as %+v", status)
	}
	seed.awaitPass(context.Background())
	if calls := mirrorPass.calls.Load(); calls != 2 {
		t.Fatalf("the new mirrors waited out the old backoff: %d fetches", calls)
	}
	func() {
		requestLock.Lock()
		defer requestLock.Unlock()
		expected := []string{"https://other.seed.example/v1/seed.bin", "https://mirror.seed.example/v1/seed.bin"}
		if last := requestUrls[len(requestUrls)-1]; !slices.Equal(last, expected) {
			t.Errorf("the pass fetched from %v, not %v", last, expected)
		}
	}()

	if space.updateInPlaceValues(func(values *NetworkSpaceValues) {
		values.ExtenderSeedMirrors = []ExtenderSeedMirrorValue{
			{Url: " https://other.seed.example "},
			{Url: testSeedMirrorConfig.Url, Address: " " + testSeedMirrorConfig.Address},
			{},
		}
	}) {
		t.Error("an edit of whitespace was a change")
	}

	space.ResetExtenders()
	values := space.valuesCopy()
	if mirrors := extenderSeedMirrorValues(&space.key, &values); len(mirrors) != 2 {
		t.Errorf("after a reset the space has mirrors %v", mirrors)
	}
}
