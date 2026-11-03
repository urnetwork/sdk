//go:build !ios_extension && !js

package sdk

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk/sn/protocol"
)

// First sight of the consent pins the coldkey, before any binding exists.
func TestSnFleetDevicePinsColdkeyOnFirstSightOfConsent(t *testing.T) {
	store := &testingFleetMemoryStore{}
	server := &testingFleetServer{store: store, work: &snFleetDeviceWork{Consent: testingFleetConsent(t, testingFleetColdkeySeed, testingFleetNetworkId, 1), NextCheckSeconds: 86400}}
	signer := newTestingFleetSigner(t)
	device := newTestingFleetDevice(t, server, signer, store)

	result, err := device.CheckSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != SnFleetDeviceActionPinned {
		t.Fatalf("action %+v", result)
	}
	coldkey := testingFleetPublic(t, testingFleetColdkeySeed)
	if state := store.state(t); state.Coldkey != snFleetHex(coldkey[:]) || state.Hotkey != "" {
		t.Fatalf("state %+v", state)
	}
	if !server.workAsked[0] {
		t.Fatal("an unpinned device did not ask for the consent")
	}
	if signer.bindings != 0 || len(server.signatures) != 0 {
		t.Fatal("pinning signed or sent something")
	}

	// from then on the device has no reason to ask
	server.work = nil
	if _, err := device.CheckSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if server.workAsked[1] {
		t.Fatal("a pinned device asked for the consent again")
	}
}

// With its coldkey pinned and everything in order, a device signs with no command.
func TestSnFleetDeviceSignsACheckedBinding(t *testing.T) {
	store := &testingFleetMemoryStore{}
	work := testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 1)
	work.Consent = testingFleetConsent(t, testingFleetColdkeySeed, testingFleetNetworkId, 1)
	server := &testingFleetServer{store: store, work: work}
	signer := newTestingFleetSigner(t)
	device := newTestingFleetDevice(t, server, signer, store)

	result, err := device.CheckSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != SnFleetDeviceActionSigned || result.Generation != 1 || result.SendFailed {
		t.Fatalf("result %+v", result)
	}
	if signer.bindings != 1 || len(server.signatures) != 1 {
		t.Fatalf("signed %d, sent %d", signer.bindings, len(server.signatures))
	}
	binding, err := snParseFleetBinding(work.BindingJson)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := snFleetParseHex(server.signatures[0].ClientSignature)
	if err != nil || !binding.VerifyClient(signature) {
		t.Fatalf("client signature: %v", err)
	}
	digest, _ := binding.Digest()
	if server.signatures[0].Digest != snFleetHex(digest[:]) {
		t.Fatal("digest")
	}
	hotkey := testingFleetPublic(t, testingFleetHotkeySeed)
	state := store.state(t)
	if state.Hotkey != snFleetHex(hotkey[:]) || len(state.Signed) != 1 {
		t.Fatalf("state %+v", state)
	}
	saved := state.Signed[0]
	if saved.BindingJson != work.BindingJson || saved.HotkeySignature != work.HotkeySignature || saved.Authorization == nil || saved.Authorization.Message != work.Authorization.Message {
		t.Fatalf("the device didn't save what it received: %+v", saved)
	}
}

