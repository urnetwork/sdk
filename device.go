package sdk

import (
	"context"
	"fmt"

	// "net/netip"
	"sync"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/golang/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/protocol"
)

// the device upgrades the api, including setting the client jwt
// closing the device does not close the api

var deviceLog = logFn("device")

type ProvideChangeListener interface {
	ProvideChanged(provideEnabled bool)
}

type ProvidePausedChangeListener interface {
	ProvidePausedChanged(providePaused bool)
}

type OfflineChangeListener interface {
	OfflineChanged(offline bool, vpnInterfaceWhileOffline bool)
}

type ConnectChangeListener interface {
	ConnectChanged(connectEnabled bool)
}

type RouteLocalChangeListener interface {
	RouteLocalChanged(routeLocal bool)
}

type ConnectLocationChangeListener interface {
	ConnectLocationChanged(location *ConnectLocation)
}

type ProvideSecretKeysListener interface {
	ProvideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList)
}

// receive a packet into the local raw socket
type ReceivePacket interface {
	ReceivePacket(packet []byte)
}


type DeviceDestination struct {
	Location *ConnectLocation
	Specs *ProviderSpecList
	ProvideMode ProvideMode
}


// FIXME need to add multi route monitor listener
type Device interface {

	func GetClientId() *Id 

	func GetApi() *BringYourApi 

	func GetNetworkSpace() *NetworkSpace 

	func GetStats() *DeviceStats

	func GetShouldShowRatingDialog() bool 

	func GetCanShowRatingDialog() bool

	func SetCanShowRatingDialog(canShowRatingDialog bool) 

	func GetProvideWhileDisconnected() bool

	func SetProvideWhileDisconnected(provideWhileDisconnected bool)

	func GetCanRefer() bool

	func SetCanRefer(canRefer bool)

	func SetRouteLocal(routeLocal bool) 

	func GetRouteLocal() bool

	func AddProvideChangeListener(listener ProvideChangeListener) Sub 

	func AddProvidePausedChangeListener(listener ProvidePausedChangeListener) Sub 

	func AddOfflineChangeListener(listener OfflineChangeListener) Sub 

	func AddConnectChangeListener(listener ConnectChangeListener) Sub 

	func AddRouteLocalChangeListener(listener RouteLocalChangeListener) Sub

	func AddConnectLocationChangeListener(listener ConnectLocationChangeListener) Sub 

	func AddProvideSecretKeysListener(listener ProvideSecretKeysListener) Sub 

	func LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList)

	func InitProvideSecretKeys()

	func GetProvideEnabled() bool 

	func GetConnectEnabled() bool 

	func SetProvideMode(provideMode ProvideMode) 

	func setProvideModeNoEvent(provideMode ProvideMode) 

	func GetProvideMode() ProvideMode 

	func SetProvidePaused(providePaused bool) 

	func GetProvidePaused() bool 

	func SetOffline(offline bool) 

	func GetOffline() bool 

	func SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool)

	func GetVpnInterfaceWhileOffline() bool

	func RemoveDestination()

	func SetDestination(destination *DeviceDestination)

	func SetConnectLocation(location *ConnectLocation) 

	func GetConnectLocation() *ConnectLocation 

	func Shuffle() 

	func SendPacket(packet []byte, n int32) bool 

	func AddReceivePacket(receivePacket ReceivePacket) Sub 

	func Close()
}



type deviceSettings struct {
	// time to give up (drop) sending a packet to a destination
	SendTimeout time.Duration
	// ClientDrainTimeout time.Duration
}

func defaultDeviceSettings() *deviceSettings {
	return &deviceSettings{
		SendTimeout: 5 * time.Second,
		// ClientDrainTimeout: 30 * time.Second,
	}
}

