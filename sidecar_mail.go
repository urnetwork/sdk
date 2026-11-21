//go:build !ios_extension && !js

// SidecarMail (§18.5, §15.9's app seams, D-SDK-2, D-SDK-31, R-SDK-1; SDK-M8-04): an app's calls on its network's
// mail, each a thin mapping onto the core's MailClient on the local backend, or onto ur daemon's control methods on
// the remote one. Nothing here adds mail logic: aliases, coverage, burners, vault entries and one-time codes are the
// core's, and every check it makes stands behind each call.
//
//	aliases    GetAliases: every address record a reader honors (MailClient.Aliases; the daemon's id.ls)
//	send       Send: §14's outbox unless this host proves (§15.10). On the remote backend, ur daemon's mail.send (§5.8,
//	           owner decision 288), sent unconfirmed: the daemon's gate holds a send naming an outside recipient for an
//	           I-8 approval as it holds a submission (§15.10 step 1), which answers approval_pending:<id> with nothing
//	           sent, and the same draft sent again once a person approved goes; mail to the network's own addresses
//	           goes at once. Both backends return the id of a send accepted for at least one recipient or
//	           recovered from the approval ledger. Its outcomes, check and mail_send event are sidecar_mail_send.go's
//	receiver   the receiver hosting API: the "mail" component, MailClient attached to the core's generations, runs this
//	           device's receiver where policy grants it the listen, only while the sidecar role runs, which on an app
//	           host is while the app is open (§18.4's Listen row; owner decision 197). SetReceiverEnabled is the
//	           indexer designation, not the listen: on the remote backend it files the daemon's IndexerSet for mail at
//	           the member scope (indexer.designate) and retires it (indexer.retire_scope), as a person's confirmed act
//	           (§15.6, §7.9; D-CORE-13); on the local backend it answers MAIL_REFUSED, since an app host receives only
//	           while open and is never an indexer (owner decision 197)
//	coverage   GetReceiverCoverage: indexer, foreground-only or none (§18.5's Coverage), the core's
//	           MailClient.ReceiverCoverage (MAIL-M8-03). Until a backend can tell, it answers foreground-only, which
//	           claims no indexer and carries the copy a person needs while reliable delivery is not shown
//	burners    sidecar_mail_burner.go: CreateBurner, Burn, GetVaultEntries, GetUsedAt and GetOneTimeCodes
//
// The member scope (reading): §15.6 files the designation "in a personal network or a member scope". A member
// scope covers this member's mail in a Team, and in a personal network, whose one member is the owner, it covers
// every mailbox while needing only the member's consented device, which a paired daemon is; a network scope would
// need an Owner's or Admin's device in a Team. So the remote backend always designates the member scope.
//
// Every call answers once, on a Go thread, through its callback, or for the synchronous GetAliases and
// GetReceiverCoverage within ReadTimeout on the caller's: Ok when it did what it asked, else a refusal whose Code is
// one of the file's MAIL_ codes and whose Message is for a person. An outward action the core's gate holds comes back
// approval_pending:<id> (sidecarResultOf). A panic in the core reaches the callback as its error, never the app.
//
// Addresses, labels and sites are the person's own and go only between the app and its core or daemon: nothing here
// logs one (I-13), and no credential crosses (I-7). Safe for concurrent use.

package sdk

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
)

func init() {
	registerSidecarComponentFactory(SidecarComponentMail, newSidecarMailComponent)
}

// What an app's mail calls are held to.
type sidecarMailSettings struct {
	// The bound of one call that answers through a callback: 30 s, a confirmed designation's signing and filing
	// included.
	CallTimeout time.Duration
	// The bound of GetAliases and GetReceiverCoverage, which answer on the caller's thread: 10 s.
	ReadTimeout time.Duration
}

// The defaults, a fresh value each call.
func defaultSidecarMailSettings() *sidecarMailSettings {
	return &sidecarMailSettings{
		CallTimeout: 30 * time.Second,
		ReadTimeout: 10 * time.Second,
	}
}

