package cp3b

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/sdk/urmessage"
)

// The two strings the restart case turns on. They are typed here and nowhere else, and the second
// one is what says the restored device is a MEMBER and not a reader of an archive.
const (
	// bob's OWN two lines. They are the whole of property (5): a restarted device that cannot
	// rebuild its own half of a conversation has lost half of what the user typed.
	bobsFirstLine  = "bob's first line, before anything was killed -- if THIS does not come back, the user has lost their own half"
	bobsSecondLine = "and bob's second line, still before anything was killed"

	beforeTheRestart = "typed before bob's device was killed -- if this does not come back, the alpha loses the conversation"
	afterTheRestart  = "typed by the SAME device after it was restarted, on the stream index its dead predecessor left behind"
	aliceAfterwards  = "and alice speaks again, to a device that has been through a restart"
)

// S2-14, WHOLE: A DEVICE THAT IS KILLED COMES BACK INTO ITS GROUP AT ITS EPOCH, READS THE WHOLE
// CONVERSATION INCLUDING ITS OWN HALF OF IT, AND SEALS ONE THE OTHER SIDE OPENS.
//
// THIS RESTART IS IN-PROCESS, AND THE SENTENCE THAT USED TO STAND HERE SAID OTHERWISE. It claimed
// property (4) below was "the only assertion in the suite that measures it across a process
// boundary". That is FALSE: [world.restart] closes and reopens inside one test process, and there
// was no os/exec anywhere in `urmessage` or `cp3b` when it was written. What IS true is the
// sentence this file can support -- everything in memory is dropped and the only thing that
// crosses is two directories on the disk. THE REAL PROCESS BOUNDARY IS
// `urmessage/crossprocess_test.go`, which re-executes the test binary and restores the device in a
// process that did not exist when the records were sealed; it is where the boundary claim now
// lives and it is the file to read for what a process death costs.
//
// WHAT THIS CASE HAS THAT THE CROSS-PROCESS ONE CANNOT. The whole seam: a real message server, a
// real `connect.Client`, real submissions and real fetches. A second process cannot reach an
// in-process server -- `connect` has no inbound listener for client frames -- so the two cases
// measure two different things on purpose and neither replaces the other.
//
// THE ONLY THING THAT CROSSES THE RESTART IS TWO DIRECTORIES. [world.restart] closes the device,
// both stores, the transport and the connect client, and then opens a new everything over the same
// two paths -- a new connect client with a new client_id, a new `sdk.MessageTransport`, a new
// `mls.CryptoProvider`, a new engine, a new device. No pointer is shared. A restore that worked
// because something stayed in memory cannot pass through that function.
//
// FIVE PROPERTIES AND NOT ONE, because "it came back" hides at least four ways of being wrong:
//
//  1. THE MEMBERSHIP. The restored device is in the group at the SAME epoch, under the SAME leaf,
//     with the SAME sender_handle. A device that re-joined would have a different leaf; a device
//     with a fresh signature key is refused by `mls.LoadGroup` outright, which is what
//     TestARestartedDeviceWithoutItsPersistedIdentityCannotRestoreItsGroup drives.
//  2. READING THE PAST. It opens a record alice sealed BEFORE the restart. That is the clause the
//     whole store exists for: the record key is `Expand(class_key, "sender/v1" ‖ LP(leaf))` off a
//     storage root that is `Extract(mls_secret, pq_secret)`, and `mls_secret` comes out of the MLS
//     exporter -- so if the MLS state did not survive, no key on that path can be re-derived and
//     the record is undecryptable rather than merely unlisted.
//  3. WRITING THE FUTURE. It seals a record ALICE opens. A restored session that had rebuilt the
//     wrong schedule would still produce a well-formed record; only the far side opening it says
//     the schedule is the group's.
//  4. THE STREAM INDEX DOES NOT REWIND. Read off the SERVER'S OWN ROWS: every record this device
//     ever sealed, before the restart and after it, carries a distinct, increasing stream index.
//     §5.6 calls a reused index "a total break of both AEADs for that record", and it is the one
//     failure a restart is most likely to cause. The durable reserver is what prevents it; this
//     case measures it through the whole seam, and `urmessage/crossprocess_test.go` measures it
//     across a real process death.
//  5. ITS OWN HALF OF THE CONVERSATION. The restored device reads back the lines IT sent, not only
//     the ones it received. THIS IS THE CLAUSE THAT WAS MISSING AND IT IS WHY THE DEFECT SURVIVED:
//     this case asserted only that ALICE's pre-restart record came back, and `Group.Receive`
//     skipped every record whose sender_handle was its own while a restored group's log started
//     empty. A user closed the app, reopened it, and got the other side's half of the conversation
//     and none of their own -- with a nil error and one counter that moves on the ordinary echo
//     case too. The asymmetry of the assertion is exactly what hid it, so the assertion is now
//     symmetric.
func TestADeviceKilledAndRestartedComesBackIntoItsGroupAndReadsAMessageSealedBeforeTheRestart(t *testing.T) {
	world := newWorld(t)
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

	// bob speaks BEFORE the restart, so the restart has a stream index to continue from rather
	// than a fresh row to allocate the first index of -- and so that property (5) has two of
	// HIS OWN lines to come back.
	bobsOwn := []string{bobsFirstLine, bobsSecondLine}
	for _, text := range bobsOwn {
		if _, err := bobGroup.Send(ctx, text); err != nil {
			t.Fatalf("bob's Send %q before the restart: %v", text, err)
		}
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive before the restart: %v", err)
	}

	// alice seals the record the restarted device has to be able to open.
	sealedBefore, err := aliceGroup.Send(ctx, beforeTheRestart)
	if err != nil {
		t.Fatalf("alice's Send before the restart: %v", err)
	}

	// what bob looked like before he died, so the restored device can be held against it
	bobHandleBefore := senderHandleOf(t, aliceGroup)
	bobEpochBefore := bobGroup.Epoch()
	bobIndicesBefore := streamIndicesOf(t, world, groupId, bobHandleBefore)
	if len(bobIndicesBefore) != 2 {
		t.Fatalf("bob sealed two records before the restart and the server holds %d for his handle: %v",
			len(bobIndicesBefore), bobIndicesBefore)
	}

	// ── the restart ──────────────────────────────────────────────────────────────────────
	bob = world.restart(t, bob)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	restored, err := bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Restore: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("bob was in one group and %d came back", len(restored))
	}
	bobGroup = restored[0]

	// AND A SECOND Restore IS A NO-OP. Two live sessions over one group would be two
	// [messagegroup.GroupSession]s drawing from ONE durable stream row -- which the reserver
	// serialises, so it does not reuse an index, but it does give the device two views of one
	// conversation and two ladders to keep. A restore that answered the group again would make
	// that the ordinary result of calling Restore twice.
	second, err := bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("a second Restore: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("a second Restore came back with %d group(s) this device already holds", len(second))
	}
	if held := bob.device.Groups(); len(held) != 1 {
		t.Errorf("after two Restores the device holds %d views of one group", len(held))
	}

	// (1) THE MEMBERSHIP
	if !bytes.Equal(bobGroup.Id(), groupId) {
		t.Fatalf("the restored group is %x and bob was in %x", bobGroup.Id(), groupId)
	}
	if epoch := bobGroup.Epoch(); epoch != bobEpochBefore {
		t.Fatalf("bob was at epoch %d and came back at %d", bobEpochBefore, epoch)
	}
	if !bobGroup.IsOpen() {
		t.Fatal("the restored group does not know it is open on the server, so it will refuse to send")
	}

	// (2) READING THE PAST
	//
	// The cursor is NOT persisted, so this re-reads the group's whole history and re-derives
	// every receiver ladder from its root. That is what makes the assertion below about KEYS
	// rather than about a list: alice's record is opened here by a schedule rebuilt off the
	// restored MLS exporter.
	got, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Receive: %v", err)
	}
	found := false
	for _, one := range got {
		if one.Text == beforeTheRestart {
			found = true
			if one.RecordId != sealedBefore.RecordId {
				t.Errorf("the restored device opened record %d and alice was told hers is record %d",
					one.RecordId, sealedBefore.RecordId)
			}
		}
	}
	if !found {
		t.Fatalf("the restored device read %d messages and none of them is the one alice sealed before the restart: %v",
			len(got), textsOf(got))
	}
	assertNothingFailedToOpen(t, "bob after the restart", bobGroup)

	// (5) ITS OWN HALF. Both of bob's pre-restart lines, opened by the restored device under a
	// receiver ladder over its OWN leaf, and marked Mine. The Mine bit is asserted separately
	// from the text because a restored device that came back with a different sender_handle
	// would open its own records as somebody else's and this assertion would pass on the text
	// alone.
	held := bobGroup.Messages()
	for _, text := range bobsOwn {
		mine := false
		for _, one := range held {
			if one.Text == text {
				mine = one.Mine
			}
		}
		if !mine {
			t.Errorf("the restored device's log does not hold %q as its own; a user who closed the app and reopened it has lost their own half of the conversation. It holds: %v",
				text, textsOf(held))
		}
	}
	if len(held) != 3 {
		t.Errorf("the restored device's log holds %d message(s) and the conversation has three: %v",
			len(held), textsOf(held))
	}
	if own := bobGroup.Stats().OpenedOwn; own != uint64(len(bobsOwn)) {
		t.Errorf("Stats.OpenedOwn is %d and this device sealed %d records before it died", own, len(bobsOwn))
	}
	t.Logf("the restored device's whole conversation: %v", textsOf(held))

	// (3) WRITING THE FUTURE
	sealedAfter, err := bobGroup.Send(ctx, afterTheRestart)
	if err != nil {
		t.Fatalf("the restarted bob's Send: %v", err)
	}
	back, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("alice's Receive after bob's restart: %v", err)
	}
	if len(back) != 1 || back[0].Text != afterTheRestart {
		t.Fatalf("alice read %v for the one line the restarted bob sent", textsOf(back))
	}
	if !bytes.Equal(back[0].SenderHandle, bobHandleBefore) {
		t.Errorf("the restarted device seals under sender_handle %x and it sealed under %x before, so it is a different leaf",
			back[0].SenderHandle, bobHandleBefore)
	}
	// and the other direction still works, which says alice did not have to do anything
	if _, err := aliceGroup.Send(ctx, aliceAfterwards); err != nil {
		t.Fatalf("alice's Send after bob's restart: %v", err)
	}
	afterwards, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's second Receive: %v", err)
	}
	if len(afterwards) != 1 || afterwards[0].Text != aliceAfterwards {
		t.Fatalf("the restarted bob read %v for the one line alice sent afterwards", textsOf(afterwards))
	}

	// (4) THE STREAM INDEX DID NOT REWIND
	indicesAfter := streamIndicesOf(t, world, groupId, bobHandleBefore)
	if len(indicesAfter) != 3 {
		t.Fatalf("bob sealed three records across the restart and the server holds %d: %v",
			len(indicesAfter), indicesAfter)
	}
	seen := map[uint64]bool{}
	for at, index := range indicesAfter {
		if seen[index] {
			t.Fatalf("stream index %d is used twice by one sender in one epoch, which section 5.6 calls a total break of both AEADs for that record: %v",
				index, indicesAfter)
		}
		seen[index] = true
		if 0 < at && index <= indicesAfter[at-1] {
			t.Errorf("bob's stream indices are not increasing across the restart: %v", indicesAfter)
		}
	}
	if indicesAfter[2] <= bobIndicesBefore[1] {
		t.Errorf("the record sealed after the restart took index %d and the last one before it took %d",
			indicesAfter[2], bobIndicesBefore[1])
	}
	t.Logf("bob's stream indices, across a restart: %v (record %d opened at alice)",
		indicesAfter, sealedAfter.RecordId)
}

