package sdk

import (
	"context"
	mathrand "math/rand/v2"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// every report, as a device with a listener of each builds them
func testing_everyContractReport() deviceContractReports {
	return deviceContractReports{
		egressStats:    true,
		ingressStats:   true,
		egressDetails:  true,
		ingressDetails: true,
	}
}

// A details list's entries in contract id order. A tracker lists its
// contracts in map order.
func testing_sortedContractDetails(details *ContractDetailsList) []*ContractDetails {
	if details == nil {
		return nil
	}
	sorted := slices.Clone(details.getAll())
	slices.SortFunc(sorted, func(a *ContractDetails, b *ContractDetails) int {
		return a.ContractId.Cmp(b.ContractId)
	})
	return sorted
}

// The state the tracker keeps for its getters and its lifecycle.
func testing_contractTrackerState(tracker *deviceContractTracker) any {
	copyContracts := func(contracts map[connect.Id]*deviceContractState) map[connect.Id]deviceContractState {
		copies := map[connect.Id]deviceContractState{}
		for contractId, state := range contracts {
			copies[contractId] = *state
		}
		return copies
	}
	return []any{
		copyContracts(tracker.egressContracts),
		copyContracts(tracker.ingressContracts),
		tracker.lastSeenSequences,
		tracker.lastEmitTime,
		tracker.pendingEmit,
		tracker.settled(),
	}
}

// One emit's reports.
type testing_contractReports struct {
	egressStats    *ContractStats
	ingressStats   *ContractStats
	egressDetails  []*ContractDetails
	ingressDetails []*ContractDetails
}

func testing_newContractReports(egressStats *ContractStats, ingressStats *ContractStats, egressDetails *ContractDetailsList, ingressDetails *ContractDetailsList) testing_contractReports {
	return testing_contractReports{
		egressStats:    egressStats,
		ingressStats:   ingressStats,
		egressDetails:  testing_sortedContractDetails(egressDetails),
		ingressDetails: testing_sortedContractDetails(ingressDetails),
	}
}

// The reports a tracker that builds only `reports` gives, from the reports of
// one that builds every report.
func testing_contractReportsFor(all testing_contractReports, reports deviceContractReports) testing_contractReports {
	wanted := testing_contractReports{}
	if reports.egressStats {
		wanted.egressStats = all.egressStats
	}
	if reports.ingressStats {
		wanted.ingressStats = all.ingressStats
	}
	if reports.egressDetails {
		wanted.egressDetails = all.egressDetails
	}
	if reports.ingressDetails {
		wanted.ingressDetails = all.ingressDetails
	}
	return wanted
}

// Three trackers take the same seeded steps in virtual time: opens, byte
// progress, closes, replayed and stale events, gated batches, flushes and
// clears. One builds every report, as the tracker did before it built only the
// listened reports; one builds none, as for a device nobody listens to; one
// builds a different subset at each step. After every step their contract
// state, lifecycle and getter reports are the same, the subset tracker's
// reports are exactly the full tracker's reports it builds, and the tracker
// without listeners builds nothing.
func TestDeviceContractTrackerBuildsOnlyListenedReports(t *testing.T) {
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(8, 1122))
	epoch := time.Second
	all := newDeviceContractTracker()
	none := newDeviceContractTracker()
	some := newDeviceContractTracker()

	us := connect.NewId()
	peers := []connect.Id{connect.NewId(), connect.NewId(), connect.NewId()}
	type contract struct {
		id        connect.Id
		receive   bool
		path      connect.TransferPath
		used      ByteCount
		sequence  uint64
		closed    bool
		lastEvent *connect.ContractStatsEvent
	}
	contracts := []*contract{}
	now := time.Date(2026, 11, 22, 7, 40, 0, 0, time.UTC)
	emits := 0
	for step := range 4000 {
		now = now.Add(time.Duration(r.Int64N(int64(3 * epoch / 2))))
		reports := deviceContractReports{
			egressStats:    r.IntN(2) == 0,
			ingressStats:   r.IntN(2) == 0,
			egressDetails:  r.IntN(2) == 0,
			ingressDetails: r.IntN(2) == 0,
		}
		var allReports, noneReports, someReports testing_contractReports
		switch action := r.IntN(10); {
		case action < 7:
			events := []*connect.ContractStatsEvent{}
			for range 1 + r.IntN(4) {
				if len(contracts) == 0 || r.IntN(4) == 0 {
					peer := peers[r.IntN(len(peers))]
					path := connect.TransferPath{SourceId: us, DestinationId: peer}
					receive := r.IntN(2) == 0
					if receive {
						path = connect.TransferPath{SourceId: peer, DestinationId: us}
					}
					contracts = append(contracts, &contract{id: connect.NewId(), receive: receive, path: path})
				}
				c := contracts[r.IntN(len(contracts))]
				if c.closed {
					// a replay of the final event, which the tracker discards
					if c.lastEvent != nil {
						events = append(events, c.lastEvent)
					}
					continue
				}
				delta := ByteCount(r.Int64N(1 << 20))
				c.used += delta
				c.sequence += 1
				c.closed = r.IntN(8) == 0
				event := &connect.ContractStatsEvent{
					ContractId:         c.id,
					Receive:            c.receive,
					Path:               c.path,
					TransferByteCount:  128 * 1024 * 1024,
					UsedByteCount:      c.used,
					UsedByteCountDelta: delta,
					Open:               !c.closed,
					Sequence:           c.sequence,
				}
				c.lastEvent = event
				events = append(events, event)
			}
			allReports = testing_newContractReports(all.updateAt(now, events, epoch, testing_everyContractReport()))
			noneReports = testing_newContractReports(none.updateAt(now, events, epoch, deviceContractReports{}))
			someReports = testing_newContractReports(some.updateAt(now, events, epoch, reports))
		case action < 9:
			allReports = testing_newContractReports(all.flushPendingAt(now, epoch, testing_everyContractReport()))
			noneReports = testing_newContractReports(none.flushPendingAt(now, epoch, deviceContractReports{}))
			someReports = testing_newContractReports(some.flushPendingAt(now, epoch, reports))
		default:
			all.clear()
			none.clear()
			some.clear()
			contracts = nil
		}
		if allReports.egressStats != nil {
			emits += 1
		}
		if !reflect.DeepEqual(noneReports, testing_contractReports{}) {
			t.Fatalf("step %d: a tracker without listeners built reports: %+v", step, noneReports)
		}
		if !reflect.DeepEqual(someReports, testing_contractReportsFor(allReports, reports)) {
			t.Fatalf("step %d: the listened reports differ from the full emit's:\n%+v\n%+v", step, someReports, testing_contractReportsFor(allReports, reports))
		}
		state := testing_contractTrackerState(all)
		if !reflect.DeepEqual(testing_contractTrackerState(none), state) || !reflect.DeepEqual(testing_contractTrackerState(some), state) {
			t.Fatalf("step %d: the trackers' state differs", step)
		}
		for _, receive := range []bool{false, true} {
			stats := all.stats(receive)
			details := testing_sortedContractDetails(all.details(receive))
			for _, other := range []*deviceContractTracker{none, some} {
				if !reflect.DeepEqual(other.stats(receive), stats) || !reflect.DeepEqual(testing_sortedContractDetails(other.details(receive)), details) {
					t.Fatalf("step %d: the getter reports differ", step)
				}
			}
		}
	}
	if emits < 500 {
		t.Fatalf("only %d emits", emits)
	}
}

