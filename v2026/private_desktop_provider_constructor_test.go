// Desktop provider source receipts distinguish unparented queues from mobile retained owners.
package sdk

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Values from production factories are checked against a real DeviceLocal's
// owner topology. Protocol settings are configuration, not observed wire credit.
type privateDesktopProviderContract struct {
	Schema                      string    `json:"schema"`
	Profile                     string    `json:"profile"`
	SettingsSource              string    `json:"settings_source"`
	DeviceTarget                ByteCount `json:"device_target"`
	ProcessMemoryTarget         ByteCount `json:"process_memory_target"`
	LowMemoryProfile            bool      `json:"low_memory_profile"`
	RootPresent                 bool      `json:"root_present"`
	ProviderParentPresent       bool      `json:"provider_parent_present"`
	ProviderShare               ByteCount `json:"provider_share"`
	SendTotal                   ByteCount `json:"send_total"`
	ReceiveTotal                ByteCount `json:"receive_total"`
	SendParentPresent           bool      `json:"send_parent_present"`
	ReceiveParentPresent        bool      `json:"receive_parent_present"`
	ResendQueueMax              ByteCount `json:"resend_queue_max"`
	ResendQueueMin              ByteCount `json:"resend_queue_min"`
	ReceiveQueueMax             ByteCount `json:"receive_queue_max"`
	ReceiveQueueMin             ByteCount `json:"receive_queue_min"`
	ReceiveAdvertisement        ByteCount `json:"receive_advertisement"`
	AdvertiseReceiveWindow      bool      `json:"advertise_receive_window"`
	SendRetained                bool      `json:"send_retained"`
	ReceiveRetained             bool      `json:"receive_retained"`
	PackQueuePresent            bool      `json:"pack_queue_present"`
	PackQueueTotal              ByteCount `json:"pack_queue_total"`
	PackRetained                bool      `json:"pack_retained"`
	SendSequenceBufferSize      int       `json:"send_sequence_buffer_size"`
	ReceiveSequenceBufferSize   int       `json:"receive_sequence_buffer_size"`
	H1ReceiveSequenceBufferSize int       `json:"h1_receive_sequence_buffer_size"`
	SendWindowSizing            int       `json:"send_window_sizing"`
	ReceiveWindowSizing         int       `json:"receive_window_sizing"`
	P2pTotal                    ByteCount `json:"p2p_total"`
	P2pParentPresent            bool      `json:"p2p_parent_present"`
	NetworkPeerTotal            ByteCount `json:"network_peer_total"`
	NetworkPeerParentPresent    bool      `json:"network_peer_parent_present"`
	TransportTotal              ByteCount `json:"transport_total"`
	TransportMaxCount           int       `json:"transport_max_count"`
	H1BaseByteCount             ByteCount `json:"h1_base_byte_count"`
	H1ReservationByteCount      ByteCount `json:"h1_reservation_byte_count"`
	H1MaximumMessageByteCount   int64     `json:"h1_maximum_message_byte_count"`
	TransportBufferSize         int       `json:"transport_buffer_size"`
	H1AckPriorityBufferSize     int       `json:"h1_ack_priority_buffer_size"`
	NatBudgetPresent            bool      `json:"nat_budget_present"`
	NatTcpBudgetPresent         bool      `json:"nat_tcp_budget_present"`
	NatReturnTotal              ByteCount `json:"nat_return_total"`
	NatTcpUserLimit             int       `json:"nat_tcp_user_limit"`
	NatTcpGlobalLimit           int       `json:"nat_tcp_global_limit"`
	NatUdpUserLimit             int       `json:"nat_udp_user_limit"`
	NatUdpGlobalLimit           int       `json:"nat_udp_global_limit"`
	NatIcmpUserLimit            int       `json:"nat_icmp_user_limit"`
	NatIcmpGlobalLimit          int       `json:"nat_icmp_global_limit"`
	NatMtu                      int       `json:"nat_mtu"`
	NatTcpReadBufferByteCount   int       `json:"nat_tcp_read_buffer_byte_count"`
	NatTcpSequenceBufferSize    int       `json:"nat_tcp_sequence_buffer_size"`
	NatTcpWriteBatchSize        int       `json:"nat_tcp_write_batch_size"`
	NatTcpMaxWindowSize         uint32    `json:"nat_tcp_max_window_size"`
}

