// The role model's committing arm: MASTER §11's "refused by the committing client", ledger item
// 242's R2, 2026-09-21. R1 (roles.go, the receiving arm) is the other half.
//
// THE SAME PREDICATE, CALLED BEFORE THE COMMIT EXISTS. §11 rules that a bad commit "is refused by
// the committing client, and is rejected by every receiving client on validation", and a build in
// which the two arms could disagree would let this device build a commit its peers refuse -- which
// is the halt item 242's cost paragraph describes, caused by an honest client. So there is no second
// rule set here: every verb below builds the [CommitAuthorization] the commit WOULD produce
// ([Group.outgoingAuthorizationLocked]), hands it to [authorizeCommit], the one pure function the
// receivers judge by, and on a refusal answers [ErrCommitUnauthorized] wrapping the rule -- the
// receivers' own sentence -- having built, merged and published nothing. The refusal is counted in
// [Stats.CommitRefusedOwn].
//
// WHAT THE SEND SIDE KNOWS THAT THE RECEIVER READS OFF THE STAGED COMMIT, and how each is derived so
// the two decisions are the same value: the committer is this device's own leaf as the PRE-commit
// tree carries it (the seam's OwnLeafIndex, the identity MemberAt answers there, the role the live
// policy gives it); the post-commit extension list is mls.ExtensionsWithGroupPolicy over the live
// list, which is the one helper the seam's CommitPolicy itself uses, so the list judged is the list
// the commit will install; and the post-commit membership is the live membership minus the removes,
// plus one leaf per key package at the leftmost blank leaf (RFC 9420 §12.1.1, the placement mls
// makes), each carrying the identity its credential claims and whether its leaf node carries
// urmessage_leaf_keys, read by the same mls.LeafKeysOf both send doors refuse with. The test that
// holds the two arms to one value builds the commit after the decision, processes it at a receiver,
// and compares the receiver's decision field for field.
//
// THE VERBS. [Group.AddMemberAndPublish] (group.go) is gated on it for ruling 1; [Group.SetRole]
// and [Group.TransferOwnership] are the two policy commits §11's table names, each one CommitPolicy
// through the seam, merged and published down the road the add already walks; and
// [Group.RemoveMember] is the removal track's product verb (ledger item 258, ruling 49), one
// CommitRemoveWithExtensions carrying every leaf the named identity holds AND the policy that drops
// its entry, down the same road.
//
// REMOVAL IS WHERE THIS FILE'S SEND-SIDE DERIVATION STOPPED BEING UNDRIVEN. Item 242's R2 filed the
// gap in its own words -- "a send-side test of R2/R3/R6a/caps over `outgoingCommit{removeLeaves}`,
// which no verb reaches yet" -- and the field has been on [outgoingCommit] since R2 with nothing
// writing it. [Group.RemoveMember] is the first writer, so R2 ("a member or an observer may not
// remove") and R3 ("only the owner may remove an admin") are decided on this arm for the first time
// from a product call rather than from the pure table.
//
// THE READ SURFACE. [Group.Members] and [Group.MyRole] are the roles as this device reads them,
// under the live policy with an unnamed identity a MEMBER (ruling 8): what a roster shows, and what
// this device is about to be judged as. The cgo and Windows legs of the surface are R3's.
//
// NOTHING HERE COMMITS BY REFERENCE (ruling 13): a fold of cached proposals would let another
// member's cached Add claiming the owner's identity ride this device's commit under its authority,
// so every commit this package builds is one of the seam's by-value arms, and a source test over the
// package's production files holds it to that.
package urmessage

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/connect/mls/syntax"
)

// ── the read surface ─────────────────────────────────────────────────────────────────────────

