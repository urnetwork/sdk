//go:build !ios_extension && !js

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A history a test sets: the plan and the continuation it answers, and each spec it was given.
type sidecarHarnessTestHistory struct {
	stateLock sync.Mutex
	specs     []*sidecar.SessionContinueSpec
	plan      *sidecar.SessionContinuePlan
	continued *sidecar.SessionContinued
	err       error
}

func (self *sidecarHarnessTestHistory) ContinuePlan(ctx context.Context, spec *sidecar.SessionContinueSpec) (*sidecar.SessionContinuePlan, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.specs = append(self.specs, spec)
	return self.plan, self.err
}

func (self *sidecarHarnessTestHistory) Continue(ctx context.Context, spec *sidecar.SessionContinueSpec) (*sidecar.SessionContinued, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.specs = append(self.specs, spec)
	if self.err != nil {
		return nil, self.err
	}
	return self.continued, nil
}

// ContinuePlan answers T10's state as JSON from the spec the app sends, its run, node, act and model as given, and
// Continue answers the new run's id (§15.3); a spec that is no JSON object of a continuation's fields, or names no run,
// is refused before the history is asked, and a refusal of the history's, an enclave's, comes back as is.
func TestSidecarHarnessContinuePlanAndContinue(t *testing.T) {
	world := newSidecarHarnessTestWorld(t, nil, true)
	runId := sidecar.NewId()
	newRunId := sidecar.NewId()
	history := &sidecarHarnessTestHistory{
		plan: &sidecar.SessionContinuePlan{RunId: runId, Act: sidecar.SessionContinueActContinue, Model: sidecarHarnessTestModel,
			PreviousModel: "engy-glm-5.3-high", NodeCount: 7, UntrustedCount: 1, Tainted: true, Private: true},
		continued: &sidecar.SessionContinued{AgentId: sidecar.NewId(), Name: "planner", NewRunId: newRunId},
	}
	world.view.history = history
	spec := `{"run":"` + runId.String() + `","at":"0123456789ab","act":"fork","model":"` + sidecarHarnessTestModel + `"}`
	answer, err := world.view.ContinuePlan(spec)
	if err != nil {
		t.Fatal(err)
	}
	plan := &sidecar.SessionContinuePlan{}
	if err := json.Unmarshal([]byte(answer), plan); err != nil || plan.RunId != runId || !plan.Tainted || !plan.Private || plan.PreviousModel != "engy-glm-5.3-high" ||
		plan.NodeCount != 7 || plan.UntrustedCount != 1 {
		t.Fatalf("the plan: %s, %v", answer, err)
	}
	if !json.Valid([]byte(answer)) || !containsJsonKey(answer, "previous_model_here") {
		t.Fatalf("the plan's JSON names no previous_model_here: %s", answer)
	}
	got, err := world.view.Continue(spec)
	if err != nil || got != newRunId.String() {
		t.Fatalf("Continue answered %q, %v", got, err)
	}
	if len(history.specs) != 2 || history.specs[1].RunId != runId || history.specs[1].At != "0123456789ab" ||
		history.specs[1].Act != sidecar.SessionContinueActFork || history.specs[1].Model != sidecarHarnessTestModel {
		t.Fatalf("the specs given: %+v", history.specs)
	}
	for _, bad := range []string{`{"run":"` + runId.String() + `","unknown":1}`, `{"at":"0123456789ab"}`, `not json`} {
		if _, err := world.view.Continue(bad); !errors.Is(err, errSidecarHarnessRequest) {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	if len(history.specs) != 2 {
		t.Fatal("a malformed spec reached the history")
	}
	history.err = sidecar.ErrSessionContinueEnclave
	if _, err := world.view.Continue(`{"run":"` + runId.String() + `","enclave":"build1"}`); !errors.Is(err, sidecar.ErrSessionContinueEnclave) {
		t.Fatalf("an enclave's continue: %v", err)
	}
}

// Whether a JSON object's text names key at its top level.
func containsJsonKey(text string, key string) bool {
	decoded := map[string]json.RawMessage{}
	if json.Unmarshal([]byte(text), &decoded) != nil {
		return false
	}
	_, ok := decoded[key]
	return ok
}
