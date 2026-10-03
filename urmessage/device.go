package urmessage

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sdk"
)

// THE EPOCH ZERO EXPORTER, RESTATED HERE BECAUSE connect DOES NOT PUBLISH IT.
//
// group_handle_key is GroupHandleKey(StorageRoot(mls_secret[0], pq_secret)), and mls_secret[n] is
// the group handle's exporter output under this label at this length -- messagegroup/session.go
// line 858, where both are unexported constants (mlsSecretLabel, mlsSecretBytes). A caller outside
// that package that has to derive the key at epoch zero, and hand it to a device that will
// construct its session at epoch one, has no way to ask for the values and must restate them.
// messagegroup/enginejoin_test.go restates them too, at engineJoinExporterLabel.
//
// THIS IS THEREFORE A SECOND SITE OF ONE CONSTANT BY CONSTRUCTION OF THE VISIBILITY RULES, and it
// is filed rather than absorbed: the exact change connect owes is to export these two, for example
// as messagegroup.MlsSecretLabel and messagegroup.MlsSecretBytes, after which this pair is
// deleted. Until then, a label changed in connect gives every device founded by this package a
// group_handle_key its own session does not derive -- which shows up as the joiner computing a
// different sender_handle and every record being refused as an untracked sender, not as a build
// failure.
const (
	storageExporterLabel = "URmessage/v1/storage"
	storageExporterBytes = 32
)

// The MLS cipher suite this build founds and joins groups on.
const deviceCipherSuite = mls.CipherSuiteX25519ChaCha20Sha256Ed25519

// DeviceConfig is one device's collaborators. Everything whose zero value would be a silent hole
// is refused by [NewDevice].
type DeviceConfig struct {
	// The 10.1 binding this device speaks over. REQUIRED, and the caller's: this package never
	// dials, never authenticates and never closes the connect client under it. S2-7, and the
	// package document says what the alpha's answer does not cover.
	Transport *sdk.MessageTransport

	// The durable allocator of 5.6's stream indices. REQUIRED and NOT defaulted to an in-memory
	// one: a reservation that does not survive a restart re-issues an index under an unmoved
	// class key, which is a reused nonce under a reused record key.
	// sdk.NewStreamIndexReserver over sdk.OpenStreamStore is the one this module ships.
	Reserver messagegroup.StreamIndexReserver

	// Where MLS keeps its group state and private keys. [NewMemoryStateStore] when nil, which
	// persists nothing -- see its document.
	StateStore mls.StateStore

	// The clock every record's sent_at and expire_at is read from. time.Now().UnixMilli when nil.
	NowMs func() int64

	// Where this device's signature keys, X-Wing seed and pq_secret are drawn from.
	// crypto/rand.Reader when nil.
	Random io.Reader

	// How [Device.Connect] rides out a reconnect window. The zero value is the default policy
	// and is what every caller that says nothing gets; see [ConnectPolicy].
	Connect ConnectPolicy

	// An ADDITIONAL receiving-client decision on an ingested commit, run AFTER the role model's
	// own rules ([authorizeCommit], MASTER §11, ledger item 242's R1) over the same
	// [CommitAuthorization], and able only to refuse more. The rules run on every commit whether
	// or not this is set; nil means "nothing beyond §11" and not "allow every commit", which is
	// what it meant before R1 landed. See [CommitAuthorizer].
	CommitAuthorizer CommitAuthorizer
}

// ── §4.3.1's Hello, across an operator window this package does not own ──────────────────────

// THE WINDOW, MEASURED ON THE DEPLOYED SERVER RATHER THAN INFERRED. One client, one client_id,
// nothing else running:
//
//	baseline, a fresh connection                            Hello OK in 27ms
//	close the client, then re-dial and Hello repeatedly:
//	  +12s FAILED  +24s FAILED  +37s FAILED  +49s FAILED  +1m1s FAILED  +1m1s OK in 32ms
//
// Attempts 5 and 6 fall in the SAME SECOND, so it is a hard edge at about 60 seconds and not a
// gradual recovery; reproduced three times with different gaps. From the client the new connection
// attaches, routes register, the Hello goes out and NOTHING COMES BACK -- no error, no refusal,
// silence until the transport's own deadline. Filed against the operator in msgrepo
// `docs/reports/2026-09-15-operator-and-connect-findings.md` item 5, and it is THEIRS: the
// candidate causes are two 60-second settings in the operator's own resident, and whether the fix
// is to invalidate the old route when a new connection for the same client_id registers is not this
// package's call.
//
// WHAT IS OURS IS THAT WE USED TO CALL IT A FAILURE. [Device.Connect] sent one Hello and returned a
// hard error, and EVERY REAL CLIENT RECONNECTS -- app resume, laptop lid, network flap, a restart --
// so every one of those was an error where the truth was "not yet".
//
// THIS DOES NOT REMOVE THE SIXTY SECONDS AND NOTHING HERE CAN. The user still waits them and the
// operator item stays open. What it does is stop calling them a failure, and stop making the caller
// invent the retry loop.
type ConnectPolicy struct {
	// Budget is the total elapsed time [Device.Connect] will keep trying for. Zero takes
	// [defaultConnectBudget].
	//
	// IT BOUNDS HOW LONG ONE CALL BLOCKS AND IT IS NOT A CLAIM THAT THE WINDOW IS OVER. That
	// distinction is the whole reason [ErrReconnecting] exists: when the budget is spent and
	// every attempt looked like silence, the answer is "not yet, ask again" rather than
	// "failed", so a budget that runs out before the operator's window closes costs a caller one
	// more call and never a false verdict.
	Budget time.Duration

	// AttemptTimeout is how long ONE Hello is given before it is abandoned and the next attempt
	// is scheduled. Zero takes [defaultConnectAttempt]. An attempt is never given more than what
	// is left of [ConnectPolicy.Budget], so a Budget shorter than this is one attempt of the
	// Budget's length and not one of this.
	//
	// IT IS SHORTER THAN THE TRANSPORT'S OWN 30s DEADLINE ON PURPOSE. Inside the window the
	// server answers nothing at all, so waiting the transport's full deadline spends the budget
	// on silence and buys three attempts where it could buy seven. A Hello is idempotent by
	// construction -- the server replaces its nonce at every one -- and a late answer to an
	// abandoned attempt is dropped rather than adopted, because the nonce is taken in
	// `messageTransport.Hello` AFTER its Call returns and an abandoned Call has already
	// forgotten its waiter.
	AttemptTimeout time.Duration

	// FirstBackoff and MaxBackoff are the pause between attempts, doubling from the first up to
	// the maximum. Zero takes [defaultConnectFirstBackoff] and [defaultConnectMaxBackoff].
	//
	// THERE IS NO JITTER, AND THAT IS A DECISION RATHER THAN AN OMISSION. Jitter buys spread
	// over a CONTENDED resource; this window is per client_id -- it is the operator's own route
	// state for one client and no other client's reconnect makes it longer or shorter -- so
	// there is nothing here to spread, and a deterministic schedule is one a case can assert
	// exactly. The day this rides out something shared, jitter is the change.
	FirstBackoff time.Duration
	MaxBackoff   time.Duration

	// OnAttempt, when set, is called after every Hello that did not connect, before the pause.
	// It is how a caller says "Reconnecting..." to a user DURING the window rather than after
	// it, which a blocking call cannot otherwise do. It must not call back into this device.
	OnAttempt func(ConnectAttempt)
}

