//go:build !ios_extension && !js

package sdk

// MetricKit's reports of why the app's processes ended, written to the exit
// diagnostics file (exit_diagnostics.go) for "send feedback with logs".
//
// MetricKit hands the app's subscriber two kinds of payload. A diagnostic
// payload holds the crash, hang, cpu exception, disk write exception and app
// launch diagnostics of a time range; iOS 15+ and macOS 12+ deliver one at the
// next launch after a crash. A metric payload, about daily, holds among much
// else the exit metric: how many times the app exited in the foreground and in
// the background, by reason. A memory limit kill (jetsam) never makes a crash
// report, so only the exit counts show one. Android logs its process exit
// records the same way at app start (the android app's ProcessExitDiagnostics).
//
// The app hands each payload's json (jsonRepresentation) to
// LogMetricKitDiagnosticPayload or LogMetricKitMetricPayload and keeps no logic
// of its own. Of a payload these keep only what tells exits apart: the
// record's kind, the mach exception type and code, the signal, the termination
// reason (sanitized and bounded), the objective-c exception's name and class,
// the hang, launch, cpu and write measures, the exit counts, the app version
// and build, the os version, the bundle id of the process that ended, and the
// payload's time range. Never a call stack, a memory address, a path, an id,
// the device type, the region, or free text without a bound. Each record is
// one line of at most exitDiagnosticsLineMaxLength bytes, and one payload adds
// at most exitDiagnosticsPayloadMaxLineCount of them.
//
// A payload is logged once. MetricKit keeps its past payloads for days and the
// app reads them at each start besides the new ones it is handed, so what was
// logged is recorded beside the file (exitDiagnosticsState) and a payload in
// that record is skipped: the watermark Android keeps of the last record it
// logged, made fit for payloads that can come out of order.

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"
)

const (
	// What has been logged to the exit diagnostics file, beside it. Never
	// uploaded: the inventory takes only the file itself.
	exitDiagnosticsStateFileName = "exit-diagnostics-state.json"
	// The largest record of what has been logged that is read.
	exitDiagnosticsStateMaxByteCount = 256 * 1024
	// The logged payloads the record keeps, and for how long before the newest
	// of them: MetricKit keeps its past payloads for 7 days.
	exitDiagnosticsStateMaxPayloadCount = 256
	exitDiagnosticsStateRetentionMillis = int64(14 * 24 * 60 * 60 * 1000)
	// An end time further ahead of the clock than this counts as the clock plus
	// this, so that one wrong time cannot lift the floor over every payload to
	// come.
	exitDiagnosticsStateMaxAheadMillis = int64(24 * 60 * 60 * 1000)

	// The longest line, in bytes. Its fields are each bounded so that together
	// they stay under it.
	exitDiagnosticsLineMaxLength = 1024
	// The most lines one payload adds. One more line counts the records past
	// them.
	exitDiagnosticsPayloadMaxLineCount = 32
	// The longest termination reason a line keeps, in bytes, as Android keeps
	// 200 of an exit's description (PROCESS_EXIT_DESCRIPTION_MAX_LENGTH).
	exitDiagnosticsReasonMaxLength = 200
	// The most of a termination reason that is read. The system's text can be a
	// whole report.
	exitDiagnosticsReasonReadMaxLength = 4096
	// A payload larger than this is not read. One with the call stacks of a
	// crash is tens of KiB.
	exitDiagnosticsPayloadMaxByteCount = 16 * 1024 * 1024
)

// The kind of a MetricKit payload, which is part of its digest.
type metricKitPayloadKind string

const (
	metricKitDiagnosticPayloadKind metricKitPayloadKind = "diagnostic"
	metricKitMetricPayloadKind     metricKitPayloadKind = "metric"
)

// Logs one MetricKit diagnostic payload, the json of an MXDiagnosticPayload
// (jsonRepresentation), to the exit diagnostics file of this process's log
// directory: a line for each crash, hang, cpu exception, disk write exception
// and app launch diagnostic in it, unless the payload was logged before.
// beginMillis and endMillis are the payload's timeStampBegin and timeStampEnd
// in unix milliseconds, or 0 to read them from the json. Returns the number of
// lines written: 0 when the payload was logged before, is not a payload or
// cannot be written, and before a log directory is set (SetLogDirForProcess).
//
// Safe to call from any thread (MetricKit calls its subscriber on a background
// queue), and never panics: a diagnostic must not be what breaks the app. Not
// in the c abi (cgo/gen): there is no MetricKit on windows or linux.
func LogMetricKitDiagnosticPayload(payloadJson string, beginMillis int64, endMillis int64) int {
	return logMetricKitPayload(metricKitDiagnosticPayloadKind, payloadJson, beginMillis, endMillis, time.Now())
}

// Logs one MetricKit metric payload, the json of an MXMetricPayload
// (jsonRepresentation), to the exit diagnostics file of this process's log
// directory: its exit metric, the app's exits in the foreground and in the
// background by reason, a line for each, unless the payload was logged before.
// A payload without an exit metric adds no line. As
// LogMetricKitDiagnosticPayload otherwise.
func LogMetricKitMetricPayload(payloadJson string, beginMillis int64, endMillis int64) int {
	return logMetricKitPayload(metricKitMetricPayloadKind, payloadJson, beginMillis, endMillis, time.Now())
}

