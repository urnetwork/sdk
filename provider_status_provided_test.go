// The provider status's SN25 data figures (api.go ProviderStatusProvided):
// the wire shape the server answers, each device's figure on its status, and
// the network's unpaid data on the controller.
package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/urnetwork/connect"
)

// The server's answer for two devices of a network in the newest settled
// epoch, with byte counts past 2^53 so a lossy decode would show: this device
// had no payout wallet, the other was paid; the network's unpaid data is this
// device's.
func providerStatusProvidedTestJson() string {
	return fmt.Sprintf(`{
		"providers": [
			{
				"client_id": %q,
				"reason": "none",
				"reason_text": "",
				"admission": {"connected": true, "location_valid": true, "provide_public": true, "reliability_ok": true, "speed_test_done": true, "egress": "pass"},
				"ranking": [],
				"evaluate_time": "2026-11-28T12:00:00Z",
				"provided": {"epoch": 9, "usage_byte_count": 9007199254740993, "payout": "unpaid"}
			},
			{
				"client_id": %q,
				"reason": "none",
				"reason_text": "",
				"admission": {"connected": true, "location_valid": true, "provide_public": true, "reliability_ok": true, "speed_test_done": true, "egress": "pass"},
				"ranking": [],
				"evaluate_time": "2026-11-28T12:00:00Z",
				"provided": {"epoch": 9, "usage_byte_count": 2048, "payout": "paid"}
			}
		],
		"total_clients": 2,
		"provided": {"epoch": 9, "usage_byte_count": 9007199254743041, "unpaid_byte_count": 9007199254740993}
	}`, providerStatusTestOwnClientId, providerStatusTestOtherClientId)
}

// The binding decodes each device's figure and the network's totals exactly.
func TestGetProviderStatusDecodesTheProvidedData(t *testing.T) {
	_, api := newTestApi(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, providerStatusProvidedTestJson())
	}))
	callback, c := connect.NewBlockingApiCallback[*GetProviderStatusResult](context.Background())
	api.GetProviderStatus(callback)
	r := awaitApiResult(t, c, "GetProviderStatus never returned")
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	result := r.Result
	if result.Providers == nil || result.Providers.Len() != 2 {
		t.Fatalf("result = %+v", result)
	}
	own := result.Providers.Get(0).Provided
	if own == nil || *own != (ProviderStatusProvided{Epoch: 9, UsageByteCount: 9007199254740993, Payout: ProviderStatusProvidedUnpaid}) {
		t.Fatalf("own provided = %+v", own)
	}
	other := result.Providers.Get(1).Provided
	if other == nil || *other != (ProviderStatusProvided{Epoch: 9, UsageByteCount: 2048, Payout: ProviderStatusProvidedPaid}) {
		t.Fatalf("other provided = %+v", other)
	}
	want := ProviderStatusNetworkProvided{Epoch: 9, UsageByteCount: 9007199254743041, UnpaidByteCount: 9007199254740993}
	if result.Provided == nil || *result.Provided != want {
		t.Fatalf("network provided = %+v, want %+v", result.Provided, want)
	}
}

// A server that predates the figures answers none, which the controller
// reports as no settled epoch.
func TestProviderStatusViewControllerUnpaidDataWithoutFigures(t *testing.T) {
	vc, fetches, _, _ := newProviderStatusTestController(t)
	vc.Start()
	vc.step()
	fetches.callback(0).Result(newProviderStatusTestResult(t, ProviderStatusReasonNone, 1), nil)
	if vc.GetNetworkProvided() != nil || vc.GetUnpaidByteCount() != 0 || vc.GetProvidedEpoch() != 0 {
		t.Fatal("figures from a server that answered none")
	}
	if status := vc.GetProviderStatus(); status == nil || status.Provided != nil {
		t.Fatalf("own status = %+v, want no figure", status)
	}
}

// The controller publishes the network's unpaid data and each device's
// figure, and a later answer without figures clears them.
func TestProviderStatusViewControllerPublishesTheUnpaidData(t *testing.T) {
	vc, fetches, _, advance := newProviderStatusTestController(t)
	vc.Start()
	vc.step()
	result := &GetProviderStatusResult{}
	if err := json.Unmarshal([]byte(providerStatusProvidedTestJson()), result); err != nil {
		t.Fatal(err)
	}
	fetches.callback(0).Result(result, nil)
	if vc.GetProvidedEpoch() != 9 || vc.GetUnpaidByteCount() != 9007199254740993 {
		t.Fatalf("epoch %d unpaid %d", vc.GetProvidedEpoch(), vc.GetUnpaidByteCount())
	}
	if provided := vc.GetNetworkProvided(); provided == nil || provided.UsageByteCount != 9007199254743041 {
		t.Fatalf("network provided = %+v", provided)
	}
	if status := vc.GetProviderStatus(); status == nil || status.Provided == nil || status.Provided.Payout != ProviderStatusProvidedUnpaid {
		t.Fatalf("own status = %+v", status)
	}

	advance(defaultProviderStatusPollInterval)
	vc.step()
	fetches.callback(1).Result(newProviderStatusTestResult(t, ProviderStatusReasonNone, 1), nil)
	if vc.GetNetworkProvided() != nil || vc.GetUnpaidByteCount() != 0 || vc.GetProvidedEpoch() != 0 {
		t.Fatal("an answer without figures kept the last ones")
	}
}
