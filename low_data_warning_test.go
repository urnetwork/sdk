//go:build !ios_extension

package sdk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const testingLowDataGib = ByteCount(1024 * 1024 * 1024)

// One active balance of a reading: its id, network and start.
type testingLowDataBalance struct {
	balanceId      *Id
	startByteCount ByteCount
}

// A balance reading as the server sends it: the summary's start and available
// bytes, and the active balances it sums.
func testingLowDataBalanceResult(
	networkId *Id,
	startByteCount ByteCount,
	availableByteCount ByteCount,
	balances ...testingLowDataBalance,
) *SubscriptionBalanceResult {
	activeTransferBalances := NewTransferBalanceList()
	for _, balance := range balances {
		activeTransferBalances.Add(&TransferBalance{
			BalanceId:             balance.balanceId,
			NetworkId:             networkId,
			StartBalanceByteCount: balance.startByteCount,
			BalanceByteCount:      balance.startByteCount,
		})
	}
	return &SubscriptionBalanceResult{
		StartBalanceByteCount:  startByteCount,
		BalanceByteCount:       availableByteCount,
		ActiveTransferBalances: activeTransferBalances,
	}
}

func testingLowDataLocalState(t *testing.T) *LocalState {
	t.Helper()
	localState := newLocalState(context.Background(), t.TempDir())
	t.Cleanup(localState.Close)
	return localState
}

// The kind of the warning, "" for none.
func testingLowDataWarningKind(warning *LowDataWarning) string {
	if warning == nil {
		return ""
	}
	return warning.Kind
}

// A free day: 30 GiB. The percent warning shows once at 10% (3 GiB), the byte
// count warning once at 1 GiB, and data coming back from pending connections
// across a threshold does not warn again.
func TestLowDataWarningFreeDayWarnsOncePerThreshold(t *testing.T) {
	localState := testingLowDataLocalState(t)
	networkId := NewId()
	day := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}

	readings := []struct {
		availableByteCount ByteCount
		kind               string
	}{
		{availableByteCount: 30 * testingLowDataGib, kind: ""},
		{availableByteCount: 12 * testingLowDataGib, kind: ""},
		{availableByteCount: 3*testingLowDataGib + 1, kind: ""},
		{availableByteCount: 3 * testingLowDataGib, kind: LowDataWarningKindPercent},
		{availableByteCount: 2 * testingLowDataGib, kind: ""},
		// pending connections returned their unused reservation
		{availableByteCount: 4 * testingLowDataGib, kind: ""},
		{availableByteCount: 2 * testingLowDataGib, kind: ""},
		{availableByteCount: testingLowDataGib, kind: LowDataWarningKindByteCount},
		{availableByteCount: testingLowDataGib / 2, kind: ""},
		{availableByteCount: 2 * testingLowDataGib, kind: ""},
		{availableByteCount: testingLowDataGib / 4, kind: ""},
	}
	for i, reading := range readings {
		warning := LowDataWarningForBalance(localState, testingLowDataBalanceResult(
			networkId,
			day.startByteCount,
			reading.availableByteCount,
			day,
		))
		if kind := testingLowDataWarningKind(warning); kind != reading.kind {
			t.Fatalf("reading %d (%d bytes available): warning %q, expected %q", i, reading.availableByteCount, kind, reading.kind)
		}
		if warning != nil && warning.AvailableByteCount != reading.availableByteCount {
			t.Fatalf("reading %d: the warning names %d bytes, expected %d", i, warning.AvailableByteCount, reading.availableByteCount)
		}
	}
}

