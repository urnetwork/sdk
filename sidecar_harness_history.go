//go:build !ios_extension && !js

// A run's continue and fork for the apps (DESIGN-COMPUTE §24.7, §15.3, D-CTXH-5; CTXH-M11-05): SidecarHarness's
// ContinuePlan and Continue, the common view controller of DESIGN-UI.md's T9 (Sessions: Switch on a session no harness
// has claimed, wherever it last ran) and T10 (Switch into a session from another harness: where it last ran and where it
// will run, the model, offered again where the old one cannot run here, and what carries over, the taint and the
// private mark), over the Sidecar's continuer in process (the root's session_continue.go), or on the remote backend over
// ur daemon's harness.continue_plan and harness.continue, so both answer the same JSON.
//
//	ContinuePlan(specJson)  {run, at?, act?, model?, name?}: T10's state, nothing started: the node, the model and the
//	                        one it replaces, whether this device's catalog names the old one, the context's nodes and
//	                        how many are flagged or untrusted, whether the new run starts tainted and with the private
//	                        or limited_use mark, what this device does not hold, and for a continue whether the run is
//	                        live (refused) or its liveness unread (refused, a fork going on)
//	Continue(specJson)      the same spec: the new run's id (§15.3). The app calls it only from the person's Switch,
//	                        after showing ContinuePlan's answer; its agent then takes Prompt, and AddListener's events
//	                        name it. On the remote backend the call is the person's confirmed act
//
// act is continue (the default), fork, which names a node in at, or clone, a fork at the tip. An enclave in the spec
// is refused: CTXH-M14-01's. gomobile-safe: strings and JSON.
//
// Nothing here runs in the VPN extension or on the js leg. Safe for concurrent use.

package sdk

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/urnetwork/sidecar"
)

// ur daemon's context-history methods, as the root's session_daemon.go names them; TestSidecarHarnessControlWireMatchesTheDaemon
// holds them on the legs that build it.
const (
	sidecarControlMethodHarnessContinuePlan = "harness.continue_plan"
	sidecarControlMethodHarnessContinue     = "harness.continue"
)

// harness.continue_plan's and harness.continue's params, as the daemon takes them (sidecar.HarnessContinueParams).
type sidecarControlHarnessContinueParams struct {
	Network string `json:"network,omitempty"`
	sidecar.SessionContinueSpec
}

// What the Harness's history methods run over: the Sidecar's continuer in process, or the daemon's methods remotely.
type sidecarHarnessHistory interface {
	ContinuePlan(ctx context.Context, spec *sidecar.SessionContinueSpec) (*sidecar.SessionContinuePlan, error)
	Continue(ctx context.Context, spec *sidecar.SessionContinueSpec) (*sidecar.SessionContinued, error)
}

// The Sidecar's own continuer, built at each call from what the network holds then.
type sidecarHarnessLocalHistory struct {
	sidecar *sidecar.Sidecar
}

func (self *sidecarHarnessLocalHistory) ContinuePlan(ctx context.Context, spec *sidecar.SessionContinueSpec) (*sidecar.SessionContinuePlan, error) {
	continues, err := self.sidecar.SessionContinue()
	if err != nil {
		return nil, err
	}
	return continues.Plan(ctx, spec)
}

func (self *sidecarHarnessLocalHistory) Continue(ctx context.Context, spec *sidecar.SessionContinueSpec) (*sidecar.SessionContinued, error) {
	continues, err := self.sidecar.SessionContinue()
	if err != nil {
		return nil, err
	}
	return continues.Continue(ctx, spec)
}

// The daemon's methods, for a desktop app in remote mode.
type sidecarHarnessRemoteHistory struct {
	harness *SidecarHarness
}

func (self *sidecarHarnessRemoteHistory) ContinuePlan(ctx context.Context, spec *sidecar.SessionContinueSpec) (*sidecar.SessionContinuePlan, error) {
	plan := &sidecar.SessionContinuePlan{}
	params := &sidecarControlHarnessContinueParams{Network: self.harness.network(), SessionContinueSpec: *spec}
	if err := self.harness.remote().call(ctx, sidecarControlMethodHarnessContinuePlan, params, false, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func (self *sidecarHarnessRemoteHistory) Continue(ctx context.Context, spec *sidecar.SessionContinueSpec) (*sidecar.SessionContinued, error) {
	continued := &sidecar.SessionContinued{}
	params := &sidecarControlHarnessContinueParams{Network: self.harness.network(), SessionContinueSpec: *spec}
	// the app calls it from the person's Switch: their confirmed act (D-CORE-13)
	if err := self.harness.remote().call(ctx, sidecarControlMethodHarnessContinue, params, true, continued); err != nil {
		return nil, err
	}
	model := ""
	if continued.Plan != nil {
		model = continued.Plan.Model
	}
	self.harness.addRemoteAgent(&sidecarControlHarnessAgentReply{Agent: continued.AgentId.String(), Name: continued.Name,
		Run: continued.NewRunId.String(), Model: model, Created: true})
	return continued, nil
}

// What a Harness with neither backend answers.
var errSidecarHarnessNoHistory = errors.New("harness: this Sidecar reads no run history")

// This view's history: the one it was given, else the Sidecar's continuer or the daemon's methods.
func (self *SidecarHarness) historyOf() (sidecarHarnessHistory, error) {
	if self.history != nil {
		return self.history, nil
	}
	switch {
	case self.args != nil && self.args.local != nil:
		return &sidecarHarnessLocalHistory{sidecar: self.args.local}, nil
	case self.args != nil && self.args.remote != nil:
		return &sidecarHarnessRemoteHistory{harness: self}, nil
	}
	return nil, errSidecarHarnessNoHistory
}

// value as the app reads it.
func sidecarHarnessJson(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// A spec from the app's JSON.
func sidecarHarnessContinueSpec(specJson string) (*sidecar.SessionContinueSpec, error) {
	spec := &sidecar.SessionContinueSpec{}
	if err := sidecarComputeDecode(specJson, spec); err != nil || spec.RunId == (sidecar.Id{}) {
		return nil, errSidecarHarnessRequest
	}
	return spec, nil
}

// What continuing or forking specJson's run would take, as JSON: T10's state. Nothing is started.
func (self *SidecarHarness) ContinuePlan(specJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		spec, err := sidecarHarnessContinueSpec(specJson)
		if err != nil {
			return "", err
		}
		history, err := self.historyOf()
		if err != nil {
			return "", err
		}
		plan, err := history.ContinuePlan(self.ctx(), spec)
		if err != nil {
			return "", err
		}
		return sidecarHarnessJson(plan)
	})
}

// Continues or forks specJson's run here (§24.7): the new run's id. Its agent takes Prompt, and AddListener's events
// name it.
func (self *SidecarHarness) Continue(specJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		spec, err := sidecarHarnessContinueSpec(specJson)
		if err != nil {
			return "", err
		}
		history, err := self.historyOf()
		if err != nil {
			return "", err
		}
		continued, err := history.Continue(self.ctx(), spec)
		if err != nil {
			return "", err
		}
		if self.harness != nil {
			if agent, ok := self.harness.Agent(continued.AgentId); ok {
				// the person is attached to its run while a listener is added, as to an agent NewAgent starts
				self.attachWhileListened(agent)
			}
		}
		return continued.NewRunId.String(), nil
	})
}
