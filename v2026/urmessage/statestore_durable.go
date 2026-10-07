package urmessage

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/urnetwork/connect/v2026/messagegroup"
	"github.com/urnetwork/connect/v2026/mls"
)

// ── S2-14: the durable mls.StateStore ────────────────────────────────────────────────────────

// DurableStateStore is [mls.StateStore] on a directory, so that a device that is killed comes back
// into the groups it was in.
//
// WHAT PROTECTS THE PRIVATE KEYS IN IT, PLAINLY, BECAUSE THE HONEST ANSWER IS SHORT.
//
// FILE PERMISSIONS AND NOTHING ELSE. Every octet this store writes is written in the CLEAR: the
// MLS epoch state (which carries this member's leaf HPKE private key, its TreeKEM path-secret
// ladder and the epoch's restore secret), the init and encryption private keys of every key
// package this device published, this device's Ed25519 identity private key, the 32-octet X-Wing
// seed whose public half this device's leaf publishes, and each group's `pq_secret` and
// `group_handle_key`. There is no passphrase, no key derivation, no OS keychain and no hardware.
// Anything that can read the directory can read every group this device is in, past and future,
// and can speak as this device.
//
// The directory is created 0o700 and every file 0o600, and on a POSIX filesystem that is a real
// bound: another user on the machine is refused. ON WINDOWS IT IS NOT A BOUND THIS CODE SETS.
// Go's mode argument is reduced to the read-only bit there, so a file inherits the ACL of the
// directory it is created in; what actually protects it is wherever the caller put the directory,
// and a caller that chose a world-readable path gets a world-readable key store with no complaint
// from here. Any process running as the same user reads it on every platform.
//
// WHAT WOULD CLOSE IT IS NOT INVENTED HERE. Encryption at rest needs a key, and a key needs
// either a passphrase the user types (with a KDF, a rekey story and a lost-passphrase story) or a
// platform keystore (DPAPI, the macOS Keychain, the Android Keystore, iOS's Secure Enclave) --
// each of which is a product decision with a different threat model, and none of which this
// package may take on its own. **FILED AS S2-24: what protects urmessage's private keys at rest.
// It is open, it has no owner, and until it is ruled the answer above is the whole answer.**
// Spec A §8.1's "sealed" columns are the server's obligation and say nothing about a client disk.
//
// WHAT IT DOES AND DOES NOT INHERIT FROM THE STREAM STORE NEXT DOOR. [sdk.StreamStore] solved
// crash safety for the stream-index reserver and this follows it rather than inventing a second
// discipline: a single-writer exclusion held by the operating system over the directory, acquired
// before anything is read or written; the guard entry BESIDE the data directory and never inside
// it, so "an entry in the data directory that is not a record is a finding" stays categorical; an
// fsync that must return before a value is observable; and no liveness heuristic anywhere -- the
// hold is released by Close and by the death of this process and by nothing else.
//
// WHERE IT DELIBERATELY DIFFERS, said at the line rather than here: the stream store's rows are
// fixed-width append-only records, so it repairs a torn tail in place; these values are
// variable-width and are REPLACED, so a half-written one can never become observable at all. See
// [DurableStateStore.writeRecord]. And the stream store has a key-space tag in every row name
// because messagegroup.StreamKey's field set can change under it; these keys are raw octets that
// mls hands over, so a format version inside each record is what stands in its place.
//
// It is safe for concurrent use. Every method takes the store's lock for the whole of its work,
// which is what makes the read-modify-erase of DeleteGroupStateBefore one step.
type DurableStateStore struct {
	dir     string
	dataDir string

	// exclusion is the SINGLE-WRITER guard, held by the operating system on an entry beside the
	// data directory. Two stores over one directory is two devices writing one device's MLS
	// state: the later writer's epoch overwrites the earlier's, and the earlier device then
	// restores a group whose ratchet position is another device's.
	exclusion io.Closer

	lock   sync.Mutex
	closed bool

	// unswept is every `.writing-*` [sweepStateTempFiles] found at open and could NOT remove.
	// It is a field and not a returned error because the sweep is not fatal; see that function
	// for why, and [DurableStateStore.UnsweptWrites] for what a caller is owed.
	unswept []string

	// flushes counts every write that passed through writeRecord, taken AFTER the Sync returns
	// rather than before it.
	//
	// WHAT IT MEASURES AND WHAT IT DOES NOT, AND THE SECOND HALF IS A CORRECTION OF WHAT USED
	// TO STAND HERE. This comment said "counted here, the flush cannot be deleted without this
	// number going to zero". THAT IS FALSE AND IT WAS FALSIFIED BY DELETING THE FLUSH: replace
	// `syncErr := temp.Sync()` with `var syncErr error`, leave `self.flushes += 1` exactly
	// where it is, and TestEveryValueTheDurableStoreNamesWasFlushedFirst still passes and so
	// does the whole of ./urmessage and ./cp3b. Re-measured at this commit, both directions.
	// The reason is structural rather than a slip of position: the increment is UNCONDITIONAL,
	// so it counts the same whether the call above it is there or not, and no rearrangement of
	// an unconditional statement turns it into a count of work performed.
	//
	// SO THE NUMBER IS ONE OF TWO CLAUSES AND IT IS THE WEAKER ONE. This counts that a value
	// went through the one write path -- one flush per value and not one per call, which is
	// what TestEveryValueTheDurableStoreNamesWasFlushedFirst actually drives, and it is worth
	// having. What holds the fsync ITSELF is TestEveryFsyncInThisPackageIsAtASiteThisSuiteNames
	// in sourcegate_test.go: it parses this package's own source and refuses any Sync call site
	// that is not writeRecord's or syncStateDir's, so deleting the flush is a RED gate rather
	// than an unchanged number. That gate is sdk/message_stream_store_test.go's, one package
	// over, and its absence here was the stream store's discipline copied one layer deep: the
	// counter came and the thing that made the counter mean something did not.
	//
	// It counts the VALUE flush and not the directory flush, because the directory flush is a
	// no-op on Windows by construction (see syncStateDir there) and a counter that read zero
	// on one platform and one on another would measure the platform rather than the code.
	flushes int

	// skipRemove is the injected failure point, set by tests in this package and by nothing
	// else. It is [sdk.StreamStore]'s `interrupt` field one package over, and it is here for
	// the same reason: the state it stands in for -- a remove that reported success and left
	// the entry readable -- is not one any filesystem this suite can run on will produce, and
	// a §5.12 clause that cannot be driven is a §5.12 clause nobody can tell is still there.
	//
	// It is a bool and not a path or a count, so it can only ever turn the erase off wholesale
	// in a test binary; there is no production path that sets it and no way to set it from
	// outside this package.
	skipRemove bool
}

var _ mls.StateStore = (*DurableStateStore)(nil)
var _ DeviceStore = (*DurableStateStore)(nil)

// DeviceStore is the durable surface a [Device] needs BEYOND [mls.StateStore], so that a restart
// is a restore rather than a new device.
//
// It is a separate interface and not extra methods on the config, because [DeviceConfig.StateStore]
// is an `mls.StateStore` and a store that cannot persist an identity must go on being legal there.
// [NewDevice] asks a store whether it satisfies this, and a store that does not gets exactly the
// behaviour it had before this interface existed: a fresh identity every process, no restore.
//
// EVERYTHING ON IT IS SECRET IN FULL. The identity private key speaks as this device in every
// group it is in; the wrap seed opens every X-Wing encapsulation addressed to this device's leaf;
// a [GroupRecord] carries two of the [Invite]'s four values, and whoever holds those plus the MLS
// state this store keeps beside them is in the group.
type DeviceStore interface {
	mls.StateStore

	// GetDeviceIdentity answers what PutDeviceIdentity last wrote, or a refusal wrapping
	// [ErrNoDeviceIdentity] when this store has never held one. It is never five nils.
	//
	// wrapSeed is the 32-octet X-Wing seed [messagegroup.XwingKeyGenFromSeed] expands into the
	// decapsulation key for the public half that is INSIDE leafKeys. The two travel in one record
	// and are written in one call precisely so they cannot come from two different mints: a seed
	// that does not expand to the published key is a device that opens nothing and says nothing,
	// which is strictly worse than the dropped key this field exists to stop.
	//
	// IT IS THE ONE VALUE HERE A STORE MAY NOT HOLD. A directory written before this field
	// existed answers it EMPTY with a nil error -- the shape [DeviceStore.PeerHeads] already uses
	// for a group persisted by a build with no head table. See
	// [DurableStateStore.GetDeviceIdentity] for what such a device can and cannot do.
	GetDeviceIdentity() (signerPub []byte, signerPriv []byte, leafKeys []byte, wrapSeed []byte, err error)
	PutDeviceIdentity(signerPub []byte, signerPriv []byte, leafKeys []byte, wrapSeed []byte) error

	// PutGroupRecord writes the urmessage-side half of one group: the values that are this
	// package's rather than MLS's, and that a restore cannot be performed without.
	PutGroupRecord(record *GroupRecord) error

	// GroupRecords is every group this store holds a record for, in no particular order.
	GroupRecords() ([]*GroupRecord, error)

	// DeleteGroupRecord removes one group's record AND every MLS epoch state beside it AND every
	// copy of a record this device sent in it. It is how a device leaves a group without leaving
	// its keys, or what it said, on the disk.
	DeleteGroupRecord(groupId []byte) error

	// MarkGroupForgetting, GroupBeingForgotten and GroupsBeingForgotten are the mark a leave writes
	// before it closes a group and the erase removes last, so an erase that stops part way is
	// finished rather than restored. See [DurableStateStore.MarkGroupForgetting].
	MarkGroupForgetting(groupId []byte) error
	GroupBeingForgotten(groupId []byte) (bool, error)
	GroupsBeingForgotten() ([][]byte, error)

	// PutSentRecord writes the copy of one record this device sealed in one group, BEFORE that
	// record is submitted. SentRecords answers every copy one group holds, ascending by stream
	// index. See [SentRecord] for why the copy exists at all.
	PutSentRecord(groupId []byte, record *SentRecord) error
	SentRecords(groupId []byte) ([]*SentRecord, error)

	// PutPeerHeads REPLACES the table of receiver-ladder heads one group has authenticated, and
	// PeerHeads answers it -- EMPTY, and no error, for a group that has none written, which is
	// every group a build before this one persisted. See [PeerHead] for what one is and why a
	// restart needs it.
	PutPeerHeads(groupId []byte, heads []PeerHead) error
	PeerHeads(groupId []byte) ([]PeerHead, error)
}

// PeerHead is the highest 5.6 stream index this device has AUTHENTICATED on one peer's receiver
// ladder AT ONE EPOCH: the number a restarted device tracks that ladder at, so that a peer past
// the receiver window is not silent for the rest of the epoch. Ledger item 241's restart half.
//
// WHY IT IS PER EPOCH AND NOT ONE NUMBER PER LADDER. The stream index is continuous across epochs,
// so the head of the whole stream is one number -- and it is the WRONG number for a device that
// re-walks its history. A restarted device opens every record again, from the first, and the
// ladder for epoch n has to start at the head as of the START of epoch n: the highest index
// authenticated at epochs BELOW n. The head at n itself, or at any later epoch, stands above the
// very records the re-walk is about to open, and a ladder tracked there refuses all of them as
// "below this receiver's head". So the table keeps one head per (ladder, epoch), and
// [Group.pastHeadLocked] reads it with the strict inequality that makes a restart correct.
//
// IT IS NOT SECRET. Every field is a number that is in the cleartext header of a record the server
// stores, and the leaf-to-handle mapping is what every member computes. It is written through the
// same framed, checksummed record every other value here is, for the same reason: a file that is
// not what its name says is refused rather than read as a head.
//
// IT IS A HINT AND NOT A PROOF. A head written here was read off records the group's keys
// authenticated at the time; the ladder it positions still opens nothing that does not open. A
// table that is missing or stale costs a restart the window only -- a ladder tracked below the
// truth by less than [messagegroup.DefaultRecordWindowSize] indices absorbs the difference, and
// one tracked at 0 is what every build before this one did.
type PeerHead struct {
	// The MLS epoch the head was authenticated at.
	Epoch uint64

	// The peer's leaf index, from which its sender_handle is derived; the retention class wire
	// byte and the eph window that name the ladder. Together with the epoch they are
	// [Group.peerHeadsAt]'s key.
	Leaf          uint32
	RetentionWire byte
	EphWindow     uint64

	// The highest stream index authenticated on that ladder at that epoch.
	Head uint64
}

// SentRecord is this device's copy of one application record it sealed: the only place its own
// half of a conversation can be read back from.
//
// WHY THERE IS A COPY. Since connect 4c030dc an application record's body is an MLS PrivateMessage,
// and a member cannot open its own -- Protect spends a generation of the leaf's own ratchet and MLS
// keeps no receiving ratchet for a leaf's own messages (connect messagegroup OPENITEMS MG-4). So a
// restarted device that re-fetches its history reads the other members' lines off the server and
// can read its OWN lines off nothing but this.
//
// IT IS PLAINTEXT ON THE DISK, AND THAT IS NOT A NEW EXPOSURE OF THIS DIRECTORY, said carefully
// because it would be easy to say too much. The epoch state beside it lets anything that can read
// this directory re-derive every OTHER member's record keys for this epoch and read their lines off
// the server; this is the same user's own lines, next to it. What it DOES change is what is readable
// with the server's rows gone: the directory alone now holds what this device said. [DurableStateStore]'s
// header is the answer on what protects any of it, and S2-24 is still what must rule that.
//
// IT GROWS WITHOUT BOUND, one file per line sent, until the group is left. Nothing prunes it, for
// the same reason nothing prunes the server's rows (7.2's sweep is not built): there is no retention
// decision yet for it to follow.
type SentRecord struct {
	// The §5.6 stream index the record was sealed at, and the name of this copy.
	StreamIndex uint64

	// The record's body_hash, which is how a record fetched back from the server is matched to the
	// copy without opening it.
	BodyHash [32]byte

	// The sender's clock reading the record's head carries, unix milliseconds.
	SentAtMs int64

	// What was sealed. Octets, never interpreted HERE -- and since the content envelope landed
	// they are the APPLICATION PLAINTEXT, `kind ‖ body`, not the text. That is the same
	// definition it always had; what changed is what [Group.Send] hands it.
	//
	// A COPY WRITTEN BY A PRE-KINDS BUILD IS RAW TEXT AND HAS NO CODE, and this record carries no
	// version byte of its own to refuse it by: the store's one version lever is read for every
	// record in the directory, the device identity included, so spending it here would brick a
	// restore rather than refuse a line. What happens instead is [Group.openOwnFromCopyLocked]'s
	// answer -- the first octet of an old line is an unassigned code, so it renders as one closed
	// placeholder under the unknown-kind rule, which is what every other member's build does with
	// a code it does not know, and is not the line rendered wrong.
	Body []byte
}

