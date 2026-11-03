//go:build !ios_extension && !js

package sdk

// The payout key's flow (apple BTEASY.md), the same on every app. A casual miner taps once: the key
// is made here, written to the app's secret store and read back, and only then signs the network's
// payout consent. Or the user saves the phrase first, and the key signs once they prove it with two
// of its words. Either way the app never signs until the phrase is safe, since earnings could
// otherwise be paid to a key nobody holds. link() is the only place here that signs, and each of its
// three callers has that proof.
//
// An app brings what only it can do: a secret store (BittensorLocalKeyStore), its screens with their
// strings, device authentication, the screen's privacy controls, the pasteboard and its local
// notification. It reads what to show from GetCard and GetScreen, and those carry no secret: the
// words leave only through RevealedWords and PhraseToCopy, and only while revealed. A stored phrase
// is read when the screen needs it and let go when it hides. Go strings cannot be wiped, so that is
// as much as can be claimed (see bittensor_local_key.go).
//
// The server is only told of the consent: to it this is any coldkey signing. The status read stays
// with the app's consent screen, which hands it over with SetConsent, so a visit reads it once.
//
// The build constraint is bittensor_local_key.go's: neither the tunnel extension nor the browser has
// this, so the phrase cannot reach them.
//
// Safe for concurrent use. The listener fires on an arbitrary goroutine. A call that can reach the
// network returns at once and reports through the listener.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// What the key is, from the Key states table of apple BTEASY.md, with the cases the table leaves to
// its edge cases. The ones marked "after a read" are what an unread input reads as.
const (
	// the network's consent has not been read yet
	BittensorLocalKeyStateLoading = "loading"
	// the latest consent read failed: unknown, which is not "no consent"
	BittensorLocalKeyStateConsentUnavailable = "consent_unavailable"
	// the store could not answer, as on a locked device: unknown, which is not "no key"
	BittensorLocalKeyStateStoreUnavailable = "store_unavailable"
	// no consent, and no key here: create or connect
	BittensorLocalKeyStateNone = "none"
	// a phrase in memory only, nothing on disk: the Key screen
	BittensorLocalKeyStateSaveNowShowing = "save_now_showing"
	// the same after a refused consent: the Key screen with Retry. Leaving discards the key, and no
	// earnings went to it
	BittensorLocalKeyStateSaveNowConsentFailed = "save_now_consent_failed"
	// a key in the store, and the consent does not pay it: Finish setup
	BittensorLocalKeyStateSaveLaterNotLinked = "save_later_not_linked"
	// a key in the store, and the consent pays it
	BittensorLocalKeyStateSaveLaterLinked = "save_later_linked"
	// the consent pays a key this device made and no longer has: claims keep working, convert and
	// one-tap Top 200 registration don't
	BittensorLocalKeyStateSavedAndDeleted = "saved_and_deleted"
	// the consent pays an address with no key here and none this device made: a key on another device
	// of the network, or an outside wallet. Nothing here tells them apart, since the server does not
	// record how a key was made
	BittensorLocalKeyStateElsewhere = "elsewhere"
)

// Why the last action stopped, for the app's own words. The consent's refusals keep their sdk codes,
// SnErrorCodeChallengeExpired, SnErrorCodeSignatureMismatch and SnErrorCodeNetworkOwnerOnly.
const (
	// a new key could not be made. Nothing was stored or signed
	BittensorLocalKeyFailureGenerate = "generate_failed"
	// the key could not be written and read back. Nothing was signed
	BittensorLocalKeyFailureStore = "store_failed"
	// the store could not answer now
	BittensorLocalKeyFailureStoreUnavailable = "store_unavailable"
	// removing the key could not be verified: it is treated as still there
	BittensorLocalKeyFailureDelete = "delete_failed"
	// the action does not apply: only the signed-in network owner, on no guest network, can make a key
	BittensorLocalKeyFailureNotAllowed = "not_allowed"
	// a consent is in force, and the user was not warned that this replaces it
	BittensorLocalKeyFailureReplacementNotConfirmed = "replacement_not_confirmed"
	// any other consent failure: FailureMessage is the sdk's or the server's text
	BittensorLocalKeyFailureConsent = "consent_failed"
)

// What the Key screen is asking the user to prove with two words.
const (
	// not asking
	BittensorLocalKeyConfirmNone = ""
	// I have saved the key, it's OK to delete it now: the destructive action, on both flows
	BittensorLocalKeyConfirmDelete = "delete"
	// the key stays on this device for convert and Top 200, and the backup reminders end
	BittensorLocalKeyConfirmKeep = "keep"
)

// What a reminder of an unsaved key asks of the app (BittensorLocalKeyFlow.Reminder).
const (
	BittensorLocalKeyReminderNone = "none"
	// post the notification at AtMillis, in place of any one posted before
	BittensorLocalKeyReminderSchedule = "schedule"
	// post it now
	BittensorLocalKeyReminderDeliverNow = "deliver_now"
	// take back the one posted
	BittensorLocalKeyReminderCancel = "cancel"
)

const (
	// the words hide themselves this long after they show
	BittensorLocalKeyRevealSeconds = 60
	// a copied phrase leaves the pasteboard after this long
	BittensorLocalKeyCopySeconds = 60
	// a store's answer for "cannot answer now", which is neither a phrase nor an address
	BittensorLocalKeyStoreUnavailable = "store_unavailable"
)

const (
	// non-secret notes the store keeps by network id, past a logout. All of them are addresses or
	// numbers: a phrase is never a note
	bittensorLocalKeyNoteSavedAddress           = "saved_address"
	bittensorLocalKeyNoteDeletedAddress         = "deleted_address"
	bittensorLocalKeyNoteReminderAtMillis       = "reminder_at_millis"
	bittensorLocalKeyNoteReminderDelivered      = "reminder_delivered"
	bittensorLocalKeyNoteReminderDeliveredValue = "1"

	// how long one consent request may take
	bittensorLocalKeyConsentTimeout = 60 * time.Second
)

