// A NEWCOMER ON A REMOVED MEMBER'S LEAF INHERITS ITS sender_handle, AND WHAT THAT COSTS IS FOUR
// PIECES OF sdk STATE. LEDGER ITEM 245.
//
// ── THE DEFECT, IN ONE SENTENCE AND ONE DERIVATION ───────────────────────────────────────────
//
//	sender_handle = SenderHandle(group_handle_key, leaf)
//
// No epoch. No identity. And group_handle_key is the epoch-ZERO storage root's expansion, fixed for
// the life of the group and never rotated. RFC 9420 §7.7 refills the LEFTMOST BLANK leaf, so the
// first Add after a Remove puts a NEWCOMER on the removed member's leaf -- and therefore under the
// removed member's sixteen octets, byte for byte, for ever.
//
// Item 245 was RULED 2026-09-22: NO WIRE CHANGE. Both discriminator designs died on their own
// headline claims, and the consequence most cited for moving the wire -- "the newcomer can never
// send" -- is a special case of open item 205, which any current member can reach today in a group
// that has never removed anybody. What is left that is unique to this item is state, and it is
// what this file drives:
//
//  1. THE RESERVER SEED, AND THE GATE THAT MAKES IT A RULE. The newcomer's durable reserver has
//     never allocated for that stream, so its first send seals at index 1 -- an index the server
//     already holds a claim at -- is answered REASON_STREAM_INDEX_REUSED, and [ErrIdentityInUse]
//     latches for the life of the process. [Group.seedOwnStreamLocked] moves the floor on the
//     first walk; [Group.ownFloorHeld] is what refuses a Send that would happen before it, and it
//     is driven over a real server in cp3b.
//  2. THE LADDER, AND THE INDEX SPACE IS ONE RUN ACROSS BOTH OCCUPANTS. The stream index is
//     allocated per (group_id, sender_handle) and the server refuses every index the removed
//     member spent, so the newcomer's first ACCEPTED record continues that numbering. The
//     survivor's ladder must therefore stand at the removed member's head and not at 0 --
//     [Group.leafStreamFloorLocked] -- while the re-track row that would install a ladder at
//     every future epoch for a leaf nobody occupies is dropped, [Group.pruneRemovedLaddersLocked].
//  3. THE PER-RECORD-EPOCH HANDLE TABLE. [Group.leavesLocked] built the table at the CURRENT epoch
//     only, so every record the removed leaf sealed BELOW the commit resolved to no leaf and was
//     abandoned after [maxRecordAttempts]. [Group.leavesAtLocked], with [Group.departedAt]
//     carrying the LAST departure so a leaf that changed hands twice keeps the middle occupant's
//     records too.
//  4. ATTRIBUTION OFF THE SIGNED LEAF, AND OFF OCTETS THIS DEVICE SEALED WHERE NOTHING SIGNS.
//     `mine := header.SenderHandle == walk.own` was true of every record the previous occupant of
//     this device's leaf ever wrote. [Group.recordIsOwnLocked] and [Message.SenderIdentity] decide
//     it on the identity the open authenticated; [Group.noteEpochGapLocked] -- the road EVERY one
//     of those records actually takes -- decides it on the index and body_hash this device sealed.
//
// ── THE CLAIM THIS FILE WAS SENT TO VERIFY RATHER THAN REPEAT ────────────────────────────────
//
// Item 245 says the reserver seed closes the message_id collision "for free, because message_id is
// computed from the record's own header, so disjoint index ranges give disjoint ids with no
// preimage change" -- and the ruling that no wire change is needed RESTS on that being true. It is
// measured here in both directions, from production's own derivation, in
// [TestANewcomerOnAReusedLeafCanSendAndItsMessageIdsAreDisjoint]:
//
//	MessageId = HKDF-Expand(group_handle_key, "mid/v1" ‖ LP(group_id) ‖ LP(sender_handle) ‖ u64(index), 32)
//
// group_id and sender_handle are EQUAL by construction for two occupants of one leaf, so the id is
// a function of the stream index alone across them. The collision at an EQUAL index is driven as
// the control -- two different members' records under ONE message_id -- and the disjointness above
// the seed is driven as the property. Neither is asserted from the formula: both are computed by
// [messagegroup.GroupSession.MessageIdOf], the door production uses.
//
// ── WHAT THIS FILE DOES NOT MEASURE, AND WHICH MUTANT SAYS SO ────────────────────────────────
//
// ONE OF THE FOUR PIECES IS UNMEASURED AND IT IS A THEOREM RATHER THAN A GAP. Replacing
// [Group.recordIsOwnLocked]'s whole body with `return maybeMine` -- the pre-repair line, the fourth
// piece's own mutant -- leaves every case here and all 176 cases of this package GREEN. The two
// facts that make that difference unproducible are written where the function is; the short of it
// is that a device can only open records at epochs it holds state for, and at every one of those it
// stands at its own leaf, so the two answers can differ only for a device whose HANDLE HAS MOVED --
// a removal and a re-Add of this device, which the sdk exposes no product method for.
//
// THE MUTANT THAT DOES DIE IS THE FAITHFUL SPELLING OF "ATTRIBUTION BACK TO THE HANDLE":
// [Message.SenderIdentity] filled from `header.SenderHandle` instead of from the signed leaf turns
// three cases here RED, because that is the value a reader attributes by and it is the one the two
// occupants of a leaf share. And since [TestANewcomerDoesNotShowTheRemovedMembersHistoryAsItsOwn]
// the SAME spelling on the road those records really take -- `mine := walk.own[SenderHandle]` in
// [Group.noteEpochGapLocked] -- dies too, which is the half the theorem above could never reach.
//
// AND ONE MORE MUTANT SURVIVES, WHICH IS ITSELF A FINDING AND IS RECORDED HERE RATHER THAN IN A
// REPORT. A review of the previous commit asked [Group.pastHeadLocked] to SKIP the rows of a
// previous occupant of a leaf. Applied in full -- both of its loops -- that mutant leaves every
// case of this package GREEN, and the probe says why: at the newcomer's epoch it takes
// pastHeadLocked from 1,024 to 0 while [Group.leafStreamFloorLocked] answers 1,024 from the same
// two tables with the class dropped out of the key, so the head the ladder is installed at does
// not move. WITHOUT that floor the same change is not inert: it is the difference between opening
// the newcomer's record and answering "index 1025 is 1025 ahead of head 0, and the window is
// 1024". The skip is refused for that reason and the reason is written at the function.
package urmessage

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/messagegroup"
)

// ── the world's remaining missing door ───────────────────────────────────────────────────────

// reuseWorld is one cohort at the moment a leaf changes hands: alice the founder and committer,
// bob at the leaf that is about to be taken away, carol the SURVIVOR that watches it happen, and
// eve the newcomer that lands on bob's leaf.
//
// EVERY MEMBER IS A REAL DEVICE over a real durable store with a real reserver -- [rotWorld] and
// [crossProcessDevice] -- so "the reserver starts at 1" and "the floor moved" are facts about a
// row on a disk and not about a field.
type reuseWorld struct {
	*rotWorld
	alice, bob, carol, eve *rotMember
	leaf                   uint32
	handle                 [16]byte
	bobRecords             []*sealed
	bobIds                 [][]byte
	aliceRecord            *sealed
	published              *rotation
}

// sealDurable seals one application record as `who` and gives it the next record id, exactly as
// [Group.Send] would below its own content codec: the class is DURABLE, the head is a clock
// reading, and the index comes off THIS DEVICE'S DURABLE RESERVER. The index is what this file is
// about, so it is never chosen here.
func (self *rotWorld) sealDurable(who *rotMember, body string) *sealed {
	self.t.Helper()
	record, err := who.group.session.SealRecord(message.RetentionDurable, 0, false,
		encodeHead(time.Now().UnixMilli()), []byte{byte(KindText), 'x'}, 0, nil)
	if err != nil {
		self.t.Fatalf("%s sealing %q: %v", who.name, body, err)
	}
	id, err := who.group.session.MessageIdOf(&record.Header)
	if err != nil {
		self.t.Fatalf("%s's message_id for %q: %v", who.name, body, err)
	}
	one := self.number(record)
	one.messageId = id[:]
	return one
}

// newReuseWorld builds the cohort and takes the leaf away, with the DEFECT ITSELF asserted as the
// precondition rather than assumed: the newcomer's handle must be byte-identical to the removed
// member's, or every case below is measuring an ordinary add.
//
// THE REMOVAL AND THE ADD ARE ONE COMMIT, which is the shape RFC 9420 §7.7 decides -- removes are
// applied before adds, so the Add takes the leaf the Remove has just blanked. It is built by
// [rotWorld.bundleAddAndRemove] (by REFERENCE, because the seam's by-value arms carry one kind of
// proposal each) and published as an HONEST rotation by [rotWorld.rotate], so the commit carries a
// real epoch digest over a real fresh pq_secret and ruling 41's refusal is not what this file
// measures.
func newReuseWorld(t *testing.T, bobLines int) *reuseWorld {
	t.Helper()
	return newReuseWorldAbove(t, bobLines, 0)
}

