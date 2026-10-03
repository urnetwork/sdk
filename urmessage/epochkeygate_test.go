package urmessage

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"testing"
)

// ══════════════════════════════════════════════════════════════════════════════════════════════
// RULING 33's GATE: AN EPOCH KEY GOES WHERE THE DISPOSITION BELOW SAYS IT GOES, AND NOWHERE ELSE
// ══════════════════════════════════════════════════════════════════════════════════════════════
//
// connect holds the DESCRIPTOR half of ruling 33 -- no server→client message transitively carries
// an EpochKeyDelivery, and `protocol.Record` declares no key field. That is a property of
// message.proto and it is held in message_keydelivery_test.go. It says nothing about what THIS
// package does with the two keys it derives, and the two halves fail differently: connect's catches
// a key field appearing on a served type, this one catches a key VALUE reaching a site nobody
// weighed.
//
// THE MECHANISM IS THE VALUE AND NOT THE FIELD NAME, and that is deliberate. Step 3's version of
// this gate in connect was defeated three times, and the last of the three is the one worth
// carrying over: a nested type carrying the two keys under the field names `wk` and `rk` walked
// through every check green, because every check -- and every mutant in that commit's table -- was
// keyed on the strings "write_key" and "read_key". So this gate never reads a field name to decide
// whether something is a key. It starts at the six sites that PRODUCE one, follows the value
// through every binding form the language has, and censuses every place the value lands.
// `BlobId: writeKey` is caught by the same clause `WriteKey: writeKey` is, and the mutation table
// below drives exactly that.
//
// AND THE VERSION THAT SENTENCE FIRST SHIPPED IN WAS DEFEATED THE SAME WAY, ONE LEVEL UP. At
// 4fde7ad the disposition was asserted in both directions and the CENSUS THAT FED IT could not see
// the site: every clause asked its landing place for an *ast.Ident, so `x = writeKey` was censused
// and `x = writeKey[:]` was not censused at all -- not refused, not printed, absent. Measured on
// that commit: `request.ReqAuth = readKey[:]`, between authorizeFetch and transport.Call, left
// this gate at `ok 0.099s` and the whole package at `ok 7.070s`, with the epoch read key riding
// out on FetchRequest.req_auth on every page that left the device. `var leaked = writeKey` did the
// same, because the taint fixpoint read *ast.AssignStmt and nothing else. The table that missed
// both had two rows for "a key onto X" and both VARIED THE DESTINATION and spelled the key as a
// bare identifier -- a table varying one attribute cannot see a change of mechanism, which is the
// sentence that commit itself wrote about connect's step-3 gate. The table below varies the
// SPELLING against one fixed destination, and mutates the gate's own clauses besides.
//
// THE NARROWING IS ASSERTED IN BOTH DIRECTIONS AND IS NOT MERELY PRINTED, AND IT IS ASSERTED TWICE
// OVER: a site with no entry in the disposition is a failure, an entry no site needs is a failure,
// a value arriving at a site its entry does not list is a failure, and a value an entry lists that
// no longer arrives is a failure. Printing alone is what connect's step-3 repair did, and item
// 244's own shape rode back in green underneath it: a printed complement tells a reader what was
// narrowed away, and only an assertion tells the NEXT COMMIT that it may not narrow further.

// The names a CALL OR A FIELD READ has to carry for its value to be an epoch key. This is the
// SEARCH NET rather than a disposition: a name here that no site uses costs nothing and widens the
// net, while a producer spelled some other way is a blindness this gate cannot see -- which is
// what epochKeyProducerSites is held both ways for.
//
// `WriteKey` and `ReadKey` cover both spellings this package uses: the method on
// messagegroup.EpochKeys, which hands back an epoch's own pair, and the package function
// message.WriteKey/message.ReadKey, which derives one off a storage root. `GetWriteKey` and
// `GetReadKey` cover a key read back OFF a protobuf, which is how one would re-enter this package
// from a message it had already been put on -- and the BARE FIELD, `delivery.WriteKey`, is read by
// the same net, because the generated struct offers both and the field is the shorter of the two.
var epochKeyProducerSelectors = map[string]bool{
	"WriteKey":    true,
	"ReadKey":     true,
	"GetWriteKey": true,
	"GetReadKey":  true,
}

