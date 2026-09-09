package sdk

import "math"

// Price presentation (mmm/onboarding/PLAN.md "PRICE PRESENTATION"). The yearly
// price is the headline; its per-month equivalent is a subordinate sub-line
// computed HERE, the same way on every platform:
//
//   - equivalent = yearly / 12, rounded UP to the currency's minor unit (never
//     understate: $39.99 / 12 = 3.3325 -> $3.34; $40 / 12 -> $3.34)
//   - saving percent against paying monthly, rounded DOWN
//   - the equivalent is suppressed when it falls under one major unit (the $4
//     tier: "$0.33/month" reads as an error)
//
// Amounts are in major units of the store-localized currency (39.99, 4, 4990
// for JPY); minorUnitDigits is the currency's exponent (2 for USD/EUR, 0 for
// JPY/KRW). The apps format the returned amounts with their own currency
// formatter; nothing here is a string.

type PriceEquivalent struct {
	// MonthlyEquivalent is yearly / 12 rounded up to the minor unit; 0 when
	// the yearly amount is not positive
	MonthlyEquivalent float64 `json:"monthly_equivalent"`
	// MonthlyEquivalentMinor is the same amount in minor units (334 for $3.34)
	MonthlyEquivalentMinor int64 `json:"monthly_equivalent_minor"`
	// ShowEquivalent is false when the equivalent is under one major unit
	ShowEquivalent bool `json:"show_equivalent"`
	// SavingPercent is the saving of the yearly price against twelve monthly
	// payments, rounded down; 0 when there is no monthly price or no saving
	SavingPercent int `json:"saving_percent"`
	// YearlyMinor / MonthlyMinor are the inputs in minor units
	YearlyMinor  int64 `json:"yearly_minor"`
	MonthlyMinor int64 `json:"monthly_minor"`
}

// ComputePriceEquivalent computes the per-month equivalent and the saving for a
// yearly price (and the monthly price it is compared against; pass 0 when there
// is none) in a currency with minorUnitDigits.
func ComputePriceEquivalent(yearlyAmount float64, monthlyAmount float64, minorUnitDigits int) *PriceEquivalent {
	if minorUnitDigits < 0 {
		minorUnitDigits = 0
	}
	if 4 < minorUnitDigits {
		minorUnitDigits = 4
	}
	scale := math.Pow10(minorUnitDigits)
	yearlyMinor := toMinor(yearlyAmount, scale)
	monthlyMinor := toMinor(monthlyAmount, scale)
	result := &PriceEquivalent{
		YearlyMinor:  yearlyMinor,
		MonthlyMinor: monthlyMinor,
	}
	if yearlyMinor <= 0 {
		return result
	}
	// ceiling division: never understate the monthly figure
	equivalentMinor := (yearlyMinor + 11) / 12
	result.MonthlyEquivalentMinor = equivalentMinor
	result.MonthlyEquivalent = float64(equivalentMinor) / scale
	result.ShowEquivalent = float64(equivalentMinor) >= scale
	if 0 < monthlyMinor {
		twelveMonths := 12 * monthlyMinor
		if yearlyMinor < twelveMonths {
			// floor: never overstate the saving
			result.SavingPercent = int((100 * (twelveMonths - yearlyMinor)) / twelveMonths)
		}
	}
	return result
}

// toMinor converts a major-unit amount to minor units, rounding to the nearest
// minor unit (the input is a store price, already on the minor grid).
func toMinor(amount float64, scale float64) int64 {
	if amount <= 0 || math.IsNaN(amount) || math.IsInf(amount, 0) {
		return 0
	}
	return int64(math.Round(amount * scale))
}

// MonthlyEquivalentAmount is the per-month equivalent of a yearly amount alone
// (yearly / 12 rounded up to the minor unit).
func MonthlyEquivalentAmount(yearlyAmount float64, minorUnitDigits int) float64 {
	return ComputePriceEquivalent(yearlyAmount, 0, minorUnitDigits).MonthlyEquivalent
}

// SavingPercent is the saving of a yearly price against twelve monthly
// payments, rounded down.
func SavingPercent(yearlyAmount float64, monthlyAmount float64, minorUnitDigits int) int {
	return ComputePriceEquivalent(yearlyAmount, monthlyAmount, minorUnitDigits).SavingPercent
}
