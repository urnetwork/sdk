//go:build !ios_extension && !js

package sdk

// Models() for the apps (sidecar_models.go; INFER-M11-09; DESIGN-COMPUTE §15.3, §6.5, CO-49): the catalog's JSON, the
// core's and the daemon's alike; §6.5's recommendation, or none and why; a pull's status told by chunks, in order, to
// every screen that asks, one joining late included, with one pull a model, the end told once and a corrupt chunk
// named; a remote pull polled, resumed at the first chunk missing. The local core is the sdk's test world with the
// test app's twin as its engine; a pull runs on a stand-in of the core's chunked download, whose own tests are the
// root's (infer_download_test.go, infer_pull_test.go). Synthetic values only.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// One call the models daemon took.
type testModelsDaemonCall struct {
	method    string
	confirmed bool
	params    json.RawMessage
}

// A daemon answering ur's model methods from scripts, one reply a call and the last one again, recording each call;
// models.set it takes only confirmed, as the daemon's Confirm method does, keeping its key unchanged otherwise.
type testModelsDaemon struct {
	stateLock sync.Mutex
	calls     []testModelsDaemonCall
	scripts   map[string][]any
	// the models key models.set changed, confirmed
	key *sidecar.InferModelsSettings
}

func newTestModelsDaemon(scripts map[string][]any) *testModelsDaemon {
	return &testModelsDaemon{scripts: scripts, key: &sidecar.InferModelsSettings{SignInMail: "withhold"}}
}

func (self *testModelsDaemon) sidecarId() *Id {
	return nil
}

func (self *testModelsDaemon) state() (string, string) {
	return "online", ""
}

func (self *testModelsDaemon) generation() int64 {
	return 1
}

func (self *testModelsDaemon) refresh(ctx context.Context) error {
	return nil
}

func (self *testModelsDaemon) pair(ctx context.Context, childJwt string) (*sidecarControlPairReply, error) {
	return nil, errors.New("this daemon pairs nothing")
}

func (self *testModelsDaemon) network() string {
	return "the paired network"
}

func (self *testModelsDaemon) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	paramsJson, err := json.Marshal(params)
	if err != nil {
		return err
	}
	self.stateLock.Lock()
	self.calls = append(self.calls, testModelsDaemonCall{method: method, confirmed: confirmed, params: paramsJson})
	var reply any
	if method == sidecarControlMethodModelsSet {
		if !confirmed {
			self.stateLock.Unlock()
			return &sidecarControlError{Code: -32001, Message: "models.set is a person's act, confirmed on /dev/tty or in an app"}
		}
		change := &sidecar.InferModelsChange{}
		if err := json.Unmarshal(paramsJson, change); err == nil && change.TeeOnly != nil {
			self.key.TeeOnly = *change.TeeOnly
		}
		reply = self.key
	} else {
		script, ok := self.scripts[method]
		if !ok || len(script) == 0 {
			self.stateLock.Unlock()
			return &sidecarControlError{Code: sidecarControlCodeMethodNotFound, Message: "method not found"}
		}
		reply = script[0]
		if 1 < len(script) {
			self.scripts[method] = script[1:]
		}
	}
	self.stateLock.Unlock()
	if failure, ok := reply.(error); ok {
		return failure
	}
	replyJson, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	return json.Unmarshal(replyJson, result)
}

func (self *testModelsDaemon) close() {
}

// The calls of method taken so far.
func (self *testModelsDaemon) taken(method string) []testModelsDaemonCall {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	calls := []testModelsDaemonCall{}
	for _, call := range self.calls {
		if call.method == method {
			calls = append(calls, call)
		}
	}
	return calls
}

// Models() on the daemon, polling at once.
func testModelsRemote(t *testing.T, daemon *testModelsDaemon) *SidecarModels {
	t.Helper()
	models := newSidecarModels(&sidecarComputeArgs{remote: daemon})
	models.settings.PullPollInterval = time.Millisecond
	t.Cleanup(models.close)
	return models
}

