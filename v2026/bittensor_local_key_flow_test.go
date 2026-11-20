//go:build !ios_extension && !js

// The payout key's flow (apple BTEASY.md, Tests). Everything is on fakes: a store and an api that
// record each call in one log, so the order of the calls is what is tested, a clock and timers the
// test moves, and fixed keys. The challenge is the real statement shape, and the real wallet session
// takes it, so the signature is checked by the server's own check.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bip39 "github.com/cosmos/go-bip39"
)

const (
	bittensorLocalKeyFlowTestNetwork      = "01890000-0000-7000-8000-000000000001"
	bittensorLocalKeyFlowTestOtherNetwork = "01890000-0000-7000-8000-000000000002"
	bittensorLocalKeyFlowTestNowMillis    = int64(1_800_000_000_000)
)

// the calls of a run, in order, across the store, the api and the signer
type testingLocalKeyLog struct {
	stateLock sync.Mutex
	events    []string
}

func (self *testingLocalKeyLog) add(event string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, event)
}

func (self *testingLocalKeyLog) all() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.events)
}

func (self *testingLocalKeyLog) has(event string) bool {
	return slices.Contains(self.all(), event)
}

// Fails unless each event is in the log, in this order, with none of them before the one ahead of it.
func (self *testingLocalKeyLog) requireOrder(t *testing.T, events ...string) {
	t.Helper()
	all := self.all()
	at := -1
	for _, event := range events {
		next := slices.Index(all, event)
		if next < 0 {
			t.Fatalf("no %s in %v", event, all)
		}
		if next < at {
			t.Fatalf("%s came too early in %v", event, all)
		}
		at = next
	}
}

type testingLocalKeyItem struct {
	mnemonic string
	address  string
}

// A store as the contract says, with the ways a real one fails.
type testingLocalKeyStore struct {
	stateLock sync.Mutex
	log       *testingLocalKeyLog
	items     map[string]testingLocalKeyItem
	notes     map[string]string

	refuseAdd bool
	// the next reads answer this instead of what was written
	readBackMnemonic string
	readBackAddress  string
	unavailable      bool
	// the device has no secure storage at all, as a Linux desktop with no Secret Service
	absent       bool
	refuseDelete bool
	// says it deleted and did not
	deleteDoesNothing bool
}

func newTestingLocalKeyStore(log *testingLocalKeyLog) *testingLocalKeyStore {
	return &testingLocalKeyStore{
		log:   log,
		items: map[string]testingLocalKeyItem{},
		notes: map[string]string{},
	}
}

func (self *testingLocalKeyStore) AddKey(networkId string, mnemonic string, address string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.log.add("store.add")
	if self.refuseAdd || self.unavailable || self.absent {
		return false
	}
	if _, exists := self.items[networkId]; exists {
		return false
	}
	self.items[networkId] = testingLocalKeyItem{mnemonic: mnemonic, address: address}
	return true
}

func (self *testingLocalKeyStore) KeyPhrase(networkId string) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.log.add("store.phrase")
	if self.absent {
		return BittensorLocalKeyStoreAbsent
	}
	if self.unavailable {
		return BittensorLocalKeyStoreUnavailable
	}
	if self.readBackMnemonic != "" {
		return self.readBackMnemonic
	}
	return self.items[networkId].mnemonic
}

func (self *testingLocalKeyStore) KeyAddress(networkId string) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.log.add("store.address")
	if self.absent {
		return BittensorLocalKeyStoreAbsent
	}
	if self.unavailable {
		return BittensorLocalKeyStoreUnavailable
	}
	if self.readBackAddress != "" {
		return self.readBackAddress
	}
	return self.items[networkId].address
}

func (self *testingLocalKeyStore) DeleteKey(networkId string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.log.add("store.delete")
	if self.refuseDelete || self.absent {
		return false
	}
	if !self.deleteDoesNothing {
		delete(self.items, networkId)
	}
	return true
}

func (self *testingLocalKeyStore) ReadNote(networkId string, name string) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.notes[networkId+"/"+name]
}

func (self *testingLocalKeyStore) WriteNote(networkId string, name string, value string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if value == "" {
		delete(self.notes, networkId+"/"+name)
		return
	}
	self.notes[networkId+"/"+name] = value
}

type testingLocalKeySubmission struct {
	coldkeySs58 string
	message     string
	signature   string
}

// The consent calls: a challenge in the server's shape for the coldkey asked for, and a record of
// what was submitted.
type testingLocalKeyApi struct {
	t            *testing.T
	stateLock    sync.Mutex
	log          *testingLocalKeyLog
	credential   bool
	challengeErr error
	submitErr    error
	submitResult *SnSetWalletResult
	// the message a challenge carries in place of the one for the coldkey
	messageFor func(coldkeySs58 string) string
	// the coldkeys the challenges were asked for
	challenges  []string
	submissions []testingLocalKeySubmission
}

func (self *testingLocalKeyApi) HasNetworkCredential() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.credential
}

func (self *testingLocalKeyApi) SnNetworkConsentChallengeSyncWithContext(ctx context.Context, coldkeySs58 string) (*SnNetworkConsentChallengeResult, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.log.add("api.challenge")
	self.challenges = append(self.challenges, coldkeySs58)
	if self.challengeErr != nil {
		return nil, self.challengeErr
	}
	from := int64(snConsentTestEpoch + 1)
	through := from + SnNetworkConsentEpochSpan
	message := snConsentTestMessage(self.t, coldkeySs58, from, through, nil)
	if self.messageFor != nil {
		message = self.messageFor(coldkeySs58)
	}
	return &SnNetworkConsentChallengeResult{
		Message:      message,
		ColdkeySs58:  coldkeySs58,
		FromEpoch:    from,
		ThroughEpoch: through,
	}, nil
}

func (self *testingLocalKeyApi) SnNetworkConsentSubmitSyncWithContext(ctx context.Context, coldkeySs58 string, message string, signatureHex string) (*SnSetWalletResult, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.log.add("api.submit")
	self.submissions = append(self.submissions, testingLocalKeySubmission{coldkeySs58, message, signatureHex})
	if self.submitErr != nil {
		return nil, self.submitErr
	}
	if self.submitResult != nil {
		return self.submitResult, nil
	}
	return &SnSetWalletResult{}, nil
}

// One flow on the fakes, with its log, its clock, its hide timers and a key it makes.
type testingLocalKeyRun struct {
	t      *testing.T
	log    *testingLocalKeyLog
	store  *testingLocalKeyStore
	api    *testingLocalKeyApi
	flow   *BittensorLocalKeyFlow
	key    *BittensorLocalKey
	clock  *testingLocalKeyClock
	timers *testingLocalKeyTimers
}

type testingLocalKeyClock struct {
	stateLock sync.Mutex
	nowMillis int64
}

func (self *testingLocalKeyClock) now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return time.UnixMilli(self.nowMillis)
}

// The hide timers the flow set, which fire when the test says.
type testingLocalKeyTimers struct {
	stateLock sync.Mutex
	delays    []time.Duration
	callbacks []func()
	stopped   []bool
}

func (self *testingLocalKeyTimers) afterFunc(delay time.Duration, callback func()) func() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.delays = append(self.delays, delay)
	self.callbacks = append(self.callbacks, callback)
	self.stopped = append(self.stopped, false)
	index := len(self.callbacks) - 1
	return func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.stopped[index] = true
		return true
	}
}

func (self *testingLocalKeyTimers) fire(index int) {
	self.stateLock.Lock()
	callback := self.callbacks[index]
	self.stateLock.Unlock()
	callback()
}

