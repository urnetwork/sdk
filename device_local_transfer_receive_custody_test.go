package sdk

import (
	"context"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Interrupt the first contract refusal at an application-supplied logger,
// after a real receive worker took the pack and materialized its item.
type custodyTestLogger struct {
	connect.Logger
	interrupted bool
}

func (l *custodyTestLogger) Errorf(format string, args ...any) {
	if !l.interrupted && strings.HasPrefix(format, "[r]drop queue head no contract") {
		l.interrupted = true
		panic("synthetic receive interruption")
	}
	l.Logger.Errorf(format, args...)
}

// Build the mobile client and provider hierarchies with their shipping
// factories. Use cleartext here to isolate the allocator and callback boundary.
func TestDeviceLocalMobileReceiveCustody(t *testing.T) {
	connect.MessagePoolOutstandingCount()
	for _, providerRole := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			device, provider := transferMemoryTestDevice(t, 32*1024*1024)
			device.applyProvideMemorySharesWithLock(true)
			settings := &device.settings.ClientSettings
			if providerRole {
				settings = provider
			}
			settings.EncryptionSettings.Mode = connect.EncryptionModeOff
			settings.ControlPingTimeout = 0
			settings.ContractManagerSettings.NetworkEventTimeEnableContracts = time.Unix(1, 0)
			log := &custodyTestLogger{Logger: connect.NewNoopLogger()}
			settings.Log = log
			c := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), settings)
			defer func() {
				cancel()
				if err := c.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
			}()
			incoming := make(connect.Route, 1)
			c.RouteManager().UpdateTransport(connect.NewReceiveGatewayTransportWithType(connect.TransportTypeH1), []connect.Route{incoming})
			rx, root := settings.ReceiveBufferSettings.ReceiveQueueBudget, device.transferMemory.root
			healthy := connect.NewId()
			c.ContractManager().AddNoContractPeer(healthy)
			delivered := 0
			c.AddReceiveCallback(func(src connect.TransferPath, frames []*protocol.Frame, _ connect.Peer) {
				if src.SourceId == healthy {
					delivered += len(frames)
				}
			})
			synctest.Wait()
			start := root.UsedByteCount()
			send := func(peer connect.Id, frame *protocol.Frame) {
				wire, err := connect.ProtoMarshal(&protocol.TransferFrame{TransferPath: connect.TransferPath{SourceId: peer, DestinationId: c.ClientId()}.ToProtobuf(), Pack: &protocol.Pack{MessageId: connect.NewId().Bytes(), SequenceId: connect.NewId().Bytes(), Head: true, Frames: []*protocol.Frame{frame}}})
				if err != nil {
					t.Fatal(err)
				}
				incoming <- wire
				synctest.Wait()
			}
			send(connect.NewId(), &protocol.Frame{MessageType: protocol.MessageType_IpIpPacketToProvider})
			if !log.interrupted || rx.UsedByteCount() != 0 || root.UsedByteCount() != start {
				t.Errorf("provider=%v interrupted=%v leaf=%d root change=%d", providerRole, log.interrupted, rx.UsedByteCount(), root.UsedByteCount()-start)
			}
			frame := connect.RequireToFrameWithDefaultProtocolVersion(&protocol.SimpleMessage{Content: "healthy"})
			send(healthy, frame)
			connect.MessagePoolReturn(frame.MessageBytes)
			if delivered != 1 || rx.UsedByteCount() != 0 || root.UsedByteCount() != start {
				t.Errorf("provider=%v delivered=%d leaf=%d root change=%d", providerRole, delivered, rx.UsedByteCount(), root.UsedByteCount()-start)
			}
		})
	}
}