// Member is one member of a group as a caller reads it: the leaf a role is read at, the
// sender_handle its records carry, the credential identity the policy keys the role by, that role,
// and whether the leaf is this device's own.
type Member struct {
	// LeafIndex is the member's leaf in the ratchet tree.
	LeafIndex uint32

	// SenderHandle is the 16 octets this member's records carry, derived from the group_handle_key
	// and the leaf. A copy.
	SenderHandle []byte

	// IdentityPub is the credential identity the leaf carries -- the member's Ed25519 identity
	// public key, which urmessage_group_policy keys a role by. A copy. One identity may hold
	// several leaves (its devices) and then appears once per leaf, with the same role on each.
	IdentityPub []byte

	// Role is the role the live policy gives IdentityPub, as the wire stable name: "owner",
	// "admin", "member" or "observer". An identity the policy does not name is "member" (MASTER
	// §11, ruling 8), and so is every member of a group whose context carries no readable policy.
	Role string

	// Mine is whether the leaf is this device's own.
	Mine bool
}

// Members is this group's membership at its current epoch, in leaf order, each with the role the
// live policy gives it. It is the roster a UI shows and it is exactly what this device would be
// judged by were it to commit now: the same membership door and the same policy reading the
// committing arm builds its decision from.
func (self *Group) Members() ([]Member, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.closed {
		return nil, fmt.Errorf("urmessage: this group is closed")
	}
	return self.membersLocked()
}

// MyRole is the role the live policy gives this device's own identity: what this device may
// commit. It is read at this device's own leaf -- the seam's OwnLeafIndex -- and not off the
// device's identity, so that what it answers is the role the tree's own credential at that leaf
// holds, which is the reading every receiver takes of a commit this device makes.
func (self *Group) MyRole() (string, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.closed {
		return "", fmt.Errorf("urmessage: this group is closed")
	}
	members, err := self.membersLocked()
	if err != nil {
		return "", err
	}
	for _, member := range members {
		if member.Mine {
			return member.Role, nil
		}
	}
	return "", fmt.Errorf("urmessage: this device's leaf %d is not among the group's %d members",
		self.handle.OwnLeafIndex(), len(members))
}

// membersLocked is [Group.Members] under the lock: the pre-commit membership the receiving arm
// reads, projected onto [Member] with the live policy's roles and this device's leaf marked.
func (self *Group) membersLocked() ([]Member, error) {
	live, err := self.liveContextLocked()
	if err != nil {
		return nil, err
	}
	members, err := self.membershipLocked(live.policy)
	if err != nil {
		return nil, err
	}
	own := self.handle.OwnLeafIndex()
	out := make([]Member, 0, len(members))
	for _, member := range members {
		out = append(out, Member{
			LeafIndex:    member.Leaf,
			SenderHandle: member.SenderHandle,
			IdentityPub:  member.IdentityPub,
			Role:         member.Role,
			Mine:         member.Leaf == own,
		})
	}
	return out, nil
}

// liveContext is the group context at this group's current epoch as the two arms read it: the
// full extension list, and the urmessage_group_policy decoded out of it -- nil, with the reason
// in policyErr, when the list carries none or one that does not parse or validate, which is
// exactly how the receiving arm reads the pre-commit side ([Group.commitAuthorizationLocked]): a
// nil policy names nobody, so every member is a MEMBER.
type liveContext struct {
	extensions []messagegroup.ExtensionBytes
	policy     *mls.GroupPolicyExtension
	policyErr  error
}

// liveContextLocked reads the [liveContext]. The one error it answers is the context's own
// failing to decode; a missing or invalid policy is a fact about the group, carried in the value.
func (self *Group) liveContextLocked() (*liveContext, error) {
	extensions, err := self.contextExtensionsLocked()
	if err != nil {
		return nil, err
	}
	policy, policyErr := mls.GroupPolicyOf(mlsExtensionsOf(extensions))
	return &liveContext{extensions: extensions, policy: policy, policyErr: policyErr}, nil
}

// ── the send-side decision ───────────────────────────────────────────────────────────────────

// outgoingCommit is what a commit this device is about to build WOULD do: the Adds it carries as
// the encoded key packages, the leaves it removes, and the policy body it installs -- nil for a
// commit that keeps the list the group has. It is the send side's spelling of the three vectors
// and the post-commit list [messagegroup.EngineProcessed] reports for an ingested commit.
//
// removeLeaves IS WRITTEN BY [Group.RemoveMember] AND BY NOTHING ELSE, and it stood here unwritten
// from R2 (2026-09-22) until ledger item 258 -- on the shape so that the decision built here was
// the receiver's WHOLE decision and not two thirds of it, and therefore judged by rules nothing
// drove from a verb. One verb writes it now, and it writes removeLeaves and policy TOGETHER: a
// removal that left the identity's policy entry standing would be refused by R0c, on this side, as
// a phantom.
type outgoingCommit struct {
	addKeyPackages [][]byte
	removeLeaves   []uint32
	policy         []byte
}

