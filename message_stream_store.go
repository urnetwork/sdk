package sdk

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/urnetwork/connect/messagegroup"
)

// StreamStore is the durable stream-index reservation store spec A section 8.2 assigns to sdk,
// and the first one that has ever existed in any tree.
//
// What this file owns is the ROW -- what identifies it, what key space it belongs to, what shape
// its bytes have, and what the store answers for a row it cannot read -- and the ALLOCATION built
// on it: ReserveStreamIndex, StreamHighWater, the fsync boundary, and the two sentinels a caller
// matches with errors.Is. The mapping from these two methods onto
// connect/messagegroup.StreamIndexReserver is NOT here: section 8.2 makes the flattening from
// two []byte parameters to that package's comparable StreamKey "the implementer's", and the
// adapter that performs it is the only code in sdk that does -- so *StreamStore deliberately does
// NOT satisfy that interface, and TestStreamStoreDoesNotYetSatisfyStreamIndexReserver reports
// that as a checked fact rather than an impression.
//
// THE ROW IS ONE FILE PER STREAM KEY, INSIDE A DIRECTORY THAT HOLDS ROWS AND NOTHING ELSE.
// dir is this store's own directory. Inside it OpenStreamStore creates the ROW DIRECTORY, and
// the single-writer exclusion sits BESIDE that directory in dir, never inside it. That is a
// construction rather than an ignore-list: because nothing but a row is ever written into the row
// directory, the rule "an entry in the row directory that is not a row is a finding" can be
// categorical, with no name exempted from it. An exemption by name is the shape that goes on
// silently ignoring the second non-row somebody writes there tomorrow.
//
// One file per key rather than one file for every key is not a performance choice. It makes the
// allocation's flush a single-file flush, and it makes a damaged row cost one stream instead of
// every stream.
type StreamStore struct {
	dir    string
	rowDir string

	// keySpaceTag is this build's key-space version tag: the fixed-width, self-delimiting
	// PREFIX of every row name. See streamKeySpaceTagOf.
	keySpaceTag string

	// exclusion is the SINGLE-WRITER guard, held by the operating system on an entry that
	// sits in dir BESIDE the row directory and never inside it. It is released by Close and
	// by the death of this process, and by nothing else. See message_errors.go's
	// ErrStreamStoreLocked for why there is no liveness heuristic here.
	exclusion io.Closer

	// allocMutex is the row lock. It is held across the READ, the INCREMENT and the FLUSH of
	// one allocation, and across the read of one high-water query, so no query can observe an
	// increment before the flush that made it durable returned. It is not the exclusion: a
	// mutex is invisible to a second process, and a second process is the case CP3b's two
	// clients actually create.
	//
	// LOCK ORDER: allocMutex then stateMutex, never the other way. stateMutex is only ever
	// held across a field read or a counter increment.
	allocMutex sync.Mutex

	// verified is the prefix of each row this store has already checksummed. See
	// streamRowVerification for what it buys and what it costs. Under allocMutex.
	//
	// IT IS ALSO THE REWIND DETECTOR, AND THERE IS NO SECOND ONE. A row this store has seen
	// can only ever grow: a flushed record is not unwritten by a crash, and the one shape a
	// live writer can leave -- a torn tail -- lengthens a row rather than shortening it. So a
	// row that is ABSENT after this store read it, or SHORTER than the prefix this store
	// confirmed, went backwards under the only writer's exclusion, and persistedHighWater
	// refuses both rather than re-deriving a smaller number from what is left.
	//
	// An earlier version kept a second map of the indices this store had RETURNED to a caller
	// and compared the read against that instead. It was strictly weaker in two ways and it is
	// gone: every returned index is also in this prefix, so that map could see nothing this
	// cannot, and this prefix is seeded by the open-time scan, so it catches a rewind on a row
	// this store has only ever READ -- which is the state every row the scan found is in, and
	// the state the removed map could never see.
	//
	// ITS HORIZON IS THIS STORE'S LIFETIME AND NOT THE ROW'S, and that bound is the same one
	// S2-23 prices from the format's side: nothing durable records what a previous process
	// confirmed, the format admits no second object that could, and a row rewound BETWEEN two
	// opens is therefore indistinguishable from a row that was always that length. A rewind
	// detector that spanned restarts would need a durable high-water witness outside the row,
	// which is a format change and not a field here.
	verified map[string]streamRowVerification

	stateMutex sync.Mutex
	closed     bool

	// rowWrites counts every write this store performs against a row file. It is incremented
	// at the write site and nowhere else, so it measures WHERE the work happens rather than
	// where somebody says it happens: a store that moved the open-time repair into the read
	// path would move this counter with it.
	rowWrites int

	// rowFlushes counts every forced flush, at the flush site and nowhere else.
	rowFlushes int

	// interrupt is the injected failure point, set by tests in this package and by nothing
	// else. See streamAppendInterrupt.
	interrupt streamAppendInterrupt
}

const (
	// the row directory. The spelling is not normative; what is normative is that this
	// directory holds rows and nothing else, by construction.
	streamRowDirName = "rows"

	// The key-space tag is 8 octets of the shape digest, hex encoded, and it is FIXED WIDTH
	// and SELF-DELIMITING for one specific reason: a row written by another build must be
	// separable from a row of this build's by the NAME ALONE, without computing an identity
	// this build cannot compute. Fold the tag into the identity hash instead and a foreign
	// row becomes a file whose name this build simply never computes -- an absent row --
	// StreamHighWater answers (0, nil), and ledger item 170's hazard is back, silently.
	streamKeySpaceTagOctets = 8
	streamKeySpaceTagLen    = 2 * streamKeySpaceTagOctets

	// The identity is a SHA-256 over the key's field set, so its width is a property of the
	// FORMAT and not of the field count. That is what keeps a three-field pre-A1 row parsing
	// as "tag then identity" -- with a tag that is not this build's -- rather than falling
	// off the end of the name shape into "not a row at all", which would be the wrong
	// refusal for it.
	streamRowIdentityLen = 2 * sha256.Size

	streamRowNameLen = streamKeySpaceTagLen + streamRowIdentityLen

	// one record: the allocated index, then a checksum bound to the row that holds it.
	streamRecordIndexOctets = 8
	streamRecordSumOctets   = 8
	streamRecordWidth       = streamRecordIndexOctets + streamRecordSumOctets
)

// streamKeyType is connect/messagegroup.StreamKey as that package declares it TODAY, read
// through reflection. Every derivation below goes through this and none of them spells a field
// name, so a field added or removed in connect changes the key-space tag -- and refuses every
// row written under the old one -- rather than silently re-keying them.
func streamKeyType() reflect.Type {
	return reflect.TypeOf(messagegroup.StreamKey{})
}

func writeStreamLengthPrefixed(digest hash.Hash, octets []byte) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(octets)))
	digest.Write(size[:])
	digest.Write(octets)
}

// streamKeyShapeDigestOf derives the key space from a key type's FIELD SET: the number of
// fields, and each field's name and type in declaration order. Nothing else -- not the type's
// own name, not the package it lives in -- so "the same field set" and "the same key space" are
// the same statement. That is what lets a test derive what a pre-A1 build would have written by
// handing this the pre-A1 field set, rather than by reimplementing this function.
func streamKeyShapeDigestOf(keyType reflect.Type) [sha256.Size]byte {
	digest := sha256.New()
	digest.Write([]byte("urnetwork/sdk message stream row key space\x00"))
	var fieldCount [4]byte
	binary.BigEndian.PutUint32(fieldCount[:], uint32(keyType.NumField()))
	digest.Write(fieldCount[:])
	for i := range keyType.NumField() {
		field := keyType.Field(i)
		writeStreamLengthPrefixed(digest, []byte(field.Name))
		writeStreamLengthPrefixed(digest, []byte(field.Type.String()))
	}
	var out [sha256.Size]byte
	copy(out[:], digest.Sum(nil))
	return out
}

func streamKeySpaceTagOf(keyType reflect.Type) string {
	shape := streamKeyShapeDigestOf(keyType)
	return hex.EncodeToString(shape[:streamKeySpaceTagOctets])
}

