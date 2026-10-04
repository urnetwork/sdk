package cp3b

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect/message"
	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/connect/mls"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/sdk/urmessage"
)

// THE ROLE MODEL OVER A RUNNING SERVER, BOTH ARMS (MASTER §11, ledger item 242's R1 and R2). Three
// real devices and one member that is not a device at all:
//
//   - THE COMMITTING ARM, through the production verbs: the owner promotes B with SetRole and every
//     device's Members() agrees; B, now an admin, adds C; C, a MEMBER, asks AddMemberAndPublish for
//     D and is refused ON THE SEND SIDE -- ErrCommitUnauthorized wrapping ErrCommitAddByNonAdmin,
//     Stats.CommitRefusedOwn one, and NO EPOCH CHANGE ANYWHERE: not at C, not at A or B after they
//     fetch, and not at the server, whose current_epoch is read off the store. C's SetRole and
//     TransferOwnership are refused the same way. Then the owner transfers to B: B's MyRole is owner
//     and the old owner's is admin at every device, the old owner may no longer touch the admin set
//     (refused on send), and B may now promote C.
//   - THE RECEIVING ARM, last, because what it costs is the group: a member that is NOT an
//     urmessage device -- the shipped engine and a session driven by hand through the seam, every
//     key an honest member holds and none of the verbs -- builds a MEMBER's Add of D, seals the
//     commit record itself and submits it. The server takes it and moves on; every honest device
//     refuses it on receipt with the same rule, stays at the epoch before, and counts one refusal.
//     A hostile committer can halt a group and cannot take it, and this is what the halt looks like
//     through the public surface. It is a seam actor and not C's own device because a device's
//     handle is not reachable through the public API, by design; the send-side refusal at C is the
//     honest-client half and this is the other.
//
// EXPORTER AGREEMENT AT EVERY EPOCH is asserted the one way the public surface allows: at each
// epoch every member sends a line and every other member opens it, which a shared storage root is
// the only way to do. A pair that disagreed about an exporter would fail at the first AEAD tag.
//
// WHAT WOULD GO RED: remove the send-side call in AddMemberAndPublish and C's add goes through --
// C at epoch four, the server at four, and A and B refusing on receipt -- so the "no epoch change
// anywhere" block fails first. Leave the ex-owner a member in TransferOwnership and every receiver
// refuses the transfer (R5). Let SetRole take "owner" and the transfer control here is a second
// road to the same policy.
func TestRolesConvergeAcrossThreeDevices(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()

	alice, _, _ := world.device(t, "alice")
	bob, _, _ := world.device(t, "bob")
	carol, _, _ := world.device(t, "carol")
	dave, _, _ := world.device(t, "dave")
	for _, who := range []*urmessage.Device{alice, bob, carol, dave} {
		if err := who.Connect(ctx); err != nil {
			t.Fatalf("Connect: %v", err)
		}
	}
	daveKeyPackage, err := dave.KeyPackage()
	if err != nil {
		t.Fatalf("dave's KeyPackage: %v", err)
	}

	// ── epoch 1: alice founds with bob ──────────────────────────────────────────────────────────
	groupId := newGroupId(t)
	aliceGroup, err := alice.CreateGroup(ctx, groupId)
	if err != nil {
		t.Fatalf("alice's CreateGroup: %v", err)
	}
	bobKeyPackage, err := bob.KeyPackage()
	if err != nil {
		t.Fatalf("bob's KeyPackage: %v", err)
	}
	invite, err := aliceGroup.AddMember(bobKeyPackage)
	if err != nil {
		t.Fatalf("alice's AddMember: %v", err)
	}
	if err := aliceGroup.Open(ctx); err != nil {
		t.Fatalf("alice's Open: %v", err)
	}
	bobGroup, err := bob.Join(ctx, gcReencodeInvite(t, invite))
	if err != nil {
		t.Fatalf("bob's Join: %v", err)
	}
	groups := map[string]*urmessage.Group{"alice": aliceGroup, "bob": bobGroup}
	aliceId, bobId := rolesIdentityOf(t, aliceGroup), rolesIdentityOf(t, bobGroup)
	identities := map[string][]byte{"alice": aliceId, "bob": bobId}
	rolesAtOne := map[string]string{"alice": "owner", "bob": "member"}
	rolesAssertRoster(t, 1, groups, identities, rolesAtOne)
	rolesAssertMesh(t, ctx, 1, groups, rolesAtOne)

	// ── epoch 2: the owner promotes bob, through the verb ───────────────────────────────────────
	if err := aliceGroup.SetRole(ctx, bobId, "admin"); err != nil {
		t.Fatalf("alice's SetRole promoting bob: %v", err)
	}
	if aliceGroup.Epoch() != 2 {
		t.Fatalf("alice is at epoch %d after her promotion commit, want 2", aliceGroup.Epoch())
	}
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 2, groups)
	rolesAtTwo := map[string]string{"alice": "owner", "bob": "admin"}
	rolesAssertRoster(t, 2, groups, identities, rolesAtTwo)
	if role, err := bobGroup.MyRole(); err != nil || role != "admin" {
		t.Fatalf("bob's MyRole after the promotion is %q, %v; want admin", role, err)
	}
	rolesAssertMesh(t, ctx, 2, groups, rolesAtTwo)

	// ── epoch 3: bob, an admin, adds carol ──────────────────────────────────────────────────────
	carolKeyPackage, err := carol.KeyPackage()
	if err != nil {
		t.Fatalf("carol's KeyPackage: %v", err)
	}
	invite, err = bobGroup.AddMemberAndPublish(ctx, carolKeyPackage)
	if err != nil {
		t.Fatalf("bob's AddMemberAndPublish of carol, as an admin: %v", err)
	}
	carolGroup, err := carol.Join(ctx, gcReencodeInvite(t, invite))
	if err != nil {
		t.Fatalf("carol's Join: %v", err)
	}
	groups["carol"] = carolGroup
	carolId := rolesIdentityOf(t, carolGroup)
	identities["carol"] = carolId
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 3, groups)
	rolesAtThree := map[string]string{"alice": "owner", "bob": "admin", "carol": "member"}
	rolesAssertRoster(t, 3, groups, identities, rolesAtThree)
	rolesAssertMesh(t, ctx, 3, groups, rolesAtThree)

	// ── carol, a MEMBER, is refused on the send side, and nothing moves anywhere ────────────────
	serverEpoch := func() uint64 {
		state, err := world.store.GroupState(ctx, groupId)
		if err != nil {
			t.Fatalf("the server's group state: %v", err)
		}
		return state.CurrentEpoch
	}
	if got := serverEpoch(); got != 3 {
		t.Fatalf("the server is at epoch %d before carol's refused add, want 3", got)
	}
	_, err = carolGroup.AddMemberAndPublish(ctx, daveKeyPackage)
	if !errors.Is(err, urmessage.ErrCommitUnauthorized) || !errors.Is(err, urmessage.ErrCommitAddByNonAdmin) {
		t.Fatalf("carol's AddMemberAndPublish as a member answered %v, want ErrCommitUnauthorized wrapping ErrCommitAddByNonAdmin", err)
	}
	// ruling 15: SetRole is an admin's or the owner's verb, and a MEMBER calling it is refused
	// before the predicate runs, with R4's own sentence -- whatever it asked for
	if err := carolGroup.SetRole(ctx, bobId, "member"); !errors.Is(err, urmessage.ErrCommitPolicyChangeByNonAdmin) {
		t.Errorf("carol's SetRole demoting bob answered %v, want R4's ErrCommitPolicyChangeByNonAdmin", err)
	}
	if err := carolGroup.TransferOwnership(ctx, carolId); !errors.Is(err, urmessage.ErrCommitOwnerTransfer) {
		t.Errorf("carol's TransferOwnership to herself answered %v, want R5's ErrCommitOwnerTransfer", err)
	}
	if got := carolGroup.Stats().CommitRefusedOwn; got != 3 {
		t.Errorf("carol's Stats.CommitRefusedOwn is %d after three send-side refusals, want 3", got)
	}
	// NO EPOCH CHANGE ANYWHERE: carol herself, the server, and the others after a fetch that has
	// nothing to ingest
	if carolGroup.Epoch() != 3 {
		t.Errorf("carol is at epoch %d after her refused verbs, want 3: something was built", carolGroup.Epoch())
	}
	if got := serverEpoch(); got != 3 {
		t.Errorf("the server is at epoch %d after carol's refused verbs, want 3: something was published", got)
	}
	ingestedBefore := map[string]uint64{}
	for name, group := range groups {
		ingestedBefore[name] = group.Stats().Ingested
	}
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 3, groups)
	for name, group := range groups {
		if got := group.Stats().Ingested; got != ingestedBefore[name] {
			t.Errorf("%s ingested a commit after carol's refused verbs (%d -> %d)", name, ingestedBefore[name], got)
		}
		if got := group.Stats().CommitRefused; got != 0 {
			t.Errorf("%s counted %d receiving-side refusal(s) after a send-side one; nothing should have reached the wire", name, got)
		}
	}
	rolesAssertRoster(t, 3, groups, identities, map[string]string{"alice": "owner", "bob": "admin", "carol": "member"})

	// ── epoch 4: the owner transfers to bob ─────────────────────────────────────────────────────
	if err := aliceGroup.TransferOwnership(ctx, bobId); err != nil {
		t.Fatalf("alice's TransferOwnership to bob: %v", err)
	}
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 4, groups)
	rolesAtFour := map[string]string{"alice": "admin", "bob": "owner", "carol": "member"}
	rolesAssertRoster(t, 4, groups, identities, rolesAtFour)
	if role, _ := bobGroup.MyRole(); role != "owner" {
		t.Errorf("bob's MyRole after the transfer is %q, want owner", role)
	}
	if role, _ := aliceGroup.MyRole(); role != "admin" {
		t.Errorf("alice's MyRole after the transfer is %q, want admin", role)
	}
	rolesAssertMesh(t, ctx, 4, groups, rolesAtFour)
	// the old owner, now an admin, may no longer touch the admin set or the ownership
	if err := aliceGroup.SetRole(ctx, carolId, "admin"); !errors.Is(err, urmessage.ErrCommitRoleChangeByNonOwner) {
		t.Errorf("the old owner's SetRole promoting carol answered %v, want R4's refusal", err)
	}
	if err := aliceGroup.TransferOwnership(ctx, aliceId); !errors.Is(err, urmessage.ErrCommitOwnerTransfer) {
		t.Errorf("the old owner's TransferOwnership back to herself answered %v, want R5's refusal", err)
	}
	if got := serverEpoch(); got != 4 {
		t.Errorf("the server is at epoch %d after the old owner's refused verbs, want 4", got)
	}

	// ── epoch 5: bob, now the owner, promotes carol ─────────────────────────────────────────────
	if err := bobGroup.SetRole(ctx, carolId, "admin"); err != nil {
		t.Fatalf("bob's SetRole promoting carol, as the new owner: %v", err)
	}
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 5, groups)
	rolesAtFive := map[string]string{"alice": "admin", "bob": "owner", "carol": "admin"}
	rolesAssertRoster(t, 5, groups, identities, rolesAtFive)
	if role, _ := carolGroup.MyRole(); role != "admin" {
		t.Errorf("carol's MyRole after her promotion is %q, want admin", role)
	}
	rolesAssertMesh(t, ctx, 5, groups, rolesAtFive)

	// ── epoch 6: bob adds mallory, a member that is the seam and a session and no device ────────
	mallory := world.seamMember(t, ctx, "mallory")
	invite, err = bobGroup.AddMemberAndPublish(ctx, mallory.keyPackage(t))
	if err != nil {
		t.Fatalf("bob's AddMemberAndPublish of mallory: %v", err)
	}
	mallory.join(t, gcReencodeInvite(t, invite))
	identities["mallory"] = mallory.identityPub
	rolesReceiveAll(t, ctx, groups)
	rolesAssertEpoch(t, 6, groups)
	if mallory.handle.Epoch() != 6 {
		t.Fatalf("mallory joined at epoch %d, want 6", mallory.handle.Epoch())
	}
	rosterAtSix := map[string]string{"alice": "admin", "bob": "owner", "carol": "admin", "mallory": "member"}
	rolesAssertRoster(t, 6, groups, identities, rosterAtSix)
	rolesAssertMesh(t, ctx, 6, groups, rosterAtSix)
	ingestedAtSix := map[string]uint64{}
	for name, group := range groups {
		if got := group.Stats().CommitRefused; got != 0 {
			t.Fatalf("%s has already refused %d commit(s) before the hostile one", name, got)
		}
		ingestedAtSix[name] = group.Stats().Ingested
	}

	// ── the hostile commit: a MEMBER's Add of dave, built and published by hand ─────────────────
	mallory.commitAddAndSubmit(t, ctx, daveKeyPackage)
	if got := serverEpoch(); got != 7 {
		t.Fatalf("the server is at epoch %d after mallory's commit, want 7: the server takes what it is handed", got)
	}
	for name, group := range groups {
		_, err := group.Receive(ctx)
		if !errors.Is(err, urmessage.ErrCommitUnauthorized) || !errors.Is(err, urmessage.ErrCommitAddByNonAdmin) {
			t.Errorf("%s's Receive over mallory's commit answered %v, want ErrCommitUnauthorized wrapping ErrCommitAddByNonAdmin", name, err)
		}
		if group.Epoch() != 6 {
			t.Errorf("%s followed mallory's commit to epoch %d; an honest receiver stays at 6", name, group.Epoch())
		}
		stats := group.Stats()
		if stats.CommitRefused != 1 {
			t.Errorf("%s's Stats.CommitRefused is %d after the hostile commit, want 1", name, stats.CommitRefused)
		}
		if stats.Ingested != ingestedAtSix[name] {
			t.Errorf("%s's Stats.Ingested moved from %d to %d over a refused commit", name, ingestedAtSix[name], stats.Ingested)
		}
	}
	rolesAssertRoster(t, 6, groups, identities, rosterAtSix)

	// ── AND THE HISTORY STILL SAYS WHO EVERYBODY WAS WHEN THEY SAID IT (item 242's R4, ruling 21) ──
	//
	// Every role in this group has changed since epoch one: alice owner -> admin, bob member ->
	// admin -> owner, carol member -> admin. The lines each of them sent at each epoch still carry
	// the role they held THEN, at every device, read back off the log rather than off a Receive.
	// A build that derived the role at render time, or off the current roster, answers today's
	// role for every line and fails on every epoch but the last.
	byEpoch := map[uint64]map[string]string{
		1: rolesAtOne, 2: rolesAtTwo, 3: rolesAtThree, 4: rolesAtFour, 5: rolesAtFive, 6: rosterAtSix,
	}
	for name, group := range groups {
		rolesAssertHistoryKeepsItsRoles(t, name, group, byEpoch)
		if got := group.Stats().RoleUndeterminable; got != 0 {
			t.Errorf("%s opened %d record(s) whose sender's role it could not read", name, got)
		}
		if got := group.Stats().HiddenObserver; got != 0 {
			t.Errorf("%s hid %d observer message(s) in a group that has no observer", name, got)
		}
	}
	t.Logf("roles: five epochs through the verbs with every roster and every exporter agreeing; carol's three send-side refusals moved nothing; mallory's hand-built add moved the server to 7 and every honest device refused it at 6")
}

