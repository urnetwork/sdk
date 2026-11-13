package sdk

// transport_connect_test.go -- the connect half of the transport quality
// (transport_connect.go): a slow connect raises the level, a prompt one does
// not, connect's own dial pacing reads as no stall, recovery clears the level
// by age or by a prompt connect, connects hovering at the good stall do not
// flap it, held transports are no evidence, the DNS fallback, the generation
// fence, the defaults, the transport watcher, and the device wiring.
//
// The window tests place their events at times of their choosing, with no
// monotonic reading, and read what the listeners would hear through a delay
// window with no samples.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// testingTransportConnectCarrier stands in for a window platform transport.
// The test sets its state, and each change notifies as a registration does.
type testingTransportConnectCarrier struct {
	stateLock        sync.Mutex
	state            connect.PlatformTransportState
	waitingForBudget bool
	activeMode       connect.TransportMode
	connectedMonitor *connect.Monitor
	done             chan struct{}
	doneOnce         sync.Once
}

// A transport dialing its first connection.
func newTestingTransportConnectCarrier() *testingTransportConnectCarrier {
	return &testingTransportConnectCarrier{
		state:            connect.PlatformTransportStateConnecting,
		activeMode:       connect.TransportModeNone,
		connectedMonitor: connect.NewMonitor(),
		done:             make(chan struct{}),
	}
}

func (self *testingTransportConnectCarrier) State() connect.PlatformTransportState {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.state
}

func (self *testingTransportConnectCarrier) IsWaitingForBudget() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.waitingForBudget
}

func (self *testingTransportConnectCarrier) ActiveMode() connect.TransportMode {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.activeMode
}

func (self *testingTransportConnectCarrier) ConnectedNotify() <-chan struct{} {
	return self.connectedMonitor.NotifyChannel()
}

func (self *testingTransportConnectCarrier) Done() <-chan struct{} {
	return self.done
}

func (self *testingTransportConnectCarrier) set(state connect.PlatformTransportState, activeMode connect.TransportMode) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.state = state
		self.activeMode = activeMode
	}()
	self.connectedMonitor.NotifyAll()
}

// Waiting for budget changes no registration, so it notifies nothing.
func (self *testingTransportConnectCarrier) setWaitingForBudget(waitingForBudget bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.waitingForBudget = waitingForBudget
}

func (self *testingTransportConnectCarrier) close() {
	self.doneOnce.Do(func() {
		close(self.done)
	})
}

// A whole second with no monotonic reading, which the window tests count
// from.
func testTransportConnectBase() time.Time {
	return time.Unix(1_800_000_000, 0)
}

func testTransportConnectAt(base time.Time, seconds float64) time.Time {
	return base.Add(time.Duration(seconds * float64(time.Second)))
}

// A connect window taking the transports of generation 1 at the default
// settings, beside a delay window of the same generation, so a test reads what
// the listeners hear.
func testTransportConnectWindows() (*transportConnectWindow, *transportDelayWindow) {
	settings := testTransportDelaySettings()
	connectWindow := newTransportConnectWindow(context.Background(), settings, nil)
	connectWindow.reset(1)
	delayWindow, _ := testTransportDelayWindowWithSettings(settings)
	return connectWindow, delayWindow
}

// What the listeners hear at now: the delay window, which has no samples,
// moved by the connect window's score.
func testTransportConnectRead(connectWindow *transportConnectWindow, delayWindow *transportDelayWindow, now time.Time) *TransportDelay {
	score, _ := connectWindow.evaluate(now)
	delay, _ := delayWindow.evaluateWithConnect(now, score)
	return delay
}

// The level the apps draw: the worse direction's.
func testTransportConnectLevel(delay *TransportDelay) float64 {
	return max(delay.UpQuality, delay.DownQuality)
}

// count new transports of generation 1, dialing from now.
func testTransportConnectAdd(t *testing.T, window *transportConnectWindow, now time.Time, count int) []*testingTransportConnectCarrier {
	t.Helper()
	carriers := []*testingTransportConnectCarrier{}
	for range count {
		carrier := newTestingTransportConnectCarrier()
		if !window.add(1, carrier, now) {
			t.Fatal("the window dropped a transport of its generation")
		}
		carriers = append(carriers, carrier)
	}
	return carriers
}

// The transport connects over activeMode at now.
func testTransportConnectConnect(t *testing.T, window *transportConnectWindow, carrier *testingTransportConnectCarrier, activeMode connect.TransportMode, now time.Time) {
	t.Helper()
	carrier.set(connect.PlatformTransportStateConnected, activeMode)
	if !window.observe(1, carrier, now) {
		t.Fatal("the window lost a transport of its generation")
	}
}

