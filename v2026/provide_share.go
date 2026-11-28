package sdk

// provide_share.go -- the owner's limits on how much this device shares, and
// the month of sharing they are measured against.
//
// The provider's owner may cap two things (owner decision 2026-11-28, feature
// 10): the sharing speed, which the provider's return sender is paced to
// (connect.ProviderShareLimiter), and a monthly data budget. The data is what
// the device carries for others through its provider: the bytes it receives
// from the clients it serves and the bytes it sends back to them, in both
// directions and for every peer (the provider's remote packet counts). The
// month is the calendar month in the device's time zone. When the month's data
// reaches the budget, providing to other people pauses (the provider's
// contract manager pause, which keeps the same-network relationship and return
// traffic, as the battery pause does) until the next month starts, and then
// resumes by itself.
//
// A cap is a user-facing setting, off until the owner sets it. Sharing less
// lowers what the device carries for the network, which can lower its
// reliability ranking and its earnings; the apps say so beside the setting.
//
// The limits and the month's data are kept in two files of the space's local
// state, or of the directory DeviceLocalSettings.ProvideShareStorageDir names
// on a space that keeps none (the sn miner), or for the life of the device
// when neither exists. A file that cannot be read keeps no limit and no data.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// The owner's limits on how much this device shares. Zero is no limit. The
// byte counts are spelled int64, not ByteCount, so gobind binds them as
// primitives (build/cmd/mobileexports type_alias_test.go).
type ProvideShareLimits struct {
	// the fastest this device shares, in bytes per second
	MaxBytesPerSecond int64 `json:"max_bytes_per_second"`
	// the most data this device shares in a calendar month, in bytes
	MonthlyByteCount int64 `json:"monthly_byte_count"`
}

func NewProvideShareLimits(maxBytesPerSecond int64, monthlyByteCount int64) *ProvideShareLimits {
	return &ProvideShareLimits{
		MaxBytesPerSecond: maxBytesPerSecond,
		MonthlyByteCount:  monthlyByteCount,
	}
}

// A negative limit is no limit.
func normalProvideShareLimits(limits *ProvideShareLimits) ProvideShareLimits {
	if limits == nil {
		return ProvideShareLimits{}
	}
	return ProvideShareLimits{
		MaxBytesPerSecond: max(limits.MaxBytesPerSecond, 0),
		MonthlyByteCount:  max(limits.MonthlyByteCount, 0),
	}
}

// The month of sharing as it stands: the limits in force, the data this
// device shared in the current month and whether that reached the budget.
// The byte counts are int64 for gobind, as ProvideShareLimits's are.
type ProvideShareStatus struct {
	MaxBytesPerSecond int64 `json:"max_bytes_per_second"`
	MonthlyByteCount  int64 `json:"monthly_byte_count"`
	// the data this device shared in the current month, in bytes
	ByteCount int64 `json:"byte_count"`
	// the current month in the device's time zone, unix milliseconds: its
	// first instant, and the first instant of the next month, when the data
	// starts again from zero
	PeriodStartMillis int64 `json:"period_start_millis"`
	PeriodEndMillis   int64 `json:"period_end_millis"`
	// the month's data reached the budget: providing to other people is
	// paused until PeriodEndMillis
	BudgetReached bool `json:"budget_reached"`
}

// Fired when the status changes: the limits, the budget state, or the data,
// at most about once a second while the device shares.
type ProvideShareStatusChangeListener interface {
	ProvideShareStatusChanged(status *ProvideShareStatus)
}

// The calendar month that holds t, in location: its first instant and the
// first instant of the next month.
func provideSharePeriod(t time.Time, location *time.Location) (start time.Time, end time.Time) {
	local := t.In(location)
	start = time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, location)
	return start, start.AddDate(0, 1, 0)
}

// The month's data as kept between runs.
type provideShareUsage struct {
	// the first instant of the month the data is for, unix milliseconds
	PeriodStartMillis int64     `json:"period_start_millis"`
	ByteCount         ByteCount `json:"byte_count"`
}

// The data shared in a month against the owner's limits. Not safe for
// concurrent use; the device's share state guards it.
type provideShareMeter struct {
	limits      ProvideShareLimits
	periodStart time.Time
	periodEnd   time.Time
	byteCount   ByteCount
}

