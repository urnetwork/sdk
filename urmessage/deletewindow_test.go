package urmessage

import (
	"bytes"
	"testing"
)

// THERE IS NO DELETE WINDOW. The owner ruled on 2026-10-02, verbatim: "I think you should be able to
// delete your messages at any time". A tombstone from a line's own sender applies however old the
// line is, here four hundred days. A build that measured MASTER §12.1's former 24-hour bound on
// SentAtMs, which is the design that ruling retired, fails this test.
func TestAnOldLineIsDeletedLikeANewOne(t *testing.T) {
	group := &Group{}
	group.initTables()

	mine := bytes.Repeat([]byte{0x01}, 16)
	mineId := bytes.Repeat([]byte{0xC1}, 32)
	lineId := aTarget(0xC1)
	const day = int64(24 * 60 * 60 * 1000)
	sent := int64(1_700_000_000_000)

	line := &Content{Kind: KindText, Text: "an old line"}
	if !deliverOneThroughAWalk(group, newMessage(line, 10, mine, mineId, false, sent, lineId, "member"), line) {
		t.Fatal("a TEXT did not become a line of the conversation")
	}
	tombstone := &Content{Kind: KindTombstone, Target: lineId}
	deliverOneThroughAWalk(group, newMessage(tombstone, 11, mine, mineId, false, sent+400*day, aTarget(0xC2), "member"), tombstone)
	if !heldIn(t, group, lineId).Deleted {
		t.Error("a tombstone sealed 400 days after its line, by the line's own sender, did not delete it")
	}
}

// A NEWCOMER ON A REFILLED LEAF CANNOT DELETE THE PREVIOUS OCCUPANT'S LINES (msgrepo ledger 273,
// review H4). The newcomer's records carry the SAME sixteen-octet handle (item 245) under a
// DIFFERENT identity. T-b tested on the handle alone and applied the newcomer's tombstone; it now
// requires both. And a second device of the line's own author, which shares the identity under
// another handle, is not answered here either way: D7 is unruled, so the rule stays per device.
func TestANewcomerOnARefilledLeafCannotDeleteThePreviousOccupantsLines(t *testing.T) {
	group := &Group{}
	group.initTables()

	leaf := bytes.Repeat([]byte{0x05}, 16)
	previous := bytes.Repeat([]byte{0xD1}, 32)
	newcomer := bytes.Repeat([]byte{0xD2}, 32)
	lineId := aTarget(0xD1)

	line := &Content{Kind: KindText, Text: "the previous occupant's line"}
	if !deliverOneThroughAWalk(group, newMessage(line, 10, leaf, previous, false, 0, lineId, "member"), line) {
		t.Fatal("a TEXT did not become a line of the conversation")
	}
	forged := &Content{Kind: KindTombstone, Target: lineId}
	deliverOneThroughAWalk(group, newMessage(forged, 20, leaf, newcomer, false, 0, aTarget(0xD2), "member"), forged)
	if heldIn(t, group, lineId).Deleted {
		t.Fatal("a newcomer on a refilled leaf deleted the previous occupant's line: T-b matched the handle alone")
	}
	// THE CONTROL: the author's own tombstone, same handle and same identity, deletes it
	genuine := &Content{Kind: KindTombstone, Target: lineId}
	deliverOneThroughAWalk(group, newMessage(genuine, 21, leaf, previous, false, 0, aTarget(0xD3), "member"), genuine)
	if !heldIn(t, group, lineId).Deleted {
		t.Error("the control: the line's own author, same leaf and same identity, did not delete it")
	}
}

// THE PER-DEVICE HALF, ISOLATED. The same identity under ANOTHER handle is, in MASTER's model,
// another device of the line's author; in this build, where the identity is the device's own
// signer, it is the same device added back onto another leaf. Either way its tombstone does not
// apply: deleting from another device (D7) is unruled, so the rule stays per device. A D7 ruling
// flips this row and must flip it on purpose. And two EMPTY identities under one handle never
// match: the RoleUndeterminable residual retracts nothing.
func TestTheSameSenderRuleIsPerDeviceAndAnEmptyIdentityMatchesNothing(t *testing.T) {
	group := &Group{}
	group.initTables()

	handle := bytes.Repeat([]byte{0x06}, 16)
	otherHandle := bytes.Repeat([]byte{0x07}, 16)
	author := bytes.Repeat([]byte{0xE1}, 32)

	// row 1: the author's other device (same identity, another handle)
	lineId := aTarget(0xE1)
	line := &Content{Kind: KindText, Text: "a line from the first device"}
	deliverOneThroughAWalk(group, newMessage(line, 10, handle, author, false, 0, lineId, "member"), line)
	fromOtherDevice := &Content{Kind: KindTombstone, Target: lineId}
	deliverOneThroughAWalk(group, newMessage(fromOtherDevice, 11, otherHandle, author, false, 0, aTarget(0xE2), "member"), fromOtherDevice)
	if heldIn(t, group, lineId).Deleted {
		t.Error("a tombstone from another device of the same identity deleted the line: a D7 ruling flips this row, and nothing has ruled D7")
	}
	// the control for row 1: the same line, its own handle and its own identity, does delete
	fromItsOwnLeaf := &Content{Kind: KindTombstone, Target: lineId}
	deliverOneThroughAWalk(group, newMessage(fromItsOwnLeaf, 14, handle, author, false, 0, aTarget(0xE5), "member"), fromItsOwnLeaf)
	if !heldIn(t, group, lineId).Deleted {
		t.Error("the control: the line's own leaf and identity did not delete it, so row 1 proves nothing")
	}

	// row 2: one handle, and no identity on either side
	bareId := aTarget(0xE3)
	bare := &Content{Kind: KindText, Text: "a line whose sender could not be determined"}
	deliverOneThroughAWalk(group, newMessage(bare, 12, handle, nil, false, 0, bareId, "member"), bare)
	empty := &Content{Kind: KindTombstone, Target: bareId}
	deliverOneThroughAWalk(group, newMessage(empty, 13, handle, nil, false, 0, aTarget(0xE4), "member"), empty)
	if heldIn(t, group, bareId).Deleted {
		t.Error("two empty identities under one handle matched: an undetermined sender retracted a line")
	}
}
