// Private constructor and lifetime controls for a conserved provider pair.
package sdk

import (
	"encoding/json"
	"testing"

	"github.com/urnetwork/connect"
)

// Values are emitted only after observing the real SDK constructor and resize.
type privateProviderFixedTotalContract struct {
	Schema                    string    `json:"schema"`
	Arm                       string    `json:"arm"`
	Profile                   string    `json:"profile"`
	DeviceTarget              ByteCount `json:"device_target"`
	ProviderShare             ByteCount `json:"provider_share"`
	RootTotal                 ByteCount `json:"root_total"`
	ClientParentTotal         ByteCount `json:"client_parent_total"`
	ProviderParentTotal       ByteCount `json:"provider_parent_total"`
	NatTotal                  ByteCount `json:"nat_total"`
	TransportTotal            ByteCount `json:"transport_total"`
	TransportMaxCount         int       `json:"transport_max_count"`
	H1BaseByteCount           ByteCount `json:"h1_base_byte_count"`
	H1ReservationByteCount    ByteCount `json:"h1_reservation_byte_count"`
	H1MaximumMessageByteCount int64     `json:"h1_maximum_message_byte_count"`
	TransportBufferSize       int       `json:"transport_buffer_size"`
	H1AckPriorityBufferSize   int       `json:"h1_ack_priority_buffer_size"`
	SendTotal                 ByteCount `json:"send_total"`
	ReceiveTotal              ByteCount `json:"receive_total"`
	ResendQueueMax            ByteCount `json:"resend_queue_max"`
	ReceiveQueueMax           ByteCount `json:"receive_queue_max"`
	ReceiveQueueMin           ByteCount `json:"receive_queue_min"`
	PackQueueTotal            ByteCount `json:"pack_queue_total"`
	P2pTotal                  ByteCount `json:"p2p_total"`
	NetworkPeerTotal          ByteCount `json:"network_peer_total"`
	SendRetained              bool      `json:"send_retained"`
	ReceiveRetained           bool      `json:"receive_retained"`
}

// Construct the same mobile owner tree and provider settings without API I/O.
func privateProviderFixedTotalDevice(t *testing.T, target ByteCount) (*DeviceLocal, *connect.ClientSettings) {
	t.Helper()
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.MemoryTargetByteCount = target
	settings.AllowProvider = true
	memory := newDeviceLocalTransferMemoryForPlatform(settings, true)
	configureDeviceLocalClientMemoryForPlatform(settings, memory, true)
	_, _, _, providerShare := deviceMemoryShares(settings)
	providerSettings := newDeviceClientSettings(&settings.ClientSettings, "", nil)
	applyMobileH1PerformanceClientSettingsForPlatform(providerSettings, target, true, true)
	providerSettings.ProviderStreamPolicy = true
	send, receive := configureDeviceLocalProviderMemory(providerSettings, providerShare, memory.provider)
	device := &DeviceLocal{
		settings:                settings,
		transferMemory:          memory,
		provider:                &deviceLocalProvider{resendQueueBudget: send, receiveQueueBudget: receive},
		dnsMemoryTarget:         connect.NewMemoryTarget(target / 10),
		platformTransportBudget: connect.NewPlatformTransportBudgetForMemoryTarget(target),
	}
	constructedSend, constructedReceive := send.TotalByteCount(), receive.TotalByteCount()
	device.applyProvideMemorySharesWithLock(true)
	if send.TotalByteCount() != constructedSend || receive.TotalByteCount() != constructedReceive {
		t.Fatal("constructor and first active resize disagree")
	}
	t.Cleanup(func() {
		for _, budget := range []*connect.TransferMemoryBudget{send, receive, memory.client, memory.provider, memory.nat, memory.root} {
			stats := budget.Stats()
			if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
				t.Errorf("provider owner was not balanced after join: %+v", stats)
			}
		}
	})
	return device, providerSettings
}

