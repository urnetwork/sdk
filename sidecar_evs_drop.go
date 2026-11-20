//go:build !ios_extension && !js

// Drop history in the apps (§18.5's drop calls; §11.7's action, boundary, states and cli rows, §11.13; R-SDK-12,
// D-SDK-29, I-4, I-8; owner decisions 126, 173, 183, 184, 396; SDK-M10-05): SidecarEvs's dry run, drop, cancel and
// listing, each a thin mapping onto the core's EvsRepo, which walks the lineages, builds, signs and checks the DROP
// push, and judges every listed version on this device's clock.
//
//	who      a preview, a drop and a cancel are a repo admin's, in an owner-bound repo only its owner's consented devices
//	         that are not read_only (D-SDK-29, D-ACL-25), and a drop only while no lock stands (§8.8): this layer asks
//	         AclState.MayDrop of the newest verified state before anything reaches the core, which checks again before
//	         it signs, as evs and every reader do at the push's own auth pin. A host whose composition gives no
//	         verified state refuses all three (sidecarEvsDropGate). Each is a person's act in the app's UI: no MCP tool
//	         reaches these calls (D-MCP-32, §16.6's refused list), and the page Worker has none, its drop_states only
//	         reading (sdk/js/sidecarworker/ops_drops.go)
//	preview  DropHistoryPreview is the server's mandatory dry run (§11.13's preview row): the lineages a drop of the
//	         path would list, evs's figures, and the preview id DropHistory names. follow, rename ancestors, is refused
//	         by the core until it is built (EVS_DROP_PATH), failing closed. A head-mode device walks the path's
//	         versions itself within 4 MiB of statements, a longer history answering EVS_DROP_TOO_LARGE, for a device
//	         that mirrors the repo (owner decision 396)
//	confirm  DropHistory names a dry run this device completed, and confirmPath, the name the person typed, must be the
//	         previewed path: it goes to the core exactly as typed, and the core refuses any other (EVS_DROP_CONFIRM).
//	         now skips the 7-day window, behind a second confirmation in the app's UI: the core takes now only with
//	         its word now typed (EvsDropConfirmNow), which this layer gives for an app's now, the bound call being the
//	         person's act, and never otherwise, so a drop without now always waits its 7 days
//	cancel   CancelDrop, any repo admin's (owner-bound: such a device), until 60 s before the drop's not_before by this
//	         device's clock, past which the core answers EVS_DROP_STATE; a lock never refuses a cancel
//	listing  Drops, any reader's (§11.20): each drop's state, its window's end and the paths this device names that its
//	         lineages cover, at most the core's DropNamesCount, 1,024, which never leave the device (I-13). A drop a
//	         hold defers reads done, as everywhere, and no field shows a hold (owner decision 184)
//
// The codes, beside sidecar_evs.go's:
//
//	EVS_DROP_CONFIRM          the typed path is not the previewed one, or now came without its word
//	EVS_DROP_PREVIEW_MISSING  a drop that names no dry run this device completed
//	EVS_DROP_REFUSED          this device may not preview, sign or cancel the drop (D-SDK-29), or the repo is locked
//	EVS_DROP_STALE            a path moved since the dry run: preview again
//	EVS_DROP_NOT_PENDING      a cancel of no pending drop
//	EVS_DROP_STATE            a cancel within 60 s of the drop's not_before, or evs's 409 DROP_STATE
//	EVS_DROP_LIST_MISMATCH    the dry run's list differs from evs's resolve of it: nothing signed
//	EVS_DROP_UNLINKED         a lineage evs cannot follow
//	EVS_DROP_TOO_LARGE        a drop past one command's bounds, or a head-mode walk past 4 MiB
//	EVS_DROP_PATH             a path a drop cannot name: a directory, a reserved name, follow
//	EVS_DROP_LIST             a DROP's list the core's own checks refuse
//	EVS_DROP_<refusal>        evs's own: EVS_DROP_CONSENT, EVS_DROP_KIND, EVS_DROP_BOUNDARY, ...
//
// Paths are the person's own and go only between the app and its core on this device: nothing here logs one (I-13).
// Safe for concurrent use.