// Every call site in this package's production source that produces an epoch key, as
// "<enclosing function>|<callee as written>" -> why it is there.
//
// IT IS HELD BOTH WAYS, and the direction that matters most is the one that looks pedantic: an
// entry no site needs is a FAILURE, because that is how this gate reports that its own search has
// gone blind. Move a derivation into a helper -- `func deriveForMutant(root []byte) []byte { return
// message.WriteKey(root) }`, called from publishCommitLocked -- and this site moves to the helper's
// name: measured, four failures, two of them this map's own directions and two of them sinks that
// stopped carrying `writeKey`. A gate that can go quiet by being avoided is not a gate, so the day
// the census stops finding one of these, this map says so in a FAILURE.
//
// A LOCAL ALIAS, `derive := message.WriteKey`, DOES NOT MOVE A SITE AND DOES NOT BLIND THIS GATE,
// and that is worth writing down because the commit before this one recorded the opposite. The
// bare selector is read by the same net, so the site is still found -- printed with a `field` note
// beside its line -- and `derive` is tainted, so `writeKey` is tainted, so both of its sinks still
// carry it. Measured: green, with the census printing `publishCommitLocked|message.WriteKey`, the
// EpochAttachment.WriteKey literal and the epochKeysFor call, all three unchanged.
var epochKeyProducerSites = map[string]string{
	"Open|keys.WriteKey":                   "epoch 1's write key, off the session the founding commit opens",
	"Open|keys.ReadKey":                    "epoch 1's read key, the same",
	"Open|bootstrap.WriteKey":              "write_key[0], §4.3.2's bootstrap key, which certifies the founding commit and is NOT what it opens",
	"publishCommitLocked|message.WriteKey": "the write key of the epoch the staged commit opens, off the staged exporter",
	"publishCommitLocked|message.ReadKey":  "the read key of the same",
	"matchesEpochDigestLocked|message.WriteKey": "THE RECEIVER'S SIDE OF THE SAME PAIR, derived to " +
		"be COMPARED and never to be used. Ledger item 251's rotation makes a receiver ask which " +
		"pq_secret the epoch it is entering was opened with, and the only authenticated answer is " +
		"H(epoch_keys) inside the commit's own attachment -- so the candidate secret is run through " +
		"the same three steps the committer took and the digest is recomputed. The keys live for the " +
		"length of that one call and are erased inside it; see the sink entries below, which is where " +
		"that claim is held rather than asserted here.",
	"matchesEpochDigestLocked|message.ReadKey": "the read key of the same comparison, for the same " +
		"call's length. Both halves are in the digest's preimage, LP-framed, so a candidate that " +
		"reproduced only one of them would not reproduce the digest.",
	"Receive|next.ReadKey": "the read key §4.3.8's req_auth is computed under, re-derived when the walk crosses an epoch",
	"subscribeLocked|keys.ReadKey": "the read key §4.3.5's subscription is authorized under, by the same " +
		"§4.3.8 req_auth a fetch carries, at op 14. It lives for the one call: keys.Destroy runs when " +
		"subscribeLocked returns.",
}

// epochKeySink is one entry in the disposition below: WHICH VALUES a site may receive, and why.
//
// THE `carries` LIST IS THE SECOND NARROWING AND IT IS WHY THIS IS A STRUCT RATHER THAN A STRING.
// A disposition keyed on the site alone excuses the site FOREVER, whatever turns up there later:
// `Open|call fmt.Errorf` is a real site -- the over-approximation reaches it, see its entry -- and
// a bare entry for it would have excused `fmt.Errorf("%x", writeKey)`, an epoch key in an error
// string, in every log that error ever reaches. So each entry names the exact value spellings it
// weighed, and it is held BOTH WAYS like everything else here: a value this site carries that the
// entry does not list is a refusal, and a value the entry lists that the census does not find
// there is a refusal. The second direction is the one that reports a rename or a deletion.
type epochKeySink struct {
	carries []string
	why     string
}