// authorizeOutgoingLocked is the committing-client decision on a commit this device is about to
// build: [authorizeCommit] over the value the commit would produce, then this device's configured
// [CommitAuthorizer] for the reason the receiving arm runs it -- a commit this device's own
// product would refuse on receipt is one it must not build. A refusal is counted in
// [Stats.CommitRefusedOwn] and answered as [ErrCommitUnauthorized] wrapping the rule, exactly as a
// receiver would answer the same commit, and the caller builds nothing.
//
// IT ANSWERS THE DECISION IT TOOK, for the reason [Group.authorizeCommitLocked] answers its own on
// the other arm: a caller that needs a field of the value it has just judged reads it off THAT
// value rather than deriving it a second time. [Group.RemoveMember] is that caller -- the
// post-commit extension list it hands the seam IS [CommitAuthorization.ExtensionsAfter], so the
// list the commit installs and the list the rules ran over are one value and cannot come to
// disagree. [Group.AddMemberAndPublish] and the two policy verbs want only the refusal.
func (self *Group) authorizeOutgoingLocked(intent *outgoingCommit) (*CommitAuthorization, error) {
	decision, err := self.outgoingAuthorizationLocked(intent)
	if err != nil {
		return nil, err
	}
	if err := authorizeCommit(decision); err != nil {
		self.stats.CommitRefusedOwn += 1
		return nil, fmt.Errorf("%w: %w", ErrCommitUnauthorized, err)
	}
	if authorizer := self.device.commitAuthorizer; authorizer != nil {
		if err := authorizer(decision); err != nil {
			self.stats.CommitRefusedOwn += 1
			return nil, fmt.Errorf("%w: %w", ErrCommitUnauthorized, err)
		}
	}
	return decision, nil
}

