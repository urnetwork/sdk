package urmessage

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/urnetwork/connect/v2026/messagegroup"
	"github.com/urnetwork/connect/v2026/mls"
)

// ── [Group.RemoveMember]: the removal track's product verb, on the SEND side ──────────────────
//
// WHAT THIS FILE CAN MEASURE AND WHAT IT CANNOT, said first because it decides every instrument
// below. No test in this package can run a publishing verb to completion: the submit goes through
// `*sdk.MessageTransport`, a concrete type with no stub, which is why [rolescommit_test.go]'s own
// header says the allowed path is cp3b's. So what is driven here is everything the verb decides
// BEFORE the connection is consulted -- which is the whole of ledger item 242's R2 coverage debt --
// and cp3b's TestOneCallRemovesEveryLeafOfOneIdentityAndTheRemovedMemberCannotFollow is the same
// verb over a running server.
//
// THE INSTRUMENT FOR WHAT THE VERB DERIVES IS THE DEVICE'S OWN [CommitAuthorizer], AND IT IS THE
// PRODUCTION DOOR RATHER THAN A HARNESS. [Group.authorizeOutgoingLocked] runs the configured
// authorizer AFTER [authorizeCommit] over the same value, and a refusal from it stops the verb with
// nothing built -- so an authorizer that COPIES the decision and then refuses reads the exact
// [CommitAuthorization] the verb derived, at the last door before the seam is asked to stage
// anything. Nothing is re-spelled: the leaves, the policy and the extension list under test are the
// ones the verb computed and the ones it would have handed [messagegroup.GroupHandle.CommitRemoveWithExtensions].
// That matters for the reason pqrotation_test.go's own header gives about a fan-out a harness built
// beside production: a case that assembled its own removal vector would measure its own arithmetic.
//
// A CAPTURE THAT NEVER FIRES IS A FAILURE AND NOT A PASS. [authorizeCommit] runs first, so a verb
// whose derivation the RULES refuse never reaches the authorizer -- which is a real outcome worth
// naming rather than a hole: it is how a policy left naming the departed is caught (R0c) on this
// side. Every case below that expects a capture asserts that one happened.

// removeCapture is the decision a verb derived, taken at the configured authorizer and refused
// there so nothing is built. `refusal` is the sentinel the verb then answers.
type removeCapture struct {
	decision *CommitAuthorization
	calls    int
}

var errRemoveCaptureStop = errors.New("the capture refuses so that nothing is built")

// captureOutgoingOn installs a capture-and-refuse authorizer on one group's device and answers it.
// The authorizer is removed when the test ends, because a world's devices are shared across a
// case's later clauses.
func captureOutgoingOn(t *testing.T, group *Group) *removeCapture {
	t.Helper()
	capture := &removeCapture{}
	group.device.commitAuthorizer = func(decision *CommitAuthorization) error {
		capture.calls += 1
		capture.decision = decision
		return errRemoveCaptureStop
	}
	t.Cleanup(func() { group.device.commitAuthorizer = nil })
	return capture
}

// captureOutgoing is [captureOutgoingOn] over a role world's member.
func captureOutgoing(t *testing.T, who *roleMember) *removeCapture {
	t.Helper()
	return captureOutgoingOn(t, who.group)
}

// removeNothingBuilt holds the half of every refusal that is about state rather than about the
// sentence: the handle never staged or merged anything, the group is where it was, and a by-value
// arm still works afterwards -- which is the reading that says no staged value was left behind,
// since the seam refuses to stage over a pending commit.
func removeNothingBuilt(t *testing.T, who *roleMember, what string, epoch uint64) {
	t.Helper()
	if got := who.handle.Epoch(); got != epoch {
		t.Errorf("%s's handle is at epoch %d after %s, want %d: something was merged", who.name, got, what, epoch)
	}
	if got := who.group.Epoch(); got != epoch {
		t.Errorf("%s's group is at epoch %d after %s, want %d", who.name, got, what, epoch)
	}
	if _, _, _, err := who.handle.Commit(nil); err != nil {
		t.Errorf("%s could not stage a bare commit after %s (%v), so the refused verb left a staged "+
			"value behind", who.name, what, err)
		return
	}
	who.handle.ClearPendingCommit()
}

// removeWorld is a role world whose policy NAMES every member, which is the state ledger item 242
// found the removal track blocked on: "once an identity has been NAMED (any SetRole keeps an
// explicit entry; nothing calls RemoveRole), a bare CommitRemove of its last leaf is an R0c phantom
// at every receiver". Every case here therefore has an entry for the verb to drop, and the drop is
// measured against a PolicyBefore that really carries one.
//
// The policy is committed through the seam and ingested at every member, because [Group.SetRole]
// publishes and no test in this package can publish. What that costs is nothing this file measures:
// the roles land in the group context either way, and the send-side decision reads them off it.
func removeWorld(t *testing.T, roles map[string]mls.Role, names ...string) *roleWorld {
	t.Helper()
	world := newRoleWorld(t, names...)
	owner := world.member(names[0])
	policy := world.policyOf(owner)
	for name, role := range roles {
		policy.SetRole(world.member(name).dev.identityPub, role)
	}
	record := world.commitAndPublish(owner, "CommitPolicy naming every member", func() ([]byte, []byte, []byte, error) {
		return owner.handle.CommitPolicy(world.policyBody(policy))
	})
	for _, name := range names[1:] {
		world.ingest(world.member(name), owner, record)
	}
	return world
}

// removePolicyNames is whether a member's identity has an entry in a policy, read the way R0c reads
// one: the entry list itself and not [mls.GroupPolicyExtension.RoleOf], whose answer for an unnamed
// identity is MEMBER (ruling 8) and so cannot tell "named MEMBER" from "not named".
func removePolicyNames(policy *mls.GroupPolicyExtension, identity []byte) bool {
	if policy == nil {
		return false
	}
	for _, entry := range policy.Roles {
		if bytes.Equal(entry.MemberId, identity) {
			return true
		}
	}
	return false
}