// An emit with no listened report allocates nothing, where it built a stats
// report and a details entry for every contract before. With every report
// listened, it builds them all.
func TestDeviceContractTrackerWithoutListenersBuildsNoReports(t *testing.T) {
	tracker := newDeviceContractTracker()
	us := connect.NewId()
	events := []*connect.ContractStatsEvent{}
	for i := range 40 {
		peer := connect.NewId()
		events = append(events, &connect.ContractStatsEvent{
			ContractId:        connect.NewId(),
			Receive:           i%2 == 0,
			Path:              connect.TransferPath{SourceId: us, DestinationId: peer},
			TransferByteCount: 128 * 1024 * 1024,
			UsedByteCount:     ByteCount(1000 * (i + 1)),
			Open:              true,
		})
	}
	now := time.Date(2026, 11, 22, 7, 40, 0, 0, time.UTC)
	tracker.updateAt(now, events, time.Second, deviceContractReports{})
	emit := func(reports deviceContractReports) float64 {
		return testing.AllocsPerRun(50, func() {
			now = now.Add(time.Second)
			tracker.emit(now, reports)
		})
	}
	if allocations := emit(deviceContractReports{}); allocations != 0 {
		t.Fatalf("an emit without listeners allocated %.0f objects", allocations)
	}
	if allocations := emit(testing_everyContractReport()); allocations < float64(len(events)) {
		t.Fatalf("an emit with every report listened allocated only %.0f objects for %d contracts", allocations, len(events))
	}
}

