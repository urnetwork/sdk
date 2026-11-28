// The share limits' month, meter and files (provide_share.go), and the
// device's share state driven step by step with a fixed clock
// (device_local_provide_share.go).
package sdk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestProvideSharePeriodIsTheCalendarMonth(t *testing.T) {
	cases := []struct {
		now   time.Time
		start time.Time
		end   time.Time
	}{
		{
			now:   time.Date(2026, 11, 28, 18, 30, 0, 0, time.UTC),
			start: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			now:   time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC),
			start: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			now:   time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC),
			start: time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			// the first instant of a month is that month
			now:   time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
			start: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, c := range cases {
		start, end := provideSharePeriod(c.now, time.UTC)
		if !start.Equal(c.start) || !end.Equal(c.end) {
			t.Errorf("period of %s = [%s, %s), want [%s, %s)", c.now, start, end, c.start, c.end)
		}
	}
}

// Data of the same month continues; data of an earlier month, or a month
// that has ended, starts again from zero.
func TestProvideShareMeterKeepsOnlyTheCurrentMonth(t *testing.T) {
	now := time.Date(2026, 11, 28, 18, 30, 0, 0, time.UTC)
	november := &provideShareUsage{
		PeriodStartMillis: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).UnixMilli(),
		ByteCount:         5000,
	}
	if meter := newProvideShareMeter(ProvideShareLimits{}, november, now, time.UTC); meter.byteCount != 5000 {
		t.Fatalf("same month byte count = %d, want 5000", meter.byteCount)
	}
	october := &provideShareUsage{
		PeriodStartMillis: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli(),
		ByteCount:         5000,
	}
	if meter := newProvideShareMeter(ProvideShareLimits{}, october, now, time.UTC); meter.byteCount != 0 {
		t.Fatalf("earlier month byte count = %d, want 0", meter.byteCount)
	}

	meter := newProvideShareMeter(ProvideShareLimits{}, november, now, time.UTC)
	if meter.roll(now, time.UTC) {
		t.Fatal("meter rolled inside its month")
	}
	meter.add(time.Date(2026, 12, 1, 0, 0, 1, 0, time.UTC), time.UTC, 700)
	if meter.byteCount != 700 || !meter.periodStart.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("meter after the month = %d bytes from %s, want 700 from December", meter.byteCount, meter.periodStart)
	}
}

// The budget is reached at exactly the budget, and never without one.
func TestProvideShareMeterBudget(t *testing.T) {
	now := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	meter := newProvideShareMeter(ProvideShareLimits{MonthlyByteCount: 1000}, nil, now, time.UTC)
	meter.add(now, time.UTC, 999)
	if meter.budgetReached() {
		t.Fatal("budget reached one byte short")
	}
	meter.add(now, time.UTC, 1)
	if !meter.budgetReached() || !meter.status().BudgetReached {
		t.Fatal("budget not reached at the budget")
	}
	meter.limits = ProvideShareLimits{}
	if meter.budgetReached() {
		t.Fatal("budget reached without a budget")
	}
}

func TestProvideShareLimitsNormalizeNegativeToNone(t *testing.T) {
	limits := normalProvideShareLimits(&ProvideShareLimits{MaxBytesPerSecond: -1, MonthlyByteCount: -5})
	if limits != (ProvideShareLimits{}) {
		t.Fatalf("negative limits = %+v, want none", limits)
	}
	if limits := normalProvideShareLimits(nil); limits != (ProvideShareLimits{}) {
		t.Fatalf("nil limits = %+v, want none", limits)
	}
}

// The files round trip, and one that cannot be read keeps nothing.
func TestProvideShareStoreRoundTripsAndIgnoresCorruptFiles(t *testing.T) {
	store := &provideShareStore{dir: t.TempDir()}
	if _, stored := store.loadLimits(); stored {
		t.Fatal("empty store has limits")
	}
	if store.loadUsage() != nil {
		t.Fatal("empty store has usage")
	}
	limits := ProvideShareLimits{MaxBytesPerSecond: 125000, MonthlyByteCount: 50 * 1000 * 1000 * 1000}
	if err := store.saveLimits(limits); err != nil {
		t.Fatalf("save limits: %v", err)
	}
	usage := provideShareUsage{PeriodStartMillis: 1793491200000, ByteCount: 9007199254740993}
	if err := store.saveUsage(usage); err != nil {
		t.Fatalf("save usage: %v", err)
	}
	if loaded, stored := store.loadLimits(); !stored || loaded != limits {
		t.Fatalf("limits = %+v (%t), want %+v", loaded, stored, limits)
	}
	if loaded := store.loadUsage(); loaded == nil || *loaded != usage {
		t.Fatalf("usage = %+v, want %+v", loaded, usage)
	}
	entries, err := os.ReadDir(store.dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("store files = %v (%v), want the two files and no temporary", entries, err)
	}

	for _, name := range []string{provideShareLimitsFileName, provideShareUsageFileName} {
		if err := os.WriteFile(filepath.Join(store.dir, name), []byte("{\"max_bytes"), LocalStorageFilePermissions); err != nil {
			t.Fatal(err)
		}
	}
	if _, stored := store.loadLimits(); stored {
		t.Fatal("corrupt limits file kept limits")
	}
	if store.loadUsage() != nil {
		t.Fatal("corrupt usage file kept usage")
	}
}