// Serializes the reads and writes of the exit diagnostics file and of its
// record in this process. Only the app process writes them.
var exitDiagnosticsLock sync.Mutex

func logMetricKitPayload(kind metricKitPayloadKind, payloadJson string, beginMillis int64, endMillis int64, now time.Time) (lineCount int) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[exit-diagnostics]a MetricKit %s payload was not logged: %v", kind, r)
			lineCount = 0
		}
	}()

	dir := GetLogDir()
	if dir == "" {
		return 0
	}

	exitDiagnosticsLock.Lock()
	defer exitDiagnosticsLock.Unlock()

	state := readExitDiagnosticsState(dir)
	lines, nextState, ok := metricKitExitDiagnosticLines(kind, payloadJson, beginMillis, endMillis, state, now.UnixMilli())
	if !ok {
		return 0
	}
	if 0 < len(lines) {
		if err := appendExitDiagnosticsLines(dir, lines); err != nil {
			glog.Infof("[exit-diagnostics]a MetricKit %s payload was not logged: %s", kind, exitDiagnosticsErrorText(err))
			return 0
		}
	}
	// the lines are written before the record that says so: a failure between
	// the two writes logs the payload twice, never not at all
	if err := writeExitDiagnosticsState(dir, nextState); err != nil {
		glog.Infof("[exit-diagnostics]the record of what was logged was not written: %s", exitDiagnosticsErrorText(err))
	}
	glog.Infof("[exit-diagnostics]logged %d lines of a MetricKit %s payload", len(lines), kind)
	return len(lines)
}

// The lines one MetricKit payload adds to the exit diagnostics file, and the
// record of what was logged once they are written, from the record before and
// the clock (unix milliseconds, which only bounds the record). ok is false when
// there is nothing to write or record: the json is not a json object, it is
// larger than exitDiagnosticsPayloadMaxByteCount, or the payload was logged
// before. A payload without records is recorded with no lines, so it is not
// read again. Pure, so the same arguments give the same lines.
func metricKitExitDiagnosticLines(
	kind metricKitPayloadKind,
	payloadJson string,
	beginMillis int64,
	endMillis int64,
	state exitDiagnosticsState,
	nowMillis int64,
) (lines []string, nextState exitDiagnosticsState, ok bool) {
	if exitDiagnosticsPayloadMaxByteCount < len(payloadJson) {
		return nil, state, false
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payloadJson), &payload); err != nil || payload == nil {
		return nil, state, false
	}
	if beginMillis <= 0 {
		beginMillis = metricKitTimeMillis(payload["timeStampBegin"])
	}
	if endMillis <= 0 {
		endMillis = metricKitTimeMillis(payload["timeStampEnd"])
	}

	digest := metricKitPayloadDigest(kind, payloadJson)
	if state.logged(digest, endMillis, nowMillis) {
		return nil, state, false
	}

	timeRange := exitDiagnosticsTime(beginMillis) + ".." + exitDiagnosticsTime(endMillis)
	switch kind {
	case metricKitDiagnosticPayloadKind:
		lines = metricKitDiagnosticLines(payload, timeRange)
	case metricKitMetricPayloadKind:
		lines = metricKitExitMetricLines(payload, timeRange)
	}
	return lines, state.with(digest, endMillis, nowMillis), true
}

// A payload's digest, to know it again: its kind and its json without the
// space between tokens, so the same payload printed twice has one digest.
func metricKitPayloadDigest(kind metricKitPayloadKind, payloadJson string) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(payloadJson)); err != nil {
		compact.Reset()
		compact.WriteString(payloadJson)
	}
	hash := sha256.New()
	hash.Write([]byte(kind))
	hash.Write([]byte{0})
	hash.Write(compact.Bytes())
	return hex.EncodeToString(hash.Sum(nil))[:32]
}

