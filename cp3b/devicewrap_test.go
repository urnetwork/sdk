package cp3b

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/urnetwork/connect/messagegroup"
	"github.com/urnetwork/sdk/urmessage"
)

// S2-26'S ACCEPTANCE PROPERTY: A DEVICE OPENS WHAT IS ADDRESSED TO THE LEAF IT PUBLISHES.
//
// UNTIL THIS COMMIT THIS TEST COULD NOT BE WRITTEN, and that is the measure of the step rather
// than a turn of phrase. `deviceIdentity` drew an X-Wing key pair, encoded the PUBLIC half into
// the urmessage_leaf_keys extension and dropped the private half in the same statement; no device
// held a decapsulation key for the leaf it advertised, and `XwingEncapsulate` and
// `XwingDecapsulate` had zero production callers outside `connect/messagegroup/xwing.go`.
//
// WHY IT MATTERS NOW. Ledger item 243 rotates `pq_secret` per epoch; ruling 36 fixed the carrier
// as the X-Wing device wrap, because HPKE in `connect/mls` is hard-wired to X25519 and the MLS
// exporter therefore carries no post-quantum contribution at all. Every later step of that item --
// the wrap record, its `env_key`, the per-epoch map -- rests on this one fact, and none of them is
// in this commit.
//
// WHAT IS REAL HERE, because the property is worth nothing over a fixture:
//
//   - a real message server, a real `connect.Client`, real submissions and fetches: [newWorld];
//   - a real founding commit and a real Join, so the ratchet tree holds a leaf per device;
//   - THE KEY THAT TRAVELLED, read off ALICE's tree through [urmessage.Group.MemberWrapKeys] ->
//     the seam's MemberAt -> the leaf node's own 0xF002 extension. It is NOT read off the device
//     that owns it. A version of this case that encapsulated to bob's own local leaf keys body
//     would pass against a build where the leaf bob published carried something else entirely,
//     which is the one failure the property exists to exclude.
//
// THE NEGATIVE CONTROL IS INLINE AND IT IS THE SHAPE OF THE ASSERTION, not an extra clause bolted
// on: every ciphertext is handed to BOTH devices, and the case demands that exactly one opens it
// and that the two rows of the tree are opened by DIFFERENT devices. A `DecapsulateToOwnLeaf` that
// answered a constant, a build where both devices somehow share a seed, and a `MemberWrapKeys`
// that quietly answered the caller's own key twice are all red under that one demand, and none of
// them is red under "bob opens bob's".
//
// THE WRONG DEVICE MUST NOT ERROR, AND THAT IS ASSERTED. X-Wing decapsulates under any well formed
// key: the wrong seed answers 32 uniform-looking octets and no error, which is exactly why "it did
// not error" can never stand in for "it opened it" here.
func TestEachLeafsPublishedWrapKeyIsOpenedByThatLeafsOwnDeviceAndNoOther(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()

	alice := world.newPersona(t, "alice")
	bob := world.newPersona(t, "bob")
	if err := alice.device.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}
	groupId := newGroupId(t)
	aliceGroup, bobGroup := openPair(t, ctx, alice, bob, groupId)

	// ── the tree, read from both sides ────────────────────────────────────────────────────
	//
	// The two members must agree on it, which is what says these are the wire values: alice's
	// row for bob came out of the leaf bob's KeyPackage carried into the group, and bob's row
	// for bob came out of the leaf bob's own session holds.
	aliceView := wrapKeysOf(t, "alice's tree", aliceGroup)
	bobView := wrapKeysOf(t, "bob's tree", bobGroup)
	if len(aliceView) != 2 {
		t.Fatalf("a two member group's tree published %d wrap keys", len(aliceView))
	}
	assertSameTree(t, "before the restart", aliceView, bobView)

	// ── the property, per leaf, with the negative control in the same loop ────────────────
	owner := map[uint32]string{}
	for leaf, published := range aliceView {
		ciphertext, shared := encapsulateTo(t, published)
		opened := openers(t, ciphertext, shared,
			namedDevice{"alice", alice.device}, namedDevice{"bob", bob.device})
		if len(opened) != 1 {
			t.Fatalf("the encapsulation to leaf %d was opened by %v; exactly one device holds the seed for one leaf",
				leaf, opened)
		}
		owner[leaf] = opened[0]
	}
	if len(owner) != 2 {
		t.Fatalf("two leaves resolved to %d owners: %v", len(owner), owner)
	}
	names := map[string]uint32{}
	for leaf, name := range owner {
		if previous, twice := names[name]; twice {
			t.Fatalf("%s's device opened BOTH leaf %d and leaf %d; the tree is not publishing a key per device, or the read is answering one device's key twice",
				name, previous, leaf)
		}
		names[name] = leaf
	}
	bobLeaf, found := names["bob"]
	if !found {
		t.Fatalf("no leaf of this group is opened by bob's device: %v", owner)
	}
	t.Logf("leaf %d is alice's and leaf %d is bob's, decided by which device opened which encapsulation",
		names["alice"], bobLeaf)

	// ── and it survives a restart from the durable store ──────────────────────────────────
	//
	// The only thing that crosses [world.restart] is two directories. A device that came back
	// without its seed is a device that occupies a leaf it can no longer read, which is the
	// permanent, undiagnosable failure ruling 38 names -- so the seed being on the disk rather
	// than in memory is not a convenience, it is the property.
	bobBefore := append([]byte(nil), aliceView[bobLeaf]...)
	dead := bob.device
	bob = world.restart(t, bob)
	if err := bob.device.Connect(ctx); err != nil {
		t.Fatalf("the restarted bob's Connect: %v", err)
	}
	restored, err := bob.device.Restore(ctx)
	if err != nil {
		t.Fatalf("the restarted bob's Restore: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("bob was in one group and %d came back", len(restored))
	}
	bobGroup = restored[0]

	// THE LEAF DID NOT MOVE. A restored device on a NEW leaf with a NEW key would pass the
	// decapsulation below and still be the failure this case is about, so the identity of the
	// leaf is asserted before anything is encapsulated to it.
	afterView := wrapKeysOf(t, "alice's tree after the restart", aliceGroup)
	assertSameTree(t, "after the restart", afterView, wrapKeysOf(t, "the restored bob's tree", bobGroup))
	if !bytes.Equal(afterView[bobLeaf], bobBefore) {
		t.Fatalf("leaf %d publishes a different encapsulation key after the restart", bobLeaf)
	}

	ciphertext, shared := encapsulateTo(t, afterView[bobLeaf])
	opened := openers(t, ciphertext, shared,
		namedDevice{"alice", alice.device}, namedDevice{"the restarted bob", bob.device})
	if len(opened) != 1 || opened[0] != "the restarted bob" {
		t.Fatalf("after a restart the encapsulation to bob's leaf was opened by %v", opened)
	}

	// AND THE DEVICE THAT DIED HOLDS NOTHING. [persona.kill] closes the device, which erases the
	// seed in place; the refusal is BY NAME because 32 zero octets are a well formed seed and a
	// device that decapsulated under them would answer a wrong secret with no error.
	if _, err := dead.DecapsulateToOwnLeaf(ciphertext); !errors.Is(err, urmessage.ErrNoDeviceWrapKey) {
		t.Errorf("the killed device answered %v, want ErrNoDeviceWrapKey", err)
	}
}

