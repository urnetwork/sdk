//go:build !ios_extension && !js && !ios && !android

package sdk

// SidecarMail's send capability (SDK-M9-01): FS-123's bound half on a scripted core, §18.5's copy and its never row,
// the I-11 label, the mappings' fail-closed words, the local backend reaching the core's MailClient.CheckSend and
// SendResult (QD-1122) with the draft whole, and both backends answering MAIL_UNAVAILABLE where there is nothing to
// call.
//
// The remote backend over ur daemon's mail.check_send, mail.send_result and mail.send (QD-853, owner decision 288):
// their JSON held to the root's types both ways, the calls sent unconfirmed with the draft's fields, and end to end
// over the root's own daemon on scripted backends, a check's domains, a known send's outcomes, an unknown id refused,
// and a send the gate holds answered approval_pending:<id> until it is approved; the daemon's own refusals read as
// their MAIL_ codes, its own backends taking the hosted core's MailClient. The root's tests run the real gate's hold
// over MAIL-M7-18's send world (mail_send_gate_test.go, mail_send_daemon_test.go), and the daemon's own backends end
// to end over a bound MailClient, a check by its sender and a send's result from or/ (mail_send_daemon_test.go); this
// module's tests import no sidecartest world that binds one (dependency_boundary_test.go), so the local backend is
// held here to the call it makes.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// SDK-M8-04's test backend has no core check or results: each answers as the core backend does until they land.
func (self *testMailBackend) checkSend(ctx context.Context, draft *SidecarDraft) (*sidecar.SmtpSendCheck, error) {
	return nil, errSidecarMailNotLanded("check a send")
}

func (self *testMailBackend) sendResult(ctx context.Context, sendId sidecar.Id) (*sidecar.SmtpSendResult, error) {
	return nil, errSidecarMailNotLanded("read a send's result")
}

// A core's send capability a test scripts. Check decides each domain's prediction by §14.5's rule over the mode the
// test has taught it, as the pin store learns one from a session or an outcome; Send queues every domain, as a
// phone's outbox does; and each outcome the test writes, as a prover host writes or/, raises mail_send through the
// dispatch, as the composing device's core does.
type testMailSendCore struct {
	testMailBackend
	dispatch *sidecarEventDispatch

	stateLock sync.Mutex
	// each domain's mode, unknown until taught
	domainModes map[string]sidecar.SmtpMode
	// what every message checks at: its encoded bytes and the cap of the host that would prove it
	byteCount    uint64
	capByteCount uint64
	bdatDelay    time.Duration
	dataDelay    time.Duration
	// each send's outcomes, in its domains' order
	sendOutcomes map[sidecar.Id][]*sidecar.SmtpDomainOutcome
	// what the calls were given
	checkedDrafts []*SidecarDraft
	readSendIds   []sidecar.Id
}

func newTestMailSendCore(dispatch *sidecarEventDispatch, byteCount uint64, capByteCount uint64) *testMailSendCore {
	return &testMailSendCore{
		dispatch:     dispatch,
		domainModes:  map[string]sidecar.SmtpMode{},
		byteCount:    byteCount,
		capByteCount: capByteCount,
		bdatDelay:    30 * time.Second,
		dataDelay:    40 * time.Minute,
		sendOutcomes: map[sidecar.Id][]*sidecar.SmtpDomainOutcome{},
	}
}

// Each recipient's domain, as it first appears among the draft's recipients.
func testMailSendDomains(draft *SidecarDraft) []string {
	domains := []string{}
	for _, recipient := range draft.recipients() {
		_, domain, _ := strings.Cut(recipient, "@")
		domain = strings.ToLower(domain)
		if !slices.Contains(domains, domain) {
			domains = append(domains, domain)
		}
	}
	return domains
}

// Teaches the core domain's mode.
func (self *testMailSendCore) learnMode(domain string, mode sidecar.SmtpMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.domainModes[domain] = mode
}

func (self *testMailSendCore) checkSend(ctx context.Context, draft *SidecarDraft) (*sidecar.SmtpSendCheck, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.checkedDrafts = append(self.checkedDrafts, draft)
	check := &sidecar.SmtpSendCheck{}
	for _, domain := range testMailSendDomains(draft) {
		mode := self.domainModes[domain]
		over := self.capByteCount < self.byteCount
		domainCheck := &sidecar.SmtpDomainCheck{
			Domain:       domain,
			Mode:         mode,
			ByteCount:    self.byteCount,
			CapByteCount: self.capByteCount,
			Prediction:   sidecar.SmtpCheckOk,
		}
		switch mode {
		case sidecar.SmtpModeBdat:
			domainCheck.ExpectedDelay = self.bdatDelay
		case sidecar.SmtpModeData:
			domainCheck.ExpectedDelay = self.dataDelay
			if over {
				domainCheck.Prediction = sidecar.SmtpCheckTooLarge
			}
		default:
			domainCheck.ExpectedDelay = self.bdatDelay
			if over {
				domainCheck.Prediction = sidecar.SmtpCheckUnknown
			}
		}
		check.Domains = append(check.Domains, domainCheck)
	}
	return check, nil
}

// Queues every domain in the outbox, as a phone does, and announces it.
func (self *testMailSendCore) send(ctx context.Context, draft *SidecarDraft) (string, error) {
	sendId := sidecar.NewId()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.sentDrafts = append(self.sentDrafts, draft)
		outcomes := []*sidecar.SmtpDomainOutcome{}
		for _, domain := range testMailSendDomains(draft) {
			outcomes = append(outcomes, &sidecar.SmtpDomainOutcome{Domain: domain, Outcome: sidecar.SmtpOutcomeQueued})
		}
		self.sendOutcomes[sendId] = outcomes
	}()
	self.announce(sendId)
	return sendId.String(), nil
}

// Writes one domain's outcome of a send, as the sending device writes or/, and announces it.
func (self *testMailSendCore) write(sendId sidecar.Id, outcome *sidecar.SmtpDomainOutcome) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		outcomes := self.sendOutcomes[sendId]
		for i, existing := range outcomes {
			if existing.Domain == outcome.Domain {
				outcomes[i] = outcome
			}
		}
	}()
	self.announce(sendId)
}

// mail_send {send_id}, as the core raises it for a changed outcome.
func (self *testMailSendCore) announce(sendId sidecar.Id) {
	self.dispatch.offer(sidecar.SidecarEvent{Kind: sidecar.SidecarEventMailSend, Generation: 1, SendId: &sendId})
}

func (self *testMailSendCore) sendResult(ctx context.Context, sendId sidecar.Id) (*sidecar.SmtpSendResult, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.readSendIds = append(self.readSendIds, sendId)
	outcomes, ok := self.sendOutcomes[sendId]
	if !ok {
		return nil, errors.New("synthetic: no such send")
	}
	result := &sidecar.SmtpSendResult{}
	for _, outcome := range outcomes {
		copied := *outcome
		result.Domains = append(result.Domains, &copied)
	}
	return result, nil
}

// The bound domain named domain, failing when there is none.
func testSendDomain(t *testing.T, domains *SidecarSendDomainList, domain string) *SidecarSendDomain {
	t.Helper()
	if domains == nil {
		t.Fatal("no domains")
	}
	for i := 0; i < domains.Len(); i += 1 {
		if domains.Get(i).Domain == domain {
			return domains.Get(i)
		}
	}
	t.Fatalf("no domain %s", domain)
	return nil
}

// A draft from the network's admin address to one recipient at each of domains.
func testSendDraft(domains ...string) *SidecarDraft {
	draft := &SidecarDraft{From: "admin@net.example", To: NewStringList(), Subject: "s", Body: "b"}
	for _, domain := range domains {
		draft.To.Add("someone@" + domain)
	}
	return draft
}

// FS-123's message: 1 KiB over the prover host's measured smtp_data_proof_bytes, about 1 MiB on the M1 reference.
const (
	testSendCapByteCount = 1024 * 1024
	testSendByteCount    = testSendCapByteCount + 1024
)

