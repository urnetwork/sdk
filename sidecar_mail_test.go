//go:build !ios_extension && !js && !ios && !android

package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
)

// A mail backend a test scripts: each call answers what its field gives, and the calls it took.
type testMailBackend struct {
	aliasesAnswer  []*SidecarAlias
	sendErr        error
	receiverErr    error
	coverage       string
	coverageErr    error
	vault          sidecarMailCoreVault
	receiverCalls  []bool
	sentDrafts     []*SidecarDraft
	burnedAddresss []string
}

func (self *testMailBackend) aliases(ctx context.Context) ([]*SidecarAlias, error) {
	return self.aliasesAnswer, nil
}

func (self *testMailBackend) send(ctx context.Context, draft *SidecarDraft) (string, error) {
	self.sentDrafts = append(self.sentDrafts, draft)
	if self.sendErr != nil {
		return "", self.sendErr
	}
	return "send-1", nil
}

func (self *testMailBackend) setReceiverEnabled(ctx context.Context, enabled bool) error {
	self.receiverCalls = append(self.receiverCalls, enabled)
	return self.receiverErr
}

func (self *testMailBackend) receiverCoverage(ctx context.Context) (string, error) {
	return self.coverage, self.coverageErr
}

func (self *testMailBackend) createBurner(ctx context.Context, site string) (*sidecarMailBurnerCreated, error) {
	return &sidecarMailBurnerCreated{address: "you@otter-4821.net.example", coverageCopy: sidecar.MailIndexerPrompt}, nil
}

func (self *testMailBackend) burn(ctx context.Context, address string) error {
	self.burnedAddresss = append(self.burnedAddresss, address)
	return nil
}

func (self *testMailBackend) vaultEntries(ctx context.Context) ([]*SidecarVaultEntry, error) {
	return sidecarVaultEntriesOf(ctx, self.vault)
}

func (self *testMailBackend) usedAt(ctx context.Context, entryId sidecar.Id) ([]*SidecarUsedAt, error) {
	return sidecarUsedAtOf(ctx, self.vault, entryId)
}

func (self *testMailBackend) oneTimeCodes(ctx context.Context, since time.Time) ([]*SidecarOneTimeCode, error) {
	return nil, errSidecarMailNotLanded("read one-time codes")
}

// A vault a test fills, or refuses as a device that is not the owner's consented one.
type testMailVault struct {
	entries []*protocol.VaultEntry
	usedAt  map[sidecar.Id][]*protocol.VaultUsedAt
	err     error
}

func (self *testMailVault) VaultEntries(ctx context.Context) ([]*protocol.VaultEntry, error) {
	return self.entries, self.err
}

func (self *testMailVault) UsedAt(ctx context.Context, entryId sidecar.Id) ([]*protocol.VaultUsedAt, error) {
	if self.err != nil {
		return nil, self.err
	}
	return self.usedAt[entryId], nil
}

// A SidecarMail on backend with the defaults, closed at the test's end.
func newTestSidecarMail(t *testing.T, backend sidecarMailBackend) *SidecarMail {
	t.Helper()
	settings := defaultSidecarMailSettings()
	settings.CallTimeout = testSidecarTimeout
	settings.ReadTimeout = testSidecarTimeout
	mail := newSidecarMail(context.Background(), backend, settings)
	t.Cleanup(mail.close)
	return mail
}

