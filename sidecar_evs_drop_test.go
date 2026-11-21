//go:build !ios_extension && !js

package sdk

// SidecarEvs's format 2 calls (SDK-M10-05; sidecar §18.5's drop calls, §11.7's action, states and cli rows, D-SDK-29,
// owner decisions 173, 183, 184): Write's versioning, ReadVersion, History's states and the drop calls, over the fake
// of the core's evs client in sidecar_evs_test.go, whose format 2 half here keeps the core's own rules where the core
// exports them: the confirmation text (EvsDropConfirmText) and its word now (EvsDropConfirmNow), a listed version's
// state on this device's clock (EvsDropState), the 7-day window (EvsFormatSettings.DropWindow), the 60 s cancel margin
// (EvsSettings.DropCancelMargin) and a drop's state as evs reports it (SidecarDropStateOf, through the code under
// test). So each test shows what this layer passes, refuses and maps; the rules themselves are EVS2's and ACL's, whose
// FS-89 to FS-96 schedules hold them on the real engine. Every repo and path is synthetic.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
)

// One drop the fake holds: the path versions it lists, its window's end, and how evs reports it.
type testEvsDrop struct {
	dropId    sidecar.Id
	repoId    sidecar.Id
	path      string
	versions  []uint64
	notBefore time.Time
	// a committed cancel names it
	cancelled bool
	// what evs reports of it once its job ran: refused, executing, done (a held drop's too), observed; zero while pending
	reported protocol.EvsDropState
	freed    uint64
}

// The fake's format 2 half: its clock, the dry runs it completed, its drops, D-SDK-29's answer and what each call was
// asked. Under testSidecarEvsBackend's stateLock.
type testEvsDropFake struct {
	now      time.Time
	previews []*sidecar.EvsDropPreview
	drops    []*testEvsDrop
	// what MayDrop answers, nil allowing, and the acts it was asked
	mayDrop error
	acts    []sidecarEvsDropAct
	// what each call was asked
	versionings []string
	previewArgs []*sidecar.EvsDropPreviewArgs
	dropArgs    []*sidecar.EvsDropHistoryArgs
	cancelArgs  []*sidecar.EvsCancelDropArgs
	dropsCount  int
	// rows Drops lists beside the drops', as evs's table would
	statuses []*testEvsDropRow
	// versions the fake answers collected, by path
	collected map[string]bool
}

// A row of evs's drop table and the names this device gives it.
type testEvsDropRow struct {
	status *protocol.EvsDropStatus
	paths  []string
}

// The fake's base time: its versions' times start here.
var testEvsDropBase = time.UnixMilli(1_800_000_000_000)

// The fake's clock.
func (self *testEvsDropFake) clock() time.Time {
	if self.now.IsZero() {
		return testEvsDropBase.Add(time.Hour)
	}
	return self.now
}

// The live drop that lists version of path, nil for none: a cancelled one lists nothing, and one evs reports refused
// leaves its versions readable (§11.7's states row).
func (self *testEvsDropFake) listing(repoId sidecar.Id, path string, version uint64) *testEvsDrop {
	for _, drop := range self.drops {
		if drop.repoId == repoId && drop.path == path && slices.Contains(drop.versions, version) && !drop.cancelled &&
			drop.reported != protocol.EvsDropState_EVS_DROP_REFUSED {
			return drop
		}
	}
	return nil
}

// Moves the fake's clock.
func (self *testSidecarEvsBackend) setNow(now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.drop.now = now
}

// The core's ReadVersion over the fake: a version a live drop lists is EVS_DROPPED from its not_before on the
// fake's clock (EvsDropState.Dropped), whatever the fake still holds of it; a collected one ErrEvsCollected.
func (self *testSidecarEvsBackend) ReadVersion(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsReadArgs, out io.Writer) (*sidecar.EvsFileInfo, error) {
	if err := self.runHook(ctx, "read_version"); err != nil {
		return nil, err
	}
	file, head, err := func() (*testEvsFile, uint64, error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		versions := self.repoIdPaths[repoId][args.Path]
		if args.Version == 0 || uint64(len(versions)) < args.Version {
			return nil, 0, sidecar.ErrEvsPathAbsent
		}
		if self.drop.collected[args.Path] {
			return nil, 0, fmt.Errorf("%w: version %d was replaced", sidecar.ErrEvsCollected, args.Version)
		}
		if drop := self.drop.listing(repoId, args.Path, args.Version); drop != nil {
			state := &sidecar.EvsDropState{DropId: drop.dropId, NotBefore: drop.notBefore}
			if state.Dropped(self.drop.clock()) {
				return nil, 0, fmt.Errorf("version %d: %w", args.Version, sidecar.ErrEvsDropped)
			}
		}
		return versions[args.Version-1], self.repoIdHeads[repoId], nil
	}()
	if err != nil {
		return nil, err
	}
	if _, err := out.Write(file.data); err != nil {
		return nil, err
	}
	return &sidecar.EvsFileInfo{
		Path: args.Path, Version: file.version, CommitVersion: file.commit, SignerSidecarId: testEvsSigner,
		Time: time.UnixMilli(1_800_000_000_000 + int64(file.commit)), Size: uint64(len(file.data)), HeadVersion: head,
	}, nil
}

// D-SDK-29's answer the test set, nil allowing.
func (self *testSidecarEvsBackend) MayDrop(repoId sidecar.Id, act sidecarEvsDropAct) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.drop.acts = append(self.drop.acts, act)
	return self.drop.mayDrop
}

