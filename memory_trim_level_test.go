package sdk

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestMobileTrimLevelActionForEveryPlatformLevel(t *testing.T) {
	for _, testCase := range []struct {
		level int64
		want  mobileTrimLevelAction
	}{
		{level: 0, want: mobileTrimLevelActionNone},
		{level: androidTrimMemoryRunningModerate, want: mobileTrimLevelActionArmQuiet},
		{level: androidTrimMemoryRunningLow, want: mobileTrimLevelActionTrim},
		{level: androidTrimMemoryRunningCritical, want: mobileTrimLevelActionTrimForced},
		{level: androidTrimMemoryUiHidden, want: mobileTrimLevelActionNone},
		{level: 30, want: mobileTrimLevelActionNone},
		{level: androidTrimMemoryBackground, want: mobileTrimLevelActionTrim},
		{level: androidTrimMemoryModerate, want: mobileTrimLevelActionTrimForced},
		{level: androidTrimMemoryComplete, want: mobileTrimLevelActionFree},
		{level: 100, want: mobileTrimLevelActionFree},
	} {
		if got := mobileTrimLevelActionFor(testCase.level); got != testCase.want {
			t.Errorf("level %d action = %d, want %d", testCase.level, got, testCase.want)
		}
	}
}

func TestMobileTrimLevelTransitionIsIdempotentAndEscalates(t *testing.T) {
	now := time.Unix(100, 0)
	var state mobileTrimLevelState
	act, state := mobileTrimLevelTransition(mobileTrimLevelActionTrim, state, now, time.Minute)
	if !act {
		t.Fatal("first report did not act")
	}
	if act, _ = mobileTrimLevelTransition(mobileTrimLevelActionTrim, state, now.Add(time.Second), time.Minute); act {
		t.Fatal("repeated level inside the cooldown acted again")
	}
	if act, _ = mobileTrimLevelTransition(mobileTrimLevelActionArmQuiet, state, now.Add(time.Second), time.Minute); act {
		t.Fatal("de-escalation inside the cooldown acted")
	}
	escalated, escalatedState := mobileTrimLevelTransition(mobileTrimLevelActionTrimForced, state, now.Add(time.Second), time.Minute)
	if !escalated || escalatedState.lastAction != mobileTrimLevelActionTrimForced {
		t.Fatalf("escalation = %v %+v, want act at forced", escalated, escalatedState)
	}
	if act, _ = mobileTrimLevelTransition(mobileTrimLevelActionTrim, state, now.Add(time.Minute), time.Minute); !act {
		t.Fatal("repeated level after the cooldown did not act")
	}
	if act, next := mobileTrimLevelTransition(mobileTrimLevelActionNone, state, now, time.Minute); act || next != state {
		t.Fatal("no-op level changed state")
	}
}

type trimLevelCalls struct {
	armQuiet, shed, free int
	trims                []bool
}

func newTestTrimLevelReporter(now *time.Time, started *bool, calls *trimLevelCalls) *mobileTrimLevelReporter {
	return &mobileTrimLevelReporter{
		cooldown:       time.Minute,
		now:            func() time.Time { return *now },
		trimmerStarted: func() bool { return *started },
		armQuiet:       func() { calls.armQuiet += 1 },
		trim: func(force bool) ByteCount {
			calls.trims = append(calls.trims, force)
			return 7
		},
		shed: func() { calls.shed += 1 },
		free: func() { calls.free += 1 },
	}
}

func TestMobileTrimLevelReporterCausesEachReclamation(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		level int64
		want  trimLevelCalls
	}{
		{name: "running moderate arms a quiet pass", level: androidTrimMemoryRunningModerate, want: trimLevelCalls{armQuiet: 1}},
		{name: "running low trims now unforced", level: androidTrimMemoryRunningLow, want: trimLevelCalls{trims: []bool{false}}},
		{name: "running critical sheds and forces", level: androidTrimMemoryRunningCritical, want: trimLevelCalls{shed: 1, trims: []bool{true}}},
		{name: "ui hidden is not pressure", level: androidTrimMemoryUiHidden, want: trimLevelCalls{}},
		{name: "background trims now unforced", level: androidTrimMemoryBackground, want: trimLevelCalls{trims: []bool{false}}},
		{name: "moderate sheds and forces", level: androidTrimMemoryModerate, want: trimLevelCalls{shed: 1, trims: []bool{true}}},
		{name: "complete frees", level: androidTrimMemoryComplete, want: trimLevelCalls{free: 1}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			now := time.Unix(100, 0)
			started := true
			var calls trimLevelCalls
			reporter := newTestTrimLevelReporter(&now, &started, &calls)
			reporter.report(testCase.level)
			// a repeat inside the cooldown is idempotent
			reporter.report(testCase.level)
			if calls.armQuiet != testCase.want.armQuiet || calls.shed != testCase.want.shed ||
				calls.free != testCase.want.free || len(calls.trims) != len(testCase.want.trims) {
				t.Fatalf("calls = %+v, want %+v", calls, testCase.want)
			}
			for i := range calls.trims {
				if calls.trims[i] != testCase.want.trims[i] {
					t.Fatalf("trim force = %v, want %v", calls.trims, testCase.want.trims)
				}
			}
		})
	}
}

