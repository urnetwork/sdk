//go:build !js

// The VPN component's forwarding keys (DESIGN-DETAILS 4.2.2's key store, 4.2.6's lifecycle; §18.6's Setup, §5.9's
// table; D-SDK-32, owner decision 197; SDK-M8-16): SidecarForwardKeyStore, the platform store the app's VPN component
// implements (the extension's own keychain items on Apple, its own AndroidKeyStore alias on Android, the service's
// sealed store on Windows and Linux), and the key sets kept in it, one network at a time.
//
//	fwd.<net8>.<slot>.sign     the forwarding key's Ed25519 seed, 32 B           slot = current | next
//	fwd.<net8>.<slot>.reader   the X-Wing seed, 32 B, whose public half the forwarding Delegation names
//	fwd.<net8>.<slot>.client   F's ClientKey seed, 32 B
//	fwd.<net8>.<slot>.id       key_set_id, 16 B, then once handed off the delegation_id, 32 B
//	fwd.<net8>.root            the forwarding root key, 32 B random, which seals the store's files
//	fwd.<net8>.token           F's jwt, replaced at each refresh
//	fwd.<net8>.floor           u64be(n) || A_n, 40 B: the highest pin the posture committed
//
// net8 is the network id's first 8 bytes, hex. Keys are generated only at a setup (C0): a store answering anything but
// Present leaves forwarding pending and never resets it (§5.9's table), and only the sidecar's own sign-out wipes them.
// A key set's id record is written last, so a kill while one is generated leaves a set that reads Absent and is made
// again. A promotion (C8) first writes fwd.<net8>.promoting, an item beside 4.2.2's (a reading: the store has no
// transactions, and only it tells a promotion a kill interrupted from a first set not yet promoted), then deletes
// current's id record, writes next's seeds and id into current, deletes next and last the marker, so a kill anywhere
// leaves next whole or current written, and the promotion runs again at the next look while the marker stands.
//
// The private halves stay here: what leaves is ForwardingKeys' public halves, signatures, and the reader keys the
// component's own Forwarder opens DelegationWraps with (sidecar.ForwarderKeys). No key of the sidecar role enters, and
// on Android, where the VPN service shares the app's process until app work gives it one of its own (§18.16), the
// names and the store are this file's alone, apart from the sidecar role's SidecarKeyStore.
//
// The app's store is untrusted across gobind, as SidecarKeyStore is: it answers statuses, never throws, and anything
// else, a panic, a nil secret or a value that does not decode, reads as Failed, never as Absent. Untagged: the tunnel
// links it. Safe for concurrent use.

package sdk

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	sidecardevice "github.com/urnetwork/sidecar/device"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// A store item's state as this file reads it.
type deviceForwardSecretStatus int

const (
	deviceForwardSecretFailed deviceForwardSecretStatus = iota
	deviceForwardSecretPresent
	deviceForwardSecretAbsent
	deviceForwardSecretLocked
)

var (
	// The component's key store answered Locked: nothing was read, written or deleted.
	errDeviceForwardKeysLocked = errors.New("the forwarding key store is locked")
	// The component's key store failed, or answered what it cannot vouch for: nothing is reset on it.
	errDeviceForwardKeysFailed = errors.New("the forwarding key store failed")
	// No forwarding key store was given to this device: forwarding cannot be set up here.
	errDeviceForwardKeysNone = errors.New("this device has no forwarding key store")
	// A hand-off that names other keys, another sidecar, or that does not verify (DESIGN-DETAILS 4.2.6's C7).
	errDeviceForwardHandOffRefused = errors.New("forwarding hand-off refused")
)

// The error for a status that is neither Present nor Absent.
func deviceForwardStatusErr(status deviceForwardSecretStatus) error {
	if status == deviceForwardSecretLocked {
		return errDeviceForwardKeysLocked
	}
	return errDeviceForwardKeysFailed
}

// SidecarForwardKeyStore read so it never throws (the file's comment).
type deviceForwardSecrets struct {
	store SidecarForwardKeyStore
}

