//go:build !js && !ios && !android && !sdk_mobile_bind && !sidecar_http && !ios_extension

package sdk

// The remote backend's harness wire against ur daemon's own (the root's harness_daemon.go, which builds on desktop legs
// alone, as this file does): the method names and modes are the daemon's, every params SidecarHarness sends decodes
// whole into the daemon's params, which it reads strictly, and the daemon's replies decode into the sdk's.

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/urnetwork/sidecar"
)

// Each method name and mode is the daemon's, each params decodes into the daemon's with no field left over, and each
// reply the daemon writes reads back whole.
func TestSidecarHarnessControlWireMatchesTheDaemon(t *testing.T) {
	for own, daemon := range map[string]string{
		sidecarControlMethodHarnessAgent:     sidecar.HarnessDaemonMethodAgent,
		sidecarControlMethodHarnessPrompt:    sidecar.HarnessDaemonMethodPrompt,
		sidecarControlMethodHarnessEvents:    sidecar.HarnessDaemonMethodEvents,
		sidecarControlMethodHarnessRespondUi: sidecar.HarnessDaemonMethodRespondUi,
		sidecarControlMethodHarnessClose:     sidecar.HarnessDaemonMethodClose,
		sidecarHarnessModePrompt:             sidecar.HarnessPromptModePrompt,
		sidecarHarnessModeSteer:              sidecar.HarnessPromptModeSteer,
		sidecarHarnessModeFollowUp:           sidecar.HarnessPromptModeFollowUp,
		sidecarHarnessModeAbort:              sidecar.HarnessPromptModeAbort,
	} {
		if own != daemon {
			t.Errorf("the sdk sends %s where the daemon takes %s", own, daemon)
		}
	}
	strict := func(label string, value any, into any) {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(into); err != nil {
			t.Errorf("%s's %s does not decode as the other side's: %v", label, data, err)
		}
	}
	agent := &sidecar.HarnessAgentParams{}
	strict("harness.agent", &sidecarControlHarnessAgentParams{Network: "n", Agent: "a", Name: "helper", Model: "m"}, agent)
	prompt := &sidecar.HarnessPromptParams{}
	strict("harness.prompt", &sidecarControlHarnessPromptParams{Network: "n", Agent: "a", Text: "hi", Mode: sidecarHarnessModeSteer}, prompt)
	events := &sidecar.HarnessEventsParams{}
	strict("harness.events", &sidecarControlHarnessEventsParams{Network: "n", Agent: "a", After: 7, WaitMs: 25}, events)
	confirmed := true
	respond := &sidecar.HarnessRespondUiParams{}
	strict("harness.respond_ui", &sidecarControlHarnessRespondUiParams{Network: "n", Agent: "a",
		Response: &sidecar.HarnessUiResponse{Id: "r", Confirmed: &confirmed}}, respond)
	closing := &sidecar.HarnessCloseParams{}
	strict("harness.close", &sidecarControlHarnessCloseParams{Network: "n", Agent: "a"}, closing)
	if agent.Name != "helper" || prompt.Mode != sidecarHarnessModeSteer || events.After != 7 || respond.Response == nil ||
		respond.Response.Confirmed == nil || !*respond.Response.Confirmed || closing.Agent != "a" {
		t.Errorf("the daemon reads %+v %+v %+v %+v %+v", agent, prompt, events, respond, closing)
	}
	strict("harness.agent's reply", &sidecar.HarnessAgentReply{Agent: "a", Name: "helper", Run: "r", Model: "m", Created: true},
		&sidecarControlHarnessAgentReply{})
	strict("harness.events' reply", &struct {
		Events []json.RawMessage `json:"events"`
		Next   uint64            `json:"next"`
		Gap    bool              `json:"gap,omitempty"`
		Closed bool              `json:"closed,omitempty"`
	}{Events: []json.RawMessage{json.RawMessage(`{"type":"agent_start"}`)}, Next: 3, Closed: true}, &sidecarControlHarnessEventsReply{})
}
