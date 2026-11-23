package sdk

import (
	"context"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A settings change that replaces the space's network client (K6) while that
// client's feed is dialing a verified extender leaves the extender measurable.
// This is the order TestDeviceLocalProbeAttestorFollowsTheProvideMode met under
// load (tip-check at sidecar 023a929f, about 1 run in 120 to 200 beside a busy
// package here): the first client's loop reached its sample only after the
// record was applied, so its feed was dialing 192.0.2.11 when the replacement
// closed it. The close recorded its own ended dial as the extender's failure
// and held it for ten minutes, so the replacement's probe pass skipped it and
// the directory offered it nowhere: "never measured [192.0.2.11 192.0.2.91],
// measured map[192.0.2.91:true]". Holding the first client's dial open until
// the replacement forces that order.
func TestNetworkSpaceExtenderRestartKeepsTheExtenderItWasDialing(t *testing.T) {
	// the feed dials of the extender wait for their owner to end them; the
	// first one announces itself
	dialing := make(chan struct{})
	var dialingOnce sync.Once
	previous := newNetworkSpaceConnectSettings
	newNetworkSpaceConnectSettings = func() *connect.ConnectSettings {
		connectSettings := testOfflineConnectSettings()
		loopback := connectSettings.DialContextSettings
		connectSettings.DialContextSettings = &connect.DialContextSettings{
			DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
				if host, _, err := net.SplitHostPort(address); err == nil && host == "192.0.2.11" {
					dialingOnce.Do(func() { close(dialing) })
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return loopback.DialContext(ctx, network, address)
			},
			PacketConnFactory: loopback.PacketConnFactory,
		}
		return connectSettings
	}
	t.Cleanup(func() {
		newNetworkSpaceConnectSettings = previous
	})

	networkSpace, rootPrivateKey, _ := testProbeAttestorSpace(t)
	testApplyProbeAttestorRecord(t, networkSpace, rootPrivateKey, "192.0.2.11")
	// the running client samples now rather than at the end of its backoff
	networkSpace.getExtenderNetworkClient().Wake()
	select {
	case <-dialing:
	case <-time.After(30 * time.Second):
		t.Fatal("the space's network client never dialed the extender's feed")
	}
	// the restart closes the dialing client and joins it before it builds the
	// replacement, so whatever the ended dial recorded has landed by now
	testReplaceProbeAttestorClient(t, networkSpace, "192.0.2.91")
	for _, entry := range networkSpace.extenderDirectory.Snapshot().Entries {
		if entry.Ip.String() == "192.0.2.11" && entry.FailureCount != 0 {
			t.Fatalf("the restart held the old client's ended dial against the extender: failures=%d", entry.FailureCount)
		}
	}
	if !slices.ContainsFunc(networkSpace.extenderDirectory.Candidates(4, 64), func(candidate *connect.ExtenderCandidate) bool {
		return candidate.Ip.String() == "192.0.2.11"
	}) {
		t.Fatal("the directory no longer offers the extender the old client was dialing")
	}
	testWaitForProbeAttestorSamples(t, networkSpace, "192.0.2.11", "192.0.2.91")
}
