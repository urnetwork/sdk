//go:build !ios_extension && !js

package sdk

// The authorization features' prompt for the apps (sidecar_auth_features.go; DESIGN-COMPUTE §16.6, CACL-M11-03): on the
// remote backend GetAuthFeatures reads the daemon's admin.settings.show unconfirmed and TurnOnAuthFeature sends the
// Confirm method admin.settings.feature confirmed, a name that is no feature's sending nothing; on the local backend
// both are the core's, which answers no state before a verified one, never an empty list.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
)

// One call the daemon took.
type testAuthFeaturesCall struct {
	method    string
	confirmed bool
	params    json.RawMessage
}

// A daemon that answers the admin settings methods with replies, recording each call.
type testAuthFeaturesDaemon struct {
	stateLock sync.Mutex
	calls     []testAuthFeaturesCall
	// each method's answer, marshaled into the call's result; an error answers as the daemon's refusal
	replies map[string]any
	refusal error
}

func (self *testAuthFeaturesDaemon) sidecarId() *Id {
	return nil
}

func (self *testAuthFeaturesDaemon) state() (string, string) {
	return "online", ""
}

func (self *testAuthFeaturesDaemon) generation() int64 {
	return 1
}

func (self *testAuthFeaturesDaemon) refresh(ctx context.Context) error {
	return nil
}

func (self *testAuthFeaturesDaemon) pair(ctx context.Context, childJwt string) (*sidecarControlPairReply, error) {
	return nil, errors.New("this daemon pairs nothing")
}

func (self *testAuthFeaturesDaemon) network() string {
	return "the paired network"
}

func (self *testAuthFeaturesDaemon) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	paramsJson, err := json.Marshal(params)
	if err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls = append(self.calls, testAuthFeaturesCall{method: method, confirmed: confirmed, params: paramsJson})
	if self.refusal != nil {
		return self.refusal
	}
	reply, ok := self.replies[method]
	if !ok {
		return &sidecarControlError{Code: sidecarControlCodeMethodNotFound, Message: "method not found"}
	}
	replyJson, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	return json.Unmarshal(replyJson, result)
}

func (self *testAuthFeaturesDaemon) close() {
}

// The calls taken so far.
func (self *testAuthFeaturesDaemon) taken() []testAuthFeaturesCall {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.calls)
}

// A turning-on in the app's prompt goes to the daemon confirmed, as the Confirm method admin.settings.feature with the
// feature and the paired network, and answers the feature as the daemon says it stands; a name that is no feature's is
// refused before anything is sent, and the daemon's refusal reaches the app.
func TestTurnOnAuthFeatureRemoteConfirmed(t *testing.T) {
	daemon := &testAuthFeaturesDaemon{replies: map[string]any{
		sidecarControlMethodAdminSettingsFeature: &sidecar.AuthFeatureStatus{Name: "agent_repos", On: true, Implemented: true, Supported: true, SupportKnown: true},
	}}
	remote := &Sidecar{remote: daemon}
	if _, err := remote.TurnOnAuthFeature("everything"); err == nil || len(daemon.taken()) != 0 {
		t.Fatalf("a name that is no feature's: %v, sent %v", err, daemon.taken())
	}
	feature, err := remote.TurnOnAuthFeature("agent_repos")
	if err != nil || feature.Name != "agent_repos" || !feature.On || feature.MayTurnOn {
		t.Fatalf("the turning-on: %+v %v", feature, err)
	}
	calls := daemon.taken()
	if len(calls) != 1 || calls[0].method != sidecarControlMethodAdminSettingsFeature || !calls[0].confirmed {
		t.Fatalf("it sent %+v", calls)
	}
	sent := &sidecarControlAdminSettingsParams{}
	if err := json.Unmarshal(calls[0].params, sent); err != nil || *sent != (sidecarControlAdminSettingsParams{Network: "the paired network", Feature: "agent_repos"}) {
		t.Fatalf("it asked %+v: %v", sent, err)
	}
	daemon.stateLock.Lock()
	daemon.refusal = &sidecarControlError{Code: -32000, Message: "authorization feature agent_repos: only an Owner's or Admin's standing device signs ServiceSettings"}
	daemon.stateLock.Unlock()
	if _, err := remote.TurnOnAuthFeature("agent_repos"); err == nil || err.Error() != daemon.refusal.Error() {
		t.Fatalf("the daemon's refusal: %v", err)
	}
}

