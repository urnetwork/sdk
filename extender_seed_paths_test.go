package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The remaining paths of the seed bootstrap: a rebuild that brings a reset, a
// url-only space, and how a pass judges what it holds when part of it no longer
// holds up.

// A rebuild that brings a reset (EXTENDER.md E7) is applied to the space it
// replaces first: its seed bootstrap stops before the store is cleared, the
// replacement starts from a fresh install's floor, and the mirrors, which are a
// setting, stay.
func TestExtenderSeedRebuildAppliesTheResetToTheStore(t *testing.T) {
	testSeedInstallBundledRootKey(t)
	testSeedInstallConnectSettings(t, newTestSeedDialer(), nil)
	testSeedInstallEmbedded(t, "")
	clock := newTestSeedClock()
	testSeedInstallSettings(t, func(settings *extenderSeedSettings) {
		settings.Now = clock.Now
	})
	now := clock.Now()

	networkSpaceManager := NewNetworkSpaceManager(t.TempDir())
	t.Cleanup(networkSpaceManager.Close)
	mirror := ExtenderSeedMirrorValue{Url: "https://mirror.seed.example", Address: "203.0.113.9"}
	replaced := networkSpaceManager.updateNetworkSpace(
		NewNetworkSpaceKey(testSeedNetworkHost, "main"),
		func(values *NetworkSpaceValues) {
			values.ExtenderSeedMirrors = []ExtenderSeedMirrorValue{mirror}
		},
	)
	replacedSeed := replaced.getExtenderSeed()
	if replacedSeed == nil {
		t.Fatal("the space runs no seed bootstrap")
	}
	if result := replaced.ImportExtenderSeed(testSeedText(t, testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 5}))); !result.Ok {
		t.Fatalf("the bundle: %+v", result)
	}

	resetId := connect.NewId().String()
	networkSpace := networkSpaceManager.updateNetworkSpace(replaced.GetKey(), func(values *NetworkSpaceValues) {
		values.LinkHostName = "link.example"
		resetExtenderValues(values, resetId)
	})
	if networkSpace == replaced {
		t.Fatal("the test expected a rebuild")
	}

	if pass := replacedSeed.joinPass(); pass != nil {
		t.Error("the replaced space's seed bootstrap still runs passes")
	}
	seed := networkSpace.getExtenderSeed()
	if seed == nil {
		t.Fatal("the replacement runs no seed bootstrap")
	}
	if sequence, err := seed.acceptor.Sequence(testSeedNetworkHost, networkSpace.extenderDirectory.RootKeys()); err != nil || sequence != 0 {
		t.Errorf("after the rebuild the floor is %d, %v", sequence, err)
	}
	if status := networkSpace.GetExtenderSeedStatus(); status.Sequence != 0 || status.MirrorCount != 1 || status.DnsIndependentMirrorCount != 1 {
		t.Errorf("the replacement's status is %+v", status)
	}
	values := networkSpace.valuesCopy()
	if mirrors := extenderSeedMirrorValues(networkSpace.GetKey(), &values); len(mirrors) != 1 || mirrors[0] != mirror {
		t.Errorf("after the rebuild the mirrors are %v", mirrors)
	}
	// as on a fresh install, a lower sequence is accepted again
	result := networkSpace.ImportExtenderSeed(testSeedText(t, testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 4})))
	if !result.Ok || result.Outcome != ExtenderSeedImportOutcomeAccepted {
		t.Errorf("the older bundle after the rebuild: %+v", result)
	}
}