// The refusal codes of SidecarMail's results (the file's comment). A format value: apps read them.
const (
	// The Sidecar closed, the call's context ended, no daemon answered, or this host's core or daemon has no such
	// call yet.
	sidecarMailCodeUnavailable = "MAIL_UNAVAILABLE"
	// The core or the daemon refused the call, or the local backend's SetReceiverEnabled (owner decision 197).
	sidecarMailCodeRefused = "MAIL_REFUSED"
	// The call names nothing it can act on: no draft, site, address or entry.
	sidecarMailCodeInvalid = "MAIL_INVALID"
	// This device is not a consented device of the mailbox's owner, who alone reads vault entries (§8.5, §15.8).
	sidecarMailCodeNotConsented = "MAIL_NOT_CONSENTED"
	// This device's member has no mailbox, or the network has no mailcfg: mail is not enabled.
	sidecarMailCodeNoMailbox = "MAIL_NO_MAILBOX"
)

// GetReceiverCoverage's values (§18.5's Coverage). A format value: apps read them.
const (
	// An IndexerSet designates a mail indexer for this user's mail (§7.9, §15.6).
	SidecarReceiverCoverageIndexer = "indexer"
	// Receivers, but no mail indexer: "A phone alone cannot reliably receive mail: an indexer, an always-on device (a
	// desktop ur daemon, a NAS or VPS), is needed; time-sensitive codes may not arrive" (owner decisions 12, 197).
	SidecarReceiverCoverageForegroundOnly = "foreground-only"
	// No receiver; CreateBurner shows foreground-only's copy (§15.9).
	SidecarReceiverCoverageNone = "none"
)

// The control methods the remote backend calls, as their owners named them (cli's ur id and ur indexer). The js leg
// links none of this file.
const (
	sidecarControlMethodIdList           = "id.ls"
	sidecarControlMethodIndexerDesignate = "indexer.designate"
	sidecarControlMethodIndexerRetire    = "indexer.retire_scope"
)

var (
	// The Sidecar closed, or the call's context ended, before an answer.
	errSidecarMailUnavailable = errors.New("mail is unavailable: the sidecar closed or the call was cancelled")
	// An app host receives only while it is open and is never an indexer (owner decision 197).
	errSidecarMailReceiverLocal = errors.New("an app receives mail only while it is open, so it is never a mail indexer: designate an always-on device, a desktop ur daemon, a NAS or VPS")
)

// A call this host's core or daemon does not have yet: what it waits on, for a person and the report.
type sidecarMailNotLandedError struct {
	call string
}

func (self *sidecarMailNotLandedError) Error() string {
	return fmt.Sprintf("this sidecar cannot %s yet", self.call)
}

// The error for a call that waits on what call names.
func errSidecarMailNotLanded(call string) error {
	return &sidecarMailNotLandedError{call: call}
}

// One address as GetAliases lists it. json in the C ABI and js.
type SidecarAlias struct {
	// The address as mailcfg spells it.
	Address string `json:"address"`
	// admin, id, group, agent, burner or bounce (§15.8's forms).
	Kind string `json:"kind"`
	// Whether a receiver takes its mail: a record a reader honors whose creator policy still grants the listen, and
	// for a burner a live label (§15.9).
	Known bool `json:"known,omitempty"`
}

// A network's addresses.
type SidecarAliasList struct {
	exportedList[*SidecarAlias]
}

// An empty list.
func NewSidecarAliasList() *SidecarAliasList {
	return &SidecarAliasList{
		exportedList: *newExportedList[*SidecarAlias](),
	}
}

// A message an app composes, for Send (§15.10). json in the C ABI and js. Its fields are a reading: §18.5 names the
// type and not its fields, and these are what §15.10's submission takes; attachments come with MAIL-M9-01.
type SidecarDraft struct {
	// One of the network's addresses that policy grants this member's send (§15.10 step 1).
	From string `json:"from"`
	// The recipients, each an RFC 5321 address; at least one among them.
	To  *StringList `json:"to,omitempty"`
	Cc  *StringList `json:"cc,omitempty"`
	Bcc *StringList `json:"bcc,omitempty"`
	// The subject and the plain-text body, UTF-8.
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
}

// The recipients of draft, To then Cc then Bcc.
func (self *SidecarDraft) recipients() []string {
	recipients := []string{}
	for _, list := range []*StringList{self.To, self.Cc, self.Bcc} {
		if list != nil {
			recipients = append(recipients, list.getAll()...)
		}
	}
	return recipients
}

