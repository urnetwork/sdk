// The real contract and window boundaries expose inherited backend degradation.
package sdk

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// Observe callbacks only after the actual contract manager has consumed them.
type reraceObservedOob struct {
	control   connect.OutOfBandControl
	completed atomic.Int64
	failures  atomic.Int64
}

func (self *reraceObservedOob) SendControl(frames []*protocol.Frame, callback connect.OobResultFunction) {
	self.control.SendControl(frames, func(results []*protocol.Frame, err error) {
		callback(results, err)
		if err != nil {
			self.failures.Add(1)
		}
		self.completed.Add(1)
	})
}

// A successful synthetic backend round trip restores the state after the test,
// through the same public producer that records failures, not a private reset.
type reraceSuccessfulOob struct{}

func (reraceSuccessfulOob) SendControl(frames []*protocol.Frame, callback connect.OobResultFunction) {
	for _, frame := range frames {
		connect.MessagePoolReturn(frame.MessageBytes)
	}
	callback(nil, nil)
}

// Fixed discovery offers two inert destinations. The actual expansion owns the
// context-aware mint, so entry proves that its target has already been published.
type reraceHeldWindowGenerator struct {
	rpcLeakTestGenerator
	destinations [2]connect.MultiHopId
	entered      chan struct{}
	returned     chan struct{}
	enterOnce    sync.Once
	returnOnce   sync.Once
	calls        atomic.Int64
	legacyCalls  atomic.Int64
}

func (self *reraceHeldWindowGenerator) NextDestinations(_ int, _ []connect.MultiHopId, _ string) (map[connect.MultiHopId]connect.DestinationStats, error) {
	return map[connect.MultiHopId]connect.DestinationStats{
		self.destinations[0]: {},
		self.destinations[1]: {},
	}, nil
}

func (*reraceHeldWindowGenerator) FixedDestinationSize() (int, bool) {
	return 2, true
}

func (self *reraceHeldWindowGenerator) NewClientArgs() (*connect.MultiClientGeneratorClientArgs, error) {
	self.legacyCalls.Add(1)
	return nil, errors.New("unexpected legacy mint in context-aware fixed fixture")
}

func (self *reraceHeldWindowGenerator) NewClientArgsContext(ctx context.Context) (*connect.MultiClientGeneratorClientArgs, error) {
	self.calls.Add(1)
	self.enterOnce.Do(func() { close(self.entered) })
	defer self.returnOnce.Do(func() { close(self.returned) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// Seed three synchronous real OOB failures and witness the actual one-slot
// degraded window before entering RR-13. The isolated child runs every original
// dial/rerace/echo assertion without reseeding its fresh process.
func TestDeviceLocalDialFailureReraceIgnoresInheritedBackendFailures(t *testing.T) {
	if os.Getenv(isolatedLoadTestEnv) == t.Name() {
		t.Log("RR13_ISOLATED_ORIGINAL_ASSERTIONS")
		TestDeviceLocalDialFailureReracesConnectToAnotherExit(t)
		return
	}

	runControls := func(control connect.OutOfBandControl, count int, wantFailures int64) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		observed := &reraceObservedOob{control: control}
		settings := connect.DefaultClientSettings()
		settings.Log = connect.NewNoopLogger()
		client := connect.NewClient(ctx, connect.NewId(), observed, settings)
		defer func() {
			cancel()
			join, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := client.CloseAndWait(join); err != nil {
				t.Errorf("backend witness client retirement: %v", err)
			}
			if got := observed.completed.Load(); got != int64(count) {
				t.Errorf("backend callbacks after joined retirement = %d, want %d", got, count)
			}
		}()
		for i := 0; i < count; i++ {
			key := connect.ContractKey{Destination: connect.DestinationId(connect.NewId())}
			client.ContractManager().CreateContract(key, uint64(i), connect.ByteCount(1024))
			if got := observed.completed.Load(); got != int64(i+1) {
				t.Fatalf("CreateContract did not complete synchronously: %d, want %d", got, i+1)
			}
		}
		if got := observed.failures.Load(); got != wantFailures {
			t.Fatalf("actual OOB failures = %d, want %d", got, wantFailures)
		}
	}
	// Run last, including after the original's expected Fatal: neither arm may
	// leave the intentionally seeded process state for an unrelated next test.
	defer runControls(reraceSuccessfulOob{}, 1, 0)
	runControls(connect.NewNoContractClientOob(), 3, 3)

	func() {
		ctx, cancel := context.WithCancel(context.Background())
		generator := &reraceHeldWindowGenerator{
			destinations: [2]connect.MultiHopId{
				connect.RequireMultiHopId(connect.NewId()),
				connect.RequireMultiHopId(connect.NewId()),
			},
			entered:  make(chan struct{}),
			returned: make(chan struct{}),
		}
		settings := connect.DefaultMultiClientSettings()
		settings.Log = connect.NewNoopLogger()
		settings.ProviderProbe = false
		settings.HeartbeatInterval = 0
		multi := connect.NewRemoteUserNatMultiClient(ctx, generator, nil, protocol.ProvideMode_Network, settings)
		defer func() {
			cancel()
			join, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := multi.CloseAndWait(join); err != nil {
				t.Errorf("backend window retirement: %v", err)
			}
			if generator.calls.Load() != 0 {
				select {
				case <-generator.returned:
				case <-join.Done():
					t.Error("backend window mint did not join cancellation")
				}
			}
			if got := generator.legacyCalls.Load(); got != 0 {
				t.Errorf("unexpected legacy mint calls = %d", got)
			}
		}()
		select {
		case <-generator.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("backend window did not reach its actual context-aware mint")
		}
		event, providers := multi.Monitor().Events()
		if event == nil || event.TargetSize != 1 || event.MinSatisfied || len(providers) != 0 {
			t.Fatalf("seeded backend did not publish a one-slot unready window: event=%+v providers=%d", event, len(providers))
		}
		if got := generator.calls.Load(); got != 1 {
			t.Fatalf("context-aware mint calls = %d, want 1", got)
		}
		t.Log("RR13_INHERITED_BACKEND_TARGET_ONE_CONFIRMED")
	}()
	if t.Failed() {
		return
	}
	TestDeviceLocalDialFailureReracesConnectToAnotherExit(t)
}
