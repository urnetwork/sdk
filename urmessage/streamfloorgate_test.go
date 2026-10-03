// THE GATE THAT SAYS THIS DEVICE'S OWN STREAM FLOOR HAS BEEN HELD, DRIVEN AGAINST THE TWO WALKS
// THAT RAISED IT WHILE THE FLOOR STOOD EXACTLY WHERE IT STARTED. LEDGER ITEM 245's FIRST PIECE.
//
// [Group.ownFloorHeld] was raised by [Group.walkSawTheWholeHistoryLocked] alone -- complete page, no
// omission, no record that failed -- which is a statement about whether the SERVER finished handing
// something over. The thing it was read as saying is that this device's durable reserver now stands
// above every stream index the server holds a claim at under this device's sixteen octets, and
// [Group.seedOwnStreamLocked], the only code that moves that floor, returned early on three roads
// the gate could not see. This file is the walks that get through, each with the floor printed beside
// the flag so that "the gate is up" and "the floor moved" cannot be confused for each other: a clean
// walk over an empty page (1), a row this build could not parse at all (2), a reserver
// that cannot move a floor at all (3), and a claim read by a walk that was not allowed to act on it
// (4).
//
// AND CASE 5 IS THE REPAIR OF WHAT CASE 2's FIRST SHAPE DID, WHICH WAS WORSE THAN THE DEFECT IT
// CLOSED. That shape was a FOURTH clause on the gate -- one row this build gave up on unparsed,
// ANYWHERE in a group's history, took [Group.Send] away and nothing put it back -- and because
// neither the cursor nor the attempt counts are persisted, every restart re-walked the row,
// re-abandoned it and re-derived the veto. Case 5 drives a group with NO REUSED LEAF AT ALL: three
// founding members, three distinct sender_handles, nothing ever removed, one bent row. It sent
// before the restart and was refused for ever after it. Case 2 is now the same row ON THIS DEVICE'S
// OWN STREAM, and its assertion has moved from a refusal to a FLOOR: §4.3.3's projection says whose
// stream an unreadable row is on and at what index, so the honest case ends in a send at an index
// the previous occupant never spent rather than in a brick.
//
// MOST CASES ARE THE SAME COHORT AS THE REST OF THE REMOVAL SUITE -- bob spends indices under leaf
// 1, the leaf is removed, eve is added onto it and derives bob's handle byte for byte, carol at
// leaf 2 is the control that this build does not derive one handle for every leaf
// ([newReuseWorld]). Case 5 is the OTHER cohort on purpose: [newRotWorld]'s founding three, where
// no leaf has ever changed hands and the gate's own subject does not arise.
//
// WHAT WOULD GO RED: M-gate-empty -- put the gate back to `if self.walkSawTheWholeHistoryLocked(walk)`
// (cases 1 and 3); M-gate-cursor -- drop its cursor clause (case 1);
// M-gate-noseed -- make [Group.seedOwnStreamLocked] answer true when the [StreamIndexSeeder]
// assertion fails (case 3); M-claim-perwalk -- clear [Group.ownClaimSeen] at the end of
// [Group.commitWalkLocked], which is what a claim number that lives on the walk does (case 4);
// M-blind-veto -- put the deleted fourth clause back on [Group.ownFloorHeldByLocked], keyed on any
// row given up on unparsed (case 5, and case 2's own send); M-proj-ignore -- have
// [Group.noteUnparsedClaimLocked] fold nothing, which is the build case 2 was written against
// (case 2's floor and its message_id); M-proj-anyhandle -- have it fold the projection without
// comparing it to this device's own handle (case 5's floor); M-proj-trustmissing -- have it answer
// true for a row with no projection (case 2's residual counter).
//
// AND ONE CLAUSE THAT IS DELIBERATELY NOT DRIVEN, because a case for it would be a case for nothing:
// the seed's `!self.reconciled` early return. [Group.commitWalkLocked] sets `reconciled` ABOVE the
// seed on the first clean walk, so the only walks that reach the seed unreconciled are DIRTY ones --
// which clause 2 of [Group.ownFloorHeldByLocked] refuses for its own reason. The clause is kept
// because it is the honest answer to "did this call establish the floor", and a mutant that makes it
// answer true survives this file. That is recorded here rather than left to be discovered.

package urmessage

import (
	"bytes"
	"errors"
	"testing"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/protocol"
)

// walkRaw is [rotWorld.deliver] with the page handed over as RAW ROWS rather than as records this
// build encoded, which is the only way to put a row a receiver CANNOT PARSE on the wire. Everything
// else is the same three lines [Group.Receive] writes.
func (self *rotWorld) walkRaw(receiver *rotMember, rows []*protocol.Record) error {
	self.t.Helper()
	return self.walkRawAt(receiver, rows, true)
}

