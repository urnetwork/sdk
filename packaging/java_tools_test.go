// SPDX-License-Identifier: MPL-2.0
// Fake tools exercise the real package-check entry point without Java or network.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Each fixture owns its artifact, tool executables, and command receipt.
type javaCheckFixture struct {
	out          string
	bin          string
	javaHome     string
	logPath      string
	maven        string
	dependency   string
	artifactPath string
}

// Model a Maven launcher with its own default JDK and a conflicting PATH JVM.
func newJavaCheckFixture(t *testing.T, system, arch string, platforms []string) javaCheckFixture {
	t.Helper()
	dir := t.TempDir()
	fixture := javaCheckFixture{
		out:        filepath.Join(dir, "package"),
		bin:        filepath.Join(dir, "path tools"),
		javaHome:   filepath.Join(dir, "maven jdk"),
		logPath:    filepath.Join(dir, "commands"),
		dependency: filepath.Join(dir, "dependency.jar"),
	}
	fixture.maven = filepath.Join(fixture.bin, "mvn")
	fixture.artifactPath = filepath.Join(fixture.out, "artifacts", "urnetwork-sdk-2026.1.1-1.jar")
	textFile(fixture.artifactPath, "synthetic checked jar")
	textFile(fixture.dependency, "synthetic dependency")
	mkdir(filepath.Join(fixture.out, "project"))
	libraries := []library{}
	for _, platform := range platforms {
		libraries = append(libraries, library{Platform: platform})
	}
	jsonWrite(filepath.Join(fixture.out, "manifest.json"), inventory{
		Version:   "2026.1.1-1",
		Native:    &nativeManifest{ABI: 1, Version: "2026.1.1-1", Libraries: libraries},
		Artifacts: artifactsIn(filepath.Join(fixture.out, "artifacts")),
	})
	toolScript(t, fixture.maven, fmt.Sprintf(`
selected="${JAVA_HOME-}"
if [ -z "$selected" ]; then selected=%s; fi
[ "$selected" = %s ] || { printf 'fixture invalid JAVA_HOME\n' >&2; exit 71; }
[ . -ef %s ] || exit 72
case "$*" in
  '--version -Dstyle.color=never')
    printf 'maven-version\n' >> %s
    printf 'Apache Maven 3.10.0\nJava version: 21, vendor: fixture, runtime: %%s\n' "$selected"
    ;;
  '-q dependency:build-classpath -Dmdep.outputFile='*)
    printf 'maven-classpath\n' >> %s
    [ "$#" = 3 ] || exit 73
    printf '%%s\n' %s > "${3#-Dmdep.outputFile=}"
    ;;
  *) exit 74 ;;
esac
`, toolQuote(fixture.javaHome), toolQuote(fixture.javaHome), toolQuote(filepath.Join(fixture.out, "project")),
		toolQuote(fixture.logPath), toolQuote(fixture.logPath), toolQuote(fixture.dependency)))
	toolScript(t, filepath.Join(fixture.javaHome, "bin", "java"), fmt.Sprintf(`
[ -z "${URNETWORK_SDK_LIBRARY-}" ] || exit 75
[ -z "${PYTHONPATH-}" ] || exit 76
case "$1" in
  -XshowSettings:properties)
    [ "$#" = 2 ] && [ "$2" = -version ] || exit 77
    printf 'java-properties\n' >> %s
    printf 'Property settings:\n    os.name = %%s\n    os.arch = %%s\n' %s %s >&2
    ;;
  -cp)
    [ "$#" = 3 ] && [ "$2" = %s ] && [ "$3" = %s ] || exit 78
    printf 'consumer\n' >> %s
    ;;
  *) exit 79 ;;
esac
`, toolQuote(fixture.logPath), toolQuote(system), toolQuote(arch),
		toolQuote(fixture.artifactPath+string(os.PathListSeparator)+fixture.dependency),
		toolQuote(path("packaging/Smoke.java")), toolQuote(fixture.logPath)))
	toolScript(t, filepath.Join(fixture.bin, "java"), fmt.Sprintf(`
printf 'wrong-path-java\n' >> %s
printf 'JAVA_CONSUMER_SELECTED_UNRELATED_PATH_JVM\n' >&2
exit 86
`, toolQuote(fixture.logPath)))
	t.Setenv("PATH", fixture.bin)
	t.Setenv("JAVA_HOME", "")
	t.Setenv("URNETWORK_SDK_LIBRARY", "/fixture/forbidden-native-override")
	t.Setenv("PYTHONPATH", "/fixture/forbidden-python-override")
	return fixture
}

