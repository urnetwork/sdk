//go:build !ios_extension && !js

package sdk

import (
	"context"
	"testing"

	"github.com/urnetwork/sidecar"
)

// The sidecar role's user root is the host table's (QD-1407; §12.4), whichever network space names its urls, as ur's
// daemon takes it, so the core resolves and files network labels under the root the server draws them under.
func TestSidecarUserRootIsTheHostTables(t *testing.T) {
	list, err := sidecar.SidecarHostListV1()
	if err != nil {
		t.Fatal(err)
	}
	if list.UserRoot == "" {
		t.Fatal("the host table names no user root")
	}
	space := newNetworkSpace(context.Background(), *NewNetworkSpaceKey("user-root.test", "test"),
		NetworkSpaceValues{ApiUrl: "https://api.sdktest.example", PlatformUrl: "wss://connect.sdktest.example"}, t.TempDir())
	defer space.close()
	settings := sidecar.DefaultSidecarSettings()
	if err := applySidecarServiceUrls(space, settings); err != nil {
		t.Fatal(err)
	}
	if settings.UserRoot != list.UserRoot {
		t.Fatal("the sidecar role's user root is not the host table's")
	}
}