// A listing as the daemon writes it: one local model this device may pull, recommended, and a TEE model.
func testModelsListing() *sidecar.InferModelsListing {
	return &sidecar.InferModelsListing{
		Entries: []*sidecar.InferModelsEntry{
			{
				Name: "local-gemma-4-e2b-off", Names: []string{"local-gemma-4-e2b-off"}, Provider: "local",
				Kind: sidecar.InferCatalogKindLocal, Label: sidecar.ComputeLabelLocal, Efforts: []sidecar.InferEffort{sidecar.InferEffortOff},
				DefaultEffort: sidecar.InferEffortOff, ContextWindow: 32 * 1024, MaxOutput: 8 * 1024,
				Modalities: []sidecar.InferModality{sidecar.InferModalityText}, DisclosureId: "inference:local", Signed: true,
				Local: &sidecar.InferModelsLocal{File: "gemma-4-e2b.litertlm", ByteCount: 1 << 30, State: sidecar.InferPullFileAbsent, MinMemoryByteCount: 4 << 30},
			},
			{
				Name: "chutes-qwen3.8-27b-tee-high", Names: []string{"chutes-qwen3.8-27b-tee-high"}, Alias: "chutes-qwen3.8-27b-tee",
				Provider: "chutes", Kind: sidecar.InferCatalogKindTee, Label: sidecar.ComputeLabelAp,
				Efforts: []sidecar.InferEffort{sidecar.InferEffortHigh}, DefaultEffort: sidecar.InferEffortHigh, ContextWindow: 128 * 1024,
				Modalities: []sidecar.InferModality{sidecar.InferModalityText}, DisclosureId: "inference:chutes",
				DisclosureLine: "Chutes runs this model in a TEE it attests.", Signed: true,
				Remote: &sidecar.InferRemoteModel{ProviderModelId: "qwen3.8-27b", InputNanoUsdPerMillion: 45_000_000, OutputNanoUsdPerMillion: 300_000_000},
			},
		},
		Recommended: "local-gemma-4-e2b-off",
	}
}

// The catalog's JSON is the daemon's on the remote backend, as it wrote it, and the core's listing on the local one, as
// the daemon would write it; a Sidecar with no core answers an error.
func TestModelsCatalogJson(t *testing.T) {
	listing := testModelsListing()
	daemon := newTestModelsDaemon(map[string][]any{sidecarControlMethodModelsList: {listing}})
	catalogJson, err := testModelsRemote(t, daemon).Catalog()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(listing)
	if catalogJson != string(want) {
		t.Errorf("the remote catalog %s, the daemon's %s", catalogJson, want)
	}
	read := &sidecar.InferModelsListing{}
	if err := json.Unmarshal([]byte(catalogJson), read); err != nil || len(read.Entries) != 2 || read.Entries[1].Remote.InputNanoUsdPerMillion != 45_000_000 {
		t.Errorf("the catalog reads %+v, %v", read, err)
	}

	// the local core's, with the app's twin a phone
	local := newTestSidecarWorld(t).open(t)
	local.SetLocalInferenceHost(&testAppInferenceHost{})
	catalogJson, err = local.Models().Catalog()
	if err != nil {
		t.Fatal(err)
	}
	pull, _ := local.Core().InferPull()
	core, err := local.Core().InferModels(context.Background(), nil, pull)
	if err != nil {
		t.Fatal(err)
	}
	want, _ = json.Marshal(core)
	if catalogJson != string(want) {
		t.Errorf("the local catalog %s, the core's %s", catalogJson, want)
	}
	// this build's manifest may list no local model yet; the listing then says so, as the core's does
	localCount := 0
	for _, entry := range core.Entries {
		if entry.Kind == sidecar.InferCatalogKindLocal {
			localCount += 1
			if entry.Local == nil || entry.Local.State != sidecar.InferPullFileAbsent || entry.Label != sidecar.ComputeLabelLocal {
				t.Errorf("a local entry before any pull: %+v", entry)
			}
		}
	}
	if localCount == 0 && core.LocalUnavailable == "" {
		t.Errorf("the phone's catalog lists no local model and says not why: %s", catalogJson)
	}

	if _, err := newSidecarModels(&sidecarComputeArgs{}).Catalog(); !errors.Is(err, errSidecarModelsNoCore) {
		t.Errorf("no core answered %v", err)
	}
}

