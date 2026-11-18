//go:build !ios_extension && !js

// The forwarding role's head reader by leg (SDK-M8-16; QD-839, QD-973; owner decision 491): the Apple network
// extensions build the reader every other VPN component does. With the extension's tags, sdk_mobile_bind and
// ios_extension, the sdk compiles the one file that builds a role's reader, device_sidecar_forward_heads.go, and the
// sidecar root compiles the forwarding role's reader and the replaying reader under it. Read from go list on this host,
// since the sdk's tests do not compile with the extension's tags; that the role then holds an anchoring reader is
// TestForwardRoleReadsHeadsThroughF's, which runs the same file.

package sdk

import (
	"slices"
	"strings"
	"testing"
)

// The files of the sdk that build a forwarding role's head reader.
const testingForwardHeadsFilePrefix = "device_sidecar_forward_heads"

// The root's files a forwarding role's head reader is: the role's reader and the replaying reader it is built over.
var testingForwardHeadsRootFiles = []string{"bridge_forward_heads.go", "evs_config_head.go"}

// Every leg a VPN component builds on compiles the same head reader, the Apple extensions' among them.
func TestForwardHeadReaderIsOnEveryVpnComponentLeg(t *testing.T) {
	for _, tags := range []string{"", "sdk_mobile_bind", "sdk_mobile_bind,ios_extension"} {
		goFiles := func(importPath string) []string {
			return strings.Fields(string(testingIosExtensionGo(t, "list", "-tags", tags, "-f", `{{join .GoFiles "\n"}}`, importPath)))
		}
		headsFiles := slices.DeleteFunc(goFiles("."), func(name string) bool {
			return !strings.HasPrefix(name, testingForwardHeadsFilePrefix)
		})
		if !slices.Equal(headsFiles, []string{testingForwardHeadsFilePrefix + ".go"}) {
			t.Errorf("with tags %q the sdk builds a role's head reader from %v, not the one file every leg shares", tags, headsFiles)
		}
		rootFiles := goFiles(sidecarImportModulePath)
		for _, name := range testingForwardHeadsRootFiles {
			if !slices.Contains(rootFiles, name) {
				t.Errorf("with tags %q the sidecar root does not compile %s, so a role there has no head reader", tags, name)
			}
		}
	}
}