// The app's secret store, one item per network id, which must outlive a logout and be readable only
// by this app, not its extensions. Keychain on Apple. Every method can be called on any goroutine.
// Each returns one value, since that is what the c abi carries.
type BittensorLocalKeyStore interface {
	// Adds the network's item: the phrase as its secret, the address readable without the secret. It
	// must never replace an item. False if one is there or the store refused.
	AddKey(networkId string, mnemonic string, address string) bool
	// The item's phrase. "" with no item, BittensorLocalKeyStoreUnavailable if the store cannot answer.
	KeyPhrase(networkId string) string
	// The item's address, without touching the secret. "" and BittensorLocalKeyStoreUnavailable as above.
	KeyAddress(networkId string) string
	// Removes the item. False if the store refused. No item is not a refusal. The flow checks the
	// result by reading again.
	DeleteKey(networkId string) bool
	// A non-secret note kept by network id. "" if none.
	ReadNote(networkId string, name string) string
	// Writes a note. "" removes it.
	WriteNote(networkId string, name string, value string)
}

// The card or the Key screen has something new to show.
type BittensorLocalKeyFlowListener interface {
	BittensorLocalKeyFlowChanged()
}

// What the payout card shows. Holds no secret.
type BittensorLocalKeyCard struct {
	// a BittensorLocalKeyState*
	State string `json:"state"`
	// the address to name: the consent's, else a key not linked yet. "" if none
	Address string `json:"address"`
	// the address of the key on this device. "" if none
	StoredAddress string `json:"stored_address"`
	// the main button: make the key, store it, sign, with no screen in between
	OffersSetUp bool `json:"offers_set_up"`
	// the smaller link: the Key screen first, and nothing on disk
	OffersSaveFirst bool `json:"offers_save_first"`
	// today's wallet flow, or Change wallet once a consent is in force
	OffersWallet bool `json:"offers_wallet"`
	// the main button while a stored key is not linked: sign with it
	OffersFinishSetup bool `json:"offers_finish_setup"`
	// the Key screen for the stored key
	OffersKeyScreen bool `json:"offers_key_screen"`
	// set up, save first and finish setup each replace the consent in force from the next epoch, and
	// earlier earnings stay on the old coldkey: warn first, then pass replaceConfirmed
	ReplacesPayoutWallet bool `json:"replaces_payout_wallet"`
	// a session without the network credential: only the network owner can choose
	OwnerOnly bool `json:"owner_only"`
	// the consent pays this key and its first epoch has not started: "Payouts start next epoch"
	PayoutsStartNextEpoch bool `json:"payouts_start_next_epoch"`
	// the stored key's phrase is not known to be saved: the banner, and the warnings at logout and
	// account deletion
	BackupReminder bool `json:"backup_reminder"`
	// the user proved they saved the stored key's phrase and kept the key
	KeySaved bool `json:"key_saved"`
	// a set up, finish setup or retry is on its way
	Busy bool `json:"busy"`
	// counts the consents this flow got accepted: a new value means read the consent and earnings again
	LinkGeneration int64 `json:"link_generation"`
	// a BittensorLocalKeyFailure* or a consent refusal's code, "" if the last action did not stop
	FailureCode    string `json:"failure_code"`
	FailureMessage string `json:"failure_message"`
}

// What the Key screen shows. Holds no secret: the words are RevealedWords.
type BittensorLocalKeyScreen struct {
	Open bool `json:"open"`
	// the phrase is in memory only, and the user has not saved it yet
	SaveNow bool `json:"save_now"`
	// the key's address
	Address string `json:"address"`
	// the words show, with Hide and Copy
	Revealed bool `json:"revealed"`
	// a stored key: tap to reveal asks for device authentication first
	NeedsAuthentication bool `json:"needs_authentication"`
	// the screen is being recorded or mirrored, and the words stay hidden until it stops
	HiddenByCapture bool `json:"hidden_by_capture"`
	// a BittensorLocalKeyConfirm*: the two-word check is up
	Confirming string `json:"confirming"`
	// the two words asked for, counting from 1
	FirstWordPosition  int `json:"first_word_position"`
	SecondWordPosition int `json:"second_word_position"`
	// the last two words did not match
	WordsRejected bool `json:"words_rejected"`
	// the confirmation adds that deleting gives up the one-tap Top 200 claim
	WarnsTop200 bool `json:"warns_top200"`
	// the non-destructive way to prove the phrase is saved, for a stored key
	OffersKeep bool `json:"offers_keep"`
	// a refused consent, Save now: sign again with the same key
	OffersRetry bool `json:"offers_retry"`
	Busy        bool `json:"busy"`
	// the screen's work is finished: the key is linked or gone, and the screen can close
	Done bool `json:"done"`
	// as on the card
	FailureCode    string `json:"failure_code"`
	FailureMessage string `json:"failure_message"`
}

// What to do about the one reminder of an unsaved key. Holds no secret.
type BittensorLocalKeyReminder struct {
	// a BittensorLocalKeyReminder*
	Action string `json:"action"`
	// with BittensorLocalKeyReminderSchedule, unix milliseconds
	AtMillis int64 `json:"at_millis"`
}

// The consent calls this flow makes. *Api is the real one.
type bittensorLocalKeyConsentApi interface {
	HasNetworkCredential() bool
	SnNetworkConsentChallengeSyncWithContext(ctx context.Context, coldkeySs58 string) (*SnNetworkConsentChallengeResult, error)
	SnNetworkConsentSubmitSyncWithContext(ctx context.Context, coldkeySs58 string, message string, signatureHex string) (*SnSetWalletResult, error)
}

// Stands in for a nil Api, so a flow with no session refuses instead of crashing.
type bittensorLocalKeyNoConsentApi struct{}

func (bittensorLocalKeyNoConsentApi) HasNetworkCredential() bool {
	return false
}

func (bittensorLocalKeyNoConsentApi) SnNetworkConsentChallengeSyncWithContext(context.Context, string) (*SnNetworkConsentChallengeResult, error) {
	return nil, fmt.Errorf("%s: %w", SnErrorCodeNetworkOwnerOnly, ErrNetworkCredentialRequired)
}

func (bittensorLocalKeyNoConsentApi) SnNetworkConsentSubmitSyncWithContext(context.Context, string, string, string) (*SnSetWalletResult, error) {
	return nil, fmt.Errorf("%s: %w", SnErrorCodeNetworkOwnerOnly, ErrNetworkCredentialRequired)
}

// What is known of the network's consent.
type bittensorLocalKeyConsentRead int

const (
	bittensorLocalKeyConsentUnread bittensorLocalKeyConsentRead = iota
	bittensorLocalKeyConsentUnavailable
	bittensorLocalKeyConsentNone
	bittensorLocalKeyConsentActive
)

