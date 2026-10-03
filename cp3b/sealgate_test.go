package cp3b

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/message-server/api"
	"github.com/urnetwork/message-server/peer"
	"github.com/urnetwork/message-server/store"
	"github.com/urnetwork/sdk/urmessage"
)

// ── a store that records every ct_body it is ever ASKED to write ─────────────────────────────
//
// IT RECORDS BEFORE THE REAL STORE RUNS, AND THAT ORDERING IS THE WHOLE INSTRUMENT. A collision
// is between one record that LANDED and one that DID NOT: the losing copy sealed its ciphertext
// before any server said anything, and the server then refused it. Every measurement taken off the
// server's stored rows is therefore blind to exactly half of the event -- it can say "one index is
// never used twice", which is a fact about the server's rows, and it cannot say "one keystream is
// never used twice", which is the fact §5.6 is about. This can.
type captureStore struct {
	store.Store
	mutex sync.Mutex
	seen  []*store.Record
}

func (self *captureStore) Submit(ctx context.Context, request *store.SubmitRequest) (*store.SubmitResponse, error) {
	self.mutex.Lock()
	for _, record := range request.Records {
		copied := *record
		copied.CtBody = append([]byte(nil), record.CtBody...)
		copied.CtHead = append([]byte(nil), record.CtHead...)
		copied.SenderHandle = append([]byte(nil), record.SenderHandle...)
		copied.BodyHash = append([]byte(nil), record.BodyHash...)
		self.seen = append(self.seen, &copied)
	}
	self.mutex.Unlock()
	return self.Store.Submit(ctx, request)
}

// distinctCiphertextsPerIndex is, for one sender, how many DISTINCT ct_bodies were submitted at
// each stream index. Distinct rather than counted: S2-2's recovery legitimately puts one record on
// the wire twice, and a raw count would call that a collision.
func (self *captureStore) distinctCiphertextsPerIndex(senderHandle []byte) map[uint64]int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	bodies := map[uint64][][]byte{}
	for _, record := range self.seen {
		if !bytes.Equal(record.SenderHandle, senderHandle) || record.IsCommit {
			continue
		}
		known := false
		for _, kept := range bodies[record.StreamIndex] {
			if bytes.Equal(kept, record.CtBody) {
				known = true
			}
		}
		if !known {
			bodies[record.StreamIndex] = append(bodies[record.StreamIndex], record.CtBody)
		}
	}
	counts := map[uint64]int{}
	for index, distinct := range bodies {
		counts[index] = len(distinct)
	}
	return counts
}

// submissionsAt is how many times this sender SUBMITTED at one index, distinct or not.
//
// IT IS A SEPARATE NUMBER FROM [captureStore.distinctCiphertextsPerIndex] AND THE DIFFERENCE IS A
// REAL PROPERTY. The keystream reuse is about DISTINCT ciphertexts; how many times a colliding
// ciphertext is put on the WIRE is about S2-2's recovery, which re-MACs and resubmits the very
// record the server just refused. Nothing but this count can tell those two apart, and without it
// the refusal at the first submit answer defends nothing a case can see.
func (self *captureStore) submissionsAt(senderHandle []byte, index uint64) int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	count := 0
	for _, record := range self.seen {
		if bytes.Equal(record.SenderHandle, senderHandle) && record.StreamIndex == index && !record.IsCommit {
			count += 1
		}
	}
	return count
}

// collidedIndices is every stream index at which this sender's handle carries more than one
// distinct ciphertext -- which is one record_key, one nonce and §5.6's total break of both AEADs.
func (self *captureStore) collidedIndices(senderHandle []byte) []uint64 {
	collided := []uint64{}
	counts := self.distinctCiphertextsPerIndex(senderHandle)
	for index := uint64(1); index < 1024; index += 1 {
		if 1 < counts[index] {
			collided = append(collided, index)
		}
	}
	return collided
}

