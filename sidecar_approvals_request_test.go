//go:build !ios_extension && !js && !ios && !android

package sdk

// §16.8's request kind through SidecarApprovals on app hosts (MCP-M8-01; §16.6's host table, §18.5, D-SDK-9, D-MCP-14,
// D-MCP-19, I-8): §22.7's devices row's app hosts at the binding, the T4 surrogate's own layer on this host. A phone in
// local mode is a real enrolled core whose approver side reads an installed approval log: it lists another sidecar's
// request on channel device and its own on presence, signs the first by its identity key and the second by the app's
// SignWithPresence over the decision's §6.9 bytes on both of §16.6's profiles (FS-124's app-host decision cases),
// signs nothing on a refusal, an answer that is no evidence, a panic, no signer or a key the member does not hold,
// lists a template with deny alone, and lists nothing where it is no approval-capable device. Its approval part's hint
// raises approval_pending, which a pending listener hears. A desktop in remote mode lists and decides over the
// daemon's app methods, confirmed, on the pairing's network, its daemon's own request listed with no decision and never
// sent; a daemon from before those methods lists no request. Every wait is on a channel or a callback's answer.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// The approval family's log as an approver's device reads it, requests the test files, each leaving the list once a
// decision is filed on it. The embedded interface supplies ApproverLog's unexported log listing, which a cancel alone
// calls and these tests never do.
type testRequestLog struct {
	sidecar.ApproverLog
	logId sidecar.Id

	stateLock sync.Mutex
	pending   []*sidecar.RecordPendingApproval
	filed     [][]byte
}

func (self *testRequestLog) Pending(ctx context.Context) ([]*sidecar.RecordPendingApproval, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.pending), nil
}

