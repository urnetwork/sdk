//go:build !ios

package sdk

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Match Connect's optional setup capability: embedding the API generator must
// not let its promoted method bypass this fixture's ownership/peer hooks.
type h1OwnerContextGenerator interface {
	NewClientContext(context.Context, context.Context, *connect.MultiClientGeneratorClientArgs, *connect.ClientSettings) (*connect.Client, error)
}

func TestH1OwnerGeneratorConstructorHooks(t *testing.T) {
	for _, mode := range []string{"legacy", "setup-context", "canceled-setup"} {
		t.Run(mode, func(t *testing.T) {
			f := newH1OwnerFixture(t)
			apiURL := f.server.URL
			if mode == "canceled-setup" {
				// Hold processed registration so cancellation cannot race a
				// successful Provide response in the constructor's select.
				endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/connect/control" {
						<-r.Context().Done()
						return
					}
					f.server.Config.Handler.ServeHTTP(w, r)
				}))
				defer endpoint.Close()
				apiURL = endpoint.URL
			}
			strategy := connect.NewClientStrategyWithDefaults(f.ctx)
			defer strategy.Close()
			settings := connect.DefaultApiMultiClientGeneratorSettings()
			settings.PlatformTransportMode = connect.TransportModeH1
			settings.PlatformTransportSettingsGenerator = func() *connect.PlatformTransportSettings {
				transport := connect.DefaultPlatformTransportSettings()
				transport.V2H1Auth = true
				return transport
			}
			clientSettings := connect.DefaultClientSettings()
			clientSettings.Log = connect.NewNoopLogger()
			clientSettings.EncryptionSettings.Mode = connect.EncryptionModeOff
			generator := &h1OwnerGenerator{providers: f.providers}
			generator.ApiMultiClientGenerator = connect.NewApiMultiClientGenerator(f.ctx, nil, strategy, nil,
				apiURL, "synthetic-fixture-token", f.space.platformUrl, "test", "test", "0", nil,
				func() *connect.ClientSettings { return clientSettings }, settings)
			defer func() {
				join, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := generator.CloseAndWait(join); err != nil {
					t.Error(err)
				}
			}()
			args, err := generator.NewClientArgs()
			if err != nil {
				t.Fatal(err)
			}
			setupCtx, cancelSetup := context.WithCancel(f.ctx)
			defer cancelSetup()
			if mode == "canceled-setup" {
				cancelSetup()
			}
			var client *connect.Client
			if mode == "legacy" {
				client, err = generator.NewClient(f.ctx, args, clientSettings)
			} else {
				contextGenerator, ok := any(generator).(h1OwnerContextGenerator)
				if !ok {
					t.Fatal("fixture lost the context-aware setup capability")
				}
				client, err = contextGenerator.NewClientContext(f.ctx, setupCtx, args, clientSettings)
			}
			if client != nil {
				defer func() {
					generator.RemoveClientWithArgs(client, args)
					client.Cancel()
				}()
			}
			if mode == "canceled-setup" {
				if client != nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled setup: client=%t err=%v", client != nil, err)
				}
			} else if err != nil || client == nil {
				t.Fatalf("constructor: client=%t err=%v", client != nil, err)
			}
			generator.mu.Lock()
			count := len(generator.clients)
			if mode != "canceled-setup" && (count != 1 || generator.clients[0] != client) {
				t.Errorf("fixture tracked %d clients; want the returned client exactly once", count)
			}
			generator.mu.Unlock()
			if mode == "canceled-setup" {
				if count != 0 {
					t.Fatalf("failed setup tracked %d clients", count)
				}
				return
			}
			for _, provider := range f.providers {
				manager := client.ContractManager()
				if !manager.SendNoContract(provider.ClientId()) || !manager.ReceiveNoContract(provider.ClientId()) {
					t.Error("constructor bypassed local provider contract exemption")
				}
			}
			// Successful setup owns the client with f.ctx, not setupCtx.
			cancelSetup()
			select {
			case <-client.Done():
				t.Fatal("setup cancellation retired the admitted client")
			default:
			}
		})
	}
}