// A remote-mode app's SetReceiverEnabled files its daemon's IndexerSet for mail at the member scope, as a confirmed
// call, and retires it: the daemon sees exactly ur indexer's params, which it reads strictly, and its refusal reaches
// the app as MAIL_REFUSED.
func TestReceiverEnableFilesIndexerSet(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	refuse := false
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		switch request.Method {
		case sidecarControlMethodIndexerDesignate, sidecarControlMethodIndexerRetire:
			if refuse {
				return nil, &sidecar.DaemonRpcError{Code: sidecar.DaemonRpcFailed, Message: "synthetic refusal"}, true
			}
			return nil, nil, true
		}
		return nil, nil, false
	}
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	mail := remote.GetMail()
	if mail == nil {
		t.Fatal("a remote-mode Sidecar has no mail")
	}

	enable := newTestEvsCallback[*SidecarResult]()
	mail.SetReceiverEnabled(true, enable)
	if result, err := enable.await(t); err != nil || !result.Ok {
		t.Fatalf("enable: %+v, %v", result, err)
	}
	disable := newTestEvsCallback[*SidecarResult]()
	mail.SetReceiverEnabled(false, disable)
	if result, err := disable.await(t); err != nil || !result.Ok {
		t.Fatalf("disable: %+v, %v", result, err)
	}

	designations := []testRemoteDaemonRequest{}
	for _, request := range daemon.seen() {
		if request.Method == sidecarControlMethodIndexerDesignate || request.Method == sidecarControlMethodIndexerRetire {
			designations = append(designations, request)
		}
	}
	if len(designations) != 2 {
		t.Fatalf("the daemon took %d designation calls", len(designations))
	}
	designate, retire := designations[0], designations[1]
	if designate.Method != sidecarControlMethodIndexerDesignate || !designate.UrConfirmed {
		t.Errorf("enable called %s, confirmed %v", designate.Method, designate.UrConfirmed)
	}
	designateParams := map[string]any{}
	if err := json.Unmarshal(designate.Params, &designateParams); err != nil {
		t.Fatal(err)
	}
	if len(designateParams) != 2 || designateParams["scope"] != float64(protocol.IndexerScope_INDEXER_SCOPE_MEMBER) {
		t.Errorf("designate's params %s", designate.Params)
	}
	if services, ok := designateParams["services"].([]any); !ok || len(services) != 1 || services[0] != float64(protocol.IndexerService_INDEXER_SERVICE_MAIL) {
		t.Errorf("designate names services %v, not mail alone", designateParams["services"])
	}
	if retire.Method != sidecarControlMethodIndexerRetire || !retire.UrConfirmed {
		t.Errorf("disable called %s, confirmed %v", retire.Method, retire.UrConfirmed)
	}
	if string(retire.Params) != `{"scope":2}` {
		t.Errorf("retire's params %s", retire.Params)
	}

	refuse = true
	refused := newTestEvsCallback[*SidecarResult]()
	mail.SetReceiverEnabled(true, refused)
	if result, err := refused.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeRefused {
		t.Errorf("a daemon's refusal answered %+v, %v", result, err)
	}
}

// The local backend never makes an app host a mail indexer (owner decision 197): SetReceiverEnabled answers
// MAIL_REFUSED either way and reaches nothing.
func TestReceiverEnableRefusedLocally(t *testing.T) {
	mail := newTestSidecarMail(t, &sidecarMailCoreBackend{})
	for _, enabled := range []bool{true, false} {
		callback := newTestEvsCallback[*SidecarResult]()
		mail.SetReceiverEnabled(enabled, callback)
		result, err := callback.await(t)
		if err != nil || result.Ok || result.Code != sidecarMailCodeRefused || !strings.Contains(result.Message, "indexer") {
			t.Errorf("SetReceiverEnabled(%v) locally answered %+v, %v", enabled, result, err)
		}
	}
}

// GetAliases over the daemon reads id.ls's addresses, and over a backend that answers nothing an empty list.
func TestMailAliasesOverTheDaemon(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		if request.Method != sidecarControlMethodIdList {
			return nil, nil, false
		}
		return map[string]any{"addresses": []map[string]any{
			{"address": "admin@net.example", "kind": "admin", "known": true},
			{"address": "you@otter-4821.net.example", "kind": "burner", "known": false},
		}}, nil, true
	}
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	aliases := remote.GetMail().GetAliases()
	if aliases.Len() != 2 {
		t.Fatalf("%d aliases", aliases.Len())
	}
	if first := aliases.Get(0); first.Address != "admin@net.example" || first.Kind != "admin" || !first.Known {
		t.Errorf("first alias %+v", first)
	}
	if second := aliases.Get(1); second.Kind != "burner" || second.Known {
		t.Errorf("second alias %+v", second)
	}

	daemon.stop()
	if aliases := remote.GetMail().GetAliases(); aliases.Len() != 0 {
		t.Errorf("no daemon answered %d aliases", aliases.Len())
	}
}

// MailClient carries VaultEntries and UsedAt (MAIL-M8-01), so the core backend's check finds them and its reads go to
// the core (TestMailVaultReadsMapAndRefuse runs them over a vault); this holds the check to the root's signatures.
var _ sidecarMailCoreVault = (*sidecar.MailClient)(nil)

