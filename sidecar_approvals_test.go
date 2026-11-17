//go:build !ios_extension && !js && !ios && !android

package sdk

// SidecarApprovals (SDK-M8-05) over a real enrolled local core and over a fake daemon: the core is enrolled against a
// fake api whose delta route records each statement posted, and its network's verified state is a fake
// SidecarAuthority here, since sidecartest's would bring its ACME test server into this module. The daemon is
// sidecar_remote_test.go's fake over a Unix socket, answering the approvals and devices methods as cli and the root
// name them, read from their source. Every wait is on a channel or a callback's answer, never a clock.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// The network's verified state as far as approvals and device revocation ask it: one member's devices, this device's
// standing, its live presence keys and which statement types the presence row covers, the registered keys each device
// holds and the entries' times. Everything else answers nothing.
type testApprovalsAuthority struct {
	stateLock sync.Mutex
	ownId     sidecar.Id
	memberId  sidecar.Id
	// every admission by sidecar_id
	admissions map[sidecar.Id]*sidecar.AclSidecar
	consented  bool
	capable    bool
	suspended  map[sidecar.Id]bool
	liveKeys   []*bridge.KeyRow
	registered map[sidecar.Id][]*bridge.KeyRow
	covered    map[bridge.SignedType]bool
	entryTimes map[uint64]time.Time
	signatures int
}

// An authority where ownId is memberId's consented, approval-capable device, admitted at entry 5.
func newTestApprovalsAuthority(ownId sidecar.Id, memberId sidecar.Id) *testApprovalsAuthority {
	authority := &testApprovalsAuthority{
		ownId:      ownId,
		memberId:   memberId,
		admissions: map[sidecar.Id]*sidecar.AclSidecar{},
		consented:  true,
		capable:    true,
		suspended:  map[sidecar.Id]bool{},
		registered: map[sidecar.Id][]*bridge.KeyRow{},
		covered:    map[bridge.SignedType]bool{},
		entryTimes: map[uint64]time.Time{},
		signatures: 1,
	}
	authority.admit(ownId, memberId, 5)
	return authority
}

// Admits sidecarId for memberId at position, untagged.
func (self *testApprovalsAuthority) admit(sidecarId sidecar.Id, memberId sidecar.Id, position uint64) *sidecar.AclSidecar {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	admission := &sidecar.AclSidecar{
		Row:      &bridge.SidecarRow{SidecarId: sidecarId.Bytes(), AdmittedAt: position},
		MemberId: memberId,
	}
	self.admissions[sidecarId] = admission
	return admission
}

func (self *testApprovalsAuthority) Pin() *bridge.AuthPin {
	return &bridge.AuthPin{Version: 50, Chain: bytes.Repeat([]byte{7}, 32)}
}

func (self *testApprovalsAuthority) Admission(sidecarId sidecar.Id) *sidecar.AclSidecar {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.admissions[sidecarId]
}

func (self *testApprovalsAuthority) MemberDevices(memberId sidecar.Id) []*sidecar.AclSidecar {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	devices := []*sidecar.AclSidecar{}
	for _, admission := range self.admissions {
		if admission.MemberId == memberId && len(admission.TagIds) == 0 {
			devices = append(devices, admission)
		}
	}
	slices.SortFunc(devices, func(a *sidecar.AclSidecar, b *sidecar.AclSidecar) int {
		return bytes.Compare(a.Row.GetSidecarId(), b.Row.GetSidecarId())
	})
	return devices
}

func (self *testApprovalsAuthority) Consented(sidecarId sidecar.Id, memberId sidecar.Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.consented && sidecarId == self.ownId && memberId == self.memberId
}

func (self *testApprovalsAuthority) CurrentBinding(memberId sidecar.Id) *bridge.RecoveryBindingRow {
	return nil
}

func (self *testApprovalsAuthority) PresenceRequired(statement *sidecar.SidecarStatement) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.covered[statement.Type] && 0 < len(self.liveKeys) && statement.SignerId == self.ownId
}

func (self *testApprovalsAuthority) LiveKeys(sidecarId sidecar.Id) []*bridge.KeyRow {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if sidecarId != self.ownId {
		return nil
	}
	return slices.Clone(self.liveKeys)
}

func (self *testApprovalsAuthority) Cascade(targetId sidecar.Id, sinceN uint64) ([]sidecar.DeviceRevokeItem, error) {
	return nil, nil
}

func (self *testApprovalsAuthority) ProposedSince(lostAt time.Time) (uint64, time.Time, bool) {
	return 0, time.Time{}, false
}

func (self *testApprovalsAuthority) AddEntryCallback(callback sidecar.SidecarAuthorityEntryFunction) func() {
	return func() {}
}

func (self *testApprovalsAuthority) ApprovalCapable(sidecarId sidecar.Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.capable && sidecarId == self.ownId
}

func (self *testApprovalsAuthority) Suspended(sidecarId sidecar.Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.suspended[sidecarId]
}

func (self *testApprovalsAuthority) RevokeSignatures(targetId sidecar.Id) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.signatures
}

func (self *testApprovalsAuthority) RegisteredKeys(sidecarId sidecar.Id) []*bridge.KeyRow {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.registered[sidecarId])
}

