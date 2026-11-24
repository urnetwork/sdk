//go:build !ios_extension && !js

package sdk

// §18.5's events: every kind the core declares maps to a bound event carrying each field the core's carries, and each
// field of the core's event, set alone, reaches the bound event; an older generation's event is dropped, a listener's
// panic costs only its own call, a full queue drops and counts without waiting, a core event reaches an app listener
// through a real local Sidecar, and the push's rollback head_alarm reaches one with its kind and both TopicVersions.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
)

// Every SidecarEventKind constant the sidecar module's root declares, by its value, read from the source, so a kind
// another track adds is in the list the day it lands.
func testSidecarEventKinds(t *testing.T) []string {
	t.Helper()
	command := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/urnetwork/sidecar")
	command.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("finding the sidecar module: %v", err)
	}
	directory := strings.TrimSpace(string(output))
	files, err := filepath.Glob(filepath.Join(directory, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	fileSet := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			general, ok := decl.(*ast.GenDecl)
			if !ok || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				value := spec.(*ast.ValueSpec)
				typeName, ok := value.Type.(*ast.Ident)
				if !ok || typeName.Name != "SidecarEventKind" {
					continue
				}
				for _, literal := range value.Values {
					basic, ok := literal.(*ast.BasicLit)
					if !ok || basic.Kind != token.STRING {
						continue
					}
					kind, err := strconv.Unquote(basic.Value)
					if err != nil {
						t.Fatal(err)
					}
					kinds = append(kinds, kind)
				}
			}
		}
	}
	slices.Sort(kinds)
	return kinds
}

// A kind no core declares, as a later component's would be: the binding passes it along as it is.
const testUndeclaredEventKind = "synthetic_later_alarm"

// A core event of kind with every field set to something other than its zero.
func testFullSidecarEvent(kind string) *sidecar.SidecarEvent {
	at := time.UnixMilli(1_700_000_000_123)
	id := func(seed byte) *sidecar.Id {
		var value sidecar.Id
		for i := range value {
			value[i] = seed + byte(i)
		}
		return &value
	}
	return &sidecar.SidecarEvent{
		Kind:          sidecar.SidecarEventKind(kind),
		Generation:    7,
		State:         sidecar.SidecarStateOnline,
		Reason:        sidecar.SidecarReasonRefused,
		GroupId:       id(0x10),
		SidecarId:     id(0x20),
		RepoId:        id(0x30),
		RefId:         id(0x40),
		Path:          "synthetic/path",
		DropId:        id(0x50),
		DropState:     sidecar.SidecarDropPending,
		NotBefore:     at,
		SearchAlarm:   sidecar.SidecarSearchAlarmOmission,
		RequestId:     id(0x60),
		Outcome:       "approved",
		SendId:        id(0x70),
		ApprovalId:    id(0x80),
		HeadAlarm:     sidecar.SidecarHeadAlarmWithheld,
		Evidence:      [2][]byte{[]byte("own pin"), []byte("peer pin")},
		Erasure:       &sidecar.SidecarErasure{ErasureId: *id(0x90), State: "waiting", ObjectsLeft: 3, BytesLeft: 4096, NextTry: at.Add(time.Second), Finish: at.Add(time.Hour)},
		EvsAlarm:      sidecar.SidecarEvsAlarmTamper,
		VerifiedSince: at.Add(time.Minute),
		RecordAlarm:   sidecar.SidecarRecordAlarmChainGap,
		BridgeAlarm:   sidecar.SidecarBridgeAlarmStateUnverified,
		DnsAlarm:      sidecar.SidecarDnsAlarmZoneMismatch,
		// evs_alarm's prefix_rule detail and ct_alarm (DESIGN-DETAILS 16.2.10, D-TLS-8)
		EvsAlarmDetail: sidecar.SidecarEvsAlarmDetailSetVoid,
		Version:        17,
		PushId:         id(0xa0),
		Count:          3,
		CtAlarm:        sidecar.SidecarCtAlarmRecentIdentity,
		// mail_receiver_alarm's reason (D-MAIL-15, MAIL-M7-25)
		MailReceiverAlarm: sidecar.SidecarMailReceiverAlarmScope,
		// upgrade_required's features (DESIGN-COMPUTE §16.6's hold; CACL-M11-02)
		AuthFeatures: sidecar.AuthFeatureAgentRepos | sidecar.AuthFeatureListeners,
	}
}

