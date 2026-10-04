package urmessage

import (
	"testing"
)

// ══════════════════════════════════════════════════════════════════════════════════════════════
// S2-26's CENSUS: THE DEVICE'S X-WING SEED GOES WHERE THE DISPOSITION BELOW SAYS IT GOES
// ══════════════════════════════════════════════════════════════════════════════════════════════
//
// WHY THIS FILE EXISTS, AND IT IS A FINDING RATHER THAN A PRECAUTION. S2-26 added a private key to
// a type in this package and added no census for it. The package's one dataflow gate --
// TestEveryEpochKeyInThisPackageGoesWhereTheDispositionSaysItGoes -- starts its walk at
// `epochKeyProducerSelectors = {WriteKey, ReadKey, GetWriteKey, GetReadKey}`, and the seed's
// producers are `xwing.Seed()`, `deviceIdentity`, `store.GetDeviceIdentity()` and the `wrapSeed`
// field itself. None of those four is in that net, so NO GATE IN THIS PACKAGE CENSUSED WHERE THE
// SEED'S VALUE GOES. Two gates did see the field -- the AST walk in devicewrapkey_test.go reads its
// declaration and refuses an unerasable one, and TestClosingADeviceErasesTheSeedAndNotOnlyTheField
// reads its octets after a Close -- and neither of those asks where the value LANDS, which is the
// gap. MEASURED, on the commit this file repairs: two production sites mutated to leak it --
//
//	device.go:419  fmt.Errorf("urmessage: this device's identity (seed %x) could not be persisted: %w", wrapSeed, err)
//	device.go:621  fmt.Errorf("urmessage: this device's leaf (seed %x) could not open this encapsulation: %w", self.wrapSeed, err)
//
// -- and the mutant passed `go test ./urmessage/ -run '.*' -timeout 1800s` at `ok 6.147s`, with
// TestEveryEpochKeyInThisPackageGoesWhereTheDispositionSaysItGoes,
// TestNoFieldInThisPackageHoldsAnUnerasableXwingPrivateKey and
// TestEveryFsyncInThisPackageIsAtASiteThisSuiteNames all `--- PASS`. The inline control, which
// fires for its own reason: the SAME snapshot/mutate/revert harness applied to `Close`'s
// `zeroizeState(self.wrapSeed)` produced `--- FAIL: TestClosingADeviceErasesTheSeedAndNotOnlyTheField`,
// so the harness and the suite do kill a mutant of this package WHEN A GATE COVERS THE MECHANISM.
// There was no leak in the tree and this file does not pretend to have found one. What was missing
// is the refusal: the epoch-key gate's own header names `fmt.Errorf("%x", writeKey)` -- "an epoch
// key in an error string, in every log that error ever reaches" -- as the exact shape it exists to
// refuse, and nothing refused that shape for the seed. This file does.
//
// AND THE NET STOPPED ONE CALL SHORT OF THE DISK, which is the second finding this file carries
// and is a defect of this file rather than of the package. The disposition named
// `PutDeviceIdentity|call self.writeRecord` "THE SEED GOING TO DISK" while `writeRecord` and
// `encodeStateRecord` -- the two functions the seed actually passes through on its way there --
// were outside the census, so a `%x` of it inside either was refused by nothing. Measured the same
// way: both mutants survived `go test ./urmessage/ -run '.*' -timeout 1800s` at `ok 6.117s` with
// this gate `--- PASS`. The repair is one name in the net below (`parts`), because PRODUCER ONE
// already knew how to seed a write path from a parameter; and where the walk NOW ends is written
// down and asserted in [wrapSeedAccumulatorSites] rather than left to be inferred. M10-M15 at the
// foot of this file drive it.
//
// IT IS THE EPOCH-KEY GATE'S SHAPE AND IT INHERITS THAT FILE'S THREE SCARS DELIBERATELY. Read its
// header for the measurements; the short form is that a census must match the VALUE and not the
// field name (a nested type spelling the keys `wk`/`rk` walked past a gate keyed on strings), must
// ask every landing place for an EXPRESSION and not an *ast.Ident (`x = writeKey[:]` was censused
// as nothing at all, not refused -- absent), and must follow EVERY binding form (`var leaked =
// writeKey` bound nothing when the fixpoint read *ast.AssignStmt alone). All three clauses are
// here, and the mutation table at the bottom of this file drives all three at the seed.
//
// WHERE IT DIFFERS FROM THE EPOCH-KEY GATE, and it is one clause: A COUNT OF A KEY IS NOT A KEY.
// `len(wrapSeed)` answers an int. Both of this package's store-side refusals format exactly that,
// so a census that called `len(wrapSeed)` a carried value would put `PutDeviceIdentity|call
// fmt.Errorf` in the disposition with `wrapSeed` in its carries list -- and that entry would then
// be a STANDING PERMIT to format the seed itself at that same call, which is the defect this file
// exists for, reintroduced by its own repair. So [censusBorneBy] refuses to look through the
// builtin `len`. THAT IS A NARROWING AND IT IS ASSERTED RATHER THAN PRINTED: every site the
// narrowing removed is collected in its own census and held both ways against
// [wrapSeedCountedNotCarriedSites] below, so an excluded site is a site somebody weighed and wrote
// down, and the day the tree stops counting the seed at one of them this file goes red. The
// mutation table drives it from the other side too: `len(wrapSeed)` -> `wrapSeed` at that same
// error must be a REFUSAL, and it is.
//
// THE OVER-APPROXIMATION PULLS IN THE SIGNER AND THAT IS COVERAGE AND NOT CONFUSION. The seed is
// born, returned and stored in the same statements as this device's signature private key --
// `deviceIdentity` hands back four values and `PutDeviceIdentity` writes them as one record -- so
// a taint that follows a multi-value binding reaches `signer` and `signerPub` as well. They are
// key material too. They are censused, dispositioned by name, and nothing here pretends the walk
// distinguishes them.

// The names that PRODUCE the seed, in any of the three positions a name can produce one: the
// callee of a call, a bare field read, or a parameter this function was handed. This is the SEARCH
// NET rather than a disposition -- a name here that no site uses costs nothing and widens the net,
// while a producer spelled some other way is a blindness this gate cannot see, which is what
// [wrapSeedProducerSites] is held BOTH WAYS for.
//
//   - `Seed` is `xwing.Seed()` in deviceIdentity, the mint. It answers a copy, which is why the
//     device can hold that array rather than copy it again.
//   - `GetDeviceIdentity` is the store's read, whose FOURTH result is the seed, and `deviceIdentity`
//     is the package helper whose fourth result is the same value one call further out. Both are
//     matched as callees and both are matched as ENCLOSING FUNCTION NAMES -- see the seeding below,
//     which is what censuses their own bodies.
//   - `wrapSeed` is the field read `self.wrapSeed` AND the parameter `PutDeviceIdentity(…, wrapSeed
//     []byte)`. The store's write path receives the value with a name and nothing else about it is
//     distinguishable from the three other parts of the record it goes into, so the name is the
//     only handle there is. Rename that parameter and the producer site disappears, which the
//     both-ways hold reports as an entry nothing needs.
var wrapSeedProducerNames = map[string]bool{
	"Seed":              true,
	"GetDeviceIdentity": true,
	"deviceIdentity":    true,
	"wrapSeed":          true,
	"parts":             true,
}