// walkRawAt is [rotWorld.walkRaw] with the server's own COMPLETE flag in the caller's hands, which is
// the difference between a page the server finished handing over and one a transport cut short.
func (self *rotWorld) walkRawAt(receiver *rotMember, rows []*protocol.Record, complete bool) error {
	self.t.Helper()
	group := receiver.group
	own, err := group.session.SenderHandle()
	if err != nil {
		self.t.Fatalf("%s's sender handle: %v", receiver.name, err)
	}
	group.ownHandles[own] = true
	walk := &pageWalk{
		own:          group.ownHandles,
		ownNow:       own,
		leaves:       map[uint64]map[[16]byte]uint32{},
		opened:       []*Message{},
		from:         group.cursor,
		reached:      group.cursor,
		resolvedTo:   group.cursor,
		reconciled:   group.reconciled,
		complete:     complete,
		unobtainable: map[uint64]bool{},
	}
	group.openPageLocked(&protocol.FetchResponse{Records: rows}, walk)
	return group.commitWalkLocked(walk, nil)
}

// rawRows encodes a page the way the server serves it, and hands back the octets so a case can bend
// one row before the receiver sees it.
//
// IT CARRIES §4.3.3's WHOLE PROJECTION AND NOT `record_bytes` ALONE, WHICH IS A FIXTURE CORRECTION
// AND NOT A CONVENIENCE. `protocol.Record` is the octets BESIDE the server-indexed projection of
// their header -- sender_handle, stream_index, epoch, body_hash -- and the deployed server fills
// every field of it out of the same `projectionOf` the submit path verifies a client's against
// (message-server `api/fetch.go`, which re-encodes the row it serves from the columns it indexed).
// A fixture that handed over the octets alone was a server no deployment has: it made a row this
// build cannot parse a row with NO ATTRIBUTION, which is the one state in which the floor question
// really is unanswerable, and a gate written against it refused every group in the world.
// [stripProjection] is how a case asks for that server on purpose.
func (self *rotWorld) rawRows(page ...*sealed) []*protocol.Record {
	self.t.Helper()
	rows := []*protocol.Record{}
	for _, one := range page {
		row, err := projectionOf(one.record)
		if err != nil {
			self.t.Fatalf("projecting record %d: %v", one.recordId, err)
		}
		row.RecordId = one.recordId
		rows = append(rows, row)
	}
	return rows
}

// stripProjection takes §4.3.3's sender_handle and stream_index off one row: a server that hands
// over a record and will not say whose stream it is on. It is a server that breaks its own MUST, and
// the one shape in which [Stats.UnopenedUnattributed] moves.
func stripProjection(rows []*protocol.Record, recordId uint64) {
	for _, row := range rows {
		if row.GetRecordId() == recordId {
			row.SenderHandle = nil
			row.StreamIndex = 0
		}
	}
}

// ownFloorOf is one member's own durable stream floor for THE HANDLE ITS GROUP SEALS UNDER, read off
// the reserver that group holds. [reuseWorld.floorOf] is the same number for the reuse cohort's ONE
// shared handle; this one asks each member about its own, which is what a world where no leaf
// changed hands needs.
func (self *rotWorld) ownFloorOf(who *rotMember) uint64 {
	self.t.Helper()
	own, err := who.group.session.SenderHandle()
	if err != nil {
		self.t.Fatalf("%s's sender handle: %v", who.name, err)
	}
	key := messagegroup.StreamKey{SenderHandle: own}
	copy(key.GroupId[:], self.groupId)
	high, err := who.group.device.reserver.HighWater(key)
	if err != nil {
		self.t.Fatalf("%s's own stream high water: %v", who.name, err)
	}
	return high
}

// bendPastParsing truncates one row's octets so that this build's codec cannot read them, with BOTH
// directions asserted in the same call: the row parsed before and does not parse after. It is what a
// record_format_version this build does not know looks like from here.
func bendPastParsing(t *testing.T, rows []*protocol.Record, recordId uint64) {
	t.Helper()
	for _, row := range rows {
		if row.GetRecordId() != recordId {
			continue
		}
		intact := row.GetRecordBytes()
		if _, err := message.ParseRecord(intact); err != nil {
			t.Fatalf("CONTROL FAILED: record %d does not parse intact: %v", recordId, err)
		}
		bent := append([]byte(nil), intact[:len(intact)-8]...)
		if _, err := message.ParseRecord(bent); err == nil {
			t.Fatalf("CONTROL FAILED: the bent row still parses, so this case is not about a " +
				"record this build cannot read")
		}
		row.RecordBytes = bent
		return
	}
	t.Fatalf("no row with record id %d to bend", recordId)
}

