package sdk

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/golang/glog"

	"github.com/urnetwork/connect/v2025"
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
		instanceId,
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
	listener.with(func() {
		assert.Equal(t, false, listener.event)
		assert.Equal(t, false, listener.eventOffline)
	})
	sub.Close()

}

func TestDeviceRemoteFull(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	for range 10 {
		func() {

			clientId := connect.NewId()
			instanceId := NewId()

			// enable rpc
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
				instanceId,
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

			location := &ConnectLocation{
				ConnectLocationId: &ConnectLocationId{
					ClientId:        NewId(),
					LocationId:      NewId(),
					LocationGroupId: NewId(),
					BestAvailable:   true,
				},
			}

			// set all properties

			deviceRemote.InitProvideSecretKeys()
			deviceRemote.LoadProvideSecretKeys(NewProvideSecretKeyList())
			deviceRemote.SetCanShowRatingDialog(true)
			deviceRemote.SetCanRefer(true)
			deviceRemote.SetRouteLocal(!defaultRouteLocal)
			deviceRemote.SetProvideMode(ProvideModeStream)
			deviceRemote.SetProvidePaused(true)
			deviceRemote.SetOffline(true)
			deviceRemote.SetVpnInterfaceWhileOffline(true)
			deviceRemote.RemoveDestination()
			deviceRemote.SetConnectLocation(location)
			deviceRemote.SetDestination(location, NewProviderSpecList(), ProvideModePublic)
			deviceRemote.Shuffle()

			assert.Equal(t, deviceRemote.GetCanShowRatingDialog(), true)
			assert.Equal(t, deviceRemote.GetCanRefer(), true)
			assert.Equal(t, deviceRemote.GetRouteLocal(), !defaultRouteLocal)
			assert.Equal(t, deviceRemote.GetProvideMode(), ProvideModeStream)
			assert.Equal(t, deviceRemote.GetProvidePaused(), true)
			assert.Equal(t, deviceRemote.GetOffline(), true)
			assert.Equal(t, deviceRemote.GetVpnInterfaceWhileOffline(), true)
			assert.Equal(t, deviceRemote.GetConnectLocation(), location)

			// wait for event callbacks on goroutines to run
			select {
			case <-time.After(500 * time.Millisecond):
			}

			glog.Infof("GG1")
			assert.Equal(t, deviceLocal.GetCanShowRatingDialog(), true)
			glog.Infof("GG2")
			assert.Equal(t, deviceLocal.GetCanRefer(), true)
			glog.Infof("GG3")
			assert.Equal(t, deviceLocal.GetRouteLocal(), !defaultRouteLocal)
			assert.Equal(t, deviceLocal.GetProvideMode(), ProvideModeStream)
			assert.Equal(t, deviceLocal.GetProvidePaused(), true)
			assert.Equal(t, deviceLocal.GetOffline(), true)
			assert.Equal(t, deviceLocal.GetVpnInterfaceWhileOffline(), true)
			glog.Infof("GGX")
			assert.Equal(t, deviceLocal.GetConnectLocation(), location)

			assert.Equal(t, deviceRemote.GetCanShowRatingDialog(), true)
			assert.Equal(t, deviceRemote.GetCanRefer(), true)
			assert.Equal(t, deviceRemote.GetRouteLocal(), !defaultRouteLocal)
			assert.Equal(t, deviceRemote.GetProvideMode(), ProvideModeStream)
			assert.Equal(t, deviceRemote.GetProvidePaused(), true)
			assert.Equal(t, deviceRemote.GetOffline(), true)
			assert.Equal(t, deviceRemote.GetVpnInterfaceWhileOffline(), true)
			assert.Equal(t, deviceRemote.GetConnectLocation(), location)

			provideChangeListener.with(func() {
				assert.Equal(t, provideChangeListener.event, true)
			})
			providePausedChangeListener.with(func() {
				assert.Equal(t, providePausedChangeListener.event, true)
			})
			offlineChangeListener.with(func() {
				assert.Equal(t, offlineChangeListener.event, true)
			})
			connectChangeListener.with(func() {
				assert.Equal(t, connectChangeListener.event, true)
			})
			routeLocalChangeListener.with(func() {
				assert.Equal(t, routeLocalChangeListener.event, true)
			})
			connectLocationChangeListener.with(func() {
				assert.Equal(t, connectLocationChangeListener.event, true)
			})
			provideSecretKeysListener.with(func() {
				assert.Equal(t, provideSecretKeysListener.event, true)
			})
			// FIXME one difference with remote sync later versus now is that the monitor doesn't getted called with empty events
			// monitorEventListener.with(func() {
			// 	assert.Equal(t, monitorEventListener.event, true)
			// })

		}()

		// FIXME once TLS certs are in place remote this
		select {
		case <-time.After(200 * time.Millisecond):
		}
	}

}

