package urmessage

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/connect/mls/syntax"
)

// ── the rules, one table, no device ──────────────────────────────────────────────────────────
//
// [authorizeCommit] is pure over a [CommitAuthorization], so every rule is a row here: a group as
// it stands, what one commit does to it, who committed, and the sentinel the rule answers. The
// value is built the way [Group.commitAuthorizationLocked] builds it -- the policies are ENCODED
// into extension lists and decoded back through mls.GroupPolicyOf, the roles are read through
// roleNameIn -- so a row exercises the same derivation the ingest path runs and not a hand-set
// field the path never produces. The end-to-end cases, through real devices and the seam's own
// commits, are rolesingest_test.go.

// roleIdentity is a deterministic 32-octet identity for a named member.
func roleIdentity(name string) []byte {
	sum := sha256.Sum256([]byte("role-model identity: " + name))
	return sum[:]
}

// roleLeaf is one occupied leaf: which member's identity it carries.
type roleLeaf struct {
	leaf uint32
	who  string
}

// roleScenario is one commit against one group, in names.
type roleScenario struct {
	committer string
	before    []roleLeaf
	after     []roleLeaf
	added     []uint32
	removed   []uint32
	updated   []uint32

	// the named roles on each side; nil after means the policy is unchanged
	rolesBefore map[string]mls.Role
	rolesAfter  map[string]mls.Role

	retentionBefore mls.RetentionPolicy
	retentionAfter  *mls.RetentionPolicy
	bucketsBefore   []uint8
	bucketsAfter    []uint8 // nil means unchanged
	serverIdBefore  []byte
	serverIdAfter   []byte // nil means unchanged

	// the shapes only a hostile committer produces
	policyAfterBody       []byte // replaces the encoded post-commit policy body
	dropPolicyAfter       bool
	dropCapabilitiesAfter bool
	capabilitiesAfter     []byte   // replaces the 0x0003 body
	committerRole         string   // overrides the derived role name
	keylessAfter          []uint32 // leaves of the post-commit tree that carry no 0xF002
}

// roleCapabilities is a stand-in required_capabilities body: the rules compare octets and never
// read it.
var roleCapabilities = []byte{0x00, 0x02, 0x00, 0x03, 0x00}

func rolePolicyOf(t *testing.T, roles map[string]mls.Role, retention mls.RetentionPolicy,
	buckets []uint8, serverId []byte) *mls.GroupPolicyExtension {

	t.Helper()
	policy := &mls.GroupPolicyExtension{
		RetentionPolicy:     retention,
		DisappearingBuckets: append([]uint8(nil), buckets...),
		ServerId:            append([]byte(nil), serverId...),
	}
	for who, role := range roles {
		policy.Roles = append(policy.Roles, mls.RoleEntry{MemberId: roleIdentity(who), Role: role})
	}
	if err := policy.Canonicalize(); err != nil {
		t.Fatalf("canonicalizing the scenario's policy: %v", err)
	}
	return policy
}

func roleExtensionsOf(t *testing.T, policy *mls.GroupPolicyExtension, capabilities []byte) []messagegroup.ExtensionBytes {
	t.Helper()
	encoded, err := policy.Encode()
	if err != nil {
		t.Fatalf("encoding the scenario's policy: %v", err)
	}
	return []messagegroup.ExtensionBytes{
		{Type: 0x0003, Data: append([]byte(nil), capabilities...)},
		{Type: uint16(encoded.ExtensionType), Data: encoded.ExtensionData},
	}
}

// roleMembersOf is one membership as the ingest path would hand it over: every leaf carries its
// leaf keys, as MemberAt's refusal makes true of the pre-commit side, except the ones a scenario
// names keyless, which is the post-commit reading a hostile Add produces.
func roleMembersOf(leaves []roleLeaf, policy *mls.GroupPolicyExtension, keyless []uint32) []CommitMember {
	out := []CommitMember{}
	for _, one := range leaves {
		identity := roleIdentity(one.who)
		handle := make([]byte, 16)
		handle[0] = byte(one.leaf)
		out = append(out, CommitMember{
			Leaf:         one.leaf,
			SenderHandle: handle,
			IdentityPub:  identity,
			Role:         roleNameIn(policy, identity),
			HasLeafKeys:  !leafSetOf(keyless)[one.leaf],
		})
	}
	return out
}

