package sdk

// The dns resolver status (connect ADBLOCK3RD C5, C6, C8, I2, I3, D5): the
// state from the mux's resolver health, the verification probe, the blocked
// count and the app log line. Time is passed in, health reports are synthetic
// and the probe is a fake, so nothing waits on a clock or the network.

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect"
)

func TestDnsResolverStateOf(t *testing.T) {
	for _, c := range []struct {
		health   connect.DnsResolverHealthState
		failover bool
		state    string
	}{
		{health: connect.DnsResolverHealthCold, state: DnsResolverStateUnknown},
		{health: connect.DnsResolverHealthOk, state: DnsResolverStateOk},
		{health: connect.DnsResolverHealthFailing, state: DnsResolverStateFailing},
		{health: connect.DnsResolverHealthFailing, failover: true, state: DnsResolverStateFailing},
		{health: connect.DnsResolverHealthFailedOver, failover: true, state: DnsResolverStateFailedOver},
		// only a selection with a failover reads failed over
		{health: connect.DnsResolverHealthFailedOver, state: DnsResolverStateFailing},
		{health: "", state: DnsResolverStateUnknown},
	} {
		connect.AssertEqual(t, dnsResolverStateOf(c.health, c.failover), c.state)
	}
}

// A tracker on a selection, following a mux.
func testingDnsResolverTracker(t *testing.T, presetId string, personalId string, now time.Time) (*dnsResolverStatusTracker, *connect.UpgradeMux) {
	t.Helper()
	tracker := newDnsResolverStatusTracker()
	tracker.setSelection(presetId, personalId)
	upgradeMux := &connect.UpgradeMux{}
	tracker.setMux(upgradeMux, now)
	return &tracker, upgradeMux
}

// The state is unknown while the window forms and the resolver is cold,
// follows the health once it has answered or failed, and returns to ok when
// the resolver answers again; a report from a retired mux changes nothing.
func TestDnsResolverStatusTrackerFollowsHealth(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	start := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	tracker, upgradeMux := testingDnsResolverTracker(t, "alpha-ads", "", start)
	selection := resolveDnsResolverSelection("alpha-ads", "")
	report := func(state connect.DnsResolverHealthState, at time.Time) bool {
		changed, _ := tracker.healthChanged(upgradeMux, connect.DnsResolverHealth{State: state}, selection, at, time.Hour)
		return changed
	}
	connect.AssertEqual(t, tracker.status().State, DnsResolverStateUnknown)
	connect.AssertEqual(t, report(connect.DnsResolverHealthCold, start.Add(time.Second)), false)
	connect.AssertEqual(t, report(connect.DnsResolverHealthOk, start.Add(2*time.Second)), true)
	connect.AssertEqual(t, tracker.status().SinceTime, start.Add(2*time.Second).UnixMilli())
	connect.AssertEqual(t, report(connect.DnsResolverHealthOk, start.Add(3*time.Second)), false)
	connect.AssertEqual(t, report(connect.DnsResolverHealthFailing, start.Add(4*time.Second)), true)
	connect.AssertEqual(t, report(connect.DnsResolverHealthFailedOver, start.Add(5*time.Second)), true)
	connect.AssertEqual(t, tracker.status().State, DnsResolverStateFailedOver)
	connect.AssertEqual(t, report(connect.DnsResolverHealthOk, start.Add(6*time.Second)), true)
	connect.AssertEqual(t, tracker.status(), &DnsResolverStatus{
		State: DnsResolverStateOk, Verification: DnsResolverVerificationNone, PresetId: "alpha-ads",
		SinceTime: start.Add(6 * time.Second).UnixMilli(),
	})
	retired := &connect.UpgradeMux{}
	if changed, probe := tracker.healthChanged(retired, connect.DnsResolverHealth{State: connect.DnsResolverHealthFailing}, selection, start, time.Hour); changed || probe {
		t.Fatal("a retired mux's report moved the status")
	}
	// a new mux starts unknown
	connect.AssertEqual(t, tracker.setMux(&connect.UpgradeMux{}, start.Add(7*time.Second)), true)
	connect.AssertEqual(t, tracker.status().State, DnsResolverStateUnknown)

	// a family preset has no failover: a failed over report reads failing
	family, familyMux := testingDnsResolverTracker(t, "alpha-family", "", start)
	family.healthChanged(familyMux, connect.DnsResolverHealth{State: connect.DnsResolverHealthFailedOver}, resolveDnsResolverSelection("alpha-family", ""), start, time.Hour)
	connect.AssertEqual(t, family.status().State, DnsResolverStateFailing)
}