func testingLocalKeyFromSeed(t *testing.T, seed byte) *BittensorLocalKey {
	t.Helper()
	entropy := make([]byte, bittensorLocalKeyEntropyByteCount)
	for i := range entropy {
		entropy[i] = seed
	}
	key, err := bittensorLocalKeyFromEntropy(entropy)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func newTestingLocalKeyRun(t *testing.T) *testingLocalKeyRun {
	t.Helper()
	log := &testingLocalKeyLog{}
	return newTestingLocalKeyRunOn(t, log, newTestingLocalKeyStore(log), bittensorLocalKeyFlowTestNetwork)
}

// A flow on a store that already exists, as after a crash, a logout or a reinstall.
func newTestingLocalKeyRunOn(t *testing.T, log *testingLocalKeyLog, store *testingLocalKeyStore, networkId string) *testingLocalKeyRun {
	t.Helper()
	api := &testingLocalKeyApi{t: t, log: log, credential: true}
	flow := newBittensorLocalKeyFlow(t.Context(), api, networkId, store)
	run := &testingLocalKeyRun{
		t:      t,
		log:    log,
		store:  store,
		api:    api,
		flow:   flow,
		key:    testingLocalKeyFromSeed(t, 1),
		clock:  &testingLocalKeyClock{nowMillis: bittensorLocalKeyFlowTestNowMillis},
		timers: &testingLocalKeyTimers{},
	}
	flow.generate = func() (*BittensorLocalKey, error) {
		log.add("generate")
		return run.key, nil
	}
	flow.sign = func(mnemonic string, message string) (string, error) {
		log.add("sign")
		return BittensorLocalKeySign(mnemonic, message)
	}
	flow.now = run.clock.now
	flow.afterFunc = run.timers.afterFunc
	// words 3 and 9
	flow.pickPositions = func(address string, wordCount int) (int, int) {
		return 2, 8
	}
	return run
}

// The two words the check asks for.
func (self *testingLocalKeyRun) rightWords() (string, string) {
	words := strings.Split(self.key.Mnemonic, " ")
	return words[2], words[8]
}

func (self *testingLocalKeyRun) requireState(state string) *BittensorLocalKeyCard {
	self.t.Helper()
	card := self.flow.GetCard()
	if card.State != state {
		self.t.Fatalf("state %q, want %q: %+v", card.State, state, card)
	}
	return card
}

// No consent is in force, and the session is the owner's: what the card offers from.
func newTestingLocalKeyRunWithNoConsent(t *testing.T) *testingLocalKeyRun {
	t.Helper()
	run := newTestingLocalKeyRun(t)
	run.flow.SetConsent(false, "", 0)
	return run
}

// Save later done: the key stored, the consent paying it.
func newTestingLocalKeyRunLinked(t *testing.T) *testingLocalKeyRun {
	t.Helper()
	run := newTestingLocalKeyRunWithNoConsent(t)
	run.flow.setUpPayouts(false)
	run.requireState(BittensorLocalKeyStateSaveLaterLinked)
	return run
}

// MARK: no signing before the phrase is safe

// Set up payouts: the key is written and read back, and only then does anything go to the server or
// get signed. What it signs is what the server's check accepts, for the address the key opens.
func TestBittensorLocalKeyFlowSaveLaterStoresAndReadsBackBeforeSigning(t *testing.T) {
	run := newTestingLocalKeyRunWithNoConsent(t)
	if card := run.requireState(BittensorLocalKeyStateNone); !card.OffersSetUp || !card.OffersSaveFirst || !card.OffersWallet {
		t.Fatalf("the card offers %+v", card)
	}
	run.flow.setUpPayouts(false)

	run.log.requireOrder(t, "generate", "store.add", "store.phrase", "api.challenge", "sign", "api.submit")
	if got := run.store.items[bittensorLocalKeyFlowTestNetwork]; got.mnemonic != run.key.Mnemonic || got.address != run.key.Address {
		t.Fatalf("stored %v", got.address)
	}
	if !slices.Equal(run.api.challenges, []string{run.key.Address}) {
		t.Fatalf("challenged %v", run.api.challenges)
	}
	if len(run.api.submissions) != 1 {
		t.Fatalf("submitted %d", len(run.api.submissions))
	}
	submitted := run.api.submissions[0]
	valid, err := testingBittensorServerVerify(submitted.coldkeySs58, submitted.message, submitted.signature)
	if err != nil || !valid || submitted.coldkeySs58 != run.key.Address {
		t.Fatalf("the server's check refused the consent: %v, %v, %s", valid, err, submitted.coldkeySs58)
	}
	card := run.requireState(BittensorLocalKeyStateSaveLaterLinked)
	if card.Address != run.key.Address || !card.PayoutsStartNextEpoch || !card.BackupReminder || !card.OffersKeyScreen || card.Busy {
		t.Fatalf("after the tap the card is %+v", card)
	}
	if card.LinkGeneration != 1 {
		t.Fatalf("link generation %d", card.LinkGeneration)
	}
}

// If the phrase is not safe the app does not sign, whatever way the store fails, and a key that is not
// this flow's own stays where it was.
func TestBittensorLocalKeyFlowSaveLaterNeverSignsWhenTheStoreFails(t *testing.T) {
	other := testingLocalKeyFromSeed(t, 2)
	for _, row := range []struct {
		name string
		set  func(*testingLocalKeyStore)
		// the item that must remain, if any
		remains string
	}{
		{name: "the add is refused", set: func(store *testingLocalKeyStore) { store.refuseAdd = true }},
		{name: "the phrase reads back different", set: func(store *testingLocalKeyStore) { store.readBackMnemonic = other.Mnemonic }},
		{name: "the address reads back different", set: func(store *testingLocalKeyStore) { store.readBackAddress = other.Address }},
	} {
		t.Run(row.name, func(t *testing.T) {
			run := newTestingLocalKeyRunWithNoConsent(t)
			row.set(run.store)
			run.flow.setUpPayouts(false)
			for _, event := range []string{"api.challenge", "sign", "api.submit"} {
				if run.log.has(event) {
					t.Errorf("%s happened: %v", event, run.log.all())
				}
			}
			if _, stored := run.store.items[bittensorLocalKeyFlowTestNetwork]; stored {
				t.Error("a key that failed its read back stayed in the store")
			}
			run.store.readBackMnemonic, run.store.readBackAddress = "", ""
			run.flow.Reload()
			card := run.requireState(BittensorLocalKeyStateNone)
			if card.FailureCode != BittensorLocalKeyFailureStore || card.Busy {
				t.Fatalf("the card is %+v", card)
			}
		})
	}
}

// A store that cannot be read reads as unknown, never as "no key", so the card offers no new key that
// would hide the one it cannot see. The store's own refusal stops a new key from replacing one.
func TestBittensorLocalKeyFlowLockedStoreOffersNothingAndAnItemIsNeverReplaced(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	run.store.unavailable = true
	run.flow.Reload()
	card := run.requireState(BittensorLocalKeyStateStoreUnavailable)
	if card.OffersSetUp || card.OffersSaveFirst || card.OffersFinishSetup || card.OffersWallet {
		t.Fatalf("a locked store offers %+v", card)
	}
	events := len(run.log.all())
	run.flow.setUpPayouts(true)
	if got := run.log.all()[events:]; slices.Contains(got, "store.add") || slices.Contains(got, "sign") {
		t.Fatalf("a locked store still made a key: %v", got)
	}
	if run.flow.GetCard().FailureCode != BittensorLocalKeyFailureNotAllowed {
		t.Fatalf("failure %q", run.flow.GetCard().FailureCode)
	}

	// a store whose item came in after the card was read: the add refuses, and that item is kept
	run = newTestingLocalKeyRunWithNoConsent(t)
	other := testingLocalKeyFromSeed(t, 2)
	run.store.items[bittensorLocalKeyFlowTestNetwork] = testingLocalKeyItem{other.Mnemonic, other.Address}
	run.flow.setUpPayouts(false)
	if run.log.has("sign") || run.store.items[bittensorLocalKeyFlowTestNetwork].mnemonic != other.Mnemonic {
		t.Fatalf("an item was replaced, or a key signed: %v", run.log.all())
	}
	if run.log.has("store.delete") {
		t.Fatal("the flow deleted an item it did not write")
	}
}

// A device with no secure storage at all, as a Linux desktop with no Secret Service, offers no key: not
// the one tap, not Save now (whose Keep would store it), not Change wallet's key. The card is the
// wallet card, as for a session that can't make a key, so the disclaimer's first sentence never shows
// there, and the claim card carries the last one. A consent in force reads as paid elsewhere.
func TestBittensorLocalKeyFlowNoSecureStorageOffersOnlyTheWallet(t *testing.T) {
	// the control: the same store with secure storage offers the key
	control := newTestingLocalKeyRunWithNoConsent(t)
	if card := control.requireState(BittensorLocalKeyStateNone); !card.OffersSetUp || !card.ShowsKeyPath {
		t.Fatalf("CONTROL FAILED: the card is %+v", card)
	}

	run := newTestingLocalKeyRunWithNoConsent(t)
	run.store.absent = true
	run.flow.Reload()
	card := run.requireState(BittensorLocalKeyStateNone)
	if card.OffersSetUp || card.OffersSaveFirst || card.OffersFinishSetup || card.OffersKeyScreen || card.ShowsKeyPath ||
		!card.OffersWallet || card.StoredAddress != "" || card.BackupReminder {
		t.Fatalf("no secure storage offers %+v", card)
	}
	if card.FundsDisclaimer != BittensorFundsDisclaimerNone || card.ClaimFundsDisclaimer != BittensorFundsDisclaimerFundsOnly {
		t.Fatalf("the wallet card's disclaimers are %q and %q", card.FundsDisclaimer, card.ClaimFundsDisclaimer)
	}
	for place, want := range map[string]string{
		BittensorFundsDisclaimerPlacePayoutCard:   BittensorFundsDisclaimerNone,
		BittensorFundsDisclaimerPlaceClaimCard:    BittensorFundsDisclaimerFundsOnly,
		BittensorFundsDisclaimerPlaceChangeWallet: BittensorFundsDisclaimerNone,
		BittensorFundsDisclaimerPlaceClaimSheet:   BittensorFundsDisclaimerFundsOnly,
		BittensorFundsDisclaimerPlaceConvert:      BittensorFundsDisclaimerFundsOnly,
	} {
		if got := run.flow.FundsDisclaimerAt(place); got != want {
			t.Errorf("no secure storage: %s says %q, want %q", place, got, want)
		}
	}

	run.flow.setUpPayouts(true)
	run.flow.SaveFirst(true)
	for _, event := range []string{"generate", "store.add", "api.challenge", "sign"} {
		if run.log.has(event) {
			t.Fatalf("%s happened with no secure storage: %v", event, run.log.all())
		}
	}
	if got := run.flow.GetCard(); got.FailureCode != BittensorLocalKeyFailureNotAllowed {
		t.Fatalf("the card is %+v", got)
	}
	run.flow.OpenScreen()
	if screen := run.flow.GetScreen(); screen.Open || screen.SaveNow {
		t.Fatalf("no secure storage opened %+v", screen)
	}
	if run.flow.NeedsBackup() {
		t.Fatal("no secure storage warns of a key")
	}
	if got := run.flow.Reminder(42, bittensorLocalKeyFlowTestNowMillis+1000, 50_400, true); got.Action != BittensorLocalKeyReminderNone {
		t.Fatalf("no secure storage reminds: %+v", got)
	}

	// a consent in force is paid to a key that is not here
	run.flow.SetConsent(true, testingLocalKeyFromSeed(t, 2).Address, 40)
	card = run.requireState(BittensorLocalKeyStateElsewhere)
	if card.OffersSetUp || card.OffersSaveFirst || !card.OffersWallet || card.FundsDisclaimer != BittensorFundsDisclaimerFundsOnly {
		t.Fatalf("no secure storage with a consent shows %+v", card)
	}
	if got := run.flow.FundsDisclaimerAt(BittensorFundsDisclaimerPlaceChangeWallet); got != BittensorFundsDisclaimerNone {
		t.Fatalf("Change wallet offers a key with no secure storage: %q", got)
	}
}

// A screen open on a key whose store has gone shows no words, and never the store's answer as words.
func TestBittensorLocalKeyFlowRevealWhenTheSecureStorageIsGone(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	run.flow.OpenScreen()
	run.store.absent = true
	run.flow.Reveal(true)
	screen := run.flow.GetScreen()
	if screen.Revealed || run.flow.PhraseToCopy() != "" || len(run.flow.RevealedWords().values) != 0 ||
		screen.FailureCode != BittensorLocalKeyFailureStoreUnavailable {
		t.Fatalf("the screen is %+v", screen)
	}
	run.flow.ConfirmKeep()
	screen = run.flow.GetScreen()
	if screen.Confirming != BittensorLocalKeyConfirmNone || screen.FailureCode != BittensorLocalKeyFailureStoreUnavailable {
		t.Fatalf("the screen is %+v", screen)
	}
	// finish setup reads the phrase again, and the store's answer is never signed with
	run.flow.SetConsent(true, testingLocalKeyFromSeed(t, 2).Address, 50)
	if card := run.flow.GetCard(); !card.OffersFinishSetup {
		t.Fatalf("CONTROL FAILED: the card is %+v", card)
	}
	events := len(run.log.all())
	run.flow.finishSetup(true)
	if got := run.log.all()[events:]; slices.Contains(got, "sign") || slices.Contains(got, "api.challenge") {
		t.Fatalf("finish setup went on with no secure storage: %v", got)
	}
	if code := run.flow.GetCard().FailureCode; code != BittensorLocalKeyFailureStoreUnavailable {
		t.Fatalf("finish setup failed with %q", code)
	}
}

// Save now: nothing touches the store, and nothing signs until the user has entered the two words.
func TestBittensorLocalKeyFlowSaveNowNeverSignsBeforeTheWordsAndNeverStores(t *testing.T) {
	run := newTestingLocalKeyRunWithNoConsent(t)
	run.flow.SaveFirst(false)
	card := run.requireState(BittensorLocalKeyStateSaveNowShowing)
	if card.Address != run.key.Address {
		t.Fatalf("the card is %+v", card)
	}
	screen := run.flow.GetScreen()
	if !screen.Open || !screen.SaveNow || screen.Revealed || screen.NeedsAuthentication || screen.Address != run.key.Address {
		t.Fatalf("the screen opens as %+v", screen)
	}

	run.flow.Reveal(false)
	run.flow.ConfirmDelete()
	screen = run.flow.GetScreen()
	if screen.Confirming != BittensorLocalKeyConfirmDelete || screen.Revealed || screen.FirstWordPosition != 3 || screen.SecondWordPosition != 9 {
		t.Fatalf("the confirmation is %+v", screen)
	}
	first, second := run.rightWords()

	// wrong words, in either place or both, sign nothing and keep the check up
	for _, attempt := range [][2]string{
		{"", ""},
		{first, "wrong"},
		{"wrong", second},
		{second, first},
	} {
		run.flow.submitWords(attempt[0], attempt[1], true)
		screen = run.flow.GetScreen()
		if !screen.WordsRejected || screen.Confirming != BittensorLocalKeyConfirmDelete {
			t.Fatalf("%v: the screen is %+v", attempt, screen)
		}
		if run.log.has("sign") || run.log.has("api.challenge") {
			t.Fatalf("%v signed: %v", attempt, run.log.all())
		}
	}

	// how a person types them still counts, and Save now asks for no device authentication
	run.flow.submitWords(" "+strings.ToUpper(first)+" ", second+"\n", false)
	run.log.requireOrder(t, "generate", "api.challenge", "sign", "api.submit")
	if run.log.has("store.add") || run.log.has("store.phrase") || run.log.has("store.delete") {
		t.Fatalf("Save now touched the secret store: %v", run.log.all())
	}
	if len(run.store.items) != 0 {
		t.Fatal("Save now left a key in the store")
	}

	// the key is gone from memory, and the card reads the deletion from the consent
	card = run.requireState(BittensorLocalKeyStateSavedAndDeleted)
	if card.Address != run.key.Address || !card.PayoutsStartNextEpoch || card.BackupReminder {
		t.Fatalf("the card is %+v", card)
	}
	if !run.flow.GetScreen().Done || run.flow.GetScreen().SaveNow {
		t.Fatalf("the screen is %+v", run.flow.GetScreen())
	}
	run.flow.Reveal(true)
	if run.flow.RevealedWords().Len() != 0 || run.flow.PhraseToCopy() != "" {
		t.Fatal("the phrase is still in memory after the consent succeeded")
	}
}

// Only the consent statement the server issued for this key gets signed, through the same session a
// wallet's answer goes through.
func TestBittensorLocalKeyFlowSignsOnlyAConsentNamingItsOwnColdkey(t *testing.T) {
	other := testingLocalKeyFromSeed(t, 2)
	for _, row := range []struct {
		name    string
		message func(t *testing.T) string
	}{
		{"another coldkey", func(t *testing.T) string {
			return snConsentTestMessage(t, other.Address, snConsentTestEpoch+1, snConsentTestEpoch+1+SnNetworkConsentEpochSpan, nil)
		}},
		{"a sign-in challenge", func(t *testing.T) string {
			return "ur.io wants you to sign in with your Bittensor account"
		}},
		{"a transfer's bytes", func(t *testing.T) string {
			return "0x0503deadbeef"
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			run := newTestingLocalKeyRunWithNoConsent(t)
			run.api.messageFor = func(string) string { return row.message(t) }
			run.flow.setUpPayouts(false)
			if run.log.has("sign") || run.log.has("api.submit") {
				t.Fatalf("signed: %v", run.log.all())
			}
			card := run.requireState(BittensorLocalKeyStateSaveLaterNotLinked)
			if card.FailureCode != BittensorLocalKeyFailureConsent {
				t.Fatalf("the card is %+v", card)
			}
		})
	}
}

// MARK: failures

// A failed consent keeps the stored key, and Finish setup signs with that key, never a new one.
func TestBittensorLocalKeyFlowFailedConsentKeepsTheKeyAndFinishSetupUsesIt(t *testing.T) {
	for _, row := range []struct {
		name string
		set  func(*testingLocalKeyApi)
		code string
	}{
		{"the challenge fails", func(api *testingLocalKeyApi) { api.challengeErr = errors.New("server_error: 502") }, BittensorLocalKeyFailureConsent},
		{"not the owner", func(api *testingLocalKeyApi) {
			api.challengeErr = fmt.Errorf("%s: %w", SnErrorCodeNetworkOwnerOnly, ErrNetworkCredentialRequired)
		}, SnErrorCodeNetworkOwnerOnly},
		{"it expired", func(api *testingLocalKeyApi) {
			api.submitErr = fmt.Errorf("%s: late", SnErrorCodeChallengeExpired)
		}, SnErrorCodeChallengeExpired},
		{"the signature is refused", func(api *testingLocalKeyApi) {
			api.submitResult = &SnSetWalletResult{Error: &SnSetWalletError{Code: SnErrorCodeSignatureMismatch, Message: "no match"}}
		}, SnErrorCodeSignatureMismatch},
		{"the server refuses otherwise", func(api *testingLocalKeyApi) {
			api.submitResult = &SnSetWalletResult{Error: &SnSetWalletError{Code: "server_error", Message: "400"}}
		}, BittensorLocalKeyFailureConsent},
	} {
		t.Run(row.name, func(t *testing.T) {
			run := newTestingLocalKeyRunWithNoConsent(t)
			row.set(run.api)
			run.flow.setUpPayouts(false)

			if run.store.items[bittensorLocalKeyFlowTestNetwork].mnemonic != run.key.Mnemonic {
				t.Fatal("a failed consent lost the key")
			}
			card := run.requireState(BittensorLocalKeyStateSaveLaterNotLinked)
			if card.FailureCode != row.code || !card.OffersFinishSetup || card.ReplacesPayoutWallet || card.Busy || card.Address != run.key.Address {
				t.Fatalf("the card is %+v", card)
			}

			run.api.challengeErr, run.api.submitErr, run.api.submitResult = nil, nil, nil
			run.key = testingLocalKeyFromSeed(t, 9)
			run.flow.finishSetup(false)
			card = run.requireState(BittensorLocalKeyStateSaveLaterLinked)
			if card.FailureCode != "" || card.Address == run.key.Address {
				t.Fatalf("the card is %+v", card)
			}
			if got := len(slices.DeleteFunc(run.log.all(), func(event string) bool { return event != "generate" })); got != 1 {
				t.Fatalf("Finish setup made a new key: %d", got)
			}
		})
	}
}

// A crash between the store and the consent: the next flow finds the key in the store, the card reads
// Finish setup, and that signs with the stored key.
func TestBittensorLocalKeyFlowCrashBetweenStoreAndConsentResumesAtFinishSetup(t *testing.T) {
	log := &testingLocalKeyLog{}
	store := newTestingLocalKeyStore(log)
	before := newTestingLocalKeyRunOn(t, log, store, bittensorLocalKeyFlowTestNetwork)
	before.flow.SetConsent(false, "", 0)
	// the process dies when the challenge is asked for
	before.api.challengeErr = errors.New("killed")
	before.flow.setUpPayouts(false)

	after := newTestingLocalKeyRunOn(t, log, store, bittensorLocalKeyFlowTestNetwork)
	after.flow.generate = func() (*BittensorLocalKey, error) {
		t.Error("the flow made a new key after the crash")
		return nil, errors.New("no")
	}
	after.flow.SetConsent(false, "", 0)
	card := after.requireState(BittensorLocalKeyStateSaveLaterNotLinked)
	if !card.OffersFinishSetup || !card.BackupReminder || card.OffersSetUp || card.OffersSaveFirst {
		t.Fatalf("after a crash the card is %+v", card)
	}
	after.flow.finishSetup(false)
	card = after.requireState(BittensorLocalKeyStateSaveLaterLinked)
	if card.Address != before.key.Address {
		t.Fatalf("linked %s", card.Address)
	}
}

// What is stored must still be the key before it signs: a phrase that does not open the stored address
// or is not a phrase at all never signs.
func TestBittensorLocalKeyFlowFinishSetupRefusesAStoredKeyThatDoesNotAgree(t *testing.T) {
	other := testingLocalKeyFromSeed(t, 2)
	for _, row := range []struct {
		name string
		item func(run *testingLocalKeyRun) testingLocalKeyItem
	}{
		{"another key's phrase", func(run *testingLocalKeyRun) testingLocalKeyItem {
			return testingLocalKeyItem{other.Mnemonic, run.key.Address}
		}},
		{"not a phrase", func(run *testingLocalKeyRun) testingLocalKeyItem {
			return testingLocalKeyItem{"garbled", run.key.Address}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			run := newTestingLocalKeyRunWithNoConsent(t)
			run.store.items[bittensorLocalKeyFlowTestNetwork] = row.item(run)
			run.flow.Reload()
			run.requireState(BittensorLocalKeyStateSaveLaterNotLinked)
			run.flow.finishSetup(false)
			if run.log.has("sign") || run.log.has("api.challenge") {
				t.Fatalf("signed: %v", run.log.all())
			}
			if card := run.flow.GetCard(); card.FailureCode != BittensorLocalKeyFailureStore {
				t.Fatalf("the card is %+v", card)
			}
		})
	}

	run := newTestingLocalKeyRunWithNoConsent(t)
	run.store.items[bittensorLocalKeyFlowTestNetwork] = testingLocalKeyItem{run.key.Mnemonic, run.key.Address}
	run.flow.Reload()
	run.store.unavailable = true
	run.flow.finishSetup(false)
	if run.log.has("sign") || run.flow.GetCard().FailureCode != BittensorLocalKeyFailureStoreUnavailable {
		t.Fatalf("%v: %+v", run.log.all(), run.flow.GetCard())
	}
}

// A refused consent on Save now keeps the phrase in memory for Retry, with no second proof and the
// same key, and still nothing on disk.
func TestBittensorLocalKeyFlowSaveNowConsentFailedRetriesWithTheSameKey(t *testing.T) {
	run := newTestingLocalKeyRunWithNoConsent(t)
	run.api.challengeErr = errors.New("server_error: 502")
	run.flow.SaveFirst(false)
	run.flow.ConfirmDelete()
	first, second := run.rightWords()
	run.flow.submitWords(first, second, true)

	card := run.requireState(BittensorLocalKeyStateSaveNowConsentFailed)
	screen := run.flow.GetScreen()
	if !screen.OffersRetry || screen.Done || screen.FailureCode != BittensorLocalKeyFailureConsent || card.Address != run.key.Address {
		t.Fatalf("the screen is %+v", screen)
	}
	// it can still be read, and the check is not asked again
	run.flow.Reveal(false)
	if run.flow.PhraseToCopy() != run.key.Mnemonic {
		t.Fatal("the phrase left memory after a failed consent")
	}
	run.flow.ConfirmDelete()
	if run.flow.GetScreen().Confirming != BittensorLocalKeyConfirmNone {
		t.Fatal("Retry asks for the words again")
	}

	run.api.challengeErr = nil
	run.flow.retrySaveNow()
	run.requireState(BittensorLocalKeyStateSavedAndDeleted)
	if len(run.api.submissions) != 1 || run.api.submissions[0].coldkeySs58 != run.key.Address || len(run.store.items) != 0 {
		t.Fatalf("submitted %+v", run.api.submissions)
	}
}

// Leaving Save now discards the key, at any point before the consent succeeds: no earnings went to it,
// and nothing about it stays.
func TestBittensorLocalKeyFlowLeavingSaveNowDiscardsTheKey(t *testing.T) {
	for _, row := range []struct {
		name string
		from func(run *testingLocalKeyRun)
	}{
		{"showing", func(run *testingLocalKeyRun) {}},
		{"revealed", func(run *testingLocalKeyRun) { run.flow.Reveal(false) }},
		{"in the check", func(run *testingLocalKeyRun) { run.flow.ConfirmDelete() }},
		{"after a failed consent", func(run *testingLocalKeyRun) {
			run.api.challengeErr = errors.New("server_error: 502")
			run.flow.ConfirmDelete()
			first, second := run.rightWords()
			run.flow.submitWords(first, second, true)
			run.requireState(BittensorLocalKeyStateSaveNowConsentFailed)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			run := newTestingLocalKeyRunWithNoConsent(t)
			run.flow.SaveFirst(false)
			row.from(run)
			run.flow.CloseScreen()

			card := run.requireState(BittensorLocalKeyStateNone)
			if card.Address != "" || card.StoredAddress != "" || !card.OffersSetUp {
				t.Fatalf("after leaving the card is %+v", card)
			}
			if len(run.store.items) != 0 || len(run.store.notes) != 0 || len(run.api.submissions) != 0 {
				t.Fatalf("leaving left %v, %v, %v", run.store.items, run.store.notes, run.api.submissions)
			}
			screen := run.flow.GetScreen()
			if screen.Open || screen.SaveNow || screen.Confirming != "" {
				t.Fatalf("the screen is %+v", screen)
			}
			if run.flow.RevealedWords().Len() != 0 || run.flow.PhraseToCopy() != "" {
				t.Fatal("the phrase stayed in memory")
			}
		})
	}
}

// MARK: the Key screen

// Hidden by default and no Copy while hidden, on both flows. A stored key needs device authentication;
// a key that signed nothing yet does not.
func TestBittensorLocalKeyFlowCopyIsImpossibleWhileHidden(t *testing.T) {
	saveNow := newTestingLocalKeyRunWithNoConsent(t)
	saveNow.flow.SaveFirst(false)
	stored := newTestingLocalKeyRunLinked(t)
	stored.flow.OpenScreen()
	if screen := stored.flow.GetScreen(); !screen.Open || screen.SaveNow || screen.Revealed || !screen.NeedsAuthentication {
		t.Fatalf("the stored key's screen opens as %+v", screen)
	}

	for name, run := range map[string]*testingLocalKeyRun{"save now": saveNow, "stored": stored} {
		t.Run(name, func(t *testing.T) {
			requireHidden := func(when string) {
				t.Helper()
				if run.flow.PhraseToCopy() != "" || run.flow.RevealedWords().Len() != 0 || run.flow.GetScreen().Revealed {
					t.Fatalf("%s: the phrase can be copied or read", when)
				}
			}
			requireHidden("at first")

			run.flow.Reveal(true)
			if run.flow.PhraseToCopy() != run.key.Mnemonic || run.flow.RevealedWords().Len() != 12 || !run.flow.GetScreen().Revealed {
				t.Fatal("revealed, there is no Copy or no words")
			}

			run.flow.Hide()
			requireHidden("after Hide")

			run.flow.Reveal(true)
			run.flow.SetCaptureActive(true)
			requireHidden("when capture starts")
			run.flow.Reveal(true)
			requireHidden("while capturing")
			run.flow.SetCaptureActive(false)
			requireHidden("after capture: it does not come back by itself")

			run.flow.Reveal(true)
			run.flow.ConfirmDelete()
			requireHidden("in the two-word check")
			run.flow.CancelConfirmation()

			run.flow.CloseScreen()
			requireHidden("after leaving")
		})
	}

	// a stored key without device authentication stays hidden, and reading it never starts
	run := newTestingLocalKeyRunLinked(t)
	run.flow.OpenScreen()
	events := len(run.log.all())
	run.flow.Reveal(false)
	if run.flow.PhraseToCopy() != "" || slices.Contains(run.log.all()[events:], "store.phrase") {
		t.Fatal("a stored key showed, or was read, with no device authentication")
	}
	// the check starts without it, since it shows nothing, and it never gives the words
	run.flow.ConfirmDelete()
	if run.flow.GetScreen().Confirming != BittensorLocalKeyConfirmDelete || run.flow.PhraseToCopy() != "" || run.flow.RevealedWords().Len() != 0 {
		t.Fatalf("the check is %+v", run.flow.GetScreen())
	}
}

// Keep and Delete hide the words before anything else, so none can be read behind the two-word check or
// the device's authentication at its button, and they stay hidden whatever comes of it. A confirmation
// that isn't offered hides them too.
func TestBittensorLocalKeyFlowWordsHideBeforeTheConfirmation(t *testing.T) {
	requireHidden := func(t *testing.T, run *testingLocalKeyRun, when string) {
		t.Helper()
		if screen := run.flow.GetScreen(); screen.Revealed || run.flow.RevealedWords().Len() != 0 || run.flow.PhraseToCopy() != "" || run.flow.revealedPhrase != "" {
			t.Fatalf("%s: the words still show: %+v", when, screen)
		}
	}
	reveal := func(t *testing.T, run *testingLocalKeyRun, authenticated bool) {
		t.Helper()
		run.flow.Reveal(authenticated)
		if !run.flow.GetScreen().Revealed {
			t.Fatal("CONTROL FAILED: the words don't show")
		}
	}
	confirm := func(run *testingLocalKeyRun, purpose string) {
		if purpose == BittensorLocalKeyConfirmDelete {
			run.flow.ConfirmDelete()
		} else {
			run.flow.ConfirmKeep()
		}
	}

	stored := newTestingLocalKeyRunLinked(t)
	stored.flow.OpenScreen()
	saveNow := newTestingLocalKeyRunWithNoConsent(t)
	saveNow.flow.SaveFirst(false)
	for name, run := range map[string]*testingLocalKeyRun{"stored": stored, "save now": saveNow} {
		for _, purpose := range []string{BittensorLocalKeyConfirmDelete, BittensorLocalKeyConfirmKeep} {
			reveal(t, run, name == "stored")
			confirm(run, purpose)
			requireHidden(t, run, name+", "+purpose)
			if got := run.flow.GetScreen().Confirming; got != purpose {
				t.Fatalf("%s: confirming %q, want %q", name, got, purpose)
			}
			if name == "stored" && purpose == BittensorLocalKeyConfirmDelete {
				// refused for want of the device's authentication: still nothing shows
				first, second := run.rightWords()
				run.flow.submitWords(first, second, false)
				requireHidden(t, run, "a delete with no authentication")
			}
			run.flow.CancelConfirmation()
			requireHidden(t, run, name+", back from "+purpose)
		}
	}

	// a saved key is offered no Keep: asking for it anyway hides the words, and says so
	kept := newTestingLocalKeyRunLinked(t)
	kept.flow.OpenScreen()
	kept.flow.ConfirmKeep()
	first, second := kept.rightWords()
	kept.flow.submitWords(first, second, false)
	kept.flow.OpenScreen()
	if kept.flow.GetScreen().OffersKeep {
		t.Fatal("CONTROL FAILED: a saved key is offered Keep")
	}
	var published atomic.Int64
	sub := kept.flow.AddBittensorLocalKeyFlowListener(testingLocalKeyListener(func() { published.Add(1) }))
	defer sub.Close()
	reveal(t, kept, true)
	before := published.Load()
	kept.flow.ConfirmKeep()
	requireHidden(t, kept, "a Keep not offered")
	if published.Load() == before || kept.flow.GetScreen().Confirming != BittensorLocalKeyConfirmNone {
		t.Fatalf("a Keep not offered: published %d, %+v", published.Load()-before, kept.flow.GetScreen())
	}
}

// The words hide after 60 s, a stale timer hides nothing, and the phrase of a stored key leaves memory
// when they do.
func TestBittensorLocalKeyFlowHidesAfterSixtySeconds(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	run.flow.OpenScreen()
	run.flow.Reveal(true)
	if len(run.timers.delays) != 1 || run.timers.delays[0] != 60*time.Second {
		t.Fatalf("the timers are %v", run.timers.delays)
	}
	run.flow.Hide()
	if !run.timers.stopped[0] {
		t.Fatal("Hide left the timer running")
	}
	run.flow.Reveal(true)
	// the first reveal's timer, if it had already started firing
	run.timers.fire(0)
	if !run.flow.GetScreen().Revealed {
		t.Fatal("an earlier reveal's timer hid the words")
	}
	run.timers.fire(1)
	if run.flow.GetScreen().Revealed || run.flow.PhraseToCopy() != "" || run.flow.revealedPhrase != "" {
		t.Fatal("the words stayed after 60 s, or the phrase stayed in memory")
	}
}

// A store that cannot answer shows no words and says so.
func TestBittensorLocalKeyFlowRevealOfAnUnreadableStore(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	run.flow.OpenScreen()
	run.store.unavailable = true
	run.flow.Reveal(true)
	screen := run.flow.GetScreen()
	if screen.Revealed || screen.FailureCode != BittensorLocalKeyFailureStoreUnavailable {
		t.Fatalf("the screen is %+v", screen)
	}
	run.flow.ConfirmDelete()
	screen = run.flow.GetScreen()
	if screen.Confirming != BittensorLocalKeyConfirmNone || screen.FailureCode != BittensorLocalKeyFailureStoreUnavailable {
		t.Fatalf("the screen is %+v", screen)
	}
}

// Delete: device authentication, then the two words, then the item goes and a read shows it gone. The
// address stays visible, from the consent.
func TestBittensorLocalKeyFlowDeleteNeedsTheWordsAndIsVerified(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	run.flow.OpenScreen()
	run.flow.ConfirmDelete()
	screen := run.flow.GetScreen()
	if screen.Confirming != BittensorLocalKeyConfirmDelete || screen.FirstWordPosition != 3 || screen.SecondWordPosition != 9 || !screen.WarnsTop200 {
		t.Fatalf("the check is %+v", screen)
	}
	run.flow.submitWords("wrong", "words", true)
	if run.log.has("store.delete") || !run.flow.GetScreen().WordsRejected {
		t.Fatal("wrong words deleted the key")
	}
	first, second := run.rightWords()
	run.flow.submitWords(first, second, true)

	if len(run.store.items) != 0 {
		t.Fatal("the key is still in the store")
	}
	run.log.requireOrder(t, "store.delete")
	card := run.requireState(BittensorLocalKeyStateSavedAndDeleted)
	if card.Address != run.key.Address || card.BackupReminder || card.OffersKeyScreen || !card.OffersWallet {
		t.Fatalf("the card is %+v", card)
	}
	if !run.flow.GetScreen().Done {
		t.Fatalf("the screen is %+v", run.flow.GetScreen())
	}
}

// A deletion a read does not confirm is not one: the key is treated as still there.
func TestBittensorLocalKeyFlowDeleteThatDidNotHappenKeepsTheKey(t *testing.T) {
	for _, row := range []struct {
		name string
		set  func(*testingLocalKeyStore)
	}{
		{"refused", func(store *testingLocalKeyStore) { store.refuseDelete = true }},
		{"said it did", func(store *testingLocalKeyStore) { store.deleteDoesNothing = true }},
	} {
		t.Run(row.name, func(t *testing.T) {
			run := newTestingLocalKeyRunLinked(t)
			row.set(run.store)
			run.flow.OpenScreen()
			run.flow.ConfirmDelete()
			first, second := run.rightWords()
			run.flow.submitWords(first, second, true)

			card := run.requireState(BittensorLocalKeyStateSaveLaterLinked)
			if card.FailureCode != BittensorLocalKeyFailureDelete || !card.BackupReminder || !card.OffersKeyScreen {
				t.Fatalf("the card is %+v", card)
			}
			if run.flow.GetScreen().Done {
				t.Fatal("the screen says it finished")
			}
			if run.store.ReadNote(bittensorLocalKeyFlowTestNetwork, bittensorLocalKeyNoteDeletedAddress) != "" {
				t.Fatal("a key still there is noted as deleted")
			}
		})
	}
}

// Deleting a key the consent never paid goes back to None: nothing is owed to it.
func TestBittensorLocalKeyFlowDeletingAnUnlinkedKeyGoesBackToNone(t *testing.T) {
	run := newTestingLocalKeyRunWithNoConsent(t)
	run.api.challengeErr = errors.New("server_error: 502")
	run.flow.setUpPayouts(false)
	run.requireState(BittensorLocalKeyStateSaveLaterNotLinked)
	run.flow.OpenScreen()
	run.flow.ConfirmDelete()
	first, second := run.rightWords()
	run.flow.submitWords(first, second, true)
	card := run.requireState(BittensorLocalKeyStateNone)
	if !card.OffersSetUp {
		t.Fatalf("the card is %+v", card)
	}
}

// The user can prove the phrase is saved and keep the key: the reminder ends and the key stays.
func TestBittensorLocalKeyFlowKeepEndsTheReminderAndKeepsTheKey(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	if !run.flow.NeedsBackup() {
		t.Fatal("an unsaved key needs no backup")
	}
	run.flow.OpenScreen()
	if !run.flow.GetScreen().OffersKeep {
		t.Fatalf("the screen is %+v", run.flow.GetScreen())
	}
	run.flow.ConfirmKeep()
	screen := run.flow.GetScreen()
	if screen.Confirming != BittensorLocalKeyConfirmKeep || screen.ConfirmNeedsAuthentication {
		t.Fatalf("the check is %+v", screen)
	}
	run.flow.submitWords("wrong", "words", false)
	if !run.flow.GetCard().BackupReminder {
		t.Fatal("wrong words ended the reminder")
	}
	// it shows and deletes nothing, so it asks for no device authentication
	first, second := run.rightWords()
	run.flow.submitWords(first, second, false)

	card := run.requireState(BittensorLocalKeyStateSaveLaterLinked)
	if card.BackupReminder || !card.KeySaved || !card.OffersKeyScreen {
		t.Fatalf("the card is %+v", card)
	}
	if run.log.has("store.delete") || run.store.items[bittensorLocalKeyFlowTestNetwork].mnemonic != run.key.Mnemonic {
		t.Fatal("keeping the key deleted it")
	}
	if run.flow.NeedsBackup() {
		t.Fatal("a saved key still warns at logout")
	}
	run.flow.OpenScreen()
	if run.flow.GetScreen().OffersKeep {
		t.Fatal("a saved key is offered Keep again")
	}
}

// Deleting a stored key asks for the device's authentication at its button, after the words: without
// it the right words delete nothing and the check stays up. Save now and Keep never ask.
func TestBittensorLocalKeyFlowDeletingAStoredKeyNeedsAuthenticationAtItsButton(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	run.flow.OpenScreen()
	run.flow.ConfirmDelete()
	screen := run.flow.GetScreen()
	if screen.Confirming != BittensorLocalKeyConfirmDelete || !screen.ConfirmNeedsAuthentication {
		t.Fatalf("the check is %+v", screen)
	}
	first, second := run.rightWords()
	run.flow.submitWords(first, second, false)
	if run.log.has("store.delete") || len(run.store.items) != 1 || run.flow.GetScreen().Confirming != BittensorLocalKeyConfirmDelete {
		t.Fatalf("the key went with no device authentication: %v, %+v", run.log.all(), run.flow.GetScreen())
	}
	run.flow.submitWords(first, second, true)
	if len(run.store.items) != 0 {
		t.Fatal("the authenticated delete left the key")
	}
	run.requireState(BittensorLocalKeyStateSavedAndDeleted)

	saveNow := newTestingLocalKeyRunWithNoConsent(t)
	saveNow.flow.SaveFirst(false)
	for _, purpose := range []string{BittensorLocalKeyConfirmDelete, BittensorLocalKeyConfirmKeep} {
		if purpose == BittensorLocalKeyConfirmDelete {
			saveNow.flow.ConfirmDelete()
		} else {
			saveNow.flow.ConfirmKeep()
		}
		if screen := saveNow.flow.GetScreen(); screen.Confirming != purpose || screen.ConfirmNeedsAuthentication {
			t.Fatalf("Save now's %s is %+v", purpose, screen)
		}
		saveNow.flow.CancelConfirmation()
	}
}

// Keep on Save now: the two words, then the key is written to the store and read back, noted saved, and
// only then signs. The banner never shows, since the phrase is proven saved, and the key stays for
// convert and Top 200.
func TestBittensorLocalKeyFlowSaveNowKeepStoresReadsBackThenSigns(t *testing.T) {
	run := newTestingLocalKeyRunWithNoConsent(t)
	run.flow.SaveFirst(false)
	if !run.flow.GetScreen().OffersKeep {
		t.Fatalf("Save now offers no Keep: %+v", run.flow.GetScreen())
	}
	run.flow.ConfirmKeep()
	run.flow.submitWords("wrong", "words", false)
	if run.log.has("store.add") || run.log.has("sign") || !run.flow.GetScreen().WordsRejected {
		t.Fatalf("wrong words stored or signed: %v", run.log.all())
	}
	first, second := run.rightWords()
	mark := len(run.log.all())
	run.flow.submitWords(first, second, false)
	// written, then read back, and only then signed
	after := &testingLocalKeyLog{}
	for _, event := range run.log.all()[mark:] {
		after.add(event)
	}
	after.requireOrder(t, "store.add", "store.phrase", "store.address", "api.challenge", "sign", "api.submit")
	if run.store.items[bittensorLocalKeyFlowTestNetwork].mnemonic != run.key.Mnemonic {
		t.Fatal("Keep left nothing in the store")
	}
	card := run.requireState(BittensorLocalKeyStateSaveLaterLinked)
	if !card.KeySaved || card.BackupReminder || !card.OffersKeyScreen || card.FailureCode != "" || card.StoredAddress != run.key.Address {
		t.Fatalf("the card is %+v", card)
	}
	if screen := run.flow.GetScreen(); !screen.Done || screen.SaveNow {
		t.Fatalf("the screen is %+v", screen)
	}
	if run.flow.NeedsBackup() {
		t.Fatal("a key kept after its words were proven warns at logout")
	}
}

// A store that refuses Keep's write, or reads back something else, signs nothing: Save now stays as it
// was, the phrase in memory, and nothing it wrote stays.
func TestBittensorLocalKeyFlowSaveNowKeepNeverSignsWhenTheStoreFails(t *testing.T) {
	other := testingLocalKeyFromSeed(t, 2)
	for _, row := range []struct {
		name string
		set  func(*testingLocalKeyStore)
	}{
		{"the add is refused", func(store *testingLocalKeyStore) { store.refuseAdd = true }},
		{"the phrase reads back different", func(store *testingLocalKeyStore) { store.readBackMnemonic = other.Mnemonic }},
		{"the address reads back different", func(store *testingLocalKeyStore) { store.readBackAddress = other.Address }},
	} {
		t.Run(row.name, func(t *testing.T) {
			run := newTestingLocalKeyRunWithNoConsent(t)
			run.flow.SaveFirst(false)
			row.set(run.store)
			run.flow.ConfirmKeep()
			first, second := run.rightWords()
			run.flow.submitWords(first, second, false)
			for _, event := range []string{"api.challenge", "sign", "api.submit"} {
				if run.log.has(event) {
					t.Fatalf("%s happened: %v", event, run.log.all())
				}
			}
			if _, stored := run.store.items[bittensorLocalKeyFlowTestNetwork]; stored {
				t.Fatal("a key that failed its read back stayed in the store")
			}
			card := run.requireState(BittensorLocalKeyStateSaveNowShowing)
			if card.FailureCode != BittensorLocalKeyFailureStore || card.Busy {
				t.Fatalf("the card is %+v", card)
			}
			run.flow.Reveal(false)
			if run.flow.PhraseToCopy() != run.key.Mnemonic {
				t.Fatal("the phrase left memory")
			}
		})
	}
}

// Keep on Save now with a refused consent: the key is stored and saved by then, so it stays, with
// Finish setup on the card and no banner.
func TestBittensorLocalKeyFlowSaveNowKeepWithARefusedConsentLeavesFinishSetup(t *testing.T) {
	run := newTestingLocalKeyRunWithNoConsent(t)
	run.api.challengeErr = errors.New("server_error: 502")
	run.flow.SaveFirst(false)
	run.flow.ConfirmKeep()
	first, second := run.rightWords()
	run.flow.submitWords(first, second, false)
	card := run.requireState(BittensorLocalKeyStateSaveLaterNotLinked)
	if !card.OffersFinishSetup || !card.KeySaved || card.BackupReminder || card.FailureCode != BittensorLocalKeyFailureConsent {
		t.Fatalf("the card is %+v", card)
	}
	if !run.flow.GetScreen().Done {
		t.Fatalf("the screen is %+v", run.flow.GetScreen())
	}
	run.api.challengeErr = nil
	run.flow.finishSetup(false)
	if card := run.requireState(BittensorLocalKeyStateSaveLaterLinked); !card.KeySaved {
		t.Fatalf("the card is %+v", card)
	}
}

// The words as they are typed: empty, the start of some listed word, wrong, or right, and the button
// only with both right. It answers only during a check, and changes and publishes nothing.
func TestBittensorLocalKeyFlowCheckWords(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	run.flow.OpenScreen()
	if check := run.flow.CheckWords("anything", "else"); check.First != "" || check.Second != "" || check.Ready {
		t.Fatalf("with no check open: %+v", check)
	}
	run.flow.ConfirmKeep()
	first, second := run.rightWords()
	var published atomic.Int64
	sub := run.flow.AddBittensorLocalKeyFlowListener(testingLocalKeyListener(func() { published.Add(1) }))
	defer sub.Close()
	// a word of the list that is no start of a longer one, and is not the word asked for
	wrongWord := ""
	for _, word := range bip39.WordList {
		if word != first && word != second && !bittensorLocalKeyWordStartsLonger(word) {
			wrongWord = word
			break
		}
	}
	for _, row := range []struct {
		first, second string
		want          BittensorLocalKeyWordCheck
	}{
		{"", "", BittensorLocalKeyWordCheck{}},
		{first, "", BittensorLocalKeyWordCheck{First: BittensorLocalKeyWordRight}},
		{" " + strings.ToUpper(first) + " ", second, BittensorLocalKeyWordCheck{First: BittensorLocalKeyWordRight, Second: BittensorLocalKeyWordRight, Ready: true}},
		{first[:1], second[:2], BittensorLocalKeyWordCheck{First: BittensorLocalKeyWordTyping, Second: BittensorLocalKeyWordTyping}},
		{wrongWord, second, BittensorLocalKeyWordCheck{First: BittensorLocalKeyWordWrong, Second: BittensorLocalKeyWordRight}},
		{first, "qqq", BittensorLocalKeyWordCheck{First: BittensorLocalKeyWordRight, Second: BittensorLocalKeyWordWrong}},
		{second, first, BittensorLocalKeyWordCheck{First: bittensorLocalKeyWordState(second, first), Second: bittensorLocalKeyWordState(first, second)}},
	} {
		if check := run.flow.CheckWords(row.first, row.second); *check != row.want {
			t.Errorf("%q, %q: %+v, want %+v", row.first, row.second, *check, row.want)
		}
	}
	if published.Load() != 0 || run.flow.GetScreen().WordsRejected || len(run.store.items) != 1 {
		t.Fatal("checking the words changed something")
	}
	// a typing state says nothing of the word asked for: the start of any listed word reads the same,
	// whether or not the word asked for starts that way
	starts := 0
	for _, word := range bip39.WordList {
		prefix := word[:2]
		if prefix == first[:2] {
			continue
		}
		starts += 1
		if got := bittensorLocalKeyWordState(prefix, first); got != BittensorLocalKeyWordTyping {
			t.Fatalf("%q, the start of %q, reads %q while %q is asked for", prefix, word, got, first)
		}
	}
	if starts < 1000 {
		t.Fatalf("CONTROL FAILED: %d starts", starts)
	}
	run.flow.CancelConfirmation()
	if check := run.flow.CheckWords(first, second); check.Ready {
		t.Fatal("the check answered after it closed")
	}
}

// The same two words are asked of a key every time, through any number of tries and on a new flow, so
// what the check says while typing can never tell more than those two words of the phrase.
func TestBittensorLocalKeyFlowAsksTheSameTwoWordsEveryTime(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	run.flow.pickPositions = bittensorLocalKeyPickPositions
	run.flow.OpenScreen()
	run.flow.ConfirmKeep()
	first, second := run.flow.GetScreen().FirstWordPosition, run.flow.GetScreen().SecondWordPosition
	if first < 1 || second <= first || bittensorLocalKeyWordCount < second {
		t.Fatalf("asked %d and %d", first, second)
	}
	for i := range 50 {
		run.flow.CancelConfirmation()
		if i%2 == 0 {
			run.flow.ConfirmDelete()
		} else {
			run.flow.ConfirmKeep()
		}
		if screen := run.flow.GetScreen(); screen.FirstWordPosition != first || screen.SecondWordPosition != second {
			t.Fatalf("try %d asked %d and %d, the first asked %d and %d", i, screen.FirstWordPosition, screen.SecondWordPosition, first, second)
		}
	}
	again := newTestingLocalKeyRunOn(t, run.log, run.store, bittensorLocalKeyFlowTestNetwork)
	again.flow.pickPositions = bittensorLocalKeyPickPositions
	again.flow.SetConsent(true, run.key.Address, 40)
	again.flow.OpenScreen()
	again.flow.ConfirmDelete()
	if screen := again.flow.GetScreen(); screen.FirstWordPosition != first || screen.SecondWordPosition != second {
		t.Fatalf("a new flow asked %d and %d", screen.FirstWordPosition, screen.SecondWordPosition)
	}
	// the Save now phrase of the same key is asked the same two
	saveNow := newTestingLocalKeyRunWithNoConsent(t)
	saveNow.flow.pickPositions = bittensorLocalKeyPickPositions
	saveNow.flow.SaveFirst(false)
	saveNow.flow.ConfirmKeep()
	if screen := saveNow.flow.GetScreen(); screen.FirstWordPosition != first || screen.SecondWordPosition != second {
		t.Fatalf("Save now asked %d and %d", screen.FirstWordPosition, screen.SecondWordPosition)
	}
}

// While the words show, the screen knows when they hide by themselves.
func TestBittensorLocalKeyFlowRevealSaysWhenItHides(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	run.flow.OpenScreen()
	if run.flow.GetScreen().RevealedUntilMillis != 0 {
		t.Fatal("hidden words have a deadline")
	}
	run.flow.Reveal(true)
	if got := run.flow.GetScreen().RevealedUntilMillis; got != bittensorLocalKeyFlowTestNowMillis+BittensorLocalKeyRevealSeconds*1000 {
		t.Fatalf("revealed until %d", got)
	}
	run.flow.Hide()
	if run.flow.GetScreen().RevealedUntilMillis != 0 {
		t.Fatal("the deadline stayed after Hide")
	}
}

// The confirmation warns about Top 200 only until the network has registered.
func TestBittensorLocalKeyFlowDeleteWarnsAboutTop200UntilRegistered(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	if !run.flow.GetScreen().WarnsTop200 {
		t.Fatal("a network not known to be registered is not warned")
	}
	run.flow.SetTop200Registered(true)
	if run.flow.GetScreen().WarnsTop200 {
		t.Fatal("a registered network is warned")
	}
}

// MARK: edge cases

// The network already has a consent: making a key replaces the payout wallet, so nothing happens until
// the user was warned. The warning covers Finish setup as well, on a key the consent does not pay.
func TestBittensorLocalKeyFlowReplacingAConsentNeedsTheWarning(t *testing.T) {
	bob := testingLocalKeyFromSeed(t, 2).Address
	run := newTestingLocalKeyRun(t)
	run.flow.SetConsent(true, bob, 40)
	card := run.requireState(BittensorLocalKeyStateElsewhere)
	if !card.OffersSetUp || !card.OffersSaveFirst || !card.ReplacesPayoutWallet || !card.OffersWallet || card.Address != bob {
		t.Fatalf("the card is %+v", card)
	}

	run.flow.setUpPayouts(false)
	run.flow.SaveFirst(false)
	if run.log.has("generate") || run.log.has("store.add") {
		t.Fatalf("a key was made with no warning: %v", run.log.all())
	}
	if run.flow.GetCard().FailureCode != BittensorLocalKeyFailureReplacementNotConfirmed {
		t.Fatalf("the card is %+v", run.flow.GetCard())
	}

	run.flow.setUpPayouts(true)
	card = run.requireState(BittensorLocalKeyStateSaveLaterLinked)
	if card.Address != run.key.Address || card.FailureCode != "" {
		t.Fatalf("the card is %+v", card)
	}

	// the user changed to a wallet later: the key stays, since it holds earlier earnings, and pointing
	// the consent back at it is a replacement too
	run.flow.SetConsent(true, bob, 50)
	card = run.requireState(BittensorLocalKeyStateSaveLaterNotLinked)
	if !card.ReplacesPayoutWallet || !card.OffersFinishSetup || !card.OffersKeyScreen || card.Address != bob || card.StoredAddress != run.key.Address {
		t.Fatalf("the card is %+v", card)
	}
	submitted := len(run.api.submissions)
	run.flow.finishSetup(false)
	if len(run.api.submissions) != submitted || run.flow.GetCard().FailureCode != BittensorLocalKeyFailureReplacementNotConfirmed {
		t.Fatalf("Finish setup replaced a wallet with no warning: %+v", run.flow.GetCard())
	}
	run.flow.finishSetup(true)
	if card := run.requireState(BittensorLocalKeyStateSaveLaterLinked); card.Address != run.key.Address {
		t.Fatalf("the card is %+v", card)
	}
}

// A second device of the network sees the address and that the key is not here, can't show a phrase,
// and creating a key there is a replacement.
func TestBittensorLocalKeyFlowSecondDeviceSeesTheAddressOnly(t *testing.T) {
	first := newTestingLocalKeyRunLinked(t)
	// another device: its own store
	secondLog := &testingLocalKeyLog{}
	second := newTestingLocalKeyRunOn(t, secondLog, newTestingLocalKeyStore(secondLog), bittensorLocalKeyFlowTestNetwork)
	second.flow.SetConsent(true, first.key.Address, snConsentTestEpoch+1)
	card := second.requireState(BittensorLocalKeyStateElsewhere)
	if card.Address != first.key.Address || card.OffersKeyScreen || card.StoredAddress != "" || card.BackupReminder || !card.ReplacesPayoutWallet {
		t.Fatalf("the card is %+v", card)
	}
	second.flow.OpenScreen()
	if second.flow.GetScreen().Open {
		t.Fatal("the screen opened with no key")
	}
	second.flow.Reveal(true)
	if second.flow.PhraseToCopy() != "" {
		t.Fatal("a device with no key showed a phrase")
	}
}

// Logout keeps the item, keyed by network id: it comes back with that network, and another network on
// the device can't see it.
func TestBittensorLocalKeyFlowSurvivesLogoutByNetworkId(t *testing.T) {
	log := &testingLocalKeyLog{}
	store := newTestingLocalKeyStore(log)
	before := newTestingLocalKeyRunOn(t, log, store, bittensorLocalKeyFlowTestNetwork)
	before.flow.SetConsent(false, "", 0)
	before.flow.setUpPayouts(false)
	before.flow.Close()

	other := newTestingLocalKeyRunOn(t, log, store, bittensorLocalKeyFlowTestOtherNetwork)
	other.flow.SetConsent(false, "", 0)
	if card := other.requireState(BittensorLocalKeyStateNone); card.StoredAddress != "" || card.BackupReminder {
		t.Fatalf("another network sees %+v", card)
	}
	if other.flow.NeedsBackup() {
		t.Fatal("another network warns about this network's key")
	}
	other.flow.OpenScreen()
	other.flow.Reveal(true)
	if other.flow.PhraseToCopy() != "" {
		t.Fatal("another network read this network's phrase")
	}
	// the other network's own key is a separate item
	other.key = testingLocalKeyFromSeed(t, 3)
	other.flow.setUpPayouts(false)
	if store.items[bittensorLocalKeyFlowTestOtherNetwork].mnemonic != other.key.Mnemonic ||
		store.items[bittensorLocalKeyFlowTestNetwork].mnemonic != before.key.Mnemonic {
		t.Fatal("the networks' items mixed")
	}

	after := newTestingLocalKeyRunOn(t, log, store, bittensorLocalKeyFlowTestNetwork)
	after.flow.SetConsent(true, before.key.Address, snConsentTestEpoch+1)
	card := after.requireState(BittensorLocalKeyStateSaveLaterLinked)
	if card.StoredAddress != before.key.Address || !card.BackupReminder {
		t.Fatalf("after signing in again the card is %+v", card)
	}
	if !after.flow.NeedsBackup() {
		t.Fatal("the logout warning does not see an unsaved key")
	}
	after.flow.OpenScreen()
	after.flow.Reveal(true)
	if after.flow.PhraseToCopy() != before.key.Mnemonic {
		t.Fatal("the key did not come back")
	}
}

// Account deletion removes the key and everything noted about it, and says if it could not.
func TestBittensorLocalKeyFlowForgetNetwork(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	run.flow.OpenScreen()
	run.flow.ConfirmKeep()
	first, second := run.rightWords()
	run.flow.submitWords(first, second, true)
	run.flow.ReminderScheduled(bittensorLocalKeyFlowTestNowMillis + 1)
	run.flow.ReminderDelivered()
	if len(run.store.notes) == 0 {
		t.Fatal("CONTROL FAILED: nothing is noted")
	}
	if !run.flow.ForgetNetwork() {
		t.Fatal("the key could not be shown gone")
	}
	if len(run.store.items) != 0 || len(run.store.notes) != 0 {
		t.Fatalf("left %v, %v", run.store.items, run.store.notes)
	}

	for _, set := range []func(*testingLocalKeyStore){
		func(store *testingLocalKeyStore) { store.refuseDelete = true },
		func(store *testingLocalKeyStore) { store.deleteDoesNothing = true },
	} {
		run = newTestingLocalKeyRunLinked(t)
		set(run.store)
		if run.flow.ForgetNetwork() {
			t.Fatal("a key still there reads as gone")
		}
	}
}

// Only the signed-in network owner makes a key, and no guest does. Without one the wallet flow stays
// as it was for whoever had it.
func TestBittensorLocalKeyFlowOnlyTheOwnerAndNoGuestsMakeAKey(t *testing.T) {
	for _, row := range []struct {
		name         string
		credential   bool
		guest        bool
		networkId    string
		offersWallet bool
		ownerOnly    bool
	}{
		{"not the owner", false, false, bittensorLocalKeyFlowTestNetwork, false, true},
		{"a guest", true, true, bittensorLocalKeyFlowTestNetwork, true, false},
		{"a session with no network", true, false, "", true, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			log := &testingLocalKeyLog{}
			run := newTestingLocalKeyRunOn(t, log, newTestingLocalKeyStore(log), row.networkId)
			run.api.credential = row.credential
			run.flow.SetGuest(row.guest)
			run.flow.SetConsent(false, "", 0)

			card := run.requireState(BittensorLocalKeyStateNone)
			if card.OffersSetUp || card.OffersSaveFirst || card.OffersWallet != row.offersWallet || card.OwnerOnly != row.ownerOnly {
				t.Fatalf("the card is %+v", card)
			}
			run.flow.setUpPayouts(true)
			run.flow.SaveFirst(true)
			if run.log.has("generate") || run.flow.GetCard().FailureCode != BittensorLocalKeyFailureNotAllowed {
				t.Fatalf("%v, %+v", run.log.all(), run.flow.GetCard())
			}
			run.flow.finishSetup(true)
			if run.log.has("sign") {
				t.Fatal("signed")
			}

			// a key already here is shown either way, since saving its phrase takes nothing away
			if row.networkId != "" {
				run.store.items[row.networkId] = testingLocalKeyItem{run.key.Mnemonic, run.key.Address}
				run.flow.Reload()
				card = run.requireState(BittensorLocalKeyStateSaveLaterNotLinked)
				if card.OffersFinishSetup || !card.OffersKeyScreen || !card.BackupReminder {
					t.Fatalf("with a key the card is %+v", card)
				}
			}
		})
	}

	// a nil Api, as with no session: the flow is there and offers nothing that signs
	log := &testingLocalKeyLog{}
	flow := NewBittensorLocalKeyFlow(nil, bittensorLocalKeyFlowTestNetwork, newTestingLocalKeyStore(log))
	flow.SetConsent(false, "", 0)
	if card := flow.GetCard(); card.OffersSetUp || !card.OwnerOnly {
		t.Fatalf("the card is %+v", card)
	}

	// and a nil store, which reads as a store that cannot answer
	flow = NewBittensorLocalKeyFlow(nil, bittensorLocalKeyFlowTestNetwork, nil)
	flow.SetConsent(false, "", 0)
	if card := flow.GetCard(); card.State != BittensorLocalKeyStateStoreUnavailable || card.OffersSetUp {
		t.Fatalf("the card is %+v", card)
	}
	if flow.NeedsBackup() || flow.ForgetNetwork() {
		t.Fatal("no store says it holds a key, or that it forgot one")
	}
}

