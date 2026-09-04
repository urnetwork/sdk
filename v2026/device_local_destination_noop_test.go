package sdk

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/connect/v2026"
)

type destinationWindowStatusRecorder struct {
	stateLock             sync.Mutex
	connectionGenerations []int64
}

func (self *destinationWindowStatusRecorder) WindowStatusChanged(windowStatus *WindowStatus) {
	if windowStatus == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.connectionGenerations = append(self.connectionGenerations, windowStatus.ConnectionGeneration)
}

func (self *destinationWindowStatusRecorder) HasConnectionGeneration(connectionGeneration int64) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, observedConnectionGeneration := range self.connectionGenerations {
		if observedConnectionGeneration == connectionGeneration {
			return true
		}
	}
	return false
}

// TestDeviceLocalEquivalentDestinationDoesNotRebuildProviders covers app
// resume/recreation applying persisted state to an already-connected extension.
// An equivalent destination must preserve the live mux and provider window.
func TestDeviceLocalEquivalentDestinationDoesNotRebuildProviders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}

	var generatorCalls atomic.Int64
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.AllowProvider = false
	settings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		generatorCalls.Add(1)
		return &rpcLeakTestGenerator{}
	}
	device, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		NewId(),
		settings,
		connect.NewId(),
	)
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()

	upgradeMuxSettings := connect.DefaultUpgradeMuxSettings()
	upgradeMuxSettings.Dns = nil
	device.SetUpgradeMuxSettings(upgradeMuxSettings)

	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
		Name:              "first label",
	}
	device.SetConnectLocation(location)
	firstClient := device.remoteUserNatClient
	firstGeneration := device.GetWindowStatus().ConnectionGeneration
	if calls := generatorCalls.Load(); calls != 1 {
		t.Fatalf("first destination generator calls = %d, want 1", calls)
	}
	if firstGeneration <= 0 {
		t.Fatalf("first destination connection generation = %d, want positive", firstGeneration)
	}

	equivalent := cloneConnectLocation(location)
	equivalent.Name = "refreshed label"
	device.SetConnectLocation(equivalent)

	if calls := generatorCalls.Load(); calls != 1 {
		t.Fatalf("equivalent destination rebuilt generator: calls = %d", calls)
	}
	if device.remoteUserNatClient != firstClient {
		t.Fatalf("equivalent destination replaced live provider window")
	}
	if generation := device.GetWindowStatus().ConnectionGeneration; generation != firstGeneration {
		t.Fatalf("equivalent destination changed connection generation: got %d, want %d", generation, firstGeneration)
	}
	if got := device.GetConnectLocation(); got == nil || got.Name != "refreshed label" {
		t.Fatalf("equivalent destination did not refresh metadata: %+v", got)
	}
}

