//go:build !ios && !android && !js

package sdk

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
	"github.com/urnetwork/connect/protocol"
)

// What the role does when its server loses a carrier (EXTENDER.md G2). On 2026-11-01
// extenders whose servers had probably closed reported listening and kept posting
// activations, for good, while their processes stayed connected as providers. The
// server now rebuilds a lost carrier itself (connect/extender/extender_carrier.go);
// these are what the role says meanwhile, what it posts, and what it does about a
// carrier that does not come back.

// A tcp listener that a test can kill from outside: Accept fails with an error that no
// retry helps, and the role's process is otherwise untouched.
type testCarrierListener struct {
	net.Listener
	failed   chan struct{}
	failOnce sync.Once
}

func newTestCarrierListener(listener net.Listener) *testCarrierListener {
	return &testCarrierListener{
		Listener: listener,
		failed:   make(chan struct{}),
	}
}

func (self *testCarrierListener) fail() {
	self.failOnce.Do(func() {
		close(self.failed)
		// wakes a blocked Accept, and frees the port
		self.Listener.Close()
	})
}

func (self *testCarrierListener) Accept() (net.Conn, error) {
	select {
	case <-self.failed:
		return nil, fmt.Errorf("the test's listener failed")
	default:
	}
	conn, err := self.Listener.Accept()
	select {
	case <-self.failed:
		return nil, fmt.Errorf("the test's listener failed")
	default:
	}
	return conn, err
}

// The tcp binds of a role, the first of which a test can kill, and each of which
// can be held or refused.
type testCarrierBinds struct {
	stateLock sync.Mutex
	listeners []*testCarrierListener
	// while set, a bind waits on this
	hold chan struct{}
	// while set, a bind answers this
	refuse error
	count  atomic.Int64
}

func (self *testCarrierBinds) current() *testCarrierListener {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.listeners[len(self.listeners)-1]
}

func (self *testCarrierBinds) setHold(hold chan struct{}) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.hold = hold
}

func (self *testCarrierBinds) setRefuse(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.refuse = err
}

func (self *testCarrierBinds) configure(settings *deviceLocalExtenderSettings) {
	listen := settings.Listen
	settings.Listen = func(network string, address string) (net.Listener, error) {
		self.count.Add(1)
		hold, refuse := func() (chan struct{}, error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.hold, self.refuse
		}()
		if hold != nil {
			<-hold
		}
		if refuse != nil {
			return nil, refuse
		}
		listener, err := listen(network, address)
		if err != nil {
			return nil, err
		}
		carrierListener := newTestCarrierListener(listener)
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.listeners = append(self.listeners, carrierListener)
		return carrierListener, nil
	}
}

// Rebuilds in moments, so a test waits on what it holds and not on a backoff, and
// restores the setting when it ends.
func testFastExtenderCarrierRebuild(t *testing.T) {
	t.Helper()
	configureExtenderServer = func(settings *extender.ExtenderSettings) {
		settings.CarrierRebuildMinBackoff = 5 * time.Millisecond
		settings.CarrierRebuildMaxBackoff = 20 * time.Millisecond
	}
	t.Cleanup(func() { configureExtenderServer = nil })
}

// Waits out the first pass of a role that activates both families.
func (self *testProvideExtenderFixture) waitActivated() {
	self.t.Helper()
	self.waitPass()
	self.waitPost()
	self.waitPost()
	self.waitStatus("activated", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening && status.ActivatedV4 && status.ActivatedV6
	})
}

// The seam of a role's asking to be replaced: every wait it asks for is recorded,
// and the wait ends only when the test sends on `fire`.
func testRestartAfter(settings *deviceLocalExtenderSettings, waits chan time.Duration, fire chan time.Time) {
	settings.RestartAfter = func(wait time.Duration) <-chan time.Time {
		waits <- wait
		return fire
	}
}

// A role's replacement posts nothing for a record still good within a test: the post
// it owes goes within this spread, and one in a million lands in the minutes a test
// runs.
func testNoReplacedPost(settings *deviceLocalExtenderSettings) {
	settings.ConfigureActivator = func(settings *connect.ExtenderActivatorSettings) {
		settings.ReplacedPostJitter = 24 * 365 * time.Hour
	}
}

