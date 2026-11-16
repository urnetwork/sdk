//go:build !ios_extension && !js

package sdk

// Search's host classes, conditions, windows and foreground (SDK-M10-02): an app host that reported nothing reads
// metered, each class's search settings over the core's defaults, Android's matching iOS's, a background window's budget
// as its deadline, a policy override that DropCache leaves, the phone's all cap from free space, and the foreground
// reaching the core's search and the class rule. Every repo is synthetic and every clock the test's.

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// An app host that reported no condition reads metered and not charging, never a daemon's unmetered (D-SDK-23): the
// sdk runs the core as an app host. Once the app reports, the core reads what it said.
func TestUnreportedConditionCountsMetered(t *testing.T) {
	for _, class := range []string{SidecarHostClassIos, SidecarHostClassAndroid, SidecarHostClassDesktop} {
		world := newTestSidecarWorld(t)
		settings := DefaultSidecarSettings()
		settings.HostClass = class
		settings.Components.Add(SidecarComponentEvs)
		local := world.openWith(t, settings, &sidecarLocalOptions{
			componentFactories:  map[string]sidecarComponentFactory{SidecarComponentEvs: testSearchEvsComponent},
			searchFreeByteCount: func(dir string) (uint64, error) { return 10 * 1024 * 1024 * 1024, nil },
		})
		conditions, _ := local.Core().SearchConditions()
		if conditions.Reported || conditions.Unmetered || conditions.Charging {
			t.Errorf("%s: unreported conditions read %+v", class, conditions)
		}
		search := local.GetSearch()
		if search == nil {
			t.Fatalf("%s: no search", class)
		}
		search.SetConditions(true, true, false)
		conditions, _ = local.Core().SearchConditions()
		if !conditions.Reported || !conditions.Unmetered || !conditions.Charging || conditions.LowPower {
			t.Errorf("%s: reported conditions read %+v", class, conditions)
		}
		search.SetConditions(false, true, true)
		if conditions, _ = local.Core().SearchConditions(); conditions.Unmetered || !conditions.LowPower {
			t.Errorf("%s: a second report reads %+v", class, conditions)
		}
	}
}

// Each class's search over the core's defaults: phones keep all wifi_charging, one search at once in a 4 MiB working
// set beside the version-table pages, all unused for 30 days evicted, no directory buckets, one sync round; Android's
// matches iOS's (owner decision 197); desktops keep the core's; every app host asks and serves nothing.
func TestHostClassPolicies(t *testing.T) {
	free := uint64(10 * 1024 * 1024 * 1024)
	applied := map[string]*sidecar.SidecarSearchSettings{}
	for _, class := range []string{SidecarHostClassIos, SidecarHostClassAndroid, SidecarHostClassDesktop} {
		hostClass, err := sidecarHostClassOf(class)
		if err != nil {
			t.Fatal(err)
		}
		coreSettings := sidecar.DefaultSidecarSettings()
		host := newSidecarSearchHost(hostClass, t.TempDir(), func(dir string) (uint64, error) { return free, nil }, nil)
		host.apply(coreSettings)
		applied[class] = coreSettings.Search
		search := coreSettings.Search
		if !search.RemoteAsk || search.RemoteServe {
			t.Errorf("%s: RemoteAsk %v RemoteServe %v", class, search.RemoteAsk, search.RemoteServe)
		}
		if search.MemoryByteCount <= 0 || coreSettings.MemoryBudgetByteCount < search.MemoryByteCount || search.DaemonSearchCount <= 0 {
			t.Errorf("%s: search memory %d of %d, %d searches", class, search.MemoryByteCount, coreSettings.MemoryBudgetByteCount, search.DaemonSearchCount)
		}
		if search.MemoryByteCount < search.VersionCacheByteCount {
			t.Errorf("%s: the version-table pages do not fit the search memory", class)
		}
	}
	for _, class := range []string{SidecarHostClassIos, SidecarHostClassAndroid} {
		search := applied[class]
		if search.AllPolicy != sidecar.SearchPolicyWifiCharging || search.DaemonSearchCount != 1 || search.MemoryByteCount != 4*1024*1024+256*1024 ||
			search.UnusedAllAge != 30*24*time.Hour || search.DirectoryBuckets || search.SyncRoundCount != 1 || search.AllCacheByteCount != 256*1024*1024 {
			t.Errorf("%s: %+v", class, search)
		}
		budget, _ := sidecarHostClassOf(class)
		if budget.memoryBudgetByteCount < search.MemoryByteCount {
			t.Errorf("%s: the search memory passes the class's budget", class)
		}
	}
	ios, android := *applied[SidecarHostClassIos], *applied[SidecarHostClassAndroid]
	if ios.AllPolicy != android.AllPolicy || ios.MemoryByteCount != android.MemoryByteCount || ios.DaemonSearchCount != android.DaemonSearchCount ||
		ios.UnusedAllAge != android.UnusedAllAge || ios.DirectoryBuckets != android.DirectoryBuckets || ios.AllCacheByteCount != android.AllCacheByteCount {
		t.Error("Android's search does not match iOS's")
	}
	defaults := sidecar.DefaultSidecarSettings().Search
	if desktop := applied[SidecarHostClassDesktop]; desktop.AllPolicy != sidecar.SearchPolicyBackground || desktop.MemoryByteCount != defaults.MemoryByteCount ||
		desktop.DaemonSearchCount != defaults.DaemonSearchCount || desktop.AllCacheByteCount != 0 || desktop.UnusedAllAge != 0 || !desktop.DirectoryBuckets {
		t.Errorf("desktop: %+v", desktop)
	}
	// the class's search reaches the core through NewSidecarLocal, before a test's adjustment
	world := newTestSidecarWorld(t)
	var seen *sidecar.SidecarSearchSettings
	world.openWith(t, &SidecarSettings{Instance: "default", Components: NewStringList(), HostClass: SidecarHostClassAndroid}, &sidecarLocalOptions{
		searchFreeByteCount: func(dir string) (uint64, error) { return free, nil },
		adjustCoreSettings: func(settings *sidecar.SidecarSettings) {
			copied := *settings.Search
			seen = &copied
		},
	})
	if seen == nil || seen.AllPolicy != sidecar.SearchPolicyWifiCharging || seen.RemoteServe || seen.AllCacheByteCount != 256*1024*1024 {
		t.Errorf("NewSidecarLocal's core search settings: %+v", seen)
	}
}

