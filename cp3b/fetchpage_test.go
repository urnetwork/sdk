package cp3b

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/message-server/store"
	"github.com/urnetwork/sdk/urmessage"
)

// A CONVERSATION LONGER THAN ONE FETCH PAGE COMES BACK WHOLE, AND ONE Receive IS WHAT DOES IT.
//
// §4.3.4's FetchResponse carries `complete` -- "false when truncated by limit OR by
// max_response_bytes; both are NORMAL" -- and an earlier build of [urmessage.Group.Receive] read
// none of it: one page came back as the whole history, with a NIL ERROR. A UI that called Receive
// once and rendered the slice showed half a conversation and believed it had all of it, which is
// the worst failure an alpha can have because a user cannot tell it from a quiet room.
//
// THE SERVER'S PAGE IS SET THE WAY AN OPERATOR WOULD SET IT and not through a test seam:
// `api.Config.MaxRecordsPerFetch` is §4.3.1's advertised `max_records_per_fetch`, it is a
// configured number in the deployed binary, and three is a legal value for it. The alternative
// would be to send 513 messages to cross the default, which measures the same thing more slowly.
//
// HOW IT FAILS IF THE PAGING IS DELETED: the group's own ceremony -- the founding commit, two
// wraps and the epoch-complete marker -- is four records ahead of the first message, so a single
// page of three answers ZERO messages. The count below is the assertion; the log line prints the
// page count beside it so a reader can see it really did take several.
func TestAConversationLongerThanOneFetchPageComesBackWhole(t *testing.T) {
	const pageLimit = 3
	const lines = 11

	world := newWorldWith(t, worldOptions{maxRecordsPerFetch: pageLimit})
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)

	typed := []string{}
	for at := 0; at < lines; at += 1 {
		text := fmt.Sprintf("line %d of %d, and every one of them has to come back", at+1, lines)
		if _, err := aliceGroup.Send(ctx, text); err != nil {
			t.Fatalf("alice's Send %d: %v", at+1, err)
		}
		typed = append(typed, text)
	}

	// ONE Receive.
	got, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("bob's Receive: %v", err)
	}
	if len(got) != lines {
		t.Fatalf("alice sent %d lines and ONE Receive answered %d over a %d-record page: %v",
			lines, len(got), pageLimit, textsOf(got))
	}
	for at, one := range got {
		if one.Text != typed[at] {
			t.Errorf("message %d came back as %q and was typed as %q", at, one.Text, typed[at])
		}
		if 0 < at && one.RecordId <= got[at-1].RecordId {
			t.Errorf("message %d is record %d and message %d is record %d, so the order is not the server's",
				at, one.RecordId, at-1, got[at-1].RecordId)
		}
	}
	stats := bobGroup.Stats()
	if stats.Pages < 2 {
		t.Fatalf("a %d-record page over %d records took %d page(s), so this case never reached the truncation path",
			pageLimit, stats.Fetched, stats.Pages)
	}
	t.Logf("%d lines over a %d-record page: %d pages, %d records fetched, %d opened, %d ceremony, %d own",
		lines, pageLimit, stats.Pages, stats.Fetched, stats.Opened, stats.SkippedCeremony, stats.SkippedOwn)

	// AND THE CURSOR LANDED AT THE END. A second Receive over a group nobody has written to
	// answers nothing and no error -- which is what says the loop terminated because the server
	// said complete, and not because it ran out of something.
	again, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("bob's second Receive: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("a second Receive over an unchanged group answered %d message(s): %v", len(again), textsOf(again))
	}
	assertNothingFailedToOpen(t, "bob", bobGroup)
}

