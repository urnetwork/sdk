//go:build !ios_extension && !js

package sdk

// Remote search's requester seams (SDK-M10-03): GetIndexers, SetRemote and AcknowledgeIndexer over the core's search
// state on an enrolled app host, and none of them on a remote-mode Sidecar, which asks no indexer and sends its daemon
// nothing for them; the remote states, fallback reasons and indexer states one for one. Every app leg builds these;
// the cases over a fake ur daemon are sidecar_search_remote_host_daemon_test.go's. Every id is synthetic, and every
// wait is on a channel.

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A remote backend's daemon standing in for ur daemon's control session: it records each call and answers none.
type testRecordingDaemon struct {
	stateLock sync.Mutex
	methods   []string
}

func (self *testRecordingDaemon) sidecarId() *Id {
	return nil
}

func (self *testRecordingDaemon) state() (string, string) {
	return "online", ""
}

func (self *testRecordingDaemon) generation() int64 {
	return 1
}

func (self *testRecordingDaemon) refresh(ctx context.Context) error {
	return nil
}

func (self *testRecordingDaemon) pair(ctx context.Context, childJwt string) (*sidecarControlPairReply, error) {
	return nil, errors.New("a recording daemon pairs nothing")
}

func (self *testRecordingDaemon) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.methods = append(self.methods, method)
	return &sidecarControlError{Code: sidecarControlCodeMethodNotFound, Message: "method not found"}
}

func (self *testRecordingDaemon) close() {
}

// Every call the daemon took, in order.
func (self *testRecordingDaemon) calls() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.methods)
}

// A remote-mode Sidecar's SidecarSearch over daemon.
func newTestRemoteModeSearch(t *testing.T, daemon *testRecordingDaemon) *SidecarSearch {
	t.Helper()
	settings := defaultSidecarSearchSettings()
	return newTestSidecarSearch(t, &sidecarSearchRemoteBackend{remote: daemon, settings: settings}, nil)
}

// An enrolled phone's role whose core runs the T4 surrogate's requester over world's fake indexers and any extra ones.
func newTestRequesterRole(t *testing.T, world *testT4RemoteWorld, extra ...*testT4FakeIndexer) (*testT4Role, *SidecarSearch) {
	t.Helper()
	role := newTestEnrolledT4Role(t, SidecarHostClassIos, nil)
	engine, err := world.install(role.local.Core())
	if err != nil {
		t.Fatal(err)
	}
	requester := engine.(*testT4Requester)
	requester.indexers = append(requester.indexers, extra...)
	uninstall, err := role.local.Core().InstallSearchEngine(requester)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(uninstall)
	search := role.local.GetSearch()
	if search == nil {
		t.Fatal("the role has no search")
	}
	return role, search
}

// The listed indexer of sidecarId, or nil.
func testListedIndexer(search *SidecarSearch, sidecarId sidecar.Id) *SidecarIndexer {
	for _, indexer := range search.GetIndexers().getAll() {
		if indexer.SidecarId.Cmp(newId(sidecarId)) == 0 {
			return indexer
		}
	}
	return nil
}

// A remote-mode Sidecar asks no indexer (§18.7's control row): GetIndexers lists none, never nil, and the daemon is
// asked nothing for it. The app host's list, by contrast, holds each indexer that may answer it, own first.
func TestRemoteModeListsNoIndexers(t *testing.T) {
	daemon := &testRecordingDaemon{}
	search := newTestRemoteModeSearch(t, daemon)
	list := search.GetIndexers()
	if list == nil || list.Len() != 0 {
		t.Fatalf("a remote-mode Sidecar lists %v", list)
	}
	if calls := daemon.calls(); len(calls) != 0 {
		t.Errorf("listing reached the daemon: %v", calls)
	}
	// a closed search lists none and answers nothing more
	search.close()
	if list := search.GetIndexers(); list == nil || list.Len() != 0 {
		t.Errorf("a closed search lists %v", list)
	}

	world := newTestT4RemoteWorld()
	_, local := newTestRequesterRole(t, world)
	indexers := local.GetIndexers().getAll()
	if len(indexers) != 2 || !indexers[0].Own || indexers[0].SidecarId.Cmp(newId(world.nas.sidecarId)) != 0 || indexers[1].Own {
		t.Errorf("an app host lists %+v", indexers)
	}
}

