// THE REMOVAL RULE IS HELD BY A DRIVEN TABLE OF INPUT SHAPES, AND NOT BY A READING OF THE SOURCE.
//
// ── WHY THIS FILE EXISTS, WHICH IS LEDGER RULING 46 ──────────────────────────────────────────
//
// Six consecutive rounds built or repaired a static gate over [refuseRemovalOnHeldSecret] and
// [Group.refuseUnrotatedRemovalLocked], and every one of them was defeated ONE LEVEL OF
// INDIRECTION FURTHER OUT. The VALUE was named (`held` against `candidate.secret`), then the CALL
// was named and its result was not, then the RESULT was named and the path to it was not, then the
// POSITION was named and the binding was not, then the BINDING was deleted and the held test's
// ARGUMENT took the narrowing, and the caller's list after that. Each round widened what the gate
// READS; each round's defeat was a new spelling outside that reading.
//
// Enforcement is a semantic property. A gate that must prove *this refusal actually fires for
// every input it should* is trying to decide a runtime question out of syntax, and every reading
// it adds is one more surface to route around. What the gate was standing in for is A SET OF
// INPUTS THAT MUST BE REFUSED. So this file drives them.
//
// ── WHAT A ROW IS ────────────────────────────────────────────────────────────────────────────
//
// One input shape, built by the production sealers ([Group.stageEpochRotationLocked],
// [Group.sealEpochWrapLocked], [messagegroup.GroupSession.SealRecord]) and put through the
// production receive path -- [Group.openPageLocked] driving [Group.ingestWrapLocked] and
// [Group.ingestCommitLocked], which is what [Group.Receive] does below the fetch -- and asserted
// to be REFUSED or to be FOLLOWED. The honest rows are not a separate suite: they are rows of this
// table, in this function, because a rule that refuses everything removes the feature rather than
// the member and a table with no followed row cannot tell the two apart.
//
// THE TWO DOORS ARE BOTH DRIVEN AND THEY ARE MARKED AS TWO, because they are reached by different
// records and take the decision at different points:
//
//   - `noDigest` is [Group.refuseUnrotatedRemovalLocked], the kind 0x0001 class: a removal on a
//     commit carrying no epoch digest at all. It is decided BEFORE ApplyCommit, so the receiver's
//     MLS handle does not move, and every row of that door asserts exactly that. An adversary
//     found this door had no gate on its own predicate at any point in the six rounds.
//   - `digest` is [Group.resolvePqSecretLocked]'s one guarded exit: everything that carries a
//     digest, judged against the value that digest NAMES. It runs after ApplyCommit -- judging a
//     candidate needs mls_secret[n+1] -- so the handle HAS moved and every row of that door
//     asserts that too, which is the residual stated rather than glossed.
//
// ── WHAT THE RULE DELIVERS, AND THE THREE SHAPES IT DOES NOT: LEDGER RULINGS 42-45 ───────────
//
// A reader meeting the rule here meets its limits here. This table does not measure a group
// property and the code cannot deliver one. What every refusing row below measures is
//
//	THIS RECEIVER DOES NOT FOLLOW A REMOVAL ONTO A SECRET THIS RECEIVER HAS HELD
//
// and the three shapes outside it are:
//
//  1. A LATE JOINER'S HISTORY IS STRICTLY SMALLER, so it refuses less, and its false negative is a
//     theorem rather than a bug. [Device.Join] files exactly one row. THIS IS NO LONGER PROSE: the
//     row `digest/one-leaf/earlier-epoch/late-joiner` drives it. One page, two receivers -- the
//     founder REFUSES it and the member admitted at epoch 3 FOLLOWS it -- with the control firing
//     for its own reason, because the late joiner does recognise its OWN row.
//  2. IF EVERY SURVIVOR JOINED AFTER EPOCH k AND THE COMMITTER REUSES pq_secret[k], NOBODY
//     REFUSES. Not "the check is weaker": there is no refuser left, because the committer never
//     runs the receive path against its own commit and every other survivor is a late joiner by
//     (1). The group-level effect is a PARTITION BY JOIN EPOCH, which ledger item 242 has already
//     priced as *a hostile committer can HALT a group; it cannot TAKE it*. The row in (1) is the
//     two-receiver half of exactly this; the all-late-joiner world is its limit and no row here
//     drives it, which is said rather than implied.
//  3. AGAINST A HOSTILE ADMIN OR OWNER COMMITTER THIS DELIVERS NOTHING, STRUCTURALLY. Only an
//     ADMIN or the OWNER may commit a removal at all (MASTER section 11), and to build the epoch
//     digest that party must hold read_key[n+1] and write_key[n+1] -- therefore storage_root[n+1]
//     -- inside its own process. It can hand over the ROOT rather than the secret, and no
//     receiver-side check on the VALUE can constrain it. No row here is a defence against it and
//     none could be: the row `digest/one-leaf/committed-by-a-non-owner-admin` drives an ADMIN
//     committer to show the rule is not keyed to WHO commits, which is a different statement.
//
// The full argument, with what holds each clause, is at [refuseRemovalOnHeldSecret].
//
// ── WHAT THIS TABLE CANNOT HOLD, PRINTED RATHER THAN DESCRIBED ───────────────────────────────
//
// A driven table is a finite set of inputs, and the axis every one of the six rounds was bypassed
// on is ARITY -- how many leaves the commit removes. The arity axis is LOGGED per door before
// anything is asserted, as the INTERVAL it drives and as the fact that the interval has a TOP, so
// what is outside it is visible in a line rather than inferred from a count: a bypass keyed on
// `5 <= len(removedLeaves)` is not driven here and would pass.
//
// IT IS AN INTERVAL AND NOT A SET, AND THAT IS THE 2026-09-24 (SEVENTH PASS) CORRECTION. The line
// used to print `{0, 1, 2, 3}`, which reads as a choice of points and leaves the reader to work
// out that the edge is what matters. For any top k this table drives, the narrowing outside it is
// `< k+1` and there is always one, so no number of rows closes the axis -- what a row buys is
// moving the edge, and what the line has to say is where the edge IS. The rows at four leaves
// bought exactly one thing and it was worth buying: the sixth pass's own surviving mutant,
// `len(removedLeaves) < 4`, sat at arity four.
//
// ── ARITY IS NOT THE ONLY AXIS, WHICH IS THE 2026-09-24 (EIGHTH PASS) CORRECTION ─────────────
//
// The sentence that stood here said the residual neither instrument covers is *a narrowing written
// into some OTHER statement of the exit, ABOVE this interval*, and "above this interval" made a
// claim about ONE axis at a door that does not decide on one. EACH DOOR DECIDES ON MORE THAN ONE
// INPUT, and until this pass only arity was driven as a bounded interval at either.
//
//   - THE RESOLUTION'S EXIT DECIDES ON THREE INPUTS: `len(removedLeaves)`, the held answer --
//     `alreadyHeld`, which carries `heldAt` beside it -- and WHICH ARM called it. MEASURED, by a
//     `println` planted above the guard and run over this table: every refusing row sat at `heldAt`
//     in {1, 2}. So `alreadyHeld = alreadyHeld && heldAt < 3`, written between the binding and the
//     guard at pqepoch.go sha256 1608f28c11c9, passed this table, passed BOTH predicate readings
//     and passed all 171 cases in this package, with the removal rule gone for every receiver whose
//     history of the replayed value starts at epoch 3 or later. That is an ordinary shape and not a
//     contrived one: a committer replaying pq_secret[k] for k >= 3 in a group that has rotated a
//     few times. The control fires for its own reason -- the same statement at `heldAt < 2` (sha256
//     2d0e19f01e8d) turns this table RED at `digest/one-leaf/committed-by-a-non-owner-admin`, the
//     only refusing row whose refusal named epoch 2.
//   - THE PRE-APPLY DOOR DECIDES ON `len(removedLeaves)`, `digest != nil` AND `self.epoch`.
//     MEASURED the same way: every digest-less call of that door arrived at `self.epoch == 1`, so
//     `1 < self.epoch -> return nil`, in any spelling, passed this table at every arity it drives.
//
// Both axes are DRIVEN and PRINTED now, each with its own three controls, beside the arity interval
// and in the same line, and each DECLARED number is held against the one production itself reports.
// What is still outside every instrument is stated on the axis it is outside of: `heldAt < 4` at
// the resolution and `2 < self.epoch` at the door are each one step above a printed top, exactly as
// `len(removedLeaves) < 5` is, and a row is what moves each edge.
//
// That is the honest residual of this instrument and it is why the predicate readings in
// pqdarkgate_test.go are kept -- for the class no table can cover (an arm that no row drives
// because the arm does not exist yet) and for a narrowing written into either door's own CONDITION,
// in any conditional shape, above these intervals. What NEITHER covers: a narrowing written into
// some OTHER statement of the resolution's exit, above the intervals this table prints -- on any of
// its three inputs, and not on arity alone.
package urmessage

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/mls"
)

// ── the world's two missing doors ────────────────────────────────────────────────────────────

// admit adds ONE member in its own honest rotation and gives it the single pq_secret row
// [Device.Join] files, so a world can hold a member whose history is strictly smaller than the
// founder's. Before it existed, ledger ruling 43's first residual was measured by a probe and by
// nothing in this package, because [newRotWorld] admits every member in the founding commit.
//
// THE ROTATION IS HONEST AND IS DELIVERED TO `receivers`, so the members named there stand where
// the new one does and the case that follows is about the removal and not about a fork.
func (self *rotWorld) admit(committer *rotMember, name string, receivers ...*rotMember) (*rotMember, *rotation) {
	self.t.Helper()
	dev := self.device(name)
	keyPackage, err := dev.engine.NewKeyPackage()
	if err != nil {
		self.t.Fatalf("%s's key package: %v", name, err)
	}
	var welcome, ratchetTree []byte
	published := self.rotate(committer, func() ([]byte, []byte, []byte, error) {
		commit, admission, tree, err := committer.handle.CommitAdd([][]byte{keyPackage})
		welcome, ratchetTree = admission, tree
		return commit, admission, tree, err
	})
	for _, receiver := range receivers {
		if err := self.deliver(receiver, published.page()...); err != nil {
			self.t.Fatalf("%s's walk over the rotation that admits %s: %v", receiver.name, name, err)
		}
	}
	handle, err := dev.engine.JoinFromWelcome(welcome, ratchetTree)
	if err != nil {
		self.t.Fatalf("%s's JoinFromWelcome: %v", name, err)
	}
	self.t.Cleanup(func() { handle.Close() })
	return self.enrollAt(name, dev, handle, published.pqSecret), published
}

