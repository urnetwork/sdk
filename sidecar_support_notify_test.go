//go:build !ios_extension && !js

package sdk

// The support push topic's side of the apps' conversation (sidecar_support_notify.go; DESIGN-COMPUTE §28.9, FB section
// 7): a Sidecar with no core answers an error from each call; with the release's switch off the local core's Wake
// answers that support is unavailable; on the remote backend Wake answers at once, the daemon reading on its own push,
// the notification listener hears support's replies and moves alone, with the person's unread replies the daemon's
// stubs count, and Unread sums them. Synthetic values only.

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// A wake's one answer.
type testSupportWakeCallback struct {
	answered chan *SidecarResult
}

func (self *testSupportWakeCallback) Result(result *SidecarResult, err error) {
	if err != nil {
		result = &SidecarResult{Message: err.Error()}
	}
	self.answered <- result
}

func (self *testSupportWakeCallback) await(t *testing.T) *SidecarResult {
	t.Helper()
	select {
	case result := <-self.answered:
		return result
	case <-time.After(30 * time.Second):
		t.Fatal("the wake never answered")
		return nil
	}
}

// A notification listener's notifications, each decoded, and a signal for each.
type testSupportNotificationListener struct {
	stateLock     sync.Mutex
	notifications []map[string]any
	heard         chan struct{}
}

func (self *testSupportNotificationListener) OnNotification(notificationJson string) {
	notification := map[string]any{}
	if err := json.Unmarshal([]byte(notificationJson), &notification); err != nil {
		notification = map[string]any{"undecodable": notificationJson}
	}
	self.stateLock.Lock()
	self.notifications = append(self.notifications, notification)
	self.stateLock.Unlock()
	self.heard <- struct{}{}
}

func (self *testSupportNotificationListener) awaitCount(t *testing.T, count int) []map[string]any {
	t.Helper()
	for range count {
		select {
		case <-self.heard:
		case <-time.After(30 * time.Second):
			t.Fatal("the listener heard too little")
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]map[string]any{}, self.notifications...)
}

// A Sidecar with no core answers an error from each call, never a wake that read or an unread count of nothing.
func TestSidecarSupportNotifyOnNoCoreAnswersAnError(t *testing.T) {
	support := (&Sidecar{}).Support()
	if _, err := support.AddNotificationListener(&testSupportNotificationListener{heard: make(chan struct{}, 1)}); !errors.Is(err, errSidecarSupportNoCore) {
		t.Fatalf("AddNotificationListener: %v", err)
	}
	if _, err := support.Unread(); !errors.Is(err, errSidecarSupportNoCore) {
		t.Fatalf("Unread: %v", err)
	}
	callback := &testSupportWakeCallback{answered: make(chan *SidecarResult, 1)}
	support.Wake(callback)
	if result := callback.await(t); result.Ok || result.Message == "" {
		t.Fatalf("Wake answered %+v", result)
	}
}

// Until G-SUP-service passes the release's switch is off, and an app's support wake answers that support is reached
// through the app's form, having read nothing. The class an app's push handler compares is §18.7's support.
func TestSidecarSupportWakeAnswersUnavailableWhileTheSwitchIsOff(t *testing.T) {
	world := newTestSidecarWorld(t)
	support := world.open(t).Support()
	callback := &testSupportWakeCallback{answered: make(chan *SidecarResult, 1)}
	support.Wake(callback)
	if result := callback.await(t); result.Ok || result.Message != sidecar.ErrSupportUnavailable.Error() {
		t.Fatalf("Wake answered %+v", result)
	}
	if wakeClass := SidecarWakeClassSupport; wakeClass != "support" {
		t.Fatalf("the wake class %q", wakeClass)
	}
}