package sdk

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// The drop calls' refusal codes (the file's comment). A format value: apps read them.
const (
	sidecarEvsCodeDropConfirm        = "EVS_DROP_CONFIRM"
	sidecarEvsCodeDropPreviewMissing = "EVS_DROP_PREVIEW_MISSING"
	sidecarEvsCodeDropRefused        = "EVS_DROP_REFUSED"
	sidecarEvsCodeDropStale          = "EVS_DROP_STALE"
	sidecarEvsCodeDropNotPending     = "EVS_DROP_NOT_PENDING"
	sidecarEvsCodeDropState          = "EVS_DROP_STATE"
	sidecarEvsCodeDropListMismatch   = "EVS_DROP_LIST_MISMATCH"
	sidecarEvsCodeDropUnlinked       = "EVS_DROP_UNLINKED"
	sidecarEvsCodeDropTooLarge       = "EVS_DROP_TOO_LARGE"
	sidecarEvsCodeDropPath           = "EVS_DROP_PATH"
	sidecarEvsCodeDropList           = "EVS_DROP_LIST"
)

// The core's format 2 refusals and their codes, in the order they are asked: a drop's own refusal before what it
// wraps.
var sidecarEvsFormat2Codes = []struct {
	err  error
	code string
}{
	{err: sidecar.ErrEvsDropConfirm, code: sidecarEvsCodeDropConfirm},
	{err: sidecar.ErrEvsDropPreviewMissing, code: sidecarEvsCodeDropPreviewMissing},
	{err: sidecar.ErrEvsDropRefused, code: sidecarEvsCodeDropRefused},
	{err: sidecar.ErrEvsDropStale, code: sidecarEvsCodeDropStale},
	{err: sidecar.ErrEvsDropNotPending, code: sidecarEvsCodeDropNotPending},
	{err: sidecar.ErrEvsDropState, code: sidecarEvsCodeDropState},
	{err: sidecar.ErrEvsDropListMismatch, code: sidecarEvsCodeDropListMismatch},
	{err: sidecar.ErrEvsDropUnlinked, code: sidecarEvsCodeDropUnlinked},
	{err: sidecar.ErrEvsDropTooLarge, code: sidecarEvsCodeDropTooLarge},
	{err: sidecar.ErrEvsDropPath, code: sidecarEvsCodeDropPath},
	{err: sidecar.ErrEvsDropList, code: sidecarEvsCodeDropList},
	{err: sidecar.ErrEvsDropped, code: sidecarEvsCodeDropped},
	{err: sidecar.ErrEvsCollected, code: sidecarEvsCodeCollected},
	{err: sidecar.ErrEvsVersioningSetting, code: sidecarEvsCodeVersioning},
}

// The code of one of the core's format 2 refusals err is, "" for none.
func sidecarEvsFormat2CodeOf(err error) string {
	for _, known := range sidecarEvsFormat2Codes {
		if errors.Is(err, known.err) {
			return known.code
		}
	}
	return ""
}

// What D-SDK-29's check judges.
type sidecarEvsDropAct int

const (
	// A dry run: no lock refuses it, since it drops nothing.
	sidecarEvsDropActPreview sidecarEvsDropAct = iota + 1
	// A DROP: refused while the repo's lock stands.
	sidecarEvsDropActDrop
	// A cancel: never refused by a lock (§8.8).
	sidecarEvsDropActCancel
)

// What D-SDK-29's check reads of a verified state: *sidecar.AclState answers both (ACL-M10-01).
type sidecarEvsDropState interface {
	MayDrop(subjectId sidecar.Id, repoId sidecar.Id, now time.Time) sidecar.AclDecision
	RepoLockAllowsDrop(repoId sidecar.Id, cancel bool) bool
}