// streamRowIdentityOf hashes the key's VALUES, field by field, under the same field set the tag
// is derived from. Every field separates a row: two streams differing in any one field get two
// rows, because a merged row hands the second stream indices the first has already used.
func streamRowIdentityOf(key reflect.Value) string {
	keyType := key.Type()
	shape := streamKeyShapeDigestOf(keyType)
	digest := sha256.New()
	digest.Write([]byte("urnetwork/sdk message stream row identity\x00"))
	digest.Write(shape[:])
	addressable := reflect.New(keyType)
	addressable.Elem().Set(key)
	for i := range keyType.NumField() {
		field := keyType.Field(i)
		value := addressable.Elem().Field(i)
		var position [4]byte
		binary.BigEndian.PutUint32(position[:], uint32(i))
		digest.Write(position[:])
		writeStreamLengthPrefixed(digest, []byte(field.Name))
		writeStreamLengthPrefixed(digest, []byte(field.Type.String()))
		switch {
		case field.Type.Kind() == reflect.Array && field.Type.Elem().Kind() == reflect.Uint8:
			writeStreamLengthPrefixed(digest, value.Slice(0, field.Type.Len()).Bytes())
		case field.Type.Kind() == reflect.Slice && field.Type.Elem().Kind() == reflect.Uint8:
			writeStreamLengthPrefixed(digest, value.Bytes())
		case value.CanUint():
			var scalar [8]byte
			binary.BigEndian.PutUint64(scalar[:], value.Uint())
			writeStreamLengthPrefixed(digest, scalar[:])
		case value.CanInt():
			var scalar [8]byte
			binary.BigEndian.PutUint64(scalar[:], uint64(value.Int()))
			writeStreamLengthPrefixed(digest, scalar[:])
		default:
			// A field kind this derivation has no octet encoding for. It is still
			// SEPARATED -- the shape digest above has already changed, so every row
			// written under the old field set is refused as a foreign key space and
			// nothing is silently re-keyed -- and the fallback only has to be
			// deterministic for the new one.
			writeStreamLengthPrefixed(digest, []byte(fmt.Sprintf("%v", value.Interface())))
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// streamRowNameOf is TAG THEN IDENTITY, in that order and never the other way, so the key space
// is readable off the name without computing the identity.
func streamRowNameOf(key reflect.Value) string {
	return streamKeySpaceTagOf(key.Type()) + streamRowIdentityOf(key)
}

func streamRowName(key messagegroup.StreamKey) string {
	return streamRowNameOf(reflect.ValueOf(key))
}

// streamKeyFromOctets is THE flattening: spec A section 8.2's positional []byte parameters onto
// StreamKey's fields, in declaration order, and this package has no other.
//
// It is also the width boundary, and the width check here is not hygiene. Both
// messagegroup.GroupHandleKey and messagegroup.SenderHandle PANIC rather than return on a
// wrong-width input, defended in that package by the argument that "nothing here is reachable
// from the network: the key is this member's own persisted derivation." A durable store is the
// thing that makes such a value a row read off a disk. This check is what keeps that argument
// true, and a panic is not a substitute for it. S2-8.
//
// The parameter COUNT is checked against the type's field count for the same reason the identity
// is derived rather than spelled: a field added or removed in connect must arrive here as a
// refusal, not as a zero-valued field nobody passed.
func streamKeyFromOctets(parts ...[]byte) (messagegroup.StreamKey, error) {
	var zero messagegroup.StreamKey
	keyType := streamKeyType()
	if len(parts) != keyType.NumField() {
		return zero, fmt.Errorf(
			"%w: the store was handed %d key parameters and %s declares %d fields",
			ErrStreamKeyWidth,
			len(parts),
			keyType.String(),
			keyType.NumField(),
		)
	}
	built := reflect.New(keyType)
	for i := range keyType.NumField() {
		field := keyType.Field(i)
		if field.Type.Kind() != reflect.Array || field.Type.Elem().Kind() != reflect.Uint8 {
			return zero, fmt.Errorf(
				"%w: parameter %d maps onto %s.%s, which is %s and holds no octets",
				ErrStreamKeyWidth,
				i,
				keyType.String(),
				field.Name,
				field.Type.String(),
			)
		}
		want := field.Type.Len()
		if len(parts[i]) != want {
			return zero, fmt.Errorf(
				"%w: parameter %d (%s.%s) is %d octets, want exactly %d; a short key padded or a long key truncated collides two streams onto one row",
				ErrStreamKeyWidth,
				i,
				keyType.String(),
				field.Name,
				len(parts[i]),
				want,
			)
		}
		reflect.Copy(built.Elem().Field(i), reflect.ValueOf(parts[i]))
	}
	key, ok := built.Elem().Interface().(messagegroup.StreamKey)
	if !ok {
		return zero, fmt.Errorf(
			"%w: the flattening did not produce a %s",
			ErrStreamKeyWidth,
			keyType.String(),
		)
	}
	return key, nil
}

// encodeStreamRecord is the row's whole format: the allocated index, then a checksum over a
// domain separator, THE ROW'S OWN NAME and the index. Binding the checksum to the name means a
// row copied under another name does not verify, so a key space cannot be laundered by a rename.
func encodeStreamRecord(rowName string, index uint64) [streamRecordWidth]byte {
	var record [streamRecordWidth]byte
	binary.BigEndian.PutUint64(record[0:streamRecordIndexOctets], index)
	digest := sha256.New()
	digest.Write([]byte("urnetwork/sdk message stream row record\x00"))
	writeStreamLengthPrefixed(digest, []byte(rowName))
	digest.Write(record[0:streamRecordIndexOctets])
	copy(record[streamRecordIndexOctets:], digest.Sum(nil)[:streamRecordSumOctets])
	return record
}

func streamRecordIndex(record []byte) uint64 {
	return binary.BigEndian.Uint64(record[0:streamRecordIndexOctets])
}

// verifyStreamRecord recomputes the record rather than checking a stored checksum against a
// second implementation of one, so the writer and the reader cannot drift apart.
func verifyStreamRecord(rowName string, record []byte) bool {
	if len(record) != streamRecordWidth {
		return false
	}
	expected := encodeStreamRecord(rowName, streamRecordIndex(record))
	differing := byte(0)
	for i := range streamRecordWidth {
		differing |= expected[i] ^ record[i]
	}
	return differing == 0
}

// streamRowClass is the three-way partition of a name in the row directory, taken over the
// NAME'S OWN SHAPE and never over intent. The tag is fixed-width and self-delimiting, so (b) and
// (c) are separated by the name alone and by nothing a build has to remember.
type streamRowClass int

const (
	// (a) a name that parses as tag then identity under THIS build's tag: an ordinary row.
	streamRowOfThisKeySpace streamRowClass = iota
	// (b) a name that parses as tag then identity under a fixed-width tag that is NOT this
	// build's, whatever its value. This is the answer a pre-A1 row gets, and it is
	// ErrStreamKeySpace. Reading this partition as "the two tags the codebase knows about"
	// would send a pre-A1 row to (c) and demand an error the store does not produce for it.
	streamRowOfAnotherKeySpace
	// (c) anything else in the row directory. Nothing legitimate is ever written there, so
	// anything found here is a real finding: ErrStreamStoreState.
	streamRowNotARow
)

func (self *StreamStore) classifyStreamRowName(name string) streamRowClass {
	if len(name) != streamRowNameLen {
		return streamRowNotARow
	}
	for i := range len(name) {
		c := name[i]
		if !('0' <= c && c <= '9') && !('a' <= c && c <= 'f') {
			return streamRowNotARow
		}
	}
	if name[:streamKeySpaceTagLen] != self.keySpaceTag {
		return streamRowOfAnotherKeySpace
	}
	return streamRowOfThisKeySpace
}

// classifyStreamRowTail is the decision procedure over the three cases a row's bytes can be in.
// It is a function of the row's LENGTH, the record width, the per-record checksum verdicts and --
// since 2026-09-12, derived below -- the sequence the verifying records spell. It has no other
// input, which is a statement about the format rather than about this implementation.
//
// Let W be the record width and L the row's length; k = L div W and r = L mod W, so the row is
// records R_1 .. R_k at offsets 0, W, .., (k-1)W followed by an r-octet partial when r > 0. Let
// f be the least j whose R_j fails its checksum, or k+1 if every record verifies.
//
//  1. an ABSENT row is (0, nil) and never reaches here; a row that is present and zero length is
//     not case 1, it is case 2's no-verifying-record sub-case and takes the same answer.
//  2. any R_j with j > f verifies                     -> case 3, corrupt body
//  3. k-f+1 >= 2, or (k-f+1 == 1 and r > 0)           -> case 3, corrupt body
//  4. otherwise                                       -> case 2, torn tail: truncate to (f-1)W
//     and answer R_{f-1}, or (0, nil) when f == 1
//
// WHY CASE 2's BOUND IS THE TWO SHAPES IN STEP 4 AND NOT "at most one failing whole record".
// The allocation path appends ONE record of exactly W octets at a W-aligned offset and flushes,
// so an interrupted append leaves exactly two shapes and no others: a trailing partial with
// every whole record verifying (k-f+1 == 0, r > 0), or a final whole record torn within its own
// octets (k-f+1 == 1, r == 0). One failing whole record WITH a partial after it is two records'
// worth of damage, which no single interrupted append can produce, so it is corruption. The
// bound is derived from the append discipline; a batched allocation that appended two records
// per flush would invalidate it without touching a line of this comment. S2-22.
//
// AND THE ONE THING THIS PROCEDURE DOES NOT DO, stated because a reader will look for it: IT
// DOES NOT DETECT TRUNCATION. A three-record row cut to half its length is R1 followed by half
// of R2. A two-record row whose second append flushed halfway is R1 followed by half of R2.
// They are BYTE-IDENTICAL, and the format admits no third input -- no header, no record count,
// no external length authority, and exactly one forced flush on the allocation path, so there is
// no second durable object that could hold a count even if one were wanted. No function of the
// row's length, the record width and the checksum verdicts can answer them differently, so this
// one does not try. Both are case 2. A rule that claims to detect truncation on this format is a
// rule that has invented an input. What that costs: an out-of-band truncation that removes whole
// FLUSHED records rewinds the high water silently, and across a restart there is nothing left to
// compare against. That is a priced residual, S2-23, not a gap.
//
// truncateTo is the length the row must be repaired to; it equals the row's current length when
// nothing is owed.
//
// AND THE FOURTH INPUT, ADDED 2026-09-12 TO CLOSE A HIGH-WATER DEFECT THE REVIEW FOUND: THE
// SEQUENCE THE VERIFYING RECORDS SPELL. The record checksum binds the index to the row's NAME and
// to nothing else -- deliberately, so that a row copied under another name does not verify -- and
// a checksum that binds only the name says nothing about WHERE in the row the record sits. So a
// record that verifies was, before this, accepted wherever it sat: plant a correctly checksummed
// record carrying index 2 over the third record of a row holding 1, 2, 3 and the store answered a
// high water of 2. The number moved BACKWARDS inside one row, silently, with no error, and the
// next allocation handed out 3 for the second time. Under spec A section 5.6 a reused
// stream_index is a reused nonce under a reused record_key -- "a total break of both AEADs for
// that record" -- so this is the hazard the whole store exists to prevent, reached through the
// store rather than around it.
//
// THE REPAIR IS THE SCAN'S, NOT THE CHECKSUM'S, and the choice between the two is not a
// preference. Binding the record's OFFSET into what it authenticates closes the same hole and
// costs something this format cannot pay: a row whose first record was removed out of band then
// has every surviving record at the wrong offset, so R_1 fails, f = 1, and the decision procedure
// answers (0, nil) and TRUNCATES THE ROW TO ZERO -- a rewind to nothing, and a destructive one,
// in place of the surviving-high-water answer name-only binding gives. Offset binding also
// destroys the one thing that makes case 2 decidable: with the offset out of the checksum a
// failing final record can only be a torn append, and with it in, a failing final record is torn
// OR shifted and the procedure has no way to tell. So the binding stays on the name and the
// SEQUENCE carries the obligation:
//
//	the indices of R_1 .. R_{f-1} are STRICTLY INCREASING, and R_1's is at least 1.
//
// It is refused outright rather than repaired, and that follows from the same append discipline
// case 2's bound is derived from. One interrupted append leaves exactly two shapes -- a trailing
// partial, or a final whole record torn within its own octets -- and a lower index sitting under a
// verifying checksum at a later offset is neither of them. It is not a shape a correct writer can
// produce at all, so there is no correct repair for it and ErrStreamStoreState is the answer.
// "At least 1" falls out of the same statement with the walk seeded at zero, and it is not
// decoration: index 0 is the answer clause 4 reserves for a stream never seen, so a record
// CARRYING zero is a durable claim that the store has allocated the value that means it has not.
//
// What this does NOT reach, said plainly because a reader will look for it: a row whose records
// were rewritten as a whole, consistently, in increasing order. That is not a regression inside a
// row, it is a forged row, and nothing a per-record checksum bound to a name can do would see it.
// It is the same residual as S2-23 and it is priced there.
//
// priorRecords and priorHighWater are the length of an ALREADY-VERIFIED PREFIX and the index its
// last record carries; tail is the row's octets from that prefix's end. A caller with nothing
// verified passes (0, 0, the whole row), which is what classifyStreamRow does. The split exists
// so a store that has already verified a row's first n records does not re-checksum them on every
// later read; see streamRowVerification for what that costs and what it buys.
func classifyStreamRowTail(
	rowName string,
	priorRecords int,
	priorHighWater uint64,
	tail []byte,
) (highWater uint64, truncateTo int64, err error) {
	tailRecords := len(tail) / streamRecordWidth
	partial := len(tail) % streamRecordWidth
	wholeRecords := priorRecords + tailRecords

	firstFailing := wholeRecords + 1
	previousIndex := priorHighWater
	for j := priorRecords + 1; j <= wholeRecords; j += 1 {
		offset := (j - priorRecords - 1) * streamRecordWidth
		record := tail[offset : offset+streamRecordWidth]
		if !verifyStreamRecord(rowName, record) {
			firstFailing = j
			break
		}
		index := streamRecordIndex(record)
		if index <= previousIndex {
			return 0, 0, fmt.Errorf(
				"%w: row %s carries index %d at position %d and index %d at position %d after it; a stream index ladder inside one row is strictly increasing and starts at 1, so an index that is not above the one before it is a high water that moved backwards, which no interrupted append can leave and which hands the next allocation a number already spent",
				ErrStreamStoreState,
				rowName,
				previousIndex,
				j-1,
				index,
				j,
			)
		}
		previousIndex = index
	}
	for j := firstFailing + 1; j <= wholeRecords; j += 1 {
		offset := (j - priorRecords - 1) * streamRecordWidth
		if verifyStreamRecord(rowName, tail[offset:offset+streamRecordWidth]) {
			return 0, 0, fmt.Errorf(
				"%w: row %s holds a record at position %d that does not verify and a verifying record at position %d after it; a failure with a verifying record after it is a corrupt body however small it is, and no interrupted append can leave one",
				ErrStreamStoreState,
				rowName,
				firstFailing,
				j,
			)
		}
	}

	failingWhole := 0
	if firstFailing <= wholeRecords {
		failingWhole = wholeRecords - firstFailing + 1
	}
	if 2 <= failingWhole {
		return 0, 0, fmt.Errorf(
			"%w: row %s ends in a failing suffix of %d whole records; one interrupted append can damage exactly one, so this is a corrupt body",
			ErrStreamStoreState,
			rowName,
			failingWhole,
		)
	}
	if failingWhole == 1 && 0 < partial {
		return 0, 0, fmt.Errorf(
			"%w: row %s ends in one failing whole record with a %d-octet partial after it, which is two records' worth of damage and is not a shape one interrupted append can leave",
			ErrStreamStoreState,
			rowName,
			partial,
		)
	}

	truncateTo = int64(firstFailing-1) * streamRecordWidth
	if firstFailing == priorRecords+1 {
		// Nothing past the verified prefix survives. With an empty prefix that is the
		// state a row is in before an index for its key has been handed out, which is the
		// same state a stream never seen is in: 0, and no error.
		return priorHighWater, truncateTo, nil
	}
	lastOffset := (firstFailing - priorRecords - 2) * streamRecordWidth
	return streamRecordIndex(tail[lastOffset : lastOffset+streamRecordWidth]), truncateTo, nil
}

// classifyStreamRow is classifyStreamRowTail over a row with no verified prefix.
func classifyStreamRow(rowName string, content []byte) (highWater uint64, truncateTo int64, err error) {
	return classifyStreamRowTail(rowName, 0, 0, content)
}

// OpenStreamStore opens, and if necessary repairs, the durable stream store rooted at dir.
//
// dir is this store's OWN directory. It is never sdk's shared LocalState home, because the next
// task's single-writer exclusion is held on an entry beside the row directory inside it, and two
// stores' guards in one shared home would collide by name and would no longer sit on the tree
// they protect.
//
// THE REPAIR IS A TRUNCATION, PERFORMED BY THE WRITER, AT OPEN, EXACTLY ONCE, BEFORE ANY INDEX
// HAS BEEN HANDED OUT, AND FORCED DURABLE BEFORE THIS RETURNS. Three consequences are the whole
// argument for putting it here:
//
//  1. It keeps the high-water read a READ. Every later classification sees a row whose length is
//     already a whole multiple of the record width and writes nothing. A repair performed lazily
//     inside the read path makes a read a write, puts a second writer to one row inside one
//     store beside a live allocation, and makes the repair's timing data-dependent.
//  2. It is on the open path, so it moves neither of the two allocation-path
//     numbers. It is not a forced flush on the allocation path and it is not a directory-entry
//     mutation at all: it is a truncation of a file that already exists.
//  3. Its own durability is not load-bearing, which is what makes it safe to do at open at all.
//     The repair hands out nothing, so a crash between the truncation and its flush leaves a row
//     the next open repairs identically. THE REPAIR IS IDEMPOTENT; AN ALLOCATION IS NOT.
//
// AND WHY IT IS A TRUNCATION RATHER THAN A SKIP, which is a safety property and not tidiness.
// Leave the torn octets in place and append at EOF instead: a crash mid-append leaves R1 then
// half of R2 at L = 1.5W; the reopened store answers R1, correctly; the next allocation appends
// a whole record at EOF, so the row becomes R1, half-R2, R2' at L = 2.5W. On the NEXT open k = 2,
// R_1 verifies, R_2 spans half-R2 and the head of R2' and fails, r = W/2, f = 2. Under a bound
// that admits one failing whole record whatever r is, the store answers R1 again -- SO IT HANDS
// OUT THE SAME INDEX IT HANDED OUT BEFORE THE RESTART, AFTER EVERY RESTART, FOR THE LIFE OF THE
// ROW. That is a reused stream_index under a reused record_key, which spec A section 5.6 calls
// "a total break of both AEADs for that record". The truncation here and the tightened bound in
// classifyStreamRow compose: under the truncation the row's length is a whole multiple of W at
// every moment an append begins, so k-f+1 == 1 with r > 0 is unreachable for a correct writer
// and is corruption when it is seen.
//
// A row whose bytes are a corrupt body is NOT repaired here. It is left exactly as it was found
// and the refusal is the reader's: a store that cannot be opened cannot be inspected, and the
// answer to a damaged row belongs where the answer is produced.
//
// Whether this repair is a sixth section 8.2 contract clause on OpenStreamStore is filed rather
// than assumed: S2-24.
// AND IT IS PERFORMED UNDER THE SINGLE-WRITER EXCLUSION, which is what makes it safe to perform
// at all. The exclusion is acquired before the scan and released only by Close or by this
// process's death, so the repair is never concurrent with anything and StreamHighWater stays a
// read for the life of the store. Without it the repair would be a second writer to a row a live
// store is appending to -- which is not a hypothetical, because OpenStreamStore is itself a
// WRITER, and before the exclusion existed a second opener against a directory a live store was
// using destroyed that store's in-flight append.
func OpenStreamStore(dir string) (*StreamStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf(
			"%w: the store directory %s could not be created: %v",
			ErrStreamStoreState,
			dir,
			err,
		)
	}
	// THE EXCLUSION IS ACQUIRED BEFORE ANYTHING IS READ AND BEFORE ANYTHING IS WRITTEN. The
	// guard entry sits in dir, BESIDE the row directory and never inside it, so the
	// enumerated object and the excluded object are different directories, one nested inside
	// the other. That is a construction rather than an exemption: the rule "an entry in the
	// row directory that is not a row is a finding" stays categorical, with no name exempted
	// from it, and a later reader does not have to know the guard's name.
	platformExclusion, err := acquireStreamStoreExclusion(dir)
	if err != nil {
		return nil, err
	}
	streamStoreNoteHeld(dir)
	exclusion := &streamStoreHeldExclusion{inner: platformExclusion, dir: dir}
	released := false
	defer func() {
		if !released {
			exclusion.Close()
		}
	}()

	rowDir := filepath.Join(dir, streamRowDirName)
	if err := os.MkdirAll(rowDir, 0o700); err != nil {
		return nil, fmt.Errorf(
			"%w: the row directory %s could not be created: %v",
			ErrStreamStoreState,
			rowDir,
			err,
		)
	}
	store := &StreamStore{
		dir:         dir,
		rowDir:      rowDir,
		keySpaceTag: streamKeySpaceTagOf(streamKeyType()),
		exclusion:   exclusion,
		verified:    map[string]streamRowVerification{},
	}
	entries, err := os.ReadDir(rowDir)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: the row directory %s could not be read: %v",
			ErrStreamStoreState,
			rowDir,
			err,
		)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		if store.classifyStreamRowName(entry.Name()) != streamRowOfThisKeySpace {
			continue
		}
		if err := store.repairRow(entry.Name()); err != nil {
			return nil, err
		}
	}
	released = true
	return store, nil
}

