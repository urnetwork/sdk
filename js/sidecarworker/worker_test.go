//go:build !sidecar_http

package sidecarworker

// §22.5's §18 row for the page Worker, natively on sidecartest (SDK-M4-04): no reply carries a secret, the Worker has
// no signing path but its sign-out, a sign-out revokes before it wipes and a refused one wipes nothing, and a long
// operation yields. Every clock is a test's and every wait a channel: nothing sleeps.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
)

// A sidecar whose Logout runs Logout's three steps as a log, each of which may refuse.
type testWorkerSidecar struct {
	identity *sidecar.SidecarIdentity

	stateLock sync.Mutex
	// what Logout did, in order
	steps []string
	// the error a step answers, by step
	refusals    map[string]error
	logoutCount int
}

func newTestWorkerSidecar() *testWorkerSidecar {
	return &testWorkerSidecar{
		identity: &sidecar.SidecarIdentity{NetworkId: sidecar.NewId(), SidecarId: sidecar.NewId(), ClientId: sidecar.NewId(), Generation: 1},
		refusals: map[string]error{},
	}
}

func (self *testWorkerSidecar) Identity() *sidecar.SidecarIdentity {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.identity
}

func (self *testWorkerSidecar) Status() sidecar.SidecarStatus {
	return sidecar.SidecarStatus{State: sidecar.SidecarStateOnline, Generation: 1}
}

// As Sidecar.Logout: the sign-out, then the removal, then the wipe, each only once the one before landed.
func (self *testWorkerSidecar) Logout(ctx context.Context) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.logoutCount += 1
	if self.identity == nil {
		return sidecar.ErrSidecarNoIdentity
	}
	for _, step := range []string{"revoke", "remove", "wipe"} {
		if err := self.refusals[step]; err != nil {
			return err
		}
		self.steps = append(self.steps, step)
	}
	self.identity = nil
	return nil
}

func (self *testWorkerSidecar) stepsDone() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.steps)
}

func (self *testWorkerSidecar) logouts() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.logoutCount
}

// A repo answering what a test set, its content written in chunks.
type testWorkerRepo struct {
	listing    *sidecar.EvsListing
	info       *sidecar.EvsFileInfo
	content    []byte
	chunkCount int
	history    *sidecar.EvsHistory
	err        error

	stateLock sync.Mutex
	calls     []string
}

func (self *testWorkerRepo) called(name string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls = append(self.calls, name)
}

func (self *testWorkerRepo) List(ctx context.Context, args *sidecar.EvsReadArgs) (*sidecar.EvsListing, error) {
	self.called("List")
	if self.err != nil {
		return nil, self.err
	}
	return self.listing, nil
}

func (self *testWorkerRepo) ReadHead(ctx context.Context, args *sidecar.EvsReadArgs, out io.Writer) (*sidecar.EvsFileInfo, error) {
	self.called("ReadHead")
	if self.err != nil {
		return nil, self.err
	}
	chunkCount := max(1, self.chunkCount)
	chunkByteCount := (len(self.content) + chunkCount - 1) / chunkCount
	for at := 0; at < len(self.content); at += chunkByteCount {
		if _, err := out.Write(self.content[at:min(len(self.content), at+chunkByteCount)]); err != nil {
			return nil, err
		}
	}
	return self.info, nil
}

func (self *testWorkerRepo) History(ctx context.Context, args *sidecar.EvsReadArgs) (*sidecar.EvsHistory, error) {
	self.called("History")
	if self.err != nil {
		return nil, self.err
	}
	return self.history, nil
}

// A record reader holding a test's entries.
type testWorkerRecords struct {
	entries []*sidecar.RecordReadEntry
	err     error

	stateLock sync.Mutex
	syncs     int
}

func (self *testWorkerRecords) Sync(ctx context.Context, logId sidecar.Id) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.syncs += 1
	return self.err
}

func (self *testWorkerRecords) Entries(logId sidecar.Id) []*sidecar.RecordReadEntry {
	return self.entries
}