// What SidecarMail asks of its backend: the core's MailClient (sidecarMailCoreBackend), ur daemon's control methods
// (sidecarMailRemoteBackend), or a test's.
type sidecarMailBackend interface {
	aliases(ctx context.Context) ([]*SidecarAlias, error)
	// Queues draft, answering its send_id.
	send(ctx context.Context, draft *SidecarDraft) (string, error)
	setReceiverEnabled(ctx context.Context, enabled bool) error
	// One of the SidecarReceiverCoverage values.
	receiverCoverage(ctx context.Context) (string, error)
	// sidecar_mail_burner.go's calls.
	createBurner(ctx context.Context, site string) (*sidecarMailBurnerCreated, error)
	burn(ctx context.Context, address string) error
	vaultEntries(ctx context.Context) ([]*SidecarVaultEntry, error)
	usedAt(ctx context.Context, entryId sidecar.Id) ([]*SidecarUsedAt, error)
	oneTimeCodes(ctx context.Context, since time.Time) ([]*SidecarOneTimeCode, error)
	// sidecar_mail_send.go's calls: the core's check of the message Send would make of draft, and the outcomes of
	// the send a send_id names, each per recipient domain.
	checkSend(ctx context.Context, draft *SidecarDraft) (*sidecar.SmtpSendCheck, error)
	sendResult(ctx context.Context, sendId sidecar.Id) (*sidecar.SmtpSendResult, error)
}

// The mail component's factory: the core's MailClient, which attaches to the core's generations as the "mail"
// component (sidecar/mail_component.go) and runs this device's receiver where policy grants the listen, the receiver
// hosting API, or errSidecarComponentNotEnrolled while the core has no network or identity to make it for.
func newSidecarMailComponent(ctx context.Context, local *sidecar.Sidecar, settings *SidecarSettings) (sidecar.SidecarComponent, error) {
	client, err := local.Mail()
	if err != nil {
		// no network or identity yet: it waits for the enrollment, or for the keychain at a restart
		return nil, fmt.Errorf("%w: %w", errSidecarComponentNotEnrolled, err)
	}
	return client, nil
}

// One network's mail for an app. Safe for concurrent use.
type SidecarMail struct {
	ctx      context.Context
	cancel   context.CancelFunc
	backend  sidecarMailBackend
	settings *sidecarMailSettings
}

// A SidecarMail on backend until ctx ends or close.
func newSidecarMail(ctx context.Context, backend sidecarMailBackend, settings *sidecarMailSettings) *SidecarMail {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &SidecarMail{
		ctx:      cancelCtx,
		cancel:   cancel,
		backend:  backend,
		settings: settings,
	}
}

// Ends every call in flight, which answers MAIL_UNAVAILABLE. Safe to call again.
func (self *SidecarMail) close() {
	self.cancel()
}

// This Sidecar's mail (§18.5): on the local backend nil unless the settings named mail and the core's mail component
// is attached, and on the remote backend its daemon's.
func (self *Sidecar) GetMail() *SidecarMail {
	mail, err := traceWithReturn(func() *SidecarMail {
		local, remote := self.backends()
		existing, settings, backend, closed := func() (*SidecarMail, *sidecarMailSettings, sidecarMailBackend, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.mail, self.mailSettings, self.mailBackend, self.closed
		}()
		if closed {
			return nil
		}
		if existing != nil {
			return existing
		}
		if settings == nil {
			settings = defaultSidecarMailSettings()
		}
		if backend == nil {
			switch {
			case local != nil:
				client, ok := self.component(SidecarComponentMail).(*sidecar.MailClient)
				if !ok || client == nil {
					return nil
				}
				backend = &sidecarMailCoreBackend{client: client}
			case remote != nil:
				backend = &sidecarMailRemoteBackend{remote: remote}
			default:
				return nil
			}
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil
		}
		if self.mail == nil {
			self.mail = newSidecarMail(self.ctx, backend, settings)
		}
		return self.mail
	})
	if err != nil {
		return nil
	}
	return mail
}