// The transport loses its connection at now and dials again.
func testTransportConnectLose(t *testing.T, window *transportConnectWindow, carrier *testingTransportConnectCarrier, now time.Time) {
	t.Helper()
	carrier.set(connect.PlatformTransportStateConnecting, connect.TransportModeNone)
	if !window.observe(1, carrier, now) {
		t.Fatal("the window lost a transport of its generation")
	}
}

func testTransportConnectExpectLevel(t *testing.T, connectWindow *transportConnectWindow, delayWindow *transportDelayWindow, base time.Time, seconds float64, level float64) *TransportDelay {
	t.Helper()
	delay := testTransportConnectRead(connectWindow, delayWindow, testTransportConnectAt(base, seconds))
	if delay.UpQuality != level || delay.DownQuality != level {
		t.Fatalf("at %vs the levels are up %v down %v, expected %v: %+v", seconds, delay.UpQuality, delay.DownQuality, level, delay)
	}
	return delay
}

// No transport connects while the device is trying to connect (the owner's
// first example). The level rises once the stall passes the good stall,
// deepens as the stall grows, and is the worst from the bad stall on, in both
// directions, as the user's network and with no numbers. The window asks to be
// evaluated while the stall grows.
func TestTransportConnectSlowConnectRaisesTheLevel(t *testing.T) {
	connectWindow, delayWindow := testTransportConnectWindows()
	base := testTransportConnectBase()
	testTransportConnectAdd(t, connectWindow, base, 4)
	for _, c := range []struct {
		seconds float64
		level   float64
	}{
		{0, 0},
		{4.9, 0},
		{5, 0},
		// (stall - 5 s) / 10 s, quantized up to 0.25 steps
		{5.1, 0.25},
		{7.5, 0.25},
		{8, 0.5},
		{10.5, 0.75},
		{12.6, 1},
		{15, 1},
		{60, 1},
	} {
		delay := testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, c.seconds, c.level)
		connect.AssertEqual(t, delay, &TransportDelay{UpQuality: c.level, DownQuality: c.level})
	}
	if _, live := connectWindow.evaluate(testTransportConnectAt(base, 61)); !live {
		t.Fatal("a growing stall did not ask to be evaluated again")
	}
}

// A healthy connect: every transport connects well inside the good stall. The
// level never leaves 0, the short stall leaves no evidence, and once the
// transports are connected the window needs no more evaluation.
func TestTransportConnectPromptConnectStaysNormal(t *testing.T) {
	connectWindow, delayWindow := testTransportConnectWindows()
	base := testTransportConnectBase()
	carriers := testTransportConnectAdd(t, connectWindow, base, 6)
	for i, carrier := range carriers {
		seconds := 0.4 + 0.5*float64(i)
		testTransportConnectConnect(t, connectWindow, carrier, connect.TransportModeH1, testTransportConnectAt(base, seconds))
		testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, seconds, 0)
	}
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 30, 0)
	score, live := connectWindow.evaluate(testTransportConnectAt(base, 30))
	if live || score != (transportConnectScore{}) || len(connectWindow.endedStalls) != 0 {
		t.Fatalf("a prompt connect left a reading: %+v live %t ended %+v", score, live, connectWindow.endedStalls)
	}
}

// Connect's own dial pacing on a healthy network reads as no stall: a burst of
// new dials spread one pacing step apart, a fleet-wide reconnect spread over
// the reconnect jitter, and one transport stuck beside a working window.
func TestTransportConnectPacingStaysNormal(t *testing.T) {
	base := testTransportConnectBase()

	// connect lets a burst of new dials out up to 1 s apart and up to 10 s
	// ahead (ClientStrategy.NextConnectTime). Each connects in a second, so a
	// connect lands at least every step, though the last dial waited 9.4 s
	connectWindow, delayWindow := testTransportConnectWindows()
	carriers := testTransportConnectAdd(t, connectWindow, base, 20)
	dialSeconds := 0.0
	for i, carrier := range carriers {
		if 0 < i {
			dialSeconds += []float64{0.1, 1.0, 0.4}[i%3]
		}
		seconds := dialSeconds + 1
		testTransportConnectConnect(t, connectWindow, carrier, connect.TransportModeH1, testTransportConnectAt(base, seconds))
		testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, seconds, 0)
	}
	if 9 > dialSeconds {
		t.Fatalf("the burst's last dial waited only %vs", dialSeconds)
	}
	if len(connectWindow.endedStalls) != 0 {
		t.Fatalf("a paced burst left evidence: %+v", connectWindow.endedStalls)
	}

	// a platform restart ends every connection at once, and each re-dial first
	// waits a draw of the reconnect jitter (ReconnectJitter, 5 s): the first of
	// eight is back within a second and the rest follow
	connectWindow, delayWindow = testTransportConnectWindows()
	carriers = testTransportConnectAdd(t, connectWindow, base, 8)
	for _, carrier := range carriers {
		testTransportConnectConnect(t, connectWindow, carrier, connect.TransportModeH1, testTransportConnectAt(base, 0.5))
	}
	for _, carrier := range carriers {
		testTransportConnectLose(t, connectWindow, carrier, testTransportConnectAt(base, 10))
	}
	for i, carrier := range carriers {
		jitterSeconds := 5 * (float64(i) + 0.5) / float64(len(carriers))
		seconds := 10 + jitterSeconds + 0.5
		testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, seconds-0.01, 0)
		testTransportConnectConnect(t, connectWindow, carrier, connect.TransportModeH1, testTransportConnectAt(base, seconds))
	}
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 30, 0)

	// one transport that cannot connect, as on a broken edge, beside six
	// that did, is no stall
	connectWindow, delayWindow = testTransportConnectWindows()
	carriers = testTransportConnectAdd(t, connectWindow, base, 7)
	for _, carrier := range carriers[:6] {
		testTransportConnectConnect(t, connectWindow, carrier, connect.TransportModeH1, testTransportConnectAt(base, 0.5))
	}
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 60, 0)
	if _, live := connectWindow.evaluate(testTransportConnectAt(base, 60)); live {
		t.Fatal("a window with no stall asked to be evaluated again")
	}
}

