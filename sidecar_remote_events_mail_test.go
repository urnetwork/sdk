//go:build !ios_extension && !js && !ios && !android

package sdk

// mail_receiver_alarm (D-MAIL-15; MAIL-M7-25) through the daemon: a daemon's sidecar raises it for each of its
// reasons, the daemon's events ring keeps it, and a remote-mode app's listener gets it with its kind, receiver, repo
// and reason and nothing another kind fills, as the js leg's conversion of the daemon's answer does.

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A mail_receiver_alarm as the core's mail client raises it: the receiver or the record's signer, the repo and why.
func testMailReceiverAlarmCoreEvent(reason sidecar.SidecarMailReceiverAlarm, receiverId sidecar.Id, repoId sidecar.Id) sidecar.SidecarEvent {
	return sidecar.SidecarEvent{
		Kind:              sidecar.SidecarEventMailReceiverAlarm,
		Generation:        1,
		SidecarId:         &receiverId,
		RepoId:            &repoId,
		MailReceiverAlarm: reason,
	}
}

// Fails unless event is the mail_receiver_alarm for reason naming receiverId and repoId, as an app sees it, with no
// field another kind fills.
func testCheckMailReceiverAlarmEvent(t *testing.T, event *SidecarEvent, reason sidecar.SidecarMailReceiverAlarm, receiverId sidecar.Id, repoId sidecar.Id) {
	t.Helper()
	if event.Kind != "mail_receiver_alarm" || event.MailReceiverAlarm != string(reason) || event.Generation != 1 {
		t.Fatalf("the mail receiver alarm as an app sees it: %+v", event)
	}
	if event.SidecarId == nil || !bytes.Equal(event.SidecarId.Bytes(), receiverId.Bytes()) ||
		event.RepoId == nil || !bytes.Equal(event.RepoId.Bytes(), repoId.Bytes()) {
		t.Errorf("%s: the receiver %v and repo %v", reason, event.SidecarId, event.RepoId)
	}
	only := &SidecarEvent{
		Kind:              event.Kind,
		Generation:        event.Generation,
		SidecarId:         event.SidecarId,
		RepoId:            event.RepoId,
		MailReceiverAlarm: event.MailReceiverAlarm,
	}
	if !reflect.DeepEqual(event, only) {
		t.Errorf("%s: fields its kind does not fill: %+v", reason, event)
	}
}

// A mail_receiver_alarm of each reason a daemon's sidecar raises reaches a remote-mode app's listener through the
// daemon's events ring, in order, with its kind, receiver, repo and reason, and the daemon's own answer carries the
// same to the js leg.
func TestRemoteMailReceiverAlarmReachesTheListener(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	source := newTestEventsSource()
	startTestEventsDaemon(t, socketPath, source, 1, nil)
	dial := newTestEventsDial(sidecarDaemonSocketDial(socketPath, testRemoteDaemonVersion))
	_, _, events := openTestEventsSidecar(t, dial.dial, nil)
	close(dial.ready)
	// subscribed: the status read after the cursor finds the daemon online
	if first := events.next(t); first.Kind != "state" || first.State != "online" {
		t.Fatalf("the first event: %+v", first)
	}
	// a cursor of the test's own, to read the daemon's answer as the js leg does
	ctx, cancel := context.WithTimeout(context.Background(), testSidecarTimeout)
	defer cancel()
	client, err := sidecar.DialDaemon(ctx, socketPath, &sidecar.DaemonHello{Mode: sidecar.DaemonModeControl, Version: testRemoteDaemonVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	start := &sidecarControlEventsReply{}
	if err := client.Call(ctx, sidecarControlMethodEvents, &sidecarControlEventsParams{}, false, start); err != nil {
		t.Fatal(err)
	}

	reasons := []sidecar.SidecarMailReceiverAlarm{
		sidecar.SidecarMailReceiverAlarmUndesignated,
		sidecar.SidecarMailReceiverAlarmScope,
		sidecar.SidecarMailReceiverAlarmDeposit,
		sidecar.SidecarMailReceiverAlarmRecord,
	}
	receiverIds := []sidecar.Id{}
	repoIds := []sidecar.Id{}
	for _, reason := range reasons {
		receiverId, repoId := sidecar.NewId(), sidecar.NewId()
		receiverIds = append(receiverIds, receiverId)
		repoIds = append(repoIds, repoId)
		source.raise(testMailReceiverAlarmCoreEvent(reason, receiverId, repoId))
	}
	for i, reason := range reasons {
		testCheckMailReceiverAlarmEvent(t, events.next(t), reason, receiverIds[i], repoIds[i])
	}

	answered := &sidecarControlEventsReply{}
	if err := client.Call(ctx, sidecarControlMethodEvents, &sidecarControlEventsParams{After: start.Cursor}, false, answered); err != nil {
		t.Fatal(err)
	}
	if len(answered.Events) != len(reasons) {
		t.Fatalf("the daemon answered %d events of %d", len(answered.Events), len(reasons))
	}
	for i, reason := range reasons {
		testCheckMailReceiverAlarmEvent(t, answered.Events[i].boundEvent(), reason, receiverIds[i], repoIds[i])
	}
}