// floorOf is one member's own durable stream floor for the reused leaf's stream, read off THE
// RESERVER ITS GROUP HOLDS -- which is the one [Group.seedOwnStreamLocked] writes through, and the
// one a restarted member has. [reuseWorld.highWater] reads the member's pre-restart device, whose
// store a restart has closed.
func (self *reuseWorld) floorOf(who *rotMember) uint64 {
	self.t.Helper()
	high, err := who.group.device.reserver.HighWater(self.ownStreamKey(self.handle))
	if err != nil {
		self.t.Fatalf("%s's own stream high water: %v", who.name, err)
	}
	return high
}

// ── 1. A WALK THAT COVERED NOTHING HAS BEEN TOLD NOTHING ─────────────────────────────────────

// ONE CLEAN WALK OVER AN EMPTY PAGE RAISED THE GATE WHILE THE SERVER HELD THREE CLAIMS.
//
// The newcomer's cursor is at zero, the server hands back no rows and calls the page complete, and
// every clause of [Group.walkSawTheWholeHistoryLocked] is satisfied -- `complete`, no omission, no
// failure -- because each of them is a statement about a page that was served rather than about the
// history. The reserver stays where a joiner's reserver starts, which is nowhere, and the first
// Send then seals at index 1 of a stream the previous occupant of this leaf has already spent.
//
// A GROUP ABOVE EPOCH ZERO CANNOT HAVE AN EMPTY HISTORY, which is what makes this checkable at all
// rather than a server's word against nothing: the commit that opened this device's own epoch is
// sealed at the epoch BELOW it, so it passes item 246's `epoch <= read_epoch` ceiling, and a joiner
// asks from record zero. A page of nothing is therefore a server that showed this device nothing.
//
// THE POSITIVE CONTROL IS IN THE SAME RUN AND ON THE SAME GROUP: the identical walk over the rows
// that DO exist raises the gate, moves the floor to bob's top index, and lets the send through. So
// this case cannot pass by the gate having been nailed shut.
func TestAWalkThatCoveredNoRecordDoesNotRaiseTheOwnStreamFloorGate(t *testing.T) {
	world := newReuseWorld(t, 3)
	eve := world.eve

	if eve.group.ownFloorHeld {
		t.Fatalf("CONTROL FAILED: the newcomer's floor is already held at the Join, so this case " +
			"cannot say anything about what raises it")
	}
	topClaim := world.bobRecords[len(world.bobRecords)-1].record.Header.StreamIndex
	if world.highWater(eve) != 0 {
		t.Fatalf("CONTROL FAILED: the newcomer's reserver starts at %d and a joiner's starts at 0",
			world.highWater(eve))
	}

	if err := world.walkRaw(eve, nil); err != nil {
		t.Fatalf("the newcomer's walk over an empty page answered %v; an empty page is not a "+
			"failure and this case is about what a CLEAN walk concludes", err)
	}
	if eve.group.ownFloorHeld {
		t.Fatalf("one clean walk over an EMPTY page raised the stream floor gate. The reserver is "+
			"at %d and the server holds claims up to %d under this device's own sender_handle %x, "+
			"so the first Send would seal at index 1 of a stream that is already spent and latch "+
			"ErrIdentityInUse for the life of the process",
			world.highWater(eve), topClaim, world.handle)
	}
	if world.highWater(eve) != 0 {
		t.Fatalf("the empty walk moved the newcomer's floor to %d off no record at all",
			world.highWater(eve))
	}
	if _, err := eve.group.sendableLocked(KindText); !errors.Is(err, ErrStreamFloorUnheld) {
		t.Fatalf("the newcomer's Send after the empty walk is refused with %v, want "+
			"ErrStreamFloorUnheld", err)
	}
	t.Logf("after one clean walk over an empty page: ownFloorHeld=%v, reserver high water %d, "+
		"while the previous occupant spent indices 1..%d under the same sender_handle %x",
		eve.group.ownFloorHeld, world.highWater(eve), topClaim, world.handle[:4])

	// ── THE CONTROL: THE SAME WALK OVER THE ROWS THAT EXIST ─────────────────────────────────
	if err := world.walkRaw(eve, world.rawRows(world.bobRecords...)); err != nil {
		t.Fatalf("the newcomer's walk over the previous occupant's records answered %v", err)
	}
	if !eve.group.ownFloorHeld {
		t.Fatalf("CONTROL FAILED: a clean walk that covered the whole history did NOT raise the " +
			"gate, so the property above is a build that refuses everything")
	}
	if world.highWater(eve) != topClaim {
		t.Fatalf("CONTROL FAILED: the floor stands at %d after that walk and the previous "+
			"occupant's top claim is %d", world.highWater(eve), topClaim)
	}
	if _, err := eve.group.sendableLocked(KindText); err != nil {
		t.Fatalf("CONTROL FAILED: the newcomer's Send after the whole history answered %v", err)
	}
	t.Logf("CONTROL: the same walk over the %d rows that exist raised the gate and moved the "+
		"floor 0 -> %d", len(world.bobRecords), world.highWater(eve))
}

