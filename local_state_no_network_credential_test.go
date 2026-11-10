package sdk

// §18.7's network credential rule (D-SDK-11, I-6) under NetworkSpaceValues.SessionOnlyNetworkCredential: .auth_state
// keeps no by_jwt once a device client exists, the Api keeps the network credential in memory and renews it there,
// an admin call after a restart fails before it is sent, a client token alone keeps a person signed in, and the
// default keeps today's file byte for byte.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The json keys of home's .auth_state, with each value as written.
func testAuthStateFileFields(t *testing.T, home string) map[string]any {
	t.Helper()
	fields := map[string]any{}
	if err := json.Unmarshal(testAuthStateFile(t, home), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestNetworkCredentialDroppedOnceClientExists(t *testing.T) {
	home := t.TempDir()
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "dropped")
	localState := newTestSealedLocalState(t, home, nil, true)

	// the sign-in that mints the device client is kept until the client exists
	if err := localState.SetByJwt(networkJwt); err != nil {
		t.Fatal(err)
	}
	if localState.GetByJwt() != networkJwt {
		t.Fatal("the sign-in was dropped before a client existed")
	}
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	if localState.GetByJwt() != "" || localState.GetByClientJwt() != clientJwt || localState.GetInstanceId() == nil {
		t.Fatal("the network credential stayed beside the device client")
	}
	data := testAuthStateFile(t, home)
	requireNoSecretInAuthStateFile(t, data, networkJwt)
	// the keys hostCredentialStores scans stay, by_jwt empty
	fields := testAuthStateFileFields(t, home)
	if byJwt, ok := fields["by_jwt"]; !ok || byJwt != "" || fields["by_client_jwt"] != clientJwt {
		t.Fatalf("the file's keys changed: %v", fields)
	}

	// today's file, both credentials, is cleaned at its first read under the rule
	todayHome := t.TempDir()
	today := newTestSealedLocalState(t, todayHome, nil, false)
	if err := today.SetByJwt(networkJwt); err != nil {
		t.Fatal(err)
	}
	if err := today.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	before, err := today.loadAuthState()
	if err != nil || before.ByJwt != networkJwt {
		t.Fatal("the fixture keeps no network credential")
	}
	cleaned := newTestSealedLocalState(t, todayHome, nil, true)
	after, err := cleaned.loadAuthState()
	if err != nil {
		t.Fatal(err)
	}
	if after.ByJwt != "" || after.ByClientJwt != before.ByClientJwt || after.InstanceId != before.InstanceId || after.Generation != before.Generation+1 {
		t.Fatalf("the first read under the rule kept %+v", after)
	}
	requireNoSecretInAuthStateFile(t, testAuthStateFile(t, todayHome), networkJwt)

	// a new sign-in starts over: its token is kept until its client exists
	otherJwt := credentialTestNetworkJwt(t, credentialTestOtherNetworkId)
	if err := localState.SetByJwt(otherJwt); err != nil {
		t.Fatal(err)
	}
	if localState.GetByJwt() != otherJwt || localState.GetByClientJwt() != "" {
		t.Fatal("a new sign-in was not kept until its client exists")
	}
}

func TestAdminCallAfterRestartNeedsReauthentication(t *testing.T) {
	home := t.TempDir()
	day := 24 * time.Hour
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-day), renewalTestBaseTime.Add(29*day), "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")

	// sign-in, the device client, then the device: the session holds the network credential
	r := newRenewalTestApi(t)
	r.api.SetByJwt(networkJwt)
	localState := newTestSealedLocalState(t, home, nil, true)
	if err := localState.SetByJwt(networkJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	r.startDevice(t, localState, clientJwt)
	connect.AssertEqual(t, r.api.HasNetworkCredential(), true)
	connect.AssertEqual(t, r.api.GetByJwt(), clientJwt)
	adminDone := make(chan error, 1)
	go func() {
		adminDone <- awaitApiCall(func(cb connect.ApiCallback[*NetworkDeleteResult]) { r.api.NetworkDelete(cb) })
	}()
	admin := r.transport.take(t)
	connect.AssertEqual(t, admin.path, "/auth/network-delete")
	connect.AssertEqual(t, admin.byJwt, networkJwt)
	admin.answer("{}")
	if err := <-adminDone; err != nil {
		t.Fatal(err)
	}

	// a restart: the device starts from the same storage, which holds no network credential
	relaunched := newRenewalTestApi(t)
	relaunchedState := newTestSealedLocalState(t, home, nil, true)
	relaunched.startDevice(t, relaunchedState, clientJwt)
	connect.AssertEqual(t, relaunched.api.HasNetworkCredential(), false)
	connect.AssertEqual(t, relaunched.api.GetByJwt(), clientJwt)
	err := awaitApiCall(func(cb connect.ApiCallback[*NetworkDeleteResult]) { relaunched.api.NetworkDelete(cb) })
	if !errors.Is(err, ErrNetworkCredentialRequired) {
		t.Fatalf("an admin call after the restart answered %v", err)
	}
	relaunched.transport.requireNone(t)
	// the client calls still go, on the client token
	clientDone := make(chan error, 1)
	go func() {
		clientDone <- awaitApiCall(func(cb connect.ApiCallback[*SubscriptionBalanceResult]) { relaunched.api.SubscriptionBalance(cb) })
	}()
	client := relaunched.transport.take(t)
	connect.AssertEqual(t, client.byJwt, clientJwt)
	client.answer("{}")
	<-clientDone
	relaunched.requireNoLogout(t)

	// signing in again for the admin action restores it, and the file still keeps none
	relaunched.api.SetByJwt(networkJwt)
	if !relaunched.api.HasNetworkCredential() {
		t.Fatal("a new sign-in gave no network credential")
	}
	requireNoSecretInAuthStateFile(t, testAuthStateFile(t, home), networkJwt)
}