// authorization builds the value the ingest path would hand [authorizeCommit] for this scenario.
func (self roleScenario) authorization(t *testing.T) *CommitAuthorization {
	t.Helper()
	policyBefore := rolePolicyOf(t, self.rolesBefore, self.retentionBefore, self.bucketsBefore, self.serverIdBefore)
	extensionsBefore := roleExtensionsOf(t, policyBefore, roleCapabilities)

	rolesAfter := self.rolesAfter
	if rolesAfter == nil {
		rolesAfter = self.rolesBefore
	}
	retentionAfter := self.retentionBefore
	if self.retentionAfter != nil {
		retentionAfter = *self.retentionAfter
	}
	bucketsAfter := self.bucketsBefore
	if self.bucketsAfter != nil {
		bucketsAfter = self.bucketsAfter
	}
	serverIdAfter := self.serverIdBefore
	if self.serverIdAfter != nil {
		serverIdAfter = self.serverIdAfter
	}
	capabilitiesAfter := roleCapabilities
	if self.capabilitiesAfter != nil {
		capabilitiesAfter = self.capabilitiesAfter
	}
	extensionsAfter := roleExtensionsOf(t, rolePolicyOf(t, rolesAfter, retentionAfter, bucketsAfter, serverIdAfter), capabilitiesAfter)
	if self.policyAfterBody != nil {
		extensionsAfter[1].Data = self.policyAfterBody
	}
	if self.dropPolicyAfter {
		extensionsAfter = extensionsAfter[:1]
	}
	if self.dropCapabilitiesAfter {
		extensionsAfter = extensionsAfter[1:]
	}

	// THE SAME DERIVATION THE INGEST PATH RUNS
	decodedBefore, errBefore := mls.GroupPolicyOf(mlsExtensionsOf(extensionsBefore))
	decodedAfter, errAfter := mls.GroupPolicyOf(mlsExtensionsOf(extensionsAfter))
	members := roleMembersOf(self.before, decodedBefore, nil)
	var committerLeaf uint32
	var committerIdentity []byte
	found := false
	for _, one := range self.before {
		if one.who == self.committer {
			committerLeaf = one.leaf
			committerIdentity = roleIdentity(one.who)
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("the scenario's committer %q holds no leaf before the commit", self.committer)
	}
	committerRole := roleNameIn(decodedBefore, committerIdentity)
	if self.committerRole != "" {
		committerRole = self.committerRole
	}
	return &CommitAuthorization{
		GroupId:           make([]byte, GroupIdBytes),
		Epoch:             2,
		CommitterLeaf:     committerLeaf,
		CommitterIdentity: committerIdentity,
		CommitterRole:     committerRole,
		AddedLeaves:       self.added,
		RemovedLeaves:     self.removed,
		UpdatedLeaves:     self.updated,
		Members:           members,
		MembersAfter:      roleMembersOf(self.after, decodedAfter, self.keylessAfter),
		PolicyBefore:      decodedBefore,
		PolicyAfter:       decodedAfter,
		PolicyBeforeErr:   errBefore,
		PolicyAfterErr:    errAfter,
		ExtensionsBefore:  extensionsBefore,
		ExtensionsAfter:   extensionsAfter,
	}
}

// the baseline group every row starts from: an owner, an admin, an unnamed member, a named
// observer and a second unnamed member. Leaves are the names' initial positions.
var (
	roleBaseline      = []roleLeaf{{0, "owner"}, {1, "admin"}, {2, "member"}, {3, "observer"}, {4, "member-2"}}
	roleBaselineRoles = map[string]mls.Role{"owner": mls.RoleOwner, "admin": mls.RoleAdmin, "observer": mls.RoleObserver}
)

// with is the baseline policy with some entries changed or (nil role value -> ) dropped.
func rolesWith(changes map[string]*mls.Role) map[string]mls.Role {
	out := map[string]mls.Role{}
	for who, role := range roleBaselineRoles {
		out[who] = role
	}
	for who, role := range changes {
		if role == nil {
			delete(out, who)
			continue
		}
		out[who] = *role
	}
	return out
}

func rolePtr(role mls.Role) *mls.Role { return &role }

// leavesWith is the baseline leaves with some replaced (by leaf) or dropped (who == "").
func leavesWith(changes ...roleLeaf) []roleLeaf {
	out := []roleLeaf{}
	for _, one := range roleBaseline {
		replaced := false
		for _, change := range changes {
			if change.leaf == one.leaf {
				if change.who != "" {
					out = append(out, change)
				}
				replaced = true
			}
		}
		if !replaced {
			out = append(out, one)
		}
	}
	for _, change := range changes {
		isNew := true
		for _, one := range roleBaseline {
			if one.leaf == change.leaf {
				isNew = false
			}
		}
		if isNew && change.who != "" {
			out = append(out, change)
		}
	}
	return out
}

func TestEveryRuleOfTheRoleModelOverOneTable(t *testing.T) {
	twoOwners := &mls.GroupPolicyExtension{Roles: []mls.RoleEntry{
		{MemberId: roleIdentity("owner"), Role: mls.RoleOwner},
		{MemberId: roleIdentity("admin"), Role: mls.RoleOwner},
	}}
	if err := twoOwners.Canonicalize(); err != nil {
		t.Fatal(err)
	}
	twoOwnersBody, err := syntax.Marshal(twoOwners)
	if err != nil {
		t.Fatal(err)
	}
	noOwnerBody, err := syntax.Marshal(&mls.GroupPolicyExtension{Roles: []mls.RoleEntry{
		{MemberId: roleIdentity("admin"), Role: mls.RoleAdmin},
	}})
	if err != nil {
		t.Fatal(err)
	}
	longer := &mls.RetentionPolicy{DurableMs: 1 << 40}

	rows := []struct {
		name     string
		scenario roleScenario
		want     []error // every one must errors.Is; empty means allowed
	}{
		// ── allowed shapes, so a refusal below is a rule and not the builder ─────────────
		{"a path-only commit by the owner is allowed", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles}, nil},
		{"a path-only commit by an admin is allowed", roleScenario{
			committer: "admin", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles}, nil},
		{"a path-only commit by a member is allowed (ruling 12)", roleScenario{
			committer: "member", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles}, nil},
		{"a path-only commit by an observer is allowed (ruling 12)", roleScenario{
			committer: "observer", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles}, nil},

		// ── R0a: the policy after ────────────────────────────────────────────────────────
		{"R0a a commit that drops 0xF001 is refused with mls's absence", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			dropPolicyAfter: true}, []error{ErrCommitPolicyInvalid, mls.ErrNoGroupPolicy}},
		{"R0a a policy naming two owners is refused with mls's reason", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			policyAfterBody: twoOwnersBody}, []error{ErrCommitPolicyInvalid, mls.ErrMultipleOwners}},

		// ── R0b: every other extension ───────────────────────────────────────────────────
		{"R0b dropping required_capabilities is refused even for the owner", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			dropCapabilitiesAfter: true}, []error{ErrCommitExtensionChanged}},
		{"R0b rewriting required_capabilities is refused", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			capabilitiesAfter: []byte{0x00, 0x00}}, []error{ErrCommitExtensionChanged}},

		// ── R0c: phantom entries ─────────────────────────────────────────────────────────
		{"R0c the owner naming an identity with no leaf is refused", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"stranger": rolePtr(mls.RoleMember)})},
			[]error{ErrCommitPolicyPhantom}},
		{"R0c an admin removing the observer's only leaf and leaving its entry is refused", roleScenario{
			committer: "admin", before: roleBaseline, after: leavesWith(roleLeaf{3, ""}), removed: []uint32{3},
			rolesBefore: roleBaselineRoles}, []error{ErrCommitPolicyPhantom}},
		{"an observer removing its own second device is allowed (rulings 2 and 5)", roleScenario{
			committer: "observer", before: leavesWith(roleLeaf{5, "observer"}), after: roleBaseline, removed: []uint32{5},
			rolesBefore: roleBaselineRoles}, nil},

		// ── R6a: an Add claiming an identity already present ────────────────────────────
		{"R6a the admin adding a leaf that claims the owner's identity is refused", roleScenario{
			committer: "admin", before: roleBaseline, after: leavesWith(roleLeaf{5, "owner"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles}, []error{ErrCommitIdentityClaimed}},
		{"R6a the member adding a leaf that claims the admin's identity is refused", roleScenario{
			committer: "member", before: roleBaseline, after: leavesWith(roleLeaf{5, "admin"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles}, []error{ErrCommitIdentityClaimed}},
		{"the owner adding its own second device is allowed", roleScenario{
			committer: "owner", before: roleBaseline, after: leavesWith(roleLeaf{5, "owner"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles}, nil},
		{"the observer adding its own second device is allowed (ruling 5)", roleScenario{
			committer: "observer", before: roleBaseline, after: leavesWith(roleLeaf{5, "observer"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles}, nil},
		{"the member adding its own second device is allowed", roleScenario{
			committer: "member", before: roleBaseline, after: leavesWith(roleLeaf{5, "member"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles}, nil},

		// ── R6c: identity continuity and the declared leaf sets ─────────────────────────
		{"R6c the committer's own path swapping its identity to the owner's is refused", roleScenario{
			committer: "member", before: roleBaseline, after: leavesWith(roleLeaf{2, "owner"}),
			rolesBefore: roleBaselineRoles}, []error{ErrCommitIdentityChanged}},
		{"R6c an update that changes a leaf's identity is refused", roleScenario{
			committer: "owner", before: roleBaseline, after: leavesWith(roleLeaf{1, "stranger"}), updated: []uint32{1},
			rolesBefore: roleBaselineRoles}, []error{ErrCommitIdentityChanged}},
		{"R6c a leaf that vanishes without a declared remove is refused", roleScenario{
			committer: "owner", before: roleBaseline, after: leavesWith(roleLeaf{4, ""}),
			rolesBefore: roleBaselineRoles}, []error{ErrCommitIdentityChanged}},
		{"R6c a leaf that appears without a declared add is refused", roleScenario{
			committer: "owner", before: roleBaseline, after: leavesWith(roleLeaf{5, "stranger"}),
			rolesBefore: roleBaselineRoles}, []error{ErrCommitIdentityChanged}},
		{"R6c a declared add over a member that is not removed is refused", roleScenario{
			committer: "owner", before: roleBaseline, after: leavesWith(roleLeaf{4, "stranger"}), added: []uint32{4},
			rolesBefore: roleBaselineRoles}, []error{ErrCommitIdentityChanged}},
		{"an add landing in the leaf a removal blanked is allowed", roleScenario{
			committer: "owner", before: roleBaseline, after: leavesWith(roleLeaf{4, "stranger"}),
			removed: []uint32{4}, added: []uint32{4}, rolesBefore: roleBaselineRoles}, nil},
		{"an update that keeps a leaf's identity, committed by an admin, is allowed", roleScenario{
			committer: "admin", before: roleBaseline, after: roleBaseline, updated: []uint32{2},
			rolesBefore: roleBaselineRoles}, nil},

		// ── R6d: every post-commit leaf carries urmessage_leaf_keys ─────────────────────
		{"R6d an admin adding a leaf without leaf keys is refused", roleScenario{
			committer: "admin", before: roleBaseline, after: leavesWith(roleLeaf{5, "stranger"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles, keylessAfter: []uint32{5}}, []error{ErrCommitLeafWithoutKeys}},
		{"R6d the owner adding a leaf without leaf keys is refused", roleScenario{
			committer: "owner", before: roleBaseline, after: leavesWith(roleLeaf{5, "stranger"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles, keylessAfter: []uint32{5}}, []error{ErrCommitLeafWithoutKeys}},
		{"R6d a member adding its own device without leaf keys is refused", roleScenario{
			committer: "member", before: roleBaseline, after: leavesWith(roleLeaf{5, "member"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles, keylessAfter: []uint32{5}}, []error{ErrCommitLeafWithoutKeys}},
		{"R6d an update that drops a leaf's leaf keys is refused", roleScenario{
			committer: "admin", before: roleBaseline, after: roleBaseline, updated: []uint32{2},
			rolesBefore: roleBaselineRoles, keylessAfter: []uint32{2}}, []error{ErrCommitLeafWithoutKeys}},
		{"R6d the committer's own path dropping its leaf keys is refused", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline,
			rolesBefore: roleBaselineRoles, keylessAfter: []uint32{0}}, []error{ErrCommitLeafWithoutKeys}},

		// ── R1: who may add ──────────────────────────────────────────────────────────────
		{"R1 a member adding a new identity is refused (ruling 1)", roleScenario{
			committer: "member", before: roleBaseline, after: leavesWith(roleLeaf{5, "stranger"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles}, []error{ErrCommitAddByNonAdmin}},
		{"R1 an observer adding a new identity is refused", roleScenario{
			committer: "observer", before: roleBaseline, after: leavesWith(roleLeaf{5, "stranger"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles}, []error{ErrCommitAddByNonAdmin}},
		{"an admin adding a new identity is allowed", roleScenario{
			committer: "admin", before: roleBaseline, after: leavesWith(roleLeaf{5, "stranger"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles}, nil},
		{"the owner adding a new identity is allowed", roleScenario{
			committer: "owner", before: roleBaseline, after: leavesWith(roleLeaf{5, "stranger"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles}, nil},

		// ── R2: who may remove a member or observer ─────────────────────────────────────
		{"R2 a member removing another member is refused", roleScenario{
			committer: "member", before: roleBaseline, after: leavesWith(roleLeaf{4, ""}), removed: []uint32{4},
			rolesBefore: roleBaselineRoles}, []error{ErrCommitRemoveByNonAdmin}},
		{"R2 an observer removing a member is refused", roleScenario{
			committer: "observer", before: roleBaseline, after: leavesWith(roleLeaf{4, ""}), removed: []uint32{4},
			rolesBefore: roleBaselineRoles}, []error{ErrCommitRemoveByNonAdmin}},
		{"an admin removing a member is allowed", roleScenario{
			committer: "admin", before: roleBaseline, after: leavesWith(roleLeaf{4, ""}), removed: []uint32{4},
			rolesBefore: roleBaselineRoles}, nil},
		{"an admin removing an observer and its entry is allowed", roleScenario{
			committer: "admin", before: roleBaseline, after: leavesWith(roleLeaf{3, ""}), removed: []uint32{3},
			rolesBefore: roleBaselineRoles, rolesAfter: rolesWith(map[string]*mls.Role{"observer": nil})}, nil},
		{"a member removing its own second device is allowed (ruling 2)", roleScenario{
			committer: "member", before: leavesWith(roleLeaf{5, "member"}), after: roleBaseline, removed: []uint32{5},
			rolesBefore: roleBaselineRoles}, nil},

		// ── R3: only the owner removes an admin or the owner ─────────────────────────────
		{"R3 a member removing the owner is refused with mls.ErrAdminRemovedByNonOwner", roleScenario{
			committer: "member", before: roleBaseline, after: leavesWith(roleLeaf{0, ""}), removed: []uint32{0},
			rolesBefore: roleBaselineRoles}, []error{mls.ErrAdminRemovedByNonOwner}},
		{"R3 an admin removing the owner is refused", roleScenario{
			committer: "admin", before: roleBaseline, after: leavesWith(roleLeaf{0, ""}), removed: []uint32{0},
			rolesBefore: roleBaselineRoles}, []error{mls.ErrAdminRemovedByNonOwner}},
		{"R3 an admin removing another admin is refused", roleScenario{
			committer: "admin", before: leavesWith(roleLeaf{5, "admin-2"}), after: leavesWith(roleLeaf{5, ""}), removed: []uint32{5},
			rolesBefore: rolesWith(map[string]*mls.Role{"admin-2": rolePtr(mls.RoleAdmin)})},
			[]error{mls.ErrAdminRemovedByNonOwner}},
		{"the owner removing an admin and its entry is allowed", roleScenario{
			committer: "owner", before: roleBaseline, after: leavesWith(roleLeaf{1, ""}), removed: []uint32{1},
			rolesBefore: roleBaselineRoles, rolesAfter: rolesWith(map[string]*mls.Role{"admin": nil})}, nil},
		{"an admin removing its own second device is allowed (ruling 2)", roleScenario{
			committer: "admin", before: leavesWith(roleLeaf{5, "admin"}), after: roleBaseline, removed: []uint32{5},
			rolesBefore: roleBaselineRoles}, nil},
		{"R6c a commit that removes its own committer is not a shape mls produces, and is refused here too", roleScenario{
			committer: "member", before: roleBaseline, after: leavesWith(roleLeaf{2, ""}), removed: []uint32{2},
			rolesBefore: roleBaselineRoles}, []error{ErrCommitIdentityChanged}},

		// ── R5: ownership ────────────────────────────────────────────────────────────────
		{"R5 a member naming itself owner is refused", roleScenario{
			committer: "member", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"member": rolePtr(mls.RoleOwner), "owner": rolePtr(mls.RoleAdmin)})},
			[]error{ErrCommitOwnerTransfer}},
		{"R5 an admin naming itself owner is refused", roleScenario{
			committer: "admin", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"admin": rolePtr(mls.RoleOwner), "owner": rolePtr(mls.RoleAdmin)})},
			[]error{ErrCommitOwnerTransfer}},
		{"R5 the owner handing over and staying a member is refused (ruling 4)", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"admin": rolePtr(mls.RoleOwner), "owner": nil})},
			[]error{ErrCommitOwnerTransfer}},
		{"the owner handing over to the admin and becoming an admin is allowed", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"admin": rolePtr(mls.RoleOwner), "owner": rolePtr(mls.RoleAdmin)})}, nil},
		{"the owner handing over to a member and becoming an admin is allowed", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"member": rolePtr(mls.RoleOwner), "owner": rolePtr(mls.RoleAdmin)})}, nil},
		{"R5 the owner adding a stranger and crowning it in one commit is refused (ruling 10)", roleScenario{
			committer: "owner", before: roleBaseline, after: leavesWith(roleLeaf{5, "stranger"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles,
			rolesAfter:  rolesWith(map[string]*mls.Role{"stranger": rolePtr(mls.RoleOwner), "owner": rolePtr(mls.RoleAdmin)})},
			[]error{ErrCommitOwnerTransfer}},
		{"the owner crowning a member it added in an earlier commit is allowed (ruling 10's other arm)", roleScenario{
			committer: "owner", before: leavesWith(roleLeaf{5, "stranger"}), after: leavesWith(roleLeaf{5, "stranger"}),
			rolesBefore: roleBaselineRoles,
			rolesAfter:  rolesWith(map[string]*mls.Role{"stranger": rolePtr(mls.RoleOwner), "owner": rolePtr(mls.RoleAdmin)})}, nil},
		{"the owner handing over and dropping its other device in one commit is allowed (ruling 4)", roleScenario{
			committer: "owner", before: leavesWith(roleLeaf{5, "owner"}), after: roleBaseline, removed: []uint32{5},
			rolesBefore: roleBaselineRoles,
			rolesAfter:  rolesWith(map[string]*mls.Role{"admin": rolePtr(mls.RoleOwner), "owner": rolePtr(mls.RoleAdmin)})}, nil},
		{"the new owner removing the ex-owner after the transfer is allowed", roleScenario{
			committer: "admin", before: roleBaseline, after: leavesWith(roleLeaf{0, ""}), removed: []uint32{0},
			rolesBefore: rolesWith(map[string]*mls.Role{"admin": rolePtr(mls.RoleOwner), "owner": rolePtr(mls.RoleAdmin)}),
			rolesAfter:  rolesWith(map[string]*mls.Role{"admin": rolePtr(mls.RoleOwner), "owner": nil})}, nil},
		{"R3 the owner's leave committed by an admin before any transfer is refused", roleScenario{
			committer: "admin", before: roleBaseline, after: leavesWith(roleLeaf{0, ""}), removed: []uint32{0},
			rolesBefore: roleBaselineRoles, rolesAfter: rolesWith(map[string]*mls.Role{"admin": rolePtr(mls.RoleOwner), "owner": nil})},
			[]error{mls.ErrAdminRemovedByNonOwner}},
		{"R0a a policy naming no owner is refused with mls's reason", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			policyAfterBody: noOwnerBody}, []error{ErrCommitPolicyInvalid, mls.ErrNoOwner}},

		// ── R4: role and policy-body changes ─────────────────────────────────────────────
		{"R4 an admin promoting a member to admin is refused", roleScenario{
			committer: "admin", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"member": rolePtr(mls.RoleAdmin)})},
			[]error{ErrCommitRoleChangeByNonOwner}},
		{"R4 an admin demoting an admin to member is refused", roleScenario{
			committer: "admin", before: leavesWith(roleLeaf{5, "admin-2"}), after: leavesWith(roleLeaf{5, "admin-2"}),
			rolesBefore: rolesWith(map[string]*mls.Role{"admin-2": rolePtr(mls.RoleAdmin)}),
			rolesAfter:  rolesWith(map[string]*mls.Role{"admin-2": nil})},
			[]error{ErrCommitRoleChangeByNonOwner}},
		{"R4 an admin demoting itself is refused", roleScenario{
			committer: "admin", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"admin": nil})},
			[]error{ErrCommitRoleChangeByNonOwner}},
		{"the owner promoting a member to admin is allowed", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"member": rolePtr(mls.RoleAdmin)})}, nil},
		{"the owner seating a new identity as admin in the add is allowed", roleScenario{
			committer: "owner", before: roleBaseline, after: leavesWith(roleLeaf{5, "stranger"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles, rolesAfter: rolesWith(map[string]*mls.Role{"stranger": rolePtr(mls.RoleAdmin)})}, nil},
		{"R4 an admin seating a new identity as admin in the add is refused", roleScenario{
			committer: "admin", before: roleBaseline, after: leavesWith(roleLeaf{5, "stranger"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles, rolesAfter: rolesWith(map[string]*mls.Role{"stranger": rolePtr(mls.RoleAdmin)})},
			[]error{ErrCommitRoleChangeByNonOwner}},
		{"R4 a member making the observer a member is refused", roleScenario{
			committer: "member", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"observer": nil})},
			[]error{ErrCommitPolicyChangeByNonAdmin}},
		{"R4 an observer making itself a member is refused", roleScenario{
			committer: "observer", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"observer": nil})},
			[]error{ErrCommitPolicyChangeByNonAdmin}},
		{"an admin making the observer a member is allowed", roleScenario{
			committer: "admin", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"observer": nil})}, nil},
		{"an admin making a member an observer is allowed", roleScenario{
			committer: "admin", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"member": rolePtr(mls.RoleObserver)})}, nil},
		{"R4 a member changing retention is refused", roleScenario{
			committer: "member", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			retentionAfter: longer}, []error{ErrCommitPolicyChangeByNonAdmin}},
		{"an admin changing retention is allowed", roleScenario{
			committer: "admin", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			retentionAfter: longer}, nil},
		{"R4 a member changing the disappearing buckets is refused", roleScenario{
			committer: "member", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			bucketsBefore: []uint8{1, 2}, bucketsAfter: []uint8{1, 2, 3}}, []error{ErrCommitPolicyChangeByNonAdmin}},
		{"R4 an observer changing the disappearing buckets is refused", roleScenario{
			committer: "observer", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			bucketsBefore: []uint8{1, 2}, bucketsAfter: []uint8{1}}, []error{ErrCommitPolicyChangeByNonAdmin}},
		{"an admin changing the disappearing buckets is allowed", roleScenario{
			committer: "admin", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			bucketsBefore: []uint8{1, 2}, bucketsAfter: []uint8{1, 2, 3}}, nil},
		{"the owner changing the disappearing buckets is allowed", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			bucketsBefore: []uint8{1, 2}, bucketsAfter: []uint8{3}}, nil},
		{"R4 a member changing the server id is refused", roleScenario{
			committer: "member", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			serverIdBefore: []byte("server-a"), serverIdAfter: []byte("server-b")}, []error{ErrCommitServerIdChangeByNonOwner}},
		{"R4 an admin changing the server id is refused: a migration is the owner's", roleScenario{
			committer: "admin", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			serverIdBefore: []byte("server-a"), serverIdAfter: []byte("server-b")}, []error{ErrCommitServerIdChangeByNonOwner}},
		{"the owner changing the server id is allowed", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			serverIdBefore: []byte("server-a"), serverIdAfter: []byte("server-b")}, nil},
		{"an admin's retention change that keeps the server id is allowed with a server id set", roleScenario{
			committer: "admin", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			serverIdBefore: []byte("server-a"), retentionAfter: longer}, nil},

		// ── R7: a member's or an observer's commit is its own device leaves, or nothing, and no more ──
		{"R7 a member's path-only commit that also rewrites the policy without changing a role is refused", roleScenario{
			committer: "member", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"member-2": rolePtr(mls.RoleMember)})},
			[]error{ErrCommitBeyondOwnDevices}},
		{"R7 an observer's path-only commit that also rewrites the policy without changing a role is refused", roleScenario{
			committer: "observer", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"member-2": rolePtr(mls.RoleMember)})},
			[]error{ErrCommitBeyondOwnDevices}},
		{"R7 a member committing another member's update by reference is refused", roleScenario{
			committer: "member", before: roleBaseline, after: roleBaseline, updated: []uint32{4},
			rolesBefore: roleBaselineRoles}, []error{ErrCommitBeyondOwnDevices}},
		{"R7 an observer committing the owner's update by reference is refused", roleScenario{
			committer: "observer", before: roleBaseline, after: roleBaseline, updated: []uint32{0},
			rolesBefore: roleBaselineRoles}, []error{ErrCommitBeyondOwnDevices}},
		{"R7 a member's own device add that also carries another member's update by reference is refused", roleScenario{
			committer: "member", before: roleBaseline, after: leavesWith(roleLeaf{5, "member"}), added: []uint32{5}, updated: []uint32{4},
			rolesBefore: roleBaselineRoles}, []error{ErrCommitBeyondOwnDevices}},
		{"R7 a member's own device add that also rewrites the policy without changing a role is refused", roleScenario{
			committer: "member", before: roleBaseline, after: leavesWith(roleLeaf{5, "member"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles, rolesAfter: rolesWith(map[string]*mls.Role{"member-2": rolePtr(mls.RoleMember)})},
			[]error{ErrCommitBeyondOwnDevices}},
		{"the owner rewriting the policy without changing a role is allowed", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			rolesAfter: rolesWith(map[string]*mls.Role{"member-2": rolePtr(mls.RoleMember)})}, nil},

		// ── a role name this profile does not define ─────────────────────────────────────
		{"a committer role name outside the four is refused, not defaulted", roleScenario{
			committer: "owner", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles,
			committerRole: "king"}, []error{ErrCommitRoleUnknown}},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			err := authorizeCommit(row.scenario.authorization(t))
			if len(row.want) == 0 {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("allowed, want a refusal wrapping %v", row.want)
			}
			for _, want := range row.want {
				if !errors.Is(err, want) {
					t.Errorf("refused with %v, which does not wrap %v", err, want)
				}
			}
		})
	}
}

