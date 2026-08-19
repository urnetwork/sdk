package sdk

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The advanced-mode bridge: the controls a desktop client needs to tune the
// sdk from a DeviceRemote, whose device lives in another process.
//
// Three things are pinned here that the reliability bridge does not cover:
//
//   - the fault injection actions (drop, stall/unstall) reach the local AND
//     report whether they found the exit, where the older action bridges
//     returned nothing at all;
//   - the probe suite round trips as a suite -- start, poll, results, stop --
//     including a results list read before any run, which is the empty-list
//     shape the c++ wrapper unwraps into a std::vector; and
//   - MigrateExit and ProbeAllExits carry their counts, which the bridge used
//     to drop on the floor.
//
// These are ordinary build-tag-free tests in the default compile set, and run
// on Windows as well as in the Linux sdk CI.

// gob wire fidelity for the new payloads

// TestRpcGobStallExitComplete covers the only reliability control that takes
// more than one argument. The false case is called out because gob omits
// zero-valued fields: if `Stalled` did not survive as false, "unstall" would
// arrive as "stall" and the control would be one-way.
func TestRpcGobStallExitComplete(t *testing.T) {
	clientId := connect.NewId()

	wiredStalled := gobRoundTrip(t, &DeviceRemoteStallExitRpc{
		ClientId: clientId,
		Stalled:  true,
	})
	connect.AssertEqual(t, wiredStalled.ClientId, clientId)
	connect.AssertEqual(t, wiredStalled.Stalled, true)

	wiredUnstalled := gobRoundTrip(t, &DeviceRemoteStallExitRpc{
		ClientId: clientId,
		Stalled:  false,
	})
	connect.AssertEqual(t, wiredUnstalled.ClientId, clientId)
	connect.AssertEqual(t, wiredUnstalled.Stalled, false)
}

func TestRpcGobProbeSuiteConfigComplete(t *testing.T) {
	seed := 0
	config := &ProbeSuiteConfig{}
	fillNonZero(t, reflect.ValueOf(config), &seed)

	wired := gobRoundTrip(t, &DeviceRemoteProbeSuiteConfigRpc{
		Config: config,
	})
	connect.AssertEqual(t, wired.Config, config)
}

// TestRpcGobProbeSuiteConfigNilCrossesWire pins the reason the config is
// wrapped at all: a nil config means "use the sdk default", and it has to
// arrive as nil so the handler can substitute the default. gob omits nil
// pointer fields, so decoding it back as a ZERO config would start a run with
// concurrency 0 and no probes enabled instead of the default suite.
func TestRpcGobProbeSuiteConfigNilCrossesWire(t *testing.T) {
	wired := gobRoundTrip(t, &DeviceRemoteProbeSuiteConfigRpc{Config: nil})
	connect.AssertEqual(t, wired.Config, nil)
}

func TestRpcMirrorProbeResultListPopulated(t *testing.T) {
	seed := 0
	results := NewProbeResultList()
	sourceResults := []*ProbeResult{}
	for range 3 {
		result := &ProbeResult{}
		fillNonZero(t, reflect.ValueOf(result), &seed)
		results.Add(result)
		sourceResults = append(sourceResults, result)
	}
	// a nil row must be skipped, not crash the encoder
	results.Add(nil)

	wired := gobRoundTrip(t, &DeviceRemoteProbeResultListRpc{
		Results: newProbeResultListRpc(results),
	})
	resultList := toProbeResultList(wired.Results)
	connect.AssertEqual(t, resultList.Len(), 3)
	for i, result := range sourceResults {
		connect.AssertEqual(t, resultList.Get(i), result)
	}
}