// Fixture .auth_state files as today's builds write them, each opened by the default local state: nothing is
// rewritten, every value reads as written, and a new client keeps the network credential beside it.
func TestDefaultKeepsTodaysState(t *testing.T) {
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "today")
	instanceId := NewId().String()
	fixtures := []struct {
		name  string
		state persistedLocalAuthState
	}{
		{"both", persistedLocalAuthState{Version: localAuthStateVersion, Generation: 4, ByJwt: networkJwt, ByClientJwt: clientJwt, InstanceId: instanceId}},
		{"sign-in only", persistedLocalAuthState{Version: localAuthStateVersion, Generation: 1, ByJwt: networkJwt}},
		{"apple extension marker", persistedLocalAuthState{Version: localAuthStateVersion, Generation: 2, ByJwt: clientJwt, InstanceId: instanceId}},
		{"client only", persistedLocalAuthState{Version: localAuthStateVersion, Generation: 3, ByClientJwt: clientJwt, InstanceId: instanceId}},
	}
	for _, fixture := range fixtures {
		home := t.TempDir()
		data, err := json.Marshal(fixture.state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(home, ".by"), LocalStorageDirectoryPermissions); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".by", localAuthStateFileName), data, LocalStorageFilePermissions); err != nil {
			t.Fatal(err)
		}
		// through a space with default values, as an app that has not opted in builds it
		space := newNetworkSpace(context.Background(), *NewNetworkSpaceKey("today-auth.test", "test"),
			NetworkSpaceValues{ApiUrl: "http://127.0.0.1:1", PlatformUrl: "ws://127.0.0.1:1"}, home)
		localState := space.GetAsyncLocalState().GetLocalState()
		if localState.sessionOnlyNetworkCredential() || localState.authSettings.sealer != nil {
			t.Fatalf("%s: a default space changed the credential rules", fixture.name)
		}
		loaded, err := localState.loadAuthState()
		if err != nil || loaded != fixture.state {
			t.Fatalf("%s: the fixture read back as %+v, %v", fixture.name, loaded, err)
		}
		if !bytes.Equal(testAuthStateFile(t, home), data) {
			t.Fatalf("%s: a default read rewrote the file", fixture.name)
		}
		if fixture.state.ByJwt == networkJwt && fixture.state.ByClientJwt == "" {
			if err := localState.SetByClientJwt(clientJwt); err != nil {
				t.Fatal(err)
			}
			if localState.GetByJwt() != networkJwt {
				t.Fatalf("%s: the default dropped the network credential", fixture.name)
			}
		}
		if fixture.state.ByClientJwt == "" && fixture.state.InstanceId != "" {
			// the shipped Apple extension's marker still starts its device
			instance, _ := ParseId(instanceId)
			if selected, err := localState.SelectClientJwtForInstance(clientJwt, instance); err != nil || selected != clientJwt {
				t.Fatalf("%s: the extension's marker no longer starts: %v", fixture.name, err)
			}
		}
		space.close()
	}

	// the values of a space that never opted in serialize as before
	data, err := json.Marshal(NetworkSpaceValues{ApiUrl: "http://127.0.0.1:1"})
	if err != nil || bytes.Contains(data, []byte("session_only_network_credential")) {
		t.Fatalf("default values carry the new key: %s", data)
	}
	args, err := json.Marshal(&AuthNetworkClientArgs{DeviceSpec: "test"})
	if err != nil || bytes.Contains(args, []byte("admin_refused")) {
		t.Fatalf("a default mint asks for admin_refused: %s", args)
	}
}

