package sdk

import (
	"github.com/urnetwork/connect/v2026"
)

// extender_reset.go — the "Reset extenders" action of the account screen's
// Extenders section (EXTENDER.md E7).
//
// Extender knowledge is per installation, not per account, so a sign-out keeps
// it; one local account can therefore poison what another dials, and the reset
// is the manual way back to a fresh install's state.
//
// A reset is named by an id, a ULID, which travels with the space's values.
// The process the user pressed it in mints the id, clears the user-added
// extender values and persists them with the id; every other process that
// holds the space -- the ios and macos tunnel extension, the windows service,
// the linux daemon -- applies the reset when the id reaches it, by the device
// rpc or the desktop control verb at once, or by the next import of the space
// at the latest. A process applies a reset only when its id is newer than the
// latest one its own storage applied, so each reset applies once in each
// process whatever the paths it arrives by, and an older one carried by
// another user's space on a shared daemon applies nothing.

// ResetExtenders returns this space's extender state to a fresh install's and
// returns the id of the reset, which another process holding the space applies
// with ApplyExtenderReset. Everything the space learned goes -- the records and
// addresses of the feed, the mesh, the dns bootstrap and shares, the holds,
// limits and failure history, the latency samples, the continent hint, the
// operator's last country and the root keys a hello installed -- and so does
// everything a user added: the manual hosts, the legacy private extender, and
// the dns name, gossip url and root keys a user or an import set, which go back
// to their derived and bundled defaults. The values are persisted through the
// manager with the id. The network client and node restart and relearn as on a
// first run. The space, its devices and its view controllers stay valid, and
// a live extender path keeps running (restartExtenderNetwork).
//
// What stays is not knowledge of other extenders: this installation's own
// extender identity (`.extender_key`), the gossip mode, the provider extender
// setting and the bootstrap DoH servers, which are settings of their own.
func (self *NetworkSpace) ResetExtenders() string {
	resetId := connect.NewId().String()
	// A reset pressed here applies whatever the ids say: a clock set back
	// since the last one would mint an older id, and the press would be
	// skipped. Forgetting the applied reset makes any id newer.
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.appliedExtenderResetId = ""
	}()
	self.updateInPlaceValues(func(values *NetworkSpaceValues) {
		resetExtenderValues(values, resetId)
	})
	return resetId
}

// ApplyExtenderReset applies a reset made in another process that holds this
// space, named by the id ResetExtenders returned there: the extender values a
// reset clears are cleared here too, with the id, and the directory is reset
// as it was there. A reset this space has applied already, or an older one,
// changes nothing, which is what lets the id arrive by every path and apply
// once. Returns whether the reset was new to this space.
func (self *NetworkSpace) ApplyExtenderReset(resetId string) bool {
	appliedResetId := func() string {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.appliedExtenderResetId
	}()
	if !extenderResetIdNewer(resetId, appliedResetId) {
		return false
	}
	self.updateInPlaceValues(func(values *NetworkSpaceValues) {
		resetExtenderValues(values, resetId)
	})
	return true
}

// The id of the latest reset of this space's extender state (E7), empty for
// a space never reset.
func (self *NetworkSpace) GetExtenderResetId() string {
	return self.valuesCopy().ExtenderResetId
}

