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
// one bounded line, and one payload adds a bounded number of them
// (exitDiagnosticsSettings).
//
// A payload is logged once. MetricKit keeps its past payloads for days and the
// app reads them at each start besides the new ones it is handed, so what was
// logged is recorded beside the file (exitDiagnosticsState) and a payload in
// that record is skipped: the watermark Android keeps of the last record it
// logged, made fit for payloads that can come out of order.
//
// The package functions here are safe for concurrent use: the file and its
// record are read and written under exitDiagnosticsStateLock.

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/urnetwork/connect/v2026"
)

// The bounds of the exit diagnostics file and of what one payload adds to it,
// and how long the record of the payloads logged keeps them.
type exitDiagnosticsSettings struct {
	// The file keeps its newest whole lines within this many bytes. 64 KiB is
	// two hundred lines or more, and stays small enough for the upload to plan
	// the file before every log file (selectUploadLogFiles).
	MaxByteCount int
	// The longest line, in bytes. A line's fields are each bounded so that
	// together they stay under it; a longer line is cut between fields.
	LineMaxLength int
	// The most lines one payload adds; one more line counts the records past
	// them. A device in a crash loop can send a payload of hundreds.
	PayloadMaxLineCount int
	// The longest termination reason a line keeps, in bytes: 200, as Android
	// keeps of an exit's description.
	ReasonMaxLength int
	// The most of a termination reason that is read, in bytes. The system's
	// text can be a whole report.
	ReasonReadMaxLength int
	// A payload larger than this, in bytes, is not read. One with the call
	// stacks of a crash is tens of KiB.
	PayloadMaxByteCount int
	// The largest record of the payloads logged that is read, in bytes; a
	// larger one counts as none.
	StateMaxByteCount int64
	// The payloads the record keeps. Past them the oldest are dropped and the
	// floor rises to them.
	StateMaxPayloadCount int
	// How long before the newest payload the record keeps one. MetricKit keeps
	// its past payloads for 7 days, so 14 outlasts any it hands out again.
	StateRetention time.Duration
	// An end further ahead of the clock than this counts as the clock plus
	// this, and a floor further ahead counts as none, so that one wrong clock
	// cannot stop the file for good.
	StateMaxAhead time.Duration
}

// A new value on every call, with the one default of each bound.
func defaultExitDiagnosticsSettings() *exitDiagnosticsSettings {
	return &exitDiagnosticsSettings{
		MaxByteCount:         64 * 1024,
		LineMaxLength:        1024,
		PayloadMaxLineCount:  32,
		ReasonMaxLength:      200,
		ReasonReadMaxLength:  4 * 1024,
		PayloadMaxByteCount:  16 * 1024 * 1024,
		StateMaxByteCount:    256 * 1024,
		StateMaxPayloadCount: 256,
		StateRetention:       14 * 24 * time.Hour,
		StateMaxAhead:        24 * time.Hour,
	}
}

// The record of what has been logged, beside the file. Never uploaded: the
// inventory takes only the file itself.
const exitDiagnosticsStateFileName = "exit-diagnostics-state.json"

// The bounds of a line's fields, in bytes. A value longer than its field, or
// with a character its field does not allow, is left out, never cut or
// mangled.
const (
	exitDiagnosticsVersionMaxLength    = 32
	exitDiagnosticsBundleIdMaxLength   = 64
	exitDiagnosticsOsVersionMaxLength  = 48
	exitDiagnosticsIdentifierMaxLength = 64
	exitDiagnosticsMeasureMaxLength    = 32
	// the name of a later os's diagnostics kind, before "Diagnostics"
	exitDiagnosticsKindMaxLength = 24
	// a time stamp of the json
	exitDiagnosticsTimeMaxLength = 64
)

