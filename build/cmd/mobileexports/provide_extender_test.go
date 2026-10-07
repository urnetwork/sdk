// Keeps the provider extender's device controls on the gomobile surface
// (EXTENDER.md G1, F3, L2) for the android and apple apps and any third-party
// mobile host: the settings fields a host sets before Sdk.newDeviceLocal --
// the two that decide whether the role runs and the opt-in to bind its dns
// carrier on 53 -- and the host-facing constructor that takes the first two.
// Each control crosses as a plain boolean, which is the form gobind binds
// (golang/go#71827).
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMobileProvideExtenderControlsAreBound(t *testing.T) {
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
	files := []struct {
		path  string
		texts []string
	}{
		{
			path: filepath.Join(javaDirectory, "java", "com", "bringyour", "sdk", "DeviceLocalSettings.java"),
			texts: []string{
				"public final native boolean getProvideExtenderEnabled();",
				"public final native void setProvideExtenderEnabled(boolean v);",
				"public final native boolean getDefaultProvideExtender();",
				"public final native void setDefaultProvideExtender(boolean v);",
				"public final native boolean getProvideExtenderDnsPrivilegedPort();",
				"public final native void setProvideExtenderDnsPrivilegedPort(boolean v);",
			},
		},
		{
			path: filepath.Join(javaDirectory, "java", "com", "bringyour", "sdk", "Sdk.java"),
			texts: []string{
				"public static native DeviceLocalSettings defaultDeviceLocalSettings();",
				"public static native DeviceLocal newDeviceLocal(NetworkSpace networkSpace, String byJwt, String deviceDescription, String deviceSpec, String appVersion, Id instanceId, DeviceLocalSettings settings) throws Exception;",
				"public static native DeviceLocal newDeviceLocalWithProvideExtender(NetworkSpace networkSpace, String byJwt, String deviceDescription, String deviceSpec, String appVersion, Id instanceId, boolean enableRpc, DeviceLocalKeyMaterial keyMaterial, boolean provideExtenderEnabled, boolean defaultProvideExtender) throws Exception;",
			},
		},
		{
			path: filepath.Join(objcDirectory, "src", "gobind", "Sdk.objc.h"),
			texts: []string{
				"@property (nonatomic) BOOL provideExtenderEnabled;",
				"@property (nonatomic) BOOL defaultProvideExtender;",
				"@property (nonatomic) BOOL provideExtenderDnsPrivilegedPort;",
				"FOUNDATION_EXPORT SdkDeviceLocalSettings* _Nullable SdkDefaultDeviceLocalSettings(void);",
				"FOUNDATION_EXPORT SdkDeviceLocal* _Nullable SdkNewDeviceLocal(SdkNetworkSpace* _Nullable networkSpace, NSString* _Nullable byJwt, NSString* _Nullable deviceDescription, NSString* _Nullable deviceSpec, NSString* _Nullable appVersion, SdkId* _Nullable instanceId, SdkDeviceLocalSettings* _Nullable settings, NSError* _Nullable* _Nullable error);",
				"FOUNDATION_EXPORT SdkDeviceLocal* _Nullable SdkNewDeviceLocalWithProvideExtender(SdkNetworkSpace* _Nullable networkSpace, NSString* _Nullable byJwt, NSString* _Nullable deviceDescription, NSString* _Nullable deviceSpec, NSString* _Nullable appVersion, SdkId* _Nullable instanceId, BOOL enableRpc, SdkDeviceLocalKeyMaterial* _Nullable keyMaterial, BOOL provideExtenderEnabled, BOOL defaultProvideExtender, NSError* _Nullable* _Nullable error);",
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
				t.Errorf("%s does not bind %s", filepath.Base(file.path), text)
			}
		}
	}
}
