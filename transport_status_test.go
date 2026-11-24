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

func TestTransportStatusUsesOwnerBudgetDespiteProcessDefault(t *testing.T) {
	connect.SetMemoryBudget(8 * 1024 * 1024)
	t.Cleanup(func() { connect.SetMemoryBudget(0) })

	settings := DefaultTransportSettings()
	largeOwner := connect.NewPlatformTransportBudgetForMemoryTarget(20 * 1024 * 1024)
	lowMemoryOwner := connect.NewPlatformTransportBudgetForMemoryTarget(8 * 1024 * 1024)
	assertTransportStatus(t, transportStatusForBudget(settings, false, largeOwner), false, []string{TransportModeH1, TransportModeH3, TransportModeDns, TransportModeDnsPump})
	assertTransportStatus(t, transportStatusForBudget(settings, false, lowMemoryOwner), true, []string{TransportModeH1})
}

func TestTransportStatusDoesNotBuildFullTransportDefaults(t *testing.T) {
	connect.SetMemoryBudget(32 * 1024 * 1024)
	t.Cleanup(func() { connect.SetMemoryBudget(0) })
	settings := DefaultTransportSettings()

	status, allocations := transportStatusPollAllocations(t, settings, 20, func() {})
	if status == nil {
		t.Fatal("transport status is nil")
	}
	// every poll builds the status it returns, so a zero count means the
	// profile no longer sees the polls and the bound below would hold vacuously
	if allocations <= 0 {
		t.Fatalf("transportStatus polls counted %.0f objects each, want the status they build", allocations)
	}
	// A full DefaultPlatformTransportSettings construction reparses the pinned
	// certificate bundle and costs hundreds of objects per poll. Leave ample
	// room for the intentional settings/list/map result objects while pinning
	// the absence of that TLS construction path.
	if allocations >= 100 {
		t.Fatalf("transportStatus allocated %.0f objects per poll, want fewer than 100", allocations)
	}
}

// Another goroutine's allocations in the same window are not the poll's: each
// poll hands a turn to a goroutine that makes two provider status polls, as an
// earlier test's devices still winding down in a whole-package run allocate
// beside this one. The process-wide count, testing.AllocsPerRun's, takes them in
// and goes over the bound the poll alone meets; the profile's count stays the
// poll's own.
func TestTransportStatusPollAllocationsCountOnlyThePoll(t *testing.T) {
	connect.SetMemoryBudget(32 * 1024 * 1024)
	t.Cleanup(func() { connect.SetMemoryBudget(0) })
	settings := DefaultTransportSettings()

	_, alone := transportStatusPollAllocations(t, settings, 20, func() {})

	turns := make(chan struct{})
	turnDone := make(chan struct{})
	otherDone := make(chan struct{})
	go func() {
		defer close(otherDone)
		for range turns {
			transportStatus(settings, true)
			transportStatus(settings, true)
			turnDone <- struct{}{}
		}
	}()
	defer func() {
		close(turns)
		<-otherDone
	}()
	handTurn := func() {
		turns <- struct{}{}
		<-turnDone
	}

	processWide := testing.AllocsPerRun(20, func() {
		transportStatus(settings, false)
		handTurn()
	})
	if processWide < 100 {
		t.Fatalf("the process-wide count beside another goroutine's polls was %.0f objects per poll, want at least the bound of 100 that the poll alone (%.0f) meets", processWide, alone)
	}

	_, besideOthers := transportStatusPollAllocations(t, settings, 20, handTurn)
	t.Logf("objects per poll: alone %.0f, beside another goroutine's polls %.0f, process-wide %.0f", alone, besideOthers, processWide)
	if besideOthers != alone {
		t.Fatalf("transportStatus polls counted %.0f objects each beside another goroutine's polls and %.0f alone", besideOthers, alone)
	}
}

// Counts the objects each of polls transportStatus calls allocates, from the
// allocation profile at full sampling and only under pollTransportStatus's
// frame, so no other goroutine's allocations count. testing.AllocsPerRun reads
// the process's malloc count instead, and in a whole-package run that took in
// what an earlier test's devices allocated while they wound down beside this
// one: 109 objects per poll at sidecar 44097a35, where a poll alone makes 53
// (in a traced run, a closed device's pinned platform transport reading the
// host's interface addresses). between runs after each poll, outside the
// counted frame.
func transportStatusPollAllocations(
	t *testing.T,
	settings *TransportSettings,
	polls int,
	between func(),
) (*TransportStatus, float64) {
	t.Helper()
	// one poll ahead of the count, as testing.AllocsPerRun warms up, so that
	// nothing built once per process counts
	status := pollTransportStatus(settings)
	allocations := profiledAllocationsUnder(t, "sdk.pollTransportStatus", false, func() {
		for range polls {
			status = pollTransportStatus(settings)
			between()
		}
	})
	return status, float64(allocations) / float64(polls)
}

// One client status poll, as a settings screen makes it, in a frame of its own
// for the allocation profile to attribute.
func pollTransportStatus(settings *TransportSettings) *TransportStatus {
	return transportStatus(settings, false)
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
	settings := testDeviceLocalSettingsRpc()
	settings.MemoryTargetByteCount = 8 * 1024 * 1024
	deviceLocal, rpcSettings := testing_newRpcDeviceLocalWithSettings(t, ctx, settings)
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
	// Listeners registered before the asynchronous initial sync receive the
	// initial snapshot. Establish that delivery as the baseline so the update
	// assertion cannot consume a valid but stale H1 status event.
	clientEventCount := len(listener.waitForCount(t, false, 1))
	providerEventCount := len(listener.waitForCount(t, true, 1))

	h3Only := testingTransportSettings(
		TransportModeAuto,
		&TransportModePriority{Mode: TransportModeH3, Priority: 1},
	)
	deviceLocal.SetTransportSettings(h3Only)
	deviceLocal.SetProviderTransportSettings(h3Only)
	clientValues := listener.waitForCount(t, false, clientEventCount+1)
	status := clientValues[len(clientValues)-1]
	assertTransportStatus(t, status, false, []string{TransportModeH3})
	providerValues := listener.waitForCount(t, true, providerEventCount+1)
	assertTransportStatus(t, providerValues[len(providerValues)-1], false, []string{TransportModeH3})
	// Listener values are detached from the remote cache.
	status.AutoEligibleModes.Add(TransportModeH1)
	assertTransportStatus(t, deviceRemote.GetTransportStatus(), false, []string{TransportModeH3})

	wired := gobRoundTrip(t, &DeviceRemoteTransportSettingsRpc{
		TransportSettings: newTransportSettingsRpc(h3Only, false),
		TransportStatus:   newTransportStatusRpc(deviceLocal.GetTransportStatus()),
	})
	assertTransportStatus(t, wired.TransportStatus.toTransportStatus(), false, []string{TransportModeH3})
}
