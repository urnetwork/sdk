package sdk

// PlanAccountName replays js/test/plan_account_vectors.json, the vectors the
// TypeScript twin (js/src/utils/plan_account.ts) replays, so the apps and
// ur.io show the same account name with the plan.

import (
	"encoding/json"
	"os"
	"testing"
)

type planAccountVector struct {
	Name        string `json:"name"`
	NetworkName string `json:"network_name"`
	Guest       bool   `json:"guest"`
	Want        string `json:"want"`
}

func TestPlanAccountNameVectors(t *testing.T) {
	data, err := os.ReadFile("js/test/plan_account_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	vectors := []*planAccountVector{}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) < 6 {
		t.Fatalf("only %d vectors", len(vectors))
	}
	for _, vector := range vectors {
		if got := PlanAccountName(vector.NetworkName, vector.Guest); got != vector.Want {
			t.Errorf("%s: PlanAccountName(%q, %t) = %q, want %q", vector.Name, vector.NetworkName, vector.Guest, got, vector.Want)
		}
	}
}