type DeviceLocal struct {
	networkSpace *NetworkSpace

	ctx    context.Context
	cancel context.CancelFunc

	byJwt string
	// platformUrl string
	// apiUrl      string

	deviceDescription string
	deviceSpec        string
	appVersion        string

	settings *deviceSettings

	clientId   connect.Id
	instanceId connect.Id

	clientStrategy *connect.ClientStrategy
	// this is the client for provide
	client *connect.Client

	// contractManager *connect.ContractManager
	// routeManager *connect.RouteManager

	platformTransport *connect.PlatformTransport

	localUserNat *connect.LocalUserNat

	stats *DeviceStats

	stateLock sync.Mutex

	connectLocation *ConnectLocation

	// when nil, packets get routed to the local user nat
	remoteUserNatClient connect.UserNatClient

	remoteUserNatProviderLocalUserNat *connect.LocalUserNat
	remoteUserNatProvider             *connect.RemoteUserNatProvider

	routeLocal          bool
	canShowRatingDialog bool
	canRefer            bool

	provideWhileDisconnected bool
	offline                  bool
	vpnInterfaceWhileOffline bool

	receiveCallbacks *connect.CallbackList[connect.ReceivePacketFunction]

	provideChangeListeners         *connect.CallbackList[ProvideChangeListener]
	providePausedChangeListeners   *connect.CallbackList[ProvidePausedChangeListener]
	offlineChangeListeners         *connect.CallbackList[OfflineChangeListener]
	connectChangeListeners         *connect.CallbackList[ConnectChangeListener]
	routeLocalChangeListeners      *connect.CallbackList[RouteLocalChangeListener]
	connectLocationChangeListeners *connect.CallbackList[ConnectLocationChangeListener]

	localUserNatUnsub func()
}

// FIXME pass NetworkSpace here instead of API
func NewDeviceLocalWithDefaults(
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
	enableRpc bool,
) (*DeviceLocal, error) {
	return traceWithReturnError(
		func() (*DeviceLocal, error) {
			return newDeviceLocal(
				networkSpace,
				byJwt,
				deviceDescription,
				deviceSpec,
				appVersion,
				instanceId,
				defaultDeviceSettings(),
			)
		},
	)
}

func newDeviceLocal(
	networkSpace *NetworkSpace,
	byJwt string,
	deviceDescription string,
	deviceSpec string,
	appVersion string,
	instanceId *Id,
	enableRpc bool,
	settings *deviceSettings,
) (*DeviceLocal, error) {
	clientId, err := parseByJwtClientId(byJwt)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	// ctx, cancel := api.ctx, api.cancel
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

	// routeManager := connect.NewRouteManager(connectClient)
	// contractManager := connect.NewContractManagerWithDefaults(connectClient)
	// connectClient.Setup(routeManager, contractManager)
	// go connectClient.Run()

	auth := &connect.ClientAuth{
		ByJwt:      byJwt,
		InstanceId: instanceId.toConnectId(),
		AppVersion: Version,
	}
	platformTransport := connect.NewPlatformTransportWithDefaults(
		client.Ctx(),
		clientStrategy,
		client.RouteManager(),
		networkSpace.platformUrl,
		auth,
	)

	// go platformTransport.Run(connectClient.RouteManager())

	localUserNatSettings := connect.DefaultLocalUserNatSettings()
	// no ulimit for local traffic
	localUserNatSettings.UdpBufferSettings.UserLimit = 0
	localUserNatSettings.TcpBufferSettings.UserLimit = 0
	localUserNat := connect.NewLocalUserNat(client.Ctx(), clientId.String(), localUserNatSettings)

	// api := newBringYourApiWithContext(cancelCtx, clientStrategy, apiUrl)
	networkSpace.api.SetByJwt(byJwt)

	byDevice := &DeviceLocal{
		networkSpace: networkSpace,
		ctx:          ctx,
		cancel:       cancel,
		byJwt:        byJwt,
		// apiUrl:            apiUrl,
		deviceDescription: deviceDescription,
		deviceSpec:        deviceSpec,
		appVersion:        appVersion,
		settings:          settings,
		clientId:          clientId,
		instanceId:        instanceId.toConnectId(),
		clientStrategy:    clientStrategy,
		client:            client,
		// contractManager: contractManager,
		// routeManager: routeManager,
		platformTransport:                 platformTransport,
		localUserNat:                      localUserNat,
		stats:                             newDeviceStats(),
		connectLocation:                   nil,
		remoteUserNatClient:               nil,
		remoteUserNatProviderLocalUserNat: nil,
		remoteUserNatProvider:             nil,
		routeLocal:                        true,
		canShowRatingDialog:               true,
		provideWhileDisconnected:          false,
		offline:                           true,
		vpnInterfaceWhileOffline:          false,
		openedViewControllers:             map[ViewController]bool{},
		receiveCallbacks:                  connect.NewCallbackList[connect.ReceivePacketFunction](),
		provideChangeListeners:            connect.NewCallbackList[ProvideChangeListener](),
		providePausedChangeListeners:      connect.NewCallbackList[ProvidePausedChangeListener](),
		offlineChangeListeners:            connect.NewCallbackList[OfflineChangeListener](),
		connectChangeListeners:            connect.NewCallbackList[ConnectChangeListener](),
		routeLocalChangeListeners:         connect.NewCallbackList[RouteLocalChangeListener](),
		connectLocationChangeListeners:    connect.NewCallbackList[ConnectLocationChangeListener](),
	}

	// set up with nil destination
	localUserNatUnsub := localUserNat.AddReceivePacketCallback(byDevice.receive)
	byDevice.localUserNatUnsub = localUserNatUnsub

	if enableRpc {
		self.deviceLocalRpc = newDeviceLocalRpcWithDefaults(ctx, byJwt)
	}

	return byDevice, nil
}

