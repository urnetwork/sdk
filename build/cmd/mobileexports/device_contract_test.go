// Verifies the native and bind-only Device method sets at the generated
// foreign-proxy assignment boundary, without an Android or Apple toolchain.
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Objective-C selectors include Go parameter names. Interface satisfaction in
// Go alone cannot detect a concrete binding that spells a label differently.
func TestMobileSocketObjectiveCSelectorContract(t *testing.T) {
	selectors := map[string][]string{}
	for _, name := range []string{"device.go", "socket_mobile.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("../../..", name), nil, 0)
		testingBuildNoError(t, err)
		ast.Inspect(file, func(node ast.Node) bool {
			var owner string
			var signature *ast.FuncType
			switch declaration := node.(type) {
			case *ast.TypeSpec:
				if declaration.Name.Name != "Device" {
					return true
				}
				for _, field := range declaration.Type.(*ast.InterfaceType).Methods.List {
					if len(field.Names) == 1 && field.Names[0].Name == "OpenSocket" {
						owner, signature = "Device", field.Type.(*ast.FuncType)
					}
				}
			case *ast.FuncDecl:
				if declaration.Name.Name == "OpenSocket" && declaration.Recv != nil {
					owner = declaration.Recv.List[0].Type.(*ast.StarExpr).X.(*ast.Ident).Name
					signature = declaration.Type
				}
			}
			if signature != nil {
				for _, field := range signature.Params.List {
					for _, parameter := range field.Names {
						selectors[owner] = append(selectors[owner], parameter.Name)
					}
				}
			}
			return true
		})
	}
	for _, owner := range []string{"DeviceLocal", "DeviceRemote"} {
		if len(selectors[owner]) != 4 || !reflect.DeepEqual(selectors["Device"], selectors[owner]) {
			t.Fatalf("OpenSocket Objective-C selector mismatch: %#v", selectors)
		}
	}
}

// Native Go callers retain both standard dial signatures and secure extensions.
func TestNativeDeviceDialInterfaceContract(t *testing.T) {
	devicePackage := testingMobileDevicePackage(t, testingMobileDeviceSource(t, false))
	deviceInterface := devicePackage.Scope().Lookup("Device").Type().Underlying().(*types.Interface)
	for index := 0; index < deviceInterface.NumMethods(); index++ {
		if method := deviceInterface.Method(index); !method.Exported() {
			t.Errorf("native Device was sealed by unexported method %s", method.Name())
		}
	}
	for _, name := range []string{"Dialer", "TLSDialer"} {
		dialInterface := devicePackage.Scope().Lookup(name).Type().Underlying().(*types.Interface)
		if !types.Implements(deviceInterface, dialInterface) {
			t.Fatalf("native Device no longer implements %s", name)
		}
	}
	if types.NewMethodSet(deviceInterface).Lookup(nil, "OpenSocket") == nil {
		t.Fatal("native Device lost the portable OpenSocket entry point")
	}
}

// Only the binding view removes the native-only foreign-proxy obligations.
func TestMobileDevicePortableInterfaceContract(t *testing.T) {
	devicePackage := testingMobileDevicePackage(t, testingMobileDeviceSource(t, true))
	deviceInterface := devicePackage.Scope().Lookup("Device").Type().Underlying().(*types.Interface)
	methods := types.NewMethodSet(deviceInterface)
	for _, name := range []string{"Dial", "DialContext", "DialTls", "DialTlsContext"} {
		if methods.Lookup(nil, name) != nil {
			t.Errorf("bind-only Device still requires native-only method %s", name)
		}
	}
	for _, name := range []string{"OpenSocket", "GetDone"} {
		if methods.Lookup(nil, name) == nil {
			t.Errorf("bind-only Device lost portable method %s", name)
		}
	}
}

// Every binder must select the same interface view, including the extension.
func TestMobileDeviceBindingTagCoversAllTargets(t *testing.T) {
	makefileBytes, err := os.ReadFile("../../Makefile")
	testingBuildNoError(t, err)
	calls := strings.Split(string(makefileBytes), "gomobile bind \\\n")[1:]
	if len(calls) != 3 {
		t.Fatalf("found %d gomobile binds, want Android, Apple, and Apple extension", len(calls))
	}
	for index, remainder := range calls {
		call := strings.SplitN(remainder, `"github.com/urnetwork/sdk/v2026"`, 2)[0]
		if !strings.Contains(call, "-tags sdk_mobile_bind") {
			t.Errorf("gomobile bind %d does not select the portable Device view", index+1)
		}
		if index == 2 && !strings.Contains(call, "-tags sdk_mobile_bind,ios_extension") {
			t.Error("Apple extension bind must retain ios_extension alongside the binding tag")
		}
	}
}

