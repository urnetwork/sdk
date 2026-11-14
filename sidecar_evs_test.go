//go:build !ios_extension && !js

package sdk

// SidecarEvs (SDK-M8-03): its calls over a fake of the core's evs client, one repo in memory, so each mapping, bound
// and refusal is shown without a network: a read and write round trip, a 1 MiB + 1 write refused before the core sees
// it, EVS_GIT_REF and evs's own refusals surfaced with their codes, a change listener hearing each commit once, a gap
// for what a full queue missed, Erasures' fields, and one terminal answer for a success, a refusal, a cancelled call
// and a panic. GetEvs is shown over the core's real EvsClient, through a test's factory, and the registered factory's
// refusal while no host composes one. Every repo and path is synthetic.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// One version of a path in the fake.
type testEvsFile struct {
	data    []byte
	version uint64
	commit  uint64
}

// A fake of the core's evs client: repos of paths in memory, each Write a commit raising one change to the listeners,
// and a hook a test sets to answer or hold any call.
type testSidecarEvsBackend struct {
	stateLock sync.Mutex
	// each repo's paths, each path's versions oldest first
	repoIdPaths map[sidecar.Id]map[string][]*testEvsFile
	repoIdHeads map[sidecar.Id]uint64
	erasures    []*sidecar.SidecarErasure
	created     []*sidecar.EvsCreateRepoArgs
	writeCount  int
	leaseCount  map[sidecar.Id]int
	leaseEnded  map[sidecar.Id]int
	listenerSeq int
	listeners   map[int]*testEvsListener
	// answers a call by name before the fake does, or nil to let it
	hook func(ctx context.Context, call string) error
}

type testEvsListener struct {
	repoId   sidecar.Id
	listener sidecar.EvsRepoChangeFunction
}

func newTestSidecarEvsBackend() *testSidecarEvsBackend {
	return &testSidecarEvsBackend{
		repoIdPaths: map[sidecar.Id]map[string][]*testEvsFile{},
		repoIdHeads: map[sidecar.Id]uint64{},
		leaseCount:  map[sidecar.Id]int{},
		leaseEnded:  map[sidecar.Id]int{},
		listeners:   map[int]*testEvsListener{},
	}
}

func (self *testSidecarEvsBackend) setHook(hook func(ctx context.Context, call string) error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.hook = hook
}

func (self *testSidecarEvsBackend) runHook(ctx context.Context, call string) error {
	self.stateLock.Lock()
	hook := self.hook
	self.stateLock.Unlock()
	if hook == nil {
		return nil
	}
	return hook(ctx, call)
}

func (self *testSidecarEvsBackend) CreateRepo(ctx context.Context, args *sidecar.EvsCreateRepoArgs) (sidecar.Id, error) {
	if err := self.runHook(ctx, "create"); err != nil {
		return sidecar.Id{}, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	copied := *args
	self.created = append(self.created, &copied)
	repoId := sidecar.NewEvsRepoId()
	self.repoIdPaths[repoId] = map[string][]*testEvsFile{}
	return repoId, nil
}

func (self *testSidecarEvsBackend) ReadHead(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsReadArgs, out io.Writer) (*sidecar.EvsFileInfo, error) {
	if err := self.runHook(ctx, "read"); err != nil {
		return nil, err
	}
	file, head := func() (*testEvsFile, uint64) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		versions := self.repoIdPaths[repoId][args.Path]
		if len(versions) == 0 {
			return nil, 0
		}
		return versions[len(versions)-1], self.repoIdHeads[repoId]
	}()
	if file == nil {
		return nil, sidecar.ErrEvsPathAbsent
	}
	// in pieces, as the core's read writes its chunks
	for offset := 0; offset < len(file.data); offset += 1024 {
		if _, err := out.Write(file.data[offset:min(len(file.data), offset+1024)]); err != nil {
			return nil, err
		}
	}
	return &sidecar.EvsFileInfo{
		Path: args.Path, Version: file.version, CommitVersion: file.commit, SignerSidecarId: testEvsSigner,
		Time: time.UnixMilli(1_800_000_000_000 + int64(file.commit)), Size: uint64(len(file.data)), HeadVersion: head,
	}, nil
}