// THE FOUNDER RESTARTS TOO, AND IT IS A DIFFERENT PATH FROM THE JOINER'S.
//
// A joiner's record is written by [urmessage.Device.Join]; a FOUNDER'S is written by
// [urmessage.Group.Open], and nothing else on the founder's path writes one --
// [urmessage.Device.CreateGroup] and [urmessage.Group.AddMember] both deliberately write nothing,
// because a group that has not been published is a group a restore brings back DEAD: the
// epoch-zero founding session that self-certifies the founding commit is not persisted, so a
// restored group can never be Opened.
//
// MEASURED: with only the joiner's restart case in the suite, deleting Open's write left
// everything green. This is the case that holds it -- and it holds the `Opened` bit specifically,
// because a founder that came back not knowing its group is published answers ErrGroupNotOpen to
// every send for ever.
func TestAFounderKilledAndRestartedComesBackIntoTheGroupItPublished(t *testing.T) {
	world := newWorld(t)
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
	_, bobGroup := openPair(t, ctx, alice, bob, groupId)

	const fromBob = "bob speaks while the founder is away"
	if _, err := bobGroup.Send(ctx, fromBob); err != nil {
		t.Fatalf("bob's Send: %v", err)
	}

	alice = world.restart(t, alice)
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted alice's Connect: %v", err)
	}
	restored, err := alice.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted alice's Restore: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("the founder was in one group and %d came back", len(restored))
	}
	aliceGroup := restored[0]
	if !aliceGroup.IsOpen() {
		t.Fatal("the restored founder does not know it published this group, so every send will answer ErrGroupNotOpen")
	}
	// the founder reads what was said while it was away
	heard, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("the restarted alice's Receive: %v", err)
	}
	found := false
	for _, one := range heard {
		if one.Text == fromBob {
			found = true
		}
	}
	if !found {
		t.Fatalf("the restarted founder read %d messages and none is what bob said while it was away: %v",
			len(heard), textsOf(heard))
	}
	// and it can still speak, which is the clause the `Opened` bit is for
	const fromAlice = "the founder is back, and the group is still open"
	if _, err := aliceGroup.Send(ctx, fromAlice); err != nil {
		t.Fatalf("the restarted founder's Send: %v", err)
	}
	back, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("bob's Receive after the founder's restart: %v", err)
	}
	if len(back) != 1 || back[0].Text != fromAlice {
		t.Fatalf("bob read %v for the one line the restarted founder sent", textsOf(back))
	}
	assertNothingFailedToOpen(t, "the restarted founder", aliceGroup)
	assertNothingFailedToOpen(t, "bob", bobGroup)
}

