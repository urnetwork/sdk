//go:build !ios_extension && !js

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/urnetwork/sidecar"
)

func (self *sidecarHarnessTestHistory) Runs(ctx context.Context, filter *sidecar.SessionSizesFilter) (*sidecar.SessionSizes, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.filters = append(self.filters, filter)
	return self.sizes, nil
}

func (self *sidecarHarnessTestHistory) ExpireOldestBlobs(ctx context.Context, capName string) (*sidecar.SessionExpired, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.expiries = append(self.expiries, capName)
	return &sidecar.SessionExpired{BlobCount: 3, ByteCount: 1024}, nil
}

func (self *sidecarHarnessTestHistory) SetAutoExpire(ctx context.Context, on bool) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.switches = append(self.switches, on)
	return nil
}

func (self *sidecarHarnessTestHistory) PrunePlan(ctx context.Context, spec *sidecar.SessionPruneSpec) (*sidecar.SessionPrunePlan, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.prunes = append(self.prunes, spec)
	return &sidecar.SessionPrunePlan{Items: []*sidecar.SessionPruneItem{{EntryId: sidecar.NewId(), Kind: "session_blob"}},
		Impact: &sidecar.RecordEraseSessionImpact{Calls: []sidecar.Id{sidecar.NewId()}}}, nil
}

func (self *sidecarHarnessTestHistory) Prune(ctx context.Context, spec *sidecar.SessionPruneSpec) (*sidecar.SessionPruned, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.prunes = append(self.prunes, spec)
	return &sidecar.SessionPruned{Erased: spec.EntryIds}, nil
}

// Runs answers the run view as JSON from the filter the app sends: the runs with their nodes' and blobs' bytes apart,
// the run each continues, the totals by agent and device, the caps' warning with the largest runs, and a run's tree
// where it asks one; ExpireOldestBlobs takes network, member or the warning's own cap and nothing else; PrunePlan and
// Prune take a run or entries.
func TestSidecarHarnessRunsSizesAndTrees(t *testing.T) {
	world := newSidecarHarnessTestWorld(t, nil, true)
	runId, continued, nodeId := sidecar.NewId(), sidecar.NewId(), sidecar.NewId()
	history := &sidecarHarnessTestHistory{sizes: &sidecar.SessionSizes{
		Runs:    []*sidecar.SessionRunSize{{RunId: runId, AgentLabel: "planner", ContinuesRunId: &continued, NodeCount: 12, NodeByteCount: 9000, BlobCount: 2, BlobByteCount: 16 << 20}},
		ByAgent: []*sidecar.SessionSizeGroup{{Id: sidecar.NewId(), RunCount: 1, NodeByteCount: 9000, BlobByteCount: 16 << 20}},
		Caps:    &sidecar.SessionCapState{StorageLimit: 20 << 20, Warning: sidecar.SessionCapMember, ExpiryNeed: 4 << 20},
		Tree:    []*sidecar.SessionTreeNode{{NodeId: &nodeId, Type: sidecar.SessionNodeMessage, State: sidecar.RecordEntryLive, PrunedPartCount: 1}},
	}}
	world.view.history = history
	answer, err := world.view.Runs(`{"run":"` + runId.String() + `","tree":true}`)
	if err != nil {
		t.Fatal(err)
	}
	view := struct {
		Runs []struct {
			Run           string `json:"run"`
			Continues     string `json:"continues"`
			NodeByteCount int64  `json:"node_byte_count"`
			BlobByteCount int64  `json:"blob_byte_count"`
		} `json:"runs"`
		ByAgent []json.RawMessage `json:"by_agent"`
		Caps    struct {
			Warning    string `json:"warning"`
			ExpiryNeed int64  `json:"expiry_need"`
		} `json:"caps"`
		Tree []struct {
			Node            string `json:"node"`
			PrunedPartCount int    `json:"pruned_part_count"`
		} `json:"tree"`
	}{}
	if err := json.Unmarshal([]byte(answer), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Runs) != 1 || view.Runs[0].Run != runId.String() || view.Runs[0].Continues != continued.String() || view.Runs[0].NodeByteCount != 9000 ||
		view.Runs[0].BlobByteCount != 16<<20 || len(view.ByAgent) != 1 || view.Caps.Warning != "member" || view.Caps.ExpiryNeed != 4<<20 ||
		len(view.Tree) != 1 || view.Tree[0].Node != nodeId.String() || view.Tree[0].PrunedPartCount != 1 {
		t.Fatalf("the view: %s", answer)
	}
	if len(history.filters) != 1 || history.filters[0].RunId == nil || *history.filters[0].RunId != runId || !history.filters[0].Tree {
		t.Fatalf("the filter given: %+v", history.filters)
	}
	if _, err := world.view.Runs(`{"unknown":true}`); !errors.Is(err, errSidecarHarnessRequest) {
		t.Fatalf("an unknown filter field: %v", err)
	}
	if _, err := world.view.ExpireOldestBlobs("everything"); !errors.Is(err, errSidecarHarnessRequest) {
		t.Fatalf("a cap that is none: %v", err)
	}
	if expired, err := world.view.ExpireOldestBlobs(sidecar.SessionCapMember); err != nil || len(history.expiries) != 1 || history.expiries[0] != "member" ||
		!json.Valid([]byte(expired)) {
		t.Fatalf("the tap: %s, %v", expired, err)
	}
	if err := world.view.SetAutoExpire(true); err != nil || len(history.switches) != 1 || !history.switches[0] {
		t.Fatalf("the switch: %v", err)
	}
	if _, err := world.view.Prune(`{}`); !errors.Is(err, errSidecarHarnessRequest) {
		t.Fatalf("a prune naming nothing: %v", err)
	}
	plan, err := world.view.PrunePlan(`{"run":"` + runId.String() + `","scope":"blobs"}`)
	if err != nil || len(history.prunes) != 1 || history.prunes[0].Scope != sidecar.RecordEraseRunBlobs || !json.Valid([]byte(plan)) {
		t.Fatalf("the prune's plan: %s, %v", plan, err)
	}
}