// The refusals the plan names, each from a device with its pins in place: no
// signature is made, the reason is said, the server is told, and no pin moves.
func TestSnFleetDeviceRefuses(t *testing.T) {
	pinnedColdkey := testingFleetPublic(t, testingFleetColdkeySeed)
	pinnedHotkey := testingFleetPublic(t, testingFleetHotkeySeed)
	otherHotkey := testingFleetPublic(t, testingFleetOtherHotkeySeed)
	clientId := testingFleetId(t, testingFleetClientId)
	otherClientKey := [32]byte(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey))

	// a binding of another member, signed by the hotkey, under an authorization
	bindingOf := func(t *testing.T, member protocol.FleetMember) *snFleetDeviceWork {
		manifest := testingFleetManifest(t, pinnedHotkey, 1, member)
		binding, err := manifest.Binding(member, 5, 16)
		if err != nil {
			t.Fatal(err)
		}
		signature, err := protocol.SignFleetBindingWithHotkey(testingFleetHotkeySeed, binding)
		if err != nil {
			t.Fatal(err)
		}
		return &snFleetDeviceWork{
			Authorization:   testingFleetAuthorization(t, testingFleetColdkeySeed, pinnedHotkey, testingFleetNetworkId),
			BindingJson:     testingFleetBindingJson(t, binding),
			HotkeySignature: snFleetHex(signature),
		}
	}

	cases := []struct {
		name   string
		code   string
		unpin  bool
		work   func(t *testing.T) *snFleetDeviceWork
		before func(state *snFleetDeviceState)
	}{
		{
			name: "another client",
			code: SnFleetRefusalOtherClient,
			work: func(t *testing.T) *snFleetDeviceWork {
				return bindingOf(t, protocol.FleetMember{ClientID: [16]byte{0xee}, ClientKey: testingFleetClientKey()})
			},
		},
		{
			name: "another client key",
			code: SnFleetRefusalOtherClientKey,
			work: func(t *testing.T) *snFleetDeviceWork {
				return bindingOf(t, protocol.FleetMember{ClientID: [16]byte(clientId), ClientKey: otherClientKey})
			},
		},
		{
			name: "a bad hotkey signature",
			code: SnFleetRefusalBadHotkeySignature,
			work: func(t *testing.T) *snFleetDeviceWork {
				work := testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 1)
				// a real signature by another hotkey over the same binding
				binding, err := snParseFleetBinding(work.BindingJson)
				if err != nil {
					t.Fatal(err)
				}
				signature, err := protocol.SignFleetBindingWithHotkey(testingFleetOtherHotkeySeed, *binding)
				if err != nil {
					t.Fatal(err)
				}
				work.HotkeySignature = snFleetHex(signature)
				return work
			},
		},
		{
			name: "an empty hotkey signature",
			code: SnFleetRefusalBadHotkeySignature,
			work: func(t *testing.T) *snFleetDeviceWork {
				work := testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 1)
				work.HotkeySignature = ""
				return work
			},
		},
		{
			name:  "an unpinned coldkey",
			code:  SnFleetRefusalColdkeyNotPinned,
			unpin: true,
			work: func(t *testing.T) *snFleetDeviceWork {
				return testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 1)
			},
		},
		{
			name: "no authorization",
			code: SnFleetRefusalBadAuthorization,
			work: func(t *testing.T) *snFleetDeviceWork {
				work := testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 1)
				work.Authorization = nil
				return work
			},
		},
		{
			name: "a consent replayed as an authorization",
			code: SnFleetRefusalBadAuthorization,
			work: func(t *testing.T) *snFleetDeviceWork {
				work := testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 1)
				work.Authorization = testingFleetConsent(t, testingFleetColdkeySeed, testingFleetNetworkId, 1)
				return work
			},
		},
		{
			name: "an authorization signature over other text",
			code: SnFleetRefusalBadAuthorization,
			work: func(t *testing.T) *snFleetDeviceWork {
				work := testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 1)
				work.Authorization.Signature = testingFleetConsent(t, testingFleetColdkeySeed, testingFleetNetworkId, 1).Signature
				return work
			},
		},
		{
			name: "an authorization of another network",
			code: SnFleetRefusalBadAuthorization,
			work: func(t *testing.T) *snFleetDeviceWork {
				work := testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 1)
				work.Authorization = testingFleetAuthorization(t, testingFleetColdkeySeed, pinnedHotkey, "019b2e10-0000-4d3e-8f01-000000000002")
				return work
			},
		},
		{
			name: "a changed coldkey",
			code: SnFleetRefusalColdkeyChanged,
			work: func(t *testing.T) *snFleetDeviceWork {
				return testingFleetWork(t, testingFleetOtherColdkeySeed, testingFleetHotkeySeed, 1)
			},
		},
		{
			name: "a changed coldkey in a new consent",
			code: SnFleetRefusalColdkeyChanged,
			work: func(t *testing.T) *snFleetDeviceWork {
				return &snFleetDeviceWork{Consent: testingFleetConsent(t, testingFleetOtherColdkeySeed, testingFleetNetworkId, 2)}
			},
		},
		{
			name: "a changed hotkey",
			code: SnFleetRefusalHotkeyChanged,
			work: func(t *testing.T) *snFleetDeviceWork {
				return testingFleetWork(t, testingFleetColdkeySeed, testingFleetOtherHotkeySeed, 1)
			},
		},
		{
			name: "a hotkey the coldkey did not authorize",
			code: SnFleetRefusalNotAuthorized,
			work: func(t *testing.T) *snFleetDeviceWork {
				work := testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 1)
				work.Authorization = testingFleetAuthorization(t, testingFleetColdkeySeed, otherHotkey, testingFleetNetworkId)
				return work
			},
		},
		{
			name: "a consent of another network",
			code: SnFleetRefusalBadConsent,
			work: func(t *testing.T) *snFleetDeviceWork {
				return &snFleetDeviceWork{Consent: testingFleetConsent(t, testingFleetColdkeySeed, "019b2e10-0000-4d3e-8f01-000000000002", 1)}
			},
		},
		{
			name: "a consent signed by someone else",
			code: SnFleetRefusalBadConsent,
			work: func(t *testing.T) *snFleetDeviceWork {
				consent := testingFleetConsent(t, testingFleetColdkeySeed, testingFleetNetworkId, 1)
				consent.Signature = testingFleetWalletSign(t, testingFleetOtherColdkeySeed, consent.Message)
				return &snFleetDeviceWork{Consent: consent}
			},
		},
		{
			name: "a binding that doesn't parse",
			code: SnFleetRefusalBadBinding,
			work: func(t *testing.T) *snFleetDeviceWork {
				work := testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 1)
				work.BindingJson = strings.TrimSuffix(work.BindingJson, "}") + `,"Hotkey":"0xaa"}`
				return work
			},
		},
		{
			name: "a device that left",
			code: SnFleetRefusalLeft,
			work: func(t *testing.T) *snFleetDeviceWork {
				return testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 1)
			},
			before: func(state *snFleetDeviceState) { state.Left = true },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &testingFleetMemoryStore{}
			state := &snFleetDeviceState{Schema: snFleetDeviceStateSchema, NetworkId: testingFleetNetworkId, ClientId: testingFleetClientId}
			if !c.unpin {
				state.Coldkey = snFleetHex(pinnedColdkey[:])
				state.ConsentGeneration = 1
				state.Hotkey = snFleetHex(pinnedHotkey[:])
			}
			if c.before != nil {
				c.before(state)
			}
			server := &testingFleetServer{work: c.work(t)}
			signer := newTestingFleetSigner(t)
			device := newTestingFleetDevice(t, server, signer, store)
			if !device.saveState(state) {
				t.Fatal("save")
			}
			savedBefore := store.saved

			result, err := device.CheckSync(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.Action != SnFleetDeviceActionRefused || result.RefusalCode != c.code {
				t.Fatalf("result %+v, want %s", result, c.code)
			}
			if result.RefusalMessage == "" {
				t.Fatal("the device didn't say why")
			}
			if signer.bindings != 0 {
				t.Fatal("the device signed")
			}
			for _, sent := range server.signatures {
				if sent.ClientSignature != "" {
					t.Fatal("a signature went out")
				}
			}
			if server.work.BindingJson != "" && (len(server.signatures) != 1 || server.signatures[0].Refusal != c.code) {
				t.Fatalf("the server wasn't told: %+v", server.signatures)
			}
			if store.saved != savedBefore {
				t.Fatalf("a refusal moved the saved state:\n%s\n%s", savedBefore, store.saved)
			}
		})
	}
}