// Runs call on a goroutine of its own within CallTimeout and answers callback once with what it answers, or with
// call's panic as the error.
func sidecarMailAnswer[T any](self *SidecarMail, call func(ctx context.Context) T, callback connect.ApiCallback[T]) {
	go func() {
		result, err := traceWithReturnError(func() (T, error) {
			ctx, cancel := context.WithTimeout(self.ctx, self.settings.CallTimeout)
			defer cancel()
			return call(ctx), nil
		})
		if callback != nil {
			callback.Result(result, err)
		}
	}()
}

// The code of err (the file's comment): "" for none.
func sidecarMailCodeOf(err error) string {
	var notLanded *sidecarMailNotLandedError
	var refusal *sidecarControlError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errSidecarMailUnavailable), errors.Is(err, sidecar.ErrSidecarClosed),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, errSidecarRemoteOffline), errors.Is(err, errSidecarRemoteClosed),
		errors.Is(err, sidecar.ErrMailUnbound), errors.Is(err, sidecar.ErrMailSendUnavailable),
		errors.Is(err, sidecar.ErrApprovalNoLookup), errors.As(err, &notLanded):
		return sidecarMailCodeUnavailable
	case errors.Is(err, errSidecarMailReceiverLocal):
		return sidecarMailCodeRefused
	case errors.Is(err, sidecar.ErrMailVaultNotConsented):
		return sidecarMailCodeNotConsented
	case errors.Is(err, sidecar.ErrMailNoMailbox), errors.Is(err, sidecar.ErrMailNoConfig):
		return sidecarMailCodeNoMailbox
	case errors.Is(err, sidecar.ErrMailClientInvalid), errors.Is(err, sidecar.ErrMailVaultEntryInvalid),
		errors.Is(err, sidecar.ErrMailVaultEntryAbsent), errors.Is(err, sidecar.ErrMailVaultEntryDeleted),
		errors.Is(err, sidecar.ErrMailSendInvalid), errors.Is(err, sidecar.ErrMailSendUnknown):
		return sidecarMailCodeInvalid
	case errors.As(err, &refusal):
		return sidecarMailRemoteCodeOf(refusal)
	}
	return sidecarMailCodeRefused
}

// The code of a daemon's refusal, which carries its handler's text and the daemon's own codes: a method the daemon
// does not have, a call with no network to act on (FAILED_PRECONDITION) and one past its time are unavailable; a
// handler's failure reads by the root's mail error its text names, the daemon writing the error's text and the sdk
// holding the same values; anything else is refused. A refusal's text goes nowhere but its Message.
func sidecarMailRemoteCodeOf(refusal *sidecarControlError) string {
	switch refusal.Code {
	case sidecarControlCodeMethodNotFound, sidecarControlCodeFailedPrecondition, sidecarControlCodeTimeout:
		return sidecarMailCodeUnavailable
	}
	for _, named := range []struct {
		err  error
		code string
	}{
		{sidecar.ErrMailSendUnavailable, sidecarMailCodeUnavailable},
		{sidecar.ErrMailUnbound, sidecarMailCodeUnavailable},
		{sidecar.ErrApprovalNoLookup, sidecarMailCodeUnavailable},
		{sidecar.ErrMailVaultNotConsented, sidecarMailCodeNotConsented},
		{sidecar.ErrMailNoMailbox, sidecarMailCodeNoMailbox},
		{sidecar.ErrMailNoConfig, sidecarMailCodeNoMailbox},
		{sidecar.ErrMailSendUnknown, sidecarMailCodeInvalid},
		{sidecar.ErrMailSendInvalid, sidecarMailCodeInvalid},
		{sidecar.ErrMailClientInvalid, sidecarMailCodeInvalid},
	} {
		if strings.Contains(refusal.Message, named.err.Error()) {
			return named.code
		}
	}
	return sidecarMailCodeRefused
}

// A SidecarResult for a call that answered value or err: sidecarResultOf's, its approval_pending:<id> included, with
// the file's code for a refusal that has none.
func sidecarMailResultOf(value string, err error) *SidecarResult {
	result := sidecarResultOf(value, err)
	if err != nil && result.Code == "" && !strings.HasPrefix(result.Value, sidecarApprovalPendingPrefix) {
		result.Code = sidecarMailCodeOf(err)
	}
	return result
}

// A refusal of a call whose arguments name nothing it can act on.
func sidecarMailInvalid(message string) *SidecarResult {
	return &SidecarResult{Code: sidecarMailCodeInvalid, Message: message}
}

