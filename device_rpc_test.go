package sdk

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/rpc"
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
	deviceLocalSettings := DefaultDeviceLocalSettings()
	deviceLocalSettings.Verbose = false
	deviceLocalSettings.EnableRpc = true
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		deviceLocalSettings,
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
				networkSpace,
				byJwt,
				"",
				"",
				"",
				instanceId,
				testDeviceLocalSettingsRpc(),
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
				networkSpace,
				byJwt,
				"",
				"",
				"",
				instanceId,
				testDeviceLocalSettingsRpc(),
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
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		testDeviceLocalSettingsRpc(),
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

	bodyBytes, err := deviceRemote.httpGetRaw(ctx, "http://74.50.11.113:8080/hello", "")
	assert.Equal(t, err, nil)
	assert.NotEqual(t, bodyBytes, nil)
	glog.Infof("response body=%s", string(bodyBytes))
	assert.NotEqual(t, len(bodyBytes), 0)

	// FIXME allow POST on the hello route
	// bodyBytes, err := deviceRemote.httpGetRaw(ctx, "http://74.50.11.113:8080/hello", "")
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
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		testDeviceLocalSettingsRpc(),
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
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		testDeviceLocalSettingsRpc(),
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
	deviceLocalSettings := DefaultDeviceLocalSettings()
	deviceLocalSettings.Verbose = false
	deviceLocalSettings.EnableRpc = true
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		deviceLocalSettings,
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
	deviceLocalSettings := DefaultDeviceLocalSettings()
	deviceLocalSettings.Verbose = false
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		deviceLocalSettings,
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

	deviceLocalSettings := DefaultDeviceLocalSettings()
	deviceLocalSettings.Verbose = false
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		deviceLocalSettings,
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
	deviceLocalSettings := DefaultDeviceLocalSettings()
	deviceLocalSettings.Verbose = false
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		deviceLocalSettings,
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

	deviceLocalSettings := DefaultDeviceLocalSettings()
	deviceLocalSettings.Verbose = false
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		deviceLocalSettings,
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
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		testDeviceLocalSettingsRpc(),
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
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		testDeviceLocalSettingsRpc(),
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

	deviceLocalSettings := DefaultDeviceLocalSettings()
	deviceLocalSettings.Verbose = false
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		deviceLocalSettings,
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

// rpc test device settings: rpc enabled, security policy monitor off
func testDeviceLocalSettingsRpc() *DeviceLocalSettings {
	settings := DefaultDeviceLocalSettings()
	settings.Verbose = false
	settings.EnableRpc = true
	return settings
}