func (self *testApprovalsAuthority) EntryTime(n uint64) (time.Time, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	at, ok := self.entryTimes[n]
	return at, ok
}

var _ sidecar.SidecarRevokeAuthority = (*testApprovalsAuthority)(nil)

// The test core's delta route: each statement posted, answered at the next position.
type testApprovalsApi struct {
	stateLock sync.Mutex
	posts     [][]byte
	nextN     uint64
}

// POST /network/config/delta: the statement kept, answered at the next position with a synthetic chain.
func (self *testApprovalsApi) post(request testSessionRequest) (int, any) {
	wire := struct {
		Signed []byte `json:"signed"`
	}{}
	if err := json.Unmarshal(request.body, &wire); err != nil || len(wire.Signed) == 0 {
		return http.StatusBadRequest, map[string]string{"error": "no statement"}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.posts = append(self.posts, wire.Signed)
	self.nextN += 1
	return http.StatusOK, map[string]any{"n": 100 + self.nextN, "chain": bytes.Repeat([]byte{9}, 32)}
}

// The statements posted, in order, each parsed.
func (self *testApprovalsApi) postedStatements(t *testing.T) []*bridge.Signed {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	statements := []*bridge.Signed{}
	for _, post := range self.posts {
		signed := &bridge.Signed{}
		if err := proto.Unmarshal(post, signed); err != nil {
			t.Fatal(err)
		}
		statements = append(statements, signed)
	}
	return statements
}

func (self *testApprovalsApi) postCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.posts)
}

// An enrolled local Sidecar on signer, its network's verified state the authority it answers, and the delta route
// its statements post to. Dials nothing but its test servers.
func newTestApprovalsSidecar(t *testing.T, signer SidecarPresenceSigner) (*Sidecar, *testApprovalsAuthority, *testApprovalsApi) {
	t.Helper()
	world := newTestSidecarWorld(t)
	childClientId := sidecar.NewId()
	api := &testApprovalsApi{}
	coreServer := newTestSessionServer(func(request testSessionRequest) (int, any) {
		switch request.path {
		case "/sidecar/bind":
			wire := struct {
				Binding []byte `json:"binding"`
			}{}
			if err := json.Unmarshal(request.body, &wire); err != nil {
				return http.StatusBadRequest, map[string]string{"error": "binding_invalid"}
			}
			binding, err := sidecar.VerifySidecarIdentityBinding(wire.Binding)
			if err != nil {
				return http.StatusBadRequest, map[string]string{"error": "binding_invalid"}
			}
			// the generation alone, as server's bind answers since QD-1029
			if binding.ClientId != childClientId {
				return http.StatusBadRequest, map[string]string{"error": "binding_invalid"}
			}
			return http.StatusOK, map[string]any{"generation": 1}
		case "/network/config/delta":
			return api.post(request)
		}
		return http.StatusNotFound, map[string]string{"error": "not found"}
	})
	httpServer := httptest.NewServer(coreServer)
	t.Cleanup(httpServer.Close)
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
	settings := DefaultSidecarSettings()
	settings.StateDir = world.stateDir
	local, err := newSidecarLocal(context.Background(), world.space, world.keyStore, signer, settings, &sidecarLocalOptions{
		adjustCoreSettings: func(settings *sidecar.SidecarSettings) {
			settings.ApiUrl = httpServer.URL
			settings.PlatformUrl = "wss://127.0.0.1:1"
		},
		hostCredentialStores: func() []sidecar.SidecarHostCredentialStore {
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		world.close(t, local)
	})
	awaitTestSidecarState(t, local, sidecar.SidecarStateUnenrolled, "")
	if _, err := local.enrollWithDeviceApi(ctx, deviceApi); err != nil {
		t.Fatal(err)
	}
	identity := local.Core().Identity()
	if identity == nil {
		t.Fatal("the enrollment left no identity")
	}
	authority := newTestApprovalsAuthority(identity.SidecarId, sidecar.NewId())
	t.Cleanup(local.Core().InstallAuthority(authority))
	return local, authority, api
}

// What a test's presence key answers SignWithPresence.
type testPresenceMode int

const (
	testPresenceSign testPresenceMode = iota
	testPresenceRefuse
	testPresenceGarbage
	testPresencePanic
)

// The app's presence key: P-256 as ecdsa-p256-sha256 holds it, answering as its mode says, recording each message it
// was asked to sign and how many statements had been posted when it was asked.
type testPresenceSigner struct {
	key   *ecdsa.PrivateKey
	keyId []byte

	stateLock sync.Mutex
	mode      testPresenceMode
	messages  [][]byte
	// read at each call: how many statements the api had taken by then
	postCount   func() int
	postsBefore []int
}

func newTestPresenceSigner(t *testing.T) *testPresenceSigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := key.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	keyId := sha256.Sum256(publicKey)
	return &testPresenceSigner{key: key, keyId: keyId[:]}
}

// The key's row, as the presence row registers it live for its device.
func (self *testPresenceSigner) keyRow(t *testing.T) *bridge.KeyRow {
	t.Helper()
	publicKey, err := self.key.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return &bridge.KeyRow{
		KeyId:     self.keyId,
		Alg:       bridge.KeyAlg_KEY_ALG_ECDSA_P256_SHA256,
		PublicKey: publicKey,
		Purpose:   bridge.KeyPurpose_KEY_PURPOSE_PRESENCE,
	}
}

