//go:build !ios_extension && !js

// SidecarMail's burner, vault and one-time-code calls (§15.9's app seams, §15.8, §18.5; SDK-M8-04): CreateBurner(site),
// Burn(address), GetVaultEntries, GetUsedAt(entry) and GetOneTimeCodes(since), each a thin mapping onto the core's
// MailClient, which owns every rule:
//
//	burner     a burner is <name>@<blabel>.<net>.<root> under the wildcard MX, created only once the core's gate passes
//	           (an I-8 outward action, approval_pending:<id> until then), with a vault entry for its site; before the
//	           first burner the core hands back §15.9's foreground-only copy while coverage is foreground-only or none,
//	           which CoverageCopy carries for the app to show, and it never refuses for coverage (owner decision 12).
//	           MailClient.CreateBurner and MailClient.Burn are MAIL-M8-01's, not on sidecar yet: both answer
//	           MAIL_UNAVAILABLE until they land
//	burn       local (owner decision 32): receivers answer 550 5.1.1 for the label from then on, the label is never
//	           reissued and its vault entry reads burned. Burn takes the burner's address, which the vault entry and
//	           CreateBurner's answer carry (a reading of §15.9's Burn(id))
//	vault      only the owner's consented devices read entries or their Used at (§8.5, §15.8): the core refuses any
//	           other device, which answers MAIL_NOT_CONSENTED. An entry crosses with its password, for the person's
//	           own autofill on that device (§18.15's app work), and without its TOTP secret, whose codes the core
//	           computes (GetOneTimeCodes); no secret is logged (I-7). MailClient.VaultEntries and MailClient.UsedAt
//	           are MailVault's methods under those names (MAIL-M8-02), which MailClient does not carry yet: the local
//	           backend takes them as soon as it does (sidecarMailCoreVault), and answers MAIL_UNAVAILABLE until then
//	codes      GetOneTimeCodes(since) merges the vault's TOTP and the mailbox index's one_time_code (MAIL-M8-03's
//	           MailClient.OneTimeCodes), newest first; MAIL_UNAVAILABLE until it lands
//
// The remote backend's daemon has no burner, vault or code method yet (§5.8), so each answers MAIL_UNAVAILABLE there.
// Safe for concurrent use.

package sdk

import (
	"context"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
)

// What a backend's CreateBurner made.
type sidecarMailBurnerCreated struct {
	address string
	entryId *Id
	// §15.9's foreground-only copy for a first burner while coverage is foreground-only or none, else empty
	coverageCopy string
}