// The facts of the numbers a line shows and of what a termination reason
// keeps of them.
const (
	// the hex digits of a 32 bit code, such as the watchdog's 0x8badf00d. An
	// exception code or a 0x number wider than this can hold an address.
	exitDiagnosticsCodeHexDigits    = 8
	exitDiagnosticsExceptionCodeMax = 0xffffffff
	// an exception type or a signal is a small number
	exitDiagnosticsSmallIntegerMax = 0xffff
	// the digits of the largest 64 bit integer
	exitDiagnosticsIntegerMaxDigits = 20
	// a run of this many decimal digits or more is a "<number>": an address,
	// a time or an id
	exitDiagnosticsRedactedNumberMinLength = 9
	// a run of this many hex digits or more is a "<hex>": an id or a hash,
	// such as a uuid without its dashes
	exitDiagnosticsRedactedHexMinLength = 12
	// a payload digest's hex digits: 128 bits
	exitDiagnosticsDigestLength = 32
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
// Never panics: a diagnostic must not be what breaks the app. MetricKit calls
// its subscriber on a background queue, which may call this directly. Not in
// the c abi (cgo/gen): there is no MetricKit on windows or linux.
func LogMetricKitDiagnosticPayload(payloadJson string, beginMillis int64, endMillis int64) int {
	return logMetricKitPayload(
		metricKitDiagnosticPayloadKind,
		payloadJson,
		beginMillis,
		endMillis,
		time.Now(),
		defaultExitDiagnosticsSettings(),
		connect.DefaultLogger(),
	)
}

// Logs one MetricKit metric payload, the json of an MXMetricPayload
// (jsonRepresentation), to the exit diagnostics file of this process's log
// directory: its exit metric, the app's exits in the foreground and in the
// background by reason, a line for each, unless the payload was logged before.
// A payload without an exit metric adds no line. As
// LogMetricKitDiagnosticPayload otherwise.
func LogMetricKitMetricPayload(payloadJson string, beginMillis int64, endMillis int64) int {
	return logMetricKitPayload(
		metricKitMetricPayloadKind,
		payloadJson,
		beginMillis,
		endMillis,
		time.Now(),
		defaultExitDiagnosticsSettings(),
		connect.DefaultLogger(),
	)
}

// Serializes the reads and writes of the exit diagnostics file and of its
// record within this process. Only the app process writes them.
var exitDiagnosticsStateLock sync.Mutex

// Logs one payload of kind to the exit diagnostics file of this process's log
// directory, and returns the number of lines written. The record is read, the
// lines decided and both files written in one locked scope, so two payloads
// handed over at once cannot both count as new; the log lines come after it.
func logMetricKitPayload(
	kind metricKitPayloadKind,
	payloadJson string,
	beginMillis int64,
	endMillis int64,
	now time.Time,
	settings *exitDiagnosticsSettings,
	log connect.Logger,
) (lineCount int) {
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("[exit-diagnostics]a MetricKit %s payload was not logged: %v", kind, r)
			lineCount = 0
		}
	}()

	dir := GetLogDir()
	if dir == "" {
		return 0
	}

	var lines []string
	var ok bool
	var appendErr error
	var stateErr error
	func() {
		exitDiagnosticsStateLock.Lock()
		defer exitDiagnosticsStateLock.Unlock()

		statePath := filepath.Join(dir, exitDiagnosticsStateFileName)
		// the record, empty when there is none or it cannot be read: then a
		// payload the file holds can be logged again, which is better than not
		// at all
		readState := func() exitDiagnosticsState {
			info, err := os.Lstat(statePath)
			if err != nil || !info.Mode().IsRegular() || settings.StateMaxByteCount < info.Size() {
				return exitDiagnosticsState{}
			}
			file, err := os.Open(statePath)
			if err != nil {
				return exitDiagnosticsState{}
			}
			defer file.Close()
			content, err := io.ReadAll(io.LimitReader(file, settings.StateMaxByteCount+1))
			if err != nil || settings.StateMaxByteCount < int64(len(content)) {
				return exitDiagnosticsState{}
			}
			var state exitDiagnosticsState
			if err := json.Unmarshal(content, &state); err != nil {
				return exitDiagnosticsState{}
			}
			if settings.StateMaxPayloadCount < len(state.Payloads) {
				state.Payloads = state.Payloads[len(state.Payloads)-settings.StateMaxPayloadCount:]
			}
			return state
		}

		var nextState exitDiagnosticsState
		lines, nextState, ok = metricKitExitDiagnosticLines(
			kind,
			payloadJson,
			beginMillis,
			endMillis,
			readState(),
			now.UnixMilli(),
			settings,
		)
		if !ok {
			return
		}
		if 0 < len(lines) {
			if appendErr = appendExitDiagnosticsLinesWithLock(dir, lines, settings); appendErr != nil {
				return
			}
		}
		// the lines are written before the record that says so: a failure
		// between the two writes logs the payload twice, never not at all
		stateContent, err := json.Marshal(nextState)
		if err == nil {
			err = writeExitDiagnosticsFileWithLock(statePath, stateContent)
		}
		stateErr = err
	}()

	if !ok {
		return 0
	}
	if appendErr != nil {
		log.Infof("[exit-diagnostics]a MetricKit %s payload was not logged: %s", kind, exitDiagnosticsErrorText(appendErr))
		return 0
	}
	if stateErr != nil {
		log.Infof("[exit-diagnostics]the record of what was logged was not written: %s", exitDiagnosticsErrorText(stateErr))
	}
	log.Infof("[exit-diagnostics]logged %d lines of a MetricKit %s payload", len(lines), kind)
	return len(lines)
}