// Some transports stay connected while connects keep taking longer than the
// good stall with at least half the window trying (the owner's second
// example). Each such stretch without a connect is a stall over the good
// stall, and the level holds while they repeat, then ages out after the last.
func TestTransportConnectRepeatedSlowConnects(t *testing.T) {
	connectWindow, delayWindow := testTransportConnectWindows()
	base := testTransportConnectBase()
	carriers := testTransportConnectAdd(t, connectWindow, base, 4)
	testTransportConnectConnect(t, connectWindow, carriers[0], connect.TransportModeH1, testTransportConnectAt(base, 0.5))
	testTransportConnectConnect(t, connectWindow, carriers[1], connect.TransportModeH1, testTransportConnectAt(base, 0.5))
	// two connected and two trying is still a stall
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 5.4, 0)
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 5.6, 0.25)
	// a connect after 6.5 s without one ends the stall
	testTransportConnectConnect(t, connectWindow, carriers[2], connect.TransportModeH1, testTransportConnectAt(base, 7))
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 7, 0.25)

	// a connection drops and takes 8 s to come back while another still dials
	testTransportConnectLose(t, connectWindow, carriers[1], testTransportConnectAt(base, 9))
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 15, 0.25)
	testTransportConnectConnect(t, connectWindow, carriers[1], connect.TransportModeH1, testTransportConnectAt(base, 17))
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 17, 0.5)

	// the 8 s stall is evidence for 20 s after it ended
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 36.9, 0.5)
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 37, 0)
}

// Recovery clears the level: a stall stays evidence for the window timeout
// after it ended and then ages out, or a transport that starts an attempt after
// the stall ended and connects within the prompt connect timeout clears it at
// once. A straggler that started during the stall, or a connect between the
// prompt timeout and the good stall, clears nothing.
func TestTransportConnectRecoveryClears(t *testing.T) {
	base := testTransportConnectBase()
	slowConnect := func() (*transportConnectWindow, *transportDelayWindow, []*testingTransportConnectCarrier) {
		connectWindow, delayWindow := testTransportConnectWindows()
		carriers := testTransportConnectAdd(t, connectWindow, base, 4)
		for _, carrier := range carriers {
			testTransportConnectConnect(t, connectWindow, carrier, connect.TransportModeH1, testTransportConnectAt(base, 8))
		}
		testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 8, 0.5)
		return connectWindow, delayWindow, carriers
	}

	// by age: the 8 s stall ended at 8 s
	connectWindow, delayWindow, _ := slowConnect()
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 27.9, 0.5)
	if _, live := connectWindow.evaluate(testTransportConnectAt(base, 27.9)); !live {
		t.Fatal("live evidence did not ask to be evaluated again")
	}
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 28, 0)
	if _, live := connectWindow.evaluate(testTransportConnectAt(base, 28)); live {
		t.Fatal("aged out evidence asked to be evaluated again")
	}

	// by a prompt connect after the stall: back in 1 s
	connectWindow, delayWindow, carriers := slowConnect()
	testTransportConnectLose(t, connectWindow, carriers[0], testTransportConnectAt(base, 10))
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 10.5, 0.5)
	testTransportConnectConnect(t, connectWindow, carriers[0], connect.TransportModeH1, testTransportConnectAt(base, 11))
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 11, 0)
	if _, live := connectWindow.evaluate(testTransportConnectAt(base, 11)); live {
		t.Fatal("cleared evidence asked to be evaluated again")
	}

	// a connect of 3 s is not prompt
	connectWindow, delayWindow, carriers = slowConnect()
	testTransportConnectLose(t, connectWindow, carriers[0], testTransportConnectAt(base, 10))
	testTransportConnectConnect(t, connectWindow, carriers[0], connect.TransportModeH1, testTransportConnectAt(base, 13))
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 13, 0.5)
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 28, 0)

	// a straggler that started dialing in the stall connects promptly after
	// it ended: it waited out the same network
	connectWindow, delayWindow = testTransportConnectWindows()
	carriers = testTransportConnectAdd(t, connectWindow, base, 4)
	straggler := testTransportConnectAdd(t, connectWindow, testTransportConnectAt(base, 7.5), 1)[0]
	for _, carrier := range carriers {
		testTransportConnectConnect(t, connectWindow, carrier, connect.TransportModeH1, testTransportConnectAt(base, 8))
	}
	testTransportConnectConnect(t, connectWindow, straggler, connect.TransportModeH1, testTransportConnectAt(base, 8.5))
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 9, 0.5)
}