// TestDeviceLocalRpcReverseCoalesceLastValue exercises the reverse-notification
// queue directly. While one delivery is stuck — standing in for a disconnected /
// suspended remote that has stopped reading — a burst of updates must coalesce
// per key to the latest value, keep the queue bounded, and never block the
// producer. When delivery resumes, each key is delivered once, in first-seen
// order, carrying the latest value.
func TestDeviceLocalRpcReverseCoalesceLastValue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rpc := &DeviceLocalRpc{
		ctx:         ctx,
		sendPending: map[string]func(){},
		sendSignal:  make(chan struct{}, 1),
	}
	go rpc.sendLoop()

	var mu sync.Mutex
	var statusVals, provideVals []int
	recordStatus := func(v int) func() {
		return func() {
			mu.Lock()
			statusVals = append(statusVals, v)
			mu.Unlock()
		}
	}
	recordProvide := func(v int) func() {
		return func() {
			mu.Lock()
			provideVals = append(provideVals, v)
			mu.Unlock()
		}
	}

	// the gate op parks the send loop, simulating a remote that has stopped
	// reading; everything enqueued behind it must coalesce
	entered := make(chan struct{})
	release := make(chan struct{})
	rpc.enqueueReverse("gate", func() {
		close(entered)
		<-release
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("send loop never started the gate delivery")
	}

	// fire a burst for two keys while delivery is stuck
	const n = 100
	for i := 0; i < n; i++ {
		rpc.enqueueReverse("status", recordStatus(i))
		rpc.enqueueReverse("provide", recordProvide(i))
	}

	// the queue holds exactly one coalesced op per key, in first-seen order
	rpc.sendMu.Lock()
	pendingLen := len(rpc.sendPending)
	order := append([]string{}, rpc.sendOrder...)
	rpc.sendMu.Unlock()
	assert.Equal(t, pendingLen, 2)
	assert.Equal(t, order, []string{"status", "provide"})

	// resume delivery; only the latest value for each key is delivered
	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		sv := append([]int{}, statusVals...)
		pv := append([]int{}, provideVals...)
		mu.Unlock()
		if len(sv) == 1 && len(pv) == 1 {
			assert.Equal(t, sv, []int{n - 1})
			assert.Equal(t, pv, []int{n - 1})
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected one coalesced delivery per key, got status=%v provide=%v", sv, pv)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDeviceLocalRpcReverseNotifyKeys verifies the real notification methods
// coalesce by their rpc method name: repeated updates collapse to a single
// queued entry per method, in first-seen order.
func TestDeviceLocalRpcReverseNotifyKeys(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rpc := &DeviceLocalRpc{
		ctx:         ctx,
		sendPending: map[string]func(){},
		sendSignal:  make(chan struct{}, 1),
	}
	// no send loop is started: inspect the coalesced queue directly

	for i := 0; i < 10; i++ {
		rpc.tunnelChanged(i%2 == 0)
		rpc.provideChanged(i%2 == 0)
		rpc.connectChanged(i%2 == 0)
		rpc.routeLocalChanged(i%2 == 0)
	}

	rpc.sendMu.Lock()
	order := append([]string{}, rpc.sendOrder...)
	pendingLen := len(rpc.sendPending)
	rpc.sendMu.Unlock()

	assert.Equal(t, order, []string{
		"DeviceRemoteRpc.TunnelChanged",
		"DeviceRemoteRpc.ProvideChanged",
		"DeviceRemoteRpc.ConnectChanged",
		"DeviceRemoteRpc.RouteLocalChanged",
	})
	assert.Equal(t, pendingLen, 4)
}

// TestDeviceLocalRpcWindowMonitorMerge verifies window monitor events coalesce
// by merging rather than replacing: provider events are last-value per clientId
// (no transition dropped), window ids union, the expand event is last-value, and
// a reset replaces the accumulated state.
func TestDeviceLocalRpcWindowMonitorMerge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rpc := &DeviceLocalRpc{
		ctx:         ctx,
		sendPending: map[string]func(){},
		sendSignal:  make(chan struct{}, 1),
	}

	w1 := connect.NewId()
	w2 := connect.NewId()
	cA := connect.NewId()
	cB := connect.NewId()
	expand1 := &connect.WindowExpandEvent{TargetSize: 4}
	expand2 := &connect.WindowExpandEvent{TargetSize: 7}

	// first (non-reset) event seeds the accumulator
	rpc.enqueueWindowMonitorEvent(&DeviceRemoteWindowMonitorEvent{
		WindowIds:         map[connect.Id]bool{w1: true},
		WindowExpandEvent: expand1,
		ProviderEvents: map[connect.Id]*connect.ProviderEvent{
			cA: &connect.ProviderEvent{ClientId: cA, State: connect.ProviderStateInEvaluation},
		},
	})
	// second event merges: new clientId, new window id, newer expand
	rpc.enqueueWindowMonitorEvent(&DeviceRemoteWindowMonitorEvent{
		WindowIds:         map[connect.Id]bool{w2: true},
		WindowExpandEvent: expand2,
		ProviderEvents: map[connect.Id]*connect.ProviderEvent{
			cB: &connect.ProviderEvent{ClientId: cB, State: connect.ProviderStateInEvaluation},
		},
	})
	// third event updates cA to a newer state (last value per clientId wins)
	rpc.enqueueWindowMonitorEvent(&DeviceRemoteWindowMonitorEvent{
		WindowIds:         map[connect.Id]bool{w1: true},
		WindowExpandEvent: expand2,
		ProviderEvents: map[connect.Id]*connect.ProviderEvent{
			cA: &connect.ProviderEvent{ClientId: cA, State: connect.ProviderStateAdded},
		},
	})

	rpc.sendMu.Lock()
	merged := rpc.sendWindowMonitorEvent
	order := append([]string{}, rpc.sendOrder...)
	pendingLen := len(rpc.sendPending)
	rpc.sendMu.Unlock()

	assert.Equal(t, order, []string{"DeviceRemoteRpc.WindowMonitorEventCallback"})
	assert.Equal(t, pendingLen, 1)
	if merged == nil {
		t.Fatal("expected a merged window monitor event")
	}
	assert.Equal(t, merged.Reset, false)
	assert.Equal(t, merged.WindowExpandEvent, expand2)
	assert.Equal(t, merged.WindowIds, map[connect.Id]bool{w1: true, w2: true})
	assert.Equal(t, len(merged.ProviderEvents), 2)
	assert.Equal(t, merged.ProviderEvents[cA].State, connect.ProviderStateAdded)
	assert.Equal(t, merged.ProviderEvents[cB].State, connect.ProviderStateInEvaluation)

	// a reset event discards the accumulated state
	cC := connect.NewId()
	rpc.enqueueWindowMonitorEvent(&DeviceRemoteWindowMonitorEvent{
		WindowIds:         map[connect.Id]bool{w2: true},
		WindowExpandEvent: expand1,
		ProviderEvents: map[connect.Id]*connect.ProviderEvent{
			cC: &connect.ProviderEvent{ClientId: cC, State: connect.ProviderStateInEvaluation},
		},
		Reset: true,
	})

	rpc.sendMu.Lock()
	merged = rpc.sendWindowMonitorEvent
	rpc.sendMu.Unlock()
	if merged == nil {
		t.Fatal("expected a window monitor event after reset")
	}
	assert.Equal(t, merged.Reset, true)
	assert.Equal(t, merged.WindowIds, map[connect.Id]bool{w2: true})
	assert.Equal(t, len(merged.ProviderEvents), 1)
	assert.Equal(t, merged.ProviderEvents[cC].State, connect.ProviderStateInEvaluation)
}

type testing_blockingRouteLocalListener struct {
	stateLock sync.Mutex
	count     int
	last      bool
	blocked   bool
	entered   chan struct{}
	release   chan struct{}
}

func (self *testing_blockingRouteLocalListener) RouteLocalChanged(routeLocal bool) {
	self.stateLock.Lock()
	self.count += 1
	self.last = routeLocal
	first := !self.blocked
	self.blocked = true
	self.stateLock.Unlock()

	// the first callback parks here, simulating a remote that has stopped
	// draining reverse callbacks (suspended / disconnected app)
	if first {
		close(self.entered)
		<-self.release
	}
}

// TestDeviceLocalRpcReverseStalledRemote drives a real remote into a stalled
// state (its callback drain is parked in a listener) and fires a burst of state
// changes on the local device. Because reverse delivery is async and coalescing,
// the local setters must not block on the stalled remote, and once the remote
// resumes it must converge to the final value having seen far fewer events than
// were fired.
func TestDeviceLocalRpcReverseStalledRemote(t *testing.T) {
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
		networkSpace, byJwt, "", "", "", instanceId, testDeviceLocalSettingsRpc(), clientId,
	)
	if err != nil {
		panic(err)
	}
	defer deviceLocal.Close()

	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace, byJwt, instanceId, settings, clientId, testing_deviceRpcDialer(settings),
	)
	if err != nil {
		panic(err)
	}
	defer deviceRemote.Close()

	listener := &testing_blockingRouteLocalListener{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(listener.release) }) }
	defer doRelease()

	sub := deviceRemote.AddRouteLocalChangeListener(listener)
	defer sub.Close()

	deviceRemote.Sync()
	assert.Equal(t, deviceRemote.waitForSync(5*time.Second), true)

	// force a change to drive the remote listener, then wait until it is parked
	initial := deviceLocal.GetRouteLocal()
	final := !initial
	deviceLocal.SetRouteLocal(final)
	select {
	case <-listener.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("remote listener never received the initial update")
	}

	// with the remote stalled, fire a burst. each setter only enqueues a reverse
	// notification, so these must return promptly rather than block on the remote.
	// (the old synchronous path would block the setter on the stalled remote.)
	const burst = 100
	start := time.Now()
	for i := 0; i < burst; i++ {
		deviceLocal.SetRouteLocal(initial)
		deviceLocal.SetRouteLocal(final)
	}
	elapsed := time.Since(start)
	assert.Equal(t, elapsed < time.Second, true)

	// resume the remote and let it drain
	doRelease()
	time.Sleep(2 * time.Second)

	listener.stateLock.Lock()
	last := listener.last
	count := listener.count
	listener.stateLock.Unlock()

	// converged to the final value, and coalescing collapsed the burst far below
	// the number of updates fired (1 initial + 2*burst)
	assert.Equal(t, last, final)
	assert.Equal(t, count < 1+2*burst, true)
}

