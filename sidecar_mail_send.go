//go:build !ios_extension && !js

// SidecarMail's send capability (§18.5's Send capability, D-SDK-31, owner decision 125, §14.5, FS-123; SDK-M9-01):
// CheckSend runs as the person composes, and GetSendResult and the mail_send event follow the send, from the outbox
// too. Each maps the core's per-recipient-domain answers one for one and adds only §18.5's copy:
//
//	check    CheckSend(draft): per recipient domain, the mode (bdat, data or unknown), the message's encoded bytes,
//	         the DATA cap, the expected delay and ok, too_large or unknown, which the core's SmtpSender.Check decides
//	         (§14.5's Check) over the message Send would make of the same draft: on a host that proves, its own cap
//	         and delay, and through a phone's outbox the largest cap its member's prover hosts record (p/) and the
//	         wait for one
//	result   GetSendResult(sendId): per recipient domain, sent, queued, deferred, too_large, refused, tls_refused or
//	         failed (§14.5's outcomes, the core's SmtpDomainOutcome), with too_large's bytes and cap, deferred's
//	         retry, refused's MX code, and for mail sent at TL4 I-11's label. The sending device writes each outcome
//	         to or/<h>/<effect_key> (§15.10), so a phone reads what the prover host that took its entry wrote
//	event    mail_send {send_id}: the core raises it for each changed outcome, the composing device from or/ as a
//	         prover host writes it (§15.10's step 4), and sidecar_events.go carries it to every listener with its
//	         send_id, from the local core and the daemon alike; an app then reads GetSendResult
//	send     Send (sidecar_mail.go) answers the send_id both name, its internet mail through §14's outbox unless
//	         this host proves
//	copy     §18.5's line for a domain where one applies, in Copy: of the results only too_large cites the DATA cap,
//	         naming that domain, the size and the cap, and no line names an indexer, a receiver or a coverage
//	         setting, which are inbound (GetReceiverCoverage)
//
// Wiring: the root's types are on sidecar, its calls are not. MailClient's check and its send results (MAIL-M9-01 and
// MAIL-M9-02, over SMTP-M9-08's SmtpSender.Check and SmtpDomainOutcome) have not landed, and §5.8 has no method for
// either, so both calls answer MAIL_UNAVAILABLE on both backends until they do (CONTRACTS.md C-17), each backend's
// method below naming what it waits on.
//
// In the copy, sizes read in bytes, KB and MB of 1,000 and 1,000,000 bytes, the cap rounded down and the size up at
// the precision shown, so a message over the cap never reads as equal to it or under it; a delay reads in minutes,
// rounded up; and a retry reads as a time in the location of the clock the copy is made at. Addresses and domains are
// the person's own and go only between the app and its core or daemon: nothing here logs one (I-13). Safe for
// concurrent use.

package sdk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
)

// How a recipient domain's MX takes mail (§14.4). A format value: apps read them.
const (
	// Its post-TLS EHLO reply offers CHUNKING.
	SidecarSendModeBdat = "bdat"
	// It does not, so a message passing the DATA cap stops before MAIL (D-SMTP-20).
	SidecarSendModeData = "data"
	// No session with its MX has reached that reply, and no outcome records one.
	SidecarSendModeUnknown = "unknown"
)

// CheckSend's predictions (§14.5's Check). A format value: apps read them.
const (
	// Nothing known stops the send.
	SidecarSendPredictionOk = "ok"
	// The domain's MX takes DATA and the message passes the cap.
	SidecarSendPredictionTooLarge = "too_large"
	// The domain's mode is unknown and the message passes the cap, so it depends on that MX.
	SidecarSendPredictionUnknown = "unknown"
)

// GetSendResult's outcomes (§14.5's outcomes). A format value: apps read them.
const (
	// The MX took the message.
	SidecarSendOutcomeSent = "sent"
	// In the outbox for one of the member's prover hosts (D-SMTP-18).
	SidecarSendOutcomeQueued = "queued"
	// Held for HALTED, QUOTA or TRY_LATER, or left by a host whose cap is under the message's bytes, until its retry.
	SidecarSendOutcomeDeferred = "deferred"
	// DATA mode, and past the cap of every host that could prove it: decided before MAIL.
	SidecarSendOutcomeTooLarge = "too_large"
	// The MX refused it, with its reply code.
	SidecarSendOutcomeRefused = "refused"
	// TL4 without the person's allowance for the domain, or below the MX host's pin (§14.4).
	SidecarSendOutcomeTlsRefused = "tls_refused"
	SidecarSendOutcomeFailed     = "failed"
	// A kind this sdk does not know, which claims nothing: neither delivery nor failure.
	SidecarSendOutcomeUnknown = "unknown"
)