// Every place in this package's production source that produces the seed, as
// "<enclosing function>|<site as written>" -> why it is there. HELD BOTH WAYS: a site with no
// entry is a seed coming from somewhere nobody weighed, and an entry with no site is this gate
// having gone BLIND -- the producer was respelled and every sink clause below is now searching a
// value nothing tainted, which passes by finding nothing.
var wrapSeedProducerSites = map[string]string{
	"NewDevice|deviceIdentity": "the device's identity, four values at once, on the one road " +
		"into [Device]. Its fourth result is the seed.",
	"deviceIdentity|store.GetDeviceIdentity": "the RESTORE arm: the seed as the durable store " +
		"last wrote it, or empty for a store written before the field existed.",
	"deviceIdentity|xwing.Seed": "the MINT arm: the 32 octets under the public half this same " +
		"call encodes into the leaf keys extension. Seed answers a copy, so this array is this " +
		"call's own -- which is what lets [Device.Close] erase ONE array rather than chase two.",
	"PutDeviceIdentity|parameter wrapSeed": "the store's WRITE path, where the value arrives from " +
		"the caller with a name and is otherwise indistinguishable from the three record parts " +
		"beside it. Seeding here is what puts statestore_durable.go inside this census at all.",

	// ── THE TWO CALLS FURTHER DOWN THE SAME ROAD ─────────────────────────────────────────────
	//
	// THE NET USED TO STOP ONE CALL SHORT OF THE DISK, AND THAT WAS A FINDING RATHER THAN A
	// CHOICE. `PutDeviceIdentity|call self.writeRecord` was dispositioned below as "THE SEED
	// GOING TO DISK" -- and `writeRecord` and `encodeStateRecord` were outside the census
	// entirely, so a `%x` of the seed INSIDE either of them was refused by nothing. MEASURED, on
	// the commit this entry repairs, two production sites mutated to leak it:
	//
	//	statestore_durable.go:720  fmt.Errorf("%w: %s could not be written (record %x): %v", …, record, err)
	//	statestore_durable.go:346  fmt.Errorf("%w: a part of %d octets (%x) …", …, len(part), part)
	//
	// -- and BOTH survived `go test ./urmessage/ -run '.*' -timeout 1800s`, which answered
	// `ok 6.117s`, with this very gate `--- PASS` when run by name. The seed reaches the disk
	// through two calls and the disposition named only the first of them.
	//
	// THE REPAIR IS THE MECHANISM THE FILE ALREADY HAD, not a new one. `PutDeviceIdentity` is
	// inside this census because PRODUCER ONE seeds a PARAMETER whose name is in the net above;
	// `writeRecord` and `encodeStateRecord` receive the same value as `parts`, so `parts` joins
	// the net and both functions seed themselves exactly as the store's write path does. That is
	// also why the net is a set of NAMES and not of call sites: one name added reaches every
	// function that spells the value that way.
	//
	// AND `parts` IS SEEDED AS A PARAMETER ONLY, which is what keeps this widening from swallowing
	// the package. Seven functions in statestore_durable.go declare a LOCAL called `parts` -- every
	// Get/Take/Records reader -- and PRODUCER ONE reads parameters and named results, so none of
	// them is tainted by this line. The one that is censused, `GetDeviceIdentity`, was already
	// censused before it, through PRODUCER TWO and its own name.
	"groupRecordOf|parameter parts": "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the six parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Seeding a PARAMETER is " +
		"what makes that function censusable at all -- see pq-M2.",
	"writeRecord|parameter parts": "THE FIRST OF THE TWO CALLS THE NET USED TO STOP SHORT OF. " +
		"Every durable value in this package is written through it -- the identity record among " +
		"them -- and the seed arrives here as one of four `parts`, with no name of its own left.",
	"encodeStateRecord|parameter parts": "THE SECOND, AND THE LAST PLACE THE SEED IS A GO VALUE " +
		"BEFORE IT IS OCTETS IN A FRAME. It is where the record is assembled, and it is the " +
		"function whose loop touches the seed's own array one part at a time.",
	"DecapsulateToOwnLeaf|self.wrapSeed": "the field read, under [Device.mutex], on the one path " +
		"that uses the seed for what it is for.",
	"openWrapToOwnLeaf|self.wrapSeed": "the field read, under the same mutex, on the SECOND path " +
		"that uses it for what it is for: opening a device wrap addressed to this leaf, which is " +
		"ledger item 243's receive leg. Two paths and not one because the wrap door needs the " +
		"PRIVATE KEY and not a shared secret -- see its own header -- and each is censused where " +
		"it stands rather than through a shared helper, so a door that stopped taking the mutex " +
		"or stopped checking the length is a site this gate reads on its own terms.",
	"Close|self.wrapSeed": "the field read on the ERASE path. It is a producer like any other read " +
		"of the field, and the sink it reaches is [zeroizeState].",

	// ── THE PRODUCER FUNCTIONS' OWN BODIES ───────────────────────────────────────────────────
	//
	// A producer named in the net above hands the seed back; INSIDE it, the value has not been
	// through a producer call and nothing would taint it. `DurableStateStore.GetDeviceIdentity`
	// is the case that matters: its results are unnamed and the seed is spelled `parts[3]`, a
	// generic record part. Without these entries the store's whole READ path -- the function
	// whose doc comment is the backward-compatibility ruling -- would be censused as nothing,
	// and `fmt.Errorf("%x", parts[3])` would be as invisible as the mutants above were. So every
	// result expression of a producer-named function seeds the taint at its own root name.
	"GetDeviceIdentity|result parts": "the record parts the identity read answers, three of them " +
		"or four. `parts[3]` is the seed when there are four; the other three are the signature " +
		"key pair and the leaf keys body, and this walk does not tell them apart.",
	"deviceIdentity|result leafKeys":  "the leaf keys extension, returned beside the seed.",
	"deviceIdentity|result signer":    "this device's signature PRIVATE key, minted in the same arm.",
	"deviceIdentity|result signerPub": "its public half.",
	"deviceIdentity|result wrapSeed":  "the seed itself, from both arms.",
}

// wrapSeedSink is one entry in the disposition below: WHICH VALUES a site may receive, and why. A
// disposition keyed on the site alone excuses the site FOREVER, whatever turns up there later, so
// each entry names the exact spellings it weighed and is held both ways on those too.
type wrapSeedSink struct {
	carries []string
	why     string
}

