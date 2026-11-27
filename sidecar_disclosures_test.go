//go:build !ios_extension && !js

package sdk

// Disclosures for the apps (sidecar_disclosures.go; CTRUST-M11-01, CTRUST-M12-01, CTRUST-M12-02): Disclosures()' JSON,
// the core's list over a real registry, in the locale SetDisclosureLocale set, else English; what it remembers per id,
// which AckDisclosure, OpenDisclosure and AckNetworkDisclosure take; disclosure_changed where the version shown is no
// longer current; the acts sent as the person's confirmed calls, which the daemon refuses unconfirmed; the network's
// acknowledgments a card lists; and the listener. On a stand-in of ur daemon's disclosure methods; the daemon's own
// rules are the root's disclosure_daemon_test.go's.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A policy at version 3 carrying engy's disclosure, in English and German, and local's, in English.
func testSdkDisclosurePolicy() *sidecar.ComputePolicy {
	return &sidecar.ComputePolicy{
		Version: 3,
		Direct:  sidecar.ComputePolicyDirectLine{Providers: []*sidecar.ComputePolicyDirect{{Slug: "engy"}}},
		Disclosures: []*sidecar.ComputePolicyDisclosure{
			{Id: "inference:engy", Version: 1, GraceDays: 14, SinceMs: 1796083200000, Locales: []*sidecar.ComputePolicyDisclosureText{
				{Locale: "en", Summary: "Not in a TEE: engy reads this conversation.", Exposed: []string{"Everything you send."}, Private: []string{"Your device."}},
				{Locale: "de", Summary: "Nicht in einer TEE: engy liest dieses Gespräch.", Exposed: []string{"Alles."}, Private: []string{"Dein Gerät."}},
			}},
			{Id: "inference:local", Version: 1, GraceDays: 14, SinceMs: 1796083200000, Locales: []*sidecar.ComputePolicyDisclosureText{
				{Locale: "en", Summary: "On this device.", Exposed: []string{"Anyone with this device unlocked."}, Private: []string{"What you said."}},
			}},
		},
	}
}

// The newest policy a test sets.
type testSdkDisclosureSource struct {
	stateLock sync.Mutex
	policy    *sidecar.ComputePolicy
}

func (self *testSdkDisclosureSource) Newest() *sidecar.ComputePolicy {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.policy
}

func (self *testSdkDisclosureSource) set(policy *sidecar.ComputePolicy) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.policy = policy
}

// One call the daemon took.
type testDisclosureDaemonCall struct {
	method    string
	confirmed bool
	params    json.RawMessage
}

// ur daemon's disclosure methods over a registry, keeping its Confirm rule: an act no person confirmed is refused and
// records nothing; an act at a version that is not the current one answers disclosure_changed. The network's
// acknowledgments it records are listed as the core lists a taken entry, an Owner's of version shown.
type testDisclosureDaemon struct {
	registry *sidecar.ComputeDisclosureRegistry

	stateLock    sync.Mutex
	calls        []testDisclosureDaemonCall
	acknowledged map[string]uint32
	opened       map[string]uint32
	networks     map[string]*sidecarControlDisclosureNetworkActParams
	// what ack_network answers in place of recording, an error for a device that may not make the act
	networkRefusal error
}

func newTestDisclosureDaemon(t *testing.T, source *testSdkDisclosureSource) *testDisclosureDaemon {
	t.Helper()
	registry, err := sidecar.NewComputeDisclosureRegistry(&sidecar.ComputeDisclosureRegistryArgs{Source: source})
	if err != nil {
		t.Fatal(err)
	}
	return &testDisclosureDaemon{registry: registry, acknowledged: map[string]uint32{}, opened: map[string]uint32{},
		networks: map[string]*sidecarControlDisclosureNetworkActParams{}}
}

func (self *testDisclosureDaemon) sidecarId() *Id {
	return nil
}

func (self *testDisclosureDaemon) state() (string, string) {
	return "online", ""
}

func (self *testDisclosureDaemon) generation() int64 {
	return 1
}