// Every place an epoch key VALUE lands, as "<enclosing function>|<site>" -> what may land and why.
//
// THREE OF THESE ENTRIES SAY "ITEM 244 IS STILL OPEN HERE", AND THAT IS WHAT A DISPOSITION IS FOR.
// A gate that refused the tree it is committed beside would be deleted within the week; a gate that
// quietly excused it would be the printed-not-asserted defect again. So the two EpochAttachment
// literals and the two sealed records that carry them are written down, by name, with the
// measurement that says why they cannot move yet -- and because the map is held BOTH ways, the day
// they do move this file goes red until somebody deletes the entry and the sentence with it.
var epochKeySinks = map[string]epochKeySink{
	"Open|call epochKeysFor": {
		carries: []string{"founding", "readKey", "writeKey"},
		why: "RULING 33's ROAD, and the DECISION that gates it. epochKeysFor " +
			"reads the sealed record's attachment kind and answers the pair for a kind 0x0005 commit or " +
			"nil for a kind 0x0001 one, then epochKeyDelivery copies. The founding commit IS kind 0x0005, " +
			"so what travels this sink is the pair, and this is the ONLY road it has. `founding` reaches " +
			"here as the thing being ASKED, not as a key: it is tainted by the over-approximation below, " +
			"and TestItem244sKeysAreInTheRequestAndNotInTheRecord measures that its octets hold neither.",
	},
	"publishCommitLocked|call epochKeysFor": {
		carries: []string{"commitRecord", "readKey", "writeKey"},
		why: "RULING 33's ROAD, the other commit path: the pair " +
			"the staged commit opens its epoch with, through the same one decision. `commitRecord` " +
			"reaches here for the same reason `founding` does at the site above -- it is what the kind " +
			"is read OFF -- and it is tainted by the over-approximation and not by holding a key.",
	},

	"Open|literal protocol.CreateGroupRequest.EpochKeys": {
		carries: []string{"delivery"},
		why: "§4.3.2's SINGULAR carrier. The request " +
			"holds exactly one record and it is always a commit, so there is no alignment to compute and " +
			"no list to keep in step. Present iff the initial commit is kind 0x0005, which is §5.4's " +
			"acceptance window and is the server's own rule read off the same octets.",
	},
	"publishCommitLocked|call self.submitLocked": {
		carries: []string{"commitRecord", "delivery"},
		why: "TWO VALUES REACH THIS ONE CALL and they are there " +
			"for opposite reasons. `delivery` is §4.3.3's REPEATED carrier, held against the record by " +
			"alignedEpochKeys in both directions, and it holds the pair because the attachment is kind " +
			"0x0005. `commitRecord` is the RECORD, and its attachment is the digest -- so of the two " +
			"values at this call site, exactly one carries key material and it is the one that goes on " +
			"the request rather than into the store. THE EARLIER VERSION OF THIS ENTRY PREDICTED THAT " +
			"`commitRecord` WOULD STOP BEING TAINTED when the attachment became a digest. MEASURED " +
			"FALSE the moment it did: the taint is an over-approximation -- `commitRecord` is bound " +
			"from a call whose arguments mention the digest, which is bound from a call whose arguments " +
			"mention the keys -- so it is tainted by DERIVATION and not by CONTENT, and no arrangement " +
			"of this analysis will separate the two. That is the whole reason the sentence 'the record " +
			"carries no key' is a MEASUREMENT over the sealed octets and not a clause of this gate.",
	},

	"matchesEpochDigestLocked|call message.EpochKeysDigest": {
		carries: []string{"writeKey", "readKey"},
		why: "THE COMPARISON ITSELF, and it is the one place in this package a pair of epoch keys " +
			"is used for something other than opening or authorising an epoch. It is SHA-256 over " +
			"\"URmessage/v1/epochkeys\" | LP(group_id) | u64(opens_epoch) | LP(write_key) | LP(read_key), " +
			"the same function the server recomputes in §5.1 check 3 -- so what leaves this call is a " +
			"digest and never a key. It is what binds item 132's wrap rows to the epoch marker: a " +
			"candidate pq_secret that reproduces the digest is the secret this epoch was opened with.",
	},
	"matchesEpochDigestLocked|call zeroizeState": {
		carries: []string{"writeKey", "readKey"},
		why: "THE ERASE. Both halves of the pair are function locals with no field to reach them, " +
			"so this call is the only thing that can, and it runs before the answer is returned " +
			"rather than in a defer -- the digest has already been computed by then and a deferred " +
			"erase would leave the pair live across the comparison for no reason. THE THIRD ERASE " +
			"AT THIS SITE IS THE STORAGE ROOT AND IT IS NOT LISTED, because `messagegroup.StorageRoot` " +
			"is not in this gate's producer net: `root` is untainted here and listing it would be an " +
			"entry naming a value this census does not find, which the both-ways hold refuses. It is " +
			"erased all the same, at the same line, and that is a fact about the code rather than " +
			"about this gate.",
	},
	"matchesEpochDigestLocked|call subtle.ConstantTimeCompare": {
		carries: []string{"computed"},
		why: "the two DIGESTS being compared -- `computed` is tainted by derivation from the pair " +
			"and is thirty-two octets of SHA-256 output, not a key. Constant time because guardrail " +
			"G8 sends every comparison over a value derived from key material in this tree through it.",
	},
	"matchesEpochDigestLocked|return": {
		carries: []string{"computed"},
		why: "the VERDICT, a bool. `computed` is listed because the census reads the whole return " +
			"statement and the expression mentions it; what crosses this boundary is the answer to " +
			"\"is this candidate the epoch's own secret\", and both keys are already erased above it.",
	},

	"Open|literal protocol.CreateGroupRequest.BootstrapWriteKey": {
		carries: []string{"created"},
		why: "write_key[0], and it is NOT what " +
			"the commit opens. §4.3.2 declares it and calls it self-certification: the server verifies " +
			"the founding commit's write_auth under it and nothing but a 20/day rate limit protects it. " +
			"It is a RULED field, it is epoch zero's, and item 244 is about the chained NEXT epoch.",
	},
	"Open|call append": {
		carries: []string{"bootstrapWriteKey"},
		why: "the copy of that bootstrap key. messagegroup.EpochKeys.WriteKey hands back " +
			"the session's own backing array and Destroy zeroizes it, so the request would carry an " +
			"erased key without this.",
	},

	"subscribeLocked|call authorizeSubscribe": {
		carries: []string{"readKey"},
		why: "the READ key into §4.3.8's req_auth for a SubscribeRequest, a mac computed under the " +
			"key, exactly as Receive's authorizeFetch consumes it. Nothing of the key reaches the wire.",
	},

	"Receive|call authorizeFetch": {
		carries: []string{"readKey"},
		why: "the READ key into §4.3.8's req_auth, which is a mac COMPUTED " +
			"under the key. Nothing of the key reaches the wire: what does is ComputeRequestAuth's " +
			"output. This is the one sink in this package that consumes a key rather than carrying it.",
	},

	// THE OVER-APPROXIMATION, PRINTING ITSELF, AND NARROWED TO THE ONE VALUE IT REACHES. The taint
	// treats anything a key-bearing call hands back as key-bearing -- which is what makes a sealed
	// record carrying the pair in the clear a tainted value -- and `transport.Call` was handed the
	// CreateGroupRequest, so its `response` is tainted and so is the `body` read off it. What
	// actually lands here is `body.GetCurrentEpoch()`, a uint64 the SERVER chose, formatted into an
	// error string. The `carries` list is doing the work in this entry: it excuses `body` and
	// nothing else, so an epoch key formatted into an error -- a leak into every log that error
	// reaches -- is still a refusal at this same site.
	"Open|call fmt.Errorf": {
		carries: []string{"body"},
		why: "the RESPONSE side of the create exchange, not the request side. `response` is " +
			"tainted because the request that produced it carried `created` and `delivery`, `body` " +
			"is tainted off `response`, and what this call formats is body.GetCurrentEpoch(), a " +
			"uint64. No key reaches it, and the carries list above is what says so.",
	},
	"Open|return": {
		carries: []string{"body", "response"},
		why: "the three returns of the create closure, which are the same response-side " +
			"over-approximation: `response.GetReason()` is an enum, `body.GetRecordId()` is a " +
			"uint64 and the third is the error above. A key returned to a caller leaves this " +
			"package by a door no other clause watches -- the caller binds it off a call nothing " +
			"taints -- so this site is weighed here, narrowed to these two values.",
	},

	// ── ITEM 244's FIX, AS FOUR SITES ────────────────────────────────────────────────────────
	//
	// These four replace the four `message.EpochAttachment.WriteKey`/`.ReadKey` entries that stood
	// here until the seal door opened. THE TWO KEYS ARE STILL CONSUMED AT BOTH COMMIT SITES -- they
	// have to be, because the digest is OVER them -- and the difference is what is left behind: a
	// 32 octet SHA-256 output instead of 64 octets of live key. So the disposition does not shrink,
	// it MOVES, and it moves to a pair of sites whose sentence is a measurable one.
	"Open|call message.NewEpochDigestAttachment": {
		carries: []string{"readKey", "writeKey"},
		why: "RULING 27's SUBSTITUTION, at the founding commit. Both keys are handed to the " +
			"constructor because H(epoch_keys) is taken over both; what it hands BACK is the digest. " +
			"The epoch is read ONCE, out of the body being built, so the digest cannot name an epoch " +
			"the attachment disagrees with -- there are three epochs live at this call site and a " +
			"wrong choice among them type checks.",
	},
	"publishCommitLocked|call message.NewEpochDigestAttachment": {
		carries: []string{"readKey", "writeKey"},
		why:     "RULING 27's SUBSTITUTION, at the epoch commit, for the reason above.",
	},
	"Open|literal message.ServerAttachment.EpochDigest": {
		carries: []string{"foundingDigest"},
		why: "THE VALUE THAT GOES INTO THE RECORD, and the one sentence in this file that this " +
			"gate cannot check for itself. `foundingDigest` is tainted by DERIVATION -- it is bound " +
			"from a call whose arguments mention both keys -- and the claim is that its CONTENT is a " +
			"digest and nothing else. A taint analysis cannot tell those apart, so the claim is held " +
			"by measurement instead: TestItem244sKeysAreInTheRequestAndNotInTheRecord searches the " +
			"whole sealed record for both key values with the keys themselves as the inline positive " +
			"control, and cp3b's TestItem244 does it again over a real publish through a real server.",
	},
	"publishCommitLocked|literal message.ServerAttachment.EpochDigest": {
		carries: []string{"commitDigest"},
		why:     "THE VALUE THAT GOES INTO THE RECORD, at the epoch commit, for the reason above.",
	},
	"Open|call self.sendSealedLocked": {
		carries: []string{"founding"},
		why: "the FOUNDING RECORD, tainted because the constructor above was handed the keys and " +
			"this record is derived from what it returned. THE ENTRY THAT STOOD HERE PREDICTED THIS " +
			"SITE WOULD DISAPPEAR when the attachment became a digest; it did not, and that is " +
			"measured rather than argued -- the over-approximation follows derivation, and a record " +
			"built from a digest built from keys is derived from keys however little of them it " +
			"holds. See publishCommitLocked|call self.submitLocked for the same correction at the " +
			"other commit path.",
	},
}