// A probe is due once the resolver has answered or failed with a preset that
// has something to verify, at once for a new selection or a new mux, and
// again after a generation change at most once per interval (C6).
func TestDnsResolverStatusTrackerProbeSchedule(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	start := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	interval := 5 * time.Minute
	tracker, upgradeMux := testingDnsResolverTracker(t, "alpha-ads", "", start)
	selection := resolveDnsResolverSelection("alpha-ads", "")
	probe := func(state connect.DnsResolverHealthState, at time.Time) bool {
		_, due := tracker.healthChanged(upgradeMux, connect.DnsResolverHealth{State: state}, selection, at, interval)
		return due
	}
	connect.AssertEqual(t, probe(connect.DnsResolverHealthCold, start), false)
	connect.AssertEqual(t, probe(connect.DnsResolverHealthOk, start.Add(time.Second)), true)
	// a generation change within the interval: no second probe
	connect.AssertEqual(t, probe(connect.DnsResolverHealthCold, start.Add(time.Minute)), false)
	connect.AssertEqual(t, probe(connect.DnsResolverHealthOk, start.Add(2*time.Minute)), false)
	// past the interval it probes again
	connect.AssertEqual(t, probe(connect.DnsResolverHealthCold, start.Add(6*time.Minute)), false)
	connect.AssertEqual(t, probe(connect.DnsResolverHealthFailing, start.Add(6*time.Minute)), true)
	// a new selection probes at once
	tracker.setSelection("beta-security", "")
	selection = resolveDnsResolverSelection("beta-security", "")
	connect.AssertEqual(t, probe(connect.DnsResolverHealthOk, start.Add(7*time.Minute)), true)
	// so does a new mux
	upgradeMux = &connect.UpgradeMux{}
	tracker.setMux(upgradeMux, start.Add(8*time.Minute))
	connect.AssertEqual(t, probe(connect.DnsResolverHealthOk, start.Add(8*time.Minute)), true)
	// a non-positive interval probes at every change
	for i := range 3 {
		if _, due := tracker.healthChanged(upgradeMux, connect.DnsResolverHealth{State: connect.DnsResolverHealthOk}, selection, start.Add(9*time.Minute), 0); !due {
			t.Fatalf("change %d: no probe with a zero interval", i)
		}
	}
	// a one-tap preset without a canary, and every selection that is no
	// preset, has nothing to verify
	for _, presetId := range []string{"beta-plain", "beta-unfiltered", DnsPresetIdDefault, ""} {
		plain, plainMux := testingDnsResolverTracker(t, presetId, "", start)
		if _, due := plain.healthChanged(plainMux, connect.DnsResolverHealth{State: connect.DnsResolverHealthOk}, resolveDnsResolverSelection(presetId, ""), start, interval); due {
			t.Errorf("%q has nothing to verify and was probed", presetId)
		}
	}
	// a personal preset is probed with any name
	connect.AssertEqual(t, dnsResolverVerifyNameOf(resolveDnsResolverSelection("gamma-personal", "abc123").preset), "example.com")
	connect.AssertEqual(t, dnsResolverVerifyNameOf(resolveDnsResolverSelection("alpha-ads", "").preset), "ads.canary.example")
}

