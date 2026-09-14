package sdk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The provider extender surface every platform carries (EXTENDER.md F3, D5,
// E1). The role itself is desktop only, so these pin the parts an ios, android
// or js binary still compiles and calls: the status text, the persisted
// settings and the disabled answers.

// The bind failures are one string in carrier order, so the text an app prints
// is stable across reads whatever order the map hands them back (F3).
func TestExtenderListenErrorText(t *testing.T) {
	tcpErr := errors.New("tcp bind refused")
	quicErr := errors.New("quic bind refused")
	dnsErr := errors.New("dns bind refused")

	cases := []struct {
		name        string
		carrierErrs map[string]error
		expect      string
	}{
		{name: "every carrier bound", carrierErrs: nil, expect: ""},
		{name: "empty map", carrierErrs: map[string]error{}, expect: ""},
		{
			name:        "one carrier",
			carrierErrs: map[string]error{connect.ExtenderCarrierTcp: tcpErr},
			expect:      "tcp: tcp bind refused",
		},
		{
			name: "every carrier, in carrier order",
			carrierErrs: map[string]error{
				connect.ExtenderCarrierDns:  dnsErr,
				connect.ExtenderCarrierQuic: quicErr,
				connect.ExtenderCarrierTcp:  tcpErr,
			},
			expect: "tcp: tcp bind refused; quic: quic bind refused; dns: dns bind refused",
		},
		{
			name: "a carrier with no error is not a failure",
			carrierErrs: map[string]error{
				connect.ExtenderCarrierTcp:  nil,
				connect.ExtenderCarrierQuic: quicErr,
			},
			expect: "quic: quic bind refused",
		},
		{
			name:        "a carrier this build does not know is left out",
			carrierErrs: map[string]error{"other": tcpErr},
			expect:      "",
		},
	}
	for _, c := range cases {
		if listenError := extenderListenErrorText(c.carrierErrs); listenError != c.expect {
			t.Errorf("%s: listen error = %q, expected %q", c.name, listenError, c.expect)
		}
	}
}

// The bound dns ports are the order a client dials them in (F3, L2).
func TestExtenderDnsPortsText(t *testing.T) {
	cases := []struct {
		name     string
		dnsPorts []int
		expect   string
	}{
		{name: "not listening", dnsPorts: nil, expect: ""},
		{name: "the unprivileged port alone", dnsPorts: []int{4053}, expect: "4053"},
		{name: "both, 53 first", dnsPorts: []int{53, 4053}, expect: "53,4053"},
	}
	for _, c := range cases {
		if dnsPorts := extenderDnsPortsText(c.dnsPorts); dnsPorts != c.expect {
			t.Errorf("%s: dns ports = %q, expected %q", c.name, dnsPorts, c.expect)
		}
	}
}

// The one state rule every app renders (N3), case by case and in order. The
// order is the rule: the same status reads as a different state depending on
// which earlier case it matches, so every pair the design orders is pinned
// here with a status that matches both. Every case also pins the error case,
// which is empty in every state but error.
func TestExtenderProvideStateRule(t *testing.T) {
	// the fields of a role that is up and activated on both families, which
	// the precedence cases below start from
	active := func() *ExtenderProvideStatus {
		return &ExtenderProvideStatus{
			Supported:   true,
			Enabled:     true,
			Listening:   true,
			ActivatedV4: true,
			ActivatedV6: true,
			Ipv4:        "192.0.2.10",
			Ipv6:        "2001:db8::a",
		}
	}

	cases := []struct {
		name            string
		status          *ExtenderProvideStatus
		provideExtender bool
		providing       bool
		expectState     string
		expectErrorCase string
		expectReason    string
	}{
		// off
		{
			name:            "the setting is off",
			status:          &ExtenderProvideStatus{Supported: true},
			provideExtender: false,
			providing:       true,
			expectState:     ExtenderProvideStateOff,
		},
		{
			name:            "the setting is off while the role still runs",
			status:          active(),
			provideExtender: false,
			providing:       true,
			expectState:     ExtenderProvideStateOff,
		},
		{
			name:            "a build with no role, whatever the setting says",
			status:          &ExtenderProvideStatus{},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateOff,
		},
		{
			name:            "the setting off beats not providing",
			status:          &ExtenderProvideStatus{Supported: true},
			provideExtender: false,
			providing:       false,
			expectState:     ExtenderProvideStateOff,
		},
		// not_providing
		{
			name:            "the setting is on and the device is not providing",
			status:          &ExtenderProvideStatus{Supported: true},
			provideExtender: true,
			providing:       false,
			expectState:     ExtenderProvideStateNotProviding,
		},
		{
			name: "not providing beats everything the role last reported",
			status: func() *ExtenderProvideStatus {
				status := active()
				status.RevokedTime = 1
				status.ListenError = "tcp: bind refused"
				return status
			}(),
			provideExtender: true,
			providing:       false,
			expectState:     ExtenderProvideStateNotProviding,
		},
		// error, start
		{
			name: "the role was asked for and could not start",
			status: &ExtenderProvideStatus{
				Supported:  true,
				StartError: "the network space has no extender directory",
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateError,
			expectErrorCase: ExtenderProvideErrorStart,
			expectReason:    "the network space has no extender directory",
		},
		{
			name: "a start error beats a stale revocation",
			status: &ExtenderProvideStatus{
				Supported:   true,
				StartError:  "the extender identity is not usable: bad seed",
				RevokedTime: 1757000000000,
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateError,
			expectErrorCase: ExtenderProvideErrorStart,
			expectReason:    "the extender identity is not usable: bad seed",
		},
		{
			name: "not providing beats a start error",
			status: &ExtenderProvideStatus{
				Supported:  true,
				StartError: "the network space has no extender directory",
			},
			provideExtender: true,
			providing:       false,
			expectState:     ExtenderProvideStateNotProviding,
		},
		{
			name: "a role that runs has no start error to report",
			status: &ExtenderProvideStatus{
				Supported:  true,
				Enabled:    true,
				Listening:  true,
				StartError: "the network space has no extender directory",
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateSettingUp,
		},
		// error, revoked
		{
			name: "the operator revoked the key",
			status: &ExtenderProvideStatus{
				Supported:   true,
				Enabled:     true,
				Listening:   true,
				RevokedTime: 1757000000000,
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateError,
			expectErrorCase: ExtenderProvideErrorRevoked,
			// the case is the whole message; the app names it (N5)
			expectReason: "",
		},
		{
			name: "revoked beats an activated family",
			status: func() *ExtenderProvideStatus {
				status := active()
				status.RevokedTime = 1757000000000
				return status
			}(),
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateError,
			expectErrorCase: ExtenderProvideErrorRevoked,
		},
		{
			name: "revoked beats a carrier bind failure",
			status: &ExtenderProvideStatus{
				Supported:   true,
				Enabled:     true,
				ListenError: "tcp: bind refused; quic: bind refused; dns: bind refused",
				RevokedTime: 1757000000000,
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateError,
			expectErrorCase: ExtenderProvideErrorRevoked,
		},
		{
			name: "revoked beats a standing activation error",
			status: &ExtenderProvideStatus{
				Supported:           true,
				Enabled:             true,
				Listening:           true,
				LastActivationTime:  1757000000000,
				LastActivationError: "post activate: connection refused",
				RevokedTime:         1757000001000,
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateError,
			expectErrorCase: ExtenderProvideErrorRevoked,
		},
		// active
		{
			name:            "both families",
			status:          active(),
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateActive,
		},
		{
			name: "one family",
			status: &ExtenderProvideStatus{
				Supported:   true,
				Enabled:     true,
				Listening:   true,
				ActivatedV4: true,
				Ipv4:        "192.0.2.10",
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateActive,
		},
		{
			// active is not an error state, so the other family's refusal is
			// the reason alone, with no case
			name: "one family, with the other family's refusal beside it",
			status: &ExtenderProvideStatus{
				Supported:           true,
				Enabled:             true,
				Listening:           true,
				ActivatedV6:         true,
				Ipv6:                "2001:db8::a",
				LastActivationTime:  1757000000000,
				LastActivationError: "the operator refused the activation",
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateActive,
			expectReason:    "the operator refused the activation",
		},
		{
			name: "an activated family beats a carrier that did not bind",
			status: func() *ExtenderProvideStatus {
				status := active()
				status.Listening = false
				status.ListenError = "tcp: bind refused"
				return status
			}(),
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateActive,
		},
		// error, listen
		{
			name: "no carrier bound",
			status: &ExtenderProvideStatus{
				Supported:   true,
				Enabled:     true,
				ListenError: "tcp: bind refused; quic: bind refused; dns: bind refused",
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateError,
			expectErrorCase: ExtenderProvideErrorListen,
			expectReason:    "tcp: bind refused; quic: bind refused; dns: bind refused",
		},
		{
			name: "the bind failure is reported before an activation that never ran",
			status: &ExtenderProvideStatus{
				Supported:           true,
				Enabled:             true,
				ListenError:         "tcp: bind refused",
				LastActivationTime:  1757000000000,
				LastActivationError: "the operator refused the activation",
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateError,
			expectErrorCase: ExtenderProvideErrorListen,
			expectReason:    "tcp: bind refused",
		},
		// error, activation
		{
			name: "the last activation failed",
			status: &ExtenderProvideStatus{
				Supported:           true,
				Enabled:             true,
				Listening:           true,
				LastActivationTime:  1757000000000,
				LastActivationError: "post activate: connection refused",
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateError,
			expectErrorCase: ExtenderProvideErrorActivationFailed,
			expectReason:    "post activate: connection refused",
		},
		{
			// the activator does not yet report a refusal apart from a failed
			// request, so a refusal is a failure until it does; this case
			// becomes activation_refused when the status carries the refusal
			name: "still an error through the backoff after a refusal",
			status: &ExtenderProvideStatus{
				Supported:           true,
				Enabled:             true,
				Listening:           true,
				LastActivationTime:  1757000000000,
				LastActivationError: "the operator refused the activation",
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateError,
			expectErrorCase: ExtenderProvideErrorActivationFailed,
			expectReason:    "the operator refused the activation",
		},
		// setting_up
		{
			name: "the carriers are still binding",
			status: &ExtenderProvideStatus{
				Supported: true,
				Enabled:   true,
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateSettingUp,
		},
		{
			name: "bound, and the first activation is in flight",
			status: &ExtenderProvideStatus{
				Supported: true,
				Enabled:   true,
				Listening: true,
				DnsPorts:  "53,4053",
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateSettingUp,
		},
		{
			name: "a carrier that failed while another bound is not a listen error",
			status: &ExtenderProvideStatus{
				Supported:   true,
				Enabled:     true,
				Listening:   true,
				ListenError: "dns: bind refused",
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateSettingUp,
		},
		{
			name: "an attempt that recorded no error is not an error",
			status: &ExtenderProvideStatus{
				Supported:          true,
				Enabled:            true,
				Listening:          true,
				LastActivationTime: 1757000000000,
			},
			provideExtender: true,
			providing:       true,
			expectState:     ExtenderProvideStateSettingUp,
		},
	}
	for _, c := range cases {
		state, errorCase, reason := extenderProvideStateRule(
			c.status,
			c.provideExtender,
			c.providing,
		)
		if state != c.expectState {
			t.Errorf("%s: state = %q, expected %q", c.name, state, c.expectState)
		}
		if errorCase != c.expectErrorCase {
			t.Errorf("%s: error case = %q, expected %q", c.name, errorCase, c.expectErrorCase)
		}
		if reason != c.expectReason {
			t.Errorf("%s: reason = %q, expected %q", c.name, reason, c.expectReason)
		}
		// the case names an error and nothing else
		if (state == ExtenderProvideStateError) != (errorCase != "") {
			t.Errorf("%s: state %q carries error case %q", c.name, state, errorCase)
		}
	}

	// the rule is pure: reading it does not rewrite the status it read
	status := active()
	before := *status
	extenderProvideStateRule(status, true, true)
	if *status != before {
		t.Fatalf("the state rule mutated the status it read: %+v", status)
	}
	// and a status a caller never filled in is off rather than a panic
	if state, errorCase, reason := extenderProvideStateRule(nil, true, true); state !=
		ExtenderProvideStateOff || errorCase != "" || reason != "" {
		t.Fatalf("no status = %q, %q, %q, expected off", state, errorCase, reason)
	}
}

// What a build that carries no role answers (G1, N2): an ios, android or js
// device runs the stub, whose disabled status carries `Supported` false
// through the same derivation, and that is exactly the unsupported status the
// rpc reader falls back to. The two must not drift, or an app hides the row on
// one platform and draws a dead one on another.
func TestExtenderProvideStubAnswersTheUnsupportedStatus(t *testing.T) {
	stub := disabledExtenderProvideStatus()
	// the stub's build sets this false; here it is set explicitly, so the
	// derivation under test is the one that runs on a phone
	stub.Supported = false
	stub.State, stub.ErrorCase, stub.Reason = extenderProvideStateRule(stub, true, true)

	if stub.Supported {
		t.Fatal("the status of a build with no role reported the role supported")
	}
	if stub.State != ExtenderProvideStateOff {
		t.Fatalf("state = %q, expected off", stub.State)
	}
	if !reflect.DeepEqual(stub, unsupportedExtenderProvideStatus()) {
		t.Fatalf(
			"the stub's status is %+v and the rpc reader's unsupported status is %+v",
			stub,
			unsupportedExtenderProvideStatus(),
		)
	}
}

// Every field of the published state reaches the status an app renders. A
// field added to the state and not mapped here is dropped silently, which is
// the same defect the rpc mirror test exists for (F3).
func TestExtenderProvideStatusCarriesEveryPublishedField(t *testing.T) {
	seed := 0
	state := extenderProvideState{}
	fillNonZero(t, reflect.ValueOf(&state), &seed)

	status := state.status(7)
	if status.ConnectionCount != 7 {
		t.Fatalf("connection count = %d, expected the live count", status.ConnectionCount)
	}
	stateValue := reflect.ValueOf(state)
	statusValue := reflect.ValueOf(*status)
	for i := range stateValue.NumField() {
		name := stateValue.Type().Field(i).Name
		statusField := statusValue.FieldByName(name)
		if !statusField.IsValid() {
			t.Errorf("the status carries no %s", name)
			continue
		}
		if !reflect.DeepEqual(statusField.Interface(), stateValue.Field(i).Interface()) {
			t.Errorf(
				"%s = %v, expected the published %v",
				name,
				statusField.Interface(),
				stateValue.Field(i).Interface(),
			)
		}
	}

	// the disabled status is every field at rest, which is what a build with
	// no role and a device that is not providing both report
	if disabled := disabledExtenderProvideStatus(); !reflect.DeepEqual(
		*disabled,
		ExtenderProvideStatus{},
	) {
		t.Fatalf("disabled status = %+v, expected nothing set", disabled)
	}
}

// A device that runs no provider runs no role, and reports the disabled status
// rather than nothing (F3, G1).
func TestDeviceLocalExtenderProvideStatusWithoutAProvider(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	settings := testExtenderStatusDeviceSettings()
	settings.AllowProvider = false
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), settings, connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceLocal.Close)

	status := deviceLocal.GetExtenderProvideStatus()
	if status == nil {
		t.Fatal("a device with no provider reported no status at all")
	}
	if status.Enabled || status.Listening {
		t.Fatalf("status = %+v, expected disabled", status)
	}
	// the listener is inert rather than nil, and the setting still answers
	sub := deviceLocal.AddExtenderProvideStatusChangeListener(
		extenderProvideStatusChangeListenerFunc(func(status *ExtenderProvideStatus) {}),
	)
	deviceLocal.SetProvideExtender(false)
	if deviceLocal.GetProvideExtender() {
		t.Fatal("the opt-out was not persisted")
	}
	deviceLocal.SetProvideExtender(true)
	if !deviceLocal.GetProvideExtender() {
		t.Fatal("the setting did not go back on")
	}
	sub.Close()

	// and the status is still the disabled one after a device close
	deviceLocal.Close()
	if status := deviceLocal.GetExtenderProvideStatus(); status == nil || status.Enabled {
		t.Fatalf("status after close = %+v, expected disabled", status)
	}
}

type extenderProvideStatusChangeListenerFunc func(status *ExtenderProvideStatus)

func (self extenderProvideStatusChangeListenerFunc) ExtenderProvideStatusChanged(
	status *ExtenderProvideStatus,
) {
	self(status)
}

// The three gossip modes, and everything else read as auto so a newer app's
// mode degrades to the platform rule rather than sticking (D5).
func TestNormalExtenderGossipMode(t *testing.T) {
	cases := []struct {
		mode   string
		expect string
	}{
		{mode: ExtenderGossipModeAuto, expect: ExtenderGossipModeAuto},
		{mode: ExtenderGossipModeFeed, expect: ExtenderGossipModeFeed},
		{mode: ExtenderGossipModeMember, expect: ExtenderGossipModeMember},
		{mode: " Member ", expect: ExtenderGossipModeMember},
		{mode: "FEED", expect: ExtenderGossipModeFeed},
		{mode: "", expect: ExtenderGossipModeAuto},
		{mode: "   ", expect: ExtenderGossipModeAuto},
		{mode: "swarm", expect: ExtenderGossipModeAuto},
	}
	for _, c := range cases {
		if mode := NormalExtenderGossipMode(c.mode); mode != c.expect {
			t.Errorf("%q: mode = %q, expected %q", c.mode, mode, c.expect)
		}
	}
}

// The four dot files of the extender state, each read through its own
// tolerance: the directory is a cache, the mode degrades to auto, and the
// opt-out is on unless it was explicitly turned off (E1, D5, F3).
func TestExtenderLocalStateFiles(t *testing.T) {
	localStorageHome := t.TempDir()
	localState := newLocalState(context.Background(), localStorageHome)
	t.Cleanup(localState.Close)
	path := func(name string) string {
		return filepath.Join(localState.localStorageDir, name)
	}

	// the directory: absent is no directory at all, not an error
	stateBytes, err := localState.getExtenders()
	if err != nil || stateBytes != nil {
		t.Fatalf("absent directory = %q, %v", stateBytes, err)
	}
	if err := localState.setExtenders([]byte(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	if stateBytes, err = localState.getExtenders(); err != nil ||
		string(stateBytes) != `{"version":1}` {
		t.Fatalf("stored directory = %q, %v", stateBytes, err)
	}
	// an empty envelope removes the file, so a cleared directory leaves no
	// stale entries behind for the next launch
	if err := localState.setExtenders(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path(extenderStoreFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the cleared directory left a file: %v", err)
	}
	if stateBytes, err = localState.getExtenders(); err != nil || stateBytes != nil {
		t.Fatalf("cleared directory = %q, %v", stateBytes, err)
	}

	// the gossip mode
	if mode := localState.GetExtenderGossipMode(); mode != ExtenderGossipModeAuto {
		t.Fatalf("unset mode = %q, expected auto", mode)
	}
	if err := localState.SetExtenderGossipMode(ExtenderGossipModeMember); err != nil {
		t.Fatal(err)
	}
	if mode := localState.GetExtenderGossipMode(); mode != ExtenderGossipModeMember {
		t.Fatalf("mode = %q, expected member", mode)
	}
	// a mode this build does not know is stored as auto rather than refused,
	// so it does not leave the previous mode in force
	if err := localState.SetExtenderGossipMode("swarm"); err != nil {
		t.Fatal(err)
	}
	if mode := localState.GetExtenderGossipMode(); mode != ExtenderGossipModeAuto {
		t.Fatalf("mode = %q, expected auto", mode)
	}
	// and a file written by something else reads as auto
	if err := os.WriteFile(
		path(extenderGossipModeFileName),
		[]byte("  MEMBER\n"),
		LocalStorageFilePermissions,
	); err != nil {
		t.Fatal(err)
	}
	if mode := localState.GetExtenderGossipMode(); mode != ExtenderGossipModeMember {
		t.Fatalf("mode = %q, expected the trimmed member", mode)
	}

	// the provider opt-out: on unless it was explicitly turned off
	if !localState.GetProvideExtender() {
		t.Fatal("the unset provider extender setting read as off")
	}
	if err := localState.SetProvideExtender(false); err != nil {
		t.Fatal(err)
	}
	if localState.GetProvideExtender() {
		t.Fatal("the opt-out did not apply")
	}
	// read once and cached: with the file gone, a read answers the cached
	// value rather than going back to the disk for the default (N4)
	if err := os.Remove(path(provideExtenderFileName)); err != nil {
		t.Fatal(err)
	}
	if localState.GetProvideExtender() {
		t.Fatal("a read went back to the disk instead of answering the cached opt-out")
	}

	// every write goes to the file as well as the cache, so a restart reads
	// what the last write left
	if err := localState.SetProvideExtender(false); err != nil {
		t.Fatal(err)
	}
	restarted := newLocalState(context.Background(), localStorageHome)
	t.Cleanup(restarted.Close)
	if restarted.GetProvideExtender() {
		t.Fatal("the opt-out was not persisted")
	}
	if err := restarted.SetProvideExtender(true); err != nil {
		t.Fatal(err)
	}
	if !restarted.GetProvideExtender() {
		t.Fatal("the setting did not go back on")
	}

	// anything that is not an explicit off is on, so a corrupt file does not
	// silently opt a provider out. The file is read once, at the next start.
	if err := os.WriteFile(
		path(provideExtenderFileName),
		[]byte("nonsense"),
		LocalStorageFilePermissions,
	); err != nil {
		t.Fatal(err)
	}
	reread := newLocalState(context.Background(), localStorageHome)
	t.Cleanup(reread.Close)
	if !reread.GetProvideExtender() {
		t.Fatal("a file that is not an explicit off read as off")
	}
}

// The relayed traffic is nil whenever the role is not running, which is what
// tells an app there is no series to show (O2): not providing, providing with
// no role (the suite keeps the role disabled), the setting off, a closed
// device, and a hosted device that is asked to provide.
func TestDeviceLocalExtenderStatsNilWhileTheRoleIsNotRunning(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), testExtenderStatusDeviceSettings(), connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer deviceLocal.Close()

	if stats := deviceLocal.GetExtenderStats(); stats != nil {
		t.Fatalf("stats = %+v while not providing, expected nil", stats)
	}
	deviceLocal.SetProvideMode(ProvideModePublic)
	if stats := deviceLocal.GetExtenderStats(); stats != nil {
		t.Fatalf("stats = %+v while providing with no role, expected nil", stats)
	}
	deviceLocal.SetProvideExtender(false)
	if stats := deviceLocal.GetExtenderStats(); stats != nil {
		t.Fatalf("stats = %+v with the setting off, expected nil", stats)
	}
	deviceLocal.Close()
	if stats := deviceLocal.GetExtenderStats(); stats != nil {
		t.Fatalf("stats = %+v after close, expected nil", stats)
	}

	_, hostedSpace := testExtenderStatusSpace(t)
	hostedSettings := testExtenderStatusDeviceSettings()
	hostedSettings.HostedIncompatible = true
	hostedDevice, err := newDeviceLocalWithOverrides(
		hostedSpace, "", "", "", "", NewId(), hostedSettings, connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer hostedDevice.Close()
	hostedDevice.SetProvideMode(ProvideModePublic)
	if stats := hostedDevice.GetExtenderStats(); stats != nil {
		t.Fatalf("stats = %+v on a hosted device, expected nil", stats)
	}
}

// The provider extender status is coalesced to at most one callback per epoch,
// carrying the complete state, so a burst of bind and activation changes is one
// ui update rather than a dozen (F3).
//
// The property is a rate, so it is measured as one: changes are published
// continuously across several epochs and the callbacks are counted. A single
// burst would instead race the watch's own first subscribe -- a change
// published before the watch is armed is not an edge it ever sees -- and that
// test passes or hangs on how the goroutine happened to be scheduled.
func TestExtenderProvideStatusListenerCoalesces(t *testing.T) {
	_, networkSpace := testExtenderStatusSpace(t)
	settings := testExtenderStatusDeviceSettings()
	// no provider, so the device monitor below is the only thing that wakes
	// the watch and the published changes are exactly what the test made them
	settings.AllowProvider = false
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace, "", "", "", "", NewId(), settings, connect.NewId(),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(deviceLocal.Close)

	var callbackCount atomic.Int64
	var statusCount atomic.Int64
	sub := deviceLocal.AddExtenderProvideStatusChangeListener(
		extenderProvideStatusChangeListenerFunc(func(status *ExtenderProvideStatus) {
			callbackCount.Add(1)
			if status != nil {
				statusCount.Add(1)
			}
		}),
	)

	const epochs = 3
	publishCount := 0
	deadline := time.Now().Add(epochs * extenderProvideStatusEpoch)
	for time.Now().Before(deadline) {
		deviceLocal.extenderProvideMonitor.NotifyAll()
		publishCount += 1
		time.Sleep(50 * time.Millisecond)
	}
	// the round the last change woke is still sleeping out its epoch
	time.Sleep(extenderProvideStatusEpoch + 500*time.Millisecond)
	sub.Close()

	callbacks := callbackCount.Load()
	if callbacks == 0 {
		t.Fatal("the provider extender status listener was never called")
	}
	if statusCount.Load() != callbacks {
		t.Fatalf("%d of %d callbacks carried no status", callbacks-statusCount.Load(), callbacks)
	}
	// one per epoch, plus the trailing round; a watch that emitted per change
	// would be near publishCount, which is an order of magnitude away
	if int64(2*epochs) < callbacks {
		t.Fatalf(
			"callbacks = %d for %d changes over %d epochs, expected about one per epoch",
			callbacks,
			publishCount,
			epochs,
		)
	}
}