// The core's dry run over the fake: one lineage, the path's every version listed below the head's next, which a
// written boundary keeps; kept for DropHistory to name, as the core keeps it. follow is refused as the core refuses it.
func (self *testSidecarEvsBackend) DropHistoryPreview(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsDropPreviewArgs) (*sidecar.EvsDropPreview, error) {
	if err := self.runHook(ctx, "drop_preview"); err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	copied := *args
	self.drop.previewArgs = append(self.drop.previewArgs, &copied)
	if args.Follow {
		return nil, fmt.Errorf("%w: --follow is not built", sidecar.ErrEvsDropPath)
	}
	if len(args.Paths) != 1 {
		return nil, fmt.Errorf("%w: %d paths", sidecar.ErrEvsDropPath, len(args.Paths))
	}
	versions := self.repoIdPaths[repoId][args.Paths[0]]
	if len(versions) == 0 {
		return nil, sidecar.ErrEvsPathAbsent
	}
	head := versions[len(versions)-1]
	lineage := &sidecar.EvsDropLineage{
		Path: args.Paths[0],
		DropPath: &protocol.EvsDropPath{
			Boundary: sidecar.EvsDropBoundaryWritten, ThroughVersion: head.version + 1, Count: uint32(len(versions)),
		},
	}
	for i := len(versions) - 1; 0 <= i; i -= 1 {
		lineage.Versions = append(lineage.Versions, &sidecar.EvsHistoryVersion{
			Version: versions[i].version, Time: time.UnixMilli(1_800_000_000_000 + int64(versions[i].commit)),
		})
	}
	previewId := sidecar.NewId()
	preview := &sidecar.EvsDropPreview{
		PreviewId: previewId.Bytes(), RefName: sidecar.EvsMainRefName, NamedPaths: slices.Clone(args.Paths),
		Lineages:     []*sidecar.EvsDropLineage{lineage},
		Batches:      []*sidecar.EvsDropPreviewBatch{{PreviewId: previewId.Bytes(), Lineages: []*sidecar.EvsDropLineage{lineage}}},
		VersionCount: uint64(len(versions)), FreedBytes: 4096, SharedBytes: 512, InlineBytes: 256, PackedBytes: 0, YoungBytes: 128,
	}
	self.drop.previews = append(self.drop.previews, preview)
	return preview, nil
}

// The core's DropHistory over the fake: a dry run it completed, the typed path its confirmation text, now only with its
// word and the word only with now (the core's own check, evs_drop_push.go), not_before 7 days on or now, the kept
// version written; the dry run forgotten once its drop commits.
func (self *testSidecarEvsBackend) DropHistory(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsDropHistoryArgs) (*sidecar.EvsDropHistoryResult, error) {
	if err := self.runHook(ctx, "drop"); err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	copied := *args
	self.drop.dropArgs = append(self.drop.dropArgs, &copied)
	index := slices.IndexFunc(self.drop.previews, func(preview *sidecar.EvsDropPreview) bool {
		return 0 < len(args.PreviewId) && bytes.Equal(preview.PreviewId, args.PreviewId)
	})
	if index < 0 {
		return nil, sidecar.ErrEvsDropPreviewMissing
	}
	preview := self.drop.previews[index]
	if args.ConfirmPath != sidecar.EvsDropConfirmText(preview) {
		return nil, fmt.Errorf("%w: the typed path is not the previewed one", sidecar.ErrEvsDropConfirm)
	}
	if args.Now != (args.ConfirmNow == sidecar.EvsDropConfirmNow) || (!args.Now && args.ConfirmNow != "") {
		return nil, fmt.Errorf("%w: now needs the word %s typed", sidecar.ErrEvsDropConfirm, sidecar.EvsDropConfirmNow)
	}
	notBefore := self.drop.clock().Add(sidecar.DefaultEvsFormatSettings().DropWindow)
	if args.Now {
		notBefore = self.drop.clock()
	}
	lineage := preview.Lineages[0]
	drop := &testEvsDrop{dropId: sidecar.NewId(), repoId: repoId, path: lineage.Path, notBefore: notBefore}
	for _, version := range lineage.Versions {
		if version.Version < lineage.DropPath.GetThroughVersion() && !version.Removed {
			drop.versions = append(drop.versions, version.Version)
		}
	}
	self.drop.drops = append(self.drop.drops, drop)
	self.drop.previews = slices.Delete(self.drop.previews, index, index+1)
	// a written boundary's DROP writes the head's content again as through_version, the kept version (§11.7's boundary
	// row), so the head is never one the drop lists
	if lineage.DropPath.GetBoundary() == sidecar.EvsDropBoundaryWritten {
		versions := self.repoIdPaths[repoId][lineage.Path]
		self.repoIdHeads[repoId] += 1
		self.repoIdPaths[repoId][lineage.Path] = append(versions, &testEvsFile{
			data: versions[len(versions)-1].data, version: lineage.DropPath.GetThroughVersion(), commit: self.repoIdHeads[repoId],
		})
	}
	return &sidecar.EvsDropHistoryResult{Pushes: []*sidecar.EvsDropPushResult{{
		PushId: sidecar.NewId(), DropId: drop.dropId, NotBefore: notBefore, PreviewId: args.PreviewId, Paths: []string{lineage.Path},
	}}}, nil
}

// The core's CancelDrop over the fake: a pending drop, refused within the core's DropCancelMargin of its not_before
// on the fake's clock.
func (self *testSidecarEvsBackend) CancelDrop(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsCancelDropArgs) (*sidecar.EvsCancelDropResult, error) {
	if err := self.runHook(ctx, "cancel"); err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	copied := *args
	self.drop.cancelArgs = append(self.drop.cancelArgs, &copied)
	for _, drop := range self.drop.drops {
		if drop.repoId != repoId || drop.dropId != args.DropId {
			continue
		}
		if drop.cancelled || drop.reported != 0 {
			return nil, sidecar.ErrEvsDropNotPending
		}
		if !self.drop.clock().Add(sidecar.DefaultEvsSettings().DropCancelMargin).Before(drop.notBefore) {
			return nil, fmt.Errorf("%w: within a minute of the drop's window end", sidecar.ErrEvsDropState)
		}
		drop.cancelled = true
		return &sidecar.EvsCancelDropResult{PushId: sidecar.NewId(), DropId: drop.dropId}, nil
	}
	return nil, sidecar.ErrEvsDropNotPending
}

// The core's Drops over the fake: each drop as evs's table lists it, with the paths it names, then the rows a test set.
func (self *testSidecarEvsBackend) Drops(ctx context.Context, repoId sidecar.Id) ([]*sidecar.EvsDrop, error) {
	if err := self.runHook(ctx, "drops"); err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.drop.dropsCount += 1
	listed := []*sidecar.EvsDrop{}
	for _, drop := range self.drop.drops {
		if drop.repoId != repoId {
			continue
		}
		state := drop.reported
		switch {
		case drop.cancelled:
			state = protocol.EvsDropState_EVS_DROP_CANCELLED
		case state == 0:
			state = protocol.EvsDropState_EVS_DROP_PENDING
		}
		listed = append(listed, &sidecar.EvsDrop{
			Status: &protocol.EvsDropStatus{
				DropId: drop.dropId.Bytes(), RefId: testEvsMainRef.Bytes(), PushId: sidecar.NewId().Bytes(), Version: 7,
				NotBeforeMs: drop.notBefore.UnixMilli(), State: state, FreedBytes: drop.freed,
			},
			Paths: []string{drop.path},
		})
	}
	for _, row := range self.drop.statuses {
		listed = append(listed, &sidecar.EvsDrop{Status: row.status, Paths: slices.Clone(row.paths)})
	}
	return listed, nil
}