func (self *DeviceLocal) ClientId() *Id {
	return self.GetClientId()
}

func (self *DeviceLocal) GetClientId() *Id {
	// clientId := self.client.ClientId()
	return newId(self.clientId)
}

// func (self *DeviceLocal) client() *connect.Client {
// 	return self.client
// }

func (self *DeviceLocal) GetApi() *BringYourApi {
	return self.networkSpace.GetApi()
}

func (self *DeviceLocal) GetNetworkSpace() *NetworkSpace {
	return self.networkSpace
}

// func (self *DeviceLocal) SetCustomExtenderAutoConfigure(extenderAutoConfigure *ExtenderAutoConfigure) {
// 	// FIXME
// }

// func (self *DeviceLocal) GetCustomExtenderAutoConfigure() *ExtenderAutoConfigure {
// 	// FIXME
// 	return nil
// }

func (self *DeviceLocal) GetStats() *DeviceStats {
	return self.stats
}

func (self *DeviceLocal) GetShouldShowRatingDialog() bool {
	if !self.stats.GetUserSuccess() {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.canShowRatingDialog
}

func (self *DeviceLocal) GetCanShowRatingDialog() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.canShowRatingDialog
}

func (self *DeviceLocal) SetCanShowRatingDialog(canShowRatingDialog bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.canShowRatingDialog = canShowRatingDialog
}

func (self *DeviceLocal) GetProvideWhileDisconnected() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.provideWhileDisconnected
}

func (self *DeviceLocal) SetProvideWhileDisconnected(provideWhileDisconnected bool) {

	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.provideWhileDisconnected != provideWhileDisconnected {
			changed = true
			self.provideWhileDisconnected = provideWhileDisconnected
		}
	}()

	if changed && !self.GetConnectEnabled() {
		if !self.GetProvideWhileDisconnected() {
			self.SetProvideMode(ProvideModeNone)
		} else {
			self.SetProvideMode(ProvideModePublic)
		}
	}

}

func (self *DeviceLocal) GetCanRefer() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.canRefer
}

func (self *DeviceLocal) SetCanRefer(canRefer bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.canRefer = canRefer
}

