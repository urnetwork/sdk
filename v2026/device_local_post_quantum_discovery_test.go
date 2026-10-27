package sdk

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A device whose performance profile requires post-quantum encryption runs
// every window client fail closed, so its discovery has to ask
// find-providers2 for providers that advertise post-quantum encryption
// sessions (support inbox 7918, 7843,
// https://github.com/urnetwork/connect/issues/220). Every app sets the
// requirement through the device's performance profile. These follow it
// through the real device construction and live setter to the requests of the
// device's own generator, read as the JSON the platform receives.

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

// The value a request asks for post_quantum_encryption, and whether it names
// the field at all.
func testingPostQuantumRequested(t *testing.T, body string) (requested bool, present bool) {
	t.Helper()
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	value, present := fields["post_quantum_encryption"]
	if !present {
		return false, false
	}
	if err := json.Unmarshal(value, &requested); err != nil {
		t.Fatal(err)
	}
	return requested, true
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
// for providers that advertise it: the windows of the multi client it
// constructs under the profile, and every later request of its generator.
// Turning the requirement off live returns the requests to those of an older
// client, and turning it on again asks again. Before this change no request
// could ask, so a post-quantum device dialed and failed through providers
// that could never seal its sessions.
func TestDevicePostQuantumProfileAsksDiscoveryForPostQuantumProviders(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	discovery := newTestingPostQuantumDiscovery()
	postQuantum := &PerformanceProfile{WindowType: WindowTypeAuto, PostQuantumEncryption: true}
	device := newTestingPostQuantumDevice(t, discovery, postQuantum)

	// the profile never changed while these were made
	for _, body := range discovery.windowDiscovered(t, ctx) {
		if requested, _ := testingPostQuantumRequested(t, body); !requested {
			t.Fatalf("a post-quantum window discovered without asking for post-quantum providers: %s", body)
		}
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

	body := discover()
	if requested, _ := testingPostQuantumRequested(t, body); !requested {
		t.Fatalf("the post-quantum device's generator did not ask for post-quantum providers: %s", body)
	}

	device.SetPerformanceProfile(&PerformanceProfile{WindowType: WindowTypeAuto})
	body = discover()
	if _, present := testingPostQuantumRequested(t, body); present {
		t.Fatalf("discovery still asks for post-quantum providers after the profile stopped requiring them: %s", body)
	}

	device.SetPerformanceProfile(postQuantum)
	body = discover()
	if requested, _ := testingPostQuantumRequested(t, body); !requested {
		t.Fatalf("discovery did not ask for post-quantum providers after the profile required them again: %s", body)
	}
}

// A device without the requirement, the default and every device before this
// change, keeps its requests exactly as they were: no post_quantum_encryption
// field, so discovery stays capability-blind.
func TestDeviceDefaultProfileKeepsCapabilityBlindDiscovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	discovery := newTestingPostQuantumDiscovery()
	newTestingPostQuantumDevice(t, discovery, nil)
	for _, body := range discovery.windowDiscovered(t, ctx) {
		if _, present := testingPostQuantumRequested(t, body); present {
			t.Fatalf("a device without the requirement asked about post-quantum providers: %s", body)
		}
	}
}