func (self *testSidecarEvsBackend) History(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsReadArgs) (*sidecar.EvsHistory, error) {
	if err := self.runHook(ctx, "history"); err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	versions := self.repoIdPaths[repoId][args.Path]
	if len(versions) == 0 {
		return nil, sidecar.ErrEvsPathAbsent
	}
	history := &sidecar.EvsHistory{Path: args.Path, HeadVersion: self.repoIdHeads[repoId]}
	for i := len(versions) - 1; 0 <= i; i -= 1 {
		history.Versions = append(history.Versions, &sidecar.EvsHistoryVersion{
			Version: versions[i].version, CommitVersion: versions[i].commit, SignerSidecarId: testEvsSigner,
			Time: time.UnixMilli(1_800_000_000_000 + int64(versions[i].commit)), State: "kept",
		})
	}
	return history, nil
}

func (self *testSidecarEvsBackend) Write(ctx context.Context, repoId sidecar.Id, args *sidecar.EvsWriteArgs) (*sidecar.EvsWriteResult, error) {
	if err := self.runHook(ctx, "write"); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(args.Content)
	if err != nil || int64(len(data)) != args.Size {
		return nil, fmt.Errorf("%w: content of %d bytes", sidecar.ErrEvsWriteInvalid, len(data))
	}
	change, listeners, result := func() (*sidecar.EvsRepoChange, []sidecar.EvsRepoChangeFunction, *sidecar.EvsWriteResult) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.writeCount += 1
		paths := self.repoIdPaths[repoId]
		if paths == nil {
			paths = map[string][]*testEvsFile{}
			self.repoIdPaths[repoId] = paths
		}
		self.repoIdHeads[repoId] += 1
		file := &testEvsFile{data: data, version: uint64(len(paths[args.Path]) + 1), commit: self.repoIdHeads[repoId]}
		paths[args.Path] = append(paths[args.Path], file)
		change := &sidecar.EvsRepoChange{
			RepoId: repoId, RefId: testEvsMainRef, Version: file.commit, PushId: sidecar.NewId(), SignerSidecarId: testEvsSigner,
			WholeRef: true,
		}
		listeners := []sidecar.EvsRepoChangeFunction{}
		for _, listenerId := range slices.Sorted(func(yield func(int) bool) {
			for listenerId := range self.listeners {
				if !yield(listenerId) {
					return
				}
			}
		}) {
			if self.listeners[listenerId].repoId == repoId {
				listeners = append(listeners, self.listeners[listenerId].listener)
			}
		}
		return change, listeners, &sidecar.EvsWriteResult{PushId: change.PushId, Version: file.commit, PathVersion: file.version}
	}()
	// the core's subscriber raises a change per verified commit, outside its lock
	for _, listener := range listeners {
		listener(change)
	}
	return result, nil
}

func (self *testSidecarEvsBackend) Erasures(ctx context.Context, repoId sidecar.Id) ([]*sidecar.SidecarErasure, error) {
	if err := self.runHook(ctx, "erasures"); err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.erasures), nil
}

func (self *testSidecarEvsBackend) Subscribe(ctx context.Context, repoId sidecar.Id) (func(), error) {
	if err := self.runHook(ctx, "subscribe"); err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.leaseCount[repoId] += 1
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.leaseEnded[repoId] += 1
	}, nil
}

func (self *testSidecarEvsBackend) AddRepoChangeListener(repoId sidecar.Id, listener sidecar.EvsRepoChangeFunction) func() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.listenerSeq += 1
	listenerId := self.listenerSeq
	self.listeners[listenerId] = &testEvsListener{repoId: repoId, listener: listener}
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.listeners, listenerId)
	}
}

// The fake's signer and main ref.
var (
	testEvsSigner  = sidecar.Id{0x51}
	testEvsMainRef = sidecar.Id{0x52}
)

// A callback that counts its answers and hands the first on.
type testEvsCallback[T any] struct {
	stateLock sync.Mutex
	count     int
	answers   chan testEvsAnswer[T]
}

type testEvsAnswer[T any] struct {
	result T
	err    error
}

func newTestEvsCallback[T any]() *testEvsCallback[T] {
	return &testEvsCallback[T]{answers: make(chan testEvsAnswer[T], 16)}
}

func (self *testEvsCallback[T]) Result(result T, err error) {
	self.stateLock.Lock()
	self.count += 1
	self.stateLock.Unlock()
	self.answers <- testEvsAnswer[T]{result: result, err: err}
}

