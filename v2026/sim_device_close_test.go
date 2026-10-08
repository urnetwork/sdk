package sdk

import (
	"context"
	"sync"
	"testing"
)

func TestCloseSimClientBridgeCancelsBlockedSendBeforeJoin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	bridgeEntered := make(chan struct{})
	bridgeExited := make(chan struct{})
	var bridgeWg sync.WaitGroup
	bridgeWg.Add(1)
	go func() {
		defer bridgeWg.Done()
		close(bridgeEntered)
		<-ctx.Done()
		close(bridgeExited)
	}()
	<-bridgeEntered

	tunClosed := false
	closeSimClientBridge(
		func() {
			tunClosed = true
		},
		cancel,
		func() {
			if !tunClosed {
				t.Fatal("bridge joined before the tun was closed")
			}
			if ctx.Err() == nil {
				t.Fatal("bridge joined before the blocked send was canceled")
			}
			bridgeWg.Wait()
		},
	)

	select {
	case <-bridgeExited:
	default:
		t.Fatal("bridge join returned before the blocked send exited")
	}
}
