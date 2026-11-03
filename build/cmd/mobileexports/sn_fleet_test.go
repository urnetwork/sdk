package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The bindings gobind makes for one language, in a fresh directory.
func snFleetGenerateBindings(t *testing.T, language string) string {
	t.Helper()
	outputDirectory := t.TempDir()
	args := []string{
		"run", "golang.org/x/mobile/cmd/gobind",
		"-lang=" + language,
		"-tags=sdk_mobile_bind",
		"-outdir=" + outputDirectory,
	}
	if language == "java" {
		args = append(args, "-javapkg=com.bringyour")
	}
	args = append(args, "github.com/urnetwork/sdk")
	command := exec.Command("go", args...)
	command.Dir = "../.."
	command.Env = append(
		os.Environ(),
		"GOEXPERIMENT=greenteagc",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOWORK=off",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate %s bindings: %v\n%s", language, err, output)
	}
	return outputDirectory
}

// The Top 200 claim on the mobile surface. An app claims, follows the claim, signs
// and leaves through the callback forms, so those are bound in both languages, and
// the only claim forms gobind skips are the Go-only ones the policy names. A form
// the apps need can't drop out under the omission policy.
func TestMobileSnFleetClaimIsBound(t *testing.T) {
	javaDirectory := snFleetGenerateBindings(t, "java")
	objcDirectory := snFleetGenerateBindings(t, "objc")
	javaPath := func(name string) string {
		return filepath.Join(javaDirectory, "java", "com", "bringyour", "sdk", name)
	}
	files := []struct {
		path  string
		texts []string
	}{
		{javaPath("SnFleetClaim.java"), []string{
			"public SnFleetClaim(Api api, SnChainSettings settings, SnFleetClaimStore store) {",
			"public native void tap(SnFleetClaimCallback callback);",
			"public native void advance(SnFleetClaimCallback callback);",
			"public native void refresh(SnFleetClaimCallback callback);",
			"public native void submitWalletAuthorization(String signature, SnFleetClaimCallback callback);",
			"public native void leave(SnFleetDevice device, SnFleetDeviceLeaveCallback callback);",
		}},
		{javaPath("SnFleetDevice.java"), []string{
			"public SnFleetDevice(Api api, DeviceLocal device, SnFleetDeviceStore store) {",
			"public SnFleetDevice(Api api, DeviceRemote device, SnFleetDeviceStore store) {",
			"public native void check(SnFleetDeviceCheckCallback callback);",
			"public native void start(SnFleetDeviceListener listener);",
			"public native void close();",
			"public native void leave(SnFleetDeviceLeaveCallback callback);",
			"public native void reset() throws Exception;",
		}},
		{javaPath("SnFleetClaimState.java"), []string{
			"public final native String getStage();",
			"public final native String getFundsDisclaimer();",
		}},
		{javaPath("SnFleetDeviceLeaveOutcome.java"), []string{
			"public final native String getFundsDisclaimer();",
		}},
		{javaPath("Sdk.java"), []string{
			"public native String loadSnFleetHotkeySeed(String networkId);",
			"public native boolean saveSnFleetHotkeySeed(String networkId, String seedHex);",
			"public native String loadSnFleetColdkeyPhrase(String networkId);",
			"public native String loadSnFleetDeviceState();",
			"public native boolean saveSnFleetDeviceState(String stateJson);",
			"public native void snFleetDeviceChecked(SnFleetDeviceCheckResult result);",
		}},
		{filepath.Join(objcDirectory, "src", "gobind", "Sdk.objc.h"), []string{
			"- (nullable instancetype)init:(SdkApi* _Nullable)api settings:(SdkSnChainSettings* _Nullable)settings store:(id<SdkSnFleetClaimStore> _Nullable)store;",
			"- (void)tap:(id<SdkSnFleetClaimCallback> _Nullable)callback;",
			"- (void)advance:(id<SdkSnFleetClaimCallback> _Nullable)callback;",
			"- (void)refresh:(id<SdkSnFleetClaimCallback> _Nullable)callback;",
			"- (void)submitWalletAuthorization:(NSString* _Nullable)signature callback:(id<SdkSnFleetClaimCallback> _Nullable)callback;",
			"- (void)leave:(SdkSnFleetDevice* _Nullable)device callback:(id<SdkSnFleetDeviceLeaveCallback> _Nullable)callback;",
			"- (nullable instancetype)initForLocal:(SdkApi* _Nullable)api device:(SdkDeviceLocal* _Nullable)device store:(id<SdkSnFleetDeviceStore> _Nullable)store;",
			"- (nullable instancetype)initForRemote:(SdkApi* _Nullable)api device:(SdkDeviceRemote* _Nullable)device store:(id<SdkSnFleetDeviceStore> _Nullable)store;",
			"- (void)check:(id<SdkSnFleetDeviceCheckCallback> _Nullable)callback;",
			"- (void)start:(id<SdkSnFleetDeviceListener> _Nullable)listener;",
			"- (void)leave:(id<SdkSnFleetDeviceLeaveCallback> _Nullable)callback;",
			"- (BOOL)reset:(NSError* _Nullable* _Nullable)error;",
		}},
	}
	for _, file := range files {
		contents, err := os.ReadFile(file.path)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range file.texts {
			if !strings.Contains(string(contents), text) {
				t.Errorf("%s does not bind %q", filepath.Base(file.path), text)
			}
		}
	}

	// what gobind skips of the claim is exactly the Go-only forms
	skipped := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(javaDirectory, "java"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(path) != ".java" {
			return walkErr
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			// the claim's own types and functions; the device RPC's signing
			// internals fall under the RPC policy
			if identifier, ok := skippedIdentifier(scanner.Text()); ok && (strings.HasPrefix(identifier, "SnFleet") || strings.HasPrefix(identifier, "NewSnFleet")) {
				skipped[identifier] = true
			}
		}
		return scanner.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	mismatched := []string{}
	for identifier := range skipped {
		if !mobileSnFleetGoOnlyIds[identifier] {
			mismatched = append(mismatched, "skipped but not Go-only: "+identifier)
		}
	}
	for identifier := range mobileSnFleetGoOnlyIds {
		if !skipped[identifier] {
			mismatched = append(mismatched, "Go-only but bound or gone: "+identifier)
		}
	}
	sort.Strings(mismatched)
	for _, line := range mismatched {
		t.Error(line)
	}
}