// The first answer, and that it was the only one once a further wait finds none.
func (self *testEvsCallback[T]) await(t *testing.T) (T, error) {
	t.Helper()
	select {
	case answer := <-self.answers:
		return answer.result, answer.err
	case <-time.After(testSidecarTimeout):
		t.Fatal("no answer")
	}
	var zero T
	return zero, nil
}

// The answers so far.
func (self *testEvsCallback[T]) answerCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.count
}

// A SidecarEvs on backend with the defaults, closed at the test's end.
func newTestSidecarEvs(t *testing.T, backend sidecarEvsBackend, settings *sidecarEvsSettings) *SidecarEvs {
	t.Helper()
	if settings == nil {
		settings = defaultSidecarEvsSettings()
	}
	evs := newSidecarEvs(context.Background(), backend, settings)
	t.Cleanup(evs.close)
	return evs
}

// A synthetic repo id.
func testEvsRepoId(b byte) *Id {
	return newId([16]byte{0xe0, b})
}

// Writes data to path and answers the result.
func testEvsWrite(t *testing.T, evs *SidecarEvs, repoId *Id, path string, data []byte, versioning string) *SidecarResult {
	t.Helper()
	callback := newTestEvsCallback[*SidecarResult]()
	evs.Write(repoId, path, data, versioning, callback)
	result, err := callback.await(t)
	if err != nil || result == nil {
		t.Fatalf("write: %v, %+v", err, result)
	}
	return result
}

// Reads path.
func testEvsRead(t *testing.T, evs *SidecarEvs, repoId *Id, path string) *SidecarBytes {
	t.Helper()
	callback := newTestEvsCallback[*SidecarBytes]()
	evs.ReadHead(repoId, path, callback)
	result, err := callback.await(t)
	if err != nil || result == nil {
		t.Fatalf("read: %v, %+v", err, result)
	}
	return result
}

// A write reads back as written, at the version the write answered, and History lists both versions newest first,
// each kept, versioning on.
func TestSidecarEvsReadWriteRoundTrip(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newTestSidecarEvs(t, backend, nil)
	repoId := testEvsRepoId(1)
	first := []byte("synthetic first content")
	if result := testEvsWrite(t, evs, repoId, "notes/a.txt", first, ""); !result.Ok || result.Value != "1" || result.Code != "" {
		t.Fatalf("first write %+v", result)
	}
	read := testEvsRead(t, evs, repoId, "notes/a.txt")
	if !read.Ok || !bytes.Equal(read.Data, first) || read.Version != 1 || read.CommitVersion != 1 ||
		read.SignerSidecarId.String() != newId([16]byte(testEvsSigner)).String() || read.TimeMillis != 1_800_000_000_001 {
		t.Fatalf("first read %+v", read)
	}
	second := bytes.Repeat([]byte("second "), 2000)
	if result := testEvsWrite(t, evs, repoId, "notes/a.txt", second, ""); !result.Ok || result.Value != "2" {
		t.Fatalf("second write %+v", result)
	}
	if read := testEvsRead(t, evs, repoId, "notes/a.txt"); !read.Ok || !bytes.Equal(read.Data, second) || read.Version != 2 {
		t.Fatalf("second read: version %d, %d bytes", read.Version, len(read.Data))
	}
	callback := newTestEvsCallback[*SidecarHistory]()
	evs.History(repoId, "notes/a.txt", callback)
	history, err := callback.await(t)
	if err != nil || !history.Ok || history.Versions.Len() != 2 || history.HeadVersion != 2 {
		t.Fatalf("history %v %+v", err, history)
	}
	for i, want := range []int64{2, 1} {
		version := history.Versions.Get(i)
		if version.Version != want || version.State != "kept" || version.Versioning != "on" || version.Removed {
			t.Errorf("history[%d] %+v", i, version)
		}
	}
	// a path the head does not hold
	if read := testEvsRead(t, evs, repoId, "notes/absent.txt"); read.Ok || read.Code != sidecarEvsCodeNotFound {
		t.Errorf("an absent path %+v", read)
	}
}

// §18.5's bound: 1 MiB writes, and 1 MiB + 1 is refused before anything reaches the core.
func TestSidecarEvsWriteOverOneMebibyteRefused(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newTestSidecarEvs(t, backend, nil)
	repoId := testEvsRepoId(2)
	if result := testEvsWrite(t, evs, repoId, "big.bin", make([]byte, 1024*1024), ""); !result.Ok {
		t.Fatalf("1 MiB: %+v", result)
	}
	result := testEvsWrite(t, evs, repoId, "big.bin", make([]byte, 1024*1024+1), "")
	if result.Ok || result.Code != sidecarEvsCodeTooLarge {
		t.Fatalf("1 MiB + 1: %+v", result)
	}
	backend.stateLock.Lock()
	defer backend.stateLock.Unlock()
	if backend.writeCount != 1 {
		t.Errorf("the core saw %d writes", backend.writeCount)
	}
}