// The next day's grant is a new balance period: both thresholds warn again,
// each once, while yesterday's grant is still active in its grace and after it
// ends.
func TestLowDataWarningNextDailyGrantWarnsAgain(t *testing.T) {
	localState := testingLowDataLocalState(t)
	networkId := NewId()
	today := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}
	tomorrow := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}

	read := func(availableByteCount ByteCount, balances ...testingLowDataBalance) string {
		return testingLowDataWarningKind(LowDataWarningForBalance(localState, testingLowDataBalanceResult(
			networkId,
			30*testingLowDataGib,
			availableByteCount,
			balances...,
		)))
	}

	if kind := read(2*testingLowDataGib, today); kind != LowDataWarningKindPercent {
		t.Fatalf("today at 2 GiB: warning %q, expected the percent warning", kind)
	}
	if kind := read(testingLowDataGib/2, today); kind != LowDataWarningKindByteCount {
		t.Fatalf("today at 0.5 GiB: warning %q, expected the byte count warning", kind)
	}

	// the next grant, with today's still in its grace
	if kind := read(30*testingLowDataGib, today, tomorrow); kind != "" {
		t.Fatalf("tomorrow's grant: warning %q, expected none", kind)
	}
	if kind := read(2*testingLowDataGib, today, tomorrow); kind != LowDataWarningKindPercent {
		t.Fatalf("tomorrow at 2 GiB: warning %q, expected the percent warning again", kind)
	}
	// today's grant ended: no new data, no new warning
	if kind := read(2*testingLowDataGib, tomorrow); kind != "" {
		t.Fatalf("after today's grant ended: warning %q, expected none", kind)
	}
	if kind := read(testingLowDataGib, tomorrow); kind != LowDataWarningKindByteCount {
		t.Fatalf("tomorrow at 1 GiB: warning %q, expected the byte count warning again", kind)
	}
	if kind := read(testingLowDataGib/2, tomorrow); kind != "" {
		t.Fatalf("tomorrow at 0.5 GiB: warning %q, expected none", kind)
	}
}

// An app that was closed across the grant reads the new period already low:
// it is told once, for the more urgent threshold, though the threshold had
// warned the day before.
func TestLowDataWarningNewPeriodReadLowWarnsOnce(t *testing.T) {
	localState := testingLowDataLocalState(t)
	networkId := NewId()
	today := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}
	tomorrow := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}

	read := func(availableByteCount ByteCount, balances ...testingLowDataBalance) *LowDataWarning {
		return LowDataWarningForBalance(localState, testingLowDataBalanceResult(
			networkId,
			30*testingLowDataGib,
			availableByteCount,
			balances...,
		))
	}

	read(2*testingLowDataGib, today)
	read(testingLowDataGib/2, today)

	warning := read(testingLowDataGib/2, tomorrow)
	if kind := testingLowDataWarningKind(warning); kind != LowDataWarningKindByteCount {
		t.Fatalf("tomorrow read at 0.5 GiB: warning %q, expected the byte count warning", kind)
	}
	if kind := testingLowDataWarningKind(read(testingLowDataGib/4, tomorrow)); kind != "" {
		t.Fatalf("the next reading: warning %q, expected none (both thresholds were past)", kind)
	}
}

// A reading past both thresholds warns once, for the one with the lower
// amount: 0.5 GiB of 30 GiB is past 10% (3 GiB) and 1 GiB, and 1 GiB is the
// more urgent.
func TestLowDataWarningPastBothThresholdsWarnsOnceForTheLowerAmount(t *testing.T) {
	localState := testingLowDataLocalState(t)
	networkId := NewId()
	day := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}

	warning := LowDataWarningForBalance(localState, testingLowDataBalanceResult(networkId, 30*testingLowDataGib, testingLowDataGib/2, day))
	if kind := testingLowDataWarningKind(warning); kind != LowDataWarningKindByteCount {
		t.Fatalf("warning %q, expected the byte count warning", kind)
	}
	for _, availableByteCount := range []ByteCount{testingLowDataGib / 2, testingLowDataGib / 4, 2 * testingLowDataGib} {
		if kind := testingLowDataWarningKind(LowDataWarningForBalance(localState, testingLowDataBalanceResult(networkId, 30*testingLowDataGib, availableByteCount, day))); kind != "" {
			t.Fatalf("at %d bytes: warning %q, expected none", availableByteCount, kind)
		}
	}
}

// With a start under 10 GiB, 1 GiB is more than 10%: the byte count warning
// comes first, then the percent warning.
func TestLowDataWarningSmallStartWarnsByteCountThenPercent(t *testing.T) {
	localState := testingLowDataLocalState(t)
	networkId := NewId()
	code := testingLowDataBalance{balanceId: NewId(), startByteCount: 5 * testingLowDataGib}

	read := func(availableByteCount ByteCount) string {
		return testingLowDataWarningKind(LowDataWarningForBalance(localState, testingLowDataBalanceResult(networkId, 5*testingLowDataGib, availableByteCount, code)))
	}
	if kind := read(testingLowDataGib); kind != LowDataWarningKindByteCount {
		t.Fatalf("at 1 GiB: warning %q, expected the byte count warning", kind)
	}
	if kind := read(testingLowDataGib / 2); kind != LowDataWarningKindPercent {
		t.Fatalf("at 0.5 GiB: warning %q, expected the percent warning", kind)
	}
	if kind := read(testingLowDataGib / 4); kind != "" {
		t.Fatalf("at 0.25 GiB: warning %q, expected none", kind)
	}
}