// TestRpcMirrorProbeResultListEmpty is the "no probes have run" shape, which
// is what a probe ui reads before its first run and therefore the single most
// likely thing to cross this bridge. A nil slice must arrive as an empty
// list, never as a nil one.
func TestRpcMirrorProbeResultListEmpty(t *testing.T) {
	// the nil list itself (what a device with no probe state would hand over)
	connect.AssertNotEqual(t, newProbeResultListRpc(nil), nil)
	connect.AssertEqual(t, len(newProbeResultListRpc(nil)), 0)

	wired := gobRoundTrip(t, &DeviceRemoteProbeResultListRpc{
		Results: newProbeResultListRpc(NewProbeResultList()),
	})
	resultList := toProbeResultList(wired.Results)
	connect.AssertNotEqual(t, resultList, nil)
	connect.AssertEqual(t, resultList.Len(), 0)
}

// TestExportedListEmptyMarshalsAsArray pins the fix for the fourth c-abi bug
// this client has found.
//
// Go marshals a nil slice as the document `null`, and every one of these
// lists starts with a nil backing slice. The generated c++ wrapper unwraps a
// list getter straight into a std::vector, and nlohmann throws
// type_error.302 converting `null` to an array -- so on a live session SEVEN
// of eleven list getters threw within milliseconds of the session coming up,
// because at that moment every list is empty.
//
// `[]` is the honest rendering of an empty list and the one every consumer
// can unwrap. The wrapper also tolerates `null` now, but this is the assertion
// that keeps the wire correct rather than merely survivable.
func TestExportedListEmptyMarshalsAsArray(t *testing.T) {
	// one list of each shape that crosses as json: the new probe list, a
	// struct list read at session start, and a scalar list
	emptyLists := map[string]any{
		"ProbeResultList":     NewProbeResultList(),
		"ExitList":            NewExitList(),
		"DestinationExitList": NewDestinationExitList(),
		"StringList":          NewStringList(),
	}
	for name, emptyList := range emptyLists {
		encoded, err := json.Marshal(emptyList)
		connect.AssertEqual(t, err, nil)
		if string(encoded) != "[]" {
			t.Fatalf("%s marshalled empty as %q, want \"[]\" (a `null` document throws type_error.302 in the c++ wrapper)", name, string(encoded))
		}
	}

	// a populated list is unaffected, and both forms decode back
	results := NewProbeResultList()
	results.Add(&ProbeResult{Name: "example.com", Kind: "dns", Ok: true})
	encoded, err := json.Marshal(results)
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, string(encoded), "[]")

	decoded := NewProbeResultList()
	connect.AssertEqual(t, json.Unmarshal([]byte("[]"), decoded), nil)
	connect.AssertEqual(t, decoded.Len(), 0)
	// the wire still accepts a `null` document from an older producer
	connect.AssertEqual(t, json.Unmarshal([]byte("null"), decoded), nil)
	connect.AssertEqual(t, decoded.Len(), 0)
}

// degraded shapes: the local with no multi client, and the remote with no
// service. Neither may return nil for a list, and each action reports the
// documented "nothing happened" value rather than pretending it worked.

func TestAdvancedModeDegradedWithoutMultiClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.AllowProvider = false
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		NewId(),
		settings,
		connect.NewId(),
	)
	connect.AssertEqual(t, err, nil)
	defer deviceLocal.Close()

	// the local path: no multi client to act on
	exitClientId := NewId()
	connect.AssertEqual(t, deviceLocal.DropExit(exitClientId), false)
	connect.AssertEqual(t, deviceLocal.StallExit(exitClientId, true), false)
	connect.AssertEqual(t, deviceLocal.StallExit(exitClientId, false), false)
	connect.AssertEqual(t, deviceLocal.MigrateExit(exitClientId), int32(-1))
	connect.AssertEqual(t, deviceLocal.ProbeAllExits(), int32(0))
	// a safe no-op rather than a panic
	deviceLocal.ShuffleExits()

	// the probe suite does not need a multi client to report its state
	connect.AssertEqual(t, deviceLocal.ProbeSuiteRunning(), false)
	localResults := deviceLocal.GetProbeResults()
	connect.AssertNotEqual(t, localResults, nil)
	connect.AssertEqual(t, localResults.Len(), 0)

	// the remote path with no service at all (the cold launch: the remote
	// exists from login, the service process is not up)
	deviceRemote := &DeviceRemote{}
	connect.AssertEqual(t, deviceRemote.DropExit(exitClientId.IdStr), false)
	connect.AssertEqual(t, deviceRemote.StallExit(exitClientId.IdStr, true), false)
	connect.AssertEqual(t, deviceRemote.StallExit(exitClientId.IdStr, false), false)
	connect.AssertEqual(t, deviceRemote.MigrateExit(exitClientId.IdStr), int32(-1))
	connect.AssertEqual(t, deviceRemote.ProbeAllExits(), int32(0))
	connect.AssertEqual(t, deviceRemote.StartProbeSuite(GetDefaultProbeSuiteConfig()), false)
	connect.AssertEqual(t, deviceRemote.ProbeSuiteRunning(), false)
	deviceRemote.StopProbeSuite()
	// safe no-ops rather than panics with no service
	deviceRemote.ShuffleExits()

	// a malformed id is rejected client side, with the same values
	connect.AssertEqual(t, deviceRemote.DropExit("not-an-id"), false)
	connect.AssertEqual(t, deviceRemote.StallExit("not-an-id", true), false)
	connect.AssertEqual(t, deviceRemote.MigrateExit("not-an-id"), int32(-1))

	// the readout degrades to empty, never nil -- this is the value that
	// reaches parseJson<ProbeResultList> on the c++ side
	remoteResults := deviceRemote.GetProbeResults()
	connect.AssertNotEqual(t, remoteResults, nil)
	connect.AssertEqual(t, remoteResults.Len(), 0)

	encoded, err := json.Marshal(remoteResults)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(encoded), "[]")
}

