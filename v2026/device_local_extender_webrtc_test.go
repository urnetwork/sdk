//go:build !ios && !android && !js

package sdk

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The peer-to-peer webrtc carrier in the sdk (connect/EXTENDER.md S, S8; G2
// under C2a): the provider extender role, which answers on the provider
// client's manager and runs without tcp 443 when it can signal, on by
// default; and the device's dial side, which signals its webrtc dials through
// the same client and opens the space directory's gate only while it has one.

// A signal sender that delivers nothing: the role under test installs an
// answerer, and no offer ever arrives.
type testWebRtcNoopSignalSender struct{}

func (self *testWebRtcNoopSignalSender) SendSignal(destinationId connect.Id, signal *protocol.Frame, opts ...any) {
	connect.MessagePoolReturn(signal.MessageBytes)
}

// One manager the role can signal through, closed with the test.
func newTestWebRtcSignalingManager(t *testing.T) *connect.WebRtcManager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := connect.DefaultWebRtcSettings()
	settings.Log = connect.NewNoopLogger()
	manager := connect.NewWebRtcManager(ctx, &testWebRtcNoopSignalSender{}, settings)
	t.Cleanup(func() {
		manager.Close()
		cancel()
	})
	return manager
}

// The device setting is on by default (owner, 2026-11-11), in the settings
// every app, the c abi and the sn miner start from.
func TestProvideExtenderWebRtcIsOnByDefault(t *testing.T) {
	if !DefaultDeviceLocalSettings().ProvideExtenderWebRtc {
		t.Fatal("ProvideExtenderWebRtc is off by default")
	}
	if !DefaultLowMemoryDeviceLocalSettings().ProvideExtenderWebRtc {
		t.Fatal("ProvideExtenderWebRtc is off in the low memory defaults")
	}
}

// Root cause: the webrtc carrier was a setting off by default that nothing
// turned on, so no device served it. Observable: with the device's defaults
// the role is built with the carrier on and the provider client's manager to
// signal through, installs its answerer there, offers webrtc beside the socket
// carriers, and reports it served.
func TestDeviceLocalProviderExtenderServesTheWebRtcCarrierByDefault(t *testing.T) {
	roleSettings := make(chan *deviceLocalExtenderSettings, 1)
	fixture := newTestProvideExtenderFixtureWithOptions(t, &testProvideExtenderFixtureOptions{
		webRtc: true,
		configure: func(settings *deviceLocalExtenderSettings) {
			select {
			case roleSettings <- settings:
			default:
			}
		},
	})
	settings := <-roleSettings
	if !settings.WebRtcCarrier || settings.SignalingManager == nil {
		t.Fatalf("the role was built without the webrtc carrier: %+v", settings.WebRtcCarrier)
	}
	providerManager := fixture.device.provider.Client().WebRtcManager()
	if settings.SignalingManager() != providerManager {
		t.Fatal("the role signals through another manager than the provider client's")
	}

	fixture.waitPass()
	expectedCarriers := []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
		connect.ExtenderCarrierWebRtc,
	}
	for range 2 {
		post := fixture.waitPost()
		if !slices.Equal(post.args.Carriers, expectedCarriers) {
			t.Fatalf("v%d carriers = %v, expected %v", post.ipVersion, post.args.Carriers, expectedCarriers)
		}
	}
	status := fixture.waitStatus("running with every carrier", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening && status.ActivatedV4
	})
	if !status.WebRtcCarrier || status.ListenError != "" || status.TcpUnavailableError != "" {
		t.Fatalf("status = %+v, expected every carrier serving", status)
	}
	if !providerManager.HasExtenderCarrierAnswerer() {
		t.Fatal("the role installed no answerer on the provider client's manager")
	}
}