// TestDeviceLocalReconnectRebuildsTheSameDestination covers choosing the
// location you are already connected to. SetConnectLocation deliberately keeps
// the live window (see the equivalent-destination test above), so the explicit
// action goes through Reconnect, which must build a NEW multi client — and with
// it a new set of peers — for that same location.
func TestDeviceLocalReconnectRebuildsTheSameDestination(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}

	var generatorCalls atomic.Int64
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.AllowProvider = false
	settings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		generatorCalls.Add(1)
		return &rpcLeakTestGenerator{}
	}
	device, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		NewId(),
		settings,
		connect.NewId(),
	)
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()
	windowStatusRecorder := &destinationWindowStatusRecorder{}
	windowStatusSub := device.AddWindowStatusChangeListener(windowStatusRecorder)
	defer windowStatusSub.Close()

	upgradeMuxSettings := connect.DefaultUpgradeMuxSettings()
	upgradeMuxSettings.Dns = nil
	device.SetUpgradeMuxSettings(upgradeMuxSettings)

	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
		Name:              "the connected location",
	}
	device.SetConnectLocation(location)
	firstClient := device.remoteUserNatClient
	firstGeneration := device.GetWindowStatus().ConnectionGeneration
	if calls := generatorCalls.Load(); calls != 1 {
		t.Fatalf("first destination generator calls = %d, want 1", calls)
	}

	// re-applying it implicitly still leaves the live connection alone
	device.SetConnectLocation(cloneConnectLocation(location))
	if calls := generatorCalls.Load(); calls != 1 {
		t.Fatalf("equivalent destination rebuilt generator: calls = %d", calls)
	}
	if generation := device.GetWindowStatus().ConnectionGeneration; generation != firstGeneration {
		t.Fatalf("equivalent destination changed connection generation: got %d, want %d", generation, firstGeneration)
	}

	device.Reconnect(cloneConnectLocation(location))
	if calls := generatorCalls.Load(); calls != 2 {
		t.Fatalf("reconnect to the same destination: generator calls = %d, want 2", calls)
	}
	if device.remoteUserNatClient == firstClient {
		t.Fatalf("reconnect kept the live provider window")
	}
	if generation := device.GetWindowStatus().ConnectionGeneration; generation != firstGeneration+1 {
		t.Fatalf("reconnect connection generation = %d, want %d", generation, firstGeneration+1)
	}
	if !windowStatusRecorder.HasConnectionGeneration(firstGeneration + 1) {
		t.Fatalf("window status listener did not carry reconnect generation %d", firstGeneration+1)
	}
	if got := device.GetConnectLocation(); got == nil || !got.Equals(location) {
		t.Fatalf("reconnect changed the destination: %+v", got)
	}
}

func TestDeviceLocalDestinationTransportChangeRebuildsProviders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}

	var generatorCalls atomic.Int64
	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.AllowProvider = false
	settings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		generatorCalls.Add(1)
		return &rpcLeakTestGenerator{}
	}
	device, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		NewId(),
		settings,
		connect.NewId(),
	)
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer device.Close()

	upgradeMuxSettings := connect.DefaultUpgradeMuxSettings()
	upgradeMuxSettings.Dns = nil
	device.SetUpgradeMuxSettings(upgradeMuxSettings)

	location := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
	}
	device.SetConnectLocation(location)
	networkPeer := cloneConnectLocation(location)
	networkPeer.NetworkPeer = true
	device.SetConnectLocation(networkPeer)

	if calls := generatorCalls.Load(); calls != 2 {
		t.Fatalf("network-peer transport change generator calls = %d, want 2", calls)
	}

	changedSpecs := NewProviderSpecList()
	changedSpecs.Add(&ProviderSpec{ClientId: NewId()})
	device.SetDestination(networkPeer, changedSpecs)
	if calls := generatorCalls.Load(); calls != 3 {
		t.Fatalf("provider-spec transport change generator calls = %d, want 3", calls)
	}
}

func TestDeviceLocalConnectLocationOwnsSetAndGetValues(t *testing.T) {
	device := &DeviceLocal{
		connectLocationChangeListeners: connect.NewCallbackList[ConnectLocationChangeListener](),
	}
	source := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{
			ClientId: NewId(),
		},
		Name: "original",
	}
	device.destinationInitialized = true
	device.connectLocation = cloneConnectLocation(source)
	source.ConnectLocationId.ClientId = NewId()

	first := device.GetConnectLocation()
	if first == nil || first.Name != "original" ||
		first.ConnectLocationId == nil ||
		first.ConnectLocationId.ClientId == nil ||
		first.ConnectLocationId.ClientId.Cmp(source.ConnectLocationId.ClientId) == 0 {
		t.Fatalf("stored location followed caller mutation: %+v", first)
	}
	first.Name = "mutated getter"
	second := device.GetConnectLocation()
	if second == nil || second.Name != "original" {
		t.Fatalf("stored location followed getter mutation: %+v", second)
	}
}

func TestConnectLocationEqualsHandlesOneMissingIdentifier(t *testing.T) {
	identified := &ConnectLocation{
		ConnectLocationId: &ConnectLocationId{BestAvailable: true},
	}
	unidentified := &ConnectLocation{}

	if identified.Equals(unidentified) {
		t.Fatalf("identified location equaled unidentified location")
	}
	if unidentified.Equals(identified) {
		t.Fatalf("unidentified location equaled identified location")
	}
}
