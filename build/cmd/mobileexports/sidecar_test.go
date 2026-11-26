// Keeps sdk.Sidecar whole on the gomobile surface (SDK-M8-06; sidecar §18.5's
// Gate table, R-SDK-5, D-SDK-30; DESIGN-COMPUTE §15.3). The handle types are
// read from the sdk's source as the mobile leg builds it, and the scan fails on
// a handle mobileSidecarHandleTypes leaves out, and on a name it keeps that the
// sdk no longer declares as a handle, rather than passing on gobind's omission
// records alone. Each handle is held to the generated bindings: in Java its
// class, every exported method and a bound call that answers it, which is how
// an app comes to hold one; in Objective-C its interface and every exported
// method. The only Sidecar omissions gobind records are the Go-only members
// mobileSidecarGoOnlyIds names. No sidecar name may fall under the broad rules
// (a Proxy, an Rpc suffix, a DeviceRemote prefix) that would hide a lost
// method, so a new method that gobind cannot bind fails here.
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
	"unicode"
)

// The handle types: §18.5's Gate table's, M10's SidecarSearch and
// SidecarSearchHandle (SDK-M10-01), the desktop apps' SidecarDiscovery
// (§17.7.1, owner decision 778), which the macOS app reaches through these
// bindings, and DESIGN-COMPUTE §15.3's wrappers (CSURF-M11-01) with the support
// conversation's (§28.4). Several wrappers are shells whose methods their
// areas add in their own files; each method is held bound once it lands. A
// handle the sdk adds fails TestMobileSidecarHandleListIsTheSdks until it is
// named here.
var mobileSidecarHandleTypes = []string{
	"Sidecar",
	"SidecarEvs",
	"SidecarMail",
	"SidecarSockets",
	"SidecarSocketListener",
	"SidecarApprovals",
	"SidecarDirectory",
	"SidecarSearch",
	"SidecarSearchHandle",
	"SidecarDiscovery",
	"SidecarAgents",
	"SidecarAutoApprovals",
	"SidecarCalendar",
	"SidecarCompute",
	"SidecarConnections",
	"SidecarEnclaves",
	"SidecarHarness",
	"SidecarListeners",
	"SidecarModels",
	"SidecarSshSession",
	"SidecarSupport",
}

// The sdk's package functions that open a Sidecar.
var mobileSidecarConstructors = []string{
	"NewSidecarLocal",
	"NewSidecarRemote",
	"DefaultSidecarSettings",
}

// The sdk's source, from this package's directory.
var mobileSidecarSdkDirectory = filepath.Join("..", "..", "..")

// The words after which gobind adds an underscore to a Java name (x/mobile's
// bind/genjava.go javaNameReplacer).
var mobileSidecarJavaReservedNames = map[string]bool{
	"abstract": true, "assert": true, "boolean": true, "break": true, "byte": true, "case": true, "catch": true,
	"char": true, "class": true, "const": true, "continue": true, "default": true, "do": true, "double": true,
	"else": true, "enum": true, "extends": true, "final": true, "finally": true, "float": true, "for": true,
	"goto": true, "if": true, "implements": true, "import": true, "instanceof": true, "int": true, "interface": true,
	"long": true, "native": true, "new": true, "package": true, "private": true, "protected": true, "public": true,
	"return": true, "short": true, "static": true, "strictfp": true, "super": true, "switch": true,
	"synchronized": true, "this": true, "throw": true, "throws": true, "transient": true, "try": true, "void": true,
	"volatile": true, "while": true, "false": true, "null": true, "true": true,
}

// The words after which gobind adds an underscore to an Objective-C name
// (x/mobile's bind/genobjc.go objcNameReplacer).
var mobileSidecarObjcReservedNames = map[string]bool{
	"bool": true, "bycopy": true, "byref": true, "char": true, "const": true, "double": true, "float": true,
	"id": true, "in": true, "init": true, "inout": true, "int": true, "long": true, "nil": true, "oneway": true,
	"out": true, "self": true, "short": true, "signed": true, "super": true, "unsigned": true, "void": true,
	"volatile": true,
}

