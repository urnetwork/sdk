package urmessage

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/sdk/v2026"
)

// ── the reconnect window ─────────────────────────────────────────────────────────────────────
//
// WHAT THESE CASES ARE OVER, MEASURED ON THE DEPLOYED SERVER AND NOT INVENTED HERE. A reconnecting
// `client_id` is not routed to for about sixty seconds: the new connection attaches, routes
// register, the Hello goes out and NOTHING COMES BACK -- no error and no refusal, silence until a
// deadline. [Device.Connect] used to send one Hello and return a hard error, so every app resume,
// every laptop lid and every network flap was reported as a failure where the truth was "not yet".
// See [ConnectPolicy] for the measurement and msgrepo's operator item 5 for the cause, which is not
// this package's.
//
// THE SHAPE THESE CASES NEED IS SILENCE AND NOT AN ERROR, which is why [acceptingSilentClient]
// exists beside [silentClient]: the existing one REFUSES the frame, so a Call fails instantly and
// never reaches the deadline that models the window. This one takes the frame -- it really is on
// the wire -- and nothing ever answers it.

// acceptingSilentClient satisfies [sdk.MessageTransportClient], ACCEPTS every frame, and never
// answers one. It is the deployed server inside the window, as a client sees it.
type acceptingSilentClient struct {
	mutex  sync.Mutex
	frames int
}

func (self *acceptingSilentClient) SendWithTimeout(frame *protocol.Frame, destination connect.Id,
	ackCallback connect.AckFunction, timeout time.Duration, opts ...any) bool {

	self.mutex.Lock()
	self.frames += 1
	self.mutex.Unlock()
	return true
}

func (self *acceptingSilentClient) AddReceiveCallback(receiveCallback connect.ReceiveFunction) func() {
	return func() {}
}

func (self *acceptingSilentClient) sent() int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.frames
}

// newUnansweredDevice is a device whose every request goes out and is never answered.
func newUnansweredDevice(t *testing.T, policy ConnectPolicy) (*Device, *acceptingSilentClient) {
	t.Helper()
	client := &acceptingSilentClient{}
	transport, err := sdk.NewMessageTransport(&sdk.MessageTransportConfig{
		Client:          client,
		Server:          connect.NewId(),
		ProtocolVersion: 1,
	})
	if err != nil {
		t.Fatalf("sdk.NewMessageTransport: %v", err)
	}
	t.Cleanup(transport.Close)
	streamStore, err := sdk.OpenStreamStore(t.TempDir())
	if err != nil {
		t.Fatalf("sdk.OpenStreamStore: %v", err)
	}
	t.Cleanup(func() { streamStore.Close() })
	device, err := NewDevice(DeviceConfig{
		Transport: transport,
		Reserver:  sdk.NewStreamIndexReserver(streamStore),
		Connect:   policy,
	})
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}
	t.Cleanup(func() { device.Close() })
	return device, client
}

