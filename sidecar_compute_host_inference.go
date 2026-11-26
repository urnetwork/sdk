//go:build !ios_extension && !js

// The app's local inference engine handed to the core (DESIGN-COMPUTE §6.5, §15.3; INFER-M11-06, CONTRACTS C-39): the
// adapter sidecar_compute_host.go waits for, which turns the app's gomobile twin of the host into the core's
// SidecarLocalInferenceHost (the root's infer_host.go), JSON text in and out, and makes it the local core's engine, so
// the Bot tab's local models run on the LiteRT-LM the app embeds (DESIGN-UI.md's T screens read them through Models()).
// The twin and the root's interface take the same calls; only the listener's type differs, which its own twin bridges.
//
// The core keeps the model files the app's engine loads under the app's StateDir and pulls them itself (Models().Pull,
// the root's InferPull); the app's host is given only a whole file's path. Where the core cannot take the host, as a
// Sidecar whose state is not on a disk, it keeps none, and its catalog says no local engine runs here. Nothing here runs
// on the remote backend, whose daemon runs its own engine, nor in the VPN extension (owner decision 197).

package sdk

import (
	"github.com/urnetwork/sidecar"
)

func init() {
	sidecarLocalInferenceHostAdapter = adaptSidecarLocalInferenceHost
}

// Hands host to the local core as its local engine; nil takes the core's away. Called outside every lock, as
// setSidecarComputeHost calls every adapter.
func adaptSidecarLocalInferenceHost(local *sidecar.Sidecar, host SidecarLocalInferenceHost) {
	var core sidecar.SidecarLocalInferenceHost
	if host != nil {
		core = &sidecarLocalInferenceHostTwin{host: host}
	}
	// a core that cannot take it keeps none, which its catalog says; the app's setter answers nothing
	local.SetLocalInferenceHost(core)
}

// The app's twin as the core's host: each call passed through, the listener bridged.
type sidecarLocalInferenceHostTwin struct {
	host SidecarLocalInferenceHost
}

// See sidecar.SidecarLocalInferenceHost.
func (self *sidecarLocalInferenceHostTwin) Capabilities() string {
	return self.host.Capabilities()
}

// See sidecar.SidecarLocalInferenceHost.
func (self *sidecarLocalInferenceHostTwin) Load(modelPath string, settingsJson string) (int64, error) {
	return self.host.Load(modelPath, settingsJson)
}

// See sidecar.SidecarLocalInferenceHost: the app's engine calls the twin listener, which calls the core's.
func (self *sidecarLocalInferenceHostTwin) Generate(handle int64, requestJson string, listener sidecar.SidecarLocalInferenceListener) error {
	return self.host.Generate(handle, requestJson, &sidecarLocalInferenceListenerTwin{listener: listener})
}

// See sidecar.SidecarLocalInferenceHost.
func (self *sidecarLocalInferenceHostTwin) Cancel(handle int64) {
	self.host.Cancel(handle)
}

// See sidecar.SidecarLocalInferenceHost.
func (self *sidecarLocalInferenceHostTwin) Unload(handle int64) {
	self.host.Unload(handle)
}

// The core's listener as the app's twin: what the app's engine calls with each event.
type sidecarLocalInferenceListenerTwin struct {
	listener sidecar.SidecarLocalInferenceListener
}

// See SidecarLocalInferenceListener.
func (self *sidecarLocalInferenceListenerTwin) OnEvent(eventJson string) {
	self.listener.OnEvent(eventJson)
}
