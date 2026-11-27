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
		sidecarControlMethodHarnessContinuePlan:   sidecar.HarnessDaemonMethodContinuePlan,
		sidecarControlMethodHarnessContinue:       sidecar.HarnessDaemonMethodContinue,
		sidecarControlMethodHarnessRuns:           sidecar.HarnessDaemonMethodRuns,
		sidecarControlMethodHarnessRunsExpire:     sidecar.HarnessDaemonMethodRunsExpire,
		sidecarControlMethodHarnessRunsAutoExpire: sidecar.HarnessDaemonMethodRunsAutoExpire,
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
	strict := func(label string, value any, into any) {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(into); err != nil {
			t.Errorf("%s's %s does not decode as the daemon's: %v", label, data, err)
		}
	}
	runId := sidecar.NewId()
	runs := &sidecar.HarnessRunsParams{}
	strict("harness.runs", &sidecarControlHarnessRunsParams{Network: "n", SessionSizesFilter: sidecar.SessionSizesFilter{RunId: &runId, Tree: true, BySize: true}}, runs)
	expire := &sidecar.HarnessRunsExpireParams{}
	strict("harness.runs_expire", &sidecarControlHarnessRunsExpireParams{Network: "n", Cap: sidecar.SessionCapMember}, expire)
	auto := &sidecar.HarnessRunsAutoExpireParams{}
	strict("harness.runs_auto_expire", &sidecarControlHarnessRunsAutoExpireParams{Network: "n", On: true}, auto)
	if runs.RunId == nil || *runs.RunId != runId || !runs.Tree || expire.Cap != sidecar.SessionCapMember || !auto.On {
		t.Fatalf("the daemon reads %+v %+v %+v", runs, expire, auto)
	}
}