// The lines one MetricKit payload adds to the exit diagnostics file, and the
// record of what was logged once they are written, from the record before and
// the clock (unix milliseconds, which only bounds the record). ok is false when
// there is nothing to write or record: the json is not a json object, it is
// larger than PayloadMaxByteCount, or the payload was logged before. A payload
// without records is recorded with no lines, so it is not read again. Pure, so
// the same arguments give the same lines.
func metricKitExitDiagnosticLines(
	kind metricKitPayloadKind,
	payloadJson string,
	beginMillis int64,
	endMillis int64,
	state exitDiagnosticsState,
	nowMillis int64,
	settings *exitDiagnosticsSettings,
) (lines []string, nextState exitDiagnosticsState, ok bool) {
	if settings.PayloadMaxByteCount < len(payloadJson) {
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

	// the payload's digest, to know it again: its kind and its json without
	// the space between tokens, so the same payload printed twice has one
	var compactPayload bytes.Buffer
	if err := json.Compact(&compactPayload, []byte(payloadJson)); err != nil {
		compactPayload.Reset()
		compactPayload.WriteString(payloadJson)
	}
	digestHash := sha256.New()
	digestHash.Write([]byte(kind))
	digestHash.Write([]byte{0})
	digestHash.Write(compactPayload.Bytes())
	digest := hex.EncodeToString(digestHash.Sum(nil))[:exitDiagnosticsDigestLength]

	if state.logged(digest, endMillis, nowMillis, settings) {
		return nil, state, false
	}

	timeRange := exitDiagnosticsTime(beginMillis) + ".." + exitDiagnosticsTime(endMillis)
	switch kind {
	case metricKitDiagnosticPayloadKind:
		lines = metricKitDiagnosticLines(payload, timeRange, settings)
	case metricKitMetricPayloadKind:
		lines = metricKitExitMetricLines(payload, timeRange, settings)
	}
	return lines, state.with(digest, endMillis, nowMillis, settings), true
}

// A MetricKit time stamp of the json in unix milliseconds, 0 when it is not
// one. The app passes the payload's own times; this reads the json's for a
// caller that does not.
func metricKitTimeMillis(value json.RawMessage) int64 {
	text, ok := jsonText(value)
	if !ok || exitDiagnosticsTimeMaxLength < len(text) {
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
	{key: "crashDiagnostics", name: "crash"},
	{key: "hangDiagnostics", name: "hang"},
	{key: "cpuExceptionDiagnostics", name: "cpu_exception"},
	{key: "diskWriteExceptionDiagnostics", name: "disk_write_exception"},
	{key: "appLaunchDiagnostics", name: "app_launch"},
}

// The name of the diagnostics under a key a later os adds, such as
// memoryExceptionDiagnostics: the key without Diagnostics, from camel case to
// snake case; "" unless that is 1 to exitDiagnosticsKindMaxLength ascii
// letters.
func metricKitDiagnosticKindName(key string) string {
	base, ok := strings.CutSuffix(key, "Diagnostics")
	if !ok || base == "" || exitDiagnosticsKindMaxLength < len(base) {
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

// A line for each diagnostic of a diagnostic payload, the kinds in a fixed
// order, at most PayloadMaxLineCount and then a line that counts the rest.
func metricKitDiagnosticLines(payload map[string]json.RawMessage, timeRange string, settings *exitDiagnosticsSettings) []string {
	type diagnostics struct {
		name    string
		records []json.RawMessage
	}
	payloadDiagnostics := []diagnostics{}
	knownKeys := map[string]bool{}
	for _, kind := range metricKitDiagnosticKinds {
		knownKeys[kind.key] = true
		if records := jsonArray(payload[kind.key]); 0 < len(records) {
			payloadDiagnostics = append(payloadDiagnostics, diagnostics{name: kind.name, records: records})
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
			payloadDiagnostics = append(payloadDiagnostics, diagnostics{name: name, records: records})
		}
	}

	// one diagnostic's line, from its json object
	diagnosticLine := func(name string, fields map[string]json.RawMessage) string {
		// a diagnostic's facts are in its diagnosticMetaData; the record's own
		// keys are read after them, should a later os move one
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
		nameOf := func(names map[int]string, n int) string {
			if name, ok := names[n]; ok {
				return name
			}
			return "unknown"
		}

		line := newExitDiagnosticsLine(timeRange, name, settings.LineMaxLength)
		line.field("process", exitDiagnosticsToken(value("bundleIdentifier"), exitDiagnosticsBundleIdMaxLength, isExitDiagnosticsBundleIdByte))
		line.field("app", exitDiagnosticsToken(value("appVersion", "applicationVersion"), exitDiagnosticsVersionMaxLength, isExitDiagnosticsVersionByte))
		line.field("build", exitDiagnosticsToken(value("appBuildVersion", "applicationBuildVersion"), exitDiagnosticsVersionMaxLength, isExitDiagnosticsVersionByte))
		line.quotedField("os", exitDiagnosticsToken(value("osVersion"), exitDiagnosticsOsVersionMaxLength, isExitDiagnosticsOsVersionByte))
		if exceptionType, ok := exitDiagnosticsSmallInteger(value("exceptionType")); ok {
			line.field("exception_type", nameOf(machExceptionTypeNames, exceptionType)+"("+strconv.Itoa(exceptionType)+")")
		}
		if exceptionCodeText, ok := jsonIntegerText(value("exceptionCode")); ok {
			// in hexadecimal, when it fits 32 bits
			exceptionCode, err := strconv.ParseUint(exceptionCodeText, 10, 64)
			if err != nil || exitDiagnosticsExceptionCodeMax < exceptionCode {
				line.field("exception_code", "omitted")
			} else {
				line.field("exception_code", "0x"+strconv.FormatUint(exceptionCode, 16))
			}
		}
		if signal, ok := exitDiagnosticsSmallInteger(value("signal")); ok {
			line.field("signal", nameOf(darwinSignalNames, signal)+"("+strconv.Itoa(signal)+")")
		}
		// an uncaught objective-c exception: its name and class, never its
		// message
		if exceptionReason := jsonObject(value("exceptionReason")); exceptionReason != nil {
			line.field("objc_exception", exitDiagnosticsToken(exceptionReason["exceptionName"], exitDiagnosticsIdentifierMaxLength, isExitDiagnosticsIdentifierByte))
			line.field("objc_class", exitDiagnosticsToken(exceptionReason["className"], exitDiagnosticsIdentifierMaxLength, isExitDiagnosticsIdentifierByte))
		}
		line.quotedField("hang_duration", exitDiagnosticsMeasure(value("hangDuration")))
		line.quotedField("launch_duration", exitDiagnosticsMeasure(value("launchDuration")))
		line.quotedField("cpu_time", exitDiagnosticsMeasure(value("totalCPUTime")))
		line.quotedField("sampled_time", exitDiagnosticsMeasure(value("totalSampledTime")))
		line.quotedField("writes", exitDiagnosticsMeasure(value("writesCaused", "totalWritesCaused")))
		if reason, ok := jsonText(value("terminationReason")); ok {
			line.quotedField("termination_reason", sanitizeExitDiagnosticsReason(reason, settings))
		}
		return line.String()
	}

	lines := []string{}
	omittedCount := 0
	for _, kindDiagnostics := range payloadDiagnostics {
		for _, record := range kindDiagnostics.records {
			if settings.PayloadMaxLineCount <= len(lines) {
				omittedCount += 1
				continue
			}
			fields := jsonObject(record)
			if fields == nil {
				continue
			}
			lines = append(lines, diagnosticLine(kindDiagnostics.name, fields))
		}
	}
	if 0 < omittedCount {
		line := newExitDiagnosticsLine(timeRange, "omitted", settings.LineMaxLength)
		line.field("records", strconv.Itoa(omittedCount))
		lines = append(lines, line.String())
	}
	return lines
}

// The reasons an exit metric counts exits by: the json key of each count and
// the name a line gives it, in the order a line lists them.
// MXForegroundExitData and MXBackgroundExitData name most of them; the json
// carries the background timeouts too.
var metricKitExitReasons = []struct {
	key  string
	name string
}{
	{key: "cumulativeNormalAppExitCount", name: "normal"},
	{key: "cumulativeMemoryResourceLimitExitCount", name: "memory_resource_limit"},
	{key: "cumulativeMemoryPressureExitCount", name: "memory_pressure"},
	{key: "cumulativeCPUResourceLimitExitCount", name: "cpu_resource_limit"},
	{key: "cumulativeBadAccessExitCount", name: "bad_access"},
	{key: "cumulativeIllegalInstructionExitCount", name: "illegal_instruction"},
	{key: "cumulativeAbnormalExitCount", name: "abnormal"},
	{key: "cumulativeAppWatchdogExitCount", name: "app_watchdog"},
	{key: "cumulativeSuspendedWithLockedFileExitCount", name: "suspended_with_locked_file"},
	{key: "cumulativeBackgroundTaskAssertionTimeoutExitCount", name: "background_task_assertion_timeout"},
	{key: "cumulativeBackgroundURLSessionCompletionTimeoutExitCount", name: "background_url_session_completion_timeout"},
	{key: "cumulativeBackgroundFetchCompletionTimeoutExitCount", name: "background_fetch_completion_timeout"},
}

// A count a later os adds, by its key: cumulative, 1 to 64 ascii letters, then
// ExitCount. It is summed into other.
var metricKitOtherExitCountKeyPattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^cumulative[A-Za-z]{1,64}ExitCount$`)
})

// The lines of a metric payload's exit metric: one for the exits in the
// foreground and one for those in the background, each with the total and
// every reason counted; none when the payload has no exit metric.
func metricKitExitMetricLines(payload map[string]json.RawMessage, timeRange string, settings *exitDiagnosticsSettings) []string {
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
		{key: "foregroundExitData", name: "foreground_exits"},
		{key: "backgroundExitData", name: "background_exits"},
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

		line := newExitDiagnosticsLine(timeRange, side.name, settings.LineMaxLength)
		line.field("app", exitDiagnosticsToken(value("appVersion", "latestApplicationVersion", "applicationVersion"), exitDiagnosticsVersionMaxLength, isExitDiagnosticsVersionByte))
		line.field("build", exitDiagnosticsToken(value("appBuildVersion", "applicationBuildVersion"), exitDiagnosticsVersionMaxLength, isExitDiagnosticsVersionByte))
		if multiple, ok := jsonBool(payload["includesMultipleApplicationVersions"]); ok && multiple {
			line.field("app_versions", "multiple")
		}
		line.quotedField("os", exitDiagnosticsToken(value("osVersion"), exitDiagnosticsOsVersionMaxLength, isExitDiagnosticsOsVersionByte))
		line.field("total", strconv.FormatUint(total, 10))
		for _, reasonCount := range reasonCounts {
			line.raw(reasonCount)
		}
		lines = append(lines, line.String())
	}
	return lines
}

// a + b, or the largest uint64 when the sum does not fit.
func saturatingAdd(a uint64, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}

// One line of the exit diagnostics file: the payload's time range, "metrickit"
// and the record's kind, then its fields as key=value, quoted where a value
// can hold a space. Every value is sanitized before it is added, and none holds
// a quote or a line break. One goroutine builds a line.
type exitDiagnosticsLine struct {
	builder   strings.Builder
	maxLength int
}

// A line of the kind over the time range, at most maxLength bytes long.
func newExitDiagnosticsLine(timeRange string, kind string, maxLength int) *exitDiagnosticsLine {
	line := &exitDiagnosticsLine{
		maxLength: maxLength,
	}
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

// Adds text as written, after a space.
func (self *exitDiagnosticsLine) raw(text string) {
	self.builder.WriteByte(' ')
	self.builder.WriteString(text)
}

// The line, cut between fields to its max length should its fields together
// pass it.
func (self *exitDiagnosticsLine) String() string {
	line := self.builder.String()
	if len(line) <= self.maxLength {
		return line
	}
	limit := max(0, self.maxLength-len(" ..."))
	cut := strings.LastIndexByte(line[:limit], ' ')
	if cut <= 0 {
		cut = limit
	}
	return line[:cut] + " ..."
}

// An exception type or a signal: an integer from 0 to
// exitDiagnosticsSmallIntegerMax.
func exitDiagnosticsSmallInteger(value json.RawMessage) (int, bool) {
	text, ok := jsonIntegerText(value)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 0 || exitDiagnosticsSmallIntegerMax < n {
		return 0, false
	}
	return n, true
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

// A version, a bundle id, an os version or an identifier as its field keeps
// it: trimmed, at most maxLength bytes, every one of them a byte allowed
// accepts; "" otherwise. A value with any other character is not one, and is
// left out rather than mangled.
func exitDiagnosticsToken(value json.RawMessage, maxLength int, allowed func(c byte) bool) string {
	text, ok := jsonText(value)
	if !ok {
		return ""
	}
	text = strings.TrimSpace(text)
	if maxLength < len(text) {
		return ""
	}
	for i := 0; i < len(text); i += 1 {
		if !allowed(text[i]) {
			return ""
		}
	}
	return text
}

// An ascii letter or digit.
func isAsciiLetterOrDigit(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

// A byte of an app version or build, such as 2026.11.3 or 4512.
func isExitDiagnosticsVersionByte(c byte) bool {
	return isAsciiLetterOrDigit(c) || c == '.' || c == '_' || c == '+' || c == '-'
}

// A byte of a bundle id, such as com.example.app.
func isExitDiagnosticsBundleIdByte(c byte) bool {
	return isAsciiLetterOrDigit(c) || c == '.' || c == '-'
}

// A byte of an os version, such as iPhone OS 26.1 (23B85).
func isExitDiagnosticsOsVersionByte(c byte) bool {
	return isAsciiLetterOrDigit(c) || c == ' ' || c == '.' || c == '_' || c == '(' || c == ')' || c == '-'
}

// A byte of an objective-c exception's name or class, such as
// NSInvalidArgumentException.
func isExitDiagnosticsIdentifierByte(c byte) bool {
	return isAsciiLetterOrDigit(c) || c == '_' || c == '.' || c == '$'
}

// A measure with its unit, such as 20 sec or 1,500 KB.
var exitDiagnosticsMeasurePattern = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[0-9][0-9.,]{0,15}(?: ?[A-Za-z]{1,8})?$`)
})

// A hang, launch, cpu or write measure as its field keeps it; "" when it is
// not a number with a unit.
func exitDiagnosticsMeasure(value json.RawMessage) string {
	text, ok := jsonText(value)
	if !ok || exitDiagnosticsMeasureMaxLength < len(text) {
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
	// a run of hex digits, which a reason keeps only when it is short
	hexRun *regexp.Regexp
}

// Compiled at the first termination reason, in the app process only.
var exitDiagnosticsReasonPatternsOnce = sync.OnceValue(func() *exitDiagnosticsReasonPatterns {
	return &exitDiagnosticsReasonPatterns{
		mac:           regexp.MustCompile(`\b[0-9A-Fa-f]{2}(?:[:-][0-9A-Fa-f]{2}){5}\b`),
		memoryAddress: regexp.MustCompile(fmt.Sprintf(`0[xX][0-9A-Fa-f]{%d,}`, exitDiagnosticsCodeHexDigits+1)),
		hexRun:        regexp.MustCompile(`[0-9A-Fa-f]+`),
	}
})

// A termination reason as a line keeps it: one line of printable ascii, with
// each path, email address, uuid, mac address, network address, memory address
// and long number or hex run replaced by a word in angle brackets that says
// what was there, and at most ReasonMaxLength bytes, cut between words and
// ending in "...". "" when nothing is left.
//
// The system writes the reason, but a dyld or a guard reason can name a file
// in the app's container, whose path holds the install's uuid, or an address.
// Short codes stay: the watchdog's 0x8badf00d, a pid, a version.
func sanitizeExitDiagnosticsReason(reason string, settings *exitDiagnosticsSettings) string {
	cut := false
	if settings.ReasonReadMaxLength < len(reason) {
		reason = reason[:settings.ReasonReadMaxLength]
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
	reason = patterns.hexRun.ReplaceAllStringFunc(reason, func(run string) string {
		decimal := strings.Trim(run, "0123456789") == ""
		switch {
		case decimal && exitDiagnosticsRedactedNumberMinLength <= len(run):
			return "<number>"
		case exitDiagnosticsRedactedHexMinLength <= len(run):
			return "<hex>"
		default:
			// a code, a pid or a version
			return run
		}
	})
	// the line quotes the reason
	reason = strings.ReplaceAll(reason, `"`, `'`)

	if settings.ReasonMaxLength < len(reason) {
		limit := max(0, settings.ReasonMaxLength-len("..."))
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
// 1 to exitDiagnosticsIntegerMaxDigits digits. ok is false for anything else.
func jsonIntegerText(value json.RawMessage) (string, bool) {
	text, ok := jsonText(value)
	if !ok {
		return "", false
	}
	digits := strings.TrimPrefix(text, "-")
	if digits == "" || exitDiagnosticsIntegerMaxDigits < len(digits) {
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
// payload counts as logged. The payloads the record no longer keeps (settings
// StateRetention and StateMaxPayloadCount) lift the floor to their end. A
// payload ending at the floor is logged unless its digest is kept: two
// payloads can end at one time, and logging one twice is better than not at
// all. A value; its methods return a new record.
type exitDiagnosticsState struct {
	FloorMillis int64                          `json:"floor_millis,omitempty"`
	Payloads    []exitDiagnosticsLoggedPayload `json:"payloads,omitempty"`
}

// One payload logged: its digest and its end, unix milliseconds.
type exitDiagnosticsLoggedPayload struct {
	Digest    string `json:"digest"`
	EndMillis int64  `json:"end_millis"`
}

// The floor, or none when it is further ahead of the clock than StateMaxAhead:
// it was raised while the clock was set ahead, and every payload to come would
// end below it. No floor logs a payload twice at worst.
func (self exitDiagnosticsState) floorMillis(nowMillis int64, settings *exitDiagnosticsSettings) int64 {
	if nowMillis+settings.StateMaxAhead.Milliseconds() < self.FloorMillis {
		return 0
	}
	return self.FloorMillis
}

// Whether the payload with digest, ending at endMillis (0 when it has no end),
// was logged.
func (self exitDiagnosticsState) logged(digest string, endMillis int64, nowMillis int64, settings *exitDiagnosticsSettings) bool {
	if 0 < endMillis && endMillis < self.floorMillis(nowMillis, settings) {
		return true
	}
	return slices.ContainsFunc(self.Payloads, func(payload exitDiagnosticsLoggedPayload) bool {
		return payload.Digest == digest
	})
}

// The record with the payload added. An end the payload does not have counts
// as the clock's, and an end further ahead of the clock than StateMaxAhead,
// the payload's or one recorded while the clock was set ahead, as that bound,
// so it cannot keep the newest payloads from being kept.
func (self exitDiagnosticsState) with(digest string, endMillis int64, nowMillis int64, settings *exitDiagnosticsSettings) exitDiagnosticsState {
	if endMillis <= 0 {
		endMillis = nowMillis
	}
	maxEndMillis := nowMillis + settings.StateMaxAhead.Milliseconds()

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
	floorMillis := self.floorMillis(nowMillis, settings)
	for 0 < len(payloads) && (settings.StateMaxPayloadCount < len(payloads) ||
		payloads[0].EndMillis < newestEndMillis-settings.StateRetention.Milliseconds()) {
		floorMillis = max(floorMillis, payloads[0].EndMillis)
		payloads = payloads[1:]
	}
	return exitDiagnosticsState{
		FloorMillis: floorMillis,
		Payloads:    payloads,
	}
}

// Adds lines to the exit diagnostics file in dir, which keeps its newest whole
// lines within MaxByteCount. The file is written whole beside itself and
// renamed over itself, so that an upload reading it, from this process or the
// other, never reads half of it. Called with exitDiagnosticsStateLock.
func appendExitDiagnosticsLinesWithLock(dir string, lines []string, settings *exitDiagnosticsSettings) error {
	path := filepath.Join(dir, exitDiagnosticsFileName)
	maxByteCount := int64(settings.MaxByteCount)

	// the file's newest whole lines within the bound, none when there is no
	// regular file there or it cannot be read
	readTail := func() []byte {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer file.Close()
		// one byte before the newest bytes, to see whether they begin a line
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

	var content bytes.Buffer
	content.Write(readTail())
	if 0 < content.Len() && content.Bytes()[content.Len()-1] != '\n' {
		content.WriteByte('\n')
	}
	for _, line := range lines {
		content.WriteString(line)
		content.WriteByte('\n')
	}
	return writeExitDiagnosticsFileWithLock(path, newestExitDiagnosticsLines(content.Bytes(), settings.MaxByteCount))
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

// Writes content to path through a new file beside it, renamed over it.
// Called with exitDiagnosticsStateLock.
func writeExitDiagnosticsFileWithLock(path string, content []byte) error {
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