// newReuseWorldAbove is [newReuseWorld] with the leaving member's own stream floor moved first, so
// that a cohort can put its history AT A CHOSEN INDEX rather than at 1, 2, 3.
//
// IT MOVES THE FLOOR THROUGH THE PRODUCTION DOOR, [StreamIndexSeeder], which is the same door
// [Group.seedOwnStreamLocked] uses -- a reserver row on a real disk, not a header written by hand.
// A world built at floor 1,023 gives bob one line at stream index 1,024 instead of 1,024 lines,
// which is the whole of why the window case below costs one seal rather than a thousand.
func newReuseWorldAbove(t *testing.T, bobLines int, bobFloor uint64) *reuseWorld {
	t.Helper()
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

	if bobFloor != 0 {
		handle, err := bob.group.session.SenderHandle()
		if err != nil {
			t.Fatalf("bob's sender handle: %v", err)
		}
		key := messagegroup.StreamKey{SenderHandle: handle}
		copy(key.GroupId[:], world.groupId)
		seeder, canSeed := bob.dev.reserver.(StreamIndexSeeder)
		if !canSeed {
			t.Fatalf("bob's reserver cannot seed, so this world cannot place his history")
		}
		if _, err := seeder.SeedTo(key, bobFloor); err != nil {
			t.Fatalf("seeding bob's own stream floor to %d: %v", bobFloor, err)
		}
	}

	// (1) BOB WRITES, AT EPOCH 1, UNDER THE HANDLE THAT IS ABOUT TO CHANGE HANDS. These are the
	// records every later clause is about: the newcomer's stream collides with their indices, the
	// survivor's ladder stands at their head, and their handle stops resolving the moment bob's
	// leaf leaves the membership.
	records := []*sealed{}
	ids := [][]byte{}
	for at := 0; at < bobLines; at += 1 {
		one := world.sealDurable(bob, fmt.Sprintf("bob's line %d", at+1))
		records = append(records, one)
		ids = append(ids, one.messageId)
	}
	for at, one := range records {
		if one.record.Header.StreamIndex != bobFloor+uint64(at+1) {
			t.Fatalf("bob's line %d is at stream index %d, want %d: this file's whole subject is "+
				"which indices that stream has spent", at+1, one.record.Header.StreamIndex,
				bobFloor+uint64(at+1))
		}
	}
	// AND ALICE WRITES ONE LINE TOO, which is the fixture for the prune's own control rather
	// than decoration: [Group.peerHeads] holds a row only for a ladder that has AUTHENTICATED a
	// record, so a cohort in which only bob ever wrote gives the prune nothing it could wrongly
	// take away and "alice's row survived" would pass over an absence.
	aliceLine := world.sealDurable(alice, "alice's line, the ladder the prune must not take")
	// carol reads them, which is what puts her receiver ladders for both leaves at their heads.
	if err := world.deliver(carol, append(append([]*sealed{}, records...), aliceLine)...); err != nil {
		t.Fatalf("carol's walk over bob's and alice's lines: %v", err)
	}

	// (2) ONE COMMIT: BOB OUT, EVE IN.
	arm, joiner := world.bundleAddAndRemove(alice, bob.leaf, "eve", carol)
	var welcome, ratchetTree []byte
	published := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		commit, admission, tree, err := arm()
		welcome, ratchetTree = admission, tree
		return commit, admission, tree, err
	})
	if err := world.deliver(carol, published.page()...); err != nil {
		t.Fatalf("carol's walk over the commit that removes bob and adds eve: %v", err)
	}
	handle, err := joiner.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		t.Fatalf("eve's JoinFromWelcome: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	eve := world.enrollAt("eve", joiner, handle, published.pqSecret)

	// (3) THE DEFECT, REPRODUCED, AS THIS FILE'S PRECONDITION. Two different members, one leaf,
	// one sender_handle.
	if eve.leaf != bob.leaf {
		t.Fatalf("eve landed at leaf %d and bob stood at leaf %d. RFC 9420 §7.7 refills the "+
			"leftmost blank, so this build's tree is not putting the newcomer where item 245 "+
			"measured it and every case in this file is measuring an ordinary add", eve.leaf, bob.leaf)
	}
	bobHandle, err := bob.group.session.SenderHandle()
	if err != nil {
		t.Fatalf("bob's sender handle: %v", err)
	}
	eveHandle, err := eve.group.session.SenderHandle()
	if err != nil {
		t.Fatalf("eve's sender handle: %v", err)
	}
	if bobHandle != eveHandle {
		t.Fatalf("eve's sender_handle %x is not bob's %x. Item 245's whole subject is that the "+
			"derivation takes no epoch and no identity, so if these differ the defect is gone and "+
			"this file is measuring nothing", eveHandle, bobHandle)
	}
	// AND THE CONTROL IN THE SAME QUERY, so that "two handles are equal" cannot be satisfied by a
	// build in which every handle is equal: carol stands at a different leaf and derives different
	// octets.
	carolHandle, err := carol.group.session.SenderHandle()
	if err != nil {
		t.Fatalf("carol's sender handle: %v", err)
	}
	if carolHandle == eveHandle {
		t.Fatalf("CONTROL FAILED: carol's sender_handle equals eve's, so this build derives one " +
			"handle for every leaf and the equality above says nothing about a reused leaf")
	}
	t.Logf("the defect: bob stood at leaf %d under sender_handle %x, eve was added onto leaf %d "+
		"and derives %x -- byte identical, with carol at leaf %d on %x as the control",
		bob.leaf, bobHandle, eve.leaf, eveHandle, carol.leaf, carolHandle)

	return &reuseWorld{
		rotWorld: world, alice: alice, bob: bob, carol: carol, eve: eve,
		leaf: bob.leaf, handle: eveHandle,
		bobRecords: records, bobIds: ids, aliceRecord: aliceLine, published: published,
	}
}

// restart closes one member's process and opens a second over the same directories, and hands back
// the member the restore produced. It is [restoredRotDevice] plus [Device.restoreOne], which is the
// door a real restart goes through.
//
// IT REPLACES A FIXTURE THAT CLEARED THREE MAPS, AND THE DIFFERENCE IS EXACTLY WHAT THIS FILE
// MEASURES. Three cases here used to model a restart by emptying `log`, `logIndex` and `delivered`
// on the live group -- which leaves [Group.peerHeads], [Group.peerHeadsAt] and [Group.tracked]
// POPULATED, and a restart empties all three because they are fields of a process that has ended.
// The only tables a restarted device really has are [Group.persistedHeads], read back off the disk,
// and what [Device.restoreOne] rebuilds from the group record. A fake restart that keeps a
// process's in-memory heads is a fake restart that cannot see a head the disk would not have had,
// and both directions of the ladder repair below live in that gap.
func (self *rotWorld) restart(member *rotMember) *rotMember {
	self.t.Helper()
	revived := restoredRotDevice(self.t, member)
	records, err := revived.store.GroupRecords()
	if err != nil {
		self.t.Fatalf("%s's GroupRecords after the restart: %v", member.name, err)
	}
	if len(records) != 1 {
		self.t.Fatalf("%s's disk holds %d group record(s) after the restart, want 1",
			member.name, len(records))
	}
	restored, err := revived.device.restoreOne(revived.store, records[0], restoreTestNonce(), 1)
	if err != nil {
		self.t.Fatalf("restoring %s: %v", member.name, err)
	}
	self.t.Cleanup(func() { restored.Close() })
	return &rotMember{
		name:    member.name + " (restarted)",
		root:    member.root,
		dev:     member.dev,
		handle:  restored.handle,
		session: restored.session,
		group:   restored,
		leaf:    member.leaf,
	}
}

// ownStreamKey is the durable reserver's row for one member's own stream in this group: exactly
// the key [Group.ownHighWaterLocked] and [Group.seedOwnStreamLocked] build.
func (self *reuseWorld) ownStreamKey(handle [16]byte) messagegroup.StreamKey {
	key := messagegroup.StreamKey{SenderHandle: handle}
	copy(key.GroupId[:], self.groupId)
	return key
}

func (self *reuseWorld) highWater(who *rotMember) uint64 {
	self.t.Helper()
	high, err := who.dev.reserver.HighWater(self.ownStreamKey(self.handle))
	if err != nil {
		self.t.Fatalf("%s's own stream high water: %v", who.name, err)
	}
	return high
}

// ── 1. THE NEWCOMER CAN SEND, AND THE message_id COLLISION IS CLOSED BY THE SAME EDIT ────────