// ── 2. A ROW THIS BUILD CANNOT PARSE IS STILL A ROW THE SERVER INDEXED ───────────────────────

// THE REACHABLE ONE, AND THERE IS NO ADVERSARY IN IT. One row under the joiner's own sixteen octets
// is served in a wire shape this build's codec cannot parse -- which is what a record_format_version
// this build does not know looks like from here, and what msgrepo item 253's rollout window exists
// to bound. The previous occupant of this leaf wrote it, so it is the one unreadable row that IS
// evidence about this device's own floor.
//
// THE MECHANISM IS THE RETRY BOUND, NOT THE PARSE. Walks 1 and 2 answer `record N does not parse`
// and are dirty, so the gate stays down for the right reason. Walk 3 spends the last of
// [maxRecordAttempts], ABANDONS the row and resolves the cursor PAST it. Walk 4 asks from above it,
// meets an empty page and is clean by every clause of [Group.walkSawTheWholeHistoryLocked] -- and
// the build this file was first written against raised the gate there with the floor standing one
// index BELOW the claim that row carried, so the first seal landed on the previous occupant's index
// carrying its message_id byte for byte.
//
// SO THE ASSERTION IS THE FLOOR AND NOT A REFUSAL, AND THAT IS THIS CASE'S CHANGE. §4.3.3 puts the
// server's own `sender_handle` and `stream_index` on the row BESIDE the octets, and the abandonment
// folds them into [Group.ownClaimSeen] ([Group.noteUnparsedClaimLocked]) -- so walk 4's seed moves
// the floor ONTO the claim and the newcomer sends at an index nobody has spent. The first shape of
// this repair refused instead, and vetoed the gate for any unparsed row in any group for the life of
// every later process; case 5 reproduces that as the brick it was.
//
// THE RESIDUAL IS IN THE SAME RUN AND IS MEASURED RATHER THAN CLAIMED CLOSED: the identical page
// with §4.3.3's projection STRIPPED off that row -- a server breaking its own MUST -- leaves the
// floor one index low, moves [Stats.UnopenedUnattributed], and the seal that follows DOES collide.
// That is priced rather than hidden: the same server can answer any submit
// REASON_STREAM_INDEX_REUSED and latch [ErrIdentityInUse] directly, which
// [Group.cloneRefusalLocked] already states and accepts -- and that refusal dies with the process,
// where the deleted veto came back at every restart.
func TestARowThisBuildCannotParseRaisesTheFloorOffTheServersOwnProjection(t *testing.T) {
	world := newReuseWorld(t, 3)
	eve := world.eve
	top := world.bobRecords[len(world.bobRecords)-1]
	topClaim := top.record.Header.StreamIndex

	rows := world.rawRows(world.bobRecords...)
	// THE PROJECTION'S OWN CONTROLS, read off the row rather than assumed: the server attributes
	// the top row to the very sixteen octets eve now derives -- item 245's defect seen from the
	// server's side -- and it names the index bob's own header names.
	if !bytes.Equal(rows[len(rows)-1].GetSenderHandle(), world.handle[:]) {
		t.Fatalf("CONTROL FAILED: the server's projection of the top row names sender_handle %x "+
			"and eve derives %x, so this case is not about a row on this device's own stream",
			rows[len(rows)-1].GetSenderHandle(), world.handle)
	}
	if rows[len(rows)-1].GetStreamIndex() != topClaim {
		t.Fatalf("CONTROL FAILED: the projection names stream index %d and the row's own header "+
			"says %d", rows[len(rows)-1].GetStreamIndex(), topClaim)
	}
	bendPastParsing(t, rows, top.recordId)

	for at := 1; at <= maxRecordAttempts+1; at += 1 {
		err := world.walkRaw(eve, rows)
		t.Logf("walk %d: ownFloorHeld=%v, cursor=%d, floor=%d, err=%v",
			at, eve.group.ownFloorHeld, eve.group.cursor, world.highWater(eve), err)
	}

	if eve.group.cursor < top.recordId {
		t.Fatalf("the cursor is at %d and the abandoned row is record %d; this case is about the "+
			"walk that comes AFTER the record is out of reach", eve.group.cursor, top.recordId)
	}
	if abandoned := eve.group.UnopenedRecords(); len(abandoned) != 1 {
		t.Fatalf("CONTROL FAILED: this case needs exactly one row GIVEN UP ON and this group has "+
			"abandoned %v", abandoned)
	}
	if world.highWater(eve) != topClaim {
		t.Fatalf("the newcomer's floor stands at %d and the row this build could not parse claims "+
			"%d in the server's own projection of it, so the next seal takes the index the "+
			"previous occupant's line %d is already on",
			world.highWater(eve), topClaim, len(world.bobRecords))
	}
	if !eve.group.ownFloorHeld {
		t.Fatalf("the gate is down after the floor was moved onto the claim: %v",
			eve.group.streamFloorRefusalLocked())
	}
	if _, err := eve.group.sendableLocked(KindText); err != nil {
		t.Fatalf("the newcomer's Send answered %v. One row this build cannot parse must not take "+
			"Send away when the row's own projection says whose stream it is on and where", err)
	}
	if unattributed := eve.group.Stats().UnopenedUnattributed; unattributed != 0 {
		t.Fatalf("Stats.UnopenedUnattributed is %d and the server attributed every row it served",
			unattributed)
	}
	// AND THE SEAL LANDS ABOVE THE CLAIM WITH A message_id THAT IS NOT THE PREVIOUS OCCUPANT'S.
	// MASTER §8.4.5 expands an id from (group_id, sender_handle, stream_index) and nothing else, so
	// equal indices under one handle ARE equal ids and a disjoint range is the whole of the fix.
	first := world.sealDurable(eve, "eve's first line, over a row this build could not read")
	if first.record.Header.StreamIndex <= topClaim {
		t.Fatalf("eve's first seal took stream index %d and the previous occupant spent up to %d",
			first.record.Header.StreamIndex, topClaim)
	}
	if bytes.Equal(first.messageId, world.bobIds[len(world.bobIds)-1]) {
		t.Fatalf("eve's first message_id is byte-identical to the previous occupant's line %d",
			len(world.bobIds))
	}
	t.Logf("the row this build could not parse moved the floor 0 -> %d off §4.3.3's projection, the "+
		"gate is up, and eve's first seal is at index %d under a message_id of its own",
		world.highWater(eve), first.record.Header.StreamIndex)

	// ── THE RESIDUAL: THE SAME PAGE FROM A SERVER THAT WILL NOT SAY WHOSE ROW IT IS ─────────
	residual := newReuseWorld(t, 3)
	rTop := residual.bobRecords[len(residual.bobRecords)-1]
	rTopClaim := rTop.record.Header.StreamIndex
	rRows := residual.rawRows(residual.bobRecords...)
	bendPastParsing(t, rRows, rTop.recordId)
	stripProjection(rRows, rTop.recordId)
	if len(rRows[len(rRows)-1].GetSenderHandle()) != 0 {
		t.Fatalf("CONTROL FAILED: the projection is still on the row, so this arm is not about a " +
			"server that will not attribute it")
	}
	for at := 1; at <= maxRecordAttempts+1; at += 1 {
		if err := residual.walkRaw(residual.eve, rRows); err != nil {
			t.Logf("residual walk %d: %v", at, err)
		}
	}
	if !residual.eve.group.ownFloorHeld {
		t.Fatalf("the gate is down for a row the server would not attribute, which is the refusal "+
			"this commit deleted: no Receive clears it and every restart re-derives it: %v",
			residual.eve.group.streamFloorRefusalLocked())
	}
	if residual.highWater(residual.eve) != rTopClaim-1 {
		t.Fatalf("the floor stands at %d with the projection stripped, want %d: the rows the walk "+
			"COULD read must still have moved it", residual.highWater(residual.eve), rTopClaim-1)
	}
	if unattributed := residual.eve.group.Stats().UnopenedUnattributed; unattributed != 1 {
		t.Fatalf("Stats.UnopenedUnattributed is %d for one row served with no sender_handle "+
			"projection, want 1: a residual a caller cannot see is a residual nobody can act on",
			unattributed)
	}
	collided := residual.sealDurable(residual.eve, "eve's first line, over a row nobody attributed")
	if collided.record.Header.StreamIndex != rTopClaim {
		t.Fatalf("the residual seal is at stream index %d and this case PINS the collision at %d, "+
			"so that closing it turns this red rather than passing silently",
			collided.record.Header.StreamIndex, rTopClaim)
	}
	if !bytes.Equal(collided.messageId, residual.bobIds[len(residual.bobIds)-1]) {
		t.Fatalf("the residual seal's message_id is not the previous occupant's; this arm exists to " +
			"measure that collision rather than to describe it")
	}
	t.Logf("RESIDUAL: with §4.3.3's projection stripped the floor stops at %d, "+
		"Stats.UnopenedUnattributed is 1, and the seal at index %d carries the previous occupant's "+
		"message_id -- the same ErrIdentityInUse that server can answer any submit with directly, "+
		"and one that dies with this process",
		residual.highWater(residual.eve), collided.record.Header.StreamIndex)
}