// Every call whose root call is not wired answers MAIL_UNAVAILABLE on both backends, naming what it waits on, and
// coverage answers foreground-only, which claims no indexer. Vault entries and Used at answer so on the remote backend
// alone: the core backend reads them from its MailClient, which this test's has none of.
func TestMailCallsAwaitingTheCoreAnswerUnavailable(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	for name, mail := range map[string]*SidecarMail{
		"local":  newTestSidecarMail(t, &sidecarMailCoreBackend{client: nil}),
		"remote": remote.GetMail(),
	} {
		draft := &SidecarDraft{From: "admin@net.example", To: NewStringList(), Subject: "s", Body: "b"}
		draft.To.Add("someone@elsewhere.example")
		send := newTestEvsCallback[*SidecarResult]()
		mail.Send(draft, send)
		if result, err := send.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeUnavailable {
			t.Errorf("%s Send answered %+v, %v", name, result, err)
		}
		burner := newTestEvsCallback[*SidecarBurner]()
		mail.CreateBurner("shop.example", burner)
		if result, err := burner.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeUnavailable {
			t.Errorf("%s CreateBurner answered %+v, %v", name, result, err)
		}
		burn := newTestEvsCallback[*SidecarResult]()
		mail.Burn("you@otter-4821.net.example", burn)
		if result, err := burn.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeUnavailable {
			t.Errorf("%s Burn answered %+v, %v", name, result, err)
		}
		if name == "remote" {
			entries := newTestEvsCallback[*SidecarVaultEntries]()
			mail.GetVaultEntries(entries)
			if result, err := entries.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeUnavailable {
				t.Errorf("%s GetVaultEntries answered %+v, %v", name, result, err)
			}
			usedAt := newTestEvsCallback[*SidecarUsedAtResult]()
			mail.GetUsedAt(newId([16]byte{1}), usedAt)
			if result, err := usedAt.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeUnavailable {
				t.Errorf("%s GetUsedAt answered %+v, %v", name, result, err)
			}
		}
		codes := newTestEvsCallback[*SidecarOneTimeCodes]()
		mail.GetOneTimeCodes(0, codes)
		if result, err := codes.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeUnavailable {
			t.Errorf("%s GetOneTimeCodes answered %+v, %v", name, result, err)
		}
		if coverage := mail.GetReceiverCoverage(); coverage != SidecarReceiverCoverageForegroundOnly {
			t.Errorf("%s coverage reads %q", name, coverage)
		}
	}
}

// Coverage passes the backend's three values, and a value it does not know, or a backend that cannot tell, reads
// foreground-only.
func TestMailCoverageValues(t *testing.T) {
	for _, testCase := range []struct {
		coverage string
		err      error
		want     string
	}{
		{coverage: SidecarReceiverCoverageIndexer, want: SidecarReceiverCoverageIndexer},
		{coverage: SidecarReceiverCoverageForegroundOnly, want: SidecarReceiverCoverageForegroundOnly},
		{coverage: SidecarReceiverCoverageNone, want: SidecarReceiverCoverageNone},
		{coverage: "everywhere", want: SidecarReceiverCoverageForegroundOnly},
		{err: errors.New("synthetic"), want: SidecarReceiverCoverageForegroundOnly},
	} {
		mail := newTestSidecarMail(t, &testMailBackend{coverage: testCase.coverage, coverageErr: testCase.err})
		if coverage := mail.GetReceiverCoverage(); coverage != testCase.want {
			t.Errorf("coverage %q, %v reads %q", testCase.coverage, testCase.err, coverage)
		}
	}
}