// An unread or unreadable consent is not "no consent": nothing is offered that could replace one. A
// stored key can still be saved.
func TestBittensorLocalKeyFlowUnknownConsentOffersNothingNew(t *testing.T) {
	run := newTestingLocalKeyRun(t)
	if card := run.requireState(BittensorLocalKeyStateLoading); card.OffersSetUp || card.OffersSaveFirst || card.OffersWallet {
		t.Fatalf("an unread consent offers %+v", card)
	}
	run.flow.SetConsentUnavailable()
	if card := run.requireState(BittensorLocalKeyStateConsentUnavailable); card.OffersSetUp || card.OffersSaveFirst || card.OffersWallet {
		t.Fatalf("an unavailable consent offers %+v", card)
	}
	run.flow.setUpPayouts(true)
	if run.log.has("generate") {
		t.Fatal("a key was made with the consent unknown")
	}

	run = newTestingLocalKeyRunLinked(t)
	run.flow.SetConsentUnavailable()
	card := run.requireState(BittensorLocalKeyStateConsentUnavailable)
	if !card.OffersKeyScreen || !card.BackupReminder || card.OffersFinishSetup {
		t.Fatalf("with a stored key the card is %+v", card)
	}
}

// One key per network: with a key here nothing makes another.
func TestBittensorLocalKeyFlowHoldsOneKeyPerNetwork(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	key := run.key
	run.key = testingLocalKeyFromSeed(t, 2)
	run.flow.setUpPayouts(true)
	run.flow.SaveFirst(true)
	if run.store.items[bittensorLocalKeyFlowTestNetwork].mnemonic != key.Mnemonic {
		t.Fatal("another key replaced the one here")
	}
	if run.flow.GetCard().FailureCode != BittensorLocalKeyFailureNotAllowed || run.flow.GetScreen().SaveNow {
		t.Fatalf("%+v", run.flow.GetCard())
	}
}