func (self *testDisclosureDaemon) refresh(ctx context.Context) error {
	return nil
}

func (self *testDisclosureDaemon) pair(ctx context.Context, childJwt string) (*sidecarControlPairReply, error) {
	return nil, errors.New("this daemon pairs nothing")
}

func (self *testDisclosureDaemon) network() string {
	return "the paired network"
}

func (self *testDisclosureDaemon) close() {
}

func (self *testDisclosureDaemon) call(ctx context.Context, method string, params any, confirmed bool, result any) error {
	paramsJson, err := json.Marshal(params)
	if err != nil {
		return err
	}
	reply, err := func() (any, error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.calls = append(self.calls, testDisclosureDaemonCall{method: method, confirmed: confirmed, params: paramsJson})
		switch method {
		case sidecarControlMethodDisclosureList:
			listParams := &sidecarControlDisclosureListParams{}
			if err := json.Unmarshal(paramsJson, listParams); err != nil {
				return nil, err
			}
			list, err := self.registry.List(listParams.Locale)
			if errors.Is(err, sidecar.ErrComputeDisclosurePolicyUnavailable) {
				return &sidecar.ComputeDisclosureList{Disclosures: []*sidecar.ComputeDisclosureView{}, Unavailable: sidecar.ComputeDisclosureCodePolicyUnavailable}, nil
			}
			if err != nil {
				return nil, err
			}
			for _, view := range list.Disclosures {
				view.Scope, view.Acknowledged = sidecar.ComputeDisclosureScopePerson, self.acknowledged[view.Id]
				view.Covered = !view.Asks || view.Version <= view.Acknowledged
				if network := self.networks[view.Id]; network != nil {
					view.Network = &sidecar.ComputeNetworkDisclosure{
						Id: view.Id, Version: network.Version, Locale: network.Locale, Revision: network.Revision,
						MemberId: testSdkDisclosureOwner, SidecarId: testSdkDisclosureOwnerDevice, PinVersion: 40,
						SignedAtMs: 1796083200000, Taken: true, Stands: view.Version <= network.Version,
					}
				}
			}
			return list, nil
		case sidecarControlMethodDisclosureAckNetwork, sidecarControlMethodDisclosureRemoveNetwork:
			if !confirmed {
				return nil, &sidecarControlError{Code: -32000, Message: "the call is not the person's confirmed act"}
			}
			if method == sidecarControlMethodDisclosureRemoveNetwork {
				remove := &sidecarControlDisclosureRemoveParams{}
				if err := json.Unmarshal(paramsJson, remove); err != nil {
					return nil, err
				}
				delete(self.networks, remove.Id)
				return &sidecarControlDisclosureActReply{Id: remove.Id}, nil
			}
			if self.networkRefusal != nil {
				return nil, self.networkRefusal
			}
			act := &sidecarControlDisclosureNetworkActParams{}
			if err := json.Unmarshal(paramsJson, act); err != nil {
				return nil, err
			}
			disclosure, err := self.registry.Disclosure(act.Id)
			if err != nil {
				return &sidecarControlDisclosureActReply{Id: act.Id, Refusal: sidecar.ComputeDisclosureCodeRequired}, nil
			}
			if disclosure.Version() != act.Version {
				return &sidecarControlDisclosureActReply{Id: act.Id, Refusal: sidecar.ComputeDisclosureCodeChanged}, nil
			}
			self.networks[act.Id] = act
			return &sidecarControlDisclosureActReply{Id: act.Id, Version: act.Version}, nil
		case sidecarControlMethodDisclosureAck, sidecarControlMethodDisclosureOpen:
			if !confirmed {
				return nil, &sidecarControlError{Code: -32000, Message: "the call is not the person's confirmed act"}
			}
			act := &sidecarControlDisclosureActParams{}
			if err := json.Unmarshal(paramsJson, act); err != nil {
				return nil, err
			}
			disclosure, err := self.registry.Disclosure(act.Id)
			if err != nil {
				return &sidecarControlDisclosureActReply{Id: act.Id, Refusal: sidecar.ComputeDisclosureCodeRequired}, nil
			}
			if disclosure.Version() != act.Version {
				return &sidecarControlDisclosureActReply{Id: act.Id, Refusal: sidecar.ComputeDisclosureCodeChanged}, nil
			}
			if method == sidecarControlMethodDisclosureAck {
				self.acknowledged[act.Id] = act.Version
			} else {
				self.opened[act.Id] = act.Version
			}
			return &sidecarControlDisclosureActReply{Id: act.Id, Version: act.Version}, nil
		}
		return nil, &sidecarControlError{Code: sidecarControlCodeMethodNotFound, Message: "method not found"}
	}()
	if err != nil {
		return err
	}
	replyJson, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	return json.Unmarshal(replyJson, result)
}

