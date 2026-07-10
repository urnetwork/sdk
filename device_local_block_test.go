package sdk

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func testing_newBlockDevice(ctx context.Context, t *testing.T, allowProvider bool) (*DeviceLocal, *NetworkSpace) {
	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	if err != nil {
		t.Fatalf("network space: %v", err)
	}
	device := testing_newBlockDeviceWithNetworkSpace(t, networkSpace, byJwt, allowProvider)
	return device, networkSpace
}

func testing_newBlockDeviceWithNetworkSpace(t *testing.T, networkSpace *NetworkSpace, byJwt string, allowProvider bool) *DeviceLocal {
	settings := DefaultDeviceLocalSettings()
	settings.AllowProvider = allowProvider
	settings.Verbose = false
	settings.DisableLogging = true
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	return device
}

type testing_blockListener struct {
	stateLock sync.Mutex
	count     int
}

func (self *testing_blockListener) fired() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.count += 1
}

func (self *testing_blockListener) fireCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.count
}

type testing_overridesListener struct {
	testing_blockListener
	overrides *BlockActionOverrideList
}

func (self *testing_overridesListener) BlockActionOverridesChanged(overrides *BlockActionOverrideList) {
	self.stateLock.Lock()
	self.overrides = overrides
	self.stateLock.Unlock()
	self.fired()
}

type testing_windowListener struct {
	testing_blockListener
	window *BlockActionWindow
}

func (self *testing_windowListener) BlockActionWindowChanged(window *BlockActionWindow) {
	self.stateLock.Lock()
	self.window = window
	self.stateLock.Unlock()
	self.fired()
}

type testing_blockStatsListener struct {
	testing_blockListener
	blockStats *BlockStats
}

func (self *testing_blockStatsListener) BlockStatsChanged(blockStats *BlockStats) {
	self.stateLock.Lock()
	self.blockStats = blockStats
	self.stateLock.Unlock()
	self.fired()
}

type testing_packetStatsListener struct {
	testing_blockListener
	packetStats *PacketStats
}

func (self *testing_packetStatsListener) PacketStatsChanged(packetStats *PacketStats) {
	self.stateLock.Lock()
	self.packetStats = packetStats
	self.stateLock.Unlock()
	self.fired()
}

type testing_contractStatsListener struct {
	testing_blockListener
	contractStats *ContractStats
}

func (self *testing_contractStatsListener) ContractStatsChanged(contractStats *ContractStats) {
	self.stateLock.Lock()
	self.contractStats = contractStats
	self.stateLock.Unlock()
	self.fired()
}

type testing_contractDetailsListener struct {
	testing_blockListener
	contractDetails *ContractDetails
}

func (self *testing_contractDetailsListener) ContractDetailsChanged(contractDetails *ContractDetails) {
	self.stateLock.Lock()
	self.contractDetails = contractDetails
	self.stateLock.Unlock()
	self.fired()
}

type testing_dnsListener struct {
	testing_blockListener
}

func (self *testing_dnsListener) DnsResolverSettingsChanged(dnsResolverSettings *DnsResolverSettings) {
	self.fired()
}

func testing_stringList(values ...string) *StringList {
	list := NewStringList()
	list.addAll(values...)
	return list
}

