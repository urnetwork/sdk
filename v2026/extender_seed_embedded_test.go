package sdk

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The seed bundles a release carries (extender_seed_embedded.go,
// EXTENDER-UPDATE2.md §2.3): one more source, judged exactly as a fetched one,
// whose expiry ends its use with an explicit freshness failure.

// The resource this build carries parses, carries no more bundles than one
// may, and every bundle line in it decodes. A release that writes a bad line
// into it fails here (the release steps in extender_seed_embedded.go).
func TestExtenderSeedEmbeddedResourceParses(t *testing.T) {
	bundles := parseExtenderSeedEmbedded(extenderSeedEmbeddedResourceText)
	if bundles.err != nil {
		t.Fatalf("the embedded resource does not parse: %v", bundles.err)
	}
	if extenderSeedEmbeddedMaxBundleCount < len(bundles.texts) {
		t.Fatalf("the embedded resource carries %d bundles", len(bundles.texts))
	}
	for _, text := range bundles.texts {
		if _, err := connect.DecodeExtenderSeedText(text); err != nil {
			t.Errorf("an embedded bundle does not decode: %v", err)
		}
	}
}

func TestExtenderSeedEmbeddedParser(t *testing.T) {
	bundleLine := connect.ExtenderSeedTextPrefix + "AAAA"
	cases := []struct {
		name         string
		resource     string
		texts        []string
		minSequences map[string]uint64
		fails        bool
	}{
		{name: "empty", resource: ""},
		{name: "comments and blank lines", resource: "# one\n\n  # two  \n\r\n"},
		{
			name:     "bundles in file order, trimmed",
			resource: fmt.Sprintf("# header\n  %s1 \r\n%s2\n", bundleLine, bundleLine),
			texts:    []string{bundleLine + "1", bundleLine + "2"},
		},
		{
			name:         "floors, normalized, the higher of two kept",
			resource:     "min-sequence Seed.Example. 7\nmin-sequence seed.example 3\nmin-sequence other.example 1\n",
			minSequences: map[string]uint64{"seed.example": 7, "other.example": 1},
		},
		{
			name:     "at the bundle cap",
			resource: strings.Repeat(bundleLine+"\n", extenderSeedEmbeddedMaxBundleCount),
			texts:    testSeedRepeat(bundleLine, extenderSeedEmbeddedMaxBundleCount),
		},
		{name: "over the bundle cap", resource: strings.Repeat(bundleLine+"\n", extenderSeedEmbeddedMaxBundleCount+1), fails: true},
		{name: "a floor with no host", resource: "min-sequence 7\n", fails: true},
		{name: "a floor with a bad host", resource: "min-sequence nodot 7\n", fails: true},
		{name: "a floor that is not a number", resource: "min-sequence seed.example seven\n", fails: true},
		{name: "a floor that is negative", resource: "min-sequence seed.example -1\n", fails: true},
		{name: "a K7 share line", resource: "ur-ext:1:AAAA\n", fails: true},
		{name: "anything else", resource: "hello\n", fails: true},
		{name: "over the size cap", resource: strings.Repeat("#", extenderSeedEmbeddedMaxByteCount+1), fails: true},
	}
	for _, c := range cases {
		bundles := parseExtenderSeedEmbedded(c.resource)
		if c.fails {
			if bundles.err == nil {
				t.Errorf("%s: parsed", c.name)
			}
			// a resource that does not parse carries nothing at all
			if len(bundles.texts) != 0 || len(bundles.minSequences) != 0 {
				t.Errorf("%s: a malformed resource carries %v %v", c.name, bundles.texts, bundles.minSequences)
			}
			continue
		}
		if bundles.err != nil {
			t.Errorf("%s: %v", c.name, bundles.err)
			continue
		}
		if strings.Join(bundles.texts, "|") != strings.Join(c.texts, "|") {
			t.Errorf("%s: texts %v, not %v", c.name, bundles.texts, c.texts)
		}
		if len(bundles.minSequences) != len(c.minSequences) {
			t.Errorf("%s: floors %v, not %v", c.name, bundles.minSequences, c.minSequences)
		}
		for networkHost, minSequence := range c.minSequences {
			if bundles.minSequence(networkHost) != minSequence {
				t.Errorf("%s: the floor of %s is %d", c.name, networkHost, bundles.minSequence(networkHost))
			}
		}
	}
	var none *extenderSeedEmbeddedBundles
	if none.minSequence(testSeedNetworkHost) != 0 {
		t.Error("no resource sets a floor")
	}
}

func testSeedRepeat(text string, count int) []string {
	texts := []string{}
	for range count {
		texts = append(texts, text)
	}
	return texts
}

// A seed test space carrying `resource` as its embedded bundles, with no
// mirrors.
func newTestSeedEmbeddedSpace(t *testing.T, resource string) (*NetworkSpace, *testSeedClock, *testSeedDialer) {
	t.Helper()
	clock := newTestSeedClock()
	dialer := newTestSeedDialer()
	testSeedInstallConnectSettings(t, dialer, nil)
	testSeedInstallEmbedded(t, resource)
	testSeedInstallSettings(t, func(settings *extenderSeedSettings) {
		settings.Now = clock.Now
	})
	return newTestSeedSpace(t, testSeedStoragePath(t)), clock, dialer
}

// An embedded bundle past its validity fails the pass with an explicit
// freshness failure the status names; nothing of it is applied, accepted or
// dialed, and the old binary does not get to bypass the check (§2.3, §2.4).
func TestExtenderSeedEmbeddedExpiredBundleFailsExplicitly(t *testing.T) {
	now := time.Now()
	expired := testSeedEnvelope(t, now, testSeedBundleSpec{
		sequence:   4,
		issueTime:  now.Add(-29 * 24 * time.Hour),
		expireTime: now.Add(-time.Hour),
		records: []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRecordSpec{
			index:      0,
			issueTime:  now.Add(-30 * 24 * time.Hour),
			expireTime: now.Add(-time.Hour),
		})},
	})
	space, _, dialer := newTestSeedEmbeddedSpace(t, testSeedText(t, expired)+"\n")
	seed := space.getExtenderSeed()

	result := seed.awaitPass(context.Background())
	if result.usable || result.outcome != ExtenderSeedPassOutcomeFailed || result.code != connect.ExtenderSeedCodeExpired {
		t.Fatalf("the pass over an expired embedded bundle: %+v", result)
	}
	status := space.GetExtenderSeedStatus()
	if status.EmbeddedCount != 1 || status.EmbeddedError != connect.ExtenderSeedCodeExpired ||
		status.LastPassOutcome != ExtenderSeedPassOutcomeFailed || status.LastPassError != connect.ExtenderSeedCodeExpired ||
		status.Sequence != 0 || status.LastPassTime == 0 {
		t.Errorf("the status after the expired bundle is %+v", status)
	}
	testSeedRequireNothingApplied(t, space)
	if sequence, err := seed.acceptor.Sequence(testSeedNetworkHost, space.extenderDirectory.RootKeys()); err != nil || sequence != 0 {
		t.Errorf("the expired bundle set the floor %d, %v", sequence, err)
	}
	if dialed := dialer.dialed(); len(dialed) != 0 {
		t.Errorf("the pass dialed %v", dialed)
	}
	// and the hook says so: nothing new
	if seed.obtainExtenderControlReach(context.Background()) {
		t.Error("the control reach reported an expired bundle as reach")
	}
}

