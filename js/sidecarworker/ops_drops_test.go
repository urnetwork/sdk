//go:build !sidecar_http && !js

package sidecarworker

// The console's drop_states (SDK-M10-05; sidecar §18.8's pages and messages rows, D-SDK-29, owner decision 184): each
// drop of a repo as the page reads it, its state by the root's mapping with a held drop's done, the names the device
// gives it, a pending drop's device command on the ref asked; its bounds and refusals; and that a page Worker never
// dry-runs, signs or cancels a drop, by its operations, its seam's methods and what a run reaches.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
)

// A repo's drops on fakes, recording each call, with the calls a page Worker must never reach beside them.
type testWorkerDrops struct {
	listing *sidecar.EvsListing
	known   []string
	drops   []*sidecar.EvsDrop
	// answered by the call of that name, before the fake answers
	errs      map[string]error
	calls     []string
	refNames  [][]byte
	dropsArgs []*sidecar.EvsDropsArgs
}

func (self *testWorkerDrops) answer(call string, refName []byte) error {
	self.calls = append(self.calls, call)
	self.refNames = append(self.refNames, refName)
	return self.errs[call]
}

func (self *testWorkerDrops) List(ctx context.Context, args *sidecar.EvsReadArgs) (*sidecar.EvsListing, error) {
	if err := self.answer("List", args.RefName); err != nil {
		return nil, err
	}
	return self.listing, nil
}

func (self *testWorkerDrops) KnownPaths(ctx context.Context, refName []byte) ([]string, error) {
	if err := self.answer("KnownPaths", refName); err != nil {
		return nil, err
	}
	return slices.Clone(self.known), nil
}

func (self *testWorkerDrops) Drops(ctx context.Context, args *sidecar.EvsDropsArgs) ([]*sidecar.EvsDrop, error) {
	if err := self.answer("Drops", args.RefName); err != nil {
		return nil, err
	}
	copied := *args
	self.dropsArgs = append(self.dropsArgs, &copied)
	return self.drops, nil
}

// The repo's own calls that act, which the seam leaves out: a run that reached one through the concrete type would
// show here.
func (self *testWorkerDrops) DropHistoryPreview(ctx context.Context, args *sidecar.EvsDropPreviewArgs) (*sidecar.EvsDropPreview, error) {
	self.calls = append(self.calls, "DropHistoryPreview")
	return nil, errors.New("a page Worker dry-ran a drop")
}

func (self *testWorkerDrops) DropHistory(ctx context.Context, args *sidecar.EvsDropHistoryArgs) (*sidecar.EvsDropHistoryResult, error) {
	self.calls = append(self.calls, "DropHistory")
	return nil, errors.New("a page Worker signed a drop")
}

func (self *testWorkerDrops) CancelDrop(ctx context.Context, args *sidecar.EvsCancelDropArgs) (*sidecar.EvsCancelDropResult, error) {
	self.calls = append(self.calls, "CancelDrop")
	return nil, errors.New("a page Worker cancelled a drop")
}

// A world whose Worker reads drops through drops.
func newTestDropsWorld(t *testing.T, drops *testWorkerDrops, adjust func(args *WorkerArgs)) *testWorkerWorld {
	t.Helper()
	return newTestWorkerWorld(t, func(args *WorkerArgs) {
		args.Drops = func(repoId sidecar.Id) (WorkerDrops, error) {
			return drops, nil
		}
		if adjust != nil {
			adjust(args)
		}
	})
}

// A drop_states request for the world's repo.
func (self *testWorkerWorld) dropStatesRequest(id string, ref string) *Request {
	return &Request{Id: id, Operation: OperationDropStates, RepoId: self.repoId.String(), Ref: ref}
}