// A Worker on the fakes, with settings adjusted by adjust.
type testWorkerWorld struct {
	sidecar   *testWorkerSidecar
	repo      *testWorkerRepo
	records   *testWorkerRecords
	worker    *Worker
	repoId    sidecar.Id
	logId     sidecar.Id
	signedOut int
}

func newTestWorkerWorld(t *testing.T, adjust func(args *WorkerArgs)) *testWorkerWorld {
	t.Helper()
	world := &testWorkerWorld{
		sidecar: newTestWorkerSidecar(),
		repo:    &testWorkerRepo{},
		records: &testWorkerRecords{},
		repoId:  sidecar.NewId(),
		logId:   sidecar.NewId(),
	}
	args := &WorkerArgs{
		Sidecar: world.sidecar,
		Repo: func(repoId sidecar.Id) (WorkerRepo, error) {
			return world.repo, nil
		},
		Records: func() (WorkerRecords, error) {
			return world.records, nil
		},
		SignedOut: func() {
			world.signedOut += 1
		},
		Settings: DefaultWorkerSettings(),
	}
	if adjust != nil {
		adjust(args)
	}
	worker, err := NewWorker(args)
	if err != nil {
		t.Fatal(err)
	}
	world.worker = worker
	return world
}

// A request of operation op with the world's ids where op reads them.
func (self *testWorkerWorld) request(id string, op string) *Request {
	request := &Request{Id: id, Operation: op}
	switch op {
	case OperationList:
		request.RepoId = self.repoId.String()
	case OperationReadView:
		request.RepoId = self.repoId.String()
		request.Path = "notes/a.txt"
	case OperationRecordView:
		request.LogId = self.logId.String()
	}
	return request
}

// A verified entry as the reader would hold it, with its salt and Signed bytes set.
func testRecordEntry(logId sidecar.Id, seq uint64, kind sidecar.RecordKind, body []byte, salt []byte, signedBytes []byte) *sidecar.RecordReadEntry {
	entryId := sidecar.NewId()
	return &sidecar.RecordReadEntry{
		LogId:   logId,
		EntryId: entryId,
		Header: &sidecar.StoredRecordHeader{Header: &protocol.RecordHeader{
			EntryId:         entryId.Bytes(),
			LogId:           logId.Bytes(),
			WriterSidecarId: sidecar.NewId().Bytes(),
			Seq:             seq,
			Kind:            uint32(kind),
			TimeMillis:      1_700_000_000_000 + int64(seq),
			Source:          uint32(sidecar.RecordSourceObserved),
		}},
		SignedBytes:    signedBytes,
		State:          sidecar.RecordEntryLive,
		Body:           body,
		Salt:           salt,
		PushTimeMillis: 1_700_000_000_500 + int64(seq),
	}
}