// Connections that keep dropping together and reconnecting in about the good
// stall: stalls alternate 5.2 s and 4.8 s, with 4 s connected between, and in
// each connected stretch one transport takes 3 s to reconnect. The level
// rises at the first stall over the good stall and holds through the ones
// under it, which are no evidence but clear nothing (neither they nor the 3 s
// reconnect is prompt). It falls only once the last stall over the good stall
// aged out: one rise and one fall.
func TestTransportConnectNoFlapAtTheThreshold(t *testing.T) {
	connectWindow, delayWindow := testTransportConnectWindows()
	base := testTransportConnectBase()
	carriers := testTransportConnectAdd(t, connectWindow, base, 4)

	// the events, in tenths of a second
	actions := map[int][]func(now time.Time){}
	at := func(tenth int, action func(now time.Time)) {
		actions[tenth] = append(actions[tenth], action)
	}
	stallTenths := []int{52, 48, 52, 48, 52}
	start := 0
	lastSlowEnd := 0
	for i, stallTenth := range stallTenths {
		end := start + stallTenth
		for _, carrier := range carriers {
			at(end, func(now time.Time) {
				testTransportConnectConnect(t, connectWindow, carrier, connect.TransportModeH1, now)
			})
		}
		if 50 < stallTenth {
			lastSlowEnd = end
		}
		at(end+5, func(now time.Time) {
			testTransportConnectLose(t, connectWindow, carriers[0], now)
		})
		at(end+35, func(now time.Time) {
			testTransportConnectConnect(t, connectWindow, carriers[0], connect.TransportModeH1, now)
		})
		if i+1 < len(stallTenths) {
			start = end + 40
			for _, carrier := range carriers {
				at(start, func(now time.Time) {
					testTransportConnectLose(t, connectWindow, carrier, now)
				})
			}
		}
	}

	type change struct {
		tenth int
		level float64
	}
	changes := []change{}
	level := 0.0
	for tenth := 0; tenth <= lastSlowEnd+300; tenth += 1 {
		now := base.Add(time.Duration(tenth) * 100 * time.Millisecond)
		for _, action := range actions[tenth] {
			action(now)
		}
		delay := testTransportConnectRead(connectWindow, delayWindow, now)
		if next := testTransportConnectLevel(delay); next != level {
			level = next
			changes = append(changes, change{tenth, level})
		}
	}
	// up once the first stall passed 5 s, down 20 s after the last 5.2 s
	// stall ended
	connect.AssertEqual(t, changes, []change{
		{51, 0.25},
		{lastSlowEnd + 200, 0},
	})
}

// A transport its owner holds, one a client limit holds and one waiting for
// memory budget say nothing about the network: no stall, however long. One
// that starts dialing once its budget frees counts from the evaluation that
// sees it, though nothing notifies.
func TestTransportConnectHeldTransportsAreNoEvidence(t *testing.T) {
	connectWindow, delayWindow := testTransportConnectWindows()
	base := testTransportConnectBase()
	carriers := testTransportConnectAdd(t, connectWindow, base, 3)
	carriers[0].set(connect.PlatformTransportStateDisabled, connect.TransportModeNone)
	carriers[1].set(connect.PlatformTransportStateClientLimit, connect.TransportModeNone)
	carriers[2].setWaitingForBudget(true)
	for _, carrier := range carriers {
		connectWindow.observe(1, carrier, base)
	}
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 60, 0)
	if _, live := connectWindow.evaluate(testTransportConnectAt(base, 60)); live {
		t.Fatal("held transports asked to be evaluated again")
	}

	carriers[2].setWaitingForBudget(false)
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 61, 0)
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 65.9, 0)
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 66.1, 0.25)
}

