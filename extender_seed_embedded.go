package sdk

import (
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// extender_seed_embedded.go — the seed bundles an application release carries
// (connect EXTENDER-UPDATE2.md §2.3).
//
// A release may carry the authenticated seed bundle of each network it ships
// with, as one more source beside the mirrors and an import. An embedded
// bundle is judged exactly as one fetched or pasted: the envelope signature
// under the root keys in force, the network host, the freshness window and the
// sequence floor, and then every record and revocation it carries. Being in
// the binary earns it nothing. Its validity ends when the publisher said it
// does, so an old release's bundle stops working; it is not a permanent
// offline escape hatch, and an expired one fails with an explicit freshness
// error rather than being used.
//
// The resource is res/extender_seed.txt, embedded at build time. By default it
// carries no bundle. Its lines are, in any order:
//
//	# a comment, ignored, as is a blank line
//	ur-seed:1:<base64url>
//	min-sequence <network host> <sequence>
//
// A `ur-seed:1:` line is one bundle in the paste form (connect
// EncodeExtenderSeedText), at most one per network host. A `min-sequence`
// line is trusted application configuration (§2.3): no bundle for that host
// with a lower sequence is accepted from any source, which bounds what a
// fresh install with no sequence history accepts. A resource that does not
// parse carries nothing at all, and the space reports it.
//
// Releasing with a bundle:
//
//  1. Take the current artifact of each network from the publisher or a
//     mirror, `GET https://<mirror>/v1/seed.bin`, whose body is the raw
//     envelope.
//  2. Encode each as its paste form and write one `ur-seed:1:` line per
//     network into res/extender_seed.txt, replacing the previous release's.
//     The bundle's expiry is how long the release can use it, so take it
//     shortly before the build.
//  3. Optionally add `min-sequence <network host> <sequence>` naming the
//     sequence just embedded. A valid embedded bundle raises the floor itself
//     when it is first accepted; the line matters for an install whose first
//     start comes after the bundle expired.
//  4. Build as usual, and run `go test -run TestExtenderSeedEmbeddedResourceParses .`
//     in the sdk, which refuses a resource that does not parse or that carries
//     too many bundles.
//
// Do not commit a production bundle to the repository: it expires, and an
// expired bundle in a later build only reports a failure.

//go:embed res/extender_seed.txt
var extenderSeedEmbeddedResourceText string

// The resource a space reads. A test replaces it.
var extenderSeedEmbeddedResource = func() string {
	return extenderSeedEmbeddedResourceText
}

// The prefix of a bundle line, the paste form's own prefix.
const extenderSeedEmbeddedTextPrefix = "ur-seed:"

// The prefix of a floor line.
const extenderSeedEmbeddedMinSequencePrefix = "min-sequence"

// Bundles one resource may carry. A release ships the networks it knows, which
// is a handful; more is a build mistake.
const extenderSeedEmbeddedMaxBundleCount = 8

// Floor lines one resource may carry.
const extenderSeedEmbeddedMaxMinSequenceCount = 16

// The largest resource read. A bundle line is at most the paste form of a
// 64 KiB envelope, so eight of them and their comments fit well inside this.
const extenderSeedEmbeddedMaxByteCount = 1024 * 1024

// The bundles and floors of one embedded resource, as read. `err` is set when
// the resource does not parse, and then `texts` and `minSequences` are empty:
// a malformed resource carries nothing.
type extenderSeedEmbeddedBundles struct {
	// the bundle lines in file order, untrimmed of nothing but whitespace
	texts []string
	// the trusted floors by normalized network host
	minSequences map[string]uint64
	err          error
}

var errExtenderSeedEmbeddedTooLarge = errors.New("the embedded extender seed resource is over its size limit")

// Reads one resource. Pure: the same text gives the same answer.
func parseExtenderSeedEmbedded(resource string) *extenderSeedEmbeddedBundles {
	failed := func(err error) *extenderSeedEmbeddedBundles {
		return &extenderSeedEmbeddedBundles{
			minSequences: map[string]uint64{},
			err:          err,
		}
	}
	if extenderSeedEmbeddedMaxByteCount < len(resource) {
		return failed(errExtenderSeedEmbeddedTooLarge)
	}
	bundles := &extenderSeedEmbeddedBundles{
		minSequences: map[string]uint64{},
	}
	for index, line := range strings.Split(resource, "\n") {
		line = strings.TrimSpace(line)
		lineNumber := index + 1
		switch {
		case line == "", strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, extenderSeedEmbeddedTextPrefix):
			if extenderSeedEmbeddedMaxBundleCount <= len(bundles.texts) {
				return failed(fmt.Errorf(
					"line %d: the embedded extender seed resource carries more than %d bundles",
					lineNumber,
					extenderSeedEmbeddedMaxBundleCount,
				))
			}
			bundles.texts = append(bundles.texts, line)
		case strings.HasPrefix(line, extenderSeedEmbeddedMinSequencePrefix):
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[0] != extenderSeedEmbeddedMinSequencePrefix {
				return failed(fmt.Errorf("line %d: a min-sequence line is `min-sequence <network host> <sequence>`", lineNumber))
			}
			networkHost := normalExtenderSeedNetworkHost(fields[1])
			if !isDottedHostName(networkHost) {
				return failed(fmt.Errorf("line %d: min-sequence names no network host", lineNumber))
			}
			minSequence, err := strconv.ParseUint(fields[2], 10, 64)
			if err != nil {
				return failed(fmt.Errorf("line %d: min-sequence is not a sequence: %w", lineNumber, err))
			}
			if _, ok := bundles.minSequences[networkHost]; !ok &&
				extenderSeedEmbeddedMaxMinSequenceCount <= len(bundles.minSequences) {
				return failed(fmt.Errorf(
					"line %d: the embedded extender seed resource carries more than %d min-sequence lines",
					lineNumber,
					extenderSeedEmbeddedMaxMinSequenceCount,
				))
			}
			// two lines for one host keep the higher floor: a floor only ever
			// narrows what is accepted
			bundles.minSequences[networkHost] = max(bundles.minSequences[networkHost], minSequence)
		default:
			return failed(fmt.Errorf("line %d: not a comment, a ur-seed bundle or a min-sequence line", lineNumber))
		}
	}
	return bundles
}

// The floor the embedded resource sets for one network host, 0 for none.
func (self *extenderSeedEmbeddedBundles) minSequence(networkHost string) uint64 {
	if self == nil {
		return 0
	}
	return self.minSequences[normalExtenderSeedNetworkHost(networkHost)]
}

// The network host form the seed store and the floors are keyed by: lower
// case, no surrounding space, no trailing dot -- the comparison every record
// and revocation is judged by (connect ExtenderNetworkHostAllowed).
func normalExtenderSeedNetworkHost(networkHost string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(networkHost), "."))
}