// §6.5's default for the device the app's twin reports, with its entry; none, and why, without an engine; the
// daemon's recommendation from its listing, and none fitting said so where its listing gives no reason.
func TestModelsRecommended(t *testing.T) {
	local := newTestSidecarWorld(t).open(t)
	local.SetLocalInferenceHost(&testAppInferenceHost{})
	recommendedJson, err := local.Models().Recommended()
	if err != nil {
		t.Fatal(err)
	}
	router, err := local.Core().InferRouter()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := router.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantName, _ := catalog.Recommended()
	read := &sidecarModelsRecommendation{}
	if err := json.Unmarshal([]byte(recommendedJson), read); err != nil {
		t.Fatal(err)
	}
	switch {
	case wantName != "":
		if read.Name != wantName || read.Model == nil || read.Model.Kind != sidecar.InferCatalogKindLocal ||
			!slices.Contains(read.Model.Names, wantName) || read.Reason != "" {
			t.Errorf("the phone's recommendation %s, the catalog's %q", recommendedJson, wantName)
		}
	default:
		// this build's manifest lists no local model yet: none, with the catalog's reason
		if read.Name != "" || read.Model != nil || read.Reason != catalog.LocalUnavailable() || read.Reason == "" {
			t.Errorf("the phone's recommendation %s, the catalog's none: %q", recommendedJson, catalog.LocalUnavailable())
		}
	}

	// no engine: none, and the catalog's reason
	local.SetLocalInferenceHost(nil)
	recommendedJson, err = local.Models().Recommended()
	if err != nil {
		t.Fatal(err)
	}
	read = &sidecarModelsRecommendation{}
	if err := json.Unmarshal([]byte(recommendedJson), read); err != nil || read.Name != "" || read.Model != nil || read.Reason == "" {
		t.Errorf("no engine recommends %s", recommendedJson)
	}

	// the daemon's
	listing := testModelsListing()
	daemon := newTestModelsDaemon(map[string][]any{sidecarControlMethodModelsList: {listing}})
	recommendedJson, err = testModelsRemote(t, daemon).Recommended()
	if err != nil {
		t.Fatal(err)
	}
	read = &sidecarModelsRecommendation{}
	if err := json.Unmarshal([]byte(recommendedJson), read); err != nil || read.Name != "local-gemma-4-e2b-off" || read.Model.Local.ByteCount != 1<<30 {
		t.Errorf("the daemon's recommendation %s", recommendedJson)
	}
	listing.Recommended = ""
	daemon = newTestModelsDaemon(map[string][]any{sidecarControlMethodModelsList: {listing}})
	recommendedJson, err = testModelsRemote(t, daemon).Recommended()
	read = &sidecarModelsRecommendation{}
	if err != nil || json.Unmarshal([]byte(recommendedJson), read) != nil || read.Name != "" || read.Reason != sidecarModelsNoneFits {
		t.Errorf("none fitting: %s, %v", recommendedJson, err)
	}
}

// A pull listener recording each status, sending each running status's chunks done on chunks while there is room, and
// closing ended at the first end.
type testModelsPullListener struct {
	stateLock sync.Mutex
	statuses  []*sidecar.InferPullStatus
	chunks    chan int
	ended     chan struct{}
}

func newTestModelsPullListener() *testModelsPullListener {
	return &testModelsPullListener{chunks: make(chan int, 64), ended: make(chan struct{})}
}

