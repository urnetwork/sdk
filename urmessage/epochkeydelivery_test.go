package urmessage

import (
	"bytes"
	"errors"
	"testing"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// ── ruling 33: where an epoch key may travel ─────────────────────────────────────────────────

func testEpochKeyPair() ([]byte, []byte) {
	writeKey := make([]byte, epochKeyBytes)
	readKey := make([]byte, epochKeyBytes)
	for at := range writeKey {
		writeKey[at] = byte(0x40 + at)
		readKey[at] = byte(0x90 + at)
	}
	return writeKey, readKey
}

// The three attachment shapes the alignment rule tells apart, built as OCTETS through
// connect/message's own two doors and hung on a record header the way a sealer would.
//
// THE KIND 0x0005 RECORD IS THE SHAPE THIS CLIENT NOW SEALS. Both doors encode the sixth kind --
// message.EncodeEpochDigestAttachment, the typed one, and message.EncodeServerAttachment, the one
// messagegroup.GroupSession.SealRecord runs -- and connect holds them byte-identical. So every
// clause below that names 0x0005 is a clause about a shape [Group.Open] and
// [Group.publishCommitLocked] really emit, and cp3b's TestItem244 drives that same shape through a
// real server rather than inferring it from these octets.
func testEpochDigestAttachmentBytes(t *testing.T, groupId [32]byte, opensEpoch uint64,
	writeKey []byte, readKey []byte) ([]byte, *message.EpochDigestAttachment) {

	t.Helper()
	body, err := message.NewEpochDigestAttachment(groupId, message.EpochDigestAttachment{
		Epoch:             opensEpoch,
		AlgId:             epochAttachmentAlgId,
		GroupContextHash:  bytes.Repeat([]byte{0x33}, 32),
		ExpectedWrapCount: 2,
	}, writeKey, readKey)
	if err != nil {
		t.Fatalf("NewEpochDigestAttachment: %v", err)
	}
	encoded, err := message.EncodeEpochDigestAttachment(body)
	if err != nil {
		t.Fatalf("EncodeEpochDigestAttachment: %v", err)
	}
	return encoded, body
}

func testEpochAttachmentBytes(t *testing.T, opensEpoch uint64, writeKey []byte, readKey []byte) []byte {
	t.Helper()
	encoded, err := message.EncodeServerAttachment(&message.ServerAttachment{
		Kind: message.AttachmentEpoch,
		Epoch: &message.EpochAttachment{
			Epoch:             opensEpoch,
			AlgId:             epochAttachmentAlgId,
			WriteKey:          writeKey,
			ReadKey:           readKey,
			GroupContextHash:  bytes.Repeat([]byte{0x33}, 32),
			ExpectedWrapCount: 2,
		},
	})
	if err != nil {
		t.Fatalf("EncodeServerAttachment(kind 0x0001): %v", err)
	}
	return encoded
}

func testRecord(isCommit bool, attachment []byte) *message.Record {
	return &message.Record{Header: message.RecordHeader{
		IsCommit:         isCommit,
		Epoch:            3,
		StreamIndex:      1,
		ServerAttachment: attachment,
	}}
}

// ITEM 244, AT THE OCTETS: THE KEYS ARE IN THE REQUEST AND THEY ARE NOT IN THE ATTACHMENT.
//
// THIS REPLACES TestTheSealDoorThisPackageUsesWillNotEncodeTheSixthKind, WHICH ASSERTED THE
// BLOCKER RATHER THAN THE PROPERTY. That case held one clause -- `message.EncodeServerAttachment`
// refuses kind 0x0005 -- and it existed to fire the day connect's door opened, which it did, by
// name, carrying the edit to make in its own failure message. What replaces it has to be STRONGER
// and not merely different, so it holds four clauses where that one held one:
//
//   - the door the SEALER runs serves kind 0x0005 (the old clause, inverted: this is still the
//     tripwire, and it fires by name if connect ever narrows that map again);
//   - the octets it produces contain NEITHER key;
//   - the same search over the kind 0x0001 encoding of the same six facts FINDS BOTH, which is the
//     inline positive control -- without it, "not found" is a search that read nothing;
//   - and the request carrier this package builds DOES contain both, which is the other half of
//     the sentence: the keys did not vanish, they moved.
//
// WHAT IT MEASURES IS THE ATTACHMENT AND NOT THE WHOLE RECORD, and the distinction is stated
// rather than blurred. `EncodeRecord` writes `WriteOpaqueLP(header.ServerAttachment)` -- the
// attachment's octets go into the record VERBATIM -- and the rest of a record is `ct_head`,
// `ct_body` and a MAC. So the attachment is the only part of a record that has ever held a key in
// the clear, and this case holds it at the narrowest place the claim is true of. THE WHOLE-RECORD
// CLAIM IS HELD IN cp3b's TestItem244, over bytes a real server stored and served back, because a
// record valid enough to encode is a record a sealer built and this package cannot build one
// without a session.
func TestItem244sKeysAreInTheRequestAndNotInTheRecord(t *testing.T) {
	var groupId [32]byte
	for at := range groupId {
		groupId[at] = byte(0x11 + at)
	}
	writeKey, readKey := testEpochKeyPair()
	const opensEpoch = 4

	digestBody, err := message.NewEpochDigestAttachment(groupId, message.EpochDigestAttachment{
		Epoch:             opensEpoch,
		AlgId:             epochAttachmentAlgId,
		GroupContextHash:  bytes.Repeat([]byte{0x33}, 32),
		ExpectedWrapCount: 2,
	}, writeKey, readKey)
	if err != nil {
		t.Fatalf("NewEpochDigestAttachment: %v", err)
	}

	// (1) THE TRIPWIRE, INVERTED. This is the door `messagegroup.GroupSession.SealRecord` runs --
	// not the sixth kind's own typed door -- and it is the one that refused this kind until ruling
	// 33 moved `serverAttachmentKindServed`. Both commit sites in group.go reach exactly here.
	served, err := message.EncodeServerAttachment(&message.ServerAttachment{
		Kind:        message.AttachmentEpochDigest,
		EpochDigest: digestBody,
	})
	if err != nil {
		t.Fatalf("message.EncodeServerAttachment refuses kind 0x0005: %v.\n"+
			"connect's seal door has CLOSED again. [Group.Open] and [Group.publishCommitLocked] "+
			"build their attachment through message.NewEpochDigestAttachment and seal it through "+
			"this encoder, so every commit this package makes is refused until that map serves "+
			"AttachmentEpochDigest again -- and item 244 cannot be re-closed by this repository alone.",
			err)
	}

	pair := []struct {
		name string
		key  []byte
	}{{"write_key", writeKey}, {"read_key", readKey}}

	// (2) AND THE OCTETS CARRY NEITHER KEY.
	for _, one := range pair {
		if bytes.Contains(served, one.key) {
			t.Errorf("the kind 0x0005 attachment's %d octets contain %s. Item 244 is that a "+
				"removed member is served the next epoch's keys forever, and the whole of ruling "+
				"27 is that what the server serves back is LP(H(epoch_keys)) and not the pair.",
				len(served), one.name)
		}
	}

	// (3) THE INLINE POSITIVE CONTROL, in the same run and through the same encoder: the same six
	// facts under kind 0x0001, which is what this package sealed until this commit. Both keys MUST
	// be found. A search that cannot find a key that IS there says nothing about a key that is
	// not, so this is a t.Fatal and not a t.Error: every clause above is vacuous without it.
	control := testEpochAttachmentBytes(t, opensEpoch, writeKey, readKey)
	for _, one := range pair {
		if !bytes.Contains(control, one.key) {
			t.Fatalf("the kind 0x0001 control's %d octets do NOT contain %s, so this case's search "+
				"cannot find a key in an attachment and its refusals above are vacuous",
				len(control), one.name)
		}
	}

	// (4) AND THE KEYS DID NOT VANISH, THEY MOVED. The request carrier this package builds holds
	// both -- marshalled and read back as octets, because a field set on a message and then
	// discarded looks identical in a struct assertion.
	delivery, err := epochKeyDelivery(writeKey, readKey)
	if err != nil {
		t.Fatalf("epochKeyDelivery: %v", err)
	}
	onTheRequest, err := proto.Marshal(&protocol.SubmitRequest{
		EpochKeys: []*protocol.EpochKeyDelivery{delivery},
	})
	if err != nil {
		t.Fatalf("marshalling the request carrier: %v", err)
	}
	for _, one := range pair {
		if !bytes.Contains(onTheRequest, one.key) {
			t.Errorf("SubmitRequest.epoch_keys does not carry %s. Ruling 33 makes the request the "+
				"ONLY road these two keys have, so a commit whose attachment is a digest and whose "+
				"request is empty is an epoch the server can never open.", one.name)
		}
	}

	t.Logf("kind 0x0005: %d octets, neither key present. kind 0x0001 control: %d octets, both "+
		"present. SubmitRequest.epoch_keys: %d octets, both present.",
		len(served), len(control), len(onTheRequest))
}

// §4.3.3's ALIGNMENT, HELD OVER ITS WHOLE TRUTH TABLE.
//
// THE RULE IS KEYED ON THE ATTACHMENT KIND AND NOT ON is_commit, which is §5.4's acceptance window
// and is exactly how the server decides it: `epoch_keys` is EMPTY when the submission carries no
// commit OR carries one kind 0x0001 commit, and holds EXACTLY ONE ENTRY when it carries one kind
// 0x0005 commit. Six rows, because each of the three record shapes is driven with a delivery and
// without one, and a table that drove only the admissible three would be a table that cannot see a
// rule that admits everything.
func TestTheEpochKeysAreAlignedWithTheRecordTheyOpen(t *testing.T) {
	var groupId [32]byte
	groupId[0] = 0x71
	writeKey, readKey := testEpochKeyPair()
	delivery, err := epochKeyDelivery(writeKey, readKey)
	if err != nil {
		t.Fatalf("epochKeyDelivery: %v", err)
	}
	digestBytes, _ := testEpochDigestAttachmentBytes(t, groupId, 4, writeKey, readKey)
	plainBytes := testEpochAttachmentBytes(t, 4, writeKey, readKey)

	for _, one := range []struct {
		name     string
		record   *message.Record
		delivery *protocol.EpochKeyDelivery
		entries  int
		refused  bool
	}{
		{"a kind 0x0005 commit with its pair", testRecord(true, digestBytes), delivery, 1, false},
		{"a kind 0x0005 commit with no pair", testRecord(true, digestBytes), nil, 0, true},
		{"a kind 0x0001 commit with no pair", testRecord(true, plainBytes), nil, 0, false},
		{"a kind 0x0001 commit with a pair beside it", testRecord(true, plainBytes), delivery, 0, true},
		{"an ordinary record with no pair", testRecord(false, nil), nil, 0, false},
		{"an ordinary record with a pair beside it", testRecord(false, nil), delivery, 0, true},
	} {
		t.Run(one.name, func(t *testing.T) {
			aligned, err := alignedEpochKeys(one.record, one.delivery)
			if one.refused {
				if !errors.Is(err, ErrEpochKeyDelivery) {
					t.Fatalf("%s answered %v, want ErrEpochKeyDelivery", one.name, err)
				}
				if aligned != nil {
					t.Errorf("%s was refused and still answered %d entries", one.name, len(aligned))
				}
				return
			}
			if err != nil {
				t.Fatalf("%s was refused: %v", one.name, err)
			}
			if len(aligned) != one.entries {
				t.Fatalf("%s carries %d epoch key entries, want %d", one.name, len(aligned), one.entries)
			}
			// and EMPTY is a nil slice and never a slice holding one zero entry: every field of
			// EpochKeyDelivery has implicit presence, so a zero entry decodes as two EMPTY KEYS
			// rather than as an absence.
			for at, entry := range aligned {
				if entry == nil {
					t.Errorf("%s put a nil entry at index %d", one.name, at)
				}
			}
		})
	}

	// and the widths, refused HERE so a key that was looked up and came back short never becomes a
	// present-but-empty delivery on the wire.
	short := make([]byte, epochKeyBytes-1)
	if _, err := epochKeyDelivery(short, readKey); !errors.Is(err, ErrEpochKeyDelivery) {
		t.Errorf("a %d octet write_key answered %v, want ErrEpochKeyDelivery", len(short), err)
	}
	if _, err := epochKeyDelivery(writeKey, short); !errors.Is(err, ErrEpochKeyDelivery) {
		t.Errorf("a %d octet read_key answered %v, want ErrEpochKeyDelivery", len(short), err)
	}
	if _, err := epochKeyDelivery(nil, nil); !errors.Is(err, ErrEpochKeyDelivery) {
		t.Errorf("a delivery of two absent keys answered %v, want ErrEpochKeyDelivery", err)
	}
}

// WHAT EACH COMMIT PATH ACTUALLY PUTS ON ITS REQUEST, DECIDED OFF THE SEALED RECORD.
//
// [epochKeysFor] is the one decision both commit sites make, and it reads the attachment kind out
// of the octets the sealer produced -- never out of the ServerAttachment value this package handed
// the sealer, because a decision whose two sides come from one expression cannot catch a sealer
// that encoded something else.
//
// THE THIRD ROW IS THE ONE THAT MATTERS TODAY AND THE FIRST IS THE ONE THAT MATTERS NEXT. A kind
// 0x0001 commit -- every commit this package can seal -- answers nil, so both requests go out with
// no `epoch_keys` at all and are accepted by today's deployed server and by the amended one alike.
// A kind 0x0005 commit answers the pair, with no other edit anywhere.
//
// A MUTANT THAT SURVIVED THIS CASE, RECORDED BECAUSE IT IS NOT A HOLE. "Build the digest over a
// different epoch" was tried as `testEpochDigestAttachmentBytes(..., 5, ...)` and the case stayed
// GREEN -- which is not a gap in the case, it is message.NewEpochDigestAttachment doing its job:
// the constructor reads the epoch ONCE, out of the body it is building, so moving the epoch moves
// the body's field AND the preimage together and the two can never disagree. The mutant that does
// bite has to go AROUND the constructor -- `digestBody.Epoch += 1` after it returns -- and that one
// turns the CheckEpochKeysDigest clause below red by name: "the two keys handed beside this record
// are not the ones H(epoch_keys) at opens_epoch 5 is over". So the parameter is not the mechanism
// here, and a mutation table that only moved the parameter would have reported a property this case
// does not hold.
func TestEachCommitPathCarriesTheKeysItsOwnAttachmentKindAsksFor(t *testing.T) {
	var groupId [32]byte
	groupId[0] = 0x71
	writeKey, readKey := testEpochKeyPair()
	digestBytes, digestBody := testEpochDigestAttachmentBytes(t, groupId, 4, writeKey, readKey)
	plainBytes := testEpochAttachmentBytes(t, 4, writeKey, readKey)

	// (1) the kind 0x0005 commit: a delivery, carrying the epoch's own pair.
	delivery, err := epochKeysFor(testRecord(true, digestBytes), writeKey, readKey)
	if err != nil {
		t.Fatalf("a kind 0x0005 commit was refused its epoch keys: %v", err)
	}
	if delivery == nil {
		t.Fatal("a kind 0x0005 commit carries no epoch key delivery, and the delivery is the only " +
			"road its two keys have")
	}
	if !bytes.Equal(delivery.GetWriteKey(), writeKey) || !bytes.Equal(delivery.GetReadKey(), readKey) {
		t.Fatal("the delivery does not carry the pair the epoch opens with")
	}

	// AND THE DIGEST THE SERVER WOULD RECOMPUTE MATCHES IT, through connect's own checker rather
	// than through a second copy of the preimage spelled here. This is §5.1 check 3's new clause
	// run from the client side: MAC covers the attachment, attachment covers LP(H(epoch_keys)),
	// and the server recomputes H(epoch_keys) over these two fields.
	if err := message.CheckEpochKeysDigest(groupId, digestBody,
		delivery.GetWriteKey(), delivery.GetReadKey()); err != nil {
		t.Fatalf("the server would refuse this pair against its own attachment's digest: %v", err)
	}
	// the failing direction, in the same case, because a checker asked one question it cannot fail
	// is a checker that has been asked nothing. One octet of one key.
	bent := append([]byte(nil), delivery.GetWriteKey()...)
	bent[0] ^= 0x01
	if err := message.CheckEpochKeysDigest(groupId, digestBody, bent, delivery.GetReadKey()); err == nil {
		t.Fatal("a write_key differing in one octet still matched the attachment's digest")
	}
	// and the group is in the preimage (ruling 34), so the same pair under another group does not
	// verify against this attachment.
	other := groupId
	other[31] ^= 0x01
	if err := message.CheckEpochKeysDigest(other, digestBody,
		delivery.GetWriteKey(), delivery.GetReadKey()); err == nil {
		t.Fatal("the pair verified against this attachment's digest under a different group_id")
	}

	// (2) and (3): a kind 0x0001 commit and an ordinary record carry NOTHING, which is what keeps
	// this client accepted by a server that reads the keys out of the attachment.
	for _, one := range []struct {
		name   string
		record *message.Record
	}{
		{"a kind 0x0001 commit", testRecord(true, plainBytes)},
		{"an ordinary record", testRecord(false, nil)},
	} {
		answered, err := epochKeysFor(one.record, writeKey, readKey)
		if err != nil {
			t.Errorf("%s was refused: %v", one.name, err)
		}
		if answered != nil {
			t.Errorf("%s carries an epoch key delivery, and a delivery beside a kind 0x0001 commit is "+
				"refused by the server as a key it would not read", one.name)
		}
	}
}

// THE DELIVERY COPIES, AND THE COPY IS NOT DECORATION.
//
// messagegroup.EpochKeys.WriteKey hands back the session's OWN backing array — read the accessor:
// it answers `self.writeKey`, not a copy — and EpochKeys.Destroy zeroizes that array. Both commit
// paths hold their EpochKeys under a deferred Destroy, so a delivery that aliased would be two
// zero-filled keys by the time anything read it back, and the failure would be a server refusing a
// digest mismatch rather than anything naming the alias.
func TestTheEpochKeyDeliveryDoesNotAliasTheKeysItWasHanded(t *testing.T) {
	writeKey, readKey := testEpochKeyPair()
	delivery, err := epochKeyDelivery(writeKey, readKey)
	if err != nil {
		t.Fatalf("epochKeyDelivery: %v", err)
	}
	// the inline control: the delivery agrees with the pair BEFORE the erasure, so the assertion
	// after it is about the aliasing and not about the builder.
	if !bytes.Equal(delivery.GetWriteKey(), writeKey) || !bytes.Equal(delivery.GetReadKey(), readKey) {
		t.Fatalf("the delivery does not carry the pair it was handed")
	}
	for at := range writeKey {
		writeKey[at] = 0
		readKey[at] = 0
	}
	if bytes.Equal(delivery.GetWriteKey(), writeKey) {
		t.Errorf("erasing the caller's write_key erased the delivery's, so the delivery aliases it")
	}
	if bytes.Equal(delivery.GetReadKey(), readKey) {
		t.Errorf("erasing the caller's read_key erased the delivery's, so the delivery aliases it")
	}
}

// THE DIGEST THE SERVER WOULD RECOMPUTE IS A DIGEST OVER THE KEYS ON THE REQUEST.
//
// Under ruling 27 the server's §5.1 check 3 gains one question — are these two keys the ones this
// attachment's digest is over — and it answers it with message.CheckEpochKeysDigest over the pair
// the REQUEST handed it. This holds the client's half of that: the pair on the request is the pair
// the commit's epoch opens with, at the epoch the attachment names, under this group.
//
// IT IS HELD THROUGH connect's OWN FUNCTION AND NOT THROUGH A SECOND COPY OF THE PREIMAGE. A test
// that spelled "URmessage/v1/epochkeys" ‖ LP(group_id) ‖ u64 ‖ LP ‖ LP here would be a test of its
// own transcription; message.EpochKeysDigest is the one function that computes it and both ends
// call it.
//
// WHAT IS NOT HELD HERE, and it is the gap this commit does not close: the attachment those keys
// belong to is still a kind 0x0001 EpochAttachment carrying the pair IN THE CLEAR, because
// message.EncodeServerAttachment refuses kind 0x0005 and it is the only encoder
// messagegroup.GroupSession.SealRecord runs. So there is no EpochDigestAttachment on the wire for
// CheckEpochKeysDigest to be run against yet, and this case holds the half that exists: the keys
// the request carries are the epoch's own.
func TestTheEpochKeysOnTheRequestAreTheOnesTheDigestWouldBeOver(t *testing.T) {
	var groupId [32]byte
	for at := range groupId {
		groupId[at] = byte(0x11 + at)
	}
	writeKey, readKey := testEpochKeyPair()
	delivery, err := epochKeyDelivery(writeKey, readKey)
	if err != nil {
		t.Fatalf("epochKeyDelivery: %v", err)
	}
	const opensEpoch = 4

	// what a committer would have put in the attachment
	want, err := message.EpochKeysDigest(groupId, opensEpoch, writeKey, readKey)
	if err != nil {
		t.Fatalf("EpochKeysDigest over the epoch's own pair: %v", err)
	}
	// what a server recomputes from the request's two fields
	got, err := message.EpochKeysDigest(groupId, opensEpoch, delivery.GetWriteKey(), delivery.GetReadKey())
	if err != nil {
		t.Fatalf("EpochKeysDigest over the request's pair: %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("the digest over the request's pair is not the digest over the epoch's pair")
	}

	// THE FAILING DIRECTION, in the same case, because a comparison of one value against itself is
	// a comparison that cannot fail. Each of the three scope terms moves the digest: a different
	// epoch, a different group (ruling 34), and a bent key.
	for _, one := range []struct {
		name  string
		build func() ([]byte, error)
	}{
		{"one epoch further on", func() ([]byte, error) {
			return message.EpochKeysDigest(groupId, opensEpoch+1, delivery.GetWriteKey(), delivery.GetReadKey())
		}},
		{"another group", func() ([]byte, error) {
			var other [32]byte
			copy(other[:], groupId[:])
			other[0] ^= 0x01
			return message.EpochKeysDigest(other, opensEpoch, delivery.GetWriteKey(), delivery.GetReadKey())
		}},
		{"a write_key differing in one octet", func() ([]byte, error) {
			bent := append([]byte(nil), delivery.GetWriteKey()...)
			bent[0] ^= 0x01
			return message.EpochKeysDigest(groupId, opensEpoch, bent, delivery.GetReadKey())
		}},
		{"a read_key differing in one octet", func() ([]byte, error) {
			bent := append([]byte(nil), delivery.GetReadKey()...)
			bent[31] ^= 0x80
			return message.EpochKeysDigest(groupId, opensEpoch, delivery.GetWriteKey(), bent)
		}},
	} {
		other, err := one.build()
		if err != nil {
			t.Fatalf("%s: %v", one.name, err)
		}
		if bytes.Equal(want, other) {
			t.Errorf("the digest over %s equals the digest over the epoch's own pair", one.name)
		}
	}
}

// NOTHING A `protocol.Record` CARRIES IS A KEY, AND THAT IS RULING 33's WHOLE PURCHASE.
//
// `Record` is the server→client type in six places — FetchResponse.records, SubmitResult.
// winning_commit, RecordPush.records, TransientPush.records, WrapFetchResponse.records and
// GroupRecords.records. A key field on it would be six serve paths that each have to remember to
// clear it, which is item 244 re-opened once per path. connect holds the descriptor half of this
// (TestRecordCarriesNoKeysAndLeavesFourteenAlone, TestNoServerToClientMessageCarriesTheEpochKeys);
// what is held HERE is the half that is this package's: the projection THIS client builds and
// submits declares no field an epoch key could ride in.
//
// WHAT THIS CASE DOES NOT CLAIM, and the distinction is still the honest one. It is about the
// `Record` MESSAGE's own fields, not about `record_bytes`. That `record_bytes` carries no key
// either is now TRUE — the attachment is kind 0x0005 — but it is a different property with a
// different proof, and it is not asserted anywhere below: it is held at the octets by
// [TestItem244sKeysAreInTheRequestAndNotInTheRecord] and end to end by cp3b's TestItem244. A
// descriptor walk cannot see the contents of an opaque field, and a case that claimed both would
// be one of them resting on the other's evidence.
func TestTheSubmittedRecordDeclaresNoFieldAnEpochKeyCouldRideIn(t *testing.T) {
	fields := (&protocol.Record{}).ProtoReflect().Descriptor().Fields()
	names := []string{}
	for at := 0; at < fields.Len(); at += 1 {
		names = append(names, string(fields.Get(at).Name()))
	}
	// the inline positive control: the field the whole projection contract is about is present, so
	// a descriptor walk that found nothing would not pass by reading an empty list.
	found := false
	for _, name := range names {
		if name == "record_bytes" {
			found = true
		}
	}
	if !found {
		t.Fatalf("protocol.Record declares no record_bytes, so this walk is not reading the Record "+
			"descriptor and every refusal below is vacuous: %v", names)
	}
	t.Logf("protocol.Record declares %d fields: %v", len(names), names)
	for _, name := range names {
		if name == "write_key" || name == "read_key" || name == "epoch_keys" {
			t.Errorf("protocol.Record declares %q. Ruling 33 puts the epoch keys on the REQUEST and "+
				"never on Record, because Record is the server→client type in six places and a key "+
				"field on it is item 244 re-opened once per serve path — and because submit checks "+
				"proto.Equal(projectionOf(ParseRecord(record_bytes)), sent), which a field no parse "+
				"can imply makes false for every commit.", name)
		}
	}

	// and the delivery is not reachable FROM a Record, however it is spelled: the two epoch key
	// carriers are message fields of SubmitRequest and CreateGroupRequest, and nothing Record
	// declares has EpochKeyDelivery as its message type.
	for at := 0; at < fields.Len(); at += 1 {
		field := fields.Get(at)
		if field.Message() != nil && field.Message().FullName() ==
			(&protocol.EpochKeyDelivery{}).ProtoReflect().Descriptor().FullName() {
			t.Errorf("protocol.Record's field %q carries an EpochKeyDelivery", field.Name())
		}
	}
}

// THE TWO REQUEST CARRIERS ARE THE ONLY TWO, AND EACH CARRIES THE PAIR THE WAY ITS OWN RULE SAYS.
//
// This is the wire shape as this package builds it, marshalled and read back rather than inspected
// as a struct: what a server parses is the octets, and a field this client set on a message it then
// discarded would look identical in a struct assertion.
func TestTheTwoRequestsCarryTheEpochKeysTheWayTheirRulesSay(t *testing.T) {
	writeKey, readKey := testEpochKeyPair()
	delivery, err := epochKeyDelivery(writeKey, readKey)
	if err != nil {
		t.Fatalf("epochKeyDelivery: %v", err)
	}
	groupId := bytes.Repeat([]byte{0x71}, GroupIdBytes)

	// §4.3.2: SINGULAR and REQUIRED. There is no alignment because the request carries exactly one
	// record and it is always a commit.
	create := &protocol.CreateGroupRequest{
		GroupId:           groupId,
		InitialCommit:     &protocol.Record{IsCommit: true, RecordBytes: []byte{0x01}},
		BootstrapWriteKey: bytes.Repeat([]byte{0x05}, epochKeyBytes),
		EpochKeys:         delivery,
	}
	createBytes, err := proto.Marshal(create)
	if err != nil {
		t.Fatalf("marshal CreateGroupRequest: %v", err)
	}
	createBack := &protocol.CreateGroupRequest{}
	if err := proto.Unmarshal(createBytes, createBack); err != nil {
		t.Fatalf("unmarshal CreateGroupRequest: %v", err)
	}
	if createBack.GetEpochKeys() == nil {
		t.Fatalf("CreateGroupRequest.epoch_keys did not survive the wire, and §4.3.2 makes it REQUIRED")
	}
	if !bytes.Equal(createBack.GetEpochKeys().GetWriteKey(), writeKey) ||
		!bytes.Equal(createBack.GetEpochKeys().GetReadKey(), readKey) {
		t.Errorf("CreateGroupRequest.epoch_keys does not carry the pair epoch 1 opens with")
	}
	// AND IT IS NOT THE BOOTSTRAP KEY. write_key[0] certifies the founding commit; the pair above
	// is what that commit OPENS. Confusing the two is a group whose epoch 1 nobody can write to.
	if bytes.Equal(createBack.GetEpochKeys().GetWriteKey(), createBack.GetBootstrapWriteKey()) {
		t.Errorf("CreateGroupRequest carries the bootstrap write_key in the epoch_keys field")
	}

	// §4.3.3: REPEATED and POSITIONALLY ALIGNED. One commit, one entry, at the same index.
	commit := &protocol.SubmitRequest{
		GroupId:   groupId,
		Records:   []*protocol.Record{{IsCommit: true, RecordBytes: []byte{0x02}}},
		EpochKeys: []*protocol.EpochKeyDelivery{delivery},
	}
	commitBack := &protocol.SubmitRequest{}
	commitBytes, err := proto.Marshal(commit)
	if err != nil {
		t.Fatalf("marshal SubmitRequest: %v", err)
	}
	if err := proto.Unmarshal(commitBytes, commitBack); err != nil {
		t.Fatalf("unmarshal SubmitRequest: %v", err)
	}
	if len(commitBack.GetEpochKeys()) != len(commitBack.GetRecords()) {
		t.Fatalf("a submitted commit carries %d records and %d epoch key entries; the two lists are "+
			"positionally aligned and are either empty or the same length",
			len(commitBack.GetRecords()), len(commitBack.GetEpochKeys()))
	}
	for at, record := range commitBack.GetRecords() {
		if record.GetIsCommit() != (commitBack.GetEpochKeys()[at] != nil) {
			t.Errorf("record %d has is_commit = %v and its epoch key entry is %v",
				at, record.GetIsCommit(), commitBack.GetEpochKeys()[at])
		}
	}

	// and the ordinary record's request, which carries NO entry at all rather than a zero one
	ordinary := &protocol.SubmitRequest{
		GroupId: groupId,
		Records: []*protocol.Record{{IsCommit: false, RecordBytes: []byte{0x03}}},
	}
	ordinaryBytes, err := proto.Marshal(ordinary)
	if err != nil {
		t.Fatalf("marshal SubmitRequest: %v", err)
	}
	ordinaryBack := &protocol.SubmitRequest{}
	if err := proto.Unmarshal(ordinaryBytes, ordinaryBack); err != nil {
		t.Fatalf("unmarshal SubmitRequest: %v", err)
	}
	if len(ordinaryBack.GetEpochKeys()) != 0 {
		t.Errorf("a submission of one ordinary record put %d epoch key entries on the wire",
			len(ordinaryBack.GetEpochKeys()))
	}
	// the inline control on that zero: the SAME marshal/unmarshal round trip DOES carry an entry
	// when there is one, so the emptiness above is a property of the request and not of the check.
	if len(commitBack.GetEpochKeys()) != 1 {
		t.Errorf("the control request carries %d epoch key entries, so the zero above is not a "+
			"measurement", len(commitBack.GetEpochKeys()))
	}
}