// A provider that restarts mid-binding doesn't sign twice: the new process sends
// what the old one saved.
func TestSnFleetDeviceRestartMidBindingDoesNotSignTwice(t *testing.T) {
	store := &testingFleetMemoryStore{}
	work := testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 1)
	work.Consent = testingFleetConsent(t, testingFleetColdkeySeed, testingFleetNetworkId, 1)
	server := &testingFleetServer{store: store, work: work, signErr: errors.New("the connection dropped")}
	first := newTestingFleetSigner(t)

	result, err := newTestingFleetDevice(t, server, first, store).CheckSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != SnFleetDeviceActionSigned || !result.SendFailed {
		t.Fatalf("first run %+v", result)
	}
	sent := server.signatures[0].ClientSignature

	// the process is gone; the store and the server's offer remain
	server.signErr = nil
	second := newTestingFleetSigner(t)
	result, err = newTestingFleetDevice(t, server, second, store).CheckSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != SnFleetDeviceActionResent || result.SendFailed {
		t.Fatalf("second run %+v", result)
	}
	if second.bindings != 0 {
		t.Fatalf("the restarted device signed %d times", second.bindings)
	}
	if len(server.signatures) != 2 || server.signatures[1].ClientSignature != sent {
		t.Fatalf("sent %+v", server.signatures)
	}
	if len(store.state(t).Signed) != 1 {
		t.Fatal("the restart added a second record")
	}
}

