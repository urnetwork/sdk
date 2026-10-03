//go:build js

package main

import (
	"testing"

	"github.com/urnetwork/sdk"
)

func TestTransportDistributionWasmPreservesH1Selection(t *testing.T) {
	shares := sdk.NewTransportShareList()
	share := &sdk.TransportShare{
		TransportType:              sdk.TransportTypeH1,
		H1WebSocketConnectionCount: 2,
		H1PlusConnectionCount:      3,
		Enabled:                    true,
	}
	shares.Add(share)
	distribution := &sdk.TransportDistribution{Shares: shares}
	got := jsTransportDistribution(distribution).Get("shares").Index(0)
	if got.Get("transportType").String() != sdk.TransportTypeH1 ||
		got.Get("h1WebSocketConnectionCount").Int() != 2 || got.Get("h1PlusConnectionCount").Int() != 3 {
		t.Fatal("WASM transport distribution lost live H1 selection")
	}
	share.H1PlusConnectionCount = 0
	fallback := jsTransportDistribution(distribution).Get("shares").Index(0)
	if fallback.Get("h1PlusConnectionCount").Int() != 0 || fallback.Get("h1WebSocketConnectionCount").Int() != 2 {
		t.Fatal("WASM distribution retained a stale H1+ count after fallback")
	}
}
