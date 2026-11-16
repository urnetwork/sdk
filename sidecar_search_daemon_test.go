//go:build !ios_extension && !js && !ios && !android

package sdk

// SidecarSearch's cases that run a fake ur daemon (SDK-M10-01): Cancel ending a remote stream with nothing more asked
// of the daemon, the remote backend streaming through the daemon's control mode, and a daemon refusal repeating the
// query reaching no message. Phones run no daemon and their builds link none of its types, so these run on desktops
// alone, beside sidecar_search_test.go's cases, which every app leg builds.

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// Cancel is local: the stream ends here and its source with it, so the core's cursors and remote legs end, and no page
// starts after Cancel returns. On the remote backend no call goes to the daemon after it, and none names a cancel.
func TestCancelStopsLocallyOnly(t *testing.T) {
	backend := newTestSearchBackend()
	search := newTestSidecarSearch(t, backend, nil)
	listener := newTestSearchListener()
	handle := search.Search(&SidecarSearchQuery{Text: "budget"}, listener)
	source := backend.nextSource(t)
	source.step(t, &testSearchStep{hits: []*SidecarSearchHit{testSearchHit(1)}, progress: &SidecarSearchProgress{}})
	listener.next(t)
	handle.Cancel()
	select {
	case <-source.closed:
	case <-time.After(testSidecarTimeout):
		t.Fatal("Cancel did not end the source")
	}
	listener.requireNone(t, func() {})
	// a More or a second Cancel after it gives nothing
	handle.More(10)
	handle.Cancel()
	listener.requireNone(t, func() {})

	// remote: the daemon answers a first page with a cursor; after Cancel nothing more is asked of it
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	searched := make(chan struct{}, 8)
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		if request.Method != sidecarControlMethodSearch {
			return nil, nil, false
		}
		searched <- struct{}{}
		return map[string]any{
			"hits":   []map[string]any{{"repo_id": sidecar.Id{0x5e}.String(), "version": 1, "time": "2027-01-15T08:00:00Z", "match": "possible", "partition": "current", "fields": map[string]string{"path": "a.md"}}},
			"status": map[string]any{"complete": false, "next_cursor": "cursor-1"},
		}, nil, true
	}
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	remoteSearch := remote.GetSearch()
	if remoteSearch == nil {
		t.Fatal("a remote Sidecar answers no search")
	}
	remoteListener := newTestSearchListener()
	remoteHandle := remoteSearch.Search(&SidecarSearchQuery{Text: "budget", PageSize: 1}, remoteListener)
	if page := remoteListener.next(t); len(page.hits) != 1 || page.done {
		t.Fatalf("the remote page: %d hits done %v", len(page.hits), page.done)
	}
	remoteHandle.Cancel()
	remoteListener.requireNone(t, func() {})
	before := daemon.seenMethods()
	for _, method := range before {
		if method != sidecarControlMethodSearch {
			t.Errorf("the daemon was asked %s", method)
		}
		if strings.Contains(method, "cancel") {
			t.Errorf("a cancel reached the daemon: %s", method)
		}
	}
	if len(before) != 1 {
		t.Errorf("the daemon was asked %d searches for one full page", len(before))
	}
}