// Every kind the core declares, and one it does not, maps to a bound event carrying each field the core's event
// carries, as the binding's types; and the core's event has no field the binding does not map.
func TestEveryEventKindMaps(t *testing.T) {
	mapped := map[string]bool{
		"Kind": true, "Generation": true, "State": true, "Reason": true, "GroupId": true, "SidecarId": true,
		"RepoId": true, "RefId": true, "Path": true, "DropId": true, "DropState": true, "NotBefore": true,
		"SearchAlarm": true, "RequestId": true, "Outcome": true, "SendId": true, "ApprovalId": true, "HeadAlarm": true,
		"Evidence": true, "Erasure": true, "EvsAlarm": true, "VerifiedSince": true, "RecordAlarm": true, "BridgeAlarm": true,
		"DnsAlarm": true, "EvsAlarmDetail": true, "Version": true, "PushId": true, "Count": true, "CtAlarm": true,
		"MailReceiverAlarm": true, "AuthFeatures": true,
	}
	eventType := reflect.TypeFor[sidecar.SidecarEvent]()
	for i := 0; i < eventType.NumField(); i += 1 {
		if !mapped[eventType.Field(i).Name] {
			t.Errorf("the core's event has a field %s the binding does not map", eventType.Field(i).Name)
		}
	}
	erasureType := reflect.TypeFor[sidecar.SidecarErasure]()
	for i := 0; i < erasureType.NumField(); i += 1 {
		switch erasureType.Field(i).Name {
		case "ErasureId", "State", "ObjectsLeft", "BytesLeft", "NextTry", "Finish":
		default:
			t.Errorf("the core's erasure has a field %s the binding does not map", erasureType.Field(i).Name)
		}
	}

	kinds := testSidecarEventKinds(t)
	if len(kinds) < 25 || !slices.Contains(kinds, "state") || !slices.Contains(kinds, "head_alarm") {
		t.Fatalf("CONTROL FAILED: the core declares %d kinds: %v", len(kinds), kinds)
	}
	// and one no core declares: dns_alarm stood here until the core declared it
	if slices.Contains(kinds, testUndeclaredEventKind) {
		t.Fatalf("CONTROL FAILED: the core declares %s", testUndeclaredEventKind)
	}
	kinds = append(kinds, testUndeclaredEventKind)
	sameId := func(bound *Id, core *sidecar.Id) bool {
		return bound != nil && core != nil && bytes.Equal(bound.Bytes(), core.Bytes())
	}
	for _, kind := range kinds {
		core := testFullSidecarEvent(kind)
		bound := newSidecarEventOf(core)
		if bound.Kind != kind || bound.Generation != core.Generation || bound.State != string(core.State) ||
			bound.Reason != string(core.Reason) || bound.Path != core.Path || bound.DropState != string(core.DropState) ||
			bound.SearchAlarm != string(core.SearchAlarm) || bound.Outcome != core.Outcome ||
			bound.HeadAlarm != string(core.HeadAlarm) || bound.EvsAlarm != string(core.EvsAlarm) ||
			bound.RecordAlarm != string(core.RecordAlarm) || bound.BridgeAlarm != string(core.BridgeAlarm) ||
			bound.DnsAlarm != string(core.DnsAlarm) || bound.EvsAlarmDetail != string(core.EvsAlarmDetail) ||
			bound.CtAlarm != string(core.CtAlarm) || bound.Version != int64(core.Version) || bound.Count != int64(core.Count) ||
			!sameId(bound.PushId, core.PushId) || bound.MailReceiverAlarm != string(core.MailReceiverAlarm) ||
			bound.AuthFeatures != core.AuthFeatures.String() || bound.AuthFeatures == "" {
			t.Errorf("%s: the strings mapped as %+v", kind, bound)
		}
		if !sameId(bound.GroupId, core.GroupId) || !sameId(bound.SidecarId, core.SidecarId) || !sameId(bound.RepoId, core.RepoId) ||
			!sameId(bound.RefId, core.RefId) || !sameId(bound.DropId, core.DropId) || !sameId(bound.RequestId, core.RequestId) ||
			!sameId(bound.SendId, core.SendId) || !sameId(bound.ApprovalId, core.ApprovalId) {
			t.Errorf("%s: an id mapped wrong", kind)
		}
		if bound.NotBeforeMillis != core.NotBefore.UnixMilli() || bound.VerifiedSinceMillis != core.VerifiedSince.UnixMilli() {
			t.Errorf("%s: times %d, %d", kind, bound.NotBeforeMillis, bound.VerifiedSinceMillis)
		}
		if !bytes.Equal(bound.FirstEvidence, core.Evidence[0]) || !bytes.Equal(bound.SecondEvidence, core.Evidence[1]) {
			t.Errorf("%s: evidence %q, %q", kind, bound.FirstEvidence, bound.SecondEvidence)
		}
		erasure := bound.Erasure
		if erasure == nil || !sameId(erasure.ErasureId, &core.Erasure.ErasureId) || erasure.State != core.Erasure.State ||
			erasure.ObjectsLeft != core.Erasure.ObjectsLeft || erasure.BytesLeft != core.Erasure.BytesLeft ||
			erasure.NextTryMillis != core.Erasure.NextTry.UnixMilli() || erasure.FinishMillis != core.Erasure.Finish.UnixMilli() {
			t.Errorf("%s: erasure %+v", kind, erasure)
		}
		// the bound event shares no bytes with the core's
		core.Evidence[0][0] ^= 0xff
		if bound.FirstEvidence[0] == core.Evidence[0][0] {
			t.Errorf("%s: the evidence is shared with the core's event", kind)
		}
	}

	// unset fields stay unset: no time reads as 1970 and no id appears from nothing
	empty := newSidecarEventOf(&sidecar.SidecarEvent{Kind: sidecar.SidecarEventState, Generation: 1})
	if empty.NotBeforeMillis != 0 || empty.VerifiedSinceMillis != 0 || empty.RepoId != nil || empty.Erasure != nil ||
		empty.FirstEvidence != nil || empty.MailReceiverAlarm != "" {
		t.Errorf("an empty event mapped as %+v", empty)
	}
}