// Pro: a month of 10 TiB plus a daily 30 GiB. Low in the month, the percent
// warning shows once, and the daily grant renewing is not new data enough to
// warn again; the next cycle is.
func TestLowDataWarningProDailyGrantDoesNotRewarnTheMonth(t *testing.T) {
	localState := testingLowDataLocalState(t)
	networkId := NewId()
	month := testingLowDataBalance{balanceId: NewId(), startByteCount: 10 * 1024 * testingLowDataGib}
	nextMonth := testingLowDataBalance{balanceId: NewId(), startByteCount: 10 * 1024 * testingLowDataGib}
	days := []testingLowDataBalance{}
	for range 3 {
		days = append(days, testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib})
	}
	startByteCount := month.startByteCount + 30*testingLowDataGib
	// 9% of the month and the day
	lowByteCount := startByteCount * 9 / 100

	read := func(availableByteCount ByteCount, balances ...testingLowDataBalance) string {
		return testingLowDataWarningKind(LowDataWarningForBalance(localState, testingLowDataBalanceResult(networkId, startByteCount, availableByteCount, balances...)))
	}

	if kind := read(lowByteCount, month, days[0]); kind != LowDataWarningKindPercent {
		t.Fatalf("9%% left: warning %q, expected the percent warning", kind)
	}
	if kind := read(lowByteCount, month, days[0], days[1]); kind != "" {
		t.Fatalf("the next day's grant, in the old one's grace: warning %q, expected none", kind)
	}
	if kind := read(lowByteCount, month, days[1]); kind != "" {
		t.Fatalf("the next day: warning %q, expected none", kind)
	}
	if kind := read(startByteCount, nextMonth, days[2]); kind != "" {
		t.Fatalf("the next cycle, full: warning %q, expected none", kind)
	}
	if kind := read(lowByteCount, nextMonth, days[2]); kind != LowDataWarningKindPercent {
		t.Fatalf("9%% left in the next cycle: warning %q, expected the percent warning", kind)
	}
}

// A Pro network past its month lives on the daily grant: each day's grant is
// new data enough for the byte count warning, not for the percent one.
func TestLowDataWarningProPastTheMonthWarnsDailyAtTheByteCount(t *testing.T) {
	localState := testingLowDataLocalState(t)
	networkId := NewId()
	month := testingLowDataBalance{balanceId: NewId(), startByteCount: 10 * 1024 * testingLowDataGib}
	today := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}
	tomorrow := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}
	startByteCount := month.startByteCount + 30*testingLowDataGib

	read := func(availableByteCount ByteCount, balances ...testingLowDataBalance) string {
		return testingLowDataWarningKind(LowDataWarningForBalance(localState, testingLowDataBalanceResult(networkId, startByteCount, availableByteCount, balances...)))
	}

	if kind := read(20*testingLowDataGib, month, today); kind != LowDataWarningKindPercent {
		t.Fatalf("the month used up: warning %q, expected the percent warning", kind)
	}
	if kind := read(testingLowDataGib, month, today); kind != LowDataWarningKindByteCount {
		t.Fatalf("today at 1 GiB: warning %q, expected the byte count warning", kind)
	}
	if kind := read(30*testingLowDataGib, month, tomorrow); kind != "" {
		t.Fatalf("tomorrow's grant: warning %q, expected none", kind)
	}
	if kind := read(testingLowDataGib, month, tomorrow); kind != LowDataWarningKindByteCount {
		t.Fatalf("tomorrow at 1 GiB: warning %q, expected the byte count warning again", kind)
	}
}

