//go:build !ios && !android && !js

package sdk

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/extender"
)

// The admission limits of the provider extender role (EXTENDER.md A12): the
// settings that reach its server, and the counts its status carries.

// The admission limits of the role's server: a zero limit keeps the connect
// default, a negative one disables it, and a positive one is the limit.
func TestDeviceLocalProviderExtenderAdmissionSettingsPassThrough(t *testing.T) {
	defaults := extender.DefaultExtenderSettings()
	if defaults.AdmissionSubnetsPerMinute <= 0 || defaults.AdmissionActionsPerSubnetPerMinute <= 0 {
		t.Fatalf(
			"connect defaults = %d subnets, %d actions a minute, expected both limits on",
			defaults.AdmissionSubnetsPerMinute,
			defaults.AdmissionActionsPerSubnetPerMinute,
		)
	}

	cases := []struct {
		name                   string
		subnetsPerMinute       int
		actionsPerMinute       int
		expectSubnetsPerMinute int
		expectActionsPerMinute int
	}{
		{
			name:                   "zero keeps the connect defaults",
			expectSubnetsPerMinute: defaults.AdmissionSubnetsPerMinute,
			expectActionsPerMinute: defaults.AdmissionActionsPerSubnetPerMinute,
		},
		{
			name:                   "a positive value is the limit",
			subnetsPerMinute:       50,
			actionsPerMinute:       3,
			expectSubnetsPerMinute: 50,
			expectActionsPerMinute: 3,
		},
		{
			name:                   "a negative value disables the limit",
			subnetsPerMinute:       -1,
			actionsPerMinute:       -1,
			expectSubnetsPerMinute: 0,
			expectActionsPerMinute: 0,
		},
		{
			name:                   "each limit is its own",
			subnetsPerMinute:       -1,
			actionsPerMinute:       2,
			expectSubnetsPerMinute: 0,
			expectActionsPerMinute: 2,
		},
	}
	for _, c := range cases {
		role := &deviceLocalExtender{
			settings: &deviceLocalExtenderSettings{
				NetworkSpace:                       &NetworkSpace{},
				AdmissionSubnetsPerMinute:          c.subnetsPerMinute,
				AdmissionActionsPerSubnetPerMinute: c.actionsPerMinute,
			},
		}
		settings := role.serverSettings(false)
		if settings.AdmissionSubnetsPerMinute != c.expectSubnetsPerMinute ||
			settings.AdmissionActionsPerSubnetPerMinute != c.expectActionsPerMinute {
			t.Errorf(
				"%s: %d subnets, %d actions a minute, expected %d and %d",
				c.name,
				settings.AdmissionSubnetsPerMinute,
				settings.AdmissionActionsPerSubnetPerMinute,
				c.expectSubnetsPerMinute,
				c.expectActionsPerMinute,
			)
		}
		// the rest of the admission is the connect default
		if settings.AdmissionRefusalsPerSubnetPerMinute != defaults.AdmissionRefusalsPerSubnetPerMinute ||
			settings.AdmissionRetryAfterMin != defaults.AdmissionRetryAfterMin ||
			settings.AdmissionRetryAfterMax != defaults.AdmissionRetryAfterMax {
			t.Errorf("%s: the rest of the admission is not the connect default", c.name)
		}
		if 0 < len(settings.AdmissionUnlimitedSources) {
			t.Errorf("%s: unlimited sources = %v, expected none", c.name, settings.AdmissionUnlimitedSources)
		}
		// the role's own settings are read, never written
		if role.settings.AdmissionSubnetsPerMinute != c.subnetsPerMinute ||
			role.settings.AdmissionActionsPerSubnetPerMinute != c.actionsPerMinute {
			t.Errorf("%s: the role's own settings were written", c.name)
		}
	}
}

// The unlimited sources reach the role's server as its own copy: a later edit
// of the role's list does not reach a server built from it, and the server's
// list is not the role's.
func TestDeviceLocalProviderExtenderAdmissionUnlimitedSourcesAreCopied(t *testing.T) {
	unlimitedSources := []netip.Prefix{
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("2001:db8:1::/48"),
	}
	role := &deviceLocalExtender{
		settings: &deviceLocalExtenderSettings{
			NetworkSpace:              &NetworkSpace{},
			AdmissionUnlimitedSources: unlimitedSources,
		},
	}
	settings := role.serverSettings(false)
	connect.AssertEqual(t, settings.AdmissionUnlimitedSources, []netip.Prefix{
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("2001:db8:1::/48"),
	})
	unlimitedSources[0] = netip.MustParsePrefix("198.51.100.0/24")
	connect.AssertEqual(t, settings.AdmissionUnlimitedSources[0], netip.MustParsePrefix("192.0.2.0/24"))
	settings.AdmissionUnlimitedSources[1] = netip.MustParsePrefix("2001:db8:2::/48")
	connect.AssertEqual(t, role.settings.AdmissionUnlimitedSources[1], netip.MustParsePrefix("2001:db8:1::/48"))
}

