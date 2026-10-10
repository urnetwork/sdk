// Keeps each exported field of the gomobile surface to the accessors gobind
// generates for it. Gobind binds a supported exported struct field as a getter
// and a setter named after the field (Java getX and setX, the Objective-C
// property x with its setX: setter) and binds each exported method under its
// own name. An explicit GetX or SetX method beside field X therefore declares
// the same Java method and JNI symbol twice, and the Android bind fails to
// compile ("redefinition of Java_com_bringyour_sdk_T_getX"). The C ABI exports
// a handle type's methods, never its fields, so the sdk keeps the getters it
// needs in files the sdk_mobile_bind view excludes.
package main

import (
	"fmt"
	"go/types"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// Checks every package view gomobile binds. A field whose type gobind does not
// bind gets no generated accessor, so an explicit accessor is then the field's
// only mobile form (SocketTLSOptions.SetNextProtos, VerifyKeysResult.GetKeys).
func TestMobileApiDeclaresNoAccessorGobindGenerates(t *testing.T) {
	views := []struct {
		goos string
		tags string
	}{
		{goos: "android", tags: "sdk_mobile_bind"},
		// build_apple's macos targets select the same files as ios
		{goos: "ios", tags: "sdk_mobile_bind"},
		{goos: "ios", tags: "sdk_mobile_bind,ios_extension"},
	}
	violationLines := map[string]bool{}
	for _, view := range views {
		sdkPackage := testingLoadMobileView(t, view.goos, view.tags)
		scope := sdkPackage.Types.Scope()
		for _, name := range scope.Names() {
			typeName, ok := scope.Lookup(name).(*types.TypeName)
			if !ok || !typeName.Exported() {
				continue
			}
			named, ok := typeName.Type().(*types.Named)
			if !ok {
				continue
			}
			structType, ok := named.Underlying().(*types.Struct)
			if !ok {
				continue
			}
			// gobind binds the method set of *T, including promoted methods
			methodSet := types.NewMethodSet(types.NewPointer(named))
			for index := 0; index < structType.NumFields(); index++ {
				field := structType.Field(index)
				if !field.Exported() || !testingGobindBindsType(sdkPackage.Types, field.Type()) {
					continue
				}
				for _, prefix := range []string{"Get", "Set"} {
					selection := methodSet.Lookup(nil, prefix+field.Name())
					if selection == nil {
						continue
					}
					position := sdkPackage.Fset.Position(selection.Obj().Pos())
					violationLines[fmt.Sprintf(
						"%s:%d: %s.%s repeats the accessor gobind generates for field %s",
						filepath.Base(position.Filename), position.Line, typeName.Name(), selection.Obj().Name(), field.Name(),
					)] = true
				}
			}
		}
	}
	if 0 < len(violationLines) {
		lines := slices.Sorted(maps.Keys(violationLines))
		t.Errorf("%d mobile api methods repeat a field accessor gobind generates; keep them out of the sdk_mobile_bind view:\n%s", len(lines), strings.Join(lines, "\n"))
	}
}

// The session surface the apps read (the Android SessionsViewModel and the
// Apple SessionsStore), each field through gobind's own accessors.
var mobileSessionFieldNames = map[string][]string{
	"ClientSessionError":      {"Message", "Retryable", "SignInRequired", "SessionRevoked", "Unsupported"},
	"ClientSessionAction":     {"Status", "State", "SessionId", "OperationId", "Loading", "Pending", "Error"},
	"ClientSessionSnapshot":   {"Sessions", "CurrentSessionId", "LegacyCoverage", "Generation", "EventId", "Loaded", "Loading", "Refreshing", "Supported", "BulkAction", "Actions", "Error"},
	"SessionLastUsed":         {"UnixTime", "City", "Region", "Country", "CountryCode", "DeviceType", "AppVersion"},
	"NetworkSessionInfo":      {"SessionId", "Current", "Kind", "CreateTime", "LastMintTime", "TokenExpireTime", "AcceptUntil", "OriginSessionId", "LastUsed"},
	"NetworkSessionsResult":   {"Sessions", "Generation", "EventId", "CurrentSessionId", "LegacyCoverage"},
	"NetworkSessionsRevision": {"Generation", "EventId"},
	"SessionOperationResult":  {"OperationId", "Status", "State", "SessionId", "KeptSessionId", "RevokedCount", "CleanupPending", "Generation", "EventId"},
	"SessionSignOutResult":    {"OperationId", "RevocationConfirmed", "CredentialCleared"},
}

// Runs the pinned gobind and checks its output where the bind failed: the
// Android C must define each JNI function once, and each session field must
// have one Java getter and one Objective-C property, under the field's name.
func TestMobileSessionBindingsGenerateEachFieldAccessorOnce(t *testing.T) {
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
	readText := func(path string) string {
		contents, err := os.ReadFile(path)
		testingBuildNoError(t, err)
		return string(contents)
	}
	countLines := func(text string, lineRe *regexp.Regexp) int {
		count := 0
		for _, line := range strings.Split(text, "\n") {
			if lineRe.MatchString(strings.TrimSpace(line)) {
				count += 1
			}
		}
		return count
	}
	javaDirectory := generate("java")
	objcDirectory := generate("objc")

	// clang rejects a second definition of a JNI function
	jniDefinitionRe := regexp.MustCompile(`^(Java_\w+)\(`)
	jniDefinitionCounts := map[string]int{}
	cPaths, err := filepath.Glob(filepath.Join(javaDirectory, "src", "gobind", "*.c"))
	testingBuildNoError(t, err)
	if len(cPaths) == 0 {
		t.Fatal("gobind generated no JNI C source")
	}
	for _, cPath := range cPaths {
		for _, line := range strings.Split(readText(cPath), "\n") {
			if match := jniDefinitionRe.FindStringSubmatch(line); match != nil {
				jniDefinitionCounts[match[1]] += 1
			}
		}
	}
	duplicateJniNames := []string{}
	for jniName, count := range jniDefinitionCounts {
		if 1 < count {
			duplicateJniNames = append(duplicateJniNames, fmt.Sprintf("%s (%d definitions)", jniName, count))
		}
	}
	if 0 < len(duplicateJniNames) {
		slices.Sort(duplicateJniNames)
		t.Errorf("%d JNI functions are defined more than once, so the Android bind cannot compile:\n%s", len(duplicateJniNames), strings.Join(duplicateJniNames, "\n"))
	}

	objcHeader := readText(filepath.Join(objcDirectory, "src", "gobind", "Sdk.objc.h"))
	objcInterface := func(className string) string {
		start := strings.Index(objcHeader, "@interface "+className+" :")
		if start < 0 {
			t.Fatalf("Sdk.objc.h does not declare %s", className)
		}
		end := strings.Index(objcHeader[start:], "\n@end")
		if end < 0 {
			t.Fatalf("Sdk.objc.h does not end %s", className)
		}
		return objcHeader[start : start+end]
	}
	for _, typeName := range slices.Sorted(maps.Keys(mobileSessionFieldNames)) {
		javaSource := readText(filepath.Join(javaDirectory, "java", "com", "bringyour", "sdk", typeName+".java"))
		objcSource := objcInterface("Sdk" + typeName)
		for _, fieldName := range mobileSessionFieldNames[typeName] {
			getterName := "get" + fieldName
			propertyName := strings.ToLower(fieldName[:1]) + fieldName[1:]
			// the field accessor is final; a second declaration is an explicit method
			javaGetterRe := regexp.MustCompile(`^public (final )?native [\w.]+ ` + getterName + `\(\);$`)
			javaFieldGetterRe := regexp.MustCompile(`^public final native [\w.]+ ` + getterName + `\(\);$`)
			if count := countLines(javaSource, javaGetterRe); count != 1 || countLines(javaSource, javaFieldGetterRe) != 1 {
				t.Errorf("%s.java declares %s() %d times, want once as the field %s accessor", typeName, getterName, count, fieldName)
			}
			objcPropertyRe := regexp.MustCompile(`^@property \(nonatomic\) .+[ *]` + propertyName + `;$`)
			if count := countLines(objcSource, objcPropertyRe); count != 1 {
				t.Errorf("Sdk%s declares the %s property %d times, want once", typeName, propertyName, count)
			}
			objcMethodRe := regexp.MustCompile(`^- \([^)]*\)` + getterName + `;$`)
			if count := countLines(objcSource, objcMethodRe); count != 0 {
				t.Errorf("Sdk%s declares a %s method beside the %s property", typeName, getterName, propertyName)
			}
		}
	}

	// apps open the controller from the api, or from a device, and read why
	// a logout happened from the api or the device that reported it
	javaTexts := map[string][]string{
		"Api.java": {
			"public native ClientSessionViewController openClientSessionViewController();",
			"public native String getAuthLogoutCause();",
		},
		"ViewControllerManager.java": {
			"public ClientSessionViewController openClientSessionViewController();",
		},
		"ClientSessionViewController.java": {
			"public native ClientSessionSnapshot getSnapshot();",
			"public native Sub addClientSessionListener(ClientSessionListener listener);",
		},
		"Device.java": {
			"public String getAuthLogoutCause();",
		},
		"DeviceLocal.java": {
			"public native String getAuthLogoutCause();",
		},
		"DeviceRemote.java": {
			"public native String getAuthLogoutCause();",
		},
		"Sdk.java": {
			`public static final String AuthLogoutCauseSessionRevoked = "session_revoked";`,
		},
	}
	for _, fileName := range slices.Sorted(maps.Keys(javaTexts)) {
		javaSource := readText(filepath.Join(javaDirectory, "java", "com", "bringyour", "sdk", fileName))
		for _, text := range javaTexts[fileName] {
			if !strings.Contains(javaSource, text) {
				t.Errorf("%s does not bind %s", fileName, text)
			}
		}
	}
	for className, texts := range map[string][]string{
		"SdkApi": {
			"- (SdkClientSessionViewController* _Nullable)openClientSessionViewController;",
			"- (NSString* _Nonnull)getAuthLogoutCause;",
		},
		"SdkClientSessionViewController": {"- (SdkClientSessionSnapshot* _Nullable)getSnapshot;"},
		"SdkDeviceLocal":                 {"- (NSString* _Nonnull)getAuthLogoutCause;"},
		"SdkDeviceRemote":                {"- (NSString* _Nonnull)getAuthLogoutCause;"},
	} {
		for _, text := range texts {
			if !strings.Contains(objcInterface(className), text) {
				t.Errorf("%s does not bind %s", className, text)
			}
		}
	}
	if text := "FOUNDATION_EXPORT NSString* _Nonnull const SdkAuthLogoutCauseSessionRevoked;"; !strings.Contains(objcHeader, text) {
		t.Errorf("Sdk.objc.h does not bind %s", text)
	}
}

// Mirrors the pinned gobind's isSupported (golang.org/x/mobile/bind/gen.go)
// for the one package gomobile binds. The Java and Objective-C wrapper types
// it also accepts do not occur in the sdk.
func testingGobindBindsType(boundPackage *types.Package, candidateType types.Type) bool {
	if types.Identical(candidateType, types.Universe.Lookup("error").Type()) {
		return true
	}
	switch candidateType := types.Unalias(candidateType).(type) {
	case *types.Basic:
		switch candidateType.Kind() {
		case types.Bool, types.Int, types.Int8, types.Uint8, types.Int16, types.Int32, types.Int64,
			types.Float32, types.Float64, types.String:
			return true
		}
	case *types.Slice:
		elementType, ok := types.Unalias(candidateType.Elem()).(*types.Basic)
		return ok && elementType.Kind() == types.Uint8
	case *types.Pointer:
		if elementType, ok := types.Unalias(candidateType.Elem()).(*types.Named); ok {
			return elementType.Obj().Pkg() == boundPackage
		}
	case *types.Named:
		switch candidateType.Underlying().(type) {
		case *types.Interface, *types.Pointer:
			return candidateType.Obj().Pkg() == boundPackage
		}
	}
	return false
}

// Type-checks one gomobile view of the sdk from source. Loading the
// dependencies from source too keeps the check independent of the export data
// format of the toolchain that runs it.
func testingLoadMobileView(t *testing.T, goos string, tags string) *packages.Package {
	t.Helper()
	config := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps,
		Dir:  "../..",
		// the sdk package has no cgo, so its declarations do not depend on cgo
		Env: append(
			os.Environ(),
			"GOOS="+goos,
			"GOARCH=arm64",
			"CGO_ENABLED=0",
			"GOEXPERIMENT=greenteagc",
			"GOPROXY=off",
			"GOSUMDB=off",
			"GOWORK=off",
		),
		BuildFlags: []string{"-tags=" + tags},
	}
	loadedPackages, err := packages.Load(config, "github.com/urnetwork/sdk")
	testingBuildNoError(t, err)
	if len(loadedPackages) != 1 {
		t.Fatalf("the %s %s view loaded %d packages, want the sdk", goos, tags, len(loadedPackages))
	}
	if 0 < len(loadedPackages[0].Errors) {
		t.Fatalf("load the %s %s view: %v", goos, tags, loadedPackages[0].Errors)
	}
	return loadedPackages[0]
}
