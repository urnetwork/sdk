//go:build !ios && !android && !js

package sdk

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/portmap"
)

// The router port mapping of the provider extender role (connect EXTENDER.md
// S10), at the device's entry points: the setting, its defaults, the role that
// maps the carriers it holds and offers what the router forwards, the skip on a
// public address, and the removal when the role stops or the setting turns off.
// The router is a NAT-PMP server answered in memory through the mapper's
// exchange seam (RFC 6886), so nothing leaves the process.

// A nat-pmp router in memory. It answers a pcp request with its version, as a
// nat-pmp server does (RFC 6886 §3.5), which is how the mapper finds it.
type testExtenderRouter struct {
	stateLock sync.Mutex
	// the external port it gives an internal one, by "tcp/443"; none gives the
	// internal port
	externalPorts map[string]int
	// what it forwards, by "tcp/443"
	mappings map[string]int
	// every map request, as "tcp/443" and its lifetime
	requests []testExtenderRouterRequest
	// the waits the mapper asked for, which a test fires
	waits chan testExtenderRouterWait
}

type testExtenderRouterRequest struct {
	key             string
	lifetimeSeconds uint32
}

type testExtenderRouterWait struct {
	wait time.Duration
	fire chan time.Time
}

var errTestExtenderRouterNoAnswer = errors.New("the test router does not answer")

func newTestExtenderRouter() *testExtenderRouter {
	return &testExtenderRouter{
		externalPorts: map[string]int{},
		mappings:      map[string]int{},
		waits:         make(chan testExtenderRouterWait, 16),
	}
}

func testExtenderRouterKey(protocol string, port int) string {
	return fmt.Sprintf("%s/%d", protocol, port)
}

// Installs the router behind every role's mapper for the test.
func (self *testExtenderRouter) install(t *testing.T) {
	t.Helper()
	configureExtenderPortMapper = func(settings *portmap.PortMapperSettings) {
		settings.Gateway = func() (netip.Addr, netip.Addr, error) {
			return netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.10"), nil
		}
		settings.Exchange = self.exchange
		settings.UpnpSearch = func(ctx context.Context, gateway netip.Addr, localIp netip.Addr) ([]string, error) {
			return nil, nil
		}
		settings.After = self.after
	}
	t.Cleanup(func() {
		configureExtenderPortMapper = nil
	})
}

func (self *testExtenderRouter) after(wait time.Duration) <-chan time.Time {
	fire := make(chan time.Time, 1)
	self.waits <- testExtenderRouterWait{wait: wait, fire: fire}
	return fire
}

func (self *testExtenderRouter) exchange(
	ctx context.Context,
	server netip.AddrPort,
	localIp netip.Addr,
	request []byte,
	accept func(response []byte) bool,
) ([]byte, error) {
	response := self.answer(request)
	if response == nil || !accept(response) {
		return nil, errTestExtenderRouterNoAnswer
	}
	return response, nil
}

func (self *testExtenderRouter) answer(request []byte) []byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(request) < 2 {
		return nil
	}
	switch request[0] {
	case 2:
		// pcp, which this router does not speak
		return []byte{0, 0x80 | request[1], 0, 1, 0, 0, 0, 0}
	case 0:
	default:
		return nil
	}
	switch request[1] {
	case 0:
		// the router's external address, a documentation address
		return []byte{0, 0x80, 0, 0, 0, 0, 0, 0, 203, 0, 113, 7}
	case 1, 2:
		if len(request) < 12 {
			return nil
		}
		protocol := "udp"
		if request[1] == 2 {
			protocol = "tcp"
		}
		internalPort := int(binary.BigEndian.Uint16(request[4:6]))
		lifetimeSeconds := binary.BigEndian.Uint32(request[8:12])
		key := testExtenderRouterKey(protocol, internalPort)
		self.requests = append(self.requests, testExtenderRouterRequest{key: key, lifetimeSeconds: lifetimeSeconds})
		response := make([]byte, 16)
		response[1] = 0x80 | request[1]
		binary.BigEndian.PutUint16(response[8:10], uint16(internalPort))
		if lifetimeSeconds == 0 {
			delete(self.mappings, key)
			return response
		}
		externalPort, ok := self.externalPorts[key]
		if !ok {
			externalPort = internalPort
		}
		self.mappings[key] = externalPort
		binary.BigEndian.PutUint16(response[10:12], uint16(externalPort))
		binary.BigEndian.PutUint32(response[12:16], lifetimeSeconds)
		return response
	default:
		return nil
	}
}