// Previews path, failing the test on a refusal.
func testEvsPreview(t *testing.T, evs *SidecarEvs, repoId *Id, path string) *SidecarDropPreview {
	t.Helper()
	callback := newTestEvsCallback[*SidecarDropPreview]()
	evs.DropHistoryPreview(repoId, path, false, callback)
	preview, err := callback.await(t)
	if err != nil || preview == nil || !preview.Ok {
		t.Fatalf("preview of %s: %v %+v", path, err, preview)
	}
	return preview
}

// Drops the dry run's history and answers the result.
func testEvsDropHistory(t *testing.T, evs *SidecarEvs, repoId *Id, previewId *Id, confirmPath string, now bool) *SidecarResult {
	t.Helper()
	callback := newTestEvsCallback[*SidecarResult]()
	evs.DropHistory(repoId, previewId, confirmPath, now, callback)
	result, err := callback.await(t)
	if err != nil || result == nil {
		t.Fatalf("drop: %v %+v", err, result)
	}
	return result
}

// Writes count versions of path.
func testEvsWriteVersions(t *testing.T, evs *SidecarEvs, repoId *Id, path string, count int) {
	t.Helper()
	for i := 1; i <= count; i += 1 {
		if result := testEvsWrite(t, evs, repoId, path, []byte(fmt.Sprintf("synthetic version %d", i)), ""); !result.Ok {
			t.Fatalf("write %d: %+v", i, result)
		}
	}
}

// The drops the fake committed.
func (self *testSidecarEvsBackend) committedDrops() []*testEvsDrop {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.drop.drops)
}

// DropHistory signs only when confirmPath is the previewed path exactly as the person typed it: another path, the same
// one with a space or another case, or nothing, answers EVS_DROP_CONFIRM with nothing dropped, each passed to the core
// as typed; the previewed path drops, its drop id in Value; the dry run is then spent, and a drop naming no dry run
// answers EVS_DROP_PREVIEW_MISSING before the core is asked.
func TestConfirmPathMustMatch(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newTestSidecarEvs(t, backend, nil)
	repoId := testEvsRepoId(20)
	testEvsWriteVersions(t, evs, repoId, "notes/a.txt", 3)
	preview := testEvsPreview(t, evs, repoId, "notes/a.txt")
	if preview.ConfirmPath != "notes/a.txt" || preview.PreviewId == nil || !preview.ListMatch {
		t.Fatalf("preview %+v", preview)
	}
	typed := []string{"notes/b.txt", " notes/a.txt", "notes/a.txt ", "NOTES/A.TXT", "notes/a.txt\n", ""}
	for _, confirmPath := range typed {
		if result := testEvsDropHistory(t, evs, repoId, preview.PreviewId, confirmPath, false); result.Ok || result.Code != sidecarEvsCodeDropConfirm || result.Value != "" {
			t.Errorf("confirm %q: %+v", confirmPath, result)
		}
	}
	if drops := backend.committedDrops(); len(drops) != 0 {
		t.Fatalf("%d drops committed on a wrong confirmation", len(drops))
	}
	result := testEvsDropHistory(t, evs, repoId, preview.PreviewId, "notes/a.txt", false)
	drops := backend.committedDrops()
	if !result.Ok || len(drops) != 1 || result.Value != newId([16]byte(drops[0].dropId)).String() {
		t.Fatalf("the previewed path: %+v, %d drops", result, len(drops))
	}
	backend.stateLock.Lock()
	asked := []string{}
	for _, args := range backend.drop.dropArgs {
		asked = append(asked, args.ConfirmPath)
		if !bytes.Equal(args.PreviewId, preview.PreviewId.Bytes()) {
			t.Errorf("the core was asked for another dry run: %x", args.PreviewId)
		}
	}
	backend.stateLock.Unlock()
	if !slices.Equal(asked, append(slices.Clone(typed), "notes/a.txt")) {
		t.Errorf("the core was asked %q", asked)
	}
	// the dry run is spent, and a drop naming none never reaches the core
	if result := testEvsDropHistory(t, evs, repoId, preview.PreviewId, "notes/a.txt", false); result.Ok || result.Code != sidecarEvsCodeDropPreviewMissing {
		t.Errorf("a spent dry run: %+v", result)
	}
	if result := testEvsDropHistory(t, evs, repoId, nil, "notes/a.txt", false); result.Ok || result.Code != sidecarEvsCodeDropPreviewMissing {
		t.Errorf("no dry run: %+v", result)
	}
	backend.stateLock.Lock()
	defer backend.stateLock.Unlock()
	if len(backend.drop.dropArgs) != len(typed)+2 {
		t.Errorf("the core was asked %d drops", len(backend.drop.dropArgs))
	}
}