// GetAuthFeatures reads the daemon's features unconfirmed, each with who can turn it on and what would pause; a daemon
// that names none, and a local core with no verified state, answer an error, never an empty list; a closed Sidecar too.
func TestGetAuthFeatures(t *testing.T) {
	admin, pausing := sidecar.NewId(), sidecar.NewId()
	daemon := &testAuthFeaturesDaemon{replies: map[string]any{
		sidecarControlMethodAdminSettingsShow: map[string]any{
			"mail_strict": "off",
			"features": []*sidecar.AuthFeatureStatus{
				{Name: "agent_repos", Implemented: true, Supported: true, SupportKnown: true, MayTurnOn: true, AdminMemberIds: []sidecar.Id{admin}, PausingSidecarIds: []sidecar.Id{pausing}, PausingKnown: true},
				{Name: "compute_policy", Implemented: true, SupportKnown: true, AdminMemberIds: []sidecar.Id{admin}},
				{Name: "listeners", AdminMemberIds: []sidecar.Id{admin}},
				{Name: "enclaves", On: true, Implemented: true, Supported: true, SupportKnown: true, AdminMemberIds: []sidecar.Id{admin}},
			},
		},
	}}
	features, err := (&Sidecar{remote: daemon}).GetAuthFeatures()
	if err != nil || features.Len() != 4 {
		t.Fatalf("the daemon's features: %v", err)
	}
	if calls := daemon.taken(); len(calls) != 1 || calls[0].method != sidecarControlMethodAdminSettingsShow || calls[0].confirmed {
		t.Fatalf("it sent %+v", calls)
	}
	agentRepos := features.Feature("agent_repos")
	if agentRepos == nil || !agentRepos.MayTurnOn || agentRepos.On || !agentRepos.PausingKnown ||
		agentRepos.AdminMemberIds.Len() != 1 || agentRepos.AdminMemberIds.Get(0).String() != admin.String() ||
		agentRepos.PausingSidecarIds.Len() != 1 || agentRepos.PausingSidecarIds.Get(0).String() != pausing.String() {
		t.Fatalf("agent_repos: %+v", agentRepos)
	}
	if listeners := features.Feature("listeners"); listeners == nil || listeners.Implemented || listeners.MayTurnOn || listeners.PausingSidecarIds.Len() != 0 {
		t.Fatalf("listeners: %+v", listeners)
	}
	if enclaves := features.Feature("enclaves"); enclaves == nil || !enclaves.On || enclaves.MayTurnOn {
		t.Fatalf("enclaves: %+v", enclaves)
	}
	if features.Feature("everything") != nil {
		t.Fatal("a name that is no feature's was found")
	}

	silent := &testAuthFeaturesDaemon{replies: map[string]any{sidecarControlMethodAdminSettingsShow: map[string]any{"mail_strict": "off"}}}
	if features, err := (&Sidecar{remote: silent}).GetAuthFeatures(); !errors.Is(err, errSidecarAuthFeaturesUnknown) || features != nil {
		t.Fatalf("a daemon naming no features: %v", err)
	}
	if features, err := (&Sidecar{local: &sidecar.Sidecar{}}).GetAuthFeatures(); !errors.Is(err, sidecar.ErrSidecarActNoAuthority) || features != nil {
		t.Fatalf("a core with no verified state: %v", err)
	}
	if features, err := (&Sidecar{}).GetAuthFeatures(); !errors.Is(err, errSidecarAuthFeaturesUnavailable) || features != nil {
		t.Fatalf("a closed Sidecar: %v", err)
	}
	if _, err := (&Sidecar{}).TurnOnAuthFeature("agent_repos"); !errors.Is(err, errSidecarAuthFeaturesUnavailable) {
		t.Fatalf("a closed Sidecar's turning-on: %v", err)
	}
}

// Every exported signature the file adds is one gomobile binds (sidecar_accessors_compute_test.go's check).
func TestAuthFeaturesSignaturesAreGomobileSafe(t *testing.T) {
	findings := testComputeSignatureFindings(t, []string{"sidecar_auth_features.go"}, map[string]string{"Sidecar": "struct", "IdList": "struct"})
	for _, finding := range findings {
		t.Error(finding)
	}
}