// A read past ReadMaxByteCount answers EVS_TOO_LARGE, its bytes read no further than the bound.
func TestSidecarEvsReadPastItsBoundRefused(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	settings := defaultSidecarEvsSettings()
	settings.ReadMaxByteCount = 4096
	evs := newTestSidecarEvs(t, backend, settings)
	repoId := testEvsRepoId(3)
	testEvsWrite(t, evs, repoId, "at-bound.bin", make([]byte, 4096), "")
	testEvsWrite(t, evs, repoId, "past-bound.bin", make([]byte, 4097), "")
	if read := testEvsRead(t, evs, repoId, "at-bound.bin"); !read.Ok || len(read.Data) != 4096 {
		t.Errorf("at the bound: ok %v, %d bytes", read.Ok, len(read.Data))
	}
	if read := testEvsRead(t, evs, repoId, "past-bound.bin"); read.Ok || read.Code != sidecarEvsCodeTooLarge || read.Data != nil {
		t.Errorf("past the bound %+v", read)
	}
}

// The core's EVS_GIT_REF reaches the app with its code, and so does each of evs's refusals by its name; a versioning
// argument is refused until format 2, before the core sees it.
func TestSidecarEvsGitRefAndRefusalsSurfaced(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newTestSidecarEvs(t, backend, nil)
	repoId := testEvsRepoId(4)
	cases := []struct {
		err  error
		code string
	}{
		{err: fmt.Errorf("plan: %w", sidecar.ErrEvsGitRef), code: "EVS_GIT_REF"},
		{err: &sidecar.EvsError{Status: http.StatusConflict, Code: protocol.EvsRefusalCode_EVS_REFUSAL_PATH_CONFLICT}, code: "EVS_PATH_CONFLICT"},
		{err: &sidecar.EvsPathConflictError{Refusal: &sidecar.EvsError{Status: http.StatusConflict, Code: protocol.EvsRefusalCode_EVS_REFUSAL_PATH_CONFLICT}, Paths: []string{"versionless.txt"}}, code: "EVS_PATH_CONFLICT"},
		{err: &sidecar.EvsError{Status: http.StatusForbidden, Code: protocol.EvsRefusalCode_EVS_REFUSAL_SCOPE}, code: "EVS_SCOPE"},
		{err: &sidecar.EvsError{Status: http.StatusPaymentRequired, Code: protocol.EvsRefusalCode_EVS_REFUSAL_QUOTA_LIMIT}, code: "EVS_QUOTA_LIMIT"},
		{err: sidecar.ErrEvsFormatUnsupported, code: "EVS_FORMAT_UNSUPPORTED"},
		{err: fmt.Errorf("%w: a path of no names", sidecar.ErrEvsWriteInvalid), code: "EVS_INVALID"},
		{err: context.Canceled, code: "EVS_UNAVAILABLE"},
		{err: errors.New("a failure with no code"), code: ""},
	}
	for _, c := range cases {
		backend.setHook(func(ctx context.Context, call string) error {
			return c.err
		})
		result := testEvsWrite(t, evs, repoId, "file.txt", []byte("x"), "")
		if result.Ok || result.Code != c.code || result.Message == "" {
			t.Errorf("%v: %+v", c.err, result)
		}
	}
	backend.setHook(nil)
	for _, versioning := range []string{"on", "off", "sometimes"} {
		if result := testEvsWrite(t, evs, repoId, "file.txt", []byte("x"), versioning); result.Ok || result.Code != sidecarEvsCodeVersioning {
			t.Errorf("versioning %q: %+v", versioning, result)
		}
	}
	backend.stateLock.Lock()
	defer backend.stateLock.Unlock()
	if backend.writeCount != 0 {
		t.Errorf("the core saw %d writes", backend.writeCount)
	}
}

