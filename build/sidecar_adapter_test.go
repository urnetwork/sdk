package main

// The adapter harnesses stay bound (SDK-M8-15; sidecar §22.5's §18 row, D-SDK-15): the Swift and Kotlin harnesses under
// adapters/ run on an Apple test host and an Android device or AVD, which this machine does not lend, so this test holds
// every binding name each one uses to the bindings gobind generates from the sdk's source now, Objective-C for Swift and
// Java for Kotlin, and fails the moment a rename or an omission would leave a harness unbuildable.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// What each harness names, as its language's binding spells it.
var sidecarAdapterHarnessNames = map[string][]string{
	"objc": {
		"@protocol SdkSidecarKeyStore <NSObject>", "- (int32_t)putSecret:", "getSecret:", "- (int32_t)deleteSecret:",
		"@interface SdkSidecarSecret ", "SdkSidecarKeyStoreOk;", "SdkSidecarKeyStoreLocked;", "SdkSidecarKeyStoreFailed;",
		"SdkSidecarSecretPresent;", "SdkSidecarSecretAbsent;", "SdkSidecarSecretLocked;", "SdkSidecarSecretFailed;",
		"SdkNewNetworkSpaceManagerNoStorage(", "@interface SdkNetworkSpaceValues ", "updateNetworkSpaceValues:",
		"SdkNewNetworkSpaceKey(", "SdkDefaultSidecarSettings(", "initLocal:", "SdkSidecarHostClassIos",
		"- (NSString* _Nonnull)getState;", "@interface SdkSidecar ",
	},
	"java": {
		"interface SidecarKeyStore", "putSecret(", "getSecret(", "deleteSecret(", "class SidecarSecret ",
		"SidecarKeyStoreOk", "SidecarKeyStoreLocked", "SidecarKeyStoreFailed", "SidecarSecretPresent", "SidecarSecretAbsent",
		"SidecarSecretLocked", "SidecarSecretFailed", "newNetworkSpaceManagerNoStorage(", "class NetworkSpaceValues ",
		"updateNetworkSpaceValues(", "newNetworkSpaceKey(", "defaultSidecarSettings(", "newSidecarLocal(", "SidecarHostClassAndroid",
		"String getState(",
	},
}

// The harness sources, each read to make sure it still names what the table checks.
var sidecarAdapterHarnessFiles = map[string]string{
	"objc": filepath.Join("adapters", "swift", "SidecarAdapterTests.swift"),
	"java": filepath.Join("adapters", "kotlin", "SidecarAdapterTest.kt"),
}

// Every name a harness uses is in the binding gobind generates now, and each harness still uses the key store and the
// local Sidecar the table checks.
func TestSidecarAdapterHarnessesStayBound(t *testing.T) {
	outputDirectory := t.TempDir()
	command := exec.Command("go", "run", "golang.org/x/mobile/cmd/gobind", "-lang=objc,java", "-javapkg=com.bringyour",
		"-tags=sdk_mobile_bind", "-outdir="+outputDirectory, "github.com/urnetwork/sdk")
	command.Env = append(os.Environ(), "GOEXPERIMENT=greenteagc", "GOPROXY=off", "GOWORK=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("gobind: %v\n%s", err, output)
	}
	readAll := func(paths ...string) string {
		var contents strings.Builder
		for _, path := range paths {
			bytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			contents.Write(bytes)
		}
		return contents.String()
	}
	javaFiles, err := filepath.Glob(filepath.Join(outputDirectory, "java", "com", "bringyour", "sdk", "*.java"))
	if err != nil || len(javaFiles) == 0 {
		t.Fatalf("no Java binding: %v", err)
	}
	bindings := map[string]string{
		"objc": readAll(filepath.Join(outputDirectory, "src", "gobind", "Sdk.objc.h")),
		"java": readAll(javaFiles...),
	}
	for language, names := range sidecarAdapterHarnessNames {
		for _, name := range names {
			if !strings.Contains(bindings[language], name) {
				t.Errorf("the %s binding has no %q, which its harness uses", language, name)
			}
		}
	}
	for language, path := range sidecarAdapterHarnessFiles {
		// Swift spells the local constructor init(local:...), Kotlin newSidecarLocal
		harness := strings.ToLower(readAll(path))
		for _, used := range []string{"SidecarKeyStore", "getSecret", "SidecarSecretFailed", "SidecarKeyStoreFailed", "local"} {
			if !strings.Contains(harness, strings.ToLower(used)) {
				t.Errorf("the %s harness no longer uses %s", language, used)
			}
		}
	}
}