func (self *testDisclosureDaemon) taken() []testDisclosureDaemonCall {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.calls)
}

func (self *testDisclosureDaemon) recorded() (map[string]uint32, map[string]uint32) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	acknowledged := map[string]uint32{}
	for id, version := range self.acknowledged {
		acknowledged[id] = version
	}
	opened := map[string]uint32{}
	for id, version := range self.opened {
		opened[id] = version
	}
	return acknowledged, opened
}

// A listener counting its calls.
type testDisclosuresListener struct {
	stateLock sync.Mutex
	count     int
}

func (self *testDisclosuresListener) DisclosuresChanged() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.count += 1
}

func (self *testDisclosuresListener) told() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.count
}

// A Sidecar whose remote backend is daemon.
func newTestDisclosureSidecar(daemon sidecarBackend) *Sidecar {
	return &Sidecar{remote: daemon}
}

// Disclosures()' JSON is the core's list, decoded strictly; it renders in the locale SetDisclosureLocale set, else
// English, never mixing them; it remembers per id the version, locale and revision it returned; with no verified
// policy it answers policy_unavailable and remembers nothing new.
func TestSidecarDisclosuresJson(t *testing.T) {
	source := &testSdkDisclosureSource{policy: testSdkDisclosurePolicy()}
	daemon := newTestDisclosureDaemon(t, source)
	app := newTestDisclosureSidecar(daemon)
	listener := &testDisclosuresListener{}
	sub := app.AddDisclosuresListener(listener)
	defer sub.Close()

	read := func() *sidecar.ComputeDisclosureList {
		t.Helper()
		answer, err := app.Disclosures()
		if err != nil {
			t.Fatal(err)
		}
		list := &sidecar.ComputeDisclosureList{}
		decoder := json.NewDecoder(strings.NewReader(answer))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(list); err != nil {
			t.Fatal(err)
		}
		return list
	}
	list := read()
	if list.PolicyVersion != 3 || len(list.Disclosures) != 2 || list.Unavailable != "" {
		t.Fatalf("the list %+v", list)
	}
	engy := list.Disclosures[0]
	if engy.Id != "inference:engy" || engy.Version != 1 || !engy.Asks || engy.Locale != "en" || engy.Summary != "Not in a TEE: engy reads this conversation." ||
		engy.AsksText != "asks before the first call" || engy.LabelText != "none: not in a TEE" || engy.Covered || engy.Acknowledged != 0 {
		t.Errorf("engy %+v", engy)
	}
	if calls := daemon.taken(); len(calls) != 1 || calls[0].confirmed || string(calls[0].params) != `{"network":"the paired network"}` {
		t.Errorf("the call %+v", calls)
	}
	if shown := app.disclosureMemoryOf().shown("inference:engy"); shown == nil || shown.version != 1 || shown.locale != "en" || shown.revision != 0 {
		t.Errorf("remembered %+v", shown)
	}

	// the person's locale, German where the policy has it, English whole where it does not
	app.SetDisclosureLocale("de-AT")
	app.SetDisclosureLocale("de-AT")
	if listener.told() != 1 {
		t.Errorf("the listener was told %d times, want 1", listener.told())
	}
	list = read()
	if string(daemon.taken()[1].params) != `{"network":"the paired network","locale":"de-AT"}` {
		t.Errorf("the params %s", daemon.taken()[1].params)
	}
	if list.Disclosures[0].Locale != "de" || list.Disclosures[0].Summary != "Nicht in einer TEE: engy liest dieses Gespräch." ||
		list.Disclosures[1].Locale != "en" || list.Disclosures[1].Summary != "On this device." {
		t.Errorf("in German: %+v %+v", list.Disclosures[0], list.Disclosures[1])
	}
	if shown := app.disclosureMemoryOf().shown("inference:engy"); shown == nil || shown.locale != "de" {
		t.Errorf("remembered %+v", shown)
	}
	app.SetDisclosureLocale("fr")
	if list = read(); list.Disclosures[0].Locale != "en" {
		t.Errorf("French falls back to English: %+v", list.Disclosures[0])
	}

	// a revision is remembered
	revised := testSdkDisclosurePolicy()
	revised.Version, revised.Disclosures[0].Revision = 4, 2
	source.set(revised)
	read()
	if shown := app.disclosureMemoryOf().shown("inference:engy"); shown == nil || shown.version != 1 || shown.revision != 2 {
		t.Errorf("remembered %+v", shown)
	}

	// no verified policy: policy_unavailable, never an empty list
	source.set(nil)
	if answer, err := app.Disclosures(); answer != "" || !errors.Is(err, sidecar.ErrComputeDisclosurePolicyUnavailable) {
		t.Errorf("no policy: %q, %v", answer, err)
	}
	if app.disclosureMemoryOf().shown("inference:engy") == nil {
		t.Error("a refused read forgot what was shown")
	}
}