// THE FALSIFICATION OF THE WHOLE RESTORE, AND IT IS THE IDENTITY RATHER THAN THE GROUP STATE.
//
// `mls.LoadGroup` signs a GroupInfo with the key the caller hands in and verifies it against the
// leaf the RESTORED TREE holds at this member's own index -- so a device that came back with a
// fresh signature key is refused there rather than at the first message a peer drops. This case
// removes exactly the device identity record from the store between the two runs, leaves every
// MLS epoch state in place, and holds that the restore FAILS AND SAYS SO.
//
// It is what makes [NewDevice]'s persisted identity load-bearing: delete the PutDeviceIdentity
// call and every device comes back with a new key, which is the state this case reproduces.
func TestARestartedDeviceWithoutItsPersistedIdentityCannotRestoreItsGroup(t *testing.T) {
	world := newWorld(t)
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
	_, _ = openPair(t, ctx, alice, bob, groupId)

	stateDir := bob.stateDir
	bob.kill()
	// the control: the identity record IS there, and this is the file the next statement removes
	identity := filepath.Join(stateDir, "state", "device")
	if _, err := os.Stat(identity); err != nil {
		t.Fatalf("the store holds no device identity to remove, so this case removes nothing: %v", err)
	}
	if err := os.Remove(identity); err != nil {
		t.Fatalf("removing the persisted identity: %v", err)
	}

	revived := world.durablePersona(t, "bob", stateDir, bob.streamDir)
	if err := revived.device.Connect(ctx); err != nil {
		t.Fatalf("the revived bob's Connect: %v", err)
	}
	restored, err := revived.device.Restore(ctx)
	if err == nil {
		t.Fatalf("a device that came back under a new signature key restored %d group(s) without complaint", len(restored))
	}
	if !errors.Is(err, urmessage.ErrRestore) {
		t.Errorf("the refusal is %v, want one wrapping ErrRestore", err)
	}
	if len(restored) != 0 {
		t.Errorf("%d group(s) came back beside the refusal", len(restored))
	}
	t.Logf("a device that lost its identity is refused by name: %v", err)
}

