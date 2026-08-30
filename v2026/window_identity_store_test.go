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
	store.SetSpecsFingerprint("specs-a")

	identity := testWindowIdentity(t)
	store.StoreWindowClientIdentities([]*connect.WindowClientIdentity{identity})

	loaded := store.LoadWindowClientIdentities()
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
	store.SetSpecsFingerprint("specs-b")
	if identities := store.LoadWindowClientIdentities(); identities != nil {
		t.Fatalf("fingerprint mismatch must not load, got %v", identities)
	}
	store.SetSpecsFingerprint("specs-a")

	// a different owner (new login) must not restore
	otherStore := newLocalStateWindowIdentityStore(localState, connect.NewId())
	otherStore.SetSpecsFingerprint("specs-a")
	if identities := otherStore.LoadWindowClientIdentities(); identities != nil {
		t.Fatalf("owner mismatch must not load, got %v", identities)
	}

	// an empty store call replaces the snapshot (all identities torn down)
	store.StoreWindowClientIdentities(nil)
	if identities := store.LoadWindowClientIdentities(); 0 < len(identities) {
		t.Fatalf("emptied snapshot must not load identities, got %v", identities)
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