// The node of an extender that activated both families, once its swaps are done.
func testExtenderNodeOfBothFamilies(fixture *testProvideExtenderFixture) *spaceExtenderNode {
	fixture.t.Helper()
	fixture.waitNodeListenAddrs([]string{
		fmt.Sprintf("/ip4/%s/tcp/%d", testProvideExtenderIpv4, fixture.tcpPort),
		fmt.Sprintf("/ip6/%s/tcp/%d", testProvideExtenderIpv6, fixture.tcpPort),
	})
	return fixture.networkSpace.getExtenderNode()
}

// A lost tcp carrier is not listening in the status, with the cause, and posts
// nothing. It comes back by itself and, with nothing owed, posts nothing either: the
// record is still good, and the role does not spend the operator's budgets for a
// flap.
func TestDeviceLocalProviderExtenderReportsALostTcpCarrier(t *testing.T) {
	testFastExtenderCarrierRebuild(t)
	binds := &testCarrierBinds{}
	waits := make(chan time.Duration, 16)
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		binds.configure(settings)
		testRestartAfter(settings, waits, make(chan time.Time))
		settings.ConfigureActivator = func(settings *connect.ExtenderActivatorSettings) {
			settings.CarrierReturnJitter = -1
		}
	})
	fixture.waitActivated()
	postCountV4, postCountV6 := fixture.operator.postCount(4), fixture.operator.postCount(6)
	node := testExtenderNodeOfBothFamilies(fixture)

	holdBind := make(chan struct{})
	binds.setHold(holdBind)
	binds.current().fail()

	status := fixture.waitStatus("not listening", func(status *ExtenderProvideStatus) bool {
		return status.ErrorCase == ExtenderProvideErrorListen
	})
	if status.Listening || status.State != ExtenderProvideStateError {
		t.Fatalf("status = %+v, expected a role that is not listening", status)
	}
	if !strings.Contains(status.ListenError, "the test's listener failed") ||
		!strings.HasPrefix(status.ListenError, connect.ExtenderCarrierTcp+": ") ||
		status.Reason != status.ListenError {
		t.Fatalf("status = %+v, expected what ended the listener as the reason", status)
	}
	if status.ActivatedV4 || status.ActivatedV6 || status.Ipv4 != "" || status.Ipv6 != "" {
		t.Fatalf("status = %+v, expected no family to read as active", status)
	}
	// the port is not known to be anyone else's, and the role goes on running
	if status.TcpUnavailableError != "" || !status.Enabled {
		t.Fatalf("status = %+v, expected a running role", status)
	}
	// and it did not take the udp carriers with it
	if carriers := fixture.extender().currentServer().Carriers(); !slices.Equal(carriers, []string{
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	}) {
		t.Fatalf("carriers = %v, expected the udp carriers", carriers)
	}
	wait := <-waits
	if wait < extenderProvideTcpLostTimeout+extenderProvideRestartFloor ||
		extenderProvideTcpLostTimeout+extenderProvideRestartFloor+extenderProvideRestartMinJitter <= wait {
		t.Fatalf("the role would ask to be replaced after %s", wait)
	}

	// no post for a listener that is gone, whatever comes due
	fixture.extender().currentActivator().Reactivate()
	fixture.step(2 * time.Minute)
	fixture.step(2 * time.Minute)
	if fixture.operator.postCount(4) != postCountV4 || fixture.operator.postCount(6) != postCountV6 {
		t.Fatalf(
			"posts = %d and %d with the listener gone, expected %d and %d",
			fixture.operator.postCount(4), fixture.operator.postCount(6), postCountV4, postCountV6)
	}

	close(holdBind)
	binds.setHold(nil)
	fixture.waitStatus("listening again", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening && status.ListenError == "" &&
			status.ActivatedV4 && status.ActivatedV6 && status.State == ExtenderProvideStateActive
	})
	if count := binds.count.Load(); count != 2 {
		t.Fatalf("tcp binds = %d, expected the one rebuild", count)
	}
	// the addresses never moved, so the node stayed through the flap, and its peers
	// with it
	if current := fixture.networkSpace.getExtenderNode(); current != node {
		t.Fatal("the node was rebuilt for a flap")
	}
}