// What the sdk's source declares as the mobile leg builds it: GOOS android,
// with cgo and the sdk_mobile_bind tag, as gomobile's android bind does.
type mobileSidecarSurface struct {
	// each type's exported methods by its name, from whichever file declares them
	typeMethodNames map[string][]string
	// every exported identifier of sdk.Sidecar's own files (sidecar*.go). The
	// device role's files (device_sidecar*.go) name their RPC messages with the
	// DeviceRemote prefix on purpose, to keep them out of the bindings.
	sidecarIdentifiers []string
	// the exported Sidecar types gobind binds as a class an app holds and calls
	// but never reads, sorted, from whichever file declares them: each a struct
	// with no exported field that embeds no exportedList. A data type's fields
	// bind as accessors and a list's exportedList as its methods, so neither is
	// a handle; nor is an interface, a func type or an alias.
	handleTypes []string
}

// Reads directory's non-test Go files that the mobile leg builds.
func mobileSidecarSource(t *testing.T, directory string) *mobileSidecarSurface {
	t.Helper()
	context := build.Default
	context.GOOS = "android"
	context.GOARCH = "arm64"
	context.CgoEnabled = true
	context.BuildTags = []string{"sdk_mobile_bind"}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	// the type a method's receiver names, or "" for a function or a generic
	// type's method, which binds on no class of its own
	receiverName := func(declaration *ast.FuncDecl) string {
		if declaration.Recv == nil || len(declaration.Recv.List) != 1 {
			return ""
		}
		receiverType := declaration.Recv.List[0].Type
		if star, ok := receiverType.(*ast.StarExpr); ok {
			receiverType = star.X
		}
		if identifier, ok := receiverType.(*ast.Ident); ok {
			return identifier.Name
		}
		return ""
	}
	isHandle := func(typeSpec *ast.TypeSpec) bool {
		structType, ok := typeSpec.Type.(*ast.StructType)
		if !ok || typeSpec.Assign.IsValid() || typeSpec.TypeParams != nil {
			return false
		}
		for _, field := range structType.Fields.List {
			for _, fieldName := range field.Names {
				if fieldName.IsExported() {
					return false
				}
			}
			if 0 < len(field.Names) {
				continue
			}
			// an embedded field is named for its type, which a pointer, a
			// package or type arguments may wrap
			embeddedType := field.Type
			if star, ok := embeddedType.(*ast.StarExpr); ok {
				embeddedType = star.X
			}
			switch instance := embeddedType.(type) {
			case *ast.IndexExpr:
				embeddedType = instance.X
			case *ast.IndexListExpr:
				embeddedType = instance.X
			}
			if selector, ok := embeddedType.(*ast.SelectorExpr); ok {
				embeddedType = selector.Sel
			}
			identifier, ok := embeddedType.(*ast.Ident)
			if !ok || identifier.IsExported() || identifier.Name == "exportedList" {
				return false
			}
		}
		return true
	}
	surface := &mobileSidecarSurface{
		typeMethodNames: map[string][]string{},
	}
	fileSet := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		match, err := context.MatchFile(directory, name)
		if err != nil {
			t.Fatal(err)
		}
		if !match {
			continue
		}
		parsed, err := parser.ParseFile(fileSet, filepath.Join(directory, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		sidecarFile := strings.HasPrefix(name, "sidecar")
		for _, declaration := range parsed.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if !declaration.Name.IsExported() {
					continue
				}
				if sidecarFile {
					surface.sidecarIdentifiers = append(surface.sidecarIdentifiers, declaration.Name.Name)
				}
				if typeName := receiverName(declaration); typeName != "" {
					surface.typeMethodNames[typeName] = append(surface.typeMethodNames[typeName], declaration.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok || !typeSpec.Name.IsExported() {
						continue
					}
					if sidecarFile {
						surface.sidecarIdentifiers = append(surface.sidecarIdentifiers, typeSpec.Name.Name)
					}
					if strings.HasPrefix(typeSpec.Name.Name, "Sidecar") && isHandle(typeSpec) {
						surface.handleTypes = append(surface.handleTypes, typeSpec.Name.Name)
					}
				}
			}
		}
	}
	slices.Sort(surface.handleTypes)
	return surface
}

// The handles surface declares that handleTypes leaves out, and the names
// handleTypes keeps that surface declares as no handle.
func mobileSidecarHandleListGaps(surface *mobileSidecarSurface, handleTypes []string) ([]string, []string) {
	leftOutTypes := []string{}
	for _, handleType := range surface.handleTypes {
		if !slices.Contains(handleTypes, handleType) {
			leftOutTypes = append(leftOutTypes, handleType)
		}
	}
	staleTypes := []string{}
	for _, handleType := range handleTypes {
		if !slices.Contains(surface.handleTypes, handleType) {
			staleTypes = append(staleTypes, handleType)
		}
	}
	return leftOutTypes, staleTypes
}