// The types in this package's production source that are built AROUND an epoch key, as
// "<enclosing function>|<type>" -> why.
//
// THIS CLAUSE IS INDEPENDENT OF THE TAINT AND THAT IS ITS JOB. Move the two keys into a helper and
// the sink census above loses them -- the taint does not cross a call boundary -- but a literal of
// a key-carrying type is a site wherever it is written. The two clauses fail on different mutants
// on purpose: a mutation table where every entry varies the same attribute cannot see a change of
// mechanism, which is exactly how connect's step-3 gate was defeated.
var epochKeyCarrierLiterals = map[string]string{
	"epochKeyDelivery|protocol.EpochKeyDelivery": "THE ONE CONSTRUCTOR of the request carrier, in " +
		"record.go. A second one anywhere is a second place the copy could be forgotten.",
	"Open|message.EpochDigestAttachment": "ITEM 244's FIX, at the founding commit: ruling 27's six " +
		"PUBLIC fields, with LP(H(epoch_keys)) where the pair used to be. `message.EpochAttachment` " +
		"is STILL IN THE NET ABOVE and has no entry here, which is the point -- a commit site that " +
		"went back to kind 0x0001 would be censused and refused for having no disposition, rather " +
		"than passing quietly because the net had been narrowed to what the tree now does.",
	"publishCommitLocked|message.EpochDigestAttachment": "ITEM 244's FIX, at the epoch commit, for " +
		"the reason above.",
}

// The literal types the clause above looks for. The net again, not a disposition.
var epochKeyCarrierTypes = map[string]bool{
	"protocol.EpochKeyDelivery":     true,
	"message.EpochAttachment":       true,
	"message.EpochDigestAttachment": true,
}

