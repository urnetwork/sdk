// Actual key-manager Http publication is joined with the existing carrier
// readiness test fixture. No device lock spans either operation.
package sdk

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Forces a provider replacement exactly during the external carrier read.
type registrationSwitchTransport struct {
	migratablePlatformTransport
	beforeConnected func()
}

// The carrier result still belongs to this original provider generation.
func (self *registrationSwitchTransport) IsConnected() bool {
	self.beforeConnected()
	return self.migratablePlatformTransport.IsConnected()
}

// A connected carrier alone cannot release the production provider readiness
// barrier. Rotation invalidates the same predicate before the next response.
func TestDeviceLocalProviderRegistrationGatesCarrierReadiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	entered, respond := make(chan struct{}, 2), make(chan struct{}, 2)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hello" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/connect/control" {
			http.NotFound(w, r)
			return
		}
		select {
		case entered <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		select {
		case <-respond:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pack":"","error":null}`))
	}))
	strategy := connect.NewClientStrategyWithDefaults(ctx)
	control := connect.NewApiOutOfBandControl(ctx, strategy, "synthetic-provider-token", endpoint.URL)
	settings := connect.DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.EncryptionSettings.Mode = connect.EncryptionModeOff
	settings.ClientKeyRegistrationRequired = true
	settings.ClientKeySeed = bytes.Repeat([]byte{31}, ed25519.SeedSize)
	// This is the actual settings copier used by newDeviceLocalProvider.
	clientSettings := newDeviceClientSettings(settings, endpoint.URL, strategy)
	if !clientSettings.ClientKeyRegistrationRequired || !settings.ClientKeyRegistrationRequired {
		t.Fatal("provider settings dropped registration admission")
	}
	client := connect.NewClient(ctx, connect.NewId(), control, clientSettings)
	t.Cleanup(func() {
		cancel()
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer joinCancel()
		if err := client.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
		if err := control.CloseAndWait(joinCtx); err != nil {
			t.Error(err)
		}
		strategy.Close()
		endpoint.Close()
	})
	transport := newFakeMigratablePlatformTransport(&connect.ClientAuth{ByJwt: "synthetic-provider", InstanceId: connect.NewId(), AppVersion: "0.0.0"}, true)
	provider := &deviceLocalProvider{client: client, platformTransport: transport}
	device := &DeviceLocal{provider: provider}
	for index := 0; index < 2; index++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if !device.GetProviderConnected() || device.GetProviderClientKeyRegistered() || device.GetProviderReady() {
			t.Fatal("carrier delivery released processed readiness", index)
		}
		respond <- struct{}{}
		if err := client.ClientKeyManager().WaitForRegistration(ctx); err != nil {
			t.Fatal(err)
		}
		if !device.GetProviderClientKeyRegistered() || !device.GetProviderReady() {
			t.Fatal("processed current key did not release live carrier readiness", index)
		}
		if index == 0 {
			if err := client.ClientKeyManager().SetSeed(bytes.Repeat([]byte{32}, ed25519.SeedSize)); err != nil {
				t.Fatal(err)
			}
			if device.GetProviderReady() {
				t.Fatal("rotation retained earlier readiness")
			}
		}
	}
	// Do not combine one provider's connected carrier with another provider's
	// registered key if device ownership changes between the external reads.
	unregistered := &deviceLocalProvider{platformTransport: &registrationSwitchTransport{
		migratablePlatformTransport: transport,
		beforeConnected:             func() { device.stateLock.Lock(); device.provider = provider; device.stateLock.Unlock() },
	}}
	device.stateLock.Lock()
	device.provider = unregistered
	device.stateLock.Unlock()
	if device.GetProviderReady() {
		t.Fatal("readiness combined different provider generations")
	}
	provider.stateLock.Lock()
	provider.closed = true
	provider.stateLock.Unlock()
	if device.GetProviderReady() {
		t.Fatal("closed provider retained readiness")
	}
	device.stateLock.Lock()
	device.closed = true
	device.stateLock.Unlock()
	if device.GetProviderClientKeyRegistered() || device.GetProviderReady() {
		t.Fatal("closed device retained registration readiness")
	}
}

// An existing custom/legacy client is not silently upgraded by a connected
// carrier. Missing and closed devices likewise cannot report registration.
func TestDeviceLocalProviderRegistrationRejectsLegacyCarrier(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	settings := connect.DefaultClientSettings()
	settings.ControlPingTimeout = 0
	settings.EncryptionSettings.Mode = connect.EncryptionModeOff
	client := connect.NewClient(ctx, connect.NewId(), connect.NewNoContractClientOob(), settings)
	defer func() {
		if err := client.CloseAndWait(ctx); err != nil {
			t.Error(err)
		}
	}()
	transport := newFakeMigratablePlatformTransport(&connect.ClientAuth{ByJwt: "synthetic-legacy", InstanceId: connect.NewId(), AppVersion: "0.0.0"}, true)
	device := &DeviceLocal{provider: &deviceLocalProvider{client: client, platformTransport: transport}}
	if !device.GetProviderConnected() || device.GetProviderClientKeyRegistered() || device.GetProviderReady() {
		t.Fatal("legacy delivery-only carrier became registered")
	}
	absent := &DeviceLocal{}
	if absent.GetProviderClientKeyRegistered() || absent.GetProviderReady() {
		t.Fatal("absent provider became ready")
	}
}