// FS-123's check, bound: a message 1 KiB over the cap to one CHUNKING and one non-CHUNKING domain. CheckSend predicts
// ok for the first and unknown for the second while its mode is unknown, then too_large with both byte counts once
// it is known, with copy naming that domain alone; the draft the core checks is the Sidecar's copy.
func TestCheckSendPredictsPerDomain(t *testing.T) {
	core := newTestMailSendCore(nil, testSendByteCount, testSendCapByteCount)
	core.learnMode("chunking.example", sidecar.SmtpModeBdat)
	mail := newTestSidecarMail(t, core)

	draft := testSendDraft("chunking.example", "plain.example")
	unknown := newTestEvsCallback[*SidecarSendCheck]()
	mail.CheckSend(draft, unknown)
	draft.To.Add("late@other.example")
	result, err := unknown.await(t)
	if err != nil || !result.Ok || result.Domains.Len() != 2 {
		t.Fatalf("check %+v, %v", result, err)
	}
	if len(core.checkedDrafts) != 1 || core.checkedDrafts[0].To.Len() != 2 {
		t.Error("the core checked the caller's draft as it changed after CheckSend")
	}
	if first := result.Domains.Get(0); first.Domain != "chunking.example" || first.Mode != SidecarSendModeBdat ||
		first.Prediction != SidecarSendPredictionOk || first.Copy != "" || first.ByteCount != testSendByteCount ||
		first.CapByteCount != testSendCapByteCount || first.ExpectedDelayMillis != 30_000 {
		t.Errorf("the CHUNKING domain checked %+v", first)
	}
	second := result.Domains.Get(1)
	if second.Domain != "plain.example" || second.Mode != SidecarSendModeUnknown || second.Prediction != SidecarSendPredictionUnknown {
		t.Errorf("the domain of unknown mode checked %+v", second)
	}
	if want := "If plain.example's mail server is an older kind, it accepts only up to 1.0 MB, and this message is 1.1 MB."; second.Copy != want {
		t.Errorf("the unknown copy reads %q", second.Copy)
	}

	// a session reaches plain.example's post-TLS EHLO, which offers no CHUNKING
	core.learnMode("plain.example", sidecar.SmtpModeData)
	known := newTestEvsCallback[*SidecarSendCheck]()
	mail.CheckSend(testSendDraft("chunking.example", "plain.example"), known)
	result, err = known.await(t)
	if err != nil || !result.Ok || result.Domains.Len() != 2 {
		t.Fatalf("check %+v, %v", result, err)
	}
	if first := result.Domains.Get(0); first.Prediction != SidecarSendPredictionOk || first.Copy != "" {
		t.Errorf("the CHUNKING domain checked %+v once the other's mode was known", first)
	}
	tooLarge := result.Domains.Get(1)
	if tooLarge.Mode != SidecarSendModeData || tooLarge.Prediction != SidecarSendPredictionTooLarge ||
		tooLarge.ByteCount != testSendByteCount || tooLarge.CapByteCount != testSendCapByteCount {
		t.Errorf("the non-CHUNKING domain checked %+v", tooLarge)
	}
	want := "plain.example accepts messages only up to 1.0 MB from your devices, and this one is 1.1 MB. Send plain.example a smaller message; your other recipients aren't affected."
	if tooLarge.Copy != want {
		t.Errorf("the too_large copy reads %q", tooLarge.Copy)
	}
	if strings.Contains(tooLarge.Copy, "chunking.example") {
		t.Error("the too_large copy names another domain")
	}

	// under the cap, the data mode's line gives the delay, and BDAT's none
	core.byteCount = testSendCapByteCount / 2
	under := newTestEvsCallback[*SidecarSendCheck]()
	mail.CheckSend(testSendDraft("chunking.example", "plain.example"), under)
	result, err = under.await(t)
	if err != nil || !result.Ok {
		t.Fatalf("check %+v, %v", result, err)
	}
	if data := testSendDomain(t, result.Domains, "plain.example"); data.Prediction != SidecarSendPredictionOk || data.Copy != "Sending to plain.example takes about 40 minutes." {
		t.Errorf("the data mode under the cap checked %+v", data)
	}
	if bdat := testSendDomain(t, result.Domains, "chunking.example"); bdat.Copy != "" {
		t.Errorf("the BDAT domain has copy %q", bdat.Copy)
	}
}

// A listener that hands each mail_send's send_id to the test.
type testMailSendListener struct {
	sendIds chan string
}

func (self *testMailSendListener) SidecarEventReceived(event *SidecarEvent) {
	if event.Kind == string(sidecar.SidecarEventMailSend) && event.SendId != nil {
		self.sendIds <- event.SendId.String()
	}
}

// Waits for the next mail_send, failing unless it names sendId.
func (self *testMailSendListener) await(t *testing.T, sendId string) {
	t.Helper()
	select {
	case got := <-self.sendIds:
		if got != sendId {
			t.Fatalf("mail_send names %s, not %s", got, sendId)
		}
	case <-time.After(testSidecarTimeout):
		t.Fatal("no mail_send")
	}
}

// FS-123's results, bound, from a phone's outbox: Send answers the send_id, every domain queued; each outcome a prover
// host writes raises mail_send naming it, and GetSendResult then reads it: a deferral by a host under the cap, then
// the send by a second whose cap fits, the MX's refusal with its code, a TL4 refusal, and too_large with both byte
// counts, each domain independently and each with its copy.
func TestSendResultsReachGetSendResultAndMailSend(t *testing.T) {
	dispatch := newSidecarEventDispatch(t.Context(), defaultSidecarEventSettings())
	defer dispatch.close()
	listener := &testMailSendListener{sendIds: make(chan string, 16)}
	dispatch.addListener(listener)
	core := newTestMailSendCore(dispatch, testSendByteCount, testSendCapByteCount)
	mail := newTestSidecarMail(t, core)

	send := newTestEvsCallback[*SidecarResult]()
	mail.Send(testSendDraft("chunking.example", "plain.example", "refuse.example", "tl4.example", "big.example"), send)
	sent, err := send.await(t)
	if err != nil || !sent.Ok {
		t.Fatalf("send %+v, %v", sent, err)
	}
	sendId, err := ParseId(sent.Value)
	if err != nil {
		t.Fatalf("Send's value %q is no send_id: %v", sent.Value, err)
	}
	listener.await(t, sendId.String())

	read := func() *SidecarSendResult {
		t.Helper()
		callback := newTestEvsCallback[*SidecarSendResult]()
		mail.GetSendResult(sendId, callback)
		result, err := callback.await(t)
		if err != nil || !result.Ok || result.SendId == nil || result.SendId.String() != sendId.String() {
			t.Fatalf("result %+v, %v", result, err)
		}
		return result
	}
	queued := read()
	if queued.Domains.Len() != 5 {
		t.Fatalf("%d domains", queued.Domains.Len())
	}
	for i := 0; i < queued.Domains.Len(); i += 1 {
		if domain := queued.Domains.Get(i); domain.Outcome != SidecarSendOutcomeQueued || domain.Copy != "Waiting for one of your computers to send it." {
			t.Errorf("a queued domain reads %+v", domain)
		}
	}

	written := time.UnixMilli(1_790_000_000_000)
	coreId := sidecar.Id(sendId.id)
	steps := []*sidecar.SmtpDomainOutcome{
		{Domain: "chunking.example", Mode: sidecar.SmtpModeBdat, Outcome: sidecar.SmtpOutcomeSent, TlsLevel: sidecar.SmtpTlsLevelDane, UpdateTime: written},
		// the first host's cap is under the bytes, so it leaves the message to one whose recorded cap fits
		{Domain: "plain.example", Mode: sidecar.SmtpModeData, Outcome: sidecar.SmtpOutcomeDeferred, RetryAfter: 10 * time.Minute, UpdateTime: written},
		{Domain: "refuse.example", Mode: sidecar.SmtpModeBdat, Outcome: sidecar.SmtpOutcomeRefused, ReplyCode: 550, TlsLevel: sidecar.SmtpTlsLevelMtaSts, UpdateTime: written},
		{Domain: "tl4.example", Outcome: sidecar.SmtpOutcomeTlsRefused, TlsLevel: sidecar.SmtpTlsLevelUnauthenticated, UpdateTime: written},
		{Domain: "big.example", Mode: sidecar.SmtpModeData, Outcome: sidecar.SmtpOutcomeTooLarge, ByteCount: testSendByteCount, CapByteCount: testSendCapByteCount, UpdateTime: written},
	}
	for _, step := range steps {
		core.write(coreId, step)
		listener.await(t, sendId.String())
	}
	result := read()
	if domain := testSendDomain(t, result.Domains, "chunking.example"); domain.Outcome != SidecarSendOutcomeSent || domain.Copy != "" ||
		domain.TlsLevel != 1 || domain.TlsLabel != "" || domain.UpdateMillis != written.UnixMilli() {
		t.Errorf("the sent domain reads %+v", domain)
	}
	deferred := testSendDomain(t, result.Domains, "plain.example")
	if deferred.Outcome != SidecarSendOutcomeDeferred || deferred.RetryAfterMillis != (10*time.Minute).Milliseconds() ||
		deferred.RetryAtMillis != written.Add(10*time.Minute).UnixMilli() || !strings.HasPrefix(deferred.Copy, "Sending again at ") {
		t.Errorf("the deferred domain reads %+v", deferred)
	}
	if domain := testSendDomain(t, result.Domains, "refuse.example"); domain.Outcome != SidecarSendOutcomeRefused || domain.ReplyCode != 550 ||
		domain.Copy != "refuse.example refused it (550)." || domain.TlsLabel != "" {
		t.Errorf("the refused domain reads %+v", domain)
	}
	if domain := testSendDomain(t, result.Domains, "tl4.example"); domain.Outcome != SidecarSendOutcomeTlsRefused || domain.TlsLevel != 4 ||
		domain.TlsLabel != "" || !strings.HasPrefix(domain.Copy, "Didn't send to tl4.example: ") {
		t.Errorf("the TLS-refused domain reads %+v", domain)
	}
	tooLarge := testSendDomain(t, result.Domains, "big.example")
	if tooLarge.Outcome != SidecarSendOutcomeTooLarge || tooLarge.ByteCount != testSendByteCount || tooLarge.CapByteCount != testSendCapByteCount ||
		!strings.HasPrefix(tooLarge.Copy, "big.example accepts messages only up to 1.0 MB") {
		t.Errorf("the too_large domain reads %+v", tooLarge)
	}

	// a second prover host whose recorded cap fits takes the message
	core.write(coreId, &sidecar.SmtpDomainOutcome{Domain: "plain.example", Mode: sidecar.SmtpModeData, Outcome: sidecar.SmtpOutcomeSent, TlsLevel: sidecar.SmtpTlsLevelIndependentMx, UpdateTime: written.Add(11 * time.Minute)})
	listener.await(t, sendId.String())
	if domain := testSendDomain(t, read().Domains, "plain.example"); domain.Outcome != SidecarSendOutcomeSent || domain.RetryAtMillis != 0 || domain.Copy != "" {
		t.Errorf("the domain the second host sent reads %+v", domain)
	}
	for _, read := range core.readSendIds {
		if read != coreId {
			t.Errorf("GetSendResult read send %s", read)
		}
	}
}

