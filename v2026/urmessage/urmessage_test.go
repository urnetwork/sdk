package urmessage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/messagegroup"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/sdk/v2026"
)

// THE CASES THAT DO NOT NEED A SERVER LIVE HERE, and the one that does lives in the nested module
// github.com/urnetwork/sdk/cp3b, which requires github.com/urnetwork/message-server.
//
// The split is a decision about the SHIPPED MODULE and not about where a file is tidy: a test in
// this package would put the server module -- and pgx with it -- in github.com/urnetwork/sdk's own
// go.mod, where a build of the mobile SDK would carry it and a checkout without ../msgrepo beside
// it would not resolve at all. ../cp3b is its own module, so `go build ./...` and `go test ./...`
// here neither see it nor need it; test.sh finds it with the same depth-2 sweep it finds cgo, js
// and build with, and by hand it is `cd cp3b && go test -timeout 300s ./...`.

// ── the invite, which is the whole hand-off ──────────────────────────────────────────────────

func TestAnInviteSurvivesTheOnlyThingACarrierDoesToIt(t *testing.T) {
	original := &Invite{
		GroupId:        bytes.Repeat([]byte{0x71}, GroupIdBytes),
		Welcome:        []byte("a welcome, which carries the mls init secret and is therefore key material"),
		RatchetTree:    []byte("a ratchet tree"),
		PqSecret:       bytes.Repeat([]byte{0x2A}, messagegroup.PqSecretBytes),
		GroupHandleKey: bytes.Repeat([]byte{0x5C}, 32),
	}
	encoded, err := original.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	parsed, err := ParseInvite(encoded)
	if err != nil {
		t.Fatalf("ParseInvite: %v", err)
	}
	for _, field := range []struct {
		name string
		want []byte
		got  []byte
	}{
		{"group_id", original.GroupId, parsed.GroupId},
		{"welcome", original.Welcome, parsed.Welcome},
		{"ratchet_tree", original.RatchetTree, parsed.RatchetTree},
		{"pq_secret", original.PqSecret, parsed.PqSecret},
		{"group_handle_key", original.GroupHandleKey, parsed.GroupHandleKey},
	} {
		if !bytes.Equal(field.want, field.got) {
			t.Errorf("%s came back as %x and was encoded as %x", field.name, field.got, field.want)
		}
	}
}

// Every field of an invite is one a join cannot be reconstructed without, so every one of them
// missing is a refusal rather than a zero that surfaces four calls later as a decryption failure.
//
// THE CLASS IS THE STRUCT'S OWN FIELD SET and the case walks it: a sixth field added to [Invite]
// with no clause in check() is a field this case does not cover, which is why the count is
// asserted against reflection rather than against the length of the table below.
func TestAnInviteMissingAnyHalfIsRefused(t *testing.T) {
	whole := func() *Invite {
		return &Invite{
			GroupId:        bytes.Repeat([]byte{0x71}, GroupIdBytes),
			Welcome:        []byte("a welcome"),
			RatchetTree:    []byte("a ratchet tree"),
			PqSecret:       bytes.Repeat([]byte{0x2A}, messagegroup.PqSecretBytes),
			GroupHandleKey: bytes.Repeat([]byte{0x5C}, 32),
		}
	}
	clear := []struct {
		name  string
		empty func(*Invite)
	}{
		{"group_id", func(i *Invite) { i.GroupId = nil }},
		{"welcome", func(i *Invite) { i.Welcome = nil }},
		{"ratchet_tree", func(i *Invite) { i.RatchetTree = nil }},
		{"pq_secret", func(i *Invite) { i.PqSecret = nil }},
		{"group_handle_key", func(i *Invite) { i.GroupHandleKey = nil }},
	}
	if fields := invitefieldCount(); fields != len(clear) {
		t.Fatalf("Invite declares %d fields and this case empties %d of them, so a field is untested", fields, len(clear))
	}
	for _, one := range clear {
		invite := whole()
		one.empty(invite)
		if _, err := invite.Encode(); err == nil {
			t.Errorf("an invite with no %s encoded without complaint", one.name)
		}
	}
	// and a group id of the wrong WIDTH, which is not the same refusal as none at all
	short := whole()
	short.GroupId = short.GroupId[:GroupIdBytes-1]
	if _, err := short.Encode(); err == nil {
		t.Error("an invite naming a short group id encoded without complaint")
	}
}

