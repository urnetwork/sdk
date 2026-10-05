package cp3b

import (
	"bytes"
	"context"
	"testing"

	"github.com/urnetwork/message-server/peer"
	"github.com/urnetwork/sdk/urmessage"
)

// THE STRING. It is typed here and nowhere else, it is not derivable from anything on the wire,
// and every assertion below compares against this variable rather than against a literal repeated
// at the point of comparison -- a test that spelled the text twice could pass with one of the two
// spellings wrong.
const typedByAlice = "hello bob -- this is the first sentence either of these devices has ever encrypted for the other"

// And the sentence that goes back, so the case says the joiner is a MEMBER rather than a reader.
const typedByBob = "hello alice -- and this is the answer, sealed under the joiner's own leaf"

// CP3b, WHOLE: A STRING TYPED ON ONE CLIENT COMES BACK AS THE SAME STRING ON A SECOND, THROUGH A
// RUNNING MESSAGE SERVER, WITH EVERY KEY REAL.
//
// WHAT IS REAL ON THIS PATH, named one by one, because the bar is about what is NOT here:
//
//   - The MLS group. `connect/messagegroup.NewConnectMlsEngine` over a real
//     `mls.CryptoProvider`, a real Ed25519 signature key pair drawn per device and a real X-Wing
//     leaf key. Alice founds, bob publishes a key package, alice commits an Add, bob joins from
//     the Welcome. Two engines, one group, one epoch.
//   - Every key on the seal and open path. `storage_root[1]` is `StorageRoot(mls_secret[1],
//     pq_secret)` off each engine's own exporter; the class keys, `record_key[k]`, both AEAD keys,
//     `write_key`, `read_key`, `sender_handle` and `group_handle_key` are all derived inside the
//     two sessions by production bodies. THIS TEST SUPPLIES NO KEY. What it supplies is a group
//     id, two strings and two temporary directories.
//   - The stream index. `sdk.OpenStreamStore` on disk, one directory per device, fsync'd before an
//     index is returned -- not an in-memory counter.
//   - The transport. `sdk.MessageTransport`, §10.1's four code points, `request_id` correlation
//     and §4.6 fragmentation, over two real `connect.Client`s.
//   - The server. `peer` + `api` + `store`, running §5.1's whole pipeline. It verifies
//     `write_auth` under the epoch key it was handed in the founding commit, enforces §6.1's
//     ordering, and refuses anything it cannot check.
//
// WHAT IS HANDED OVER OUT OF BAND, and it is a CARRIER that is missing rather than a key source:
// the [urmessage.Invite]. It is encoded to octets and parsed back below, so what crosses in this
// test is the same blob a real carrier would move, and the rendezvous that would move it is out of
// scope for the alpha.
//
// THE CONTROL THAT MAKES "ENCRYPTED" MORE THAN A WORD is the last clause: the server's OWN STORAGE
// is read directly, through `store.MemoryStore.Fetch`, and neither string appears in any column of
// any row. A build whose ciphertext was a copy of its plaintext would pass every other assertion
// here.
func TestATextTypedOnOneClientComesBackOnASecondThroughARunningServer(t *testing.T) {
	world := newWorld(t)
	ctx := context.Background()
	before := world.peer.Stats()

	alice, _, _ := world.device(t, "alice")
	bob, _, _ := world.device(t, "bob")

	// ── §4.3.1 on both clients ───────────────────────────────────────────────────────────
	if err := alice.Connect(ctx); err != nil {
		t.Fatalf("alice's Connect: %v", err)
	}
	if err := bob.Connect(ctx); err != nil {
		t.Fatalf("bob's Connect: %v", err)
	}

	// ── (1) a group exists: one client creates it, a second joins by Welcome ─────────────
	groupId := newGroupId(t)
	aliceGroup, err := alice.CreateGroup(ctx, groupId)
	if err != nil {
		t.Fatalf("alice's CreateGroup: %v", err)
	}
	if epoch := aliceGroup.Epoch(); epoch != 0 {
		t.Fatalf("a freshly founded group is at epoch %d, want 0", epoch)
	}
	keyPackage, err := bob.KeyPackage()
	if err != nil {
		t.Fatalf("bob's KeyPackage: %v", err)
	}
	invite, err := aliceGroup.AddMember(keyPackage)
	if err != nil {
		t.Fatalf("alice's AddMember: %v", err)
	}
	if epoch := aliceGroup.Epoch(); epoch != 1 {
		t.Fatalf("the commit that added bob left the group at epoch %d, want 1", epoch)
	}
	// the hand-off crosses as OCTETS, which is what a carrier would move
	encoded, err := invite.Encode()
	if err != nil {
		t.Fatalf("encoding the invite: %v", err)
	}
	carried, err := urmessage.ParseInvite(encoded)
	if err != nil {
		t.Fatalf("parsing the invite: %v", err)
	}

	// the group is published: §6.1's founding commit, the epoch's wraps, and the marker
	if err := aliceGroup.Open(ctx); err != nil {
		t.Fatalf("alice's Open: %v", err)
	}
	if !aliceGroup.IsOpen() {
		t.Fatal("Open returned no error and the group does not say it is open")
	}

	// ── (2) send a text message ──────────────────────────────────────────────────────────
	sent, err := aliceGroup.Send(ctx, typedByAlice)
	if err != nil {
		t.Fatalf("alice's Send: %v", err)
	}
	if sent.RecordId == 0 {
		t.Fatal("the message was accepted and allocated no record_id")
	}
	if !sent.Mine {
		t.Fatal("a message this device sent came back marked as somebody else's")
	}

	bobGroup, err := bob.Join(ctx, carried)
	if err != nil {
		t.Fatalf("bob's Join: %v", err)
	}
	if epoch := bobGroup.Epoch(); epoch != aliceGroup.Epoch() {
		t.Fatalf("alice is at epoch %d and bob joined at %d", aliceGroup.Epoch(), epoch)
	}

	// ── (3) receive it: fetch, parse, OpenRecord, get the text back ─────────────────────
	received, err := bobGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("bob's Receive: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("bob received %d messages for the one alice sent: %+v", len(received), received)
	}
	// THE PROPERTY.
	if received[0].Text != typedByAlice {
		t.Fatalf("the text came back as %q and was typed as %q", received[0].Text, typedByAlice)
	}
	if received[0].RecordId != sent.RecordId {
		t.Fatalf("bob opened record %d and alice was told her message is record %d",
			received[0].RecordId, sent.RecordId)
	}
	if received[0].Mine {
		t.Fatal("a message bob received from alice came back marked as bob's own")
	}
	if received[0].SentAtMs != sent.SentAtMs {
		t.Fatalf("the head opened with sent_at %d and alice sealed %d", received[0].SentAtMs, sent.SentAtMs)
	}
	if bytes.Equal(received[0].SenderHandle, make([]byte, len(received[0].SenderHandle))) {
		t.Fatal("the record names a sender_handle of zeroes")
	}
	t.Logf("a DURABLE record crossed: %d octets of text, record_id %d, sender_handle %x",
		len(received[0].Text), received[0].RecordId, received[0].SenderHandle)

	// ── and back the other way, which is what says the joiner is a member ────────────────
	answered, err := bobGroup.Send(ctx, typedByBob)
	if err != nil {
		t.Fatalf("bob's Send: %v", err)
	}
	back, err := aliceGroup.Receive(ctx)
	if err != nil {
		t.Fatalf("alice's Receive: %v", err)
	}
	if len(back) != 1 {
		t.Fatalf("alice received %d messages for the one bob sent: %+v", len(back), back)
	}
	if back[0].Text != typedByBob {
		t.Fatalf("bob's text came back as %q and was typed as %q", back[0].Text, typedByBob)
	}
	if back[0].RecordId != answered.RecordId {
		t.Fatalf("alice opened record %d and bob was told his message is record %d",
			back[0].RecordId, answered.RecordId)
	}
	if bytes.Equal(back[0].SenderHandle, received[0].SenderHandle) {
		t.Fatal("alice and bob seal under the same sender_handle, so the two directions are one stream")
	}

	// ── nothing was dropped on either side ───────────────────────────────────────────────
	assertNothingFailedToOpen(t, "alice", aliceGroup)
	assertNothingFailedToOpen(t, "bob", bobGroup)
	if rebound := aliceGroup.Stats().Rebound; rebound != 0 {
		t.Errorf("alice's sends took %d S2-2 recoveries on a connection nothing replaced", rebound)
	}
	if rebound := bobGroup.Stats().Rebound; rebound != 0 {
		t.Errorf("bob's sends took %d S2-2 recoveries on a connection nothing replaced", rebound)
	}

	// ── it travelled, and the server counted it ──────────────────────────────────────────
	//
	// Every request this case makes, named, so the number is a statement about the seam's
	// chattiness rather than a total nobody can check: two Hellos (one per device); four for
	// Open (the create_group, two wraps for two members, the epoch-complete marker); alice's
	// submit; bob's fetch; bob's submit; alice's fetch.
	assertServed(t, world.peer.Stats(), before, 10)

	// ── THE CONTROL: the server's own storage holds neither string ───────────────────────
	assertServerCannotRead(t, world, groupId, typedByAlice, typedByBob)
}