// now skips the 7-day window behind a second confirmation: the core takes now only with its word now typed, which
// this layer gives it for an app's now and never otherwise. A drop without now reaches the core with neither and waits
// its 7 days; a drop with now reaches it with both and has no window; the word never travels without now; and now
// stands in for no first confirmation, a wrong typed path refused with now as without it.
func TestNowNeedsSecondConfirmation(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newTestSidecarEvs(t, backend, nil)
	repoId := testEvsRepoId(21)
	testEvsWriteVersions(t, evs, repoId, "later.txt", 2)
	testEvsWriteVersions(t, evs, repoId, "at-once.txt", 2)
	now := testEvsDropBase.Add(24 * time.Hour)
	backend.setNow(now)

	later := testEvsPreview(t, evs, repoId, "later.txt")
	if result := testEvsDropHistory(t, evs, repoId, later.PreviewId, "later.txt", false); !result.Ok {
		t.Fatalf("a drop without now: %+v", result)
	}
	atOnce := testEvsPreview(t, evs, repoId, "at-once.txt")
	if result := testEvsDropHistory(t, evs, repoId, atOnce.PreviewId, "at-once.tx", true); result.Ok || result.Code != sidecarEvsCodeDropConfirm {
		t.Fatalf("now with a wrong typed path: %+v", result)
	}
	if result := testEvsDropHistory(t, evs, repoId, atOnce.PreviewId, "at-once.txt", true); !result.Ok {
		t.Fatalf("a drop with now: %+v", result)
	}

	backend.stateLock.Lock()
	for _, args := range backend.drop.dropArgs {
		if args.Now != (args.ConfirmNow == sidecar.EvsDropConfirmNow) || (!args.Now && args.ConfirmNow != "") {
			t.Errorf("the core was asked now %v with the word %q", args.Now, args.ConfirmNow)
		}
	}
	if len(backend.drop.dropArgs) != 3 || backend.drop.dropArgs[0].Now || !backend.drop.dropArgs[2].Now {
		t.Errorf("the core was asked %d drops", len(backend.drop.dropArgs))
	}
	backend.stateLock.Unlock()
	drops := backend.committedDrops()
	if len(drops) != 2 {
		t.Fatalf("%d drops", len(drops))
	}
	if window := drops[0].notBefore.Sub(now); window != 7*24*time.Hour {
		t.Errorf("a drop without now waits %s", window)
	}
	if !drops[1].notBefore.Equal(now) {
		t.Errorf("a drop with now waits until %s", drops[1].notBefore)
	}
}

// A racing write to a versionless path gets the 409 every write gets (§11.7's writers row): the app hears
// EVS_PATH_CONFLICT once, the core was asked once, and nothing writes the loser's content again over the winner's,
// which stands; the setting reached the core as asked.
func TestVersionless409Surfaced(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newTestSidecarEvs(t, backend, nil)
	repoId := testEvsRepoId(22)
	if result := testEvsWrite(t, evs, repoId, "draft.txt", []byte("the winner's content"), "off"); !result.Ok {
		t.Fatalf("the winner: %+v", result)
	}
	conflict := &sidecar.EvsPathConflictError{
		Refusal: &sidecar.EvsError{Status: http.StatusConflict, Code: protocol.EvsRefusalCode_EVS_REFUSAL_PATH_CONFLICT},
		Paths:   []string{"draft.txt"},
	}
	calls := 0
	backend.setHook(func(ctx context.Context, call string) error {
		if call != "write" {
			return nil
		}
		calls += 1
		return errors.Join(conflict, sidecar.ErrEvsVersionlessConflict)
	})
	result := testEvsWrite(t, evs, repoId, "draft.txt", []byte("the loser's content"), "")
	if result.Ok || result.Code != sidecarEvsCodePathConflict || result.Message == "" {
		t.Fatalf("the loser: %+v", result)
	}
	backend.setHook(nil)
	if calls != 1 {
		t.Errorf("the core was asked %d writes for one Write", calls)
	}
	if read := testEvsRead(t, evs, repoId, "draft.txt"); !read.Ok || string(read.Data) != "the winner's content" || read.Version != 1 {
		t.Errorf("after the 409: %+v", read)
	}
	backend.stateLock.Lock()
	defer backend.stateLock.Unlock()
	if !slices.Equal(backend.drop.versionings, []string{"off"}) {
		t.Errorf("the core wrote with versioning %q", backend.drop.versionings)
	}
}

// Write's versioning (§18.5, owner decision 167): "", "on" and "off" reach the core as asked; the core's refusal of off
// outside the format 2 gate is EVS_FORMAT_UNSUPPORTED, and evs's 409 VERSIONING EVS_VERSIONING, each answered once and
// never retried.
func TestSidecarEvsWriteVersioning(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newTestSidecarEvs(t, backend, nil)
	repoId := testEvsRepoId(23)
	for _, versioning := range []string{"", "on", "off"} {
		if result := testEvsWrite(t, evs, repoId, "file.txt", []byte(versioning+"x"), versioning); !result.Ok {
			t.Errorf("versioning %q: %+v", versioning, result)
		}
	}
	for _, c := range []struct {
		err  error
		code string
	}{
		{err: fmt.Errorf("%w: versioning off is format 2's, outside this network's gate", sidecar.ErrEvsFormatUnsupported), code: sidecarEvsCodeFormatUnsupported},
		{err: &sidecar.EvsError{Status: http.StatusConflict, Code: protocol.EvsRefusalCode_EVS_REFUSAL_VERSIONING}, code: sidecarEvsCodeVersioning},
		{err: fmt.Errorf("%w: %q", sidecar.ErrEvsVersioningSetting, "sometimes"), code: sidecarEvsCodeVersioning},
	} {
		calls := 0
		backend.setHook(func(ctx context.Context, call string) error {
			calls += 1
			return c.err
		})
		if result := testEvsWrite(t, evs, repoId, "file.txt", []byte("x"), "off"); result.Ok || result.Code != c.code || calls != 1 {
			t.Errorf("%v: %+v after %d calls", c.err, result, calls)
		}
	}
	backend.setHook(nil)
	backend.stateLock.Lock()
	defer backend.stateLock.Unlock()
	if !slices.Equal(backend.drop.versionings, []string{"", "on", "off"}) {
		t.Errorf("the core wrote with versioning %q", backend.drop.versionings)
	}
}

