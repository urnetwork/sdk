// The app log line: its bounds and sanitizing, and that it reaches the logs a
// user uploads with feedback.
package sdk

import (
	"archive/zip"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/urnetwork/connect"
)

// An app line is one bounded line that cannot split itself or forge another
// line, and is marked as the app's.
func TestAppLogLine(t *testing.T) {
	cases := []struct {
		name    string
		tag     string
		message string
		want    string
	}{
		{
			name:    "plain",
			tag:     "service",
			message: "private dns mode=strict(dns.example)",
			want:    "[app][service] private dns mode=strict(dns.example)",
		},
		{
			name:    "line breaks",
			tag:     "whitelist-probe",
			message: "a\nb\r\nc",
			want:    "[app][whitelist-probe] a b  c",
		},
		{
			name:    "forged glog line",
			tag:     "service",
			message: "ok\nI1005 00:00:00.000000 1 contract.go:1] [contract]close",
			want:    "[app][service] ok I1005 00:00:00.000000 1 contract.go:1] [contract]close",
		},
		{
			name:    "control characters and line separators",
			tag:     "service",
			message: "a\tb\x00c d e\x1bf",
			want:    "[app][service] a b c d e f",
		},
		{
			name:    "invalid utf-8",
			tag:     "service",
			message: "a\xffb",
			want:    "[app][service] a�b",
		},
		{
			name:    "tag keeps only name characters",
			tag:     "ser vice][contract\n",
			message: "x",
			want:    "[app][servicecontract] x",
		},
		{
			name:    "tag is capped",
			tag:     strings.Repeat("t", 40),
			message: "x",
			want:    "[app][" + strings.Repeat("t", 32) + "] x",
		},
		{
			name:    "no tag",
			tag:     "",
			message: "x",
			want:    "[app] x",
		},
		{
			name:    "no usable tag",
			tag:     "[]\n",
			message: "x",
			want:    "[app] x",
		},
		{
			name:    "a message at the cap is not cut",
			tag:     "t",
			message: strings.Repeat("b", 1024),
			want:    "[app][t] " + strings.Repeat("b", 1024),
		},
	}
	for _, c := range cases {
		if got := appLogLine(c.tag, c.message); got != c.want {
			t.Errorf("%s: appLogLine(%q, %q) = %q, want %q", c.name, c.tag, c.message, got, c.want)
		}
	}

	// over the cap the message is cut to 1024 bytes on a rune boundary, ending
	// in the marker
	for _, message := range []string{
		strings.Repeat("a", 2000),
		strings.Repeat("é", 1000),
	} {
		got := strings.TrimPrefix(appLogLine("t", message), "[app][t] ")
		if 1024 < len(got) || len(got) < 1020 || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
			t.Errorf("a %d byte message became %d bytes (valid utf-8 %t): ...%q", len(message), len(got), utf8.ValidString(got), got[len(got)-8:])
		}
	}
}

// An app line written just before the user sends feedback is in the zip that
// DeviceLocal.UploadLogs uploads: LogAppInfo writes glog's INFO file, and the
// upload flushes glog first (glog flushes its files only every 30 seconds).
// A multi-line message stays one line in the file.
func TestLogAppInfoLineIsInTheUploadedLogs(t *testing.T) {
	restoreTestingLogDir(t)

	dir := t.TempDir()
	if err := SetLogDir(dir); err != nil {
		t.Fatalf("SetLogDir(%q) = %v, want nil", dir, err)
	}

	LogAppInfo("service", "private dns mode=strict(dns.example)")
	LogAppInfo("whitelist-probe", "  [fail] api-reachable: SocketTimeoutException after 5001ms\nI0101 00:00:00.000000 1 forged.go:1] forged")

	zipPath, err := zipUploadLogs(uploadLogsMaxByteCount, connect.DefaultLogger())
	if err != nil {
		t.Fatalf("zipUploadLogs = %v, want nil", err)
	}
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("zip.OpenReader(%q) = %v", zipPath, err)
	}
	defer reader.Close()

	uploaded := ""
	for _, file := range reader.File {
		entry, err := file.Open()
		if err != nil {
			t.Fatalf("open %q in the upload: %v", file.Name, err)
		}
		data, err := io.ReadAll(entry)
		entry.Close()
		if err != nil {
			t.Fatalf("read %q in the upload: %v", file.Name, err)
		}
		uploaded += string(data)
	}

	for _, line := range []string{
		"] [app][service] private dns mode=strict(dns.example)\n",
		"] [app][whitelist-probe]   [fail] api-reachable: SocketTimeoutException after 5001ms I0101 00:00:00.000000 1 forged.go:1] forged\n",
	} {
		if !strings.Contains(uploaded, line) {
			t.Errorf("the uploaded logs lack the app line %q", line)
		}
	}
	if strings.Contains(uploaded, "\nI0101 00:00:00.000000 1 forged.go:1]") {
		t.Error("an app message started a line of its own in the uploaded logs")
	}
}
