package sdk

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

func localFallbackStatsAck(t *testing.T, ordinal, optionBytes int) []byte {
	t.Helper()
	ip := &layers.IPv4{
		Version: 4, TTL: 64, Protocol: layers.IPProtocolTCP,
		SrcIP: net.IPv4(192, 0, 2, 10), DstIP: net.IPv4(198, 51, 100, 20),
	}
	tcp := &layers.TCP{
		SrcPort: layers.TCPPort(41000 + ordinal), DstPort: 443,
		Seq: uint32(100 + ordinal), Ack: 1, ACK: true, Window: 65535,
	}
	for i := 0; i < optionBytes; i++ {
		tcp.Options = append(tcp.Options, layers.TCPOption{OptionType: layers.TCPOptionKindNop})
	}
	if err := tcp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatal(err)
	}
	buffer := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buffer, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, ip, tcp); err != nil {
		t.Fatal(err)
	}
	packet := buffer.Bytes()
	if !connect.IsTcpAckOnlyPacket(packet) {
		t.Fatal("local fallback fixture did not build a pure ACK")
	}
	return packet
}

// A real public return callback parks the single NAT worker. Its one queue
// slot then secures exactly the first bounded chunk. This reaches the actual
// concrete local-fallback branch without a private NAT admission hook.
func TestDeviceLocalFallbackCountsAcceptedProtocolPrefixBytes(t *testing.T) {
	connect.MessagePoolReturn(connect.MessagePoolGet(256))
	connect.GetMessagePoolAggregateStats()
	baselineRoots := connect.MessagePoolOutstandingCount()
	baselineBytes := connect.MessagePoolOutstandingByteCount()
	previousViolation := connect.SetMessagePoolViolationHandler(func(err error, _ []byte) {
		t.Errorf("local fallback packet custody: %v", err)
	})
	defer connect.SetMessagePoolViolationHandler(previousViolation)
	var causes []string
	for _, testCase := range []struct {
		name   string
		count  int
		closed bool
		want   int
	}{
		{name: "partial", count: 64, want: 16},
		{name: "whole", count: 16, want: 16},
		{name: "closed", count: 64, closed: true, want: 0},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		settings := connect.DefaultLocalUserNatSettings()
		settings.Log = connect.NewNoopLogger()
		settings.SequenceBufferSize = 1
		settings.SendShardCount = 1
		settings.TcpBufferSettings.EnableOrphanRst = true
		settings.TcpBufferSettings.OrphanRstPerSecond = 0
		nat := connect.NewLocalUserNat(ctx, "local fallback prefix statistics", settings)
		entered, release := make(chan struct{}), make(chan struct{})
		var enterOnce, releaseOnce, cleanOnce sync.Once
		unpark := func() { releaseOnce.Do(func() { close(release) }) }
		remove := nat.AddReceivePacketCallback(func(_ connect.TransferPath, _ protocol.ProvideMode, _ *connect.IpPath, _ []byte) {
			enterOnce.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		})
		clean := func() {
			cleanOnce.Do(func() {
				unpark()
				cancel()
				join, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if err := nat.CloseAndWait(join); err != nil {
					t.Errorf("%s NAT did not join: %v", testCase.name, err)
				}
				remove()
			})
		}
		t.Cleanup(clean)
		seed := connect.MessagePoolCopy(localFallbackStatsAck(t, 1000, 0))
		if !nat.SendPacket(connect.SourceId(connect.NewId()), protocol.ProvideMode_Network, seed, 0) {
			connect.MessagePoolReturn(seed)
			clean()
			t.Fatal("could not enter the real NAT callback barrier")
		}
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			clean()
			t.Fatal("orphan ACK did not reach the public NAT return callback")
		}
		device := newPacketBatchTestDevice(nil)
		device.settings.SendTimeout = 0
		device.sendRoute.Store(&deviceLocalSendRoute{routeLocal: true, provider: &deviceLocalProvider{localUserNat: nat}})
		packets := make([][]byte, testCase.count)
		wantBytes := int64(0)
		for i := range packets {
			optionBytes := 4 * (i % 2)
			if i >= 16 {
				optionBytes += 8
			}
			packets[i] = localFallbackStatsAck(t, i, optionBytes)
			if i < testCase.want {
				wantBytes += int64(len(packets[i]))
			}
		}
		encoded := packetBatchTestEncode(packets)
		if testCase.closed {
			nat.Close()
		}
		accepted := device.SendPacketBatch(encoded)
		packetCount := device.localFallbackEgressPacketCount.Load()
		byteCount := device.localFallbackEgressByteCount.Load()
		clean()
		if accepted != int32(testCase.want) || packetCount != int64(testCase.want) {
			t.Fatalf("%s accepted=%d packets=%d want=%d", testCase.name, accepted, packetCount, testCase.want)
		}
		if roots, bytes := connect.MessagePoolOutstandingCount(), connect.MessagePoolOutstandingByteCount(); roots != baselineRoots || bytes != baselineBytes {
			t.Fatalf("%s retained roots after NAT join: roots=%d/%d bytes=%d/%d", testCase.name, roots, baselineRoots, bytes, baselineBytes)
		}
		if byteCount != wantBytes {
			causes = append(causes, fmt.Sprintf("%s accepted=%d bytes=%d want=%d", testCase.name, accepted, byteCount, wantBytes))
		}
		t.Logf("local fallback %s: accepted=%d packets=%d bytes=%d want_bytes=%d", testCase.name, accepted, packetCount, byteCount, wantBytes)
	}
	t.Log("[local-fallback-prefix-stats-cleanup] all NAT workers joined and pooled roots returned")
	if len(causes) != 0 {
		t.Errorf("LOCAL_FALLBACK_ACCEPTED_PREFIX_BYTES_UNCOUNTED: %s", strings.Join(causes, "; "))
	}
}