// newCaptureWorld is [newWorldWith] with a capturing decorator in front of the memory store and
// nothing else changed: the same api.Handler, the same peer, the same real §6.1 transaction.
func newCaptureWorld(t *testing.T) (*world, *captureStore) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := connect.DefaultClientSettings()
	serverClient := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), settings)
	connections, err := peer.NewConnections(rand.Reader, time.Now, time.Hour)
	if err != nil {
		cancel()
		t.Fatalf("peer.NewConnections: %v", err)
	}
	checks, err := peer.NewChecks(connections, peer.DefaultMaxRequestBytes)
	if err != nil {
		cancel()
		t.Fatalf("peer.NewChecks: %v", err)
	}
	memory := store.NewMemoryStore(store.DefaultLimits())
	capture := &captureStore{Store: memory}
	handler, err := api.New(api.Config{
		Store:       capture,
		KnownGroups: api.NewMemoryKnownGroups(),
		Front:       checks,
	})
	if err != nil {
		cancel()
		t.Fatalf("api.New: %v", err)
	}
	served, err := peer.New(peer.Config{
		Client: serverClient, Handler: handler, Connections: connections, Checks: checks,
		Capabilities: &protocol.Capabilities{
			MaxRequestBytes:    peer.DefaultMaxRequestBytes,
			MaxRecordsPerFetch: 0,
		},
		ProtocolVersion: worldProtocolVersion,
		ServerId:        bytes.Repeat([]byte{0x5A}, 16),
	})
	if err != nil {
		cancel()
		t.Fatalf("peer.New: %v", err)
	}
	current := &world{ctx: ctx, cancel: cancel, serverClient: serverClient,
		store: memory, peer: served, handler: handler}
	t.Cleanup(func() { served.Close(); serverClient.Close(); cancel() })
	return current, capture
}

// newCaptureWorldShaped is [newCaptureWorld] with the committed fetch/submit decorator UNDER the
// capture, so the server bends its answers exactly as the committed omission and partition cases'
// servers do and every ct_body it is asked to write is still recorded. The ORDER matters: the
// capture has to see submissions the shape is about to drop, which is the whole instrument for
// [TestACopyThatSealsWhileItCannotReachTheServerIsBoundedByNothing].
func newCaptureWorldShaped(t *testing.T, shape fetchShape) (*world, *captureStore) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	serverClient := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(),
		connect.DefaultClientSettings())
	connections, err := peer.NewConnections(rand.Reader, time.Now, time.Hour)
	if err != nil {
		cancel()
		t.Fatalf("peer.NewConnections: %v", err)
	}
	checks, err := peer.NewChecks(connections, peer.DefaultMaxRequestBytes)
	if err != nil {
		cancel()
		t.Fatalf("peer.NewChecks: %v", err)
	}
	memory := store.NewMemoryStore(store.DefaultLimits())
	shaped := &shapedStore{Store: memory, shape: shape}
	capture := &captureStore{Store: shaped}
	handler, err := api.New(api.Config{Store: capture, KnownGroups: api.NewMemoryKnownGroups(), Front: checks})
	if err != nil {
		cancel()
		t.Fatalf("api.New: %v", err)
	}
	served, err := peer.New(peer.Config{
		Client: serverClient, Handler: handler, Connections: connections, Checks: checks,
		Capabilities:    &protocol.Capabilities{MaxRequestBytes: peer.DefaultMaxRequestBytes},
		ProtocolVersion: worldProtocolVersion, ServerId: bytes.Repeat([]byte{0x5A}, 16),
	})
	if err != nil {
		cancel()
		t.Fatalf("peer.New: %v", err)
	}
	current := &world{ctx: ctx, cancel: cancel, serverClient: serverClient,
		store: memory, peer: served, handler: handler, shaped: shaped}
	t.Cleanup(func() { served.Close(); serverClient.Close(); cancel() })
	return current, capture
}

