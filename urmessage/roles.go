// The role model's receiving arm: MASTER §11's rules over one ingested commit, ledger item 242's
// R1, ruled 2026-09-21.
//
// ONE PURE FUNCTION, [authorizeCommit], over one value, [CommitAuthorization]. It reads nothing
// but its argument and it holds no state, which is what lets the same predicate serve both arms
// §11 names -- "refused by the committing client, and rejected by every receiving client on
// validation" -- and what lets every rule be tested as a table rather than through a device. The
// receiving-side call is [Group.authorizeCommitLocked], on every commit, before ApplyCommit, and
// nothing configured on a device can skip it. The committing-side call is
// [Group.authorizeOutgoingLocked] (rolescommit.go, R2), over the value the commit WOULD produce,
// before the seam builds it.
//
// EVERY PROPOSAL IS THE COMMITTER'S. A commit carries proposals by value or by reference, and
// this file never asks who proposed one: each is judged against the AUTHENTICATED committer's
// role (ruling 3), so a by-reference proposal is never more permissive than the same proposal by
// value, and there is no proposer accessor to be wrong about.
//
// WHAT A REFUSAL COSTS, stated because it is a consequence and not a bug (item 242): the server
// has already accepted the refused commit and moved the epoch, so an honest receiver that refuses
// stays at the epoch before and can no longer write. A hostile committer can HALT a group; it
// cannot TAKE it. That is the outcome §11 asks for.
//
// THE RULES, in the order they run. Each is a small named function below, and each names the §11
// sentence or the 2026-09-21 ruling it implements:
//
//	R0a  the post-commit policy exists, parses and validates
//	R0b  every extension but 0xF001 is byte identical before and after
//	R6a  an Add claiming an identity already in the group is that identity's own
//	R6c  no leaf changes identity; the leaf sets agree with the declared adds and removes
//	R6d  every leaf of the post-commit tree carries urmessage_leaf_keys (0xF002)
//	R1   an Add of a new identity needs ADMIN or OWNER
//	R2   a Remove of a MEMBER or OBSERVER, not one's own, needs ADMIN or OWNER
//	R3   a Remove of an ADMIN or the OWNER, not one's own, needs the OWNER
//	R0c  the post-commit policy names no identity without a leaf
//	R5   an ownership transfer is the owner's, to a PRE-commit member, and the old owner is ADMIN
//	R4   admin-set changes and the server id need the OWNER; MEMBER/OBSERVER, retention and
//	     disappearing bucket changes need ADMIN
//	R7   a MEMBER's or an OBSERVER's commit is its own device leaves, or nothing, and no more
//	caps 500 identities, 1,000 leaves, 10 leaves per identity
//
// R0c RUNS AFTER THE REMOVAL RULES AND NOT WITH THE OTHER STRUCTURAL ONES, and the reason is what
// a removed owner leaves behind: a Remove of the owner's only leaf that carries no policy change
// leaves a policy naming an identity with no leaf, which is a phantom by R0c's reading. Judged
// first, every such commit would answer "phantom entry" and the authority violation §11 names by
// name -- "a commit in which a non-owner removes an admin is invalid" -- would never be the
// answer. So who may remove whom is decided first, and a policy left naming the departed is
// refused after, for the commits that had the authority.
//
// R7 RUNS AFTER EVERY RULE THAT NAMES A PARTICULAR AUTHORITY, for the same reason turned around:
// a MEMBER's Add of a stranger, Remove of the owner or promotion of itself is refused by R1, R3
// and R5 with the sentence §11 wrote for it, and R7 judged first would answer each with the
// general one. What R7 catches is the commit none of them names -- the Update of another leaf
// carried by reference, a policy rewrite that changes no role R4 can see -- and it checks the
// adds and removes again on its own terms, so that what it allows is stated in one place and
// does not depend on which rules ran before it. The bare commit is NOT among what it catches
// since ruling 12: a path-only commit is the PCS self-heal and every role may make one.
package urmessage

import (
	"bytes"
	"fmt"

	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/mls"
)