// A revocation that arrived while the listener was gone is what the listener's return
// must answer: its post was held back, and nothing but the return or the next look
// at the floor would post it.
func TestDeviceLocalProviderExtenderPostsOnceTheTcpCarrierReturns(t *testing.T) {
	testFastExtenderCarrierRebuild(t)
	binds := &testCarrierBinds{}
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		binds.configure(settings)
		testRestartAfter(settings, make(chan time.Duration, 16), make(chan time.Time))
		settings.ConfigureActivator = func(settings *connect.ExtenderActivatorSettings) {
			settings.CarrierReturnJitter = -1
		}
	})
	fixture.waitActivated()
	postCountV4 := fixture.operator.postCount(4)

	holdBind := make(chan struct{})
	binds.setHold(holdBind)
	binds.current().fail()
	fixture.waitStatus("not listening", func(status *ExtenderProvideStatus) bool {
		return !status.Listening
	})

	fixture.clock.advance(1 * time.Hour)
	revocation, err := connect.SignExtenderRevocation(
		fixture.rootPrivateKey,
		&protocol.ExtenderRevocationBody{
			PublicKey:   fixture.extender().publicKey,
			IssueTimeMs: uint64(fixture.clock.Now().UnixMilli()),
			NetworkHost: testProvideExtenderHost,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.networkSpace.extenderDirectory.ApplyRevocation(revocation); err != nil {
		t.Fatal(err)
	}
	fixture.step(2 * time.Minute)
	fixture.step(30 * time.Minute)
	if count := fixture.operator.postCount(4); count != postCountV4 {
		t.Fatalf("posts = %d for a revoked listener that is gone, expected %d", count, postCountV4)
	}

	// the next look is minutes off on a clock that is not moving: only the return can
	// post
	close(holdBind)
	binds.setHold(nil)
	for range 2 {
		fixture.waitPost()
	}
	fixture.waitStatus("activated again", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening && status.ActivatedV4 && status.ActivatedV6 && status.RevokedTime == 0
	})
}

// A port that stays lost is what G2 says of one that cannot be bound: the role is
// replaced by one that is off, holding no udp carrier, saying why and trying the port
// every three minutes, and the port's return starts it.
func TestDeviceLocalProviderExtenderReplacedWhenTheTcpPortIsTaken(t *testing.T) {
	testFastExtenderCarrierRebuild(t)
	binds := &testCarrierBinds{}
	restartWaits := make(chan time.Duration, 16)
	restartFire := make(chan time.Time)
	retryWaits := make(chan time.Duration, 16)
	retry := make(chan time.Time)
	udpBinds := make(chan int, 64)
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		binds.configure(settings)
		testRestartAfter(settings, restartWaits, restartFire)
		testTcpRetry(settings, retryWaits, retry)
		testRecordUdpBinds(settings, udpBinds)
		testNoReplacedPost(settings)
	})
	fixture.waitActivated()
	firstRole := fixture.extender()
	postCountV4 := fixture.operator.postCount(4)

	// the holder's rebuild loses the port to another process of this host
	binds.setRefuse(fmt.Errorf("bind: address already in use"))
	binds.current().fail()
	status := fixture.waitStatus("not listening", func(status *ExtenderProvideStatus) bool {
		return status.ErrorCase == ExtenderProvideErrorListen &&
			strings.Contains(status.ListenError, "address already in use")
	})
	if status.Listening {
		t.Fatalf("status = %+v", status)
	}
	<-restartWaits
	testDrainUdpBinds(udpBinds)

	restartFire <- time.Time{}
	testExtenderRoleReplaced(t, fixture, firstRole)
	status = fixture.waitStatus("the replacement off for want of tcp", func(status *ExtenderProvideStatus) bool {
		return status.ErrorCase == ExtenderProvideErrorTcpUnavailable &&
			strings.Contains(status.TcpUnavailableError, "address already in use") &&
			!status.Enabled
	})
	if status.Listening || status.ActivatedV4 || status.ActivatedV6 || status.DnsPorts != "" {
		t.Fatalf("status = %+v, expected the replacement off", status)
	}
	<-retryWaits
	if udpPorts := testDrainUdpBinds(udpBinds); 0 < len(udpPorts) {
		t.Fatalf("the replacement bound udp %v without tcp 443", udpPorts)
	}

	// the other process exits, and the retry takes the port. The role that was off in
	// between had no activation, so it handed on what the first one held: the record
	// was good all along, and nothing is posted for it
	binds.setRefuse(nil)
	retry <- time.Time{}
	fixture.waitStatus("the replacement serving", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening && status.ActivatedV4 && status.ActivatedV6
	})
	if binds.current() == nil {
		t.Fatal("the replacement holds no listener")
	}
	fixture.step(2 * time.Minute)
	if count := fixture.operator.postCount(4); count != postCountV4 {
		t.Fatalf("posts = %d for a record still good, expected %d", count, postCountV4)
	}
}