// Everything the card is made from, so its table is tested without a flow.
type bittensorLocalKeyCardInputs struct {
	networkId            string
	hasNetworkCredential bool
	guest                bool
	consentRead          bittensorLocalKeyConsentRead
	consentAddress       string
	consentFromEpoch     int64
	// 0 while unknown
	currentEpoch     int64
	justLinked       bool
	storeUnavailable bool
	storedAddress    string
	savedAddress     string
	deletedAddress   string
	saveNowAddress   string
	saveNowFailed    bool
	busy             bool
	linkGeneration   int64
	failureCode      string
	failureMessage   string
}

// The payout key's flow for one network on this device. One visit of the Earnings screen makes one.
type BittensorLocalKeyFlow struct {
	ctx       context.Context
	cancel    context.CancelFunc
	networkId string
	store     BittensorLocalKeyStore
	api       bittensorLocalKeyConsentApi
	listeners *connect.CallbackList[BittensorLocalKeyFlowListener]

	// what tests replace
	generate      func() (*BittensorLocalKey, error)
	sign          func(mnemonic string, message string) (string, error)
	now           func() time.Time
	afterFunc     func(time.Duration, func()) (stop func() bool)
	pickPositions func(wordCount int) (first int, second int)

	stateLock sync.Mutex

	guest            bool
	top200Registered bool
	consentRead      bittensorLocalKeyConsentRead
	consentAddress   string
	consentFromEpoch int64
	currentEpoch     int64
	justLinked       bool
	linkGeneration   int64

	// read from the store by reload
	storeUnavailable bool
	storedAddress    string
	savedAddress     string
	deletedAddress   string

	// the phrase in memory only: Save now, until the consent succeeds or the user leaves
	saveNowKey    *BittensorLocalKey
	saveNowFailed bool

	screenOpen bool
	screenDone bool
	// the words on the screen. A stored phrase is let go when they hide
	revealedPhrase string
	revealed       bool
	captureActive  bool
	// counts the reveals, so a timer from an earlier one hides nothing
	revealGeneration int
	stopHideTimer    func() bool

	confirming     string
	confirmPhrase  string
	firstPosition  int
	secondPosition int
	wordsRejected  bool

	busy           bool
	failureCode    string
	failureMessage string
}

// api may be nil, as with no session: the flow then offers nothing that signs.
func NewBittensorLocalKeyFlow(api *Api, networkId string, store BittensorLocalKeyStore) *BittensorLocalKeyFlow {
	var consentApi bittensorLocalKeyConsentApi = bittensorLocalKeyNoConsentApi{}
	ctx := context.Background()
	if api != nil {
		consentApi = api
		if api.ctx != nil {
			ctx = api.ctx
		}
	}
	return newBittensorLocalKeyFlow(ctx, consentApi, networkId, store)
}

func newBittensorLocalKeyFlow(ctx context.Context, api bittensorLocalKeyConsentApi, networkId string, store BittensorLocalKeyStore) *BittensorLocalKeyFlow {
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &BittensorLocalKeyFlow{
		ctx:       cancelCtx,
		cancel:    cancel,
		networkId: networkId,
		store:     store,
		api:       api,
		listeners: connect.NewCallbackList[BittensorLocalKeyFlowListener](),
		generate:  BittensorLocalKeyGenerate,
		sign:      BittensorLocalKeySign,
		now:       time.Now,
		afterFunc: func(delay time.Duration, callback func()) func() bool {
			return time.AfterFunc(delay, callback).Stop
		},
		pickPositions: bittensorLocalKeyPickPositions,
	}
	self.reload()
	return self
}

// The address alone: a flow that reaches a log line through %v or %s does not carry a phrase.
func (self *BittensorLocalKeyFlow) String() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return "BittensorLocalKeyFlow(" + self.storedAddress + ")"
}

// As String, for %#v.
func (self *BittensorLocalKeyFlow) GoString() string {
	return self.String()
}

func (self *BittensorLocalKeyFlow) AddBittensorLocalKeyFlowListener(listener BittensorLocalKeyFlowListener) Sub {
	id := self.listeners.Add(listener)
	return newSub(func() { self.listeners.Remove(id) })
}

func (self *BittensorLocalKeyFlow) publish() {
	for _, listener := range self.listeners.Get() {
		connect.HandleError(listener.BittensorLocalKeyFlowChanged)
	}
}

// Ends the flow: a consent on its way is cancelled and the phrase is let go.
func (self *BittensorLocalKeyFlow) Close() {
	self.cancel()
	self.CloseScreen()
}

// A call that can reach the network, on its own goroutine. A panic is the last failure a user could
// otherwise wait on forever, so it ends the busy state like any failed consent, keeping what is stored.
func (self *BittensorLocalKeyFlow) runAsync(work func()) {
	go connect.HandleError(work, func() {
		self.stateLock.Lock()
		if self.saveNowKey != nil && self.busy {
			self.saveNowFailed = true
		}
		self.stateLock.Unlock()
		self.finish(BittensorLocalKeyFailureConsent, "")
	})
}

func (self *BittensorLocalKeyFlow) GetCard() *BittensorLocalKeyCard {
	// outside the lock: the api has its own
	hasNetworkCredential := self.api.HasNetworkCredential()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return bittensorLocalKeyCardOf(self.cardInputsWithLock(hasNetworkCredential))
}

func (self *BittensorLocalKeyFlow) cardInputsWithLock(hasNetworkCredential bool) bittensorLocalKeyCardInputs {
	inputs := bittensorLocalKeyCardInputs{
		networkId:            self.networkId,
		hasNetworkCredential: hasNetworkCredential,
		guest:                self.guest,
		consentRead:          self.consentRead,
		consentAddress:       self.consentAddress,
		consentFromEpoch:     self.consentFromEpoch,
		currentEpoch:         self.currentEpoch,
		justLinked:           self.justLinked,
		storeUnavailable:     self.storeUnavailable,
		storedAddress:        self.storedAddress,
		savedAddress:         self.savedAddress,
		deletedAddress:       self.deletedAddress,
		saveNowFailed:        self.saveNowFailed,
		busy:                 self.busy,
		linkGeneration:       self.linkGeneration,
		failureCode:          self.failureCode,
		failureMessage:       self.failureMessage,
	}
	if self.saveNowKey != nil {
		inputs.saveNowAddress = self.saveNowKey.Address
	}
	return inputs
}

