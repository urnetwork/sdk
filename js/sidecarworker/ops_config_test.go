//go:build !sidecar_http && !js

package sidecarworker

// config_view natively on sidecartest (SDK-M6-02, §23.4's "the console shows members and policy"): a verified
// ConfigState of sidecartest's admission world, every statement signed and applied by every kind of verifier, its
// head saved with a names table, K_sock and a policy that ACL's own compile compiles. The view shows members with
// their roles and the policy's emails for them, sidecars with their client names, tags, read_only and consent, the
// head's tags, groups and labels, the policy's document and rules, and the settings; it carries no key; and it fails
// closed while no verified state is given.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
)

// The policy the world saves: a tag its admins own, a group naming alice, a grant from the group to the tag on two
// port forms and a requests rule.
const testConfigPolicySource = `{
  "tagOwners": { "tag:ci": ["autogroup:admin"] },
  "groups":    { "group:eng": ["alice@team.example"] },
  "grants": [
    { "id": "eng-to-ci", "note": "engineers reach the build hosts", "src": ["group:eng"], "dst": ["tag:ci"], "allow": ["dial"],
      "ip": ["tcp:22", "8000-8080"] }
  ]
}`

// A team network's verified config as the console reads it: an owner, alice and her devices, her read_only page
// Worker, a tagged CI runner, and the head they stand on.
type testConfigWorld struct {
	t           *testing.T
	net         *sidecartest.AdmissionNet
	plan        *sidecar.AclSavePlan
	aliceId     sidecar.Id
	aliceLaptop *sidecartest.AdmissionDevice
	pageWorker  *sidecartest.AdmissionDevice
	ciRunner    *sidecartest.AdmissionDevice
	ciTagId     sidecar.Id
	ciNotAfter  int64
	engGroupId  sidecar.Id
	// K_sock as the head holds it, which no view may carry
	socketKey []byte
}