// A listed version reads as any other until its drop's not_before on the device's clock, History showing it
// drop_pending with the drop and that time; from not_before on ReadVersion answers EVS_DROPPED with no bytes and History
// shows it dropped, its row kept; a drop never takes the head, which reads at every time; a cancelled drop, and one evs
// reports refused, leave their versions readable and kept; a replaced versionless version answers EVS_COLLECTED; and a
// version below 1 is no version.
func TestDroppedFromNotBefore(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newTestSidecarEvs(t, backend, nil)
	repoId := testEvsRepoId(24)
	testEvsWriteVersions(t, evs, repoId, "report.txt", 3)
	start := testEvsDropBase.Add(2 * time.Hour)
	backend.setNow(start)
	preview := testEvsPreview(t, evs, repoId, "report.txt")
	if result := testEvsDropHistory(t, evs, repoId, preview.PreviewId, "report.txt", false); !result.Ok {
		t.Fatalf("drop: %+v", result)
	}
	drop := backend.committedDrops()[0]
	readVersion := func(version int64) *SidecarBytes {
		t.Helper()
		callback := newTestEvsCallback[*SidecarBytes]()
		evs.ReadVersion(repoId, "report.txt", version, callback)
		read, err := callback.await(t)
		if err != nil || read == nil {
			t.Fatalf("read %d: %v %+v", version, err, read)
		}
		return read
	}
	history := func() []*SidecarHistoryVersion {
		t.Helper()
		callback := newTestEvsCallback[*SidecarHistory]()
		evs.History(repoId, "report.txt", callback)
		answer, err := callback.await(t)
		if err != nil || !answer.Ok {
			t.Fatalf("history: %v %+v", err, answer)
		}
		rows := []*SidecarHistoryVersion{}
		for i := 0; i < answer.Versions.Len(); i += 1 {
			rows = append(rows, answer.Versions.Get(i))
		}
		return rows
	}
	dropId := newId([16]byte(drop.dropId)).String()
	requireStates := func(name string, want map[int64]string) {
		t.Helper()
		for _, row := range history() {
			if row.State != want[row.Version] {
				t.Errorf("%s: version %d reads %q, want %q", name, row.Version, row.State, want[row.Version])
			}
			listed := row.State == sidecar.EvsVersionStateDropPending || row.State == sidecar.EvsVersionStateDropped
			if listed && (row.DropId == nil || row.DropId.String() != dropId || row.DropNotBeforeMillis != drop.notBefore.UnixMilli()) {
				t.Errorf("%s: version %d names drop %v until %d", name, row.Version, row.DropId, row.DropNotBeforeMillis)
			}
			if !listed && (row.DropId != nil || row.DropNotBeforeMillis != 0) {
				t.Errorf("%s: version %d names a drop it is not in", name, row.Version)
			}
		}
	}

	// before not_before: readable, drop_pending; the drop listed every version, the head's content written again by
	// its push as version 4, which it keeps
	for _, version := range []int64{1, 2, 3} {
		if read := readVersion(version); !read.Ok || string(read.Data) != fmt.Sprintf("synthetic version %d", version) || read.Version != version {
			t.Errorf("pending: version %d %+v", version, read)
		}
	}
	requireStates("pending", map[int64]string{1: "drop_pending", 2: "drop_pending", 3: "drop_pending", 4: "kept"})

	// at not_before and after: dropped, nothing read, rows kept; the head reads at every time
	for _, at := range []time.Time{drop.notBefore, drop.notBefore.Add(time.Minute)} {
		backend.setNow(at)
		for _, version := range []int64{1, 2, 3} {
			if read := readVersion(version); read.Ok || read.Code != sidecarEvsCodeDropped || read.Data != nil {
				t.Errorf("at %s: version %d %+v", at, version, read)
			}
		}
		requireStates("dropped", map[int64]string{1: "dropped", 2: "dropped", 3: "dropped", 4: "kept"})
		if read := testEvsRead(t, evs, repoId, "report.txt"); !read.Ok || read.Version != 4 || string(read.Data) != "synthetic version 3" {
			t.Errorf("the head at %s: %+v", at, read)
		}
		if read := readVersion(4); !read.Ok || read.Version != 4 {
			t.Errorf("the kept version at %s: %+v", at, read)
		}
	}

	// a cancelled drop, and one evs reports refused, list nothing: the versions read and are kept
	backend.stateLock.Lock()
	drop.cancelled = true
	backend.stateLock.Unlock()
	if read := readVersion(1); !read.Ok {
		t.Errorf("a cancelled drop's version: %+v", read)
	}
	requireStates("cancelled", map[int64]string{1: "kept", 2: "kept", 3: "kept", 4: "kept"})
	backend.stateLock.Lock()
	drop.cancelled, drop.reported = false, protocol.EvsDropState_EVS_DROP_REFUSED
	backend.stateLock.Unlock()
	if read := readVersion(2); !read.Ok {
		t.Errorf("a refused drop's version: %+v", read)
	}
	requireStates("refused", map[int64]string{1: "kept", 2: "kept", 3: "kept", 4: "kept"})

	// a replaced versionless version, and a version that is no number
	backend.stateLock.Lock()
	backend.drop.collected = map[string]bool{"report.txt": true}
	backend.stateLock.Unlock()
	if read := readVersion(1); read.Ok || read.Code != sidecarEvsCodeCollected {
		t.Errorf("collected: %+v", read)
	}
	for _, version := range []int64{0, -1} {
		if read := readVersion(version); read.Ok || read.Code != sidecarEvsCodeInvalid {
			t.Errorf("version %d: %+v", version, read)
		}
	}
}

