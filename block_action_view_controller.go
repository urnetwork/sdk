// view controller for the block/routing activity ui.
// Stores a time window of the most recent routing decisions (block actions),
// gated by an adjustable window duration, along with the latest block stats
// and the local-override app ids derived from the block action overrides.
// All methods are safe for concurrent use.
package sdk

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

const (
	defaultBlockActionWindowDuration = 5 * time.Minute
	// how often the run loop evicts expired block actions
	blockActionEvictInterval = 10 * time.Second
)

type BlockActionsListener interface {
	BlockActionsChanged()
}

type BlockActionStatsListener interface {
	BlockActionStatsChanged()
}

type LocalOverrideAppIdsListener interface {
	LocalOverrideAppIdsChanged()
}

type BlockActionViewController struct {
	ctx    context.Context
	cancel context.CancelFunc
	device Device

	stateLock sync.Mutex

	blockActionWindowChangedSub    Sub
	blockStatsChangedSub           Sub
	blockActionOverridesChangedSub Sub

	windowDuration time.Duration
	// newest last
	blockActions        []*BlockAction
	blockStats          *BlockStats
	localOverrideAppIds *OverrideLocalAppIds

	blockActionsListeners        *connect.CallbackList[BlockActionsListener]
	blockActionStatsListeners    *connect.CallbackList[BlockActionStatsListener]
	localOverrideAppIdsListeners *connect.CallbackList[LocalOverrideAppIdsListener]
}

func newBlockActionViewController(ctx context.Context, device Device) *BlockActionViewController {
	cancelCtx, cancel := context.WithCancel(ctx)

	vc := &BlockActionViewController{
		ctx:    cancelCtx,
		cancel: cancel,
		device: device,

		windowDuration: defaultBlockActionWindowDuration,
		blockActions:   []*BlockAction{},

		blockActionsListeners:        connect.NewCallbackList[BlockActionsListener](),
		blockActionStatsListeners:    connect.NewCallbackList[BlockActionStatsListener](),
		localOverrideAppIdsListeners: connect.NewCallbackList[LocalOverrideAppIdsListener](),
	}
	vc.blockActionWindowChangedSub = device.AddBlockActionWindowChangeListener(vc)
	vc.blockStatsChangedSub = device.AddBlockStatsChangeListener(vc)
	vc.blockActionOverridesChangedSub = device.AddBlockActionOverridesChangeListener(vc)

	// seed the initial state from the device
	vc.BlockActionWindowChanged(device.GetBlockActions())
	vc.BlockStatsChanged(device.GetBlockStats())
	vc.BlockActionOverridesChanged(device.GetBlockActionOverrides())

	go connect.HandleError(vc.run, cancel)

	return vc
}

// run evicts expired block actions on a fixed interval,
// so the window advances even when the device emits no new actions
func (self *BlockActionViewController) run() {
	defer self.cancel()

	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(blockActionEvictInterval):
		}

		evicted := false
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			blockActions := self.trimBlockActionsWithLock(self.blockActions)
			if len(blockActions) != len(self.blockActions) {
				self.blockActions = blockActions
				evicted = true
			}
		}()
		if evicted {
			self.blockActionsChanged()
		}
	}
}

// must be called with `stateLock`.
// returns the block actions inside the window, preserving order
func (self *BlockActionViewController) trimBlockActionsWithLock(blockActions []*BlockAction) []*BlockAction {
	windowStartTime := time.Now().Add(-self.windowDuration).UnixMilli()
	windowBlockActions := []*BlockAction{}
	for _, blockAction := range blockActions {
		if windowStartTime <= blockAction.Time {
			windowBlockActions = append(windowBlockActions, blockAction)
		}
	}
	return windowBlockActions
}

// BlockActionWindowChangeListener
func (self *BlockActionViewController) BlockActionWindowChanged(blockActionWindow *BlockActionWindow) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		blockActions := []*BlockAction{}
		if blockActionWindow != nil && blockActionWindow.BlockActions != nil {
			blockActions = blockActionWindow.BlockActions.getAll()
		}
		self.blockActions = self.trimBlockActionsWithLock(blockActions)
	}()
	self.blockActionsChanged()
}

// BlockStatsChangeListener
func (self *BlockActionViewController) BlockStatsChanged(blockStats *BlockStats) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.blockStats = blockStats
	}()
	self.blockActionStatsChanged()
}