func TestDeviceLocalBlockActionOverrides(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device, networkSpace := testing_newBlockDevice(ctx, t, false)
	defer device.Close()

	overridesListener := &testing_overridesListener{}
	sub := device.AddBlockActionOverridesChangeListener(overridesListener)
	defer sub.Close()

	localOverrideId := NewId()
	localOverride := &BlockActionOverride{
		OverrideId:    localOverrideId,
		Hosts:         testing_stringList("example.com"),
		AppIds:        testing_stringList("com.app.local"),
		RouteOverride: &RouteOverride{Local: true},
	}
	remoteOverride := &BlockActionOverride{
		OverrideId:    NewId(),
		Hosts:         testing_stringList("*.tracker.net"),
		AppIds:        testing_stringList("com.app.remote"),
		BlockOverride: &BlockOverride{Block: true},
		RouteOverride: &RouteOverride{Local: false},
	}

	device.AddBlockActionOverride(localOverride)
	device.AddBlockActionOverride(remoteOverride)

	if count := device.GetBlockActionOverrides().Len(); count != 2 {
		t.Fatalf("expected 2 overrides, got %d", count)
	}
	if count := overridesListener.fireCount(); count != 2 {
		t.Fatalf("expected 2 listener fires, got %d", count)
	}

	// replacing the same override id does not duplicate
	device.AddBlockActionOverride(&BlockActionOverride{
		OverrideId:    localOverrideId,
		Hosts:         testing_stringList("example.com", "example.org"),
		AppIds:        testing_stringList("com.app.local"),
		RouteOverride: &RouteOverride{Local: true},
	})
	if count := device.GetBlockActionOverrides().Len(); count != 2 {
		t.Fatalf("expected 2 overrides after replace, got %d", count)
	}

	// derived app ids
	localOverrideAppIds := device.GetLocalOverrideAppIds()
	if !localOverrideAppIds.Included.Contains("com.app.local") {
		t.Fatalf("expected com.app.local included")
	}
	if !localOverrideAppIds.Excluded.Contains("com.app.remote") {
		t.Fatalf("expected com.app.remote excluded")
	}

	// persisted to local state (async)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if persisted := localState.GetBlockActionOverrides(); persisted != nil && persisted.Len() == 2 {
			break
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("overrides were not persisted")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// a new device on the same network space restores the persisted overrides
	device2 := testing_newBlockDeviceWithNetworkSpace(t, networkSpace, "", false)
	defer device2.Close()
	if count := device2.GetBlockActionOverrides().Len(); count != 2 {
		t.Fatalf("expected 2 restored overrides, got %d", count)
	}

	// remove
	device.RemoveBlockActionOverride(remoteOverride.OverrideId)
	if count := device.GetBlockActionOverrides().Len(); count != 1 {
		t.Fatalf("expected 1 override after remove, got %d", count)
	}

	// set replaces everything
	device.SetBlockActionOverrides(NewBlockActionOverrideList())
	if count := device.GetBlockActionOverrides().Len(); count != 0 {
		t.Fatalf("expected 0 overrides after set, got %d", count)
	}
}

func TestDeviceLocalBlockActions(t *testing.T) {
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
	settings.BlockActionWindowDuration = 1 * time.Second
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	defer device.Close()

	blockOverride := &BlockActionOverride{
		OverrideId:    NewId(),
		Hosts:         testing_stringList("blocked.example.com"),
		BlockOverride: &BlockOverride{Block: true},
	}
	device.AddBlockActionOverride(blockOverride)

	windowListener := &testing_windowListener{}
	sub := device.AddBlockActionWindowChangeListener(windowListener)
	defer sub.Close()

	overrideConnectId := blockOverride.OverrideId.toConnectId()
	removedOverrideId := connect.NewId()
	now := time.Now()
	device.updateBlockActions([]*connect.BlockAction{
		// expired, outside the window
		{
			Time:  now.Add(-2 * time.Second),
			Ips:   []netip.Addr{netip.MustParseAddr("1.0.0.9")},
			Hosts: []string{"old.example.com"},
			Block: false,
			Local: true,
		},
		// current, blocked by the override
		{
			Time:            now,
			Ips:             []netip.Addr{netip.MustParseAddr("1.0.0.1"), netip.MustParseAddr("1.0.0.2")},
			Hosts:           []string{"blocked.example.com"},
			Block:           true,
			BlockOverrideId: &overrideConnectId,
			PacketCount:     3,
			ByteCount:       300,
		},
		// current, routed local by an override removed since the decision
		{
			Time:            now,
			Ips:             []netip.Addr{netip.MustParseAddr("1.0.0.3")},
			Hosts:           []string{"local.example.com"},
			Local:           true,
			RouteOverrideId: &removedOverrideId,
			PacketCount:     1,
			ByteCount:       100,
		},
	})

	if count := windowListener.fireCount(); count != 1 {
		t.Fatalf("expected 1 window fire, got %d", count)
	}

	window := device.GetBlockActions()
	if count := window.BlockActions.Len(); count != 2 {
		t.Fatalf("expected 2 windowed actions, got %d", count)
	}
	blockAction := window.BlockActions.Get(0)
	if !blockAction.Block {
		t.Fatalf("expected blocked action")
	}
	if blockAction.BlockOverride == nil || !blockAction.BlockOverride.Block {
		t.Fatalf("expected resolved block override")
	}
	if blockAction.OverrideId == nil || blockAction.OverrideId.Cmp(blockOverride.OverrideId) != 0 {
		t.Fatalf("expected the deciding override id, got %+v", blockAction.OverrideId)
	}
	if blockAction.Ips.Len() != 2 || !blockAction.Ips.Contains("1.0.0.2") {
		t.Fatalf("expected cluster ips")
	}
	if !blockAction.Hosts.Contains("blocked.example.com") {
		t.Fatalf("expected cluster hosts")
	}
	if blockAction.PacketCount != 3 || blockAction.ByteCount != 300 {
		t.Fatalf("expected packet counts")
	}
	if blockAction.BlockActionId == nil {
		t.Fatalf("expected a block action id")
	}

	// the removed override still identifies the decision, with the override
	// value falling back to the decision itself
	routeAction := window.BlockActions.Get(1)
	if routeAction.OverrideId == nil || routeAction.OverrideId.Cmp(newId(removedOverrideId)) != 0 {
		t.Fatalf("expected the removed override id, got %+v", routeAction.OverrideId)
	}
	if routeAction.RouteOverride == nil || !routeAction.RouteOverride.Local {
		t.Fatalf("expected the decision-fallback route override %+v", routeAction.RouteOverride)
	}
	if routeAction.BlockOverride != nil {
		t.Fatalf("expected no block override %+v", routeAction.BlockOverride)
	}
}