// A NEWCOMER ON A REUSED LEAF CAN SEND, AND THE PRE-FIX BRICK IS THE INLINE CONTROL.
//
// THE BRICK, DRIVEN AND NOT DESCRIBED. Before the walk, eve's reserver is a row that has never
// been allocated for -- the durable store answers contract clause 4's error-free zero -- so eve's
// first seal takes index 1. That record is sealed here, and TWO things about it are measured:
// its stream index is 1, an index bob's first line already spent; and its message_id, computed by
// [messagegroup.GroupSession.MessageIdOf], is BYTE-IDENTICAL to bob's first line's. On a server
// that is REASON_STREAM_INDEX_REUSED -- the claim map is keyed on (group_id, sender_handle,
// stream_index) with no epoch -- which [Group.cloneRefusalLocked] latches as [ErrIdentityInUse]
// for the life of the process. A member that has just joined can never send in the group it just
// joined.
//
// THE REPAIR. One walk over the page that is already on the server, and the floor has moved past
// every index claimed under those octets. The next seal is at 4 -- and 4 rather than 2 is exactly
// the seed's effect, because eve has already spent 1 herself.
//
// THE CLAIM VERIFIED RATHER THAN REPEATED, which is what this case was sent for. Item 245 says the
// seed closes the message_id collision "for free" and the no-wire-change ruling rests on it. Both
// directions are measured from production's own door: equal index under equal (group, handle)
// gives ONE id (the brick above), and disjoint indices give ids disjoint from bob's whole set.
// THE PREIMAGE IS UNCHANGED and that is held too -- the id is recomputed with the free function
// [messagegroup.MessageId] over (group_handle_key, group_id, sender_handle, stream_index) and must
// equal the session's answer, so "disjoint" is a property of the INDEX RANGE and not of some field
// this repair quietly added.
//
// WHAT WOULD GO RED: delete the seed in [Group.seedOwnStreamLocked]; gate it on
// [Group.walkReconcilesLocked] (a joined group is `reconciled` by construction, so the seed would
// never run for the one device that needs it); take the floor from records that OPENED rather than
// from the headers of records under this handle (eve can open none of bob's -- they are at an
// epoch she holds no state for).
func TestANewcomerOnAReusedLeafCanSendAndItsMessageIdsAreDisjoint(t *testing.T) {
	world := newReuseWorld(t, 3)
	eve := world.eve

	// ── THE BRICK, AS THE CONTROL, BEFORE ANY WALK ──────────────────────────────────────────
	if high := world.highWater(eve); high != 0 {
		t.Fatalf("CONTROL FAILED: eve's reserver already stands at %d before any walk, so an "+
			"index above bob's below would not be evidence that anything seeded it", high)
	}
	brick := world.sealDurable(eve, "eve's first line, on a fresh reserver")
	if brick.record.Header.StreamIndex != 1 {
		t.Fatalf("CONTROL FAILED: eve's first seal took index %d and a fresh reserver hands out 1; "+
			"the collision below is about index 1", brick.record.Header.StreamIndex)
	}
	if !bytes.Equal(brick.messageId, world.bobIds[0]) {
		t.Fatalf("CONTROL FAILED: eve's record at index 1 has message_id %x and bob's has %x. "+
			"They must be EQUAL: MASTER §8.4.5 expands an id from (group_id, sender_handle, "+
			"stream_index) and the first two are equal by construction here, so a difference "+
			"means the id takes an input item 245's ruling does not know about and the "+
			"no-wire-change premise has to be re-examined", brick.messageId, world.bobIds[0])
	}
	t.Logf("THE BRICK, DRIVEN: eve's first record and bob's first record are both at "+
		"(group %x, sender_handle %x, stream_index 1) and carry ONE message_id %x. On the server "+
		"that is REASON_STREAM_INDEX_REUSED and ErrIdentityInUse for the life of the process",
		world.groupId[:4], world.handle, brick.messageId)

	// ── THE WALK, AND THE FLOOR IT MOVES ────────────────────────────────────────────────────
	page := append([]*sealed{}, world.bobRecords...)
	page = append(page, world.published.page()...)
	if err := world.deliver(eve, page...); err != nil {
		t.Fatalf("eve's first walk answered %v. It must not: bob's lines are at an epoch eve "+
			"holds no state for, which is a GAP and not a failure, and the commit is one eve has "+
			"already applied through its Welcome", err)
	}
	if high := world.highWater(eve); high != 3 {
		t.Fatalf("eve's own stream floor stands at %d after the walk, want 3 -- the highest index "+
			"the page claimed under her own sender_handle. A floor below that is a device whose "+
			"next send collides with a record the server already holds", high)
	}
	if seeded := eve.group.Stats().StreamFloorSeeded; seeded != 1 {
		t.Fatalf("Stats.StreamFloorSeeded is %d, want 1: the one number that says a leaf changed "+
			"hands under this device", seeded)
	}

	// ── THE PROPERTY: THE NEXT SEAL IS ABOVE EVERYTHING BOB SPENT ───────────────────────────
	next := world.sealDurable(eve, "eve's line after the walk")
	if next.record.Header.StreamIndex != 4 {
		t.Fatalf("eve's next seal took index %d, want 4. Three is what bob spent and one is what "+
			"eve spent on the brick above, so 2 is the answer a device with NO seed gives and 4 "+
			"is the answer a seeded one gives -- the two are what this case tells apart",
			next.record.Header.StreamIndex)
	}
	for at, id := range world.bobIds {
		if bytes.Equal(next.messageId, id) {
			t.Fatalf("eve's record at index %d carries bob's line %d's message_id %x",
				next.record.Header.StreamIndex, at+1, id)
		}
	}
	if bytes.Equal(next.messageId, brick.messageId) {
		t.Fatalf("eve's two records carry one message_id")
	}

	// ── AND THE PREIMAGE IS UNCHANGED, which is the half of the claim a count could hide ────
	rebuilt := messagegroup.MessageId([]byte(world.groupHandleKey), [32]byte(world.groupId),
		world.handle, next.record.Header.StreamIndex)
	if !bytes.Equal(rebuilt[:], next.messageId) {
		t.Fatalf("the id the session answered (%x) is not HKDF-Expand(group_handle_key, "+
			"\"mid/v1\" ‖ LP(group_id) ‖ LP(sender_handle) ‖ u64(%d)) (%x). Item 245's ruling "+
			"rests on the id being a function of exactly those three inputs, so a difference here "+
			"means the preimage moved and the lead must be told",
			next.messageId, next.record.Header.StreamIndex, rebuilt)
	}
	t.Logf("ITEM 245's message_id CLAIM, VERIFIED IN BOTH DIRECTIONS: at an EQUAL index two " +
		"occupants of one leaf carry one id (the brick), and above the seed eve's indices {4} are " +
		"disjoint from bob's {1,2,3} so their ids are too -- with the id recomputed from the " +
		"published preimage to show nothing was added to it")
}

// ── 2. THE SURVIVOR DOES NOT RE-TRACK THE REUSED LEAF AT THE REMOVED MEMBER'S HEAD ───────────

// A SURVIVOR DROPS THE REMOVED LEAF'S RE-TRACK ROW AND KEEPS ITS HEAD, AND THE NEWCOMER'S FIRST
// RECORD OPENS.
//
// WHAT THE PRUNE IS FOR. [Group.peerHeads] is the table [Group.crossEpochLadderLocked] walks at
// every epoch change to RE-TRACK each peer's ratchet eagerly, and nothing pruned it by
// RemovedLeaves -- so a leaf whose occupant had been removed got a ladder installed at every
// future epoch, in a schedule nobody can write to. That is the row this case asserts is gone.
//
// AND WHAT THE PRUNE MUST NOT TAKE WITH IT, which is this case's correction to itself. It used to
// assert that the two PER-EPOCH tables were emptied too, on the argument that the newcomer's
// ladder is then "installed lazily at 0, which is the correct head for a stream that starts here".
// The newcomer's stream does not start here: a stream index is per (group_id, sender_handle), the
// server refuses every index the previous occupant spent, and [Group.seedOwnStreamLocked] is this
// device's own half of that fact -- so the newcomer's first ACCEPTED index is the previous
// occupant's high water plus one. A survivor at 0 is therefore too low by that whole history, and
// [TestASurvivorOpensANewcomerWhoseStreamStartsPastTheRatchetWindow] drives what that costs. The
// head is kept in [Group.peerHeadsAt] and read back by [Group.leafStreamFloorLocked].
//
// THE CONTROL FIRES FOR ITS OWN REASON: alice's ladder, in the same table, at the same moment, is
// KEPT. Without it "the removed leaf's head is gone" would be satisfied by a prune that emptied
// the whole table -- which is the D3 starvation [Group.peerHeads] exists to prevent, arriving as
// the repair for this one.
//
// AND THE NEWCOMER IS SEEDED BEFORE IT SEALS, which is the second correction. This case used to
// seal eve's first line on a fresh reserver, at stream index 1 -- the state
// [TestANewcomerOnAReusedLeafCanSendAndItsMessageIdsAreDisjoint] drives as THE BRICK and names as
// REASON_STREAM_INDEX_REUSED on the server. A record at that index cannot exist on the wire, so a
// case that opened one was measuring a state no survivor can meet.
//
// WHAT WOULD GO RED: delete the [Group.peerHeads] loop in [Group.pruneRemovedLaddersLocked]; call
// it AFTER [Group.crossEpochLadderLocked] instead of before it (the re-track has already installed
// the stale ladder by then); prune [Group.peerHeadsAt] as well (the floor loses the previous
// occupant's head and the newcomer's record is out of window as soon as that history passes
// [messagegroup.DefaultRecordWindowSize]).
func TestASurvivorDoesNotReTrackAReusedLeafAtTheRemovedMembersHead(t *testing.T) {
	world := newReuseWorld(t, 3)
	carol, eve := world.carol, world.eve

	wire, err := message.RetentionClassWire(message.RetentionDurable, 0)
	if err != nil {
		t.Fatalf("the durable retention wire byte: %v", err)
	}
	removed := ladderKey{leaf: world.leaf, retentionWire: wire, ephWindow: 0}
	kept := ladderKey{leaf: world.alice.leaf, retentionWire: wire, ephWindow: 0}

	// ── THE CONTROL FOR THE FIXTURE ITSELF: carol really did authenticate bob's head ────────
	// It is taken from a SECOND world, walked to exactly the same point but with the commit not
	// yet delivered, because the assertion below is that the head is GONE and a head that was
	// never there would satisfy it.
	before := newRotWorld(t, "alice", "bob", "carol")
	beforeBob, beforeCarol := before.member("bob"), before.member("carol")
	beforeRecords := []*sealed{}
	for at := 0; at < 3; at += 1 {
		beforeRecords = append(beforeRecords, before.sealDurable(beforeBob, fmt.Sprintf("line %d", at+1)))
	}
	if err := before.deliver(beforeCarol, beforeRecords...); err != nil {
		t.Fatalf("CONTROL FAILED: carol's walk over bob's lines answered %v", err)
	}
	beforeLadder := ladderKey{leaf: beforeBob.leaf, retentionWire: wire, ephWindow: 0}
	if head := beforeCarol.group.peerHeads[beforeLadder]; head != 3 {
		t.Fatalf("CONTROL FAILED: with no removal at all, carol's head for bob's ladder is %d and "+
			"want 3. If it is 0 here then nothing in this world ever tracked that ladder and the "+
			"prune below would pass over an empty table", head)
	}
	t.Logf("CONTROL: with no removal, carol's ladder for leaf %d stands at head 3", beforeBob.leaf)

	// ── THE PROPERTY: after the commit, the removed leaf's rows are gone and alice's are not ─
	if head, held := carol.group.peerHeads[removed]; held {
		t.Fatalf("carol still holds a peer head of %d for leaf %d after the commit that removed "+
			"its occupant. [Group.crossEpochLadderLocked] re-tracks every entry of this table at "+
			"the new epoch, so this row is a ladder installed at the REMOVED member's head for a "+
			"leaf the NEWCOMER now stands at", head, removed.leaf)
	}
	// AND THE PER-EPOCH HEAD IS STILL THERE, WHICH IS THE OTHER HALF OF THE PROPERTY AND NOT AN
	// OMISSION. It is the number [Group.leafStreamFloorLocked] answers the newcomer's ladder with,
	// and a build that pruned it would put that ladder at 0 -- see the case named in this test's
	// header for what that costs once the previous occupant's history is longer than the window.
	floor := uint64(0)
	for key, head := range carol.group.peerHeadsAt {
		if key.leaf == world.leaf && floor < head {
			floor = head
		}
	}
	if floor != 3 {
		t.Fatalf("carol's per-epoch head for the reused leaf %d is %d and want 3: the removed "+
			"member's high water is the floor the NEWCOMER's ladder stands at, because the stream "+
			"index space is per (group_id, sender_handle) and the server refuses every index that "+
			"member spent", world.leaf, floor)
	}
	// AND NO MEMO SURVIVES FOR IT EITHER -- which is [Group.crossEpochLadderLocked]'s wholesale
	// clear and NOT the prune's doing, measured: a fourth loop over [Group.tracked] inside
	// [Group.pruneRemovedLaddersLocked] was deleted because removing it turned nothing red. It is
	// asserted here anyway, because what this case owes is the STATE the newcomer meets and not a
	// list of which function produced it.
	for key := range carol.group.tracked {
		if key.leaf == world.leaf {
			t.Fatalf("carol still memos leaf %d as tracked at epoch %d, so [Group.trackLocked] "+
				"would install nothing for the newcomer at all", key.leaf, key.epoch)
		}
	}
	if _, held := carol.group.peerHeads[kept]; !held {
		t.Fatalf("CONTROL FAILED: carol's head for alice's ladder (leaf %d) went with the removed "+
			"leaf's. The prune must take the leaves the commit REMOVED and no others -- emptying "+
			"the table is the D3 starvation this table exists to prevent", kept.leaf)
	}

	// ── AND THE NEWCOMER'S FIRST RECORD OPENS AT THE SURVIVOR ───────────────────────────────
	// Driven end to end from the state the wire can produce: eve walks the page that is already
	// on the server, her floor moves past every index bob spent, and her first seal is at 4.
	if err := world.deliver(eve, append(append([]*sealed{}, world.bobRecords...),
		world.published.page()...)...); err != nil {
		t.Fatalf("eve's own first walk answered %v", err)
	}
	first := world.sealDurable(eve, "eve's first line to the survivor")
	if first.record.Header.StreamIndex != 4 {
		t.Fatalf("eve's first record is at index %d and want 4: three is what bob spent, and an "+
			"index at or below it is REASON_STREAM_INDEX_REUSED on the server, so a case that "+
			"opened one would be measuring a record that cannot exist",
			first.record.Header.StreamIndex)
	}
	opened, err := carol.group.receiveForTest(world.rotWorld, carol, first)
	if err != nil {
		t.Fatalf("carol's walk over the newcomer's first record answered %v. It is the first "+
			"record of a stream that CONTINUES bob's numbering, and the ladder it meets has to "+
			"stand at bob's head rather than at 0 or at anything above 4", err)
	}
	if len(opened) != 1 {
		t.Fatalf("carol opened %d message(s) from the newcomer's first record, want 1", len(opened))
	}
	if !bytes.Equal(opened[0].SenderIdentity, eve.dev.identityPub) {
		t.Fatalf("carol attributed the newcomer's record to identity %x, want eve's %x",
			opened[0].SenderIdentity, eve.dev.identityPub)
	}
	t.Logf("the reused leaf's re-track row is gone and its head of 3 is kept; the newcomer's " +
		"first record, at stream index 4 of a stream that continues bob's numbering, opened at " +
		"the survivor and was attributed to eve")
}

