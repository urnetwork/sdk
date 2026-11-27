// The memory soak must explain every compact-ABI refusal without tying the
// number of redundant TCP packets, or of answers to a completed flow's late
// packets, to the unrelated blocked UDP workload.
package sdk

import "testing"

// Explicit counts reproduce the old false failures and hidden packet loss.
func TestSyntheticPacketRejectionsRequireExactAccounting(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		rejected   int64
		blocked    int64
		collapsed  int64
		generation int64
		wantError  bool
	}{
		{name: "no refusals"},
		{name: "policy only", rejected: 4, blocked: 4},
		{name: "duplicate tcp only", rejected: 16, collapsed: 16},
		{name: "flow generation only", rejected: 1, generation: 1},
		{name: "independent policy and duplicate tcp", rejected: 20, blocked: 4, collapsed: 16},
		{name: "independent policy, duplicate tcp and flow generation", rejected: 22, blocked: 4, collapsed: 16, generation: 2},
		// tip-check at sidecar aa0f5910: the device's reset to a completed
		// flow's repeated ACK, which nothing counted before flow generation drops
		{name: "the soak's refusal with no reason", rejected: 381, blocked: 380, wantError: true},
		{name: "the same refusal named by its flow generation", rejected: 381, blocked: 380, generation: 1},
		{name: "one unexplained refusal", rejected: 5, blocked: 4, wantError: true},
		{name: "one unexplained refusal beside duplicates", rejected: 21, blocked: 4, collapsed: 16, wantError: true},
		{name: "one unexplained refusal beside a flow generation", rejected: 22, blocked: 4, collapsed: 16, generation: 1, wantError: true},
		{name: "one missing refusal", rejected: 19, blocked: 4, collapsed: 16, wantError: true},
		{name: "one missing refusal beside a flow generation", rejected: 20, blocked: 4, collapsed: 16, generation: 1, wantError: true},
	} {
		err := syntheticPacketRejectionError(testCase.rejected, testCase.blocked, testCase.collapsed, testCase.generation)
		if (err != nil) != testCase.wantError {
			t.Errorf("%s: error=%v, want error=%t", testCase.name, err, testCase.wantError)
		}
	}
}