// The card, from everything that decides it. A key in memory is on the Key screen and decides first.
// Then an unknown input offers nothing that could overwrite what it hides. A stored key is shown even
// then, since saving its phrase needs no consent read.
func bittensorLocalKeyCardOf(in bittensorLocalKeyCardInputs) *BittensorLocalKeyCard {
	card := &BittensorLocalKeyCard{
		StoredAddress:  in.storedAddress,
		OwnerOnly:      !in.hasNetworkCredential,
		Busy:           in.busy,
		LinkGeneration: in.linkGeneration,
		FailureCode:    in.failureCode,
		FailureMessage: in.failureMessage,
	}
	if in.saveNowAddress != "" {
		card.Address = in.saveNowAddress
		if in.saveNowFailed {
			card.State = BittensorLocalKeyStateSaveNowConsentFailed
		} else {
			card.State = BittensorLocalKeyStateSaveNowShowing
		}
		return card
	}
	if in.storeUnavailable {
		card.State = BittensorLocalKeyStateStoreUnavailable
		return card
	}
	hasStoredKey := in.storedAddress != ""
	card.OffersKeyScreen = hasStoredKey
	card.KeySaved = hasStoredKey && in.savedAddress == in.storedAddress
	card.BackupReminder = hasStoredKey && !card.KeySaved

	mayMakeKey := in.hasNetworkCredential && !in.guest && in.networkId != ""
	active := in.consentRead == bittensorLocalKeyConsentActive
	switch {
	case in.consentRead == bittensorLocalKeyConsentUnread:
		card.State = BittensorLocalKeyStateLoading
		card.Address = in.storedAddress
	case in.consentRead == bittensorLocalKeyConsentUnavailable:
		card.State = BittensorLocalKeyStateConsentUnavailable
		card.Address = in.storedAddress
	case hasStoredKey && active && in.consentAddress == in.storedAddress:
		card.State = BittensorLocalKeyStateSaveLaterLinked
		card.Address = in.consentAddress
		card.OffersWallet = in.hasNetworkCredential
	case hasStoredKey:
		// a crash between the store and the consent ends here too
		card.State = BittensorLocalKeyStateSaveLaterNotLinked
		card.OffersFinishSetup = mayMakeKey
		card.OffersWallet = in.hasNetworkCredential
		if active {
			card.Address = in.consentAddress
			card.ReplacesPayoutWallet = true
		} else {
			card.Address = in.storedAddress
		}
	case active && in.consentAddress == in.deletedAddress:
		card.State = BittensorLocalKeyStateSavedAndDeleted
		card.Address = in.consentAddress
		card.OffersWallet = in.hasNetworkCredential
	case active:
		card.State = BittensorLocalKeyStateElsewhere
		card.Address = in.consentAddress
		card.OffersWallet = in.hasNetworkCredential
		card.OffersSetUp = mayMakeKey
		card.OffersSaveFirst = mayMakeKey
		card.ReplacesPayoutWallet = true
	default:
		card.State = BittensorLocalKeyStateNone
		card.OffersWallet = in.hasNetworkCredential
		card.OffersSetUp = mayMakeKey
		card.OffersSaveFirst = mayMakeKey
	}
	switch card.State {
	case BittensorLocalKeyStateSaveLaterLinked, BittensorLocalKeyStateSavedAndDeleted:
		if in.currentEpoch == 0 {
			// the epoch is not read yet: a consent just accepted starts next epoch by definition
			card.PayoutsStartNextEpoch = in.justLinked
		} else {
			card.PayoutsStartNextEpoch = in.currentEpoch < in.consentFromEpoch
		}
	}
	return card
}

func (self *BittensorLocalKeyFlow) GetScreen() *BittensorLocalKeyScreen {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.screenWithLock()
}

func (self *BittensorLocalKeyFlow) screenWithLock() *BittensorLocalKeyScreen {
	saveNow := self.saveNowKey != nil
	screen := &BittensorLocalKeyScreen{
		Open:                self.screenOpen,
		SaveNow:             saveNow,
		Revealed:            self.revealed,
		NeedsAuthentication: !saveNow,
		HiddenByCapture:     self.captureActive,
		Confirming:          self.confirming,
		FirstWordPosition:   self.firstPosition,
		SecondWordPosition:  self.secondPosition,
		WordsRejected:       self.wordsRejected,
		WarnsTop200:         !self.top200Registered,
		OffersKeep:          !saveNow && self.storedAddress != "" && self.savedAddress != self.storedAddress,
		OffersRetry:         saveNow && self.saveNowFailed,
		Busy:                self.busy,
		Done:                self.screenDone,
		FailureCode:         self.failureCode,
		FailureMessage:      self.failureMessage,
	}
	if saveNow {
		screen.Address = self.saveNowKey.Address
	} else {
		screen.Address = self.storedAddress
	}
	return screen
}

// Reads the store again. Each visit does, and a key is found here after a crash, a logout or a
// reinstall: nothing but the store says it exists.
func (self *BittensorLocalKeyFlow) Reload() {
	self.reload()
	self.publish()
}

func (self *BittensorLocalKeyFlow) reload() {
	storedAddress := self.store.KeyAddress(self.networkId)
	savedAddress := self.store.ReadNote(self.networkId, bittensorLocalKeyNoteSavedAddress)
	deletedAddress := self.store.ReadNote(self.networkId, bittensorLocalKeyNoteDeletedAddress)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.storeUnavailable = storedAddress == BittensorLocalKeyStoreUnavailable
	if self.storeUnavailable {
		self.storedAddress = ""
	} else {
		self.storedAddress = storedAddress
	}
	self.savedAddress = savedAddress
	self.deletedAddress = deletedAddress
}

// The app read the network's consent. active false: none is in force or starting.
func (self *BittensorLocalKeyFlow) SetConsent(active bool, coldkeySs58 string, fromEpoch int64) {
	self.stateLock.Lock()
	if active {
		self.consentRead = bittensorLocalKeyConsentActive
		self.consentAddress = strings.TrimSpace(coldkeySs58)
		self.consentFromEpoch = fromEpoch
	} else {
		self.consentRead = bittensorLocalKeyConsentNone
		self.consentAddress = ""
		self.consentFromEpoch = 0
	}
	self.stateLock.Unlock()
	self.publish()
}

// The app could not read the consent: the latest read decides, so this is unknown even after a good one.
func (self *BittensorLocalKeyFlow) SetConsentUnavailable() {
	self.stateLock.Lock()
	self.consentRead = bittensorLocalKeyConsentUnavailable
	self.consentAddress = ""
	self.consentFromEpoch = 0
	self.stateLock.Unlock()
	self.publish()
}

// The settlement epoch now. 0 while it is unknown.
func (self *BittensorLocalKeyFlow) SetCurrentEpoch(currentEpoch int64) {
	self.stateLock.Lock()
	self.currentEpoch = currentEpoch
	self.stateLock.Unlock()
	self.publish()
}