// RunBackgroundSync's budget is the window's deadline on the host's clock, an OS window lets a foreground-only class
// run while it lasts, a window with no budget is refused, and on the core with no engine the window answers
// unavailable.
func TestBudgetMapsToDeadline(t *testing.T) {
	now := time.Date(2027, 2, 1, 3, 0, 0, 0, time.UTC)
	hostClass, _ := sidecarHostClassOf(SidecarHostClassIos)
	host := newSidecarSearchHost(hostClass, t.TempDir(), func(dir string) (uint64, error) { return 4 * 1024 * 1024 * 1024, nil }, func() time.Time { return now })
	backend := newTestSearchBackend()
	mayRunInWindow := make(chan bool, 1)
	backend.windowHook = func() {
		mayRunInWindow <- host.mayRun(false)
	}
	search := newTestSidecarSearch(t, backend, host)
	if host.mayRun(false) {
		t.Fatal("a phone in the background with no window may run")
	}
	window, results := testSidecarResultCallback()
	search.RunBackgroundSync(1500, window)
	if result := <-results; !result.Ok {
		t.Fatalf("the window: %+v", result)
	}
	if len(backend.windows) != 1 || !backend.windows[0].Equal(now.Add(1500*time.Millisecond)) {
		t.Fatalf("the window's deadline: %v", backend.windows)
	}
	if !<-mayRunInWindow {
		t.Error("the class may not run inside an OS window")
	}
	if host.mayRun(false) {
		t.Error("the window outlived its call")
	}
	for _, budget := range []int64{0, -5} {
		refused, refusedResults := testSidecarResultCallback()
		search.RunBackgroundSync(budget, refused)
		if result := <-refusedResults; result.Ok || result.Code != SidecarSearchCodeInvalid {
			t.Errorf("a budget of %d: %+v", budget, result)
		}
	}
	backend.windowErr = errors.New("the core's window failed")
	failed, failedResults := testSidecarResultCallback()
	search.RunBackgroundSync(1000, failed)
	if result := <-failedResults; result.Ok || result.Code != SidecarSearchCodeFailed {
		t.Errorf("a failed window: %+v", result)
	}

	// the core: no engine runs, so the window answers unavailable
	world := newTestSidecarWorld(t)
	settings := DefaultSidecarSettings()
	settings.Components.Add(SidecarComponentEvs)
	local := world.openWith(t, settings, &sidecarLocalOptions{componentFactories: map[string]sidecarComponentFactory{SidecarComponentEvs: testSearchEvsComponent}})
	core, coreResults := testSidecarResultCallback()
	local.GetSearch().RunBackgroundSync(1000, core)
	if result := <-coreResults; result.Ok || result.Code != SidecarSearchCodeUnavailable {
		t.Errorf("a window on the core with no engine: %+v", result)
	}
}

