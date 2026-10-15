//go:build !js

package sdk

// This file verifies the native hosted-rpc policy that differs from the
// browser build.

import (
	"testing"
)

// Native websocket progress does not depend on the caller yielding an event
// loop, so platform remotes retain synchronous reads and cross-remote state
// visibility.
func TestNativePlatformDeviceRpcUsesSynchronousState(t *testing.T) {
	networkSpace, byJwt, err := testing_newNetworkSpace(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	deviceRemote, err := NewPlatformDeviceRemote(
		networkSpace,
		byJwt,
		"ws://127.0.0.1:1",
		"test-signed-proxy-id",
		NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceRemote.Close()

	if deviceRemote.settings.BrowserStateOnly {
		t.Fatal("native platform remote was restricted to browser cached state")
	}
}