// A signature that can't be saved doesn't leave: otherwise a restart couldn't know.
func TestSnFleetDeviceSignatureLeavesOnlyAfterItIsSaved(t *testing.T) {
	store := &testingFleetMemoryStore{refuse: true}
	work := testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 1)
	work.Consent = testingFleetConsent(t, testingFleetColdkeySeed, testingFleetNetworkId, 1)
	server := &testingFleetServer{work: work}
	if _, err := newTestingFleetDevice(t, server, newTestingFleetSigner(t), store).CheckSync(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if len(server.signatures) != 0 {
		t.Fatal("a signature left unsaved")
	}
}

// A different binding of a generation the device signed, or of an older one, isn't
// signed; the next generation is. That's what renewing looks like.
func TestSnFleetDeviceOneBindingPerGeneration(t *testing.T) {
	store := &testingFleetMemoryStore{}
	work := testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 2)
	work.Consent = testingFleetConsent(t, testingFleetColdkeySeed, testingFleetNetworkId, 1)
	server := &testingFleetServer{store: store, work: work}
	signer := newTestingFleetSigner(t)
	device := newTestingFleetDevice(t, server, signer, store)
	if result, err := device.CheckSync(context.Background()); err != nil || result.Action != SnFleetDeviceActionSigned {
		t.Fatalf("%+v %v", result, err)
	}

	hotkey := testingFleetPublic(t, testingFleetHotkeySeed)
	offer := func(generation uint64, from uint64) *snFleetDeviceWork {
		manifest := testingFleetManifest(t, hotkey, generation)
		binding, err := manifest.Binding(manifest.Members[0], from, from+11)
		if err != nil {
			t.Fatal(err)
		}
		signature, err := protocol.SignFleetBindingWithHotkey(testingFleetHotkeySeed, binding)
		if err != nil {
			t.Fatal(err)
		}
		return &snFleetDeviceWork{
			Authorization:   testingFleetAuthorization(t, testingFleetColdkeySeed, hotkey, testingFleetNetworkId),
			BindingJson:     testingFleetBindingJson(t, binding),
			HotkeySignature: snFleetHex(signature),
		}
	}
	for name, work := range map[string]*snFleetDeviceWork{
		"the same generation, another window": offer(2, 50),
		"an older generation":                 offer(1, 60),
	} {
		server.work = work
		result, err := device.CheckSync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.RefusalCode != SnFleetRefusalGenerationReplayed {
			t.Fatalf("%s: %+v", name, result)
		}
	}
	if signer.bindings != 1 {
		t.Fatalf("signed %d", signer.bindings)
	}
	server.work = offer(3, 70)
	if result, err := device.CheckSync(context.Background()); err != nil || result.Action != SnFleetDeviceActionSigned || result.Generation != 3 {
		t.Fatalf("renewal: %+v %v", result, err)
	}
}

// A failed read of the state isn't a new device: it would pin whatever came next.
func TestSnFleetDeviceUnreadableStateStops(t *testing.T) {
	store := &testingFleetMemoryStore{unreadable: "unavailable"}
	server := &testingFleetServer{work: &snFleetDeviceWork{Consent: testingFleetConsent(t, testingFleetColdkeySeed, testingFleetNetworkId, 1)}}
	result, err := newTestingFleetDevice(t, server, newTestingFleetSigner(t), store).CheckSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.RefusalCode != SnFleetRefusalStateUnreadable {
		t.Fatalf("%+v", result)
	}
	if store.saves != 0 || len(server.workAsked) != 0 {
		t.Fatal("it acted on an unreadable state")
	}
}