// receiveForTest is [rotWorld.deliver] with the opened messages handed back, which `deliver`
// answers only an error for. It is a method on [Group] so that a case can read what a walk
// DELIVERED and not only whether it refused.
func (self *Group) receiveForTest(world *rotWorld, who *rotMember, page ...*sealed) ([]*Message, error) {
	world.t.Helper()
	at := len(self.log)
	err := world.deliver(who, page...)
	delivered := []*Message{}
	for _, one := range self.log[min(at, len(self.log)):] {
		delivered = append(delivered, one)
	}
	return delivered, err
}

// ── 3. A REMOVED LEAF'S RECORD STILL RESOLVES, AND IS ATTRIBUTED TO THE REMOVED MEMBER ───────

// A RECORD FROM A REMOVED LEAF, AT ITS OWN EPOCH, MET BY A SURVIVOR STANDING AT THE EPOCH ABOVE.
//
// IT IS THE ORDINARY FIRST DAY OF REMOVE AND NOT A CORNER. The commit that removes a member is the
// LAST record of that member's history, so its whole conversation sits below it in record order --
// and the cursor is not persisted, so every restart re-walks all of it. With the handle table
// built at the CURRENT epoch the removed leaf is simply not in it: every one of those records
// answered "which is no leaf of this group", was retried [maxRecordAttempts] times and abandoned.
//
// THE TWO HALVES, AND THE SECOND IS THE ONE THE HANDLE CANNOT DO. It RESOLVES (the per-record-epoch
// table), and it is attributed to BOB (the signed leaf). The newcomer's record under the SAME
// sixteen octets is attributed to EVE, in the same case and at the same survivor, which is the
// control: two occupants, one handle, two identities. A build that attributed by sender_handle
// answers one value for both and cannot tell them apart at all -- and this case asserts the handles
// ARE equal, so that the identities differing is a statement about the repair and not about the
// fixture.
//
// AND IT DRIVES THE RESTART, THROUGH THE DOOR A RESTART REALLY GOES THROUGH: the survivor's
// process ENDS and a second one opens over the same directories ([reuseWorld.restart]), so the
// removed member's records are re-opened by a device ALREADY STANDING at the epoch above and
// holding only what the disk kept. This used to be three maps emptied on the live group, which
// leaves every in-memory head table populated -- and both directions of the ladder repair live in
// exactly that gap.
//
// WHAT WOULD GO RED: put [Group.leavesAtLocked] back to the current epoch (the record is abandoned);
// take [Message.SenderIdentity] off the header's sender_handle instead of the signed leaf (bob's
// line and eve's line come back under one identity); make [Group.pastHeadLocked] skip the rows of
// a previous occupant of the leaf (the newcomer's line below is then met by a ladder at 0).
func TestARecordFromARemovedLeafResolvesAtItsOwnEpochAndIsAttributedToTheRemovedMember(t *testing.T) {
	world := newReuseWorld(t, 3)
	carol, eve := world.carol, world.eve

	// carol stands at the epoch the commit opened; bob's records are at the one below it.
	if carol.group.epoch != world.published.opens {
		t.Fatalf("carol stands at epoch %d and the commit opened %d", carol.group.epoch, world.published.opens)
	}
	for _, one := range world.bobRecords {
		if one.record.Header.Epoch != world.published.opens-1 {
			t.Fatalf("bob's record is at epoch %d and the commit opened %d; this case is about a "+
				"record sealed one epoch BELOW the removal", one.record.Header.Epoch, world.published.opens)
		}
	}

	// THE RESTART. The cursor is not persisted, so the second process re-walks the whole history.
	carol = world.restart(carol)
	if carol.group.epoch != world.published.opens {
		t.Fatalf("the restarted survivor came back at epoch %d and the commit opened %d",
			carol.group.epoch, world.published.opens)
	}

	delivered, err := carol.group.receiveForTest(world.rotWorld, carol, world.bobRecords...)
	if err != nil {
		t.Fatalf("carol's walk over the removed member's own records answered %v. They are at an "+
			"epoch she holds state for and under a handle her table must still resolve; a refusal "+
			"here is the abandonment item 245's third piece exists to stop", err)
	}
	if len(delivered) != len(world.bobRecords) {
		t.Fatalf("carol delivered %d of the removed member's %d records", len(delivered), len(world.bobRecords))
	}
	for at, one := range delivered {
		if one.Gap != "" {
			t.Fatalf("the removed member's line %d came back as a %q gap rather than as a message", at+1, one.Gap)
		}
		if one.Mine {
			t.Fatalf("carol reads the removed member's line %d as her own", at+1)
		}
		if !bytes.Equal(one.SenderIdentity, world.bob.dev.identityPub) {
			t.Fatalf("the removed member's line %d is attributed to identity %x, want bob's %x",
				at+1, one.SenderIdentity, world.bob.dev.identityPub)
		}
	}

	// AND THE RE-WALK LEFT THE REUSED LEAF'S HEAD AT THE PREVIOUS OCCUPANT'S HIGH WATER, WHICH IS
	// WHERE THE NEWCOMER'S LADDER HAS TO STAND. Three records of the previous occupant just opened
	// at their own epoch; the index space is per (group_id, sender_handle) and the server refuses
	// every index that occupant spent, so the newcomer's stream CONTINUES this numbering and a
	// head of 0 would be three rungs -- and, on a longer history, a whole window -- too low.
	wire, err := message.RetentionClassWire(message.RetentionDurable, 0)
	if err != nil {
		t.Fatalf("the durable retention wire byte: %v", err)
	}
	reused := ladderKey{leaf: world.leaf, retentionWire: wire, ephWindow: 0}
	if head := carol.group.peerHeads[reused]; head != uint64(len(delivered)) {
		t.Fatalf("after re-opening the removed member's %d records, carol's CURRENT head for leaf "+
			"%d stands at %d and want %d", len(delivered), reused.leaf, head, len(delivered))
	}
	if floor := carol.group.leafStreamFloorLocked(world.leaf, world.published.opens); floor != uint64(len(delivered)) {
		t.Fatalf("the floor a ladder over leaf %d is installed at, at epoch %d, is %d and want %d",
			world.leaf, world.published.opens, floor, len(delivered))
	}
	// AND THE CONTROL IN THE SAME QUERY: the PER-EPOCH head for that leaf at the epoch the
	// records were sealed at DID rise. Without it "the floor is 3" would be satisfiable by a
	// current head alone, and it is the per-epoch table the floor and the next restart read.
	rose := false
	for key, head := range carol.group.peerHeadsAt {
		if key.leaf == world.leaf && key.epoch == world.published.opens-1 && head == uint64(len(delivered)) {
			rose = true
		}
	}
	if !rose {
		t.Fatalf("CONTROL FAILED: carol recorded no per-epoch head of %d for leaf %d at epoch %d. "+
			"That is the table [Group.leafStreamFloorLocked] reads and the one the disk gets, so a "+
			"build that recorded no head there would starve the newcomer at the next restart",
			len(delivered), world.leaf, world.published.opens-1)
	}

	// ── THE CONTROL, AT THE SAME SURVIVOR AND UNDER THE SAME SIXTEEN OCTETS ─────────────────
	// eve's floor is seeded off the same page first, so her line is at an index the server would
	// accept rather than at one bob already spent.
	if err := world.deliver(eve, append(append([]*sealed{}, world.bobRecords...),
		world.published.page()...)...); err != nil {
		t.Fatalf("eve's own first walk answered %v", err)
	}
	eveLine := world.sealDurable(eve, "eve's line, same handle, different person")
	if eveLine.record.Header.SenderHandle != world.bobRecords[0].record.Header.SenderHandle {
		t.Fatalf("CONTROL FAILED: eve's record and bob's carry different sender_handles, so " +
			"attributing them to two identities says nothing about a reused leaf")
	}
	fromEve, err := carol.group.receiveForTest(world.rotWorld, carol, eveLine)
	if err != nil {
		t.Fatalf("carol's walk over the newcomer's line answered %v", err)
	}
	if len(fromEve) != 1 {
		t.Fatalf("carol delivered %d message(s) for the newcomer's line, want 1", len(fromEve))
	}
	if !bytes.Equal(fromEve[0].SenderIdentity, eve.dev.identityPub) {
		t.Fatalf("the newcomer's line is attributed to identity %x, want eve's %x",
			fromEve[0].SenderIdentity, eve.dev.identityPub)
	}
	if bytes.Equal(world.bob.dev.identityPub, eve.dev.identityPub) {
		t.Fatalf("CONTROL FAILED: bob and eve are one identity, so the two assertions above are " +
			"one assertion")
	}
	t.Logf("ONE sender_handle %x, TWO members: %d record(s) attributed to bob %x and 1 to eve %x, "+
		"at one survivor, out of one table. The handle cannot tell them apart and the signed leaf can",
		world.handle, len(delivered), world.bob.dev.identityPub[:4], eve.dev.identityPub[:4])
}