// A policy override stays when the cache goes: DropCache drops files and derived data, never §5.9's search state, and
// the sdk sends nothing that resets the override. Unknown modes are refused before the core sees them. On the core,
// unenrolled, both answer unavailable.
func TestOverrideSurvivesDropCache(t *testing.T) {
	repoId := sidecar.Id{0x5e, 0x07}
	backend := newTestSearchBackend()
	backend.statuses[repoId.String()] = &SidecarSearchStatus{IndexOn: true, FormatSupported: true, Policy: sidecar.SearchPolicyWifiCharging, Remote: "on"}
	search := newTestSidecarSearch(t, backend, nil)
	set, setResults := testSidecarResultCallback()
	search.SetPolicy(newId(repoId), sidecar.SearchPolicyNever, set)
	if result := <-setResults; !result.Ok {
		t.Fatalf("SetPolicy: %+v", result)
	}
	dropped, droppedResults := testSidecarResultCallback()
	search.DropCache(newId(repoId), dropped)
	if result := <-droppedResults; !result.Ok {
		t.Fatalf("DropCache: %+v", result)
	}
	if status := search.GetStatus(newId(repoId)); status == nil || status.Policy != sidecar.SearchPolicyNever {
		t.Fatalf("the policy after DropCache: %+v", status)
	}
	if len(backend.dropped) != 1 || backend.dropped[0] != repoId.String() || backend.policies[repoId.String()] != sidecar.SearchPolicyNever {
		t.Errorf("the backend saw dropped %v policies %v", backend.dropped, backend.policies)
	}
	restored, restoredResults := testSidecarResultCallback()
	search.SetPolicy(nil, "", restored)
	if result := <-restoredResults; !result.Ok || backend.policies["*"] != "" {
		t.Errorf("restoring every repo's default: %+v %v", result, backend.policies)
	}
	unknown, unknownResults := testSidecarResultCallback()
	search.SetPolicy(nil, "sometimes", unknown)
	if result := <-unknownResults; result.Ok || result.Code != SidecarSearchCodeInvalid {
		t.Errorf("an unknown mode: %+v", result)
	}

	world := newTestSidecarWorld(t)
	settings := DefaultSidecarSettings()
	settings.Components.Add(SidecarComponentEvs)
	local := world.openWith(t, settings, &sidecarLocalOptions{componentFactories: map[string]sidecarComponentFactory{SidecarComponentEvs: testSearchEvsComponent}})
	core := local.GetSearch()
	corePolicy, corePolicyResults := testSidecarResultCallback()
	core.SetPolicy(newId(repoId), sidecar.SearchPolicyNever, corePolicy)
	if result := <-corePolicyResults; result.Ok || result.Code != SidecarSearchCodeUnavailable {
		t.Errorf("SetPolicy on an unenrolled core: %+v", result)
	}
	coreDrop, coreDropResults := testSidecarResultCallback()
	core.DropCache(nil, coreDrop)
	if result := <-coreDropResults; result.Ok || result.Code != SidecarSearchCodeUnavailable {
		t.Errorf("DropCache on an unenrolled core: %+v", result)
	}
}