// invitefieldCount is [Invite]'s field count READ THROUGH REFLECTION AT TEST TIME, so a sixth
// field added to the struct with no clause in check() fails the case above instead of passing it.
// A constant here would be the check that cannot fail.
func invitefieldCount() int {
	return reflect.TypeOf(Invite{}).NumField()
}

func TestAnInviteFromAnotherVersionOrWithOctetsAfterItIsRefused(t *testing.T) {
	whole := &Invite{
		GroupId:        bytes.Repeat([]byte{0x71}, GroupIdBytes),
		Welcome:        []byte("a welcome"),
		RatchetTree:    []byte("a ratchet tree"),
		PqSecret:       bytes.Repeat([]byte{0x2A}, messagegroup.PqSecretBytes),
		GroupHandleKey: bytes.Repeat([]byte{0x5C}, 32),
	}
	encoded, err := whole.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// the control: the untouched blob parses, so the two refusals below are about what they say
	if _, err := ParseInvite(encoded); err != nil {
		t.Fatalf("the untouched invite does not parse: %v", err)
	}

	later := append([]byte(nil), encoded...)
	later[1] = byte(InviteVersion) + 1
	if _, err := ParseInvite(later); err == nil {
		t.Error("an invite at a version this build does not write parsed without complaint")
	}

	// an octet after the last field UNDER A CHECKSUM THAT COVERS IT, so the refusal is the framing's
	// and not the checksum's: this is the blob another encoder would produce, not a damaged one.
	body := append(append([]byte(nil), encoded[:len(encoded)-inviteChecksumBytes]...), 0x00)
	sum := sha256.Sum256(body)
	trailing := append(body, sum[:]...)
	if _, err := ParseInvite(trailing); err == nil {
		t.Error("an invite with an octet after its last field parsed without complaint")
	} else if errors.Is(err, ErrInviteDamaged) {
		t.Errorf("an invite whose checksum covers an extra octet was refused as damaged rather than by its framing: %v", err)
	}

	if _, err := ParseInvite(nil); err == nil {
		t.Error("no octets at all parsed as an invite")
	}
}

// EVERY SINGLE BIT FLIPPED ANYWHERE IN AN INVITE IS REFUSED AT PARSE.
//
// THE REVIEW THAT FOUND THIS flipped one bit at each of 3,408 positions of a real invite and 3,386
// PARSED -- only the 22 that broke the length framing were caught -- and one that corrupted
// group_handle_key JOINED as the intended recipient and then never received a message, the precise
// error arriving at Receive. A user who pastes a damaged invite is told at the paste now, which is
// what this measures: every bit of the encoding, flipped one at a time, and not one parses.
//
// It is every BIT and not every octet, because a check that caught a whole-octet change and missed a
// single-bit one would pass an octet sweep. The bits this cannot refuse as DAMAGED are the version's,
// which are refused by version instead; the case holds that every refusal is one or the other.
//
// WHAT WOULD GO RED: take the checksum comparison out of ParseInvite, and almost every flip parses.
func TestEveryBitFlippedAnywhereInAnInviteIsRefusedAtParse(t *testing.T) {
	whole := &Invite{
		GroupId:        bytes.Repeat([]byte{0x71}, GroupIdBytes),
		Welcome:        bytes.Repeat([]byte("a welcome of some length "), 12),
		RatchetTree:    []byte("a ratchet tree"),
		PqSecret:       bytes.Repeat([]byte{0x2A}, messagegroup.PqSecretBytes),
		GroupHandleKey: bytes.Repeat([]byte{0x5C}, 32),
	}
	encoded, err := whole.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := ParseInvite(encoded); err != nil {
		t.Fatalf("the untouched invite does not parse: %v", err)
	}
	parsed, damaged, byVersion := 0, 0, 0
	for at := 0; at < len(encoded)*8; at += 1 {
		bent := append([]byte(nil), encoded...)
		bent[at/8] ^= 1 << (at % 8)
		_, err := ParseInvite(bent)
		switch {
		case err == nil:
			parsed += 1
			if parsed <= 3 {
				t.Errorf("bit %d (octet %d) flipped and the invite parsed", at, at/8)
			}
		case errors.Is(err, ErrInviteDamaged):
			damaged += 1
		case at < 16:
			byVersion += 1
		default:
			t.Errorf("bit %d flipped and was refused by something other than the checksum: %v", at, err)
		}
	}
	if parsed != 0 {
		t.Fatalf("%d of %d single-bit flips parsed", parsed, len(encoded)*8)
	}
	if byVersion != 16 {
		t.Errorf("%d version bits were refused by version, want all 16", byVersion)
	}
	// AND EVERY TRUNCATION, which is the other thing a carrier does, including the ones shorter than
	// the checksum itself: a paste cut off after the version must be a refusal and not a slice
	// bounds panic in the parser.
	for keep := 0; keep < len(encoded); keep += 1 {
		if _, err := ParseInvite(encoded[:keep]); err == nil {
			t.Fatalf("the first %d of %d octets parsed as an invite", keep, len(encoded))
		}
	}
	t.Logf("%d octets, %d single-bit flips: %d refused as damaged, %d by version, 0 parsed",
		len(encoded), len(encoded)*8, damaged, byVersion)
}