// testing_blockingReverseRpc stands in for the app side of the reverse rpc. The
// first Notify call parks until released — a slow but live app that has not
// drained the call — while later calls return immediately.
type testing_blockingReverseRpc struct {
	enteredOnce *sync.Once
	entered     chan struct{}
	release     chan struct{}
}

func (self *testing_blockingReverseRpc) Notify(_ bool, _ RpcVoid) error {
	first := false
	self.enteredOnce.Do(func() {
		first = true
		close(self.entered)
	})
	if first {
		<-self.release
	}
	return nil
}

// testing_newReverseService wires a DeviceLocalRpc whose reverse service is a
// real net/rpc client to server over an in-memory pipe. Returns the rpc, the
// service, and the local conn (close it to simulate the connection breaking).
func testing_newReverseService(t *testing.T, ctx context.Context, server *testing_blockingReverseRpc) (*DeviceLocalRpc, *rpcClientWithTimeout, net.Conn) {
	localConn, appConn := net.Pipe()
	t.Cleanup(func() { localConn.Close() })
	t.Cleanup(func() { appConn.Close() })

	appServer := rpc.NewServer()
	if err := appServer.RegisterName("DeviceRemoteRpc", server); err != nil {
		t.Fatal(err)
	}
	go appServer.ServeConn(appConn)

	settings := defaultDeviceRpcSettings()
	service := &rpcClientWithTimeout{
		ctx:         ctx,
		log:         settings.logger(),
		timeout:     settings.RpcCallTimeout, // unused: reverse delivery blocks
		closeClient: localConn.Close,
		client:      rpc.NewClient(localConn),
	}
	localRpc := &DeviceLocalRpc{
		ctx:     ctx,
		service: service,
	}
	return localRpc, service, localConn
}

