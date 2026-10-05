package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The provider status binding and view controller against a fake api: the
// wire shape, polling, stop, listener updates and failed polls.

const providerStatusTestOwnClientId = "00000000-0000-0000-0000-0000000000a1"
const providerStatusTestOtherClientId = "00000000-0000-0000-0000-0000000000a2"

// The server's GET /network/provider-status answer for a network of two
// provider clients, the caller's own first, as the server marshals it.
func providerStatusTestJson(ownReason string, ownCount int) string {
	counts := make([]string, 60)
	for i := range counts {
		counts[i] = "0"
	}
	counts[59] = fmt.Sprint(ownCount)
	counts[10] = "4"
	return fmt.Sprintf(`{
		"providers": [
			{
				"client_id": %q,
				"reason": %q,
				"reason_text": "Building reliability.",
				"admission": {
					"connected": true,
					"location_valid": true,
					"provide_public": true,
					"reliability_ok": false,
					"speed_test_done": true,
					"egress": "pass",
					"egress_measured_at": "2026-10-04T11:00:00Z"
				},
				"ranking": [
					{"name": "reliability_1h", "has_value": true, "value": 0.99, "has_minimum": true, "minimum": 0.95, "has_maximum": false, "maximum": 0, "passes": true, "count": 0, "total": 0, "explanation": "How steadily this device stayed connected over the last hour: 0.99."},
					{"name": "reliability_12h", "has_value": true, "value": 0.31, "has_minimum": true, "minimum": 0.7, "has_maximum": false, "maximum": 0, "passes": false, "count": 0, "total": 0, "explanation": "How steadily this device stayed connected over the last 12 hours: 0.31."},
					{"name": "url_checks", "has_value": true, "value": 0.95, "has_minimum": true, "minimum": 0.8, "has_maximum": false, "maximum": 0, "passes": true, "count": 19, "total": 20, "explanation": "Test sites that loaded: 19 of 20."}
				],
				"country": {"country_code": "us", "country": "United States", "observed_country_code": "us", "explanation": "Clients who choose United States can be offered this device."},
				"evaluate_time": "2026-10-04T12:00:00Z",
				"appearances": {"start_minute": 29851140, "bucket_seconds": 60, "appearances_per_minute": [%s]}
			},
			{
				"client_id": %q,
				"reason": "not_connected",
				"reason_text": "This device isn't connected right now.",
				"admission": {"connected": false, "location_valid": false, "provide_public": true, "reliability_ok": true, "speed_test_done": false, "egress": "unprobed"},
				"ranking": [],
				"evaluate_time": "2026-10-04T12:00:00Z"
			}
		],
		"truncated": true
	}`, providerStatusTestOwnClientId, ownReason, strings.Join(counts, ","), providerStatusTestOtherClientId)
}

