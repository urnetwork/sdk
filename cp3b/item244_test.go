package cp3b

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/message-server/api"
	"github.com/urnetwork/message-server/peer"
	"github.com/urnetwork/sdk/urmessage"
	"google.golang.org/protobuf/proto"
)

// ══════════════════════════════════════════════════════════════════════════════════════════════
// LEDGER ITEM 244: THE SERVED COMMIT HANDS A REMOVED MEMBER THE NEXT EPOCH'S READ AND WRITE KEYS
// ══════════════════════════════════════════════════════════════════════════════════════════════
//
// This file is the answer to "is item 244 fixed?" and it is deliberately one test rather than
// six, because the thing being asked about is one property and the way it used to be reported
// fixed was six cases that each held a piece of it.
//
// WHAT THE DEFECT WAS. Spec B §5.4 as RULED put `read_key[n+1]` and `write_key[n+1]` in the clear
// inside the commit record's `EpochAttachment`, and §4 has the read path rebuild `record_bytes`
// verbatim from the stored columns. So the commit that REMOVES a member is sealed at epoch n,
// fetchable under `read_key[n]` -- which that member legitimately holds -- and carries the keys of
// epoch n+1, which it must not have. Reproduced twice against this very server: a fetch
// authenticated under `read_key[1]` returned records across epochs {0,1,2}, the epoch-1 commit's
// attachment parsed to `read_key[2]` and `write_key[2]`, a fetch of epoch 2 under the LEARNED key
// answered REASON_OK, and a forged write at epoch 2 under a current member's handle answered
// REASON_OK and bricked that member through the stream-index latch.
//
// WHAT THE FIX IS (ruling 27, refined by rulings 33 and 34). A sixth attachment kind, `0x0005`,
// carrying the six PUBLIC fields of `EpochAttachment` and `LP(H(epoch_keys))` where the pair used
// to be, with the pair itself riding on the REQUEST messages -- `SubmitRequest.epoch_keys`
// positionally aligned with `records`, and `CreateGroupRequest.epoch_keys`, singular. The server
// recomputes `H(epoch_keys)` over the keys the request carried and compares. Nothing new is
// authenticated: `LP(H(server_attachment))` is already inside the `write_auth` preimage, so the
// MAC covers the attachment, the attachment covers the digest, and the digest covers the keys.
//
// WHY THE REQUEST AND NOT THE RECORD, which is ruling 33 and is the part that was got wrong once
// already. `protocol.Record` is the server→client type in SIX places -- `FetchResponse.records`,
// `SubmitResult.winning_commit`, `RecordPush.records`, `TransientPush.records`,
// `WrapFetchResponse.records`, `GroupRecords.records` -- so a key field on it would be six serve
// paths that each have to remember to clear it, which is item 244 re-opened once per path. Paying
// for a second field pair on the two REQUEST types instead makes the served type STRUCTURALLY
// unable to carry a key, and structurally is the only kind of unable that survives a new arm.
//
// ── WHAT THIS CASE MEASURES, AND WHERE EACH CLAUSE'S EVIDENCE COMES FROM ─────────────────────
//
// Everything below is over a REAL PUBLISH: a real `urmessage` client sealing through
// `messagegroup.GroupSession.SealRecord`, a real `connect.Client` carrying §4.2 frames, the real
// `api.Handler` running §5.1's pipeline, and the real `store.MemoryStore` behind it. Nothing here
// builds a record, computes an authenticator or hand-rolls a request.
//
//  1. THE KEYS ARE IN THE REQUEST. Read off [wireTap], which is the api layer's own doorway, so
//     these are the octets the server was handed and not values this test chose. That is also the
//     INLINE POSITIVE CONTROL for clause 2: the two keys are searched for in a place they MUST be
//     found before they are searched for in a place they must not, in the same run, over the same
//     needles. Without it, "not found" is a search that read nothing -- which is how this corpus
//     has been bitten before, and why the control is a t.Fatal and not a t.Error.
//
//  2. AND THEY ARE NOWHERE IN WHAT THE SERVER SERVES. Searched over the marshalled
//     `FetchResponse`s the api layer really answered -- every field of every `protocol.Record`,
//     `record_bytes` included -- and over every byte-bearing column of every row the store holds,
//     which is what an operator or a stolen dump sees. The two are different reaches on purpose:
//     the response is what a removed member would be served, the columns are what a thief gets.
//
//  3. THE DIGEST IS WHAT THE SERVER RECOMPUTES. Not asserted by equality with itself: the
//     attachment's field is compared against `message.EpochKeysDigest` taken over the REQUEST's
//     keys, and then against `message.CheckEpochKeysDigest`, which is the exact function
//     `msgrepo/api/epochkeys.go` calls with the exact inputs it passes -- the group id off the
//     request and the epoch off the attachment's own body. The failing direction is driven in the
//     same loop, because a comparison that has never failed is a comparison nobody has calibrated.
//
//  4. THE ALIGNMENT RULE HOLDS OVER EVERY REQUEST IN THE RUN, both directions: a submission
//     carrying a kind `0x0005` commit has exactly one delivery, and every other submission has
//     none. It is held over ALL the traffic rather than over the one commit this case is about,
//     because the rule is about a relationship and a rule checked at one point is a coincidence.
//
//  5. AND THE SERVER REFUSES THE SAME COMMIT WITH ITS KEYS TAKEN AWAY. A second whole run of the
//     same client against the same server, with [wireTap] clearing `epoch_keys` in flight, so the
//     server meets a genuinely sealed and genuinely MAC'd kind `0x0005` commit that differs from
//     an accepted one in exactly one field. It is §5.4's acceptance window in its refusing
//     direction, and it is what says clause 3 is a CHECK and not a value the server copies down.
//     The first version of this clause replayed a recorded request instead, and measured the
//     front's replay refusal; [wireTap.strip] carries that measurement.
//
// WHAT IT DOES NOT CLAIM. It says nothing about a REMOVED member, because nothing can be removed
// yet -- the Remove arm is step 6 of the removal track. Item 244's exposure is zero until that
// ships and this case is what must still be true on the day it does. It also says nothing about
// the 90-day sweep, which is unbuilt, or about §4.3.4's attestation, which is unsigned.

