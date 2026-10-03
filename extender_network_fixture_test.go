// The live sdk extender fixtures must finish discovery without using host
// networking, including operator hints, signed dns records and feed sampling.
package sdk

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Keeps configuration and lifecycle tests in process while the real refresh
// loop applies manual addresses. No carrier family is available to sample.
func testConfigureInProcessExtenderNetworkClient(settings *connect.ExtenderNetworkClientSettings) {
	settings.ResolveDns = func(ctx context.Context, name string) ([]netip.Addr, error) {
		return nil, nil
	}
	settings.ResolveDnsTxt = func(ctx context.Context, name string) ([]string, error) {
		return nil, nil
	}
	settings.Hello = func(ctx context.Context) (*connect.ExtenderHelloResult, error) {
		return &connect.ExtenderHelloResult{}, nil
	}
	settings.Hint = func(ctx context.Context) (string, error) {
		return "", nil
	}
	settings.IpVersionSupported = func(ipVersion int) bool { return false }
	settings.ProbeWindowCount = 0
}

// Manual-host settings exercise the real refresh loop with every external
// dial rejected, so a missing fixture seam fails at the call that escaped.
func TestExtenderManualHostsNetworkStaysInProcess(t *testing.T) {
	testEnableExtenderManualHostsNetwork(t)
	testExtenderNetworkClientStaysInProcess(t)
}

// Url-only spaces use the same isolation contract as managed spaces.
func TestUrlSpaceExtenderNetworkStaysInProcess(t *testing.T) {
	testEnableUrlSpaceExtenderNetwork(t)
	testExtenderNetworkClientStaysInProcess(t)
}

// Runs with and without an operator url: the latter reaches dns even when
// the operator hook is missing. A manual address also exercises feed selection.
func testExtenderNetworkClientStaysInProcess(t *testing.T) {
	t.Helper()
	for _, apiUrl := range []string{"https://api.space.example", ""} {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			networkOperations := make(chan string, 1)
			rejectNetwork := func(operation string) error {
				select {
				case networkOperations <- operation:
				default:
				}
				cancel()
				return fmt.Errorf("unexpected %s in extender fixture", operation)
			}

			strategySettings := connect.DefaultClientStrategySettings()
			strategySettings.EnableResilient = false
			// With no DoH servers or discovery directory, all fallbacks reach
			// these rejecting stream, packet and system-resolver boundaries.
			strategySettings.DohSettings = nil
			strategySettings.ConnectSettings.Resolver = &net.Resolver{
				PreferGo: true,
				Dial: func(ctx context.Context, network string, address string) (net.Conn, error) {
					return nil, rejectNetwork("DNS lookup")
				},
			}
			strategySettings.ConnectSettings.DialContextSettings = &connect.DialContextSettings{
				DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
					return nil, rejectNetwork("stream dial")
				},
				PacketConnFactory: func(ctx context.Context) (net.PacketConn, error) {
					return nil, rejectNetwork("packet socket")
				},
			}
			strategy := connect.NewClientStrategy(ctx, strategySettings)
			defer strategy.Close()
			directory := connect.NewExtenderDirectoryWithDefaults(ctx)
			defer directory.Close()
			settings := spaceExtenderNetworkClientSettings(
				NewNetworkSpaceKey("space.example", "main"),
				&NetworkSpaceValues{ExtenderHosts: []string{"192.0.2.1"}},
				ExtenderRoleMember,
				apiUrl,
				nil,
			)
			if settings.IpVersionSupported == nil {
				// A missing fixture override represents a host with IPv4. Do
				// not let this regression depend on the host's family probe.
				settings.IpVersionSupported = func(ipVersion int) bool { return ipVersion == 4 }
			}
			client := connect.NewExtenderNetworkClient(ctx, strategy, directory, settings)
			defer client.Close()

		waitForInitialAttempt:
			for {
				status, update := client.StatusMonitor().Get()
				if status.InitialAttemptDone {
					break
				}
				select {
				case <-update:
				case <-ctx.Done():
					break waitForInitialAttempt
				}
			}
			client.Close()
			select {
			case operation := <-networkOperations:
				t.Errorf("api URL %q: fixture attempted %s before completing discovery", apiUrl, operation)
				return
			default:
			}
			if !client.Status().InitialAttemptDone {
				t.Errorf("api URL %q: fixture never completed its initial attempt", apiUrl)
				return
			}
			entries := directory.Snapshot().Entries
			if len(entries) != 1 || entries[0].Ip != netip.MustParseAddr("192.0.2.1") ||
				entries[0].Source != connect.ExtenderSourceManual {
				t.Errorf("api URL %q: discovery did not apply the manual address: %+v", apiUrl, entries)
			}
		}()
	}
}