// The caps, ruling 7: 500 identities AND 1,000 leaves in v1, and MASTER §11's ten device leaves
// per identity. The two mls declares are read from mls so the committing client and this arm
// cannot hold different numbers; the leaf cap is this profile's own.
const (
	MaxGroupIdentities   = mls.MaxGroupMembers
	MaxGroupLeaves       = 1000
	MaxLeavesPerIdentity = mls.MaxDeviceLeavesPerIdentity
)

// authorizeCommit is the receiving-client decision on one commit: nil to allow, or the rule that
// refuses it -- one of the sentinels in errors.go, or mls's own for R3 and the caps -- which the
// ingest path wraps in [ErrCommitUnauthorized].
//
// The rules run in the order the file header gives, and the first refusal is the answer.
func authorizeCommit(a *CommitAuthorization) error {
	committer, err := roleNamed(a.CommitterRole)
	if err != nil {
		return err
	}
	before := leavesOf(a.Members)
	after := leavesOf(a.MembersAfter)
	rules := []func() error{
		func() error { return rulePolicyValid(a) },
		func() error { return ruleOtherExtensionsUnchanged(a) },
		func() error { return ruleClaimedIdentityIsOwn(a, before, after) },
		func() error { return ruleIdentityContinuity(a, before, after) },
		func() error { return ruleLeafKeysPresent(a) },
		func() error { return ruleAddByAdmin(a, committer, before, after) },
		func() error { return ruleRemoveByAdmin(a, committer, before) },
		func() error { return ruleAdminRemovedByOwner(a, committer, before) },
		func() error { return ruleNoPhantomEntries(a, after) },
		func() error { return ruleOwnerTransfer(a, committer, before, after) },
		func() error { return ruleRoleChanges(a, committer) },
		func() error { return ruleOwnDevicesOnly(a, committer, before, after) },
		func() error { return ruleCaps(a) },
	}
	for _, rule := range rules {
		if err := rule(); err != nil {
			return err
		}
	}
	return nil
}

// ── R0: the policy and the extension list ────────────────────────────────────────────────────

// rulePolicyValid is R0a. "Roles live in the group-context extension (§6), so they are covered by
// the MLS transcript hash": a commit that leaves the group with no urmessage_group_policy, or with
// one mls.GroupPolicyExtension.Validate refuses -- no owner, two owners, a non canonical list --
// leaves nothing for any later commit to be judged against, and is refused with the reason the
// decode gave (mls.ErrNoGroupPolicy for the absence, item 242's P3).
func rulePolicyValid(a *CommitAuthorization) error {
	if a.PolicyAfter == nil {
		reason := a.PolicyAfterErr
		if reason == nil {
			reason = mls.ErrNoGroupPolicy
		}
		return fmt.Errorf("%w: %w", ErrCommitPolicyInvalid, reason)
	}
	if err := a.PolicyAfter.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrCommitPolicyInvalid, err)
	}
	return nil
}

// ruleOtherExtensionsUnchanged is R0b. A policy change is RFC 9420's wholesale replacement of the
// group context extension list, and item 242's P4 measured what a careless one does: it stripped
// 0x0003 required_capabilities from every group it touched. So every entry but 0xF001 must be
// byte identical, in the same position, before and after. A commit that carries no
// GroupContextExtensions proposal keeps the list it had and passes trivially.
func ruleOtherExtensionsUnchanged(a *CommitAuthorization) error {
	before := extensionsExceptPolicy(a.ExtensionsBefore)
	after := extensionsExceptPolicy(a.ExtensionsAfter)
	if len(before) != len(after) {
		return fmt.Errorf("%w: %d extension(s) besides the policy before the commit and %d after",
			ErrCommitExtensionChanged, len(before), len(after))
	}
	for at := range before {
		if before[at].Type != after[at].Type || !bytes.Equal(before[at].Data, after[at].Data) {
			return fmt.Errorf("%w: extension %#04x at position %d is not the one the group had",
				ErrCommitExtensionChanged, after[at].Type, at)
		}
	}
	return nil
}

