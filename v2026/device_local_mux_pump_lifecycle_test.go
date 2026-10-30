// Actual SDK destination ownership includes the shared internal TUN pump.
// The stalled work is a borrowed real TCP/53 SYNACK callback. Explicit gates
// retain scalar receipts; the pump completion primitive has an SDK owner.
package sdk

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

const muxPumpLifecycleWatchdog = 5 * time.Second

// Gates retain scalar receipts only. Packet bytes stay borrowed on the actual
// pump callback stack until release; every pump saved here has a real owner.
type muxPumpLifecycleFixture struct {
	device                *DeviceLocal
	muxes                 []*connect.UpgradeMux
	entered               chan bool
	replacementReply      chan bool
	callbackCloseReturned chan struct{}
	release               chan struct{}
	callbackExited        chan struct{}
	releaseOnce           sync.Once
	enteredOnce           atomic.Bool
	callbackActive        atomic.Bool
	callbackCount         atomic.Int64
	ownerDone             []<-chan struct{}
	baselineRoots         uint64
	baselineBytes         connect.ByteCount
}

// Only candidate enumeration is empty. The actual DeviceLocal constructor,
// selected mux, internal gVisor TCP server and downstream callback all run.
func newMuxPumpLifecycleFixture(t *testing.T, closeFromCallback bool) *muxPumpLifecycleFixture {
	t.Helper()
	for _, size := range []int{60, 256, 2048} {
		connect.MessagePoolReturn(connect.MessagePoolGet(size))
	}
	f := &muxPumpLifecycleFixture{
		entered:               make(chan bool, 1),
		replacementReply:      make(chan bool, 1),
		callbackCloseReturned: make(chan struct{}),
		release:               make(chan struct{}),
		callbackExited:        make(chan struct{}),
		baselineRoots:         connect.MessagePoolOutstandingCount(),
		baselineBytes:         connect.MessagePoolOutstandingByteCount(),
	}
	previousViolation := connect.SetMessagePoolViolationHandler(func(err error, _ []byte) {
		t.Errorf("mux pump lifecycle pool custody: %v", err)
	})
	t.Cleanup(func() { connect.SetMessagePoolViolationHandler(previousViolation) })
	var unsubscribe func()
	f.device = testingDnsOwnerDevice(t)
	t.Cleanup(func() {
		f.unpark()
		f.device.Close()
		for _, mux := range f.muxes {
			ctx, cancel := context.WithTimeout(context.Background(), muxPumpLifecycleWatchdog)
			if err := mux.WaitForPump(ctx); err != nil {
				t.Errorf("mux pump lifecycle cleanup did not join actual pump: %v", err)
			}
			cancel()
		}
		ctx, cancel := context.WithTimeout(context.Background(), muxPumpLifecycleWatchdog)
		if err := f.device.CloseAndWait(ctx); err != nil {
			t.Errorf("mux pump lifecycle cleanup did not join actual SDK owner: %v", err)
		}
		cancel()
		for _, done := range f.ownerDone {
			select {
			case <-done:
			case <-time.After(muxPumpLifecycleWatchdog):
				t.Error("mux pump lifecycle cleanup did not join an actual owner action")
			}
		}
		if unsubscribe != nil {
			unsubscribe()
		}
		// This fixture also owns its independent API/local-state lifetime.
		f.device.networkSpace.close()
		if roots, bytes := connect.MessagePoolOutstandingCount(), connect.MessagePoolOutstandingByteCount(); roots != f.baselineRoots || bytes != f.baselineBytes {
			t.Errorf("mux pump lifecycle pool imbalance after explicit pump and SDK joins: roots=%d/%d bytes=%d/%d", roots, f.baselineRoots, bytes, f.baselineBytes)
		}
	})
	settings := testingDnsOwnerSettings()
	settings.Dns.Resolver.DnsUpgradeMaskAddress = "198.51.100.53"
	f.device.SetUpgradeMuxSettings(settings)
	f.device.SetConnectLocation(testingStoredLocation("mux-pump-lifecycle.example"))
	f.device.stateLock.Lock()
	selected := f.device.upgradeMux
	f.device.stateLock.Unlock()
	if selected != nil {
		f.muxes = append(f.muxes, selected)
	}
	if selected == nil || !f.device.GetTunnelDnsInterceptorActive() {
		t.Fatal("mux pump lifecycle premise: no actual selected DNS mux")
	}
	unsubscribe = f.device.AddReceivePacketCallback(func(_ connect.TransferPath, _ protocol.ProvideMode, _ *connect.IpPath, packet []byte) {
		path, _, err := connect.ParseIpPathWithPayload(packet)
		if err != nil || path.Protocol != connect.IpProtocolTcp || !path.DestinationIp.Equal(net.IPv4(192, 0, 2, 20)) {
			return
		}
		valid := path.SourceIp.Equal(net.IPv4(198, 51, 100, 53)) && path.SourcePort == 53 && path.Syn && path.Ack && !path.Rst && path.AckSequenceNumber == 7001
		if path.DestinationPort == 47054 {
			select {
			case f.replacementReply <- valid:
			default:
			}
			return
		}
		if path.DestinationPort != 47053 {
			return
		}
		f.callbackCount.Add(1)
		if !f.enteredOnce.CompareAndSwap(false, true) {
			return
		}
		pooled, shared := connect.MessagePoolCheck(packet)
		f.callbackActive.Store(true)
		f.entered <- valid && pooled && !shared && 0 < connect.MessagePoolRootByteCount(packet)
		if closeFromCallback {
			f.device.Close()
			close(f.callbackCloseReturned)
		}
		<-f.release
		f.callbackActive.Store(false)
		close(f.callbackExited)
	})
	return f
}

