//go:build !ios

package sdk

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"google.golang.org/protobuf/proto"
)

// The fixture still publishes the actual H1 lane classification, but neither
// reliable nor unknown carriers may park the shared Client pump at a full
// logical queue. These are explicit NoAck packets: exactly the accepted prefix
// drains, and the refused packet has no delivery or retry guarantee. They carry
// DATA frames: Connect hands protocol-only originals to a separate protocol
// mailbox, outside the one-slot DATA queue this fixture fills.
func TestH1OwnerFixtureReliableProviderHandoff(t *testing.T) {
	for _, reliable := range []bool{false, true} {
		name := "old-unknown-drops"
		if reliable {
			name = "h1-refuses-full"
		}
		t.Run(name, func(t *testing.T) {
			settings := connect.DefaultClientSettings()
			settings.Log = connect.NewNoopLogger()
			settings.EncryptionSettings.Mode = connect.EncryptionModeOff
			settings.ReceiveBufferSettings.SequenceBufferSize = 1
			settings.ReceiveBufferSettings.H1SequenceBufferSize = 1
			client := connect.NewClient(t.Context(), connect.NewId(), connect.NewNoContractClientOob(), settings)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := client.CloseAndWait(ctx); err != nil {
					t.Error(err)
				}
			})
			source, sequence := connect.NewId(), connect.NewId()
			client.ContractManager().AddNoContractPeer(source)
			incoming := make(chan []byte, 3)
			_, receive, properties := h1OwnerRelayTransports(client.ClientId())
			if !reliable {
				receive = connect.NewReceiveGatewayTransport()
				properties = connect.TransferCarrierProperties{}
			}
			client.RouteManager().UpdateTransportWithProperties(receive, []connect.Route{incoming}, properties)
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			delivered := make(chan byte, 3)
			client.AddReceiveCallback(func(_ connect.TransferPath, frames []*protocol.Frame, _ connect.Peer) {
				for _, frame := range frames {
					marker := frame.MessageBytes[0]
					if marker == 0 {
						close(started)
						<-release
					}
					delivered <- marker
				}
			})
			send := func(marker byte) {
				wire, err := proto.Marshal(&protocol.TransferFrame{
					TransferPath: connect.TransferPath{SourceId: source, DestinationId: client.ClientId()}.ToProtobuf(),
					Pack: &protocol.Pack{MessageId: connect.NewId().Bytes(), SequenceId: sequence.Bytes(),
						SequenceNumber: uint64(marker), Head: marker == 0, Nack: true,
						Frames: []*protocol.Frame{{MessageType: protocol.MessageType_TestSimpleMessage, MessageBytes: []byte{marker}}}},
				})
				if err != nil {
					t.Fatal(err)
				}
				incoming <- connect.MessagePoolCopy(wire)
			}
			send(0)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("callback did not start")
			}
			send(1)
			send(2)
			deadline := time.Now().Add(time.Second)
			for {
				stats := client.ReceiveStats()
				if stats.PackHandoffDropCount > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("no saturation evidence: reliable=%t wait=%d drop=%d", reliable, stats.PackHandoffWaitCount, stats.PackHandoffDropCount)
				}
				runtime.Gosched()
			}
			unblock()
			want := 2
			for i := range want {
				select {
				case got := <-delivered:
					if got != byte(i) {
						t.Fatalf("out of order: got=%d want=%d", got, i)
					}
				case <-time.After(time.Second):
					t.Fatalf("missing packet %d", i)
				}
			}
			// The one-slot DATA queue held the accepted packet; a protocol
			// mailbox would leave this count at zero.
			if stats := client.ReceiveStats(); stats.PackHandoffWaitCount != 0 || stats.PackHandoffDropCount != 1 || stats.PackHandoffMaxCount != 1 {
				t.Fatalf("unexpected carrier handoff: reliable=%t drops=%d max=%d", reliable, stats.PackHandoffDropCount, stats.PackHandoffMaxCount)
			}
		})
	}
}
