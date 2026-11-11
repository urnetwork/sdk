// Provider send permission may overlap sibling ceilings, never its shared root.
package sdk

import (
	"context"
	"math"
	"reflect"
	"testing"

	"github.com/urnetwork/connect"
)

// Literal endpoint oracles are independent of the candidate sizing path.
type providerSendBorrowCase struct {
	name                                               string
	target, root, client, provider, nat, send, receive ByteCount
}

// These are owner admission values, not physical heap or wire-window limits.
func providerSendBorrowCases() []providerSendBorrowCase {
	return []providerSendBorrowCase{
		{name: "low32", target: 32 * 1024 * 1024, root: 21810380, client: 15099494, provider: 3355443, nat: 3355443, send: 1438047, receive: 1917396},
		{name: "android64", target: 64 * 1024 * 1024, root: 43620760, client: 30198988, provider: 6710886, nat: 6710886, send: 2876094, receive: 3834792},
	}
}

// No workers are launched here; real SDK memory factories own every budget.
// Cleanup releases only successful fixture claims before checking the tree.
type providerSendBorrowFixture struct {
	t        *testing.T
	device   *DeviceLocal
	provider *connect.ClientSettings
	held     map[*connect.TransferMemoryBudget]ByteCount
}

// Keep all setup before publication, including the opt-in and custom profile.
func newProviderSendBorrowFixture(t *testing.T, settings *DeviceLocalSettings) *providerSendBorrowFixture {
	t.Helper()
	memory := newDeviceLocalTransferMemoryForPlatform(settings, settings.LowMemoryProfile)
	configureDeviceLocalClientMemoryForPlatform(settings, memory, settings.LowMemoryProfile)
	_, _, _, providerShare := deviceMemoryShares(settings)
	provider := newDeviceClientSettings(&settings.ClientSettings, "", nil)
	var parent *connect.TransferMemoryBudget
	if memory != nil {
		parent = memory.provider
	}
	send, receive := configureDeviceLocalProviderMemory(provider, providerShare, parent)
	device := &DeviceLocal{settings: settings, transferMemory: memory}
	if settings.AllowProvider && !settings.HostedIncompatible {
		device.provider = &deviceLocalProvider{resendQueueBudget: send, receiveQueueBudget: receive}
	}
	fixture := &providerSendBorrowFixture{t: t, device: device, provider: provider, held: map[*connect.TransferMemoryBudget]ByteCount{}}
	device.applyProvideMemorySharesWithLock(false)
	t.Cleanup(func() {
		for budget, bytes := range fixture.held {
			if 0 < bytes {
				budget.Release(bytes)
			}
		}
		if memory != nil {
			for _, budget := range []*connect.TransferMemoryBudget{memory.root, memory.client, memory.provider, memory.nat, memory.peerKeyPins} {
				stats := budget.Stats()
				if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
					t.Errorf("borrow fixture retained ownership: %+v", stats)
				}
			}
		}
	})
	return fixture
}

// Fixture reservations are actual exact admission, not manually written counters.
func (self *providerSendBorrowFixture) reserve(budget *connect.TransferMemoryBudget, bytes ByteCount) {
	self.t.Helper()
	if !budget.TryReserve(bytes) {
		self.t.Fatalf("cannot reserve %d from %+v", bytes, budget.Stats())
	}
	self.held[budget] += bytes
}

// Fatal cleanup never releases unacquired or already-returned bytes.
func (self *providerSendBorrowFixture) release(budget *connect.TransferMemoryBudget, bytes ByteCount) {
	self.t.Helper()
	if self.held[budget] < bytes {
		self.t.Fatal("fixture release exceeds held claim")
	}
	self.held[budget] -= bytes
	budget.Release(bytes)
}