// Every place the seed VALUE lands, as "<enclosing function>|<site>" -> what may land and why.
//
// NOT ONE `fmt.Errorf` OR LOG SITE IS IN THIS MAP, and that absence is the property. Every
// formatting site in the four functions this census reaches formats a COUNT, which the narrowing
// above declines to carry and [wrapSeedCountedNotCarriedSites] writes down by name. So a
// formatting site appearing here at all is a new site with no entry, and it is refused -- which is
// what the two reproduced mutants do.
var wrapSeedSinks = map[string]wrapSeedSink{
	// ── THE MINT AND THE STORE ───────────────────────────────────────────────────────────────
	"deviceIdentity|call store.PutDeviceIdentity": {
		carries: []string{"leafKeys", "signer", "signerPub", "wrapSeed"},
		why: "THE SEED GOING TO DISK, and the leaf keys body going with it in the SAME CALL. That " +
			"is the point of the arrangement and not an accident of arity: the public half and the " +
			"seed under it come from one XwingGenerateKey and are written as one record, so no crash " +
			"leaves a stored public half beside a seed that does not expand to it. The seed is on " +
			"disk in the clear, like every other secret in this store -- S2-24, still open, and this " +
			"entry is where that fact is written down in a place a gate will keep honest.",
	},
	"deviceIdentity|call mls.SignaturePrivateKey": {
		carries: []string{"priv"},
		why: "the restore arm's cast of the stored signature private key to its named type. It is " +
			"tainted because the multi-value binding that produced it also produced the seed; the " +
			"walk does not separate the two and does not claim to.",
	},
	"deviceIdentity|call mls.SignaturePublicKey": {
		carries: []string{"pub"},
		why:     "the same cast for the public half.",
	},
	"deviceIdentity|return": {
		carries: []string{"leafKeys", "priv", "pub", "signer", "signerPub", "wrapSeed"},
		why: "THE ONE DOOR OUT OF THE MINT, and both arms use it. A seed handed to a caller leaves " +
			"this function by an exit no other clause watches -- the caller binds it from a call " +
			"nothing else taints -- and the caller is [NewDevice], whose own sites are below.",
	},
	"NewDevice|call zeroizeState": {
		carries: []string{"wrapSeed"},
		why: "THE ERASE ON THE WAY IN, inside the deferred closure that covers every exit of " +
			"[NewDevice] below the binding but the one that hands the seed to the field. It is the " +
			"same [zeroizeState] over the same array [Device.Close] clears at the other end of the " +
			"device's life, and it is here because a device whose engine refused is a device " +
			"nothing will ever Close. See TestEveryPathThatDropsTheDeviceErasesItsWrapSeed, which " +
			"asserts the COVER rather than this call.",
	},
	"deviceIdentity|call zeroizeState": {
		carries: []string{"wrapSeed"},
		why: "the same erase in both of `deviceIdentity`'s arms -- one deferred closure per live " +
			"range, the restore arm's over the array the store answered and the mint arm's over " +
			"`xwing.Seed()`'s own. Two defers, one site, because this census keys on the call as " +
			"written; the gate that tells them apart is the one that counts covers per BINDING.",
	},
	"NewDevice|literal Device.wrapSeed": {
		carries: []string{"wrapSeed"},
		why: "THE FIELD. It is HELD rather than copied, which is the erase talking: both of " +
			"deviceIdentity's arms hand back an array nothing else references, so holding it leaves " +
			"ONE array for [Device.Close] to clear where copying would leave the original behind " +
			"with nothing pointing at it.",
	},
	"NewDevice|literal Device.leafKeys": {
		carries: []string{"leafKeys"},
		why:     "the leaf keys body, the PUBLIC half's carrier, tainted by the binding beside the seed.",
	},
	"NewDevice|literal Device.identityPub": {
		carries: []string{"signerPub"},
		why:     "the credential identity, a copy of the signer's public half.",
	},
	"NewDevice|call append": {
		carries: []string{"signerPub"},
		why:     "that copy being taken.",
	},
	"NewDevice|call mls.BasicCredential": {
		carries: []string{"signerPub"},
		why:     "the public half becoming the credential the engine publishes.",
	},
	"NewDevice|call messagegroup.NewConnectMlsEngine": {
		carries: []string{"leafKeys", "signer", "signerPub"},
		why: "the signature private key going into the engine, which is where this device's signer " +
			"LIVES -- [Device] holds no field for it, and the seed is the only key material this " +
			"type holds in a field of its own. The seed is NOT at this call and must never be: the " +
			"engine is `connect`'s and has no door for it.",
	},
	"NewDevice|literal Device.engine": {
		carries: []string{"engine"},
		why: "the engine FIELD, and it is here because the over-approximation is right about it: " +
			"`engine` was bound from a call handed `signer`, and the engine does hold this device's " +
			"signature private key for the rest of the process. It does not hold the seed, which is " +
			"why the two `connect` erase gates cover the one and this package's Close covers the " +
			"other. An entry listing `wrapSeed` here would be a different device.",
	},

	// ── THE STORE'S TWO HALVES ───────────────────────────────────────────────────────────────
	"PutDeviceIdentity|call self.writeRecord": {
		carries: []string{"wrapSeed"},
		why: "THE WRITE. One record, four parts, one fsync -- see " +
			"TestEveryFsyncInThisPackageIsAtASiteThisSuiteNames for the durability half. IT IS NOT " +
			"THE END OF THE ROAD and this entry used to read as though it were: the three sites " +
			"below are what the seed does AFTER this call, and until they were censused a `%x` of " +
			"it inside either callee was refused by nothing.",
	},

	// ── AND THE TWO FUNCTIONS UNDER THAT CALL ────────────────────────────────────────────────
	"groupRecordOf|literal GroupRecord.GroupId": {
		carries: []string{"parts"},
		why:     "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the six parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is part one, the group id.",
	},
	"groupRecordOf|literal GroupRecord.PqSecret": {
		carries: []string{"parts"},
		why:     "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the six parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is part two, the persisted pq_secret.",
	},
	"groupRecordOf|literal GroupRecord.GroupHandleKey": {
		carries: []string{"parts"},
		why:     "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the six parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is part three, the group_handle_key.",
	},
	"groupRecordOf|call decodeWrapDark": {
		carries: []string{"parts"},
		why:     "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is part seven going to the wrap_dark decoder.",
	},
	"groupRecordOf|assign record.WrapDarkKind": {
		carries: []string{"kind"},
		why:     "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is that decoder's kind octet landing on the record.",
	},
	"groupRecordOf|assign record.WrapDarkEpoch": {
		carries: []string{"epoch"},
		why:     "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is the epoch beside it.",
	},
	"groupRecordOf|literal GroupRecord.Epoch": {
		carries: []string{"parts"},
		why:     "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the six parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is part four, an epoch.",
	},
	"groupRecordOf|literal GroupRecord.Opened": {
		carries: []string{"parts"},
		why:     "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the six parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is part five, a flag.",
	},
	"groupRecordOf|call binary.BigEndian.Uint64": {
		carries: []string{"parts"},
		why:     "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the six parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is part four being read as a number.",
	},
	"groupRecordOf|call decodePqSecretTable": {
		carries: []string{"parts"},
		why:     "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the six parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is part six going to the table decoder.",
	},
	"groupRecordOf|assign record.PqSecrets": {
		carries: []string{"table"},
		why:     "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the six parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is that decoded table landing on the record.",
	},
	"groupRecordOf|call decodePqSecretWitness": {
		carries: []string{"parts"},
		why:     "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the parts of a group record are read into a [GroupRecord] and nowhere else. Here it is part eight going to the pq_secret witness decoder, which reads digests and no key material of any kind.",
	},
	"groupRecordOf|assign record.PqSecretWitness": {
		carries: []string{"witness"},
		why:     "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the parts of a group record are read into a [GroupRecord] and nowhere else. Here it is that decoded witness landing on the record: 32 octets of SHA-256 per epoch, which are a digest of a pq_secret and are never a seed.",
	},
	"groupRecordOf|call decodeLeafOccupancy": {
		carries: []string{"parts"},
		why: "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the " +
			"net above seeds ANY parameter called `parts`. Here it is part NINE going to the leaf " +
			"ledger decoder, which reads leaf indices, epochs and a flag octet, and no key material " +
			"of any kind.",
	},
	"groupRecordOf|assign record.Leaves": {
		carries: []string{"ledger"},
		why:     "that decoded leaf ledger landing on the record.",
	},
	"groupRecordOf|call decodeRemoval": {
		carries: []string{"parts"},
		why: "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the " +
			"net above seeds ANY parameter called `parts`. Here it is part TEN going to ruling 52's " +
			"removal decoder, which reads one kind octet and one epoch and no key material of any " +
			"kind.",
	},
	"groupRecordOf|assign record.RemovedKind": {
		carries: []string{"kind"},
		why:     "that decoder's kind octet landing on the record: removedNone or removedByCommit.",
	},
	"groupRecordOf|assign record.RemovedEpoch": {
		carries: []string{"epoch"},
		why:     "the epoch beside it -- the last one this device was a member at.",
	},
	"groupRecordOf|return": {
		carries: []string{"record"},
		why:     "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the six parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is the whole record, or one of two arity refusals that format the record's directory name and two counts.",
	},
	"writeRecord|call encodeStateRecord": {
		carries: []string{"parts"},
		why: "the four record parts -- signature public half, signature private half, leaf keys " +
			"body, SEED -- being handed to the framing. `parts...` is a variadic forward and the " +
			"walk reads it as the value it is: the same arrays the caller passed, not a copy.",
	},
	"writeRecord|call temp.Write": {
		carries: []string{"record"},
		why: "THE SEED REACHING THE FILE, which is the sentence the entry above used to carry on " +
			"its own. `record` is tainted by DERIVATION -- it was bound from a call handed `parts` " +
			"-- and by derivation is the right answer rather than an over-approximation here: the " +
			"framed record literally contains the seed's octets, length-prefixed. It goes to the " +
			"disk in the clear, which is S2-24 and is still open; this is now the site where that " +
			"fact is held rather than one call upstream of it.",
	},
	"writeRecord|call zeroizeState": {
		carries: []string{"record"},
		why: "THE SECOND COPY BEING ERASED. `encodeStateRecord` assembles a record that is another " +
			"copy of whatever secret it carries, and this deferred erase is the only thing that can " +
			"still reach it once the write has returned. It is [zeroizeState] at a site this census " +
			"had never seen, and an entry that listed `parts` here would be a different function: " +
			"the CALLER's arrays are not this erase's to clear.",
	},
	"encodeStateRecord|call body.Write": {
		carries: []string{"part"},
		why: "THE SEED'S OWN ARRAY, one part at a time, going into the buffer the frame is built " +
			"in. `part` is the range variable over `parts`, so on the identity record's fourth " +
			"turn of that loop this IS the seed. See [wrapSeedAccumulatorSites] for where the walk " +
			"stops after this call and why that limit is asserted rather than described.",
	},
	"PutDeviceIdentity|return": {
		carries: []string{"wrapSeed"},
		why: "the SAME statement as the write above, seen by the return clause as well, because " +
			"`return self.writeRecord(…, wrapSeed)` both passes the seed and hands a value back. " +
			"What is handed back is an `error`; the seed is borne by the call's ARGUMENTS and the " +
			"walk does not separate an expression's arguments from its result. Two clauses reporting " +
			"one statement is the correct answer here and not a duplicate: the day the write moves " +
			"off the return, one of the two entries goes stale and this file says so.",
	},
	"GetDeviceIdentity|return": {
		carries: []string{"parts"},
		why: "THE READ, all five of its returns at once. A three part record answers `parts[0], " +
			"parts[1], parts[2], nil, nil` -- a nil error and an EMPTY seed -- and that is the " +
			"backward-compatibility ruling this package's version lever could not pay for: " +
			"[stateRecordVersion] is read for EVERY record in the directory, so spending it here " +
			"would refuse the group states and the key packages beside the identity, which is a " +
			"device that can never start again. A four part record whose fourth part is not a seed " +
			"is refused instead, at this same read.",
	},

	// ── THE USE, AND THE ERASE ───────────────────────────────────────────────────────────────
	"DecapsulateToOwnLeaf|call messagegroup.XwingKeyGenFromSeed": {
		carries: []string{"self.wrapSeed"},
		why: "THE SEED BEING SPENT FOR WHAT IT IS FOR: re-expanded into the pair whose public half " +
			"this device's leaf publishes. It is re-expanded on EVERY call rather than cached, " +
			"because a cached pair would be a `*messagegroup.XwingPrivateKey` in a field -- " +
			"unerasable from this package, and the thing `connect/mls`'s erase gate excuses on the " +
			"written ground that no production declaration holds one.",
	},
	"DecapsulateToOwnLeaf|call messagegroup.XwingDecapsulate": {
		carries: []string{"private"},
		why: "the expanded pair reaching the decapsulation. `private` is tainted because it was " +
			"bound from a call whose argument was the seed -- by DERIVATION, which is the same " +
			"over-approximation the epoch-key gate records for a sealed record, and it is the " +
			"correct answer here: an expanded X-Wing private key IS the seed's content.",
	},
	"DecapsulateToOwnLeaf|return": {
		carries: []string{"shared"},
		why: "THE SHARED SECRET LEAVING THIS PACKAGE, which is this method's whole purpose and is " +
			"the one place the over-approximation reaches a value that is deliberately handed out. " +
			"It is tainted by derivation from the private key; what it is is " +
			"[messagegroup.XwingSharedSize] octets of ML-KEM/X25519 output, and the caller is the " +
			"party the ciphertext was addressed to. The SEED is not at this return and an entry " +
			"that listed it would be a different method.",
	},
	"openWrapToOwnLeaf|call messagegroup.XwingKeyGenFromSeed": {
		carries: []string{"self.wrapSeed"},
		why: "THE SAME SPEND, FOR THE WRAP DOOR. Ledger item 243's receive leg opens a device wrap " +
			"addressed to this leaf, and `messagegroup.OpenWrapBody` takes the private key rather " +
			"than a shared secret -- because the KEM's answer is not the answer: ML-KEM-768 uses " +
			"implicit rejection, so a ciphertext for another leaf decapsulates SUCCESSFULLY, and " +
			"everything that separates mine from not-mine (the envelope comparison, then the " +
			"Poly1305 tag) lives above the KEM inside that call. Re-expanded on every call for " +
			"DecapsulateToOwnLeaf's reason, unchanged.",
	},
	"openWrapToOwnLeaf|call messagegroup.OpenWrapBody": {
		carries: []string{"private"},
		why: "the expanded pair reaching the wrap door. `private` is tainted by DERIVATION exactly " +
			"as it is at the decapsulation site above, and correctly: an expanded X-Wing private " +
			"key IS the seed's content. The other six arguments are wire values off the record -- " +
			"the group id, the epoch, the two type octets, the wrap_target_handle and the body -- " +
			"and none of them is tainted, which is what this entry's carries list says.",
	},
	"openWrapToOwnLeaf|return": {
		carries: []string{"private"},
		why: "THE WRAP'S ENVELOPE AND PAYLOAD LEAVING THIS METHOD, and `private` is listed because " +
			"the census reads the whole return statement and the expression mentions it. What " +
			"crosses this boundary is OpenWrapBody's two results: an eleven-octet cleartext " +
			"envelope, and the pq_secret the wrap carried -- which is key material, is the caller's " +
			"to file in [Group.pqSecrets], and is erased by that table's own discipline. THE SEED " +
			"IS NOT AT THIS RETURN and an entry that listed it would be a different method.",
	},
	"Close|call zeroizeState": {
		carries: []string{"self.wrapSeed"},
		why: "THE ERASE. [zeroizeState] overwrites the one array [NewDevice] held, IN PLACE, under " +
			"the mutex a decapsulation also takes; the field is nil'd after, so a second Close " +
			"erases nothing and a decapsulation after a Close refuses by name rather than " +
			"decapsulating under 32 zero octets -- a well formed seed that would answer a perfectly " +
			"uniform-looking wrong secret. See TestClosingADeviceErasesTheSeedAndNotOnlyTheField. " +
			"It does NOT reach the copy inside the transient *messagegroup.XwingPrivateKey, which " +
			"declares no erase and which `connect` is not this step's to change.",
	},
}