// A carrier that came back before the wait ended cancels the replacement.
func TestDeviceLocalProviderExtenderKeepsTheRoleIfTheTcpPortReturns(t *testing.T) {
	testFastExtenderCarrierRebuild(t)
	binds := &testCarrierBinds{}
	restartWaits := make(chan time.Duration, 16)
	restartFire := make(chan time.Time, 1)
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		binds.configure(settings)
		testRestartAfter(settings, restartWaits, restartFire)
	})
	fixture.waitActivated()
	role := fixture.extender()

	holdBind := make(chan struct{})
	binds.setHold(holdBind)
	binds.current().fail()
	fixture.waitStatus("not listening", func(status *ExtenderProvideStatus) bool {
		return !status.Listening && status.ErrorCase == ExtenderProvideErrorListen
	})
	<-restartWaits
	close(holdBind)
	binds.setHold(nil)
	fixture.waitStatus("listening again", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening
	})

	// the wait ends after the return, and nothing is waiting on it
	restartFire <- time.Time{}
	time.Sleep(100 * time.Millisecond)
	if current := fixture.extender(); current != role {
		t.Fatal("the role was replaced after its carrier came back")
	}
}

// A server gone for good, which nothing ends but a bug, is the role's to replace: the
// status stops saying listening, the activation stops posting, and a new role serves.
// It keeps what the old one's activation held, so a fleet replaced together does not
// post together. The change listener hears the new role too.
func TestDeviceLocalProviderExtenderReplacedWhenItsServerEnds(t *testing.T) {
	binds := &testCarrierBinds{}
	restartWaits := make(chan time.Duration, 16)
	restartFire := make(chan time.Time)
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		binds.configure(settings)
		testRestartAfter(settings, restartWaits, restartFire)
		// far enough from the lost port's wait to tell the two apart
		settings.RestartJitter = 1 * time.Minute
		testNoReplacedPost(settings)
	})
	fixture.waitActivated()
	firstRole := fixture.extender()
	postCountV4 := fixture.operator.postCount(4)

	firstRole.currentServer().Close()
	status := fixture.waitStatus("the server ended", func(status *ExtenderProvideStatus) bool {
		return status.ErrorCase == ExtenderProvideErrorListen
	})
	if status.Listening || !strings.Contains(status.Reason, "the extender server ended") {
		t.Fatalf("status = %+v", status)
	}
	if carriers := firstRole.currentServer().Carriers(); len(carriers) != 0 {
		t.Fatalf("carriers = %v of a server that ended", carriers)
	}
	testExtenderRestartWaitUnder(t, restartWaits, extenderProvideRestartFloor+1*time.Minute)

	firstRole.currentActivator().Reactivate()
	fixture.step(2 * time.Minute)
	if count := fixture.operator.postCount(4); count != postCountV4 {
		t.Fatalf("posts = %d for a server that ended, expected %d", count, postCountV4)
	}

	restartFire <- time.Time{}
	testExtenderRoleReplaced(t, fixture, firstRole)
	fixture.waitStatus("the replacement serving", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening && status.ActivatedV4 && status.ActivatedV6
	})
	serving := fixture.extender().currentServer()
	if !slices.Contains(serving.Carriers(), connect.ExtenderCarrierTcp) {
		t.Fatalf("carriers = %v of the replacement", serving.Carriers())
	}
	fixture.step(2 * time.Minute)
	if count := fixture.operator.postCount(4); count != postCountV4 {
		t.Fatalf("posts = %d after the replacement, expected %d", count, postCountV4)
	}

	// turning the role off and on is not a replacement: it starts as a new one does
	fixture.device.SetProvideExtender(false)
	fixture.device.SetProvideExtender(true)
	fixture.waitPost()
}