// A drop a hold defers reads done, as everywhere (owner decision 184): evs's done for a held drop, its held bytes
// counted as freed, and its observe mode's both list as done; a state this sdk does not know reads unknown; no field
// of a listed drop could carry a hold, its phase and progress never crossing; and a held drop's listed version past
// not_before answers EVS_DROPPED although the fake still holds its bytes, so nothing shows what a hold kept.
func TestHeldDropReadsDone(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newTestSidecarEvs(t, backend, nil)
	repoId := testEvsRepoId(25)
	testEvsWriteVersions(t, evs, repoId, "held.txt", 2)
	preview := testEvsPreview(t, evs, repoId, "held.txt")
	if result := testEvsDropHistory(t, evs, repoId, preview.PreviewId, "held.txt", false); !result.Ok {
		t.Fatalf("drop: %+v", result)
	}
	held := backend.committedDrops()[0]
	backend.stateLock.Lock()
	// what evs reports of a drop a hold defers: done, the bytes the hold keeps counted as freed (§11.20's drops row)
	held.reported, held.freed = protocol.EvsDropState_EVS_DROP_DONE, 8192
	executing := &protocol.EvsDropStatus{
		DropId: sidecar.NewId().Bytes(), RefId: testEvsMainRef.Bytes(), Version: 9, NotBeforeMs: 1, State: protocol.EvsDropState_EVS_DROP_EXECUTING,
		Phase: protocol.EvsDropPhase_EVS_DROP_HORIZON, Progress: 3, ProgressTotal: 10,
	}
	backend.drop.statuses = []*testEvsDropRow{
		{status: &protocol.EvsDropStatus{DropId: sidecar.NewId().Bytes(), RefId: testEvsMainRef.Bytes(), State: protocol.EvsDropState_EVS_DROP_OBSERVED, FreedBytes: 0}},
		{status: executing},
		{status: &protocol.EvsDropStatus{DropId: sidecar.NewId().Bytes(), RefId: testEvsMainRef.Bytes(), State: protocol.EvsDropState(99)}},
		{status: &protocol.EvsDropStatus{DropId: []byte{1, 2, 3}, State: protocol.EvsDropState_EVS_DROP_REFUSED, Reason: protocol.EvsDropRefusal(1)}},
	}
	backend.stateLock.Unlock()
	backend.setNow(held.notBefore.Add(time.Hour))

	callback := newTestEvsCallback[*SidecarDrops]()
	evs.Drops(repoId, callback)
	answer, err := callback.await(t)
	if err != nil || !answer.Ok || answer.Drops.Len() != 5 {
		t.Fatalf("drops: %v %+v", err, answer)
	}
	states := []string{}
	for i := 0; i < answer.Drops.Len(); i += 1 {
		states = append(states, answer.Drops.Get(i).State)
	}
	if !slices.Equal(states, []string{"done", "done", "executing", "unknown", "refused"}) {
		t.Errorf("states %q", states)
	}
	first := answer.Drops.Get(0)
	if first.DropId.String() != newId([16]byte(held.dropId)).String() || first.FreedBytes != 8192 || first.NotBeforeMillis != held.notBefore.UnixMilli() ||
		first.RefId.String() != newId([16]byte(testEvsMainRef)).String() || first.Version != 7 || first.Paths.Len() != 1 || first.Paths.Get(0) != "held.txt" {
		t.Errorf("the held drop %+v", first)
	}
	if malformed := answer.Drops.Get(4); malformed.DropId != nil || malformed.RefId != nil {
		t.Errorf("a row with no 16-byte id names %v on %v", malformed.DropId, malformed.RefId)
	}
	// no field could carry a hold: a listed drop's fields are these, the status's phase, progress and reason left out
	fields := []string{}
	for i := 0; i < reflect.TypeOf(SidecarDrop{}).NumField(); i += 1 {
		fields = append(fields, strings.Split(reflect.TypeOf(SidecarDrop{}).Field(i).Tag.Get("json"), ",")[0])
	}
	if !slices.Equal(fields, []string{"drop_id", "ref_id", "version", "state", "not_before_millis", "freed_bytes", "paths"}) {
		t.Errorf("a listed drop's fields %q", fields)
	}
	read := newTestEvsCallback[*SidecarBytes]()
	evs.ReadVersion(repoId, "held.txt", 1, read)
	if result, _ := read.await(t); result.Ok || result.Code != sidecarEvsCodeDropped || result.Data != nil {
		t.Errorf("a held drop's version past its window: %+v", result)
	}
}

// D-SDK-29 before the core: a preview, a drop and a cancel each ask this device's check first, with its act, and one
// it refuses answers EVS_DROP_REFUSED with the core asked nothing; the Drops listing is any reader's and asks nothing.
func TestDropCallsAskMayDropFirst(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newTestSidecarEvs(t, backend, nil)
	repoId := testEvsRepoId(26)
	testEvsWriteVersions(t, evs, repoId, "vault/secret-note.txt", 2)
	preview := testEvsPreview(t, evs, repoId, "vault/secret-note.txt")
	backend.stateLock.Lock()
	backend.drop.mayDrop = fmt.Errorf("%w: only a repo admin drops history (unconsented)", sidecar.ErrEvsDropRefused)
	backend.drop.acts = nil
	backend.stateLock.Unlock()

	previewed := newTestEvsCallback[*SidecarDropPreview]()
	evs.DropHistoryPreview(repoId, "vault/secret-note.txt", false, previewed)
	if result, _ := previewed.await(t); result.Ok || result.Code != sidecarEvsCodeDropRefused || result.PreviewId != nil {
		t.Errorf("preview: %+v", result)
	}
	if result := testEvsDropHistory(t, evs, repoId, preview.PreviewId, "vault/secret-note.txt", false); result.Ok || result.Code != sidecarEvsCodeDropRefused {
		t.Errorf("drop: %+v", result)
	}
	cancelled := newTestEvsCallback[*SidecarResult]()
	evs.CancelDrop(repoId, NewId(), cancelled)
	if result, _ := cancelled.await(t); result.Ok || result.Code != sidecarEvsCodeDropRefused {
		t.Errorf("cancel: %+v", result)
	}
	listed := newTestEvsCallback[*SidecarDrops]()
	evs.Drops(repoId, listed)
	if result, _ := listed.await(t); !result.Ok {
		t.Errorf("the listing: %+v", result)
	}
	backend.stateLock.Lock()
	defer backend.stateLock.Unlock()
	if !slices.Equal(backend.drop.acts, []sidecarEvsDropAct{sidecarEvsDropActPreview, sidecarEvsDropActDrop, sidecarEvsDropActCancel}) {
		t.Errorf("acts asked %v", backend.drop.acts)
	}
	if len(backend.drop.previewArgs) != 1 || len(backend.drop.dropArgs) != 0 || len(backend.drop.cancelArgs) != 0 || backend.drop.dropsCount != 1 {
		t.Errorf("the core was asked %d previews, %d drops, %d cancels and %d listings",
			len(backend.drop.previewArgs), len(backend.drop.dropArgs), len(backend.drop.cancelArgs), backend.drop.dropsCount)
	}
}

// A verified state's answers, for the gate.
type testEvsDropState struct {
	decision sidecar.AclDecision
	locked   bool
	asked    []sidecar.Id
}

func (self *testEvsDropState) MayDrop(subjectId sidecar.Id, repoId sidecar.Id, now time.Time) sidecar.AclDecision {
	self.asked = append(self.asked, subjectId)
	return self.decision
}

func (self *testEvsDropState) RepoLockAllowsDrop(repoId sidecar.Id, cancel bool) bool {
	return cancel || !self.locked
}