// A probe's verification counts only for the mux and selection it ran on; a
// new selection starts the verification over.
func TestDnsResolverStatusTrackerDropsStaleVerification(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	start := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	tracker, upgradeMux := testingDnsResolverTracker(t, "gamma-personal", "abc123", start)
	key := dnsResolverSelectionKey("gamma-personal", "abc123")
	connect.AssertEqual(t, tracker.verified(&connect.UpgradeMux{}, key, DnsResolverVerificationRejected), false)
	connect.AssertEqual(t, tracker.verified(upgradeMux, dnsResolverSelectionKey("gamma-personal", "def456"), DnsResolverVerificationRejected), false)
	connect.AssertEqual(t, tracker.verified(upgradeMux, key, DnsResolverVerificationUnconfirmed), true)
	connect.AssertEqual(t, tracker.verified(upgradeMux, key, DnsResolverVerificationUnconfirmed), false)
	connect.AssertEqual(t, tracker.status().Verification, DnsResolverVerificationUnconfirmed)
	connect.AssertEqual(t, tracker.setSelection("gamma-personal", "def456"), true)
	connect.AssertEqual(t, tracker.status().Verification, DnsResolverVerificationNone)
	connect.AssertEqual(t, tracker.setSelection("gamma-personal", "def456"), false)
}

func testingProbeResult(rcode dnsmessage.RCode, addrs []string, edeCodes ...uint16) connect.DnsResolverProbeResult {
	result := connect.DnsResolverProbeResult{
		Url:      "https://probe.example/dns-query",
		Answered: true,
		RCode:    rcode,
		EdeCodes: edeCodes,
	}
	for _, addr := range addrs {
		result.Addrs = append(result.Addrs, netip.MustParseAddr(addr))
	}
	return result
}

// Each row of the verification table (C6): a one-tap preset's canary blocked
// is confirmed and resolved is not filtering; Control D's refusal is a
// rejected id and AdGuard's server failure a malformed one; a personal answer
// is unconfirmed, never confirmed; no answer says nothing.
func TestDnsResolverVerificationOf(t *testing.T) {
	catalog := testingWithDnsPresetCatalog(t)
	unanswered := connect.DnsResolverProbeResult{Url: "https://probe.example/dns-query", Err: fmt.Errorf("timeout")}
	for _, c := range []struct {
		presetId     string
		results      []connect.DnsResolverProbeResult
		verification string
	}{
		{presetId: "alpha-ads", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeSuccess, []string{"0.0.0.0"})}, verification: DnsResolverVerificationConfirmed},
		{presetId: "alpha-ads", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeSuccess, []string{"192.0.2.99"})}, verification: DnsResolverVerificationNotFiltering},
		{presetId: "alpha-ads", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeSuccess, []string{"0.0.0.0", "192.0.2.99"})}, verification: DnsResolverVerificationNotFiltering},
		{presetId: "alpha-ads", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeSuccess, nil)}, verification: DnsResolverVerificationNone},
		{presetId: "alpha-ads", results: []connect.DnsResolverProbeResult{unanswered}, verification: DnsResolverVerificationNone},
		{presetId: "alpha-ads", results: nil, verification: DnsResolverVerificationNone},
		{presetId: "alpha-ads", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeRefused, nil)}, verification: DnsResolverVerificationNone},
		{presetId: "beta-security", results: []connect.DnsResolverProbeResult{
			testingProbeResult(dnsmessage.RCodeNameError, nil, 17),
			testingProbeResult(dnsmessage.RCodeNameError, nil, 15),
		}, verification: DnsResolverVerificationConfirmed},
		{presetId: "beta-security", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeNameError, nil)}, verification: DnsResolverVerificationNone},
		{presetId: "beta-security", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeNameError, nil, 18)}, verification: DnsResolverVerificationNone},
		// one url of a pair resolving the canary is enough: it leaks
		{presetId: "beta-security", results: []connect.DnsResolverProbeResult{
			testingProbeResult(dnsmessage.RCodeNameError, nil, 17),
			testingProbeResult(dnsmessage.RCodeSuccess, []string{"192.0.2.99"}),
		}, verification: DnsResolverVerificationNotFiltering},
		{presetId: "beta-security", results: []connect.DnsResolverProbeResult{
			testingProbeResult(dnsmessage.RCodeNameError, nil, 17),
			unanswered,
		}, verification: DnsResolverVerificationConfirmed},
		// a one-tap preset without a canary never reads unconfirmed
		{presetId: "beta-plain", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeSuccess, []string{"192.0.2.99"})}, verification: DnsResolverVerificationNone},
		{presetId: "gamma-personal", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeRefused, nil)}, verification: DnsResolverVerificationRejected},
		{presetId: "gamma-personal", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeServerFailure, nil)}, verification: DnsResolverVerificationNone},
		{presetId: "gamma-personal", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeSuccess, []string{"192.0.2.99"})}, verification: DnsResolverVerificationUnconfirmed},
		// a personal preset is never confirmed, even by a sink
		{presetId: "gamma-personal", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeSuccess, []string{"0.0.0.0"})}, verification: DnsResolverVerificationUnconfirmed},
		{presetId: "alpha-personal", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeServerFailure, nil)}, verification: DnsResolverVerificationMalformed},
		{presetId: "alpha-personal", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeRefused, nil)}, verification: DnsResolverVerificationNone},
		{presetId: "alpha-personal", results: []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeNameError, nil)}, verification: DnsResolverVerificationUnconfirmed},
		{presetId: "alpha-personal", results: []connect.DnsResolverProbeResult{unanswered}, verification: DnsResolverVerificationNone},
	} {
		got := dnsResolverVerificationOf(catalog.preset(c.presetId), c.results)
		if got != c.verification {
			t.Errorf("%s %+v: %q, want %q", c.presetId, c.results, got, c.verification)
		}
	}
	connect.AssertEqual(t, dnsResolverVerificationOf(nil, nil), DnsResolverVerificationNone)
}

