package sdk

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func assertTransportStatus(
	t *testing.T,
	status *TransportStatus,
	wantDegraded bool,
	wantModes []string,
) {
	t.Helper()
	if status == nil {
		t.Fatal("transport status is nil")
	}
	if status.AutoDegraded != wantDegraded {
		t.Fatalf("AutoDegraded = %t, want %t", status.AutoDegraded, wantDegraded)
	}
	var gotModes []string
	if status.AutoEligibleModes != nil {
		gotModes = status.AutoEligibleModes.getAll()
	}
	if !slices.Equal(gotModes, wantModes) {
		t.Fatalf("AutoEligibleModes = %v, want %v", gotModes, wantModes)
	}
	wantConstraint := ""
	if wantDegraded {
		wantConstraint = TransportConstraintMemory
	}
	if status.AutoConstraint != wantConstraint {
		t.Fatalf("AutoConstraint = %q, want %q", status.AutoConstraint, wantConstraint)
	}
}

func TestTransportStatusFollowsAutoPolicyAndMemoryBudget(t *testing.T) {
	connect.SetMemoryBudget(8 * 1024 * 1024)
	t.Cleanup(func() { connect.SetMemoryBudget(0) })

	assertTransportStatus(
		t,
		transportStatus(DefaultTransportSettings(), false),
		true,
		[]string{TransportModeH1},
	)

	// Explicit H3 is not an Auto eligibility decision. The retained default
	// Auto policy is degraded, but selecting H3 itself remains valid and uses
	// H3's standalone reservation.
	explicitH3 := DefaultTransportSettings()
	explicitH3.Mode = TransportModeH3
	assertTransportStatus(
		t,
		transportStatus(explicitH3, false),
		true,
		[]string{TransportModeH1},
	)

	h3Only := testingTransportSettings(
		TransportModeAuto,
		&TransportModePriority{Mode: TransportModeH3, Priority: 1},
	)
	assertTransportStatus(
		t,
		transportStatus(h3Only, false),
		false,
		[]string{TransportModeH3},
	)
}

type testingTransportStatusChangeListener struct {
	mutex          sync.Mutex
	clientValues   []*TransportStatus
	providerValues []*TransportStatus
	notify         chan struct{}
}

func newTestingTransportStatusChangeListener() *testingTransportStatusChangeListener {
	return &testingTransportStatusChangeListener{notify: make(chan struct{}, 32)}
}

func (self *testingTransportStatusChangeListener) TransportStatusChanged(status *TransportStatus) {
	self.mutex.Lock()
	self.clientValues = append(self.clientValues, status)
	self.mutex.Unlock()
	select {
	case self.notify <- struct{}{}:
	default:
	}
}

func (self *testingTransportStatusChangeListener) ProviderTransportStatusChanged(status *TransportStatus) {
	self.mutex.Lock()
	self.providerValues = append(self.providerValues, status)
	self.mutex.Unlock()
	select {
	case self.notify <- struct{}{}:
	default:
	}
}

func (self *testingTransportStatusChangeListener) values(provider bool) []*TransportStatus {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if provider {
		return slices.Clone(self.providerValues)
	}
	return slices.Clone(self.clientValues)
}

func (self *testingTransportStatusChangeListener) waitForCount(
	t *testing.T,
	provider bool,
	want int,
) []*TransportStatus {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		values := self.values(provider)
		if want <= len(values) {
			return values
		}
		select {
		case <-self.notify:
		case <-deadline.C:
			t.Fatalf("transport status listener count=%d, want at least %d", len(values), want)
		}
	}
}

func TestDeviceRemoteTransportStatusGetterListenerAndRpcWire(t *testing.T) {
	connect.SetMemoryBudget(8 * 1024 * 1024)
	t.Cleanup(func() { connect.SetMemoryBudget(0) })

	ctx := t.Context()
	deviceLocal, rpcSettings := testing_newRpcDeviceLocal(t, ctx)
	deviceRemote := testing_newRpcDeviceRemote(
		t,
		deviceLocal,
		rpcSettings,
		deviceLocal.GetInstanceId(),
		DeviceRpcVersion,
	)
	listener := newTestingTransportStatusChangeListener()
	clientSub := deviceRemote.AddTransportStatusChangeListener(listener)
	defer clientSub.Close()
	providerSub := deviceRemote.AddProviderTransportStatusChangeListener(listener)
	defer providerSub.Close()

	deviceRemote.Sync()
	connect.AssertEqual(t, deviceRemote.waitForSync(10*time.Second), true)
	assertTransportStatus(t, deviceRemote.GetTransportStatus(), true, []string{TransportModeH1})
	assertTransportStatus(t, deviceRemote.GetProviderTransportStatus(), true, []string{TransportModeH1})

	h3Only := testingTransportSettings(
		TransportModeAuto,
		&TransportModePriority{Mode: TransportModeH3, Priority: 1},
	)
	deviceLocal.SetTransportSettings(h3Only)
	deviceLocal.SetProviderTransportSettings(h3Only)
	status := listener.waitForCount(t, false, 1)[0]
	assertTransportStatus(t, status, false, []string{TransportModeH3})
	assertTransportStatus(t, listener.waitForCount(t, true, 1)[0], false, []string{TransportModeH3})
	// Listener values are detached from the remote cache.
	status.AutoEligibleModes.Add(TransportModeH1)
	assertTransportStatus(t, deviceRemote.GetTransportStatus(), false, []string{TransportModeH3})

	wired := gobRoundTrip(t, &DeviceRemoteTransportSettingsRpc{
		TransportSettings: newTransportSettingsRpc(h3Only, false),
		TransportStatus:   newTransportStatusRpc(deviceLocal.GetTransportStatus()),
	})
	assertTransportStatus(t, wired.TransportStatus.toTransportStatus(), false, []string{TransportModeH3})
}