// ── 3. A RESERVER THAT CANNOT MOVE A FLOOR NEVER HOLDS ONE ───────────────────────────────────

// noSeedReserver is [messagegroup.StreamIndexReserver] and NOTHING ELSE: the two methods a sender
// ratchet allocates through, with no [StreamIndexSeeder] on it. It is what a caller that built its
// own reserver over some other store hands this package.
type noSeedReserver struct {
	inner messagegroup.StreamIndexReserver
}

func (self *noSeedReserver) Reserve(stream messagegroup.StreamKey) (uint64, error) {
	return self.inner.Reserve(stream)
}

func (self *noSeedReserver) HighWater(stream messagegroup.StreamKey) (uint64, error) {
	return self.inner.HighWater(stream)
}

// A DEVICE THAT CANNOT SEED IS REFUSED RATHER THAN GIVEN THE BENEFIT OF THE DOUBT, AND THAT IS A
// CHANGE OF BEHAVIOUR THIS CASE EXISTS TO PIN.
//
// [StreamIndexSeeder] is an OPTIONAL interface and a reserver without it still works -- deliberately,
// because Reserve and HighWater are a sender ratchet's surface and a ratchet has no business moving a
// floor. What such a device could NOT do is hold its floor against a previous occupant's claims, and
// the gate used to raise anyway: [Group.seedOwnStreamLocked] returned nil for "there was nothing I
// could do", the walk was tidy, and the flag went up. So the one device that certainly WILL collide
// was the one the gate certified.
//
// It is refused now, by name, and the refusal is the good half of the trade: a sticky
// [ErrIdentityInUse] after the seal against a sentence before it. The CONTROL is the same cohort on
// the shipping reserver, which raises the gate over the same page.
//
// WHAT WOULD GO RED: make [Group.seedOwnStreamLocked] answer true when the type assertion fails.
func TestAReserverThatCannotSeedNeverRaisesTheStreamFloorGate(t *testing.T) {
	world := newReuseWorld(t, 3)
	eve := world.eve

	shipping := eve.group.device.reserver
	eve.group.device.reserver = &noSeedReserver{inner: shipping}
	if _, canSeed := eve.group.device.reserver.(StreamIndexSeeder); canSeed {
		t.Fatalf("CONTROL FAILED: the wrapper still answers StreamIndexSeeder, so this case is not " +
			"about a reserver that cannot move a floor")
	}
	if _, canSeed := shipping.(StreamIndexSeeder); !canSeed {
		t.Fatalf("CONTROL FAILED: the shipping reserver does not answer StreamIndexSeeder either, " +
			"so the refusal below would say nothing about the wrapper")
	}

	if err := world.walkRaw(eve, world.rawRows(world.bobRecords...)); err != nil {
		t.Fatalf("the walk over the previous occupant's records answered %v", err)
	}
	if eve.group.ownFloorHeld {
		t.Fatalf("the gate is up on a device whose reserver cannot move a floor. Its next seal " +
			"takes index 1 of a stream the previous occupant has spent, which is the collision " +
			"this gate exists to prevent")
	}
	if _, err := eve.group.sendableLocked(KindText); !errors.Is(err, ErrStreamFloorUnheld) {
		t.Fatalf("that device's Send answered %v, want ErrStreamFloorUnheld", err)
	}
	t.Logf("a reserver with no SeedTo left the gate down over %d rows and the send is refused by name",
		len(world.bobRecords))

	// ── THE CONTROL: THE SHIPPING RESERVER, SAME COHORT, SAME PAGE ──────────────────────────
	control := newReuseWorld(t, 3)
	if err := control.walkRaw(control.eve, control.rawRows(control.bobRecords...)); err != nil {
		t.Fatalf("CONTROL FAILED: the same walk on the shipping reserver answered %v", err)
	}
	if !control.eve.group.ownFloorHeld {
		t.Fatalf("CONTROL FAILED: the shipping reserver did not raise the gate over the same page")
	}
	if _, err := control.eve.group.sendableLocked(KindText); err != nil {
		t.Fatalf("CONTROL FAILED: the shipping reserver's Send answered %v", err)
	}
	t.Logf("CONTROL: the shipping reserver raised the gate over the same page and moved the floor to %d",
		control.highWater(control.eve))
}