// TestDeviceLocalRpcReverseBlocksAsBackPressure asserts that a reverse delivery
// to a slow but live app BLOCKS (back pressure) instead of failing, and does not
// tear down the reverse rpc. Failing here is the root cause of the extra tunnel
// disconnects: tearing down closes reverseConn, which unblocks the app's
// ServeConn, fires tunnelChanged(false), and forces a reconnect. A transiently
// slow app (the connection is fine, only the app's callback drain is parked) must
// apply back pressure, not flap the tunnel.
func TestDeviceLocalRpcReverseBlocksAsBackPressure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := &testing_blockingReverseRpc{
		enteredOnce: &sync.Once{},
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	var releaseOnce sync.Once
	doRelease := func() { releaseOnce.Do(func() { close(server.release) }) }
	defer doRelease()

	localRpc, service, _ := testing_newReverseService(t, ctx, server)

	// deliver on a goroutine; the app parks in the handler, so this must block
	done := make(chan struct{})
	go func() {
		localRpc.reverseCall("DeviceRemoteRpc.Notify", true)
		close(done)
	}()

	select {
	case <-server.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("app reverse handler never received the call")
	}

	// while the app is parked the delivery blocks (back pressure) and does NOT
	// tear down — a transiently slow app must not flap the tunnel
	select {
	case <-done:
		t.Fatal("reverseCall returned while the app was parked (should block as back pressure)")
	case <-time.After(500 * time.Millisecond):
	}
	localRpc.stateLock.Lock()
	serviceWhileBlocked := localRpc.service
	localRpc.stateLock.Unlock()
	if serviceWhileBlocked != service {
		t.Fatal("reverse service was torn down while merely blocked on a slow app")
	}

	// once the app drains, the delivery completes — still no teardown
	doRelease()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reverseCall did not complete after the app drained")
	}
	localRpc.stateLock.Lock()
	serviceAfter := localRpc.service
	localRpc.stateLock.Unlock()
	if serviceAfter != service {
		t.Fatal("reverse service was torn down after a successful delivery")
	}
}