// D-SDK-29's check on a host: the acl part of the newest verified authorization state and this device's identity,
// the same the composition gives its EvsClient as Config and Identity, judged at now. A nil gate, a piece missing,
// or a state that does not answer, refuses: an unbuilt check fails closed.
type sidecarEvsDropGate struct {
	state    func() (sidecarEvsDropState, error)
	identity func() *sidecar.SidecarIdentity
	now      func() time.Time
}

// No verified state answered, so nothing says this device may act.
var errSidecarEvsDropNoState = errors.New("this device holds no verified authorization state to check the drop by")

// A gate over a composition's EvsClientArgs.Config, whose view is the verified *sidecar.ConfigState, and its Identity.
func newSidecarEvsDropGate(config sidecar.EvsConfigFunction, identity func() *sidecar.SidecarIdentity, now func() time.Time) *sidecarEvsDropGate {
	return &sidecarEvsDropGate{
		state: func() (sidecarEvsDropState, error) {
			if config == nil {
				return nil, errSidecarEvsDropNoState
			}
			state, ok := config().(*sidecar.ConfigState)
			if !ok || state == nil {
				return nil, errSidecarEvsDropNoState
			}
			acl, err := state.Acl()
			if err != nil {
				return nil, err
			}
			return acl, nil
		},
		identity: identity,
		now:      now,
	}
}

// Whether this device may take act on repoId now: nil, or an error that is sidecar.ErrEvsDropRefused naming why.
func (self *sidecarEvsDropGate) check(repoId sidecar.Id, act sidecarEvsDropAct) error {
	refuse := func(why string) error {
		return fmt.Errorf("%w: %s", sidecar.ErrEvsDropRefused, why)
	}
	if self == nil || self.state == nil || self.identity == nil || self.now == nil {
		return refuse(errSidecarEvsDropNoState.Error())
	}
	identity := self.identity()
	if identity == nil {
		return refuse("this device holds no enrolled identity")
	}
	state, err := self.state()
	if err != nil || state == nil {
		return refuse(errSidecarEvsDropNoState.Error())
	}
	if decision := state.MayDrop(identity.SidecarId, repoId, self.now()); !decision.Allowed {
		return refuse(fmt.Sprintf("only a repo admin drops history, and in an owner-bound repo only its owner's consented devices that are not read only (%s)", decision.Reason))
	}
	if act == sidecarEvsDropActDrop && !state.RepoLockAllowsDrop(repoId, false) {
		return refuse("the repo's history is locked")
	}
	return nil
}

// A DROP's boundaries as a preview names them (§11.6's EvsDropPath.boundary).
const (
	// The path stands: its head is written again as the drop's kept version, and every older version is listed.
	sidecarDropBoundaryWritten = "written"
	// The path is removed: its removal stays, and every version below it is listed.
	sidecarDropBoundaryRemoved = "removed"
)

// One lineage of a dry run: a path as far as evs can follow it, and what a drop of it lists. json in the C ABI and
// js.
type SidecarDropLineage struct {
	// The path, as this device names it.
	Path string `json:"path"`
	// The versions a drop lists: every one with content below the boundary.
	VersionCount int64 `json:"version_count"`
	// written: the head stays, written again as the kept version; removed: the path's removal stays.
	Boundary string `json:"boundary"`
	// The oldest and the newest listed version's signed times, in unix milliseconds; 0 where the walk holds none.
	OldestMillis int64 `json:"oldest_millis,omitempty"`
	NewestMillis int64 `json:"newest_millis,omitempty"`
}

// A dry run's lineages.
type SidecarDropLineageList struct {
	exportedList[*SidecarDropLineage]
}

// An empty list.
func NewSidecarDropLineageList() *SidecarDropLineageList {
	return &SidecarDropLineageList{
		exportedList: *newExportedList[*SidecarDropLineage](),
	}
}