// A stall that DNS carried every connected transport out of waited on the
// fallback, not on the user's network (D10). One a direct carrier ended reads
// as the user's network, and so does a stall with nothing connected.
func TestTransportConnectFallback(t *testing.T) {
	base := testTransportConnectBase()
	for _, c := range []struct {
		name     string
		modes    []connect.TransportMode
		fallback bool
	}{
		{"dns", []connect.TransportMode{connect.TransportModeH3Dns, connect.TransportModeH3DnsPump}, true},
		{"dns and h1", []connect.TransportMode{connect.TransportModeH3Dns, connect.TransportModeH1}, false},
		{"h3", []connect.TransportMode{connect.TransportModeH3, connect.TransportModeH3}, false},
	} {
		connectWindow, delayWindow := testTransportConnectWindows()
		carriers := testTransportConnectAdd(t, connectWindow, base, 3)
		for i, activeMode := range c.modes {
			testTransportConnectConnect(t, connectWindow, carriers[i], activeMode, testTransportConnectAt(base, 6))
		}
		delay := testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 6, 0.25)
		if delay.Fallback != c.fallback {
			t.Fatalf("%s: fallback %t, expected %t", c.name, delay.Fallback, c.fallback)
		}
	}

	// an ongoing stall in which DNS carries every connected transport
	connectWindow, delayWindow := testTransportConnectWindows()
	carriers := testTransportConnectAdd(t, connectWindow, base, 4)
	testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 0.5, 0)
	for _, carrier := range carriers[:2] {
		testTransportConnectConnect(t, connectWindow, carrier, connect.TransportModeH3Dns, testTransportConnectAt(base, 1))
	}
	delay := testTransportConnectExpectLevel(t, connectWindow, delayWindow, base, 7, 0.25)
	if !delay.Fallback {
		t.Fatalf("the fallback's ongoing stall read as the user's network: %+v", delay)
	}
}

// The connect score folds into a direction as the worse of the two: none
// leaves the delay reading as it was, a worse one takes over with its
// fallback, and between equal readings the fallback needs both. Through the
// window, each direction takes the worse, the numbers stay the delay
// samples', and the delay's own level wins where it is worse.
func TestTransportDelayWithConnect(t *testing.T) {
	for _, c := range []struct {
		name     string
		score    transportDelayScore
		connect  transportConnectScore
		expected transportDelayScore
	}{
		{"no connect score", transportDelayScore{quality: 0.4, fallback: true}, transportConnectScore{}, transportDelayScore{quality: 0.4, fallback: true}},
		{"neither", transportDelayScore{}, transportConnectScore{}, transportDelayScore{}},
		{"connect worse", transportDelayScore{quality: 0.2, fallback: true}, transportConnectScore{stall: time.Second, quality: 0.5}, transportDelayScore{quality: 0.5}},
		{"connect worse fallback", transportDelayScore{quality: 0.2}, transportConnectScore{stall: time.Second, quality: 0.5, fallback: true}, transportDelayScore{quality: 0.5, fallback: true}},
		{"delay worse", transportDelayScore{quality: 0.7}, transportConnectScore{stall: time.Second, quality: 0.5, fallback: true}, transportDelayScore{quality: 0.7}},
		{"equal both fallback", transportDelayScore{quality: 0.5, fallback: true}, transportConnectScore{stall: time.Second, quality: 0.5, fallback: true}, transportDelayScore{quality: 0.5, fallback: true}},
		{"equal one fallback", transportDelayScore{quality: 0.5, fallback: true}, transportConnectScore{stall: time.Second, quality: 0.5}, transportDelayScore{quality: 0.5}},
	} {
		connect.AssertEqual(t, transportDelayWithConnect(c.score, c.connect), c.expected)
	}

	// a 1.5 s down queue (0.5) beside a 12 s connect stall (0.7): up takes
	// the stall's level, down the worse of the two
	window, multi := testTransportDelayWindow()
	base := testTransportDelayBase()
	for second := range 2 {
		testTransportDelayAdd(t, window, multi, testTransportDelayQueue(connect.NewId(), connect.TransportTypeH1, testTransportDelayAt(base, second), testTransportDelayNoQueueDelay, 1500*time.Millisecond))
	}
	now := testTransportDelayAt(base, 1)
	delay, _ := window.evaluateWithConnect(now, transportConnectScore{stall: 12 * time.Second, quality: 0.7})
	connect.AssertEqual(t, delay, &TransportDelay{UpQuality: 0.75, DownQuality: 0.75, DownQueueDelayMillis: 1500})
	// the stall gone, up returns to 0, and down holds the 0.75 level by the
	// release hysteresis: its own 0.5 is not 0.1 under the level's band
	delay, _ = window.evaluateWithConnect(now, transportConnectScore{})
	connect.AssertEqual(t, delay, &TransportDelay{UpQuality: 0, DownQuality: 0.75, DownQueueDelayMillis: 1500})
}