// GroupRecord is the urmessage-side state of one group: the values that do not live in MLS and
// that [Device.Restore] cannot rebuild a session without.
//
// IT IS SECRET IN FULL. PqSecret and GroupHandleKey are two of the [Invite]'s four values; see
// [DurableStateStore] for what does and does not protect them on the disk.
type GroupRecord struct {
	// The 32 octet group id the server keys its rows by.
	GroupId []byte

	// §7's pq_secret AT THE EPOCH THIS RECORD NAMES, drawn by [messagegroup.NewPqSecret] and
	// carried to a joiner in the [Invite].
	//
	// IT USED TO BE THE GROUP'S ONE SECRET FOR ITS WHOLE LIFE and this field is what is left of
	// that reading. Ledger item 243 ruled the lifetime value in 2026-09-18 "on the explicit
	// condition that rotating it is a prerequisite of shipping REMOVAL", and item 251's ruling 40
	// is that condition coming due: a removed member that keeps the post-quantum half of every
	// future epoch's storage root has not been removed from a quantum adversary at all. So the
	// state that matters is [GroupRecord.PqSecrets] below, and this field is the CURRENT epoch's
	// entry written a second time.
	//
	// WHY IT IS STILL WRITTEN, which is the whole of what an OLD STORE does. A record written
	// before rotation has five parts and this field is part two; one written before the
	// wrap_dark part has six, the sixth being the table; and one written by this build has
	// TEN. (That count was written as "seven" and stayed there while parts eight, nine and ten
	// landed -- so it is stated as the arity [groupRecordOf] actually switches on, which is the
	// only place this number is checkable.) The reader takes every arity: on a five-part record it
	// files this one scalar at the epoch the record names and leaves connect's group-lifetime
	// premise standing, which answers every past epoch out of that one value -- the behaviour of
	// every build before this one, exactly. So a device whose disk was written by the deployed
	// alpha restores, opens its backlog and keeps working, and the day it ingests a rotated
	// commit the premise is refuted by the octets rather than by a version.
	//
	// WHAT IT DOES NOT BUY IS A DOWNGRADE, AND THE COST GOES UP BY ONE ARITY WITH EVERY PART.
	// A build from before the table meeting a six- or seven-part record refuses THAT GROUP by
	// name (`ErrStateStoreFormat`), because its reader tests `len(parts) != 5`; a build from
	// before the wrap_dark part refuses a seven-part one, because its reader tests
	// `len(parts) != 5 && != 6`. That is loud rather than silent -- neither ever mis-reads a
	// later part as some other field -- and it is stated here rather than discovered. The alpha's
	// disk is FIVE parts, so what a downgrade costs is a group written by a build newer than the
	// one reading it, which is a rollback and not an upgrade.
	PqSecret []byte

	// pq_secret PER EPOCH: item 251's ruling 40, and the value a restart has to come back holding
	// or it opens nothing above the epoch it was written at.
	//
	// ASCENDING BY EPOCH, and the order is part of the value rather than tidiness: the record is
	// one frame of appended parts, and a map's iteration order would make two writes of one
	// unchanged table two different files for the store's rename dance to pay for.
	//
	// BOUNDED BY [messagegroup.PastEpochWindow] by the writer, not by the reader, for the same
	// reason connect bounds its own: an entry further behind than that can serve no open any
	// schedule on this device would admit, so persisting it is persisting a retired epoch's
	// post-quantum secret for nothing.
	//
	// EMPTY ON A RECORD WRITTEN BEFORE ROTATION, which is how the reader tells the two shapes
	// apart without a version octet: see [DurableStateStore.GroupRecords].
	PqSecrets []EpochPqSecret

	// SHA-256 OF EVERY pq_secret THIS DEVICE HAS EVER FILED, keyed by the lowest epoch it was
	// filed at, and NOT bounded by [messagegroup.PastEpochWindow].
	//
	// THIS DEVICE'S AND NOT THE GROUP'S, WHICH IS THE RULE'S WHOLE SCOPE (ledger rulings 42-45).
	// [Device.Join] files exactly one row, so this table is strictly smaller on a member admitted
	// later, and the rule spelled against it is "this receiver does not follow a removal onto a
	// secret THIS RECEIVER has held" -- never a statement about what the group holds. What follows
	// from that is written out where the rule is, at [Group.resolvePqSecretLocked]'s refusal.
	//
	// WHY A SECOND TABLE INSTEAD OF WIDENING THE FIRST. [GroupRecord.PqSecrets] is bounded on
	// purpose: an entry further behind than the window can serve no open any schedule on this
	// device would admit, so keeping the SECRET is keeping a retired epoch's post-quantum half for
	// nothing. But the rule item 243 is about -- a removal may not be followed on a value the
	// removed member also holds -- needs to know whether this device has EVER held a value, and the
	// removed member's set does not shrink when this device's window moves. Measured: after 33
	// honest rotations a removal fanned out on pq_secret[1] was followed with a nil error, and the
	// removed member's retained value was the post-quantum half of the survivors' storage_root at
	// the epoch it was removed at. So the answer to "have I held this" is kept for ever and the
	// SECRET is not: 32 octets of digest per epoch, which is a question-answerer and not key
	// material, beside a 32-octet secret that is erased on schedule.
	//
	// EMPTY ON A RECORD WRITTEN BEFORE THIS PART, and that is the residual rather than a gap to be
	// invented around: such a device comes back witnessing only the rows its table carries. See
	// [Group.pqSecretWitness] for the one repair that closes it, which is a wire change.
	PqSecretWitness []EpochPqSecretWitness

	// group_handle_key: the epoch ZERO storage root's expansion. It never moves, which is why it
	// is stored once rather than per epoch.
	GroupHandleKey []byte

	// The MLS epoch this device's session was at when the record was written. It is the epoch
	// `messagegroup.GroupEngine.LoadGroup` is asked for, and it has to be carried HERE because
	// nothing on [mls.StateStore] enumerates epochs: no layer below this one can answer "the
	// latest" without a scan it has no method for, so the device that persisted the group is the
	// one that says which epoch it was at. That absence was half of open item J1-8; the other half
	// -- that no engine method opened a persisted group at all -- is closed, and LoadGroup refuses
	// by name when the state handed back does not stand at the epoch this field names.
	Epoch uint64

	// Whether [Group.Open] has published this group on the server. A restored group that was
	// never opened would be refused by the server at its first send, with a REASON the caller
	// would have to decode; carrying the bit means [Group.Send] refuses it by name instead.
	Opened bool

	// ── THE DIAGNOSIS A DARK GROUP OWES, ACROSS THE PROCESS THAT TOOK IT ────────────────────
	//
	// WHY IT IS ON THE DISK AT ALL. [Group.wrapDark] is ruling 38's whole answer to "an
	// undiagnosable REASON_REJECTED", and it was a field of a value that dies at exit. A device
	// that went dark and restarted came back with a persisted pq_secret no peer agrees with, no
	// diagnosis, and a pq_secret table that reads as HEALTHY -- [pqSecretsShowRotation] compares
	// octets and the fallback wrote the same octets as the epoch below, so nothing in the table
	// says anything is wrong. From then on Send and Receive gave the server's generic refusal
	// with no sentence about the wrap: exactly the state ruling 38 exists to prevent, arriving
	// through a restart instead of through a fan-out.
	//
	// THE OTHER SHAPE WAS TRIED FIRST AND IS WRITTEN DOWN BECAUSE IT IS THE TEMPTING ONE: stop
	// filing the fallback secret, so the restored session meets
	// [messagegroup.ErrPqSecretUnknownEpoch] by name instead of an AEAD tag, and carry no new
	// field. It cannot be built from here. The session has to be advanced into the epoch the
	// handle is at, [messagegroup.GroupSession.AdvanceEpoch] refuses an empty pq_secret by name,
	// and not advancing it leaves the session an epoch behind the handle -- which is the state
	// [Group.ingestCommitLocked] refuses for its own reasons, and which `ReadEpoch: self.epoch`
	// on every fetch would then be wrong about. So the fallback stays and the diagnosis is made
	// durable instead.
	//
	// WHICH of the three it was, as the octet [GroupRecord.WrapDarkKind] spells, or zero for
	// "this group is not dark". It is the kind and not the sentence: an error string is a thing
	// this build wrote and the next build would have to keep writing, while the sentinel is a
	// value a caller compares with [errors.Is].
	WrapDarkKind uint8

	// The epoch [GroupRecord.WrapDarkKind] was taken at. Meaningless when that is zero.
	WrapDarkEpoch uint64

	// ── THE LEAF LEDGER: WHICH LEAVES THIS DEVICE HAS STOOD AT, AND WHICH ONES HAVE LEFT ────
	//
	// LEDGER ITEM 245, AND IT IS ONE PART CARRYING TWO TABLES because the two are facts about the
	// same thing -- a LEAF -- and a leaf's row answers both. A device's sender_handle is
	// SenderHandle(group_handle_key, leaf) and group_handle_key never rotates, so the set of
	// handles this device has held is exactly the set of leaves it has stood at, derived and never
	// stored; and the epoch a leaf departed at is what says which epochs its records belong to it.
	//
	// WHY IT IS ON THE DISK. Neither table can be rebuilt from anything else here. The commits
	// that would say so are on the SERVER and this device has already followed them -- a re-walk
	// meets each one at an epoch it has left and skips it as ceremony -- and the tree the MLS state
	// holds says who stands in the group NOW, which is precisely the question a departed leaf is
	// outside of. A device that came back without them abandoned the removed member's records after
	// three fetches ([maxRecordAttempts]) and showed its own lines from an earlier leaf as a
	// stranger's.
	//
	// WHAT AN OLD STORE DOES, NAMED RATHER THAN DISCOVERED. A record written before part nine
	// carries NO ledger and decodes to a nil slice -- the same signal [GroupRecord.PqSecrets] uses
	// one field up -- and [Device.restoreOne] then seeds the set with the ONE leaf this device
	// stands at today and leaves the departed table empty. That is exactly the state every build
	// before this one was in, so such a device restores, opens its backlog and keeps working. What
	// it loses is stated: handles it held at an EARLIER leaf, and the records of leaves that
	// departed before the restart AND were never refilled. A refilled leaf still resolves, because
	// the member standing at it derives the same sixteen octets. A restore that REFUSED such a
	// record would be a device that can never start again, which is the one outcome no
	// compatibility question may reach.
	//
	// IT IS BOUNDED AND THE BOUND IS THE TREE'S, which is said because "one row per leaf that has
	// ever stood here" reads like an unbounded log. A row is written for a leaf INDEX, once,
	// whatever happens at it afterwards -- a leaf removed, refilled and removed again is one row --
	// and RFC 9420 indexes leaves densely, so the table is at most as wide as the widest the tree
	// has ever been. Item 242's ruling 7 caps that at 1,000 leaves in v1, and a row is thirteen
	// octets.
	Leaves []LeafOccupancy

	// ── RULING 52: THIS DEVICE IS NOT A MEMBER OF THIS GROUP ANY MORE ───────────────────────
	//
	// LEDGER ITEM 257's RULING 52, AND IT IS PART TEN. A valid commit this group received removed
	// this device. [GroupRecord.WrapDarkKind] two fields up carries ruling 41's two outcomes in one
	// octet because they are two disjoint answers to ONE decision this package takes; this is a
	// third state taken at a different step by a different party -- mls, applying a commit this
	// package had already authorized -- and it gets its own part for the reason
	// [Group.removedLocked] gets its own function.
	//
	// WHY IT IS ON THE DISK, AND THIS IS THE WHOLE OF THE RULING. The state is available for exactly
	// ONE walk in the life of an MLS handle: mls closes the group and zeroizes its epoch secrets as
	// it answers, so the next walk's Process answers `the group is closed` and the walk after that
	// abandons the record. Measured over a real server at sdk ca89760, the fourth Receive and every
	// one after it answered NIL with [Stats.Omitted] at zero -- item 246's ceiling serves the rows at
	// and below the removal epoch and calls the page complete with a ceiling-relative high water, so
	// the omission predicate has nothing to report. A device thrown out of a group read as caught up
	// and silent, for ever, with a live composer. Persisting the state is what makes the answer
	// survive the one walk that can derive it.
	//
	// WHICH of the two ways, as the octet [removedByCommit] spells, or zero for "this device is
	// still a member". It is the kind and not the sentence, for [GroupRecord.WrapDarkKind]'s reason.
	RemovedKind uint8

	// The epoch [GroupRecord.RemovedKind] was taken at: the LAST epoch this device was a member
	// of, not the epoch the removing commit opened. Meaningless when that is zero.
	//
	// IT IS THE LOWER OF THE TWO NUMBERS ON PURPOSE. The epoch the commit opened is one this device
	// holds no state for and one item 246's ceiling will not serve it, so it is the wrong number to
	// render and the wrong number to put in a fetch. The epoch here is the one whose keys this
	// device still holds, whose rows it may still read, and whose transcript Spec C screen 10's
	// read-only variant shows.
	RemovedEpoch uint64
}