func newTestConfigWorld(t *testing.T) *testConfigWorld {
	t.Helper()
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	net := sidecartest.NewAdmissionNet(t, &sidecartest.AdmissionTier{
		Cluster: cluster,
		NewApi: func(clock sidecar.Clock, cluster sidecar.BlobCluster) sidecartest.AdmissionApi {
			return sidecartest.FakeApiAdmission(sidecartest.NewFakeApi(t.Context(), clock, sidecartest.NewFaultQueue(), nil, cluster, nil))
		},
		AclCompile: sidecartest.AclAdmissionCompile,
	})
	t.Cleanup(net.Close)
	world := &testConfigWorld{t: t, net: net, socketKey: bytes.Repeat([]byte{0x5e}, 32)}
	world.aliceId, world.aliceLaptop = net.ConsentedMember("alice")
	world.pageWorker = net.Device("alice page worker", world.aliceId)
	net.AdmitDevice(world.aliceLaptop, world.pageWorker, uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_READ_ONLY))

	keyFileBytes, err := proto.Marshal(&protocol.SocketKeyFile{CurrentKey: world.socketKey})
	if err != nil {
		t.Fatal(err)
	}
	namesBytes, err := proto.Marshal(&protocol.ConfigNameTable{Names: []*protocol.ConfigName{
		{SidecarId: world.aliceLaptop.SidecarId.Bytes(), Name: "alice-laptop"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	net.SaveHead(func(head *sidecar.ConfigHead) (*sidecar.ConfigHead, error) {
		head, err := head.WithFile(sidecar.ConfigHeadSocketKeyPath, keyFileBytes)
		if err != nil {
			return nil, err
		}
		return head.WithFile(sidecar.ConfigHeadNamesPath, namesBytes)
	})
	world.plan, _ = net.SaveAclPolicy([]byte(testConfigPolicySource), sidecartest.AclSaveMembers{"alice@team.example": world.aliceId})
	var ok bool
	if world.ciTagId, ok = sidecartest.AclTagAtom(world.plan, "ci"); !ok {
		t.Fatal("the policy names no tag:ci")
	}
	if world.engGroupId, ok = sidecartest.AclGroupAtom(world.plan, "eng"); !ok {
		t.Fatal("the policy names no group:eng")
	}
	// a CI runner tagged ci, which the policy lets admins apply, its admission ending in 30 days as a tagged one must
	world.ciRunner = net.Device("ci runner", sidecar.Id{})
	world.ciNotAfter = net.Clock().Now().Add(30 * 24 * time.Hour).UnixMilli()
	net.MustLand(net.Sign(net.Owner(), bridge.SignedType_SIGNED_SIDECAR_ADMIT, net.Admission(world.ciRunner, []sidecar.Id{world.ciTagId}, false, world.ciNotAfter, 0)))
	return world
}

// What the Worker is given: the tip's verified state, the head its newest base names and the policy compiled from it.
func (self *testConfigWorld) configState() *WorkerConfigState {
	return &WorkerConfigState{State: self.net.State(), Head: self.plan.Head, Local: self.plan.Local}
}

// A Worker on the world's clock whose config seam answers the world's config.
func (self *testConfigWorld) worker(adjust func(args *WorkerArgs)) *testWorkerWorld {
	return newTestWorkerWorld(self.t, func(args *WorkerArgs) {
		args.Now = self.net.Clock().Now
		args.Config = func() (*WorkerConfigState, error) {
			return self.configState(), nil
		}
		if adjust != nil {
			adjust(args)
		}
	})
}

// The sidecar view of sidecarId, failing unless one shows.
func testSidecarViewOf(t *testing.T, view *ConfigView, sidecarId sidecar.Id) *ConfigSidecarView {
	t.Helper()
	for _, sidecarView := range view.Sidecars {
		if sidecarView.SidecarId == sidecarId.String() {
			return sidecarView
		}
	}
	t.Fatalf("no sidecar view of %s", sidecarId)
	return nil
}

// config_view shows the members with their roles and the policy's emails, each sidecar with its client name, its
// member or tags, read_only and consent, the head's tags and groups, the policy's document and its rules' detail, the
// settings with their defaults, and the AdminSet's k of n, all of the tip's verified state.
func TestConfigViewShowsMembersAndPolicy(t *testing.T) {
	world := newTestConfigWorld(t)
	worker := world.worker(nil)
	reply := testReplyOf(t, worker.worker, &Request{Id: "config", Operation: OperationConfigView})
	view := reply.Config
	if !reply.Ok || view == nil {
		t.Fatalf("the config view: %+v", reply)
	}
	state := world.net.State()
	if view.NetworkId != world.net.NetworkId().String() || view.Version != fmt.Sprint(state.Pin().GetVersion()) || view.TimeMillis != state.TimeMs() ||
		view.ConfigVersion != fmt.Sprint(state.Base().GetConfigVersion()) || view.HeadUnread || view.Truncated {
		t.Errorf("the view's position: %+v", view)
	}
	if view.ThresholdK != 1 || view.ThresholdN != 1 {
		t.Errorf("k of n: %d of %d", view.ThresholdK, view.ThresholdN)
	}

	roles := map[string]*ConfigMemberView{}
	for _, member := range view.Members {
		roles[member.MemberId] = member
	}
	owner, alice := roles[world.net.Owner().MemberId.String()], roles[world.aliceId.String()]
	if len(view.Members) != 2 || owner == nil || owner.Role != "owner" || alice == nil || alice.Role != "member" || !slices.Equal(alice.Emails, []string{"alice@team.example"}) ||
		owner.Emails != nil {
		t.Errorf("the members: %+v", view.Members)
	}

	laptop := testSidecarViewOf(t, view, world.aliceLaptop.SidecarId)
	if laptop.Name != "alice-laptop" || laptop.MemberId != world.aliceId.String() || !laptop.Consented || laptop.ReadOnly || laptop.State != "live" ||
		laptop.TagIds != nil || laptop.AdmittedTimeMillis == 0 {
		t.Errorf("alice's laptop: %+v", laptop)
	}
	page := testSidecarViewOf(t, view, world.pageWorker.SidecarId)
	if !page.ReadOnly || page.Name != "" || page.MemberId != world.aliceId.String() || page.State != "live" {
		t.Errorf("alice's page Worker: %+v", page)
	}
	runner := testSidecarViewOf(t, view, world.ciRunner.SidecarId)
	if !slices.Equal(runner.TagIds, []string{world.ciTagId.String()}) || runner.MemberId != "" || runner.Consented || runner.State != "live" ||
		runner.NotAfterMillis != world.ciNotAfter {
		t.Errorf("the CI runner: %+v", runner)
	}
	ownerDevice := testSidecarViewOf(t, view, world.net.Owner().SidecarId)
	if ownerDevice.MemberId != world.net.Owner().MemberId.String() || ownerDevice.State != "live" {
		t.Errorf("the owner's device: %+v", ownerDevice)
	}

	if len(view.Tags) != 1 || *view.Tags[0] != (ConfigNameView{Id: world.ciTagId.String(), Name: "ci"}) ||
		len(view.Groups) != 1 || *view.Groups[0] != (ConfigNameView{Id: world.engGroupId.String(), Name: "eng"}) {
		t.Errorf("tags %+v, groups %+v", view.Tags, view.Groups)
	}

	policy := view.Policy
	if policy == nil || !policy.HasDocument || policy.Source == nil || policy.Source.Text != testConfigPolicySource || policy.Source.Base64 != "" || policy.SourceOmitted {
		t.Fatalf("the policy's document: %+v", policy)
	}
	rule := (*ConfigRuleView)(nil)
	for _, candidate := range policy.Rules {
		if candidate.DocumentId == "eng-to-ci" {
			rule = candidate
		}
	}
	if rule == nil || rule.Note != "engineers reach the build hosts" || !slices.Equal(rule.Ports, []string{"tcp:22", "8000-8080"}) || rule.RuleId == "" {
		t.Errorf("the eng-to-ci rule: %+v of %+v", rule, policy.Rules)
	}

	settings := view.Settings
	if settings == nil || settings.SelfAdmit != "on" || !settings.HeadKnown || settings.StaleLeafDays != 90 || settings.WitnessBoundHours != 72 ||
		settings.DirectoryRuns != "auto" || settings.DirectoryWorkers != "on" || settings.TeamMail != "" || settings.ScimSignerSidecarId != "" {
		t.Errorf("the settings: %+v", settings)
	}

	// the runner's admission ends at its not_after, by the Worker's clock
	world.net.Clock().Advance(31 * 24 * time.Hour)
	later := testReplyOf(t, worker.worker, &Request{Id: "later", Operation: OperationConfigView})
	if state := testSidecarViewOf(t, later.Config, world.ciRunner.SidecarId).State; state != "expired" {
		t.Errorf("the CI runner past its not_after reads %s", state)
	}
	if state := testSidecarViewOf(t, later.Config, world.aliceLaptop.SidecarId).State; state != "live" {
		t.Errorf("alice's laptop reads %s", state)
	}
}

// No reply carries K_sock, which the head holds beside the files a view reads, nor any device's key, in any encoding:
// a view is built field by field, and the head's other files never cross.
func TestConfigViewCarriesNoKey(t *testing.T) {
	world := newTestConfigWorld(t)
	capture := sidecartest.NewPrivacyCapture()
	capture.AddSecret("sockets K_sock", world.socketKey)
	for _, device := range []*sidecartest.AdmissionDevice{world.net.Owner(), world.aliceLaptop, world.pageWorker, world.ciRunner} {
		capture.AddSecret("identity key of "+device.Name, device.Key.Seed())
	}
	worker := world.worker(nil)
	encoded := worker.worker.HandleJson(context.Background(), []byte(`{"id":"config","op":"config_view"}`))
	if !bytes.Contains(encoded, []byte(`"ok":true`)) {
		t.Fatalf("the config view: %s", encoded)
	}
	capture.Add(sidecartest.PrivacyClass("CC4/page"), "config view", encoded)
	for _, finding := range capture.Scan() {
		t.Errorf("%s", finding)
	}
	// the control: a reply that did carry K_sock, as hex or base64, is found
	for _, encoding := range []string{hex.EncodeToString(world.socketKey), base64.StdEncoding.EncodeToString(world.socketKey)} {
		control := sidecartest.NewPrivacyCapture()
		control.AddSecret("sockets K_sock", world.socketKey)
		control.Add("CC4/page", "planted", []byte(`{"id":"x","config":{"key":"`+encoding+`"}}`))
		if len(control.Scan()) == 0 {
			t.Fatalf("CONTROL FAILED: K_sock as %s was not found", encoding)
		}
	}
}

// Without a verified state the view fails closed by its seam's answer: no seam and no state answer unavailable, as
// does a leg that verifies none; an unenrolled Worker answers not_enrolled; a head that did not open still shows the
// state's members and sidecars, saying so.
func TestConfigViewFailsClosedWithoutAVerifiedState(t *testing.T) {
	unwired := newTestWorkerWorld(t, nil)
	if reply := testReplyOf(t, unwired.worker, &Request{Id: "config", Operation: OperationConfigView}); reply.Ok || reply.Code != CodeUnavailable {
		t.Errorf("no config seam: %+v", reply)
	}
	for _, c := range []struct {
		config *WorkerConfigState
		err    error
		code   string
	}{
		{err: sidecar.ErrSidecarNoIdentity, code: CodeNotEnrolled},
		{err: fmt.Errorf("%w: no config state verified on this leg", sidecar.ErrEvsNoConfig), code: CodeUnavailable},
		{config: nil, code: CodeUnavailable},
		{config: &WorkerConfigState{}, code: CodeUnavailable},
	} {
		world := newTestWorkerWorld(t, func(args *WorkerArgs) {
			args.Config = func() (*WorkerConfigState, error) {
				return c.config, c.err
			}
		})
		if reply := testReplyOf(t, world.worker, &Request{Id: "config", Operation: OperationConfigView}); reply.Ok || reply.Code != c.code || reply.Message != codeMessages[c.code] {
			t.Errorf("%v: %+v", c.err, reply)
		}
	}

	world := newTestConfigWorld(t)
	rowsOnly := newTestWorkerWorld(t, func(args *WorkerArgs) {
		args.Config = func() (*WorkerConfigState, error) {
			return &WorkerConfigState{State: world.net.State()}, nil
		}
	})
	reply := testReplyOf(t, rowsOnly.worker, &Request{Id: "config", Operation: OperationConfigView})
	if !reply.Ok || !reply.Config.HeadUnread || reply.Config.Policy != nil || len(reply.Config.Tags) != 0 || len(reply.Config.Members) != 2 ||
		testSidecarViewOf(t, reply.Config, world.aliceLaptop.SidecarId).Name != "" {
		t.Errorf("a view whose head did not open: %+v", reply.Config)
	}
}

// Each list stops at ConfigMaxRowCount and marks the view truncated, and a document past
// ConfigPolicySourceMaxByteCount is left out and marked.
func TestConfigViewBounds(t *testing.T) {
	world := newTestConfigWorld(t)
	worker := world.worker(func(args *WorkerArgs) {
		args.Settings.ConfigMaxRowCount = 1
		args.Settings.ConfigPolicySourceMaxByteCount = 16
	})
	reply := testReplyOf(t, worker.worker, &Request{Id: "config", Operation: OperationConfigView})
	view := reply.Config
	if !reply.Ok || !view.Truncated || len(view.Members) != 1 || len(view.Sidecars) != 1 || 1 < len(view.Policy.Rules) {
		t.Fatalf("a view over its row bound: %+v", view)
	}
	if view.Policy.Source != nil || !view.Policy.SourceOmitted {
		t.Fatalf("a document over its bound: %+v", view.Policy)
	}
}

// A port range reads as the document wrote it.
func TestConfigPortRangeText(t *testing.T) {
	for _, c := range []struct {
		ports sidecar.AclPortRange
		text  string
	}{
		{ports: sidecar.AclPortRange{Last: 65535}, text: "*"},
		{ports: sidecar.AclPortRange{Protocol: configIpProtocolTcp, First: 22, Last: 22}, text: "tcp:22"},
		{ports: sidecar.AclPortRange{Protocol: configIpProtocolUdp, First: 1000, Last: 2000}, text: "udp:1000-2000"},
		{ports: sidecar.AclPortRange{Protocol: configIpProtocolIcmp, Last: 65535}, text: "icmp:*"},
		{ports: sidecar.AclPortRange{First: 443, Last: 443}, text: "443"},
		{ports: sidecar.AclPortRange{Protocol: configIpProtocolTcp, Last: 65535}, text: "tcp:*"},
	} {
		if text := configPortRangeText(c.ports); text != c.text {
			t.Errorf("%+v reads %q, want %q", c.ports, text, c.text)
		}
	}
}
