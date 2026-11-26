//go:build !ios_extension && !js

package sdk

// The app's local inference twin reaching the core (sidecar_compute_host_inference.go; INFER-M11-06, CONTRACTS C-39):
// a test app's twin set through the sdk becomes the local core's engine, its capabilities the catalog's device, its
// events reaching the core's listener; nil takes it away again.

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A test app's LiteRT-LM twin: it reports a phone and records the listener each generation is given.
type testAppInferenceHost struct {
	stateLock sync.Mutex
	listeners []SidecarLocalInferenceListener
	loads     []string
}

func (self *testAppInferenceHost) Capabilities() string {
	return `{"platform":"ios/arm64","class":"phone","memory_byte_count":8589934592,"unified_memory":true,"accelerators":["gpu"],"gpu":"Apple A18 Pro GPU"}`
}

func (self *testAppInferenceHost) Load(modelPath string, settingsJson string) (int64, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.loads = append(self.loads, modelPath)
	return int64(len(self.loads)), nil
}

func (self *testAppInferenceHost) Generate(handle int64, requestJson string, listener SidecarLocalInferenceListener) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.listeners = append(self.listeners, listener)
	return nil
}

func (self *testAppInferenceHost) Cancel(handle int64) {}

func (self *testAppInferenceHost) Unload(handle int64) {}

// The core's listener, recording each event.
type testCoreInferenceListener struct {
	stateLock sync.Mutex
	events    []string
}

func (self *testCoreInferenceListener) OnEvent(eventJson string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, eventJson)
}

// Set through the sdk, the twin is the local core's engine: the core's catalog is judged for the device it reports;
// nil takes it away, and the core says no local engine runs.
func TestLocalInferenceTwinReachesTheCore(t *testing.T) {
	local := newTestSidecarWorld(t).open(t)
	host := &testAppInferenceHost{}
	local.SetLocalInferenceHost(host)
	router, err := local.Core().InferRouter()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := router.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	device := catalog.Device()
	if device == nil || device.Platform != "ios/arm64" || device.Class != sidecar.InferDeviceClassPhone ||
		!slices.Contains(device.Accelerators, sidecar.InferAcceleratorGpu) {
		t.Fatalf("the core's device is %+v, not the twin's", device)
	}
	local.SetLocalInferenceHost(nil)
	catalog, err = router.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Device() != nil || catalog.LocalUnavailable() == "" {
		t.Errorf("with the host taken away the core still runs one: %+v, %q", catalog.Device(), catalog.LocalUnavailable())
	}
}

// The twin passes each call through and bridges the listener: the app's engine's events reach the core's listener as
// they were sent.
func TestLocalInferenceTwinBridgesTheListener(t *testing.T) {
	host := &testAppInferenceHost{}
	twin := &sidecarLocalInferenceHostTwin{host: host}
	var core sidecar.SidecarLocalInferenceHost = twin
	if handle, err := core.Load("/state/models/abc", `{"backend":"gpu"}`); err != nil || handle != 1 {
		t.Fatalf("load %d %v", handle, err)
	}
	listener := &testCoreInferenceListener{}
	if err := core.Generate(1, `{"messages":[]}`, listener); err != nil {
		t.Fatal(err)
	}
	host.stateLock.Lock()
	given := host.listeners[0]
	host.stateLock.Unlock()
	event, _ := json.Marshal(&sidecar.InferEvent{Kind: sidecar.InferEventText, Text: "hello"})
	given.OnEvent(string(event))
	given.OnEvent(`{"kind":"end"}`)
	if !slices.Equal(listener.events, []string{string(event), `{"kind":"end"}`}) {
		t.Errorf("the core heard %v", listener.events)
	}
	if core.Capabilities() != host.Capabilities() {
		t.Error("the capabilities were not passed through")
	}
}