func (self *testPresenceSigner) setMode(mode testPresenceMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.mode = mode
}

func (self *testPresenceSigner) GetAttestation() string {
	return ""
}

func (self *testPresenceSigner) SignWithPresence(messageBase64 string) string {
	message, err := base64.StdEncoding.DecodeString(messageBase64)
	self.stateLock.Lock()
	mode := self.mode
	self.messages = append(self.messages, message)
	if self.postCount != nil {
		self.postsBefore = append(self.postsBefore, self.postCount())
	}
	self.stateLock.Unlock()
	if err != nil {
		return ""
	}
	switch mode {
	case testPresenceRefuse:
		return ""
	case testPresenceGarbage:
		return "not-base64-evidence!"
	case testPresencePanic:
		panic("the authenticator crashed")
	}
	digest := sha256.Sum256(message)
	signature, err := self.key.Sign(cryptorand.Reader, digest[:], crypto.SHA256)
	if err != nil {
		return ""
	}
	encoded, err := proto.Marshal(&bridge.WebAuthnEvidence{KeyId: self.keyId, Signature: signature})
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

// The messages it was asked to sign, in order.
func (self *testPresenceSigner) asked() [][]byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.messages)
}

// Whether evidence is this key's signature over message.
func (self *testPresenceSigner) verifies(evidenceBytes []byte, message []byte) bool {
	evidence := &bridge.WebAuthnEvidence{}
	if err := proto.Unmarshal(evidenceBytes, evidence); err != nil || !bytes.Equal(evidence.GetKeyId(), self.keyId) {
		return false
	}
	digest := sha256.Sum256(message)
	return ecdsa.VerifyASN1(&self.key.PublicKey, digest[:], evidence.GetSignature())
}

// One call of a callback, answered.
func awaitTestApprovalsResult(t *testing.T, call func(callback SidecarResultCallback)) *SidecarResult {
	t.Helper()
	callback, results := testSidecarResultCallback()
	call(callback)
	select {
	case result := <-results:
		return result
	case <-time.After(testSidecarTimeout):
		t.Fatal("the call never answered")
		return nil
	}
}

// One preview, answered.
func awaitTestRevokePlan(t *testing.T, approvals *SidecarApprovals, sidecarId *Id, reason string, lostAtMillis int64, sinceN int64) *SidecarRevokePlan {
	t.Helper()
	plans := make(chan *SidecarRevokePlan, 1)
	approvals.PreviewRevokeDevice(sidecarId, reason, lostAtMillis, sinceN, connect.NewApiCallback[*SidecarRevokePlan](func(plan *SidecarRevokePlan, err error) {
		if err != nil {
			plan = &SidecarRevokePlan{Message: err.Error()}
		}
		plans <- plan
	}))
	select {
	case plan := <-plans:
		return plan
	case <-time.After(testSidecarTimeout):
		t.Fatal("the preview never answered")
		return nil
	}
}

// The item of approvalId in list, nil for none.
func testApprovalItem(list *SidecarApprovalList, approvalId *Id) *SidecarApprovalItem {
	if list == nil {
		return nil
	}
	for _, item := range list.getAll() {
		if item.ApprovalId != nil && approvalId != nil && item.ApprovalId.Cmp(approvalId) == 0 {
			return item
		}
	}
	return nil
}