// A HELLO THAT IS NOT ANSWERED IS RETRIED ACROSS THE BUDGET AND REPORTED AS "RECONNECTING".
//
// THE TWO HALVES ARE BOTH THE POINT. It must RETRY -- one Hello and a hard error is the defect --
// and it must name what it is: [ErrReconnecting] and not [ErrHelloRefused], because a caller
// showing a user "could not connect" during an ordinary app resume is telling them something
// false.
//
// AND THE BUDGET BOUNDS THE CALL AND NOT THE WINDOW. When it runs out the answer is still
// "not yet, ask again", which is what makes the 90-second default a cost rather than a verdict:
// if the operator's window were ever longer, a caller that retries is correct and a caller that
// gave up was told to.
//
// WHAT WOULD GO RED WITHOUT THE FIX: one frame is sent instead of several, and the error is the
// transport's own timeout rather than a sentence about reconnecting.
func TestAnUnansweredHelloIsRetriedAcrossTheBudgetAndAnsweredAsReconnecting(t *testing.T) {
	var attempts []ConnectAttempt
	var mutex sync.Mutex
	policy := ConnectPolicy{
		Budget:         700 * time.Millisecond,
		AttemptTimeout: 60 * time.Millisecond,
		FirstBackoff:   20 * time.Millisecond,
		MaxBackoff:     40 * time.Millisecond,
		OnAttempt: func(one ConnectAttempt) {
			mutex.Lock()
			attempts = append(attempts, one)
			mutex.Unlock()
		},
	}
	device, client := newUnansweredDevice(t, policy)

	started := time.Now()
	err := device.Connect(context.Background())
	elapsed := time.Since(started)

	if !errors.Is(err, ErrReconnecting) {
		t.Fatalf("an unanswered Hello answered %v, want one wrapping ErrReconnecting", err)
	}
	if errors.Is(err, ErrHelloRefused) {
		t.Error("silence was reported as a refusal, which is the sentence a user must not be shown")
	}
	t.Logf("after %v: %v", elapsed.Round(time.Millisecond), err)

	// IT RETRIED, and the count is taken off the CLIENT rather than off the callback, so a
	// callback that fired without a frame going out could not satisfy it.
	mutex.Lock()
	seen := len(attempts)
	mutex.Unlock()
	if seen < 3 {
		t.Fatalf("%d attempts over a %v budget: this is not a retry", seen, policy.Budget)
	}
	if client.sent() < seen {
		t.Fatalf("%d attempts were reported and only %d frames reached the wire", seen, client.sent())
	}
	t.Logf("%d Hello attempts, %d frames on the wire", seen, client.sent())

	// AND IT SPENT THE BUDGET RATHER THAN GIVING UP EARLY. The upper bound is loose on purpose:
	// it is here to catch a loop that never terminates, not to assert a schedule, which is
	// [TestTheConnectBackoffIsTheDeclaredSchedule]'s job -- and not the budget's bound either,
	// which is [TestTheConnectBudgetBoundsTheCallWhateverTheAttemptTimeout]'s.
	if elapsed < policy.Budget {
		t.Errorf("gave up after %v, which is inside its own %v budget", elapsed, policy.Budget)
	}
	if 5*policy.Budget < elapsed {
		t.Errorf("took %v against a %v budget", elapsed, policy.Budget)
	}
}

// THE BUDGET BOUNDS HOW LONG THE CALL BLOCKS, WHATEVER THE ATTEMPT TIMEOUT SAYS.
//
// THE REVIEW THAT FOUND IT, MEASURED FROM C against an unroutable server: a 500 ms budget with the
// default attempt blocked for 10,000 ms; 2,000 over a 3,000 attempt blocked 3,000; 4,000 over a
// 1,000 attempt blocked 5,000. The budget was consulted only after an attempt RETURNED and nothing
// asked whether the next one fitted, so a call overshot by up to one attempt timeout -- and the
// defaults' own schedule returned at about 100 s against the 90 s the C header states.
//
// THE ROWS ARE THE REVIEW'S SHAPES SCALED DOWN, and the row that matters most is the first: a
// budget SHORTER than one attempt, with the attempt left at its default. The bound is the budget
// plus a slack for the scheduler, and the floor is that the call did not give up early either.
//
// WHAT WOULD GO RED: take the cut of each attempt to what is left of the budget out of
// Device.Connect, and the first row blocks for the default attempt's 10 s.
func TestTheConnectBudgetBoundsTheCallWhateverTheAttemptTimeout(t *testing.T) {
	const slack = 150 * time.Millisecond
	for _, one := range []struct {
		name    string
		budget  time.Duration
		attempt time.Duration
		backoff time.Duration
	}{
		// each row's comment is what the build before this case returned at, so that every row
		// is one the old loop overshot by more than the slack and none is here for decoration.
		{"a budget under the DEFAULT attempt", 300 * time.Millisecond, 0, 0},                                                      // 10,000 ms
		{"a budget under a longer attempt", 200 * time.Millisecond, time.Second, 50 * time.Millisecond},                           // 1,000 ms
		{"a budget that ends inside a later attempt", 500 * time.Millisecond, 400 * time.Millisecond, 50 * time.Millisecond},      // 850 ms
		{"a budget whose last pause would leave nothing", 350 * time.Millisecond, 300 * time.Millisecond, 200 * time.Millisecond}, // 650 ms
	} {
		t.Run(one.name, func(t *testing.T) {
			var reported []ConnectAttempt
			var mutex sync.Mutex
			device, client := newUnansweredDevice(t, ConnectPolicy{
				Budget:         one.budget,
				AttemptTimeout: one.attempt,
				FirstBackoff:   one.backoff,
				MaxBackoff:     one.backoff,
				OnAttempt: func(attempt ConnectAttempt) {
					mutex.Lock()
					reported = append(reported, attempt)
					mutex.Unlock()
				},
			})
			started := time.Now()
			err := device.Connect(context.Background())
			elapsed := time.Since(started)
			if !errors.Is(err, ErrReconnecting) {
				t.Fatalf("answered %v, want ErrReconnecting", err)
			}
			if one.budget+slack < elapsed {
				t.Fatalf("a %v budget blocked for %v", one.budget, elapsed)
			}
			if elapsed < one.budget {
				t.Fatalf("a %v budget gave up after %v", one.budget, elapsed)
			}
			mutex.Lock()
			defer mutex.Unlock()
			if client.sent() < len(reported) {
				t.Fatalf("%d attempts were reported and %d frames reached the wire", len(reported), client.sent())
			}
			backoffs := []time.Duration{}
			for _, attempt := range reported {
				backoffs = append(backoffs, attempt.Backoff)
			}
			if len(reported) == 0 || reported[len(reported)-1].Backoff != 0 {
				t.Errorf("the last attempt reported a pause no attempt followed: %v", backoffs)
			}
			t.Logf("%v budget: returned after %v, %d attempt(s), pauses %v",
				one.budget, elapsed.Round(time.Millisecond), len(reported), backoffs)
		})
	}
}

