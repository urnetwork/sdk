//go:build !ios_extension && !js

// The run view and its storage for the apps (DESIGN-COMPUTE §24.10, §15.3, §15.4, R-CTXH-8, D-CTXH-8; CTXH-M11-06):
// SidecarHarness's Runs, ExpireOldestBlobs, SetAutoExpire, PrunePlan and Prune, the common view controller of
// DESIGN-UI.md's T9 (Sessions: every session newest first, each naming its agent, Join or Switch, Compact) and of
// §15.4's history warning, over the Sidecar's storage view in process (the root's session_storage.go), or on the remote
// backend over ur daemon's harness.runs, harness.runs_expire and harness.runs_auto_expire, so both answer the same
// JSON.
//
//	Runs(filterJson)           {agent?, agent_label?, run?, tree?, by_size?}: the runs newest first, or largest first,
//	                           each with its agent, model and label, the device that wrote it, the run it continues
//	                           (a run and its continuations are one conversation), its nodes' and blobs' bytes apart and
//	                           what is held back, dropped or erased, and whether it is open here; history's totals by
//	                           agent and by device; the caps: at 80% of the network's storage or of the member's share,
//	                           the warning, the largest runs and the bytes the one tap frees; and with tree and a run,
//	                           that run's nodes, their parents, types, states and parts not held
//	ExpireOldestBlobs(cap)     the warning's one tap, network, member or "" for the cap it warns at: the oldest blobs
//	                           expired until history is under 70% of that cap, every node kept; the person's act, which
//	                           the app calls only from the tap
//	SetAutoExpire(on)          the switch that does so again at each cap, the person's act
//	PrunePlan(specJson)        {run, scope: all|context|blobs} or {entries}: what a prune erases and what it cuts, the
//	                           run's same_as nodes, the calls whose context it cuts and the runs that continue it,
//	                           listed before the person confirms; nothing erased
//	Prune(specJson)            the same spec, the entries the plan listed: erased, the person's act (RECORD.md section 8),
//	                           in process only; the remote backend's daemon takes it through `ur record erase`
//
// §15.3 lists Runs; ExpireOldestBlobs, SetAutoExpire, PrunePlan and Prune serve §15.4's warning, which §15.3 does not list
// (proposed, reported). gomobile-safe: strings, bool and JSON.
//
// Nothing here runs in the VPN extension or on the js leg. Safe for concurrent use.

package sdk

import (
	"context"

	"github.com/urnetwork/sidecar"
)

// ur daemon's run view methods, as the root's session_daemon.go names them.
const (
	sidecarControlMethodHarnessRuns           = "harness.runs"
	sidecarControlMethodHarnessRunsExpire     = "harness.runs_expire"
	sidecarControlMethodHarnessRunsAutoExpire = "harness.runs_auto_expire"
)

// harness.runs' params, as the daemon takes them (sidecar.HarnessRunsParams).
type sidecarControlHarnessRunsParams struct {
	Network string `json:"network,omitempty"`
	sidecar.SessionSizesFilter
}

// harness.runs_expire's params (sidecar.HarnessRunsExpireParams).
type sidecarControlHarnessRunsExpireParams struct {
	Network string `json:"network,omitempty"`
	Cap     string `json:"cap,omitempty"`
}

// harness.runs_auto_expire's params (sidecar.HarnessRunsAutoExpireParams).
type sidecarControlHarnessRunsAutoExpireParams struct {
	Network string `json:"network,omitempty"`
	On      bool   `json:"on"`
}

// What the run view runs over, beside the history: the Sidecar's storage view in process, or the daemon's methods.
type sidecarHarnessStorage interface {
	Runs(ctx context.Context, filter *sidecar.SessionSizesFilter) (*sidecar.SessionSizes, error)
	ExpireOldestBlobs(ctx context.Context, capName string) (*sidecar.SessionExpired, error)
	SetAutoExpire(ctx context.Context, on bool) error
	PrunePlan(ctx context.Context, spec *sidecar.SessionPruneSpec) (*sidecar.SessionPrunePlan, error)
	Prune(ctx context.Context, spec *sidecar.SessionPruneSpec) (*sidecar.SessionPruned, error)
}

func (self *sidecarHarnessLocalHistory) Runs(ctx context.Context, filter *sidecar.SessionSizesFilter) (*sidecar.SessionSizes, error) {
	storage, err := self.sidecar.SessionStorage()
	if err != nil {
		return nil, err
	}
	return storage.Sizes(ctx, filter)
}

// The app calls it only from the person's tap: their act, confirmed in its UI.
func (self *sidecarHarnessLocalHistory) ExpireOldestBlobs(ctx context.Context, capName string) (*sidecar.SessionExpired, error) {
	storage, err := self.sidecar.SessionStorage()
	if err != nil {
		return nil, err
	}
	return storage.ExpireOldestBlobs(ctx, &sidecar.SessionExpireArgs{Cap: capName, Confirmed: true})
}

func (self *sidecarHarnessLocalHistory) SetAutoExpire(ctx context.Context, on bool) error {
	storage, err := self.sidecar.SessionStorage()
	if err != nil {
		return err
	}
	return storage.SetAutoExpire(on, true, false)
}

func (self *sidecarHarnessLocalHistory) PrunePlan(ctx context.Context, spec *sidecar.SessionPruneSpec) (*sidecar.SessionPrunePlan, error) {
	record, err := self.sidecar.Record()
	if err != nil {
		return nil, err
	}
	return record.SessionPrunePlan(ctx, spec, false)
}