// streamGuardName is the single-writer guard entry. It sits in dir, beside the row directory.
//
// Its spelling is not normative and nothing reads it as data -- the exclusion is the operating
// system's hold on the handle, not anything written in the file, which stays empty forever. What
// IS normative is where it sits: see streamStoreGuardPath.
const streamGuardName = "single-writer.lock"

// streamStoreGuardPath is the ONE place the guard entry's location is decided, so there is no
// second spelling of it to drift. It is filepath.Join(dir, ...) and never
// filepath.Join(dir, streamRowDirName, ...): a guard inside the enumerated directory IS a finding
// under this store's own categorical rule, and every StreamHighWater after a successful open
// would refuse with ErrStreamStoreState.
func streamStoreGuardPath(dir string) string {
	return filepath.Join(dir, streamGuardName)
}

// ----------------------------------------------------------------------------------------------
// the single-writer exclusion's DIAGNOSTIC, which is not the exclusion
// ----------------------------------------------------------------------------------------------

// streamStoreHeldHere records which directories THIS PROCESS holds an exclusion on, so the
// refusal a second opener gets can say whether the holder is this process or another one --
// Property 1 asks for that "where the platform can tell", and neither dwShareMode nor flock tells
// you who the holder is.
//
// IT IS A MESSAGE DECORATOR AND IT IS NOT THE EXCLUSION, and the difference is the whole of why
// it is safe to have. It is consulted only AFTER the operating system has already refused, it is
// never consulted to decide whether to refuse, and an empty map changes no decision this package
// makes. An exclusion held here instead would be a package-level mutex: invisible to a second
// process, which is the case CP3b's two clients actually create, and
// `TestAtMostOneStoreAllocatesAgainstOneDirectory` is what a mutant that tried it fails on: it
// drives BOTH members of the class -- a second OpenStreamStore inside this process and a second
// PROCESS opening the same directory -- and a mutex answers only the first.
//
// It is also not a liveness oracle. It records nothing durable, it survives no process, and it
// never decides that a holder is dead.
var streamStoreHeldHereMutex sync.Mutex
var streamStoreHeldHere = map[string]bool{}