// ConnectAttempt is one Hello that did not connect, as [ConnectPolicy.OnAttempt] sees it.
type ConnectAttempt struct {
	// Attempt counts from 1.
	Attempt int
	// Elapsed is how long [Device.Connect] has been trying.
	Elapsed time.Duration
	// Backoff is how long it is about to wait before the next attempt. Zero when there will not
	// be one, AND zero when the next attempt is the last and follows at once because a full pause
	// would have left the budget nothing to try with (see [Device.Connect]).
	Backoff time.Duration
	// Err is why this attempt did not connect.
	Err error
}

const (
	// defaultConnectBudget covers the measured ~60s window with margin.
	//
	// NINETY SECONDS, AND THE NUMBER IS ARGUED RATHER THAN ROUND. The measured edge is at about
	// 61s from the close of the previous connection. The two candidate causes named in the
	// operator report are both 60-second settings, so a mechanism that starts its 60s at some
	// point AFTER the disconnect rather than at it puts the worst case somewhat past 61s; 90s is
	// half as long again as anything measured. A bound UNDER the window would be a bound that
	// does not cover the case it exists for, which is why this is not 30s.
	defaultConnectBudget = 90 * time.Second

	// defaultConnectAttempt is one Hello's deadline. Ten seconds is generous for a round trip
	// that measures 27ms on a good connection and short enough that the budget buys attempts
	// rather than silence.
	defaultConnectAttempt = 10 * time.Second

	defaultConnectFirstBackoff = 1 * time.Second
	defaultConnectMaxBackoff   = 8 * time.Second
)

func (self ConnectPolicy) withDefaults() ConnectPolicy {
	if self.Budget <= 0 {
		self.Budget = defaultConnectBudget
	}
	if self.AttemptTimeout <= 0 {
		self.AttemptTimeout = defaultConnectAttempt
	}
	if self.FirstBackoff <= 0 {
		self.FirstBackoff = defaultConnectFirstBackoff
	}
	if self.MaxBackoff <= 0 {
		self.MaxBackoff = defaultConnectMaxBackoff
	}
	if self.MaxBackoff < self.FirstBackoff {
		self.MaxBackoff = self.FirstBackoff
	}
	return self
}

