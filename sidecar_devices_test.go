//go:build !ios_extension && !js && !ios && !android

package sdk

// SidecarApprovals' device calls (SDK-M8-05; D-SDK-30, FS-98, FS-124's app-host rows on fakes): GetDevices,
// PreviewRevokeDevice and RevokeDevice over a real enrolled local core with a fake authority and a recording delta route
// (sidecar_approvals_test.go), and over a fake daemon serving §5.8's two calls. The presence key is a P-256 key the test
// holds, registered live for this device, so the core's act verifies the app's evidence before it signs.

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// A remote Sidecar on the daemon at socketPath with signer, its status reads the test's own.
func openTestRemoteApprovals(t *testing.T, socketPath string, signer SidecarPresenceSigner) *SidecarApprovals {
	t.Helper()
	remoteSettings := defaultSidecarRemoteSettings()
	remoteSettings.CallTimeout = testSidecarTimeout
	remote, err := newSidecarRemote(context.Background(), signer, DefaultSidecarSettings(), &sidecarRemoteOptions{
		dial:           sidecarDaemonSocketDial(socketPath, testRemoteDaemonVersion),
		remoteSettings: remoteSettings,
		noStatusPoll:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.Close)
	approvals := remote.GetApprovals()
	if approvals == nil {
		t.Fatal("no approvals")
	}
	return approvals
}

// GetDevices lists this member's devices with this one first, then by admission: a page Worker, a parked device and a
// suspended one with their marks and times, and no tagged sidecar or other member's device; over the daemon, its rows
// in its order. Locked or unenrolled, it answers nil, so an unknown never reads as no devices.
func TestGetDevicesThisDeviceFirst(t *testing.T) {
	local, authority, _ := newTestApprovalsSidecar(t, nil)
	ownId := authority.ownId
	worker, parked, suspended, tagged, otherMember := sidecar.NewId(), sidecar.NewId(), sidecar.NewId(), sidecar.NewId(), sidecar.NewId()
	authority.admit(worker, authority.memberId, 3).Row.Capabilities = uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_READ_ONLY)
	authority.admit(parked, authority.memberId, 7).Row.ParkedAt = 9
	authority.admit(suspended, authority.memberId, 8)
	authority.admit(tagged, authority.memberId, 4).TagIds = []sidecar.Id{sidecar.NewId()}
	authority.admit(otherMember, sidecar.NewId(), 2)
	authority.stateLock.Lock()
	authority.suspended[suspended] = true
	authority.entryTimes[3] = time.UnixMilli(1700000003000)
	authority.entryTimes[5] = time.UnixMilli(1700000005000)
	authority.stateLock.Unlock()

	devices := local.GetApprovals().GetDevices()
	if devices == nil {
		t.Fatal("the devices did not read")
	}
	want := []struct {
		id       sidecar.Id
		this     bool
		readOnly bool
		state    string
		admitted int64
	}{
		{ownId, true, false, SidecarDeviceLive, 1700000005000},
		{worker, false, true, SidecarDeviceLive, 1700000003000},
		{parked, false, false, SidecarDeviceParked, 0},
		{suspended, false, false, SidecarDevicePending, 0},
	}
	if devices.Len() != len(want) {
		t.Fatalf("%d devices", devices.Len())
	}
	for i, expected := range want {
		device := devices.Get(i)
		if sidecar.Id(device.SidecarId.id) != expected.id || device.This != expected.this || device.ReadOnly != expected.readOnly ||
			device.State != expected.state || device.AdmittedMillis != expected.admitted {
			t.Errorf("device %d: %+v", i, device)
		}
	}

	// an unenrolled Sidecar has no devices to list
	unenrolled := newTestSidecarWorld(t).open(t)
	if unenrolled.GetApprovals().GetDevices() != nil {
		t.Error("an unenrolled Sidecar listed devices")
	}

	// the daemon's rows, in its order
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	thisId, laptopId := sidecar.NewId(), sidecar.NewId()
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		if request.Method != sidecarControlMethodDevicesList {
			return nil, nil, false
		}
		return map[string]any{"devices": []map[string]any{
			{"sidecar_id": thisId, "name": "phone", "this": true, "state": "live", "admitted_at_ms": 1700000000000},
			{"sidecar_id": laptopId, "name": "laptop", "state": "stale", "last_change_ms": 1700000009000},
		}}, nil, true
	}
	daemon.start()
	remoteDevices := openTestRemoteApprovals(t, socketPath, nil).GetDevices()
	if remoteDevices == nil || remoteDevices.Len() != 2 {
		t.Fatalf("the daemon's devices: %v", remoteDevices)
	}
	first, second := remoteDevices.Get(0), remoteDevices.Get(1)
	if sidecar.Id(first.SidecarId.id) != thisId || !first.This || first.Name != "phone" || first.AdmittedMillis != 1700000000000 ||
		sidecar.Id(second.SidecarId.id) != laptopId || second.This || second.State != SidecarDeviceStale || second.LastChangeMillis != 1700000009000 {
		t.Errorf("the daemon's devices: %+v %+v", first, second)
	}
}

