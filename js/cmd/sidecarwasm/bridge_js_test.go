//go:build js && wasm && sidecar_http

// The page Worker's shell on its own leg (SDK-M4-04), in Node under test/sidecarwasm_harness.mjs with the fake
// IndexedDB: the published calls answer through promises with JSON whose ids are strings, a Worker before open and an
// unenrolled one answer each operation's refusal, enroll answers unavailable, an operation over 50 ms hands the
// thread to the event loop between its steps, and each network's Worker opens that network's own database
// (SDK-M4-10, owner decision 217). test/sidecarwasm_ops.test.ts builds and runs them.

package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"syscall/js"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/sdk/js/sidecarworker"
)

// The urls of a stack nothing dials: a test's Worker reaches no service.
func testPageServiceUrls() *pageServiceUrls {
	return &pageServiceUrls{
		Api:      "https://api.pagetest.example",
		Auth:     "https://auth.pagetest.example",
		Evs:      "https://evs.pagetest.example",
		Gkm:      "https://gkm.pagetest.example",
		Sockets:  "https://sockets.pagetest.example",
		Dns:      "https://dns.pagetest.example",
		Platform: "wss://connect.pagetest.example",
	}
}

// A page Worker naming its networks' databases under prefix, closed at the end.
func testPageWorkerUnder(t *testing.T, prefix string) *pageWorker {
	t.Helper()
	testFakeIndexedDb(t)
	worker := newPageWorker(context.Background())
	worker.databaseNamePrefix = prefix
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		worker.close(ctx)
	})
	return worker
}

// A page Worker on databases of the test's own, closed at the end.
func testPageWorker(t *testing.T) *pageWorker {
	t.Helper()
	return testPageWorkerUnder(t, testDatabaseNamePrefix(t))
}

// Opens networkId on worker with a stack nothing dials.
func testOpenNetwork(t *testing.T, worker *pageWorker, networkId sidecar.Id) *pageStatusView {
	t.Helper()
	status, err := worker.open(&pageOpenConfig{NetworkId: networkId.String(), Urls: testPageServiceUrls()})
	if err != nil {
		t.Fatal(err)
	}
	return status
}

