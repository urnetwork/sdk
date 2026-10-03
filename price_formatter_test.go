package sdk

import (
	"testing"

	"github.com/urnetwork/connect"
)

// TestComputePriceEquivalent pins the price presentation rules: ceiling to the
// minor unit, saving rounded down, suppression under one major unit, and the
// zero-digit currencies.
func TestComputePriceEquivalent(t *testing.T) {
	// $39.99 / 12 = 3.3325 -> $3.34; saving against $4.99 x 12 = 59.88 -> 33.2% -> 33
	e := ComputePriceEquivalent(39.99, 4.99, 2)
	connect.AssertEqual(t, int64(334), e.MonthlyEquivalentMinor)
	connect.AssertEqual(t, 3.34, e.MonthlyEquivalent)
	connect.AssertEqual(t, true, e.ShowEquivalent)
	connect.AssertEqual(t, 33, e.SavingPercent)
	connect.AssertEqual(t, int64(3999), e.YearlyMinor)
	connect.AssertEqual(t, int64(499), e.MonthlyMinor)

	// $40 / 12 = 3.333.. -> $3.34; saving against $5 x 12 = 60 -> 33.33% -> 33
	e = ComputePriceEquivalent(40, 5, 2)
	connect.AssertEqual(t, 3.34, e.MonthlyEquivalent)
	connect.AssertEqual(t, 33, e.SavingPercent)

	// the regional tier: $4 / 12 -> $0.34, under one major unit: suppressed;
	// saving against $0.50 x 12 = 6 -> 33
	e = ComputePriceEquivalent(4, 0.5, 2)
	connect.AssertEqual(t, int64(34), e.MonthlyEquivalentMinor)
	connect.AssertEqual(t, false, e.ShowEquivalent)
	connect.AssertEqual(t, 33, e.SavingPercent)

	// exactly one major unit shows
	e = ComputePriceEquivalent(12, 0, 2)
	connect.AssertEqual(t, 1.0, e.MonthlyEquivalent)
	connect.AssertEqual(t, true, e.ShowEquivalent)
	connect.AssertEqual(t, 0, e.SavingPercent)

	// JPY: no minor unit; 4990 / 12 = 415.83 -> 416
	e = ComputePriceEquivalent(4990, 600, 0)
	connect.AssertEqual(t, int64(416), e.MonthlyEquivalentMinor)
	connect.AssertEqual(t, 416.0, e.MonthlyEquivalent)
	connect.AssertEqual(t, true, e.ShowEquivalent)
	// 7200 - 4990 = 2210 / 7200 = 30.69% -> 30
	connect.AssertEqual(t, 30, e.SavingPercent)

	// an exact division does not round up
	e = ComputePriceEquivalent(36, 3, 2)
	connect.AssertEqual(t, 3.0, e.MonthlyEquivalent)
	connect.AssertEqual(t, 0, e.SavingPercent)

	// no saving when monthly is cheaper (never negative)
	e = ComputePriceEquivalent(60, 4, 2)
	connect.AssertEqual(t, 0, e.SavingPercent)

	// nothing to show for a non-positive yearly price
	e = ComputePriceEquivalent(0, 5, 2)
	connect.AssertEqual(t, 0.0, e.MonthlyEquivalent)
	connect.AssertEqual(t, false, e.ShowEquivalent)
	e = ComputePriceEquivalent(-1, 5, 2)
	connect.AssertEqual(t, false, e.ShowEquivalent)

	// the convenience forms
	connect.AssertEqual(t, 3.34, MonthlyEquivalentAmount(39.99, 2))
	connect.AssertEqual(t, 33, SavingPercent(39.99, 4.99, 2))
	// three-digit currencies (BHD) and a clamp on silly digit counts
	connect.AssertEqual(t, int64(1000), ComputePriceEquivalent(12, 0, 3).MonthlyEquivalentMinor)
	connect.AssertEqual(t, int64(1), ComputePriceEquivalent(12, 0, -2).MonthlyEquivalentMinor)
}