func TestGetProviderStatusDecodeMatchesServer(t *testing.T) {
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/network/provider-status" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, providerStatusTestJson(ProviderStatusReasonReliabilityWarmingUp, 7))
	}))

	callback, c := connect.NewBlockingApiCallback[*GetProviderStatusResult](context.Background())
	api.GetProviderStatus(callback)
	r := awaitApiResult(t, c, "GetProviderStatus never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	result := r.Result
	if !result.Truncated || result.Providers == nil || result.Providers.Len() != 2 {
		t.Fatalf("result = %+v", result)
	}
	own := result.Providers.Get(0)
	if own.ClientId.String() != providerStatusTestOwnClientId || own.Reason != ProviderStatusReasonReliabilityWarmingUp || own.ReasonText != "Building reliability." {
		t.Fatalf("own = %+v", own)
	}
	if own.Admission == nil || !own.Admission.Connected || own.Admission.ReliabilityOk || own.Admission.Egress != ProviderEgressPass ||
		own.Admission.EgressMeasuredAt == nil || own.Admission.EgressMeasuredAt.UnixMilli() != time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("admission = %+v", own.Admission)
	}
	if own.Ranking == nil || own.Ranking.Len() != 3 {
		t.Fatalf("ranking = %+v", own.Ranking)
	}
	urlChecks := own.Ranking.Get(2)
	if urlChecks.Name != ProviderStatusNumberUrlChecks || urlChecks.Count != 19 || urlChecks.Total != 20 || !urlChecks.HasMinimum || urlChecks.Minimum != 0.8 || !urlChecks.Passes {
		t.Fatalf("url_checks = %+v", urlChecks)
	}
	if halfDay := own.Ranking.Get(1); halfDay.Passes || halfDay.Value != 0.31 || halfDay.Explanation == "" {
		t.Fatalf("reliability_12h = %+v", halfDay)
	}
	if own.Country == nil || own.Country.Country != "United States" || own.Country.ObservedCountryCode != "us" {
		t.Fatalf("country = %+v", own.Country)
	}
	if own.EvaluateTime == nil || own.Appearances == nil || own.Appearances.StartMinute != 29851140 || own.Appearances.BucketSeconds != 60 {
		t.Fatalf("appearances = %+v", own.Appearances)
	}
	counts := own.Appearances.AppearancesPerMinute
	if counts == nil || counts.Len() != 60 || counts.Get(59) != 7 || counts.Get(10) != 4 || counts.Get(0) != 0 {
		t.Fatalf("counts = %+v", counts)
	}
	other := result.Providers.Get(1)
	if other.Appearances != nil || other.Country != nil || other.Ranking.Len() != 0 || other.Admission.Egress != ProviderEgressUnprobed {
		t.Fatalf("other = %+v", other)
	}
}

// A fake api: each poll is recorded and answered when the test says.
type providerStatusTestFetches struct {
	stateLock sync.Mutex
	callbacks []GetProviderStatusCallback
}

func (self *providerStatusTestFetches) fetch(callback GetProviderStatusCallback) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.callbacks = append(self.callbacks, callback)
}

func (self *providerStatusTestFetches) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.callbacks)
}

func (self *providerStatusTestFetches) callback(i int) GetProviderStatusCallback {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.callbacks[i]
}

type providerStatusTestListener struct {
	changes atomic.Int32
}

func (self *providerStatusTestListener) ProviderStatusChanged() {
	self.changes.Add(1)
}

func newProviderStatusTestResult(t *testing.T, ownReason string, ownCount int) *GetProviderStatusResult {
	t.Helper()
	result := &GetProviderStatusResult{}
	if err := json.Unmarshal([]byte(providerStatusTestJson(ownReason, ownCount)), result); err != nil {
		t.Fatal(err)
	}
	return result
}

func newProviderStatusTestController(t *testing.T) (*ProviderStatusViewController, *providerStatusTestFetches, *providerStatusTestListener, func(time.Duration)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	clientId := mustParseTestId(t, providerStatusTestOwnClientId)
	vc := newProviderStatusViewControllerWithoutRun(ctx, nil, clientId)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	vc.nowFunc = func() time.Time { return now }
	fetches := &providerStatusTestFetches{}
	vc.fetchFunc = fetches.fetch
	listener := &providerStatusTestListener{}
	vc.AddProviderStatusListener(listener)
	advance := func(d time.Duration) { now = now.Add(d) }
	return vc, fetches, listener, advance
}