// One CreateBurner's terminal answer. json in the C ABI and js.
type SidecarBurner struct {
	// The burner was made.
	Ok bool `json:"ok"`
	// <name>@<blabel>.<net>.<root>.
	Address string `json:"address,omitempty"`
	// The vault entry made for its site.
	EntryId *Id `json:"entry_id,omitempty"`
	// §15.9's foreground-only copy, for the app to show with the first burner while coverage is foreground-only or
	// none; empty otherwise. It never stops the burner (owner decision 12).
	CoverageCopy string `json:"coverage_copy,omitempty"`
	// approval_pending:<id> while the core's gate holds the creation for a person (§18.5's gate).
	Value string `json:"value,omitempty"`
	// A MAIL_ code, for a refusal.
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Answers a CreateBurner once.
type SidecarBurnerCallback connect.ApiCallback[*SidecarBurner]

// One vault entry (§15.8's VaultEntry) as an app reads it. json in the C ABI and js.
type SidecarVaultEntry struct {
	EntryId *Id `json:"entry_id"`
	// The id, group or burner address it was made for.
	Address  string `json:"address,omitempty"`
	Title    string `json:"title,omitempty"`
	Username string `json:"username,omitempty"`
	// The person's own password, for autofill on this consented device (the file's comment). Never logged.
	Password string      `json:"password,omitempty"`
	Urls     *StringList `json:"urls,omitempty"`
	Notes    string      `json:"notes,omitempty"`
	// The entry holds a TOTP secret, whose codes GetOneTimeCodes answers; the secret stays in the core.
	HasTotp       bool  `json:"has_totp,omitempty"`
	CreatedMillis int64 `json:"created_millis,omitempty"`
	UpdatedMillis int64 `json:"updated_millis,omitempty"`
	// live or burned (§15.9).
	State string `json:"state"`
}

// The vault's entries.
type SidecarVaultEntryList struct {
	exportedList[*SidecarVaultEntry]
}

// An empty list.
func NewSidecarVaultEntryList() *SidecarVaultEntryList {
	return &SidecarVaultEntryList{
		exportedList: *newExportedList[*SidecarVaultEntry](),
	}
}

// One GetVaultEntries call's terminal answer. json in the C ABI and js.
type SidecarVaultEntries struct {
	Ok      bool                   `json:"ok"`
	Entries *SidecarVaultEntryList `json:"entries,omitempty"`
	Code    string                 `json:"code,omitempty"`
	Message string                 `json:"message,omitempty"`
}

// Answers a GetVaultEntries call once.
type SidecarVaultEntriesCallback connect.ApiCallback[*SidecarVaultEntries]

// One registrable domain an entry's address got mail from (§15.8's used_at). json in the C ABI and js.
type SidecarUsedAt struct {
	// The sender's eTLD+1.
	RegistrableDomain string `json:"registrable_domain"`
	FirstMillis       int64  `json:"first_millis"`
	LastMillis        int64  `json:"last_millis"`
}

// An entry's Used at.
type SidecarUsedAtList struct {
	exportedList[*SidecarUsedAt]
}

// An empty list.
func NewSidecarUsedAtList() *SidecarUsedAtList {
	return &SidecarUsedAtList{
		exportedList: *newExportedList[*SidecarUsedAt](),
	}
}

// One GetUsedAt call's terminal answer. json in the C ABI and js.
type SidecarUsedAtResult struct {
	Ok      bool               `json:"ok"`
	UsedAt  *SidecarUsedAtList `json:"used_at,omitempty"`
	Code    string             `json:"code,omitempty"`
	Message string             `json:"message,omitempty"`
}

// Answers a GetUsedAt call once.
type SidecarUsedAtCallback connect.ApiCallback[*SidecarUsedAtResult]

// One one-time code (§15.9's GetOneTimeCodes). json in the C ABI and js.
type SidecarOneTimeCode struct {
	Code string `json:"code"`
	// vault, a TOTP the core computed, or mail, a code the index took from a message.
	Source string `json:"source"`
	// The vault entry, or the message's address, it belongs to.
	EntryId *Id    `json:"entry_id,omitempty"`
	Address string `json:"address,omitempty"`
	// When it was made or received, and when it stops being good, 0 for unknown.
	TimeMillis    int64 `json:"time_millis,omitempty"`
	ExpiresMillis int64 `json:"expires_millis,omitempty"`
}

// One-time codes, newest first.
type SidecarOneTimeCodeList struct {
	exportedList[*SidecarOneTimeCode]
}

// An empty list.
func NewSidecarOneTimeCodeList() *SidecarOneTimeCodeList {
	return &SidecarOneTimeCodeList{
		exportedList: *newExportedList[*SidecarOneTimeCode](),
	}
}

// One GetOneTimeCodes call's terminal answer. json in the C ABI and js.
type SidecarOneTimeCodes struct {
	Ok      bool                    `json:"ok"`
	Codes   *SidecarOneTimeCodeList `json:"codes,omitempty"`
	Code    string                  `json:"code,omitempty"`
	Message string                  `json:"message,omitempty"`
}

// Answers a GetOneTimeCodes call once.
type SidecarOneTimeCodesCallback connect.ApiCallback[*SidecarOneTimeCodes]

// Makes a burner for site, a registrable domain or a url the person signs up at (§15.9): the answer names its address
// and vault entry, and for a first burner while coverage is foreground-only or none the copy to show. An outward
// action, so it comes back approval_pending:<id> in Value while the gate holds it.
func (self *SidecarMail) CreateBurner(site string, callback SidecarBurnerCallback) {
	trace(func() {
		sidecarMailAnswer(self, func(ctx context.Context) *SidecarBurner {
			refuse := func(result *SidecarResult) *SidecarBurner {
				return &SidecarBurner{Value: result.Value, Code: result.Code, Message: result.Message}
			}
			if err := self.callErr(); err != nil {
				return refuse(sidecarMailResultOf("", err))
			}
			if site == "" {
				return refuse(sidecarMailInvalid("a burner is made for a site"))
			}
			created, err := self.backend.createBurner(ctx, site)
			if err != nil {
				return refuse(sidecarMailResultOf("", err))
			}
			return &SidecarBurner{Ok: true, Address: created.address, EntryId: created.entryId, CoverageCopy: created.coverageCopy}
		}, connect.ApiCallback[*SidecarBurner](callback))
	})
}

// Burns the burner at address (§15.9): receivers answer 550 5.1.1 for its label from then on, the label is never
// reissued, and its vault entry reads burned. Local to the network's devices: DNS and the server change only when a
// replying burner's selector retires.
func (self *SidecarMail) Burn(address string, callback SidecarResultCallback) {
	trace(func() {
		sidecarMailAnswer(self, func(ctx context.Context) *SidecarResult {
			if err := self.callErr(); err != nil {
				return sidecarMailResultOf("", err)
			}
			if address == "" {
				return sidecarMailInvalid("burn names a burner's address")
			}
			return sidecarMailResultOf("", self.backend.burn(ctx, address))
		}, connect.ApiCallback[*SidecarResult](callback))
	})
}

// Every vault entry that stands, by creation time (§15.8), on an owner's consented device alone.
func (self *SidecarMail) GetVaultEntries(callback SidecarVaultEntriesCallback) {
	trace(func() {
		sidecarMailAnswer(self, func(ctx context.Context) *SidecarVaultEntries {
			refuse := func(err error) *SidecarVaultEntries {
				return &SidecarVaultEntries{Code: sidecarMailCodeOf(err), Message: err.Error()}
			}
			if err := self.callErr(); err != nil {
				return refuse(err)
			}
			entries, err := self.backend.vaultEntries(ctx)
			if err != nil {
				return refuse(err)
			}
			list := NewSidecarVaultEntryList()
			list.addAll(entries...)
			return &SidecarVaultEntries{Ok: true, Entries: list}
		}, connect.ApiCallback[*SidecarVaultEntries](callback))
	})
}

// The registrable domains entryId's address got mail from, with first and last times (§15.8's used_at).
func (self *SidecarMail) GetUsedAt(entryId *Id, callback SidecarUsedAtCallback) {
	trace(func() {
		sidecarMailAnswer(self, func(ctx context.Context) *SidecarUsedAtResult {
			refuse := func(err error) *SidecarUsedAtResult {
				return &SidecarUsedAtResult{Code: sidecarMailCodeOf(err), Message: err.Error()}
			}
			if err := self.callErr(); err != nil {
				return refuse(err)
			}
			if entryId == nil {
				return &SidecarUsedAtResult{Code: sidecarMailCodeInvalid, Message: "Used at names a vault entry"}
			}
			usedAt, err := self.backend.usedAt(ctx, sidecar.Id(entryId.id))
			if err != nil {
				return refuse(err)
			}
			list := NewSidecarUsedAtList()
			list.addAll(usedAt...)
			return &SidecarUsedAtResult{Ok: true, UsedAt: list}
		}, connect.ApiCallback[*SidecarUsedAtResult](callback))
	})
}

// The one-time codes made or received since sinceMillis, 0 for all the core keeps, newest first: the vault's TOTP and
// the codes the index took from mail (§15.9).
func (self *SidecarMail) GetOneTimeCodes(sinceMillis int64, callback SidecarOneTimeCodesCallback) {
	trace(func() {
		sidecarMailAnswer(self, func(ctx context.Context) *SidecarOneTimeCodes {
			refuse := func(err error) *SidecarOneTimeCodes {
				return &SidecarOneTimeCodes{Code: sidecarMailCodeOf(err), Message: err.Error()}
			}
			if err := self.callErr(); err != nil {
				return refuse(err)
			}
			var since time.Time
			if 0 < sinceMillis {
				since = time.UnixMilli(sinceMillis)
			}
			codes, err := self.backend.oneTimeCodes(ctx, since)
			if err != nil {
				return refuse(err)
			}
			list := NewSidecarOneTimeCodeList()
			list.addAll(codes...)
			return &SidecarOneTimeCodes{Ok: true, Codes: list}
		}, connect.ApiCallback[*SidecarOneTimeCodes](callback))
	})
}

// The bound entry of the core's, without its TOTP secret or earlier passwords.
func sidecarVaultEntryOf(entry *protocol.VaultEntry) *SidecarVaultEntry {
	bound := &SidecarVaultEntry{
		Address:       entry.GetAddress(),
		Title:         entry.GetTitle(),
		Username:      entry.GetUsername(),
		Password:      entry.GetPassword(),
		Urls:          NewStringList(),
		Notes:         entry.GetNotes(),
		HasTotp:       0 < len(entry.GetTotp().GetSecret()),
		CreatedMillis: sidecarMailMillisOf(entry.GetCreated()),
		UpdatedMillis: sidecarMailMillisOf(entry.GetUpdated()),
		State:         sidecarVaultEntryStateOf(entry.GetState()),
	}
	if entryId, err := sidecar.IdFromBytes(entry.GetEntryId()); err == nil {
		bound.EntryId = newId([16]byte(entryId))
	}
	bound.Urls.addAll(entry.GetUrls()...)
	return bound
}

// A state's word: live, burned, or unknown for one this sdk does not know.
func sidecarVaultEntryStateOf(state protocol.VaultEntryState) string {
	switch state {
	case protocol.VaultEntryState_VAULT_ENTRY_LIVE:
		return "live"
	case protocol.VaultEntryState_VAULT_ENTRY_BURNED:
		return "burned"
	}
	return "unknown"
}

// Milliseconds since the epoch as an int64, the largest for one past it.
func sidecarMailMillisOf(millis uint64) int64 {
	return int64(min(millis, uint64(1<<63-1)))
}

// See sidecarMailBackend: MailClient.CreateBurner is MAIL-M8-01's.
func (self *sidecarMailCoreBackend) createBurner(ctx context.Context, site string) (*sidecarMailBurnerCreated, error) {
	return nil, errSidecarMailNotLanded("make burners (MailClient.CreateBurner)")
}

// See sidecarMailBackend: MailClient.Burn is MAIL-M8-01's.
func (self *sidecarMailCoreBackend) burn(ctx context.Context, address string) error {
	return errSidecarMailNotLanded("burn burners (MailClient.Burn)")
}

// See sidecarMailBackend: MailVault's VaultEntries, once MailClient carries it.
func (self *sidecarMailCoreBackend) vaultEntries(ctx context.Context) ([]*SidecarVaultEntry, error) {
	vault, ok := any(self.client).(sidecarMailCoreVault)
	if !ok {
		return nil, errSidecarMailNotLanded("read vault entries (MailClient.VaultEntries)")
	}
	return sidecarVaultEntriesOf(ctx, vault)
}

// The bound entries vault reads.
func sidecarVaultEntriesOf(ctx context.Context, vault sidecarMailCoreVault) ([]*SidecarVaultEntry, error) {
	entries, err := vault.VaultEntries(ctx)
	if err != nil {
		return nil, err
	}
	bound := []*SidecarVaultEntry{}
	for _, entry := range entries {
		bound = append(bound, sidecarVaultEntryOf(entry))
	}
	return bound, nil
}

// See sidecarMailBackend: MailVault's UsedAt, once MailClient carries it.
func (self *sidecarMailCoreBackend) usedAt(ctx context.Context, entryId sidecar.Id) ([]*SidecarUsedAt, error) {
	vault, ok := any(self.client).(sidecarMailCoreVault)
	if !ok {
		return nil, errSidecarMailNotLanded("read Used at (MailClient.UsedAt)")
	}
	return sidecarUsedAtOf(ctx, vault, entryId)
}

// The bound Used at vault reads for entryId.
func sidecarUsedAtOf(ctx context.Context, vault sidecarMailCoreVault, entryId sidecar.Id) ([]*SidecarUsedAt, error) {
	usedAt, err := vault.UsedAt(ctx, entryId)
	if err != nil {
		return nil, err
	}
	bound := []*SidecarUsedAt{}
	for _, use := range usedAt {
		bound = append(bound, &SidecarUsedAt{
			RegistrableDomain: use.GetRegistrableDomain(),
			FirstMillis:       sidecarMailMillisOf(use.GetFirst()),
			LastMillis:        sidecarMailMillisOf(use.GetLast()),
		})
	}
	return bound, nil
}

// See sidecarMailBackend: MailClient.OneTimeCodes is MAIL-M8-03's.
func (self *sidecarMailCoreBackend) oneTimeCodes(ctx context.Context, since time.Time) ([]*SidecarOneTimeCode, error) {
	return nil, errSidecarMailNotLanded("read one-time codes (MailClient.OneTimeCodes)")
}

// See sidecarMailBackend: no control method yet.
func (self *sidecarMailRemoteBackend) createBurner(ctx context.Context, site string) (*sidecarMailBurnerCreated, error) {
	return nil, errSidecarMailNotLanded("make burners through its daemon (no control method yet)")
}

// See sidecarMailBackend: no control method yet.
func (self *sidecarMailRemoteBackend) burn(ctx context.Context, address string) error {
	return errSidecarMailNotLanded("burn burners through its daemon (no control method yet)")
}

// See sidecarMailBackend: no control method reaches a vault, and none may for an agent (§15.8).
func (self *sidecarMailRemoteBackend) vaultEntries(ctx context.Context) ([]*SidecarVaultEntry, error) {
	return nil, errSidecarMailNotLanded("read vault entries through its daemon (no control method yet)")
}

// See sidecarMailBackend: no control method yet.
func (self *sidecarMailRemoteBackend) usedAt(ctx context.Context, entryId sidecar.Id) ([]*SidecarUsedAt, error) {
	return nil, errSidecarMailNotLanded("read Used at through its daemon (no control method yet)")
}

// See sidecarMailBackend: no control method yet.
func (self *sidecarMailRemoteBackend) oneTimeCodes(ctx context.Context, since time.Time) ([]*SidecarOneTimeCode, error) {
	return nil, errSidecarMailNotLanded("read one-time codes through its daemon (no control method yet)")
}
