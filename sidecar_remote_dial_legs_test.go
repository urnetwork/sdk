// Which remote dial each slice gomobile binds selects (§18.10's tags; owner decisions 493, 554). The macOS slices are
// desktops, and gomobile runs each of them in two configurations, failing the bind if either fails: its binding step
// type-checks with GOOS=ios and the macos tag, and the archive the slice links compiles with GOOS=darwin and the
// bind's tags alone. Both select sidecar_remote_dial.go and the root's daemon client, so the macOS app's
// NewSidecarRemote reaches ur daemon (§18.4, §18.5, R-SDK-9), and both compile. The phones' slices select
// sidecar_remote_dial_mobile.go and no daemon client, and the network extension's slices neither dial, the sidecar
// role staying out of the extension. The slices are the gomobile rows of messagingBoundaryLegs, which
// TestTheMessagingBoundaryLegsListEveryBoundTarget holds to build/Makefile's binds.

package sdk

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The root's files a desktop's dial links: the client, the hello it opens with, and the Unix socket it dials.
var remoteDialDaemonClientFiles = []string{"daemon_client.go", "daemon_hello.go", "daemon_socket_unix.go"}

// The value the leg's environment gives key, or "" when it gives none.
func remoteDialLegEnvironmentValue(leg messagingBoundaryLeg, key string) string {
	for _, entry := range leg.environment {
		if name, value, found := strings.Cut(entry, "="); found && name == key {
			return value
		}
	}
	return ""
}

// The files go list selects on the leg for the sdk package and for the sidecar root. GOFLAGS is cleared, so the leg
// is exactly what it names, and GOWORK is off, so the root is the one this module's replace names.
func remoteDialLegFiles(t *testing.T, leg messagingBoundaryLeg) (sdkFiles []string, rootFiles []string) {
	t.Helper()
	arguments := []string{"list", "-f", "{{.ImportPath}} {{join .GoFiles \" \"}}"}
	if leg.tags != "" {
		arguments = append(arguments, "-tags", leg.tags)
	}
	arguments = append(arguments, ".", "github.com/urnetwork/sidecar")
	command := exec.Command("go", arguments...)
	command.Dir = filepath.FromSlash(leg.moduleDirectory)
	command.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	command.Env = append(command.Env, leg.environment...)
	output, err := command.Output()
	if err != nil {
		stderr := ""
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			stderr = string(exitError.Stderr)
		}
		t.Fatalf("%s: go %s: %v\n%s", leg.name, strings.Join(arguments, " "), err, stderr)
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(output), "\r\n", "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "github.com/urnetwork/sdk":
			sdkFiles = fields[1:]
		case "github.com/urnetwork/sidecar":
			rootFiles = fields[1:]
		}
	}
	if sdkFiles == nil || rootFiles == nil {
		t.Fatalf("CONTROL FAILED: %s listed %q, without the sdk's files or the root's", leg.name, output)
	}
	return sdkFiles, rootFiles
}