// levelCopyOf stands up a second device over a copy of this one's two directories, taken while the
// original is LIVE and holding both of its exclusions -- which is what a backup utility does -- and
// drives it to the point where it has reconciled cleanly.
func levelCopyOf(t *testing.T, ctx context.Context, world *world, original *persona, name string) (
	*persona, *urmessage.Group) {

	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, name, "state")
	streamDir := filepath.Join(root, name, "stream")
	copyAppData(t, original.stateDir, stateDir)
	copyAppData(t, original.streamDir, streamDir)
	copy := world.durablePersona(t, name, stateDir, streamDir)
	if err := copy.device.Connect(ctx); err != nil {
		t.Fatalf("%s's Connect: %v", name, err)
	}
	restored, err := copy.device.Restore(ctx)
	if err != nil {
		t.Fatalf("%s's Restore: %v", name, err)
	}
	if len(restored) != 1 {
		t.Fatalf("%s restored %d group(s)", name, len(restored))
	}
	group := restored[0]
	// IT RECONCILES CLEANLY, AND THAT IS THE PREMISE OF EVERY CASE BELOW: a copy taken at the
	// exact current index agrees with the server and the server agrees with it, so clause 1 has
	// nothing to find. If this ever starts failing, the residual is closed and these cases are
	// measuring something that no longer happens.
	if _, err := group.Receive(ctx); err != nil {
		t.Fatalf("a copy taken at the exact current index did not reconcile cleanly: %v", err)
	}
	if err := group.IdentityInUse(); err != nil {
		t.Fatalf("a copy taken at the exact current index was caught before it sealed: %v", err)
	}
	return copy, group
}

// reserverHighWater is the index this device's DURABLE reserver has allocated up to. It is read off
// the reserver rather than off the group, because the reserver is the thing that cannot rewind and
// is therefore the thing a "did this device seal again?" assertion has to watch.
func reserverHighWater(t *testing.T, who *persona, groupId []byte, senderHandle []byte) uint64 {
	t.Helper()
	high, err := who.streamStore.StreamHighWater(groupId, senderHandle)
	if err != nil {
		t.Fatalf("%s's StreamHighWater: %v", who.name, err)
	}
	return high
}

