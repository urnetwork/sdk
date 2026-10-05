package urmessage

import (
	"fmt"
	"sync"

	"github.com/urnetwork/connect/mls"
)

// MemoryStateStore is [mls.StateStore] in three maps.
//
// IT PERSISTS NOTHING AND THE NAME IS THE WHOLE WARNING. Close the process and every group this
// device is a member of is gone: the MLS state, the init and encryption private keys, and the key
// packages it published. There is no recovery from that in the alpha, because recovery is out of
// scope; what a device does instead is create or join again.
//
// WHY IT IS HERE AT ALL. `connect/mls` publishes the StateStore INTERFACE and ships no
// implementation of it, and the only implementation in the corpus is `messagegroup`'s own
// `memoryStateStore`, which is declared in a _test.go file and is therefore unreachable from every
// build. A seam that could not construct an engine would be a seam nobody could call, so this is
// the door -- and it is deliberately the simplest thing that satisfies the interface rather than a
// durable store that looks like one and is not tested as one.
//
// THE DURABLE HALF THAT DOES EXIST IS THE STREAM INDEX RESERVER, and the asymmetry is the right
// way round. A lost MLS group costs a re-join; a reused stream index is a reused nonce under a
// reused record key, which spec A §5.6 calls "a total break of both AEADs for that record". So the
// reserver is [sdk.StreamStore], crash safe and fsync'd, and this is a map.
type MemoryStateStore struct {
	lock        sync.Mutex
	groupStates map[string][]byte
	privateKeys map[string][]byte
	keyPackages map[string][3][]byte
}

// NewMemoryStateStore opens an empty store.
func NewMemoryStateStore() *MemoryStateStore {
	return &MemoryStateStore{
		groupStates: map[string][]byte{},
		privateKeys: map[string][]byte{},
		keyPackages: map[string][3][]byte{},
	}
}

var _ mls.StateStore = (*MemoryStateStore)(nil)

func (self *MemoryStateStore) PutGroupState(groupId []byte, epoch uint64, state []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.groupStates[fmt.Sprintf("%x/%d", groupId, epoch)] = append([]byte(nil), state...)
	return nil
}

func (self *MemoryStateStore) GetGroupState(groupId []byte, epoch uint64) ([]byte, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	state, held := self.groupStates[fmt.Sprintf("%x/%d", groupId, epoch)]
	if !held {
		// the SAME sentinel the durable store answers, because the walk branches on it: a
		// record from an epoch this device holds no state for is a gap, and a store that
		// answered a bare error for that would make it a failure retried three times instead.
		return nil, fmt.Errorf("%w: no mls group state for %x at epoch %d", ErrStateNotFound, groupId, epoch)
	}
	return append([]byte(nil), state...), nil
}

func (self *MemoryStateStore) DeleteGroupStateBefore(groupId []byte, epoch uint64) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	for at := uint64(0); at < epoch; at += 1 {
		delete(self.groupStates, fmt.Sprintf("%x/%d", groupId, at))
	}
	return nil
}

func (self *MemoryStateStore) PutPrivateKey(pub []byte, priv []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.privateKeys[fmt.Sprintf("%x", pub)] = append([]byte(nil), priv...)
	return nil
}

func (self *MemoryStateStore) GetPrivateKey(pub []byte) ([]byte, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	priv, held := self.privateKeys[fmt.Sprintf("%x", pub)]
	if !held {
		return nil, fmt.Errorf("urmessage: no mls private key for %x", pub)
	}
	return append([]byte(nil), priv...), nil
}

func (self *MemoryStateStore) DeletePrivateKey(pub []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	delete(self.privateKeys, fmt.Sprintf("%x", pub))
	return nil
}

func (self *MemoryStateStore) PutKeyPackage(ref []byte, kp []byte, initPriv []byte, encPriv []byte) error {
	self.lock.Lock()
	defer self.lock.Unlock()
	self.keyPackages[fmt.Sprintf("%x", ref)] = [3][]byte{
		append([]byte(nil), kp...), append([]byte(nil), initPriv...), append([]byte(nil), encPriv...),
	}
	return nil
}

// TakeKeyPackage is DESTRUCTIVE, which is the interface's contract and not this store's choice: a
// key package is single use, and a second join off one published package is a second device
// deriving the same init secret.
func (self *MemoryStateStore) TakeKeyPackage(ref []byte) ([]byte, []byte, []byte, error) {
	self.lock.Lock()
	defer self.lock.Unlock()
	held, isHeld := self.keyPackages[fmt.Sprintf("%x", ref)]
	if !isHeld {
		return nil, nil, nil, fmt.Errorf("urmessage: no mls key package for %x", ref)
	}
	delete(self.keyPackages, fmt.Sprintf("%x", ref))
	return held[0], held[1], held[2], nil
}