// MARK: the card

// When the card says payouts start next epoch.
func TestBittensorLocalKeyFlowPayoutsStartNextEpoch(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	// the consent's first epoch is 43
	if !run.flow.GetCard().PayoutsStartNextEpoch {
		t.Fatal("just linked with the epoch unread, it starts next epoch")
	}
	run.flow.SetCurrentEpoch(snConsentTestEpoch)
	if !run.flow.GetCard().PayoutsStartNextEpoch {
		t.Fatal("before its first epoch, it starts next epoch")
	}
	run.flow.SetCurrentEpoch(snConsentTestEpoch + 1)
	if run.flow.GetCard().PayoutsStartNextEpoch {
		t.Fatal("in its first epoch, it has started")
	}

	// a later visit with the epoch unread says nothing, since it may have started long ago
	log := &testingLocalKeyLog{}
	later := newTestingLocalKeyRunOn(t, log, run.store, bittensorLocalKeyFlowTestNetwork)
	later.flow.SetConsent(true, run.key.Address, snConsentTestEpoch+1)
	if later.flow.GetCard().PayoutsStartNextEpoch {
		t.Fatal("a later visit with the epoch unread still says it starts next epoch")
	}
}

// Every row of the Key states table, from its inputs, and what each offers the owner and anyone else.
func TestBittensorLocalKeyCardStates(t *testing.T) {
	const (
		key     = "5KeyOnThisDevice"
		other   = "5AnotherColdkey"
		network = bittensorLocalKeyFlowTestNetwork
	)
	owner := func(in bittensorLocalKeyCardInputs) bittensorLocalKeyCardInputs {
		in.networkId = network
		in.hasNetworkCredential = true
		return in
	}
	for _, row := range []struct {
		name string
		in   bittensorLocalKeyCardInputs
		want BittensorLocalKeyCard
	}{
		{"nothing read", owner(bittensorLocalKeyCardInputs{}), BittensorLocalKeyCard{State: BittensorLocalKeyStateLoading, FundsDisclaimer: BittensorFundsDisclaimerNone, ClaimFundsDisclaimer: BittensorFundsDisclaimerFundsOnly}},
		{"none", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentNone}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateNone, ShowsKeyPath: true, OffersSetUp: true, OffersSaveFirst: true, OffersWallet: true, FundsDisclaimer: BittensorFundsDisclaimerFundsOnly, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
		}},
		{"save now, showing", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentNone, saveNowAddress: key}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateSaveNowShowing, Address: key, FundsDisclaimer: BittensorFundsDisclaimerFundsOnly, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
		}},
		{"save now, consent failed", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentNone, saveNowAddress: key, saveNowFailed: true, failureCode: BittensorLocalKeyFailureConsent, failureMessage: "x"}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateSaveNowConsentFailed, Address: key, FailureCode: BittensorLocalKeyFailureConsent, FailureMessage: "x", FundsDisclaimer: BittensorFundsDisclaimerFundsOnly, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
		}},
		{"save later, not linked", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentNone, storedAddress: key}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateSaveLaterNotLinked, ShowsKeyPath: true, Address: key, StoredAddress: key, OffersFinishSetup: true, OffersKeyScreen: true, OffersWallet: true, BackupReminder: true, FundsDisclaimer: BittensorFundsDisclaimerFull, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
		}},
		{"save later, not linked, another consent", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentActive, consentAddress: other, storedAddress: key}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateSaveLaterNotLinked, ShowsKeyPath: true, Address: other, StoredAddress: key, OffersFinishSetup: true, OffersKeyScreen: true, OffersWallet: true, BackupReminder: true, ReplacesPayoutWallet: true, FundsDisclaimer: BittensorFundsDisclaimerFull, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
		}},
		{"save later, linked", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentActive, consentAddress: key, storedAddress: key}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateSaveLaterLinked, Address: key, StoredAddress: key, OffersKeyScreen: true, OffersWallet: true, BackupReminder: true, FundsDisclaimer: BittensorFundsDisclaimerFull, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
			PaysKeyOnThisDevice: true,
		}},
		{"save later, linked, saved and kept", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentActive, consentAddress: key, storedAddress: key, savedAddress: key}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateSaveLaterLinked, Address: key, StoredAddress: key, OffersKeyScreen: true, OffersWallet: true, KeySaved: true, FundsDisclaimer: BittensorFundsDisclaimerFull, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
			PaysKeyOnThisDevice: true,
		}},
		{"saved and deleted", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentActive, consentAddress: key, deletedAddress: key}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateSavedAndDeleted, Address: key, OffersWallet: true, FundsDisclaimer: BittensorFundsDisclaimerFundsOnly, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
		}},
		{"a key made here, paid to another since", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentActive, consentAddress: other, deletedAddress: key}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateElsewhere, Address: other, OffersSetUp: true, OffersSaveFirst: true, OffersWallet: true, ReplacesPayoutWallet: true, FundsDisclaimer: BittensorFundsDisclaimerFundsOnly, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
		}},
		{"another device or a wallet", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentActive, consentAddress: other}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateElsewhere, Address: other, OffersSetUp: true, OffersSaveFirst: true, OffersWallet: true, ReplacesPayoutWallet: true, FundsDisclaimer: BittensorFundsDisclaimerFundsOnly, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
		}},
		{"a guest, no consent", bittensorLocalKeyCardInputs{networkId: network, hasNetworkCredential: true, guest: true, consentRead: bittensorLocalKeyConsentNone}, BittensorLocalKeyCard{
			State: BittensorLocalKeyStateNone, OffersWallet: true, FundsDisclaimer: BittensorFundsDisclaimerNone, ClaimFundsDisclaimer: BittensorFundsDisclaimerFundsOnly,
		}},
		{"not the owner, no consent", bittensorLocalKeyCardInputs{networkId: network, consentRead: bittensorLocalKeyConsentNone}, BittensorLocalKeyCard{
			State: BittensorLocalKeyStateNone, OwnerOnly: true, FundsDisclaimer: BittensorFundsDisclaimerNone, ClaimFundsDisclaimer: BittensorFundsDisclaimerFundsOnly,
		}},
		{"not the owner, another consent", bittensorLocalKeyCardInputs{networkId: network, consentRead: bittensorLocalKeyConsentActive, consentAddress: other}, BittensorLocalKeyCard{
			State: BittensorLocalKeyStateElsewhere, Address: other, OwnerOnly: true, ReplacesPayoutWallet: true, FundsDisclaimer: BittensorFundsDisclaimerFundsOnly, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
		}},
		{"not the owner, linked here", bittensorLocalKeyCardInputs{networkId: network, consentRead: bittensorLocalKeyConsentActive, consentAddress: key, storedAddress: key}, BittensorLocalKeyCard{
			State: BittensorLocalKeyStateSaveLaterLinked, Address: key, StoredAddress: key, OwnerOnly: true, OffersKeyScreen: true, BackupReminder: true, FundsDisclaimer: BittensorFundsDisclaimerFull, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
			PaysKeyOnThisDevice: true,
		}},
		{"the store cannot answer", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentActive, consentAddress: key, storeUnavailable: true}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateStoreUnavailable, ShowsKeyPath: true, FundsDisclaimer: BittensorFundsDisclaimerFundsOnly, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
		}},
		{"no secure storage, no consent", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentNone, storeAbsent: true}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateNone, OffersWallet: true, FundsDisclaimer: BittensorFundsDisclaimerNone, ClaimFundsDisclaimer: BittensorFundsDisclaimerFundsOnly,
		}},
		{"no secure storage, another consent", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentActive, consentAddress: other, storeAbsent: true}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateElsewhere, Address: other, OffersWallet: true, ReplacesPayoutWallet: true, FundsDisclaimer: BittensorFundsDisclaimerFundsOnly, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
		}},
		{"no secure storage, a key this device deleted", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentActive, consentAddress: key, deletedAddress: key, storeAbsent: true}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateSavedAndDeleted, Address: key, OffersWallet: true, FundsDisclaimer: BittensorFundsDisclaimerFundsOnly, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
		}},
		{"the consent cannot be read", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentUnavailable}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateConsentUnavailable, FundsDisclaimer: BittensorFundsDisclaimerNone, ClaimFundsDisclaimer: BittensorFundsDisclaimerFundsOnly,
		}},
		{"the consent cannot be read, a key here", owner(bittensorLocalKeyCardInputs{consentRead: bittensorLocalKeyConsentUnavailable, storedAddress: key}), BittensorLocalKeyCard{
			State: BittensorLocalKeyStateConsentUnavailable, Address: key, StoredAddress: key, OffersKeyScreen: true, BackupReminder: true, FundsDisclaimer: BittensorFundsDisclaimerFull, ClaimFundsDisclaimer: BittensorFundsDisclaimerNone,
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := *bittensorLocalKeyCardOf(row.in); got != row.want {
				t.Errorf("\n got %+v\nwant %+v", got, row.want)
			}
		})
	}
}