// TWO COPIES THAT KEEP SENDING COLLIDE AT EXACTLY ONE INDEX, AND THE NUMBER IS THE POINT.
//
// WHY THIS CASE EXISTS: THE PUBLISHED BOUND WAS FALSE AND WAS FALSIFIED BY MEASUREMENT.
// [urmessage.Device.Restore]'s header and the commit that wrote it both said "THEY COLLIDE ONCE ...
// so the count is one record, not a stream of them". Clause 2 of the clone check lived only in
// [urmessage.Group.Receive]; [urmessage.Group.Send] consulted `identityInUse` and `reconciled` and
// nothing else. So two level copies that kept sending collided on EVERY index -- four typed
// messages, four indices each carrying two distinct ciphertexts under one keystream, with the
// ct_body XOR equal to the plaintext XOR on every one.
//
// THE COMMITTED CASE THAT WAS MEANT TO COVER THIS COULD NOT SEE IT.
// [TestTwoCopiesThatAreExactlyLevelCollideOnceAndTheLoserFindsOut] calls Receive immediately after
// the single collision, which is the friendly ordering and not the ordinary one: a user types the
// next line, they do not fetch first. THIS CASE NEVER CALLS Receive ON THE LOSER AT ALL, so the
// only thing that can stop it is the answer to its own submission -- which is exactly the path
// [urmessage.Group.cloneRefusalLocked] was added to.
//
// WHAT WOULD GO RED WITHOUT THE FIX: `collided` counts 4 rather than 1.
func TestTwoCopiesThatKeepSendingCollideAtExactlyOneIndex(t *testing.T) {
	world, capture := newCaptureWorld(t)
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
	if _, err := bobGroup.Send(ctx, "bob's only line before the copy"); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	bobHandle := senderHandleOf(t, aliceGroup)
	_, cloneGroup := levelCopyOf(t, ctx, world, bob, "bob-clone")

	// ── four rounds, and NEITHER side ever fetches ───────────────────────────────────────
	const rounds = 4
	var loserRefusal error
	for round := 0; round < rounds; round += 1 {
		_, cloneErr := cloneGroup.Send(ctx, fmt.Sprintf("CLONE round %d", round))
		_, originalErr := bobGroup.Send(ctx, fmt.Sprintf("ORIG  round %d", round))
		if originalErr != nil && loserRefusal == nil {
			loserRefusal = originalErr
		}
		t.Logf("round %d: clone %v / original %v", round, cloneErr, originalErr)
	}

	collided := capture.collidedIndices(bobHandle)
	t.Logf("stream indices carrying MORE THAN ONE distinct ciphertext under bob's handle: %v", collided)
	if len(collided) != 1 {
		t.Fatalf("%d stream indices carry two distinct ciphertexts under one (epoch, sender_handle, stream_index) (%v); the published bound is ONE",
			len(collided), collided)
	}

	// AND THE COLLIDING CIPHERTEXT GOES ON THE WIRE ONCE, WHICH IS A SECOND PROPERTY AND NOT THE
	// SAME ONE. S2-2's recovery answers a refusal with one Hello, one re-MAC and a RESUBMISSION
	// OF THE SAME RECORD -- so a build that read REASON_STREAM_INDEX_REUSED only at the retry
	// would put the loser's colliding ct_body on the wire a SECOND time before deciding
	// anything, which is what "3 submissions, 2 distinct ciphertexts" meant when this was
	// measured. The recovery repairs a NONCE and a reused index is not a nonce fact, so the
	// refusal is taken at the FIRST answer.
	if submissions := capture.submissionsAt(bobHandle, collided[0]); submissions != 2 {
		t.Fatalf("index %d saw %d submissions, want 2 (one from each copy): the colliding ciphertext was put on the wire more than once",
			collided[0], submissions)
	}
	t.Logf("index %d: 2 submissions and 2 distinct ciphertexts -- neither side resubmitted its collision",
		collided[0])

	// AND THE LOSER IS STOPPED BY ITS OWN SUBMISSION'S ANSWER, with no Receive anywhere in this
	// case. That is the repair: the check reaches the seal path.
	if !errors.Is(loserRefusal, urmessage.ErrIdentityInUse) {
		t.Fatalf("the refused side answered %v, want one wrapping ErrIdentityInUse", loserRefusal)
	}
	if bobGroup.IdentityInUse() == nil {
		t.Fatal("the refusal is not sticky, so the next Send seals again")
	}
	t.Logf("the loser is stopped at the submit: %v", loserRefusal)

	// THE WINNER CARRIES ON, which is correct and is the half a "wedge both" repair would have
	// broken: it is now the only writer of this stream and produces no further collision.
	if _, err := cloneGroup.Send(ctx, "the winner is the only writer now"); err != nil {
		t.Fatalf("the side whose record the server took was wedged too: %v", err)
	}
	if after := capture.collidedIndices(bobHandle); len(after) != 1 {
		t.Fatalf("the winner's further sends collided: %v", after)
	}
}

