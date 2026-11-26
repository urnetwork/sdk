//go:build !ios_extension && !js && !ios && !android

// The app's sidecar role's clock check (sidecar_check_clock.go; owner decision 803, QD-1427), on a role clock whose
// wall time jumps while its timers stand still, as a device's does across a sleep: an app listener's stream whose grant
// the device slept past stays open on its standing wait until Sidecar.CheckClock, which reaches the core's check and
// through it the sockets component, waiting for the enrollment as the sdk's factory makes it wait, and ends the stream
// at once; checks from many goroutines and across Close are safe, and none reaches the core once it has closed; and on
// the remote backend the call asks the daemon nothing, since ur daemon checks its own clock.

package sdk

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// The component the sdk's sockets factory builds takes the core's check (sidecar's core_clock_check.go), so the
// stand-in below holds its place.
var _ sidecar.SidecarClockComponent = (*sidecar.SocketComponent)(nil)

// A stand-in for the core's sockets component as an app's sidecar role runs it (sidecar's socket_listen.go): one
// listener's streams, each reset at its grant's end, fixed at admission, by a wait on the role's clock, which a check
// arms again from the clock's now. A stream on the core's own component needs a network, the sockets service's ticket
// and a dialer over connect, none of which the sdk's tests have; sidecar's core_clock_check_test.go proves that
// component's reset at the check on its listeners, and these tests the sdk's call reaching it through the core. Safe
// for concurrent use.
type testingClockListener struct {
	clock sidecar.Clock
	// the core's checks that reached it
	checks atomic.Int64

	stateLock sync.Mutex
	streams   map[*testingClockListenerStream]bool
}

// One stream the listener holds, ended once.
type testingClockListenerStream struct {
	grantEnd time.Time
	// closed when the stream ends
	ended   chan struct{}
	endOnce sync.Once
	// stops the wait armed for grantEnd, under the listener's lock
	stop func() bool
}

func newTestingClockListener(clock sidecar.Clock) *testingClockListener {
	return &testingClockListener{
		clock:   clock,
		streams: map[*testingClockListenerStream]bool{},
	}
}

func (self *testingClockListener) Name() string {
	return SidecarComponentSockets
}

func (self *testingClockListener) Attach(env *sidecar.SidecarEnv) error {
	return nil
}

func (self *testingClockListener) Close() {}

// See sidecar.SidecarClockComponent: arms each live stream's wait again from the clock's now.
func (self *testingClockListener) CheckClock() {
	self.checks.Add(1)
	streams := func() []*testingClockListenerStream {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		streams := []*testingClockListenerStream{}
		for stream := range self.streams {
			streams = append(streams, stream)
		}
		return streams
	}()
	for _, stream := range streams {
		self.arm(stream)
	}
}

// A stream admitted now whose grant ends at grantEnd, its reset armed on the clock.
func (self *testingClockListener) open(grantEnd time.Time) *testingClockListenerStream {
	stream := &testingClockListenerStream{grantEnd: grantEnd, ended: make(chan struct{})}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.streams[stream] = true
	}()
	self.arm(stream)
	return stream
}

// Arms stream's end at its grant's end, counted from the clock's now, in place of the wait armed before; one already
// passed ends it at once.
func (self *testingClockListener) arm(stream *testingClockListenerStream) {
	previous, live := func() (func() bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		previous := stream.stop
		stream.stop = nil
		return previous, self.streams[stream]
	}()
	if previous != nil {
		previous()
	}
	if !live {
		return
	}
	stop := self.clock.AfterFunc(stream.grantEnd.Sub(self.clock.Now()), func() {
		self.end(stream)
	})
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		stream.stop = stop
	}()
}

// Takes stream out and ends it, once.
func (self *testingClockListener) end(stream *testingClockListenerStream) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.streams, stream)
	}()
	stream.endOnce.Do(func() {
		close(stream.ended)
	})
}

// Whether stream has ended.
func (self *testingClockListenerStream) isEnded() bool {
	select {
	case <-self.ended:
		return true
	default:
		return false
	}
}

// Where these tests' clock starts: a fixed date, so a run repeats.
func testingClockCheckStart() time.Time {
	return time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
}

// Settings that name the sockets component.
func testingClockCheckSettings() *SidecarSettings {
	settings := DefaultSidecarSettings()
	settings.Components.Add(SidecarComponentSockets)
	return settings
}

