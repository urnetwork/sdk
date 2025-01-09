package sdk

import (
	"context"
	"testing"
	"time"
	"sync"

	"github.com/go-playground/assert/v2"

	// "github.com/golang/glog"

	"github.com/urnetwork/connect"
)


// FIXME start remote and local
// FIXME use a test JWT against a bogus network space, the client doesn't need to connect

func TestDeviceRemoteSimple(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()


	// FIXME enable RPC
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		true,
		defaultDeviceLocalSettings(),
		clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceLocal.Close()


	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		defaultDeviceRpcSettings(),
		clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()


	assert.Equal(t, true, deviceRemote.GetOffline())
	assert.Equal(t, true, deviceLocal.GetOffline())
	
	deviceRemote.SetOffline(false)
	assert.Equal(t, false, deviceRemote.GetOffline())
	assert.Equal(t, false, deviceLocal.GetOffline())

	deviceLocal.SetOffline(true)
	assert.Equal(t, true, deviceRemote.GetOffline())
	assert.Equal(t, true, deviceLocal.GetOffline())


	listener := &testing_offlineChangeListener{}
	sub := deviceRemote.AddOfflineChangeListener(listener)
	deviceRemote.SetOffline(false)
	assert.Equal(t, false, listener.event)
	assert.Equal(t, false, listener.eventOffline)
	sub.Close()


}


func TestDeviceRemoteFullSync(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()




	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		defaultDeviceRpcSettings(),
		clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()


	// add all listeners

	provideChangeListener := &testing_provideChangeListener{}
	providePausedChangeListener := &testing_providePausedChangeListener{}
	offlineChangeListener := &testing_offlineChangeListener{}
	connectChangeListener := &testing_connectChangeListener{}
	routeLocalChangeListener := &testing_routeLocalChangeListener{}
	connectLocationChangeListener := &testing_connectLocationChangeListener{}
	provideSecretKeysListener := &testing_provideSecretKeysListener{}
	monitorEventListener := &testing_monitorEventListener{}



	provideChangeListenerSub := deviceRemote.AddProvideChangeListener(provideChangeListener)
	defer provideChangeListenerSub.Close()

	providePausedChangeListenerSub := deviceRemote.AddProvidePausedChangeListener(providePausedChangeListener)
	defer providePausedChangeListenerSub.Close()

	offlineChangeListenerSub := deviceRemote.AddOfflineChangeListener(offlineChangeListener)
	defer offlineChangeListenerSub.Close()

	connectChangeSub := deviceRemote.AddConnectChangeListener(connectChangeListener)
	defer connectChangeSub.Close()

	routeLocalChangeListenerSub := deviceRemote.AddRouteLocalChangeListener(routeLocalChangeListener)
	defer routeLocalChangeListenerSub.Close()

	connectLocationChangeListenerSub := deviceRemote.AddConnectLocationChangeListener(connectLocationChangeListener)
	defer connectLocationChangeListenerSub.Close()

	provideSecretKeysListenerSub := deviceRemote.AddProvideSecretKeysListener(provideSecretKeysListener)
	defer provideSecretKeysListenerSub.Close()


	windowMonitor := deviceRemote.windowMonitor()
	windowExpandEvent, providerEvents := windowMonitor.Events()
	assert.NotEqual(t, windowExpandEvent, nil)
	assert.NotEqual(t, providerEvents, nil)

	monitorEventCallbackSub := windowMonitor.AddMonitorEventCallback(monitorEventListener.MonitorEventCallback)
	defer monitorEventCallbackSub()


	// set all properties

	deviceRemote.SetCanShowRatingDialog(true)
	deviceRemote.SetCanRefer(true)
	deviceRemote.SetRouteLocal(true)
	deviceRemote.SetProvideMode(ProvideModeStream)
	deviceRemote.SetProvidePaused(true)
	deviceRemote.SetOffline(true)
	deviceRemote.SetVpnInterfaceWhileOffline(true)
	deviceRemote.SetConnectLocation(&ConnectLocation{})
	deviceRemote.SetDestination(&ConnectLocation{}, NewProviderSpecList(), ProvideModePublic)
	deviceRemote.Shuffle()


	// sync



	// FIXME enable RPC
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		true,
		defaultDeviceLocalSettings(),
		clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceLocal.Close()


	deviceRemote.Sync()

	deviceRemote.waitForSync(5 * time.Second)


	assert.Equal(t, deviceLocal.GetCanShowRatingDialog(), true)
	assert.Equal(t, deviceLocal.GetCanRefer(), true)
	assert.Equal(t, deviceLocal.GetRouteLocal(), true)
	assert.Equal(t, deviceLocal.GetProvideMode(), ProvideModeStream)
	assert.Equal(t, deviceLocal.GetProvidePaused(), true)
	assert.Equal(t, deviceLocal.GetOffline(), true)
	assert.Equal(t, deviceLocal.GetVpnInterfaceWhileOffline(), true)
	assert.Equal(t, deviceRemote.GetConnectLocation(), &ConnectLocation{})


	assert.Equal(t, provideChangeListener.event, true)
	assert.Equal(t, providePausedChangeListener.event, true)
	assert.Equal(t, offlineChangeListener.event, true)
	assert.Equal(t, connectChangeListener.event, true)
	assert.Equal(t, routeLocalChangeListener.event, true)
	assert.Equal(t, connectLocationChangeListener.event, true)
	assert.Equal(t, provideSecretKeysListener.event, true)
	assert.Equal(t, monitorEventListener.event, true)


}