// A device that builds and replaces actual muxes, with candidate enumeration
// empty, so no public network is touched (device_local_dns_owner_test.go).
func testingDnsPresetDevice(t *testing.T, configure func(settings *DeviceLocalSettings)) *DeviceLocal {
	t.Helper()
	fixture := testingPairedAuthSpace(t)
	fixture.seedDistinctLogin(t)
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.AllowProvider = false
	settings.GeneratorFunc = func([]*connect.ProviderSpec) connect.MultiClientGenerator { return &testingDnsOwnerGenerator{} }
	if configure != nil {
		configure(settings)
	}
	device, err := newDeviceLocalWithOverrides(fixture.networkSpace, fixture.initialJwt, "dns-preset", "test", "0",
		fixture.instanceId, settings, connect.NewId())
	if err != nil {
		t.Fatal("could not build a dns preset device")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := device.CloseAndWait(ctx); err != nil {
			t.Error("dns preset device did not join")
		}
	})
	return device
}

// Collects the statuses a device publishes.
type testingDnsResolverStatusListener struct {
	statuses chan *DnsResolverStatus
}

func newTestingDnsResolverStatusListener() *testingDnsResolverStatusListener {
	return &testingDnsResolverStatusListener{statuses: make(chan *DnsResolverStatus, 64)}
}

func (self *testingDnsResolverStatusListener) DnsResolverStatusChanged(status *DnsResolverStatus) {
	self.statuses <- status
}

// The next published status that satisfies match, waiting on the listener's
// channel; the deadline only bounds a failing test.
func (self *testingDnsResolverStatusListener) next(t *testing.T, what string, match func(*DnsResolverStatus) bool) *DnsResolverStatus {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case status := <-self.statuses:
			if match(status) {
				return status
			}
		case <-deadline:
			t.Fatalf("no status %s", what)
			return nil
		}
	}
}

// A blocked count the test sets, recording resets.
type testingDnsBlockedCounter struct {
	stateLock  sync.Mutex
	count      uint64
	resetCount int
}