// One DropHistoryPreview's terminal answer, the server's dry run (§11.13's preview row): the versions a drop would
// list, the bytes it would free and keep, and their dates. json in the C ABI and js.
type SidecarDropPreview struct {
	Ok bool `json:"ok"`
	// What DropHistory names.
	PreviewId *Id `json:"preview_id,omitempty"`
	// What DropHistory's confirmPath must be: the path as the dry run was asked for it, which the app asks the person
	// to type.
	ConfirmPath string `json:"confirm_path,omitempty"`
	// Every lineage's list matched evs's resolve of it. A dry run that found a mismatch answers
	// EVS_DROP_LIST_MISMATCH with this false, and nothing can be signed from it.
	ListMatch bool `json:"list_match"`
	// The versions the drop lists, in all.
	VersionCount int64 `json:"version_count,omitempty"`
	// What evs's dry run reported: bytes only the listed versions name, which the drop frees; bytes a kept version
	// shares, which stay; the listed versions' inline records, kept until a compaction; candidates in packs; and
	// candidates too young to delete yet, which a later round deletes.
	FreedBytes  int64 `json:"freed_bytes,omitempty"`
	SharedBytes int64 `json:"shared_bytes,omitempty"`
	InlineBytes int64 `json:"inline_bytes,omitempty"`
	PackedBytes int64 `json:"packed_bytes,omitempty"`
	YoungBytes  int64 `json:"young_bytes,omitempty"`
	// The oldest and the newest listed version's signed times over every lineage, in unix milliseconds.
	OldestMillis int64                   `json:"oldest_millis,omitempty"`
	NewestMillis int64                   `json:"newest_millis,omitempty"`
	Lineages     *SidecarDropLineageList `json:"lineages,omitempty"`
	// The DROP pushes the drop takes, one confirmation for all of them.
	PushCount int32  `json:"push_count,omitempty"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message,omitempty"`
}

// Answers a DropHistoryPreview once.
type SidecarDropPreviewCallback connect.ApiCallback[*SidecarDropPreview]

// The states a drop reads in (§11.7's states row, §5.4's history_drop), beside the core's: a state this sdk does not
// know reads unknown, claiming nothing.
const sidecarDropStateUnknown = "unknown"

// One drop of a repo as Drops lists it. json in the C ABI and js.
type SidecarDrop struct {
	// The drop: what CancelDrop names. Nil for a row evs answered with no 16-byte id, which nothing can name.
	DropId *Id `json:"drop_id,omitempty"`
	// The ref it is on, and the ref version its DROP push committed at.
	RefId   *Id   `json:"ref_id,omitempty"`
	Version int64 `json:"version"`
	// pending, cancelled, executing, done or refused; a drop a hold defers reads done (owner decision 184), and a
	// state this sdk does not know reads unknown.
	State string `json:"state"`
	// Its window's end, in unix milliseconds: its versions read drop_pending before it, and CancelDrop takes it until
	// 60 s before it.
	NotBeforeMillis int64 `json:"not_before_millis"`
	// The bytes deleted so far, a held drop's counted as deleted.
	FreedBytes int64 `json:"freed_bytes,omitempty"`
	// The paths this device names on main that its lineages cover, at most 1,024 (I-13): none for a drop on another
	// ref, or one whose body this device cannot read.
	Paths *StringList `json:"paths,omitempty"`
}

// A repo's drops, in evs's order: oldest first.
type SidecarDropList struct {
	exportedList[*SidecarDrop]
}

// An empty list.
func NewSidecarDropList() *SidecarDropList {
	return &SidecarDropList{
		exportedList: *newExportedList[*SidecarDrop](),
	}
}

// One Drops call's terminal answer. json in the C ABI and js.
type SidecarDrops struct {
	Ok      bool             `json:"ok"`
	Drops   *SidecarDropList `json:"drops,omitempty"`
	Code    string           `json:"code,omitempty"`
	Message string           `json:"message,omitempty"`
}

