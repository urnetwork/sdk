//go:build !ios_extension

// The crypto invoice's controller: the one state machine behind every app's
// invoice screen (mmm/ZCASH-CONTRACT.md §4.4). It starts an invoice, counts the quote
// down, re-quotes it, polls its status while the screen is visible, and keeps
// it on the device so the next launch can resume it. Each app only renders
// GetState and calls the actions. The TypeScript twin is
// sdk/js/src/utils/crypto_invoice.ts, with the same behavior and the same
// tests on a fake clock and a fake api.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/connect/v2026"
)

// The invoice's phases (CryptoInvoiceState.Phase).
const (
	// no invoice on screen: none was started, or it was dismissed
	CryptoInvoicePhaseNone = "none"
	// StartInvoice is waiting for the server's quote
	CryptoInvoicePhaseStarting = "starting"
	// an invoice that is not paid yet: Status says whether it waits for a
	// payment, confirms one, has expired or is in review
	CryptoInvoicePhaseOpen = "open"
	// credited
	CryptoInvoicePhasePaid = "paid"
	// the invoice could not be started (ErrorCode, ErrorMessage), or the
	// server no longer knows it (Status CryptoPaymentStatusUnknown)
	CryptoInvoicePhaseFailed = "failed"
)

const (
	// the signed-in network's saved invoice, the invoices bought without a
	// sign-in (one file each) and the last payment method, in the local-state
	// directory, which a sign-out clears
	cryptoInvoiceFileName             = ".crypto_invoice"
	cryptoInvoiceIntentFileNamePrefix = ".crypto_invoice.intent."
	cryptoPaymentMethodFileName       = ".payment_method"
	// a saved invoice older than this is dropped instead of resumed
	cryptoInvoiceMaxAge = 7 * 24 * time.Hour
	// how often SecondsLeft is recomputed while the screen is visible
	cryptoInvoiceTickInterval = time.Second
	// the local times a record keeps, to the millisecond so a resumed
	// countdown lands on the same second
	cryptoInvoiceLocalTimeLayout = "2006-01-02T15:04:05.000Z07:00"
)

// The status poll cadence while started: often while a payment may be on its
// way, slower once the quote expired (a late payment still credits), slowest
// in review, where an operator acts. A paid or unknown invoice is never
// polled again.
const (
	cryptoInvoiceActivePollInterval  = 5 * time.Second
	cryptoInvoiceExpiredPollInterval = 30 * time.Second
	cryptoInvoiceReviewPollInterval  = 60 * time.Second
)

// Fires whenever GetState would return something new: a phase, a status, the
// countdown's second, a requote starting or ending. The app re-reads GetState.
type CryptoInvoiceListener interface {
	CryptoInvoiceChanged()
}

