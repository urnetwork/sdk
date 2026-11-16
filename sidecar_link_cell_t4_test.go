//go:build !ios_extension && !js

package sdk

// §22.9's link cell on T4's ios32 surrogate (BRIDGE-M8-03; D-BRIDGE-5, D-BRIDGE-12, R-BRIDGE-9, §17.5's memory row,
// §18.4's budgets; owner decision 100): what one link costs a VPN component, and what an app host's exit NAT holds with
// public egress flows under the sdk host table, each measured in t4cell's cell under the profile's Go limit and
// reported in its table, since the link's figure is what decides links in the iOS tunnel and no surrogate figure is
// evidence for a device. The cell is not vacuous: a planted link holding twice the forwarding role's steady budget fails
// its check. Through the NAT a public flow's bytes reach their destination exactly as with no hook, and its decision
// holds the NAT's own idle.
//
// A sidecar-role test file, so the cell harness's import passes the sdk's import rule, which keeps t4cell out of the
// device role's files (dependency_boundary_test.go); the plan named it device_sidecar_memory_test.go.
//
// What the cell leaves to the review phase and the surrogate devices, each in BRIDGE-M8-03's report: the link measured
// as a real window client with its transports and sealed session on a surrogate, every LOWBAR phase, bridge flows at
// §17.5's idles through C5's IdleTimeout in the NAT (a principal's flow needs a sealed session no in-process peer forms
// here; device's tests hold the decisions' idles), the forwarding compile sampled beside the link against DESIGN-DETAILS
// 4.4's table, and 4.4's rows fixture rerun, both of which wait on an exported N-device compile fixture.

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	sidecardevice "github.com/urnetwork/sidecar/device"
	"github.com/urnetwork/sidecar/sidecartest/t4cell"
)

// A UDP datagram from 198.51.100.20:sourcePort to destination carrying payload, a fresh slice.
func testingLinkCellDatagram(t *testing.T, destination netip.AddrPort, sourcePort uint16, payload []byte) []byte {
	t.Helper()
	ip := &layers.IPv4{
		Version:  4,
		TTL:      64,
		SrcIP:    net.IP(testingDeviceRoleSource.AsSlice()),
		DstIP:    net.IP(destination.Addr().AsSlice()),
		Protocol: layers.IPProtocolUDP,
	}
	udp := &layers.UDP{SrcPort: layers.UDPPort(sourcePort), DstPort: layers.UDPPort(destination.Port())}
	if err := udp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatal(err)
	}
	buffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, ip, udp, gopacket.Payload(payload)); err != nil {
		t.Fatal(err)
	}
	return slices.Clone(buffer.Bytes())
}

// A loopback UDP listener that keeps every datagram it reads.
type testingLinkCellUpstream struct {
	conn net.PacketConn

	stateLock sync.Mutex
	datagrams [][]byte
	arrived   chan struct{}
}