// Every reply, of every operation and of every refusal, holds no token, key, salt or Signed bytes the seams held,
// in any encoding: a view is built field by field, and an error's text goes nowhere. The canaries sit where a
// passed-through struct or an echoed error would carry them.
func TestNoReplyCarriesSecret(t *testing.T) {
	capture := sidecartest.NewPrivacyCapture()
	token := capture.CanaryText("by_jwt")
	clientKeySeed := capture.Canary("client_key_seed")
	salt := capture.Canary("record_salt")
	readerKey := capture.Canary("reader_key")
	world := newTestWorkerWorld(t, nil)
	world.repo.listing = &sidecar.EvsListing{RefId: sidecar.NewId(), HeadVersion: 3, Entries: []*sidecar.EvsListEntry{
		{Name: []byte("notes"), Kind: protocol.EvsEntryKind_EVS_ENTRY_DIRECTORY},
	}}
	world.repo.info = &sidecar.EvsFileInfo{RefId: sidecar.NewId(), Path: "notes/a.txt", Version: 2, Kind: protocol.EvsEntryKind_EVS_ENTRY_FILE, Size: 5}
	world.repo.content = []byte("hello")
	world.repo.history = &sidecar.EvsHistory{Versions: []*sidecar.EvsHistoryVersion{{Version: 2, State: "kept"}}}
	world.records.entries = []*sidecar.RecordReadEntry{
		testRecordEntry(world.logId, 1, sidecar.RecordKindChange, []byte("a body"), salt, append([]byte("signed:"), clientKeySeed...)),
	}

	replies := [][]byte{}
	ask := func(request *Request) {
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		replies = append(replies, world.worker.HandleJson(context.Background(), encoded))
	}
	history := world.request("history", OperationReadView)
	history.History = true
	for _, request := range []*Request{
		world.request("list", OperationList),
		world.request("read", OperationReadView),
		history,
		world.request("records", OperationRecordView),
		world.request("enroll", OperationEnroll),
		world.request("admission", OperationAdmissionStatus),
	} {
		ask(request)
	}
	// each seam's error carries a secret in its text, which no reply may repeat
	world.repo.err = fmt.Errorf("evs refused the token %s with key %x", token, readerKey)
	world.records.err = fmt.Errorf("%w: under %x", sidecar.ErrRecordStreamsUnavailable, readerKey)
	world.sidecar.refusals["remove"] = fmt.Errorf("remove-client refused %s", token)
	for _, op := range []string{OperationList, OperationReadView, OperationRecordView, OperationSignOut} {
		ask(world.request("refused-"+op, op))
	}
	replies = append(replies, world.worker.HandleJson(context.Background(), []byte(`{"id":"bad","op":"list","token":"`+token+`"}`)))

	for i, reply := range replies {
		capture.Add(sidecartest.PrivacyClass("CC4/page"), fmt.Sprintf("reply %d", i), reply)
	}
	if count := capture.ItemCount("CC4"); count != len(replies) {
		t.Fatalf("CONTROL FAILED: the scan holds %d replies of %d", count, len(replies))
	}
	for _, finding := range capture.Scan() {
		t.Errorf("%s", finding)
	}
	// the control: a reply that did carry the token is found
	control := sidecartest.NewPrivacyCapture()
	control.AddSecret("by_jwt", []byte(token))
	control.Add("CC4/page", "planted", []byte(`{"id":"x","message":"`+token+`"}`))
	if len(control.Scan()) == 0 {
		t.Fatal("CONTROL FAILED: a planted token was not found")
	}
}

// A page Worker is read_only (§7.9): it offers exactly §18.8's M4 operations, no approval, request, decision or
// revocation, the seams it holds have no signing call, and of its operations only sign_out reaches the one call that
// signs, Logout's own SidecarRevoke.
func TestReadOnlyOperationsOnly(t *testing.T) {
	operations := []string{}
	for name := range workerOperations {
		operations = append(operations, name)
	}
	slices.Sort(operations)
	want := []string{OperationAdmissionStatus, OperationEnroll, OperationList, OperationReadView, OperationRecordView, OperationSignOut}
	if !slices.Equal(operations, want) {
		t.Fatalf("the operations %v, want %v", operations, want)
	}
	methodsOf := func(value any) []string {
		kind := reflect.TypeOf(value).Elem()
		names := []string{}
		for i := 0; i < kind.NumMethod(); i++ {
			names = append(names, kind.Method(i).Name)
		}
		return names
	}
	for _, c := range []struct {
		seam    any
		methods []string
	}{
		{seam: (*WorkerSidecar)(nil), methods: []string{"Identity", "Logout", "Status"}},
		{seam: (*WorkerRepo)(nil), methods: []string{"History", "List", "ReadHead"}},
		{seam: (*WorkerRecords)(nil), methods: []string{"Entries", "Sync"}},
	} {
		if methods := methodsOf(c.seam); !slices.Equal(methods, c.methods) {
			t.Errorf("a seam offers %v, want %v", methods, c.methods)
		}
	}
	world := newTestWorkerWorld(t, nil)
	world.repo.listing = &sidecar.EvsListing{}
	world.repo.info = &sidecar.EvsFileInfo{}
	world.repo.history = &sidecar.EvsHistory{}
	for _, op := range operations {
		if op == OperationSignOut {
			continue
		}
		world.worker.Handle(context.Background(), world.request(op, op))
	}
	if world.sidecar.logouts() != 0 || world.signedOut != 0 {
		t.Fatalf("a read reached Logout %d times", world.sidecar.logouts())
	}
	if reply := world.worker.Handle(context.Background(), world.request("out", OperationSignOut)); !reply.Ok || world.sidecar.logouts() != 1 {
		t.Fatalf("sign_out: %+v, %d logouts", reply, world.sidecar.logouts())
	}
}