func streamStoreHolderKey(dir string) string {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}
	return filepath.Clean(absolute)
}

func streamStoreNoteHeld(dir string) {
	streamStoreHeldHereMutex.Lock()
	defer streamStoreHeldHereMutex.Unlock()
	streamStoreHeldHere[streamStoreHolderKey(dir)] = true
}

func streamStoreNoteReleased(dir string) {
	streamStoreHeldHereMutex.Lock()
	defer streamStoreHeldHereMutex.Unlock()
	delete(streamStoreHeldHere, streamStoreHolderKey(dir))
}

// streamStoreExclusionHolder is best-effort and says so in the words it produces: it can only
// distinguish "a store in this process" from "something outside this process", and it does that
// from a map this process keeps rather than from anything the platform reports.
func streamStoreExclusionHolder(dir string) string {
	streamStoreHeldHereMutex.Lock()
	defer streamStoreHeldHereMutex.Unlock()
	if streamStoreHeldHere[streamStoreHolderKey(dir)] {
		return "another StreamStore in this process"
	}
	return "a StreamStore in another process"
}

// streamStoreHeldExclusion pairs the platform's hold with the diagnostic note, so the note cannot
// outlive the hold. Close is idempotent through the sync.Once.
type streamStoreHeldExclusion struct {
	inner io.Closer
	dir   string
	once  sync.Once
}

func (self *streamStoreHeldExclusion) Close() error {
	var err error
	self.once.Do(func() {
		err = self.inner.Close()
		streamStoreNoteReleased(self.dir)
	})
	return err
}

// repairRow truncates away a torn tail, once, and only when one is owed.
func (self *StreamStore) repairRow(rowName string) error {
	path := filepath.Join(self.rowDir, rowName)
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%w: row %s could not be read: %v", ErrStreamStoreState, rowName, err)
	}
	highWater, truncateTo, err := classifyStreamRow(rowName, content)
	if err != nil {
		// A CORRUPT BODY: not repairable, and the row is left exactly as it was found. It
		// is entered into the verified map ANYWAY, as class (5) of the partition above
		// persistedHighWater, and that entry is the whole of what closes this class.
		//
		// An earlier version remembered NOTHING about it -- "the next read re-derives the
		// refusal from the bytes" -- which is true for as long as the bytes are there. A
		// row this store holds no record of is a row the rewind detector cannot see, so
		// REMOVING the corrupt row made it a stream never seen: (0, nil), and the next
		// allocation handed out index 1 on a key whose row had durably carried indices up
		// to whatever the damage hid. Emptying it in place did the same. A row that could
		// not be read at open must REFUSE, not allocate, and it must go on refusing when
		// the evidence of it is taken away.
		self.verified[rowName] = streamRowVerification{unreadable: true}
		return nil
	}
	// The full read above is what seeds the verified prefix, so this row's first high-water
	// query in this store's life costs no second pass over it and every query after that
	// costs the records appended since. A store that is reopened re-verifies from record 1,
	// which is what bounds streamRowVerification's residual to one store's lifetime.
	self.verified[rowName] = streamRowVerification{
		records:   int(truncateTo / streamRecordWidth),
		highWater: highWater,
	}
	if truncateTo == int64(len(content)) {
		return nil
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf(
			"%w: row %s could not be opened for repair: %v",
			ErrStreamStoreState,
			rowName,
			err,
		)
	}
	defer file.Close()
	self.countRowWrite()
	if err := file.Truncate(truncateTo); err != nil {
		return fmt.Errorf(
			"%w: row %s could not be truncated to %d: %v",
			ErrStreamStoreState,
			rowName,
			truncateTo,
			err,
		)
	}
	if err := self.forceFlush(file); err != nil {
		return fmt.Errorf(
			"%w: row %s could not be flushed after repair: %v",
			ErrStreamStoreState,
			rowName,
			err,
		)
	}
	return nil
}

// countRowWrite is called at the write site and nowhere else.
func (self *StreamStore) countRowWrite() {
	self.stateMutex.Lock()
	defer self.stateMutex.Unlock()
	self.rowWrites += 1
}

