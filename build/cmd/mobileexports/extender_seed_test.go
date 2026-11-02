// Keeps the seed bundle's import and status on the gomobile surface (connect
// EXTENDER-UPDATE2.md §2.3) for the android and apple apps and any third-party
// mobile host: the imports of pasted text, of a file's contents and of a path,
// on the space and on the account screen's controller, with their typed results
// and the error ids an app maps to its own strings. Every value crosses as a
// string, a byte array, a boolean or a number, the forms gobind binds
// (golang/go#71827).
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMobileExtenderSeedImportIsBound(t *testing.T) {
	// the bindings gomobile builds for one language, in a fresh directory
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
			"GOSUMDB=off",
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
			path: javaPath("NetworkSpace.java"),
			texts: []string{
				"public native ExtenderSeedImportResult importExtenderSeed(String text);",
				"public native ExtenderSeedImportResult importExtenderSeedData(byte[] data);",
				"public native ExtenderSeedImportResult importExtenderSeedFile(String path);",
				"public native ExtenderSeedStatus getExtenderSeedStatus();",
			},
		},
		{
			path: javaPath("ExtenderViewController.java"),
			texts: []string{
				"public native ExtenderSeedImportResult importSeed(String text);",
				"public native ExtenderSeedImportResult importSeedData(byte[] data);",
				"public native ExtenderSeedImportResult importSeedFile(String path);",
				"public native ExtenderSeedStatus getSeedStatus();",
			},
		},
		{
			path: javaPath("ExtenderSeedImportResult.java"),
			texts: []string{
				"public final native boolean getOk();",
				"public final native String getError();",
				"public final native String getOutcome();",
				"public final native String getNetworkHost();",
				"public final native long getSequence();",
				"public final native long getExpireTime();",
				"public final native long getRecordCount();",
				"public final native long getRevocationCount();",
				"public final native long getUsableCount();",
			},
		},
		{
			path: javaPath("ExtenderSeedStatus.java"),
			texts: []string{
				"public final native long getLastPassTime();",
				"public final native String getLastPassOutcome();",
				"public final native String getLastPassError();",
				"public final native long getSequence();",
				"public final native long getExpireTime();",
				"public final native String getSource();",
				"public final native long getEmbeddedCount();",
				"public final native String getEmbeddedError();",
				"public final native long getMirrorCount();",
				"public final native long getDnsIndependentMirrorCount();",
			},
		},
		{
			// constants made from connect's, which an alias would silently drop
			path: javaPath("Sdk.java"),
			texts: []string{
				`public static final String ExtenderSeedImportErrorForged = "import_extender_seed_forged";`,
				`public static final String ExtenderSeedImportErrorExpired = "import_extender_seed_expired";`,
				`public static final String ExtenderSeedImportErrorRollback = "import_extender_seed_rollback";`,
				`public static final String ExtenderSeedImportOutcomeAccepted = "accepted";`,
				`public static final String ExtenderSeedImportOutcomeIdentical = "identical";`,
			},
		},
		{
			path: filepath.Join(objcDirectory, "src", "gobind", "Sdk.objc.h"),
			texts: []string{
				"- (SdkExtenderSeedImportResult* _Nullable)importExtenderSeed:(NSString* _Nullable)text;",
				"- (SdkExtenderSeedImportResult* _Nullable)importExtenderSeedData:(NSData* _Nullable)data;",
				"- (SdkExtenderSeedImportResult* _Nullable)importExtenderSeedFile:(NSString* _Nullable)path;",
				"- (SdkExtenderSeedStatus* _Nullable)getExtenderSeedStatus;",
				"- (SdkExtenderSeedImportResult* _Nullable)importSeed:(NSString* _Nullable)text;",
				"- (SdkExtenderSeedImportResult* _Nullable)importSeedData:(NSData* _Nullable)data;",
				"- (SdkExtenderSeedImportResult* _Nullable)importSeedFile:(NSString* _Nullable)path;",
				"- (SdkExtenderSeedStatus* _Nullable)getSeedStatus;",
				"FOUNDATION_EXPORT NSString* _Nonnull const SdkExtenderSeedImportErrorForged;",
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
