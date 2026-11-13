package sdk

// An unavailable replica is different from a measured score of zero, including
// the JSON representation passed through the desktop ABI.

import (
	"encoding/json"
	"testing"
)

// Wire nulls, omitted evidence and SDK round trips retain score availability.
func TestSnTopFleetReplicaScoreAvailability(t *testing.T) {
	for _, test := range []struct {
		name      string
		body      string
		score     float64
		available bool
	}{
		{name: "missing", body: `{}`},
		{name: "unavailable", body: `{"replica_score":null}`},
		{name: "measured zero", body: `{"replica_score":0}`, available: true},
		{name: "measured score", body: `{"replica_score":1.25}`, score: 1.25, available: true},
		{name: "sdk unavailable", body: `{"replica_score":7,"has_replica_score":false}`},
		{name: "sdk measured zero", body: `{"replica_score":0,"has_replica_score":true}`, available: true},
	} {
		fleet := SnTopFleet{ReplicaScore: 99}
		if err := json.Unmarshal([]byte(test.body), &fleet); err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if fleet.ReplicaScore != test.score {
			t.Errorf("%s: score %v, want %v", test.name, fleet.ReplicaScore, test.score)
		}
		for round := 0; round < 2; round++ {
			encoded, err := json.Marshal(&fleet)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["has_replica_score"] != test.available {
				t.Errorf("%s round %d: availability %v, want %t", test.name, round, fields["has_replica_score"], test.available)
			}
			if err := json.Unmarshal(encoded, &fleet); err != nil {
				t.Fatal(err)
			}
		}
	}
}