// AckDisclosure records the version Disclosures() last returned: refused where none was returned, sending nothing;
// disclosure_changed where the policy moved on since, recording nothing; and once the newer text is shown, its version.
func TestAckDisclosureChanged(t *testing.T) {
	source := &testSdkDisclosureSource{policy: testSdkDisclosurePolicy()}
	daemon := newTestDisclosureDaemon(t, source)
	app := newTestDisclosureSidecar(daemon)
	listener := &testDisclosuresListener{}
	sub := app.AddDisclosuresListener(listener)
	defer sub.Close()

	if err := app.AckDisclosure("inference:engy"); !errors.Is(err, errSidecarDisclosureNotShown) || len(daemon.taken()) != 0 {
		t.Fatalf("before any list: %v, %d calls", err, len(daemon.taken()))
	}
	if _, err := app.Disclosures(); err != nil {
		t.Fatal(err)
	}
	moved := testSdkDisclosurePolicy()
	moved.Version, moved.Disclosures[0].Version = 4, 2
	source.set(moved)
	if err := app.AckDisclosure("inference:engy"); !errors.Is(err, sidecar.ErrDisclosureChanged) {
		t.Fatalf("a version moved on: %v", err)
	}
	if acknowledged, _ := daemon.recorded(); len(acknowledged) != 0 || listener.told() != 0 {
		t.Fatalf("recorded %v, the listener told %d", acknowledged, listener.told())
	}
	if _, err := app.Disclosures(); err != nil {
		t.Fatal(err)
	}
	if err := app.AckDisclosure("inference:engy"); err != nil {
		t.Fatal(err)
	}
	if acknowledged, _ := daemon.recorded(); acknowledged["inference:engy"] != 2 || listener.told() != 1 {
		t.Fatalf("recorded %v, the listener told %d", acknowledged, listener.told())
	}
	// the list then says so
	answer, err := app.Disclosures()
	if err != nil || !strings.Contains(answer, `"acknowledged":2`) || !strings.Contains(answer, `"covered":true`) {
		t.Fatalf("after the acknowledgment: %s, %v", answer, err)
	}
}

// The Owner and the device the stand-in's network acknowledgments name.
var (
	testSdkDisclosureOwner       = sidecar.Id{0x0a, 0x01}
	testSdkDisclosureOwnerDevice = sidecar.Id{0x0a, 0x02}
)