// Reset lets a person accept a real change: the next consent is a first sight. What
// it signed stays, so it still won't sign two bindings for one generation.
func TestSnFleetDeviceResetAcceptsAChangedColdkey(t *testing.T) {
	store := &testingFleetMemoryStore{}
	work := testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 1)
	work.Consent = testingFleetConsent(t, testingFleetColdkeySeed, testingFleetNetworkId, 1)
	server := &testingFleetServer{store: store, work: work}
	signer := newTestingFleetSigner(t)
	device := newTestingFleetDevice(t, server, signer, store)
	if _, err := device.CheckSync(context.Background()); err != nil {
		t.Fatal(err)
	}

	server.work = testingFleetWork(t, testingFleetOtherColdkeySeed, testingFleetOtherHotkeySeed, 2)
	if result, _ := device.CheckSync(context.Background()); result.RefusalCode != SnFleetRefusalColdkeyChanged {
		t.Fatalf("%+v", result)
	}
	if err := device.Reset(); err != nil {
		t.Fatal(err)
	}
	if state := store.state(t); state.Coldkey != "" || state.Hotkey != "" || len(state.Signed) != 1 {
		t.Fatalf("state %+v", state)
	}
	work = testingFleetWork(t, testingFleetOtherColdkeySeed, testingFleetOtherHotkeySeed, 2)
	work.Consent = testingFleetConsent(t, testingFleetOtherColdkeySeed, testingFleetNetworkId, 2)
	server.work = work
	if result, err := device.CheckSync(context.Background()); err != nil || result.Action != SnFleetDeviceActionSigned {
		t.Fatalf("%+v %v", result, err)
	}
	if !server.workAsked[len(server.workAsked)-1] {
		t.Fatal("a reset device did not ask for the consent")
	}
}

func TestSnFleetDeviceNextCheckBounds(t *testing.T) {
	device := newTestingFleetDevice(t, &testingFleetServer{}, newTestingFleetSigner(t), &testingFleetMemoryStore{})
	// random answers the middle of the window, which is the chosen delay
	for name, c := range map[string]struct {
		seconds int64
		want    time.Duration
	}{
		"no answer":   {0, snFleetDeviceCheckDefault},
		"a negative":  {-5, snFleetDeviceCheckDefault},
		"too eager":   {1, snFleetDeviceCheckMin},
		"in bounds":   {21600, 6 * time.Hour},
		"too patient": {365 * 86400, snFleetDeviceCheckMax},
	} {
		if got := device.nextCheck(c.seconds); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
	// the jitter stays within a quarter either way
	device.random = func(int64) int64 { return 0 }
	if got := device.nextCheck(21600); got != 6*time.Hour-90*time.Minute {
		t.Fatalf("low %s", got)
	}
	device.random = func(n int64) int64 { return n - 1 }
	if got := device.nextCheck(21600); got != 6*time.Hour+90*time.Minute-1 {
		t.Fatalf("high %s", got)
	}
}

// A fleet restarting together doesn't ask together, and a failure never asks sooner.
func TestSnFleetDeviceRunIsSlowAndJittered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &testingFleetMemoryStore{}
	server := &testingFleetServer{workErr: &connect.HttpStatusError{StatusCode: 404}}
	device := newTestingFleetDevice(t, server, newTestingFleetSigner(t), store)
	delays := []time.Duration{}
	device.sleep = func(ctx context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		if len(delays) == 2 {
			// the route exists now, and the server names its cadence
			server.workErr = nil
			server.work = &snFleetDeviceWork{NextCheckSeconds: 21600}
		}
		if len(delays) == 4 {
			cancel()
			return false
		}
		return true
	}
	checked := 0
	device.RunWithContext(ctx, testingFleetListener(func(*SnFleetDeviceCheckResult) { checked += 1 }))
	want := []time.Duration{12 * time.Hour, snFleetDeviceCheckDefault, 6 * time.Hour, 6 * time.Hour}
	if len(delays) != len(want) {
		t.Fatalf("delays %v", delays)
	}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("delays %v, want %v", delays, want)
		}
	}
	if len(server.workAsked) != 3 || checked != 2 {
		t.Fatalf("asked %d, heard %d", len(server.workAsked), checked)
	}
}

