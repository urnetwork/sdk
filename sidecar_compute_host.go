//go:build !ios_extension && !js

// The app's compute hosts (DESIGN-COMPUTE §15.3; CSURF-M11-01, CONTRACTS C-39): what an app implements for the core in
// its own process, and the setters' storage and hand-off. gomobile binds only this package's own types, strings,
// int64, int, bool and listener interfaces, so each host is declared here, every structure as JSON text, as a twin of
// the root's interface: INFER declares SidecarLocalInferenceHost and SidecarSafetyClassifierHost in the root's
// infer_host.go (INFER-M11-06), CONN its browser seam in its oauth files (CONN-M13-10). Each area adapts its twin to
// the root's interface in an sdk file of its own, setting its adapter below at init; INFER-M11-06, SAFE-M13-04 and
// CONN-M13-10 do.
//
// A setter stores the host and hands it to the local backend's core through the adapter. With no host set, no adapter
// landed yet, or the remote backend, whose daemon runs its own engine and browser, the core sees no host and refuses:
// local inference off on the app leg, the classifier asking the person, no connection flow. Nothing here runs on the
// VPN component or the js leg (owner decision 197).

package sdk

import (
	"github.com/urnetwork/sidecar"
)

// The app's local inference engine (§6.5): LiteRT-LM embedded in the app, on iOS, macOS and Android, which the router
// calls for local models. The root's interface takes the same calls (INFER-M11-06).
type SidecarLocalInferenceHost interface {
	// JSON: the platform, its RAM and accelerators.
	Capabilities() string
	// Loads the model at modelPath with settingsJson's engine settings, answering a handle.
	Load(modelPath string, settingsJson string) (int64, error)
	// Runs requestJson on handle's model, its events to listener, until it ends, Cancel or Unload.
	Generate(handle int64, requestJson string, listener SidecarLocalInferenceListener) error
	Cancel(handle int64)
	Unload(handle int64)
}

// A local generation's events.
type SidecarLocalInferenceListener interface {
	// One InferEvent, as JSON.
	OnEvent(eventJson string)
}

// The app's safety classifier (§27.5): a reduced ONNX Runtime embedded in the app, which scores what a run read for
// injected instructions. The root's interface takes the same calls (INFER-M11-06 declares it for SAFE).
type SidecarSafetyClassifierHost interface {
	// Loads the classifier at modelPath, answering a handle.
	Load(modelPath string) (int64, error)
	// Scores idsJson's token id windows on handle's model: two logits a window, as JSON.
	Classify(handle int64, idsJson string) (string, error)
	Unload(handle int64)
}

// The app's browser seam for connected services (§25.5): the system's sign-in sessions, which only the person
// completes. CONN-M13-10 adapts it to the root's seam.
type SidecarConnectHost interface {
	// Runs ASWebAuthenticationSession or Chrome's Auth Tab on requestJson and answers its callback, once.
	Open(requestJson string) (string, error)
	// Runs Android's AuthorizationClient on requestJson and answers an access token, held for one call.
	Authorize(requestJson string) (string, error)
}

// Each area's hand-off of its host to the local core, which its own sdk file sets at init: INFER-M11-06's for the
// local inference host, SAFE-M13-04's for the classifier, CONN-M13-10's for the connect host. Nil until that file
// lands, and then the core sees no host. Called outside every lock, with the host the app set, nil included, which
// takes the core's away.
var (
	sidecarLocalInferenceHostAdapter   func(local *sidecar.Sidecar, host SidecarLocalInferenceHost)
	sidecarSafetyClassifierHostAdapter func(local *sidecar.Sidecar, host SidecarSafetyClassifierHost)
	sidecarConnectHostAdapter          func(local *sidecar.Sidecar, host SidecarConnectHost)
)

// Stores host in slot, replacing what was there, and hands it to the local core through adapter, where one has landed.
// A closed Sidecar stores it and hands nothing on.
func setSidecarComputeHost[T any](self *Sidecar, slot *T, host T, adapter func(local *sidecar.Sidecar, host T)) {
	local, closed := func() (*sidecar.Sidecar, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		*slot = host
		return self.local, self.closed
	}()
	if adapter == nil || local == nil || closed {
		return
	}
	adapter(local, host)
}

// The hosts as set, nil where none is.
func (self *Sidecar) computeHosts() (SidecarConnectHost, SidecarLocalInferenceHost, SidecarSafetyClassifierHost) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.connectHost, self.localInferenceHost, self.safetyClassifierHost
}