// A valid embedded bundle is accepted into the store and applied by the first
// pass, which then reports reach.
func TestExtenderSeedEmbeddedBundleIsASource(t *testing.T) {
	now := time.Now()
	envelope := testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 12})
	space, _, dialer := newTestSeedEmbeddedSpace(t, "# release\n"+testSeedText(t, envelope)+"\n")
	seed := space.getExtenderSeed()

	if !seed.obtainExtenderControlReach(context.Background()) {
		t.Fatal("the embedded bundle did not make a record usable")
	}
	entry := testSeedDirectoryEntry(space, 0)
	if entry == nil || entry.Source != connect.ExtenderSourceSeed {
		t.Fatalf("the embedded record landed as %+v", entry)
	}
	status := space.GetExtenderSeedStatus()
	if status.Sequence != 12 || status.Source != ExtenderSeedSourceEmbedded || status.EmbeddedError != "" ||
		status.LastPassOutcome != ExtenderSeedPassOutcomeApplied {
		t.Errorf("the status after the embedded bundle is %+v", status)
	}
	if sequence, err := seed.acceptor.Sequence(testSeedNetworkHost, space.extenderDirectory.RootKeys()); err != nil || sequence != 12 {
		t.Errorf("the embedded bundle set the floor %d, %v", sequence, err)
	}
	// a second pass has nothing new: the record is already usable
	if seed.obtainExtenderControlReach(context.Background()) {
		t.Error("a second pass reported the same record as new reach")
	}
	if dialed := dialer.dialed(); len(dialed) != 0 {
		t.Errorf("the passes dialed %v", dialed)
	}
}

