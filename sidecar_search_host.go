//go:build !ios_extension && !js

// Search's host classes, policy and windows on an app host (§18.4's Search block, §18.7's Search windows; SDK-M10-02;
// R-SDK-13, D-SDK-21, D-SDK-23, D-SDK-25, I-13, R-CTX-35).
//
//	class      app hosts and the page Worker's main set Search.AllPolicy and Search.MemoryByteCount over
//	           DefaultSidecarSettings() per host class (sidecar_host_class.go), as sdk narrows connect's defaults for
//	           mobile: on phones wifi_charging, one search at once in a 4 MiB working set beside the 256 KiB of
//	           version-table pages, all shards unused for 30 days evicted, no team directory buckets, one sync round;
//	           on desktops the core's defaults. Every app host runs remote search's requester side and serves none
//	           (Search.RemoteAsk on, Search.RemoteServe off), so an app host that reported no condition reads metered
//	           and not charging, never a daemon's unmetered (D-SDK-23)
//	free       a phone keeps all only below min(256 MiB, a tenth of the free space on StateDir's volume), read by
//	           statfs at start and passed in the core's settings (plan-SDK-small-readings #14); a phone whose free
//	           space cannot be read keeps no all, since the core reads a cap of 0 as none. Each RunBackgroundSync reads
//	           it again; the running core takes a new cap at its next start, since it reads its settings without a lock
//	policy     SetPolicy overrides the mode for all, for a repo or every repo; "" restores the host's default. It is
//	           kept in §5.9's non-derived search state, which DropCache and cache eviction keep, never in the config
//	           repo, and never set through MCP (an app's call, which the person's settings make)
//	windows    RunBackgroundSync(budgetMillis) is an OS-granted window: iOS's BGProcessingTask, Android's WorkManager
//	           periodic work with charging and unmetered constraints (owner decision 197: the same on both, and never
//	           because the VPN runs). The budget maps to the call's deadline, which the core's window stops at and
//	           resumes from next time
//
// Nothing here reads a platform API but statfs: conditions and the foreground are the app's reports
// (sidecar_search_conditions.go). Safe for concurrent use.

package sdk

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"
)

// The cap that keeps no all: the core reads a cap of 0 as none, so a phone that may keep nothing passes the smallest
// one no shard fits.
const sidecarSearchKeepNoAllByteCount = 1

// A background window asked with no budget.
var errSidecarSearchBudget = errors.New("a background window needs a positive budget")

// The host's search state the app reports to: its class, the directory free space is read on, its foreground, and the
// OS windows in progress.
type sidecarSearchHost struct {
	hostClass *sidecarHostClass
	stateDir  string
	// statfs's free bytes on a directory's volume: sidecarFreeByteCount, or a test's
	freeByteCount func(dir string) (uint64, error)
	// the clock a window's deadline is read on: time.Now, or a test's
	now func() time.Time

	stateLock sync.Mutex
	// the app's last report, false before one (§18.7: unreported counts as background)
	foreground bool
	// OS windows in progress
	windowCount int
	// the cap the last read of free space gave, which the core took at start
	allCacheByteCount int64
}

// The host of hostClass on stateDir. freeByteCount and now may be nil for statfs and time.Now.
func newSidecarSearchHost(hostClass *sidecarHostClass, stateDir string, freeByteCount func(dir string) (uint64, error), now func() time.Time) *sidecarSearchHost {
	if freeByteCount == nil {
		freeByteCount = sidecarFreeByteCount
	}
	if now == nil {
		now = time.Now
	}
	return &sidecarSearchHost{hostClass: hostClass, stateDir: stateDir, freeByteCount: freeByteCount, now: now}
}

// The cap on all the class keeps now: none on a class without one, else min(AllCacheMaxByteCount, free /
// AllCacheFreeDivisor), and the cap that keeps none where free space cannot be read or leaves nothing.
func (self *sidecarSearchHost) readAllCacheByteCount() int64 {
	search := self.hostClass.search
	if search.AllCacheMaxByteCount <= 0 {
		return 0
	}
	free, err := self.freeByteCount(self.stateDir)
	if err != nil {
		return sidecarSearchKeepNoAllByteCount
	}
	allCap := search.AllCacheMaxByteCount
	if 0 < search.AllCacheFreeDivisor {
		share := free / uint64(search.AllCacheFreeDivisor)
		if share < uint64(allCap) {
			allCap = int64(share)
		}
	}
	if allCap <= 0 {
		return sidecarSearchKeepNoAllByteCount
	}
	return allCap
}