// TestDeviceRemoteAdvancedModeActionsReachTheLocal asserts each new action
// bridge actually reached the DeviceLocal side rather than merely returning
// without error. No-oping any of the new DeviceLocalRpc handlers must fail
// this.
//
// The observable is the connect-side `[rel]` action log, as for the older
// actions. For the fault injection controls the log also carries the exit id
// (and, for stall, the flag), which is what pins the ARGUMENTS crossing the
// wire: string -> connect.Id -> *Id -> connect, and the bool alongside it.
func TestDeviceRemoteAdvancedModeActionsReachTheLocal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	clientId := connect.NewId()
	instanceId := NewId()

	actionLogger := &testingReliabilityActionLogger{}
	settings := testDeviceLocalSettingsRpc()
	// the capturing logger only reaches connect when logging is enabled
	settings.DisableLogging = false
	settings.ClientSettings.Log = actionLogger
	settings.AllowProvider = false
	settings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		return &rpcLeakTestGenerator{}
	}
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		settings,
		clientId,
	)
	connect.AssertEqual(t, err, nil)
	defer deviceLocal.Close()

	upgradeMuxSettings := connect.DefaultUpgradeMuxSettings()
	upgradeMuxSettings.Dns = nil
	deviceLocal.SetUpgradeMuxSettings(upgradeMuxSettings)

	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		defaultDeviceRpcSettings(),
		clientId,
		testing_deviceRpcDialerDefault(),
	)
	connect.AssertEqual(t, err, nil)
	defer deviceRemote.Close()

	// the actions need a multi client to act on
	deviceLocal.SetConnectLocation(testingReliabilityConnectLocation("advanced mode actions"))
	testingReliabilityWaitFor(t, "the remote reaches the local", func() bool {
		return deviceRemote.GetReliabilitySettings() != nil
	})

	// distinct exit ids per action: a shared id would let one action's log
	// line satisfy another's assertion, and the whole point of these
	// assertions is that each handler reached the local INDEPENDENTLY
	dropClientId := NewId()
	stallClientId := NewId()
	idTail := func(id *Id) string {
		return id.IdStr[len(id.IdStr)-8:]
	}

	// DropExit: the action ran AND the exit id arrived intact. The stub window
	// admits no clients, so the exit is not found and the bridge reports false
	// -- which is itself the return value crossing back.
	connect.AssertEqual(t, deviceRemote.DropExit(dropClientId.IdStr), false)
	testingReliabilityWaitFor(t, "drop_exit reached the local with its exit id", func() bool {
		return actionLogger.contains("drop_exit", "exit="+idTail(dropClientId))
	})

	// StallExit in both directions: stall and unstall must each reach the
	// local, since an unstall that arrives as a stall would leave the exit
	// swallowing packets with no way back. The `[rel]` grammar renders bools
	// as 1/0 (see connect's relValue: `stalled=1` greps cleanly where
	// `stalled=true` is a prefix of nothing useful), so the flag is what
	// distinguishes the two lines.
	connect.AssertEqual(t, deviceRemote.StallExit(stallClientId.IdStr, true), false)
	testingReliabilityWaitFor(t, "stall_exit(stalled) reached the local", func() bool {
		return actionLogger.contains("stall_exit", "exit="+idTail(stallClientId), "stalled=1")
	})

	connect.AssertEqual(t, deviceRemote.StallExit(stallClientId.IdStr, false), false)
	testingReliabilityWaitFor(t, "stall_exit(unstalled) reached the local", func() bool {
		return actionLogger.contains("stall_exit", "exit="+idTail(stallClientId), "stalled=0")
	})

	// the counts the bridge used to drop. Against a stub window these are the
	// documented "nothing to do" values, but they are now VALUES rather than
	// void -- see TestDeviceRemoteProbeSuiteBridge for a non-sentinel result
	// crossing the same machinery.
	connect.AssertEqual(t, deviceRemote.MigrateExit(dropClientId.IdStr), int32(-1))
	connect.AssertEqual(t, deviceRemote.ProbeAllExits(), int32(0))
	testingReliabilityWaitFor(t, "probe_all reached the local", func() bool {
		return actionLogger.contains("probe_all")
	})

	// ShuffleExits is the whole-window action. It reaches the same connect
	// call as Shuffle; the two differ only in what a FAILED call does, which
	// TestDeviceRemoteAdvancedModeActionsAreNeverQueued pins.
	deviceRemote.ShuffleExits()
	testingReliabilityWaitFor(t, "shuffle_exits reached the local", func() bool {
		return actionLogger.contains("shuffle")
	})

	// malformed ids are rejected client-side and never reach the local
	dropCount := actionLogger.count("drop_exit")
	stallCount := actionLogger.count("stall_exit")
	connect.AssertEqual(t, deviceRemote.DropExit("not-an-id"), false)
	connect.AssertEqual(t, deviceRemote.StallExit("not-an-id", true), false)
	select {
	case <-time.After(500 * time.Millisecond):
	}
	connect.AssertEqual(t, actionLogger.count("drop_exit"), dropCount)
	connect.AssertEqual(t, actionLogger.count("stall_exit"), stallCount)
}