// ONE IDENTITY-KEYED CALL CARRIES EVERY LEAF THAT IDENTITY HOLDS, AND THE POLICY ENTRY THAT NAMES
// IT, IN ONE COMMIT (ledger item 258, ruling 49).
//
// WHY THE TWO CLAUSES ARE ONE CASE. An identity may hold up to ten device leaves (§11, ruling 7),
// and a removal that takes some of them has removed nobody: the leaves left standing read every
// epoch and write to it. A removal that takes every leaf and leaves the POLICY entry is an R0c
// phantom every receiver refuses -- which is how the OWNER was unable to remove an ADMIN at all
// before this verb (item 242's R2 filed it as the removal track's blocker). So the property is that
// one call answers BOTH, and the two are asserted off ONE value: the [CommitAuthorization] the verb
// itself derived.
//
// THE CONTROLS ARE INLINE AND EACH FIRES FOR ITS OWN REASON. Before the call: the subject holds TWO
// leaves in the live tree (else "both leaves go" is a sentence about one leaf) and PolicyBefore
// NAMES it (else the drop below is measured against an entry that was never there). After: the
// survivors' own entries are still in PolicyAfter (else a policy that lost every entry would satisfy
// the drop), and every extension but 0xF001 is byte identical (else a list rebuilt from nothing
// would satisfy it too).
//
// WHAT WOULD GO RED: remove only the first leaf the identity holds (RemovedLeaves has one entry and
// MembersAfter still carries the other); drop the Remove of the policy entry (PolicyAfter still
// names the subject -- and R0c refuses the derivation before the capture, so the capture count goes
// to zero); build the extension list from the policy alone rather than through
// mls.ExtensionsWithGroupPolicy (0x0003 required_capabilities disappears).
func TestOneRemoveMemberCallCarriesEveryLeafOfTheIdentityAndDropsItsPolicyEntry(t *testing.T) {
	ctx := context.Background()
	world := removeWorld(t, map[string]mls.Role{"bob": mls.RoleAdmin, "carol": mls.RoleMember},
		"owner", "bob", "carol")
	owner, bob, carol := world.member("owner"), world.member("bob"), world.member("carol")

	// carol's SECOND DEVICE, added by carol herself: §11's self-service rule and R6a -- "an
	// identity already in the group may be given a further leaf only by a commit that identity
	// itself makes" -- so the commit is carol's and every honest receiver follows it.
	laptop := claimingKeyPackage(t, filepath.Join(world.root, "carol-laptop"), carol.dev.identityPub)
	record := world.commitAndPublish(carol, "CommitAdd of carol's own second device", func() ([]byte, []byte, []byte, error) {
		return carol.handle.CommitAdd([][]byte{laptop})
	})
	for _, honest := range []*roleMember{owner, bob} {
		world.ingest(honest, carol, record)
	}

	// ── THE CONTROLS, BEFORE THE CALL ───────────────────────────────────────────────────────
	members, err := owner.group.Members()
	if err != nil {
		t.Fatalf("the owner's roster: %v", err)
	}
	held := []uint32{}
	for _, member := range members {
		if bytes.Equal(member.IdentityPub, carol.dev.identityPub) {
			held = append(held, member.LeafIndex)
		}
	}
	if len(held) != 2 {
		t.Fatalf("CONTROL FAILED: carol's identity holds %d leaf/leaves in the live tree (%v), and "+
			"this case is about a call that takes EVERY leaf of one identity", len(held), held)
	}
	before := world.policyOf(owner)
	if !removePolicyNames(before, carol.dev.identityPub) {
		t.Fatalf("CONTROL FAILED: the live policy does not name carol, so the entry this case " +
			"watches the verb drop was never there")
	}

	// ── THE CALL, captured at the production door and refused there ─────────────────────────
	capture := captureOutgoing(t, owner)
	epoch := owner.group.Epoch()
	err = owner.group.RemoveMember(ctx, carol.dev.identityPub)
	if !errors.Is(err, ErrCommitUnauthorized) || !errors.Is(err, errRemoveCaptureStop) {
		t.Fatalf("the owner's RemoveMember answered %v; this case's authorizer refuses so that "+
			"nothing is built, so the verb must answer ErrCommitUnauthorized wrapping that refusal", err)
	}
	if capture.calls != 1 {
		t.Fatalf("the configured authorizer saw %d decision(s), want 1: the verb's derivation was "+
			"refused by the RULES before it got there, so there is nothing to read", capture.calls)
	}
	removeNothingBuilt(t, owner, "a refused RemoveMember", epoch)
	decision := capture.decision

	// ── BOTH LEAVES, IN ONE COMMIT ──────────────────────────────────────────────────────────
	got := slices.Clone(decision.RemovedLeaves)
	slices.Sort(got)
	want := slices.Clone(held)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("one RemoveMember call removes leaves %v and carol's identity holds %v. A removal "+
			"that leaves one of an identity's device leaves standing has removed nobody: that leaf "+
			"reads every epoch and writes to it", got, want)
	}
	if len(decision.AddedLeaves) != 0 || len(decision.UpdatedLeaves) != 0 {
		t.Errorf("the removal declares %d add(s) and %d update(s); it is a removal and a policy and "+
			"nothing else", len(decision.AddedLeaves), len(decision.UpdatedLeaves))
	}
	for _, member := range decision.MembersAfter {
		if bytes.Equal(member.IdentityPub, carol.dev.identityPub) {
			t.Errorf("leaf %d still carries carol's identity after the commit the verb built", member.Leaf)
		}
	}
	// the positive control on the membership: the survivors are all still there
	if len(decision.MembersAfter) != len(decision.Members)-2 {
		t.Errorf("the post-commit membership holds %d leaves and the pre-commit one %d; want two fewer",
			len(decision.MembersAfter), len(decision.Members))
	}

	// ── AND THE POLICY ENTRY, IN THE SAME COMMIT ────────────────────────────────────────────
	if decision.PolicyAfter == nil {
		t.Fatalf("the commit the verb built leaves the group with no readable policy: %v", decision.PolicyAfterErr)
	}
	if !removePolicyNames(decision.PolicyBefore, carol.dev.identityPub) {
		t.Errorf("the decision's PolicyBefore does not name carol, so its PolicyAfter not naming her says nothing")
	}
	if removePolicyNames(decision.PolicyAfter, carol.dev.identityPub) {
		t.Errorf("the commit removes every leaf carol's identity holds and the policy it installs " +
			"still names her. Every honest receiver refuses that as an R0c phantom, which is the " +
			"whole reason this verb commits a policy beside the Remove")
	}
	// the positive control on the policy: the survivors' own entries survive the drop
	for _, survivor := range []*roleMember{owner, bob} {
		if !removePolicyNames(decision.PolicyAfter, survivor.dev.identityPub) {
			t.Errorf("CONTROL FAILED: the policy the commit installs does not name %s either, so "+
				"the drop above is satisfied by a policy that lost every entry", survivor.name)
		}
	}
	if owner, named := decision.PolicyAfter.OwnerId(); !named || !bytes.Equal(owner, world.member("owner").dev.identityPub) {
		t.Errorf("the policy the commit installs names %x as owner, want the founder's identity", owner)
	}

	// ── AND EVERY OTHER EXTENSION IS THE ONE THE GROUP HAD (R0b, and 0x0003 with it) ─────────
	if !extensionsEqual(extensionsExceptPolicy(decision.ExtensionsBefore), extensionsExceptPolicy(decision.ExtensionsAfter)) {
		t.Errorf("the extension list the verb hands the seam changes an entry other than 0xF001: "+
			"before %v, after %v", decision.ExtensionsBefore, decision.ExtensionsAfter)
	}
	capabilities := false
	for _, extension := range decision.ExtensionsAfter {
		if extension.Type == uint16(mls.ExtensionTypeRequiredCapabilities) {
			capabilities = true
		}
	}
	if !capabilities {
		t.Errorf("CONTROL FAILED: the list the verb hands the seam carries no 0x0003 "+
			"required_capabilities (%v), so the equality above is over a list this group never had",
			decision.ExtensionsAfter)
	}
	t.Logf("one call removed leaves %v of one identity, dropped its policy entry, and kept %d "+
		"extension(s) byte identical", got, len(extensionsExceptPolicy(decision.ExtensionsAfter)))
}