// Answers a Drops call once.
type SidecarDropsCallback connect.ApiCallback[*SidecarDrops]

// A count evs reported, as an int64: past the largest it reads as the largest.
func sidecarDropInt64Of(count uint64) int64 {
	if math.MaxInt64 < count {
		return math.MaxInt64
	}
	return int64(count)
}

// The server's mandatory dry run of a drop of path's history on repoId's main ref (§18.5, §11.13's preview row): the
// lineages it would list, evs's figures, and the preview id DropHistory names, with the text its confirmation must
// be. follow adds rename ancestors, each a lineage, which the core refuses until it is built (EVS_DROP_PATH). A repo
// admin's act, in an owner-bound repo only its owner's consented devices', refused EVS_DROP_REFUSED before anything is
// sent otherwise (D-SDK-29). A dry run whose list does not match evs's answers EVS_DROP_LIST_MISMATCH, and a history
// longer than a head-mode device walks EVS_DROP_TOO_LARGE.
func (self *SidecarEvs) DropHistoryPreview(repoId *Id, path string, follow bool, callback SidecarDropPreviewCallback) {
	trace(func() {
		sidecarEvsAnswer(func() *SidecarDropPreview {
			refuse := func(err error) *SidecarDropPreview {
				return &SidecarDropPreview{Code: sidecarEvsCodeOf(err), Message: sidecarEvsMessageOf(err)}
			}
			ctx, err := self.callCtx()
			if err != nil {
				return refuse(err)
			}
			coreRepoId, err := sidecarEvsRepoIdOf(repoId)
			if err != nil {
				return refuse(err)
			}
			if err := self.backend.MayDrop(coreRepoId, sidecarEvsDropActPreview); err != nil {
				return refuse(err)
			}
			preview, err := self.backend.DropHistoryPreview(ctx, coreRepoId, &sidecar.EvsDropPreviewArgs{
				Paths:  []string{path},
				Follow: follow,
			})
			if err != nil {
				return refuse(err)
			}
			bound, err := sidecarDropPreviewOf(preview)
			if err != nil {
				return refuse(err)
			}
			return bound
		}, connect.ApiCallback[*SidecarDropPreview](callback))
	})
}

// The bound answer of a completed dry run.
func sidecarDropPreviewOf(preview *sidecar.EvsDropPreview) (*SidecarDropPreview, error) {
	previewId, err := IdFromBytes(preview.PreviewId)
	if err != nil {
		return nil, fmt.Errorf("%w: a dry run with no preview id", sidecar.ErrEvsReplyInvalid)
	}
	bound := &SidecarDropPreview{
		Ok:           true,
		PreviewId:    previewId,
		ConfirmPath:  sidecar.EvsDropConfirmText(preview),
		ListMatch:    true,
		VersionCount: sidecarDropInt64Of(preview.VersionCount),
		FreedBytes:   sidecarDropInt64Of(preview.FreedBytes),
		SharedBytes:  sidecarDropInt64Of(preview.SharedBytes),
		InlineBytes:  sidecarDropInt64Of(preview.InlineBytes),
		PackedBytes:  sidecarDropInt64Of(preview.PackedBytes),
		YoungBytes:   sidecarDropInt64Of(preview.YoungBytes),
		Lineages:     NewSidecarDropLineageList(),
		PushCount:    int32(min(len(preview.Batches), math.MaxInt32)),
	}
	for _, lineage := range preview.Lineages {
		row := sidecarDropLineageOf(lineage)
		bound.Lineages.Add(row)
		if row.OldestMillis != 0 && (bound.OldestMillis == 0 || row.OldestMillis < bound.OldestMillis) {
			bound.OldestMillis = row.OldestMillis
		}
		bound.NewestMillis = max(bound.NewestMillis, row.NewestMillis)
	}
	return bound, nil
}