// promote makes one member an ADMIN through a real CommitPolicy, published as an honest rotation
// and delivered to `receivers`. It is what lets a row vary the COMMITTER, which every removal case
// written before this table left as a silent premise: all of them are committed by the founder.
func (self *rotWorld) promote(owner *rotMember, member *rotMember, receivers ...*rotMember) *rotation {
	self.t.Helper()
	extensions, err := owner.group.contextExtensionsLocked()
	if err != nil {
		self.t.Fatalf("%s's group context: %v", owner.name, err)
	}
	policy, err := mls.GroupPolicyOf(mlsExtensionsOf(extensions))
	if err != nil {
		self.t.Fatalf("%s's policy: %v", owner.name, err)
	}
	policy.SetRole(member.dev.identityPub, mls.RoleAdmin)
	if err := policy.Canonicalize(); err != nil {
		self.t.Fatalf("canonicalizing the policy: %v", err)
	}
	encoded, err := policy.Encode()
	if err != nil {
		self.t.Fatalf("encoding the policy: %v", err)
	}
	published := self.rotate(owner, func() ([]byte, []byte, []byte, error) {
		return owner.handle.CommitPolicy(encoded.ExtensionData)
	})
	for _, receiver := range receivers {
		if err := self.deliver(receiver, published.page()...); err != nil {
			self.t.Fatalf("%s's walk over the promotion of %s: %v", receiver.name, member.name, err)
		}
	}
	return published
}

// bundleAddAndRemove stages one Add and one Remove as BY-REFERENCE proposals and hands back the
// commit arm that folds both in, plus the key package the add carries.
//
// IT HAS TO BE BY REFERENCE AND THAT IS A PROPERTY OF THE SEAM, not a preference. The seam's
// by-value arms are one kind each -- [messagegroup.GroupHandle.CommitAdd] and CommitRemove build a
// commit carrying one kind of proposal and nothing by reference -- so a commit that both adds and
// removes cannot be built through either. `Commit(nil)` folds in every proposal the committer has
// cached, which is the one arm that can carry both, and the cost is that every receiver must have
// been handed both proposals first: a reference resolves against the RECEIVER's own cache, and a
// member that never saw the proposal answers `proposal reference is not cached for this epoch`.
// The proposals are handed over through the handle rather than through the record layer because
// this package's page walk carries commits and wraps and no proposal record exists yet; what is
// under test here is the commit, and the caching is MLS plumbing beneath it.
func (self *rotWorld) bundleAddAndRemove(committer *rotMember, removing uint32, joining string,
	receivers ...*rotMember) (func() ([]byte, []byte, []byte, error), *crossProcessDevice) {

	self.t.Helper()
	dev := self.device(joining)
	keyPackage, err := dev.engine.NewKeyPackage()
	if err != nil {
		self.t.Fatalf("%s's key package: %v", joining, err)
	}
	add, err := committer.handle.ProposeAdd(keyPackage)
	if err != nil {
		self.t.Fatalf("%s's ProposeAdd: %v", committer.name, err)
	}
	remove, err := committer.handle.ProposeRemove(removing)
	if err != nil {
		self.t.Fatalf("%s's ProposeRemove(%d): %v", committer.name, removing, err)
	}
	for _, receiver := range receivers {
		for at, proposal := range [][]byte{add, remove} {
			if _, err := receiver.handle.Process(proposal); err != nil {
				self.t.Fatalf("%s caching proposal %d of the bundle: %v. A receiver that has not "+
					"cached both refuses the commit for the cache's reason and this row would be "+
					"measuring that instead", receiver.name, at, err)
			}
		}
	}
	return func() ([]byte, []byte, []byte, error) { return committer.handle.Commit(nil) }, dev
}

// holdsIdentity reports whether a handle's CURRENT tree holds a leaf under `identity`. It is the
// control on a bundled commit: a commit that quietly dropped one of its two proposals would
// otherwise make that row a slow copy of the plain removal.
func (self *rotWorld) holdsIdentity(member *rotMember, identity []byte) bool {
	self.t.Helper()
	for at := 0; at < member.handle.MemberCount(); at += 1 {
		_, leafIdentity, _, err := member.handle.MemberAt(at)
		if err != nil {
			self.t.Fatalf("%s's member %d: %v", member.name, at, err)
		}
		if bytes.Equal(leafIdentity, identity) {
			return true
		}
	}
	return false
}

// reproduces is the counterfactual every refusing row is about, asked of the COMMITTER's own
// exporter at the epoch it has just opened: does `retained` -- the value the removed member keeps
// by construction -- mix to the same storage_root as the value the epoch actually runs on.
func (self *rotWorld) reproduces(committer *rotMember, retained []byte, opensOn []byte) bool {
	self.t.Helper()
	granted, err := committer.handle.Export(storageExporterLabel, nil, storageExporterBytes)
	if err != nil {
		self.t.Fatalf("%s's exporter at epoch %d: %v", committer.name, committer.handle.Epoch(), err)
	}
	return bytes.Equal(messagegroup.StorageRoot(granted, retained),
		messagegroup.StorageRoot(granted, opensOn))
}

// ── the table ────────────────────────────────────────────────────────────────────────────────

// removalDoor names which of the two detectors a row is aimed at. It decides one assertion and
// nothing else: whether the receiver's MLS handle may have moved by the time the refusal is taken.
type removalDoor string

const (
	// noDigestDoor is [Group.refuseUnrotatedRemovalLocked], pre-apply, the kind 0x0001 class.
	noDigestDoor removalDoor = "no digest (pre-apply)"
	// digestDoor is [Group.resolvePqSecretLocked]'s one guarded exit, after ApplyCommit.
	digestDoor removalDoor = "digest (the resolution)"
)

// removalPage is one built input shape, everything the shared assertions need to judge it, and the
// row's OWN controls as two closures. The controls are on the page rather than on the row because
// they are written by the builder and read what the builder measured.
type removalPage struct {
	world     *rotWorld
	committer *rotMember
	receiver  *rotMember
	page      []*sealed
	opens     uint64
	// removes is the leaf list the commit carries. The refusal PRINTS it, and asserting that the
	// printed list is this list whole is what holds every narrowing between connect's
	// `processed.Commit.RemovedLeaves()` and the guard -- `decision.RemovedLeaves[:1]` at either
	// call site, a rewrite of `decision` between the two doors, a `removedLeaves = removedLeaves
	// [:1]` inside the resolution -- without a gate reading any of those spellings.
	removes []uint32
	// opensOn is the pq_secret the epoch this commit opens actually runs on.
	opensOn []byte
	// retained is the value a removed member keeps by construction, or nil when the row removes
	// nobody. Non-nil turns on the counterfactual: it must reproduce the committer's root on a
	// refusing row and must NOT on a followed one, so a fixture that quietly rotated (or quietly
	// did not) is caught before the disposition is read.
	retained []byte
	// also runs before the page is delivered; then runs after the shared assertions.
	also func(t *testing.T)
	then func(t *testing.T)
}

type removalInput struct {
	name  string
	door  removalDoor
	shape string
	// arity is len(removedLeaves), the axis every one of the six bypassed gates was defeated on.
	// It is DECLARED here because the interval is printed before any row is built, and it is HELD
	// against the built page in [removalPage.drive]: a row declaring an arity its builder does not
	// produce would make the printed interval false in exactly the direction it exists to be true
	// in.
	arity int
	// heldAt is the epoch [refuseRemovalOnHeldSecret] must NAME for this row, and 0 when this row
	// takes no such refusal -- every followed row, and every row of the pre-apply door, whose
	// refusal cannot name one because that door never asks. It is the resolution's SECOND input,
	// printed as its own interval beside the arity one, and it is held against the number
	// production wrote into its own sentence rather than against a re-derivation here.
	heldAt uint64
	// doorEpoch is `self.epoch` when [Group.refuseUnrotatedRemovalLocked] runs, which is the
	// receiver's epoch before this row's page is delivered. It is that door's THIRD input and it
	// was driven at exactly one point until this pass. It is 0 on a row of the other door.
	doorEpoch uint64
	followed  bool
	build     func(t *testing.T) *removalPage
}