func TestDeviceRemoteFullSync(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	for range 10 {
		func() {

			clientId := connect.NewId()
			instanceId := NewId()

			deviceRemote, err := newDeviceRemoteWithOverrides(
				networkSpace,
				byJwt,
				instanceId,
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
			networkModeListener := &testing_networkModeChangeListener{}

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

			networkModeListenerSub := deviceRemote.AddProvideNetworkModeChangeListener(networkModeListener)
			defer networkModeListenerSub.Close()

			windowMonitor := deviceRemote.windowMonitor()
			windowExpandEvent, providerEvents := windowMonitor.Events()
			assert.NotEqual(t, windowExpandEvent, nil)
			assert.NotEqual(t, providerEvents, nil)

			monitorEventCallbackSub := windowMonitor.AddMonitorEventCallback(monitorEventListener.MonitorEventCallback)
			defer monitorEventCallbackSub()

			location := &ConnectLocation{
				ConnectLocationId: &ConnectLocationId{
					ClientId:        NewId(),
					LocationId:      NewId(),
					LocationGroupId: NewId(),
					BestAvailable:   true,
				},
			}

			// set all properties

			deviceRemote.InitProvideSecretKeys()
			deviceRemote.LoadProvideSecretKeys(NewProvideSecretKeyList())
			deviceRemote.SetCanShowRatingDialog(true)
			deviceRemote.SetCanRefer(true)
			deviceRemote.SetRouteLocal(!defaultRouteLocal)
			deviceRemote.SetProvideMode(ProvideModeStream)
			deviceRemote.SetProvideNetworkMode(ProvideNetworkModeWiFi)
			deviceRemote.SetProvidePaused(true)
			deviceRemote.SetOffline(true)
			deviceRemote.SetVpnInterfaceWhileOffline(true)
			deviceRemote.RemoveDestination()
			deviceRemote.SetConnectLocation(location)
			deviceRemote.SetDestination(location, NewProviderSpecList(), ProvideModePublic)
			deviceRemote.Shuffle()

			assert.Equal(t, deviceRemote.GetCanShowRatingDialog(), true)
			assert.Equal(t, deviceRemote.GetCanRefer(), true)
			assert.Equal(t, deviceRemote.GetRouteLocal(), !defaultRouteLocal)
			assert.Equal(t, deviceRemote.GetProvideMode(), ProvideModeStream)
			assert.Equal(t, deviceRemote.GetProvidePaused(), true)
			assert.Equal(t, deviceRemote.GetOffline(), true)
			assert.Equal(t, deviceRemote.GetVpnInterfaceWhileOffline(), true)
			assert.Equal(t, deviceRemote.GetConnectLocation(), location)
			assert.Equal(t, deviceRemote.GetProvideNetworkMode(), ProvideNetworkModeWiFi)

			// sync

			// enable rpc
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

			// wait for event callbacks on goroutines to run
			select {
			case <-time.After(500 * time.Millisecond):
			}

			glog.Infof("GG1")
			assert.Equal(t, deviceLocal.GetCanShowRatingDialog(), true)
			glog.Infof("GG2")
			assert.Equal(t, deviceLocal.GetCanRefer(), true)
			glog.Infof("GG3")
			assert.Equal(t, deviceLocal.GetRouteLocal(), !defaultRouteLocal)
			assert.Equal(t, deviceLocal.GetProvideMode(), ProvideModeStream)
			assert.Equal(t, deviceLocal.GetProvidePaused(), true)
			assert.Equal(t, deviceLocal.GetOffline(), true)
			assert.Equal(t, deviceLocal.GetVpnInterfaceWhileOffline(), true)
			glog.Infof("GGX")
			assert.Equal(t, deviceLocal.GetConnectLocation(), location)

			assert.Equal(t, deviceRemote.GetCanShowRatingDialog(), true)
			assert.Equal(t, deviceRemote.GetCanRefer(), true)
			assert.Equal(t, deviceRemote.GetRouteLocal(), !defaultRouteLocal)
			assert.Equal(t, deviceRemote.GetProvideMode(), ProvideModeStream)
			assert.Equal(t, deviceRemote.GetProvidePaused(), true)
			assert.Equal(t, deviceRemote.GetOffline(), true)
			assert.Equal(t, deviceRemote.GetVpnInterfaceWhileOffline(), true)
			assert.Equal(t, deviceRemote.GetConnectLocation(), location)
			assert.Equal(t, deviceRemote.GetProvideNetworkMode(), ProvideNetworkModeWiFi)

			provideChangeListener.with(func() {
				assert.Equal(t, provideChangeListener.event, true)
			})
			providePausedChangeListener.with(func() {
				assert.Equal(t, providePausedChangeListener.event, true)
			})
			offlineChangeListener.with(func() {
				assert.Equal(t, offlineChangeListener.event, true)
			})
			connectChangeListener.with(func() {
				assert.Equal(t, connectChangeListener.event, true)
			})
			routeLocalChangeListener.with(func() {
				assert.Equal(t, routeLocalChangeListener.event, true)
			})
			connectLocationChangeListener.with(func() {
				assert.Equal(t, connectLocationChangeListener.event, true)
			})
			provideSecretKeysListener.with(func() {
				assert.Equal(t, provideSecretKeysListener.event, true)
			})
			monitorEventListener.with(func() {
				assert.Equal(t, monitorEventListener.event, true)
			})
			networkModeListener.with(func() {
				assert.Equal(t, monitorEventListener.event, true)
			})

		}()

		// FIXME once TLS certs are in place remote this
		select {
		case <-time.After(200 * time.Millisecond):
		}
	}

}

func TestDeviceRemoteApi(t *testing.T) {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()

	// enable rpc
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
		instanceId,
		defaultDeviceRpcSettings(),
		clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()

	bodyBytes, err := deviceRemote.httpGetRaw(ctx, "https://api.bringyour.com/hello", "")
	assert.Equal(t, err, nil)
	assert.NotEqual(t, bodyBytes, nil)
	glog.Infof("response body=%s", string(bodyBytes))
	assert.NotEqual(t, len(bodyBytes), 0)

	// FIXME allow POST on the hello route
	// bodyBytes, err := deviceRemote.httpGetRaw(ctx, "https://api.bringyour.com/hello", "")
	// assert.Equal(t, err, nil)

}

func TestDeviceRemoteLastKnownValues(t *testing.T) {
	// sync
	// then close local
	// remote should retain the values

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
		instanceId,
		defaultDeviceRpcSettings(),
		clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()

	// set all properties

	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{
			ClientId:        NewId(),
			LocationId:      NewId(),
			LocationGroupId: NewId(),
			BestAvailable:   true,
		},
	}

	deviceRemote.SetProvideControlMode(ProvideControlModeManual)
	deviceRemote.SetCanShowRatingDialog(true)
	deviceRemote.SetCanRefer(true)
	deviceRemote.SetRouteLocal(!defaultRouteLocal)
	deviceRemote.SetProvideMode(ProvideModeStream)
	deviceRemote.SetProvidePaused(true)
	deviceRemote.SetOffline(true)
	deviceRemote.SetVpnInterfaceWhileOffline(true)
	deviceRemote.RemoveDestination()
	deviceRemote.SetConnectLocation(location)
	deviceRemote.SetDestination(location, NewProviderSpecList(), ProvideModePublic)
	// deviceRemote.SetProvideNetworkMode(ProvideNetworkModeAll)
	deviceRemote.Shuffle()

	// sync

	// enable rpc
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

	// wait for event callbacks on goroutines to run
	select {
	case <-time.After(500 * time.Millisecond):
	}

	glog.Infof("GG1")
	assert.Equal(t, deviceLocal.GetProvideControlMode(), ProvideControlModeManual)
	assert.Equal(t, deviceLocal.GetCanShowRatingDialog(), true)
	glog.Infof("GG2")
	assert.Equal(t, deviceLocal.GetCanRefer(), true)
	glog.Infof("GG3")
	assert.Equal(t, deviceLocal.GetRouteLocal(), !defaultRouteLocal)
	assert.Equal(t, deviceLocal.GetProvideMode(), ProvideModeStream)
	assert.Equal(t, deviceLocal.GetProvidePaused(), true)
	assert.Equal(t, deviceLocal.GetOffline(), true)
	assert.Equal(t, deviceLocal.GetVpnInterfaceWhileOffline(), true)
	glog.Infof("GGX")
	assert.Equal(t, deviceLocal.GetConnectLocation(), location)

	assert.Equal(t, deviceRemote.GetProvideControlMode(), ProvideControlModeManual)
	assert.Equal(t, deviceRemote.GetCanShowRatingDialog(), true)
	assert.Equal(t, deviceRemote.GetCanRefer(), true)
	assert.Equal(t, deviceRemote.GetRouteLocal(), !defaultRouteLocal)
	assert.Equal(t, deviceRemote.GetProvideMode(), ProvideModeStream)
	assert.Equal(t, deviceRemote.GetProvidePaused(), true)
	assert.Equal(t, deviceRemote.GetOffline(), true)
	assert.Equal(t, deviceRemote.GetVpnInterfaceWhileOffline(), true)
	assert.Equal(t, deviceRemote.GetConnectLocation(), location)
	// assert.Equal(t, deviceRemote.GetProvideNetworkMode(), ProvideNetworkModeAll)

	deviceLocal.Close()

	// make sure the remote value retains the last know state

	assert.Equal(t, deviceRemote.GetProvideControlMode(), ProvideControlModeManual)
	assert.Equal(t, deviceRemote.GetCanShowRatingDialog(), true)
	assert.Equal(t, deviceRemote.GetCanRefer(), true)
	assert.Equal(t, deviceRemote.GetRouteLocal(), !defaultRouteLocal)
	assert.Equal(t, deviceRemote.GetProvideMode(), ProvideModeStream)
	assert.Equal(t, deviceRemote.GetProvidePaused(), true)
	assert.Equal(t, deviceRemote.GetOffline(), true)
	assert.Equal(t, deviceRemote.GetVpnInterfaceWhileOffline(), true)
	assert.Equal(t, deviceRemote.GetConnectLocation(), location)
	// assert.Equal(t, deviceRemote.GetProvideNetworkMode(), ProvideNetworkModeAll)

}

