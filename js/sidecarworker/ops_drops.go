//go:build sidecar_http || !js

// The team console's drop states (§18.8's pages and messages rows, §18.5's Drops; D-SDK-29, I-13, owner decisions 183,
// 184; SDK-M10-05): drop_states lists a repo's drops as evs's table holds them, each with its state, its window's end,
// the paths this browser names that it covers, and for a pending drop on the ref asked the command that cancels it on
// a device. It only reads: a page Worker is read_only and admin_refused, so it never dry-runs, signs or cancels a drop
// (§18.8's pages row), and its seam, WorkerDrops, holds three reads and nothing else.
//
//	drop_states  repo_id, [ref]   each drop of the repo (EvsRepo.Drops), oldest first, at most DropStatesMaxDropCount,
//	                              named against the paths this device names on the ref (EvsRepo.KnownPaths), at most
//	                              the core's DropNamesCount a drop: names that stay between the Worker and its page
//
// A drop a hold defers reads done, as everywhere, its held bytes counted as freed, and no field shows a hold: a status's
// phase, progress and refusal reason never reach the view (owner decision 184). The device command names only ids, a
// ref asked for and the words of `ur history cancel` (§11.7's cli row), never a path.

package sidecarworker

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/urnetwork/sidecar"
)

// The operation, as requests name it.
const OperationDropStates = "drop_states"

// What the console reads of a repo's drops: on the page Worker, its EvsClient's *sidecar.EvsRepo, whose other calls, the
// dry run, the drop and the cancel among them, this seam leaves out, so no reply path reaches one.
type WorkerDrops interface {
	// The ref's verified listing, whose RefId names the ref a drop is on.
	List(ctx context.Context, args *sidecar.EvsReadArgs) (*sidecar.EvsListing, error)
	// The paths of files this device names on the ref, at most the core's DropNamesCount.
	KnownPaths(ctx context.Context, refName []byte) ([]string, error)
	// The repo's drops, each named against the known paths of the ref args names.
	Drops(ctx context.Context, args *sidecar.EvsDropsArgs) ([]*sidecar.EvsDrop, error)
}

// A repo's drops as drop_states answers them.
type DropStatesView struct {
	RepoId string           `json:"repo_id"`
	Drops  []*DropStateView `json:"drops"`
	// More drops than DropStatesMaxDropCount: the newest are not shown.
	Truncated bool `json:"truncated,omitempty"`
}

// One drop.
type DropStateView struct {
	// Empty for a row evs answered with no 16-byte id.
	DropId string `json:"drop_id,omitempty"`
	RefId  string `json:"ref_id,omitempty"`
	// The ref version its DROP push committed at, decimal.
	Version string `json:"version"`
	// pending, cancelled, executing, done or refused; a drop a hold defers reads done; unknown for a state this Worker
	// does not know.
	State string `json:"state"`
	// Its window's end, in milliseconds since the epoch: its versions read drop pending before it, and a repo admin's
	// device cancels it until 60 s before it.
	NotBeforeMillis int64 `json:"not_before_millis"`
	// The bytes deleted so far, decimal, a held drop's counted as deleted.
	FreedBytes string `json:"freed_bytes"`
	// The paths this device names that its lineages cover: none for a drop on another ref, or one whose body this
	// device cannot read.
	Paths []NameView `json:"paths"`
	// A pending drop on the ref asked: the command a repo admin runs on a device to cancel it, which this page cannot.
	DeviceCommand string `json:"device_command,omitempty"`
}

// The state a view shows for a drop whose state this Worker does not know.
const dropStateUnknown = "unknown"

// Each drop of the repo, with its state and names (the file's comment).
func runDropStates(worker *Worker, ctx context.Context, request *Request, fields *requestFields, step *workerStep) *Reply {
	if worker.args.Drops == nil {
		return refusalReply("", CodeUnavailable)
	}
	drops, err := worker.args.Drops(fields.repoId)
	if err != nil {
		return errorReply(err)
	}
	listing, err := drops.List(ctx, &sidecar.EvsReadArgs{RefName: fields.refName})
	if err != nil {
		return errorReply(err)
	}
	if err := step.step(ctx); err != nil {
		return errorReply(err)
	}
	known, err := drops.KnownPaths(ctx, fields.refName)
	if err != nil {
		return errorReply(err)
	}
	if err := step.step(ctx); err != nil {
		return errorReply(err)
	}
	listed, err := drops.Drops(ctx, &sidecar.EvsDropsArgs{RefName: fields.refName, KnownPaths: known})
	if err != nil {
		return errorReply(err)
	}
	view := &DropStatesView{RepoId: fields.repoId.String(), Drops: []*DropStateView{}}
	for _, drop := range listed {
		if err := step.step(ctx); err != nil {
			return errorReply(err)
		}
		if worker.settings.DropStatesMaxDropCount <= len(view.Drops) {
			view.Truncated = true
			break
		}
		view.Drops = append(view.Drops, dropStateViewOf(fields, listing.RefId, drop))
	}
	return &Reply{Ok: true, DropStates: view}
}

// drop's view: its state by the root's one mapping, a held drop's done (sidecar.SidecarDropStateOf), and none of its
// status's phase, progress or reason.
func dropStateViewOf(fields *requestFields, refId sidecar.Id, drop *sidecar.EvsDrop) *DropStateView {
	status := drop.Status
	view := &DropStateView{
		DropId:          optionalIdString(status.GetDropId()),
		RefId:           optionalIdString(status.GetRefId()),
		Version:         strconv.FormatUint(status.GetVersion(), 10),
		State:           dropStateUnknown,
		NotBeforeMillis: status.GetNotBeforeMs(),
		FreedBytes:      strconv.FormatUint(status.GetFreedBytes(), 10),
		Paths:           []NameView{},
	}
	state, known := sidecar.SidecarDropStateOf(status.GetState())
	if known {
		view.State = string(state)
	}
	for _, path := range drop.Paths {
		view.Paths = append(view.Paths, nameViewOf([]byte(path)))
	}
	if state == sidecar.SidecarDropPending && view.DropId != "" && view.RefId == refId.String() {
		view.DeviceCommand = dropCancelCommand(fields, view.DropId)
	}
	return view
}

// `ur history cancel <repo_id> <drop_id> [--ref=<ref>]` (§11.7's cli row) for a drop on the ref asked.
func dropCancelCommand(fields *requestFields, dropId string) string {
	command := "ur history cancel " + fields.repoId.String() + " " + dropId
	if fields.refName != nil {
		command += " --ref=" + shellQuoted(string(fields.refName))
	}
	return command
}

// A word a POSIX shell reads as itself.
var shellPlainWord = regexp.MustCompile(`^[A-Za-z0-9._/@%+=:,-]+$`)

// s as one POSIX shell word: as it is when it holds nothing a shell reads, else in single quotes, each quote within
// closed, escaped and reopened.
func shellQuoted(s string) string {
	if shellPlainWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