func (self *testExtenderRouter) setExternalPort(protocol string, internalPort int, externalPort int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.externalPorts[testExtenderRouterKey(protocol, internalPort)] = externalPort
}

func (self *testExtenderRouter) held() map[string]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	held := map[string]int{}
	for key, externalPort := range self.mappings {
		held[key] = externalPort
	}
	return held
}

func (self *testExtenderRouter) mapRequests() []testExtenderRouterRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requests)
}

// The next wait the role's mapper asked for.
func (self *testExtenderRouter) nextWait(t *testing.T) testExtenderRouterWait {
	t.Helper()
	select {
	case wait := <-self.waits:
		return wait
	case <-time.After(60 * time.Second):
		t.Fatal("the mapper asked for no wait")
		return testExtenderRouterWait{}
	}
}

// The role's three carriers, as the router names them.
func (self *testProvideExtenderFixture) carrierKeys() []string {
	return []string{
		testExtenderRouterKey("tcp", self.tcpPort),
		testExtenderRouterKey("udp", self.udpPort),
		testExtenderRouterKey("udp", self.dnsPort),
	}
}

// Every request the router saw names one of the role's own carriers.
func (self *testProvideExtenderFixture) assertOwnCarriersOnly(router *testExtenderRouter) {
	self.t.Helper()
	for _, request := range router.mapRequests() {
		if !slices.Contains(self.carrierKeys(), request.key) {
			self.t.Fatalf("the router was asked to map %s, which is not one of the role's carriers %v",
				request.key, self.carrierKeys())
		}
	}
}

// A role behind a router, with the setting on by the device default.
func newTestPortMappingFixture(t *testing.T, router *testExtenderRouter, public bool) *testProvideExtenderFixture {
	t.Helper()
	router.install(t)
	return newTestProvideExtenderFixtureWithDevice(t, func(settings *DeviceLocalSettings) {
		settings.DefaultProvideExtenderPortMapping = true
	}, func(settings *deviceLocalExtenderSettings) {
		settings.PortMappingPublicAddress = func() bool {
			return public
		}
	})
}

// The apps' default is off and the setting reads it until it is set; the sn
// miner turns the default on (sn miner.ProviderDeviceSettings).
func TestDefaultDeviceLocalSettingsLeavePortMappingOff(t *testing.T) {
	if DefaultDeviceLocalSettings().DefaultProvideExtenderPortMapping {
		t.Fatal("the device default maps the router's ports, expected off for every app")
	}
}

// With the default off the role asks the router for nothing and offers the
// bound ports.
func TestDeviceLocalProviderExtenderMapsNothingByDefault(t *testing.T) {
	router := newTestExtenderRouter()
	router.install(t)
	fixture := newTestProvideExtenderFixture(t, nil)
	if fixture.device.GetProvideExtenderPortMapping() {
		t.Fatal("the setting reads on, expected the default off")
	}

	fixture.waitPass()
	for range 2 {
		post := fixture.waitPost()
		if post.args.TcpPort != fixture.tcpPort || post.args.UdpPort != fixture.udpPort || post.args.DnsPort != fixture.dnsPort {
			t.Fatalf("v%d ports = %d/%d/%d, expected the bound carriers", post.ipVersion,
				post.args.TcpPort, post.args.UdpPort, post.args.DnsPort)
		}
	}
	fixture.waitStatus("port mapping off", func(status *ExtenderProvideStatus) bool {
		return status.Listening && status.PortMapping == ExtenderPortMappingOff
	})
	if requests := router.mapRequests(); len(requests) != 0 {
		t.Fatalf("the router was asked %v, expected nothing", requests)
	}
}

