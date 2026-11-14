//go:build !ios_extension && !js

package sdk

// sdk.Sidecar's shell: NewSidecarLocal over a real state directory and an app key store in memory, the urls it takes
// from its space, its components by name, its refusals, Close, the gate's result, and the binding's two rules that a
// reader can hold: no sidecar-package type crosses, and no leg links mcp/ or cli/. Every host is a .example name, and
// no Sidecar here enrolls, so none dials.

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
)

// How long a test waits on a Sidecar before calling itself broken. Every wait is on an event or a channel.
const testSidecarTimeout = 30 * time.Second

// One test's app: a state directory, an app key store, and a space.
type testSidecarWorld struct {
	stateDir string
	keyStore *testSidecarKeyStore
	space    *NetworkSpace
}

func newTestSidecarWorld(t *testing.T) *testSidecarWorld {
	t.Helper()
	return &testSidecarWorld{
		stateDir: t.TempDir(),
		keyStore: newTestSidecarKeyStore(),
		space:    newTestSidecarSpace("https://api.sdktest.example", "wss://connect.sdktest.example"),
	}
}

// A space holding only its urls, which starts nothing and dials nothing.
func newTestSidecarSpace(apiUrl string, platformUrl string) *NetworkSpace {
	return &NetworkSpace{apiUrl: apiUrl, platformUrl: platformUrl}
}

// A local Sidecar of the world, closed at the test's end if the test has not.
func (self *testSidecarWorld) open(t *testing.T) *Sidecar {
	t.Helper()
	return self.openWith(t, DefaultSidecarSettings(), &sidecarLocalOptions{})
}

// A local Sidecar of the world on settings, its StateDir the world's.
func (self *testSidecarWorld) openWith(t *testing.T, settings *SidecarSettings, options *sidecarLocalOptions) *Sidecar {
	t.Helper()
	settings.StateDir = self.stateDir
	local, err := newSidecarLocal(context.Background(), self.space, self.keyStore, nil, settings, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		self.close(t, local)
	})
	return local
}

// Closes local and waits for it.
func (self *testSidecarWorld) close(t *testing.T, local *Sidecar) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testSidecarTimeout)
	defer cancel()
	if err := local.CloseAndWait(ctx); err != nil {
		t.Errorf("close: %v", err)
	}
}

// Records a network under synthetic ids, as an enrollment's last step does, so a lost key is a loss from here on.
func (self *testSidecarWorld) recordNetwork(t *testing.T, local *Sidecar) sidecar.SidecarNetworkRecord {
	t.Helper()
	record := sidecar.SidecarNetworkRecord{
		NetworkId:         sidecar.NewId(),
		ClientId:          sidecar.NewId(),
		SidecarId:         sidecar.NewId(),
		ConnectInstanceId: sidecar.NewId(),
	}
	if err := local.instance.RecordNetwork(record); err != nil {
		t.Fatal(err)
	}
	return record
}

// Fails unless nothing reached the store or the state directory.
func (self *testSidecarWorld) requireNothingCreated(t *testing.T) {
	t.Helper()
	if stored, writeCount, deleteCount := self.keyStore.snapshot(); len(stored) != 0 || writeCount != 0 || deleteCount != 0 {
		t.Errorf("the store holds %d items after %d writes and %d deletes", len(stored), writeCount, deleteCount)
	}
	if files := self.stateFiles(t); len(files) != 0 {
		names := []string{}
		for name := range files {
			names = append(names, name)
		}
		t.Errorf("the state directory holds %v", names)
	}
}

// Waits until local's core is in state with reason.
func awaitTestSidecarState(t *testing.T, local *Sidecar, state sidecar.SidecarState, reason sidecar.SidecarStateReason) {
	t.Helper()
	awaitTestSidecarStatus(t, local, func(status sidecar.SidecarStatus) bool {
		return status.State == state && status.Reason == reason
	})
}