// A guest network has no login to come back to, so it gets no key, only the wallet flow.
func (self *BittensorLocalKeyFlow) SetGuest(guest bool) {
	self.stateLock.Lock()
	self.guest = guest
	self.stateLock.Unlock()
	self.publish()
}

// The network has a registered fleet: deleting the key no longer gives up a one-tap claim.
func (self *BittensorLocalKeyFlow) SetTop200Registered(registered bool) {
	self.stateLock.Lock()
	self.top200Registered = registered
	self.stateLock.Unlock()
	self.publish()
}

// Set up payouts, the one tap: a new key, stored and read back, then signed with.
func (self *BittensorLocalKeyFlow) SetUpPayouts(replaceConfirmed bool) {
	self.runAsync(func() {
		self.setUpPayouts(replaceConfirmed)
	})
}

func (self *BittensorLocalKeyFlow) setUpPayouts(replaceConfirmed bool) {
	if !self.beginMakingKey(replaceConfirmed) {
		return
	}
	key, err := self.generate()
	if err != nil {
		self.finish(BittensorLocalKeyFailureGenerate, "")
		return
	}
	// the safety rule: the signing below happens only with what this read back
	if !self.store.AddKey(self.networkId, key.Mnemonic, key.Address) {
		self.reload()
		self.finish(BittensorLocalKeyFailureStore, "")
		return
	}
	phrase := self.store.KeyPhrase(self.networkId)
	address := self.store.KeyAddress(self.networkId)
	if phrase != key.Mnemonic || address != key.Address {
		// this flow wrote it and nothing has signed with it, so there is nothing to keep
		self.store.DeleteKey(self.networkId)
		self.reload()
		self.finish(BittensorLocalKeyFailureStore, "")
		return
	}
	self.reload()
	code, message := self.link(phrase, address)
	self.finish(code, message)
}

// Whether a new key may be made now, and if so the flow is busy making it.
func (self *BittensorLocalKeyFlow) beginMakingKey(replaceConfirmed bool) bool {
	card := self.GetCard()
	self.stateLock.Lock()
	var code string
	switch {
	case self.busy:
		self.stateLock.Unlock()
		return false
	case !card.OffersSetUp:
		code = BittensorLocalKeyFailureNotAllowed
	case card.ReplacesPayoutWallet && !replaceConfirmed:
		code = BittensorLocalKeyFailureReplacementNotConfirmed
	default:
		self.busy = true
		self.failureCode = ""
		self.failureMessage = ""
		self.stateLock.Unlock()
		self.publish()
		return true
	}
	self.failureCode = code
	self.failureMessage = ""
	self.stateLock.Unlock()
	self.publish()
	return false
}

// Finish setup: the stored key signs. This is where a crash between the store and the consent
// resumes, so it never makes a key.
func (self *BittensorLocalKeyFlow) FinishSetup(replaceConfirmed bool) {
	self.runAsync(func() {
		self.finishSetup(replaceConfirmed)
	})
}

func (self *BittensorLocalKeyFlow) finishSetup(replaceConfirmed bool) {
	card := self.GetCard()
	self.stateLock.Lock()
	var code string
	switch {
	case self.busy:
		self.stateLock.Unlock()
		return
	case !card.OffersFinishSetup:
		code = BittensorLocalKeyFailureNotAllowed
	case card.ReplacesPayoutWallet && !replaceConfirmed:
		code = BittensorLocalKeyFailureReplacementNotConfirmed
	default:
		self.busy = true
		self.failureCode = ""
		self.failureMessage = ""
	}
	if code != "" {
		self.failureCode = code
		self.failureMessage = ""
		self.stateLock.Unlock()
		self.publish()
		return
	}
	self.stateLock.Unlock()
	self.publish()

	phrase := self.store.KeyPhrase(self.networkId)
	address := self.store.KeyAddress(self.networkId)
	switch {
	case phrase == BittensorLocalKeyStoreUnavailable || address == BittensorLocalKeyStoreUnavailable:
		self.finish(BittensorLocalKeyFailureStoreUnavailable, "")
		return
	case phrase == "" || address == "":
		self.reload()
		self.finish(BittensorLocalKeyFailureStore, "")
		return
	}
	// the key signs for the address it opens, and the item must agree. Anything else means what was
	// stored is not what was written, and earnings must not go to it
	derivedAddress, err := BittensorLocalKeyAddress(phrase)
	if err != nil || derivedAddress != address {
		self.finish(BittensorLocalKeyFailureStore, "")
		return
	}
	code, message := self.link(phrase, address)
	self.finish(code, message)
}

// Save the recovery phrase first: a new key in memory and the Key screen. Nothing is written or
// signed. replaceConfirmed as for SetUpPayouts.
func (self *BittensorLocalKeyFlow) SaveFirst(replaceConfirmed bool) {
	if !self.beginMakingKey(replaceConfirmed) {
		return
	}
	key, err := self.generate()
	if err != nil {
		self.finish(BittensorLocalKeyFailureGenerate, "")
		return
	}
	self.stateLock.Lock()
	self.saveNowKey = key
	self.saveNowFailed = false
	self.openScreenWithLock()
	self.busy = false
	self.stateLock.Unlock()
	self.publish()
}

// The Key screen for the stored key, hidden.
func (self *BittensorLocalKeyFlow) OpenScreen() {
	self.reload()
	self.stateLock.Lock()
	if self.storedAddress != "" {
		self.openScreenWithLock()
	}
	self.stateLock.Unlock()
	self.publish()
}

func (self *BittensorLocalKeyFlow) openScreenWithLock() {
	self.hideWithLock()
	self.clearConfirmationWithLock()
	self.screenOpen = true
	self.screenDone = false
	self.failureCode = ""
	self.failureMessage = ""
}

// The user left the Key screen. A phrase in memory is discarded: the consent that would make it
// matter has not succeeded, so no earnings went to it.
func (self *BittensorLocalKeyFlow) CloseScreen() {
	self.stateLock.Lock()
	self.hideWithLock()
	self.clearConfirmationWithLock()
	self.screenOpen = false
	self.screenDone = false
	self.saveNowKey = nil
	self.saveNowFailed = false
	self.stateLock.Unlock()
	self.publish()
}