// A remote-mode Sidecar's search streams through the daemon's control mode: the first call carries §16.10's arguments
// and asks for untrusted text, the next names the reply's cursor alone, the pages replace as each call's hits land,
// and the hits map their match and fields; GetStatus and SetPolicy forward, and DropCache and a background window are
// refused, the daemon running its own.
func TestRemoteModeStreams(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	repoId := sidecar.Id{0x5e, 0x01}
	// set once the test wants a daemon without the methods
	var missing atomic.Bool
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		if missing.Load() {
			return nil, nil, false
		}
		switch request.Method {
		case sidecarControlMethodSearch:
			params := &sidecarControlSearchParams{}
			if err := json.Unmarshal(request.Params, params); err != nil {
				return nil, &sidecar.DaemonRpcError{Code: sidecarControlCodeInvalidParams, Message: err.Error()}, true
			}
			if params.Cursor == "" {
				return map[string]any{
					"hits": []map[string]any{
						{"repo_id": repoId.String(), "version": 7, "time": "2027-01-15T08:00:00Z", "match": "confirmed", "by": "text", "partition": "current",
							"fields": map[string]string{"path": "plans/q1.md", "title": "Q1 plan"}, "snippet": "the q1 plan"},
						{"repo_id": repoId.String(), "version": 3, "time": "2026-06-01T08:00:00Z", "match": "confirmed", "by": "field", "partition": "all",
							"fields": map[string]string{"subject": "Re: plan", "from": "Dana"}},
					},
					"status": map[string]any{"complete": false, "next_cursor": "cursor-1"},
				}, nil, true
			}
			return map[string]any{
				"hits":   []map[string]any{{"repo_id": repoId.String(), "version": 2, "time": "2025-02-01T08:00:00Z", "match": "near", "correction": "plan", "partition": "older"}},
				"status": map[string]any{"complete": true},
			}, nil, true
		case sidecarControlMethodSearchStatus:
			return &SidecarSearchStatus{IndexOn: true, FormatSupported: true, Policy: "background", Remote: "off", AllCachedByteCount: 4096}, nil, true
		case sidecarControlMethodSearchPolicy:
			return map[string]any{}, nil, true
		}
		return nil, nil, false
	}
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	search := remote.GetSearch()
	if search == nil {
		t.Fatal("a remote Sidecar answers no search")
	}
	listener := newTestSearchListener()
	handle := search.Search(&SidecarSearchQuery{Text: "plan", RepoIds: testSearchIdList(repoId), Scope: SidecarSearchScopeAll, SinceMillis: 1_700_000_000_000}, listener)
	defer handle.Cancel()
	first := listener.next(t)
	if !slices.Equal(testSearchPaths(first.hits), []string{"plans/q1.md", ""}) || first.done {
		t.Fatalf("the first remote page: %v done %v", testSearchPaths(first.hits), first.done)
	}
	if hit := first.hits[0]; hit.State != SidecarSearchHitVerified || hit.Title != "Q1 plan" || hit.Version != 7 || hit.Partition != SidecarSearchPartitionLatest ||
		hit.TimeMillis != time.Date(2027, 1, 15, 8, 0, 0, 0, time.UTC).UnixMilli() || hit.Snippet != "the q1 plan" || hit.RepoId.Cmp(newId(repoId)) != 0 {
		t.Errorf("the verified hit: %+v", hit)
	}
	if hit := first.hits[1]; hit.State != SidecarSearchHitConfirmed || hit.Title != "Re: plan" || hit.From != "Dana" || hit.Partition != SidecarSearchPartitionAll {
		t.Errorf("the confirmed hit: %+v", hit)
	}
	last := listener.next(t)
	if len(last.hits) != 3 || !last.done || last.hits[2].State != SidecarSearchHitNear || last.hits[2].Correction != "plan" {
		t.Fatalf("the last remote page: %d hits done %v", len(last.hits), last.done)
	}
	searches := []*sidecarControlSearchParams{}
	for _, request := range daemon.seen() {
		if request.Method == sidecarControlMethodSearch {
			params := &sidecarControlSearchParams{}
			json.Unmarshal(request.Params, params)
			searches = append(searches, params)
		}
	}
	if len(searches) != 2 {
		t.Fatalf("the daemon took %d searches", len(searches))
	}
	if first := searches[0]; first.Query != "plan" || first.Scope != sidecarControlSearchScopeRepo || !slices.Equal(first.Repos, []string{"id:" + repoId.String()}) ||
		!first.More || first.Untrusted != sidecarControlSearchUntrustedInclude || first.Limit != 50 || first.WaitMs != 2000 ||
		first.Filters == nil || first.Filters.Since == "" || first.Cursor != "" {
		t.Errorf("the first call's arguments: %+v", first)
	}
	if next := searches[1]; next.Cursor != "cursor-1" || next.Query != "" || next.Untrusted != sidecarControlSearchUntrustedInclude {
		t.Errorf("the cursor call's arguments: %+v", next)
	}

	if status := search.GetStatus(newId(repoId)); status == nil || !status.IndexOn || status.Policy != "background" || status.AllCachedByteCount != 4096 {
		t.Errorf("the remote status: %+v", status)
	}
	policy, policyResults := testSidecarResultCallback()
	search.SetPolicy(newId(repoId), sidecar.SearchPolicyNever, policy)
	if result := <-policyResults; !result.Ok {
		t.Errorf("the remote policy: %+v", result)
	}
	for _, request := range daemon.seen() {
		if request.Method == sidecarControlMethodSearchPolicy {
			params := &sidecarControlSearchPolicyParams{}
			if json.Unmarshal(request.Params, params) != nil || params.Policy != sidecar.SearchPolicyNever || params.RepoId == nil || params.RepoId.Cmp(newId(repoId)) != 0 {
				t.Errorf("search.policy's params: %s", request.Params)
			}
		}
	}
	dropped, droppedResults := testSidecarResultCallback()
	search.DropCache(nil, dropped)
	if result := <-droppedResults; result.Ok || result.Code != SidecarSearchCodeRefused {
		t.Errorf("a remote DropCache: %+v", result)
	}
	window, windowResults := testSidecarResultCallback()
	search.RunBackgroundSync(1000, window)
	if result := <-windowResults; result.Ok || result.Code != SidecarSearchCodeRefused {
		t.Errorf("a remote background window: %+v", result)
	}

	// a daemon without the method reads unavailable
	missing.Store(true)
	unanswered := newTestSearchListener()
	search.Search(&SidecarSearchQuery{Text: "plan"}, unanswered)
	if page := unanswered.next(t); !page.done || page.progress.Code != SidecarSearchCodeUnavailable {
		t.Errorf("a daemon with no search method: %+v", page.progress)
	}
}