// Release is idempotent so every assertion path can still finish the real pump.
func (self *muxPumpLifecycleFixture) unpark() {
	self.releaseOnce.Do(func() { close(self.release) })
}

// A checksum-valid SYN reaches the actual selected mux through the public SDK
// packet entry. No host TCP connection or DNS server is substituted.
func (self *muxPumpLifecycleFixture) sendSyn(t *testing.T, port uint16) {
	t.Helper()
	ip := &layers.IPv4{Version: 4, TTL: 64, SrcIP: net.IPv4(192, 0, 2, 20), DstIP: net.IPv4(198, 51, 100, 53), Protocol: layers.IPProtocolTCP}
	tcp := &layers.TCP{SrcPort: layers.TCPPort(port), DstPort: 53, SYN: true, Seq: 7000, Window: 65535}
	if err := tcp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatal("mux pump lifecycle premise: could not construct TCP checksum")
	}
	buffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, ip, tcp); err != nil {
		t.Fatal("mux pump lifecycle premise: could not construct SYN")
	}
	packet := buffer.Bytes()
	if !self.device.SendPacket(packet, int32(len(packet))) {
		t.Fatal("mux pump lifecycle premise: actual selected mux refused DNS SYN")
	}
}

// Callback entry proves actual pump read and borrowed pooled ownership. The
// timeout only bounds a missing premise; it cannot produce the causal marker.
func (self *muxPumpLifecycleFixture) holdReply(t *testing.T) {
	t.Helper()
	self.sendSyn(t, 47053)
	select {
	case valid := <-self.entered:
		if !valid || !self.callbackActive.Load() {
			t.Fatal("mux pump lifecycle premise: held callback was not exact pooled TCP53 SYNACK")
		}
	case <-time.After(muxPumpLifecycleWatchdog):
		t.Fatal("mux pump lifecycle premise: actual shared pump callback did not enter")
	}
}

// Close must return while the callback remains parked. Its existing resolver
// cancellation remains unchanged; no new pump wait may enter Close itself.
func (self *muxPumpLifecycleFixture) closeWithoutJoining(t *testing.T) {
	t.Helper()
	closed := make(chan struct{})
	self.ownerDone = append(self.ownerDone, closed)
	go func() {
		self.device.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(muxPumpLifecycleWatchdog):
		t.Fatal("mux pump lifecycle premise: Close joined its live pump callback")
	}
}

// A canceled owner context probes completion without a negative timing window.
// A held borrowed callback means this exact actual pump must still be live.
func (self *muxPumpLifecycleFixture) requireHeldPump(t *testing.T, mux *connect.UpgradeMux) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mux.WaitForPump(ctx); !errors.Is(err, context.Canceled) || !self.callbackActive.Load() {
		t.Fatal("mux pump lifecycle premise: borrowed callback did not retain its actual pump")
	}
}

// The finite context tests the real SDK join API while the packet callback is
// explicitly held. Only a positive nil SDK return with that exact pump still
// live is the old-owner failure; a watchdog or constructor error is not proof.
func (self *muxPumpLifecycleFixture) requireSdkOwnsHeldPump(t *testing.T, mux *connect.UpgradeMux, marker string) {
	t.Helper()
	self.requireHeldPump(t, mux)
	ctx, cancel := context.WithTimeout(context.Background(), muxPumpLifecycleWatchdog)
	err := self.device.CloseAndWait(ctx)
	cancel()
	self.requireHeldPump(t, mux)
	if err == nil {
		t.Errorf("%s: SDK lifecycle completed while exact pooled TCP53 callback remained borrowed", marker)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("mux pump lifecycle premise: unexpected bounded owner join result: %v", err)
	}
	self.unpark()
	self.requireJoined(t)
}