// rolesIdentityOf reads a device's own identity off its group's roster, through Mine.
func rolesIdentityOf(t *testing.T, group *urmessage.Group) []byte {
	t.Helper()
	members, err := group.Members()
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	for _, member := range members {
		if member.Mine {
			return member.IdentityPub
		}
	}
	t.Fatal("the roster marks no leaf as this device's own")
	return nil
}

// rolesReceiveAll fetches on every group until it has caught up, failing on any refusal.
//
// IT DRAINS RATHER THAN RECEIVING ONCE, AND THAT IS F0's EPOCH CEILING AND NOT A RETRY LOOP.
//
// Under item 246's ceiling a fetch is bounded by the `read_epoch` its own `req_auth` was computed
// under, so a device two commits behind is served the FIRST of them and nothing above it: it walks
// forward ONE EPOCH PER ROUND TRIP, by design, which msgrepo holds end to end in
// `TestAMemberSeveralEpochsBehindWalksForwardOneEpochPerRoundTrip`. This helper called Receive once
// per group, which was enough before the ceiling landed and is not enough after it -- and what
// that produces is not "a record was lost" but "carol is at epoch 6, want 7": a whole epoch
// behind, with a nil error, which reads like a product defect and is a helper that stopped early.
//
// THE LOOP IS BOUNDED AND THE BOUND IS A FAILURE, not a break. A group that will not settle is a
// real defect and must not be smoothed over by a drain that gives up quietly, so the cap is a
// t.Fatalf naming the group and the epoch it stalled at.
func rolesReceiveAll(t *testing.T, ctx context.Context, groups map[string]*urmessage.Group) {
	t.Helper()
	// one more than the deepest epoch walk in this file: every case here advances a handful of
	// epochs, and a group needing more than this has stopped converging.
	const rounds = 16
	for name, group := range groups {
		settled := false
		for at := 0; at < rounds && !settled; at += 1 {
			before := group.Epoch()
			got, err := group.Receive(ctx)
			if err != nil {
				t.Fatalf("%s's Receive: %v", name, err)
			}
			// nothing new and no epoch crossed: this device is at its ceiling's head, which
			// under F0 is the only "caught up" a reader can observe for itself.
			settled = len(got) == 0 && group.Epoch() == before
		}
		if !settled {
			t.Fatalf("%s did not settle after %d Receive rounds, stalled at epoch %d",
				name, rounds, group.Epoch())
		}
	}
}

