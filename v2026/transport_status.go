package sdk

import "github.com/urnetwork/connect/v2026"

const (
	// Keep this untyped so gomobile exports it on every app platform.
	TransportConstraintMemory = "memory"
)

// TransportStatus is the effective runtime capability paired with a persisted
// TransportSettings policy. Settings remain the user's intent; this status
// says which of that policy's Auto modes the platform can structurally admit.
// It does not follow connectivity or temporary candidate contention.
type TransportStatus struct {
	AutoDegraded      bool        `json:"auto_degraded"`
	AutoEligibleModes *StringList `json:"auto_eligible_modes"`
	AutoConstraint    string      `json:"auto_constraint"`
}

func cloneTransportStatus(status *TransportStatus) *TransportStatus {
	if status == nil {
		return nil
	}
	clone := &TransportStatus{
		AutoDegraded:      status.AutoDegraded,
		AutoEligibleModes: NewStringList(),
		AutoConstraint:    status.AutoConstraint,
	}
	if status.AutoEligibleModes != nil {
		clone.AutoEligibleModes.addAll(status.AutoEligibleModes.getAll()...)
	}
	return clone
}

func transportStatus(settings *TransportSettings, provider bool) *TransportStatus {
	settings = normalizeTransportSettings(settings, provider)
	// Eligibility only needs the configured modes and the process transport
	// budget. Building all platform defaults here also creates TLS settings and
	// reparses the pinned CA bundle on every status poll, even though neither is
	// consulted by PlatformTransportAutoEligibility. Keep this frequent UI and
	// diagnostics path to the small structural input it actually consumes; zero
	// H1/H3 claims deliberately select that helper's memory-scaled defaults.
	platformSettings := &connect.PlatformTransportSettings{
		ModePreferences:         toConnectAutoModePreferences(settings, provider),
		PlatformTransportBudget: connect.DefaultPlatformTransportBudget(),
	}
	eligibility := connect.PlatformTransportAutoEligibility(platformSettings)

	eligibleModes := NewStringList()
	for _, item := range settings.AutoModePriorities.getAll() {
		if eligibility[toConnectTransportMode(item.Mode)] {
			eligibleModes.Add(item.Mode)
		}
	}
	degraded := eligibleModes.Len() < settings.AutoModePriorities.Len()
	constraint := ""
	if degraded {
		constraint = TransportConstraintMemory
	}
	return &TransportStatus{
		AutoDegraded:      degraded,
		AutoEligibleModes: eligibleModes,
		AutoConstraint:    constraint,
	}
}