// Device is one device: its MLS engine and identity, the transport it speaks over, and the groups
// it is a member of.
//
// It is safe for concurrent use. Every method that touches a group takes that group's lock, and
// the MLS session under it is serialized through its own goroutine by connect/messagegroup.
type Device struct {
	transport *sdk.MessageTransport
	reserver  messagegroup.StreamIndexReserver
	crypto    mls.CryptoProvider
	engine    messagegroup.GroupEngine
	leafKeys  []byte

	// The store the engine was built over, HELD BESIDE the engine for [DeviceStore]: every
	// durable-only path in this package -- [Device.Restore], [Device.persistGroup],
	// [Device.persistSent], [Device.persistPeerHeads] -- asks this value whether it is a
	// [DeviceStore] and does nothing when it is not. The query, so the claim is checkable:
	// `grep -n "self.stateStore" urmessage/*.go` answers those four type assertions and nothing
	// else.
	//
	// IT USED TO BE HELD FOR A SECOND REASON AND THAT REASON IS GONE. Until LoadGroup landed,
	// `restoreOne` called `mls.LoadGroup` DIRECTLY -- because `messagegroup.GroupEngine` had four
	// methods and none of them opened a persisted group -- so this field was also the *mls.Store
	// half of a GroupConfig this package assembled itself, and a `signer mls.SignaturePrivateKey`
	// field stood beside it to be the other argument that call took. J1-8 is CLOSED: the engine
	// opens the group, the signer it signs with is the engine's own copy, and the field that
	// existed only to feed that call has been deleted rather than left as state nothing reads.
	stateStore mls.StateStore

	// The credential identity this device founds and joins under: its signer's public half. See
	// [NewDevice] for why it is that value and not another.
	identityPub []byte

	// wrapSeed is the PRIVATE half of the X-Wing key pair whose public half is inside
	// [Device.leafKeys], as the 32 octet seed [messagegroup.XwingKeyGenFromSeed] expands. It is
	// what lets this device open an encapsulation addressed to the leaf it publishes -- S2-26,
	// the first step of ledger item 243. EMPTY is a real and supported value: a store written
	// before the seed was retained holds none, and [Device.DecapsulateToOwnLeaf] refuses by name
	// rather than guessing. See [DurableStateStore.GetDeviceIdentity].
	//
	// IT IS THE SEED AND NOT A *messagegroup.XwingPrivateKey, and that is a decision with two
	// reasons rather than a preference. The first is erasure: this package's one erase is
	// [zeroizeState] over octets, `XwingPrivateKey` declares no erase of its own, and a field
	// holding key material with no way to clear it is exactly what [Device.Close] must not leave
	// behind. The second is that `connect/mls`'s erase gate excuses `XwingPrivateKey` from owing
	// an erase on the written ground that *"no production declaration holds one in a field"* --
	// a sentence that gate cannot check outside `connect`, and that holding the seed keeps true.
	//
	// IT IS GUARDED BY [Device.mutex] AND `identityPub` AND `leafKeys` BESIDE IT ARE NOT, because
	// unlike them it is WRITTEN after construction: [Device.Close] erases it IN PLACE, over the
	// same backing array a decapsulation reads. An unguarded read beside that write is a data
	// race in the literal `-race` sense and a half-erased seed in the practical one, so both
	// sides take the mutex and this is the only field of the three that has to.
	//
	// IT IS ALSO HELD RATHER THAN COPIED, where `identityPub` beside it is copied, and that is
	// the erase talking again. Both of `deviceIdentity`'s arms hand back an array nothing else
	// references -- `XwingPrivateKey.Seed` answers a copy, and the store re-reads its file on
	// every call -- so holding it leaves ONE array for Close to clear, while copying would leave
	// the original behind with nothing pointing at it.
	//
	// AND [Device.Close] IS NOT THE ONLY ERASE THE ARRAY OWES, which is a repair and not a
	// restatement. The seed is live BEFORE there is a Device to close -- `deviceIdentity` mints it
	// two fallible statements before it hands it back, and [NewDevice] holds it across the engine's
	// construction -- and on every refusal in between it used to go out of scope with its octets
	// intact, with nothing left that could ever clear it. Each of those three live ranges now
	// carries a deferred erase disarmed at its one exit that hands the seed on;
	// TestEveryPathThatDropsTheDeviceErasesItsWrapSeed holds the property over the source rather
	// than over a list of the exits that exist today.
	wrapSeed []byte

	nowMs  func() int64
	random io.Reader

	// connect is [DeviceConfig.Connect] with its defaults filled in ONCE, at construction. It
	// is read without a lock and never written after, which is what lets [Device.Connect] be
	// called concurrently without the policy being a second thing to synchronise.
	connect ConnectPolicy

	// commitAuthorizer is [DeviceConfig.CommitAuthorizer], read on the commit-ingest path after
	// the role model's own rules. Nil adds no rule of its own. Read without a lock and never
	// written after construction, for `connect`'s reason one field up.
	commitAuthorizer CommitAuthorizer

	mutex  sync.Mutex
	groups map[string]*Group

	// §4.3.5's pushes and the replaced-session flag: see [pushInbox], which is what the transport's
	// callbacks hold instead of this device.
	inbox *pushInbox
}

// pushInbox is the part of a device the transport's callbacks are bound to, and it holds NONE of
// the device's keys: registering with the transport must hand it no reference to the wrap seed
// (TestEveryWrapSeedInThisPackageGoesWhereTheDispositionSaysItGoes). It runs on the client's
// receive goroutine, so it only queues.
type pushInbox struct {
	// one group id per push, for [Device.WaitPush]. Buffered and never blocking: a push that finds it
	// full is dropped, because a full buffer already says "fetch", and the fetch the waiter does next
	// reads everything a dropped push would have named. An EMPTY id is the replaced-session wake.
	pushes chan []byte
	// set when the client reports a NEW session to the server, which holds no connection until
	// somebody says Hello on it; the next operation that needs the nonce does ([Device.helloIfReplaced])
	helloNeeded atomic.Bool

	unsubscribePush     func()
	unsubscribeReplaced func()
}

func newPushInbox(transport *sdk.MessageTransport) *pushInbox {
	inbox := &pushInbox{pushes: make(chan []byte, 64)}
	inbox.unsubscribePush = transport.OnPush(inbox.pushed)
	inbox.unsubscribeReplaced = transport.OnSessionReplaced(inbox.sessionReplaced)
	return inbox
}

func (self *pushInbox) pushed(push *protocol.MessageServerPush) {
	records := push.GetRecords()
	if records == nil || len(records.GetGroupId()) == 0 {
		return
	}
	self.wake(append([]byte(nil), records.GetGroupId()...))
}

// sessionReplaced marks the Hello as owed and wakes [Device.WaitPush] with an empty id, so the
// caller's next Receive or EnsureSubscribed says it at once rather than at the next poll.
func (self *pushInbox) sessionReplaced() {
	self.helloNeeded.Store(true)
	self.wake([]byte{})
}

func (self *pushInbox) wake(groupId []byte) {
	select {
	case self.pushes <- groupId:
	default:
	}
}