// forceFlush is THE ONLY PLACE THIS PACKAGE CALLS Sync, and the count is taken AFTER the call
// returns rather than before it, so the number is flushes PERFORMED and not flushes intended.
//
// That distinction is not pedantry, it is the mutation this whole wave exists for. A first
// version of this counted at the call site with the increment ABOVE the Sync, and the mutation
// "return from Reserve before the flush" -- written as deleting the Sync and leaving everything
// around it -- SURVIVED the entire suite, because the counter still said one. The count now
// cannot be reached without the flush having been attempted, and
// TestEveryForcedFlushInTheStoreIsCounted holds that there is exactly one Sync call site in this
// package's production source and that it is this one, so a second flush cannot appear anywhere
// uncounted and this one cannot be removed without the number going to zero.
func (self *StreamStore) forceFlush(file *os.File) error {
	err := file.Sync()
	self.stateMutex.Lock()
	self.rowFlushes += 1
	self.stateMutex.Unlock()
	return err
}

// rowWriteCount is the number of writes this store has performed against a row file. It is the
// observable that separates an open-time repair from a lazy one, because every ANSWER the two
// give is identical.
func (self *StreamStore) rowWriteCount() int {
	self.stateMutex.Lock()
	defer self.stateMutex.Unlock()
	return self.rowWrites
}

// rowFlushCount is the first of the two numbers section 8.2's durability property is stated over:
// the forced flushes observed on the allocation path. The second -- directory-entry mutations --
// is deliberately NOT a counter here. It is observed from outside, by comparing the row
// directory's entry set and each surviving entry's file identity across the call, because a
// self-reported number cannot see a rename and a rename is exactly what mutation 2 performs.
func (self *StreamStore) rowFlushCount() int {
	self.stateMutex.Lock()
	defer self.stateMutex.Unlock()
	return self.rowFlushes
}

// ----------------------------------------------------------------------------------------------
// the allocation path
// ----------------------------------------------------------------------------------------------

// streamAppendInterrupt is the INJECTED FAILURE POINT the durability property cannot be stated
// without. The observable difference between a durable write and a buffered one is only visible
// if the process can be stopped between them, and a test cannot stop this one -- so the append
// carries the three places a real interruption lands, and a test selects one.
//
// It is unexported, it is per store rather than per package so two parallel tests cannot see each
// other's, and NO PRODUCTION SOURCE IN THIS PACKAGE ASSIGNS IT --
// TestNoProductionSourceSetsTheAppendInterrupt reads the package's syntax tree to hold that. A
// hook that production could set is a durability property with an off switch.
type streamAppendInterrupt int

const (
	// the ordinary path: write the whole record, flush it, return.
	streamAppendUninterrupted streamAppendInterrupt = iota
	// the process dies after part of the record has reached the disk and before the flush.
	// This is the shape an interrupted append actually leaves, and the reason a test cannot
	// model it by simply skipping the flush: an unflushed write is still in the page cache
	// and a later read in the same machine's lifetime sees it, so "did not flush" and "did
	// not survive" are not the same experiment.
	streamAppendTearBeforeFlush
	// the flush itself fails. The index must not be handed out: a Reserve that returned after
	// a failed flush has handed out an index it cannot prove it recorded.
	streamAppendFailTheFlush
	// the flush returned and the process dies before Reserve does. The index is BURNED --
	// durable, never handed out, never reused -- and a burned index is a legal gap, because
	// the server enforces monotonicity and not contiguity.
	streamAppendDieAfterFlush
)

// errStreamAppendInterrupted is the synthetic death the injected failure point raises. It is not
// one of the store's four typed refusals and it is deliberately unexported: nothing outside this
// package can produce it, so nothing outside this package can branch on it.
var errStreamAppendInterrupted = errors.New("stream append interrupted")

// errStreamInjectedFlushFailure is the synthetic flush failure. Real flush failures arrive from
// the filesystem; this one lets the "never swallowed" half of the property be exercised without
// one.
var errStreamInjectedFlushFailure = errors.New("stream flush failed")

func (self *StreamStore) appendInterrupt() streamAppendInterrupt {
	self.stateMutex.Lock()
	defer self.stateMutex.Unlock()
	return self.interrupt
}

// ReserveStreamIndex is spec A section 8.2's allocator: it takes no index and returns one.
//
// IT IS ONE STATEMENT UNDER ONE LOCK -- the read, the increment and the flush -- and that is the
// whole of why this shape was chosen over one where a caller picks the number.
// connect/messagegroup/streamindex.go says it: the store that owes the persistence "cannot
// implement it atomically: a read, then a caller's decision, then a write with an fsync in it is
// a window that an allocation done in one statement does not have". allocMutex closes that window
// inside this process; the exclusion acquired at open closes it against every other one, because
// a mutex is invisible to a second process and a second process is the case CP3b's two clients
// actually create.
//
// THE FSYNC BOUNDARY, and why it is ONE forced flush and not two. The recipe a reader will expect
// -- write a temp file, Sync it, rename it over the row, Sync the DIRECTORY -- cannot be run on
// Windows: os.File.Sync on a directory handle answers "Access is denied", measured on this
// machine with this toolchain, and FlushFileBuffers on a volume handle needs administrator
// privilege and flushes the whole volume, which is not something a client SDK may do. So the
// design is constrained instead of the platform: an allocation against a row that already exists
// performs NO DIRECTORY-ENTRY MUTATION AT ALL -- no create, no rename, no remove -- and is an
// in-place durable write of a file that already exists, which os.File.Sync forces everywhere this
// ships. The one exception is exactly once per key and is the CREATE itself, inside that key's
// first ReserveStreamIndex, at a point where no index has been handed out for it. Its durability
// is the residual S2-16 prices, and it is not closed here.
//
// WHY THE RECORD GOES AT AN OFFSET THIS CALL COMPUTED rather than at EOF. The offset is the end
// of the prefix the high-water read just VERIFIED, so the record lands where the number it
// carries was derived from, as a property of the code rather than of where the file happens to
// end. It also makes the one shape a live writer can leave -- a torn tail from an interrupted
// append, strictly shorter than one record and starting at exactly this offset -- repairable by
// the next append overwriting it in place, with no second writer and no read that writes.
func (self *StreamStore) ReserveStreamIndex(groupId []byte, senderHandle []byte) (uint64, error) {
	key, err := streamKeyFromOctets(groupId, senderHandle)
	if err != nil {
		return 0, err
	}
	rowName := streamRowName(key)

	self.allocMutex.Lock()
	defer self.allocMutex.Unlock()

	if err := self.refuseIfClosed(); err != nil {
		return 0, err
	}
	persisted, err := self.persistedHighWater(rowName)
	if err != nil {
		if errors.Is(err, ErrStreamStoreRewound) {
			// BOTH NAMES, OFF ONE VALUE, because this is one state seen from two seats:
			// the reader's -- the number moved -- and the allocator's -- the next
			// position is one I have already returned, and I have no way past it. The
			// read raises the reader's name, and this is the only place in the store
			// that adds the allocator's to it.
			return 0, fmt.Errorf(
				"%w; the next index this store would allocate is one it has already returned to a caller, and a second record under a reused stream_index is a reused nonce under a reused record_key (%w)",
				err,
				ErrStreamStoreConsumed,
			)
		}
		return 0, err
	}
	if persisted == math.MaxUint64 {
		return 0, fmt.Errorf(
			"%w: row %s has spent the last index a u64 holds, so there is no next position and no later call can make one",
			ErrStreamStoreConsumed,
			rowName,
		)
	}

	next := persisted + 1
	if err := self.writeOneRecord(rowName, next); err != nil {
		return 0, err
	}
	return next, nil
}

// SeedStreamIndex raises one stream's high water to `floor` WITHOUT handing an index out, so that
// the next ReserveStreamIndex for it answers floor+1. It answers the high water the row carries
// afterwards: `floor` when the seed moved it, and the row's own number when it did not.
//
// WHY IT EXISTS, and it is ledger item 245's first piece. A sender_handle is
// SenderHandle(group_handle_key, leaf) and carries NO epoch and no identity, so a newcomer that
// lands on a leaf a removed member used to stand at inherits that member's sixteen octets --
// therefore its row here, which is keyed on (group_id, sender_handle) and on nothing else. Its
// reserver has never allocated for that row, so it starts at index 1, and index 1 under those
// octets is a stream index the SERVER already holds a claim at: the submit is answered
// REASON_STREAM_INDEX_REUSED and urmessage latches ErrIdentityInUse for the life of the process.
// The newcomer can never send in that group. Seeding the row past the highest index a walk saw
// under those octets is what makes the two occupants' index ranges DISJOINT -- and disjoint ranges
// are also what stops their message_ids colliding, because MASTER section 8.4.5 expands an id from
// (group_id, sender_handle, stream_index) and the first two are equal by construction here.
//
// IT IS NOT AN ALLOCATION AND THAT IS THE WHOLE OF ITS CONTRACT. Nothing may seal at `floor`: the
// caller is declaring that somebody ELSE has already spent every index up to it. Contract clause 1
// is a promise about indices this store HANDS OUT, and this hands none out -- it moves the floor
// the next one is taken above, which is clause 2's "HighWater never rewinds" read forwards.
//
// IT IS MONOTONE, AND A SEED BELOW THE ROW IS A NO-OP RATHER THAN A REFUSAL. The number a caller
// reads off a walk is EVIDENCE about what a stream has spent and is not an authority over it; a
// row that already stands higher holds better evidence, and rewinding it is the one thing this
// store exists to make impossible.
//
// THE LAST INDEX A u64 HOLDS IS REFUSED BY NAME rather than written. A row seeded there has no
// next position, so the seed would leave the stream permanently unallocatable -- the same state
// ReserveStreamIndex refuses above, arrived at through a call that hands out nothing and would
// otherwise report success.
func (self *StreamStore) SeedStreamIndex(groupId []byte, senderHandle []byte, floor uint64) (uint64, error) {
	key, err := streamKeyFromOctets(groupId, senderHandle)
	if err != nil {
		return 0, err
	}
	rowName := streamRowName(key)

	self.allocMutex.Lock()
	defer self.allocMutex.Unlock()

	if err := self.refuseIfClosed(); err != nil {
		return 0, err
	}
	if floor == math.MaxUint64 {
		return 0, fmt.Errorf(
			"%w: row %s cannot be seeded at the last index a u64 holds, because a row seeded there has no next position and no later call could make one",
			ErrStreamStoreConsumed,
			rowName,
		)
	}
	persisted, err := self.persistedHighWater(rowName)
	if err != nil {
		if errors.Is(err, ErrStreamStoreRewound) {
			return 0, fmt.Errorf(
				"%w; the next index this store would allocate is one it has already returned to a caller, and a seed moves a floor rather than repairing that (%w)",
				err,
				ErrStreamStoreConsumed,
			)
		}
		return 0, err
	}
	if floor <= persisted {
		return persisted, nil
	}
	if err := self.writeOneRecord(rowName, floor); err != nil {
		return 0, err
	}
	return floor, nil
}