// A test clock for the device's share state: fixed until advanced, with the
// loop's timers never firing on their own.
type provideShareTestClock struct {
	nowTime time.Time
}

func (self *provideShareTestClock) now() time.Time {
	return self.nowTime
}

func (self *provideShareTestClock) after(time.Duration) <-chan time.Time {
	return make(chan time.Time)
}

// Records each budget pause change and each status the listeners hear.
type provideShareTestRecorder struct {
	pauses   []bool
	statuses []*ProvideShareStatus
}

func (self *provideShareTestRecorder) ProvideShareStatusChanged(status *ProvideShareStatus) {
	self.statuses = append(self.statuses, status)
}

func newProvideShareForTest(
	t *testing.T,
	clock *provideShareTestClock,
	store *provideShareStore,
) (*deviceLocalProvideShare, *provideShareTestRecorder) {
	t.Helper()
	recorder := &provideShareTestRecorder{}
	share := newDeviceLocalProvideShareWithoutRun(
		context.Background(),
		defaultDeviceLocalProvideShareSettings(),
		store,
		time.UTC,
		connect.NewNoopLogger(),
		func(budgetPaused bool) {
			recorder.pauses = append(recorder.pauses, budgetPaused)
		},
		clock.now,
		clock.after,
	)
	share.addStatusChangeListener(recorder)
	return share, recorder
}

// The month's data reaching the budget pauses providing at once, and the next
// month resumes it by itself, from zero.
func TestDeviceLocalProvideShareBudgetPausesUntilTheNextMonth(t *testing.T) {
	clock := &provideShareTestClock{nowTime: time.Date(2026, 11, 28, 18, 0, 0, 0, time.UTC)}
	share, recorder := newProvideShareForTest(t, clock, &provideShareStore{dir: t.TempDir()})
	if err := share.setLimits(ProvideShareLimits{MonthlyByteCount: 1000}); err != nil {
		t.Fatalf("set limits: %v", err)
	}
	share.step()
	share.add(999)
	share.step()
	if len(recorder.pauses) != 0 || share.isBudgetPaused() {
		t.Fatalf("paused one byte short of the budget: %v", recorder.pauses)
	}

	share.add(1)
	untilNext := share.step()
	if len(recorder.pauses) != 1 || !recorder.pauses[0] || !share.isBudgetPaused() {
		t.Fatalf("budget pause changes = %v, want one pause", recorder.pauses)
	}
	last := recorder.statuses[len(recorder.statuses)-1]
	if !last.BudgetReached || last.ByteCount != 1000 || last.PeriodEndMillis != time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("status at the budget = %+v", last)
	}
	// reaching the budget saved at once, so the next thing due is the month's
	// end, which the loop wakes for without any traffic
	if want := 54 * time.Hour; untilNext != want {
		t.Fatalf("next step in %s, want %s at the month's end", untilNext, want)
	}

	// the month ends
	clock.nowTime = time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	share.step()
	if len(recorder.pauses) != 2 || recorder.pauses[1] || share.isBudgetPaused() {
		t.Fatalf("budget pause changes = %v, want the pause lifted", recorder.pauses)
	}
	last = recorder.statuses[len(recorder.statuses)-1]
	if last.BudgetReached || last.ByteCount != 0 || last.PeriodStartMillis != clock.nowTime.UnixMilli() {
		t.Fatalf("status in the new month = %+v", last)
	}
}