func TestDeviceRemoteLastKnownValuesListeners(t *testing.T) {
	// sync
	// then close local
	// remote should retain the values

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
		instanceId,
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

	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{
			ClientId:        NewId(),
			LocationId:      NewId(),
			LocationGroupId: NewId(),
			BestAvailable:   true,
		},
	}

	// set all properties

	deviceRemote.SetCanShowRatingDialog(true)
	deviceRemote.SetCanRefer(true)
	deviceRemote.SetRouteLocal(!defaultRouteLocal)
	deviceRemote.SetProvideMode(ProvideModeStream)
	deviceRemote.SetProvidePaused(true)
	deviceRemote.SetOffline(true)
	deviceRemote.SetVpnInterfaceWhileOffline(true)
	deviceRemote.RemoveDestination()
	deviceRemote.SetConnectLocation(location)
	deviceRemote.SetDestination(location, NewProviderSpecList(), ProvideModePublic)
	deviceRemote.Shuffle()

	// sync

	// enable rpc
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

	// wait for event callbacks on goroutines to run
	select {
	case <-time.After(500 * time.Millisecond):
	}

	glog.Infof("GG1")
	assert.Equal(t, deviceLocal.GetCanShowRatingDialog(), true)
	glog.Infof("GG2")
	assert.Equal(t, deviceLocal.GetCanRefer(), true)
	glog.Infof("GG3")
	assert.Equal(t, deviceLocal.GetRouteLocal(), !defaultRouteLocal)
	assert.Equal(t, deviceLocal.GetProvideMode(), ProvideModeStream)
	assert.Equal(t, deviceLocal.GetProvidePaused(), true)
	assert.Equal(t, deviceLocal.GetOffline(), true)
	assert.Equal(t, deviceLocal.GetVpnInterfaceWhileOffline(), true)
	glog.Infof("GGX")
	assert.Equal(t, deviceLocal.GetConnectLocation(), location)

	provideChangeListener.with(func() {
		assert.Equal(t, provideChangeListener.event, true)
	})
	providePausedChangeListener.with(func() {
		assert.Equal(t, providePausedChangeListener.event, true)
	})
	offlineChangeListener.with(func() {
		assert.Equal(t, offlineChangeListener.event, true)
	})
	connectChangeListener.with(func() {
		assert.Equal(t, connectChangeListener.event, true)
	})
	routeLocalChangeListener.with(func() {
		assert.Equal(t, routeLocalChangeListener.event, true)
	})
	connectLocationChangeListener.with(func() {
		assert.Equal(t, connectLocationChangeListener.event, true)
	})
	provideSecretKeysListener.with(func() {
		assert.Equal(t, provideSecretKeysListener.event, true)
	})
	monitorEventListener.with(func() {
		assert.Equal(t, monitorEventListener.event, true)
	})

	deviceLocal.Close()

	// make sure the remote value retains the last know state
	// the last known state was set in the listeners

	assert.Equal(t, deviceRemote.GetCanShowRatingDialog(), true)
	assert.Equal(t, deviceRemote.GetCanRefer(), true)
	assert.Equal(t, deviceRemote.GetRouteLocal(), !defaultRouteLocal)
	assert.Equal(t, deviceRemote.GetProvideMode(), ProvideModeStream)
	assert.Equal(t, deviceRemote.GetProvidePaused(), true)
	assert.Equal(t, deviceRemote.GetOffline(), true)
	assert.Equal(t, deviceRemote.GetVpnInterfaceWhileOffline(), true)
	assert.Equal(t, deviceRemote.GetConnectLocation(), location)

}

