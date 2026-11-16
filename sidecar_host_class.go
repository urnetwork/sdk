//go:build !ios_extension && !js

// §18.4's host classes for an app's sidecar role: the budget each host's Sidecar runs in, when it may run at all, and
// how its search keeps the index (SDK-M10-02; R-SDK-13, D-SDK-21, D-SDK-25).
//
//	class    budget    runs                                                  search's all
//	ios      32 MiB    while the app is open, and in an OS-granted window    wifi_charging, plus on_demand at "search
//	                   or for a wake (§18.7)                                 more"; buckets synced while open
//	android  16 MiB    the same: never because the VPN service runs, though  the same as ios: the foreground, and
//	                   it shares the app's process (owner decision 197):     WorkManager's charging and unmetered
//	                   forwarding alone runs in the background, in the VPN   windows as iOS's BGProcessingTask; no
//	                   component                                             VPN-held background
//	desktop  128 MiB   whenever the app runs it; desktop apps run ur daemon  background, the core's defaults
//	                   in remote mode (§5.8)
//
// An app's MemoryBudgetByteCount, where it sets one, takes the class's place. The foreground, the OS windows and the
// wakes that let a phone's sidecar run in the background are reported to the sdk by SDK-M10-02's host calls
// (sidecar_search_conditions.go, sidecar_search_host.go) and SDK-M8-10's wakes; mayRun is the rule they consult, and
// an Android class never counts the VPN as a reason to run.
//
// Every value of the table is a field of sidecarHostClassSettings, set only by defaultSidecarHostClassSettings
// (QD-702): the classes are §18.4's rows, and a test or a later host adjusts an instance.

package sdk

import (
	"fmt"
	"runtime"
	"time"

	"github.com/urnetwork/sidecar"
)

// The host classes, as SidecarSettings.HostClass names them.
const (
	SidecarHostClassIos     = "ios"
	SidecarHostClassAndroid = "android"
	SidecarHostClassDesktop = "desktop"
)

// One host class's policy.
type sidecarHostClass struct {
	name string
	// the process's budget for the Sidecar (§18.4's Budget column)
	memoryBudgetByteCount int64
	// the sidecar role runs only while the app is open, OS-granted windows and wakes aside
	foregroundOnly bool
	// how the class's search keeps the index
	search *sidecarSearchHostClassSettings
}

// How one host class's search keeps the index (§18.4's Search block, D-SDK-21, D-SDK-25), which applySidecarSearchHost
// sets over the core's defaults, as sdk narrows connect's for mobile.
type sidecarSearchHostClassSettings struct {
	// §18.4's mode for all: wifi_charging on phones, background on desktops.
	AllPolicy string
	// Search.MemoryByteCount, the decoded index state inside the class's budget: on phones one search's 4 MiB working
	// set (§18.4's targets) beside the process's 256 KiB of version-table pages (§5.9), 4,456,448 B; on desktops the
	// core's 64 MiB.
	SearchMemoryByteCount int64
	// Local searches at once, which Search.DaemonSearchCount takes, set so the searches fit SearchMemoryByteCount: 1 on
	// phones; the core's 4 on desktops.
	SearchCount int
	// The most of one network's all a phone keeps: 256 MiB, and a tenth of the free space on StateDir's volume
	// (§18.4's caps row). Zero sets no cap beyond Search.CacheByteCount, as desktops keep.
	AllCacheMaxByteCount int64
	// The share of the free space all may take: 10, a tenth. Zero takes no share.
	AllCacheFreeDivisor int64
	// All shards no search used this long are evicted: 30 d on phones (§18.4's caps row); 0 on desktops.
	UnusedAllAge time.Duration
	// Whether the team directory's buckets are kept synced: false on phones, which fetch its cards while a view is
	// open and its buckets and all only for "search more" (§18.4's directory row); true on desktops.
	DirectoryBuckets bool
	// Sync rounds at once a network: 1 on app hosts (DESIGN-DETAILS 7.2.1).
	SyncRoundCount int
}