func (self *DeviceLocal) SetRouteLocal(routeLocal bool) {
	set := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		if self.routeLocal != routeLocal {
			self.routeLocal = routeLocal
			set = true
		}
	}()
	if set {
		self.routeLocalChanged(routeLocal)
	}
}

func (self *DeviceLocal) GetRouteLocal() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.routeLocal
}

// func (self *DeviceLocal) SetCustomExtenderResolver(extenderResolver *ExtenderResolver) {
// 	// FIXME
// }

// func (self *DeviceLocal) CustomExtenderResolver() *ExtenderResolver {
// 	// FIXME
// 	return nil
// }

func (self *DeviceLocal) windowMonitor() *connect.RemoteUserNatMultiClientMonitor {
	switch v := self.remoteUserNatClient.(type) {
	case *connect.RemoteUserNatMultiClient:
		return v.Monitor()
	default:
		return nil
	}
}

func (self *DeviceLocal) AddProvideChangeListener(listener ProvideChangeListener) Sub {
	callbackId := self.provideChangeListeners.Add(listener)
	return newSub(func() {
		self.provideChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddProvidePausedChangeListener(listener ProvidePausedChangeListener) Sub {
	callbackId := self.providePausedChangeListeners.Add(listener)
	return newSub(func() {
		self.providePausedChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddOfflineChangeListener(listener OfflineChangeListener) Sub {
	callbackId := self.offlineChangeListeners.Add(listener)
	return newSub(func() {
		self.offlineChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddConnectChangeListener(listener ConnectChangeListener) Sub {
	callbackId := self.connectChangeListeners.Add(listener)
	return newSub(func() {
		self.connectChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddRouteLocalChangeListener(listener RouteLocalChangeListener) Sub {
	callbackId := self.routeLocalChangeListeners.Add(listener)
	return newSub(func() {
		self.routeLocalChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) AddConnectLocationChangeListener(listener ConnectLocationChangeListener) Sub {
	callbackId := self.connectLocationChangeListeners.Add(listener)
	return newSub(func() {
		self.connectLocationChangeListeners.Remove(callbackId)
	})
}

func (self *DeviceLocal) provideChanged(provideEnabled bool) {
	for _, listener := range self.provideChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ProvideChanged(provideEnabled)
		})
	}
}

func (self *DeviceLocal) providePausedChanged(providePaused bool) {
	for _, listener := range self.providePausedChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ProvidePausedChanged(providePaused)
		})
	}
}

func (self *DeviceLocal) offlineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	for _, listener := range self.offlineChangeListeners.Get() {
		connect.HandleError(func() {
			listener.OfflineChanged(offline, vpnInterfaceWhileOffline)
		})
	}
}

func (self *DeviceLocal) connectChanged(connectEnabled bool) {
	for _, listener := range self.connectChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectChanged(connectEnabled)
		})
	}
}

func (self *DeviceLocal) routeLocalChanged(routeLocal bool) {
	for _, listener := range self.routeLocalChangeListeners.Get() {
		connect.HandleError(func() {
			listener.RouteLocalChanged(routeLocal)
		})
	}
}

func (self *DeviceLocal) connectLocationChanged(location *ConnectLocation) {
	for _, listener := range self.connectLocationChangeListeners.Get() {
		connect.HandleError(func() {
			listener.ConnectLocationChanged(location)
		})
	}
}

// `ReceivePacketFunction`
func (self *DeviceLocal) receive(source connect.TransferPath, ipProtocol connect.IpProtocol, packet []byte) {
	// deviceLog("GOT A PACKET %d", len(packet))
	for _, receiveCallback := range self.receiveCallbacks.Get() {
		receiveCallback(source, ipProtocol, packet)
	}
}

func (self *DeviceLocal) GetProvideSecretKeys() *ProvideSecretKeyList {
	provideSecretKeys := self.client.ContractManager().GetProvideSecretKeys()
	provideSecretKeyList := NewProvideSecretKeyList()
	for provideMode, provideSecretKey := range provideSecretKeys {
		provideSecretKey := &ProvideSecretKey{
			ProvideMode:      ProvideMode(provideMode),
			ProvideSecretKey: string(provideSecretKey),
		}
		provideSecretKeyList.Add(provideSecretKey)
	}
	return provideSecretKeyList
}

func (self *DeviceLocal) LoadProvideSecretKeys(provideSecretKeyList *ProvideSecretKeyList) {
	provideSecretKeys := map[protocol.ProvideMode][]byte{}
	for i := 0; i < provideSecretKeyList.Len(); i += 1 {
		provideSecretKey := provideSecretKeyList.Get(i)
		provideMode := protocol.ProvideMode(provideSecretKey.ProvideMode)
		provideSecretKeys[provideMode] = []byte(provideSecretKey.ProvideSecretKey)
	}
	self.client.ContractManager().LoadProvideSecretKeys(provideSecretKeys)
}

func (self *DeviceLocal) InitProvideSecretKeys() {
	self.client.ContractManager().InitProvideSecretKeys()
}

func (self *DeviceLocal) GetProvideEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.remoteUserNatProvider != nil
}

func (self *DeviceLocal) GetConnectEnabled() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	return self.remoteUserNatClient != nil
}

func (self *DeviceLocal) SetProvideMode(provideMode ProvideMode) {
	self.setProvideModeNoEvent(provideMode)
	self.provideChanged(self.GetProvideEnabled())
}

func (self *DeviceLocal) setProvideModeNoEvent(provideMode ProvideMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	// TODO create a new provider only client?

	provideModes := map[protocol.ProvideMode]bool{}
	if ProvideModePublic <= provideMode {
		provideModes[protocol.ProvideMode_Public] = true
	}
	if ProvideModeFriendsAndFamily <= provideMode {
		provideModes[protocol.ProvideMode_FriendsAndFamily] = true
	}
	if ProvideModeNetwork <= provideMode {
		provideModes[protocol.ProvideMode_Network] = true
	}
	self.client.ContractManager().SetProvideModesWithReturnTraffic(provideModes)

	// recreate the provider user nat
	if self.remoteUserNatProviderLocalUserNat != nil {
		self.remoteUserNatProviderLocalUserNat.Close()
		self.remoteUserNatProviderLocalUserNat = nil
	}
	if self.remoteUserNatProvider != nil {
		self.remoteUserNatProvider.Close()
		self.remoteUserNatProvider = nil
	}

	if ProvideModeNone < provideMode {
		self.remoteUserNatProviderLocalUserNat = connect.NewLocalUserNatWithDefaults(self.client.Ctx(), self.clientId.String())
		self.remoteUserNatProvider = connect.NewRemoteUserNatProviderWithDefaults(self.client, self.remoteUserNatProviderLocalUserNat)
	}
}

func (self *DeviceLocal) GetProvideMode() ProvideMode {
	maxProvideMode := protocol.ProvideMode_None
	for provideMode, _ := range self.client.ContractManager().GetProvideModes() {
		maxProvideMode = max(maxProvideMode, provideMode)
	}
	return ProvideMode(maxProvideMode)
}

func (self *DeviceLocal) SetProvidePaused(providePaused bool) {
	glog.Infof("[device]provide paused = %t\n", providePaused)

	self.client.ContractManager().SetProvidePaused(providePaused)
	self.providePausedChanged(self.GetProvidePaused())
}

func (self *DeviceLocal) GetProvidePaused() bool {
	return self.client.ContractManager().IsProvidePaused()
}

func (self *DeviceLocal) SetOffline(offline bool) {
	glog.Infof("[device]offline = %t\n", offline)

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.offline = offline
	}()
	self.offlineChanged(self.GetOffline(), self.GetVpnInterfaceWhileOffline())
}

func (self *DeviceLocal) GetOffline() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.offline
}