// The item under name: its bytes when Present.
func (self *deviceForwardSecrets) get(name string) ([]byte, deviceForwardSecretStatus) {
	if self == nil || self.store == nil {
		return nil, deviceForwardSecretFailed
	}
	var secret *SidecarForwardSecret
	if err := trace(func() { secret = self.store.GetSecret(name) }); err != nil || secret == nil {
		return nil, deviceForwardSecretFailed
	}
	switch secret.Status {
	case SidecarForwardSecretPresent:
		value, err := base64.StdEncoding.DecodeString(secret.ValueBase64)
		if err != nil {
			return nil, deviceForwardSecretFailed
		}
		return value, deviceForwardSecretPresent
	case SidecarForwardSecretAbsent:
		return nil, deviceForwardSecretAbsent
	case SidecarForwardSecretLocked:
		return nil, deviceForwardSecretLocked
	default:
		return nil, deviceForwardSecretFailed
	}
}

// Keeps value under name.
func (self *deviceForwardSecrets) put(name string, value []byte) error {
	if self == nil || self.store == nil {
		return errDeviceForwardKeysNone
	}
	status := SidecarForwardKeyStoreFailed
	if err := trace(func() { status = self.store.PutSecret(name, base64.StdEncoding.EncodeToString(value)) }); err != nil {
		return errDeviceForwardKeysFailed
	}
	return deviceForwardWriteErr(status)
}

// Deletes name; absent is no error.
func (self *deviceForwardSecrets) remove(name string) error {
	if self == nil || self.store == nil {
		return errDeviceForwardKeysNone
	}
	status := SidecarForwardKeyStoreFailed
	if err := trace(func() { status = self.store.DeleteSecret(name) }); err != nil {
		return errDeviceForwardKeysFailed
	}
	return deviceForwardWriteErr(status)
}

// A write's status as an error, nil for Ok.
func deviceForwardWriteErr(status int32) error {
	switch status {
	case SidecarForwardKeyStoreOk:
		return nil
	case SidecarForwardKeyStoreLocked:
		return errDeviceForwardKeysLocked
	default:
		return errDeviceForwardKeysFailed
	}
}

// The byte counts of 4.2.2's items. Format values.
const (
	deviceForwardSeedByteCount     = 32
	deviceForwardKeySetIdByteCount = 16
	deviceForwardRootKeyByteCount  = 32
	deviceForwardFloorByteCount    = 8 + sha256.Size
)

// The key set slots (4.2.6). Format values.
const (
	deviceForwardSlotCurrent = "current"
	deviceForwardSlotNext    = "next"
)

// One key set: its seeds, its id and, once handed off, the forwarding Delegation that names its public halves.
type deviceForwardKeySet struct {
	keySetId     [deviceForwardKeySetIdByteCount]byte
	signSeed     []byte
	readerSeed   []byte
	clientSeed   []byte
	delegationId []byte
}

// The forwarding private key.
func (self *deviceForwardKeySet) signKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(self.signSeed)
}

// The set's public halves (C0's ForwardingKeys), with the host client where this network is the device client's.
func (self *deviceForwardKeySet) publicKeys(hostClientId []byte, hostClientPublicKey []byte) (*bridge.ForwardingKeys, error) {
	readerKey, err := sidecar.LoadGkmDelegationKey(self.readerSeed)
	if err != nil {
		return nil, err
	}
	return &bridge.ForwardingKeys{
		KeySetId:             bytes.Clone(self.keySetId[:]),
		ForwardPublicKey:     self.signKey().Public().(ed25519.PublicKey),
		ReaderXwingPublicKey: readerKey.PublicKey().Bytes(),
		ClientPublicKey:      ed25519.NewKeyFromSeed(self.clientSeed).Public().(ed25519.PublicKey),
		HostClientId:         bytes.Clone(hostClientId),
		HostClientPublicKey:  bytes.Clone(hostClientPublicKey),
	}, nil
}

// One network's key sets and items (the file's comment). Implements sidecar.ForwarderKeys for its Forwarder.
type deviceForwardKeys struct {
	secrets   *deviceForwardSecrets
	networkId connect.Id
	// fwd.<net8>.
	prefix string

	// held across a read-modify-write of the items
	stateLock sync.Mutex
}

var _ sidecar.ForwarderKeys = (*deviceForwardKeys)(nil)

// The key sets of networkId in secrets.
func newDeviceForwardKeys(secrets *deviceForwardSecrets, networkId connect.Id) *deviceForwardKeys {
	return &deviceForwardKeys{
		secrets:   secrets,
		networkId: networkId,
		prefix:    "fwd." + hex.EncodeToString(networkId[:8]) + ".",
	}
}

// An item's name.
func (self *deviceForwardKeys) name(item string) string {
	return self.prefix + item
}