// CreateRepo makes a general or record repo with its private flag and answers its id; another kind is refused before
// the core sees it.
func TestSidecarEvsCreateRepo(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newTestSidecarEvs(t, backend, nil)
	for _, c := range []struct {
		kind    string
		private bool
		want    protocol.RepoKind
	}{
		{kind: "general", private: false, want: protocol.RepoKind_REPO_KIND_GENERAL},
		{kind: "record", private: true, want: protocol.RepoKind_REPO_KIND_RECORD},
	} {
		callback := newTestEvsCallback[*SidecarResult]()
		evs.CreateRepo(c.kind, "synthetic repo", c.private, callback)
		result, err := callback.await(t)
		if err != nil || !result.Ok {
			t.Fatalf("%s: %v %+v", c.kind, err, result)
		}
		if _, err := ParseId(result.Value); err != nil {
			t.Errorf("%s: value %q", c.kind, result.Value)
		}
		backend.stateLock.Lock()
		created := backend.created[len(backend.created)-1]
		backend.stateLock.Unlock()
		if created.Kind != c.want || created.Private != c.private || created.DisplayName != "synthetic repo" || created.OwnerMemberId != nil {
			t.Errorf("%s: created %+v", c.kind, created)
		}
	}
	for _, kind := range []string{"vault", "config", "mailbox", ""} {
		callback := newTestEvsCallback[*SidecarResult]()
		evs.CreateRepo(kind, "synthetic repo", false, callback)
		if result, _ := callback.await(t); result.Ok || result.Code != sidecarEvsCodeInvalid {
			t.Errorf("%q: %+v", kind, result)
		}
	}
	backend.stateLock.Lock()
	defer backend.stateLock.Unlock()
	if len(backend.created) != 2 {
		t.Errorf("the core made %d repos", len(backend.created))
	}
}

// A listener the core's changes reach.
type testRepoChangeListener struct {
	changes chan *SidecarRepoChange
	// held until a test lets the first call return
	hold chan struct{}
}

func (self *testRepoChangeListener) RepoChanged(change *SidecarRepoChange) {
	self.changes <- change
	if self.hold != nil {
		<-self.hold
	}
}

// The next change, or a failure.
func (self *testRepoChangeListener) next(t *testing.T) *SidecarRepoChange {
	t.Helper()
	select {
	case change := <-self.changes:
		return change
	case <-time.After(testSidecarTimeout):
		t.Fatal("no change")
		return nil
	}
}

// Nothing more arrives once the queue's work is done: a write the test makes after it, read through to the change it
// raises, is the next thing heard.
func (self *testRepoChangeListener) requireNoneBefore(t *testing.T, evs *SidecarEvs, repoId *Id, wantVersion int64) {
	t.Helper()
	testEvsWrite(t, evs, repoId, "marker.txt", []byte("marker"), "")
	if change := self.next(t); change.Version != wantVersion {
		t.Fatalf("heard version %d before the marker's %d", change.Version, wantVersion)
	}
}

// Each commit reaches a listener of its repo once, in order, with its ref, version, push and signer; another repo's
// does not; the repo is leased once however many listeners it has, and Close ends both listeners and the lease.
func TestSidecarEvsChangeListenerFiresOncePerCommit(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newSidecarEvs(context.Background(), backend, defaultSidecarEvsSettings())
	settled := make(chan bool, 4)
	evs.leaseSettled = func(repoId sidecar.Id, kept bool) {
		settled <- kept
	}
	repoId := testEvsRepoId(5)
	other := testEvsRepoId(6)
	listener := &testRepoChangeListener{changes: make(chan *SidecarRepoChange, 16)}
	second := &testRepoChangeListener{changes: make(chan *SidecarRepoChange, 16)}
	sub := evs.AddRepoChangeListener(repoId, listener)
	select {
	case kept := <-settled:
		if !kept {
			t.Fatal("the repo's lease was not kept")
		}
	case <-time.After(testSidecarTimeout):
		t.Fatal("the repo was never leased")
	}
	// a second listener of a leased repo takes no second lease
	evs.AddRepoChangeListener(repoId, second)
	for i := 1; i <= 3; i += 1 {
		testEvsWrite(t, evs, repoId, fmt.Sprintf("file-%d.txt", i), []byte("x"), "")
	}
	testEvsWrite(t, evs, other, "elsewhere.txt", []byte("x"), "")
	for _, heard := range []*testRepoChangeListener{listener, second} {
		for want := int64(1); want <= 3; want += 1 {
			change := heard.next(t)
			if change.Version != want || change.RepoId.String() != repoId.String() || change.RefId == nil || change.PushId == nil ||
				change.SignerSidecarId.String() != newId([16]byte(testEvsSigner)).String() || change.Gap {
				t.Fatalf("change %d: %+v", want, change)
			}
		}
	}
	listener.requireNoneBefore(t, evs, repoId, 4)
	second.next(t)
	// a closed sub leaves the core's listeners at once and hears nothing more, while the other listener does
	sub.Close()
	backend.stateLock.Lock()
	listenerCount := len(backend.listeners)
	backend.stateLock.Unlock()
	if listenerCount != 1 {
		t.Fatalf("%d listeners after a Close", listenerCount)
	}
	testEvsWrite(t, evs, repoId, "after.txt", []byte("x"), "")
	if change := second.next(t); change.Version != 5 {
		t.Fatalf("the other listener heard %+v", change)
	}
	select {
	case change := <-listener.changes:
		t.Fatalf("a closed sub heard %+v", change)
	default:
	}
	evs.close()
	backend.stateLock.Lock()
	defer backend.stateLock.Unlock()
	if backend.leaseCount[sidecar.Id(repoId.id)] != 1 || backend.leaseEnded[sidecar.Id(repoId.id)] != 1 || backend.leaseCount[sidecar.Id(other.id)] != 0 {
		t.Errorf("leases %v, ended %v", backend.leaseCount, backend.leaseEnded)
	}
}