// ── the tap ──────────────────────────────────────────────────────────────────────────────────

// wireTap keeps every §4.3 request the api layer was handed and every response it gave back.
//
// IT BENDS NOTHING AND THAT IS WHAT MAKES IT EVIDENCE. `shapedStore` exists to bend a fetch;
// this exists to watch one. Every method hands the call straight to the real `*api.Handler` and
// keeps a CLONE of what went past -- a clone, because the api layer is free to retain or mutate
// what it was given and a test holding the same pointer would be reading the server's own state
// back as though it were the wire.
//
// It sits in front of the HANDLER rather than the STORE deliberately. The store sees rows; the
// handler sees the `FetchResponse`, which is where `record_bytes` is rebuilt and is therefore the
// only place in this process where "the bytes the server serves" exists as a value.
type wireTap struct {
	peer.Handler

	mutex   sync.Mutex
	creates []*protocol.CreateGroupRequest
	submits []*protocol.SubmitRequest
	fetches []*protocol.FetchResponse

	// WHEN SET, `epoch_keys` IS CLEARED ON ITS WAY IN, and this is the ONE thing this type does
	// that is not passive. It is how clause 5 reaches "a kind 0x0005 commit arrived with nothing
	// that opens the epoch it announces" with a request that is otherwise entirely genuine: the
	// record is really sealed, the `write_auth` MAC is really over it, the connection nonce is
	// really this connection's, and exactly one field differs from the request the same client
	// sends when this flag is off.
	//
	// IT IS HERE RATHER THAN AS A REPLAY, AND THE REPLAY IS WHY. The first version of clause 5
	// re-submitted a recorded request with its keys stripped, and the server refused it --
	// REASON_INTERNAL, no per-record results. So did the SAME REQUEST REPLAYED UNMODIFIED, which
	// is the control that was missing: the refusal was the front's, for replaying, and the
	// clause would have passed just as well against a server that never looked at `epoch_keys`
	// at all. Measured, both at REASON_INTERNAL, before this was rewritten.
	strip bool
}

func (self *wireTap) CreateGroup(ctx context.Context, conn *api.Connection,
	request *protocol.CreateGroupRequest) (protocol.Reason, *protocol.CreateGroupResponse, error) {

	self.mutex.Lock()
	self.creates = append(self.creates, proto.Clone(request).(*protocol.CreateGroupRequest))
	strip := self.strip
	self.mutex.Unlock()
	if strip {
		request = proto.Clone(request).(*protocol.CreateGroupRequest)
		request.EpochKeys = nil
	}
	return self.Handler.CreateGroup(ctx, conn, request)
}