// BlockActionOverridesChangeListener.
// recomputes the derived local-override app ids. An app id is included when
// its override routes local, excluded when its override routes remote.
// Emits only when the derived sets actually changed.
func (self *BlockActionViewController) BlockActionOverridesChanged(blockActionOverrides *BlockActionOverrideList) {
	included := NewStringList()
	excluded := NewStringList()
	// dedup app ids across overrides, first wins
	seenAppIds := map[string]bool{}
	if blockActionOverrides != nil {
		for _, override := range blockActionOverrides.getAll() {
			if override.RouteOverride == nil || override.AppIds == nil {
				continue
			}
			for _, appId := range override.AppIds.getAll() {
				if seenAppIds[appId] {
					continue
				}
				seenAppIds[appId] = true
				if override.RouteOverride.Local {
					included.Add(appId)
				} else {
					excluded.Add(appId)
				}
			}
		}
	}

	// order-insensitive set comparison. both sides are deduped by construction.
	setEqual := func(a []string, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		values := map[string]bool{}
		for _, v := range a {
			values[v] = true
		}
		for _, v := range b {
			if !values[v] {
				return false
			}
		}
		return true
	}

	changed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.localOverrideAppIds == nil ||
			!setEqual(self.localOverrideAppIds.Included.getAll(), included.getAll()) ||
			!setEqual(self.localOverrideAppIds.Excluded.getAll(), excluded.getAll()) {
			self.localOverrideAppIds = &OverrideLocalAppIds{
				Included: included,
				Excluded: excluded,
			}
			changed = true
		}
	}()
	if changed {
		self.localOverrideAppIdsChanged()
	}
}

// returns a snapshot of the block actions inside the window, oldest first
func (self *BlockActionViewController) GetBlockActions() *BlockActionList {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	blockActions := NewBlockActionList()
	blockActions.addAll(self.trimBlockActionsWithLock(self.blockActions)...)
	return blockActions
}

func (self *BlockActionViewController) GetBlockStats() *BlockStats {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.blockStats
}

func (self *BlockActionViewController) GetLocalOverrideAppIds() *OverrideLocalAppIds {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.localOverrideAppIds
}

func (self *BlockActionViewController) SetWindowDurationSeconds(seconds int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.windowDuration = time.Duration(seconds) * time.Second
}

func (self *BlockActionViewController) GetWindowDurationSeconds() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return int(self.windowDuration / time.Second)
}

func (self *BlockActionViewController) blockActionsChanged() {
	for _, listener := range self.blockActionsListeners.Get() {
		connect.HandleError(func() {
			listener.BlockActionsChanged()
		})
	}
}

func (self *BlockActionViewController) blockActionStatsChanged() {
	for _, listener := range self.blockActionStatsListeners.Get() {
		connect.HandleError(func() {
			listener.BlockActionStatsChanged()
		})
	}
}

func (self *BlockActionViewController) localOverrideAppIdsChanged() {
	for _, listener := range self.localOverrideAppIdsListeners.Get() {
		connect.HandleError(func() {
			listener.LocalOverrideAppIdsChanged()
		})
	}
}

func (self *BlockActionViewController) AddBlockActionsListener(listener BlockActionsListener) Sub {
	callbackId := self.blockActionsListeners.Add(listener)
	return newSub(func() {
		self.blockActionsListeners.Remove(callbackId)
	})
}

func (self *BlockActionViewController) AddBlockActionStatsListener(listener BlockActionStatsListener) Sub {
	callbackId := self.blockActionStatsListeners.Add(listener)
	return newSub(func() {
		self.blockActionStatsListeners.Remove(callbackId)
	})
}

func (self *BlockActionViewController) AddLocalOverrideAppIdsListener(listener LocalOverrideAppIdsListener) Sub {
	callbackId := self.localOverrideAppIdsListeners.Add(listener)
	return newSub(func() {
		self.localOverrideAppIdsListeners.Remove(callbackId)
	})
}

func (self *BlockActionViewController) Start() {}

func (self *BlockActionViewController) Stop() {}

func (self *BlockActionViewController) Close() {
	deviceLog(self.device).Info("[bavc]close")
	self.cancel()
	self.blockActionWindowChangedSub.Close()
	self.blockStatsChangedSub.Close()
	self.blockActionOverridesChangedSub.Close()
}