// rolesAssertEpoch holds every group at one epoch.
func rolesAssertEpoch(t *testing.T, epoch uint64, groups map[string]*urmessage.Group) {
	t.Helper()
	for name, group := range groups {
		if group.Epoch() != epoch {
			t.Fatalf("%s is at epoch %d, want %d", name, group.Epoch(), epoch)
		}
	}
}

// rolesAssertRoster holds every group's Members() to the wanted roles, keyed by the name of the
// member whose identity the entry carries, and to each other: the same identities, the same roles,
// with exactly one leaf marked Mine at every reader.
func rolesAssertRoster(t *testing.T, epoch uint64, groups map[string]*urmessage.Group,
	identities map[string][]byte, want map[string]string) {

	t.Helper()
	for reader, group := range groups {
		members, err := group.Members()
		if err != nil {
			t.Fatalf("%s's Members at epoch %d: %v", reader, epoch, err)
		}
		if len(members) != len(want) {
			t.Errorf("%s reads %d members at epoch %d, want %d", reader, len(members), epoch, len(want))
		}
		mine := 0
		for _, member := range members {
			if member.Mine {
				mine += 1
			}
			named := ""
			for name, identity := range identities {
				if bytes.Equal(identity, member.IdentityPub) {
					named = name
				}
			}
			if named == "" {
				t.Errorf("%s reads a member %x at epoch %d that is nobody's identity", reader, member.IdentityPub, epoch)
				continue
			}
			if member.Role != want[named] {
				t.Errorf("%s reads %s as %q at epoch %d, want %q", reader, named, member.Role, epoch, want[named])
			}
		}
		if mine != 1 {
			t.Errorf("%s marks %d leaves as its own at epoch %d, want 1", reader, mine, epoch)
		}
	}
}

