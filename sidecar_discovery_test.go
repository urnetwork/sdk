//go:build !ios_extension && !js

package sdk

// GetDiscovery (§18.5; owner decision 778): nil without a daemon; List, Share and Set calling the daemon's discovery
// methods, a share and an unhide as the person's confirmed acts and the rest not; the method names the root's.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// A remote backend recording each call and answering a fixed reply.
type testDiscoveryRemote struct {
	testSidecarRemote
	stateLock sync.Mutex
	calls     []testDiscoveryCall
}

type testDiscoveryCall struct {
	method    string
	params    map[string]any
	confirmed bool
}

func (self *testDiscoveryRemote) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	data, _ := json.Marshal(params)
	decoded := map[string]any{}
	json.Unmarshal(data, &decoded)
	self.calls = append(self.calls, testDiscoveryCall{method: method, params: decoded, confirmed: confirmed})
	if result != nil {
		return json.Unmarshal([]byte(`{"state":"waiting","approval_pending_id":"01970000-0000-7000-8000-000000000001","own":true}`), result)
	}
	return nil
}

// A bytes callback's one result.
func testDiscoveryBytesCallback() (SidecarBytesCallback, chan *SidecarBytes) {
	results := make(chan *SidecarBytes, 1)
	return connect.NewApiCallback[*SidecarBytes](func(result *SidecarBytes, err error) {
		if err != nil {
			result = &SidecarBytes{}
		}
		results <- result
	}), results
}

func TestSidecarDiscoveryCallsTheDaemon(t *testing.T) {
	if (&Sidecar{}).GetDiscovery() != nil {
		t.Fatal("a Sidecar with no daemon has a discovery view")
	}
	remote := &testDiscoveryRemote{}
	discovery := &SidecarDiscovery{ctx: context.Background(), remote: remote, settings: defaultSidecarDiscoverySettings()}
	bytesCallback, bytesResults := testDiscoveryBytesCallback()
	discovery.List(bytesCallback)
	if listed := <-bytesResults; !listed.Ok || len(listed.Data) == 0 {
		t.Fatalf("the listing: %+v", listed)
	}
	callback, results := testSidecarResultCallback()
	discovery.Share("synthetic-phone", true, callback)
	if shared := <-results; !shared.Ok || shared.Value != "approval_pending:01970000-0000-7000-8000-000000000001" {
		t.Fatalf("the share: %+v", shared)
	}
	callback, results = testSidecarResultCallback()
	discovery.Set(SidecarDiscoveryUnhide, "445", callback)
	<-results
	callback, results = testSidecarResultCallback()
	discovery.Set(SidecarDiscoveryOwn, SidecarDiscoveryOff, callback)
	<-results
	callback, results = testSidecarResultCallback()
	discovery.Set("color", "on", callback)
	if refused := <-results; refused.Ok || refused.Code != SidecarCodeInvalidArgument {
		t.Fatalf("an unknown setting: %+v", refused)
	}
	want := []testDiscoveryCall{
		{method: sidecar.DaemonMethodBridgeDevices},
		{method: sidecar.DaemonMethodBridgeShare, confirmed: true},
		{method: sidecar.DaemonMethodBridgeDiscoveryUnhide, confirmed: true},
		{method: sidecar.DaemonMethodBridgeDiscovery},
	}
	if len(remote.calls) != len(want) {
		t.Fatalf("%d calls", len(remote.calls))
	}
	for i, call := range want {
		if remote.calls[i].method != call.method || remote.calls[i].confirmed != call.confirmed {
			t.Errorf("call %d: %+v, want %+v", i, remote.calls[i], call)
		}
	}
	if remote.calls[1].params["device"] != "synthetic-phone" || remote.calls[1].params["unshare"] != false ||
		remote.calls[2].params["setting"] != SidecarDiscoveryHide || remote.calls[2].params["value"] != "445" {
		t.Errorf("params %+v %+v", remote.calls[1].params, remote.calls[2].params)
	}
}