// Waits until the listener hears chunk done.
func (self *testModelsPullListener) waitChunk(t *testing.T, done int) {
	t.Helper()
	for {
		select {
		case heard := <-self.chunks:
			if heard == done {
				return
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("chunk %d was never heard", done)
		}
	}
}

func (self *testModelsPullListener) OnPull(statusJson string) {
	status := &sidecar.InferPullStatus{}
	if json.Unmarshal([]byte(statusJson), status) != nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.statuses = append(self.statuses, status)
	if !status.Ended() && status.Progress != nil {
		select {
		case self.chunks <- status.Progress.DoneChunkCount:
		default:
		}
	}
	if status.Ended() && len(self.endsWithLock()) == 1 {
		close(self.ended)
	}
}

func (self *testModelsPullListener) endsWithLock() []*sidecar.InferPullStatus {
	ends := []*sidecar.InferPullStatus{}
	for _, status := range self.statuses {
		if status.Ended() {
			ends = append(ends, status)
		}
	}
	return ends
}

// The statuses heard, waiting for the first end.
func (self *testModelsPullListener) heard(t *testing.T) []*sidecar.InferPullStatus {
	t.Helper()
	select {
	case <-self.ended:
	case <-time.After(30 * time.Second):
		t.Fatal("the pull never ended")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.statuses)
}

// The chunks done of a listener's running statuses, in the order heard.
func testModelsChunksDone(statuses []*sidecar.InferPullStatus) []int {
	done := []int{}
	for _, status := range statuses {
		if !status.Ended() && status.Progress != nil {
			done = append(done, status.Progress.DoneChunkCount)
		}
	}
	return done
}

// A stand-in of the core's chunked download: it reports chunks done one by one, waiting at chunk hold for release, and
// ends with end; it counts its runs.
type testModelsPull struct {
	stateLock  sync.Mutex
	runs       int
	chunkCount int
	hold       int
	release    chan struct{}
	held       chan struct{}
	end        error
}

func (self *testModelsPull) pull(ctx context.Context, name string, listener func(*sidecar.InferPullProgress)) error {
	self.stateLock.Lock()
	self.runs += 1
	self.stateLock.Unlock()
	for done := 0; done <= self.chunkCount; done += 1 {
		listener(&sidecar.InferPullProgress{
			Name: name, Phase: sidecar.InferPullPhaseModel, File: "gemma-4-e2b.litertlm", ChunkCount: self.chunkCount,
			DoneChunkCount: done, ByteCount: int64(self.chunkCount) * 4096, DoneByteCount: int64(done) * 4096,
			Source: "api", FileIndex: 1, FileCount: 1,
		})
		if done == self.hold && self.release != nil {
			close(self.held)
			select {
			case <-self.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if self.end != nil && done == self.hold {
			return self.end
		}
	}
	return nil
}

// The stand-in's runs so far.
func (self *testModelsPull) runCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.runs
}

// Models() on the test world's core, its pulls on stand-in.
func testModelsLocal(t *testing.T, stand *testModelsPull) *SidecarModels {
	t.Helper()
	local := newTestSidecarWorld(t).open(t)
	models := newSidecarModels(&sidecarComputeArgs{ctx: context.Background(), local: local.Core()})
	models.localPull = func() (sidecar.InferPullFunc, error) {
		return stand.pull, nil
	}
	t.Cleanup(models.close)
	return models
}

// A models listener counting its calls.
type testModelsListener struct {
	stateLock sync.Mutex
	count     int
}

func (self *testModelsListener) ModelsChanged() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.count += 1
}

func (self *testModelsListener) changes() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.count
}