// AN HONEST DEVICE'S OWN RESUBMISSION AT A CONSUMED INDEX IS ANSWERED REASON_OK, NOT REUSED.
//
// THIS IS THE PRICE OF [urmessage.Group.cloneRefusalLocked] AND IT IS MEASURED RATHER THAN ARGUED.
// Making REASON_STREAM_INDEX_REUSED a sticky, permanent refusal is only safe if no healthy device
// can provoke it, and there is exactly one way a healthy device submits twice at one index: S2-2's
// recovery, which performs one Hello, one rebind, one re-MAC and one resubmission of the SAME
// record.
//
// THE MECHANISM, READ OUT OF BOTH OF THE SERVER'S STORES. Step (0)'s idempotency probe compares the
// submitted record's body_hash AND the hash of its ct_head against the claim standing at this
// (group_id, sender_handle, stream_index): equal on both is `probeIdentical` and REASON_OK,
// different is `probeDiffers` and REASON_STREAM_INDEX_REUSED (msgrepo `store/memory.go:442-452` and
// `store/pgx.go:985-996`). And `messagegroup.GroupSession.ReauthRecord` writes exactly one field --
// `record.WriteAuth` -- which the probe does not read. So the re-MAC'd resubmission is identical
// where it is compared.
//
// WHAT WOULD GO RED IF THAT REASONING WERE WRONG: this send fails with ErrIdentityInUse, and every
// dropped connection between a server's commit and a client's read of the reply would permanently
// accuse a user of running a copy of their app data.
func TestAnHonestResubmissionOfTheSameRecordIsAnsweredOkAndNotReadAsAClone(t *testing.T) {
	world := newWorldWith(t, worldOptions{fetchShape: submitRefusesOnceAfterWriting})
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

	// ONE refusal, so S2-2's single recovery is exactly enough: the record is written by the
	// first submission and the second meets its own claim.
	const typed = "a line whose first answer was a refusal and which is on the server anyway"
	world.shaped.refuseSubmitAnswers(1)
	sent, err := bobGroup.Send(ctx, typed)
	if err != nil {
		t.Fatalf("an honest resubmission at a consumed index was refused: %v", err)
	}
	t.Logf("the resubmission was accepted as record %d", sent.RecordId)

	// THE POINT: the clone refusal did not fire on a device with no copy anywhere near it.
	if err := bobGroup.IdentityInUse(); err != nil {
		t.Fatalf("an honest device wedged itself as a copy on its own S2-2 recovery: %v", err)
	}
	if rebound := bobGroup.Stats().Rebound; rebound != 1 {
		t.Errorf("Stats.Rebound is %d, so the recovery path this case exists to drive was not taken", rebound)
	}

	// THE CONTROL: the record really is on the server and really is the one that was typed. If
	// the first submission had written nothing, the resubmission would be an ordinary first
	// write and this case would be over nothing at all.
	got, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	if len(got) != 1 || got[0].Text != typed {
		t.Fatalf("alice read %v, want the one line that was typed exactly once", textsOf(got))
	}
	if indices := streamIndicesOf(t, world, groupId, senderHandleOf(t, aliceGroup)); len(indices) != 1 {
		t.Fatalf("one record was typed and the server holds stream indices %v for its sender", indices)
	}

	// and the device is not wedged: it still sends.
	if _, err := bobGroup.Send(ctx, "and the device that resubmitted still works"); err != nil {
		t.Fatalf("the sender's next Send: %v", err)
	}
}