// The arm-specific expected table contains literals independent of the helper.
func TestPrivateProviderFixedTotalConstructorContract(t *testing.T) {
	arm, rows := privateProviderFixedTotalExpected()
	for _, row := range rows {
		device, settings := privateProviderFixedTotalDevice(t, row.target)
		memory := device.transferMemory
		send, receive := device.provider.transferBudgets()
		_, clientShare, _, providerShare := deviceMemoryShares(device.settings)
		if send.TotalByteCount() != row.send || receive.TotalByteCount() != row.receive {
			t.Fatalf("%s target=%d got send=%d receive=%d, want %d/%d", arm, row.target, send.TotalByteCount(), receive.TotalByteCount(), row.send, row.receive)
		}
		if send.Parent() != memory.provider || receive.Parent() != memory.provider || memory.provider.Parent() != memory.root || memory.nat.Parent() != memory.root || memory.client.Parent() != memory.root {
			t.Fatal("constructor lost actual parent identity")
		}
		if settings.ReceiveBufferSettings.PackQueueBudget.Parent() != memory.client ||
			settings.WebRtcSettings.MemoryBudget.Parent() != memory.provider ||
			settings.WebRtcSettings.NetworkPeerMemoryBudget.Parent() != memory.provider {
			t.Fatal("constructor projected a sibling leaf under the wrong parent")
		}
		if memory.root.TotalByteCount() != clientShare+providerShare || memory.provider.TotalByteCount() != providerShare/2 || memory.nat.TotalByteCount() != providerShare/2 {
			t.Fatal("provider ratio changed a parent or NAT limit")
		}
		transport := device.platformTransportBudget.Stats()
		platform := newDeviceLocalPlatformTransportSettingsForProfile(row.target, device.platformTransportBudget, nil, "", "", true, nil)
		contract := privateProviderFixedTotalContract{
			Schema: "private-sdk-provider-fixed-total-v1", Arm: arm, Profile: "explicit-mobile-low-h1",
			DeviceTarget: row.target, ProviderShare: providerShare,
			RootTotal: memory.root.TotalByteCount(), ClientParentTotal: memory.client.TotalByteCount(), ProviderParentTotal: memory.provider.TotalByteCount(), NatTotal: memory.nat.TotalByteCount(),
			TransportTotal: transport.TotalByteCount, TransportMaxCount: transport.MaxTransportCount,
			H1BaseByteCount: platform.H1BudgetByteCount, H1ReservationByteCount: platform.H1MemoryReservationByteCount(),
			H1MaximumMessageByteCount: platform.H1MaxMessageByteCount, TransportBufferSize: platform.TransportBufferSize, H1AckPriorityBufferSize: platform.H1AckPriorityBufferSize,
			SendTotal: send.TotalByteCount(), ReceiveTotal: receive.TotalByteCount(),
			ResendQueueMax:  settings.SendBufferSettings.ResendQueueMaxByteCount,
			ReceiveQueueMax: settings.ReceiveBufferSettings.ReceiveQueueMaxByteCount, ReceiveQueueMin: settings.ReceiveBufferSettings.ReceiveQueueMinByteCount,
			PackQueueTotal:   settings.ReceiveBufferSettings.PackQueueBudget.TotalByteCount(),
			P2pTotal:         settings.WebRtcSettings.MemoryBudget.TotalByteCount(),
			NetworkPeerTotal: settings.WebRtcSettings.NetworkPeerMemoryBudget.TotalByteCount(),
			SendRetained:     settings.SendBufferSettings.ResendQueueRetainedByteAccounting,
			ReceiveRetained:  settings.ReceiveBufferSettings.ReceiveQueueRetainedByteAccounting,
		}
		encoded, err := json.Marshal(contract)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("[private-provider-constructor] %s", encoded)
	}
}

// No-target construction preserves the caller's existing budget pointers.
func TestPrivateProviderFixedTotalLegacyNoop(t *testing.T) {
	for _, target := range []ByteCount{-1, 0} {
		settings := connect.DefaultClientSettings()
		send, receive := settings.SendBufferSettings.ResendQueueBudget, settings.ReceiveBufferSettings.ReceiveQueueBudget
		configuredSend, configuredReceive := configureDeviceLocalProviderMemory(settings, target)
		if configuredSend != nil || configuredReceive != nil || settings.SendBufferSettings.ResendQueueBudget != send || settings.ReceiveBufferSettings.ReceiveQueueBudget != receive {
			t.Fatal("targetless constructor changed historical wiring")
		}
	}
}

// A role flip resizes the same leaves; constructors and active resize agree.
func TestPrivateProviderFixedTotalConstructionResizeEquivalence(t *testing.T) {
	_, rows := privateProviderFixedTotalExpected()
	for _, row := range rows {
		device, settings := privateProviderFixedTotalDevice(t, row.target)
		send, receive := device.provider.transferBudgets()
		pack := settings.ReceiveBufferSettings.PackQueueBudget
		root, nat := device.transferMemory.root.TotalByteCount(), device.transferMemory.nat.TotalByteCount()
		device.applyProvideMemorySharesWithLock(false)
		if send.TotalByteCount() != 256*1024 || receive.TotalByteCount() != 384*1024 {
			t.Fatal("idle floors changed")
		}
		device.applyProvideMemorySharesWithLock(true)
		nextSend, nextReceive := device.provider.transferBudgets()
		if nextSend != send || nextReceive != receive || settings.ReceiveBufferSettings.PackQueueBudget != pack || send.TotalByteCount() != row.send || receive.TotalByteCount() != row.receive || device.transferMemory.root.TotalByteCount() != root || device.transferMemory.nat.TotalByteCount() != nat {
			t.Fatal("resize replaced or mis-sized an existing owner")
		}
	}
}