// A role behind a router asks it for the carriers it holds, offers the external
// ports the router gives in every post and in its mesh address, and reports
// the mapping.
func TestDeviceLocalProviderExtenderOffersTheRoutersPorts(t *testing.T) {
	router := newTestExtenderRouter()
	fixture := newTestPortMappingFixtureWithPorts(t, router, map[string]int{"tcp": 40443, "udp": 40444, "dns": 40053})

	fixture.waitPass()
	for range 2 {
		post := fixture.waitPost()
		if post.args.TcpPort != 40443 || post.args.UdpPort != 40444 || post.args.DnsPort != 40053 {
			t.Fatalf("v%d ports = %d/%d/%d, expected the router's 40443/40444/40053", post.ipVersion,
				post.args.TcpPort, post.args.UdpPort, post.args.DnsPort)
		}
		if !slices.Equal(post.args.DnsPorts, []int{40053}) {
			t.Fatalf("v%d dns ports = %v, expected the router's", post.ipVersion, post.args.DnsPorts)
		}
	}
	status := fixture.waitStatus("mapped", func(status *ExtenderProvideStatus) bool {
		return status.PortMapping == ExtenderPortMappingMapped
	})
	if status.PortMappingMethod != portmap.MethodNatPmp || status.PortMappingExternalIp != "203.0.113.7" ||
		status.PortMappingTcpPort != 40443 || status.PortMappingUdpPort != 40444 || status.PortMappingDnsPort != 40053 ||
		status.PortMappingError != "" {
		t.Fatalf("status = %+v, expected the router's mapping", status)
	}
	if held := router.held(); len(held) != 3 {
		t.Fatalf("the router forwards %v, expected the three carriers", held)
	}
	fixture.assertOwnCarriersOnly(router)
	// peers dial the mesh on the port the router forwards (D2)
	fixture.waitNodeListenAddrs([]string{
		fmt.Sprintf("/ip4/%s/tcp/%d", testProvideExtenderIpv4, 40443),
		fmt.Sprintf("/ip6/%s/tcp/%d", testProvideExtenderIpv6, 40443),
	})
}

// newTestPortMappingFixture with the router giving each carrier the external
// port named by "tcp", "udp" or "dns".
func newTestPortMappingFixtureWithPorts(
	t *testing.T,
	router *testExtenderRouter,
	externalPorts map[string]int,
) *testProvideExtenderFixture {
	t.Helper()
	router.install(t)
	return newTestProvideExtenderFixtureWithOptions(t, &testProvideExtenderFixtureOptions{
		configureDevice: func(settings *DeviceLocalSettings) {
			settings.DefaultProvideExtenderPortMapping = true
		},
		configure: func(settings *deviceLocalExtenderSettings) {
			settings.PortMappingPublicAddress = func() bool {
				return false
			}
		},
		prepare: func(fixture *testProvideExtenderFixture) {
			router.setExternalPort("tcp", fixture.tcpPort, externalPorts["tcp"])
			router.setExternalPort("udp", fixture.udpPort, externalPorts["udp"])
			router.setExternalPort("udp", fixture.dnsPort, externalPorts["dns"])
		},
	})
}

// A device with a public address has no NAT to map: the router is asked
// nothing, and the posts offer the bound ports.
func TestDeviceLocalProviderExtenderMapsNothingWithAPublicAddress(t *testing.T) {
	router := newTestExtenderRouter()
	fixture := newTestPortMappingFixture(t, router, true)

	fixture.waitPass()
	for range 2 {
		post := fixture.waitPost()
		if post.args.TcpPort != fixture.tcpPort {
			t.Fatalf("v%d tcp port = %d, expected the bound %d", post.ipVersion, post.args.TcpPort, fixture.tcpPort)
		}
	}
	status := fixture.waitStatus("not needed", func(status *ExtenderProvideStatus) bool {
		return status.PortMapping == ExtenderPortMappingNotNeeded
	})
	if status.PortMappingTcpPort != 0 || status.PortMappingMethod != "" {
		t.Fatalf("status = %+v, expected nothing mapped", status)
	}
	if requests := router.mapRequests(); len(requests) != 0 {
		t.Fatalf("the router was asked %v, expected nothing", requests)
	}
}

