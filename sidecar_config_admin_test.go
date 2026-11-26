//go:build !ios_extension && !js

package sdk

// The config head's carried records for the apps (sidecar_config_admin.go; D-TEAM-26; owner decision 699; QD-1207): on
// the remote backend GetCarriedConfigRecords reads the daemon's admin.settings.show unconfirmed and binds each carried
// record with who files another; a daemon that could not read the head's records, or whose settings are not known,
// answers an error, never an empty list; on the local backend it is the core's, which answers no state before a
// verified one. The list crosses the C ABI as a json array.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A carried ServiceSettings whose signer was revoked with its member's removal, and carried VersioningRules whose
// signer's member is now a Member.
func testSidecarCarriedRecords() []*sidecar.ConfigCarriedRecord {
	removedMemberId, demotedMemberId := sidecar.NewId(), sidecar.NewId()
	return []*sidecar.ConfigCarriedRecord{
		{
			Record: sidecar.ConfigHeadServiceSettingsPath, SignerId: sidecar.NewId(), MemberId: &removedMemberId,
			Why: sidecar.ConfigCarriedRevoked, At: 42, MemberRemoved: true,
		},
		{
			Record: sidecar.ConfigHeadVersioningRulesPath, SignerId: sidecar.NewId(), MemberId: &demotedMemberId,
			Why: sidecar.ConfigCarriedRole, Role: "member",
		},
	}
}

// The daemon's carried records bind one for one, unconfirmed, for the paired network: each signer and member, why, the
// position, the role and the member's removal, and who files another, an Owner or Admin for the settings and a Network
// admin as well for the rules.
func TestGetCarriedConfigRecordsReadsTheDaemonsShow(t *testing.T) {
	carried := testSidecarCarriedRecords()
	daemon := &testAuthFeaturesDaemon{replies: map[string]any{
		sidecarControlMethodAdminSettingsShow: map[string]any{"mail_strict": "off", "carried": carried},
	}}
	records, err := (&Sidecar{remote: daemon}).GetCarriedConfigRecords()
	if err != nil || records.Len() != 2 {
		t.Fatalf("the daemon's records: %v", err)
	}
	calls := daemon.taken()
	if len(calls) != 1 || calls[0].method != sidecarControlMethodAdminSettingsShow || calls[0].confirmed {
		t.Fatalf("it sent %+v", calls)
	}
	sent := &sidecarControlAdminSettingsParams{}
	if err := json.Unmarshal(calls[0].params, sent); err != nil || *sent != (sidecarControlAdminSettingsParams{Network: "the paired network"}) {
		t.Fatalf("it asked %+v: %v", sent, err)
	}
	settings := records.Get(0)
	if settings.Record != sidecar.ConfigHeadServiceSettingsPath || settings.SignerId.String() != carried[0].SignerId.String() ||
		settings.MemberId.String() != carried[0].MemberId.String() || settings.Why != SidecarCarriedRevoked || settings.Position != 42 ||
		!settings.MemberRemoved || settings.Role != "" || !slices.Equal(settings.FiledBy.getAll(), []string{"owner", "admin"}) {
		t.Errorf("the settings: %+v filed by %v", settings, settings.FiledBy.getAll())
	}
	rules := records.Get(1)
	if rules.Record != sidecar.ConfigHeadVersioningRulesPath || rules.SignerId.String() != carried[1].SignerId.String() ||
		rules.Why != SidecarCarriedRole || rules.Role != "member" || rules.Position != 0 || rules.MemberRemoved ||
		!slices.Equal(rules.FiledBy.getAll(), []string{"owner", "admin", "network_admin"}) {
		t.Errorf("the rules: %+v filed by %v", rules, rules.FiledBy.getAll())
	}

	// as the C ABI crosses it: a json array, ids as text, who files another as an array
	encoded, err := json.Marshal(records)
	if err != nil || !strings.HasPrefix(string(encoded), "[") || !strings.Contains(string(encoded), `"signer_id":"`+carried[0].SignerId.String()+`"`) ||
		!strings.Contains(string(encoded), `"filed_by":["owner","admin","network_admin"]`) {
		t.Errorf("the list crosses as %s: %v", encoded, err)
	}
}

