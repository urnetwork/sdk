//go:build !ios_extension && !js

package sdk

// MetricKit's reports in the exit diagnostics file (exit_diagnostics_metrickit.go),
// support 8190: a line per record that says why the app's process ended and
// nothing else -- no call stack, address, path, id or unbounded text, even
// from a payload built to carry them -- a payload logged once however often
// it comes, and json that is not a payload writing nothing.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A crash payload as MXDiagnosticPayload.jsonRepresentation writes one: the
// diagnostic's facts in its diagnosticMetaData, beside a call stack tree, the
// memory region of the bad access, and the device's type and region. Every
// id, address and name in it is synthetic.
const testingMetricKitCrashPayload = `{
  "timeStampEnd" : "2026-11-10 13:19:00 +0000",
  "crashDiagnostics" : [
    {
      "version" : "1.0.0",
      "callStackTree" : {
        "callStackPerThread" : true,
        "callStacks" : [
          {
            "threadAttributed" : true,
            "callStackRootFrames" : [
              {
                "binaryUUID" : "70B89F27-1634-3580-A695-57CDB41D7743",
                "offsetIntoBinaryTextSegment" : 165304,
                "sampleCount" : 1,
                "binaryName" : "ExampleApp",
                "address" : 7170766264,
                "subFrames" : [
                  {
                    "binaryUUID" : "4E1C2B3A-5D6F-3789-8A9B-0C1D2E3F4A5B",
                    "offsetIntoBinaryTextSegment" : 9832,
                    "sampleCount" : 1,
                    "binaryName" : "libsystem_kernel.dylib",
                    "address" : 6979321212
                  }
                ]
              }
            ]
          }
        ]
      },
      "diagnosticMetaData" : {
        "appBuildVersion" : "4512",
        "appVersion" : "2026.11.3",
        "regionFormat" : "US",
        "exceptionType" : 1,
        "osVersion" : "iPhone OS 26.1 (23B85)",
        "deviceType" : "iPhone17,1",
        "exceptionCode" : 1,
        "signal" : 11,
        "pid" : 98765,
        "bundleIdentifier" : "com.example.app",
        "isTestFlightApp" : false,
        "lowPowerModeEnabled" : false,
        "platformArchitecture" : "arm64e",
        "virtualMemoryRegionInfo" : "0x0000000107a5c000 is not in any region.  Bytes before following region: 4000000000 REGION TYPE START - END [ VSIZE] PRT/MAX SHRMOD REGION DETAIL UNUSED SPACE AT START ---> __TEXT 0000000100000000-0000000100008000 [ 32K] r-x/r-x SM=COW /private/var/containers/Bundle/Application/1F6E4C59-6A1E-4C3B-9E5D-0D2C5B1A7E33/ExampleApp.app/ExampleApp",
        "terminationReason" : "Namespace SIGNAL, Code 11 Segmentation fault: 11"
      }
    }
  ],
  "timeStampBegin" : "2026-11-10 13:19:00 +0000"
}`

const testingMetricKitCrashLine = `2026-11-10T13:19:00Z..2026-11-10T13:19:00Z metrickit crash process=com.example.app app=2026.11.3 build=4512 os="iPhone OS 26.1 (23B85)" exception_type=EXC_BAD_ACCESS(1) exception_code=0x1 signal=SIGSEGV(11) termination_reason="Namespace SIGNAL, Code 11 Segmentation fault: 11"`

// A day's metric payload as MXMetricPayload.jsonRepresentation writes one:
// the exit metric among the others, the device's type and region in its
// metaData, and a count of a later os's reason. One more key is not a count's.
const testingMetricKitMetricPayload = `{
  "appVersion" : "2026.11.3",
  "includesMultipleApplicationVersions" : false,
  "timeStampBegin" : "2026-11-09 00:00:00 +0000",
  "timeStampEnd" : "2026-11-09 23:59:00 +0000",
  "metaData" : {
    "appBuildVersion" : "4512",
    "osVersion" : "iPhone OS 26.1 (23B85)",
    "regionFormat" : "US",
    "deviceType" : "iPhone17,1",
    "platformArchitecture" : "arm64e"
  },
  "memoryMetrics" : {
    "peakMemoryUsage" : "180,000 kB",
    "averageSuspendedMemory" : {"averageValue" : "40,000 kB", "standardDeviation" : 0, "sampleCount" : 500}
  },
  "networkTransferMetrics" : {
    "cumulativeCellularDownload" : "80,000 kB",
    "cumulativeWifiDownload" : "1,000,000 kB"
  },
  "applicationExitMetrics" : {
    "foregroundExitData" : {
      "cumulativeBadAccessExitCount" : 0,
      "cumulativeAbnormalExitCount" : 0,
      "cumulativeMemoryResourceLimitExitCount" : 2,
      "cumulativeNormalAppExitCount" : 5,
      "cumulativeCPUResourceLimitExitCount" : 0,
      "cumulativeIllegalInstructionExitCount" : 0,
      "cumulativeAppWatchdogExitCount" : 1
    },
    "backgroundExitData" : {
      "cumulativeAppWatchdogExitCount" : 0,
      "cumulativeMemoryResourceLimitExitCount" : 0,
      "cumulativeBackgroundURLSessionCompletionTimeoutExitCount" : 0,
      "cumulativeBackgroundFetchCompletionTimeoutExitCount" : 0,
      "cumulativeAbnormalExitCount" : 0,
      "cumulativeSuspendedWithLockedFileExitCount" : 1,
      "cumulativeIllegalInstructionExitCount" : 0,
      "cumulativeMemoryPressureExitCount" : 7,
      "cumulativeBadAccessExitCount" : 0,
      "cumulativeCPUResourceLimitExitCount" : 0,
      "cumulativeBackgroundTaskAssertionTimeoutExitCount" : 0,
      "cumulativeNormalAppExitCount" : 0,
      "cumulativeNewReasonExitCount" : 3,
      "cumulative/Users/alice/ExitCount" : 9
    }
  }
}`