func TestEveryEpochKeyInThisPackageGoesWhereTheDispositionSaysItGoes(t *testing.T) {
	producers := map[string][]string{}
	sinks := map[string][]string{}
	carried := map[string]map[string]bool{}
	carriers := map[string][]string{}
	unspent := map[string][]string{}

	sources := stateTestProductionSources(t)
	for _, name := range sources {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		fileSet := token.NewFileSet()
		parsed, err := parser.ParseFile(fileSet, name, content, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			where := function.Name.Name

			// THE NAMES THIS FUNCTION DECLARED, which is what makes a bare name on the left of an
			// assignment a rebinding rather than a sink. `stash = writeKey` where `stash` is a
			// package level var parks a key for the lifetime of the process, and calling that a
			// rebinding because it is spelled with one identifier is the same mistake as calling
			// `writeKey[:]` not-a-key because it is spelled with three tokens. A name this
			// function did not declare outlives it, so it is a sink; `_` is neither.
			local := map[string]bool{"_": true}
			declare := func(fields *ast.FieldList) {
				if fields == nil {
					return
				}
				for _, field := range fields.List {
					for _, target := range field.Names {
						local[target.Name] = true
					}
				}
			}
			declare(function.Recv)
			declare(function.Type.Params)
			declare(function.Type.Results)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch shape := node.(type) {
				case *ast.AssignStmt:
					if shape.Tok == token.DEFINE {
						for _, target := range shape.Lhs {
							if identifier, ok := target.(*ast.Ident); ok {
								local[identifier.Name] = true
							}
						}
					}
				case *ast.ValueSpec:
					for _, target := range shape.Names {
						local[target.Name] = true
					}
				case *ast.RangeStmt:
					if shape.Tok == token.DEFINE {
						for _, target := range []ast.Expr{shape.Key, shape.Value} {
							if identifier, ok := target.(*ast.Ident); ok {
								local[identifier.Name] = true
							}
						}
					}
				case *ast.FuncLit:
					declare(shape.Type.Params)
					declare(shape.Type.Results)
				}
				return true
			})

			// the carrier clause, which is independent of the taint: a literal of one of the
			// key-carrying types is a site whatever its fields are built from.
			ast.Inspect(function.Body, func(node ast.Node) bool {
				literal, ok := node.(*ast.CompositeLit)
				if !ok {
					return true
				}
				spelled := epochKeyExpr(literal.Type)
				if epochKeyCarrierTypes[spelled] {
					carriers[where+"|"+spelled] = append(carriers[where+"|"+spelled],
						fmt.Sprintf("%s:%d", name, fileSet.Position(literal.Pos()).Line))
				}
				return true
			})

			// the producers, and the taint they seed. TWO SPELLINGS PRODUCE A KEY, not one: a
			// CALL (`keys.WriteKey()`, `message.WriteKey(root)`, `msg.GetWriteKey()`) and a bare
			// FIELD READ off a protobuf (`delivery.WriteKey`), which is the generated struct's
			// own spelling and the shorter of the two. A net that asked only for the call shape
			// would be the same blindness as the sink census's, one clause up. The callee of a
			// producer call is not counted a second time as a field read -- `callee` holds the
			// selectors already spent that way -- so the two passes cannot double-report a site.
			tainted := map[string]bool{}
			callee := map[ast.Node]bool{}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !epochKeyProducerSelectors[selector.Sel.Name] {
					return true
				}
				callee[selector] = true
				site := where + "|" + epochKeyExpr(selector)
				producers[site] = append(producers[site],
					fmt.Sprintf("%s:%d", name, fileSet.Position(call.Pos()).Line))
				return true
			})
			ast.Inspect(function.Body, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok || callee[selector] || !epochKeyProducerSelectors[selector.Sel.Name] {
					return true
				}
				site := where + "|" + epochKeyExpr(selector)
				producers[site] = append(producers[site],
					fmt.Sprintf("%s:%d field", name, fileSet.Position(selector.Pos()).Line))
				return true
			})
			// the fixpoint: a binding whose right hand side mentions a producer call or an
			// already-tainted identifier binds a key. It runs to a fixpoint rather than once
			// because a key reaches its sink through as many hops as the source cares to take,
			// and an analysis that followed one hop would be defeated by writing two.
			//
			// IT FOLLOWS EVERY BINDING FORM THE LANGUAGE HAS AND NOT ONLY `:=`. This loop used to
			// inspect *ast.AssignStmt alone, and `var leaked = writeKey` -- the same key, the same
			// function, the same one hop, spelled with the other keyword -- bound nothing, so
			// `leaked` was not tainted, so every sink clause below searched a value nothing
			// tainted and passed by finding nothing. Measured at 4fde7ad: that spelling put the
			// write key on `commitRecord.Header.BlobId` with this gate and the whole ./urmessage
			// suite green. `var` (*ast.ValueSpec) and `range` (*ast.RangeStmt) bind here now.
			for spin := 0; spin < 16; spin += 1 {
				grew := false
				bind := func(targets []ast.Expr, values []ast.Expr) {
					if !epochKeyRhsCarriesAKey(values, tainted) {
						return
					}
					for _, target := range targets {
						identifier, ok := target.(*ast.Ident)
						if !ok || identifier.Name == "_" || identifier.Name == "err" {
							continue
						}
						if !tainted[identifier.Name] {
							tainted[identifier.Name] = true
							grew = true
						}
					}
				}
				ast.Inspect(function.Body, func(node ast.Node) bool {
					switch shape := node.(type) {
					case *ast.AssignStmt:
						bind(shape.Lhs, shape.Rhs)
					case *ast.ValueSpec:
						// `var x = writeKey`, and `var x, y = f()`. A spec with no values --
						// `var readKey []byte`, which Receive really writes -- carries nothing
						// and binds nothing, which is the same answer it gave before.
						declared := []ast.Expr{}
						for _, target := range shape.Names {
							declared = append(declared, target)
						}
						bind(declared, shape.Values)
					case *ast.RangeStmt:
						// `for _, b := range writeKey` hands the loop variable the key's own
						// octets. Over-approximate on purpose, like every other clause here: a
						// gate that called an octet of a key not-a-key would be arguing with
						// itself about how much of a key is a key.
						bind([]ast.Expr{shape.Key, shape.Value}, []ast.Expr{shape.X})
					}
					return true
				})
				if !grew {
					break
				}
			}
			if len(tainted) == 0 {
				continue
			}

			// the sinks, and the complement: a tainted identifier that reaches no sink at all is
			// printed rather than asserted, because it is the part of the census that is NOT the
			// property -- a value that goes nowhere is a value that leaked nowhere.
			//
			// EVERY CLAUSE HERE ASKS AN EXPRESSION AND NOT A NODE CLASS. Each of the three used to
			// ask its landing place for an *ast.Ident and drop anything else without a word, so
			// `x = writeKey` was a site and `x = writeKey[:]` -- one character, the same key, the
			// same octets -- was censused nowhere: the disposition had nothing to refuse and the
			// gate reported success by not looking. Measured at 4fde7ad, the version this repairs:
			// `request.ReqAuth = readKey[:]` between authorizeFetch and transport.Call left this
			// gate at `ok 0.099s` and `go test ./urmessage -run '.*'` at `ok 7.070s`, with the
			// epoch read key riding out on FetchRequest.req_auth on every page that left the
			// device. That is this file's own subject defeated one abstraction level up: the
			// disposition was asserted in both directions and the census that fed it could not see
			// the site. So the one question asked is [epochKeyBorneBy]'s -- does this EXPRESSION
			// bear a key, however it is spelled -- and `writeKey[:]`, `(writeKey)`, `keys[0]`,
			// `[]byte(writeKey)` and `any(writeKey).([]byte)` are one site rather than five holes.
			landed := map[string]bool{}
			record := func(site string, pos token.Pos, expression ast.Expr, borne []string) {
				sinks[site] = append(sinks[site], fmt.Sprintf("%s:%d %s", name,
					fileSet.Position(pos).Line, epochKeyExpr(expression)))
				if carried[site] == nil {
					carried[site] = map[string]bool{}
				}
				for _, spelled := range borne {
					carried[site][spelled] = true
					if tainted[spelled] {
						landed[spelled] = true
					}
				}
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch shape := node.(type) {
				case *ast.CompositeLit:
					spelled := epochKeyExpr(shape.Type)
					for at, element := range shape.Elts {
						// a positional element is a landing place too: `[][]byte{writeKey}` puts
						// the key exactly where `{WriteKey: writeKey}` does and names no field.
						value := element
						field := fmt.Sprintf("element %d", at)
						if pair, ok := element.(*ast.KeyValueExpr); ok {
							value = pair.Value
							field = epochKeyExpr(pair.Key)
						}
						borne := epochKeyBorneBy(value, tainted, false)
						if len(borne) == 0 {
							continue
						}
						record(fmt.Sprintf("%s|literal %s.%s", where, spelled, field),
							element.Pos(), value, borne)
					}
				case *ast.CallExpr:
					for _, argument := range shape.Args {
						borne := epochKeyBorneBy(argument, tainted, false)
						if len(borne) == 0 {
							continue
						}
						record(fmt.Sprintf("%s|call %s", where, epochKeyExpr(shape.Fun)),
							shape.Pos(), argument, borne)
					}
				case *ast.AssignStmt:
					for at, target := range shape.Lhs {
						// A BARE NAME THIS FUNCTION DECLARED IS A REBINDING AND NOT A SINK -- the
						// fixpoint above already taints it and follows it onward. ANYTHING ELSE
						// stores into something that outlives this call, and it is a site
						// whatever its shape: a field (`request.ReqAuth`), a map entry (`m[k]`),
						// a pointee (`*p`), a package level var (`stash`). Only the SelectorExpr
						// case was asked for before, which is the same blindness as the right
						// hand side's, one side of the equals sign over.
						if identifier, bare := target.(*ast.Ident); bare && local[identifier.Name] {
							continue
						}
						var source ast.Expr
						switch {
						case len(shape.Lhs) == len(shape.Rhs):
							source = shape.Rhs[at]
						case len(shape.Rhs) == 1:
							// `a, b = f()`: the one right hand side feeds every target.
							source = shape.Rhs[0]
						default:
							continue
						}
						borne := epochKeyBorneBy(source, tainted, false)
						if len(borne) == 0 {
							continue
						}
						record(fmt.Sprintf("%s|assign %s", where, epochKeyExpr(target)),
							shape.Pos(), source, borne)
					}
				case *ast.ReturnStmt:
					// A KEY HANDED BACK TO THE CALLER LEAVES THIS FUNCTION AS SURELY AS ONE
					// ASSIGNED TO A FIELD, and it is the one exit the three clauses above cannot
					// see: the caller binds it from a call this gate has no reason to taint. It
					// is the clause that refuses `func (self *Group) EpochWriteKey() []byte`.
					// What it finds today is `Open|return`, and that is the over-approximation
					// reaching the create closure's three returns rather than a key -- the
					// disposition entry names the two values and the measurement.
					for _, result := range shape.Results {
						borne := epochKeyBorneBy(result, tainted, false)
						if len(borne) == 0 {
							continue
						}
						record(where+"|return", shape.Pos(), result, borne)
					}
				case *ast.SendStmt:
					// and the other exit a statement can be: `ch <- writeKey`.
					borne := epochKeyBorneBy(shape.Value, tainted, false)
					if 0 < len(borne) {
						record(fmt.Sprintf("%s|send %s", where, epochKeyExpr(shape.Chan)),
							shape.Pos(), shape.Value, borne)
					}
				}
				return true
			})
			for identifier := range tainted {
				if !landed[identifier] {
					unspent[where] = append(unspent[where], identifier)
				}
			}
		}
	}

	// ── THE COMPLEMENT, PRINTED: what this search covered and what it left out ────────────────
	t.Logf("production sources read (%d): %v", len(sources), sources)
	t.Logf("producer net (a call whose result is taken to be an epoch key): %v",
		epochKeySortedKeys(epochKeyProducerSelectors))
	t.Logf("carrier net (a composite literal taken to be built around an epoch key): %v",
		epochKeySortedKeys(epochKeyCarrierTypes))
	t.Logf("producer sites found (%d):", len(producers))
	for _, site := range epochKeySortedMap(producers) {
		t.Logf("    %s  at %v", site, producers[site])
	}
	t.Logf("sink sites found (%d), each with THE VALUES IT CARRIES, which is what the disposition "+
		"is held against a second time:", len(sinks))
	for _, site := range epochKeySortedMap(sinks) {
		t.Logf("    %s  carries %v  at %v", site, epochKeySortedKeys(carried[site]), sinks[site])
	}
	t.Logf("carrier literals found (%d):", len(carriers))
	for _, site := range epochKeySortedMap(carriers) {
		t.Logf("    %s  at %v", site, carriers[site])
	}
	t.Logf("EXCLUDED, and excluded is not the same as absent -- tainted values that reach no sink "+
		"in their own function, so nothing carried them anywhere: %v", unspent)

	// ── AND ASSERTED, IN BOTH DIRECTIONS, AGAINST THREE WRITTEN-DOWN DISPOSITIONS ─────────────
	epochKeyHold(t, "producer site", producers, epochKeyProducerSites,
		"A call that produces an epoch key is where this gate's whole search begins. A site with no "+
			"entry is a key derived somewhere nobody weighed; an entry with no site is this gate "+
			"having gone BLIND -- the producer was respelled and the sink clauses below are now "+
			"searching a value nothing tainted, which passes by finding nothing.")
	sinkWhy := map[string]string{}
	for site, entry := range epochKeySinks {
		sinkWhy[site] = entry.why
	}
	sinkNarrowing := "This is ruling 33 held on the value rather than on the field name: the epoch " +
		"keys travel on the REQUEST carrier and nowhere else. A site with no entry is a key landing " +
		"somewhere this package has not weighed -- `BlobId: writeKey` fails here exactly as " +
		"`WriteKey: writeKey` does, which is the defeat connect's step-3 gate took three times, and " +
		"`BlobId: writeKey[:]` fails exactly as both, which is the defeat THIS file took at 4fde7ad. " +
		"An entry with no site is a disposition that has stopped describing the code."
	epochKeyHold(t, "sink site", sinks, sinkWhy, sinkNarrowing)

	// ── AND THE SECOND NARROWING: NOT ONLY WHERE, BUT WHICH VALUE ─────────────────────────────
	//
	// An entry excuses the values it names and no others. Without this loop `Open|call fmt.Errorf`
	// -- which the over-approximation reaches honestly, carrying a uint64 off the server's own
	// response -- would be a standing permit to format an epoch key into an error string at that
	// same call. It is held both ways like everything else: an unlisted value that lands is a
	// refusal, and a listed value that no longer lands is a refusal.
	for _, site := range epochKeySortedMap(sinks) {
		entry, dispositioned := epochKeySinks[site]
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
	epochKeyHold(t, "carrier literal", carriers, epochKeyCarrierLiterals,
		"A structure built AROUND an epoch key is a site whatever its fields are built from, which "+
			"is the clause that survives the keys being moved one function away from the literal. "+
			"The two message.EpochAttachment entries are item 244 STILL OPEN in this package and "+
			"are marked so; the day connect's seal door will encode kind 0x0005 they become "+
			"message.NewEpochDigestAttachment calls, this census loses them, and this map has to be "+
			"emptied in the same commit -- which it will report as an entry nothing needs.")
}