func (self *DeviceLocal) SetVpnInterfaceWhileOffline(vpnInterfaceWhileOffline bool) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.vpnInterfaceWhileOffline = vpnInterfaceWhileOffline
	}()
	self.offlineChanged(self.GetOffline(), self.GetVpnInterfaceWhileOffline())
}

func (self *DeviceLocal) GetVpnInterfaceWhileOffline() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.vpnInterfaceWhileOffline
}

func (self *DeviceLocal) RemoveDestination() {
	self.SetDestination(nil, nil, ProvideModeNone)
}

func (self *DeviceLocal) SetDestination(location *ConnectLocation, specs *ProviderSpecList, provideMode ProvideMode) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		self.connectLocation = location

		if self.remoteUserNatClient != nil {
			self.remoteUserNatClient.Close()
			self.remoteUserNatClient = nil
		}

		if specs != nil && 0 < specs.Len() {
			connectSpecs := []*connect.ProviderSpec{}
			for i := 0; i < specs.Len(); i += 1 {
				connectSpecs = append(connectSpecs, specs.Get(i).toConnectProviderSpec())
			}

			generator := connect.NewApiMultiClientGenerator(
				self.ctx,
				connectSpecs,
				self.clientStrategy,
				// exclude self
				[]connect.Id{self.clientId},
				self.networkSpace.apiUrl,
				self.byJwt,
				self.networkSpace.platformUrl,
				self.deviceDescription,
				self.deviceSpec,
				self.appVersion,
				// connect.DefaultClientSettingsNoNetworkEvents,
				connect.DefaultClientSettings,
				connect.DefaultApiMultiClientGeneratorSettings(),
			)
			remoteReceive := func(source connect.TransferPath, ipProtocol connect.IpProtocol, packet []byte) {
				self.stats.UpdateRemoteReceive(ByteCount(len(packet)))
				self.receive(source, ipProtocol, packet)
			}
			self.remoteUserNatClient = connect.NewRemoteUserNatMultiClientWithDefaults(
				self.ctx,
				generator,
				remoteReceive,
				protocol.ProvideMode_Network,
			)
		}
		// else no specs, not an error
	}()
	self.connectLocationChanged(self.GetConnectLocation())
	connectEnabled := self.GetConnectEnabled()
	self.stats.UpdateConnect(connectEnabled)
	self.connectChanged(connectEnabled)
}

