//go:build !ios_extension && !js

package sdk

// SignEdgeLogin over a real local core (SDK-M6-03): the core is enrolled against a fake api, its network's verified
// state is a fake SidecarAuthority here, since sidecartest's would bring its ACME test server into this module, and
// each statement is checked as §13.7's front checks one: the Signed verifies as SIGNED_TLS_EDGE_LOGIN under purpose
// edge-login with the device's identity key, and its body names the origin and the nonce the front issued. The
// front's own check, tlsEdgeLoginVerify, is the root's and unexported; the root's
// TestSidecarSignTlsEdgeLoginNeedsAConsentedDevice runs it over the same call.

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/base64"
	"net/http"
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

// A Sidecar that never enrolled, a closed one and a remote-mode one sign nothing.
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

	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	if result := testSignEdgeLogin(t, remote, origin, nonceText); result.Ok || result.Value != "" {
		t.Errorf("a remote-mode sidecar signed: %+v", result)
	}
	for _, method := range daemon.seenMethods() {
		if method != "status" {
			t.Errorf("the edge login reached the daemon as %s", method)
		}
	}
}