// A MetricKit time stamp of the json in unix milliseconds, 0 when it is not
// one. The app passes the payload's own times; this reads the json's for a
// caller that does not.
func metricKitTimeMillis(value json.RawMessage) int64 {
	text, ok := jsonText(value)
	if !ok || 64 < len(text) {
		return 0
	}
	text = strings.TrimSpace(text)
	for _, layout := range []string{
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05 -07:00",
		time.RFC3339Nano,
		// no zone: utc
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

// A time as a line shows it: utc to the second, "unknown" when there is none.
func exitDiagnosticsTime(millis int64) string {
	if millis <= 0 {
		return "unknown"
	}
	return time.UnixMilli(millis).UTC().Format("2006-01-02T15:04:05Z")
}

// The diagnostic kinds of a payload: the key of each one's array and the name
// its lines give it, in the order they are written. An array under another key
// that ends in Diagnostics, which a later os may add, is written after them,
// named from its key (metricKitDiagnosticKindName).
var metricKitDiagnosticKinds = []struct {
	key  string
	name string
}{
	{"crashDiagnostics", "crash"},
	{"hangDiagnostics", "hang"},
	{"cpuExceptionDiagnostics", "cpu_exception"},
	{"diskWriteExceptionDiagnostics", "disk_write_exception"},
	{"appLaunchDiagnostics", "app_launch"},
}

// The name of the diagnostics under a key a later os adds, such as
// memoryExceptionDiagnostics: the key without Diagnostics, from camel case to
// snake case, "" unless that is 1 to 24 ascii letters.
func metricKitDiagnosticKindName(key string) string {
	base, ok := strings.CutSuffix(key, "Diagnostics")
	if !ok || base == "" || 24 < len(base) {
		return ""
	}
	var name strings.Builder
	for i := 0; i < len(base); i += 1 {
		c := base[i]
		switch {
		case 'a' <= c && c <= 'z':
			name.WriteByte(c)
		case 'A' <= c && c <= 'Z':
			if 0 < i {
				name.WriteByte('_')
			}
			name.WriteByte(c - 'A' + 'a')
		default:
			return ""
		}
	}
	return name.String()
}

// A line for each diagnostic of a diagnostic payload, the kinds in a fixed
// order, at most exitDiagnosticsPayloadMaxLineCount and then a line that counts
// the rest.
func metricKitDiagnosticLines(payload map[string]json.RawMessage, timeRange string) []string {
	type diagnostics struct {
		name    string
		records []json.RawMessage
	}
	payloadDiagnostics := []diagnostics{}
	knownKeys := map[string]bool{}
	for _, kind := range metricKitDiagnosticKinds {
		knownKeys[kind.key] = true
		if records := jsonArray(payload[kind.key]); 0 < len(records) {
			payloadDiagnostics = append(payloadDiagnostics, diagnostics{kind.name, records})
		}
	}
	otherKeys := []string{}
	for key := range payload {
		if !knownKeys[key] && strings.HasSuffix(key, "Diagnostics") {
			otherKeys = append(otherKeys, key)
		}
	}
	slices.Sort(otherKeys)
	for _, key := range otherKeys {
		name := metricKitDiagnosticKindName(key)
		if name == "" {
			continue
		}
		if records := jsonArray(payload[key]); 0 < len(records) {
			payloadDiagnostics = append(payloadDiagnostics, diagnostics{name, records})
		}
	}

	lines := []string{}
	omittedCount := 0
	for _, kindDiagnostics := range payloadDiagnostics {
		for _, record := range kindDiagnostics.records {
			if exitDiagnosticsPayloadMaxLineCount <= len(lines) {
				omittedCount += 1
				continue
			}
			fields := jsonObject(record)
			if fields == nil {
				continue
			}
			lines = append(lines, metricKitDiagnosticLine(kindDiagnostics.name, fields, timeRange))
		}
	}
	if 0 < omittedCount {
		line := newExitDiagnosticsLine(timeRange, "omitted")
		line.field("records", strconv.Itoa(omittedCount))
		lines = append(lines, line.String())
	}
	return lines
}

// One diagnostic's line, from the diagnostic's json object.
func metricKitDiagnosticLine(name string, fields map[string]json.RawMessage, timeRange string) string {
	// a diagnostic's facts are in its diagnosticMetaData; the record's own keys
	// are read after them, should a later os move one
	metaData := jsonObject(fields["diagnosticMetaData"])
	if metaData == nil {
		metaData = jsonObject(fields["metaData"])
	}
	value := func(keys ...string) json.RawMessage {
		for _, key := range keys {
			if v, ok := metaData[key]; ok {
				return v
			}
		}
		for _, key := range keys {
			if v, ok := fields[key]; ok {
				return v
			}
		}
		return nil
	}

	line := newExitDiagnosticsLine(timeRange, name)
	line.field("process", exitDiagnosticsBundleId(value("bundleIdentifier")))
	line.field("app", exitDiagnosticsVersion(value("appVersion", "applicationVersion")))
	line.field("build", exitDiagnosticsVersion(value("appBuildVersion", "applicationBuildVersion")))
	line.quotedField("os", exitDiagnosticsOsVersion(value("osVersion")))
	if exceptionType, ok := exitDiagnosticsSmallInteger(value("exceptionType")); ok {
		line.field("exception_type", machExceptionTypeName(exceptionType)+"("+strconv.Itoa(exceptionType)+")")
	}
	if exceptionCode, ok := jsonIntegerText(value("exceptionCode")); ok {
		line.field("exception_code", exitDiagnosticsExceptionCode(exceptionCode))
	}
	if signal, ok := exitDiagnosticsSmallInteger(value("signal")); ok {
		line.field("signal", darwinSignalName(signal)+"("+strconv.Itoa(signal)+")")
	}
	// an uncaught objective-c exception: its name and class, never its message
	if exceptionReason := jsonObject(value("exceptionReason")); exceptionReason != nil {
		line.field("objc_exception", exitDiagnosticsIdentifier(exceptionReason["exceptionName"]))
		line.field("objc_class", exitDiagnosticsIdentifier(exceptionReason["className"]))
	}
	line.quotedField("hang_duration", exitDiagnosticsMeasure(value("hangDuration")))
	line.quotedField("launch_duration", exitDiagnosticsMeasure(value("launchDuration")))
	line.quotedField("cpu_time", exitDiagnosticsMeasure(value("totalCPUTime")))
	line.quotedField("sampled_time", exitDiagnosticsMeasure(value("totalSampledTime")))
	line.quotedField("writes", exitDiagnosticsMeasure(value("writesCaused", "totalWritesCaused")))
	if reason, ok := jsonText(value("terminationReason")); ok {
		line.quotedField("termination_reason", sanitizeExitDiagnosticsReason(reason))
	}
	return line.String()
}

// The reasons an exit metric counts exits by: the json key of each count and
// the name a line gives it, in the order a line lists them.
// MXForegroundExitData and MXBackgroundExitData name most of them; the json
// carries the background timeouts too.
var metricKitExitReasons = []struct {
	key  string
	name string
}{
	{"cumulativeNormalAppExitCount", "normal"},
	{"cumulativeMemoryResourceLimitExitCount", "memory_resource_limit"},
	{"cumulativeMemoryPressureExitCount", "memory_pressure"},
	{"cumulativeCPUResourceLimitExitCount", "cpu_resource_limit"},
	{"cumulativeBadAccessExitCount", "bad_access"},
	{"cumulativeIllegalInstructionExitCount", "illegal_instruction"},
	{"cumulativeAbnormalExitCount", "abnormal"},
	{"cumulativeAppWatchdogExitCount", "app_watchdog"},
	{"cumulativeSuspendedWithLockedFileExitCount", "suspended_with_locked_file"},
	{"cumulativeBackgroundTaskAssertionTimeoutExitCount", "background_task_assertion_timeout"},
	{"cumulativeBackgroundURLSessionCompletionTimeoutExitCount", "background_url_session_completion_timeout"},
	{"cumulativeBackgroundFetchCompletionTimeoutExitCount", "background_fetch_completion_timeout"},
}

// A count a later os adds, by its key: cumulative, 1 to 64 ascii letters, then
// ExitCount. It is summed into other.
var metricKitOtherExitCountKeyPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^cumulative[A-Za-z]{1,64}ExitCount$`)
})

// The lines of a metric payload's exit metric: one for the exits in the
// foreground and one for those in the background, each with the total and
// every reason counted, none when the payload has no exit metric.
func metricKitExitMetricLines(payload map[string]json.RawMessage, timeRange string) []string {
	exitMetric := jsonObject(payload["applicationExitMetrics"])
	if exitMetric == nil {
		return nil
	}
	metaData := jsonObject(payload["metaData"])
	value := func(keys ...string) json.RawMessage {
		// the payload's own keys first: its app version is the latest of the
		// range
		for _, key := range keys {
			if v, ok := payload[key]; ok {
				return v
			}
		}
		for _, key := range keys {
			if v, ok := metaData[key]; ok {
				return v
			}
		}
		return nil
	}

	lines := []string{}
	for _, side := range []struct {
		key  string
		name string
	}{
		{"foregroundExitData", "foreground_exits"},
		{"backgroundExitData", "background_exits"},
	} {
		exitData := jsonObject(exitMetric[side.key])
		if exitData == nil {
			continue
		}

		knownKeys := map[string]bool{}
		total := uint64(0)
		reasonCounts := []string{}
		for _, reason := range metricKitExitReasons {
			knownKeys[reason.key] = true
			if count, ok := exitDiagnosticsCount(exitData[reason.key]); ok && 0 < count {
				total = saturatingAdd(total, count)
				reasonCounts = append(reasonCounts, reason.name+"="+strconv.FormatUint(count, 10))
			}
		}
		otherCount := uint64(0)
		for key, raw := range exitData {
			if knownKeys[key] || !metricKitOtherExitCountKeyPattern().MatchString(key) {
				continue
			}
			if count, ok := exitDiagnosticsCount(raw); ok {
				otherCount = saturatingAdd(otherCount, count)
			}
		}
		if 0 < otherCount {
			total = saturatingAdd(total, otherCount)
			reasonCounts = append(reasonCounts, "other="+strconv.FormatUint(otherCount, 10))
		}

		line := newExitDiagnosticsLine(timeRange, side.name)
		line.field("app", exitDiagnosticsVersion(value("appVersion", "latestApplicationVersion", "applicationVersion")))
		line.field("build", exitDiagnosticsVersion(value("appBuildVersion", "applicationBuildVersion")))
		if multiple, ok := jsonBool(payload["includesMultipleApplicationVersions"]); ok && multiple {
			line.field("app_versions", "multiple")
		}
		line.quotedField("os", exitDiagnosticsOsVersion(value("osVersion")))
		line.field("total", strconv.FormatUint(total, 10))
		for _, reasonCount := range reasonCounts {
			line.raw(reasonCount)
		}
		lines = append(lines, line.String())
	}
	return lines
}

func saturatingAdd(a uint64, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}

// One line of the exit diagnostics file: the payload's time range, "metrickit"
// and the record's kind, then its fields as key=value, quoted where a value
// can hold a space. Every value is sanitized before it is added, and none holds
// a quote or a line break.
type exitDiagnosticsLine struct {
	builder strings.Builder
}

func newExitDiagnosticsLine(timeRange string, kind string) *exitDiagnosticsLine {
	line := &exitDiagnosticsLine{}
	line.builder.WriteString(timeRange)
	line.builder.WriteString(" metrickit ")
	line.builder.WriteString(kind)
	return line
}

// Adds key=value, or nothing when value is "".
func (self *exitDiagnosticsLine) field(key string, value string) {
	if value == "" {
		return
	}
	self.raw(key + "=" + value)
}

// Adds key="value", or nothing when value is "".
func (self *exitDiagnosticsLine) quotedField(key string, value string) {
	if value == "" {
		return
	}
	self.raw(key + "=\"" + value + "\"")
}

func (self *exitDiagnosticsLine) raw(text string) {
	self.builder.WriteByte(' ')
	self.builder.WriteString(text)
}

// The line, cut between fields to exitDiagnosticsLineMaxLength should its
// fields together pass it.
func (self *exitDiagnosticsLine) String() string {
	line := self.builder.String()
	if len(line) <= exitDiagnosticsLineMaxLength {
		return line
	}
	limit := exitDiagnosticsLineMaxLength - len(" ...")
	cut := strings.LastIndexByte(line[:limit], ' ')
	if cut <= 0 {
		cut = limit
	}
	return line[:cut] + " ..."
}

// The mach exception types (sys/exception_types.h) by value.
var machExceptionTypeNames = map[int]string{
	1:  "EXC_BAD_ACCESS",
	2:  "EXC_BAD_INSTRUCTION",
	3:  "EXC_ARITHMETIC",
	4:  "EXC_EMULATION",
	5:  "EXC_SOFTWARE",
	6:  "EXC_BREAKPOINT",
	7:  "EXC_SYSCALL",
	8:  "EXC_MACH_SYSCALL",
	9:  "EXC_RPC_ALERT",
	10: "EXC_CRASH",
	11: "EXC_RESOURCE",
	12: "EXC_GUARD",
	13: "EXC_CORPSE_NOTIFY",
}

func machExceptionTypeName(exceptionType int) string {
	if name, ok := machExceptionTypeNames[exceptionType]; ok {
		return name
	}
	return "unknown"
}

// The darwin signals (sys/signal.h) by value.
var darwinSignalNames = map[int]string{
	1:  "SIGHUP",
	2:  "SIGINT",
	3:  "SIGQUIT",
	4:  "SIGILL",
	5:  "SIGTRAP",
	6:  "SIGABRT",
	7:  "SIGEMT",
	8:  "SIGFPE",
	9:  "SIGKILL",
	10: "SIGBUS",
	11: "SIGSEGV",
	12: "SIGSYS",
	13: "SIGPIPE",
	14: "SIGALRM",
	15: "SIGTERM",
	16: "SIGURG",
	17: "SIGSTOP",
	18: "SIGTSTP",
	19: "SIGCONT",
	20: "SIGCHLD",
	21: "SIGTTIN",
	22: "SIGTTOU",
	23: "SIGIO",
	24: "SIGXCPU",
	25: "SIGXFSZ",
	26: "SIGVTALRM",
	27: "SIGPROF",
	28: "SIGWINCH",
	29: "SIGINFO",
	30: "SIGUSR1",
	31: "SIGUSR2",
}

func darwinSignalName(signal int) string {
	if name, ok := darwinSignalNames[signal]; ok {
		return name
	}
	return "unknown"
}

// An exception type or a signal: an integer from 0 to 65535.
func exitDiagnosticsSmallInteger(value json.RawMessage) (int, bool) {
	text, ok := jsonIntegerText(value)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 0 || 0xffff < n {
		return 0, false
	}
	return n, true
}

// An exception code as a line shows it, in hexadecimal when it fits 32 bits.
// A wider code can hold an address, and an address never leaves the device.
func exitDiagnosticsExceptionCode(text string) string {
	code, err := strconv.ParseUint(text, 10, 64)
	if err != nil || 0xffffffff < code {
		return "omitted"
	}
	return "0x" + strconv.FormatUint(code, 16)
}

// An exit count, a whole number.
func exitDiagnosticsCount(value json.RawMessage) (uint64, bool) {
	text, ok := jsonIntegerText(value)
	if !ok {
		return 0, false
	}
	count, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, false
	}
	return count, true
}

// Whether every byte of text is one that allowed accepts.
func exitDiagnosticsAllowed(text string, allowed func(c byte) bool) bool {
	for i := 0; i < len(text); i += 1 {
		if !allowed(text[i]) {
			return false
		}
	}
	return true
}

func isAsciiLetterOrDigit(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

// An app version or build, such as 2026.11.3 or 4512: at most 32 ascii letters,
// digits, '.', '_', '+' and '-'. Anything else leaves the field out: a value
// with any other character is not a version.
func exitDiagnosticsVersion(value json.RawMessage) string {
	text, ok := jsonText(value)
	if !ok || 32 < len(text) {
		return ""
	}
	if !exitDiagnosticsAllowed(text, func(c byte) bool {
		return isAsciiLetterOrDigit(c) || c == '.' || c == '_' || c == '+' || c == '-'
	}) {
		return ""
	}
	return text
}

// A bundle id, such as network.ur: at most 64 ascii letters, digits, '.' and
// '-', else left out.
func exitDiagnosticsBundleId(value json.RawMessage) string {
	text, ok := jsonText(value)
	if !ok || 64 < len(text) {
		return ""
	}
	if !exitDiagnosticsAllowed(text, func(c byte) bool {
		return isAsciiLetterOrDigit(c) || c == '.' || c == '-'
	}) {
		return ""
	}
	return text
}

// An os version, such as iPhone OS 26.1 (23B85): at most 48 ascii letters,
// digits, spaces, '.', '_', '(', ')' and '-', else left out.
func exitDiagnosticsOsVersion(value json.RawMessage) string {
	text, ok := jsonText(value)
	if !ok || 48 < len(text) {
		return ""
	}
	if !exitDiagnosticsAllowed(text, func(c byte) bool {
		return isAsciiLetterOrDigit(c) || c == ' ' || c == '.' || c == '_' || c == '(' || c == ')' || c == '-'
	}) {
		return ""
	}
	return strings.TrimSpace(text)
}

// An objective-c exception's name or class, such as NSInvalidArgumentException:
// at most 64 ascii letters, digits, '_', '.' and '$', else left out.
func exitDiagnosticsIdentifier(value json.RawMessage) string {
	text, ok := jsonText(value)
	if !ok || 64 < len(text) {
		return ""
	}
	if !exitDiagnosticsAllowed(text, func(c byte) bool {
		return isAsciiLetterOrDigit(c) || c == '_' || c == '.' || c == '$'
	}) {
		return ""
	}
	return text
}

// A measure with its unit, such as 20 sec or 1,500 KB, else left out.
var exitDiagnosticsMeasurePattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[0-9][0-9.,]{0,15}(?: ?[A-Za-z]{1,8})?$`)
})

