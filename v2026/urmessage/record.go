package urmessage

import (
	"encoding/binary"
	"fmt"

	"github.com/urnetwork/connect/v2026/message"
	"github.com/urnetwork/connect/v2026/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ── §4.3.3's projection, as a client populates it ────────────────────────────────────────────

// projectionOf is the §4.3.3 projection of one sealed record: the header fields the server indexes
// on, beside the `record_bytes` they are a projection OF.
//
// It is built from the PARSE and never from the values this package passed to the sealer, because
// §5.1 check 3 compares the two and a check whose two sides come from one expression is a check
// that cannot fail. The join of the retention class and the eph bucket is
// [message.RetentionClassWire] and is not restated here: §12.1 A-1 is written against a second
// copy of that table.
func projectionOf(record *message.Record) (*protocol.Record, error) {
	header := &record.Header
	attachment, err := message.ParseServerAttachment(header.ServerAttachment)
	if err != nil {
		return nil, err
	}
	retentionWire, err := message.RetentionClassWire(header.RetentionClass, header.EphBucket)
	if err != nil {
		return nil, err
	}
	recordBytes, err := message.EncodeRecord(record)
	if err != nil {
		return nil, err
	}
	projection := &protocol.Record{
		SenderHandle:   append([]byte{}, header.SenderHandle[:]...),
		Epoch:          header.Epoch,
		StreamIndex:    header.StreamIndex,
		IsCommit:       header.IsCommit,
		RetentionClass: uint32(retentionWire),
		SizeBucket:     uint32(header.SizeBucket),
		ExpireAtMs:     header.ExpireAt,
		BodyHash:       append([]byte{}, header.BodyHash[:]...),
		BlobId:         append([]byte{}, header.BlobId...),
		RecordBytes:    recordBytes,
	}
	if attachment != nil && attachment.Wrap != nil {
		projection.WrapTargetHandle = append([]byte{}, attachment.Wrap.WrapTargetHandle...)
	}
	if attachment != nil && attachment.Recovery != nil {
		projection.RecoveryHandle = append([]byte{}, attachment.Recovery.RecoveryHandle...)
	}
	return projection, nil
}

// ── ruling 33's epoch key delivery, as a client populates it ─────────────────────────────────

// epochKeyBytes is the width §5.4 gives write_key and read_key, and the width
// `EpochKeyDelivery` declares for both of its fields: EXACTLY 32 octets, not at least.
//
// It is checked on this side as well as on the server's because the one caller that could get
// here with a short key is a caller that looked one up and got nothing back, and a zero-length
// key would ride onto the wire as a present-but-empty delivery -- which `message.proto` records as
// the REASON_OK = 0 hazard: an empty entry decodes as two EMPTY KEYS and never as an absence.
const epochKeyBytes = 32

// epochKeyDelivery is the pair a commit's own epoch opens with, copied onto the request that
// carries the commit.
//
// THE KEYS TRAVEL BESIDE THE RECORD AND NEVER INSIDE `protocol.Record`. That is ruling 33 and the
// reason is item 244: `Record` is the server→client type in six places, so a key field on it would
// be six serve paths that each have to remember to clear it -- and `Record`'s projection contract
// makes it impossible anyway, because submit checks proto.Equal(projectionOf(ParseRecord(bytes)),
// sent) and a key is by construction not implied by a projection of `record_bytes`.
//
// IT COPIES. `messagegroup.EpochKeys.WriteKey` hands back the session's own backing array rather
// than a copy, and `EpochKeys.Destroy` zeroizes it -- so a delivery that aliased it would be two
// empty keys the moment the deferred Destroy in the caller ran. The copy is the same move
// [Group.Open] already makes for `bootstrap_write_key`.
func epochKeyDelivery(writeKey []byte, readKey []byte) (*protocol.EpochKeyDelivery, error) {
	if len(writeKey) != epochKeyBytes {
		return nil, fmt.Errorf("%w: write_key is %d octets and the delivery carries exactly %d",
			ErrEpochKeyDelivery, len(writeKey), epochKeyBytes)
	}
	if len(readKey) != epochKeyBytes {
		return nil, fmt.Errorf("%w: read_key is %d octets and the delivery carries exactly %d",
			ErrEpochKeyDelivery, len(readKey), epochKeyBytes)
	}
	return &protocol.EpochKeyDelivery{
		WriteKey: append([]byte(nil), writeKey...),
		ReadKey:  append([]byte(nil), readKey...),
	}, nil
}

// epochDigestGroupId is the group ruling 34 frames into H(epoch_keys), at the fixed width
// [message.EpochKeysDigest] frames it in.
//
// IT IS CHECKED RATHER THAN CONVERTED BLIND, and the check is the same one the server makes for
// the same reason. `api/epochkeys.go`'s checkEpochKeysDigest refuses a group id of another width
// before its own conversion, calls that arm unreachable from Submit and CreateGroup, and keeps it
// because "a short group id would be silently zero-padded into a different group's preimage". Go's
// slice-to-array conversion panics rather than padding, so what is at stake on this side is a
// panic in [Group.Open] rather than a silent cross-group digest -- better, and still not what a
// library should hand its caller. Both constructors of a [Group] already refuse another width
// ([Device.CreateGroup] by hand, [Device.Join] through Invite.check), so this is a third copy of a
// check that has never failed; it is here because the two sides of this digest are computed in two
// different repositories and this is the only one of them that can see a [Group].
func epochDigestGroupId(groupId []byte) ([GroupIdBytes]byte, error) {
	if len(groupId) != GroupIdBytes {
		return [GroupIdBytes]byte{}, fmt.Errorf(
			"%w: a group id is %d octets and this one is %d, and ruling 34 frames it into H(epoch_keys)",
			ErrEpochKeyDelivery, GroupIdBytes, len(groupId))
	}
	return [GroupIdBytes]byte(groupId), nil
}

// commitCarriesItsKeys says whether a sealed record is a commit whose epoch keys travel BESIDE it
// rather than inside it, which is the one question both epoch key sites turn on.
//
// THE ANSWER IS THE ATTACHMENT KIND AND IT IS READ OFF THE SEALED OCTETS. Under kind 0x0001 the
// pair is IN the attachment, so a delivery beside it would be a second copy of two keys the
// write_auth mac already covers, able to disagree with the one it covers. Under kind 0x0005 the
// attachment carries LP(H(epoch_keys)) and no keys at all, so the delivery is the ONLY road they
// have. Spec B §5.4 calls those two sentences its acceptance window, and the server answers this
// question exactly this way -- `record.attachment.Kind == message.AttachmentEpochDigest` -- so a
// client that answered it any other way would be guessing at a rule that is written down.
//
// IT IS PARSED AND NOT REMEMBERED. The kind comes from Header.ServerAttachment, the octets the
// sealer produced and the write_auth mac covers, and never from the ServerAttachment value this
// package handed the sealer: a decision whose two sides come from one expression cannot catch a
// sealer that encoded something else. ParseEpochDigestAttachment is the sixth kind's only door and
// it refuses every other kind by name, so its yes IS the discriminator and no second test is owed.
func commitCarriesItsKeys(record *message.Record) bool {
	if record == nil || !record.Header.IsCommit {
		return false
	}
	_, err := message.ParseEpochDigestAttachment(record.Header.ServerAttachment)
	return err == nil
}

// epochKeysFor is the delivery a sealed record owes, or nil for a record that owes none.
//
// IT ANSWERS A DELIVERY FOR EVERY COMMIT THIS PACKAGE SEALS, which is item 244 closed rather than
// a feature switched on. Both commit sites -- [Group.Open]'s founding commit and
// [Group.publishCommitLocked]'s epoch commit -- build their attachment through
// message.NewEpochDigestAttachment, so the kind in the sealed octets is 0x0005, the served bytes
// carry LP(H(epoch_keys)) and no key, and this carrier is the ONLY road the pair has.
//
// NOTHING HERE CHANGED WHEN connect's DOOR OPENED, and that was the design. The decision is keyed
// on the sealed record rather than on a flag, so the edit that closed item 244 was two attachment
// literals in group.go and not one line in this file: a flag would have been a second place to
// remember. The kind 0x0001 arm below is still live and is not dead code -- §5.4's acceptance
// window is dated and admits both kinds until it closes.
//
// AND EMITTING ONE UNCONDITIONALLY IS NOT THE SAFE SHAPE, measured rather than reasoned: a delivery
// beside a kind 0x0001 commit is REFUSED -- "a kind 0x0001 commit arrived with an epoch key
// delivery beside it" -- because under 0x0001 the server reads the keys out of the attachment and a
// delivery is a field it would not read. Driven end to end through the real server, the
// unconditional shape answered REASON_REJECTED to every epoch commit, with the unmodified client
// as the control answering ok.
func epochKeysFor(record *message.Record, writeKey []byte, readKey []byte) (
	*protocol.EpochKeyDelivery, error) {

	if !commitCarriesItsKeys(record) {
		return nil, nil
	}
	return epochKeyDelivery(writeKey, readKey)
}

// alignedEpochKeys is `SubmitRequest.epoch_keys` for a submission of ONE record: the positional
// alignment Spec B §4.3.3 states, enforced on this side.
//
// WHAT IT ADMITS, ENUMERATED, because it admits exactly two lengths and not a general batch.
// §4.3.3 makes a batch containing a commit exactly one record, so `epoch_keys` is EMPTY when the
// submission carries no commit OR carries one kind 0x0001 commit, and holds EXACTLY ONE ENTRY when
// it carries one kind 0x0005 commit. There is no third length. For a mixed batch the field is not
// under-specified, it is UNSATISFIABLE -- which is why this package submits one record at a time.
//
// THE RULE IS KEYED ON THE ATTACHMENT KIND AND NOT ON is_commit ALONE, and the distinction is the
// whole of §5.4's acceptance window. `is_commit` alone gives two wrong answers: it would put a
// delivery beside a 0x0001 commit, which the server refuses as a key it would not read, and it
// would leave a 0x0005 commit with none, which the server refuses as an epoch it was never handed
// what opens. Both are refusals the client can see coming, and seeing one here costs a sentence
// where seeing it on the wire costs a spent stream index and a spent MLS generation.
//
// IT REFUSES IN BOTH DIRECTIONS AND NEITHER IS THE OTHER'S MIRROR. A 0x0005 commit with no
// delivery is a device that derived an epoch and did not hand over what opens it. A record WITH an
// unwanted delivery is the worse of the two: the entry is aimed POSITIONALLY, so a live epoch key
// is pointed at a record that either opens nothing or already carries its own.
//
// IT IS NOT AN EMPTY ENTRY IN EITHER DIRECTION. Every field of `EpochKeyDelivery` has implicit
// presence, so a zero entry is two EMPTY KEYS on the wire and not an absence -- the REASON_OK = 0
// hazard `message.proto` records -- which is why the no-delivery case answers a nil slice and never
// a slice of one zero value.
//
// BOTH SIDES ARE READ OFF THE SEALED RECORD, which is what the server checks against
// ParseRecord(record_bytes) before it weighs this field at all. Reading either off the caller's
// intention would be a check whose two sides came from one expression.
func alignedEpochKeys(record *message.Record, delivery *protocol.EpochKeyDelivery) (
	[]*protocol.EpochKeyDelivery, error) {

	if record == nil {
		return nil, fmt.Errorf("%w: there is no record to align against", ErrEpochKeyDelivery)
	}
	if commitCarriesItsKeys(record) {
		if delivery == nil {
			return nil, fmt.Errorf("%w: this is a kind 0x0005 commit and no epoch keys were handed over, so the epoch it opens would be installed by a server that was never given what opens it",
				ErrEpochKeyDelivery)
		}
		return []*protocol.EpochKeyDelivery{delivery}, nil
	}
	if delivery != nil {
		if record.Header.IsCommit {
			return nil, fmt.Errorf("%w: this commit carries its keys inside a kind 0x0001 attachment and an epoch key delivery was handed over beside it, which is a second copy of two keys the write_auth mac already covers",
				ErrEpochKeyDelivery)
		}
		return nil, fmt.Errorf("%w: this record is not a commit and an epoch key delivery was handed over for it, and an entry is aimed positionally at the record beside it",
			ErrEpochKeyDelivery)
	}
	return nil, nil
}

// ── §4.3.8's req_auth, which the read path is authorized by ──────────────────────────────────

// authorizeFetch computes §4.3.8's `req_auth` over the request's own canonical bytes, under the
// read key of the epoch the request names, over this connection's nonce.
//
// The request is mutated in place and authorized LAST, which is the ordering the authenticator
// forces: it covers the deterministic marshal of the body with `req_auth` itself cleared, so a
// caller that filled the field and then changed another one would be sending a MAC over a request
// it did not send.
func authorizeFetch(request *protocol.FetchRequest, readKey []byte, serverNonce []byte) error {
	if len(readKey) == 0 {
		return fmt.Errorf("%w: a fetch is macced under the epoch's read key", ErrFetchRefused)
	}
	if len(serverNonce) == 0 {
		return ErrNotConnected
	}
	op, err := opOf(request)
	if err != nil {
		return err
	}
	request.ReqAuth = nil
	canonical, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return err
	}
	auth := message.ComputeRequestAuth(readKey, serverNonce, op, canonical)
	request.ReqAuth = auth[:]
	return nil
}