// ruleNoPhantomEntries is R0c. A policy entry is a role for a member, and "a member" is an
// identity holding at least one leaf: an entry for an identity with no leaf after the commit is
// a role nobody holds -- the ejected owner still named as owner in item 242's Q2 -- and it is
// refused so the policy and the tree cannot come to name two different groups.
func ruleNoPhantomEntries(a *CommitAuthorization, after map[uint32][]byte) error {
	present := identitySetOf(after)
	for _, entry := range a.PolicyAfter.Roles {
		if !present[string(entry.MemberId)] {
			return fmt.Errorf("%w: the policy names %x as %s and no leaf carries that identity after the commit",
				ErrCommitPolicyPhantom, entry.MemberId, entry.Role)
		}
	}
	return nil
}

// ── R6: identity continuity, the two rules that close item 242's M7 ─────────────────────────

// ruleClaimedIdentityIsOwn is R6a. "An identity already in the group may be given a further leaf
// only by a commit that identity itself makes": nothing binds a credential's identity to the
// leaf's signature key (item 242's M7), so an Add whose credential merely CLAIMS an identity in
// the group would inherit that identity's role. A second device is the identity's own to add, and
// a member of any role may add its own -- OBSERVER included (ruling 5).
func ruleClaimedIdentityIsOwn(a *CommitAuthorization, before map[uint32][]byte, after map[uint32][]byte) error {
	inGroup := identitySetOf(before)
	for _, leaf := range a.AddedLeaves {
		identity, occupied := after[leaf]
		if !occupied {
			// the commit says it added a leaf the post-commit tree does not hold, which is R6c's
			// refusal and not this rule's
			continue
		}
		if inGroup[string(identity)] && !bytes.Equal(identity, a.CommitterIdentity) {
			return fmt.Errorf("%w: leaf %d claims %x, which holds a leaf already, and the committer is %x",
				ErrCommitIdentityClaimed, leaf, identity, a.CommitterIdentity)
		}
	}
	return nil
}

// ruleIdentityContinuity is R6c. "A leaf's identity does not change across an Update or the
// committer's own path": connect/mls holds no such rule of its own -- a committer's path can
// rewrite its leaf's identity to the owner's and every honest receiver accepts it, pinned in mls
// by TestTheCommittersOwnPathCanSwapItsLeafIdentityAndOnlyThePreCommitTreeStillNamesIt -- so the
// comparison is made here and nowhere below. Every leaf present on both sides keeps its identity,
// the committer's leaf first and by name; and the two leaf sets differ by exactly the leaves the
// commit declares added and removed, so a membership change this rule did not see cannot ride in
// under a shape it did.
func ruleIdentityContinuity(a *CommitAuthorization, before map[uint32][]byte, after map[uint32][]byte) error {
	removed := leafSetOf(a.RemovedLeaves)
	added := leafSetOf(a.AddedLeaves)

	// the committer's own path
	was, member := before[a.CommitterLeaf]
	if !member || !bytes.Equal(was, a.CommitterIdentity) {
		return fmt.Errorf("%w: the committer's leaf %d does not carry %x in the pre-commit tree",
			ErrCommitIdentityChanged, a.CommitterLeaf, a.CommitterIdentity)
	}
	now, still := after[a.CommitterLeaf]
	if !still || !bytes.Equal(now, a.CommitterIdentity) {
		return fmt.Errorf("%w: the committer's own path moved leaf %d from %x to %x",
			ErrCommitIdentityChanged, a.CommitterLeaf, a.CommitterIdentity, now)
	}

	// every removed leaf was occupied
	for _, leaf := range a.RemovedLeaves {
		if _, occupied := before[leaf]; !occupied {
			return fmt.Errorf("%w: the commit removes leaf %d, which was not occupied",
				ErrCommitIdentityChanged, leaf)
		}
	}
	// every updated leaf is present on both sides; its identity is held below with the rest
	for _, leaf := range a.UpdatedLeaves {
		if _, occupied := before[leaf]; !occupied {
			return fmt.Errorf("%w: the commit updates leaf %d, which was not occupied",
				ErrCommitIdentityChanged, leaf)
		}
		if removed[leaf] {
			return fmt.Errorf("%w: the commit both updates and removes leaf %d",
				ErrCommitIdentityChanged, leaf)
		}
	}
	// every leaf occupied before is either kept with its identity, or removed -- and a removed
	// leaf occupied again is a declared add landing in the blank the removal left
	for _, member := range a.Members {
		leaf := member.Leaf
		now, still := after[leaf]
		if removed[leaf] {
			if still && !added[leaf] {
				return fmt.Errorf("%w: leaf %d is removed and still occupied, and the commit declares no add there",
					ErrCommitIdentityChanged, leaf)
			}
			continue
		}
		if !still {
			return fmt.Errorf("%w: leaf %d (%x) is gone and the commit declares no removal of it",
				ErrCommitIdentityChanged, leaf, member.IdentityPub)
		}
		if !bytes.Equal(now, member.IdentityPub) {
			return fmt.Errorf("%w: leaf %d carried %x and now carries %x",
				ErrCommitIdentityChanged, leaf, member.IdentityPub, now)
		}
	}
	// every leaf occupied after is either one that was kept, or a declared add
	for _, member := range a.MembersAfter {
		leaf := member.Leaf
		_, was := before[leaf]
		if (!was || removed[leaf]) && !added[leaf] {
			return fmt.Errorf("%w: leaf %d (%x) is occupied after the commit and the commit declares no add there",
				ErrCommitIdentityChanged, leaf, member.IdentityPub)
		}
	}
	// every declared add is a leaf that is occupied after and was blank, or blanked, before
	for _, leaf := range a.AddedLeaves {
		if _, occupied := after[leaf]; !occupied {
			return fmt.Errorf("%w: the commit adds leaf %d and the post-commit tree does not hold it",
				ErrCommitIdentityChanged, leaf)
		}
		if _, was := before[leaf]; was && !removed[leaf] {
			return fmt.Errorf("%w: the commit adds leaf %d over a member it does not remove",
				ErrCommitIdentityChanged, leaf)
		}
	}
	return nil
}