// The role's wait for a port whose loss it reads first: the role bound tcp before its
// server started, so a first read that finds it gone is a port already lost, and
// taking that read as the starting state would see no change, and never replace it.
func TestDeviceLocalProviderExtenderReplacedWhenTheTcpPortIsLostBeforeItsFirstRead(t *testing.T) {
	testFastExtenderCarrierRebuild(t)
	binds := &testCarrierBinds{}
	waits := make(chan time.Duration, 16)
	activatorStarting := make(chan struct{})
	portLost := make(chan struct{})
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		binds.configure(settings)
		testRestartAfter(settings, waits, make(chan time.Time))
		// the one step of the role's loop between the bind and its first read
		settings.ConfigureActivator = func(settings *connect.ExtenderActivatorSettings) {
			close(activatorStarting)
			<-portLost
		}
	})
	select {
	case <-activatorStarting:
	case <-time.After(60 * time.Second):
		t.Fatal("the role did not start its activation")
	}
	holdBind := make(chan struct{})
	t.Cleanup(func() { close(holdBind) })
	binds.setHold(holdBind)
	binds.current().fail()
	deadline := time.Now().Add(60 * time.Second)
	for {
		// the provider has the role a moment after the role has started
		role := fixture.extender()
		if role != nil && !slices.Contains(role.currentServer().Carriers(), connect.ExtenderCarrierTcp) {
			break
		}
		if deadline.Before(time.Now()) {
			t.Fatal("the server did not lose its tcp carrier")
		}
		time.Sleep(time.Millisecond)
	}
	close(portLost)

	select {
	case wait := <-waits:
		if wait < extenderProvideTcpLostTimeout {
			t.Fatalf("the role would ask to be replaced after %s", wait)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the role never armed its wait for the lost port")
	}
}

// A role replaced while the operator has said to wait goes on waiting, whatever
// else the replacement starts. This is what kept a fleet of replaced roles from posting
// at once.
func TestDeviceLocalProviderExtenderReplacementKeepsARefusalsHold(t *testing.T) {
	binds := &testCarrierBinds{}
	restartFire := make(chan time.Time)
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		binds.configure(settings)
		testRestartAfter(settings, make(chan time.Duration, 16), restartFire)
	})
	fixture.waitActivated()
	firstRole := fixture.extender()

	fixture.operator.setRefusal("the test's refusal")
	firstRole.currentActivator().Reactivate()
	fixture.waitPost()
	fixture.waitPost()
	fixture.waitStatus("refused", func(status *ExtenderProvideStatus) bool {
		return status.ErrorCase == ExtenderProvideErrorActivationRefused
	})
	postCountV4 := fixture.operator.postCount(4)

	firstRole.currentServer().Close()
	restartFire <- time.Time{}
	testExtenderRoleReplaced(t, fixture, firstRole)
	fixture.waitStatus("the refusal carried over", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening && status.ErrorCase == ExtenderProvideErrorActivationRefused
	})
	fixture.step(time.Minute)
	fixture.step(time.Minute)
	if count := fixture.operator.postCount(4); count != postCountV4 {
		t.Fatalf("posts = %d inside the hold, expected %d", count, postCountV4)
	}
}

