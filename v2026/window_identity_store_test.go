package sdk

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func newTestIdentityStore(t *testing.T) (*localStateWindowIdentityStore, *LocalState, connect.Id) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	localState := newLocalState(ctx, t.TempDir())
	ownerClientId := connect.NewId()
	return newLocalStateWindowIdentityStore(localState, ownerClientId), localState, ownerClientId
}

func testWindowIdentity(t *testing.T) *connect.WindowClientIdentity {
	destination, err := connect.NewMultiHopId(connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	return &connect.WindowClientIdentity{
		ClientId:    connect.NewId(),
		ByJwt:       "test.jwt.value",
		InstanceId:  connect.NewId(),
		Destination: destination,
	}
}

// TestWindowIdentityStoreRoundTrip pins the persistence: identities stored
// under a (owner, specs fingerprint) scope load back identically within it,
// and never load under a different owner, fingerprint, or after the ttl.
func TestWindowIdentityStoreRoundTrip(t *testing.T) {
	store, localState, _ := newTestIdentityStore(t)
	specsAStore := store.ForSpecsFingerprint("specs-a")

	identity := testWindowIdentity(t)
	specsAStore.StoreWindowClientIdentities([]*connect.WindowClientIdentity{identity})

	loaded := specsAStore.LoadWindowClientIdentities()
	if len(loaded) != 1 {
		t.Fatalf("expected 1 identity, got %d", len(loaded))
	}
	if loaded[0].ClientId != identity.ClientId ||
		loaded[0].ByJwt != identity.ByJwt ||
		loaded[0].InstanceId != identity.InstanceId ||
		loaded[0].Destination != identity.Destination {
		t.Fatalf("round trip mismatch: %+v vs %+v", loaded[0], identity)
	}

	// a different destination fingerprint must not restore (restored
	// identities are dialed first — cross-location restore would steer the
	// connect to the wrong providers)
	if identities := store.ForSpecsFingerprint("specs-b").LoadWindowClientIdentities(); identities != nil {
		t.Fatalf("fingerprint mismatch must not load, got %v", identities)
	}

	// a different owner (new login) must not restore
	otherStore := newLocalStateWindowIdentityStore(localState, connect.NewId())
	if identities := otherStore.ForSpecsFingerprint("specs-a").LoadWindowClientIdentities(); identities != nil {
		t.Fatalf("owner mismatch must not load, got %v", identities)
	}

	// an empty store call replaces the snapshot (all identities torn down)
	specsAStore.StoreWindowClientIdentities(nil)
	if identities := specsAStore.LoadWindowClientIdentities(); 0 < len(identities) {
		t.Fatalf("emptied snapshot must not load identities, got %v", identities)
	}
}

// A destination change retires the old multi-client asynchronously while the
// replacement starts immediately. The old generator may therefore enter its
// identity-store call before the change but reach the shared store after it.
// Its snapshot must retain the OLD destination scope: relabeling that snapshot
// with the new fingerprint lets the replacement restore the same client id,
// after which old-generation cleanup deactivates the row underneath it and
// both platform auth and /connect/control return 401.
func TestWindowIdentityStoreLatePriorDestinationWriteKeepsPriorScope(t *testing.T) {
	store, _, _ := newTestIdentityStore(t)
	priorStore := store.ForSpecsFingerprint("prior-destination")
	nextStore := store.ForSpecsFingerprint("next-destination")

	priorIdentity := testWindowIdentity(t)
	latePriorWrite := func() {
		priorStore.StoreWindowClientIdentities([]*connect.WindowClientIdentity{priorIdentity})
	}

	latePriorWrite()

	if identities := nextStore.LoadWindowClientIdentities(); 0 < len(identities) {
		t.Fatalf("late prior-generation snapshot was relabeled and restored by next destination: %v", identities)
	}
}

// Persistence bridges process restarts, not two overlapping destination
// generations in one DeviceLocal. Reusing the old client id in-process lets
// predecessor cleanup revoke the replacement's server row. The first
// generation may consume the saved snapshot; every later one must start fresh
// and must ignore late writes from its predecessor.
func TestWindowIdentityStoreRestoresOnlyFirstDestinationGeneration(t *testing.T) {
	store, _, _ := newTestIdentityStore(t)
	identity := testWindowIdentity(t)
	scopedStore := store.ForSpecsFingerprint("same-destination")
	scopedStore.StoreWindowClientIdentities([]*connect.WindowClientIdentity{identity})

	generations := newWindowIdentityStoreGenerations()
	first := generations.Next(scopedStore)
	if identities := first.LoadWindowClientIdentities(); len(identities) != 1 || identities[0].ClientId != identity.ClientId {
		t.Fatalf("first destination generation did not restore prior-process identity: %v", identities)
	}

	second := generations.Next(store.ForSpecsFingerprint("same-destination"))
	if identities := second.LoadWindowClientIdentities(); len(identities) != 0 {
		t.Fatalf("second in-process generation restored predecessor identity: %v", identities)
	}

	lateIdentity := testWindowIdentity(t)
	first.StoreWindowClientIdentities([]*connect.WindowClientIdentity{lateIdentity})
	if identities := store.ForSpecsFingerprint("same-destination").LoadWindowClientIdentities(); len(identities) != 0 {
		t.Fatalf("stale first generation rewrote the replacement snapshot: %v", identities)
	}
}

// TestProviderSpecsFingerprint pins the canonical fingerprint: order
// independent, spec sensitive.
func TestProviderSpecsFingerprint(t *testing.T) {
	locationId := connect.NewId()
	clientId := connect.NewId()
	specA := &connect.ProviderSpec{LocationId: &locationId}
	specB := &connect.ProviderSpec{ClientId: &clientId}
	specC := &connect.ProviderSpec{BestAvailable: true}

	fp1 := providerSpecsFingerprint([]*connect.ProviderSpec{specA, specB})
	fp2 := providerSpecsFingerprint([]*connect.ProviderSpec{specB, specA})
	if fp1 != fp2 {
		t.Fatalf("fingerprint must be order independent")
	}
	fp3 := providerSpecsFingerprint([]*connect.ProviderSpec{specA, specC})
	if fp1 == fp3 {
		t.Fatalf("different specs must fingerprint differently")
	}
	if providerSpecsFingerprint(nil) != "" {
		t.Fatalf("empty specs must fingerprint empty")
	}
}

// TestDohServerScoresPersistence pins the score snapshot used to seed the
// resolver ordering across sessions, including the staleness cutoff.
func TestDohServerScoresPersistence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	localState := newLocalState(ctx, t.TempDir())

	if scores := localState.getDohServerScores(); scores != nil {
		t.Fatalf("no persisted scores must read nil, got %v", scores)
	}

	scores := map[string]float64{
		"https://1.1.1.1/dns-query": 7.5,
		"https://9.9.9.9/dns-query": 2.25,
	}
	if err := localState.setDohServerScores(scores); err != nil {
		t.Fatal(err)
	}
	loaded := localState.getDohServerScores()
	if len(loaded) != 2 || loaded["https://1.1.1.1/dns-query"] != 7.5 {
		t.Fatalf("score round trip mismatch: %v", loaded)
	}

	// clearing removes the snapshot
	if err := localState.setDohServerScores(nil); err != nil {
		t.Fatal(err)
	}
	if scores := localState.getDohServerScores(); scores != nil {
		t.Fatalf("cleared scores must read nil, got %v", scores)
	}
}
