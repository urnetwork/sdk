//go:build !ios_extension

package sdk

import (
	"context"
	"testing"
	"time"
)

type networkClientsTestListener struct {
	results chan *NetworkClientInfoList
}

func (self *networkClientsTestListener) NetworkClientsChanged(networkClients *NetworkClientInfoList) {
	self.results <- networkClients
}

// Main historically encoded an empty server slice as `clients: null`. The
// asynchronous view callback must publish an empty list rather than panic when
// it decodes that valid empty-account response.
func TestDevicesViewControllerAcceptsNullClientsAsEmpty(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	strategy := newTestClientStrategy(ctx)
	api := NewApi(ctx, strategy, "https://api.example.test")
	controller := NewDevicesViewControllerWithApi(ctx, api)
	listener := &networkClientsTestListener{results: make(chan *NetworkClientInfoList, 1)}
	sub := controller.AddNetworkClientsListener(listener)
	t.Cleanup(func() {
		sub.Close()
		controller.Close()
		api.Close()
		strategy.Close()
		cancel()
	})

	api.setHttpGetRaw(func(context.Context, string, string) ([]byte, error) {
		return []byte(`{"clients":null}`), nil
	})
	controller.Start()

	select {
	case networkClients := <-listener.results:
		if networkClients == nil {
			t.Fatal("devices view published a nil list")
		}
		if networkClients.Len() != 0 {
			t.Fatalf("devices view published %d clients, expected 0", networkClients.Len())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("devices view did not publish the empty client list")
	}
}