func (self *testingDnsBlockedCounter) DnsBlockedCount() uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.count
}

func (self *testingDnsBlockedCounter) ResetDnsBlockedCount() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.count = 0
	self.resetCount += 1
}

func (self *testingDnsBlockedCounter) set(count uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.count = count
}

func (self *testingDnsBlockedCounter) resets() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.resetCount
}

// Points the device's status at a mux that is never live, with a blocked
// count the test sets, so health reports and probes are the test's alone.
func testingDnsResolverFakeMux(device *DeviceLocal) (*connect.UpgradeMux, *testingDnsBlockedCounter) {
	upgradeMux := &connect.UpgradeMux{}
	counter := &testingDnsBlockedCounter{}
	device.stateLock.Lock()
	defer device.stateLock.Unlock()
	device.dnsResolver.setMux(upgradeMux, time.Now())
	device.dnsResolver.blockedCounter = counter
	return upgradeMux, counter
}

// The device publishes its status on a selection change, a health change and
// a verification, and the probe runs once the resolver has answered (C5, C6).
func TestDeviceLocalDnsResolverStatusFollowsHealthAndVerification(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	device := testingDnsPresetDevice(t, nil)
	probes := make(chan string, 8)
	device.testingDnsResolverProbe = func(ctx context.Context, upgradeMux *connect.UpgradeMux, name string) []connect.DnsResolverProbeResult {
		probes <- name
		return []connect.DnsResolverProbeResult{testingProbeResult(dnsmessage.RCodeRefused, nil)}
	}
	listener := newTestingDnsResolverStatusListener()
	sub := device.AddDnsResolverStatusChangeListener(listener)
	defer sub.Close()
	upgradeMux, _ := testingDnsResolverFakeMux(device)

	connect.AssertEqual(t, device.GetDnsResolverStatus(), &DnsResolverStatus{
		State: DnsResolverStateUnknown, Verification: DnsResolverVerificationNone, PresetId: DnsPresetIdDefault,
	})
	device.SetDnsResolverSettings(testingPresetSettings(t, "gamma-personal", "abc123"))
	connect.AssertEqual(t, device.GetDnsResolverSettings().PresetId, "gamma-personal")
	listener.next(t, "for the new selection", func(status *DnsResolverStatus) bool {
		return status.PresetId == "gamma-personal" && status.State == DnsResolverStateUnknown
	})
	device.dnsResolverHealthChanged(upgradeMux, connect.DnsResolverHealth{State: connect.DnsResolverHealthFailing})
	listener.next(t, "failing", func(status *DnsResolverStatus) bool { return status.State == DnsResolverStateFailing })
	select {
	case name := <-probes:
		connect.AssertEqual(t, name, "example.com")
	case <-time.After(10 * time.Second):
		t.Fatal("no verification probe")
	}
	rejected := listener.next(t, "rejected", func(status *DnsResolverStatus) bool {
		return status.Verification == DnsResolverVerificationRejected
	})
	connect.AssertEqual(t, rejected.PresetId, "gamma-personal")
	connect.AssertEqual(t, device.GetDnsResolverStatus().Verification, DnsResolverVerificationRejected)
	connect.AssertEqual(t, DescribeDnsResolverStatus(device.GetDnsResolverSettings(), device.GetDnsResolverStatus()).VerificationKey, "dns_verify_rejected_controld")

	// "Use URnetwork DNS": the selection is default and the status follows
	device.SetDnsResolverSettings(DnsResolverSettingsWithPreset(device.GetDnsResolverSettings(), DnsPresetIdDefault, ""))
	listener.next(t, "for default", func(status *DnsResolverStatus) bool {
		return status.PresetId == DnsPresetIdDefault && status.Verification == DnsResolverVerificationNone
	})
}

