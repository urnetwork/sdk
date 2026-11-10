// Keeps §18.7's credential rules (D-SDK-11) on the gomobile surface for the
// desktop releases that opt in: the app-implemented LocalStateSealer with its
// status-returning methods and its LocalStateSealKey answer, the manager
// constructor that takes it, the space value that keeps the network
// credential for the session only, and the admin_refused mark of a device
// client's mint. Every value crosses as a string, a number or a boolean, the
// forms gobind binds (golang/go#71827), so nothing here is an omission.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMobileLocalStateSealerIsBound(t *testing.T) {
	// the bindings gomobile builds for one language, in a fresh directory,
	// from the module cache alone
	generate := func(language string) string {
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
			"GOWORK=off",
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("generate %s bindings: %v\n%s", language, err, output)
		}
		return outputDirectory
	}

	javaDirectory := generate("java")
	objcDirectory := generate("objc")
	javaPath := func(name string) string {
		return filepath.Join(javaDirectory, "java", "com", "bringyour", "sdk", name)
	}
	files := []struct {
		path  string
		texts []string
	}{
		{
			path: javaPath("LocalStateSealer.java"),
			texts: []string{
				"public int putSealKey(String name, String keyBase64);",
				"public LocalStateSealKey getSealKey(String name);",
				"public int deleteSealKey(String name);",
			},
		},
		{
			path: javaPath("LocalStateSealKey.java"),
			texts: []string{
				"public final native int getStatus();",
				"public final native void setStatus(int v);",
				"public final native String getKeyBase64();",
				"public final native void setKeyBase64(String v);",
			},
		},
		{
			path: javaPath("Sdk.java"),
			texts: []string{
				"public static native NetworkSpaceManager newNetworkSpaceManagerWithLocalStateSealer(String storagePath, boolean apiAltEnabled, LocalStateSealer sealer);",
				"public static final int LocalStateSealerOk = 1;",
				"public static final int LocalStateSealerLocked = 4;",
				"public static final int LocalStateSealKeyPresent = 2;",
				"public static final int LocalStateSealKeyAbsent = 3;",
			},
		},
		{
			path: javaPath("NetworkSpaceValues.java"),
			texts: []string{
				"public final native boolean getSessionOnlyNetworkCredential();",
				"public final native void setSessionOnlyNetworkCredential(boolean v);",
			},
		},
		{
			path: javaPath("AuthNetworkClientArgs.java"),
			texts: []string{
				"public final native boolean getAdminRefused();",
				"public final native void setAdminRefused(boolean v);",
			},
		},
		{
			path: javaPath("AuthNetworkClientResult.java"),
			texts: []string{
				"public final native boolean getAdminRefused();",
			},
		},
		{
			path: filepath.Join(objcDirectory, "src", "gobind", "Sdk.objc.h"),
			texts: []string{
				"@protocol SdkLocalStateSealer <NSObject>",
				"- (int32_t)putSealKey:(NSString* _Nullable)name keyBase64:(NSString* _Nullable)keyBase64;",
				"- (SdkLocalStateSealKey* _Nullable)getSealKey:(NSString* _Nullable)name;",
				"- (int32_t)deleteSealKey:(NSString* _Nullable)name;",
				"FOUNDATION_EXPORT SdkNetworkSpaceManager* _Nullable SdkNewNetworkSpaceManagerWithLocalStateSealer(NSString* _Nullable storagePath, BOOL apiAltEnabled, id<SdkLocalStateSealer> _Nullable sealer);",
				"FOUNDATION_EXPORT const int32_t SdkLocalStateSealerLocked;",
			},
		},
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

	// the generated source passes the check the android build runs on it
	if err := validateMobileExports(javaDirectory); err != nil {
		t.Errorf("the generated bindings have unexpected omissions:\n%v", err)
	}
}