// A listener that falls a full queue behind hears what was queued and then one gap for its repo, never a change twice.
func TestSidecarEvsListenerHearsAGapForWhatAFullQueueMissed(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	settings := defaultSidecarEvsSettings()
	settings.ChangeQueueCount = 1
	evs := newTestSidecarEvs(t, backend, settings)
	repoId := testEvsRepoId(7)
	listener := &testRepoChangeListener{changes: make(chan *SidecarRepoChange, 16), hold: make(chan struct{})}
	evs.AddRepoChangeListener(repoId, listener)
	// the first change holds the listener, the second fills the queue, and the third and fourth are missed
	testEvsWrite(t, evs, repoId, "a.txt", []byte("x"), "")
	if change := listener.next(t); change.Version != 1 {
		t.Fatalf("first %+v", change)
	}
	for i := 2; i <= 4; i += 1 {
		testEvsWrite(t, evs, repoId, "a.txt", []byte("x"), "")
	}
	close(listener.hold)
	if change := listener.next(t); change.Version != 2 || change.Gap {
		t.Fatalf("queued %+v", change)
	}
	if change := listener.next(t); !change.Gap || change.RepoId.String() != repoId.String() || change.RefId != nil {
		t.Fatalf("gap %+v", change)
	}
	listener.requireNoneBefore(t, evs, repoId, 5)
}

// Erasures maps each of the core's SidecarErasure fields, times as unix milliseconds and zero as 0.
func TestSidecarEvsErasuresFields(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	erasureId := sidecar.Id{0x61}
	backend.erasures = []*sidecar.SidecarErasure{
		{ErasureId: erasureId, State: "waiting", ObjectsLeft: 12, BytesLeft: 3456, NextTry: time.UnixMilli(1_800_000_060_000), Finish: time.UnixMilli(1_800_003_600_000)},
		{ErasureId: sidecar.Id{0x62}, State: "done"},
	}
	evs := newTestSidecarEvs(t, backend, nil)
	callback := newTestEvsCallback[*SidecarErasures]()
	evs.Erasures(testEvsRepoId(8), callback)
	result, err := callback.await(t)
	if err != nil || !result.Ok || result.Erasures.Len() != 2 {
		t.Fatalf("%v %+v", err, result)
	}
	first := result.Erasures.Get(0)
	if first.ErasureId.String() != newId([16]byte(erasureId)).String() || first.State != "waiting" || first.ObjectsLeft != 12 ||
		first.BytesLeft != 3456 || first.NextTryMillis != 1_800_000_060_000 || first.FinishMillis != 1_800_003_600_000 {
		t.Errorf("first %+v", first)
	}
	if second := result.Erasures.Get(1); second.State != "done" || second.NextTryMillis != 0 || second.FinishMillis != 0 {
		t.Errorf("second %+v", second)
	}
}

