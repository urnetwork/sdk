package sdk

import "github.com/urnetwork/connect"

// RoutingTier is the single user-facing dial for the Phase-1 smart-routing
// knobs: off, light (pure-Go behavior), or full (light + a per-platform
// classifier installed in a later phase -- see routingTierKnobs).
//
// Off is the zero value on purpose: a client that never calls
// SetRoutingTier -- an older app build, or a fresh install before the
// persisted setting is restored -- gets exactly today's behavior, not an
// accidental opt-in.
//
// RoutingTier itself, and its three constants, are exported because the
// task interface calls for them, but they never appear in the signature of
// any exported DeviceLocal method -- SetRoutingTier takes a bare int (see
// below) -- so gomobile's binder has nothing here to generate .aar surface
// for.
type RoutingTier int

const (
	RoutingTierOff RoutingTier = iota
	RoutingTierLight
	RoutingTierFull
)

// routingTierKnobs maps a routing tier to the connect reliability knobs it
// turns on. The returned value is a PARTIAL overlay: only the five
// smart-routing fields (ScoredPlacement, RewardInstrumentation,
// PlacementHysteresisPct, PlacementDemoteConsecutive, QuarantineDampening)
// are meaningful here -- every other connect.ReliabilitySettings field is
// left at its zero value. Callers MUST merge this onto the settings already
// in effect (the constructed baseline, or the current runtime override)
// rather than applying it standalone: a standalone apply would silently
// turn off the six always-on reliability fixes that ship default-on (see
// the ReliabilitySettings field comments in reliability_controls.go). See
// applyRoutingTierToMultiClientSettings and SetRoutingTier for the two
// places this gets merged rather than applied raw.
//
// Off returns the zero-value overlay: every knob off, exactly legacy.
//
// Light turns on the pure-Go smart-routing behavior: scored placement with
// a 10% hysteresis band (a candidate must beat the current pick by more
// than 10% before displacing it, so two near-equal exits do not thrash), 3
// consecutive worse samples before a candidate is actually demoted, reward
// instrumentation for observability, and quarantine flap damping. None of
// this depends on a classifier: connect's scoredPlacementReorder classifies
// every candidate ClassUnknown until one is installed, which returns
// candidates completely untouched, so turning ScoredPlacement on by itself
// is a no-op for CLASS-aware placement today. Light's real behavior change
// is entirely the hysteresis/demote-streak/damping shape.
//
// Full is DELIBERATELY identical to Light today -- same fields, same
// values. The plan installs a classifier per platform in a later phase;
// once one is wired in, Full will additionally get class-aware placement
// while Light stays pure-Go-only, and the two will diverge. Until then
// there is nothing to tell them apart, so do NOT "simplify" this by having
// Full fall through to the Light case or by merging the two branches --
// that collapsing would have to be undone the moment a classifier lands.
//
// CAVEAT: QuarantineDampening's reconviction counter does not yet decay
// within a session (tracked separately, not fixed by this task) -- a
// channel that flaps in and out of quarantine several times in one long
// session keeps escalating toward the capped 240s hold and never relaxes
// back down until reconnect. Correct to enable per the plan; just not the
// full story yet.
//
// An unrecognized tier -- anything outside the three constants above --
// fails safe to the Off overlay rather than panicking or enabling
// anything. This path is reachable, not just defensive: SetRoutingTier
// crosses the gomobile boundary as a bare int, so a platform caller can
// pass any int32 value, and this must never crash or silently light up
// scored placement on garbage input.
func routingTierKnobs(tier int) connect.ReliabilitySettings {
	switch RoutingTier(tier) {
	case RoutingTierLight, RoutingTierFull:
		return connect.ReliabilitySettings{
			ScoredPlacement:            true,
			RewardInstrumentation:      true,
			PlacementHysteresisPct:     10,
			PlacementDemoteConsecutive: 3,
			QuarantineDampening:        true,
		}
	default:
		return connect.ReliabilitySettings{}
	}
}