func TestDeviceLocalPacketAndBlockStats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device, _ := testing_newBlockDevice(ctx, t, false)
	defer device.Close()

	packetStatsListener := &testing_packetStatsListener{}
	packetStatsSub := device.AddPacketStatsChangeListener(packetStatsListener)
	defer packetStatsSub.Close()

	blockStatsListener := &testing_blockStatsListener{}
	blockStatsSub := device.AddBlockStatsChangeListener(blockStatsListener)
	defer blockStatsSub.Close()

	device.updatePacketStats(&connect.PacketStats{
		RemoteEgressPacketCount:  10,
		RemoteEgressByteCount:    1000,
		RemoteIngressPacketCount: 20,
		RemoteIngressByteCount:   2000,
		LocalEgressPacketCount:   5,
		LocalEgressByteCount:     500,
		LocalIngressPacketCount:  6,
		LocalIngressByteCount:    600,
		BlockEgressPacketCount:   2,
		BlockEgressByteCount:     64,
		BlockIngressPacketCount:  1,
		BlockIngressByteCount:    32,
	})

	if count := packetStatsListener.fireCount(); count != 1 {
		t.Fatalf("expected 1 packet stats fire, got %d", count)
	}
	func() {
		packetStatsListener.stateLock.Lock()
		defer packetStatsListener.stateLock.Unlock()
		if packetStatsListener.packetStats.RemoteEgressByteCount != 1000 {
			t.Fatalf("unexpected packet stats %v", packetStatsListener.packetStats)
		}
		if packetStatsListener.packetStats.LocalIngressByteCount != 600 {
			t.Fatalf("unexpected packet stats %v", packetStatsListener.packetStats)
		}
		if packetStatsListener.packetStats.BlockEgressPacketCount != 2 || packetStatsListener.packetStats.BlockEgressByteCount != 64 {
			t.Fatalf("unexpected packet stats %v", packetStatsListener.packetStats)
		}
		if packetStatsListener.packetStats.BlockIngressPacketCount != 1 || packetStatsListener.packetStats.BlockIngressByteCount != 32 {
			t.Fatalf("unexpected packet stats %v", packetStatsListener.packetStats)
		}
	}()
	if count := blockStatsListener.fireCount(); count != 1 {
		t.Fatalf("expected 1 block stats fire, got %d", count)
	}
	func() {
		blockStatsListener.stateLock.Lock()
		defer blockStatsListener.stateLock.Unlock()
		if blockStatsListener.blockStats.AllowedCount != 15 {
			t.Fatalf("unexpected allowed count %d", blockStatsListener.blockStats.AllowedCount)
		}
		// the block stats sum both directions
		if blockStatsListener.blockStats.BlockedCount != 3 {
			t.Fatalf("unexpected blocked count %d", blockStatsListener.blockStats.BlockedCount)
		}
	}()

	// an identical update does not re-fire the block stats
	device.updatePacketStats(&connect.PacketStats{
		RemoteEgressPacketCount: 10,
		LocalEgressPacketCount:  5,
		BlockEgressPacketCount:  2,
		BlockIngressPacketCount: 1,
	})
	if count := blockStatsListener.fireCount(); count != 1 {
		t.Fatalf("expected no block stats re-fire, got %d", count)
	}

	// the getter combines the accumulated base (zero here, no live client)
	packetStats := device.GetPacketStats()
	if packetStats.RemoteEgressPacketCount != 0 {
		t.Fatalf("expected zero accumulated stats, got %v", packetStats)
	}
}