// The gate fails closed: no gate, a host with no verified state (no config, a config with no view, a view that is no
// verified ConfigState), and no identity each refuse every act, as does the client backend no composition gave a gate.
// Over a state that answers, the subject asked is this device's sidecar; MayDrop's refusal refuses all three acts with
// its reason; and a lock refuses the drop alone, never the preview or the cancel.
func TestDropGateFailsClosed(t *testing.T) {
	repoId := sidecar.Id{0xd1}
	acts := []sidecarEvsDropAct{sidecarEvsDropActPreview, sidecarEvsDropActDrop, sidecarEvsDropActCancel}
	identity := func() *sidecar.SidecarIdentity {
		return &sidecar.SidecarIdentity{SidecarId: sidecar.Id{0xd2}}
	}
	now := func() time.Time {
		return testEvsDropBase
	}
	allowing := func() (sidecarEvsDropState, error) {
		return &testEvsDropState{decision: sidecar.AclDecision{Allowed: true}}, nil
	}
	failing := func() (sidecarEvsDropState, error) {
		return nil, errors.New("a test's rows do not build")
	}
	nobody := func() *sidecar.SidecarIdentity {
		return nil
	}
	closed := map[string]*sidecarEvsDropGate{
		"no gate":         nil,
		"no config":       newSidecarEvsDropGate(nil, identity, now),
		"no view":         newSidecarEvsDropGate(func() sidecar.GkmAuthView { return nil }, identity, now),
		"no state":        {identity: identity, now: now},
		"no identity":     {state: allowing, now: now},
		"enrolled nobody": {state: allowing, identity: nobody, now: now},
		"a state error":   {state: failing, identity: identity, now: now},
	}
	for name, gate := range closed {
		for _, act := range acts {
			if err := gate.check(repoId, act); !errors.Is(err, sidecar.ErrEvsDropRefused) {
				t.Errorf("%s, act %d: %v", name, act, err)
			}
		}
	}
	for _, act := range acts {
		if err := (&sidecarEvsClientBackend{}).MayDrop(repoId, act); !errors.Is(err, sidecar.ErrEvsDropRefused) {
			t.Errorf("the client backend with no gate, act %d: %v", act, err)
		}
	}

	state := &testEvsDropState{decision: sidecar.AclDecision{Allowed: true}}
	gate := &sidecarEvsDropGate{state: func() (sidecarEvsDropState, error) { return state, nil }, identity: identity, now: now}
	for _, act := range acts {
		if err := gate.check(repoId, act); err != nil {
			t.Errorf("an admin, act %d: %v", act, err)
		}
	}
	if len(state.asked) != 3 || state.asked[0] != (sidecar.Id{0xd2}) {
		t.Errorf("the state was asked for %v", state.asked)
	}
	state.decision = sidecar.AclDecision{Reason: sidecar.AclReasonUnconsented}
	for _, act := range acts {
		if err := gate.check(repoId, act); !errors.Is(err, sidecar.ErrEvsDropRefused) || !strings.Contains(err.Error(), sidecar.AclReasonUnconsented.String()) {
			t.Errorf("an unconsented admin, act %d: %v", act, err)
		}
	}
	state.decision, state.locked = sidecar.AclDecision{Allowed: true}, true
	for _, act := range acts {
		err := gate.check(repoId, act)
		if (act == sidecarEvsDropActDrop) != errors.Is(err, sidecar.ErrEvsDropRefused) {
			t.Errorf("under a lock, act %d: %v", act, err)
		}
	}
}

// CancelDrop until 60 s before the drop's not_before on the device's clock (owner decision 183): a minute and a second
// before cancels and lists the drop cancelled; a minute before answers EVS_DROP_STATE; a drop no longer pending
// EVS_DROP_NOT_PENDING; a cancel naming no drop EVS_INVALID before the core is asked.
func TestCancelDropUntilSixtySecondsBefore(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newTestSidecarEvs(t, backend, nil)
	repoId := testEvsRepoId(27)
	testEvsWriteVersions(t, evs, repoId, "one.txt", 2)
	testEvsWriteVersions(t, evs, repoId, "two.txt", 2)
	for _, path := range []string{"one.txt", "two.txt"} {
		preview := testEvsPreview(t, evs, repoId, path)
		if result := testEvsDropHistory(t, evs, repoId, preview.PreviewId, path, false); !result.Ok {
			t.Fatalf("drop %s: %+v", path, result)
		}
	}
	drops := backend.committedDrops()
	cancel := func(dropId *Id) *SidecarResult {
		t.Helper()
		callback := newTestEvsCallback[*SidecarResult]()
		evs.CancelDrop(repoId, dropId, callback)
		result, err := callback.await(t)
		if err != nil || result == nil {
			t.Fatalf("cancel: %v %+v", err, result)
		}
		return result
	}
	first, second := newId([16]byte(drops[0].dropId)), newId([16]byte(drops[1].dropId))
	backend.setNow(drops[0].notBefore.Add(-61 * time.Second))
	if result := cancel(first); !result.Ok || result.Value != first.String() {
		t.Errorf("61 s before: %+v", result)
	}
	if result := cancel(first); result.Ok || result.Code != sidecarEvsCodeDropNotPending {
		t.Errorf("a cancelled drop again: %+v", result)
	}
	backend.setNow(drops[1].notBefore.Add(-60 * time.Second))
	if result := cancel(second); result.Ok || result.Code != sidecarEvsCodeDropState {
		t.Errorf("60 s before: %+v", result)
	}
	if result := cancel(nil); result.Ok || result.Code != sidecarEvsCodeInvalid {
		t.Errorf("no drop: %+v", result)
	}
	listed := newTestEvsCallback[*SidecarDrops]()
	evs.Drops(repoId, listed)
	answer, _ := listed.await(t)
	if !answer.Ok || answer.Drops.Len() != 2 || answer.Drops.Get(0).State != "cancelled" || answer.Drops.Get(1).State != "pending" {
		t.Errorf("after the cancels: %+v", answer)
	}
	backend.stateLock.Lock()
	defer backend.stateLock.Unlock()
	if len(backend.drop.cancelArgs) != 3 {
		t.Errorf("the core was asked %d cancels", len(backend.drop.cancelArgs))
	}
}