// Under Presence, an empty signature, an answer that is no evidence, a panicking authenticator and no signer at all
// each answer REVOKE_CANCELLED with nothing signed or posted, and the plan stays good for the person's next try.
func TestEmptySignatureCancels(t *testing.T) {
	signer := newTestPresenceSigner(t)
	local, authority, api := newTestApprovalsSidecar(t, signer)
	lostId := sidecar.NewId()
	authority.admit(lostId, authority.memberId, 6)
	authority.stateLock.Lock()
	authority.liveKeys = []*bridge.KeyRow{signer.keyRow(t)}
	authority.covered[bridge.SignedType_SIGNED_SIDECAR_REVOKE] = true
	authority.stateLock.Unlock()

	approvals := local.GetApprovals()
	plan := awaitTestRevokePlan(t, approvals, newId([16]byte(lostId)), SidecarRevokeLost, 0, 0)
	if plan.Code != "" || plan.PlanId == nil || !plan.Presence || plan.Signatures != 1 || plan.Reason != SidecarRevokeLost {
		t.Fatalf("the plan: %+v", plan)
	}
	for _, mode := range []testPresenceMode{testPresenceRefuse, testPresenceGarbage, testPresencePanic} {
		signer.setMode(mode)
		result := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
			approvals.RevokeDevice(plan.PlanId, nil, callback)
		})
		if result.Ok || result.Code != SidecarCodeRevokeCancelled || result.Value != "" {
			t.Errorf("mode %d: %+v", mode, result)
		}
	}
	if count := api.postCount(); count != 0 {
		t.Fatalf("%d statements posted with no presence", count)
	}

	// the person tries again and passes: the same plan signs
	signer.setMode(testPresenceSign)
	result := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
		approvals.RevokeDevice(plan.PlanId, nil, callback)
	})
	if !result.Ok || result.Value == "" {
		t.Fatalf("the retried revocation: %+v", result)
	}

	// an app with no presence key signs nothing a covered act needs
	unsigned, unsignedAuthority, unsignedApi := newTestApprovalsSidecar(t, nil)
	otherId := sidecar.NewId()
	unsignedAuthority.admit(otherId, unsignedAuthority.memberId, 6)
	unsignedAuthority.stateLock.Lock()
	unsignedAuthority.liveKeys = []*bridge.KeyRow{signer.keyRow(t)}
	unsignedAuthority.covered[bridge.SignedType_SIGNED_SIDECAR_REVOKE] = true
	unsignedAuthority.stateLock.Unlock()
	unsignedApprovals := unsigned.GetApprovals()
	unsignedPlan := awaitTestRevokePlan(t, unsignedApprovals, newId([16]byte(otherId)), SidecarRevokeLost, 0, 0)
	if !unsignedPlan.Presence {
		t.Fatalf("the plan: %+v", unsignedPlan)
	}
	noSigner := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
		unsignedApprovals.RevokeDevice(unsignedPlan.PlanId, nil, callback)
	})
	if noSigner.Ok || noSigner.Code != SidecarCodeRevokeCancelled {
		t.Errorf("no signer: %+v", noSigner)
	}
	if count := unsignedApi.postCount(); count != 0 {
		t.Errorf("%d statements posted with no signer", count)
	}
}