// §18.4's table, as settings.
type sidecarHostClassSettings struct {
	Ios     *sidecarHostClass
	Android *sidecarHostClass
	Desktop *sidecarHostClass
}

// §18.4's rows. A fresh value each call. Android's search matches iOS's (owner decision 197).
func defaultSidecarHostClassSettings() *sidecarHostClassSettings {
	phoneSearch := func() *sidecarSearchHostClassSettings {
		return &sidecarSearchHostClassSettings{
			AllPolicy:             sidecar.SearchPolicyWifiCharging,
			SearchMemoryByteCount: 4*1024*1024 + 256*1024,
			SearchCount:           1,
			AllCacheMaxByteCount:  256 * 1024 * 1024,
			AllCacheFreeDivisor:   10,
			UnusedAllAge:          30 * 24 * time.Hour,
			DirectoryBuckets:      false,
			SyncRoundCount:        1,
		}
	}
	return &sidecarHostClassSettings{
		Ios: &sidecarHostClass{
			name:                  SidecarHostClassIos,
			memoryBudgetByteCount: 32 * 1024 * 1024,
			foregroundOnly:        true,
			search:                phoneSearch(),
		},
		Android: &sidecarHostClass{
			name:                  SidecarHostClassAndroid,
			memoryBudgetByteCount: 16 * 1024 * 1024,
			foregroundOnly:        true,
			search:                phoneSearch(),
		},
		Desktop: &sidecarHostClass{
			name:                  SidecarHostClassDesktop,
			memoryBudgetByteCount: 128 * 1024 * 1024,
			foregroundOnly:        false,
			search: &sidecarSearchHostClassSettings{
				AllPolicy:             sidecar.SearchPolicyBackground,
				SearchMemoryByteCount: 64 * 1024 * 1024,
				SearchCount:           4,
				DirectoryBuckets:      true,
				SyncRoundCount:        1,
			},
		},
	}
}

// The class name names in settings, or for an empty one this build's platform's.
func (self *sidecarHostClassSettings) classOf(name string) (*sidecarHostClass, error) {
	if name == "" {
		switch runtime.GOOS {
		case "ios":
			name = SidecarHostClassIos
		case "android":
			name = SidecarHostClassAndroid
		default:
			name = SidecarHostClassDesktop
		}
	}
	switch name {
	case SidecarHostClassIos:
		return self.Ios, nil
	case SidecarHostClassAndroid:
		return self.Android, nil
	case SidecarHostClassDesktop:
		return self.Desktop, nil
	}
	return nil, fmt.Errorf("no sidecar host class %q", name)
}

// The class name names under the defaults, or for an empty one this build's platform's.
func sidecarHostClassOf(name string) (*sidecarHostClass, error) {
	return defaultSidecarHostClassSettings().classOf(name)
}

// Applies settings' host class to coreSettings, the core's, and answers the class: its budget, unless the app set its
// own.
func applySidecarHostClass(settings *SidecarSettings, coreSettings *sidecar.SidecarSettings) (*sidecarHostClass, error) {
	hostClass, err := sidecarHostClassOf(settings.HostClass)
	if err != nil {
		return nil, err
	}
	coreSettings.MemoryBudgetByteCount = hostClass.memoryBudgetByteCount
	if 0 < settings.MemoryBudgetByteCount {
		coreSettings.MemoryBudgetByteCount = settings.MemoryBudgetByteCount
	}
	return hostClass, nil
}

// Whether the sidecar role may run now: on a foreground-only class only while the app is open, in an OS-granted
// window or for a wake; whether the app's VPN component runs never enters it, so the VPN keeps no sidecar running
// in the background (owner decision 197).
func (self *sidecarHostClass) mayRun(appOpen bool, osWindow bool, wake bool, vpnRunning bool) bool {
	if !self.foregroundOnly {
		return true
	}
	return appOpen || osWindow || wake
}