// ── 4. A STORE WRITTEN BEFORE PART NINE STILL STARTS ─────────────────────────────────────────

// A DEVICE BUILT ON AN OLD STORE KEEPS WORKING, AND WHAT IT LOSES IS MEASURED RATHER THAN CLAIMED.
//
// WHY THIS IS THE CASE THAT DECIDES THE SHAPE OF PART NINE. A restore that REFUSED a record written
// by an older build is a device that can never start again -- the deployed alpha's disk is FIVE
// parts, the build before this one wrote EIGHT, and none of them carries a leaf ledger. So the
// reader takes both arities and [Device.restoreOne] seeds the handle set with the one leaf the tree
// says, which is exactly what every build before this one held.
//
// THE FIXTURE IS A REAL EIGHT-PART RECORD, written through the store's own framing with part nine
// dropped, and not a nine-part record with an empty ledger: those two decode to NIL and to an EMPTY
// SLICE and the distinction is the one [GroupRecord.Leaves] is read by.
//
// WHAT IS ASSERTED, and the second clause is the honest half: the device STARTS, opens its backlog
// and recognises its own handle; and the departed table comes back EMPTY, which is the loss named
// at [GroupRecord.Leaves] -- such a device resolves a departed leaf's records only while the leaf
// has been refilled.
//
// WHAT WOULD GO RED: make [groupRecordOf] refuse an eight-part record; stop seeding the handle set
// in [Group.initTables] (the restored device would recognise none of its own records); invent a
// departed table out of the current membership at the read (this case's second clause).
func TestADeviceRestoredFromAStoreWithNoLeafLedgerStillStarts(t *testing.T) {
	world := newRotWorld(t, "alice", "bob")
	alice := world.member("alice")
	line := world.sealDurable(alice, "a line written before the restart")

	// ── THE DISK, REWRITTEN AS A BUILD BEFORE PART NINE WROTE IT ────────────────────────────
	store := alice.dev.store
	path := store.groupRecordPath(alice.group.id)
	parts, err := store.readRecord(path, stateKindGroupRecord)
	if err != nil {
		t.Fatalf("reading the group record back: %v", err)
	}
	// THE ARITY IS READ AND NOT ASSUMED, and this case's fixture is the first EIGHT parts however
	// many this build writes: part ten (ruling 52's removal) landed after this case was written, and
	// an equality against the total would have made every later part a red suite here rather than at
	// the arity switch that owns the question.
	if len(parts) < 9 {
		t.Fatalf("this build wrote %d parts and this case strips everything from the ninth up; if "+
			"the arity has fallen below nine, the fixture is no longer an old store", len(parts))
	}
	if len(parts[8]) == 0 {
		t.Fatalf("CONTROL FAILED: the ninth part this build wrote is EMPTY, so stripping it " +
			"changes nothing and this case would pass against a build that never wrote one")
	}
	if err := store.writeRecord(path, stateKindGroupRecord, parts[:8]...); err != nil {
		t.Fatalf("rewriting the group record with eight parts: %v", err)
	}
	// AND THE FIXTURE IS CHECKED THROUGH THE READER ITSELF, so "an eight-part record" is what is
	// on the disk and not what this case meant to put there.
	records, err := store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords over the eight-part disk: %v", err)
	}
	if len(records) != 1 || records[0].Leaves != nil {
		t.Fatalf("the eight-part record decoded with %d leaf ledger row(s); a record written "+
			"before part nine must come back with NIL, which is the signal [Device.restoreOne] "+
			"acts on", len(records[0].Leaves))
	}

	// ── THE RESTORE ─────────────────────────────────────────────────────────────────────────
	revived := restoredRotDevice(t, alice)
	group, err := revived.device.restoreOne(revived.store, records[0], restoreTestNonce(), 1)
	if err != nil {
		t.Fatalf("a device whose store predates part nine did not start: %v. A restore that "+
			"refuses an older record is a device that can never start again, which is the one "+
			"outcome no compatibility question may reach", err)
	}
	defer group.Close()

	// it recognises its own handle -- the set seeded from the one leaf the tree says
	own, err := group.session.SenderHandle()
	if err != nil {
		t.Fatalf("the restored group's sender handle: %v", err)
	}
	if !group.ownHandles[own] {
		t.Fatalf("the restored group does not hold its own sender_handle %x in its handle set, "+
			"so every record it ever wrote comes back as a stranger's", own)
	}
	if len(group.ownHandles) != 1 {
		t.Fatalf("the restored group holds %d handle(s) and an old store can say exactly one",
			len(group.ownHandles))
	}
	// AND THE LOSS, ASSERTED AND NOT ONLY WRITTEN DOWN.
	if len(group.departedAt) != 0 {
		t.Fatalf("the restored group came back with %d departed leaf/leaves out of a record that "+
			"carries none; inventing one is this build deciding, from the current membership, a "+
			"question the disk does not answer", len(group.departedAt))
	}

	// AND IT STILL RECOGNISES THE RECORD IT WROTE BEFORE THE RESTART AS ITS OWN, which is the
	// clause that makes "it starts" mean something: the handle set is what both graceful
	// own-record roads are gated on, so a restored device whose set were EMPTY would come back
	// showing its own half of the conversation as a stranger's -- with no error anywhere.
	if line.record.Header.SenderHandle != own {
		t.Fatalf("the line written before the restart carries sender_handle %x and the restored "+
			"device derives %x", line.record.Header.SenderHandle, own)
	}
	if !group.ownHandles[line.record.Header.SenderHandle] {
		t.Fatalf("the restored device does not read the record it wrote before the restart as its own")
	}
	t.Logf("a device whose disk carries EIGHT parts restored, recognises its one handle %x, and "+
		"comes back with an EMPTY departed table -- which is the state every build before this "+
		"one was in", own)
}

// ── 5. A REMOVAL WITH NO REFILL, WHICH IS THE ONE THE DEPARTED TABLE IS THE ONLY ANSWER FOR ──

