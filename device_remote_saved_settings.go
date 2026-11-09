// The kill switch and the split rules an app sets through its DeviceRemote,
// kept in the app's own local state and queued for every device the remote
// syncs with.
//
// The device a remote controls does not keep them. The windows service and the
// linux daemon build a new DeviceLocal for each tunnel session, and again after
// a restart, and neither loads or auto saves that device's preferences: since
// the preference catalog (c46108dd) a device does that only when its host calls
// DeviceLocal.Load and SetAutoSave. So the split rules such a device was told
// over the rpc ended with it, and so did route local where the app did not set
// it again (windows): the next session came up with no split rules, and on
// windows with route local on, which is the kill switch off for every packet
// the tunnel cannot carry, while the apps still showed both as the user left
// them (support inbox 8227, and 8200 for the kill switch). The ios extension
// keeps its own copy, but in a container the app cannot read, so the ios app
// keeps a copy of the split rules and queues it and the saved route local for
// each new remote itself (apple BlockActionsStore and
// DeviceManager.initDevice), as the linux app does for route local.
//
// The app is the one author of both: a device changes them only through the
// rpc setters the app drives. So the remote keeps the app's record and replays
// it, for every app:
//
//   - At construction it queues the saved route local and split rules for the
//     device, as writes made while the rpc is down: the first sync applies them
//     over whatever the device came up with, and the getters answer with them
//     until it does. Nothing saved queues nothing, and the device's own values
//     stand.
//   - It saves the split rules whenever it learns the whole list the device
//     holds or is about to hold: the device's list, reported or read over the
//     rpc, a whole list the app set, and an edit of a list the remote knew,
//     applied or queued for the next sync. A device's list that arrives while
//     an edit is queued predates the edit, which the next sync applies over
//     it, and is not saved. An edit of a list the remote never saw is not the
//     whole list, and is not saved either: saving it would replace the
//     device's rules with it at the next remote's first sync. Route local is
//     not saved here: every app saves its kill switch to its local state when
//     the user sets it (LocalState.SetRouteLocal).
//
// The saves go to this process's local state only, in the order the remote
// learned the lists, off the caller's goroutine (AsyncLocalState), and one made
// after a sign-out is dropped (local_state_sign_in.go), so the next account
// never comes up with the previous one's rules. A remote to a hosted device
// (DisableHostedIncompatible) neither queues nor saves: its rules are the
// hosted device's, not this app's.
package sdk

import (
	"sync"
)

// Where a split rule list the remote learned came from.
type deviceRemoteBlockActionOverridesSource int

const (
	// the device's list, reported or read over the rpc
	deviceRemoteBlockActionOverridesReported deviceRemoteBlockActionOverridesSource = iota
	// a whole list the app set
	deviceRemoteBlockActionOverridesSet
	// the app's edit of one rule, applied to the list the remote last knew
	deviceRemoteBlockActionOverridesEdited
)

// The split rule saves of one remote.
type deviceRemoteSavedBlockActionOverrides struct {
	// the remote has a whole list: one it queued at construction, the
	// device's, or one the app set. Guarded by DeviceRemote.stateLock
	known bool
	// the revision of the latest list learned. Guarded by
	// DeviceRemote.stateLock
	learned uint64

	// the revision of the latest list saved, so a save that runs after a
	// newer one never replaces it
	saveLock sync.Mutex
	saved    uint64
}

// The app's local state, when this remote keeps the app's settings in it.
func (self *DeviceRemote) savedSettingsLocalState() *AsyncLocalState {
	if self.settings == nil || self.settings.DisableHostedIncompatible || self.networkSpace == nil {
		return nil
	}
	return self.networkSpace.GetAsyncLocalState()
}

// Queues the saved route local and split rules for the device. The constructor
// calls this before the run loop starts, so it sets the state without
// stateLock, as the log verbosity restore does.
func (self *DeviceRemote) queueSavedSettings() {
	asyncLocalState := self.savedSettingsLocalState()
	if asyncLocalState == nil {
		return
	}
	localState := asyncLocalState.GetLocalState()
	if routeLocal, ok := localState.routeLocalIfSet(); ok {
		self.state.RouteLocal.Set(routeLocal)
		self.lastKnownState.RouteLocal.Set(routeLocal)
	}
	// nil is never saved or unreadable, which leaves the device's rules alone.
	// An empty list is a real value, the last rule removed, and clears them
	if overrides := localState.GetBlockActionOverrides(); overrides != nil {
		overridesRpc := newBlockActionOverridesRpc(overrides)
		self.state.BlockActionOverrides.Set(overridesRpc)
		self.lastKnownState.BlockActionOverrides.Set(overridesRpc)
		self.savedBlockActionOverrides.known = true
	}
}

// Keeps a split rule list the remote learned as last known, and returns its
// save, to run once stateLock is released, or nil when it is not saved (see
// the file header). Must be called with stateLock.
func (self *DeviceRemote) learnBlockActionOverridesWithLock(
	overridesRpc []*BlockActionOverrideRpc,
	source deviceRemoteBlockActionOverridesSource,
) func() {
	self.lastKnownState.BlockActionOverrides.Set(overridesRpc)
	saves := &self.savedBlockActionOverrides
	switch source {
	case deviceRemoteBlockActionOverridesReported:
		if self.state.BlockActionOverrides.IsSet {
			// made before the device had the queued edit
			return nil
		}
		saves.known = true
	case deviceRemoteBlockActionOverridesSet:
		saves.known = true
	case deviceRemoteBlockActionOverridesEdited:
		if !saves.known {
			return nil
		}
	}
	asyncLocalState := self.savedSettingsLocalState()
	if asyncLocalState == nil {
		return nil
	}
	saves.learned += 1
	revision := saves.learned
	// the save's own copy, made now: the rpc values stay with the state
	overrides := toBlockActionOverrideList(overridesRpc)
	return func() {
		self.saveBlockActionOverrides(asyncLocalState, revision, overrides)
	}
}

// Saves the split rules the next remote queues, after every save queued before
// it. Never called with stateLock.
func (self *DeviceRemote) saveBlockActionOverrides(
	asyncLocalState *AsyncLocalState,
	revision uint64,
	overrides *BlockActionOverrideList,
) {
	// the sign-in the list belongs to: a sign-out before the save runs drops it
	signIn := asyncLocalState.GetLocalState().currentSignIn()
	asyncLocalState.serialAsync(func() error {
		saves := &self.savedBlockActionOverrides
		saves.saveLock.Lock()
		defer saves.saveLock.Unlock()
		if revision <= saves.saved {
			// a newer list was saved first
			return nil
		}
		saved, err := signIn.write(func(localState *LocalState) error {
			return localState.saveCatalogPreferenceWithLock("block-action-overrides", overrides)
		})
		if err != nil {
			self.log.Warningf("[dr]save split rules: %v", err)
			return err
		}
		if saved {
			saves.saved = revision
		}
		return nil
	})
}