// TestDeviceRemoteAdvancedModeActionsAreNeverQueued is the negative of the
// sync-state property every advanced-mode action's doc comment promises.
//
// The reliability SETTINGS bridge deliberately queues: a value set while the
// rpc is down is re-applied on the next sync, because the user is describing
// how the device should behave. An ACTION is the opposite. If a fault
// injection pressed against a dead service were queued, the reconnect --
// which can be minutes later, and is not something the user sees -- would
// drop an exit, stall an exit, or replace every exit under someone who has
// moved on. `DeviceRemote.Shuffle` has exactly that shape (it sets
// `state.Shuffle` on failure and the sync replays it), which is why
// `ShuffleExits` exists alongside it rather than redirecting to it.
//
// The positive property has enforcing tests; without this one, nothing stops
// the next person adding a `self.state.X.Set(...)` to one of these seven and
// every test still passing.
func TestDeviceRemoteAdvancedModeActionsAreNeverQueued(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	clientId := connect.NewId()
	instanceId := NewId()

	// the remote comes up with NO local: every action below is pressed
	// against a dead service
	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		defaultDeviceRpcSettings(),
		clientId,
		testing_deviceRpcDialerDefault(),
	)
	connect.AssertEqual(t, err, nil)
	defer deviceRemote.Close()

	exitClientId := NewId()
	connect.AssertEqual(t, deviceRemote.DropExit(exitClientId.IdStr), false)
	connect.AssertEqual(t, deviceRemote.StallExit(exitClientId.IdStr, true), false)
	connect.AssertEqual(t, deviceRemote.MigrateExit(exitClientId.IdStr), int32(-1))
	connect.AssertEqual(t, deviceRemote.ProbeAllExits(), int32(0))
	connect.AssertEqual(t, deviceRemote.StartProbeSuite(GetDefaultProbeSuiteConfig()), false)
	deviceRemote.StopProbeSuite()
	deviceRemote.ShuffleExits()

	// nothing was recorded to replay. Shuffle is the counter-example and is
	// checked explicitly: it MUST still queue, because its existing callers
	// depend on that, and this test would otherwise pass just as well if
	// ShuffleExits had been redirected to it.
	func() {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()
		connect.AssertEqual(t, deviceRemote.state.Shuffle.IsSet, false)
	}()
	deviceRemote.Shuffle()
	func() {
		deviceRemote.stateLock.Lock()
		defer deviceRemote.stateLock.Unlock()
		connect.AssertEqual(t, deviceRemote.state.Shuffle.IsSet, true)
		// and clear it, so the sync below cannot replay a shuffle this test
		// asked for on purpose
		deviceRemote.state.Shuffle.Unset()
	}()

	// now bring the service up and let the remote sync against it. A queued
	// action would fire HERE, which is the moment the user is not watching.
	actionLogger := &testingReliabilityActionLogger{}
	settings := testDeviceLocalSettingsRpc()
	settings.DisableLogging = false
	settings.ClientSettings.Log = actionLogger
	settings.AllowProvider = false
	settings.GeneratorFunc = func(specs []*connect.ProviderSpec) connect.MultiClientGenerator {
		return &rpcLeakTestGenerator{}
	}
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		instanceId,
		settings,
		clientId,
	)
	connect.AssertEqual(t, err, nil)
	defer deviceLocal.Close()

	upgradeMuxSettings := connect.DefaultUpgradeMuxSettings()
	upgradeMuxSettings.Dns = nil
	deviceLocal.SetUpgradeMuxSettings(upgradeMuxSettings)

	testingForceDeviceRpcResync(deviceRemote)
	testingReliabilityWaitFor(t, "the remote syncs to the local", func() bool {
		return deviceRemote.GetRemoteConnected()
	})
	// the multi client the actions would act on, built AFTER the sync -- the
	// ordering that catches a replay landing on a device that had nothing to
	// act on at sync time
	deviceLocal.SetConnectLocation(testingReliabilityConnectLocation("never queued"))
	testingReliabilityWaitFor(t, "the local has a multi client", func() bool {
		return deviceLocal.GetReliabilitySettings() != nil
	})

	// a live action here proves the log is actually wired: without it, an
	// empty action log would "pass" this test for the wrong reason
	deviceRemote.ProbeAllExits()
	testingReliabilityWaitFor(t, "the action log is live", func() bool {
		return actionLogger.contains("probe_all")
	})

	// and none of the queued-action shapes ever arrived
	connect.AssertEqual(t, actionLogger.count("drop_exit"), 0)
	connect.AssertEqual(t, actionLogger.count("stall_exit"), 0)
	connect.AssertEqual(t, actionLogger.count("migrate_exit"), 0)
	connect.AssertEqual(t, actionLogger.count("shuffle"), 0)

	// ShuffleExits is the one that shares a connect call with the QUEUED
	// Shuffle, so it gets its own live pass: it must reach the local exactly
	// once -- the press below, and not a replay of the Shuffle pressed
	// against the dead service earlier in this test.
	deviceRemote.ShuffleExits()
	testingReliabilityWaitFor(t, "shuffle_exits reached the local", func() bool {
		return actionLogger.contains("shuffle")
	})
	connect.AssertEqual(t, actionLogger.count("shuffle"), 1)
}