// ── 4. THE CLAIM A CUT-SHORT WALK READ IS STILL THERE ON THE WALK THAT MAY ACT ON IT ─────────

// A RESTORED GROUP READS THE CLAIMS ON A WALK THAT MAY NOT SEED, AND SEEDS ON A WALK THAT NO LONGER
// SEES THEM. That gap is why the claimed index lives on the group ([Group.ownClaimSeen]) and not on
// the walk, and it is the same defect one field along from the one that moved [Group.ownIndexSeen]
// there.
//
// THE TWO WALKS, AND BOTH ARE ORDINARY. Walk 1 is a restored group's first: a page the transport cut
// short, so `complete` is false, the group does not reconcile -- and [Group.seedOwnStreamLocked]
// returns early, because a seed taken before the clone check LAUNDERS that check's own evidence. The
// cursor still moves over every row it resolved. Walk 2 asks from ABOVE those rows, gets a clean
// empty page, reconciles, and is the first walk allowed to move the floor -- with nothing under this
// device's handle in front of it.
//
// SO THE ASSERTION IS THE FLOOR AND NOT THE FLAG. A per-walk number leaves the gate up with the
// reserver exactly where the restore found it, which is the state that seals into a spent index; the
// cumulative number puts the floor on the previous occupant's top claim before the gate rises.
//
// WHAT WOULD GO RED: M-claim-perwalk -- clear [Group.ownClaimSeen] at the end of
// [Group.commitWalkLocked], which is what a number that lives on the walk does.
func TestAClaimReadByACutShortWalkStillRaisesTheFloorOnTheWalkThatSeeds(t *testing.T) {
	world := newReuseWorld(t, 3)
	topClaim := world.bobRecords[len(world.bobRecords)-1].record.Header.StreamIndex
	top := world.bobRecords[len(world.bobRecords)-1]

	eve := world.restart(world.eve)
	if eve.group.reconciled {
		t.Fatalf("CONTROL FAILED: the restored group came back reconciled, so walk 1 below would " +
			"seed and this case would not be about the walk that cannot")
	}
	if eve.group.ownFloorHeld {
		t.Fatalf("CONTROL FAILED: the restored group came back with its floor held")
	}

	// WALK 1: the page the transport cut short. It reads the claims and may not act on them.
	if err := world.walkRawAt(eve, world.rawRows(world.bobRecords...), false); err != nil {
		t.Fatalf("the cut-short walk answered %v", err)
	}
	if eve.group.reconciled {
		t.Fatalf("CONTROL FAILED: the cut-short walk reconciled the group, so the seed was allowed " +
			"to run on it after all")
	}
	if world.floorOf(eve) != 0 {
		t.Fatalf("CONTROL FAILED: the cut-short walk moved the floor to %d; the seed is gated on "+
			"the clone check and must not have run", world.floorOf(eve))
	}
	if eve.group.cursor < top.recordId {
		t.Fatalf("the cut-short walk left the cursor at %d and the rows it read end at %d; this "+
			"case needs the next walk to ask from ABOVE them", eve.group.cursor, top.recordId)
	}

	// WALK 2: clean, and there is nothing under this device's handle left to see.
	if err := world.walkRawAt(eve, nil, true); err != nil {
		t.Fatalf("the clean walk above the rows answered %v", err)
	}
	if !eve.group.reconciled {
		t.Fatalf("CONTROL FAILED: the clean walk did not reconcile the restored group")
	}
	if world.floorOf(eve) != topClaim {
		t.Fatalf("the floor stands at %d after the walk that was allowed to seed, and the previous "+
			"occupant's top claim is %d. The claim was read by walk 1 and forgotten, so the gate "+
			"below certifies a floor nobody holds", world.floorOf(eve), topClaim)
	}
	if !eve.group.ownFloorHeld {
		t.Fatalf("CONTROL FAILED: the gate is still down after a clean walk that established the " +
			"floor, so the floor assertion above says nothing about what a caller may do")
	}
	if _, err := eve.group.sendableLocked(KindText); err != nil {
		t.Fatalf("CONTROL FAILED: the restored group's Send after both walks answered %v", err)
	}
	t.Logf("the claim at index %d was read by a cut-short walk and the floor moved 0 -> %d on the "+
		"clean walk that saw none of those rows", topClaim, world.floorOf(eve))
}

