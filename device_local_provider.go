package sdk

import (
	"context"
	"fmt"

	// "github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
)

type deviceLocalProvider struct {
	ctx context.Context
	// this is the client for provide
	client            *connect.Client
	platformTransport *connect.PlatformTransport
	localUserNat      *connect.LocalUserNat

	appVersion string
	instanceId connect.Id
}

func newDeviceLocalProviderWithOverrides(
	ctx context.Context,
	networkSpace *NetworkSpace,
	byJwt string,
	appVersion string,
	instanceId connect.Id,
	settings *connect.ClientSettings,
	clientId connect.Id,
) *deviceLocalProvider {
	apiUrl := networkSpace.apiUrl
	clientStrategy := networkSpace.clientStrategy

	clientOob := connect.NewApiOutOfBandControl(ctx, clientStrategy, byJwt, apiUrl)

	clientSettings := newDeviceClientSettings(settings, apiUrl, clientStrategy)

	client := connect.NewClient(
		ctx,
		clientId,
		clientOob,
		clientSettings,
	)

	auth := &connect.ClientAuth{
		ByJwt:      byJwt,
		InstanceId: instanceId,
		AppVersion: appVersion,
	}
	platformTransportSettings := connect.DefaultPlatformTransportSettings()
	platformTransportSettings.Log = clientSettings.Log
	platformTransport := connect.NewPlatformTransport(
		client.Ctx(),
		clientStrategy,
		client.RouteManager(),
		networkSpace.platformUrl,
		auth,
		platformTransportSettings,
	)

	localUserNatSettings := connect.DefaultLocalUserNatSettings()
	// no ulimit for local traffic
	localUserNatSettings.UdpBufferSettings.UserLimit = 0
	localUserNatSettings.TcpBufferSettings.UserLimit = 0
	localUserNatSettings.Log = clientSettings.Log
	localUserNat := connect.NewLocalUserNat(client.Ctx(), clientId.String(), localUserNatSettings)

	return &deviceLocalProvider{
		ctx:               ctx,
		client:            client,
		platformTransport: platformTransport,
		localUserNat:      localUserNat,

		appVersion: appVersion,
		instanceId: instanceId,
	}
}

func (self *deviceLocalProvider) Client() *connect.Client {
	return self.client
}

func (self *deviceLocalProvider) LocalUserNat() *connect.LocalUserNat {
	return self.localUserNat
}

func (self *deviceLocalProvider) SetByJwt(byJwt string) {
	auth := &connect.ClientAuth{
		ByJwt:      byJwt,
		InstanceId: self.instanceId,
		AppVersion: self.appVersion,
	}
	self.platformTransport.SetAuth(auth)
}

func (self *deviceLocalProvider) Close() {
	self.client.Close()
	self.platformTransport.Close()
	self.localUserNat.Close()
}

func newDeviceClientSettings(
	settings *connect.ClientSettings,
	apiUrl string,
	clientStrategy *connect.ClientStrategy,
) *connect.ClientSettings {
	// Shallow-copy settings (and nested EncryptionSettings) so that
	// filling in defaults never mutates the caller's struct.
	var clientSettings connect.ClientSettings
	if settings != nil {
		clientSettings = *settings
	} else {
		clientSettings = *connect.DefaultClientSettings()
	}
	if clientSettings.EncryptionSettings != nil {
		encryptionSettings := *clientSettings.EncryptionSettings
		clientSettings.EncryptionSettings = &encryptionSettings
	}

	// Install the default out-of-band peer-key cross-check when none
	// is configured. Callers who want to disable the check can set a
	// no-op NewPeerClientPublicKeyFetcher in their settings.
	if clientSettings.EncryptionSettings != nil &&
		clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher == nil {
		clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher = func(peerId connect.Id) func(context.Context) ([]byte, error) {
			url := fmt.Sprintf("%s/key/%s", apiUrl, peerId)
			return func(fetchCtx context.Context) ([]byte, error) {
				r, err := connect.HttpGetWithStrategy(
					fetchCtx,
					clientStrategy,
					url,
					"",
					&connect.GetClientKeyResult{},
					connect.NewNoopApiCallback[*connect.GetClientKeyResult](),
				)
				if err != nil {
					return nil, err
				}
				return r.PublicKey, nil
			}
		}
	}

	return &clientSettings
}
