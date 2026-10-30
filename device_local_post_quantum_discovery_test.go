package sdk

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A device whose performance profile requires post-quantum encryption runs
// every window client fail closed, so its discovery has to ask
// find-providers2 for providers that can seal its sessions (support inbox
// 7918, 7843, https://github.com/urnetwork/connect/issues/220). Since connect
// 72eb15a4 it asks for modern providers (require_modern_provider), which hold
// post-quantum session evidence and resizable windows, and no longer for
// declaring providers (post_quantum_encryption) (server FP2-MODERN.md,
// decision 1). Every app sets the requirement through the device's
// performance profile. These follow it through the real device construction
// and live setter to the requests of the device's own generator, read as the
// JSON the platform receives.

// testingPostQuantumDiscovery records each find-providers2 request and answers
// no providers, so the windows keep discovering and never dial. A request
// that excludes the marker destination is the test's own.
type testingPostQuantumDiscovery struct {
	marker connect.MultiHopId

	stateLock      sync.Mutex
	windowRequests []string
	testRequests   []string
	windowRequest  chan struct{}
}

func newTestingPostQuantumDiscovery() *testingPostQuantumDiscovery {
	return &testingPostQuantumDiscovery{
		marker:        connect.RequireMultiHopId(connect.NewId()),
		windowRequest: make(chan struct{}, 1),
	}
}

func (self *testingPostQuantumDiscovery) FindProviders2(ctx context.Context, _ string, args *connect.FindProviders2Args) (*connect.FindProviders2Result, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	marked := slices.ContainsFunc(args.ExcludeDestinations, func(ids []connect.Id) bool {
		return slices.Equal(ids, self.marker.Ids())
	})
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if marked {
			self.testRequests = append(self.testRequests, string(body))
		} else {
			self.windowRequests = append(self.windowRequests, string(body))
		}
	}()
	if !marked {
		select {
		case self.windowRequest <- struct{}{}:
		default:
		}
	}
	return &connect.FindProviders2Result{Providers: []*connect.FindProvidersProvider{}}, nil
}

// Waits for a window's own request, then returns every window request so far.
func (self *testingPostQuantumDiscovery) windowDiscovered(t *testing.T, ctx context.Context) []string {
	t.Helper()
	select {
	case <-self.windowRequest:
	case <-ctx.Done():
		t.Fatal("no window discovered")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.windowRequests)
}

// The value a request asks for a boolean field, and whether it names the
// field at all.
func testingDiscoveryRequestFlag(t *testing.T, body string, field string) (requested bool, present bool) {
	t.Helper()
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	value, present := fields[field]
	if !present {
		return false, false
	}
	if err := json.Unmarshal(value, &requested); err != nil {
		t.Fatal(err)
	}
	return requested, true
}

// Fails unless the request sends require_modern_provider true and leaves
// post_quantum_encryption off the wire.
func assertModernDiscoveryRequest(t *testing.T, body string, description string) {
	t.Helper()
	if requested, _ := testingDiscoveryRequestFlag(t, body, "require_modern_provider"); !requested {
		t.Fatalf("%s did not ask for modern providers: %s", description, body)
	}
	if _, present := testingDiscoveryRequestFlag(t, body, "post_quantum_encryption"); present {
		t.Fatalf("%s still asked for declaring providers: %s", description, body)
	}
}

// Fails unless the request names neither capability field, as every request
// before them did not.
func assertCapabilityBlindDiscoveryRequest(t *testing.T, body string, description string) {
	t.Helper()
	for _, field := range []string{"require_modern_provider", "post_quantum_encryption"} {
		if _, present := testingDiscoveryRequestFlag(t, body, field); present {
			t.Fatalf("%s sent %s: %s", description, field, body)
		}
	}
}

// A connected device whose discovery goes to `discovery`, with the profile set
// before it connects, as an app restores its saved profile.
func newTestingPostQuantumDevice(t *testing.T, discovery *testingPostQuantumDiscovery, performanceProfile *PerformanceProfile) *DeviceLocal {
	t.Helper()
	fixture := testingAuthClientShapeSpace(t)
	settings := DefaultLowMemoryDeviceLocalSettings()
	settings.MemoryTargetByteCount = 32 * 1024 * 1024
	settings.AllowProvider = false
	settings.DisableLogging = true
	settings.ProviderDiscovery = discovery
	device := newSettingsOwnershipDevice(t, fixture, settings, true)
	upgrade := connect.DefaultUpgradeMuxSettings()
	upgrade.Dns = nil
	device.SetUpgradeMuxSettings(upgrade)
	device.SetPerformanceProfile(performanceProfile)
	location := &ConnectLocation{ConnectLocationId: &ConnectLocationId{BestAvailable: true}}
	if err := device.SetConnectLocationChecked(location); err != nil {
		t.Fatal(err)
	}
	return device
}

// A device whose profile requires post-quantum encryption asks find-providers2
// for modern providers: the windows of the multi client it constructs under
// the profile, and every later request of its generator. None of them asks
// for declaring providers any more. Turning the requirement off live returns
// the requests to those of an older client, and turning it on again asks
// again. Before discovery could ask, a post-quantum device dialed and failed
// through providers that could never seal its sessions.
func TestDevicePostQuantumProfileAsksDiscoveryForModernProviders(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	discovery := newTestingPostQuantumDiscovery()
	postQuantum := &PerformanceProfile{WindowType: WindowTypeAuto, PostQuantumEncryption: true}
	device := newTestingPostQuantumDevice(t, discovery, postQuantum)

	// the profile never changed while these were made
	for _, body := range discovery.windowDiscovered(t, ctx) {
		assertModernDiscoveryRequest(t, body, "a post-quantum window")
	}

	device.stateLock.Lock()
	generator := device.apiMultiClientGenerator
	device.stateLock.Unlock()
	if generator == nil {
		t.Fatal("the device did not construct its API generator")
	}
	// the live setter has updated the generator by the time it returns, so a
	// request made after it is decided by the new profile alone
	discover := func() string {
		t.Helper()
		func() {
			discovery.stateLock.Lock()
			defer discovery.stateLock.Unlock()
			discovery.testRequests = nil
		}()
		if _, err := generator.NextDestinationsContext(ctx, 3, []connect.MultiHopId{discovery.marker}, "quality"); err != nil {
			t.Fatal(err)
		}
		discovery.stateLock.Lock()
		defer discovery.stateLock.Unlock()
		if len(discovery.testRequests) != 1 {
			t.Fatalf("discovered by %d requests, want 1", len(discovery.testRequests))
		}
		return discovery.testRequests[0]
	}

	assertModernDiscoveryRequest(t, discover(), "the post-quantum device's generator")

	device.SetPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeAuto})
	assertCapabilityBlindDiscoveryRequest(t, discover(), "discovery after the profile stopped requiring post-quantum encryption")

	device.SetPerformanceProfile(postQuantum)
	assertModernDiscoveryRequest(t, discover(), "discovery after the profile required post-quantum encryption again")
}

// A device without the requirement, the default and every device before this
// change, keeps its requests exactly as they were: neither
// require_modern_provider nor post_quantum_encryption, so discovery stays
// capability-blind.
func TestDeviceDefaultProfileKeepsCapabilityBlindDiscovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	discovery := newTestingPostQuantumDiscovery()
	newTestingPostQuantumDevice(t, discovery, nil)
	for _, body := range discovery.windowDiscovered(t, ctx) {
		assertCapabilityBlindDiscoveryRequest(t, body, "a device without the requirement")
	}
}