// What an invoice screen renders. Amounts are exact decimal strings in the
// coin; show them as they are, beside Ticker.
type CryptoInvoiceState struct {
	// one of the CryptoInvoicePhase* values
	Phase       string `json:"phase"`
	IntentId    string `json:"intent_id"`
	Asset       string `json:"asset"`
	Plan        string `json:"plan"`
	ItemId      string `json:"item_id"`
	Delivery    string `json:"delivery"`
	NetworkName string `json:"network_name"`
	// the invoice's own receive address
	Address      string `json:"address"`
	Amount       string `json:"amount"`
	AmountAtomic string `json:"amount_atomic"`
	// the coin's ticker, CryptoAssetTicker(Asset)
	Ticker string `json:"ticker"`
	// the server's payment uri, for the QR code and "Open in wallet". Empty
	// when it does not pass CheckCryptoPaymentUri against Address, or when a
	// quote changed elsewhere and the uri no longer pays the amount: show the
	// address and the amount to send by hand then.
	Uri string `json:"uri"`
	// usdc_solana only: the memo for a transfer sent by hand
	Memo     string  `json:"memo"`
	PriceUsd float64 `json:"price_usd"`
	RateUsd  float64 `json:"rate_usd"`
	// when the quote ends, RFC 3339 in the server's clock
	ExpiresAt string `json:"expires_at"`
	// the quote's seconds left, rounded up; 0 once it expired. It counts the
	// quote's own duration from when the quote arrived, so a device clock
	// that is off does not shorten or lengthen it.
	SecondsLeft int `json:"seconds_left"`
	// the server's CryptoPaymentStatus* value; "" for a resumed invoice
	// until its first status poll answers
	Status       string `json:"status"`
	ReviewReason string `json:"review_reason"`
	Received     string `json:"received"`
	// how much more to send, while Status is seen
	Remaining             string `json:"remaining"`
	Confirmations         int    `json:"confirmations"`
	ConfirmationsRequired int    `json:"confirmations_required"`
	// the balance code a paid invoice bought, for delivery "code"
	BalanceCode string `json:"balance_code"`
	// a requote is in flight
	Refreshing bool `json:"refreshing"`
	// the last start or requote failure: a PurchaseErrorCode* value (for
	// example PurchaseErrorCodeAssetUnavailable), "" for a transport failure
	// or an answer that could not be read, with the server's or the
	// transport's message
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

// A saved invoice: the network it was started on, the invoice and its status
// token, and the quote as last shown. The status result carries no address
// and no uri, so without the quote a resumed invoice could not be paid until
// it was re-quoted. An account invoice is saved in the signed-in network's
// slot (.crypto_invoice). One bought without a sign-in (delivery network or
// code) belongs to whoever holds the device, not to a network, and is saved in
// a file of its own, so nothing else ever overwrites it: for a code it is the
// only way to the code.
type cryptoInvoiceRecord struct {
	NetworkId   string `json:"network_id"`
	IntentId    string `json:"intent_id"`
	StatusToken string `json:"status_token"`
	Asset       string `json:"asset"`
	Plan        string `json:"plan,omitempty"`
	ItemId      string `json:"item_id,omitempty"`
	// the local time the invoice was first saved; the 7-day cap counts from
	// here
	CreatedAt string `json:"created_at"`

	Delivery     string  `json:"delivery,omitempty"`
	NetworkName  string  `json:"network_name,omitempty"`
	Address      string  `json:"address,omitempty"`
	Amount       string  `json:"amount,omitempty"`
	AmountAtomic string  `json:"amount_atomic,omitempty"`
	Uri          string  `json:"uri,omitempty"`
	Memo         string  `json:"memo,omitempty"`
	PriceUsd     float64 `json:"price_usd,omitempty"`
	RateUsd      float64 `json:"rate_usd,omitempty"`
	// the quote's start and end in the server's clock, and the local time it
	// arrived: the countdown runs the quote's own duration from its arrival
	QuotedAt   string `json:"quoted_at,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	ReceivedAt string `json:"received_at,omitempty"`

	// balance-code invoices only: the last status and the code once paid
	Status      string `json:"status,omitempty"`
	BalanceCode string `json:"balance_code,omitempty"`
	// invoices bought without a sign-in only: a Dismiss hid it while it might
	// still pay
	Hidden bool `json:"hidden,omitempty"`
}

func (self *cryptoInvoiceRecord) isCode() bool {
	return self.Delivery == CryptoPaymentDeliveryCode
}

// Bought without a sign-in (/buy-data): the data goes to a named network or to
// a balance code, so the invoice belongs to whoever holds the device, not to
// the signed-in network.
func (self *cryptoInvoiceRecord) ownedByDevice() bool {
	return self.Delivery == CryptoPaymentDeliveryNetwork || self.Delivery == CryptoPaymentDeliveryCode
}

// The server's intent ids are uuids. Anything else never names a file.
func validCryptoInvoiceIntentId(intentId string) bool {
	if intentId == "" || 64 < len(intentId) {
		return false
	}
	for _, c := range intentId {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}

// Keeps the saved invoices and the payment method as 0600 files in a
// local-state directory, or in memory when there is none.
type cryptoInvoiceStorage struct {
	dir string

	slot          *cryptoInvoiceRecord
	owned         map[string]*cryptoInvoiceRecord
	paymentMethod string
}

func readCryptoInvoiceRecord(path string) *cryptoInvoiceRecord {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	record := &cryptoInvoiceRecord{}
	if err := json.Unmarshal(data, record); err != nil {
		return nil
	}
	return record
}

func (self *cryptoInvoiceStorage) writeRecord(name string, record *cryptoInvoiceRecord) {
	data, err := json.Marshal(record)
	if err == nil {
		err = writeCryptoInvoiceFile(self.dir, name, data)
	}
	if err != nil {
		glog.V(1).Infof("[crypto]could not save the invoice: %s\n", err)
	}
}

func (self *cryptoInvoiceStorage) removeFile(name string) {
	err := os.Remove(filepath.Join(self.dir, name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		glog.V(1).Infof("[crypto]could not clear the invoice: %s\n", err)
	}
}

// The signed-in network's slot, or nil.
func (self *cryptoInvoiceStorage) loadSlot() *cryptoInvoiceRecord {
	if self.dir == "" {
		if self.slot == nil {
			return nil
		}
		record := *self.slot
		return &record
	}
	return readCryptoInvoiceRecord(filepath.Join(self.dir, cryptoInvoiceFileName))
}

// The device's invoice saved for an intent (one bought without a sign-in), or
// nil.
func (self *cryptoInvoiceStorage) loadOwned(intentId string) *cryptoInvoiceRecord {
	if !validCryptoInvoiceIntentId(intentId) {
		return nil
	}
	if self.dir == "" {
		record, ok := self.owned[intentId]
		if !ok {
			return nil
		}
		saved := *record
		return &saved
	}
	record := readCryptoInvoiceRecord(filepath.Join(self.dir, cryptoInvoiceIntentFileNamePrefix+intentId))
	if record == nil || record.IntentId != intentId {
		return nil
	}
	return record
}

// Every invoice of the device's own saved, the ones bought without a sign-in.
func (self *cryptoInvoiceStorage) loadAllOwned() []*cryptoInvoiceRecord {
	records := []*cryptoInvoiceRecord{}
	if self.dir == "" {
		for _, record := range self.owned {
			saved := *record
			records = append(records, &saved)
		}
		return records
	}
	entries, err := os.ReadDir(self.dir)
	if err != nil {
		return records
	}
	for _, entry := range entries {
		intentId, ok := strings.CutPrefix(entry.Name(), cryptoInvoiceIntentFileNamePrefix)
		if !ok || entry.IsDir() {
			continue
		}
		// a staged temp file has a suffix that is no intent id
		if record := self.loadOwned(intentId); record != nil {
			records = append(records, record)
		}
	}
	return records
}

// Saves a record in its own place: an invoice bought without a sign-in in its
// file, an account invoice in the network's slot.
func (self *cryptoInvoiceStorage) save(record *cryptoInvoiceRecord) {
	saved := *record
	if record.ownedByDevice() {
		if !validCryptoInvoiceIntentId(record.IntentId) {
			return
		}
		if self.dir == "" {
			if self.owned == nil {
				self.owned = map[string]*cryptoInvoiceRecord{}
			}
			self.owned[record.IntentId] = &saved
			return
		}
		self.writeRecord(cryptoInvoiceIntentFileNamePrefix+record.IntentId, &saved)
		return
	}
	if self.dir == "" {
		self.slot = &saved
		return
	}
	self.writeRecord(cryptoInvoiceFileName, &saved)
}

// Drops a record from its place. The network's slot is compare-and-delete:
// it is left alone when it holds another invoice by now, which another
// controller on the same directory saved.
func (self *cryptoInvoiceStorage) clear(record *cryptoInvoiceRecord) {
	if record.ownedByDevice() {
		self.clearOwned(record.IntentId)
		return
	}
	if slot := self.loadSlot(); slot == nil || slot.IntentId != record.IntentId {
		return
	}
	if self.dir == "" {
		self.slot = nil
		return
	}
	self.removeFile(cryptoInvoiceFileName)
}

func (self *cryptoInvoiceStorage) clearOwned(intentId string) {
	if !validCryptoInvoiceIntentId(intentId) {
		return
	}
	if self.dir == "" {
		delete(self.owned, intentId)
		return
	}
	self.removeFile(cryptoInvoiceIntentFileNamePrefix + intentId)
}

func (self *cryptoInvoiceStorage) loadPaymentMethod() string {
	if self.dir == "" {
		return self.paymentMethod
	}
	data, err := os.ReadFile(filepath.Join(self.dir, cryptoPaymentMethodFileName))
	if err != nil {
		return ""
	}
	var method string
	if err := json.Unmarshal(data, &method); err != nil {
		return ""
	}
	return method
}

func (self *cryptoInvoiceStorage) savePaymentMethod(method string) {
	if self.dir == "" {
		self.paymentMethod = method
		return
	}
	path := filepath.Join(self.dir, cryptoPaymentMethodFileName)
	var err error
	if method == "" {
		if err = os.Remove(path); errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	} else {
		var data []byte
		if data, err = json.Marshal(method); err == nil {
			err = writeCryptoInvoiceFile(self.dir, cryptoPaymentMethodFileName, data)
		}
	}
	if err != nil {
		glog.V(1).Infof("[crypto]could not save the payment method: %s\n", err)
	}
}

// Writes a 0600 file whole or not at all: a torn invoice file would lose the
// status token of an invoice that may already be paid.
func writeCryptoInvoiceFile(dir string, name string, data []byte) (returnErr error) {
	temp, err := os.CreateTemp(dir, name+".tmp-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		if returnErr != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(LocalStorageFilePermissions); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, filepath.Join(dir, name)); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if directory, err := os.Open(dir); err == nil {
			_ = directory.Sync()
			_ = directory.Close()
		}
	}
	return nil
}

// The invoice screen's state machine (mmm/ZCASH-CONTRACT.md §4.4). The SDK owns the
// quote, the countdown, the requote, the status and the saved invoice; an app
// renders GetState and calls the actions.
//
// Start means the screen is visible: the controller polls the invoice's
// status (every 5 s while a payment may be on its way, every 30 s once the
// quote expired, every 60 s in review, never once paid or unknown) and ticks
// SecondsLeft once a second. Stop means hidden, and nothing runs then: there
// is no background check. An answer that was already in flight still lands;
// once Close returns nothing lands and nothing is written.
//
// An account invoice is saved in the local-state directory (.crypto_invoice,
// 0600), which a sign-out clears, for the signed-in network only. An invoice
// bought without a sign-in (delivery network or code) belongs to whoever holds
// the device and is saved in a file of its own, whoever is signed in. Either
// is kept from the moment the server quotes it until it is paid and that was
// shown once, the server no longer knows it, or it is 7 days old. A Dismiss
// with nothing received drops an account or network invoice; once money
// arrived it only hides it. A balance-code invoice is never dropped while it
// might still pay: only by a Dismiss after its code was shown, and any other
// Dismiss only hides it.
type CryptoInvoiceViewController struct {
	ctx    context.Context
	cancel context.CancelFunc

	wake chan struct{}

	stateLock sync.Mutex

	storage *cryptoInvoiceStorage

	started bool
	// bumped whenever the invoice on screen changes (StartInvoice, Resume,
	// Dismiss) and when a requote starts, so the answer to a call made for
	// an earlier invoice or quote is dropped
	generation int

	state CryptoInvoiceState
	// the network StartInvoice was called on, while its answer is awaited
	startNetworkId string
	// the invoice's saved form; nil while there is none on screen
	record *cryptoInvoiceRecord
	// what storage holds for it, as this controller last wrote or read it;
	// nil when it holds nothing of this invoice
	savedRecord *cryptoInvoiceRecord
	// the quote's end in the local clock: the server's expiry moved by
	// quoteOffset
	expiresAt time.Time
	// the local time minus the server's, as of the quote's arrival
	quoteOffset time.Duration

	pollInFlight bool
	// poll at once, whatever nextPollAt says (Start, Resume, a new quote)
	forcePoll  bool
	nextPollAt time.Time

	nextTickAt time.Time
	// the SecondsLeft the last tick published; -1 before the first tick of a
	// quote, and the tick stops once it published 0
	tickSecondsLeft int

	listeners *connect.CallbackList[CryptoInvoiceListener]

	// test seams (unexported; not bound)
	nowFunc          func() time.Time
	networkIdFunc    func() string
	createIntentFunc func(args *CreateCryptoPaymentIntentArgs, callback CryptoPaymentIntentCallback)
	statusFunc       func(args *CryptoPaymentStatusArgs, callback CryptoPaymentStatusCallback)
	requoteFunc      func(args *CryptoPaymentStatusArgs, callback CryptoPaymentIntentCallback)
	// the wait for the next poll or tick, time.After outside tests
	afterFunc func(delay time.Duration) <-chan time.Time
}

// Creates a standalone controller on an Api. storageDir is the local-state
// directory the invoice is saved in, the one a sign-out clears; "" keeps the
// invoice and the payment method in memory only. Platforms with a Device
// should prefer OpenCryptoInvoiceViewController, which uses the device's
// local-state directory and ties the controller into the device's
// view-controller lifecycle. The caller owns Close.
func NewCryptoInvoiceViewController(api *Api, storageDir string) *CryptoInvoiceViewController {
	return newCryptoInvoiceViewController(api.ctx, api, storageDir)
}

func newCryptoInvoiceViewController(ctx context.Context, api *Api, storageDir string) *CryptoInvoiceViewController {
	vc := newCryptoInvoiceViewControllerWithoutRun(ctx, api, storageDir)
	go connect.HandleError(vc.run)
	return vc
}

// The controller without its loop; a test drives step() itself.
func newCryptoInvoiceViewControllerWithoutRun(ctx context.Context, api *Api, storageDir string) *CryptoInvoiceViewController {
	cancelCtx, cancel := context.WithCancel(ctx)
	vc := &CryptoInvoiceViewController{
		ctx:             cancelCtx,
		cancel:          cancel,
		wake:            make(chan struct{}, 1),
		storage:         &cryptoInvoiceStorage{dir: storageDir},
		state:           CryptoInvoiceState{Phase: CryptoInvoicePhaseNone},
		tickSecondsLeft: -1,
		listeners:       connect.NewCallbackList[CryptoInvoiceListener](),
		nowFunc:         time.Now,
		afterFunc:       time.After,
	}
	if api != nil {
		// "" when the credential names no network (signed out, an API key):
		// then nothing is saved to or resumed from the network's slot
		vc.networkIdFunc = func() string {
			if networkId, ok := credentialNetworkId(api.GetByJwt()); ok {
				return networkId.String()
			}
			return ""
		}
		vc.createIntentFunc = api.CreateCryptoPaymentIntent
		vc.statusFunc = api.GetCryptoPaymentStatus
		vc.requoteFunc = api.RequoteCryptoPayment
	} else {
		vc.networkIdFunc = func() string { return "" }
	}
	return vc
}

// The local-state directory of the device's network space, the one a
// sign-out clears; "" when the space keeps no local state.
func deviceLocalStorageDir(device Device) string {
	networkSpace := device.GetNetworkSpace()
	if networkSpace == nil {
		return ""
	}
	asyncLocalState := networkSpace.GetAsyncLocalState()
	if asyncLocalState == nil {
		return ""
	}
	return asyncLocalState.GetLocalState().localStorageDir
}

// ---- lifecycle ---------------------------------------------------------------

// The screen is visible: poll the invoice's status at once and then on its
// cadence, and tick the countdown.
func (self *CryptoInvoiceViewController) Start() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.started {
			return
		}
		self.started = true
		self.forcePoll = true
		self.nextTickAt = time.Time{}
	}()
	self.scheduleWake()
}

// The screen is hidden: nothing runs until Start.
func (self *CryptoInvoiceViewController) Stop() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.started = false
	}()
	self.scheduleWake()
}

// Ends the controller and its loop. Nothing is written once Close returns:
// an answer still in flight is dropped, and one being applied finishes first.
// The saved invoice stays saved; Close never deletes it.
func (self *CryptoInvoiceViewController) Close() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.cancel()
}

// Caller holds stateLock.
func (self *CryptoInvoiceViewController) closedLocked() bool {
	return self.ctx.Err() != nil
}

// ---- actions -----------------------------------------------------------------

// Starts an invoice for the signed-in network: one of CryptoAsset*, and either
// a plan (PlanYearly, PlanYearlyOnboarding) or a data pack's item id. The
// server returns the network's open invoice for the same asset and item, if
// there is one. A failure ends in CryptoInvoicePhaseFailed with ErrorCode.
func (self *CryptoInvoiceViewController) StartInvoice(asset string, plan string, itemId string, storefrontCountry string) {
	var generation int
	var networkId string
	started := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closedLocked() {
			return
		}
		self.resetLocked()
		self.state = CryptoInvoiceState{
			Phase:  CryptoInvoicePhaseStarting,
			Asset:  asset,
			Plan:   plan,
			ItemId: itemId,
			Ticker: CryptoAssetTicker(asset),
		}
		generation = self.generation
		networkId = self.networkIdFunc()
		self.startNetworkId = networkId
		started = true
	}()
	if !started {
		return
	}
	self.changed()

	args := &CreateCryptoPaymentIntentArgs{
		Asset:             asset,
		Plan:              plan,
		ItemId:            itemId,
		StorefrontCountry: storefrontCountry,
	}
	self.createIntentFunc(args, connect.NewApiCallback[*CryptoPaymentIntentResult](
		func(result *CryptoPaymentIntentResult, err error) {
			self.intentStarted(generation, networkId, result, err)
		},
	))
}

// Shows a saved invoice, and polls it at once while started. In order: a
// balance-code invoice that is paid and not yet dismissed after its code was
// shown; an invoice bought without a sign-in that was on screen; the
// signed-in network's own invoice. Every other invoice bought without a
// sign-in is polled once: a balance code that turns out paid is shown while
// nothing else is on screen, and an invoice the server no longer knows, or a
// network's that was paid, is dropped. An invoice already on screen stays,
// unless it is an account invoice of another network than the signed-in one
// (a sign-out or another account), which puts it away. True when an invoice
// is on screen after the call.
func (self *CryptoInvoiceViewController) Resume() bool {
	shown := false
	changed := false
	checks := []*cryptoInvoiceRecord{}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closedLocked() {
			return
		}
		networkId := self.networkIdFunc()
		switch self.state.Phase {
		case CryptoInvoicePhaseStarting, CryptoInvoicePhaseOpen, CryptoInvoicePhasePaid:
			if self.showingOwnedLocked() || self.showingNetworkIdLocked() == networkId {
				shown = true
				return
			}
			// another network's invoice, or one from before a sign-out
			self.resetLocked()
			self.state = CryptoInvoiceState{Phase: CryptoInvoicePhaseNone}
			changed = true
		}

		now := self.nowFunc()
		owned := []*cryptoInvoiceRecord{}
		for _, record := range self.storage.loadAllOwned() {
			if record.StatusToken == "" || !record.ownedByDevice() || cryptoInvoiceRecordTooOld(record, now) {
				self.storage.clearOwned(record.IntentId)
				continue
			}
			owned = append(owned, record)
		}
		// newest first
		slices.SortFunc(owned, func(a *cryptoInvoiceRecord, b *cryptoInvoiceRecord) int {
			return strings.Compare(b.CreatedAt, a.CreatedAt)
		})
		var show *cryptoInvoiceRecord
		for _, record := range owned {
			if record.isCode() && record.Status == CryptoPaymentStatusPaid {
				show = record
				break
			}
		}
		if show == nil {
			for _, record := range owned {
				if !record.Hidden && record.Status != CryptoPaymentStatusPaid {
					show = record
					break
				}
			}
		}
		if show == nil && networkId != "" {
			if record := self.storage.loadSlot(); record != nil && record.IntentId != "" && record.StatusToken != "" &&
				record.NetworkId == networkId && !record.ownedByDevice() {
				if cryptoInvoiceRecordTooOld(record, now) {
					self.storage.clear(record)
				} else {
					show = record
				}
			}
		}
		for _, record := range owned {
			if record != show {
				checks = append(checks, record)
			}
		}
		if show != nil {
			self.showRecordLocked(show)
			shown = true
			changed = true
		}
	}()
	if changed {
		self.changed()
		self.scheduleWake()
	}
	for _, record := range checks {
		intentId := record.IntentId
		self.statusFunc(
			&CryptoPaymentStatusArgs{IntentId: intentId, StatusToken: record.StatusToken},
			connect.NewApiCallback[*CryptoPaymentStatusResult](func(result *CryptoPaymentStatusResult, err error) {
				self.ownedChecked(intentId, result, err)
			}),
		)
	}
	if shown {
		return true
	}
	// a check that already answered may have shown a paid code
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return !self.closedLocked() && self.state.Phase != CryptoInvoicePhaseNone && self.state.Phase != CryptoInvoicePhaseFailed
}

// The invoice on screen was bought without a sign-in, so it belongs to no
// network. Caller holds stateLock.
func (self *CryptoInvoiceViewController) showingOwnedLocked() bool {
	return self.record != nil && self.record.ownedByDevice()
}

// The network the invoice on screen was started on. Caller holds stateLock.
func (self *CryptoInvoiceViewController) showingNetworkIdLocked() string {
	if self.record != nil {
		return self.record.NetworkId
	}
	return self.startNetworkId
}

func cryptoInvoiceRecordTooOld(record *cryptoInvoiceRecord, now time.Time) bool {
	createdAt, err := time.Parse(time.RFC3339, record.CreatedAt)
	return err != nil || cryptoInvoiceMaxAge < now.Sub(createdAt)
}

// Asks for a new quote for an invoice nothing has been paid to (Status
// pending or expired): a new amount and expiry for the same address, or a
// later expiry for USDC. A poll in flight is dropped, since it answers for the
// old quote, and the new quote is polled at once. Refreshing is true while it
// is in flight; a failure leaves the invoice as it was, with ErrorCode set.
// Anything else is ignored.
func (self *CryptoInvoiceViewController) Requote() {
	var generation int
	var args *CryptoPaymentStatusArgs
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closedLocked() || self.state.Phase != CryptoInvoicePhaseOpen || self.record == nil || self.state.Refreshing {
			return
		}
		switch self.state.Status {
		case CryptoPaymentStatusPending, CryptoPaymentStatusExpired:
		default:
			return
		}
		self.state.Refreshing = true
		self.state.ErrorCode = ""
		self.state.ErrorMessage = ""
		self.generation += 1
		self.pollInFlight = false
		generation = self.generation
		args = &CryptoPaymentStatusArgs{
			IntentId:    self.record.IntentId,
			StatusToken: self.record.StatusToken,
		}
	}()
	if args == nil {
		return
	}
	self.changed()
	self.requoteFunc(args, connect.NewApiCallback[*CryptoPaymentIntentResult](
		func(result *CryptoPaymentIntentResult, err error) {
			self.requoteDone(generation, result, err)
		},
	))
}

// Closes the invoice ("Pay another way"); closing the screen is Stop and
// Close, never this. An account or network invoice with nothing received is
// dropped: nothing is lost, the data goes to the network anyway. Once money
// arrived it is only put away: an account invoice comes back at the next
// Resume, and one bought without a sign-in is hidden. A balance-code invoice
// is dropped only when it is paid and its code was shown; before that it is
// only hidden, since its status lags the payment (USDC goes from pending
// straight to paid, and a late payment credits while it reads expired) and a
// dropped code invoice would lose a code nobody else can read.
func (self *CryptoInvoiceViewController) Dismiss() {
	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closedLocked() || self.state.Phase == CryptoInvoicePhaseNone {
			return
		}
		if self.record != nil && self.state.Status != CryptoPaymentStatusUnknown {
			switch {
			case self.record.isCode():
				if self.state.Phase == CryptoInvoicePhasePaid && self.state.BalanceCode != "" {
					self.clearRecordLocked()
				} else {
					self.record.Hidden = true
					self.saveRecordLocked()
				}
			case !cryptoInvoiceMayHaveReceived(self.state.Status):
				self.clearRecordLocked()
			case self.record.ownedByDevice():
				self.record.Hidden = true
				self.saveRecordLocked()
			}
		}
		self.resetLocked()
		self.state = CryptoInvoiceState{Phase: CryptoInvoicePhaseNone}
		changed = true
	}()
	if changed {
		self.changed()
		self.scheduleWake()
	}
}

// Money may have arrived: the status says so, or a resumed invoice has no
// status yet.
func cryptoInvoiceMayHaveReceived(status string) bool {
	switch status {
	case "", CryptoPaymentStatusSeen, CryptoPaymentStatusConfirming, CryptoPaymentStatusReview:
		return true
	default:
		return false
	}
}

// A snapshot of the invoice, with SecondsLeft as of now.
func (self *CryptoInvoiceViewController) GetState() *CryptoInvoiceState {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	state := self.state
	if state.Phase == CryptoInvoicePhaseOpen {
		state.SecondsLeft = cryptoInvoiceSecondsLeft(self.expiresAt, self.nowFunc())
	}
	return &state
}

func (self *CryptoInvoiceViewController) AddCryptoInvoiceListener(listener CryptoInvoiceListener) Sub {
	callbackId := self.listeners.Add(listener)
	return newSub(func() {
		self.listeners.Remove(callbackId)
	})
}

// The payment method last chosen on this device (PaymentMethodCard or a
// CryptoAsset* value), "" when none was. A sign-out forgets it.
func (self *CryptoInvoiceViewController) GetPaymentMethod() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	method := self.storage.loadPaymentMethod()
	if method != PaymentMethodCard && !isCryptoAsset(method) {
		return ""
	}
	return method
}

// Remembers the payment method chosen on this device: PaymentMethodCard or a
// CryptoAsset* value, or "" to forget it. Anything else is ignored.
func (self *CryptoInvoiceViewController) SetPaymentMethod(method string) {
	if method != "" && method != PaymentMethodCard && !isCryptoAsset(method) {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closedLocked() {
		return
	}
	self.storage.savePaymentMethod(method)
}

// ---- answers -----------------------------------------------------------------

func (self *CryptoInvoiceViewController) intentStarted(generation int, networkId string, result *CryptoPaymentIntentResult, err error) {
	applied := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if generation != self.generation || self.closedLocked() {
			// dismissed, replaced by another invoice or closed while in flight
			return
		}
		applied = true
		switch {
		case err != nil:
			self.failLocked("", err.Error())
		case result == nil:
			self.failLocked("", "no invoice")
		case result.Error != nil:
			self.failLocked(result.Error.Code, result.Error.Message)
		case result.IntentId == "" || result.StatusToken == "" || result.Address == "" || result.AmountAtomic == "":
			// an answer that cannot be paid is no answer
			self.failLocked("", "no invoice")
		default:
			self.record = &cryptoInvoiceRecord{
				NetworkId:   networkId,
				IntentId:    result.IntentId,
				StatusToken: result.StatusToken,
				CreatedAt:   self.nowFunc().UTC().Format(time.RFC3339),
			}
			self.applyQuoteLocked(result, false)
		}
	}()
	if applied {
		self.changed()
		self.scheduleWake()
	}
}

func (self *CryptoInvoiceViewController) requoteDone(generation int, result *CryptoPaymentIntentResult, err error) {
	applied := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if generation != self.generation || self.record == nil || self.closedLocked() {
			return
		}
		applied = true
		self.state.Refreshing = false
		switch {
		case err != nil:
			self.state.ErrorCode = ""
			self.state.ErrorMessage = err.Error()
		case result == nil:
			self.state.ErrorCode = ""
			self.state.ErrorMessage = "no quote"
		case result.Error != nil:
			self.state.ErrorCode = result.Error.Code
			self.state.ErrorMessage = result.Error.Message
		case result.IntentId != self.record.IntentId || result.Address == "" || result.AmountAtomic == "":
			// another invoice's quote, or one that cannot be paid
			self.state.ErrorCode = ""
			self.state.ErrorMessage = "no quote"
		default:
			if result.StatusToken != "" {
				self.record.StatusToken = result.StatusToken
			}
			self.applyQuoteLocked(result, true)
		}
	}()
	if applied {
		self.changed()
		self.scheduleWake()
	}
}

func (self *CryptoInvoiceViewController) pollDone(generation int, result *CryptoPaymentStatusResult, err error) {
	applied := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if generation != self.generation || self.record == nil || self.closedLocked() {
			return
		}
		self.pollInFlight = false
		self.forcePoll = false
		now := self.nowFunc()
		if err != nil || result == nil || (result.IntentId != "" && result.IntentId != self.record.IntentId) {
			// keep the invoice as it is and ask again on its cadence
			if interval, ok := cryptoInvoicePollInterval(self.state.Status); ok {
				self.nextPollAt = now.Add(interval)
			}
			return
		}
		applied = true
		self.applyStatusLocked(result)
		if interval, ok := cryptoInvoicePollInterval(self.state.Status); ok {
			self.nextPollAt = now.Add(interval)
		}
	}()
	if applied {
		self.changed()
	}
	self.scheduleWake()
}

// The answer to Resume's single poll of an invoice bought without a sign-in
// that is not on screen. Unknown drops it. Paid saves a balance code, and
// shows it when nothing else is on screen; a network's paid invoice is done,
// its data on the network, and is dropped.
func (self *CryptoInvoiceViewController) ownedChecked(intentId string, result *CryptoPaymentStatusResult, err error) {
	shown := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closedLocked() || err != nil || result == nil || (result.IntentId != "" && result.IntentId != intentId) {
			return
		}
		if self.record != nil && self.record.IntentId == intentId {
			// on screen by now, polled as such
			return
		}
		record := self.storage.loadOwned(intentId)
		if record == nil {
			return
		}
		switch result.Status {
		case CryptoPaymentStatusUnknown:
			self.storage.clearOwned(intentId)
		case CryptoPaymentStatusPaid:
			if !record.isCode() {
				self.storage.clearOwned(intentId)
				return
			}
			record.Status = CryptoPaymentStatusPaid
			record.BalanceCode = result.BalanceCode
			record.Hidden = false
			self.storage.save(record)
			if self.state.Phase == CryptoInvoicePhaseNone {
				self.showRecordLocked(record)
				shown = true
			}
		}
	}()
	if shown {
		self.changed()
		self.scheduleWake()
	}
}

// ---- state -------------------------------------------------------------------

// Forgets the invoice on screen and drops every answer in flight. The caller
// sets the new state. Caller holds stateLock.
func (self *CryptoInvoiceViewController) resetLocked() {
	self.generation += 1
	self.startNetworkId = ""
	self.record = nil
	self.savedRecord = nil
	self.expiresAt = time.Time{}
	self.quoteOffset = 0
	self.pollInFlight = false
	self.forcePoll = false
	self.nextPollAt = time.Time{}
	self.nextTickAt = time.Time{}
	self.tickSecondsLeft = -1
}

// Caller holds stateLock.
func (self *CryptoInvoiceViewController) failLocked(code string, message string) {
	self.state.Phase = CryptoInvoicePhaseFailed
	self.state.ErrorCode = code
	self.state.ErrorMessage = message
}

// Shows a saved invoice: open and not yet polled, or paid when its record
// says so (a balance-code invoice). Caller holds stateLock.
func (self *CryptoInvoiceViewController) showRecordLocked(record *cryptoInvoiceRecord) {
	self.resetLocked()
	// as storage holds it; on screen it is no longer hidden
	saved := *record
	self.savedRecord = &saved
	record.Hidden = false
	self.record = record
	self.state = CryptoInvoiceState{
		Phase:        CryptoInvoicePhaseOpen,
		IntentId:     record.IntentId,
		Asset:        record.Asset,
		Plan:         record.Plan,
		ItemId:       record.ItemId,
		Delivery:     record.Delivery,
		NetworkName:  record.NetworkName,
		Address:      record.Address,
		Amount:       record.Amount,
		AmountAtomic: record.AmountAtomic,
		Ticker:       CryptoAssetTicker(record.Asset),
		Memo:         record.Memo,
		PriceUsd:     record.PriceUsd,
		RateUsd:      record.RateUsd,
	}
	if record.isCode() && record.Status == CryptoPaymentStatusPaid {
		self.state.Phase = CryptoInvoicePhasePaid
		self.state.Status = CryptoPaymentStatusPaid
		self.state.BalanceCode = record.BalanceCode
	}
	self.setUriLocked(record.Uri)
	self.quoteOffset = cryptoInvoiceQuoteOffset(record.QuotedAt, record.ReceivedAt)
	self.setExpiresAtLocked(record.ExpiresAt)
	self.forcePoll = true
}

// The local time minus the server's, from a quote's server start and the
// local time it arrived; 0 when either is missing, which falls back to the
// device clock.
func cryptoInvoiceQuoteOffset(quotedAt string, receivedAt string) time.Duration {
	quoted, err := time.Parse(time.RFC3339, quotedAt)
	if err != nil {
		return 0
	}
	received, err := time.Parse(time.RFC3339, receivedAt)
	if err != nil {
		return 0
	}
	return received.Sub(quoted)
}

// Shows a quote from the server (a new invoice or a requote) and saves it.
// The quote's own fields come from the answer as they are; the invoice's
// names keep what the invoice already shows when the answer leaves them out.
// A requote never moves a status past pending or expired back to pending, and
// its quote is polled at once. Caller holds stateLock with self.record set.
func (self *CryptoInvoiceViewController) applyQuoteLocked(result *CryptoPaymentIntentResult, requote bool) {
	now := self.nowFunc()
	keep := func(value string, current string) string {
		if value == "" {
			return current
		}
		return value
	}
	self.state.Phase = CryptoInvoicePhaseOpen
	self.state.IntentId = self.record.IntentId
	self.state.Asset = keep(result.Asset, self.state.Asset)
	self.state.Plan = keep(result.Plan, self.state.Plan)
	self.state.ItemId = keep(result.ItemId, self.state.ItemId)
	self.state.Delivery = keep(result.Delivery, self.state.Delivery)
	self.state.NetworkName = keep(result.NetworkName, self.state.NetworkName)
	self.state.Address = result.Address
	self.state.Amount = result.Amount
	self.state.AmountAtomic = result.AmountAtomic
	self.state.Ticker = CryptoAssetTicker(self.state.Asset)
	self.state.Memo = result.Memo
	self.state.PriceUsd = result.PriceUsd
	self.state.RateUsd = result.RateUsd
	if 0 < result.ConfirmationsRequired {
		self.state.ConfirmationsRequired = result.ConfirmationsRequired
	}
	self.state.ErrorCode = ""
	self.state.ErrorMessage = ""
	self.setUriLocked(result.Uri)

	// the countdown runs the quote's own duration from now, whatever the
	// device clock says about the server's
	receivedAt := now.UTC().Format(cryptoInvoiceLocalTimeLayout)
	self.quoteOffset = cryptoInvoiceQuoteOffset(result.QuotedAt, receivedAt)
	self.expiresAt = time.Time{}
	self.setExpiresAtLocked(result.ExpiresAt)

	self.record.Asset = self.state.Asset
	self.record.Plan = self.state.Plan
	self.record.ItemId = self.state.ItemId
	self.record.Delivery = self.state.Delivery
	self.record.QuotedAt = result.QuotedAt
	self.record.ReceivedAt = receivedAt

	status := result.Status
	if status == nil {
		// both a new invoice and a requote have nothing received yet
		status = &CryptoPaymentStatusResult{Status: CryptoPaymentStatusPending}
	}
	if requote && cryptoInvoiceStatusPastPending(self.state.Status) {
		// money arrived: the requote's pending never makes it payable again
		self.saveRecordLocked()
	} else {
		self.applyStatusLocked(status)
	}
	if requote {
		self.forcePoll = true
	} else {
		self.forcePoll = false
		if interval, ok := cryptoInvoicePollInterval(self.state.Status); ok {
			self.nextPollAt = now.Add(interval)
		}
	}
}

// Past pending or expired: something arrived, or the invoice is done.
func cryptoInvoiceStatusPastPending(status string) bool {
	switch status {
	case CryptoPaymentStatusSeen, CryptoPaymentStatusConfirming, CryptoPaymentStatusReview, CryptoPaymentStatusPaid:
		return true
	default:
		return false
	}
}

// Shows a status from the server and keeps the saved invoice in step: saved
// while open, dropped once paid (a balance-code invoice's is kept, with its
// code, until it is dismissed) or unknown. Caller holds stateLock with
// self.record set.
func (self *CryptoInvoiceViewController) applyStatusLocked(status *CryptoPaymentStatusResult) {
	if status.Status == CryptoPaymentStatusUnknown {
		self.state.Status = CryptoPaymentStatusUnknown
		self.state.Phase = CryptoInvoicePhaseFailed
		self.clearRecordLocked()
		return
	}
	self.state.Status = status.Status
	self.state.ReviewReason = status.ReviewReason
	self.state.Received = status.Received
	self.state.Remaining = status.Remaining
	self.state.Confirmations = status.Confirmations
	if 0 < status.ConfirmationsRequired {
		self.state.ConfirmationsRequired = status.ConfirmationsRequired
	}
	self.state.BalanceCode = status.BalanceCode
	if status.Delivery != "" && self.state.Delivery == "" {
		self.state.Delivery = status.Delivery
		self.record.Delivery = status.Delivery
	}
	if status.NetworkName != "" {
		self.state.NetworkName = status.NetworkName
	}
	if status.AmountAtomic != "" && status.AmountAtomic != self.state.AmountAtomic {
		// quoted again elsewhere (another device requoted this network's
		// invoice): the uri still asks for the old amount, so it goes
		self.state.Amount = status.Amount
		self.state.AmountAtomic = status.AmountAtomic
		self.state.Uri = ""
	}
	if status.ExpiresAt != "" {
		self.setExpiresAtLocked(status.ExpiresAt)
	}
	if status.Status == CryptoPaymentStatusExpired {
		// the server's word ends the countdown, whatever is left of it here
		// (an open invoice handed out again keeps its first quote time)
		now := self.nowFunc()
		if self.expiresAt.After(now) {
			self.expiresAt = now
			self.tickSecondsLeft = 0
		}
	}

	if status.Status == CryptoPaymentStatusPaid {
		self.state.Phase = CryptoInvoicePhasePaid
		if !self.record.isCode() {
			// this answer is the delivery: the listeners hear it next
			self.clearRecordLocked()
			return
		}
	} else {
		self.state.Phase = CryptoInvoicePhaseOpen
	}
	self.saveRecordLocked()
}

// Saves the invoice as it shows now, when that changed. Nothing is written
// after Close. An account invoice is only ever saved for the network that is
// signed in, never for none, so an answer that lands after a sign-out cannot
// write it back; an invoice bought without a sign-in is saved whoever is
// signed in, since it belongs to no network. Caller holds stateLock with
// self.record set.
func (self *CryptoInvoiceViewController) saveRecordLocked() {
	record := *self.record
	record.Delivery = self.state.Delivery
	record.NetworkName = self.state.NetworkName
	record.Address = self.state.Address
	record.Amount = self.state.Amount
	record.AmountAtomic = self.state.AmountAtomic
	record.Uri = self.state.Uri
	record.Memo = self.state.Memo
	record.PriceUsd = self.state.PriceUsd
	record.RateUsd = self.state.RateUsd
	record.ExpiresAt = self.state.ExpiresAt
	if record.isCode() {
		record.Status = self.state.Status
		record.BalanceCode = self.state.BalanceCode
	}
	self.record = &record
	if self.savedRecord != nil && record == *self.savedRecord {
		return
	}
	if self.closedLocked() {
		return
	}
	if !record.ownedByDevice() {
		if networkId := self.networkIdFunc(); record.NetworkId == "" || record.NetworkId != networkId {
			return
		}
	}
	self.storage.save(&record)
	saved := record
	self.savedRecord = &saved
}

// Drops the saved invoice, from its own place only. Caller holds stateLock.
func (self *CryptoInvoiceViewController) clearRecordLocked() {
	if self.record != nil && !self.closedLocked() {
		self.storage.clear(self.record)
	}
	self.savedRecord = nil
}

// Caller holds stateLock.
func (self *CryptoInvoiceViewController) setUriLocked(uri string) {
	if CheckCryptoPaymentUri(self.state.Asset, uri, self.state.Address) {
		self.state.Uri = uri
	} else {
		self.state.Uri = ""
	}
}

// Moves the quote's end, a server time, into the local clock by the quote's
// offset. A new end restarts the countdown from the second it shows now; the
// change itself is announced by its caller, so the next tick is a second
// away. Caller holds stateLock.
func (self *CryptoInvoiceViewController) setExpiresAtLocked(expiresAt string) {
	self.state.ExpiresAt = expiresAt
	var local time.Time
	if t, err := time.Parse(time.RFC3339, expiresAt); err == nil {
		local = t.Add(self.quoteOffset)
	}
	if local.Equal(self.expiresAt) {
		return
	}
	self.expiresAt = local
	now := self.nowFunc()
	self.tickSecondsLeft = cryptoInvoiceSecondsLeft(local, now)
	self.nextTickAt = now.Add(cryptoInvoiceTickInterval)
}

// The quote's seconds left at now, rounded up, so it is 0 exactly once the
// quote expired.
func cryptoInvoiceSecondsLeft(expiresAt time.Time, now time.Time) int {
	if expiresAt.IsZero() {
		return 0
	}
	remaining := expiresAt.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return int((remaining + time.Second - 1) / time.Second)
}

// How long until the next status poll, and false for a status that is never
// polled again. A resumed invoice with no status yet polls like one waiting
// for a payment; a status this sdk does not know polls at the slowest rate.
func cryptoInvoicePollInterval(status string) (time.Duration, bool) {
	switch status {
	case "", CryptoPaymentStatusPending, CryptoPaymentStatusSeen, CryptoPaymentStatusConfirming:
		return cryptoInvoiceActivePollInterval, true
	case CryptoPaymentStatusExpired:
		return cryptoInvoiceExpiredPollInterval, true
	case CryptoPaymentStatusPaid, CryptoPaymentStatusUnknown:
		return 0, false
	default:
		return cryptoInvoiceReviewPollInterval, true
	}
}

// ---- loop --------------------------------------------------------------------

// Wakes the loop without blocking; a wake already pending covers this one.
func (self *CryptoInvoiceViewController) scheduleWake() {
	select {
	case self.wake <- struct{}{}:
	default:
	}
}

// Steps on every wake and when the next poll or tick is due, until Close.
// Each step replaces the wait: none while stopped, and none for a poll in
// flight, whose answer wakes the loop.
func (self *CryptoInvoiceViewController) run() {
	// nil while nothing is scheduled
	var due <-chan time.Time
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.wake:
		case <-due:
		}
		if delay, arm := self.step(); arm {
			due = self.afterFunc(max(delay, 0))
		} else {
			due = nil
		}
	}
}

// Launches a due status poll, publishes a due tick, and returns the delay
// until the next of either; arm is false when nothing is due later (stopped,
// closed, no open invoice, a poll or requote in flight and the countdown
// done).
func (self *CryptoInvoiceViewController) step() (delay time.Duration, arm bool) {
	var pollArgs *CryptoPaymentStatusArgs
	var generation int
	ticked := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.started || self.closedLocked() || self.state.Phase != CryptoInvoicePhaseOpen || self.record == nil {
			return
		}
		now := self.nowFunc()

		var next time.Time
		interval, pollable := cryptoInvoicePollInterval(self.state.Status)
		if pollable && !self.pollInFlight && !self.state.Refreshing {
			if self.forcePoll || !now.Before(self.nextPollAt) {
				self.pollInFlight = true
				self.forcePoll = false
				// the answer sets the next poll from the status it brings
				self.nextPollAt = now.Add(interval)
				generation = self.generation
				pollArgs = &CryptoPaymentStatusArgs{
					IntentId:    self.record.IntentId,
					StatusToken: self.record.StatusToken,
				}
			} else {
				next = self.nextPollAt
			}
		}

		if !self.expiresAt.IsZero() && self.tickSecondsLeft != 0 {
			if !now.Before(self.nextTickAt) {
				secondsLeft := cryptoInvoiceSecondsLeft(self.expiresAt, now)
				if secondsLeft != self.tickSecondsLeft {
					self.tickSecondsLeft = secondsLeft
					ticked = true
				}
				self.nextTickAt = now.Add(cryptoInvoiceTickInterval)
			}
			if self.tickSecondsLeft != 0 && (next.IsZero() || self.nextTickAt.Before(next)) {
				next = self.nextTickAt
			}
		}

		if !next.IsZero() {
			delay = next.Sub(now)
			arm = true
		}
	}()

	if ticked {
		self.changed()
	}
	if pollArgs != nil {
		self.statusFunc(pollArgs, connect.NewApiCallback[*CryptoPaymentStatusResult](
			func(result *CryptoPaymentStatusResult, err error) {
				self.pollDone(generation, result, err)
			},
		))
	}
	return
}

func (self *CryptoInvoiceViewController) changed() {
	for _, listener := range self.listeners.Get() {
		connect.HandleError(func() {
			listener.CryptoInvoiceChanged()
		})
	}
}