// TestDeviceRemoteProbeSuiteBridge runs the probe suite as a suite across the
// bridge: start, poll, read results, stop.
//
// This is the test that proves a return value genuinely crosses the wire
// rather than being confused with a fallback. Every other control here
// reports its "nothing happened" value against a stub window, and those are
// the same values a dead rpc returns. `StartProbeSuite` returns TRUE on a
// fresh device, which no failure path can produce: a down rpc, an
// unresolvable handle and an already-running suite all return false.
//
// The suite is configured with every probe kind off, so it builds zero jobs
// and touches no network -- the run exists to move the running flag, not to
// measure anything.
func TestDeviceRemoteProbeSuiteBridge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	clientId := connect.NewId()
	instanceId := NewId()

	deviceLocal := testingNewReliabilityDeviceLocal(t, networkSpace, byJwt, instanceId, clientId)
	defer deviceLocal.Close()

	deviceRemote, err := newDeviceRemoteWithOverrides(
		networkSpace,
		byJwt,
		instanceId,
		defaultDeviceRpcSettings(),
		clientId,
		testing_deviceRpcDialerDefault(),
	)
	connect.AssertEqual(t, err, nil)
	defer deviceRemote.Close()

	testingReliabilityWaitFor(t, "the remote reaches the local", func() bool {
		return deviceRemote.GetRemoteConnected()
	})

	// before any run: both ends agree nothing is running, and the results
	// list is empty and NOT nil. This is the exact value a probe ui reads on
	// first paint, and the one that used to reach the c++ wrapper as `null`.
	connect.AssertEqual(t, deviceLocal.ProbeSuiteRunning(), false)
	testingReliabilityWaitFor(t, "the remote reports the suite is not running", func() bool {
		return !deviceRemote.ProbeSuiteRunning()
	})
	initialResults := deviceRemote.GetProbeResults()
	connect.AssertNotEqual(t, initialResults, nil)
	connect.AssertEqual(t, initialResults.Len(), 0)
	encoded, err := json.Marshal(initialResults)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(encoded), "[]")

	// The run needs WORK, not just a flag flip. This config used to enable no
	// probes at all -- "the run exists to move the flag" -- and that made the
	// test a race it lost about one run in three in the full suite (it passes
	// alone, where the process is idle and the poll lands inside the window).
	// With no jobs, the run goroutine reaches its `running = false` defer as
	// soon as `newProbeHarness` returns, and the harness normally SUCCEEDS, so
	// it appends no result either: the flag is down within milliseconds and
	// the results list stays empty, leaving nothing for the wait below to ever
	// observe again.
	//
	// Three dns jobs give the run an outcome that outlives it. The tun the
	// harness builds has no egress -- the device's stub multi client drops
	// every packet and nothing leaves the machine, so this is still a
	// no-network test -- so each probe fails at TimeoutMillis and records a
	// result, and the suite keeps its results until the next start.
	config := &ProbeSuiteConfig{
		Concurrency:     1,
		TimeoutMillis:   1000,
		RepeatCount:     1,
		IncludeDns:      true,
		IncludeHttp:     false,
		IncludeDownload: false,
	}

	// TRUE across the bridge -- the assertion no fallback can satisfy
	connect.AssertEqual(t, deviceRemote.StartProbeSuite(config), true)

	// The start reached the DeviceLocal side: only the local owns the suite
	// state, so a result recorded THERE is proof the handler ran there.
	//
	// Wait on the results, not on `ProbeSuiteRunning`. The running flag is a
	// pulse, not a level: it is up only between `StartProbeSuite` and the run
	// goroutine's `running = false` defer, and a poll that misses that window
	// can never see it again. Results are the monotonic signal -- the suite
	// keeps them until the next start -- so this wait cannot be lost to
	// scheduling.
	testingReliabilityWaitFor(t, "the suite recorded a result on the local", func() bool {
		return 0 < deviceLocal.GetProbeResults().Len()
	})

	// stop, and both ends settle on not-running
	deviceRemote.StopProbeSuite()
	testingReliabilityWaitFor(t, "the suite stops on the local", func() bool {
		return !deviceLocal.ProbeSuiteRunning()
	})
	testingReliabilityWaitFor(t, "the remote reports the suite stopped", func() bool {
		return !deviceRemote.ProbeSuiteRunning()
	})

	// results after a run cross the bridge as the same list the local holds
	localResults := deviceLocal.GetProbeResults()
	remoteResults := deviceRemote.GetProbeResults()
	connect.AssertNotEqual(t, remoteResults, nil)
	connect.AssertEqual(t, remoteResults.Len(), localResults.Len())
	// and the run left something behind. This guards the config above: turn
	// every probe back off and the wait for a recorded result stops being
	// satisfiable by the run itself, which is exactly how this test became a
	// race the first time.
	if localResults.Len() == 0 {
		t.Fatal("the run recorded no results -- the probe config produces no work, so the wait above has nothing monotonic to observe")
	}
	for i := range localResults.Len() {
		connect.AssertEqual(t, remoteResults.Get(i), localResults.Get(i))
	}

	// The nil config ("use the sdk default") is deliberately NOT exercised
	// through the rpc here: the default suite names real probe targets, and
	// the nil is resolved on the DeviceLocal side rather than on this one.
	// TestNormalizeProbeSuiteConfig and TestDeviceLocalStartProbeSuiteNilConfig
	// cover that seam directly, including the goroutine that used to deref it.
}

