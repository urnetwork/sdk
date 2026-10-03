package cp3b

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sdk/urmessage"
)

// THE SEND SIDE OF T-b, ASKED OF A MEMBER THAT IS NOT AT LEAF 0 (the 2026-10-03 diff review's probe,
// kept as written). Every other successful Delete in these suites is the founder's, so a send side
// that derived the wrong leaf's handle refused every joiner's deletion and stayed green.
func TestAJoinerDeletesItsOwnLine(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()
	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	for _, who := range []*persona{alice, bob} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, newGroupId(t))
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive: %v", err)
	}
	line, err := bobGroup.Send(ctx, "bob's own line")
	if err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	if _, err := bobGroup.Delete(ctx, line.MessageId); err != nil {
		t.Fatalf("bob (leaf 1) may not delete his own line: %v", err)
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	if got := messageById(t, aliceGroup, line.MessageId); !got.Deleted {
		t.Errorf("alice did not apply bob's tombstone")
	}
}

// A LINE ANOTHER DEVICE OF THE SAME IDENTITY SENT IS Message.Mine HERE (Mine is the identity) AND T-b
// IS PER DEVICE (the 2026-10-03 diff review's probe, kept as written). The send side that asked
// held.Mine sealed a tombstone for it that every receiver ignores; sameSender refuses it. A D7 ruling
// flips this row, and must flip it on purpose.
func TestThisDeviceCannotDeleteItsOtherDevicesLine(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()
	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	for _, who := range []*persona{alice, bob} {
		if err := who.device.Connect(ctx); err != nil {
			t.Fatalf("%s's Connect: %v", who.name, err)
		}
	}
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, newGroupId(t))
	aliceId := rolesIdentityOf(t, aliceGroup)
	laptop := world.seamMemberClaiming(t, ctx, "alice-laptop", aliceId)
	invite, err := aliceGroup.AddMemberAndPublish(ctx, laptop.keyPackage(t))
	if err != nil {
		t.Fatalf("add laptop: %v", err)
	}
	laptop.join(t, gcReencodeInvite(t, invite))
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's Receive: %v", err)
	}
	// the laptop seals and submits one TEXT line
	head := make([]byte, 9)
	head[0] = 0x02
	binary.BigEndian.PutUint64(head[1:], uint64(time.Now().UnixMilli()))
	plaintext := append([]byte{0x01}, []byte("a line from alice's laptop")...)
	record, err := laptop.session.SealRecord(message.RetentionDurable, 0, false, head, plaintext, 0, nil)
	if err != nil {
		t.Fatalf("laptop seal: %v", err)
	}
	header := &record.Header
	retentionWire, err := message.RetentionClassWire(header.RetentionClass, header.EphBucket)
	if err != nil {
		t.Fatal(err)
	}
	recordBytes, err := message.EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	response, err := laptop.transport.Call(ctx, &protocol.SubmitRequest{
		GroupId: laptop.groupId,
		Records: []*protocol.Record{{
			SenderHandle:   append([]byte{}, header.SenderHandle[:]...),
			Epoch:          header.Epoch,
			StreamIndex:    header.StreamIndex,
			IsCommit:       header.IsCommit,
			RetentionClass: uint32(retentionWire),
			SizeBucket:     uint32(header.SizeBucket),
			ExpireAtMs:     header.ExpireAt,
			BodyHash:       append([]byte{}, header.BodyHash[:]...),
			BlobId:         append([]byte{}, header.BlobId...),
			RecordBytes:    recordBytes,
		}},
	})
	if err != nil || response.GetReason() != protocol.Reason_REASON_OK {
		t.Fatalf("laptop submit: %v %v", err, response.GetReason())
	}
	if _, err := aliceGroup.Receive(ctx); err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	var target *urmessage.Message
	for _, m := range aliceGroup.Messages() {
		if m.Text == "a line from alice's laptop" {
			target = m
		}
	}
	if target == nil {
		t.Fatalf("CONTROL FAILED: alice's phone holds no laptop line; %d message(s)", len(aliceGroup.Messages()))
	}
	t.Logf("the laptop's line at alice's phone: Mine=%v identity-is-alice=%v", target.Mine, string(target.SenderIdentity) == string(aliceId))
	if !target.Mine {
		t.Fatalf("CONTROL FAILED: the laptop's line is not Mine at the phone, so this row distinguishes nothing")
	}
	_, err = aliceGroup.Delete(ctx, target.MessageId)
	if err == nil {
		t.Errorf("alice's phone sealed a tombstone for her laptop's line, which T-b's receive side ignores")
	} else if !errors.Is(err, urmessage.ErrContentMalformed) {
		t.Errorf("refused with %v", err)
	}
}
