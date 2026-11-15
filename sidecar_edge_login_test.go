//go:build !ios_extension && !js

package sdk

// SignEdgeLogin over a real local core (SDK-M6-03), and in remote mode over a daemon (QD-827): the core is enrolled
// against a fake api, its network's verified state is a fake SidecarAuthority here, since sidecartest's would bring
// its ACME test server into this module, and each statement is checked as §13.7's front checks one: the Signed
// verifies as SIGNED_TLS_EDGE_LOGIN under purpose edge-login with the device's identity key, and its body names the
// origin and the nonce the front issued. The front's own check, tlsEdgeLoginVerify, is the root's and unexported; the
// root's TestSidecarSignTlsEdgeLoginNeedsAConsentedDevice and TestDaemonTlsEdgeLoginSignConfirmedVerifies run it over
// the same calls. Remote mode runs over the fake daemon of sidecar_remote_test.go, which signs under a key of its own,
// and over the root's own daemon hosting the real enrolled core, whose confirmation gate the call passes.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// The network's verified state as far as an edge login asks it: one device's admission and whether its member
// consented to it. Everything else answers nothing.
type testEdgeLoginAuthority struct {
	stateLock sync.Mutex
	sidecarId sidecar.Id
	memberId  sidecar.Id
	tagIds    []sidecar.Id
	consented bool
}

func (self *testEdgeLoginAuthority) set(consented bool, tagIds ...sidecar.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.consented = consented
	self.tagIds = tagIds
}

func (self *testEdgeLoginAuthority) Pin() *bridge.AuthPin {
	return &bridge.AuthPin{Version: 1}
}

func (self *testEdgeLoginAuthority) Admission(sidecarId sidecar.Id) *sidecar.AclSidecar {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if sidecarId != self.sidecarId {
		return nil
	}
	admission := &sidecar.AclSidecar{Row: &bridge.SidecarRow{SidecarId: sidecarId.Bytes()}, TagIds: self.tagIds}
	if len(self.tagIds) == 0 {
		admission.MemberId = self.memberId
	}
	return admission
}

func (self *testEdgeLoginAuthority) MemberDevices(memberId sidecar.Id) []*sidecar.AclSidecar {
	return nil
}

func (self *testEdgeLoginAuthority) Consented(sidecarId sidecar.Id, memberId sidecar.Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.consented && sidecarId == self.sidecarId && memberId == self.memberId
}

func (self *testEdgeLoginAuthority) CurrentBinding(memberId sidecar.Id) *bridge.RecoveryBindingRow {
	return nil
}

func (self *testEdgeLoginAuthority) PresenceRequired(statement *sidecar.SidecarStatement) bool {
	return false
}

func (self *testEdgeLoginAuthority) LiveKeys(sidecarId sidecar.Id) []*bridge.KeyRow {
	return nil
}

func (self *testEdgeLoginAuthority) Cascade(targetId sidecar.Id, sinceN uint64) ([]sidecar.DeviceRevokeItem, error) {
	return nil, nil
}

func (self *testEdgeLoginAuthority) ProposedSince(lostAt time.Time) (uint64, time.Time, bool) {
	return 0, time.Time{}, false
}

func (self *testEdgeLoginAuthority) AddEntryCallback(callback sidecar.SidecarAuthorityEntryFunction) func() {
	return func() {}
}

