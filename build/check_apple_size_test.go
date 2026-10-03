// SPDX-License-Identifier: MPL-2.0

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const syntheticIOSBuildMetadata = "synthetic-extension: go1.test\n\tbuild\tGOOS=ios\n\tbuild\tGOFIPS140=off\n"

type appleSizeFixture struct {
	flag      string
	bytes     int64
	bundle    bool
	metadata  string
	goFails   bool
	overrides map[string]string
}

// Use sparse synthetic files and a local metadata reader, never a signed app,
// a real device, provisioning credentials, or a production executable.
func runAppleSizeFixture(t *testing.T, fixture appleSizeFixture) (string, error) {
	t.Helper()
	dir := t.TempDir()
	artifact := filepath.Join(dir, "synthetic-artifact")
	if fixture.bundle {
		artifact = filepath.Join(dir, "synthetic.appex")
		if err := os.Mkdir(artifact, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable := artifact
	if fixture.bundle {
		executable = filepath.Join(artifact, "URnetworkVPN")
	}
	file, err := os.Create(executable)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(fixture.bytes); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeGo := "#!/bin/sh\n" +
		"if [ \"$#\" -ne 3 ] || [ \"$1\" != version ] || [ \"$2\" != -m ]; then exit 2; fi\n" +
		"if [ \"$APPLE_SIZE_FIXTURE_GO_FAIL\" = true ]; then exit 1; fi\n" +
		"printf '%s\\n' \"$APPLE_SIZE_FIXTURE_METADATA\"\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fakeGo), 0o700); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("check_apple_size.sh")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sh", script, fixture.flag, artifact)
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		switch name {
		case "PATH", "URNETWORK_IOS_SDK_MAX_BYTE_COUNT", "URNETWORK_IOS_EXTENSION_SDK_MAX_BYTE_COUNT",
			"URNETWORK_IOS_EXTENSION_MAX_BYTE_COUNT", "APPLE_SIZE_FIXTURE_METADATA", "APPLE_SIZE_FIXTURE_GO_FAIL":
			continue
		}
		command.Env = append(command.Env, value)
	}
	command.Env = append(command.Env,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"APPLE_SIZE_FIXTURE_METADATA="+fixture.metadata,
		"APPLE_SIZE_FIXTURE_GO_FAIL="+strconv.FormatBool(fixture.goFails),
	)
	for name, value := range fixture.overrides {
		command.Env = append(command.Env, name+"="+value)
	}
	output, err := command.CombinedOutput()
	return string(output), err
}

func TestAppleSizeExtensionDefaultCeiling(t *testing.T) {
	t.Parallel()
	const ceiling int64 = 48 << 20
	for _, test := range []struct {
		name   string
		bytes  int64
		bundle bool
		fails  bool
	}{
		{name: "above_previous_ceiling", bytes: (39 << 20) + 1},
		{name: "exactly_48_mib", bytes: ceiling},
		{name: "exactly_48_mib_bundle", bytes: ceiling, bundle: true},
		{name: "one_byte_over", bytes: ceiling + 1, bundle: true, fails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := runAppleSizeFixture(t, appleSizeFixture{
				flag: "--extension", bytes: test.bytes, bundle: test.bundle, metadata: syntheticIOSBuildMetadata,
			})
			if !strings.Contains(output, "ceiling="+strconv.FormatInt(ceiling, 10)+" bytes (48.000 MiB)") {
				t.Fatalf("extension ceiling is not 48 MiB:\n%s", output)
			}
			if test.fails {
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) || exitError.ExitCode() != 1 ||
					!strings.Contains(output, "exceeds its compiled-size budget") {
					t.Fatalf("oversize extension did not fail at its size gate: err=%v\n%s", err, output)
				}
			} else if err != nil || !strings.Contains(output, "fips140 build default=off") {
				t.Fatalf("in-budget extension failed: err=%v\n%s", err, output)
			}
		})
	}
}

func TestAppleSizePreservesOtherCeilingsAndExplicitOverride(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		flag     string
		bytes    int64
		ceiling  string
		fails    bool
		override map[string]string
	}{
		{name: "full_sdk_unchanged", flag: "--sdk", bytes: 1, ceiling: "100663296"},
		{name: "extension_sdk_unchanged", flag: "--extension-sdk", bytes: 1, ceiling: "96468992"},
		{name: "explicit_extension_override", flag: "--extension", bytes: 8, ceiling: "8",
			override: map[string]string{"URNETWORK_IOS_EXTENSION_MAX_BYTE_COUNT": "8"}},
		{name: "explicit_override_is_enforced", flag: "--extension", bytes: 9, ceiling: "8", fails: true,
			override: map[string]string{"URNETWORK_IOS_EXTENSION_MAX_BYTE_COUNT": "8"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := runAppleSizeFixture(t, appleSizeFixture{
				flag: test.flag, bytes: test.bytes, metadata: syntheticIOSBuildMetadata, overrides: test.override,
			})
			if !strings.Contains(output, "ceiling="+test.ceiling+" bytes") || (err != nil) != test.fails {
				t.Fatalf("artifact ceiling or explicit override changed: err=%v\n%s", err, output)
			}
		})
	}
}

func TestAppleSizeRetainsFIPSMetadataGuard(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		metadata string
		goFails  bool
		reason   string
	}{
		{name: "fips_latest", metadata: "\tbuild\tGOOS=ios\n\tbuild\tGOFIPS140=latest\n", reason: "built with FIPS 140 enabled"},
		{name: "fips_default_on", metadata: "\tbuild\tGOOS=ios\n\tbuild\tDefaultGODEBUG=fips140=on\n", reason: "built with FIPS 140 enabled"},
		{name: "missing_ios_metadata", metadata: "\tbuild\tGOOS=darwin\n", reason: "no verifiable iOS Go build metadata"},
		{name: "unreadable_metadata", goFails: true, reason: "could not read Go build metadata"},
	} {
		t.Run(test.name, func(t *testing.T) {
			output, err := runAppleSizeFixture(t, appleSizeFixture{
				flag: "--extension", bytes: 1, metadata: test.metadata, goFails: test.goFails,
			})
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 1 ||
				!strings.Contains(output, test.reason) || strings.Contains(output, "fips140 build default=off") {
				t.Fatalf("metadata guard did not fail safely: err=%v\n%s", err, output)
			}
		})
	}
}