// A SERVER THAT ADVERTISES §4.3.4 AND SENDS NO ATTESTATION IS REFUSED.
//
// This is the half of the attestation a client can check WITHOUT a key, and it is a real
// downgrade: `capabilities.attestation_supported` is the server's own claim, made in its Hello,
// and a page that arrives unsigned afterwards is either a server that lied or something between
// the two that stripped the field. The signature itself is NOT verified and
// [urmessage.Group.Receive] says at length why -- there is no fleet key chain and no compiled-in
// root to verify one against (S2-27) -- so this clause is the whole of what is enforced today.
//
// THE SERVER HERE IS THE REAL ONE AND THE ADVERTISEMENT IS THE ONLY THING CHANGED. msgrepo signs
// nothing whatever its Capabilities say (`api/fetch.go:112`: "§4.3.4's FetchAttestation is absent,
// not empty"), so turning the bit on is exactly "a server that claims to sign and does not".
func TestAServerThatAdvertisesAnAttestationAndSendsNoneIsRefused(t *testing.T) {
	world := newWorldWith(t, worldOptions{attestationSupported: true})
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	if _, err := aliceGroup.Send(ctx, "a line from a server that says it signs its fetches"); err != nil {
		t.Fatalf("alice's Send: %v", err)
	}

	got, err := bobGroup.Receive(ctx)
	if !errors.Is(err, urmessage.ErrFetchAttestation) {
		t.Fatalf("a fetch from a server advertising attestation support and sending none answered %v, want ErrFetchAttestation",
			err)
	}
	if len(got) != 0 {
		t.Errorf("%d message(s) came back from a page that was refused: %v", len(got), textsOf(got))
	}
	t.Logf("the downgrade is refused by name: %v", err)

	// THE CONTROL. The same server, the same records, with the advertisement OFF: the page is
	// taken, the message opens, and the page is COUNTED as unattested rather than silently
	// treated as verified. Without this clause the case above would also pass on a build that
	// refused every fetch.
	honest := newWorldWith(t, worldOptions{})
	honestAlice := honest.newPersona(t, "alice")
	honestBob := honest.newPersona(t, "bob")
	if err := honestAlice.device.Connect(ctx); err != nil {
		t.Fatalf("the control alice's Connect: %v", err)
	}
	if err := honestBob.device.Connect(ctx); err != nil {
		t.Fatalf("the control bob's Connect: %v", err)
	}
	honestGroupId := newGroupId(t)
	honestAliceGroup, honestBobGroup := openPair(t, ctx, honestAlice, honestBob, honestGroupId)
	if _, err := honestAliceGroup.Send(ctx, "a line from a server that never claimed to sign"); err != nil {
		t.Fatalf("the control alice's Send: %v", err)
	}
	control, err := honestBobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("the control Receive: %v", err)
	}
	if len(control) != 1 {
		t.Fatalf("the control read %d message(s), want 1: %v", len(control), textsOf(control))
	}
	if unattested := honestBobGroup.Stats().Unattested; unattested == 0 {
		t.Error("a page with no attestation was taken and counted as attested; Stats.Unattested did not move")
	}
}

// A SERVER THAT PAGES IN CIRCLES IS REFUSED RATHER THAN LOOPED ON.
//
// The decorator below is a real `store.Store` behind the real `api.Handler`: every check §5.1 runs
// still runs, and the only thing changed is the shape of the fetch RESULT -- no records,
// `complete` false, and a `next_record_id` that does not move. That is a server a client must not
// spin on, and an unbounded "page until complete" would hang the call forever with no error and
// no messages.
func TestAServerThatAnswersIncompleteAndAdvancesNoCursorIsRefused(t *testing.T) {
	world := newWorldWith(t, worldOptions{fetchShape: fetchStandsStill})
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	if _, err := aliceGroup.Send(ctx, "a line no fetch will ever page past"); err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	got, err := bobGroup.Receive(ctx)
	if !errors.Is(err, urmessage.ErrFetchNoProgress) {
		t.Fatalf("a fetch that advanced no cursor answered %v, want ErrFetchNoProgress", err)
	}
	if len(got) != 0 {
		t.Errorf("%d message(s) came back from a fetch that never advanced: %v", len(got), textsOf(got))
	}
	t.Logf("a server paging in circles is refused by name: %v", err)
}