// The blocked count is the live mux's, read by the device; a change of
// preset zeroes it so it names the selected provider, and a Fast DNS toggle
// keeps it (I3). It rides the status when read, and nothing logs it.
func TestDeviceLocalDnsBlockedCount(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	device := testingDnsPresetDevice(t, nil)
	connect.AssertEqual(t, device.GetDnsBlockedCount(), int64(0))
	_, counter := testingDnsResolverFakeMux(device)
	device.SetDnsResolverSettings(testingPresetSettings(t, "alpha-ads", ""))
	resets := counter.resets()
	counter.set(42)
	connect.AssertEqual(t, device.GetDnsBlockedCount(), int64(42))
	connect.AssertEqual(t, device.GetDnsResolverStatus().BlockedCount, int64(42))

	device.SetDnsResolverSettings(DnsResolverSettingsWithFallback(device.GetDnsResolverSettings(), true))
	connect.AssertEqual(t, device.GetDnsResolverSettings().EnableFallback, true)
	connect.AssertEqual(t, counter.resets(), resets)
	connect.AssertEqual(t, device.GetDnsBlockedCount(), int64(42))

	device.SetDnsResolverSettings(DnsResolverSettingsWithPreset(device.GetDnsResolverSettings(), "beta-security", ""))
	connect.AssertEqual(t, counter.resets(), resets+1)
	connect.AssertEqual(t, device.GetDnsBlockedCount(), int64(0))

	counter.set(1 << 63)
	connect.AssertEqual(t, device.GetDnsBlockedCount(), int64(1<<63-1))
}

// The status follows the actual mux the device builds: a connect subscribes
// to its health, a reconnect moves to the replacement, and a disconnect reads
// unknown with no count.
func TestDeviceLocalDnsResolverStatusFollowsTheLiveMux(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	device := testingDnsPresetDevice(t, nil)
	device.SetDnsResolverSettings(testingPresetSettings(t, "alpha-ads", ""))
	device.SetConnectLocation(testingStoredLocation("live-destination"))
	device.stateLock.Lock()
	first := device.upgradeMux
	followed := device.dnsResolver.upgradeMux
	subscribed := device.dnsResolver.healthSub != nil
	failover := device.upgradeMuxSettings.Dns.Failover
	device.stateLock.Unlock()
	if first == nil || followed != first || !subscribed {
		t.Fatalf("the status does not follow the live mux: live %p followed %p subscribed %t", first, followed, subscribed)
	}
	connect.AssertEqual(t, failover, defaultDnsResolver())
	connect.AssertEqual(t, device.GetDnsBlockedCount(), int64(0))
	connect.AssertEqual(t, device.GetDnsResolverStatus().State, DnsResolverStateUnknown)

	device.Reconnect(testingStoredLocation("live-destination"))
	device.stateLock.Lock()
	replacement := device.upgradeMux
	followed = device.dnsResolver.upgradeMux
	device.stateLock.Unlock()
	if replacement == nil || replacement == first || followed != replacement {
		t.Fatal("the status did not move to the replacement mux")
	}
	// a late report of the retired mux changes nothing
	device.dnsResolverHealthChanged(first, connect.DnsResolverHealth{State: connect.DnsResolverHealthFailing})
	connect.AssertEqual(t, device.GetDnsResolverStatus().State, DnsResolverStateUnknown)

	device.RemoveDestination()
	device.stateLock.Lock()
	followed = device.dnsResolver.upgradeMux
	device.stateLock.Unlock()
	if followed != nil {
		t.Fatal("the status follows a closed mux")
	}
	connect.AssertEqual(t, device.GetDnsBlockedCount(), int64(0))
	connect.AssertEqual(t, device.GetDnsResolverStatus().State, DnsResolverStateUnknown)
}

// Records every line a device logs, every verbose level included.
type testingDnsLogLogger struct {
	stateLock sync.Mutex
	lines     []string
}

func (self *testingDnsLogLogger) record(line string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, line)
}