// AND THE CLONE IS CAUGHT WHEN THE REUSED ARRIVES ONLY AT THE S2-2 RETRY, WHICH IS A SECOND
// ORDERING AND A SECOND SITE.
//
// WHY THE SECOND SITE EXISTS AT ALL. [urmessage.Group.cloneRefusalLocked] is consulted at BOTH of
// sendSealedLocked's answers, and deleting the retry one left the whole suite green -- the first
// one always fired first, so on the ordinary ordering the second is unreachable. That is a clause
// defending nothing until the ordering that needs it is driven, and this is that ordering.
//
// THE ORDERING. The loser's FIRST answer is something other than REUSED -- a REASON_INTERNAL, a
// server hiccup, a connection replaced underneath the binding, which is exactly the state S2-2's
// recovery exists for -- and the collision is only visible on the RESUBMISSION. Without the second
// site the loser gets a plain, NON-STICKY [urmessage.ErrSubmitRefused] and its next Send seals at
// the next index and collides there too, which is the defect in its original form reached by a
// different door.
//
// HOW IT IS BUILT, WITHOUT RE-ENTRANCY. The clone writes at the contested index first; then
// `submitRefusesOnceAfterWriting` MASKS the loser's first answer as REASON_INTERNAL. The masking
// hides a REUSED the server really did compute, so the retry -- which the shape no longer touches
// -- meets the true answer. Nothing is interleaved inside a server callback and no lock is held
// across a second request.
//
// WHAT WOULD GO RED WITHOUT THE RETRY-SITE CLAUSE: the refusal is ErrSubmitRefused and
// IdentityInUse() is nil, so the copy seals again.
func TestACloneCaughtOnlyAtTheS2_2RetryIsStillCaught(t *testing.T) {
	world := newWorldWith(t, worldOptions{fetchShape: submitRefusesOnceAfterWriting})
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
	if _, err := bobGroup.Send(ctx, "bob's only line before the copy"); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	_, cloneGroup := levelCopyOf(t, ctx, world, bob, "bob-clone")

	// the clone takes the contested index first, so the server really does hold different
	// content there when the loser arrives.
	if _, err := cloneGroup.Send(ctx, "the clone's line at the contested index"); err != nil {
		t.Fatalf("the clone's Send: %v", err)
	}

	// AND THE LOSER'S FIRST ANSWER IS MASKED, so the REUSED it would have seen is replaced by a
	// REASON_INTERNAL and only the S2-2 resubmission meets the truth.
	world.shaped.refuseSubmitAnswers(1)
	_, err := bobGroup.Send(ctx, "the original's line at the same contested index")
	if !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("a clone whose REUSED arrived only at the S2-2 retry answered %v, want ErrIdentityInUse", err)
	}
	if bobGroup.IdentityInUse() == nil {
		t.Fatal("the refusal taken at the retry is not sticky, so the next Send seals again")
	}
	t.Logf("caught at the retry: %v", err)

	// and it really is stopped, which is the consequence the stickiness exists for.
	before := reserverHighWater(t, bob, groupId, senderHandleOf(t, aliceGroup))
	if _, err := bobGroup.Send(ctx, "and the loser must not seal again"); !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("the loser's next Send answered %v", err)
	}
	if after := reserverHighWater(t, bob, groupId, senderHandleOf(t, aliceGroup)); after != before {
		t.Fatalf("the loser's reserver went %d -> %d, so it sealed again", before, after)
	}
}