// rolesAssertMesh has every member send one line at the epoch and every other member open it: the
// public surface's proof that every pair shares the epoch's storage root.
//
// AND SINCE R4 IT IS ALSO WHERE SenderRoleAtSend IS MEASURED OVER A RUNNING SERVER, through
// [urmessage.Group.Send] and [urmessage.Group.Receive] and nothing below them. `roles` is the same
// map [rolesAssertRoster] has just held every roster to, so what is asserted is that the role
// stamped on a line at epoch n is the role its sender held AT EPOCH n -- at the sender, which
// captures it at the seal, and at every receiver, which captures it at the open. The two are
// different code paths reading different trees and they must answer one value.
//
// IT IS NOT A FIXED VALUE ACROSS THE RUN, which is what makes it a measurement: this case moves
// alice owner -> admin, bob member -> admin -> owner and carol member -> admin, and a line from
// each epoch keeps the role of THAT epoch afterwards.
func rolesAssertMesh(t *testing.T, ctx context.Context, epoch uint64, groups map[string]*urmessage.Group,
	roles map[string]string) {

	t.Helper()
	for sender, group := range groups {
		line := fmt.Sprintf("%s at epoch %d", sender, epoch)
		sent, err := group.Send(ctx, line)
		if err != nil {
			t.Fatalf("%s's Send at epoch %d: %v", sender, epoch, err)
		}
		want, named := roles[sender]
		if !named {
			t.Fatalf("this case did not say what role %s holds at epoch %d", sender, epoch)
		}
		if sent.SenderRoleAtSend != want {
			t.Errorf("%s sent at epoch %d and stamped its own line %q, want %q",
				sender, epoch, sent.SenderRoleAtSend, want)
		}
		for receiver, other := range groups {
			if receiver == sender {
				continue
			}
			got := gcReceiveTextMessage(t, ctx, receiver, other, line)
			if got.SenderRoleAtSend != want {
				t.Errorf("%s reads %s's epoch-%d line as sent by a %q, want %q",
					receiver, sender, epoch, got.SenderRoleAtSend, want)
			}
		}
	}
}