// THE NARROWING'S OWN CENSUS: every place this package takes a COUNT of the seed and this gate
// therefore declined to call a carried value. Held both ways, exactly like the three above.
//
// THIS IS THE COMPLEMENT OF [censusBorneBy]'s ONE EXCLUSION, WRITTEN DOWN AND ASSERTED. A
// narrowing that is merely printed tells a reader what was removed; only an assertion tells the
// NEXT COMMIT that it may not narrow further. An entry here with no site is the tree having
// stopped counting the seed where it used to. That is sometimes a fix nobody deleted the entry
// for, and sometimes it is the exclusion having been switched OFF: M8 in the table at the foot of
// this file declares a local `len`, shadowing the builtin from inside the source this gate reads,
// and both this census's stale direction and the sink disposition go red at that one commit.
// A site here with no entry is a new count nobody weighed, and a count is one edit from a value --
// M9 drives that direction and M3 drives the edit.
var wrapSeedCountedNotCarriedSites = map[string]string{
	"PutDeviceIdentity|len wrapSeed": "the write path's length refusal: 32 and 64 both expand " +
		"into a well formed X-Wing pair and only one of them is the pair whose public half is in " +
		"leafKeys, so the length is checked at the one moment the value is still in the caller's " +
		"hand. The error formats this COUNT and must never format the value.",
	"GetDeviceIdentity|len parts": "the read path's arity switch and its refusal text: three parts " +
		"or four, and anything else is a record this build cannot read.",
	"GetDeviceIdentity|len parts[3]": "the read path's own length check on the STORED seed, and " +
		"its refusal text. It is a site of its own rather than part of the entry above because " +
		"this census spells the index: `len(parts)` counts the record and `len(parts[3])` counts " +
		"the key inside it, and a disposition that called those one site would excuse the second " +
		"by having weighed the first.",
	"DecapsulateToOwnLeaf|len self.wrapSeed": "the EMPTY-seed guard, which is a store written " +
		"before the seed was retained or a device that has been Closed. It refuses by name " +
		"([ErrNoDeviceWrapKey]) rather than expanding zero octets into a valid-looking key.",
	"openWrapToOwnLeaf|len self.wrapSeed": "the SAME empty-seed guard on the wrap door, and it is " +
		"a site of its own rather than a shared one because this census keys on the enclosing " +
		"function: a door that stopped making the check would leave the other door's entry still " +
		"describing the tree. It refuses by the same name for the same reason -- 32 zero octets " +
		"expand into a well formed pair that opens nothing and reports nothing.",
	"groupRecordOf|len parts":    "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the six parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is the group record's arity switch: five parts or six, and the refusals beside them format those COUNTS.",
	"groupRecordOf|len parts[3]": "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the six parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is its epoch part's width, as a site of its own because this census spells the index, and the refusals beside them format those COUNTS.",
	"groupRecordOf|len parts[4]": "THE GROUP RECORD'S DECODE, WHICH HOLDS NO SEED, and it is in this census because the net above seeds ANY parameter called `parts` -- the widening that reached the disk. This gate cannot tell one record kind from another and does not claim to: what it holds here is that the six parts of a group record are read into a [GroupRecord] and nowhere else. The value that IS this function's subject is censused by pqsecretgate_test.go, which is the gate for it. Here it is its opened flag's width, likewise, and the refusals beside them format those COUNTS.",
	"encodeStateRecord|len parts": "the framing's own arity: the 255 refusal, and the part count " +
		"written into the frame's header as one byte. It counts the RECORD's parts and not the " +
		"seed's octets, and the refusal beside it formats that count.",
	"encodeStateRecord|len part": "the framing's per-part width: the length that does not fit a " +
		"uint32 prefix, and the prefix itself. On the identity record's fourth part this is the " +
		"length of the seed, and the refusal beside it formats that length. It is the site M12 " +
		"drives: one edit turns it into the value.",
}