// An admission of a new device of the authority's member, opened in the core.
func openTestAdmission(t *testing.T, local *Sidecar, authority *testApprovalsAuthority) (*Id, sidecar.Id) {
	t.Helper()
	newDeviceId := sidecar.NewId()
	identityPublicKey := make([]byte, 32)
	if _, err := cryptorand.Read(identityPublicKey); err != nil {
		t.Fatal(err)
	}
	approvalId, err := local.Core().OpenAdmission(&sidecar.SidecarAdmissionRequest{
		SidecarId:         newDeviceId,
		MemberId:          authority.memberId,
		IdentityPublicKey: identityPublicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return newId([16]byte(approvalId)), newDeviceId
}

// Each item takes one decision: an admission approved here signs its SidecarAdmit once, a second Decide of it, also
// one racing the first, is refused with nothing signed, and the core no longer lists it. A decision the item does not
// offer is refused and leaves it decidable.
func TestDecideIsSingleUse(t *testing.T) {
	local, authority, api := newTestApprovalsSidecar(t, nil)
	approvals := local.GetApprovals()
	if approvals == nil {
		t.Fatal("no approvals")
	}
	approvalId, newDeviceId := openTestAdmission(t, local, authority)
	item := testApprovalItem(approvals.GetPending(), approvalId)
	if item == nil {
		t.Fatal("the admission is not listed")
	}
	if item.Kind != SidecarApprovalKindAdmission || item.SidecarId == nil || sidecar.Id(item.SidecarId.id) != newDeviceId {
		t.Fatalf("the item: %+v", item)
	}
	if !item.Decisions.Contains(SidecarDecisionApprove) || !item.Decisions.Contains(SidecarDecisionDeny) {
		t.Fatalf("the item's decisions: %v", item.Decisions.getAll())
	}

	// a decision the kind does not take is refused, signs nothing and frees the item
	refused := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
		approvals.Decide(approvalId, SidecarDecisionRevoke, callback)
	})
	if refused.Ok || refused.Code != SidecarCodeApprovalNotEligible {
		t.Fatalf("revoke of an admission: %+v", refused)
	}

	// two at once: one decides, the other is refused before anything is signed
	results := make(chan *SidecarResult, 2)
	for range 2 {
		callback := connect.NewApiCallback[*SidecarResult](func(result *SidecarResult, err error) {
			if err != nil {
				result = &SidecarResult{Message: err.Error()}
			}
			results <- result
		})
		approvals.Decide(approvalId, SidecarDecisionApprove, callback)
	}
	okCount, decidedCount := 0, 0
	for range 2 {
		select {
		case result := <-results:
			switch {
			case result.Ok:
				okCount += 1
			case result.Code == SidecarCodeApprovalDecided:
				decidedCount += 1
			default:
				t.Errorf("a racing Decide: %+v", result)
			}
		case <-time.After(testSidecarTimeout):
			t.Fatal("a Decide never answered")
		}
	}
	if okCount != 1 || decidedCount != 1 {
		t.Fatalf("%d decided and %d refused", okCount, decidedCount)
	}
	statements := api.postedStatements(t)
	if len(statements) != 1 || bridge.SignedType(statements[0].GetType()) != bridge.SignedType_SIGNED_SIDECAR_ADMIT {
		t.Fatalf("the statements posted: %d", len(statements))
	}
	admit := &protocol.SidecarAdmit{}
	if err := proto.Unmarshal(statements[0].GetBody(), admit); err != nil || !bytes.Equal(admit.GetSidecarId(), newDeviceId.Bytes()) {
		t.Fatalf("the admission names another device: %v", err)
	}

	// and again, later: refused, with nothing more signed
	again := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
		approvals.Decide(approvalId, SidecarDecisionApprove, callback)
	})
	if again.Ok || again.Code != SidecarCodeApprovalDecided {
		t.Fatalf("a second Decide: %+v", again)
	}
	if testApprovalItem(approvals.GetPending(), approvalId) != nil {
		t.Error("the decided admission is still listed")
	}
	if count := api.postCount(); count != 1 {
		t.Errorf("%d statements posted", count)
	}

	// an id nothing lists is unknown, not decided
	unknown := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
		approvals.Decide(NewId(), SidecarDecisionDeny, callback)
	})
	if unknown.Ok || unknown.Code != SidecarCodeApprovalUnknown {
		t.Fatalf("an unknown id: %+v", unknown)
	}
}