// The call's context ended or the SidecarMail closed.
func (self *SidecarMail) callErr() error {
	if self.ctx.Err() != nil {
		return errSidecarMailUnavailable
	}
	return nil
}

// Every address record a reader honors (§15.8; `ur id ls`), each Known only while policy grants its creator the
// listen, read within ReadTimeout on the caller's thread. An empty list where none could be read.
func (self *SidecarMail) GetAliases() *SidecarAliasList {
	list, err := traceWithReturn(func() *SidecarAliasList {
		list := NewSidecarAliasList()
		if self.callErr() != nil {
			return list
		}
		ctx, cancel := context.WithTimeout(self.ctx, self.settings.ReadTimeout)
		defer cancel()
		aliases, err := self.backend.aliases(ctx)
		if err != nil {
			return list
		}
		for _, alias := range aliases {
			list.Add(alias)
		}
		return list
	})
	if err != nil {
		return NewSidecarAliasList()
	}
	return list
}

// Sends draft (§15.10): same-network recipients by deposit, internet ones through §14's outbox unless this host
// proves, the result naming its send_id, whose outcomes per recipient domain GetSendResult reads and mail_send
// announces as each changes, from the outbox too (sidecar_mail_send.go). An outward action, so a recipient no grant
// covers comes back approval_pending:<id>.
func (self *SidecarMail) Send(draft *SidecarDraft, callback SidecarResultCallback) {
	trace(func() {
		// the draft's lists are the caller's to reuse once Send returns
		draftCopy := sidecarMailCopyDraft(draft)
		sidecarMailAnswer(self, func(ctx context.Context) *SidecarResult {
			if err := self.callErr(); err != nil {
				return sidecarMailResultOf("", err)
			}
			if draftCopy == nil || len(draftCopy.recipients()) == 0 || draftCopy.From == "" {
				return sidecarMailInvalid("a message needs a From address and at least one recipient")
			}
			sendId, err := self.backend.send(ctx, draftCopy)
			return sidecarMailResultOf(sendId, err)
		}, connect.ApiCallback[*SidecarResult](callback))
	})
}

// A copy of list, nil for nil.
func sidecarMailCopyList(list *StringList) *StringList {
	if list == nil {
		return nil
	}
	copied := NewStringList()
	copied.addAll(list.getAll()...)
	return copied
}

// Files (enabled) or retires this Sidecar's daemon's IndexerSet for mail (§15.6, §7.9): a remote-mode app's paired
// daemon is an always-on receiver, and a person's confirmed act in the app's UI makes it this member's mail indexer.
// The local backend answers MAIL_REFUSED, since an app host receives only while open and is never an indexer (owner
// decision 197); its receiver still listens while the app is open, where policy grants it.
func (self *SidecarMail) SetReceiverEnabled(enabled bool, callback SidecarResultCallback) {
	trace(func() {
		sidecarMailAnswer(self, func(ctx context.Context) *SidecarResult {
			if err := self.callErr(); err != nil {
				return sidecarMailResultOf("", err)
			}
			return sidecarMailResultOf("", self.backend.setReceiverEnabled(ctx, enabled))
		}, connect.ApiCallback[*SidecarResult](callback))
	})
}

// indexer, foreground-only or none (§18.5's Coverage, §15.9), read within ReadTimeout on the caller's thread. It says
// nothing of sending, whose limits are CheckSend's and GetSendResult's. Where the backend cannot tell, foreground-only
// (the file's comment).
func (self *SidecarMail) GetReceiverCoverage() string {
	coverage, err := traceWithReturn(func() string {
		if self.callErr() != nil {
			return SidecarReceiverCoverageForegroundOnly
		}
		ctx, cancel := context.WithTimeout(self.ctx, self.settings.ReadTimeout)
		defer cancel()
		coverage, err := self.backend.receiverCoverage(ctx)
		if err != nil {
			return SidecarReceiverCoverageForegroundOnly
		}
		switch coverage {
		case SidecarReceiverCoverageIndexer, SidecarReceiverCoverageForegroundOnly, SidecarReceiverCoverageNone:
			return coverage
		}
		// a value this sdk does not know claims no indexer
		return SidecarReceiverCoverageForegroundOnly
	})
	if err != nil {
		return SidecarReceiverCoverageForegroundOnly
	}
	return coverage
}