// rolesAssertHistoryKeepsItsRoles re-reads a member's whole log and holds every line to the role
// its sender held AT THE EPOCH IT WAS SENT, after every role in the group has changed since.
//
// THIS IS RULING 21 OVER A RUNNING SERVER. The lines are named "<who> at epoch <n>", so the log
// itself says what each one's answer must be; a build that derived the role at render time, or off
// the current roster, answers today's role for every line and fails on every epoch but the last.
func rolesAssertHistoryKeepsItsRoles(t *testing.T, who string, group *urmessage.Group,
	byEpoch map[uint64]map[string]string) {

	t.Helper()
	checked := 0
	for _, held := range group.Messages() {
		var sender string
		var epoch uint64
		if _, err := fmt.Sscanf(held.Text, "%s at epoch %d", &sender, &epoch); err != nil {
			continue
		}
		want, named := byEpoch[epoch][sender]
		if !named {
			continue
		}
		checked += 1
		if held.SenderRoleAtSend != want {
			t.Errorf("%s's log reads %q as sent by a %q, want %q -- a role is a fact about the epoch "+
				"it was sent at and not about now", who, held.Text, held.SenderRoleAtSend, want)
		}
	}
	if checked < 4 {
		t.Fatalf("%s's log matched %d line(s) against a known role; this control examined nothing", who, checked)
	}
	t.Logf("%s: %d line(s) across five epochs still carry the role their sender held then", who, checked)
}