// An enrolled local Sidecar that dials nothing but its test servers.
func newTestEnrolledSidecar(t *testing.T) *Sidecar {
	t.Helper()
	world := newTestSidecarWorld(t)
	childClientId := sidecar.NewId()
	coreUrl := newTestTlsCoreServer(t, childClientId.String())
	child := testSidecarChildToken(t, credentialTestNetworkId, childClientId.String(), 0)
	deviceServer := newTestSessionServer(func(request testSessionRequest) (int, any) {
		switch request.path {
		case "/network/auth-client":
			return http.StatusOK, map[string]any{"by_client_jwt": child, "client_id": childClientId.String(), "admin_refused": true, "strict_exp": true}
		case "/auth/refresh":
			return http.StatusOK, map[string]string{"by_jwt": child}
		}
		return http.StatusNotFound, map[string]string{"error": "not found"}
	})
	ctx, deviceApi := newTestApi(t, deviceServer)
	deviceApi.SetByJwt(dpopTestClientToken(t))
	local := world.openWith(t, DefaultSidecarSettings(), &sidecarLocalOptions{
		adjustCoreSettings: func(settings *sidecar.SidecarSettings) {
			settings.ApiUrl = coreUrl
			settings.PlatformUrl = "wss://127.0.0.1:1"
		},
		hostCredentialStores: func() []sidecar.SidecarHostCredentialStore {
			return nil
		},
	})
	awaitTestSidecarState(t, local, sidecar.SidecarStateUnenrolled, "")
	if _, err := local.enrollWithDeviceApi(ctx, deviceApi); err != nil {
		t.Fatal(err)
	}
	if local.Core().Identity() == nil {
		t.Fatal("the enrollment left no identity")
	}
	return local
}

// One SignEdgeLogin, answered.
func testSignEdgeLogin(t *testing.T, local *Sidecar, origin string, nonceBase64 string) *SidecarResult {
	t.Helper()
	callback, results := testSidecarResultCallback()
	local.SignEdgeLogin(origin, nonceBase64, callback)
	select {
	case result := <-results:
		return result
	case <-time.After(testSidecarTimeout):
		t.Fatal("SignEdgeLogin never answered")
		return nil
	}
}