// The presence check runs over the act's countersigned bytes before any statement is signed or posted, once for the
// whole act, and its evidence rides each statement: the SidecarRevoke and the KeyRevoke of the key the person left
// selected both carry the countersignature, and the result names the revocation's entry.
func TestPresenceBeforeAnySignature(t *testing.T) {
	signer := newTestPresenceSigner(t)
	local, authority, api := newTestApprovalsSidecar(t, signer)
	signer.postCount = api.postCount
	lostId := sidecar.NewId()
	authority.admit(lostId, authority.memberId, 6)
	lostKeyId := bytes.Repeat([]byte{4}, 32)
	authority.stateLock.Lock()
	authority.liveKeys = []*bridge.KeyRow{signer.keyRow(t)}
	authority.covered[bridge.SignedType_SIGNED_SIDECAR_REVOKE] = true
	authority.covered[bridge.SignedType_SIGNED_KEY_REVOKE] = true
	authority.registered[lostId] = []*bridge.KeyRow{{KeyId: lostKeyId, Purpose: bridge.KeyPurpose_KEY_PURPOSE_PRESENCE, At: 6}}
	authority.stateLock.Unlock()

	approvals := local.GetApprovals()
	plan := awaitTestRevokePlan(t, approvals, newId([16]byte(lostId)), SidecarRevokeLost, 0, 0)
	if plan.Code != "" || !plan.Presence || plan.Items == nil {
		t.Fatalf("the plan: %+v", plan)
	}
	keys := NewSidecarRevokeItemList()
	for _, item := range plan.Items.getAll() {
		if item.Effect == "key" {
			keys.Add(item)
		}
	}
	if keys.Len() != 1 || !bytes.Equal(keys.Get(0).Id, lostKeyId) {
		t.Fatalf("the plan's key rows: %d", keys.Len())
	}
	result := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
		approvals.RevokeDevice(plan.PlanId, keys, callback)
	})
	if !result.Ok {
		t.Fatalf("the revocation: %+v", result)
	}
	if position, err := strconv.ParseUint(result.Value, 10, 64); err != nil || position <= 100 {
		t.Errorf("the result names no entry: %q", result.Value)
	}

	asked := signer.asked()
	signer.stateLock.Lock()
	postsBefore := append([]int{}, signer.postsBefore...)
	signer.stateLock.Unlock()
	if len(asked) != 1 || len(postsBefore) != 1 || postsBefore[0] != 0 {
		t.Fatalf("the key was asked %d times, with %v statements posted before", len(asked), postsBefore)
	}
	statements := api.postedStatements(t)
	if len(statements) != 2 {
		t.Fatalf("%d statements posted", len(statements))
	}
	types := []bridge.SignedType{}
	for _, statement := range statements {
		types = append(types, bridge.SignedType(statement.GetType()))
		evidence := statement.GetWebauthn()
		if !bytes.Equal(evidence.GetKeyId(), signer.keyId) || len(evidence.GetSignature()) == 0 || evidence.GetActSize() != 2 {
			t.Errorf("a statement of type %d carries no countersignature of the act", statement.GetType())
		}
		// the one evidence over the act's bytes, which the key signed once
		if !signer.verifies(mustMarshalTestEvidence(t, evidence), asked[0]) {
			t.Error("the countersignature is not the key's over the bytes it was shown")
		}
	}
	if types[0] != bridge.SignedType_SIGNED_SIDECAR_REVOKE || types[1] != bridge.SignedType_SIGNED_KEY_REVOKE {
		t.Errorf("the act's statements: %v", types)
	}
}