// epochKeyHold is the both-directions assertion the three censuses share.
func epochKeyHold(t *testing.T, what string, found map[string][]string, disposition map[string]string,
	why string) {

	t.Helper()
	if len(found) == 0 {
		t.Errorf("this gate found no %s at all in %d production files. An empty census passes every "+
			"refusal below by having nothing to refuse, which is how a search that has stopped "+
			"reading its own subject reports success.", what, len(stateTestProductionSources(t)))
	}
	for site := range found {
		reason, dispositioned := disposition[site]
		if !dispositioned {
			t.Errorf("%s %q has no entry in the disposition.\n%s", what, site, why)
			continue
		}
		t.Logf("    %s %s is allowed: %s", what, site, reason)
	}
	for site := range disposition {
		if _, ok := found[site]; !ok {
			t.Errorf("the disposition says %s %q is allowed and the census does not find it.\n%s",
				what, site, why)
		}
	}
}

// epochKeyBorneBy is THE ONE QUESTION this gate asks of an expression: which epoch keys does it
// bear? It answers the tainted identifiers and the producer calls anywhere inside the expression,
// spelled the way the source spells them, and both the taint step and all three sink clauses run
// it. One predicate, asked in every position, is the point: the defect this repairs was two
// positions asking a DIFFERENT and narrower question than the taint step did.
//
// IT MATCHES THE VALUE AND NOT THE NODE CLASS. `writeKey` is an *ast.Ident, `writeKey[:]` is an
// *ast.SliceExpr, `(writeKey)` is an *ast.ParenExpr, `keys[0]` is an *ast.IndexExpr,
// `[]byte(writeKey)` is an *ast.CallExpr and `any(writeKey).([]byte)` is an *ast.TypeAssertExpr --
// six spellings of one key, and a census that asked for the first shape by name saw one of them.
// [epochKeyExpr] in this same file already had cases for four of those shapes, so the file knew
// they occur in this source and the census did not ask; that gap is the whole finding.
//
// IT IS OVER-APPROXIMATE ON PURPOSE. A sealer handed a key returns a record that CONTAINS it, so
// the record is tainted too, and the submit that carries that record is a sink. That is the correct
// answer for as long as the attachment carries the pair in the clear, and it is the clause that
// will report the change when it stops: the record stops being tainted, the submit stops being a
// sink, and the disposition entry for it becomes an entry nothing needs.
// A NAME IN A NAME POSITION IS NOT A VALUE, and it walks accordingly. `self.founding` is the
// group's field and not the local `founding`, and `EpochAttachment{WriteKey: ...}` names a field
// and does not read one, so the walk never treats a selector's `Sel` or a struct literal's key as
// a value it bears. Without that, `self.founding` bore `founding` and every call taking it was a
// sink -- a census that cries at a name collision is a census nobody keeps.
//
// WHAT IT DOES NOT WALK INTO, when intoLiterals is false, is a composite literal or a function
// literal, because the clause above censuses every literal AT ITS OWN FIELD wherever it is
// written, nested or not. `SealRecord(&ServerAttachment{Epoch: &EpochAttachment{WriteKey: k}})`
// is reported once, at `literal message.EpochAttachment.WriteKey`, rather than four times up the
// spine of one expression. That is precision and not coverage: the mutation table drives a key
// into a nested literal and the site is still red, at the precise name. The TAINT step walks with
// intoLiterals true, because a sealer handed a literal containing a key returns a value carrying
// one, and that is how a sealed record comes to be tainted.
func epochKeyBorneBy(expression ast.Expr, tainted map[string]bool, intoLiterals bool) []string {
	if expression == nil {
		return nil
	}
	borne := []string{}
	seen := map[string]bool{}
	carry := func(spelled string) {
		if !seen[spelled] {
			seen[spelled] = true
			borne = append(borne, spelled)
		}
	}
	recurse := func(inner ast.Expr) {
		for _, spelled := range epochKeyBorneBy(inner, tainted, intoLiterals) {
			carry(spelled)
		}
	}
	ast.Inspect(expression, func(node ast.Node) bool {
		switch shape := node.(type) {
		case *ast.CallExpr:
			if selector, ok := shape.Fun.(*ast.SelectorExpr); ok &&
				epochKeyProducerSelectors[selector.Sel.Name] {
				carry(epochKeyExpr(selector))
				// the callee is the producer's own name; only its arguments are values
				for _, argument := range shape.Args {
					recurse(argument)
				}
				return false
			}
		case *ast.SelectorExpr:
			if epochKeyProducerSelectors[shape.Sel.Name] {
				// a key read STRAIGHT OFF A FIELD -- `delivery.WriteKey`, which is the
				// protobuf's own spelling and one character shorter than the getter the
				// producer net was written for.
				carry(epochKeyExpr(shape))
			}
			recurse(shape.X)
			return false
		case *ast.KeyValueExpr:
			if _, named := shape.Key.(*ast.Ident); named {
				recurse(shape.Value)
				return false
			}
		case *ast.CompositeLit:
			if !intoLiterals {
				return false
			}
		case *ast.FuncLit:
			if !intoLiterals {
				return false
			}
		case *ast.Ident:
			if tainted[shape.Name] {
				carry(shape.Name)
			}
		}
		return true
	})
	sort.Strings(borne)
	return borne
}