var testingMetricKitExitLines = []string{
	`2026-11-09T00:00:00Z..2026-11-09T23:59:00Z metrickit foreground_exits app=2026.11.3 build=4512 os="iPhone OS 26.1 (23B85)" total=8 normal=5 memory_resource_limit=2 app_watchdog=1`,
	`2026-11-09T00:00:00Z..2026-11-09T23:59:00Z metrickit background_exits app=2026.11.3 build=4512 os="iPhone OS 26.1 (23B85)" total=11 memory_pressure=7 suspended_with_locked_file=1 other=3`,
}

// The clock the pure tests run at.
var testingExitDiagnosticsNowMillis = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC).UnixMilli()

// A time of the tests in unix milliseconds.
func testingUtcMillis(year int, month time.Month, day int, hour int, minute int) int64 {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC).UnixMilli()
}

// A minimal crash payload whose termination reason names n, so each n is a
// payload of its own.
func testingMetricKitCrashPayloadNamed(n int) string {
	return fmt.Sprintf(`{"crashDiagnostics":[{"diagnosticMetaData":{"signal":6,"terminationReason":"abort %d"}}]}`, n)
}

// Every byte of the line is printable ascii.
func testingExitDiagnosticsLineIsPrintableAscii(line string) bool {
	for i := 0; i < len(line); i += 1 {
		if line[i] < 0x20 || 0x7e < line[i] {
			return false
		}
	}
	return true
}

// A crash is one line: when, which process and app build, the os, the
// exception, the signal and the reason. Of the call stack, the memory region,
// the device and the region nothing is left.
func TestMetricKitCrashLineKeepsWhyAndNothingElse(t *testing.T) {
	settings := defaultExitDiagnosticsSettings()
	lines, _, ok := metricKitExitDiagnosticLines(metricKitDiagnosticPayloadKind, testingMetricKitCrashPayload, 0, 0, exitDiagnosticsState{}, testingExitDiagnosticsNowMillis, settings)
	if !ok || len(lines) != 1 || lines[0] != testingMetricKitCrashLine {
		t.Fatalf("lines = %q (ok %t), want [%q]", lines, ok, testingMetricKitCrashLine)
	}
	for _, left := range []string{"70B89F27", "7170766264", "165304", "libsystem", "1F6E4C59", "/private", "0x0000000107a5c000", "iPhone17,1", "arm64e", "98765", "region", "US "} {
		if strings.Contains(lines[0], left) {
			t.Errorf("the line holds %q: %q", left, lines[0])
		}
	}

	// the payload's own times, which the app passes, win over the json's
	begin := testingUtcMillis(2026, 11, 10, 13, 18)
	end := testingUtcMillis(2026, 11, 10, 13, 20)
	lines, _, _ = metricKitExitDiagnosticLines(metricKitDiagnosticPayloadKind, testingMetricKitCrashPayload, begin, end, exitDiagnosticsState{}, testingExitDiagnosticsNowMillis, settings)
	want := strings.Replace(testingMetricKitCrashLine, "2026-11-10T13:19:00Z..2026-11-10T13:19:00Z", "2026-11-10T13:18:00Z..2026-11-10T13:20:00Z", 1)
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("with the payload's times, lines = %q, want [%q]", lines, want)
	}
}