// ── a member that is the seam and a session, and no device ───────────────────────────────────

// seamMember is a member of a group that is NOT an urmessage.Device: the shipped engine over the
// shipped store, a real GroupSession over a real transport with its own Hello, and no verb in
// front of any of it. It holds every key an honest member holds and it skips the send-side check,
// which is what a hostile build is.
type seamMember struct {
	name        string
	transport   *sdk.MessageTransport
	reserver    messagegroup.StreamIndexReserver
	engine      messagegroup.GroupEngine
	identityPub []byte
	handle      messagegroup.GroupHandle
	session     *messagegroup.GroupSession
	groupId     []byte
	pqSecret    []byte
}

// seamMember stands one up and says Hello through its own transport.
func (self *world) seamMember(t *testing.T, ctx context.Context, name string) *seamMember {
	t.Helper()
	return self.seamMemberClaiming(t, ctx, name, nil)
}

// seamMemberClaiming is [world.seamMember] whose CREDENTIAL carries an identity that is not the one
// it signs with, which is the only way this module can put TWO LEAVES OF ONE IDENTITY in a group: an
// urmessage.Device mints one identity per state store and there is no door onto a second leaf for it.
//
// IT IS NOT A FORGERY WHEN THAT IDENTITY COMMITS ITS OWN ADD. MASTER section 11's self-service rule
// gives every member its own device leaves, and R6a requires an Add claiming an identity already in
// the group to be committed BY that identity -- so a claiming key package added by the real device
// that holds the identity is a second DEVICE of that member and is followed by every honest
// receiver, while the same package added by anybody else is refused
// (TestAnAddClaimingTheOwnersIdentityByAnAdminIsRefused, one module over). `claimed` nil is
// [world.seamMember]: a member whose credential is its own signer.
func (self *world) seamMemberClaiming(t *testing.T, ctx context.Context, name string,
	claimed []byte) *seamMember {

	t.Helper()
	client := self.connectClient(t)
	transport := self.transport(t, client)
	streamStore, err := sdk.OpenStreamStore(t.TempDir())
	if err != nil {
		t.Fatalf("%s: sdk.OpenStreamStore: %v", name, err)
	}
	t.Cleanup(func() { streamStore.Close() })
	stateStore, err := urmessage.OpenDurableStateStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("%s: OpenDurableStateStore: %v", name, err)
	}
	t.Cleanup(func() { stateStore.Close() })
	crypto, err := mls.NewCryptoProvider(mls.CipherSuiteX25519ChaCha20Sha256Ed25519)
	if err != nil {
		t.Fatalf("%s: the crypto provider: %v", name, err)
	}
	signer, signerPub, err := crypto.SignatureKeyPair()
	if err != nil {
		t.Fatalf("%s: the signature key pair: %v", name, err)
	}
	xwing, err := messagegroup.XwingGenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("%s: the x-wing key: %v", name, err)
	}
	leafKeys, err := (&mls.LeafKeysExtension{AlgId: mls.AlgIdXwing, DeviceXwingPub: xwing.Public().Bytes()}).Encode()
	if err != nil {
		t.Fatalf("%s: the leaf keys extension: %v", name, err)
	}
	credential := signerPub
	if claimed != nil {
		credential = append([]byte(nil), claimed...)
	}
	engine, err := messagegroup.NewConnectMlsEngine(crypto, stateStore, signer, mls.BasicCredential(credential), leafKeys.ExtensionData)
	if err != nil {
		t.Fatalf("%s: the engine: %v", name, err)
	}
	reason, hello, err := transport.Hello(ctx)
	if err != nil || reason != protocol.Reason_REASON_OK || len(hello.GetServerNonce()) == 0 {
		t.Fatalf("%s's Hello: %v %v", name, reason, err)
	}
	return &seamMember{
		name:      name,
		transport: transport,
		reserver:  sdk.NewStreamIndexReserver(streamStore),
		engine:    engine,
		// the identity this member is KNOWN BY, which is its credential's and not its signer's:
		// the roster, the policy and every role decision are keyed on the credential identity.
		identityPub: append([]byte(nil), credential...),
	}
}

