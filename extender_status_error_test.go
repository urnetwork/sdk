//go:build !ios && !android && !js

package sdk

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A feed pass pauses after publishing its status. Wake can bypass any timer
// backoff and replace a failed dial with an empty-candidate result, so neither
// a long backoff nor polling supplies the failed-pass observation boundary.
type testExtenderStatusPassFixture struct {
	ctx           context.Context
	cancel        context.CancelFunc
	space         *NetworkSpace
	networkClient *connect.ExtenderNetworkClient
	dialer        *testSeedDialer
	passReached   chan time.Duration
	passRelease   chan struct{}
}

// All real client operations stay on the recorded offline dialer. The first
// empty pass is held before the test installs its signed candidate.
func newTestExtenderStatusPassFixture(t *testing.T) *testExtenderStatusPassFixture {
	t.Helper()
	testSeedEnableBlockedNetworkClient(t, false)
	extenderSeedLoopDisabled.Store(true)
	testSeedInstallEmbedded(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	fixture := &testExtenderStatusPassFixture{
		ctx: ctx, cancel: cancel, dialer: newTestSeedDialer(),
		passReached: make(chan time.Duration), passRelease: make(chan struct{}),
	}
	testSeedInstallConnectSettings(t, fixture.dialer, nil)
	previousConfigure := extenderNetworkClientConfigure
	extenderNetworkClientConfigure = func(settings *connect.ExtenderNetworkClientSettings) {
		previousConfigure(settings)
		settings.PassAfter = func(wait time.Duration) <-chan time.Time {
			select {
			case fixture.passReached <- wait:
			case <-ctx.Done():
				return nil
			}
			select {
			case <-fixture.passRelease:
			case <-ctx.Done():
			}
			return nil
		}
	}
	t.Cleanup(func() { extenderNetworkClientConfigure = previousConfigure })
	fixture.space = newTestSeedSpace(t, testSeedStoragePath(t))
	// Cancel the pass barrier before newTestSeedSpace joins the real client.
	t.Cleanup(cancel)
	fixture.networkClient = fixture.space.getExtenderNetworkClient()
	if fixture.networkClient == nil {
		t.Fatal("the space runs no network client")
	}
	fixture.awaitPass(t)
	if status := fixture.space.GetExtenderStatus(); status.LastError != "no extender candidate" || !fixture.networkClient.Status().InitialAttemptDone {
		t.Fatalf("the initial empty pass is not at its completed boundary: %+v", status)
	}
	return fixture
}

// Deadlines only bound a broken fixture; the pass channel establishes order.
func (self *testExtenderStatusPassFixture) awaitPass(t *testing.T) {
	t.Helper()
	select {
	case <-self.passReached:
	case <-time.After(10 * time.Second):
		t.Fatal("the extender pass did not reach its controlled status boundary")
	}
}

// Resume only a pass already observed at its boundary; timers remain disabled.
func (self *testExtenderStatusPassFixture) releasePass(t *testing.T) {
	t.Helper()
	select {
	case self.passRelease <- struct{}{}:
	case <-self.ctx.Done():
		t.Fatal("the extender pass fixture was canceled before release")
	}
}

// A real refused feed dial holds the candidate. Its status is read before any
// queued wake can run a subsequent empty pass.
func (self *testExtenderStatusPassFixture) failFeed(t *testing.T) string {
	t.Helper()
	now := time.Now()
	record := testSeedRecord(t, testSeedRecordSpec{
		index: 0, issueTime: now.Add(-time.Hour), expireTime: now.Add(24 * time.Hour),
	})
	if _, err := self.space.extenderDirectory.ApplyRecord(record, connect.ExtenderSourceFeed); err != nil {
		t.Fatal(err)
	}
	self.networkClient.Wake()
	self.releasePass(t)
	self.awaitPass(t)
	entry := testSeedDirectoryEntry(self.space, 0)
	if entry == nil || entry.FailureCount != 1 || entry.State != connect.ExtenderStateHold ||
		!self.dialer.dialedAddress(net.JoinHostPort(testSeedExtenderIp(0).String(), "443")) {
		t.Fatal("the controlled pass did not actually refuse and hold its signed feed candidate")
	}
	return self.space.GetExtenderStatus().LastError
}

// A failed feed dial must lose its address before the app reads the status.
// Observe the actual failed pass, since LastError describes the latest failed
// sample, including a later pass that could dial no candidate.
func TestExtenderStatusLastErrorNamesNoAddress(t *testing.T) {
	fixture := newTestExtenderStatusPassFixture(t)
	lastError := fixture.failFeed(t)
	if !strings.Contains(lastError, "refused") {
		t.Fatalf("the status is not about the dial: %q", lastError)
	}
	if strings.Contains(lastError, testSeedExtenderIp(0).String()) {
		t.Fatalf("the status names the address: %q", lastError)
	}
	if lastError != fixture.networkClient.Status().LastError {
		t.Fatal("the SDK status did not carry the actual failed-pass error")
	}
}

// A queued wake replaces a held candidate's dial failure with the next empty
// result. No timer or polling race is needed to reproduce that transition.
func TestExtenderStatusWakeReplacesFailedPass(t *testing.T) {
	fixture := newTestExtenderStatusPassFixture(t)
	failed := fixture.failFeed(t)
	if !strings.Contains(failed, "refused") {
		t.Fatalf("the controlled feed failure was not published: %q", failed)
	}
	fixture.networkClient.Wake()
	if status := fixture.space.GetExtenderStatus(); status.LastError != failed {
		t.Fatal("the queued wake crossed the held pass boundary")
	}
	fixture.releasePass(t)
	fixture.awaitPass(t)
	if status := fixture.space.GetExtenderStatus(); status.LastError != "no extender candidate" || status.LastError != fixture.networkClient.Status().LastError {
		t.Fatalf("the next empty pass retained an old dial failure: %+v", status)
	}
	if entry := testSeedDirectoryEntry(fixture.space, 0); entry == nil || entry.FailureCount != 1 {
		t.Fatal("the empty pass dialed the held candidate again")
	}
}

// Canceling at the pass barrier must let NetworkSpace close and join the
// client, even when the test never releases its waiting pass.
func TestExtenderStatusPassBarrierCancellationJoins(t *testing.T) {
	fixture := newTestExtenderStatusPassFixture(t)
	fixture.cancel()
	fixture.space.close()
	if fixture.space.getExtenderNetworkClient() != nil || fixture.space.ctx.Err() == nil {
		t.Fatal("the canceled pass barrier retained its network-space client")
	}
	if len(fixture.dialer.dialed()) != 0 {
		t.Fatal("an initial empty pass made a feed dial")
	}
}