// EVERY REMOVAL SHAPE THIS PACKAGE CAN PUT ON THE WIRE, DRIVEN THROUGH THE PRODUCTION RECEIVE PATH
// AND ASSERTED TO BE REFUSED OR FOLLOWED.
//
// The header of this file says why the instrument is a table and what it cannot hold. Each row is
// built by the production sealers and delivered by [rotWorld.deliver]; the assertions below are
// shared, so a row is a SHAPE and not a copy of the assertions with one thing changed.
func TestEveryRemovalShapeThisPackageCanPutOnTheWireIsRefusedOrFollowedByTheProductionReceivePath(t *testing.T) {
	rows := []removalInput{
		// ── the digest door: one to FOUR leaves on the secret the victims hold ──────────────
		{name: "digest/one-leaf/current-epoch", door: digestDoor, arity: 1, heldAt: 1, followed: false,
			shape: "a removal with a complete openable fan-out carrying the secret the group is " +
				"standing on, under a digest that names it",
			build: buildHeldFanOut(1, unrotatedFanOut{})},
		{name: "digest/two-leaves/current-epoch", door: digestDoor, arity: 2, heldAt: 1, followed: false,
			shape: "the same, removing two leaves: one ADMIN ejecting two devices at once",
			build: buildHeldFanOut(2, unrotatedFanOut{})},
		{name: "digest/three-leaves/current-epoch", door: digestDoor, arity: 3, heldAt: 1, followed: false,
			shape: "the same, removing THREE leaves. This is the axis three separate mutants " +
				"survived on: `len(removedLeaves) < 3` in the guard's binding left every other " +
				"case in this package green",
			build: buildHeldFanOut(3, unrotatedFanOut{})},
		{name: "digest/three-leaves/fresh-wraps-held-digest", door: digestDoor, arity: 3, heldAt: 1, followed: false,
			shape: "three leaves, wraps carrying FRESH octets and a digest naming the held value: " +
				"the compatibility arm reached at arity three, which no candidate-reading check " +
				"could ever have refused",
			build: buildFreshFanOutHeldDigest(3)},
		{name: "digest/four-leaves/current-epoch", door: digestDoor, arity: 4, heldAt: 1, followed: false,
			shape: "the same, removing FOUR leaves. THIS IS THE ARITY THE SIXTH PASS LEFT " +
				"OUTSIDE THE TABLE, and it is here because the static reading that was supposed " +
				"to cover the rest of the axis did not: while the guard was two nested " +
				"conditionals that reading took the OUTER one, and `alreadyHeld && " +
				"len(removedLeaves) < 4` written into the INNER one passed the reading, this " +
				"table and this package (pqepoch.go sha256 553bd9fffa2c). A narrowing a driven " +
				"case can catch belongs in the table as a row",
			build: buildHeldFanOut(4, unrotatedFanOut{})},

		// ── the digest door: what must be FOLLOWED, in the same test ────────────────────────
		{name: "digest/one-leaf/honest-rotated", door: digestDoor, arity: 1, followed: true,
			shape: "THE CONTROL. An honest rotated removal of one leaf. Without it the rule is " +
				"satisfied by a build that refuses every removal, which removes the feature " +
				"rather than the member",
			build: buildHonestRotation(1)},
		{name: "digest/three-leaves/honest-rotated", door: digestDoor, arity: 3, followed: true,
			shape: "THE CONTROL AT THE NEW ARITY. An honest rotated removal of three leaves, so " +
				"the three-leaf rows above are not refusals of arity itself",
			build: buildHonestRotation(3)},
		{name: "digest/four-leaves/honest-rotated", door: digestDoor, arity: 4, followed: true,
			shape: "THE CONTROL AT THE TOP OF THE INTERVAL. An honest rotated removal of four " +
				"leaves, so the four-leaf row above is not a refusal of arity itself -- every " +
				"arity this table drives carries both dispositions or the refusing row at it " +
				"proves nothing",
			build: buildHonestRotation(4)},
		{name: "digest/one-leaf/honest-rotated/after-three-rotations", door: digestDoor, arity: 1, followed: true,
			shape: "THE CONTROL AT THE TOP OF THE heldAt INTERVAL. The same world as " +
				"`earlier-epoch/held-at-epoch-3` -- three honest rotations first -- and then a " +
				"removal that DOES rotate, which is followed. Without it the refusal at heldAt 3 " +
				"reads as a refusal of a DEEP HISTORY rather than of the replay inside it, which " +
				"is the same argument the four-leaf control above makes on the arity axis",
			build: buildHonestRotationAfter(3, 1)},
		{name: "digest/no-removal/unrotated", door: digestDoor, arity: 0, followed: true,
			shape: "THE COMPLEMENT OF THE GUARD'S PREDICATE. An epoch change on the secret the " +
				"group already holds that removes NOBODY -- every group on the deployed alpha -- " +
				"is followed. A guard that refuses everything goes red here",
			build: buildUnrotatedNoRemoval},
		{name: "digest/one-leaf/one-octet-from-a-held-secret", door: digestDoor, arity: 1, followed: true,
			shape: "A REMOVAL FANNED OUT ON A SECRET THAT DIFFERS FROM A HELD ONE IN A SINGLE " +
				"OCTET. It is FRESH, so it must be FOLLOWED: the rule is on the octets and not " +
				"on a resemblance, and a comparison narrowed to a prefix or to a length goes red",
			build: buildOneOctetFromHeld},

		// ── the digest door: the table, not the current row; and the committer axis ─────────
		{name: "digest/one-leaf/earlier-epoch", door: digestDoor, arity: 1, heldAt: 1, followed: false,
			shape: "a removal opening epoch 3 on pq_secret[1]: a different octet string, the same " +
				"removed member holding it, and a subject narrowed to the current row answers no",
			build: buildEarlierEpochReplay(1, 1)},
		{name: "digest/one-leaf/earlier-epoch/held-at-epoch-3", door: digestDoor, arity: 1, heldAt: 3, followed: false,
			shape: "THE TOP OF THE SECOND INTERVAL. A group that has rotated three honest times " +
				"and a committer that replays pq_secret[3]: the same shape as the row above it " +
				"with the REPLAYED EPOCH moved up, so the refusal it takes names epoch 3. This is " +
				"the arity axis's counterpart and it is here because the axis was one point wide " +
				"below three: `alreadyHeld = alreadyHeld && heldAt < 3` written between the " +
				"binding and the guard (pqepoch.go sha256 1608f28c11c9) passed this table, both " +
				"predicate readings and all 171 cases in this package. A narrowing a driven case " +
				"can catch belongs in the table as a row",
			build: buildEarlierEpochReplay(3, 3)},
		{name: "digest/one-leaf/earlier-epoch/late-joiner", door: digestDoor, arity: 1, heldAt: 1, followed: false,
			shape: "the same replay in a world holding a member admitted at epoch 3. The FOUNDER " +
				"refuses it; the late joiner FOLLOWS it, in this row's own `then`, which is " +
				"ledger ruling 43's first residual driven rather than described",
			build: buildLateJoinerResidual},
		{name: "digest/one-leaf/committed-by-a-non-owner-admin", door: digestDoor, arity: 1, heldAt: 2, followed: false,
			shape: "the removal is committed by a promoted ADMIN and not by the founder, so " +
				"`the committer is the group's owner` stops being a silent premise of every " +
				"removal case in this package",
			build: buildAdminCommitter},

		// ── the digest door: a removal bundled with an add ──────────────────────────────────
		{name: "digest/one-leaf-and-one-add/current-epoch", door: digestDoor, arity: 1, heldAt: 1, followed: false,
			shape: "ONE COMMIT THAT BOTH ADDS AND REMOVES, on the held secret. The removed-leaf " +
				"list is not the whole of what the commit does, and the rule is keyed to the " +
				"removal inside it",
			build: buildBundledRemoval(false)},
		{name: "digest/one-leaf-and-one-add/honest-rotated", door: digestDoor, arity: 1, followed: true,
			shape: "THE CONTROL for the bundle: the same add-and-remove commit, rotated honestly, " +
				"is followed",
			build: buildBundledRemoval(true)},

		// ── the other door: the 0x0001 class, which had no gate on its own predicate ────────
		{name: "noDigest/one-leaf", door: noDigestDoor, arity: 1, doorEpoch: 1, followed: false,
			shape: "a removal on a commit carrying no epoch digest at all -- the shape every " +
				"build before the rotation emits -- refused BEFORE ApplyCommit",
			build: buildNoDigestRemoval(1)},
		{name: "noDigest/two-leaves", door: noDigestDoor, arity: 2, doorEpoch: 1, followed: false,
			shape: "the same, two leaves",
			build: buildNoDigestRemoval(2)},
		{name: "noDigest/three-leaves", door: noDigestDoor, arity: 3, doorEpoch: 1, followed: false,
			shape: "the same, THREE leaves. This door's predicate was held by neither behaviour " +
				"nor structure at any arity above one",
			build: buildNoDigestRemoval(3)},
		{name: "noDigest/four-leaves", door: noDigestDoor, arity: 4, doorEpoch: 1, followed: false,
			shape: "the same, FOUR leaves. The two doors carry the same arity interval on " +
				"purpose: a narrowing written at one arity above the table is a mutant at EITHER " +
				"door, and an interval that stopped a leaf short here would be the door's own " +
				"edge sitting somewhere a reader has to work out",
			build: buildNoDigestRemoval(4)},
		{name: "noDigest/one-leaf-and-one-add", door: noDigestDoor, arity: 1, doorEpoch: 1, followed: false,
			shape: "a digest-less commit that both adds and removes, refused before the apply",
			build: buildNoDigestBundle},
		{name: "noDigest/no-removal", door: noDigestDoor, arity: 0, doorEpoch: 1, followed: true,
			shape: "THE COMPLEMENT AT THIS DOOR. A digest-less commit that removes NOBODY reaches " +
				"the resolution's no-digest arm and is followed; a door that refused every " +
				"digest-less commit goes red here",
			build: buildNoDigestNoRemoval(0)},

		// ── the other door at a SECOND self.epoch, which is its own undriven axis ───────────
		{name: "noDigest/one-leaf/after-one-rotation", door: noDigestDoor, arity: 1, doorEpoch: 2, followed: false,
			shape: "THE SAME DIGEST-LESS REMOVAL ONE HONEST ROTATION LATER, so this door is " +
				"decided at `self.epoch == 2` and not only at the founding epoch. Every " +
				"digest-less call of it arrived at epoch 1 until this row existed, MEASURED by a " +
				"`println` at the top of the door, and a narrowing keyed on `1 < self.epoch` -- " +
				"as a top-level `if`, as an `else if` welded onto `digest != nil`, or as a " +
				"`switch` -- passed this table at every arity",
			build: buildNoDigestRemovalAfter(1, 1)},
		{name: "noDigest/no-removal/after-one-rotation", door: noDigestDoor, arity: 0, doorEpoch: 2, followed: true,
			shape: "THE COMPLEMENT AT THAT SECOND EPOCH. A digest-less commit removing NOBODY at " +
				"epoch 2 is followed, so the row above it is not a refusal of the EPOCH itself -- " +
				"every point of this axis carries both dispositions, for the reason the arity " +
				"controls give",
			build: buildNoDigestNoRemoval(1)},
	}

	// ── THE COMPLEMENT, PRINTED BEFORE ANYTHING IS ASSERTED ─────────────────────────────────
	//
	// What this instrument does NOT drive is the thing a reader has to be able to see, because a
	// finite table reads as complete unless its edges are written down. The arities are the axis
	// every one of the six bypassed gates was defeated on.
	drivenArities := map[removalDoor][]int{}
	dispositions := map[string]int{}
	// THE SECOND AXIS AT EACH DOOR, WHICH IS THE 2026-09-24 (EIGHTH PASS) ADDITION. Neither door
	// decides on arity alone -- the resolution reads the held answer, which carries `heldAt`, and
	// this door reads `self.epoch` -- and a complement printed on one axis of three reads as a
	// complement. `heldAt` is collected over the REFUSING rows of the digest door, because those
	// are the rows whose refusal carries a `heldAt` to name; `self.epoch` is collected over every
	// row of the other door, because that door reads it on every call.
	drivenHeldAt, drivenDoorEpochs := []uint64{}, []uint64{}
	for _, row := range rows {
		if !slices.Contains(drivenArities[row.door], row.arity) {
			drivenArities[row.door] = append(drivenArities[row.door], row.arity)
		}
		if row.door == digestDoor && !row.followed && !slices.Contains(drivenHeldAt, row.heldAt) {
			drivenHeldAt = append(drivenHeldAt, row.heldAt)
		}
		if row.door == noDigestDoor && !slices.Contains(drivenDoorEpochs, row.doorEpoch) {
			drivenDoorEpochs = append(drivenDoorEpochs, row.doorEpoch)
		}
		key := string(row.door) + "/followed"
		if !row.followed {
			key = string(row.door) + "/refused"
		}
		dispositions[key] += 1
	}
	slices.Sort(drivenHeldAt)
	slices.Sort(drivenDoorEpochs)
	// THE AXIS IS PRINTED AS AN INTERVAL AND AS THE FACT THAT IT IS BOUNDED, which is the
	// 2026-09-24 (seventh pass) correction. It was printed as the SET {0, 1, 2, 3}, and a set
	// reads as a choice of points while the thing a reader needs is the EDGE: for any constant k
	// this table drives, `< k+1` is the narrowing outside it, so what matters is where the top is
	// and that there IS a top. Both are in the line now, with the first undriven arity named.
	//
	// AND THE SECOND AXIS IS IN THE SAME LINE, which is the 2026-09-24 (eighth pass) correction:
	// an axis printed in a different place from the one a reader is looking at is an axis nobody
	// compares. Each door's line now names both of the inputs this table drives as intervals AND
	// the first point above each top, so the two residuals read the same way.
	for _, door := range []removalDoor{digestDoor, noDigestDoor} {
		arities := drivenArities[door]
		slices.Sort(arities)
		second, top := "heldAt (the epoch the refusal NAMES)", drivenHeldAt
		outside := "`alreadyHeld = alreadyHeld && heldAt < %d` between the binding and the guard, " +
			"which NOTHING here and nothing in pqdarkgate_test.go reads"
		if door == noDigestDoor {
			second, top = "self.epoch (where this door is decided)", drivenDoorEpochs
			outside = "`%d <= self.epoch -> return nil` in the door, in any conditional shape"
		}
		t.Logf("DOOR %q drives len(removedLeaves) over the INTERVAL [%d, %d], every arity in it "+
			"and NOTHING ABOVE IT, AND %s over the INTERVAL [%d, %d]: %d followed row(s), %d "+
			"refused. BOTH axes are BOUNDED, so a bypass keyed on %d <= len(removedLeaves) -- "+
			"`len(removedLeaves) < %d` at either door -- passes this table, and so does one keyed "+
			"one step above the second interval: "+outside+". Above the arity interval and above "+
			"the second one, the predicate readings in pqdarkgate_test.go cover each door's own "+
			"CONDITION, in any conditional shape, and nothing else",
			door, arities[0], arities[len(arities)-1],
			second, top[0], top[len(top)-1],
			dispositions[string(door)+"/followed"], dispositions[string(door)+"/refused"],
			arities[len(arities)-1]+1, arities[len(arities)-1]+1,
			top[len(top)-1]+1)
	}

	// ── AND THE TABLE'S OWN CONTROLS ────────────────────────────────────────────────────────
	for _, door := range []removalDoor{digestDoor, noDigestDoor} {
		if dispositions[string(door)+"/followed"] == 0 || dispositions[string(door)+"/refused"] == 0 {
			t.Fatalf("CONTROL FAILED: door %q has %d followed row(s) and %d refused one(s). A door "+
				"with no followed row is satisfied by a build that refuses everything, and one "+
				"with no refused row measures nothing at all", door,
				dispositions[string(door)+"/followed"], dispositions[string(door)+"/refused"])
		}
		// THE PRINTED INTERVAL IS AN INTERVAL, which is what makes the line above a complement
		// rather than a summary: a hole in it would make `[0, 4]` false while the two endpoints
		// stayed true, and a deleted middle row is exactly how that happens.
		arities := drivenArities[door]
		for at, arity := range arities {
			if arity != arities[0]+at {
				t.Fatalf("CONTROL FAILED: door %q drives %v, which is not the contiguous interval "+
					"the line above prints -- arity %d is missing. A hole makes that line false "+
					"in the one direction it exists to be true in", door, arities, arities[0]+at)
			}
		}
		if arities[0] != 0 {
			t.Fatalf("CONTROL FAILED: door %q drives %v and does not start at zero, so the "+
				"COMPLEMENT of the rule -- a commit that removes nobody -- is not driven here",
				door, arities)
		}
		if arities[len(arities)-1] < 4 {
			t.Fatalf("CONTROL FAILED: door %q drives %v and stops below FOUR leaves. Three is the "+
				"axis three separate mutants survived on; four is where the sixth pass's own "+
				"narrowing sat -- `len(removedLeaves) < 4` written one line below the condition "+
				"the static reading reads -- and it passed everything while this table stopped "+
				"at three", door, arities)
		}
	}

	// ── AND THE SECOND AXIS HAS THE SAME THREE CONTROLS, WHICH IS WHAT MAKES IT AN AXIS ─────
	//
	// A second interval printed without controls is the first one's mistake repeated: `[1, 3]`
	// stays true at both endpoints while the middle is gone, and a top nobody asserts drifts back
	// down the next time a row is deleted. Both axes start at ONE and not at zero -- epoch 1 is
	// the founding epoch, the lowest a device can hold anything at and the lowest this door can be
	// decided at -- which is the difference from the arity axis and is why it is a separate check
	// rather than the same loop.
	for _, axis := range []struct {
		name    string
		driven  []uint64
		atLeast uint64
		why     string
	}{
		{name: "heldAt at the digest door", driven: drivenHeldAt, atLeast: 3,
			why: "epoch 3 is where `alreadyHeld = alreadyHeld && heldAt < 3` (pqepoch.go sha256 " +
				"1608f28c11c9) sat and survived this table, both predicate readings and all 171 " +
				"cases in this package, while every refusing row here named epoch 1 or 2"},
		{name: "self.epoch at the pre-apply door", driven: drivenDoorEpochs, atLeast: 2,
			why: "every digest-less call of that door arrived at epoch 1 until a row drove a " +
				"second point, so `1 < self.epoch -> return nil` -- as a top-level `if`, as an " +
				"`else if`, or as a `switch` -- passed this table at every arity it drives"},
	} {
		if len(axis.driven) == 0 {
			t.Fatalf("CONTROL FAILED: %s is driven at no point at all, so the interval printed "+
				"above it is empty and the line is a summary of nothing", axis.name)
		}
		// AND WHAT THIS ONE CATCHES TODAY, SAID RATHER THAN ASSUMED. Contiguity is load-bearing on
		// the heldAt axis, which is three points wide: deleting the row at heldAt 2 leaves [1 3]
		// and this fires. On the door's epoch axis it is two points wide, so there is no middle to
		// delete and the printed interval is already held whole by the two checks below it. It is
		// written for both because the third point is one row away, and a control added the day it
		// is needed is one nobody has driven.
		for at, point := range axis.driven {
			if point != axis.driven[0]+uint64(at) {
				t.Fatalf("CONTROL FAILED: %s drives %v, which is not the contiguous interval the "+
					"line above prints -- %d is missing. A hole makes that line false in the one "+
					"direction it exists to be true in", axis.name, axis.driven, axis.driven[0]+uint64(at))
			}
		}
		if axis.driven[0] != 1 {
			t.Fatalf("CONTROL FAILED: %s drives %v and does not start at the FOUNDING EPOCH. "+
				"Epoch 1 is the lowest value this input can take, and an interval that starts "+
				"above it leaves the oldest history in the group undriven", axis.name, axis.driven)
		}
		if axis.driven[len(axis.driven)-1] < axis.atLeast {
			t.Fatalf("CONTROL FAILED: %s drives %v and stops below %d. %s", axis.name,
				axis.driven, axis.atLeast, axis.why)
		}
	}

	// THE THIRD INPUT OF THE RESOLUTION'S EXIT IS THE ARM, and it is MEASURED rather than
	// declared: each refusing row of the digest door reports which of [Group.resolvePqSecretLocked]'s
	// arms called the guard, read out of the sentence production itself wrote.
	armsAtARefusal := map[string]int{}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			built := row.build(t)
			if arm := built.drive(t, row); arm != "" {
				armsAtARefusal[arm] += 1
			}
		})
	}

	// ── AND THE THIRD AXIS, PRINTED FROM WHAT THE RUN ACTUALLY REACHED ──────────────────────
	//
	// WHY THIS ONE IS NOT AN INTERVAL. The arm is a finite enumeration and not a number, so its
	// complement is stated by naming the member no row reaches and WHY: the third arm, "carries no
	// epoch digest at all", cannot reach a refusal here at all, because a digest-less commit that
	// removes a leaf is refused by the OTHER door before ApplyCommit. That is a theorem about the
	// two doors and not a gap -- and the `noDigest/no-removal` rows drive that arm at arity zero,
	// where it is FOLLOWED, so the arm is reached and only its refusing disposition is unreachable.
	arms, reached := slices.Sorted(maps.Keys(armsAtARefusal)), 0
	for _, count := range armsAtARefusal {
		reached += count
	}
	t.Logf("THE RESOLUTION'S THIRD INPUT, MEASURED: %d refusing row(s) reached %d of the exit's "+
		"THREE arms -- %q. The arm this table cannot drive to a refusal is `carries no epoch "+
		"digest at all`, and it is unreachable by construction rather than by omission: a "+
		"digest-less commit that removes a leaf never gets past the pre-apply door",
		reached, len(arms), arms)
	if len(arms) < 2 {
		t.Fatalf("CONTROL FAILED: every refusing row of the digest door reached the same arm "+
			"(%v). A narrowing keyed on `how` would then be a narrowing this table cannot see, "+
			"and the line above would be reporting one point as an enumeration", arms)
	}
}