// The evidence as the key answered it: its key_id and signature alone, the act fields the core adds per statement
// cleared.
func mustMarshalTestEvidence(t *testing.T, evidence *bridge.WebAuthnEvidence) []byte {
	t.Helper()
	encoded, err := proto.Marshal(&bridge.WebAuthnEvidence{KeyId: evidence.GetKeyId(), Signature: evidence.GetSignature()})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// The daemon §5.8's two calls run on: devices.preview's plan, devices.revoke.begin's act and bytes, and
// devices.revoke.finish, which signs only with evidence verifying over those bytes; and what it took.
type testTwoCallDaemon struct {
	daemon        *testRemoteDaemon
	countersigned []byte
	actId         sidecar.Id
	// whether begin is served at all, and what it answers in place of the act
	noBegin      bool
	beginRefusal string
	// set once finish signed
	signed bool
}

func newTestTwoCallDaemon(t *testing.T, signer *testPresenceSigner, presence bool) *testTwoCallDaemon {
	t.Helper()
	self := &testTwoCallDaemon{
		daemon:        newTestRemoteDaemon(t, testRemoteSocketPath(t)),
		countersigned: []byte("the act's countersigned bytes, as the daemon built them"),
		actId:         sidecar.NewId(),
	}
	planId, targetId := sidecar.NewId(), sidecar.NewId()
	self.daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		switch request.Method {
		case sidecarControlMethodDevicesPreview:
			return map[string]any{"plan": map[string]any{
				"plan_id": planId, "sidecar_id": targetId, "name": "laptop", "reason": "lost", "items": []any{}, "signatures": 1, "presence": presence,
			}}, nil, true
		case sidecarControlMethodDevicesRevokeBegin:
			if self.noBegin {
				return nil, &sidecar.DaemonRpcError{Code: sidecarControlCodeMethodNotFound, Message: "method not found"}, true
			}
			if !request.UrConfirmed {
				return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcConfirmationRequired, Message: "confirmation required"}, true
			}
			if self.beginRefusal != "" {
				return map[string]any{"refusal": map[string]any{"code": self.beginRefusal}}, nil, true
			}
			reply := map[string]any{"act_id": self.actId}
			if presence {
				reply["countersigned"] = self.countersigned
			}
			return reply, nil, true
		case sidecarControlMethodDevicesRevokeFinish:
			params := &sidecarControlPresenceFinishParams{}
			if err := json.Unmarshal(request.Params, params); err != nil || params.ActId == nil || sidecar.Id(params.ActId.id) != self.actId {
				return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "no such act"}, true
			}
			if presence && (signer == nil || !signer.verifies(params.Evidence, self.countersigned)) {
				return map[string]any{"refusal": map[string]any{"code": SidecarCodeRevokeCancelled}}, nil, true
			}
			self.signed = true
			return map[string]any{"position": 42}, nil, true
		case sidecarControlMethodDevicesRevoke:
			if !request.UrConfirmed {
				return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcConfirmationRequired, Message: "confirmation required"}, true
			}
			self.signed = true
			return map[string]any{"position": 43}, nil, true
		}
		return nil, nil, false
	}
	self.daemon.start()
	return self
}

// Whether finish, or the one-call revoke, signed.
func (self *testTwoCallDaemon) hasSigned() bool {
	self.daemon.stateLock.Lock()
	defer self.daemon.stateLock.Unlock()
	return self.signed
}

// The methods the daemon took past the preview, in order, and whether each was confirmed.
func (self *testTwoCallDaemon) calls() ([]string, []bool) {
	methods, confirmed := []string{}, []bool{}
	for _, request := range self.daemon.seen() {
		if request.Method == sidecarControlMethodDevicesPreview {
			continue
		}
		methods = append(methods, request.Method)
		confirmed = append(confirmed, request.UrConfirmed)
	}
	return methods, confirmed
}