// Every copy line of §18.5's Send capability, each case's numbers as FS-123 has them, read exactly.
func TestSendCopyText(t *testing.T) {
	now := time.Date(2026, time.November, 15, 18, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		domain *SidecarSendDomain
		check  bool
		want   string
	}{
		{check: true, domain: &SidecarSendDomain{Domain: "plain.example", Mode: SidecarSendModeData, Prediction: SidecarSendPredictionTooLarge, ByteCount: testSendByteCount, CapByteCount: testSendCapByteCount},
			want: "plain.example accepts messages only up to 1.0 MB from your devices, and this one is 1.1 MB. Send plain.example a smaller message; your other recipients aren't affected."},
		{check: true, domain: &SidecarSendDomain{Domain: "plain.example", Mode: SidecarSendModeUnknown, Prediction: SidecarSendPredictionUnknown, ByteCount: 700_001, CapByteCount: 614_400},
			want: "If plain.example's mail server is an older kind, it accepts only up to 614 KB, and this message is 701 KB."},
		{check: true, domain: &SidecarSendDomain{Domain: "plain.example", Mode: SidecarSendModeData, Prediction: SidecarSendPredictionOk, ExpectedDelayMillis: (2 * time.Hour).Milliseconds()},
			want: "Sending to plain.example takes about 2 hours."},
		// no copy: BDAT, an unknown mode under the cap, a data mode with no delay, and a too_large with no cap
		{check: true, domain: &SidecarSendDomain{Domain: "plain.example", Mode: SidecarSendModeBdat, Prediction: SidecarSendPredictionOk, ExpectedDelayMillis: 30_000}},
		{check: true, domain: &SidecarSendDomain{Domain: "plain.example", Mode: SidecarSendModeUnknown, Prediction: SidecarSendPredictionUnknown, ByteCount: 10, CapByteCount: 20}},
		{check: true, domain: &SidecarSendDomain{Domain: "plain.example", Mode: SidecarSendModeData, Prediction: SidecarSendPredictionOk}},
		{check: true, domain: &SidecarSendDomain{Domain: "plain.example", Mode: SidecarSendModeData, Prediction: SidecarSendPredictionTooLarge, ByteCount: 10}},
		{domain: &SidecarSendDomain{Domain: "plain.example", Outcome: SidecarSendOutcomeSent}},
		{domain: &SidecarSendDomain{Domain: "plain.example", Outcome: SidecarSendOutcomeQueued},
			want: "Waiting for one of your computers to send it."},
		{domain: &SidecarSendDomain{Domain: "plain.example", Outcome: SidecarSendOutcomeDeferred, RetryAtMillis: now.Add(30 * time.Minute).UnixMilli()},
			want: "Sending again at 18:30 UTC."},
		{domain: &SidecarSendDomain{Domain: "plain.example", Outcome: SidecarSendOutcomeDeferred, RetryAtMillis: now.Add(7 * time.Hour).UnixMilli()},
			want: "Sending again at Nov 16 01:00 UTC."},
		{domain: &SidecarSendDomain{Domain: "plain.example", Outcome: SidecarSendOutcomeDeferred, RetryAtMillis: now.AddDate(0, 2, 0).UnixMilli()},
			want: "Sending again at Jan 15 2027 18:00 UTC."},
		{domain: &SidecarSendDomain{Domain: "plain.example", Outcome: SidecarSendOutcomeTooLarge, ByteCount: testSendByteCount, CapByteCount: testSendCapByteCount},
			want: "plain.example accepts messages only up to 1.0 MB from your devices, and this one is 1.1 MB. Send plain.example a smaller message; your other recipients aren't affected."},
		{domain: &SidecarSendDomain{Domain: "plain.example", Outcome: SidecarSendOutcomeRefused, ReplyCode: 554},
			want: "plain.example refused it (554)."},
		{domain: &SidecarSendDomain{Domain: "plain.example", Outcome: SidecarSendOutcomeRefused},
			want: "plain.example refused it."},
		{domain: &SidecarSendDomain{Domain: "plain.example", Outcome: SidecarSendOutcomeTlsRefused},
			want: "Didn't send to plain.example: its mail server can't be authenticated, so an active URnetwork edge could read the message."},
		{domain: &SidecarSendDomain{Domain: "plain.example", Outcome: SidecarSendOutcomeFailed},
			want: "Couldn't send to plain.example."},
		{domain: &SidecarSendDomain{Domain: "plain.example", Outcome: SidecarSendOutcomeUnknown}},
	} {
		got := ""
		if testCase.check {
			got = sidecarSendCheckCopy(testCase.domain)
		} else {
			got = sidecarSendResultCopy(testCase.domain, now)
		}
		if got != testCase.want {
			t.Errorf("%+v reads %q, not %q", testCase.domain, got, testCase.want)
		}
	}
}

// Every send copy line, compose and results, names no indexer, receiver or coverage setting, which are inbound
// (§18.5's never row, §14.5's copy rule, FS-123 as QD-277 reads it): of the results only too_large cites the cap, each
// line that names a domain names that one alone, and GetReceiverCoverage's copy says nothing of sending.
func TestSendCopyNamesNoIndexer(t *testing.T) {
	now := time.Date(2026, time.November, 15, 18, 0, 0, 0, time.UTC)
	check := &sidecar.SmtpSendCheck{}
	for _, mode := range []sidecar.SmtpMode{sidecar.SmtpModeUnknown, sidecar.SmtpModeBdat, sidecar.SmtpModeData, 9} {
		for _, prediction := range []sidecar.SmtpCheckPrediction{sidecar.SmtpCheckUnspecified, sidecar.SmtpCheckOk, sidecar.SmtpCheckTooLarge, sidecar.SmtpCheckUnknown, 9} {
			for _, byteCount := range []uint64{testSendCapByteCount / 2, testSendByteCount} {
				check.Domains = append(check.Domains, &sidecar.SmtpDomainCheck{
					Domain: "plain.example", Mode: mode, ByteCount: byteCount, CapByteCount: testSendCapByteCount,
					ExpectedDelay: 40 * time.Minute, Prediction: prediction,
				})
			}
		}
	}
	result := &sidecar.SmtpSendResult{}
	for kind := sidecar.SmtpOutcomeUnspecified; kind <= sidecar.SmtpOutcomeFailed+1; kind += 1 {
		for _, level := range []sidecar.SmtpTlsLevel{sidecar.SmtpTlsLevelUnreached, sidecar.SmtpTlsLevelDane, sidecar.SmtpTlsLevelUnauthenticated} {
			result.Domains = append(result.Domains, &sidecar.SmtpDomainOutcome{
				Domain: "plain.example", Mode: sidecar.SmtpModeData, ByteCount: testSendByteCount, CapByteCount: testSendCapByteCount,
				Outcome: kind, ReplyCode: 550, RetryAfter: 10 * time.Minute, UpdateTime: now, TlsLevel: level,
			})
		}
	}
	_, capCopy := sidecarSendSizesCopy(testSendByteCount, testSendCapByteCount)
	inbound := []string{"index", "receiv", "coverage", "always-on", "inbound"}
	scan := func(origin string, copy string) {
		lowered := strings.ToLower(copy)
		for _, word := range inbound {
			if strings.Contains(lowered, word) {
				t.Errorf("%s's copy names %q: %q", origin, word, copy)
			}
		}
		if strings.Contains(strings.ReplaceAll(copy, "plain.example", ""), "example") {
			t.Errorf("%s's copy names another domain: %q", origin, copy)
		}
	}
	lines := 0
	checked := sidecarSendCheckDomainsOf(check)
	for i := 0; i < checked.Len(); i += 1 {
		domain := checked.Get(i)
		scan("a "+domain.Prediction+" check", domain.Copy)
		if domain.Copy != "" {
			lines += 1
		}
	}
	results := sidecarSendResultDomainsOf(result, now)
	for i := 0; i < results.Len(); i += 1 {
		domain := results.Get(i)
		scan("a "+domain.Outcome+" result", domain.Copy)
		if strings.Contains(domain.Copy, capCopy) != (domain.Outcome == SidecarSendOutcomeTooLarge) {
			t.Errorf("a %s result's copy cites the cap %v: %q", domain.Outcome, strings.Contains(domain.Copy, capCopy), domain.Copy)
		}
		if domain.Copy != "" {
			lines += 1
		}
	}
	if lines == 0 {
		t.Fatal("no copy was made to scan")
	}
	// GetReceiverCoverage's copy is inbound, and says nothing of sending or its limits
	lowered := strings.ToLower(sidecar.MailIndexerPrompt)
	for _, word := range []string{"send", "sent", "cap", "size", "large", " mb", " kb", "byte"} {
		if strings.Contains(lowered, word) {
			t.Errorf("the coverage copy names %q: %q", word, sidecar.MailIndexerPrompt)
		}
	}
}