// A slot's item's name.
func (self *deviceForwardKeys) slotName(slot string, item string) string {
	return self.prefix + slot + "." + item
}

// The slot's key set: Present only with its four items whole; Absent where its id record is (a kill while it was made
// leaves seeds and no id); Locked or Failed as the store answers.
func (self *deviceForwardKeys) loadSetWithLock(slot string) (*deviceForwardKeySet, deviceForwardSecretStatus) {
	idValue, status := self.secrets.get(self.slotName(slot, "id"))
	if status != deviceForwardSecretPresent {
		return nil, status
	}
	if len(idValue) != deviceForwardKeySetIdByteCount && len(idValue) != deviceForwardKeySetIdByteCount+sha256.Size {
		return nil, deviceForwardSecretFailed
	}
	set := &deviceForwardKeySet{}
	copy(set.keySetId[:], idValue)
	if len(idValue) == deviceForwardKeySetIdByteCount+sha256.Size {
		set.delegationId = bytes.Clone(idValue[deviceForwardKeySetIdByteCount:])
	}
	for _, item := range []struct {
		name string
		into *[]byte
	}{{"sign", &set.signSeed}, {"reader", &set.readerSeed}, {"client", &set.clientSeed}} {
		value, status := self.secrets.get(self.slotName(slot, item.name))
		switch {
		case status == deviceForwardSecretAbsent:
			// an id record with no seed is no kill's leftover: the store lost what it held
			return nil, deviceForwardSecretFailed
		case status != deviceForwardSecretPresent:
			return nil, status
		case len(value) != deviceForwardSeedByteCount:
			return nil, deviceForwardSecretFailed
		}
		*item.into = value
	}
	return set, deviceForwardSecretPresent
}

// The slot's set, with a promotion a kill interrupted finished first.
func (self *deviceForwardKeys) loadSet(slot string) (*deviceForwardKeySet, deviceForwardSecretStatus) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.finishPromotionWithLock()
	return self.loadSetWithLock(slot)
}

// Finishes a promotion a kill interrupted, which the promoting marker says: next, while it stands, is written into
// current again; once next is gone, current was written whole and only the marker is left.
func (self *deviceForwardKeys) finishPromotionWithLock() {
	if _, status := self.secrets.get(self.name("promoting")); status != deviceForwardSecretPresent {
		return
	}
	next, nextStatus := self.loadSetWithLock(deviceForwardSlotNext)
	switch nextStatus {
	case deviceForwardSecretPresent:
		self.promoteWithLock(next)
	case deviceForwardSecretAbsent:
		self.secrets.remove(self.name("promoting"))
	}
}

// Writes set into slot, its id record last.
func (self *deviceForwardKeys) writeSetWithLock(slot string, set *deviceForwardKeySet) error {
	for _, item := range []struct {
		name  string
		value []byte
	}{{"sign", set.signSeed}, {"reader", set.readerSeed}, {"client", set.clientSeed}} {
		if err := self.secrets.put(self.slotName(slot, item.name), item.value); err != nil {
			return err
		}
	}
	return self.secrets.put(self.slotName(slot, "id"), append(bytes.Clone(set.keySetId[:]), set.delegationId...))
}

// Deletes slot, its id record first.
func (self *deviceForwardKeys) deleteSetWithLock(slot string) error {
	for _, item := range []string{"id", "sign", "reader", "client"} {
		if err := self.secrets.remove(self.slotName(slot, item)); err != nil {
			return err
		}
	}
	return nil
}

// C0: a key set in next under a fresh key_set_id, or the one next holds while it is unpromoted, so a repeat answers
// the same set. The root key is made here too where none is, since a setup is the only time anything is made.
func (self *deviceForwardKeys) generate() (*deviceForwardKeySet, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.finishPromotionWithLock()
	if _, err := self.rootKeyWithLock(true); err != nil {
		return nil, err
	}
	next, status := self.loadSetWithLock(deviceForwardSlotNext)
	switch status {
	case deviceForwardSecretPresent:
		return next, nil
	case deviceForwardSecretAbsent:
	default:
		// a store that cannot answer is never reset (§5.9)
		return nil, deviceForwardStatusErr(status)
	}
	set := &deviceForwardKeySet{
		signSeed:   make([]byte, deviceForwardSeedByteCount),
		readerSeed: make([]byte, deviceForwardSeedByteCount),
		clientSeed: make([]byte, deviceForwardSeedByteCount),
	}
	for _, random := range [][]byte{set.keySetId[:], set.signSeed, set.readerSeed, set.clientSeed} {
		if _, err := rand.Read(random); err != nil {
			return nil, err
		}
	}
	if err := self.writeSetWithLock(deviceForwardSlotNext, set); err != nil {
		return nil, err
	}
	return set, nil
}