// drop_states shows each drop with its ids, its version and freed bytes as decimals past 2^53, its window's end and
// its state by the root's mapping: done for evs's done, a held drop's included, and for its observe mode; unknown for a
// state the Worker does not know; and nothing of a status's phase, progress or reason. Names that are not UTF-8 cross
// as base64 too. A pending drop on the ref asked carries the device command that cancels it, the ref quoted for a
// shell; a pending one on another ref, and every other state, carries none. The ref asked reaches each read, and the
// known paths the listing.
func TestDropStatesView(t *testing.T) {
	refId, otherRefId := sidecar.NewId(), sidecar.NewId()
	raw := []byte{'r', 0xfe, '.', 't', 'x', 't'}
	status := func(state protocol.EvsDropState, ref sidecar.Id) *protocol.EvsDropStatus {
		return &protocol.EvsDropStatus{DropId: sidecar.NewId().Bytes(), RefId: ref.Bytes(), PushId: sidecar.NewId().Bytes(), Version: 5, NotBeforeMs: 1_800_604_800_000, State: state}
	}
	pending := status(protocol.EvsDropState_EVS_DROP_PENDING, refId)
	pending.Version, pending.FreedBytes = math.MaxUint64, math.MaxUint64
	executing := status(protocol.EvsDropState_EVS_DROP_EXECUTING, refId)
	executing.Phase, executing.Progress, executing.ProgressTotal = protocol.EvsDropPhase_EVS_DROP_HORIZON, 4, 9
	held := status(protocol.EvsDropState_EVS_DROP_DONE, refId)
	held.FreedBytes = 65536
	refused := status(protocol.EvsDropState_EVS_DROP_REFUSED, refId)
	refused.Reason = protocol.EvsDropRefusal(1)
	malformed := status(protocol.EvsDropState_EVS_DROP_PENDING, refId)
	malformed.DropId = []byte{1, 2, 3}
	drops := &testWorkerDrops{
		listing: &sidecar.EvsListing{RefId: refId},
		known:   []string{"notes/a.txt", string(raw)},
		drops: []*sidecar.EvsDrop{
			{Status: pending, Paths: []string{"notes/a.txt", string(raw)}},
			{Status: status(protocol.EvsDropState_EVS_DROP_CANCELLED, refId)},
			{Status: executing},
			{Status: held, Paths: []string{"notes/a.txt"}},
			{Status: status(protocol.EvsDropState_EVS_DROP_OBSERVED, refId)},
			{Status: refused},
			{Status: status(protocol.EvsDropState(99), refId)},
			{Status: status(protocol.EvsDropState_EVS_DROP_PENDING, otherRefId)},
			{Status: malformed},
		},
	}
	world := newTestDropsWorld(t, drops, nil)
	reply := testReplyOf(t, world.worker, world.dropStatesRequest("drops", "release 1's"))
	view := reply.DropStates
	if !reply.Ok || view == nil || view.RepoId != world.repoId.String() || len(view.Drops) != 9 || view.Truncated {
		t.Fatalf("drop_states: %+v", reply)
	}
	states := []string{}
	for _, drop := range view.Drops {
		states = append(states, drop.State)
	}
	if !slices.Equal(states, []string{"pending", "cancelled", "executing", "done", "done", "refused", "unknown", "pending", "pending"}) {
		t.Errorf("states %q", states)
	}
	first := view.Drops[0]
	pendingId, _ := sidecar.IdFromBytes(pending.DropId)
	if first.DropId != pendingId.String() || first.RefId != refId.String() || first.Version != strconv.FormatUint(math.MaxUint64, 10) ||
		first.FreedBytes != strconv.FormatUint(math.MaxUint64, 10) || first.NotBeforeMillis != 1_800_604_800_000 {
		t.Errorf("the pending drop %+v", first)
	}
	if len(first.Paths) != 2 || first.Paths[0].Text != "notes/a.txt" || first.Paths[0].Base64 != "" ||
		first.Paths[1].Base64 != base64.StdEncoding.EncodeToString(raw) {
		t.Errorf("the pending drop's names %+v", first.Paths)
	}
	if want := "ur history cancel " + world.repoId.String() + " " + pendingId.String() + ` --ref='release 1'\''s'`; first.DeviceCommand != want {
		t.Errorf("the device command %q, want %q", first.DeviceCommand, want)
	}
	if view.Drops[3].FreedBytes != "65536" || len(view.Drops[3].Paths) != 1 {
		t.Errorf("the held drop %+v", view.Drops[3])
	}
	for i, drop := range view.Drops[1:] {
		if drop.DeviceCommand != "" {
			t.Errorf("drop %d (%s) carries the command %q", i+1, drop.State, drop.DeviceCommand)
		}
	}
	if view.Drops[8].DropId != "" || view.Drops[1].Paths == nil || len(view.Drops[1].Paths) != 0 {
		t.Errorf("a row with no id %+v, one with no names %+v", view.Drops[8], view.Drops[1])
	}
	// a drop's view is these fields: no phase, progress, reason or anything else a hold could show in
	fields := []string{}
	for i := 0; i < reflect.TypeOf(DropStateView{}).NumField(); i += 1 {
		fields = append(fields, strings.Split(reflect.TypeOf(DropStateView{}).Field(i).Tag.Get("json"), ",")[0])
	}
	if !slices.Equal(fields, []string{"drop_id", "ref_id", "version", "state", "not_before_millis", "freed_bytes", "paths", "device_command"}) {
		t.Errorf("a drop's view %q", fields)
	}
	if !slices.Equal(drops.calls, []string{"List", "KnownPaths", "Drops"}) {
		t.Errorf("the reads %q", drops.calls)
	}
	for i, refName := range drops.refNames {
		if string(refName) != "release 1's" {
			t.Errorf("read %d named the ref %q", i, refName)
		}
	}
	if !slices.Equal(drops.dropsArgs[0].KnownPaths, drops.known) {
		t.Errorf("the listing matched %q", drops.dropsArgs[0].KnownPaths)
	}

	// main: the command names no ref
	drops.calls, drops.refNames = nil, nil
	reply = testReplyOf(t, world.worker, world.dropStatesRequest("main", ""))
	if want := "ur history cancel " + world.repoId.String() + " " + pendingId.String(); !reply.Ok || reply.DropStates.Drops[0].DeviceCommand != want {
		t.Errorf("on main: %+v", reply.DropStates.Drops[0])
	}
	if drops.refNames[0] != nil {
		t.Errorf("main read as the ref %q", drops.refNames[0])
	}
	if got := shellQuoted("v2.1/rc-1"); got != "v2.1/rc-1" {
		t.Errorf("a plain ref quoted %q", got)
	}
}

