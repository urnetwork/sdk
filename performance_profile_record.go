// The saved performance profile record, which the app's local state
// (LocalState.SetPerformanceProfile) and the device's preference catalog
// ("performance-profile") both write as `.performance_profile`.
package sdk

import (
	"encoding/json"
)

// The record carries the post-quantum default version. Post-quantum
// encryption used to be off by default, and every app saves the whole profile
// when any connect option changes, so an unversioned record's
// PostQuantumEncryption=false cannot be told apart from the old default
// carried along. An unversioned record therefore loads with the current
// default (DefaultPerformanceProfile: on), keeping its other settings; a
// record written at this version keeps the user's choice (owner, 2026-11-24,
// the rule the dns fallback opt-in took on 2026-10-02,
// dnsResolverSettingsFallbackOptInVersion).
const performanceProfilePostQuantumDefaultVersion = 1

type performanceProfileRecord struct {
	PerformanceProfile
	PostQuantumDefaultVersion int `json:",omitempty"`
}

func encodePerformanceProfileRecord(performanceProfile *PerformanceProfile) ([]byte, error) {
	return json.Marshal(&performanceProfileRecord{
		PerformanceProfile:        *performanceProfile,
		PostQuantumDefaultVersion: performanceProfilePostQuantumDefaultVersion,
	})
}

// Applies the post-quantum default to a record an older sdk wrote, then the
// saved-profile normalization (normalizeSavedPerformanceProfile). Reading
// never writes: an unversioned record reads with the default until the next
// save stamps the version, and from then on the saved choice is kept, so the
// migration happens once.
func decodePerformanceProfileRecord(data []byte) (*PerformanceProfile, error) {
	var record performanceProfileRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	performanceProfile := record.PerformanceProfile
	if record.PostQuantumDefaultVersion < performanceProfilePostQuantumDefaultVersion {
		performanceProfile.PostQuantumEncryption = DefaultPerformanceProfile().PostQuantumEncryption
	}
	return normalizeSavedPerformanceProfile(&performanceProfile), nil
}
