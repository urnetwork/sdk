// The lines a provider's status shows when the url checks downgraded it
// (server FP2-PROTOCOL.md §5.5, D11.6): what it is offered for, and how many
// checks took the rest away. The server tells it on the url_checks number,
// as English sentences and as a count per sentence. The decode turns the
// counts into lines each app localizes with its own strings, in one order,
// so every app tells the same thing; a reason the counts do not cover keeps
// the server's English.
package sdk

import (
	"encoding/json"
)

// ProviderStatusReasonLine.Label values.
const (
	// Checks failed only over HTTP/3 (UDP), so the provider is offered for TCP
	// traffic only. Count is the server's reason_tcp_count.
	ProviderStatusReasonLineTcp = "tcp"
	// Checks failed only over IPv6, so the provider is offered for IPv4
	// traffic only. Count is the server's reason_ipv4_count.
	ProviderStatusReasonLineIpv4 = "ipv4"
	// A reason without a count this build knows, from a newer server or one
	// that sent only the sentences. Text is the server's English.
	ProviderStatusReasonLineText = "text"
)

// One line under a ranking number.
type ProviderStatusReasonLine struct {
	// one of the ProviderStatusReasonLine* values
	Label string `json:"label"`
	// how many checks a tcp or ipv4 line counts, at least 1
	Count int `json:"count,omitempty"`
	// the server's English, for a text line
	Text string `json:"text,omitempty"`
}

// The bound form of []*ProviderStatusReasonLine, in the order the apps show them.
type ProviderStatusReasonLineList struct {
	exportedList[*ProviderStatusReasonLine]
}

// An empty list.
func NewProviderStatusReasonLineList() *ProviderStatusReasonLineList {
	return &ProviderStatusReasonLineList{
		exportedList: *newExportedList[*ProviderStatusReasonLine](),
	}
}

// Decodes the server's fields, then derives ReasonLines from them: the tcp
// line, then the ipv4 line, each when the server counted checks for it,
// which is the order of the server's sentences. With neither count, a
// non-empty reason is one text line, so a sentence this build has no string
// for still shows; with no reason there are no lines. Every status reaches an
// app through this decode: the view controller's poll and GetProviderStatus,
// whose answers the c abi hands the desktop apps as json.
func (self *ProviderRankingNumber) UnmarshalJSON(data []byte) error {
	// the same fields without this method, so the decode does not recurse
	type providerRankingNumberFields ProviderRankingNumber
	number := providerRankingNumberFields{}
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	*self = ProviderRankingNumber(number)

	lines := NewProviderStatusReasonLineList()
	if 0 < self.ReasonTcpCount {
		lines.Add(&ProviderStatusReasonLine{
			Label: ProviderStatusReasonLineTcp,
			Count: self.ReasonTcpCount,
		})
	}
	if 0 < self.ReasonIpv4Count {
		lines.Add(&ProviderStatusReasonLine{
			Label: ProviderStatusReasonLineIpv4,
			Count: self.ReasonIpv4Count,
		})
	}
	if lines.Len() == 0 && self.Reason != "" {
		lines.Add(&ProviderStatusReasonLine{
			Label: ProviderStatusReasonLineText,
			Text:  self.Reason,
		})
	}
	self.ReasonLines = lines
	return nil
}