// keyPackage is the package an honest admin adds this member with.
func (self *seamMember) keyPackage(t *testing.T) []byte {
	t.Helper()
	keyPackage, err := self.engine.NewKeyPackage()
	if err != nil {
		t.Fatalf("%s's key package: %v", self.name, err)
	}
	return keyPackage
}

// join takes the invite as urmessage.Device.Join would: the Welcome through the seam, and a session
// over the handle at the epoch it joined, bound to this member's own nonce.
func (self *seamMember) join(t *testing.T, invite *urmessage.Invite) {
	t.Helper()
	handle, err := self.engine.JoinFromWelcome(invite.Welcome, invite.RatchetTree)
	if err != nil {
		t.Fatalf("%s's JoinFromWelcome: %v", self.name, err)
	}
	session, err := messagegroup.NewGroupSession(handle, invite.PqSecret, invite.GroupHandleKey,
		self.reserver, func() int64 { return time.Now().UnixMilli() }, self.transport.Nonce())
	if err != nil {
		handle.Close()
		t.Fatalf("%s's session: %v", self.name, err)
	}
	t.Cleanup(func() { session.Close() })
	self.handle, self.session = handle, session
	self.groupId = append([]byte(nil), invite.GroupId...)
	self.pqSecret = append([]byte(nil), invite.PqSecret...)
}