func TestDeviceLocalContractStats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device, _ := testing_newBlockDevice(ctx, t, false)
	defer device.Close()

	egressStatsListener := &testing_contractStatsListener{}
	egressStatsSub := device.AddEgressContractStatsChangeListener(egressStatsListener)
	defer egressStatsSub.Close()

	us := connect.NewId()
	peer1 := connect.NewId()
	peer2 := connect.NewId()
	stream := connect.NewId()

	egressContractId := connect.NewId()
	companionContractId := connect.NewId()
	otherIngressContractId := connect.NewId()

	device.updateContractStatsEvents([]*connect.ContractStatsEvent{
		{
			ContractId:         egressContractId,
			Receive:            false,
			Path:               connect.TransferPath{SourceId: us, DestinationId: peer1, StreamId: stream},
			TransferByteCount:  10000,
			UsedByteCount:      1000,
			UsedByteCountDelta: 1000,
			Open:               true,
		},
		{
			ContractId:         companionContractId,
			Receive:            true,
			Companion:          false,
			Path:               connect.TransferPath{SourceId: peer1, DestinationId: us, StreamId: stream},
			TransferByteCount:  20000,
			UsedByteCount:      500,
			UsedByteCountDelta: 500,
			Open:               true,
		},
		{
			ContractId:        otherIngressContractId,
			Receive:           true,
			Path:              connect.TransferPath{SourceId: peer2, DestinationId: us},
			TransferByteCount: 20000,
			UsedByteCount:     50,
			Open:              true,
		},
	})

	if count := egressStatsListener.fireCount(); count != 1 {
		t.Fatalf("expected 1 egress stats fire, got %d", count)
	}

	egressStats := device.GetEgressContractStats()
	if egressStats.ContractUsedByteCount != 1000 || egressStats.ContractByteCount != 10000 {
		t.Fatalf("unexpected egress stats %+v", egressStats)
	}
	if egressStats.CompanionContractUsedByteCount != 550 {
		t.Fatalf("unexpected egress companion stats %+v", egressStats)
	}

	ingressStats := device.GetIngressContractStats()
	if ingressStats.ContractUsedByteCount != 550 || ingressStats.CompanionContractUsedByteCount != 1000 {
		t.Fatalf("unexpected ingress stats %+v", ingressStats)
	}

	// the egress contract pairs with the exact reverse path companion
	egressDetails := device.GetEgressContractDetails()
	if egressDetails.Len() != 1 {
		t.Fatalf("expected 1 egress details, got %d", egressDetails.Len())
	}
	details := egressDetails.Get(0)
	if details.ContractId.Cmp(newId(egressContractId)) != 0 {
		t.Fatalf("unexpected contract id")
	}
	if details.CompanionContractId == nil || details.CompanionContractId.Cmp(newId(companionContractId)) != 0 {
		t.Fatalf("expected the reverse path companion")
	}
	if details.ContractUsedByteCount != 1000 || details.CompanionContractUsedByteCount != 500 {
		t.Fatalf("unexpected details %+v", details)
	}

	// closing evicts after the final report
	device.updateContractStatsEvents([]*connect.ContractStatsEvent{
		{
			ContractId:        egressContractId,
			Receive:           false,
			Path:              connect.TransferPath{SourceId: us, DestinationId: peer1, StreamId: stream},
			TransferByteCount: 10000,
			UsedByteCount:     1000,
			Open:              false,
		},
	})
	if count := device.GetEgressContractDetails().Len(); count != 0 {
		t.Fatalf("expected closed contract evicted, got %d", count)
	}
}