// The points card asks for no coldkey while the consent pays the key on this device, which is the
// network's coldkey before the wallet read lists one: Apple's rule, the card's State is SaveLaterLinked.
// From every combination of what decides the card, and through the flow, set up and then deleted.
func TestBittensorLocalKeyCardPaysKeyOnThisDevice(t *testing.T) {
	const (
		key     = "5KeyOnThisDevice"
		deleted = "5AKeyThisDeviceDeleted"
		other   = "5AnotherColdkey"
	)
	seen := map[bool]int{}
	for _, consentRead := range []bittensorLocalKeyConsentRead{bittensorLocalKeyConsentUnread, bittensorLocalKeyConsentUnavailable, bittensorLocalKeyConsentNone, bittensorLocalKeyConsentActive} {
		for _, consentAddress := range []string{"", key, deleted, other} {
			if consentRead != bittensorLocalKeyConsentActive && consentAddress != "" {
				continue
			}
			for _, storedAddress := range []string{"", key} {
				for _, deletedAddress := range []string{"", deleted, key} {
					for _, saveNowAddress := range []string{"", other} {
						for flags := range 32 {
							in := bittensorLocalKeyCardInputs{
								networkId:            bittensorLocalKeyFlowTestNetwork,
								consentRead:          consentRead,
								consentAddress:       consentAddress,
								storedAddress:        storedAddress,
								savedAddress:         storedAddress,
								deletedAddress:       deletedAddress,
								saveNowAddress:       saveNowAddress,
								saveNowFailed:        flags&1 != 0,
								storeUnavailable:     flags&2 != 0,
								hasNetworkCredential: flags&4 != 0,
								guest:                flags&8 != 0,
							}
							if flags&16 != 0 {
								in.networkId = ""
							}
							card := bittensorLocalKeyCardOf(in)
							if card.PaysKeyOnThisDevice != (card.State == BittensorLocalKeyStateSaveLaterLinked) {
								t.Fatalf("pays a key on this device %t in state %s: %+v from %+v", card.PaysKeyOnThisDevice, card.State, card, in)
							}
							seen[card.PaysKeyOnThisDevice] += 1
						}
					}
				}
			}
		}
	}
	if seen[true] == 0 || seen[false] == 0 {
		t.Fatalf("CONTROL FAILED: %v", seen)
	}

	run := newTestingLocalKeyRunWithNoConsent(t)
	if card := run.requireState(BittensorLocalKeyStateNone); card.PaysKeyOnThisDevice {
		t.Fatalf("before setup the card is %+v", card)
	}
	run.flow.setUpPayouts(false)
	if card := run.requireState(BittensorLocalKeyStateSaveLaterLinked); !card.PaysKeyOnThisDevice {
		t.Fatalf("linked, the card is %+v", card)
	}
	run.flow.OpenScreen()
	run.flow.ConfirmDelete()
	first, second := run.rightWords()
	run.flow.submitWords(first, second, true)
	if card := run.requireState(BittensorLocalKeyStateSavedAndDeleted); card.PaysKeyOnThisDevice {
		t.Fatalf("deleted, the card is %+v", card)
	}
}