// LeafOccupancy is one row of [GroupRecord.Leaves]: one leaf of one group, whether THIS DEVICE has
// ever stood at it, and the epoch its occupant was removed at.
//
// IT IS A LEAF AND NOT A HANDLE, and that is a saving and a discipline at once. The handle is
// SenderHandle(group_handle_key, leaf), derivable by anybody holding the group's own lifetime key,
// so storing sixteen derived octets beside the four they come from would be a second spelling of
// one fact -- and the day the two disagreed, the stored one would win over the derivation every
// other member computes.
type LeafOccupancy struct {
	// The leaf index.
	Leaf uint32

	// DepartedEpoch is the epoch the LAST commit that REMOVED an occupant of this leaf OPENED: no
	// occupant stood at any epoch at or above it. ZERO means "not departed", which is not
	// ambiguous -- no commit opens epoch zero, because epoch zero is where a group is founded.
	//
	// A LEAF THAT CHANGED HANDS TWICE IS STILL ONE ROW, and the row carries the LAST departure
	// rather than the first. What the reader asks of it is whether a record sealed at some epoch
	// may carry this leaf's handle, which is true of every occupant it ever had; keeping the FIRST
	// departure answered `false` for the middle occupant's own records and abandoned them. See
	// [Group.noteDepartedLeavesLocked].
	DepartedEpoch uint64

	// Own is whether THIS DEVICE has stood at this leaf, and therefore whether the handle it
	// derives is one this device's own records carry.
	Own bool
}

// The values of [GroupRecord.WrapDarkKind]. Zero is "not dark" and is not a kind, so a record
// that has never been dark and a record whose kind was lost are the same state and there is no
// third reading to tell apart.
//
// THEY ARE WIRE VALUES ON A DISK THIS BUILD HAS TO KEEP READING, so they are numbered here once
// and never derived from a slice order or an iota over the sentinel list -- an inserted sentinel
// would renumber every record already written.
const (
	wrapDarkNone       uint8 = 0
	wrapDarkNoWrap     uint8 = 1
	wrapDarkUnreadable uint8 = 2
	wrapDarkOrphan     uint8 = 3
	wrapDarkRemoval    uint8 = 4
	// wrapDarkUnfollowable IS THE ONE CATCH-ALL AND IT IS ASSERTED TO HAVE ONE PRODUCER.
	// [Group.ingestCommitLocked] makes a group dark on ANY error the resolution returns, not
	// only on the three wrap sentinels, and a kind octet with a silent default would persist
	// those as HEALTHY -- the exact failure making the diagnosis durable exists to close. So
	// there is a kind for "this device could not follow the commit and the reason is not one
	// this build persists by name", and pqdarkgate_test.go's census enumerates every value the
	// resolution can return and holds each against a written disposition: a NEW refusal landing
	// here fails that gate rather than quietly becoming this.
	wrapDarkUnfollowable uint8 = 5
)

// The values of [GroupRecord.RemovedKind]. Zero is "still a member" and is not a kind, for
// [wrapDarkNone]'s reason, and they are numbered here once for the same reason those are.
const (
	removedNone uint8 = 0

	// removedByCommit: a VALID commit this group received took this device's last leaf out of the
	// tree, and [mls.Group.ApplyCommit] said so. It is the only way this state is reached today, and
	// it is a KIND rather than a bare flag so that a second way -- a group the owner dissolves, a
	// server that drops a member by some future ruling -- has somewhere to land without an eleventh
	// part, and so that epoch ZERO stays representable beside it.
	removedByCommit uint8 = 1

	// removedUnnamed is NOT WRITTEN AND IS NOT READ: it exists so that [removedKindOf] has a value
	// for "this device is out of the group and the reason is not one this build persists by name",
	// and so that [encodeRemoval] can REFUSE it. [wrapDarkUnfollowable] takes the opposite road --
	// it is a real catch-all that is written -- because that field has many producers and a refusal
	// there would turn a new refusal into a failed persist. This field has ONE producer, which
	// wraps [ErrRemovedFromGroup] itself, so a value that reaches here is a bug in this package and
	// not a state on a disk; failing the write is the loud reading, and persisting "still a member"
	// over it is the silent one this part exists to close.
	removedUnnamed uint8 = 255
)

// EpochPqSecret is one row of [GroupRecord.PqSecrets]: an epoch and the post-quantum half its
// storage root was extracted from.
type EpochPqSecret struct {
	Epoch    uint64
	PqSecret []byte
}

// EpochPqSecretWitness is one row of [GroupRecord.PqSecretWitness]: an epoch and SHA-256 of the
// pq_secret THIS DEVICE filed at it.
//
// IT IS A DIGEST AND NEVER A SECRET, and the type is separate from [EpochPqSecret] for exactly that
// reason: one field named PqSecret that sometimes holds a hash is one erase discipline away from a
// secret nobody zeroized, and one `%x` away from a leak the census could not tell from a diagnosis.
type EpochPqSecretWitness struct {
	Epoch  uint64
	Digest []byte
}

// sortEpochPqSecretWitness puts a witness in ascending epoch order, in place, for
// [sortEpochPqSecrets]'s reason: the part is one appended frame and a map's iteration order would
// make two writes of one unchanged witness two different files.
//
// IT IS THE SAME INSERTION SORT AND NOT A CALL INTO THE OTHER ONE, because the two carry different
// row types and an adapter that copied rows between them would be a place a digest could be handed
// to a function whose parameter is called a secret.
func sortEpochPqSecretWitness(rows []EpochPqSecretWitness) {
	for at := 1; at < len(rows); at += 1 {
		row := rows[at]
		back := at - 1
		for 0 <= back && row.Epoch < rows[back].Epoch {
			rows[back+1] = rows[back]
			back -= 1
		}
		rows[back+1] = row
	}
}

// encodePqSecretWitness is [GroupRecord.PqSecretWitness] as the one octet string part eight
// carries: u64(epoch) ‖ 32 octets of digest, repeated, big-endian.
//
// THE WIDTH IS FIXED AND THERE IS NO LENGTH PREFIX, which is the one place this part is spelled
// differently from the pq_secret table beside it and it is not a saving. A digest's width is this
// package's own -- [sha256.Size], decided here and not by a peer or by a wire format -- so a row of
// any other width is a record this build did not write, and a length octet would be a field whose
// only purpose is to carry a value the reader must then refuse anyway.
func encodePqSecretWitness(rows []EpochPqSecretWitness) ([]byte, error) {
	encoded := make([]byte, 0, len(rows)*(8+sha256.Size))
	for _, row := range rows {
		if len(row.Digest) != sha256.Size {
			return nil, fmt.Errorf("%w: the pq_secret witness for epoch %d is %d octets and a digest is %d",
				ErrStateStoreFormat, row.Epoch, len(row.Digest), sha256.Size)
		}
		var epochOctets [8]byte
		binary.BigEndian.PutUint64(epochOctets[:], row.Epoch)
		encoded = append(encoded, epochOctets[:]...)
		encoded = append(encoded, row.Digest...)
	}
	return encoded, nil
}

// decodePqSecretWitness reads what [encodePqSecretWitness] wrote, and refuses anything else.
//
// A SHORT TAIL IS A REFUSAL, for [decodePqSecretTable]'s reason one type over: a witness that
// silently lost its last rows is a device that comes back able to follow a removal fanned out on a
// value THIS DEVICE has held, which is the defect this part exists to close arriving through the
// reader. The holder is named rather than left to a pronoun because the rule's whole subject is
// WHOSE history is being checked.
func decodePqSecretWitness(encoded []byte) ([]EpochPqSecretWitness, error) {
	const row = 8 + sha256.Size
	if len(encoded)%row != 0 {
		return nil, fmt.Errorf("%w: the pq_secret witness is %d octets and a row is %d",
			ErrStateStoreFormat, len(encoded), row)
	}
	rows := []EpochPqSecretWitness{}
	for at := 0; at < len(encoded); at += row {
		rows = append(rows, EpochPqSecretWitness{
			Epoch:  binary.BigEndian.Uint64(encoded[at : at+8]),
			Digest: append([]byte(nil), encoded[at+8:at+row]...),
		})
	}
	return rows, nil
}

// sortLeafOccupancy puts a leaf ledger in ascending leaf order, in place, for
// [sortEpochPqSecretWitness]'s reason: the part is one appended frame, and a map's iteration order
// would make two writes of one unchanged ledger two different files for the store's rename dance to
// pay for.
//
// IT IS A THIRD COPY OF THE SAME INSERTION SORT AND THAT IS DELIBERATE, exactly as the second one
// is. The three carry three different row types over three different keys -- an epoch, an epoch and
// a LEAF -- and a shared one would either take a comparison function (paying reflection and an
// interface allocation on a handful of rows) or collapse the three row types into one, which is how
// a digest ends up in a field called a secret.
func sortLeafOccupancy(rows []LeafOccupancy) {
	for at := 1; at < len(rows); at += 1 {
		row := rows[at]
		back := at - 1
		for 0 <= back && row.Leaf < rows[back].Leaf {
			rows[back+1] = rows[back]
			back -= 1
		}
		rows[back+1] = row
	}
}

// encodeLeafOccupancy is [GroupRecord.Leaves] as the one octet string part NINE carries:
// u32(leaf) ‖ u64(departed_epoch) ‖ u8(flags), repeated, big-endian. Thirteen octets a row.
//
// THE WIDTH IS FIXED AND THERE IS NO LENGTH PREFIX, which is the witness part's shape one table
// over and for its reason: every field here is a number of this package's own choosing, so a row of
// any other width is a record this build did not write, and a length octet would be a field whose
// only purpose is to carry a value the reader must then refuse anyway.
//
// FLAGS AND NOT A BOOL OCTET, with exactly one bit defined. The two tables this part carries are
// two questions about one leaf and a third will be a third bit rather than a tenth part; a reader
// of this build refuses any other bit set, so a row written by a later build reaches this one as a
// named refusal instead of as a leaf whose ownership it has silently mis-read.
func encodeLeafOccupancy(rows []LeafOccupancy) []byte {
	encoded := make([]byte, 0, len(rows)*leafOccupancyRowBytes)
	for _, row := range rows {
		var octets [leafOccupancyRowBytes]byte
		binary.BigEndian.PutUint32(octets[0:4], row.Leaf)
		binary.BigEndian.PutUint64(octets[4:12], row.DepartedEpoch)
		if row.Own {
			octets[12] = leafOccupancyOwnBit
		}
		encoded = append(encoded, octets[:]...)
	}
	return encoded
}

// decodeLeafOccupancy reads what [encodeLeafOccupancy] wrote, and refuses anything else.
//
// A SHORT TAIL IS A REFUSAL, for [decodePqSecretWitness]'s reason one part over: a ledger that
// silently lost its last rows is a device that comes back unable to resolve a departed member's
// records, or showing its own earlier lines as a stranger's -- the two defects this part exists to
// close, arriving through the reader.
func decodeLeafOccupancy(encoded []byte) ([]LeafOccupancy, error) {
	if len(encoded)%leafOccupancyRowBytes != 0 {
		return nil, fmt.Errorf("%w: the leaf ledger is %d octets and a row is %d",
			ErrStateStoreFormat, len(encoded), leafOccupancyRowBytes)
	}
	rows := []LeafOccupancy{}
	for at := 0; at < len(encoded); at += leafOccupancyRowBytes {
		flags := encoded[at+12]
		if flags&^leafOccupancyOwnBit != 0 {
			return nil, fmt.Errorf("%w: a leaf ledger row carries flags %#02x and this build defines %#02x",
				ErrStateStoreFormat, flags, leafOccupancyOwnBit)
		}
		rows = append(rows, LeafOccupancy{
			Leaf:          binary.BigEndian.Uint32(encoded[at : at+4]),
			DepartedEpoch: binary.BigEndian.Uint64(encoded[at+4 : at+12]),
			Own:           flags&leafOccupancyOwnBit != 0,
		})
	}
	return rows, nil
}

const (
	// One [LeafOccupancy] row on the disk: u32(leaf) ‖ u64(departed_epoch) ‖ u8(flags).
	leafOccupancyRowBytes = 4 + 8 + 1

	// The one bit part nine's flags octet defines: this device has stood at this leaf.
	leafOccupancyOwnBit byte = 0x01
)

// sortEpochPqSecrets puts a table in ascending epoch order, in place.
//
// AN INSERTION SORT AND NOT sort.Slice, because the table is at most
// [messagegroup.PastEpochWindow] + 1 rows and is almost always already sorted -- it is built by
// walking a map whose keys are a short run of consecutive epochs -- so the comparison function, the
// reflection and the interface allocation would all be spent on 33 rows that are in order.
func sortEpochPqSecrets(rows []EpochPqSecret) {
	for at := 1; at < len(rows); at += 1 {
		row := rows[at]
		back := at - 1
		for 0 <= back && row.Epoch < rows[back].Epoch {
			rows[back+1] = rows[back]
			back -= 1
		}
		rows[back+1] = row
	}
}

