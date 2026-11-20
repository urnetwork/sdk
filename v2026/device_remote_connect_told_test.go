// Covers what a DeviceRemote tells its connect state listeners and how it
// delivers it (device_remote_connect_state.go). The rpc stays down, so every
// write is pending and told from the caller's goroutine.
package sdk

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Writes again from inside its connect callback, the first time it hears
// connect enabled.
type testingRewritingConnectListener struct {
	deviceRemote *DeviceRemote
	recorder     *testingConnectStateRecorder
	rewritten    bool
}

func (self *testingRewritingConnectListener) ConnectChanged(connectEnabled bool) {
	self.recorder.ConnectChanged(connectEnabled)
	if connectEnabled && !self.rewritten {
		self.rewritten = true
		self.deviceRemote.SetConnectLocation(nil)
	}
}

// A listener that writes from its callback neither deadlocks the delivery nor
// hears the two writes out of order.
func TestDeviceRemoteConnectStateListenerWritesFromCallback(t *testing.T) {
	deviceRemote, recorder := testingConnectStateRemote(t)
	rewriting := &testingRewritingConnectListener{
		deviceRemote: deviceRemote,
		recorder:     &testingConnectStateRecorder{},
	}
	sub := deviceRemote.AddConnectChangeListener(rewriting)
	defer sub.Close()

	written := make(chan struct{})
	go func() {
		defer close(written)
		deviceRemote.Reconnect(testingConnectStateLocation("tapped"))
	}()
	select {
	case <-written:
	case <-time.After(15 * time.Second):
		t.Fatal("a listener that writes from its callback deadlocked the delivery")
	}

	connect.AssertEqual(t, rewriting.recorder.get(), []string{"connect:true", "connect:false"})
	connect.AssertEqual(t, recorder.get(), []string{
		"location:tapped", "connect:true",
		"location:nil", "connect:false",
	})
	connect.AssertEqual(t, deviceRemote.GetConnectEnabled(), false)
}

// An unchanged SetConnectLocation tells nothing new, but a Reconnect to the
// same location is a new connection and tells its location again, so the
// view builds a grid for it.
func TestDeviceRemoteConnectStateReconnectTellsUnchangedLocation(t *testing.T) {
	deviceRemote, recorder := testingConnectStateRemote(t)
	location := testingConnectStateLocation("tapped")

	deviceRemote.SetConnectLocation(location)
	connect.AssertEqual(t, recorder.get(), []string{"location:tapped", "connect:true"})

	// the pending write already holds it
	deviceRemote.SetConnectLocation(location)
	connect.AssertEqual(t, recorder.get(), []string{"location:tapped", "connect:true"})

	// delivered, then written again: the listeners were already told
	testingTakeConnectSync(t, deviceRemote)
	deviceRemote.SetConnectLocation(location)
	connect.AssertEqual(t, recorder.get(), []string{"location:tapped", "connect:true"})

	deviceRemote.Reconnect(location)
	connect.AssertEqual(t, recorder.get(), []string{
		"location:tapped", "connect:true",
		"location:tapped",
	})
}

// Exits its goroutine (runtime.Goexit) the first time it hears connect
// enabled, as t.FailNow does in a test listener.
type testingGoexitConnectListener struct {
	once sync.Once
}

func (self *testingGoexitConnectListener) ConnectChanged(connectEnabled bool) {
	exit := false
	if connectEnabled {
		self.once.Do(func() { exit = true })
	}
	if exit {
		runtime.Goexit()
	}
}

// A listener that exits the delivering goroutine ends that delivery, not the
// later ones: panic containment does not catch runtime.Goexit.
func TestDeviceRemoteConnectStateDeliveryResumesAfterListenerGoexit(t *testing.T) {
	deviceRemote, recorder := testingConnectStateRemote(t)
	sub := deviceRemote.AddConnectChangeListener(&testingGoexitConnectListener{})
	defer sub.Close()

	written := make(chan struct{})
	go func() {
		defer close(written)
		deviceRemote.Reconnect(testingConnectStateLocation("tapped"))
	}()
	select {
	case <-written:
	case <-time.After(15 * time.Second):
		t.Fatal("the write never returned")
	}

	deviceRemote.SetConnectLocation(nil)
	testingAssertConnectStateLastHeard(t, deviceRemote, recorder, "nil", false)
}