// sign_out signs SidecarRevoke{removed}, removes the client and wipes, in that order, and only then is the Worker
// done: every later request answers signed_out and reaches no seam.
func TestSignOutRevokesThenWipes(t *testing.T) {
	world := newTestWorkerWorld(t, nil)
	sidecarId := world.sidecar.Identity().SidecarId
	reply := world.worker.Handle(context.Background(), world.request("out", OperationSignOut))
	if !reply.Ok || reply.Id != "out" || reply.SignOut == nil || reply.SignOut.SidecarId != sidecarId.String() {
		t.Fatalf("the reply: %+v", reply)
	}
	if steps := world.sidecar.stepsDone(); !slices.Equal(steps, []string{"revoke", "remove", "wipe"}) || world.signedOut != 1 {
		t.Fatalf("the steps %v, signed out %d", steps, world.signedOut)
	}
	for _, op := range []string{OperationList, OperationReadView, OperationRecordView, OperationSignOut} {
		if reply := world.worker.Handle(context.Background(), world.request("after", op)); reply.Ok || reply.Code != CodeSignedOut {
			t.Errorf("%s after the sign-out: %+v", op, reply)
		}
	}
	if len(world.repo.calls) != 0 || world.records.syncs != 0 || world.sidecar.logouts() != 1 {
		t.Fatalf("a request after the sign-out reached a seam: %v, %d syncs, %d logouts", world.repo.calls, world.records.syncs, world.sidecar.logouts())
	}
}

// A sign-out that cannot land, its statement pending or its removal refused, wipes nothing: the Worker keeps serving,
// and the same request again signs out once the network takes it. With nothing enrolled a sign-out answers ok, so a
// relayed one repeated after a hand-over answers as the first.
func TestSignOutThatCannotLandWipesNothing(t *testing.T) {
	world := newTestWorkerWorld(t, nil)
	world.repo.listing = &sidecar.EvsListing{}
	world.sidecar.refusals["revoke"] = sidecar.ErrSidecarLogoutPending
	reply := world.worker.Handle(context.Background(), world.request("out", OperationSignOut))
	if reply.Ok || reply.Code != CodeFailed || world.signedOut != 0 || len(world.sidecar.stepsDone()) != 0 {
		t.Fatalf("a pending sign-out: %+v, steps %v", reply, world.sidecar.stepsDone())
	}
	if reply := world.worker.Handle(context.Background(), world.request("list", OperationList)); !reply.Ok {
		t.Fatalf("the Worker stopped serving after a refused sign-out: %+v", reply)
	}
	delete(world.sidecar.refusals, "revoke")
	if reply := world.worker.Handle(context.Background(), world.request("out", OperationSignOut)); !reply.Ok || world.signedOut != 1 {
		t.Fatalf("the retry: %+v", reply)
	}

	unenrolled := newTestWorkerWorld(t, nil)
	unenrolled.sidecar.identity = nil
	if reply := unenrolled.worker.Handle(context.Background(), unenrolled.request("out", OperationSignOut)); !reply.Ok || reply.SignOut.SidecarId != "" {
		t.Fatalf("a sign-out with nothing enrolled: %+v", reply)
	}
}

// A clock that moves stepDuration at each read, and the times each yield came at.
type testWorkerClock struct {
	stateLock    sync.Mutex
	now          time.Time
	stepDuration time.Duration
	yieldTimes   []time.Time
}

