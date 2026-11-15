//go:build !js

package sdk

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// How a test's forwarding key store answers.
type testingForwardKeyStoreMode int

const (
	testingForwardKeyStoreWorking testingForwardKeyStoreMode = iota
	testingForwardKeyStoreLocked
	testingForwardKeyStoreFailing
)

// An in-memory SidecarForwardKeyStore, as an extension's keychain items answer: every call it took is countable.
type testingForwardKeyStore struct {
	stateLock sync.Mutex
	mode      testingForwardKeyStoreMode
	items     map[string]string
	writes    int
}

func newTestingForwardKeyStore() *testingForwardKeyStore {
	return &testingForwardKeyStore{items: map[string]string{}}
}

func (self *testingForwardKeyStore) setMode(mode testingForwardKeyStoreMode) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.mode = mode
}

func (self *testingForwardKeyStore) PutSecret(name string, valueBase64 string) int32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch self.mode {
	case testingForwardKeyStoreLocked:
		return SidecarForwardKeyStoreLocked
	case testingForwardKeyStoreFailing:
		return SidecarForwardKeyStoreFailed
	}
	self.items[name] = valueBase64
	self.writes += 1
	return SidecarForwardKeyStoreOk
}

func (self *testingForwardKeyStore) GetSecret(name string) *SidecarForwardSecret {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch self.mode {
	case testingForwardKeyStoreLocked:
		return &SidecarForwardSecret{Status: SidecarForwardSecretLocked}
	case testingForwardKeyStoreFailing:
		return &SidecarForwardSecret{Status: SidecarForwardSecretFailed}
	}
	value, ok := self.items[name]
	if !ok {
		return &SidecarForwardSecret{Status: SidecarForwardSecretAbsent}
	}
	return &SidecarForwardSecret{Status: SidecarForwardSecretPresent, ValueBase64: value}
}

func (self *testingForwardKeyStore) DeleteSecret(name string) int32 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch self.mode {
	case testingForwardKeyStoreLocked:
		return SidecarForwardKeyStoreLocked
	case testingForwardKeyStoreFailing:
		return SidecarForwardKeyStoreFailed
	}
	delete(self.items, name)
	return SidecarForwardKeyStoreOk
}

// The item's bytes, nil for none.
func (self *testingForwardKeyStore) value(t *testing.T, name string) []byte {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	encoded, ok := self.items[name]
	if !ok {
		return nil
	}
	value, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// The names held, and the writes taken.
func (self *testingForwardKeyStore) names() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	names := []string{}
	for name := range self.items {
		names = append(names, name)
	}
	return names
}

// A sidecar's identity, which signs its forwarding Delegations in these tests.
type testingForwardIdentity struct {
	sidecarId  connect.Id
	privateKey ed25519.PrivateKey
}

func newTestingForwardIdentity(t *testing.T) *testingForwardIdentity {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &testingForwardIdentity{sidecarId: connect.NewId(), privateKey: privateKey}
}

// A Pending for networkId and genesis naming this identity (QD-211's fields).
func (self *testingForwardIdentity) pending(networkId connect.Id, genesisSha256 []byte) *bridge.Pending {
	return &bridge.Pending{
		NetworkId:         networkId.Bytes(),
		GenesisSha256:     bytes.Clone(genesisSha256),
		SidecarId:         self.sidecarId.Bytes(),
		IdentityPublicKey: self.privateKey.Public().(ed25519.PublicKey),
	}
}

// What a test changes in a hand-off it builds.
type testingForwardHandOffChange struct {
	forwardPublicKey     []byte
	readerXwingPublicKey []byte
	forwardSidecarId     []byte
	hostClientId         []byte
	hostClientPublicKey  []byte
	signer               ed25519.PrivateKey
	signerId             []byte
}