func (self *sidecarHarnessLocalHistory) Prune(ctx context.Context, spec *sidecar.SessionPruneSpec) (*sidecar.SessionPruned, error) {
	record, err := self.sidecar.Record()
	if err != nil {
		return nil, err
	}
	return record.SessionPrune(ctx, spec, true, false)
}

func (self *sidecarHarnessRemoteHistory) Runs(ctx context.Context, filter *sidecar.SessionSizesFilter) (*sidecar.SessionSizes, error) {
	sizes := &sidecar.SessionSizes{}
	params := &sidecarControlHarnessRunsParams{Network: self.harness.network(), SessionSizesFilter: *filter}
	if err := self.harness.remote().call(ctx, sidecarControlMethodHarnessRuns, params, false, sizes); err != nil {
		return nil, err
	}
	return sizes, nil
}

func (self *sidecarHarnessRemoteHistory) ExpireOldestBlobs(ctx context.Context, capName string) (*sidecar.SessionExpired, error) {
	expired := &sidecar.SessionExpired{}
	params := &sidecarControlHarnessRunsExpireParams{Network: self.harness.network(), Cap: capName}
	if err := self.harness.remote().call(ctx, sidecarControlMethodHarnessRunsExpire, params, true, expired); err != nil {
		return nil, err
	}
	return expired, nil
}

func (self *sidecarHarnessRemoteHistory) SetAutoExpire(ctx context.Context, on bool) error {
	params := &sidecarControlHarnessRunsAutoExpireParams{Network: self.harness.network(), On: on}
	return self.harness.remote().call(ctx, sidecarControlMethodHarnessRunsAutoExpire, params, true, &struct{}{})
}

func (self *sidecarHarnessRemoteHistory) PrunePlan(ctx context.Context, spec *sidecar.SessionPruneSpec) (*sidecar.SessionPrunePlan, error) {
	return nil, errSidecarHarnessRemoteUnbuilt
}

func (self *sidecarHarnessRemoteHistory) Prune(ctx context.Context, spec *sidecar.SessionPruneSpec) (*sidecar.SessionPruned, error) {
	return nil, errSidecarHarnessRemoteUnbuilt
}

// This view's storage: the history's, which serves both.
func (self *SidecarHarness) storageOf() (sidecarHarnessStorage, error) {
	history, err := self.historyOf()
	if err != nil {
		return nil, err
	}
	storage, ok := history.(sidecarHarnessStorage)
	if !ok {
		return nil, errSidecarHarnessNoHistory
	}
	return storage, nil
}

// The run view as JSON (see the file header's Runs row).
func (self *SidecarHarness) Runs(filterJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		filter := &sidecar.SessionSizesFilter{}
		if err := sidecarComputeDecode(filterJson, filter); err != nil {
			return "", errSidecarHarnessRequest
		}
		storage, err := self.storageOf()
		if err != nil {
			return "", err
		}
		sizes, err := storage.Runs(self.ctx(), filter)
		if err != nil {
			return "", err
		}
		return sidecarHarnessJson(sizes)
	})
}

// The warning's one tap, as JSON: the blobs expired and their bytes. capName is network, member, or "" for the cap the
// warning names.
func (self *SidecarHarness) ExpireOldestBlobs(capName string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		switch capName {
		case "", sidecar.SessionCapNetwork, sidecar.SessionCapMember:
		default:
			return "", errSidecarHarnessRequest
		}
		storage, err := self.storageOf()
		if err != nil {
			return "", err
		}
		expired, err := storage.ExpireOldestBlobs(self.ctx(), capName)
		if err != nil {
			return "", err
		}
		return sidecarHarnessJson(expired)
	})
}

// The switch that expires the oldest blobs again at each cap.
func (self *SidecarHarness) SetAutoExpire(on bool) error {
	return traceWithError(func() error {
		storage, err := self.storageOf()
		if err != nil {
			return err
		}
		return storage.SetAutoExpire(self.ctx(), on)
	})
}

// A prune's spec from the app's JSON: a run, or entries.
func sidecarHarnessPruneSpec(specJson string) (*sidecar.SessionPruneSpec, error) {
	spec := &sidecar.SessionPruneSpec{}
	if err := sidecarComputeDecode(specJson, spec); err != nil || (spec.RunId == nil && len(spec.EntryIds) == 0) {
		return nil, errSidecarHarnessRequest
	}
	return spec, nil
}

// What a prune would erase and cut, as JSON; nothing erased.
func (self *SidecarHarness) PrunePlan(specJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		spec, err := sidecarHarnessPruneSpec(specJson)
		if err != nil {
			return "", err
		}
		storage, err := self.storageOf()
		if err != nil {
			return "", err
		}
		plan, err := storage.PrunePlan(self.ctx(), spec)
		if err != nil {
			return "", err
		}
		return sidecarHarnessJson(plan)
	})
}

// Erases what the spec names, as JSON: the person's act, which the app calls only after showing PrunePlan's answer and
// with the entries it listed.
func (self *SidecarHarness) Prune(specJson string) (string, error) {
	return traceWithReturnError(func() (string, error) {
		spec, err := sidecarHarnessPruneSpec(specJson)
		if err != nil {
			return "", err
		}
		storage, err := self.storageOf()
		if err != nil {
			return "", err
		}
		pruned, err := storage.Prune(self.ctx(), spec)
		if err != nil {
			return "", err
		}
		return sidecarHarnessJson(pruned)
	})
}
