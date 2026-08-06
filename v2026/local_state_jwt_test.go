package sdk

import (
	"context"
	"os"
	"testing"
)

// TestJwtPersistOrderKeepsClientJwt pins the ordering contract between
// LocalState.SetByJwt and LocalState.SetByClientJwt.
//
// SetByJwt CLEARS the client jwt and the instance id whenever the value changes --
// which is always true on a token refresh. So the order the two are written in
// decides whether .by_client_jwt survives. It must: the next cold launch reads it to
// decide the user is still logged in, and an empty one drops them at the login
// screen. DeviceLocal.SetByJwt / DeviceRemote.setByJwt had these backwards.
func TestJwtPersistOrderKeepsClientJwt(t *testing.T) {
	ctx := context.Background()

	// the CORRECT order, as DeviceLocal.SetByJwt / DeviceRemote.setByJwt now do it:
	// network jwt first (which may clear), then the client jwt.
	dir, err := os.MkdirTemp("", "jwtorder-fixed")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	localState := newLocalState(ctx, dir)

	localState.SetByJwt("CLIENT_JWT_v0")
	localState.SetByClientJwt("CLIENT_JWT_v0")
	if got := localState.GetByClientJwt(); got != "CLIENT_JWT_v0" {
		t.Fatalf("after login: by_client_jwt = %q, want CLIENT_JWT_v0", got)
	}

	// a refresh: /auth/refresh mints a NEW token for the same identity
	localState.SetByJwt("CLIENT_JWT_v1")
	localState.SetByClientJwt("CLIENT_JWT_v1")

	if got := localState.GetByClientJwt(); got != "CLIENT_JWT_v1" {
		t.Fatalf("after refresh: by_client_jwt = %q, want CLIENT_JWT_v1 -- the user would be logged out on next launch", got)
	}
	if localState.GetInstanceId() == nil {
		t.Fatal("after refresh: instance_id is nil -- the user would be logged out on next launch")
	}

	// and the REVERSED order (what the code used to do) destroys it -- this is the
	// bug, pinned so it cannot come back
	dir2, err := os.MkdirTemp("", "jwtorder-buggy")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir2)
	buggy := newLocalState(ctx, dir2)

	buggy.SetByClientJwt("CLIENT_JWT_v0")
	buggy.SetByJwt("CLIENT_JWT_v0")
	if got := buggy.GetByClientJwt(); got != "" {
		t.Fatalf("reversed order: by_client_jwt = %q, expected it to be WIPED (that was the bug)", got)
	}
}