type testingFleetListener func(*SnFleetDeviceCheckResult)

func (self testingFleetListener) SnFleetDeviceChecked(result *SnFleetDeviceCheckResult) {
	self(result)
}

// Leave needs only the client key and goes through the relay. From then on the
// device signs nothing.
func TestSnFleetDeviceLeave(t *testing.T) {
	store := &testingFleetMemoryStore{}
	binding := &snFleetDeviceBinding{Bound: true, ChainId: 964, Netuid: 25, Coordinator: testingFleetCoordinatorHex(), Generation: 3, ValidFromEpoch: 5, ValidToEpoch: 16, CurrentEpoch: 7}
	server := &testingFleetServer{binding: binding}
	signer := newTestingFleetSigner(t)
	device := newTestingFleetDevice(t, server, signer, store)

	result, err := device.LeaveSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Left || result.EffectiveEpoch != 8 {
		t.Fatalf("%+v", result)
	}
	// Leave signs with the client key: the disclaimer's last sentence
	testingFleetExpectDisclaimer(t, "leave", result.FundsDisclaimerKey, result.FundsDisclaimer, false)
	if len(server.leaves) != 1 || signer.revokes != 1 {
		t.Fatalf("leaves %+v", server.leaves)
	}
	sent := server.leaves[0]
	if sent.Generation != 3 || sent.EffectiveEpoch != 8 {
		t.Fatalf("sent %+v", sent)
	}
	revoke := protocol.FleetRevoke{ChainID: 964, Netuid: 25, Coordinator: testingFleetCoordinator(), ClientID: [16]byte(testingFleetId(t, testingFleetClientId)), Generation: 3, EffectiveEpoch: 8}
	signature, err := snFleetParseHex(sent.ClientSignature)
	if err != nil || !revoke.VerifyClient(ed25519.NewKeyFromSeed(testingFleetClientSeed).Public().(ed25519.PublicKey), signature) {
		t.Fatalf("revoke signature: %v", err)
	}
	if !store.state(t).Left {
		t.Fatal("the device doesn't remember it left")
	}

	server.work = testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 4)
	server.work.Consent = testingFleetConsent(t, testingFleetColdkeySeed, testingFleetNetworkId, 1)
	if checked, err := device.CheckSync(context.Background()); err != nil || checked.RefusalCode != SnFleetRefusalLeft {
		t.Fatalf("%+v %v", checked, err)
	}
	if signer.bindings != 0 {
		t.Fatal("a device that left signed")
	}
}