func TestDeviceLocalDnsResolverSettings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	device, networkSpace := testing_newBlockDevice(ctx, t, false)
	defer device.Close()

	dnsListener := &testing_dnsListener{}
	sub := device.AddDnsResolverSettingsChangeListener(dnsListener)
	defer sub.Close()

	// the default mirrors the default upgrade mux resolver, with the fallback on
	dnsResolverSettings := device.GetDnsResolverSettings()
	if dnsResolverSettings == nil || !dnsResolverSettings.EnableRemoteDoh {
		t.Fatalf("unexpected default dns resolver settings %+v", dnsResolverSettings)
	}
	if !dnsResolverSettings.EnableFallback {
		t.Fatalf("expected the default fallback enabled %+v", dnsResolverSettings)
	}

	next := &DnsResolverSettings{
		EnableRemoteDoh:   true,
		RemoteDohUrlsIpv4: testing_stringList("https://9.9.9.9/dns-query"),
		EnableLocalDns:    true,
		LocalDnsIpv4:      testing_stringList("9.9.9.9"),
		EnableFallback:    true,
	}
	device.SetDnsResolverSettings(next)

	if count := dnsListener.fireCount(); count != 1 {
		t.Fatalf("expected 1 dns fire, got %d", count)
	}
	dnsResolverSettings = device.GetDnsResolverSettings()
	if !dnsResolverSettings.EnableRemoteDoh || !dnsResolverSettings.EnableLocalDns || dnsResolverSettings.EnableLocalDoh {
		t.Fatalf("unexpected dns resolver settings %+v", dnsResolverSettings)
	}
	if !dnsResolverSettings.LocalDnsIpv4.Contains("9.9.9.9") {
		t.Fatalf("unexpected local dns %+v", dnsResolverSettings.LocalDnsIpv4)
	}
	if !dnsResolverSettings.EnableFallback {
		t.Fatalf("expected the fallback enabled %+v", dnsResolverSettings)
	}

	// the fallback is the host-side projection of the resolver
	func() {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		fallback := device.upgradeMuxSettings.Dns.Fallback
		if !fallback.EnableLocalDoh || !fallback.EnableLocalDns {
			t.Fatalf("unexpected fallback %+v", fallback)
		}
		if len(fallback.LocalDohUrlsIpv4) != 1 || fallback.LocalDohUrlsIpv4[0] != "https://9.9.9.9/dns-query" {
			t.Fatalf("expected remote doh projected to host doh, got %+v", fallback.LocalDohUrlsIpv4)
		}
		if fallback.EnableRemoteDoh || fallback.EnableRemoteDns {
			t.Fatalf("the fallback must not use tunnel paths %+v", fallback)
		}
	}()

	// persisted to local state (async)
	localState := networkSpace.GetAsyncLocalState().GetLocalState()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if persisted := localState.GetDnsResolverSettings(); persisted != nil && persisted.EnableLocalDns {
			break
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("dns resolver settings were not persisted")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// a new device on the same network space restores the persisted settings
	device2 := testing_newBlockDeviceWithNetworkSpace(t, networkSpace, "", false)
	defer device2.Close()
	restored := device2.GetDnsResolverSettings()
	if !restored.EnableLocalDns || !restored.LocalDnsIpv4.Contains("9.9.9.9") {
		t.Fatalf("unexpected restored dns resolver settings %+v", restored)
	}
	if !restored.EnableFallback {
		t.Fatalf("expected the restored fallback enabled %+v", restored)
	}

	// disabling the fallback clears it from the mux settings
	next.EnableFallback = false
	device.SetDnsResolverSettings(next)
	if dnsResolverSettings := device.GetDnsResolverSettings(); dnsResolverSettings.EnableFallback {
		t.Fatalf("expected the fallback disabled %+v", dnsResolverSettings)
	}
	func() {
		device.stateLock.Lock()
		defer device.stateLock.Unlock()
		if device.upgradeMuxSettings.Dns.Fallback != nil {
			t.Fatalf("expected no fallback resolver %+v", device.upgradeMuxSettings.Dns.Fallback)
		}
	}()
}