// Each call answers once: on success, on a refusal, when the Sidecar closes under it with the core still working, when
// it is made after the close, and when the core panics, whose panic reaches the callback as its error.
func TestOneTerminalResult(t *testing.T) {
	backend := newTestSidecarEvsBackend()
	evs := newSidecarEvs(context.Background(), backend, defaultSidecarEvsSettings())
	repoId := testEvsRepoId(9)
	requireOne := func(name string, count func() int) {
		t.Helper()
		// a further answer, if one came, would be on its way by the time a marker write's answer is back
		testEvsWrite(t, evs, repoId, "marker.txt", []byte("m"), "")
		if got := count(); got != 1 {
			t.Errorf("%s: %d answers", name, got)
		}
	}
	success := newTestEvsCallback[*SidecarResult]()
	evs.Write(repoId, "ok.txt", []byte("ok"), "", success)
	if result, err := success.await(t); err != nil || !result.Ok {
		t.Fatalf("success %v %+v", err, result)
	}
	requireOne("success", success.answerCount)
	refusal := newTestEvsCallback[*SidecarResult]()
	evs.Write(repoId, "big.bin", make([]byte, 1024*1024+1), "", refusal)
	if result, err := refusal.await(t); err != nil || result.Code != sidecarEvsCodeTooLarge {
		t.Fatalf("refusal %v %+v", err, result)
	}
	requireOne("refusal", refusal.answerCount)
	panicking := newTestEvsCallback[*SidecarHistory]()
	backend.setHook(func(ctx context.Context, call string) error {
		if call == "history" {
			panic("a synthetic panic in the core")
		}
		return nil
	})
	evs.History(repoId, "ok.txt", panicking)
	if result, err := panicking.await(t); err == nil || result != nil {
		t.Fatalf("panic %v %+v", err, result)
	}
	requireOne("panic", panicking.answerCount)

	// the core holds a read until its context ends, which the close ends
	entered := make(chan struct{})
	backend.setHook(func(ctx context.Context, call string) error {
		if call != "read" {
			return nil
		}
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	})
	cancelled := newTestEvsCallback[*SidecarBytes]()
	evs.ReadHead(repoId, "ok.txt", cancelled)
	<-entered
	evs.close()
	if result, err := cancelled.await(t); err != nil || result.Ok || result.Code != sidecarEvsCodeUnavailable {
		t.Fatalf("cancelled %v %+v", err, result)
	}
	after := newTestEvsCallback[*SidecarErasures]()
	evs.Erasures(repoId, after)
	if result, err := after.await(t); err != nil || result.Ok || result.Code != sidecarEvsCodeUnavailable {
		t.Fatalf("after the close %v %+v", err, result)
	}
	// each answer is its call's goroutine's last act, so the counts at the answers are the counts for good
	if cancelled.answerCount() != 1 || after.answerCount() != 1 {
		t.Errorf("answers %d and %d", cancelled.answerCount(), after.answerCount())
	}
	// a nil callback takes its answer nowhere, and nothing panics
	evs.Write(repoId, "ok.txt", []byte("ok"), "", nil)
}

// A gkm client that refuses everything, for an EvsClient that must exist and need never key anything.
type testRefusingGkm struct{}

var errTestGkm = errors.New("a test's gkm refuses")

func (self testRefusingGkm) CurrentRepoKey(ctx context.Context, repoId sidecar.Id) (*sidecar.EvsRepoKey, error) {
	return nil, errTestGkm
}
func (self testRefusingGkm) RepoKey(ctx context.Context, repoId sidecar.Id, generation uint64) (*sidecar.EvsRepoKey, error) {
	return nil, errTestGkm
}
func (self testRefusingGkm) EnsureWritable(ctx context.Context, repoId sidecar.Id) (*sidecar.EvsRepoKey, error) {
	return nil, errTestGkm
}
func (self testRefusingGkm) DropKey(ctx context.Context, repoId sidecar.Id) (*sidecar.GkmDropKey, error) {
	return nil, errTestGkm
}
func (self testRefusingGkm) CreateGroup(ctx context.Context, args *sidecar.GkmCreateGroupArgs) (*sidecar.EvsRepoKey, error) {
	return nil, errTestGkm
}
func (self testRefusingGkm) RotateRepo(ctx context.Context, repoId sidecar.Id) (*sidecar.EvsRepoKey, error) {
	return nil, errTestGkm
}
func (self testRefusingGkm) AuthAdvanced(state sidecar.GkmAuthView) {}
func (self testRefusingGkm) ForgetRepo(ctx context.Context, repoId sidecar.Id) error {
	return errTestGkm
}

// A store that holds nothing and keeps nothing: what an EvsClient that never reads a pin needs.
type testEmptySidecarStore struct{}