// The vault's entries cross with their fields and without the TOTP secret, Used at with its times, and a device that
// is not the owner's consented one answers MAIL_NOT_CONSENTED for both.
func TestMailVaultReadsMapAndRefuse(t *testing.T) {
	entryId := sidecar.Id{0x21}
	vault := &testMailVault{
		entries: []*protocol.VaultEntry{{
			EntryId:  entryId.Bytes(),
			Address:  "you@otter-4821.net.example",
			Title:    "Shop",
			Username: "you",
			Password: "synthetic-password",
			Totp:     &protocol.VaultTotp{Secret: []byte("synthetic-totp-secret"), Digits: 6, Period: 30},
			Urls:     []string{"https://shop.example"},
			Created:  1000,
			Updated:  2000,
			State:    protocol.VaultEntryState_VAULT_ENTRY_BURNED,
		}},
		usedAt: map[sidecar.Id][]*protocol.VaultUsedAt{entryId: {{RegistrableDomain: "shop.example", First: 10, Last: 20}}},
	}
	mail := newTestSidecarMail(t, &testMailBackend{vault: vault})

	entries := newTestEvsCallback[*SidecarVaultEntries]()
	mail.GetVaultEntries(entries)
	result, err := entries.await(t)
	if err != nil || !result.Ok || result.Entries.Len() != 1 {
		t.Fatalf("entries %+v, %v", result, err)
	}
	entry := result.Entries.Get(0)
	if entry.EntryId == nil || entry.EntryId.id != [16]byte(entryId) || entry.Address != "you@otter-4821.net.example" ||
		entry.Password != "synthetic-password" || !entry.HasTotp || entry.State != "burned" ||
		entry.CreatedMillis != 1000 || entry.UpdatedMillis != 2000 || entry.Urls.Len() != 1 {
		t.Errorf("entry %+v", entry)
	}
	encoded, _ := json.Marshal(entry)
	if strings.Contains(string(encoded), "synthetic-totp-secret") || strings.Contains(string(encoded), "c3ludGhldGljLXRvdHAtc2VjcmV0") {
		t.Error("the TOTP secret crossed")
	}

	usedAt := newTestEvsCallback[*SidecarUsedAtResult]()
	mail.GetUsedAt(newId([16]byte(entryId)), usedAt)
	if result, err := usedAt.await(t); err != nil || !result.Ok || result.UsedAt.Len() != 1 ||
		*result.UsedAt.Get(0) != (SidecarUsedAt{RegistrableDomain: "shop.example", FirstMillis: 10, LastMillis: 20}) {
		t.Errorf("used at %+v, %v", result, err)
	}

	vault.err = sidecar.ErrMailVaultNotConsented
	refused := newTestEvsCallback[*SidecarVaultEntries]()
	mail.GetVaultEntries(refused)
	if result, err := refused.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeNotConsented {
		t.Errorf("a device that is not consented read %+v, %v", result, err)
	}
	refusedUsedAt := newTestEvsCallback[*SidecarUsedAtResult]()
	mail.GetUsedAt(newId([16]byte(entryId)), refusedUsedAt)
	if result, err := refusedUsedAt.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeNotConsented {
		t.Errorf("a device that is not consented read Used at %+v, %v", result, err)
	}
}

// A call that names nothing it can act on answers MAIL_INVALID and reaches no backend.
func TestMailInvalidArgs(t *testing.T) {
	backend := &testMailBackend{}
	mail := newTestSidecarMail(t, backend)
	noRecipient := newTestEvsCallback[*SidecarResult]()
	mail.Send(&SidecarDraft{From: "admin@net.example"}, noRecipient)
	if result, _ := noRecipient.await(t); result.Code != sidecarMailCodeInvalid {
		t.Errorf("a draft with no recipient answered %+v", result)
	}
	noDraft := newTestEvsCallback[*SidecarResult]()
	mail.Send(nil, noDraft)
	if result, _ := noDraft.await(t); result.Code != sidecarMailCodeInvalid {
		t.Errorf("no draft answered %+v", result)
	}
	noSite := newTestEvsCallback[*SidecarBurner]()
	mail.CreateBurner("", noSite)
	if result, _ := noSite.await(t); result.Code != sidecarMailCodeInvalid {
		t.Errorf("no site answered %+v", result)
	}
	noAddress := newTestEvsCallback[*SidecarResult]()
	mail.Burn("", noAddress)
	if result, _ := noAddress.await(t); result.Code != sidecarMailCodeInvalid {
		t.Errorf("no address answered %+v", result)
	}
	noEntry := newTestEvsCallback[*SidecarUsedAtResult]()
	mail.GetUsedAt(nil, noEntry)
	if result, _ := noEntry.await(t); result.Code != sidecarMailCodeInvalid {
		t.Errorf("no entry answered %+v", result)
	}
	if len(backend.sentDrafts) != 0 || len(backend.burnedAddresss) != 0 {
		t.Error("an invalid call reached the backend")
	}
}