// namedDevice is a device and the name this case reports it under.
type namedDevice struct {
	name   string
	device *urmessage.Device
}

// openers is every device that decapsulated `ciphertext` to `shared`.
//
// IT ASSERTS WHAT THE OTHERS DID, and that is the half a "which one opened it" helper usually
// leaves out: a device that is not the owner must answer a full-length secret and a NIL error,
// because X-Wing has no way to refuse a ciphertext that is well formed. A device that errored here
// would make "exactly one opened it" true for a reason that has nothing to do with which seed it
// holds.
func openers(t *testing.T, ciphertext []byte, shared []byte, devices ...namedDevice) []string {
	t.Helper()
	opened := []string{}
	for _, one := range devices {
		got, err := one.device.DecapsulateToOwnLeaf(ciphertext)
		if err != nil {
			t.Fatalf("%s's DecapsulateToOwnLeaf answered %v; a device holding a seed decapsulates every well formed ciphertext, rightly or wrongly",
				one.name, err)
		}
		if len(got) != messagegroup.XwingSharedSize {
			t.Fatalf("%s's DecapsulateToOwnLeaf answered %d octets, want %d",
				one.name, len(got), messagegroup.XwingSharedSize)
		}
		if bytes.Equal(got, shared) {
			opened = append(opened, one.name)
		}
	}
	return opened
}

// encapsulateTo addresses one published encapsulation key, through the parser the wire value has
// to survive: a key that reached here as 1216 octets and is not one is refused by
// ParseXwingPublicKey before anything is encapsulated to it.
func encapsulateTo(t *testing.T, published []byte) (ciphertext []byte, shared []byte) {
	t.Helper()
	if len(published) != messagegroup.XwingPublicKeySize {
		t.Fatalf("a published wrap key is %d octets, want %d", len(published), messagegroup.XwingPublicKeySize)
	}
	pub, err := messagegroup.ParseXwingPublicKey(published)
	if err != nil {
		t.Fatalf("messagegroup.ParseXwingPublicKey over a key read off the tree: %v", err)
	}
	ciphertext, shared, err = messagegroup.XwingEncapsulate(rand.Reader, pub)
	if err != nil {
		t.Fatalf("messagegroup.XwingEncapsulate: %v", err)
	}
	return ciphertext, shared
}

// wrapKeysOf is one group's view of the tree, keyed by leaf.
func wrapKeysOf(t *testing.T, who string, group *urmessage.Group) map[uint32][]byte {
	t.Helper()
	rows, err := group.MemberWrapKeys()
	if err != nil {
		t.Fatalf("%s: MemberWrapKeys: %v", who, err)
	}
	view := map[uint32][]byte{}
	for _, row := range rows {
		if _, twice := view[row.Leaf]; twice {
			t.Fatalf("%s names leaf %d twice", who, row.Leaf)
		}
		view[row.Leaf] = row.XwingPub
	}
	if len(view) != len(rows) {
		t.Fatalf("%s answered %d rows over %d leaves", who, len(rows), len(view))
	}
	return view
}

// assertSameTree is the two members agreeing on every leaf's published key.
func assertSameTree(t *testing.T, when string, left map[uint32][]byte, right map[uint32][]byte) {
	t.Helper()
	if len(left) != len(right) {
		t.Fatalf("%s: the two members see %d and %d leaves", when, len(left), len(right))
	}
	for leaf, key := range left {
		other, found := right[leaf]
		if !found {
			t.Fatalf("%s: one member sees leaf %d and the other does not", when, leaf)
		}
		if !bytes.Equal(key, other) {
			t.Fatalf("%s: the two members disagree about leaf %d's encapsulation key", when, leaf)
		}
	}
}