// An app listener's stream whose grant ends ten minutes on: the device sleeps an hour, its wall clock passing the end
// while the stream's wait stands still, and the stream stays open with no check; Sidecar.CheckClock reaches the core's
// check, which reaches the sockets component, built once the enrollment came as the sdk's factory builds it, and the
// stream ends at once, while one whose grant ends three hours on stays. Before the component is built a check reaches
// nothing of it.
func TestSidecarCheckClockEndsAnAppListenerStreamTheDeviceSleptPast(t *testing.T) {
	start := testingClockCheckStart()
	clock := &testingSleepingClock{awake: newTestingForwardClock(start)}
	listener := newTestingClockListener(clock)
	var enrolled atomic.Bool
	world := newTestSidecarWorld(t)
	local := world.openWith(t, testingClockCheckSettings(), &sidecarLocalOptions{componentFactories: map[string]sidecarComponentFactory{
		SidecarComponentSockets: func(ctx context.Context, local *sidecar.Sidecar, settings *SidecarSettings) (sidecar.SidecarComponent, error) {
			// as the sdk's own factory: the component waits for the core's enrollment
			if !enrolled.Load() {
				return nil, errSidecarComponentNotEnrolled
			}
			return listener, nil
		},
	}})

	local.CheckClock()
	if checks := listener.checks.Load(); checks != 0 {
		t.Fatalf("a check before the component was built reached it %d times", checks)
	}
	enrolled.Store(true)
	if waiting := local.addDeferredComponents(local.Core()); len(waiting) != 0 || local.component(SidecarComponentSockets) != listener {
		t.Fatalf("waiting %v, component %v", waiting, local.component(SidecarComponentSockets))
	}

	near := listener.open(start.Add(10 * time.Minute))
	far := listener.open(start.Add(3 * time.Hour))
	clock.sleep(time.Hour)
	if near.isEnded() || far.isEnded() {
		t.Fatal("a stream ended across the sleep with no check")
	}

	local.CheckClock()
	if checks := listener.checks.Load(); checks != 1 {
		t.Fatalf("one check reached the sockets component %d times", checks)
	}
	select {
	case <-near.ended:
	case <-time.After(testSidecarTimeout):
		t.Fatal("the check left open the stream the device slept past")
	}
	if far.isEnded() {
		t.Error("the check ended a stream whose grant has two hours left")
	}
}

// Checks from many goroutines, while the Sidecar closes, are safe and each reaches the core at most once; once Close
// has returned none reaches it.
func TestSidecarCheckClockFromManyGoroutinesAndAfterClose(t *testing.T) {
	listener := newTestingClockListener(&testingSleepingClock{awake: newTestingForwardClock(testingClockCheckStart())})
	world := newTestSidecarWorld(t)
	local := world.openWith(t, testingClockCheckSettings(), &sidecarLocalOptions{componentFactories: map[string]sidecarComponentFactory{
		SidecarComponentSockets: func(ctx context.Context, local *sidecar.Sidecar, settings *SidecarSettings) (sidecar.SidecarComponent, error) {
			return listener, nil
		},
	}})
	if local.component(SidecarComponentSockets) != listener {
		t.Fatal("the component was not built at once")
	}

	local.CheckClock()
	if checks := listener.checks.Load(); checks != 1 {
		t.Fatalf("one check reached the sockets component %d times", checks)
	}
	const goroutineCount = 8
	const checkCount = 64
	callCount := atomic.Int64{}
	started := make(chan struct{}, goroutineCount)
	var checking sync.WaitGroup
	for range goroutineCount {
		checking.Add(1)
		go func() {
			defer checking.Done()
			started <- struct{}{}
			for range checkCount {
				local.CheckClock()
				callCount.Add(1)
			}
		}()
	}
	for range goroutineCount {
		<-started
	}
	world.close(t, local)
	checking.Wait()
	afterClose := listener.checks.Load()
	if afterClose < 1 || 1+callCount.Load() < afterClose {
		t.Fatalf("%d checks reached the component for %d calls", afterClose-1, callCount.Load())
	}
	local.CheckClock()
	if checks := listener.checks.Load(); checks != afterClose {
		t.Errorf("a check after Close reached the component: %d, then %d", afterClose, checks)
	}
}

// On the remote backend the daemon checks its own clock: the call asks it nothing, and a remote Sidecar holds no core.
func TestSidecarCheckClockOnTheRemoteBackendAsksTheDaemonNothing(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	if remote.Core() != nil {
		t.Fatal("a remote Sidecar holds a core")
	}
	before := len(daemon.seenMethods())
	remote.CheckClock()
	// a call the check made would be answered, so seen, before it returned; the events long-poll may add its own
	for _, method := range daemon.seenMethods()[before:] {
		if method != sidecarControlMethodEvents {
			t.Errorf("the check asked the daemon %s", method)
		}
	}
}