// A purchase is new data: a network that bought data after a warning is
// warned again when the purchase runs low.
func TestLowDataWarningPurchaseStartsANewPeriod(t *testing.T) {
	localState := testingLowDataLocalState(t)
	networkId := NewId()
	day := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}
	pack := testingLowDataBalance{balanceId: NewId(), startByteCount: 1024 * testingLowDataGib}

	read := func(startByteCount ByteCount, availableByteCount ByteCount, balances ...testingLowDataBalance) string {
		return testingLowDataWarningKind(LowDataWarningForBalance(localState, testingLowDataBalanceResult(networkId, startByteCount, availableByteCount, balances...)))
	}
	if kind := read(30*testingLowDataGib, testingLowDataGib/2, day); kind != LowDataWarningKindByteCount {
		t.Fatalf("before the purchase: warning %q, expected the byte count warning", kind)
	}
	withPack := day.startByteCount + pack.startByteCount
	if kind := read(withPack, withPack-30*testingLowDataGib, day, pack); kind != "" {
		t.Fatalf("after the purchase: warning %q, expected none", kind)
	}
	if kind := read(withPack, withPack/20, day, pack); kind != LowDataWarningKindPercent {
		t.Fatalf("the pack at 5%%: warning %q, expected the percent warning", kind)
	}
}

// The state is stored: a new local state over the same space (an app
// restart) does not warn again for a threshold that already warned.
func TestLowDataWarningIsStoredAcrossRestarts(t *testing.T) {
	home := t.TempDir()
	networkId := NewId()
	day := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}
	result := testingLowDataBalanceResult(networkId, 30*testingLowDataGib, 2*testingLowDataGib, day)

	first := newLocalState(context.Background(), home)
	if kind := testingLowDataWarningKind(LowDataWarningForBalance(first, result)); kind != LowDataWarningKindPercent {
		t.Fatalf("first launch: warning %q, expected the percent warning", kind)
	}
	first.Close()

	second := newLocalState(context.Background(), home)
	defer second.Close()
	if kind := testingLowDataWarningKind(LowDataWarningForBalance(second, result)); kind != "" {
		t.Fatalf("after a restart: warning %q, expected none", kind)
	}
}

// A sign-out removes the stored state with the rest of the account, and a
// signed-out space warns nothing, since it could not remember the warning.
func TestLowDataWarningSignOutClearsTheState(t *testing.T) {
	home := t.TempDir()
	networkId := NewId()
	day := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}
	result := testingLowDataBalanceResult(networkId, 30*testingLowDataGib, 2*testingLowDataGib, day)

	localState := newLocalState(context.Background(), home)
	if kind := testingLowDataWarningKind(LowDataWarningForBalance(localState, result)); kind != LowDataWarningKindPercent {
		t.Fatalf("warning %q, expected the percent warning", kind)
	}
	if err := localState.Logout(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(localState.localStorageDir, lowDataWarningFileName)); !os.IsNotExist(err) {
		t.Fatalf("the warning state outlived the sign-out: %v", err)
	}
	if warning := LowDataWarningForBalance(localState, result); warning != nil {
		t.Fatalf("a signed-out space warned %q", warning.Kind)
	}
	localState.Close()

	// the next sign-in starts over
	next := newLocalState(context.Background(), home)
	defer next.Close()
	if kind := testingLowDataWarningKind(LowDataWarningForBalance(next, result)); kind != LowDataWarningKindPercent {
		t.Fatalf("after signing in again: warning %q, expected the percent warning", kind)
	}
}

// The warnings of another network are not this one's.
func TestLowDataWarningAnotherNetworkStartsOver(t *testing.T) {
	localState := testingLowDataLocalState(t)
	day := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}
	otherDay := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}

	if kind := testingLowDataWarningKind(LowDataWarningForBalance(localState, testingLowDataBalanceResult(NewId(), 30*testingLowDataGib, testingLowDataGib/2, day))); kind != LowDataWarningKindByteCount {
		t.Fatalf("first network: warning %q, expected the byte count warning", kind)
	}
	if kind := testingLowDataWarningKind(LowDataWarningForBalance(localState, testingLowDataBalanceResult(NewId(), 30*testingLowDataGib, testingLowDataGib/2, otherDay))); kind != LowDataWarningKindByteCount {
		t.Fatalf("another network: warning %q, expected the byte count warning", kind)
	}
}