func exitDiagnosticsMeasure(value json.RawMessage) string {
	text, ok := jsonText(value)
	if !ok || 32 < len(text) {
		return ""
	}
	text = strings.TrimSpace(text)
	if !exitDiagnosticsMeasurePattern().MatchString(text) {
		return ""
	}
	return text
}

// What the termination reason sanitizer replaces, besides the network
// addresses and ids the export's redactor finds (diagnostics_redact.go).
type exitDiagnosticsReasonPatterns struct {
	// a mac address
	mac *regexp.Regexp
	// 0x and more hex digits than a 32 bit code has: an address
	memoryAddress *regexp.Regexp
	// a run of hex digits, which exitDiagnosticsHexRun judges
	hexRun *regexp.Regexp
}

// Compiled at the first termination reason, in the app process only.
var exitDiagnosticsReasonPatternsOnce = sync.OnceValue(func() *exitDiagnosticsReasonPatterns {
	return &exitDiagnosticsReasonPatterns{
		mac:           regexp.MustCompile(`\b[0-9A-Fa-f]{2}(?:[:-][0-9A-Fa-f]{2}){5}\b`),
		memoryAddress: regexp.MustCompile(`0[xX][0-9A-Fa-f]{9,}`),
		hexRun:        regexp.MustCompile(`[0-9A-Fa-f]+`),
	}
})