// Waits until local's core has left state.
func awaitTestSidecarStateMoved(t *testing.T, local *Sidecar, state sidecar.SidecarState) {
	t.Helper()
	awaitTestSidecarStatus(t, local, func(status sidecar.SidecarStatus) bool {
		return status.State != state
	})
}

// Waits until want holds of local's core's status, reading it again at each event.
func awaitTestSidecarStatus(t *testing.T, local *Sidecar, want func(status sidecar.SidecarStatus) bool) {
	t.Helper()
	events := sidecar.NewMonitorValue(0)
	remove := local.Core().AddEventCallback(func(event sidecar.SidecarEvent) {
		events.Update(func(count int) int { return count + 1 })
	})
	defer remove()
	deadline := time.After(testSidecarTimeout)
	for {
		_, changed := events.Get()
		status := local.Core().Status()
		if want(status) {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("the status stayed %+v", status)
		}
	}
}

// Fails unless local's instance is in state.
func requireTestInstanceState(t *testing.T, local *Sidecar, state sidecar.SidecarInstanceState) {
	t.Helper()
	if got, _ := local.instance.State(); got != state {
		t.Errorf("the instance is %s, not %s", got, state)
	}
}

// A first run opens U by the key store's wrap, writing it once, and a reopen opens the same U with no write. Before
// enrollment there is no sidecar_id.
func TestNewSidecarLocalOpensByTheKeyStoreWrap(t *testing.T) {
	world := newTestSidecarWorld(t)
	first := world.open(t)
	awaitTestSidecarState(t, first, sidecar.SidecarStateUnenrolled, "")
	requireTestInstanceState(t, first, sidecar.SidecarInstanceOpen)
	if sidecarId := first.GetSidecarId(); sidecarId != nil {
		t.Errorf("an unenrolled sidecar names %s", sidecarId)
	}
	stored, writeCount, _ := world.keyStore.snapshot()
	if len(stored) != 1 || writeCount != 1 {
		t.Fatalf("a first run wrote %d items in %d writes", len(stored), writeCount)
	}
	world.close(t, first)
	before := world.stateFiles(t)
	if len(before) == 0 {
		t.Fatal("a first run wrote no state")
	}

	second := world.open(t)
	awaitTestSidecarState(t, second, sidecar.SidecarStateUnenrolled, "")
	if nowStored, nowWrites, _ := world.keyStore.snapshot(); nowWrites != writeCount || !mapsEqual(stored, nowStored) {
		t.Errorf("a reopen wrote the store %d times", nowWrites-writeCount)
	}
}

// What NewSidecarLocal cannot run it refuses, writing nothing.
func TestNewSidecarLocalRefusesWhatItCannotRun(t *testing.T) {
	world := newTestSidecarWorld(t)
	newSettings := func(edit func(settings *SidecarSettings)) *SidecarSettings {
		settings := DefaultSidecarSettings()
		settings.StateDir = world.stateDir
		if edit != nil {
			edit(settings)
		}
		return settings
	}
	cases := []struct {
		name     string
		space    *NetworkSpace
		keyStore SidecarKeyStore
		settings *SidecarSettings
	}{
		{name: "no space", keyStore: world.keyStore, settings: newSettings(nil)},
		{name: "no key store", space: world.space, settings: newSettings(nil)},
		{name: "no settings", space: world.space, keyStore: world.keyStore},
		{name: "bad instance", space: world.space, keyStore: world.keyStore, settings: newSettings(func(settings *SidecarSettings) { settings.Instance = "Not An Instance" })},
		{name: "relative state dir", space: world.space, keyStore: world.keyStore, settings: newSettings(func(settings *SidecarSettings) { settings.StateDir = "relative/dir" })},
		{name: "no state dir", space: world.space, keyStore: world.keyStore, settings: newSettings(func(settings *SidecarSettings) { settings.StateDir = "" })},
		{name: "negative budget", space: world.space, keyStore: world.keyStore, settings: newSettings(func(settings *SidecarSettings) { settings.MemoryBudgetByteCount = -1 })},
		{name: "unknown component", space: world.space, keyStore: world.keyStore, settings: newSettings(func(settings *SidecarSettings) { settings.Components.Add("ntp") })},
		// an app lists no bridge, since forwarding is its VPN component's (§18.5), so no factory binds it
		{name: "unbound component", space: world.space, keyStore: world.keyStore, settings: newSettings(func(settings *SidecarSettings) { settings.Components.Add(SidecarComponentBridge) })},
		{name: "an address for the api", space: newTestSidecarSpace("https://192.0.2.1", "wss://192.0.2.1"), keyStore: world.keyStore, settings: newSettings(nil)},
		{name: "an api host without the api label", space: newTestSidecarSpace("https://gateway.sdktest.example", "wss://connect.sdktest.example"), keyStore: world.keyStore, settings: newSettings(nil)},
	}
	for _, c := range cases {
		local, err := NewSidecarLocal(c.space, c.keyStore, nil, c.settings)
		if err == nil {
			local.Close()
			t.Errorf("%s: opened", c.name)
		}
	}
	world.requireNothingCreated(t)
}