// The word ur id ls uses for a kind.
func sidecarMailAliasKind(kind protocol.MailAddressKind) string {
	switch kind {
	case protocol.MailAddressKind_MAIL_ADDRESS_ADMIN:
		return "admin"
	case protocol.MailAddressKind_MAIL_ADDRESS_ID:
		return "id"
	case protocol.MailAddressKind_MAIL_ADDRESS_GROUP:
		return "group"
	case protocol.MailAddressKind_MAIL_ADDRESS_AGENT:
		return "agent"
	case protocol.MailAddressKind_MAIL_ADDRESS_BURNER:
		return "burner"
	case protocol.MailAddressKind_MAIL_ADDRESS_BOUNCE:
		return "bounce"
	}
	return "unknown"
}

// sidecarMailBackend over the core's MailClient. A call whose root call is not on MailClient yet answers
// errSidecarMailNotLanded, except the vault's, which it takes as soon as MailClient has MailVault's methods of those
// names (sidecarMailCoreVault).
type sidecarMailCoreBackend struct {
	client *sidecar.MailClient
}

// MailVault's reads, which MAIL-M8-02 puts on MailClient under the same names (MailClient.VaultEntries,
// MailClient.UsedAt): taken here as soon as MailClient has them.
type sidecarMailCoreVault interface {
	VaultEntries(ctx context.Context) ([]*protocol.VaultEntry, error)
	UsedAt(ctx context.Context, entryId sidecar.Id) ([]*protocol.VaultUsedAt, error)
}

// See sidecarMailBackend.
func (self *sidecarMailCoreBackend) aliases(ctx context.Context) ([]*SidecarAlias, error) {
	lookups, err := self.client.Aliases(ctx)
	if err != nil {
		return nil, err
	}
	aliases := []*SidecarAlias{}
	for _, lookup := range lookups {
		aliases = append(aliases, &SidecarAlias{Address: lookup.Address, Kind: sidecarMailAliasKind(lookup.Kind), Known: lookup.Known})
	}
	return aliases, nil
}

// Submits through the same core gate as the daemon's mail.send.
func (self *sidecarMailCoreBackend) send(ctx context.Context, draft *SidecarDraft) (string, error) {
	if self.client == nil {
		return "", sidecar.ErrMailUnbound
	}
	result, err := self.client.SubmitSend(ctx, sidecarMailSendArgsOf(draft))
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", errSidecarMailSendEmpty
	}
	return sidecarMailSubmitted(result)
}

// The public send id. A recorded approval replay names the committed effect alone. A fresh
// send also names recipients, of whom at least one must have been delivered or queued.
func sidecarMailSubmitted(result *sidecar.MailSendResult) (string, error) {
	if result.SendId != (sidecar.Id{}) {
		if len(result.Recipients) == 0 {
			return result.SendId.String(), nil
		}
		for _, recipient := range result.Recipients {
			if recipient != nil && (recipient.Outcome == sidecar.MailSendDelivered || recipient.Outcome == sidecar.MailSendQueued) {
				return result.SendId.String(), nil
			}
		}
	}
	return "", errors.New("no recipient accepted the message")
}

// See sidecarMailBackend: an app host is never a mail indexer (owner decision 197).
func (self *sidecarMailCoreBackend) setReceiverEnabled(ctx context.Context, enabled bool) error {
	return errSidecarMailReceiverLocal
}

// See sidecarMailBackend: MailClient.ReceiverCoverage is MAIL-M8-03's.
func (self *sidecarMailCoreBackend) receiverCoverage(ctx context.Context) (string, error) {
	return "", errSidecarMailNotLanded("tell its receiver coverage (MailClient.ReceiverCoverage)")
}

// sidecarMailBackend over ur daemon's control methods (sidecar_backend.go). The daemon has methods for addresses,
// designations and sends (sidecar_mail_send.go's check and result among them) so far, so the rest answer
// errSidecarMailNotLanded.
type sidecarMailRemoteBackend struct {
	remote sidecarBackend
}

// The network the pairing bound, as a control call's network argument.
func (self *sidecarMailRemoteBackend) network() string {
	if named, ok := self.remote.(interface{ network() string }); ok {
		return named.network()
	}
	return ""
}

