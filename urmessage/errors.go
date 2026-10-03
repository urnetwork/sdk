package urmessage

import "errors"

// Every refusal this package owns. Each one exists because its alternative is a silent zero: a
// message that was never sent, a message that arrived and was dropped, or a group that looks open
// and is not.
var (
	// The config did not carry something with no honest default.
	ErrNoTransport = errors.New("urmessage: a device needs an sdk.MessageTransport; S2-7 is open and this package stands nothing up")
	ErrNoReserver  = errors.New("urmessage: a device needs a durable stream index reserver; sdk.NewStreamIndexReserver over an sdk.StreamStore is the one this module ships")

	// Hello has not been said on this transport, so there is no connection nonce for write_auth
	// or req_auth to be computed over.
	ErrNotConnected = errors.New("urmessage: this device has not said Hello, and every authenticator is a mac over the connection's server_nonce")

	// §4.3.1's nonce moved and the session would not take the new one. NEVER swallowed: a send
	// that cannot be re-bound is a send that has not happened.
	ErrNonceRebind = errors.New("urmessage: this group's session could not be rebound onto the connection's current server_nonce")

	// The server refused. Carried rather than collapsed into a nil, because §4.5's reasons are
	// what a caller has to see.
	ErrSubmitRefused = errors.New("urmessage: the message server refused this record")
	ErrFetchRefused  = errors.New("urmessage: the message server refused this fetch")
	ErrCreateRefused = errors.New("urmessage: the message server refused this group")
	ErrHelloRefused  = errors.New("urmessage: the message server refused this Hello")

	// Ruling 33's alignment, raised HERE rather than met as a REASON_REJECTED on the wire.
	// `SubmitRequest.epoch_keys` is positionally aligned with `records`: an entry opposite a
	// record with is_commit = 0 is a refusal, and so is a commit with no entry. A client that got
	// the alignment wrong would be handing the server a key aimed at a record it does not open,
	// and the one sentence that says which way round it went wrong is cheaper to read than the
	// reason code that answers it.
	ErrEpochKeyDelivery = errors.New("urmessage: the epoch keys on this request are not the ones the records on it need")

	// The ordering §6.1 imposes, raised here rather than met as a REASON_REJECTED on the wire.
	ErrGroupNotOpen  = errors.New("urmessage: this group has not been opened on the server; Open publishes the founding commit, the epoch's wraps and the marker that closes them")
	ErrGroupOpen     = errors.New("urmessage: this group is already open on the server")
	ErrNoMemberAdded = errors.New("urmessage: a group is opened at the epoch its first commit creates, so AddMember comes before Open")
	ErrAlphaOneAdd   = errors.New("urmessage: the alpha adds one member, before Open, in the commit that opens epoch 1; a second add is a second epoch and is not built")

	// A text that will not fit a rung, refused by the sealer and named here so the caller sees a
	// sentence about its message rather than about a size bucket.
	ErrTextTooLong = errors.New("urmessage: this text does not fit the largest inline size bucket; blob-backed bodies are out of scope for the alpha")

	// An invite whose octets are not the octets that were encoded: truncated, mangled in a paste, or
	// rewritten by a carrier. It is refused at ParseInvite, which is where a user pasting one can be
	// told, and never at the join or at the first Receive after it.
	ErrInviteDamaged = errors.New("urmessage: this invite is damaged: it is not the octets the founder encoded")

	// A record came back that a key should have opened and did not.
	ErrRecordOpen = errors.New("urmessage: a record from a member of this group did not open")

	// ── ingesting a commit (§6.1's membership change, MASTER §11) ──────────────────────────

	// An is_commit record was received and this device could not follow the group into the epoch
	// it opens: the commit would not process, would not apply, or the session could not be advanced
	// onto the new epoch. It is carried rather than swallowed because a member that cannot ingest a
	// commit has fallen off the group and cannot read the next message.
	ErrCommitIngest = errors.New("urmessage: this group received a membership-change commit it could not follow into the next epoch")

	// ── the three ways a rotated pq_secret fails to arrive (ledger item 251's ruling 38) ───
	//
	// THEY ARE THREE SENTINELS AND NOT ONE, AND THAT IS THE RULING RATHER THAN A PREFERENCE.
	// The day a device wrap carries key material, a member that never opens a readable one
	// goes dark in BOTH directions and permanently: read_key[n+1] and write_key[n+1] both hang
	// off storage_root[n+1], and the server verifies req_auth before any AEAD is reached, so
	// what the field sees is REASON_REJECTED with nothing readable behind it. The orphan case
	// -- a fan-out from a committer that LOST its CAS race, addressed to an epoch that never
	// opened under its secret -- "must be a typed refusal separable from this one, or the two
	// are indistinguishable in the field". They name three different CAUSES -- a lost CAS race, a
	// wrong key or an altered record, and item 132's omission -- and the operator acts on the
	// three differently.
	//
	// WHAT THEY DO NOT NAME IS THREE DIFFERENT COSTS, AND THIS BLOCK USED TO SAY THEY DID.
	// Reaching ANY of the three means this device followed a commit into an epoch it holds no
	// pq_secret for, and from that moment the cost is one cost and it is total and permanent:
	// see [ErrOrphanWrap] for the measurement, which is the same for all three. A caller reads
	// the sentinel to learn WHO to go to; it must not read it as a severity.
	//
	// Each has a counter beside it on [Stats], because a sentinel is only visible to a caller
	// that is holding the error and cannot answer "is this happening". The counters are this
	// PROCESS's; the diagnosis itself is durable, because [GroupRecord] carries the epoch this
	// device went dark at and which of these three it was.

	// NO WRAP ADDRESSED TO THIS DEVICE ARRIVED for an epoch that was opened with a pq_secret
	// this device does not hold. It is item 132's omission attack arriving as a diagnosis: a
	// committer that leaves one member out of the fan-out while declaring the matching
	// expected_wrap_count produces a group that is writable, self-consistent to the server and
	// permanently unreadable for the omitted member, and this is the omitted member saying so.
	//
	// IT IS NOT REACHED WHEN THE COMMITTER SIMPLY DID NOT ROTATE. A commit built before
	// rotation opens its epoch with the secret every member already holds, which reproduces
	// that epoch's own H(epoch_keys) and is taken; see [Group.resolvePqSecretLocked].
	ErrNoWrapForEpoch = errors.New("urmessage: no device wrap for this epoch reached this device, so it holds neither of that epoch's keys and can neither read nor write in it")

	// A WRAP AT THIS DEVICE'S OWN wrap_target_handle ARRIVED AND DID NOT OPEN. X-Wing's
	// ML-KEM-768 half uses implicit rejection -- a ciphertext produced for another key
	// decapsulates successfully, to a pseudorandom secret -- so this is always the Poly1305 tag
	// and never a decapsulation error, and it means the record was sealed to a different
	// encapsulation key, at a different epoch, or was altered.
	ErrWrapUnreadable = errors.New("urmessage: a device wrap addressed to this device did not open, so this device holds no pq_secret for the epoch it was for")

	// A WRAP ADDRESSED TO THIS DEVICE OPENED, AND IT IS NOT THE SECRET THE EPOCH WAS OPENED
	// WITH. Ruling 37 has the fan-out submitted at epoch n, staged and pre-merge, so a committer
	// writes its wraps and then loses the race for the commit -- which leaves wraps addressed to
	// an epoch that never opened under their secret. This build PRODUCES that state by design,
	// which is why the detector ships with the rotation rather than after it.
	//
	// AND THE LOST RACE IS ONE OF TWO CAUSES AND NOT THE ONLY ONE, which is a 2026-09-24
	// correction of this sentence and not an addition to it. A device wrap is addressed to a
	// wrap_target_handle any member can derive from the group handle key, and sealed to a leaf
	// key that is public in the ratchet tree -- so ANY member can land an openable row at any
	// other member's handle for any epoch. That is item 132's decoy, this build cannot tell it
	// from a losing committer's fan-out, and it no longer claims to: the sentinel names both.
	// What it means for THIS device is the same either way, and that is the paragraph below.
	//
	// IT IS NOBODY'S FAULT AND IT DOES NOT REPAIR ITSELF. Those two used to be one sentence here
	// and the second half was FALSE, not merely unmeasured. The loser's fan-out is harmless only
	// while the WINNER's wrap is in the same page; this sentinel is reached exactly when it was
	// not, and by then the epoch is open, this device has followed it on a secret no peer holds,
	// and there is no later page in which the right wrap can arrive.
	//
	// WHY NO LATER PAGE, MEASURED IN connect RATHER THAN ASSERTED, because "it resolves itself"
	// is the kind of sentence that is true of a design and false of a build. A fetch carries
	// req_auth MAC'd under read_key[read_epoch], which msgrepo's api/fetch.go check 7 verifies
	// against the key the committer published for that epoch, BEFORE a single row is read. A
	// device that holds the wrong pq_secret for the epoch it stands at derives the wrong
	// read_key for it, so every fetch it makes is REASON_REJECTED. It cannot fall back to the
	// epoch below either: connect 74abe029 answers read_key and write_key through exactly one
	// door, [messagegroup.GroupSession.EpochKeys], which answers the SESSION's own epoch
	// (session.go:446, `newEpochKeys(self.epoch, self.readKey, self.writeKey)`), and the
	// past-epoch value `pastEpoch` carries is `classKeys` and no read or write key at all. The
	// control for that query is in it: the past-epoch doors that DO exist are RoleAt and
	// TrackSenderAt, so the absence is of this key pair and not of past-epoch access.
	//
	// SO THE COST IS: dark in both directions, at that epoch, for ever, across restarts, and the
	// only repair is out of band -- this device is re-Added to the group and receives the current
	// epoch's secret in its Welcome. [Group.wrapDark] and [GroupRecord.WrapDarkKind] are the
	// diagnosis kept where a caller can find it, which is all this build can do about it.
	ErrOrphanWrap = errors.New("urmessage: the device wraps this device opened for this epoch carry no secret this epoch was opened with, so they are a fan-out for an epoch that never opened or records landed at this device's handle by somebody else")

	// A COMMIT THAT REMOVES A LEAF AND DOES NOT ROTATE pq_secret, refused on ingest.
	//
	// IT IS ITEM 243 ARRIVING INVERTED. Following a commit on a secret this device already holds
	// is right for every group built before rotation and is exactly wrong when the commit
	// REMOVES somebody: the removed member holds that same value by construction, so it
	// reproduces the survivors' storage root at the epoch it was removed at and the removal
	// removed nothing.
	//
	// WHAT IT CHECKS IS ONE RECEIVER'S OWN HISTORY, AND THE PROMISE SAYS SO NOW -- LEDGER RULINGS
	// 42-45. This sentinel used to be documented, and worded, as a GROUP property: *no removal may
	// be followed on a secret this group already holds*. It cannot deliver one. What it delivers is
	//
	//	THIS RECEIVER DOES NOT FOLLOW A REMOVAL ONTO A SECRET THIS RECEIVER HAS HELD
	//
	// and the three shapes that fall outside that -- a late joiner whose history is strictly
	// smaller, a group in which every survivor joined after the reused epoch and NOBODY refuses,
	// and a hostile ADMIN or OWNER committer against which this delivers nothing structurally --
	// are written out with what holds each at [refuseRemovalOnHeldSecret]. A caller that acts on
	// this error is holding a statement about ITS OWN device and not about the group.
	//
	// THE RULE IS ON THE VALUE AND NOT ON ONE ARM, which is the 2026-09-24 repair. It was
	// written as a rule about the two arms of [Group.resolvePqSecretLocked] that return the
	// secret this device HOLDS, and the arm that returns a WRAP CANDIDATE reaches the same value
	// off the wire: a committer that removed a leaf and fanned out the held secret was followed
	// with a nil error, no dark state and no refusal. Every secret that resolution answers now
	// leaves by one exit and is compared against every value this device HAS EVER HELD.
	//
	// AND "EVER HELD" IS NOT "STILL HOLDS", WHICH IS THE SECOND HALF OF THAT REPAIR. The subject
	// was the live pq_secret table, and that table is pruned at [messagegroup.PastEpochWindow] --
	// so the rule's set shrank while the removed member's did not, and a removal fanned out on an
	// EVICTED epoch's secret was followed with a nil error after 33 honest rotations.
	// [Group.pqSecretWitness] is a digest of every value this device has filed, kept for ever and
	// persisted, and it is what the rule is spelled against now.
	//
	// RULING 41: IT IS AN INVALID COMMIT AND NOT A DARK STATE. It is refused the way an
	// unauthorized commit is -- the receiver stays at epoch n, does not advance and does not
	// set [Group.wrapDark] -- because advancing into a permanent brick on a commit just judged
	// invalid is how any client on an older build would brick every up-to-date member of its
	// group by removing somebody. [Group.refuseUnrotatedRemovalLocked] takes the decision
	// before ApplyCommit, where staying at n is possible, and carries the residual it does not
	// reach. What it does NOT refuse is an ABSENCE -- no wrap at all, or a wrap that did not
	// open -- because that is what an honest rotated removal looks like to a member whose own
	// wrap was omitted, and spending this sentinel on it made ruling 41's other outcome
	// unreachable for a removal. Those go DARK at n+1 under their own sentinel.
	//
	// AND THE HALT IT NAMES IS STICKY, PERSISTED AND PERMANENT: [Group.halted]. Every later
	// walk, Send and Commit answers this, the next process reads it off the group record, and
	// the refused commit is never retried or abandoned. A committer that re-commits properly
	// does NOT repair it -- the refused commit stays in the log ahead of this receiver -- and
	// the repair is the same one a dark group needs: this device is re-Added.
	//
	// IT IS A RECEIVE-SIDE RULE BECAUSE THE SEND SIDE CANNOT PRODUCE IT. This build's own
	// removal always rotates -- [Group.stageEpochRotationLocked] draws before it enumerates --
	// so what this refuses is a commit from some OTHER build, which Spec B section 5.4's open
	// acceptance window still admits and which is the shape every build before this one emitted.
	// No production verb writes removeLeaves yet (rolescommit.go), so it is landed ahead of the
	// verb rather than after it.
	// THE SENTENCE IS WHAT IS MEASURED AND NOT WHAT IS INFERRED. It used to say the commit
	// "opened its epoch with the pq_secret this group already held", and one of the three arms
	// that reach this cannot know that: a commit carrying NO epoch digest carries no
	// authenticator to open anything against, and what is true of it is that there is no way to
	// follow it other than on a value this device already has. "Could only be followed on" is true
	// of all three arms; the arm's own clause, carried in the wrapped message, says which.
	ErrRemovalWithoutRotation = errors.New("urmessage: a commit that removes a member could only be followed on a pq_secret THIS DEVICE has already held, so the removed member keeps the post-quantum half of that epoch's storage root and has not been removed from a quantum adversary at all; this device has refused the commit and is halted at the epoch it was at -- this is a statement about this receiver's own history and not about the group, and a member admitted later would not have refused")

	// ── RULING 52: THE THIRD STATE, AND IT IS A *VALID* COMMIT ─────────────────────────────────
	//
	// A commit this group received TOOK THIS DEVICE OUT OF THE GROUP. Ledger item 257's ruling 52
	// is written as what this is NOT, and each clause is a different field it must not be confused
	// with:
	//
	//   - NOT an eighth [GapReason]. Ruling 16 closed that set, and a gap is a record that arrived
	//     and would not render. This record arrived, opened, verified against its committer and
	//     was judged VALID by every §11 rule -- and then ended the membership.
	//   - NOT [Group.halted]. Ruling 41's halt is a commit this device REFUSED as invalid, and it
	//     is the honest answer to a removal nobody rotated for. This commit is valid; refusing it
	//     would be refusing arithmetic.
	//   - NOT [Group.wrapDark]. A dark group followed a valid commit into an epoch whose wrap did
	//     not reach it, so it holds the epoch and not the keys. This device does not hold the
	//     epoch at all, and nothing in it was ever addressed to it -- by construction, since
	//     ledger item 258's derivation shuts a removed leaf out of the fan-out its own removal
	//     opens.
	//
	// So it is a third thing with its own field ([Group.removed]), its own persisted part (part
	// TEN of [GroupRecord]) and this sentinel.
	//
	// WHAT A DEVICE SAW BEFORE THIS SENTINEL EXISTED, MEASURED AT sdk ca89760 OVER A REAL SERVER
	// AND NOT REASONED. Four walks, in this order, and then silence for ever:
	//
	//	1st Receive: ErrCommitIngest: applying the commit: mls: this client was removed by the commit
	//	2nd Receive: ErrCommitIngest: processing the commit: mls: the group is closed and its epoch secrets have been zeroized
	//	3rd Receive: ErrRecordAbandoned: record 7, after 3 attempts: <the 2nd sentence>
	//	4th Receive: nil. 5th: nil. Every later one: nil.
	//
	// Four things are wrong with that and this sentinel is the answer to all four. The one walk
	// that named the cause named it as a GENERIC failure to follow a commit, which is the same
	// sentinel a bent ciphertext answers. The SECOND walk lost the cause altogether --
	// mls.ErrRemovedFromGroup is unreachable once the handle has closed itself, exactly as ruling
	// 41's refusal became `ratchet generation already consumed` on its second walk. The third
	// spent [maxRecordAttempts] and resolved the cursor PAST the record, as though a removal were
	// a record that did not open. And from the fourth on the device was INDISTINGUISHABLE FROM
	// CAUGHT-UP-AND-SILENT: nil error, [Stats.Omitted] at zero -- item 246's ceiling serves the
	// rows at and below the epoch it was removed at and calls the page COMPLETE with a
	// ceiling-relative high water, so the omission predicate has nothing to report -- and a
	// composer the user could still type into.
	//
	// WHAT A CALLER DOES WITH IT. It is STICKY, PERSISTED and PERMANENT: every later [Group.Receive],
	// [Group.Send] and commit door answers it, the next process reads it off the group record rather
	// than re-deriving it, and the walk keeps the cursor BELOW the removing commit instead of giving
	// up on it. The group is not broken and its history is not lost -- this device holds the keys of
	// the epoch it was removed at, so the transcript up to that epoch still fetches and still opens,
	// which is what Spec C screen 10's read-only variant renders. The only way back in is to be
	// added again, which is a new leaf and a new epoch.
	//
	// AND WHAT THAT ONE REPAIR COSTS IS MEASURED RATHER THAN LEFT AS A SENTENCE, because a sentinel
	// that names exactly one road owes the price of it. Driven by
	// TestTheOnlyRepairTheRemovedSentinelNamesIsBeingAddedBackAndItCostsThreeWalksAndTheHistory,
	// over both leaves a re-add can land on -- RFC 9420 §7.7 refills the leftmost BLANK leaf, so a
	// device added straight back takes its OWN old leaf and one admitted after a newcomer takes the
	// next -- and the cost is the same in both, which is the finding: it is the EPOCH's and not the
	// handle's.
	//
	//   - THE STATE GOES. [Device.Join] builds a fresh [Group] at the epoch the welcome names and
	//     rewrites part ten with it, so [Group.Removal] answers (0, nil) and the disk agrees.
	//   - BOTH DOORS ARE STILL SHUT, answering [ErrStreamFloorUnheld] and not this: a re-add lands
	//     where a previous occupant may have sealed, so one clean walk is owed before a seal. That
	//     is [Device.Join]'s standing price for any joiner above epoch one, not a removal's.
	//   - AND ITS OWN PRE-REMOVAL LINES ARE LOST, at [maxRecordAttempts] failed walks EACH. A
	//     re-added device holds state from its admission on, so a record it wrote ITSELF below that
	//     does not authenticate; the walk cannot tell that from a transient, spends its attempts,
	//     answers [ErrRecordAbandoned], counts [Stats.Unopened] and resolves the cursor past it.
	//     Nothing later repairs it. What the removed device could still read is exactly what the
	//     re-added device cannot.
	//
	// AND THE PRECEDENCE AGAINST THE HALT IS DECIDED BY THE COMMIT'S VALIDITY, not by which field
	// is read first. [Group.ingestCommitLocked]'s step (3a) refuses an unrotated removal BEFORE
	// ApplyCommit, so a removal this device judges INVALID halts it and never reaches this state --
	// correctly, because a commit this device refused did not remove it from anything. This state is
	// reachable only from the one arm where mls has applied the commit to its own tree and answered
	// that this client is no longer in the group.
	ErrRemovedFromGroup = errors.New("urmessage: a commit this group received removed this device from the group: it is a VALID commit, not one this device refused and not a wrap that did not arrive, and this device is not a member any more -- it holds the keys of the epoch it was removed at, so the history up to that epoch still reads, and it can neither follow anything above it nor send again in this group until it is added back")

	// The role model refused a commit, on either arm: MASTER §11's "refused by the committing
	// client, and rejected by every receiving client on validation". On RECEIPT it is an ingested
	// commit this device would not follow (ledger item 242's R1, [authorizeCommit] on every
	// commit before ApplyCommit); on SEND it is a commit this device was asked to build and
	// refused before building it (R2, the same predicate over the value the commit would
	// produce, and ruling 15's caller check in [Group.SetRole]). The rule that refused it is
	// carried, as one of the sentinels below, mls's own, or the cause a configured
	// [CommitAuthorizer] returned -- so a caller can errors.Is this AND the rule.
	//
	// ON RECEIPT IT IS NOT STICKY, AND THAT IS TWO FACTS RATHER THAN ONE. Measured walk by walk by
	// TestARoleModelRefusalIsNeitherTheRemovalNorTheHaltAndTheWalksSayWhich: walk one answers this
	// wrapping the rule; walk two answers [ErrCommitIngest] over `ratchet generation already
	// consumed`, because step (0) of [Group.ingestCommitLocked] has spent the committer's ladder by
	// then and the sentinel cannot be re-derived; walk three spends [maxRecordAttempts], answers
	// [ErrRecordAbandoned] and resolves the cursor PAST the commit; and every walk after it answers
	// nil.
	//
	//   - THE FIRST FACT IS CORRECT AND IS RULING 41 READ LITERALLY: it is NOT [Group.removed]. A
	//     commit this device refused removed it from nothing -- its leaf is in the tree it is
	//     standing in and it is still a member at n -- and step (3) takes the refusal before
	//     ApplyCommit, so the removed arm is not reachable from it at all.
	//   - THE SECOND IS A RESIDUAL AND IS NAMED HERE RATHER THAN DRESSED UP: it is not
	//     [Group.halted] either. That field is [ErrRemovalWithoutRotation] and nothing else, and the
	//     2026-09-24 repair that made it sticky and persisted was made for exactly this sequence one
	//     refusal site along. Extending it here needs a second persisted kind, a projection and a
	//     ruling on whether an unauthorized commit and an unrotated removal are one state or two, so
	//     it is ruling 41's track. What it costs in the field is item 242's own accounting and is
	//     not new: the server accepted the commit and moved current_epoch, so a refusing receiver is
	//     stale, cannot write, and recovers by re-founding.
	ErrCommitUnauthorized = errors.New("urmessage: the role model refused this commit: not built on the send side, not followed on receipt")

	// ── the role model's rules, MASTER §11 and ledger item 242 (roles.go) ──────────────────
	//
	// One sentinel per rule, so that a refusal names the rule and a test can hold each rule
	// apart from its neighbours. Three rules answer connect/mls's own sentinels instead and are
	// not redeclared here: R3 is mls.ErrAdminRemovedByNonOwner, and the two caps are
	// mls.ErrGroupSizeExceeded and mls.ErrDeviceLimitExceeded.

	// R0a: the commit leaves the group with no urmessage_group_policy, or with one that does not
	// parse or does not validate (two owners, no owner, a non canonical role list). The mls
	// reason is carried -- mls.ErrNoGroupPolicy for the absence.
	ErrCommitPolicyInvalid = errors.New("urmessage: the commit leaves the group without a valid urmessage_group_policy")

	// R0b: a group context extension other than 0xF001 -- 0x0003 required_capabilities above
	// all -- is not byte identical before and after the commit.
	ErrCommitExtensionChanged = errors.New("urmessage: the commit changes a group context extension a policy commit may not touch")

	// R0c: the post-commit policy names an identity that holds no leaf after the commit.
	ErrCommitPolicyPhantom = errors.New("urmessage: the post-commit policy names an identity with no leaf in the group")

	// R6a: an Add whose credential claims an identity already in the group, committed by anyone
	// but that identity. A second device is the identity's own to add.
	ErrCommitIdentityClaimed = errors.New("urmessage: an added leaf claims an identity already in the group and the committer is not that identity")

	// R6c: a leaf present before and after the commit changed its credential identity (an
	// Update, or the committer's own path), or the leaf sets before and after do not agree with
	// what the commit says it added and removed.
	ErrCommitIdentityChanged = errors.New("urmessage: a leaf's identity changed across the commit, or the membership change is not the one the commit declares")

	// R6d: a leaf of the post-commit tree carries no urmessage_leaf_keys extension (0xF002), so
	// no epoch wrap could reach it. Both send doors refuse such a key package; this is the
	// receiving side's twin of that refusal, over the seam's HasLeafKeys.
	ErrCommitLeafWithoutKeys = errors.New("urmessage: a leaf of the post-commit tree carries no urmessage_leaf_keys, so no epoch wrap could reach it")

	// R1: an Add of a NEW identity by a committer who is neither ADMIN nor OWNER (ruling 1).
	ErrCommitAddByNonAdmin = errors.New("urmessage: only an admin or the owner may add a new identity to the group")

	// R2: a Remove of a MEMBER's or OBSERVER's leaf, not the committer's own identity, by a
	// committer who is neither ADMIN nor OWNER.
	ErrCommitRemoveByNonAdmin = errors.New("urmessage: only an admin or the owner may remove another member")

	// R5: the owner changed and the committer is not the owner, the new owner held no leaf
	// before the commit (ruling 10) or holds none after it, or the outgoing owner is still
	// present and is not an ADMIN afterwards (ruling 4).
	ErrCommitOwnerTransfer = errors.New("urmessage: ownership may only be transferred by the owner, to a current member, and the outgoing owner becomes an admin")

	// R4: a change to the admin set -- any role to ADMIN, or ADMIN to MEMBER or OBSERVER -- by a
	// committer who is not the owner.
	ErrCommitRoleChangeByNonOwner = errors.New("urmessage: only the owner may change who is an admin")

	// R4: a MEMBER to OBSERVER or OBSERVER to MEMBER change, or a retention or disappearing
	// bucket change, by a committer who is neither ADMIN nor OWNER.
	ErrCommitPolicyChangeByNonAdmin = errors.New("urmessage: only an admin or the owner may change a member's role or the group's policy")

	// R4: a change to the policy's server_id -- the message server the group lives on (MASTER
	// §6), whose change is V2's group migration between hosts -- by a committer who is not the
	// owner. Its own value rather than the admin-set one so a refusal names what moved.
	ErrCommitServerIdChangeByNonOwner = errors.New("urmessage: only the owner may change the server the group lives on")

	// R7: a commit by a MEMBER or an OBSERVER that carries more than its own device leaves -- an
	// Update of another leaf carried by reference, a group context extension list other than the
	// one the group had -- since §11's table gives "commit epochs" to ADMIN and OWNER and ruling 5
	// gives an OBSERVER "its own device add / remove and nothing else". A bare, path-only commit
	// is NOT this: ruling 12 makes it the PCS self-heal every role may make.
	ErrCommitBeyondOwnDevices = errors.New("urmessage: a member or an observer may commit its own device leaves and nothing else")

	// A role name on a [CommitAuthorization] that is not one of the four this profile defines.
	// The ingest path never builds one; it is here so the pure rule function refuses rather
	// than guesses when handed a value it did not build.
	ErrCommitRoleUnknown = errors.New("urmessage: a role name on the commit authorization is not one this profile defines")

	// ── the committing arm's own refusals (R2, rolescommit.go) ─────────────────────────────
	//
	// A role refusal on the send side is NEVER one of these: it is [ErrCommitUnauthorized]
	// wrapping the rule above, the receivers' own sentence, because the send side judges by the
	// same predicate. These four name a REQUEST that is malformed before any rule is reached.

	// [Group.SetRole] was asked for "owner", for a name this profile does not define, or to
	// set the role of the identity that OWNS the group. Ownership moves through
	// [Group.TransferOwnership] and nothing else, because a transfer is two role changes in one
	// commit (the new owner up, the old owner to ADMIN, ruling 4): a SetRole to owner would
	// leave two owners for R0a to refuse, and a SetRole of the owner would leave none for a
	// member to be judged against.
	ErrRoleNotSettable = errors.New("urmessage: SetRole takes admin, member or observer, and never the owner's own role; ownership moves through TransferOwnership")

	// [Group.TransferOwnership] named the identity that already owns the group. It is refused by
	// name rather than built, because the policy it would build -- the same identity set to owner
	// and then to admin -- has no owner at all, and R0a's answer to that describes a broken
	// policy rather than a pointless request.
	ErrAlreadyOwner = errors.New("urmessage: that identity already owns this group")

	// [Group.RemoveMember] WAS ASKED FOR THIS DEVICE'S OWN IDENTITY, AND LEAVING IS NOT THIS VERB
	// (ledger item 257's rulings 11 and 48). The verb is keyed on an IDENTITY and removes every
	// leaf that identity holds, so the identity's own call always carries this device's own leaf --
	// and RFC 9420 §12.4 forbids a committer removing itself (mls.ErrRemoveCommitter), which is
	// ruling 11's "no identity's last leaf ever leaves in its own commit" enforced one layer down.
	//
	// IT IS REFUSED BY NAME AND BEFORE ANYTHING IS BUILT, and the reason is the SENTENCE rather
	// than the outcome. A naive path reaches the refusal anyway: the send-side predicate would
	// answer R6c's [ErrCommitIdentityChanged] -- "a leaf's identity changed across the commit" --
	// because the committer's own leaf is declared removed and still standing after. That is a
	// true sentence about a malformed commit and the WRONG sentence for somebody who pressed
	// Leave, which is the surface ruling 48 made product: a leave request the app states, with
	// mute-and-hide locally, and an admin's Remove as its MLS half. So the text names the door.
	//
	// IT IS NOT SPEC A §7.5's `RemoveDevice` EITHER, which ruling 50 put in its own track and
	// which nothing in this package declares: that verb is keyed on LEAVES, is one commit per
	// group the identity belongs to, and has a partial-success state machine. A member revoking
	// one of its OTHER devices is its business (§11's self-service rule, ruling 2) and it is not
	// reachable through an identity-keyed call. (§7.5 is "Devices"; the number here read §7.3 for
	// one commit, which is the section that declares `RemoveMember` -- the very verb this sentence
	// is distinguishing `RemoveDevice` from. See [Group.RemoveMember]'s own note on it.)
	ErrRemoveSelf = errors.New("urmessage: RemoveMember does not remove your own identity: no identity's last leaf ever leaves in its own commit, so ask an admin or the owner of this group to remove you")

	// [Group.RemoveMember] NAMED THE IDENTITY THAT OWNS THE GROUP, and no commit removes it
	// (ruling 11: "an OWNER's leaf is removed by nobody"). MASTER §11 states the product half --
	// "an owner must hand the group over before leaving. The leave action is refused for an OWNER
	// until ownership has been transferred to a current member" -- so the owner's own leaf leaves
	// in a Remove the NEW owner commits, by which time that identity is an ADMIN (ruling 4) and is
	// an ordinary subject of this verb.
	//
	// IT IS REFUSED BY NAME AND BEFORE THE PREDICATE, for [ErrRoleNotSettable]'s reason at
	// [Group.SetRole]: the policy this verb would build drops the named identity's entry, and
	// dropping the OWNER's leaves a policy with no owner, which mls's own encoder refuses as
	// [mls.ErrNoOwner] -- a sentence about a broken policy for what is a request at the wrong
	// door. R3 would also refuse the commit at every receiver when the committer is not the owner
	// ([mls.ErrAdminRemovedByNonOwner]); this answers the owner's OWN call as well, which R3
	// cannot, and it names the verb that does move ownership.
	ErrRemoveOwner = errors.New("urmessage: that identity owns this group, and an owner's leaf is removed by nobody: transfer ownership first with TransferOwnership, and the outgoing owner becomes an admin the new owner may remove")

	// [Group.RemoveMember] named an identity no leaf of this group carries. It is refused by name
	// before anything is built: with no leaf to remove the commit would carry no Remove proposal
	// at all, and the seam's own refusal for that (messagegroup.ErrEngineCommitRemoveEmpty) is a
	// sentence about an empty proposal vector where the caller asked about a person.
	ErrNoSuchMember = errors.New("urmessage: no leaf of this group carries that identity")

	// THIS DEVICE HOLDS OBSERVER IN THIS GROUP, SO IT MAY READ AND MAY NOT SEND (MASTER §11, spec
	// C §5.6, ledger item 242's R4). It is answered by [Group.Send], [Group.SendReply],
	// [Group.React], [Group.Unreact] and [Group.Delete] alike -- all four sendable kinds, which is
	// the whole askable set -- from the one clause in [Group.sendableLocked], and nothing is
	// sealed, no stream index is spent and no MLS generation is spent.
	//
	// ITS SENTENCE IS SPEC C'S OWN AND CARRIES NO CAVEAT, which is ruling 22: what a composer says
	// is about THIS app's behaviour, and after R4 "you can read this group but not send to it" is
	// true of this build unqualified. The caveat that belongs beside it -- someone who modifies
	// their app CAN still send, and this version cannot stop it at the server, only hide the
	// result -- is a fact about OTHER people's clients and belongs where the group is configured,
	// not above the box a person types in. [Stats.HiddenObserver] is what the hiding costs.
	//
	// IT IS NOT A SENDABILITY SURFACE AND R4 DELIBERATELY BUILDS NONE. Spec C requires only that
	// an app never infer sendability from a send FAILING, and no app has to: the role is readable
	// before the fact through [Group.MyRole] (and `urnet_message_group_my_role` over the abi).
	// A `CanSend`/`MessageSendability` vocabulary on top of that would be a second source of truth
	// for one question, naming states this build does not have (item 242's ruling 23).
	ErrObserverMayNotSend = errors.New("urmessage: you can read this group but not send to it")

	// A commit this device built and submitted LOST THE EPOCH RACE: the server answered
	// REASON_COMMIT_LOST or REASON_EPOCH_STALE to it, which is MASTER §9.3's delivery service
	// saying another commit closed this epoch first. Both reasons are answered only after
	// write_auth verified (spec B §4.5), so neither is a nonce fact and S2-2's recovery is not
	// spent on them.
	//
	// THE GROUP IS WHERE IT WAS. The staged epoch is erased through the seam's ClearPendingCommit,
	// the handle, the session and [Group.Epoch] all still stand at the epoch the commit was built
	// against, and [Group.Members] reads the policy that is live rather than the one that did not
	// land. What the caller owes is §9.3's other half: [Group.Receive] to follow the winner into the
	// next epoch, then the verb again, which re-derives against the winner. Before 2026-09-22 the
	// commit was merged BEFORE it was submitted, and the loser was left at a private epoch nobody
	// else entered -- unable to open the winner's commit or to seal a record the server would
	// take -- until the app restarted. It wraps [ErrSubmitRefused] too, so a caller reading "did
	// it land" sees the answer it always saw.
	ErrCommitLost = errors.New("urmessage: another commit closed this epoch first, so this one was not built on the group's current state; Receive to follow the winner, then retry")

	// The head this package writes, read back as something else.
	ErrHeadFormat = errors.New("urmessage: this record's head is not one this build wrote")

	// ── the content envelope ──────────────────────────────────────────────────────────────

	// THE SENDER BROKE A RULE THE KIND CODE ALONE DECIDES: a body too short for its layout,
	// trailing octets after a layout with no tail, an empty required tail, a kind of 0x00, or a
	// code outside the retention classes its range allows. It is spec A §7.4's "malformed", and
	// it is a statement about the octets rather than about this build's age -- which is what
	// makes it a different value from [ErrContentUnsupported].
	ErrContentMalformed = errors.New("urmessage: this record's application plaintext is not a content envelope this build can read")

	// A CODE THIS BUILD DOES NOT KNOW, on a class its range allows. It is NOT a failure: the
	// record keeps its position and its message_id, the walk continues, and it renders as one
	// closed placeholder. A future kind is not malformed, and the day spec A §7.4's closed set
	// grows the "unsupported" value owner choice 11 owes, this is what carries it.
	ErrContentUnsupported = errors.New("urmessage: this record carries a content kind this build does not know")

	// An unknown code on EPH(0), which is never persisted: dropped, with nothing to render and
	// no history for a gap to be a hole in. It is carried as an error so that a caller that
	// wanted to know can, and no walk in this build can reach it -- see [transientOnly].
	ErrContentDropped = errors.New("urmessage: this record is a transient nothing on this build would keep")

	// An emoji this package will not seal. See checkEmoji for exactly what is checked and for
	// the larger half that is NOT, which is open item M1-41.
	ErrEmojiRefused = errors.New("urmessage: this is not an emoji a reaction may carry")

	// A kind that names another message named one this group does not hold. It is raised on the
	// SEND side only: on the receive side a reaction or a tombstone whose target has not
	// arrived is HELD, because the walk's order is not the conversation's order.
	ErrNoSuchMessage = errors.New("urmessage: this group holds no message under that message_id")

	// ── the durable state store ───────────────────────────────────────────────────────────

	// The directory could not be opened, read, written or flushed. It is the store's "this
	// disk is not answering", and it is deliberately NOT the same value as a missing record:
	// J1-4 is that mls.StateStore gives its callers no way to tell those two apart, and
	// [Device.Restore] is a caller that must.
	ErrStateStoreState = errors.New("urmessage: the durable state store could not be read or written")

	// A second store over one directory. It is the one refusal that is about another process
	// rather than about this one.
	ErrStateStoreLocked = errors.New("urmessage: this state directory is already held by a single-writer exclusion")

	// A file under a name this store computes that is not a record this build wrote: the wrong
	// magic, the wrong version, the wrong kind, a checksum that does not match, or a record
	// whose own key octets are not the key that was asked for.
	ErrStateStoreFormat = errors.New("urmessage: this is not a state record this build wrote")

	// No such value. It is a sentinel and not a nil: "this device was never in that group" and
	// "the disk is broken" are two readings a restore has to branch on.
	ErrStateNotFound = errors.New("urmessage: this state store holds no such value")

	// The table of authenticated receiver-ladder heads ([PeerHead]) could not be written after a
	// walk that raised one. Nothing in THIS process is affected -- the heads are in memory -- and
	// what it costs is a restart: a device restored without them tracks a peer's ladder at the
	// head the disk last held, and a peer more than one window past that is silent for the rest
	// of the epoch. Ledger item 241. It is answered by [Group.Receive] after the walk's own
	// answer, and the write is tried again on the next walk.
	ErrPeerHeadsPersist = errors.New("urmessage: the receiver-ladder heads could not be persisted")

	// This device's own durable stream floor could not be raised past a stream index the server
	// already holds a claim at under this device's own sender_handle. Ledger item 245.
	//
	// IT IS NOT THE COLLISION AND IT IS THE ONE MOMENT BEFORE ONE. A newcomer that lands on a
	// removed member's leaf inherits that member's sender_handle byte for byte -- the derivation
	// takes no epoch and no identity -- so its first send would seal at an index the removed
	// member has already spent, be answered REASON_STREAM_INDEX_REUSED, and latch
	// [ErrIdentityInUse] for the life of the process. [Group.seedOwnStreamLocked] moves the floor
	// past those claims on the walk that sees them, and this is what it answers when it cannot:
	// every record of the walk is still delivered, and what is refused is the SENTENCE that this
	// group's next send would land. Unlike [ErrIdentityInUse] it is NOT sticky, because nothing
	// has gone wrong yet -- the next [Group.Receive] tries the floor again.
	ErrStreamFloor = errors.New("urmessage: this device's own stream floor could not be raised past another occupant's claims")

	// The store holds no device identity yet, which is the ordinary state of a fresh
	// directory and is what makes [NewDevice] mint one rather than refuse.
	ErrNoDeviceIdentity = errors.New("urmessage: this state store holds no device identity")

	// This device holds no X-Wing seed, so it cannot open an encapsulation addressed to the
	// leaf it publishes. TWO CAUSES, ONE REFUSAL: a state store written before S2-26 retained
	// the seed -- the deployed alpha's is one -- and a device that has been Closed, which
	// erases it. NAMED rather than answered as a wrong secret, because 32 zero octets are a
	// well formed X-Wing seed: decapsulating under them succeeds and returns 32 uniform
	// looking octets that open nothing, with no error anywhere to point at.
	ErrNoDeviceWrapKey = errors.New("urmessage: this device holds no x-wing seed for the leaf it publishes, so it cannot open an encapsulation addressed to that leaf")

	// THERE IS NO SENTINEL FOR "THIS MEMBER PUBLISHES NO WRAP KEY", and its absence is a
	// decision. [Group.MemberWrapKeys] refuses such a member rather than skipping it -- a
	// fan-out that silently left a member out is ledger item 132's undercount -- but the seam
	// it reads through already refuses a leaf with no urmessage_leaf_keys, and the body it
	// hands back was produced by `mls.LeafKeysExtension.Encode`, which refuses a wrong alg_id
	// and a wrong length. So a parse failure there is this build disagreeing with itself and
	// not a condition a caller can be in, and this file's own rule -- every refusal here exists
	// because its alternative is a silent zero -- does not admit a name for it. See
	// [ErrRestore] for the sentinel this corpus removed for the same reason.

	// ── restoring ─────────────────────────────────────────────────────────────────────────

	// [Device.Restore] was called on a device whose state store cannot persist an identity or
	// a group record, so there is nothing to restore FROM. Refused by name rather than
	// answering an empty slice, which reads exactly like "this device was in no groups".
	ErrNoDeviceStore = errors.New("urmessage: this device's state store is not durable, so there is nothing to restore; OpenDurableStateStore is the one this module ships")

	// A restored group could not be rebuilt. The cause is carried -- including
	// `messagegroup.ErrEngineLoadedEpoch`, which is the epoch mismatch this package used to refuse
	// itself and which now belongs to the engine door every caller of that interface goes through.
	//
	// THERE WAS A SECOND SENTINEL HERE AND IT IS GONE. It named the two methods a restored group
	// could not perform -- Process and ApplyCommit -- and it named its own cause: `connect`'s
	// GroupEngine had no LoadGroup, so this package carried a handle of its own that could not
	// write `messagegroup.EngineProcessed`'s unexported staged field. LoadGroup landed, the second
	// handle was deleted, and a sentinel for an impossibility that is no longer one is exactly the
	// shape this corpus keeps filing. It is REMOVED rather than retired in place.
	ErrRestore = errors.New("urmessage: this group could not be restored from the durable state store")

	// ── §4.3.4's fetch attestation, and §4.3.4's pagination ───────────────────────────────

	// The server advertised attestation support and then answered a fetch without one, or
	// with one that does not describe the page it came with. Both are downgrades and both are
	// detectable WITHOUT a key; the signature is not, and [Group.Receive] says why.
	ErrFetchAttestation = errors.New("urmessage: this fetch's attestation does not describe the page it came with")

	// The server answered a truncated page and then advanced no cursor, so paging cannot
	// terminate. Refused rather than looped.
	ErrFetchNoProgress = errors.New("urmessage: the message server answered an incomplete fetch page that advanced no cursor")

	// The page bound was reached with the server still saying there is more. The messages read
	// so far are returned WITH this error, never silently.
	ErrFetchIncomplete = errors.New("urmessage: the message server still has records for this group and this Receive stopped at its page bound")

	// The server answered a page it called COMPLETE and named a `high_water_record_id` above
	// every record it handed over. §4.3.4 makes that field the server's own statement of the
	// highest record it holds for this group, so a complete page that stops below it is the
	// server holding records back -- the one failure the AEAD cannot see, detectable with no
	// key and no attestation. Returned WITH whatever did arrive, never instead of it. See
	// [Group.Receive] for the one honest server that also produces it.
	ErrFetchOmitted = errors.New("urmessage: the message server answered a complete page and named a high water above every record it handed over")

	// A record that did not open has been re-fetched [maxRecordAttempts] times and is given up
	// on. It is named ONCE, here, rather than silently dropped: before this error existed the
	// cursor moved past a failed record on its first sight of it and no later fetch ever asked
	// for it again.
	ErrRecordAbandoned = errors.New("urmessage: a record from a member of this group did not open after every retry and is no longer being fetched")

	// ── one identity, two devices ─────────────────────────────────────────────────────────

	// A SECOND WRITER IS SEALING UNDER THIS DEVICE'S IDENTITY IN THIS GROUP, which is what a
	// COPY of the app-data folder produces: two devices at one leaf, one sender_handle and one
	// stream counter. Two records under one (epoch, sender_handle, stream_index) are one
	// record_key and one nonce, which spec A §5.6 calls a total break of both AEADs for that
	// record. STICKY: a group that has seen this refuses to seal again for the life of the
	// process, because the alternative is to go on producing the collision. See
	// [Group.Receive] for exactly what is detected, when, and what is NOT.
	ErrIdentityInUse = errors.New("urmessage: another device is sealing records under this device's identity in this group, so this group will not seal again")

	// ── reconnecting is not failing ───────────────────────────────────────────────────────

	// [Device.Connect]'s Hellos were NOT ANSWERED for the whole of its budget. It is "not yet",
	// and it is a different value from every other refusal here for exactly one reason: on the
	// deployed server a reconnecting client_id is not routed to for about sixty seconds
	// (measured; msgrepo docs/reports/2026-09-15-operator-and-connect-findings.md item 5), so
	// the ordinary state of a client that just woke up is this one. A caller that shows a user
	// "could not connect" here is telling them something false; the sentence is "reconnecting".
	//
	// IT IS NOT A CLAIM THAT THE WINDOW IS STILL OPEN. The budget bounds how long one call
	// blocks, nothing more, and the answer to this error is to call [Device.Connect] again.
	// A server that ANSWERS -- a refusal by reason, or a Hello carrying no nonce -- is
	// [ErrHelloRefused] or [ErrNotConnected] on the first attempt and is never this.
	ErrReconnecting = errors.New("urmessage: this device is reconnecting: the message server has not answered Hello yet, which is the ordinary state of a client_id that has just re-dialled")

	// A RESTORED GROUP HAS NOT YET COMPARED ITS STREAM POSITION AGAINST THE SERVER'S ROWS.
	// [Group.Receive] is what performs that comparison, and until it has run once this group
	// will not seal -- because the seal is the irreversible half: a copied folder that sends
	// before it listens has already produced the two-time pad whatever the server then does
	// with the record.
	ErrNotReconciled = errors.New("urmessage: this restored group has not reconciled its stream position against the server yet; Receive once before Send")

	// A GROUP THIS DEVICE JOINED HAS NOT YET HELD ITS OWN STREAM FLOOR AGAINST THE SERVER'S
	// CLAIMS. Ledger item 245's first piece, and the half that made it a GATE rather than a
	// repair that runs when it happens to run.
	//
	// A joiner lands on whatever leaf RFC 9420 section 7.7 gives it, which is the LEFTMOST BLANK
	// -- a leaf a removed member may have stood at. Its sender_handle is
	// SenderHandle(group_handle_key, leaf), so it inherits that member's sixteen octets byte for
	// byte, and the server holds a stream claim at every index that member spent. A first Send
	// with no walk behind it therefore seals at index 1 of a stream that is already spent to N,
	// is answered REASON_STREAM_INDEX_REUSED, and latches [ErrIdentityInUse] for the life of the
	// process: a member that has just joined can never send in the group it just joined.
	//
	// [Group.seedOwnStreamLocked] moves the floor past those claims ON THE FIRST WALK -- the
	// ruling's own words -- and this is what refuses a Send that would happen BEFORE that walk.
	// It is not sticky and it is not a diagnosis: one [Group.Receive] that completes cleanly
	// clears it, exactly as [ErrNotReconciled] is cleared, and a group FOUNDED in this process
	// never has it, because a group id drawn here has no claim under any handle of it.
	//
	// ── THE PRODUCT CONTRACT, AND TWO SENTENCES ABOUT IT THAT WERE FALSE ─────────────────────
	//
	// A CALLER THAT JOINS ABOVE EPOCH ONE MUST [Group.Receive] ONCE BEFORE ITS FIRST [Group.Send],
	// [Group.SetRole] or [Group.AddMemberAndPublish]. That is an obligation on every joiner and not
	// a fact about reused leaves, so it is written here as one.
	//
	// WHAT THIS MODULE'S OWN CALLERS DO, MEASURED AT LAST, because both earlier statements of it
	// were read off the files and both were false by the same method -- a count of Join SITES with a
	// property hung on it. The first was *"every Device.Join site in cp3b is an epoch-one join, so
	// nothing needed to change"*. The second was *"FOUR of cp3b's fourteen Join sites, and
	// liveprobe's, are above epoch one ... every one of those joiners RECEIVES before its first
	// Send"*. DRIVEN instead -- a probe on [Device.Join] and on BOTH write doors
	// ([Group.sendableLocked] and [Group.committableLocked]), over the whole suite, green:
	//
	//   - cp3b has FOURTEEN Device.Join sites, all fourteen run, and they run 103 times.
	//   - EIGHT of the fourteen run ABOVE EPOCH ONE, at epochs 2 to 34, and 46 of the 103 joins are:
	//     groupchat_test.go twice (both at 2), history_test.go's `hsAddAndJoin` helper -- ONE site,
	//     39 joins, every epoch from 2 to 34 -- lostrace_test.go three times (3, 4, 7),
	//     roles_test.go once (3), and streamfloorgate_test.go once (2), which is the site the second
	//     sentence's own commit added and did not count.
	//   - Of those 46, THIRTY-SEVEN never reach a write door or a [Group.Receive] at all, EIGHT
	//     Receive first, and exactly ONE reaches a write door with nothing behind it: the gate's own
	//     case, which requires this sentinel by name.
	//
	// SO THE TRUE PROPERTY IS NOT "EVERY ABOVE-EPOCH-ONE JOINER RECEIVES FIRST" -- that is false at
	// one site and vacuous at thirty-seven. It is: *no above-epoch-one joiner in this corpus reaches
	// a write door unreceived except the one case that asserts the refusal.* Which is still measured
	// rather than asserted -- with this gate in the build a joiner that wrote first would go red --
	// and it was driven at the roles site by putting that Send above that Receive. liveprobe's third
	// party is read and not run: it joins at epoch 2 (`C joined at epoch %d, want 2`) and drains
	// with a Receive before it sends.
	//
	// AND THERE IS NO LONGER A REFUSAL A Receive DOES NOT CLEAR, which is the other false sentence.
	// It said that a record this group gave up on before reading its header left the refusal
	// standing for ever, in its own text, "so a caller is never told to retry what cannot succeed".
	// What it was actually doing was refusing EVERY group after EVERY restart on one unreadable row
	// anywhere in the history -- including groups on a leaf nobody else has ever stood at, where the
	// gate's subject does not arise. A row this build cannot parse now contributes §4.3.3's own
	// `sender_handle` and `stream_index` projection of itself
	// ([Group.noteUnparsedClaimLocked]), so this sentinel means exactly what its text says and
	// nothing else: Receive once before Send.
	ErrStreamFloorUnheld = errors.New("urmessage: this group was joined on a leaf that may carry a previous occupant's stream claims and its own floor has not been held against them yet; Receive once before Send")

	// [Device.ForgetGroup] was asked to leave a group this device does not hold: never joined,
	// already left, or closed under it.
	ErrGroupNotHeld = errors.New("urmessage: this device holds no such group")

	// [Device.ForgetGroup] LEFT the group -- it is closed, its keys are gone from memory, and this
	// device no longer holds it -- but the erase of its copy on the disk did not finish. The leave
	// mark stands, so the next [Device.Restore] finishes the erase, and so does ForgetGroup again.
	// [Device.Join] and [Device.CreateGroup] answer it too, for a group whose leave they could not
	// finish before writing anything of it.
	ErrForgetUnfinished = errors.New("urmessage: this device has left the group, but its copy on the disk could not be erased in full; ForgetGroup again, or the next Restore, finishes the erase")

	// [Device.ForgetGroup] was asked to leave a group this device OWNS, from its identity's last
	// leaf, while somebody else is in it. Nobody removes an owner's leaf (ruling 11), so leaving
	// would strand every other member with an owner gone for good -- who could never be removed,
	// or added back. MASTER §11: "the leave action is refused for an OWNER until ownership has been
	// transferred". Nothing changed: the group is open, and every file of it is still on the disk.
	ErrOwnerMustTransfer = errors.New("urmessage: you own this group and somebody else is in it, so hand it over first with TransferOwnership; leaving now would leave them with an owner nobody can remove")
)