type testing_provideChangeListener struct {
	stateLock sync.Mutex
	event bool
	provideEnabled bool
}

func (self *testing_provideChangeListener) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	
	self.event = false
}

func (self *testing_provideChangeListener) ProvideChanged(provideEnabled bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.provideEnabled = provideEnabled
}


type testing_providePausedChangeListener struct {
	stateLock sync.Mutex
	event bool
	providePaused bool
}

func (self *testing_providePausedChangeListener) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	
	self.event = false
}

func (self *testing_providePausedChangeListener) ProvidePausedChanged(providePaused bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.providePaused = providePaused
}


type testing_offlineChangeListener struct {
	stateLock sync.Mutex
	event bool
	eventOffline bool
	eventVpnInterfaceWhileOffline bool
}

func (self *testing_offlineChangeListener) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	
	self.event = false
}

func (self *testing_offlineChangeListener) OfflineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.eventOffline = offline
	self.eventVpnInterfaceWhileOffline = vpnInterfaceWhileOffline
}


type testing_connectChangeListener struct {
	stateLock sync.Mutex
	event bool
	connectEnabled bool
}

func (self *testing_connectChangeListener) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	
	self.event = false
}

func (self *testing_connectChangeListener) ConnectChanged(connectEnabled bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.connectEnabled = connectEnabled
}


type testing_routeLocalChangeListener struct {
	stateLock sync.Mutex
	event bool
	routeLocal bool
}

func (self *testing_routeLocalChangeListener) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	
	self.event = false
}

func (self *testing_routeLocalChangeListener) RouteLocalChanged(routeLocal bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.routeLocal = routeLocal
}


type testing_connectLocationChangeListener struct {
	stateLock sync.Mutex
	event bool
	location *ConnectLocation
}

func (self *testing_connectLocationChangeListener) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	
	self.event = false
}

func (self *testing_connectLocationChangeListener) ConnectLocationChanged(location *ConnectLocation) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.location = location
}


type testing_provideSecretKeysListener struct {
	stateLock sync.Mutex
	event bool
	provideSecretKeyList *ProvideSecretKeyList
}

func (self *testing_provideSecretKeysListener) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	
	self.event = false
}

func (self *testing_provideSecretKeysListener) ProvideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.provideSecretKeyList = provideSecretKeyList
}


type testing_monitorEventListener struct {
	stateLock sync.Mutex
	event bool
	windowExpandEvent *connect.WindowExpandEvent
	providerEvents map[connect.Id]*connect.ProviderEvent
}

func (self *testing_monitorEventListener) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	
	self.event = false
}

func (self *testing_monitorEventListener) MonitorEventCallback(windowExpandEvent *connect.WindowExpandEvent, providerEvents map[connect.Id]*connect.ProviderEvent) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.windowExpandEvent = windowExpandEvent
	self.providerEvents = providerEvents
}