// In remote mode under Presence the first call answers the act's countersigned bytes and signs nothing, the app signs
// exactly those bytes, and the second call carries the evidence, after which alone the daemon signs: the result names
// the entry the second call answered. A refused signature ends it after the first call, nothing signed.
func TestRemoteTwoCallsSignOnSecond(t *testing.T) {
	signer := newTestPresenceSigner(t)
	daemon := newTestTwoCallDaemon(t, signer, true)
	approvals := openTestRemoteApprovals(t, daemon.daemon.socketPath, signer)
	plan := awaitTestRevokePlan(t, approvals, NewId(), SidecarRevokeLost, 0, 0)
	if plan.Code != "" || !plan.Presence {
		t.Fatalf("the plan: %+v", plan)
	}

	// refused at the key: one call, nothing signed
	signer.setMode(testPresenceRefuse)
	refused := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
		approvals.RevokeDevice(plan.PlanId, nil, callback)
	})
	if refused.Ok || refused.Code != SidecarCodeRevokeCancelled {
		t.Fatalf("a refused key: %+v", refused)
	}
	if methods, _ := daemon.calls(); len(methods) != 1 || methods[0] != sidecarControlMethodDevicesRevokeBegin || daemon.hasSigned() {
		t.Fatalf("after a refusal the daemon took %v, signed %v", methods, daemon.hasSigned())
	}

	// signed: two calls, both confirmed, the evidence over exactly the first call's bytes
	signer.setMode(testPresenceSign)
	result := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
		approvals.RevokeDevice(plan.PlanId, nil, callback)
	})
	if !result.Ok || result.Value != "42" || !daemon.hasSigned() {
		t.Fatalf("the revocation: %+v, signed %v", result, daemon.hasSigned())
	}
	methods, confirmed := daemon.calls()
	if len(methods) != 3 || methods[1] != sidecarControlMethodDevicesRevokeBegin || methods[2] != sidecarControlMethodDevicesRevokeFinish {
		t.Fatalf("the calls: %v", methods)
	}
	for i, each := range confirmed {
		if !each {
			t.Errorf("%s went unconfirmed", methods[i])
		}
	}
	asked := signer.asked()
	if len(asked) != 2 || !bytes.Equal(asked[1], daemon.countersigned) {
		t.Errorf("the key signed other bytes than the first call answered")
	}

	// no Presence: the first call answers no bytes, and the second signs on the person's confirmation alone
	unsigned := newTestTwoCallDaemon(t, nil, false)
	unsignedApprovals := openTestRemoteApprovals(t, unsigned.daemon.socketPath, nil)
	unsignedPlan := awaitTestRevokePlan(t, unsignedApprovals, NewId(), SidecarRevokeLost, 0, 0)
	confirmedOnly := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
		unsignedApprovals.RevokeDevice(unsignedPlan.PlanId, nil, callback)
	})
	if !confirmedOnly.Ok || confirmedOnly.Value != "42" {
		t.Errorf("an act no presence covers: %+v", confirmedOnly)
	}
	for _, request := range unsigned.daemon.seen() {
		if request.Method == sidecarControlMethodDevicesRevokeFinish {
			params := &sidecarControlPresenceFinishParams{}
			if err := json.Unmarshal(request.Params, params); err != nil || len(params.Evidence) != 0 {
				t.Errorf("the second call carried evidence nobody signed")
			}
		}
	}

	// a daemon from before the two calls takes the one confirmed devices.revoke
	old := newTestTwoCallDaemon(t, nil, false)
	old.daemon.stateLock.Lock()
	old.noBegin = true
	old.daemon.stateLock.Unlock()
	oldApprovals := openTestRemoteApprovals(t, old.daemon.socketPath, nil)
	oldPlan := awaitTestRevokePlan(t, oldApprovals, NewId(), SidecarRevokeLost, 0, 0)
	oneCall := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
		oldApprovals.RevokeDevice(oldPlan.PlanId, nil, callback)
	})
	if !oneCall.Ok || oneCall.Value != "43" {
		t.Errorf("the one-call fallback: %+v", oneCall)
	}
	if methods, confirmed := old.calls(); len(methods) != 2 || methods[1] != sidecarControlMethodDevicesRevoke || !confirmed[1] {
		t.Errorf("the old daemon took %v %v", methods, confirmed)
	}
}