func (self *wireTap) Submit(ctx context.Context, conn *api.Connection,
	request *protocol.SubmitRequest) (protocol.Reason, *protocol.SubmitResponse, error) {

	self.mutex.Lock()
	self.submits = append(self.submits, proto.Clone(request).(*protocol.SubmitRequest))
	strip := self.strip
	self.mutex.Unlock()
	if strip {
		request = proto.Clone(request).(*protocol.SubmitRequest)
		request.EpochKeys = nil
	}
	return self.Handler.Submit(ctx, conn, request)
}

func (self *wireTap) Fetch(ctx context.Context, conn *api.Connection,
	request *protocol.FetchRequest) (protocol.Reason, *protocol.FetchResponse, error) {

	reason, response, err := self.Handler.Fetch(ctx, conn, request)
	if response != nil {
		self.mutex.Lock()
		self.fetches = append(self.fetches, proto.Clone(response).(*protocol.FetchResponse))
		self.mutex.Unlock()
	}
	return reason, response, err
}

// ── the case ─────────────────────────────────────────────────────────────────────────────────

// needle is one epoch key, with the name of the request field it was read off, so that a failure
// says WHICH key was found where rather than "a 32 octet string turned up".
type needle struct {
	what string
	key  []byte
}

func TestItem244TheServedCommitHandsOutNoEpochKey(t *testing.T) {
	world := newWorldWith(t, worldOptions{recordWire: true})
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	for _, who := range []*persona{alice, bob} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}
	groupId := newGroupId(t)

	// BOTH COMMIT PATHS, because there are two and they are different code. openPair publishes
	// the FOUNDING commit through [urmessage.Group.Open] and `CreateGroupRequest`; adding a third
	// member publishes an EPOCH commit through `Group.publishCommitLocked` and `SubmitRequest`.
	// A case that drove only one would be measuring one literal.
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	carolGroup := hsAddAndJoin(t, ctx, aliceGroup, world.newPersona(t, "carol"))

	// ordinary traffic, so the fetches below are fetches a member really made, and so that the
	// alignment rule has non-commit submissions to be held over as well as commits.
	if _, err := aliceGroup.Send(ctx, "one ordinary line, after two epochs"); err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	for name, group := range map[string]*urmessage.Group{"bob": bobGroup, "carol": carolGroup} {
		for at := 0; at < 4; at += 1 {
			if _, err := group.Receive(ctx); err != nil {
				t.Fatalf("%s's Receive: %v", name, err)
			}
		}
	}

	world.wire.mutex.Lock()
	creates := world.wire.creates
	submits := world.wire.submits
	fetches := world.wire.fetches
	world.wire.mutex.Unlock()

	// ── (1) THE KEYS ARE ON THE REQUESTS, AND THIS IS THE POSITIVE CONTROL ───────────────────
	//
	// Every needle below is read OFF THE WIRE. A key this test invented would be a key nothing
	// could have served, and searching for one would be the shape of a passing test that measures
	// nothing at all.
	needles := []needle{}
	if len(creates) != 1 {
		t.Fatalf("the run made %d CreateGroupRequest(s), want exactly 1", len(creates))
	}
	create := creates[0]
	if create.GetEpochKeys() == nil {
		t.Fatal("§4.3.2's epoch_keys is absent from the founding request. Under ruling 33 it is " +
			"the ONLY road epoch 1's keys have, so the server was handed a kind 0x0005 commit and " +
			"nothing that opens the epoch it announces.")
	}
	needles = append(needles,
		needle{"CreateGroupRequest.epoch_keys.write_key", create.GetEpochKeys().GetWriteKey()},
		needle{"CreateGroupRequest.epoch_keys.read_key", create.GetEpochKeys().GetReadKey()})

	commitSubmits := 0
	for at, submit := range submits {
		for _, delivery := range submit.GetEpochKeys() {
			commitSubmits += 1
			needles = append(needles,
				needle{"SubmitRequest.epoch_keys.write_key", delivery.GetWriteKey()},
				needle{"SubmitRequest.epoch_keys.read_key", delivery.GetReadKey()})
		}
		_ = at
	}
	if commitSubmits != 1 {
		t.Fatalf("the run submitted %d epoch key deliver(ies) on SubmitRequest, want exactly 1 "+
			"(the commit that opens epoch 2, from adding carol)", commitSubmits)
	}
	for _, one := range needles {
		if len(one.key) != 32 {
			t.Fatalf("%s is %d octets, want 32; a short needle would not be found anywhere and "+
				"every refusal below would be vacuous", one.what, len(one.key))
		}
	}
	// the four needles are two distinct pairs: epoch 1's and epoch 2's. If the client sent one
	// pair twice, the searches below would be one search wearing two names.
	distinct := map[string]bool{}
	for _, one := range needles {
		distinct[string(one.key)] = true
	}
	if len(distinct) != len(needles) {
		t.Fatalf("the %d keys on the wire are only %d distinct values, so two epochs opened with "+
			"the same key material", len(needles), len(distinct))
	}

	// THE CONTROL FIRES: each needle is found in the marshalled request it came off. This is the
	// same bytes.Contains, over the same needles, in the same run, as clause 2 -- which is the
	// only arrangement that makes clause 2's silence mean anything.
	onTheWire, err := proto.Marshal(create)
	if err != nil {
		t.Fatalf("marshalling the founding request: %v", err)
	}
	for _, submit := range submits {
		encoded, err := proto.Marshal(submit)
		if err != nil {
			t.Fatalf("marshalling a submit: %v", err)
		}
		onTheWire = append(onTheWire, encoded...)
	}
	for _, one := range needles {
		if !bytes.Contains(onTheWire, one.key) {
			t.Fatalf("%s was not found in the requests this run sent, so this case's search "+
				"cannot find a key that IS there and every refusal below is vacuous", one.what)
		}
	}
	t.Logf("the control fires: %d distinct epoch keys, all %d of them found in the %d octets of "+
		"request this run sent", len(distinct), len(needles), len(onTheWire))

	// ── (2) AND NOWHERE IN WHAT THE SERVER SERVES ────────────────────────────────────────────
	//
	// THE RESPONSES FIRST: the marshalled FetchResponse is every byte a reader is answered,
	// `record_bytes` included, rebuilt by the api layer from the stored columns exactly as
	// §4.3.3 says. This is the reach a REMOVED MEMBER would have.
	if len(fetches) == 0 {
		t.Fatal("the api layer answered no fetch at all, so there is nothing served to search")
	}
	servedRecords := 0
	for at, response := range fetches {
		servedRecords += len(response.GetRecords())
		encoded, err := proto.Marshal(response)
		if err != nil {
			t.Fatalf("marshalling fetch response %d: %v", at, err)
		}
		for _, one := range needles {
			if bytes.Contains(encoded, one.key) {
				t.Errorf("fetch response %d, %d octets over %d record(s), CONTAINS %s. "+
					"ITEM 244 IS OPEN: this is the byte string a member holding an older "+
					"epoch's read key is served, and it is the next epoch's key.",
					at, len(encoded), len(response.GetRecords()), one.what)
			}
		}
	}
	if servedRecords == 0 {
		t.Fatal("the api layer answered fetches but served no record in any of them, so the " +
			"clause above searched a set of empty responses")
	}

	// AND THE ROWS: every byte-bearing column of every row the store holds. This is the reach a
	// STOLEN DUMP has, and it is a different question from the one above -- §5.3 asks for both.
	rows := world.allRows(t, groupId)
	if len(rows) == 0 {
		t.Fatal("the server holds no rows for this group, so the column search examined nothing")
	}
	commits := 0
	for _, row := range rows {
		columns := map[string][]byte{
			"ct_head":           row.CtHead,
			"ct_body":           row.CtBody,
			"server_attachment": row.ServerAttachment,
			"body_hash":         row.BodyHash,
			"sender_handle":     row.SenderHandle,
			"blob_id":           row.BlobId,
		}
		for name, column := range columns {
			for _, one := range needles {
				if bytes.Contains(column, one.key) {
					t.Errorf("the stored %s column of record %d (epoch %d, is_commit=%v) "+
						"CONTAINS %s", name, row.RecordId, row.Epoch, row.IsCommit, one.what)
				}
			}
		}
		if row.IsCommit {
			commits += 1
		}
	}
	if commits != 2 {
		t.Fatalf("the store holds %d commit row(s) for this group, want 2 (the founding commit "+
			"and the one that opens epoch 2)", commits)
	}
	// the summary is CONDITIONAL, because a line that says "neither key is anywhere" printed
	// beside four failures saying exactly where they are is a sentence the measurement does not
	// support -- and it is the sentence a reader skimming the output would believe.
	if !t.Failed() {
		t.Logf("neither key is in any of %d served record(s) across %d fetch response(s), nor in "+
			"any column of any of %d stored row(s)", servedRecords, len(fetches), len(rows))
	}

	// ── (3) EVERY COMMIT IS KIND 0x0005 AND ITS DIGEST IS WHAT THE SERVER RECOMPUTES ─────────
	//
	// The attachment is PARSED back out of the stored octets -- the bytes the sealer produced and
	// the write_auth MAC covers -- and not read off anything this test or this client remembers.
	group, err := epochDigestGroupIdFor(groupId)
	if err != nil {
		t.Fatalf("the group id: %v", err)
	}
	deliveries := map[uint64]*protocol.EpochKeyDelivery{}
	deliveries[1] = create.GetEpochKeys()
	for _, submit := range submits {
		for _, delivery := range submit.GetEpochKeys() {
			deliveries[2] = delivery
		}
	}
	checked := 0
	for _, row := range rows {
		if !row.IsCommit {
			continue
		}
		attachment, err := message.ParseServerAttachment(row.ServerAttachment)
		if err != nil {
			t.Fatalf("parsing the stored attachment of commit %d: %v", row.RecordId, err)
		}
		if attachment.Kind != message.AttachmentEpochDigest {
			t.Fatalf("the commit at record %d carries attachment kind 0x%04x, want 0x%04x. "+
				"Under kind 0x0001 the two keys are IN these octets, in the clear, which is "+
				"item 244 exactly.", row.RecordId, uint16(attachment.Kind),
				uint16(message.AttachmentEpochDigest))
		}
		body := attachment.EpochDigest
		delivery, found := deliveries[body.Epoch]
		if !found {
			t.Fatalf("the commit at record %d opens epoch %d and no request in this run carried "+
				"keys for that epoch", row.RecordId, body.Epoch)
		}

		// (a) the digest equals H over the REQUEST's keys -- the two sides come from two places,
		// which is the whole point: one off the sealed octets, one off the wire.
		want, err := message.EpochKeysDigest(group, body.Epoch,
			delivery.GetWriteKey(), delivery.GetReadKey())
		if err != nil {
			t.Fatalf("recomputing H(epoch_keys) for epoch %d: %v", body.Epoch, err)
		}
		if !bytes.Equal(want, body.EpochKeysDigest) {
			t.Errorf("the commit that opens epoch %d carries a digest that is NOT H over the "+
				"keys its own request delivered", body.Epoch)
		}

		// (b) and through the server's own function, with the server's own inputs. This is
		// `msgrepo/api/epochkeys.go`'s checkEpochKeysDigest with its one line of glue removed:
		// the group id the request named, at the [32]byte width the preimage frames, and the
		// epoch taken from the attachment's body because the checker refuses to be given one.
		if err := message.CheckEpochKeysDigest(group, body,
			delivery.GetWriteKey(), delivery.GetReadKey()); err != nil {
			t.Errorf("message.CheckEpochKeysDigest -- the exact call this server makes at §5.1 "+
				"check 3 -- refuses the commit that opens epoch %d: %v", body.Epoch, err)
		}

		// (c) THE FAILING DIRECTION, in the same loop. A comparison that has never answered no
		// is a comparison nobody has calibrated, and each of these moves one term of the
		// preimage: the group (ruling 34), the epoch, and each key.
		for _, bent := range []struct {
			what    string
			group   [32]byte
			body    *message.EpochDigestAttachment
			writeAt []byte
			readAt  []byte
		}{
			{"another group", flipFirst(group), body, delivery.GetWriteKey(), delivery.GetReadKey()},
			{"one epoch further on", group, withEpoch(body, body.Epoch+1),
				delivery.GetWriteKey(), delivery.GetReadKey()},
			{"a write_key differing in one octet", group, body,
				flipOctet(delivery.GetWriteKey(), 0), delivery.GetReadKey()},
			{"a read_key differing in one octet", group, body,
				delivery.GetWriteKey(), flipOctet(delivery.GetReadKey(), 31)},
		} {
			if err := message.CheckEpochKeysDigest(bent.group, bent.body,
				bent.writeAt, bent.readAt); err == nil {
				t.Errorf("the digest on the commit that opens epoch %d is accepted under %s, so "+
					"the comparison is not over the term it is supposed to bind",
					body.Epoch, bent.what)
			}
		}
		checked += 1
	}
	if checked != 2 {
		t.Fatalf("this case checked %d commit digest(s), want 2", checked)
	}

	// ── (4) THE ALIGNMENT RULE, OVER EVERY REQUEST THIS RUN MADE, BOTH DIRECTIONS ────────────
	//
	// §4.3.3: `epoch_keys` is positionally aligned with `records`. §5.4's acceptance window keys
	// it on the ATTACHMENT KIND and not on `is_commit`, and the distinction is the whole of the
	// window: a kind 0x0001 commit with a delivery beside it is refused as a second copy of two
	// keys the MAC already covers, and a kind 0x0005 commit without one is refused as an epoch
	// the server was never handed what opens. Held here over real traffic rather than over a
	// table, because a rule checked at one point is a coincidence.
	for at, submit := range submits {
		records := submit.GetRecords()
		keys := submit.GetEpochKeys()
		if len(records) != 1 {
			t.Fatalf("submit %d carries %d records; this client submits one at a time and "+
				"§4.3.3's alignment for a batch containing a commit is unsatisfiable", at, len(records))
		}
		parsed, err := message.ParseRecord(records[0].GetRecordBytes())
		if err != nil {
			t.Fatalf("parsing the record of submit %d: %v", at, err)
		}
		attachment, err := message.ParseServerAttachment(parsed.Header.ServerAttachment)
		if err != nil {
			t.Fatalf("parsing the attachment of submit %d: %v", at, err)
		}
		wantsKeys := parsed.Header.IsCommit && attachment != nil &&
			attachment.Kind == message.AttachmentEpochDigest
		if wantsKeys && len(keys) != 1 {
			t.Errorf("submit %d carries a kind 0x0005 commit and %d epoch key deliver(ies), "+
				"want exactly 1: the epoch it announces would be installed by a server that was "+
				"never given what opens it", at, len(keys))
		}
		if !wantsKeys && len(keys) != 0 {
			t.Errorf("submit %d carries no kind 0x0005 commit and %d epoch key deliver(ies), "+
				"want 0: an entry is aimed POSITIONALLY at the record beside it, so this is a "+
				"live epoch key pointed at a record that opens nothing", at, len(keys))
		}
	}
	t.Logf("the alignment rule holds over all %d submission(s) in this run, in both directions",
		len(submits))

	// ── (5) AND THE SERVER REFUSES THE SAME COMMIT WITH ITS KEYS TAKEN AWAY ──────────────────
	//
	// THE POINT OF THIS CLAUSE. Everything above would also be true of a server that copied the
	// digest down and never looked at it: the keys would still be only on the request, the
	// attachment would still hold a matching digest, and no fetch would carry a key. What makes
	// clause 3 a CHECK rather than a transcription is that the same commit without its keys is
	// REFUSED.
	//
	// IT IS A WHOLE SECOND RUN AND NOT A REPLAY. See [wireTap.strip] for the measurement that
	// forced that: a recorded request re-submitted with its keys stripped WAS refused, and so was
	// the same request replayed unmodified, both REASON_INTERNAL -- the front refusing a replay,
	// which is a refusal this clause would have counted as the server checking a digest it had
	// never looked at. So this world runs the same client through the same flow from a clean
	// start, and the tap clears exactly one field on its way in.
	//
	// THE INLINE POSITIVE CONTROL IS THE RUN ABOVE, in this same test: an identical
	// `openPair` over an identical world with `strip` off founded the group and opened it, which
	// is why reaching a refusal here is attributable to the one field and not to the fixture.
	hostile := newWorldWith(t, worldOptions{recordWire: true, stripEpochKeys: true})
	hostileAlice := hostile.newPersona(t, "alice-under-a-stripping-tap")
	hostileBob := hostile.newPersona(t, "bob-under-a-stripping-tap")
	for _, who := range []*persona{hostileAlice, hostileBob} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}
	hostileGroup, err := hostileAlice.device.CreateGroup(ctx, newGroupId(t))
	if err != nil {
		t.Fatalf("CreateGroup under the stripping tap: %v", err)
	}
	hostileKeyPackage, err := hostileBob.device.KeyPackage()
	if err != nil {
		t.Fatalf("bob's KeyPackage under the stripping tap: %v", err)
	}
	if _, err := hostileGroup.AddMember(hostileKeyPackage); err != nil {
		t.Fatalf("AddMember under the stripping tap: %v", err)
	}
	// the founding commit is kind 0x0005 and the tap takes its keys away in flight. §5.4's
	// acceptance window says the server must refuse it: an epoch it was never handed what opens.
	if err := hostileGroup.Open(ctx); err == nil {
		t.Error("THE SERVER ACCEPTED A KIND 0x0005 COMMIT WITH NO EPOCH KEYS BESIDE IT. " +
			"H(epoch_keys) is then a value it copied down and never compared against anything, " +
			"and every clause above is satisfied by a server that does not check the digest at " +
			"all -- which is item 244 open behind a fix that only looks like one.")
	} else {
		t.Logf("the same founding commit with epoch_keys cleared in flight is refused: %v", err)
	}
	// AND THE REQUEST REALLY DID CARRY THEM BEFORE THE TAP TOOK THEM, which is what says the
	// refusal is the server's answer to their absence and not the client declining to send.
	hostile.wire.mutex.Lock()
	hostileCreates := hostile.wire.creates
	hostile.wire.mutex.Unlock()
	if len(hostileCreates) == 0 {
		t.Fatal("the client under the stripping tap sent no CreateGroupRequest at all, so the " +
			"refusal above never reached the server and this clause measured nothing")
	}
	// EVERY one of them, because the client answers a REASON_REJECTED founding commit with a
	// fresh Hello and a re-MAC and sends it again -- so there are two here, and a control that
	// looked only at the first would still be looking at what the CLIENT built rather than at
	// what every attempt built.
	for at, one := range hostileCreates {
		if one.GetEpochKeys() == nil {
			t.Fatalf("attempt %d under the stripping tap carried no epoch_keys as it LEFT the "+
				"client, so the refusal above is the client declining to send rather than the "+
				"server refusing what it was handed, and this clause measured nothing", at)
		}
	}
	t.Logf("all %d founding attempt(s) left the client carrying epoch_keys and every one was "+
		"refused with them cleared in flight", len(hostileCreates))
}

