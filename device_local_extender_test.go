//go:build !ios && !android && !js

package sdk

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/gossip"
	"github.com/urnetwork/connect/protocol"
)

// The provider extender role in the sdk (EXTENDER.md G1 to G3, F3).
//
// The operator is two httptest servers, one per family loopback, exactly as
// the api-v4 and api-v6 hosts an activation is posted to, plus a third that
// answers `/hello`. The loop is single threaded, so the hello of pass n+1
// proves pass n ran to completion, which is what every cadence assertion here
// waits on rather than a clock.
//
// The carriers bind ephemeral loopback ports through the provider settings
// seam, and every address the operator publishes is a documentation address,
// so nothing outside the machine is touched.

// The addresses the fixture operator publishes for each family.
const (
	testProvideExtenderIpv4 = "198.51.100.11"
	testProvideExtenderIpv6 = "2001:db8::11"
)

// The space these tests run under, and the host its records are keyed by.
const testProvideExtenderHost = "space.example"

// Synthetic encoding tld of the dns carrier in these tests.
const testProvideExtenderDnsTld = "x.example."

// Turns the provider extender role on for one test. The suite keeps it off, so
// a test that turns providing on does not bind the carrier ports.
func testEnableExtenderProvideRole(t *testing.T) {
	t.Helper()
	extenderProvideRoleEnabled = true
	t.Cleanup(func() {
		extenderProvideRoleEnabled = false
	})
}

// testExtenderClock is the only clock the activation loop reads, so every
// cadence decision is a step rather than a wait.
type testExtenderClock struct {
	stateLock sync.Mutex
	now       time.Time
}

// The fixture clock starts at the real instant: the directory judges a
// record's expiry against the process clock, so a record signed in a fake
// epoch would be expired the moment it is applied.
func newTestExtenderClock() *testExtenderClock {
	return &testExtenderClock{now: time.Now()}
}

func (self *testExtenderClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testExtenderClock) advance(d time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(d)
}

// One activation post as the operator saw it.
type testProvideExtenderPost struct {
	ipVersion int
	args      *connect.ExtenderActivateArgs
}

// testProvideExtenderOperator serves `/network/extender-activate` and
// `/hello` for both families (C2, C7).
type testProvideExtenderOperator struct {
	clock          *testExtenderClock
	rootPrivateKey ed25519.PrivateKey

	v4Url    string
	v6Url    string
	helloUrl string

	posts  chan *testProvideExtenderPost
	hellos chan struct{}

	stateLock     sync.Mutex
	clientAddress string
	// non-empty refuses every activation with this message
	refusal string
	// the address published for each family, which a test moves to prove a
	// stale mesh address is dropped rather than kept (G2, D2)
	publishedIps map[int]string
	postCounts   map[int]int
	activatedIps map[int]string
	// keeps each signed record newer than the last, which is what the newest
	// wins rule of B5 needs when two activations land in the same instant
	issueSerial int
}

func newTestProvideExtenderOperator(
	t *testing.T,
	clock *testExtenderClock,
	rootPrivateKey ed25519.PrivateKey,
) *testProvideExtenderOperator {
	t.Helper()
	operator := &testProvideExtenderOperator{
		clock:          clock,
		rootPrivateKey: rootPrivateKey,
		posts:          make(chan *testProvideExtenderPost, 64),
		hellos:         make(chan struct{}, 64),
		clientAddress:  testProvideExtenderIpv4 + ":41001",
		publishedIps: map[int]string{
			4: testProvideExtenderIpv4,
			6: testProvideExtenderIpv6,
		},
		postCounts:   map[int]int{},
		activatedIps: map[int]string{},
	}
	v4Server := newTestProvideExtenderServer(t, "127.0.0.1", operator.familyHandler(4))
	v6Server := newTestProvideExtenderServer(t, "[::1]", operator.familyHandler(6))
	helloServer := newTestProvideExtenderServer(t, "127.0.0.1", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			select {
			case operator.hellos <- struct{}{}:
			default:
			}
			operator.writeHello(w)
		}))
	t.Cleanup(func() {
		v4Server.Close()
		v6Server.Close()
		helloServer.Close()
	})
	operator.v4Url = v4Server.URL
	operator.v6Url = v6Server.URL
	operator.helloUrl = helloServer.URL
	return operator
}

// One httptest server on the given loopback address, which is how a family api
// host is reached without any name service.
func newTestProvideExtenderServer(
	t *testing.T,
	loopbackHost string,
	handler http.Handler,
) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp", loopbackHost+":0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	return server
}

func (self *testProvideExtenderOperator) familyHandler(ipVersion int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hello" {
			self.writeHello(w)
			return
		}
		if r.URL.Path != connect.ExtenderActivatePath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		args := &connect.ExtenderActivateArgs{}
		if err := json.NewDecoder(r.Body).Decode(args); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		self.stateLock.Lock()
		refusal := self.refusal
		self.postCounts[ipVersion] += 1
		self.stateLock.Unlock()
		select {
		case self.posts <- &testProvideExtenderPost{ipVersion: ipVersion, args: args}:
		default:
		}

		if refusal != "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(&connect.ExtenderActivateResult{
				Activated: false,
				Error:     refusal,
			})
			return
		}
		result, err := self.activateResult(ipVersion, args)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})
}

