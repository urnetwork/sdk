package sdk

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// Both peers reply to their TCP SYNs before either handshake can finish. The
// chosen family completes first; the other's SYN-ACKs remain held until the
// winner has returned and carried application data. DNS and launch ordering
// cannot decide the expected winner, and the deadline is only a watchdog.
func TestSocketNamedTCPSelectsFirstCompletedFamily(t *testing.T) {
	for _, winnerVersion := range []int{4, 6} {
		t.Run("ipv"+strconv.Itoa(winnerVersion), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var mu sync.Mutex
			pending := make(map[int][]func())
			seen := make(map[int]bool)
			released := make(map[int]bool)
			n := newSocketTestNetworkWithPacketReply(t, func(path *connect.IpPath, reply func()) {
				if path == nil || path.Protocol != connect.IpProtocolTcp || path.SourcePort != 8443 || !path.Syn || !path.Ack {
					reply()
					return
				}
				mu.Lock()
				if released[path.Version] {
					mu.Unlock()
					reply()
					return
				}
				pending[path.Version] = append(pending[path.Version], reply)
				seen[path.Version] = true
				var completeWinner []func()
				if seen[4] && seen[6] {
					released[winnerVersion] = true
					completeWinner, pending[winnerVersion] = pending[winnerVersion], nil
				}
				mu.Unlock()
				for _, complete := range completeWinner {
					complete()
				}
			})
			peers := map[int]string{
				4: n.echo(t, "tcp4", 8443),
				6: n.echo(t, "tcp6", 8443),
			}
			conn, err := n.device.DialContext(ctx, "tcp", "socket.test:8443")
			if err != nil {
				t.Fatalf("first completed IPv%d handshake did not win: %v", winnerVersion, err)
			}
			defer conn.Close()
			if got := conn.RemoteAddr().String(); got != peers[winnerVersion] {
				t.Fatalf("winner=%s, want first completed peer %s", got, peers[winnerVersion])
			}
			socketRoundTrip(t, conn, []byte("before losing handshake"), false)
			loserVersion := 4
			if winnerVersion == 4 {
				loserVersion = 6
			}
			mu.Lock()
			completeLoser := pending[loserVersion]
			pending[loserVersion] = nil
			released[loserVersion] = true
			mu.Unlock()
			if len(completeLoser) == 0 {
				t.Fatal("dial completed without the other family attempting a handshake")
			}
			for _, complete := range completeLoser {
				complete()
			}
			socketRoundTrip(t, conn, []byte("after losing handshake"), false)
			if got := conn.RemoteAddr().String(); got != peers[winnerVersion] {
				t.Fatalf("late handshake changed winner to %s", got)
			}
		})
	}
}
