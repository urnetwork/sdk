// A hosted low-profile device must recreate the phone's complete final window
// policy without changing the host's process budget, including on reconnect.
package sdk

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Capture the actual phone constructor only before workers. The expected
// final policy then uses the same SDK transforms and the device's real owner
// pointers, never a table of manually reproduced limits.
func testDeviceLocalWindowProfileIndependentOfHost(t *testing.T, target ByteCount, hosted bool) {
	t.Helper()
	if connect.MemoryBudget() != 0 {
		t.Fatal("window profile control requires process0")
	}
	var settings *DeviceLocalSettings
	var expected *connect.ClientSettings
	func() {
		connect.SetMemoryBudget(target)
		defer connect.SetMemoryBudget(0)
		settings = DefaultLowMemoryDeviceLocalSettings()
		if target == 64*1024*1024 {
			settings = defaultDeviceLocalSettingsForPlatform("android")
		}
		settings.MemoryTargetByteCount = target
		expected = connect.DefaultClientSettingsWithBufferSize(settings.SequenceBufferSize)
	}()
	settings.HostedIncompatible = hosted
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.ProvideExtenderEnabled, settings.EnableRpc = false, false
	settings.ProviderDiscovery = &heldH3PolicyDiscovery{}
	device := newSettingsOwnershipDevice(t, testingAuthClientShapeSpace(t), settings, false)
	device.SetTransportSettings(&TransportSettings{Mode: TransportModeH1})
	upgrade := connect.DefaultUpgradeMuxSettings()
	upgrade.Dns = nil
	device.SetUpgradeMuxSettings(upgrade)
	expected.SendBufferSettings.ResendQueueBudget = device.settings.SendBufferSettings.ResendQueueBudget
	expected.ReceiveBufferSettings.ReceiveQueueBudget = device.settings.ReceiveBufferSettings.ReceiveQueueBudget
	expected.ReceiveBufferSettings.ReceiveHeadRecoveryBudget = device.settings.ReceiveBufferSettings.ReceiveHeadRecoveryBudget
	expected.ReceiveBufferSettings.PackQueueBudget = device.settings.ReceiveBufferSettings.PackQueueBudget
	applyMobileLowMemoryClientSettingsForPlatform(expected, target, true)
	applyMobileH1PerformanceClientSettingsForPlatform(expected, target, true, true)
	// Independent candidate expectations supplement the unchanged generic
	// transforms; the new product projection is never called on the oracle.
	wantPhysical, wantWire, wantRoot, wantH1 := ByteCount(7633633), ByteCount(2181038), ByteCount(21810380), 256
	if target == 64*1024*1024 {
		wantPhysical, wantWire, wantRoot, wantH1 = 15267266, 4362076, 43620760, 512
	}
	expected.ReceiveBufferSettings.H1SequenceBufferSize = wantH1
	expected.ReceiveBufferSettings.AdvertisedReceiveWindowBudgetScaleNumerator = 2
	expected.ReceiveBufferSettings.AdvertisedReceiveWindowBudgetScaleDenominator = 7
	expected.ReceiveBufferSettings.AdvertisedReceiveWindowMaxByteCount = 0
	if device.settings.ReceiveBufferSettings.ReceiveQueueBudget.TotalByteCount() != wantPhysical ||
		device.settings.ReceiveBufferSettings.ReceiveHeadRecoveryBudget.TotalByteCount() != wantWire ||
		device.settings.ReceiveBufferSettings.PackQueueBudget.TotalByteCount() != wantWire || device.transferMemory.root.TotalByteCount() != wantRoot {
		t.Fatal("actual constructor changed the independent physical/wire/root contract")
	}
	receiveSize, peerBudget := deviceLocalDestinationWebRtcSettings(device.settings.WebRtcSettings, false)
	applyDeviceLocalDestinationWebRtcSettings(expected.WebRtcSettings, false, nil, receiveSize, peerBudget)
	expected.WebRtcSettings.UseEgressOnlyIceInterfaces = device.settings.WebRtcSettings.UseEgressOnlyIceInterfaces
	root := device.transferMemory.root
	var previous *connect.ApiMultiClientGenerator
	var differences []string
	for generation := range 2 {
		device.SetConnectLocation(&ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}})
		device.stateLock.Lock()
		generator := device.apiMultiClientGenerator
		device.stateLock.Unlock()
		if generator == nil || generator == previous {
			t.Fatal("real destination factory did not create a fresh generation")
		}
		actual := generator.NewClientSettings()
		if actual.ReceiveBufferSettings.ReceiveQueueMaxByteCount != expected.ReceiveBufferSettings.ReceiveQueueMaxByteCount {
			differences = append(differences, fmt.Sprintf("generation=%d final_receive_max=%d expected=%d root=%d", generation,
				actual.ReceiveBufferSettings.ReceiveQueueMaxByteCount, expected.ReceiveBufferSettings.ReceiveQueueMaxByteCount, root.TotalByteCount()))
		}
		actual.WebRtcSettings.Log, expected.WebRtcSettings.Log = nil, nil
		for _, pair := range []struct {
			name             string
			actual, expected any
		}{
			{"Send", actual.SendBufferSettings, expected.SendBufferSettings},
			{"Receive", actual.ReceiveBufferSettings, expected.ReceiveBufferSettings},
			{"Forward", actual.ForwardBufferSettings, expected.ForwardBufferSettings},
			{"Contracts", actual.ContractManagerSettings, expected.ContractManagerSettings},
			{"P2p", actual.WebRtcSettings, expected.WebRtcSettings},
			{"client channels", [2]int{actual.SendBufferSize, actual.ForwardBufferSize}, [2]int{expected.SendBufferSize, expected.ForwardBufferSize}},
		} {
			if !reflect.DeepEqual(pair.actual, pair.expected) {
				differences = append(differences, fmt.Sprintf("generation=%d complete_%s_policy_differs", generation, pair.name))
			}
		}
		if actual.SendBufferSettings.ResendQueueBudget != device.settings.SendBufferSettings.ResendQueueBudget ||
			actual.ReceiveBufferSettings.ReceiveQueueBudget.Parent() != device.transferMemory.client ||
			actual.ReceiveBufferSettings.PackQueueBudget != device.settings.ReceiveBufferSettings.PackQueueBudget ||
			device.transferMemory.root != root || connect.MemoryBudget() != 0 {
			t.Fatal("window construction changed the host target or stable device owners")
		}
		previous = generator
		device.SetConnectLocation(nil)
	}
	joinCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := device.CloseAndWait(joinCtx); err != nil {
		t.Fatal(err)
	}
	stats := root.Stats()
	if stats.UsedByteCount != 0 || stats.QuotaByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
		t.Fatal("window factory/reconnect left a physical owner or quota after join")
	}
	if len(differences) != 0 {
		t.Errorf("PRODUCTION_WINDOW_POLICY_DRIFT target=%d %s", target, strings.Join(differences, "; "))
	}
}

// This is the host/server/proxy low profile under an unscaled host process.
func TestDeviceLocalHosted32WindowFactoryMatchesPhonePolicy(t *testing.T) {
	testDeviceLocalWindowProfileIndependentOfHost(t, 32*1024*1024, true)
}

// The Android host surrogate retains the app-selected explicit 64-MiB policy.
func TestDeviceLocalAndroid64WindowFactoryMatchesPhonePolicy(t *testing.T) {
	testDeviceLocalWindowProfileIndependentOfHost(t, 64*1024*1024, false)
}