// Each of §5.6's REVOKE_ codes surfaces in a plan's or a result's Code: locally the ones this device's own state
// decides, over the daemon every one its refusals carry, REVOKE_PARKS_SELF with its entry; FAILED_PRECONDITION on an
// unenrolled Sidecar, UNAVAILABLE with no daemon or once closed, INVALID_ARGUMENT for a reason neither lost nor
// compromise. The sdk's codes are the core's.
func TestRevokeCodesSurface(t *testing.T) {
	for own, core := range map[string]string{
		SidecarCodeRevokeSelf:       sidecar.DeviceRevokeCodeSelf,
		SidecarCodeRevokeNotOwn:     sidecar.DeviceRevokeCodeNotOwn,
		SidecarCodeRevokeNotCapable: sidecar.DeviceRevokeCodeNotCapable,
		SidecarCodeRevokeCancelled:  sidecar.DeviceRevokeCodeCancelled,
		SidecarCodeRevokeSince:      sidecar.DeviceRevokeCodeSince,
		SidecarCodeRevokeParksSelf:  sidecar.DeviceRevokeCodeParksSelf,
		SidecarCodeRevokeStale:      sidecar.DeviceRevokeCodeStale,
		SidecarCodeRevokeRefused:    sidecar.DeviceRevokeCodeRefused,
		SidecarRevokeLost:           sidecar.DeviceRevokeLost,
		SidecarRevokeCompromise:     sidecar.DeviceRevokeCompromise,
		SidecarDeviceLive:           sidecar.MemberDeviceLive,
		SidecarDevicePending:        sidecar.MemberDevicePending,
		SidecarDeviceParked:         sidecar.MemberDeviceParked,
		SidecarDeviceStale:          sidecar.MemberDeviceStale,
		SidecarDeviceRevoked:        sidecar.MemberDeviceRevoked,
	} {
		if own != core {
			t.Errorf("%q is the core's %q", own, core)
		}
	}

	local, authority, api := newTestApprovalsSidecar(t, nil)
	approvals := local.GetApprovals()
	targetId := sidecar.NewId()
	authority.admit(targetId, authority.memberId, 6)
	for _, preview := range []struct {
		name      string
		sidecarId sidecar.Id
		reason    string
		code      string
	}{
		{"this device", authority.ownId, SidecarRevokeLost, SidecarCodeRevokeSelf},
		{"no device of the member", sidecar.NewId(), SidecarRevokeLost, SidecarCodeRevokeNotOwn},
		{"a compromise from nowhere", targetId, SidecarRevokeCompromise, SidecarCodeRevokeSince},
		{"another reason", targetId, "stolen", SidecarCodeInvalidArgument},
	} {
		plan := awaitTestRevokePlan(t, approvals, newId([16]byte(preview.sidecarId)), preview.reason, 0, 0)
		if plan.Code != preview.code || plan.PlanId != nil {
			t.Errorf("%s: %+v", preview.name, plan)
		}
	}
	stale := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
		approvals.RevokeDevice(NewId(), nil, callback)
	})
	if stale.Ok || stale.Code != SidecarCodeRevokeStale {
		t.Errorf("a plan never previewed: %+v", stale)
	}
	authority.stateLock.Lock()
	authority.capable = false
	authority.stateLock.Unlock()
	if plan := awaitTestRevokePlan(t, approvals, newId([16]byte(targetId)), SidecarRevokeLost, 0, 0); plan.Code != SidecarCodeRevokeNotCapable {
		t.Errorf("a device not approval-capable: %+v", plan)
	}
	if count := api.postCount(); count != 0 {
		t.Errorf("%d statements posted by refusals", count)
	}

	unenrolled := newTestSidecarWorld(t).open(t)
	if plan := awaitTestRevokePlan(t, unenrolled.GetApprovals(), newId([16]byte(targetId)), SidecarRevokeLost, 0, 0); plan.Code != SidecarCodeFailedPrecondition {
		t.Errorf("an unenrolled Sidecar: %+v", plan)
	}

	// over the daemon, every refusal it carries
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	refusal := ""
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		switch request.Method {
		case sidecarControlMethodDevicesPreview:
			return map[string]any{"refusal": map[string]any{"code": refusal, "position": 31}}, nil, true
		case sidecarControlMethodDevicesRevokeBegin:
			return map[string]any{"refusal": map[string]any{"code": refusal}}, nil, true
		}
		return nil, nil, false
	}
	daemon.start()
	remoteApprovals := openTestRemoteApprovals(t, socketPath, nil)
	for _, code := range sidecarRevokeCodes {
		daemon.stateLock.Lock()
		refusal = code
		daemon.stateLock.Unlock()
		plan := awaitTestRevokePlan(t, remoteApprovals, NewId(), SidecarRevokeCompromise, 1700000000000, 0)
		if plan.Code != code || plan.PlanId != nil {
			t.Errorf("a preview refused %s: %+v", code, plan)
		}
		if parks := plan.ParksSelfPosition; (code == SidecarCodeRevokeParksSelf) != (parks == 31) {
			t.Errorf("%s's entry: %d", code, parks)
		}
		result := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
			remoteApprovals.RevokeDevice(NewId(), nil, callback)
		})
		if result.Ok || result.Code != code {
			t.Errorf("a revocation refused %s: %+v", code, result)
		}
	}

	// no daemon at all, and a closed Sidecar
	daemon.stop()
	if plan := awaitTestRevokePlan(t, remoteApprovals, NewId(), SidecarRevokeLost, 0, 0); plan.Code != SidecarCodeUnavailable {
		t.Errorf("no daemon: %+v", plan)
	}
	approvals.close()
	closed := awaitTestApprovalsResult(t, func(callback SidecarResultCallback) {
		approvals.RevokeDevice(NewId(), nil, callback)
	})
	if closed.Ok || closed.Code != SidecarCodeUnavailable {
		t.Errorf("closed: %+v", closed)
	}
}