// The byte count a size line reads as, from its own words.
func testSendParseByteCopy(t *testing.T, copy string) int64 {
	t.Helper()
	number, unit, ok := strings.Cut(copy, " ")
	if !ok {
		t.Fatalf("%q has no unit", copy)
	}
	multiplier := map[string]int64{"byte": 1, "bytes": 1, "KB": 1000, "MB": 1000 * 1000, "GB": 1000 * 1000 * 1000}[unit]
	if multiplier == 0 {
		t.Fatalf("%q has an unknown unit", copy)
	}
	whole, tenth, decimal := strings.Cut(number, ".")
	value, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		t.Fatalf("%q: %v", copy, err)
	}
	byteCount := value * multiplier
	if decimal {
		tenths, err := strconv.ParseInt(tenth, 10, 64)
		if err != nil || len(tenth) != 1 {
			t.Fatalf("%q: %v", copy, err)
		}
		byteCount += tenths * multiplier / 10
	}
	return byteCount
}

// A size over the cap never reads as equal to it or under it: the cap reads at or under its bytes, and the size at or
// over its own, at every unit boundary, FS-123's 1 KiB over about 1 MiB and a byte over among them.
func TestSendSizesNeverReadUnderTheCap(t *testing.T) {
	caps := []int64{1, 2, 999, 1000, 1001, 1999, 614_400, 999_499, 999_500, 999_999, 1000 * 1000, testSendCapByteCount, 999_999_999, 1000 * 1000 * 1000, 5_123_456_789}
	overs := []int64{1, 2, 99, 999, 1000, 1024, 99_999, 100_000, 1000 * 1000}
	for _, capByteCount := range caps {
		for _, over := range overs {
			byteCount := capByteCount + over
			size, cap := sidecarSendSizesCopy(byteCount, capByteCount)
			shownSize, shownCap := testSendParseByteCopy(t, size), testSendParseByteCopy(t, cap)
			if shownSize <= shownCap {
				t.Errorf("%d bytes over a cap of %d read %q, not over %q", byteCount, capByteCount, size, cap)
			}
			if capByteCount < shownCap || shownSize < byteCount {
				t.Errorf("%d over %d read %q and %q: the cap rounded up or the size down", byteCount, capByteCount, size, cap)
			}
		}
	}
	for byteCount, want := range map[int64]string{0: "0 bytes", 1: "1 byte", 999: "999 bytes", 1000: "1 KB", 999_999: "999 KB", 1000 * 1000: "1.0 MB", 1_234_567: "1.2 MB"} {
		if got := sidecarSendByteCopy(byteCount, false); got != want {
			t.Errorf("%d rounded down reads %q, not %q", byteCount, got, want)
		}
	}
	for byteCount, want := range map[int64]string{999: "999 bytes", 1001: "2 KB", 999_001: "1.0 MB", 1_234_567: "1.3 MB"} {
		if got := sidecarSendByteCopy(byteCount, true); got != want {
			t.Errorf("%d rounded up reads %q, not %q", byteCount, got, want)
		}
	}
}

// A delay reads in minutes, rounded up, and past an hour in hours and tens of minutes.
func TestSendDelayCopy(t *testing.T) {
	for delay, want := range map[time.Duration]string{
		0:                                "a minute",
		30 * time.Second:                 "a minute",
		time.Minute:                      "a minute",
		time.Minute + time.Second:        "2 minutes",
		59 * time.Minute:                 "59 minutes",
		time.Hour:                        "an hour",
		time.Hour + time.Minute:          "1 hour 10 minutes",
		time.Hour + 45*time.Minute:       "1 hour 50 minutes",
		time.Hour + 51*time.Minute:       "2 hours",
		2 * time.Hour:                    "2 hours",
		2*time.Hour + 20*time.Minute:     "2 hours 20 minutes",
		time.Duration(1<<63 - 1):         fmt.Sprintf("%d hours 50 minutes", int64(time.Duration(1<<63-1)/time.Hour)),
		time.Duration(1<<63-1) - 1000000: fmt.Sprintf("%d hours 50 minutes", int64(time.Duration(1<<63-1)/time.Hour)),
	} {
		if got := sidecarSendDelayCopy(delay); got != want {
			t.Errorf("%v reads %q, not %q", delay, got, want)
		}
	}
}

// I-11: only mail sent at TL4 carries EX4's label, TL1 to TL3 none, a refusal at TL4 none since nothing was sent, and a
// level this sdk does not know, or none, the label, which shows no authentication.
func TestSendTlsLabelOnlyOnSentAtTl4(t *testing.T) {
	now := time.Date(2026, time.November, 15, 18, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		kind  sidecar.SmtpOutcomeKind
		level sidecar.SmtpTlsLevel
		label string
	}{
		{kind: sidecar.SmtpOutcomeSent, level: sidecar.SmtpTlsLevelDane},
		{kind: sidecar.SmtpOutcomeSent, level: sidecar.SmtpTlsLevelMtaSts},
		{kind: sidecar.SmtpOutcomeSent, level: sidecar.SmtpTlsLevelIndependentMx},
		{kind: sidecar.SmtpOutcomeSent, level: sidecar.SmtpTlsLevelUnauthenticated, label: sidecar.SmtpTlsLabelUnauthenticated},
		{kind: sidecar.SmtpOutcomeSent, level: 7, label: sidecar.SmtpTlsLabelUnauthenticated},
		// sent with no level reached shows no authentication either
		{kind: sidecar.SmtpOutcomeSent, level: sidecar.SmtpTlsLevelUnreached, label: sidecar.SmtpTlsLabelUnauthenticated},
		{kind: sidecar.SmtpOutcomeTlsRefused, level: sidecar.SmtpTlsLevelUnauthenticated},
		{kind: sidecar.SmtpOutcomeRefused, level: sidecar.SmtpTlsLevelUnauthenticated},
	} {
		result := &sidecar.SmtpSendResult{Domains: []*sidecar.SmtpDomainOutcome{{Domain: "plain.example", Outcome: testCase.kind, TlsLevel: testCase.level}}}
		domain := sidecarSendResultDomainsOf(result, now).Get(0)
		if domain.TlsLabel != testCase.label || domain.TlsLevel != int32(testCase.level) {
			t.Errorf("outcome %d at level %d labels %q, level %d", testCase.kind, testCase.level, domain.TlsLabel, domain.TlsLevel)
		}
	}
}

// What the sdk cannot read claims nothing: an unknown or unspecified mode reads unknown, a prediction it does not know
// or the unspecified one unknown rather than ok, an outcome kind it does not know unknown with no copy, byte counts
// past an int64 the largest, and a deferral with no write time counts from the read. Nil entries are skipped.
func TestSendMappingsClaimNothingUnread(t *testing.T) {
	now := time.Date(2026, time.November, 15, 18, 0, 0, 0, time.UTC)
	checked := sidecarSendCheckDomainsOf(&sidecar.SmtpSendCheck{Domains: []*sidecar.SmtpDomainCheck{
		nil,
		{Domain: "a.example", Mode: 9, Prediction: sidecar.SmtpCheckUnspecified, ByteCount: 1 << 63, CapByteCount: 1<<64 - 1, ExpectedDelay: -time.Second},
		{Domain: "b.example", Mode: sidecar.SmtpModeData, Prediction: 9},
	}})
	if checked.Len() != 2 {
		t.Fatalf("%d checked domains", checked.Len())
	}
	if first := checked.Get(0); first.Mode != SidecarSendModeUnknown || first.Prediction != SidecarSendPredictionUnknown ||
		first.ByteCount != 1<<63-1 || first.CapByteCount != 1<<63-1 || first.ExpectedDelayMillis != 0 {
		t.Errorf("the first unread check maps as %+v", first)
	}
	if second := checked.Get(1); second.Prediction != SidecarSendPredictionUnknown {
		t.Errorf("an unknown prediction maps as %+v", second)
	}
	results := sidecarSendResultDomainsOf(&sidecar.SmtpSendResult{Domains: []*sidecar.SmtpDomainOutcome{
		nil,
		{Domain: "a.example", Outcome: sidecar.SmtpOutcomeUnspecified, ReplyCode: 550},
		{Domain: "b.example", Outcome: 99, Mode: 9},
		{Domain: "c.example", Outcome: sidecar.SmtpOutcomeDeferred, RetryAfter: time.Minute},
		{Domain: "d.example", Outcome: sidecar.SmtpOutcomeRefused, ReplyCode: 1<<32 - 1},
	}}, now)
	if results.Len() != 4 {
		t.Fatalf("%d result domains", results.Len())
	}
	for i := 0; i < 2; i += 1 {
		if domain := results.Get(i); domain.Outcome != SidecarSendOutcomeUnknown || domain.Copy != "" || domain.ReplyCode != 0 || domain.Mode != SidecarSendModeUnknown {
			t.Errorf("an unread outcome maps as %+v", domain)
		}
	}
	if deferred := results.Get(2); deferred.RetryAtMillis != now.Add(time.Minute).UnixMilli() || deferred.Copy != "Sending again at 18:01 UTC." {
		t.Errorf("a deferral with no write time maps as %+v", deferred)
	}
	if refused := results.Get(3); refused.ReplyCode != 1<<31-1 {
		t.Errorf("a reply code past an int32 maps as %d", refused.ReplyCode)
	}
}