// THE REMOVED LEAF STAYS BLANK, AND THE SURVIVOR STILL RESOLVES THE RECORDS IT SEALED.
//
// WHY THIS CASE EXISTS AND IT IS A MUTANT'S DOING. The reuse cohort above removes and refills a
// leaf in ONE commit, so the leaf is back in the CURRENT membership and
// SenderHandle(group_handle_key, leaf) -- which takes the leaf alone -- resolves the removed
// member's records through the NEWCOMER's row. Measured: disabling the departed-leaf half of
// [Group.leavesAtLocked] entirely (`if false && epoch < departed`) left every case above GREEN.
// The table's whole subject is a leaf that is GONE, and a refilled leaf is not gone.
//
// SO THIS IS THE ORDINARY FIRST DAY OF REMOVE, in the ledger's own words: one member removed,
// nobody added, and every record that member ever sealed sitting BELOW the commit in record order
// -- which every restart re-walks, because the cursor is not persisted. With the handle table
// built at the current epoch those records answer "which is no leaf of this group", are retried
// [maxRecordAttempts] times and are ABANDONED: the removed member's half of the conversation
// disappears from every survivor, permanently, with [ErrRecordAbandoned] as the only sign.
//
// THE CONTROL FIRES FOR ITS OWN REASON: the leaf really is out of the CURRENT membership. It is
// checked by deriving the handle table at the epoch the commit opened and finding the removed
// member's handle absent from it, which is the state the old code handed to every record.
//
// WHAT WOULD GO RED: delete the departed-leaf half of [Group.leavesAtLocked]; file the departure
// epoch at the epoch the commit CLOSED instead of the one it opened (the removed member's records
// at the closing epoch would stop resolving, which is most of them).
func TestARemovedLeafThatIsNeverRefilledStillResolvesItsOwnRecords(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

	lines := []*sealed{}
	for at := 0; at < 3; at += 1 {
		lines = append(lines, world.sealDurable(bob, fmt.Sprintf("bob's line %d", at+1)))
	}
	if err := world.deliver(carol, lines...); err != nil {
		t.Fatalf("carol's walk over bob's lines: %v", err)
	}
	published := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove([]uint32{bob.leaf})
	})
	if err := world.deliver(carol, published.page()...); err != nil {
		t.Fatalf("carol's walk over the commit that removes bob: %v", err)
	}

	// ── THE CONTROL: the leaf really is out of the current membership ───────────────────────
	bobHandle, err := bob.group.session.SenderHandle()
	if err != nil {
		t.Fatalf("bob's sender handle: %v", err)
	}
	carol.group.mutex.Lock()
	current := map[[16]byte]uint32{}
	for at := 0; at < carol.group.handle.MemberCount(); at += 1 {
		leaf, _, _, memberErr := carol.group.handle.MemberAt(at)
		if memberErr != nil {
			carol.group.mutex.Unlock()
			t.Fatalf("carol's member %d: %v", at, memberErr)
		}
		current[messagegroup.SenderHandle(carol.group.groupHandleKey, leaf)] = leaf
	}
	carol.group.mutex.Unlock()
	if _, standing := current[bobHandle]; standing {
		t.Fatalf("CONTROL FAILED: bob's sender_handle %x is still in carol's CURRENT membership "+
			"after the commit that removed him, so the table built at the current epoch would "+
			"resolve his records anyway and this case measures nothing", bobHandle)
	}
	t.Logf("CONTROL: bob's sender_handle %x is not in the membership at epoch %d (%d member(s)), "+
		"which is the table every record used to be resolved through",
		bobHandle, carol.group.epoch, len(current))

	// ── THE PROPERTY: the re-walk still resolves them, opens them, and names bob ────────────
	// THE RESTART GOES THROUGH THE DOOR A RESTART GOES THROUGH. Emptying three maps on the live
	// group leaves every in-memory head table standing, which is not what a second process holds.
	carol = world.restart(carol)

	delivered, err := carol.group.receiveForTest(world, carol, lines...)
	if err != nil {
		t.Fatalf("carol's re-walk over the removed member's records answered %v. That is the "+
			"abandonment ledger item 245's third piece exists to stop: the removed member's whole "+
			"half of the conversation, gone from every survivor at the first restart", err)
	}
	if len(delivered) != len(lines) {
		t.Fatalf("carol delivered %d of the removed member's %d records", len(delivered), len(lines))
	}
	for at, one := range delivered {
		if one.Gap != "" {
			t.Fatalf("the removed member's line %d came back as a %q gap", at+1, one.Gap)
		}
		if !bytes.Equal(one.SenderIdentity, bob.dev.identityPub) {
			t.Fatalf("the removed member's line %d is attributed to %x, want bob's %x",
				at+1, one.SenderIdentity, bob.dev.identityPub)
		}
	}
	if len(carol.group.unopened) != 0 {
		t.Fatalf("carol gave up on %d record(s) of the removed member's", len(carol.group.unopened))
	}
	t.Logf("a leaf removed at epoch %d and never refilled: all %d records it sealed at epoch %d "+
		"still resolve at the survivor and are attributed to the member that wrote them",
		published.opens, len(delivered), published.opens-1)
}

// ── 6. THE SEED MUST NOT LAUNDER THE CLONE CHECK'S OWN EVIDENCE ──────────────────────────────