// TestNormalizeProbeSuiteConfig covers the seam every probe config passes
// through, whichever process composed it.
//
// A nil config reaches `buildProbeJobs` on a SPAWNED GOROUTINE, so the nil
// deref it used to cause was not catchable by the cgo boundary's recover: an
// unrecovered goroutine panic aborts the process, and on Windows that process
// is the privileged service. The bounds matter for the same reason -- the
// config crosses a process boundary, so `Concurrency: 1<<30` is a billion
// goroutines asked for by the unprivileged side.
func TestNormalizeProbeSuiteConfig(t *testing.T) {
	// nil is the default, never a zero config (which would run nothing)
	normalized := normalizeProbeSuiteConfig(nil, nil)
	connect.AssertNotEqual(t, normalized, nil)
	connect.AssertEqual(t, normalized, GetDefaultProbeSuiteConfig())

	// an in-range config is unchanged
	sane := &ProbeSuiteConfig{
		Concurrency:       4,
		TimeoutMillis:     15_000,
		RepeatCount:       2,
		IncludeDns:        true,
		DownloadByteCount: 1 << 20,
	}
	connect.AssertEqual(t, normalizeProbeSuiteConfig(sane, nil), sane)

	// the hostile values, each clamped to its ceiling
	hostile := &ProbeSuiteConfig{
		Concurrency:       1 << 30,
		RepeatCount:       1 << 30,
		TimeoutMillis:     1 << 40,
		DownloadByteCount: 1 << 40,
	}
	bounded := normalizeProbeSuiteConfig(hostile, nil)
	connect.AssertEqual(t, bounded.Concurrency, int32(probeMaxConcurrency))
	connect.AssertEqual(t, bounded.RepeatCount, int32(probeMaxRepeatCount))
	connect.AssertEqual(t, bounded.TimeoutMillis, int64(probeMaxTimeoutMillis))
	connect.AssertEqual(t, bounded.DownloadByteCount, int64(probeMaxDownloadByteCount))
	// the caller's struct is not mutated -- a run in flight cannot be
	// retuned by a caller that keeps hold of the config it passed
	connect.AssertEqual(t, hostile.Concurrency, int32(1<<30))

	// a zero timeout is "no deadline", which is how a stalled probe becomes a
	// goroutine that never returns; it gets a floor
	connect.AssertEqual(t, normalizeProbeSuiteConfig(&ProbeSuiteConfig{}, nil).TimeoutMillis, int64(probeMinTimeoutMillis))

	// but a non-positive DownloadByteCount is a real choice ("no download
	// probe"), so it keeps no floor
	connect.AssertEqual(t, normalizeProbeSuiteConfig(&ProbeSuiteConfig{}, nil).DownloadByteCount, int64(0))
	connect.AssertEqual(t, len(buildProbeJobs(normalizeProbeSuiteConfig(&ProbeSuiteConfig{IncludeDownload: true}, nil))), 0)

	// the normalized default builds real jobs, where a zero config builds
	// none -- the difference a lost nil would have erased
	connect.AssertEqual(t, 0 < len(buildProbeJobs(normalizeProbeSuiteConfig(nil, nil))), true)
	connect.AssertEqual(t, len(buildProbeJobs(&ProbeSuiteConfig{})), 0)

	// the wire's nil survives to this seam as nil, since the rpc handler
	// passes it through rather than resolving it
	connect.AssertEqual(t, gobRoundTrip(t, &DeviceRemoteProbeSuiteConfigRpc{Config: nil}).Config, nil)
}