func TestDeviceRemoteSecurityPolicyStats(t *testing.T) {

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
		instanceId,
		defaultDeviceRpcSettings(),
		clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()

	deviceRemote.Sync()
	deviceRemote.waitForSync(5 * time.Second)

	deviceRemote.egressSecurityPolicy().Stats(false)
	deviceRemote.ingressSecurityPolicy().Stats(false)

	deviceRemote.egressSecurityPolicy().Stats(true)
	deviceRemote.ingressSecurityPolicy().Stats(true)

}

type testing_listener struct {
	stateLock sync.Mutex
	event     bool
}

func (self *testing_listener) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = false
}

func (self *testing_listener) with(c func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	c()
}

type testing_provideChangeListener struct {
	testing_listener
	provideEnabled bool
}

func (self *testing_provideChangeListener) ProvideChanged(provideEnabled bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.provideEnabled = provideEnabled
}

type testing_providePausedChangeListener struct {
	testing_listener
	providePaused bool
}

func (self *testing_providePausedChangeListener) ProvidePausedChanged(providePaused bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.providePaused = providePaused
}

type testing_offlineChangeListener struct {
	testing_listener
	eventOffline                  bool
	eventVpnInterfaceWhileOffline bool
}

func (self *testing_offlineChangeListener) OfflineChanged(offline bool, vpnInterfaceWhileOffline bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.eventOffline = offline
	self.eventVpnInterfaceWhileOffline = vpnInterfaceWhileOffline
}

