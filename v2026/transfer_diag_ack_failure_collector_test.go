// The real SDK collector retains a real failed ACK after its window candidate retires.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// One finite receiver attempt, constructed with the window's actual settings.
type ackFailureCollectorAttempt struct {
	client     *connect.Client
	sequence   *connect.ReceiveSequence
	source     connect.TransferPath
	sequenceId connect.Id
}

// Offers one inert destination, never creates a transport or external connection.
type ackFailureCollectorGenerator struct {
	device      *DeviceLocal
	destination connect.MultiHopId
	minted      atomic.Bool
	attempt     chan ackFailureCollectorAttempt
	release     chan struct{}
	completed   chan error
	releaseOnce sync.Once
}

// Fixed discovery keeps this fixture on the real quality-window expansion path.
func (self *ackFailureCollectorGenerator) NextDestinations(_ int, _ []connect.MultiHopId, _ string) (map[connect.MultiHopId]connect.DestinationStats, error) {
	return map[connect.MultiHopId]connect.DestinationStats{self.destination: {}}, nil
}

// The one candidate is bounded independently from retry or enumeration cadence.
func (self *ackFailureCollectorGenerator) NewClientArgs() (*connect.MultiClientGeneratorClientArgs, error) {
	if !self.minted.CompareAndSwap(false, true) {
		return nil, errors.New("synthetic candidate already consumed")
	}
	return &connect.MultiClientGeneratorClientArgs{ClientId: connect.NewId()}, nil
}

// No platform identity was registered by the synthetic generator.
func (*ackFailureCollectorGenerator) RemoveClientArgs(*connect.MultiClientGeneratorClientArgs) {}

// No candidate is returned successfully, so there is no channel-owned transport.
func (*ackFailureCollectorGenerator) RemoveClientWithArgs(*connect.Client, *connect.MultiClientGeneratorClientArgs) {
}

// Uses the same device-owned opt-in seam as production window settings.
func (self *ackFailureCollectorGenerator) NewClientSettings() *connect.ClientSettings {
	settings := connect.DefaultClientSettings()
	settings.Log = connect.NewNoopLogger()
	settings.ControlPingTimeout = 0
	settings.EncryptionSettings.Mode = connect.EncryptionModeOff
	self.device.attachTransferDiag(settings)
	return settings
}

// The actual window passes its counter owner before this client/receiver starts.
func (self *ackFailureCollectorGenerator) NewClient(ctx context.Context, args *connect.MultiClientGeneratorClientArgs, settings *connect.ClientSettings) (*connect.Client, error) {
	client := connect.NewClient(ctx, args.ClientId, connect.NewNoContractClientOob(), settings)
	receiveSettings := connect.DefaultReceiveBufferSettings()
	receiveSettings.AckCompressTimeout = 0
	receiveSettings.WriteTimeout = 0
	receiveSettings.IdleTimeout = time.Hour
	source, sequenceId := connect.SourceId(connect.NewId()), connect.NewId()
	sequence := connect.NewReceiveSequence(ctx, client, source, sequenceId, 0, false, receiveSettings)
	go sequence.Run()
	select {
	case self.attempt <- ackFailureCollectorAttempt{client: client, sequence: sequence, source: source, sequenceId: sequenceId}:
	case <-ctx.Done():
	}
	select {
	case <-self.release:
	case <-ctx.Done():
	}
	sequence.Cancel()
	sequence.WaitForExit()
	joinCtx, cancelJoin := context.WithTimeout(context.Background(), time.Minute)
	joinErr := client.CloseAndWait(joinCtx)
	cancelJoin()
	self.completed <- joinErr
	return nil, errors.New("synthetic candidate retired after joined receiver")
}

// A fixed destination creates only the quality window, not a second independent attempt.
func (*ackFailureCollectorGenerator) FixedDestinationSize() (int, bool) { return 1, true }

// Releases the owned receiver exactly once, including on an earlier assertion failure.
func (self *ackFailureCollectorGenerator) stop() {
	self.releaseOnce.Do(func() { close(self.release) })
}