// A diagnostic built to carry everything a report must never send: paths with
// a user's name, the install's uuid in the container path, an email address,
// memory addresses, an ecid, network and mac addresses, a long time stamp,
// line breaks and other control characters to forge a line, non-ascii text,
// quotes to end the field, and 14 KB of text; metadata with a path, a forged
// log line and an email where versions belong; and an objective-c message
// with a user's data. None of it reaches the line, and the line stays one
// bounded line of printable ascii. Every value is synthetic.
func TestMetricKitHostileDiagnosticLeavesNothingSensitive(t *testing.T) {
	settings := defaultExitDiagnosticsSettings()
	hostileReason := "Namespace DYLD, Code 1 Library missing\n" +
		"Library not loaded: @rpath/Tunnel.framework/Tunnel\r\n" +
		"  Referenced from: <1F6E4C59-6A1E-4C3B-9E5D-0D2C5B1A7E33> /private/var/containers/Bundle/Application/2A7F5D6E-1B2C-4D3E-8F9A-0B1C2D3E4F5A/ExampleApp.app/ExampleApp\t" +
		"Reason: tried: 'C:\\Users\\alice\\Tunnel' '~alice/Tunnel' (no such file) owner alice@example.com " +
		"at 0x00000001045a8000 \"quoted\" ecid 00008110-001A2B3C4D5E801E " +
		"from 192.0.2.23:443 and [2001:db8::1]:443 via 0a:1b:2c:3d:4e:5f code 0x8badf00d at 1731244740123 " +
		"\x00\x1b[31mred\u2028\u2029next\x7f \u00e9 " + strings.Repeat("filler ", 2000)
	payload := map[string]any{
		"timeStampBegin": "2026-11-10 13:19:00 +0000",
		"timeStampEnd":   "2026-11-10 13:19:00 +0000",
		"crashDiagnostics": []any{
			map[string]any{
				"callStackTree": map[string]any{
					"callStacks": []any{map[string]any{"callStackRootFrames": []any{map[string]any{
						"binaryUUID": "70B89F27-1634-3580-A695-57CDB41D7743",
						"binaryName": "alice-tools",
						"address":    7170766264,
					}}}},
				},
				"diagnosticMetaData": map[string]any{
					"appVersion":              "/Users/alice/2026.11.3",
					"appBuildVersion":         "4512\nI1110 00:00:00.000000 1 forged.go:1] forged",
					"osVersion":               "iPhone OS 26.1 (23B85)\u2028alice@example.com",
					"bundleIdentifier":        "com.example.app/../../alice",
					"deviceType":              "Alice's iPhone 17",
					"regionFormat":            "US",
					"pid":                     98765,
					"exceptionType":           1,
					"exceptionCode":           4400000000,
					"signal":                  11,
					"virtualMemoryRegionInfo": "0x0000000107a5c000 /Users/alice/Library",
					"hangDuration":            "20 sec /Users/alice",
					"terminationReason":       hostileReason,
					"exceptionReason": map[string]any{
						"composedMessage": "*** -[__NSDictionaryM setObject:forKey:]: key cannot be nil for alice@example.com at /Users/alice",
						"formatString":    "*** %s: key cannot be nil for %@",
						"arguments":       []any{"alice@example.com", "/Users/alice"},
						"exceptionType":   "Uncaught",
						"className":       "NSException",
						"exceptionName":   "NSInvalidArgumentException",
					},
				},
			},
		},
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	lines, _, ok := metricKitExitDiagnosticLines(metricKitDiagnosticPayloadKind, string(payloadBytes), 0, 0, exitDiagnosticsState{}, testingExitDiagnosticsNowMillis, settings)
	if !ok || len(lines) != 1 {
		t.Fatalf("lines = %q (ok %t), want one", lines, ok)
	}
	line := lines[0]

	if !testingExitDiagnosticsLineIsPrintableAscii(line) {
		t.Errorf("the line is not one line of printable ascii: %q", line)
	}
	if settings.LineMaxLength < len(line) {
		t.Errorf("the line is %d bytes, over %d", len(line), settings.LineMaxLength)
	}
	for _, sensitive := range []string{
		"alice", "Alice", "Users", "/private", "/", "\\", "@", "~",
		"1F6E4C59", "6A1E", "2A7F5D6E", "70B89F27",
		"1045a8000", "0x00000001", "107a5c000", "7170766264", "4400000000",
		"001A2B3C4D5E801E", "192.0.2.23", "2001:db8", "0a:1b:2c", "1731244740123",
		"98765", "iPhone 17", "region", "setObject", "key cannot be nil", "forged",
		"quoted\"", "\u00e9", "filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler filler",
	} {
		if strings.Contains(line, sensitive) {
			t.Errorf("the line holds %q: %q", sensitive, line)
		}
	}
	for _, kept := range []string{
		"2026-11-10T13:19:00Z..2026-11-10T13:19:00Z metrickit crash ",
		" exception_type=EXC_BAD_ACCESS(1) exception_code=omitted signal=SIGSEGV(11)",
		" objc_exception=NSInvalidArgumentException objc_class=NSException ",
		` termination_reason="Namespace DYLD, Code 1 Library missing Library not loaded: <path> Referenced from: <<uuid>> <path> Reason: tried: <path> <path> (no such file) owner <email> at <address> 'quoted' `,
	} {
		if !strings.Contains(line, kept) {
			t.Errorf("the line lacks %q: %q", kept, line)
		}
	}
	// what had no clean value is left out, not mangled
	for _, field := range []string{" process=", " app=", " build=", " os=", " hang_duration="} {
		if strings.Contains(line, field) {
			t.Errorf("the line has%s from a value that is not one: %q", field, line)
		}
	}

	reason, ok := strings.CutPrefix(line[strings.Index(line, ` termination_reason="`):], ` termination_reason="`)
	if !ok || !strings.HasSuffix(reason, `..."`) {
		t.Fatalf("the reason is not the line's last, cut field: %q", line)
	}
	reason = strings.TrimSuffix(reason, `"`)
	if settings.ReasonMaxLength < len(reason) {
		t.Errorf("the reason is %d bytes, over %d: %q", len(reason), settings.ReasonMaxLength, reason)
	}
}

// Each kind of thing a reason can carry is replaced by a word that says what
// was there, short codes stay, and the reason is cut between words.
func TestSanitizeExitDiagnosticsReason(t *testing.T) {
	settings := defaultExitDiagnosticsSettings()
	for _, c := range []struct {
		name   string
		reason string
		want   string
	}{
		{
			name:   "a signal",
			reason: "Namespace SIGNAL, Code 11 Segmentation fault: 11",
			want:   "Namespace SIGNAL, Code 11 Segmentation fault: 11",
		},
		{
			name:   "the watchdog's code",
			reason: "Namespace SPRINGBOARD, Code 0x8badf00d",
			want:   "Namespace SPRINGBOARD, Code 0x8badf00d",
		},
		{
			name:   "a watchdog report",
			reason: "<RBSTerminateContext| domain:10 code:0x8BADF00D explanation:scene-create watchdog transgression: application<com.example.app>:1234 exhausted real (wall clock) time allowance of 19.97 seconds",
			want:   "<RBSTerminateContext| domain:10 code:0x8BADF00D explanation:scene-create watchdog transgression: application<com.example.app>:1234 exhausted real (wall clock) time allowance of 19.97 seconds",
		},
		{
			name:   "line breaks and control characters",
			reason: "line\nbreak\r\ttab\x00nul\x1b[31m\u2028sep\u2029x\x7fdel \u00e9 \xff",
			want:   "line break tab nul [31m sep x del",
		},
		{
			name:   "paths",
			reason: "dyld: /private/var/containers/Bundle/Application/X/ExampleApp.app/ExampleApp and C:\\Users\\alice and ~alice and file:///etc/hosts",
			want:   "dyld: <path> and <path> and <path> and <path>",
		},
		{
			name:   "an email address",
			reason: "owner alice@example.com",
			want:   "owner <email>",
		},
		{
			name:   "a uuid",
			reason: "id 1F6E4C59-6A1E-4C3B-9E5D-0D2C5B1A7E33.",
			want:   "id <uuid>.",
		},
		{
			name:   "a mac address",
			reason: "via 0a:1b:2c:3d:4e:5f and 0A-1B-2C-3D-4E-5F",
			want:   "via <mac> and <mac>",
		},
		{
			name:   "ipv4",
			reason: "to 192.0.2.23:443 and 198.51.100.1",
			want:   "to <ip> and <ip>",
		},
		{
			name:   "ipv6",
			reason: "to [2001:db8::1]:443 and 2001:db8::3%en0 and 2001:db8:0:0:0:0:0:2",
			want:   "to <ip> and <ip> and <ip>",
		},
		{
			name:   "memory addresses",
			reason: "at 0x00000001045a8000 or 0X7FFF5FBFF8A0 but not 0xdead10cc",
			want:   "at <address> or <address> but not 0xdead10cc",
		},
		{
			name:   "long numbers",
			reason: "tick 1731244740123 and Code 2343432205 but pid 4242",
			want:   "tick <number> and Code <number> but pid 4242",
		},
		{
			name:   "long hex runs",
			reason: "ecid 00008110-001A2B3C4D5E801E binary 70B89F2716343580A69557CDB41D7743",
			want:   "ecid 00008110-<hex> binary <hex>",
		},
		{
			name:   "quotes",
			reason: `said "hi"`,
			want:   `said 'hi'`,
		},
		{
			name:   "nothing",
			reason: " \n\t ",
			want:   "",
		},
		{
			name:   "a time is not an address",
			reason: "at 13:19:00 on 2026-11-10",
			want:   "at 13:19:00 on 2026-11-10",
		},
		{
			name:   "a c++ type is not an address",
			reason: "terminating due to uncaught exception of type std::runtime_error: boom",
			want:   "terminating due to uncaught exception of type std::runtime_error: boom",
		},
	} {
		if got := sanitizeExitDiagnosticsReason(c.reason, settings); got != c.want {
			t.Errorf("%s: sanitizeExitDiagnosticsReason(%q) = %q, want %q", c.name, c.reason, got, c.want)
		}
	}

	// a long reason is cut between words to the bound, ending in "..."
	long := sanitizeExitDiagnosticsReason(strings.Repeat("word ", 100), settings)
	if settings.ReasonMaxLength < len(long) || !strings.HasSuffix(long, "...") {
		t.Errorf("a long reason became %d bytes: %q", len(long), long)
	}
	if words := strings.TrimSuffix(long, "..."); strings.Trim(strings.ReplaceAll(words, "word", ""), " ") != "" || strings.HasSuffix(words, " ") {
		t.Errorf("a long reason was not cut between words: %q", long)
	}

	// only ReasonReadMaxLength bytes are read, and a word the read cuts is
	// dropped: what is left of it may have lost what marks it as a path or an
	// email address
	cutWord := strings.Repeat("\n", settings.ReasonReadMaxLength-5) + " alice@example.com"
	if got := sanitizeExitDiagnosticsReason(cutWord, settings); got != "" {
		t.Errorf("a reason cut inside an email address became %q, want nothing", got)
	}
	if got := sanitizeExitDiagnosticsReason(strings.Repeat("\n", settings.ReasonReadMaxLength-5)+" ok", settings); got != "ok" {
		t.Errorf("a reason under the read bound became %q, want ok", got)
	}
}

// Hangs, cpu and disk write exceptions, slow launches and a kind a later os
// adds each get a line, with their measures; a kind under a key that is not a
// name, and a measure that is not one, are left out.
func TestMetricKitDiagnosticKinds(t *testing.T) {
	payload := `{
  "timeStampBegin" : "2026-11-10 08:00:00 +0000",
  "timeStampEnd" : "2026-11-10 08:00:00 +0000",
  "crashDiagnostics" : [],
  "hangDiagnostics" : [
    {"callStackTree" : {}, "diagnosticMetaData" : {"appVersion" : "2026.11.3", "appBuildVersion" : "4512", "osVersion" : "iPhone OS 26.1 (23B85)", "hangDuration" : "20 sec"}},
    {"callStackTree" : {}, "diagnosticMetaData" : {"hangDuration" : "20 sec /Users/alice"}}
  ],
  "cpuExceptionDiagnostics" : [{"callStackTree" : {}, "diagnosticMetaData" : {"totalCPUTime" : "90 sec", "totalSampledTime" : "180 sec"}}],
  "diskWriteExceptionDiagnostics" : [{"callStackTree" : {}, "diagnosticMetaData" : {"writesCaused" : "1,073,741 KB"}}],
  "appLaunchDiagnostics" : [{"callStackTree" : {}, "diagnosticMetaData" : {"launchDuration" : "21.5 sec"}}],
  "memoryExceptionDiagnostics" : [{"diagnosticMetaData" : {"appVersion" : "2026.11.3"}}],
  "alice/Users Diagnostics" : [{"diagnosticMetaData" : {}}]
}`
	lines, _, ok := metricKitExitDiagnosticLines(metricKitDiagnosticPayloadKind, payload, 0, 0, exitDiagnosticsState{}, testingExitDiagnosticsNowMillis, defaultExitDiagnosticsSettings())
	timeRange := "2026-11-10T08:00:00Z..2026-11-10T08:00:00Z metrickit "
	want := []string{
		timeRange + `hang app=2026.11.3 build=4512 os="iPhone OS 26.1 (23B85)" hang_duration="20 sec"`,
		timeRange + `hang`,
		timeRange + `cpu_exception cpu_time="90 sec" sampled_time="180 sec"`,
		timeRange + `disk_write_exception writes="1,073,741 KB"`,
		timeRange + `app_launch launch_duration="21.5 sec"`,
		timeRange + `memory_exception app=2026.11.3`,
	}
	if !ok || !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines = %q (ok %t), want %q", lines, ok, want)
	}

	for key, want := range map[string]string{
		"memoryExceptionDiagnostics":            "memory_exception",
		"fooDiagnostics":                        "foo",
		"Diagnostics":                           "",
		"crash/Diagnostics":                     "",
		"crash Diagnostics":                     "",
		"crashDiagnostic":                       "",
		strings.Repeat("a", 25) + "Diagnostics": "",
	} {
		if got := metricKitDiagnosticKindName(key); got != want {
			t.Errorf("metricKitDiagnosticKindName(%q) = %q, want %q", key, got, want)
		}
	}
}

