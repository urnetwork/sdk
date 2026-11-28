// The share limits on a real device: the budget pause composes with the
// platform's pause on the provider's contract manager, the sharing speed
// reaches every provider the device builds, the provider's remote bytes feed
// the month, and the limits and status cross the device rpc.
package sdk

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Hands each status the device tells to the test.
type provideShareStatusEvents struct {
	statuses chan *ProvideShareStatus
}

func newProvideShareStatusEvents() *provideShareStatusEvents {
	return &provideShareStatusEvents{
		statuses: make(chan *ProvideShareStatus, 64),
	}
}

func (self *provideShareStatusEvents) ProvideShareStatusChanged(status *ProvideShareStatus) {
	select {
	case self.statuses <- status:
	default:
	}
}

// Waits for a told status that matches, the wait only a liveness bound.
func (self *provideShareStatusEvents) waitFor(t *testing.T, match func(status *ProvideShareStatus) bool) *ProvideShareStatus {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case status := <-self.statuses:
			if match(status) {
				return status
			}
		case <-timeout:
			t.Fatal("the device never told the expected share status")
			return nil
		}
	}
}

func newProvideShareTestDevice(t *testing.T) *DeviceLocal {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settings := DefaultDeviceLocalSettings()
	settings.Verbose = false
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(device.Close)
	return device
}

// The budget pauses the provider without touching the platform's pause, and
// the provider resumes only once neither holds.
func TestDeviceLocalProvideShareBudgetComposesWithThePlatformPause(t *testing.T) {
	device := newProvideShareTestDevice(t)
	contractManager := device.providerClientSnapshot().ContractManager()
	events := newProvideShareStatusEvents()
	sub := device.AddProvideShareStatusChangeListener(events)
	defer sub.Close()

	device.SetProvideShareLimits(&ProvideShareLimits{MonthlyByteCount: 1000})
	if limits := device.GetProvideShareLimits(); limits.MonthlyByteCount != 1000 || limits.MaxBytesPerSecond != 0 {
		t.Fatalf("limits = %+v", limits)
	}
	// the data the provider carries for others is its remote bytes, both
	// directions; local and blocked bytes are not shared
	device.updateProviderPacketStats(&connect.PacketStats{
		RemoteEgressByteCount:  600,
		RemoteIngressByteCount: 400,
		LocalEgressByteCount:   5000,
		BlockEgressByteCount:   5000,
	})
	status := events.waitFor(t, func(status *ProvideShareStatus) bool { return status.BudgetReached })
	if status.ByteCount != 1000 {
		t.Fatalf("month's data = %d, want the 1000 remote bytes", status.ByteCount)
	}
	if !contractManager.IsProvidePaused() {
		t.Fatal("the budget did not pause the provider")
	}
	if device.GetProvidePaused() {
		t.Fatal("the budget changed the platform's pause")
	}

	device.SetProvidePaused(true)
	if !device.GetProvidePaused() || !contractManager.IsProvidePaused() {
		t.Fatal("the platform's pause did not hold")
	}
	device.SetProvideShareLimits(&ProvideShareLimits{})
	events.waitFor(t, func(status *ProvideShareStatus) bool { return !status.BudgetReached })
	if !contractManager.IsProvidePaused() {
		t.Fatal("lifting the budget lifted the platform's pause")
	}
	device.SetProvidePaused(false)
	if device.GetProvidePaused() || contractManager.IsProvidePaused() {
		t.Fatal("the provider stayed paused with neither pause holding")
	}
}

// Every provider the device builds paces its returns with the device's
// limiter, whose rate the limits set.
func TestDeviceLocalProvideShareLimiterReachesTheProvider(t *testing.T) {
	device := newProvideShareTestDevice(t)
	device.SetProvideShareLimits(&ProvideShareLimits{MaxBytesPerSecond: 250000})
	limiters := make(chan *connect.ProviderShareLimiter, 1)
	device.newRemoteUserNatProviderForTest = func(client *connect.Client, nat *connect.LocalUserNat, settings *connect.RemoteUserNatProviderSettings) *connect.RemoteUserNatProvider {
		limiters <- settings.ShareLimiter
		return connect.NewRemoteUserNatProvider(client, nat, settings)
	}
	device.SetProvideMode(ProvideModePublic)
	select {
	case limiter := <-limiters:
		if limiter == nil || limiter != device.provideShare.limiter || limiter.ByteRate() != 250000 {
			t.Fatalf("provider limiter = %p at %d B/s, want the device's at 250000", limiter, limiter.ByteRate())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("providing built no provider")
	}
	device.SetProvideShareLimits(&ProvideShareLimits{MaxBytesPerSecond: 500000})
	if device.provideShare.limiter.ByteRate() != 500000 {
		t.Fatalf("running provider's rate = %d, want 500000", device.provideShare.limiter.ByteRate())
	}
}

// The remote sets the limits on the device and reads them back, and the
// device's status reaches the remote's listener.
func TestDeviceRemoteProvideShareLimitsAndStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}
	clientId := connect.NewId()
	instanceId := NewId()
	rpcSettings := defaultDeviceRpcSettings()
	deviceLocal, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", instanceId, testDeviceLocalSettingsRpc(), clientId)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceLocal.Close()
	deviceRemote, err := newDeviceRemoteWithOverrides(networkSpace, byJwt, instanceId, rpcSettings, clientId, testing_deviceRpcDialer(rpcSettings))
	if err != nil {
		t.Fatal(err)
	}
	defer deviceRemote.Close()
	deviceRemote.Sync()
	if !deviceRemote.waitForSync(10 * time.Second) {
		t.Fatal("device remote did not complete its initial sync")
	}
	events := newProvideShareStatusEvents()
	sub := deviceRemote.AddProvideShareStatusChangeListener(events)
	defer sub.Close()

	deviceRemote.SetProvideShareLimits(&ProvideShareLimits{MaxBytesPerSecond: 125000, MonthlyByteCount: 3000})
	if limits := deviceLocal.GetProvideShareLimits(); *limits != (ProvideShareLimits{MaxBytesPerSecond: 125000, MonthlyByteCount: 3000}) {
		t.Fatalf("device limits = %+v", limits)
	}
	if limits := deviceRemote.GetProvideShareLimits(); *limits != (ProvideShareLimits{MaxBytesPerSecond: 125000, MonthlyByteCount: 3000}) {
		t.Fatalf("remote limits = %+v", limits)
	}
	deviceLocal.updateProviderPacketStats(&connect.PacketStats{RemoteEgressByteCount: 3000})
	status := events.waitFor(t, func(status *ProvideShareStatus) bool { return status.BudgetReached })
	if status.ByteCount != 3000 || status.MonthlyByteCount != 3000 || status.MaxBytesPerSecond != 125000 {
		t.Fatalf("remote status = %+v", status)
	}
	if read := deviceRemote.GetProvideShareStatus(); !read.BudgetReached || read.ByteCount != 3000 {
		t.Fatalf("remote status read = %+v", read)
	}
}
