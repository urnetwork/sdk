package urmessage

import (
	"bytes"
	"crypto/sha256"
	"fmt"

	"github.com/urnetwork/connect/mls/syntax"
)

// InviteVersion is the version every [Invite] this build encodes carries. A blob written by another
// build is refused by [ParseInvite] rather than read as this one.
//
// 0x0002 ADDED THE CHECKSUM, AND AN 0x0001 BLOB IS REFUSED BY VERSION. Version one was framing and
// nothing else, so a damaged invite PARSED and JOINED and the damage surfaced one call later: a
// review flipped one bit at each of 3,408 positions of a real invite and 3,386 parsed; one that
// corrupted group_handle_key joined cleanly as the intended recipient and then never received a
// message, with the precise error arriving at Receive instead of at the paste. See [ParseInvite].
const InviteVersion uint16 = 0x0002

// inviteChecksumBytes is the width of the SHA-256 an encoded invite ends with.
const inviteChecksumBytes = sha256.Size

// Invite is everything a second device needs in order to join a group, and it is a HAND-OFF rather
// than a message: this package has no channel to carry it and does not invent one, because the
// rendezvous and the contact card are out of scope for the alpha.
//
// IT IS SECRET IN FULL. Two of its four fields are key material -- pq_secret is the IKM of every
// storage root this group will ever extract, and the Welcome carries the MLS init secret -- so an
// invite that reaches a third party is a group that third party is in. Move it the way you would
// move a private key: over a channel that is already authenticated and already confidential, once,
// and destroy it afterwards.
//
// WHAT EACH FIELD IS AND WHOSE OPEN ITEM ITS DELIVERY IS. Every one is a value a PRODUCTION
// function of connect produced; what does not exist is a carrier.
//
//   - Welcome and RatchetTree: connect/mls's own, from GroupHandle.Commit. Ledger 44a's named,
//     gated hand-off.
//   - PqSecret: messagegroup.NewPqSecret. Its delivery is M1-20 and m1 task 14.
//   - GroupHandleKey: GroupHandleKey(StorageRoot(mls_secret[0], pq_secret)), computed at epoch zero
//     and never recomputed. Its carrier is M1-2. It is in here because a session opened at any
//     epoch after zero is REFUSED without it -- a value recomputed from a later root would give
//     every epoch a different sender_handle and end every member's stream at every commit.
//   - GroupId: the 32 octets the server keys its rows by.
type Invite struct {
	GroupId        []byte
	Welcome        []byte
	RatchetTree    []byte
	PqSecret       []byte
	GroupHandleKey []byte
}

// check refuses an invite that is missing a half nothing downstream could recover.
func (self *Invite) check() error {
	if len(self.GroupId) != GroupIdBytes {
		return fmt.Errorf("urmessage: an invite names a %d octet group id, want %d", len(self.GroupId), GroupIdBytes)
	}
	if len(self.Welcome) == 0 {
		return fmt.Errorf("urmessage: an invite carries no welcome, and there is nothing else to join from")
	}
	if len(self.RatchetTree) == 0 {
		return fmt.Errorf("urmessage: an invite carries no ratchet tree")
	}
	if len(self.PqSecret) == 0 {
		return fmt.Errorf("urmessage: an invite carries no pq_secret, which is the ikm of every storage root this group extracts")
	}
	if len(self.GroupHandleKey) == 0 {
		return fmt.Errorf("urmessage: an invite carries no group_handle_key, and a session at any epoch after zero is refused without it")
	}
	return nil
}

// Encode is the invite as octets a caller can move.
//
// The encoding is connect/mls/syntax's length prefixes, which is the one length prefix this corpus
// writes, so a field added to [Invite] later is a version bump here rather than a second framing --
// followed by the SHA-256 of every octet before it. See [ParseInvite] for what the checksum is and
// is not.
func (self *Invite) Encode() ([]byte, error) {
	if err := self.check(); err != nil {
		return nil, err
	}
	writer := syntax.NewWriter()
	writer.WriteUint16(InviteVersion)
	writer.WriteOpaqueLP(self.GroupId)
	writer.WriteOpaqueLP(self.Welcome)
	writer.WriteOpaqueLP(self.RatchetTree)
	writer.WriteOpaqueLP(self.PqSecret)
	writer.WriteOpaqueLP(self.GroupHandleKey)
	encoded, err := writer.Bytes()
	if err != nil {
		return nil, fmt.Errorf("urmessage: encoding an invite: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return append(encoded, sum[:]...), nil
}

// ParseInvite reads back what [Invite.Encode] wrote, and refuses a blob that is not exactly that.
//
// A DAMAGED INVITE IS REFUSED HERE, AT THE PASTE, AND NOT ONE CALL AFTER THE JOIN. The last 32
// octets are the SHA-256 of everything before them, and a blob whose checksum does not match is
// [ErrInviteDamaged] before any field is read. That is the whole of what changed at version 0x0002,
// and the reason is where a user is told: a truncated copy, a mangled paste or a carrier that
// rewrote one octet used to PARSE, JOIN, and then fail at the first Receive with a sentence about
// sender handles that no user can act on.
//
// THE CHECKSUM IS NOT AN AUTHENTICATION AND MUST NOT BE READ AS ONE. Anybody who can change an
// invite can recompute it. What stands between a hostile carrier and a group is what always stood
// there -- the invite is key material and must travel over a channel that is already authenticated
// and confidential -- and, one layer down, MLS: a Welcome addressed to a key package this device did
// not publish is refused at the join whatever the checksum says.
func ParseInvite(encoded []byte) (*Invite, error) {
	reader := syntax.NewReader(encoded)
	version, err := reader.ReadUint16()
	if err != nil {
		return nil, fmt.Errorf("%w: it has no version: %w", ErrInviteDamaged, err)
	}
	if version != InviteVersion {
		return nil, fmt.Errorf("urmessage: an invite at version %#04x, and this build reads only %#04x",
			version, InviteVersion)
	}
	if len(encoded) < 2+inviteChecksumBytes {
		return nil, fmt.Errorf("%w: %d octets is shorter than a version and a checksum", ErrInviteDamaged, len(encoded))
	}
	body := encoded[:len(encoded)-inviteChecksumBytes]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], encoded[len(encoded)-inviteChecksumBytes:]) {
		return nil, fmt.Errorf("%w: its last %d octets are not the SHA-256 of the %d before them",
			ErrInviteDamaged, inviteChecksumBytes, len(body))
	}
	reader = syntax.NewReader(body)
	if _, err := reader.ReadUint16(); err != nil {
		return nil, fmt.Errorf("%w: it has no version: %w", ErrInviteDamaged, err)
	}
	invite := &Invite{}
	for _, field := range []struct {
		name string
		into *[]byte
	}{
		{"group_id", &invite.GroupId},
		{"welcome", &invite.Welcome},
		{"ratchet_tree", &invite.RatchetTree},
		{"pq_secret", &invite.PqSecret},
		{"group_handle_key", &invite.GroupHandleKey},
	} {
		value, err := reader.ReadOpaqueLP()
		if err != nil {
			return nil, fmt.Errorf("urmessage: an invite with no %s: %w", field.name, err)
		}
		*field.into = value
	}
	if err := reader.Done(); err != nil {
		return nil, fmt.Errorf("urmessage: an invite with octets after its last field: %w", err)
	}
	if err := invite.check(); err != nil {
		return nil, err
	}
	return invite, nil
}