// TestDeviceLocalRpcReverseTearsDownOnTransportError asserts the other half: a
// real transport error (a dead/suspended app reaped by the keepalive, modeled by
// the connection breaking under the blocked call) DOES tear down the reverse rpc
// so the tunnel reconnects. Blocking sheds transient slowness; only a dead tunnel
// reconnects.
func TestDeviceLocalRpcReverseTearsDownOnTransportError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := &testing_blockingReverseRpc{
		enteredOnce: &sync.Once{},
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	defer func() {
		// release any parked handler goroutine at cleanup
		var once sync.Once
		once.Do(func() { close(server.release) })
	}()

	localRpc, _, localConn := testing_newReverseService(t, ctx, server)

	done := make(chan struct{})
	go func() {
		localRpc.reverseCall("DeviceRemoteRpc.Notify", true)
		close(done)
	}()

	select {
	case <-server.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("app reverse handler never received the call")
	}

	// the connection breaks under the blocked call
	localConn.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reverseCall did not return after the connection broke")
	}
	localRpc.stateLock.Lock()
	serviceAfter := localRpc.service
	localRpc.stateLock.Unlock()
	if serviceAfter != nil {
		t.Fatal("reverse service was not torn down on a real transport error")
	}
}

// testing_newSyncedDeviceLocalRemote builds a local+remote pair over the real rpc
// transport and waits until the remote has synced (so the remote routes http
// through the rpc, not the local fallback). Both are closed via t.Cleanup.
func testing_newSyncedDeviceLocalRemote(t *testing.T, ctx context.Context) (*DeviceLocal, *DeviceRemote) {
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatal(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()
	settings := defaultDeviceRpcSettings()

	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, byJwt, "", "", "", instanceId, testDeviceLocalSettingsRpc(), clientId,
	)
	if err != nil {
		t.Fatal(err)
	}

	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace, byJwt, instanceId, settings, clientId, testing_deviceRpcDialer(settings),
	)
	if err != nil {
		deviceLocal.Close()
		t.Fatal(err)
	}

	t.Cleanup(func() {
		deviceRemote.Close()
		deviceLocal.Close()
	})

	deviceRemote.Sync()
	if !deviceRemote.waitForSync(5 * time.Second) {
		t.Fatal("device remote did not sync")
	}
	if !deviceRemote.GetRemoteConnected() {
		t.Fatal("device remote is not connected after sync (http would not route through rpc)")
	}
	return deviceLocal, deviceRemote
}