// A release carries the bundles of every network it ships with: one for
// another network is skipped, not reported, and a forged one for this network
// is refused and named.
func TestExtenderSeedEmbeddedBundlesForOtherNetworksAreSkipped(t *testing.T) {
	now := time.Now()
	foreignRecord := testSeedRecord(t, testSeedRecordSpec{
		index:       1,
		networkHost: testSeedForeignNetworkHost,
		issueTime:   now.Add(-2 * time.Hour),
		expireTime:  now.Add(48 * time.Hour),
		// the other network's own key, which this space does not trust
		signer: testSeedForgerPrivateKey,
	})
	foreign := testSeedEnvelope(t, now, testSeedBundleSpec{
		networkHost: testSeedForeignNetworkHost,
		records:     []*protocol.ExtenderRecord{foreignRecord},
		signer:      testSeedForgerPrivateKey,
	})
	own := testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 2})
	space, _, _ := newTestSeedEmbeddedSpace(t, testSeedText(t, foreign)+"\n"+testSeedText(t, own)+"\n")
	seed := space.getExtenderSeed()
	if !seed.obtainExtenderControlReach(context.Background()) {
		t.Fatal("this network's bundle was not applied")
	}
	if status := space.GetExtenderSeedStatus(); status.EmbeddedError != "" || status.EmbeddedCount != 2 {
		t.Errorf("another network's bundle was reported: %+v", status)
	}

	forged := testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 3, signer: testSeedForgerPrivateKey})
	forgedSpace, _, _ := newTestSeedEmbeddedSpace(t, testSeedText(t, forged)+"\n")
	if forgedSpace.getExtenderSeed().obtainExtenderControlReach(context.Background()) {
		t.Fatal("a forged embedded bundle made a record usable")
	}
	if status := forgedSpace.GetExtenderSeedStatus(); status.EmbeddedError != connect.ExtenderSeedCodeSignature {
		t.Errorf("a forged embedded bundle was reported as %+v", status)
	}
	testSeedRequireNothingApplied(t, forgedSpace)
}

// A floor from the build is trusted application configuration: no bundle
// below it is accepted, from any source (§2.3).
func TestExtenderSeedEmbeddedMinSequenceIsAFloor(t *testing.T) {
	space, clock, _ := newTestSeedEmbeddedSpace(t, "min-sequence seed.example 10\n")
	below := testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 9})
	if result := space.ImportExtenderSeed(testSeedText(t, below)); result.Ok || result.Error != ExtenderSeedImportErrorRollback {
		t.Errorf("a bundle below the build's floor: %+v", result)
	}
	at := testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 10})
	if result := space.ImportExtenderSeed(testSeedText(t, at)); !result.Ok {
		t.Errorf("a bundle at the build's floor: %+v", result)
	}
}

// A resource that does not parse carries nothing and is named on the status.
func TestExtenderSeedEmbeddedMalformedResourceIsReported(t *testing.T) {
	space, _, _ := newTestSeedEmbeddedSpace(t, "not a seed\n")
	seed := space.getExtenderSeed()
	if status := space.GetExtenderSeedStatus(); status.EmbeddedError != extenderSeedEmbeddedInvalid || status.EmbeddedCount != 0 {
		t.Errorf("a malformed resource is reported as %+v", status)
	}
	result := seed.awaitPass(context.Background())
	if result.outcome != ExtenderSeedPassOutcomeFailed || result.code != extenderSeedEmbeddedInvalid {
		t.Errorf("a pass over a malformed resource: %+v", result)
	}
}