func (self *pushInbox) close() {
	self.unsubscribePush()
	self.unsubscribeReplaced()
}

// NewDevice opens this device's identity and its MLS engine.
//
// THE IDENTITY IS PERSISTED WHEN THE STORE CAN HOLD ONE, AND DRAWN FRESH WHEN IT CANNOT, and which
// of the two happened is a fact about the store the caller supplied rather than a mode.
//
//   - Over a [DeviceStore] -- which [OpenDurableStateStore] is -- the signature key pair and the
//     leaf keys body are read back if the directory holds them and are minted and written once if
//     it does not. That is what makes a restart a RESTORE: [mls.LoadGroup] verifies the restored
//     group's own leaf against the key handed in, so a device with a new signature key is refused
//     by every group it was in, and a durable store would be write-only without this.
//   - Over anything else -- [MemoryStateStore], or a caller's own map -- a fresh pair is drawn
//     every process, exactly as before this paragraph existed. A device that restarts is then a
//     new device: it is not the leaf any group remembers, and it re-joins rather than resumes.
//
// THE X-WING LEAF PRIVATE KEY IS KEPT, on both paths. S2-26, ledger item 243's first step.
// [messagegroup.XwingGenerateKey] draws a pair, the PUBLIC half is encoded into the leaf keys
// extension as before, and the 32 octet SEED under it is now held in [Device.wrapSeed] and
// written into the same durable record as the leaf keys body it belongs to. Until this landed the
// private half was unreferenced the moment it was drawn, so this device could never open an
// X-Wing device wrap addressed to the leaf it publishes -- and it published that leaf anyway,
// which is a device advertising a wrap target it cannot read. Nothing in the alpha opens a device
// wrap yet (6.1's wraps carry no key material; see [alphaWrapBody]) and nothing in this change
// makes one: what it buys is that [Device.DecapsulateToOwnLeaf] exists and can be measured, which
// is what the rest of item 243 is built on.
//
// A STORE WRITTEN BEFORE THIS IS NOT REFUSED AND DOES NOT GET A NEW KEY. It restores with an
// empty seed and every path but the decapsulation behaves exactly as it did;
// [DurableStateStore.GetDeviceIdentity] carries the full reasoning, including why minting a
// replacement here would be worse than the absence.
//
// AND A DEVICE THAT IS NOT BUILT ERASES THE SEED IT WAS HANDED. Between `deviceIdentity` and the
// struct literal below, this function holds a private key across a call that can refuse, and a
// refusal there produces no [Device] and therefore no [Device.Close] to clear it. The deferred
// erase at the binding covers every exit below it, which is a property of the shape rather than of
// the exits that exist today -- see the comment there, and
// TestEveryPathThatDropsTheDeviceErasesItsWrapSeed, which asserts it over this function's source.
func NewDevice(config DeviceConfig) (*Device, error) {
	if config.Transport == nil {
		return nil, ErrNoTransport
	}
	if config.Reserver == nil {
		return nil, ErrNoReserver
	}
	random := config.Random
	if random == nil {
		random = rand.Reader
	}
	nowMs := config.NowMs
	if nowMs == nil {
		nowMs = func() int64 { return time.Now().UnixMilli() }
	}
	stateStore := config.StateStore
	if stateStore == nil {
		stateStore = NewMemoryStateStore()
	}

	crypto, err := mls.NewCryptoProvider(deviceCipherSuite)
	if err != nil {
		return nil, fmt.Errorf("urmessage: the mls crypto provider: %w", err)
	}
	signer, signerPub, leafKeys, wrapSeed, err := deviceIdentity(crypto, stateStore, random)
	// THE SEED IS LIVE FROM HERE AND EVERY EXIT BELOW BUT ONE DROPS IT, so the erase is registered
	// once, here, rather than written in front of the exits that exist today. Until this defer,
	// [NewDevice] held a private key across one fallible call -- the engine's construction -- and
	// returned it to the heap intact when that call refused. Nothing would ever Close such a
	// device, because it was never built.
	//
	// IT IS REGISTERED BEFORE THE ERROR CHECK ON PURPOSE, which costs a no-op: on that arm
	// `deviceIdentity` answers a nil seed and [zeroizeState] over nil does nothing. What it buys
	// is that "every return below this line erases" is a property of the SHAPE rather than a case
	// analysis a reader has to redo after each new return -- which is the whole of what
	// TestEveryPathThatDropsTheDeviceErasesItsWrapSeed asserts, over this function and not over a
	// list of its exits.
	held := false
	defer func() {
		if !held {
			zeroizeState(wrapSeed)
		}
	}()
	if err != nil {
		return nil, err
	}
	// THE CREDENTIAL IDENTITY IS THE SIGNER'S PUBLIC HALF, which is a decision and not an
	// accident. The alpha has no identity system at all -- contact cards and the rendezvous are
	// out of scope -- so the only thing a credential could honestly name is the key this device
	// signs with, and naming anything else would be publishing an identity nobody can check.
	// What a member reads off MemberAt is therefore exactly "the leaf that signs", and MG-1's
	// obligation -- that a joiner must decide whether it expected THAT identity -- is the
	// caller's and is not met here.
	engine, err := messagegroup.NewConnectMlsEngine(crypto, stateStore, signer,
		mls.BasicCredential(signerPub), leafKeys)
	if err != nil {
		return nil, fmt.Errorf("urmessage: the mls engine: %w", err)
	}
	// the one exit that hands the seed on: the field below holds the array the defer would
	// otherwise clear, and from here [Device.Close] owns it.
	inbox := newPushInbox(config.Transport)
	held = true
	return &Device{
		transport:        config.Transport,
		reserver:         config.Reserver,
		connect:          config.Connect.withDefaults(),
		crypto:           crypto,
		engine:           engine,
		leafKeys:         leafKeys,
		stateStore:       stateStore,
		identityPub:      append([]byte(nil), signerPub...),
		wrapSeed:         wrapSeed,
		nowMs:            nowMs,
		random:           random,
		commitAuthorizer: config.CommitAuthorizer,
		groups:           map[string]*Group{},
		inbox:            inbox,
	}, nil
}