// MARK: the funds disclaimer

// Every state of the card, from every combination of what decides it: wherever the card shows any part of
// the easy flows it carries the disclaimer, the whole text exactly when a key is in the app's secure
// storage, and the claim card, which shares Earnings with it, carries the last sentence exactly when the
// payout card carries none, so Earnings shows it once.
func TestBittensorLocalKeyFlowEveryEasyFlowCardCarriesTheFundsDisclaimer(t *testing.T) {
	const (
		key     = "5KeyOnThisDevice"
		deleted = "5AKeyThisDeviceDeleted"
		other   = "5AnotherColdkey"
	)
	easyFlowStates := map[string]bool{
		BittensorLocalKeyStateSaveNowShowing:       true,
		BittensorLocalKeyStateSaveNowConsentFailed: true,
		BittensorLocalKeyStateSaveLaterNotLinked:   true,
		BittensorLocalKeyStateSaveLaterLinked:      true,
		BittensorLocalKeyStateSavedAndDeleted:      true,
		BittensorLocalKeyStateElsewhere:            true,
		BittensorLocalKeyStateStoreUnavailable:     true,
	}
	checked := 0
	seen := map[string]bool{}
	for _, consentRead := range []bittensorLocalKeyConsentRead{bittensorLocalKeyConsentUnread, bittensorLocalKeyConsentUnavailable, bittensorLocalKeyConsentNone, bittensorLocalKeyConsentActive} {
		for _, consentAddress := range []string{"", key, deleted, other} {
			for _, storedAddress := range []string{"", key} {
				for _, savedAddress := range []string{"", key} {
					for _, deletedAddress := range []string{"", deleted, key} {
						for _, saveNowAddress := range []string{"", other} {
							for flags := 0; flags < 64; flags++ {
								if consentRead != bittensorLocalKeyConsentActive && consentAddress != "" {
									continue
								}
								// with no secure storage the store reads no key, and it is not one that can't answer
								storeAbsent := flags&32 != 0
								if storeAbsent && (storedAddress != "" || flags&2 != 0) {
									continue
								}
								in := bittensorLocalKeyCardInputs{
									networkId:            bittensorLocalKeyFlowTestNetwork,
									consentRead:          consentRead,
									consentAddress:       consentAddress,
									storedAddress:        storedAddress,
									savedAddress:         savedAddress,
									deletedAddress:       deletedAddress,
									saveNowAddress:       saveNowAddress,
									saveNowFailed:        flags&1 != 0,
									storeUnavailable:     flags&2 != 0,
									hasNetworkCredential: flags&4 != 0,
									guest:                flags&8 != 0,
									storeAbsent:          storeAbsent,
								}
								if flags&16 != 0 {
									in.networkId = ""
								}
								card := bittensorLocalKeyCardOf(in)
								// no secure storage: nothing offers a key, and with no consent the card is the
								// wallet card
								if storeAbsent && saveNowAddress == "" {
									if card.OffersSetUp || card.OffersSaveFirst || card.OffersFinishSetup || card.ShowsKeyPath {
										t.Fatalf("no secure storage offers a key: %+v from %+v", card, in)
									}
									if card.State == BittensorLocalKeyStateNone && card.FundsDisclaimer != BittensorFundsDisclaimerNone {
										t.Fatalf("no secure storage shows %q on the wallet card: %+v from %+v", card.FundsDisclaimer, card, in)
									}
								}
								checked += 1
								seen[card.State+"/"+card.FundsDisclaimer] = true
								showsEasyFlow := easyFlowStates[card.State] || card.ShowsKeyPath || card.OffersSetUp || card.OffersSaveFirst ||
									card.OffersFinishSetup || card.OffersKeyScreen || card.StoredAddress != ""
								// a store that can't answer can't say where a key is, and a phrase being saved first is
								// in memory only
								keyReadable := card.StoredAddress != "" && card.State != BittensorLocalKeyStateStoreUnavailable &&
									card.State != BittensorLocalKeyStateSaveNowShowing && card.State != BittensorLocalKeyStateSaveNowConsentFailed
								wantFull := keyReadable
								switch {
								case !showsEasyFlow && card.FundsDisclaimer != BittensorFundsDisclaimerNone:
									t.Fatalf("the wallet card carries %q: %+v from %+v", card.FundsDisclaimer, card, in)
								case showsEasyFlow && wantFull && card.FundsDisclaimer != BittensorFundsDisclaimerFull:
									t.Fatalf("a key in the app's storage shows %q: %+v from %+v", card.FundsDisclaimer, card, in)
								case showsEasyFlow && !wantFull && card.FundsDisclaimer != BittensorFundsDisclaimerFundsOnly:
									t.Fatalf("a key not stored here shows %q: %+v from %+v", card.FundsDisclaimer, card, in)
								}
								// once on Earnings: on the payout card when it has one, else on the claim card
								claimWant := BittensorFundsDisclaimerFundsOnly
								if card.FundsDisclaimer != BittensorFundsDisclaimerNone {
									claimWant = BittensorFundsDisclaimerNone
								}
								if card.ClaimFundsDisclaimer != claimWant {
									t.Fatalf("the claim card shows %q: %+v from %+v", card.ClaimFundsDisclaimer, card, in)
								}
							}
						}
					}
				}
			}
		}
	}
	// every state the card can be in came up, with each kind of disclaimer
	for _, want := range []string{
		BittensorLocalKeyStateLoading + "/" + BittensorFundsDisclaimerNone,
		BittensorLocalKeyStateLoading + "/" + BittensorFundsDisclaimerFull,
		BittensorLocalKeyStateConsentUnavailable + "/" + BittensorFundsDisclaimerFull,
		BittensorLocalKeyStateNone + "/" + BittensorFundsDisclaimerFundsOnly,
		BittensorLocalKeyStateNone + "/" + BittensorFundsDisclaimerNone,
		BittensorLocalKeyStateSaveNowShowing + "/" + BittensorFundsDisclaimerFundsOnly,
		BittensorLocalKeyStateSaveNowConsentFailed + "/" + BittensorFundsDisclaimerFundsOnly,
		BittensorLocalKeyStateSaveLaterNotLinked + "/" + BittensorFundsDisclaimerFull,
		BittensorLocalKeyStateSaveLaterLinked + "/" + BittensorFundsDisclaimerFull,
		BittensorLocalKeyStateSavedAndDeleted + "/" + BittensorFundsDisclaimerFundsOnly,
		BittensorLocalKeyStateElsewhere + "/" + BittensorFundsDisclaimerFundsOnly,
		BittensorLocalKeyStateStoreUnavailable + "/" + BittensorFundsDisclaimerFundsOnly,
	} {
		if !seen[want] {
			t.Errorf("CONTROL FAILED: no combination gave %s", want)
		}
	}
	if checked < 1000 {
		t.Fatalf("CONTROL FAILED: %d combinations", checked)
	}
}