// Every kind the core lists maps to its bound item: the local core's admission with its ids and decisions; over the
// daemon every authority kind, the cleanup as admissions_near_cap with the list a revoke signs, a request rendered
// and one shown refused; the core's request, access request and cleanup list mappings field by field; and the kind
// names are the core's own.
func TestEveryPendingKindMaps(t *testing.T) {
	for own, core := range map[string]string{
		SidecarApprovalKindAdmission:      sidecar.SidecarApprovalAdmission,
		SidecarApprovalKindAdmissionAlarm: sidecar.SidecarApprovalAdmissionAlarm,
		SidecarApprovalKindRecoveryWindow: sidecar.SidecarApprovalRecoveryWindow,
		SidecarApprovalKindRecoveryRebind: sidecar.SidecarApprovalRecoveryRebind,
		SidecarApprovalKindRecoveryUnseen: sidecar.SidecarApprovalRecoveryUnseen,
		SidecarApprovalKindRequest:        sidecar.SidecarApprovalRequest,
	} {
		if own != core || sidecarApprovalKindOfCore(core) != own {
			t.Errorf("the kind %q is the core's %q", own, core)
		}
	}
	if sidecarApprovalKindOfCore(sidecar.SidecarApprovalCleanup) != SidecarApprovalKindAdmissionsNearCap {
		t.Error("the cleanup is not admissions_near_cap")
	}
	for own, core := range map[string]string{
		SidecarDecisionApprove: sidecar.SidecarDecisionApprove,
		SidecarDecisionDeny:    sidecar.SidecarDecisionDeny,
		SidecarDecisionRevoke:  sidecar.SidecarDecisionRevoke,
	} {
		if own != core {
			t.Errorf("the decision %q is the core's %q", own, core)
		}
	}

	// the local core's admission
	local, authority, _ := newTestApprovalsSidecar(t, nil)
	approvalId, newDeviceId := openTestAdmission(t, local, authority)
	item := testApprovalItem(local.GetApprovals().GetPending(), approvalId)
	switch {
	case item == nil:
		t.Fatal("the admission is not listed")
	case item.Kind != SidecarApprovalKindAdmission, item.SidecarId == nil, sidecar.Id(item.SidecarId.id) != newDeviceId,
		item.MemberId == nil, sidecar.Id(item.MemberId.id) != authority.memberId, item.RaisedMillis == 0,
		item.SafetyNumber != "" && len(item.SafetyNumber) != 60:
		t.Errorf("the admission's item: %+v", item)
	}

	// every authority kind and the requests over the daemon
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	ids := map[string]sidecar.Id{}
	for _, kind := range []string{"admission", "admission_alarm", "recovery_window", "recovery_rebind", "recovery_unseen", "cleanup"} {
		ids[kind] = sidecar.NewId()
	}
	requestId, workerId, memberId := sidecar.NewId(), sidecar.NewId(), sidecar.NewId()
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		switch request.Method {
		case sidecarControlMethodApprovalsList:
			items := []map[string]any{}
			for kind, id := range ids {
				items = append(items, map[string]any{
					"approval_id": id, "kind": kind, "sidecar_id": workerId, "member_id": memberId, "position": 12,
					"decisions": []string{"revoke", "deny"},
				})
			}
			return map[string]any{"items": items}, nil, true
		case sidecarControlMethodApprovalsCleanupList:
			return map[string]any{
				"admission_count": 90, "admission_cap": 100, "presence": true,
				"members": []map[string]any{{"member_id": memberId, "workers": []map[string]any{{
					"sidecar_id": workerId, "admitted_at": 7, "admitted_time_ms": 1700000000000, "last_change_ms": 1700000001000, "signatures": 2,
				}}}},
			}, nil, true
		case sidecarControlMethodApprovalsAppRequests:
			return map[string]any{"items": []map[string]any{
				{"approval_id": requestId, "entry_id": sidecar.NewId(), "sidecar_id": workerId, "kind": "dns_set", "template": true, "text": "request: dns_set",
					"channel": "device", "decisions": []string{"approve", "deny"}},
				{"entry_id": sidecar.NewId(), "refused": "the request's bytes do not check", "decisions": []string{}},
			}}, nil, true
		}
		return nil, nil, false
	}
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	pending := remote.GetApprovals().GetPending()
	if pending == nil {
		t.Fatal("the daemon's pending list did not read")
	}
	for kind, id := range ids {
		item := testApprovalItem(pending, newId([16]byte(id)))
		if item == nil {
			t.Errorf("%s is not listed", kind)
			continue
		}
		if item.Kind != sidecarApprovalKindOfCore(kind) || item.Position != 12 || item.SidecarId == nil || sidecar.Id(item.SidecarId.id) != workerId ||
			!item.Decisions.Contains(SidecarDecisionRevoke) {
			t.Errorf("%s's item: %+v", kind, item)
		}
		if kind == "cleanup" {
			cleanup := item.Cleanup
			if cleanup == nil || cleanup.AdmissionCount != 90 || cleanup.AdmissionCap != 100 || !cleanup.Presence || cleanup.Workers.Len() != 1 {
				t.Fatalf("the cleanup's list: %+v", cleanup)
			}
			worker := cleanup.Workers.Get(0)
			if sidecar.Id(worker.SidecarId.id) != workerId || sidecar.Id(worker.MemberId.id) != memberId || worker.AdmittedPosition != 7 ||
				worker.AdmittedMillis != 1700000000000 || worker.LastChangeMillis != 1700000001000 || worker.Signatures != 2 {
				t.Errorf("the cleanup's worker: %+v", worker)
			}
		} else if item.Cleanup != nil {
			t.Errorf("%s carries a cleanup list", kind)
		}
	}
	request := testApprovalItem(pending, newId([16]byte(requestId)))
	if request == nil || request.Kind != SidecarApprovalKindRequest || request.Action != "dns_set" || !request.Template ||
		request.Channel != "device" || request.Text == "" || !request.Decisions.Contains(SidecarDecisionApprove) ||
		request.SidecarId == nil || sidecar.Id(request.SidecarId.id) != workerId {
		t.Fatalf("the request: %+v", request)
	}
	refusedCount := 0
	for _, each := range pending.getAll() {
		if each.Kind == SidecarApprovalKindRequest && each.ApprovalId == nil {
			refusedCount += 1
			if each.Refused == "" || each.Decisions.Len() != 0 {
				t.Errorf("the refused request: %+v", each)
			}
		}
	}
	if refusedCount != 1 {
		t.Errorf("%d requests shown refused", refusedCount)
	}

	// the core's request, rendered and refused
	renderedId, requesterId := sidecar.NewId(), sidecar.NewId()
	rendered := sidecarRequestItemOf(&sidecar.SidecarRequestItem{
		ApprovalId: renderedId,
		Render:     &sidecar.ApprovalRender{ApprovalId: renderedId, KindName: "mail_send", Template: true, RequesterSidecarId: requesterId, Summary: "agent words"},
		Channel:    protocol.ApprovalChannel_APPROVAL_CHANNEL_PRESENCE,
		Decisions:  []string{sidecar.SidecarDecisionApprove, sidecar.SidecarDecisionDeny},
	})
	if sidecar.Id(rendered.ApprovalId.id) != renderedId || rendered.Action != "mail_send" || !rendered.Template || rendered.Channel != "presence" ||
		sidecar.Id(rendered.SidecarId.id) != requesterId || !strings.Contains(rendered.Text, "untrusted text") || rendered.Decisions.Len() != 2 {
		t.Errorf("a rendered request: %+v", rendered)
	}
	shownRefused := sidecarRequestItemOf(&sidecar.SidecarRequestItem{Refused: "does not check", Decisions: []string{}})
	if shownRefused.ApprovalId != nil || shownRefused.Refused != "does not check" || shownRefused.Decisions.Len() != 0 {
		t.Errorf("a refused request: %+v", shownRefused)
	}

	// the core's access request
	accessId, repoId := sidecar.NewId(), sidecar.NewId()
	access := sidecarAccessItemOf(&sidecar.DirectoryRequestStub{
		RequestId: accessId.String(), TargetKind: SidecarAccessTargetRepo, Target: repoId.String(), Access: SidecarAccessReader,
		State: "pending", Decidable: true,
	})
	if access == nil || access.Kind != SidecarApprovalKindAccessRequest || sidecar.Id(access.ApprovalId.id) != accessId ||
		access.AccessRequest == nil || sidecar.Id(access.AccessRequest.RepoId.id) != repoId || access.Decisions.Len() != 2 {
		t.Errorf("an access request: %+v", access)
	}
	if sidecarAccessItemOf(&sidecar.DirectoryRequestStub{RequestId: accessId.String(), Decidable: false}) != nil {
		t.Error("an access request this device does not decide is listed")
	}

	// the core's cleanup list
	cleanup := sidecarCleanupListOf(&sidecar.SidecarCleanupList{
		AdmissionCount: 95, AdmissionCap: 100, Presence: true,
		Members: []sidecar.SidecarCleanupMember{{MemberId: memberId, Workers: []sidecar.SidecarCleanupWorker{
			{SidecarId: workerId, AdmittedAt: 3, AdmittedTime: time.UnixMilli(1700000000000), Signatures: 1},
		}}},
	})
	if cleanup.AdmissionCount != 95 || cleanup.Workers.Len() != 1 || cleanup.Workers.Get(0).AdmittedMillis != 1700000000000 ||
		sidecar.Id(cleanup.Workers.Get(0).MemberId.id) != memberId {
		t.Errorf("a cleanup list: %+v", cleanup)
	}
}