// Tap to reveal. A stored key needs device authentication, which only the app can ask for; with
// authenticated false the words stay hidden. Save now needs none: the phrase is there because the
// user just asked for it, and it has signed nothing.
func (self *BittensorLocalKeyFlow) Reveal(authenticated bool) {
	self.stateLock.Lock()
	if !self.screenOpen || self.revealed || self.captureActive || self.confirming != BittensorLocalKeyConfirmNone {
		self.stateLock.Unlock()
		return
	}
	var phrase string
	if self.saveNowKey != nil {
		phrase = self.saveNowKey.Mnemonic
	} else if !authenticated {
		self.stateLock.Unlock()
		return
	}
	self.stateLock.Unlock()

	if phrase == "" {
		phrase = self.store.KeyPhrase(self.networkId)
		if phrase == "" || phrase == BittensorLocalKeyStoreUnavailable {
			self.setFailure(BittensorLocalKeyFailureStoreUnavailable, "")
			return
		}
	}

	self.stateLock.Lock()
	// what came in during the read wins
	if !self.screenOpen || self.captureActive || self.confirming != BittensorLocalKeyConfirmNone {
		self.stateLock.Unlock()
		return
	}
	self.revealed = true
	self.revealedPhrase = phrase
	self.failureCode = ""
	self.failureMessage = ""
	self.revealGeneration += 1
	generation := self.revealGeneration
	self.stopHideTimer = self.afterFunc(BittensorLocalKeyRevealSeconds*time.Second, func() {
		self.hideGeneration(generation)
	})
	self.stateLock.Unlock()
	self.publish()
}

// Sixty seconds after a reveal. A later reveal has its own timer.
func (self *BittensorLocalKeyFlow) hideGeneration(generation int) {
	self.stateLock.Lock()
	if generation != self.revealGeneration || !self.revealed {
		self.stateLock.Unlock()
		return
	}
	self.hideWithLock()
	self.stateLock.Unlock()
	self.publish()
}

// Hide. The app also calls this when the app leaves the foreground, or a window loses focus.
func (self *BittensorLocalKeyFlow) Hide() {
	self.stateLock.Lock()
	wasRevealed := self.revealed
	self.hideWithLock()
	self.stateLock.Unlock()
	if wasRevealed {
		self.publish()
	}
}

func (self *BittensorLocalKeyFlow) hideWithLock() {
	self.revealed = false
	self.revealedPhrase = ""
	if self.stopHideTimer != nil {
		self.stopHideTimer()
		self.stopHideTimer = nil
	}
}

// The screen is being recorded or mirrored, or stopped being: the words stay hidden meanwhile.
func (self *BittensorLocalKeyFlow) SetCaptureActive(active bool) {
	self.stateLock.Lock()
	self.captureActive = active
	if active {
		self.hideWithLock()
	}
	self.stateLock.Unlock()
	self.publish()
}

// The words, while they show. Empty while hidden. Never in a snapshot, so never in its JSON.
func (self *BittensorLocalKeyFlow) RevealedWords() *StringList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	words := NewStringList()
	if self.revealed {
		words.values = strings.Split(self.revealedPhrase, " ")
	}
	return words
}

// The phrase for an explicit Copy, while the words show. "" while hidden, so there is no Copy then.
func (self *BittensorLocalKeyFlow) PhraseToCopy() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.revealed {
		return ""
	}
	return self.revealedPhrase
}

// I have saved the key, it's OK to delete it now: the words hide and two of them are asked for. A
// stored key needs device authentication first, as for Reveal. On Save now this same step is what
// lets the key sign.
func (self *BittensorLocalKeyFlow) ConfirmDelete(authenticated bool) {
	self.beginConfirmation(BittensorLocalKeyConfirmDelete, authenticated)
}

// I have saved the key and will keep it here: the same proof, for a stored key, without the deletion.
func (self *BittensorLocalKeyFlow) ConfirmKeep(authenticated bool) {
	self.beginConfirmation(BittensorLocalKeyConfirmKeep, authenticated)
}

func (self *BittensorLocalKeyFlow) beginConfirmation(purpose string, authenticated bool) {
	self.stateLock.Lock()
	if !self.screenOpen || self.busy || self.saveNowFailed {
		self.stateLock.Unlock()
		return
	}
	var phrase string
	if self.saveNowKey != nil {
		if purpose != BittensorLocalKeyConfirmDelete {
			self.stateLock.Unlock()
			return
		}
		phrase = self.saveNowKey.Mnemonic
	} else if !authenticated {
		self.stateLock.Unlock()
		return
	}
	self.hideWithLock()
	self.stateLock.Unlock()

	if phrase == "" {
		phrase = self.store.KeyPhrase(self.networkId)
		if phrase == "" || phrase == BittensorLocalKeyStoreUnavailable {
			self.setFailure(BittensorLocalKeyFailureStoreUnavailable, "")
			return
		}
	}

	wordCount := len(strings.Split(phrase, " "))
	first, second := self.pickPositions(wordCount)
	self.stateLock.Lock()
	self.confirming = purpose
	self.confirmPhrase = phrase
	self.firstPosition = first + 1
	self.secondPosition = second + 1
	self.wordsRejected = false
	self.failureCode = ""
	self.failureMessage = ""
	self.stateLock.Unlock()
	self.publish()
}

// Back to the hidden screen.
func (self *BittensorLocalKeyFlow) CancelConfirmation() {
	self.stateLock.Lock()
	self.clearConfirmationWithLock()
	self.stateLock.Unlock()
	self.publish()
}

func (self *BittensorLocalKeyFlow) clearConfirmationWithLock() {
	self.confirming = BittensorLocalKeyConfirmNone
	self.confirmPhrase = ""
	self.firstPosition = 0
	self.secondPosition = 0
	self.wordsRejected = false
}

// The two words the user entered. A match does what the confirmation asked. Anything else keeps it
// up and changes nothing.
func (self *BittensorLocalKeyFlow) SubmitWords(firstWord string, secondWord string) {
	self.runAsync(func() {
		self.submitWords(firstWord, secondWord)
	})
}