// EVERY RULE A REMOVAL CAN BREAK IS DECIDED FROM THE VERB, ON THE SEND SIDE, BY THE PREDICATE THE
// RECEIVERS RUN -- AND THE THREE REQUESTS FOR WHICH NO COMMIT EXISTS ARE ANSWERED BY NAME.
//
// THIS IS LEDGER ITEM 242's R2 COVERAGE DEBT, DISCHARGED. R2 filed it in its own words: "a
// send-side test of R2/R3/R6a/caps over `outgoingCommit{removeLeaves}`, which no verb reaches yet".
// The field has been on [outgoingCommit] since 2026-09-22 with nothing writing it, so
// [ruleRemoveByAdmin] and [ruleAdminRemovedByOwner] were held only by the pure table in
// roles_test.go and by hand-built commits at receivers. [Group.RemoveMember] is the first verb that
// writes it, and these are the first driven send-side cases for either rule.
//
// MEASURED RATHER THAN ASSERTED, AND THE QUERY IS PUBLISHED BESIDE THE ANSWER. Over every _test.go
// file of `urmessage` and `cp3b` at the commit before this one, for each rule sentinel, "is it named
// within a window of a call to AddMemberAndPublish / SetRole / TransferOwnership / RemoveMember":
//
//	driven from a verb      ErrCommitAddByNonAdmin (R1), ErrCommitOwnerTransfer (R5),
//	                        ErrCommitRoleChangeByNonOwner and ErrCommitPolicyChangeByNonAdmin (R4),
//	                        ErrCommitPolicyPhantom (R0c), ErrCommitBeyondOwnDevices (R7)
//	NO verb-driven case     ErrCommitRemoveByNonAdmin (R2), mls.ErrAdminRemovedByNonOwner (R3),
//	                        ErrCommitPolicyInvalid (R0a), ErrCommitExtensionChanged (R0b),
//	                        ErrCommitIdentityClaimed (R6a), ErrCommitIdentityChanged (R6c),
//	                        ErrCommitLeafWithoutKeys (R6d), ErrCommitServerIdChangeByNonOwner,
//	                        mls.ErrGroupSizeExceeded and mls.ErrDeviceLimitExceeded (the caps)
//
// R2 AND R3 ARE THE TWO THIS VERB CLOSES. The rest are stated rather than quietly left out: R0a and
// R0b are unreachable from any verb in this package because every verb encodes its policy through
// mls's own validating encoder before the predicate runs; R6a, R6c and R6d judge ADDED leaves and
// leaf identities, which no removal declares; the server id has no verb at all; and no removal can
// break a cap, whatever the size of the group. The window is a crude instrument -- a sentinel inside
// a table whose verb call is thirty lines away is missed -- so it is a FLOOR on what was undriven
// and not a ceiling.
//
// R6a AND THE CAPS ARE NOT HERE, AND THE REASON IS STRUCTURAL RATHER THAN A COST -- which is ledger
// item 259's own correction of this paragraph. R6a judges ADDED leaves ("an Add claiming an identity
// already in the group is that identity's own") and a removal declares none, so no call of this verb
// can reach it. The caps are the same kind of unreachable and it is NOT that a case would need 500
// identities or 1,000 leaves to build: [ruleCaps] reads three numbers off the POST-COMMIT tree --
// the leaf count, the identity count and each identity's leaf count -- and a removal only ever
// shrinks all three. So no removal of any size in a group of any size can move either cap's input
// toward its bound; a group already over one could never have formed, because every commit that
// built it was judged by this same predicate, and a removal there is the repair rather than the
// offence. [ruleCaps] is exercised by the pure table, which is where a rule no verb can break
// belongs.
//
// THE ORDER OF THE TWO KINDS OF REFUSAL IS A DECISION AND IT IS NOT [Group.SetRole]'s. SetRole
// answers its caller check FIRST, "so that a member is still answered R4 and not this", because the
// predicate CANNOT decide SetRole's case (ruling 15: a named member's no-op policy commit is
// indistinguishable from ruling 12's path-only self-heal). Here the predicate decides every
// authority question, so nothing is taken from it: what is answered first is the set of requests for
// which NO commit exists, whoever asks -- an identity with no leaf, this device's own identity, and
// the identity that owns the group. A MEMBER asking to remove the OWNER is therefore answered
// [ErrRemoveOwner] and not R3, deliberately: "nobody removes the owner, transfer first" is true and
// names the door, where "only the owner may remove an admin" invites a member to think an admin
// could.
//
// WHAT WOULD GO RED: skip the send-side decision and every REFUSED row is allowed and reaches the
// capture; allow self-removal and the two ErrRemoveSelf rows answer R6c's
// [ErrCommitIdentityChanged] instead -- which is the sentence ledger item 258 names as the wrong one
// for somebody who pressed Leave.
func TestEveryRemoveMemberRefusalIsTheRuleOrTheDoorThatNamesIt(t *testing.T) {
	ctx := context.Background()
	world := removeWorld(t, map[string]mls.Role{
		"adminA": mls.RoleAdmin, "adminB": mls.RoleAdmin,
		"carol": mls.RoleMember, "obs": mls.RoleObserver,
	}, "owner", "adminA", "adminB", "carol", "obs")
	stranger := world.device("stranger")

	// the roles this world stands at, read back off the verb's own read surface so the table
	// below is over the roles the predicate will judge and not over the ones this case intended
	for name, want := range map[string]string{
		"owner": "owner", "adminA": "admin", "adminB": "admin", "carol": "member", "obs": "observer",
	} {
		if got, err := world.member(name).group.MyRole(); err != nil || got != want {
			t.Fatalf("CONTROL FAILED: %s reads its own role as %q, %v; want %q -- the table below "+
				"would be judging different roles from the ones it names", name, got, err, want)
		}
	}

	byName := []struct {
		name    string
		caller  string
		subject []byte
		want    error
	}{
		{
			name:    "an admin asking to remove the OWNER",
			caller:  "adminA",
			subject: world.member("owner").dev.identityPub,
			want:    ErrRemoveOwner,
		},
		{
			name:    "a member asking to remove the OWNER",
			caller:  "carol",
			subject: world.member("owner").dev.identityPub,
			want:    ErrRemoveOwner,
		},
		{
			name:    "the OWNER asking to remove itself",
			caller:  "owner",
			subject: world.member("owner").dev.identityPub,
			want:    ErrRemoveOwner,
		},
		{
			name:    "a member asking to remove ITSELF, which is a Leave and not this verb",
			caller:  "carol",
			subject: world.member("carol").dev.identityPub,
			want:    ErrRemoveSelf,
		},
		{
			name:    "an admin asking to remove itself",
			caller:  "adminA",
			subject: world.member("adminA").dev.identityPub,
			want:    ErrRemoveSelf,
		},
		{
			name:    "the owner asking to remove an identity that holds no leaf",
			caller:  "owner",
			subject: stranger.identityPub,
			want:    ErrNoSuchMember,
		},
	}
	for _, one := range byName {
		t.Run(one.name, func(t *testing.T) {
			who := world.member(one.caller)
			epoch, stats := who.group.Epoch(), who.group.Stats()
			err := who.group.RemoveMember(ctx, one.subject)
			if !errors.Is(err, one.want) {
				t.Errorf("%s: %s answered %v, want %v", one.name, one.caller, err, one.want)
			}
			// A REQUEST REFUSED BY NAME IS NOT A ROLE REFUSAL AND MUST NOT BE COUNTED AS ONE: the
			// cgo header projects these as INVALID -- "a caller bug, nothing counted" -- where a
			// rule refusal is REFUSED with the counter moved.
			if errors.Is(err, ErrCommitUnauthorized) {
				t.Errorf("%s: %v also wraps ErrCommitUnauthorized, which is what a RULE refusal "+
					"answers; a request no commit exists for is INVALID and not REFUSED", one.name, err)
			}
			if after := who.group.Stats(); after.CommitRefusedOwn != stats.CommitRefusedOwn {
				t.Errorf("%s: Stats.CommitRefusedOwn went %d -> %d over a by-name refusal",
					one.name, stats.CommitRefusedOwn, after.CommitRefusedOwn)
			}
			removeNothingBuilt(t, who, one.name, epoch)
		})
	}

	byRule := []struct {
		name    string
		caller  string
		subject string
		want    error
	}{
		{
			name:    "a MEMBER removing an observer is R2's",
			caller:  "carol",
			subject: "obs",
			want:    ErrCommitRemoveByNonAdmin,
		},
		{
			name:    "an OBSERVER removing a member is R2's",
			caller:  "obs",
			subject: "carol",
			want:    ErrCommitRemoveByNonAdmin,
		},
		{
			name:    "an ADMIN removing an ADMIN is R3's",
			caller:  "adminA",
			subject: "adminB",
			want:    mls.ErrAdminRemovedByNonOwner,
		},
		{
			name:    "a MEMBER removing an ADMIN is R3's too",
			caller:  "carol",
			subject: "adminB",
			want:    mls.ErrAdminRemovedByNonOwner,
		},
	}
	for _, one := range byRule {
		t.Run(one.name, func(t *testing.T) {
			who, subject := world.member(one.caller), world.member(one.subject)
			epoch, stats := who.group.Epoch(), who.group.Stats()
			err := who.group.RemoveMember(ctx, subject.dev.identityPub)
			if !errors.Is(err, ErrCommitUnauthorized) {
				t.Errorf("%s: %s answered %v, which does not wrap ErrCommitUnauthorized", one.name, one.caller, err)
			}
			if !errors.Is(err, one.want) {
				t.Errorf("%s: %s answered %v, which does not wrap the rule %v", one.name, one.caller, err, one.want)
			}
			if after := who.group.Stats(); after.CommitRefusedOwn != stats.CommitRefusedOwn+1 {
				t.Errorf("%s: Stats.CommitRefusedOwn went %d -> %d, want one more",
					one.name, stats.CommitRefusedOwn, after.CommitRefusedOwn)
			}
			removeNothingBuilt(t, who, one.name, epoch)
		})
	}

	// ── THE CONTROLS: what §11's table DOES permit reaches the last door ─────────────────────
	//
	// Without these every refusal above is satisfied by a verb that refuses everything.
	for _, one := range []struct {
		name    string
		caller  string
		subject string
	}{
		{"the OWNER removing an admin", "owner", "adminB"},
		{"an ADMIN removing a member", "adminA", "carol"},
		{"an ADMIN removing an observer", "adminA", "obs"},
	} {
		t.Run(one.name+" passes every rule", func(t *testing.T) {
			who, subject := world.member(one.caller), world.member(one.subject)
			capture := captureOutgoing(t, who)
			epoch := who.group.Epoch()
			err := who.group.RemoveMember(ctx, subject.dev.identityPub)
			if !errors.Is(err, errRemoveCaptureStop) {
				t.Fatalf("CONTROL FAILED: %s answered %v, and this case's authorizer is the only "+
					"thing that should have refused it -- so a rule refused a removal §11's table "+
					"permits", one.name, err)
			}
			if capture.calls != 1 {
				t.Fatalf("CONTROL FAILED: %s: the authorizer saw %d decision(s), want 1", one.name, capture.calls)
			}
			if got := capture.decision.RemovedLeaves; len(got) != 1 || got[0] != subject.leaf {
				t.Errorf("%s: the decision removes %v, want just leaf %d", one.name, got, subject.leaf)
			}
			if got := capture.decision.CommitterRole; got != mustRoleName(t, who) {
				t.Errorf("%s: the decision names the committer a %q", one.name, got)
			}
			removeNothingBuilt(t, who, one.name, epoch)
		})
	}
}

