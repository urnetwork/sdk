// SPDX-License-Identifier: MPL-2.0
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const mobileTransientFailure = `gomobile: go mod tidy failed: exit status 1
go: downloading example.com/dependency v1.0.0
go: example.com/keygen@v1.0.0 requires
	golang.org/x/xerrors@v0.0.0-20200804184101-5ec99f83aff1: verifying go.mod: golang.org/x/xerrors@v0.0.0-20200804184101-5ec99f83aff1/go.mod: Get "https://sum.golang.org/tile/8/0/x005/826": read tcp [2001:db8::1]:54964->[2001:db8::2]:443: read: no route to host
`

func TestGomobileRetryClassifiesOnlyTransientTidyTransport(t *testing.T) {
	script, err := filepath.Abs("gomobile-bind-retry.sh")
	testingBuildNoError(t, err)
	for _, test := range []struct {
		name, log string
		want      bool
	}{
		{"IPv6 route failure", mobileTransientFailure, true},
		{"timeout", strings.ReplaceAll(mobileTransientFailure, "no route to host", "i/o timeout"), true},
		{"reset", strings.ReplaceAll(mobileTransientFailure, "no route to host", "connection reset by peer"), true},
		{"TLS timeout", strings.ReplaceAll(mobileTransientFailure, "no route to host", "TLS handshake timeout"), true},
		{"proxy unavailable", "gomobile: go mod tidy failed: exit status 1\ngo: example.com/m@v1.0.0: reading https://proxy.golang.org/example.com/m/@v/v1.0.0.mod: 503 Service Unavailable\n", true},
		{"checksum mismatch", mobileTransientFailure + "verifying example.com/m@v1: checksum mismatch\n", false},
		{"security error", mobileTransientFailure + "SECURITY ERROR\n", false},
		{"compiler failure", mobileTransientFailure + "../sdk.go:12: undefined: Missing\n", false},
		{"invalid version", mobileTransientFailure + "go: example.com/bad: invalid version: unknown revision\n", false},
		{"ordinary tidy", "gomobile: go mod tidy failed: exit status 1\ngo: module example.com/m does not contain package example.com/m/missing\n", false},
		{"no tidy provenance", strings.TrimPrefix(mobileTransientFailure, "gomobile: go mod tidy failed: exit status 1\n"), false},
		{"unknown diagnostic", mobileTransientFailure + "unexpected diagnostic\n", false},
		{"certificate failure", strings.ReplaceAll(mobileTransientFailure, "no route to host", "x509: certificate signed by unknown authority"), false},
		{"authorization failure", strings.ReplaceAll(mobileTransientFailure, "no route to host", "403 Forbidden"), false},
		{"module not found", strings.ReplaceAll(mobileTransientFailure, "no route to host", "404 Not Found"), false},
		{"empty", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "error.log")
			testingBuildNoError(t, os.WriteFile(log, []byte(test.log), 0o600))
			cmd := exec.Command("bash", "-c", `source "$1"; gomobile_tidy_transport_failure "$2"`, "classifier", script, log)
			if output, err := cmd.CombinedOutput(); (err == nil) != test.want {
				t.Fatalf("transient=%v, want %v: %v\n%s", err == nil, test.want, err, output)
			}
		})
	}
}