// What the role hears of its carriers reaches the activation: a post made with the
// quic carrier being rebuilt leaves it off the record, and its return posts the family
// again rather than leaving that for the tick.
func TestDeviceLocalProviderExtenderPostsARecordMissingACarrierWhenItReturns(t *testing.T) {
	testFastExtenderCarrierRebuild(t)
	holdQuicBind := make(chan struct{})
	holdingQuicBind := &atomic.Bool{}
	var quicConn atomic.Pointer[net.PacketConn]
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		quicAddressSuffix := fmt.Sprintf(":%d", settings.UdpPort)
		listenPacket := settings.ListenPacket
		settings.ListenPacket = func(network string, address string) (net.PacketConn, error) {
			quic := strings.HasSuffix(address, quicAddressSuffix)
			if quic && holdingQuicBind.Load() {
				<-holdQuicBind
			}
			conn, err := listenPacket(network, address)
			if quic && err == nil {
				quicConn.Store(&conn)
			}
			return conn, err
		}
		settings.ConfigureActivator = func(settings *connect.ExtenderActivatorSettings) {
			settings.CarrierReturnJitter = -1
		}
	})
	fixture.waitActivated()

	holdingQuicBind.Store(true)
	(*quicConn.Load()).Close()
	deadline := time.Now().Add(60 * time.Second)
	for slices.Contains(fixture.extender().currentServer().Carriers(), connect.ExtenderCarrierQuic) {
		if deadline.Before(time.Now()) {
			t.Fatal("the server did not lose its quic carrier")
		}
		time.Sleep(time.Millisecond)
	}
	fixture.extender().currentActivator().Reactivate()
	for range 2 {
		if post := fixture.waitPost(); slices.Contains(post.args.Carriers, connect.ExtenderCarrierQuic) {
			t.Fatalf("post offered %v with the carrier down", post.args.Carriers)
		}
	}

	close(holdQuicBind)
	for range 2 {
		if post := fixture.waitPost(); !slices.Contains(post.args.Carriers, connect.ExtenderCarrierQuic) {
			t.Fatalf("post offered %v with the carrier back", post.args.Carriers)
		}
	}
}

// A replacement asked for when the role is no longer the one running changes nothing:
// the setting that stopped the role stands.
func TestDeviceLocalProviderExtenderRestartOfAStoppedRoleIsNothing(t *testing.T) {
	fixture := newTestProvideExtenderFixture(t, nil)
	fixture.waitActivated()
	role := fixture.extender()

	fixture.device.SetProvideExtender(false)
	if fixture.extender() != nil {
		t.Fatal("the setting did not stop the role")
	}
	fixture.device.provider.restartExtender(role)
	if fixture.extender() != nil {
		t.Fatal("a replacement of a stopped role started one")
	}
}

// A role whose loop panics is replaced at the pace any other is, not as fast as it
// can fail: a loop that panics as it starts, with nothing to wait on, would otherwise
// rebuild the role, and post its activations, thousands of times a second. It is not
// replaced until the wait ends, the wait has its floor, and the spread doubles while
// the replacements do not last.
func TestDeviceLocalProviderExtenderPanicIsPaced(t *testing.T) {
	tcpPort := testFreeTcpPort(t)
	testHoldTcpPort(t, tcpPort)
	builds := &atomic.Int64{}
	waits := make(chan time.Duration, 16)
	fire := make(chan time.Time)
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		builds.Add(1)
		settings.TcpPort = tcpPort
		// the loop's first wait, which with the port held is the first thing it does
		settings.TcpRetryAfter = func(wait time.Duration) <-chan time.Time {
			panic("the test's panic in the role's loop")
		}
		testRestartAfter(settings, waits, fire)
	})

	// each role asks, and the next one is built only when its wait ends
	for i, jitter := range []time.Duration{
		extenderProvideRestartMinJitter,
		extenderProvideRestartMinJitter,
		2 * extenderProvideRestartMinJitter,
		4 * extenderProvideRestartMinJitter,
	} {
		var wait time.Duration
		select {
		case wait = <-waits:
		case <-time.After(60 * time.Second):
			t.Fatalf("role %d did not ask to be replaced", i)
		}
		if wait < extenderProvideRestartFloor || extenderProvideRestartFloor+jitter <= wait {
			t.Fatalf("role %d would ask after %s, expected the floor and up to %s", i, wait, jitter)
		}
		time.Sleep(20 * time.Millisecond)
		if count := builds.Load(); count != int64(i+1) {
			t.Fatalf("%d roles built while role %d waits, expected %d", count, i, i+1)
		}
		// the status says so through the wait, which can be an hour, without what the
		// panic said, which may name anything
		fixture.waitStatus("the failed role", func(status *ExtenderProvideStatus) bool {
			return status.ErrorCase == ExtenderProvideErrorListen &&
				strings.Contains(status.Reason, errExtenderRoleLoopFailed.Error()) &&
				!strings.Contains(status.Reason, "the test's panic")
		})
		if status := fixture.device.GetExtenderProvideStatus(); status.Listening || status.ActivatedV4 || status.ActivatedV6 {
			t.Fatalf("status = %+v of a role that failed", status)
		}
		select {
		case fire <- time.Time{}:
		case <-time.After(60 * time.Second):
			t.Fatalf("role %d did not wait on its wait", i)
		}
	}
	// the last replacement is over once its role asks in turn, which leaves nothing
	// reading the test's seams when its cleanup restores them
	select {
	case <-waits:
	case <-time.After(60 * time.Second):
		t.Fatal("the last role did not ask to be replaced")
	}
	if fixture.extender() == nil && builds.Load() < 4 {
		t.Fatal("no role is running")
	}
}

