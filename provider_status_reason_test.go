// The url_checks reason of a downgraded provider (server FP2-PROTOCOL.md
// §5.5): the wire fields decode, and the decode derives the lines the apps
// show, in order, for every combination of the server's counts.
package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/urnetwork/connect"
)

// The server's two sentences, as FP2-PROTOCOL.md §5.5 words them.
const providerStatusReasonTestTcp = "3 of these checks failed only over HTTP/3 (UDP), so this device is offered for TCP traffic only. Some app builds and some networks do not route UDP."
const providerStatusReasonTestIpv4 = "2 of these checks failed only over IPv6, so this device is offered for IPv4 traffic only."

// A url_checks number as the server marshals it, with the reason fields
// given as raw json members (empty for none).
func providerStatusReasonTestNumberJson(reasonMembers string) string {
	explanation := "Test sites that loaded through this connection in the last 8 hours: 13 of 20. At least 4 in 5 must load; filtering by your internet provider lowers it."
	if reasonMembers != "" {
		reasonMembers = ", " + reasonMembers
	}
	return fmt.Sprintf(
		`{"name": "url_checks", "has_value": true, "value": 0.65, "has_minimum": true, "minimum": 0.8, "has_maximum": false, "maximum": 0, "passes": false, "count": 13, "total": 20, "explanation": %q%s}`,
		explanation,
		reasonMembers,
	)
}

// The lines of a decoded number, as plain values.
func providerStatusReasonTestLines(t *testing.T, number *ProviderRankingNumber) []ProviderStatusReasonLine {
	t.Helper()
	if number.ReasonLines == nil {
		t.Fatal("decode left no reason lines")
	}
	lines := []ProviderStatusReasonLine{}
	for i := 0; i < number.ReasonLines.Len(); i += 1 {
		lines = append(lines, *number.ReasonLines.Get(i))
	}
	return lines
}

// Each combination of the server's reason and counts decodes to the lines
// the apps show: tcp before ipv4, each only with its count, and the English
// only when no count covers the reason.
func TestProviderRankingNumberReasonLines(t *testing.T) {
	both := providerStatusReasonTestTcp + " " + providerStatusReasonTestIpv4
	cases := []struct {
		name          string
		members       string
		reason        string
		tcpCount      int
		ipv4Count     int
		expectedLines []ProviderStatusReasonLine
	}{
		{
			name:     "tcp",
			members:  fmt.Sprintf(`"reason": %q, "reason_tcp_count": 3`, providerStatusReasonTestTcp),
			reason:   providerStatusReasonTestTcp,
			tcpCount: 3,
			expectedLines: []ProviderStatusReasonLine{
				{Label: ProviderStatusReasonLineTcp, Count: 3},
			},
		},
		{
			name:      "ipv4",
			members:   fmt.Sprintf(`"reason": %q, "reason_ipv4_count": 2`, providerStatusReasonTestIpv4),
			reason:    providerStatusReasonTestIpv4,
			ipv4Count: 2,
			expectedLines: []ProviderStatusReasonLine{
				{Label: ProviderStatusReasonLineIpv4, Count: 2},
			},
		},
		{
			// the server's order, whichever order the members arrive in
			name:      "both",
			members:   fmt.Sprintf(`"reason_ipv4_count": 2, "reason": %q, "reason_tcp_count": 3`, both),
			reason:    both,
			tcpCount:  3,
			ipv4Count: 2,
			expectedLines: []ProviderStatusReasonLine{
				{Label: ProviderStatusReasonLineTcp, Count: 3},
				{Label: ProviderStatusReasonLineIpv4, Count: 2},
			},
		},
		{
			// a sentence this build has no count for keeps the server's English
			name:    "reason without counts",
			members: `"reason": "A newer server's sentence."`,
			reason:  "A newer server's sentence.",
			expectedLines: []ProviderStatusReasonLine{
				{Label: ProviderStatusReasonLineText, Text: "A newer server's sentence."},
			},
		},
		{
			// the compact form carries the counts without the sentences
			name:      "counts without reason",
			members:   `"reason_tcp_count": 1, "reason_ipv4_count": 4`,
			tcpCount:  1,
			ipv4Count: 4,
			expectedLines: []ProviderStatusReasonLine{
				{Label: ProviderStatusReasonLineTcp, Count: 1},
				{Label: ProviderStatusReasonLineIpv4, Count: 4},
			},
		},
		{
			// a provider that is not downgraded, and every server before the fields
			name:          "neither",
			members:       "",
			expectedLines: []ProviderStatusReasonLine{},
		},
		{
			// the lines are the decode's, never the wire's
			name:          "lines on the wire",
			members:       `"reason_lines": [{"label": "tcp", "count": 9}]`,
			expectedLines: []ProviderStatusReasonLine{},
		},
		{
			// a count below 1 counts nothing
			name:      "non-positive counts",
			members:   fmt.Sprintf(`"reason": %q, "reason_tcp_count": -1, "reason_ipv4_count": 0`, providerStatusReasonTestTcp),
			reason:    providerStatusReasonTestTcp,
			tcpCount:  -1,
			ipv4Count: 0,
			expectedLines: []ProviderStatusReasonLine{
				{Label: ProviderStatusReasonLineText, Text: providerStatusReasonTestTcp},
			},
		},
	}
	for _, c := range cases {
		number := &ProviderRankingNumber{}
		if err := json.Unmarshal([]byte(providerStatusReasonTestNumberJson(c.members)), number); err != nil {
			t.Fatalf("%s: %s", c.name, err)
		}
		if number.Reason != c.reason || number.ReasonTcpCount != c.tcpCount || number.ReasonIpv4Count != c.ipv4Count {
			t.Errorf("%s: reason fields = %q, %d, %d", c.name, number.Reason, number.ReasonTcpCount, number.ReasonIpv4Count)
		}
		// the fields every app already reads decode as before
		if number.Name != ProviderStatusNumberUrlChecks || number.Count != 13 || number.Total != 20 ||
			number.Passes || !number.HasMinimum || number.Minimum != 0.8 || number.Explanation == "" {
			t.Errorf("%s: number = %+v", c.name, number)
		}
		if lines := providerStatusReasonTestLines(t, number); !reflect.DeepEqual(lines, c.expectedLines) {
			t.Errorf("%s: lines = %+v, want %+v", c.name, lines, c.expectedLines)
		}
	}
}