// Final completion is positive and bounded after the actual callback returns.
func (self *muxPumpLifecycleFixture) requireJoined(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), muxPumpLifecycleWatchdog)
	defer cancel()
	if err := self.device.CloseAndWait(ctx); err != nil {
		t.Fatalf("mux pump lifecycle owner did not join after callback release: %v", err)
	}
	for _, mux := range self.muxes {
		if err := mux.WaitForPump(ctx); err != nil {
			t.Fatalf("mux pump lifecycle reader did not join after callback release: %v", err)
		}
	}
	if self.callbackActive.Load() {
		t.Fatal("mux pump lifecycle owner completed before borrowed callback returned")
	}
	if self.enteredOnce.Load() {
		select {
		case <-self.callbackExited:
		default:
			t.Fatal("actual pump joined without the held callback's exit receipt")
		}
		if self.callbackCount.Load() != 1 {
			t.Fatal("mux pump lifecycle premise: more than the exact held SYNACK callback ran")
		}
	}
}

// Current selection must be admitted before Close's lifecycle Wait starts.
func TestDeviceLocalCloseAndWaitJoinsCurrentMuxPump(t *testing.T) {
	f := newMuxPumpLifecycleFixture(t, false)
	f.holdReply(t)
	f.closeWithoutJoining(t)
	f.requireSdkOwnsHeldPump(t, f.muxes[0], "DEVICE_CLOSE_JOIN_OMITTED_CURRENT_MUX_PUMP")
}

// A replacement remains independently serviceable while its retired pump is
// held. The final SDK join owns both generations, even after old field detach.
func TestDeviceLocalCloseAndWaitJoinsRetiredMuxPump(t *testing.T) {
	f := newMuxPumpLifecycleFixture(t, false)
	f.holdReply(t)
	replaced := make(chan struct{})
	f.ownerDone = append(f.ownerDone, replaced)
	go func() {
		f.device.Reconnect(testingStoredLocation("mux-pump-replacement.example"))
		close(replaced)
	}()
	select {
	case <-replaced:
	case <-time.After(muxPumpLifecycleWatchdog):
		t.Fatal("mux pump lifecycle premise: replacement waited for retired pump callback")
	}
	f.device.stateLock.Lock()
	replacement := f.device.upgradeMux
	f.device.stateLock.Unlock()
	if replacement != nil && replacement != f.muxes[0] {
		f.muxes = append(f.muxes, replacement)
	}
	if replacement == nil || replacement == f.muxes[0] || !f.device.GetTunnelDnsInterceptorActive() {
		t.Fatal("mux pump lifecycle premise: no independent actual replacement mux")
	}
	f.sendSyn(t, 47054)
	select {
	case valid := <-f.replacementReply:
		if !valid {
			t.Fatal("mux pump lifecycle premise: replacement did not serve exact independent TCP53 reply")
		}
	case <-time.After(muxPumpLifecycleWatchdog):
		t.Fatal("mux pump lifecycle premise: held retired pump blocked replacement delivery")
	}
	f.requireHeldPump(t, f.muxes[0])
	f.closeWithoutJoining(t)
	f.requireSdkOwnsHeldPump(t, f.muxes[0], "DEVICE_CLOSE_JOIN_OMITTED_RETIRED_MUX_PUMP")
}

// A native callback may request cancellation and then return. Only an
// independent owner calls the joining API; the callback never self-joins.
func TestDeviceLocalMuxPumpCallbackCloseDoesNotSelfJoin(t *testing.T) {
	f := newMuxPumpLifecycleFixture(t, true)
	f.holdReply(t)
	select {
	case <-f.callbackCloseReturned:
	case <-time.After(muxPumpLifecycleWatchdog):
		t.Fatal("mux pump callback Close did not return while callback remained borrowed")
	}
	f.requireHeldPump(t, f.muxes[0])
	f.unpark()
	f.requireJoined(t)
}

// Completion remains terminal after the admission lock publishes closed.
// A later destination setter cannot add another pump behind a completed Wait.
func TestDeviceLocalMuxPumpJoinIsTerminalAfterClose(t *testing.T) {
	f := newMuxPumpLifecycleFixture(t, false)
	f.closeWithoutJoining(t)
	f.requireJoined(t)
	f.device.stateLock.Lock()
	generation := f.device.connectionGeneration
	f.device.stateLock.Unlock()
	f.device.Reconnect(testingStoredLocation("closed-mux-owner.example"))
	f.device.stateLock.Lock()
	current, after := f.device.upgradeMux, f.device.connectionGeneration
	f.device.stateLock.Unlock()
	if current != nil || generation != after {
		t.Fatal("closed SDK admitted a mux after its lifecycle Wait completed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.muxes[0].WaitForPump(ctx); err != nil {
		t.Fatal("already joined pump lost terminal completion under canceled caller")
	}
}
