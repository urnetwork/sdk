package sdk

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestConnectGridBoundedOverRpcUnableToConnect reproduces the iOS wiring for the
// "connect grid dots grow unbounded while unable to connect" report: a real
// DeviceLocal (the network extension side) driven by a synthetic generator that
// can never reach a provider, a real DeviceLocalRpc/DeviceRemote pair over
// loopback (the app side), and the real ConnectViewController + ConnectGrid
// subscribed through the remote window monitor. The grid's point set must stay
// bounded while the multi client churns evaluations at the default timescales,
// including across connect-location re-sets (grid swaps).
func TestConnectGridBoundedOverRpcUnableToConnect(t *testing.T) {
	if testing.Short() {
		t.Skip("long-running leak reproduction")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()

	generator := &rpcLeakTestGenerator{}

	localSettings := testDeviceLocalSettingsRpc()
	localSettings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		return generator
	}

	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		localSettings,
		clientId,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceLocal.Close()

	rpcSettings := defaultDeviceRpcSettings()
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		rpcSettings,
		clientId,
		testing_deviceRpcDialer(rpcSettings),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceRemote.Close()

	// the app-side view controller, like iOS ConnectViewModel
	vc := newConnectViewController(ctx, deviceRemote)
	defer vc.Close()

	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{
			BestAvailable: true,
		},
	}
	vc.Connect(location)

	sampleGrid := func() (int, map[ProviderState]int, int32) {
		grid := vc.GetGrid()
		if grid == nil {
			return 0, map[ProviderState]int{}, 0
		}
		list := grid.GetProviderGridPointList()
		states := map[ProviderState]int{}
		for i := 0; i < list.Len(); i += 1 {
			point := list.Get(i)
			states[point.State] += 1
		}
		return list.Len(), states, grid.GetWindowCurrentSize()
	}

	maxLive := 0
	samples := []int{}
	samplePhase := func(name string, duration time.Duration, tick func(i int)) {
		endTime := time.Now().Add(duration)
		i := 0
		for time.Now().Before(endTime) {
			select {
			case <-ctx.Done():
				t.Fatal("context died")
			case <-time.After(5 * time.Second):
			}
			if tick != nil {
				tick(i)
			}
			live, states, windowCurrentSize := sampleGrid()
			samples = append(samples, live)
			if maxLive < live {
				maxLive = live
			}
			fmt.Printf("[rpcleak %s] live=%d states=%v window=%d candidates=%d\n", name, live, states, windowCurrentSize, generator.clientCount.Load())
			i += 1
		}
	}

	// phase A: steady unable-to-connect churn at default timescales
	samplePhase("steady", 150*time.Second, nil)

	// phase B: periodic connect re-sets (same location), like the user re-tapping
	// connect / the app re-applying the location — swaps the grid + monitor
	samplePhase("reset", 90*time.Second, func(i int) {
		if i%4 == 3 {
			fmt.Printf("[rpcleak reset] re-set connect location\n")
			vc.Connect(location)
		}
	})

	live, states, _ := sampleGrid()
	fmt.Printf("[rpcleak] final live=%d states=%v maxLive=%d candidates=%d\n", live, states, maxLive, generator.clientCount.Load())

	// the same bound as the connect-level test: a small multiple of the hard max
	// across both windows
	bound := 3 * (12 + 4)
	if maxLive > bound {
		t.Errorf("grid point set grew beyond bound: maxLive=%d > %d", maxLive, bound)
	}
	if len(samples) >= 8 {
		early := 0
		for _, s := range samples[2:6] {
			early = max(early, s)
		}
		last := samples[len(samples)-1]
		if last > 2*max(early, 8) {
			t.Errorf("grid point set is growing: early=%d last=%d samples=%v", early, last, samples)
		}
	}
}

// rpcLeakTestGenerator simulates "unable to connect" against real timescales: an
// unlimited supply of fresh candidates. A third of clients fail creation
// immediately (unreachable), the rest get clients with no transports whose
// evaluation ping times out (default 30s).
type rpcLeakTestGenerator struct {
	clientCount atomic.Int64
}

func (self *rpcLeakTestGenerator) NextDestinations(count int, excludeDestinations []connect.MultiHopId, rankMode string) (map[connect.MultiHopId]connect.DestinationStats, error) {
	next := map[connect.MultiHopId]connect.DestinationStats{}
	for range count {
		next[connect.RequireMultiHopId(connect.NewId())] = connect.DestinationStats{
			EstimatedBytesPerSecond: connect.ByteCount(0),
			Tier:                    0,
		}
	}
	return next, nil
}

func (self *rpcLeakTestGenerator) NewClientArgs() (*connect.MultiClientGeneratorClientArgs, error) {
	return &connect.MultiClientGeneratorClientArgs{
		ClientId:   connect.NewId(),
		ClientAuth: nil,
	}, nil
}

func (self *rpcLeakTestGenerator) RemoveClientArgs(args *connect.MultiClientGeneratorClientArgs) {
}

func (self *rpcLeakTestGenerator) RemoveClientWithArgs(client *connect.Client, args *connect.MultiClientGeneratorClientArgs) {
}

func (self *rpcLeakTestGenerator) NewClientSettings() *connect.ClientSettings {
	settings := connect.DefaultClientSettings()
	settings.SendBufferSettings.SequenceBufferSize = 0
	settings.SendBufferSettings.AckBufferSize = 0
	settings.ReceiveBufferSettings.SequenceBufferSize = 0
	settings.ForwardBufferSettings.SequenceBufferSize = 0
	return settings
}

func (self *rpcLeakTestGenerator) NewClient(ctx context.Context, args *connect.MultiClientGeneratorClientArgs, clientSettings *connect.ClientSettings) (*connect.Client, error) {
	n := self.clientCount.Add(1)
	if n%3 == 0 {
		// unreachable: client creation fails immediately
		return nil, fmt.Errorf("test client unreachable")
	}
	// reachable to create, but no transports: the evaluation ping goes nowhere
	client := connect.NewClient(ctx, args.ClientId, connect.NewNoContractClientOob(), clientSettings)
	return client, nil
}

func (self *rpcLeakTestGenerator) FixedDestinationSize() (int, bool) {
	return 0, false
}