// THE WALK'S OWN HORIZON: every call at which the seed enters an OBJECT this census does not
// follow it into. Held both ways, exactly like the four above.
//
// WHY IT IS A CENSUS AND NOT A PARAGRAPH. This file's net now reaches the last Go value the seed
// is before it becomes octets in a frame -- but a taint walk over names stops where a value is
// handed to a method and lives on inside the receiver. Two such calls exist and both are real:
// `body.Write(part)` puts the seed inside a *bytes.Buffer, and `temp.Write(record)` puts the
// framed record inside an *os.File. After each of them the walk knows nothing, so
// `fmt.Errorf("%x", body.Bytes())` INSIDE encodeStateRecord is a shape this gate would not refuse.
// That is a limit, it is stated here rather than in a comment nothing checks, and it is ASSERTED:
// a third accumulator appearing -- the seed written into a second buffer, a hasher, a writer -- is
// a site with no entry and this gate goes red at it. M13 drives exactly that.
//
// THE THREE CLAUSES THAT NARROW IT, each for its own reason:
//
//   - THE RECEIVER MUST NOT BE A PACKAGE. `fmt.Errorf(…, wrapSeed)` and
//     `messagegroup.XwingKeyGenFromSeed(self.wrapSeed)` are calls through a qualified name and
//     not methods on a value, and calling them accumulators would put every leak shape this file
//     exists to refuse into a permitted list. The import set of each file is read for this, so
//     the test is "is this identifier one of THIS FILE's imports" and not a guess at a spelling.
//   - THE CALLEE MUST NOT RE-SEED. `self.writeRecord(…, wrapSeed)` and
//     `store.PutDeviceIdentity(…, wrapSeed)` are calls into functions whose own parameters are in
//     [wrapSeedProducerNames], so the walk does NOT stop there -- it starts again inside them,
//     which is the whole of this commit's repair. A site listed here that re-seeds would be a
//     limit claimed where there is none.
//   - THE RECEIVER MUST BE UNTAINTED. A method on a value the walk already follows is not a
//     horizon; it is a sink, and it is censused as one above.
//
// A BARE CALL IS NOT AN ACCUMULATOR. `zeroizeState(self.wrapSeed)` has no receiver to accumulate
// into and its argument is censused as a sink like every other call's.
var wrapSeedAccumulatorSites = map[string]string{
	"writeRecord|accumulate temp.Write": "THE DISK. The framed record -- the seed inside it -- " +
		"goes into the *os.File this write is committing, and what happens to those octets after " +
		"this call is the filesystem's and not a value any walk over this package's names can " +
		"follow. It is the honest end of this census: the seed is on disk in the clear, S2-24.",
	"encodeStateRecord|accumulate body.Write": "THE FRAME. Each part, the seed among them, goes " +
		"into the *bytes.Buffer the record is assembled in, and the walk does not follow it into " +
		"the buffer -- so `body.Bytes()` reads back a value this census does not know carries the " +
		"seed. It is the one blind spelling left on the road to the disk, and naming it here is " +
		"what stops the next reader assuming the road is fully censused. The value comes back out " +
		"as `writeRecord`'s `record`, which IS censused, so the blindness is bounded by this one " +
		"function's body.",
}