// ── 5. A ROW THIS BUILD CANNOT PARSE ON SOMEBODY ELSE'S STREAM SAYS NOTHING ABOUT THIS ONE ───

// THE REGRESSION CASE 2's FIRST REPAIR INTRODUCED, WHICH WAS WORSE THAN THE DEFECT IT CLOSED.
// That repair was a FOURTH clause on [Group.ownFloorHeldByLocked]: while this group had given up on
// ANY row before reading its header, no walk could raise [Group.ownFloorHeld]. Every other clause
// there is a fact about ONE walk and clears when a later walk is better; that one was a fact about
// the GROUP, and neither the cursor nor the attempt counts are persisted -- so a restarted device
// re-walks the row, spends [maxRecordAttempts] on it again and re-derives the veto. A refusal that no
// [Group.Receive] clears and that comes back at every restart is the most expensive thing this
// package can answer, and this one fired in groups the gate's own subject does not arise in at all.
//
// THE COHORT IS THE POINT: NO LEAF HERE HAS EVER CHANGED HANDS. [newRotWorld] admits every member in
// the FOUNDING commit, so nothing has been removed, RFC 9420 §7.7 has had no blank to refill, and
// the three sender_handles are asserted DISTINCT -- the same query [newReuseWorld] uses to assert
// the opposite. The unreadable row is ALICE's, and the server says so in its own projection of it.
//
// AND THE RESTART IS THE WHOLE MEASUREMENT, BECAUSE IT IS THE ONLY THING THAT CHANGES. The same
// group, the same page, the same bent row: carol sends while the group is the one this process built,
// and was refused for ever once the group had come back off the disk, because a restored group's
// floor is not held until a walk holds it and before this commit that walk never came.
//
// WHAT WOULD GO RED: M-blind-veto -- put the fourth clause back; M-proj-anyhandle -- have
// [Group.noteUnparsedClaimLocked] fold the projection without comparing it to this device's own
// handle, which moves carol's floor off alice's index.
func TestARowThisBuildCannotParseOnAnotherStreamDoesNotRefuseThisDevicesSends(t *testing.T) {
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

	// ── THE PRECONDITION, MEASURED: THREE MEMBERS, THREE STREAMS, NO LEAF EVER REUSED ───────
	seen := map[[16]byte]string{}
	for _, who := range []*rotMember{alice, bob, carol} {
		own, err := who.group.session.SenderHandle()
		if err != nil {
			t.Fatalf("%s's sender handle: %v", who.name, err)
		}
		if other, already := seen[own]; already {
			t.Fatalf("CONTROL FAILED: %s and %s derive one sender_handle %x, so this world has a "+
				"reused leaf in it and the case below would be measuring the gate's own subject",
				other, who.name, own)
		}
		seen[own] = who.name
	}

	line := world.sealDurable(alice, "alice's only line, in a shape this build cannot read")
	rows := world.rawRows(line)
	bendPastParsing(t, rows, line.recordId)
	aliceOwn, err := alice.group.session.SenderHandle()
	if err != nil {
		t.Fatalf("alice's sender handle: %v", err)
	}
	if !bytes.Equal(rows[0].GetSenderHandle(), aliceOwn[:]) {
		t.Fatalf("CONTROL FAILED: the server's projection of the bent row names %x and alice seals "+
			"under %x, so this case is not about a row on somebody ELSE's stream",
			rows[0].GetSenderHandle(), aliceOwn)
	}

	// ── BEFORE THE RESTART: THE SAME GROUP, THE SAME ROW, AND THE SEND GOES THROUGH ──────────
	for at := 1; at <= maxRecordAttempts+1; at += 1 {
		err := world.walkRaw(carol, rows)
		t.Logf("pre-restart walk %d: ownFloorHeld=%v cursor=%d err=%v",
			at, carol.group.ownFloorHeld, carol.group.cursor, err)
	}
	if _, err := carol.group.sendableLocked(KindText); err != nil {
		t.Fatalf("CONTROL FAILED: before the restart the same group with the same unreadable row in "+
			"it answered %v, so this case cannot say what the RESTART changes", err)
	}

	// ── AND AFTER IT ────────────────────────────────────────────────────────────────────────
	revived := world.restart(carol)
	if revived.group.ownFloorHeld {
		t.Fatalf("CONTROL FAILED: the restored group came back with its floor already held, so the " +
			"walks below would be raising nothing")
	}
	if revived.group.reconciled {
		t.Fatalf("CONTROL FAILED: the restored group came back reconciled")
	}
	for at := 1; at <= maxRecordAttempts+1; at += 1 {
		err := world.walkRaw(revived, rows)
		t.Logf("post-restart walk %d: ownFloorHeld=%v cursor=%d abandoned=%v err=%v",
			at, revived.group.ownFloorHeld, revived.group.cursor,
			revived.group.UnopenedRecords(), err)
	}
	if abandoned := revived.group.UnopenedRecords(); len(abandoned) != 1 {
		t.Fatalf("CONTROL FAILED: this case needs the row GIVEN UP ON after the restart and this "+
			"group has abandoned %v", abandoned)
	}
	if !revived.group.ownFloorHeld {
		t.Fatalf("the restored group's floor is still unheld after %d walks over a row that is on "+
			"ALICE's stream. Nothing clears this -- no Receive, and a restart re-derives it -- so "+
			"this group can never send again, and NO LEAF IN THIS WORLD HAS EVER CHANGED HANDS: %v",
			maxRecordAttempts+1, revived.group.streamFloorRefusalLocked())
	}
	if _, err := revived.group.sendableLocked(KindText); err != nil {
		t.Fatalf("the restored group's Send answered %v after the gate rose", err)
	}
	if floor := world.ownFloorOf(revived); floor != 0 {
		t.Fatalf("the floor of a device that has never sealed anything stands at %d. The row the "+
			"walk could not read is on alice's stream and the index it claims is not a fact about "+
			"this one", floor)
	}
	if unattributed := revived.group.Stats().UnopenedUnattributed; unattributed != 0 {
		t.Fatalf("Stats.UnopenedUnattributed is %d and the server attributed the row it served",
			unattributed)
	}
	t.Logf("three founding members, three distinct sender_handles, one row this build cannot parse "+
		"on alice's stream: the restored group holds its floor after %d walks, its own floor is "+
		"still 0, and Send is allowed", maxRecordAttempts+1)
}
