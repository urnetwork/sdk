package sdk

import (
	"fmt"

	"github.com/urnetwork/glog/v2026"
)

// Records one bounded native recovery transition in the existing SDK log and
// returns the same secret-free line for the platform logger. Callers deduplicate
// identical stage/state tuples within their lifecycle; never call per packet.
// This is observation only: intended connection is separate from an actual
// consumer, and provider count is not a claim of DNS or transport health.
// Pass -1 when no provider observation exists; zero means a measured empty set.
func RecordTunnelRecoveryStage(stage string, result string, intended bool, consumerPresent bool, hasLocation bool, providerCount int64, generation int64) string {
	switch stage {
	case "auth-observation", "auth-reset", "key-load", "intent-load", "saved-load", "default-load",
		"destination", "destination-persist", "default-persist", "consumer", "rpc", "readiness",
		"settings", "wake", "path-recovery", "dns", "startup", "stop", "preferences-load", "auto-save", "destination-apply", "default-apply":
	default:
		stage = "unknown"
	}
	switch result {
	case "started", "present", "missing", "accepted", "preserved", "completed", "superseded", "failed",
		"applied", "local", "establishing", "connected", "retry", "empty-window", "not-needed", "restored", "owned", "unowned",
		"user-disabled", "failure", "other", "skipped-unowned", "enabled", "disabled", "waiting", "timeout":
	default:
		result = "unknown"
	}
	line := fmt.Sprintf("[recovery] stage=%s result=%s intended=%t consumer_present=%t has_location=%t providers=%d generation=%d",
		stage, result, intended, consumerPresent, hasLocation,
		min(max(providerCount, -1), 1_000_000), min(max(generation, 0), 1_000_000_000))
	glog.Info(line)
	return line
}