// The core's urls follow the space: api and platform its own, and every other service its label in the api's
// place, an env's prefix, port and path kept, so no service of another space is reached.
func TestNewSidecarLocalTakesTheSpacesUrls(t *testing.T) {
	cases := []struct {
		apiUrl      string
		platformUrl string
		want        map[string]string
	}{
		{
			apiUrl:      "https://api.sdktest.example",
			platformUrl: "wss://connect.sdktest.example",
			want: map[string]string{
				"auth": "https://auth.sdktest.example", "evs": "https://evs.sdktest.example", "gkm": "https://gkm.sdktest.example",
				"sockets": "https://sockets.sdktest.example", "dns": "https://dns.sdktest.example",
			},
		},
		{
			apiUrl:      "https://test-api.sdktest.example:8443/synthetic-env-path",
			platformUrl: "wss://test-connect.sdktest.example/synthetic-env-path",
			want: map[string]string{
				"auth": "https://test-auth.sdktest.example:8443/synthetic-env-path", "evs": "https://test-evs.sdktest.example:8443/synthetic-env-path",
				"gkm": "https://test-gkm.sdktest.example:8443/synthetic-env-path", "sockets": "https://test-sockets.sdktest.example:8443/synthetic-env-path",
				"dns": "https://test-dns.sdktest.example:8443/synthetic-env-path",
			},
		},
	}
	for _, c := range cases {
		world := newTestSidecarWorld(t)
		world.space = newTestSidecarSpace(c.apiUrl, c.platformUrl)
		var coreSettings *sidecar.SidecarSettings
		world.openWith(t, DefaultSidecarSettings(), &sidecarLocalOptions{
			adjustCoreSettings: func(settings *sidecar.SidecarSettings) {
				coreSettings = settings
			},
		})
		got := map[string]string{
			"auth": coreSettings.AuthUrl, "evs": coreSettings.EvsUrl, "gkm": coreSettings.GkmUrl,
			"sockets": coreSettings.SocketsUrl, "dns": coreSettings.DnsUrl,
		}
		for service, want := range c.want {
			if got[service] != want {
				t.Errorf("%s: %s is %q, not %q", c.apiUrl, service, got[service], want)
			}
		}
		if coreSettings.ApiUrl != c.apiUrl || coreSettings.PlatformUrl != c.platformUrl {
			t.Errorf("%s: api %q, platform %q", c.apiUrl, coreSettings.ApiUrl, coreSettings.PlatformUrl)
		}
	}
}

// A component with no factory, the bare shell's case for all five.
type testSidecarComponent struct {
	name      string
	closed    chan struct{}
	closeOnce sync.Once
}

func (self *testSidecarComponent) Name() string {
	return self.name
}

func (self *testSidecarComponent) Attach(env *sidecar.SidecarEnv) error {
	return nil
}

func (self *testSidecarComponent) Close() {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
}

