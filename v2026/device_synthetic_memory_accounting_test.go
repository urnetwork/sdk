// The memory soak must explain every compact-ABI refusal without tying the
// number of redundant TCP packets to the unrelated blocked UDP workload.
package sdk

import "testing"

// Explicit counts reproduce both the old false failure and hidden packet loss.
func TestSyntheticPacketRejectionsRequireExactAccounting(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		rejected  int64
		blocked   int64
		collapsed int64
		wantError bool
	}{
		{name: "no refusals"},
		{name: "policy only", rejected: 4, blocked: 4},
		{name: "duplicate tcp only", rejected: 16, collapsed: 16},
		{name: "independent policy and duplicate tcp", rejected: 20, blocked: 4, collapsed: 16},
		{name: "one unexplained refusal", rejected: 5, blocked: 4, wantError: true},
		{name: "one unexplained refusal beside duplicates", rejected: 21, blocked: 4, collapsed: 16, wantError: true},
		{name: "one missing refusal", rejected: 19, blocked: 4, collapsed: 16, wantError: true},
	} {
		err := syntheticPacketRejectionError(testCase.rejected, testCase.blocked, testCase.collapsed)
		if (err != nil) != testCase.wantError {
			t.Errorf("%s: error=%v, want error=%t", testCase.name, err, testCase.wantError)
		}
	}
}