// authorizeSubscribe is [authorizeFetch] for §4.3.5's SubscribeRequest, op 14: the same MAC under
// the read key of the epoch the request names, over this connection's nonce, authorized LAST.
func authorizeSubscribe(request *protocol.SubscribeRequest, readKey []byte, serverNonce []byte) error {
	if len(readKey) == 0 {
		return fmt.Errorf("%w: a subscription is macced under the epoch's read key", ErrSubscribeRefused)
	}
	if len(serverNonce) == 0 {
		return ErrNotConnected
	}
	op, err := opOf(request)
	if err != nil {
		return err
	}
	request.ReqAuth = nil
	canonical, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return err
	}
	auth := message.ComputeRequestAuth(readKey, serverNonce, op, canonical)
	request.ReqAuth = auth[:]
	return nil
}

// opOf is §4.3.8's `op`: the field number of the arm of `MessageServerRequest.body` that carries
// this type, read out of the compiled descriptor rather than written down.
//
// A switch over the typed wrappers is where a copy-paste puts a fetch under the submit op, and the
// MAC then covers an operation nobody asked for.
func opOf(body proto.Message) (uint8, error) {
	field, err := armOf(body)
	if err != nil {
		return 0, err
	}
	if number := field.Number(); number < 0 || 255 < number {
		return 0, fmt.Errorf("urmessage: %s is arm %d, which is not a u8",
			body.ProtoReflect().Descriptor().FullName(), field.Number())
	}
	return uint8(field.Number()), nil
}