// Raising the budget past the month's data lifts the pause at the next step;
// lowering it below pauses again.
func TestDeviceLocalProvideShareBudgetChangeAppliesAtOnce(t *testing.T) {
	clock := &provideShareTestClock{nowTime: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	share, recorder := newProvideShareForTest(t, clock, nil)
	share.setLimits(ProvideShareLimits{MonthlyByteCount: 1000})
	share.add(1500)
	share.step()
	share.setLimits(ProvideShareLimits{MonthlyByteCount: 2000})
	share.step()
	share.setLimits(ProvideShareLimits{MonthlyByteCount: 1500})
	share.step()
	share.setLimits(ProvideShareLimits{})
	share.step()
	want := []bool{true, false, true, false}
	if len(recorder.pauses) != len(want) {
		t.Fatalf("budget pause changes = %v, want %v", recorder.pauses, want)
	}
	for i := range want {
		if recorder.pauses[i] != want[i] {
			t.Fatalf("budget pause changes = %v, want %v", recorder.pauses, want)
		}
	}
}

// The sharing speed is the limiter's rate, at once.
func TestDeviceLocalProvideShareLimitsSetTheSharingSpeed(t *testing.T) {
	clock := &provideShareTestClock{nowTime: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	share, _ := newProvideShareForTest(t, clock, nil)
	if share.limiter.ByteRate() != 0 {
		t.Fatalf("default sharing speed = %d, want none", share.limiter.ByteRate())
	}
	share.setLimits(ProvideShareLimits{MaxBytesPerSecond: 625000})
	if share.limiter.ByteRate() != 625000 {
		t.Fatalf("sharing speed = %d, want 625000", share.limiter.ByteRate())
	}
	share.setLimits(ProvideShareLimits{})
	if share.limiter.ByteRate() != 0 {
		t.Fatalf("sharing speed after removing the cap = %d, want none", share.limiter.ByteRate())
	}
}

// A restart keeps the limits and the month's data, and a month that already
// reached its budget pauses on the first step.
func TestDeviceLocalProvideShareRestartKeepsTheMonth(t *testing.T) {
	clock := &provideShareTestClock{nowTime: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	store := &provideShareStore{dir: t.TempDir()}
	share, _ := newProvideShareForTest(t, clock, store)
	share.setLimits(ProvideShareLimits{MaxBytesPerSecond: 125000, MonthlyByteCount: 5000})
	share.add(6000)
	// reaching the budget saves at once
	share.step()

	restarted, recorder := newProvideShareForTest(t, clock, store)
	if limits := restarted.limits(); limits != (ProvideShareLimits{MaxBytesPerSecond: 125000, MonthlyByteCount: 5000}) {
		t.Fatalf("limits after restart = %+v", limits)
	}
	if restarted.limiter.ByteRate() != 125000 {
		t.Fatalf("sharing speed after restart = %d", restarted.limiter.ByteRate())
	}
	if status := restarted.status(); status.ByteCount != 6000 {
		t.Fatalf("month's data after restart = %d, want 6000", status.ByteCount)
	}
	restarted.step()
	if len(recorder.pauses) != 1 || !recorder.pauses[0] {
		t.Fatalf("budget pause changes after restart = %v, want one pause", recorder.pauses)
	}
}

// Growing data is saved after the save interval or the save byte count,
// whichever comes first, and the listeners hear it at most once a second.
func TestDeviceLocalProvideShareSavesAndTellsAtTheirIntervals(t *testing.T) {
	clock := &provideShareTestClock{nowTime: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	store := &provideShareStore{dir: t.TempDir()}
	share, recorder := newProvideShareForTest(t, clock, store)
	share.step()
	toldBefore := len(recorder.statuses)

	share.add(1000)
	share.step()
	if store.loadUsage() != nil {
		t.Fatal("saved before the save interval")
	}
	share.add(1000)
	share.step()
	// the first growth is told at once (no status was told yet), the second
	// waits for the second to pass
	if told := len(recorder.statuses) - toldBefore; told != 1 {
		t.Fatalf("told %d statuses within a second, want 1", told)
	}
	clock.nowTime = clock.nowTime.Add(time.Second)
	share.step()
	if told := len(recorder.statuses) - toldBefore; told != 2 || recorder.statuses[len(recorder.statuses)-1].ByteCount != 2000 {
		t.Fatalf("told %d statuses after a second, want 2 ending at 2000 bytes", told)
	}

	clock.nowTime = clock.nowTime.Add(time.Minute)
	share.step()
	if usage := store.loadUsage(); usage == nil || usage.ByteCount != 2000 {
		t.Fatalf("usage after the save interval = %+v, want 2000 bytes", usage)
	}

	share.add(64 * 1024 * 1024)
	share.step()
	if usage := store.loadUsage(); usage == nil || usage.ByteCount != 2000+64*1024*1024 {
		t.Fatalf("usage after the save byte count = %+v", usage)
	}
}