// TestDeviceLocalStartProbeSuiteNilConfig runs the real crash path: a nil
// config through `DeviceLocal.StartProbeSuite` and into the goroutine that
// used to deref it.
//
// If the guard regresses, the panic is unrecovered on a spawned goroutine and
// takes the test binary down rather than failing an assertion -- which is
// precisely what it would do to the privileged service. Reachable with no rpc
// in between via `urnet_device_local_start_probe_suite(handle, NULL)` and
// gomobile's `startProbeSuite(null)`.
func TestDeviceLocalStartProbeSuiteNilConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	networkSpace, byJwt, err := testing_newNetworkSpace(ctx)
	connect.AssertEqual(t, err, nil)

	settings := DefaultDeviceLocalSettings()
	settings.DisableLogging = true
	settings.AllowProvider = false
	deviceLocal, err := newDeviceLocalWithOverrides(
		networkSpace,
		byJwt,
		"",
		"",
		"",
		NewId(),
		settings,
		connect.NewId(),
	)
	connect.AssertEqual(t, err, nil)
	defer deviceLocal.Close()

	// the nil the cgo export and gomobile both let through
	connect.AssertEqual(t, deviceLocal.StartProbeSuite(nil), true)
	// stopped immediately: the default config names real probe targets, and
	// this test is about surviving the nil rather than measuring anything.
	// The probes ride the suite's own tun into a device with no tunnel, so
	// they reach no host network either way.
	deviceLocal.StopProbeSuite()
	testingReliabilityWaitFor(t, "the nil-config suite stops", func() bool {
		return !deviceLocal.ProbeSuiteRunning()
	})

	// and the hostile config the ui process could compose, which must be
	// bounded rather than spawning a billion goroutines in the service
	connect.AssertEqual(t, deviceLocal.StartProbeSuite(&ProbeSuiteConfig{
		Concurrency:   1 << 30,
		RepeatCount:   1 << 30,
		TimeoutMillis: 1 << 40,
	}), true)
	deviceLocal.StopProbeSuite()
	testingReliabilityWaitFor(t, "the clamped suite stops", func() bool {
		return !deviceLocal.ProbeSuiteRunning()
	})
}