// One lineage's row: its count as its DROP carries it, its boundary, and the signed times of the versions it lists,
// every version the walk holds below its through_version that is no removal.
func sidecarDropLineageOf(lineage *sidecar.EvsDropLineage) *SidecarDropLineage {
	dropPath := lineage.DropPath
	row := &SidecarDropLineage{
		Path:         lineage.Path,
		VersionCount: int64(dropPath.GetCount()),
		Boundary:     sidecarDropBoundaryWritten,
	}
	if dropPath.GetBoundary() == sidecar.EvsDropBoundaryRemoved {
		row.Boundary = sidecarDropBoundaryRemoved
	}
	for _, version := range lineage.Versions {
		if version.Removed || dropPath.GetThroughVersion() <= version.Version || version.Time.IsZero() {
			continue
		}
		at := version.Time.UnixMilli()
		if row.OldestMillis == 0 || at < row.OldestMillis {
			row.OldestMillis = at
		}
		row.NewestMillis = max(row.NewestMillis, at)
	}
	return row
}

// Drops the history the dry run previewId previewed on repoId (§18.5, §11.7's action row): one DROP push for each of
// its batches, signed here, the result's Value naming the drop's id, and each push's drop id, comma-separated, for a
// drop that took several. confirmPath is the name the person typed, refused EVS_DROP_CONFIRM unless it is the
// previewed path (the preview's ConfirmPath), passed as typed. now skips the 7-day window: the app passes it only
// after a second confirmation, and the core takes it only with that confirmation's word, which this gives it with
// now and never without it, so a drop without now waits its 7 days and stays cancellable. A repo admin's act, in an
// owner-bound repo only its owner's consented devices', and never while the repo is locked (EVS_DROP_REFUSED, before
// anything is sent); a dry run this device did not complete answers EVS_DROP_PREVIEW_MISSING, and a path that moved
// since it EVS_DROP_STALE. Repo admins' devices then raise history_drop with its cancel. A refusal after some pushes
// committed names their drops in Value: those stand, and CancelDrop takes them.
func (self *SidecarEvs) DropHistory(repoId *Id, previewId *Id, confirmPath string, now bool, callback SidecarResultCallback) {
	trace(func() {
		sidecarEvsAnswer(func() *SidecarResult {
			ctx, err := self.callCtx()
			if err != nil {
				return sidecarEvsResultOf("", err)
			}
			coreRepoId, err := sidecarEvsRepoIdOf(repoId)
			if err != nil {
				return sidecarEvsResultOf("", err)
			}
			if previewId == nil {
				return &SidecarResult{Code: sidecarEvsCodeDropPreviewMissing, Message: "a drop names the dry run it confirms"}
			}
			if err := self.backend.MayDrop(coreRepoId, sidecarEvsDropActDrop); err != nil {
				return sidecarEvsResultOf("", err)
			}
			args := &sidecar.EvsDropHistoryArgs{
				PreviewId:   previewId.Bytes(),
				ConfirmPath: confirmPath,
				Now:         now,
			}
			if now {
				// the app's second confirmation, which the core asks for as the word now typed
				args.ConfirmNow = sidecar.EvsDropConfirmNow
			}
			result, err := self.backend.DropHistory(ctx, coreRepoId, args)
			dropIds := []string{}
			if result != nil {
				for _, push := range result.Pushes {
					dropIds = append(dropIds, push.DropId.String())
				}
			}
			answer := sidecarEvsResultOf(strings.Join(dropIds, ","), err)
			if err != nil && 0 < len(dropIds) {
				answer.Value = strings.Join(dropIds, ",")
				answer.Message = fmt.Sprintf("%s; %d of the drop's pushes committed and stand", answer.Message, len(dropIds))
			}
			return answer
		}, connect.ApiCallback[*SidecarResult](callback))
	})
}