// ── the head ─────────────────────────────────────────────────────────────────────────────────

func TestAHeadCarriesTheSendersOwnClockReadingBackUnchanged(t *testing.T) {
	for _, sentAtMs := range []int64{0, 1, 1_700_000_000_000, 1 << 40} {
		got, err := decodeHead(encodeHead(sentAtMs))
		if err != nil {
			t.Fatalf("decodeHead over %d: %v", sentAtMs, err)
		}
		if got != sentAtMs {
			t.Errorf("sent_at %d came back as %d", sentAtMs, got)
		}
	}
}

func TestAHeadThisBuildDidNotWriteIsRefused(t *testing.T) {
	head := encodeHead(1_700_000_000_000)
	if _, err := decodeHead(head); err != nil {
		t.Fatalf("the control: this build's own head does not decode: %v", err)
	}

	short := head[:len(head)-1]
	if _, err := decodeHead(short); !errors.Is(err, ErrHeadFormat) {
		t.Errorf("a head one octet short answered %v, want ErrHeadFormat", err)
	}
	long := append(append([]byte(nil), head...), 0x00)
	if _, err := decodeHead(long); !errors.Is(err, ErrHeadFormat) {
		t.Errorf("a head one octet long answered %v, want ErrHeadFormat", err)
	}
	other := append([]byte(nil), head...)
	other[0] = headVersion + 1
	if _, err := decodeHead(other); !errors.Is(err, ErrHeadFormat) {
		t.Errorf("a head at another version answered %v, want ErrHeadFormat", err)
	}
}

// ── §4.3.8's op byte, read off the descriptor ────────────────────────────────────────────────

// The op a fetch is macced under is the FIELD NUMBER of the fetch arm, and this holds the two
// against each other through the descriptor rather than against a number written here.
//
// A constant would be a number that stays right until the proto is renumbered, at which point
// every fetch this package sends carries a MAC over the wrong operation and the server answers
// REASON_REJECTED with nothing to say why.
func TestTheOpOfARequestIsItsOwnArmsFieldNumber(t *testing.T) {
	oneof := (&protocol.MessageServerRequest{}).ProtoReflect().Descriptor().Oneofs().ByName("body")
	if oneof == nil {
		t.Fatal("MessageServerRequest declares no body oneof")
	}
	seen := 0
	for index := 0; index < oneof.Fields().Len(); index += 1 {
		field := oneof.Fields().Get(index)
		if field.Message().FullName() != (&protocol.FetchRequest{}).ProtoReflect().Descriptor().FullName() {
			continue
		}
		seen += 1
		op, err := opOf(&protocol.FetchRequest{})
		if err != nil {
			t.Fatalf("opOf over a FetchRequest: %v", err)
		}
		if uint64(op) != uint64(field.Number()) {
			t.Errorf("opOf answered %d and the fetch arm is field %d", op, field.Number())
		}
	}
	if seen != 1 {
		t.Fatalf("%d arms of MessageServerRequest.body carry a FetchRequest, want exactly 1", seen)
	}

	// and a message that is no arm of that oneof at all is a refusal rather than a zero, because
	// zero is a real field number's neighbour and a MAC under it verifies nowhere
	if _, err := opOf(&protocol.HelloResponse{}); err == nil {
		t.Error("a message that is no arm of MessageServerRequest.body answered an op")
	}
}