// AckNetworkDisclosure signs for the version, locale and revision Disclosures() last returned: refused where none was
// returned, sending nothing; sent as the person's confirmed act with the locale shown, German here; listed then on the
// card's list, which changes nothing the acts take; disclosure_changed where the policy moved on, recording nothing; a
// device the daemon refuses, as a Member's, records nothing; and RemoveNetworkDisclosure takes it out.
func TestAckNetworkDisclosure(t *testing.T) {
	source := &testSdkDisclosureSource{policy: testSdkDisclosurePolicy()}
	daemon := newTestDisclosureDaemon(t, source)
	app := newTestDisclosureSidecar(daemon)
	listener := &testDisclosuresListener{}
	sub := app.AddDisclosuresListener(listener)
	defer sub.Close()

	if err := app.AckNetworkDisclosure("inference:engy"); !errors.Is(err, errSidecarDisclosureNotShown) || len(daemon.taken()) != 0 {
		t.Fatalf("before any list: %v, %d calls", err, len(daemon.taken()))
	}
	app.SetDisclosureLocale("de")
	if _, err := app.Disclosures(); err != nil {
		t.Fatal(err)
	}
	if err := app.AckNetworkDisclosure("inference:engy"); err != nil {
		t.Fatal(err)
	}
	last := daemon.taken()[len(daemon.taken())-1]
	if last.method != sidecarControlMethodDisclosureAckNetwork || !last.confirmed ||
		string(last.params) != `{"network":"the paired network","id":"inference:engy","version":1,"locale":"de","revision":0}` {
		t.Fatalf("the call %+v %s", last, last.params)
	}
	if listener.told() != 2 {
		t.Errorf("the listener was told %d times, want 2", listener.told())
	}
	// the card's list names it, and reading it moves nothing the acts take
	moved := testSdkDisclosurePolicy()
	moved.Version, moved.Disclosures[0].Version = 4, 2
	networks, err := app.NetworkDisclosures()
	if err != nil {
		t.Fatal(err)
	}
	if networks.Len() != 1 {
		t.Fatalf("%d network acknowledgments", networks.Len())
	}
	engy := networks.Get(0)
	if engy.Id != "inference:engy" || engy.Version != 1 || engy.CurrentVersion != 1 || engy.Locale != "de" || !engy.Taken ||
		!engy.Stands || engy.MemberId == nil || engy.SidecarId == nil || engy.SignedAtMillis != 1796083200000 {
		t.Errorf("the card's entry %+v", engy)
	}
	source.set(moved)
	networks, err = app.NetworkDisclosures()
	if err != nil || networks.Len() != 1 || networks.Get(0).Stands || networks.Get(0).CurrentVersion != 2 {
		t.Fatalf("at version 2: %v, %v", networks, err)
	}
	if shown := app.disclosureMemoryOf().shown("inference:engy"); shown == nil || shown.version != 1 {
		t.Fatalf("the card's list moved what was shown: %+v", shown)
	}
	// the version shown is no longer the current one
	if err := app.AckNetworkDisclosure("inference:engy"); !errors.Is(err, sidecar.ErrDisclosureChanged) {
		t.Fatalf("a version moved on: %v", err)
	}
	// a device the daemon refuses records nothing
	daemon.networkRefusal = &sidecarControlError{Code: -32000, Message: "this device's member is no Owner or Admin"}
	if _, err := app.Disclosures(); err != nil {
		t.Fatal(err)
	}
	if err := app.AckNetworkDisclosure("inference:engy"); err == nil || !strings.Contains(err.Error(), "no Owner or Admin") {
		t.Fatalf("a Member's device: %v", err)
	}
	if networks, _ := app.NetworkDisclosures(); networks.Len() != 1 || networks.Get(0).Version != 1 {
		t.Fatalf("a refused act recorded: %+v", networks.Get(0))
	}
	// an unconfirmed call is the daemon's to refuse
	unconfirmed := &sidecarControlDisclosureActReply{}
	params := &sidecarControlDisclosureNetworkActParams{Id: "inference:engy", Version: 2, Locale: "en"}
	if err := daemon.call(t.Context(), sidecarControlMethodDisclosureAckNetwork, params, false, unconfirmed); err == nil {
		t.Fatal("an unconfirmed network acknowledgment was taken")
	}
	// the removal, sent confirmed
	if err := app.RemoveNetworkDisclosure("inference:engy"); err != nil {
		t.Fatal(err)
	}
	last = daemon.taken()[len(daemon.taken())-1]
	if last.method != sidecarControlMethodDisclosureRemoveNetwork || !last.confirmed || string(last.params) != `{"network":"the paired network","id":"inference:engy"}` {
		t.Fatalf("the removal's call %+v %s", last, last.params)
	}
	if networks, _ := app.NetworkDisclosures(); networks.Len() != 0 {
		t.Fatalf("after the removal: %d", networks.Len())
	}
}

