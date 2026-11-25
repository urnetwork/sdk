//go:build !js && !ios && !android && !sdk_mobile_bind && !sidecar_http && !ios_extension

package sdk

// The remote backend's compute wire against ur daemon's own (the root's compute_daemon.go, which builds on desktop legs
// alone, as this file does): the method names are the daemon's, every params Compute() sends decodes whole into the
// daemon's params, which it reads strictly, and the refusal reply is the daemon's shape.

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/urnetwork/sidecar"
)

// Each method name is the daemon's, and each params decodes into the daemon's with no field left over.
func TestComputeControlWireMatchesTheDaemon(t *testing.T) {
	for own, daemon := range map[string]string{
		sidecarControlMethodComputeBalance:      sidecar.DaemonMethodComputeBalance,
		sidecarControlMethodComputePacks:        sidecar.DaemonMethodComputePacks,
		sidecarControlMethodComputeCheckout:     sidecar.DaemonMethodComputeCheckout,
		sidecarControlMethodComputeRedeem:       sidecar.DaemonMethodComputeRedeem,
		sidecarControlMethodComputeAutoRenewSet: sidecar.DaemonMethodComputeAutoRenewSet,
		sidecarControlMethodComputeCharges:      sidecar.DaemonMethodComputeCharges,
		sidecarControlMethodComputeSpend:        sidecar.DaemonMethodComputeSpend,
		sidecarControlMethodComputeBudgetRaise:  sidecar.DaemonMethodComputeBudgetRaise,
	} {
		if own != daemon {
			t.Errorf("the sdk calls %s where the daemon serves %s", own, daemon)
		}
	}
	// the methods Redeem and AutoRenew call are the daemon's Confirm methods, the others not
	confirms := map[string]bool{}
	for _, method := range sidecar.DaemonComputeMethods(nil) {
		confirms[method.Name] = method.Confirm
	}
	if !confirms[sidecarControlMethodComputeRedeem] || !confirms[sidecarControlMethodComputeAutoRenewSet] || confirms[sidecarControlMethodComputeBudgetRaise] {
		t.Errorf("the daemon's Confirm methods are %v", confirms)
	}

	strict := func(label string, params any, into any) {
		data, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(into); err != nil {
			t.Errorf("%s's params %s do not decode as the daemon's: %v", label, data, err)
		}
	}
	strict("balance", &sidecarControlComputeParams{Network: "n"}, &sidecar.DaemonComputeParams{})
	packs := &sidecar.DaemonComputePacksParams{}
	strict("packs", &sidecarControlComputePacksParams{Network: "n", Market: "stripe"}, packs)
	redeem := &sidecar.DaemonComputeRedeemParams{}
	strict("redeem", &sidecarControlComputeRedeemParams{Network: "n", Code: "TESTCODE0001"}, redeem)
	charges := &sidecar.DaemonComputeChargesParams{}
	strict("charges", &sidecarControlComputeChargesParams{Network: "n", Cursor: "c"}, charges)
	spend := &sidecar.DaemonComputeSpendParams{}
	strict("spend", &sidecarControlComputeSpendParams{Network: "n", By: "agent"}, spend)
	if packs.Market != "stripe" || redeem.Code != "TESTCODE0001" || charges.Cursor != "c" || spend.By != "agent" || spend.Network != "n" {
		t.Errorf("the daemon reads %+v %+v %+v %+v", packs, redeem, charges, spend)
	}
	checkout := &sidecar.DaemonComputeCheckoutParams{}
	strict("checkout", &struct {
		Network string `json:"network,omitempty"`
		*sidecar.ComputeCheckoutArgs
	}{Network: "n", ComputeCheckoutArgs: &sidecar.ComputeCheckoutArgs{PackCredits: 25, AutoRenew: sidecar.ComputeAutoRenewThreshold}}, checkout)
	if checkout.PackCredits != 25 || checkout.AutoRenew != sidecar.ComputeAutoRenewThreshold {
		t.Errorf("the daemon reads the checkout %+v", checkout)
	}
	autoRenew := &sidecar.DaemonComputeAutoRenewSetParams{}
	strict("auto_renew.set", &struct {
		Network string `json:"network,omitempty"`
		*sidecar.ComputeAutoRenewChange
	}{Network: "n", ComputeAutoRenewChange: &sidecar.ComputeAutoRenewChange{Mode: sidecar.ComputeAutoRenewMonthly, On: true, Credits: 50}}, autoRenew)
	if autoRenew.Mode != sidecar.ComputeAutoRenewMonthly || autoRenew.Credits != 50 {
		t.Errorf("the daemon reads the change %+v", autoRenew)
	}
	raise := &sidecar.DaemonComputeBudgetRaiseParams{}
	strict("budget.raise", &struct {
		Network string `json:"network,omitempty"`
		*sidecar.ComputeBudgetRaiseArgs
	}{Network: "n", ComputeBudgetRaiseArgs: &sidecar.ComputeBudgetRaiseArgs{Period: "month", AmountNanoUsd: 2, CurrentNanoUsd: 1}}, raise)
	if raise.Period != "month" {
		t.Errorf("the daemon reads the raise %+v", raise)
	}

	// the refusal reply is the daemon's shape
	own, _ := json.Marshal(&sidecarComputeRefusalReply{Refusal: &sidecarComputeRefusal{Code: "code_ended", RetryAfterMs: 3}})
	daemon, _ := json.Marshal(&sidecar.DaemonComputeRefusalReply{Refusal: &sidecar.DaemonComputeRefusal{Code: "code_ended", RetryAfterMs: 3}})
	if !bytes.Equal(own, daemon) {
		t.Errorf("the sdk's refusal %s is not the daemon's %s", own, daemon)
	}
}