// What the admission limits turned away reaches the status an app renders
// (A12, F3). With one action a minute from a subnet, the second dial from
// loopback is answered 429 with a Retry-After in the default range, and the
// next status carries it as a per-subnet refusal.
func TestDeviceLocalProviderExtenderReportsAdmissionLimits(t *testing.T) {
	fixture := newTestProvideExtenderFixture(t, func(settings *deviceLocalExtenderSettings) {
		settings.AdmissionActionsPerSubnetPerMinute = 1
	})
	fixture.waitStatus("listening", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening
	})
	if status := fixture.device.GetExtenderProvideStatus(); status.LimitedBySubnetsCount != 0 ||
		status.LimitedBySourceCount != 0 {
		t.Fatalf("status = %+v before any dial, expected no admission refusals", status)
	}

	dial := func() error {
		connectSettings := connect.DefaultConnectSettings()
		connectSettings.ConnectTimeout = 10 * time.Second
		connectSettings.TlsTimeout = 10 * time.Second
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, _, err := connect.DialExtender(
			ctx,
			connectSettings,
			&connect.ExtenderConfig{
				Profile: connect.ExtenderProfile{
					ConnectMode: connect.ExtenderConnectModeTcpTls,
					ServerName:  "front.example",
					Port:        fixture.tcpPort,
				},
				Ip:        netip.MustParseAddr("127.0.0.1"),
				PublicKey: fixture.extender().publicKey,
			},
			&connect.ExtenderDial{
				// never allowed, so the admitted dial is refused before
				// anything is resolved or dialed
				DestinationHost: "blocked.example",
				DestinationPort: 443,
			},
		)
		if err == nil {
			conn.Close()
		}
		return err
	}

	// the first action takes the subnet's one token: admitted, then refused
	// for its destination
	var refusedErr *connect.ExtenderRefusedError
	if err := dial(); !errors.As(err, &refusedErr) {
		t.Fatalf("first dial err = %v, expected the destination refusal", err)
	}
	// the second is over the limit
	var limitedErr *connect.ExtenderLimitedError
	if err := dial(); !errors.As(err, &limitedErr) {
		t.Fatalf("second dial err = %v, expected the admission limit", err)
	}
	defaults := extender.DefaultExtenderSettings()
	if limitedErr.RetryAfter < defaults.AdmissionRetryAfterMin || defaults.AdmissionRetryAfterMax < limitedErr.RetryAfter {
		t.Fatalf(
			"retry after = %s, expected within [%s, %s]",
			limitedErr.RetryAfter,
			defaults.AdmissionRetryAfterMin,
			defaults.AdmissionRetryAfterMax,
		)
	}
	connect.AssertEqual(t, fixture.extender().server.AdmissionStats(), extender.ExtenderAdmissionStats{
		LimitedBySourceCount: 1,
	})

	// a refusal alone wakes nothing; the next pass reads the count
	fixture.wake()
	status := fixture.waitStatus("the admission refusal", func(status *ExtenderProvideStatus) bool {
		return 0 < status.LimitedBySourceCount
	})
	connect.AssertEqual(t, status.LimitedBySourceCount, 1)
	connect.AssertEqual(t, status.LimitedBySubnetsCount, 0)
	connect.AssertEqual(t, fixture.device.GetExtenderProvideStatus().LimitedBySourceCount, 1)

	// a restarted role is a fresh server, counting from zero
	fixture.device.SetProvideExtender(false)
	fixture.device.SetProvideExtender(true)
	restarted := fixture.waitStatus("the restarted role", func(status *ExtenderProvideStatus) bool {
		return status.Enabled && status.Listening && status.LimitedBySourceCount == 0
	})
	connect.AssertEqual(t, restarted.LimitedBySubnetsCount, 0)
	connect.AssertEqual(t, fixture.extender().server.AdmissionStats(), extender.ExtenderAdmissionStats{})
}