// A day's exits are two lines, the foreground's and the background's: the
// total and each reason counted, a count a later os adds as other, and the
// device's type and region left out.
func TestMetricKitExitMetricLines(t *testing.T) {
	settings := defaultExitDiagnosticsSettings()
	lines, _, ok := metricKitExitDiagnosticLines(metricKitMetricPayloadKind, testingMetricKitMetricPayload, 0, 0, exitDiagnosticsState{}, testingExitDiagnosticsNowMillis, settings)
	if !ok || !reflect.DeepEqual(lines, testingMetricKitExitLines) {
		t.Fatalf("lines = %q (ok %t), want %q", lines, ok, testingMetricKitExitLines)
	}
	for _, line := range lines {
		for _, left := range []string{"iPhone17,1", "arm64e", "180,000", "alice", "=9"} {
			if strings.Contains(line, left) {
				t.Errorf("the line holds %q: %q", left, line)
			}
		}
	}

	for _, c := range []struct {
		name    string
		payload string
		want    []string
	}{
		{
			name:    "no exits",
			payload: `{"includesMultipleApplicationVersions" : true, "applicationExitMetrics" : {"foregroundExitData" : {"cumulativeNormalAppExitCount" : 0}}}`,
			want:    []string{"unknown..unknown metrickit foreground_exits app_versions=multiple total=0"},
		},
		{
			name:    "no exit metric",
			payload: `{"appVersion" : "2026.11.3", "memoryMetrics" : {"peakMemoryUsage" : "180,000 kB"}}`,
			want:    []string{},
		},
		{
			name:    "counts that are not counts",
			payload: `{"applicationExitMetrics" : {"foregroundExitData" : {"cumulativeNormalAppExitCount" : "x", "cumulativeBadAccessExitCount" : -3, "cumulativeAbnormalExitCount" : 1.5, "cumulativeAppWatchdogExitCount" : "2"}}}`,
			want:    []string{"unknown..unknown metrickit foreground_exits total=2 app_watchdog=2"},
		},
		{
			name:    "an exit metric that is not an object",
			payload: `{"applicationExitMetrics" : [1, 2], "metaData" : {"osVersion" : "iPhone OS 26.1 (23B85)"}}`,
			want:    []string{},
		},
	} {
		lines, _, ok := metricKitExitDiagnosticLines(metricKitMetricPayloadKind, c.payload, 0, 0, exitDiagnosticsState{}, testingExitDiagnosticsNowMillis, settings)
		if !ok || len(lines) != len(c.want) || (0 < len(c.want) && !reflect.DeepEqual(lines, c.want)) {
			t.Errorf("%s: lines = %q (ok %t), want %q", c.name, lines, ok, c.want)
		}
	}
}