func TestGomobileRetryBoundsAndPreservesVerification(t *testing.T) {
	script, err := filepath.Abs("gomobile-bind-retry.sh")
	testingBuildNoError(t, err)
	for _, test := range []struct {
		name, failure string
		failures      int
		status        int
		wantAttempts  int
		wantStatus    int
	}{
		{"success", "", 0, 0, 1, 0},
		{"recover transport", mobileTransientFailure, 1, 19, 2, 0},
		{"retry cap", mobileTransientFailure, 10, 19, 3, 19},
		{"checksum mismatch", mobileTransientFailure + "SECURITY ERROR: checksum mismatch\n", 10, 19, 1, 19},
		{"compiler error", "gomobile: go build failed: undefined: Missing\n", 10, 23, 1, 23},
		{"ordinary tidy", "gomobile: go mod tidy failed: exit status 1\ngo: unknown revision\n", 10, 1, 1, 1},
		{"interrupted", mobileTransientFailure, 10, 130, 1, 130},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			scratch := filepath.Join(dir, "scratch")
			testingBuildNoError(t, os.Mkdir(bin, 0o700))
			testingBuildNoError(t, os.Mkdir(scratch, 0o700))
			failure := filepath.Join(dir, "failure.log")
			testingBuildNoError(t, os.WriteFile(failure, []byte(test.failure), 0o600))
			fake := `#!/bin/sh
n=0
[ ! -f "$MOBILE_TEST_DIR/count" ] || n=$(cat "$MOBILE_TEST_DIR/count")
n=$((n + 1))
printf '%s\n' "$n" > "$MOBILE_TEST_DIR/count"
printf '%s\n' "$@" > "$MOBILE_TEST_DIR/arguments"
[ "$GOSUMDB" = sum.golang.org ] && [ -z "$GONOSUMDB" ] && [ -z "$GOINSECURE" ] && [ "$GOPROXY" = https://proxy.golang.org ] || exit 91
if [ "$n" -le "$MOBILE_TEST_FAILURES" ]; then
  cat "$MOBILE_TEST_DIR/failure.log"
  exit "$MOBILE_TEST_STATUS"
fi
printf 'binding completed\n'
`
			testingBuildNoError(t, os.WriteFile(filepath.Join(bin, "gomobile"), []byte(fake), 0o700))
			testingBuildNoError(t, os.WriteFile(filepath.Join(bin, "sleep"), []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$MOBILE_TEST_DIR/backoffs\"\n"), 0o700))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", script, "gomobile", "bind", "-target", "android/arm64", "-ldflags", "-X example.com/sdk.Version=version with spaces")
			cmd.Env = []string{
				"PATH=" + bin + ":/usr/bin:/bin", "TMPDIR=" + scratch,
				"MOBILE_TEST_DIR=" + dir, "MOBILE_TEST_FAILURES=" + strconv.Itoa(test.failures),
				"MOBILE_TEST_STATUS=" + strconv.Itoa(test.status),
				"GOSUMDB=sum.golang.org", "GONOSUMDB=", "GOINSECURE=", "GOPROXY=https://proxy.golang.org",
			}
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != test.wantStatus {
				t.Fatalf("exit = %v, want %d: %s", err, test.wantStatus, output)
			}
			count, err := os.ReadFile(filepath.Join(dir, "count"))
			testingBuildNoError(t, err)
			if string(count) != fmt.Sprintf("%d\n", test.wantAttempts) {
				t.Fatalf("attempt count = %q, want %d", count, test.wantAttempts)
			}
			args, err := os.ReadFile(filepath.Join(dir, "arguments"))
			testingBuildNoError(t, err)
			if string(args) != "bind\n-target\nandroid/arm64\n-ldflags\n-X example.com/sdk.Version=version with spaces\n" {
				t.Fatalf("bind arguments changed: %q", args)
			}
			backoffs, _ := os.ReadFile(filepath.Join(dir, "backoffs"))
			wantBackoffs := ""
			for i := 1; i < test.wantAttempts; i++ {
				wantBackoffs += fmt.Sprintf("%d\n", i)
			}
			if string(backoffs) != wantBackoffs {
				t.Fatalf("backoffs = %q, want %q", backoffs, wantBackoffs)
			}
			if test.failures > 0 && !strings.Contains(string(output), test.failure) {
				t.Fatal("original failure diagnostics were suppressed")
			}
			entries, err := os.ReadDir(scratch)
			testingBuildNoError(t, err)
			if len(entries) != 0 {
				t.Fatalf("attempt log leaked: %v", entries)
			}
		})
	}
}

func TestMobileBindTargetsUseBoundedRetry(t *testing.T) {
	makefile, err := os.ReadFile("Makefile")
	testingBuildNoError(t, err)
	if strings.Count(string(makefile), `"$(GOMOBILE_BIND_RETRY)" gomobile bind`) != 3 {
		t.Fatal("Android, Apple, and Apple extension binds must use the bounded wrapper")
	}
}