// outgoingAuthorizationLocked builds the [CommitAuthorization] a commit doing what intent says
// would produce at every receiver, from what this device holds BEFORE building it. Every field is
// derived the way the file header says, and every slice is this value's own.
func (self *Group) outgoingAuthorizationLocked(intent *outgoingCommit) (*CommitAuthorization, error) {
	live, err := self.liveContextLocked()
	if err != nil {
		return nil, err
	}
	extensionsBefore, policyBefore, policyBeforeErr := live.extensions, live.policy, live.policyErr
	members, err := self.membershipLocked(policyBefore)
	if err != nil {
		return nil, err
	}

	// the committer: this device's own leaf, as the pre-commit tree carries it
	ownLeaf := self.handle.OwnLeafIndex()
	var committer *CommitMember
	for at := range members {
		if members[at].Leaf == ownLeaf {
			committer = &members[at]
		}
	}
	if committer == nil {
		return nil, fmt.Errorf("urmessage: this device's leaf %d is not among the group's %d members", ownLeaf, len(members))
	}

	// the post-commit extension list: the seam's own replacement over the live list, so what is
	// judged is what CommitPolicy installs -- 0xF001 replaced in its position and every other
	// entry kept -- or the live list itself for a commit that carries no policy
	extensionsAfter := cloneExtensionBytes(extensionsBefore)
	if intent.policy != nil {
		replaced, err := mls.ExtensionsWithGroupPolicy(mlsExtensionsOf(extensionsBefore), intent.policy)
		if err != nil {
			return nil, fmt.Errorf("urmessage: the policy this commit would install: %w", err)
		}
		extensionsAfter = seamExtensionsOf(replaced)
	}
	policyAfter, policyAfterErr := mls.GroupPolicyOf(mlsExtensionsOf(extensionsAfter))

	// the post-commit membership: every kept leaf under the post-commit policy, then one leaf per
	// key package at the leftmost blank leaf, which is where mls places an Add after the removes
	// have blanked theirs (RFC 9420 §12.1.1; apply_proposals.go applies Adds last)
	removed := leafSetOf(intent.removeLeaves)
	occupied := map[uint32]bool{}
	after := make([]CommitMember, 0, len(members)+len(intent.addKeyPackages))
	for _, member := range members {
		if removed[member.Leaf] {
			continue
		}
		occupied[member.Leaf] = true
		after = append(after, self.commitMemberLocked(member.Leaf, member.IdentityPub, member.HasLeafKeys, policyAfter))
	}
	added := make([]uint32, 0, len(intent.addKeyPackages))
	next := uint32(0)
	for at, encoded := range intent.addKeyPackages {
		var keyPackage mls.KeyPackage
		if err := syntax.Unmarshal(encoded, &keyPackage); err != nil {
			return nil, fmt.Errorf("urmessage: key package %d of %d does not decode: %w", at, len(intent.addKeyPackages), err)
		}
		for occupied[next] {
			next += 1
		}
		occupied[next] = true
		_, keysErr := mls.LeafKeysOf(&keyPackage.LeafNode)
		after = append(after, self.commitMemberLocked(next, keyPackage.LeafNode.Credential.Identity, keysErr == nil, policyAfter))
		added = append(added, next)
	}
	slices.SortFunc(after, func(a CommitMember, b CommitMember) int {
		return cmp.Compare(a.Leaf, b.Leaf)
	})

	return &CommitAuthorization{
		GroupId:           append([]byte(nil), self.id...),
		Epoch:             self.epoch + 1,
		CommitterLeaf:     ownLeaf,
		CommitterIdentity: append([]byte(nil), committer.IdentityPub...),
		CommitterRole:     committer.Role,
		AddedLeaves:       added,
		RemovedLeaves:     append([]uint32(nil), intent.removeLeaves...),
		UpdatedLeaves:     []uint32{},
		Members:           members,
		MembersAfter:      after,
		PolicyBefore:      policyBefore,
		PolicyAfter:       policyAfter,
		PolicyBeforeErr:   policyBeforeErr,
		PolicyAfterErr:    policyAfterErr,
		ExtensionsBefore:  extensionsBefore,
		ExtensionsAfter:   extensionsAfter,
	}, nil
}

// seamExtensionsOf is an mls extension list as the seam spells it, every body cloned: the inverse
// of [mlsExtensionsOf], for the one place this package takes a list back from mls's own helper.
func seamExtensionsOf(extensions []mls.Extension) []messagegroup.ExtensionBytes {
	out := make([]messagegroup.ExtensionBytes, 0, len(extensions))
	for _, extension := range extensions {
		out = append(out, messagegroup.ExtensionBytes{
			Type: uint16(extension.ExtensionType),
			Data: append([]byte(nil), extension.ExtensionData...),
		})
	}
	return out
}

// ── the two policy verbs ─────────────────────────────────────────────────────────────────────