// ruleLeafKeysPresent is R6d, the hardening R1 carried into this pass (item 242): every leaf of
// the tree the commit ENTERS carries a urmessage_leaf_keys extension (0xF002) this profile can
// wrap to. Both send doors refuse a key package without one, mls's list rules require an added
// leaf to LIST the type and never to carry one, and a hostile mls build walks neither door -- so
// a keyless leaf admitted past them is a member every epoch wrap silently skips, whose first
// symptom is MemberAt refusing that ordinal one commit later and whose second is that member
// reading nothing. The fact is the seam's [messagegroup.ProcessedMember.HasLeafKeys], read off
// the staged tree by the same call the send doors refuse with, and it is judged over EVERY leaf
// rather than the added ones alone so that an Update or the committer's own path dropping the
// extension is the same refusal. The pre-commit membership needs no twin: MemberAt already
// refuses a keyless leaf, and a group that holds one cannot build its authorization at all.
func ruleLeafKeysPresent(a *CommitAuthorization) error {
	for _, member := range a.MembersAfter {
		if !member.HasLeafKeys {
			return fmt.Errorf("%w: leaf %d (%x) carries no urmessage_leaf_keys after the commit",
				ErrCommitLeafWithoutKeys, member.Leaf, member.IdentityPub)
		}
	}
	return nil
}

// ── R1, R2, R3: who may add and who may remove ──────────────────────────────────────────────

// ruleAddByAdmin is R1. "An Add is an ADMIN's or the OWNER's to commit" (§11's table; ruling 1,
// which had MASTER win over spec A §7.3a's member-committed link). It is judged over the added
// leaves whose identity is NEW to the group; an added leaf claiming an identity already present
// is R6a's, and R6a has already required it to be the committer's own. ONE LINE DECIDES IT, so
// the ruling is a flip and not a rewrite.
func ruleAddByAdmin(a *CommitAuthorization, committer mls.Role, before map[uint32][]byte, after map[uint32][]byte) error {
	inGroup := identitySetOf(before)
	for _, leaf := range a.AddedLeaves {
		identity, occupied := after[leaf]
		if !occupied || inGroup[string(identity)] {
			continue
		}
		if !isAdminOrOwner(committer) {
			return fmt.Errorf("%w: leaf %d (%x) added by a %s", ErrCommitAddByNonAdmin, leaf, identity, committer)
		}
	}
	return nil
}