func (self *DeviceLocal) SetConnectLocation(location *ConnectLocation) {
	if location == nil {
		self.RemoveDestination()
	} else {
		specs := NewProviderSpecList()
		specs.Add(&ProviderSpec{
			LocationId:      location.ConnectLocationId.LocationId,
			LocationGroupId: location.ConnectLocationId.LocationGroupId,
			ClientId:        location.ConnectLocationId.ClientId,
			BestAvailable:   location.ConnectLocationId.BestAvailable,
		})

		self.SetDestination(location, specs, ProvideModePublic)
	}
}

func (self *DeviceLocal) GetConnectLocation() *ConnectLocation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.connectLocation
}

func (self *DeviceLocal) Shuffle() {
	var remoteUserNatClient connect.UserNatClient
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		remoteUserNatClient = self.remoteUserNatClient
	}()

	if remoteUserNatClient != nil {
		remoteUserNatClient.Shuffle()
	}
}

func (self *DeviceLocal) SendPacket(packet []byte, n int32) bool {
	packetCopy := make([]byte, n)
	copy(packetCopy, packet[0:n])
	source := connect.SourceId(self.clientId)

	var remoteUserNatClient connect.UserNatClient
	var localUserNat *connect.LocalUserNat
	var routeLocal bool
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		remoteUserNatClient = self.remoteUserNatClient
		localUserNat = self.localUserNat
		routeLocal = self.routeLocal
	}()

	if remoteUserNatClient != nil {
		self.stats.UpdateRemoteSend(ByteCount(n))
		return remoteUserNatClient.SendPacket(
			source,
			protocol.ProvideMode_Network,
			packetCopy,
			self.settings.SendTimeout,
		)
	} else if routeLocal {
		// route locally
		return localUserNat.SendPacket(
			source,
			protocol.ProvideMode_Network,
			packetCopy,
			self.settings.SendTimeout,
		)
	} else {
		return false
	}
}