// Applies the reset the values were built with when this space's storage has
// not applied it yet: the import of a space reset in another process -- the
// app's, in the tunnel process; the desktop app's, in its service. Runs in the
// constructor, before the network client and the node exist and before any
// dial, so nothing reads the directory before it is reset. The values are
// taken as they are: a host the user added after the reset is the space's.
func (self *NetworkSpace) applyStoredExtenderReset() {
	appliedResetId := ""
	if self.asyncLocalState != nil {
		appliedResetId = self.asyncLocalState.GetLocalState().getExtenderResetId()
	}
	if extenderResetIdNewer(self.values.ExtenderResetId, appliedResetId) {
		if self.extenderDirectory != nil {
			self.extenderDirectory.Reset(spaceExtenderRootKeySet(&self.key, &self.values))
		}
		appliedResetId = self.values.ExtenderResetId
		if self.asyncLocalState != nil {
			if err := self.asyncLocalState.GetLocalState().setExtenderResetId(appliedResetId); err != nil {
				self.logger().Infof("[extender]reset id err = %s\n", err)
			}
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.appliedExtenderResetId = appliedResetId
}

// Applies a reset the values replacing this space bring, ahead of the rebuild
// that replaces it (NetworkSpaceManager.updateNetworkSpace). The replacement's
// directory loads the store this space's directory writes, so this space must
// not write what the reset clears after the replacement has loaded: its client
// and node, built on the values being replaced, would go on learning -- the
// manual hosts of those values included -- and its close would write it all
// back. They are stopped and not started again, since the space is about to
// close; the directory is reset and written, and the reset recorded, so the
// replacement loads a fresh directory and does not reset it a second time.
func (self *NetworkSpace) applyExtenderResetForRebuild(resetId string) {
	claimed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || !extenderResetIdNewer(resetId, self.appliedExtenderResetId) {
			return false
		}
		self.appliedExtenderResetId = resetId
		return true
	}()
	if !claimed {
		return
	}

	self.extenderRestartLock.Lock()
	defer self.extenderRestartLock.Unlock()

	self.stopExtenderNode()
	if self.extenderDirectory != nil {
		previousNetworkClient := func() *connect.ExtenderNetworkClient {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			previousNetworkClient := self.extenderNetworkClient
			self.extenderNetworkClient = nil
			return previousNetworkClient
		}()
		if previousNetworkClient != nil {
			previousNetworkClient.Close()
		}
		// the replacement installs the anchor of its own values
		self.extenderDirectory.Reset(nil)
	}
	if self.asyncLocalState != nil {
		if err := self.asyncLocalState.GetLocalState().setExtenderResetId(resetId); err != nil {
			self.logger().Infof("[extender]reset id err = %s\n", err)
		}
	}
	self.extenderNodeMonitor.NotifyAll()
	self.extenderStatusChanged()
}

// The values a reset leaves: every extender value a user or an import sets
// back to its default, and the reset's id.
func resetExtenderValues(values *NetworkSpaceValues, resetId string) {
	values.NetExtender = nil
	values.ExtenderDnsName = ""
	values.GossipUrl = ""
	values.ExtenderRootPublicKeys = nil
	values.ExtenderHosts = nil
	values.ExtenderResetId = resetId
}

// Reports whether `resetId` names a reset after `appliedResetId`. Reset ids
// are ULIDs, ordered by when they were minted; an empty or unreadable
// `resetId` is no reset, and an empty or unreadable applied id is older than
// every reset.
func extenderResetIdNewer(resetId string, appliedResetId string) bool {
	if resetId == "" {
		return false
	}
	id, err := connect.ParseId(resetId)
	if err != nil {
		return false
	}
	appliedId, err := connect.ParseId(appliedResetId)
	if err != nil {
		return true
	}
	return appliedId.LessThan(id)
}

// ResetExtenders resets the extender state of this device's network space
// (E7). A hosted device never does: its space is the proxy host's, shared by
// unrelated customers, and none of its extender state is theirs to clear.
func (self *DeviceLocal) ResetExtenders() {
	if self.hostedIncompatibleGuarded("ResetExtenders") {
		return
	}
	self.networkSpace.ResetExtenders()
}

// Applies a reset another process minted to this device's space (E7): what
// the device rpc carries from the app.
func (self *DeviceLocal) applyExtenderReset(resetId string) {
	if self.hostedIncompatibleGuarded("ApplyExtenderReset") {
		return
	}
	if self.networkSpace.ApplyExtenderReset(resetId) {
		self.log.Infof("[device]extenders reset %s\n", resetId)
	}
}