// assertNothingFailedToOpen holds the counter that says a message arrived and this build would not
// show it. It is separate from the round trip because a group can open the one record a case looks
// at and silently fail on every other.
func assertNothingFailedToOpen(t *testing.T, who string, group *urmessage.Group) {
	t.Helper()
	stats := group.Stats()
	t.Logf("%s: fetched %d, opened %d, ceremony %d, own %d, other classes %d, failed %d",
		who, stats.Fetched, stats.Opened, stats.SkippedCeremony, stats.SkippedOwn,
		stats.SkippedClass, stats.FailedOpen)
	if stats.FailedOpen != 0 {
		t.Errorf("%s: %d records from a member of this group did not open", who, stats.FailedOpen)
	}
	if stats.Fetched == 0 {
		t.Errorf("%s: the fetch answered no records at all, so nothing was examined", who)
	}
}

// assertServed holds what the SERVER counted against what this case asked for.
//
// It is the server's own dispatcher counter and not the client's, which is what makes it
// unfakeable from the client side: a seam that answered out of a cache, or a fixture that called
// api directly, would move nothing here.
func assertServed(t *testing.T, after peer.Stats, before peer.Stats, want uint64) {
	t.Helper()
	served := after.RequestsServed - before.RequestsServed
	t.Logf("the server served %d requests, sent %d responses, dropped %d frames, failed %d sends",
		served, after.ResponsesSent-before.ResponsesSent,
		after.FramesDropped-before.FramesDropped, after.ResponsesFailed-before.ResponsesFailed)
	if served != want {
		t.Errorf("this case made %d requests and the server's dispatcher served %d", want, served)
	}
	if dropped := after.FramesDropped - before.FramesDropped; dropped != 0 {
		t.Errorf("the server dropped %d frames it could not decode", dropped)
	}
	if failed := after.ResponsesFailed - before.ResponsesFailed; failed != 0 {
		t.Errorf("the server failed %d sends", failed)
	}
	if refused := after.RefusalsDropped - before.RefusalsDropped; refused != 0 {
		t.Errorf("the server dropped %d refusals", refused)
	}
}