// A DEVICE POINTED AT AN EMPTY DIRECTORY RESTORES NOTHING, AND THAT IS THE CONTROL ON THE CONTROL.
//
// Without it, the restart case above proves only that "a device in this process can read a
// message" -- which the two-device case already proves. This says the DISK is what carried the
// membership: the same group, the same server, the same everything, over a directory that holds no
// record, comes back with nothing and says so rather than answering an empty slice by accident.
func TestADeviceOverAnEmptyDirectoryRestoresNothingAndIsNotInTheGroup(t *testing.T) {
	world := newWorld(t)
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
	aliceGroup, _ := openPair(t, ctx, alice, bob, groupId)
	if _, err := aliceGroup.Send(ctx, beforeTheRestart); err != nil {
		t.Fatalf("alice's Send: %v", err)
	}

	bob.kill()
	root := t.TempDir()
	stranger := world.durablePersona(t, "bob", filepath.Join(root, "state"), filepath.Join(root, "stream"))
	if err := stranger.device.Connect(ctx); err != nil {
		t.Fatalf("the stranger's Connect: %v", err)
	}
	restored, err := stranger.device.Restore(ctx)
	if err != nil {
		t.Fatalf("Restore over an empty directory answered an error: %v", err)
	}
	if len(restored) != 0 {
		t.Fatalf("a device over an empty directory came back into %d group(s)", len(restored))
	}
	if groups := stranger.device.Groups(); len(groups) != 0 {
		t.Fatalf("a device over an empty directory holds %d group(s)", len(groups))
	}
}