// AND HERE IS WHERE THE BOUND STOPS HOLDING, WRITTEN DOWN AS A CASE SO THAT NOBODY HAS TO
// REDISCOVER IT.
//
// EVERY CLAUSE OF THE CLONE CHECK IS FED BY SOMETHING THE SERVER SAID. Clauses 1 and 2 read records
// the server handed over; clause 3 reads the reason it answered a submission. A copy that RECONCILED
// while it had a connection and then LOST it still seals -- [urmessage.Group.Send] consumes a stream
// index and produces a ciphertext BEFORE it submits, which is deliberate and is what makes a lost
// submit answer recoverable -- and no evidence ever arrives. So it collides once per Send, and
// nothing in this build bounds that.
//
// MEASURED HERE RATHER THAN REASONED: four sends behind a partition against four by the original
// produce FOUR contested indices, with IdentityInUse nil on both sides.
//
// IT IS NOT A REGRESSION AND IT IS NOT NEW. At b2db371 there was no seal-path check at all, so this
// case behaved exactly as it does now; what is new is that it is the ONLY remaining shape that is
// unbounded, and that [urmessage.Device.Restore]'s bound paragraph now says so instead of implying
// otherwise. The neighbouring sentence -- "a copy that cannot reach the server at all is caught,
// because clause 1 refuses the send when the reconciliation has not run" -- is TRUE and is about a
// copy that never reconciled; this is a copy that DID, and the two are one word apart.
//
// WHAT WOULD CLOSE IT is not a detection: it is either S2-28's new leaf, or a send path that will
// not seal at a new index while an older one is unacknowledged -- which would break the lost-answer
// recovery [TestARecordWhoseSubmitAnswerWasLostComesBackAsItsSendersOwn] depends on, and is a
// protocol decision rather than a repair.
//
// IF THIS CASE EVER GOES RED BECAUSE THE COUNT DROPPED, THAT IS GOOD NEWS: the hole closed, and the
// bound paragraph in restore.go is what must be updated.
func TestACopyThatSealsWhileItCannotReachTheServerIsBoundedByNothing(t *testing.T) {
	world, capture := newCaptureWorldShaped(t, submitDropsBeforeWriting)
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
	if _, err := bobGroup.Send(ctx, "bob's only line before the copy"); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	bobHandle := senderHandleOf(t, aliceGroup)
	_, cloneGroup := levelCopyOf(t, ctx, world, bob, "bob-clone")

	// THE COPY GOES DARK. It reconciled a moment ago, so clause 1 is satisfied and behind it;
	// what it loses now is every channel the remaining clauses are fed through. Two drops per
	// send, because S2-2's recovery resubmits once.
	const rounds = 4
	world.shaped.dropNextSubmissions(2 * rounds)
	for at := 0; at < rounds; at += 1 {
		if _, err := cloneGroup.Send(ctx, fmt.Sprintf("CLONE behind a partition %d", at)); err == nil {
			t.Fatalf("send %d succeeded, so this case is not over a partition", at)
		}
	}
	if err := cloneGroup.IdentityInUse(); err != nil {
		t.Fatalf("the partitioned copy was stopped by %v -- the hole this case documents has closed, and restore.go's bound paragraph must be updated", err)
	}
	world.shaped.dropNextSubmissions(0)

	// and the original, still connected, seals at exactly those indices.
	for at := 0; at < rounds; at += 1 {
		if _, err := bobGroup.Send(ctx, fmt.Sprintf("ORIG  connected %d", at)); err != nil {
			t.Fatalf("the original's Send %d: %v", at, err)
		}
	}

	collided := capture.collidedIndices(bobHandle)
	t.Logf("THE UNBOUNDED SHAPE: %d sends behind a partition produced %d contested indices %v",
		rounds, len(collided), collided)
	if len(collided) != rounds {
		t.Fatalf("%d sends behind a partition produced %d contested indices (%v); this case documents ONE PER SEND, and a different number means the shape changed",
			rounds, len(collided), collided)
	}
	// and NEITHER side knows, which is the whole of why it is unbounded rather than merely bad.
	if cloneGroup.IdentityInUse() != nil || bobGroup.IdentityInUse() != nil {
		t.Errorf("a side was stopped after the fact (clone %v, original %v); the hole may have narrowed",
			cloneGroup.IdentityInUse(), bobGroup.IdentityInUse())
	}
}

