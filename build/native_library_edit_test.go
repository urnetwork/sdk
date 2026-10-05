// Run the real Android build recipe with stub tools, so the native library
// edit between gomobile bind and the AAR repack is observable without an NDK.
package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// One libgojni.so per ABI that build_android binds (-target android/arm64,
// android/arm, android/amd64).
var androidNativeEditAbis = []string{"arm64-v8a", "armeabi-v7a", "x86_64"}

// Owns a disposable build directory, a fake NDK and stubs for every tool the
// recipe runs after its lock gate.
type androidNativeEditFixture struct {
	directory string
	makefile  string
	logs      string
	env       []string
}

// Installs the stubs. Each one appends its arguments to a log named after the
// tool; llvm-objcopy and checksec fail on the library of one chosen ABI.
func newAndroidNativeEditFixture(t *testing.T) *androidNativeEditFixture {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Android SDK output locking supports Darwin and Linux build hosts")
	}
	makefile, err := filepath.Abs("Makefile")
	testingBuildNoError(t, err)
	base := t.TempDir()
	fixture := &androidNativeEditFixture{
		directory: filepath.Join(base, "out"),
		makefile:  makefile,
		logs:      filepath.Join(base, "logs"),
	}
	for _, directory := range []string{fixture.directory, fixture.logs} {
		testingBuildNoError(t, os.Mkdir(directory, 0o755))
	}
	fixture.write(t, "bin/gomobile", `#!/bin/sh
# gomobile bind ... -o <aar> ...: the AAR, and the sources JAR gomobile writes beside it.
printf '%s\n' "$*" >>"$ANDROID_EDIT_FIXTURE_LOGS/gomobile"
aar=
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then aar="$2"; fi
  shift
done
[ -n "$aar" ] || exit 64
printf 'fixture aar\n' >"$aar"
printf 'fixture sources\n' >"${aar%/*}/URnetworkSdk-sources.jar"
`)
	fixture.write(t, "bin/unzip", `#!/bin/sh
# unzip <aar> -d <directory>: one library per ABI, each still carrying its .comment section.
[ "$#" -eq 3 ] && [ "$2" = -d ] || exit 64
for abi in $ANDROID_EDIT_FIXTURE_ABIS; do
  mkdir -p "$3/jni/$abi"
  printf '.comment present\n' >"$3/jni/$abi/libgojni.so"
done
`)
	fixture.write(t, "ndk/llvm-objcopy", `#!/bin/sh
# One library per call: objcopy reads a second path as its output file.
printf '%s\n' "$*" >>"$ANDROID_EDIT_FIXTURE_LOGS/objcopy"
if [ "$#" -ne 3 ] || [ "$1" != --remove-section ] || [ "$2" != .comment ] || [ ! -f "$3" ]; then
  echo "llvm-objcopy stub: unexpected arguments: $*" >&2
  exit 2
fi
case "$3" in
*/"$ANDROID_EDIT_FIXTURE_OBJCOPY_FAILS"/*)
  echo "llvm-objcopy stub: injected failure on $3" >&2
  exit 1
  ;;
esac
printf '.comment removed\n' >"$3"
`)
	fixture.write(t, "bin/checksec", `#!/bin/sh
# checksec file <library> --output json exits 1 when the library is not a readable ELF file.
printf '%s\n' "$*" >>"$ANDROID_EDIT_FIXTURE_LOGS/checksec"
if [ "$#" -ne 4 ] || [ "$1" != file ] || [ ! -f "$2" ] || [ "$3" != --output ] || [ "$4" != json ]; then
  echo "checksec stub: unexpected arguments: $*" >&2
  exit 2
fi
case "$2" in
*/"$ANDROID_EDIT_FIXTURE_CHECKSEC_FAILS"/*)
  echo "checksec stub: injected failure on $2" >&2
  exit 1
  ;;
esac
printf '[{"name":"%s"}]\n' "$2"
`)
	fixture.write(t, "bin/jar", `#!/bin/sh
# jar cvf <aar> -C <directory> . repacks the libraries; the fixture AAR lists each one's state.
printf '%s\n' "$*" >>"$ANDROID_EDIT_FIXTURE_LOGS/jar"
if [ "$1" = cvf ]; then
  for library in "$4"/jni/*/libgojni.so; do
    printf '%s: %s\n' "${library#"$4"/}" "$(cat "$library")"
  done >"$2"
fi
`)
	fixture.write(t, "bin/go", `#!/bin/sh
# make reads DefaultGODEBUG with go list; the recipe validates with go run ./cmd/mobileexports.
printf '%s\n' "$*" >>"$ANDROID_EDIT_FIXTURE_LOGS/go"
`)
	fixture.env = append(testingBuildEnvironmentWithoutAndroidLock(),
		"PATH="+filepath.Join(base, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"ANDROID_NDK_HOME="+filepath.Join(base, "ndk"),
		"WARP_VERSION=test",
		"ANDROID_EDIT_FIXTURE_LOGS="+fixture.logs,
		"ANDROID_EDIT_FIXTURE_ABIS="+strings.Join(androidNativeEditAbis, " "),
	)
	return fixture
}

// Writes an executable stub below the fixture's base directory.
func (self *androidNativeEditFixture) write(t *testing.T, relative, script string) {
	t.Helper()
	path := filepath.Join(filepath.Dir(self.directory), relative)
	testingBuildNoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	testingBuildNoError(t, os.WriteFile(path, []byte(script), 0o755))
}

