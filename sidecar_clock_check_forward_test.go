//go:build !js

// The forwarding roles' clock (sidecar_clock_check_forward.go; QD-1422), on a role clock whose wall time jumps while
// its timers stand still, as a device's does across a sleep: DeviceLocal.CheckClock refreshes F at once once the
// device slept past its refresh point, which its standing wait would have reached only after the remaining awake time,
// and has the role's Forwarder follow what came due; and a desktop role checks itself each of the bridges'
// ClockCheckInterval, so a follow the device slept past comes at the next interval with no call at all.

package sdk

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	sidecardevice "github.com/urnetwork/sidecar/device"
)

// A clock whose wall time can jump while its timers stand still: Now reads the awake clock's time plus the time slept,
// and a timer runs on the awake clock beneath, as Go's runtime timers count only the time a device is awake. A
// timeout that is not positive fires at once, as sidecar.Clock says. Safe for concurrent use.
type testingSleepingClock struct {
	awake *testingForwardClock

	stateLock sync.Mutex
	slept     time.Duration
}

func (self *testingSleepingClock) Now() time.Time {
	self.stateLock.Lock()
	slept := self.slept
	self.stateLock.Unlock()
	return self.awake.Now().Add(slept)
}

func (self *testingSleepingClock) After(timeout time.Duration) <-chan time.Time {
	if timeout <= 0 {
		fired := make(chan time.Time, 1)
		fired <- self.Now()
		return fired
	}
	return self.awake.After(timeout)
}

func (self *testingSleepingClock) AfterFunc(timeout time.Duration, function func()) func() bool {
	if timeout <= 0 {
		go function()
		return func() bool { return false }
	}
	return self.awake.AfterFunc(timeout, function)
}

// Sleeps for timeout: the wall clock moves on, and no timer fires.
func (self *testingSleepingClock) sleep(timeout time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.slept += timeout
}

// Answers every request 503, as testingForwardTransport does, and tells the test of each read of the projection, a
// follow's.
type testingForwardProjectionTransport struct {
	reads chan struct{}
}

func (self *testingForwardProjectionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/network/config/projection") {
		self.reads <- struct{}{}
	}
	return testingForwardTransport{}.RoundTrip(request)
}

// Waits for the role's Forwarder's next read of the projection.
func (self *testingForwardProjectionTransport) next(t *testing.T) {
	t.Helper()
	select {
	case <-self.reads:
	case <-time.After(testSidecarTimeout):
		t.Fatal("the Forwarder did not follow")
	}
}

// Takes every read made so far, once the loop that made them has answered a witness after them: none can come then
// until something wakes the Forwarder.
func (self *testingForwardProjectionTransport) drain() {
	for {
		select {
		case <-self.reads:
		default:
			return
		}
	}
}

// Fails on a read since the last, which a role whose loops all wait cannot make.
func (self *testingForwardProjectionTransport) expectNone(t *testing.T, what string) {
	t.Helper()
	select {
	case <-self.reads:
		t.Fatalf("%s: the Forwarder followed", what)
	default:
	}
}

// A device with one forwarding role running on the sleeping clock over a role clock at start, its F a token of a week
// from start, adjust changing the device's settings: the device and its network.
func testingClockCheckForwardDevice(t *testing.T, forward *testingForwardDevice, start time.Time, adjust func(settings *DeviceLocalSettings)) (*DeviceLocal, connect.Id) {
	t.Helper()
	device := forward.device(t, adjust)
	networkId := forward.role.vectors.networkId()
	keys, err := device.forwardingHost().keys(networkId)
	if err != nil {
		t.Fatal(err)
	}
	handOff := forward.identity.handOff(t, keys, forward.pending(), nil)
	handOff.ForwardClientJwt = testingForwardJwt(t, start.Unix(), int64(7*24*60*60))
	if err := device.forwardingHost().handOff(handOff); err != nil {
		t.Fatal(err)
	}
	testingForwardStarted(t, device, networkId)
	return device, networkId
}