// A device that closes while a failed role waits leaves nothing waiting.
func TestDeviceLocalProviderExtenderPanicWaitEndsWithTheDevice(t *testing.T) {
	tcpPort := testFreeTcpPort(t)
	testHoldTcpPort(t, tcpPort)
	builds := &atomic.Int64{}
	waits := make(chan time.Duration, 16)
	fire := make(chan time.Time, 1)
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		builds.Add(1)
		settings.TcpPort = tcpPort
		settings.TcpRetryAfter = func(wait time.Duration) <-chan time.Time {
			panic("the test's panic in the role's loop")
		}
		testRestartAfter(settings, waits, fire)
	})
	<-waits
	if err := fixture.device.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	fire <- time.Time{}
	time.Sleep(20 * time.Millisecond)
	if count := builds.Load(); count != 1 {
		t.Fatalf("%d roles built after the device closed", count)
	}
}

// Replacements that do not last spread further each time, so a role that ends at
// once is not rebuilt in a loop, and one that ran for a long time starts over.
func TestDeviceLocalProviderExtenderRestartSpreadDoubles(t *testing.T) {
	testFastExtenderCarrierRebuild(t)
	jitters := make(chan time.Duration, 16)
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		jitters <- settings.RestartJitter
	})
	fixture.waitActivated()
	provider := fixture.device.provider
	if jitter := <-jitters; jitter != 0 {
		t.Fatalf("first role's spread = %s, expected the default", jitter)
	}

	expected := []time.Duration{
		extenderProvideRestartMinJitter,
		2 * extenderProvideRestartMinJitter,
		4 * extenderProvideRestartMinJitter,
	}
	for _, expectedJitter := range expected {
		provider.restartExtender(fixture.extender())
		if jitter := <-jitters; jitter != expectedJitter {
			t.Fatalf("spread = %s, expected %s", jitter, expectedJitter)
		}
	}

	// a role that lasted starts over
	provider.extenderLock.Lock()
	provider.extenderStartTime = time.Now().Add(-extenderProvideRestartStableTimeout)
	provider.extenderLock.Unlock()
	provider.restartExtender(fixture.extender())
	if jitter := <-jitters; jitter != 0 {
		t.Fatalf("spread after a role that lasted = %s, expected the default", jitter)
	}
}

func testExtenderRestartWaitUnder(t *testing.T, waits chan time.Duration, maxWait time.Duration) {
	t.Helper()
	deadline := time.After(60 * time.Second)
	// the wait for the lost port may be asked first, on the way to the server's end
	for {
		select {
		case wait := <-waits:
			if wait < maxWait {
				return
			}
		case <-deadline:
			t.Fatal("the role did not ask to be replaced soon")
		}
	}
}

// Waits for the provider to be running another role than `role`.
func testExtenderRoleReplaced(t *testing.T, fixture *testProvideExtenderFixture, role *deviceLocalExtender) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if current := fixture.extender(); current != nil && current != role {
			return
		}
		if deadline.Before(time.Now()) {
			t.Fatal("the role was not replaced")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