// All three process-sizing contexts retain the same desktop device target.
// Linux's base daemon uses 384 MiB; 32 MiB explicitly represents a shared
// synthetic consumer epoch, and zero leaves Connect defaults unscaled.
func TestPrivateDesktopProvider128ConstructorContract(t *testing.T) {
	previousBudget := connect.MemoryBudget()
	t.Cleanup(func() { connect.SetMemoryBudget(previousBudget) })
	for _, row := range []struct {
		name                                                string
		processTarget, resendMax, receiveMax, advertisement ByteCount
	}{
		{name: "unscaled-process", processTarget: 0, resendMax: 2097152, receiveMax: 2621440, advertisement: 2621440},
		{name: "shared-process32", processTarget: 32 * 1024 * 1024, resendMax: 1048576, receiveMax: 4194304, advertisement: 4194304},
		{name: "linux-process384", processTarget: 384 * 1024 * 1024, resendMax: 2097152, receiveMax: 50331648, advertisement: 7669584},
	} {
		func() {
			connect.SetMemoryBudget(row.processTarget)
			settings := defaultDeviceLocalSettingsForPlatform("linux")
			settings.MemoryTargetByteCount = 128 * 1024 * 1024
			settings.DisableLogging = true
			if settings.LowMemoryProfile {
				t.Fatalf("%s desktop defaults selected low policy", row.name)
			}
			auth := testingAuthClientShapeSpace(t)
			device := newSettingsOwnershipDevice(t, auth, settings, false)
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := device.CloseAndWait(ctx); err != nil {
					t.Error(err)
				}
				if device.provider != nil {
					send, receive := device.provider.transferBudgets()
					for _, budget := range []*connect.TransferMemoryBudget{send, receive} {
						stats := budget.Stats()
						if stats.UsedByteCount != 0 || stats.ReservedByteCount != stats.ReleasedByteCount {
							t.Errorf("%s retained provider claim after join: %+v", row.name, stats)
						}
					}
				}
			}()
			if device.provider == nil {
				t.Fatalf("%s missing real provider owner", row.name)
			}
			device.provider.platformTransport.Close()
			if device.settings.LowMemoryProfile || device.transferMemory != nil {
				t.Fatalf("%s desktop acquired mobile hierarchy", row.name)
			}
			func() {
				device.stateLock.Lock()
				defer device.stateLock.Unlock()
				device.applyProvideMemorySharesWithLock(true)
			}()
			actualSend, actualReceive := device.provider.transferBudgets()
			if actualSend == nil || actualReceive == nil || actualSend.Parent() != nil || actualReceive.Parent() != nil ||
				actualSend.TotalByteCount() != 5752188 || actualReceive.TotalByteCount() != 7669584 {
				t.Fatalf("%s real desktop provider lost independent 3:4 budget pair", row.name)
			}

			// These are the same nested factories called by newDeviceLocalProviderWithOverrides.
			// No public getter exposes the Connect client's nested settings; do not call
			// this projection an observed wire advertisement or a remote provider receipt.
			_, _, _, providerShare := deviceMemoryShares(device.settings)
			provider := newDeviceClientSettings(&device.settings.ClientSettings, "", nil)
			applyMobileH1PerformanceClientSettingsForPlatform(provider, device.settings.MemoryTargetByteCount, false, true)
			provider.ProviderStreamPolicy = true
			send, receive := configureDeviceLocalProviderMemory(provider, providerShare)
			transmit, accept := provider.SendBufferSettings, provider.ReceiveBufferSettings
			if providerShare != 26843545 || send.TotalByteCount() != actualSend.TotalByteCount() || receive.TotalByteCount() != actualReceive.TotalByteCount() ||
				send.Parent() != nil || receive.Parent() != nil || transmit.ResendQueueRetainedByteAccounting || accept.ReceiveQueueRetainedByteAccounting ||
				accept.PackQueueBudget != nil || accept.PackQueueRetainedByteAccounting || transmit.ResendQueueMaxByteCount != row.resendMax ||
				transmit.ResendQueueMinByteCount != 262144 || accept.ReceiveQueueMaxByteCount != row.receiveMax || accept.ReceiveQueueMinByteCount != 327680 ||
				!accept.AdvertiseReceiveWindow || min(accept.ReceiveQueueMaxByteCount, receive.TotalByteCount()) != row.advertisement {
				t.Fatalf("%s desktop protocol projection changed", row.name)
			}
			if provider.WebRtcSettings.MemoryBudget == nil || provider.WebRtcSettings.MemoryBudget.TotalByteCount() != 3355443 || provider.WebRtcSettings.MemoryBudget.Parent() != nil ||
				provider.WebRtcSettings.NetworkPeerMemoryBudget == nil || provider.WebRtcSettings.NetworkPeerMemoryBudget.TotalByteCount() != 1048576 || provider.WebRtcSettings.NetworkPeerMemoryBudget.Parent() != nil {
				t.Fatalf("%s desktop P2P owners changed", row.name)
			}
			platform := device.provider.platformTransportSettings
			platformStats := device.platformTransportBudget.Stats()
			nat := providerLocalUserNatSettings(providerShare, device.settings.ClientSettings.Log)
			if platform == nil || platform.PlatformTransportBudget != device.platformTransportBudget || platformStats.TotalByteCount != 33554432 ||
				nat.MemoryBudget != nil || nat.TcpBufferSettings.MemoryBudget != nil || nat.TcpBufferSettings.ReturnQueueBudget == nil ||
				nat.TcpBufferSettings.UserLimit != 327 || nat.TcpBufferSettings.GlobalLimit != 655 || nat.UdpBufferSettings.UserLimit != 983 || nat.UdpBufferSettings.GlobalLimit != 3932 ||
				nat.IcmpBufferSettings.UserLimit != 64 || nat.IcmpBufferSettings.GlobalLimit != 128 {
				t.Fatalf("%s desktop carrier or independent NAT settings changed", row.name)
			}
			record := privateDesktopProviderContract{
				Schema: "private-sdk-desktop-provider-v1", Profile: row.name, SettingsSource: "production-factories-with-real-device-owner-check",
				DeviceTarget: device.settings.MemoryTargetByteCount, ProcessMemoryTarget: connect.MemoryBudget(), LowMemoryProfile: device.settings.LowMemoryProfile,
				RootPresent: device.transferMemory != nil, ProviderParentPresent: actualSend.Parent() != nil, ProviderShare: providerShare,
				SendTotal: actualSend.TotalByteCount(), ReceiveTotal: actualReceive.TotalByteCount(), SendParentPresent: actualSend.Parent() != nil, ReceiveParentPresent: actualReceive.Parent() != nil,
				ResendQueueMax: transmit.ResendQueueMaxByteCount, ResendQueueMin: transmit.ResendQueueMinByteCount,
				ReceiveQueueMax: accept.ReceiveQueueMaxByteCount, ReceiveQueueMin: accept.ReceiveQueueMinByteCount, ReceiveAdvertisement: min(accept.ReceiveQueueMaxByteCount, actualReceive.TotalByteCount()),
				AdvertiseReceiveWindow: accept.AdvertiseReceiveWindow, SendRetained: transmit.ResendQueueRetainedByteAccounting, ReceiveRetained: accept.ReceiveQueueRetainedByteAccounting,
				PackQueuePresent: accept.PackQueueBudget != nil, PackRetained: accept.PackQueueRetainedByteAccounting,
				SendSequenceBufferSize: transmit.SequenceBufferSize, ReceiveSequenceBufferSize: accept.SequenceBufferSize, H1ReceiveSequenceBufferSize: accept.H1SequenceBufferSize,
				SendWindowSizing: int(transmit.WindowSizing), ReceiveWindowSizing: int(accept.WindowSizing),
				P2pTotal: provider.WebRtcSettings.MemoryBudget.TotalByteCount(), P2pParentPresent: provider.WebRtcSettings.MemoryBudget.Parent() != nil,
				NetworkPeerTotal: provider.WebRtcSettings.NetworkPeerMemoryBudget.TotalByteCount(), NetworkPeerParentPresent: provider.WebRtcSettings.NetworkPeerMemoryBudget.Parent() != nil,
				TransportTotal: platformStats.TotalByteCount, TransportMaxCount: platformStats.MaxTransportCount,
				H1BaseByteCount: platform.H1BudgetByteCount, H1ReservationByteCount: platform.H1MemoryReservationByteCount(), H1MaximumMessageByteCount: platform.H1MaxMessageByteCount,
				TransportBufferSize: platform.TransportBufferSize, H1AckPriorityBufferSize: platform.H1AckPriorityBufferSize,
				NatBudgetPresent: nat.MemoryBudget != nil, NatTcpBudgetPresent: nat.TcpBufferSettings.MemoryBudget != nil, NatReturnTotal: nat.TcpBufferSettings.ReturnQueueBudget.TotalByteCount(),
				NatTcpUserLimit: nat.TcpBufferSettings.UserLimit, NatTcpGlobalLimit: nat.TcpBufferSettings.GlobalLimit, NatUdpUserLimit: nat.UdpBufferSettings.UserLimit, NatUdpGlobalLimit: nat.UdpBufferSettings.GlobalLimit,
				NatIcmpUserLimit: nat.IcmpBufferSettings.UserLimit, NatIcmpGlobalLimit: nat.IcmpBufferSettings.GlobalLimit, NatMtu: nat.TcpBufferSettings.Mtu,
				NatTcpReadBufferByteCount: nat.TcpBufferSettings.ReadBufferByteCount, NatTcpSequenceBufferSize: nat.TcpBufferSettings.SequenceBufferSize,
				NatTcpWriteBatchSize: nat.TcpBufferSettings.WriteBatchSize, NatTcpMaxWindowSize: nat.TcpBufferSettings.MaxWindowSize,
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("[private-desktop-provider-constructor] %s", encoded)
		}()
	}
}