// ruleRemoveByAdmin is R2. §11's table gives an ADMIN "remove MEMBERs and OBSERVERs only" and a
// MEMBER "send, read"; ruling 2 exempts a member's OWN leaves for any role -- "otherwise revoking
// a stolen laptop would block on an admin" -- and ruling 5 lets an OBSERVER do exactly that and
// nothing else.
func ruleRemoveByAdmin(a *CommitAuthorization, committer mls.Role, before map[uint32][]byte) error {
	for _, member := range removedMembers(a, before) {
		role, err := roleNamed(member.Role)
		if err != nil {
			return err
		}
		if isAdminOrOwner(role) {
			continue
		}
		if !isAdminOrOwner(committer) {
			return fmt.Errorf("%w: leaf %d (%x, %s) removed by a %s",
				ErrCommitRemoveByNonAdmin, member.Leaf, member.IdentityPub, role, committer)
		}
	}
	return nil
}

// ruleAdminRemovedByOwner is R3, and the first production call of mls.ErrAdminRemovedByNonOwner.
// "Only the OWNER may remove an ADMIN. ... a commit in which a non-owner removes an admin is
// invalid ... one compromised admin could otherwise strip the entire admin set including the owner
// in a single commit, and the removed owner's keys are gone from the very next epoch, so there is
// no undo by construction." The owner's own leaves are covered the same way: a Remove of the owner
// by anyone but the owner is refused here, and the owner removing its own LAST leaf is exempt here
// and refused by R0c or R5 unless the same commit transfers ownership (ruling 2).
func ruleAdminRemovedByOwner(a *CommitAuthorization, committer mls.Role, before map[uint32][]byte) error {
	for _, member := range removedMembers(a, before) {
		role, err := roleNamed(member.Role)
		if err != nil {
			return err
		}
		if !isAdminOrOwner(role) {
			continue
		}
		if committer != mls.RoleOwner {
			return fmt.Errorf("%w: leaf %d (%x, %s) removed by a %s",
				mls.ErrAdminRemovedByNonOwner, member.Leaf, member.IdentityPub, role, committer)
		}
	}
	return nil
}

// removedMembers is the pre-commit member at each removed leaf, EXCEPT the committer's own
// identity's leaves: "a member of any role may remove its own leaf" (§11, ruling 2), so those are
// nobody's to judge. A removed leaf the pre-commit tree does not hold is R6c's refusal and is
// skipped here.
func removedMembers(a *CommitAuthorization, before map[uint32][]byte) []CommitMember {
	byLeaf := map[uint32]CommitMember{}
	for _, member := range a.Members {
		byLeaf[member.Leaf] = member
	}
	out := []CommitMember{}
	for _, leaf := range a.RemovedLeaves {
		member, occupied := byLeaf[leaf]
		if !occupied {
			continue
		}
		if bytes.Equal(before[leaf], a.CommitterIdentity) {
			continue
		}
		out = append(out, member)
	}
	return out
}

// ── R5 and R4: what a policy commit may change ───────────────────────────────────────────────