// Past DropStatesMaxDropCount the view stops and is marked truncated.
func TestDropStatesTruncated(t *testing.T) {
	drops := &testWorkerDrops{listing: &sidecar.EvsListing{RefId: sidecar.NewId()}}
	for i := 0; i < 5; i += 1 {
		drops.drops = append(drops.drops, &sidecar.EvsDrop{Status: &protocol.EvsDropStatus{DropId: sidecar.NewId().Bytes(), State: protocol.EvsDropState_EVS_DROP_DONE}})
	}
	world := newTestDropsWorld(t, drops, func(args *WorkerArgs) {
		args.Settings.DropStatesMaxDropCount = 2
	})
	reply := testReplyOf(t, world.worker, world.dropStatesRequest("drops", ""))
	if !reply.Ok || len(reply.DropStates.Drops) != 2 || !reply.DropStates.Truncated {
		t.Fatalf("over the bound: %+v", reply.DropStates)
	}
	if _, err := NewWorker(&WorkerArgs{Sidecar: newTestWorkerSidecar(), Repo: world.worker.args.Repo, Records: world.worker.args.Records, Settings: func() *WorkerSettings {
		settings := DefaultWorkerSettings()
		settings.DropStatesMaxDropCount = 0
		return settings
	}()}); err == nil {
		t.Error("a Worker with no drop bound")
	}
}