// AN UNNAMED MEMBER IS A MEMBER, ruling 8, read where the rules read it: the derived role name.
func TestAnUnnamedIdentityReadsAsMemberOnBothSidesOfACommit(t *testing.T) {
	decision := roleScenario{committer: "member", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles}.authorization(t)
	if decision.CommitterRole != mls.RoleMember.String() {
		t.Errorf("the unnamed committer's role is %q, want %q", decision.CommitterRole, mls.RoleMember.String())
	}
	for _, member := range append(decision.Members, decision.MembersAfter...) {
		if string(member.IdentityPub) == string(roleIdentity("member-2")) && member.Role != mls.RoleMember.String() {
			t.Errorf("the unnamed member-2 reads as %q", member.Role)
		}
	}
	if roleNameIn(nil, roleIdentity("anyone")) != mls.RoleMember.String() {
		t.Error("a nil policy reads an identity as something other than member")
	}
}

// R7 JUDGES THE ADDS AND REMOVES ON ITS OWN TERMS, called directly: through [authorizeCommit] a
// member's Add of a stranger or Remove of another member is R1's or R2's refusal before R7 is
// reached, so the two loops in ruleOwnDevicesOnly are a statement of what the rule allows that
// no table row above can turn red. This holds them, so that "its own device leaves" is decided by
// the rule that says it and not by the order the rules happen to run in.
func TestR7JudgesTheAddsAndRemovesOnItsOwnTerms(t *testing.T) {
	rows := []struct {
		name     string
		scenario roleScenario
		refused  bool
	}{
		{"a member's add of a stranger", roleScenario{
			committer: "member", before: roleBaseline, after: leavesWith(roleLeaf{5, "stranger"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles}, true},
		{"a member's add of a leaf claiming the admin", roleScenario{
			committer: "member", before: roleBaseline, after: leavesWith(roleLeaf{5, "admin"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles}, true},
		{"a member's remove of another member", roleScenario{
			committer: "member", before: roleBaseline, after: leavesWith(roleLeaf{4, ""}), removed: []uint32{4},
			rolesBefore: roleBaselineRoles}, true},
		{"an observer's remove of the owner", roleScenario{
			committer: "observer", before: roleBaseline, after: leavesWith(roleLeaf{0, ""}), removed: []uint32{0},
			rolesBefore: roleBaselineRoles}, true},
		{"a member's add of its own device", roleScenario{
			committer: "member", before: roleBaseline, after: leavesWith(roleLeaf{5, "member"}), added: []uint32{5},
			rolesBefore: roleBaselineRoles}, false},
		{"a member's remove of its own device", roleScenario{
			committer: "member", before: leavesWith(roleLeaf{5, "member"}), after: roleBaseline, removed: []uint32{5},
			rolesBefore: roleBaselineRoles}, false},
		{"an observer's add of one device and remove of another, in one commit", roleScenario{
			committer: "observer", before: leavesWith(roleLeaf{5, "observer"}), after: leavesWith(roleLeaf{5, ""}, roleLeaf{6, "observer"}),
			removed: []uint32{5}, added: []uint32{6}, rolesBefore: roleBaselineRoles}, false},
		{"a member's path-only commit (ruling 12)", roleScenario{
			committer: "member", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles}, false},
		{"an observer's path-only commit (ruling 12)", roleScenario{
			committer: "observer", before: roleBaseline, after: roleBaseline, rolesBefore: roleBaselineRoles}, false},
		{"an observer's path-only commit carrying a foreign update by reference", roleScenario{
			committer: "observer", before: roleBaseline, after: roleBaseline, updated: []uint32{2}, rolesBefore: roleBaselineRoles}, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			decision := row.scenario.authorization(t)
			committer, err := roleNamed(decision.CommitterRole)
			if err != nil {
				t.Fatal(err)
			}
			err = ruleOwnDevicesOnly(decision, committer, leavesOf(decision.Members), leavesOf(decision.MembersAfter))
			if row.refused && !errors.Is(err, ErrCommitBeyondOwnDevices) {
				t.Fatalf("R7 alone answered %v, want ErrCommitBeyondOwnDevices", err)
			}
			if !row.refused && err != nil {
				t.Fatalf("R7 alone refused a member's own device change: %v", err)
			}
		})
	}
}

// THE CAPS, ruling 7 and MASTER §11: over the post-commit tree.
func TestTheCapsAreJudgedOverThePostCommitTree(t *testing.T) {
	leavesOfIdentities := func(identities int, perIdentity int) []roleLeaf {
		out := []roleLeaf{{0, "owner"}}
		leaf := uint32(1)
		for i := 0; i < identities; i += 1 {
			for d := 0; d < perIdentity; d += 1 {
				out = append(out, roleLeaf{leaf, fmt.Sprintf("identity-%d", i)})
				leaf += 1
			}
		}
		return out
	}
	addedFrom := func(before []roleLeaf, after []roleLeaf) []uint32 {
		out := []uint32{}
		for _, one := range after[len(before):] {
			out = append(out, one.leaf)
		}
		return out
	}
	// the owner adds NEW identities (R1) and an identity adds its OWN leaves (R6a): the per
	// identity cap can only be reached by that identity's own commit
	rows := []struct {
		name      string
		committer string
		before    []roleLeaf
		after     []roleLeaf
		want      error
	}{
		{"500 identities is allowed", "owner", leavesOfIdentities(498, 1), leavesOfIdentities(499, 1), nil},
		{"501 identities is refused", "owner", leavesOfIdentities(499, 1), leavesOfIdentities(500, 1), mls.ErrGroupSizeExceeded},
		{"1000 leaves is allowed", "owner", leavesOfIdentities(99, 10), leavesOfIdentities(100, 10)[:1000], nil},
		{"1001 leaves is refused", "owner", leavesOfIdentities(99, 10), leavesOfIdentities(100, 10), mls.ErrGroupSizeExceeded},
		{"10 leaves for one identity is allowed", "identity-0", leavesOfIdentities(1, 9), leavesOfIdentities(1, 10), nil},
		{"11 leaves for one identity is refused", "identity-0", leavesOfIdentities(1, 10), leavesOfIdentities(1, 11), mls.ErrDeviceLimitExceeded},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			decision := roleScenario{
				committer: row.committer, before: row.before, after: row.after,
				added:       addedFrom(row.before, row.after),
				rolesBefore: map[string]mls.Role{"owner": mls.RoleOwner},
			}.authorization(t)
			err := authorizeCommit(decision)
			if row.want == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, row.want) {
				t.Fatalf("answered %v, want %v", err, row.want)
			}
		})
	}
}
