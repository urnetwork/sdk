package sdk

import (
	"context"
	"testing"
	// "time"

	"github.com/go-playground/assert/v2"
)


// FIXME start remote and local
// FIXME use a test JWT against a bogus network space, the client doesn't need to connect

func TestDeviceRemote(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	instanceId := NewId()


	// FIXME enable RPC
	deviceLocal, err := newDeviceLocal(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		true,
		defaultDeviceLocalSettings(),
	)
	if err != nil {
		panic(err)
	}
	defer deviceLocal.Close()


	deviceRemote, err := newDeviceRemote(
		networkSpace,
		byJwt,
		defaultDeviceRpcSettings(),
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()


	assert.Equal(t, false, deviceRemote.GetOffline())
	assert.Equal(t, false, deviceLocal.GetOffline())
	
	deviceRemote.SetOffline(true)
	assert.Equal(t, true, deviceRemote.GetOffline())
	assert.Equal(t, true, deviceLocal.GetOffline())

	deviceLocal.SetOffline(false)
	assert.Equal(t, false, deviceRemote.GetOffline())
	assert.Equal(t, false, deviceLocal.GetOffline())


	listener := &testing_offlineChangeListener{}
	sub := deviceRemote.AddOfflineChangeListener(listener)
	deviceRemote.SetOffline(true)
	assert.Equal(t, true, listener.event)
	assert.Equal(t, true, listener.eventOffline)
	sub.Close()

}


type testing_offlineChangeListener struct {
	event bool
	eventOffline bool
	eventVpnInterfaceWhileOffline bool
}

func (self *testing_offlineChangeListener) clear() {
	self.event = false
}

func (self *testing_offlineChangeListener) OfflineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	self.eventOffline = offline
	self.eventVpnInterfaceWhileOffline = vpnInterfaceWhileOffline
}