// epochKeyRhsCarriesAKey is the taint step, and it is [epochKeyBorneBy] asked of each value in a
// binding. It is kept as its own name because it reads as what the fixpoint above needs.
func epochKeyRhsCarriesAKey(right []ast.Expr, tainted map[string]bool) bool {
	for _, expression := range right {
		if 0 < len(epochKeyBorneBy(expression, tainted, true)) {
			return true
		}
	}
	return false
}

// epochKeyExpr spells a qualified name the way the source does, so a census entry reads as the code
// reads. Anything it cannot spell answers "?" rather than being dropped: a site this gate could not
// name is still a site, and a silent drop is the blindness the whole file is arranged against.
func epochKeyExpr(expression ast.Expr) string {
	switch shape := expression.(type) {
	case nil:
		return "?"
	case *ast.Ident:
		return shape.Name
	case *ast.SelectorExpr:
		return epochKeyExpr(shape.X) + "." + shape.Sel.Name
	case *ast.StarExpr:
		return "*" + epochKeyExpr(shape.X)
	case *ast.UnaryExpr:
		return epochKeyExpr(shape.X)
	case *ast.IndexExpr:
		return epochKeyExpr(shape.X)
	case *ast.ArrayType:
		return "[]" + epochKeyExpr(shape.Elt)

	// THE FOUR SHAPES THE CENSUS USED TO DROP, now spelled rather than answered "?". They are
	// here so a site's evidence reads as the source reads -- `writeKey[:]` rather than `?` -- and
	// so the day one of them appears at a live site the failure names what it saw.
	case *ast.SliceExpr:
		return epochKeyExpr(shape.X) + "[:]"
	case *ast.ParenExpr:
		return "(" + epochKeyExpr(shape.X) + ")"
	case *ast.TypeAssertExpr:
		return epochKeyExpr(shape.X) + ".(" + epochKeyExpr(shape.Type) + ")"
	case *ast.CallExpr:
		return epochKeyExpr(shape.Fun) + "(...)"
	case *ast.CompositeLit:
		return epochKeyExpr(shape.Type) + "{...}"
	case *ast.FuncLit:
		return "func(...)"
	case *ast.BasicLit:
		return shape.Value
	}
	return "?"
}

func epochKeySortedKeys(set map[string]bool) []string {
	names := []string{}
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func epochKeySortedMap(sites map[string][]string) []string {
	names := []string{}
	for name := range sites {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