// Without a drops seam drop_states answers unavailable; a read's refusal answers its code alone; a request without its
// repo, or with a field drop_states does not take, answers invalid and reaches no read.
func TestDropStatesRefusals(t *testing.T) {
	bare := newTestWorkerWorld(t, nil)
	if reply := testReplyOf(t, bare.worker, bare.dropStatesRequest("none", "")); reply.Ok || reply.Code != CodeUnavailable {
		t.Errorf("no seam: %+v", reply)
	}
	for _, c := range []struct {
		call string
		err  error
		code string
	}{
		{call: "List", err: sidecar.ErrEvsNoConfig, code: CodeUnavailable},
		{call: "KnownPaths", err: sidecar.ErrEvsPinFork, code: CodeUnverified},
		{call: "Drops", err: &sidecar.EvsError{Status: 403}, code: CodeRefused},
		{call: "Drops", err: errors.New("a failure whose text names notes/secret.txt"), code: CodeFailed},
	} {
		drops := &testWorkerDrops{listing: &sidecar.EvsListing{}, errs: map[string]error{c.call: c.err}}
		world := newTestDropsWorld(t, drops, nil)
		reply := testReplyOf(t, world.worker, world.dropStatesRequest("refused", ""))
		if reply.Ok || reply.Code != c.code || reply.Message != codeMessages[c.code] || strings.Contains(reply.Message, "secret") {
			t.Errorf("%s answering %v: %+v", c.call, c.err, reply)
		}
	}
	drops := &testWorkerDrops{listing: &sidecar.EvsListing{}}
	world := newTestDropsWorld(t, drops, nil)
	for _, request := range []*Request{
		{Id: "a", Operation: OperationDropStates},
		{Id: "b", Operation: OperationDropStates, RepoId: world.repoId.String(), Path: "notes/a.txt"},
		{Id: "c", Operation: OperationDropStates, RepoId: world.repoId.String(), Version: "3"},
		{Id: "d", Operation: OperationDropStates, RepoId: "not an id"},
	} {
		if reply := testReplyOf(t, world.worker, request); reply.Ok || reply.Code != CodeInvalid {
			t.Errorf("%+v: %+v", request, reply)
		}
	}
	if len(drops.calls) != 0 {
		t.Errorf("an invalid request read %q", drops.calls)
	}
}

// The console cannot drop (§18.8's pages row, D-SDK-29): drop_states is the one operation that names drops, and it only
// reads; no operation that would dry-run, sign or cancel a drop exists, each answering unknown_operation and reaching
// no seam; a drop_states request carrying a drop's preview id, typed path or now does not parse; the seam offers its
// three reads alone; and a run reaches none of the repo's acting calls, though the concrete repo has them.
func TestConsoleCannotDrop(t *testing.T) {
	for name := range workerOperations {
		if strings.Contains(name, "drop") && name != OperationDropStates {
			t.Errorf("an operation %q", name)
		}
	}
	methods := []string{}
	seam := reflect.TypeOf((*WorkerDrops)(nil)).Elem()
	for i := 0; i < seam.NumMethod(); i += 1 {
		methods = append(methods, seam.Method(i).Name)
	}
	if !slices.Equal(methods, []string{"Drops", "KnownPaths", "List"}) {
		t.Errorf("the drops seam offers %v", methods)
	}
	drops := &testWorkerDrops{
		listing: &sidecar.EvsListing{RefId: sidecar.NewId()},
		drops:   []*sidecar.EvsDrop{{Status: &protocol.EvsDropStatus{DropId: sidecar.NewId().Bytes(), State: protocol.EvsDropState_EVS_DROP_PENDING}}},
	}
	world := newTestDropsWorld(t, drops, nil)
	for _, op := range []string{"drop_preview", "drop_history", "drop_history_preview", "cancel_drop", "drop_cancel", "drop", "drops_cancel"} {
		request := world.dropStatesRequest(op, "")
		request.Operation = op
		if reply := testReplyOf(t, world.worker, request); reply.Ok || reply.Code != CodeUnknownOperation {
			t.Errorf("%s: %+v", op, reply)
		}
	}
	for _, extra := range []string{`"preview_id":"` + sidecar.NewId().String() + `"`, `"confirm_path":"notes/a.txt"`, `"now":true`, `"drop_id":"` + sidecar.NewId().String() + `"`} {
		requestJson := `{"id":"x","op":"drop_states","repo_id":"` + world.repoId.String() + `",` + extra + `}`
		reply := &Reply{}
		if err := json.Unmarshal(world.worker.HandleJson(context.Background(), []byte(requestJson)), reply); err != nil || reply.Ok || reply.Code != CodeInvalid {
			t.Errorf("drop_states with %s: %+v %v", extra, reply, err)
		}
	}
	if len(drops.calls) != 0 {
		t.Fatalf("a refused request reached %q", drops.calls)
	}
	if reply := testReplyOf(t, world.worker, world.dropStatesRequest("states", "")); !reply.Ok {
		t.Fatalf("drop_states: %+v", reply)
	}
	for _, call := range drops.calls {
		if call != "List" && call != "KnownPaths" && call != "Drops" {
			t.Errorf("drop_states reached %s", call)
		}
	}
	if world.sidecar.logouts() != 0 {
		t.Error("drop_states signed something")
	}
}