func armOf(body proto.Message) (protoreflect.FieldDescriptor, error) {
	oneof := (&protocol.MessageServerRequest{}).ProtoReflect().Descriptor().Oneofs().ByName("body")
	if oneof == nil {
		return nil, fmt.Errorf("urmessage: MessageServerRequest declares no body oneof")
	}
	want := body.ProtoReflect().Descriptor().FullName()
	for index := 0; index < oneof.Fields().Len(); index += 1 {
		field := oneof.Fields().Get(index)
		if field.Kind() != protoreflect.MessageKind || field.Message().FullName() != want {
			continue
		}
		return field, nil
	}
	return nil, fmt.Errorf("urmessage: no arm of MessageServerRequest.body carries %s", want)
}

// ── the head this build writes ───────────────────────────────────────────────────────────────

// The version byte every head this package seals starts with. It is INSIDE `ct_head`, so the
// server never sees it and it costs nothing on the wire that is readable by anyone but a member.
//
// The version byte is the RECORD's format epoch, not the head's own layout version. It is bumped
// whenever a build's reading of a record changes in a way an older build would get wrong --
// including a change to the APPLICATION PLAINTEXT's grammar, which is what 0x02 announces. The head
// layout itself is frozen at version ‖ sent_at, 9 octets, and invariant H1 gates that.
//
// CONCRETELY, 0x02 ANNOUNCES "the application plaintext is kind ‖ body(kind)" -- see kind.go. It
// does not announce a head change, because there is none. What it buys is exactly one population:
// records sealed by a post-MASTER-§8.4, pre-kinds build, which is what the live table holds today.
// An sdk at eebd50c does `Text: string(bodyPlain)` with no branch, so handed a kinded record it
// would render `0x05 ‖ <32 octets> ‖ 👍` as a text line attributed to a real sender; decodeHead's
// strict equality below is what turns that into [ErrHeadFormat] instead. A kinded build keeps NO
// raw-text parser for head 0x01, so no body is ever interpretable under two grammars.
//
// RECORDS SEALED BEFORE MASTER §8.4 ARE NOT WHAT THIS PROTECTS AGAINST, stated so nobody claims it:
// those carry no inner frame and OpenRecord already refuses them at the peek
// (connect/messagegroup/mlsframe.go:383).
const headVersion byte = 0x02