// A url-only space whose api url names a real host runs the seed bootstrap like
// any other (F1): the host derived from the url selects its root keys and its
// bundled mirrors. It keeps no local state, so its floors last as long as it
// does. An api url that names an ip literal keys no network and runs none.
func TestExtenderSeedRunsOnAUrlOnlySpace(t *testing.T) {
	testSeedInstallBundledRootKey(t)
	testSeedInstallEmbedded(t, "")
	clock := newTestSeedClock()
	testSeedInstallSettings(t, func(settings *extenderSeedSettings) {
		settings.Now = clock.Now
	})
	bundledExtenderSeedMirrorValues[testSeedNetworkHost] = []ExtenderSeedMirrorValue{
		{Url: "https://bundled.seed.example", Address: "203.0.113.9"},
	}
	t.Cleanup(func() {
		delete(bundledExtenderSeedMirrorValues, testSeedNetworkHost)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	space := NewNetworkSpaceWithUrls(
		ctx,
		"https://api."+testSeedNetworkHost,
		"wss://connect."+testSeedNetworkHost,
		testOfflineClientStrategySettings(),
	)
	t.Cleanup(space.close)
	seed := space.getExtenderSeed()
	if seed == nil {
		t.Fatal("a url-only space on a real host runs no seed bootstrap")
	}
	if _, ok := seed.store.(*connect.ExtenderSeedMemoryStore); !ok {
		t.Errorf("a space with no local state stores its seeds in %T", seed.store)
	}
	if status := space.GetExtenderSeedStatus(); status.MirrorCount != 1 || status.DnsIndependentMirrorCount != 1 {
		t.Errorf("the status counts the bundled mirrors as %+v", status)
	}
	now := clock.Now()
	result := space.ImportExtenderSeed(testSeedText(t, testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 5})))
	if !result.Ok || result.Outcome != ExtenderSeedImportOutcomeAccepted || result.UsableCount != 1 {
		t.Fatalf("the bundle: %+v", result)
	}
	if result := space.ImportExtenderSeed(testSeedText(t, testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 4}))); result.Error != ExtenderSeedImportErrorRollback {
		t.Errorf("an older bundle: %+v", result)
	}

	noNetwork := NewNetworkSpaceWithUrls(ctx, "https://192.0.2.1", "wss://192.0.2.1", testOfflineClientStrategySettings())
	t.Cleanup(noNetwork.close)
	if noNetwork.getExtenderSeed() != nil {
		t.Fatal("a space with no extender network runs a seed bootstrap")
	}
	if result := noNetwork.ImportExtenderSeed(testSeedText(t, testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 5}))); result.Error != ExtenderSeedImportErrorUnavailable {
		t.Errorf("a space with no extender network imported: %+v", result)
	}
	// its mirrors can still be set; nothing reads them
	if !noNetwork.updateInPlaceValues(func(values *NetworkSpaceValues) {
		values.ExtenderSeedMirrors = []ExtenderSeedMirrorValue{{Url: "https://mirror.seed.example"}}
	}) {
		t.Error("the mirrors of a space with no extender network were not taken")
	}
	if status := noNetwork.GetExtenderSeedStatus(); *status != (ExtenderSeedStatus{}) {
		t.Errorf("a space with no extender network reports %+v", status)
	}
}

// A release older than what the store holds is not a failure: the store's
// newer bundle stays in force, and the status names no embedded refusal.
func TestExtenderSeedEmbeddedOlderThanTheStoreIsNoFailure(t *testing.T) {
	now := time.Now()
	older := testSeedEnvelope(t, now, testSeedBundleSpec{sequence: 4})
	space, clock, _ := newTestSeedEmbeddedSpace(t, testSeedText(t, older)+"\n")
	seed := space.getExtenderSeed()
	if result := space.ImportExtenderSeed(testSeedText(t, testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 9}))); !result.Ok {
		t.Fatalf("the newer bundle: %+v", result)
	}

	result := seed.awaitPass(context.Background())
	if result.usable || result.outcome != ExtenderSeedPassOutcomeNothingNew {
		t.Errorf("the pass with an older embedded bundle: %+v", result)
	}
	status := space.GetExtenderSeedStatus()
	if status.EmbeddedError != "" || status.Sequence != 9 || status.Source != ExtenderSeedSourceImport {
		t.Errorf("the status after the pass is %+v", status)
	}
	if sequence, err := seed.acceptor.Sequence(testSeedNetworkHost, space.extenderDirectory.RootKeys()); err != nil || sequence != 9 {
		t.Errorf("the floor is %d, %v", sequence, err)
	}
}

// An embedded bundle whose body does not decode claims no host, so it is judged
// rather than skipped, and the refusal is named.
func TestExtenderSeedEmbeddedUndecodableBodyIsNamed(t *testing.T) {
	undecodable := testSeedAdversarialEnvelopeBytes(testSeedForgerPrivateKey, []byte{0xff, 0xff, 0xff})
	space, _, _ := newTestSeedEmbeddedSpace(t, testSeedText(t, undecodable)+"\n")
	result := space.getExtenderSeed().awaitPass(context.Background())
	if result.usable || result.outcome != ExtenderSeedPassOutcomeFailed || result.code != connect.ExtenderSeedCodeSignature {
		t.Errorf("the pass over an undecodable embedded bundle: %+v", result)
	}
	if status := space.GetExtenderSeedStatus(); status.EmbeddedError != connect.ExtenderSeedCodeSignature {
		t.Errorf("the status names %+v", status)
	}
	testSeedRequireNothingApplied(t, space)
}