// id.ls's params and reply, as cli's ur id names them.
type sidecarControlIdParams struct {
	Network string `json:"network,omitempty"`
}

type sidecarControlIdAddress struct {
	Address string `json:"address"`
	Kind    string `json:"kind"`
	Known   bool   `json:"known"`
}

type sidecarControlIdListReply struct {
	Addresses []*sidecarControlIdAddress `json:"addresses"`
}

// indexer.designate's and indexer.retire_scope's params, as cli's ur indexer names them: the daemon reads them
// strictly, so nothing else rides along.
type sidecarControlIndexerParams struct {
	Scope    int32   `json:"scope"`
	Services []int32 `json:"services,omitempty"`
}

// See sidecarMailBackend.
func (self *sidecarMailRemoteBackend) aliases(ctx context.Context) ([]*SidecarAlias, error) {
	reply := &sidecarControlIdListReply{}
	if err := self.remote.call(ctx, sidecarControlMethodIdList, &sidecarControlIdParams{Network: self.network()}, false, reply); err != nil {
		return nil, err
	}
	aliases := []*SidecarAlias{}
	for _, address := range reply.Addresses {
		if address == nil {
			continue
		}
		aliases = append(aliases, &SidecarAlias{Address: address.Address, Kind: address.Kind, Known: address.Known})
	}
	return aliases, nil
}

// See sidecarMailBackend: ur daemon's mail.send on the pairing's network (the file's comment), sent unconfirmed: I-8's
// approval is the daemon's gate's, which no word of the app's stands in for. A send the gate holds answers
// *sidecar.ApprovalPendingError, approval_pending:<id> to the app, and one that went its send_id; a null answer, or one
// naming neither, is none.
func (self *sidecarMailRemoteBackend) send(ctx context.Context, draft *SidecarDraft) (string, error) {
	var reply *sidecarControlMailSendReply
	if err := self.remote.call(ctx, sidecarControlMethodMailSend, sidecarControlMailDraftOf(self.network(), draft), false, &reply); err != nil {
		return "", sidecarMailRemoteErr(err, "send mail through its daemon")
	}
	switch {
	case reply == nil:
		return "", errSidecarMailSendEmpty
	case reply.ApprovalId != nil:
		return "", &sidecar.ApprovalPendingError{ApprovalId: *reply.ApprovalId}
	case reply.SendId == nil:
		return "", errSidecarMailSendEmpty
	}
	result := &sidecar.MailSendResult{SendId: *reply.SendId}
	for _, recipient := range reply.Recipients {
		if recipient == nil {
			result.Recipients = append(result.Recipients, nil)
			continue
		}
		result.Recipients = append(result.Recipients, &sidecar.MailSendRecipient{Outcome: sidecar.MailSendOutcome(recipient.Outcome)})
	}
	return sidecarMailSubmitted(result)
}

// See sidecarMailBackend: the daemon's IndexerSet for mail at the member scope (the file's comment), confirmed, since
// the bound call is the person's act in the app's UI.
func (self *sidecarMailRemoteBackend) setReceiverEnabled(ctx context.Context, enabled bool) error {
	scope := int32(protocol.IndexerScope_INDEXER_SCOPE_MEMBER)
	if enabled {
		params := &sidecarControlIndexerParams{Scope: scope, Services: []int32{int32(protocol.IndexerService_INDEXER_SERVICE_MAIL)}}
		return self.remote.call(ctx, sidecarControlMethodIndexerDesignate, params, true, nil)
	}
	return self.remote.call(ctx, sidecarControlMethodIndexerRetire, &sidecarControlIndexerParams{Scope: scope}, true, nil)
}

// See sidecarMailBackend: §5.8 has no coverage method yet.
func (self *sidecarMailRemoteBackend) receiverCoverage(ctx context.Context) (string, error) {
	return "", errSidecarMailNotLanded("tell its daemon's receiver coverage (no control method yet)")
}

var (
	_ sidecarMailBackend = (*sidecarMailCoreBackend)(nil)
	_ sidecarMailBackend = (*sidecarMailRemoteBackend)(nil)
	// the vault interface has MailVault's shape, so MAIL-M8-02's MailClient methods of those names satisfy it
	_ sidecarMailCoreVault = (*sidecar.MailVault)(nil)
)