// A RESTORED GROUP DOES NOT MOVE ITS OWN FLOOR UNTIL THE CLONE CHECK HAS CONCLUDED.
//
// WHY THIS CASE EXISTS AND IT IS A REGRESSION THIS PASS CAUSED AND THEN CLOSED. The seed and the
// clone check read ONE fact -- "an index on the server under my own sender_handle that my reserver
// never allocated" -- and draw opposite conclusions from it: the check says ANOTHER COPY OF THIS
// FOLDER, the seed says A PREVIOUS OCCUPANT OF THIS LEAF. The handle cannot tell them apart; that
// is item 245's linkability residual seen from the inside. What tells them apart is the AEAD: a
// clone's record was sealed by this device's own leaf key at an epoch this device stands in, so it
// authenticates and raises [Group.ownIndexSeen]; a previous occupant's is below this device's
// admission and raises nothing. The check compares the reserver against that authenticated number,
// so a seed taken BEFORE the comparison raises the high water past the evidence.
//
// MEASURED, on the first run of the full battery: without the [Group.reconciled] gate,
// cp3b.TestACopyWhoseEvidenceArrivedInADirtyWalkIsStillCaught went RED -- "the copy's clean Receive
// answered <nil>, want ErrIdentityInUse" -- because a copy's FIRST walk is dirty, the check does not
// run on a dirty walk, and an ungated seed fires on it anyway.
//
// THIS CASE IS THE MECHANISM WITHOUT THE SERVER, so the property is held in the package that owns
// the code and not only in the module that has a message server. The two halves are driven in one
// case, which is what makes the second half a control rather than a claim: on the DIRTY walk the
// floor must not move, and on the CLEAN walk that follows it must.
//
// WHAT WOULD GO RED: drop `!self.reconciled` from [Group.seedOwnStreamLocked] (the dirty walk
// seeds); move the seed ABOVE the reconciliation block in [Group.commitWalkLocked] (the clean walk
// seeds before the check reads the high water).
func TestARestoredGroupDoesNotSeedItsFloorOverADirtyWalk(t *testing.T) {
	world := newReuseWorld(t, 3)
	eve, carol := world.eve, world.carol

	// A LINE THE RESTORED DEVICE CAN OPEN, BENT, so the walk has a record that FAILS rather than
	// one that is a gap. Bob's lines are at an epoch eve holds no state for: those are
	// [GapOutOfWindow] and do not make a walk dirty.
	clean := world.sealDurable(carol, "carol's line at the current epoch")
	bent := &sealed{recordId: clean.recordId, messageId: clean.messageId, record: &message.Record{
		Header: clean.record.Header,
		CtHead: append([]byte(nil), clean.record.CtHead...),
		CtBody: append([]byte(nil), clean.record.CtBody...),
	}}
	bent.record.CtBody[0] ^= 0xFF

	// ── THE RESTORE, which is the ONLY way a group comes back NOT reconciled ────────────────
	revived := restoredRotDevice(t, eve)
	records, err := revived.store.GroupRecords()
	if err != nil {
		t.Fatalf("GroupRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("eve's disk holds %d group record(s), want 1", len(records))
	}
	restored, err := revived.device.restoreOne(revived.store, records[0], restoreTestNonce(), 1)
	if err != nil {
		t.Fatalf("restoring eve: %v", err)
	}
	defer restored.Close()
	if restored.Reconciled() {
		t.Fatalf("CONTROL FAILED: the restored group came back RECONCILED, so the gate this case " +
			"measures is not even reached and the dirty walk below would seed for the right reason " +
			"by accident")
	}
	if high, err := restored.ownHighWaterLocked(world.handle); err != nil || high != 0 {
		t.Fatalf("CONTROL FAILED: eve's floor already stands at %d (%v) before any walk, so a "+
			"floor that has not moved below would say nothing", high, err)
	}
	at := &rotMember{name: "eve-restored", dev: eve.dev, group: restored, leaf: eve.leaf}

	// ── THE DIRTY WALK: the claims are there, the floor does not move ───────────────────────
	page := append([]*sealed{}, world.bobRecords...)
	page = append(page, bent)
	err = world.deliver(at, page...)
	if err == nil {
		t.Fatalf("CONTROL FAILED: the walk carrying a bent record answered nil, so it is not dirty " +
			"and this case measures a clean walk twice")
	}
	if restored.Reconciled() {
		t.Fatalf("the restored group reconciled over a dirty walk, which is a different defect")
	}
	if high, err := restored.ownHighWaterLocked(world.handle); err != nil || high != 0 {
		t.Fatalf("the dirty walk moved eve's floor to %d (%v). The clone check has not run yet -- "+
			"a dirty walk is exactly the walk it refuses to conclude from -- so a floor raised here "+
			"is the evidence of a second copy of this folder erased before anything read it", high, err)
	}
	if seeded := restored.Stats().StreamFloorSeeded; seeded != 0 {
		t.Fatalf("Stats.StreamFloorSeeded is %d after a dirty walk, want 0", seeded)
	}

	// ── THE CLEAN WALK: the check concludes, and THEN the floor moves ───────────────────────
	if err := world.deliver(at, world.bobRecords...); err != nil {
		t.Fatalf("the clean walk answered %v", err)
	}
	if !restored.Reconciled() {
		t.Fatalf("CONTROL FAILED: the clean walk did not reconcile, so the floor below is held " +
			"back by the gate rather than released by it and the two halves are one")
	}
	high, err := restored.ownHighWaterLocked(world.handle)
	if err != nil {
		t.Fatalf("eve's floor after the clean walk: %v", err)
	}
	if high != 3 {
		t.Fatalf("eve's floor stands at %d after the clean walk, want 3. The gate delays the seed "+
			"by one walk; it must not cancel it, or the newcomer is bricked exactly as before", high)
	}
	t.Logf("the dirty walk left eve's floor at 0 with the clone check unconcluded, and the clean " +
		"walk that followed reconciled first and then moved it to 3")
}

// ── 7. THE NEWCOMER'S STREAM STARTS WHERE THE PREVIOUS OCCUPANT'S ENDED ──────────────────────

// A SURVIVOR OPENS A NEWCOMER WHOSE FIRST INDEX IS PAST THE RATCHET WINDOW, AND THAT IS THE STATE
// THE FIRST TWO PIECES OF THIS ITEM PRODUCE BETWEEN THEM.
//
// THE TWO PIECES DISAGREED ABOUT ONE FACT AND ONLY ONE OF THEM COULD BE RIGHT. The SEED
// ([Group.seedOwnStreamLocked]) moves the newcomer's own reserver past every index the removed
// member spent, because the server's stream monotonicity is keyed on (group_id, sender_handle)
// with no epoch and refuses anything at or below its last index there -- so the newcomer's first
// ACCEPTED record is at the previous occupant's high water plus one. The PRUNE
// ([Group.pruneRemovedLaddersLocked]) then dropped the survivor's head for that leaf on the
// argument that the newcomer's ladder belongs "at 0, which is the correct head for a stream that
// starts here". Both cannot hold: a stream that starts at prev+1 met by a ladder at 0 is
// prev+1 rungs ahead of its head, and [messagegroup.ReceiverRatchet] refuses anything more than
// [messagegroup.DefaultRecordWindowSize] ahead.
//
// MEASURED BEFORE THE REPAIR, with a previous occupant of 1,025 lines: the newcomer's first record
// is at index 1,026 and every survivor answered "index 1026 is 1026 ahead of head 0, and the
// window is 1024", three times, and then ABANDONED it -- the newcomer's whole history, gone from
// every member that watched the commit, with [ErrRecordAbandoned] as the only sign. A restart
// repaired it by accident, because a restored device seeds its current heads off the disk's
// per-epoch table instead.
//
// THIS CASE IS THAT COHORT AT ONE SEAL RATHER THAN A THOUSAND. The leaving member's own floor is
// moved through the production seeder first ([newReuseWorldAbove]), so its single line sits at
// exactly the window's edge and the newcomer's first line sits one past it. The arithmetic is
// asserted rather than assumed, so a build whose window moved does not turn this case vacuous.
//
// THE CONTROL FIRES FOR ITS OWN REASON, IN THE SAME QUERY: the survivor's PER-LADDER row for that
// leaf is 0 -- the prune really did take it -- so what opens the record is
// [Group.leafStreamFloorLocked] reading the per-epoch table, and not a row the prune left behind.
//
// WHAT WOULD GO RED: delete the floor in [Group.trackLocked]; prune [Group.peerHeadsAt] or
// [Group.persistedHeads] with [Group.peerHeads] (the floor has nothing to read); make
// [Group.pastHeadLocked] skip a previous occupant's rows (the restart half below).
func TestASurvivorOpensANewcomerWhoseStreamStartsPastTheRatchetWindow(t *testing.T) {
	window := uint64(messagegroup.DefaultRecordWindowSize)
	world := newReuseWorldAbove(t, 1, window-1)
	carol, eve := world.carol, world.eve

	if at := world.bobRecords[0].record.Header.StreamIndex; at != window {
		t.Fatalf("the leaving member's one line is at stream index %d and this case needs it at "+
			"the window's edge, %d", at, window)
	}

	// ── THE NEWCOMER'S FIRST ACCEPTED INDEX, THROUGH THE SEED ───────────────────────────────
	page := append([]*sealed{}, world.bobRecords...)
	page = append(page, world.published.page()...)
	if err := world.deliver(eve, page...); err != nil {
		t.Fatalf("eve's first walk answered %v", err)
	}
	if high := world.highWater(eve); high != window {
		t.Fatalf("eve's floor stands at %d after the walk, want %d", high, window)
	}
	line := world.sealDurable(eve, "the newcomer's first line, one past the window's edge")
	if at := line.record.Header.StreamIndex; at != window+1 {
		t.Fatalf("eve's first seal took index %d, want %d", at, window+1)
	}

	// ── THE CONTROL: THE PER-LADDER ROW IS GONE, SO THE FLOOR IS WHAT ANSWERS ───────────────
	wire, err := message.RetentionClassWire(message.RetentionDurable, 0)
	if err != nil {
		t.Fatalf("the durable retention wire byte: %v", err)
	}
	reused := ladderKey{leaf: world.leaf, retentionWire: wire, ephWindow: 0}
	if head := carol.group.peerHeads[reused]; head != 0 {
		t.Fatalf("CONTROL FAILED: the survivor still holds a per-ladder head of %d for the reused "+
			"leaf, so this case would pass on a build with no floor at all", head)
	}
	floor := carol.group.leafStreamFloorLocked(world.leaf, carol.group.epoch)
	if floor != window {
		t.Fatalf("the floor a ladder over the reused leaf is installed at is %d, want %d", floor, window)
	}
	if line.record.Header.StreamIndex <= window {
		t.Fatalf("this cohort does not reach past the window, so the refusal it exists to drive " +
			"cannot happen and the case is vacuous")
	}
	t.Logf("the newcomer's first index is %d, the survivor's per-ladder row is 0, and a ladder at "+
		"0 is %d ahead of its head against a window of %d",
		line.record.Header.StreamIndex, line.record.Header.StreamIndex, window)

	// ── THE PROPERTY, IN PROCESS ────────────────────────────────────────────────────────────
	opened, err := carol.group.receiveForTest(world.rotWorld, carol, line)
	if err != nil {
		t.Fatalf("the survivor's walk over the newcomer's first record answered %v. That is the "+
			"newcomer's whole history abandoned at every member that watched the commit", err)
	}
	if len(opened) != 1 {
		t.Fatalf("the survivor opened %d message(s) from the newcomer's first record, want 1", len(opened))
	}
	if !bytes.Equal(opened[0].SenderIdentity, eve.dev.identityPub) {
		t.Fatalf("the newcomer's record is attributed to %x, want eve's %x",
			opened[0].SenderIdentity, eve.dev.identityPub)
	}

	// ── AND AFTER A RESTART, WHICH READS THE FLOOR OFF THE DISK ─────────────────────────────
	carol = world.restart(carol)
	delivered, err := carol.group.receiveForTest(world.rotWorld, carol,
		append(append([]*sealed{}, world.bobRecords...), line)...)
	if err != nil {
		t.Fatalf("the restarted survivor's walk over the previous occupant's line and then the "+
			"newcomer's answered %v", err)
	}
	if len(delivered) != 2 {
		t.Fatalf("the restarted survivor delivered %d record(s), want 2", len(delivered))
	}
	if len(carol.group.unopened) != 0 {
		t.Fatalf("the restarted survivor gave up on %d record(s)", len(carol.group.unopened))
	}
	t.Logf("one leaf, two occupants, one run of stream indices: %d then %d, opened at the survivor "+
		"in process and again after a restart", window, window+1)
}

// ── 8. A LEAF THAT CHANGES HANDS TWICE, WHICH IS THE ONE NUMBER THE DEPARTED TABLE HOLDS ─────

// THE MIDDLE OCCUPANT'S OWN RECORDS STILL RESOLVE, AND THE TABLE THAT ANSWERS THAT IS ONE ROW WIDE.
//
// [Group.departedAt] is one uint64 per LEAF, and a leaf can be removed, refilled and removed again.
// The row therefore has to choose which departure it carries, and the choice decides whose records
// can still be resolved: the handle table [Group.leavesAtLocked] builds is the PRE-FILTER every
// record of a leaf the current tree no longer carries has to pass, and a record that does not pass
// it answers "which is no leaf of this group at epoch n", is retried [maxRecordAttempts] times and
// is ABANDONED.
//
// THE OLD ROW CARRIED THE FIRST DEPARTURE, on the argument that "an entry raised to the second
// removal's epoch would claim the leaf stood continuously between them". It did not stand
// continuously -- and that claim costs nothing, because the table is a pre-filter that ALREADY
// over-claims for a leaf added later, and what decides that a record was really written by the leaf
// it names is MASTER section 8.4.3's R1 inside the open. What the first departure costs is this
// case: with a leaf removed at epoch 2 and again at epoch 3, every record the MIDDLE occupant
// sealed at epoch 2 fails the filter and is abandoned at every survivor, on every restart.
//
// THE CONTROL IS IN THE SAME WALK AND FIRES FOR ITS OWN REASON: the FIRST occupant's records, at
// the epoch below, resolve too -- so "the filter passed" cannot be satisfied by a build that
// stopped filtering, and the two occupants come back under two different identities out of one
// sixteen-octet handle.
//
// WHAT WOULD GO RED: file the LOWEST departure epoch instead of the highest in
// [Group.noteDepartedLeavesLocked] (the middle occupant's line is abandoned); delete the
// departed-leaf half of [Group.leavesAtLocked] (both occupants' lines are).
func TestALeafThatChangesHandsTwiceStillResolvesTheMiddleOccupantsRecords(t *testing.T) {
	world := newReuseWorld(t, 3)
	alice, carol, eve := world.alice, world.carol, world.eve

	// (1) THE MIDDLE OCCUPANT WRITES, at the epoch the first removal opened, on a floor seeded
	// past everything the first occupant spent.
	page := append([]*sealed{}, world.bobRecords...)
	page = append(page, world.published.page()...)
	if err := world.deliver(eve, page...); err != nil {
		t.Fatalf("eve's first walk answered %v", err)
	}
	middle := world.sealDurable(eve, "the middle occupant's only line")
	if middle.record.Header.Epoch != world.published.opens {
		t.Fatalf("the middle occupant's line is at epoch %d and it was admitted at %d",
			middle.record.Header.Epoch, world.published.opens)
	}
	if err := world.deliver(carol, middle); err != nil {
		t.Fatalf("carol's walk over the middle occupant's line answered %v", err)
	}

	// (2) AND IS REMOVED IN ITS TURN, with nobody added, so the leaf is out of the tree for good.
	second := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemove([]uint32{world.leaf})
	})
	if err := world.deliver(carol, second.page()...); err != nil {
		t.Fatalf("carol's walk over the commit that removes the middle occupant: %v", err)
	}
	carol.group.mutex.Lock()
	departed := carol.group.departedAt[world.leaf]
	carol.group.mutex.Unlock()
	if departed != second.opens {
		t.Fatalf("the survivor's departed row for leaf %d carries epoch %d and the LAST removal "+
			"opened %d. A row carrying the FIRST departure (%d) answers `not standing` for the "+
			"middle occupant's own epoch", world.leaf, departed, second.opens, world.published.opens)
	}

	// AND THE CONTROL FOR THE FIXTURE: the leaf really is out of the CURRENT membership, so the
	// filter below is the departed table's answer and not the tree's.
	carol.group.mutex.Lock()
	standing := false
	for at := 0; at < carol.group.handle.MemberCount(); at += 1 {
		leaf, _, _, memberErr := carol.group.handle.MemberAt(at)
		if memberErr != nil {
			carol.group.mutex.Unlock()
			t.Fatalf("carol's member %d: %v", at, memberErr)
		}
		if leaf == world.leaf {
			standing = true
		}
	}
	carol.group.mutex.Unlock()
	if standing {
		t.Fatalf("CONTROL FAILED: leaf %d is still in the survivor's current membership, so the "+
			"tree resolves its handle anyway and this case measures nothing", world.leaf)
	}

	// (3) THE RESTART, AND BOTH OCCUPANTS' RECORDS COME BACK.
	carol = world.restart(carol)
	delivered, err := carol.group.receiveForTest(world.rotWorld, carol,
		append(append([]*sealed{}, world.bobRecords...), middle)...)
	if err != nil {
		t.Fatalf("the restarted survivor's walk over both occupants' records answered %v", err)
	}
	if len(delivered) != len(world.bobRecords)+1 {
		t.Fatalf("the restarted survivor delivered %d record(s), want %d",
			len(delivered), len(world.bobRecords)+1)
	}
	if len(carol.group.unopened) != 0 {
		t.Fatalf("the restarted survivor gave up on %d record(s)", len(carol.group.unopened))
	}
	for at, one := range delivered[:len(world.bobRecords)] {
		if !bytes.Equal(one.SenderIdentity, world.bob.dev.identityPub) {
			t.Fatalf("the first occupant's line %d is attributed to %x, want bob's %x",
				at+1, one.SenderIdentity, world.bob.dev.identityPub)
		}
	}
	last := delivered[len(delivered)-1]
	if last.Gap != "" {
		t.Fatalf("the middle occupant's line came back as a %q gap", last.Gap)
	}
	if !bytes.Equal(last.SenderIdentity, eve.dev.identityPub) {
		t.Fatalf("the middle occupant's line is attributed to %x, want eve's %x",
			last.SenderIdentity, eve.dev.identityPub)
	}
	t.Logf("leaf %d held two occupants and then nobody: both removals are one row carrying epoch "+
		"%d, and all %d records survive the restart under two identities out of one handle %x",
		world.leaf, departed, len(delivered), world.handle)
}