// gobind's name for a Go method or function in a language: lowerFirst's, which
// lowercases a leading acronym but for the letter that starts the next word
// (OSName to osName), then an underscore after a word the language reserves
// (x/mobile's bind/gen.go).
func mobileSidecarBoundName(name string, reservedNames map[string]bool) string {
	runes := []rune(name)
	loweredCount := 0
	for loweredCount < len(runes) && unicode.IsUpper(runes[loweredCount]) {
		runes[loweredCount] = unicode.ToLower(runes[loweredCount])
		loweredCount += 1
	}
	if 1 < loweredCount && loweredCount < len(runes) {
		runes[loweredCount-1] = unicode.ToUpper(runes[loweredCount-1])
	}
	boundName := string(runes)
	if reservedNames[boundName] {
		return boundName + "_"
	}
	return boundName
}

// What the Java bindings under directory lack of handleTypes, each a line of
// the report, sorted: a handle's class, a method surface declares on one but
// for mobileSidecarGoOnlyIds' members, and a bound call that answers one; and
// every Sidecar omission gobind records that mobileSidecarGoOnlyIds does not
// name. Classes are found by file name, so gobind's tree and the sources jar's
// read alike.
func mobileSidecarJavaProblems(directory string, surface *mobileSidecarSurface, handleTypes []string) ([]string, error) {
	classSources := map[string]string{}
	problems := []string{}
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(path) != ".java" {
			return walkErr
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		classSources[strings.TrimSuffix(entry.Name(), ".java")] = string(contents)
		for _, line := range strings.Split(string(contents), "\n") {
			identifier, ok := skippedIdentifier(line)
			if ok && strings.HasPrefix(identifier, "Sidecar") && !mobileSidecarGoOnlyIds[identifier] {
				problems = append(problems, "gobind omits "+identifier+", which no policy names")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, handleType := range handleTypes {
		source, ok := classSources[handleType]
		if !ok || !strings.Contains(source, "public final class "+handleType+" ") {
			problems = append(problems, handleType+" is not bound")
			continue
		}
		lines := strings.Split(source, "\n")
		for _, methodName := range surface.typeMethodNames[handleType] {
			if mobileSidecarGoOnlyIds[handleType+"."+methodName] {
				continue
			}
			javaName := mobileSidecarBoundName(methodName, mobileSidecarJavaReservedNames)
			bound := slices.ContainsFunc(lines, func(line string) bool {
				return strings.Contains(line, "native ") && strings.Contains(line, " "+javaName+"(")
			})
			if !bound {
				problems = append(problems, handleType+"."+methodName+" is not bound")
			}
		}
		// an app holds a handle only as a bound call's answer
		answered := false
		for _, answeringSource := range classSources {
			if strings.Contains(answeringSource, "native "+handleType+" ") {
				answered = true
				break
			}
		}
		if !answered {
			problems = append(problems, "no bound call answers "+handleType)
		}
	}
	slices.Sort(problems)
	return problems, nil
}

// What the Objective-C header lacks of handleTypes, each a line of the report,
// sorted: a handle's interface and a method surface declares on one but for
// mobileSidecarGoOnlyIds' members; and every Sidecar omission gobind records
// there that mobileSidecarGoOnlyIds does not name.
func mobileSidecarObjcProblems(header string, surface *mobileSidecarSurface, handleTypes []string) []string {
	problems := []string{}
	// each interface's method declarations, by the name of the Go type it binds
	interfaceMethodLines := map[string][]string{}
	interfaceName := ""
	for _, line := range strings.Split(header, "\n") {
		identifier, ok := skippedIdentifier(line)
		if ok && strings.HasPrefix(identifier, "Sidecar") && !mobileSidecarGoOnlyIds[identifier] {
			problems = append(problems, "gobind omits "+identifier+" from Objective-C, which no policy names")
		}
		fields := strings.Fields(line)
		switch {
		case 3 <= len(fields) && fields[0] == "@interface" && fields[2] == ":":
			interfaceName = strings.TrimPrefix(fields[1], "Sdk")
			interfaceMethodLines[interfaceName] = []string{}
		case strings.HasPrefix(line, "@end"):
			interfaceName = ""
		case interfaceName != "" && strings.HasPrefix(line, "- ("):
			interfaceMethodLines[interfaceName] = append(interfaceMethodLines[interfaceName], line)
		}
	}
	for _, handleType := range handleTypes {
		methodLines, ok := interfaceMethodLines[handleType]
		if !ok {
			problems = append(problems, handleType+" is not bound in Objective-C")
			continue
		}
		for _, methodName := range surface.typeMethodNames[handleType] {
			if mobileSidecarGoOnlyIds[handleType+"."+methodName] {
				continue
			}
			// a selector opens with the name, then its first parameter or the end
			objcName := mobileSidecarBoundName(methodName, mobileSidecarObjcReservedNames)
			bound := slices.ContainsFunc(methodLines, func(line string) bool {
				return strings.Contains(line, ")"+objcName+":") || strings.Contains(line, ")"+objcName+";")
			})
			if !bound {
				problems = append(problems, handleType+"."+methodName+" is not bound in Objective-C")
			}
		}
	}
	slices.Sort(problems)
	return problems
}

// The bindings gobind makes for languages, in a fresh directory, from the
// module cache alone.
func mobileSidecarGenerate(t *testing.T, languages ...string) string {
	t.Helper()
	outputDirectory := t.TempDir()
	args := []string{
		"run", "golang.org/x/mobile/cmd/gobind",
		"-lang=" + strings.Join(languages, ","),
		"-tags=sdk_mobile_bind",
		"-outdir=" + outputDirectory,
	}
	if slices.Contains(languages, "java") {
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
		t.Fatalf("generate %s bindings: %v\n%s", strings.Join(languages, ","), err, output)
	}
	return outputDirectory
}

// The handle list is the sdk's, from its source alone: every handle the sdk
// declares on the mobile leg is named in mobileSidecarHandleTypes and every
// name there is one, so a handle added without its row fails before any
// binding is generated.
func TestMobileSidecarHandleListIsTheSdks(t *testing.T) {
	surface := mobileSidecarSource(t, mobileSidecarSdkDirectory)
	leftOutTypes, staleTypes := mobileSidecarHandleListGaps(surface, mobileSidecarHandleTypes)
	for _, handleType := range leftOutTypes {
		t.Errorf("the sdk declares the handle %s, which mobileSidecarHandleTypes leaves out", handleType)
	}
	for _, handleType := range staleTypes {
		t.Errorf("mobileSidecarHandleTypes names %s, which the sdk does not declare as a handle", handleType)
	}
}

// Every handle is bound in the generated Java and Objective-C, the handle list
// is the sdk's, a handle it leaves out is held to the bindings too, the
// constructors are on the package facade, and the generated source passes the
// check the android build runs on it.
func TestMobileSidecarIsBound(t *testing.T) {
	surface := mobileSidecarSource(t, mobileSidecarSdkDirectory)
	leftOutTypes, staleTypes := mobileSidecarHandleListGaps(surface, mobileSidecarHandleTypes)
	for _, handleType := range leftOutTypes {
		t.Errorf("the sdk declares the handle %s, which mobileSidecarHandleTypes leaves out", handleType)
	}
	for _, handleType := range staleTypes {
		t.Errorf("mobileSidecarHandleTypes names %s, which the sdk does not declare as a handle", handleType)
	}
	handleTypes := append(slices.Clone(mobileSidecarHandleTypes), leftOutTypes...)
	outputDirectory := mobileSidecarGenerate(t, "java", "objc")
	javaProblems, err := mobileSidecarJavaProblems(filepath.Join(outputDirectory, "java"), surface, handleTypes)
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range javaProblems {
		t.Error(problem)
	}
	header, err := os.ReadFile(filepath.Join(outputDirectory, "src", "gobind", "Sdk.objc.h"))
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range mobileSidecarObjcProblems(string(header), surface, handleTypes) {
		t.Error(problem)
	}
	facade, err := os.ReadFile(filepath.Join(outputDirectory, "java", "com", "bringyour", "sdk", "Sdk.java"))
	if err != nil {
		t.Fatal(err)
	}
	for _, constructor := range mobileSidecarConstructors {
		if !strings.Contains(string(facade), " "+mobileSidecarBoundName(constructor, mobileSidecarJavaReservedNames)+"(") {
			t.Errorf("%s is not bound", constructor)
		}
	}
	if err := validateMobileExports(outputDirectory); err != nil {
		t.Errorf("the generated bindings have unexpected omissions:\n%v", err)
	}
}

// The scan fails on a handle the list leaves out, whichever file declares it,
// a shell with no method and one embedding an unexported type among them, and
// on a name the list keeps that the sdk declares as no handle. A data type, a
// list, an interface, a func type, an alias, a type of another prefix, a type
// the mobile leg does not build and a test's type are no handles.
func TestMobileSidecarHandleListGapsFailTheScan(t *testing.T) {
	directory := t.TempDir()
	sources := map[string]string{
		"sidecar_fixture.go": `package sdk

type SidecarFixtureListed struct {
	args *int
}

func (self *SidecarFixtureListed) Run() {}

func (self *SidecarFixtureListed) run() {}

type SidecarFixtureShell struct {
	args *int
}

type SidecarFixtureEmbedded struct {
	sidecarFixtureBase
}

type sidecarFixtureBase struct {
	args *int
}

type SidecarFixtureData struct {
	Name string
	args *int
}

type SidecarFixtureList struct {
	exportedList[*SidecarFixtureData]
}

type SidecarFixtureListener interface {
	OnEvent(eventJson string)
}

type SidecarFixtureCallback func(eventJson string)

type SidecarFixtureAlias = SidecarFixtureListed

type FixtureController struct {
	args *int
}
`,
		"device_fixture.go": `package sdk

type SidecarFixtureElsewhere struct {
	args *int
}

func (self *SidecarFixtureElsewhere) Stop() {}
`,
		"sidecar_fixture_js.go": `//go:build js

package sdk

type SidecarFixtureBrowser struct {
	args *int
}
`,
		"sidecar_fixture_extension.go": `//go:build ios_extension

package sdk

type SidecarFixtureExtension struct {
	args *int
}
`,
		"sidecar_fixture_test.go": `package sdk

type SidecarFixtureInTest struct {
	args *int
}
`,
	}
	for name, source := range sources {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	surface := mobileSidecarSource(t, directory)
	handleTypes := []string{"SidecarFixtureElsewhere", "SidecarFixtureEmbedded", "SidecarFixtureListed", "SidecarFixtureShell"}
	if !slices.Equal(surface.handleTypes, handleTypes) {
		t.Fatalf("the source declares the handles %v, not %v", surface.handleTypes, handleTypes)
	}
	if !slices.Equal(surface.typeMethodNames["SidecarFixtureListed"], []string{"Run"}) ||
		!slices.Equal(surface.typeMethodNames["SidecarFixtureElsewhere"], []string{"Stop"}) {
		t.Errorf("the source declares the methods %v", surface.typeMethodNames)
	}
	leftOutTypes, staleTypes := mobileSidecarHandleListGaps(
		surface,
		[]string{"SidecarFixtureListed", "SidecarFixtureData", "SidecarFixtureGone"},
	)
	if !slices.Equal(leftOutTypes, []string{"SidecarFixtureElsewhere", "SidecarFixtureEmbedded", "SidecarFixtureShell"}) {
		t.Errorf("the scan finds %v left out", leftOutTypes)
	}
	if !slices.Equal(staleTypes, []string{"SidecarFixtureData", "SidecarFixtureGone"}) {
		t.Errorf("the scan finds %v stale", staleTypes)
	}
	leftOutTypes, staleTypes = mobileSidecarHandleListGaps(surface, handleTypes)
	if len(leftOutTypes) != 0 || len(staleTypes) != 0 {
		t.Errorf("the scan fails a whole list: %v left out, %v stale", leftOutTypes, staleTypes)
	}
}

// The Java scan fails on a handle the bindings do not export whole: a class
// gobind did not write, a method missing from one, no bound call answering
// one, and an omission of a handle's method that no policy names. It passes on
// bindings that export each, a Go-only member aside, with gobind's names for
// reserved words and acronyms.
func TestMobileSidecarJavaScanFailsOnAHandleNotExported(t *testing.T) {
	surface := &mobileSidecarSurface{
		typeMethodNames: map[string][]string{
			"Sidecar":              {"Core", "FixtureListed"},
			"SidecarFixtureListed": {"Run", "OpenSsh", "New", "URLFor"},
		},
		handleTypes: []string{"Sidecar", "SidecarFixtureListed"},
	}
	wholeSources := map[string]string{
		"Sdk": `public abstract class Sdk {
	public static native Sidecar newSidecarLocal(NetworkSpace networkSpace) throws Exception;
}`,
		"Sidecar": `public final class Sidecar implements Seq.Proxy {
	// skipped method Sidecar.Core with unsupported parameter or return types

	public native SidecarFixtureListed fixtureListed();
}`,
		"SidecarFixtureListed": `public final class SidecarFixtureListed implements Seq.Proxy {
	public native void run();
	public native SidecarSshSession openSsh(String host) throws Exception;
	public native void new_();
	public native String urlFor(String name);
}`,
	}
	scan := func(sources map[string]string, handleTypes []string) []string {
		directory := t.TempDir()
		classDirectory := filepath.Join(directory, "java", "com", "bringyour", "sdk")
		if err := os.MkdirAll(classDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, source := range sources {
			contents := "package com.bringyour.sdk;\n\n" + source + "\n"
			if err := os.WriteFile(filepath.Join(classDirectory, name+".java"), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		problems, err := mobileSidecarJavaProblems(directory, surface, handleTypes)
		if err != nil {
			t.Fatal(err)
		}
		return problems
	}
	// a source of the whole set with one of its classes rewritten
	replaced := func(name string, oldText string, newText string) map[string]string {
		sources := map[string]string{}
		for sourceName, source := range wholeSources {
			sources[sourceName] = source
		}
		if !strings.Contains(sources[name], oldText) {
			t.Fatalf("%s has no %q", name, oldText)
		}
		sources[name] = strings.Replace(sources[name], oldText, newText, 1)
		return sources
	}
	if problems := scan(wholeSources, surface.handleTypes); len(problems) != 0 {
		t.Errorf("the scan fails bindings that export each handle: %v", problems)
	}
	for _, c := range []struct {
		sources     map[string]string
		handleTypes []string
		problems    []string
	}{
		{
			sources:     replaced("SidecarFixtureListed", "\tpublic native void run();\n", ""),
			handleTypes: surface.handleTypes,
			problems:    []string{"SidecarFixtureListed.Run is not bound"},
		},
		{
			sources:     replaced("SidecarFixtureListed", "urlFor(", "uRLFor("),
			handleTypes: surface.handleTypes,
			problems:    []string{"SidecarFixtureListed.URLFor is not bound"},
		},
		{
			sources: replaced(
				"SidecarFixtureListed",
				"\tpublic native void run();\n",
				"\t// skipped method SidecarFixtureListed.Run with unsupported parameter or return types\n",
			),
			handleTypes: surface.handleTypes,
			problems: []string{
				"SidecarFixtureListed.Run is not bound",
				"gobind omits SidecarFixtureListed.Run, which no policy names",
			},
		},
		{
			sources:     replaced("Sidecar", "\tpublic native SidecarFixtureListed fixtureListed();\n", ""),
			handleTypes: surface.handleTypes,
			problems: []string{
				"Sidecar.FixtureListed is not bound",
				"no bound call answers SidecarFixtureListed",
			},
		},
		{
			sources:     wholeSources,
			handleTypes: append(slices.Clone(surface.handleTypes), "SidecarFixtureShell"),
			problems:    []string{"SidecarFixtureShell is not bound"},
		},
	} {
		if problems := scan(c.sources, c.handleTypes); !slices.Equal(problems, c.problems) {
			t.Errorf("the scan finds %v, not %v", problems, c.problems)
		}
	}
}

// The Objective-C scan fails on a handle's interface gobind did not write, a
// method missing from one, and an omission no policy names. It passes on a
// header that binds each, a Go-only member aside, with gobind's names for
// reserved words.
func TestMobileSidecarObjcScanFailsOnAHandleNotExported(t *testing.T) {
	surface := &mobileSidecarSurface{
		typeMethodNames: map[string][]string{
			"Sidecar":              {"Core", "FixtureListed"},
			"SidecarFixtureListed": {"Run", "OpenSsh", "Init"},
		},
		handleTypes: []string{"Sidecar", "SidecarFixtureListed"},
	}
	header := strings.Join([]string{
		"@class SdkSidecarFixtureListed;",
		"@interface SdkSidecar : NSObject <goSeqRefInterface> {",
		"}",
		"- (SdkSidecarFixtureListed* _Nullable)fixtureListed;",
		"// skipped method Sidecar.Core with unsupported parameter or return types",
		"",
		"@end",
		"",
		"@interface SdkSidecarFixtureListed : NSObject <goSeqRefInterface> {",
		"}",
		"- (void)run;",
		"- (SdkSidecarSshSession* _Nullable)openSsh:(NSString* _Nullable)host error:(NSError* _Nullable* _Nullable)error;",
		"- (void)init_;",
		"@end",
	}, "\n")
	if problems := mobileSidecarObjcProblems(header, surface, surface.handleTypes); len(problems) != 0 {
		t.Errorf("the scan fails a header that binds each handle: %v", problems)
	}
	for _, c := range []struct {
		header      string
		handleTypes []string
		problems    []string
	}{
		{
			header:      strings.Replace(header, "- (void)run;", "- (void)running;", 1),
			handleTypes: surface.handleTypes,
			problems:    []string{"SidecarFixtureListed.Run is not bound in Objective-C"},
		},
		{
			header:      strings.Replace(header, "- (void)init_;", "- (void)init;", 1),
			handleTypes: surface.handleTypes,
			problems:    []string{"SidecarFixtureListed.Init is not bound in Objective-C"},
		},
		{
			header: strings.Replace(
				header,
				"- (void)run;",
				"// skipped method SidecarFixtureListed.Run with unsupported parameter or return types",
				1,
			),
			handleTypes: surface.handleTypes,
			problems: []string{
				"SidecarFixtureListed.Run is not bound in Objective-C",
				"gobind omits SidecarFixtureListed.Run from Objective-C, which no policy names",
			},
		},
		{
			// a method outside its interface binds nothing there
			header:      strings.Replace(header, "- (void)run;\n", "", 1) + "\n- (void)run;",
			handleTypes: surface.handleTypes,
			problems:    []string{"SidecarFixtureListed.Run is not bound in Objective-C"},
		},
		{
			header:      header,
			handleTypes: append(slices.Clone(surface.handleTypes), "SidecarFixtureShell"),
			problems:    []string{"SidecarFixtureShell is not bound in Objective-C"},
		},
	} {
		if problems := mobileSidecarObjcProblems(c.header, surface, c.handleTypes); !slices.Equal(problems, c.problems) {
			t.Errorf("the scan finds %v, not %v", problems, c.problems)
		}
	}
}

// Names are gobind's: a leading acronym lowercased but for the letter that
// starts the next word, and an underscore after a word the language reserves.
func TestMobileSidecarBoundNamesAreGobinds(t *testing.T) {
	for _, c := range []struct {
		name     string
		javaName string
		objcName string
	}{
		{name: "Close", javaName: "close", objcName: "close"},
		{name: "OpenSsh", javaName: "openSsh", objcName: "openSsh"},
		{name: "GetDiscovery", javaName: "getDiscovery", objcName: "getDiscovery"},
		{name: "URLFor", javaName: "urlFor", objcName: "urlFor"},
		{name: "ID", javaName: "id", objcName: "id_"},
		{name: "New", javaName: "new_", objcName: "new"},
		{name: "Init", javaName: "init", objcName: "init_"},
		{name: "A", javaName: "a", objcName: "a"},
	} {
		if javaName := mobileSidecarBoundName(c.name, mobileSidecarJavaReservedNames); javaName != c.javaName {
			t.Errorf("%s binds in Java as %s, not %s", c.name, javaName, c.javaName)
		}
		if objcName := mobileSidecarBoundName(c.name, mobileSidecarObjcReservedNames); objcName != c.objcName {
			t.Errorf("%s binds in Objective-C as %s, not %s", c.name, objcName, c.objcName)
		}
	}
}

// No exported sidecar name falls under a broad omission rule that would hide a
// lost method: a Proxy, an Rpc suffix, a DeviceRemote prefix. Each Go-only
// member is accepted by name alone, and a name beside it is not; and no
// handle's method, whichever file declares it, would pass as an omission but
// the Go-only members.
func TestMobileSidecarNamesAvoidTheBroadRules(t *testing.T) {
	surface := mobileSidecarSource(t, mobileSidecarSdkDirectory)
	if !slices.Contains(surface.sidecarIdentifiers, "SidecarApprovals") {
		t.Fatal("the sdk's sidecar source read to nothing")
	}
	for _, identifier := range surface.sidecarIdentifiers {
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
	for _, handleType := range mobileSidecarHandleTypes {
		if allowedMobileOmission(handleType + ".GetPending") {
			t.Errorf("an omission of %s's methods would pass", handleType)
		}
		for _, methodName := range surface.typeMethodNames[handleType] {
			identifier := handleType + "." + methodName
			if allowedMobileOmission(identifier) && !mobileSidecarGoOnlyIds[identifier] {
				t.Errorf("an omission of %s would pass", identifier)
			}
		}
	}
}

// The VPN component's forwarding key store is whole on the gomobile surface
// (QD-844; sidecar §18.6's Setup, owner decision 197): the app's interface and
// its answer, the setter on DeviceLocalSettings, and
// NewDeviceLocalWithForwardKeyStore on the package facade, in Java and in
// Objective-C, and the generated source passes the check the android build
// runs on it.
func TestMobileForwardKeyStoreIsBound(t *testing.T) {
	outputDirectory := mobileSidecarGenerate(t, "java", "objc")
	javaPath := func(name string) string {
		return filepath.Join(outputDirectory, "java", "com", "bringyour", "sdk", name)
	}
	files := []struct {
		path  string
		texts []string
	}{
		{
			path: javaPath("Sdk.java"),
			texts: []string{
				"public static native DeviceLocal newDeviceLocalWithForwardKeyStore(NetworkSpace networkSpace, String byJwt, String deviceDescription, String deviceSpec, String appVersion, Id instanceId, DeviceLocalSettings settings, SidecarForwardKeyStore forwardKeyStore) throws Exception;",
			},
		},
		{
			path: javaPath("DeviceLocalSettings.java"),
			texts: []string{
				"public native void setForwardKeyStore(SidecarForwardKeyStore keyStore);",
				"public final native String getForwardStateDir();",
				"public final native void setForwardStateDir(String v);",
			},
		},
		{
			path: javaPath("SidecarForwardKeyStore.java"),
			texts: []string{
				"public int putSecret(String name, String valueBase64);",
				"public SidecarForwardSecret getSecret(String name);",
				"public int deleteSecret(String name);",
			},
		},
		{
			path: javaPath("SidecarForwardSecret.java"),
			texts: []string{
				"public final native int getStatus();",
				"public final native void setStatus(int v);",
				"public final native String getValueBase64();",
				"public final native void setValueBase64(String v);",
			},
		},
		{
			path: filepath.Join(outputDirectory, "src", "gobind", "Sdk.objc.h"),
			texts: []string{
				"FOUNDATION_EXPORT SdkDeviceLocal* _Nullable SdkNewDeviceLocalWithForwardKeyStore(SdkNetworkSpace* _Nullable networkSpace, NSString* _Nullable byJwt, NSString* _Nullable deviceDescription, NSString* _Nullable deviceSpec, NSString* _Nullable appVersion, SdkId* _Nullable instanceId, SdkDeviceLocalSettings* _Nullable settings, id<SdkSidecarForwardKeyStore> _Nullable forwardKeyStore, NSError* _Nullable* _Nullable error);",
				"- (void)setForwardKeyStore:(id<SdkSidecarForwardKeyStore> _Nullable)keyStore;",
				"@protocol SdkSidecarForwardKeyStore <NSObject>",
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
	if err := validateMobileExports(outputDirectory); err != nil {
		t.Errorf("the generated bindings have unexpected omissions:\n%v", err)
	}
}

// The gate refuses generated source that omits a forwarding key store entry
// point, each in the record gobind writes for it: the setter, which
// DeviceLocalSettings' broad rule would have let through, and the
// constructor's two records. Another DeviceLocalSettings omission passes as
// before, and so does source with none.
func TestMobileForwardKeyStoreOmissionFailsTheGate(t *testing.T) {
	for identifier := range mobileForwardKeyStoreBoundIds {
		if allowedMobileOmission(identifier) {
			t.Errorf("an omission of %s is accepted", identifier)
		}
	}
	if !allowedMobileOmission("DeviceLocalSettings.GeneratorFunc") {
		t.Error("the settings' Go-only fields are no longer accepted")
	}
	write := func(lines ...string) string {
		directory := t.TempDir()
		source := "package com.bringyour.sdk;\n" + strings.Join(lines, "\n") + "\n"
		if err := os.WriteFile(filepath.Join(directory, "Fixture.java"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		return directory
	}
	for _, record := range []string{
		"\t// skipped method DeviceLocalSettings.SetForwardKeyStore with unsupported parameter or return types",
		"\t// skipped function NewDeviceLocalWithForwardKeyStore with unsupported parameter or return types",
		"\t// skipped constructor DeviceLocal.NewDeviceLocalWithForwardKeyStore with unsupported parameter or return types",
	} {
		err := validateMobileExports(write(record))
		if err == nil || !strings.Contains(err.Error(), strings.TrimSpace(record)) {
			t.Errorf("the gate passed %q: %v", strings.TrimSpace(record), err)
		}
	}
	if err := validateMobileExports(write(
		"\t// skipped field DeviceLocalSettings.GeneratorFunc with unsupported type: func",
		"\tpublic native void setForwardKeyStore(SidecarForwardKeyStore keyStore);",
	)); err != nil {
		t.Errorf("the gate refused source that omits no forwarding key store entry point: %v", err)
	}
}