// A SERVER THAT NEVER SAYS complete IS BOUNDED, AND WHAT COMES BACK CARRIES THE REFUSAL WITH IT.
//
// The decorator here advances the cursor by one per page and never answers complete, so the loop
// makes progress forever. [urmessage.maxFetchPages] is what stops it, and the contract is that the
// messages read so far come back TOGETHER WITH [urmessage.ErrFetchIncomplete] -- a caller that
// ignores the error still renders what arrived, and a caller that reads it knows there is more.
func TestAFetchThatNeverCompletesStopsAtItsPageBoundAndSaysSo(t *testing.T) {
	world := newWorldWith(t, worldOptions{fetchShape: fetchNeverCompletes})
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	const typed = "the one line this server will hand over before it starts padding"
	if _, err := aliceGroup.Send(ctx, typed); err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	got, err := bobGroup.Receive(ctx)
	if !errors.Is(err, urmessage.ErrFetchIncomplete) {
		t.Fatalf("a fetch that never completes answered %v, want ErrFetchIncomplete", err)
	}
	// AND THE MESSAGES ARE STILL THERE. This is the clause that separates "stopped early and
	// said so" from "refused the whole page": the first page carried a real message and it comes
	// back beside the refusal.
	if len(got) != 1 || got[0].Text != typed {
		t.Fatalf("the bounded fetch answered %v beside its refusal, want the one line alice sent", textsOf(got))
	}
	t.Logf("bounded, and what arrived came with it: %v", err)
}

// ── the two fetch shapes a real server must not be believed about ────────────────────────────

type fetchShape int

const (
	fetchNormal fetchShape = iota

	// no records, complete false, next_record_id unmoved: a server paging in circles.
	fetchStandsStill

	// no records, complete false, next_record_id one past where it was asked: a server that
	// makes progress and never finishes.
	fetchNeverCompletes

	// THE SILENT OMISSION. The ceremony goes over, every MESSAGE is held back, and `complete`
	// and `high_water_record_id` are left exactly as the real store computed them -- so the
	// server is TRUTHFUL about what it holds and simply does not hand it over. It is the one
	// failure the AEAD cannot see: nothing is tampered with, records are merely absent, and a
	// client that reads neither field answers an empty conversation with a nil error.
	fetchDropsMessages

	// THE SUBMIT LANDS AND THE ANSWER DOES NOT COME BACK. The record is written through the
	// real §6.1 transaction and then the RESPONSE is lost, which is what a connection dropping
	// between the server's commit and the client's read of the reply looks like from the
	// client: the send failed, and the record is on the server anyway. It is the one state in
	// which a device meets, on a later fetch, a record of its OWN that its log has never held.
	fetchLosesOneSubmitAnswer

	// ONE RECORD'S ct_body BENT ON ITS WAY OUT, for a bounded number of fetches. It is the
	// transient: a ciphertext a middlebox chewed, a body truncated in flight. The record on the
	// server is untouched -- this bends the RESULT -- so a later fetch of the same record is
	// the record, which is what makes "the client never asked again" the finding rather than
	// "the record was destroyed".
	fetchBendsOneRecord

	// THE SUBMIT NEVER REACHES THE SERVER AT ALL. It is a partition, and it is the one shape in
	// which a device SEALS -- consuming a stream index and producing a ciphertext -- while the
	// server learns nothing. Every other shape here lets the record land, which is what makes
	// the server able to answer REASON_STREAM_INDEX_REUSED to a second writer; this one is what
	// happens when there is no answer to be had.
	submitDropsBeforeWriting

	// THE SUBMIT LANDS AND IS ANSWERED A REFUSAL ANYWAY, which is the ONE shape that drives
	// S2-2's recovery into a server that already holds the record. The record goes through the
	// real §6.1 transaction and the RESULT's reason is then overwritten, so the client sees a
	// refusal, performs its one Hello + re-MAC + resubmission, and the resubmission arrives at
	// an index the server has a claim at ALREADY -- which is the exact state
	// [urmessage.Group.cloneRefusalLocked] must not read as a second writer. Without this shape
	// nothing in the suite puts an HONEST device at a consumed index with a live answer coming
	// back; `fetchLosesOneSubmitAnswer` loses the answer, so no REASON is ever seen at all.
	submitRefusesOnceAfterWriting

	// ONE RECORD HANDED BACK A SECOND TIME UNDER A RECORD ID THE SERVER NEVER ALLOCATED, appended
	// after everything the real store returned. The octets are the real record's, so every key
	// check passes; only the number is new. It is the replay a receiver ladder used to refuse, and
	// the one a record shown from this device's own copy has to refuse without a ladder.
	fetchRepeatsOneRecord
)