// Actual device construction clones the caller, attaches the hard hierarchy,
// and preserves the client's profile while the provider enables its loan.
func TestProviderSendBorrowActualConstructor32And64(t *testing.T) {
	auth := testingAuthClientShapeSpace(t)
	for _, row := range providerSendBorrowCases() {
		func() {
			settings := DefaultLowMemoryDeviceLocalSettings()
			settings.MemoryTargetByteCount = row.target
			settings.ProviderSendBorrowByteCount = math.MaxInt64
			settings.DisableLogging = true
			settings.SendBufferSettings.SequenceBufferSize = 3
			callerBudget := settings.SendBufferSettings.ResendQueueBudget
			device := newSettingsOwnershipDevice(t, auth, settings, false)
			defer func() {
				if err := device.CloseAndWait(context.Background()); err != nil {
					t.Error(err)
				}
				stats := device.transferMemory.root.Stats()
				if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
					t.Errorf("%s joined owner: %+v", row.name, stats)
				}
			}()
			if device.provider == nil {
				t.Fatalf("%s missing provider", row.name)
			}
			device.provider.platformTransport.Close()
			memory := device.transferMemory
			send, receive := device.provider.transferBudgets()
			if memory == nil || send.Parent() != memory.provider || receive.Parent() != memory.provider ||
				memory.provider.Parent() != memory.root || memory.client.Parent() != memory.root || memory.nat.Parent() != memory.root {
				t.Fatalf("%s constructor escaped immutable root", row.name)
			}
			if send.TotalByteCount() != 256*1024 || memory.provider.TotalByteCount() != 640*1024 {
				t.Fatalf("%s off role borrowed", row.name)
			}
			func() {
				device.stateLock.Lock()
				defer device.stateLock.Unlock()
				device.applyProvideMemorySharesWithLock(true)
			}()
			if memory.root.TotalByteCount() != row.root || memory.client.TotalByteCount() != row.client ||
				memory.provider.TotalByteCount() != row.provider+row.send || memory.nat.TotalByteCount() != row.nat ||
				send.TotalByteCount() != 2*row.send || receive.TotalByteCount() != row.receive {
				t.Fatalf("%s constructor active borrow totals changed", row.name)
			}
			snapshot := lowMemorySettingsSnapshot(device.settings)
			for range 100 {
				func() {
					device.stateLock.Lock()
					defer device.stateLock.Unlock()
					device.applyProvideMemorySharesWithLock(true)
				}()
			}
			if memory.provider.TotalByteCount() != row.provider+row.send || send.TotalByteCount() != 2*row.send ||
				!reflect.DeepEqual(snapshot, lowMemorySettingsSnapshot(device.settings)) {
				t.Fatalf("%s repeated apply compounded or changed client", row.name)
			}
			if settings.SendBufferSettings.ResendQueueBudget != callerBudget || settings.ProviderSendBorrowByteCount != math.MaxInt64 ||
				device.settings == settings || device.settings.SendBufferSettings.SequenceBufferSize != 3 {
				t.Fatalf("%s caller settings mutated or custom cap lost", row.name)
			}
		}()
	}
}

// Busy client owners retain their entire old ceiling. Extra provider credit is
// available only after real root ownership drains, without a client-cap cut.
func TestProviderSendBorrowFullRootPreservesBusyClient(t *testing.T) {
	for _, row := range providerSendBorrowCases() {
		settings := DefaultLowMemoryDeviceLocalSettings()
		settings.MemoryTargetByteCount, settings.ProviderSendBorrowByteCount = row.target, math.MaxInt64
		f := newProviderSendBorrowFixture(t, settings)
		d, m := f.device, f.device.transferMemory
		d.applyProvideMemorySharesWithLock(true)
		send, receive := d.provider.transferBudgets()
		f.reserve(m.client, row.client)
		f.reserve(m.nat, row.nat)
		f.reserve(receive, row.receive)
		f.reserve(send, row.send)
		if m.root.UsedByteCount() != row.root || send.TryReserve(1) || m.client.TotalByteCount() != row.client {
			t.Fatalf("%s shared root or busy client ceiling changed", row.name)
		}
		f.release(m.client, row.send)
		f.reserve(send, row.send)
		if m.root.UsedByteCount() != row.root || m.client.TryReserve(1) || send.TotalByteCount() != 2*row.send {
			t.Fatalf("%s borrow was not funded by actual release", row.name)
		}
		f.release(send, row.send)
		f.reserve(m.client, row.send)
		if m.root.UsedByteCount() != row.root {
			t.Fatalf("%s client could not reclaim returned root capacity", row.name)
		}
	}
}