// CheckSend and GetSendResult answer MAIL_UNAVAILABLE on a local backend with no core mail to work on, and on the
// remote one over a daemon from before mail.check_send and mail.send_result, naming the call it has none of; a closed
// SidecarMail answers the same and reaches no backend.
func TestSendCallsWithNothingToCallAnswerUnavailable(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	for name, test := range map[string]struct {
		mail *SidecarMail
		// what each refusal names
		check  string
		result string
	}{
		"local":  {mail: newTestSidecarMail(t, &sidecarMailCoreBackend{client: nil}), check: sidecar.ErrMailUnbound.Error(), result: sidecar.ErrMailUnbound.Error()},
		"remote": {mail: remote.GetMail(), check: "check a send", result: "result"},
	} {
		check := newTestEvsCallback[*SidecarSendCheck]()
		test.mail.CheckSend(testSendDraft("plain.example"), check)
		if result, err := check.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeUnavailable || !strings.Contains(result.Message, test.check) {
			t.Errorf("%s CheckSend answered %+v, %v", name, result, err)
		}
		read := newTestEvsCallback[*SidecarSendResult]()
		test.mail.GetSendResult(NewId(), read)
		if result, err := read.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeUnavailable || !strings.Contains(result.Message, test.result) {
			t.Errorf("%s GetSendResult answered %+v, %v", name, result, err)
		}
	}

	core := newTestMailSendCore(nil, testSendByteCount, testSendCapByteCount)
	closed := newTestSidecarMail(t, core)
	closed.close()
	check := newTestEvsCallback[*SidecarSendCheck]()
	closed.CheckSend(testSendDraft("plain.example"), check)
	if result, _ := check.await(t); result.Ok || result.Code != sidecarMailCodeUnavailable {
		t.Errorf("a closed mail checked %+v", result)
	}
	read := newTestEvsCallback[*SidecarSendResult]()
	closed.GetSendResult(NewId(), read)
	if result, _ := read.await(t); result.Ok || result.Code != sidecarMailCodeUnavailable {
		t.Errorf("a closed mail read %+v", result)
	}
	if len(core.checkedDrafts) != 0 || len(core.readSendIds) != 0 {
		t.Error("a closed mail reached its backend")
	}
}

