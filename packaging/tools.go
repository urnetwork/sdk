// SPDX-License-Identifier: MPL-2.0
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

type dotnetTool struct {
	executable string
	install    string
	version    string
}

func dotnetArch() string {
	if runtime.GOARCH == "amd64" {
		return "x64"
	}
	return runtime.GOARCH
}

func (d dotnetTool) environment(overrides map[string]string) map[string]string {
	e := map[string]string{}
	for k, v := range overrides {
		e[k] = v
	}
	// Apphosts launched by `dotnet run` must find the same runtime as the CLI,
	// including Homebrew's keg-only SDK and machines with multiple architectures.
	install := d.install
	e["DOTNET_ROOT"] = install
	e["DOTNET_ROOT_"+strings.ToUpper(dotnetArch())] = install
	e["DOTNET_CLI_UI_LANGUAGE"] = "en-US"
	e["DOTNET_CLI_TELEMETRY_OPTOUT"] = "1"
	e["DOTNET_NOLOGO"] = "1"
	return e
}

func (d dotnetTool) probe(arg string) (string, error) {
	cmd := exec.Command(d.executable, arg)
	cmd.Dir = path("csharp") // global.json selects a stable 8.0 SDK.
	cmd.Env = withEnv(d.environment(nil))
	b, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", arg, err, strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}

var dotnetSDKVersion = regexp.MustCompile(`^8\.0\.[0-9]+$`)
var dotnetRuntime = regexp.MustCompile(`(?m)^Microsoft\.NETCore\.App 8\.0\.[0-9]+ \[([^\r\n]+)\]`)
var dotnetArchitecture = regexp.MustCompile(`(?m)^\s*Architecture:\s*(\S+)\s*$`)

func inspectDotnet(executable string) (dotnetTool, error) {
	p, err := exec.LookPath(executable)
	if err != nil {
		return dotnetTool{}, err
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return dotnetTool{}, err
	}
	p, err = filepath.Abs(p)
	if err != nil {
		return dotnetTool{}, err
	}
	d := dotnetTool{executable: p, install: filepath.Dir(p)}
	version, err := d.probe("--version")
	if err != nil {
		return d, fmt.Errorf(".NET 8 SDK required: %w", err)
	}
	if !dotnetSDKVersion.MatchString(version) {
		return d, fmt.Errorf(".NET 8 SDK required; selected %q", version)
	}
	d.version = version
	runtimes, err := d.probe("--list-runtimes")
	if err != nil {
		return d, err
	}
	installedRuntime := dotnetRuntime.FindStringSubmatch(runtimes)
	if len(installedRuntime) != 2 {
		return d, fmt.Errorf("Microsoft.NETCore.App 8.0 runtime required")
	}
	// PATH may contain a launcher script, as Homebrew's bin/dotnet does.
	// The runtime inventory identifies the installation used by that launcher.
	d.install = filepath.Dir(filepath.Dir(installedRuntime[1]))
	info, err := d.probe("--info")
	if err != nil {
		return d, err
	}
	arch := dotnetArchitecture.FindStringSubmatch(info)
	if len(arch) != 2 || arch[1] != dotnetArch() {
		return d, fmt.Errorf(".NET host architecture must be %s to load the native SDK", dotnetArch())
	}
	return d, nil
}

func dotnetCandidates() []string {
	if p := os.Getenv("SDK_DOTNET"); p != "" {
		return []string{p} // An explicit selection must work; never silently replace it.
	}
	name := "dotnet"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	paths := []string{name}
	for _, key := range []string{"DOTNET_ROOT_" + strings.ToUpper(dotnetArch()), "DOTNET_ROOT"} {
		if dir := os.Getenv(key); dir != "" {
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	if brew, err := exec.LookPath("brew"); err == nil {
		if prefix, err := exec.Command(brew, "--prefix", "dotnet@8").Output(); err == nil {
			paths = append(paths, filepath.Join(strings.TrimSpace(string(prefix)), "libexec", name))
		}
	}
	for _, dir := range []string{"/opt/homebrew/opt/dotnet@8/libexec", "/usr/local/opt/dotnet@8/libexec", "/usr/local/share/dotnet", "/usr/share/dotnet"} {
		paths = append(paths, filepath.Join(dir, name))
	}
	if userHome, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(userHome, ".dotnet", name))
	}
	return paths
}

func findDotnet(candidates []string) (dotnetTool, error) {
	failures := []string{}
	seen := map[string]bool{}
	for _, p := range candidates {
		if seen[p] {
			continue
		}
		seen[p] = true
		d, err := inspectDotnet(p)
		if err == nil {
			return d, nil
		}
		// Report incompatible installations, without listing every absent default.
		if d.executable != "" || os.Getenv("SDK_DOTNET") != "" {
			failures = append(failures, fmt.Sprintf("%s: %v", p, err))
		}
	}
	detail := ""
	if len(failures) != 0 {
		detail = "\n" + strings.Join(failures, "\n")
	}
	return dotnetTool{}, fmt.Errorf("C# packaging needs a %s .NET 8 SDK and runtime; run make -C %s init (macOS/Homebrew), or install .NET 8 and set SDK_DOTNET to its executable%s", dotnetArch(), path("csharp"), detail)
}

func requireDotnet() dotnetTool {
	d, err := findDotnet(dotnetCandidates())
	must(err)
	return d
}

func checkTools(language string) {
	require(language == "csharp", "unknown toolchain: %s", language)
	d := requireDotnet()
	fmt.Printf("C# tools ready: .NET SDK %s (%s), %s\n", d.version, dotnetArch(), d.executable)
}

func initTools(language string) {
	require(language == "csharp", "unknown toolchain: %s", language)
	if _, err := findDotnet(dotnetCandidates()); err != nil {
		require(os.Getenv("SDK_DOTNET") == "", "%v; fix or unset SDK_DOTNET before automatic setup", err)
		require(runtime.GOOS == "darwin", "%v", err)
		brew, brewErr := exec.LookPath("brew")
		require(brewErr == nil, "%v; install Homebrew before running make init", err)
		command(path("csharp"), nil, brew, "install", "dotnet@8")
	}
	checkTools(language)
}