// THE BACKOFF IS THE SCHEDULE THIS PACKAGE DECLARES: doubling from the first pause up to the cap.
//
// IT IS ASSERTABLE EXACTLY BECAUSE THERE IS NO JITTER, which is the decision [ConnectPolicy]
// argues: the window is per client_id and is not a contended resource, so jitter would spread
// nothing and would cost this assertion.
//
// WHAT WOULD GO RED IF THE DOUBLING WERE DROPPED: the pauses stay at the first value and a long
// window costs many more Hellos than it needs.
func TestTheConnectBackoffIsTheDeclaredSchedule(t *testing.T) {
	var pauses []time.Duration
	var mutex sync.Mutex
	policy := ConnectPolicy{
		Budget:         10 * time.Second, // long, so the tail is never the thing being measured
		AttemptTimeout: 20 * time.Millisecond,
		FirstBackoff:   10 * time.Millisecond,
		MaxBackoff:     40 * time.Millisecond,
		OnAttempt: func(one ConnectAttempt) {
			mutex.Lock()
			pauses = append(pauses, one.Backoff)
			mutex.Unlock()
		},
	}
	device, _ := newUnansweredDevice(t, policy)
	ctx, cancel := context.WithCancel(context.Background())
	// five attempts' worth, then stop: the schedule is what is under test and not the budget.
	go func() {
		for {
			mutex.Lock()
			enough := 5 <= len(pauses)
			mutex.Unlock()
			if enough {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	device.Connect(ctx)
	cancel()

	mutex.Lock()
	got := append([]time.Duration(nil), pauses...)
	mutex.Unlock()
	if len(got) < 5 {
		t.Fatalf("only %d attempts were reported: %v", len(got), got)
	}
	want := []time.Duration{
		10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond,
		40 * time.Millisecond, 40 * time.Millisecond,
	}
	for at, expected := range want {
		if got[at] != expected {
			t.Fatalf("the backoff schedule is %v, want %v", got[:len(want)], want)
		}
	}
	t.Logf("the declared schedule, driven: %v", got[:len(want)])
}

// THE CALLER'S OWN CONTEXT ENDS A RECONNECT AT ONCE, AND IT IS NOT REPORTED AS "RECONNECTING".
//
// A CANCELLED CALLER IS NOT "NOT YET". It is the caller saying stop -- the app is closing, the user
// left the screen -- and a retry loop that outlived it would be a device holding a thread nobody
// asked for. The distinction also has to reach the ERROR: [ErrReconnecting] means "ask again", and
// answering it to a caller that cancelled would be telling it to.
//
// WHAT WOULD GO RED IF THE LOOP IGNORED THE CALLER'S CONTEXT: this takes the full budget instead of
// the cancellation, and the error names reconnecting rather than the cancellation.
func TestTheCallersContextEndsAReconnectAtOnce(t *testing.T) {
	// THE CANCELLATION LANDS WHERE THE LOOP HAS NO PAUSE LEFT TO NOTICE IT IN, and that is what
	// makes this case discriminating rather than decorative. With a long budget the pause between
	// attempts notices the cancellation on its own and the explicit check costs nothing -- the
	// first draft of this case was written that way and deleting the check left it GREEN.
	//
	// IT WAS "THE BUDGET IS ALREADY SPENT WHEN THE CANCELLATION ARRIVES" -- a 50 ms budget under a
	// 200 ms caller deadline -- and that arrangement stopped existing when the budget started to
	// bound the call: the attempt is now cut to the 50 ms, so the call ends at the budget before
	// the caller has cancelled anything, and "reconnecting" is then the true answer. So the caller
	// now cancels INSIDE the one attempt, at a moment when what is left of the budget is shorter
	// than a pause: a loop that did not ask the caller's context would take its last-chance
	// attempt with a dead context, fail it at once, and answer ErrReconnecting -- telling a caller
	// that cancelled to ask again.
	policy := ConnectPolicy{
		Budget:         200 * time.Millisecond,
		AttemptTimeout: 10 * time.Second,
		FirstBackoff:   100 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
	}
	device, _ := newUnansweredDevice(t, policy)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := device.Connect(ctx)
	elapsed := time.Since(started)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a cancelled reconnect answered %v, want the caller's own context error", err)
	}
	if errors.Is(err, ErrReconnecting) {
		t.Fatal("a caller that cancelled was told to ask again")
	}
	if 2*time.Second < elapsed {
		t.Fatalf("the cancellation took %v to be noticed", elapsed)
	}
	t.Logf("cancelled after %v, inside the one attempt, with less of the budget left than a pause: %v", elapsed.Round(time.Millisecond), err)
}

// AN ATTEMPT THAT FAILS AT ONCE DOES NOT TURN THE LAST STRETCH OF THE BUDGET INTO A TIGHT LOOP.
//
// The last-chance attempt is taken WITHOUT a pause, because a pause that no attempt follows only
// lengthens the block. That is safe only while the last chance ENDS the call whatever it answers:
// against a transport that refuses a frame instantly -- a dead socket, not the operator's silence
// -- an attempt returns in microseconds, and a loop that went on taking pause-free attempts while
// any budget was left would send thousands of Hellos in its last few hundred milliseconds.
//
// WHAT WOULD GO RED: take lastChance out of Device.Connect's `final`.
func TestAnAttemptThatFailsAtOnceIsNotRetriedInATightLoop(t *testing.T) {
	var attempts int
	var mutex sync.Mutex
	streamStore, err := sdk.OpenStreamStore(t.TempDir())
	if err != nil {
		t.Fatalf("sdk.OpenStreamStore: %v", err)
	}
	t.Cleanup(func() { streamStore.Close() })
	device, err := NewDevice(DeviceConfig{
		Transport: newSilentTransport(t),
		Reserver:  sdk.NewStreamIndexReserver(streamStore),
		Connect: ConnectPolicy{
			Budget:         300 * time.Millisecond,
			AttemptTimeout: time.Second,
			FirstBackoff:   200 * time.Millisecond,
			MaxBackoff:     200 * time.Millisecond,
			OnAttempt: func(ConnectAttempt) {
				mutex.Lock()
				attempts += 1
				mutex.Unlock()
			},
		},
	})
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}
	t.Cleanup(func() { device.Close() })
	started := time.Now()
	err = device.Connect(context.Background())
	elapsed := time.Since(started)
	mutex.Lock()
	defer mutex.Unlock()
	if errors.Is(err, ErrHelloRefused) {
		t.Fatalf("a frame the transport refused was reported as the server refusing: %v", err)
	}
	if 10 < attempts {
		t.Fatalf("%d Hello attempts in %v against a transport that fails at once", attempts, elapsed)
	}
	if time.Second < elapsed {
		t.Fatalf("took %v against a 300ms budget", elapsed)
	}
	t.Logf("%d attempts in %v: %v", attempts, elapsed.Round(time.Millisecond), err)
}

// AND THE DEFAULT BUDGET COVERS THE WINDOW IT EXISTS FOR.
//
// THE MEASURED EDGE IS ABOUT 61 SECONDS. A default under that would be a bound that does not cover
// its own case, which is the thing worth gating: the constant is easy to "tidy" downward by
// somebody who has not read the measurement, and a 30-second default would look perfectly
// reasonable and would fail every time.
//
// WHAT WOULD GO RED IF THE DEFAULT WERE SHORTENED BELOW THE WINDOW: exactly this.
func TestTheDefaultConnectBudgetCoversTheMeasuredWindow(t *testing.T) {
	const measuredWindow = 61 * time.Second
	policy := ConnectPolicy{}.withDefaults()
	if policy.Budget < measuredWindow {
		t.Fatalf("the default budget is %v and the measured reconnect window is %v", policy.Budget, measuredWindow)
	}
	if policy.AttemptTimeout >= policy.Budget {
		t.Fatalf("one attempt (%v) is the whole budget (%v), so nothing is retried", policy.AttemptTimeout, policy.Budget)
	}
	// and the defaults buy more than a couple of attempts across the window, which is the
	// difference between a retry and a longer single timeout.
	attempts, spent := 0, time.Duration(0)
	backoff := policy.FirstBackoff
	for spent < policy.Budget {
		attempts += 1
		spent += policy.AttemptTimeout + backoff
		if backoff < policy.MaxBackoff {
			backoff *= 2
			if policy.MaxBackoff < backoff {
				backoff = policy.MaxBackoff
			}
		}
	}
	if attempts < 4 {
		t.Fatalf("the defaults buy %d attempts across %v", attempts, policy.Budget)
	}
	t.Logf("the defaults buy %d Hello attempts across %v, covering a measured %v window",
		attempts, policy.Budget, measuredWindow)
}

// ── a client that ANSWERS, so that "an answer is not silence" is driven and not argued ───────

// answeringClient answers every Hello with a reason and a nonce of the case's choosing, from
// inside SendWithTimeout. It is the smallest thing that can put a SPOKEN answer in front of
// [Device.Connect]; everything else about the transport under it is the real one.
type answeringClient struct {
	mutex   sync.Mutex
	receive connect.ReceiveFunction
	reason  protocol.Reason
	nonce   []byte
	answers int
}

func (self *answeringClient) AddReceiveCallback(receiveCallback connect.ReceiveFunction) func() {
	self.mutex.Lock()
	self.receive = receiveCallback
	self.mutex.Unlock()
	return func() {}
}

func (self *answeringClient) SendWithTimeout(frame *protocol.Frame, destination connect.Id,
	ackCallback connect.AckFunction, timeout time.Duration, opts ...any) bool {

	if frame.GetMessageType() != protocol.MessageType_MessageMessageServerRequest {
		return true
	}
	request := &protocol.MessageServerRequest{}
	if proto.Unmarshal(frame.GetMessageBytes(), request) != nil {
		return false
	}
	self.mutex.Lock()
	receive, reason, nonce := self.receive, self.reason, self.nonce
	self.answers += 1
	self.mutex.Unlock()
	if receive == nil {
		return false
	}
	response := &protocol.MessageServerResponse{RequestId: request.GetRequestId(), Reason: reason}
	if reason == protocol.Reason_REASON_OK {
		response.Body = &protocol.MessageServerResponse_Hello{
			Hello: &protocol.HelloResponse{ServerNonce: nonce},
		}
	}
	encoded, err := proto.Marshal(response)
	if err != nil {
		return false
	}
	// delivered on ANOTHER goroutine, because this transport is still inside its own `send`
	// here: its waiter is registered but it has not reached the select that reads it.
	go receive(connect.TransferPath{}, []*protocol.Frame{{
		MessageType:  protocol.MessageType_MessageMessageServerResponse,
		MessageBytes: encoded,
	}}, connect.Peer{})
	return true
}

func (self *answeringClient) answered() int {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.answers
}

func newAnsweringDevice(t *testing.T, reason protocol.Reason, nonce []byte) (*Device, *answeringClient) {
	t.Helper()
	client := &answeringClient{reason: reason, nonce: nonce}
	transport, err := sdk.NewMessageTransport(&sdk.MessageTransportConfig{
		Client:          client,
		Server:          connect.NewId(),
		ProtocolVersion: 1,
	})
	if err != nil {
		t.Fatalf("sdk.NewMessageTransport: %v", err)
	}
	t.Cleanup(transport.Close)
	streamStore, err := sdk.OpenStreamStore(t.TempDir())
	if err != nil {
		t.Fatalf("sdk.OpenStreamStore: %v", err)
	}
	t.Cleanup(func() { streamStore.Close() })
	device, err := NewDevice(DeviceConfig{
		Transport: transport,
		Reserver:  sdk.NewStreamIndexReserver(streamStore),
		// a budget long enough that one trip through the retry loop would be
		// UNMISTAKABLE in the elapsed time below.
		Connect: ConnectPolicy{
			Budget:         30 * time.Second,
			AttemptTimeout: 2 * time.Second,
			FirstBackoff:   200 * time.Millisecond,
			MaxBackoff:     200 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}
	t.Cleanup(func() { device.Close() })
	return device, client
}

// AN ANSWER IS NOT SILENCE, AND NEITHER SHAPE OF ANSWER IS RETRIED.
//
// THIS IS THE CLAUSE THAT MAKES THE RETRY SAFE. The window produces SILENCE -- the request goes out
// and nothing at all comes back. A server that SPEAKS is not in that state, so retrying it for
// ninety seconds would turn one clear answer into a hang and would hide a real fault behind a wait.
//
// TWO SHAPES, BOTH DRIVEN HERE. A Hello refused by REASON is [ErrHelloRefused]; a Hello answered
// REASON_OK carrying no server_nonce is [ErrNotConnected], because every authenticator in this
// protocol is a MAC over that nonce and a connection without one is unusable rather than pending.
//
// THE MEASUREMENT IS THE ANSWER COUNT AND THE CLOCK, not only the error: a loop that retried and
// happened to return the same error at the end is caught by both.
//
// WHAT WOULD GO RED IF AN ANSWERED REFUSAL WERE RETRIED: answered() is many rather than one, and
// this case takes thirty seconds.
func TestAnAnsweredHelloIsNotRetried(t *testing.T) {
	for _, one := range []struct {
		name   string
		reason protocol.Reason
		nonce  []byte
		want   error
	}{
		{"a refusal by reason", protocol.Reason_REASON_UNSUPPORTED_VERSION, nil, ErrHelloRefused},
		{"REASON_OK and no server_nonce", protocol.Reason_REASON_OK, nil, ErrNotConnected},
	} {
		t.Run(one.name, func(t *testing.T) {
			device, client := newAnsweringDevice(t, one.reason, one.nonce)
			started := time.Now()
			err := device.Connect(context.Background())
			elapsed := time.Since(started)

			if !errors.Is(err, one.want) {
				t.Fatalf("%s answered %v, want one wrapping %v", one.name, err, one.want)
			}
			if errors.Is(err, ErrReconnecting) {
				t.Errorf("%s was reported as a wait rather than as an answer", one.name)
			}
			if answers := client.answered(); answers != 1 {
				t.Fatalf("%s was sent %d Hellos; an answer must not be retried", one.name, answers)
			}
			if time.Second < elapsed {
				t.Fatalf("%s took %v, so it went through the retry loop", one.name, elapsed)
			}
			t.Logf("%s: one Hello, %v, %v", one.name, elapsed.Round(time.Millisecond), err)
		})
	}
}

// AND A HELLO THAT IS ANSWERED PROPERLY CONNECTS ON THE FIRST ATTEMPT, which is the control that
// stops every case above from passing over a Connect that never succeeds at all.
func TestAnAnsweredHelloConnectsOnTheFirstAttempt(t *testing.T) {
	device, client := newAnsweringDevice(t, protocol.Reason_REASON_OK, bytes.Repeat([]byte{0x7C}, 32))
	started := time.Now()
	if err := device.Connect(context.Background()); err != nil {
		t.Fatalf("a Hello answered with a nonce did not connect: %v", err)
	}
	elapsed := time.Since(started)
	if answers := client.answered(); answers != 1 {
		t.Fatalf("connecting took %d Hellos", answers)
	}
	if time.Second < elapsed {
		t.Fatalf("connecting took %v", elapsed)
	}
	if len(device.transport.Nonce()) == 0 {
		t.Fatal("the device connected and holds no server_nonce")
	}
	t.Logf("one Hello, %v, connected", elapsed.Round(time.Millisecond))
}
