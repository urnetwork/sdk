package sdk

import (
	"context"
	"runtime"
	"testing"

	"github.com/urnetwork/connect"
)

// A remote can call the device from the moment its rpc manager starts. On ios
// the app's DeviceRemote redials the extension's rpc listener until it
// answers, and its first sync applies the settings the app holds (the
// transport settings, the provide mode), so that call can land while the
// constructor is still building the device. What the constructor sets up
// without stateLock has to be done by then.

// testingUnorderedSignal orders two goroutines in time without ordering them
// for the race detector. Its accesses are hidden from the detector
// (go:norace), so the wait adds no happens-before edge. A channel, a mutex or
// an atomic would add one and hide the race the test makes, and a failed
// TryLock is no substitute: under the detector a failed compare-and-swap
// acquires. Only for a test that makes a race on purpose.
type testingUnorderedSignal struct {
	done bool
}

//go:norace
func (self *testingUnorderedSignal) set() {
	self.done = true
}

//go:norace
func (self *testingUnorderedSignal) wait() {
	for !self.done {
		runtime.Gosched()
	}
}

// The remote's call is the transport settings its sync applies, made on a
// goroutine of its own as an rpc session makes it, the instant the rpc manager
// starts. The constructor goes on once the call has applied, through a signal
// the race detector does not see, so the detector sees the call and the rest
// of the constructor as concurrent, which they are in the app. The device is a
// phone's, so it has the packet pressure gate the transport settings steer.
// Under -race this fails while the constructor still sets up the gate after
// the rpc manager starts.
func TestDeviceLocalRpcCallAtRpcStartDoesNotRaceConstruction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}

	applied := &testingUnorderedSignal{}
	callDone := make(chan error, 1)
	settings := testDeviceLocalSettingsRpc()
	settings.DisableLogging = true
	settings.AllowProvider = false
	settings.testingAfterRpcStart = func(deviceLocal *DeviceLocal) {
		go func() {
			rpc := &DeviceLocalRpc{deviceLocal: deviceLocal, settings: &deviceRpcSettings{}}
			err := rpc.SetTransportSettings(
				&DeviceRemoteTransportSettingsRpc{
					TransportSettings: newTransportSettingsRpc(&TransportSettings{Mode: TransportModeH1}, false),
				},
				nil,
			)
			applied.set()
			callDone <- err
		}()
		applied.wait()
	}
	deviceLocal, err := newDeviceLocalWithOverridesForPlatform(
		networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId(), true,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceLocal.Close()
	if err := <-callDone; err != nil {
		t.Fatalf("set transport settings: %v", err)
	}

	connect.AssertEqual(t, deviceLocal.GetTransportSettings().Mode, TransportModeH1)
	// an explicit h1 client with providing off spends the ack reserve
	if pressure := deviceLocal.mobilePacketPressure; pressure == nil || !pressure.h1AckReserveEnabled.Load() {
		t.Fatal("the h1 ack reserve does not follow the transport settings the remote set")
	}
}

// A provide mode the remote's sync sets the instant the rpc manager starts
// keeps the memory shares providing takes. The constructor's initial
// allocation is the provide-off one. Made after the rpc manager starts, it
// overwrote the shares of a provide mode the remote had already set, so the
// device provided with the provider pair at its idle floor and the provider
// share still backing the client pair. The test reads only the state the call
// leaves, so the constructor waits for the call in the ordinary way.
func TestDeviceLocalRpcProvideModeAtRpcStartKeepsProvideShares(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var callErr error
	settings := testDeviceLocalSettingsRpc()
	settings.DisableLogging = true
	settings.testingAfterRpcStart = func(deviceLocal *DeviceLocal) {
		callDone := make(chan error, 1)
		go func() {
			rpc := &DeviceLocalRpc{deviceLocal: deviceLocal, settings: &deviceRpcSettings{}}
			callDone <- rpc.SetProvideMode(ProvideModeNetwork, nil)
		}()
		callErr = <-callDone
	}
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceLocal.Close()
	if callErr != nil {
		t.Fatalf("set provide mode: %v", callErr)
	}
	connect.AssertEqual(t, deviceLocal.GetProvideMode(), ProvideModeNetwork)

	// what SetProvideMode leaves on a built device: the provider share moves
	// from the client pair to the provider pair (half; the other half sizes
	// the egress nat)
	_, clientShare, _, providerShare := deviceMemoryShares(deviceLocal.settings)
	clientResend := deviceLocal.settings.ClientSettings.SendBufferSettings.ResendQueueBudget
	clientReceive := deviceLocal.settings.ClientSettings.ReceiveBufferSettings.ReceiveQueueBudget
	providerResend, providerReceive := deviceLocal.provider.transferBudgets()
	connect.AssertEqual(t, clientResend.TotalByteCount(), clientShare*3/7)
	connect.AssertEqual(t, clientReceive.TotalByteCount(), clientShare*4/7)
	connect.AssertEqual(t, providerResend.TotalByteCount(), providerShare/2*3/7)
	connect.AssertEqual(t, providerReceive.TotalByteCount(), providerShare/2*4/7)
}