// StreamHighWater is section 8.2's query: the highest index this store has ever allocated for the
// stream, or 0 for a stream it has never seen.
//
// IT IS ANSWERED FROM PERSISTED STATE, never from a recomputed value and never from anything a
// ratchet remembers. NewSenderRatchet reads it in its CONSTRUCTOR and walks highWater + 1 rungs,
// so a store that answered a recomputed number would place a live ladder under a counter nothing
// has recorded. It takes allocMutex, so it can never observe an increment before the flush that
// made it durable returned.
func (self *StreamStore) StreamHighWater(groupId []byte, senderHandle []byte) (uint64, error) {
	key, err := streamKeyFromOctets(groupId, senderHandle)
	if err != nil {
		return 0, err
	}
	rowName := streamRowName(key)

	self.allocMutex.Lock()
	defer self.allocMutex.Unlock()

	if err := self.refuseIfClosed(); err != nil {
		return 0, err
	}
	persisted, err := self.persistedHighWater(rowName)
	if err != nil {
		// THE READER'S SEAT, AND IT STOPS HERE. persistedHighWater raises
		// ErrStreamStoreRewound for a row that went backwards; the allocator's
		// ErrStreamStoreConsumed is added by ReserveStreamIndex and never by this method,
		// because a caller that only ever queried is owed the news that the number moved
		// and is owed no claim that a ladder is permanently wedged.
		return 0, err
	}
	return persisted, nil
}

func (self *StreamStore) refuseIfClosed() error {
	self.stateMutex.Lock()
	defer self.stateMutex.Unlock()
	if self.closed {
		return fmt.Errorf("%w: the store at %s is closed", ErrStreamStoreState, self.dir)
	}
	return nil
}

// streamRowVerification is the prefix of a row this store has ALREADY checksummed: how many whole
// records from the row's start, and the index the last of them carries.
//
// IT EXISTS BECAUSE THE COST OF NOT HAVING IT IS LINEAR IN MESSAGES EVER SENT. Without it every
// high-water read re-checksums from record 1, so reserving the n-th index of a stream costs n
// SHA-256 blocks and sending m messages costs O(m^2). Measured on this machine with this
// toolchain -- see BenchmarkStreamReserveAtDepth, whose numbers are quoted in the commit rather
// than in a comment that can go stale -- the full rescan is the dominant cost by four digits at a
// hundred thousand records.
//
// WHAT IT COSTS, priced rather than absorbed. A row's already-verified prefix is not re-read, so
// an out-of-band IN-PLACE mutation of a record this store has already checksummed is not seen by
// THIS store until it is reopened. That is not a new residual class: S2-23 already prices
// out-of-band mutation of a row, because no function of the row's length, the record width and
// the checksum verdicts can distinguish an out-of-band truncation from an interrupted append
// either. What the prefix widens it from is "removes whole flushed records" to "mutates the row
// at all while this store holds it open". Two things bound that. The exclusion this store
// acquires at open makes it the only writer for the life of the directory, so the mutation has to
// come from something that bypassed the exclusion. And a REOPENED store re-verifies from record 1
// -- OpenStreamStore's scan seeds this from a full read -- so the miss lasts one store's lifetime
// and not a row's. TestAnOutOfBandMutationOfAVerifiedPrefixIsMissedUntilTheStoreIsReopened is
// that residual, executable, with both halves asserted.
//
// AND ITS THIRD FIELD IS THE ROW CLASS THE OPEN-TIME SCAN CANNOT READ. The scan seeds this map
// from a full read of every row it could classify; a row whose body is a CORRUPT one classifies
// as nothing, so before this field existed such a row was left OUT of the map entirely -- and a
// row that is not in the map is a row the rewind detector above cannot see. It then had the
// weakest state of any row in the directory: vanish it, and persistedHighWater answers contract
// clause 4's error-free zero for a row that durably carried indices, and the very next allocation
// hands out index 1 on a key that has already spent it. The partition the seeding is stated over,
// and the complement this field closes, is written out above persistedHighWater.
type streamRowVerification struct {
	records   int
	highWater uint64

	// unreadable is A ROW THIS STORE HAS READ AND COULD NOT CLASSIFY. It is keyed on that
	// condition and on nothing else -- in particular NOT on when the store found out. The
	// open-time scan sets it from repairRow and persistedHighWater sets it from a live read,
	// because the same bytes in the same directory are the same refusal on either path:
	// nothing in this store ever rewrites a row it refused, so a body it could not classify
	// is a body it will go on being unable to classify.
	//
	// Nothing about how many indices such a row has already spent is derivable from it, so it
	// is entered here rather than omitted -- a row the map has no entry for is a row the
	// rewind detector cannot see -- and persistedHighWater refuses it PRESENT, SHORTER, GONE,
	// or CLASSIFYING AGAIN, for the life of this store. records and highWater are meaningless
	// when this is set and no path reads them.
	unreadable bool
}