// ── 9. THE NEWCOMER DOES NOT SHOW THE REMOVED MEMBER'S HISTORY AS ITS OWN ────────────────────

// EVERY RECORD OF THE PREVIOUS OCCUPANT REACHES THE NEWCOMER ON THE ONE ROAD THAT CANNOT OPEN
// ANYTHING, AND IT USED TO COME BACK `mine`.
//
// WHY THIS ROAD AND NO OTHER, WHICH IS WHY THE FOURTH PIECE'S OWN REPAIR COULD NOT REACH IT.
// [Group.recordIsOwnLocked] decides `mine` on the credential identity the OPEN authenticated --
// and a newcomer on a reused leaf can open not one record the previous occupant wrote: every one
// of them is below its admission, so the session holds no state for their epoch and the walk
// delivers them as [GapOutOfWindow] gaps. [Group.noteEpochGapLocked] is that road, it is reached
// before the open, and its `mine` was `walk.own[header.SenderHandle]` -- the sixteen octets. So
// the repair covered the records a newcomer CAN open, which is none of them, and the records it
// cannot were exactly the removed member's whole history.
//
// MEASURED BEFORE THE REPAIR: three records, `gap="out_of_window" mine=true senderIdentity=`, all
// carrying the removed member's message_ids, at the newcomer -- which is verbatim the harm
// [Message.Mine]'s own doc says this item prevents, and it reached the C ABI as `"mine": true`.
//
// THE PRE-FILTER IS ASSERTED TO FIRE, which is what makes `mine == false` a statement about the
// repair rather than about the fixture: the handle on every one of these records IS in the
// newcomer's own handle set, so the build this replaces answered `true` for all three.
//
// AND THE POSITIVE CONTROL IS ON THE SAME ROAD, THROUGH THE SAME FUNCTION. A device cannot reach
// the out-of-window road for a record of its OWN inside [messagegroup.PastEpochWindow] epochs --
// the reason a record lands there is that no schedule on this device reaches its epoch, and its
// own records are at epochs it holds state for -- so the control hands [Group.noteEpochGapLocked]
// the header of a record this device really sealed, with the own-index row [Group.Send] writes,
// on a walk built as [rotWorld.deliver] builds one. Without it, "mine is false" would be
// satisfied by a build that answered false for everything on this road.
//
// WHAT WOULD GO RED: put `mine := walk.own[header.SenderHandle]` back (all three of the removed
// member's lines come back as the newcomer's own); drop the body_hash half of the test (the
// control still passes and a previous occupant's record at a coincident index would too).
func TestANewcomerDoesNotShowTheRemovedMembersHistoryAsItsOwn(t *testing.T) {
	world := newReuseWorld(t, 3)
	eve := world.eve

	delivered, err := eve.group.receiveForTest(world.rotWorld, eve, world.bobRecords...)
	if err != nil {
		t.Fatalf("the newcomer's walk over the previous occupant's records answered %v. They are "+
			"below its admission, which is a GAP and not a failure", err)
	}
	if len(delivered) != len(world.bobRecords) {
		t.Fatalf("the newcomer delivered %d of the previous occupant's %d records",
			len(delivered), len(world.bobRecords))
	}
	for at, one := range delivered {
		if one.Gap != GapOutOfWindow {
			t.Fatalf("the previous occupant's line %d came back as %q and want %q",
				at+1, one.Gap, GapOutOfWindow)
		}
		// THE PRE-FILTER FIRES: these records carry THIS device's own sixteen octets.
		if [16]byte(one.SenderHandle) != world.handle {
			t.Fatalf("CONTROL FAILED: the previous occupant's line %d carries sender_handle %x and "+
				"the newcomer derives %x, so this case is not about a reused leaf",
				at+1, one.SenderHandle, world.handle)
		}
		if !eve.group.ownHandles[world.handle] {
			t.Fatalf("CONTROL FAILED: the newcomer does not hold %x in its own handle set, so the "+
				"road below never took the handle at face value and `mine == false` says nothing",
				world.handle)
		}
		if one.Mine {
			t.Fatalf("the newcomer reads the previous occupant's line %d as its OWN. The handle is "+
				"SenderHandle(group_handle_key, leaf) and takes no identity, so a device that "+
				"concludes `mine` from it shows a removed member's whole history as its own",
				at+1)
		}
		if len(one.SenderIdentity) != 0 {
			t.Fatalf("the previous occupant's line %d carries sender_identity %x on a record that "+
				"did not open", at+1, one.SenderIdentity)
		}
		if !bytes.Equal(one.MessageId, world.bobIds[at]) {
			t.Fatalf("the newcomer named the previous occupant's line %d %x and it is %x",
				at+1, one.MessageId, world.bobIds[at])
		}
	}
	t.Logf("all %d of the previous occupant's records reached the newcomer under the newcomer's "+
		"OWN handle %x, as out_of_window gaps, and not one of them is `mine`",
		len(delivered), world.handle)

	// ── THE POSITIVE CONTROL, ON THE SAME ROAD ──────────────────────────────────────────────
	own := world.sealDurable(eve, "a line this device really sealed")
	eve.group.mutex.Lock()
	eve.group.ownIndices[own.record.Header.StreamIndex] = &ownSealed{
		bodyHash: own.record.Header.BodyHash,
		hasCopy:  true,
		body:     []byte{byte(KindText), 'x'},
		sentAtMs: time.Now().UnixMilli(),
	}
	walk := &pageWalk{own: eve.group.ownHandles, ownNow: world.handle, opened: []*Message{},
		leaves: map[uint64]map[[16]byte]uint32{}, unobtainable: map[uint64]bool{}, reconciled: true}
	eve.group.noteEpochGapLocked(walk, 9_000, &own.record.Header)
	eve.group.mutex.Unlock()
	if len(walk.opened) != 1 {
		t.Fatalf("CONTROL FAILED: the same road delivered %d message(s) for a record this device "+
			"sealed, want 1", len(walk.opened))
	}
	if !walk.opened[0].Mine {
		t.Fatalf("CONTROL FAILED: a record this device sealed, at an index and a body_hash it " +
			"holds, is not `mine` on this road -- so the property above is a build that answers " +
			"false for everything and not one that can tell two occupants of a leaf apart")
	}
	if !bytes.Equal(walk.opened[0].SenderIdentity, eve.dev.identityPub) {
		t.Fatalf("CONTROL FAILED: the same record carries sender_identity %x and want this "+
			"device's %x -- the two fields must agree on every record of this road",
			walk.opened[0].SenderIdentity, eve.dev.identityPub)
	}
	t.Logf("CONTROL: a record this device sealed, met on the SAME out_of_window road, is `mine` "+
		"and carries this device's own sender_identity %x", eve.dev.identityPub[:4])
}