// Collects every details report a listener gets.
type testing_contractDetailsCollector struct {
	stateLock sync.Mutex
	details   map[connect.Id]*ContractDetails
}

func (self *testing_contractDetailsCollector) ContractDetailsChanged(contractDetails *ContractDetails) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.details == nil {
		self.details = map[connect.Id]*ContractDetails{}
	}
	self.details[contractDetails.ContractId.toConnectId()] = contractDetails
}

func (self *testing_contractDetailsCollector) take() []*ContractDetails {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	taken := []*ContractDetails{}
	for _, details := range self.details {
		taken = append(taken, details)
	}
	self.details = nil
	slices.SortFunc(taken, func(a *ContractDetails, b *ContractDetails) int {
		return a.ContractId.Cmp(b.ContractId)
	})
	return taken
}

// The multi client's contract stats callback on a device: with no contract
// listener an emit builds and dispatches nothing, while the getters keep
// answering from the folded state; a details listener gets the details the
// getter gives at that emit, and once it is removed the emits build nothing
// again.
func TestDeviceLocalContractStatsBuildsOnlyForListeners(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = false
	settings.Verbose = false
	settings.DisableLogging = true
	// the flush loop waits out its first epoch; each update below opens the
	// gate itself, so every update emits
	settings.ContractStatsEpoch = time.Hour
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	defer device.Close()

	us := connect.NewId()
	events := []*connect.ContractStatsEvent{}
	for i := range 20 {
		events = append(events, &connect.ContractStatsEvent{
			ContractId:         connect.NewId(),
			Path:               connect.TransferPath{SourceId: us, DestinationId: connect.NewId()},
			TransferByteCount:  128 * 1024 * 1024,
			UsedByteCount:      ByteCount(1000 * (i + 1)),
			UsedByteCountDelta: 1000,
			Open:               true,
		})
	}
	update := func() {
		func() {
			device.stateLock.Lock()
			defer device.stateLock.Unlock()
			device.contracts.lastEmitTime = time.Time{}
		}()
		device.updateContractStatsEvents(events)
	}
	update()

	if allocations := testing.AllocsPerRun(50, update); allocations != 0 {
		t.Fatalf("an emit without contract listeners allocated %.0f objects", allocations)
	}
	if stats := device.GetEgressContractStats(); stats.ContractUsedByteCount != 210000 {
		t.Fatalf("the getter lost the folded state: %+v", stats)
	}

	collector := &testing_contractDetailsCollector{}
	sub := device.AddEgressContractDetailsChangeListener(collector)
	if allocations := testing.AllocsPerRun(10, update); allocations < float64(len(events)) {
		t.Fatalf("an emit with a details listener allocated only %.0f objects", allocations)
	}
	collector.take()
	update()
	received := collector.take()
	if want := testing_sortedContractDetails(device.GetEgressContractDetails()); len(received) != len(events) || !reflect.DeepEqual(received, want) {
		t.Fatalf("the listener got %d details, not the getter's %d", len(received), len(want))
	}

	sub.Close()
	if allocations := testing.AllocsPerRun(50, update); allocations != 0 {
		t.Fatalf("an emit after the listener left allocated %.0f objects", allocations)
	}
	if received := collector.take(); len(received) != 0 {
		t.Fatalf("a removed listener got %d details", len(received))
	}
}