// Uses the real pinned generator; stripping bodies/C support preserves the
// exact proxy method signatures and the assignment that failed in gomobile.
func TestMobileDeviceGeneratedProxyContract(t *testing.T) {
	source := testingMobileDeviceSource(t, true)
	generated := testingMobileDeviceGenerate(t, source)
	if err := testingMobileDeviceProxyAssignment(t, source, generated); err != nil {
		t.Fatalf("generated mobile proxy does not implement Device: %v", err)
	}
}

// The native method set reproduces the old missing-Dial generator failure,
// proving that an omission allowlist cannot fix an incomplete foreign proxy.
func TestMobileDeviceGeneratorRejectsNativeProxyObligations(t *testing.T) {
	source := testingMobileDeviceSource(t, false)
	generated := testingMobileDeviceGenerate(t, source)
	err := testingMobileDeviceProxyAssignment(t, source, generated)
	if err == nil || !strings.Contains(err.Error(), "missing method Dial") {
		t.Fatalf("native proxy boundary returned %v, want missing method Dial", err)
	}
}

// Copies actual interface declarations into a synthetic, focused package.
// Unrelated Device methods do not affect the socket/generator invariant.
func testingMobileDeviceSource(t *testing.T, mobile bool) string {
	t.Helper()
	const sdkDirectory = "../../.."
	fileSet := token.NewFileSet()
	declarations := []*ast.TypeSpec{}
	for _, path := range []string{filepath.Join(sdkDirectory, "device.go"), filepath.Join(sdkDirectory, "socket.go")} {
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		testingBuildNoError(t, err)
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				switch typeSpec.Name.Name {
				case "Device":
					deviceInterface := typeSpec.Type.(*ast.InterfaceType)
					fields := []*ast.Field{}
					for _, field := range deviceInterface.Methods.List {
						if len(field.Names) == 0 || field.Names[0].Name == "OpenSocket" || field.Names[0].Name == "GetDone" {
							fields = append(fields, field)
						}
					}
					deviceInterface.Methods.List = fields
					declarations = append(declarations, typeSpec)
				case "Dialer", "TLSDialer":
					declarations = append(declarations, typeSpec)
				}
			}
		}
	}
	context := build.Default
	context.BuildTags = nil
	if mobile {
		context.BuildTags = []string{"sdk_mobile_bind"}
	}
	entries, err := os.ReadDir(sdkDirectory)
	testingBuildNoError(t, err)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "device_socket_dialer_") || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		matches, err := context.MatchFile(sdkDirectory, entry.Name())
		testingBuildNoError(t, err)
		if !matches {
			continue
		}
		file, err := parser.ParseFile(fileSet, filepath.Join(sdkDirectory, entry.Name()), nil, 0)
		testingBuildNoError(t, err)
		for _, declaration := range file.Decls {
			if general, ok := declaration.(*ast.GenDecl); ok && general.Tok == token.TYPE {
				for _, spec := range general.Specs {
					if spec.(*ast.TypeSpec).Name.Name == "deviceSocketDialer" {
						declarations = append(declarations, spec.(*ast.TypeSpec))
					}
				}
			}
		}
	}
	var source bytes.Buffer
	source.WriteString("package sdk\nimport (\"context\"; \"crypto/tls\"; \"net\")\n")
	source.WriteString("type Socket struct{}\ntype SocketTLSOptions struct{}\n")
	for _, declaration := range declarations {
		source.WriteString("type ")
		testingBuildNoError(t, format.Node(&source, fileSet, declaration))
		source.WriteByte('\n')
	}
	source.WriteString("func AcceptDevice(device Device) {}\n")
	return source.String()
}

// Synthetic imported types preserve signature identity without loading a
// network stack or depending on installed compiler export archives.
type testingMobileDeviceImporter map[string]*types.Package

// Resolves only the explicitly declared synthetic packages.
func (self testingMobileDeviceImporter) Import(path string) (*types.Package, error) {
	if importedPackage, ok := self[path]; ok {
		return importedPackage, nil
	}
	return nil, fmt.Errorf("unexpected synthetic import %q", path)
}

// Type-checks the focused source with visibly synthetic package identities.
func testingMobileDevicePackage(t *testing.T, source string) *types.Package {
	t.Helper()
	imports := testingMobileDeviceImporter{}
	for _, entry := range []struct{ path, name, typeName string }{
		{path: "context", name: "context", typeName: "Context"},
		{path: "crypto/tls", name: "tls", typeName: "Config"},
		{path: "net", name: "net", typeName: "Conn"},
	} {
		importedPackage := types.NewPackage(entry.path, entry.name)
		typeName := types.NewTypeName(token.NoPos, importedPackage, entry.typeName, nil)
		types.NewNamed(typeName, types.NewStruct(nil, nil), nil)
		importedPackage.Scope().Insert(typeName)
		importedPackage.MarkComplete()
		imports[entry.path] = importedPackage
	}
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "sdk.go", source, 0)
	testingBuildNoError(t, err)
	configuration := types.Config{Importer: imports}
	devicePackage, err := configuration.Check("mobilefixture.example/sdk", fileSet, []*ast.File{file}, nil)
	testingBuildNoError(t, err)
	return devicePackage
}

