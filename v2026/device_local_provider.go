package sdk

import (
	"context"

	// "github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/connect/v2026"
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
	client := connect.NewClient(
		ctx,
		clientId,
		clientOob,
		// connect.DefaultClientSettingsNoNetworkEvents(),
		connect.DefaultClientSettings(),
	)

	auth := &connect.ClientAuth{
		ByJwt:      byJwt,
		InstanceId: instanceId,
		AppVersion: appVersion,
	}
	platformTransport := connect.NewPlatformTransportWithDefaults(
		client.Ctx(),
		clientStrategy,
		client.RouteManager(),
		networkSpace.platformUrl,
		auth,
	)

	localUserNatSettings := connect.DefaultLocalUserNatSettings()
	// no ulimit for local traffic
	localUserNatSettings.UdpBufferSettings.UserLimit = 0
	localUserNatSettings.TcpBufferSettings.UserLimit = 0
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