// SetRole makes one identity an "admin", a "member" or an "observer" in one commit, and publishes
// the epoch it opens: the live policy with that one entry set, canonicalized, committed by value
// through the seam's CommitPolicy so that 0x0003 required_capabilities and every other entry
// survive (item 242's P4), and published down the road [Group.AddMemberAndPublish] walks -- merged
// only once the server has taken it, so a lost epoch race answers [ErrCommitLost] and moves nothing.
//
// IT IS AN ADMIN'S OR THE OWNER'S VERB, WHATEVER THE DELTA (ruling 15, 2026-09-22), and that is
// decided here, BEFORE the predicate runs and off the same membership reading every receiver
// would take of this device's commit. The predicate alone could not hold it: a NAMED member
// calling SetRole(self, <its current role>) built a policy commit that changed no entry, which the
// predicate cannot tell from ruling 12's path-only self-heal, so a non-admin moved the epoch
// through a public verb -- while an UNNAMED member making the same call was refused by R7 for
// adding an entry. The verb's answer must not depend on whether the caller was ever named: a
// caller that is neither ADMIN nor OWNER is refused with R4's own sentence,
// [ErrCommitUnauthorized] wrapping [ErrCommitPolicyChangeByNonAdmin], counted in
// [Stats.CommitRefusedOwn], with nothing built.
//
// AND A CALL THAT NAMES THE ROLE THE IDENTITY ALREADY HOLDS IS A NO-OP: nil, nothing committed,
// nothing published, the epoch where it was everywhere -- the second half of ruling 15. "Already
// holds" is read off the live membership under the live policy with an unnamed identity a MEMBER
// (ruling 8), so an owner naming an unnamed member "member" moves nothing rather than bumping the
// epoch to write an entry that changes no role. An identity that holds NO leaf is not read as an
// unnamed member: it falls through to the predicate, which refuses it as a phantom (R0c).
//
// WHAT AN ADMIN MAY SET, is still the predicate's to say: §11's table gives the OWNER the admin
// set and an ADMIN "set MEMBER/OBSERVER", so any role to admin or admin to anything is the
// owner's (R4, [ErrCommitRoleChangeByNonOwner]) and member to observer and back is an admin's or
// the owner's, and each refusal is [ErrCommitUnauthorized] wrapping the rule with nothing built.
//
// "owner" IS NOT A ROLE THIS SETS, AND THE OWNER IS NOT AN IDENTITY THIS SETS: both are
// [ErrRoleNotSettable], and [Group.TransferOwnership] is the door. The second is refused by name
// because the policy it would build has no owner, and the encoder's refusal of that describes a
// broken policy rather than a request at the wrong door.
func (self *Group) SetRole(ctx context.Context, identityPub []byte, role string) error {
	wanted, err := mls.ParseRole(role)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRoleNotSettable, err)
	}
	if wanted == mls.RoleOwner {
		return fmt.Errorf("%w: %q", ErrRoleNotSettable, role)
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if err := self.committableLocked(); err != nil {
		return err
	}
	// ruling 15, first half: the caller's role, before anything is built or judged
	members, err := self.membersLocked()
	if err != nil {
		return err
	}
	var mine *Member
	var subject *Member
	for at := range members {
		if members[at].Mine {
			mine = &members[at]
		}
		if subject == nil && bytes.Equal(members[at].IdentityPub, identityPub) {
			subject = &members[at]
		}
	}
	if mine == nil {
		return fmt.Errorf("urmessage: this device's leaf %d is not among the group's %d members",
			self.handle.OwnLeafIndex(), len(members))
	}
	callerRole, err := roleNamed(mine.Role)
	if err != nil {
		return err
	}
	if !isAdminOrOwner(callerRole) {
		self.stats.CommitRefusedOwn += 1
		return fmt.Errorf("%w: %w: SetRole is an admin's or the owner's verb and this device is a %s",
			ErrCommitUnauthorized, ErrCommitPolicyChangeByNonAdmin, mine.Role)
	}
	// the OWNER's role is not this verb's to set either: any role for the owner leaves the
	// group ownerless, which the policy encoder refuses as "no owner" -- a sentence about a
	// broken policy for what is a request naming the wrong door. Refused by name, after the
	// caller check so that a member is still answered R4 and not this.
	if subject != nil && subject.Role == mls.RoleOwner.String() {
		return fmt.Errorf("%w: %x owns this group", ErrRoleNotSettable, identityPub)
	}
	// ruling 15, second half: the role the identity already holds is a no-op, not an epoch
	if subject != nil && subject.Role == wanted.String() {
		return nil
	}
	policy, err := self.editablePolicyLocked()
	if err != nil {
		return err
	}
	policy.SetRole(identityPub, wanted)
	body, err := policyBodyOf(policy)
	if err != nil {
		return err
	}
	return self.commitPolicyAndPublishLocked(ctx, body)
}