// Every gomobile row selects the dial its platform runs: each step of each macOS slice the desktop's dial and the
// root's daemon client, each phone slice the phone's dial and no daemon client, and each network extension slice
// neither dial and no daemon client. Each macOS slice of the main bind must be read in both steps, and the iPhone's
// slice read, so a table that lost a step or a phone cannot pass.
func TestEveryGomobileSliceSelectsTheRemoteDialItsPlatformRuns(t *testing.T) {
	// "<slice> GOOS=<goos>" for each step of a macOS slice of the main bind, and each phone slice, as read
	macosStepsRead := []string{}
	phoneSlicesRead := []string{}
	for _, leg := range messagingBoundaryLegs {
		if !strings.HasPrefix(leg.name, "gomobile ") {
			continue
		}
		slice := strings.Fields(leg.name)[1]
		sdkFiles, rootFiles := remoteDialLegFiles(t, leg)
		desktopDial := slices.Contains(sdkFiles, "sidecar_remote_dial.go")
		phoneDial := slices.Contains(sdkFiles, "sidecar_remote_dial_mobile.go")
		daemonClientFiles := slices.DeleteFunc(slices.Clone(rootFiles), func(file string) bool {
			return !slices.Contains(remoteDialDaemonClientFiles, file)
		})
		slices.Sort(daemonClientFiles)
		switch {
		case strings.Contains(leg.tags, "ios_extension"):
			if desktopDial || phoneDial || len(daemonClientFiles) != 0 {
				t.Errorf("%s, a network extension's slice, selects the desktop's dial %t, the phone's %t and the root's %v",
					leg.name, desktopDial, phoneDial, daemonClientFiles)
			}
		case strings.HasPrefix(slice, "macos/"):
			if !desktopDial || phoneDial || !slices.Equal(daemonClientFiles, remoteDialDaemonClientFiles) {
				t.Errorf("%s, a desktop's slice, selects the desktop's dial %t, the phone's %t and the root's %v, want the desktop's and %v",
					leg.name, desktopDial, phoneDial, daemonClientFiles, remoteDialDaemonClientFiles)
			}
			macosStepsRead = append(macosStepsRead, slice+" GOOS="+remoteDialLegEnvironmentValue(leg, "GOOS"))
		default:
			if desktopDial || !phoneDial || len(daemonClientFiles) != 0 {
				t.Errorf("%s, a phone's slice, selects the desktop's dial %t, the phone's %t and the root's %v, want the phone's alone",
					leg.name, desktopDial, phoneDial, daemonClientFiles)
			}
			phoneSlicesRead = append(phoneSlicesRead, slice)
		}
		t.Logf("%s: the desktop's dial %t, the phone's %t, the root's daemon client %v", leg.name, desktopDial, phoneDial, daemonClientFiles)
	}
	slices.Sort(macosStepsRead)
	wantMacosSteps := []string{"macos/amd64 GOOS=darwin", "macos/amd64 GOOS=ios", "macos/arm64 GOOS=darwin", "macos/arm64 GOOS=ios"}
	if !slices.Equal(macosStepsRead, wantMacosSteps) {
		t.Errorf("CONTROL FAILED: the main bind's macOS steps read are %v, want %v", macosStepsRead, wantMacosSteps)
	}
	if !slices.Contains(phoneSlicesRead, "ios/arm64") || !slices.Contains(phoneSlicesRead, "android/arm64") {
		t.Errorf("CONTROL FAILED: the phone slices read are %v, without the iPhone's or Android's", phoneSlicesRead)
	}
}

// The sdk package, and the root with it, compiles on every macOS row, each step of the main bind's slices and of the
// extension's, as the bind needs both to: the compile step's rows compile daemon_socket_unix.go, which only a Unix
// desktop builds, and the binding step's type-check the desktop's dial and the daemon client with GOOS=ios. cgo is off,
// as in TestSdkCompilesForEveryBoundTarget, since the failures this guards are Go's and a compile with no cgo needs no
// Xcode; the caller's GOFLAGS stay, so the builds share its cache.
func TestEveryMacosRowCompilesTheSdk(t *testing.T) {
	legs := []messagingBoundaryLeg{}
	for _, leg := range messagingBoundaryLegs {
		if strings.HasPrefix(leg.name, "gomobile macos/") {
			legs = append(legs, leg)
		}
	}
	bindingStepCount := 0
	compileCount := 0
	for _, leg := range legs {
		switch {
		case strings.HasSuffix(leg.name, " binding step"):
			bindingStepCount += 1
		case strings.HasSuffix(leg.name, " compile"):
			compileCount += 1
		}
	}
	if bindingStepCount == 0 || bindingStepCount != compileCount || bindingStepCount+compileCount != len(legs) {
		t.Fatalf("CONTROL FAILED: %d macOS rows, %d binding steps and %d compiles, want each slice's two steps", len(legs), bindingStepCount, compileCount)
	}
	failures := make([]string, len(legs))
	waitGroup := sync.WaitGroup{}
	for i, leg := range legs {
		waitGroup.Go(func() {
			arguments := []string{"build", "-o", os.DevNull}
			if leg.tags != "" {
				arguments = append(arguments, "-tags", leg.tags)
			}
			arguments = append(arguments, leg.patterns...)
			command := exec.Command("go", arguments...)
			command.Dir = filepath.FromSlash(leg.moduleDirectory)
			command.Env = append(os.Environ(), "GOWORK=off")
			command.Env = append(command.Env, leg.environment...)
			command.Env = append(command.Env, "CGO_ENABLED=0")
			if output, err := command.CombinedOutput(); err != nil {
				failures[i] = leg.name + ": " + err.Error() + "\n" + string(output)
			}
		})
	}
	waitGroup.Wait()
	for _, failure := range failures {
		if failure != "" {
			t.Error(failure)
		}
	}
	t.Logf("compiled the sdk on %d macOS rows, %d binding steps and %d compiles", len(legs), bindingStepCount, compileCount)
}