// mustRoleName is a member's own role as its group reads it, for the assertion that the decision the
// verb built names the committer the role the group gives it.
func mustRoleName(t *testing.T, who *roleMember) string {
	t.Helper()
	role, err := who.group.MyRole()
	if err != nil {
		t.Fatalf("%s's MyRole: %v", who.name, err)
	}
	return role
}

// THE VERB'S FAN-OUT EXCLUSION IS THE STAGED COMMIT'S OWN, OVER A REMOVAL THE VERB DERIVED: every
// leaf the removed identity holds is left out, and every survivor is addressed.
//
// WHY IT IS HERE AND NOT ONLY IN pqrotation_test.go. That file holds the exclusion over commits a
// case builds; this holds it over the commit [Group.RemoveMember] derives, which is the arm ruling
// 51 exists for -- "the omission class an arm can cause by forgetting an argument is gone by
// construction" is a claim about a SIGNATURE, and the way to measure it from a verb is to take the
// verb's own leaf vector into the seam and ask the fan-out what it addresses. With a two-leaf
// identity the difference matters twice: a derivation that named one leaf would seal the epoch's own
// post-quantum secret straight to the other one.
//
// THE CONTROL IS IN THE SAME LOOP: every surviving leaf IS addressed, so the exclusion cannot be
// told from an empty fan-out. And the size is asserted against the live tree rather than against
// pending.MemberCount, which is false on every Add.
func TestTheFanOutForAVerbDerivedRemovalAddressesNeitherLeafOfTheRemovedIdentity(t *testing.T) {
	ctx := context.Background()
	world := removeWorld(t, map[string]mls.Role{"bob": mls.RoleAdmin, "carol": mls.RoleMember},
		"owner", "bob", "carol")
	owner, bob, carol := world.member("owner"), world.member("bob"), world.member("carol")

	laptop := claimingKeyPackage(t, filepath.Join(world.root, "carol-laptop"), carol.dev.identityPub)
	added := world.commitAndPublish(carol, "CommitAdd of carol's own second device", func() ([]byte, []byte, []byte, error) {
		return carol.handle.CommitAdd([][]byte{laptop})
	})
	for _, honest := range []*roleMember{owner, bob} {
		world.ingest(honest, carol, added)
	}

	capture := captureOutgoing(t, owner)
	if err := owner.group.RemoveMember(ctx, carol.dev.identityPub); !errors.Is(err, errRemoveCaptureStop) {
		t.Fatalf("the owner's RemoveMember answered %v, want this case's authorizer refusal", err)
	}
	owner.group.device.commitAuthorizer = nil
	leaves, list := capture.decision.RemovedLeaves, capture.decision.ExtensionsAfter
	if len(leaves) != 2 {
		t.Fatalf("the verb derived %d leaf/leaves for an identity holding two", len(leaves))
	}

	liveCount := owner.handle.MemberCount()
	if _, _, _, err := owner.handle.CommitRemoveWithExtensions(leaves, list); err != nil {
		t.Fatalf("staging the verb's own removal: %v", err)
	}
	defer owner.handle.ClearPendingCommit()
	pending, err := owner.handle.PendingEpoch()
	if err != nil {
		t.Fatalf("the staged commit's facts: %v", err)
	}
	if got := len(pending.RemovedLeaves); got != 2 {
		t.Fatalf("the staged commit names %d removed leaf/leaves (%v), and the fan-out's exclusion "+
			"is read off exactly this field", got, pending.RemovedLeaves)
	}
	targets, err := owner.group.wrapTargetsAtLocked(pending)
	if err != nil {
		t.Fatalf("the fan-out for the epoch the verb's removal opens: %v", err)
	}
	addressed := map[uint32]bool{}
	for _, target := range targets {
		addressed[target.leaf] = true
	}
	for _, leaf := range leaves {
		if addressed[leaf] {
			t.Errorf("the fan-out for epoch %d addresses leaf %d, which is one of the leaves the "+
				"verb's own removal takes out. This epoch's pq_secret would be sealed straight to "+
				"the X-Wing key of the member the commit exists to shut out", pending.Epoch, leaf)
		}
	}
	for _, survivor := range []*roleMember{owner, bob} {
		if !addressed[survivor.leaf] {
			t.Fatalf("CONTROL FAILED: the fan-out does not address %s at leaf %d, so the exclusion "+
				"above cannot be told from a fan-out that addresses nobody; addressed %v",
				survivor.name, survivor.leaf, addressed)
		}
	}
	if len(targets) != liveCount-2 {
		t.Errorf("the fan-out addresses %d leaves over a live tree of %d with two removals, want %d",
			len(targets), liveCount, liveCount-2)
	}
	t.Logf("the verb's removal of two leaves leaves a fan-out of %d over a live tree of %d, staged "+
		"RemovedLeaves %v", len(targets), liveCount, pending.RemovedLeaves)
}

