//go:build js

// The settlement schedule on the wasm claims result.
package main

import (
	"testing"

	"github.com/urnetwork/sdk"
)

// The claims result carries the epoch schedule to the site, and null while
// the coordinator's policy is unreadable.
func TestSnClaimsResultWasmSchedule(t *testing.T) {
	result := jsSnClaimsResult(&sdk.SnClaimsResult{
		Claims:       sdk.NewSnEpochClaimList(),
		CurrentEpoch: 9,
		BlockNumber:  1000,
		Schedule: &sdk.SnEpochSchedule{
			Epoch:                 9,
			EpochBlocks:           50_400,
			ClaimOpenOffsetBlocks: 14_400,
			ClaimTtlEpochs:        8,
			ClaimGraceEpochs:      1,
			EndBlock:              51_350,
			ClaimOpenBlock:        65_750,
			ExpiryBlock:           504_949,
			HeadBlock:             1000,
			HeadMillis:            1_791_244_800_000,
			EndMillis:             1_791_849_000_000,
			ClaimOpenMillis:       1_792_021_800_000,
			ExpiryMillis:          1_797_292_188_000,
		},
	})
	schedule := result.Get("Schedule")
	if schedule.IsNull() || schedule.IsUndefined() {
		t.Fatal("no schedule")
	}
	for name, want := range map[string]int64{
		"Epoch":                 9,
		"ClaimOpenOffsetBlocks": 14_400,
		"ClaimTtlEpochs":        8,
		"ClaimGraceEpochs":      1,
		"EndMillis":             1_791_849_000_000,
		"ClaimOpenMillis":       1_792_021_800_000,
		"ExpiryMillis":          1_797_292_188_000,
	} {
		if got := int64(schedule.Get(name).Float()); got != want {
			t.Fatalf("%s = %d, want %d", name, got, want)
		}
	}

	none := jsSnClaimsResult(&sdk.SnClaimsResult{Claims: sdk.NewSnEpochClaimList()})
	if !none.Get("Schedule").IsNull() {
		t.Fatalf("schedule without a policy: %v", none.Get("Schedule"))
	}
}

// CL-2: the site sees each v2 account's states, and an unavailable ledger as
// an error with no account.
func TestSnLedgerAccountsResultWasm(t *testing.T) {
	accounts := sdk.NewSnLedgerAccountList()
	accounts.Add(&sdk.SnLedgerAccount{
		Vault:        "0xabababababababababababababababababababab",
		NoId:         1,
		Index:        42,
		FinalizedRao: 123_456_789,
		ClaimableRao: 23_456_789,
		PaidRao:      100_000_000,
		Pending:      true,
		PendingRao:   130_000_000,
	})
	result := jsSnLedgerAccountsResult(&sdk.SnLedgerAccountsResult{Accounts: accounts, ColdkeySs58: "5F"})
	if result.Get("Accounts").Length() != 1 || result.Get("ColdkeySs58").String() != "5F" || !result.Get("Error").IsNull() {
		t.Fatalf("result %v", result)
	}
	account := result.Get("Accounts").Index(0)
	for name, want := range map[string]int64{
		"NoId":         1,
		"Index":        42,
		"FinalizedRao": 123_456_789,
		"ClaimableRao": 23_456_789,
		"PaidRao":      100_000_000,
		"PendingRao":   130_000_000,
	} {
		if got := int64(account.Get(name).Float()); got != want {
			t.Fatalf("%s = %d, want %d", name, got, want)
		}
	}
	if !account.Get("Pending").Bool() || account.Get("Vault").String() != "0xabababababababababababababababababababab" {
		t.Fatalf("account %v", account)
	}

	unavailable := jsSnLedgerAccountsResult(&sdk.SnLedgerAccountsResult{
		Accounts: sdk.NewSnLedgerAccountList(),
		Error:    &sdk.SnError{Code: sdk.SnErrorCodeLedgerUnavailable, Message: "http 500 fetching the ledger account"},
	})
	if unavailable.Get("Accounts").Length() != 0 || unavailable.Get("Error").Get("Code").String() != sdk.SnErrorCodeLedgerUnavailable {
		t.Fatalf("unavailable %v", unavailable)
	}
}
