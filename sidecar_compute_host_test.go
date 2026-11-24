//go:build !ios_extension && !js

package sdk

// The app's compute hosts (CSURF-M11-01, CONTRACTS C-39): each setter stores its host and replaces it, and hands it to
// the local core through its area's adapter once one has landed, never on the remote backend or a closed Sidecar.

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
)

// An app's local inference host that does nothing; its name tells one from another.
type testLocalInferenceHost struct{ name string }

func (self *testLocalInferenceHost) Capabilities() string { return "{}" }
func (self *testLocalInferenceHost) Load(modelPath string, settingsJson string) (int64, error) {
	return 0, ErrSidecarComputeNotBuilt
}
func (self *testLocalInferenceHost) Generate(handle int64, requestJson string, listener SidecarLocalInferenceListener) error {
	return ErrSidecarComputeNotBuilt
}
func (self *testLocalInferenceHost) Cancel(handle int64) {}
func (self *testLocalInferenceHost) Unload(handle int64) {}

// An app's classifier host that does nothing.
type testSafetyClassifierHost struct{ name string }

func (self *testSafetyClassifierHost) Load(modelPath string) (int64, error) {
	return 0, ErrSidecarComputeNotBuilt
}
func (self *testSafetyClassifierHost) Classify(handle int64, idsJson string) (string, error) {
	return "", ErrSidecarComputeNotBuilt
}
func (self *testSafetyClassifierHost) Unload(handle int64) {}

// An app's browser seam that does nothing.
type testConnectHost struct{ name string }

func (self *testConnectHost) Open(requestJson string) (string, error) {
	return "", ErrSidecarComputeNotBuilt
}
func (self *testConnectHost) Authorize(requestJson string) (string, error) {
	return "", ErrSidecarComputeNotBuilt
}

// What the adapters were handed, in order.
type testComputeHandOffs struct {
	stateLock sync.Mutex
	events    []string
	cores     []*sidecar.Sidecar
}

func (self *testComputeHandOffs) record(event string, local *sidecar.Sidecar) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, event)
	self.cores = append(self.cores, local)
}

func (self *testComputeHandOffs) all() ([]string, []*sidecar.Sidecar) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.events), slices.Clone(self.cores)
}

// Adapters that record what they are handed, in place of the areas' until the test ends.
func testRecordComputeHandOffs(t *testing.T) *testComputeHandOffs {
	t.Helper()
	handOffs := &testComputeHandOffs{}
	localInference, safetyClassifier, connect := sidecarLocalInferenceHostAdapter, sidecarSafetyClassifierHostAdapter, sidecarConnectHostAdapter
	t.Cleanup(func() {
		sidecarLocalInferenceHostAdapter, sidecarSafetyClassifierHostAdapter, sidecarConnectHostAdapter = localInference, safetyClassifier, connect
	})
	name := func(host any) string {
		switch typed := host.(type) {
		case *testLocalInferenceHost:
			return typed.name
		case *testSafetyClassifierHost:
			return typed.name
		case *testConnectHost:
			return typed.name
		}
		return "none"
	}
	sidecarLocalInferenceHostAdapter = func(local *sidecar.Sidecar, host SidecarLocalInferenceHost) {
		handOffs.record("inference "+name(host), local)
	}
	sidecarSafetyClassifierHostAdapter = func(local *sidecar.Sidecar, host SidecarSafetyClassifierHost) {
		handOffs.record("classifier "+name(host), local)
	}
	sidecarConnectHostAdapter = func(local *sidecar.Sidecar, host SidecarConnectHost) {
		handOffs.record("connect "+name(host), local)
	}
	return handOffs
}

// Each setter stores its host and the next replaces it; on the local backend each reaches its adapter with the core.
func TestComputeHostsAreStoredAndReplaced(t *testing.T) {
	handOffs := testRecordComputeHandOffs(t)
	world := newTestSidecarWorld(t)
	local := world.open(t)

	first := []any{&testConnectHost{name: "a"}, &testLocalInferenceHost{name: "a"}, &testSafetyClassifierHost{name: "a"}}
	local.SetConnectHost(first[0].(SidecarConnectHost))
	local.SetLocalInferenceHost(first[1].(SidecarLocalInferenceHost))
	local.SetSafetyClassifierHost(first[2].(SidecarSafetyClassifierHost))
	connect, inference, classifier := local.computeHosts()
	if connect != first[0] || inference != first[1] || classifier != first[2] {
		t.Errorf("stored %v, %v, %v", connect, inference, classifier)
	}

	second := []any{&testConnectHost{name: "b"}, &testLocalInferenceHost{name: "b"}, &testSafetyClassifierHost{name: "b"}}
	local.SetConnectHost(second[0].(SidecarConnectHost))
	local.SetLocalInferenceHost(second[1].(SidecarLocalInferenceHost))
	local.SetSafetyClassifierHost(second[2].(SidecarSafetyClassifierHost))
	connect, inference, classifier = local.computeHosts()
	if connect != second[0] || inference != second[1] || classifier != second[2] {
		t.Errorf("replaced with %v, %v, %v", connect, inference, classifier)
	}

	// nil takes a host away, and is handed on as such
	local.SetLocalInferenceHost(nil)
	if _, inference, _ = local.computeHosts(); inference != nil {
		t.Errorf("a nil host left %v", inference)
	}

	events, cores := handOffs.all()
	want := []string{"connect a", "inference a", "classifier a", "connect b", "inference b", "classifier b", "inference none"}
	if !slices.Equal(events, want) {
		t.Errorf("handed off %v, want %v", events, want)
	}
	for _, core := range cores {
		if core == nil || core != local.Core() {
			t.Errorf("handed to %p, not the core %p", core, local.Core())
		}
	}

	// a closed Sidecar keeps what it is given and hands nothing on
	ctx, cancel := context.WithTimeout(context.Background(), testSidecarTimeout)
	defer cancel()
	if err := local.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	local.SetConnectHost(first[0].(SidecarConnectHost))
	if events, _ := handOffs.all(); len(events) != len(want) {
		t.Errorf("a closed Sidecar handed off %v", events[len(want):])
	}
}

// With no adapter landed, or on a Sidecar with no core, a host is stored and reaches no core: the core sees none, and
// refuses.
func TestComputeHostsReachNoCoreWithoutAnAdapter(t *testing.T) {
	localInference, safetyClassifier, connect := sidecarLocalInferenceHostAdapter, sidecarSafetyClassifierHostAdapter, sidecarConnectHostAdapter
	t.Cleanup(func() {
		sidecarLocalInferenceHostAdapter, sidecarSafetyClassifierHostAdapter, sidecarConnectHostAdapter = localInference, safetyClassifier, connect
	})
	sidecarLocalInferenceHostAdapter, sidecarSafetyClassifierHostAdapter, sidecarConnectHostAdapter = nil, nil, nil
	local := newTestSidecarWorld(t).open(t)
	host := &testLocalInferenceHost{name: "a"}
	local.SetLocalInferenceHost(host)
	if _, inference, _ := local.computeHosts(); inference != host {
		t.Errorf("stored %v", inference)
	}

	handOffs := testRecordComputeHandOffs(t)
	bare := &Sidecar{}
	bare.SetLocalInferenceHost(host)
	bare.SetSafetyClassifierHost(&testSafetyClassifierHost{name: "a"})
	bare.SetConnectHost(&testConnectHost{name: "a"})
	if events, _ := handOffs.all(); len(events) != 0 {
		t.Errorf("a Sidecar with no core handed off %v", events)
	}
}