// Sets field, of the core's event or a struct it points to, to a value other than its zero, as the core's own types
// hold one; false for a type with none here.
func testSetSyntheticEventField(field reflect.Value, name string) bool {
	switch {
	case field.Kind() == reflect.String:
		field.SetString("synthetic-" + strings.ToLower(name))
	case field.CanInt():
		field.SetInt(7)
	case field.CanUint():
		field.SetUint(7)
	case field.Type() == reflect.TypeFor[time.Time]():
		field.Set(reflect.ValueOf(time.UnixMilli(1_700_000_000_123)))
	case field.Type() == reflect.TypeFor[sidecar.Id]():
		field.Set(reflect.ValueOf(sidecar.NewId()))
	case field.Type() == reflect.TypeFor[*sidecar.Id]():
		id := sidecar.NewId()
		field.Set(reflect.ValueOf(&id))
	case field.Type() == reflect.TypeFor[[]byte]():
		field.SetBytes([]byte{1, 2, 3})
	default:
		return false
	}
	return true
}

// Calls check once for each leaf of the core's event with an event whose only value is at that leaf, and the event
// that leaf was set on: each field; each element of an array, such as Evidence; and for a pointer to a struct, such
// as Erasure, the struct left zero and then each of its fields set on it. A field of a type with no synthetic value
// fails the test, so a field of a new type is named here before anything passes over it.
func testEachCoreEventLeaf(t *testing.T, check func(leaf string, event *sidecar.SidecarEvent, base *sidecar.SidecarEvent)) {
	t.Helper()
	eventType := reflect.TypeFor[sidecar.SidecarEvent]()
	for i := 0; i < eventType.NumField(); i += 1 {
		name := eventType.Field(i).Name
		fieldType := eventType.Field(i).Type
		switch {
		case fieldType.Kind() == reflect.Array && fieldType.Elem().Kind() == reflect.Slice:
			for k := 0; k < fieldType.Len(); k += 1 {
				event := &sidecar.SidecarEvent{}
				if !testSetSyntheticEventField(reflect.ValueOf(event).Elem().Field(i).Index(k), name) {
					t.Fatalf("SidecarEvent.%s's element type %s has no synthetic value", name, fieldType.Elem())
				}
				check(name+"["+strconv.Itoa(k)+"]", event, &sidecar.SidecarEvent{})
			}
		case fieldType.Kind() == reflect.Pointer && fieldType.Elem().Kind() == reflect.Struct:
			structType := fieldType.Elem()
			withZeroStruct := func() *sidecar.SidecarEvent {
				event := &sidecar.SidecarEvent{}
				reflect.ValueOf(event).Elem().Field(i).Set(reflect.New(structType))
				return event
			}
			check(name, withZeroStruct(), &sidecar.SidecarEvent{})
			for j := 0; j < structType.NumField(); j += 1 {
				event := withZeroStruct()
				inner := reflect.ValueOf(event).Elem().Field(i).Elem().Field(j)
				if !testSetSyntheticEventField(inner, structType.Field(j).Name) {
					t.Fatalf("SidecarEvent.%s.%s's type %s has no synthetic value", name, structType.Field(j).Name, inner.Type())
				}
				check(name+"."+structType.Field(j).Name, event, withZeroStruct())
			}
		default:
			event := &sidecar.SidecarEvent{}
			if !testSetSyntheticEventField(reflect.ValueOf(event).Elem().Field(i), name) {
				t.Fatalf("SidecarEvent.%s's type %s has no synthetic value", name, fieldType)
			}
			check(name, event, &sidecar.SidecarEvent{})
		}
	}
}

