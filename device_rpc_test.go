package sdk

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/glog"

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
		true,
		false,
		nil,
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
		testing_deviceRpcDialerDefault(),
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

			settings := defaultDeviceRpcSettings()

			// enable rpc
			deviceLocal, err := newDeviceLocalWithOverrides(
				true,
				false,
				nil,
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
				settings,
				clientId,
				testing_deviceRpcDialer(settings),
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
			performanceProfileChangeListener := &testing_performanceProfileChangeListener{}
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

			performanceProfileChangeListenerSub := deviceRemote.AddPerformanceProfileChangeListener(performanceProfileChangeListener)
			defer performanceProfileChangeListenerSub.Close()

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
			deviceRemote.SetRouteLocal(!settings.DefaultRouteLocal)
			deviceRemote.SetPerformanceProfile(&PerformanceProfile{})
			deviceRemote.SetProvideMode(ProvideModeStream)
			deviceRemote.SetProvidePaused(true)
			deviceRemote.SetOffline(true)
			deviceRemote.SetVpnInterfaceWhileOffline(true)
			deviceRemote.RemoveDestination()
			deviceRemote.SetConnectLocation(location)
			deviceRemote.SetDestination(location, NewProviderSpecList())
			deviceRemote.Shuffle()

			assert.Equal(t, deviceRemote.GetCanShowRatingDialog(), true)
			assert.Equal(t, deviceRemote.GetCanRefer(), true)
			assert.Equal(t, deviceRemote.GetRouteLocal(), !settings.DefaultRouteLocal)
			assert.NotEqual(t, deviceRemote.GetPerformanceProfile(), nil)
			assert.Equal(t, deviceRemote.GetProvideMode(), ProvideModeStream)
			assert.Equal(t, deviceRemote.GetProvidePaused(), true)
			assert.Equal(t, deviceRemote.GetOffline(), true)
			assert.Equal(t, deviceRemote.GetVpnInterfaceWhileOffline(), true)
			assert.Equal(t, deviceRemote.GetConnectLocation(), location)

			// wait for event callbacks on goroutines to run
			select {
			case <-time.After(500 * time.Millisecond):
			}

			assert.Equal(t, deviceLocal.GetCanShowRatingDialog(), true)
			assert.Equal(t, deviceLocal.GetCanRefer(), true)
			assert.Equal(t, deviceLocal.GetRouteLocal(), !settings.DefaultRouteLocal)
			assert.NotEqual(t, deviceLocal.GetPerformanceProfile(), nil)
			assert.Equal(t, deviceLocal.GetProvideMode(), ProvideModeStream)
			assert.Equal(t, deviceLocal.GetProvidePaused(), true)
			assert.Equal(t, deviceLocal.GetOffline(), true)
			assert.Equal(t, deviceLocal.GetVpnInterfaceWhileOffline(), true)
			assert.Equal(t, deviceLocal.GetConnectLocation(), location)

			assert.Equal(t, deviceRemote.GetCanShowRatingDialog(), true)
			assert.Equal(t, deviceRemote.GetCanRefer(), true)
			assert.Equal(t, deviceRemote.GetRouteLocal(), !settings.DefaultRouteLocal)
			assert.NotEqual(t, deviceRemote.GetPerformanceProfile(), nil)
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
			performanceProfileChangeListener.with(func() {
				assert.Equal(t, performanceProfileChangeListener.event, true)
				assert.NotEqual(t, performanceProfileChangeListener.performanceProfile, nil)
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

			settings := defaultDeviceRpcSettings()

			deviceRemote, err := newDeviceRemoteWithOverrides(
				networkSpace,
				byJwt,
				instanceId,
				settings,
				clientId,
				testing_deviceRpcDialer(settings),
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
			deviceRemote.SetRouteLocal(!settings.DefaultRouteLocal)
			deviceRemote.SetPerformanceProfile(&PerformanceProfile{})
			deviceRemote.SetProvideMode(ProvideModeStream)
			deviceRemote.SetProvideNetworkMode(ProvideNetworkModeWiFi)
			deviceRemote.SetProvidePaused(true)
			deviceRemote.SetOffline(true)
			deviceRemote.SetVpnInterfaceWhileOffline(true)
			deviceRemote.RemoveDestination()
			deviceRemote.SetConnectLocation(location)
			deviceRemote.SetDestination(location, NewProviderSpecList())
			deviceRemote.Shuffle()

			assert.Equal(t, deviceRemote.GetCanShowRatingDialog(), true)
			assert.Equal(t, deviceRemote.GetCanRefer(), true)
			assert.Equal(t, deviceRemote.GetRouteLocal(), !settings.DefaultRouteLocal)
			assert.NotEqual(t, deviceRemote.GetPerformanceProfile(), nil)
			assert.Equal(t, deviceRemote.GetProvideMode(), ProvideModeStream)
			assert.Equal(t, deviceRemote.GetProvidePaused(), true)
			assert.Equal(t, deviceRemote.GetOffline(), true)
			assert.Equal(t, deviceRemote.GetVpnInterfaceWhileOffline(), true)
			assert.Equal(t, deviceRemote.GetConnectLocation(), location)
			assert.Equal(t, deviceRemote.GetProvideNetworkMode(), ProvideNetworkModeWiFi)

			// sync

			// enable rpc
			deviceLocal, err := newDeviceLocalWithOverrides(
				true,
				false,
				nil,
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

			assert.Equal(t, deviceLocal.GetCanShowRatingDialog(), true)
			assert.Equal(t, deviceLocal.GetCanRefer(), true)
			assert.Equal(t, deviceLocal.GetRouteLocal(), !settings.DefaultRouteLocal)
			assert.NotEqual(t, deviceLocal.GetPerformanceProfile(), nil)
			assert.Equal(t, deviceLocal.GetProvideMode(), ProvideModeStream)
			assert.Equal(t, deviceLocal.GetProvidePaused(), true)
			assert.Equal(t, deviceLocal.GetOffline(), true)
			assert.Equal(t, deviceLocal.GetVpnInterfaceWhileOffline(), true)
			assert.Equal(t, deviceLocal.GetConnectLocation(), location)

			assert.Equal(t, deviceRemote.GetCanShowRatingDialog(), true)
			assert.Equal(t, deviceRemote.GetCanRefer(), true)
			assert.Equal(t, deviceRemote.GetRouteLocal(), !settings.DefaultRouteLocal)
			assert.NotEqual(t, deviceRemote.GetPerformanceProfile(), nil)
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
		true,
		false,
		nil,
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
		testing_deviceRpcDialerDefault(),
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

	settings := defaultDeviceRpcSettings()

	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		settings,
		clientId,
		testing_deviceRpcDialer(settings),
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
	deviceRemote.SetRouteLocal(!settings.DefaultRouteLocal)
	deviceRemote.SetPerformanceProfile(&PerformanceProfile{})
	deviceRemote.SetProvideMode(ProvideModeStream)
	deviceRemote.SetProvidePaused(true)
	deviceRemote.SetOffline(true)
	deviceRemote.SetVpnInterfaceWhileOffline(true)
	deviceRemote.RemoveDestination()
	deviceRemote.SetConnectLocation(location)
	deviceRemote.SetDestination(location, NewProviderSpecList())
	// deviceRemote.SetProvideNetworkMode(ProvideNetworkModeAll)
	deviceRemote.Shuffle()

	// sync

	// enable rpc
	deviceLocal, err := newDeviceLocalWithOverrides(
		true,
		false,
		nil,
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

	assert.Equal(t, deviceLocal.GetProvideControlMode(), ProvideControlModeManual)
	assert.Equal(t, deviceLocal.GetCanShowRatingDialog(), true)
	assert.Equal(t, deviceLocal.GetCanRefer(), true)
	assert.Equal(t, deviceLocal.GetRouteLocal(), !settings.DefaultRouteLocal)
	assert.NotEqual(t, deviceLocal.GetPerformanceProfile(), nil)
	assert.Equal(t, deviceLocal.GetProvideMode(), ProvideModeStream)
	assert.Equal(t, deviceLocal.GetProvidePaused(), true)
	assert.Equal(t, deviceLocal.GetOffline(), true)
	assert.Equal(t, deviceLocal.GetVpnInterfaceWhileOffline(), true)
	assert.Equal(t, deviceLocal.GetConnectLocation(), location)

	assert.Equal(t, deviceRemote.GetProvideControlMode(), ProvideControlModeManual)
	assert.Equal(t, deviceRemote.GetCanShowRatingDialog(), true)
	assert.Equal(t, deviceRemote.GetCanRefer(), true)
	assert.Equal(t, deviceRemote.GetRouteLocal(), !settings.DefaultRouteLocal)
	assert.NotEqual(t, deviceLocal.GetPerformanceProfile(), nil)
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
	assert.Equal(t, deviceRemote.GetRouteLocal(), !settings.DefaultRouteLocal)
	assert.NotEqual(t, deviceRemote.GetPerformanceProfile(), nil)
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

	settings := defaultDeviceRpcSettings()

	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		settings,
		clientId,
		testing_deviceRpcDialer(settings),
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
	deviceRemote.SetRouteLocal(!settings.DefaultRouteLocal)
	deviceRemote.SetPerformanceProfile(&PerformanceProfile{})
	deviceRemote.SetProvideMode(ProvideModeStream)
	deviceRemote.SetProvidePaused(true)
	deviceRemote.SetOffline(true)
	deviceRemote.SetVpnInterfaceWhileOffline(true)
	deviceRemote.RemoveDestination()
	deviceRemote.SetConnectLocation(location)
	deviceRemote.SetDestination(location, NewProviderSpecList())
	deviceRemote.Shuffle()

	// sync

	// enable rpc
	deviceLocal, err := newDeviceLocalWithOverrides(
		true,
		false,
		nil,
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

	assert.Equal(t, deviceLocal.GetCanShowRatingDialog(), true)
	assert.Equal(t, deviceLocal.GetCanRefer(), true)
	assert.Equal(t, deviceLocal.GetRouteLocal(), !settings.DefaultRouteLocal)
	assert.NotEqual(t, deviceLocal.GetPerformanceProfile(), nil)
	assert.Equal(t, deviceLocal.GetProvideMode(), ProvideModeStream)
	assert.Equal(t, deviceLocal.GetProvidePaused(), true)
	assert.Equal(t, deviceLocal.GetOffline(), true)
	assert.Equal(t, deviceLocal.GetVpnInterfaceWhileOffline(), true)
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
	assert.Equal(t, deviceRemote.GetRouteLocal(), !settings.DefaultRouteLocal)
	assert.NotEqual(t, deviceRemote.GetPerformanceProfile(), nil)
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
		true,
		false,
		nil,
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
		testing_deviceRpcDialerDefault(),
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

func TestDeviceRemoteSelfSignedCert(t *testing.T) {
	// the device remote dials the device local over wss, pinning a self-signed
	// certificate generated for this session

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()

	keyMaterial, err := GenerateDeviceRpcKeyMaterial()
	assert.Equal(t, err, nil)
	clientPem := keyMaterial.GetClientPem()
	clientCertPem := keyMaterial.GetClientCertPem()
	serverPem := keyMaterial.GetServerPem()
	serverCertPem := keyMaterial.GetServerCertPem()
	assert.NotEqual(t, len(clientPem), 0)
	assert.NotEqual(t, len(serverPem), 0)

	settings := defaultDeviceRpcSettings()

	// device local with its built-in rpc disabled; attach a manager backed by a
	// tls listener using the generated server pem
	deviceLocal, err := newDeviceLocalWithOverrides(
		true,
		false,
		nil,
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		false,
		defaultDeviceLocalSettings(),
		clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceLocal.Close()

	listener := NewWebsocketDeviceRpcListener(settings.Address, serverPem, clientCertPem, settings)
	rpcManager := newDeviceLocalRpcManager(deviceLocal.ctx, deviceLocal, settings, listener)
	defer rpcManager.Close()

	// device remote presents its client cert and pins the server cert (mTLS)
	dialer := NewWebsocketDeviceRpcDialer(settings.Address, clientPem, serverCertPem, settings)
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		settings,
		clientId,
		dialer,
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()

	deviceRemote.Sync()
	synced := deviceRemote.waitForSync(5 * time.Second)
	assert.Equal(t, synced, true)

	// state propagates over the tls connection in both directions
	deviceRemote.SetOffline(false)
	assert.Equal(t, deviceRemote.GetOffline(), false)
	assert.Equal(t, deviceLocal.GetOffline(), false)

	deviceLocal.SetOffline(true)
	assert.Equal(t, deviceRemote.GetOffline(), true)
	assert.Equal(t, deviceLocal.GetOffline(), true)
}

// testing_mtlsPinMismatch runs an mTLS session where one side's pin is wrong and
// asserts the handshake fails so sync never completes.
func testing_mtlsPinMismatch(t *testing.T, serverPem string, clientCertPem string, clientPem string, serverCertPem string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()

	settings := defaultDeviceRpcSettings()

	deviceLocal, err := newDeviceLocalWithOverrides(
		true,
		false,
		nil,
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		false,
		defaultDeviceLocalSettings(),
		clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceLocal.Close()

	listener := NewWebsocketDeviceRpcListener(settings.Address, serverPem, clientCertPem, settings)
	rpcManager := newDeviceLocalRpcManager(deviceLocal.ctx, deviceLocal, settings, listener)
	defer rpcManager.Close()

	dialer := NewWebsocketDeviceRpcDialer(settings.Address, clientPem, serverCertPem, settings)
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		settings,
		clientId,
		dialer,
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()

	deviceRemote.Sync()
	// the pin does not match, so the handshake fails and sync never completes
	synced := deviceRemote.waitForSync(2 * time.Second)
	assert.Equal(t, synced, false)
}

// TestDeviceRemoteSelfSignedCertServerPinMismatch verifies the dialer rejects a
// server presenting a certificate other than the pinned one.
func TestDeviceRemoteSelfSignedCertServerPinMismatch(t *testing.T) {
	km, err := GenerateDeviceRpcKeyMaterial()
	assert.Equal(t, err, nil)
	wrong, err := GenerateDeviceRpcKeyMaterial()
	assert.Equal(t, err, nil)

	// dialer pins the wrong server cert
	testing_mtlsPinMismatch(t, km.GetServerPem(), km.GetClientCertPem(), km.GetClientPem(), wrong.GetServerCertPem())
}

// TestDeviceRemoteSelfSignedCertClientPinMismatch verifies the listener rejects a
// client presenting a certificate other than the pinned one (mTLS).
func TestDeviceRemoteSelfSignedCertClientPinMismatch(t *testing.T) {
	km, err := GenerateDeviceRpcKeyMaterial()
	assert.Equal(t, err, nil)
	wrong, err := GenerateDeviceRpcKeyMaterial()
	assert.Equal(t, err, nil)

	// dialer presents the wrong client identity
	testing_mtlsPinMismatch(t, km.GetServerPem(), km.GetClientCertPem(), wrong.GetClientPem(), km.GetServerCertPem())
}

// TestDeviceRemoteSetRpcServerReset verifies that SetRpcServer swaps the
// transport at runtime and reconnects.
func TestDeviceRemoteSetRpcServerReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()

	keyMaterial, err := GenerateDeviceRpcKeyMaterial()
	assert.Equal(t, err, nil)

	hostPort := "127.0.0.1:12077"
	settings := defaultDeviceRpcSettings()

	// local with built-in rpc disabled; start a tls listener via SetRpcServer
	deviceLocal, err := newDeviceLocalWithOverrides(
		true,
		false,
		nil,
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		false,
		defaultDeviceLocalSettings(),
		clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceLocal.Close()

	err = deviceLocal.SetRpcServer(keyMaterial.GetServerPem(), keyMaterial.GetClientCertPem(), hostPort)
	assert.Equal(t, err, nil)

	// remote starts pointed at a dead port, so it cannot connect
	deadDialer := NewWebsocketDeviceRpcDialer(requireRemoteAddress("127.0.0.1:12099"), "", "", settings)
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		settings,
		clientId,
		deadDialer,
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()

	assert.Equal(t, deviceRemote.waitForSync(500*time.Millisecond), false)

	// reset the transport to the tls listener; this should connect
	err = deviceRemote.SetRpcServer(keyMaterial.GetClientPem(), keyMaterial.GetServerCertPem(), hostPort)
	assert.Equal(t, err, nil)
	assert.Equal(t, deviceRemote.waitForSync(5*time.Second), true)

	deviceRemote.SetOffline(false)
	assert.Equal(t, deviceRemote.GetOffline(), false)
	assert.Equal(t, deviceLocal.GetOffline(), false)
}

// TestDeviceLocalSetRpcServerRebind verifies that calling SetRpcServer again
// closes the old listener and binds a new one on the same port. A dialer using
// the new session's material connecting successfully proves the new listener
// (which pins the new client cert) is the one bound — the old listener pinned a
// different client cert and would reject it.
func TestDeviceLocalSetRpcServerRebind(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()

	hostPort := "127.0.0.1:12078"
	settings := defaultDeviceRpcSettings()

	deviceLocal, err := newDeviceLocalWithOverrides(
		true,
		false,
		nil,
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		false,
		defaultDeviceLocalSettings(),
		clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceLocal.Close()

	// bind once
	km1, err := GenerateDeviceRpcKeyMaterial()
	assert.Equal(t, err, nil)
	err = deviceLocal.SetRpcServer(km1.GetServerPem(), km1.GetClientCertPem(), hostPort)
	assert.Equal(t, err, nil)

	// rebind on the SAME port with fresh material
	km2, err := GenerateDeviceRpcKeyMaterial()
	assert.Equal(t, err, nil)
	err = deviceLocal.SetRpcServer(km2.GetServerPem(), km2.GetClientCertPem(), hostPort)
	assert.Equal(t, err, nil)

	// a remote using the new material must connect to the rebound listener
	dialer := NewWebsocketDeviceRpcDialer(requireRemoteAddress(hostPort), km2.GetClientPem(), km2.GetServerCertPem(), settings)
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		settings,
		clientId,
		dialer,
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()

	assert.Equal(t, deviceRemote.waitForSync(5*time.Second), true)
	deviceRemote.SetOffline(false)
	assert.Equal(t, deviceLocal.GetOffline(), false)
}

type testing_remoteChangeCounter struct {
	stateLock    sync.Mutex
	connectCount int
}

func (self *testing_remoteChangeCounter) RemoteChanged(remoteConnected bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if remoteConnected {
		self.connectCount++
	}
}

func (self *testing_remoteChangeCounter) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.connectCount
}

// TestDeviceRemoteStaysConnected verifies that once synced the session stays
// connected (no tight sync/reconnect loop).
func TestDeviceRemoteStaysConnected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()

	settings := defaultDeviceRpcSettings()

	deviceLocal, err := newDeviceLocalWithOverrides(
		true,
		false,
		nil,
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
		settings,
		clientId,
		testing_deviceRpcDialer(settings),
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()

	counter := &testing_remoteChangeCounter{}
	sub := deviceRemote.AddRemoteChangeListener(counter)
	defer sub.Close()

	deviceRemote.Sync()
	assert.Equal(t, deviceRemote.waitForSync(5*time.Second), true)

	// hold; a healthy session stays connected (one connect event), a tight loop
	// produces many
	select {
	case <-time.After(3 * time.Second):
	}

	c := counter.count()
	glog.Infof("[test]remote connect count = %d", c)
	assert.Equal(t, c <= 1, true)
}

// TestDeviceRpcSetRpcServerIdempotent verifies that re-applying the same rpc
// server (on both the remote dialer and the local listener) does not reset a
// live connection — i.e. no tight resync loop when the app re-applies the same
// transport on every state change.
func TestDeviceRpcSetRpcServerIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()

	hostPort := "127.0.0.1:12025"
	settings := defaultDeviceRpcSettings()

	deviceLocal, err := newDeviceLocalWithOverrides(
		true,
		false,
		nil,
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
		settings,
		clientId,
		testing_deviceRpcDialer(settings),
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()

	counter := &testing_remoteChangeCounter{}
	sub := deviceRemote.AddRemoteChangeListener(counter)
	defer sub.Close()

	deviceRemote.Sync()
	assert.Equal(t, deviceRemote.waitForSync(5*time.Second), true)

	// record the transport config on both sides (may rebind/reconnect once)
	assert.Equal(t, deviceLocal.SetRpcServer("", "", hostPort), nil)
	assert.Equal(t, deviceRemote.SetRpcServer("", "", hostPort), nil)
	deviceRemote.waitForSync(5 * time.Second)
	select {
	case <-time.After(1 * time.Second):
	}

	baseline := counter.count()

	// re-applying the same transport repeatedly must be a no-op (no reconnects)
	for range 10 {
		assert.Equal(t, deviceLocal.SetRpcServer("", "", hostPort), nil)
		assert.Equal(t, deviceRemote.SetRpcServer("", "", hostPort), nil)
		select {
		case <-time.After(100 * time.Millisecond):
		}
	}
	select {
	case <-time.After(500 * time.Millisecond):
	}

	glog.Infof("[test]connect count baseline=%d final=%d", baseline, counter.count())
	assert.Equal(t, counter.count(), baseline)
}

// TestDeviceRpcKeyMaterialStrings verifies the generated PEM strings are
// well-formed and complete an mTLS handshake when used verbatim — the app and
// extension carry them through unchanged, so all encoding lives in the SDK.
func TestDeviceRpcKeyMaterialStrings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()

	keyMaterial, err := GenerateDeviceRpcKeyMaterial()
	assert.Equal(t, err, nil)

	clientPem := keyMaterial.GetClientPem()
	clientCertPem := keyMaterial.GetClientCertPem()
	serverPem := keyMaterial.GetServerPem()
	serverCertPem := keyMaterial.GetServerCertPem()

	// the values are opaque PEM strings, carried verbatim by the app/extension
	assert.Equal(t, strings.Contains(serverCertPem, "BEGIN CERTIFICATE"), true)
	assert.Equal(t, strings.Contains(serverPem, "BEGIN CERTIFICATE"), true)
	assert.Equal(t, strings.Contains(serverPem, "PRIVATE KEY"), true)
	assert.Equal(t, strings.Contains(clientCertPem, "BEGIN CERTIFICATE"), true)
	assert.Equal(t, strings.Contains(clientPem, "PRIVATE KEY"), true)

	settings := defaultDeviceRpcSettings()

	deviceLocal, err := newDeviceLocalWithOverrides(
		true,
		false,
		nil,
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		false,
		defaultDeviceLocalSettings(),
		clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceLocal.Close()

	err = deviceLocal.SetRpcServer(serverPem, clientCertPem, settings.Address.HostPort())
	assert.Equal(t, err, nil)

	dialer := NewWebsocketDeviceRpcDialer(settings.Address, clientPem, serverCertPem, settings)
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		settings,
		clientId,
		dialer,
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()

	deviceRemote.Sync()
	assert.Equal(t, deviceRemote.waitForSync(5*time.Second), true)

	deviceRemote.SetOffline(false)
	assert.Equal(t, deviceRemote.GetOffline(), false)
	assert.Equal(t, deviceLocal.GetOffline(), false)
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

type testing_performanceProfileChangeListener struct {
	testing_listener
	performanceProfile *PerformanceProfile
}

func (self *testing_performanceProfileChangeListener) PerformanceProfileChanged(performanceProfile *PerformanceProfile) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.event = true
	self.performanceProfile = performanceProfile
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

func testing_deviceRpcDialer(settings *deviceRpcSettings) DeviceRpcDialer {
	return NewWebsocketDeviceRpcDialer(settings.Address, "", "", settings)
}

func testing_deviceRpcDialerDefault() DeviceRpcDialer {
	settings := defaultDeviceRpcSettings()
	return NewWebsocketDeviceRpcDialer(settings.Address, "", "", settings)
}
