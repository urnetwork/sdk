package cp3b

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/urnetwork/sdk/v2026/urmessage"
)

// §4.3.5 PUSH, END TO END: THE SERVER TELLS A SUBSCRIBER THE MOMENT A RECORD LANDS, AND THE DEVICE
// ANSWERS WITH THE FETCH IT WOULD HAVE POLLED FOR.
//
// The world is the server's own endpoint with the real pipeline behind it (endpointroute_test.go).
// A push names the group and carries no records, so what is measured is that a push ARRIVES, that
// it arrives only for a subscriber, and that the Receive it prompts reads the message, and NOT
// that a record travelled inside the push.

func foundAndJoin(t *testing.T, alice *urmessage.Device, bob *urmessage.Device) (*urmessage.Group, *urmessage.Group) {
	t.Helper()
	ctx := context.Background()
	if err := alice.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}
	aliceGroup, err := alice.CreateGroup(ctx, newGroupId(t))
	if err != nil {
		t.Fatalf("alice's CreateGroup: %v", err)
	}
	keyPackage, err := bob.KeyPackage()
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
	carried, err := urmessage.ParseInvite(encoded)
	if err != nil {
		t.Fatalf("parsing the invite: %v", err)
	}
	if err := aliceGroup.Open(ctx); err != nil {
		t.Fatalf("alice's Open: %v", err)
	}
	bobGroup, err := bob.Join(ctx, carried)
	if err != nil {
		t.Fatalf("bob's Join: %v", err)
	}
	if _, err := bobGroup.Receive(ctx); err != nil {
		t.Fatalf("bob's first Receive: %v", err)
	}
	return aliceGroup, bobGroup
}

// waitPush answers the group id of the next wake of device and true, or false if none came within
// `within`. An EMPTY id is the wake that says the session to the server was replaced.
func waitPush(device *urmessage.Device, within time.Duration) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	groupId, err := device.WaitPush(ctx)
	if err != nil {
		return nil, false
	}
	return groupId, true
}

func receivedText(t *testing.T, group *urmessage.Group, text string) bool {
	t.Helper()
	messages, err := group.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	for _, message := range messages {
		if message.Text == text {
			return true
		}
	}
	return false
}

func TestASubscriberIsPushedWithinAMomentOfASendAndAnUnsubscribedOneIsNot(t *testing.T) {
	world := newEndpointWorld(t)
	alice := world.device(t, "alice")
	bob := world.device(t, "bob")
	aliceGroup, bobGroup := foundAndJoin(t, alice, bob)
	ctx := context.Background()

	// THE CONTROL FIRST: bob holds no subscription, alice sends, and nothing is pushed to bob
	const before = "sent before bob subscribed"
	if _, err := aliceGroup.Send(ctx, before); err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	if groupId, woken := waitPush(bob, 700*time.Millisecond); woken {
		t.Fatalf("bob was pushed %x while holding no subscription", groupId)
	}

	subscribed, err := bobGroup.EnsureSubscribed(ctx)
	if err != nil || !subscribed {
		t.Fatalf("bob's first EnsureSubscribed answered %v, %v; want a new subscription", subscribed, err)
	}
	if again, err := bobGroup.EnsureSubscribed(ctx); err != nil || again {
		t.Fatalf("a second EnsureSubscribed at the same epoch and Hello answered %v, %v; want current", again, err)
	}
	// a subscription announces what arrives AFTER it, so the line from before is read by a Receive
	if !receivedText(t, bobGroup, before) {
		t.Fatalf("bob's catch-up Receive did not read %q", before)
	}

	// THE PROPERTY
	sentAt := time.Now()
	if _, err := aliceGroup.Send(ctx, typedByAlice); err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	groupId, _ := waitPush(bob, 5*time.Second)
	latency := time.Since(sentAt)
	if !bytes.Equal(groupId, aliceGroup.Id()) {
		t.Fatalf("bob was pushed %x within 5s of alice's send, want the group %x", groupId, aliceGroup.Id())
	}
	if !receivedText(t, bobGroup, typedByAlice) {
		t.Fatalf("the Receive bob answered the push with did not read %q", typedByAlice)
	}
	if world.peer.Stats().PushesSent == 0 {
		t.Error("the peer counts no push sent")
	}
	t.Logf("a push reached bob %v after alice's Send returned; subscriptions %+v; peer pushes sent %d",
		latency.Round(time.Millisecond), world.subscriptions.Stats(), world.peer.Stats().PushesSent)
}

func TestAfterItsSessionIsCutTheDeviceSaysHelloAgainAndThePushesResume(t *testing.T) {
	world := newEndpointWorld(t)
	alice := world.device(t, "alice")
	bob, bobRoute, bobTransport := world.deviceAndRoute(t, "bob")
	aliceGroup, bobGroup := foundAndJoin(t, alice, bob)
	ctx := context.Background()
	if subscribed, err := bobGroup.EnsureSubscribed(ctx); err != nil || !subscribed {
		t.Fatalf("bob's EnsureSubscribed answered %v, %v", subscribed, err)
	}
	nonceEpoch := bobTransport.NonceEpoch()

	// what an exit provider leaving the mesh looks like to the server: every session, gone
	if dropped := world.listener.dropAll(); dropped < 2 {
		t.Fatalf("dropped %d sessions, want both devices'", dropped)
	}

	// BOB SENDS NOTHING. The route redials, and bob's device is WOKEN with the empty id that says
	// its session was replaced -- the wake the app's push waiter turns into a turn of its loop
	woke, woken := waitPush(bob, 20*time.Second)
	if !woken || len(woke) != 0 {
		t.Fatalf("after the cut bob's device was woken %v with %x; want the empty id of a replaced session (route %+v)",
			woken, woke, bobRoute.Status())
	}
	if status := bobRoute.Status(); status.Connects < 2 {
		t.Fatalf("bob's route reports %d sessions after a cut, want a second", status.Connects)
	}
	// the loop answers any wake with EnsureSubscribed, which says the owed Hello FIRST, and the new
	// connection holds no subscription, which the Hello's new nonce epoch is what reveals
	if subscribed, err := bobGroup.EnsureSubscribed(ctx); err != nil || !subscribed {
		t.Fatalf("after the wake EnsureSubscribed answered %v, %v; want a new subscription", subscribed, err)
	}
	if bobTransport.NonceEpoch() == nonceEpoch {
		t.Fatal("EnsureSubscribed resubscribed without the Hello the replaced session was owed")
	}

	// THE PROPERTY: alice sends, bob is pushed, and bob's Receive reads it, all without bob sending
	if _, err := aliceGroup.Send(ctx, typedByBob); err != nil {
		t.Fatalf("alice's Send after the cut: %v", err)
	}
	if groupId, _ := waitPush(bob, 10*time.Second); !bytes.Equal(groupId, aliceGroup.Id()) {
		t.Fatalf("bob was pushed %x after the cut, want the group %x", groupId, aliceGroup.Id())
	}
	if !receivedText(t, bobGroup, typedByBob) {
		t.Fatalf("bob's Receive after the cut did not read %q", typedByBob)
	}
	t.Logf("after the cut: bob's route at %d sessions, Hello count %d -> %d, and the push resumed",
		bobRoute.Status().Connects, nonceEpoch, bobTransport.NonceEpoch())
}
