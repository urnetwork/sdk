package sdk

import (
	"testing"
	"time"
)

// A browser websocket callback shares the single JavaScript event loop with
// the page. A full handoff must refuse immediately so closing the generation
// can recover instead of freezing every page callback behind it.
func TestDeviceRpcReceiveHandoffRefusesFullQueueWithoutBlocking(t *testing.T) {
	receive := make(chan []byte, 1)
	receive <- []byte("occupied")
	done := make(chan struct{})

	result := make(chan bool, 1)
	go func() {
		result <- offerDeviceRpcReceive(done, receive, []byte("must refuse"))
	}()

	select {
	case accepted := <-result:
		if accepted {
			t.Fatal("full receive handoff accepted another reliable message")
		}
	case <-time.After(time.Second):
		t.Fatal("full receive handoff blocked the shared callback")
	}
	if got := len(receive); got != 1 {
		t.Fatalf("receive queue contains %d messages, want 1", got)
	}
}

func TestDeviceRpcReceiveHandoffRefusesAfterClose(t *testing.T) {
	receive := make(chan []byte, 1)
	done := make(chan struct{})
	close(done)

	if offerDeviceRpcReceive(done, receive, []byte("must refuse")) {
		t.Fatal("closed receive handoff accepted a message")
	}
	if got := len(receive); got != 0 {
		t.Fatalf("receive queue contains %d messages, want 0", got)
	}
}
