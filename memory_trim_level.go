package sdk

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// Android ComponentCallbacks2 values as delivered to onTrimMemory. Since API
// 34 the platform delivers only UI_HIDDEN and BACKGROUND to apps; the other
// levels still arrive on older releases and from `am send-trim-memory`.
const (
	androidTrimMemoryRunningModerate int64 = 5
	androidTrimMemoryRunningLow      int64 = 10
	androidTrimMemoryRunningCritical int64 = 15
	androidTrimMemoryUiHidden        int64 = 20
	androidTrimMemoryBackground      int64 = 40
	androidTrimMemoryModerate        int64 = 60
	androidTrimMemoryComplete        int64 = 80
)

// Ordered by cost, so a report that escalates past the last action acts at
// once even inside the cooldown.
type mobileTrimLevelAction uint8

const (
	mobileTrimLevelActionNone mobileTrimLevelAction = iota
	// Arm one quiet-epoch pass of the idle reclaimer; the pass still requires
	// the Go total above target and runs under the reclaimer's cooldown.
	mobileTrimLevelActionArmQuiet
	// Drop free lists above the warm set now; force a collection only when
	// that drop is material.
	mobileTrimLevelActionTrim
	// Shed recoverable caches and rebuild the pools with a forced collection.
	mobileTrimLevelActionTrimForced
	// What FreeMemory does: caches shed, pools cleared, spans returned.
	mobileTrimLevelActionFree
)

// mobileTrimLevelActionFor maps a platform trim level to its reclamation.
//
// RUNNING_* levels are delivered while the process is running and the device
// is short of memory: exactly the state of a connected tunnel, so they must
// reclaim without breaking it. MODERATE is gentle (a gated quiet pass), LOW
// releases free lists now, CRITICAL is the last signal before the killer
// acts, so it pays for a forced collection and sheds caches.
//
// UI_HIDDEN only says the interface went away; a VPN keeps running and is not
// under pressure, so it does nothing. BACKGROUND and above mean the process is
// in the cached LRU, which a foreground VPN service is never in while the
// tunnel is up: the tunnel is idle or gone, a collection costs no latency, and
// the app freezer may stop the process before a 15-second quiet epoch ends.
// BACKGROUND therefore releases now, MODERATE forces it, COMPLETE frees all.
func mobileTrimLevelActionFor(level int64) mobileTrimLevelAction {
	switch {
	case androidTrimMemoryComplete <= level:
		return mobileTrimLevelActionFree
	case androidTrimMemoryModerate <= level:
		return mobileTrimLevelActionTrimForced
	case androidTrimMemoryBackground <= level:
		return mobileTrimLevelActionTrim
	case androidTrimMemoryUiHidden <= level:
		return mobileTrimLevelActionNone
	case androidTrimMemoryRunningCritical <= level:
		return mobileTrimLevelActionTrimForced
	case androidTrimMemoryRunningLow <= level:
		return mobileTrimLevelActionTrim
	case androidTrimMemoryRunningModerate <= level:
		return mobileTrimLevelActionArmQuiet
	default:
		return mobileTrimLevelActionNone
	}
}

type mobileTrimLevelState struct {
	lastAction     mobileTrimLevelAction
	lastActionTime time.Time
}

// mobileTrimLevelTransition makes repeated reports idempotent: the platform
// re-delivers the same level as its LRU position is recomputed, and each
// forced collection has a latency and battery cost. A report acts when it
// escalates past the last action, or once the cooldown since that action has
// elapsed.
func mobileTrimLevelTransition(
	action mobileTrimLevelAction,
	state mobileTrimLevelState,
	now time.Time,
	cooldown time.Duration,
) (act bool, next mobileTrimLevelState) {
	if action == mobileTrimLevelActionNone {
		return false, state
	}
	if state.lastActionTime.IsZero() ||
		state.lastAction < action ||
		cooldown <= now.Sub(state.lastActionTime) {
		return true, mobileTrimLevelState{lastAction: action, lastActionTime: now}
	}
	return false, state
}

type mobileTrimLevelReporter struct {
	stateLock sync.Mutex
	state     mobileTrimLevelState
	cooldown  time.Duration
	now       func() time.Time
	// the quiet-epoch arm is meaningful only once the idle trimmer runs
	trimmerStarted func() bool
	armQuiet       func()
	trim           func(force bool) ByteCount
	shed           func()
	free           func()
}

// report applies one platform trim level and returns the action it took.
func (self *mobileTrimLevelReporter) report(level int64) mobileTrimLevelAction {
	mobileTrimLevelLast.Store(level)
	mobileTrimLevelCount.Add(1)

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	action := mobileTrimLevelActionFor(level)
	if action == mobileTrimLevelActionArmQuiet && !self.trimmerStarted() {
		// Like the footprint path: do not consume the latch before the
		// trimmer can receive the epoch.
		return mobileTrimLevelActionNone
	}
	act, next := mobileTrimLevelTransition(action, self.state, self.now(), self.cooldown)
	if !act {
		return mobileTrimLevelActionNone
	}
	self.state = next
	mobileTrimLevelActionCount.Add(1)
	switch action {
	case mobileTrimLevelActionArmQuiet:
		self.armQuiet()
	case mobileTrimLevelActionTrim:
		mobileTrimLevelDropped.Store(int64(self.trim(false)))
	case mobileTrimLevelActionTrimForced:
		self.shed()
		mobileTrimLevelDropped.Store(int64(self.trim(true)))
	case mobileTrimLevelActionFree:
		self.free()
	}
	return action
}

var (
	mobileTrimLevelLast        atomic.Int64
	mobileTrimLevelCount       atomic.Int64
	mobileTrimLevelActionCount atomic.Int64
	mobileTrimLevelDropped     atomic.Int64

	defaultMobileTrimLevelReporter = &mobileTrimLevelReporter{
		cooldown:       mobileIdleMemoryTrimCooldown,
		now:            time.Now,
		trimmerStarted: mobileIdleMemoryTrimmerStarted.Load,
		armQuiet: func() {
			noteMobileMemoryActivity(mobileIdleMemoryActivityMinByteCount)
		},
		trim: trimMemory,
		shed: connect.ShedMemory,
		free: FreeMemory,
	}
)

// ReportMemoryTrimLevel relays the host platform's memory trim level, as
// Android delivers it to ComponentCallbacks2.onTrimMemory (onLowMemory reports
// 80). The sdk maps the level to a reclamation; see mobileTrimLevelActionFor.
// Safe to call from any thread and before SetMemoryLimit.
func ReportMemoryTrimLevel(level int64) {
	defaultMobileTrimLevelReporter.report(level)
}