// Leaving stops signing first, so a relay that's down can't leave it signing.
func TestSnFleetDeviceLeaveStopsSigningEvenIfTheRelayFails(t *testing.T) {
	store := &testingFleetMemoryStore{}
	server := &testingFleetServer{
		binding: &snFleetDeviceBinding{Bound: true, ChainId: 964, Netuid: 25, Coordinator: testingFleetCoordinatorHex(), Generation: 3, ValidFromEpoch: 5, ValidToEpoch: 16, CurrentEpoch: 7},
		leave: func(*snFleetDeviceLeaveArgs) *snFleetDeviceLeaveResult {
			return &snFleetDeviceLeaveResult{Error: &SnError{Message: "the relay has no funded key"}}
		},
	}
	device := newTestingFleetDevice(t, server, newTestingFleetSigner(t), store)
	if _, err := device.LeaveSync(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if !store.state(t).Left {
		t.Fatal("the device would go on signing")
	}
}

// An epoch that turns over during the request gets a new signature, once.
func TestSnFleetDeviceLeaveAcrossAnEpochBoundary(t *testing.T) {
	binding := &snFleetDeviceBinding{Bound: true, ChainId: 964, Netuid: 25, Coordinator: testingFleetCoordinatorHex(), Generation: 3, ValidFromEpoch: 5, ValidToEpoch: 16, CurrentEpoch: 7}
	server := &testingFleetServer{binding: binding}
	server.leave = func(args *snFleetDeviceLeaveArgs) *snFleetDeviceLeaveResult {
		if args.EffectiveEpoch == 8 {
			server.binding = &snFleetDeviceBinding{Bound: true, ChainId: 964, Netuid: 25, Coordinator: binding.Coordinator, Generation: 3, ValidFromEpoch: 5, ValidToEpoch: 16, CurrentEpoch: 8}
			return &snFleetDeviceLeaveResult{CurrentEpoch: 8}
		}
		return &snFleetDeviceLeaveResult{Left: true, EffectiveEpoch: args.EffectiveEpoch}
	}
	result, err := newTestingFleetDevice(t, server, newTestingFleetSigner(t), &testingFleetMemoryStore{}).LeaveSync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.EffectiveEpoch != 9 || len(server.leaves) != 2 {
		t.Fatalf("%+v %+v", result, server.leaves)
	}
	testingFleetExpectDisclaimer(t, "leave after the epoch turned", result.FundsDisclaimerKey, result.FundsDisclaimer, false)
}

// A device with nothing on chain, or a binding already in its last epoch, has nothing
// to revoke: the server only stops choosing it.
func TestSnFleetDeviceLeaveWithNothingToRevoke(t *testing.T) {
	for name, binding := range map[string]*snFleetDeviceBinding{
		"never bound":   nil,
		"ending anyway": {Bound: true, ChainId: 964, Netuid: 25, Coordinator: testingFleetCoordinatorHex(), Generation: 3, ValidFromEpoch: 5, ValidToEpoch: 16, CurrentEpoch: 16},
	} {
		server := &testingFleetServer{binding: binding}
		signer := newTestingFleetSigner(t)
		result, err := newTestingFleetDevice(t, server, signer, &testingFleetMemoryStore{}).LeaveSync(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !result.Left || signer.revokes != 0 || server.leaves[0].ClientSignature != "" {
			t.Fatalf("%s: %+v, %d revokes", name, result, signer.revokes)
		}
	}
}

// A saved binding names the deployment. If the server names another, the device
// doesn't sign a revocation for it.
func TestSnFleetDeviceLeaveRefusesAnotherDeployment(t *testing.T) {
	store := &testingFleetMemoryStore{}
	work := testingFleetWork(t, testingFleetColdkeySeed, testingFleetHotkeySeed, 3)
	work.Consent = testingFleetConsent(t, testingFleetColdkeySeed, testingFleetNetworkId, 1)
	server := &testingFleetServer{store: store, work: work}
	signer := newTestingFleetSigner(t)
	device := newTestingFleetDevice(t, server, signer, store)
	if _, err := device.CheckSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	server.binding = &snFleetDeviceBinding{Bound: true, ChainId: 964, Netuid: 25, Coordinator: "0x" + hex.EncodeToString(make([]byte, 19)) + "01", Generation: 3, ValidFromEpoch: 15, ValidToEpoch: 26, CurrentEpoch: 15}
	if _, err := device.LeaveSync(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if signer.revokes != 0 {
		t.Fatal("it signed a revocation for another deployment")
	}
}

// SignSnFleetRevoke runs where the key is, and signs only this client's id.
func TestSnFleetRevokeSignsOnlyThisClient(t *testing.T) {
	signer := newTestingFleetSigner(t)
	coordinator := testingFleetCoordinatorHex()
	good := `{"chain_id":964,"netuid":25,"coordinator":"` + coordinator + `","client_id":"` + testingFleetClientId + `","generation":3,"effective_epoch":8}`
	if signature, err := signer.SignSnFleetRevoke(good); err != nil || signature == "" {
		t.Fatalf("%s %v", signature, err)
	}
	for name, text := range map[string]string{
		"another client": strings.Replace(good, testingFleetClientId, "019b2e10-0000-4d3e-8f01-0000000000c2", 1),
		"unknown field":  strings.Replace(good, `"generation":3`, `"generation":3,"note":"x"`, 1),
		"no generation":  strings.Replace(good, `"generation":3,`, ``, 1),
		"zero epoch":     strings.Replace(good, `"effective_epoch":8`, `"effective_epoch":0`, 1),
		"short key":      strings.Replace(good, coordinator, "0x01", 1),
	} {
		if signature, err := signer.SignSnFleetRevoke(text); err == nil || signature != "" {
			t.Errorf("%s: signed %s", name, signature)
		}
	}
}