// ruleOwnerTransfer is R5. "Ownership transfers only to a current member, and the outgoing owner
// becomes an ADMIN" (§11; ruling 4), and the transfer is the owner's alone -- §11's table gives
// the OWNER "sole authority for ... ownership transfer". A pre-commit policy with no owner to read
// -- a group with no policy at all -- makes every commit that installs one a transfer by a
// committer who is unnamed, and so refused; a group in that state is halted, which is the honest
// outcome of having lost the value every later commit is judged against.
//
// "A CURRENT MEMBER" IS THE PRE-COMMIT MEMBERSHIP (ruling 10, §11's literal reading): the new
// owner held a leaf before the commit, so an owner may not add a stranger and crown it in the
// same commit -- the first cut of this rule read the post-commit tree and allowed exactly that.
// The new owner must still hold a leaf AFTER the commit, which R0c refuses as a phantom before
// this rule is reached and which is held here as well, so that what this rule allows is stated
// in one place.
func ruleOwnerTransfer(a *CommitAuthorization, committer mls.Role, before map[uint32][]byte, after map[uint32][]byte) error {
	ownerAfter, named := a.PolicyAfter.OwnerId()
	if !named {
		// unreachable past R0a, which validated exactly one owner
		return fmt.Errorf("%w: %w", ErrCommitPolicyInvalid, mls.ErrNoOwner)
	}
	var ownerBefore []byte
	hadOwner := false
	if a.PolicyBefore != nil {
		ownerBefore, hadOwner = a.PolicyBefore.OwnerId()
	}
	if hadOwner && bytes.Equal(ownerBefore, ownerAfter) {
		return nil
	}
	if committer != mls.RoleOwner {
		return fmt.Errorf("%w: ownership moved to %x in a commit by a %s", ErrCommitOwnerTransfer, ownerAfter, committer)
	}
	if !identitySetOf(before)[string(ownerAfter)] {
		return fmt.Errorf("%w: the new owner %x held no leaf before the commit, and ownership transfers only to a current member",
			ErrCommitOwnerTransfer, ownerAfter)
	}
	present := identitySetOf(after)
	if !present[string(ownerAfter)] {
		return fmt.Errorf("%w: the new owner %x holds no leaf after the commit", ErrCommitOwnerTransfer, ownerAfter)
	}
	if hadOwner && present[string(ownerBefore)] {
		if role, _ := a.PolicyAfter.RoleOf(ownerBefore); role != mls.RoleAdmin {
			return fmt.Errorf("%w: the outgoing owner %x is still a member and the policy makes it %s rather than admin",
				ErrCommitOwnerTransfer, ownerBefore, role)
		}
	}
	return nil
}

// ruleRoleChanges is R4, over every identity present after the commit. §11's table: the OWNER
// holds "sole authority for ... admin-set changes", so any role to ADMIN and ADMIN to MEMBER or
// OBSERVER need the owner; an ADMIN may "set MEMBER/OBSERVER" and change the "retention policy,
// group metadata", so MEMBER to OBSERVER and back, the retention pair and the disappearing
// buckets (§12.2's EPH class, "admin-settable for groups") need an admin or the owner. A
// transition to or from OWNER is R5's and is not judged twice here. An identity that left the
// group has no role after it and is R0c's; an identity that arrived is unnamed before, so naming
// it ADMIN in the commit that adds it is an admin-set change and the owner's.
//
// THE SERVER ID IS THE OWNER'S ALONE, and it is not "group metadata". MASTER §6 names it as the
// message server the group lives on -- the one that stores its ciphertext and orders its records
// -- and mls's own doc for the field says it is "a v2 field retained in v1, where it is always
// the one server". A change to it is MASTER §2's "group migration between hosts", deferred to
// V2+ with no code implementing it: it moves where the group EXISTS, which is the weight of an
// ownership transfer and not of a retention number. No v1 client makes the change, so the
// narrowest authority is the safe default, and a V2 design that wants an admin to migrate a
// group widens it with a ruling rather than inherits it from a clause that never named it.
//
// The DM's joint policy -- either party may shorten and neither may lengthen alone -- is a later
// step (plan R5) and is not here: a two-member group is judged by these rules until it lands.
func ruleRoleChanges(a *CommitAuthorization, committer mls.Role) error {
	if a.PolicyBefore == nil {
		// unreachable past R5: with no owner before, every valid PolicyAfter is a transfer, and
		// a committer unnamed before is not the owner
		return nil
	}
	seen := map[string]bool{}
	for _, member := range a.MembersAfter {
		key := string(member.IdentityPub)
		if seen[key] {
			continue
		}
		seen[key] = true
		was, _ := a.PolicyBefore.RoleOf(member.IdentityPub)
		now, _ := a.PolicyAfter.RoleOf(member.IdentityPub)
		if was == now || was == mls.RoleOwner || now == mls.RoleOwner {
			continue
		}
		if was == mls.RoleAdmin || now == mls.RoleAdmin {
			if committer != mls.RoleOwner {
				return fmt.Errorf("%w: %x moved from %s to %s in a commit by a %s",
					ErrCommitRoleChangeByNonOwner, member.IdentityPub, was, now, committer)
			}
			continue
		}
		if !isAdminOrOwner(committer) {
			return fmt.Errorf("%w: %x moved from %s to %s in a commit by a %s",
				ErrCommitPolicyChangeByNonAdmin, member.IdentityPub, was, now, committer)
		}
	}
	if a.PolicyBefore.RetentionPolicy != a.PolicyAfter.RetentionPolicy {
		if !isAdminOrOwner(committer) {
			return fmt.Errorf("%w: the retention policy changed in a commit by a %s",
				ErrCommitPolicyChangeByNonAdmin, committer)
		}
	}
	if !bytes.Equal(a.PolicyBefore.DisappearingBuckets, a.PolicyAfter.DisappearingBuckets) {
		if !isAdminOrOwner(committer) {
			return fmt.Errorf("%w: the disappearing buckets changed in a commit by a %s",
				ErrCommitPolicyChangeByNonAdmin, committer)
		}
	}
	if !bytes.Equal(a.PolicyBefore.ServerId, a.PolicyAfter.ServerId) {
		if committer != mls.RoleOwner {
			return fmt.Errorf("%w: the server id moved from %x to %x in a commit by a %s",
				ErrCommitServerIdChangeByNonOwner, a.PolicyBefore.ServerId, a.PolicyAfter.ServerId, committer)
		}
	}
	return nil
}

