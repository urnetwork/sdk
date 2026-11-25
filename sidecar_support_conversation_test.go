//go:build !ios_extension && !js

package sdk

import (
	"errors"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A Sidecar with no local core, as the remote backend is, answers ErrSidecarComputeNotBuilt from every support call,
// never an empty conversation or a success.
func TestSidecarSupportOnNoLocalCoreAnswersNotBuilt(t *testing.T) {
	support := (&Sidecar{}).Support()
	if _, err := support.Conversation(""); !errors.Is(err, ErrSidecarComputeNotBuilt) {
		t.Fatalf("Conversation: %v", err)
	}
	if _, err := support.Send(`{"text":"hello"}`); !errors.Is(err, ErrSidecarComputeNotBuilt) {
		t.Fatalf("Send: %v", err)
	}
	if err := support.Delete("100171"); !errors.Is(err, ErrSidecarComputeNotBuilt) {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := support.Settings(); !errors.Is(err, ErrSidecarComputeNotBuilt) {
		t.Fatalf("Settings: %v", err)
	}
}

// Until G-SUP-service passes the release's switch is off, and the person's message answers that support is reached
// through the app's form (§28.21); the app's support log still renders whole.
func TestSidecarSupportSendAnswersUnavailableWhileTheSwitchIsOff(t *testing.T) {
	world := newTestSidecarWorld(t)
	local := world.open(t)
	support := local.Support()
	if _, err := support.Send(`{"text":"the app's VPN drops"}`); !errors.Is(err, sidecar.ErrSupportUnavailable) {
		t.Fatalf("Send: %v", err)
	}
	logs, err := support.Logs("")
	if err != nil || logs == "" {
		t.Fatalf("Logs: %q, %v", logs, err)
	}
}