// commitAddAndSubmit builds an Add of one key package through the seam's by-value arm, merges it,
// seals the commit record at the epoch that is closing with the attachment that opens the next --
// the (1) to (3) of urmessage's own publish path, spelled here from the public packages -- and
// submits it. The wrap set and the marker are not published: the honest receivers refuse the
// commit before either matters, and the halt they are left in is the same either way.
func (self *seamMember) commitAddAndSubmit(t *testing.T, ctx context.Context, keyPackage []byte) {
	t.Helper()
	commit, _, _, err := self.handle.CommitAdd([][]byte{keyPackage})
	if err != nil {
		t.Fatalf("%s's CommitAdd: %v", self.name, err)
	}
	if err := self.handle.MergePendingCommit(); err != nil {
		t.Fatalf("%s's MergePendingCommit: %v", self.name, err)
	}
	newEpoch := self.handle.Epoch()
	newMlsSecret, err := self.handle.Export("URmessage/v1/storage", nil, 32)
	if err != nil {
		t.Fatalf("%s's exporter: %v", self.name, err)
	}
	newRoot := messagegroup.StorageRoot(newMlsSecret, self.pqSecret)
	groupContext, err := self.handle.GroupContextBytes()
	if err != nil {
		t.Fatalf("%s's group context: %v", self.name, err)
	}
	contextHash := sha256.Sum256(groupContext)
	head := make([]byte, 9)
	head[0] = 0x02
	binary.BigEndian.PutUint64(head[1:], uint64(time.Now().UnixMilli()))
	record, err := self.session.SealRecord(message.RetentionPermanent, 0, true, head, commit, 0,
		&message.ServerAttachment{
			Kind: message.AttachmentEpoch,
			Epoch: &message.EpochAttachment{
				Epoch:             newEpoch,
				AlgId:             0x0031,
				WriteKey:          message.WriteKey(newRoot),
				ReadKey:           message.ReadKey(newRoot),
				GroupContextHash:  contextHash[:],
				ExpectedWrapCount: uint32(self.handle.MemberCount()),
			},
		})
	if err != nil {
		t.Fatalf("%s sealing its commit record: %v", self.name, err)
	}
	header := &record.Header
	retentionWire, err := message.RetentionClassWire(header.RetentionClass, header.EphBucket)
	if err != nil {
		t.Fatal(err)
	}
	recordBytes, err := message.EncodeRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	response, err := self.transport.Call(ctx, &protocol.SubmitRequest{
		GroupId: self.groupId,
		Records: []*protocol.Record{{
			SenderHandle:   append([]byte{}, header.SenderHandle[:]...),
			Epoch:          header.Epoch,
			StreamIndex:    header.StreamIndex,
			IsCommit:       header.IsCommit,
			RetentionClass: uint32(retentionWire),
			SizeBucket:     uint32(header.SizeBucket),
			ExpireAtMs:     header.ExpireAt,
			BodyHash:       append([]byte{}, header.BodyHash[:]...),
			BlobId:         append([]byte{}, header.BlobId...),
			RecordBytes:    recordBytes,
		}},
	})
	if err != nil {
		t.Fatalf("%s's submit: %v", self.name, err)
	}
	if reason := response.GetReason(); reason != protocol.Reason_REASON_OK {
		t.Fatalf("%s's submit was answered %v", self.name, reason)
	}
	results := response.GetSubmit().GetResults()
	if len(results) != 1 || results[0].GetReason() != protocol.Reason_REASON_OK {
		t.Fatalf("%s's commit record was answered %v; the server was expected to take it", self.name, results)
	}
	if err := self.session.AdvanceEpoch(self.pqSecret); err != nil {
		t.Fatalf("%s advancing its session: %v", self.name, err)
	}
}
