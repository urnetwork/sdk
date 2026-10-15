package sdk

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// The pieces the ipv6 pattern is assembled from: one hex group, the dotted
// quad an ipv4-mapped literal ends with, either of those, and an optional
// zone.
const (
	addrGroup = `[0-9a-fA-F]{1,4}`
	addrQuad  = `\d{1,3}(?:\.\d{1,3}){3}`
	addrPart  = `(?:` + addrQuad + `|` + addrGroup + `)`
	addrZone  = `(?:%[0-9a-zA-Z._-]{1,16})?`
)

// The patterns the redactor rewrites. Everything else in a line -- timestamps,
// the file:line header, component tags, counters, message text -- is left
// exactly as written, so a redacted bundle is still readable as a log.
var (
	// dotted-quad with an optional :port
	redactIPv4Pattern = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b(?::\d{1,5})?`)
	// uuid, the shape of client, network, device and instance ids
	redactUUIDPattern = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	// ipv6, bracketed with an optional :port or bare, with an optional zone
	// and an optional trailing dotted quad for the ipv4-mapped forms.
	//
	// The pattern is loose in the MIDDLE and strict at the EDGES, and the
	// split is what makes both halves of the job hold at once.
	//
	// Loose in the middle: a bare candidate is any run of hex groups joined by
	// ':' or '::', down to two groups, and isAddrLiteral decides what is
	// really an address. That is what admits the compressed literals
	// netip.Addr.String() prints -- 2001::1, fd00::1234, fe80::1, ::1 -- which
	// an older three-colon floor missed entirely. Parsing the candidate
	// protects a glog HH:MM:SS timestamp exactly, since 12:34:56 is not an
	// address, and it is also what keeps a bracketed counter like [10] or [42]
	// from being rewritten as one.
	//
	// Strict at the edges: a bare candidate begins at a word boundary on a hex
	// group, or on the '::' of a leading compression, and it ends on a group,
	// on a dotted quad, or on the '::' of a trailing compression -- never on a
	// bare ':'. An edge that over-matches is unrecoverable in a way a middle
	// that over-matches is not: swallowing the ':' beside an address makes the
	// whole span unparseable, and a rejected span is skipped whole, so the
	// address inside it goes out in the clear. That is what a pattern without
	// these anchors did to "dial 2001:db8::1: connection refused" and
	// "{Ip:2001:db8::1 Port:443}", the two commonest address shapes in a Go
	// network log.
	redactIPv6Pattern = regexp.MustCompile(
		`\[[0-9a-fA-F:.]{2,45}` + addrZone + `\](?::\d{1,5})?` +
			`|\b` + addrGroup + `(?:(?::{1,2}` + addrPart + `){1,7}(?:::|\b)|::)` + addrZone +
			`|::` + addrPart + `(?::{1,2}` + addrPart + `){0,6}\b` + addrZone)

	// the byte-slice renderings, which carry neither a dot nor a colon and so
	// are invisible to both patterns above.
	//
	// fmt prints an address as a bracketed list of space-separated decimal
	// BYTES whenever it cannot reach a String method: a [4]byte or [16]byte
	// field (connect.Ip4Path and connect.Ip6Path hold their source and
	// destination in exactly those), a []byte, or a net.IP in an unexported
	// field, whose method fmt may not call. A REDACTED bundle exported from a
	// real device carried 25 distinct destination addresses in this form,
	// hundreds of times each, while every dotted-quad rendering of the same
	// addresses was masked and the bundle's own README asserted that addresses
	// were replaced by tokens.
	//
	// What was verified, by printing this tree's own types:
	//
	//	Ip4Path           %v  {tcp [0 0 0 0] 0 [17 23 18 34] 443 }
	//	Ip6Path           %v  {tcp [0 ...] 0 [32 1 13 184 0 0 0 0 0 0 0 0 0 0 0 1] 443 }
	//	struct{ip net.IP} %v  {[0 0 0 0 0 0 0 0 0 0 255 255 17 23 18 34] 443}
	//
	// So the ipv6 rendering is SIXTEEN groups and not eight: net.IP, [16]byte
	// and their kin hold one BYTE per element, not one hex group. The third
	// line is why sixteen groups are not an ipv6-only concern -- net.ParseIP
	// returns the 16-byte representation for an ipv4 address too, so an ipv4
	// destination can reach the log as a 16-group v4-mapped list, which
	// canonicalAddr unmaps so it tokenises as the ipv4 address it is.
	//
	// Loose in the middle like the pattern above, and strict at BOTH edges
	// unlike it: four to sixteen groups are offered and only four and sixteen
	// can parse, so a five-element byte slice is matched and handed straight
	// back untouched. Looseness costs nothing here because the brackets make
	// the span exact -- unlike a bare ipv6 candidate this one cannot swallow
	// the character beside an address, so a rejected span provably holds no
	// address rendering inside it and the fail-safe search does not apply.
	//
	// One consequence worth naming: ANY sixteen bytes are a valid ipv6
	// address, so a sixteen-byte identifier printed this way -- a connect.Id
	// in an unexported field, say, which fmt cannot call String on either --
	// is masked as an address rather than as an id. That is the safe
	// direction, and a bundle where an id reads as <addr:...> is a labelling
	// wart, not a leak.
	redactAddrBytesPattern = regexp.MustCompile(`\[\d{1,3}(?: \d{1,3}){3,15}\]`)
)

// matchAddr resolves one candidate match to the address it renders, in any of
// the forms the patterns can hand it: bare, bracketed, bracketed with a port,
// a dotted quad with a port, or a bracketed list of decimal bytes.
//
// This is the guard that lets the patterns be generous. Timestamps, bracketed
// counters, hex-looking tags and byte slices that are not addresses reach it
// and are rejected, so timestamps, component tags, counters and message
// structure survive verbatim.
func matchAddr(match string) (netip.Addr, bool) {
	if addr, ok := addrFromByteList(match); ok {
		return addr, true
	}
	host := match
	if strings.HasPrefix(host, "[") {
		// [addr] or [addr]:port -- what follows the closing bracket is a port
		// and says nothing about whether the inside is an address
		end := strings.LastIndex(host, "]")
		if end <= 1 {
			return netip.Addr{}, false
		}
		host = host[1:end]
	} else if i := strings.LastIndex(host, ":"); 0 < i && strings.Contains(host[:i], ".") {
		// a dotted quad with a trailing :port. Only the v4 pattern and the
		// ipv4-mapped tail can produce one; bare ipv6 is matched without a
		// port, so nothing here can strip a group off a real address.
		host = host[:i]
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr, true
}

// addrFromByteList parses the byte-slice rendering of an address -- the
// "[17 23 18 34]" and sixteen-group forms redactAddrBytesPattern offers -- and
// reports whether the candidate really is one.
//
// The candidate is turned back into TEXT and handed to netip, the same way
// every other shape here defers the decision to the parser instead of
// re-deciding what an address is. A list of any length but four or sixteen is
// not an address in any rendering fmt produces, so it is rejected on length
// before that.
func addrFromByteList(match string) (netip.Addr, bool) {
	inner, ok := strings.CutPrefix(match, "[")
	if !ok {
		return netip.Addr{}, false
	}
	inner, ok = strings.CutSuffix(inner, "]")
	if !ok {
		return netip.Addr{}, false
	}
	groups := strings.Split(inner, " ")
	switch len(groups) {
	case 4:
		// rebuilding the dotted quad also hands netip the two rejections this
		// arm would otherwise have to make itself: a group over 255, and a
		// leading zero, which fmt never prints for a byte
		addr, err := netip.ParseAddr(strings.Join(groups, "."))
		if err != nil {
			return netip.Addr{}, false
		}
		return addr, true
	case 16:
		var ip [16]byte
		for i, group := range groups {
			if 1 < len(group) && group[0] == '0' {
				// the leading zero netip rejects in a dotted quad, so both
				// arms agree on what a printed byte looks like
				return netip.Addr{}, false
			}
			b, err := strconv.ParseUint(group, 10, 8)
			if err != nil {
				return netip.Addr{}, false
			}
			ip[i] = byte(b)
		}
		return netip.AddrFrom16(ip), true
	}
	return netip.Addr{}, false
}

// maskedAddr reports whether an address that parsed is one this redactor
// masks.
//
// The unspecified address -- 0.0.0.0, ::, and the all-zero byte lists they
// print as -- is deliberately NOT masked, in any rendering. It is the zero
// value of an address field and the wildcard bind, so it names no host,
// belongs to nobody, and is a constant every reader already knows: a token
// over it protects nothing, since it would be the same token on every line of
// every bundle for a value anyone can guess in one try.
//
// What masking it would cost is real. The line
//
//	[multi]max source count 3 = {tcp [0 0 0 0] 0 [17 23 18 34] 443 }
//
// says no source was recorded. Put a token on both halves and it says a
// source WAS recorded and hides which -- not a redaction of the truth but a
// change to it. The placeholder is also the commonest address-shaped thing in
// these logs, so masking it would add a token to nearly every line and bury
// the addresses that do matter.
//
// The exported bundle's README states this exception, so the bundle's own
// description of itself stays exactly true.
func maskedAddr(addr netip.Addr) bool {
	// Unmap first: ::ffff:0.0.0.0 is the same wildcard, and netip compares
	// the 4-byte and 16-byte forms as different addresses.
	return !addr.Unmap().IsUnspecified()
}

// canonicalAddr is the text an address token is derived from.
//
// One address is one token however the log spelled it. The token used to be
// hmac'd over the matched TEXT, so the byte-slice and dotted-quad renderings
// of one destination -- and "1.2.3.4" against "1.2.3.4:443" -- came out as
// different tokens, and a reader could not follow one flow across the two
// spellings the same file uses. Hashing the parsed address instead makes
// every rendering of it agree.
//
// Unmap is part of the canonical form because the v4-mapped renderings name
// the same host as the dotted quad: an unexported net.IP field prints an ipv4
// destination as a 16-byte v4-mapped list, and "::ffff:1.2.3.4" is a literal
// net itself prints. The zone is NOT stripped: two links' fe80::1 are two
// different hosts.
func canonicalAddr(addr netip.Addr) string {
	return addr.Unmap().String()
}

// longestAddrLiteral returns the bounds of the longest address literal inside
// one candidate span, or an empty range when the span holds none.
//
// It is what makes the rejection path fail safe. A span that does not parse
// whole is written back verbatim and the scan resumes past it, so whatever the
// span swallowed is never reconsidered -- an over-match leaks, it does not
// merely add noise. So instead of trusting the pattern to be exact, a
// rejected span is searched: every substring that begins and ends on a group
// boundary (the span's own ends, and either side of a ':' or a bracket) is
// parsed, and the longest that parses wins. "2001:db8::1::2" is not an
// address, and neither is "[2001:db8::1::2]:443", but the address inside each
// is still masked.
//
// It searches only for the textual literals, since the byte-slice pattern is
// exact at both edges and can leave nothing over: a bracketed list that does
// not parse holds no address rendering, and hunting a four-group window inside
// a five-group slice would be pure over-redaction.
//
// The search is bounded by the span, which the pattern holds well under a
// hundred characters, and it neither recurses nor rescans its own output, so
// redaction stays one pass over the line and always terminates.
func longestAddrLiteral(candidate string) (int, int) {
	// the ends of the span, and either side of every separator the patterns
	// can leave inside one
	bounds := []int{0}
	for i := 0; i < len(candidate); i += 1 {
		switch candidate[i] {
		case ':', '[', ']':
			bounds = append(bounds, i, i+1)
		}
	}
	bounds = append(bounds, len(candidate))

	start, end := 0, 0
	for _, lo := range bounds {
		for _, hi := range bounds {
			if hi-lo <= end-start {
				continue
			}
			addr, err := netip.ParseAddr(candidate[lo:hi])
			if err == nil && maskedAddr(addr) {
				start, end = lo, hi
			}
		}
	}
	return start, end
}

// logRedactor maps sensitive values to stable per-export tokens.
//
// Stability within one export is what keeps a redacted bundle useful: the same
// address reads as the same token on every line, so a flow can still be
// followed. The salt is random per export and is never written into the bundle
// or logged, so tokens cannot be correlated between bundles or reversed.
type logRedactor struct {
	salt []byte
}

// newLogRedactor creates a redactor with a fresh, random per-export salt. It
// fails closed: when crypto/rand cannot supply real randomness, it returns an
// error instead of substituting anything derived from process-constant state
// (like the log paths). A path-derived salt would be the same for every
// bundle this install ever exports, letting tokens be correlated across
// bundles -- exactly the property redaction exists to prevent -- so there is
// no safe fallback here, only success or an error.
func newLogRedactor() (*logRedactor, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating redaction salt: %w", err)
	}
	return &logRedactor{salt: salt}, nil
}

func (self *logRedactor) token(prefix string, value string) string {
	mac := hmac.New(sha256.New, self.salt)
	mac.Write([]byte(value))
	return prefix + hex.EncodeToString(mac.Sum(nil))[:12] + ">"
}

// redactLine rewrites one line. It is applied to EVERY line of a log file,
// including the plaintext header block, the rotation footer, and backtrace
// continuation lines, which carry no [IWEF] header prefix -- so it must never
// depend on a line being a well-formed glog entry.
func (self *logRedactor) redactLine(line string) string {
	line = redactUUIDPattern.ReplaceAllStringFunc(line, func(match string) string {
		return self.token("<id:", match)
	})
	// The three address patterns are disjoint by construction: a byte-slice
	// list holds neither a dot nor a colon, and neither of the other two can
	// match across a space. So the order among them is free, and no pattern
	// can see another's output -- a token is "<addr:" plus hex, which has no
	// bracket, no dot, and no hex group ending on a colon.
	line = redactAddrBytesPattern.ReplaceAllStringFunc(line, self.addrToken)
	line = redactIPv6Pattern.ReplaceAllStringFunc(line, self.addrToken)
	line = redactIPv4Pattern.ReplaceAllStringFunc(line, self.addrToken)
	return line
}

// addrToken rewrites a candidate address match, and leaves anything that is
// not an address exactly as it was.
func (self *logRedactor) addrToken(match string) string {
	if addr, ok := matchAddr(match); ok {
		if !maskedAddr(addr) {
			// the unspecified placeholder, which is left as written
			return match
		}
		return self.token("<addr:", canonicalAddr(addr))
	}
	// Not an address whole. Either the span is a lookalike the pattern was
	// generous enough to offer -- a timestamp, a counter, a byte slice that is
	// not an address -- and holds no address at all, or it took in more than
	// the address inside it, in which case the address is masked and the
	// surplus is written back untouched.
	start, end := longestAddrLiteral(match)
	if start == end {
		return match
	}
	addr, ok := matchAddr(match[start:end])
	if !ok {
		return match
	}
	return match[:start] + self.token("<addr:", canonicalAddr(addr)) + match[end:]
}