// The dry run's figures: evs's bytes and counts, the confirmation text, one push, the lineage's boundary and its
// listed versions' dates; a mismatch stops it EVS_DROP_LIST_MISMATCH with ListMatch false, follow is refused as the
// core refuses it until built, a history past a head-mode walk answers EVS_DROP_TOO_LARGE, and a removed lineage's
// removal is no listed version.
func TestDropPreviewFigures(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newTestSidecarEvs(t, backend, nil)
	repoId := testEvsRepoId(28)
	testEvsWriteVersions(t, evs, repoId, "figures.txt", 3)
	preview := testEvsPreview(t, evs, repoId, "figures.txt")
	if preview.VersionCount != 3 || preview.FreedBytes != 4096 || preview.SharedBytes != 512 || preview.InlineBytes != 256 ||
		preview.PackedBytes != 0 || preview.YoungBytes != 128 || preview.PushCount != 1 || preview.Lineages.Len() != 1 {
		t.Fatalf("figures %+v", preview)
	}
	lineage := preview.Lineages.Get(0)
	if lineage.Path != "figures.txt" || lineage.VersionCount != 3 || lineage.Boundary != "written" ||
		lineage.OldestMillis != 1_800_000_000_001 || lineage.NewestMillis != 1_800_000_000_003 ||
		preview.OldestMillis != lineage.OldestMillis || preview.NewestMillis != lineage.NewestMillis {
		t.Errorf("lineage %+v", lineage)
	}

	// a removed boundary: the removal stays and is no listed version
	removed := sidecarDropLineageOf(&sidecar.EvsDropLineage{
		Path:     "gone.txt",
		DropPath: &protocol.EvsDropPath{Boundary: sidecar.EvsDropBoundaryRemoved, ThroughVersion: 3, Count: 2},
		Versions: []*sidecar.EvsHistoryVersion{
			{Version: 3, Removed: true, Time: time.UnixMilli(3_000)},
			{Version: 2, Time: time.UnixMilli(2_000)},
			{Version: 1, Time: time.UnixMilli(1_000)},
		},
	})
	if removed.Boundary != "removed" || removed.VersionCount != 2 || removed.OldestMillis != 1_000 || removed.NewestMillis != 2_000 {
		t.Errorf("a removed lineage %+v", removed)
	}

	for _, c := range []struct {
		err  error
		code string
	}{
		{err: fmt.Errorf("%w: lineage 0 lists 3 versions where evs resolved 4", sidecar.ErrEvsDropListMismatch), code: sidecarEvsCodeDropListMismatch},
		{err: fmt.Errorf("%w: past 4 MiB of statements: run ur history drop on a device that mirrors the repo", sidecar.ErrEvsDropTooLarge), code: sidecarEvsCodeDropTooLarge},
		{err: fmt.Errorf("%w: lineage 0", sidecar.ErrEvsDropUnlinked), code: sidecarEvsCodeDropUnlinked},
	} {
		backend.setHook(func(ctx context.Context, call string) error {
			return c.err
		})
		callback := newTestEvsCallback[*SidecarDropPreview]()
		evs.DropHistoryPreview(repoId, "figures.txt", false, callback)
		if result, _ := callback.await(t); result.Ok || result.Code != c.code || result.ListMatch || result.PreviewId != nil {
			t.Errorf("%v: %+v", c.err, result)
		}
	}
	backend.setHook(nil)
	followed := newTestEvsCallback[*SidecarDropPreview]()
	evs.DropHistoryPreview(repoId, "figures.txt", true, followed)
	if result, _ := followed.await(t); result.Ok || result.Code != sidecarEvsCodeDropPath {
		t.Errorf("follow: %+v", result)
	}
	backend.stateLock.Lock()
	defer backend.stateLock.Unlock()
	if last := backend.drop.previewArgs[len(backend.drop.previewArgs)-1]; !last.Follow || !slices.Equal(last.Paths, []string{"figures.txt"}) {
		t.Errorf("follow reached the core as %+v", last)
	}
}

// Each of the core's format 2 refusals answers its code, a drop's own before what it wraps: a stale drop on a 409 is
// EVS_DROP_STALE; evs's own drop refusals keep their names.
func TestSidecarEvsFormat2Codes(t *testing.T) {
	conflict := &sidecar.EvsPathConflictError{Refusal: &sidecar.EvsError{Status: http.StatusConflict, Code: protocol.EvsRefusalCode_EVS_REFUSAL_PATH_CONFLICT}}
	cases := []struct {
		err  error
		code string
	}{
		{err: sidecar.ErrEvsDropConfirm, code: "EVS_DROP_CONFIRM"},
		{err: sidecar.ErrEvsDropPreviewMissing, code: "EVS_DROP_PREVIEW_MISSING"},
		{err: sidecar.ErrEvsDropRefused, code: "EVS_DROP_REFUSED"},
		{err: fmt.Errorf("%w: %w", sidecar.ErrEvsDropStale, conflict), code: "EVS_DROP_STALE"},
		{err: sidecar.ErrEvsDropNotPending, code: "EVS_DROP_NOT_PENDING"},
		{err: sidecar.ErrEvsDropState, code: "EVS_DROP_STATE"},
		{err: sidecar.ErrEvsDropListMismatch, code: "EVS_DROP_LIST_MISMATCH"},
		{err: sidecar.ErrEvsDropUnlinked, code: "EVS_DROP_UNLINKED"},
		{err: sidecar.ErrEvsDropTooLarge, code: "EVS_DROP_TOO_LARGE"},
		{err: sidecar.ErrEvsDropPath, code: "EVS_DROP_PATH"},
		{err: sidecar.ErrEvsDropList, code: "EVS_DROP_LIST"},
		{err: fmt.Errorf("read: %w", sidecar.ErrEvsDropped), code: "EVS_DROPPED"},
		{err: sidecar.ErrEvsCollected, code: "EVS_COLLECTED"},
		{err: fmt.Errorf("read: %w", sidecar.ErrEvsErased), code: "EVS_ERASED"},
		{err: sidecar.ErrEvsVersioningSetting, code: "EVS_VERSIONING"},
		{err: &sidecar.EvsError{Status: http.StatusConflict, Code: protocol.EvsRefusalCode_EVS_REFUSAL_DROP_CONSENT}, code: "EVS_DROP_CONSENT"},
		{err: &sidecar.EvsError{Status: http.StatusConflict, Code: protocol.EvsRefusalCode_EVS_REFUSAL_DROP_KIND}, code: "EVS_DROP_KIND"},
		{err: &sidecar.EvsError{Status: http.StatusConflict, Code: protocol.EvsRefusalCode_EVS_REFUSAL_DROP_STATE}, code: "EVS_DROP_STATE"},
		{err: &sidecar.EvsError{Status: http.StatusConflict, Code: protocol.EvsRefusalCode_EVS_REFUSAL_DROPPED}, code: "EVS_DROPPED"},
	}
	for _, c := range cases {
		if code := sidecarEvsCodeOf(c.err); code != c.code {
			t.Errorf("%v: %q, want %q", c.err, code, c.code)
		}
	}
}