// Runs gobind entirely in temporary output with module downloads disabled.
func testingMobileDeviceGenerate(t *testing.T, source string) []byte {
	t.Helper()
	environment := append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=-mod=mod")
	moduleBytes, err := os.ReadFile("../../go.mod")
	testingBuildNoError(t, err)
	version := ""
	for _, line := range strings.Split(string(moduleBytes), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[0] == "golang.org/x/mobile" {
			version = fields[1]
		}
	}
	if version == "" {
		t.Fatal("mobile build module no longer pins x/mobile")
	}
	fixtureDirectory := t.TempDir()
	module := "module mobilefixture.example/sdk\n\ngo 1.26.5\n\nrequire golang.org/x/mobile " + version + "\n"
	testingBuildNoError(t, os.WriteFile(filepath.Join(fixtureDirectory, "go.mod"), []byte(module), 0o600))
	sumBytes, err := os.ReadFile("../../go.sum")
	testingBuildNoError(t, err)
	testingBuildNoError(t, os.WriteFile(filepath.Join(fixtureDirectory, "go.sum"), sumBytes, 0o600))
	testingBuildNoError(t, os.WriteFile(filepath.Join(fixtureDirectory, "sdk.go"), []byte(source), 0o600))
	outputDirectory := filepath.Join(fixtureDirectory, "generated")
	command := exec.Command("go", "run", "golang.org/x/mobile/cmd/gobind", "-lang=go", "-outdir="+outputDirectory, ".")
	command.Dir = fixtureDirectory
	command.Env = environment
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("synthetic gobind generation failed: %v\n%s", err, output)
	}
	generated, err := os.ReadFile(filepath.Join(outputDirectory, "src", "gobind", "go_sdkmain.go"))
	testingBuildNoError(t, err)
	return generated
}

// Checks the generator's actual proxy signatures against its actual foreign
// assignment. C bodies are immaterial to Go interface satisfaction.
func testingMobileDeviceProxyAssignment(t *testing.T, source string, generated []byte) error {
	t.Helper()
	devicePackage := testingMobileDevicePackage(t, source)
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "go_sdkmain.go", generated, 0)
	testingBuildNoError(t, err)
	const proxyName = "proxysdk_Device"
	var skeleton bytes.Buffer
	skeleton.WriteString("package main\nimport sdk \"mobilefixture.example/sdk\"\ntype " + proxyName + " struct{}\n")
	foundProxy := false
	foundAssignment := false
	for _, declaration := range file.Decls {
		if general, ok := declaration.(*ast.GenDecl); ok && general.Tok == token.TYPE {
			for _, spec := range general.Specs {
				if spec.(*ast.TypeSpec).Name.Name == proxyName {
					foundProxy = true
				}
			}
		}
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if function.Recv != nil {
			receiver, ok := function.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			identifier, ok := receiver.X.(*ast.Ident)
			if !ok || identifier.Name != proxyName {
				continue
			}
			function.Body = nil
			testingBuildNoError(t, format.Node(&skeleton, fileSet, function))
			skeleton.WriteByte('\n')
		}
		if function.Name.Name == "proxysdk__AcceptDevice" {
			ast.Inspect(function, func(node ast.Node) bool {
				assignment, ok := node.(*ast.AssignStmt)
				if !ok || len(assignment.Rhs) != 1 {
					return true
				}
				var expression bytes.Buffer
				testingBuildNoError(t, format.Node(&expression, fileSet, assignment.Rhs[0]))
				if strings.Contains(expression.String(), "(*"+proxyName+")") {
					foundAssignment = true
				}
				return true
			})
		}
	}
	if !foundProxy || !foundAssignment {
		t.Fatal("gobind did not exercise the expected foreign Device proxy assignment")
	}
	skeleton.WriteString("var _ sdk.Device = (*" + proxyName + ")(nil)\n")
	skeletonFile, err := parser.ParseFile(fileSet, "proxy.go", skeleton.Bytes(), 0)
	testingBuildNoError(t, err)
	configuration := types.Config{Importer: testingMobileDeviceImporter{devicePackage.Path(): devicePackage}}
	_, err = configuration.Check("mobilefixture.example/gobind", fileSet, []*ast.File{skeletonFile}, nil)
	return err
}

// Fails immediately on source, fixture, or tool setup errors.
func testingBuildNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