// Each leaf of the core's event, set alone, changes the bound event an app gets: no field the core fills is dropped
// on the way, whatever TestEveryEventKindMaps's list of names says, and a field the core adds fails here the day it
// lands until the binding carries it.
func TestEveryCoreEventFieldReachesTheBoundEvent(t *testing.T) {
	leaves := []string{}
	testEachCoreEventLeaf(t, func(leaf string, event *sidecar.SidecarEvent, base *sidecar.SidecarEvent) {
		leaves = append(leaves, leaf)
		if reflect.DeepEqual(newSidecarEventOf(event), newSidecarEventOf(base)) {
			t.Errorf("the core's event's %s reaches nothing of the bound event", leaf)
		}
	})
	for _, leaf := range []string{"MailReceiverAlarm", "CtAlarm", "Evidence[1]", "Erasure.Finish"} {
		if !slices.Contains(leaves, leaf) {
			t.Errorf("CONTROL FAILED: no leaf %s among %v", leaf, leaves)
		}
	}
}

// A listener that records the events it is given, and signals each on a channel.
type testRecordingListener struct {
	stateLock sync.Mutex
	events    []*SidecarEvent
	received  chan *SidecarEvent
}

func newTestRecordingListener() *testRecordingListener {
	return &testRecordingListener{received: make(chan *SidecarEvent, 64)}
}

func (self *testRecordingListener) SidecarEventReceived(event *SidecarEvent) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.events = append(self.events, event)
	}()
	self.received <- event
}

// Waits for the event of kind, failing on a timeout.
func (self *testRecordingListener) awaitKind(t *testing.T, kind string) *SidecarEvent {
	t.Helper()
	deadline := time.After(testSidecarTimeout)
	for {
		select {
		case event := <-self.received:
			if event.Kind == kind {
				return event
			}
		case <-deadline:
			t.Fatalf("no %s event", kind)
		}
	}
}

// What it recorded.
func (self *testRecordingListener) recorded() []*SidecarEvent {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.events)
}

// An event of a generation older than one already delivered is dropped, at every listener; the same or a newer
// generation's is delivered.
func TestOlderGenerationEventDropped(t *testing.T) {
	dispatch := newSidecarEventDispatch(t.Context(), defaultSidecarEventSettings())
	defer dispatch.close()
	first := newTestRecordingListener()
	second := newTestRecordingListener()
	dispatch.addListener(first)
	dispatch.addListener(second)

	for _, generation := range []int64{2, 1, 2, 3, 2} {
		dispatch.offer(sidecar.SidecarEvent{Kind: sidecar.SidecarEventState, Generation: generation})
	}
	dispatch.offer(sidecar.SidecarEvent{Kind: "done", Generation: 3})
	first.awaitKind(t, "done")
	second.awaitKind(t, "done")
	for _, listener := range []*testRecordingListener{first, second} {
		generations := []int64{}
		for _, event := range listener.recorded() {
			generations = append(generations, event.Generation)
		}
		if !slices.Equal(generations, []int64{2, 2, 3, 3}) {
			t.Errorf("delivered generations %v", generations)
		}
	}
}

// A listener that panics loses only its own call: the next listener and the next event still arrive.
func TestListenerPanicLosesOnlyItsCall(t *testing.T) {
	dispatch := newSidecarEventDispatch(t.Context(), defaultSidecarEventSettings())
	defer dispatch.close()
	dispatch.addListener(&testPanickingListener{})
	recording := newTestRecordingListener()
	dispatch.addListener(recording)
	dispatch.offer(sidecar.SidecarEvent{Kind: "first", Generation: 1})
	dispatch.offer(sidecar.SidecarEvent{Kind: "second", Generation: 1})
	recording.awaitKind(t, "first")
	recording.awaitKind(t, "second")
}

// A listener that always panics.
type testPanickingListener struct{}

func (self *testPanickingListener) SidecarEventReceived(event *SidecarEvent) {
	panic("synthetic listener panic")
}

// A listener held at a barrier until released.
type testHeldListener struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (self *testHeldListener) SidecarEventReceived(event *SidecarEvent) {
	self.once.Do(func() {
		close(self.entered)
	})
	<-self.release
}