// A local pull tells each screen its status by chunks done, in order, the newest at each telling: a screen joining
// while it runs hears where it is first, then its news; both hear its end, with every chunk done, once; one pull ran
// for both; Pulls keeps the end, and the models' listener heard each change.
func TestModelsPullProgress(t *testing.T) {
	stand := &testModelsPull{chunkCount: 4, hold: 2, release: make(chan struct{}), held: make(chan struct{})}
	models := testModelsLocal(t, stand)
	changes := &testModelsListener{}
	sub := models.AddListener(changes)
	defer sub.Close()
	first := newTestModelsPullListener()
	if err := models.Pull("local-gemma-4-e2b-off", first); err != nil {
		t.Fatal(err)
	}
	<-stand.held
	first.waitChunk(t, 2)
	late := newTestModelsPullListener()
	if err := models.Pull("local-gemma-4-e2b-off", late); err != nil {
		t.Fatal(err)
	}
	close(stand.release)
	firstHeard, lateHeard := first.heard(t), late.heard(t)
	if done := testModelsChunksDone(firstHeard); !slices.IsSorted(done) || !slices.Contains(done, 2) {
		t.Errorf("the first screen heard chunks %v", done)
	}
	if done := testModelsChunksDone(lateHeard); len(done) == 0 || !slices.IsSorted(done) || done[0] != 2 {
		t.Errorf("the late screen heard chunks %v", done)
	}
	for _, heard := range [][]*sidecar.InferPullStatus{firstHeard, lateHeard} {
		end := heard[len(heard)-1]
		ends := slices.DeleteFunc(slices.Clone(heard), func(status *sidecar.InferPullStatus) bool { return !status.Ended() })
		if end.State != sidecar.InferPullDone || end.Error != nil || end.Name != "local-gemma-4-e2b-off" || len(ends) != 1 ||
			end.Progress == nil || end.Progress.DoneChunkCount != 4 {
			t.Errorf("a screen's end %+v, of %d ends", end, len(ends))
		}
	}
	if runs := stand.runCount(); runs != 1 {
		t.Errorf("%d pulls ran for two screens", runs)
	}
	pulls := &sidecarModelsPulls{}
	if err := json.Unmarshal([]byte(models.Pulls()), pulls); err != nil || len(pulls.Pulls) != 1 || pulls.Pulls[0].State != sidecar.InferPullDone {
		t.Errorf("Pulls answered %s", models.Pulls())
	}
	if changes.changes() < 2 {
		t.Errorf("the models' listener heard %d changes", changes.changes())
	}
	if err := models.Pull("", first); !errors.Is(err, errSidecarModelsRequest) {
		t.Errorf("a pull of no name answered %v", err)
	}
}

// CO-49's progress by chunks as the app sees it: a remote pull polled, each change told once, a chunk failing its check
// ending it download_corrupt naming the chunk; asked again, it resumes at the first chunk missing and ends done. A
// local pull's corrupt chunk is named the same way.
func TestModelsPullProgressByChunks(t *testing.T) {
	running := func(done int) *sidecar.InferPullStatus {
		return &sidecar.InferPullStatus{Name: "local-gemma-4-e2b-off", State: sidecar.InferPullRunning, Progress: &sidecar.InferPullProgress{
			Name: "local-gemma-4-e2b-off", Phase: sidecar.InferPullPhaseModel, File: "gemma-4-e2b.litertlm", ChunkCount: 4,
			DoneChunkCount: done, ByteCount: 4 * 4096, DoneByteCount: int64(done) * 4096, Source: "api", FileIndex: 1, FileCount: 1,
		}}
	}
	corrupt := 3
	failed := running(2)
	failed.State = sidecar.InferPullFailed
	failed.Error = &sidecar.InferPullError{Code: string(sidecar.InferDownloadErrorCorrupt), Chunk: &corrupt, Message: "chunk 3 failed its check twice"}
	done := running(4)
	done.State = sidecar.InferPullDone
	daemon := newTestModelsDaemon(map[string][]any{sidecarControlMethodModelsPull: {
		running(0), running(1), running(1), running(2), failed,
		// the second pull: the chunks done are kept, so it starts at the first missing
		running(2), running(3), done,
	}})
	models := testModelsRemote(t, daemon)
	listener := newTestModelsPullListener()
	if err := models.Pull("local-gemma-4-e2b-off", listener); err != nil {
		t.Fatal(err)
	}
	heard := listener.heard(t)
	if chunks := testModelsChunksDone(heard); !slices.Equal(chunks, []int{0, 1, 2}) {
		t.Errorf("the screen heard chunks %v", chunks)
	}
	end := heard[len(heard)-1]
	if end.State != sidecar.InferPullFailed || end.Error == nil || end.Error.Code != string(sidecar.InferDownloadErrorCorrupt) ||
		end.Error.Chunk == nil || *end.Error.Chunk != 3 {
		t.Fatalf("the corrupt chunk ended %+v", end)
	}
	for _, call := range daemon.taken(sidecarControlMethodModelsPull) {
		params := &sidecarControlModelsNameParams{}
		if json.Unmarshal(call.params, params) != nil || params.Name != "local-gemma-4-e2b-off" || params.Network != "the paired network" || call.confirmed {
			t.Errorf("a poll sent %s, confirmed %v", call.params, call.confirmed)
		}
	}

	again := newTestModelsPullListener()
	if err := models.Pull("local-gemma-4-e2b-off", again); err != nil {
		t.Fatal(err)
	}
	heard = again.heard(t)
	if chunks := testModelsChunksDone(heard); len(chunks) == 0 || chunks[0] != 2 {
		t.Errorf("the second pull did not resume at the first missing chunk: %v", chunks)
	}
	if end := heard[len(heard)-1]; end.State != sidecar.InferPullDone || end.Error != nil {
		t.Errorf("the second pull ended %+v", end)
	}

	// a local pull's corrupt chunk, as the core's download names it
	stand := &testModelsPull{chunkCount: 4, hold: 1, end: &sidecar.InferDownloadError{Code: sidecar.InferDownloadErrorCorrupt, Chunk: 1, Message: "chunk 1 failed its check"}}
	local := newTestModelsPullListener()
	if err := testModelsLocal(t, stand).Pull("local-gemma-4-e2b-off", local); err != nil {
		t.Fatal(err)
	}
	heard = local.heard(t)
	if end := heard[len(heard)-1]; end.State != sidecar.InferPullFailed || end.Error.Code != string(sidecar.InferDownloadErrorCorrupt) ||
		end.Error.Chunk == nil || *end.Error.Chunk != 1 {
		t.Errorf("the local corrupt chunk ended %+v", end)
	}
}