// The login a statement names, once it verifies as the front verifies it: the Signed under the device's identity key,
// of the edge-login type and purpose.
func testEdgeLoginOf(t *testing.T, statement string, signerId sidecar.Id, identityPublicKey []byte) *protocol.TlsEdgeLogin {
	t.Helper()
	signedBytes, err := base64.RawURLEncoding.DecodeString(statement)
	if err != nil {
		t.Fatalf("the statement is not base64url: %v", err)
	}
	signed := &bridge.Signed{}
	if err := proto.Unmarshal(signedBytes, signed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(signed.GetSignerId(), signerId.Bytes()) {
		t.Fatalf("signed by %x", signed.GetSignerId())
	}
	if err := bridge.VerifySigned(signed, sidecar.TlsEdgeLoginPurpose, bridge.SignedType_SIGNED_TLS_EDGE_LOGIN, bridge.SignedKeyAlgEd25519, identityPublicKey); err != nil {
		t.Fatalf("the statement does not verify as an edge login: %v", err)
	}
	// no other purpose takes it
	if err := bridge.VerifySigned(signed, "other-purpose", bridge.SignedType_SIGNED_TLS_EDGE_LOGIN, bridge.SignedKeyAlgEd25519, identityPublicKey); err == nil {
		t.Error("the statement verifies under another purpose")
	}
	login := &protocol.TlsEdgeLogin{}
	if err := proto.Unmarshal(signed.GetBody(), login); err != nil {
		t.Fatal(err)
	}
	return login
}

// The statement binds the origin and the nonce, so a front takes it for that login alone, and only a member's
// consented device signs one: an unconsented device, an origin or nonce no front issues, and a core with no verified
// state each answer a refusal that holds no statement and repeats no nonce.
func TestSignEdgeLoginBindsOrigin(t *testing.T) {
	local := newTestEnrolledSidecar(t)
	identity := local.Core().Identity()
	origin := "https://app.sdktest.example"
	otherOrigin := "https://other.sdktest.example"
	nonce := make([]byte, 32)
	if _, err := cryptorand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	nonceText := base64.RawURLEncoding.EncodeToString(nonce)
	refused := func(when string, result *SidecarResult) {
		t.Helper()
		if result.Ok || result.Value != "" || result.Message == "" {
			t.Errorf("%s: %+v", when, result)
		}
		if strings.Contains(result.Message, nonceText) {
			t.Errorf("%s: the refusal repeats the nonce", when)
		}
	}

	// no verified state of the network: nothing says this device is anyone's
	refused("no authority", testSignEdgeLogin(t, local, origin, nonceText))

	authority := &testEdgeLoginAuthority{sidecarId: identity.SidecarId, memberId: sidecar.NewId()}
	t.Cleanup(local.Core().InstallAuthority(authority))
	refused("unconsented", testSignEdgeLogin(t, local, origin, nonceText))
	// a tagged sidecar is no member's device, whatever the consents say
	authority.set(true, sidecar.NewId())
	refused("tagged", testSignEdgeLogin(t, local, origin, nonceText))

	authority.set(true)
	refused("an http origin", testSignEdgeLogin(t, local, "http://app.sdktest.example", nonceText))
	refused("an origin with a path", testSignEdgeLogin(t, local, origin+"/login", nonceText))
	refused("a short nonce", testSignEdgeLogin(t, local, origin, base64.RawURLEncoding.EncodeToString(nonce[:16])))
	refused("no nonce", testSignEdgeLogin(t, local, origin, ""))
	refused("a nonce that is not base64", testSignEdgeLogin(t, local, origin, "not base64 !"))
	refused("a nonce past the bound", testSignEdgeLogin(t, local, origin, strings.Repeat("A", defaultSidecarEdgeLoginSettings().NonceTextMaxByteCount+1)))

	result := testSignEdgeLogin(t, local, origin, nonceText)
	if !result.Ok || result.Value == "" {
		t.Fatalf("a consented device's statement: %+v", result)
	}
	login := testEdgeLoginOf(t, result.Value, identity.SidecarId, identity.IdentityPublicKey)
	if login.GetOrigin() != origin || !bytes.Equal(login.GetNonce(), nonce) {
		t.Fatalf("the statement names %q and another nonce: %v", login.GetOrigin(), bytes.Equal(login.GetNonce(), nonce))
	}
	// the front at another origin compares the statement's origin with its own connection's, and refuses
	if login.GetOrigin() == otherOrigin {
		t.Error("the statement opens another origin")
	}

	// the same nonce at another origin is another statement, which names that origin alone
	other := testSignEdgeLogin(t, local, otherOrigin, nonceText)
	if !other.Ok || other.Value == result.Value {
		t.Fatalf("another origin's statement: %+v", other)
	}
	if otherLogin := testEdgeLoginOf(t, other.Value, identity.SidecarId, identity.IdentityPublicKey); otherLogin.GetOrigin() != otherOrigin {
		t.Errorf("the other origin's statement names %q", otherLogin.GetOrigin())
	}

	// the nonce as standard base64 with padding names the same bytes
	padded := testSignEdgeLogin(t, local, origin, base64.StdEncoding.EncodeToString(nonce))
	if !padded.Ok {
		t.Fatalf("a padded nonce: %+v", padded)
	}
	if paddedLogin := testEdgeLoginOf(t, padded.Value, identity.SidecarId, identity.IdentityPublicKey); !bytes.Equal(paddedLogin.GetNonce(), nonce) {
		t.Error("a padded nonce signed other bytes")
	}

	// a consent withdrawn ends it
	authority.set(false)
	refused("consent withdrawn", testSignEdgeLogin(t, local, origin, nonceText))
}

// A Sidecar that never enrolled, a closed one and one with neither backend sign nothing, and a closed remote-mode one
// sends its daemon nothing.
func TestSignEdgeLoginRefusesWithoutADevice(t *testing.T) {
	nonceText := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	origin := "https://app.sdktest.example"

	world := newTestSidecarWorld(t)
	local := world.open(t)
	if result := testSignEdgeLogin(t, local, origin, nonceText); result.Ok || result.Value != "" {
		t.Errorf("an unenrolled sidecar signed: %+v", result)
	}
	world.close(t, local)
	if result := testSignEdgeLogin(t, local, origin, nonceText); result.Ok || result.Value != "" {
		t.Errorf("a closed sidecar signed: %+v", result)
	}
	if result := testSignEdgeLogin(t, &Sidecar{}, origin, nonceText); result.Ok || result.Value != "" {
		t.Errorf("a sidecar with no backend signed: %+v", result)
	}

	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	remote.Close()
	if result := testSignEdgeLogin(t, remote, origin, nonceText); result.Ok || result.Value != "" {
		t.Errorf("a closed remote-mode sidecar signed: %+v", result)
	}
	for _, method := range daemon.seenMethods() {
		if method != "status" {
			t.Errorf("the edge login reached the daemon as %s", method)
		}
	}
}

// The fake daemon's tls.edge_login.sign: the call is taken only as the root's dispatcher takes it, from control and
// confirmed, its params decoded strictly into the daemon's type and held to what a front issues, and the statement is
// signed under a key of the fake's own as the daemon's core would sign it; or, when a test sets one, a refusal or an
// answer of the test's in its place.
type testEdgeLoginDaemonSigner struct {
	sidecarId sidecar.Id
	publicKey ed25519.PublicKey
	key       ed25519.PrivateKey

	stateLock sync.Mutex
	refusal   *sidecar.DaemonRpcError
	statement []byte
}

func newTestEdgeLoginDaemonSigner(t *testing.T, sidecarId sidecar.Id) *testEdgeLoginDaemonSigner {
	t.Helper()
	publicKey, key, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testEdgeLoginDaemonSigner{sidecarId: sidecarId, publicKey: publicKey, key: key}
}

// Answers in place of the statement from here on: refusal when set, else statement when not nil, else the statement
// asked for.
func (self *testEdgeLoginDaemonSigner) set(refusal *sidecar.DaemonRpcError, statement []byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.refusal, self.statement = refusal, statement
}

// Signs as the daemon's identity key does, any purpose and type: Sidecar.Sign's shape.
func (self *testEdgeLoginDaemonSigner) sign(purpose string, signedType bridge.SignedType, body []byte, agentId *sidecar.Id) (*bridge.Signed, error) {
	return &bridge.Signed{
		Body:      bytes.Clone(body),
		SignerId:  self.sidecarId.Bytes(),
		Signature: ed25519.Sign(self.key, bridge.SignedBytes(purpose, signedType, body)),
		Type:      uint32(signedType),
	}, nil
}

// testRemoteDaemon.answer for tls.edge_login.sign, leaving every other method to the fake.
func (self *testEdgeLoginDaemonSigner) answer(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
	if request.Method != sidecar.DaemonMethodTlsEdgeLoginSign {
		return nil, nil, false
	}
	if !request.UrConfirmed {
		return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcConfirmationRequired, Message: "this call needs a person's confirmation on /dev/tty or in an app"}, true
	}
	refusal, statement := func() (*sidecar.DaemonRpcError, []byte) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.refusal, self.statement
	}()
	if refusal != nil {
		return nil, refusal, true
	}
	if statement != nil {
		return &sidecar.DaemonTlsEdgeLoginSignReply{Statement: statement}, nil, true
	}
	params := &sidecar.DaemonTlsEdgeLoginSignParams{}
	decoder := json.NewDecoder(bytes.NewReader(request.Params))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(params); err != nil {
		return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "the params do not decode"}, true
	}
	login, err := sidecar.NewTlsEdgeLogin(params.Origin, params.Nonce)
	if err != nil {
		return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: err.Error()}, true
	}
	signed, err := sidecar.SignTlsEdgeLogin(login, self.sign)
	if err != nil {
		return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: err.Error()}, true
	}
	return &sidecar.DaemonTlsEdgeLoginSignReply{Statement: signed}, nil, true
}

