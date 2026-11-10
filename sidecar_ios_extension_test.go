//go:build !ios_extension && !js

// §22.5's §18 row for the iOS extension (sidecar §18.10, D-SDK-20, Q-SDK-5, DESIGN-DETAILS 4.2.3): the extension links
// the forwarding role, sidecar/device over protocol/bridge, and of the sidecar root and protocol only the files on the
// sidecar module's ios_extension leg list, which the module tags every other file out of, so no other init function or
// descriptor lands in the jetsam-bound tunnel. A program built with the extension's tags, sdk_mobile_bind and
// ios_extension, imports the sdk and reads protoregistry.GlobalFiles: of the sidecar module's descriptors it registers
// protocol/bridge's, and of protocol's only those whose generated file is on the list; and every sidecar root or protocol
// file its build compiles is on the list. The program runs on this host, since an ios binary cannot; the ios/arm64 leg's
// own closure is TestSdkImportsTheSidecarModuleByRole's and TestBindingsLinkNoMcpOrCli's.
package sdk

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
)

// The descriptors the device role brings to the extension, by their registered paths, sorted: protocol/bridge's, which
// it links whole.
var iosExtensionSidecarDescriptorPaths = []string{
	"protocol/bridge/auth.proto",
	"protocol/bridge/bridge.proto",
	"protocol/bridge/signed.proto",
}

// True for a sidecar descriptor the extension may register: protocol/bridge's, or one of protocol's whose generated
// file the list names.
func iosExtensionAllowsDescriptor(list map[string]bool, descriptorPath string) bool {
	if strings.HasPrefix(descriptorPath, "protocol/bridge/") {
		return true
	}
	return strings.HasPrefix(descriptorPath, "protocol/") && list[strings.TrimSuffix(descriptorPath, ".proto")+".pb.go"]
}

// The probe: prints each registered file whose proto package is the sidecar module's.
const iosExtensionDescriptorProbeSource = `package main

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "github.com/urnetwork/sdk"
)

func main() {
	protoregistry.GlobalFiles.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		if strings.HasPrefix(string(file.Package()), "urnetwork.sidecar") {
			fmt.Println(file.Path())
		}
		return true
	})
}
`

// The environment the probe builds in: this host's, with no flags or workspace of the caller's.
func testingIosExtensionEnvironment() []string {
	return append(os.Environ(), "GOFLAGS=", "GOWORK=off")
}

// Runs go with arguments in this module, failing the test with what go said.
func testingIosExtensionGo(t *testing.T, arguments ...string) []byte {
	t.Helper()
	command := exec.Command("go", arguments...)
	command.Env = testingIosExtensionEnvironment()
	output, err := command.Output()
	if err != nil {
		stderr := ""
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			stderr = string(exitError.Stderr)
		}
		t.Fatalf("go %s: %v\n%s", strings.Join(arguments, " "), err, stderr)
	}
	return output
}

// The extension registers protocol/bridge's descriptors and, of protocol's, only the list's, and compiles no sidecar
// root or protocol file the list does not name, while it does link the device role.
func TestIosExtensionRegistersOnlyBridgeDescriptors(t *testing.T) {
	directory := t.TempDir()
	probePath := filepath.Join(directory, "main.go")
	if err := os.WriteFile(probePath, []byte(iosExtensionDescriptorProbeSource), 0o600); err != nil {
		t.Fatal(err)
	}
	moduleDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// an overlay puts the probe inside this module, on its requires and replaces, and leaves the tree as it is
	overlay, err := json.Marshal(map[string]map[string]string{
		"Replace": {filepath.Join(moduleDirectory, "sidecariosextensionprobe", "main.go"): probePath},
	})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(directory, "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(directory, "probe")
	testingIosExtensionGo(t, "build", "-overlay="+overlayPath, "-tags", "sdk_mobile_bind,ios_extension", "-o", binaryPath,
		"./sidecariosextensionprobe")

	output, err := exec.Command(binaryPath).Output()
	if err != nil {
		t.Fatalf("the probe: %v", err)
	}
	registered := strings.Fields(string(output))
	slices.Sort(registered)
	list := sidecarIosExtensionLegList(t, moduleDirectory)
	for _, descriptorPath := range registered {
		if !iosExtensionAllowsDescriptor(list, descriptorPath) {
			t.Errorf("the extension registers %s, whose generated file is not on the sidecar module's ios_extension leg list", descriptorPath)
		}
	}
	for _, descriptorPath := range iosExtensionSidecarDescriptorPaths {
		if !slices.Contains(registered, descriptorPath) {
			t.Errorf("CONTROL FAILED: the extension does not register %s, so the device role is not linked (it registers %v)", descriptorPath, registered)
		}
	}

	findings, compiled := sidecarIosExtensionFileFindings(t, "the extension probe", moduleDirectory, testingIosExtensionEnvironment(),
		[]string{"-overlay=" + overlayPath, "-tags", "sdk_mobile_bind,ios_extension", "./sidecariosextensionprobe"}, list)
	for _, finding := range findings {
		t.Errorf("%s", finding)
	}
	t.Logf("the extension probe registers %v and compiles %d sidecar root and protocol files, each on the list", registered, compiled)

	linksDevice := false
	for _, line := range strings.Split(string(testingIosExtensionGo(t, "tool", "nm", binaryPath)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 0 && strings.Contains(fields[len(fields)-1], sidecarImportModulePath+"/device.") {
			linksDevice = true
		}
	}
	if !linksDevice {
		t.Fatal("CONTROL FAILED: the extension links nothing of the device role, so the symbol scan saw nothing")
	}
}

// The ids device_local_subprotocol.go refuses to apps are the ones the sidecar subprotocol holds, which that file, in
// the tunnel, cannot name.
func TestSidecarSubprotocolIdsAreTheRoots(t *testing.T) {
	if sidecarSubprotocolFirstId != sidecar.SubprotocolId || sidecarSubprotocolLastId != sidecar.SubprotocolHeldLastId {
		t.Fatalf("the sdk refuses %d to %d, and the sidecar holds %d to %d", sidecarSubprotocolFirstId, sidecarSubprotocolLastId,
			sidecar.SubprotocolId, sidecar.SubprotocolHeldLastId)
	}
}