// The lines reach the apps through GetProviderStatus: each number of the
// ranking list is decoded with its own lines.
func TestGetProviderStatusDecodesReasonLines(t *testing.T) {
	both := providerStatusReasonTestTcp + " " + providerStatusReasonTestIpv4
	body := fmt.Sprintf(`{"providers": [{
		"client_id": %q,
		"reason": "none",
		"reason_text": "Everything checks out.",
		"ranking": [
			{"name": "reliability_1h", "has_value": true, "value": 0.99, "has_minimum": true, "minimum": 0.95, "passes": true, "explanation": "How steadily this device stayed connected over the last hour: 0.99."},
			%s
		]
	}]}`, providerStatusTestOwnClientId, providerStatusReasonTestNumberJson(fmt.Sprintf(`"reason": %q, "reason_tcp_count": 3, "reason_ipv4_count": 2`, both)))
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))

	callback, c := connect.NewBlockingApiCallback[*GetProviderStatusResult](context.Background())
	api.GetProviderStatus(callback)
	r := awaitApiResult(t, c, "GetProviderStatus never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	ranking := r.Result.Providers.Get(0).Ranking
	if ranking == nil || ranking.Len() != 2 {
		t.Fatalf("ranking = %+v", ranking)
	}
	if lines := providerStatusReasonTestLines(t, ranking.Get(0)); len(lines) != 0 {
		t.Fatalf("reliability lines = %+v", lines)
	}
	expectedLines := []ProviderStatusReasonLine{
		{Label: ProviderStatusReasonLineTcp, Count: 3},
		{Label: ProviderStatusReasonLineIpv4, Count: 2},
	}
	if lines := providerStatusReasonTestLines(t, ranking.Get(1)); !reflect.DeepEqual(lines, expectedLines) {
		t.Fatalf("url_checks lines = %+v, want %+v", lines, expectedLines)
	}
}

// The c abi hands the desktop apps the decoded number as json, so the lines
// travel with the fields they were derived from.
func TestProviderRankingNumberReasonLinesMarshal(t *testing.T) {
	number := &ProviderRankingNumber{}
	members := fmt.Sprintf(`"reason": %q, "reason_tcp_count": 3`, providerStatusReasonTestTcp)
	if err := json.Unmarshal([]byte(providerStatusReasonTestNumberJson(members)), number); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(number)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	expectedLines := []any{map[string]any{"label": ProviderStatusReasonLineTcp, "count": float64(3)}}
	if !reflect.DeepEqual(fields["reason_lines"], expectedLines) || fields["reason"] != providerStatusReasonTestTcp ||
		fields["reason_tcp_count"] != float64(3) {
		t.Fatalf("json = %s", data)
	}
	if _, ok := fields["reason_ipv4_count"]; ok {
		t.Fatalf("an absent count is on the wire: %s", data)
	}
	// a number without a reason marshals no reason members but its empty lines
	plain := &ProviderRankingNumber{}
	if err := json.Unmarshal([]byte(providerStatusReasonTestNumberJson("")), plain); err != nil {
		t.Fatal(err)
	}
	plainData, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	plainFields := map[string]any{}
	if err := json.Unmarshal(plainData, &plainFields); err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{"reason", "reason_tcp_count", "reason_ipv4_count"} {
		if _, ok := plainFields[member]; ok {
			t.Fatalf("%s on the wire without a reason: %s", member, plainData)
		}
	}
	if !reflect.DeepEqual(plainFields["reason_lines"], []any{}) {
		t.Fatalf("lines without a reason = %s", plainData)
	}
}