// A paired remote-mode Sidecar's SignEdgeLogin completes over its daemon: the call goes as tls.edge_login.sign,
// confirmed, on the paired network, carrying the origin as given and the nonce's bytes and nothing more, and the
// statement the daemon signed comes back as the result, verifying under the daemon's key for that origin and nonce.
// The daemon's refusal comes back as one with nothing signed, a refusal that repeats the nonce is replaced, an answer
// that is not the statement asked for is refused, and a nonce that does not decode is refused before anything is sent.
func TestSignEdgeLoginRemoteCompletesOverTheDaemon(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	signer := newTestEdgeLoginDaemonSigner(t, daemon.sidecarId)
	daemon.answer = signer.answer
	daemon.start()
	device := newTestRemoteDeviceApi(t, testRemoteBothMarks)
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	if _, err := remote.enrollWithDeviceApi(device.ctx, device.api); err != nil {
		t.Fatal(err)
	}
	origin := "https://app.sdktest.example"
	nonce := make([]byte, 32)
	if _, err := cryptorand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	nonceText := base64.RawURLEncoding.EncodeToString(nonce)
	signCalls := func() []testRemoteDaemonRequest {
		calls := []testRemoteDaemonRequest{}
		for _, request := range daemon.seen() {
			if request.Method == sidecar.DaemonMethodTlsEdgeLoginSign {
				calls = append(calls, request)
			}
		}
		return calls
	}
	paramsOf := func(request testRemoteDaemonRequest) *sidecar.DaemonTlsEdgeLoginSignParams {
		t.Helper()
		params := &sidecar.DaemonTlsEdgeLoginSignParams{}
		decoder := json.NewDecoder(bytes.NewReader(request.Params))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(params); err != nil {
			t.Fatalf("the daemon's type does not take the params: %s", request.line)
		}
		return params
	}
	refused := func(when string, result *SidecarResult) {
		t.Helper()
		if result.Ok || result.Value != "" || result.Message == "" {
			t.Errorf("%s: %+v", when, result)
		}
		for _, held := range []string{nonceText, base64.StdEncoding.EncodeToString(nonce)} {
			if strings.Contains(result.Message, held) {
				t.Errorf("%s: the refusal repeats the nonce", when)
			}
		}
	}

	result := testSignEdgeLogin(t, remote, origin, nonceText)
	if !result.Ok || result.Value == "" {
		t.Fatalf("the daemon's statement: %+v", result)
	}
	login := testEdgeLoginOf(t, result.Value, daemon.sidecarId, signer.publicKey)
	if login.GetOrigin() != origin || !bytes.Equal(login.GetNonce(), nonce) {
		t.Fatalf("the statement names %q and another nonce: %v", login.GetOrigin(), bytes.Equal(login.GetNonce(), nonce))
	}
	calls := signCalls()
	if len(calls) != 1 {
		t.Fatalf("%d sign calls", len(calls))
	}
	params := paramsOf(calls[0])
	if !calls[0].UrConfirmed || params.Network != daemon.networkId.String() || params.Origin != origin || !bytes.Equal(params.Nonce, nonce) {
		t.Errorf("the sign call: confirmed %t, network %q, origin %q, the nonce sent %t", calls[0].UrConfirmed, params.Network, params.Origin, bytes.Equal(params.Nonce, nonce))
	}
	extra := map[string]json.RawMessage{}
	if err := json.Unmarshal(calls[0].Params, &extra); err != nil {
		t.Fatal(err)
	}
	delete(extra, "network")
	delete(extra, "origin")
	delete(extra, "nonce")
	if len(extra) != 0 {
		t.Errorf("the sign call carried more than the origin and the nonce: %v", extra)
	}

	// the nonce as padded standard base64 sends the same bytes, and the daemon's statement is the same
	padded := testSignEdgeLogin(t, remote, origin, base64.StdEncoding.EncodeToString(nonce))
	if !padded.Ok || padded.Value != result.Value {
		t.Errorf("a padded nonce: %+v", padded)
	}
	if calls := signCalls(); !bytes.Equal(paramsOf(calls[len(calls)-1]).Nonce, nonce) {
		t.Error("a padded nonce sent other bytes")
	}

	// the daemon refuses an origin or a nonce no front issues, and the app gets the refusal
	refused("an http origin", testSignEdgeLogin(t, remote, "http://app.sdktest.example", nonceText))
	refused("a short nonce", testSignEdgeLogin(t, remote, origin, base64.RawURLEncoding.EncodeToString(nonce[:16])))
	// a daemon refusal that repeats the nonce is replaced
	signer.set(&sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "refused " + base64.StdEncoding.EncodeToString(nonce)}, nil)
	refused("a refusal repeating the nonce", testSignEdgeLogin(t, remote, origin, nonceText))

	// an answer that is not the statement asked for is refused
	otherOriginLogin, err := sidecar.NewTlsEdgeLogin("https://other.sdktest.example", nonce)
	if err != nil {
		t.Fatal(err)
	}
	otherOrigin, err := sidecar.SignTlsEdgeLogin(otherOriginLogin, signer.sign)
	if err != nil {
		t.Fatal(err)
	}
	otherNonceLogin, err := sidecar.NewTlsEdgeLogin(origin, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	otherNonce, err := sidecar.SignTlsEdgeLogin(otherNonceLogin, signer.sign)
	if err != nil {
		t.Fatal(err)
	}
	body, err := proto.Marshal(&protocol.TlsEdgeLogin{Origin: origin, Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	otherTypeSigned, err := signer.sign("tls-link", bridge.SignedType_SIGNED_TLS_LINK, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	otherType, err := proto.Marshal(otherTypeSigned)
	if err != nil {
		t.Fatal(err)
	}
	unsigned, err := proto.Marshal(&bridge.Signed{Body: body, SignerId: daemon.sidecarId.Bytes(), Type: uint32(bridge.SignedType_SIGNED_TLS_EDGE_LOGIN)})
	if err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string][]byte{
		"another origin's statement": otherOrigin,
		"another nonce's statement":  otherNonce,
		"another type's statement":   otherType,
		"an unsigned statement":      unsigned,
		"bytes that are no Signed":   []byte("not a signed statement"),
		"an empty statement":         {},
	} {
		signer.set(nil, statement)
		refused(name, testSignEdgeLogin(t, remote, origin, nonceText))
	}
	signer.set(nil, nil)

	// a nonce that does not decode is refused here, with nothing sent
	before := len(signCalls())
	refused("a nonce that is not base64", testSignEdgeLogin(t, remote, origin, "not base64 !"))
	if after := len(signCalls()); after != before {
		t.Errorf("%d sign calls for a nonce that does not decode", after-before)
	}
	for _, call := range signCalls() {
		if !call.UrConfirmed {
			t.Errorf("a sign call went unconfirmed: %s", call.line)
		}
	}
}

// A root daemon on socketPath hosting core's network, with status answering online for it and tls.edge_login.sign
// registered as ur daemon registers it, stopped at the test's end.
func startTestEdgeLoginDaemon(t *testing.T, socketPath string, core *sidecar.Sidecar) {
	t.Helper()
	stateDirectory, err := os.MkdirTemp("", "ur")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(stateDirectory) })
	daemon, err := sidecar.NewDaemon(context.Background(), &sidecar.DaemonSettings{
		Instance:       "default",
		StateDirectory: filepath.Join(stateDirectory, "state"),
		SocketPath:     socketPath,
		Local:          sidecar.DefaultSidecarSettings().LocalSettings,
		Version:        testRemoteDaemonVersion,
		Log:            sidecar.NewNoopLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := core.Identity()
	if err := daemon.RegisterMethod(&sidecar.DaemonMethod{Name: sidecarControlMethodStatus, Handler: func(ctx context.Context, call *sidecar.DaemonCall) (any, error) {
		return map[string]any{
			"instance":   "default",
			"state":      "online",
			"generation": identity.Generation,
			"key_store":  "keychain",
			"networks": []map[string]any{{
				"network_id": identity.NetworkId,
				"sidecar_id": identity.SidecarId,
				"state":      "online",
				"generation": identity.Generation,
			}},
		}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	for _, method := range sidecar.DaemonTlsEdgeLoginMethods() {
		if err := daemon.RegisterMethod(method); err != nil {
			t.Fatal(err)
		}
	}
	if err := daemon.AddSidecar(identity.NetworkId, core); err != nil {
		t.Fatal(err)
	}
	ran := make(chan error, 1)
	go func() {
		ran <- daemon.Run()
	}()
	t.Cleanup(func() {
		daemon.Close()
		<-ran
	})
}

// End to end over the root's daemon and a real enrolled core: a remote-mode Sidecar's call passes the daemon's
// confirmation gate as the person's act, the daemon's core signs only once its member has consented to it, and the
// statement verifies as the front verifies one, under the core's identity key for that origin and nonce. An origin or
// a nonce no front issues is the daemon's to refuse.
func TestSignEdgeLoginRemoteOverTheRootDaemon(t *testing.T) {
	local := newTestEnrolledSidecar(t)
	core := local.Core()
	identity := core.Identity()
	socketPath := testRemoteSocketPath(t)
	startTestEdgeLoginDaemon(t, socketPath, core)
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	origin := "https://app.sdktest.example"
	nonce := make([]byte, 32)
	if _, err := cryptorand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	nonceText := base64.RawURLEncoding.EncodeToString(nonce)
	refused := func(when string, result *SidecarResult) {
		t.Helper()
		if result.Ok || result.Value != "" || result.Message == "" {
			t.Errorf("%s: %+v", when, result)
		}
		if strings.Contains(result.Message, nonceText) {
			t.Errorf("%s: the refusal repeats the nonce", when)
		}
	}

	// the daemon's core holds no verified state of its network yet: nothing says this device is anyone's
	refused("no authority", testSignEdgeLogin(t, remote, origin, nonceText))
	authority := &testEdgeLoginAuthority{sidecarId: identity.SidecarId, memberId: sidecar.NewId()}
	t.Cleanup(core.InstallAuthority(authority))
	refused("unconsented", testSignEdgeLogin(t, remote, origin, nonceText))

	authority.set(true)
	result := testSignEdgeLogin(t, remote, origin, nonceText)
	if !result.Ok || result.Value == "" {
		t.Fatalf("a consented device's statement through its daemon: %+v", result)
	}
	login := testEdgeLoginOf(t, result.Value, identity.SidecarId, identity.IdentityPublicKey)
	if login.GetOrigin() != origin || !bytes.Equal(login.GetNonce(), nonce) {
		t.Fatalf("the statement names %q and another nonce: %v", login.GetOrigin(), bytes.Equal(login.GetNonce(), nonce))
	}
	refused("an http origin", testSignEdgeLogin(t, remote, "http://app.sdktest.example", nonceText))
	refused("an origin with a path", testSignEdgeLogin(t, remote, origin+"/login", nonceText))
	refused("a short nonce", testSignEdgeLogin(t, remote, origin, base64.RawURLEncoding.EncodeToString(nonce[:16])))

	// a consent withdrawn ends it
	authority.set(false)
	refused("consent withdrawn", testSignEdgeLogin(t, remote, origin, nonceText))
}

// The method's name and shapes this sdk spells are the daemon's own: each message crosses the other side's type and
// back, losing nothing.
func TestRemoteEdgeLoginWireMatchesTheDaemon(t *testing.T) {
	if sidecarControlMethodTlsEdgeLoginSign != sidecar.DaemonMethodTlsEdgeLoginSign {
		t.Errorf("%q is the daemon's %q", sidecarControlMethodTlsEdgeLoginSign, sidecar.DaemonMethodTlsEdgeLoginSign)
	}
	crossings := []struct {
		name    string
		own     any
		daemons any
		back    any
	}{
		{
			name:    "params",
			own:     &sidecarControlTlsEdgeLoginSignParams{Network: "a-network", Origin: "https://app.sdktest.example", Nonce: []byte{1, 2}},
			daemons: &sidecar.DaemonTlsEdgeLoginSignParams{},
			back:    &sidecarControlTlsEdgeLoginSignParams{},
		},
		{
			name:    "reply",
			own:     &sidecarControlTlsEdgeLoginSignReply{Statement: []byte{3, 4}},
			daemons: &sidecar.DaemonTlsEdgeLoginSignReply{},
			back:    &sidecarControlTlsEdgeLoginSignReply{},
		},
	}
	for _, crossing := range crossings {
		first, err := json.Marshal(crossing.own)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(first))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(crossing.daemons); err != nil {
			t.Errorf("%s: the daemon's type does not take it: %v", crossing.name, err)
			continue
		}
		second, err := json.Marshal(crossing.daemons)
		if err != nil {
			t.Fatal(err)
		}
		decoder = json.NewDecoder(bytes.NewReader(second))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(crossing.back); err != nil {
			t.Errorf("%s: this sdk's type does not take the daemon's: %v", crossing.name, err)
			continue
		}
		third, err := json.Marshal(crossing.back)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, third) {
			t.Errorf("%s changed across the daemon's type: %s to %s", crossing.name, first, third)
		}
	}
}