// A test's backend: the items it holds, and the decisions it took.
type testApprovalsBackend struct {
	stateLock sync.Mutex
	items     []*SidecarApprovalItem
	decided   []string
	// the previews each read asked for
	previews []bool
}

func (self *testApprovalsBackend) set(items ...*SidecarApprovalItem) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.items = items
}

func (self *testApprovalsBackend) pending(ctx context.Context, preview bool) ([]*SidecarApprovalItem, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.previews = append(self.previews, preview)
	items := []*SidecarApprovalItem{}
	for _, item := range self.items {
		copied := *item
		items = append(items, &copied)
	}
	return items, nil
}

func (self *testApprovalsBackend) decide(ctx context.Context, item *SidecarApprovalItem, decision string, signer SidecarPresenceSigner) (string, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.decided = append(self.decided, item.ApprovalId.String()+" "+decision)
	self.items = slices.DeleteFunc(self.items, func(each *SidecarApprovalItem) bool { return each.ApprovalId.Cmp(item.ApprovalId) == 0 })
	return "", nil
}

func (self *testApprovalsBackend) devices(ctx context.Context) ([]*SidecarDevice, error) {
	return nil, sidecar.ErrSidecarDevicesPrecondition
}

func (self *testApprovalsBackend) previewRevoke(ctx context.Context, sidecarId sidecar.Id, reason string, lostAt time.Time, sinceN uint64) (*SidecarRevokePlan, error) {
	return nil, sidecar.ErrSidecarDevicesPrecondition
}

func (self *testApprovalsBackend) revoke(ctx context.Context, planId sidecar.Id, revokeKeyIds [][]byte, signer SidecarPresenceSigner) (*sidecarRevokeOutcome, error) {
	return nil, sidecar.ErrSidecarDevicesPrecondition
}

// A listener's deliveries, in order.
type testApprovalsListener struct {
	lists chan *SidecarApprovalList
}

func (self *testApprovalsListener) ApprovalsChanged(pending *SidecarApprovalList) {
	self.lists <- pending
}

// The next delivery whose item count is want, skipping older ones.
func (self *testApprovalsListener) next(t *testing.T, want int) *SidecarApprovalList {
	t.Helper()
	deadline := time.After(testSidecarTimeout)
	for {
		select {
		case list := <-self.lists:
			if list.Len() == want {
				return list
			}
		case <-deadline:
			t.Fatalf("no list of %d items", want)
			return nil
		}
	}
}

