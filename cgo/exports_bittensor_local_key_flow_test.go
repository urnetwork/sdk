// The payout key's flow through the real c abi: what a desktop app reads of the card and the screen is
// json with the sdk's names, and the words have no way out while hidden. The store's callbacks need a
// host to implement them, so a port's own tests carry those; with none, the flow reads as a store that
// cannot answer, and offers nothing that could sign.
package main

import (
	"encoding/json"
	"reflect"
	"testing"
	"unsafe"

	"github.com/urnetwork/sdk"
)

func TestBittensorLocalKeyFlowThroughTheAbi(t *testing.T) {
	takeString := func(result reflect.Value) string {
		t.Helper()
		text := goStringAt(result.UnsafePointer())
		socketABICall(urnet_free_string, result)
		return text
	}

	network := cString("01890000-0000-7000-8000-000000000001")
	defer cStringFree(network)
	nothing := unsafe.Pointer(nil)
	flow := urnet_new_bittensor_local_key_flow(0, network, nil, nil, nil, nil, nil, nil, nothing)
	if flow == 0 {
		t.Fatal("no flow")
	}
	defer urnet_release(flow)
	defer urnet_bittensor_local_key_flow_close(flow)

	urnet_bittensor_local_key_flow_set_consent(flow, false, nil, 0)

	card := struct {
		State           string `json:"state"`
		OffersSetUp     bool   `json:"offers_set_up"`
		OffersSaveFirst bool   `json:"offers_save_first"`
		OwnerOnly       bool   `json:"owner_only"`
	}{}
	if err := json.Unmarshal([]byte(takeString(reflect.ValueOf(urnet_bittensor_local_key_flow_get_card(flow)))), &card); err != nil {
		t.Fatal(err)
	}
	if card.State != sdk.BittensorLocalKeyStateStoreUnavailable || card.OffersSetUp || card.OffersSaveFirst || !card.OwnerOnly {
		t.Fatalf("the card is %+v", card)
	}

	screen := struct {
		Open       bool   `json:"open"`
		Revealed   bool   `json:"revealed"`
		Confirming string `json:"confirming"`
	}{}
	if err := json.Unmarshal([]byte(takeString(reflect.ValueOf(urnet_bittensor_local_key_flow_get_screen(flow)))), &screen); err != nil {
		t.Fatal(err)
	}
	if screen.Open || screen.Revealed || screen.Confirming != sdk.BittensorLocalKeyConfirmNone {
		t.Fatalf("the screen is %+v", screen)
	}

	urnet_bittensor_local_key_flow_reveal(flow, true)
	if phrase := takeString(reflect.ValueOf(urnet_bittensor_local_key_flow_phrase_to_copy(flow))); phrase != "" {
		t.Fatalf("a hidden screen gave out %q", phrase)
	}
	if urnet_bittensor_local_key_flow_needs_backup(flow) {
		t.Fatal("no store reads as an unsaved key")
	}
}