// shapedStore is `store.Store` with ONE method overridden.
//
// It is an embedding and not a reimplementation on purpose: every other call -- CreateGroup,
// Submit, the epoch keys, the group state -- is the real memory store's, so the group is founded,
// opened and written through the real §6.1 transaction, and only the fetch RESULT is bent. A
// hand-written double would have been a second server, and a case against a second server says
// nothing about this one.
type shapedStore struct {
	store.Store
	shape fetchShape

	// what fetchBendsOneRecord bends, and for how many more fetches. Under a mutex because a
	// case sets it from the test goroutine and the server reads it on its own.
	mutex        sync.Mutex
	bendRecordId uint64
	bendsLeft    int

	// what [shapedStore.bendAll] bends, and for how many more fetches each one is bent.
	bendMany map[uint64]int

	// how many more submit ANSWERS to lose, under fetchLosesOneSubmitAnswer.
	loseAnswers int

	// how many more submit answers to REFUSE after writing, under
	// submitRefusesOnceAfterWriting.
	refuseAnswers int

	// how many more submissions to DROP before writing, under submitDropsBeforeWriting.
	dropSubmissions int

	// the record fetchRepeatsOneRecord hands back a second time, zero for none.
	repeatRecordId uint64
}

// repeat names the record fetchRepeatsOneRecord hands back twice.
func (self *shapedStore) repeat(recordId uint64) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.repeatRecordId = recordId
}

// dropNextSubmissions makes the next n submissions fail without reaching the store at all.
func (self *shapedStore) dropNextSubmissions(n int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.dropSubmissions = n
}

// loseSubmitAnswers makes the next n submissions land and then fail to answer.
func (self *shapedStore) loseSubmitAnswers(n int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.loseAnswers = n
}

// Submit writes through the real transaction and THEN loses the answer, which is the ordering
// that matters: a shape that refused before writing would be an ordinary rejection and would
// leave the server with nothing, which is the opposite of the state under test.
func (self *shapedStore) Submit(ctx context.Context, request *store.SubmitRequest) (*store.SubmitResponse, error) {
	if self.shape == submitDropsBeforeWriting {
		self.mutex.Lock()
		drop := 0 < self.dropSubmissions
		if drop {
			self.dropSubmissions -= 1
		}
		self.mutex.Unlock()
		if drop {
			// BEFORE the real transaction, which is the whole difference from
			// fetchLosesOneSubmitAnswer: the server ends up holding nothing at this index.
			return nil, errors.New("this submission never reached the server")
		}
	}
	response, err := self.Store.Submit(ctx, request)
	if err != nil {
		return response, err
	}
	switch self.shape {
	case fetchLosesOneSubmitAnswer:
		self.mutex.Lock()
		defer self.mutex.Unlock()
		if self.loseAnswers <= 0 {
			return response, err
		}
		self.loseAnswers -= 1
		return nil, errors.New("the answer to this submission did not come back")
	case submitRefusesOnceAfterWriting:
		self.mutex.Lock()
		defer self.mutex.Unlock()
		if self.refuseAnswers <= 0 || response == nil {
			return response, err
		}
		self.refuseAnswers -= 1
		// the REASON only. The rows the real transaction wrote are untouched, which is
		// the whole point: the resubmission has to meet them.
		for _, result := range response.Results {
			result.Reason = protocol.Reason_REASON_INTERNAL
			result.RecordId = 0
		}
		return response, nil
	}
	return response, err
}

// refuseSubmitAnswers makes the next n submissions land and then be answered REASON_INTERNAL.
func (self *shapedStore) refuseSubmitAnswers(n int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.refuseAnswers = n
}

// bend names the record whose ct_body is bent on its way out of the store, and how many more
// fetches it is bent on. A count of one is the transient; a large one is the record that will
// never open.
func (self *shapedStore) bend(recordId uint64, fetches int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.bendRecordId, self.bendsLeft = recordId, fetches
}