// applyRoutingTierToMultiClientSettings overlays a tier's knobs onto a
// connect.MultiClientSettings in place -- used once per window build (see
// SetDestination) so the tier is part of the constructed baseline every
// fresh window starts from, including the very first window after a
// restart, before SetRoutingTier is ever called again in this process.
func applyRoutingTierToMultiClientSettings(settings *connect.MultiClientSettings, tier int) {
	overlay := routingTierKnobs(tier)
	settings.ScoredPlacement = overlay.ScoredPlacement
	settings.RewardInstrumentation = overlay.RewardInstrumentation
	settings.PlacementHysteresisPct = overlay.PlacementHysteresisPct
	settings.PlacementDemoteConsecutive = overlay.PlacementDemoteConsecutive
	settings.QuarantineDampening = overlay.QuarantineDampening
}

// applyRoutingTierToReliabilitySettings overlays a tier's knobs onto a
// gomobile-facing ReliabilitySettings in place -- used by SetRoutingTier to
// merge onto the CURRENTLY effective settings (developer-menu override or
// baseline, whichever is in force) before pushing through
// SetReliabilitySettings, so a routing-tier change never clobbers an
// unrelated field a developer already toggled.
func applyRoutingTierToReliabilitySettings(settings *ReliabilitySettings, tier int) {
	overlay := routingTierKnobs(tier)
	settings.ScoredPlacement = overlay.ScoredPlacement
	settings.RewardInstrumentation = overlay.RewardInstrumentation
	settings.PlacementHysteresisPct = overlay.PlacementHysteresisPct
	settings.PlacementDemoteConsecutive = int32(overlay.PlacementDemoteConsecutive)
	settings.QuarantineDampening = overlay.QuarantineDampening
}

// SetRoutingTier is the single tier dial for the Phase-1 smart-routing
// knobs -- off, light, or full (see RoutingTier and routingTierKnobs).
// Takes a bare int, not the typed RoutingTier: gomobile's exported surface
// only admits basic types (int/string/bool/int64), and an out-of-range
// value fails safe to Off via routingTierKnobs, never panics.
//
// Persisted the same way the performance profile is (see
// LocalState.SetPerformanceProfile / SetRoutingTier in local_state.go):
// JSON to its own dotfile under LocalState, via the device's
// AsyncLocalState so the write does not block the caller. Restored into
// self.routingTier when the device is constructed (see the LocalState
// restore block in NewDeviceLocal), so the choice survives a process
// restart even before this is called again in the new process.
//
// Applied to the live multi client immediately, through the SAME override
// path the developer-menu reliability controls use
// (DeviceLocal.SetReliabilitySettings): the tier's knobs are merged onto
// whatever is CURRENTLY effective -- preserving any developer-menu override
// already in force -- rather than replacing it outright, which would
// silently turn off the six always-on reliability fixes. Safe while
// disconnected: GetReliabilitySettings reports nil, there is nothing live
// to update, and the next window build picks up self.routingTier from the
// settings-construction overlay (see applyRoutingTierToMultiClientSettings
// in SetDestination) -- the change is not lost, only deferred until
// connect.
func (self *DeviceLocal) SetRoutingTier(tier int) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.routingTier = tier
	}()
	self.persistRoutingTier(tier)

	if effective := self.GetReliabilitySettings(); effective != nil {
		merged := *effective
		applyRoutingTierToReliabilitySettings(&merged, tier)
		self.SetReliabilitySettings(&merged)
	}
}

// persistRoutingTier writes the tier to LocalState asynchronously, mirroring
// persistBlockerEnabled's pattern: a no-op when the device has no
// AsyncLocalState (e.g. a hosted/embedded device with no local storage).
func (self *DeviceLocal) persistRoutingTier(tier int) {
	if asyncLocalState := self.networkSpace.GetAsyncLocalState(); asyncLocalState != nil {
		asyncLocalState.serialAsync(func() error {
			return asyncLocalState.GetLocalState().SetRoutingTier(tier)
		})
	}
}