func TestEveryWrapSeedInThisPackageGoesWhereTheDispositionSaysItGoes(t *testing.T) {
	// THE WALK IS [runCensus] AND THE NET IS THIS FILE'S. Until this commit the four-producer
	// fixpoint below lived in this function's body; the third census in this package would have
	// been a third copy of it, so it moved to census_test.go and this gate now asks for the
	// same five censuses by name. Nothing in the walk changed but the producer net becoming an
	// argument -- which rows M3, M5, M6 and M9 of the mutation table at the foot of this file
	// were re-run to hold, after the move, against the same failure texts.
	census := runCensus(t, wrapSeedProducerNames)
	sources := census.sources
	producers := census.producers
	sinks := census.sinks
	carried := census.carried
	counted := census.counted
	accumulated := census.accumulated
	unspent := census.unspent

	// ── THE COMPLEMENT, PRINTED: what this search covered and what it left out ────────────────
	t.Logf("production sources read (%d): %v", len(sources), sources)
	t.Logf("producer net (a name whose call, field read or parameter is taken to be the seed): %v",
		epochKeySortedKeys(wrapSeedProducerNames))
	t.Logf("producer sites found (%d):", len(producers))
	for _, site := range epochKeySortedMap(producers) {
		t.Logf("    %s  at %v", site, producers[site])
	}
	t.Logf("sink sites found (%d), each with THE VALUES IT CARRIES, which is what the disposition "+
		"is held against a second time:", len(sinks))
	for _, site := range epochKeySortedMap(sinks) {
		t.Logf("    %s  carries %v  at %v", site, epochKeySortedKeys(carried[site]), sinks[site])
	}
	t.Logf("COUNTED AND NOT CARRIED (%d) -- the sites the one narrowing removed, which are "+
		"asserted below and not merely printed:", len(counted))
	for _, site := range epochKeySortedMap(counted) {
		t.Logf("    %s  at %v", site, counted[site])
	}
	t.Logf("THE WALK'S HORIZON (%d) -- the calls at which a seed-bearing value enters an object "+
		"this census does not follow it into, which is where this gate stops knowing:", len(accumulated))
	for _, site := range epochKeySortedMap(accumulated) {
		t.Logf("    %s  at %v", site, accumulated[site])
	}
	t.Logf("EXCLUDED, and excluded is not the same as absent -- tainted values that reach no sink "+
		"in their own function, so nothing carried them anywhere: %v", unspent)

	// ── AND ASSERTED, IN BOTH DIRECTIONS, AGAINST THREE WRITTEN-DOWN DISPOSITIONS ─────────────
	censusHold(t, "producer site", producers, wrapSeedProducerSites,
		"A name that produces this device's X-Wing seed is where this gate's whole search begins. "+
			"A site with no entry is a seed coming from somewhere nobody weighed; an entry with no "+
			"site is this gate having gone BLIND -- the producer was respelled, and the sink "+
			"clauses are now searching a value nothing tainted, which passes by finding nothing.")

	sinkWhy := map[string]string{}
	for site, entry := range wrapSeedSinks {
		sinkWhy[site] = entry.why
	}
	sinkNarrowing := "The device's X-Wing decapsulation seed is a PRIVATE KEY, and the shape this " +
		"gate exists to refuse is the one the epoch-key gate names in its own header: " +
		"`fmt.Errorf(\"%x\", seed)` -- a private key in an error string, in every log that error " +
		"ever reaches. It was MEASURED possible on the commit before this file: two such sites were " +
		"added to device.go and the whole ./urmessage suite stayed green. A site with no entry is a " +
		"seed landing somewhere nobody weighed, and `BlobId: wrapSeed[:]` fails here exactly as " +
		"`fmt.Errorf(\"%x\", wrapSeed)` does. An entry with no site is a disposition that has " +
		"stopped describing the code."
	censusHold(t, "sink site", sinks, sinkWhy, sinkNarrowing)

	// ── THE SECOND NARROWING: NOT ONLY WHERE, BUT WHICH VALUE ─────────────────────────────────
	for _, site := range epochKeySortedMap(sinks) {
		entry, dispositioned := wrapSeedSinks[site]
		if !dispositioned {
			continue // already refused above, by name
		}
		allowed := map[string]bool{}
		for _, spelled := range entry.carries {
			allowed[spelled] = true
		}
		for _, spelled := range epochKeySortedKeys(carried[site]) {
			if !allowed[spelled] {
				t.Errorf("sink site %q carries %q and its disposition entry does not list it "+
					"(it lists %v).\n%s", site, spelled, entry.carries, sinkNarrowing)
			}
		}
		for _, spelled := range entry.carries {
			if !carried[site][spelled] {
				t.Errorf("the disposition says sink site %q carries %q and the census finds only "+
					"%v there. A value that has stopped arriving is either a fix nobody deleted "+
					"the entry for or a rename this gate is now blind to.\n%s",
					site, spelled, epochKeySortedKeys(carried[site]), sinkNarrowing)
			}
		}
	}

	// ── AND THE ONE NARROWING'S OWN COMPLEMENT, ASSERTED THE SAME WAY ─────────────────────────
	censusHold(t, "counted-not-carried site", counted, wrapSeedCountedNotCarriedSites,
		"A COUNT OF THE SEED IS NOT THE SEED -- `len(wrapSeed)` is an int -- and that single "+
			"exclusion is what keeps the two store-side refusals OUT of the sink disposition, so "+
			"that an entry for them can never become a standing permit to format the value itself "+
			"at the same call. The exclusion is therefore a narrowing, and this is the assertion "+
			"that holds it: every site it removed is named here. A site with no entry is a count "+
			"nobody weighed; an entry with no site means the tree stopped counting the seed there, "+
			"which is a change this file has to be read against before it is deleted.")

	// ── AND WHERE THE CENSUS ENDS, ASSERTED RATHER THAN LEFT TO BE INFERRED ───────────────────
	censusHold(t, "accumulator site", accumulated, wrapSeedAccumulatorSites,
		"A TAINT WALK OVER NAMES STOPS WHERE A VALUE IS HANDED TO A METHOD AND LIVES ON INSIDE THE "+
			"RECEIVER, and this census is that stopping place written down. A site with no entry is "+
			"a NEW blind spot -- the seed put into a buffer, a hasher or a writer nobody weighed -- "+
			"and the sink clause above will have censused the call while knowing nothing about what "+
			"the object does with it afterwards. An entry with no site is a limit that has been "+
			"lifted or moved, and this file's claim about how far it reaches has to be re-read "+
			"before the entry is deleted.")
}

