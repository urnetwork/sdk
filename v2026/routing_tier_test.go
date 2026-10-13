package sdk

import (
	"context"
	"testing"
)

// TestRoutingTierMapsToKnobs is the brief's Step-1 smoke test: Off is fully
// legacy, Light turns on scored placement with the documented hysteresis
// and demote shape.
func TestRoutingTierMapsToKnobs(t *testing.T) {
	off := routingTierKnobs(int(RoutingTierOff))
	if off.ScoredPlacement || off.RewardInstrumentation {
		t.Fatal("Off tier must be fully legacy")
	}
	light := routingTierKnobs(int(RoutingTierLight))
	if !light.ScoredPlacement || light.PlacementHysteresisPct != 10 || light.PlacementDemoteConsecutive != 3 {
		t.Fatalf("Light tier knobs wrong: %+v", light)
	}
}

// TestRoutingTierKnobsEveryField asserts every field of the overlay
// individually, for all three tiers -- a struct-equality-only check could
// pass with, say, QuarantineDampening silently missing from Light as long
// as the other four fields happened to be right.
func TestRoutingTierKnobsEveryField(t *testing.T) {
	off := routingTierKnobs(int(RoutingTierOff))
	if off.ScoredPlacement != false {
		t.Errorf("Off ScoredPlacement = %v, want false", off.ScoredPlacement)
	}
	if off.RewardInstrumentation != false {
		t.Errorf("Off RewardInstrumentation = %v, want false", off.RewardInstrumentation)
	}
	if off.PlacementHysteresisPct != 0 {
		t.Errorf("Off PlacementHysteresisPct = %v, want 0", off.PlacementHysteresisPct)
	}
	if off.PlacementDemoteConsecutive != 0 {
		t.Errorf("Off PlacementDemoteConsecutive = %v, want 0", off.PlacementDemoteConsecutive)
	}
	if off.QuarantineDampening != false {
		t.Errorf("Off QuarantineDampening = %v, want false", off.QuarantineDampening)
	}

	// Full is deliberately identical to Light today (see routingTierKnobs'
	// doc comment) -- both are checked against the exact same expectations
	// so a future accidental divergence in either direction fails here.
	for _, tc := range []struct {
		name string
		tier int
	}{
		{"Light", int(RoutingTierLight)},
		{"Full", int(RoutingTierFull)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			knobs := routingTierKnobs(tc.tier)
			if knobs.ScoredPlacement != true {
				t.Errorf("%s ScoredPlacement = %v, want true", tc.name, knobs.ScoredPlacement)
			}
			if knobs.RewardInstrumentation != true {
				t.Errorf("%s RewardInstrumentation = %v, want true", tc.name, knobs.RewardInstrumentation)
			}
			if knobs.PlacementHysteresisPct != 10 {
				t.Errorf("%s PlacementHysteresisPct = %v, want 10", tc.name, knobs.PlacementHysteresisPct)
			}
			if knobs.PlacementDemoteConsecutive != 3 {
				t.Errorf("%s PlacementDemoteConsecutive = %v, want 3", tc.name, knobs.PlacementDemoteConsecutive)
			}
			if knobs.QuarantineDampening != true {
				t.Errorf("%s QuarantineDampening = %v, want true", tc.name, knobs.QuarantineDampening)
			}
		})
	}
}

// TestRoutingTierUnknownFailsSafeToOff proves an out-of-range int -- the
// real shape a gomobile caller can pass, since SetRoutingTier crosses the
// boundary as a bare int rather than the typed RoutingTier -- maps to the
// Off overlay instead of panicking or silently enabling anything.
func TestRoutingTierUnknownFailsSafeToOff(t *testing.T) {
	for _, tier := range []int{-1, 3, 99, 1 << 20} {
		knobs := routingTierKnobs(tier)
		if knobs.ScoredPlacement || knobs.RewardInstrumentation || knobs.QuarantineDampening ||
			knobs.PlacementHysteresisPct != 0 || knobs.PlacementDemoteConsecutive != 0 {
			t.Fatalf("tier %d did not fail safe to Off: %+v", tier, knobs)
		}
	}
}

// TestRoutingTierPersistsAcrossLocalStateReload round-trips a tier through
// LocalState the same way local_state_performance_profile_test.go exercises
// the performance profile: set, then load a FRESH LocalState rooted at the
// same temp dir (simulating a process restart) and confirm the tier comes
// back unchanged.
func TestRoutingTierPersistsAcrossLocalStateReload(t *testing.T) {
	dir := t.TempDir()
	localState := newLocalState(context.Background(), dir)
	if err := localState.SetRoutingTier(int(RoutingTierFull)); err != nil {
		t.Fatalf("set: %v", err)
	}

	reloaded := newLocalState(context.Background(), dir)
	if got := reloaded.GetRoutingTier(); got != int(RoutingTierFull) {
		t.Fatalf("reloaded tier = %d, want %d (RoutingTierFull)", got, int(RoutingTierFull))
	}
}

// TestRoutingTierUnsetDefaultsToOff confirms a LocalState with nothing
// written yet -- the fresh-install case -- reads back Off, not a decode
// error masquerading as some other value.
func TestRoutingTierUnsetDefaultsToOff(t *testing.T) {
	localState := newLocalState(context.Background(), t.TempDir())
	if got := localState.GetRoutingTier(); got != int(RoutingTierOff) {
		t.Fatalf("unset tier = %d, want Off (%d)", got, int(RoutingTierOff))
	}
}