// bendAll names SEVERAL records at once, each bent for the same number of fetches.
//
// IT IS NOT A CONVENIENCE OVER [shapedStore.bend]. One case has to bend a whole epoch fan-out
// because it cannot tell one member's wrap row from another's FROM THE SERVER SIDE: a
// wrap_target_handle is derived from the group_handle_key and the server -- correctly, m1 Task 14
// Property 2 -- cannot invert one, and neither can a test holding only what the store holds.
// Bending every wrap row of the epoch is how a case makes the one addressed to a named device
// unreadable without knowing which one it is.
func (self *shapedStore) bendAll(recordIds []uint64, fetches int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.bendMany == nil {
		self.bendMany = map[uint64]int{}
	}
	for _, recordId := range recordIds {
		self.bendMany[recordId] = fetches
	}
}

func (self *shapedStore) Fetch(ctx context.Context, request *store.FetchRequest) (*store.FetchResult, error) {
	result, err := self.Store.Fetch(ctx, request)
	if err != nil || self.shape == fetchNormal {
		return result, err
	}
	if self.shape == fetchLosesOneSubmitAnswer || self.shape == submitRefusesOnceAfterWriting ||
		self.shape == submitDropsBeforeWriting {
		// those shapes are about the SUBMIT path and bend nothing on the way out. They are
		// named here rather than left to fall through the switch below, because falling
		// through would silently give them fetchStandsStill's behaviour -- which is how a
		// case can be over a server nobody meant to build. It is not hypothetical: the
		// second of these two was written without this line and its first run answered
		// ErrFetchNoProgress from a fetch nobody had asked to bend.
		return result, nil
	}
	if self.shape == fetchRepeatsOneRecord {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		for _, record := range result.Records {
			if self.repeatRecordId == 0 || record.RecordId != self.repeatRecordId {
				continue
			}
			again := *record
			// above the high water, so the page is not also read as a server holding records back
			again.RecordId = result.HighWaterRecordId + 1000
			result.Records = append(result.Records, &again)
			break
		}
		return result, nil
	}
	if self.shape == fetchDropsMessages {
		// the ceremony goes, the messages do not, and NOTHING ELSE IS TOUCHED: complete
		// and high_water_record_id are the real store's own numbers.
		kept := []*store.Record{}
		for _, record := range result.Records {
			if record.IsCommit || len(record.ServerAttachment) != 0 {
				kept = append(kept, record)
			}
		}
		result.Records = kept
		return result, nil
	}
	if self.shape == fetchBendsOneRecord {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		// the SET first, and it is independent of the single-record arm below rather than a
		// special case of it: a case that names a set is bending a fan-out and a case that names
		// one record is bending one record, and neither is allowed to disturb the other's count.
		for _, record := range result.Records {
			left, marked := self.bendMany[record.RecordId]
			if !marked || left <= 0 || len(record.CtBody) == 0 {
				continue
			}
			bent := append([]byte(nil), record.CtBody...)
			bent[0] ^= 0xFF
			record.CtBody = bent
			self.bendMany[record.RecordId] = left - 1
		}
		if self.bendsLeft <= 0 {
			return result, nil
		}
		for _, record := range result.Records {
			if record.RecordId != self.bendRecordId || len(record.CtBody) == 0 {
				continue
			}
			bent := append([]byte(nil), record.CtBody...)
			bent[0] ^= 0xFF
			record.CtBody = bent
			self.bendsLeft -= 1
			break
		}
		return result, nil
	}
	// the first page is the honest one, so a case can hold what came back BEFORE the bending
	// started. After it, nothing but the shape.
	if request.SinceRecordId == 0 && self.shape == fetchNeverCompletes {
		result.Complete = false
		return result, nil
	}
	result.Records = nil
	result.Complete = false
	switch self.shape {
	case fetchStandsStill:
		result.NextRecordId = request.SinceRecordId
	case fetchNeverCompletes:
		result.NextRecordId = request.SinceRecordId + 1
	}
	return result, nil
}