// ── R7: what a MEMBER or an OBSERVER may commit at all ───────────────────────────────────────

// ruleOwnDevicesOnly is R7. §11's table gives "commit epochs" to ADMIN and OWNER and gives MEMBER
// "send, read" and OBSERVER "read only"; the one membership commit either may make is §11's
// self-service one -- "a member may add or remove their own device leaves and commit that
// change" -- and ruling 5 says it of the OBSERVER in so many words: "its own device add / remove
// and nothing else". So a commit by either carries its own device leaves and no more: every
// added leaf carries the committer's identity, every removed leaf carried it, no other leaf is
// updated, and the group context extension list is the one the group had, 0xF001 included.
//
// THE PATH-ONLY COMMIT IS ALLOWED FOR EVERY ROLE, OBSERVER INCLUDED (ruling 12). RFC 9420 §12.4
// forbids a committer carrying its own Update, so a member's ONLY way to refresh its own leaf
// keys -- the PCS self-heal -- is a commit whose path does it: no proposals, an extension list
// byte identical to the group's. "Commit epochs" in §11's table means commits that change
// membership or policy, and a bare commit changes neither. The first build of this rule refused
// it as "an epoch bump", and that reading left a compromised member's leaf unhealable by that
// member. Halting-by-spam is not an argument against this: any garbage commit halts a group
// already (item 242's cost paragraph), and the defence against that is not a rule here.
//
// AN UPDATE BY REFERENCE IS STILL REFUSED: the proposal is its proposer's own key rotation, but
// the commit is the committer's (ruling 3), and committing another leaf's Update is committing
// a change to a leaf that is not the committer's own. And any extension change is still refused,
// a policy rewrite that changes no role R4 can see included.
//
// An ADMIN or the OWNER is not judged here; the rules before this one already said what each
// may carry.
func ruleOwnDevicesOnly(a *CommitAuthorization, committer mls.Role, before map[uint32][]byte, after map[uint32][]byte) error {
	if isAdminOrOwner(committer) {
		return nil
	}
	if len(a.UpdatedLeaves) != 0 {
		return fmt.Errorf("%w: a %s committed an update of leaf %d, which is not its own device change",
			ErrCommitBeyondOwnDevices, committer, a.UpdatedLeaves[0])
	}
	if !extensionsEqual(a.ExtensionsBefore, a.ExtensionsAfter) {
		return fmt.Errorf("%w: a %s committed a group context extension list other than the one the group had",
			ErrCommitBeyondOwnDevices, committer)
	}
	for _, leaf := range a.AddedLeaves {
		if !bytes.Equal(after[leaf], a.CommitterIdentity) {
			return fmt.Errorf("%w: a %s added leaf %d (%x), which is not its own device",
				ErrCommitBeyondOwnDevices, committer, leaf, after[leaf])
		}
	}
	for _, leaf := range a.RemovedLeaves {
		if !bytes.Equal(before[leaf], a.CommitterIdentity) {
			return fmt.Errorf("%w: a %s removed leaf %d (%x), which is not its own device",
				ErrCommitBeyondOwnDevices, committer, leaf, before[leaf])
		}
	}
	return nil
}