func (self *testRequestLog) FileDecision(ctx context.Context, logId sidecar.Id, storedSigned []byte, runId *sidecar.Id) (*sidecar.RecordEntryRef, error) {
	decision, err := sidecar.ParseStoredApprovalDecision(storedSigned)
	if err != nil {
		return nil, err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.filed = append(self.filed, bytes.Clone(storedSigned))
	self.pending = slices.DeleteFunc(self.pending, func(waiting *sidecar.RecordPendingApproval) bool {
		return bytes.Equal(waiting.Request.Request.GetApprovalId(), decision.Decision.GetApprovalId())
	})
	return &sidecar.RecordEntryRef{LogId: logId, EntryId: sidecar.NewId()}, nil
}

func (self *testRequestLog) FileCancel(ctx context.Context, logId sidecar.Id, storedSigned []byte) (*sidecar.RecordEntryRef, error) {
	return nil, sidecar.ErrRecordApprovalUnknown
}

// The decisions filed so far, parsed.
func (self *testRequestLog) decisions(t *testing.T) []*sidecar.StoredApprovalDecision {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	decisions := []*sidecar.StoredApprovalDecision{}
	for _, filed := range self.filed {
		decision, err := sidecar.ParseStoredApprovalDecision(filed)
		if err != nil {
			t.Fatal(err)
		}
		decisions = append(decisions, decision)
	}
	return decisions
}

// A request requester files for a dns_set of value, the authority's member its approver, waiting in the log; scope makes
// it a multi-use template.
func (self *testRequestLog) file(t *testing.T, requester sidecar.Id, memberId sidecar.Id, value string, scope bool) *sidecar.StoredApprovalRequest {
	t.Helper()
	action, err := sidecar.NewCanonicalAction(&protocol.CanonicalAction{
		Kind: uint32(sidecar.SidecarActionDnsSet), DnsName: "app.example", DnsType: "A", DnsValue: value, DnsTtl: 300, Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	approvalId := sidecar.NewId()
	actionId := action.ActionId(approvalId)
	payloadSha256 := action.PayloadSha256()
	stored, err := proto.MarshalOptions{Deterministic: true}.Marshal(&protocol.ApprovalRequest{
		ApprovalId: approvalId.Bytes(), ActionId: actionId[:], RunId: sidecar.NewId().Bytes(), CanonicalAction: action.Bytes,
		PayloadSha256: payloadSha256[:], Summary: "the agent's words", Risk: uint32(sidecar.SidecarRiskNormal),
		RequesterSidecarId: requester.Bytes(), Approvers: [][]byte{memberId.Bytes()},
		NotAfterMillis: time.Now().Add(time.Hour).UnixMilli(), Version: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := sidecar.ParseStoredApprovalRequest(stored)
	if err != nil {
		t.Fatal(err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.pending = append(self.pending, &sidecar.RecordPendingApproval{LogId: self.logId, EntryId: sidecar.NewId(), Request: request, WriterSidecarId: requester})
	return request
}

// A phone in local mode on signer: an enrolled core, its verified state the authority it answers, its approval log
// installed in place of a record.
func newTestRequestPhone(t *testing.T, signer SidecarPresenceSigner) (*Sidecar, *testApprovalsAuthority, *testRequestLog) {
	t.Helper()
	local, authority, _ := newTestApprovalsSidecar(t, signer)
	log := &testRequestLog{logId: sidecar.NewId()}
	t.Cleanup(local.Core().InstallApproverLog(log))
	return local, authority, log
}

// Registers row as a live presence key of this device's member.
func (self *testApprovalsAuthority) addMemberKey(row *bridge.KeyRow) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	row.MemberId = self.memberId.Bytes()
	self.liveKeys = append(self.liveKeys, row)
}

// The app's presence key as webauthn-es256 holds it, a passkey through the platform's API: each SignWithPresence an
// assertion over the message with rpId ur.io, user present and verified, the counter moving on.
type testWebAuthnPresenceSigner struct {
	key   *ecdsa.PrivateKey
	keyId []byte

	stateLock sync.Mutex
	signCount uint32
	messages  [][]byte
}

func newTestWebAuthnPresenceSigner(t *testing.T) *testWebAuthnPresenceSigner {
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
	return &testWebAuthnPresenceSigner{key: key, keyId: keyId[:16], signCount: 1}
}

func (self *testWebAuthnPresenceSigner) keyRow(t *testing.T) *bridge.KeyRow {
	t.Helper()
	publicKey, err := self.key.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return &bridge.KeyRow{KeyId: self.keyId, Alg: bridge.KeyAlg_KEY_ALG_WEBAUTHN_ES256, PublicKey: publicKey, Purpose: bridge.KeyPurpose_KEY_PURPOSE_PRESENCE}
}

func (self *testWebAuthnPresenceSigner) GetAttestation() string {
	return ""
}

func (self *testWebAuthnPresenceSigner) SignWithPresence(messageBase64 string) string {
	message, err := base64.StdEncoding.DecodeString(messageBase64)
	if err != nil {
		return ""
	}
	signCount := func() uint32 {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.messages = append(self.messages, message)
		self.signCount += 1
		return self.signCount
	}()
	rpIdHash := sha256.Sum256([]byte("ur.io"))
	// user present and user verified
	authenticatorData := append(rpIdHash[:], 0x01|0x04)
	authenticatorData = binary.BigEndian.AppendUint32(authenticatorData, signCount)
	challenge := sha256.Sum256(message)
	clientDataJson := fmt.Appendf(nil, `{"type":"webauthn.get","challenge":%q,"origin":%q,"crossOrigin":false}`,
		base64.RawURLEncoding.EncodeToString(challenge[:]), "https://ur.io")
	clientDataHash := sha256.Sum256(clientDataJson)
	digest := sha256.Sum256(append(bytes.Clone(authenticatorData), clientDataHash[:]...))
	signature, err := ecdsa.SignASN1(cryptorand.Reader, self.key, digest[:])
	if err != nil {
		return ""
	}
	encoded, err := proto.Marshal(&bridge.WebAuthnEvidence{
		KeyId: self.keyId, Signature: signature, AuthenticatorData: authenticatorData, ClientDataJson: clientDataJson,
	})
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

func (self *testWebAuthnPresenceSigner) asked() [][]byte {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.messages)
}

// One Decide, answered.
func testRequestDecide(t *testing.T, approvals *SidecarApprovals, approvalId sidecar.Id, decision string) *SidecarResult {
	t.Helper()
	return awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
		approvals.Decide(newId([16]byte(approvalId)), decision, callback)
	})
}

// A phone in local mode lists another sidecar's request on channel device and its own on presence, each with the
// requester, the action's kind and its render; it signs the first by its identity key and the second by the app's
// presence key over the decision's §6.9 bytes, which verifies under §16.6's per-use profile; each takes one decision.
func TestRequestKindOnALocalPhone(t *testing.T) {
	signer := newTestPresenceSigner(t)
	local, authority, log := newTestRequestPhone(t, signer)
	authority.addMemberKey(signer.keyRow(t))
	identity := local.Core().Identity()
	requester := sidecar.NewId()
	other := log.file(t, requester, authority.memberId, "198.51.100.7", false)
	own := log.file(t, identity.SidecarId, authority.memberId, "198.51.100.8", false)
	approvals := local.GetApprovals()

	pending := approvals.GetPending()
	otherItem := testApprovalItem(pending, newId([16]byte(other.ApprovalId())))
	ownItem := testApprovalItem(pending, newId([16]byte(own.ApprovalId())))
	for _, item := range []*SidecarApprovalItem{otherItem, ownItem} {
		if item == nil || item.Kind != SidecarApprovalKindRequest || item.Action != "dns_set" || item.Template ||
			!strings.Contains(item.Text, "198.51.100.") || !strings.Contains(item.Text, "the agent's words") ||
			!item.Decisions.Contains(SidecarDecisionApprove) || !item.Decisions.Contains(SidecarDecisionDeny) || item.Refused != "" {
			t.Fatalf("a request's item: %+v", item)
		}
	}
	if otherItem.Channel != "device" || sidecar.Id(otherItem.SidecarId.id) != requester {
		t.Errorf("another sidecar's request: %+v", otherItem)
	}
	if ownItem.Channel != "presence" || sidecar.Id(ownItem.SidecarId.id) != identity.SidecarId {
		t.Errorf("its own request: %+v", ownItem)
	}

	if result := testRequestDecide(t, approvals, other.ApprovalId(), SidecarDecisionApprove); !result.Ok {
		t.Fatalf("another's request: %+v", result)
	}
	if len(signer.asked()) != 0 {
		t.Fatal("a device decision asked the presence key")
	}
	if result := testRequestDecide(t, approvals, own.ApprovalId(), SidecarDecisionApprove); !result.Ok {
		t.Fatalf("its own request: %+v", result)
	}
	decisions := log.decisions(t)
	if len(decisions) != 2 {
		t.Fatalf("%d decisions filed", len(decisions))
	}
	device, presence := decisions[0], decisions[1]
	if device.Decision.GetChannel() != uint32(protocol.ApprovalChannel_APPROVAL_CHANNEL_DEVICE) || !device.Binds(other) ||
		!bytes.Equal(device.Signed.GetSignerId(), identity.SidecarId.Bytes()) {
		t.Errorf("the device decision: %+v", device.Decision)
	}
	if err := device.Verify(bridge.SignedKeyAlgEd25519, identity.IdentityPublicKey); err != nil {
		t.Errorf("the device decision does not verify under the identity key: %v", err)
	}
	if presence.Decision.GetChannel() != uint32(protocol.ApprovalChannel_APPROVAL_CHANNEL_PRESENCE) || !presence.Binds(own) ||
		!bytes.Equal(presence.Signed.GetKeyId(), signer.keyId) {
		t.Errorf("the presence decision: %+v", presence.Decision)
	}
	if err := sidecar.VerifyPresenceUse(presence.Signed, sidecar.ApprovalPurpose, bridge.SignedType_SIGNED_APPROVAL_DECISION, signer.keyRow(t), nil); err != nil {
		t.Errorf("the presence decision does not verify: %v", err)
	}
	asked := signer.asked()
	if len(asked) != 1 || !bytes.Equal(asked[0], bridge.SignedBytes(sidecar.ApprovalPurpose, bridge.SignedType_SIGNED_APPROVAL_DECISION, presence.Signed.GetBody())) {
		t.Errorf("the presence key was asked over %d messages, not the decision's bytes", len(asked))
	}
	// single use: a decided request takes no second decision
	if result := testRequestDecide(t, approvals, own.ApprovalId(), SidecarDecisionDeny); result.Ok || result.Code != SidecarCodeApprovalDecided {
		t.Errorf("a second decision: %+v", result)
	}
	if len(log.decisions(t)) != 2 {
		t.Error("a second decision was filed")
	}
}

// FS-124's app-host decision case on both of §16.6's profiles: the phone's own request signed by its presence key
// through SignWithPresence, as a Secure Enclave key (ecdsa-p256-sha256) and as a passkey (webauthn-es256), each
// verifying under its per-use profile against the member's live key.
func TestRequestOwnByPresenceOnBothProfiles(t *testing.T) {
	ecdsaSigner := newTestPresenceSigner(t)
	webAuthnSigner := newTestWebAuthnPresenceSigner(t)
	for _, profile := range []struct {
		name   string
		signer SidecarPresenceSigner
		row    *bridge.KeyRow
	}{
		{name: "ecdsa-p256-sha256", signer: ecdsaSigner, row: ecdsaSigner.keyRow(t)},
		{name: "webauthn-es256", signer: webAuthnSigner, row: webAuthnSigner.keyRow(t)},
	} {
		func() {
			local, authority, log := newTestRequestPhone(t, profile.signer)
			authority.addMemberKey(profile.row)
			own := log.file(t, local.Core().Identity().SidecarId, authority.memberId, "198.51.100.7", false)
			if result := testRequestDecide(t, local.GetApprovals(), own.ApprovalId(), SidecarDecisionApprove); !result.Ok {
				t.Fatalf("%s: its own request: %+v", profile.name, result)
			}
			decisions := log.decisions(t)
			if len(decisions) != 1 || decisions[0].Decision.GetChannel() != uint32(protocol.ApprovalChannel_APPROVAL_CHANNEL_PRESENCE) {
				t.Fatalf("%s: %d decisions", profile.name, len(decisions))
			}
			if err := sidecar.VerifyPresenceUse(decisions[0].Signed, sidecar.ApprovalPurpose, bridge.SignedType_SIGNED_APPROVAL_DECISION, profile.row, nil); err != nil {
				t.Errorf("%s: the decision does not verify: %v", profile.name, err)
			}
			if profile.row.GetAlg() == bridge.KeyAlg_KEY_ALG_WEBAUTHN_ES256 && decisions[0].Signed.GetWebauthn() == nil {
				t.Errorf("%s: no assertion rides the decision", profile.name)
			}
		}()
	}
	if len(webAuthnSigner.asked()) != 1 {
		t.Errorf("the passkey was asked %d times", len(webAuthnSigner.asked()))
	}
}

// The phone's own request signs nothing with no presence signer, on a refusal, an answer that is no evidence, a panic
// in the authenticator, or a key the member does not hold, each answering its code; the item stays decidable, and the
// person's next try with the member's key signs it.
func TestRequestOwnRefusalsSignNothing(t *testing.T) {
	// no signer: another device approves
	local, authority, log := newTestRequestPhone(t, nil)
	own := log.file(t, local.Core().Identity().SidecarId, authority.memberId, "198.51.100.7", false)
	if result := testRequestDecide(t, local.GetApprovals(), own.ApprovalId(), SidecarDecisionApprove); result.Ok || result.Code != SidecarCodeApprovalNotEligible {
		t.Errorf("no signer: %+v", result)
	}
	if len(log.decisions(t)) != 0 {
		t.Fatal("a decision was filed with no signer")
	}

	signer := newTestPresenceSigner(t)
	local, authority, log = newTestRequestPhone(t, signer)
	own = log.file(t, local.Core().Identity().SidecarId, authority.memberId, "198.51.100.7", false)
	approvals := local.GetApprovals()
	// the member holds no live key yet: the app's key is no key of the member's
	if result := testRequestDecide(t, approvals, own.ApprovalId(), SidecarDecisionApprove); result.Ok || result.Code != SidecarCodeRevokeCancelled {
		t.Errorf("a key the member does not hold: %+v", result)
	}
	authority.addMemberKey(signer.keyRow(t))
	for _, mode := range []testPresenceMode{testPresenceRefuse, testPresenceGarbage, testPresencePanic} {
		signer.setMode(mode)
		if result := testRequestDecide(t, approvals, own.ApprovalId(), SidecarDecisionApprove); result.Ok || result.Code != SidecarCodeRevokeCancelled {
			t.Errorf("mode %d: %+v", mode, result)
		}
	}
	if len(log.decisions(t)) != 0 {
		t.Fatalf("%d decisions filed on refusals", len(log.decisions(t)))
	}
	// twin: the person's next try
	signer.setMode(testPresenceSign)
	if result := testRequestDecide(t, approvals, own.ApprovalId(), SidecarDecisionApprove); !result.Ok {
		t.Fatalf("the next try: %+v", result)
	}
	if decisions := log.decisions(t); len(decisions) != 1 || decisions[0].Decision.GetChannel() != uint32(protocol.ApprovalChannel_APPROVAL_CHANNEL_PRESENCE) {
		t.Errorf("%d decisions after the next try", len(decisions))
	}
}

// An app's Decide carries no uses, so a template request lists deny alone: its approval is refused before anything is
// signed, and its denial is signed.
func TestRequestTemplateTakesDenyOnAnApp(t *testing.T) {
	local, authority, log := newTestRequestPhone(t, nil)
	template := log.file(t, sidecar.NewId(), authority.memberId, "198.51.100.7", true)
	approvals := local.GetApprovals()
	item := testApprovalItem(approvals.GetPending(), newId([16]byte(template.ApprovalId())))
	if item == nil || !item.Template || item.Decisions.Len() != 1 || !item.Decisions.Contains(SidecarDecisionDeny) {
		t.Fatalf("the template's item: %+v", item)
	}
	if result := testRequestDecide(t, approvals, template.ApprovalId(), SidecarDecisionApprove); result.Ok || result.Code != SidecarCodeApprovalNotEligible {
		t.Errorf("an approval of a template: %+v", result)
	}
	if result := testRequestDecide(t, approvals, template.ApprovalId(), SidecarDecisionDeny); !result.Ok {
		t.Fatalf("a denial of a template: %+v", result)
	}
	if decisions := log.decisions(t); len(decisions) != 1 || decisions[0].Decision.GetDecision() != uint32(protocol.ApprovalVerdict_APPROVAL_VERDICT_DENY) {
		t.Errorf("the decisions filed: %+v", decisions)
	}
}

// A phone that is no approval-capable device of its member, unconsented or read_only, lists no request and signs none;
// the twin, consented and capable, lists it.
func TestRequestNotListedOnANonCapablePhone(t *testing.T) {
	local, authority, log := newTestRequestPhone(t, nil)
	request := log.file(t, sidecar.NewId(), authority.memberId, "198.51.100.7", false)
	approvals := local.GetApprovals()
	id := newId([16]byte(request.ApprovalId()))
	listed := func() bool {
		return testApprovalItem(approvals.GetPending(), id) != nil
	}
	if !listed() {
		t.Fatal("the capable phone does not list the request")
	}
	authority.stateLock.Lock()
	authority.consented = false
	authority.stateLock.Unlock()
	if listed() {
		t.Error("an unconsented phone lists the request")
	}
	if result := testRequestDecide(t, approvals, request.ApprovalId(), SidecarDecisionApprove); result.Ok {
		t.Errorf("an unconsented phone decided: %+v", result)
	}
	authority.stateLock.Lock()
	authority.consented = true
	authority.admissions[local.Core().Identity().SidecarId].Row.Capabilities = uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_READ_ONLY)
	authority.stateLock.Unlock()
	if listed() {
		t.Error("a read_only phone lists the request")
	}
	if len(log.decisions(t)) != 0 {
		t.Fatal("a decision was filed")
	}
}

// A fake leaser of the phone's approval logs: what each commit hints.
type testRequestLeaser struct {
	stateLock sync.Mutex
	callbacks map[sidecar.Id][]func(logId sidecar.Id)
}

func (self *testRequestLeaser) Lease(logId sidecar.Id, callback func(logId sidecar.Id)) (func(), error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.callbacks == nil {
		self.callbacks = map[sidecar.Id][]func(logId sidecar.Id){}
	}
	self.callbacks[logId] = append(self.callbacks[logId], callback)
	return func() {}, nil
}

func (self *testRequestLeaser) commit(logId sidecar.Id) {
	callbacks := func() []func(logId sidecar.Id) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return slices.Clone(self.callbacks[logId])
	}()
	for _, callback := range callbacks {
		callback(logId)
	}
}

// A request's hint reaches the app: the phone's approval part, cued by the hint, resyncs and raises approval_pending
// with the request's id, which the app's event listener hears, and the pending listener, which reads on events alone
// here, hears the list with the request.
func TestRequestHintReachesThePendingListener(t *testing.T) {
	local, authority, log := newTestRequestPhone(t, nil)
	local.stateLock.Lock()
	settings := defaultSidecarApprovalsSettings()
	settings.PendingRefreshInterval = 0
	local.approvalsSettings = settings
	local.stateLock.Unlock()
	approvals := local.GetApprovals()
	listener := &testApprovalsListener{lists: make(chan *SidecarApprovalList, 16)}
	sub := approvals.AddPendingListener(listener)
	defer sub.Close()
	listener.next(t, 0)
	events := &testRemoteEvents{events: make(chan *SidecarEvent, 64)}
	local.AddEventListener(events)

	leaser := &testRequestLeaser{}
	hinted := make(chan struct{}, 4)
	phone, err := local.Core().BindApprovalPhone(&sidecar.ApprovalPhoneArgs{
		Leaser: leaser,
		Logs:   func() ([]sidecar.Id, error) { return []sidecar.Id{log.logId}, nil },
		Hinted: func() { hinted <- struct{}{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer phone.Close()
	if _, err := phone.Resync(t.Context()); err != nil {
		t.Fatal(err)
	}
	request := log.file(t, sidecar.NewId(), authority.memberId, "198.51.100.7", false)
	leaser.commit(log.logId)
	select {
	case <-hinted:
	case <-time.After(testSidecarTimeout):
		t.Fatal("the hint cued nothing")
	}
	resynced, err := phone.Resync(t.Context())
	if err != nil || !slices.Equal(resynced.Raised, []sidecar.Id{request.ApprovalId()}) {
		t.Fatalf("the cued resync: %+v, %v", resynced, err)
	}
	for {
		event := events.next(t)
		if event.Kind == string(sidecar.SidecarEventApprovalPending) {
			if event.ApprovalId == nil || sidecar.Id(event.ApprovalId.id) != request.ApprovalId() {
				t.Fatalf("approval_pending names %+v", event.ApprovalId)
			}
			break
		}
	}
	list := listener.next(t, 1)
	if item := testApprovalItem(list, newId([16]byte(request.ApprovalId()))); item == nil || item.Channel != "device" {
		t.Fatalf("the listener's list: %+v", item)
	}
}

// A desktop in remote mode lists the daemon's requests by approvals.app.requests on the pairing's network, unconfirmed:
// another sidecar's on channel device with approve and deny, the daemon's own with no decision; Decide of the daemon's
// own is refused before anything is sent, and of another's goes by approvals.app.request, confirmed, on that network.
// A daemon from before the app methods lists the authority items alone.
func TestRequestRemoteDesktopOverTheAppMethods(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	authorityId, otherId, ownId, requesterId := sidecar.NewId(), sidecar.NewId(), sidecar.NewId(), sidecar.NewId()
	appMethods := true
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		switch request.Method {
		case sidecarControlMethodApprovalsList:
			return map[string]any{"items": []map[string]any{{"approval_id": authorityId, "kind": "admission", "decisions": []string{"approve", "deny"}}}}, nil, true
		case sidecarControlMethodApprovalsAppRequests, sidecarControlMethodApprovalsAppRequest:
			if !appMethods {
				return nil, nil, false
			}
		}
		switch request.Method {
		case sidecarControlMethodApprovalsAppRequests:
			return map[string]any{"items": []map[string]any{
				{"approval_id": otherId, "entry_id": sidecar.NewId(), "sidecar_id": requesterId, "kind": "dns_set", "text": "request: dns_set",
					"channel": "device", "decisions": []string{"approve", "deny"}},
				{"approval_id": ownId, "entry_id": sidecar.NewId(), "sidecar_id": daemon.sidecarId, "kind": "mail_send", "text": "request: mail_send",
					"decisions": []string{}},
			}}, nil, true
		case sidecarControlMethodApprovalsAppRequest:
			if !request.UrConfirmed {
				return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcConfirmationRequired, Message: "confirmation required"}, true
			}
			return map[string]any{}, nil, true
		}
		return nil, nil, false
	}
	daemon.start()
	remote, backend, _ := openTestRemoteSidecar(t, socketPath)
	// the pairing names the daemon's network
	if _, err := backend.pair(t.Context(), "a-synthetic-child-token"); err != nil {
		t.Fatal(err)
	}
	network := backend.network()
	if network != daemon.networkId.String() {
		t.Fatalf("the pairing names network %q", network)
	}
	approvals := remote.GetApprovals()
	pending := approvals.GetPending()
	other := testApprovalItem(pending, newId([16]byte(otherId)))
	own := testApprovalItem(pending, newId([16]byte(ownId)))
	if other == nil || other.Kind != SidecarApprovalKindRequest || other.Channel != "device" || sidecar.Id(other.SidecarId.id) != requesterId ||
		!other.Decisions.Contains(SidecarDecisionApprove) {
		t.Fatalf("another sidecar's request: %+v", other)
	}
	if own == nil || own.Channel != "" || own.Decisions.Len() != 0 || sidecar.Id(own.SidecarId.id) != daemon.sidecarId {
		t.Fatalf("the daemon's own request: %+v", own)
	}
	if result := testRequestDecide(t, approvals, ownId, SidecarDecisionApprove); result.Ok || result.Code != SidecarCodeApprovalNotEligible {
		t.Errorf("the daemon's own request: %+v", result)
	}
	if slices.Contains(daemon.seenMethods(), sidecarControlMethodApprovalsAppRequest) {
		t.Fatal("a decision on the daemon's own request was sent")
	}
	if result := testRequestDecide(t, approvals, otherId, SidecarDecisionApprove); !result.Ok {
		t.Fatalf("another sidecar's request: %+v", result)
	}
	sent := 0
	for _, request := range daemon.seen() {
		switch request.Method {
		case sidecarControlMethodApprovalsAppRequests:
			params := &sidecarControlRequestsParams{}
			if request.UrConfirmed || json.Unmarshal(request.Params, params) != nil || params.Network != network {
				t.Errorf("approvals.app.requests: confirmed %t, %s", request.UrConfirmed, request.Params)
			}
		case sidecarControlMethodApprovalsAppRequest:
			sent += 1
			params := &sidecarControlRequestDecideParams{}
			if !request.UrConfirmed || json.Unmarshal(request.Params, params) != nil || params.Network != network ||
				sidecar.Id(params.ApprovalId.id) != otherId || params.Decision != SidecarDecisionApprove {
				t.Errorf("approvals.app.request: confirmed %t, %s", request.UrConfirmed, request.Params)
			}
		}
	}
	if sent != 1 {
		t.Errorf("%d decisions sent", sent)
	}

	// a daemon from before the app methods: the authority items alone
	daemon.stateLock.Lock()
	appMethods = false
	daemon.stateLock.Unlock()
	pending = approvals.GetPending()
	if pending == nil || pending.Len() != 1 || testApprovalItem(pending, newId([16]byte(authorityId))) == nil {
		t.Fatalf("an older daemon's list: %+v", pending)
	}
}

// A remote Sidecar on the daemon at socketPath whose app holds signer, its status reads the test's own.
func openTestRemoteSidecarSigning(t *testing.T, socketPath string, signer SidecarPresenceSigner) (*Sidecar, *sidecarRemoteBackend) {
	t.Helper()
	remoteSettings := defaultSidecarRemoteSettings()
	remoteSettings.CallTimeout = testSidecarTimeout
	remoteSettings.PairTimeout = testSidecarTimeout
	remote, err := newSidecarRemote(context.Background(), signer, DefaultSidecarSettings(), &sidecarRemoteOptions{
		dial:           sidecarDaemonSocketDial(socketPath, testRemoteDaemonVersion),
		remoteSettings: remoteSettings,
		noStatusPoll:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.Close)
	backend := remote.remote.(*sidecarRemoteBackend)
	if _, err := backend.pair(t.Context(), "a-synthetic-child-token"); err != nil {
		t.Fatal(err)
	}
	return remote, backend
}

// FS-124's app-host decision case in remote mode: Decide of an authority item goes by §5.8's two calls on the
// pairing's network, both confirmed, the app's presence key signing exactly the bytes the first answers before the
// second carries its evidence; the daemon's refusal of either call answers its code with nothing more sent; a refused
// check ends the act after the first call; a first call with no bytes takes the second with no evidence; a decision
// whose post's answer was lost is a decision made; and a daemon from before the two calls takes the one confirmed
// approvals.decide.
func TestRemoteAuthorityDecideByTwoCalls(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	signer := newTestPresenceSigner(t)
	countersigned := []byte("the act's countersigned bytes")
	ids := map[string]sidecar.Id{}
	for _, name := range []string{"signed", "refused check", "begin refused", "finish refused", "no bytes", "pending", "older daemon"} {
		ids[name] = sidecar.NewId()
	}
	nameOf := func(raw json.RawMessage) string {
		params := struct {
			ApprovalId sidecar.Id `json:"approval_id"`
		}{}
		json.Unmarshal(raw, &params)
		for name, id := range ids {
			if id == params.ApprovalId {
				return name
			}
		}
		return ""
	}
	actNames := map[string]string{}
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		switch request.Method {
		case sidecarControlMethodApprovalsList:
			items := []map[string]any{}
			for _, id := range ids {
				items = append(items, map[string]any{"approval_id": id, "kind": "admission", "decisions": []string{"approve", "deny"}})
			}
			return map[string]any{"items": items}, nil, true
		case sidecarControlMethodApprovalsAppRequests:
			return map[string]any{"items": []map[string]any{}}, nil, true
		case sidecarControlMethodApprovalsDecideBegin:
			if !request.UrConfirmed {
				return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcConfirmationRequired, Message: "confirmation required"}, true
			}
			name := nameOf(request.Params)
			switch name {
			case "older daemon":
				return nil, nil, false
			case "begin refused":
				return map[string]any{"refusal": map[string]any{"code": SidecarCodeRevokeNotCapable}}, nil, true
			}
			actId := sidecar.NewId()
			actNames[actId.String()] = name
			if name == "no bytes" {
				return map[string]any{"act_id": actId}, nil, true
			}
			return map[string]any{"act_id": actId, "countersigned": countersigned}, nil, true
		case sidecarControlMethodApprovalsDecideFinish:
			if !request.UrConfirmed {
				return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcConfirmationRequired, Message: "confirmation required"}, true
			}
			params := &sidecarControlPresenceFinishParams{}
			if json.Unmarshal(request.Params, params) != nil || params.ActId == nil {
				return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "no act"}, true
			}
			switch actNames[params.ActId.String()] {
			case "no bytes":
				if len(params.Evidence) != 0 {
					return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "evidence for no bytes"}, true
				}
				return map[string]any{}, nil, true
			case "finish refused":
				return map[string]any{"refusal": map[string]any{"code": SidecarCodeRevokeRefused}}, nil, true
			case "pending":
				return map[string]any{"pending": true}, nil, true
			}
			if !signer.verifies(params.Evidence, countersigned) {
				return map[string]any{"refusal": map[string]any{"code": SidecarCodeRevokeCancelled}}, nil, true
			}
			return map[string]any{}, nil, true
		case sidecarControlMethodApprovalsDecide:
			if !request.UrConfirmed || nameOf(request.Params) != "older daemon" {
				return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "the one call came for another item"}, true
			}
			return map[string]any{}, nil, true
		}
		return nil, nil, false
	}
	daemon.start()
	remote, backend := openTestRemoteSidecarSigning(t, socketPath, signer)
	network := backend.network()
	approvals := remote.GetApprovals()
	decide := func(name string) *SidecarResult {
		return testRequestDecide(t, approvals, ids[name], SidecarDecisionApprove)
	}

	if result := decide("signed"); !result.Ok {
		t.Fatalf("the signed decision: %+v", result)
	}
	if asked := signer.asked(); len(asked) != 1 || !bytes.Equal(asked[0], countersigned) {
		t.Fatalf("the presence key was asked over %d messages", len(asked))
	}
	signer.setMode(testPresenceRefuse)
	if result := decide("refused check"); result.Ok || result.Code != SidecarCodeRevokeCancelled {
		t.Errorf("a refused check: %+v", result)
	}
	signer.setMode(testPresenceSign)
	if result := decide("begin refused"); result.Ok || result.Code != SidecarCodeRevokeNotCapable {
		t.Errorf("a refused first call: %+v", result)
	}
	if result := decide("finish refused"); result.Ok || result.Code != SidecarCodeRevokeRefused {
		t.Errorf("a refused second call: %+v", result)
	}
	if result := decide("no bytes"); !result.Ok {
		t.Errorf("an act with no bytes: %+v", result)
	}
	if result := decide("pending"); !result.Ok || result.Message == "" {
		t.Errorf("a post's answer lost: %+v", result)
	}
	if result := decide("older daemon"); !result.Ok {
		t.Errorf("an older daemon: %+v", result)
	}

	finishes := map[string]int{}
	for _, request := range daemon.seen() {
		switch request.Method {
		case sidecarControlMethodApprovalsDecideBegin:
			params := &sidecarControlApprovalsDecideBeginParams{}
			if !request.UrConfirmed || json.Unmarshal(request.Params, params) != nil || params.Network != network || params.Decision != SidecarDecisionApprove {
				t.Errorf("approvals.decide.begin: confirmed %t, %s", request.UrConfirmed, request.Params)
			}
		case sidecarControlMethodApprovalsDecideFinish:
			params := &sidecarControlPresenceFinishParams{}
			json.Unmarshal(request.Params, params)
			finishes[actNames[params.ActId.String()]] += 1
			if params.Network != network {
				t.Errorf("approvals.decide.finish's network %q", params.Network)
			}
		}
	}
	for _, name := range []string{"signed", "finish refused", "no bytes", "pending"} {
		if finishes[name] != 1 {
			t.Errorf("%s: %d second calls", name, finishes[name])
		}
	}
	for _, name := range []string{"refused check", "begin refused", "older daemon"} {
		if finishes[name] != 0 {
			t.Errorf("%s: %d second calls", name, finishes[name])
		}
	}
}