// Root cause: a role off for want of tcp 443 can never be accepted, however
// well its webrtc path works, so a provider behind nat never extends.
// Observable: with the carrier on and a manager to signal through, a role
// whose tcp bind fails runs anyway -- enabled and listening with the tcp
// failure beside the other carriers' rather than tcp_unavailable, the
// activation offers the webrtc carrier without tcp, the answerer is installed
// on the manager, the role does not retry the port on the off role's cadence,
// and the answerer is removed when the role closes.
func TestDeviceLocalProviderExtenderWithTcpTakenRunsOnTheWebRtcCarrier(t *testing.T) {
	tcpPort := testFreeTcpPort(t)
	testHoldTcpPort(t, tcpPort)
	udpBinds := make(chan int, 64)
	waits := make(chan time.Duration, 16)
	manager := newTestWebRtcSignalingManager(t)
	fixture := newTestProvideExtenderFixtureWithOptions(t, &testProvideExtenderFixtureOptions{
		webRtc: true,
		configure: func(settings *deviceLocalExtenderSettings) {
			settings.TcpPort = tcpPort
			settings.WebRtcCarrier = true
			settings.SignalingManager = func() *connect.WebRtcManager { return manager }
			testRecordUdpBinds(settings, udpBinds)
			testTcpRetry(settings, waits, make(chan time.Time))
		},
	})

	fixture.waitPass()
	expectedCarriers := []string{
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
		connect.ExtenderCarrierWebRtc,
	}
	for range 2 {
		post := fixture.waitPost()
		if !slices.Equal(post.args.Carriers, expectedCarriers) {
			t.Fatalf("v%d carriers = %v, expected %v", post.ipVersion, post.args.Carriers, expectedCarriers)
		}
	}
	status := fixture.waitStatus("running on the webrtc carrier", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening && status.ActivatedV4
	})
	if status.TcpUnavailableError != "" || status.ErrorCase == ExtenderProvideErrorTcpUnavailable {
		t.Fatalf("status = %+v, expected the role on without tcp_unavailable", status)
	}
	if !status.WebRtcCarrier {
		t.Fatalf("status = %+v, expected the webrtc carrier served", status)
	}
	if !strings.HasPrefix(status.ListenError, connect.ExtenderCarrierTcp+": ") {
		t.Fatalf("listen error = %q, expected the tcp bind failure named like any carrier's", status.ListenError)
	}
	if status.State != ExtenderProvideStateActive {
		t.Fatalf("state = %q, expected active", status.State)
	}
	udpPorts := testDrainUdpBinds(udpBinds)
	for _, port := range []int{fixture.udpPort, fixture.dnsPort} {
		if !slices.Contains(udpPorts, port) {
			t.Fatalf("udp binds = %v, expected %d beside the webrtc carrier", udpPorts, port)
		}
	}
	if !manager.HasExtenderCarrierAnswerer() {
		t.Fatal("the role installed no answerer on the manager")
	}
	// the off role binds the port again on its cadence; this one does not
	select {
	case wait := <-waits:
		t.Fatalf("the role waited %s to bind tcp again while running on webrtc", wait)
	default:
	}

	extender := fixture.extender()
	if extender == nil {
		t.Fatal("the role is not running")
	}
	extender.Close()
	if manager.HasExtenderCarrierAnswerer() {
		t.Fatal("the closed role left its answerer on the manager")
	}
}