// ══════════════════════════════════════════════════════════════════════════════════════════════
// THE MUTATION TABLE, MEASURED
// ══════════════════════════════════════════════════════════════════════════════════════════════
//
// M1-M9 WERE RUN AGAINST THE FIRST VERSION OF THIS FILE AND M10-M15 AGAINST THE SECOND, the one
// whose net reaches the disk. They are one table because they are one gate; see the second block
// below for what the six later rows vary and why the first nine could not have caught them.
//
// NINE MUTANTS, AND EVERY ONE VARIES A DIFFERENT MECHANISM. That is the whole discipline of this
// table and it is the one four gates in this track were beaten for want of: a table whose every
// entry varies the same attribute -- a different destination for the same bare identifier --
// cannot see a change of mechanism, and a change of mechanism is what defeats a gate. So M1 and M2
// vary the PRODUCER the taint starts at, M3 and M8 come at the one narrowing from its two opposite
// sides, M4 is the source file the net could most easily not reach, M5 varies the SPELLING of the
// value, M6 varies the BINDING FORM, M7 does not attack the gate at all but AVOIDS it, and M9 is
// the second direction of the narrowing's own census. A SURVIVING MUTANT IS FIRST A CLAIM ABOUT
// THE QUERY, so each row below names the failure text it produced rather than the word "killed".
//
// Applied one at a time with a python edit that ASSERTS count == 1, and reverted by a byte copy of
// a snapshot taken before the run whose sha256 is verified after: no `git checkout --`, no stash.
// The clean tree was re-run as a control after every single row and answered
// `ok github.com/urnetwork/sdk/urmessage` each time.
//
//	M1  the finding's own mutant A: fmt.Errorf("…(seed %x)…", wrapSeed, err) at deviceIdentity's
//	    persist failure -- the taint started at a PRODUCER CALL
//	    -> sink site "deviceIdentity|call fmt.Errorf" has no entry in the disposition
//	M2  the finding's own mutant B: the same leak at DecapsulateToOwnLeaf, the taint started at a
//	    FIELD READ in a different function
//	    -> sink site "DecapsulateToOwnLeaf|call fmt.Errorf" has no entry in the disposition
//	    -> AND sink site "DecapsulateToOwnLeaf|return" carries "self.wrapSeed" and its disposition
//	       entry does not list it (it lists [shared])  -- two clauses, independently
//	M3  the ONE NARROWING, from the side that would make it a permit: `len(wrapSeed)` -> `wrapSeed`
//	    inside PutDeviceIdentity's own length refusal, the exact call a length-carrying census
//	    would have had to excuse
//	    -> sink site "PutDeviceIdentity|call fmt.Errorf" has no entry in the disposition
//	    (the counted-not-carried entry does NOT go stale here, and that is correct rather than a
//	    miss: the `if len(wrapSeed) != messagegroup.XwingSeedSize` guard that this error belongs to
//	    is still a count of the seed in the same function, so the site is still occupied. M8 is the
//	    row that drives that entry's stale direction, and M9 its unentered direction.)
//	M4  the STORE'S READ PATH: `len(parts[3])` -> `parts[3]` in GetDeviceIdentity's refusal, where
//	    the seed has no name, no producer call and no field read, and only the producer-function
//	    result seeding reaches it at all
//	    -> sink site "GetDeviceIdentity|call fmt.Errorf" has no entry in the disposition
//	M5  the SPELLING: `identityPub: append([]byte(nil), signerPub...)` -> `identityPub: wrapSeed[:]`
//	    -- the 4fde7ad scar, one character, an *ast.SliceExpr where the census used to want an
//	    *ast.Ident
//	    -> "NewDevice|literal Device.identityPub" carries "wrapSeed" and its entry does not list it
//	    -> AND the entry says it carries "signerPub" and the census finds only [wrapSeed] there
//	M6  the BINDING FORM: `var parked = self.wrapSeed` before the erase and `self.wrapSeed = parked`
//	    after it -- the erase performed and then undone, bound with the keyword that bound nothing
//	    when the fixpoint read *ast.AssignStmt alone
//	    -> sink site "Close|assign self.wrapSeed" has no entry in the disposition
//	M7  A CHANGE OF MECHANISM RATHER THAN AN ATTACK: `xwing.Seed()` moved behind a helper
//	    `wrapSeedOf`, which is how a gate is AVOIDED. Both directions of the producer hold fire:
//	    -> producer site "wrapSeedOf|key.Seed" has no entry in the disposition
//	    -> AND the disposition says producer site "deviceIdentity|xwing.Seed" is allowed and the
//	       census does not find it  -- the gate reporting that it has gone blind
//	    -> AND sink site "wrapSeedOf|return" has no entry in the disposition
//	M8  the NARROWING'S OWN GUARD: a local `len` declared in DecapsulateToOwnLeaf, shadowing the
//	    builtin from inside the source this gate inspects
//	    -> sink site "DecapsulateToOwnLeaf|call len" has no entry in the disposition
//	    -> AND the disposition says counted-not-carried site "DecapsulateToOwnLeaf|len
//	       self.wrapSeed" is allowed and the census does not find it
//	M9  the COUNTED CENSUS's second direction: `_ = len(wrapSeed)` added to NewDevice, a function
//	    that counts the seed nowhere today
//	    -> counted-not-carried site "NewDevice|len wrapSeed" has no entry in the disposition
//
// AND THE ONE MEASUREMENT THAT IS THE POINT OF THE FILE. M1 and M2 applied TOGETHER, which is the
// finding exactly as it was reproduced, under the whole package rather than this gate by name:
//
//	before this file:  go test ./urmessage/ -run '.*' -timeout 1800s  ->  ok   … 6.147s
//	after  this file:  go test ./urmessage/ -run '.*' -timeout 1800s  ->  FAIL … 6.118s
//	                   naming both sites, "deviceIdentity|call fmt.Errorf" and
//	                   "DecapsulateToOwnLeaf|call fmt.Errorf"
//
// THERE IS NO LEAK IN THE TREE THIS FILE LANDS ON and this file does not claim to have found one.
// What it claims is the narrower and checkable thing: the leak was possible, it was measured
// possible, and it is now refused.
//
// ══════════════════════════════════════════════════════════════════════════════════════════════
// AND SIX MORE, FOR THE NET THAT NOW REACHES THE DISK
// ══════════════════════════════════════════════════════════════════════════════════════════════
//
// THE FINDING THE SECOND PASS TOOK, and it is this file's own disposition having overclaimed. The
// entry at `PutDeviceIdentity|call self.writeRecord` read "THE SEED GOING TO DISK" -- and
// `writeRecord` and `encodeStateRecord` were outside the census, so the seed's last two hops were
// censused by nothing at all. The two mutants that reproduced it are M10 and M11 and both SURVIVED
// the unrepaired gate:
//
//	before the repair:  go test ./urmessage/ -run '.*' -timeout 1800s          ->  ok  … 6.117s
//	                    …-run 'TestEveryWrapSeedInThisPackage…' -v             ->  --- PASS (0.01s)
//	after  the repair:  each of M10, M11 run alone                             ->  --- FAIL
//
// WHAT THE REPAIR WAS: one name. `parts` joins [wrapSeedProducerNames], and PRODUCER ONE -- the
// clause that put `statestore_durable.go` inside this census at all, by seeding the PARAMETER
// `PutDeviceIdentity(…, wrapSeed []byte)` -- seeds `writeRecord(…, parts …[]byte)` and
// `encodeStateRecord(…, parts …[]byte)` the same way. The mechanism was already here; what was
// missing was the second name on the road.
//
// M10-M15 ARE SIX MECHANISMS AGAIN AND NOT SIX DESTINATIONS. M10 and M11 are the finding, at the
// two ends of the new reach and by two different producer paths (a DERIVED value in one, a RANGE
// VARIABLE off the parameter in the other). M12 drives the narrowing's census inside the new
// region. M13 introduces a SECOND OBJECT the seed enters, which is the shape the horizon census
// exists for. M14 does not attack the gate at all -- it changes the WRITE'S API, which is how a
// road moves out from under a disposition. M15 attacks the re-seeding clause itself, which is the
// one thing holding the whole widening up.
//
//	M10 a `%x` of the assembled record at writeRecord's write failure -- the taint reached it by
//	    DERIVATION, `record` having been bound from a call handed `parts`
//	    -> sink site "writeRecord|call fmt.Errorf" has no entry in the disposition
//	    -> AND sink site "writeRecord|return" has no entry  -- two clauses, independently
//	M11 a `%x` of one part at encodeStateRecord's width refusal -- the taint reached it through a
//	    RANGE BINDING off the parameter, and on the identity record that part IS the seed
//	    -> sink site "encodeStateRecord|call fmt.Errorf" has no entry in the disposition
//	    -> AND sink site "encodeStateRecord|return" has no entry
//	M12 the NARROWING inside the newly reached region: `_ = len(record)` added to writeRecord, a
//	    function that counts nothing today
//	    -> counted-not-carried site "writeRecord|len record" has no entry in the disposition
//	M13 A SECOND ACCUMULATOR: `body.Write(part)` routed through a `spare := bytes.NewBuffer(nil)`.
//	    This is the shape the horizon census is for -- the seed entering an object nobody weighed
//	    -> accumulator site "encodeStateRecord|accumulate spare.Write" has no entry
//	    -> AND the disposition says accumulator site "encodeStateRecord|accumulate body.Write" is
//	       allowed and the census does not find it
//	    -> AND both directions again at the SINK clause, for the same two spellings
//	M14 A CHANGE OF API RATHER THAN AN ATTACK: `temp.Write(record)` replaced by
//	    `os.WriteFile(tempPath, record, 0o600)`, which is how the road moves out from under an entry
//	    -> sink site "writeRecord|call os.WriteFile" has no entry in the disposition
//	    -> AND the disposition says sink site "writeRecord|call temp.Write" is allowed and the
//	       census does not find it
//	    -> AND the same stale report from the horizon census
//	M15 THE RE-SEEDING CLAUSE ITSELF: `writeRecord`'s parameter renamed `parts` -> `values`, which
//	    is the whole widening switched off from inside the source this gate reads. Every direction
//	    fires at once, which is the gate reporting that it has gone blind:
//	    -> the disposition says producer site "writeRecord|parameter parts" is allowed and the
//	       census does not find it
//	    -> AND its three sinks all go stale -- "call encodeStateRecord", "call temp.Write",
//	       "call zeroizeState"
//	    -> AND, because `writeRecord` has left [censusReseedingFunctions], accumulator site
//	       "PutDeviceIdentity|accumulate self.writeRecord" has no entry -- the census correctly
//	       reporting that the walk now STOPS at the call it used to walk through
//
// Applied one at a time by the same python edit that ASSERTS count == 1, reverted by a byte copy
// of a snapshot taken before the run, sha256 verified equal after every single revert
// (`ba343cf4b332595bbafa3192e8d4e97f0fff2b6af4660d7f6eec35702be77368` for statestore_durable.go).
// No `git checkout --`, no stash.