// encodePqSecretTable is [GroupRecord.PqSecrets] as the one octet string part six carries:
// u64(epoch) ‖ u8(len) ‖ pq_secret, repeated, big-endian.
//
// IT IS ONE PART AND NOT ONE PART PER ROW, and that is not a saving. The record framing's own
// arity is a single octet ([encodeStateRecord] refuses 255 parts), so a table spread over parts
// would put a 253-epoch ceiling on this group's history inside a frame whose reader could not say
// which limit it had hit. One part is a table with its own length discipline, and the arity of the
// record stays a shape rather than a budget.
//
// THE LENGTH IS A u8 AND THE WIDTH IS NOT CHECKED AGAINST [messagegroup.PqSecretBytes] HERE, which
// is deliberate and is a rule this store already follows for every other secret it writes: the
// store frames octets and the party that knows what a value has to be is the one that uses it.
// A 32-octet check here would be a SECOND copy of connect's own -- NewGroupSession and
// InstallPqSecret both refuse a pq_secret that is not [messagegroup.PqSecretBytes], by name, at the
// moment it would become a storage root -- and two copies of one width is one of them drifting
// when the suite moves. What this refuses is what the FRAMING cannot carry: an empty value, which
// would make a row indistinguishable from a row that is not there, and one past 255, which the
// length octet cannot express.
func encodePqSecretTable(rows []EpochPqSecret) ([]byte, error) {
	encoded := make([]byte, 0, len(rows)*(8+1+messagegroup.PqSecretBytes))
	for _, row := range rows {
		if len(row.PqSecret) == 0 || 255 < len(row.PqSecret) {
			return nil, fmt.Errorf("%w: the pq_secret for epoch %d is %d octets and a row's length prefix is one octet and may not be zero",
				ErrStateStoreFormat, row.Epoch, len(row.PqSecret))
		}
		var epochOctets [8]byte
		binary.BigEndian.PutUint64(epochOctets[:], row.Epoch)
		encoded = append(encoded, epochOctets[:]...)
		encoded = append(encoded, byte(len(row.PqSecret)))
		encoded = append(encoded, row.PqSecret...)
	}
	return encoded, nil
}

// decodePqSecretTable reads what [encodePqSecretTable] wrote, and refuses anything else.
//
// A SHORT TAIL IS A REFUSAL AND NEVER A TABLE THAT ENDS EARLY. A restore that silently dropped the
// last rows of this table would be a device that comes back holding the wrong post-quantum half
// for its most recent epochs -- ruling 40's own defect, arriving through the reader -- and the
// symptom is an AEAD tag with no diagnosis anywhere. The whole part parses or the group refuses.
func decodePqSecretTable(encoded []byte) ([]EpochPqSecret, error) {
	rows := []EpochPqSecret{}
	at := 0
	for at < len(encoded) {
		if len(encoded)-at < 9 {
			return nil, fmt.Errorf("%w: the pq_secret table has %d octets left and a row's head is 9",
				ErrStateStoreFormat, len(encoded)-at)
		}
		epoch := binary.BigEndian.Uint64(encoded[at : at+8])
		width := int(encoded[at+8])
		at += 9
		if len(encoded)-at < width {
			return nil, fmt.Errorf("%w: the pq_secret for epoch %d says %d octets and %d are left",
				ErrStateStoreFormat, epoch, width, len(encoded)-at)
		}
		if width == 0 {
			return nil, fmt.Errorf("%w: the pq_secret for epoch %d is zero octets, which is a row that is not there",
				ErrStateStoreFormat, epoch)
		}
		rows = append(rows, EpochPqSecret{
			Epoch:    epoch,
			PqSecret: append([]byte(nil), encoded[at:at+width]...),
		})
		at += width
	}
	return rows, nil
}

// ── the record format ────────────────────────────────────────────────────────────────────────

// Every value this store writes is one record, and the record is self-describing so that a file
// which is not what the name says is a REFUSAL rather than a wrong answer.
//
// The name of a file is a hash of its key, so two keys that collided -- or a directory somebody
// copied from another device -- would otherwise be read as the value that was asked for. The key
// octets are therefore INSIDE the record and are compared against the key the caller supplied, on
// every read.
const (
	stateRecordMagic   = "URMSTATE"
	stateRecordVersion = byte(0x01)
)

// The kinds. A record read under the wrong kind is refused, so a private key file renamed over a
// group state cannot be handed to LoadGroup as an epoch.
const (
	stateKindGroupState     byte = 1
	stateKindPrivateKey     byte = 2
	stateKindKeyPackage     byte = 3
	stateKindDeviceIdentity byte = 4
	stateKindGroupRecord    byte = 5
	stateKindSentRecord     byte = 6
	stateKindPeerHeads      byte = 7
	stateKindForgetting     byte = 8
)

// encodeStateRecord frames one record: magic, version, kind, the parts each length-prefixed, and
// a SHA-256 over every octet before it.
//
// The checksum is NOT a security property and must not be read as one -- anything that can write
// this directory can recompute it. It is here for the reason the stream store's per-record
// checksum is: a file that a filesystem returned altered is refused instead of being decoded into
// a key schedule that then fails somewhere with nothing to point at.
func encodeStateRecord(kind byte, parts ...[]byte) ([]byte, error) {
	if len(parts) > 255 {
		return nil, fmt.Errorf("%w: a record of kind %d carries %d parts", ErrStateStoreFormat, kind, len(parts))
	}
	body := bytes.NewBuffer(nil)
	body.WriteString(stateRecordMagic)
	body.WriteByte(stateRecordVersion)
	body.WriteByte(kind)
	body.WriteByte(byte(len(parts)))
	for _, part := range parts {
		if len(part) > int(^uint32(0)>>1) {
			return nil, fmt.Errorf("%w: a part of %d octets does not fit its length prefix", ErrStateStoreFormat, len(part))
		}
		var prefix [4]byte
		binary.BigEndian.PutUint32(prefix[:], uint32(len(part)))
		body.Write(prefix[:])
		body.Write(part)
	}
	sum := sha256.Sum256(body.Bytes())
	body.Write(sum[:])
	return body.Bytes(), nil
}

// decodeStateRecord reads back what encodeStateRecord wrote and refuses everything else.
//
// EVERY REFUSAL NAMES WHAT IT SAW. A truncated record, a version this build does not write, the
// wrong kind and a checksum that does not match are four different sentences, because they have
// four different causes and a caller staring at one at 2am has to be able to tell them apart.
func decodeStateRecord(raw []byte, kind byte) ([][]byte, error) {
	header := len(stateRecordMagic) + 3
	if len(raw) < header+sha256.Size {
		return nil, fmt.Errorf("%w: %d octets is shorter than an empty record", ErrStateStoreFormat, len(raw))
	}
	if string(raw[:len(stateRecordMagic)]) != stateRecordMagic {
		return nil, fmt.Errorf("%w: the first %d octets are not this store's magic", ErrStateStoreFormat, len(stateRecordMagic))
	}
	if version := raw[len(stateRecordMagic)]; version != stateRecordVersion {
		return nil, fmt.Errorf("%w: the record is at version %#02x and this build writes %#02x",
			ErrStateStoreFormat, version, stateRecordVersion)
	}
	if got := raw[len(stateRecordMagic)+1]; got != kind {
		return nil, fmt.Errorf("%w: the record is of kind %d and kind %d was asked for",
			ErrStateStoreFormat, got, kind)
	}
	sum := sha256.Sum256(raw[:len(raw)-sha256.Size])
	if !bytes.Equal(sum[:], raw[len(raw)-sha256.Size:]) {
		return nil, fmt.Errorf("%w: the record's checksum is not the hash of the record", ErrStateStoreFormat)
	}
	count := int(raw[len(stateRecordMagic)+2])
	parts := make([][]byte, 0, count)
	at := header
	end := len(raw) - sha256.Size
	for index := 0; index < count; index += 1 {
		if end-at < 4 {
			return nil, fmt.Errorf("%w: part %d of %d has no length prefix", ErrStateStoreFormat, index, count)
		}
		width := int(binary.BigEndian.Uint32(raw[at : at+4]))
		at += 4
		if width < 0 || end-at < width {
			return nil, fmt.Errorf("%w: part %d of %d announces %d octets and %d are left",
				ErrStateStoreFormat, index, count, width, end-at)
		}
		parts = append(parts, append([]byte(nil), raw[at:at+width]...))
		at += width
	}
	if at != end {
		return nil, fmt.Errorf("%w: %d octets stand after the last part", ErrStateStoreFormat, end-at)
	}
	return parts, nil
}

// ── opening ──────────────────────────────────────────────────────────────────────────────────

// The data directory, and the guard entry BESIDE it. See [DurableStateStore] for why the guard is
// not inside the directory it excludes.
const (
	stateDataDirName = "state"
	stateGuardName   = "single-writer.lock"

	// The prefix every in-flight write wears. IT IS A CONSTANT BECAUSE THREE PLACES HAVE TO
	// AGREE ON IT: writeRecord makes them, OpenDurableStateStore sweeps them, and
	// deleteEpochsLocked has to remove them before it can say a directory is empty of state.
	// A second spelling of this string is a file nothing ever deletes, which is exactly the
	// defect the sweep exists for.
	stateTempPrefix = ".writing-"
)

// stateStoreGuardPath is the ONE place the guard's location is decided, so there is no second
// spelling of it to drift. This is [sdk.StreamStore]'s streamStoreGuardPath one package over and
// the reason is the same one.
func stateStoreGuardPath(dir string) string {
	return filepath.Join(dir, stateGuardName)
}

// OpenDurableStateStore opens the store at dir, creating it if it is not there.
//
// THE EXCLUSION IS ACQUIRED BEFORE ANYTHING IS READ AND BEFORE ANYTHING IS WRITTEN, which is the
// stream store's ordering and is here for the same reason: a second opener that had already read
// the directory has already made a decision on state it does not own.
//
// It is refused on a platform where this build can hold no exclusion, rather than opened with the
// property quietly deleted by a build constraint.
func OpenDurableStateStore(dir string) (*DurableStateStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("%w: a durable state store needs a directory", ErrStateStoreState)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: the store directory %s could not be created: %v", ErrStateStoreState, dir, err)
	}
	exclusion, err := acquireStateStoreExclusion(dir)
	if err != nil {
		return nil, err
	}
	released := false
	defer func() {
		if !released {
			exclusion.Close()
		}
	}()
	dataDir := filepath.Join(dir, stateDataDirName)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: the data directory %s could not be created: %v", ErrStateStoreState, dataDir, err)
	}
	unswept, err := sweepStateTempFiles(dataDir)
	if err != nil {
		return nil, err
	}
	released = true
	return &DurableStateStore{dir: dir, dataDir: dataDir, exclusion: exclusion, unswept: unswept}, nil
}

// UnsweptWrites is every leftover write [sweepStateTempFiles] found at open and could not remove,
// by path. Empty on the ordinary open, which is every open on a machine where nothing else is
// holding this directory's files.
//
// IT EXISTS SO THAT "THE SWEEP DID NOT REFUSE THE OPEN" IS NOT THE SAME SENTENCE AS "THERE WAS
// NOTHING TO SWEEP". A caller that wants to tell a user, or a probe that wants to assert the clean
// case, has one place to read it; nothing in this package makes a decision on it.
func (self *DurableStateStore) UnsweptWrites() []string {
	self.lock.Lock()
	defer self.lock.Unlock()
	return append([]string(nil), self.unswept...)
}