// assertServerCannotRead reads the SERVER'S OWN ROWS and holds every octet column against every
// string this case typed.
//
// This is the clause that makes "encrypted" a measurement. It reads `store.MemoryStore` directly
// rather than the wire, so it is looking at what the operator would have: a build that put the
// plaintext in `ct_body`, or that sealed under a key both sides agreed on and then also stored the
// text beside it, fails here and passes every round trip above.
func assertServerCannotRead(t *testing.T, world *world, groupId []byte, secrets ...string) {
	t.Helper()
	records := world.allRows(t, groupId)
	if len(records) == 0 {
		t.Fatal("the server holds no rows for this group, so this control examined nothing")
	}
	t.Logf("the server holds %d rows for this group, and neither string is in any of them", len(records))
	searched := 0
	for _, row := range records {
		columns := map[string][]byte{
			"ct_head":           row.CtHead,
			"ct_body":           row.CtBody,
			"server_attachment": row.ServerAttachment,
			"body_hash":         row.BodyHash,
			"sender_handle":     row.SenderHandle,
		}
		// THE CONTROL ON THE CONTROL, and it was missing: every clause below is a
		// bytes.Contains that must NOT match, and a search over five fields that are empty,
		// or a Contains that never matches anything, passes all of them in silence. So each
		// non-empty column is first searched for a needle that IS in it -- its own first four
		// octets -- and the column is COUNTED. A build that renamed these fields, or that
		// answered rows with nothing in them, fails here instead of passing the negatives.
		for name, column := range columns {
			if len(column) < 4 {
				continue
			}
			if !bytes.Contains(column, column[:4]) {
				t.Fatalf("the search over the server's %s column cannot find a needle that is in it, so the clauses below prove nothing", name)
			}
			searched += 1
		}
		for _, secret := range secrets {
			for name, column := range columns {
				if bytes.Contains(column, []byte(secret)) {
					t.Errorf("the server's %s column of record %d carries %q in the clear",
						name, row.StreamIndex, secret)
				}
			}
		}
	}
	if searched == 0 {
		t.Fatal("every column of every row is shorter than four octets, so this control searched nothing")
	}
	t.Logf("%d non-empty octet columns were searched, each first proven searchable against itself", searched)
}