func (self *testWorkerClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(self.stepDuration)
	return self.now
}

func (self *testWorkerClock) yield(ctx context.Context) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.yieldTimes = append(self.yieldTimes, self.now)
	return ctx.Err()
}

// An operation over 50 ms yields between its steps, a record page and a read alike, so the page Worker's long-poll and
// other requests interleave with it (§18.10's threads row); a short one does not yield at all.
func TestLongOperationYields(t *testing.T) {
	clock := &testWorkerClock{now: time.Unix(1_700_000_000, 0), stepDuration: 7 * time.Millisecond}
	world := newTestWorkerWorld(t, func(args *WorkerArgs) {
		args.Now = clock.Now
		args.Yield = clock.yield
	})
	for i := 0; i < 300; i++ {
		world.records.entries = append(world.records.entries, testRecordEntry(world.logId, uint64(i+1), sidecar.RecordKindNote, []byte("body"), nil, nil))
	}
	start := clock.Now()
	if reply := world.worker.Handle(context.Background(), world.request("records", OperationRecordView)); !reply.Ok || len(reply.Record.Entries) != 300 {
		t.Fatalf("the record view: %+v", reply)
	}
	end := clock.Now()
	interval := DefaultWorkerSettings().YieldInterval
	// between two yields at most the interval and the two clock reads a step makes
	last := start
	for _, at := range append(slices.Clone(clock.yieldTimes), end) {
		if gap := at.Sub(last); interval+2*clock.stepDuration < gap {
			t.Fatalf("%s passed without a yield", gap)
		}
		last = at
	}
	if len(clock.yieldTimes) < int(end.Sub(start)/(interval+2*clock.stepDuration)) {
		t.Fatalf("%d yields over %s", len(clock.yieldTimes), end.Sub(start))
	}

	// a read yields between the chunks it writes
	clock.yieldTimes = nil
	world.repo.info = &sidecar.EvsFileInfo{Path: "big.bin", Size: 64 * 1024}
	world.repo.content = make([]byte, 64*1024)
	world.repo.chunkCount = 64
	if reply := world.worker.Handle(context.Background(), world.request("read", OperationReadView)); !reply.Ok {
		t.Fatalf("the read: %+v", reply)
	}
	if len(clock.yieldTimes) < 4 {
		t.Fatalf("a 64-chunk read yielded %d times", len(clock.yieldTimes))
	}

	// a short operation yields nothing
	clock.yieldTimes = nil
	world.records.entries = world.records.entries[:2]
	world.worker.Handle(context.Background(), world.request("short", OperationRecordView))
	if len(clock.yieldTimes) != 0 {
		t.Fatalf("a short operation yielded %d times", len(clock.yieldTimes))
	}
}

// A yield that ends with the operation's ctx ends the operation, which answers cancelled.
func TestCancelledOperationStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	yields := 0
	world := newTestWorkerWorld(t, func(args *WorkerArgs) {
		args.Settings.YieldInterval = 0
		args.Yield = func(yieldCtx context.Context) error {
			yields += 1
			if yields == 3 {
				cancel()
			}
			return yieldCtx.Err()
		}
	})
	for i := 0; i < 10; i++ {
		world.records.entries = append(world.records.entries, testRecordEntry(world.logId, uint64(i+1), sidecar.RecordKindNote, nil, nil, nil))
	}
	reply := world.worker.Handle(ctx, world.request("records", OperationRecordView))
	if reply.Ok || reply.Code != CodeCancelled || yields != 3 {
		t.Fatalf("a cancelled operation: %+v after %d yields", reply, yields)
	}
}

