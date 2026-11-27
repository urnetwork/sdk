//go:build !js && !ios && !android && !sdk_mobile_bind && !sidecar_http && !ios_extension

package sdk

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/urnetwork/sidecar"
)

// The sdk's context-history methods and params are the daemon's: each name its own, and the params the sdk sends decode
// strictly as the daemon's.
func TestSidecarHarnessHistoryWireMatchesTheDaemon(t *testing.T) {
	for own, daemon := range map[string]string{
		sidecarControlMethodHarnessContinuePlan: sidecar.HarnessDaemonMethodContinuePlan,
		sidecarControlMethodHarnessContinue:     sidecar.HarnessDaemonMethodContinue,
	} {
		if own != daemon {
			t.Errorf("the sdk sends %s where the daemon takes %s", own, daemon)
		}
	}
	data, err := json.Marshal(&sidecarControlHarnessContinueParams{Network: "n", SessionContinueSpec: sidecar.SessionContinueSpec{
		RunId: sidecar.NewId(), At: "0123456789ab", Act: sidecar.SessionContinueActFork, Model: "m", Name: "planner"}})
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	params := &sidecar.HarnessContinueParams{}
	if err := decoder.Decode(params); err != nil || params.Network != "n" || params.At != "0123456789ab" || params.Act != sidecar.SessionContinueActFork {
		t.Fatalf("the daemon reads %s as %+v, %v", data, params, err)
	}
}