func newTestingLinkCellUpstream(t *testing.T) *testingLinkCellUpstream {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	self := &testingLinkCellUpstream{conn: conn, arrived: make(chan struct{}, 1024)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buffer := make([]byte, 64*1024)
		for {
			n, _, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			self.stateLock.Lock()
			self.datagrams = append(self.datagrams, bytes.Clone(buffer[:n]))
			self.stateLock.Unlock()
			select {
			case self.arrived <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() {
		conn.Close()
		<-done
	})
	return self
}

func (self *testingLinkCellUpstream) address() netip.AddrPort {
	return self.conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Waits until count datagrams have arrived, then answers them sorted, failing past the watchdog.
func (self *testingLinkCellUpstream) await(t *testing.T, count int) [][]byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testingDeviceRoleWatchdog)
	defer cancel()
	for {
		self.stateLock.Lock()
		datagrams := slices.Clone(self.datagrams)
		self.stateLock.Unlock()
		if count <= len(datagrams) {
			slices.SortFunc(datagrams, bytes.Compare)
			return datagrams
		}
		select {
		case <-self.arrived:
		case <-ctx.Done():
			t.Fatalf("%d of %d datagrams arrived", len(datagrams), count)
		}
	}
}

// Sends flowCount public UDP flows, one datagram each, through a provider NAT holding hook, nil for none, to upstream:
// the NAT, kept open, and the payloads sent.
func testingLinkCellPublicFlows(t *testing.T, ctx context.Context, hook connect.EgressHook, upstream *testingLinkCellUpstream, flowCount int, tag string) (*connect.LocalUserNat, [][]byte) {
	t.Helper()
	natSettings := providerLocalUserNatSettings(0, connect.NewNoopLogger(), nil)
	natSettings.EgressHook = hook
	nat, err := connect.TryNewLocalUserNat(ctx, connect.NewId().String(), natSettings)
	if err != nil {
		t.Fatal(err)
	}
	source := connect.SourceId(connect.NewId())
	payloads := [][]byte{}
	for i := range flowCount {
		payload := []byte(tag + " public flow " + string(rune('A'+i%26)) + string(rune('a'+i/26)))
		payloads = append(payloads, payload)
		if !nat.SendPacket(source, protocol.ProvideMode_Public, testingLinkCellDatagram(t, upstream.address(), uint16(41000+i), payload), time.Second) {
			t.Fatalf("the NAT refused flow %d", i)
		}
	}
	slices.SortFunc(payloads, bytes.Compare)
	return nat, payloads
}

// §22.9's link cell: one link's footprint and an app host's NAT, each a row of the ios32 cell, reported; a planted
// over-budget link fails the cell's check; and public flows through the sdk host table arrive byte for byte as with no
// hook.
func TestLinkCellOnTheIos32Surrogate(t *testing.T) {
	cell := t4cell.NewCell(t4cell.Ios32Profile())
	profile := cell.Profile()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings := DefaultDeviceLocalSettings()
	settings.LowMemoryProfile = true
	bridgeSettings := deviceLocalBridgeSettingsFor(settings, "ios")

	// one link: its multi client in the link's shape, a fixed window of one post-quantum window client
	// (device.BridgeLinkMultiClientSettings), and that window client's whole client graph, on a phone's settings
	var keep []any
	linkRow, err := cell.Measure("one link", profile.ForwardingSteadyByteCount, t4cell.HoldsSteady, func() (any, error) {
		clientSettings := connect.DefaultClientSettings()
		clientSettings.Log = connect.NewNoopLogger()
		applyMobileLowMemoryClientSettingsForPlatform(clientSettings, settings.MemoryTargetByteCount, true)
		clientSettings.EncryptionSettings.Mode = connect.EncryptionModeRequired
		windowClient := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), clientSettings)
		multiClientSettings := sidecardevice.BridgeLinkMultiClientSettings(bridgeSettings)
		if err := sidecardevice.CheckBridgeLinkMultiClientSettings(multiClientSettings, bridgeSettings.LinkAllowDirect); err != nil {
			return nil, err
		}
		link := connect.NewRemoteUserNatMultiClient(ctx, &testingDnsOwnerGenerator{}, func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
		}, protocol.ProvideMode_Network, multiClientSettings)
		cell.Sample()
		keep = []any{windowClient, link}
		return keep, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("cell=link profile=%s step=%q peak=%d steady=%d budget=%d within=%t (reported: §22.9 decides links on iOS on a surrogate's run)",
		linkRow.Profile, linkRow.Step, linkRow.PeakByteCount, linkRow.SteadyByteCount, linkRow.BudgetByteCount, linkRow.Within())
	for _, kept := range keep {
		switch kept := kept.(type) {
		case *connect.Client:
			kept.Cancel()
		case *connect.RemoteUserNatMultiClient:
			kept.Close()
		}
	}

	// the cell catches a link past the budget: a planted one holding twice the forwarding role's steady budget
	planted := t4cell.NewCell(profile)
	if _, err := planted.Measure("a planted link at twice the budget", profile.ForwardingSteadyByteCount, t4cell.HoldsSteady, func() (any, error) {
		held := make([]byte, 2*profile.ForwardingSteadyByteCount)
		for i := range held {
			held[i] = byte(i)
		}
		return held, nil
	}); err != nil {
		t.Fatal(err)
	}
	if planted.CheckWithin() == nil {
		t.Fatalf("the cell let a link at twice the budget through:\n%s", planted.Table())
	}

	// an app host's exit NAT under the sdk host table with public flows, beside a hookless NAT: each flow's bytes
	// arrive exactly as without the hook, and its decision keeps the NAT's own idle
	const flowCount = 32
	policy := sidecardevice.NewBridgeEgressPolicy(sidecardevice.BridgePolicyProfileSdkHost, bridgeSettings)
	hookedUpstream := newTestingLinkCellUpstream(t)
	var hookedNat *connect.LocalUserNat
	var hookedPayloads [][]byte
	natRow, err := cell.Measure("app host NAT, 32 public flows", 0, t4cell.HoldsSteady, func() (any, error) {
		hookedNat, hookedPayloads = testingLinkCellPublicFlows(t, ctx, policy, hookedUpstream, flowCount, "hooked")
		hookedUpstream.await(t, flowCount)
		cell.Sample()
		return hookedNat, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer hookedNat.Close()
	t.Logf("cell=link profile=%s step=%q peak=%d steady=%d (reported)", natRow.Profile, natRow.Step, natRow.PeakByteCount, natRow.SteadyByteCount)
	if got := hookedUpstream.await(t, flowCount); !slices.EqualFunc(got, hookedPayloads, bytes.Equal) {
		t.Errorf("through the sdk host table the upstream read %q, want %q", got, hookedPayloads)
	}
	plainUpstream := newTestingLinkCellUpstream(t)
	plainNat, plainPayloads := testingLinkCellPublicFlows(t, ctx, nil, plainUpstream, flowCount, "hooked")
	defer plainNat.Close()
	if got := plainUpstream.await(t, flowCount); !slices.EqualFunc(got, plainPayloads, bytes.Equal) ||
		!slices.EqualFunc(got, hookedUpstream.await(t, flowCount), bytes.Equal) {
		t.Error("public flows' bytes differ with the hook and without it")
	}
	publicFlow := &connect.EgressFlow{ProvideMode: protocol.ProvideMode_Public, Protocol: connect.IpProtocolUdp,
		From: netip.MustParseAddrPort("198.51.100.20:41000"), To: hookedUpstream.address()}
	if decision := policy.ResolveEgress(publicFlow); decision.Verdict != connect.EgressVerdictPass || decision.IdleTimeout != 0 || decision.Target.IsValid() {
		t.Errorf("a public flow's decision %+v", decision)
	}
	t.Logf("link cell, ios32:\n%s", cell.Table())
}
