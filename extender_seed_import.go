package sdk

import (
	"io"
	"os"

	"github.com/urnetwork/connect"
)

// extender_seed_import.go — importing an authenticated seed bundle from a file
// or pasted text (connect EXTENDER-UPDATE2.md §2.3).
//
// A seed is `ur-seed:1:` text, or the raw envelope a mirror serves as
// `/v1/seed.bin`. Importing one judges it exactly as a fetched one: the
// envelope signature under the root keys the space already holds, its network
// host, its freshness by the local clock with connect's skew allowance, its
// sequence against the floor the space has accepted, and every record and
// revocation it carries. An accepted bundle is committed to the seed store and
// applied to the directory, revocations first, with source `seed`. Nothing in
// a seed can replace a root key, a network host, a gossip url or a resolver
// setting, so unlike the K7 share there is no settings decision to confirm: a
// seed for another network is refused, never taken with its settings.
//
// This is separate from the unsigned K7 share (`ur-ext:1:`,
// extender_view_controller.go), whose import is unchanged: a share's
// addresses stay unverified hints, while a seed carries signed records only.
//
// The result is a plain gomobile value the apps show as is. Its error is a
// localization key id, like the K7 import's, so each app maps it to its own
// string.

// The error ids an import answers with.
const (
	// Not a seed: a wrong prefix, bad base64, a body that does not decode, a
	// format version this build does not read.
	ExtenderSeedImportErrorInvalid = "import_extender_seed_invalid"
	// Over the 64 KiB envelope cap, or more records, addresses or revocations
	// than a bundle may carry.
	ExtenderSeedImportErrorTooLarge = "import_extender_seed_too_large"
	// The envelope signature does not verify under any root key this space
	// trusts: forged, or signed by a key it does not or no longer trusts.
	ExtenderSeedImportErrorForged = "import_extender_seed_forged"
	// The bundle is for another operator's network.
	ExtenderSeedImportErrorForeignNetwork = "import_extender_seed_foreign_network"
	// The bundle carries a gated or canary-restricted record, which no public
	// bundle may carry.
	ExtenderSeedImportErrorGatedRecord = "import_extender_seed_gated_record"
	// A record or revocation the bundle carries fails its own verification.
	ExtenderSeedImportErrorInvalidRecord = "import_extender_seed_invalid_record"
	// Past its validity by this device's clock: an old bundle, or a clock that
	// is ahead.
	ExtenderSeedImportErrorExpired = "import_extender_seed_expired"
	// Not valid yet by this device's clock, which is most likely behind.
	ExtenderSeedImportErrorNotYetValid = "import_extender_seed_not_yet_valid"
	// Older than the bundle this space already accepted.
	ExtenderSeedImportErrorRollback = "import_extender_seed_rollback"
	// A different bundle under the sequence this space already accepted.
	ExtenderSeedImportErrorConflict = "import_extender_seed_conflict"
	// The space trusts no root key yet, so nothing can be verified.
	ExtenderSeedImportErrorNoRootKeys = "import_extender_seed_no_root_keys"
	// The import could not run: no space or directory, a space that refuses
	// imports, a file that cannot be read, or a store that cannot be written.
	ExtenderSeedImportErrorUnavailable = "import_extender_seed_unavailable"
)

// The outcome of an accepted import (ExtenderSeedImportResult.Outcome).
const (
	// a newer bundle than any this space had
	ExtenderSeedImportOutcomeAccepted = connect.ExtenderSeedOutcomeAccepted
	// the bundle already in force, again; nothing changed but what the
	// directory had lost of it
	ExtenderSeedImportOutcomeIdentical = connect.ExtenderSeedOutcomeIdentical
)

// What an import did.
type ExtenderSeedImportResult struct {
	Ok bool
	// One of the ExtenderSeedImportError ids, empty when Ok.
	Error string
	// When Ok, ExtenderSeedImportOutcomeAccepted or
	// ExtenderSeedImportOutcomeIdentical.
	Outcome string
	// The network the bundle is for, its sequence and its expiry in unix
	// milliseconds. Set when the bundle verified.
	NetworkHost string
	Sequence    int64
	ExpireTime  int64
	// The records the bundle carries that no revocation in it dominates, and
	// its revocations.
	RecordCount     int
	RevocationCount int
	// How many of its records the directory holds as usable now. Zero after
	// an accepted import means every one was already withdrawn.
	UsableCount int
}

// The largest artifact an import reads: the text form of a full envelope with
// room for the whitespace a paste or a file adds around it. Anything larger is
// refused before it is decoded.
var extenderSeedImportMaxByteCount = connect.ExtenderSeedMaxTextByteCount + 4096

// ImportExtenderSeed imports pasted `ur-seed:1:` text.
func (self *NetworkSpace) ImportExtenderSeed(text string) *ExtenderSeedImportResult {
	if extenderSeedImportMaxByteCount < len(text) {
		return &ExtenderSeedImportResult{Error: ExtenderSeedImportErrorTooLarge}
	}
	return self.importExtenderSeedArtifact([]byte(text))
}