// The reply to request's JSON.
func testPageReply(t *testing.T, worker *pageWorker, request string) *sidecarworker.Reply {
	t.Helper()
	reply := &sidecarworker.Reply{}
	if err := json.Unmarshal(worker.handle(context.Background(), []byte(request)), reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

// Before open, each operation answers as it would on a Worker with nothing open: reads and the sign-out unavailable,
// enroll unavailable, a malformed request invalid.
func TestPageWorkerBeforeOpen(t *testing.T) {
	worker := testPageWorker(t)
	repoId := sidecar.NewId().String()
	for _, c := range []struct {
		request string
		code    string
	}{
		{request: `{"id":"a","op":"list","repo_id":"` + repoId + `"}`, code: sidecarworker.CodeUnavailable},
		{request: `{"id":"b","op":"record_view","log_id":"` + repoId + `"}`, code: sidecarworker.CodeUnavailable},
		{request: `{"id":"c","op":"sign_out"}`, code: sidecarworker.CodeUnavailable},
		{request: `{"id":"d","op":"enroll","code":"abc"}`, code: sidecarworker.CodeUnavailable},
		{request: `{"id":"e","op":"list"}`, code: sidecarworker.CodeInvalid},
		{request: `[]`, code: sidecarworker.CodeInvalid},
	} {
		if reply := testPageReply(t, worker, c.request); reply.Ok || reply.Code != c.code {
			t.Errorf("%s: %+v", c.request, reply)
		}
	}
}

// open opens the network on the key origin's database, unenrolled when the instance holds no record of it, and
// answers its state; each read then answers not_enrolled, a sign-out has nothing to sign out of, and a second network
// is refused, since a Worker serves one.
func TestPageWorkerOpensUnenrolled(t *testing.T) {
	worker := testPageWorker(t)
	networkId := sidecar.NewId()
	status, err := worker.open(&pageOpenConfig{NetworkId: networkId.String(), Urls: testPageServiceUrls()})
	if err != nil {
		t.Fatal(err)
	}
	if status.NetworkId != networkId.String() || status.SidecarId != "" {
		t.Fatalf("the status: %+v", status)
	}
	again, err := worker.open(&pageOpenConfig{NetworkId: networkId.String()})
	if err != nil || again.NetworkId != networkId.String() {
		t.Fatalf("opening the same network again: %+v %v", again, err)
	}
	if _, err := worker.open(&pageOpenConfig{NetworkId: sidecar.NewId().String()}); err == nil {
		t.Fatal("a Worker opened a second network")
	}
	if _, err := worker.open(&pageOpenConfig{NetworkId: "not an id"}); err == nil {
		t.Fatal("open took a network id that is not one")
	}
	repoId := sidecar.NewId().String()
	for _, request := range []string{
		`{"id":"a","op":"list","repo_id":"` + repoId + `"}`,
		`{"id":"b","op":"read_view","repo_id":"` + repoId + `","path":"a.txt"}`,
		`{"id":"c","op":"record_view","log_id":"` + repoId + `"}`,
	} {
		if reply := testPageReply(t, worker, request); reply.Ok || reply.Code != sidecarworker.CodeNotEnrolled {
			t.Errorf("%s: %+v", request, reply)
		}
	}
	if reply := testPageReply(t, worker, `{"id":"out","op":"enroll"}`); reply.Code != sidecarworker.CodeUnavailable {
		t.Errorf("enroll: %+v", reply)
	}
	if reply := testPageReply(t, worker, `{"id":"out","op":"sign_out"}`); !reply.Ok || reply.SignOut == nil || reply.SignOut.SidecarId != "" {
		t.Errorf("a sign-out with nothing enrolled: %+v", reply)
	}
}

// open names the network's database by the key origin's prefix and the parsed id's canonical text,
// urnetwork-sidecar-v1-<network_id>, and that database holds the network's instance, its instance document and its
// root key; the unsuffixed urnetwork-sidecar-v1 is never made; and an id written any other way than its canonical
// text, worker.js's spelling of the network's lock, or the zero id, is refused before any database opens.
func TestPageWorkerOpensItsNetworksOwnDatabase(t *testing.T) {
	control := testFakeIndexedDb(t)
	networkId := sidecar.NewId()
	testOpenNetwork(t, testPageWorkerUnder(t, indexedDbNamePrefix), networkId)
	name := "urnetwork-sidecar-v1-" + networkId.String()
	databases := control.Get("databases")
	if !databases.Call("has", name).Bool() {
		t.Fatalf("no database %s", name)
	}
	for _, record := range []struct {
		store string
		key   string
	}{
		{store: indexedDbFilesStore, key: "sidecar.json"},
		{store: indexedDbFilesStore, key: "unlock.wrap"},
		{store: indexedDbKeysStore, key: webCryptoRootKeyName},
	} {
		if control.Call("raw", name, record.store, record.key).IsUndefined() {
			t.Errorf("the network's database holds no %s", record.key)
		}
	}
	if databases.Call("has", "urnetwork-sidecar-v1").Bool() {
		t.Error("the unsuffixed database was made")
	}
	databaseCount := databases.Get("size").Int()
	for _, spelling := range []string{
		strings.ToUpper(networkId.String()),
		strings.ReplaceAll(networkId.String(), "-", ""),
		sidecar.Id{}.String(),
	} {
		other := testPageWorkerUnder(t, indexedDbNamePrefix)
		if _, err := other.open(&pageOpenConfig{NetworkId: spelling, Urls: testPageServiceUrls()}); err == nil {
			t.Errorf("open took %s", spelling)
		}
		if other.current() != nil {
			t.Errorf("a refused %s left a network open", spelling)
		}
	}
	if databases.Get("size").Int() != databaseCount {
		t.Error("a refused open made a database")
	}
	// every database under the key origin's prefix is named by an id's canonical text
	names := js.Global().Get("Array").Call("from", databases.Call("keys"))
	for index := 0; index < names.Length(); index += 1 {
		databaseName := names.Index(index).String()
		if !strings.HasPrefix(databaseName, "urnetwork-sidecar") {
			continue
		}
		suffix, prefixed := strings.CutPrefix(databaseName, indexedDbNamePrefix)
		if parsed, err := sidecar.ParseId(suffix); !prefixed || err != nil || parsed.String() != suffix {
			t.Errorf("a database named %s", databaseName)
		}
	}
}

// Two Workers of two networks open two databases, each its own instance with its own instance document and root
// key, and what one Worker's network writes, an enrollment's record of the network and a secret in its key store,
// changes nothing in the other's database or instance.
func TestTwoNetworksWorkersShareNothing(t *testing.T) {
	control := testFakeIndexedDb(t)
	prefix := testDatabaseNamePrefix(t)
	networkIds := []sidecar.Id{sidecar.NewId(), sidecar.NewId()}
	workers := []*pageWorker{}
	names := []string{}
	for _, networkId := range networkIds {
		worker := testPageWorkerUnder(t, prefix)
		testOpenNetwork(t, worker, networkId)
		workers = append(workers, worker)
		names = append(names, indexedDbNetworkName(prefix, networkId))
	}
	if testInstanceId(t, control, names[0]) == testInstanceId(t, control, names[1]) {
		t.Fatal("two networks' databases hold one instance")
	}
	for index, name := range names {
		if !webCryptoIsRootKey(control.Call("raw", name, indexedDbKeysStore, webCryptoRootKeyName)) {
			t.Errorf("network %d's database holds no root key", index)
		}
	}
	other := testDatabaseRecords(control, names[1])
	own := testDatabaseRecords(control, names[0])
	if err := workers[0].current().instance.RecordNetwork(testNetworkRecord(networkIds[0])); err != nil {
		t.Fatal(err)
	}
	database, err := openIndexedDb(names[0])
	if err != nil {
		t.Fatal(err)
	}
	if status := newWebCryptoKeyStore(database).PutSecret("another", []byte("another secret")); status != sidecar.SidecarKeyStoreOk {
		t.Fatal(status)
	}
	if testRecordsEqual(own, testDatabaseRecords(control, names[0])) {
		t.Fatal("the first network's writes did not reach its database")
	}
	if !testRecordsEqual(other, testDatabaseRecords(control, names[1])) {
		t.Error("the first network's writes changed the second network's database")
	}
	if networks := workers[1].current().instance.Networks(); len(networks) != 0 {
		t.Errorf("the second network's instance records %+v", networks)
	}
	if networks := workers[0].current().instance.Networks(); len(networks) != 1 || networks[0].NetworkId != networkIds[0] {
		t.Errorf("the first network's instance records %+v", networks)
	}
}

// A Worker reopened on a network finds that network's own state, each network in turn: the instance its database
// holds, opened by that database's own root key, with nothing made anew.
func TestReopenedWorkerFindsItsOwnNetwork(t *testing.T) {
	control := testFakeIndexedDb(t)
	prefix := testDatabaseNamePrefix(t)
	networkIds := []sidecar.Id{sidecar.NewId(), sidecar.NewId()}
	names := []string{}
	instanceIds := []string{}
	for _, networkId := range networkIds {
		worker := newPageWorker(context.Background())
		worker.databaseNamePrefix = prefix
		testOpenNetwork(t, worker, networkId)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := worker.close(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, indexedDbNetworkName(prefix, networkId))
		instanceIds = append(instanceIds, testInstanceId(t, control, names[len(names)-1]))
	}
	if instanceIds[0] == instanceIds[1] {
		t.Fatal("two networks' databases hold one instance")
	}
	// the second network first, so neither reopen finds the other's state by order
	for index := len(networkIds) - 1; 0 <= index; index -= 1 {
		before := testDatabaseRecords(control, names[index])
		worker := testPageWorkerUnder(t, prefix)
		status := testOpenNetwork(t, worker, networkIds[index])
		if status.NetworkId != networkIds[index].String() {
			t.Errorf("network %d reopened as %+v", index, status)
		}
		if state, _ := worker.current().instance.State(); state != sidecar.SidecarInstanceOpen {
			t.Errorf("network %d's reopened instance: %v", index, state)
		}
		if instanceId := testInstanceId(t, control, names[index]); instanceId != instanceIds[index] {
			t.Errorf("network %d reopened instance %s, was %s", index, instanceId, instanceIds[index])
		}
		if !testRecordsEqual(before, testDatabaseRecords(control, names[index])) {
			t.Errorf("network %d's reopen wrote its database anew", index)
		}
	}
}

// open's urls name every service, or none, which takes §18.9's table's.
func TestPageServiceUrlsWholeOrTable(t *testing.T) {
	settings := sidecar.DefaultSidecarSettings()
	if err := applyPageServiceUrls(settings, nil); err != nil || !strings.HasPrefix(settings.ApiUrl, "https://") {
		t.Fatalf("the table's urls: %q %v", settings.ApiUrl, err)
	}
	partial := testPageServiceUrls()
	partial.Gkm = ""
	if err := applyPageServiceUrls(sidecar.DefaultSidecarSettings(), partial); err == nil {
		t.Fatal("urls missing a service were taken")
	}
}

// The published calls answer through promises: handle's reply is a JSON string under the request's id, and open's
// rejection carries its fixed message.
func TestPublishedCallsAnswerThroughPromises(t *testing.T) {
	worker := testPageWorker(t)
	publishPageWorker(worker)
	published := js.Global().Get(sidecarWasmGlobalName)
	if published.Get("version").Type() != js.TypeString {
		t.Fatal("no version is published")
	}
	replyJson, err := awaitPromise(published.Call("handle", `{"id":"call-1","op":"enroll"}`))
	if err != nil || replyJson.Type() != js.TypeString {
		t.Fatalf("handle answered %v, %v", replyJson, err)
	}
	reply := &sidecarworker.Reply{}
	if err := json.Unmarshal([]byte(replyJson.String()), reply); err != nil || reply.Id != "call-1" || reply.Code != sidecarworker.CodeUnavailable {
		t.Fatalf("the reply %s: %v", replyJson.String(), err)
	}
	// a request that is not a string is answered, not thrown
	if replyJson, err := awaitPromise(published.Call("handle", 7)); err != nil || !strings.Contains(replyJson.String(), `"invalid"`) {
		t.Fatalf("a request that is not a string: %v %v", replyJson, err)
	}
	if _, err := awaitPromise(published.Call("open", `{"network_id":"nope"}`)); err == nil {
		t.Fatal("open took a network id that is not one")
	}
	config, _ := json.Marshal(&pageOpenConfig{NetworkId: sidecar.NewId().String(), Urls: testPageServiceUrls()})
	statusJson, err := awaitPromise(published.Call("open", string(config)))
	if err != nil {
		t.Fatal(err)
	}
	status := &pageStatusView{}
	if err := json.Unmarshal([]byte(statusJson.String()), status); err != nil || status.State == "" {
		t.Fatalf("open answered %s: %v", statusJson.String(), err)
	}
}

// A record log of many entries, verified as a reader would hold them.
type testPageRecords struct {
	entries []*sidecar.RecordReadEntry
}

func (self *testPageRecords) Sync(ctx context.Context, logId sidecar.Id) error {
	return nil
}

func (self *testPageRecords) Entries(logId sidecar.Id) []*sidecar.RecordReadEntry {
	return self.entries
}

// An enrolled sidecar for the Worker's seam.
type testPageSidecar struct{}

func (self testPageSidecar) Identity() *sidecar.SidecarIdentity {
	return &sidecar.SidecarIdentity{SidecarId: sidecar.NewId()}
}

func (self testPageSidecar) Status() sidecar.SidecarStatus {
	return sidecar.SidecarStatus{State: sidecar.SidecarStateOnline}
}

func (self testPageSidecar) Logout(ctx context.Context) error {
	return nil
}

// A repo the long operation never reads.
type testPageRepo struct{}

func (self testPageRepo) List(ctx context.Context, args *sidecar.EvsReadArgs) (*sidecar.EvsListing, error) {
	return nil, sidecar.ErrEvsPathAbsent
}

func (self testPageRepo) ReadHead(ctx context.Context, args *sidecar.EvsReadArgs, out io.Writer) (*sidecar.EvsFileInfo, error) {
	return nil, sidecar.ErrEvsPathAbsent
}

func (self testPageRepo) History(ctx context.Context, args *sidecar.EvsReadArgs) (*sidecar.EvsHistory, error) {
	return nil, sidecar.ErrEvsPathAbsent
}

// An operation over 50 ms on this leg hands the thread to the event loop between its steps, through
// yieldToEventLoop: a timer the loop runs fires while the operation is under way, as a poll's reply or another page's
// request would, and the operation still answers whole (§18.10's threads row).
func TestOperationYieldsToTheEventLoop(t *testing.T) {
	logId := sidecar.NewId()
	records := &testPageRecords{}
	for i := 0; i < 400; i++ {
		entryId := sidecar.NewId()
		records.entries = append(records.entries, &sidecar.RecordReadEntry{
			LogId:   logId,
			EntryId: entryId,
			Header:  &sidecar.StoredRecordHeader{Header: &protocol.RecordHeader{EntryId: entryId.Bytes(), Seq: uint64(i + 1), Kind: uint32(sidecar.RecordKindNote)}},
			State:   sidecar.RecordEntryLive,
		})
	}
	// a clock that moves 5 ms at each read, so the 400 entries take 2 s of it
	var clockLock sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time {
		clockLock.Lock()
		defer clockLock.Unlock()
		now = now.Add(5 * time.Millisecond)
		return now
	}
	settings := sidecarworker.DefaultWorkerSettings()
	settings.RecordMaxEntryCount = 1000
	worker, err := sidecarworker.NewWorker(&sidecarworker.WorkerArgs{
		Sidecar:  testPageSidecar{},
		Repo:     func(sidecar.Id) (sidecarworker.WorkerRepo, error) { return testPageRepo{}, nil },
		Records:  func() (sidecarworker.WorkerRecords, error) { return records, nil },
		Yield:    yieldToEventLoop,
		Now:      clock,
		Settings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	// a timer chain the event loop runs: it ticks only while Go hands the thread back, and its last timer is cleared
	// at the end, so nothing calls into Go once the test binary has exited
	var tickLock sync.Mutex
	ticks := 0
	stopped := false
	var pending js.Value
	var tick js.Func
	tick = js.FuncOf(func(this js.Value, args []js.Value) any {
		tickLock.Lock()
		defer tickLock.Unlock()
		if stopped {
			return nil
		}
		ticks += 1
		pending = js.Global().Call("setTimeout", tick, 0)
		return nil
	})
	defer tick.Release()
	func() {
		tickLock.Lock()
		defer tickLock.Unlock()
		pending = js.Global().Call("setTimeout", tick, 0)
	}()
	encoded := worker.HandleJson(context.Background(), []byte(`{"id":"long","op":"record_view","log_id":"`+logId.String()+`"}`))
	during := func() int {
		tickLock.Lock()
		defer tickLock.Unlock()
		stopped = true
		js.Global().Call("clearTimeout", pending)
		return ticks
	}()
	reply := &sidecarworker.Reply{}
	if err := json.Unmarshal(encoded, reply); err != nil || !reply.Ok || len(reply.Record.Entries) != 400 {
		t.Fatalf("the long operation: %s %v", encoded, err)
	}
	// 2 s of the clock in 50 ms intervals is about 40 yields, each a turn of the loop
	if during < 10 {
		t.Fatalf("the event loop ran %d times during a 2 s operation", during)
	}
}