// F's refresh point, between 40% and 50% of its week, the device slept past, four days on and before F's exp: the
// refresh loop's wait stood on the awake clock, so no refresh was asked, and the role would have lapsed at the exp
// before its timer's remaining awake time ran out. DeviceLocal.CheckClock has the loop arm its wait anew from the wall
// clock, so F refreshes at once, presenting its token; and so again past the new token's point, presenting the new
// token, which it kept, with the role never lapsed.
func TestDeviceLocalCheckClockRefreshesFTheDeviceSleptPast(t *testing.T) {
	forward := newTestingForwardDevice(t)
	start := time.Unix(1800000000, 0)
	clock := &testingSleepingClock{awake: newTestingForwardClock(start)}
	transport := &testingForwardRefreshTransport{requests: make(chan *testingForwardRefresh)}
	forward.clock, forward.transport = clock, transport
	device, networkId := testingClockCheckForwardDevice(t, forward, start, nil)
	role, _ := device.forwardingHost().role(networkId, false)
	first, _ := role.keys.token()
	week := int64(7 * 24 * 60 * 60)

	clock.sleep(4 * 24 * time.Hour)
	select {
	case <-transport.requests:
		t.Fatal("F refreshed with no check")
	default:
	}
	device.CheckClock()
	refresh := transport.next(t)
	if bearer := refresh.request.Header.Get("Authorization"); bearer != "Bearer "+first {
		t.Errorf("the refresh presented %q", bearer)
	}
	second := testingForwardJwt(t, start.Unix()+4*24*60*60, week)
	refresh.answer <- second

	// past the new token's point, a little over seven days on, and before its exp
	clock.sleep(4 * 24 * time.Hour)
	device.CheckClock()
	refresh = transport.next(t)
	if bearer := refresh.request.Header.Get("Authorization"); bearer != "Bearer "+second {
		t.Errorf("the next refresh presented %q", bearer)
	}
	if kept, _ := role.keys.token(); kept != second {
		t.Error("the new token was presented before it was kept")
	}
	refresh.answer <- testingForwardJwt(t, start.Unix()+8*24*60*60, week)
	if status := role.status(); status.Lapsed {
		t.Errorf("a refreshed role lapsed: %+v", status)
	}
}

// The role's Forwarder's next follow the device slept past, after those its start and the witness made: no follow with
// no check, its wait standing on the awake clock; DeviceLocal.CheckClock has it follow at once.
func TestDeviceLocalCheckClockReachesTheRolesForwarder(t *testing.T) {
	forward := newTestingForwardDevice(t)
	start := time.Unix(1800000000, 0)
	clock := &testingSleepingClock{awake: newTestingForwardClock(start)}
	transport := &testingForwardProjectionTransport{reads: make(chan struct{}, 64)}
	forward.clock, forward.transport = clock, transport
	device, _ := testingClockCheckForwardDevice(t, forward, start, nil)
	transport.drain()

	clock.sleep(time.Hour)
	transport.expectNone(t, "slept past the follow with no check")
	device.CheckClock()
	transport.next(t)
}

// A desktop role checks its own clock each of the bridges' ClockCheckInterval: the Forwarder's next follow is 20 min
// of awake time away, its retries taking 10 min and doubling, and the device slept an hour past it; the awake clock
// moving one interval brings the follow, with no call of CheckClock.
func TestDesktopForwardRoleChecksItsClockEachInterval(t *testing.T) {
	interval := sidecardevice.DefaultBridgeSettings().ClockCheckInterval
	if interval <= 0 || 10*time.Minute <= interval {
		t.Fatalf("a desktop's cap is %v", interval)
	}
	forward := newTestingForwardDevice(t)
	start := time.Unix(1800000000, 0)
	awake := newTestingForwardClock(start)
	clock := &testingSleepingClock{awake: awake}
	transport := &testingForwardProjectionTransport{reads: make(chan struct{}, 64)}
	forward.clock, forward.transport = clock, transport
	device, networkId := testingClockCheckForwardDevice(t, forward, start, func(settings *DeviceLocalSettings) {
		forwardingSettings := defaultDeviceForwardingSettings()
		forwardingSettings.Forwarder = sidecar.DefaultForwarderSettings(sidecar.ForwarderHostDesktop)
		forwardingSettings.Forwarder.FollowRetryInterval = 10 * time.Minute
		settings.forwardingSettings = forwardingSettings
	})
	if role, _ := device.forwardingHost().role(networkId, false); role.host.bridgeSettings.ClockCheckInterval != interval {
		t.Fatalf("this device's role takes a cap of %v", role.host.bridgeSettings.ClockCheckInterval)
	}
	transport.drain()

	clock.sleep(time.Hour)
	transport.expectNone(t, "slept past the follow")
	awake.Advance(interval)
	transport.next(t)
}
