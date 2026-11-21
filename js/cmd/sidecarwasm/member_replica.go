//go:build js && wasm && sidecar_http

package main

import (
	"bytes"
	"time"

	"github.com/urnetwork/sdk/js/sidecarworker"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// Start the follower once this network has bound its identity. Its published client and
// EVS are withheld until their anchor and full config replay stand.
func (self *pageNetwork) standingReplica() (*sidecar.ConfigClient, *sidecar.EvsClient, error) {
	self.replicaLock.Lock()
	defer self.replicaLock.Unlock()
	if self.closed {
		return nil, nil, sidecar.ErrSidecarClosed
	}
	if network := self.local.NetworkId(); network == nil || *network != self.networkId {
		return nil, nil, sidecar.ErrSidecarNoIdentity
	}
	if self.replica == nil {
		if self.local.Identity() == nil {
			return nil, nil, sidecar.ErrEvsNoConfig
		}
		replica, err := self.local.OpenPageMemberReplica(self.ctx)
		if err != nil {
			return nil, nil, err
		}
		self.replica = replica
	}
	config, evs := self.replica.Config(), self.replica.Evs()
	if config == nil || evs == nil || config.State() == nil {
		return nil, nil, sidecar.ErrEvsNoConfig
	}
	return config, evs, nil
}

func (self *pageNetwork) closeReplica() {
	self.replicaLock.Lock()
	self.closed = true
	replica := self.replica
	self.replica = nil
	self.replicaLock.Unlock()
	if replica != nil {
		replica.Close()
	}
}

// All page reads require this identity's own active, untagged, read-only admission.
// A durable page also needs member consent; a burner proves its separate mailbox share.
func (self *pageNetwork) admittedMember(state *sidecar.ConfigState) *sidecar.AclSidecar {
	identity := self.local.Identity()
	status := self.local.Status()
	if state == nil || identity == nil || identity.NetworkId != self.networkId || state.NetworkId() != self.networkId || !self.local.Admitted() ||
		(status.State != sidecar.SidecarStateOnline && status.State != sidecar.SidecarStateOffline) {
		return nil
	}
	own := state.Sidecar(identity.SidecarId)
	if own == nil || own.Row == nil || own.MemberId == (sidecar.Id{}) || len(own.TagIds) != 0 ||
		own.Ephemeral != self.ephemeral || (!self.ephemeral && own.Row.GetCapabilities()&uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_READ_ONLY) == 0) ||
		own.Row.GetRevokedAt() != 0 || own.Row.GetParkedAt() != 0 || state.Suspended(identity.SidecarId) ||
		!bytes.Equal(state.IdentityKey(identity.SidecarId), identity.IdentityPublicKey) {
		return nil
	}
	acl, err := state.Acl()
	if err != nil || (!self.ephemeral && !acl.ConsentedFor(identity.SidecarId, own.MemberId)) {
		return nil
	}
	return own
}

func (self *pageNetwork) sharedMailbox() (sidecar.Id, bool) {
	config, _, err := self.standingReplica()
	if err != nil || !self.ephemeral {
		return sidecar.Id{}, false
	}
	state := config.State()
	own := self.admittedMember(state)
	if own == nil {
		return sidecar.Id{}, false
	}
	acl, err := state.Acl()
	if err != nil {
		return sidecar.Id{}, false
	}
	identity := self.local.Identity()
	if identity == nil {
		return sidecar.Id{}, false
	}
	for _, id := range acl.RepoIds() {
		repo := acl.Repo(id)
		if repo.GetKind() != protocol.RepoKind_REPO_KIND_MAILBOX || !bytes.Equal(repo.GetOwnerMemberId(), own.MemberId.Bytes()) {
			continue
		}
		read := acl.EvsAccess(sidecar.AclSidecarSubject(identity.SidecarId), id, sidecar.AclTokenAudienceEvs, time.Now())
		if read.Rights.Has(sidecar.AclEvsRead) && read.Reason == sidecar.AclReasonEvsShare {
			return id, true
		}
	}
	return sidecar.Id{}, false
}

func (self *pageNetwork) evsClient() (*sidecar.EvsClient, error) {
	config, evs, err := self.standingReplica()
	if err != nil {
		return nil, err
	}
	if self.admittedMember(config.State()) == nil {
		return nil, sidecar.ErrEvsUnauthorized
	}
	return evs, nil
}

func (self *pageNetwork) records() (sidecarworker.WorkerRecords, error) {
	config, _, err := self.standingReplica()
	if err != nil {
		return nil, err
	}
	if self.ephemeral || self.admittedMember(config.State()) == nil {
		return nil, sidecar.ErrEvsUnauthorized
	}
	record, err := self.local.Record()
	if err != nil {
		return nil, err
	}
	return record.Reader(), nil
}

func (self *pageNetwork) recordReadable(id sidecar.Id) bool {
	config, _, err := self.standingReplica()
	identity := self.local.Identity()
	if err != nil || identity == nil || self.ephemeral {
		return false
	}
	state := config.State()
	own := self.admittedMember(state)
	if own == nil {
		return false
	}
	auth := sidecar.NewRecordAuthView(state)
	if vault, ok := auth.Vault(own.MemberId); ok && vault == id && !auth.Consented(id, identity.SidecarId) {
		return false
	}
	return auth.Reads(id, identity.SidecarId)
}

func (self *pageNetwork) directory() (sidecarworker.WorkerDirectory, error) {
	config, evs, err := self.standingReplica()
	if err != nil {
		return nil, err
	}
	if self.ephemeral || self.admittedMember(config.State()) == nil {
		return nil, sidecar.ErrEvsUnauthorized
	}
	self.replicaLock.Lock()
	defer self.replicaLock.Unlock()
	directory, err := self.local.DirectoryClient()
	if err != nil {
		return nil, err
	}
	if self.cardsConfig != config || self.cardsEvs != evs {
		cards, err := sidecar.NewDirectoryEvsCards(&sidecar.DirectoryEvsCardsArgs{Client: evs})
		if err != nil {
			return nil, err
		}
		directory.Bind(cards, func() sidecar.DirectoryAuthView {
			state := config.State()
			if self.admittedMember(state) == nil {
				return sidecar.NewDirectoryAuthView(nil)
			}
			return sidecar.NewDirectoryAuthView(state)
		})
		self.cardsConfig, self.cardsEvs = config, evs
	}
	return directory, nil
}

func (self *pageNetwork) config() (*sidecarworker.WorkerConfigState, error) {
	client, _, err := self.standingReplica()
	if err != nil {
		return nil, err
	}
	if self.ephemeral || self.admittedMember(client.State()) == nil {
		return nil, sidecar.ErrEvsUnauthorized
	}
	state, head, local, err := client.ReadMemberView(self.ctx)
	if err != nil {
		return nil, err
	}
	if self.admittedMember(state) == nil || self.admittedMember(client.State()) == nil {
		return nil, sidecar.ErrEvsUnauthorized
	}
	return &sidecarworker.WorkerConfigState{State: state, Head: head, Local: local}, nil
}
