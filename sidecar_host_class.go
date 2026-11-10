//go:build !ios_extension && !js

// §18.4's host classes for an app's sidecar role: the budget each host's Sidecar runs in, and when it may run at all.
//
//	class    budget    runs
//	ios      32 MiB    while the app is open, and in an OS-granted window or for a wake (§18.7)
//	android  16 MiB    the same: never because the VPN service runs, though it shares the app's process (owner
//	                   decision 197): forwarding alone runs in the background, in the VPN component
//	desktop  128 MiB   whenever the app runs it; desktop apps run ur daemon in remote mode (§5.8)
//
// An app's MemoryBudgetByteCount, where it sets one, takes the class's place. The foreground, the OS windows and the
// wakes that let a phone's sidecar run in the background are reported to the sdk by SDK-M10-02's host calls and
// SDK-M8-10's wakes; mayRun is the rule they consult, and an Android class never counts the VPN as a reason to run.

package sdk

import (
	"fmt"
	"runtime"

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
}

// §18.4's table. A class's values are facts of the table, not tunables: an app's settings override the budget.
var sidecarHostClasses = map[string]*sidecarHostClass{
	SidecarHostClassIos: {
		name:                  SidecarHostClassIos,
		memoryBudgetByteCount: 32 * 1024 * 1024,
		foregroundOnly:        true,
	},
	SidecarHostClassAndroid: {
		name:                  SidecarHostClassAndroid,
		memoryBudgetByteCount: 16 * 1024 * 1024,
		foregroundOnly:        true,
	},
	SidecarHostClassDesktop: {
		name:                  SidecarHostClassDesktop,
		memoryBudgetByteCount: 128 * 1024 * 1024,
		foregroundOnly:        false,
	},
}

// The class name names, or for an empty one this build's platform's.
func sidecarHostClassOf(name string) (*sidecarHostClass, error) {
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
	hostClass, ok := sidecarHostClasses[name]
	if !ok {
		return nil, fmt.Errorf("no sidecar host class %q", name)
	}
	return hostClass, nil
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
