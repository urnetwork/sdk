//go:build js

// The provider status a JavaScript provider shows reads these bindings: the
// provide getters and listeners, the provider's connected state and client
// limit status, and the provider traffic, whose contract rows carry the
// transfer path with its stream id.
package main

import (
	"syscall/js"
	"testing"

	"github.com/urnetwork/sdk"
)

func TestProviderStatusWasmBindings(t *testing.T) {
	_, device := newUnopenedExtensionDeviceRemote(t)
	listenerMethods := []string{
		"addProvidePausedChangeListener",
		"addProvideModeChangeListener",
		"addProvideChangeListener",
		"addClientLimitStatusChangeListener",
		"addProviderPacketStatsChangeListener",
		"addProviderEgressContractDetailsChangeListener",
		"addProviderIngressContractDetailsChangeListener",
	}
	for _, method := range append([]string{
		"getProvideMode",
		"setProvideMode",
		"getProviderConnected",
		"getClientLimitStatus",
		"getProviderPacketStats",
		"getProviderEgressContractDetails",
		"getProviderIngressContractDetails",
	}, listenerMethods...) {
		if device.Get(method).Type() != js.TypeFunction {
			t.Fatalf("the DeviceRemote binding has no %s", method)
		}
	}

	// before any sync: not connected, no hold, and no provider to describe
	if device.Call("getProviderConnected").Bool() {
		t.Fatal("an unsynced remote read a connected provider")
	}
	status := device.Call("getClientLimitStatus")
	if status.Get("status").String() != sdk.ClientLimitStatusNone || status.Get("retryTime").Int() != 0 {
		t.Fatalf("an unsynced remote read a client limit hold: %s %d", status.Get("status").String(), status.Get("retryTime").Int())
	}
	if !device.Call("getProviderPacketStats").IsNull() {
		t.Fatal("an unsynced remote read provider packet stats")
	}
	if !device.Call("getProviderIngressContractDetails").IsNull() || !device.Call("getProviderEgressContractDetails").IsNull() {
		t.Fatal("an unsynced remote read provider contract rows")
	}

	listener := js.FuncOf(func(this js.Value, args []js.Value) any { return nil })
	defer listener.Release()
	for _, method := range listenerMethods {
		if !device.Call(method, "not a function").IsNull() {
			t.Fatalf("%s accepted a non-function listener", method)
		}
		unsubscribe := device.Call(method, listener)
		if unsubscribe.Type() != js.TypeFunction {
			t.Fatalf("%s did not return an unsubscribe function", method)
		}
		unsubscribe.Invoke()
	}
}

// A contract row carries what a provider counts clients by: the path's ends
// (null where the path carries none) and the stream id, and so do the contract
// details controller's entries.
func TestProviderContractRowsWasmCarryThePath(t *testing.T) {
	contractId := sdk.NewId()
	provider := sdk.NewId()
	stream := sdk.NewId()
	contractDetails := &sdk.ContractDetails{
		ContractId:            contractId,
		ContractUsedByteCount: 1024,
		ContractByteCount:     64 * 1024,
		ContractBitRate:       8,
		ContractTransferPath:  sdk.NewTransferPath(nil, provider, stream),
		Status:                sdk.ContractStatusOpen,
	}

	row := jsContractDetails(contractDetails)
	if row.Get("contractId").String() != contractId.String() ||
		row.Get("contractUsedByteCount").Int() != 1024 ||
		row.Get("contractByteCount").Int() != 64*1024 ||
		row.Get("contractBitRate").Int() != 8 ||
		row.Get("status").String() != sdk.ContractStatusOpen {
		t.Fatal("the contract row lost a field")
	}
	path := row.Get("contractTransferPath")
	if !path.Get("sourceId").IsNull() {
		t.Fatalf("the path's missing source read %s", path.Get("sourceId").String())
	}
	if path.Get("destinationId").String() != provider.String() || path.Get("streamId").String() != stream.String() {
		t.Fatal("the contract row lost its path")
	}

	if !jsContractDetailsList(nil).IsNull() {
		t.Fatal("a device without a provider read contract rows")
	}
	contractDetailsList := sdk.NewContractDetailsList()
	contractDetailsList.Add(contractDetails)
	if rows := jsContractDetailsList(contractDetailsList); rows.Length() != 1 || rows.Index(0).Get("contractId").String() != contractId.String() {
		t.Fatal("the contract rows lost the row")
	}

	entry := jsContractEntry(&sdk.ContractEntry{
		ContractId: contractId.String(),
		HasStream:  true,
		StreamId:   stream.String(),
	})
	if entry.Get("streamId").String() != stream.String() {
		t.Fatal("the controller's contract entry lost its stream id")
	}

	status := jsClientLimitStatus(&sdk.ClientLimitStatus{
		Status:    sdk.ClientLimitStatusExceeded,
		RetryTime: 1791313500000,
	})
	if status.Get("status").String() != sdk.ClientLimitStatusExceeded || status.Get("retryTime").Int() != 1791313500000 {
		t.Fatal("the client limit status lost a field")
	}
}