// TestDeviceRpcHttpOverRpc exercises the full http-over-rpc round trip: the remote
// forwards the request to the local (DeviceLocalRpc.HttpPostRaw / HttpGetRaw), the
// local performs the http call, and the body is returned to the remote via the
// reverse DeviceRemoteRpc.HttpResponse. Covers get, post (with a body), and a
// non-200 error.
func TestDeviceRpcHttpOverRpc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("boom"))
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(r.Method + ":" + r.URL.Path + ":" + string(body)))
	}))
	defer ts.Close()

	_, deviceRemote := testing_newSyncedDeviceLocalRemote(t, ctx)

	getBody, err := deviceRemote.httpGetRaw(ctx, ts.URL+"/ok", "")
	assert.Equal(t, err, nil)
	assert.Equal(t, string(getBody), "GET:/ok:")

	postBody, err := deviceRemote.httpPostRaw(ctx, ts.URL+"/ok", []byte("payload"), "")
	assert.Equal(t, err, nil)
	assert.Equal(t, string(postBody), "POST:/ok:payload")

	// a non-200 is surfaced as an error carried back over the reverse rpc
	_, err = deviceRemote.httpGetRaw(ctx, ts.URL+"/fail", "")
	assert.NotEqual(t, err, nil)
}

// TestDeviceRpcHttpOverRpcConcurrent fires many http requests at once. Each
// response is delivered directly on its own goroutine (not via the coalescing
// state-notification queue), so this checks that concurrent responses are routed
// back to the correct caller by request id with no cross-talk.
func TestDeviceRpcHttpOverRpcConcurrent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// echo the id query param so each response is unique to its request
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(r.URL.Query().Get("id")))
	}))
	defer ts.Close()

	_, deviceRemote := testing_newSyncedDeviceLocalRemote(t, ctx)

	const n = 50
	results := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("%d", i)
			url := ts.URL + "?id=" + id
			var body []byte
			var err error
			if i%2 == 0 {
				body, err = deviceRemote.httpGetRaw(ctx, url, "")
			} else {
				body, err = deviceRemote.httpPostRaw(ctx, url, []byte(id), "")
			}
			results[i] = string(body)
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		assert.Equal(t, errs[i], nil)
		// each request got its own response, not another request's
		assert.Equal(t, results[i], fmt.Sprintf("%d", i))
	}
}

// testing_httpAndStateReverseRpc is an app-side reverse server whose state
// notification (TunnelChanged) parks until released, while HttpResponse always
// returns immediately and records what it received.
type testing_httpAndStateReverseRpc struct {
	tunnelOnce    *sync.Once
	tunnelEntered chan struct{}
	release       chan struct{}
	httpReceived  chan *DeviceRemoteHttpResponse
}

func (self *testing_httpAndStateReverseRpc) TunnelChanged(_ bool, _ RpcVoid) error {
	self.tunnelOnce.Do(func() { close(self.tunnelEntered) })
	<-self.release
	return nil
}

func (self *testing_httpAndStateReverseRpc) HttpResponse(httpResponse *DeviceRemoteHttpResponse, _ RpcVoid) error {
	self.httpReceived <- httpResponse
	return nil
}

