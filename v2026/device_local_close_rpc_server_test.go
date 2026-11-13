package sdk

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func testing_dialable(hostPort string) bool {
	conn, err := net.DialTimeout("tcp", hostPort, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func testing_waitDialable(hostPort string, want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if testing_dialable(hostPort) == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A host that keeps one device across sessions (the windows service keeps its
// provider running between tunnel sessions) closes the session's rpc listener
// without closing the device: the port stops accepting, the device keeps
// running, and a later SetRpcServer serves the next session.
func TestDeviceLocalCloseRpcServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		panic(err)
	}

	clientId := connect.NewId()
	instanceId := NewId()

	hostPort := testing_freeHostPort()
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

	// closing with no server is a no-op
	deviceLocal.CloseRpcServer()

	km1, err := GenerateDeviceRpcKeyMaterial()
	connect.AssertEqual(t, err, nil)
	err = deviceLocal.SetRpcServer(km1.GetServerPem(), km1.GetClientCertPem(), hostPort)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, testing_waitDialable(hostPort, true, 5*time.Second), true)

	deviceLocal.CloseRpcServer()
	// the listener is released: nothing can reach the device over the rpc
	connect.AssertEqual(t, testing_waitDialable(hostPort, false, 5*time.Second), true)
	// and the device itself keeps running
	connect.AssertEqual(t, deviceLocal.GetDone(), false)
	deviceLocal.SetOffline(true)
	connect.AssertEqual(t, deviceLocal.GetOffline(), true)

	// a second close is a no-op
	deviceLocal.CloseRpcServer()
	connect.AssertEqual(t, deviceLocal.GetDone(), false)

	// the next session's server starts on the same device, and its remote syncs
	km2, err := GenerateDeviceRpcKeyMaterial()
	connect.AssertEqual(t, err, nil)
	err = deviceLocal.SetRpcServer(km2.GetServerPem(), km2.GetClientCertPem(), hostPort)
	connect.AssertEqual(t, err, nil)

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

	connect.AssertEqual(t, deviceRemote.waitForSync(5*time.Second), true)
	deviceRemote.SetOffline(false)
	connect.AssertEqual(t, deviceLocal.GetOffline(), false)

	// closing after close of the device is a no-op too
	deviceLocal.Close()
	deviceLocal.CloseRpcServer()
}