// Omitting the collector's lifetime assignment loses this real empty-window JSON evidence.
func TestTransferDiagAckRouteFailureCollectorRetainsRetiredOwner(t *testing.T) {
	// Pool statistics have a process-lifetime worker. Initialize its public
	// snapshot before entering the finite synthetic receiver's clock domain.
	_, _, _ = connect.MessagePoolCounts()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		device, _ := transferMemoryTestDevice(t, 32*1024*1024)
		device.transferDiagStats = &connect.P2pDataPlaneStats{}
		device.memorySampler = &mobileMemorySampler{}
		generator := &ackFailureCollectorGenerator{
			device: device, destination: connect.RequireMultiHopId(connect.NewId()),
			attempt: make(chan ackFailureCollectorAttempt, 1), release: make(chan struct{}), completed: make(chan error, 1),
		}
		settings := connect.DefaultMultiClientSettings()
		settings.Log = connect.NewNoopLogger()
		multi := connect.NewRemoteUserNatMultiClient(ctx, generator, nil, protocol.ProvideMode_Network, settings)
		device.remoteUserNatClient = multi
		attemptReceived, joined := false, false
		defer func() {
			generator.stop()
			cancel()
			joinCtx, cancelJoin := context.WithTimeout(context.Background(), time.Minute)
			defer cancelJoin()
			if err := multi.CloseAndWait(joinCtx); err != nil {
				t.Errorf("multi-client owner did not join: %v", err)
			}
			if attemptReceived && !joined {
				select {
				case err := <-generator.completed:
					if err != nil {
						t.Errorf("receiver client did not join: %v", err)
					}
				case <-joinCtx.Done():
					t.Error("receiver cleanup did not complete")
				}
			}
		}()
		var attempt ackFailureCollectorAttempt
		select {
		case attempt = <-generator.attempt:
			attemptReceived = true
		case <-time.After(time.Minute):
			t.Fatal("real window expansion did not construct the receiver")
		}
		pack := &connect.ReceivePack{
			Ctx: ctx, Source: attempt.source, SequenceId: attempt.sequenceId,
			Pack:          &protocol.Pack{Head: true, MessageId: connect.NewId().Bytes(), ContractId: connect.NewId().Bytes()},
			TransportType: connect.TransportTypeH1, Unwrapped: true,
		}
		if accepted, err := attempt.sequence.Pack(pack, 0); !accepted || err != nil {
			t.Fatalf("synthetic compact head was not admitted: accepted=%t error=%v", accepted, err)
		}
		synctest.Wait()
		if stats := attempt.client.ReceiveStats(); stats.AckRouteWriteFailures == nil || stats.AckRouteWriteFailures.RouteTimeoutCount == 0 {
			t.Fatalf("actual ACK did not reach the empty-route failure boundary: %+v", stats.AckRouteWriteFailures)
		}
		generator.stop()
		if err := <-generator.completed; err != nil {
			t.Fatalf("receiver client did not join: %v", err)
		}
		joined = true
		synctest.Wait()
		want := attempt.client.ReceiveStats().AckRouteWriteFailures
		if live := multi.MemorySnapshot(); live.QualityClientCount != 0 || live.SpeedClientCount != 0 || live.AckRouteWriteErrorCount != 0 {
			t.Fatalf("retired fixture did not leave an empty live window: %+v", live)
		}
		batch, err := device.TransferDiagnosticSnapshotJson()
		if err != nil {
			t.Fatal(err)
		}
		var state map[string]any
		for _, line := range strings.Split(strings.TrimSpace(batch), "\n") {
			var row map[string]any
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatal(err)
			}
			if row["part"] == "state" {
				state = row
			}
		}
		failure, ok := state["quality_ack_route_write_lifetime_failures"].(map[string]any)
		if !ok || failure["ErrorCount"] != float64(want.ErrorCount) || failure["RouteTimeoutCount"] != float64(want.RouteTimeoutCount) || state["quality_client_count"] != float64(0) || state["window_client_count"] != float64(0) {
			t.Fatalf("actual collector lost retired-owner ACK evidence: failure=%+v expected=%+v", failure, want)
		}
		if state["speed_ack_route_write_lifetime_failures"] != nil {
			t.Fatal("quality failure contaminated an independent speed owner")
		}
	})
}