// A query never reaches a message, a log or a crash report (I-19): a core error and a daemon refusal that each repeat
// the query answer their code's fixed text, and no file of the wrapper logs at all.
func TestQueryCanaryInNoLog(t *testing.T) {
	const canary = "query-canary-7f3a9c"
	backend := newTestSearchBackend()
	search := newTestSidecarSearch(t, backend, nil)
	listener := newTestSearchListener()
	handle := search.Search(&SidecarSearchQuery{Text: canary}, listener)
	defer handle.Cancel()
	source := backend.nextSource(t)
	source.step(t, &testSearchStep{err: fmt.Errorf("an engine that repeats %s: %w", canary, sidecar.ErrSearchBusy)})
	page := listener.next(t)
	if !page.done || page.progress.Code != SidecarSearchCodeBusy || strings.Contains(page.progress.Message, canary) {
		t.Fatalf("a core error: %+v", page.progress)
	}

	backend.searchErr = fmt.Errorf("a backend that repeats %s", canary)
	refused := newTestSearchListener()
	search.Search(&SidecarSearchQuery{Text: canary}, refused)
	if page := refused.next(t); page.progress.Code != SidecarSearchCodeFailed || strings.Contains(page.progress.Message, canary) {
		t.Fatalf("a backend refusal: %+v", page.progress)
	}

	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		if request.Method == sidecarControlMethodSearch {
			return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "a daemon that repeats " + canary}, true
		}
		return nil, nil, false
	}
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	remoteListener := newTestSearchListener()
	remote.GetSearch().Search(&SidecarSearchQuery{Text: canary}, remoteListener)
	if page := remoteListener.next(t); page.progress.Code != SidecarSearchCodeFailed || strings.Contains(page.progress.Message, canary) {
		t.Fatalf("a daemon refusal: %+v", page.progress)
	}
	for code, message := range sidecarSearchCodeMessages {
		if strings.Contains(message, canary) || message == "" {
			t.Errorf("%s's message: %q", code, message)
		}
	}

	// no file of the wrapper logs: none imports a logger or prints
	files, err := filepath.Glob("sidecar_search*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("the wrapper's files: %v %v", files, err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imported := range parsed.Imports {
			path := strings.Trim(imported.Path.Value, `"`)
			if path == "log" || path == "log/slog" || strings.HasSuffix(path, "/glog") || path == "fmt" {
				t.Errorf("%s imports %s", file, path)
			}
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range []string{"Infof(", "Warningf(", "Errorf(", "Println(", "Printf(", "gmLog("} {
			if strings.Contains(string(contents), call) {
				t.Errorf("%s calls %s", file, call)
			}
		}
	}
}