func TestDeviceLocalContractStatsEmitGate(t *testing.T) {
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
	// gate everything except closes
	settings.ContractStatsEpoch = 1 * time.Hour
	device, err := newDeviceLocalWithOverrides(networkSpace, byJwt, "", "", "", NewId(), settings, connect.NewId())
	if err != nil {
		t.Fatalf("device: %v", err)
	}
	defer device.Close()

	egressStatsListener := &testing_contractStatsListener{}
	egressStatsSub := device.AddEgressContractStatsChangeListener(egressStatsListener)
	defer egressStatsSub.Close()

	us := connect.NewId()
	peer := connect.NewId()
	contractId := connect.NewId()
	openEvent := &connect.ContractStatsEvent{
		ContractId:        contractId,
		Path:              connect.TransferPath{SourceId: us, DestinationId: peer},
		TransferByteCount: 10000,
		UsedByteCount:     100,
		Open:              true,
	}

	// the first batch emits (the gate starts open)
	device.updateContractStatsEvents([]*connect.ContractStatsEvent{openEvent})
	if count := egressStatsListener.fireCount(); count != 1 {
		t.Fatalf("expected 1 fire, got %d", count)
	}

	// subsequent open batches inside the epoch are gated
	device.updateContractStatsEvents([]*connect.ContractStatsEvent{openEvent})
	device.updateContractStatsEvents([]*connect.ContractStatsEvent{openEvent})
	if count := egressStatsListener.fireCount(); count != 1 {
		t.Fatalf("expected the gate to hold, got %d", count)
	}

	// the state is still updated while gated
	if stats := device.GetEgressContractStats(); stats.ContractUsedByteCount != 100 {
		t.Fatalf("expected updated state, got %+v", stats)
	}

	// a close event always emits
	closeEvent := *openEvent
	closeEvent.Open = false
	device.updateContractStatsEvents([]*connect.ContractStatsEvent{&closeEvent})
	if count := egressStatsListener.fireCount(); count != 2 {
		t.Fatalf("expected the close to emit, got %d", count)
	}
}

func TestDeviceLocalProviderPacketStats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// with the provider disabled the getter is nil and the listener noops
	device, _ := testing_newBlockDevice(ctx, t, false)
	defer device.Close()
	if packetStats := device.GetProviderPacketStats(); packetStats != nil {
		t.Fatalf("expected nil provider packet stats without a provider")
	}
	noopListener := &testing_packetStatsListener{}
	noopSub := device.AddProviderPacketStatsChangeListener(noopListener)
	noopSub.Close()

	providerDevice, _ := testing_newBlockDevice(ctx, t, true)
	defer providerDevice.Close()

	// with a provider but no provided traffic the stats are zero
	packetStats := providerDevice.GetProviderPacketStats()
	if packetStats == nil || packetStats.RemoteEgressPacketCount != 0 {
		t.Fatalf("expected zero provider packet stats, got %+v", packetStats)
	}

	packetStatsListener := &testing_packetStatsListener{}
	packetStatsSub := providerDevice.AddProviderPacketStatsChangeListener(packetStatsListener)
	defer packetStatsSub.Close()

	// the client packet stats listener must not see provider events
	clientPacketStatsListener := &testing_packetStatsListener{}
	clientPacketStatsSub := providerDevice.AddPacketStatsChangeListener(clientPacketStatsListener)
	defer clientPacketStatsSub.Close()

	providerDevice.updateProviderPacketStats(&connect.PacketStats{
		RemoteEgressPacketCount:  10,
		RemoteEgressByteCount:    1000,
		RemoteIngressPacketCount: 20,
		RemoteIngressByteCount:   2000,
		BlockEgressPacketCount:   2,
		BlockEgressByteCount:     64,
		BlockIngressPacketCount:  3,
		BlockIngressByteCount:    96,
	})

	if count := packetStatsListener.fireCount(); count != 1 {
		t.Fatalf("expected 1 provider packet stats fire, got %d", count)
	}
	func() {
		packetStatsListener.stateLock.Lock()
		defer packetStatsListener.stateLock.Unlock()
		if packetStatsListener.packetStats.RemoteEgressByteCount != 1000 {
			t.Fatalf("unexpected provider packet stats %+v", packetStatsListener.packetStats)
		}
		if packetStatsListener.packetStats.RemoteIngressByteCount != 2000 {
			t.Fatalf("unexpected provider packet stats %+v", packetStatsListener.packetStats)
		}
		if packetStatsListener.packetStats.BlockEgressByteCount != 64 || packetStatsListener.packetStats.BlockIngressByteCount != 96 {
			t.Fatalf("unexpected provider packet stats %+v", packetStatsListener.packetStats)
		}
	}()
	if count := clientPacketStatsListener.fireCount(); count != 0 {
		t.Fatalf("provider packet stats must not fire the client listener, got %d", count)
	}

	// the fallback local route counts belong to the client stats only
	providerDevice.localFallbackEgressPacketCount.Add(5)
	providerDevice.localFallbackEgressByteCount.Add(500)
	if clientStats := providerDevice.GetPacketStats(); clientStats.LocalEgressPacketCount != 5 {
		t.Fatalf("expected the fallback route in the client stats, got %+v", clientStats)
	}
	if providerStats := providerDevice.GetProviderPacketStats(); providerStats.LocalEgressPacketCount != 0 {
		t.Fatalf("the fallback route must not leak into the provider stats %+v", providerStats)
	}

	// a provide enable/disable cycle folds the live counters into the base
	providerDevice.SetProvideMode(ProvideModePublic)
	if packetStats := providerDevice.GetProviderPacketStats(); packetStats == nil {
		t.Fatalf("expected provider packet stats while providing")
	}
	providerDevice.SetProvideMode(ProvideModeNone)
	if packetStats := providerDevice.GetProviderPacketStats(); packetStats == nil || packetStats.RemoteEgressPacketCount != 0 {
		t.Fatalf("expected zero folded provider packet stats, got %+v", packetStats)
	}
}