// The window takes the transports of one generation: a transport of another
// generation, a second report of one transport, and anything before the first
// reset or after the close are dropped, and a reset drops the transports, the
// stall and the evidence.
func TestTransportConnectWindowGenerationFence(t *testing.T) {
	window := newTransportConnectWindow(context.Background(), testTransportDelaySettings(), nil)
	base := testTransportConnectBase()
	carrier := newTestingTransportConnectCarrier()
	if window.add(0, carrier, base) {
		t.Fatal("a window with no generation took a transport")
	}
	window.reset(1)
	if window.add(2, carrier, base) {
		t.Fatal("the window took a transport of another generation")
	}
	if !window.add(1, carrier, base) {
		t.Fatal("the window dropped a transport of its generation")
	}
	if window.add(1, carrier, base) {
		t.Fatal("the window took a transport twice")
	}
	if window.observe(2, carrier, base) || window.remove(2, carrier, base) {
		t.Fatal("the window took a change of another generation")
	}
	if score, live := window.evaluate(testTransportConnectAt(base, 10)); !live || score.quality != 0.5 {
		t.Fatalf("a 10 s stall read %+v live %t", score, live)
	}

	window.reset(2)
	if score, live := window.evaluate(testTransportConnectAt(base, 10)); live || score != (transportConnectScore{}) {
		t.Fatalf("the reset kept the retired generation's stall: %+v live %t", score, live)
	}
	if window.observe(1, carrier, base) || window.remove(1, carrier, base) {
		t.Fatal("the window took a change of the retired generation")
	}
	if !window.add(2, newTestingTransportConnectCarrier(), base) {
		t.Fatal("the window dropped a transport of the new generation")
	}
	if window.remove(2, carrier, base) {
		t.Fatal("a removal of a transport the window does not hold reported it")
	}

	window.close()
	if window.add(2, newTestingTransportConnectCarrier(), base) {
		t.Fatal("a closed window took a transport")
	}
	if score, live := window.evaluate(testTransportConnectAt(base, 10)); live || score != (transportConnectScore{}) {
		t.Fatalf("a closed window read %+v live %t", score, live)
	}
}

// The defaults are the window transports' own connect timing, the durations
// bind as milliseconds, and a window built from zeroed settings takes the
// thresholds as set and the delay window's timeout default.
func TestTransportConnectThresholdsFollowConnectDefaults(t *testing.T) {
	platform := connect.DefaultPlatformTransportSettings()
	connect.AssertEqual(t, defaultTransportDelayGoodConnectStall, platform.ReconnectTimeout)
	for _, dialTimeout := range []time.Duration{
		platform.HttpConnectTimeout,
		platform.WsHandshakeTimeout,
		platform.QuicConnectTimeout,
		platform.QuicHandshakeTimeout,
	} {
		connect.AssertEqual(t, defaultTransportDelayBadConnectStall, dialTimeout)
	}
	connect.AssertEqual(t, defaultTransportDelayPromptConnectTimeout, platform.ModeInitialDelay)
	if defaultTransportDelayPromptConnectTimeout >= defaultTransportDelayGoodConnectStall {
		t.Fatal("the prompt connect timeout leaves no band under the good stall")
	}

	for _, settings := range []*DeviceLocalSettings{DefaultDeviceLocalSettings(), DefaultLowMemoryDeviceLocalSettings()} {
		connect.AssertEqual(t, settings.TransportDelayGoodConnectStall, 5*time.Second)
		connect.AssertEqual(t, settings.TransportDelayBadConnectStall, 15*time.Second)
		connect.AssertEqual(t, settings.TransportDelayPromptConnectTimeout, 2*time.Second)
	}
	settings := DefaultDeviceLocalSettings()
	connect.AssertEqual(t, settings.GetTransportDelayGoodConnectStallMillis(), int64(5000))
	connect.AssertEqual(t, settings.GetTransportDelayBadConnectStallMillis(), int64(15_000))
	connect.AssertEqual(t, settings.GetTransportDelayPromptConnectTimeoutMillis(), int64(2000))
	settings.SetTransportDelayGoodConnectStallMillis(4000)
	connect.AssertEqual(t, settings.TransportDelayGoodConnectStall, 4*time.Second)
	settings.SetTransportDelayBadConnectStallMillis(20_000)
	connect.AssertEqual(t, settings.TransportDelayBadConnectStall, 20*time.Second)
	settings.SetTransportDelayPromptConnectTimeoutMillis(1500)
	connect.AssertEqual(t, settings.TransportDelayPromptConnectTimeout, 1500*time.Millisecond)
	window := newTransportConnectWindow(context.Background(), settings, nil)
	connect.AssertEqual(t, window.goodStall, 4*time.Second)
	connect.AssertEqual(t, window.badStall, 20*time.Second)
	connect.AssertEqual(t, window.promptConnectTimeout, 1500*time.Millisecond)
	connect.AssertEqual(t, window.timeout, defaultTransportDelayWindowTimeout)

	window = newTransportConnectWindow(context.Background(), &DeviceLocalSettings{}, nil)
	connect.AssertEqual(t, window.goodStall, time.Duration(0))
	connect.AssertEqual(t, window.promptConnectTimeout, time.Duration(0))
	connect.AssertEqual(t, window.timeout, defaultTransportDelayWindowTimeout)
	// no stall reads 0 even with a good stall of 0
	connect.AssertEqual(t, transportConnectStallQuality(0, 0, 0), 0.0)
	connect.AssertEqual(t, transportConnectStallQuality(time.Millisecond, 0, 0), 1.0)
}