// The local backend calls the core's MailClient (QD-1122): a core's mail that binds nothing yet answers its own refusal
// for both calls, MAIL_UNAVAILABLE naming nothing to work on, so the calls reach MailClient.CheckSend and SendResult
// and stop at nothing of this module's own.
func TestSendCoreBackendCallsTheCoresMail(t *testing.T) {
	client, err := sidecar.NewMailClient(&sidecar.MailClientArgs{NetworkId: sidecar.NewId(), SidecarId: sidecar.NewId(), Store: testEmptySidecarStore{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	mail := newTestSidecarMail(t, &sidecarMailCoreBackend{client: client})
	check := newTestEvsCallback[*SidecarSendCheck]()
	mail.CheckSend(testSendDraft("plain.example"), check)
	if result, err := check.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeUnavailable ||
		!strings.Contains(result.Message, sidecar.ErrMailUnbound.Error()) || strings.Contains(result.Message, "no core mail") {
		t.Errorf("CheckSend answered %+v, %v", result, err)
	}
	read := newTestEvsCallback[*SidecarSendResult]()
	mail.GetSendResult(NewId(), read)
	if result, err := read.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeUnavailable ||
		!strings.Contains(result.Message, sidecar.ErrMailUnbound.Error()) || strings.Contains(result.Message, "no core mail") {
		t.Errorf("GetSendResult answered %+v, %v", result, err)
	}
}

// A draft reaches the core as the draft Send takes: its From, its three lists in order, its subject, and its body as
// the text; a list the app left nil is none.
func TestSendCoreBackendTakesTheDraftWhole(t *testing.T) {
	draft := &SidecarDraft{From: "ann@acme.root.example", To: NewStringList(), Bcc: NewStringList(), Subject: "Plans", Body: "Dinner at eight."}
	draft.To.Add("a@chunking.example")
	draft.To.Add("b@plain.example")
	draft.Bcc.Add("c@hidden.example")
	args := sidecarMailSendArgsOf(draft)
	if args.From != draft.From || !slices.Equal(args.To, []string{"a@chunking.example", "b@plain.example"}) || args.Cc != nil ||
		!slices.Equal(args.Bcc, []string{"c@hidden.example"}) || args.Subject != "Plans" || args.Text != "Dinner at eight." || args.Action.RunId != nil {
		t.Errorf("the core takes %+v", args)
	}
}

// A check that names nothing it can act on, or a result with no send_id, answers MAIL_INVALID and reaches no backend;
// a core that answers nothing and no error is a refusal, never an empty Ok.
func TestSendCallsRefuseWhatTheyCannotRead(t *testing.T) {
	core := newTestMailSendCore(nil, testSendByteCount, testSendCapByteCount)
	mail := newTestSidecarMail(t, core)
	for _, draft := range []*SidecarDraft{nil, {From: "admin@net.example"}, {To: testSendDraft("plain.example").To}} {
		callback := newTestEvsCallback[*SidecarSendCheck]()
		mail.CheckSend(draft, callback)
		if result, _ := callback.await(t); result.Ok || result.Code != sidecarMailCodeInvalid {
			t.Errorf("draft %+v checked %+v", draft, result)
		}
	}
	noSend := newTestEvsCallback[*SidecarSendResult]()
	mail.GetSendResult(nil, noSend)
	if result, _ := noSend.await(t); result.Ok || result.Code != sidecarMailCodeInvalid {
		t.Errorf("no send_id read %+v", result)
	}
	if len(core.checkedDrafts) != 0 || len(core.readSendIds) != 0 {
		t.Error("an invalid call reached the backend")
	}

	unknownSend := newTestEvsCallback[*SidecarSendResult]()
	mail.GetSendResult(NewId(), unknownSend)
	if result, _ := unknownSend.await(t); result.Ok || result.Code != sidecarMailCodeRefused || result.Domains != nil {
		t.Errorf("a send the core does not know read %+v", result)
	}
	empty := newTestSidecarMail(t, &testMailSendEmpty{})
	emptyCheck := newTestEvsCallback[*SidecarSendCheck]()
	empty.CheckSend(testSendDraft("plain.example"), emptyCheck)
	if result, _ := emptyCheck.await(t); result.Ok || result.Code == "" {
		t.Errorf("an empty check answered %+v", result)
	}
	emptyResult := newTestEvsCallback[*SidecarSendResult]()
	empty.GetSendResult(NewId(), emptyResult)
	if result, _ := emptyResult.await(t); result.Ok || result.Code == "" {
		t.Errorf("an empty result answered %+v", result)
	}
}

// A core that answers neither a check nor a result, and no error.
type testMailSendEmpty struct {
	testMailBackend
}

func (self *testMailSendEmpty) checkSend(ctx context.Context, draft *SidecarDraft) (*sidecar.SmtpSendCheck, error) {
	return nil, nil
}

func (self *testMailSendEmpty) sendResult(ctx context.Context, sendId sidecar.Id) (*sidecar.SmtpSendResult, error) {
	return nil, nil
}

// The answers cross as json with §18.5's snake_case names, their lists as arrays, an empty list as [] and never
// null, and an answer reads back as it was.
func TestSendAnswersCrossAsJson(t *testing.T) {
	domains := NewSidecarSendDomainList()
	domains.Add(&SidecarSendDomain{Domain: "plain.example", Mode: SidecarSendModeData, ByteCount: testSendByteCount, CapByteCount: testSendCapByteCount,
		Prediction: SidecarSendPredictionTooLarge, ExpectedDelayMillis: 1, Outcome: SidecarSendOutcomeDeferred, RetryAfterMillis: 2, RetryAtMillis: 3,
		ReplyCode: 550, TlsLevel: 4, TlsLabel: sidecar.SmtpTlsLabelUnauthenticated, UpdateMillis: 5, Copy: "c"})
	result := &SidecarSendResult{Ok: true, SendId: NewId(), Domains: domains}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["domains"].([]any); !ok || fields["send_id"] == nil {
		t.Errorf("the result's json is %s", encoded)
	}
	domain := fields["domains"].([]any)[0].(map[string]any)
	for _, name := range []string{"domain", "mode", "byte_count", "cap_byte_count", "prediction", "expected_delay_millis", "outcome",
		"retry_after_millis", "retry_at_millis", "reply_code", "tls_level", "tls_label", "update_millis", "copy"} {
		if _, ok := domain[name]; !ok {
			t.Errorf("a domain's json lacks %s: %s", name, encoded)
		}
	}
	decoded := &SidecarSendResult{}
	if err := json.Unmarshal(encoded, decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Domains.Len() != 1 || *decoded.Domains.Get(0) != *domains.Get(0) || decoded.SendId.String() != result.SendId.String() {
		t.Errorf("the result read back as %+v", decoded)
	}
	emptyList, err := json.Marshal(NewSidecarSendDomainList())
	if err != nil || string(emptyList) != "[]" {
		t.Errorf("an empty domain list is %s, %v", emptyList, err)
	}
	check, err := json.Marshal(&SidecarSendCheck{Code: sidecarMailCodeUnavailable, Message: "m"})
	if err != nil || string(check) != `{"ok":false,"code":"MAIL_UNAVAILABLE","message":"m"}` {
		t.Errorf("a refused check is %s, %v", check, err)
	}
}

// The method names this sdk sends are the daemon's own, and each params and reply crosses the daemon's type and back
// strictly, losing nothing; a draft crosses with its body as the daemon's text, and the daemon's replies read back as
// the core's values, delays and times from milliseconds and each enum by its number.
func TestMailSendControlWireMatchesTheDaemon(t *testing.T) {
	for own, daemons := range map[string]string{
		sidecarControlMethodMailCheckSend:  sidecar.DaemonMethodMailCheckSend,
		sidecarControlMethodMailSendResult: sidecar.DaemonMethodMailSendResult,
		sidecarControlMethodMailSend:       sidecar.DaemonMethodMailSend,
	} {
		if own != daemons {
			t.Errorf("%q is the daemon's %q", own, daemons)
		}
	}
	sendId := sidecar.NewId()
	approvalId := sidecar.NewId()
	crossings := []struct {
		name    string
		own     any
		daemons any
		back    any
	}{
		{
			name: "draft params",
			own: &sidecarControlMailDraftParams{Network: "a-network", From: "ann@acme.root.example", To: []string{"a@x.example"},
				Cc: []string{"b@y.example"}, Bcc: []string{"c@z.example"}, Subject: "s", Text: "t"},
			daemons: &sidecar.DaemonMailDraftParams{},
			back:    &sidecarControlMailDraftParams{},
		},
		{
			name:    "result params",
			own:     &sidecarControlMailSendResultParams{Network: "a-network", SendId: &sendId},
			daemons: &sidecar.DaemonMailSendResultParams{},
			back:    &sidecarControlMailSendResultParams{},
		},
		{
			name: "check reply",
			own: &sidecarControlMailSendCheckReply{Domains: []*sidecarControlMailDomainCheck{
				{Domain: "x.example", Mode: 2, ByteCount: 3, CapByteCount: 4, ExpectedDelayMillis: 5, Prediction: 2},
			}},
			daemons: &sidecar.DaemonMailSendCheckReply{},
			back:    &sidecarControlMailSendCheckReply{},
		},
		{
			name: "result reply",
			own: &sidecarControlMailSendResultReply{Domains: []*sidecarControlMailDomainOutcome{
				{Domain: "x.example", Mode: 1, ByteCount: 2, CapByteCount: 3, Outcome: 4, ReplyCode: 550, RetryAfterMillis: 6, UpdateTimeMillis: 7, TlsLevel: 4},
			}},
			daemons: &sidecar.DaemonMailSendResultReply{},
			back:    &sidecarControlMailSendResultReply{},
		},
		{
			name: "send reply",
			own: &sidecarControlMailSendReply{SendId: &sendId, SentCopy: true, Recipients: []*sidecarControlMailSendRecipient{
				{Address: "a@x.example", Outcome: "internet_waits"},
			}},
			daemons: &sidecar.DaemonMailSendReply{},
			back:    &sidecarControlMailSendReply{},
		},
		{
			name:    "held send reply",
			own:     &sidecarControlMailSendReply{ApprovalId: &approvalId},
			daemons: &sidecar.DaemonMailSendReply{},
			back:    &sidecarControlMailSendReply{},
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

	draft := &SidecarDraft{From: "ann@acme.root.example", To: NewStringList(), Cc: NewStringList(), Bcc: NewStringList(), Subject: "Plans", Body: "Dinner at eight."}
	draft.To.Add("a@x.example")
	draft.Cc.Add("b@y.example")
	draft.Bcc.Add("c@z.example")
	encoded, err := json.Marshal(sidecarControlMailDraftOf("a-network", draft))
	if err != nil {
		t.Fatal(err)
	}
	daemonDraft := &sidecar.DaemonMailDraftParams{}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(daemonDraft); err != nil {
		t.Fatal(err)
	}
	if daemonDraft.Network != "a-network" || daemonDraft.From != draft.From || !slices.Equal(daemonDraft.To, []string{"a@x.example"}) ||
		!slices.Equal(daemonDraft.Cc, []string{"b@y.example"}) || !slices.Equal(daemonDraft.Bcc, []string{"c@z.example"}) ||
		daemonDraft.Subject != draft.Subject || daemonDraft.Text != draft.Body {
		t.Errorf("the draft reached the daemon as %+v", daemonDraft)
	}
	if bare, err := json.Marshal(sidecarControlMailDraftOf("", &SidecarDraft{From: "ann@acme.root.example"})); err != nil || string(bare) != `{"from":"ann@acme.root.example"}` {
		t.Errorf("a bare draft crosses as %s, %v", bare, err)
	}

	cross := func(daemons any, own any) {
		t.Helper()
		encoded, err := json.Marshal(daemons)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, own); err != nil {
			t.Fatal(err)
		}
	}
	checkReply := &sidecarControlMailSendCheckReply{}
	cross(&sidecar.DaemonMailSendCheckReply{Domains: []*sidecar.DaemonMailDomainCheck{{
		Domain: "plain.example", Mode: uint32(sidecar.SmtpModeData), ByteCount: 2048, CapByteCount: 1024, ExpectedDelayMillis: 2_400_000,
		Prediction: uint32(sidecar.SmtpCheckTooLarge),
	}}}, checkReply)
	check := checkReply.smtpSendCheck()
	if len(check.Domains) != 1 || *check.Domains[0] != (sidecar.SmtpDomainCheck{Domain: "plain.example", Mode: sidecar.SmtpModeData, ByteCount: 2048,
		CapByteCount: 1024, ExpectedDelay: 40 * time.Minute, Prediction: sidecar.SmtpCheckTooLarge}) {
		t.Errorf("the check read back as %+v", check.Domains)
	}
	updateTime := time.UnixMilli(1_700_000_000_000)
	resultReply := &sidecarControlMailSendResultReply{}
	cross(&sidecar.DaemonMailSendResultReply{Domains: []*sidecar.DaemonMailDomainOutcome{
		{Domain: "a.example", Mode: uint32(sidecar.SmtpModeBdat), Outcome: uint32(sidecar.SmtpOutcomeDeferred), RetryAfterMillis: 90_000,
			UpdateTimeMillis: updateTime.UnixMilli(), TlsLevel: uint32(sidecar.SmtpTlsLevelMtaSts)},
		{Domain: "b.example", Outcome: uint32(sidecar.SmtpOutcomeRefused), ReplyCode: 550},
	}}, resultReply)
	result := resultReply.smtpSendResult()
	if len(result.Domains) != 2 {
		t.Fatalf("the result read back with %d domains", len(result.Domains))
	}
	deferred, refused := result.Domains[0], result.Domains[1]
	if deferred.Domain != "a.example" || deferred.Mode != sidecar.SmtpModeBdat || deferred.Outcome != sidecar.SmtpOutcomeDeferred ||
		deferred.RetryAfter != 90*time.Second || !deferred.UpdateTime.Equal(updateTime) || deferred.TlsLevel != sidecar.SmtpTlsLevelMtaSts {
		t.Errorf("a deferral read back as %+v", deferred)
	}
	if refused.Outcome != sidecar.SmtpOutcomeRefused || refused.ReplyCode != 550 || !refused.UpdateTime.IsZero() {
		t.Errorf("a refusal read back as %+v", refused)
	}
	if sidecarControlMillisDuration(1<<62) != time.Duration(1<<63-1) || sidecarControlMillisDuration(-1) != 0 {
		t.Error("a delay past a duration, or below none, reads as another")
	}
}

// A root daemon on socketPath hosting core as networkId's Sidecar, status answering online for it, and the send
// methods over backends registered as ur daemon registers them; stopped at the test's end.
func startTestMailSendDaemon(t *testing.T, socketPath string, core *sidecar.Sidecar, networkId sidecar.Id, backends *sidecar.DaemonMailSendBackends) {
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
	sidecarId := sidecar.NewId()
	if err := daemon.RegisterMethod(&sidecar.DaemonMethod{Name: sidecarControlMethodStatus, Handler: func(ctx context.Context, call *sidecar.DaemonCall) (any, error) {
		return map[string]any{
			"instance":   "default",
			"state":      "online",
			"generation": 1,
			"key_store":  "keychain",
			"networks": []map[string]any{{
				"network_id": networkId,
				"sidecar_id": sidecarId,
				"state":      "online",
				"generation": 1,
			}},
		}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	for _, method := range sidecar.DaemonMailSendMethods(backends) {
		if err := daemon.RegisterMethod(method); err != nil {
			t.Fatal(err)
		}
	}
	if err := daemon.AddSidecar(networkId, core); err != nil {
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

// The root's send methods' backends as a test scripts them: a check of every draft's two domains, one known send's
// outcomes, and a send the gate holds until the test approves it, which then queues its internet recipient at o/
// (§15.10's step 4). What each took is kept.
type testMailSendDaemonBackends struct {
	sendId     sidecar.Id
	approvalId sidecar.Id
	updateTime time.Time

	stateLock sync.Mutex
	held      bool
	checked   []*sidecar.MailSendArgs
	read      []sidecar.Id
	submitted []*sidecar.MailSendArgs
	sent      []*sidecar.MailSendArgs
}

func (self *testMailSendDaemonBackends) backends() *sidecar.DaemonMailSendBackends {
	return &sidecar.DaemonMailSendBackends{
		Check: func(ctx context.Context, core *sidecar.Sidecar, args *sidecar.MailSendArgs) (*sidecar.SmtpSendCheck, error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.checked = append(self.checked, args)
			return &sidecar.SmtpSendCheck{Domains: []*sidecar.SmtpDomainCheck{
				{Domain: "chunking.example", Mode: sidecar.SmtpModeBdat, ByteCount: testSendByteCount, CapByteCount: testSendCapByteCount,
					ExpectedDelay: 30 * time.Second, Prediction: sidecar.SmtpCheckOk},
				{Domain: "plain.example", Mode: sidecar.SmtpModeData, ByteCount: testSendByteCount, CapByteCount: testSendCapByteCount,
					ExpectedDelay: 40 * time.Minute, Prediction: sidecar.SmtpCheckTooLarge},
			}}, nil
		},
		Result: func(ctx context.Context, core *sidecar.Sidecar, sendId sidecar.Id) (*sidecar.SmtpSendResult, error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.read = append(self.read, sendId)
			if sendId != self.sendId {
				return nil, sidecar.ErrMailSendUnknown
			}
			return &sidecar.SmtpSendResult{Domains: []*sidecar.SmtpDomainOutcome{
				{Domain: "chunking.example", Mode: sidecar.SmtpModeBdat, Outcome: sidecar.SmtpOutcomeSent,
					TlsLevel: sidecar.SmtpTlsLevelUnauthenticated, UpdateTime: self.updateTime},
				{Domain: "plain.example", Mode: sidecar.SmtpModeData, Outcome: sidecar.SmtpOutcomeDeferred, RetryAfter: time.Minute,
					UpdateTime: self.updateTime},
				{Domain: "refusing.example", Outcome: sidecar.SmtpOutcomeRefused, ReplyCode: 550, UpdateTime: self.updateTime},
			}}, nil
		},
		Send: func(ctx context.Context, core *sidecar.Sidecar, args *sidecar.MailSendArgs) (*sidecar.MailSendResult, error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.submitted = append(self.submitted, args)
			if self.held {
				return nil, &sidecar.ApprovalPendingError{ApprovalId: self.approvalId}
			}
			self.sent = append(self.sent, args)
			// queued, as a send that went names it: one whose every recipient waits sent nothing, and Send answers
			// it no send_id (sidecar_mail_submit_test.go)
			return &sidecar.MailSendResult{SendId: self.sendId, SentCopy: true, Recipients: []*sidecar.MailSendRecipient{
				{Address: "someone@chunking.example", Outcome: sidecar.MailSendQueued},
			}}, nil
		},
	}
}

func (self *testMailSendDaemonBackends) approve() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.held = false
}

// What the backends took: checks, reads, submissions and sends.
func (self *testMailSendDaemonBackends) counts() (int, int, int, int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.checked), len(self.read), len(self.submitted), len(self.sent)
}

// The drafts the check took and those sent.
func (self *testMailSendDaemonBackends) drafts() ([]*sidecar.MailSendArgs, []*sidecar.MailSendArgs) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.checked), slices.Clone(self.sent)
}

// A local Sidecar of a world of its own, for a daemon to host, closed after everything the test started.
func testMailSendHostedCore(t *testing.T) *sidecar.Sidecar {
	t.Helper()
	world := newTestSidecarWorld(t)
	local := world.open(t)
	t.Cleanup(func() { world.close(t, local) })
	return local.Core()
}

// End to end over the root's daemon on scripted backends: CheckSend reads a draft's domains and GetSendResult a known
// send's outcomes, each with its copy; an unknown send answers MAIL_INVALID; and a send the daemon's gate holds
// answers approval_pending:<id> with nothing sent, until it is approved, when the same draft, its internet recipient
// queued, answers its send_id. Each backend took the draft's fields whole.
func TestMailSendRemoteOverTheRootDaemon(t *testing.T) {
	core := testMailSendHostedCore(t)
	scripted := &testMailSendDaemonBackends{
		sendId:     sidecar.NewId(),
		approvalId: sidecar.NewId(),
		updateTime: time.UnixMilli(1_700_000_000_000),
		held:       true,
	}
	socketPath := testRemoteSocketPath(t)
	startTestMailSendDaemon(t, socketPath, core, sidecar.NewId(), scripted.backends())
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	mail := remote.GetMail()
	if mail == nil {
		t.Fatal("a remote-mode Sidecar has no mail")
	}
	draft := &SidecarDraft{From: "ann@acme.root.example", To: NewStringList(), Cc: NewStringList(), Bcc: NewStringList(), Subject: "Plans", Body: "Dinner at eight."}
	draft.To.Add("someone@chunking.example")
	draft.Cc.Add("other@plain.example")
	draft.Bcc.Add("hidden@refusing.example")

	check := newTestEvsCallback[*SidecarSendCheck]()
	mail.CheckSend(draft, check)
	checked, err := check.await(t)
	if err != nil || !checked.Ok || checked.Domains.Len() != 2 {
		t.Fatalf("CheckSend answered %+v, %v", checked, err)
	}
	if chunking := testSendDomain(t, checked.Domains, "chunking.example"); chunking.Mode != SidecarSendModeBdat ||
		chunking.Prediction != SidecarSendPredictionOk || chunking.ExpectedDelayMillis != 30_000 || chunking.Copy != "" {
		t.Errorf("the CHUNKING domain checked as %+v", chunking)
	}
	if plain := testSendDomain(t, checked.Domains, "plain.example"); plain.Mode != SidecarSendModeData ||
		plain.Prediction != SidecarSendPredictionTooLarge || plain.ByteCount != testSendByteCount || plain.CapByteCount != testSendCapByteCount ||
		!strings.Contains(plain.Copy, "plain.example") {
		t.Errorf("the DATA domain checked as %+v", plain)
	}
	checkedDrafts, _ := scripted.drafts()
	if len(checkedDrafts) != 1 {
		t.Fatalf("the daemon took %d checks", len(checkedDrafts))
	}
	took := checkedDrafts[0]
	if took.From != draft.From || !slices.Equal(took.To, []string{"someone@chunking.example"}) || !slices.Equal(took.Cc, []string{"other@plain.example"}) ||
		!slices.Equal(took.Bcc, []string{"hidden@refusing.example"}) || took.Subject != draft.Subject || took.Text != draft.Body {
		t.Errorf("the daemon checked %+v", took)
	}

	read := newTestEvsCallback[*SidecarSendResult]()
	mail.GetSendResult(newId([16]byte(scripted.sendId)), read)
	outcomes, err := read.await(t)
	if err != nil || !outcomes.Ok || outcomes.Domains.Len() != 3 || outcomes.SendId.String() != scripted.sendId.String() {
		t.Fatalf("GetSendResult answered %+v, %v", outcomes, err)
	}
	if sent := testSendDomain(t, outcomes.Domains, "chunking.example"); sent.Outcome != SidecarSendOutcomeSent ||
		sent.TlsLabel != sidecar.SmtpTlsLabelUnauthenticated || sent.UpdateMillis != scripted.updateTime.UnixMilli() {
		t.Errorf("a send at TL4 read as %+v", sent)
	}
	if deferred := testSendDomain(t, outcomes.Domains, "plain.example"); deferred.Outcome != SidecarSendOutcomeDeferred ||
		deferred.RetryAfterMillis != 60_000 || deferred.RetryAtMillis != scripted.updateTime.Add(time.Minute).UnixMilli() {
		t.Errorf("a deferral read as %+v", deferred)
	}
	if refused := testSendDomain(t, outcomes.Domains, "refusing.example"); refused.Outcome != SidecarSendOutcomeRefused || refused.ReplyCode != 550 {
		t.Errorf("a refusal read as %+v", refused)
	}
	unknown := newTestEvsCallback[*SidecarSendResult]()
	mail.GetSendResult(NewId(), unknown)
	if result, err := unknown.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeInvalid || result.Domains != nil {
		t.Errorf("an unknown send read %+v, %v", result, err)
	}

	held := newTestEvsCallback[*SidecarResult]()
	mail.Send(draft, held)
	if result, err := held.await(t); err != nil || result.Ok || result.Code != "" || result.Value != sidecarApprovalPendingPrefix+scripted.approvalId.String() {
		t.Fatalf("a held send answered %+v, %v", result, err)
	}
	if _, _, submitted, sent := scripted.counts(); submitted != 1 || sent != 0 {
		t.Fatalf("%d submitted, %d sent while held", submitted, sent)
	}
	scripted.approve()
	approved := newTestEvsCallback[*SidecarResult]()
	mail.Send(draft, approved)
	result, err := approved.await(t)
	if err != nil || !result.Ok || result.Value != scripted.sendId.String() {
		t.Fatalf("an approved send answered %+v, %v", result, err)
	}
	if parsed, err := ParseId(result.Value); err != nil || parsed.String() != scripted.sendId.String() {
		t.Errorf("the send_id does not parse back: %v", err)
	}
	if checks, reads, submitted, sent := scripted.counts(); checks != 1 || reads != 2 || submitted != 2 || sent != 1 {
		t.Errorf("the daemon took %d checks, %d reads, %d submissions and %d sends", checks, reads, submitted, sent)
	}
	if _, sentDrafts := scripted.drafts(); len(sentDrafts) != 1 {
		t.Fatalf("%d sent", len(sentDrafts))
	} else if sent := sentDrafts[0]; sent.From != draft.From || !slices.Equal(sent.Bcc, []string{"hidden@refusing.example"}) || sent.Text != draft.Body {
		t.Errorf("the daemon sent %+v", sent)
	}
}

// On ur daemon's own backends, which take the hosted core's MailClient, a core whose mail binds nothing yet answers the
// daemon's unbound refusal for both reads and a send, each MAIL_UNAVAILABLE.
func TestMailSendRemoteUnavailableOverTheRootDaemon(t *testing.T) {
	core := testMailSendHostedCore(t)
	socketPath := testRemoteSocketPath(t)
	startTestMailSendDaemon(t, socketPath, core, sidecar.NewId(), nil)
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	mail := remote.GetMail()

	check := newTestEvsCallback[*SidecarSendCheck]()
	mail.CheckSend(testSendDraft("plain.example"), check)
	if result, err := check.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeUnavailable ||
		!strings.Contains(result.Message, sidecar.ErrMailUnbound.Error()) {
		t.Errorf("CheckSend answered %+v, %v", result, err)
	}
	read := newTestEvsCallback[*SidecarSendResult]()
	mail.GetSendResult(NewId(), read)
	if result, err := read.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeUnavailable ||
		!strings.Contains(result.Message, sidecar.ErrMailUnbound.Error()) {
		t.Errorf("GetSendResult answered %+v, %v", result, err)
	}
	send := newTestEvsCallback[*SidecarResult]()
	mail.Send(testSendDraft("plain.example"), send)
	if result, err := send.await(t); err != nil || result.Ok || result.Code != sidecarMailCodeUnavailable || strings.HasPrefix(result.Value, sidecarApprovalPendingPrefix) {
		t.Errorf("Send answered %+v, %v", result, err)
	}
}

// The remote backend sends each method unconfirmed with the draft's fields as the daemon reads them strictly, and a
// null answer, or a send's naming neither a send nor an approval, is no Ok.
func TestMailSendCallsCrossUnconfirmed(t *testing.T) {
	socketPath := testRemoteSocketPath(t)
	daemon := newTestRemoteDaemon(t, socketPath)
	approvalId := sidecar.NewId()
	empty := &atomic.Bool{}
	daemon.answer = func(request testRemoteDaemonRequest) (any, *sidecar.DaemonRpcError, bool) {
		switch request.Method {
		case sidecarControlMethodMailCheckSend:
			if empty.Load() {
				return nil, nil, true
			}
			return &sidecar.DaemonMailSendCheckReply{Domains: []*sidecar.DaemonMailDomainCheck{
				{Domain: "plain.example", Mode: uint32(sidecar.SmtpModeBdat), Prediction: uint32(sidecar.SmtpCheckOk)},
			}}, nil, true
		case sidecarControlMethodMailSendResult:
			if empty.Load() {
				return nil, nil, true
			}
			return &sidecar.DaemonMailSendResultReply{Domains: []*sidecar.DaemonMailDomainOutcome{
				{Domain: "plain.example", Outcome: uint32(sidecar.SmtpOutcomeQueued)},
			}}, nil, true
		case sidecarControlMethodMailSend:
			if empty.Load() {
				return &sidecar.DaemonMailSendReply{}, nil, true
			}
			return &sidecar.DaemonMailSendReply{ApprovalId: &approvalId}, nil, true
		}
		return nil, nil, false
	}
	daemon.start()
	remote, _, _ := openTestRemoteSidecar(t, socketPath)
	mail := remote.GetMail()

	check := newTestEvsCallback[*SidecarSendCheck]()
	mail.CheckSend(testSendDraft("plain.example"), check)
	if result, err := check.await(t); err != nil || !result.Ok || result.Domains.Len() != 1 || result.Domains.Get(0).Mode != SidecarSendModeBdat {
		t.Errorf("CheckSend answered %+v, %v", result, err)
	}
	read := newTestEvsCallback[*SidecarSendResult]()
	mail.GetSendResult(NewId(), read)
	if result, err := read.await(t); err != nil || !result.Ok || result.Domains.Len() != 1 || result.Domains.Get(0).Outcome != SidecarSendOutcomeQueued {
		t.Errorf("GetSendResult answered %+v, %v", result, err)
	}
	send := newTestEvsCallback[*SidecarResult]()
	mail.Send(testSendDraft("plain.example"), send)
	if result, err := send.await(t); err != nil || result.Value != sidecarApprovalPendingPrefix+approvalId.String() {
		t.Errorf("Send answered %+v, %v", result, err)
	}

	mailCalls := 0
	for _, request := range daemon.seen() {
		if !strings.HasPrefix(request.Method, "mail.") {
			continue
		}
		mailCalls += 1
		if request.UrConfirmed {
			t.Errorf("%s was sent confirmed", request.Method)
		}
		var params any = &sidecar.DaemonMailDraftParams{}
		if request.Method == sidecarControlMethodMailSendResult {
			params = &sidecar.DaemonMailSendResultParams{}
		}
		decoder := json.NewDecoder(bytes.NewReader(request.Params))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(params); err != nil {
			t.Errorf("%s's params %s: %v", request.Method, request.Params, err)
		}
		if draft, ok := params.(*sidecar.DaemonMailDraftParams); ok && (draft.From != "admin@net.example" || !slices.Equal(draft.To, []string{"someone@plain.example"}) || draft.Text != "b") {
			t.Errorf("%s sent the draft as %+v", request.Method, draft)
		}
	}
	if mailCalls != 3 {
		t.Errorf("%d mail calls", mailCalls)
	}

	empty.Store(true)
	emptyCheck := newTestEvsCallback[*SidecarSendCheck]()
	mail.CheckSend(testSendDraft("plain.example"), emptyCheck)
	if result, _ := emptyCheck.await(t); result.Ok || result.Code == "" {
		t.Errorf("a null check answered %+v", result)
	}
	emptyRead := newTestEvsCallback[*SidecarSendResult]()
	mail.GetSendResult(NewId(), emptyRead)
	if result, _ := emptyRead.await(t); result.Ok || result.Code == "" {
		t.Errorf("a null result answered %+v", result)
	}
	emptySend := newTestEvsCallback[*SidecarResult]()
	mail.Send(testSendDraft("plain.example"), emptySend)
	if result, _ := emptySend.await(t); result.Ok || result.Code == "" || result.Value != "" {
		t.Errorf("a send naming nothing answered %+v", result)
	}
}

// A daemon's refusal reads as its MAIL_ code: a method it lacks, no network to act on and a call past its time as
// unavailable; a handler's failure by the root's mail error its text names; anything else, a person's denial and a
// From the effect refused among them, as refused.
func TestMailRemoteRefusalCodes(t *testing.T) {
	failed := func(err error) *sidecarControlError {
		return &sidecarControlError{Code: sidecar.DaemonRpcFailed, Message: err.Error()}
	}
	for _, c := range []struct {
		refusal *sidecarControlError
		code    string
	}{
		{refusal: &sidecarControlError{Code: sidecarControlCodeMethodNotFound, Message: "method not found"}, code: sidecarMailCodeUnavailable},
		{refusal: &sidecarControlError{Code: sidecarControlCodeFailedPrecondition, Message: "no network"}, code: sidecarMailCodeUnavailable},
		{refusal: &sidecarControlError{Code: sidecarControlCodeTimeout, Message: "the call timed out"}, code: sidecarMailCodeUnavailable},
		{refusal: failed(fmt.Errorf("%w: check a send", sidecar.ErrMailSendUnavailable)), code: sidecarMailCodeUnavailable},
		{refusal: failed(fmt.Errorf("%w: %w", sidecar.ErrMailUnbound, errors.New("synthetic"))), code: sidecarMailCodeUnavailable},
		{refusal: failed(fmt.Errorf("%w: kind 14", sidecar.ErrApprovalNoLookup)), code: sidecarMailCodeUnavailable},
		{refusal: failed(sidecar.ErrMailSendUnknown), code: sidecarMailCodeInvalid},
		{refusal: failed(fmt.Errorf("%w: a message needs a From and a recipient", sidecar.ErrMailSendInvalid)), code: sidecarMailCodeInvalid},
		{refusal: failed(sidecar.ErrMailNoMailbox), code: sidecarMailCodeNoMailbox},
		{refusal: failed(sidecar.ErrMailVaultNotConsented), code: sidecarMailCodeNotConsented},
		{refusal: failed(fmt.Errorf("%w: %w", sidecar.ErrSidecarEffectNotDone, sidecar.ErrMailSendFromRefused)), code: sidecarMailCodeRefused},
		{refusal: failed(fmt.Errorf("%w: approval x", sidecar.ErrApprovalDenied)), code: sidecarMailCodeRefused},
		{refusal: failed(errors.New("synthetic refusal")), code: sidecarMailCodeRefused},
	} {
		if code := sidecarMailCodeOf(c.refusal); code != c.code {
			t.Errorf("%d %q reads %s, not %s", c.refusal.Code, c.refusal.Message, code, c.code)
		}
		if result := sidecarMailResultOf("", c.refusal); result.Code != c.code || result.Message != c.refusal.Message {
			t.Errorf("%q's result %+v", c.refusal.Message, result)
		}
	}
}