func (self *testProvideExtenderOperator) writeHello(w http.ResponseWriter) {
	self.stateLock.Lock()
	clientAddress := self.clientAddress
	self.stateLock.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"client_address":%q}`, clientAddress)
}

// The signed answer of one activation: this extender's own record for every
// family it has activated so far (C2).
func (self *testProvideExtenderOperator) activateResult(
	ipVersion int,
	args *connect.ExtenderActivateArgs,
) (*connect.ExtenderActivateResult, error) {
	publicKey, err := connect.ParseExtenderPublicKeyHex(args.PublicKeyHex)
	if err != nil {
		return nil, err
	}
	now := self.clock.Now()

	self.stateLock.Lock()
	ip := self.publishedIps[ipVersion]
	self.activatedIps[ipVersion] = ip
	activatedIps := []string{}
	for _, activatedIpVersion := range []int{4, 6} {
		if activatedIp, ok := self.activatedIps[activatedIpVersion]; ok {
			activatedIps = append(activatedIps, activatedIp)
		}
	}
	self.issueSerial += 1
	issueTime := now.Add(time.Duration(self.issueSerial) * time.Millisecond)
	self.stateLock.Unlock()

	expireTime := now.Add(14 * 24 * time.Hour)
	addresses := []*protocol.ExtenderAddress{}
	for _, activatedIp := range activatedIps {
		addr := netip.MustParseAddr(activatedIp)
		addressIpVersion := 4
		if addr.Is6() {
			addressIpVersion = 6
		}
		addresses = append(addresses, &protocol.ExtenderAddress{
			Ip:        activatedIp,
			IpVersion: uint32(addressIpVersion),
			Carriers:  slices.Clone(args.Carriers),
		})
	}
	record, err := connect.SignExtenderRecord(self.rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey:    publicKey,
		Addresses:    addresses,
		TcpPort:      uint32(args.TcpPort),
		UdpPort:      uint32(args.UdpPort),
		DnsPort:      uint32(args.DnsPort),
		DnsTld:       args.DnsTld,
		CountryCode:  "zz",
		IssueTimeMs:  uint64(issueTime.UnixMilli()),
		ExpireTimeMs: uint64(expireTime.UnixMilli()),
		NetworkHost:  testProvideExtenderHost,
	})
	if err != nil {
		return nil, err
	}
	recordBytes, err := proto.Marshal(record)
	if err != nil {
		return nil, err
	}
	return &connect.ExtenderActivateResult{
		Activated:    true,
		Ip:           ip,
		IpVersion:    ipVersion,
		Carriers:     slices.Clone(args.Carriers),
		ExpireTime:   &expireTime,
		AllowedHosts: []string{testProvideExtenderHost, "*." + testProvideExtenderHost},
		Record:       base64.StdEncoding.EncodeToString(recordBytes),
	}, nil
}

func (self *testProvideExtenderOperator) setClientAddress(clientAddress string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clientAddress = clientAddress
}

// Moves the address this operator publishes for one family, which is what a
// host whose public address changed looks like from here.
func (self *testProvideExtenderOperator) setPublishedIp(ipVersion int, ip string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.publishedIps[ipVersion] = ip
}

func (self *testProvideExtenderOperator) setRefusal(refusal string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.refusal = refusal
}

func (self *testProvideExtenderOperator) postCount(ipVersion int) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.postCounts[ipVersion]
}

// testProvideExtenderFixture is one providing device whose extender role runs
// against the fixture operator on ephemeral loopback carriers.
type testProvideExtenderFixture struct {
	t     *testing.T
	clock *testExtenderClock

	rootPrivateKey ed25519.PrivateKey
	operator       *testProvideExtenderOperator

	networkSpaceManager *NetworkSpaceManager
	networkSpace        *NetworkSpace
	device              *DeviceLocal
	storagePath         string

	tcpPort int
	udpPort int
	dnsPort int

	// the statuses the change listener reported
	statuses chan *ExtenderProvideStatus

	stateLock  sync.Mutex
	wakeSerial int
}

// Builds the space, the device and the operator, and turns provide on, which
// is what starts the role (G2). configure runs on the role's settings before
// it is built.
func newTestProvideExtenderFixture(
	t *testing.T,
	configure func(settings *deviceLocalExtenderSettings),
) *testProvideExtenderFixture {
	t.Helper()
	return newTestProvideExtenderFixtureWithDevice(t, nil, configure)
}

// The same fixture with the device's own settings adjusted first, which is how
// a test reaches the provider egress the relay dials through (G2).
func newTestProvideExtenderFixtureWithDevice(
	t *testing.T,
	configureDevice func(settings *DeviceLocalSettings),
	configure func(settings *deviceLocalExtenderSettings),
) *testProvideExtenderFixture {
	t.Helper()
	return newTestProvideExtenderFixtureWithSpace(t, nil, configureDevice, configure)
}

// The same fixture over a space `newSpace` built, which is how a test runs the
// role on a url-only space (F1). Nil builds the manager-backed space these
// tests otherwise use, which is what an app has.
func newTestProvideExtenderFixtureWithSpace(
	t *testing.T,
	newSpace func(ctx context.Context) *NetworkSpace,
	configureDevice func(settings *DeviceLocalSettings),
	configure func(settings *deviceLocalExtenderSettings),
) *testProvideExtenderFixture {
	t.Helper()
	testEnableExtenderNode(t)
	testEnableExtenderProvideRole(t)

	storagePath, err := os.MkdirTemp("", "test_provide_extender")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	_, rootPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clock := newTestExtenderClock()
	operator := newTestProvideExtenderOperator(t, clock, rootPrivateKey)

	fixture := &testProvideExtenderFixture{
		t:              t,
		clock:          clock,
		rootPrivateKey: rootPrivateKey,
		operator:       operator,
		storagePath:    storagePath,
		tcpPort:        testFreeTcpPort(t),
		udpPort:        testFreeUdpPort(t),
		dnsPort:        testFreeUdpPort(t),
		statuses:       make(chan *ExtenderProvideStatus, 256),
	}

	rootPublicKey := rootPrivateKey.Public().(ed25519.PublicKey)
	if newSpace != nil {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		fixture.networkSpace = newSpace(ctx)
		t.Cleanup(fixture.networkSpace.Close)
		// a url-only space takes its anchor from the bundled table, which
		// names no synthetic host, so the fixture root key is installed the
		// way a hello answer installs one (B4)
		keySet, err := connect.NewExtenderRootKeySetFromHex(hex.EncodeToString(rootPublicKey))
		if err != nil {
			t.Fatal(err)
		}
		fixture.networkSpace.extenderDirectory.SetRootKeys(keySet)
	} else {
		fixture.networkSpaceManager = NewNetworkSpaceManager(storagePath)
		fixture.networkSpace = fixture.networkSpaceManager.updateNetworkSpace(
			NewNetworkSpaceKey(testProvideExtenderHost, "main"),
			func(values *NetworkSpaceValues) {
				values.ExtenderRootPublicKeys = []string{hex.EncodeToString(rootPublicKey)}
			},
		)
		t.Cleanup(fixture.networkSpaceManager.Close)
	}

	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = true
	settings.Verbose = false
	settings.DisableLogging = true
	settings.providerExtenderSettings = func(extenderSettings *deviceLocalExtenderSettings) {
		fixture.configureExtender(extenderSettings)
		if configure != nil {
			configure(extenderSettings)
		}
	}
	if configureDevice != nil {
		configureDevice(settings)
	}
	device, err := newDeviceLocalWithOverrides(
		fixture.networkSpace, "", "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	fixture.device = device
	t.Cleanup(func() {
		_ = device.CloseAndWait(context.Background())
	})

	device.AddExtenderProvideStatusChangeListener(
		&testProvideExtenderStatusListener{fixture: fixture})

	// providing is what the role follows (G2)
	device.SetProvideMode(ProvideModePublic)
	return fixture
}

// The role's settings: ephemeral loopback carriers, the fixture operator, and
// the fake clock every cadence decision reads.
func (self *testProvideExtenderFixture) configureExtender(settings *deviceLocalExtenderSettings) {
	settings.TcpPort = self.tcpPort
	settings.UdpPort = self.udpPort
	settings.DnsPort = self.dnsPort
	// the ephemeral carrier is the only dns port here, on every host: a linux
	// build would otherwise also try 53 through the loopback listen seam (L2)
	settings.DnsPrivilegedPort = false
	settings.DnsTld = testProvideExtenderDnsTld
	settings.Listen = func(network string, address string) (net.Listener, error) {
		return net.Listen(network, testLoopbackAddress(address))
	}
	settings.ListenPacket = func(network string, address string) (net.PacketConn, error) {
		return net.ListenPacket(network, testLoopbackAddress(address))
	}
	// the bundled spoof list is empty until operations provide it (A10); a
	// synthetic name keeps the whitelist shape explicit here
	settings.SpoofDomains = []string{"spoof.example"}
	settings.ApiUrlV4 = self.operator.v4Url
	settings.ApiUrlV6 = self.operator.v6Url
	settings.ApiUrl = ""
	settings.HelloUrl = self.operator.helloUrl
	// one dialer, so one request reaches the operator per call: the resilient
	// variants race the same request and would count a pass more than once
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.EnableResilient = false
	settings.ClientStrategySettings = strategySettings
	// every stepped pass advances the clock by at least this, so the caller
	// address check -- the pass barrier -- runs on every step
	settings.AddressCheckTimeout = 1 * time.Minute
	settings.RequestTimeout = 20 * time.Second
	settings.Now = self.clock.Now
	settings.IpVersionSupported = func(ipVersion int) bool { return true }
}

// The loopback form of a carrier bind address, so a test never binds a
// wildcard port on the machine it runs on.
func testLoopbackAddress(address string) string {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// One free tcp port on loopback. The kernel cycles ephemeral assignments, so
// successive calls return distinct ports.
func testFreeTcpPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func testFreeUdpPort(t *testing.T) int {
	t.Helper()
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()
	return packetConn.LocalAddr().(*net.UDPAddr).Port
}

type testProvideExtenderStatusListener struct {
	fixture *testProvideExtenderFixture
}

func (self *testProvideExtenderStatusListener) ExtenderProvideStatusChanged(
	status *ExtenderProvideStatus,
) {
	select {
	case self.fixture.statuses <- status:
	default:
	}
}

// Wakes the activation loop with a directory change, which is a wake it
// already selects on. Each call adds a fresh unverified address so the change
// is real.
func (self *testProvideExtenderFixture) wake() {
	self.stateLock.Lock()
	self.wakeSerial += 1
	serial := self.wakeSerial
	self.stateLock.Unlock()
	self.networkSpace.extenderDirectory.AddBootstrap(
		netip.MustParseAddr(fmt.Sprintf("203.0.113.%d", serial)), connect.ExtenderSourceDns)
}

// Waits for the loop to begin one pass, which its caller address check marks.
// Because the loop is single threaded, this also proves every earlier pass
// finished.
func (self *testProvideExtenderFixture) waitPass() {
	self.t.Helper()
	select {
	case <-self.operator.hellos:
	case <-time.After(60 * time.Second):
		self.t.Fatal("the activation loop did not run a pass")
	}
}

// Advances the fake clock, wakes the loop and waits for the pass it starts.
func (self *testProvideExtenderFixture) step(d time.Duration) {
	self.t.Helper()
	self.clock.advance(d)
	self.wake()
	self.waitPass()
}

func (self *testProvideExtenderFixture) waitPost() *testProvideExtenderPost {
	self.t.Helper()
	select {
	case post := <-self.operator.posts:
		return post
	case <-time.After(60 * time.Second):
		self.t.Fatal("the activation loop did not post an activation")
		return nil
	}
}

// Waits for a status the predicate accepts, reported through the change
// listener (F3).
func (self *testProvideExtenderFixture) waitStatus(
	name string,
	accept func(status *ExtenderProvideStatus) bool,
) *ExtenderProvideStatus {
	self.t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case status := <-self.statuses:
			if accept(status) {
				return status
			}
		case <-deadline:
			self.t.Fatalf(
				"the status never reported %s, last = %+v",
				name, self.device.GetExtenderProvideStatus())
			return nil
		}
	}
}

// Waits for the space's extender node to advertise exactly these mesh
// addresses. The node is rebuilt on every change of the activated set, so this
// is the barrier for both the first activation and a moved address (G2, D2).
func (self *testProvideExtenderFixture) waitNodeListenAddrs(expectedAddrs []string) {
	self.t.Helper()
	slices.Sort(expectedAddrs)
	deadline := time.After(60 * time.Second)
	for {
		// every swap notifies this monitor, so the subscribe belongs before
		// the read: a rebuild in between wakes the next wait rather than
		// being lost
		update := self.networkSpace.extenderNodeMonitor.NotifyChannel()
		extenderNode := self.networkSpace.getExtenderNode()
		listenAddrs := []string{}
		if extenderNode.role() == gossip.NodeRoleExtender {
			for _, listenAddr := range extenderNode.node.ListenAddrs() {
				listenAddrs = append(listenAddrs, listenAddr.String())
			}
			slices.Sort(listenAddrs)
			if slices.Equal(listenAddrs, expectedAddrs) {
				return
			}
		}
		select {
		case <-update:
		case <-deadline:
			self.t.Fatalf(
				"node role = %q with addrs %v, expected an extender node at %v",
				extenderNode.role(), listenAddrs, expectedAddrs)
		}
	}
}

// The role of this device, nil when it runs none.
func (self *testProvideExtenderFixture) extender() *deviceLocalExtender {
	self.device.stateLock.Lock()
	provider := self.device.provider
	self.device.stateLock.Unlock()
	if provider == nil {
		return nil
	}
	provider.stateLock.Lock()
	defer provider.stateLock.Unlock()
	return provider.extender
}

// The own directory entry of this extender's key, nil when it has none.
func (self *testProvideExtenderFixture) ownEntry(ip string) *connect.ExtenderDirectoryEntry {
	publicKey := self.extender().publicKey
	for _, entry := range self.networkSpace.extenderDirectory.Snapshot().Entries {
		if entry.Ip.String() == ip && slices.Equal(entry.PublicKey, publicKey) {
			return entry
		}
	}
	return nil
}

// A providing desktop binds all three carriers, activates over v4 and over v6,
// appears in its own directory, becomes a listening extender node, and reports
// every step through the status listener (G2, G3, F3).
func TestDeviceLocalProviderExtenderActivatesEveryFamily(t *testing.T) {
	fixture := newTestProvideExtenderFixture(t, nil)

	fixture.waitPass()
	posts := map[int]*testProvideExtenderPost{}
	for range 2 {
		post := fixture.waitPost()
		posts[post.ipVersion] = post
	}
	if len(posts) != 2 {
		t.Fatalf("posts = %v, expected one per family", posts)
	}

	expectedCarriers := []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	}
	for ipVersion, post := range posts {
		if !slices.Equal(post.args.Carriers, expectedCarriers) {
			t.Fatalf("v%d carriers = %v, expected %v", ipVersion, post.args.Carriers, expectedCarriers)
		}
		if post.args.TcpPort != fixture.tcpPort ||
			post.args.UdpPort != fixture.udpPort ||
			post.args.DnsPort != fixture.dnsPort {
			t.Fatalf("v%d ports = %d/%d/%d, expected the bound carriers",
				ipVersion, post.args.TcpPort, post.args.UdpPort, post.args.DnsPort)
		}
		if post.args.DnsTld != testProvideExtenderDnsTld {
			t.Fatalf("v%d dns tld = %q", ipVersion, post.args.DnsTld)
		}
		// the dns ports that actually bound, which the operator probes one by
		// one (L2). Only the unprivileged carrier is bound here
		if !slices.Equal(post.args.DnsPorts, []int{fixture.dnsPort}) {
			t.Fatalf("v%d dns ports = %v, expected the bound carrier %d",
				ipVersion, post.args.DnsPorts, fixture.dnsPort)
		}
	}

	status := fixture.waitStatus("both families activated", func(status *ExtenderProvideStatus) bool {
		return status.ActivatedV4 && status.ActivatedV6
	})
	if !status.Enabled || !status.Listening {
		t.Fatalf("status = %+v, expected an enabled listening extender", status)
	}
	if status.ListenError != "" {
		t.Fatalf("listen error = %q, expected every carrier to bind", status.ListenError)
	}
	if status.Ipv4 != testProvideExtenderIpv4 || status.Ipv6 != testProvideExtenderIpv6 {
		t.Fatalf("status addresses = %s / %s", status.Ipv4, status.Ipv6)
	}
	if status.LastActivationError != "" || status.LastActivationTime == 0 {
		t.Fatalf("status = %+v, expected a clean activation", status)
	}
	if status.RevokedTime != 0 {
		t.Fatalf("revoked time = %d, expected none", status.RevokedTime)
	}
	if status.DnsPorts != strconv.Itoa(fixture.dnsPort) {
		t.Fatalf("status dns ports = %q, expected the bound carrier %d",
			status.DnsPorts, fixture.dnsPort)
	}
	// and the state an app renders, derived from exactly these fields (N3)
	if !status.Supported {
		t.Fatalf("status = %+v, expected a build that carries the role", status)
	}
	if status.State != ExtenderProvideStateActive || status.ErrorCase != "" || status.Reason != "" {
		t.Fatalf("state = %q, %q, %q, expected active with nothing to say",
			status.State, status.ErrorCase, status.Reason)
	}

	// the operator's record for this extender's own key is in the directory,
	// with the carriers the activation proved (C2, E1)
	for _, ip := range []string{testProvideExtenderIpv4, testProvideExtenderIpv6} {
		entry := fixture.ownEntry(ip)
		if entry == nil {
			t.Fatalf("%s is not in the directory under the extender key", ip)
		}
		if entry.State != connect.ExtenderStateActive {
			t.Fatalf("%s state = %q, expected active", ip, entry.State)
		}
		if !slices.Equal(entry.Carriers, expectedCarriers) {
			t.Fatalf("%s carriers = %v, expected %v", ip, entry.Carriers, expectedCarriers)
		}
	}

	// the space's node is now a listening extender node, advertising one mesh
	// address per activated family (D2, G2)
	fixture.waitNodeListenAddrs([]string{
		fmt.Sprintf("/ip4/%s/tcp/%d", testProvideExtenderIpv4, fixture.tcpPort),
		fmt.Sprintf("/ip6/%s/tcp/%d", testProvideExtenderIpv6, fixture.tcpPort),
	})
}

// A revocation of this extender's own key, observed in the directory,
// re-activates at once and is reported in the status (G3, B5, F3).
func TestDeviceLocalProviderExtenderReactivatesOnRevocation(t *testing.T) {
	fixture := newTestProvideExtenderFixture(t, nil)
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitPost()
	fixture.waitStatus("activated", func(status *ExtenderProvideStatus) bool {
		return status.ActivatedV4 && status.ActivatedV6
	})

	// the operator revoked this key; the record it signs next is newer, so the
	// addresses return to active
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

	// the re-activation is immediate: it does not wait out the 24 hour tick,
	// and the record it issues is newer than the revocation, so the address
	// returns to active (B5)
	fixture.waitPost()
	fixture.waitStatus("re-activated", func(status *ExtenderProvideStatus) bool {
		return status.RevokedTime == 0 && status.ActivatedV4
	})
	if entry := fixture.ownEntry(testProvideExtenderIpv4); entry == nil ||
		entry.State != connect.ExtenderStateActive {
		t.Fatalf("the re-activated address is %+v, expected active", entry)
	}
}

// A revocation that stands, because the operator refuses the re-activation, is
// what the status reports as revoked (F3, G3).
func TestDeviceLocalProviderExtenderReportsARevokedKey(t *testing.T) {
	fixture := newTestProvideExtenderFixture(t, nil)
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitPost()
	fixture.waitStatus("activated", func(status *ExtenderProvideStatus) bool {
		return status.ActivatedV4 && status.ActivatedV6
	})

	fixture.operator.setRefusal("the tcp carrier did not answer")
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

	status := fixture.waitStatus("revoked", func(status *ExtenderProvideStatus) bool {
		return status.RevokedTime != 0
	})
	if status.ActivatedV4 || status.ActivatedV6 {
		t.Fatalf("status = %+v, expected a revoked extender to be inactive", status)
	}
	if !strings.Contains(status.LastActivationError, "the tcp carrier did not answer") {
		t.Fatalf("last activation error = %q, expected the operator's refusal", status.LastActivationError)
	}
	// a revocation is the state whatever else the role reports, and the case
	// is the whole message (N3)
	if status.State != ExtenderProvideStateError ||
		status.ErrorCase != ExtenderProvideErrorRevoked ||
		status.Reason != "" {
		t.Fatalf("state = %q, %q, %q, expected the revoked error",
			status.State, status.ErrorCase, status.Reason)
	}

	// the operator accepts again on the next attempt, which the backoff holds
	// for ten minutes (G3)
	fixture.operator.setRefusal("")
	fixture.clock.advance(11 * time.Minute)
	fixture.wake()
	fixture.waitPost()
	fixture.waitStatus("recovered", func(status *ExtenderProvideStatus) bool {
		return status.RevokedTime == 0 && status.ActivatedV4
	})
}

// A caller address that no longer matches what the operator published
// re-activates on the address check (G3).
func TestDeviceLocalProviderExtenderReactivatesOnAddressChange(t *testing.T) {
	fixture := newTestProvideExtenderFixture(t, nil)
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitPost()

	// the same address changes nothing, and neither does a new source port:
	// every check arrives on its own connection
	fixture.step(2 * time.Minute)
	fixture.operator.setClientAddress(testProvideExtenderIpv4 + ":41002")
	fixture.step(2 * time.Minute)
	fixture.step(2 * time.Minute)
	if count := fixture.operator.postCount(4); count != 1 {
		t.Fatalf("posts with an unchanged address = %d, expected one", count)
	}

	// this host moved, so the operator sees a new caller address and publishes
	// the extender at it
	movedIpv4 := "198.51.100.12"
	fixture.operator.setPublishedIp(4, movedIpv4)
	fixture.operator.setClientAddress(movedIpv4 + ":41003")
	fixture.clock.advance(2 * time.Minute)
	fixture.wake()
	fixture.waitPost()
	if count := fixture.operator.postCount(4); count < 2 {
		t.Fatalf("posts after the address changed = %d, expected a re-activation", count)
	}
	fixture.waitStatus("the moved address", func(status *ExtenderProvideStatus) bool {
		return status.Ipv4 == movedIpv4
	})

	// the node is rebuilt on the new address, and the address this host no
	// longer has is gone rather than still advertised (G2, D2)
	expectedAddrs := []string{
		fmt.Sprintf("/ip4/%s/tcp/%d", movedIpv4, fixture.tcpPort),
		fmt.Sprintf("/ip6/%s/tcp/%d", testProvideExtenderIpv6, fixture.tcpPort),
	}
	fixture.waitNodeListenAddrs(expectedAddrs)
}

// The relay's forward dial is the device's own egress, so a relayed connection
// never enters the device's tunnel, and the extender narrows it by the
// client's family itself (A7, G2).
func TestDeviceLocalProviderExtenderRelaysThroughTheDeviceEgress(t *testing.T) {
	dials := make(chan string, 8)
	fixture := newTestProvideExtenderFixtureWithDevice(
		t,
		func(settings *DeviceLocalSettings) {
			settings.ProviderDialContextSettings = &connect.DialContextSettings{
				DialContext: func(
					ctx context.Context,
					network string,
					address string,
				) (net.Conn, error) {
					select {
					case dials <- network + " " + address:
					default:
					}
					return nil, fmt.Errorf("the device egress is a test seam")
				},
			}
		},
		nil,
	)
	fixture.waitPass()

	dialContext := fixture.extender().settings.DialContext
	if dialContext == nil {
		t.Fatal("the role has no forward dial")
	}
	conn, err := dialContext(context.Background(), "tcp4", "dest.example:443")
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("the forward dial did not reach the device egress")
	}
	select {
	case dial := <-dials:
		if dial != "tcp4 dest.example:443" {
			t.Fatalf("forward dial = %q, expected the family narrowed destination", dial)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the forward dial did not reach the device egress")
	}

	// the whitelist is the space's operator patterns (A5)
	expectedHosts := []string{testProvideExtenderHost, "*." + testProvideExtenderHost}
	if allowedHosts := fixture.extender().settings.AllowedHosts; !slices.Equal(allowedHosts, expectedHosts) {
		t.Fatalf("allowed hosts = %v, expected %v", allowedHosts, expectedHosts)
	}
}

// The opt-out stops the extender server, the activation loop and the extender
// node together, and turning it back on starts them again (F3, G2).
func TestDeviceLocalProviderExtenderOptOut(t *testing.T) {
	fixture := newTestProvideExtenderFixture(t, nil)
	fixture.waitPass()
	fixture.waitPost()
	fixture.waitPost()
	fixture.waitStatus("activated", func(status *ExtenderProvideStatus) bool {
		return status.ActivatedV4 && status.ActivatedV6
	})
	if !fixture.device.GetProvideExtender() {
		t.Fatal("the setting is off by default, expected on")
	}

	fixture.device.SetProvideExtender(false)
	if fixture.extender() != nil {
		t.Fatal("the opt-out left the role running")
	}
	if status := fixture.device.GetExtenderProvideStatus(); status.Enabled || status.Listening {
		t.Fatalf("status after the opt-out = %+v, expected disabled", status)
	}
	// the carriers are released, so the ports bind again
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", fixture.tcpPort)))
	if err != nil {
		t.Fatalf("the opt-out did not release the tcp carrier: %v", err)
	}
	listener.Close()
	// the member node returns (D5, G2)
	if role := fixture.networkSpace.getExtenderNode().role(); role != gossip.NodeRoleMember {
		t.Fatalf("node role after the opt-out = %q, expected member", role)
	}

	fixture.device.SetProvideExtender(true)
	if fixture.extender() == nil {
		t.Fatal("turning the setting back on did not start the role")
	}
	fixture.waitPost()
	fixture.waitStatus("re-activated", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening && status.ActivatedV4
	})

	// turning provide off stops the role the same way (G2)
	fixture.device.SetProvideMode(ProvideModeNone)
	if fixture.extender() != nil {
		t.Fatal("the role outlived providing")
	}
	if role := fixture.networkSpace.getExtenderNode().role(); role != gossip.NodeRoleMember {
		t.Fatalf("node role after provide off = %q, expected member", role)
	}
}

// A carrier whose port is taken disables only that carrier: the others are
// activated, and the status names the one that failed (G2, F3).
func TestDeviceLocalProviderExtenderSkipsAFailedCarrier(t *testing.T) {
	occupiedPort := testFreeTcpPort(t)
	occupied, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", occupiedPort)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { occupied.Close() })

	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		settings.TcpPort = occupiedPort
	})

	fixture.waitPass()
	post := fixture.waitPost()
	expectedCarriers := []string{connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns}
	if !slices.Equal(post.args.Carriers, expectedCarriers) {
		t.Fatalf("carriers = %v, expected the carriers that bound", post.args.Carriers)
	}

	status := fixture.waitStatus("the failed carrier", func(status *ExtenderProvideStatus) bool {
		return status.ListenError != "" && status.ActivatedV4
	})
	if !status.Listening {
		t.Fatalf("status = %+v, expected the remaining carriers to be listening", status)
	}
	if !strings.HasPrefix(status.ListenError, connect.ExtenderCarrierTcp+": ") {
		t.Fatalf("listen error = %q, expected the tcp carrier", status.ListenError)
	}
	// one carrier down while another is activated is still active (N3)
	if status.State != ExtenderProvideStateActive {
		t.Fatalf("state = %q, expected active", status.State)
	}

	// only the tcp carrier carries the mesh, so nothing is advertised (D2)
	if extenderNode := fixture.networkSpace.getExtenderNode(); extenderNode != nil {
		if listenAddrs := extenderNode.node.ListenAddrs(); 0 < len(listenAddrs) {
			t.Fatalf("mesh addresses = %v, expected none without the tcp carrier", listenAddrs)
		}
	}
}

// An operator with no family api hosts is activated through the plain api url,
// once per cycle, and the family comes from the answer (C2, G3).
func TestDeviceLocalProviderExtenderUsesThePlainApiUrl(t *testing.T) {
	var operatorV6Url string
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		operatorV6Url = settings.ApiUrlV6
		settings.ApiUrl = settings.ApiUrlV6
		settings.ApiUrlV4 = ""
		settings.ApiUrlV6 = ""
	})

	fixture.waitPass()
	if post := fixture.waitPost(); post.ipVersion != 6 {
		t.Fatalf("posted to v%d, expected the plain api url %s", post.ipVersion, operatorV6Url)
	}
	status := fixture.waitStatus("the reported family", func(status *ExtenderProvideStatus) bool {
		return status.ActivatedV6
	})
	if status.ActivatedV4 || status.Ipv4 != "" {
		t.Fatalf("status = %+v, expected only the family the answer named", status)
	}
	if status.Ipv6 != testProvideExtenderIpv6 {
		t.Fatalf("v6 address = %s", status.Ipv6)
	}

	// one post per cycle: there is no second url to reach the other family with
	fixture.step(1 * time.Minute)
	if count := fixture.operator.postCount(6); count != 1 {
		t.Fatalf("posts = %d, expected one per cycle", count)
	}
	if count := fixture.operator.postCount(4); count != 0 {
		t.Fatalf("posts to the v4 host = %d, expected none", count)
	}
}

// The extender identity is the space's `.extender_key`, so the member node and
// the role are the same peer (B1, G2).
func TestDeviceLocalProviderExtenderReusesTheMemberNodeKey(t *testing.T) {
	fixture := newTestProvideExtenderFixture(t, nil)
	fixture.waitPass()
	fixture.waitPost()

	keySeed, err := fixture.networkSpace.asyncLocalState.GetLocalState().GetOrCreateExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(keySeed)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fixture.extender().publicKey, publicKey) {
		t.Fatal("the role did not take the stored identity key")
	}
	if !slices.Equal(fixture.extender().server.PublicKey(), publicKey) {
		t.Fatal("the extender server did not take the stored identity key")
	}

	// the node the role installed carries the same identity, so the peer id
	// the mesh knows this host by does not change when it becomes an extender
	extenderPeerId := fixture.networkSpace.getExtenderNode().node.PeerId()
	fixture.device.SetProvideExtender(false)
	memberPeerId := fixture.networkSpace.getExtenderNode().node.PeerId()
	if extenderPeerId != memberPeerId {
		t.Fatalf("peer id = %s as an extender and %s as a member", extenderPeerId, memberPeerId)
	}
}

// The setting is persisted per space and survives a reload of local state
// (F3).
func TestProvideExtenderSettingPersists(t *testing.T) {
	storagePath, err := os.MkdirTemp("", "test_provide_extender_setting")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(storagePath) })

	localState := newLocalState(context.Background(), storagePath)
	if !localState.GetProvideExtender() {
		t.Fatal("the default is off, expected on")
	}
	if err := localState.SetProvideExtender(false); err != nil {
		t.Fatal(err)
	}
	if localState.GetProvideExtender() {
		t.Fatal("the setting did not take")
	}
	if reloaded := newLocalState(context.Background(), storagePath); reloaded.GetProvideExtender() {
		t.Fatal("the setting did not survive a reload")
	}
	if err := localState.SetProvideExtender(true); err != nil {
		t.Fatal(err)
	}
	if reloaded := newLocalState(context.Background(), storagePath); !reloaded.GetProvideExtender() {
		t.Fatal("the setting did not survive a reload")
	}
}

// This build carries the role; ios, android and js take the stub whose status
// is disabled (G1). The tag-only builds are checked by the build itself.
func TestExtenderProvideSupportedOnThisBuild(t *testing.T) {
	if !extenderProvideSupported {
		t.Fatal("the desktop build does not carry the provider extender role")
	}
	status := disabledExtenderProvideStatus()
	if status.Enabled || status.Listening || status.ListenError != "" ||
		status.ActivatedV4 || status.ActivatedV6 ||
		status.Ipv4 != "" || status.Ipv6 != "" ||
		status.LastActivationTime != 0 || status.LastActivationError != "" ||
		status.RevokedTime != 0 || status.ConnectionCount != 0 {
		t.Fatalf("the disabled status is %+v, expected every field zero", status)
	}
}

// A url-only space runs the role exactly as a stored one does (F1, G2): the
// space is built from an api url alone, with no local state to keep an
// identity in, so the identity comes from the key material the embedder passed
// in and the activation runs under it.
func TestDeviceLocalProviderExtenderActivatesOnAUrlOnlySpace(t *testing.T) {
	keySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(keySeed)
	if err != nil {
		t.Fatal(err)
	}
	keyMaterial := NewDeviceLocalKeyMaterial(nil, nil, nil)
	keyMaterial.SetExtenderKeySeed(keySeed)

	fixture := newTestProvideExtenderFixtureWithSpace(
		t,
		newTestProvideExtenderUrlSpace,
		func(settings *DeviceLocalSettings) {
			settings.KeyMaterial = keyMaterial
		},
		nil,
	)
	if fixture.networkSpace.asyncLocalState != nil {
		t.Fatal("the url-only space kept local state")
	}

	fixture.waitPass()
	post := fixture.waitPost()
	if post.args.PublicKeyHex != hex.EncodeToString(publicKey) {
		t.Fatalf("the activation named %q, expected the key material identity", post.args.PublicKeyHex)
	}
	status := fixture.waitStatus("a family activated", func(status *ExtenderProvideStatus) bool {
		return status.ActivatedV4 || status.ActivatedV6
	})
	if !status.Enabled || !status.Listening {
		t.Fatalf("status = %+v, expected an enabled listening extender", status)
	}
	if !slices.Equal(fixture.extender().publicKey, publicKey) {
		t.Fatal("the role did not take the key material identity")
	}
	// the embedder reads the same seed back, so saving it after start keeps
	// this identity across restarts (B1)
	if !slices.Equal(fixture.device.GetKeyMaterial().GetExtenderKeySeed(), keySeed) {
		t.Fatal("the key material did not carry the identity back")
	}
}

// With no seed anywhere the role generates one, and the embedder reads it back
// through the key material, which is the only place a url-only space can keep
// it (B1, G2).
func TestDeviceLocalProviderExtenderExposesAGeneratedKeySeed(t *testing.T) {
	fixture := newTestProvideExtenderFixtureWithSpace(t, newTestProvideExtenderUrlSpace, nil, nil)

	fixture.waitPass()
	post := fixture.waitPost()

	keySeed := fixture.device.GetKeyMaterial().GetExtenderKeySeed()
	if len(keySeed) == 0 {
		t.Fatal("the key material carried no generated identity")
	}
	publicKey, err := connect.ExtenderPublicKeyFromSeed(keySeed)
	if err != nil {
		t.Fatal(err)
	}
	if post.args.PublicKeyHex != hex.EncodeToString(publicKey) {
		t.Fatalf("the activation named %q, expected the generated identity", post.args.PublicKeyHex)
	}
	if !slices.Equal(fixture.extender().publicKey, publicKey) {
		t.Fatal("the role ran under an identity the key material does not carry")
	}
	// the same seed every time it is read, so an embedder that saves it after
	// start saves the identity that activated
	if !slices.Equal(fixture.device.GetKeyMaterial().GetExtenderKeySeed(), keySeed) {
		t.Fatal("the key material carried a second identity")
	}
}

// An embedder that runs many providers in one process turns the role off, and
// nothing then binds a carrier or activates (G1).
func TestDeviceLocalProviderExtenderDisabledBySettings(t *testing.T) {
	fixture := newTestProvideExtenderFixtureWithSpace(
		t,
		newTestProvideExtenderUrlSpace,
		func(settings *DeviceLocalSettings) {
			settings.ProvideExtenderEnabled = false
		},
		nil,
	)

	// providing is on, which is what the role follows when it is allowed to
	deadline := time.Now().Add(60 * time.Second)
	for !fixture.device.GetProvideEnabled() {
		if deadline.Before(time.Now()) {
			t.Fatal("the device never started providing")
		}
		time.Sleep(time.Millisecond)
	}
	// the user's own setting is untouched: only the embedder's switch is off
	if !fixture.device.GetProvideExtender() {
		t.Fatal("the persisted setting was turned off, expected only the embedder switch")
	}
	if fixture.extender() != nil {
		t.Fatal("the role ran with the embedder switch off")
	}
	if status := fixture.device.GetExtenderProvideStatus(); status.Enabled || status.Listening {
		t.Fatalf("status = %+v, expected a disabled role", status)
	}
	// nothing reaches the operator, which is the observable half of the role
	// not running
	select {
	case post := <-fixture.operator.posts:
		t.Fatalf("the role activated v%d with the embedder switch off", post.ipVersion)
	case <-fixture.operator.hellos:
		t.Fatal("the role ran an activation pass with the embedder switch off")
	case <-time.After(5 * time.Second):
	}
}

// A hosted device never runs the role: its space is shared across unrelated
// customers and an extender published for it would name the proxy host (G1).
func TestDeviceLocalProviderExtenderDisabledWhenHosted(t *testing.T) {
	fixture := newTestProvideExtenderFixtureWithSpace(
		t,
		newTestProvideExtenderUrlSpace,
		func(settings *DeviceLocalSettings) {
			settings.HostedIncompatible = true
		},
		nil,
	)
	if fixture.extender() != nil {
		t.Fatal("a hosted device ran the provider extender role")
	}
	if status := fixture.device.GetExtenderProvideStatus(); status.Enabled {
		t.Fatalf("status = %+v, expected a disabled role", status)
	}
	select {
	case post := <-fixture.operator.posts:
		t.Fatalf("a hosted device activated v%d", post.ipVersion)
	case <-fixture.operator.hellos:
		t.Fatal("a hosted device ran an activation pass")
	case <-time.After(5 * time.Second):
	}
}

// The url-only space of these tests: an api url and nothing else, which is
// what a headless embedder builds (F1).
func newTestProvideExtenderUrlSpace(ctx context.Context) *NetworkSpace {
	strategySettings := connect.DefaultClientStrategySettings()
	strategySettings.Log = connect.NewNoopLogger()
	return NewNetworkSpaceWithUrls(
		ctx,
		"https://api."+testProvideExtenderHost,
		"wss://connect."+testProvideExtenderHost,
		strategySettings,
	)
}

// The dns carrier also binds 53 where the platform takes it without privilege
// (L2): the linux daemon runs as root and the windows service as LocalSystem.
// Everything else, macOS included, binds its unprivileged port alone. The rule
// is parameterized so it is pinned on whatever host runs this.
func TestExtenderDnsPrivilegedPortForPlatform(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		if !extenderDnsPrivilegedPortForPlatform(goos) {
			t.Errorf("%s does not take the privileged dns port", goos)
		}
	}
	for _, goos := range []string{"darwin", "ios", "android", "js", "freebsd", "openbsd"} {
		if extenderDnsPrivilegedPortForPlatform(goos) {
			t.Errorf("%s takes the privileged dns port", goos)
		}
	}
	connect.AssertEqual(t,
		extenderDnsPrivilegedPort(), extenderDnsPrivilegedPortForPlatform(runtime.GOOS))
}

// The role's dns carrier is the extender's unprivileged 4053 and the
// privileged bind follows the platform rule (L2). The ports are what an
// activation advertises, so a production default that drifted would publish a
// port no client dials.
func TestDeviceLocalProviderExtenderSettingsDnsPorts(t *testing.T) {
	networkSpace := newNetworkSpace(
		context.Background(),
		*NewNetworkSpaceKey(testProvideExtenderHost, "main"),
		NetworkSpaceValues{},
		"",
	)
	defer networkSpace.close()

	provider := &deviceLocalProvider{networkSpace: networkSpace}
	settings := provider.extenderSettings()
	if settings == nil {
		t.Fatal("the space has no identity to activate under")
	}
	connect.AssertEqual(t, settings.DnsPort, connect.ExtenderDnsPort)
	connect.AssertEqual(t, settings.DnsPort, connect.DefaultWhodisPort)
	connect.AssertEqual(t, settings.DnsPrivilegedPort, extenderDnsPrivilegedPort())
}

// A host that can take 53 activates on both dns ports, and the activation
// advertises them in dial order, 53 first (L2). The privileged bind is served
// by an ephemeral socket here, so nothing on this machine needs privilege.
func TestDeviceLocalProviderExtenderActivatesEveryDnsPort(t *testing.T) {
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		settings.DnsPrivilegedPort = true
		settings.ListenPacket = func(network string, address string) (net.PacketConn, error) {
			if _, port, err := net.SplitHostPort(address); err == nil &&
				port == strconv.Itoa(connect.DefaultDnsPort) {
				// the privileged bind without the privilege: the advertised
				// port is the configured one, not the socket's
				return net.ListenPacket(network, "127.0.0.1:0")
			}
			return net.ListenPacket(network, testLoopbackAddress(address))
		}
	})

	fixture.waitPass()
	expectedDnsPorts := []int{connect.DefaultDnsPort, fixture.dnsPort}
	for range 2 {
		post := fixture.waitPost()
		if !slices.Equal(post.args.DnsPorts, expectedDnsPorts) {
			t.Fatalf("v%d dns ports = %v, expected %v",
				post.ipVersion, post.args.DnsPorts, expectedDnsPorts)
		}
		// the single port stays for an operator that predates the list
		if post.args.DnsPort != fixture.dnsPort {
			t.Fatalf("v%d dns port = %d, expected the configured carrier %d",
				post.ipVersion, post.args.DnsPort, fixture.dnsPort)
		}
	}

	expectedDnsPortsText := fmt.Sprintf("%d,%d", connect.DefaultDnsPort, fixture.dnsPort)
	status := fixture.waitStatus("both dns ports", func(status *ExtenderProvideStatus) bool {
		return status.DnsPorts == expectedDnsPortsText
	})
	if status.ListenError != "" {
		t.Fatalf("listen error = %q, expected every carrier to bind", status.ListenError)
	}
}