// ── the caps ─────────────────────────────────────────────────────────────────────────────────

// ruleCaps is ruling 7 and §11's "An identity may hold at most ten device leaves, and a group at
// most 500 members (§6). Both caps are enforced by the committing client and by every receiving
// client on validation." Judged over the post-commit tree, which is what the commit would make
// the group.
func ruleCaps(a *CommitAuthorization) error {
	perIdentity := map[string]int{}
	for _, member := range a.MembersAfter {
		perIdentity[string(member.IdentityPub)] += 1
	}
	if len(a.MembersAfter) > MaxGroupLeaves {
		return fmt.Errorf("%w: %d leaves after the commit, cap %d", mls.ErrGroupSizeExceeded, len(a.MembersAfter), MaxGroupLeaves)
	}
	if len(perIdentity) > MaxGroupIdentities {
		return fmt.Errorf("%w: %d identities after the commit, cap %d", mls.ErrGroupSizeExceeded, len(perIdentity), MaxGroupIdentities)
	}
	for _, member := range a.MembersAfter {
		if count := perIdentity[string(member.IdentityPub)]; count > MaxLeavesPerIdentity {
			return fmt.Errorf("%w: %x holds %d leaves after the commit, cap %d",
				mls.ErrDeviceLimitExceeded, member.IdentityPub, count, MaxLeavesPerIdentity)
		}
	}
	return nil
}

// ── the small readings the rules share ───────────────────────────────────────────────────────

// roleNamed is the mls role a [CommitMember.Role] or [CommitAuthorization.CommitterRole] name
// stands for, refusing a name this profile does not define rather than defaulting it.
func roleNamed(name string) (mls.Role, error) {
	role, err := mls.ParseRole(name)
	if err != nil {
		return mls.RoleObserver, fmt.Errorf("%w: %q", ErrCommitRoleUnknown, name)
	}
	return role, nil
}

// isAdminOrOwner is the "ADMIN or OWNER" of §11's table, spelled once.
func isAdminOrOwner(role mls.Role) bool {
	return role == mls.RoleAdmin || role == mls.RoleOwner
}

// leavesOf is a membership as a leaf-to-identity map.
func leavesOf(members []CommitMember) map[uint32][]byte {
	out := make(map[uint32][]byte, len(members))
	for _, member := range members {
		out[member.Leaf] = member.IdentityPub
	}
	return out
}

// identitySetOf is the set of identities a leaf map holds.
func identitySetOf(leaves map[uint32][]byte) map[string]bool {
	out := make(map[string]bool, len(leaves))
	for _, identity := range leaves {
		out[string(identity)] = true
	}
	return out
}

// leafSetOf is a leaf vector as a set.
func leafSetOf(leaves []uint32) map[uint32]bool {
	out := make(map[uint32]bool, len(leaves))
	for _, leaf := range leaves {
		out[leaf] = true
	}
	return out
}

// extensionsExceptPolicy is an extension list with the 0xF001 entry left out, in order.
func extensionsExceptPolicy(extensions []messagegroup.ExtensionBytes) []messagegroup.ExtensionBytes {
	out := make([]messagegroup.ExtensionBytes, 0, len(extensions))
	for _, extension := range extensions {
		if extension.Type == uint16(mls.ExtensionTypeUrmessageGroupPolicy) {
			continue
		}
		out = append(out, extension)
	}
	return out
}

// extensionsEqual is whether two extension lists are the same entries, in the same order, with
// the same octets -- the whole list, 0xF001 included, where R0b reads everything but it.
func extensionsEqual(before []messagegroup.ExtensionBytes, after []messagegroup.ExtensionBytes) bool {
	if len(before) != len(after) {
		return false
	}
	for at := range before {
		if before[at].Type != after[at].Type || !bytes.Equal(before[at].Data, after[at].Data) {
			return false
		}
	}
	return true
}