// One recipient domain of a CheckSend or a GetSendResult (§14.5, §18.5). json in the C ABI and js. A field is set only
// for the call and the answer its comment names; the rest are zero.
type SidecarSendDomain struct {
	// The recipient domain, as the recipients' addresses name it.
	Domain string `json:"domain"`
	// bdat, data or unknown: from this device's last session with the domain's MX, kept as its TLS pin is, or from a
	// send's outcome.
	Mode string `json:"mode"`
	// The message's encoded bytes, its headers, DKIM signatures and dot-stuffing included: CheckSend, and a result's
	// too_large.
	ByteCount int64 `json:"byte_count,omitempty"`
	// The DATA cap, the smtp_data_proof_bytes of the host that will prove the message or, through an outbox, the
	// largest the member's prover hosts record; 0 where no host has recorded one: CheckSend, and a result's too_large.
	CapByteCount int64 `json:"cap_byte_count,omitempty"`
	// CheckSend: ok, too_large or unknown.
	Prediction string `json:"prediction,omitempty"`
	// CheckSend: BDAT's proofs, or a DATA send's parts at the proving host's measured rate, plus through an outbox the
	// wait for a prover host, in milliseconds.
	ExpectedDelayMillis int64 `json:"expected_delay_millis,omitempty"`
	// GetSendResult: sent, queued, deferred, too_large, refused, tls_refused or failed.
	Outcome string `json:"outcome,omitempty"`
	// GetSendResult, deferred: how long the queue holds the message from the result's write, in milliseconds.
	RetryAfterMillis int64 `json:"retry_after_millis,omitempty"`
	// GetSendResult, deferred: when it is tried again, in unix milliseconds.
	RetryAtMillis int64 `json:"retry_at_millis,omitempty"`
	// GetSendResult, refused: the MX's reply code, such as 550; 0 where it gave none.
	ReplyCode int32 `json:"reply_code,omitempty"`
	// GetSendResult: the TLS level the send reached, 1 to 4 for §14.4's TL1 to TL4; 0 for none.
	TlsLevel int32 `json:"tls_level,omitempty"`
	// GetSendResult, sent: I-11's label, "readable by an active URnetwork edge" for mail sent at TL4 (§2.8 EX4), and
	// empty at TL1 to TL3. An app shows it with the message.
	TlsLabel string `json:"tls_label,omitempty"`
	// GetSendResult: when the sending device last wrote this domain's result, in unix milliseconds; 0 for unknown.
	UpdateMillis int64 `json:"update_millis,omitempty"`
	// §18.5's copy for this domain, for a person; empty where none applies.
	Copy string `json:"copy,omitempty"`
}

// A send's recipient domains, each as it first appears among the recipients.
type SidecarSendDomainList struct {
	exportedList[*SidecarSendDomain]
}

// An empty list.
func NewSidecarSendDomainList() *SidecarSendDomainList {
	return &SidecarSendDomainList{
		exportedList: *newExportedList[*SidecarSendDomain](),
	}
}

