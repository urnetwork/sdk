// Keeps sdk.Sidecar whole on the gomobile surface (SDK-M8-06; sidecar §18.5's
// Gate table, R-SDK-5, D-SDK-30): every exported method of its handle types,
// read from the sdk's source as the mobile leg builds it, is bound in the
// generated Java, and the only Sidecar omissions gobind records are the Go-only
// members mobileSidecarGoOnlyIds names. No sidecar name may fall under the
// broad rules (a Proxy, an Rpc suffix, a DeviceRemote prefix) that would hide
// a lost method, so a new method that gobind cannot bind fails here.
package main

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The handle types §18.5's Gate table names that the sdk declares now; M10's
// SidecarSearch and SidecarSearchHandle add their own rows.
var mobileSidecarHandleTypes = []string{
	"Sidecar",
	"SidecarEvs",
	"SidecarMail",
	"SidecarSockets",
	"SidecarSocketListener",
	"SidecarApprovals",
	"SidecarDirectory",
}

// The sdk's package functions that open a Sidecar.
var mobileSidecarConstructors = []string{
	"NewSidecarLocal",
	"NewSidecarRemote",
	"DefaultSidecarSettings",
}

// The exported methods of each handle type, and every exported identifier of
// sdk.Sidecar's own files (sidecar*.go), from the sdk's files the mobile leg
// builds. The device role's files (device_sidecar*.go) name their RPC messages
// with the DeviceRemote prefix on purpose, to keep them out of the bindings.
func mobileSidecarSource(t *testing.T) (map[string][]string, []string) {
	t.Helper()
	sdkDirectory := filepath.Join("..", "..", "..")
	context := build.Default
	context.GOOS = "android"
	context.GOARCH = "arm64"
	context.BuildTags = []string{"sdk_mobile_bind"}
	entries, err := os.ReadDir(sdkDirectory)
	if err != nil {
		t.Fatal(err)
	}
	methods := map[string][]string{}
	identifiers := []string{}
	fileSet := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if !strings.HasPrefix(name, "sidecar") {
			continue
		}
		if match, err := context.MatchFile(sdkDirectory, name); err != nil || !match {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, filepath.Join(sdkDirectory, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range parsed.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if !declaration.Name.IsExported() {
					continue
				}
				identifiers = append(identifiers, declaration.Name.Name)
				if declaration.Recv == nil || len(declaration.Recv.List) != 1 {
					continue
				}
				star, ok := declaration.Recv.List[0].Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				if receiver, ok := star.X.(*ast.Ident); ok {
					methods[receiver.Name] = append(methods[receiver.Name], declaration.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					if typeSpec, ok := spec.(*ast.TypeSpec); ok && typeSpec.Name.IsExported() {
						identifiers = append(identifiers, typeSpec.Name.Name)
					}
				}
			}
		}
	}
	return methods, identifiers
}

// The bindings gobind makes for language, in a fresh directory, from the
// module cache alone.
func mobileSidecarGenerate(t *testing.T, language string) string {
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
		"GOWORK=off",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate %s bindings: %v\n%s", language, err, output)
	}
	return outputDirectory
}

// Java's name for a Go method.
func mobileSidecarJavaName(name string) string {
	return strings.ToLower(name[:1]) + name[1:]
}

// Every bound Sidecar method is present in the generated Java, each handle
// type's in its own class and the constructors on the package facade; the only
// sidecar omissions are mobileSidecarGoOnlyIds' members; and the generated
// source passes the check the android build runs on it.
func TestMobileSidecarIsBound(t *testing.T) {
	methods, _ := mobileSidecarSource(t)
	javaDirectory := mobileSidecarGenerate(t, "java")
	javaPath := func(name string) string {
		return filepath.Join(javaDirectory, "java", "com", "bringyour", "sdk", name)
	}
	for _, handle := range mobileSidecarHandleTypes {
		if len(methods[handle]) == 0 {
			t.Errorf("the sdk declares no method of %s", handle)
			continue
		}
		contents, err := os.ReadFile(javaPath(handle + ".java"))
		if err != nil {
			t.Errorf("%s is not bound: %v", handle, err)
			continue
		}
		for _, method := range methods[handle] {
			if mobileSidecarGoOnlyIds[handle+"."+method] {
				continue
			}
			if !strings.Contains(string(contents), " native ") || !strings.Contains(string(contents), " "+mobileSidecarJavaName(method)+"(") {
				t.Errorf("%s.%s is not bound", handle, method)
			}
		}
	}
	facade, err := os.ReadFile(javaPath("Sdk.java"))
	if err != nil {
		t.Fatal(err)
	}
	for _, constructor := range mobileSidecarConstructors {
		if !strings.Contains(string(facade), " "+mobileSidecarJavaName(constructor)+"(") {
			t.Errorf("%s is not bound", constructor)
		}
	}
	if err := validateMobileExports(javaDirectory); err != nil {
		t.Errorf("the generated bindings have unexpected omissions:\n%v", err)
	}
	// every sidecar omission gobind records is a named Go-only member
	err = filepath.WalkDir(javaDirectory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(path) != ".java" {
			return walkErr
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(contents), "\n") {
			identifier, ok := skippedIdentifier(line)
			if ok && strings.HasPrefix(identifier, "Sidecar") && !mobileSidecarGoOnlyIds[identifier] {
				t.Errorf("gobind omits %s, which no policy names", identifier)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// No exported sidecar name falls under a broad omission rule that would hide a
// lost method: a Proxy, an Rpc suffix, a DeviceRemote prefix. Each Go-only
// member is accepted by name alone, and a name beside it is not.
func TestMobileSidecarNamesAvoidTheBroadRules(t *testing.T) {
	_, identifiers := mobileSidecarSource(t)
	if !slices.Contains(identifiers, "SidecarApprovals") {
		t.Fatal("the sdk's sidecar source read to nothing")
	}
	for _, identifier := range identifiers {
		if strings.Contains(identifier, "Proxy") || strings.HasSuffix(identifier, "Rpc") || strings.HasPrefix(identifier, "DeviceRemote") {
			t.Errorf("%s falls under a broad omission rule", identifier)
		}
	}
	for identifier := range mobileSidecarGoOnlyIds {
		if !allowedMobileOmission(identifier) {
			t.Errorf("%s is not accepted", identifier)
		}
		if allowedMobileOmission(identifier + "Leak") {
			t.Errorf("a name beside %s is accepted", identifier)
		}
	}
	for _, handle := range mobileSidecarHandleTypes {
		if allowedMobileOmission(handle + ".GetPending") {
			t.Errorf("an omission of %s's methods would pass", handle)
		}
	}
}
