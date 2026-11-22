//go:build !ios_extension && !js

package sdk

// The local Sidecar's dns component (§12.5's label row; the core's dns_component.go): the composition adds it to the
// core whatever components the app names, and once the core enrolls, its first client generation feeds the core's
// DnsClient the network's label table in the network's dns/ directory, before any component the app named attaches.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// An app component that tells each attach.
type testAttachSignalComponent struct {
	*testSidecarComponent
	attached chan struct{}
}

func (self *testAttachSignalComponent) Attach(env *sidecar.SidecarEnv) error {
	select {
	case self.attached <- struct{}{}:
	default:
	}
	return nil
}

// With no component named the core holds the dns component already, so a second of that name does not add.
func TestNewSidecarLocalAddsTheDnsComponent(t *testing.T) {
	world := newTestSidecarWorld(t)
	local := world.open(t)
	if remove, err := local.Core().AddComponent(&testSidecarComponent{name: "dns", closed: make(chan struct{})}); err == nil {
		remove()
		t.Fatal("the local Sidecar's composition added no dns component")
	}
}

// Enrolled as EnrollWithDevice enrolls, the role's first generation attaches the dns component before the app's tls
// component, its client fed the network's own table, which a reopen of the network's dns/ directory reads.
func TestLocalSidecarFeedsTheNetworksLabelTable(t *testing.T) {
	role := newTestT4Role(t, SidecarHostClassIos)
	appComponent := &testAttachSignalComponent{
		testSidecarComponent: &testSidecarComponent{name: SidecarComponentTls, closed: make(chan struct{})},
		attached:             make(chan struct{}, 1),
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
	})
	if err != nil {
		t.Fatal(err)
	}
	role.local = local
	t.Cleanup(func() {
		role.world.close(t, local)
	})
	role.awaitUnenrolled()
	if err := role.enroll(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-appComponent.attached:
	case <-time.After(testSidecarTimeout):
		t.Fatal("the enrolled role's generation attached no app component")
	}
	core := local.Core()
	dns, err := core.Dns()
	if err != nil {
		t.Fatal(err)
	}
	labels := dns.Labels()
	if labels == nil {
		t.Fatal("the role's generation fed its DnsClient no label table")
	}
	if held, err := core.NetworkLabels(); err != nil || held != labels {
		t.Fatalf("the table fed is not the network's: %v", err)
	}
	store, err := core.LocalStore().Directory("dns", "dns")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sidecar.OpenNetworkLabelTable(store); err != nil {
		t.Fatalf("the network's dns/ directory: %v", err)
	}
}