// C7's record: next, named keySetId, is the forwarding Delegation delegationId's.
func (self *deviceForwardKeys) recordDelegation(keySetId []byte, delegationId [sha256.Size]byte) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	next, status := self.loadSetWithLock(deviceForwardSlotNext)
	if status != deviceForwardSecretPresent {
		return deviceForwardStatusErr(status)
	}
	if !bytes.Equal(next.keySetId[:], keySetId) {
		return fmt.Errorf("%w: the hand-off names another key set", errDeviceForwardHandOffRefused)
	}
	next.delegationId = bytes.Clone(delegationId[:])
	return self.secrets.put(self.slotName(deviceForwardSlotNext, "id"), append(bytes.Clone(next.keySetId[:]), next.delegationId...))
}

// C8: next becomes current and the old set goes, when next is handed off as delegationId. False when next is not that
// set, already promoted or never handed off.
func (self *deviceForwardKeys) promote(delegationId []byte) (bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	next, status := self.loadSetWithLock(deviceForwardSlotNext)
	if status != deviceForwardSecretPresent || !bytes.Equal(next.delegationId, delegationId) || len(delegationId) != sha256.Size {
		return false, nil
	}
	if err := self.promoteWithLock(next); err != nil {
		return false, err
	}
	return true, nil
}

// next into current under the promoting marker, current's id record deleted first, next deleted and the marker last
// (the file's comment).
func (self *deviceForwardKeys) promoteWithLock(next *deviceForwardKeySet) error {
	if err := self.secrets.put(self.name("promoting"), next.keySetId[:]); err != nil {
		return err
	}
	if err := self.secrets.remove(self.slotName(deviceForwardSlotCurrent, "id")); err != nil {
		return err
	}
	if err := self.writeSetWithLock(deviceForwardSlotCurrent, next); err != nil {
		return err
	}
	if err := self.deleteSetWithLock(deviceForwardSlotNext); err != nil {
		return err
	}
	return self.secrets.remove(self.name("promoting"))
}

// The set F's ClientKey is: next while it is handed off and unpromoted, since F was minted for its client key, else
// current; nil with neither.
func (self *deviceForwardKeys) clientSet() (*deviceForwardKeySet, error) {
	if next, status := self.loadSet(deviceForwardSlotNext); status == deviceForwardSecretPresent && next.delegationId != nil {
		return next, nil
	} else if status != deviceForwardSecretPresent && status != deviceForwardSecretAbsent {
		return nil, deviceForwardStatusErr(status)
	}
	current, status := self.loadSet(deviceForwardSlotCurrent)
	switch status {
	case deviceForwardSecretPresent:
		return current, nil
	case deviceForwardSecretAbsent:
		return nil, nil
	}
	return nil, deviceForwardStatusErr(status)
}

// sidecar.ForwarderKeys: current's Delegation and key, false while no set is current.
func (self *deviceForwardKeys) Delegation() ([sha256.Size]byte, ed25519.PublicKey, bool) {
	current, status := self.loadSet(deviceForwardSlotCurrent)
	if status != deviceForwardSecretPresent || len(current.delegationId) != sha256.Size {
		return [sha256.Size]byte{}, nil, false
	}
	return [sha256.Size]byte(current.delegationId), current.signKey().Public().(ed25519.PublicKey), true
}

// sidecar.ForwarderKeys: signs message with current's forwarding key.
func (self *deviceForwardKeys) Sign(message []byte) ([]byte, error) {
	current, status := self.loadSet(deviceForwardSlotCurrent)
	if status != deviceForwardSecretPresent || len(current.delegationId) != sha256.Size {
		return nil, errors.New("no forwarding key set is current")
	}
	return ed25519.Sign(current.signKey(), message), nil
}