// SetRemote and AcknowledgeIndexer on a remote-mode Sidecar answer SEARCH_REFUSED, whatever they name, and send its
// daemon nothing: its daemon's mode and answers are not a remote-mode app's to set. On a host whose core runs no
// requester side, as ur daemon's, the core refuses them too, and its list is empty.
func TestSetRemoteRefusedOnRemoteHost(t *testing.T) {
	daemon := &testRecordingDaemon{}
	search := newTestRemoteModeSearch(t, daemon)
	memberId := newId(sidecar.NewId())
	for _, call := range []func(callback SidecarResultCallback){
		func(callback SidecarResultCallback) { search.SetRemote(SidecarSearchRemoteOn, callback) },
		func(callback SidecarResultCallback) { search.SetRemote(SidecarSearchRemoteOwn, callback) },
		func(callback SidecarResultCallback) { search.SetRemote(SidecarSearchRemoteOff, callback) },
		func(callback SidecarResultCallback) { search.SetRemote("sometimes", callback) },
		func(callback SidecarResultCallback) { search.AcknowledgeIndexer(memberId, true, callback) },
		func(callback SidecarResultCallback) { search.AcknowledgeIndexer(memberId, false, callback) },
		func(callback SidecarResultCallback) { search.AcknowledgeIndexer(nil, false, callback) },
	} {
		result := testT4Result(t, call)
		if result.Ok || result.Code != SidecarSearchCodeRefused || result.Message != sidecarSearchCodeMessages[SidecarSearchCodeRefused] {
			t.Errorf("a remote-mode call answered %+v", result)
		}
	}
	if calls := daemon.calls(); len(calls) != 0 {
		t.Errorf("the daemon was asked %v", calls)
	}

	// a closed search answers unavailable
	search.close()
	if result := testT4Result(t, func(callback SidecarResultCallback) { search.SetRemote(SidecarSearchRemoteOn, callback) }); result.Ok || result.Code != SidecarSearchCodeUnavailable {
		t.Errorf("SetRemote on a closed search: %+v", result)
	}

	// the core of a host that asks no indexer refuses, as the daemon's does
	world := newTestT4RemoteWorld()
	role := newTestEnrolledT4Role(t, SidecarHostClassDesktop, func(settings *sidecar.SidecarSettings) {
		settings.Search.RemoteAsk = false
	})
	engine, err := world.install(role.local.Core())
	if err != nil {
		t.Fatal(err)
	}
	uninstall, err := role.local.Core().InstallSearchEngine(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer uninstall()
	daemonHost := role.local.GetSearch()
	for _, call := range []func(callback SidecarResultCallback){
		func(callback SidecarResultCallback) { daemonHost.SetRemote(SidecarSearchRemoteOwn, callback) },
		func(callback SidecarResultCallback) {
			daemonHost.AcknowledgeIndexer(newId(world.otherId), true, callback)
		},
	} {
		if result := testT4Result(t, call); result.Ok || result.Code != SidecarSearchCodeRefused {
			t.Errorf("a host that asks no indexer answered %+v", result)
		}
	}
	if list := daemonHost.GetIndexers(); list.Len() != 0 {
		t.Errorf("a host that asks no indexer lists %d", list.Len())
	}
	if status := daemonHost.GetStatus(nil); status == nil || status.Remote != SidecarSearchRemoteOff {
		t.Errorf("a host that asks no indexer reads its mode as %+v", status)
	}
}

// [Only my devices]: AcknowledgeIndexer(member, false) records the refusal and sets the mode to own in the same write,
// so another member's indexer is asked nothing more; an allow then changes no mode; an unknown mode is refused, the mode
// staying, and SetRemote on brings it back.
func TestAcknowledgeFalseSetsOwn(t *testing.T) {
	world := newTestT4RemoteWorld()
	role, search := newTestRequesterRole(t, world)
	if status := search.GetStatus(nil); status == nil || status.Remote != SidecarSearchRemoteOn {
		t.Fatalf("the mode at start: %+v", status)
	}
	if result := testT4Result(t, func(callback SidecarResultCallback) { search.AcknowledgeIndexer(newId(world.otherId), false, callback) }); !result.Ok {
		t.Fatalf("the refusal: %+v", result)
	}
	if status := search.GetStatus(nil); status == nil || status.Remote != SidecarSearchRemoteOwn {
		t.Fatalf("the mode after the refusal: %+v", status)
	}
	state, err := role.local.Core().SearchState()
	if err != nil {
		t.Fatal(err)
	}
	if allow, answered, err := state.Answer(world.otherId); err != nil || !answered || allow {
		t.Errorf("the answer kept: allow %v answered %v %v", allow, answered, err)
	}
	if listed := testListedIndexer(search, world.laptop.sidecarId); listed == nil || listed.Acknowledged {
		t.Errorf("the refused member's indexer: %+v", listed)
	}
	// a search more in mode own asks only the own indexer
	listener := newTestSearchListener()
	handle := search.Search(&SidecarSearchQuery{Text: "harbor", Scope: SidecarSearchScopeAll}, listener)
	for page := listener.next(t); !page.done; page = listener.next(t) {
	}
	handle.Cancel()
	if asked := world.laptop.asks(); len(asked) != 0 {
		t.Errorf("another member's device was asked in mode own: %+v", asked)
	}

	if result := testT4Result(t, func(callback SidecarResultCallback) { search.AcknowledgeIndexer(newId(world.otherId), true, callback) }); !result.Ok {
		t.Fatalf("the allow: %+v", result)
	}
	if listed := testListedIndexer(search, world.laptop.sidecarId); listed == nil || !listed.Acknowledged {
		t.Errorf("the allowed member's indexer: %+v", listed)
	}
	if status := search.GetStatus(nil); status == nil || status.Remote != SidecarSearchRemoteOwn {
		t.Errorf("an allow moved the mode: %+v", status)
	}
	if result := testT4Result(t, func(callback SidecarResultCallback) { search.SetRemote("everywhere", callback) }); result.Ok || result.Code != SidecarSearchCodeInvalid {
		t.Errorf("an unknown mode: %+v", result)
	}
	if status := search.GetStatus(nil); status == nil || status.Remote != SidecarSearchRemoteOwn {
		t.Errorf("an unknown mode moved the mode: %+v", status)
	}
	if result := testT4Result(t, func(callback SidecarResultCallback) { search.SetRemote(SidecarSearchRemoteOn, callback) }); !result.Ok {
		t.Fatalf("SetRemote on: %+v", result)
	}
	if status := search.GetStatus(nil); status == nil || status.Remote != SidecarSearchRemoteOn {
		t.Errorf("the mode after SetRemote on: %+v", status)
	}
}

// "Reset notices": AcknowledgeIndexer(nil, ...) clears every answer, allowed and refused alike, the mode staying, so
// each member's notice is shown again before its device is asked.
func TestResetNoticesClears(t *testing.T) {
	world := newTestT4RemoteWorld()
	thirdId := sidecar.NewId()
	third := &testT4FakeIndexer{sidecarId: sidecar.NewId(), name: "kai-desktop", memberId: thirdId, member: "Kai", repoIds: []sidecar.Id{world.recordId}}
	_, search := newTestRequesterRole(t, world, third)
	for _, memberId := range []sidecar.Id{world.otherId, thirdId} {
		if result := testT4Result(t, func(callback SidecarResultCallback) { search.AcknowledgeIndexer(newId(memberId), true, callback) }); !result.Ok {
			t.Fatalf("allowing %s: %+v", memberId, result)
		}
	}
	for _, indexerId := range []sidecar.Id{world.laptop.sidecarId, third.sidecarId} {
		if listed := testListedIndexer(search, indexerId); listed == nil || !listed.Acknowledged {
			t.Fatalf("an allowed member's indexer: %+v", listed)
		}
	}
	if result := testT4Result(t, func(callback SidecarResultCallback) { search.AcknowledgeIndexer(nil, false, callback) }); !result.Ok {
		t.Fatalf("reset notices: %+v", result)
	}
	for _, indexerId := range []sidecar.Id{world.laptop.sidecarId, third.sidecarId} {
		if listed := testListedIndexer(search, indexerId); listed == nil || listed.Acknowledged {
			t.Errorf("an answer outlived the reset: %+v", listed)
		}
	}
	if status := search.GetStatus(nil); status == nil || status.Remote != SidecarSearchRemoteOn {
		t.Errorf("the reset moved the mode: %+v", status)
	}
	// the record waits on a notice again: nothing reaches either member's device
	listener := newTestSearchListener()
	handle := search.Search(&SidecarSearchQuery{Text: "harbor", Scope: SidecarSearchScopeAll, RepoIds: testSearchIdList(world.recordId)}, listener)
	page := listener.next(t)
	for !page.done {
		page = listener.next(t)
	}
	handle.Cancel()
	if page.progress.Remote != SidecarSearchRemoteStateNotice || len(world.laptop.asks()) != 0 || len(third.asks()) != 0 {
		t.Errorf("after the reset: %+v, asked %d and %d", page.progress, len(world.laptop.asks()), len(third.asks()))
	}

	// a reset in mode own keeps own
	if result := testT4Result(t, func(callback SidecarResultCallback) { search.SetRemote(SidecarSearchRemoteOwn, callback) }); !result.Ok {
		t.Fatal(result)
	}
	if result := testT4Result(t, func(callback SidecarResultCallback) { search.AcknowledgeIndexer(nil, true, callback) }); !result.Ok {
		t.Fatal(result)
	}
	if status := search.GetStatus(nil); status == nil || status.Remote != SidecarSearchRemoteOwn {
		t.Errorf("a reset in mode own reads %+v", status)
	}
}

// The mode is the person's, set on this device and nowhere else (§18.4's setting row): SetRemote writes the network's
// search.state under the role's state directory and posts the core's api no statement, so the config repo never holds it;
// and the core of the host MCP's tools run on, ur daemon's, which asks no indexer, refuses every way to set it (the
// remote-mode app's own path is TestSetRemoteRefusedOnRemoteHost's, and the control methods' names are
// TestNoControlMethodCarriesTheMode's).
func TestModeNeverThroughMcp(t *testing.T) {
	world := newTestT4RemoteWorld()
	role, search := newTestRequesterRole(t, world)
	before := len(role.coreServer.seen())
	if result := testT4Result(t, func(callback SidecarResultCallback) { search.SetRemote(SidecarSearchRemoteOwn, callback) }); !result.Ok {
		t.Fatalf("SetRemote own: %+v", result)
	}
	if result := testT4Result(t, func(callback SidecarResultCallback) { search.AcknowledgeIndexer(newId(world.otherId), true, callback) }); !result.Ok {
		t.Fatalf("an answer: %+v", result)
	}
	// the core's own rounds may reach its api meanwhile: a version sync, and the member replica's follower reading
	// §7.9's log (GET /network/config/projection), as every enrolled member host follows its verified configuration
	// (§3.7's F1). No write does, a statement or a base, which is how anything reaches the config repo (§7.9's POST
	// /network/config/delta and POST /network/config/projection)
	for _, request := range role.coreServer.seen()[before:] {
		if strings.HasPrefix(request.path, "/network/config") && request.method != http.MethodGet {
			t.Errorf("setting the mode reached the config: %s %s", request.method, request.path)
		}
	}
	found := []string{}
	if err := filepath.WalkDir(role.world.stateDir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() == "search.state" {
			found = append(found, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Errorf("the network's search.state: %v", found)
	}

	daemon := newTestEnrolledT4Role(t, SidecarHostClassDesktop, func(settings *sidecar.SidecarSettings) {
		settings.Search.RemoteAsk = false
		settings.Search.RemoteServe = true
	})
	core := daemon.local.Core()
	if err := core.SetSearchRemote(sidecar.SearchRemoteOn); !errors.Is(err, sidecar.ErrSidecarSearchRefused) {
		t.Errorf("the daemon's core set a mode: %v", err)
	}
	if err := core.AcknowledgeSearchIndexer(&world.otherId, true); !errors.Is(err, sidecar.ErrSidecarSearchRefused) {
		t.Errorf("the daemon's core took an answer: %v", err)
	}
}

// A search's remote state, its fallback reason and an indexer's state map one for one from the core's names, and a name
// §18.5 does not give reads as none, or for an indexer as unknown, never passed on.
func TestRemoteStatesMapOneForOne(t *testing.T) {
	for core, bound := range map[string]string{
		sidecar.SearchRemoteStateNotice:   SidecarSearchRemoteStateNotice,
		sidecar.SearchRemoteStateAsking:   SidecarSearchRemoteStateAsking,
		sidecar.SearchRemoteStateAnswered: SidecarSearchRemoteStateAnswered,
		sidecar.SearchRemoteStatePartial:  SidecarSearchRemoteStatePartial,
		sidecar.SearchRemoteStateFallback: SidecarSearchRemoteStateFallback,
		sidecar.SearchRemoteStateOff:      SidecarSearchRemoteStateOff,
		"":                                "",
		"warming":                         "",
	} {
		if got := sidecarSearchRemoteStateOf(core); got != bound {
			t.Errorf("remote state %q reads %q, want %q", core, got, bound)
		}
	}
	for core, bound := range map[string]string{
		sidecar.SearchFallbackNone:        SidecarSearchFallbackNone,
		sidecar.SearchFallbackOffline:     SidecarSearchFallbackOffline,
		sidecar.SearchFallbackBusy:        SidecarSearchFallbackBusy,
		sidecar.SearchFallbackRefused:     SidecarSearchFallbackRefused,
		sidecar.SearchFallbackUnreachable: SidecarSearchFallbackUnreachable,
		sidecar.SearchFallbackOff:         SidecarSearchFallbackOff,
		sidecar.SearchFallbackNotice:      SidecarSearchFallbackNotice,
		"":                                "",
		"flaky":                           "",
	} {
		if got := sidecarSearchFallbackReasonOf(core); got != bound {
			t.Errorf("fallback reason %q reads %q, want %q", core, got, bound)
		}
	}
	if len(sidecarSearchRemoteStates) != 6 || len(sidecarSearchFallbackReasons) != 7 {
		t.Errorf("the tables hold %d states and %d reasons, §18.5's 6 and 7", len(sidecarSearchRemoteStates), len(sidecarSearchFallbackReasons))
	}
	for _, state := range []string{"online", "unreachable", "busy", "unknown"} {
		if got := sidecarIndexerStateOf(state); got != state {
			t.Errorf("indexer state %q reads %q", state, got)
		}
	}
	if got := sidecarIndexerStateOf("warm"); got != SidecarIndexerUnknown {
		t.Errorf("an indexer state §18.5 does not give reads %q", got)
	}
	indexerId := sidecar.NewId()
	progress := sidecarSearchProgressOf(&sidecar.SearchProgress{Remote: "warming", FallbackReason: "flaky", RemoteBy: &indexerId}, func(sidecarId sidecar.Id) *SidecarIndexer {
		return sidecarIndexerOf(&sidecar.SearchIndexer{SidecarId: sidecarId, State: "warm"})
	})
	if progress.Remote != "" || progress.FallbackReason != "" || progress.RemoteBy == nil || progress.RemoteBy.State != SidecarIndexerUnknown {
		t.Errorf("a progress of names §18.5 does not give: %+v by %+v", progress, progress.RemoteBy)
	}
	list := NewSidecarIndexerList()
	list.Add(&SidecarIndexer{SidecarId: newId(indexerId), Name: "nas", State: SidecarIndexerOnline})
	if encoded, err := json.Marshal(list); err != nil || len(encoded) == 0 || encoded[0] != '[' {
		t.Errorf("the indexer list's json: %s %v", encoded, err)
	}
}