// A listener hears the list when it is added, after an event of a kind that may change it, and after a Decide; every
// delivery is read with the cleanup lists shown, and Decide's own read is not, so a revoke signs what was shown.
func TestPendingListenerHearsEventsAndDecisions(t *testing.T) {
	world := newTestSidecarWorld(t)
	local := world.open(t)
	backend := &testApprovalsBackend{}
	local.stateLock.Lock()
	local.approvalsBackend = backend
	settings := defaultSidecarApprovalsSettings()
	settings.PendingRefreshInterval = 0
	local.approvalsSettings = settings
	local.stateLock.Unlock()
	approvals := local.GetApprovals()
	first := &SidecarApprovalItem{ApprovalId: NewId(), Kind: SidecarApprovalKindAdmission, Decisions: newSidecarDecisionList([]string{SidecarDecisionDeny})}
	backend.set(first)

	listener := &testApprovalsListener{lists: make(chan *SidecarApprovalList, 16)}
	sub := approvals.AddPendingListener(listener)
	defer sub.Close()
	listener.next(t, 1)

	// an alarm raised: the listener hears the new item
	second := &SidecarApprovalItem{ApprovalId: NewId(), Kind: SidecarApprovalKindAdmissionAlarm, Decisions: newSidecarDecisionList([]string{SidecarDecisionDeny})}
	backend.set(first, second)
	local.events.offer(sidecar.SidecarEvent{Kind: sidecar.SidecarEventAdmissionAlarm})
	listener.next(t, 2)

	// a decision: the listener hears it gone
	result := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
		approvals.Decide(second.ApprovalId, SidecarDecisionDeny, callback)
	})
	if !result.Ok {
		t.Fatalf("the decision: %+v", result)
	}
	listener.next(t, 1)

	backend.stateLock.Lock()
	previews := slices.Clone(backend.previews)
	decided := slices.Clone(backend.decided)
	backend.stateLock.Unlock()
	if len(decided) != 1 || !strings.HasSuffix(decided[0], " deny") {
		t.Errorf("decided %v", decided)
	}
	// the one read without a preview is Decide's
	if unpreviewed := len(previews) - len(slices.DeleteFunc(slices.Clone(previews), func(preview bool) bool { return !preview })); unpreviewed != 1 {
		t.Errorf("%d reads without the lists shown: %v", unpreviewed, previews)
	}
}

// Over the daemon, reads go unconfirmed and a decision confirmed, by the Confirm method of the item's kind: an
// authority item's approvals.decide, a request's approvals.app.request.
func TestDecideRemoteGoesConfirmed(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	authorityId, requestId := sidecar.NewId(), sidecar.NewId()
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		switch request.Method {
		case sidecarControlMethodApprovalsList:
			return map[string]any{"items": []map[string]any{{"approval_id": authorityId, "kind": "recovery_window", "decisions": []string{"approve", "deny"}}}}, nil, true
		case sidecarControlMethodApprovalsAppRequests:
			return map[string]any{"items": []map[string]any{{"approval_id": requestId, "entry_id": sidecar.NewId(), "kind": "mail_send", "decisions": []string{"approve", "deny"}}}}, nil, true
		case sidecarControlMethodApprovalsDecide, sidecarControlMethodApprovalsAppRequest:
			if !request.UrConfirmed {
				return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcConfirmationRequired, Message: "confirmation required"}, true
			}
			return map[string]any{}, nil, true
		}
		return nil, nil, false
	}
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	approvals := remote.GetApprovals()
	for _, decided := range []struct {
		id       sidecar.Id
		decision string
	}{{authorityId, SidecarDecisionDeny}, {requestId, SidecarDecisionApprove}} {
		result := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
			approvals.Decide(newId([16]byte(decided.id)), decided.decision, callback)
		})
		if !result.Ok {
			t.Fatalf("a remote decision: %+v", result)
		}
	}
	seen := map[string][]bool{}
	for _, request := range daemon.seen() {
		seen[request.Method] = append(seen[request.Method], request.UrConfirmed)
		if request.Method == sidecarControlMethodApprovalsDecide {
			params := &sidecarControlDecideParams{}
			if err := json.Unmarshal(request.Params, params); err != nil || params.Decision != SidecarDecisionDeny || sidecar.Id(params.ApprovalId.id) != authorityId {
				t.Errorf("approvals.decide's params: %s", request.Params)
			}
		}
	}
	for _, method := range []string{sidecarControlMethodApprovalsList, sidecarControlMethodApprovalsAppRequests} {
		if len(seen[method]) == 0 || slices.Contains(seen[method], true) {
			t.Errorf("%s: %v", method, seen[method])
		}
	}
	for _, method := range []string{sidecarControlMethodApprovalsDecide, sidecarControlMethodApprovalsAppRequest} {
		if len(seen[method]) != 1 || !seen[method][0] {
			t.Errorf("%s: %v", method, seen[method])
		}
	}
}

