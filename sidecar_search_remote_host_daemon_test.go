//go:build !ios_extension && !js && !ios && !android

package sdk

// Remote search's requester seams on a remote-mode Sidecar over a fake ur daemon (SDK-M10-03; §18.7's control row): it
// lists no indexer and refuses the mode and the notice answers with nothing sent, while its search still streams from
// the daemon; and no control method this sdk calls carries a mode or an answer, so neither reaches the daemon MCP runs
// on. Phones run no daemon, so these run on desktops alone.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A remote-mode Sidecar on a fake daemon lists no indexer and refuses SetRemote and AcknowledgeIndexer, the daemon
// asked nothing for any of them, while its search still goes to the daemon.
func TestRemoteModeSidecarAsksNoIndexerOfItsDaemon(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		if request.Method != sidecarControlMethodSearch {
			return nil, nil, false
		}
		return map[string]any{"hits": []map[string]any{}, "status": map[string]any{"complete": true}}, nil, true
	}
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	search := remote.GetSearch()
	if search == nil {
		t.Fatal("a remote Sidecar answers no search")
	}
	before := daemon.seenMethods()
	if list := search.GetIndexers(); list == nil || list.Len() != 0 {
		t.Errorf("a remote-mode Sidecar lists %v", list)
	}
	for _, call := range []func(callback SidecarResultCallback){
		func(callback SidecarResultCallback) { search.SetRemote(SidecarSearchRemoteOwn, callback) },
		func(callback SidecarResultCallback) {
			search.AcknowledgeIndexer(newId(sidecar.NewId()), true, callback)
		},
		func(callback SidecarResultCallback) { search.AcknowledgeIndexer(nil, false, callback) },
	} {
		if result := testT4Result(t, call); result.Ok || result.Code != SidecarSearchCodeRefused {
			t.Errorf("a remote-mode call answered %+v", result)
		}
	}
	if after := daemon.seenMethods(); len(after) != len(before) {
		t.Errorf("the daemon was asked %v", after[len(before):])
	}
	listener := newTestSearchListener()
	handle := search.Search(&SidecarSearchQuery{Text: "harbor", Scope: SidecarSearchScopeAll}, listener)
	defer handle.Cancel()
	page := listener.next(t)
	for !page.done {
		page = listener.next(t)
	}
	if page.progress.Remote != "" || page.progress.Code != "" {
		t.Errorf("a remote-mode search more's progress: %+v", page.progress)
	}
	searched := 0
	for _, method := range daemon.seenMethods() {
		if method == sidecarControlMethodSearch {
			searched += 1
		}
	}
	if searched != 1 {
		t.Errorf("the daemon was asked %d searches", searched)
	}
}

// No control method this sdk names carries the search mode or a notice's answer (§18.4's setting row: never set through
// MCP): every string constant named sidecarControlMethod in the package's sources is read, and none names one. A
// control method added for it fails here.
func TestNoControlMethodCarriesTheMode(t *testing.T) {
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	methods := map[string]string{}
	fileSet := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range parsed.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, name := range value.Names {
					if !strings.HasPrefix(name.Name, "sidecarControlMethod") || len(value.Values) <= index {
						continue
					}
					literal, ok := value.Values[index].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					method, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					methods[name.Name] = method
				}
			}
		}
	}
	if methods["sidecarControlMethodSearch"] != "search" || methods["sidecarControlMethodSearchPolicy"] != "search.policy" {
		t.Fatalf("CONTROL FAILED: the scan did not find the search methods: %v", methods)
	}
	// the search methods a remote-mode app calls (§18.7's control row) are these three, and no method names the mode,
	// a notice or an answer; indexer.designate and indexer.retire_scope are the mail receiver's, not search's
	searchMethods := map[string]bool{"search": true, "search.status": true, "search.policy": true}
	for name, method := range methods {
		lower := strings.ToLower(method)
		if strings.HasPrefix(lower, "search") && !searchMethods[lower] {
			t.Errorf("%s = %q is a search method §18.7's control row does not name", name, method)
		}
		for _, word := range []string{"remote", "acknowledge", "notice"} {
			if strings.Contains(lower, word) {
				t.Errorf("%s = %q carries the search mode or an answer to the daemon", name, method)
			}
		}
	}
}