func (self *BittensorLocalKeyFlow) submitWords(firstWord string, secondWord string) {
	self.stateLock.Lock()
	if self.confirming == BittensorLocalKeyConfirmNone || self.busy {
		self.stateLock.Unlock()
		return
	}
	words := strings.Split(self.confirmPhrase, " ")
	if self.firstPosition < 1 || len(words) < self.firstPosition ||
		self.secondPosition < 1 || len(words) < self.secondPosition ||
		bittensorLocalKeyNormalizedWord(firstWord) != words[self.firstPosition-1] ||
		bittensorLocalKeyNormalizedWord(secondWord) != words[self.secondPosition-1] {
		self.wordsRejected = true
		self.stateLock.Unlock()
		self.publish()
		return
	}
	purpose := self.confirming
	self.clearConfirmationWithLock()
	saveNowKey := self.saveNowKey
	if saveNowKey != nil {
		self.busy = true
	}
	storedAddress := self.storedAddress
	self.stateLock.Unlock()

	switch {
	case saveNowKey != nil:
		// the user confirmed the phrase: it is safe, and the key may sign
		self.publish()
		self.linkSaveNow(saveNowKey)
	case purpose == BittensorLocalKeyConfirmKeep:
		self.store.WriteNote(self.networkId, bittensorLocalKeyNoteSavedAddress, storedAddress)
		self.reload()
		self.stateLock.Lock()
		self.screenDone = true
		self.stateLock.Unlock()
		self.publish()
	default:
		self.deleteStoredKey(storedAddress)
	}
}

// Sign again after a refused consent, with the same key and no second proof.
func (self *BittensorLocalKeyFlow) RetrySaveNow() {
	self.runAsync(self.retrySaveNow)
}

func (self *BittensorLocalKeyFlow) retrySaveNow() {
	self.stateLock.Lock()
	saveNowKey := self.saveNowKey
	if saveNowKey == nil || !self.saveNowFailed || self.busy {
		self.stateLock.Unlock()
		return
	}
	self.busy = true
	self.failureCode = ""
	self.failureMessage = ""
	self.stateLock.Unlock()
	self.publish()
	self.linkSaveNow(saveNowKey)
}

func (self *BittensorLocalKeyFlow) linkSaveNow(key *BittensorLocalKey) {
	code, message := self.link(key.Mnemonic, key.Address)
	if code != "" {
		self.stateLock.Lock()
		self.saveNowFailed = true
		self.stateLock.Unlock()
		self.finish(code, message)
		return
	}
	// the note says this device made the address, which is what the card reads the deletion from
	self.store.WriteNote(self.networkId, bittensorLocalKeyNoteDeletedAddress, key.Address)
	self.reload()
	self.stateLock.Lock()
	self.saveNowKey = nil
	self.saveNowFailed = false
	self.screenDone = true
	self.stateLock.Unlock()
	self.finish("", "")
}

// The item goes, and a read proves it. Until a read shows it gone the key is treated as still there.
func (self *BittensorLocalKeyFlow) deleteStoredKey(address string) {
	deleted := self.store.DeleteKey(self.networkId)
	gone := deleted && self.store.KeyAddress(self.networkId) == "" && self.store.KeyPhrase(self.networkId) == ""
	if !gone {
		self.reload()
		self.setFailure(BittensorLocalKeyFailureDelete, "")
		return
	}
	self.store.WriteNote(self.networkId, bittensorLocalKeyNoteDeletedAddress, address)
	self.store.WriteNote(self.networkId, bittensorLocalKeyNoteSavedAddress, "")
	self.reload()
	self.stateLock.Lock()
	self.screenDone = true
	self.stateLock.Unlock()
	self.publish()
}

// The one place a key signs. Each caller has the phrase safe: stored and read back, read from the
// store, or confirmed by the user. The same session as a wallet's takes the server's challenge, so
// the key signs only a consent statement naming its own coldkey, and checks the result like any
// wallet's. A failed consent changes nothing in the store. Returns the failure, "" when accepted.
func (self *BittensorLocalKeyFlow) link(phrase string, address string) (string, string) {
	session, err := NewBittensorWalletSession(BittensorWalletManual, BittensorWalletPlatformWeb, BittensorWalletPurposeConsent, "")
	if err != nil {
		return BittensorLocalKeyFailureConsent, err.Error()
	}
	defer session.Cancel()

	challengeCtx, cancelChallenge := context.WithTimeout(self.ctx, bittensorLocalKeyConsentTimeout)
	defer cancelChallenge()
	challenge, err := self.api.SnNetworkConsentChallengeSyncWithContext(challengeCtx, address)
	if err != nil {
		return bittensorLocalKeyConsentFailure(err)
	}
	// the session counts the expiry from here, whatever the device clock
	if err := session.SetNetworkConsentChallenge(challenge, self.now().UnixMilli()); err != nil {
		return BittensorLocalKeyFailureConsent, err.Error()
	}
	signature, err := self.sign(phrase, session.Message())
	if err != nil {
		return BittensorLocalKeyFailureConsent, err.Error()
	}
	result := session.HandleSignature(address, signature, self.now().UnixMilli())
	if !result.Ok() {
		return bittensorLocalKeySessionFailure(result)
	}
	proof := session.Proof()

	submitCtx, cancelSubmit := context.WithTimeout(self.ctx, bittensorLocalKeyConsentTimeout)
	defer cancelSubmit()
	submitted, err := self.api.SnNetworkConsentSubmitSyncWithContext(submitCtx, proof.Address, proof.Message, proof.Signature)
	if err != nil {
		return bittensorLocalKeyConsentFailure(err)
	}
	if submitted == nil {
		return BittensorLocalKeyFailureConsent, "POST /sn/wallet returned no result"
	}
	if submitted.Error != nil {
		if submitted.Error.Code == SnErrorCodeSignatureMismatch {
			return SnErrorCodeSignatureMismatch, submitted.Error.Message
		}
		if submitted.Error.Code == "" {
			return BittensorLocalKeyFailureConsent, submitted.Error.Message
		}
		return BittensorLocalKeyFailureConsent, submitted.Error.Code + ": " + submitted.Error.Message
	}

	self.stateLock.Lock()
	self.consentRead = bittensorLocalKeyConsentActive
	self.consentAddress = address
	self.consentFromEpoch = challenge.FromEpoch
	self.justLinked = true
	self.linkGeneration += 1
	self.stateLock.Unlock()
	return "", ""
}

// The sdk's "code: message" errors keep their code when an app has words of its own for it.
func bittensorLocalKeyConsentFailure(err error) (string, string) {
	text := err.Error()
	code, _, _ := strings.Cut(text, ":")
	switch code {
	case SnErrorCodeNetworkOwnerOnly, SnErrorCodeChallengeExpired:
		return code, text
	}
	if errors.Is(err, context.Canceled) {
		return BittensorLocalKeyFailureConsent, context.Canceled.Error()
	}
	return BittensorLocalKeyFailureConsent, text
}