// Off/on supersession changes admission only; retained bytes survive every
// shrink and a hundred reapplications without being counted or returned twice.
func TestProviderSendBorrowRoleDisablePreservesOwnedBytes(t *testing.T) {
	for _, row := range providerSendBorrowCases() {
		settings := DefaultLowMemoryDeviceLocalSettings()
		settings.MemoryTargetByteCount, settings.ProviderSendBorrowByteCount = row.target, math.MaxInt64
		f := newProviderSendBorrowFixture(t, settings)
		d, m := f.device, f.device.transferMemory
		d.applyProvideMemorySharesWithLock(true)
		send, _ := d.provider.transferBudgets()
		f.reserve(send, 2*row.send)
		before := m.root.Stats()
		for range 100 {
			d.applyProvideMemorySharesWithLock(false)
			if send.TotalByteCount() != 256*1024 || m.provider.TotalByteCount() != 640*1024 ||
				send.UsedByteCount() != 2*row.send || send.TryReserve(1) || m.nat.TotalByteCount() != row.nat {
				t.Fatalf("%s off role evicted or lent more", row.name)
			}
			d.applyProvideMemorySharesWithLock(true)
			if send.TotalByteCount() != 2*row.send || m.provider.TotalByteCount() != row.provider+row.send || m.client.TotalByteCount() != row.client {
				t.Fatalf("%s reenable compounded", row.name)
			}
		}
		if m.root.Stats() != before {
			t.Fatalf("%s role changes changed retained ownership", row.name)
		}
		d.applyProvideMemorySharesWithLock(false)
		f.reserve(m.client, row.root-2*row.send)
		if m.root.TryReserve(1) {
			t.Fatalf("%s draining provider root was forgotten", row.name)
		}
		f.release(send, 2*row.send)
		f.reserve(m.client, 2*row.send)
	}
}

// The requested allowance is per owner, disabled by non-positive values and
// clipped to the ordinary send ceiling derived from the final target.
func TestProviderSendBorrowConfiguredAmountIsBounded(t *testing.T) {
	for _, row := range providerSendBorrowCases() {
		for _, request := range []ByteCount{-1, 0, 1, row.send / 2, row.send, math.MaxInt64} {
			settings := DefaultLowMemoryDeviceLocalSettings()
			settings.MemoryTargetByteCount, settings.ProviderSendBorrowByteCount = row.target, request
			f := newProviderSendBorrowFixture(t, settings)
			f.device.applyProvideMemorySharesWithLock(true)
			extra := min(max(0, request), row.send)
			send, receive := f.device.provider.transferBudgets()
			if send.TotalByteCount() != row.send+extra || receive.TotalByteCount() != row.receive ||
				f.device.transferMemory.provider.TotalByteCount() != row.provider+extra || f.device.transferMemory.client.TotalByteCount() != row.client {
				t.Fatalf("%s request %d has wrong bounded permission", row.name, request)
			}
		}
	}
}

// Legacy/desktop, targetless and never-providing devices have no borrow path.
func TestProviderSendBorrowExcludedProfilesAreUnchanged(t *testing.T) {
	cases := []struct {
		name               string
		target             ByteCount
		low, allow, hosted bool
	}{
		{name: "desktop", target: 20 * 1024 * 1024, low: false, allow: true},
		{name: "targetless", target: 0, low: true, allow: true},
		{name: "proxy32", target: 32 * 1024 * 1024, low: true, allow: false, hosted: true},
		{name: "disabled32", target: 32 * 1024 * 1024, low: true, allow: false},
	}
	for _, row := range cases {
		var snapshots [][]int64
		for _, request := range []ByteCount{0, math.MaxInt64} {
			settings := defaultDeviceLocalSettingsWithProfile(row.target, row.low)
			settings.AllowProvider, settings.HostedIncompatible, settings.ProviderSendBorrowByteCount = row.allow, row.hosted, request
			f := newProviderSendBorrowFixture(t, settings)
			f.device.applyProvideMemorySharesWithLock(true)
			budgetTotal := func(budget *connect.TransferMemoryBudget) ByteCount {
				if budget == nil {
					return 0
				}
				return budget.TotalByteCount()
			}
			client := &f.device.settings.ClientSettings
			snapshot := []int64{settings.MemoryTargetByteCount, client.SendBufferSettings.ResendQueueMaxByteCount,
				client.ReceiveBufferSettings.ReceiveQueueMaxByteCount, budgetTotal(client.SendBufferSettings.ResendQueueBudget),
				budgetTotal(client.ReceiveBufferSettings.ReceiveQueueBudget), budgetTotal(client.ReceiveBufferSettings.PackQueueBudget),
				budgetTotal(client.WebRtcSettings.MemoryBudget)}
			if m := f.device.transferMemory; m != nil {
				snapshot = append(snapshot, m.root.TotalByteCount(), m.client.TotalByteCount(), m.provider.TotalByteCount(), m.nat.TotalByteCount())
			}
			if f.device.provider != nil {
				send, receive := f.device.provider.transferBudgets()
				if send != nil {
					snapshot = append(snapshot, send.TotalByteCount(), receive.TotalByteCount())
				}
			}
			snapshots = append(snapshots, snapshot)
		}
		if !reflect.DeepEqual(snapshots[0], snapshots[1]) {
			t.Fatalf("%s changed despite missing eligible parent/role", row.name)
		}
	}
}