// TransferOwnership makes one identity the OWNER and this device's identity -- the outgoing owner
// -- an ADMIN, in one commit (§11: "the outgoing owner becomes an ADMIN", ruling 4), and publishes
// the epoch it opens. The new owner must hold a leaf BEFORE the commit (ruling 10): "ownership
// transfers only to a current member", and R5 refuses a stranger. Only the owner may call this
// with effect -- R5's [ErrCommitOwnerTransfer] answers everybody else, with nothing built -- and
// naming the identity that already owns the group is [ErrAlreadyOwner].
func (self *Group) TransferOwnership(ctx context.Context, identityPub []byte) error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if err := self.committableLocked(); err != nil {
		return err
	}
	policy, err := self.editablePolicyLocked()
	if err != nil {
		return err
	}
	ownerBefore, named := policy.OwnerId()
	if !named {
		// unreachable past editablePolicyLocked, whose decode validated exactly one owner
		return fmt.Errorf("urmessage: this group's policy names no owner: %w", mls.ErrNoOwner)
	}
	if bytes.Equal(ownerBefore, identityPub) {
		return ErrAlreadyOwner
	}
	policy.SetRole(identityPub, mls.RoleOwner)
	policy.SetRole(ownerBefore, mls.RoleAdmin)
	body, err := policyBodyOf(policy)
	if err != nil {
		return err
	}
	return self.commitPolicyAndPublishLocked(ctx, body)
}

// ── the removal verb ─────────────────────────────────────────────────────────────────────────