// A checked receipt must follow both the actual selected JVM and smoke consumer.
func requireJavaCheckSuccess(t *testing.T, fixture javaCheckFixture) {
	t.Helper()
	if failure := panicMessage(func() { checkPackage("java", fixture.out) }); failure != "" {
		t.Fatalf("JAVA_CONSUMER_RUNTIME_SELECTION: %s; commands=%q", failure, string(read(fixture.logPath)))
	}
	if got := string(read(fixture.logPath)); got != "maven-version\njava-properties\nmaven-classpath\nconsumer\n" {
		t.Fatalf("consumer did not use the inspected Maven runtime: %q", got)
	}
	verifiedFiles(fixture.out, "2026.1.1-1", true)
}

// Reproduce an unset JAVA_HOME and a Maven wrapper's architecture-specific JDK.
func TestJavaPackageCheckUsesMavenRuntimeWithoutJavaHome(t *testing.T) {
	fixture := newJavaCheckFixture(t, "Mac OS X", "aarch64", []string{"darwin-arm64"})
	requireJavaCheckSuccess(t, fixture)
	if os.Getenv("JAVA_HOME") != "" {
		t.Fatal("package check changed the caller's unset JAVA_HOME")
	}
}

// An explicit JDK remains Maven's choice even when PATH selects another JVM.
func TestJavaPackageCheckHonorsJavaHomeOverPath(t *testing.T) {
	fixture := newJavaCheckFixture(t, "Mac OS X", "arm64", []string{"darwin-arm64"})
	t.Setenv("JAVA_HOME", fixture.javaHome)
	requireJavaCheckSuccess(t, fixture)
	if os.Getenv("JAVA_HOME") != fixture.javaHome {
		t.Fatal("package check replaced the explicit JAVA_HOME")
	}
}

// Report the incompatible runtime before dependency resolution or JNI loading.
func TestJavaPackageCheckRejectsNativePlatformMismatch(t *testing.T) {
	fixture := newJavaCheckFixture(t, "Mac OS X", "x86_64", []string{"darwin-arm64"})
	t.Setenv("JAVA_HOME", fixture.javaHome)
	failure := panicMessage(func() { checkPackage("java", fixture.out) })
	if !strings.Contains(failure, "JVM platform darwin-amd64") ||
		!strings.Contains(failure, "available: darwin-arm64") ||
		!strings.Contains(failure, "JAVA_HOME") || !strings.Contains(failure, "SDK_NATIVE_MANIFEST") {
		t.Fatalf("missing explicit JVM/native mismatch: %q", failure)
	}
	if got := string(read(fixture.logPath)); got != "maven-version\njava-properties\n" || exists(filepath.Join(fixture.out, "checked.json")) {
		t.Fatalf("incompatible runtime reached a consumer or checked receipt: %q", got)
	}
}

// Package membership, not the packaging process's CPU, determines compatibility.
func TestJavaPackageCheckAcceptsPackagedPlatformMatrix(t *testing.T) {
	for _, c := range []struct {
		system, arch string
		platforms    []string
	}{
		{system: "Mac OS X", arch: "amd64", platforms: []string{"darwin-arm64", "darwin-amd64"}},
		{system: "Linux", arch: "x86_64", platforms: []string{"linux-amd64"}},
		{system: "Windows 11", arch: "aarch64", platforms: []string{"windows-arm64"}},
	} {
		fixture := newJavaCheckFixture(t, c.system, c.arch, c.platforms)
		requireJavaCheckSuccess(t, fixture)
	}
}