func (self testEmptySidecarStore) Open(name string) (io.ReadCloser, error) {
	return nil, sidecar.ErrSidecarStoreAbsent
}
func (self testEmptySidecarStore) Create(name string) (sidecar.SidecarStoreWriter, error) {
	return nil, errors.New("a test's store keeps nothing")
}
func (self testEmptySidecarStore) Remove(name string) error {
	return nil
}
func (self testEmptySidecarStore) List(prefix string) ([]sidecar.SidecarStoreEntry, error) {
	return nil, nil
}
func (self testEmptySidecarStore) Directory(name string, class string) (sidecar.SidecarStore, error) {
	return self, nil
}

// A transport that reaches nothing and counts what it was asked.
type testCountingTransport struct {
	stateLock sync.Mutex
	count     int
}

func (self *testCountingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.count += 1
	return nil, errors.New("a test's transport reaches nothing")
}

// GetEvs is nil unless the settings name evs; with evs named, it wraps the core's EvsComponent a factory attached, the
// core's real EvsClient answering, here with no verified config, by refusing a read before any request. The
// registered factory refuses a Sidecar named with evs while no host composes an EvsClient, naming why.
func TestGetEvsWrapsTheCoresEvsComponent(t *testing.T) {
	world := newTestSidecarWorld(t)
	if bare := world.open(t); bare.GetEvs() != nil {
		t.Error("GetEvs without evs named")
	}

	transport := &testCountingTransport{}
	var built *sidecar.EvsComponent
	options := &sidecarLocalOptions{componentFactories: map[string]sidecarComponentFactory{
		SidecarComponentEvs: func(ctx context.Context, local *sidecar.Sidecar, settings *SidecarSettings) (sidecar.SidecarComponent, error) {
			client, err := sidecar.NewEvsClient(&sidecar.EvsClientArgs{
				NetworkId: sidecar.Id{0x71},
				EvsUrl:    "https://evs.sdktest.example",
				Transport: transport,
				ServiceToken: func(ctx context.Context, resource string, scopes []string) (string, error) {
					return "", errors.New("a test's token refuses")
				},
				Sign: func(purpose string, signedType bridge.SignedType, body []byte, agentId *sidecar.Id) (*bridge.Signed, error) {
					return nil, errors.New("a test's signer refuses")
				},
				Identity: func() *sidecar.SidecarIdentity {
					return &sidecar.SidecarIdentity{NetworkId: sidecar.Id{0x71}, SidecarId: sidecar.Id{0x72}, ClientId: sidecar.Id{0x73}, Generation: 1}
				},
				Gkm:      testRefusingGkm{},
				Store:    testEmptySidecarStore{},
				Settings: sidecar.DefaultEvsSettings(),
			})
			if err != nil {
				return nil, err
			}
			built = sidecar.NewEvsComponent(client)
			return built, nil
		},
	}}
	settings := DefaultSidecarSettings()
	settings.Components.Add(SidecarComponentEvs)
	second := newTestSidecarWorld(t)
	local := second.openWith(t, settings, options)
	evs := local.GetEvs()
	if evs == nil || built == nil || local.GetEvs() != evs {
		t.Fatalf("GetEvs %v, built %v", evs, built)
	}
	callback := newTestEvsCallback[*SidecarBytes]()
	evs.ReadHead(testEvsRepoId(10), "a.txt", callback)
	if result, err := callback.await(t); err != nil || result.Ok || result.Message == "" {
		t.Fatalf("a read with no verified config: %v %+v", err, result)
	}
	second.close(t, local)
	if local.GetEvs() != nil {
		t.Error("GetEvs after Close")
	}
	after := newTestEvsCallback[*SidecarBytes]()
	evs.ReadHead(testEvsRepoId(10), "a.txt", after)
	if result, _ := after.await(t); result.Ok || result.Code != sidecarEvsCodeUnavailable {
		t.Errorf("a read after Close %+v", result)
	}

	// the registered factory: no host composes an EvsClient yet
	refused := DefaultSidecarSettings()
	refused.StateDir = t.TempDir()
	refused.Components.Add(SidecarComponentEvs)
	if local, err := NewSidecarLocal(world.space, newTestSidecarKeyStore(), nil, refused); err == nil || !strings.Contains(err.Error(), "composes no evs client") {
		if local != nil {
			local.Close()
		}
		t.Errorf("evs with no composition: %v", err)
	}
}