// Every request is checked whole before anything is read: a malformed one, or one with a field its operation does not
// take, answers invalid and reaches no seam, under its id where one was readable.
func TestMalformedRequestsReachNoSeam(t *testing.T) {
	world := newTestWorkerWorld(t, nil)
	repoId := world.repoId.String()
	for _, c := range []struct {
		json string
		id   string
		code string
	}{
		{json: `not json`, id: "", code: CodeInvalid},
		{json: `{"id":"a","op":"list","repo_id":"` + repoId + `","token":"x"}`, id: "a", code: CodeInvalid},
		{json: `{"id":"a","op":"list","repo_id":"` + repoId + `"} {"id":"b"}`, id: "", code: CodeInvalid},
		{json: `{"id":"","op":"list","repo_id":"` + repoId + `"}`, id: "", code: CodeInvalid},
		{json: `{"id":"a b","op":"list","repo_id":"` + repoId + `"}`, id: "", code: CodeInvalid},
		{json: `{"id":"` + strings.Repeat("a", 65) + `","op":"list","repo_id":"` + repoId + `"}`, id: "", code: CodeInvalid},
		{json: `{"id":"a","op":"decide"}`, id: "a", code: CodeUnknownOperation},
		{json: `{"id":"a","op":"list"}`, id: "a", code: CodeInvalid},
		{json: `{"id":"a","op":"list","repo_id":"not-an-id"}`, id: "a", code: CodeInvalid},
		{json: `{"id":"a","op":"list","repo_id":"` + repoId + `","log_id":"` + repoId + `"}`, id: "a", code: CodeInvalid},
		{json: `{"id":"a","op":"read_view","repo_id":"` + repoId + `"}`, id: "a", code: CodeInvalid},
		{json: `{"id":"a","op":"read_view","repo_id":"` + repoId + `","path":"a","path_base64":"YQ=="}`, id: "a", code: CodeInvalid},
		{json: `{"id":"a","op":"read_view","repo_id":"` + repoId + `","path":"a","version":"-1"}`, id: "a", code: CodeInvalid},
		{json: `{"id":"a","op":"read_view","repo_id":"` + repoId + `","path_base64":"not base64!"}`, id: "a", code: CodeInvalid},
		{json: `{"id":"a","op":"read_view","repo_id":"` + repoId + `","path":"` + strings.Repeat("p", 4097) + `"}`, id: "a", code: CodeInvalid},
		{json: `{"id":"a","op":"record_view","log_id":"` + repoId + `","after":"x"}`, id: "a", code: CodeInvalid},
		{json: `{"id":"a","op":"sign_out","repo_id":"` + repoId + `"}`, id: "a", code: CodeInvalid},
		{json: `{"id":"a","op":"enroll","code":"has space"}`, id: "a", code: CodeInvalid},
		{json: `{"id":"a","op":"admission_status","history":true}`, id: "a", code: CodeInvalid},
		{json: `{"id":"a","op":"list","repo_id":"` + repoId + `","path":"` + strings.Repeat("p", 64*1024) + `"}`, id: "", code: CodeTooLarge},
	} {
		reply := &Reply{}
		if err := json.Unmarshal(world.worker.HandleJson(context.Background(), []byte(c.json)), reply); err != nil {
			t.Fatal(err)
		}
		if reply.Ok || reply.Id != c.id || reply.Code != c.code || reply.Message != codeMessages[c.code] {
			t.Errorf("%.80s: %+v", c.json, reply)
		}
	}
	if len(world.repo.calls) != 0 || world.records.syncs != 0 || world.sidecar.logouts() != 0 {
		t.Fatalf("a malformed request reached a seam: %v", world.repo.calls)
	}
}

// A Worker needs its seams and positive bounds.
func TestNewWorkerRefusesMissingSeams(t *testing.T) {
	if _, err := NewWorker(&WorkerArgs{}); err == nil {
		t.Fatal("a Worker with no seams")
	}
	settings := DefaultWorkerSettings()
	settings.ReadMaxByteCount = 0
	_, err := NewWorker(&WorkerArgs{
		Sidecar:  newTestWorkerSidecar(),
		Repo:     func(sidecar.Id) (WorkerRepo, error) { return nil, errors.New("none") },
		Records:  func() (WorkerRecords, error) { return nil, errors.New("none") },
		Settings: settings,
	})
	if err == nil {
		t.Fatal("a Worker with no read bound")
	}
}