// A payload is logged once: again, printed another way, it adds nothing,
// while another payload of the same time, or an earlier one that comes late,
// is logged. The record keeps the newest payloads, and one that ends before
// the oldest it no longer keeps -- the watermark -- counts as logged.
func TestMetricKitPayloadIsLoggedOnce(t *testing.T) {
	settings := defaultExitDiagnosticsSettings()
	now := testingExitDiagnosticsNowMillis
	end := testingUtcMillis(2026, 11, 10, 13, 19)
	kind := metricKitDiagnosticPayloadKind
	minute := time.Minute.Milliseconds()

	lines, state, ok := metricKitExitDiagnosticLines(kind, testingMetricKitCrashPayload, end, end, exitDiagnosticsState{}, now, settings)
	if !ok || len(lines) != 1 || len(state.Payloads) != 1 {
		t.Fatalf("the first time: lines %q, ok %t, record %+v", lines, ok, state)
	}

	// again, as MetricKit's past payloads come at each start
	if lines, next, ok := metricKitExitDiagnosticLines(kind, testingMetricKitCrashPayload, end, end, state, now, settings); ok || len(lines) != 0 || !reflect.DeepEqual(next, state) {
		t.Fatalf("again: lines %q, ok %t", lines, ok)
	}
	// again, printed without the spaces
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(testingMetricKitCrashPayload)); err != nil {
		t.Fatal(err)
	}
	if lines, _, ok := metricKitExitDiagnosticLines(kind, compact.String(), end, end, state, now, settings); ok || len(lines) != 0 {
		t.Fatalf("again, compact: lines %q, ok %t", lines, ok)
	}
	// the same json is another payload as a metric payload
	if _, _, ok := metricKitExitDiagnosticLines(metricKitMetricPayloadKind, testingMetricKitCrashPayload, end, end, state, now, settings); !ok {
		t.Fatal("the diagnostic payload's json as a metric payload counted as logged")
	}

	// another payload that ends at the same time
	lines, state, ok = metricKitExitDiagnosticLines(kind, testingMetricKitCrashPayloadNamed(1), end, end, state, now, settings)
	if !ok || len(lines) != 1 {
		t.Fatalf("another payload of the same time: lines %q, ok %t", lines, ok)
	}
	// an earlier payload that comes late
	earlier := end - time.Hour.Milliseconds()
	lines, state, ok = metricKitExitDiagnosticLines(kind, testingMetricKitCrashPayloadNamed(2), earlier, earlier, state, now, settings)
	if !ok || len(lines) != 1 || len(state.Payloads) != 3 || state.FloorMillis != 0 {
		t.Fatalf("an earlier payload: lines %q, ok %t, record %+v", lines, ok, state)
	}

	// the record keeps the newest payloads: past them the floor rises to the
	// newest it dropped, and an old payload below it counts as logged
	fewSettings := defaultExitDiagnosticsSettings()
	fewSettings.StateMaxPayloadCount = 4
	state = exitDiagnosticsState{}
	first := testingUtcMillis(2026, 11, 5, 0, 0)
	for i := range 10 {
		at := first + int64(i)*minute
		if _, state, ok = metricKitExitDiagnosticLines(kind, testingMetricKitCrashPayloadNamed(100+i), at, at, state, now, fewSettings); !ok {
			t.Fatalf("payload %d was not logged", i)
		}
	}
	if len(state.Payloads) != 4 {
		t.Fatalf("the record keeps %d payloads, want 4", len(state.Payloads))
	}
	if wantFloor := first + 5*minute; state.FloorMillis != wantFloor {
		t.Fatalf("the floor is %s, want %s", exitDiagnosticsTime(state.FloorMillis), exitDiagnosticsTime(wantFloor))
	}
	old := first + 2*minute
	if lines, _, ok := metricKitExitDiagnosticLines(kind, testingMetricKitCrashPayloadNamed(9999), old, old, state, now, fewSettings); ok || len(lines) != 0 {
		t.Fatalf("a new payload older than the watermark: lines %q, ok %t, want it skipped", lines, ok)
	}
	atFloor := state.FloorMillis
	if _, _, ok := metricKitExitDiagnosticLines(kind, testingMetricKitCrashPayloadNamed(9998), atFloor, atFloor, state, now, fewSettings); !ok {
		t.Fatal("a new payload ending at the watermark was skipped")
	}

	// past StateRetention the record drops a payload and the floor rises to it
	state = exitDiagnosticsState{}
	_, state, _ = metricKitExitDiagnosticLines(kind, testingMetricKitCrashPayloadNamed(1), first, first, state, now, settings)
	later := first + settings.StateRetention.Milliseconds() + 24*60*minute
	_, state, _ = metricKitExitDiagnosticLines(kind, testingMetricKitCrashPayloadNamed(2), later, later, state, later, settings)
	if len(state.Payloads) != 1 || state.FloorMillis != first {
		t.Fatalf("past the retention the record is %+v, want only the newer payload and the floor at the older", state)
	}
	if _, _, ok := metricKitExitDiagnosticLines(kind, testingMetricKitCrashPayloadNamed(3), first-minute, first-minute, state, later, settings); ok {
		t.Fatal("a payload from before the dropped one was logged")
	}

	// a time far ahead of the clock never lifts the floor over what comes next
	yearsAhead := now + 10*365*24*60*minute
	state = exitDiagnosticsState{FloorMillis: yearsAhead}
	if _, _, ok := metricKitExitDiagnosticLines(kind, testingMetricKitCrashPayloadNamed(4), now-minute, now-minute, state, now, settings); !ok {
		t.Fatal("a floor raised while the clock was years ahead skipped a payload of now")
	}
	_, state, _ = metricKitExitDiagnosticLines(kind, testingMetricKitCrashPayloadNamed(5), now, yearsAhead, exitDiagnosticsState{}, now, settings)
	if want := now + settings.StateMaxAhead.Milliseconds(); len(state.Payloads) != 1 || state.Payloads[0].EndMillis != want {
		t.Fatalf("a payload ending years ahead was recorded as %+v, want it at %s", state, exitDiagnosticsTime(want))
	}
	_, state, _ = metricKitExitDiagnosticLines(kind, testingMetricKitCrashPayloadNamed(6), now-minute, now-minute, state, now, settings)
	if len(state.Payloads) != 2 || state.FloorMillis != 0 {
		t.Fatalf("after a payload ending years ahead the record is %+v, want both payloads and no floor", state)
	}

	// a payload without times is logged once, known by its digest
	noTimes := testingMetricKitCrashPayloadNamed(7)
	lines, state, ok = metricKitExitDiagnosticLines(kind, noTimes, 0, 0, exitDiagnosticsState{}, now, settings)
	if !ok || len(lines) != 1 || !strings.HasPrefix(lines[0], "unknown..unknown metrickit crash ") {
		t.Fatalf("a payload without times: lines %q, ok %t", lines, ok)
	}
	if _, _, ok := metricKitExitDiagnosticLines(kind, noTimes, 0, 0, state, now+minute, settings); ok {
		t.Fatal("a payload without times was logged twice")
	}
}