// An outward action the core's gate holds comes back approval_pending:<id>, not Ok, and a created burner's copy and
// a burn reach the app; the draft the backend takes is the Sidecar's copy, not the caller's.
func TestMailGateAndBurnerAnswers(t *testing.T) {
	approvalId := sidecar.NewId()
	backend := &testMailBackend{sendErr: &sidecar.ApprovalPendingError{ApprovalId: approvalId}}
	mail := newTestSidecarMail(t, backend)
	draft := &SidecarDraft{From: "admin@net.example", To: NewStringList()}
	draft.To.Add("someone@elsewhere.example")
	send := newTestEvsCallback[*SidecarResult]()
	mail.Send(draft, send)
	draft.To.Add("late@elsewhere.example")
	result, err := send.await(t)
	if err != nil || result.Ok || result.Value != sidecarApprovalPendingPrefix+approvalId.String() || result.Code != "" {
		t.Errorf("a held send answered %+v, %v", result, err)
	}
	if len(backend.sentDrafts) != 1 || backend.sentDrafts[0].To.Len() != 1 {
		t.Error("the backend saw the caller's draft change after Send")
	}

	burner := newTestEvsCallback[*SidecarBurner]()
	mail.CreateBurner("shop.example", burner)
	if result, err := burner.await(t); err != nil || !result.Ok || result.CoverageCopy != sidecar.MailIndexerPrompt {
		t.Errorf("a first burner answered %+v, %v", result, err)
	}
	burn := newTestEvsCallback[*SidecarResult]()
	mail.Burn("you@otter-4821.net.example", burn)
	if result, err := burn.await(t); err != nil || !result.Ok || len(backend.burnedAddresss) != 1 {
		t.Errorf("a burn answered %+v, %v", result, err)
	}
}

// A closed SidecarMail answers every call MAIL_UNAVAILABLE.
func TestMailClosedAnswersUnavailable(t *testing.T) {
	backend := &testMailBackend{}
	mail := newTestSidecarMail(t, backend)
	mail.close()
	callback := newTestEvsCallback[*SidecarResult]()
	mail.SetReceiverEnabled(true, callback)
	if result, _ := callback.await(t); result.Ok || result.Code != sidecarMailCodeUnavailable {
		t.Errorf("a closed mail answered %+v", result)
	}
	if len(backend.receiverCalls) != 0 {
		t.Error("a closed mail reached its backend")
	}
	if aliases := mail.GetAliases(); aliases.Len() != 0 {
		t.Error("a closed mail listed aliases")
	}
}

// The mail component waits for the enrollment, and a local Sidecar that names no mail has none, while a named one
// builds its factory's component and GetMail wraps it.
func TestMailComponentAndGetMail(t *testing.T) {
	world := newTestSidecarWorld(t)
	local := world.open(t)
	if _, err := newSidecarMailComponent(context.Background(), local.Core(), local.settings); !errors.Is(err, errSidecarComponentNotEnrolled) {
		t.Errorf("an unenrolled core's mail component: %v", err)
	}
	if local.GetMail() != nil {
		t.Error("a Sidecar that named no mail has mail")
	}
	world.close(t, local)
}

// The control methods and params the remote backend sends are ur id's and ur indexer's as cli declares them, read from
// its source since package main exports nothing, so a renamed method or field fails here rather than at the daemon,
// which reads params strictly.
func TestMailControlWireMatchesUr(t *testing.T) {
	cliDirectory := filepath.Join("..", "sidecar", "cli")
	declarations := testSidecarDeclarations(t, filepath.Join(cliDirectory, "cmd_id.go"), filepath.Join(cliDirectory, "cmd_indexer.go"))
	for own, cliName := range map[string]string{
		sidecarControlMethodIdList:           "urMethodIdList",
		sidecarControlMethodIndexerDesignate: "urMethodIndexerDesignate",
		sidecarControlMethodIndexerRetire:    "urMethodIndexerRetireOne",
	} {
		if !strings.Contains(declarations[cliName], strconv.Quote(own)) {
			t.Errorf("%s is not %q: %s", cliName, own, declarations[cliName])
		}
	}
	for _, field := range []string{`json:"scope"`, `json:"services,omitempty"`} {
		if !strings.Contains(declarations["urIndexerParams"], field) {
			t.Errorf("ur indexer's params lack %s", field)
		}
	}
	for _, field := range []string{`json:"address"`, `json:"kind"`, `json:"known"`} {
		if !strings.Contains(declarations["urIdAddress"], field) {
			t.Errorf("ur id's address lacks %s", field)
		}
	}
	if !strings.Contains(declarations["urIdListReply"], `json:"addresses"`) || !strings.Contains(declarations["urIdParams"], `json:"network,omitempty"`) {
		t.Error("ur id's list reply or params changed")
	}
}