func TestDeviceLocalProviderContractStats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// with the provider disabled the getters are nil
	device, _ := testing_newBlockDevice(ctx, t, false)
	defer device.Close()
	if stats := device.GetProviderEgressContractStats(); stats != nil {
		t.Fatalf("expected nil provider egress stats without a provider")
	}
	if stats := device.GetProviderIngressContractStats(); stats != nil {
		t.Fatalf("expected nil provider ingress stats without a provider")
	}
	if details := device.GetProviderEgressContractDetails(); details != nil {
		t.Fatalf("expected nil provider egress details without a provider")
	}
	if details := device.GetProviderIngressContractDetails(); details != nil {
		t.Fatalf("expected nil provider ingress details without a provider")
	}

	providerDevice, _ := testing_newBlockDevice(ctx, t, true)
	defer providerDevice.Close()

	ingressStatsListener := &testing_contractStatsListener{}
	ingressStatsSub := providerDevice.AddProviderIngressContractStatsChangeListener(ingressStatsListener)
	defer ingressStatsSub.Close()

	ingressDetailsListener := &testing_contractDetailsListener{}
	ingressDetailsSub := providerDevice.AddProviderIngressContractDetailsChangeListener(ingressDetailsListener)
	defer ingressDetailsSub.Close()

	us := connect.NewId()
	peer := connect.NewId()
	stream := connect.NewId()

	// the remote client's egress contract is the provider's ingress contract,
	// and the provider's return traffic rides an egress companion contract
	ingressContractId := connect.NewId()
	companionContractId := connect.NewId()

	providerDevice.updateProviderContractStatsEvents([]*connect.ContractStatsEvent{
		{
			ContractId:         ingressContractId,
			Receive:            true,
			Path:               connect.TransferPath{SourceId: peer, DestinationId: us, StreamId: stream},
			TransferByteCount:  10000,
			UsedByteCount:      1000,
			UsedByteCountDelta: 1000,
			Open:               true,
		},
		{
			ContractId:         companionContractId,
			Receive:            false,
			Companion:          true,
			Path:               connect.TransferPath{SourceId: us, DestinationId: peer, StreamId: stream},
			TransferByteCount:  20000,
			UsedByteCount:      500,
			UsedByteCountDelta: 500,
			Open:               true,
		},
	})

	if count := ingressStatsListener.fireCount(); count != 1 {
		t.Fatalf("expected 1 provider ingress stats fire, got %d", count)
	}

	ingressStats := providerDevice.GetProviderIngressContractStats()
	if ingressStats.ContractUsedByteCount != 1000 || ingressStats.ContractByteCount != 10000 {
		t.Fatalf("unexpected provider ingress stats %+v", ingressStats)
	}
	if ingressStats.CompanionContractUsedByteCount != 500 {
		t.Fatalf("unexpected provider ingress companion stats %+v", ingressStats)
	}

	egressStats := providerDevice.GetProviderEgressContractStats()
	if egressStats.ContractUsedByteCount != 500 || egressStats.CompanionContractUsedByteCount != 1000 {
		t.Fatalf("unexpected provider egress stats %+v", egressStats)
	}

	// the ingress contract pairs with the exact reverse path companion
	ingressDetails := providerDevice.GetProviderIngressContractDetails()
	if ingressDetails.Len() != 1 {
		t.Fatalf("expected 1 provider ingress details, got %d", ingressDetails.Len())
	}
	details := ingressDetails.Get(0)
	if details.ContractId.Cmp(newId(ingressContractId)) != 0 {
		t.Fatalf("unexpected provider contract id")
	}
	if details.CompanionContractId == nil || details.CompanionContractId.Cmp(newId(companionContractId)) != 0 {
		t.Fatalf("expected the reverse path companion")
	}
	if count := ingressDetailsListener.fireCount(); count != 1 {
		t.Fatalf("expected 1 provider ingress details fire, got %d", count)
	}

	// the provider contracts are tracked apart from the client contracts
	if clientStats := providerDevice.GetIngressContractStats(); clientStats.ContractUsedByteCount != 0 {
		t.Fatalf("provider contracts must not leak into the client stats %+v", clientStats)
	}

	// closing evicts after the final report
	providerDevice.updateProviderContractStatsEvents([]*connect.ContractStatsEvent{
		{
			ContractId:        ingressContractId,
			Receive:           true,
			Path:              connect.TransferPath{SourceId: peer, DestinationId: us, StreamId: stream},
			TransferByteCount: 10000,
			UsedByteCount:     1000,
			Open:              false,
		},
		{
			ContractId:        companionContractId,
			Receive:           false,
			Companion:         true,
			Path:              connect.TransferPath{SourceId: us, DestinationId: peer, StreamId: stream},
			TransferByteCount: 20000,
			UsedByteCount:     500,
			Open:              false,
		},
	})
	if count := providerDevice.GetProviderIngressContractDetails().Len(); count != 0 {
		t.Fatalf("expected closed provider contracts evicted, got %d", count)
	}
}