// A refusal the daemon answers ends a remote pull at once, told as failed with the daemon's words; Remove is refused
// while this Models() pulls the model, and once it ended goes to the daemon and forgets the pull's end.
func TestModelsPullRefusedAndRemove(t *testing.T) {
	daemon := newTestModelsDaemon(map[string][]any{
		sidecarControlMethodModelsPull:   {&sidecarControlError{Code: -32000, Message: "this daemon pulls no model: its state is not on a disk"}},
		sidecarControlMethodModelsRemove: {struct{}{}},
	})
	models := testModelsRemote(t, daemon)
	listener := newTestModelsPullListener()
	if err := models.Pull("local-gemma-4-e2b-off", listener); err != nil {
		t.Fatal(err)
	}
	heard := listener.heard(t)
	if end := heard[len(heard)-1]; end.State != sidecar.InferPullFailed || end.Error.Code != sidecarModelsPullFailedCode ||
		end.Error.Message != "this daemon pulls no model: its state is not on a disk" {
		t.Errorf("the refusal ended %+v", end)
	}
	if calls := daemon.taken(sidecarControlMethodModelsPull); len(calls) != 1 {
		t.Errorf("a refused pull was polled %d times", len(calls))
	}
	if err := models.Remove("local-gemma-4-e2b-off"); err != nil {
		t.Fatal(err)
	}
	if pulls := models.Pulls(); pulls != `{"pulls":[]}` {
		t.Errorf("Remove kept %s", pulls)
	}

	stand := &testModelsPull{chunkCount: 2, hold: 1, release: make(chan struct{}), held: make(chan struct{})}
	local := testModelsLocal(t, stand)
	running := newTestModelsPullListener()
	if err := local.Pull("local-gemma-4-e2b-off", running); err != nil {
		t.Fatal(err)
	}
	<-stand.held
	if err := local.Remove("local-gemma-4-e2b-off"); !errors.Is(err, errSidecarModelsPullRunning) {
		t.Errorf("Remove during the pull answered %v", err)
	}
	close(stand.release)
	running.heard(t)
}
