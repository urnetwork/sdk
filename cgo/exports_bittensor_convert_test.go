// Convert through the real c abi: a desktop app draws an estimate from json with the sdk's names,
// hands that json back to submit, keeps the submission's json to wait on, and the amounts format the
// same. The callbacks cross as c function pointers, which cgo tests cannot make, so `make smoke` is
// what would run them through a host.
package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/urnetwork/sdk"
)

func TestBittensorConvertFormatsThroughTheAbi(t *testing.T) {
	takeString := func(result reflect.Value) string {
		t.Helper()
		text := goStringAt(result.UnsafePointer())
		socketABICall(urnet_free_string, result)
		return text
	}
	if got := takeString(socketABICall(urnet_format_tao, int64(500_000))[0]); got != "0.000500 TAO" {
		t.Fatalf("format tao: %q", got)
	}
	if got := takeString(socketABICall(urnet_format_tao_amount, int64(1_500_000_000))[0]); got != "1.500000" {
		t.Fatalf("format tao amount: %q", got)
	}
}

// A sheet hands back what it was given. The amount, the limit and the promise have to survive the
// trip by name, or a submit would refuse or, worse, read other values.
func TestBittensorConvertEstimateJsonRoundTrip(t *testing.T) {
	estimate := &sdk.BittensorConvertEstimateResult{
		ColdkeySs58:            "5DfhGyQdFobKM8NsWvEeAKk5EQQgYe9AydgJ7rMB6E1EqRzV",
		HotkeySs58:             "5FsGk9mJLFyvRNxYkZ5BZZzW3CYLCD7qwmWPFfRrFjbnZfbn",
		Netuid:                 25,
		StakeRao:               5_000_000_000,
		FreeTaoRao:             1,
		PriceRao:               4_153_679,
		MinAmountRao:           491_725_504,
		MaxAmountRao:           4_817_446_751,
		AmountRao:              1_000_000_000,
		ExpectedTaoRao:         4_151_587,
		ExpectedNetTaoRao:      4_151_586,
		MinTaoRao:              4_068_555,
		LimitPriceRao:          4_070_605,
		PoolFeeAlphaRao:        503_547,
		PriceImpactBps:         1,
		FeeTaoRao:              645_118,
		FeeAlphaRao:            155_000_000,
		FeeInAlpha:             true,
		RemainderSweptAlphaRao: 7,
		RemainderSweptTaoRao:   8,
		BurnRao:                500_000,
		RegisterFeeRao:         1_065_867,
		BurnTaoNeededRao:       1_722_954,
		BurnAlreadyCovered:     true,
		CanConvert:             true,
		Error:                  &sdk.SnError{Code: sdk.BittensorConvertErrorPriceMoved, Message: "moved"},
	}
	encoded, err := json.Marshal(estimate)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"coldkey_ss58", "amount_rao", "limit_price_rao", "min_tao_rao", "can_convert", "max_amount_rao", "min_amount_rao", "fee_in_alpha", "remainder_swept_alpha_rao"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("no %s in %s", name, encoded)
		}
	}
	back := &sdk.BittensorConvertEstimateResult{}
	if err := json.Unmarshal(encoded, back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(estimate, back) {
		t.Fatalf("round trip\n got %+v\nwant %+v", back, estimate)
	}
}

func TestBittensorConvertSubmissionJsonRoundTrip(t *testing.T) {
	submission := &sdk.BittensorConvertSubmission{
		TxHash:      "0xcf1e4cafeab3397b",
		ColdkeySs58: "5DfhGyQdFobKM8NsWvEeAKk5EQQgYe9AydgJ7rMB6E1EqRzV",
		FirstBlock:  9_422_017,
		LastBlock:   9_422_080,
	}
	encoded, err := json.Marshal(submission)
	if err != nil {
		t.Fatal(err)
	}
	back := &sdk.BittensorConvertSubmission{}
	if err := json.Unmarshal(encoded, back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(submission, back) {
		t.Fatalf("round trip\n got %+v\nwant %+v", back, submission)
	}
}