// sidecar.ForwarderKeys: the reader keys handed off, current's first and then next's.
func (self *deviceForwardKeys) ReaderKeys() ([]sidecar.ForwarderReaderKey, error) {
	readerKeys := []sidecar.ForwarderReaderKey{}
	for _, slot := range []string{deviceForwardSlotCurrent, deviceForwardSlotNext} {
		set, status := self.loadSet(slot)
		switch {
		case status == deviceForwardSecretAbsent:
			continue
		case status != deviceForwardSecretPresent:
			return nil, deviceForwardStatusErr(status)
		case len(set.delegationId) != sha256.Size:
			continue
		}
		privateKey, err := sidecar.LoadGkmDelegationKey(set.readerSeed)
		if err != nil {
			return nil, err
		}
		readerKeys = append(readerKeys, sidecar.ForwarderReaderKey{DelegationId: [sha256.Size]byte(set.delegationId), PrivateKey: privateKey})
	}
	return readerKeys, nil
}

// The root key: Present's; made when create and the store answers Absent; else an error, so a store that cannot answer
// leaves the sealed files closed and forwarding pending.
func (self *deviceForwardKeys) rootKey(create bool) ([]byte, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.rootKeyWithLock(create)
}

func (self *deviceForwardKeys) rootKeyWithLock(create bool) ([]byte, error) {
	value, status := self.secrets.get(self.name("root"))
	switch {
	case status == deviceForwardSecretPresent && len(value) == deviceForwardRootKeyByteCount:
		return value, nil
	case status == deviceForwardSecretPresent:
		return nil, errDeviceForwardKeysFailed
	case status == deviceForwardSecretAbsent && create:
		rootKey := make([]byte, deviceForwardRootKeyByteCount)
		if _, err := rand.Read(rootKey); err != nil {
			return nil, err
		}
		if err := self.secrets.put(self.name("root"), rootKey); err != nil {
			return nil, err
		}
		return rootKey, nil
	case status == deviceForwardSecretAbsent:
		return nil, fmt.Errorf("%w: no root key", errDeviceForwardKeysFailed)
	}
	return nil, deviceForwardStatusErr(status)
}

// F's token, "" while none is kept.
func (self *deviceForwardKeys) token() (string, error) {
	value, status := self.secrets.get(self.name("token"))
	switch status {
	case deviceForwardSecretPresent:
		return string(value), nil
	case deviceForwardSecretAbsent:
		return "", nil
	}
	return "", deviceForwardStatusErr(status)
}

// Keeps F's token, before anything presents it (D-CORE-8's order).
func (self *deviceForwardKeys) setToken(byJwt string) error {
	return self.secrets.put(self.name("token"), []byte(byJwt))
}

// The posture's floor (DESIGN-DETAILS 4.2.5's restart row) in fwd.<net8>.floor: none while absent, an error that
// keeps the posture pending while the store cannot answer.
func (self *deviceForwardKeys) floor() *sidecardevice.PostureFloor {
	return &sidecardevice.PostureFloor{
		Load: func() (uint64, []byte, error) {
			value, status := self.secrets.get(self.name("floor"))
			switch {
			case status == deviceForwardSecretAbsent:
				return 0, nil, nil
			case status != deviceForwardSecretPresent:
				return 0, nil, deviceForwardStatusErr(status)
			case len(value) != deviceForwardFloorByteCount:
				return 0, nil, errDeviceForwardKeysFailed
			}
			return binary.BigEndian.Uint64(value[:8]), bytes.Clone(value[8:]), nil
		},
		Commit: func(n uint64, chain []byte) error {
			if len(chain) != sha256.Size {
				return errors.New("a floor's chain is 32 bytes")
			}
			return self.secrets.put(self.name("floor"), append(binary.BigEndian.AppendUint64(nil, n), chain...))
		},
	}
}

// The sidecar's own sign-out (4.2.6's wiped): every item of this network goes, the floor and the root key last, so a
// kill leaves nothing that opens or seals.
func (self *deviceForwardKeys) wipe() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, slot := range []string{deviceForwardSlotNext, deviceForwardSlotCurrent} {
		if err := self.deleteSetWithLock(slot); err != nil {
			return err
		}
	}
	for _, item := range []string{"promoting", "token", "floor", "root"} {
		if err := self.secrets.remove(self.name(item)); err != nil {
			return err
		}
	}
	return nil
}

