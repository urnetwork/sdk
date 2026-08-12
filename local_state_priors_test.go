package sdk

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// newTestPriorsLocalState builds a LocalState rooted in a fresh t.TempDir(),
// the same temp-dir pattern newTestIdentityStore uses (window_identity_store_test.go).
func newTestPriorsLocalState(t *testing.T) *LocalState {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newLocalState(ctx, t.TempDir())
}

// TestProviderPriorsDotFileRoundTrip pins the basic persistence: what is set
// is what comes back, field for field.
func TestProviderPriorsDotFileRoundTrip(t *testing.T) {
	ls := newTestPriorsLocalState(t)

	if out := ls.getProviderPriors(); out != nil {
		t.Fatalf("no persisted priors must read nil, got %v", out)
	}

	in := map[string]connect.ProviderPrior{
		"p1": {ScoreEwma: 0.8, Convictions: 1, LastSeenUnix: 1000},
		"p2": {ScoreEwma: 0.35, Convictions: 3, LastSeenUnix: 2000},
	}
	if err := ls.setProviderPriors(in); err != nil {
		t.Fatal(err)
	}

	out := ls.getProviderPriors()
	if len(out) != 2 {
		t.Fatalf("round trip lost entries: got %d, want 2: %+v", len(out), out)
	}
	if out["p1"].ScoreEwma != 0.8 || out["p1"].Convictions != 1 || out["p1"].LastSeenUnix != 1000 {
		t.Fatalf("round trip mismatch for p1: %+v", out["p1"])
	}
	if out["p2"].ScoreEwma != 0.35 || out["p2"].Convictions != 3 || out["p2"].LastSeenUnix != 2000 {
		t.Fatalf("round trip mismatch for p2: %+v", out["p2"])
	}
}

// TestProviderPriorsRetentionStaleness pins the staleness cutoff: an
// envelope older than its own stamped Retention reads back nil, but the
// SAME envelope with Retention == 0 (unlimited) loads regardless of age.
func TestProviderPriorsRetentionStaleness(t *testing.T) {
	ls := newTestPriorsLocalState(t)
	path := filepath.Join(ls.localStorageDir, ".provider_priors")

	priors := map[string]connect.ProviderPrior{"p1": {ScoreEwma: 0.9, Convictions: 0, LastSeenUnix: 42}}

	writeEnvelope := func(retention time.Duration, age time.Duration) {
		envelope := persistedProviderPriors{
			SavedAt:   time.Now().Add(-age),
			Retention: retention,
			Priors:    priors,
		}
		envelopeBytes, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, envelopeBytes, LocalStorageFilePermissions); err != nil {
			t.Fatal(err)
		}
	}

	// bounded retention (1 hour), aged past it (2 hours old) -> stale, nil
	writeEnvelope(time.Hour, 2*time.Hour)
	if out := ls.getProviderPriors(); out != nil {
		t.Fatalf("envelope older than its retention must read nil, got %v", out)
	}

	// bounded retention (1 hour), aged within it (10 minutes old) -> loads
	writeEnvelope(time.Hour, 10*time.Minute)
	if out := ls.getProviderPriors(); len(out) != 1 || out["p1"].ScoreEwma != 0.9 {
		t.Fatalf("envelope within its retention must load, got %v", out)
	}

	// SAME age (2 hours, which was stale above), but Retention == 0
	// (unlimited) -> loads. This is the "settable to unlimited" requirement:
	// the exact envelope that was rejected above must now succeed once
	// Retention is 0, proving the zero check actually short-circuits the
	// staleness comparison rather than happening to pass some other way.
	writeEnvelope(0, 2*time.Hour)
	out := ls.getProviderPriors()
	if len(out) != 1 || out["p1"].ScoreEwma != 0.9 || out["p1"].LastSeenUnix != 42 {
		t.Fatalf("unlimited retention (0) must load an old envelope, got %v", out)
	}
}

// TestProviderPriorsCorruptionTolerance pins that a missing file and
// malformed json both degrade to nil rather than panicking or erroring the
// caller -- callers treat nil as "nothing to seed with", not an error state.
func TestProviderPriorsCorruptionTolerance(t *testing.T) {
	ls := newTestPriorsLocalState(t)
	path := filepath.Join(ls.localStorageDir, ".provider_priors")

	// missing file
	if out := ls.getProviderPriors(); out != nil {
		t.Fatalf("missing file must read nil, got %v", out)
	}

	// malformed json
	if err := os.WriteFile(path, []byte("{not valid json"), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	if out := ls.getProviderPriors(); out != nil {
		t.Fatalf("malformed json must read nil, got %v", out)
	}
}

// TestProviderPriorsEmptySaveRemoves pins that saving nil or an empty map
// removes any previously persisted snapshot, so a caller with nothing left
// to persist (e.g. every prior forgotten) can clear the file the same way
// GetDohServerScores/SetDohServerScores does.
func TestProviderPriorsEmptySaveRemoves(t *testing.T) {
	ls := newTestPriorsLocalState(t)
	path := filepath.Join(ls.localStorageDir, ".provider_priors")

	if err := ls.setProviderPriors(map[string]connect.ProviderPrior{"p1": {ScoreEwma: 0.5}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file to exist after a non-empty save: %v", err)
	}

	if err := ls.setProviderPriors(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected file to be removed after a nil save, stat err = %v", err)
	}
	if out := ls.getProviderPriors(); out != nil {
		t.Fatalf("cleared priors must read nil, got %v", out)
	}
}

// TestLocalStatePriorsStoreRoundTrip pins the connect.PriorsStore adapter:
// Save then Load returns the same data through the store, not just through
// the underlying LocalState methods directly.
func TestLocalStatePriorsStoreRoundTrip(t *testing.T) {
	ls := newTestPriorsLocalState(t)
	store := newLocalStatePriorsStore(ls)

	if out := store.Load(); out != nil {
		t.Fatalf("empty store must Load nil, got %v", out)
	}

	in := map[string]connect.ProviderPrior{"p9": {ScoreEwma: 0.6, Convictions: 2, LastSeenUnix: 555}}
	if err := store.Save(in); err != nil {
		t.Fatal(err)
	}
	out := store.Load()
	if len(out) != 1 || out["p9"].ScoreEwma != 0.6 || out["p9"].Convictions != 2 || out["p9"].LastSeenUnix != 555 {
		t.Fatalf("store round trip mismatch: %+v", out)
	}
}