// ImportExtenderSeedData imports the contents of a seed file: `ur-seed:1:`
// text, or the raw envelope a mirror serves.
func (self *NetworkSpace) ImportExtenderSeedData(data []byte) *ExtenderSeedImportResult {
	if extenderSeedImportMaxByteCount < len(data) {
		return &ExtenderSeedImportResult{Error: ExtenderSeedImportErrorTooLarge}
	}
	return self.importExtenderSeedArtifact(data)
}

// ImportExtenderSeedFile imports the seed file at `path`, in either form. A
// file larger than any seed is refused before more than that is read.
func (self *NetworkSpace) ImportExtenderSeedFile(path string) *ExtenderSeedImportResult {
	file, err := os.Open(path)
	if err != nil {
		return &ExtenderSeedImportResult{Error: ExtenderSeedImportErrorUnavailable}
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, int64(extenderSeedImportMaxByteCount)+1))
	if err != nil {
		return &ExtenderSeedImportResult{Error: ExtenderSeedImportErrorUnavailable}
	}
	return self.ImportExtenderSeedData(data)
}

// The import of one artifact, judged, committed and applied.
func (self *NetworkSpace) importExtenderSeedArtifact(artifact []byte) *ExtenderSeedImportResult {
	if self.hostedIncompatibleGuarded("ImportExtenderSeed") {
		return &ExtenderSeedImportResult{Error: ExtenderSeedImportErrorUnavailable}
	}
	seed := self.getExtenderSeed()
	if seed == nil {
		return &ExtenderSeedImportResult{Error: ExtenderSeedImportErrorUnavailable}
	}
	return seed.importArtifact(artifact)
}

func (self *spaceExtenderSeed) importArtifact(artifact []byte) *ExtenderSeedImportResult {
	envelope, err := connect.ParseExtenderSeedArtifact(artifact)
	if err != nil {
		return self.importRefused(err)
	}
	rootKeys := self.directory.RootKeys()
	networkHosts := self.networkHosts()
	self.revalidate(rootKeys)
	bundle, err := self.verify(envelope, rootKeys, networkHosts)
	if err != nil {
		return self.importRefused(err)
	}
	outcome, err := self.acceptor.Accept(bundle, rootKeys)
	if err != nil {
		return self.importRefused(err)
	}
	usableCount := self.directory.UsableCount(0)
	if _, err := connect.ApplyExtenderSeed(self.directory, bundle); err != nil {
		// only when the directory's keys or hosts moved since the verify; the
		// counts below say what landed
		self.log.V(1).Infof("[extender]imported seed apply: %s\n", connect.ExtenderSeedErrorCode(err))
	}
	self.recordBundle(bundle, ExtenderSeedSourceImport)
	if usableCount < self.directory.UsableCount(0) {
		// the network client samples and reads hello through what was
		// imported, and the release fetch takes its chance through it; the
		// fetch is a no-op without a bound identity
		self.wakeNetworkClient()
		self.space.requestExtenderRelease()
	}
	return &ExtenderSeedImportResult{
		Ok:              true,
		Outcome:         outcome,
		NetworkHost:     bundle.NetworkHost,
		Sequence:        int64(min(bundle.Sequence, uint64(1<<63-1))),
		ExpireTime:      extenderSeedTimeMillis(bundle.ExpireTime),
		RecordCount:     len(bundle.UnrevokedRecords()),
		RevocationCount: len(bundle.Revocations),
		UsableCount:     connect.ExtenderSeedUsableRecordCount(self.directory, bundle),
	}
}

// The refusal of an import, by its seed code. A paste is client input, so the
// detail goes to V(1) only.
func (self *spaceExtenderSeed) importRefused(err error) *ExtenderSeedImportResult {
	code := connect.ExtenderSeedErrorCode(err)
	self.log.V(1).Infof("[extender]seed import refused: %s\n", code)
	return &ExtenderSeedImportResult{Error: extenderSeedImportErrorId(code)}
}

// The import error id of a seed code. A code this build does not know is
// invalid input, which is what it most likely is.
func extenderSeedImportErrorId(code string) string {
	switch code {
	case connect.ExtenderSeedCodeTooLarge, connect.ExtenderSeedCodeTooMany:
		return ExtenderSeedImportErrorTooLarge
	case connect.ExtenderSeedCodeSignature:
		return ExtenderSeedImportErrorForged
	case connect.ExtenderSeedCodeForeignNetwork:
		return ExtenderSeedImportErrorForeignNetwork
	case connect.ExtenderSeedCodeGatedRecord:
		return ExtenderSeedImportErrorGatedRecord
	case connect.ExtenderSeedCodeRecordInvalid, connect.ExtenderSeedCodeRevocationInvalid:
		return ExtenderSeedImportErrorInvalidRecord
	case connect.ExtenderSeedCodeExpired:
		return ExtenderSeedImportErrorExpired
	case connect.ExtenderSeedCodeNotYetValid:
		return ExtenderSeedImportErrorNotYetValid
	case connect.ExtenderSeedCodeRollback:
		return ExtenderSeedImportErrorRollback
	case connect.ExtenderSeedCodeConflict:
		return ExtenderSeedImportErrorConflict
	case connect.ExtenderSeedCodeNoRootKeys:
		return ExtenderSeedImportErrorNoRootKeys
	case connect.ExtenderSeedCodeStore:
		return ExtenderSeedImportErrorUnavailable
	default:
		return ExtenderSeedImportErrorInvalid
	}
}