// headBytes is the octets a head of this version is: the version, then `sent_at` as unix
// milliseconds, big endian.
//
// IT IS FROZEN AND THE FREEZE IS A CASE, not this sentence: 9 octets here plus
// chacha20poly1305.Overhead (16, connect/messagegroup/recordaead.go:67) is 25 octets of `ct_head`
// on every record of every class, which is invariant H1 and is what
// TestEveryRecordThisBuildSealsCarriesA25OctetCtHead asserts on the SEAL side across all four
// record kinds. Widening this is not an edit: `ct_head` travels as a bare WriteOpaqueLP
// (connect/message/codec.go:155) bounded by a cap rather than padded into a rung, so a head whose
// width depends on what a record SAYS leaks that to the server, permanently, for every PERMANENT
// and DURABLE row already stored.
const headBytes = 1 + 8

// encodeHead builds one record's head from the clock reading its sender took.
func encodeHead(sentAtMs int64) []byte {
	head := make([]byte, headBytes)
	head[0] = headVersion
	binary.BigEndian.PutUint64(head[1:], uint64(sentAtMs))
	return head
}

// decodeHead reads back what encodeHead wrote, and refuses anything else.
//
// Every octet it reads was authenticated by the head AEAD before this function sees it, so a
// refusal here means the two builds disagree rather than that somebody tampered.
func decodeHead(head []byte) (int64, error) {
	if len(head) != headBytes {
		return 0, fmt.Errorf("%w: %d octets, want %d", ErrHeadFormat, len(head), headBytes)
	}
	if head[0] != headVersion {
		return 0, fmt.Errorf("%w: version %#02x, want %#02x", ErrHeadFormat, head[0], headVersion)
	}
	return int64(binary.BigEndian.Uint64(head[1:])), nil
}