// The core's callback never waits: with the delivering goroutine held in a listener, events past the queue's room are
// dropped and counted at once, and the rest deliver once it lets go.
func TestFullQueueDropsAndCounts(t *testing.T) {
	settings := defaultSidecarEventSettings()
	settings.QueueEventCount = 2
	dispatch := newSidecarEventDispatch(t.Context(), settings)
	defer dispatch.close()
	held := &testHeldListener{entered: make(chan struct{}), release: make(chan struct{})}
	dispatch.addListener(held)

	dispatch.offer(sidecar.SidecarEvent{Kind: "held", Generation: 1})
	select {
	case <-held.entered:
	case <-time.After(testSidecarTimeout):
		t.Fatal("the first event was not delivered")
	}
	// the queue holds two; the third and fourth drop, each offer returning at once
	for range 4 {
		dispatch.offer(sidecar.SidecarEvent{Kind: "queued", Generation: 1})
	}
	if dropped := dispatch.droppedCount.Load(); dropped != 2 {
		t.Errorf("dropped %d", dropped)
	}
	close(held.release)
}

// A core event reaches an app listener as its bound event, through a real local Sidecar, and a closed Sub hears no
// more.
func TestCoreEventsReachListeners(t *testing.T) {
	world := newTestSidecarWorld(t)
	world.keyStore.setMode(testKeyStoreLocked)
	local := world.open(t)
	awaitTestSidecarState(t, local, sidecar.SidecarStateLocked, "")
	listener := newTestRecordingListener()
	sub := local.AddEventListener(listener)
	closed := newTestRecordingListener()
	local.AddEventListener(closed).Close()

	world.keyStore.setMode(testKeyStoreWorking)
	if err := local.Core().Unlock(nil); err != nil {
		t.Fatal(err)
	}
	event := listener.awaitKind(t, string(sidecar.SidecarEventState))
	if event.State != string(sidecar.SidecarStateUnenrolled) || event.Generation != 0 {
		t.Errorf("the state event %+v", event)
	}
	if events := closed.recorded(); len(events) != 0 {
		t.Errorf("a closed Sub heard %d events", len(events))
	}
	sub.Close()
	sub.Close()
	if nilSub := local.AddEventListener(nil); nilSub == nil {
		t.Error("a nil listener's Sub is nil")
	}
}

// The core's rollback head_alarm as the push raises it (QD-928, owner decision 300): no peer, and as Evidence team's
// TopicVersion held at 10, then the one offered at 6, as protobuf bytes.
func testRollbackCoreEvent(t *testing.T) sidecar.SidecarEvent {
	t.Helper()
	held, err := proto.Marshal(&protocol.TopicVersion{Topic: sidecar.PushTopicTeam, Version: 10})
	if err != nil {
		t.Fatal(err)
	}
	offered, err := proto.Marshal(&protocol.TopicVersion{Topic: sidecar.PushTopicTeam, Version: 6})
	if err != nil {
		t.Fatal(err)
	}
	return sidecar.SidecarEvent{
		Kind:       sidecar.SidecarEventHeadAlarm,
		Generation: 1,
		HeadAlarm:  sidecar.SidecarHeadAlarmRollback,
		Evidence:   [2][]byte{held, offered},
	}
}

// Fails unless event is testRollbackCoreEvent's as an app sees it: head_alarm rollback, not fork, with no peer, and
// the held and offered TopicVersions in that order.
func testCheckRollbackEvent(t *testing.T, event *SidecarEvent) {
	t.Helper()
	if event.Kind != "head_alarm" || event.HeadAlarm != "rollback" || event.SidecarId != nil {
		t.Fatalf("the rollback as an app sees it: %+v", event)
	}
	held := &protocol.TopicVersion{}
	offered := &protocol.TopicVersion{}
	if err := proto.Unmarshal(event.FirstEvidence, held); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(event.SecondEvidence, offered); err != nil {
		t.Fatal(err)
	}
	if held.Topic != sidecar.PushTopicTeam || held.Version != 10 || offered.Topic != sidecar.PushTopicTeam || offered.Version != 6 {
		t.Errorf("the rollback's evidence: held %v, offered %v", held, offered)
	}
}

// The local backend's relay passes the push's rollback head_alarm to an app listener as the core raised it: its kind
// and both TopicVersions, the held one first.
func TestRollbackHeadAlarmReachesTheListener(t *testing.T) {
	dispatch := newSidecarEventDispatch(t.Context(), defaultSidecarEventSettings())
	defer dispatch.close()
	listener := newTestRecordingListener()
	dispatch.addListener(listener)
	dispatch.offer(testRollbackCoreEvent(t))
	testCheckRollbackEvent(t, listener.awaitKind(t, string(sidecar.SidecarEventHeadAlarm)))
}