// On the remote backend the daemon holds its own push and reads on it: Wake answers at once and asks the daemon
// nothing; Unread sums the person's unread replies across the daemon's stubs.
func TestSidecarSupportRemoteWakeAndUnread(t *testing.T) {
	daemon := newTestSupportDaemon(map[string]any{
		sidecarControlMethodSupportList: &sidecarControlSupportListReply{Items: []*sidecar.SupportItemStub{
			{Entry: "100171", Status: "in_progress", Replies: 2, UnreadReplies: 1},
			{Entry: "100172", Status: "fixed", Replies: 3, UnreadReplies: 2},
			nil,
		}},
	})
	support := (&Sidecar{remote: daemon}).Support()
	callback := &testSupportWakeCallback{answered: make(chan *SidecarResult, 1)}
	support.Wake(callback)
	if result := callback.await(t); !result.Ok || result.Value != "0" {
		t.Fatalf("Wake answered %+v", result)
	}
	if calls := daemon.taken(); len(calls) != 0 {
		t.Fatalf("Wake called the daemon: %+v", calls)
	}
	unread, err := support.Unread()
	if err != nil || unread != 3 {
		t.Fatalf("Unread: %d, %v", unread, err)
	}
	if listed := daemon.last(t); listed.method != sidecarControlMethodSupportList || listed.confirmed ||
		testSupportParamsOf(t, listed).Network != "the paired network" {
		t.Fatalf("Unread called %+v", listed)
	}
}

// The notification listener hears support's reply and its move, each its enumerated fields with the person's unread
// replies, and nothing of the person's own message, an agent's or a message given up; closing its Sub ends the watch.
func TestSidecarSupportRemoteNotificationsKeepSupportsRepliesAndMoves(t *testing.T) {
	daemon := newTestSupportDaemon(map[string]any{
		sidecarControlMethodSupportSettings: &sidecar.SupportPersonKey{Agents: true, AgentLogs: true},
		sidecarControlMethodSupportList: &sidecarControlSupportListReply{Items: []*sidecar.SupportItemStub{
			{Entry: "100171", Status: "fixed", Replies: 1, UnreadReplies: 1},
		}},
	})
	support := (&Sidecar{remote: daemon}).Support()
	listener := &testSupportNotificationListener{heard: make(chan struct{}, 8)}
	sub, err := support.AddNotificationListener(listener)
	if err != nil {
		t.Fatal(err)
	}
	daemon.updates <- []*sidecar.SupportUpdate{
		{Product: "urnetwork", Entry: "100171", Seq: 1, Type: "submitted", Status: "received", Agents: []string{"release-bot"}},
		{Product: "urnetwork", Entry: "100171", Seq: 2, Type: "note", Status: "in_progress", Agents: []string{"release-bot"}},
		{Product: "urnetwork", Entry: "100171", Seq: 3, Type: "followup", Status: "in_progress"},
		{Product: "urnetwork", Entry: "100171", Type: "not_sent"},
		{Product: "urnetwork", Entry: "100171", Seq: 4, Type: "status", Status: "fixed", FixedIn: "2026.11.30-100", Final: true},
	}
	notifications := listener.awaitCount(t, 2)
	if len(notifications) != 2 {
		t.Fatalf("notifications %v", notifications)
	}
	reply, moved := notifications[0], notifications[1]
	if reply["kind"] != sidecar.SupportNotificationReply || reply["entry"] != "100171" || reply["seq"] != float64(2) || reply["unread"] != float64(1) {
		t.Fatalf("the reply's notification %v", reply)
	}
	if moved["kind"] != sidecar.SupportNotificationStatus || moved["status"] != "fixed" || moved["fixed_in"] != "2026.11.30-100" ||
		moved["final"] != true || moved["seq"] != float64(4) {
		t.Fatalf("the move's notification %v", moved)
	}
	for _, notification := range notifications {
		encoded, _ := json.Marshal(notification)
		if strings.Contains(string(encoded), "release-bot") || strings.Contains(string(encoded), "agents") {
			t.Fatalf("a notification names an agent: %s", encoded)
		}
	}
	sub.Close()
	select {
	case <-daemon.sessionClosed:
	case <-time.After(30 * time.Second):
		t.Fatal("the watch's session stayed open after its Sub closed")
	}
}