// C7 (DESIGN-DETAILS 4.2.6): the hand-off's Signed Delegation names exactly next's public keys and, where this network
// is the device client's, this device as host; forward names the Pending's sidecar; it is a Delegation signed by that
// sidecar under the Pending's identity key; and it is next's key set the hand-off names. Answers the Delegation's id.
func checkDeviceForwardHandOff(handOff *bridge.ForwardingHandOff, next *deviceForwardKeySet, hostClientId []byte, hostClientPublicKey []byte) ([sha256.Size]byte, error) {
	refuse := func(reason string) ([sha256.Size]byte, error) {
		return [sha256.Size]byte{}, fmt.Errorf("%w: %s", errDeviceForwardHandOffRefused, reason)
	}
	pending := handOff.GetPending()
	switch {
	case pending == nil || len(pending.GetSidecarId()) != 16 || len(pending.GetIdentityPublicKey()) != ed25519.PublicKeySize:
		return refuse("no anchor with a sidecar and its identity key")
	case next == nil || !bytes.Equal(handOff.GetKeySetId(), next.keySetId[:]):
		return refuse("another key set than the one generated")
	case handOff.GetForwardClientJwt() == "" || len(handOff.GetForwardClientId()) != 16:
		return refuse("no forwarding client")
	}
	signed := &bridge.Signed{}
	if err := proto.Unmarshal(handOff.GetSignedDelegation(), signed); err != nil {
		return refuse("a Delegation that does not decode")
	}
	purpose, _ := sidecar.ConfigStatementPurpose(bridge.SignedType_SIGNED_DELEGATION)
	if err := bridge.VerifySigned(signed, purpose, bridge.SignedType_SIGNED_DELEGATION, bridge.SignedKeyAlgEd25519, pending.GetIdentityPublicKey()); err != nil {
		return refuse("a Delegation the sidecar's identity key did not sign")
	}
	if !bytes.Equal(signed.GetSignerId(), pending.GetSidecarId()) {
		return refuse("a Delegation another sidecar signed")
	}
	fields, err := deviceForwardDelegationFields(signed.GetBody())
	if err != nil {
		return refuse(err.Error())
	}
	publicKeys, err := next.publicKeys(hostClientId, hostClientPublicKey)
	if err != nil {
		return refuse("the key set's public halves")
	}
	switch {
	case !bytes.Equal(fields.pubkey, publicKeys.GetForwardPublicKey()):
		return refuse("another forwarding key")
	case !bytes.Equal(fields.readerXwingPublicKey, publicKeys.GetReaderXwingPublicKey()):
		return refuse("another reader key")
	case fields.forward == nil || !bytes.Equal(fields.forward.GetForwardSidecarId(), pending.GetSidecarId()):
		return refuse("forward names another sidecar")
	case !bytes.Equal(fields.forward.GetHostClientId(), hostClientId) || !bytes.Equal(fields.forward.GetHostClientPublicKey(), hostClientPublicKey):
		return refuse("another host client")
	}
	delegationId, err := sidecar.DelegationId(handOff.GetSignedDelegation())
	if err != nil {
		return refuse("no delegation id")
	}
	return delegationId, nil
}

// What C7 reads of a Delegation's body, by wire, since a forwarding file imports no sidecar/protocol (D-SDK-20).
type deviceForwardDelegationBody struct {
	pubkey               []byte
	readerXwingPublicKey []byte
	forward              *bridge.ForwardScope
}

// config.proto's Delegation fields 2, 3 and 7, the last a bridge.ForwardScope.
func deviceForwardDelegationFields(body []byte) (*deviceForwardDelegationBody, error) {
	fields := &deviceForwardDelegationBody{}
	for 0 < len(body) {
		number, kind, length := protowire.ConsumeTag(body)
		if length < 0 {
			return nil, errors.New("a Delegation that does not decode")
		}
		body = body[length:]
		if kind != protowire.BytesType {
			skipped := protowire.ConsumeFieldValue(number, kind, body)
			if skipped < 0 {
				return nil, errors.New("a Delegation that does not decode")
			}
			body = body[skipped:]
			continue
		}
		value, length := protowire.ConsumeBytes(body)
		if length < 0 {
			return nil, errors.New("a Delegation that does not decode")
		}
		body = body[length:]
		switch number {
		case 2:
			fields.pubkey = bytes.Clone(value)
		case 3:
			fields.readerXwingPublicKey = bytes.Clone(value)
		case 7:
			forward := &bridge.ForwardScope{}
			if err := proto.Unmarshal(value, forward); err != nil {
				return nil, errors.New("a forward scope that does not decode")
			}
			fields.forward = forward
		}
	}
	return fields, nil
}