// Puts factory in place for name until the test ends, as a wrapper registers at init, then puts back what was there:
// the factory of a component a wrapper binds, tls's among them, which the test stands in for, or none.
func withTestSidecarComponentFactory(t *testing.T, name string, factory sidecarComponentFactory) {
	t.Helper()
	previous, bound := sidecarComponentFactoryOf(name)
	if bound {
		sidecarComponentFactories.stateLock.Lock()
		sidecarComponentFactories.factories[name] = factory
		sidecarComponentFactories.stateLock.Unlock()
	} else {
		registerSidecarComponentFactory(name, factory)
	}
	t.Cleanup(func() {
		sidecarComponentFactories.stateLock.Lock()
		defer sidecarComponentFactories.stateLock.Unlock()
		if bound {
			sidecarComponentFactories.factories[name] = previous
			return
		}
		delete(sidecarComponentFactories.factories, name)
	})
}

// A component named in the settings is built by its wrapper's factory from the core and attached, and closes with
// the Sidecar; a factory that fails refuses the Sidecar.
func TestComponentsAttachByName(t *testing.T) {
	component := &testSidecarComponent{name: SidecarComponentTls, closed: make(chan struct{})}
	var builtFrom *sidecar.Sidecar
	withTestSidecarComponentFactory(t, SidecarComponentTls, func(ctx context.Context, local *sidecar.Sidecar, settings *SidecarSettings) (sidecar.SidecarComponent, error) {
		builtFrom = local
		return component, nil
	})
	world := newTestSidecarWorld(t)
	settings := DefaultSidecarSettings()
	settings.Components.Add(SidecarComponentTls)
	local := world.openWith(t, settings, &sidecarLocalOptions{})
	if builtFrom == nil || builtFrom != local.Core() {
		t.Fatal("the factory was not given the core")
	}
	if _, err := local.Core().AddComponent(&testSidecarComponent{name: SidecarComponentTls, closed: make(chan struct{})}); err == nil {
		t.Error("a second tls component attached: the first was not")
	}
	world.close(t, local)
	select {
	case <-component.closed:
	default:
		t.Error("the component outlived the Sidecar")
	}

	withTestSidecarComponentFactory(t, SidecarComponentMail, func(ctx context.Context, local *sidecar.Sidecar, settings *SidecarSettings) (sidecar.SidecarComponent, error) {
		return nil, errors.New("synthetic factory refusal")
	})
	failing := DefaultSidecarSettings()
	failing.StateDir = t.TempDir()
	failing.Components.Add(SidecarComponentMail)
	if local, err := NewSidecarLocal(world.space, newTestSidecarKeyStore(), nil, failing); err == nil {
		local.Close()
		t.Error("a failing factory opened")
	}
	twice := DefaultSidecarSettings()
	twice.StateDir = t.TempDir()
	twice.Components.Add(SidecarComponentTls)
	twice.Components.Add(SidecarComponentTls)
	if local, err := NewSidecarLocal(world.space, newTestSidecarKeyStore(), nil, twice); err == nil {
		local.Close()
		t.Error("a component named twice opened")
	}
}

// Close ends delivery at once and the core and instance after, is safe twice and from a listener, and CloseAndWait
// joins it.
func TestCloseEndsTheSidecar(t *testing.T) {
	world := newTestSidecarWorld(t)
	world.keyStore.setMode(testKeyStoreLocked)
	local := world.open(t)
	awaitTestSidecarState(t, local, sidecar.SidecarStateLocked, "")
	received := make(chan *SidecarEvent, 16)
	local.AddEventListener(&testSidecarListener{events: received, closeOnFirst: local})

	world.keyStore.setMode(testKeyStoreWorking)
	if err := local.Core().Unlock(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-received:
		if event.Kind != "state" {
			t.Errorf("first event %+v", event)
		}
	case <-time.After(testSidecarTimeout):
		t.Fatal("no event before the close")
	}
	ctx, cancel := context.WithTimeout(context.Background(), testSidecarTimeout)
	defer cancel()
	if err := local.CloseAndWait(ctx); err != nil {
		t.Fatal(err)
	}
	local.Close()
	if err := local.Core().CloseAndWait(ctx); err != nil {
		t.Errorf("the core outlived the Sidecar: %v", err)
	}
	select {
	case event := <-received:
		t.Errorf("an event after the close: %+v", event)
	default:
	}
}