// One CheckSend's terminal answer. json in the C ABI and js.
type SidecarSendCheck struct {
	// The check ran.
	Ok bool `json:"ok"`
	// One a recipient domain: its mode, bytes, cap, expected delay, prediction and copy.
	Domains *SidecarSendDomainList `json:"domains,omitempty"`
	// A MAIL_ code, for a refusal.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Answers a CheckSend once.
type SidecarSendCheckCallback connect.ApiCallback[*SidecarSendCheck]

// One GetSendResult's terminal answer. json in the C ABI and js.
type SidecarSendResult struct {
	// The send's outcomes were read.
	Ok bool `json:"ok"`
	// The send, as Send answered it.
	SendId *Id `json:"send_id,omitempty"`
	// One a recipient domain: its outcome, what that outcome carries, and its copy.
	Domains *SidecarSendDomainList `json:"domains,omitempty"`
	// A MAIL_ code, for a refusal.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Answers a GetSendResult once.
type SidecarSendResultCallback connect.ApiCallback[*SidecarSendResult]

// The core answered no check or no result, and no error either.
var errSidecarMailSendEmpty = errors.New("the sidecar answered nothing for this send")

// Checks draft as the person composes it (§14.5's Check): per recipient domain, the mode, the message's encoded
// bytes, the DATA cap, the expected delay and ok, too_large or unknown, each with its copy. It sends nothing, and an
// app may call it on each change.
func (self *SidecarMail) CheckSend(draft *SidecarDraft, callback SidecarSendCheckCallback) {
	trace(func() {
		// the draft's lists are the caller's to reuse once CheckSend returns
		draftCopy := sidecarMailCopyDraft(draft)
		sidecarMailAnswer(self, func(ctx context.Context) *SidecarSendCheck {
			refuse := func(result *SidecarResult) *SidecarSendCheck {
				return &SidecarSendCheck{Code: result.Code, Message: result.Message}
			}
			if err := self.callErr(); err != nil {
				return refuse(sidecarMailResultOf("", err))
			}
			if draftCopy == nil || len(draftCopy.recipients()) == 0 || draftCopy.From == "" {
				return refuse(sidecarMailInvalid("a message needs a From address and at least one recipient"))
			}
			check, err := self.backend.checkSend(ctx, draftCopy)
			if err == nil && check == nil {
				err = errSidecarMailSendEmpty
			}
			if err != nil {
				return refuse(sidecarMailResultOf("", err))
			}
			return &SidecarSendCheck{Ok: true, Domains: sidecarSendCheckDomainsOf(check)}
		}, connect.ApiCallback[*SidecarSendCheck](callback))
	})
}

// The outcomes of the send sendId names, per recipient domain (§14.5's outcomes), each with its copy: what Send
// answered, read again on each mail_send that names it.
func (self *SidecarMail) GetSendResult(sendId *Id, callback SidecarSendResultCallback) {
	trace(func() {
		var sendIdValue *[16]byte
		if sendId != nil {
			value := sendId.id
			sendIdValue = &value
		}
		sidecarMailAnswer(self, func(ctx context.Context) *SidecarSendResult {
			refuse := func(result *SidecarResult) *SidecarSendResult {
				return &SidecarSendResult{Code: result.Code, Message: result.Message}
			}
			if err := self.callErr(); err != nil {
				return refuse(sidecarMailResultOf("", err))
			}
			if sendIdValue == nil {
				return refuse(sidecarMailInvalid("a send's result names its send_id"))
			}
			result, err := self.backend.sendResult(ctx, sidecar.Id(*sendIdValue))
			if err == nil && result == nil {
				err = errSidecarMailSendEmpty
			}
			if err != nil {
				return refuse(sidecarMailResultOf("", err))
			}
			return &SidecarSendResult{
				Ok:      true,
				SendId:  newId(*sendIdValue),
				Domains: sidecarSendResultDomainsOf(result, time.Now()),
			}
		}, connect.ApiCallback[*SidecarSendResult](callback))
	})
}

// A copy of draft whose lists are its own, nil for nil.
func sidecarMailCopyDraft(draft *SidecarDraft) *SidecarDraft {
	if draft == nil {
		return nil
	}
	copied := *draft
	copied.To, copied.Cc, copied.Bcc = sidecarMailCopyList(draft.To), sidecarMailCopyList(draft.Cc), sidecarMailCopyList(draft.Bcc)
	return &copied
}

// The bound domains of the core's check, in its order, each with its compose copy.
func sidecarSendCheckDomainsOf(check *sidecar.SmtpSendCheck) *SidecarSendDomainList {
	list := NewSidecarSendDomainList()
	for _, domainCheck := range check.Domains {
		if domainCheck == nil {
			continue
		}
		domain := &SidecarSendDomain{
			Domain:              domainCheck.Domain,
			Mode:                sidecarSendModeOf(domainCheck.Mode),
			ByteCount:           sidecarSendByteCountOf(domainCheck.ByteCount),
			CapByteCount:        sidecarSendByteCountOf(domainCheck.CapByteCount),
			Prediction:          sidecarSendPredictionOf(domainCheck.Prediction),
			ExpectedDelayMillis: max(domainCheck.ExpectedDelay.Milliseconds(), 0),
		}
		domain.Copy = sidecarSendCheckCopy(domain)
		list.Add(domain)
	}
	return list
}

// The bound domains of the core's result, in its order, each with its result copy, a retry's time read at now.
func sidecarSendResultDomainsOf(result *sidecar.SmtpSendResult, now time.Time) *SidecarSendDomainList {
	list := NewSidecarSendDomainList()
	for _, outcome := range result.Domains {
		if outcome == nil {
			continue
		}
		domain := &SidecarSendDomain{
			Domain:       outcome.Domain,
			Mode:         sidecarSendModeOf(outcome.Mode),
			Outcome:      sidecarSendOutcomeOf(outcome.Outcome),
			TlsLevel:     int32(min(uint32(outcome.TlsLevel), math.MaxInt32)),
			UpdateMillis: sidecarMillisOf(outcome.UpdateTime),
		}
		switch domain.Outcome {
		case SidecarSendOutcomeTooLarge:
			domain.ByteCount = sidecarSendByteCountOf(outcome.ByteCount)
			domain.CapByteCount = sidecarSendByteCountOf(outcome.CapByteCount)
		case SidecarSendOutcomeDeferred:
			// retry_after_ms counts from the result's write; with no write time, from this read, the nearest known
			retryAfter := max(outcome.RetryAfter, 0)
			from := now
			if !outcome.UpdateTime.IsZero() {
				from = outcome.UpdateTime
			}
			domain.RetryAfterMillis = retryAfter.Milliseconds()
			domain.RetryAtMillis = from.Add(retryAfter).UnixMilli()
		case SidecarSendOutcomeRefused:
			domain.ReplyCode = int32(min(outcome.ReplyCode, math.MaxInt32))
		case SidecarSendOutcomeSent:
			domain.TlsLabel = sidecarSendTlsLabelOf(outcome.TlsLevel)
		}
		domain.Copy = sidecarSendResultCopy(domain, now)
		list.Add(domain)
	}
	return list
}

// A mode's word, unknown for one this sdk does not know.
func sidecarSendModeOf(mode sidecar.SmtpMode) string {
	switch mode {
	case sidecar.SmtpModeBdat:
		return SidecarSendModeBdat
	case sidecar.SmtpModeData:
		return SidecarSendModeData
	}
	return SidecarSendModeUnknown
}

// A prediction's word. One this sdk does not know, or the never-reported unspecified, reads unknown, which promises
// nothing, rather than ok.
func sidecarSendPredictionOf(prediction sidecar.SmtpCheckPrediction) string {
	switch prediction {
	case sidecar.SmtpCheckOk:
		return SidecarSendPredictionOk
	case sidecar.SmtpCheckTooLarge:
		return SidecarSendPredictionTooLarge
	}
	return SidecarSendPredictionUnknown
}

// An outcome's word, unknown for a kind this sdk does not know or the never-written unspecified.
func sidecarSendOutcomeOf(kind sidecar.SmtpOutcomeKind) string {
	switch kind {
	case sidecar.SmtpOutcomeSent:
		return SidecarSendOutcomeSent
	case sidecar.SmtpOutcomeQueued:
		return SidecarSendOutcomeQueued
	case sidecar.SmtpOutcomeDeferred:
		return SidecarSendOutcomeDeferred
	case sidecar.SmtpOutcomeTooLarge:
		return SidecarSendOutcomeTooLarge
	case sidecar.SmtpOutcomeRefused:
		return SidecarSendOutcomeRefused
	case sidecar.SmtpOutcomeTlsRefused:
		return SidecarSendOutcomeTlsRefused
	case sidecar.SmtpOutcomeFailed:
		return SidecarSendOutcomeFailed
	}
	return SidecarSendOutcomeUnknown
}

// The label mail sent at level carries (I-11): EX4's at TL4, none at TL1 to TL3, which exclude even an active edge,
// and EX4's for a level this sdk does not know, which shows no authentication.
func sidecarSendTlsLabelOf(level sidecar.SmtpTlsLevel) string {
	switch level {
	case sidecar.SmtpTlsLevelDane, sidecar.SmtpTlsLevelMtaSts, sidecar.SmtpTlsLevelIndependentMx:
		return ""
	}
	return sidecar.SmtpTlsLabelUnauthenticated
}

// A byte count as an int64, the largest for one past it.
func sidecarSendByteCountOf(byteCount uint64) int64 {
	return int64(min(byteCount, uint64(math.MaxInt64)))
}

// §18.5's compose line for domain: too_large's, the unknown mode's over the cap, and the data mode's delay under it;
// none for anything else, or where a number its line needs is missing.
func sidecarSendCheckCopy(domain *SidecarSendDomain) string {
	switch domain.Prediction {
	case SidecarSendPredictionTooLarge:
		return sidecarSendTooLargeCopy(domain)
	case SidecarSendPredictionUnknown:
		if 0 < domain.CapByteCount && domain.CapByteCount < domain.ByteCount {
			size, cap := sidecarSendSizesCopy(domain.ByteCount, domain.CapByteCount)
			return fmt.Sprintf("If %s's mail server is an older kind, it accepts only up to %s, and this message is %s.", domain.Domain, cap, size)
		}
	case SidecarSendPredictionOk:
		if domain.Mode == SidecarSendModeData && 0 < domain.ExpectedDelayMillis {
			delay := time.Duration(domain.ExpectedDelayMillis) * time.Millisecond
			return fmt.Sprintf("Sending to %s takes about %s.", domain.Domain, sidecarSendDelayCopy(delay))
		}
	}
	return ""
}

// too_large's line, which compose and the results share: the domain, the cap and the size, and that the other
// recipients keep their own outcomes. None without a cap the size passes.
func sidecarSendTooLargeCopy(domain *SidecarSendDomain) string {
	if domain.CapByteCount <= 0 || domain.ByteCount <= domain.CapByteCount {
		return ""
	}
	size, cap := sidecarSendSizesCopy(domain.ByteCount, domain.CapByteCount)
	return fmt.Sprintf("%s accepts messages only up to %s from your devices, and this one is %s. Send %s a smaller message; your other recipients aren't affected.", domain.Domain, cap, size, domain.Domain)
}

// §18.5's results line for domain, a retry's time read in now's location; none for sent or an unknown outcome.
func sidecarSendResultCopy(domain *SidecarSendDomain, now time.Time) string {
	switch domain.Outcome {
	case SidecarSendOutcomeQueued:
		return "Waiting for one of your computers to send it."
	case SidecarSendOutcomeDeferred:
		return fmt.Sprintf("Sending again at %s.", sidecarSendTimeCopy(time.UnixMilli(domain.RetryAtMillis), now))
	case SidecarSendOutcomeTooLarge:
		return sidecarSendTooLargeCopy(domain)
	case SidecarSendOutcomeRefused:
		if domain.ReplyCode <= 0 {
			return fmt.Sprintf("%s refused it.", domain.Domain)
		}
		return fmt.Sprintf("%s refused it (%d).", domain.Domain, domain.ReplyCode)
	case SidecarSendOutcomeTlsRefused:
		// §14.4's TL4 refusal, in the words QD-729 gives the allowance's approver
		return fmt.Sprintf("Didn't send to %s: its mail server can't be authenticated, so an active URnetwork edge could read the message.", domain.Domain)
	case SidecarSendOutcomeFailed:
		return fmt.Sprintf("Couldn't send to %s.", domain.Domain)
	}
	return ""
}

// The size and the cap as a person reads them (the file's comment): the cap rounded down and the size up.
func sidecarSendSizesCopy(byteCount int64, capByteCount int64) (string, string) {
	return sidecarSendByteCopy(byteCount, true), sidecarSendByteCopy(capByteCount, false)
}

// A byte count in the smallest of bytes, KB (whole) and MB or GB (to a tenth) that holds it under 1,000, rounded up
// or down at that precision. Rounding up past 999 of a unit reads in the next.
func sidecarSendByteCopy(byteCount int64, roundUp bool) string {
	byteCount = max(byteCount, 0)
	type byteUnit struct {
		name string
		// the bytes in one of the unit, and in one step of the precision shown
		unitByteCount int64
		stepByteCount int64
	}
	units := []byteUnit{
		{name: "bytes", unitByteCount: 1, stepByteCount: 1},
		{name: "KB", unitByteCount: 1000, stepByteCount: 1000},
		{name: "MB", unitByteCount: 1000 * 1000, stepByteCount: 100 * 1000},
		{name: "GB", unitByteCount: 1000 * 1000 * 1000, stepByteCount: 100 * 1000 * 1000},
	}
	for i, unit := range units {
		steps := byteCount / unit.stepByteCount
		if roundUp && byteCount%unit.stepByteCount != 0 {
			steps += 1
		}
		stepsPerUnit := unit.unitByteCount / unit.stepByteCount
		if i < len(units)-1 && 1000*stepsPerUnit <= steps {
			continue
		}
		switch {
		case unit.unitByteCount == 1 && steps == 1:
			return "1 byte"
		case stepsPerUnit == 1:
			return fmt.Sprintf("%d %s", steps, unit.name)
		default:
			return fmt.Sprintf("%d.%d %s", steps/stepsPerUnit, steps%stepsPerUnit, unit.name)
		}
	}
	return ""
}

// A delay as a person reads it after "about": a minute, minutes, or hours and tens of minutes, rounded up.
func sidecarSendDelayCopy(delay time.Duration) string {
	minutes := int64(delay / time.Minute)
	if delay%time.Minute != 0 {
		minutes += 1
	}
	minutes = max(minutes, 1)
	if minutes == 1 {
		return "a minute"
	}
	if minutes < 60 {
		return fmt.Sprintf("%d minutes", minutes)
	}
	hours := minutes / 60
	// the minutes past the hours, up to the next ten
	tens := (minutes%60 + 9) / 10
	if tens == 6 {
		hours, tens = hours+1, 0
	}
	parts := []string{}
	switch {
	case hours == 1 && tens == 0:
		parts = append(parts, "an hour")
	case hours == 1:
		parts = append(parts, "1 hour")
	default:
		parts = append(parts, fmt.Sprintf("%d hours", hours))
	}
	if 0 < tens {
		parts = append(parts, fmt.Sprintf("%d minutes", 10*tens))
	}
	return strings.Join(parts, " ")
}

// A time as a person reads it in now's location, with its zone: the clock alone on now's day, else with the date.
func sidecarSendTimeCopy(at time.Time, now time.Time) string {
	local := at.In(now.Location())
	year, month, day := local.Date()
	nowYear, nowMonth, nowDay := now.Date()
	if year == nowYear && month == nowMonth && day == nowDay {
		return local.Format("15:04 MST")
	}
	if year == nowYear {
		return local.Format("Jan 2 15:04 MST")
	}
	return local.Format("Jan 2 2006 15:04 MST")
}

// See sidecarMailBackend: the core's check of the message Send would make of draft, which MAIL-M9-01 puts on
// MailClient over SMTP-M9-08's SmtpSender.Check (and, for a phone, MAIL-M9-02's p/ caps). Not on sidecar yet.
func (self *sidecarMailCoreBackend) checkSend(ctx context.Context, draft *SidecarDraft) (*sidecar.SmtpSendCheck, error) {
	return nil, errSidecarMailNotLanded("check a send (MailClient's check over SmtpSender.Check)")
}

// See sidecarMailBackend: a send's outcomes by its send_id, which MAIL-M9-02 puts on MailClient from or/ (its
// MailOutbox), each SMTP-M9-08's SmtpDomainOutcome. Not on sidecar yet.
func (self *sidecarMailCoreBackend) sendResult(ctx context.Context, sendId sidecar.Id) (*sidecar.SmtpSendResult, error) {
	return nil, errSidecarMailNotLanded("read a send's result (MailClient's outcomes from or/)")
}

// See sidecarMailBackend: §5.8 has no check method yet.
func (self *sidecarMailRemoteBackend) checkSend(ctx context.Context, draft *SidecarDraft) (*sidecar.SmtpSendCheck, error) {
	return nil, errSidecarMailNotLanded("check a send through its daemon (no control method yet)")
}

// See sidecarMailBackend: §5.8 has no result method yet. The daemon's mail_send events do reach the app
// (sidecar_backend.go), with their send_id.
func (self *sidecarMailRemoteBackend) sendResult(ctx context.Context, sendId sidecar.Id) (*sidecar.SmtpSendResult, error) {
	return nil, errSidecarMailNotLanded("read a send's result through its daemon (no control method yet)")
}