// Cancels the pending drop dropId on repoId's main ref (§18.5, §11.7's action row; owner decision 183): any repo admin,
// in an owner-bound repo only its owner's consented devices (EVS_DROP_REFUSED before anything is sent otherwise),
// until 60 s before the drop's not_before (EVS_DROP_STATE past it). A lock never refuses a cancel. A drop no longer
// pending answers EVS_DROP_NOT_PENDING. The result's Value names the drop cancelled.
func (self *SidecarEvs) CancelDrop(repoId *Id, dropId *Id, callback SidecarResultCallback) {
	trace(func() {
		sidecarEvsAnswer(func() *SidecarResult {
			ctx, err := self.callCtx()
			if err != nil {
				return sidecarEvsResultOf("", err)
			}
			coreRepoId, err := sidecarEvsRepoIdOf(repoId)
			if err != nil {
				return sidecarEvsResultOf("", err)
			}
			if dropId == nil {
				return &SidecarResult{Code: sidecarEvsCodeInvalid, Message: "a cancel names its drop"}
			}
			if err := self.backend.MayDrop(coreRepoId, sidecarEvsDropActCancel); err != nil {
				return sidecarEvsResultOf("", err)
			}
			result, err := self.backend.CancelDrop(ctx, coreRepoId, &sidecar.EvsCancelDropArgs{DropId: sidecar.Id(dropId.id)})
			if err != nil {
				return sidecarEvsResultOf("", err)
			}
			return sidecarEvsResultOf(result.DropId.String(), nil)
		}, connect.ApiCallback[*SidecarResult](callback))
	})
}

// Each drop of repoId as evs lists it, any reader's (§18.5, §11.20): its state, its window's end and the paths this
// device names that its lineages cover, at most 1,024, named here and nowhere else (I-13). A drop a hold defers reads
// done, as everywhere, and nothing here shows a hold (owner decision 184).
func (self *SidecarEvs) Drops(repoId *Id, callback SidecarDropsCallback) {
	trace(func() {
		sidecarEvsAnswer(func() *SidecarDrops {
			refuse := func(err error) *SidecarDrops {
				return &SidecarDrops{Code: sidecarEvsCodeOf(err), Message: sidecarEvsMessageOf(err)}
			}
			ctx, err := self.callCtx()
			if err != nil {
				return refuse(err)
			}
			coreRepoId, err := sidecarEvsRepoIdOf(repoId)
			if err != nil {
				return refuse(err)
			}
			drops, err := self.backend.Drops(ctx, coreRepoId)
			if err != nil {
				return refuse(err)
			}
			list := NewSidecarDropList()
			for _, drop := range drops {
				list.Add(sidecarDropOf(drop))
			}
			return &SidecarDrops{Ok: true, Drops: list}
		}, connect.ApiCallback[*SidecarDrops](callback))
	})
}

// The bound row of one drop evs listed: its state by the core's one mapping, a held drop's and an observed one's done
// (sidecar.SidecarDropStateOf), and nothing of the status that could show a hold, its phase and progress left out.
func sidecarDropOf(drop *sidecar.EvsDrop) *SidecarDrop {
	status := drop.Status
	row := &SidecarDrop{
		Version:         int64(min(status.GetVersion(), math.MaxInt64)),
		State:           sidecarDropStateUnknown,
		NotBeforeMillis: status.GetNotBeforeMs(),
		FreedBytes:      sidecarDropInt64Of(status.GetFreedBytes()),
	}
	if dropId, err := IdFromBytes(status.GetDropId()); err == nil {
		row.DropId = dropId
	}
	if refId, err := IdFromBytes(status.GetRefId()); err == nil {
		row.RefId = refId
	}
	if state, ok := sidecar.SidecarDropStateOf(status.GetState()); ok {
		row.State = string(state)
	}
	if 0 < len(drop.Paths) {
		row.Paths = NewStringList()
		row.Paths.addAll(drop.Paths...)
	}
	return row
}