// Json that is not a payload writes nothing and records nothing, whatever
// it is; json of a payload's shape with values of the wrong kinds writes only
// what has a clean value. Nothing panics.
func TestMetricKitMalformedPayloadsWriteNothing(t *testing.T) {
	settings := defaultExitDiagnosticsSettings()
	state := exitDiagnosticsState{
		FloorMillis: 7,
		Payloads: []exitDiagnosticsLoggedPayload{
			{Digest: "d", EndMillis: testingExitDiagnosticsNowMillis - 1000},
		},
	}
	for _, payload := range []string{
		"", " ", "{", "}", "null", "[]", "[{}]", "42", `"text"`, "true",
		`{"crashDiagnostics":`, "\xff\xfe{}", `{"a":1}{"b":2}`,
		strings.Repeat("[", 20000) + strings.Repeat("]", 20000),
		`{"crashDiagnostics":` + strings.Repeat("[", 20000) + strings.Repeat("]", 20000) + `}`,
	} {
		for _, kind := range []metricKitPayloadKind{metricKitDiagnosticPayloadKind, metricKitMetricPayloadKind} {
			lines, next, ok := metricKitExitDiagnosticLines(kind, payload, 1, 2, state, testingExitDiagnosticsNowMillis, settings)
			if ok || len(lines) != 0 || !reflect.DeepEqual(next, state) {
				t.Errorf("%s %.40q: lines %q, ok %t, record %+v", kind, payload, lines, ok, next)
			}
		}
	}
	if _, _, ok := metricKitExitDiagnosticLines(metricKitDiagnosticPayloadKind, "{}"+strings.Repeat(" ", settings.PayloadMaxByteCount), 1, 2, state, testingExitDiagnosticsNowMillis, settings); ok {
		t.Error("a payload over the size bound was read")
	}

	for _, c := range []struct {
		payload string
		want    []string
	}{
		{
			payload: `{}`,
			want:    nil,
		},
		{
			payload: `{"crashDiagnostics" : "x", "hangDiagnostics" : {"a" : 1}}`,
			want:    nil,
		},
		{
			payload: `{"crashDiagnostics" : [1, "x", null, [], true]}`,
			want:    nil,
		},
		{
			payload: `{"crashDiagnostics" : [{"diagnosticMetaData" : 5, "signal" : "eleven", "exceptionType" : {}, "exceptionCode" : -1, "terminationReason" : ["x"]}]}`,
			want:    []string{"unknown..unknown metrickit crash exception_code=omitted"},
		},
		{
			payload: `{"crashDiagnostics" : [{"diagnosticMetaData" : {"signal" : 99999, "exceptionType" : 99, "exceptionCode" : "4294967295", "terminationReason" : 42, "appVersion" : 7}}]}`,
			want:    []string{`unknown..unknown metrickit crash app=7 exception_type=unknown(99) exception_code=0xffffffff termination_reason="42"`},
		},
	} {
		lines, next, ok := metricKitExitDiagnosticLines(metricKitDiagnosticPayloadKind, c.payload, 0, 0, state, testingExitDiagnosticsNowMillis, settings)
		if !ok || !reflect.DeepEqual(lines, c.want) && !(len(lines) == 0 && len(c.want) == 0) || len(next.Payloads) != 2 {
			t.Errorf("%q: lines %q (ok %t, record %+v), want %q", c.payload, lines, ok, next, c.want)
		}
	}
}