// sweepStateTempFiles removes every in-flight write left behind by a process that died.
//
// WHY THERE IS ANYTHING TO SWEEP. [DurableStateStore.writeRecord] is temp file, fsync, rename, and
// the `defer` that removes the temp on a failure only runs if the CALL RETURNS. A process killed
// between CreateTemp and Rename returns from nothing, and what it leaves in the epoch directory is
// a `.writing-XXXXXXXX` holding a COMPLETE epoch state -- this member's leaf HPKE private key and
// its whole TreeKEM path-secret ladder -- that decodes cleanly under decodeStateRecord. Measured
// with a real Process.Kill: it is not debris, it is a readable copy.
//
// AND NOTHING USED TO REMOVE IT, WHICH IS THE HALF THAT MATTERS. It is not epoch-named, so
// deleteEpochsLocked walked straight past it, and SO DID THE RE-READ that is sold as making
// "deleted" a measurement -- section 5.12's total erase reported success over a file holding the
// keys it had just promised to discard. The sweep here and the refusal in deleteEpochsLocked are
// the two halves of closing that, and they are deliberately in two places: this one bounds how
// long a leftover can live (one open), that one makes an erase that cannot see something REFUSE
// rather than report success.
//
// IT IS AN UNLINK AND NOT AN ERASE, which is this store's discipline everywhere and is stated
// rather than implied: the octets are still wherever the filesystem put them. See
// [DurableStateStore]'s header for what does and does not protect them.
//
// IT RUNS UNDER THE EXCLUSION AND BEFORE ANYTHING IS READ, so it can never race a live WRITER: the
// only process that may hold this directory for writing is this one, and it has not written yet.
// THAT SENTENCE USED TO SAY "a live writer: the only process that may hold this directory is this
// one", and it was reasoning about the wrong party -- the exclusion binds writers, and the party
// that breaks this is a third-party READER.
//
// A LEFTOVER THAT WILL NOT DELETE IS A WARNING AND NOT A REFUSAL, AND THAT IS THE DECISION.
// On Windows `os.Remove` of a file another handle holds without FILE_SHARE_DELETE answers
// ERROR_ACCESS_DENIED -- the same mechanism, from the same causes (a scanner, the search indexer, a
// backup agent), that `statestore_rename_windows_test.go` pins for MoveFileEx. Returning that error
// from here made the WHOLE STORE FAIL TO OPEN: a crash-recovery path turning a recoverable state
// into an unopenable one, and a transient third-party handle turning into "the app does not start".
// It was measured; it is not a hypothesis.
//
// WHY NON-FATAL IS RIGHT HERE AND FATAL IS STILL RIGHT IN deleteEpochsLocked, because the two look
// alike and are not. THE DIFFERENCE IS WHOSE PROMISE IT IS. §5.12's erase is a caller asking for
// octets to be gone, so an erase that cannot see a file MUST refuse rather than report success --
// that is the defect this store was carrying and it stays closed. Nobody asked this function for
// anything: it is opportunistic hygiene that bounds how long a leftover lives, and a leftover it
// cannot remove today is removed at the next open. Refusing the open makes the debris no smaller
// and costs the user their device.
//
// WHAT IS NOT SILENT. The paths are carried out to [DurableStateStore.UnsweptWrites], so a caller
// that wants to say something about them can, and the clean case is assertable rather than assumed.
// A WALK that fails is still fatal: a data directory this process cannot even enumerate is not a
// leftover problem, and the store is about to need that directory for every read it performs.
//
// NO RETRY BUDGET IS INVENTED HERE. S2-29's repair at writeRecord's rename is a bounded retry on
// ERROR_ACCESS_DENIED and ERROR_SHARING_VIOLATION, and 1141236's successor deliberately did not
// take it because the budget is somebody's to own; inventing one here, on a path where carrying on
// is free, would be taking that decision sideways. The day S2-29's retry lands, this site is the
// second caller of the same primitive.
func sweepStateTempFiles(dataDir string) ([]string, error) {
	unswept := []string{}
	err := filepath.WalkDir(dataDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%w: %s could not be walked for leftover writes: %v",
				ErrStateStoreState, dataDir, err)
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), stateTempPrefix) {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			unswept = append(unswept, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return unswept, nil
}

// Close releases the store, and with it the single-writer exclusion, which is the only thing that
// releases it other than the death of this process.
//
// A closed store stops answering rather than answering an empty value, for [MemoryStateStore]'s
// reason and the stream store's: a store that answered "no group state" after it was closed would
// send a live device off to re-join a group it is already in.
func (self *DurableStateStore) Close() error {
	self.lock.Lock()
	defer self.lock.Unlock()
	if self.closed {
		return nil
	}
	self.closed = true
	return self.exclusion.Close()
}

// flushCount is how many value fsyncs this store has performed. It is unexported and read only
// by this package's own tests, for the reason the stream store gives: it is an OBSERVABLE of where
// the work happens, not part of the interface.
func (self *DurableStateStore) flushCount() int {
	self.lock.Lock()
	defer self.lock.Unlock()
	return self.flushes
}

func (self *DurableStateStore) refuseIfClosed() error {
	if self.closed {
		return fmt.Errorf("%w: this store is closed", ErrStateStoreState)
	}
	return nil
}

// ── the paths ────────────────────────────────────────────────────────────────────────────────

// stateNameOf is the file name one key gets: the SHA-256 of the key octets, hex.
//
// FIXED WIDTH RATHER THAN THE KEY ITSELF, and that is a decision about the FILESYSTEM and not
// about secrecy -- a key package ref and a group id are public-ish values and hex would carry
// them fine, but mls's interface admits a key of any width and a name is bounded at 255 octets on
// every filesystem that matters. The key octets are inside the record and are compared on read,
// so the hash is a name and never an identity: two keys that hashed alike are refused rather than
// confused.
func stateNameOf(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

func (self *DurableStateStore) privatePath(pub []byte) string {
	return filepath.Join(self.dataDir, "priv", stateNameOf(pub))
}

func (self *DurableStateStore) keyPackagePath(ref []byte) string {
	return filepath.Join(self.dataDir, "kp", stateNameOf(ref))
}

func (self *DurableStateStore) identityPath() string {
	return filepath.Join(self.dataDir, "device")
}

func (self *DurableStateStore) groupDir(groupId []byte) string {
	return filepath.Join(self.dataDir, "group", stateNameOf(groupId))
}

func (self *DurableStateStore) groupRecordPath(groupId []byte) string {
	return filepath.Join(self.groupDir(groupId), "meta")
}

func (self *DurableStateStore) epochDir(groupId []byte) string {
	return filepath.Join(self.groupDir(groupId), "epoch")
}

// peerHeadsPath is where one group's [PeerHead] table lives: ONE file beside meta, replaced whole
// on every write, so that a build that never heard of it -- every build before ledger item 241 --
// reads the group exactly as it did. That is the whole of the format decision and it is why the
// table is not a sixth part of the group record: GroupRecords refuses a meta with any part count
// but five, so a part added there would make every group of this build unreadable to the build
// before it, and a version bump would do the same to every record in the directory at once.
func (self *DurableStateStore) peerHeadsPath(groupId []byte) string {
	return filepath.Join(self.groupDir(groupId), "heads")
}

// forgettingPath is the mark a leave writes BEFORE it closes one group and that the erase removes
// LAST ([DurableStateStore.MarkGroupForgetting]). While it stands the group is being left: it is
// no group to restore, and [Device.Restore] or [Device.ForgetGroup] finishes the erase.
func (self *DurableStateStore) forgettingPath(groupId []byte) string {
	return filepath.Join(self.groupDir(groupId), "forgetting")
}

// sentDir is where one group's [SentRecord] copies live, each named by its stream index as sixteen
// hex digits -- stateEpochName's shape, and for its reason: the listing's lexical order is the
// numeric order.
func (self *DurableStateStore) sentDir(groupId []byte) string {
	return filepath.Join(self.groupDir(groupId), "sent")
}

// stateEpochName is one epoch's file name: the epoch as sixteen zero-padded hex digits, so that
// the lexical order of a directory listing IS the numeric order of the epochs and
// DeleteGroupStateBefore does not have to sort to be correct.
func stateEpochName(epoch uint64) string {
	return fmt.Sprintf("%016x", epoch)
}

func stateEpochOfName(name string) (uint64, bool) {
	if len(name) != 16 {
		return 0, false
	}
	epoch, err := strconv.ParseUint(name, 16, 64)
	if err != nil {
		return 0, false
	}
	return epoch, true
}

// ── the write, which is the whole of the crash safety ────────────────────────────────────────

// writeRecord makes one value observable, or makes nothing observable.
//
// TEMP FILE, FSYNC, RENAME -- AND THE FSYNC IS BEFORE THE RENAME, which is the whole property. A
// value written in place could be observed half-written by the next process to open this store,
// and half of an epoch state is a key schedule that rebuilds into a group agreeing with nobody;
// half of a key-package record is an init private key with no encryption private key beside it.
// After the fsync returns, the temp file holds the whole value on stable storage; the rename then
// puts that whole value under the name, and a reader sees the previous whole value or this one.
//
// THIS IS WHERE IT DIFFERS FROM THE STREAM STORE NEXT DOOR, and the difference is the shape of the
// data rather than a different opinion about durability. A stream row is fixed-width append-only
// records, so the store can and does repair a torn tail in place at open time; these values are
// variable width and every write REPLACES, so there is no tail to repair and no state in which a
// partially written value has a name -- the temp file that held it is not a record name and is
// removed.
//
// THE DIRECTORY FSYNC IS WHAT MAKES THE RENAME ITSELF DURABLE, and it is a no-op on Windows; see
// syncStateDir for exactly what that costs and why there is no second discipline hiding in it.
//
// WHAT THE `defer` BELOW DOES NOT COVER, said here because it used to be nowhere: it removes the
// temp file only if this CALL RETURNS. A process killed between CreateTemp and Rename leaves a
// `.writing-*` in the record's own directory holding a complete, decodable value -- for an epoch
// state, this member's leaf private key and its path-secret ladder. [sweepStateTempFiles] at open
// is what bounds how long that lives, and deleteEpochsLocked's re-read is what stops a discard
// reporting success over one.
func (self *DurableStateStore) writeRecord(path string, kind byte, parts ...[]byte) error {
	record, err := encodeStateRecord(kind, parts...)
	if err != nil {
		return err
	}
	// the assembled record is a SECOND copy of whatever secret it carries, and this function is
	// the only thing that can still reach it once the write has returned. Erasing it costs one
	// pass and takes the copy out of the heap for the collector to move around, which is
	// mls.(*Group).persist's own discipline one layer down.
	defer zeroizeState(record)

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: %s could not be created: %v", ErrStateStoreState, dir, err)
	}
	temp, err := os.CreateTemp(dir, ".writing-*")
	if err != nil {
		return fmt.Errorf("%w: a temporary file in %s could not be created: %v", ErrStateStoreState, dir, err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		if !committed {
			temp.Close()
			os.Remove(tempPath)
		}
	}()
	// 0o600 explicitly and not CreateTemp's own mode, which is already 0o600 today -- said here
	// because "the file mode is the whole of what protects these octets" is this type's headline
	// and a value that important is not left to another package's default.
	if err := temp.Chmod(0o600); err != nil && !errors.Is(err, os.ErrInvalid) && !errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w: %s could not be given mode 0600: %v", ErrStateStoreState, tempPath, err)
	}
	if _, err := temp.Write(record); err != nil {
		return fmt.Errorf("%w: %s could not be written: %v", ErrStateStoreState, tempPath, err)
	}
	// NEVER SWALLOWED. A Put that returned after a failed flush has told a caller that a value is
	// durable when nothing recorded it, and for PutGroupState that caller is a seal that has
	// already consumed a ratchet generation.
	syncErr := temp.Sync()
	self.flushes += 1
	if syncErr != nil {
		return fmt.Errorf("%w: %s could not be flushed, so this value is not durable and must not be named: %v",
			ErrStateStoreState, tempPath, syncErr)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("%w: %s could not be closed: %v", ErrStateStoreState, tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("%w: %s could not be renamed onto %s: %v", ErrStateStoreState, tempPath, path, err)
	}
	committed = true
	return syncStateDir(dir)
}

// readRecord answers the parts of one record, or a refusal that says which of the two absences it
// is: no such value, or a value this build cannot read.
//
// THE NOT-FOUND CASE IS ITS OWN SENTINEL, which J1-4 says mls.StateStore does not give and which
// this package's callers need: [Device.Restore] has to tell "this device was never in that group"
// from "the disk is broken", and a bare error makes those one reading.
func (self *DurableStateStore) readRecord(path string, kind byte) ([][]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrStateNotFound, filepath.Base(path))
		}
		return nil, fmt.Errorf("%w: %s could not be read: %v", ErrStateStoreState, path, err)
	}
	return decodeStateRecord(raw, kind)
}

// zeroizeState overwrites one buffer this store assembled. It is this package's only erase and it
// erases what THIS process can still reach: it does not and cannot erase the file, the filesystem
// cache or whatever the allocator did with an earlier copy.
func zeroizeState(octets []byte) {
	for at := range octets {
		octets[at] = 0
	}
}

// ── mls.StateStore ───────────────────────────────────────────────────────────────────────────

// PutGroupState writes one epoch of one group.
//
// IT IS DURABLE BEFORE IT RETURNS, which is J1-11: mls persists inside the seal, before the
// ciphertext reaches its caller, precisely so that a restored member never re-draws a generation
// it has already spent. A store that buffered this would hand that guarantee back.
//
// The state is COPIED into the record this call writes, because mls erases the buffer it passed
// as soon as this returns -- the obligation on [mls.StateStore]'s own header. Nothing of the
// caller's is retained past the call: this store keeps no map.
func (self *DurableStateStore) PutGroupState(groupId []byte, epoch uint64, state []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return err
	}
	var epochOctets [8]byte
	binary.BigEndian.PutUint64(epochOctets[:], epoch)
	return self.writeRecord(filepath.Join(self.epochDir(groupId), stateEpochName(epoch)),
		stateKindGroupState, groupId, epochOctets[:], state)
}

// GetGroupState answers the state PutGroupState wrote at this epoch.
//
// The group id and the epoch inside the record are compared against the ones asked for, so a file
// that is under this name for any reason other than this store having put it there is refused
// rather than decoded.
func (self *DurableStateStore) GetGroupState(groupId []byte, epoch uint64) ([]byte, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return nil, err
	}
	parts, err := self.readRecord(filepath.Join(self.epochDir(groupId), stateEpochName(epoch)), stateKindGroupState)
	if err != nil {
		return nil, err
	}
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: a group state carries %d parts, want 3", ErrStateStoreFormat, len(parts))
	}
	if !bytes.Equal(parts[0], groupId) {
		return nil, fmt.Errorf("%w: the record under this name names group %x and group %x was asked for",
			ErrStateStoreFormat, parts[0], groupId)
	}
	if len(parts[1]) != 8 || binary.BigEndian.Uint64(parts[1]) != epoch {
		return nil, fmt.Errorf("%w: the record under this name does not name epoch %d", ErrStateStoreFormat, epoch)
	}
	return parts[2], nil
}

// DeleteGroupStateBefore ACTUALLY DELETES, and then reads the directory again to say so.
//
// §5.12 discards storage_root[n+1], write_key[n+1], eph_root[n+1] and every X-Wing wrap at once,
// and the hazard it names is the HALF erase: a surviving half looks exactly like a value somebody
// may still use, and nothing downstream reports it. So this does three things and not one.
//
//   - it removes by UNLINK and never by truncation or overwrite, so every epoch is removed whole:
//     a crash in the middle of the loop leaves some epochs and no half of one;
//   - it fsyncs the directory afterwards, so the unlinks are on stable storage rather than only
//     in the cache -- without which a crash could bring back a state this call reported gone;
//   - and it RE-READS the directory and refuses if any epoch below the cutoff survived. That is
//     the clause that makes "deleted" a measurement rather than an intention. A caller that was
//     told the discard happened, over a disk where it did not, is the exact shape §5.12 warns
//     about.
//
// Deleting NOTHING is success: mls calls this at every commit with a cutoff that is zero for the
// first thirty-two epochs of every group.
func (self *DurableStateStore) DeleteGroupStateBefore(groupId []byte, epoch uint64) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return err
	}
	return self.deleteEpochsLocked(groupId, epoch)
}