// Waits until done reads true of the window's state.
func testWaitTransportConnect(t *testing.T, window *transportConnectWindow, message string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		window.stateLock.Lock()
		ok := done()
		window.stateLock.Unlock()
		if ok {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatal(message)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The watcher of a tracked transport follows its connect changes, which wake
// the reader, until it closes, which drops it; a reset ends the retired
// generation's watchers, and a closed window tracks nothing and joins them.
func TestTransportConnectWatcherFollowsTheTransport(t *testing.T) {
	changes := make(chan struct{}, 1)
	window := newTransportConnectWindow(context.Background(), testTransportDelaySettings(), func() {
		select {
		case changes <- struct{}{}:
		default:
		}
	})
	window.reset(1)
	nextChange := func(message string) {
		t.Helper()
		select {
		case <-changes:
		case <-time.After(30 * time.Second):
			t.Fatal(message)
		}
	}

	carrier := newTestingTransportConnectCarrier()
	window.track(1, carrier)
	nextChange("the tracked transport woke no reader")
	testWaitTransportConnect(t, window, "the window did not hold the tracked transport", func() bool {
		return window.carrierStates[carrier] != nil
	})
	carrier.set(connect.PlatformTransportStateConnected, connect.TransportModeH1)
	nextChange("the connect woke no reader")
	testWaitTransportConnect(t, window, "the watcher missed the connect", func() bool {
		carrierState := window.carrierStates[carrier]
		return carrierState != nil && carrierState.connected
	})
	carrier.close()
	testWaitTransportConnect(t, window, "the watcher kept a closed transport", func() bool {
		return window.carrierStates[carrier] == nil
	})

	// a report for another generation starts no watcher
	window.track(2, newTestingTransportConnectCarrier())
	// a reset ends the retired generation's watchers, though their transports
	// never close
	retired := newTestingTransportConnectCarrier()
	window.track(1, retired)
	testWaitTransportConnect(t, window, "the window did not hold the tracked transport", func() bool {
		return window.carrierStates[retired] != nil
	})
	window.reset(2)
	retired.set(connect.PlatformTransportStateConnected, connect.TransportModeH1)
	testWaitTransportConnect(t, window, "the reset kept a retired transport", func() bool {
		return len(window.carrierStates) == 0
	})

	current := newTestingTransportConnectCarrier()
	window.track(2, current)
	window.close()
	window.track(2, newTestingTransportConnectCarrier())
	joined := make(chan struct{})
	go func() {
		window.wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(30 * time.Second):
		t.Fatal("the closed window did not join its watchers")
	}
}

// A local device raises its transport delay while its window transports are
// slow to connect, from no samples at all: the listener hears the level rise
// to the worst as the stall passes the bad stall, then return to normal once
// the transports connect and the stall's evidence aged out. A closed
// transport leaves the window, and a rebuild retires the generation's
// transports. The thresholds are short so the device's real clock runs the
// stall in moments; the window tests pin the defaults.
func TestDeviceLocalTransportDelaySlowConnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	settings := DefaultDeviceLocalSettings()
	testTransportDelayDeviceSettings(settings)
	settings.TransportDelayGoodConnectStall = 50 * time.Millisecond
	settings.TransportDelayBadConnectStall = 200 * time.Millisecond
	settings.TransportDelayWindowTimeout = 300 * time.Millisecond
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()

	listener := newTestingTransportDelayListener()
	sub := device.AddTransportDelayChangeListener(listener)
	defer sub.Close()

	location := testTransportDelayConnect(device)
	_, generation := testTransportDelayMultiClient(t, device)
	carriers := []*testingTransportConnectCarrier{
		newTestingTransportConnectCarrier(),
		newTestingTransportConnectCarrier(),
	}
	for _, carrier := range carriers {
		// what the window generator's report does (applyWindowTransportDelaySettings)
		device.transportConnect.track(generation, carrier)
	}
	delay := listener.next(t, "the slow connect reached no listener")
	if delay.UpQuality <= 0 || delay.DownQuality <= 0 || delay.Fallback {
		t.Fatalf("the slow connect read %+v", delay)
	}
	testWaitTransportDelay(t, device, func(delay *TransportDelay) bool {
		return delay.UpQuality == 1 && delay.DownQuality == 1
	})

	for _, carrier := range carriers {
		carrier.set(connect.PlatformTransportStateConnected, connect.TransportModeH1)
	}
	testWaitTransportDelay(t, device, func(delay *TransportDelay) bool {
		return *delay == TransportDelay{}
	})
	for {
		if *listener.next(t, "the recovery reached no listener") == (TransportDelay{}) {
			break
		}
	}

	carriers[0].close()
	testWaitTransportConnect(t, device.transportConnect, "the device kept a closed transport", func() bool {
		return len(device.transportConnect.carrierStates) == 1
	})
	device.Reconnect(location)
	testWaitTransportConnect(t, device.transportConnect, "the rebuild kept the retired generation's transports", func() bool {
		return len(device.transportConnect.carrierStates) == 0
	})
	carriers[1].set(connect.PlatformTransportStateConnecting, connect.TransportModeNone)
	listener.quiet(t, 200*time.Millisecond, "a retired transport moved the level")
	connect.AssertEqual(t, device.GetTransportDelay(), noneTransportDelay())
}

// The window generator reports each window transport it builds to the connect
// window of the generation that built it: a real platform transport, held so
// it never dials, is watched until it closes, and is no stall. A report from a
// retired generation's generator is dropped. With EnableTransportDelay off the
// generator reports nothing and offers no delay header.
func TestDeviceLocalWindowGeneratorReportsTransports(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	settings := DefaultDeviceLocalSettings()
	testTransportDelayDeviceSettings(settings)
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()
	testTransportDelayConnect(device)
	_, generation := testTransportDelayMultiClient(t, device)

	apiSettings := connect.DefaultApiMultiClientGeneratorSettings()
	device.applyWindowTransportDelaySettings(apiSettings, generation)
	if apiSettings.TransportDelay == nil || apiSettings.PlatformTransportCreated == nil {
		t.Fatal("the window generator neither measures nor reports its transports")
	}
	retiredSettings := connect.DefaultApiMultiClientGeneratorSettings()
	device.applyWindowTransportDelaySettings(retiredSettings, generation-1)

	strategy := connect.NewClientStrategyWithDefaults(ctx)
	defer strategy.Close()
	newHeldTransport := func() *connect.PlatformTransport {
		platformSettings := connect.DefaultPlatformTransportSettings()
		platformSettings.StartDisabled = true
		return connect.NewPlatformTransportWithTargetMode(
			ctx,
			strategy,
			connect.NewRouteManager(ctx, "test"),
			"wss://127.0.0.1:1",
			&connect.ClientAuth{ByJwt: "test", InstanceId: connect.NewId(), AppVersion: "0.0.0"},
			connect.TransportModeAuto,
			platformSettings,
		)
	}
	transport := newHeldTransport()
	retired := newHeldTransport()
	defer retired.Close()
	apiSettings.PlatformTransportCreated(nil, transport)
	retiredSettings.PlatformTransportCreated(nil, retired)
	apiSettings.PlatformTransportCreated(nil, nil)
	testWaitTransportConnect(t, device.transportConnect, "the device did not watch the reported transport", func() bool {
		return device.transportConnect.carrierStates[transport] != nil
	})
	testWaitTransportConnect(t, device.transportConnect, "the device took a retired generation's transport", func() bool {
		return len(device.transportConnect.carrierStates) == 1
	})
	if score, live := device.transportConnect.evaluate(time.Now().Add(time.Minute)); live || score != (transportConnectScore{}) {
		t.Fatalf("a held transport read as a stall: %+v live %t", score, live)
	}
	transport.Close()
	testWaitTransportConnect(t, device.transportConnect, "the device kept the closed transport", func() bool {
		return len(device.transportConnect.carrierStates) == 0
	})

	disabled := &DeviceLocal{settings: &DeviceLocalSettings{EnableTransportDelay: false}}
	disabledSettings := connect.DefaultApiMultiClientGeneratorSettings()
	disabled.applyWindowTransportDelaySettings(disabledSettings, generation)
	if disabledSettings.TransportDelay != nil || disabledSettings.PlatformTransportCreated != nil {
		t.Fatal("a device with the transport delay off set up its window generator")
	}
}