// The hand-off a sidecar role would answer keys with (DESIGN-DETAILS 4.2.6's S1 to S6): a Delegation naming the keys,
// signed by this identity, F's token, and the anchor; change alters what it names.
func (self *testingForwardIdentity) handOff(t *testing.T, keys *bridge.ForwardingKeys, pending *bridge.Pending, change *testingForwardHandOffChange) *bridge.ForwardingHandOff {
	t.Helper()
	if change == nil {
		change = &testingForwardHandOffChange{}
	}
	pick := func(changed []byte, value []byte) []byte {
		if changed != nil {
			return changed
		}
		return value
	}
	forward, err := proto.Marshal(&bridge.ForwardScope{
		ForwardSidecarId:    pick(change.forwardSidecarId, self.sidecarId.Bytes()),
		HostClientId:        pick(change.hostClientId, keys.GetHostClientId()),
		HostClientPublicKey: pick(change.hostClientPublicKey, keys.GetHostClientPublicKey()),
	})
	if err != nil {
		t.Fatal(err)
	}
	body := protowire.AppendTag(nil, 2, protowire.BytesType)
	body = protowire.AppendBytes(body, pick(change.forwardPublicKey, keys.GetForwardPublicKey()))
	body = protowire.AppendTag(body, 3, protowire.BytesType)
	body = protowire.AppendBytes(body, pick(change.readerXwingPublicKey, keys.GetReaderXwingPublicKey()))
	body = protowire.AppendTag(body, 7, protowire.BytesType)
	body = protowire.AppendBytes(body, forward)
	purpose, _ := sidecar.ConfigStatementPurpose(bridge.SignedType_SIGNED_DELEGATION)
	signer := self.privateKey
	if change.signer != nil {
		signer = change.signer
	}
	signed, err := proto.Marshal(&bridge.Signed{
		Type:      uint32(bridge.SignedType_SIGNED_DELEGATION),
		Body:      body,
		SignerId:  pick(change.signerId, self.sidecarId.Bytes()),
		Signature: ed25519.Sign(signer, bridge.SignedBytes(purpose, bridge.SignedType_SIGNED_DELEGATION, body)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &bridge.ForwardingHandOff{
		Pending:          pending,
		KeySetId:         keys.GetKeySetId(),
		ForwardClientId:  connect.NewId().Bytes(),
		ForwardClientJwt: testingForwardJwt(t, time.Now().Unix(), 7*24*60*60),
		StrictExpMs:      1,
		ConfigRepoId:     connect.NewId().Bytes(),
		ConfigGroupId:    connect.NewId().Bytes(),
		SignedDelegation: signed,
		DelegationAt:     &bridge.AuthPin{Version: 4, Chain: make([]byte, sha256.Size)},
	}
}

// A forwarding client's token issued at the epoch plus iat s, expiring exp s after it: a synthetic jwt nothing checks.
func testingForwardJwt(t *testing.T, iat int64, lifetime int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"iat":` + strconv.FormatInt(iat, 10) + `,"exp":` + strconv.FormatInt(iat+lifetime, 10) + `}`))
	return header + "." + claims + ".synthetic"
}

// The keys a role answers are public halves alone: no seed of the store's is in them, a repeat answers the same set,
// and every item the role keeps is named fwd.<net8>. (KeysStayPut).
func TestForwardKeysNeverCross(t *testing.T) {
	store := newTestingForwardKeyStore()
	networkId := connect.NewId()
	keys := newDeviceForwardKeys(&deviceForwardSecrets{store: store}, networkId)
	set, err := keys.generate()
	if err != nil {
		t.Fatal(err)
	}
	public, err := set.publicKeys(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(public.GetForwardPublicKey()) != ed25519.PublicKeySize || len(public.GetReaderXwingPublicKey()) != 1216 ||
		len(public.GetClientPublicKey()) != ed25519.PublicKeySize || len(public.GetKeySetId()) != 16 {
		t.Fatalf("public halves %d, %d, %d, %d bytes", len(public.GetForwardPublicKey()), len(public.GetReaderXwingPublicKey()),
			len(public.GetClientPublicKey()), len(public.GetKeySetId()))
	}
	encoded, err := proto.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "fwd." + hex.EncodeToString(networkId[:8]) + "."
	for _, name := range store.names() {
		if !strings.HasPrefix(name, prefix) {
			t.Errorf("an item outside the network's names: %q", name)
		}
		value := store.value(t, name)
		if 16 < len(value) && bytes.Contains(encoded, value) {
			t.Errorf("the keys answered carry %s", name)
		}
	}
	for _, item := range []string{"next.sign", "next.reader", "next.client", "root"} {
		if store.value(t, prefix+item) == nil {
			t.Errorf("%s was not kept", item)
		}
	}
	again, err := keys.generate()
	if err != nil || again.keySetId != set.keySetId {
		t.Errorf("a repeat while next is unpromoted made another set: %v", err)
	}
	if _, _, ok := keys.Delegation(); ok {
		t.Error("a set never handed off signs")
	}
	if readerKeys, err := keys.ReaderKeys(); err != nil || len(readerKeys) != 0 {
		t.Errorf("a set never handed off opens wraps: %d, %v", len(readerKeys), err)
	}
}

// Keys are made only where the store answers Absent: a locked or failing store makes nothing and resets nothing (§5.9).
func TestForwardKeysMadeOnlyWhenTheStoreAnswersAbsent(t *testing.T) {
	store := newTestingForwardKeyStore()
	keys := newDeviceForwardKeys(&deviceForwardSecrets{store: store}, connect.NewId())
	store.setMode(testingForwardKeyStoreLocked)
	if _, err := keys.generate(); !errors.Is(err, errDeviceForwardKeysLocked) {
		t.Errorf("a locked store answered %v", err)
	}
	store.setMode(testingForwardKeyStoreFailing)
	if _, err := keys.generate(); !errors.Is(err, errDeviceForwardKeysFailed) {
		t.Errorf("a failing store answered %v", err)
	}
	if store.writes != 0 {
		t.Errorf("%d writes on a store that could not answer", store.writes)
	}
	store.setMode(testingForwardKeyStoreWorking)
	if _, err := keys.generate(); err != nil {
		t.Fatal(err)
	}
	// an id record whose seed went is a store that lost what it held, which reads failed and is never made again
	store.DeleteSecret(keys.slotName(deviceForwardSlotNext, "reader"))
	if _, status := keys.loadSet(deviceForwardSlotNext); status != deviceForwardSecretFailed {
		t.Errorf("a set missing its reader seed reads %v", status)
	}
	writes := store.writes
	if _, err := keys.generate(); !errors.Is(err, errDeviceForwardKeysFailed) || store.writes != writes {
		t.Errorf("a damaged set was made again: %v", err)
	}
	// a store that answers nonsense reads failed, never absent
	adapter := &deviceForwardSecrets{store: &testingPanickingForwardKeyStore{}}
	if _, status := adapter.get("any"); status != deviceForwardSecretFailed {
		t.Errorf("a panicking store reads %v", status)
	}
	if err := adapter.put("any", []byte{1}); !errors.Is(err, errDeviceForwardKeysFailed) {
		t.Errorf("a panicking store's write answered %v", err)
	}
}

// A store that panics across the binding.
type testingPanickingForwardKeyStore struct{}

func (self *testingPanickingForwardKeyStore) PutSecret(name string, valueBase64 string) int32 {
	panic("synthetic store panic")
}

func (self *testingPanickingForwardKeyStore) GetSecret(name string) *SidecarForwardSecret {
	panic("synthetic store panic")
}

func (self *testingPanickingForwardKeyStore) DeleteSecret(name string) int32 {
	panic("synthetic store panic")
}

// C8 promotes only the handed-off set, and a kill inside a promotion leaves next whole, so the next look finishes it.
func TestForwardPromotionSurvivesAKill(t *testing.T) {
	store := newTestingForwardKeyStore()
	keys := newDeviceForwardKeys(&deviceForwardSecrets{store: store}, connect.NewId())
	set, err := keys.generate()
	if err != nil {
		t.Fatal(err)
	}
	delegationId := sha256.Sum256([]byte("synthetic delegation"))
	if promoted, err := keys.promote(delegationId[:]); promoted || err != nil {
		t.Errorf("a set never handed off was promoted: %v", err)
	}
	if err := keys.recordDelegation(make([]byte, 16), delegationId); !errors.Is(err, errDeviceForwardHandOffRefused) {
		t.Errorf("a hand-off naming another key set was recorded: %v", err)
	}
	if err := keys.recordDelegation(set.keySetId[:], delegationId); err != nil {
		t.Fatal(err)
	}
	other := sha256.Sum256([]byte("another delegation"))
	if promoted, _ := keys.promote(other[:]); promoted {
		t.Error("another Delegation's state promoted the set")
	}
	clientSet, err := keys.clientSet()
	if err != nil || clientSet == nil || clientSet.keySetId != set.keySetId {
		t.Errorf("between the hand-off and the promotion F's key is not next's: %v", err)
	}

	// between the hand-off and the promotion nothing is current, however often it is read
	if _, _, ok := keys.Delegation(); ok {
		t.Fatal("a handed-off set was promoted before its state")
	}
	if _, status := keys.loadSet(deviceForwardSlotNext); status != deviceForwardSecretPresent {
		t.Fatal("next left before its promotion")
	}

	// a kill inside the promotion: the marker written, current's id deleted and one seed written
	store.PutSecret(keys.name("promoting"), base64.StdEncoding.EncodeToString(set.keySetId[:]))
	store.DeleteSecret(keys.slotName(deviceForwardSlotCurrent, "id"))
	store.PutSecret(keys.slotName(deviceForwardSlotCurrent, "sign"), base64.StdEncoding.EncodeToString(set.signSeed))
	delegation, publicKey, ok := keys.Delegation()
	if !ok || delegation != delegationId || !bytes.Equal(publicKey, set.signKey().Public().(ed25519.PublicKey)) {
		t.Fatal("the interrupted promotion was not finished at the next look")
	}
	if _, status := keys.loadSet(deviceForwardSlotNext); status != deviceForwardSecretAbsent {
		t.Errorf("next stayed after the promotion: %v", status)
	}
	if store.value(t, keys.name("promoting")) != nil {
		t.Error("the promoting marker stayed")
	}
	signature, err := keys.Sign([]byte("message"))
	if err != nil || !ed25519.Verify(publicKey, []byte("message"), signature) {
		t.Errorf("current does not sign: %v", err)
	}
	readerKeys, err := keys.ReaderKeys()
	if err != nil || len(readerKeys) != 1 || readerKeys[0].DelegationId != delegationId {
		t.Errorf("reader keys %v, %v", readerKeys, err)
	}
}

// The floor reads none while absent, keeps what a commit raised, and leaves the posture pending while the store cannot
// answer.
func TestForwardFloorRoundTrips(t *testing.T) {
	store := newTestingForwardKeyStore()
	floor := newDeviceForwardKeys(&deviceForwardSecrets{store: store}, connect.NewId()).floor()
	if n, chain, err := floor.Load(); n != 0 || chain != nil || err != nil {
		t.Errorf("an absent floor read %d, %x, %v", n, chain, err)
	}
	chain := sha256.Sum256([]byte("A_9"))
	if err := floor.Commit(9, chain[:]); err != nil {
		t.Fatal(err)
	}
	if n, held, err := floor.Load(); n != 9 || !bytes.Equal(held, chain[:]) || err != nil {
		t.Errorf("the floor read %d, %x, %v", n, held, err)
	}
	store.setMode(testingForwardKeyStoreLocked)
	if _, _, err := floor.Load(); err == nil {
		t.Error("a locked store read a floor")
	}
}