// RemoveMember takes one identity out of this group -- every leaf it holds and its entry in the
// policy, in ONE commit -- and publishes the epoch that commit opens. It is the removal track's
// product verb (ledger item 258, ruling 49: "one identity-keyed call removes ALL of that identity's
// leaves in one commit"), and its shape is [Group.SetRole]'s and [Group.TransferOwnership]'s so
// that cgo projects it through the URNET_MESSAGE_COMMIT_* kinds those two established.
//
// ONE CALL, EVERY LEAF. An identity may hold up to ten device leaves (§11, ruling 7) and removing
// some of them is not removing the member: the ones left standing still read every epoch and still
// write. So the leaves are found off the MEMBERSHIP -- the same roster [Group.Members] answers and
// the same one every receiver reads the commit against -- and they go into one Remove vector. A
// per-leaf verb is Spec A §7.3's `RemoveDevice`, which ruling 50 put in its own track after this
// one ships and which nothing in this package declares: that verb is keyed on leaves, runs once per
// group the identity belongs to, and owes a partial-success state machine. This one is keyed on an
// identity in one group and has no partial state, because one commit either lands or does not.
//
// AND THE POLICY LEAVES WITH THE TREE, WHICH IS WHY THIS VERB IS NOT A CommitRemove. MASTER §6's
// urmessage_group_policy is keyed by credential identity and nothing ever drops an entry, so a bare
// Remove of the last leaf of an identity any SetRole has NAMED leaves the group naming an identity
// with no leaf -- R0c's phantom, refused by every honest receiver, which is exactly how the OWNER
// was unable to remove an ADMIN before this (item 242's R2 filed it). So the live policy's entry for
// that identity is dropped, the full post-commit extension list is built the way [Group.SetRole]
// builds it -- mls's own ExtensionsWithGroupPolicy over the live list, 0xF001 replaced in place and
// 0x0003 required_capabilities kept -- and the Remove and the GroupContextExtensions ride ONE commit
// through the seam's CommitRemoveWithExtensions, whose proposal order is fixed inside it (ruling
// 51). THE LIST HANDED TO THE SEAM IS THE LIST THE RULES RAN OVER: it is read off the decision
// [Group.authorizeOutgoingLocked] answered rather than derived a second time, so an edit cannot make
// the judged list and the installed list two values.
//
// THE REFUSALS ARE THE RECEIVERS' OWN AND THEY LAND BEFORE ANYTHING IS BUILT. The send-side
// decision is [authorizeCommit] over the value this commit WOULD produce, so a call this device's
// peers would refuse is one it does not make: R2 answers a MEMBER's or an OBSERVER's attempt
// ([ErrCommitRemoveByNonAdmin]), R3 answers anybody but the owner removing an ADMIN
// ([mls.ErrAdminRemovedByNonOwner]), R0c answers a policy left naming the departed, and ruling 7's
// caps are judged over the post-commit tree. Each is [ErrCommitUnauthorized] wrapping the rule,
// counted once in [Stats.CommitRefusedOwn], with no commit staged, no record sealed and no round
// trip spent. R6a is not reachable from here and saying so is the honest half: it judges ADDED
// leaves and a removal declares none.
//
// THREE REQUESTS ARE REFUSED BY NAME INSTEAD, IN THIS ORDER, each because the predicate's sentence
// would be about something other than what was asked, and each true whoever asks. [ErrNoSuchMember]:
// an identity with no leaf would build a commit with no Remove proposal, which is the seam's refusal
// about a vector rather than an answer about a person. [ErrRemoveOwner]: an owner's leaf is removed
// by nobody (ruling 11), and the policy this verb would build has no owner for mls to encode.
// [ErrRemoveSelf]: leaving is not this verb (rulings 11 and 48), and the naive path surfaces R6c's
// "a leaf's identity changed across the commit" to somebody who pressed Leave.
//
// THE OWNER'S COMES BEFORE THE SELF ONE, which is the whole reason there are two: an OWNER pressing
// Leave must be told to hand the group over, not to ask an admin. MASTER §11 rules it -- "the leave
// action is refused for an OWNER until ownership has been transferred to a current member; the
// client offers the transfer in the same flow rather than reporting a bare failure" -- and
// [ErrRemoveSelf]'s sentence is the wrong one for it.
//
// AND THE ORDER OF THOSE THREE AGAINST THE PREDICATE IS NOT [Group.SetRole]'s, deliberately. SetRole
// answers its caller check FIRST, "so that a member is still answered R4 and not this", because the
// predicate CANNOT decide SetRole's case (ruling 15). Here the predicate decides every authority
// question and nothing is taken from it; what is answered first is the set of requests for which NO
// commit exists at all. A MEMBER asking to remove the OWNER is therefore answered [ErrRemoveOwner]
// and not R3: "nobody removes the owner, transfer first" is true and names the door, where "only the
// owner may remove an admin" would invite a member to think some admin could.
//
// WHAT THE EPOCH IT OPENS COSTS THE REMOVED MEMBER, which is the whole point of the verb.
// [Group.publishCommitLocked] draws a FRESH pq_secret for that epoch and fans it out to the leaves
// [Group.wrapTargetsAtLocked] enumerates -- the live tree minus the leaves the STAGED COMMIT
// removes, read off the seam's own [messagegroup.PendingEpoch] and not off anything this verb
// passes down (ruling 51). So the removed identity holds no pq_secret for the epoch its own removal
// opens, therefore no storage_root, therefore neither that epoch's read key nor its write key: it
// can neither follow the group nor write to it, and that denial survives an adversary who later
// breaks X25519 while holding an archive (item 243, ruling 42's clause (a)).
// TestTheRemovedMembersOwnRemovalOpensAnEpochItCannotDerive drives it from this verb with a
// survivor's agreement as the inline control.
func (self *Group) RemoveMember(ctx context.Context, identityPub []byte) error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if err := self.committableLocked(); err != nil {
		return err
	}
	members, err := self.membersLocked()
	if err != nil {
		return err
	}
	// EVERY LEAF THE NAMED IDENTITY HOLDS, in leaf order, with the role it holds and whether one of
	// those leaves is this device's own. The own-leaf reading is [Member.Mine] -- the LEAF and not
	// the device's stored identity -- because what must not go into the Remove vector is a leaf:
	// RFC 9420 §12.4 forbids a committer removing itself (mls.ErrRemoveCommitter), which is ruling
	// 11's "no identity's last leaf ever leaves in its own commit" one layer down.
	leaves := []uint32{}
	role := ""
	mine := false
	for _, member := range members {
		if !bytes.Equal(member.IdentityPub, identityPub) {
			continue
		}
		mine = mine || member.Mine
		role = member.Role
		leaves = append(leaves, member.LeafIndex)
	}
	// THE THREE BY-NAME REFUSALS, IN THIS ORDER, AND THE ORDER IS THE ARGUMENT. Each is about the
	// SUBJECT rather than about the caller's authority, so each is true whoever asks -- and the
	// owner's comes before the self one because an OWNER pressing Leave must be told to hand the
	// group over (MASTER §11: "the leave action is refused for an OWNER until ownership has been
	// transferred"), which is a different sentence from "ask an admin to remove you" and the only
	// one that is true for it.
	if len(leaves) == 0 {
		return fmt.Errorf("%w: %x", ErrNoSuchMember, identityPub)
	}
	if role == mls.RoleOwner.String() {
		return fmt.Errorf("%w: %x holds %d leaf/leaves", ErrRemoveOwner, identityPub, len(leaves))
	}
	if mine {
		return fmt.Errorf("%w: leaves %v include this device's own", ErrRemoveSelf, leaves)
	}
	// THE POLICY THE COMMIT INSTALLS: the live one with that identity's entry gone. RemoveRole is
	// a no-op for an identity the policy never named, which is every non-founder of every group
	// no SetRole has touched (item 242) -- so the list this builds is byte identical to the live
	// one for such a member, and the commit still carries it. One shape, and R0b holds the rest of
	// the list identical either way.
	policy, err := self.editablePolicyLocked()
	if err != nil {
		return err
	}
	policy.RemoveRole(identityPub)
	body, err := policyBodyOf(policy)
	if err != nil {
		return err
	}
	// THE DECISION OVER BOTH VECTORS AT ONCE, before the connection is consulted and before
	// anything is staged. removeLeaves and policy are set together for R0c's sake: judged apart,
	// the Remove would be authorized and the phantom would be somebody else's refusal.
	decision, err := self.authorizeOutgoingLocked(&outgoingCommit{removeLeaves: leaves, policy: body})
	if err != nil {
		return err
	}
	if err := self.rebindLocked(); err != nil {
		return err
	}
	commit, _, _, err := self.handle.CommitRemoveWithExtensions(leaves, decision.ExtensionsAfter)
	if err != nil {
		return fmt.Errorf("urmessage: CommitRemoveWithExtensions: %w", err)
	}
	return self.publishCommitLocked(ctx, commit)
}