func TestMobileTrimLevelReporterForcesCriticalRegardlessOfQuiet(t *testing.T) {
	now := time.Unix(100, 0)
	started := false
	var calls trimLevelCalls
	reporter := newTestTrimLevelReporter(&now, &started, &calls)

	// Before the trimmer starts, the quiet arm is not consumed...
	if got := reporter.report(androidTrimMemoryRunningModerate); got != mobileTrimLevelActionNone || calls.armQuiet != 0 {
		t.Fatalf("pre-start moderate = %d calls=%+v", got, calls)
	}
	started = true
	if got := reporter.report(androidTrimMemoryRunningModerate); got != mobileTrimLevelActionArmQuiet || calls.armQuiet != 1 {
		t.Fatalf("post-start moderate = %d calls=%+v", got, calls)
	}
	// ...and an escalation to critical inside the cooldown still forces a
	// pass at once, with no quiet epoch in between.
	now = now.Add(time.Second)
	if got := reporter.report(androidTrimMemoryRunningCritical); got != mobileTrimLevelActionTrimForced ||
		calls.shed != 1 || len(calls.trims) != 1 || !calls.trims[0] {
		t.Fatalf("critical = %d calls=%+v", got, calls)
	}
	if mobileTrimLevelDropped.Load() != 7 {
		t.Fatalf("dropped = %d, want 7", mobileTrimLevelDropped.Load())
	}
}

// The mechanism end to end with the production trim: a free list holding a
// burst's worth of returned packet buffers is reduced to the warm set by one
// RUNNING_CRITICAL report.
func TestReportMemoryTrimLevelCriticalRebuildsPoolsToWarm(t *testing.T) {
	const mib = int64(1024 * 1024)
	connect.ResizeMessagePools(8*mib, 2*mib)
	connect.ClearMessagePools()
	t.Cleanup(func() {
		connect.ClearMessagePools()
		connect.ResizeMessagePools(
			connect.InitialMessagePoolByteCount/2,
			connect.InitialMessagePoolByteCount/2,
		)
	})
	const fullPacketSize = 2048
	messages := make([][]byte, 3*mib/fullPacketSize)
	for i := range messages {
		messages[i] = connect.MessagePoolGet(fullPacketSize)
	}
	for _, message := range messages {
		connect.MessagePoolReturn(message)
	}
	warm := trimWarmByteCountForTest()
	before := connect.GetMessagePoolAggregateStats().PacketRetainedByteCount
	if before <= warm+ByteCount(mib) {
		t.Fatalf("setup retained %d, want above warm %d plus a MiB", before, warm)
	}

	now := time.Unix(100, 0)
	started := true
	reporter := &mobileTrimLevelReporter{
		cooldown:       time.Minute,
		now:            func() time.Time { return now },
		trimmerStarted: func() bool { return started },
		armQuiet:       func() {},
		trim:           trimMemory,
		shed:           connect.ShedMemory,
		free:           FreeMemory,
	}
	if got := reporter.report(androidTrimMemoryRunningCritical); got != mobileTrimLevelActionTrimForced {
		t.Fatalf("action = %d, want forced", got)
	}
	after := connect.GetMessagePoolAggregateStats().PacketRetainedByteCount
	if warm < after {
		t.Fatalf("retained after critical = %d, want <= warm %d (before %d)", after, warm, before)
	}
	if dropped := ByteCount(mobileTrimLevelDropped.Load()); dropped < before-warm {
		t.Fatalf("recorded drop = %d, want >= %d", dropped, before-warm)
	}
}

func trimWarmByteCountForTest() ByteCount {
	if mobileRuntime() {
		return mobilePacketPoolWarmByteCount
	}
	return ByteCount(1024 * 1024)
}
