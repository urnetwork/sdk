package cp3b

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/sdk/v2026/urmessage"
)

// DELETE FOR EVERYONE HAS NO TIME LIMIT (the owner's ruling of 2026-10-02, verbatim: "I think you
// should be able to delete your messages at any time"), held END TO END, through the real send side,
// on every road a line reaches a reader by, and across restarts. Both tests were written by the diff
// review of that change (msgrepo ledger §7, 2026-10-02) and are kept as it wrote them: the unit test
// in urmessage pins the receipt rule, and these pin what it cannot reach, which is a send-side
// refusal and the deleter's own view.

// A durable persona whose device reads its clock from `clock`, so a line can be sent "400 days ago".
func (self *world) clockPersona(t *testing.T, name string, clock *atomic.Int64) *persona {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, name, "state")
	streamDir := filepath.Join(root, name, "stream")
	client := self.connectClient(t)
	transport := self.transport(t, client)
	streamStore, err := sdk.OpenStreamStore(streamDir)
	if err != nil {
		t.Fatalf("%s: OpenStreamStore: %v", name, err)
	}
	stateStore, err := urmessage.OpenDurableStateStore(stateDir)
	if err != nil {
		streamStore.Close()
		t.Fatalf("%s: OpenDurableStateStore: %v", name, err)
	}
	device, err := urmessage.NewDevice(urmessage.DeviceConfig{
		Transport:  transport,
		Reserver:   sdk.NewStreamIndexReserver(streamStore),
		StateStore: stateStore,
		NowMs:      func() int64 { return clock.Load() },
	})
	if err != nil {
		stateStore.Close()
		streamStore.Close()
		t.Fatalf("%s: NewDevice: %v", name, err)
	}
	current := &persona{
		name: name, stateDir: stateDir, streamDir: streamDir,
		client: client, transport: transport,
		streamStore: streamStore, stateStore: stateStore, device: device,
	}
	t.Cleanup(current.kill)
	return current
}

// A 400-day-old own line is deleted through the REAL send side, on every road, and the deletion
// survives a restart of the receiver AND of the deleter. A send-side window in Group.Delete fails
// here and nowhere else; so does an own road that loses the identity T-b now needs.
func TestAnOldOwnLineIsDeletedOnEveryRoadAndAcrossRestarts(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()
	const day = int64(24 * 60 * 60 * 1000)
	var clock atomic.Int64
	clock.Store(time.Now().UnixMilli() - 400*day)
	alice := world.clockPersona(t, "alice", &clock)
	bob := world.newPersona(t, "bob")
	for _, who := range []*persona{alice, bob} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)

	old, err := aliceGroup.Send(ctx, "a line alice sent four hundred days ago")
	if err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	clock.Store(time.Now().UnixMilli())
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive: %v", err)
	}
	atBob := messageById(t, bobGroup, old.MessageId)
	if age := time.Now().UnixMilli() - atBob.SentAtMs; age < 399*day {
		t.Fatalf("CONTROL FAILED: the line's SentAtMs is only %d ms old at bob", age)
	}
	if len(atBob.SenderIdentity) == 0 {
		t.Fatalf("bob holds alice's line with no identity")
	}

	if _, err := aliceGroup.Delete(ctx, old.MessageId); err != nil {
		t.Fatalf("SEND SIDE: alice's Delete of her own 400-day-old line: %v", err)
	}
	if !messageById(t, aliceGroup, old.MessageId).Deleted {
		t.Error("alice's own view, straight after Delete: not deleted")
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	if !messageById(t, aliceGroup, old.MessageId).Deleted {
		t.Error("alice's own view, after her own walk: not deleted")
	}
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive of the tombstone: %v", err)
	}
	if !messageById(t, bobGroup, old.MessageId).Deleted {
		t.Error("bob's view: not deleted")
	}

	bob = world.restart(t, bob)
	bobGroup = hsRestoreOne(t, ctx, bob)
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("the restarted bob's Receive: %v", err)
	}
	if !messageById(t, bobGroup, old.MessageId).Deleted {
		t.Error("the restarted bob: not deleted")
	}
	if n := bobGroup.Stats().RoleUndeterminable; n != 0 {
		t.Errorf("the restarted bob could not determine %d sender(s)", n)
	}

	alice = world.restart(t, alice)
	aliceGroup = hsRestoreOne(t, ctx, alice)
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("the restarted alice's Receive: %v", err)
	}
	if !messageById(t, aliceGroup, old.MessageId).Deleted {
		t.Error("the restarted alice: her own deletion of her own line did not come back")
	}
}

// The line at epoch 1, its tombstone at epoch 2, a peer live and restarted: the identity T-b reads
// is the one each record was opened under, at its own epoch.
func TestADeletionAcrossAnEpochChange(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()
	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	for _, who := range []*persona{alice, bob} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)
	line, err := aliceGroup.Send(ctx, "a line sealed at epoch one")
	if err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive: %v", err)
	}
	hsAddAndJoin(t, ctx, aliceGroup, world.newPersona(t, "carol"))
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive of the commit: %v", err)
	}
	if aliceGroup.Epoch() != 2 || bobGroup.Epoch() != 2 {
		t.Fatalf("alice at %d, bob at %d, want 2", aliceGroup.Epoch(), bobGroup.Epoch())
	}
	if _, err := aliceGroup.Delete(ctx, line.MessageId); err != nil {
		t.Fatalf("alice's Delete at epoch 2: %v", err)
	}
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive of the tombstone: %v", err)
	}
	if !messageById(t, bobGroup, line.MessageId).Deleted {
		t.Error("bob: a tombstone at epoch 2 did not delete a line at epoch 1")
	}
	bob = world.restart(t, bob)
	bobGroup = hsRestoreOne(t, ctx, bob)
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("the restarted bob's Receive: %v", err)
	}
	if !messageById(t, bobGroup, line.MessageId).Deleted {
		t.Error("the restarted bob: line at epoch 1, tombstone at epoch 2, not deleted")
	}
	if n := bobGroup.Stats().RoleUndeterminable; n != 0 {
		t.Errorf("the restarted bob could not determine %d sender(s)", n)
	}
	alice = world.restart(t, alice)
	aliceGroup = hsRestoreOne(t, ctx, alice)
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("the restarted alice's Receive: %v", err)
	}
	if !messageById(t, aliceGroup, line.MessageId).Deleted {
		t.Error("the restarted alice: not deleted")
	}
}