// Runs make build_android. failingTool ("objcopy" or "checksec") fails on the
// library of failingAbi; an empty failingTool lets every tool succeed.
func (self *androidNativeEditFixture) build(t *testing.T, failingTool, failingAbi string) (string, error) {
	t.Helper()
	objcopyFails, checksecFails := "none", "none"
	switch failingTool {
	case "objcopy":
		objcopyFails = failingAbi
	case "checksec":
		checksecFails = failingAbi
	case "":
	default:
		t.Fatalf("unknown failing tool %q", failingTool)
	}
	command := exec.CommandContext(t.Context(), "make", "-f", self.makefile, "build_android")
	command.Dir = self.directory
	command.Env = append(slices.Clone(self.env),
		"ANDROID_EDIT_FIXTURE_OBJCOPY_FAILS="+objcopyFails,
		"ANDROID_EDIT_FIXTURE_CHECKSEC_FAILS="+checksecFails,
	)
	output, err := command.CombinedOutput()
	return string(output), err
}

// Returns the logged argument lines of one stub, none when it never ran.
func (self *androidNativeEditFixture) calls(t *testing.T, tool string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(self.logs, tool))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	testingBuildNoError(t, err)
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// Returns the published AAR's library listing and whether the build published one.
func (self *androidNativeEditFixture) published(t *testing.T) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(self.directory, "android", "URnetworkSdk.aar"))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	testingBuildNoError(t, err)
	return string(data), true
}

// A failed llvm-objcopy or checksec on any one library must stop the build
// before the AAR is repacked, validated or published. find's -exec ... \; form
// ignores the command's exit status, which let a failed objcopy publish an AAR
// whose library kept its .comment section, and a library checksec could not
// read as ELF ship unnoticed. Failing each ABI in turn puts the failure before
// the last library at least once, where a loop that kept only its last status
// would also pass.
func TestAndroidBuildStopsWhenAnyNativeLibraryEditFails(t *testing.T) {
	for _, tool := range []string{"objcopy", "checksec"} {
		for _, abi := range androidNativeEditAbis {
			t.Run(tool+"/"+abi, func(t *testing.T) {
				fixture := newAndroidNativeEditFixture(t)
				output, err := fixture.build(t, tool, abi)
				if err == nil {
					aar, _ := fixture.published(t)
					t.Fatalf("build succeeded although %s failed on the %s library; it published an AAR with:\n%s", tool, abi, aar)
				}
				if !strings.Contains(output, "stub: injected failure on ") || !strings.Contains(output, "/jni/"+abi+"/libgojni.so") {
					t.Fatalf("build failed, but not at the injected %s failure on %s:\n%s", tool, abi, output)
				}
				if aar, ok := fixture.published(t); ok {
					t.Fatalf("build published an AAR after %s failed on %s:\n%s", tool, abi, aar)
				}
				if jar := fixture.calls(t, "jar"); len(jar) != 0 {
					t.Fatalf("build repacked the AAR after %s failed on %s: %q", tool, abi, jar)
				}
				for _, call := range fixture.calls(t, "go") {
					if strings.HasPrefix(call, "run ./cmd/mobileexports ") {
						t.Fatalf("build validated the AAR after %s failed on %s: %q", tool, abi, call)
					}
				}
			})
		}
	}
}

// With every tool succeeding, each library gets exactly one llvm-objcopy call
// of its own and one checksec report, and the published AAR holds only edited
// libraries. One input per call matters: given two inputs, llvm-objcopy writes
// the first library over the second and exits 0, so batching the libraries
// into one call (find -exec ... {} +) would publish one ABI's code as another's.
func TestAndroidBuildEditsEachNativeLibraryOnce(t *testing.T) {
	fixture := newAndroidNativeEditFixture(t)
	output, err := fixture.build(t, "", "")
	if err != nil {
		t.Fatalf("build failed with every tool succeeding: %v\n%s", err, output)
	}
	objcopy := fixture.calls(t, "objcopy")
	checksec := fixture.calls(t, "checksec")
	if len(objcopy) != len(androidNativeEditAbis) || len(checksec) != len(androidNativeEditAbis) {
		t.Fatalf("want one llvm-objcopy and one checksec call per library, got:\n%q\n%q", objcopy, checksec)
	}
	aar, ok := fixture.published(t)
	if !ok {
		t.Fatalf("build did not publish an AAR:\n%s", output)
	}
	for _, abi := range androidNativeEditAbis {
		library := "/jni/" + abi + "/libgojni.so"
		if want := "--remove-section .comment "; !slices.ContainsFunc(objcopy, func(call string) bool {
			return strings.HasPrefix(call, want) && strings.HasSuffix(call, library)
		}) {
			t.Errorf("llvm-objcopy did not edit the %s library on its own: %q", abi, objcopy)
		}
		if !slices.ContainsFunc(checksec, func(call string) bool {
			return strings.HasPrefix(call, "file ") && strings.HasSuffix(call, library+" --output json")
		}) {
			t.Errorf("checksec did not report the %s library: %q", abi, checksec)
		}
		if want := "jni/" + abi + "/libgojni.so: .comment removed\n"; !strings.Contains(aar, want) {
			t.Errorf("published AAR lacks %q:\n%s", want, aar)
		}
	}
	if strings.Contains(aar, ".comment present") {
		t.Errorf("published AAR holds a library that kept its .comment section:\n%s", aar)
	}
	if !slices.ContainsFunc(fixture.calls(t, "go"), func(call string) bool {
		return strings.HasPrefix(call, "run ./cmd/mobileexports ")
	}) {
		t.Errorf("build published the AAR without validating its sources:\n%s", output)
	}
}
