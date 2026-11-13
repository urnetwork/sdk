package sdk

// pro_features_status_test.go -- the Pro features in use notice
// (pro_features_status.go): the status a proxies list reads as, and when the
// main screen's notice shows. Both replay js/test/pro_features_vectors.json,
// which js/test/pro_features.test.ts replays too.

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

type proFeaturesStatusVector struct {
	Name   string          `json:"name"`
	Result json.RawMessage `json:"result"`
	Want   struct {
		Prompt              string `json:"prompt"`
		SocksProxyCount     int    `json:"socks_proxy_count"`
		WireguardProxyCount int    `json:"wireguard_proxy_count"`
		PlanKnown           bool   `json:"plan_known"`
		Pro                 bool   `json:"pro"`
		ProFeaturesEnforced bool   `json:"pro_features_enforced"`
	} `json:"want"`
}

type proFeaturesTrackerVector struct {
	Name  string `json:"name"`
	Steps []struct {
		Op     string `json:"op"`
		Prompt string `json:"prompt"`
		Action string `json:"action"`
		Shown  string `json:"shown"`
	} `json:"steps"`
}

type proFeaturesVectors struct {
	Status  []*proFeaturesStatusVector  `json:"status"`
	Tracker []*proFeaturesTrackerVector `json:"tracker"`
}

