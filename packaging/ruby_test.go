// SPDX-License-Identifier: MPL-2.0
package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestRubyFFIInstallArguments(t *testing.T) {
	for _, test := range []struct {
		system, arch, platform string
	}{
		{"darwin", "arm64", "arm64-darwin"},
		{"darwin", "amd64", "x86_64-darwin"},
		{"linux", "arm64", ""},
		{"linux", "amd64", ""},
		{"windows", "arm64", ""},
		{"windows", "amd64", ""},
	} {
		t.Run(test.system+"-"+test.arch, func(t *testing.T) {
			want := []string{"install", "ffi", "-v", "1.17.2", "--no-document"}
			if test.platform != "" {
				want = append(want, "--platform", test.platform)
			}
			if got := rubyFFIInstallArgs(test.system, test.arch); !slices.Equal(got, want) {
				t.Fatalf("FFI arguments = %q, want %q", got, want)
			}
		})
	}
	if failure := panicMessage(func() { rubyFFIInstallArgs("darwin", "unknown") }); !strings.Contains(failure, "unsupported Darwin Ruby consumer architecture") {
		t.Fatalf("unknown architecture did not fail closed: %q", failure)
	}
}

func TestRubyFFIExplicitPlatformRejectsOtherDarwinArchitecture(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("Ruby unavailable; fake-CLI and platform argument regressions remain mandatory")
	}
	for _, test := range []struct{ arch, wanted, rejected string }{
		{"arm64", "arm64-darwin", "x86_64-darwin"},
		{"amd64", "x86_64-darwin", "arm64-darwin"},
	} {
		t.Run(test.arch, func(t *testing.T) {
			// Offline reproduction of Apple's universal-platform ambiguity using
			// the real RubyGems option parser. Never run the install command.
			const script = `
wanted, rejected = ARGV.shift(2).map { |value| Gem::Platform.new(value) }
Gem::Platform.instance_variable_set(:@local, Gem::Platform.new("universal-darwin-25"))
Gem.platforms = [Gem::Platform::RUBY, Gem::Platform.local]
abort "fixture did not reproduce universal ambiguity" unless Gem::Platform.match(wanted) && Gem::Platform.match(rejected)
Gem::Commands::InstallCommand.new.handle_options(ARGV)
abort "wrong CPU remains installable" if Gem::Platform.match(rejected)
abort "correct CPU is not installable" unless Gem::Platform.match(wanted)
`
			args := []string{"-rrubygems/commands/install_command", "-e", script, "--", test.wanted, test.rejected}
			args = append(args, rubyFFIInstallArgs("darwin", test.arch)[1:]...)
			cmd := exec.Command(ruby, args...)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("RubyGems platform selection: %v\n%s", err, output)
			}
		})
	}
}

func TestRubyPackageCheckPinsHostFFIPlatform(t *testing.T) {
	out, artifact := fixturePackage(t, "urnetwork-sdk.fixture.gem")
	bin := t.TempDir()
	log := filepath.Join(bin, "commands")
	ffiCommand := "install ffi -v 1.17.2 --no-document"
	if runtime.GOOS == "darwin" {
		switch runtime.GOARCH {
		case "arm64":
			ffiCommand += " --platform arm64-darwin"
		case "amd64":
			ffiCommand += " --platform x86_64-darwin"
		default:
			t.Fatalf("unsupported Darwin test architecture %s", runtime.GOARCH)
		}
	}
	// Model a universal RubyGems resolver that can select the wrong binary
	// unless its caller restricts Darwin dependencies to the native SDK's CPU.
	toolScript(t, filepath.Join(bin, "gem"), fmt.Sprintf(`
[ "$GEM_HOME" = "$GEM_PATH" ] || exit 91
case "$GEM_HOME" in */urnetwork-package-check-*/gems) ;; *) exit 92 ;; esac
case "$*" in
  %s) printf 'ffi\n' >> %s ;;
  %s) printf 'sdk\n' >> %s ;;
  *) printf 'unexpected gem arguments: %%s\n' "$*" >&2; exit 97 ;;
esac
`, toolQuote(ffiCommand), toolQuote(log), toolQuote("install --local --ignore-dependencies --no-document "+artifact), toolQuote(log)))
	toolScript(t, filepath.Join(bin, "ruby"), fmt.Sprintf(`
[ "$1" = %s ] || exit 93
[ "$GEM_HOME" = "$GEM_PATH" ] || exit 94
[ "$SDK_GEM_VERSION" = '2026.9.14.pre.123' ] || exit 95
printf 'consumer\n' >> %s
`, toolQuote(path("packaging/smoke_ruby.rb")), toolQuote(log)))
	t.Setenv("PATH", bin)
	t.Setenv("GEM_HOME", "/not-the-consumer-gems")
	t.Setenv("GEM_PATH", "/not-the-consumer-gems")
	checkPackage("ruby", out)
	if got := string(read(log)); got != "ffi\nsdk\nconsumer\n" {
		t.Fatalf("unexpected Ruby command sequence: %q", got)
	}
	verifiedFiles(out, "2026.9.14-123", true)
}