// Turning the setting off replaces the role with one that maps nothing, which
// removes what the router forwards; turning it on maps again. The setting
// persists in the space's local state.
func TestDeviceLocalProviderExtenderRemovesItsMappingsWhenTheSettingTurnsOff(t *testing.T) {
	router := newTestExtenderRouter()
	fixture := newTestPortMappingFixture(t, router, false)
	fixture.waitPass()
	fixture.waitStatus("mapped", func(status *ExtenderProvideStatus) bool {
		return status.PortMapping == ExtenderPortMappingMapped
	})
	if held := router.held(); len(held) != 3 {
		t.Fatalf("the router forwards %v, expected the three carriers", held)
	}

	fixture.device.SetProvideExtenderPortMapping(false)
	if fixture.device.GetProvideExtenderPortMapping() {
		t.Fatal("the setting stayed on")
	}
	if held := router.held(); len(held) != 0 {
		t.Fatalf("the router still forwards %v after the setting turned off", held)
	}
	if extender := fixture.extender(); extender == nil || extender.portMappingEnabled() {
		t.Fatal("the role was not replaced by one that maps nothing")
	}
	fixture.waitStatus("port mapping off", func(status *ExtenderProvideStatus) bool {
		return status.PortMapping == ExtenderPortMappingOff
	})
	stored, err := os.ReadFile(filepath.Join(
		fixture.networkSpace.asyncLocalState.GetLocalState().localStorageDir,
		provideExtenderPortMappingFileName,
	))
	if err != nil || string(stored) != "false" {
		t.Fatalf("stored setting = %q %v, expected false", stored, err)
	}

	fixture.device.SetProvideExtenderPortMapping(true)
	fixture.waitStatus("mapped again", func(status *ExtenderProvideStatus) bool {
		return status.PortMapping == ExtenderPortMappingMapped
	})
	if held := router.held(); len(held) != 3 {
		t.Fatalf("the router forwards %v, expected the three carriers again", held)
	}
	fixture.assertOwnCarriersOnly(router)
}

// A role that stops removes what the router forwards to it.
func TestDeviceLocalProviderExtenderRemovesItsMappingsWhenItStops(t *testing.T) {
	router := newTestExtenderRouter()
	fixture := newTestPortMappingFixture(t, router, false)
	fixture.waitPass()
	fixture.waitStatus("mapped", func(status *ExtenderProvideStatus) bool {
		return status.PortMapping == ExtenderPortMappingMapped
	})

	fixture.device.SetProvideExtender(false)
	if fixture.extender() != nil {
		t.Fatal("the role kept running")
	}
	if held := router.held(); len(held) != 0 {
		t.Fatalf("the router still forwards %v after the role stopped", held)
	}
	removals := 0
	for _, request := range router.mapRequests() {
		if request.lifetimeSeconds == 0 {
			removals += 1
		}
	}
	if removals != 3 {
		t.Fatalf("removals = %d, expected one per carrier", removals)
	}
}

// A mapping the router moved at a renewal is posted at once, not at the tick,
// so the record names the port the router forwards now.
func TestDeviceLocalProviderExtenderPostsAMovedMapping(t *testing.T) {
	router := newTestExtenderRouter()
	fixture := newTestPortMappingFixtureWithPorts(t, router, map[string]int{"tcp": 40443})
	fixture.waitPass()
	for range 2 {
		if post := fixture.waitPost(); post.args.TcpPort != 40443 {
			t.Fatalf("v%d tcp port = %d, expected 40443", post.ipVersion, post.args.TcpPort)
		}
	}

	// the router lost its table and gives another port at the renewal
	router.setExternalPort("tcp", fixture.tcpPort, 41443)
	wait := router.nextWait(t)
	if wait.wait != 30*time.Minute {
		t.Fatalf("renewal wait = %s, expected half the hour", wait.wait)
	}
	wait.fire <- time.Now()
	for range 2 {
		if post := fixture.waitPost(); post.args.TcpPort != 41443 {
			t.Fatalf("v%d tcp port after the move = %d, expected 41443", post.ipVersion, post.args.TcpPort)
		}
	}
	fixture.waitStatus("moved", func(status *ExtenderProvideStatus) bool {
		return status.PortMappingTcpPort == 41443
	})
}

// The setting persists per space: a stored value reads back after the cache is
// gone, and a corrupt file stores none, so the device default applies.
func TestProvideExtenderPortMappingSettingPersists(t *testing.T) {
	storageDir := t.TempDir()
	localState := &LocalState{localStorageDir: storageDir}
	if _, stored := localState.getStoredProvideExtenderPortMapping(); stored {
		t.Fatal("an empty space stores a setting")
	}
	if err := localState.setProvideExtenderPortMapping(true); err != nil {
		t.Fatal(err)
	}
	reread := &LocalState{localStorageDir: storageDir}
	if portMapping, stored := reread.getStoredProvideExtenderPortMapping(); !stored || !portMapping {
		t.Fatalf("reread = %t %t, expected stored on", portMapping, stored)
	}
	if err := os.WriteFile(filepath.Join(storageDir, provideExtenderPortMappingFileName), []byte("yes"), 0600); err != nil {
		t.Fatal(err)
	}
	corrupt := &LocalState{localStorageDir: storageDir}
	if _, stored := corrupt.getStoredProvideExtenderPortMapping(); stored {
		t.Fatal("a corrupt file stores a setting")
	}
}