// ── the refusals a device owes before anything reaches a wire ────────────────────────────────

func TestADeviceWithNoTransportOrNoReserverIsRefused(t *testing.T) {
	if _, err := NewDevice(DeviceConfig{}); !errors.Is(err, ErrNoTransport) {
		t.Errorf("a device with no transport answered %v, want ErrNoTransport", err)
	}
	if _, err := NewDevice(DeviceConfig{Transport: newSilentTransport(t)}); !errors.Is(err, ErrNoReserver) {
		t.Errorf("a device with no reserver answered %v, want ErrNoReserver", err)
	}
}

// Nothing this package does can precede Hello, because every authenticator it computes is a MAC
// over the connection's nonce and there is no nonce until 4.3.1 issues one.
//
// IT IS REFUSED BY NAME HERE rather than met as a REASON_REJECTED on the wire: a request sent
// before Hello costs a round trip and comes back as the same refusal a forged MAC does.
func TestEverythingIsRefusedBeforeHello(t *testing.T) {
	device := newSilentDevice(t)
	ctx := context.Background()

	if _, err := device.CreateGroup(ctx, bytes.Repeat([]byte{0x71}, GroupIdBytes)); !errors.Is(err, ErrNotConnected) {
		t.Errorf("CreateGroup before Hello answered %v, want ErrNotConnected", err)
	}
	if _, err := device.Join(ctx, &Invite{
		GroupId:        bytes.Repeat([]byte{0x71}, GroupIdBytes),
		Welcome:        []byte("a welcome"),
		RatchetTree:    []byte("a ratchet tree"),
		PqSecret:       bytes.Repeat([]byte{0x2A}, messagegroup.PqSecretBytes),
		GroupHandleKey: bytes.Repeat([]byte{0x5C}, 32),
	}); !errors.Is(err, ErrNotConnected) {
		t.Errorf("Join before Hello answered %v, want ErrNotConnected", err)
	}
	// and a group id of the wrong width is refused before the connection is even consulted,
	// which is the control that says the refusal above is about the connection
	if _, err := device.CreateGroup(ctx, []byte("short")); errors.Is(err, ErrNotConnected) {
		t.Error("a short group id was refused for the connection rather than for its width")
	}
}

// ── a transport over a client that answers nothing ───────────────────────────────────────────

// silentClient satisfies [sdk.MessageTransportClient] and never answers. It exists so that the
// refusals above can be reached without a server: every one of them is raised BEFORE a frame is
// sent, so a client that would never answer is exactly the right shape to prove it.
type silentClient struct{}

func (silentClient) SendWithTimeout(frame *protocol.Frame, destination connect.Id,
	ackCallback connect.AckFunction, timeout time.Duration, opts ...any) bool {
	return false
}

func (silentClient) AddReceiveCallback(receiveCallback connect.ReceiveFunction) func() {
	return func() {}
}

func newSilentTransport(t *testing.T) *sdk.MessageTransport {
	t.Helper()
	transport, err := sdk.NewMessageTransport(&sdk.MessageTransportConfig{
		Client:          silentClient{},
		Server:          connect.NewId(),
		ProtocolVersion: 1,
	})
	if err != nil {
		t.Fatalf("sdk.NewMessageTransport: %v", err)
	}
	t.Cleanup(transport.Close)
	return transport
}

func newSilentDevice(t *testing.T) *Device {
	t.Helper()
	streamStore, err := sdk.OpenStreamStore(t.TempDir())
	if err != nil {
		t.Fatalf("sdk.OpenStreamStore: %v", err)
	}
	t.Cleanup(func() { streamStore.Close() })
	device, err := NewDevice(DeviceConfig{
		Transport: newSilentTransport(t),
		Reserver:  sdk.NewStreamIndexReserver(streamStore),
	})
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}
	t.Cleanup(func() { device.Close() })
	return device
}