// editablePolicyLocked is the live policy as a value the verbs may write into: freshly decoded off
// this group's own context, so nothing it shares is the epoch the group is running. A group whose
// context carries no valid policy has no policy to edit and is refused with the decode's reason:
// there is no owner for a change to be judged against, and a policy commit that installed one
// would be a transfer by an unnamed committer, which R5 refuses at every receiver anyway.
func (self *Group) editablePolicyLocked() (*mls.GroupPolicyExtension, error) {
	live, err := self.liveContextLocked()
	if err != nil {
		return nil, err
	}
	if live.policy == nil {
		return nil, fmt.Errorf("urmessage: this group's context carries no policy a role change could edit: %w", live.policyErr)
	}
	return live.policy, nil
}

// policyBodyOf is a policy canonicalized and encoded as the body CommitPolicy takes -- the octets
// under the 0xF001 tag -- through mls's own validating encoder, so a policy that would not
// validate is refused here with mls's reason rather than built.
func policyBodyOf(policy *mls.GroupPolicyExtension) ([]byte, error) {
	if err := policy.Canonicalize(); err != nil {
		return nil, fmt.Errorf("urmessage: the policy this commit would install: %w", err)
	}
	encoded, err := policy.Encode()
	if err != nil {
		return nil, fmt.Errorf("urmessage: the policy this commit would install: %w", err)
	}
	return encoded.ExtensionData, nil
}

// commitPolicyAndPublishLocked is the one road both policy verbs take: the send-side decision
// over the policy the commit would install, then the seam's by-value CommitPolicy, which STAGES
// the commit, and [Group.publishCommitLocked], which submits it and merges it only on the server's
// REASON_OK. The order is [Group.AddMemberAndPublish]'s: the decision before the connection is
// consulted, the rebind before anything is sealed, and the merge after the server has answered --
// so a policy commit that loses MASTER §9.3's race is answered [ErrCommitLost] with the group
// exactly where it was, the live policy still the one every receiver holds, and the verb ready to
// be asked again after [Group.Receive] has followed the winner.
func (self *Group) commitPolicyAndPublishLocked(ctx context.Context, policy []byte) error {
	if _, err := self.authorizeOutgoingLocked(&outgoingCommit{policy: policy}); err != nil {
		return err
	}
	if err := self.rebindLocked(); err != nil {
		return err
	}
	commit, _, _, err := self.handle.CommitPolicy(policy)
	if err != nil {
		return fmt.Errorf("urmessage: CommitPolicy: %w", err)
	}
	return self.publishCommitLocked(ctx, commit)
}
