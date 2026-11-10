//go:build !ios_extension && !js

package sdk

// SetPushToken's refusals (SDK-M8-01): a platform or token the route does not take, a sidecar with no enrolled
// client and a remote backend are each refused before anything is sent, and no result names the token. The post on
// the child's token is TestEnrollWithDeviceHandsTheSessionTheCoreSigner's.

import (
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
)

func TestSetPushTokenRefusesBeforeSending(t *testing.T) {
	world := newTestSidecarWorld(t)
	coreServer, coreUrl := newTestCoreServer(t)
	local := openTestEnrollingSidecar(t, world, coreUrl)
	const token = "push-token-canary-refused"
	for _, c := range []struct {
		platform string
		token    string
	}{
		{platform: "gcm", token: token},
		{platform: SidecarPushPlatformApns, token: "has a space"},
		{platform: "gcm", token: ""},
		// valid, but nothing is enrolled to post for
		{platform: SidecarPushPlatformFcm, token: token},
		{platform: "", token: ""},
	} {
		callback, results := testSidecarResultCallback()
		local.SetPushToken(c.platform, c.token, callback)
		result := <-results
		if result.Ok || strings.Contains(result.Message, token) {
			t.Errorf("%q %q: %+v", c.platform, c.token, result)
		}
	}
	for _, request := range coreServer.seen() {
		if request.path == "/sidecar/push-token" {
			t.Fatal("a push token was posted with nothing enrolled")
		}
	}
	if local.GetState() != string(sidecar.SidecarStateUnenrolled) {
		t.Fatal("a refused push token moved the state")
	}

	remote := &Sidecar{remote: &testSidecarRemote{}}
	callback, results := testSidecarResultCallback()
	remote.SetPushToken(SidecarPushPlatformApns, token, callback)
	if result := <-results; result.Ok {
		t.Fatalf("a remote backend took an app's token: %+v", result)
	}
}
