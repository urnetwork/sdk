package sdk

import (
	"bytes"
	"time"

	"github.com/urnetwork/connect"
)

// extender_network_key.go — a key of the network's own when the operator refuses
// this installation's extender key as another network's (EXTENDER.md C2c).
//
// A key belongs to the network whose activation first stored it, and the key
// stays on the device when the account changes (B1, E7). So a device signed in
// to another account, one signed in to a new account after the old one was
// deleted, and a state directory copied to a host under another account each
// post a key the operator refuses at every post. On that refusal the role asks
// the space for a key of the network's own, which the space makes once, keeps
// in `.extender_network_keys` under the network's id, and presents whenever the
// role runs for that network. `.extender_key` is left as it was, so a device
// that goes back to the account that holds it presents it again, with its
// history. The new key is a new extender with no history, as one a person makes
// by removing `.extender_key` is.
//
// A space with no local state has nowhere to keep a second key, so it makes
// none: its embedder keeps the identity (the sn miner's `.provider.extender.key`)
// and replacing it is the embedder's, as before.

// The least time between two keys made for one network, so that no run of the
// operator's answers can make an installation churn through keys; a refusal of
// the network's key within that time stands in the status.
const extenderNetworkKeyRenewTimeout = 24 * time.Hour

// The network a client jwt belongs to, read without verification -- it is this
// client's own token, and the operator verifies it. Zero when it names none.
func extenderKeyNetworkIdOfJwt(byJwt string) connect.Id {
	return parseExtenderReleaseIdentity(byJwt).networkId
}

// The network the provider extender role last ran for, zero before one has.
func (self *NetworkSpace) currentExtenderKeyNetworkId() connect.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.extenderKeyNetworkId
}

// Names the network the provider extender role runs for, so the space presents
// that network's key: the one it made for the network when it made one, else
// `.extender_key`. A member node built on the key presented before is rebuilt on
// this one, since the mesh peer id is derived from it (B1).
func (self *NetworkSpace) setExtenderKeyNetwork(networkId connect.Id) {
	before := self.extenderIdentityKeySeed()
	changed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.extenderKeyNetworkId == networkId {
			return false
		}
		self.extenderKeyNetworkId = networkId
		return true
	}()
	if !changed {
		return
	}
	if after := self.extenderIdentityKeySeed(); !bytes.Equal(before, after) {
		self.rebuildExtenderMemberNode()
	}
}

// Makes a key of the network's own after the operator refused refusedPublicKey
// as another network's (C2c), keeps it for the network and returns its seed.
// Nil when the space makes none: it keeps no local state, the jwt named no
// network, the key the space presents for the network is no longer the one
// refused (a key was made since the post), or the space made the network a key
// less than a day ago (extenderNetworkKeyRenewTimeout), whose refusal then
// stands.
func (self *NetworkSpace) renewExtenderKeyForNetwork(
	networkId connect.Id,
	refusedPublicKey []byte,
	now time.Time,
) []byte {
	if self.asyncLocalState == nil || networkId == (connect.Id{}) {
		return nil
	}
	localState := self.asyncLocalState.GetLocalState()

	// the key the space presents for the network now
	presentedSeed, createTime, made := localState.getExtenderNetworkKeySeed(networkId)
	if !made {
		var err error
		presentedSeed, err = localState.GetOrCreateExtenderKeySeed()
		if err != nil {
			self.logger().Infof("[extender]identity key err = %s\n", err)
			return nil
		}
	}
	presentedPublicKey, err := connect.ExtenderPublicKeyFromSeed(presentedSeed)
	if err != nil || !bytes.Equal(presentedPublicKey, refusedPublicKey) {
		return nil
	}
	if made && now.Before(createTime.Add(extenderNetworkKeyRenewTimeout)) {
		return nil
	}

	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		self.logger().Infof("[extender]identity key err = %s\n", err)
		return nil
	}
	if err := localState.setExtenderNetworkKeySeed(networkId, seed, now); err != nil {
		self.logger().Infof("[extender]identity key err = %s\n", err)
		return nil
	}
	if self.currentExtenderKeyNetworkId() == networkId {
		self.rebuildExtenderMemberNode()
	}
	return seed
}