func TestDeviceLocalNetworkPeers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// with the provider disabled there are no network peers
	device, _ := testing_newBlockDevice(ctx, t, false)
	defer device.Close()
	if networkPeers := device.GetNetworkPeers(); networkPeers != nil {
		t.Fatalf("expected nil network peers without a provider")
	}

	// with a provider, the (stub) peers are empty
	providerDevice, _ := testing_newBlockDevice(ctx, t, true)
	defer providerDevice.Close()
	networkPeers := providerDevice.GetNetworkPeers()
	if networkPeers == nil {
		t.Fatalf("expected network peers with a provider")
	}
	if networkPeers.Connected.Len() != 0 || networkPeers.DisconnectedCount != 0 {
		t.Fatalf("expected empty stub network peers")
	}
}

func TestDnsIgnoreHostValues(t *testing.T) {
	if n := len(dnsIgnoreHostValues(nil)); n != 0 {
		t.Fatalf("expected no values for nil settings, got %d", n)
	}

	settings := &DnsResolverSettings{
		EnableRemoteDoh: true,
		EnableRemoteDns: true,
	}
	remoteDohUrlsIpv4 := NewStringList()
	remoteDohUrlsIpv4.addAll("https://dns.google/dns-query", "https://1.1.1.1/dns-query")
	settings.RemoteDohUrlsIpv4 = remoteDohUrlsIpv4
	localDohUrlsIpv6 := NewStringList()
	localDohUrlsIpv6.addAll("https://[2606:4700:4700::1111]/dns-query")
	settings.LocalDohUrlsIpv6 = localDohUrlsIpv6
	remoteDnsIpv4 := NewStringList()
	// duplicate of the doh url host, deduped
	remoteDnsIpv4.addAll("9.9.9.9", "1.1.1.1")
	settings.RemoteDnsIpv4 = remoteDnsIpv4

	values := dnsIgnoreHostValues(settings)
	// doh urls contribute their host
	expected := []string{
		"dns.google",
		"1.1.1.1",
		"2606:4700:4700::1111",
		"9.9.9.9",
	}
	if !slices.Equal(expected, values) {
		t.Fatalf("expected %v, got %v", expected, values)
	}
}