// THE PARTITION OpenStreamStore's SEEDING IS STATED OVER, and the complement of the class it can
// read, written down so it is a derivation rather than an impression. Over the entries of the row
// directory:
//
//	(1) not a regular file                       -> NOT SEEDED. rowDirectoryHolds refuses the
//	                                                whole directory with ErrStreamStoreState on
//	                                                every later call, so no key allocates.
//	(2) a name of another key space              -> NOT SEEDED. rowDirectoryHolds refuses the
//	                                                whole directory with ErrStreamKeySpace.
//	(3) a name that is not a row at all          -> NOT SEEDED. rowDirectoryHolds refuses the
//	                                                whole directory with ErrStreamStoreState.
//	(4) this build's row, body classifies        -> SEEDED with the confirmed prefix and its
//	                                                high water. This is the class the scan was
//	                                                written for.
//	(5) this build's row, body does NOT classify -> SEEDED UNREADABLE, since 2026-09-12. This
//	                                                is the complement of (4) inside the rows
//	                                                this build owns, and it is the one class
//	                                                whose refusal is NOT carried by
//	                                                rowDirectoryHolds: the directory is
//	                                                well-formed, so every other key in it goes
//	                                                on allocating, and only this row must stop.
//	                                                THE SCAN IS NOT THE ONLY PLACE THE MARK IS
//	                                                SET: persistedHighWater sets the same mark
//	                                                when a LIVE read meets the same condition,
//	                                                because the mark is keyed on the condition
//	                                                and not on the clock. What the SCAN alone
//	                                                buys is the row whose octets vanish before
//	                                                any call reads them -- a live read cannot
//	                                                mark what it never sees.
//	(6) this build's row, body cannot be READ    -> the store does not open at all; repairRow
//	                                                returns ErrStreamStoreState and
//	                                                OpenStreamStore propagates it.
//
// The member that matters is (5): (1), (2) and (3) are refused directory-wide, (4) is seeded and
// (6) never produces a store. TestTheRowClassesTheOpenTimeSeedingCoversAndItsComplement is that
// partition, executable, with FIVE of the six classes exercised and the complement printed. Class
// (6) is the one it does not run and it says so in its own log line: the store carries no injected
// failure point on the open-time read, and there is no portable way to make a regular file
// unreadable to its owner on the GOOS set this ships to.
//
// persistedHighWater is the row read. It must be called with allocMutex held.
//
// IT ENUMERATES THE ROW DIRECTORY RATHER THAN STATTING ONE PATH, and the cost of that -- one
// directory read per call, linear in ROWS and not in messages -- is priced here rather than
// discovered. Statting one path cannot see a row this build cannot NAME, and a row this build
// cannot name is exactly ledger item 170: a pre-A1 row, indistinguishable from an absent one,
// answered (0, nil), restarting the ladder at index 1 under a class key that has not moved.
func (self *StreamStore) persistedHighWater(rowName string) (uint64, error) {
	present, err := self.rowDirectoryHolds(rowName)
	if err != nil {
		return 0, err
	}
	prior, seen := self.verified[rowName]
	if !present && seen && prior.unreadable {
		// CLASS (5) OF THE PARTITION ABOVE, IN ITS DANGEROUS SHAPE: a row THIS STORE READ AND
		// COULD NOT CLASSIFY -- at open, or later under a live store, which is the same
		// condition and not two -- and whose bytes are now GONE. How many indices it had
		// already spent is not derivable from it -- that is what "did not classify" means --
		// and there is nothing left to re-derive a refusal from, so the entry this store kept
		// for it IS the refusal.
		//
		// Before that entry existed this row took the branch below: not present, nothing
		// remembered, contract clause 4's error-free zero, and the very next allocation
		// handed out index 1 on a key whose row had durably carried indices. That was true of
		// the open-time half until 2026-09-11 and of the LIVE half until 2026-09-12, and the
		// second was reproduced before it was closed: plant a corrupt row under a running
		// store, let one call meet it, remove it, and the next allocation answered 1.
		//
		// IT CARRIES ErrStreamStoreConsumed BESIDE ErrStreamStoreState, which is the store
		// supplying a discriminator the adapter cannot invent. The adapter rules the whole
		// ErrStreamStoreState class TRANSIENT -- a failed flush and a full disk must stay a
		// retry -- and a corrupt row forwarded as transient is an unbounded retry loop
		// paying a durable write per attempt against a row that will never accept one. This
		// is the sub-class where permanence is knowable HERE and nowhere else: a store that
		// could not classify this row's octets cannot classify them later, because nothing in
		// this store ever rewrites a row it refused. See streamStoreSentinelRulings for the part
		// of that class that is still ruled transient and the open question filed on it.
		return 0, fmt.Errorf(
			"%w: row %s is one this store read in %s and could not classify, and its bytes have since been removed; the indices it had already spent are not derivable from it and nothing is left to re-derive them from, so answering contract clause 4's error-free zero would restart the ladder at index 1 under a class key that has not moved, and a second record under a reused stream_index is a reused nonce under a reused record_key (%w)",
			ErrStreamStoreState,
			rowName,
			self.rowDir,
			ErrStreamStoreConsumed,
		)
	}
	if !present {
		if seen {
			// THE ROW VANISHED UNDER THE ONLY WRITER. It is refused here rather than
			// answered as a stream never seen, and the discriminator between those two
			// states is not on the disk: it is the fact that THIS store already read
			// this row, which is what seen records. A row the open-time scan found is
			// seen before any index has been handed out for it.
			//
			// An earlier version answered (0, nil) here and left the refusal to an
			// offset check in writeOneRecord. That refusal did not hold. The write
			// opened the row with os.O_CREATE BEFORE it compared the size, so the
			// refusal itself recreated the row at zero length; the very next call read
			// a present, empty row, re-derived a high water of 0 from it and handed out
			// index 1 on a key that had already spent it. Refusing before anything
			// opens the file is what makes this refusal STICKY: the prefix stays, so
			// every later call for this row meets the same answer.
			return 0, fmt.Errorf(
				"%w: row %s is absent from %s and this store has already confirmed %d record(s) of it carrying a high water of %d; a row that went backwards under the only writer's exclusion is not a stream never seen, and answering contract clause 4's error-free zero for one restarts the ladder at index 1 under a class key that has not moved",
				ErrStreamStoreRewound,
				rowName,
				self.rowDir,
				prior.records,
				prior.highWater,
			)
		}
		// contract clause 4: a stream never seen is 0 with no error, so the first
		// allocation is 1.
		return 0, nil
	}

	path := filepath.Join(self.rowDir, rowName)
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf(
			"%w: row %s is present and could not be opened: %v",
			ErrStreamStoreState,
			rowName,
			err,
		)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("%w: row %s could not be sized: %v", ErrStreamStoreState, rowName, err)
	}

	priorLength := int64(prior.records) * streamRecordWidth
	if info.Size() < priorLength {
		// THE ROW IS SHORTER THAN THE PREFIX THIS STORE CONFIRMED, and it is refused for
		// the same reason the absent row above is. A flushed record is not unwritten by a
		// crash and an interrupted append lengthens a row rather than shortening it, so no
		// writer -- correct or interrupted -- can reach this branch. The row lost confirmed
		// records out of band, under the only writer's exclusion.
		//
		// An earlier version RESET the prefix here and re-derived a high water from
		// whatever was left. Emptying a row IN PLACE then answered 0 with no error and the
		// very next allocation handed out index 1 on a row that had already spent it --
		// unless this same process happened to have returned an index for that row, which
		// a row seeded by the open-time scan never has.
		return 0, fmt.Errorf(
			"%w: row %s is %d octets and this store has confirmed %d octets of it carrying a high water of %d; a flushed record is not unwritten by a crash and an interrupted append lengthens a row rather than shortening it, so a row that lost confirmed records lost them out of band",
			ErrStreamStoreRewound,
			rowName,
			info.Size(),
			priorLength,
			prior.highWater,
		)
	}
	tail := make([]byte, info.Size()-priorLength)
	if 0 < len(tail) {
		if _, err := file.ReadAt(tail, priorLength); err != nil {
			return 0, fmt.Errorf(
				"%w: row %s could not be read from offset %d: %v",
				ErrStreamStoreState,
				rowName,
				priorLength,
				err,
			)
		}
	}
	highWater, verifiedTo, err := classifyStreamRowTail(rowName, prior.records, prior.highWater, tail)
	if err != nil {
		// CLASS (5), AND THE PERMANENCE IS KEYED ON THE CONDITION AND NOT ON THE CLOCK.
		//
		// THIS USED TO BE TWO CLAUSES and the discriminator between them was WHEN this store
		// first met the row: a body that did not classify AT OPEN carried
		// ErrStreamStoreConsumed, and the same bytes in the same directory reached one call
		// later under a live store were forwarded bare -- ErrStreamStoreState, which the
		// adapter rules TRANSIENT, which is the unbounded retry against a row that will never
		// accept a record that the adapter exists to stop. The row was the same row. Only the
		// observer's clock differed, and a clock is not what makes a refusal permanent.
		//
		// WHAT MAKES IT PERMANENT is a property of this store: NOTHING IN IT EVER REWRITES A
		// ROW IT REFUSED. repairRow is the only code that truncates, it runs once per row
		// inside OpenStreamStore, and it leaves a body that failed to classify exactly as it
		// found it; writeOneRecord is the only code that appends, and it is unreachable until
		// this function has ANSWERED. So a body this store has read and could not classify is
		// a body this store will go on being unable to classify, whenever it first found out.
		// The mark is set here for the same reason repairRow sets it at open -- so the
		// refusal survives the bytes being taken away, which is the shape that used to read
		// as a stream never seen -- and it is set HERE as well because the discovery time is
		// not what the mark means.
		//
		// THE REFUSAL ITSELF IS THE ONE THE BYTES PRODUCE, not a second one written here: the
		// three shapes a corrupt body can take share one error value and a refusal that
		// stopped naming its shape could no longer tell them apart --
		// TestARowsThreeCasesAndTheDiscriminatorBetweenThem holds exactly that. What is added
		// is the permanence, and nothing else.
		//
		// WHAT IS GENUINELY OPEN-TIME-ONLY, because "state what, if anything, is" does not
		// answer "nothing": the REPAIR, and the store's refusal to come into existence. A
		// torn tail is truncated once, by repairRow, at open; under a live store the next
		// append overwrites it in place instead. Those two paths differ in what they DO and
		// not in how they classify. And class (6) -- a row whose octets could not be READ at
		// all -- stops OpenStreamStore rather than marking anything, which is a refusal to
		// open and not a permanence claim: an I/O failure is a condition a retry can clear,
		// so on the live path it stays ErrStreamStoreState alone, raised by the os.Open and
		// ReadAt sites above and never by this clause. That is the distinction this clause
		// now turns on. OCTETS OBTAINED AND REFUSED is permanent; OCTETS NOT OBTAINED is
		// transient. Neither is a statement about when.
		self.verified[rowName] = streamRowVerification{unreadable: true}
		return 0, fmt.Errorf(
			"%w; row %s is a row this store has read and could not classify, and nothing in this store ever rewrites a row it refused, so no later call in this store's life can clear it (%w)",
			err,
			rowName,
			ErrStreamStoreConsumed,
		)
	}
	if prior.unreadable {
		// THE ROW CLASSIFIES NOW AND DID NOT WHEN THIS STORE LAST READ IT, so its bytes
		// changed under the only writer's exclusion. What it spent before that change is
		// still not derivable, and re-deriving a high water from whatever replaced it is
		// exactly the move that hands the next allocation a number this stream may already
		// have used.
		//
		// It is the only exit from the unreadable mark that the bytes do not already refuse
		// on their own, so it is the clause that decides whether the mark is STICKY. It
		// returns WITHOUT writing the verified map, and that omission is the stickiness: the
		// row meets this same answer for the life of the store, and no later call can launder
		// it by reading a prefix an earlier call recorded.
		// TestARowThatClassifiesAfterItDidNotIsRefusedRatherThanReSeeded drives it, on both
		// discovery orders -- marked at open, and marked by a live read -- and drives the
		// stickiness and the allocator's seat as well as the reader's.
		return 0, fmt.Errorf(
			"%w: row %s did not classify when this store read it and classifies now, so its bytes changed under the only writer's exclusion; what it had already spent is still not derivable from it and allocating on what replaced it would hand out a number this stream may already have used (%w)",
			ErrStreamStoreState,
			rowName,
			ErrStreamStoreConsumed,
		)
	}
	self.verified[rowName] = streamRowVerification{
		records:   int(verifiedTo / streamRecordWidth),
		highWater: highWater,
	}
	return highWater, nil
}