// A COMMITTER FILES WHAT ITS OWN REMOVAL TOOK OUT OF THE GROUP, THE WAY A RECEIVER DOES.
//
// WHY THIS IS A CASE AND NOT A COMMENT. [Group.ingestCommitLocked]'s step (5a) has filed
// [Group.departedAt] and pruned [Group.peerHeads] since ledger item 245, on the arm that ingests
// SOMEBODY ELSE'S commit. The publishing arm had nothing to file, because no verb could put a leaf
// in the staged commit's RemovedLeaves -- [Group.RemoveMember] is the first, and the admin that
// removes somebody is the ONE device in the group that does not learn the removal from an ingest.
// Without the filing that admin loses the removed member's history at its next restart: the cursor
// is not persisted, so it re-walks records whose sender_handle [Group.leavesAtLocked] can no longer
// resolve, and each is abandoned after [maxRecordAttempts].
//
// IT IS MEASURED THROUGH [Group.leavesAtLocked], which is the reader both costs run through, at the
// epochs either side of the removal -- and the CONTROL is the same query at the epoch ABOVE it,
// where the leaf must NOT resolve. A table that filed every leaf unconditionally would pass the
// first clause and fail the second.
//
// WHAT IS NOT HERE: the publish path itself, for this file's stated reason. The two lines under test
// run inside [Group.publishCommitLocked] between the merge and [Group.crossEpochLadderLocked]; what
// is driven here is the same pair over the same vector, and cp3b's restart clause is the end to end.
func TestADepartedLeafStaysResolvableAtTheEpochsItStoodAtAndNotAbove(t *testing.T) {
	world := removeWorld(t, map[string]mls.Role{"bob": mls.RoleAdmin, "carol": mls.RoleMember},
		"owner", "bob", "carol")
	owner, carol := world.member("owner"), world.member("carol")
	handleOf := func(leaf uint32) [16]byte {
		return messagegroup.SenderHandle(world.groupHandleKey, leaf)
	}

	stood := owner.group.Epoch()
	// the CONTROL FIRST: while carol stands in the tree her handle resolves at that epoch
	atStood, err := owner.group.leavesAtLocked(stood)
	if err != nil {
		t.Fatalf("the membership at epoch %d: %v", stood, err)
	}
	if atStood[handleOf(carol.leaf)] != carol.leaf {
		t.Fatalf("CONTROL FAILED: carol's handle does not resolve to leaf %d at epoch %d, where she "+
			"stands in the live tree", carol.leaf, stood)
	}

	// the removal, through the seam over the vector the verb derives, and the filing the publish
	// path makes from the staged commit's own answer
	opens := stood + 1
	if _, _, _, err := owner.handle.CommitRemoveWithExtensions([]uint32{carol.leaf},
		removeListWithout(t, world, owner, carol)); err != nil {
		t.Fatalf("staging the removal: %v", err)
	}
	pending, err := owner.handle.PendingEpoch()
	if err != nil {
		t.Fatalf("the staged commit's facts: %v", err)
	}
	if err := owner.handle.MergePendingCommit(); err != nil {
		t.Fatalf("merging the removal: %v", err)
	}
	owner.group.mutex.Lock()
	owner.group.noteDepartedLeavesLocked(pending.RemovedLeaves, opens)
	owner.group.pruneRemovedLaddersLocked(pending.RemovedLeaves)
	owner.group.mutex.Unlock()

	// ── THE PROPERTY: the leaf still resolves BELOW the epoch the removal opened ────────────
	below, err := owner.group.leavesAtLocked(stood)
	if err != nil {
		t.Fatalf("the membership at epoch %d after the removal: %v", stood, err)
	}
	if below[handleOf(carol.leaf)] != carol.leaf {
		t.Errorf("after removing carol at epoch %d the committer can no longer resolve her handle "+
			"at epoch %d, where she stood. Her records sit BELOW the removing commit and the "+
			"cursor is not persisted, so this device abandons every one of them at its next restart",
			opens, stood)
	}
	// ── AND NOT AT OR ABOVE IT, which is what makes the table a pre-filter and not a leak ────
	above, err := owner.group.leavesAtLocked(opens)
	if err != nil {
		t.Fatalf("the membership at epoch %d: %v", opens, err)
	}
	if _, resolves := above[handleOf(carol.leaf)]; resolves {
		t.Errorf("carol's handle still resolves at epoch %d, which her removal opened and at which "+
			"no occupant of leaf %d stood", opens, carol.leaf)
	}
	// and the ladder bookkeeping for the departed leaf is gone, which is what stops
	// crossEpochLadderLocked re-tracking a ratchet for a leaf nobody can write from
	owner.group.mutex.Lock()
	for ladder := range owner.group.peerHeads {
		if ladder.leaf == carol.leaf {
			t.Errorf("the committer still holds a receiver-ladder head for the leaf its own commit removed")
		}
	}
	owner.group.mutex.Unlock()
	t.Logf("the committer resolves the departed leaf at epoch %d and not at %d", stood, opens)
}