// A custom foreign or flat send parent cannot gain authority from this device.
func TestProviderSendBorrowCustomParentsRemainOrdinary(t *testing.T) {
	for _, flat := range []bool{false, true} {
		row := providerSendBorrowCases()[0]
		settings := DefaultLowMemoryDeviceLocalSettings()
		settings.ProviderSendBorrowByteCount = math.MaxInt64
		f := newProviderSendBorrowFixture(t, settings)
		foreign := connect.NewTransferMemoryBudget(32 * 1024 * 1024)
		parent := foreign
		if flat {
			parent = nil
		}
		send := connect.NewTransferMemoryBudgetWithParent(512*1024, parent)
		f.device.provider.resendQueueBudget = send
		f.device.applyProvideMemorySharesWithLock(true)
		if send.TotalByteCount() != row.send || f.device.transferMemory.provider.TotalByteCount() != row.provider || foreign.TotalByteCount() != 32*1024*1024 || send.Parent() != parent {
			t.Fatalf("flat=%v foreign parent acquired a device loan", flat)
		}
	}
}

// Simultaneous client leaves retain every pre-existing ceiling. Their shared
// parent and root determine contention, not a synthetic Pack-only donor. These
// profiles back RX with physical bytes, 7/2 of its logical grant, so the leaf
// ceilings together exceed their parent: a client that fills every leaf as far
// as admission allows holds its whole parent, and the provider keeps exactly
// its baseline send. Only client bytes that drain fund the loan.
func TestProviderSendBorrowMixedClientLeaves(t *testing.T) {
	for _, row := range providerSendBorrowCases() {
		settings := DefaultLowMemoryDeviceLocalSettings()
		settings.MemoryTargetByteCount, settings.ProviderSendBorrowByteCount = row.target, math.MaxInt64
		f := newProviderSendBorrowFixture(t, settings)
		d, m := f.device, f.device.transferMemory
		d.applyProvideMemorySharesWithLock(true)
		client := &d.settings.ClientSettings
		_, selected := deviceLocalDestinationWebRtcSettings(client.WebRtcSettings, true)
		rx := client.ReceiveBufferSettings.ReceiveQueueBudget
		leaves := []*connect.TransferMemoryBudget{client.SendBufferSettings.ResendQueueBudget, rx,
			client.ReceiveBufferSettings.PackQueueBudget, client.WebRtcSettings.MemoryBudget, selected, m.peerKeyPins}
		ceilings := ByteCount(0)
		for _, leaf := range leaves {
			ceilings += leaf.TotalByteCount()
		}
		if ceilings <= row.client {
			t.Fatalf("%s client leaf ceilings %d no longer overlap their %d parent", row.name, ceilings, row.client)
		}
		for _, leaf := range leaves {
			if bytes := min(leaf.TotalByteCount(), leaf.Available()); 0 < bytes {
				f.reserve(leaf, bytes)
			}
		}
		if m.client.UsedByteCount() != row.client || m.client.Available() != 0 {
			t.Fatalf("%s busy client leaves did not reach their shared parent: used=%d", row.name, m.client.UsedByteCount())
		}
		send, receive := d.provider.transferBudgets()
		f.reserve(receive, row.receive)
		f.reserve(m.nat, row.nat)
		available := min(send.TotalByteCount(), m.root.Available())
		if available != row.send {
			t.Fatalf("%s busy client leaves left provider send %d, want its baseline %d", row.name, available, row.send)
		}
		f.reserve(send, available)
		if send.TryReserve(1) {
			t.Fatalf("%s provider escaped its leaf/root bound", row.name)
		}
		// RX, not Pack, returns the bytes: whichever client leaf drains funds the loan.
		f.release(rx, row.send)
		f.reserve(send, row.send)
		if send.UsedByteCount() != 2*row.send || send.TryReserve(1) || m.root.Available() != 0 {
			t.Fatalf("%s drained client leaf did not fund exactly the provider loan", row.name)
		}
	}
}