// Sets the class's search over coreSettings (the file's class and free rows): the core's search runs as an app host's.
func (self *sidecarSearchHost) apply(coreSettings *sidecar.SidecarSettings) {
	search := self.hostClass.search
	coreSettings.Search.AllPolicy = search.AllPolicy
	coreSettings.Search.DaemonSearchCount = search.SearchCount
	coreSettings.Search.MemoryByteCount = search.SearchMemoryByteCount
	coreSettings.Search.UnusedAllAge = search.UnusedAllAge
	coreSettings.Search.DirectoryBuckets = search.DirectoryBuckets
	coreSettings.Search.SyncRoundCount = search.SyncRoundCount
	coreSettings.Search.RemoteAsk = true
	coreSettings.Search.RemoteServe = false
	allCacheByteCount := self.readAllCacheByteCount()
	coreSettings.Search.AllCacheByteCount = allCacheByteCount
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.allCacheByteCount = allCacheByteCount
}

// The cap a read of free space gave last.
func (self *sidecarSearchHost) lastAllCacheByteCount() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.allCacheByteCount
}

// Notes the app's foreground.
func (self *sidecarSearchHost) setForeground(foreground bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.foreground = foreground
}

// Whether the class lets the sidecar role run now, by the app's foreground and the windows in progress; wake says a
// wake push is being handled. The VPN never enters it (owner decision 197).
func (self *sidecarSearchHost) mayRun(wake bool) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.hostClass.mayRun(self.foreground, 0 < self.windowCount, wake, false)
}

// Opens a window, answering what closes it.
func (self *sidecarSearchHost) openWindow() func() {
	allCacheByteCount := self.readAllCacheByteCount()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.windowCount += 1
	self.allCacheByteCount = allCacheByteCount
	var once sync.Once
	return func() {
		once.Do(func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.windowCount -= 1
		})
	}
}

// Whether policy is one SetPolicy takes: §18.4's modes, or "" for the host's default.
func isSidecarSearchPolicy(policy string) bool {
	switch policy {
	case "", sidecar.SearchPolicyBackground, sidecar.SearchPolicyWifiCharging, sidecar.SearchPolicyOnDemand, sidecar.SearchPolicyNever:
		return true
	}
	return false
}

// Sets this device's policy for all (§18.4's modes: background, wifi_charging, on_demand or never) for repoId, or
// with a nil repoId for every repo; "" restores the host's default (§18.5). Kept in §5.9's search state, which DropCache
// keeps. Answers callback once.
func (self *SidecarSearch) SetPolicy(repoId *Id, policy string, callback SidecarResultCallback) {
	sidecarSearchAnswer(func() *SidecarResult {
		ctx, err := self.callCtx()
		if err != nil {
			return sidecarSearchResultOf(err)
		}
		if !isSidecarSearchPolicy(policy) {
			return sidecarSearchResultOf(errSidecarSearchQuery)
		}
		return sidecarSearchResultOf(self.backend.setPolicy(ctx, sidecarSearchRepoIdOf(repoId), policy))
	}, callback)
}

// Runs one OS-granted background window (§18.5, §18.7): the app's BGProcessingTask on iOS or its WorkManager work on
// Android calls it with the window's budget, and it does §18.4's window work while the reported conditions allow,
// stops at the budget and resumes next time. Answers callback once, Ok when the window ended at its budget or ran out
// of work.
func (self *SidecarSearch) RunBackgroundSync(budgetMillis int64, callback SidecarResultCallback) {
	sidecarSearchAnswer(func() *SidecarResult {
		ctx, err := self.callCtx()
		if err != nil {
			return sidecarSearchResultOf(err)
		}
		if budgetMillis <= 0 {
			return sidecarSearchResultOf(errors.Join(errSidecarSearchQuery, errSidecarSearchBudget))
		}
		now := time.Now
		if self.host != nil {
			now = self.host.now
			closeWindow := self.host.openWindow()
			defer closeWindow()
		}
		windowCtx, cancel := context.WithDeadline(ctx, now().Add(time.Duration(budgetMillis)*time.Millisecond))
		defer cancel()
		return sidecarSearchResultOf(self.backend.runBackgroundSync(windowCtx))
	}, callback)
}

// The core's foreground setter, which SEARCH-M10-33 adds to sidecar.Sidecar (plan-SEARCH-foreground-signal).
type sidecarSearchForegroundSetter interface {
	SetSearchForeground(foreground bool)
}

// See sidecarSearchBackend.
func (self *sidecarSearchCoreBackend) setPolicy(ctx context.Context, repoId *sidecar.Id, policy string) error {
	return self.local.SetSearchPolicy(repoId, policy)
}

// See sidecarSearchBackend.
func (self *sidecarSearchCoreBackend) setConditions(unmetered bool, charging bool, lowPower bool) {
	self.local.SetSearchConditions(unmetered, charging, lowPower)
}

// See sidecarSearchBackend: the core's setter once it has one; until then the report stays with the sdk's host.
func (self *sidecarSearchCoreBackend) setForeground(foreground bool) {
	if setter, ok := any(self.local).(sidecarSearchForegroundSetter); ok {
		setter.SetSearchForeground(foreground)
	}
}

// See sidecarSearchBackend: ctx's deadline is the window's budget.
func (self *sidecarSearchCoreBackend) runBackgroundSync(ctx context.Context) error {
	return self.local.RunSearchBackgroundSync(ctx)
}