// Nothing polls until Start; Start polls at once, then once per interval,
// never two at a time, and Refresh polls early.
func TestProviderStatusViewControllerPolls(t *testing.T) {
	vc, fetches, listener, advance := newProviderStatusTestController(t)

	if _, arm := vc.step(); arm || fetches.count() != 0 {
		t.Fatal("polled before Start")
	}
	vc.Start()
	if _, arm := vc.step(); arm || fetches.count() != 1 || !vc.GetIsLoading() {
		t.Fatalf("start: arm %t, %d fetches, loading %t", arm, fetches.count(), vc.GetIsLoading())
	}
	// in flight: no second poll, even once the next one is due
	advance(defaultProviderStatusPollInterval)
	if vc.step(); fetches.count() != 1 {
		t.Fatal("a second poll overlapped the first")
	}
	advance(-defaultProviderStatusPollInterval + 10*time.Second)

	fetches.callback(0).Result(newProviderStatusTestResult(t, ProviderStatusReasonReliabilityWarmingUp, 7), nil)
	if listener.changes.Load() != 1 || !vc.GetIsLoaded() || vc.GetIsLoading() || vc.GetLastFetchError() != "" {
		t.Fatalf("changes %d loaded %t loading %t err %q", listener.changes.Load(), vc.GetIsLoaded(), vc.GetIsLoading(), vc.GetLastFetchError())
	}
	status := vc.GetProviderStatus()
	if status == nil || status.ClientId.String() != providerStatusTestOwnClientId {
		t.Fatalf("status = %+v", status)
	}
	if vc.GetReason() != ProviderStatusReasonReliabilityWarmingUp || vc.GetReasonText() != "Building reliability." || vc.GetAdmission() == nil || !vc.GetTruncated() {
		t.Fatal("this device's reason, admission or truncation")
	}
	if vc.GetRankingNumbers().Len() != 3 || vc.GetProviderStatuses().Len() != 2 {
		t.Fatal("numbers or statuses")
	}
	if vc.GetAppearancesPerMinute().Len() != 60 || vc.GetAppearanceTotal() != 11 || vc.GetAppearanceMaxCount() != 7 || vc.GetAppearances().StartMinute != 29851140 {
		t.Fatalf("histogram total %d max %d", vc.GetAppearanceTotal(), vc.GetAppearanceMaxCount())
	}

	// the next poll is due one interval after the last one started
	delay, arm := vc.step()
	if !arm || delay != defaultProviderStatusPollInterval-10*time.Second || fetches.count() != 1 {
		t.Fatalf("delay %s arm %t", delay, arm)
	}
	advance(delay - time.Second)
	if vc.step(); fetches.count() != 1 {
		t.Fatal("polled before the interval")
	}
	advance(time.Second)
	if vc.step(); fetches.count() != 2 {
		t.Fatalf("%d fetches after an interval", fetches.count())
	}
	fetches.callback(1).Result(newProviderStatusTestResult(t, ProviderStatusReasonNone, 9), nil)
	if vc.GetReason() != ProviderStatusReasonNone || vc.GetAppearanceMaxCount() != 9 || listener.changes.Load() != 2 {
		t.Fatal("the second poll was not published")
	}

	vc.Refresh()
	if vc.step(); fetches.count() != 3 {
		t.Fatal("Refresh did not poll early")
	}
}

// Stop pauses polling and drops the poll in flight; its late answer is
// ignored and the last snapshot stays published; Start polls again at once.
func TestProviderStatusViewControllerStop(t *testing.T) {
	vc, fetches, listener, advance := newProviderStatusTestController(t)
	vc.Start()
	vc.step()
	fetches.callback(0).Result(newProviderStatusTestResult(t, ProviderStatusReasonNone, 3), nil)
	advance(defaultProviderStatusPollInterval)
	vc.step()
	if fetches.count() != 2 || !vc.GetIsLoading() {
		t.Fatal("no second poll in flight")
	}

	vc.Stop()
	changes := listener.changes.Load()
	if vc.GetIsLoading() || changes != 2 {
		t.Fatalf("loading %t changes %d", vc.GetIsLoading(), changes)
	}
	fetches.callback(1).Result(newProviderStatusTestResult(t, ProviderStatusReasonSlow, 1), nil)
	if vc.GetReason() != ProviderStatusReasonNone || listener.changes.Load() != changes {
		t.Fatal("a poll answered after Stop was published")
	}
	advance(10 * defaultProviderStatusPollInterval)
	if _, arm := vc.step(); arm || fetches.count() != 2 {
		t.Fatal("polled while stopped")
	}
	if vc.GetProviderStatus() == nil || vc.GetAppearanceTotal() != 7 {
		t.Fatal("Stop dropped the last snapshot")
	}

	vc.Start()
	if vc.step(); fetches.count() != 3 {
		t.Fatal("Start did not poll at once")
	}
}