func (self *DurableStateStore) deleteEpochsLocked(groupId []byte, before uint64) error {
	dir := self.epochDir(groupId)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: %s could not be read: %v", ErrStateStoreState, dir, err)
	}
	removed := 0
	for _, entry := range entries {
		at, named := stateEpochOfName(entry.Name())
		if !named {
			// AN IN-FLIGHT WRITE LEFT BY A PROCESS THAT DIED, and section 5.12's discard
			// has to remove it or it is not a discard. It carries a complete epoch state
			// of THIS group -- writeRecord makes it in this very directory -- so leaving
			// it is leaving the leaf private key and the path-secret ladder behind after
			// reporting the erase succeeded. Anything else that is not epoch-named is left
			// alone here and REFUSED by the re-read below.
			if !strings.HasPrefix(entry.Name(), stateTempPrefix) {
				continue
			}
			if !self.skipRemove {
				if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("%w: %s is an unfinished write of group %x's epoch state and it could not be discarded: %v",
						ErrStateStoreState, entry.Name(), groupId, err)
				}
			}
			removed += 1
			continue
		}
		if before <= at {
			continue
		}
		if !self.skipRemove {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: epoch %d of group %x could not be discarded: %v",
					ErrStateStoreState, at, groupId, err)
			}
		}
		removed += 1
	}
	if removed == 0 {
		return nil
	}
	if err := syncStateDir(dir); err != nil {
		return err
	}
	// the measurement. It is a second ReadDir and it is the point of this method.
	entries, err = os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: %s could not be re-read after the discard: %v", ErrStateStoreState, dir, err)
	}
	for _, entry := range entries {
		at, named := stateEpochOfName(entry.Name())
		if !named {
			// CATEGORICAL, AND THAT IS THE REPAIR. This loop used to consider only entries
			// that parse as sixteen hex digits, so the one thing the discard could not see
			// was the one thing it also could not delete: a `.writing-*` holding a complete
			// epoch state survived the erase AND survived the measurement that is sold as
			// proving the erase happened. This store's own header says "an entry in the
			// data directory that is not a record is a finding"; this is that sentence
			// enforced instead of asserted.
			//
			// WHAT IT COSTS, because a fail-closed rule with an unnamed cost is a trap.
			// Anything a third party drops in this directory -- a .DS_Store, a Thumbs.db,
			// an antivirus quarantine stub -- makes a discard REFUSE rather than report
			// success. That is the right way round: this store must not delete octets it
			// did not write, and a discard that walked past an entry it could not read
			// would be the exact half-erase §5.12 names. The exposure is also narrow by
			// construction -- the loop above returns early when it removed NOTHING, and
			// with PastEpochWindow at 32 the alpha calls this with cutoff 0 for every
			// epoch it has, so this re-read runs only when something actually went.
			return fmt.Errorf("%w: %s stands in group %x's epoch directory after a discard below %d, and it is not an epoch this store wrote",
				ErrStateStoreState, entry.Name(), groupId, before)
		}
		if at < before {
			return fmt.Errorf("%w: epoch %d of group %x is still readable after a discard below %d",
				ErrStateStoreState, at, groupId, before)
		}
	}
	return nil
}

// PutPrivateKey writes one MLS private key under its public half.
func (self *DurableStateStore) PutPrivateKey(pub []byte, priv []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return err
	}
	return self.writeRecord(self.privatePath(pub), stateKindPrivateKey, pub, priv)
}

func (self *DurableStateStore) GetPrivateKey(pub []byte) ([]byte, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return nil, err
	}
	parts, err := self.readRecord(self.privatePath(pub), stateKindPrivateKey)
	if err != nil {
		return nil, err
	}
	if len(parts) != 2 || !bytes.Equal(parts[0], pub) {
		return nil, fmt.Errorf("%w: the record under this name is not the private key of %x", ErrStateStoreFormat, pub)
	}
	return parts[1], nil
}

// DeletePrivateKey removes one key, durably.
//
// J1-12 IS NOT CLOSED BY THIS AND THE NUMBER IS THE POINT: nothing in the corpus calls it. The
// query, so it is checkable rather than quoted -- over this workspace at the commit that adds
// this file, `grep -rn "DeletePrivateKey(" --include=*.go connect sdk | grep -v _test.go` answers
// the declaration on mls.StateStore, this body and MemoryStateStore's, and no call. So
// `PutPrivateKey` on every ProposeUpdate grows this directory without bound, exactly as it grows
// the map next door. It is implemented because the interface declares it and because a store that
// could not perform the erase would make the eventual caller a store change as well.
func (self *DurableStateStore) DeletePrivateKey(pub []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return err
	}
	return self.removeLocked(self.privatePath(pub))
}

func (self *DurableStateStore) removeLocked(path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: %s could not be removed: %v", ErrStateStoreState, path, err)
	}
	if err := syncStateDir(filepath.Dir(path)); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w: %s is still readable after it was removed", ErrStateStoreState, path)
	}
	return nil
}

func (self *DurableStateStore) PutKeyPackage(ref []byte, kp []byte, initPriv []byte, encPriv []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return err
	}
	return self.writeRecord(self.keyPackagePath(ref), stateKindKeyPackage, ref, kp, initPriv, encPriv)
}

// TakeKeyPackage is DESTRUCTIVE, which is mls.StateStore's contract and not this store's choice: a
// key package is single use, and a second join off one published package is a second device
// deriving the same init secret.
//
// THE ARRAYS IT ANSWERS ARE THIS CALL'S OWN AND ARE RETAINED NOWHERE, which is J1-5. The caller --
// `mls.JoinKeyMaterial.Zeroize`, through messagegroup's join -- ERASES exactly these three arrays
// when it is done with them, and a store that handed back storage it kept would have its own
// records wiped by a correct caller. A store that reads the file on every call cannot make that
// mistake; a store that cached would have to copy, and would have to remember to.
//
// THE REMOVE IS BEFORE THE RETURN AND ITS FAILURE IS THE CALL'S. A take that answered the material
// and could not delete it has published a single-use key package twice.
func (self *DurableStateStore) TakeKeyPackage(ref []byte) ([]byte, []byte, []byte, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return nil, nil, nil, err
	}
	path := self.keyPackagePath(ref)
	parts, err := self.readRecord(path, stateKindKeyPackage)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(parts) != 4 || !bytes.Equal(parts[0], ref) {
		return nil, nil, nil, fmt.Errorf("%w: the record under this name is not the key package of %x",
			ErrStateStoreFormat, ref)
	}
	if err := self.removeLocked(path); err != nil {
		return nil, nil, nil, err
	}
	return parts[1], parts[2], parts[3], nil
}

// ── DeviceStore ──────────────────────────────────────────────────────────────────────────────

// PutDeviceIdentity writes the four values a restart has to come back with, as ONE record.
//
// THE WRAP SEED IS REQUIRED AND ITS LENGTH IS CHECKED HERE. A write that omitted it would put the
// directory back in the state this field was added to leave -- a leaf publishing an encapsulation
// key whose private half nothing holds -- and would do it with a nil error, at the one moment the
// value is still in the caller's hand. A seed of any other length is refused for the reason
// [messagegroup.XwingKeyGenFromSeed] names its two sizes apart: 32 and 64 both expand into a well
// formed key pair, and only one of them is the pair whose public half is in leafKeys.
func (self *DurableStateStore) PutDeviceIdentity(signerPub []byte, signerPriv []byte, leafKeys []byte, wrapSeed []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return err
	}
	if len(signerPub) == 0 || len(signerPriv) == 0 || len(leafKeys) == 0 {
		return fmt.Errorf("%w: a device identity is a signature key pair and a leaf keys body, and one of the three is empty",
			ErrStateStoreFormat)
	}
	if len(wrapSeed) != messagegroup.XwingSeedSize {
		return fmt.Errorf("%w: a device identity carries a %d octet x-wing seed and this one is %d; the public half is already inside the leaf keys body and a device that stores no seed for it can never open a wrap addressed to its own leaf",
			ErrStateStoreFormat, messagegroup.XwingSeedSize, len(wrapSeed))
	}
	return self.writeRecord(self.identityPath(), stateKindDeviceIdentity, signerPub, signerPriv, leafKeys, wrapSeed)
}

// GetDeviceIdentity answers the identity record, and takes THREE parts as well as four.
//
// WHAT AN OLD STORE DOES, and it is the whole of the backward compatibility question. A directory
// written before the wrap seed existed -- the deployed alpha's is one -- holds a three part
// record. It is answered with a nil error and an EMPTY wrapSeed, because the alternative is a
// device that can never start again: this store's ONE version lever is [stateRecordVersion], read
// for every record in the directory, so spending it here would refuse the group states and the
// key packages beside the identity as well. That is the same reasoning [SentRecord.Body] gives for
// not spending it on a pre-kinds body, and the same shape [DeviceStore.PeerHeads] uses for a group
// with no head table.
//
// WHAT THE EMPTY VALUE MEANS AND WHAT IT MUST NOT CAUSE. The leaf keys body in that record
// publishes an X-Wing encapsulation key whose private half the build that minted it dropped;
// nothing on this machine can reconstruct it. Such a device restores, runs, sends and reads
// exactly as it does today, and refuses BY NAME ([ErrNoDeviceWrapKey]) the one thing it cannot do.
// MINTING A REPLACEMENT HERE WOULD BE WORSE THAN THE ABSENCE: the leaf every group's ratchet tree
// holds carries the OLD public half, so a fresh seed opens nothing either and turns a refusal that
// names its cause into a decapsulation that silently answers the wrong secret.
func (self *DurableStateStore) GetDeviceIdentity() ([]byte, []byte, []byte, []byte, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return nil, nil, nil, nil, err
	}
	parts, err := self.readRecord(self.identityPath(), stateKindDeviceIdentity)
	if err != nil {
		if errors.Is(err, ErrStateNotFound) {
			return nil, nil, nil, nil, fmt.Errorf("%w: %s holds no device identity", ErrNoDeviceIdentity, self.dir)
		}
		return nil, nil, nil, nil, err
	}
	switch len(parts) {
	case 3:
		return parts[0], parts[1], parts[2], nil, nil
	case 4:
		if len(parts[3]) != messagegroup.XwingSeedSize {
			return nil, nil, nil, nil, fmt.Errorf("%w: this device identity's x-wing seed is %d octets and a seed is %d",
				ErrStateStoreFormat, len(parts[3]), messagegroup.XwingSeedSize)
		}
		return parts[0], parts[1], parts[2], parts[3], nil
	default:
		return nil, nil, nil, nil, fmt.Errorf("%w: a device identity carries %d parts, want 4 or the 3 a store written before the x-wing seed holds",
			ErrStateStoreFormat, len(parts))
	}
}

func (self *DurableStateStore) PutGroupRecord(record *GroupRecord) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return err
	}
	if record == nil {
		return fmt.Errorf("%w: no group record", ErrStateStoreFormat)
	}
	if len(record.GroupId) != GroupIdBytes {
		return fmt.Errorf("%w: a group id is %d octets and this one is %d",
			ErrStateStoreFormat, GroupIdBytes, len(record.GroupId))
	}
	if len(record.PqSecret) == 0 || len(record.GroupHandleKey) == 0 {
		return fmt.Errorf("%w: a group record with no pq_secret or no group_handle_key is a group no session can be rebuilt for",
			ErrStateStoreFormat)
	}
	var epochOctets [8]byte
	binary.BigEndian.PutUint64(epochOctets[:], record.Epoch)
	flags := []byte{0}
	if record.Opened {
		flags[0] = 1
	}
	// THE SIXTH PART, AND A RECORD WITH NO TABLE IS STILL WRITTEN WITH ONE -- empty. The reader
	// tells the shapes apart by ARITY and not by content, so a six-part record whose table is
	// empty is a record this build wrote about a group it holds no per-epoch secret for, while a
	// five-part record is a record written before rotation. Collapsing the two would make "the
	// table is empty" and "there was never a table" one state, and the first is a bug in this
	// package while the second is the alpha's disk.
	//
	// AND A CALLER THAT SUPPLIED ONLY THE SCALAR GETS THE ROW THAT SCALAR IS. [GroupRecord] is an
	// exported type and the field it used to have one of is still there; a caller filling in
	// PqSecret and leaving PqSecrets nil has said exactly one true thing -- "this group's secret
	// at this epoch is these octets" -- and writing that as an EMPTY table would produce a record
	// this package's own restore then refuses, which is a footgun built out of a compatibility
	// field. The two shapes are one row either way.
	rows := record.PqSecrets
	if rows == nil {
		rows = []EpochPqSecret{{Epoch: record.Epoch, PqSecret: record.PqSecret}}
	}
	// A TABLE THAT DOES NOT COVER THE RECORD'S OWN EPOCH IS REFUSED HERE AND NOT AT THE READ.
	// A group restored without pq_secret at the epoch its session is built at seals every record
	// under a storage root no peer reproduces, silently -- ruling 40's defect with a restart in
	// front of it -- and the write is the last moment the caller that could fix it is still on the
	// stack. The read refuses it too, because a file can be hand-edited between the two.
	covered := false
	for _, row := range rows {
		if row.Epoch == record.Epoch {
			covered = true
			break
		}
	}
	if !covered {
		return fmt.Errorf("%w: this group record stands at epoch %d and its pq_secret table holds %d row(s), none of them that epoch's",
			ErrStateStoreFormat, record.Epoch, len(rows))
	}
	table, err := encodePqSecretTable(rows)
	if err != nil {
		return err
	}
	// THE SEVENTH PART, AND IT IS WRITTEN ON EVERY RECORD RATHER THAN ONLY ON A DARK ONE. The
	// reader tells shapes apart by ARITY, which is the rule the sixth part's paragraph above
	// states; a part written only when a group is dark would make the arity depend on the
	// group's STATE, so "six parts" would mean "this build, healthy" and two records of one
	// group would have two shapes. Nine octets when dark, empty when not.
	dark, err := encodeWrapDark(record.WrapDarkKind, record.WrapDarkEpoch)
	if err != nil {
		return err
	}
	// THE EIGHTH PART, AND IT IS WRITTEN ON EVERY RECORD FOR THE SEVENTH'S REASON: the reader tells
	// shapes apart by ARITY, so a part whose presence depended on the group's history would make
	// two records of one group two shapes. Empty for a group that has witnessed nothing, which is
	// a real state -- a caller that filled in the compatibility scalar alone has said nothing about
	// what this group has EVER held, and inventing a witness row from the one row it did supply
	// would be this store deciding a security question on the caller's behalf.
	witness, err := encodePqSecretWitness(record.PqSecretWitness)
	if err != nil {
		return err
	}
	// THE NINTH PART, AND IT IS WRITTEN ON EVERY RECORD FOR THE SEVENTH'S AND EIGHTH'S REASON: the
	// reader tells shapes apart by ARITY, so a part whose presence depended on whether this group
	// had ever removed anybody would make two records of one group two shapes. Empty for a group
	// whose ledger a caller supplied nothing for, which is a real state and is NOT invented around:
	// a caller that filled in the compatibility fields alone has said nothing about which leaves
	// this device has stood at, and seeding a row from the group's current membership here would be
	// this store answering, on the caller's behalf, the one question [Device.restoreOne] answers
	// where a handle can actually be derived.
	ledger := encodeLeafOccupancy(record.Leaves)
	// THE TENTH PART, AND IT IS WRITTEN ON EVERY RECORD FOR THE SEVENTH THROUGH NINTH'S REASON: the
	// reader tells shapes apart by ARITY, so a part written only for a device that had been removed
	// would make "nine parts" mean "this build, still a member" and give two records of one group two
	// shapes -- and the one record that would then be short is the one written by the group this
	// state is about. Nine octets when removed, empty when not.
	removal, err := encodeRemoval(record.RemovedKind, record.RemovedEpoch)
	if err != nil {
		return err
	}
	return self.writeRecord(self.groupRecordPath(record.GroupId), stateKindGroupRecord,
		record.GroupId, record.PqSecret, record.GroupHandleKey, epochOctets[:], flags, table, dark,
		witness, ledger, removal)
}

