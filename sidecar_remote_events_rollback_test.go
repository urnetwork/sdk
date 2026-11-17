//go:build !ios_extension && !js && !ios && !android

package sdk

// The push's rollback head_alarm (QD-928, owner decision 300) through the daemon: a daemon's sidecar raises it, the
// daemon's events ring keeps it, and a remote-mode app's listener gets it with its kind and both TopicVersions, as the
// js leg's conversion of the daemon's answer does.

import (
	"context"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A rollback head_alarm a daemon's sidecar raises reaches a remote-mode app's listener through the daemon's events
// ring with its kind and both TopicVersions, the held one first, and the daemon's own answer carries the same to the
// js leg.
func TestRemoteRollbackHeadAlarmReachesTheListener(t *testing.T) {
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

	source.raise(testRollbackCoreEvent(t))
	testCheckRollbackEvent(t, events.next(t))

	answered := &sidecarControlEventsReply{}
	if err := client.Call(ctx, sidecarControlMethodEvents, &sidecarControlEventsParams{After: start.Cursor}, false, answered); err != nil {
		t.Fatal(err)
	}
	if len(answered.Events) != 1 {
		t.Fatalf("the daemon answered %d events", len(answered.Events))
	}
	testCheckRollbackEvent(t, answered.Events[0].boundEvent())
}
