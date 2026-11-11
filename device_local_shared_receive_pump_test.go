//go:build !ios

package sdk

import (
	"context"
	"encoding/binary"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

func TestMobile32WindowFactorySharedPumpRefusesNoAckWithoutWaiting(t *testing.T) {
	testingMobileWindowSharedPump(t, 32*1024*1024)
}

func TestMobile64WindowFactorySharedPumpRefusesNoAckWithoutWaiting(t *testing.T) {
	testingMobileWindowSharedPump(t, 64*1024*1024)
}

// The real DeviceLocal destination factory supplies every queue, budget and
// timeout below. Its explicit negative handoff settings must not park the
// shared Client pump. This deliberately uses one-shot NoAck packets: a refused
// packet is not delivered or acknowledged, and this fixture never replays it.
// Reliable-original recovery is covered by the separate real-peer ACK witness.
func testingMobileWindowSharedPump(t *testing.T, target ByteCount) {
	t.Helper()
	connect.GetMessagePoolAggregateStats()
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.MemoryTargetByteCount = target
	settings.HostedIncompatible = true
	settings.KeepWindowClientKeysLocal = true
	clientSettings := testingWindowClientSettings(t, settings)
	receive := clientSettings.ReceiveBufferSettings
	if receive.ReliablePackHandoffTimeout != -time.Nanosecond || receive.H1PackHandoffTimeout != -time.Nanosecond ||
		receive.H1AckHandoffTimeout != time.Millisecond || receive.H1SequenceBufferAdaptiveMaxSize != 0 ||
		receive.PackQueueBudget == nil || receive.ReceiveQueueBudget == nil ||
		receive.PackQueueRetainedByteAccounting || !receive.ReceiveQueueRetainedByteAccounting ||
		!clientSettings.SkipClientKeyPublication || clientSettings.EncryptionSettings.Mode != connect.EncryptionModeOff {
		t.Fatal("actual mobile window factory lost its policy or local-key fixture premise")
	}
	count := receive.H1SequenceBufferSize
	if count <= 0 || count > 512 {
		t.Fatal("actual H1 queue lies outside the finite fixture envelope")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source, sequence := connect.NewId(), connect.NewId()
	outcomes := make(chan bool, count+4)
	var observerOverflow atomic.Bool
	previousObserver := receive.ProgressObserver
	receive.ProgressObserver = func(event connect.TransferProgressEvent) {
		if previousObserver != nil {
			previousObserver(event)
		}
		if event.Stage == "receive_pack_end" && event.PeerId == source {
			select {
			case outcomes <- event.Success:
			default:
				observerOverflow.Store(true)
			}
		}
	}
	client := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), clientSettings)
	client.ContractManager().AddNoContractPeer(source)
	incoming := make(connect.Route, count+2)
	outgoing := make(connect.Route, count+2)
	_, transport, properties := h1OwnerRelayTransports(client.ClientId())
	client.RouteManager().UpdateTransportWithProperties(transport, []connect.Route{incoming}, properties)
	client.RouteManager().UpdateTransport(connect.NewSendGatewayTransport(), []connect.Route{outgoing})
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	delivered := make(chan uint32, count+2)
	client.AddReceiveCallback(func(path connect.TransferPath, frames []*protocol.Frame, _ connect.Peer) {
		if path.SourceId != source {
			return
		}
		for _, frame := range frames {
			if len(frame.MessageBytes) != 4 {
				t.Error("controlled payload changed length")
				continue
			}
			marker := binary.BigEndian.Uint32(frame.MessageBytes)
			if marker == 0 {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
			select {
			case delivered <- marker:
			default:
				t.Error("receive callback exceeded the finite delivery envelope")
			}
		}
	})
	t.Cleanup(func() {
		unblock()
		cancel()
		join, cancelJoin := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelJoin()
		if err := client.CloseAndWait(join); err != nil {
			t.Error(err)
		}
		for _, route := range []connect.Route{incoming, outgoing} {
			for len(route) != 0 {
				connect.MessagePoolReturn(<-route)
			}
		}
		for _, budget := range []*connect.TransferMemoryBudget{receive.PackQueueBudget, receive.ReceiveQueueBudget, receive.ReceiveHeadRecoveryBudget} {
			if budget == nil {
				continue
			}
			stats := budget.Stats()
			if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Error("window Client did not return its actual queue budget")
			}
		}
	})
	offer := func(marker int) {
		payload := make([]byte, 4)
		binary.BigEndian.PutUint32(payload, uint32(marker))
		wire, err := connect.ProtoMarshal(&protocol.TransferFrame{
			TransferPath: connect.TransferPath{SourceId: source, DestinationId: client.ClientId()}.ToProtobuf(),
			Pack: &protocol.Pack{MessageId: connect.NewId().Bytes(), SequenceId: sequence.Bytes(),
				SequenceNumber: uint64(marker), Head: marker == 0, Nack: true,
				Frames: []*protocol.Frame{{MessageType: protocol.MessageType_TransferExchangeSignals, MessageBytes: payload}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		incoming <- wire
	}
	outcome := func() bool {
		select {
		case accepted := <-outcomes:
			return accepted
		case <-time.After(2 * time.Second):
			t.Fatal("shared Client waited for an unrelated callback despite a full queue")
			return false
		}
	}
	offer(0)
	if !outcome() {
		t.Fatal("first actual NoAck original was refused")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("independent application callback did not start")
	}
	for marker := 1; marker <= count; marker++ {
		offer(marker)
		if !outcome() {
			t.Fatal("actual queue refused before its configured count limit")
		}
	}
	refusalDeadline := time.Now().Add(2 * time.Second)
	offer(count + 1)
	if outcome() {
		t.Fatal("full actual queue admitted the extra NoAck original")
	}
	// The false disposition observer runs just before the Client records its
	// drop. Settle that already-observed refusal within the same watchdog;
	// this is not a second admission attempt or callback release.
	for client.ReceiveStats().PackHandoffDropCount == 0 && time.Now().Before(refusalDeadline) {
		runtime.Gosched()
	}
	before := client.ReceiveStats()
	if before.PackHandoffWaitCount != 0 || before.PackHandoffDropCount != 1 ||
		before.PackHandoffMaxCount != uint64(count) || len(delivered) != 0 || len(outgoing) != 0 || observerOverflow.Load() {
		t.Fatal("full queue did not truthfully refuse without ACK or callback progress")
	}
	unblock()
	for marker := 0; marker <= count; marker++ {
		select {
		case got := <-delivered:
			if got != uint32(marker) {
				t.Fatal("accepted NoAck prefix changed order or duplicated delivery")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("accepted NoAck prefix did not drain")
		}
	}
	join, cancelJoin := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelJoin()
	if err := client.CloseAndWait(join); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 0 || len(outgoing) != 0 {
		t.Fatal("refused NoAck was redelivered or gained an ACK")
	}
	if receive.ReliablePackHandoffTimeout != -time.Nanosecond || receive.H1PackHandoffTimeout != -time.Nanosecond ||
		receive.H1SequenceBufferSize != count {
		t.Fatal("runtime changed the factory's explicit policy")
	}
}