// ── small helpers, each one line of intent ───────────────────────────────────────────────────

// epochDigestGroupIdFor is the [32]byte the preimage frames, refusing any other width for the
// reason `msgrepo/api/epochkeys.go` gives: a short group id would be zero-padded into a different
// group's preimage.
func epochDigestGroupIdFor(groupId []byte) ([32]byte, error) {
	var group [32]byte
	if len(groupId) != len(group) {
		return group, errGroupIdWidth
	}
	copy(group[:], groupId)
	return group, nil
}

var errGroupIdWidth = errorString("a group id is 32 octets and this one is not")

type errorString string

func (self errorString) Error() string { return string(self) }

func flipFirst(group [32]byte) [32]byte {
	group[0] ^= 0x01
	return group
}

func flipOctet(key []byte, at int) []byte {
	bent := append([]byte(nil), key...)
	bent[at] ^= 0x01
	return bent
}

// withEpoch is the same body with its epoch moved, which moves `opens_epoch` inside the preimage
// without touching the digest field -- so the comparison must fail.
func withEpoch(body *message.EpochDigestAttachment, epoch uint64) *message.EpochDigestAttachment {
	moved := *body
	moved.Epoch = epoch
	return &moved
}

func allResultsOk(response *protocol.SubmitResponse) bool {
	for _, result := range response.GetResults() {
		if result.GetReason() != protocol.Reason_REASON_OK {
			return false
		}
	}
	return response != nil && 0 < len(response.GetResults())
}

func reasonsOf(response *protocol.SubmitResponse) []protocol.Reason {
	reasons := []protocol.Reason{}
	for _, result := range response.GetResults() {
		reasons = append(reasons, result.GetReason())
	}
	return reasons
}