// A meter for the month that holds now, which continues usage when usage is
// for that same month, and starts from zero otherwise. The month is compared
// as a calendar month in location, so a device that moved time zones within a
// month keeps its data.
func newProvideShareMeter(limits ProvideShareLimits, usage *provideShareUsage, now time.Time, location *time.Location) *provideShareMeter {
	meter := &provideShareMeter{
		limits: limits,
	}
	meter.periodStart, meter.periodEnd = provideSharePeriod(now, location)
	if usage != nil && 0 < usage.ByteCount {
		usageStart, _ := provideSharePeriod(time.UnixMilli(usage.PeriodStartMillis), location)
		if usageStart.Equal(meter.periodStart) {
			meter.byteCount = usage.ByteCount
		}
	}
	return meter
}

// Starts the month that holds now when the current one has ended. True when a
// new month started.
func (self *provideShareMeter) roll(now time.Time, location *time.Location) bool {
	if now.Before(self.periodEnd) {
		return false
	}
	self.periodStart, self.periodEnd = provideSharePeriod(now, location)
	self.byteCount = 0
	return true
}

// Adds shared data to the month that holds now.
func (self *provideShareMeter) add(now time.Time, location *time.Location, byteCount ByteCount) {
	self.roll(now, location)
	if 0 < byteCount {
		self.byteCount += byteCount
	}
}

func (self *provideShareMeter) budgetReached() bool {
	return 0 < self.limits.MonthlyByteCount && self.limits.MonthlyByteCount <= self.byteCount
}

func (self *provideShareMeter) usage() provideShareUsage {
	return provideShareUsage{
		PeriodStartMillis: self.periodStart.UnixMilli(),
		ByteCount:         self.byteCount,
	}
}

func (self *provideShareMeter) status() *ProvideShareStatus {
	return &ProvideShareStatus{
		MaxBytesPerSecond: self.limits.MaxBytesPerSecond,
		MonthlyByteCount:  self.limits.MonthlyByteCount,
		ByteCount:         self.byteCount,
		PeriodStartMillis: self.periodStart.UnixMilli(),
		PeriodEndMillis:   self.periodEnd.UnixMilli(),
		BudgetReached:     self.budgetReached(),
	}
}

// The files the limits and the month's data are kept in.
const (
	provideShareLimitsFileName = ".provide_share_limits"
	provideShareUsageFileName  = ".provide_share_usage"
)

// A directory that keeps the limits and the month's data. One device writes
// it; every write replaces its file whole, so a reader never sees half of one.
type provideShareStore struct {
	dir string
}

// The stored limits, and whether there are any. A missing, unreadable or
// corrupt file stores none.
func (self *provideShareStore) loadLimits() (ProvideShareLimits, bool) {
	var limits ProvideShareLimits
	if !self.load(provideShareLimitsFileName, &limits) {
		return ProvideShareLimits{}, false
	}
	return normalProvideShareLimits(&limits), true
}

func (self *provideShareStore) saveLimits(limits ProvideShareLimits) error {
	return self.save(provideShareLimitsFileName, limits)
}

// The stored month's data, nil when there is none.
func (self *provideShareStore) loadUsage() *provideShareUsage {
	var usage provideShareUsage
	if !self.load(provideShareUsageFileName, &usage) || usage.ByteCount < 0 {
		return nil
	}
	return &usage
}

func (self *provideShareStore) saveUsage(usage provideShareUsage) error {
	return self.save(provideShareUsageFileName, usage)
}

func (self *provideShareStore) load(name string, value any) bool {
	data, err := os.ReadFile(filepath.Join(self.dir, name))
	if err != nil {
		return false
	}
	return json.Unmarshal(data, value) == nil
}

// Writes a temporary file beside the target and renames it over the target.
func (self *provideShareStore) save(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(self.dir, name+".*.tmp")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		os.Remove(tempPath)
		return err
	}
	if err := os.Chmod(tempPath, LocalStorageFilePermissions); err != nil {
		os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, filepath.Join(self.dir, name)); err != nil {
		os.Remove(tempPath)
		return err
	}
	return nil
}
