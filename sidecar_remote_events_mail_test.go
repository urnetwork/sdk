//go:build !ios_extension && !js && !ios && !android

package sdk

// mail_receiver_alarm (D-MAIL-15; MAIL-M7-25) through the daemon: a daemon's sidecar raises it for each of its
// reasons, the daemon's events ring keeps it, and a remote-mode app's listener gets it with its kind, receiver, repo
// and reason and nothing another kind fills, as the js leg's conversion of the daemon's answer does. And the remote
// backend's ring event reads every field the daemon's DaemonEvent answers, so a field the core adds to it does not
// stop at the daemon on its way to an app.

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
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

// Sets every field of value, a struct of the daemon's wire, to a value other than its zero, and every field of each
// struct it points to. A field of a type with no value here fails the test, so a field of a new type is named here
// before anything passes over it.
func testSetEveryDaemonField(t *testing.T, value reflect.Value) {
	t.Helper()
	for i := 0; i < value.NumField(); i += 1 {
		field := value.Field(i)
		name := value.Type().Field(i).Name
		switch {
		case field.Kind() == reflect.String:
			field.SetString("synthetic-" + strings.ToLower(name))
		case field.Kind() == reflect.Bool:
			field.SetBool(true)
		case field.CanInt():
			field.SetInt(int64(100 + i))
		case field.CanUint():
			field.SetUint(uint64(200 + i))
		case field.Type() == reflect.TypeFor[[]byte]():
			field.SetBytes([]byte{byte(i), 1})
		case field.Type() == reflect.TypeFor[sidecar.Id]():
			field.Set(reflect.ValueOf(sidecar.NewId()))
		case field.Type() == reflect.TypeFor[*sidecar.Id]():
			id := sidecar.NewId()
			field.Set(reflect.ValueOf(&id))
		case field.Kind() == reflect.Pointer && field.Type().Elem().Kind() == reflect.Struct:
			field.Set(reflect.New(field.Type().Elem()))
			testSetEveryDaemonField(t, field.Elem())
		default:
			t.Fatalf("%s.%s's type %s has no synthetic value", value.Type().Name(), name, field.Type())
		}
	}
}

// The remote backend's ring event reads every field of the daemon's DaemonEvent under the daemon's own name: a daemon
// event with every field set, its erasure's too, crosses into it with unknown fields refused and comes back as the
// same json. A field the core adds to the daemon's event fails here until the remote backend reads it, as
// mail_receiver_alarm would have.
func TestRemoteControlEventReadsEveryDaemonField(t *testing.T) {
	daemon := &sidecar.DaemonEvent{}
	testSetEveryDaemonField(t, reflect.ValueOf(daemon).Elem())
	if daemon.MailReceiverAlarm == "" || daemon.Erasure == nil || daemon.Erasure.FinishMs == 0 {
		t.Fatalf("CONTROL FAILED: the daemon's event %+v", daemon)
	}
	first, err := json.Marshal(daemon)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(first))
	decoder.DisallowUnknownFields()
	own := &sidecarControlEvent{}
	if err := decoder.Decode(own); err != nil {
		t.Fatalf("the remote backend's event does not read the daemon's: %v", err)
	}
	second, err := json.Marshal(own)
	if err != nil {
		t.Fatal(err)
	}
	fields := func(encoded []byte) map[string]any {
		decoded := map[string]any{}
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	if !reflect.DeepEqual(fields(first), fields(second)) {
		t.Errorf("the daemon's event changed across the remote backend's:\n%s\n%s", first, second)
	}
}
