package urmessage

import (
	"bytes"
	"errors"
	"testing"

	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// §4.3.4'S ATTESTATION, HELD AGAINST THE PAGE IT ARRIVED WITH.
//
// WHY THIS CASE IS HERE AND NOT IN cp3b, said plainly because it is a limit on what has been
// measured: the deployed server SIGNS NOTHING (`msgrepo/api/fetch.go:112` -- "§4.3.4's
// FetchAttestation is absent, not empty"), and there is no way to make it produce one through
// `store.Store`, which is the only seam `cp3b` can bend. So the four description clauses below
// are UNREACHABLE against a real server today, and against a fake one they would be a test of the
// fake. They are driven here, directly, against the function that holds them.
//
// WHAT IS STILL NOT CHECKED ANYWHERE, and it is the one that matters: the SIGNATURE. Verifying it
// needs a fleet key chain the server does not publish and a compiled-in fleet root this workspace
// does not have. See [Group.Receive] and S2-27. Everything below is what a client can hold
// WITHOUT a key -- which is "this attestation describes this page" and never "this page is what
// the server has".
func TestAnAttestationThatDoesNotDescribeItsOwnPageIsRefused(t *testing.T) {
	groupId := bytes.Repeat([]byte{0x71}, GroupIdBytes)
	page := func() *protocol.FetchResponse {
		return &protocol.FetchResponse{
			Records: []*protocol.Record{
				{RecordId: 5},
				{RecordId: 6},
			},
			HighWaterRecordId: 9,
			Complete:          true,
			Attestation: &protocol.FetchAttestation{
				GroupId:           append([]byte(nil), groupId...),
				SinceRecordId:     4,
				RecordIds:         []uint64{5, 6},
				HighWaterRecordId: 9,
				ReadEpoch:         3,
			},
		}
	}
	group := &Group{id: groupId}

	// THE CONTROL: the whole, agreeing page is taken, and it is COUNTED as unattested because
	// its signature was not verified. Without this clause every refusal below would also pass on
	// a build that refused everything.
	if err := group.checkAttestationLocked(4, 3, page()); err != nil {
		t.Fatalf("an attestation that describes its own page was refused: %v", err)
	}
	if group.stats.Unattested != 1 {
		t.Fatalf("a page whose signature was not verified moved Stats.Unattested to %d, want 1",
			group.stats.Unattested)
	}

	for _, one := range []struct {
		name string
		bend func(*protocol.FetchResponse)
	}{
		{"it names another group", func(p *protocol.FetchResponse) {
			p.Attestation.GroupId = bytes.Repeat([]byte{0x27}, GroupIdBytes)
		}},
		{"it names another since_record_id", func(p *protocol.FetchResponse) {
			p.Attestation.SinceRecordId = 3
		}},
		// RULING 32. The value the request put on the wire itself, so the disagreement needs no
		// key -- and the arm it does NOT close is driven on its own, below.
		{"it names a read_epoch below the one the request authenticated under", func(p *protocol.FetchResponse) {
			p.Attestation.ReadEpoch = 2
		}},
		{"it names a read_epoch above the one the request authenticated under", func(p *protocol.FetchResponse) {
			p.Attestation.ReadEpoch = 4
		}},
		{"it names no read_epoch at all", func(p *protocol.FetchResponse) {
			p.Attestation.ReadEpoch = 0
		}},
		{"its high_water is not the response's", func(p *protocol.FetchResponse) {
			p.Attestation.HighWaterRecordId = 8
		}},
		{"it lists fewer records than the page carries", func(p *protocol.FetchResponse) {
			p.Attestation.RecordIds = []uint64{5}
		}},
		{"it lists more records than the page carries", func(p *protocol.FetchResponse) {
			p.Attestation.RecordIds = []uint64{5, 6, 7}
		}},
		{"one of its record ids is not the page's", func(p *protocol.FetchResponse) {
			p.Attestation.RecordIds = []uint64{5, 7}
		}},
		{"its high_water is below a record the page carries", func(p *protocol.FetchResponse) {
			p.Records = append(p.Records, &protocol.Record{RecordId: 11})
			p.Attestation.RecordIds = []uint64{5, 6, 11}
		}},
	} {
		t.Run(one.name, func(t *testing.T) {
			bent := page()
			one.bend(bent)
			if err := group.checkAttestationLocked(4, 3, bent); !errors.Is(err, ErrFetchAttestation) {
				t.Fatalf("an attestation where %s answered %v, want ErrFetchAttestation", one.name, err)
			}
		})
	}
}

// RULING 32, BOTH ARMS, IN ONE CASE: A TRUTHFUL CLAMP IS CAUGHT AND A LYING ONE IS NOT.
//
// The keyless comparison [Group.checkAttestationLocked] runs is "the attestation names the epoch
// this request authenticated under". The client sent `ReadEpoch: self.epoch` and knows what it
// sent, so a server that names a DIFFERENT ceiling has contradicted the request in a value the
// client already holds -- no fleet key, no signature, no PKI. That arm is real and it is closed.
//
// THE OTHER ARM IS NOT CLOSED AND MUST NOT BE DESCRIBED AS IF IT WERE. A server that clamps the
// reader to epoch 1, answers a `read_epoch = 3` request with the epoch-1 rows, and writes 3 in the
// attestation anyway produces a response that is IDENTICAL, field for field, to the response an
// honest server at ceiling 3 would produce for a group that happens to hold exactly those rows.
// The third clause below is that identity, asserted with proto.Equal rather than argued: if the two
// octet strings are the same, no keyless check can separate them, and this one does not.
//
// WHAT THE SIGNATURE WOULD BUY, STATED WITHOUT OVERCLAIMING. It does not make the lying answer
// distinguishable at the moment it is read -- a server that lies signs its lie. What §4.3.4 buys by
// putting `read_epoch` in the preimage is that the lie becomes an ATTRIBUTABLE COMMITMENT: the
// server has signed "I served you everything at or below epoch 3", and any member who can show a
// record at epoch 2 that the page omitted holds proof against a named server key. That is ruling
// 32 in its own words -- "what a signature buys is not the value; it is the server's attributable
// commitment to having used it" -- and both the signature and the fleet key are unbuilt.
func TestATruthfulClampIsCaughtAndALyingOneIsNot(t *testing.T) {
	groupId := bytes.Repeat([]byte{0x71}, GroupIdBytes)
	// the page a server clamping this reader to epoch 1 hands back: the epoch-1 rows, complete,
	// with a high water that is the max AT OR BELOW the ceiling it applied (F0).
	clamped := func(attestedReadEpoch uint64) *protocol.FetchResponse {
		return &protocol.FetchResponse{
			Records:           []*protocol.Record{{RecordId: 5}, {RecordId: 6}},
			HighWaterRecordId: 6,
			Complete:          true,
			Attestation: &protocol.FetchAttestation{
				GroupId:           append([]byte(nil), groupId...),
				SinceRecordId:     4,
				RecordIds:         []uint64{5, 6},
				HighWaterRecordId: 6,
				ReadEpoch:         attestedReadEpoch,
			},
		}
	}

	// (1) THE TRUTHFUL CLAMP IS CAUGHT. The request was MAC'd under read_epoch 3 and the server
	// says it answered at 1. Nothing here needed a key.
	group := &Group{id: groupId}
	err := group.checkAttestationLocked(4, 3, clamped(1))
	if !errors.Is(err, ErrFetchAttestation) {
		t.Fatalf("a server that truthfully named a ceiling below the one the request authenticated "+
			"under answered %v, want ErrFetchAttestation", err)
	}

	// (2) THE LYING CLAMP IS NOT. The same withheld page, with 3 written in the attestation, is
	// taken -- and the only trace it leaves is the counter that says nothing verified the
	// signature.
	before := group.stats.Unattested
	if err := group.checkAttestationLocked(4, 3, clamped(3)); err != nil {
		t.Fatalf("a server that clamped this reader and named the request's own read_epoch answered "+
			"%v; this check cannot see that and the sentence at the site says so", err)
	}
	if group.stats.Unattested != before+1 {
		t.Errorf("the lying clamp's page moved Stats.Unattested by %d, want 1",
			group.stats.Unattested-before)
	}

	// (3) AND IT IS NOT MERELY UNCHECKED, IT IS INDISTINGUISHABLE. The lying clamp's response and
	// the response an honest server at ceiling 3 would give for a group holding exactly these rows
	// are the same message. A keyless check that "caught" this would be a check that refused the
	// honest server too.
	lying := clamped(3)
	honest := clamped(3)
	if !proto.Equal(lying, honest) {
		t.Fatalf("the two responses differ, so clause (3) is not the identity it claims to be")
	}
	// the inline control on that equality: proto.Equal DOES separate the pair that differs only in
	// read_epoch, so the identity above is a fact about these two messages and not about the
	// comparison.
	if proto.Equal(clamped(1), clamped(3)) {
		t.Fatalf("proto.Equal reports two attestations naming read_epoch 1 and 3 as equal, so the " +
			"identity asserted above is vacuous")
	}
}

// ONE FLUSH PER VALUE AND NOT ONE PER CALL, AND A READ FLUSHES NOTHING.
//
// WHAT THIS CASE DOES NOT DO, stated first because the sentence that used to stand here claimed
// it did: it does NOT make the fsync in [DurableStateStore.writeRecord] undeletable. It said
// "remove that line and this number is zero on every write below" and that is false. MEASURED at
// this commit, both directions:
//
//	was:  syncErr := temp.Sync()
//	made: var syncErr error // the Sync is gone and the counter is not
//	      self.flushes += 1
//
//	go test -count=1 -race -run TestEveryValueTheDurableStoreNamesWasFlushedFirst ./urmessage -> ok
//	go test -count=1 -race ./urmessage (this gate excluded)                                   -> ok
//	cd cp3b && go test -count=1 -race ./...                                                   -> ok
//
// The increment is unconditional, so it counts a write that passed through writeRecord and not a
// flush that happened. WHAT HOLDS THE FLUSH IS TestEveryFsyncInThisPackageIsAtASiteThisSuiteNames
// in sourcegate_test.go, which reads this package's source and goes RED on exactly that mutation
// -- measured, same commit.
//
// WHAT THIS CASE DOES DO, which is worth having and is the whole of its claim: one flush per
// value and not one per call. A store that flushed twice per value would be paying twice for the
// same guarantee; one that batched would be reporting a value durable before it is; and one that
// flushed on a READ would be measuring calls rather than writes. All three move this number and
// all three are held below.
func TestEveryValueTheDurableStoreNamesWasFlushedFirst(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	if store.flushCount() != 0 {
		t.Fatalf("a freshly opened store has already flushed %d time(s)", store.flushCount())
	}
	for at, one := range []struct {
		name string
		call func() error
	}{
		{"PutGroupState", func() error { return store.PutGroupState(testGroupId, 0, testState) }},
		{"PutPrivateKey", func() error { return store.PutPrivateKey(testPub, testPriv) }},
		{"PutKeyPackage", func() error { return store.PutKeyPackage(testRef, testKp, testInit, testEnc) }},
		{"PutDeviceIdentity", func() error { return store.PutDeviceIdentity(testPub, testPriv, testKp, testWrapSeed) }},
		{"PutGroupRecord", func() error {
			return store.PutGroupRecord(&GroupRecord{
				GroupId: testGroupId, PqSecret: testPriv, GroupHandleKey: testPub, Epoch: 0,
			})
		}},
	} {
		if err := one.call(); err != nil {
			t.Fatalf("%s: %v", one.name, err)
		}
		if flushes := store.flushCount(); flushes != at+1 {
			t.Fatalf("after %d writes ending in %s the store has performed %d value flush(es)",
				at+1, one.name, flushes)
		}
	}
	// and a READ flushes nothing, which is what says the number counts writes rather than calls
	before := store.flushCount()
	if _, err := store.GetGroupState(testGroupId, 0); err != nil {
		t.Fatalf("GetGroupState: %v", err)
	}
	if _, err := store.GroupRecords(); err != nil {
		t.Fatalf("GroupRecords: %v", err)
	}
	if after := store.flushCount(); after != before {
		t.Errorf("two reads performed %d value flush(es)", after-before)
	}
}