// deviceIdentity is the signature key pair, the leaf keys body and the X-Wing seed under it that
// this device runs under: read back from a durable store when there is one, minted and written
// once when there is not.
//
// THE MINT-AND-WRITE IS ONE STEP AND ITS FAILURE IS THE CALL'S. A device that minted an identity,
// failed to write it and ran anyway would found groups under a key the next process cannot
// produce -- which is the same state as no store at all, reached by a path nobody would look at
// again.
//
// A STORE THAT REFUSES FOR ANY OTHER REASON IS NOT TREATED AS AN EMPTY ONE. Only
// [ErrNoDeviceIdentity] falls through to the mint; a disk that would not answer is returned,
// because minting over it would silently replace an identity that is still on the disk and leave
// every group this device is in unreachable.
//
// AND A MINT THAT DOES NOT COMPLETE ERASES WHAT IT DREW. The seed is live from `xwing.Seed()`
// until the return, across two statements that can both refuse -- the leaf keys encoding and the
// write -- and until the deferred erase below it left the heap on either of them holding the
// private half of a key pair whose public half nothing had published. The restore arm has a cover
// of its own for the same reason, on an array the store answered. Neither erase reaches the copy
// INSIDE the transient `messagegroup.XwingPrivateKey`; that limit is stated at `wrapSeed` below
// and is `connect`'s to fix, not this step's.
//
// THE SEED AND THE LEAF KEYS BODY ARE ONE VALUE HERE AND ONE RECORD THERE. The public half this
// function encodes and the seed it keeps come from a single [messagegroup.XwingGenerateKey] call
// and are handed to [DeviceStore.PutDeviceIdentity] together, so there is no ordering in which a
// crash leaves a stored public half beside a seed that does not expand to it. A store that held
// no seed -- one written before this existed -- answers an empty fourth value and the mint is NOT
// re-run over it: see [DurableStateStore.GetDeviceIdentity].
func deviceIdentity(crypto mls.CryptoProvider, stateStore mls.StateStore, random io.Reader) (
	mls.SignaturePrivateKey, mls.SignaturePublicKey, []byte, []byte, error) {

	store, durable := stateStore.(DeviceStore)
	if durable {
		pub, priv, leafKeys, wrapSeed, err := store.GetDeviceIdentity()
		// the restore arm's own cover. The array is the store's answer, freshly decoded and
		// referenced by nothing else, so it is this call's to erase -- and on the one exit below
		// that drops it the store refused and the array is empty, which makes this erase a no-op
		// TODAY. It is registered anyway, for the reason [NewDevice]'s is: a cover written only
		// where a value happens to be non-empty is a cover that has to be re-argued the day the
		// arm changes, and nothing would report that it had not been.
		restored := false
		defer func() {
			if !restored {
				zeroizeState(wrapSeed)
			}
		}()
		switch {
		case err == nil:
			restored = true
			return mls.SignaturePrivateKey(priv), mls.SignaturePublicKey(pub), leafKeys, wrapSeed, nil
		case errors.Is(err, ErrNoDeviceIdentity):
			// the ordinary state of a fresh directory: fall through and mint.
		default:
			return nil, nil, nil, nil, fmt.Errorf("urmessage: this device's persisted identity: %w", err)
		}
	}
	signer, signerPub, err := crypto.SignatureKeyPair()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("urmessage: this device's signature key pair: %w", err)
	}
	xwing, err := messagegroup.XwingGenerateKey(random)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("urmessage: this device's x-wing key: %w", err)
	}
	// Seed answers a copy, so this array is this call's own and is what the device holds and
	// erases. The copy INSIDE xwing is not reachable from here and is not erasable from here:
	// `messagegroup.XwingPrivateKey` declares no erase and `connect` is not this step's to
	// change. That is a real and stated limit on the erase below, not a claim it covers.
	wrapSeed := xwing.Seed()
	// AND THE MINT ARM'S, WHICH IS THE ONE THE FINDING WAS ABOUT. The seed exists for two more
	// fallible statements -- the leaf keys encoding and the write -- before it is handed back, and
	// until this defer BOTH of those refusals returned it to the heap with its octets intact. The
	// leaf keys arm is not reachable from any case: Encode's two refusals are the alg_id, which
	// this call writes as a constant, and the public half's length, which comes out of
	// XwingGenerateKey -- so it is this ONE cover over BOTH returns that carries the measurement
	// from the arm a case can drive to the arm it cannot, and
	// TestEveryPathThatDropsTheDeviceErasesItsWrapSeed asserts that there is exactly one.
	minted := false
	defer func() {
		if !minted {
			zeroizeState(wrapSeed)
		}
	}()
	leafKeys, err := (&mls.LeafKeysExtension{
		AlgId:          mls.AlgIdXwing,
		DeviceXwingPub: xwing.Public().Bytes(),
	}).Encode()
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("urmessage: this device's leaf keys extension: %w", err)
	}
	if durable {
		if err := store.PutDeviceIdentity(signerPub, signer, leafKeys.ExtensionData, wrapSeed); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("urmessage: this device's identity could not be persisted: %w", err)
		}
	}
	// the mint arm's one exit that hands the seed on, and from here it is [NewDevice]'s.
	minted = true
	return signer, signerPub, leafKeys.ExtensionData, wrapSeed, nil
}