// A bad explicit home is not replaced by a working but unrelated PATH runtime.
func TestJavaPackageCheckPreservesInvalidJavaHomeFailure(t *testing.T) {
	fixture := newJavaCheckFixture(t, "Mac OS X", "arm64", []string{"darwin-arm64"})
	t.Setenv("JAVA_HOME", filepath.Join(t.TempDir(), "missing jdk"))
	failure := panicMessage(func() { checkPackage("java", fixture.out) })
	if !strings.Contains(failure, "cannot inspect Maven Java runtime") {
		t.Fatalf("invalid explicit JAVA_HOME did not fail at Maven selection: %q", failure)
	}
	if exists(fixture.logPath) || exists(filepath.Join(fixture.out, "checked.json")) {
		t.Fatal("invalid explicit JDK silently fell back or acquired a checked receipt")
	}
}

// Unreported runtime paths and unknown CPUs cannot acquire guessed authority.
func TestJavaPackageCheckRejectsUnknownRuntime(t *testing.T) {
	for _, version := range []string{
		"Apache Maven 3.10.0\n",
		"Java version: 21, vendor: fixture, runtime: relative/jdk\n",
	} {
		fixture := newJavaCheckFixture(t, "Mac OS X", "arm64", []string{"darwin-arm64"})
		toolScript(t, fixture.maven, fmt.Sprintf(`
[ "$*" = '--version -Dstyle.color=never' ] || exit 90
printf '%%s' %s
`, toolQuote(version)))
		failure := panicMessage(func() { checkPackage("java", fixture.out) })
		if !strings.Contains(failure, "Maven did not report an absolute Java runtime") {
			t.Fatalf("runtime %q did not fail closed: %q", version, failure)
		}
		if exists(fixture.logPath) || exists(filepath.Join(fixture.out, "checked.json")) {
			t.Fatal("missing runtime metadata reached Java or acquired a receipt")
		}
	}
	fixture := newJavaCheckFixture(t, "Mac OS X", "unknown-cpu", []string{"darwin-arm64"})
	failure := panicMessage(func() { checkPackage("java", fixture.out) })
	if !strings.Contains(failure, "unsupported Maven-selected Java platform") {
		t.Fatalf("unknown JVM CPU was guessed: %q", failure)
	}
	if got := string(read(fixture.logPath)); got != "maven-version\njava-properties\n" || exists(filepath.Join(fixture.out, "checked.json")) {
		t.Fatalf("unknown CPU reached a consumer or checked receipt: %q", got)
	}
}

// Kotlin's ordinary smoke target delegates the same Java package/check contract.
func TestKotlinSmokeDelegatesJavaToolchain(t *testing.T) {
	fixture := t.TempDir()
	for _, source := range []string{"packaging/language.mk", "java/Makefile", "kotlin/Makefile"} {
		copyFile(path(source), filepath.Join(fixture, source))
	}
	logPath := filepath.Join(fixture, "commands")
	goTool := filepath.Join(fixture, "fixture-go")
	javaHome := filepath.Join(fixture, "explicit jdk")
	toolScript(t, goTool, fmt.Sprintf(`
[ "$JAVA_HOME" = %s ] || exit 91
printf '%%s %%s\n' "$5" "${6-}" >> %s
`, toolQuote(javaHome), toolQuote(logPath)))
	t.Setenv("JAVA_HOME", javaHome)
	cmd := exec.Command("make", "--no-print-directory", "smoke", "GO="+goTool)
	cmd.Dir = filepath.Join(fixture, "kotlin")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Kotlin delegation: %v\n%s", err, output)
	}
	if got := string(read(logPath)); got != "check-generated \npackage java\ncheck java\n" {
		t.Fatalf("Kotlin bypassed the Java toolchain contract: %q", got)
	}
}
