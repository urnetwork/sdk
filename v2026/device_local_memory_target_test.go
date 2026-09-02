package sdk

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"crypto/tls"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect/v2026"
)

// TestDeviceLocalMemoryTargetUnderLoad drives one device's memory areas
// concurrently and checks the tracked accounting (`MemoryUsed`) stays within
// the device memory target (`DeviceLocalSettings.MemoryTargetByteCount`):
//
//   - device send/receive: loopback echo through the device tun over the
//     route-local path
//   - provider send/receive: remote peers egressing tcp+udp echo through the
//     device's provider (the provider client's own budget pair)
//   - dns: concurrent doh resolutions against a local doh server, drawing
//     from the device's dns byte budget
//
// The window-client transfer path needs a platform window, so its pair is
// sized here but idles; the churn/grid tests cover that path. Tracked usage
// must stay within the target plus the documented admission overdraft
// (budgets can overdraft up to ~one message per sequence past an admission
// that saw headroom). The process-heap telemetry line records the untracked
// remainder (nat flows, channels, goroutines) for comparison across changes.
func TestDeviceLocalMemoryTargetUnderLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping DeviceLocal memory target test in -short mode")
	}
	if runIsolatedLoadTest(t) {
		return
	}

	const memoryTargetByteCount = 20 * 1024 * 1024
	// budgets admission-gate then reserve, so the tracked total can pass the
	// target by up to ~one message per active sequence
	const overdraftSlackByteCount = 512 * 1024

	const (
		peerCount       = 4
		rounds          = 2
		flowsPerPeer    = 2
		bytesPerFlow    = 96 << 10
		udpFlowsPerPeer = 16

		loopbackRounds       = 2
		loopbackFlows        = 2
		loopbackBytesPerFlow = 128 << 10

		dnsQueryCount       = 384
		dnsQueryConcurrency = 64
	)

	pinIosGcPacing(t)
	// Testing cleanups run after this function's deferred device, peer, bridge,
	// and resolver teardown. Wait for their asynchronous gVisor stack shutdown
	// so a following memory-budget test does not inherit transient heap and
	// goroutines from this one.
	t.Cleanup(func() {
		sampleStable()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	echoAddr, closeEcho := startTcpEchoServer(t)
	defer closeEcho()
	udpEchoAddr, closeUdpEcho := startUdpEchoServer(t)
	defer closeUdpEcho()
	dohUrl, dohTlsConfig, closeDoh := startLocalDohServer(t)
	defer closeDoh()

	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.Verbose = false
	// the explicit per-device target under test
	settings.MemoryTargetByteCount = memoryTargetByteCount
	device, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		NewId(),
		settings,
		connect.NewId(),
	)
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()
	// Every measured path below is in-process. Stop the independent carrier
	// from reconnecting to the fake platform host so its transient H3-over-DNS
	// allocations do not consume the target during an unrelated load phase.
	platformTransport := func() migratablePlatformTransport {
		device.provider.stateLock.Lock()
		defer device.provider.stateLock.Unlock()
		return device.provider.platformTransport
	}()
	platformTransport.Close()

	// provider path
	device.SetProvideMode(ProvideModePublic)
	if !device.GetProvideEnabled() {
		t.Fatal("expected provide enabled")
	}
	providerClient := device.provider.Client()
	peers := make([]*providerLoadPeer, peerCount)
	for i := range peers {
		peers[i] = newProviderLoadPeer(t, ctx, providerClient)
	}
	defer func() {
		for _, peer := range peers {
			peer.close()
		}
	}()

	// device's own path (route local through the exit nat)
	tun, bridgeTeardown := newLoopbackBridgeForDevice(t, device)
	defer bridgeTeardown()

	// dns path: a resolver drawing from the device's dns byte budget, the
	// same wiring the device's mux resolvers get (see DohSettings.MemoryTarget)
	dohSettings := connect.DefaultDohSettings()
	dohSettings.MemoryTarget = device.dnsMemoryTarget
	dohSettings.DohServerStagger = 0
	dohSettings.DnsResolverSettings = &connect.DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: []string{dohUrl},
		TlsConfig:         dohTlsConfig,
	}
	dohCache := connect.NewDohCache(dohSettings)
	defer dohCache.Close()

	// sample the tracked accounting through the load, keeping per-area peaks
	var samplePeak DeviceLocalMemoryUsage
	sampleStop := make(chan struct{})
	var sampleWg sync.WaitGroup
	sampleWg.Add(1)
	go func() {
		defer sampleWg.Done()
		for {
			usage := device.MemoryUsed()
			samplePeak.DnsByteCount = max(samplePeak.DnsByteCount, usage.DnsByteCount)
			samplePeak.ClientSendByteCount = max(samplePeak.ClientSendByteCount, usage.ClientSendByteCount)
			samplePeak.ClientReceiveByteCount = max(samplePeak.ClientReceiveByteCount, usage.ClientReceiveByteCount)
			samplePeak.ProviderSendByteCount = max(samplePeak.ProviderSendByteCount, usage.ProviderSendByteCount)
			samplePeak.ProviderReceiveByteCount = max(samplePeak.ProviderReceiveByteCount, usage.ProviderReceiveByteCount)
			samplePeak.TotalByteCount = max(samplePeak.TotalByteCount, usage.TotalByteCount)
			select {
			case <-sampleStop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()

	heapSampler := startPeakSampler()

	// run the three loads concurrently
	loadErrs := make(chan error, peerCount+2)
	for _, peer := range peers {
		go func() {
			loadErrs <- runLoadIteration(ctx, peer.tun, echoAddr, rounds, flowsPerPeer, bytesPerFlow)
		}()
	}
	go func() {
		loadErrs <- runLoadIteration(ctx, tun, echoAddr, loopbackRounds, loopbackFlows, loopbackBytesPerFlow)
	}()
	go func() {
		sem := make(chan struct{}, dnsQueryConcurrency)
		var wg sync.WaitGroup
		for i := 0; i < dnsQueryCount; i++ {
			sem <- struct{}{}
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				// distinct names defeat the answer cache, so every query is
				// a real resolution holding a dns reservation
				addrs, _ := dohCache.QueryResult(ctx, "A", fmt.Sprintf("q%d.device-mem.test", i))
				_ = addrs
			}(i)
		}
		wg.Wait()
		loadErrs <- nil
	}()
	for i := 0; i < peerCount+2; i += 1 {
		if err := <-loadErrs; err != nil {
			skipOnRaceGvisorWedge(t, "load", err)
			t.Fatalf("load: %v", err)
		}
	}

	// udp burst into the exit nat
	for _, peer := range peers {
		go func() {
			loadErrs <- runPeerUdpBurst(ctx, peer.tun, udpEchoAddr, udpFlowsPerPeer)
		}()
	}
	for range peers {
		if err := <-loadErrs; err != nil {
			skipOnRaceGvisorWedge(t, "udp burst", err)
			t.Fatalf("udp burst: %v", err)
		}
	}

	close(sampleStop)
	sampleWg.Wait()
	peakTotal, peakHeap, _ := heapSampler.stop()

	final := device.MemoryUsed()
	t.Logf("[device-mem-target] target=%s trackedPeak=%s (dns=%s clientSend=%s clientReceive=%s providerSend=%s providerReceive=%s) trackedFinal=%s peakHeap=%s peakTotal=%s",
		humanBytes(uint64(memoryTargetByteCount)),
		humanBytes(uint64(samplePeak.TotalByteCount)),
		humanBytes(uint64(samplePeak.DnsByteCount)),
		humanBytes(uint64(samplePeak.ClientSendByteCount)),
		humanBytes(uint64(samplePeak.ClientReceiveByteCount)),
		humanBytes(uint64(samplePeak.ProviderSendByteCount)),
		humanBytes(uint64(samplePeak.ProviderReceiveByteCount)),
		humanBytes(uint64(final.TotalByteCount)),
		humanBytes(uint64(peakHeap)),
		humanBytes(uint64(peakTotal)))

	// the dns load must draw real reservations from the dns byte budget
	if samplePeak.DnsByteCount == 0 {
		t.Errorf("expected dns load to draw from the dns byte budget")
	}
	// the provider must own its own pair (the target split wiring). its
	// tracked usage registers only when queues borrow above their
	// per-sequence floors, which the lossless in-memory echo rarely forces —
	// the load completing (echo verified) is the proof the provider path
	// carried the traffic.
	if resendQueueBudget, receiveQueueBudget := device.provider.transferBudgets(); resendQueueBudget == nil || receiveQueueBudget == nil {
		t.Errorf("expected the provider client to own its own budget pair under a device memory target")
	}

	// tracked usage must stay within the device target (+ admission overdraft)
	if memoryTargetByteCount+overdraftSlackByteCount < samplePeak.TotalByteCount {
		t.Errorf("tracked peak %s exceeds the device memory target %s (+%s overdraft slack)",
			humanBytes(uint64(samplePeak.TotalByteCount)),
			humanBytes(uint64(memoryTargetByteCount)),
			humanBytes(uint64(overdraftSlackByteCount)))
	}
	// drained: the tracked accounting must return toward zero (no lost releases)
	if memoryTargetByteCount/4 < final.TotalByteCount {
		t.Errorf("tracked usage %s did not drain after load", humanBytes(uint64(final.TotalByteCount)))
	}
}

// startLocalDohServer serves RFC 8484 wire-format doh (GET ?dns=..., h2) with
// a synthetic A answer for every question, so dns load needs no network.
func startLocalDohServer(t *testing.T) (dohUrl string, tlsConfig *tls.Config, closeFn func()) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var msg dnsmessage.Message
		if err := msg.Unpack(wire); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		msg.Response = true
		msg.Authoritative = true
		if len(msg.Questions) == 1 && msg.Questions[0].Type == dnsmessage.TypeA {
			msg.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{
					Name:  msg.Questions[0].Name,
					Type:  dnsmessage.TypeA,
					Class: dnsmessage.ClassINET,
					TTL:   60,
				},
				Body: &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}},
			}}
		}
		out, err := msg.Pack()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(out)
	}))
	// the doh client speaks h2 only (see httpClientWithDialer)
	server.EnableHTTP2 = true
	server.StartTLS()
	transport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		server.Close()
		t.Fatal("unexpected test server client transport")
	}
	return server.URL + "/dns-query", transport.TLSClientConfig, server.Close
}