// A phone keeps all within min(256 MiB, a tenth of the free space): the cap is read at start and again at each window;
// free space it cannot read, or none, keeps no all, since the core reads 0 as no cap; a desktop sets none.
func TestCapFromFreeSpace(t *testing.T) {
	const mib = 1024 * 1024
	ios, _ := sidecarHostClassOf(SidecarHostClassIos)
	desktop, _ := sidecarHostClassOf(SidecarHostClassDesktop)
	for _, c := range []struct {
		free uint64
		err  error
		cap  int64
	}{
		{free: 10 * 1024 * mib, cap: 256 * mib},
		{free: 2560 * mib, cap: 256 * mib},
		{free: 1024 * mib, cap: 1024 * mib / 10},
		{free: 9, cap: sidecarSearchKeepNoAllByteCount},
		{free: 0, cap: sidecarSearchKeepNoAllByteCount},
		{err: errors.New("statfs failed"), cap: sidecarSearchKeepNoAllByteCount},
	} {
		host := newSidecarSearchHost(ios, "/state", func(dir string) (uint64, error) {
			if dir != "/state" {
				t.Errorf("free space read on %s", dir)
			}
			return c.free, c.err
		}, nil)
		if got := host.readAllCacheByteCount(); got != c.cap {
			t.Errorf("free %d err %v: cap %d, want %d", c.free, c.err, got, c.cap)
		}
		desktopHost := newSidecarSearchHost(desktop, "/state", func(dir string) (uint64, error) { return c.free, c.err }, nil)
		if got := desktopHost.readAllCacheByteCount(); got != 0 {
			t.Errorf("a desktop's cap: %d", got)
		}
	}
	// read at start and again at each window
	free := uint64(10 * 1024 * mib)
	host := newSidecarSearchHost(ios, t.TempDir(), func(dir string) (uint64, error) { return free, nil }, nil)
	coreSettings := sidecar.DefaultSidecarSettings()
	host.apply(coreSettings)
	if coreSettings.Search.AllCacheByteCount != 256*mib || host.lastAllCacheByteCount() != 256*mib {
		t.Fatalf("the cap at start: %d", coreSettings.Search.AllCacheByteCount)
	}
	free = 500 * mib
	closeWindow := host.openWindow()
	if host.lastAllCacheByteCount() != 50*mib {
		t.Errorf("the cap at a window: %d", host.lastAllCacheByteCount())
	}
	closeWindow()
	closeWindow()
	if host.mayRun(false) {
		t.Error("a closed window still counts")
	}
	// statfs on this host answers something for a real directory
	if _, err := sidecarFreeByteCount(t.TempDir()); err != nil {
		t.Errorf("statfs here: %v", err)
	}
}

// SetForeground reaches the backend at each change and the class rule: a phone in the foreground may run and in the
// background may not, a desktop always may, and the VPN never counts (owner decision 197). On the core it goes to
// Sidecar.SetSearchForeground once the core has it (SEARCH-M10-33).
func TestForegroundForwarded(t *testing.T) {
	ios, _ := sidecarHostClassOf(SidecarHostClassIos)
	host := newSidecarSearchHost(ios, t.TempDir(), func(dir string) (uint64, error) { return 0, nil }, nil)
	backend := newTestSearchBackend()
	search := newTestSidecarSearch(t, backend, host)
	if host.mayRun(false) {
		t.Fatal("an unreported foreground counts as the foreground")
	}
	search.SetForeground(true)
	if !host.mayRun(false) {
		t.Error("a phone in the foreground may not run")
	}
	search.SetForeground(false)
	if host.mayRun(false) || !host.mayRun(true) {
		t.Error("a phone in the background runs, or a wake does not let it")
	}
	if ios.mayRun(false, false, false, true) {
		t.Error("the VPN running lets a phone's sidecar run")
	}
	if len(backend.foregrounds) != 2 || !backend.foregrounds[0] || backend.foregrounds[1] {
		t.Errorf("the backend saw %v", backend.foregrounds)
	}
	desktop, _ := sidecarHostClassOf(SidecarHostClassDesktop)
	if desktopHost := newSidecarSearchHost(desktop, t.TempDir(), nil, nil); !desktopHost.mayRun(false) {
		t.Error("a desktop may not run in the background")
	}
	// a closed search reports nothing more to its backend, but still to the host
	search.close()
	search.SetForeground(true)
	if len(backend.foregrounds) != 2 || !host.mayRun(false) {
		t.Errorf("after close the backend saw %v", backend.foregrounds)
	}

	// the core's search: the report goes to the core's setter where it has one
	world := newTestSidecarWorld(t)
	settings := DefaultSidecarSettings()
	settings.HostClass = SidecarHostClassAndroid
	settings.Components.Add(SidecarComponentEvs)
	local := world.openWith(t, settings, &sidecarLocalOptions{componentFactories: map[string]sidecarComponentFactory{SidecarComponentEvs: testSearchEvsComponent}})
	local.GetSearch().SetForeground(true)
	if !local.searchHost.mayRun(false) {
		t.Error("the Sidecar's host did not take the foreground")
	}
	if _, ok := any(local.Core()).(sidecarSearchForegroundSetter); !ok {
		t.Log("the core has no SetSearchForeground yet (SEARCH-M10-33): the foreground reaches the sdk's host alone")
	}
}