// removeListWithout is the post-commit extension list for a removal of one member, built the way
// [Group.RemoveMember] builds it: the live policy with that identity's entry dropped, through mls's
// own ExtensionsWithGroupPolicy over the live list.
func removeListWithout(t *testing.T, world *roleWorld, committer *roleMember, subject *roleMember) []messagegroup.ExtensionBytes {
	t.Helper()
	live, err := committer.group.contextExtensionsLocked()
	if err != nil {
		t.Fatalf("the live extension list: %v", err)
	}
	policy := world.policyOf(committer)
	policy.RemoveRole(subject.dev.identityPub)
	replaced, err := mls.ExtensionsWithGroupPolicy(mlsExtensionsOf(live), world.policyBody(policy))
	if err != nil {
		t.Fatalf("the list the removal would install: %v", err)
	}
	return seamExtensionsOf(replaced)
}

// THE MEMBER A RemoveMember CALL TAKES OUT CANNOT DERIVE THE EPOCH ITS OWN REMOVAL OPENS, AND EVERY
// SURVIVOR CAN -- OVER A REMOVAL THE VERB ITSELF DERIVED, WITH A ROTATING FAN-OUT.
//
// THIS IS THE PROPERTY THE WHOLE REMOVAL TRACK EXISTS FOR, asked of the product verb. Ledger item
// 243 made pq_secret a group-lifetime value "on the explicit condition that rotating it is a
// prerequisite of shipping REMOVAL"; item 251's ruling 42 clause (a) states what has to be true --
// a removal at epoch n denies the removed member storage_root[n+1], including against a future
// adversary who breaks X25519 while holding an archive. pqrotation_test.go holds it over commits a
// CASE builds. This holds it over the commit [Group.RemoveMember] derives, which is the arm ruling
// 51's derivation was built for, and it does it with a TWO-LEAF identity so that a derivation which
// named one leaf would be caught sealing the epoch's own secret to the other one.
//
// THE THREE CONTROLS, EACH FIRING FOR ITS OWN REASON. Before the removal all three members --
// including the one about to go -- derive ONE storage root, so the denial below is not a group that
// never agreed about anything. In the fan-out loop every SURVIVING leaf is addressed, so the
// exclusion cannot be told from an empty fan-out. And in the counterfactual the epoch's OWN secret
// mixed with the granted exporter DOES reproduce the survivors' root, which pins the difference on
// the post-quantum half rather than on the exporter.
//
// THE COUNTERFACTUAL GRANTS THE REMOVED MEMBER MORE THAN MLS DOES, deliberately: mls_secret[n+1] is
// handed to it, which a Remove's forced UpdatePath denies it outright, so the only variable left is
// pq_secret -- the half ledger item 243 is about.
//
// WHAT WOULD GO RED: derive one of the two leaves (the other one's wrap is sealed and the removed
// identity follows the epoch); leave the removed leaves in the fan-out (the "no wrap" clause fails);
// reuse the group's existing secret instead of drawing one (the counterfactual reproduces the
// survivors' root and the removal buys nothing).
func TestTheRemovedMembersOwnRemovalOpensAnEpochItCannotDerive(t *testing.T) {
	ctx := context.Background()
	world := newRotWorld(t, "alice", "bob", "carol")
	alice, bob, carol := world.member("alice"), world.member("bob"), world.member("carol")

	// NAME EVERY MEMBER IN THE POLICY, which is the state the removal track was blocked on: an
	// identity any SetRole has named keeps an entry for ever, and a bare Remove of its last leaf is
	// then an R0c phantom at every receiver (ledger item 242's R2).
	named := rotPolicyOf(t, alice)
	named.SetRole(bob.dev.identityPub, mls.RoleAdmin)
	named.SetRole(carol.dev.identityPub, mls.RoleMember)
	naming := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitPolicy(rotPolicyBody(t, named))
	})
	for _, who := range []*rotMember{bob, carol} {
		if err := world.deliver(who, naming.page()...); err != nil {
			t.Fatalf("%s's walk over the policy that names everybody: %v", who.name, err)
		}
	}
	if !removePolicyNames(rotPolicyOf(t, bob), carol.dev.identityPub) {
		t.Fatalf("CONTROL FAILED: the live policy does not name carol, so the entry the verb drops " +
			"below was never there")
	}

	// CAROL'S SECOND DEVICE, added by carol herself (MASTER section 11's self-service rule and R6a).
	laptopKeyPackage := claimingKeyPackage(t, world.rootOf("carol-laptop"), carol.dev.identityPub)
	laptop := world.rotate(carol, func() ([]byte, []byte, []byte, error) {
		return carol.handle.CommitAdd([][]byte{laptopKeyPackage})
	})
	for _, who := range []*rotMember{alice, bob} {
		if err := world.deliver(who, laptop.page()...); err != nil {
			t.Fatalf("%s's walk over carol's own second device: %v", who.name, err)
		}
	}

	// THE CONTROL: at the epoch before the removal all three agree
	before := world.storageRootOf(alice)
	for _, who := range []*rotMember{bob, carol} {
		if got := world.storageRootOf(who); !bytes.Equal(before, got) {
			t.Fatalf("CONTROL FAILED: at epoch %d %s's storage root is not alice's, so this group "+
				"never agreed about anything and the removal below would prove nothing",
				alice.group.Epoch(), who.name)
		}
	}
	retained := append([]byte(nil), carol.group.pqSecretLocked()...)
	t.Logf("CONTROL HELD: at epoch %d all three members derive one storage root", alice.group.Epoch())

	// THE VERB: alice removes carol's identity, and the decision it derived is captured
	capture := captureOutgoingOn(t, alice.group)
	if err := alice.group.RemoveMember(ctx, carol.dev.identityPub); !errors.Is(err, errRemoveCaptureStop) {
		t.Fatalf("alice's RemoveMember answered %v, want this case's authorizer refusal", err)
	}
	alice.group.device.commitAuthorizer = nil
	if capture.calls != 1 {
		t.Fatalf("the configured authorizer saw %d decision(s), want 1: a rule refused the verb's "+
			"derivation", capture.calls)
	}
	leaves, list := capture.decision.RemovedLeaves, capture.decision.ExtensionsAfter
	if len(leaves) != 2 {
		t.Fatalf("the verb derived %d leaf/leaves (%v) for an identity holding two devices; every "+
			"clause below would be about a different commit", len(leaves), leaves)
	}

	// the rotation the verb's own two arguments open, through the production draw and the
	// production fan-out ([Group.stageEpochRotationLocked]); only the submit is the harness's
	removal := world.rotate(alice, func() ([]byte, []byte, []byte, error) {
		return alice.handle.CommitRemoveWithExtensions(leaves, list)
	})

	// THE FAN-OUT ADDRESSES NEITHER REMOVED LEAF, AND EVERY SURVIVOR
	addressed := map[uint32]bool{}
	for _, target := range removal.targets {
		addressed[target.leaf] = true
	}
	for _, leaf := range leaves {
		if addressed[leaf] {
			t.Fatalf("the fan-out for epoch %d addresses leaf %d, which the verb's own removal takes "+
				"out: this epoch's pq_secret is sealed straight to the X-Wing key of a member the "+
				"commit exists to shut out", removal.opens, leaf)
		}
	}
	for _, who := range []*rotMember{alice, bob} {
		if !addressed[who.leaf] {
			t.Fatalf("CONTROL FAILED: the fan-out does not address %s at leaf %d, so the exclusion "+
				"above cannot be told from a fan-out that addresses nobody; addressed %v",
				who.name, who.leaf, addressed)
		}
	}

	// EVERY SURVIVOR FOLLOWS, AND CONVERGES
	if err := world.deliver(bob, removal.page()...); err != nil {
		t.Fatalf("bob's walk over the removal: %v", err)
	}
	after := world.storageRootOf(alice)
	if bytes.Equal(after, before) {
		t.Fatalf("epoch %d's storage root is the one before it; the removal rotated nothing", removal.opens)
	}
	if got := world.storageRootOf(bob); !bytes.Equal(after, got) {
		t.Fatalf("after the removal bob's storage root at epoch %d is not alice's: a member that "+
			"followed the commit did not follow the secret", removal.opens)
	}
	for _, who := range []*rotMember{alice, bob} {
		members, err := who.group.Members()
		if err != nil {
			t.Fatalf("%s's roster after the removal: %v", who.name, err)
		}
		if len(members) != 2 {
			t.Errorf("%s reads %d members after one call removed one identity's two leaves out of "+
				"four, want 2", who.name, len(members))
		}
		for _, member := range members {
			if bytes.Equal(member.IdentityPub, carol.dev.identityPub) {
				t.Errorf("%s still reads carol's identity at leaf %d after the removal", who.name, member.LeafIndex)
			}
		}
		if removePolicyNames(rotPolicyOf(t, who), carol.dev.identityPub) {
			t.Errorf("%s's live policy still names carol after the removal", who.name)
		}
		// the positive control in the same loop: the survivors are still named
		for _, other := range []*rotMember{alice, bob} {
			if !removePolicyNames(rotPolicyOf(t, who), other.dev.identityPub) {
				t.Errorf("CONTROL FAILED: %s's live policy does not name %s either, so the absence "+
					"above is satisfied by a policy that lost every entry", who.name, other.name)
			}
		}
	}

	// AND THE REMOVED MEMBER CANNOT FOLLOW
	err := world.deliver(carol, removal.page()...)
	if err == nil {
		t.Fatalf("the removed member walked the commit that removes it and is now at epoch %d", carol.group.Epoch())
	}
	if !errors.Is(err, mls.ErrRemovedFromGroup) {
		t.Errorf("the removed member's walk answered %v; mls.ErrRemovedFromGroup is what its own "+
			"ApplyCommit answers and is what a carrier for this state would be built over (ledger "+
			"item 257's ruling 52, which is NOT in this step)", err)
	}
	if got := carol.group.Epoch(); got != removal.opens-1 {
		t.Errorf("the removed member is at epoch %d, want %d: it did not follow its own removal", got, removal.opens-1)
	}
	if _, held := carol.group.pqSecretAtLocked(removal.opens); held {
		t.Errorf("the removed member holds a pq_secret for epoch %d", removal.opens)
	}

	// THE COUNTERFACTUAL, WITH ITS OWN CONTROL BESIDE IT
	granted, err := alice.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		t.Fatalf("the exporter at epoch %d: %v", removal.opens, err)
	}
	if !bytes.Equal(messagegroup.StorageRoot(granted, removal.pqSecret), after) {
		t.Fatalf("CONTROL FAILED: the epoch-%d exporter mixed with the epoch's OWN secret is not the "+
			"root the survivors derived, so the counterfactual below is not about the pq half", removal.opens)
	}
	if bytes.Equal(messagegroup.StorageRoot(granted, retained), after) {
		t.Fatalf("the removed member's RETAINED pq_secret, mixed with the exporter of the epoch its "+
			"own removal opened, reproduces the survivors' storage_root[%d]. Ledger item 243's whole "+
			"subject: the removal removed nothing", removal.opens)
	}
	t.Logf("one RemoveMember call took leaves %v out of the tree and the policy; the survivors "+
		"converge at epoch %d and the removed identity derives neither that epoch nor its secret",
		leaves, removal.opens)
}

// rotPolicyOf is a rot world member's live policy, decoded off its own group context.
func rotPolicyOf(t *testing.T, member *rotMember) *mls.GroupPolicyExtension {
	t.Helper()
	extensions, err := member.group.contextExtensionsLocked()
	if err != nil {
		t.Fatalf("%s's group context: %v", member.name, err)
	}
	policy, err := mls.GroupPolicyOf(mlsExtensionsOf(extensions))
	if err != nil {
		t.Fatalf("%s's policy: %v", member.name, err)
	}
	return policy
}

// rotPolicyBody encodes a policy as the body CommitPolicy takes.
func rotPolicyBody(t *testing.T, policy *mls.GroupPolicyExtension) []byte {
	t.Helper()
	body, err := policyBodyOf(policy)
	if err != nil {
		t.Fatalf("encoding the policy: %v", err)
	}
	return body
}