// A DEVICE OVER A NON-DURABLE STORE IS REFUSED BY NAME RATHER THAN ANSWERING AN EMPTY SLICE.
//
// `MemoryStateStore` is still legal and still the default, and a caller that asks it to restore is
// asking for something it cannot do. An empty slice with a nil error would read exactly like "this
// device was in no groups", which is the silent-zero shape this package refuses everywhere else.
func TestRestoreOverAStoreThatPersistsNothingIsRefusedByName(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()
	device, _, _ := world.device(t, "ephemeral")
	if err := device.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	restored, err := device.Restore(ctx)
	if !errors.Is(err, urmessage.ErrNoDeviceStore) {
		t.Errorf("Restore over a memory store answered %v, want ErrNoDeviceStore", err)
	}
	if len(restored) != 0 {
		t.Errorf("%d group(s) came back from a store that persists nothing", len(restored))
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────────────────────

// openPair is the founding sequence: alice creates, bob is added, the invite crosses as octets,
// alice opens the group on the server and bob joins it.
func openPair(t *testing.T, ctx context.Context, alice *persona, bob *persona, groupId []byte) (
	*urmessage.Group, *urmessage.Group) {

	t.Helper()
	aliceGroup, err := alice.device.CreateGroup(ctx, groupId)
	if err != nil {
		t.Fatalf("alice's CreateGroup: %v", err)
	}
	keyPackage, err := bob.device.KeyPackage()
	if err != nil {
		t.Fatalf("bob's KeyPackage: %v", err)
	}
	invite, err := aliceGroup.AddMember(keyPackage)
	if err != nil {
		t.Fatalf("alice's AddMember: %v", err)
	}
	encoded, err := invite.Encode()
	if err != nil {
		t.Fatalf("encoding the invite: %v", err)
	}
	if err := aliceGroup.Open(ctx); err != nil {
		t.Fatalf("alice's Open: %v", err)
	}
	carried, err := urmessage.ParseInvite(encoded)
	if err != nil {
		t.Fatalf("parsing the invite: %v", err)
	}
	bobGroup, err := bob.device.Join(ctx, carried)
	if err != nil {
		t.Fatalf("bob's Join: %v", err)
	}
	return aliceGroup, bobGroup
}

// senderHandleOf reads the far side's sender_handle off a message this group has already opened.
// It is read rather than derived here on purpose: the derivation is the code under test.
func senderHandleOf(t *testing.T, group *urmessage.Group) []byte {
	t.Helper()
	for _, one := range group.Messages() {
		if !one.Mine {
			return append([]byte(nil), one.SenderHandle...)
		}
	}
	t.Fatal("this group has opened no message from anybody else, so there is no sender_handle to read")
	return nil
}

// streamIndicesOf reads the SERVER'S OWN ROWS and answers the stream indices one sender has used,
// in record order.
//
// The server is where this has to be measured. A client that rewound its reserver would still
// report every send as successful; the rows are the only place two records under one index are
// visible at all.
func streamIndicesOf(t *testing.T, world *world, groupId []byte, senderHandle []byte) []uint64 {
	t.Helper()
	indices := []uint64{}
	for _, row := range world.allRows(t, groupId) {
		if bytes.Equal(row.SenderHandle, senderHandle) {
			indices = append(indices, row.StreamIndex)
		}
	}
	return indices
}

func textsOf(messages []*urmessage.Message) string {
	texts := []string{}
	for _, one := range messages {
		texts = append(texts, fmt.Sprintf("%q", one.Text))
	}
	return "[" + strings.Join(texts, " ") + "]"
}