// The control methods and fields the remote backend sends and reads are cli's ur approve's and the root's app request
// and devices methods', read from their source since cli is package main and the root's method files build on no
// mobile leg, so a renamed method or field fails here rather than at the daemon, which reads params strictly.
func TestApprovalsControlWireMatchesUr(t *testing.T) {
	cliDirectory := filepath.Join("..", "sidecar", "cli")
	cli := testSidecarDeclarations(t, filepath.Join(cliDirectory, "cmd_approve.go"), filepath.Join(cliDirectory, "cmd_approve_request.go"))
	for own, name := range map[string]string{
		sidecarControlMethodApprovalsList:        "urMethodApprovalsList",
		sidecarControlMethodApprovalsDecide:      "urMethodApprovalsDecide",
		sidecarControlMethodApprovalsCleanupList: "urMethodApprovalsCleanupList",
	} {
		if !strings.Contains(cli[name], strconv.Quote(own)) {
			t.Errorf("%s is not %q: %s", name, own, cli[name])
		}
	}
	for declaration, fields := range map[string][]string{
		"urApprovalItem":             {`json:"approval_id"`, `json:"kind"`, `json:"sidecar_id,omitempty"`, `json:"member_id,omitempty"`, `json:"position,omitempty"`, `json:"safety_number,omitempty"`, `json:"decisions"`},
		"urApprovalsReply":           {`json:"items"`},
		"urDecideParams":             {`json:"approval_id"`, `json:"decision"`},
		"urApprovalRevokePlanParams": {`json:"approval_id"`},
		"urCleanupListReply":         {`json:"admission_count"`, `json:"admission_cap"`, `json:"members"`, `json:"presence,omitempty"`},
		"urCleanupMember":            {`json:"member_id"`, `json:"workers"`},
		"urCleanupWorker":            {`json:"sidecar_id"`, `json:"admitted_at"`, `json:"admitted_time_ms,omitempty"`, `json:"last_change_ms,omitempty"`, `json:"signatures"`},
	} {
		for _, field := range fields {
			if !strings.Contains(cli[declaration], field) {
				t.Errorf("%s lacks %s", declaration, field)
			}
		}
	}
	root := testSidecarDeclarations(t, filepath.Join("..", "sidecar", "daemon_devices.go"), filepath.Join("..", "sidecar", "daemon_approvals_app.go"),
		filepath.Join("..", "sidecar", "daemon_approvals.go"))
	for own, name := range map[string]string{
		sidecarControlMethodDevicesList:           "DaemonMethodDevicesList",
		sidecarControlMethodDevicesPreview:        "DaemonMethodDevicesPreview",
		sidecarControlMethodDevicesRevoke:         "DaemonMethodDevicesRevoke",
		sidecarControlMethodApprovalsAppRequests:  "DaemonMethodApprovalsAppRequests",
		sidecarControlMethodApprovalsAppRequest:   "DaemonMethodApprovalsAppRequest",
		sidecarControlMethodApprovalsDecideBegin:  "DaemonMethodApprovalsDecideBegin",
		sidecarControlMethodApprovalsDecideFinish: "DaemonMethodApprovalsDecideFinish",
	} {
		if !strings.Contains(root[name], strconv.Quote(own)) {
			t.Errorf("%s is not %q: %s", name, own, root[name])
		}
	}
	for declaration, fields := range map[string][]string{
		"DaemonDeviceRefusal":        {`json:"code"`, `json:"position,omitempty"`},
		"DaemonDevice":               {`json:"sidecar_id"`, `json:"name,omitempty"`, `json:"this,omitempty"`, `json:"read_only,omitempty"`, `json:"ephemeral,omitempty"`, `json:"state"`, `json:"admitted_at_ms,omitempty"`, `json:"last_change_ms,omitempty"`},
		"DaemonDevicesListParams":    {`json:"network,omitempty"`},
		"DaemonDevicesListReply":     {`json:"devices"`},
		"DaemonDeviceRevokeItem":     {`json:"effect"`, `json:"kind"`, `json:"id"`, `json:"name,omitempty"`, `json:"position,omitempty"`},
		"DaemonDevicesPreviewParams": {`json:"network,omitempty"`, `json:"device"`, `json:"reason"`, `json:"lost_at_ms,omitempty"`, `json:"since_n,omitempty"`},
		"DaemonDeviceRevokePlan":     {`json:"plan_id"`, `json:"sidecar_id"`, `json:"name,omitempty"`, `json:"reason"`, `json:"since_n,omitempty"`, `json:"since_time_ms,omitempty"`, `json:"items"`, `json:"signatures"`, `json:"presence,omitempty"`},
		"DaemonDevicesPreviewReply":  {`json:"plan,omitempty"`, `json:"refusal,omitempty"`},
		"DaemonDevicesRevokeParams":  {`json:"network,omitempty"`, `json:"plan_id"`, `json:"revoke_key_ids,omitempty"`},
		"DaemonDevicesRevokeReply":   {`json:"position,omitempty"`, `json:"proposal_id,omitempty"`, `json:"joined,omitempty"`, `json:"revoked_key_ids,omitempty"`, `json:"pending,omitempty"`, `json:"refusal,omitempty"`},
		"DaemonApprovalsAppParams":   {`json:"network,omitempty"`},
		"DaemonApprovalsAppItem": {`json:"approval_id,omitempty"`, `json:"entry_id"`, `json:"sidecar_id,omitempty"`, `json:"kind,omitempty"`, `json:"template,omitempty"`,
			`json:"text,omitempty"`, `json:"refused,omitempty"`, `json:"channel,omitempty"`, `json:"decisions"`},
		"DaemonApprovalsAppReply":        {`json:"items"`},
		"DaemonApprovalsAppDecideParams": {`json:"network,omitempty"`, `json:"approval_id"`, `json:"decision"`},
		"DaemonApprovalsDecideParams":    {`json:"network,omitempty"`, `json:"approval_id"`, `json:"decision"`},
		"DaemonApprovalsDecideReply":     {`json:"pending,omitempty"`, `json:"refusal,omitempty"`},
	} {
		for _, field := range fields {
			if !strings.Contains(root[declaration], field) {
				t.Errorf("%s lacks %s", declaration, field)
			}
		}
	}
}