// A record that names no signer binds with neither signer nor member; an expired signer's record carries its not_after;
// a show that lists none answers an empty list.
func TestGetCarriedConfigRecordsBindsEachReason(t *testing.T) {
	daemon := &testAuthFeaturesDaemon{replies: map[string]any{
		sidecarControlMethodAdminSettingsShow: map[string]any{"carried": []*sidecar.ConfigCarriedRecord{
			{Record: sidecar.ConfigHeadServiceSettingsPath, Why: sidecar.ConfigCarriedSignature},
			{Record: sidecar.ConfigHeadVersioningRulesPath, SignerId: sidecar.NewId(), Why: sidecar.ConfigCarriedExpired, NotAfterMs: 1_800_000_000_000},
		}},
	}}
	records, err := (&Sidecar{remote: daemon}).GetCarriedConfigRecords()
	if err != nil || records.Len() != 2 {
		t.Fatalf("the daemon's records: %v", err)
	}
	if nameless := records.Get(0); nameless.SignerId != nil || nameless.MemberId != nil || nameless.Why != SidecarCarriedSignature {
		t.Errorf("a record naming no signer: %+v", nameless)
	}
	if expired := records.Get(1); expired.Why != SidecarCarriedExpired || expired.NotAfterMillis != 1_800_000_000_000 || expired.SignerId == nil {
		t.Errorf("an expired signer's record: %+v", expired)
	}

	none := &testAuthFeaturesDaemon{replies: map[string]any{sidecarControlMethodAdminSettingsShow: map[string]any{"mail_strict": "off"}}}
	if records, err := (&Sidecar{remote: none}).GetCarriedConfigRecords(); err != nil || records.Len() != 0 {
		t.Errorf("a show listing none: %v", err)
	}
	if encoded, err := json.Marshal(NewSidecarCarriedConfigRecordList()); err != nil || string(encoded) != "[]" {
		t.Errorf("an empty list crosses as %s: %v", encoded, err)
	}
}

// A daemon that could not read the head's records, one whose settings are not known, and one that refuses each answer
// an error, never an empty list; a local core with no verified state and a closed Sidecar too.
func TestGetCarriedConfigRecordsAnswersAnErrorWhereItCannotSay(t *testing.T) {
	for _, reply := range []map[string]any{
		{"mail_strict": "off", "carried_unknown": true},
		{"unknown": true},
	} {
		daemon := &testAuthFeaturesDaemon{replies: map[string]any{sidecarControlMethodAdminSettingsShow: reply}}
		if records, err := (&Sidecar{remote: daemon}).GetCarriedConfigRecords(); !errors.Is(err, errSidecarCarriedRecordsUnknown) || records != nil {
			t.Errorf("%v: %v", reply, err)
		}
	}
	refusing := &testAuthFeaturesDaemon{refusal: &sidecarControlError{Code: -32000, Message: "no sidecar for the network"}}
	if records, err := (&Sidecar{remote: refusing}).GetCarriedConfigRecords(); err == nil || err.Error() != refusing.refusal.Error() || records != nil {
		t.Errorf("the daemon's refusal: %v", err)
	}
	// the constructors set the Sidecar's ctx, which bounds the local read
	if records, err := (&Sidecar{ctx: context.Background(), local: &sidecar.Sidecar{}}).GetCarriedConfigRecords(); !errors.Is(err, sidecar.ErrSidecarActNoAuthority) || records != nil {
		t.Errorf("a core with no verified state: %v", err)
	}
	if records, err := (&Sidecar{}).GetCarriedConfigRecords(); !errors.Is(err, errSidecarCarriedRecordsUnavailable) || records != nil {
		t.Errorf("a closed Sidecar: %v", err)
	}
}

// Every exported signature the file adds is one gomobile binds (sidecar_accessors_compute_test.go's check).
func TestCarriedConfigRecordsSignaturesAreGomobileSafe(t *testing.T) {
	findings := testComputeSignatureFindings(t, []string{"sidecar_config_admin.go"}, map[string]string{"Sidecar": "struct", "StringList": "struct", "Id": "struct"})
	for _, finding := range findings {
		t.Error(finding)
	}
}
