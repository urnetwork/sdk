//go:build !ios_extension && !js

// §22.5's §18 row for the iOS extension (sidecar §18.10, D-SDK-20, Q-SDK-5): the extension links the device role,
// sidecar/device over protocol/bridge, and none of the sidecar root or protocol, whose init functions and descriptors
// would land in the jetsam-bound tunnel. A program built with the extension's tags, sdk_mobile_bind and
// ios_extension, imports the sdk and reads protoregistry.GlobalFiles: of the sidecar module's descriptors only
// protocol/bridge's auth.proto, bridge.proto and signed.proto are registered, and its symbol table names nothing of
// the root or protocol package, so no root init is linked. The program runs on this host, since an ios binary cannot;
// the ios/arm64 leg's own closure is TestSdkImportsTheSidecarModuleByRole's and TestBindingsLinkNoMcpOrCli's.
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

// The descriptors the device role brings to the extension, by their registered paths, sorted.
var iosExtensionSidecarDescriptorPaths = []string{
	"protocol/bridge/auth.proto",
	"protocol/bridge/bridge.proto",
	"protocol/bridge/signed.proto",
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

// Runs go with arguments in this module, failing the test with what go said.
func testingIosExtensionGo(t *testing.T, arguments ...string) []byte {
	t.Helper()
	command := exec.Command("go", arguments...)
	command.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
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

// The extension registers protocol/bridge's three descriptors and no other of the sidecar module's, and links no
// function, type or init of the root or protocol, while it does link the device role.
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
	if !slices.Equal(registered, iosExtensionSidecarDescriptorPaths) {
		t.Fatalf("the extension registers %v of the sidecar module's descriptors, want only %v", registered, iosExtensionSidecarDescriptorPaths)
	}

	linksDevice := false
	for _, line := range strings.Split(string(testingIosExtensionGo(t, "tool", "nm", binaryPath)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		symbol := fields[len(fields)-1]
		if strings.Contains(symbol, "github.com/urnetwork/sidecar.") || strings.Contains(symbol, "github.com/urnetwork/sidecar/protocol.") {
			t.Errorf("the extension links %s", symbol)
		}
		if strings.Contains(symbol, "github.com/urnetwork/sidecar/device.") {
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