// A stored bundle past its validity fails the pass with an explicit freshness
// failure and is not applied; its floor stays, so the expiry reopens no replay.
func TestExtenderSeedStoredBundlePastItsExpiryFailsExplicitly(t *testing.T) {
	space, clock, dialer := newTestSeedImportSpace(t, testSeedStoragePath(t))
	seed := space.getExtenderSeed()
	if result := space.ImportExtenderSeed(testSeedText(t, testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 6}))); !result.Ok {
		t.Fatalf("the bundle: %+v", result)
	}

	clock.advance(24*time.Hour + connect.ExtenderSeedClockSkew + time.Minute)

	result := seed.awaitPass(context.Background())
	if result.usable || result.outcome != ExtenderSeedPassOutcomeFailed || result.code != connect.ExtenderSeedCodeExpired {
		t.Fatalf("the pass over an expired stored bundle: %+v", result)
	}
	if status := space.GetExtenderSeedStatus(); status.LastPassError != connect.ExtenderSeedCodeExpired {
		t.Errorf("the status after the pass is %+v", status)
	}
	if sequence, err := seed.acceptor.Sequence(testSeedNetworkHost, space.extenderDirectory.RootKeys()); err != nil || sequence != 6 {
		t.Errorf("the expiry moved the floor to %d, %v", sequence, err)
	}
	older := testSeedEnvelope(t, clock.Now(), testSeedBundleSpec{sequence: 5})
	if result := space.ImportExtenderSeed(testSeedText(t, older)); result.Error != ExtenderSeedImportErrorRollback {
		t.Errorf("a fresh bundle below the floor: %+v", result)
	}
	if dialed := dialer.dialed(); len(dialed) != 0 {
		t.Errorf("the pass dialed %v", dialed)
	}
}

// A space accepts its key host and its migration host, and each host's stored
// bundle is judged alone: the rule of connect's ApplyStoredExtenderSeed. The
// status names the bundle of the host the extender network is keyed by, the
// migration host, though the key host's comes first; and when the key host's
// expires, the other still applies and the pass has not failed.
func TestExtenderSeedEveryHostsStoredBundleIsJudgedAlone(t *testing.T) {
	const migrationHost = "migrated.example"
	clock := newTestSeedClock()
	testSeedInstallConnectSettings(t, newTestSeedDialer(), nil)
	testSeedInstallEmbedded(t, "")
	testSeedInstallSettings(t, func(settings *extenderSeedSettings) {
		settings.Now = clock.Now
	})
	values := testSeedSpaceValues()
	values.MigrationHostName = migrationHost
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	space := newNetworkSpace(ctx, NetworkSpaceKey{HostName: testSeedNetworkHost, EnvName: "main"}, values, testSeedStoragePath(t))
	t.Cleanup(space.close)
	seed := space.getExtenderSeed()

	now := clock.Now()
	keyHostBundle := testSeedEnvelope(t, now, testSeedBundleSpec{
		sequence:   3,
		issueTime:  now.Add(-time.Hour),
		expireTime: now.Add(time.Hour),
		records: []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRecordSpec{
			index:      0,
			issueTime:  now.Add(-2 * time.Hour),
			expireTime: now.Add(2 * time.Hour),
		})},
	})
	migrationHostBundle := testSeedEnvelope(t, now, testSeedBundleSpec{
		networkHost: migrationHost,
		sequence:    8,
		issueTime:   now.Add(-time.Hour),
		expireTime:  now.Add(24 * time.Hour),
		records: []*protocol.ExtenderRecord{testSeedRecord(t, testSeedRecordSpec{
			index:       1,
			networkHost: migrationHost,
			issueTime:   now.Add(-2 * time.Hour),
			expireTime:  now.Add(48 * time.Hour),
		})},
	})
	for _, envelope := range []*protocol.ExtenderSeedEnvelope{keyHostBundle, migrationHostBundle} {
		if result := space.ImportExtenderSeed(testSeedText(t, envelope)); !result.Ok {
			t.Fatalf("a bundle: %+v", result)
		}
	}

	requireStatus := func() {
		t.Helper()
		if status := space.GetExtenderSeedStatus(); status.Sequence != 8 || status.LastPassError != "" {
			t.Errorf("the status is %+v, not the migration host's bundle", status)
		}
	}
	if result := seed.awaitPass(context.Background()); result.outcome != ExtenderSeedPassOutcomeNothingNew {
		t.Errorf("the pass with both in force: %+v", result)
	}
	requireStatus()

	clock.advance(time.Hour + connect.ExtenderSeedClockSkew + time.Minute)

	if result := seed.awaitPass(context.Background()); result.outcome != ExtenderSeedPassOutcomeNothingNew {
		t.Errorf("the pass with the key host's expired: %+v", result)
	}
	requireStatus()
	rootKeys := space.extenderDirectory.RootKeys()
	if sequence, err := seed.acceptor.Sequence(testSeedNetworkHost, rootKeys); err != nil || sequence != 3 {
		t.Errorf("the key host's floor is %d, %v", sequence, err)
	}
	if sequence, err := seed.acceptor.Sequence(migrationHost, rootKeys); err != nil || sequence != 8 {
		t.Errorf("the migration host's floor is %d, %v", sequence, err)
	}
}