// A listener that hands each event on, and closes its Sidecar at the first when told to.
type testSidecarListener struct {
	events       chan *SidecarEvent
	closeOnFirst *Sidecar
	closeOnce    sync.Once
}

func (self *testSidecarListener) SidecarEventReceived(event *SidecarEvent) {
	select {
	case self.events <- event:
	default:
	}
	if self.closeOnFirst != nil {
		self.closeOnce.Do(self.closeOnFirst.Close)
	}
}

// An outward action the gate holds reads approval_pending:<id>, not Ok, so no wrapper adds a check of its own; a
// revocation's refusal keeps its code; anything else carries its message.
func TestSidecarResultCarriesTheGate(t *testing.T) {
	approvalId := sidecar.NewId()
	pending := sidecarResultOf("", &sidecar.ApprovalPendingError{ApprovalId: approvalId})
	if pending.Ok || pending.Value != "approval_pending:"+approvalId.String() || pending.Code != "" {
		t.Errorf("pending %+v", pending)
	}
	wrapped := sidecarResultOf("", errors.Join(errors.New("context"), &sidecar.ApprovalPendingError{ApprovalId: approvalId}))
	if wrapped.Value != pending.Value {
		t.Errorf("a wrapped pending %+v", wrapped)
	}
	refused := sidecarResultOf("", &sidecar.DeviceRevokeError{Code: "REVOKE_NOT_CAPABLE"})
	if refused.Ok || refused.Code != "REVOKE_NOT_CAPABLE" {
		t.Errorf("refused %+v", refused)
	}
	failed := sidecarResultOf("", errors.New("synthetic failure"))
	if failed.Ok || failed.Message != "synthetic failure" || failed.Value != "" {
		t.Errorf("failed %+v", failed)
	}
	if ok := sidecarResultOf("repo-id", nil); !ok.Ok || ok.Value != "repo-id" {
		t.Errorf("ok %+v", ok)
	}
}

// Whether a type, or anything it is built of, comes from the sidecar package.
func testSidecarPackageTypeIn(typ reflect.Type, visited map[reflect.Type]bool) bool {
	if typ == nil || visited[typ] {
		return false
	}
	visited[typ] = true
	if strings.HasPrefix(typ.PkgPath(), "github.com/urnetwork/sidecar") {
		return true
	}
	switch typ.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		return testSidecarPackageTypeIn(typ.Elem(), visited)
	case reflect.Map:
		return testSidecarPackageTypeIn(typ.Key(), visited) || testSidecarPackageTypeIn(typ.Elem(), visited)
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i += 1 {
			if typ.Field(i).IsExported() && testSidecarPackageTypeIn(typ.Field(i).Type, visited) {
				return true
			}
		}
	case reflect.Func:
		for i := 0; i < typ.NumIn(); i += 1 {
			if testSidecarPackageTypeIn(typ.In(i), visited) {
				return true
			}
		}
		for i := 0; i < typ.NumOut(); i += 1 {
			if testSidecarPackageTypeIn(typ.Out(i), visited) {
				return true
			}
		}
	case reflect.Interface:
		for i := 0; i < typ.NumMethod(); i += 1 {
			if testSidecarPackageTypeIn(typ.Method(i).Type, visited) {
				return true
			}
		}
	}
	return false
}

