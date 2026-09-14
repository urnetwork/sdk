//go:build !ios_extension

package sdk

import (
	"encoding/json"
	"testing"
)

// Main historically encoded an empty server slice as `clients: null`. The
// view conversion must produce an empty list rather than panic when it decodes
// that valid empty-account response.
func TestDevicesViewControllerAcceptsNullClientsAsEmpty(t *testing.T) {
	result := &NetworkClientsResult{}
	if err := json.Unmarshal([]byte(`{"clients":null}`), result); err != nil {
		t.Fatal(err)
	}

	controller := &DevicesViewController{}
	networkClients := controller.networkClientsFromResult(result)
	if networkClients == nil {
		t.Fatal("devices view produced a nil list")
	}
	if networkClients.Len() != 0 {
		t.Fatalf("devices view produced %d clients, expected 0", networkClients.Len())
	}
}