// Connect performs 4.3.1's Hello and rebinds every live group onto the nonce it issued.
//
// IT IS THE ONLY PLACE A NONCE ENTERS THIS PACKAGE and it is idempotent: calling it again is a
// reconnect, which is the S2-2 case the package document states. A group whose session refuses the
// new nonce is reported here rather than at its next send.
//
// IT RETRIES A HELLO THAT IS NOT ANSWERED, ACROSS THE OPERATOR WINDOW DESCRIBED AT [ConnectPolicy],
// AND IT SAYS "NOT YET" RATHER THAN "FAILED". A reconnecting client_id is not routed to for about
// sixty seconds on the deployed server; this call rides that out and, if its budget runs out first,
// answers [ErrReconnecting] so that a caller can tell "keep waiting" from "something is wrong".
//
// AND THE TWO KINDS OF FAILURE ARE NOT THE SAME KIND, which is the whole of what makes the retry
// safe. SILENCE is retried: no answer arrived, and the one thing known about the server is that it
// has said nothing. AN ANSWER IS NOT: a Hello the server REFUSED by reason, or one that issued no
// server_nonce, is the server speaking, and speaking is not the state this window produces --
// retrying it would turn one clear refusal into ninety seconds of the same refusal. So
// [ErrHelloRefused] and [ErrNotConnected] come straight back on the first attempt, exactly as
// before.
//
// THE CALLER'S OWN ctx STILL ENDS IT IMMEDIATELY. A cancelled or expired caller context is not
// "not yet": it is the caller saying stop, and it is returned rather than retried.
//
// THE BUDGET BOUNDS THE CALL, AND UNTIL THIS PARAGRAPH IT DID NOT. The budget used to be consulted
// only AFTER an attempt returned, and nothing asked whether the NEXT attempt fitted in what was
// left, so one call could return a whole AttemptTimeout past its budget: a 500 ms budget blocked
// for the 10 s default attempt, and the defaults' own schedule returned at about 100 s against the
// 90 s the C header states. For a UI that is a hang. Now every attempt is CUT TO WHAT IS LEFT of the
// budget -- so a budget shorter than one attempt is one attempt of the budget's length -- and when a
// full pause would leave nothing for another attempt, the rest of the budget is spent on one last
// attempt at once rather than on a pause no attempt follows.
// TestTheConnectBudgetBoundsTheCallWhateverTheAttemptTimeout holds it.
func (self *Device) Connect(ctx context.Context) error {
	policy := self.connect
	started := time.Now()
	backoff := policy.FirstBackoff
	var lastErr error
	lastChance := false
	for attempt := 1; ; attempt += 1 {
		timeout := policy.AttemptTimeout
		if remaining := policy.Budget - time.Since(started); remaining < timeout {
			timeout = remaining
		}
		if timeout <= 0 {
			// reachable only when a pause overran what was left of the budget. An attempt with
			// no time is not an attempt, and it is not reported as one. NOTHING GOES RED WITHOUT
			// THIS CLAUSE, measured: a pause is only taken when strictly more than it is left, so
			// only a timer firing late reaches here, and no case can make one.
			return self.reconnecting(attempt-1, time.Since(started), lastErr)
		}
		reason, hello, err := self.helloOnce(ctx, timeout)
		switch {
		case err == nil && reason != protocol.Reason_REASON_OK:
			// THE SERVER SPOKE. Not this window, and not retried.
			return fmt.Errorf("%w: %v", ErrHelloRefused, reason)
		case err == nil && len(hello.GetServerNonce()) == 0:
			return fmt.Errorf("%w: Hello issued no server_nonce", ErrNotConnected)
		case err == nil:
			return self.rebindAll()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			// the CALLER's context, not the per-attempt one. Stop means stop.
			return fmt.Errorf("urmessage: Hello: %w", ctxErr)
		}
		lastErr = err
		elapsed := time.Since(started)
		remaining := policy.Budget - elapsed
		final := lastChance || remaining <= 0
		pause := backoff
		switch {
		case final:
			pause = 0
		case remaining <= pause:
			// A FULL PAUSE WOULD LEAVE NOTHING TO TRY WITH. The pause exists to space attempts
			// out, and a pause that no attempt follows only lengthens the block; so the rest of
			// the budget is ONE attempt, at once, and then the call ends whatever it answers --
			// which is also what stops an attempt that fails instantly from looping here.
			pause = 0
			lastChance = true
		}
		if policy.OnAttempt != nil {
			policy.OnAttempt(ConnectAttempt{
				Attempt: attempt, Elapsed: elapsed, Backoff: pause, Err: err,
			})
		}
		if final {
			return self.reconnecting(attempt, elapsed, lastErr)
		}
		if err := self.pause(ctx, pause); err != nil {
			return fmt.Errorf("urmessage: Hello: %w", err)
		}
		if backoff < policy.MaxBackoff {
			backoff *= 2
			if policy.MaxBackoff < backoff {
				backoff = policy.MaxBackoff
			}
		}
	}
}

// reconnecting is the "not yet, ask again" answer a budget spent on silence gets.
func (self *Device) reconnecting(attempts int, elapsed time.Duration, lastErr error) error {
	return fmt.Errorf(
		"%w: %d Hello attempts over %v were not answered; a reconnecting client_id is not routed to for about 60s on this server (msgrepo operator item 5), so this is 'not yet' rather than 'failed': %w",
		ErrReconnecting, attempts, elapsed.Round(time.Millisecond), lastErr)
}