// Out of data is the out-of-balance notice's: no warning at zero, and none is
// used up there, so a reading with data back still warns.
func TestLowDataWarningNoneWhenOutOfData(t *testing.T) {
	localState := testingLowDataLocalState(t)
	networkId := NewId()
	day := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}

	for _, availableByteCount := range []ByteCount{0, -1} {
		if warning := LowDataWarningForBalance(localState, testingLowDataBalanceResult(networkId, 30*testingLowDataGib, availableByteCount, day)); warning != nil {
			t.Fatalf("at %d bytes: warning %q, expected none", availableByteCount, warning.Kind)
		}
	}
	if kind := testingLowDataWarningKind(LowDataWarningForBalance(localState, testingLowDataBalanceResult(networkId, 30*testingLowDataGib, testingLowDataGib/2, day))); kind != LowDataWarningKindByteCount {
		t.Fatalf("data back at 0.5 GiB: warning %q, expected the byte count warning", kind)
	}
	// no data at all
	if warning := LowDataWarningForBalance(localState, testingLowDataBalanceResult(networkId, 0, 0)); warning != nil {
		t.Fatalf("no balance: warning %q, expected none", warning.Kind)
	}
}

// The percent a warning names is rounded up, so it never reads 0% while data
// is left.
func TestLowDataWarningAvailablePercentRoundsUp(t *testing.T) {
	cases := []struct {
		startByteCount     ByteCount
		availableByteCount ByteCount
		availablePercent   int32
	}{
		{startByteCount: 30 * testingLowDataGib, availableByteCount: 3 * testingLowDataGib, availablePercent: 10},
		{startByteCount: 30 * testingLowDataGib, availableByteCount: 29 * testingLowDataGib / 10, availablePercent: 10},
		{startByteCount: 30 * testingLowDataGib, availableByteCount: testingLowDataGib / 2, availablePercent: 2},
		{startByteCount: 10 * 1024 * testingLowDataGib, availableByteCount: testingLowDataGib, availablePercent: 1},
	}
	for _, c := range cases {
		localState := testingLowDataLocalState(t)
		balance := testingLowDataBalance{balanceId: NewId(), startByteCount: c.startByteCount}
		warning := LowDataWarningForBalance(localState, testingLowDataBalanceResult(NewId(), c.startByteCount, c.availableByteCount, balance))
		if warning == nil {
			t.Fatalf("%d of %d bytes: no warning", c.availableByteCount, c.startByteCount)
		}
		if warning.AvailablePercent != c.availablePercent {
			t.Errorf("%d of %d bytes: %d%%, expected %d%%", c.availableByteCount, c.startByteCount, warning.AvailablePercent, c.availablePercent)
		}
		if warning.StartBalanceByteCount != c.startByteCount {
			t.Errorf("%d of %d bytes: start %d", c.availableByteCount, c.startByteCount, warning.StartBalanceByteCount)
		}
	}
}

// Without a local state there is nowhere to remember a warning, so none shows
// (it would show on every reading).
func TestLowDataWarningNeedsALocalState(t *testing.T) {
	day := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}
	if warning := LowDataWarningForBalance(nil, testingLowDataBalanceResult(NewId(), 30*testingLowDataGib, testingLowDataGib/2, day)); warning != nil {
		t.Fatalf("no local state: warning %q, expected none", warning.Kind)
	}
	if warning := LowDataWarningForBalance(testingLowDataLocalState(t), nil); warning != nil {
		t.Fatalf("no balance: warning %q, expected none", warning.Kind)
	}
}

// A stored state that does not decode reads as nothing warned, and is
// replaced by the next warning.
func TestLowDataWarningMalformedStateReadsAsArmed(t *testing.T) {
	localState := testingLowDataLocalState(t)
	if err := os.WriteFile(filepath.Join(localState.localStorageDir, lowDataWarningFileName), []byte("{not json"), LocalStorageFilePermissions); err != nil {
		t.Fatal(err)
	}
	networkId := NewId()
	day := testingLowDataBalance{balanceId: NewId(), startByteCount: 30 * testingLowDataGib}
	result := testingLowDataBalanceResult(networkId, 30*testingLowDataGib, 2*testingLowDataGib, day)
	if kind := testingLowDataWarningKind(LowDataWarningForBalance(localState, result)); kind != LowDataWarningKindPercent {
		t.Fatalf("warning %q, expected the percent warning", kind)
	}
	if kind := testingLowDataWarningKind(LowDataWarningForBalance(localState, result)); kind != "" {
		t.Fatalf("the next reading: warning %q, expected none", kind)
	}
}