// A payload of a hundred crashes, a device in a crash loop, adds a bounded
// number of lines and one that counts the rest.
func TestMetricKitPayloadLineCap(t *testing.T) {
	settings := defaultExitDiagnosticsSettings()
	records := []string{}
	for i := range 100 {
		records = append(records, fmt.Sprintf(`{"diagnosticMetaData":{"signal":6,"terminationReason":"abort %d"}}`, i))
	}
	payload := `{"crashDiagnostics":[` + strings.Join(records, ",") + `]}`
	lines, _, ok := metricKitExitDiagnosticLines(metricKitDiagnosticPayloadKind, payload, 0, 0, exitDiagnosticsState{}, testingExitDiagnosticsNowMillis, settings)
	if !ok || len(lines) != settings.PayloadMaxLineCount+1 {
		t.Fatalf("%d lines (ok %t), want %d", len(lines), ok, settings.PayloadMaxLineCount+1)
	}
	if want := fmt.Sprintf("unknown..unknown metrickit omitted records=%d", 100-settings.PayloadMaxLineCount); lines[len(lines)-1] != want {
		t.Fatalf("the last line is %q, want %q", lines[len(lines)-1], want)
	}
	last := settings.PayloadMaxLineCount - 1
	if want := fmt.Sprintf(`unknown..unknown metrickit crash signal=SIGABRT(6) termination_reason="abort %d"`, last); lines[last] != want {
		t.Fatalf("line %d is %q, want %q", last, lines[last], want)
	}
}