func bittensorLocalKeySessionFailure(result *BittensorWalletResult) (string, string) {
	if result.ErrorCode == BittensorWalletErrorExpired {
		return SnErrorCodeChallengeExpired, result.ErrorCode
	}
	// the session's text can quote what it was given, which here is never a phrase, only an address
	return BittensorLocalKeyFailureConsent, result.ErrorCode
}

// Ends the busy state with the action's result.
func (self *BittensorLocalKeyFlow) finish(failureCode string, failureMessage string) {
	self.stateLock.Lock()
	self.busy = false
	self.failureCode = failureCode
	self.failureMessage = failureMessage
	self.stateLock.Unlock()
	self.publish()
}

func (self *BittensorLocalKeyFlow) setFailure(failureCode string, failureMessage string) {
	self.stateLock.Lock()
	self.failureCode = failureCode
	self.failureMessage = failureMessage
	self.stateLock.Unlock()
	self.publish()
}

// The one local notification of an unsaved key, when the first epoch becomes claimable, since that is
// when the key starts to be worth something. Decided on each refresh of the app's earnings, from the
// current epoch's schedule: currentEpoch and claimOpenMillis are that epoch's, epochBlocks the policy's,
// and 0 for any means not known. claimable: something is claimable now. The app does what this says
// and reports it with ReminderScheduled or ReminderDelivered, and an app without permission to notify
// reports nothing, so a later permission still gets the reminder. Nothing repeats after that.
func (self *BittensorLocalKeyFlow) Reminder(currentEpoch int64, claimOpenMillis int64, epochBlocks int64, claimable bool) *BittensorLocalKeyReminder {
	self.stateLock.Lock()
	unsavedAndPaid := self.storedAddress != "" && self.savedAddress != self.storedAddress &&
		self.consentRead == bittensorLocalKeyConsentActive && self.consentAddress == self.storedAddress
	fromEpoch := self.consentFromEpoch
	self.stateLock.Unlock()

	nowMillis := self.now().UnixMilli()
	delivered := self.store.ReadNote(self.networkId, bittensorLocalKeyNoteReminderDelivered) == bittensorLocalKeyNoteReminderDeliveredValue
	scheduledAtMillis, _ := strconv.ParseInt(self.store.ReadNote(self.networkId, bittensorLocalKeyNoteReminderAtMillis), 10, 64)

	if !unsavedAndPaid {
		if 0 < scheduledAtMillis {
			self.store.WriteNote(self.networkId, bittensorLocalKeyNoteReminderAtMillis, "")
			return &BittensorLocalKeyReminder{Action: BittensorLocalKeyReminderCancel}
		}
		return &BittensorLocalKeyReminder{Action: BittensorLocalKeyReminderNone}
	}
	if delivered {
		return &BittensorLocalKeyReminder{Action: BittensorLocalKeyReminderNone}
	}
	if 0 < scheduledAtMillis && scheduledAtMillis <= nowMillis {
		// the system has delivered it by now
		self.ReminderDelivered()
		return &BittensorLocalKeyReminder{Action: BittensorLocalKeyReminderNone}
	}
	if claimable {
		return &BittensorLocalKeyReminder{Action: BittensorLocalKeyReminderDeliverNow}
	}
	if currentEpoch <= 0 || claimOpenMillis <= 0 || epochBlocks <= 0 {
		return &BittensorLocalKeyReminder{Action: BittensorLocalKeyReminderNone}
	}
	atMillis := claimOpenMillis + (fromEpoch-currentEpoch)*epochBlocks*snBlockSeconds*1000
	if atMillis <= nowMillis {
		return &BittensorLocalKeyReminder{Action: BittensorLocalKeyReminderDeliverNow}
	}
	if atMillis == scheduledAtMillis {
		return &BittensorLocalKeyReminder{Action: BittensorLocalKeyReminderNone}
	}
	return &BittensorLocalKeyReminder{Action: BittensorLocalKeyReminderSchedule, AtMillis: atMillis}
}

// The app gave the system the notification for atMillis.
func (self *BittensorLocalKeyFlow) ReminderScheduled(atMillis int64) {
	self.store.WriteNote(self.networkId, bittensorLocalKeyNoteReminderAtMillis, strconv.FormatInt(atMillis, 10))
}

// The notification went out: it is the only one.
func (self *BittensorLocalKeyFlow) ReminderDelivered() {
	self.store.WriteNote(self.networkId, bittensorLocalKeyNoteReminderDelivered, bittensorLocalKeyNoteReminderDeliveredValue)
	self.store.WriteNote(self.networkId, bittensorLocalKeyNoteReminderAtMillis, "")
}

// Whether the network has a key on this device whose phrase is not known to be saved. The logout and
// account deletion confirmations warn on it. A store that cannot answer warns nothing: a logout keeps
// the key anyway.
func BittensorLocalKeyNeedsBackup(store BittensorLocalKeyStore, networkId string) bool {
	address := store.KeyAddress(networkId)
	if address == "" || address == BittensorLocalKeyStoreUnavailable {
		return false
	}
	return store.ReadNote(networkId, bittensorLocalKeyNoteSavedAddress) != address
}

// Removes everything this device holds for the network, after an account deletion. False if the key
// could not be shown gone.
func BittensorLocalKeyForgetNetwork(store BittensorLocalKeyStore, networkId string) bool {
	deleted := store.DeleteKey(networkId)
	gone := deleted && store.KeyAddress(networkId) == "" && store.KeyPhrase(networkId) == ""
	for _, name := range []string{
		bittensorLocalKeyNoteSavedAddress,
		bittensorLocalKeyNoteDeletedAddress,
		bittensorLocalKeyNoteReminderAtMillis,
		bittensorLocalKeyNoteReminderDelivered,
	} {
		store.WriteNote(networkId, name, "")
	}
	return gone
}

// Two different words to ask for, in order, from the system's secure randomness. It only has to stop a
// user from learning which two get asked, so a failed read falling back to the first and last is fine.
func bittensorLocalKeyPickPositions(wordCount int) (int, int) {
	if wordCount < 2 {
		return 0, 0
	}
	var seed [8]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return 0, wordCount - 1
	}
	value := binary.BigEndian.Uint64(seed[:])
	first := int(value % uint64(wordCount))
	second := int((value >> 32) % uint64(wordCount-1))
	if first <= second {
		second += 1
	}
	if second < first {
		first, second = second, first
	}
	return first, second
}

// What the user meant by the word they typed: the list is lower case, with no spaces.
func bittensorLocalKeyNormalizedWord(word string) string {
	return strings.ToLower(strings.TrimSpace(word))
}