// drive puts one built page through the production receive path and holds everything a
// disposition means. It is shared by every row, so what a row says is a SHAPE. It answers the arm
// of [Group.resolvePqSecretLocked] the refusal came from, or "" when the row took no refusal that
// names one.
func (self *removalPage) drive(t *testing.T, row removalInput) string {
	t.Helper()
	world, receiver := self.world, self.receiver
	t.Logf("SHAPE: %s", row.shape)

	// ── EVERY DECLARED AXIS IS HELD AGAINST THE BUILT PAGE, BEFORE ANYTHING ELSE ────────────
	//
	// The intervals are printed from the DECLARATIONS on the rows, above, and a declaration that
	// disagrees with what its builder produces makes that line false while every row still passes.
	// Arity is checkable here; `heldAt` and `self.epoch` are checked below against what production
	// itself reports, which is the stronger direction and is why they are not re-derived here.
	if row.arity != len(self.removes) {
		t.Fatalf("CONTROL FAILED: this row declares arity %d and its page removes %v. The interval "+
			"printed above is built from the DECLARATIONS, so a row that declares one arity and "+
			"drives another makes that line false in the direction it exists to be true in",
			row.arity, self.removes)
	}

	// ── THE COUNTERFACTUAL, FIRST, so the disposition below is about something ──────────────
	if self.retained != nil {
		reproduces := world.reproduces(self.committer, self.retained, self.opensOn)
		switch {
		case !row.followed && !reproduces:
			t.Fatalf("CONTROL FAILED: this fixture's removal DID rotate -- the removed member's " +
				"retained pq_secret does not reproduce the committer's storage root at the epoch " +
				"it was removed at -- so it is not the shape this row refuses and the refusal " +
				"below would be about nothing")
		case row.followed && reproduces:
			t.Fatalf("CONTROL FAILED: this fixture's removal did NOT rotate, so the row is " +
				"asserting that an unrotated removal is followed. Either the builder is wrong or " +
				"the rule is gone")
		}
		t.Logf("the counterfactual: the removed member's retained pq_secret %s the committer's "+
			"storage_root at epoch %d", map[bool]string{true: "REPRODUCES", false: "does NOT reproduce"}[reproduces],
			self.opens)
	}
	if self.also != nil {
		self.also(t)
	}

	at := receiver.group.epoch
	handleAt := receiver.handle.Epoch()
	before := append([]byte(nil), receiver.group.pqSecretLocked()...)
	refusedBefore := receiver.group.Stats().CommitRefused

	// `self.epoch` INSIDE THE PRE-APPLY DOOR IS THE RECEIVER'S EPOCH HERE, one statement before
	// the page goes in, so the declaration that builds this door's printed interval is held
	// against the value the door will actually read.
	if row.door == noDigestDoor && at != row.doorEpoch {
		t.Fatalf("CONTROL FAILED: this row declares that %s's door runs at self.epoch %d and the "+
			"receiver stands at %d. That declaration is what the epoch interval printed above is "+
			"built from", receiver.name, row.doorEpoch, at)
	}
	if row.door == digestDoor && row.doorEpoch != 0 {
		t.Fatalf("CONTROL FAILED: this row is at the digest door and declares doorEpoch %d. That "+
			"field is the PRE-APPLY door's input and a row of the other door carrying one would "+
			"put a point on an interval nothing drives", row.doorEpoch)
	}

	err := world.deliver(receiver, self.page...)

	if row.followed {
		if err != nil {
			t.Fatalf("%s REFUSED a shape that must be followed: %v", receiver.name, err)
		}
		if receiver.group.epoch != self.opens {
			t.Fatalf("%s stands at epoch %d after following, want %d", receiver.name,
				receiver.group.epoch, self.opens)
		}
		if receiver.group.halted != nil {
			t.Fatalf("%s halted on a shape it followed: %v", receiver.name, receiver.group.halted)
		}
		if receiver.group.wrapDark != nil {
			t.Fatalf("%s went dark on a shape it followed: %v", receiver.name, receiver.group.wrapDark)
		}
		if !bytes.Equal(receiver.group.pqSecretLocked(), self.opensOn) {
			t.Fatalf("%s followed onto a pq_secret that is not the one this epoch runs on, so it "+
				"is in the epoch and cannot read it", receiver.name)
		}
		if self.then != nil {
			self.then(t)
		}
		return ""
	}

	// ── THE REFUSAL, BY NAME ────────────────────────────────────────────────────────────────
	if err == nil {
		t.Fatalf("%s FOLLOWED this removal. The removed member's retained pq_secret reproduces "+
			"the survivors' storage_root at epoch %d, so the removal removed nothing",
			receiver.name, self.opens)
	}
	if !errors.Is(err, ErrRemovalWithoutRotation) {
		t.Fatalf("%s's walk answered %v, want ErrRemovalWithoutRotation", receiver.name, err)
	}
	// ── AND IT NAMES THE COMMIT'S WHOLE REMOVED-LEAF LIST ───────────────────────────────────
	//
	// This is what holds every narrowing of that list without a gate reading one: a truncation
	// anywhere between connect's `processed.Commit.RemovedLeaves()` and the refusal still REFUSES
	// -- the predicate is `0 < len(...)` -- and is invisible to a disposition, but it cannot print
	// leaves it no longer has.
	self.assertNamesEveryRemovedLeaf(t, err)
	// ── AND THE EPOCH IT NAMES IS THE SECOND AXIS, READ OFF PRODUCTION'S OWN SENTENCE ───────
	//
	// [refuseRemovalOnHeldSecret] prints `heldAt`, so the number this row declares is held against
	// the number the EXIT computed rather than against a re-derivation in this file -- which is
	// the difference between an axis and a label. The pre-apply door cannot name one: it never
	// asks [Group.pqSecretHeldAtLocked] anything, and a row of that door declaring a heldAt would
	// put a point on the digest door's interval that nothing drives.
	arm := ""
	if named, readable := heldAtNamedIn(err.Error()); readable {
		if row.door != digestDoor {
			t.Fatalf("CONTROL FAILED: this row is at door %q and its refusal names a heldAt of "+
				"%d. Only the resolution's exit asks that question, so a refusal from the "+
				"pre-apply door carrying one means the doors have moved", row.door, named)
		}
		if named != row.heldAt {
			t.Fatalf("CONTROL FAILED: this row declares heldAt %d and the refusal production "+
				"wrote names epoch %d. The heldAt interval printed above is built from the "+
				"DECLARATIONS, so a row sitting somewhere other than where it says makes that "+
				"line false", row.heldAt, named)
		}
		arm, _ = armNamedIn(err.Error())
	} else if row.heldAt != 0 {
		t.Fatalf("CONTROL FAILED: this row declares heldAt %d and its refusal names no epoch at "+
			"all: %q. A declared point that no refusal reports is a point on the printed "+
			"interval that nothing drives", row.heldAt, err.Error())
	}
	// ── THE GROUP DID NOT FOLLOW IT, WHICH IS RULING 41 ─────────────────────────────────────
	if receiver.group.epoch != at {
		t.Fatalf("%s stands at epoch %d after refusing, want %d: a refused commit is not followed",
			receiver.name, receiver.group.epoch, at)
	}
	if receiver.group.wrapDark != nil {
		t.Fatalf("%s went DARK over a commit it refused: %v. Ruling 41's two outcomes are two "+
			"fields, and an invalid commit is refused the way an unauthorized one is",
			receiver.name, receiver.group.wrapDark)
	}
	if receiver.group.halted == nil {
		t.Fatalf("%s refused the commit and did not halt, so the refusal is answered once and the "+
			"next walk sees a group an epoch behind its own log", receiver.name)
	}
	if !bytes.Equal(receiver.group.pqSecretLocked(), before) {
		t.Fatalf("%s's pq_secret moved although it did not follow the commit", receiver.name)
	}
	if _, isHeld := receiver.group.pqSecretAtLocked(self.opens); isHeld {
		t.Fatalf("%s filed a pq_secret for epoch %d although it refused the commit that opens it",
			receiver.name, self.opens)
	}
	if got := receiver.group.Stats().CommitRefused; got != refusedBefore+1 {
		t.Fatalf("%s counted %d refusal(s) over this page, want %d", receiver.name, got, refusedBefore+1)
	}
	// ── THE HALT IS STICKY AND IS ANSWERED BY SEND AND BY COMMIT ────────────────────────────
	if _, err := receiver.group.sendableLocked(KindText); !errors.Is(err, ErrRemovalWithoutRotation) {
		t.Fatalf("%s's Send answers %v; a halted group stands at an epoch the server has already "+
			"left, so leaving Send open hands the user an undiagnosable refusal by another road",
			receiver.name, err)
	}
	if err := receiver.group.committableLocked(); !errors.Is(err, ErrRemovalWithoutRotation) {
		t.Fatalf("%s's Commit answers %v, want the halt", receiver.name, err)
	}
	// ── AND WHICH DOOR TOOK IT, WHICH IS WHERE THE MLS HANDLE IS ────────────────────────────
	switch row.door {
	case noDigestDoor:
		if receiver.handle.Epoch() != handleAt {
			t.Fatalf("%s's MLS handle stands at epoch %d and it stood at %d before the page. This "+
				"row is the 0x0001 class and its whole claim is that the decision is taken BEFORE "+
				"ApplyCommit; if it has moved to the resolution, move the row and say what "+
				"evidence it uses", receiver.name, receiver.handle.Epoch(), handleAt)
		}
	case digestDoor:
		if receiver.handle.Epoch() != self.opens {
			t.Fatalf("%s's MLS handle stands at epoch %d, want %d. Judging a digest needs "+
				"mls_secret[n+1], so the apply has run by then and the handle HAS moved -- that "+
				"is the residual of taking the decision here and it is asserted rather than "+
				"glossed", receiver.name, receiver.handle.Epoch(), self.opens)
		}
	}
	if self.then != nil {
		self.then(t)
	}
	return arm
}