// The Key screen and both confirmations carry the disclaimer at every step of both flows: the last
// sentence while the phrase is in memory only, the whole text for a key in the app's secure storage.
func TestBittensorLocalKeyFlowEveryKeyScreenCarriesTheFundsDisclaimer(t *testing.T) {
	requireScreen := func(t *testing.T, run *testingLocalKeyRun, want string, when string) {
		t.Helper()
		screen := run.flow.GetScreen()
		if !screen.Open || screen.FundsDisclaimer != want {
			t.Fatalf("%s: the screen is %+v, want %q", when, screen, want)
		}
		place := BittensorFundsDisclaimerPlaceKeyScreen
		switch screen.Confirming {
		case BittensorLocalKeyConfirmDelete:
			place = BittensorFundsDisclaimerPlaceDeleteConfirmation
		case BittensorLocalKeyConfirmKeep:
			place = BittensorFundsDisclaimerPlaceKeepConfirmation
		}
		if got := run.flow.FundsDisclaimerAt(place); got != want {
			t.Fatalf("%s: %s says %q, want %q", when, place, got, want)
		}
	}

	t.Run("save now", func(t *testing.T) {
		run := newTestingLocalKeyRunWithNoConsent(t)
		if screen := run.flow.GetScreen(); screen.FundsDisclaimer != BittensorFundsDisclaimerNone {
			t.Fatalf("a closed screen carries %q", screen.FundsDisclaimer)
		}
		run.api.challengeErr = errors.New("server_error: 502")
		run.flow.SaveFirst(false)
		requireScreen(t, run, BittensorFundsDisclaimerFundsOnly, "showing")
		run.flow.ConfirmKeep()
		requireScreen(t, run, BittensorFundsDisclaimerFundsOnly, "confirming the keep")
		run.flow.CancelConfirmation()
		run.flow.Reveal(false)
		requireScreen(t, run, BittensorFundsDisclaimerFundsOnly, "revealed")
		run.flow.ConfirmDelete()
		requireScreen(t, run, BittensorFundsDisclaimerFundsOnly, "confirming")
		run.flow.submitWords("wrong", "words", true)
		requireScreen(t, run, BittensorFundsDisclaimerFundsOnly, "words rejected")
		first, second := run.rightWords()
		run.flow.submitWords(first, second, true)
		requireScreen(t, run, BittensorFundsDisclaimerFundsOnly, "consent failed")
		if got := run.flow.FundsDisclaimerAt(BittensorFundsDisclaimerPlaceKeepConfirmation); got != BittensorFundsDisclaimerFundsOnly {
			t.Fatalf("Save now's keep confirmation says %q", got)
		}
		run.flow.CloseScreen()
		if screen := run.flow.GetScreen(); screen.FundsDisclaimer != BittensorFundsDisclaimerNone {
			t.Fatalf("a closed screen carries %q", screen.FundsDisclaimer)
		}
	})

	t.Run("a stored key", func(t *testing.T) {
		run := newTestingLocalKeyRunLinked(t)
		run.flow.OpenScreen()
		requireScreen(t, run, BittensorFundsDisclaimerFull, "hidden")
		run.flow.Reveal(true)
		requireScreen(t, run, BittensorFundsDisclaimerFull, "revealed")
		run.flow.ConfirmDelete()
		requireScreen(t, run, BittensorFundsDisclaimerFull, "confirming the delete")
		run.flow.submitWords("wrong", "words", true)
		requireScreen(t, run, BittensorFundsDisclaimerFull, "words rejected")
		run.flow.CancelConfirmation()
		run.flow.ConfirmKeep()
		requireScreen(t, run, BittensorFundsDisclaimerFull, "confirming the keep")
		run.flow.CancelConfirmation()
		first, second := run.rightWords()
		run.flow.ConfirmDelete()
		run.flow.submitWords(first, second, true)
		// the key is gone: the screen is done, and with no key there is nothing to show
		if screen := run.flow.GetScreen(); !screen.Done || screen.FundsDisclaimer != BittensorFundsDisclaimerNone {
			t.Fatalf("after the delete the screen is %+v", screen)
		}
		if card := run.flow.GetCard(); card.FundsDisclaimer != BittensorFundsDisclaimerFundsOnly {
			t.Fatalf("after the delete the card is %+v", card)
		}
	})
}

// Every place of the easy flows has its disclaimer, the convert and Top 200 seams included, and an unknown
// place has none. The claim card shares Earnings with the payout card, so it has one only while the payout
// card has none. The places a snapshot covers say what the snapshot says.
func TestBittensorLocalKeyFlowFundsDisclaimerAtEveryPlace(t *testing.T) {
	places := []string{
		BittensorFundsDisclaimerPlacePayoutCard,
		BittensorFundsDisclaimerPlaceKeyScreen,
		BittensorFundsDisclaimerPlaceDeleteConfirmation,
		BittensorFundsDisclaimerPlaceKeepConfirmation,
		BittensorFundsDisclaimerPlaceClaimCard,
		BittensorFundsDisclaimerPlaceConvert,
		BittensorFundsDisclaimerPlaceTop200,
		BittensorFundsDisclaimerPlaceChangeWallet,
		BittensorFundsDisclaimerPlaceClaimSheet,
	}

	linked := newTestingLocalKeyRunLinked(t)
	for _, place := range places {
		want := BittensorFundsDisclaimerFull
		switch place {
		case BittensorFundsDisclaimerPlaceClaimCard, BittensorFundsDisclaimerPlaceChangeWallet:
			// Earnings shows it on the payout card; Change wallet offers no key while this one is paid
			want = BittensorFundsDisclaimerNone
		case BittensorFundsDisclaimerPlaceClaimSheet:
			want = BittensorFundsDisclaimerFundsOnly
		}
		if got := linked.flow.FundsDisclaimerAt(place); got != want {
			t.Errorf("a linked key in the app's storage: %s says %q, want %q", place, got, want)
		}
	}
	if got := linked.flow.FundsDisclaimerAt("somewhere_else"); got != BittensorFundsDisclaimerNone {
		t.Errorf("an unknown place says %q", got)
	}
	card := linked.flow.GetCard()
	if card.FundsDisclaimer != linked.flow.FundsDisclaimerAt(BittensorFundsDisclaimerPlacePayoutCard) ||
		card.ClaimFundsDisclaimer != linked.flow.FundsDisclaimerAt(BittensorFundsDisclaimerPlaceClaimCard) {
		t.Errorf("the card %+v and FundsDisclaimerAt disagree", card)
	}

	// a network paid to an outside wallet, with no key here: only the last sentence, wherever it shows
	wallet := newTestingLocalKeyRun(t)
	wallet.flow.SetConsent(true, testingLocalKeyFromSeed(t, 2).Address, 40)
	for place, want := range map[string]string{
		BittensorFundsDisclaimerPlacePayoutCard:         BittensorFundsDisclaimerFundsOnly,
		BittensorFundsDisclaimerPlaceClaimCard:          BittensorFundsDisclaimerNone,
		BittensorFundsDisclaimerPlaceConvert:            BittensorFundsDisclaimerFundsOnly,
		BittensorFundsDisclaimerPlaceTop200:             BittensorFundsDisclaimerFundsOnly,
		BittensorFundsDisclaimerPlaceKeyScreen:          BittensorFundsDisclaimerNone,
		BittensorFundsDisclaimerPlaceDeleteConfirmation: BittensorFundsDisclaimerNone,
		BittensorFundsDisclaimerPlaceKeepConfirmation:   BittensorFundsDisclaimerNone,
		// Change wallet offers a key here, which there isn't yet
		BittensorFundsDisclaimerPlaceChangeWallet: BittensorFundsDisclaimerFundsOnly,
		BittensorFundsDisclaimerPlaceClaimSheet:   BittensorFundsDisclaimerFundsOnly,
	} {
		if got := wallet.flow.FundsDisclaimerAt(place); got != want {
			t.Errorf("a wallet network: %s says %q, want %q", place, got, want)
		}
	}

	// a key here that the consent no longer pays: the card and the key's screens say where it is
	moved := newTestingLocalKeyRunLinked(t)
	moved.flow.SetConsent(true, testingLocalKeyFromSeed(t, 2).Address, 50)
	for place, want := range map[string]string{
		BittensorFundsDisclaimerPlacePayoutCard: BittensorFundsDisclaimerFull,
		BittensorFundsDisclaimerPlaceKeyScreen:  BittensorFundsDisclaimerFull,
		BittensorFundsDisclaimerPlaceClaimCard:  BittensorFundsDisclaimerNone,
		BittensorFundsDisclaimerPlaceConvert:    BittensorFundsDisclaimerFull,
	} {
		if got := moved.flow.FundsDisclaimerAt(place); got != want {
			t.Errorf("a key paid no more: %s says %q, want %q", place, got, want)
		}
	}

	// a store that can't answer can't say where a key is
	locked := newTestingLocalKeyRunLinked(t)
	locked.store.unavailable = true
	locked.flow.Reload()
	for place, want := range map[string]string{
		BittensorFundsDisclaimerPlacePayoutCard: BittensorFundsDisclaimerFundsOnly,
		BittensorFundsDisclaimerPlaceClaimCard:  BittensorFundsDisclaimerNone,
		BittensorFundsDisclaimerPlaceConvert:    BittensorFundsDisclaimerFundsOnly,
		BittensorFundsDisclaimerPlaceTop200:     BittensorFundsDisclaimerFundsOnly,
	} {
		if got := locked.flow.FundsDisclaimerAt(place); got != want {
			t.Errorf("a locked store: %s says %q, want %q", place, got, want)
		}
	}

	// before setup there is no key yet, and a session that can't make one sees the wallet card: the claim
	// card then carries the last sentence, once
	none := newTestingLocalKeyRunWithNoConsent(t)
	if got := none.flow.FundsDisclaimerAt(BittensorFundsDisclaimerPlacePayoutCard); got != BittensorFundsDisclaimerFundsOnly {
		t.Errorf("before setup the payout card says %q", got)
	}
	guest := newTestingLocalKeyRunWithNoConsent(t)
	guest.flow.SetGuest(true)
	if got := guest.flow.FundsDisclaimerAt(BittensorFundsDisclaimerPlacePayoutCard); got != BittensorFundsDisclaimerNone {
		t.Errorf("the wallet card says %q", got)
	}
	if got := guest.flow.FundsDisclaimerAt(BittensorFundsDisclaimerPlaceClaimCard); got != BittensorFundsDisclaimerFundsOnly {
		t.Errorf("beside the wallet card the claim card says %q", got)
	}
}

// The values are the ids of the localizations keys the apps look up, so they never change without the store.
func TestBittensorFundsDisclaimerIdsAreTheStoreKeys(t *testing.T) {
	if BittensorFundsDisclaimerFull != "bittensor_funds_disclaimer" || BittensorFundsDisclaimerFundsOnly != "bittensor_funds_disclaimer_funds_only" || BittensorFundsDisclaimerNone != "" {
		t.Fatal("the disclaimer ids no longer name the localizations keys")
	}
}

// MARK: the reminder

// One notification, when the first epoch becomes claimable, while the key is unsaved. No repeats, and
// an app without permission reports nothing, so it isn't used up.
func TestBittensorLocalKeyFlowReminder(t *testing.T) {
	const (
		currentEpoch    = int64(42)
		fromEpoch       = int64(43)
		epochBlocks     = int64(50_400)
		epochMillis     = epochBlocks * snBlockSeconds * 1000
		claimOpenMillis = bittensorLocalKeyFlowTestNowMillis + 1000
		firstClaimOpen  = claimOpenMillis + (fromEpoch-currentEpoch)*epochMillis
	)
	action := func(run *testingLocalKeyRun, claimable bool) *BittensorLocalKeyReminder {
		return run.flow.Reminder(currentEpoch, claimOpenMillis, epochBlocks, claimable)
	}
	require := func(t *testing.T, got *BittensorLocalKeyReminder, want string, atMillis int64) {
		t.Helper()
		if got.Action != want || got.AtMillis != atMillis {
			t.Fatalf("%+v, want %s at %d", got, want, atMillis)
		}
	}

	t.Run("scheduled once for the first epoch's claim opening", func(t *testing.T) {
		run := newTestingLocalKeyRunLinked(t)
		require(t, action(run, false), BittensorLocalKeyReminderSchedule, firstClaimOpen)
		// with no permission it asks again next time
		require(t, action(run, false), BittensorLocalKeyReminderSchedule, firstClaimOpen)
		run.flow.ReminderScheduled(firstClaimOpen)
		require(t, action(run, false), BittensorLocalKeyReminderNone, 0)
		// past its time the system delivered it, and nothing repeats
		run.clock.nowMillis = firstClaimOpen
		require(t, action(run, true), BittensorLocalKeyReminderNone, 0)
		require(t, action(run, false), BittensorLocalKeyReminderNone, 0)
	})

	t.Run("a schedule that moved replaces the one posted", func(t *testing.T) {
		run := newTestingLocalKeyRunLinked(t)
		run.flow.ReminderScheduled(firstClaimOpen - 1000)
		require(t, action(run, false), BittensorLocalKeyReminderSchedule, firstClaimOpen)
	})

	t.Run("something claimable early delivers now, once", func(t *testing.T) {
		run := newTestingLocalKeyRunLinked(t)
		run.flow.ReminderScheduled(firstClaimOpen)
		require(t, action(run, true), BittensorLocalKeyReminderDeliverNow, 0)
		run.flow.ReminderDelivered()
		require(t, action(run, true), BittensorLocalKeyReminderNone, 0)
	})

	t.Run("an opening already past delivers now", func(t *testing.T) {
		run := newTestingLocalKeyRunLinked(t)
		run.clock.nowMillis = firstClaimOpen + 1
		require(t, action(run, false), BittensorLocalKeyReminderDeliverNow, 0)
	})

	t.Run("nothing while the schedule is unknown", func(t *testing.T) {
		run := newTestingLocalKeyRunLinked(t)
		require(t, run.flow.Reminder(0, 0, 0, false), BittensorLocalKeyReminderNone, 0)
		require(t, run.flow.Reminder(currentEpoch, 0, epochBlocks, false), BittensorLocalKeyReminderNone, 0)
		require(t, run.flow.Reminder(currentEpoch, claimOpenMillis, 0, false), BittensorLocalKeyReminderNone, 0)
	})

	t.Run("a saved key takes the reminder back", func(t *testing.T) {
		run := newTestingLocalKeyRunLinked(t)
		run.flow.ReminderScheduled(firstClaimOpen)
		run.flow.OpenScreen()
		run.flow.ConfirmKeep()
		first, second := run.rightWords()
		run.flow.submitWords(first, second, true)
		require(t, action(run, false), BittensorLocalKeyReminderCancel, 0)
		require(t, action(run, false), BittensorLocalKeyReminderNone, 0)
	})

	t.Run("a deleted key takes the reminder back", func(t *testing.T) {
		run := newTestingLocalKeyRunLinked(t)
		run.flow.ReminderScheduled(firstClaimOpen)
		run.flow.OpenScreen()
		run.flow.ConfirmDelete()
		first, second := run.rightWords()
		run.flow.submitWords(first, second, true)
		require(t, action(run, false), BittensorLocalKeyReminderCancel, 0)
	})

	t.Run("nothing for a key not paid yet, or none", func(t *testing.T) {
		run := newTestingLocalKeyRunWithNoConsent(t)
		require(t, action(run, true), BittensorLocalKeyReminderNone, 0)
		run.api.challengeErr = errors.New("server_error: 502")
		run.flow.setUpPayouts(false)
		require(t, action(run, true), BittensorLocalKeyReminderNone, 0)
	})

	t.Run("a store that can't answer leaves a posted reminder alone", func(t *testing.T) {
		run := newTestingLocalKeyRunLinked(t)
		run.flow.ReminderScheduled(firstClaimOpen)
		run.store.unavailable = true
		run.flow.Reload()
		if card := run.flow.GetCard(); card.State != BittensorLocalKeyStateStoreUnavailable || card.BackupReminder {
			t.Fatalf("CONTROL FAILED: the card is %+v", card)
		}
		require(t, action(run, false), BittensorLocalKeyReminderNone, 0)
		if run.store.ReadNote(bittensorLocalKeyFlowTestNetwork, bittensorLocalKeyNoteReminderAtMillis) == "" {
			t.Fatal("a locked store took the reminder back")
		}
		// unlocked again, the key is still unsaved and the reminder is still the one posted
		run.store.unavailable = false
		run.flow.Reload()
		require(t, action(run, false), BittensorLocalKeyReminderNone, 0)
	})
}