// The acts reach the daemon only as the person's confirmed calls, which its Confirm methods take; an unconfirmed call,
// as anything but the app's UI would make, records nothing.
func TestAckDisclosureRemoteUnconfirmedRecordsNothing(t *testing.T) {
	daemon := newTestDisclosureDaemon(t, &testSdkDisclosureSource{policy: testSdkDisclosurePolicy()})
	app := newTestDisclosureSidecar(daemon)
	if _, err := app.Disclosures(); err != nil {
		t.Fatal(err)
	}
	unconfirmed := &sidecarControlDisclosureActReply{}
	params := &sidecarControlDisclosureActParams{Id: "inference:engy", Version: 1}
	if err := daemon.call(t.Context(), sidecarControlMethodDisclosureAck, params, false, unconfirmed); err == nil {
		t.Fatal("an unconfirmed acknowledgment was taken")
	}
	if err := daemon.call(t.Context(), sidecarControlMethodDisclosureOpen, params, false, unconfirmed); err == nil {
		t.Fatal("an unconfirmed opening was taken")
	}
	if acknowledged, opened := daemon.recorded(); len(acknowledged) != 0 || len(opened) != 0 {
		t.Fatalf("an unconfirmed call recorded %v %v", acknowledged, opened)
	}
	before := len(daemon.taken())
	if err := app.OpenDisclosure("inference:engy"); err != nil {
		t.Fatal(err)
	}
	if err := app.AckDisclosure("inference:engy"); err != nil {
		t.Fatal(err)
	}
	// the app's own calls, after the test's unconfirmed ones
	calls := daemon.taken()[before:]
	if len(calls) != 2 {
		t.Fatalf("the app made %d calls, want 2", len(calls))
	}
	for _, call := range calls {
		if call.method != sidecarControlMethodDisclosureList && !call.confirmed {
			t.Errorf("%s was sent unconfirmed", call.method)
		}
	}
	if last := calls[len(calls)-1]; last.method != sidecarControlMethodDisclosureAck || string(last.params) != `{"network":"the paired network","id":"inference:engy","version":1}` {
		t.Errorf("the acknowledgment's call %+v", last)
	}
	if acknowledged, opened := daemon.recorded(); acknowledged["inference:engy"] != 1 || opened["inference:engy"] != 1 {
		t.Fatalf("recorded %v %v", acknowledged, opened)
	}
}

// Every exported signature this file adds is one gomobile binds: strings, errors, a listener interface, Sub and a
// typed list.
func TestSidecarDisclosuresBindable(t *testing.T) {
	var _ func(string) = (&Sidecar{}).SetDisclosureLocale
	var _ func(string) error = (&Sidecar{}).OpenDisclosure
	var _ func(SidecarDisclosuresListener) Sub = (&Sidecar{}).AddDisclosuresListener
	var _ func(string) error = (&Sidecar{}).RemoveNetworkDisclosure
	var _ func() (*SidecarNetworkDisclosureList, error) = (&Sidecar{}).NetworkDisclosures
	listener := &testDisclosuresListener{}
	app := &Sidecar{}
	sub := app.AddDisclosuresListener(listener)
	app.SetDisclosureLocale("en")
	sub.Close()
	app.SetDisclosureLocale("de")
	if listener.told() != 1 {
		t.Errorf("a removed listener was told: %d", listener.told())
	}
}