func (self *DeviceLocal) AddReceivePacket(receivePacket ReceivePacket) Sub {
	receive := func(destination connect.TransferPath, ipProtocol connect.IpProtocol, packet []byte) {
		receivePacket.ReceivePacket(packet)
	}
	callbackId := self.receiveCallbacks.Add(receive)
	return newSub(func() {
		self.receiveCallbacks.Remove(callbackId)
	})
}


func (self *DeviceLocal) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.cancel()

	self.client.Cancel()

	if self.remoteUserNatClient != nil {
		self.remoteUserNatClient.Close()
		self.remoteUserNatClient = nil
	}
	// self.localUserNat.RemoveReceivePacketCallback(self.receive)
	self.localUserNatUnsub()
	if self.remoteUserNatProviderLocalUserNat != nil {
		self.remoteUserNatProviderLocalUserNat.Close()
		self.remoteUserNatProviderLocalUserNat = nil
	}
	if self.remoteUserNatProvider != nil {
		self.remoteUserNatProvider.Close()
		self.remoteUserNatProvider = nil
	}

	self.localUserNat.Close()

	for vc, _ := range self.openedViewControllers {
		vc.Close()
	}
	clear(self.openedViewControllers)

	if self.deviceLocalRpc != nil {
		self.deviceLocalRpc.Close()
	}
}

func parseByJwtClientId(byJwt string) (connect.Id, error) {
	claims := gojwt.MapClaims{}
	gojwt.NewParser().ParseUnverified(byJwt, claims)

	jwtClientId, ok := claims["client_id"]
	if !ok {
		return connect.Id{}, fmt.Errorf("byJwt does not contain claim client_id")
	}
	switch v := jwtClientId.(type) {
	case string:
		return connect.ParseId(v)
	default:
		return connect.Id{}, fmt.Errorf("byJwt hav invalid type for client_id: %T", v)
	}
}

/*
type WindowEvents struct {
	windowExpandEvent *connect.WindowExpandEvent
	providerEvents    map[connect.Id]*connect.ProviderEvent
}

func newWindowEvents(
	windowExpandEvent *connect.WindowExpandEvent,
	providerEvents map[connect.Id]*connect.ProviderEvent,
) *WindowEvents {
	return &WindowEvents{
		windowExpandEvent: windowExpandEvent,
		providerEvents:    providerEvents,
	}
}

func (self *WindowEvents) CurrentSize() int {
	count := 0
	for _, providerEvent := range self.providerEvents {
		if providerEvent.State.IsActive() {
			count += 1
		}
	}
	return count
}

func (self *WindowEvents) TargetSize() int {
	return self.windowExpandEvent.TargetSize
}

func (self *WindowEvents) InEvaluationClientCount() int {
	count := 0
	for _, providerEvent := range self.providerEvents {
		if providerEvent.State == connect.ProviderStateInEvaluation {
			count += 1
		}
	}
	return count
}

func (self *WindowEvents) AddedClientCount() int {
	count := 0
	for _, providerEvent := range self.providerEvents {
		if providerEvent.State == connect.ProviderStateAdded {
			count += 1
		}
	}
	return count
}

func (self *WindowEvents) NotAddedClientCount() int {
	count := 0
	for _, providerEvent := range self.providerEvents {
		if providerEvent.State == connect.ProviderStateNotAdded {
			count += 1
		}
	}
	return count
}

func (self *WindowEvents) EvaluationFailedClientCount() int {
	count := 0
	for _, providerEvent := range self.providerEvents {
		if providerEvent.State == connect.ProviderStateEvaluationFailed {
			count += 1
		}
	}
	return count
}
*/