// MARK: where the phrase goes

// A full run of both flows, with every failure a phrase could leak through, shows no word of it in
// anything that can reach a log, a crash report, a preference or a C caller's JSON: the card, the
// screen, the notes, the store's address reads, the failures, or a flow printed by accident.
func TestBittensorLocalKeyFlowNeverShowsThePhrase(t *testing.T) {
	seen := []string{}
	see := func(text string) {
		seen = append(seen, text)
	}
	exercise := func(run *testingLocalKeyRun) {
		record := func() {
			card, err := json.Marshal(run.flow.GetCard())
			if err != nil {
				t.Fatal(err)
			}
			screen, err := json.Marshal(run.flow.GetScreen())
			if err != nil {
				t.Fatal(err)
			}
			see(string(card))
			see(string(screen))
			see(fmt.Sprintf("%v %+v %#v %s", run.flow, run.flow, run.flow, run.flow))
			see(fmt.Sprintf("%v %+v", run.flow.GetCard(), run.flow.GetScreen()))
			reminder := run.flow.Reminder(snConsentTestEpoch, bittensorLocalKeyFlowTestNowMillis+1, 50_400, true)
			see(fmt.Sprintf("%+v", reminder))
			for name, note := range run.store.notes {
				see(name + "=" + note)
			}
		}
		sub := run.flow.AddBittensorLocalKeyFlowListener(testingLocalKeyListener(record))
		defer sub.Close()
		record()

		run.flow.SetConsent(false, "", 0)
		run.api.challengeErr = errors.New("server_error: 502")
		run.flow.SaveFirst(false)
		run.flow.Reveal(false)
		run.flow.Hide()
		run.flow.ConfirmDelete()
		run.flow.submitWords("wrong", "words", true)
		first, second := run.rightWords()
		run.flow.submitWords(first, second, true)
		run.api.challengeErr = nil
		run.api.submitErr = errors.New("server_error: 500")
		run.flow.retrySaveNow()
		run.api.submitErr = nil
		run.flow.CloseScreen()

		run.api.challengeErr = errors.New("server_error: 502")
		run.flow.setUpPayouts(false)
		run.api.challengeErr = nil
		run.flow.finishSetup(false)
		run.flow.OpenScreen()
		run.flow.Reveal(true)
		run.flow.Hide()
		run.flow.ConfirmKeep()
		run.flow.CancelConfirmation()
		run.flow.ConfirmDelete()
		run.flow.submitWords(first, second, true)
		record()
	}
	run := newTestingLocalKeyRun(t)
	exercise(run)

	// the words may be plain English, so the check is for the phrase's own pieces in order
	words := strings.Split(run.key.Mnemonic, " ")
	pieces := []string{run.key.Mnemonic, strings.Join(words, ""), strings.Join(words[:2], " "), strings.Join(words[len(words)-2:], " ")}
	if len(seen) < 10 {
		t.Fatalf("CONTROL FAILED: saw %d texts", len(seen))
	}
	for _, text := range seen {
		for _, piece := range pieces {
			if strings.Contains(text, piece) {
				t.Errorf("a phrase piece reached %q", text)
			}
		}
	}
	// the fixed entropy makes a phrase of one repeated word pattern; a key of varied words holds the
	// rule for any single word as well
	varied := newTestingLocalKeyRun(t)
	varied.key = &BittensorLocalKey{
		Mnemonic: "hotel forest jar hover kite book view eight stuff angle legend defense",
		Address:  "5Hpm9fq3W3dQgwWpAwDS2ZHKAdnk86QRCu7iX4GnmDxycrte",
	}
	seen = nil
	exercise(varied)
	for _, text := range seen {
		testingRequireNoPhraseText(t, "a flow text", text, varied.key.Mnemonic)
	}
}

type testingLocalKeyListener func()

func (self testingLocalKeyListener) BittensorLocalKeyFlowChanged() {
	self()
}

// The flow's source holds no way to log: nothing it holds can reach one by accident.
func TestBittensorLocalKeyFlowHasNoLogging(t *testing.T) {
	source, err := os.ReadFile("bittensor_local_key_flow.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"glog.", "log.", "fmt.Print", "println(", "os.Stdout", "os.Stderr", "LocalState", "localState"} {
		for line := range strings.Lines(string(source)) {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, forbidden) {
				t.Errorf("%q: %s", forbidden, line)
			}
		}
	}
	// the build constraint that keeps the flow out of the tunnel extension and the browser
	if !strings.HasPrefix(string(source), "//go:build !ios_extension && !js\n") {
		t.Error("the flow lost its build constraint")
	}
}

// The calls an app makes return at once, and the listener hears the result. Everything else here is on
// the same steps run in line, which is what keeps it deterministic.
func TestBittensorLocalKeyFlowPublicCallsReportThroughTheListener(t *testing.T) {
	run := newTestingLocalKeyRunWithNoConsent(t)
	changed := make(chan struct{}, 64)
	sub := run.flow.AddBittensorLocalKeyFlowListener(testingLocalKeyListener(func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}))
	defer sub.Close()
	waitFor := func(done func(*BittensorLocalKeyCard, *BittensorLocalKeyScreen) bool) {
		t.Helper()
		deadline := time.After(30 * time.Second)
		for {
			if done(run.flow.GetCard(), run.flow.GetScreen()) {
				return
			}
			select {
			case <-changed:
			case <-deadline:
				t.Fatalf("no change came: %+v %+v", run.flow.GetCard(), run.flow.GetScreen())
			}
		}
	}

	run.flow.SetUpPayouts(false)
	waitFor(func(card *BittensorLocalKeyCard, _ *BittensorLocalKeyScreen) bool {
		return card.State == BittensorLocalKeyStateSaveLaterLinked && !card.Busy
	})

	run.flow.OpenScreen()
	run.flow.ConfirmKeep()
	first, second := run.rightWords()
	run.flow.SubmitWords(first, second, true)
	waitFor(func(card *BittensorLocalKeyCard, screen *BittensorLocalKeyScreen) bool {
		return card.KeySaved && screen.Done
	})

	// Finish setup and Retry, through their own calls
	retry := newTestingLocalKeyRunWithNoConsent(t)
	retry.api.challengeErr = errors.New("server_error: 502")
	retry.flow.setUpPayouts(false)
	retry.api.challengeErr = nil
	retryChanged := make(chan struct{}, 64)
	retrySub := retry.flow.AddBittensorLocalKeyFlowListener(testingLocalKeyListener(func() {
		select {
		case retryChanged <- struct{}{}:
		default:
		}
	}))
	defer retrySub.Close()
	retry.flow.FinishSetup(false)
	deadline := time.After(30 * time.Second)
	for retry.flow.GetCard().State != BittensorLocalKeyStateSaveLaterLinked {
		select {
		case <-retryChanged:
		case <-deadline:
			t.Fatalf("Finish setup never finished: %+v", retry.flow.GetCard())
		}
	}
}

// A panic in the work ends the busy state: nothing waits on it forever, and the key stays.
func TestBittensorLocalKeyFlowPanicEndsTheBusyState(t *testing.T) {
	waitIdle := func(run *testingLocalKeyRun) *BittensorLocalKeyCard {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			card := run.flow.GetCard()
			if card.FailureCode != "" && !card.Busy {
				return card
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("still busy: %+v", run.flow.GetCard())
		return nil
	}

	run := newTestingLocalKeyRunWithNoConsent(t)
	run.flow.sign = func(string, string) (string, error) {
		panic("broken")
	}
	run.flow.SetUpPayouts(false)
	card := waitIdle(run)
	if card.State != BittensorLocalKeyStateSaveLaterNotLinked || card.FailureCode != BittensorLocalKeyFailureConsent {
		t.Fatalf("the card is %+v", card)
	}
	if run.store.items[bittensorLocalKeyFlowTestNetwork].mnemonic != run.key.Mnemonic {
		t.Fatal("the key is gone")
	}

	run = newTestingLocalKeyRunWithNoConsent(t)
	run.flow.sign = func(string, string) (string, error) {
		panic("broken")
	}
	run.flow.SaveFirst(false)
	run.flow.ConfirmDelete()
	first, second := run.rightWords()
	run.flow.SubmitWords(first, second, true)
	card = waitIdle(run)
	if card.State != BittensorLocalKeyStateSaveNowConsentFailed || !run.flow.GetScreen().OffersRetry {
		t.Fatalf("the card is %+v", card)
	}
}

// The whole of Set up payouts through the real Api against a fake operator: the requests a network
// owner's session makes, in order, and a signature the server's check accepts for the key the store
// holds. A second flow on the same store finds the key linked.
func TestBittensorLocalKeyFlowThroughTheRealApi(t *testing.T) {
	server := newSnConsentServer(t)
	_, api, networkJwt := newSnConsentOwnerApi(t, server)
	log := &testingLocalKeyLog{}
	store := newTestingLocalKeyStore(log)
	flow := NewBittensorLocalKeyFlow(api, bittensorLocalKeyFlowTestNetwork, store)
	flow.SetConsent(false, "", 0)
	if card := flow.GetCard(); !card.OffersSetUp || card.OwnerOnly {
		t.Fatalf("the owner's card is %+v", card)
	}

	flow.setUpPayouts(false)

	card := flow.GetCard()
	stored := store.items[bittensorLocalKeyFlowTestNetwork]
	if card.State != BittensorLocalKeyStateSaveLaterLinked || card.Address != stored.address || card.FailureCode != "" {
		t.Fatalf("the card is %+v", card)
	}
	requests := server.takeRequests()
	routes := []string{}
	for _, request := range requests {
		routes = append(routes, request.route)
	}
	if !slices.Equal(routes, []string{"GET /sn/epoch", "GET /sn/wallet", "POST /sn/wallet/network-consent", "POST /sn/wallet"}) {
		t.Fatalf("requests %v", routes)
	}
	submitted := requests[len(requests)-1]
	if submitted.byJwt != networkJwt {
		t.Fatal("the consent did not go with the network credential")
	}
	args := &SnSetWalletArgs{}
	if err := json.Unmarshal([]byte(submitted.body), args); err != nil {
		t.Fatal(err)
	}
	valid, err := testingBittensorServerVerify(args.ColdkeySs58, args.Message, args.Signature)
	if err != nil || !valid || args.ColdkeySs58 != stored.address {
		t.Fatalf("the server's check refused the consent: %v, %v, %s", valid, err, args.ColdkeySs58)
	}
	// the key is new each run, and its words are English: the check is for pieces of the phrase, which
	// the request's own words never make by chance
	for _, request := range requests {
		testingRequireNoPhrasePiece(t, request.route, request.route+" "+request.body, stored.mnemonic)
	}
	if testingPhrasePieceIn(submitted.body+" "+stored.mnemonic, stored.mnemonic) == "" {
		t.Fatal("CONTROL FAILED: the check misses this phrase in this request")
	}

	again := NewBittensorLocalKeyFlow(api, bittensorLocalKeyFlowTestNetwork, store)
	again.SetConsent(true, stored.address, snConsentTestEpoch+1)
	if card := again.GetCard(); card.State != BittensorLocalKeyStateSaveLaterLinked || !card.BackupReminder {
		t.Fatalf("the next flow's card is %+v", card)
	}
}

// A server that refuses is a failed consent through the real Api too, with the code an app has words for.
func TestBittensorLocalKeyFlowThroughTheRealApiRefused(t *testing.T) {
	for _, row := range []struct {
		name string
		set  func(*snConsentServer)
		code string
	}{
		{"the owner only", func(server *snConsentServer) {
			server.challenge = func(*SnNetworkWalletMappingChallengeArgs) (int, string) {
				return http.StatusForbidden, "network owner only"
			}
		}, SnErrorCodeNetworkOwnerOnly},
		{"expired", func(server *snConsentServer) {
			server.submit = answer(http.StatusBadRequest, snNetworkConsentExpiredRefusal)
		}, SnErrorCodeChallengeExpired},
		{"signature mismatch", func(server *snConsentServer) {
			server.submit = answer(http.StatusOK, `{"error":{"code":"signature_mismatch","message":"no match"}}`)
		}, SnErrorCodeSignatureMismatch},
	} {
		t.Run(row.name, func(t *testing.T) {
			server := newSnConsentServer(t)
			row.set(server)
			_, api, _ := newSnConsentOwnerApi(t, server)
			log := &testingLocalKeyLog{}
			store := newTestingLocalKeyStore(log)
			flow := NewBittensorLocalKeyFlow(api, bittensorLocalKeyFlowTestNetwork, store)
			flow.SetConsent(false, "", 0)
			flow.setUpPayouts(false)
			card := flow.GetCard()
			if card.State != BittensorLocalKeyStateSaveLaterNotLinked || card.FailureCode != row.code {
				t.Fatalf("the card is %+v", card)
			}
			if store.items[bittensorLocalKeyFlowTestNetwork].mnemonic == "" {
				t.Fatal("a refused consent lost the key")
			}
		})
	}
}

// The real timer: a reveal schedules it, and Close lets the phrase go at once.
func TestBittensorLocalKeyFlowCloseLetsThePhraseGo(t *testing.T) {
	run := newTestingLocalKeyRunLinked(t)
	run.flow.OpenScreen()
	run.flow.Reveal(true)
	run.flow.Close()
	if run.flow.PhraseToCopy() != "" || run.flow.revealedPhrase != "" || run.flow.ctx.Err() == nil {
		t.Fatal("Close kept the phrase, or the flow's work")
	}
}

// Two different words, in order, for any phrase length: the same two for an address every time, and
// over many addresses every position gets asked.
func TestBittensorLocalKeyPickPositions(t *testing.T) {
	asked := map[int]bool{}
	pairs := map[[2]int]bool{}
	for i := range 2000 {
		address := testingLocalKeyFromSeed(t, byte(i%250)).Address + strconv.Itoa(i)
		first, second := bittensorLocalKeyPickPositions(address, bittensorLocalKeyWordCount)
		if first < 0 || bittensorLocalKeyWordCount <= second || second <= first {
			t.Fatalf("%d, %d", first, second)
		}
		if againFirst, againSecond := bittensorLocalKeyPickPositions(address, bittensorLocalKeyWordCount); againFirst != first || againSecond != second {
			t.Fatalf("%s: %d, %d then %d, %d", address, first, second, againFirst, againSecond)
		}
		asked[first], asked[second] = true, true
		pairs[[2]int{first, second}] = true
	}
	if len(asked) != bittensorLocalKeyWordCount || len(pairs) < 60 {
		t.Fatalf("over 2000 addresses %d positions and %d pairs were asked", len(asked), len(pairs))
	}
	if first, second := bittensorLocalKeyPickPositions("5Any", 1); first != 0 || second != 0 {
		t.Fatalf("%d, %d", first, second)
	}
	if first, second := bittensorLocalKeyPickPositions("5Any", 2); first != 0 || second != 1 {
		t.Fatalf("%d, %d", first, second)
	}
}