// The app's calls write the file in its log directory, which the upload of
// either process carries; a payload that comes again adds nothing, and the
// record of what was logged stays on the device.
func TestLogMetricKitPayloadsWriteTheFileTheUploadCarries(t *testing.T) {
	restoreTestingLogDir(t)

	root := t.TempDir()
	if err := SetLogDirForProcess(root, "app"); err != nil {
		t.Fatalf("SetLogDirForProcess: %v", err)
	}

	if n := LogMetricKitDiagnosticPayload(testingMetricKitCrashPayload, 0, 0); n != 1 {
		t.Fatalf("LogMetricKitDiagnosticPayload = %d, want 1", n)
	}
	if n := LogMetricKitDiagnosticPayload(testingMetricKitCrashPayload, 0, 0); n != 0 {
		t.Fatalf("LogMetricKitDiagnosticPayload again = %d, want 0", n)
	}
	if n := LogMetricKitMetricPayload(testingMetricKitMetricPayload, 0, 0); n != 2 {
		t.Fatalf("LogMetricKitMetricPayload = %d, want 2", n)
	}
	if n := LogMetricKitMetricPayload("not json", 0, 0); n != 0 {
		t.Fatalf("LogMetricKitMetricPayload of not json = %d, want 0", n)
	}

	path := filepath.Join(root, "app", exitDiagnosticsFileName)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantContent := testingMetricKitCrashLine + "\n" + strings.Join(testingMetricKitExitLines, "\n") + "\n"
	if string(content) != wantContent {
		t.Fatalf("the file is\n%s\nwant\n%s", content, wantContent)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != LocalStorageFilePermissions {
		t.Errorf("the file's mode is %v (%v), want %v", info.Mode().Perm(), err, os.FileMode(LocalStorageFilePermissions))
	}
	stateContent, err := os.ReadFile(filepath.Join(root, "app", exitDiagnosticsStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var state exitDiagnosticsState
	if err := json.Unmarshal(stateContent, &state); err != nil || len(state.Payloads) != 2 {
		t.Errorf("the record is %s (%v), want the two payloads", stateContent, err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("a write left %s.tmp behind (%v)", exitDiagnosticsFileName, err)
	}

	// the app's upload, while the tunnel is down, then the extension's from the
	// same root while it is up
	for _, processName := range []string{"app", "extension"} {
		if processName != "app" {
			if err := SetLogDirForProcess(root, processName); err != nil {
				t.Fatalf("SetLogDirForProcess(%q): %v", processName, err)
			}
		}
		zipPath, err := zipUploadLogs(uploadLogsMaxByteCount, nil, connect.DefaultLogger())
		if err != nil {
			t.Fatalf("zipUploadLogs = %v", err)
		}
		zipBytes, err := os.ReadFile(zipPath)
		if err != nil {
			t.Fatal(err)
		}
		os.Remove(zipPath)
		entryNameContents := readTestingZip(t, zipBytes)
		if got := entryNameContents["app/"+exitDiagnosticsFileName]; got != wantContent {
			t.Errorf("the %s's upload has app/%s = %q, want the file", processName, exitDiagnosticsFileName, got)
		}
		for entryName := range entryNameContents {
			if strings.Contains(entryName, exitDiagnosticsStateFileName) {
				t.Errorf("the %s's upload has %q", processName, entryName)
			}
		}
	}
}

// Before a log directory is set there is nowhere to write: nothing is
// written and nothing panics.
func TestLogMetricKitPayloadWithoutALogDirectory(t *testing.T) {
	restoreTestingLogDir(t)
	func() {
		currentLogDirMu.Lock()
		defer currentLogDirMu.Unlock()
		currentLogDir = ""
		currentLogRoot = ""
	}()
	if GetLogDir() != "" {
		t.Skip("the log_dir flag names a directory in this run")
	}

	if n := LogMetricKitDiagnosticPayload(testingMetricKitCrashPayload, 0, 0); n != 0 {
		t.Fatalf("LogMetricKitDiagnosticPayload without a log directory = %d, want 0", n)
	}
}

// The file keeps its newest whole lines within its bound, however many are
// added.
func TestExitDiagnosticsFileKeepsItsNewestLines(t *testing.T) {
	settings := defaultExitDiagnosticsSettings()
	// nine lines of 211 bytes fit, ten do not
	settings.MaxByteCount = 2000
	dir := t.TempDir()
	line := func(i int) string {
		return fmt.Sprintf("line %04d %s", i, strings.Repeat("x", 200))
	}
	for i := range 40 {
		err := func() error {
			exitDiagnosticsStateLock.Lock()
			defer exitDiagnosticsStateLock.Unlock()
			return appendExitDiagnosticsLinesWithLock(dir, []string{line(i)}, settings)
		}()
		if err != nil {
			t.Fatal(err)
		}
	}
	content, err := os.ReadFile(filepath.Join(dir, exitDiagnosticsFileName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(lines) != 9 || settings.MaxByteCount < len(content) {
		t.Fatalf("the file has %d lines in %d bytes, want the newest 9 within %d", len(lines), len(content), settings.MaxByteCount)
	}
	for i, got := range lines {
		if want := line(31 + i); got != want {
			t.Fatalf("line %d of the file is %.20q, want %.20q", i, got, want)
		}
	}

	for _, c := range []struct {
		content      string
		maxByteCount int
		want         string
	}{
		{content: "aa\nbb\ncc\n", maxByteCount: 9, want: "aa\nbb\ncc\n"},
		{content: "aa\nbb\ncc\n", maxByteCount: 6, want: "bb\ncc\n"},
		{content: "aa\nbb\ncc\n", maxByteCount: 5, want: "cc\n"},
		{content: "aa\nbb\ncc\n", maxByteCount: 2, want: ""},
	} {
		if got := string(newestExitDiagnosticsLines([]byte(c.content), c.maxByteCount)); got != c.want {
			t.Errorf("newestExitDiagnosticsLines(%q, %d) = %q, want %q", c.content, c.maxByteCount, got, c.want)
		}
	}
}

// A link where the file belongs is never read through or written through:
// the file that replaces it holds only its own lines.
func TestExitDiagnosticsFileIsNeverReadThroughALink(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secretPath, []byte("secret line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, exitDiagnosticsFileName)
	if err := os.Symlink(secretPath, path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretPath, path+".tmp"); err != nil {
		t.Fatal(err)
	}

	err := func() error {
		exitDiagnosticsStateLock.Lock()
		defer exitDiagnosticsStateLock.Unlock()
		return appendExitDiagnosticsLinesWithLock(dir, []string{"a line"}, defaultExitDiagnosticsSettings())
	}()
	if err != nil {
		t.Fatal(err)
	}

	if content, err := os.ReadFile(path); err != nil || string(content) != "a line\n" {
		t.Fatalf("the file is %q (%v), want only its own line", content, err)
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("the file is not a regular file: %v", err)
	}
	if content, err := os.ReadFile(secretPath); err != nil || string(content) != "secret line\n" {
		t.Fatalf("the link's target became %q (%v)", content, err)
	}
}

// The json's time stamps, for a caller that passes no times.
func TestMetricKitTimeMillis(t *testing.T) {
	want := testingUtcMillis(2026, 11, 10, 13, 19)
	for _, text := range []string{
		`"2026-11-10 13:19:00 +0000"`,
		`"2026-11-10 14:19:00 +0100"`,
		`"2026-11-10 13:19:00 +00:00"`,
		`"2026-11-10T13:19:00Z"`,
		`"2026-11-10 13:19:00"`,
	} {
		if got := metricKitTimeMillis(json.RawMessage(text)); got != want {
			t.Errorf("metricKitTimeMillis(%s) = %s, want %s", text, exitDiagnosticsTime(got), exitDiagnosticsTime(want))
		}
	}
	for _, text := range []string{``, `null`, `5`, `"yesterday"`, `"` + strings.Repeat("2", 100) + `"`, `{}`} {
		if got := metricKitTimeMillis(json.RawMessage(text)); got != 0 {
			t.Errorf("metricKitTimeMillis(%s) = %d, want 0", text, got)
		}
	}
	if got := exitDiagnosticsTime(0); got != "unknown" {
		t.Errorf("exitDiagnosticsTime(0) = %q", got)
	}
}

// The upload's tests name the file by its literal name; it is this one.
func TestExitDiagnosticsFileName(t *testing.T) {
	if exitDiagnosticsFileName != testingExitDiagnosticsFileName {
		t.Fatalf("exitDiagnosticsFileName = %q, the upload tests name %q", exitDiagnosticsFileName, testingExitDiagnosticsFileName)
	}
}