type testing_connectChangeListener struct {
	testing_listener
	connectEnabled bool
}

func (self *testing_connectChangeListener) ConnectChanged(connectEnabled bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.connectEnabled = connectEnabled
}

type testing_routeLocalChangeListener struct {
	testing_listener
	routeLocal bool
}

func (self *testing_routeLocalChangeListener) RouteLocalChanged(routeLocal bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.routeLocal = routeLocal
}

type testing_connectLocationChangeListener struct {
	testing_listener
	location *ConnectLocation
}

func (self *testing_connectLocationChangeListener) ConnectLocationChanged(location *ConnectLocation) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.location = location
}

type testing_provideSecretKeysListener struct {
	testing_listener
	provideSecretKeyList *ProvideSecretKeyList
}

func (self *testing_provideSecretKeysListener) ProvideSecretKeysChanged(provideSecretKeyList *ProvideSecretKeyList) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.provideSecretKeyList = provideSecretKeyList
}

type testing_monitorEventListener struct {
	testing_listener
	windowExpandEvent *connect.WindowExpandEvent
	providerEvents    map[connect.Id]*connect.ProviderEvent
}

func (self *testing_monitorEventListener) MonitorEventCallback(windowExpandEvent *connect.WindowExpandEvent, providerEvents map[connect.Id]*connect.ProviderEvent, reset bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.windowExpandEvent = windowExpandEvent
	self.providerEvents = providerEvents
}

type testing_networkModeChangeListener struct {
	testing_listener
	provideNetworkMode *ProvideNetworkMode
}

func (self *testing_networkModeChangeListener) ProvideNetworkModeChanged(provideNetworkMode ProvideNetworkMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.provideNetworkMode = &provideNetworkMode
}