// No exported signature or field of the shell names a sidecar-package type but the Go-only Core and CloseAndWait,
// and none takes a name an Objective-C binding refuses (§18.5's rules).
func TestNoSidecarTypeCrosses(t *testing.T) {
	goOnly := []string{"Core", "CloseAndWait"}
	checked := 0
	check := func(name string, typ reflect.Type) {
		checked += 1
		if testSidecarPackageTypeIn(typ, map[reflect.Type]bool{}) {
			t.Errorf("%s crosses a sidecar-package type: %s", name, typ)
		}
	}
	sidecarType := reflect.TypeFor[*Sidecar]()
	for i := 0; i < sidecarType.NumMethod(); i += 1 {
		method := sidecarType.Method(i)
		if slices.Contains(goOnly, method.Name) {
			continue
		}
		check("Sidecar."+method.Name, method.Type)
	}
	for name, function := range map[string]any{
		"NewSidecarLocal":        NewSidecarLocal,
		"DefaultSidecarSettings": DefaultSidecarSettings,
	} {
		check(name, reflect.TypeOf(function))
	}
	for _, typ := range []reflect.Type{
		reflect.TypeFor[SidecarSettings](),
		reflect.TypeFor[SidecarSecret](),
		reflect.TypeFor[SidecarResult](),
		reflect.TypeFor[SidecarResultCallback](),
		reflect.TypeFor[SidecarEvent](),
		reflect.TypeFor[SidecarErasure](),
		reflect.TypeFor[SidecarKeyStore](),
		reflect.TypeFor[SidecarPresenceSigner](),
		reflect.TypeFor[SidecarEventListener](),
	} {
		check(typ.Name(), typ)
		names := []string{}
		switch typ.Kind() {
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i += 1 {
				names = append(names, typ.Field(i).Name)
			}
		case reflect.Interface:
			for i := 0; i < typ.NumMethod(); i += 1 {
				names = append(names, typ.Method(i).Name)
			}
		}
		for _, name := range names {
			if name == "Description" || name == "Hash" {
				t.Errorf("%s.%s collides with NSObject in the Objective-C binding", typ.Name(), name)
			}
		}
	}
	if checked < 14 {
		t.Fatalf("CONTROL FAILED: only %d signatures checked", checked)
	}
	// the rule sees a crossing where there is one
	if !testSidecarPackageTypeIn(reflect.TypeOf((*Sidecar).Core), map[reflect.Type]bool{}) {
		t.Error("CONTROL FAILED: Core's sidecar type was not seen")
	}
}

// No leg the sdk ships links mcp/, cli/ or sidecarctl (R-SDK-4), and the iOS extension's links of the sidecar module
// only the device role, sidecar/device and protocol/bridge, since the sidecar role is !ios_extension (D-SDK-20).
func TestBindingsLinkNoMcpOrCli(t *testing.T) {
	refused := []string{
		"github.com/urnetwork/sidecar/mcp",
		"github.com/urnetwork/sidecar/cli",
		"github.com/urnetwork/server/sidecarctl",
	}
	deviceRole := []string{
		"github.com/urnetwork/sidecar/device",
		"github.com/urnetwork/sidecar/protocol/bridge",
	}
	linksSidecar := false
	extensionLinksDeviceRole := false
	for _, leg := range messagingBoundaryLegs {
		if leg.name == sidecarWasmLegName || leg.moduleDirectory == "build" {
			continue
		}
		packages, _ := messagingBoundaryLegPackages(t, ".", leg)
		for _, packagePath := range packages {
			for _, root := range refused {
				if packagePath == root || strings.HasPrefix(packagePath, root+"/") {
					t.Errorf("%s links %s", leg.name, packagePath)
				}
			}
			if !strings.Contains(leg.tags, "ios_extension") {
				continue
			}
			if slices.Contains(deviceRole, packagePath) {
				extensionLinksDeviceRole = true
				continue
			}
			if packagePath == "github.com/urnetwork/sidecar" || strings.HasPrefix(packagePath, "github.com/urnetwork/sidecar/") {
				t.Errorf("%s links %s", leg.name, packagePath)
			}
		}
		if slices.Contains(packages, "github.com/urnetwork/sidecar") {
			linksSidecar = true
		}
	}
	if !linksSidecar || !extensionLinksDeviceRole {
		t.Fatal("CONTROL FAILED: no leg links the sidecar root, or the extension no device role, so the rule saw nothing")
	}
}
