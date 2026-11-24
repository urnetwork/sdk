package sdk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A test's network space, on storage of its own, which ends with the test. ctx
// cancels the space as it would any space. The cleanup this registers then
// closes the space, which joins its api, its extender workers, its local
// state's worker and its client strategy, and deletes its storage once nothing
// the space ran can still write there, so neither the space's goroutines nor
// the deletion run on into the next test. A deletion left to ctx ran in a
// goroutine nothing waited for, beside the space's own writers still winding
// down, and never ran for a ctx that outlived the test. The cleanup is
// registered before any device the test builds on the space registers its
// own, so the devices are joined first. A space that does not join, or storage
// that is not deleted, fails the test.
func testing_newNetworkSpace(t testing.TB, ctx context.Context) (networkSpace *NetworkSpace, byJwt string, returnErr error) {
	t.Helper()
	key := NetworkSpaceKey{
		HostName: "test",
		EnvName:  "test",
	}
	values := NetworkSpaceValues{
		Bundled:                  true,
		NetExposeServerIps:       true,
		NetExposeServerHostNames: true,
	}
	storagePath, err := os.MkdirTemp("", "networkspace")
	if err != nil {
		returnErr = err
		return
	}

	networkSpace = newNetworkSpace(
		ctx,
		key,
		values,
		storagePath,
	)
	t.Cleanup(func() {
		// a space still closing could write into the storage after it is gone
		if !testingCloseAndJoinSpace(t, networkSpace) {
			return
		}
		if err := os.RemoveAll(storagePath); err != nil {
			t.Errorf("the test's network space storage was not deleted: %v", err)
		}
	})
	byJwt = ""
	return
}

// Closes a test's space and waits for the close to end; the close joins the
// space's api, its extender workers, its local state's worker and its client
// strategy. A close that runs out fails the test, which then ends with the
// space still closing. Answers whether the close ended.
func testingCloseAndJoinSpace(t testing.TB, networkSpace *NetworkSpace) bool {
	t.Helper()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		networkSpace.Close()
	}()
	select {
	case <-closed:
		return true
	case <-time.After(30 * time.Second):
		t.Errorf("the test's network space did not join")
		return false
	}
}

// Closes something a test made, a device, an api or a sidecar, and waits for
// everything it runs to end, so none of it runs on into the next test. A join
// that runs out fails the test, which then ends with it still running.
func testingCloseAndJoin(t testing.TB, name string, closer interface {
	CloseAndWait(ctx context.Context) error
}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := closer.CloseAndWait(ctx); err != nil {
		t.Errorf("the test's %s did not join: %v", name, err)
	}
}

// A test's space ends with the test that made it, whether the context it was
// made from ends with the test or outlives it: once the subtest that made it
// returns, the space is closed, its local state's worker has ended and its
// storage is gone. The subtest boundary is the point. A context that outlives
// the test is the shape of a fixture made from a parent's context its subtests
// share: a deletion left to the context ran only once the parent's ended, so
// the space's storage and its local state's worker were still there when the
// subtest returned.
func TestTestingNetworkSpaceEndsWithItsTest(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	cases := []struct {
		name            string
		endsWithTheTest bool
	}{
		{name: "context ends with the test", endsWithTheTest: true},
		{name: "context outlives the test", endsWithTheTest: false},
	}
	for _, c := range cases {
		var networkSpace *NetworkSpace
		t.Run(c.name, func(t *testing.T) {
			ctx := parentCtx
			if c.endsWithTheTest {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(parentCtx)
				defer cancel()
			}
			var err error
			networkSpace, _, err = testing_newNetworkSpace(t, ctx)
			if err != nil {
				t.Fatalf("network space: %v", err)
			}
			// a file in the storage, which the deletion takes with it
			plantedPath := filepath.Join(networkSpace.storagePath, "planted")
			if err := os.WriteFile(plantedPath, []byte("planted"), 0o600); err != nil {
				t.Fatalf("plant a file in the space's storage: %v", err)
			}
		})
		if networkSpace == nil {
			t.Fatalf("%s: no network space was made", c.name)
		}

		closed := func() bool {
			networkSpace.stateLock.Lock()
			defer networkSpace.stateLock.Unlock()
			return networkSpace.closed
		}()
		if !closed {
			t.Errorf("%s: the space was still open after its test returned", c.name)
		}
		select {
		case <-networkSpace.asyncLocalState.done:
		default:
			t.Errorf("%s: the space's local state worker was still running after its test returned", c.name)
		}
		if _, err := os.Stat(networkSpace.storagePath); !os.IsNotExist(err) {
			t.Errorf("%s: the space's storage was still there after its test returned (stat: %v)", c.name, err)
		}
	}
}
