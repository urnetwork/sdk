package sdk

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The client settings every window client of this device is built from, read
// through the real destination factory.
func testingWindowClientSettings(t *testing.T, settings *DeviceLocalSettings) *connect.ClientSettings {
	t.Helper()
	settings.AllowProvider = false
	settings.DisableLogging = true
	device := newSettingsOwnershipDevice(t, testingAuthClientShapeSpace(t), settings, false)
	device.SetTransportSettings(&TransportSettings{Mode: TransportModeH1})
	upgrade := connect.DefaultUpgradeMuxSettings()
	upgrade.Dns = nil
	device.SetUpgradeMuxSettings(upgrade)
	device.SetConnectLocation(&ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}})
	device.stateLock.Lock()
	generator := device.apiMultiClientGenerator
	device.stateLock.Unlock()
	if generator == nil {
		t.Fatal("the real destination factory built no generator")
	}
	return generator.NewClientSettings()
}

// Root cause: a discovery window builds a new client with a new key for every
// candidate, and each key it published became a permanent signed registration,
// whether or not the candidate ever got a contract. A hosted window client has
// no reader of its key: encryption is off, so it signs no certificate or
// identity proof, and it has no close-report domain, so it signs no original
// close report. A host that opts in keeps every window client's key local, and
// connect still gives the client a key manager.
func TestWindowClientsKeepTheirKeysLocalWhenTheHostOptsIn(t *testing.T) {
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.HostedIncompatible = true
	settings.KeepWindowClientKeysLocal = true
	client := testingWindowClientSettings(t, settings)
	if !client.SkipClientKeyPublication {
		t.Error("a window client of a device that opted in publishes a key nothing reads")
	}
	// the skip is valid only for a key with no reader
	if client.EncryptionSettings.Mode != connect.EncryptionModeOff || client.ClientKeyRegistrationRequired ||
		client.ContractManagerSettings.CloseReportDomainHash != ([32]byte{}) {
		t.Error("a window client that skips publication has a reader of its key")
	}
	// a connect that refused these settings would leave the client without a
	// key manager
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	built := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), client)
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := built.CloseAndWait(closeCtx); err != nil {
			t.Error(err)
		}
	}()
	if built.ClientKeyManager() == nil {
		t.Fatal("connect built a window client without a key manager")
	}
}

// Only a host that opts in keeps its window clients' keys local. Apps and every
// other host keep publishing, and a default setting never opts a host in.
func TestWindowClientsPublishTheirKeysWithoutTheOptIn(t *testing.T) {
	for name, settings := range map[string]*DeviceLocalSettings{
		"default":    DefaultDeviceLocalSettings(),
		"low memory": DefaultLowMemoryDeviceLocalSettings(),
		"android":    defaultDeviceLocalSettingsForPlatform("android"),
		"ios":        defaultDeviceLocalSettingsForPlatform("ios"),
		"windows":    defaultDeviceLocalSettingsForPlatform("windows"),
		"linux":      defaultDeviceLocalSettingsForPlatform("linux"),
		"darwin":     defaultDeviceLocalSettingsForPlatform("darwin"),
	} {
		if settings.KeepWindowClientKeysLocal {
			t.Errorf("%s: the default settings keep window client keys local", name)
		}
		if testingWindowClientSettings(t, settings).SkipClientKeyPublication {
			t.Errorf("%s: a window client of a device that did not opt in keeps its key local", name)
		}
	}
}

// The ClientKey control one routed transfer frame carries, if any. Returns the
// frame's pooled bytes.
func testingRoutedClientKey(t *testing.T, transferFrameBytes []byte) *protocol.ClientKey {
	t.Helper()
	if transferFrameBytes == nil {
		return nil
	}
	defer connect.MessagePoolReturn(transferFrameBytes)
	var transferFrame protocol.TransferFrame
	if err := connect.ProtoUnmarshal(transferFrameBytes, &transferFrame); err != nil {
		t.Fatalf("decode TransferFrame: %v", err)
	}
	pack := transferFrame.Pack
	if pack == nil {
		frame := transferFrame.GetFrame()
		if frame == nil || frame.GetMessageType() != protocol.MessageType_TransferPack {
			return nil
		}
		pack = &protocol.Pack{}
		if err := connect.ProtoUnmarshal(frame.MessageBytes, pack); err != nil {
			t.Fatalf("decode Pack: %v", err)
		}
	}
	for _, frame := range pack.Frames {
		if frame.GetMessageType() != protocol.MessageType_TransferClientKey {
			continue
		}
		message, err := connect.FromFrame(frame)
		if err != nil {
			t.Fatalf("decode ClientKey: %v", err)
		}
		if clientKey, ok := message.(*protocol.ClientKey); ok {
			return clientKey
		}
	}
	return nil
}

// The opt-in reaches window clients only. A provider's key has readers, so a
// device's provider client publishes it whatever the host chose for its windows.
func TestProviderClientPublishesWhenTheHostKeepsWindowKeysLocal(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	route := make(connect.Route, 64)
	t.Cleanup(func() {
		for {
			select {
			case message := <-route:
				if message != nil {
					connect.MessagePoolReturn(message)
				}
			default:
				return
			}
		}
	})
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.KeepWindowClientKeysLocal = true
	device := newSettingsOwnershipDevice(t, testingAuthClientShapeSpace(t), settings, false)
	if device.provider == nil {
		t.Fatal("the device built no provider")
	}
	platformTransport := func() migratablePlatformTransport {
		device.provider.stateLock.Lock()
		defer device.provider.stateLock.Unlock()
		return device.provider.platformTransport
	}()
	platformTransport.Close()
	client := device.provider.Client()
	client.RouteManager().UpdateTransport(connect.NewSendGatewayTransport(), []connect.Route{route})
	// the constructor's publisher sends the key on this route whenever it ran:
	// a control nothing acked is sent again
	publicKey := client.ClientKeyManager().PublicKey()
	for {
		select {
		case message := <-route:
			if clientKey := testingRoutedClientKey(t, message); clientKey != nil && bytes.Equal(clientKey.PublicKey, publicKey) {
				return
			}
		case <-ctx.Done():
			t.Fatal("the provider client of a device that opted in never published its key")
		}
	}
}