// Independent NAT/client claims still constrain both provider directions.
func TestPrivateProviderFixedTotalParentAdmission(t *testing.T) {
	_, rows := privateProviderFixedTotalExpected()
	for _, row := range rows {
		device, _ := privateProviderFixedTotalDevice(t, row.target)
		memory := device.transferMemory
		send, receive := device.provider.transferBudgets()
		if !send.TryReserve(row.send) {
			t.Fatal("funded provider send could not admit")
		}
		t.Cleanup(func() { send.Release(row.send) })
		if !receive.TryReserve(row.receive) {
			t.Fatal("funded provider receive could not admit")
		}
		t.Cleanup(func() { receive.Release(row.receive) })
		parentRemainder := memory.provider.TotalByteCount() - row.send - row.receive
		if parentRemainder > 0 {
			if !memory.provider.TryReserve(parentRemainder) {
				t.Fatal("exact parent residue was not available")
			}
			t.Cleanup(func() { memory.provider.Release(parentRemainder) })
		}
		if memory.provider.TryReserve(1) {
			memory.provider.Release(1)
			t.Fatal("provider parent admitted above its real cap")
		}
		nat := memory.nat.TotalByteCount()
		if !memory.nat.TryReserve(nat) {
			t.Fatal("separate NAT allowance disappeared")
		}
		t.Cleanup(func() { memory.nat.Release(nat) })
		client := memory.root.TotalByteCount() - memory.provider.TotalByteCount() - nat
		if !memory.client.TryReserve(client) {
			t.Fatal("unchanged root remainder refused")
		}
		t.Cleanup(func() { memory.client.Release(client) })
		if memory.root.UsedByteCount() != memory.root.TotalByteCount() {
			t.Fatal("aggregate claim did not reach the exact root cap")
		}
		if memory.root.TryReserve(1) {
			memory.root.Release(1)
			t.Fatal("aggregate admission escaped the root")
		}
	}
}

// Outstanding receive ownership survives a shrinking role until actual drain.
func TestPrivateProviderFixedTotalShrinkDrain(t *testing.T) {
	_, rows := privateProviderFixedTotalExpected()
	for _, row := range rows {
		device, _ := privateProviderFixedTotalDevice(t, row.target)
		memory := device.transferMemory
		send, receive := device.provider.transferBudgets()
		if !receive.TryReserve(row.receive) {
			t.Fatal("initial receive admission failed")
		}
		device.applyProvideMemorySharesWithLock(false)
		if receive.UsedByteCount() != row.receive || memory.root.UsedByteCount() != row.receive || receive.TryReserve(1) || send.TryReserve(1) {
			t.Fatal("shrunk provider lost retained ownership or admitted new work")
		}
		notify := send.CapacityNotify()
		receive.Release(row.receive)
		select {
		case <-notify:
		default:
			t.Fatal("real drain did not notify the sibling")
		}
		if !send.TryReserve(1) {
			t.Fatal("idle control credit did not recover after drain")
		}
		send.Release(1)
		device.applyProvideMemorySharesWithLock(true)
		if !receive.TryReserve(row.receive) {
			t.Fatal("return to active policy did not recover")
		}
		receive.Release(row.receive)
	}
}

// Desktop/miner constructors keep their existing unparented profile distinction.
func TestPrivateProviderFixedTotalDesktopProfile(t *testing.T) {
	_, rows := privateProviderFixedTotalExpected()
	for _, row := range rows {
		settings := DefaultDeviceLocalSettings()
		settings.MemoryTargetByteCount = row.target
		settings.LowMemoryProfile = false
		if newDeviceLocalTransferMemoryForPlatform(settings, false) != nil {
			t.Fatal("desktop profile acquired a mobile hierarchy")
		}
		_, _, _, share := deviceMemoryShares(settings)
		provider := newDeviceClientSettings(&settings.ClientSettings, "", nil)
		send, receive := configureDeviceLocalProviderMemory(provider, share)
		if send.Parent() != nil || receive.Parent() != nil || send.TotalByteCount() != row.send || receive.TotalByteCount() != row.receive {
			t.Fatal("desktop constructor lost its exact split or profile")
		}
	}
}
