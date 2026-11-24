//go:build !ios_extension && !js

package sdk

// The local Sidecar's zone watch (D-TRUST-9, QD-1384; the core's dns_watch_component.go): on a host class that stays
// up, desktop's, the composition adds the core's zone watch beside its dns component, as ur daemon does, and an
// enrolled role's first generation composes the watch over the resolvers its settings name, which closing the
// Sidecar releases; the foreground-only classes, iOS and Android, add none, as they run no CT watch. Every name is
// synthetic, and no query leaves the test.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/sidecar"
)

// A resolver that answers nothing, for a watch whose passes these tests never run.
type testSidecarSilentForwarder struct{}

func (testSidecarSilentForwarder) Forward(ctx context.Context, qType dnsmessage.Type, domain string) ([]byte, bool) {
	return nil, false
}

// The composition adds the zone watch on the desktop class alone: a component of its name does not add there, and
// does on iOS and Android.
func TestNewSidecarLocalAddsTheZoneWatchWhereTheHostStaysUp(t *testing.T) {
	for _, c := range []struct {
		class   string
		watched bool
	}{
		{class: SidecarHostClassDesktop, watched: true},
		{class: SidecarHostClassIos, watched: false},
		{class: SidecarHostClassAndroid, watched: false},
	} {
		world := newTestSidecarWorld(t)
		settings := DefaultSidecarSettings()
		settings.HostClass = c.class
		local := world.openWith(t, settings, &sidecarLocalOptions{
			searchFreeByteCount: func(dir string) (uint64, error) { return 8 * 1024 * 1024 * 1024, nil },
		})
		remove, err := local.Core().AddComponent(&testSidecarComponent{name: "dns-watch", closed: make(chan struct{})})
		if err == nil {
			remove()
		}
		if added := err != nil; added != c.watched {
			t.Errorf("%s: the composition added the zone watch %t, want %t", c.class, added, c.watched)
		}
	}
}

// Enrolled as EnrollWithDevice enrolls, a desktop role's first generation composes the zone watch over the resolvers
// its settings name, once, before the app's component attaches, and closing the Sidecar releases them; an iOS role
// builds none.
func TestDesktopRoleComposesTheZoneWatchAtItsFirstGeneration(t *testing.T) {
	for _, c := range []struct {
		class  string
		builds int32
	}{
		{class: SidecarHostClassDesktop, builds: 1},
		{class: SidecarHostClassIos, builds: 0},
	} {
		role := newTestT4Role(t, c.class)
		appComponent := &testAttachSignalComponent{
			testSidecarComponent: &testSidecarComponent{name: SidecarComponentTls, closed: make(chan struct{})},
			attached:             make(chan struct{}, 1),
		}
		var builds atomic.Int32
		var releases atomic.Int32
		watchSettings := sidecar.DefaultDnsWatchComponentSettings()
		watchSettings.Doh.ForwarderFactory = func() (*sidecar.DnsWatchForwarders, error) {
			builds.Add(1)
			return &sidecar.DnsWatchForwarders{
				Forwarders: []sidecar.DnsWatchForwarder{testSidecarSilentForwarder{}, testSidecarSilentForwarder{}},
				Close:      func() { releases.Add(1) },
			}, nil
		}
		settings := DefaultSidecarSettings()
		settings.HostClass = role.class
		settings.StateDir = role.world.stateDir
		settings.Components.Add(SidecarComponentTls)
		local, err := newSidecarLocal(context.Background(), role.world.space, role.world.keyStore, nil, settings, &sidecarLocalOptions{
			adjustCoreSettings: func(settings *sidecar.SidecarSettings) {
				settings.ApiUrl = role.coreUrl
				settings.PlatformUrl = "wss://127.0.0.1:1"
			},
			hostCredentialStores: func() []sidecar.SidecarHostCredentialStore {
				return nil
			},
			componentFactories: map[string]sidecarComponentFactory{
				SidecarComponentTls: func(ctx context.Context, local *sidecar.Sidecar, settings *SidecarSettings) (sidecar.SidecarComponent, error) {
					return appComponent, nil
				},
			},
			searchFreeByteCount: func(dir string) (uint64, error) { return 8 * 1024 * 1024 * 1024, nil },
			dnsWatchSettings:    watchSettings,
		})
		if err != nil {
			t.Fatal(err)
		}
		role.local = local
		// closed again below, which a second close answers at once
		t.Cleanup(func() {
			role.world.close(t, local)
		})
		role.awaitUnenrolled()
		if err := role.enroll(); err != nil {
			t.Fatal(err)
		}
		// a generation attaches components in the order they were added: the zone watch before the app's
		select {
		case <-appComponent.attached:
		case <-time.After(testSidecarTimeout):
			t.Fatalf("%s: the enrolled role's generation attached no app component", c.class)
		}
		if got := builds.Load(); got != c.builds || releases.Load() != 0 {
			t.Fatalf("%s: the zone watch built %d resolvers, not %d, and released %d", c.class, got, c.builds, releases.Load())
		}
		role.world.close(t, local)
		if releases.Load() != c.builds {
			t.Errorf("%s: closing the Sidecar released the zone watch's resolvers %d times", c.class, releases.Load())
		}
	}
}