// The carrier needs a manager to signal through: with the setting on and no
// manager, tcp 443 is still the one port the role requires, so a role that
// cannot bind it stays off with tcp_unavailable, exactly as before.
func TestDeviceLocalProviderExtenderWithTcpTakenAndNoSignalingStaysOff(t *testing.T) {
	tcpPort := testFreeTcpPort(t)
	testHoldTcpPort(t, tcpPort)
	udpBinds := make(chan int, 64)
	waits := make(chan time.Duration, 16)
	fixture := newTestProvideExtenderFixtureWithOptions(t, &testProvideExtenderFixtureOptions{
		webRtc: true,
		configure: func(settings *deviceLocalExtenderSettings) {
			settings.TcpPort = tcpPort
			settings.WebRtcCarrier = true
			settings.SignalingManager = func() *connect.WebRtcManager { return nil }
			testRecordUdpBinds(settings, udpBinds)
			testTcpRetry(settings, waits, make(chan time.Time))
		},
	})

	status := fixture.waitStatus("off for want of tcp", func(status *ExtenderProvideStatus) bool {
		return status.ErrorCase == ExtenderProvideErrorTcpUnavailable
	})
	if status.Enabled || status.Listening || status.WebRtcCarrier {
		t.Fatalf("status = %+v, expected the role off", status)
	}
	select {
	case <-waits:
	case <-time.After(10 * time.Second):
		t.Fatal("the role does not try the tcp port again")
	}
	if udpPorts := testDrainUdpBinds(udpBinds); 0 < len(udpPorts) {
		t.Fatalf("the role bound udp %v without tcp 443", udpPorts)
	}
	if extender := fixture.extender(); extender == nil || extender.currentServer() != nil {
		t.Fatal("the role started its server without tcp 443 or a signaling path")
	}
}

// Root cause: a role that closed after its successor installed an answerer of
// its own cleared the successor's, so the device declined every offer. A
// close removes only its own answerer.
func TestDeviceLocalExtenderCloseLeavesItsSuccessorsAnswerer(t *testing.T) {
	manager := newTestWebRtcSignalingManager(t)
	fixture := newTestProvideExtenderFixtureWithOptions(t, &testProvideExtenderFixtureOptions{
		webRtc: true,
		configure: func(settings *deviceLocalExtenderSettings) {
			settings.WebRtcCarrier = true
			settings.SignalingManager = func() *connect.WebRtcManager { return manager }
		},
	})
	fixture.waitPass()
	extender := fixture.extender()
	if extender == nil || !manager.HasExtenderCarrierAnswerer() {
		t.Fatal("the role is not answering")
	}
	successor := newTestOfferAnswererStub()
	manager.SetExtenderCarrierAnswerer(successor)
	extender.Close()
	if !manager.HasExtenderCarrierAnswerer() {
		t.Fatal("the closed role removed its successor's answerer")
	}
}

// An answerer that answers nothing, standing for a successor role's.
type testOfferAnswererStub struct{}

func newTestOfferAnswererStub() *testOfferAnswererStub {
	return &testOfferAnswererStub{}
}

func (self *testOfferAnswererStub) AnswerWebRtcExtenderOffer(
	ctx context.Context,
	offer webrtc.SessionDescription,
) (webrtc.SessionDescription, error) {
	return offer, nil
}

// Root cause: no device dialed the webrtc carrier: nothing installed a
// carrier on a device's connect settings or opened its directory (S8).
// Observable: a space's strategy carries the carrier; once the device's
// provider client exists the space signals through it and the directory lists
// the carrier, and once the device closes, it does not.
func TestDeviceLocalSignalsItsWebRtcExtenderDialsThroughItsProviderClient(t *testing.T) {
	fixture := newTestProvideExtenderFixture(t, nil)
	space := fixture.networkSpace
	if space.clientStrategySettings.ConnectSettings.WebRtcExtenderCarrier == nil {
		t.Fatal("the space's strategy carries no webrtc carrier")
	}
	if space.extenderWebRtcDial == nil {
		t.Fatal("the space has no webrtc dial signaling")
	}
	if !space.extenderWebRtcDial.HasClient() || !space.extenderDirectory.WebRtcCarrierEnabled() {
		t.Fatal("the device's provider client is not what its webrtc dials signal through")
	}
	if err := fixture.device.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if space.extenderWebRtcDial.HasClient() || space.extenderDirectory.WebRtcCarrierEnabled() {
		t.Fatal("a closed device left its client as the webrtc dial signaling")
	}
}