// A run of hex digits as a reason keeps it: 9 or more decimal digits are a
// "<number>", an address, a time or an id; 12 or more hex digits are a
// "<hex>", an id or a hash, such as a uuid without its dashes; a shorter run
// stays, a code, a pid or a version.
func exitDiagnosticsHexRun(run string) string {
	if 12 <= len(run) {
		if strings.Trim(run, "0123456789") == "" {
			return "<number>"
		}
		return "<hex>"
	}
	if 9 <= len(run) && strings.Trim(run, "0123456789") == "" {
		return "<number>"
	}
	return run
}

// A termination reason as a line keeps it: one line of printable ascii, with
// each path, email address, uuid, mac address, network address, memory address
// and long number or hex run replaced by a word in angle brackets that says
// what was there, and at most exitDiagnosticsReasonMaxLength bytes, cut between
// words and ending in "...". "" when nothing is left.
//
// The system writes the reason, but a dyld or a guard reason can name a file
// in the app's container, whose path holds the install's uuid, or an address.
// Short codes stay: the watchdog's 0x8badf00d, a pid, a version.
func sanitizeExitDiagnosticsReason(reason string) string {
	cut := false
	if exitDiagnosticsReasonReadMaxLength < len(reason) {
		reason = reason[:exitDiagnosticsReasonReadMaxLength]
		cut = true
	}
	// one line of printable ascii: line breaks, every other control
	// character, and any other text (an invalid byte too) become spaces
	reason = strings.Map(func(r rune) rune {
		if r < 0x20 || 0x7e < r {
			return ' '
		}
		return r
	}, reason)
	words := strings.Fields(reason)
	if cut && 0 < len(words) {
		// the cut can end inside a word: drop it, so that no piece of a path
		// or an id is left without what marks it as one
		words = words[:len(words)-1]
	}
	for i, word := range words {
		switch {
		case strings.ContainsAny(word, `/\`) || strings.HasPrefix(word, "~"):
			// a path or a url, whatever it names
			words[i] = "<path>"
		case strings.Contains(word, "@"):
			words[i] = "<email>"
		}
	}
	reason = strings.Join(words, " ")

	patterns := exitDiagnosticsReasonPatternsOnce()
	reason = redactUUIDPattern.ReplaceAllString(reason, "<uuid>")
	reason = patterns.mac.ReplaceAllString(reason, "<mac>")
	reason = redactAddrBytesPattern.ReplaceAllStringFunc(reason, maskExitDiagnosticsNetworkAddress)
	reason = redactIPv6Pattern.ReplaceAllStringFunc(reason, maskExitDiagnosticsNetworkAddress)
	reason = redactIPv4Pattern.ReplaceAllStringFunc(reason, maskExitDiagnosticsNetworkAddress)
	reason = patterns.memoryAddress.ReplaceAllString(reason, "<address>")
	reason = patterns.hexRun.ReplaceAllStringFunc(reason, exitDiagnosticsHexRun)
	// the line quotes the reason
	reason = strings.ReplaceAll(reason, `"`, `'`)

	if exitDiagnosticsReasonMaxLength < len(reason) {
		limit := exitDiagnosticsReasonMaxLength - len("...")
		end := strings.LastIndexByte(reason[:limit+1], ' ')
		if end <= 0 {
			end = limit
		}
		reason = strings.TrimRight(reason[:end], " ") + "..."
	}
	return reason
}

// Masks a network address that the export's redactor masks
// (logRedactor.addrToken), as "<ip>" rather than a per-export token: there is
// no flow to follow across the lines of this file.
func maskExitDiagnosticsNetworkAddress(match string) string {
	if addr, ok := matchAddr(match); ok {
		if !maskedAddr(addr) {
			return match
		}
		return "<ip>"
	}
	start, end := longestAddrLiteral(match)
	if start == end {
		return match
	}
	if _, ok := matchAddr(match[start:end]); !ok {
		return match
	}
	return match[:start] + "<ip>" + match[end:]
}

// A json object's members, nil when the value is not an object.
func jsonObject(value json.RawMessage) map[string]json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil {
		return nil
	}
	return object
}

// A json array's elements, nil when the value is not an array.
func jsonArray(value json.RawMessage) []json.RawMessage {
	if len(value) == 0 {
		return nil
	}
	var array []json.RawMessage
	if err := json.Unmarshal(value, &array); err != nil {
		return nil
	}
	return array
}

// A json string's text or a json number's literal; ok is false for anything
// else.
func jsonText(value json.RawMessage) (string, bool) {
	if len(value) == 0 {
		return "", false
	}
	var text string
	if err := json.Unmarshal(value, &text); err == nil {
		return text, true
	}
	var number json.Number
	if err := json.Unmarshal(value, &number); err == nil {
		return number.String(), true
	}
	return "", false
}

// A json integer, or a string of one, as its decimal text: an optional '-' and
// 1 to 20 digits. ok is false for anything else.
func jsonIntegerText(value json.RawMessage) (string, bool) {
	text, ok := jsonText(value)
	if !ok {
		return "", false
	}
	digits := strings.TrimPrefix(text, "-")
	if digits == "" || 20 < len(digits) {
		return "", false
	}
	for i := 0; i < len(digits); i += 1 {
		if digits[i] < '0' || '9' < digits[i] {
			return "", false
		}
	}
	return text, true
}

// A json bool; ok is false for anything else.
func jsonBool(value json.RawMessage) (bool, bool) {
	if len(value) == 0 {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(value, &b); err != nil {
		return false, false
	}
	return b, true
}

// The record of what the exit diagnostics file holds, beside it: the digest
// and end of each payload logged, oldest first, and the floor below which a
// payload counts as logged. The payloads the record no longer keeps, those
// ending more than exitDiagnosticsStateRetentionMillis before the newest and
// the oldest past exitDiagnosticsStateMaxPayloadCount, lift the floor to their
// end. A payload ending at the floor is logged unless its digest is kept: two
// payloads can end at one time, and logging one twice is better than not at
// all.
type exitDiagnosticsState struct {
	FloorMillis int64                          `json:"floor_millis,omitempty"`
	Payloads    []exitDiagnosticsLoggedPayload `json:"payloads,omitempty"`
}

type exitDiagnosticsLoggedPayload struct {
	Digest    string `json:"digest"`
	EndMillis int64  `json:"end_millis"`
}

// The floor, or none when it is further ahead of the clock than
// exitDiagnosticsStateMaxAheadMillis: it was raised while the clock was set
// ahead, and every payload to come would end below it. No floor logs a
// payload twice at worst.
func (self exitDiagnosticsState) floorMillis(nowMillis int64) int64 {
	if nowMillis+exitDiagnosticsStateMaxAheadMillis < self.FloorMillis {
		return 0
	}
	return self.FloorMillis
}

// Whether the payload with digest, ending at endMillis (0 when it has no end),
// was logged.
func (self exitDiagnosticsState) logged(digest string, endMillis int64, nowMillis int64) bool {
	if 0 < endMillis && endMillis < self.floorMillis(nowMillis) {
		return true
	}
	return slices.ContainsFunc(self.Payloads, func(payload exitDiagnosticsLoggedPayload) bool {
		return payload.Digest == digest
	})
}

// The record with the payload added. An end the payload does not have counts
// as the clock's, and an end further ahead of the clock than
// exitDiagnosticsStateMaxAheadMillis, the payload's or one recorded while the
// clock was set ahead, as that bound, so it cannot keep the newest payloads
// from being kept.
func (self exitDiagnosticsState) with(digest string, endMillis int64, nowMillis int64) exitDiagnosticsState {
	if endMillis <= 0 {
		endMillis = nowMillis
	}
	maxEndMillis := nowMillis + exitDiagnosticsStateMaxAheadMillis

	payloads := append(slices.Clone(self.Payloads), exitDiagnosticsLoggedPayload{
		Digest:    digest,
		EndMillis: endMillis,
	})
	for i := range payloads {
		payloads[i].EndMillis = min(payloads[i].EndMillis, maxEndMillis)
	}
	slices.SortStableFunc(payloads, func(a exitDiagnosticsLoggedPayload, b exitDiagnosticsLoggedPayload) int {
		return cmp.Compare(a.EndMillis, b.EndMillis)
	})
	newestEndMillis := payloads[len(payloads)-1].EndMillis
	floorMillis := self.floorMillis(nowMillis)
	for 0 < len(payloads) && (exitDiagnosticsStateMaxPayloadCount < len(payloads) ||
		payloads[0].EndMillis < newestEndMillis-exitDiagnosticsStateRetentionMillis) {
		floorMillis = max(floorMillis, payloads[0].EndMillis)
		payloads = payloads[1:]
	}
	return exitDiagnosticsState{
		FloorMillis: floorMillis,
		Payloads:    payloads,
	}
}

// The record in dir, empty when there is none or it cannot be read: then
// payloads the file already holds can be logged again, which is better than
// not at all.
func readExitDiagnosticsState(dir string) exitDiagnosticsState {
	content, err := readExitDiagnosticsRegularFile(filepath.Join(dir, exitDiagnosticsStateFileName), exitDiagnosticsStateMaxByteCount)
	if err != nil {
		return exitDiagnosticsState{}
	}
	var state exitDiagnosticsState
	if err := json.Unmarshal(content, &state); err != nil {
		return exitDiagnosticsState{}
	}
	if exitDiagnosticsStateMaxPayloadCount < len(state.Payloads) {
		state.Payloads = state.Payloads[len(state.Payloads)-exitDiagnosticsStateMaxPayloadCount:]
	}
	return state
}

func writeExitDiagnosticsState(dir string, state exitDiagnosticsState) error {
	content, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeExitDiagnosticsFile(filepath.Join(dir, exitDiagnosticsStateFileName), content)
}

// Adds lines to the exit diagnostics file in dir, which keeps its newest
// whole lines within exitDiagnosticsMaxByteCount. The file is written whole
// beside itself and renamed over itself, so that an upload reading it, from
// this process or the other, never reads half of it.
func appendExitDiagnosticsLines(dir string, lines []string) error {
	path := filepath.Join(dir, exitDiagnosticsFileName)
	var content bytes.Buffer
	content.Write(readExitDiagnosticsTail(path, exitDiagnosticsMaxByteCount))
	if 0 < content.Len() && content.Bytes()[content.Len()-1] != '\n' {
		content.WriteByte('\n')
	}
	for _, line := range lines {
		content.WriteString(line)
		content.WriteByte('\n')
	}
	return writeExitDiagnosticsFile(path, newestExitDiagnosticsLines(content.Bytes(), exitDiagnosticsMaxByteCount))
}

// The newest whole lines of content within maxByteCount.
func newestExitDiagnosticsLines(content []byte, maxByteCount int) []byte {
	if len(content) <= maxByteCount {
		return content
	}
	start := len(content) - maxByteCount
	if content[start-1] != '\n' {
		// the newest maxByteCount bytes begin inside a line: from the next one
		i := bytes.IndexByte(content[start:], '\n')
		if i < 0 {
			return nil
		}
		start += i + 1
	}
	return content[start:]
}

// The newest whole lines of the regular file at path within maxByteCount, nil
// when there is no regular file there or it cannot be read.
func readExitDiagnosticsTail(path string, maxByteCount int64) []byte {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	// one byte before the newest maxByteCount, to see whether they begin a
	// line
	start := max(0, info.Size()-maxByteCount-1)
	content, err := io.ReadAll(io.LimitReader(io.NewSectionReader(file, start, info.Size()-start), maxByteCount+1))
	if err != nil {
		return nil
	}
	if 0 < start {
		i := bytes.IndexByte(content, '\n')
		if i < 0 {
			return nil
		}
		content = content[i+1:]
	}
	return content
}

// The content of the regular file at path, at most maxByteCount bytes.
func readExitDiagnosticsRegularFile(path string, maxByteCount int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || maxByteCount < info.Size() {
		return nil, errors.New("not a regular file within its bound")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, maxByteCount))
}

// Writes content to path through a new file beside it, renamed over it.
func writeExitDiagnosticsFile(path string, content []byte) error {
	tempPath := path + ".tmp"
	// a leftover of an earlier write, or anything else of that name, goes
	// first: the file written is always a new one, never one reached through
	// a link
	os.Remove(tempPath)
	file, err := os.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, LocalStorageFilePermissions)
	if err != nil {
		return err
	}
	_, err = file.Write(content)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tempPath, path)
	}
	if err != nil {
		os.Remove(tempPath)
		return err
	}
	return nil
}

// An error as a log line may carry it: an os error without the path it names,
// which holds the app group container's uuid on ios.
func exitDiagnosticsErrorText(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Op + ": " + pathErr.Err.Error()
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Op + ": " + linkErr.Err.Error()
	}
	return err.Error()
}