// A failed poll keeps the last snapshot, records the error and notifies; the
// next success clears the error.
func TestProviderStatusViewControllerFailedPoll(t *testing.T) {
	vc, fetches, listener, advance := newProviderStatusTestController(t)
	vc.Start()
	vc.step()
	fetches.callback(0).Result(nil, errors.New("503 Cached response is temporarily unavailable."))
	if vc.GetIsLoaded() || vc.GetLastFetchError() == "" || listener.changes.Load() != 1 || vc.GetProviderStatus() != nil {
		t.Fatal("a first failed poll")
	}
	if vc.GetAppearancesPerMinute().Len() != 0 || vc.GetRankingNumbers().Len() != 0 || vc.GetReason() != "" {
		t.Fatal("empty getters before a load")
	}

	advance(defaultProviderStatusPollInterval)
	vc.step()
	fetches.callback(1).Result(newProviderStatusTestResult(t, ProviderStatusReasonNone, 2), nil)
	if !vc.GetIsLoaded() || vc.GetLastFetchError() != "" {
		t.Fatal("a success did not clear the error")
	}

	advance(defaultProviderStatusPollInterval)
	vc.step()
	fetches.callback(2).Result(nil, errors.New("timeout"))
	if vc.GetLastFetchError() != "timeout" || vc.GetReason() != ProviderStatusReasonNone || listener.changes.Load() != 3 {
		t.Fatal("a failed poll dropped the snapshot")
	}
	// it retries at the next interval
	advance(defaultProviderStatusPollInterval)
	if vc.step(); fetches.count() != 4 {
		t.Fatal("no retry after a failed poll")
	}
}

// A device that is not one of the network's providers has no status of its
// own; the others are still listed.
func TestProviderStatusViewControllerWithoutOwnStatus(t *testing.T) {
	vc, fetches, _, _ := newProviderStatusTestController(t)
	vc.clientId = mustParseTestId(t, "00000000-0000-0000-0000-0000000000ff")
	vc.Start()
	vc.step()
	fetches.callback(0).Result(newProviderStatusTestResult(t, ProviderStatusReasonNone, 2), nil)
	if vc.GetProviderStatus() != nil || vc.GetReason() != "" || vc.GetAdmission() != nil || vc.GetAppearances() != nil || vc.GetProviderStatuses().Len() != 2 {
		t.Fatal("a non-provider device was given a status")
	}
}

// The real loop against a fake api server: it polls at the interval while
// started, stops polling on Stop and ends on Close.
func TestProviderStatusViewControllerRunLoop(t *testing.T) {
	var requests atomic.Int32
	ctx, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, providerStatusTestJson(ProviderStatusReasonNone, 5))
	}))
	vc := newProviderStatusViewControllerWithoutRun(ctx, api, mustParseTestId(t, providerStatusTestOwnClientId))
	vc.pollInterval = 30 * time.Millisecond
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		vc.run()
	}()

	waitFor := func(message string, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !condition() {
			if time.Now().After(deadline) {
				t.Fatal(message)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if time.Sleep(100 * time.Millisecond); requests.Load() != 0 {
		t.Fatal("polled before Start")
	}
	vc.Start()
	waitFor("did not poll repeatedly while started", func() bool { return 3 <= requests.Load() })
	waitFor("did not publish", func() bool { return vc.GetAppearanceTotal() == 9 })

	vc.Stop()
	time.Sleep(50 * time.Millisecond)
	stopped := requests.Load()
	if time.Sleep(200 * time.Millisecond); requests.Load() != stopped {
		t.Fatalf("polled while stopped: %d after %d", requests.Load(), stopped)
	}

	vc.Close()
	select {
	case <-loopDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not end the loop")
	}
}

func TestCloseConcreteProviderStatusViewControllerReleasesOwnership(t *testing.T) {
	device := newViewControllerCloseTestDevice(t)

	device.CloseProviderStatusViewController(device.OpenProviderStatusViewController())

	requireNoOwnedViewControllers(t, device)
}