// rowDirectoryHolds enumerates the row directory and answers whether rowName is in it.
//
// THE REFUSAL IS DELIBERATELY COARSE. One foreign-tagged row refuses every key in the directory,
// not just the key whose identity that row might hold -- because the identity under a foreign
// derivation is opaque to this build, so a directory written by another build tells this build
// nothing about which of its keys that build had already spent. That is the "versioned and
// refused" half of the transition rule item 170 asks for, and it is the half this store takes.
// The other half -- migration, by taking the maximum over the classes of one (group_id,
// sender_handle) -- is vacuous here and saying so is load-bearing: this is the FIRST durable
// reserver in any tree, so a store that has never existed cannot hold a pre-A1 row, and a
// migration written for one is dead code on the day it ships. A version tag also closes the
// hazard's PROPERTY rather than its instance: a future ruling that changes row identity again,
// and a row left by a build nobody has, are refused identically.
func (self *StreamStore) rowDirectoryHolds(rowName string) (bool, error) {
	entries, err := os.ReadDir(self.rowDir)
	if err != nil {
		return false, fmt.Errorf(
			"%w: the row directory %s could not be read: %v",
			ErrStreamStoreState,
			self.rowDir,
			err,
		)
	}
	present := false
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return false, fmt.Errorf(
				"%w: %q in the row directory %s is not a regular file, and that directory holds rows and nothing else",
				ErrStreamStoreState,
				entry.Name(),
				self.rowDir,
			)
		}
		switch self.classifyStreamRowName(entry.Name()) {
		case streamRowOfAnotherKeySpace:
			return false, fmt.Errorf(
				"%w: row %q in %s carries key-space tag %q and this build produces %q; answering this key with a silent zero would restart the stream index ladder at 1 under a class key that has not moved",
				ErrStreamKeySpace,
				entry.Name(),
				self.rowDir,
				entry.Name()[:streamKeySpaceTagLen],
				self.keySpaceTag,
			)
		case streamRowNotARow:
			return false, fmt.Errorf(
				"%w: %q in the row directory %s is not a row under any key-space tag",
				ErrStreamStoreState,
				entry.Name(),
				self.rowDir,
			)
		}
		if entry.Name() == rowName {
			present = true
		}
	}
	return present, nil
}

// writeOneRecord is the whole durable half of an allocation: ONE write of ONE whole record at the
// offset the high-water read verified up to, then ONE forced flush. It must be called with
// allocMutex held, and only after persistedHighWater has answered for this row in this call.
func (self *StreamStore) writeOneRecord(rowName string, index uint64) error {
	path := filepath.Join(self.rowDir, rowName)
	at := int64(self.verified[rowName].records) * streamRecordWidth

	// os.O_CREATE and NOT os.O_APPEND. The create is the one directory-entry mutation this
	// design admits, it happens once per key inside that key's first allocation, and it is
	// S2-16. O_APPEND would put the record wherever the file ends, which is not the same
	// place as where the number it carries was derived from.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf(
			"%w: row %s could not be opened to record index %d: %v",
			ErrStreamStoreState,
			rowName,
			index,
			err,
		)
	}
	defer file.Close()
	// THE ROW'S LENGTH IS ALREADY BOUNDED WHEN THIS RUNS, by the read that produced at.
	// persistedHighWater refuses a row that is absent or shorter than the confirmed prefix,
	// and classifyStreamRowTail admits at most one record's worth of unconfirmed tail past
	// that prefix -- one partial, or one whole record that does not verify. So the row is
	// between at and at+streamRecordWidth octets on every path that reaches here, and writing
	// a whole record at at overwrites either shape entirely: the live writer repairs its own
	// torn tail in the same write that allocates, with no second writer, no read that writes,
	// and a length that is a whole multiple of the record width again.
	//
	// That bound used to be RESTATED here as a refusal, and the restatement was UNDRIVEN.
	// Measured at 9022851, before this change: replacing it with an unconditional append at the
	// rounded-down end of the file left all 33 of that commit's stream tests green and left the
	// unfiltered root pass unchanged against its own baseline. The one state it could actually
	// have refused -- a row that lost records under this store -- is refused at the read
	// instead, where two cases drive it, and refusing there is also what makes the refusal
	// stick, because nothing has opened the row with os.O_CREATE yet.
	record := encodeStreamRecord(rowName, index)
	octets := record[:]
	interrupt := self.appendInterrupt()
	if interrupt == streamAppendTearBeforeFlush {
		octets = octets[:streamRecordWidth/2]
	}
	self.countRowWrite()
	if _, err := file.WriteAt(octets, at); err != nil {
		return fmt.Errorf(
			"%w: row %s could not be written at offset %d: %v",
			ErrStreamStoreState,
			rowName,
			at,
			err,
		)
	}
	if interrupt == streamAppendTearBeforeFlush {
		return fmt.Errorf(
			"%w: the append to row %s stopped after %d of %d octets and before the flush",
			errStreamAppendInterrupted,
			rowName,
			len(octets),
			streamRecordWidth,
		)
	}

	syncErr := self.forceFlush(file)
	if interrupt == streamAppendFailTheFlush && syncErr == nil {
		syncErr = errStreamInjectedFlushFailure
	}
	if syncErr != nil {
		// never swallowed. A Reserve that returned after a failed flush has handed out an
		// index it cannot prove it recorded.
		return fmt.Errorf(
			"%w: row %s could not be flushed after recording index %d, so that index is not durable and must not be handed out: %v",
			ErrStreamStoreState,
			rowName,
			index,
			syncErr,
		)
	}

	// The flush returned, so the record is on stable storage and the verified prefix grows by
	// exactly the record that was just written.
	//
	// THE ORDERING IS THE PROPERTY HERE, NOT THE ASSIGNMENT. The prefix is what the rewind
	// detector stands on, so recording it before the write records a record the write may
	// never make: an interrupted append then leaves the row SHORTER than the prefix this store
	// claims to have confirmed, the next read refuses it as a rewind, and the row is wedged
	// permanently by a failure a retry would have cleared.
	// TestAnInterruptedAppendLeavesTheRowAllocatableByTheNextCall drives exactly that -- move
	// this assignment above the write and it goes red.
	//
	// AND THE ASSIGNMENT ITSELF IS THE PROPERTY, NOT ONLY ITS POSITION. Every mutation this
	// clause had ever been put through MOVED it; none DELETED it, and deleting it left the
	// whole unfiltered root suite green at a1d55b1. What it removes is this: the prefix is the
	// only record that an index was handed out for a row this call CREATED. On every later
	// allocation persistedHighWater re-reads the row and re-seeds the prefix, so the deletion
	// is invisible there -- but on the FIRST allocation of a row, persistedHighWater took the
	// absent-row exit at (0, nil) and recorded nothing, so with this line gone the store
	// returns index 1 having confirmed no part of the row. Remove that row, or empty it in
	// place, and the next call reads a stream never seen and hands out index 1 a SECOND time,
	// which section 5.6 calls a total break of both AEADs for that record.
	// TestTheIndexTheStoreJustReturnedIsInsideItsConfirmedPrefix drives the deletion, on both
	// shapes and on the prefix itself.
	self.verified[rowName] = streamRowVerification{
		records:   int(at/streamRecordWidth) + 1,
		highWater: index,
	}
	if interrupt == streamAppendDieAfterFlush {
		return fmt.Errorf(
			"%w: row %s recorded index %d durably and the process died before it was returned; the index is burned",
			errStreamAppendInterrupted,
			rowName,
			index,
		)
	}
	return nil
}

// Close releases the store, and with it the single-writer exclusion, which is the only thing that
// releases it other than the death of this process.
//
// A closed store stops answering, because a closed store that answered (0, nil) would be exactly
// the silent zero this file exists to make unreachable. It is idempotent: a second Close releases
// nothing a second time.
func (self *StreamStore) Close() error {
	self.stateMutex.Lock()
	alreadyClosed := self.closed
	self.closed = true
	exclusion := self.exclusion
	self.exclusion = nil
	self.stateMutex.Unlock()
	if alreadyClosed || exclusion == nil {
		return nil
	}
	if err := exclusion.Close(); err != nil {
		return fmt.Errorf(
			"%w: the single-writer exclusion on %s could not be released: %v",
			ErrStreamStoreState,
			self.dir,
			err,
		)
	}
	return nil
}