// A RESTARTED LOSING COPY COSTS AT MOST ONE MORE CONTESTED INDEX, WHICH IS WHAT MAKES THE BOUND A
// BOUND.
//
// THE STICKY REFUSAL IS PER PROCESS, so "one index" would be an empty claim if a restart simply
// reopened the hole. It does not, and the reason is clause 1: the restarted copy must Receive
// before it may Send ([urmessage.ErrNotReconciled]), and its first send of the new lifetime meets
// the winner's claim and is refused the same way.
//
// SO THE HONEST BOUND IS: ONE CONTESTED INDEX PER GROUP PER PROCESS LIFETIME OF THE LOSING COPY,
// not one per Send. That sentence is what [urmessage.Device.Restore]'s header now says and this is
// the case that holds it to it. It is deliberately NOT written as "one for ever": a user who
// restarts a copied folder ten times pays up to ten, and saying so is the difference between a
// bound and a slogan.
//
// WHAT WOULD GO RED WITHOUT THE FIX: the restarted copy sends freely again and the second
// lifetime's collisions grow with the number of lines typed.
func TestARestartedLosingCopyCostsAtMostOneMoreIndex(t *testing.T) {
	world, capture := newCaptureWorld(t)
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
	if _, err := bobGroup.Send(ctx, "bob's only line before the copy"); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	bobHandle := senderHandleOf(t, aliceGroup)
	_, cloneGroup := levelCopyOf(t, ctx, world, bob, "bob-clone")

	// ── lifetime one: the clone wins the race, the original is stopped ───────────────────
	if _, err := cloneGroup.Send(ctx, "the clone's contested line"); err != nil {
		t.Fatalf("the clone's Send: %v", err)
	}
	if _, err := bobGroup.Send(ctx, "the original's contested line"); !errors.Is(err, urmessage.ErrIdentityInUse) {
		t.Fatalf("the losing side answered %v, want ErrIdentityInUse", err)
	}
	stopped := reserverHighWater(t, bob, groupId, bobHandle)
	for at := 0; at < 3; at += 1 {
		if _, err := bobGroup.Send(ctx, "the loser keeps typing"); !errors.Is(err, urmessage.ErrIdentityInUse) {
			t.Fatalf("the loser's send %d answered %v", at, err)
		}
	}
	if now := reserverHighWater(t, bob, groupId, bobHandle); now != stopped {
		t.Fatalf("the loser's reserver went %d -> %d after it was stopped, so it sealed again", stopped, now)
	}
	if first := capture.collidedIndices(bobHandle); len(first) != 1 {
		t.Fatalf("the first lifetime collided at %v, and the bound is one", first)
	}

	// the winner goes on being the only writer, which is what puts records at the indices the
	// restarted loser's reserver will hand out next.
	for at := 0; at < 3; at += 1 {
		if _, err := cloneGroup.Send(ctx, fmt.Sprintf("the winner's line %d", at)); err != nil {
			t.Fatalf("the winner's Send %d: %v", at, err)
		}
	}

	// ── lifetime two: the SAME directory, nothing carried in memory ──────────────────────
	bob = world.restart(t, bob)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted loser's Connect: %v", err)
	}
	back, err := bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted loser's Restore: %v", err)
	}
	if len(back) != 1 {
		t.Fatalf("the restarted loser came back into %d group(s)", len(back))
	}
	bobGroup = back[0]
	if bobGroup.IdentityInUse() != nil {
		t.Fatal("the sticky refusal survived a restart, which this case's premise says it does not")
	}

	// IT MUST LISTEN FIRST, which is clause 1 and is what keeps the second lifetime bounded.
	if _, err := bobGroup.Send(ctx, "the restarted loser speaks before it listens"); !errors.Is(err, urmessage.ErrNotReconciled) {
		t.Fatalf("a restored group sealed before it reconciled: %v", err)
	}
	t.Logf("the restarted loser's reconciling Receive: %v", func() error {
		_, err := bobGroup.Receive(ctx)
		return err
	}())

	// AND THE SECOND LIFETIME COSTS AT MOST ONE MORE. Whether the reconciliation stopped it or
	// its own submission's answer did, what must not happen is a second, third and fourth.
	for at := 0; at < 4; at += 1 {
		if _, err := bobGroup.Send(ctx, fmt.Sprintf("the restarted loser's line %d", at)); err != nil {
			if !errors.Is(err, urmessage.ErrIdentityInUse) && !errors.Is(err, urmessage.ErrNotReconciled) {
				t.Fatalf("the restarted loser's send %d answered %v", at, err)
			}
		}
	}
	collided := capture.collidedIndices(bobHandle)
	t.Logf("contested indices over BOTH lifetimes: %v", collided)
	if 2 < len(collided) {
		t.Fatalf("two process lifetimes of a losing copy contested %d indices (%v); the bound is one per lifetime",
			len(collided), collided)
	}
	if bobGroup.IdentityInUse() == nil {
		t.Fatal("the restarted losing copy was never stopped at all, so the bound is not a bound")
	}
	t.Logf("the second lifetime was stopped by: %v", bobGroup.IdentityInUse())
}