// encodeWrapDark is [GroupRecord.WrapDarkKind] and [GroupRecord.WrapDarkEpoch] as the one octet
// string part seven carries: empty for a group that is not dark, or u8(kind) ‖ u64(epoch).
//
// THE KIND IS REFUSED RATHER THAN CLAMPED. A value this build does not name is a caller -- or a
// later build sharing this type -- writing a diagnosis the reader would have to invent a meaning
// for, and inventing one is how "not dark" gets written over a dark group.
func encodeWrapDark(kind uint8, epoch uint64) ([]byte, error) {
	switch kind {
	case wrapDarkNone:
		return nil, nil
	case wrapDarkNoWrap, wrapDarkUnreadable, wrapDarkOrphan, wrapDarkRemoval, wrapDarkUnfollowable:
	default:
		return nil, fmt.Errorf("%w: wrap_dark kind %d is not one this build names", ErrStateStoreFormat, kind)
	}
	encoded := make([]byte, 0, 1+8)
	encoded = append(encoded, kind)
	var epochOctets [8]byte
	binary.BigEndian.PutUint64(epochOctets[:], epoch)
	return append(encoded, epochOctets[:]...), nil
}

// decodeWrapDark reads what [encodeWrapDark] wrote, and refuses anything else.
//
// AN EMPTY PART IS "not dark" AND IS THE ONLY SHORT SHAPE ADMITTED. A part of any other length is
// a record this build did not write, and answering "not dark" for it would be answering the
// safest-sounding thing about a file that has been altered.
func decodeWrapDark(part []byte) (uint8, uint64, error) {
	if len(part) == 0 {
		return wrapDarkNone, 0, nil
	}
	if len(part) != 1+8 {
		return 0, 0, fmt.Errorf("%w: the wrap_dark part is %d octets and it is either empty or %d",
			ErrStateStoreFormat, len(part), 1+8)
	}
	switch part[0] {
	case wrapDarkNoWrap, wrapDarkUnreadable, wrapDarkOrphan, wrapDarkRemoval, wrapDarkUnfollowable:
	default:
		return 0, 0, fmt.Errorf("%w: the wrap_dark part names kind %d, which is not one this build names",
			ErrStateStoreFormat, part[0])
	}
	return part[0], binary.BigEndian.Uint64(part[1:]), nil
}

// encodeRemoval is [GroupRecord.RemovedKind] and [GroupRecord.RemovedEpoch] as the one octet string
// part TEN carries: empty for a device that is still a member, or u8(kind) ‖ u64(epoch).
//
// IT IS [encodeWrapDark]'s SHAPE AND NOT A SHARED CALL, for [sortEpochPqSecretWitness]'s reason one
// field over: the two parts carry different kind spaces, and one function taking whichever space its
// caller happened to mean is a place a wrap_dark kind could be written into the removal part and read
// back as a removal. Nine octets when removed, empty when not, on EVERY record.
//
// THE KIND IS REFUSED RATHER THAN CLAMPED, and the refusal reaches further here than it does for the
// wrap: [removedKindOf] answers [removedUnnamed] for a non-nil state whose sentinel this build does
// not recognise, which cannot happen from inside this package and would be a bug if it did. Refusing
// it fails the persist loudly. Writing [removedNone] instead would record "this device is still a
// member" over a device that is not, which is the silence part ten exists to end.
func encodeRemoval(kind uint8, epoch uint64) ([]byte, error) {
	switch kind {
	case removedNone:
		return nil, nil
	case removedByCommit:
	default:
		return nil, fmt.Errorf("%w: removal kind %d is not one this build names", ErrStateStoreFormat, kind)
	}
	encoded := make([]byte, 0, 1+8)
	encoded = append(encoded, kind)
	var epochOctets [8]byte
	binary.BigEndian.PutUint64(epochOctets[:], epoch)
	return append(encoded, epochOctets[:]...), nil
}

// decodeRemoval reads what [encodeRemoval] wrote, and refuses anything else.
//
// AN EMPTY PART IS "still a member" AND IS THE ONLY SHORT SHAPE ADMITTED, for [decodeWrapDark]'s
// reason: a part of any other length is a record this build did not write, and answering "still a
// member" for it would be answering the most comfortable thing about a file that has been altered.
//
// AND EPOCH ZERO IS A LEGAL VALUE HERE, which is why the kind octet is carried at all rather than a
// bare epoch with zero standing for "not removed". No commit OPENS epoch zero, so
// [LeafOccupancy.DepartedEpoch] can use zero as its sentinel; this field holds the epoch a device was
// STANDING at, and a group's founder stands at epoch zero until its first commit is merged.
func decodeRemoval(part []byte) (uint8, uint64, error) {
	if len(part) == 0 {
		return removedNone, 0, nil
	}
	if len(part) != 1+8 {
		return 0, 0, fmt.Errorf("%w: the removal part is %d octets and it is either empty or %d",
			ErrStateStoreFormat, len(part), 1+8)
	}
	if part[0] != removedByCommit {
		return 0, 0, fmt.Errorf("%w: the removal part names kind %d, which is not one this build names",
			ErrStateStoreFormat, part[0])
	}
	return part[0], binary.BigEndian.Uint64(part[1:]), nil
}

// GroupRecords walks the group directory and answers every record it holds.
//
// A DIRECTORY WITH NO meta IN IT IS SKIPPED AND NOT A REFUSAL, because that is a real state and
// not a corruption: mls writes an epoch state at NewGroup, before this package has a pq_secret to
// write beside it, so a crash between the two leaves exactly that. What it is NOT is a restorable
// group -- and a group with no record is one this device has to be re-invited to, which is what a
// missing pq_secret means whatever the MLS state says.
func (self *DurableStateStore) GroupRecords() ([]*GroupRecord, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return nil, err
	}
	root := filepath.Join(self.dataDir, "group")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %s could not be read: %v", ErrStateStoreState, root, err)
	}
	names := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	records := []*GroupRecord{}
	for _, name := range names {
		// A GROUP BEING LEFT IS NOT A GROUP TO RESTORE: its epoch states may already be gone, and a
		// restore of it would fail at every launch. [Device.Restore] finishes its erase instead.
		if _, err := os.Stat(filepath.Join(root, name, "forgetting")); err == nil {
			continue
		}
		parts, err := self.readRecord(filepath.Join(root, name, "meta"), stateKindGroupRecord)
		if err != nil {
			if errors.Is(err, ErrStateNotFound) {
				continue
			}
			return nil, err
		}
		record, err := groupRecordOf(name, parts)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// groupRecordOf is one durable group record's parts as a [GroupRecord], and the arity switch that
// lets a store written before the pq_secret table still be read.
//
// FIVE PARTS OR SIX, AND THE FIVE IS THE DEPLOYED ALPHA'S DISK. It is the same arity switch
// [DurableStateStore.GetDeviceIdentity] already takes for the x-wing seed, and it is what makes
// item 243's rotation shippable at all: a restore that refused a record written before the table
// is a device that can never start again, and every group on the alpha was written before it.
//
// A FIVE-PART RECORD LEAVES [GroupRecord.PqSecrets] NIL, and nil is the signal rather than an
// accident: a six-part record whose table happens to be empty decodes to an EMPTY SLICE, so "this
// build wrote no rows" and "there was never a table" stay two states. The first is a bug in this
// package and the second is the alpha's disk, and a reader that collapsed them would answer the
// bug with the compatibility path. [Device.restoreOne] is the one place that distinction is acted
// on.
//
// IT IS A FUNCTION RATHER THAN THE BODY OF THAT LOOP BECAUSE OF A MEASUREMENT, which is the part
// worth keeping. Inside [DurableStateStore.GroupRecords] the record's parts are a LOCAL called
// `parts` -- a generic record off a generic read -- and no census in this package could taint it:
// pqsecretgate_test.go's walk seeds a PARAMETER, and wrapseedgate_test.go's `parts` entry seeds
// the WRITE path's three functions and not this one. Mutant pq-M2 measured it: a `%x` of
// `parts[1]` -- the persisted pq_secret itself -- in this decode's own "not one this build wrote"
// refusal SURVIVED `go test ./urmessage -run '.*'` at `ok 6.6s`, with both dataflow gates PASS.
// A parameter has a name the walk can seed, so the same leak in this function is refused. The
// reader's arity switch, its two refusals and its field reads are one unit anyway, and splitting
// them out is what the writer ([DurableStateStore.PutGroupRecord]) already did.
func groupRecordOf(name string, parts [][]byte) (*GroupRecord, error) {
	if len(parts) < 5 || 10 < len(parts) {
		return nil, fmt.Errorf("%w: the group record in %s carries %d parts, want 10, the 9 a store written before the removal part holds, the 8 a store written before the leaf ledger holds, the 7 a store written before the pq_secret witness holds, the 6 a store written before the wrap_dark part holds, or the 5 a store written before the pq_secret table holds",
			ErrStateStoreFormat, name, len(parts))
	}
	if len(parts[3]) != 8 || len(parts[4]) != 1 {
		return nil, fmt.Errorf("%w: the group record in %s is not one this build wrote", ErrStateStoreFormat, name)
	}
	record := &GroupRecord{
		GroupId:        parts[0],
		PqSecret:       parts[1],
		GroupHandleKey: parts[2],
		Epoch:          binary.BigEndian.Uint64(parts[3]),
		Opened:         parts[4][0] == 1,
	}
	if 6 <= len(parts) {
		table, err := decodePqSecretTable(parts[5])
		if err != nil {
			return nil, fmt.Errorf("%w: the group record in %s: %w", ErrStateStoreFormat, name, err)
		}
		record.PqSecrets = table
	}
	// A SIX-PART RECORD IS NOT DARK AND THAT IS EVIDENCE RATHER THAN A DEFAULT: it was written
	// by a build in which a dark group could not be persisted as dark at all, so there is no
	// diagnosis on that disk to recover and none to invent. What such a device loses is named in
	// [GroupRecord.WrapDarkKind] and it is one restart's worth of groups.
	if 7 <= len(parts) {
		kind, epoch, err := decodeWrapDark(parts[6])
		if err != nil {
			return nil, fmt.Errorf("%w: the group record in %s: %w", ErrStateStoreFormat, name, err)
		}
		record.WrapDarkKind = kind
		record.WrapDarkEpoch = epoch
	}
	// AND A RECORD WITH NO WITNESS PART WITNESSES NOTHING, which is evidence and not a default: it
	// was written by a build that kept the removal rule's subject inside
	// [messagegroup.PastEpochWindow], so there is no record on that disk of what the group held
	// below the window and none to invent. What such a device loses is named in
	// [GroupRecord.PqSecretWitness] and in [Group.pqSecretWitness], and it is one restart's worth
	// of pre-window history per group.
	if 8 <= len(parts) {
		witness, err := decodePqSecretWitness(parts[7])
		if err != nil {
			return nil, fmt.Errorf("%w: the group record in %s: %w", ErrStateStoreFormat, name, err)
		}
		record.PqSecretWitness = witness
	}
	// AND A RECORD WITH NO LEAF LEDGER KNOWS ONE LEAF, which is evidence and not a default: it was
	// written by a build in which a device's handle was one value and a departed leaf was nothing at
	// all, so there is no ledger on that disk and none to invent HERE -- inventing one needs a
	// group_handle_key expansion, and this function decodes octets. [Device.restoreOne] is where the
	// one leaf this device stands at becomes the set, which is what every build before this one
	// held. What such a device loses is named at [GroupRecord.Leaves].
	if 9 <= len(parts) {
		ledger, err := decodeLeafOccupancy(parts[8])
		if err != nil {
			return nil, fmt.Errorf("%w: the group record in %s: %w", ErrStateStoreFormat, name, err)
		}
		record.Leaves = ledger
	}
	// AND A RECORD WITH NO REMOVAL PART SAYS THIS DEVICE IS STILL A MEMBER, which is evidence and not
	// a default: it was written by a build in which a removed device's state died with the process, so
	// there is nothing on that disk to recover and nothing to invent. What such a device loses is
	// named at [GroupRecord.RemovedKind] and it is bounded to ONE WALK rather than to the device: the
	// removing commit is still the first record above its cursor, the cursor is not persisted, and the
	// MLS state on the disk still stands at the epoch before the removal -- so the first
	// [Group.Receive] after the restore re-derives the state from mls and [Group.removedLocked] writes
	// part ten. Until that walk runs the group reads as a member, which is exactly what every build
	// before this one held. Driven by
	// TestAStoreWrittenBeforeTheRemovalPartStillStartsAndTheFirstWalkFilesTheRemoval.
	if len(parts) == 10 {
		kind, epoch, err := decodeRemoval(parts[9])
		if err != nil {
			return nil, fmt.Errorf("%w: the group record in %s: %w", ErrStateStoreFormat, name, err)
		}
		record.RemovedKind = kind
		record.RemovedEpoch = epoch
	}
	return record, nil
}

// DeleteGroupRecord removes one group's record AND every MLS epoch state beside it.
//
// BOTH HALVES, for DeleteGroupStateBefore's reason: a record with no epoch state is a restore that
// fails at LoadGroup, and an epoch state with no record is this device's leaf private key and its
// whole path-secret ladder left on the disk for a group it has left. The epoch discard runs FIRST
// and its failure is the call's, so there is no path on which the record is gone and the keys are
// not.
func (self *DurableStateStore) DeleteGroupRecord(groupId []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return err
	}
	if err := self.deleteEpochsLocked(groupId, ^uint64(0)); err != nil {
		return err
	}
	// the copies of what this device said, SECOND and before the record for the same reason the
	// epochs go first: a failure leaves a group that can still be restored and left again, never a
	// directory of plaintext lines belonging to a group this device no longer knows it was in.
	if err := self.deleteSentLocked(groupId); err != nil {
		return err
	}
	// the head table THIRD, before the record and after the secrets: it is not secret, so its
	// order matters only for the directory being empty at the end, and a table left behind is a
	// directory the best-effort remove below cannot take.
	if err := self.removeLocked(self.peerHeadsPath(groupId)); err != nil {
		return err
	}
	if err := self.removeLocked(self.groupRecordPath(groupId)); err != nil {
		return err
	}
	// THE MARK A LEAVE WROTE GOES LAST, so an erase interrupted anywhere above leaves it standing,
	// and the group is finished rather than restored ([Device.Restore], [Device.ForgetGroup]). A
	// group with no mark is erased all the same: removing a mark that is not there is not a failure.
	if err := self.removeLocked(self.forgettingPath(groupId)); err != nil {
		return err
	}
	// the now-empty epoch, sent and group directories, best effort: an empty directory is not a
	// value anybody can read, so a failure to remove one is not a failure of this call.
	os.Remove(self.epochDir(groupId))
	os.Remove(self.sentDir(groupId))
	os.Remove(self.groupDir(groupId))
	return nil
}