// heldAtNamedIn reads the epoch [refuseRemovalOnHeldSecret] says this receiver first held the
// answered value at, out of the sentence production wrote, or reports that the sentence carries
// none -- which is every refusal from the pre-apply door.
func heldAtNamedIn(message string) (uint64, bool) {
	at := strings.Index(message, heldAtOpener)
	if at < 0 {
		return 0, false
	}
	value, err := strconv.ParseUint(strings.TrimSpace(message[at+len(heldAtOpener):]), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// armNamedIn reads `how` -- which arm of [Group.resolvePqSecretLocked] answered -- out of the same
// sentence. It is the exit's third input and it is enumerated rather than bounded, so it is
// collected from the run and printed instead of being declared on the rows.
func armNamedIn(message string) (string, bool) {
	const opener = "] and "
	at := strings.Index(message, opener)
	end := strings.Index(message, heldAtOpener)
	if at < 0 || end < 0 || end < at+len(opener) {
		return "", false
	}
	return strings.TrimSuffix(message[at+len(opener):end], " -- "), true
}

// heldAtOpener is the tail of [refuseRemovalOnHeldSecret]'s own format string, copied from it. Two
// readers take it, so a change to that sentence moves one literal and not two.
const heldAtOpener = "a value THIS DEVICE has held at epoch "

// assertNamesEveryRemovedLeaf holds that the refusal an operator reads names the commit's WHOLE
// removed-leaf list, as a set and not in a fixed order -- the order is connect's application order
// and is not this package's to assert.
func (self *removalPage) assertNamesEveryRemovedLeaf(t *testing.T, err error) {
	t.Helper()
	message := err.Error()
	named, readable := leavesNamedIn(message)
	if !readable {
		// The pre-apply door names a COUNT rather than a list, because it is refusing before the
		// authorization decision's list has anything else read off it.
		want := fmt.Sprintf("removes %d leaf/leaves", len(self.removes))
		if !strings.Contains(message, want) {
			t.Fatalf("the refusal reads %q and names neither a leaf list nor %q. A truncation of "+
				"the removed-leaf list between connect and the guard still refuses -- the "+
				"predicate is `0 < len(...)` -- and this sentence is the only thing that can show "+
				"it", message, want)
		}
		return
	}
	slices.Sort(named)
	want := append([]uint32(nil), self.removes...)
	slices.Sort(want)
	if !slices.Equal(named, want) {
		t.Fatalf("the refusal names leaves %v and the commit removes %v. A list narrowed anywhere "+
			"between connect's processed.Commit.RemovedLeaves() and the guard still REFUSES, so "+
			"the disposition cannot see it and this sentence is what does", named, want)
	}
}

// leavesNamedIn reads the leaf list out of [refuseRemovalOnHeldSecret]'s sentence, or reports that
// the sentence does not carry one.
func leavesNamedIn(message string) ([]uint32, bool) {
	const opener = "removes leaf/leaves ["
	at := strings.Index(message, opener)
	if at < 0 {
		return nil, false
	}
	rest := message[at+len(opener):]
	end := strings.Index(rest, "]")
	if end < 0 {
		return nil, false
	}
	named := []uint32{}
	for _, field := range strings.Fields(rest[:end]) {
		value, err := strconv.ParseUint(field, 10, 32)
		if err != nil {
			return nil, false
		}
		named = append(named, uint32(value))
	}
	return named, true
}

// ── the builders ─────────────────────────────────────────────────────────────────────────────

// removalCohort founds a world with one committer, one receiver and `victims` members to remove,
// and answers the three things every builder needs. THE RECEIVER IS NEVER THE COMMITTER: a
// committer does not run the receive path against its own commit, which is ledger ruling 43's
// second residual and is why it cannot be the subject here.
func removalCohort(t *testing.T, victims int) (*rotWorld, *rotMember, *rotMember, []uint32, []byte) {
	t.Helper()
	names := []string{"alice", "bob"}
	for at := 0; at < victims; at += 1 {
		names = append(names, fmt.Sprintf("victim%d", at))
	}
	world := newRotWorld(t, names...)
	committer, receiver := world.member("alice"), world.member("bob")
	removing := []uint32{}
	for at := 0; at < victims; at += 1 {
		removing = append(removing, world.member(fmt.Sprintf("victim%d", at)).leaf)
	}
	retained := []byte(nil)
	if 0 < victims {
		retained = append([]byte(nil), world.member("victim0").group.pqSecretLocked()...)
	}
	return world, committer, receiver, removing, retained
}

// removalVictims answers the members [removalCohort] founded to be removed, so a builder that has
// to deliver something to all of them does not re-spell their names.
func removalVictims(world *rotWorld, victims int) []*rotMember {
	world.t.Helper()
	members := []*rotMember{}
	for at := 0; at < victims; at += 1 {
		members = append(members, world.member(fmt.Sprintf("victim%d", at)))
	}
	return members
}

// rotateHonestly runs `rounds` honest rotations that remove nobody, delivers each to every
// receiver, and answers the pq_secret each round opened its epoch on, BY THAT EPOCH.
//
// IT EXISTS BECAUSE THE SECOND AXIS NEEDS A HISTORY AND THREE BUILDERS NEED THE SAME ONE. At epoch
// 1 a device holds exactly one row, so `heldAt` can only be 1 and `self.epoch` can only be 1 -- the
// two inputs the 2026-09-24 (eighth pass) repair drives -- and every builder that wants a second
// point has to walk the receiver forward first. The map is keyed by epoch so a caller can replay a
// NAMED one rather than counting rotations backwards.
func (self *rotWorld) rotateHonestly(committer *rotMember, rounds int, receivers ...*rotMember) map[uint64][]byte {
	self.t.Helper()
	opened := map[uint64][]byte{}
	for at := 0; at < rounds; at += 1 {
		published := self.rotate(committer, func() ([]byte, []byte, []byte, error) {
			return committer.handle.Commit(nil)
		})
		for _, receiver := range receivers {
			if err := self.deliver(receiver, published.page()...); err != nil {
				self.t.Fatalf("CONTROL FAILED: %s's walk over honest rotation %d of %d answered %v",
					receiver.name, at+1, rounds, err)
			}
		}
		opened[published.opens] = append([]byte(nil), published.pqSecret...)
	}
	return opened
}

func buildHeldFanOut(victims int, how unrotatedFanOut) func(t *testing.T) *removalPage {
	return func(t *testing.T) *removalPage {
		world, committer, receiver, removing, retained := removalCohort(t, victims)
		published := world.fanOutOnTheHeldSecret(committer, func() ([]byte, []byte, []byte, error) {
			return committer.handle.CommitRemove(removing)
		}, how)
		page := &removalPage{world: world, committer: committer, receiver: receiver,
			page: published.page(), opens: published.opens, removes: removing,
			opensOn: published.pqSecret, retained: retained}
		page.also = func(t *testing.T) {
			// THE FAN-OUT IS REAL AND THE RECEIVER OPENS ITS OWN WRAP, so the refusal is not an
			// absence. Without it a page the receiver could not read would satisfy the row, and
			// that is a different sentinel and a different defect.
			if err := world.deliver(receiver, published.wraps...); err != nil {
				t.Fatalf("CONTROL FAILED: %s's walk over the fan-out alone answered %v", receiver.name, err)
			}
			if opened := receiver.group.Stats().WrapOpened; opened != 1 {
				t.Fatalf("CONTROL FAILED: %s opened %d wrap(s) of this fan-out, want 1", receiver.name, opened)
			}
			if len(removing) != victims || len(slices.Compact(slices.Sorted(slices.Values(removing)))) != victims {
				t.Fatalf("CONTROL FAILED: this row removes %v, which is not %d distinct leaves",
					removing, victims)
			}
			page.page = []*sealed{published.commit}
		}
		return page
	}
}

func buildFreshFanOutHeldDigest(victims int) func(t *testing.T) *removalPage {
	return func(t *testing.T) *removalPage {
		fresh := make([]byte, messagegroup.PqSecretBytes)
		if _, err := rand.Read(fresh); err != nil {
			t.Fatalf("the fresh wrap payload: %v", err)
		}
		return buildHeldFanOut(victims, unrotatedFanOut{payload: fresh})(t)
	}
}

func buildHonestRotation(victims int) func(t *testing.T) *removalPage {
	return buildHonestRotationAfter(0, victims)
}

// buildHonestRotationAfter is the honest rotated removal with a HISTORY in front of it: `rotations`
// honest rotations that remove nobody, walked by the receiver and by every victim, and then the
// removal. At `rotations == 0` it is exactly what [buildHonestRotation] always was.
//
// THE RETAINED VALUE IS READ AFTER THE ROTATIONS AND NOT BEFORE THEM, for [buildAdminCommitter]'s
// reason: the counterfactual has to be about the row the committer is replaying, and the founding
// value stops being that row the moment the group moves.
func buildHonestRotationAfter(rotations int, victims int) func(t *testing.T) *removalPage {
	return func(t *testing.T) *removalPage {
		world, committer, receiver, removing, retained := removalCohort(t, victims)
		walkers := append([]*rotMember{receiver}, removalVictims(world, victims)...)
		world.rotateHonestly(committer, rotations, walkers...)
		if 0 < victims {
			retained = append([]byte(nil), world.member("victim0").group.pqSecretLocked()...)
		}
		published := world.rotate(committer, func() ([]byte, []byte, []byte, error) {
			return committer.handle.CommitRemove(removing)
		})
		page := &removalPage{world: world, committer: committer, receiver: receiver,
			page: published.page(), opens: published.opens, removes: removing,
			opensOn: published.pqSecret, retained: retained}
		page.also = func(t *testing.T) {
			if want := rotations + 1; len(receiver.group.pqSecrets) != want {
				t.Fatalf("CONTROL FAILED: %s holds %d pq_secret row(s) after %d honest "+
					"rotation(s), want %d. This row is the FOLLOWED control at the top of the "+
					"heldAt interval and its whole claim is that the world has a history",
					receiver.name, len(receiver.group.pqSecrets), rotations, want)
			}
		}
		return page
	}
}

func buildUnrotatedNoRemoval(t *testing.T) *removalPage {
	world, committer, receiver, _, _ := removalCohort(t, 0)
	published := world.advanceWithoutRotating(committer, func() ([]byte, []byte, []byte, error) {
		return committer.handle.Commit(nil)
	})
	return &removalPage{world: world, committer: committer, receiver: receiver,
		page: published.page(), opens: published.opens, removes: nil,
		opensOn: published.pqSecret}
}

func buildOneOctetFromHeld(t *testing.T) *removalPage {
	world, committer, receiver, removing, retained := removalCohort(t, 1)
	// ONE OCTET, AND IT IS THE LAST ONE. A value that differs from a held secret anywhere is a
	// fresh value; a rule that answered "held" for this would be answering about a resemblance.
	bent := append([]byte(nil), retained...)
	bent[len(bent)-1] ^= 0x01
	published := world.fanOutOnTheHeldSecret(committer, func() ([]byte, []byte, []byte, error) {
		return committer.handle.CommitRemove(removing)
	}, unrotatedFanOut{opensOn: bent})
	page := &removalPage{world: world, committer: committer, receiver: receiver,
		page: published.page(), opens: published.opens, removes: removing,
		opensOn: published.pqSecret, retained: retained}
	page.also = func(t *testing.T) {
		differing := 0
		for at := range retained {
			if retained[at] != bent[at] {
				differing += 1
			}
		}
		if differing != 1 {
			t.Fatalf("CONTROL FAILED: the value this epoch is opened on differs from the held one "+
				"in %d octet(s), want exactly 1", differing)
		}
		if !bytes.Equal(published.pqSecret, bent) {
			t.Fatalf("CONTROL FAILED: the fixture opened epoch %d on something other than the "+
				"bent value", published.opens)
		}
		if _, held := receiver.group.pqSecretHeldAtLocked(bent); held {
			t.Fatalf("CONTROL FAILED: %s answers HELD for a value it has never filed, so this row "+
				"would be followed for the wrong reason", receiver.name)
		}
		if _, held := receiver.group.pqSecretHeldAtLocked(retained); !held {
			t.Fatalf("CONTROL FAILED: %s does not answer held for the value one octet away, so "+
				"there is no near miss here and the row measures nothing", receiver.name)
		}
	}
	return page
}

// buildEarlierEpochReplay is a removal fanned out on an EARLIER epoch's pq_secret, after
// `rotations` honest rotations, replaying the value the receiver first held at `replaying`.
//
// IT TAKES THE REPLAYED EPOCH AS A PARAMETER, WHICH IS THE 2026-09-24 (EIGHTH PASS) REPAIR. It was
// written with both numbers fixed at one -- one rotation, epoch 1's value -- and so was every other
// refusing row in this table: `heldAt` was 1 everywhere but at the admin row, where it is 2. That
// left the exit's SECOND input driven over two points while its first was driven over five, and
// `alreadyHeld = alreadyHeld && heldAt < 3` (pqepoch.go sha256 1608f28c11c9) passed both
// instruments and the whole package. The shape it let through is ordinary: a committer replaying
// pq_secret[k] for k >= 3 in a group that has rotated a few times.
func buildEarlierEpochReplay(rotations int, replaying uint64) func(t *testing.T) *removalPage {
	return func(t *testing.T) *removalPage {
		world, committer, receiver, removing, atOne := removalCohort(t, 1)
		victim := world.member("victim0")
		// THE HONEST ROTATIONS THAT GIVE THE RECEIVER MORE THAN ONE ROW. At epoch 1 there is one
		// row, so "the current row" and "the whole table" are the same set and any replay is
		// invisible; the founding draw is epoch 1's and is not one of them.
		opened := world.rotateHonestly(committer, rotations, receiver, victim)
		opened[1] = atOne
		replayed, known := opened[replaying]
		if !known {
			t.Fatalf("this row replays epoch %d and %d honest rotation(s) opened epochs %v; a "+
				"replay of an epoch nobody opened would be a replay of nil", replaying, rotations,
				slices.Sorted(maps.Keys(opened)))
		}
		published := world.fanOutOnTheHeldSecret(committer, func() ([]byte, []byte, []byte, error) {
			return committer.handle.CommitRemove(removing)
		}, unrotatedFanOut{opensOn: replayed})
		page := &removalPage{world: world, committer: committer, receiver: receiver,
			page: published.page(), opens: published.opens, removes: removing,
			opensOn: published.pqSecret, retained: replayed}
		page.also = func(t *testing.T) {
			if want := rotations + 1; len(receiver.group.pqSecrets) != want {
				t.Fatalf("CONTROL FAILED: %s holds %d row(s) after %d rotation(s), want %d; with "+
					"one row this row measures the case above it", receiver.name,
					len(receiver.group.pqSecrets), rotations, want)
			}
			if bytes.Equal(receiver.group.pqSecretLocked(), replayed) {
				t.Fatalf("CONTROL FAILED: the group's CURRENT secret IS the replayed one, so " +
					"this is the unrotated shape and not a replay of a different octet string")
			}
			if !bytes.Equal(published.pqSecret, replayed) {
				t.Fatalf("CONTROL FAILED: the fixture opened epoch %d on something other than "+
					"epoch %d's secret", published.opens, replaying)
			}
			// AND THE ROW SITS WHERE IT SAYS ON THE SECOND AXIS. The refusal's own `heldAt` is
			// asserted in [removalPage.drive] against the row's declaration; this is the same
			// fact one step earlier, so a fixture whose rotations quietly re-drew the same octets
			// -- which would collapse every epoch onto heldAt 1 -- is caught before delivery
			// rather than as a confusing disagreement afterwards.
			if at, held := receiver.group.pqSecretHeldAtLocked(replayed); !held || at != replaying {
				t.Fatalf("CONTROL FAILED: %s first held the replayed value at epoch %d "+
					"(held=%v) and this row is built to replay epoch %d. Every rotation must "+
					"draw fresh octets or the epochs collapse onto one point", receiver.name,
					at, held, replaying)
			}
		}
		return page
	}
}

// buildLateJoinerResidual is ledger ruling 43's first residual, driven. One page, two receivers,
// and the difference between them is a HISTORY and nothing else.
func buildLateJoinerResidual(t *testing.T) *removalPage {
	world, committer, receiver, removing, atOne := removalCohort(t, 1)
	victim := world.member("victim0")
	first := world.rotate(committer, func() ([]byte, []byte, []byte, error) {
		return committer.handle.Commit(nil)
	})
	for _, member := range []*rotMember{receiver, victim} {
		if err := world.deliver(member, first.page()...); err != nil {
			t.Fatalf("CONTROL FAILED: %s's walk over an honest rotation answered %v", member.name, err)
		}
	}
	joiner, admission := world.admit(committer, "dave", receiver, victim)
	published := world.fanOutOnTheHeldSecret(committer, func() ([]byte, []byte, []byte, error) {
		return committer.handle.CommitRemove(removing)
	}, unrotatedFanOut{opensOn: atOne})
	page := &removalPage{world: world, committer: committer, receiver: receiver,
		page: published.page(), opens: published.opens, removes: removing,
		opensOn: published.pqSecret, retained: atOne}
	page.also = func(t *testing.T) {
		if len(joiner.group.pqSecrets) != 1 {
			t.Fatalf("CONTROL FAILED: the member admitted at epoch %d holds %d pq_secret row(s), "+
				"want 1. Device.Join files exactly one and this row is about that",
				admission.opens, len(joiner.group.pqSecrets))
		}
		if _, held := receiver.group.pqSecretHeldAtLocked(atOne); !held {
			t.Fatalf("CONTROL FAILED: the founder does not answer held for epoch 1's secret, so " +
				"the refusal below is not about a history")
		}
		if _, held := joiner.group.pqSecretHeldAtLocked(atOne); held {
			t.Fatalf("CONTROL FAILED: the late joiner answers HELD for a value drawn before it " +
				"was admitted, so the two receivers do not differ and this row measures nothing")
		}
		// AND THE CONTROL FIRES FOR ITS OWN REASON: the late joiner DOES recognise its own row.
		// Without this, "the joiner answers not held" would also be satisfied by a joiner whose
		// table is empty or whose lookup is broken.
		if _, held := joiner.group.pqSecretHeldAtLocked(admission.pqSecret); !held {
			t.Fatalf("CONTROL FAILED: the late joiner does not answer held for its OWN row, so " +
				"its `not held` above is a broken lookup and not a smaller history")
		}
	}
	page.then = func(t *testing.T) {
		// THE RESIDUAL ITSELF. The same page, the same removal, a receiver with a smaller
		// history: it is FOLLOWED, and nothing in this package can make it otherwise.
		if err := world.deliver(joiner, published.page()...); err != nil {
			t.Fatalf("the late joiner answered %v. It cannot refuse -- it has never held the "+
				"replayed value -- so a refusal here means this row is measuring something else", err)
		}
		if joiner.group.epoch != published.opens {
			t.Fatalf("the late joiner stands at epoch %d after following, want %d",
				joiner.group.epoch, published.opens)
		}
		if joiner.group.halted != nil {
			t.Fatalf("the late joiner halted: %v", joiner.group.halted)
		}
		t.Logf("RULING 43's FIRST RESIDUAL, DRIVEN: one page removing leaf %v on pq_secret[1]; "+
			"the founder REFUSED it and the member admitted at epoch %d FOLLOWED it. A receiver "+
			"cannot check a property about a history it does not have, and if every survivor had "+
			"joined after that epoch there would be no refuser at all",
			removing, admission.opens)
	}
	return page
}

func buildAdminCommitter(t *testing.T) *removalPage {
	world, owner, admin, removing, _ := removalCohort(t, 1)
	victim := world.member("victim0")
	world.promote(owner, admin, admin, victim)
	// THE VALUE THE VICTIM KEEPS IS READ AFTER THE PROMOTION AND NOT BEFORE IT. The promotion is
	// an honest rotation, so the group has moved to a second secret and the removal below is
	// fanned out on THAT one; a counterfactual built on the founding value would be asking about
	// a row nobody is replaying.
	retained := append([]byte(nil), victim.group.pqSecretLocked()...)
	// THE COMMITTER IS THE PROMOTED ADMIN AND THE RECEIVER IS THE OWNER, so this row is the only
	// one in the table whose commit was not written by the group's founder.
	published := world.fanOutOnTheHeldSecret(admin, func() ([]byte, []byte, []byte, error) {
		return admin.handle.CommitRemove(removing)
	}, unrotatedFanOut{})
	page := &removalPage{world: world, committer: admin, receiver: owner,
		page: published.page(), opens: published.opens, removes: removing,
		opensOn: published.pqSecret, retained: retained}
	page.also = func(t *testing.T) {
		// THE COMMITTER REALLY IS AN ADMIN, read off the receiver's own policy. Without it a
		// promotion that silently did nothing would make this a copy of the first row.
		extensions, err := owner.group.contextExtensionsLocked()
		if err != nil {
			t.Fatalf("the owner's group context: %v", err)
		}
		policy, err := mls.GroupPolicyOf(mlsExtensionsOf(extensions))
		if err != nil {
			t.Fatalf("the owner's policy: %v", err)
		}
		if role, named := policy.RoleOf(admin.dev.identityPub); !named || role != mls.RoleAdmin {
			t.Fatalf("CONTROL FAILED: the committer is named %v (named=%v) in the policy the "+
				"receiver holds, want ADMIN", role, named)
		}
		// AND THE VICTIM HAS NO SECOND DEVICE TO COMMIT WITH, MEASURED RATHER THAN ASSERTED.
		// A removal committed by the removed member's OWN second device is not a shape this
		// build can put on the wire: credential identity IS the device signer (ledger item 242's
		// M7), so an identity holds exactly one leaf, and mls refuses a second leaf under one
		// signature key by name. The control is in the same call: the refusal is the mls error
		// and not some other failure.
		keyPackage, err := victim.dev.engine.NewKeyPackage()
		if err != nil {
			t.Fatalf("the victim's key package: %v", err)
		}
		if _, _, _, err := owner.handle.CommitAdd([][]byte{keyPackage}); !errors.Is(err, mls.ErrAddDuplicateSignatureKey) {
			t.Fatalf("a second leaf under the victim's own identity was accepted with %v. If this "+
				"build now admits one, a removal committed by the victim's own second device "+
				"becomes constructible and this table owes it a row", err)
		}
		owner.handle.ClearPendingCommit()
	}
	return page
}

func buildBundledRemoval(honest bool) func(t *testing.T) *removalPage {
	return func(t *testing.T) *removalPage {
		world, committer, receiver, removing, retained := removalCohort(t, 1)
		victim := world.member("victim0")
		arm, joining := world.bundleAddAndRemove(committer, removing[0], "dave", receiver, victim)
		published := (*rotation)(nil)
		if honest {
			published = world.rotate(committer, arm)
		} else {
			published = world.fanOutOnTheHeldSecret(committer, arm, unrotatedFanOut{})
		}
		page := &removalPage{world: world, committer: committer, receiver: receiver,
			page: published.page(), opens: published.opens, removes: removing,
			opensOn: published.pqSecret, retained: retained}
		page.also = func(t *testing.T) {
			// THE COMMIT REALLY DOES BOTH, read off the committer's own post-commit tree. A
			// commit that quietly dropped one of its two proposals would make this row a slower
			// copy of the plain removal, and the disposition would not show it.
			if !world.holdsIdentity(committer, joining.identityPub) {
				t.Fatalf("CONTROL FAILED: the committer's tree does not hold the ADDED identity " +
					"after the commit, so this row is not a bundle")
			}
			if world.holdsIdentity(committer, victim.dev.identityPub) {
				t.Fatalf("CONTROL FAILED: the committer's tree still holds the REMOVED identity " +
					"after the commit, so this row is not a removal")
			}
		}
		return page
	}
}

func buildNoDigestRemoval(victims int) func(t *testing.T) *removalPage {
	return buildNoDigestRemovalAfter(0, victims)
}

// buildNoDigestRemovalAfter is the digest-less removal with `rotations` honest rotations in front
// of it, which is the only knob that moves `self.epoch` -- [Group.refuseUnrotatedRemovalLocked]'s
// THIRD input and the one this table drove at exactly one point until the eighth pass.
//
// WHAT THAT ONE POINT COST, measured rather than asserted: a `println` at the top of that door,
// run over this table, reported `self.epoch == 1` on every digest-less call of it -- one-leaf,
// two-leaves, three-leaves, four-leaves, one-leaf-and-one-add and no-removal, six rows and one
// epoch. So `1 < self.epoch -> return nil` passed at every arity, in three spellings: a top-level
// `if` (pqepoch.go sha256 aa6068572817), an `else if` welded onto `digest != nil` (68d2fbf1c38c)
// and a `switch` (bdd81e69ac7f). The last two also passed every gate in pqdarkgate_test.go.
func buildNoDigestRemovalAfter(rotations int, victims int) func(t *testing.T) *removalPage {
	return func(t *testing.T) *removalPage {
		world, committer, receiver, removing, retained := removalCohort(t, victims)
		walkers := append([]*rotMember{receiver}, removalVictims(world, victims)...)
		world.rotateHonestly(committer, rotations, walkers...)
		if 0 < victims {
			retained = append([]byte(nil), world.member("victim0").group.pqSecretLocked()...)
		}
		published := world.fanOutOnTheHeldSecret(committer, func() ([]byte, []byte, []byte, error) {
			return committer.handle.CommitRemove(removing)
		}, unrotatedFanOut{noDigest: true})
		page := &removalPage{world: world, committer: committer, receiver: receiver,
			page: published.page(), opens: published.opens, removes: removing,
			opensOn: published.pqSecret, retained: retained}
		page.also = func(t *testing.T) {
			// THE COMMIT REALLY CARRIES NO DIGEST, read off the record this page delivers. The
			// door's whole predicate is `digest != nil`, so a fixture that quietly attached one
			// would make this row a slow copy of a digest-door row -- at the wrong door, and
			// with its `self.epoch` declaration on an interval nothing drives.
			digest, err := epochDigestOf(&published.commit.record.Header)
			if err != nil {
				t.Fatalf("reading this row's commit for a digest: %v", err)
			}
			if digest != nil {
				t.Fatalf("CONTROL FAILED: this row's commit carries an epoch digest for epoch "+
					"%d, so it does not reach the pre-apply door at all", digest.Epoch)
			}
		}
		return page
	}
}

func buildNoDigestBundle(t *testing.T) *removalPage {
	world, committer, receiver, removing, retained := removalCohort(t, 1)
	victim := world.member("victim0")
	arm, _ := world.bundleAddAndRemove(committer, removing[0], "dave", receiver, victim)
	published := world.fanOutOnTheHeldSecret(committer, arm, unrotatedFanOut{noDigest: true})
	return &removalPage{world: world, committer: committer, receiver: receiver,
		page: published.page(), opens: published.opens, removes: removing,
		opensOn: published.pqSecret, retained: retained}
}

// buildNoDigestNoRemoval is the pre-apply door's complement at a chosen `self.epoch`: a
// digest-less commit that removes NOBODY, after `rotations` honest rotations. It carries the same
// parameter as the refusing builder because every point of the epoch axis needs both dispositions
// -- a refused row at an epoch with no followed row beside it is a refusal of the EPOCH.
func buildNoDigestNoRemoval(rotations int) func(t *testing.T) *removalPage {
	return func(t *testing.T) *removalPage {
		world, committer, receiver, _, _ := removalCohort(t, 0)
		world.rotateHonestly(committer, rotations, receiver)
		published := world.fanOutOnTheHeldSecret(committer, func() ([]byte, []byte, []byte, error) {
			return committer.handle.Commit(nil)
		}, unrotatedFanOut{noDigest: true})
		return &removalPage{world: world, committer: committer, receiver: receiver,
			page: published.page(), opens: published.opens, removes: nil,
			opensOn: published.pqSecret}
	}
}