// helloOnce is one Hello under its own deadline, so that a server answering nothing costs this
// attempt's timeout rather than the transport's.
//
// THE PER-ATTEMPT CONTEXT IS DERIVED FROM THE CALLER'S, so a cancelled caller cancels the attempt
// in flight and the loop above can tell the two apart by asking the CALLER's context afterwards.
func (self *Device) helloOnce(ctx context.Context, timeout time.Duration) (
	protocol.Reason, *protocol.HelloResponse, error) {

	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	reason, hello, err := self.transport.Hello(attemptCtx)
	if err != nil {
		return protocol.Reason_REASON_INTERNAL, nil, fmt.Errorf("urmessage: Hello: %w", err)
	}
	return reason, hello, nil
}

// pause waits, or answers the caller's context ending first.
func (self *Device) pause(ctx context.Context, howLong time.Duration) error {
	if howLong <= 0 {
		return nil
	}
	timer := time.NewTimer(howLong)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// rebindAll moves every live session onto the nonce this transport now holds.
func (self *Device) rebindAll() error {
	for _, group := range self.Groups() {
		if err := group.rebind(); err != nil {
			return err
		}
	}
	return nil
}

// KeyPackage publishes one single-use MLS key package, which is what another device adds to a
// group with.
//
// IT IS SINGLE USE AND THE STORE ENFORCES THAT: the private halves are taken destructively at the
// join, so two joins off one published package is not something this device can do.
func (self *Device) KeyPackage() ([]byte, error) {
	keyPackage, err := self.engine.NewKeyPackage()
	if err != nil {
		return nil, fmt.Errorf("urmessage: this device's key package: %w", err)
	}
	return keyPackage, nil
}

// DecapsulateToOwnLeaf opens an X-Wing encapsulation addressed to the leaf THIS device publishes
// and answers the shared secret, [messagegroup.XwingSharedSize] octets.
//
// IT IS S2-26's WHOLE POINT AND IT IS DELIBERATELY NOT A WRAP. Ledger item 243 rotates
// `pq_secret` per epoch and ruling 36 fixed the carrier as the X-Wing device wrap; the wrap
// record, its body, its signature and the `env_key` under it are LATER steps and ruling 37 fixes
// their shape. What was missing before this method is cruder than any of that: the public half
// travels in every leaf's urmessage_leaf_keys extension and NO device held the private half, so
// there was no answer to "can a device open what is addressed to it" at all. This is that answer
// and nothing more.
//
// THE KEY IS RE-EXPANDED ON EVERY CALL, from the seed, and that is a choice. Caching the expanded
// pair would mean a `*messagegroup.XwingPrivateKey` in a field -- unerasable from this package,
// and the thing `connect/mls`'s erase gate excuses on the ground that nothing holds one. The cost
// is one SHAKE-256 and one ML-KEM-768 key generation per call, against a fan-out that addresses
// this leaf a small fixed number of times per epoch -- two records at one wrap_target_handle
// under the 2026-09-13 device-wrap split, by design and in the normal case.
//
// A DEVICE WITH NO SEED REFUSES BY NAME. [ErrNoDeviceWrapKey] is a store written before the seed
// was retained, or a device that has been Closed. It is never a zero-filled seed, which would
// expand into a valid key pair and answer a wrong secret that looks exactly like a right one.
func (self *Device) DecapsulateToOwnLeaf(ciphertext []byte) ([]byte, error) {
	// HELD ACROSS THE EXPANSION rather than copied out of: [Device.Close] overwrites this array
	// in place, and the alternative to the lock is a second copy of a private key to erase.
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if len(self.wrapSeed) == 0 {
		return nil, ErrNoDeviceWrapKey
	}
	private, err := messagegroup.XwingKeyGenFromSeed(self.wrapSeed)
	if err != nil {
		return nil, fmt.Errorf("urmessage: this device's x-wing key: %w", err)
	}
	shared, err := messagegroup.XwingDecapsulate(private, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("urmessage: this device's leaf could not open this encapsulation: %w", err)
	}
	return shared, nil
}

// openWrapToOwnLeaf opens one MASTER section 7 wrap body addressed to the leaf THIS device
// publishes, and answers its envelope and its payload.
//
// IT IS [Device.DecapsulateToOwnLeaf] ONE LAYER OUT AND IT IS NOT THAT METHOD WITH AN EXTRA STEP.
// The KEM's answer is not the answer: X-Wing's ML-KEM-768 half uses implicit rejection, so a
// ciphertext produced for some other leaf decapsulates SUCCESSFULLY to a pseudorandom secret --
// which is what leaves no oracle to query and is why [messagegroup.XwingDecapsulate] returns a
// secret and no verdict. Everything that tells "this wrap is mine" from "this wrap is not" happens
// above the KEM, inside [messagegroup.OpenWrapBody]: the envelope comparison the opener states
// with its own seven arguments, and then the Poly1305 tag. A caller that reached for the shared
// secret here and derived its own key would be reimplementing MASTER section 7's nine-element info
// on this side of the seam, which is the one thing a wrap format cannot survive two copies of.
//
// THE KEY IS RE-EXPANDED ON EVERY CALL, for [Device.DecapsulateToOwnLeaf]'s reason unchanged:
// caching the pair would mean a `*messagegroup.XwingPrivateKey` in a field, which declares no
// erase and which connect/mls's erase gate excuses precisely on the ground that nothing holds one.
// A fan-out addresses this leaf a small fixed number of times per epoch.
//
// IT IS UNEXPORTED BECAUSE ITS CALLER IS THIS PACKAGE'S WALK. The exported surface for "can this
// device open what is addressed to it" is DecapsulateToOwnLeaf, which is S2-26's measurement and
// answers a secret; this one answers a wrap's payload and is only meaningful beside the record
// that carried it.
func (self *Device) openWrapToOwnLeaf(groupId []byte, contentEpoch uint64, targetType uint8,
	targetId []byte, payloadType uint8, body []byte) (messagegroup.WrapEnvelope, []byte, error) {

	// HELD ACROSS THE EXPANSION, exactly as above: [Device.Close] overwrites this array in place.
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if len(self.wrapSeed) == 0 {
		return messagegroup.WrapEnvelope{}, nil, ErrNoDeviceWrapKey
	}
	private, err := messagegroup.XwingKeyGenFromSeed(self.wrapSeed)
	if err != nil {
		return messagegroup.WrapEnvelope{}, nil, fmt.Errorf("urmessage: this device's x-wing key: %w", err)
	}
	return messagegroup.OpenWrapBody(private, groupId, contentEpoch, targetType, targetId, payloadType, body)
}

// Groups is every group this device holds, in no particular order.
func (self *Device) Groups() []*Group {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	groups := make([]*Group, 0, len(self.groups))
	for _, group := range self.groups {
		groups = append(groups, group)
	}
	return groups
}

// Close closes every group's session AND ERASES THIS DEVICE'S WRAP SEED. The transport and the
// connect client under it are the caller's and are not closed.
//
// THE SEED IS ERASED HERE BECAUSE THIS IS WHERE THE DEVICE IS DROPPED. It is the only key
// material this type holds in a field of its own -- the signer lives in the engine, every epoch
// secret in the session -- and a field holding key material that no path clears is what
// `connect/mls`'s erase gate refuses one package over. [zeroizeState] is this package's one
// erase and its own header says what it can and cannot reach; what it reaches here is the ONE
// array [NewDevice] held rather than copied.
//
// IT IS IDEMPOTENT AND IT IS NOT A RESET. The field is set to nil after the overwrite, so a
// second Close erases nothing and a decapsulation after a Close refuses by name
// ([ErrNoDeviceWrapKey]) instead of decapsulating under 32 zero octets -- which is a well formed
// X-Wing seed and would answer a perfectly uniform-looking wrong secret.
// helloIfReplaced says Hello when the transport reported a new session since the last one, on
// the caller's goroutine and context. A failed Hello leaves it owed, so the next call tries again.
func (self *Device) helloIfReplaced(ctx context.Context) error {
	if !self.inbox.helloNeeded.CompareAndSwap(true, false) {
		return nil
	}
	reason, hello, err := self.transport.Hello(ctx)
	if err == nil && reason == protocol.Reason_REASON_OK && 0 < len(hello.GetServerNonce()) {
		return nil
	}
	self.inbox.helloNeeded.Store(true)
	if err != nil {
		return fmt.Errorf("%w: the session to the server was replaced, and the Hello on the new one failed: %w", ErrNotConnected, err)
	}
	return fmt.Errorf("%w: the session to the server was replaced, and the Hello on the new one was answered %v", ErrNotConnected, reason)
}

// WaitPush answers the group id of the next §4.3.5 push, or the context's error. A push says only
// that the group has records above what this device was last told; [Group.Receive] is what reads
// them, so a caller answers a push with a Receive. An EMPTY id means the session to the server was
// replaced: the same Receive answers it too, and says Hello on the new session first.
func (self *Device) WaitPush(ctx context.Context) ([]byte, error) {
	select {
	case groupId := <-self.inbox.pushes:
		return groupId, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (self *Device) Close() error {
	if self.inbox != nil {
		self.inbox.close()
	}
	self.mutex.Lock()
	groups := make([]*Group, 0, len(self.groups))
	for _, group := range self.groups {
		groups = append(groups, group)
	}
	self.groups = map[string]*Group{}
	zeroizeState(self.wrapSeed)
	self.wrapSeed = nil
	self.mutex.Unlock()
	var first error
	for _, group := range groups {
		if err := group.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// nonce is the connection's server_nonce and the Hello count it was issued at, or a refusal that
// names the absence.
func (self *Device) nonce() ([]byte, uint64, error) {
	nonce := self.transport.Nonce()
	if len(nonce) == 0 {
		return nil, 0, ErrNotConnected
	}
	return nonce, self.transport.NonceEpoch(), nil
}

// createMlsGroup founds the MLS group one [Group] is a view of.
//
// The policy names this device as the group's owner, which is the only role there is to assign
// before there is a second member and is what mls.GroupPolicyExtension.Validate requires. It is
// canonicalized before it is encoded, because the extension travels inside the group context and
// two members that encoded it differently would export different secrets.
func (self *Device) createMlsGroup(groupId []byte) (messagegroup.GroupHandle, error) {
	policy := &mls.GroupPolicyExtension{
		Roles: []mls.RoleEntry{{MemberId: self.identityPub, Role: mls.RoleOwner}},
	}
	if err := policy.Canonicalize(); err != nil {
		return nil, fmt.Errorf("urmessage: the group policy: %w", err)
	}
	encoded, err := policy.Encode()
	if err != nil {
		return nil, fmt.Errorf("urmessage: the group policy: %w", err)
	}
	handle, err := self.engine.CreateGroup(groupId, encoded.ExtensionData, self.leafKeys)
	if err != nil {
		return nil, fmt.Errorf("urmessage: CreateGroup: %w", err)
	}
	return handle, nil
}

func (self *Device) hold(group *Group) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.groups[string(group.id)] = group
}
