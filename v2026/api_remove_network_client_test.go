package sdk

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// Keeps client cleanup on the NetworkSpace transport. Linux direct-file DNS
// transitions are corrected by that transport at wire-dial time; a raw
// net/http cleanup silently bypasses the correction and can reuse the old
// tunnel-only resolver after disconnect.
func TestApiRemoveNetworkClientUsesConfiguredTransportAndExplicitOwnerJwt(t *testing.T) {
	const (
		networkJwt = "network-bearer"
		clientJwt  = "client-bearer"
	)
	clientId := NewId()
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/network/remove-client" {
			http.Error(w, "unexpected route", http.StatusNotFound)
			return
		}
		if request.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.Method)
		}
		requireRequestBearer(t, request, networkJwt)
		var args RemoveNetworkClientArgs
		if err := json.NewDecoder(request.Body).Decode(&args); err != nil {
			t.Errorf("decode args: %v", err)
		} else if args.ClientId == nil || args.ClientId.Cmp(clientId) != 0 {
			t.Errorf("client id = %v, want %s", args.ClientId, clientId)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	})
	ctx, api := newTestApi(t, handler)
	api.SetByJwt(clientJwt)

	result, err := api.RemoveNetworkClientSyncWithContextAndJwt(
		ctx,
		&RemoveNetworkClientArgs{ClientId: clientId},
		networkJwt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Error != nil {
		t.Fatalf("remove result = %+v", result)
	}
	if current := api.GetByJwt(); current != clientJwt {
		t.Fatalf("current jwt = %q, want unchanged client jwt", current)
	}
}