// TestDeviceLocalRpcHttpResponseNotBlockedByStuckNotification asserts the split:
// http responses are delivered directly (see HttpPostRaw / HttpGetRaw), not via
// sendLoop, so a state notification stuck in the coalescing queue does not
// head-of-line block an http response (and vice versa). With sendLoop blocked
// (back pressure) inside a state-notification delivery to a parked app, a
// directly delivered http response must still arrive promptly.
func TestDeviceLocalRpcHttpResponseNotBlockedByStuckNotification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	localConn, appConn := net.Pipe()
	defer localConn.Close()
	defer appConn.Close()

	server := &testing_httpAndStateReverseRpc{
		tunnelOnce:    &sync.Once{},
		tunnelEntered: make(chan struct{}),
		release:       make(chan struct{}),
		httpReceived:  make(chan *DeviceRemoteHttpResponse, 1),
	}
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(server.release) })

	appServer := rpc.NewServer()
	if err := appServer.RegisterName("DeviceRemoteRpc", server); err != nil {
		t.Fatal(err)
	}
	go appServer.ServeConn(appConn)

	settings := defaultDeviceRpcSettings()
	service := &rpcClientWithTimeout{
		ctx:         ctx,
		log:         settings.logger(),
		timeout:     settings.RpcCallTimeout, // unused: reverse delivery blocks
		closeClient: localConn.Close,
		client:      rpc.NewClient(localConn),
	}
	defer service.Close()

	localRpc := &DeviceLocalRpc{
		ctx:         ctx,
		service:     service,
		sendPending: map[string]func(){},
		sendSignal:  make(chan struct{}, 1),
	}
	go localRpc.sendLoop()

	// park sendLoop inside a state-notification delivery (the app handler blocks)
	localRpc.reverseNotify("DeviceRemoteRpc.TunnelChanged", true)
	select {
	case <-server.tunnelEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("sendLoop never delivered the state notification")
	}

	// with sendLoop stuck, an http response delivered directly must still arrive
	httpResponse := newDeviceRemoteHttpResponse(connect.NewId(), []byte("ok"), nil)
	start := time.Now()
	localRpc.reverseCall("DeviceRemoteRpc.HttpResponse", httpResponse)
	elapsed := time.Since(start)

	select {
	case got := <-server.httpReceived:
		assert.Equal(t, got.RequestId, httpResponse.RequestId)
		assert.Equal(t, string(got.BodyBytes), "ok")
	case <-time.After(time.Second):
		t.Fatal("http response was not delivered while a state notification was stuck in sendLoop")
	}
	// delivered promptly, i.e. it did not wait behind the state notification that
	// is blocking sendLoop
	assert.Equal(t, elapsed < time.Second, true)
}

// testing_panicRpc has a served method that panics, to exercise the forward
// serve loop's panic recovery.
type testing_panicRpc struct {
	pinged chan struct{}
}

func (self *testing_panicRpc) Panic(_ bool, _ RpcVoid) error {
	panic("boom")
}

func (self *testing_panicRpc) Ping(_ bool, _ RpcVoid) error {
	select {
	case self.pinged <- struct{}{}:
	default:
	}
	return nil
}

// TestDeviceLocalRpcServedPanicRecovered mirrors run()'s serial serve loop and
// asserts that a panic in a served method does NOT crash the process (net/rpc
// does not recover handler panics — without the HandleError wrapper this test
// binary would crash) and that serving continues with later calls.
func TestDeviceLocalRpcServedPanicRecovered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	server := rpc.NewServer()
	srv := &testing_panicRpc{pinged: make(chan struct{}, 1)}
	if err := server.RegisterName("T", srv); err != nil {
		t.Fatal(err)
	}

	// the exact serve loop from DeviceLocalRpc.run()
	go func() {
		codec := newGobServerCodec(serverConn)
		for ctx.Err() == nil {
			var serveErr error
			connect.HandleError(func() {
				serveErr = server.ServeRequest(codec)
			})
			if serveErr != nil {
				return
			}
		}
	}()

	client := rpc.NewClient(clientConn)
	defer client.Close()

	var void RpcVoid
	// fire the panicking call; the handler panics before writing a response, so
	// this call never completes — we only need the server to have processed it
	client.Go("T.Panic", true, &void, make(chan *rpc.Call, 1))

	// serving must continue after the recovered panic: a later call still succeeds
	done := make(chan error, 1)
	go func() { done <- client.Call("T.Ping", true, &void) }()
	select {
	case err := <-done:
		assert.Equal(t, err, nil)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not continue serving after a recovered handler panic")
	}
	select {
	case <-srv.pinged:
	case <-time.After(time.Second):
		t.Fatal("ping handler did not run")
	}
}