func loadProFeaturesVectors(t *testing.T) *proFeaturesVectors {
	t.Helper()
	data, err := os.ReadFile("js/test/pro_features_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	vectors := &proFeaturesVectors{}
	if err := json.Unmarshal(data, vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Status) == 0 || len(vectors.Tracker) == 0 {
		t.Fatal("the vectors are empty")
	}
	return vectors
}

// The wire strings are the server's (pro.yml's feature names) and the other
// platforms'.
func TestProFeaturesConstants(t *testing.T) {
	pins := map[string][2]string{
		"ProFeatureSocksProxy":           {ProFeatureSocksProxy, "socks_proxy"},
		"ProFeatureWireguardProxy":       {ProFeatureWireguardProxy, "wireguard_proxy"},
		"ProFeaturesPromptNone":          {ProFeaturesPromptNone, ""},
		"ProFeaturesPromptUpgrade":       {ProFeaturesPromptUpgrade, "upgrade"},
		"ProFeaturesPromptStopped":       {ProFeaturesPromptStopped, "stopped"},
		"ProFeaturesNoticeNone":          {ProFeaturesNoticeNone, ""},
		"ProFeaturesNoticePost":          {ProFeaturesNoticePost, "post"},
		"ProFeaturesNoticeUpdate":        {ProFeaturesNoticeUpdate, "update"},
		"ProFeaturesNoticeRemove":        {ProFeaturesNoticeRemove, "remove"},
		"ProFeaturesErrorNone":           {ProFeaturesErrorNone, ""},
		"ProFeaturesErrorSignInRequired": {ProFeaturesErrorSignInRequired, "sign_in_required"},
		"ProFeaturesErrorLoadFailed":     {ProFeaturesErrorLoadFailed, "load_failed"},
	}
	for name, pin := range pins {
		if pin[0] != pin[1] {
			t.Errorf("%s = %q, want %q", name, pin[0], pin[1])
		}
	}
}

// The status a proxies list reads as: the prompt only for a network the
// server says holds no Pro, with a SOCKS or WireGuard proxy; the upgrade before
// enforcement and what stopped after; no prompt without the server's plan.
func TestProFeaturesStatusVectors(t *testing.T) {
	for _, vector := range loadProFeaturesVectors(t).Status {
		var result *NetworkClientsResult
		if !bytes.Equal(bytes.TrimSpace(vector.Result), []byte("null")) {
			result = &NetworkClientsResult{}
			if err := json.Unmarshal(vector.Result, result); err != nil {
				t.Fatalf("%s: %v", vector.Name, err)
			}
		}
		status := ProFeaturesStatusFor(result)
		got := map[string]any{
			"prompt":                status.Prompt,
			"socks_proxy_count":     status.SocksProxyCount,
			"wireguard_proxy_count": status.WireguardProxyCount,
			"plan_known":            status.PlanKnown,
			"pro":                   status.Pro,
			"pro_features_enforced": status.ProFeaturesEnforced,
		}
		want := map[string]any{
			"prompt":                vector.Want.Prompt,
			"socks_proxy_count":     vector.Want.SocksProxyCount,
			"wireguard_proxy_count": vector.Want.WireguardProxyCount,
			"plan_known":            vector.Want.PlanKnown,
			"pro":                   vector.Want.Pro,
			"pro_features_enforced": vector.Want.ProFeaturesEnforced,
		}
		for key, value := range want {
			if got[key] != value {
				t.Errorf("%s: %s = %v, want %v", vector.Name, key, got[key], value)
			}
		}
	}
}

// When the main screen's notice shows, alerts and goes, as the client limit's
// notice does, and what a dismissal holds back.
func TestProFeaturesNoticeTrackerVectors(t *testing.T) {
	for _, vector := range loadProFeaturesVectors(t).Tracker {
		tracker := NewProFeaturesNoticeTracker()
		for i, step := range vector.Steps {
			var action string
			switch step.Op {
			case "update":
				action = tracker.Update(&ProFeaturesStatus{Prompt: step.Prompt})
			case "dismiss":
				action = tracker.Dismiss()
			case "reset":
				action = tracker.Reset()
			default:
				t.Fatalf("%s: step %d: unknown op %q", vector.Name, i, step.Op)
			}
			if action != step.Action {
				t.Errorf("%s: step %d (%s %q): action %q, want %q", vector.Name, i, step.Op, step.Prompt, action, step.Action)
			}
			if shown := tracker.GetShownPrompt(); shown != step.Shown {
				t.Errorf("%s: step %d (%s %q): shows %q, want %q", vector.Name, i, step.Op, step.Prompt, shown, step.Shown)
			}
		}
	}
}

// A nil status is no prompt: the notice goes.
func TestProFeaturesNoticeTrackerNilStatusWithdraws(t *testing.T) {
	tracker := NewProFeaturesNoticeTracker()
	connect.AssertEqual(t, tracker.Update(nil), ProFeaturesNoticeNone)
	connect.AssertEqual(t, tracker.Update(&ProFeaturesStatus{Prompt: ProFeaturesPromptUpgrade}), ProFeaturesNoticePost)
	connect.AssertEqual(t, tracker.Update(nil), ProFeaturesNoticeRemove)
	connect.AssertEqual(t, tracker.GetShownPrompt(), ProFeaturesPromptNone)
}

// The status is what the server says. A network the list says holds Pro has no
// prompt however many SOCKS and WireGuard proxies it holds, and a list without
// the plan has none either: the client never decides Pro by itself.
func TestProFeaturesStatusFollowsTheServersPlan(t *testing.T) {
	proxies := NewNetworkClientInfoList()
	features := NewStringList()
	features.Add(ProFeatureSocksProxy)
	features.Add(ProFeatureWireguardProxy)
	proxies.Add(&NetworkClientInfo{ClientId: NewId(), ProFeatures: features})

	free := ProFeaturesStatusFor(&NetworkClientsResult{Clients: proxies, Plan: &NetworkProxiesPlan{}})
	connect.AssertEqual(t, free.Prompt, ProFeaturesPromptUpgrade)
	pro := ProFeaturesStatusFor(&NetworkClientsResult{Clients: proxies, Plan: &NetworkProxiesPlan{Pro: true, ProFeaturesEnforced: true}})
	connect.AssertEqual(t, pro.Prompt, ProFeaturesPromptNone)
	older := ProFeaturesStatusFor(&NetworkClientsResult{Clients: proxies})
	connect.AssertEqual(t, older.Prompt, ProFeaturesPromptNone)
	connect.AssertEqual(t, older.PlanKnown, false)
	connect.AssertEqual(t, older.SocksProxyCount, 1)
}