func (self *testingDnsLogLogger) Info(args ...any)                   { self.record(fmt.Sprint(args...)) }
func (self *testingDnsLogLogger) Infof(format string, args ...any)    { self.record(fmt.Sprintf(format, args...)) }
func (self *testingDnsLogLogger) Warningf(format string, args ...any) { self.record(fmt.Sprintf(format, args...)) }
func (self *testingDnsLogLogger) Errorf(format string, args ...any)   { self.record(fmt.Sprintf(format, args...)) }
func (self *testingDnsLogLogger) V(level int32) connect.Verbose {
	return testingDnsLogVerbose{logger: self}
}

func (self *testingDnsLogLogger) all() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.lines...)
}

type testingDnsLogVerbose struct{ logger *testingDnsLogLogger }

func (self testingDnsLogVerbose) Enabled() bool                    { return true }
func (self testingDnsLogVerbose) Info(args ...any)                 { self.logger.record(fmt.Sprint(args...)) }
func (self testingDnsLogVerbose) Infof(format string, args ...any) { self.logger.record(fmt.Sprintf(format, args...)) }

// Applying a personal preset, a refused id, every status and verification
// change and a connect write no personal id, no url path and no blocked count
// to the device log, at any level; the [dns] line names the preset only
// (D5, I2, I3).
func TestDeviceLocalDnsLogsNoPersonalIdOrCount(t *testing.T) {
	testingWithDnsPresetCatalog(t)
	logger := &testingDnsLogLogger{}
	device := testingDnsPresetDevice(t, func(settings *DeviceLocalSettings) {
		settings.DisableLogging = false
		settings.ClientSettings.Log = logger
	})
	personalId := "c0ffee"
	device.testingDnsResolverProbe = func(ctx context.Context, upgradeMux *connect.UpgradeMux, name string) []connect.DnsResolverProbeResult {
		result := testingProbeResult(dnsmessage.RCodeSuccess, []string{"192.0.2.99"})
		result.Url = "https://id.gamma.example/" + personalId
		return []connect.DnsResolverProbeResult{result}
	}
	listener := newTestingDnsResolverStatusListener()
	sub := device.AddDnsResolverStatusChangeListener(listener)
	defer sub.Close()

	device.SetDnsResolverSettings(testingPresetSettings(t, "gamma-personal", personalId))
	// a refused id is refused without naming it
	device.SetDnsResolverSettings(&DnsResolverSettings{PresetId: "gamma-personal", PresetPersonalId: "c0ffee/../x"})
	connect.AssertEqual(t, device.GetDnsResolverSettings().PresetPersonalId, personalId)
	device.SetConnectLocation(testingStoredLocation("live-destination"))
	upgradeMux, counter := testingDnsResolverFakeMux(device)
	counter.set(424242)
	device.dnsResolverHealthChanged(upgradeMux, connect.DnsResolverHealth{State: connect.DnsResolverHealthOk, BlockedAnswerCount: 424242})
	listener.next(t, "unconfirmed", func(status *DnsResolverStatus) bool {
		return status.Verification == DnsResolverVerificationUnconfirmed
	})
	device.dnsResolverHealthChanged(upgradeMux, connect.DnsResolverHealth{State: connect.DnsResolverHealthFailing})
	listener.next(t, "failing", func(status *DnsResolverStatus) bool { return status.State == DnsResolverStateFailing })
	connect.AssertEqual(t, device.GetDnsBlockedCount(), int64(424242))

	dnsLines := 0
	for _, line := range logger.all() {
		for _, secret := range []string{personalId, "424242", "id.gamma.example/"} {
			if strings.Contains(strings.ToLower(line), secret) {
				t.Errorf("the log carries %q: %s", secret, line)
			}
		}
		if strings.HasPrefix(line, "[dns]") {
			dnsLines += 1
			if !strings.HasPrefix(line, "[dns]preset=gamma-personal personal=yes state=") &&
				!strings.HasPrefix(line, "[dns]preset=default personal=no state=") {
				t.Errorf("unexpected dns line: %s", line)
			}
		}
	}
	if dnsLines == 0 {
		t.Fatal("the device logged no [dns] line for support")
	}
}