func TestClientTokenAloneStaysSignedIn(t *testing.T) {
	home := t.TempDir()
	networkJwt := credentialTestNetworkJwt(t, credentialTestNetworkId)
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "alone")
	localState := newTestSealedLocalState(t, home, nil, true)
	if err := localState.SetByJwt(networkJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	if localState.GetByJwt() != "" {
		t.Fatal("the fixture kept the network credential")
	}
	signIn := localState.currentSignIn()
	if !signIn.signedIn {
		t.Fatal("dropping the network credential signed the person out")
	}
	location := &ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}
	if wrote, err := signIn.setConnectLocation(location); err != nil || !wrote {
		t.Fatalf("a write of the sign-in was dropped: %v %v", wrote, err)
	}

	// a restart reads a signed-in space, whose device starts on the client token alone
	relaunched := newTestSealedLocalState(t, home, nil, true)
	if !relaunched.currentSignIn().signedIn || relaunched.GetConnectLocation() == nil {
		t.Fatal("a client token alone did not keep the person signed in")
	}
	snapshot, err := relaunched.GetAuthStateSnapshot()
	if err != nil || snapshot.GetEmpty() || snapshot.GetByClientJwt() != clientJwt {
		t.Fatalf("the restarted space reads as signed out: %v", err)
	}
	api := newApi(t.Context(), nil, "https://alone.example")
	installTestDeviceByJwt(t, api, relaunched, clientJwt, relaunched.GetInstanceId())
	connect.AssertEqual(t, api.GetByJwt(), clientJwt)
	connect.AssertEqual(t, api.HasNetworkCredential(), false)
	if !relaunched.currentSignIn().signedIn {
		t.Fatal("the device's start signed the person out")
	}
}

func TestRenewalWritesNothingToDisk(t *testing.T) {
	r := newRenewalTestApi(t)
	home := t.TempDir()
	day := 24 * time.Hour
	// past its window, so the renewer renews at once
	networkJwt := renewalTestNetworkJwt(t, renewalTestBaseTime.Add(-20*day), renewalTestBaseTime.Add(10*day), "sign-in")
	clientJwt := credentialTestClientJwt(t, credentialTestNetworkId, "device")
	r.api.SetByJwt(networkJwt)
	localState := newTestSealedLocalState(t, home, nil, true)
	if err := localState.SetByJwt(networkJwt); err != nil {
		t.Fatal(err)
	}
	if err := localState.SetByClientJwt(clientJwt); err != nil {
		t.Fatal(err)
	}
	r.startDevice(t, localState, clientJwt)
	before := testAuthStateFile(t, home)

	request := r.transport.takeRenewal(t, networkJwt)
	renewedJwt := renewalTestRenewedJwt(t, r.clock.Now(), "renewed")
	request.answer(renewalTestReplyJwt(renewedJwt))
	r.requireRenewed(t)
	r.clock.requireArmed(t, 15*day)

	connect.AssertEqual(t, r.api.networkCredential(), renewedJwt)
	connect.AssertEqual(t, r.api.GetByJwt(), clientJwt)
	connect.AssertEqual(t, localState.GetByJwt(), "")
	after := testAuthStateFile(t, home)
	if !bytes.Equal(after, before) {
		t.Fatal("the renewal wrote the auth state")
	}
	requireNoSecretInAuthStateFile(t, after, networkJwt, renewedJwt)
	r.requireNoLogout(t)

	// the admin calls carry the renewal
	adminDone := make(chan error, 1)
	go func() {
		adminDone <- awaitApiCall(func(cb connect.ApiCallback[*NetworkDeleteResult]) { r.api.NetworkDelete(cb) })
	}()
	admin := r.transport.take(t)
	connect.AssertEqual(t, admin.byJwt, renewedJwt)
	admin.answer("{}")
	<-adminDone

	// a rejection of the session's credential drops it from memory, writes nothing and signs nobody out
	r.clock.Advance(15 * day)
	rejected := r.transport.takeRenewal(t, renewedJwt)
	rejected.fail(&connect.HttpStatusError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized"})
	r.requireRenewed(t)
	connect.AssertEqual(t, r.api.HasNetworkCredential(), false)
	connect.AssertEqual(t, r.api.GetByJwt(), clientJwt)
	if !bytes.Equal(testAuthStateFile(t, home), before) {
		t.Fatal("the rejection wrote the auth state")
	}
}