// MarkGroupForgetting writes the mark that says this group is being LEFT, durably, before it
// returns. It does not ask for a record: a group founded and never opened has none yet and may
// still have epoch states on the disk, which are key material the erase must take.
//
// IT IS WHAT MAKES A LEAVE RESUMABLE (msgrepo ledger §7, 2026-10-03, review H1). The erase
// removes the epoch states first, so an erase that fails or is interrupted part way leaves a group
// that cannot be restored and, unmarked, could not be named again: closed and dropped by the
// process that tried, refused by every Restore after it, and still holding this device's own
// lines on the disk. With the mark it is neither restored nor lost: [Device.Restore] and
// [Device.ForgetGroup] both finish it.
func (self *DurableStateStore) MarkGroupForgetting(groupId []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return err
	}
	return self.writeRecord(self.forgettingPath(groupId), stateKindForgetting, groupId)
}

// GroupBeingForgotten reports whether that mark stands for one group.
func (self *DurableStateStore) GroupBeingForgotten(groupId []byte) (bool, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return false, err
	}
	if _, err := os.Stat(self.forgettingPath(groupId)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%w: %v", ErrStateStoreState, err)
	}
	return true, nil
}

// GroupsBeingForgotten is every group that mark stands for, by the group id the mark itself
// carries: the name of a directory is a hash and cannot be turned back into an id. A non-nil error
// beside a list reports a mark it skipped.
func (self *DurableStateStore) GroupsBeingForgotten() ([][]byte, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return nil, err
	}
	root := filepath.Join(self.dataDir, "group")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %s could not be read: %v", ErrStateStoreState, root, err)
	}
	// A MARK THAT CANNOT BE READ, or that names another directory, is REPORTED and skipped: one bad
	// mark does not stop every other leave from being finished. Its group is not restored either
	// (GroupRecords skips any directory with a mark), so it waits for somebody to look.
	ids := [][]byte{}
	var bad error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		parts, err := self.readRecord(filepath.Join(root, entry.Name(), "forgetting"), stateKindForgetting)
		if err != nil {
			if !errors.Is(err, ErrStateNotFound) && bad == nil {
				bad = err
			}
			continue
		}
		// THE MARK MUST NAME THE DIRECTORY IT SITS IN: one copied in from another group's directory
		// would otherwise send the erase to a group nobody left.
		if len(parts) != 1 || stateNameOf(parts[0]) != entry.Name() {
			if bad == nil {
				bad = fmt.Errorf("%w: the leave mark in %s does not name its own group", ErrStateStoreFormat, entry.Name())
			}
			continue
		}
		ids = append(ids, parts[0])
	}
	return ids, bad
}

// PutSentRecord writes one [SentRecord], durably, before it returns.
//
// IT IS WRITE-ONCE BY INDEX AND A SECOND WRITE AT ONE INDEX IS REFUSED, because a reserver never
// hands one index out twice and this device seals once per index. A second copy at an index already
// held is therefore either a reserver that rewound -- the stream directory lost and the state
// directory kept, which [Group.Receive] refuses as [ErrIdentityInUse] -- or a caller bug, and in
// neither case may the copy of what the user said at that index be silently replaced with a
// different line.
func (self *DurableStateStore) PutSentRecord(groupId []byte, record *SentRecord) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return err
	}
	if record == nil {
		return fmt.Errorf("%w: no sent record", ErrStateStoreFormat)
	}
	if len(groupId) != GroupIdBytes {
		return fmt.Errorf("%w: a group id is %d octets and this one is %d", ErrStateStoreFormat, GroupIdBytes, len(groupId))
	}
	path := filepath.Join(self.sentDir(groupId), stateEpochName(record.StreamIndex))
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w: group %x already holds a copy of this device's record at stream index %d, and a reserver never hands an index out twice",
			ErrStateStoreState, groupId, record.StreamIndex)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s could not be examined: %v", ErrStateStoreState, path, err)
	}
	var indexOctets [8]byte
	binary.BigEndian.PutUint64(indexOctets[:], record.StreamIndex)
	var sentAtOctets [8]byte
	binary.BigEndian.PutUint64(sentAtOctets[:], uint64(record.SentAtMs))
	return self.writeRecord(path, stateKindSentRecord,
		groupId, indexOctets[:], record.BodyHash[:], sentAtOctets[:], record.Body)
}

// SentRecords answers every [SentRecord] one group holds, ascending by stream index.
//
// AN ENTRY IN THE DIRECTORY THAT IS NOT A COPY IS A REFUSAL, which is this store's discipline
// everywhere: the one exception is an unfinished write a killed process left, which the sweep at
// open already tried to remove and which is not a record. A copy whose own group id or index is not
// the one its name says is refused rather than shown, because a copy moved under another index would
// be shown as the user's line at a position where they said something else.
func (self *DurableStateStore) SentRecords(groupId []byte) ([]*SentRecord, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return nil, err
	}
	dir := self.sentDir(groupId)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []*SentRecord{}, nil
		}
		return nil, fmt.Errorf("%w: %s could not be read: %v", ErrStateStoreState, dir, err)
	}
	records := []*SentRecord{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), stateTempPrefix) {
			continue
		}
		index, named := stateEpochOfName(entry.Name())
		if !named || entry.IsDir() {
			return nil, fmt.Errorf("%w: %s stands in group %x's sent directory and it is not a copy this store wrote",
				ErrStateStoreFormat, entry.Name(), groupId)
		}
		parts, err := self.readRecord(filepath.Join(dir, entry.Name()), stateKindSentRecord)
		if err != nil {
			return nil, err
		}
		if len(parts) != 5 || len(parts[1]) != 8 || len(parts[2]) != 32 || len(parts[3]) != 8 {
			return nil, fmt.Errorf("%w: the sent record %s is not one this build wrote", ErrStateStoreFormat, entry.Name())
		}
		if !bytes.Equal(parts[0], groupId) || binary.BigEndian.Uint64(parts[1]) != index {
			return nil, fmt.Errorf("%w: the sent record under %s names group %x index %d",
				ErrStateStoreFormat, entry.Name(), parts[0], binary.BigEndian.Uint64(parts[1]))
		}
		one := &SentRecord{
			StreamIndex: index,
			SentAtMs:    int64(binary.BigEndian.Uint64(parts[3])),
			Body:        parts[4],
		}
		copy(one.BodyHash[:], parts[2])
		records = append(records, one)
	}
	sort.Slice(records, func(a, b int) bool { return records[a].StreamIndex < records[b].StreamIndex })
	return records, nil
}

// deleteSentLocked removes every copy one group holds, and then reads the directory again to say
// so -- deleteEpochsLocked's discipline, and for §5.12's reason carried one step over: a copy that
// survived a discard reported as done is the user's own words left behind for a group they left.
func (self *DurableStateStore) deleteSentLocked(groupId []byte) error {
	dir := self.sentDir(groupId)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: %s could not be read: %v", ErrStateStoreState, dir, err)
	}
	if len(entries) == 0 {
		return nil
	}
	for _, entry := range entries {
		_, named := stateEpochOfName(entry.Name())
		if !named && !strings.HasPrefix(entry.Name(), stateTempPrefix) {
			// not ours, and not removed: this store does not delete octets it did not write. The
			// re-read below refuses over it.
			continue
		}
		if !self.skipRemove {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: the sent copy %s of group %x could not be discarded: %v",
					ErrStateStoreState, entry.Name(), groupId, err)
			}
		}
	}
	if err := syncStateDir(dir); err != nil {
		return err
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%w: %s could not be re-read after the discard: %v", ErrStateStoreState, dir, err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("%w: %s still stands in group %x's sent directory after every copy was discarded",
			ErrStateStoreState, entries[0].Name(), groupId)
	}
	return nil
}

// The width of one [PeerHead] on the disk: epoch, leaf, wire byte, window and head, big endian,
// in that order.
const peerHeadOctets = 8 + 4 + 1 + 8 + 8

// PutPeerHeads writes one group's whole [PeerHead] table, replacing what was there.
//
// REPLACED WHOLE AND NOT APPENDED, because a head only ever rises and the table is small: one row
// per (peer ladder, epoch) this device has opened anything on. The record is the group id and then
// one fixed-width part per head, so a reader that finds any other shape refuses it by name.
func (self *DurableStateStore) PutPeerHeads(groupId []byte, heads []PeerHead) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return err
	}
	if len(groupId) != GroupIdBytes {
		return fmt.Errorf("%w: a group id is %d octets and this one is %d", ErrStateStoreFormat, GroupIdBytes, len(groupId))
	}
	// encodeStateRecord counts parts in a byte, so the table is bounded at 254 rows per write; a
	// device that authenticated more (peer ladders x epochs) than that in one group keeps the
	// highest-epoch rows, which are the ones a restart reads for the epochs still inside the
	// window. Rows are sorted so the choice is stated rather than left to map order.
	sorted := append([]PeerHead(nil), heads...)
	sort.Slice(sorted, func(a, b int) bool {
		if sorted[a].Epoch != sorted[b].Epoch {
			return sorted[a].Epoch < sorted[b].Epoch
		}
		if sorted[a].Leaf != sorted[b].Leaf {
			return sorted[a].Leaf < sorted[b].Leaf
		}
		if sorted[a].RetentionWire != sorted[b].RetentionWire {
			return sorted[a].RetentionWire < sorted[b].RetentionWire
		}
		return sorted[a].EphWindow < sorted[b].EphWindow
	})
	if len(sorted) > 254 {
		sorted = sorted[len(sorted)-254:]
	}
	parts := [][]byte{groupId}
	for _, head := range sorted {
		row := make([]byte, 0, peerHeadOctets)
		row = binary.BigEndian.AppendUint64(row, head.Epoch)
		row = binary.BigEndian.AppendUint32(row, head.Leaf)
		row = append(row, head.RetentionWire)
		row = binary.BigEndian.AppendUint64(row, head.EphWindow)
		row = binary.BigEndian.AppendUint64(row, head.Head)
		parts = append(parts, row)
	}
	return self.writeRecord(self.peerHeadsPath(groupId), stateKindPeerHeads, parts...)
}

// PeerHeads answers the table PutPeerHeads last wrote for one group, or an EMPTY table and no
// error when none was ever written.
//
// EMPTY AND NOT A REFUSAL is the one decision here and it is what makes an old directory readable:
// a build before ledger item 241 wrote no table, and a restore that refused the group over that
// would turn every existing device's groups into a restore failure on upgrade. What a missing
// table costs is stated on [PeerHead]: the restart tracks at 0, which is what that build did.
func (self *DurableStateStore) PeerHeads(groupId []byte) ([]PeerHead, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	if err := self.refuseIfClosed(); err != nil {
		return nil, err
	}
	parts, err := self.readRecord(self.peerHeadsPath(groupId), stateKindPeerHeads)
	if err != nil {
		if errors.Is(err, ErrStateNotFound) {
			return []PeerHead{}, nil
		}
		return nil, err
	}
	if len(parts) < 1 || !bytes.Equal(parts[0], groupId) {
		return nil, fmt.Errorf("%w: the head table under group %x's name is not that group's", ErrStateStoreFormat, groupId)
	}
	heads := make([]PeerHead, 0, len(parts)-1)
	for index, row := range parts[1:] {
		if len(row) != peerHeadOctets {
			return nil, fmt.Errorf("%w: head row %d of group %x is %d octets and this build writes %d",
				ErrStateStoreFormat, index, groupId, len(row), peerHeadOctets)
		}
		heads = append(heads, PeerHead{
			Epoch:         binary.BigEndian.Uint64(row[0:8]),
			Leaf:          binary.BigEndian.Uint32(row[8:12]),
			RetentionWire: row[12],
			EphWindow:     binary.BigEndian.Uint64(row[13:21]),
			Head:          binary.BigEndian.Uint64(row[21:29]),
		})
	}
	return heads, nil
}
