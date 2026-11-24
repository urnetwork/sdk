//go:build !ios_extension && !js

package sdk

// DESIGN-COMPUTE §15.3's accessors on sdk.Sidecar and the areas' shells (CSURF-M11-01): what each answers until its area
// fills its file, that each wrapper is made once, that no compute file reaches the js leg or the VPN component, and
// that every new exported signature is one gomobile binds.

import (
	"errors"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The files CSURF-M11-01 creates or fills: the accessors' and hosts' and each area's shell.
var testComputeSdkFiles = []string{
	"sidecar_compute_host.go", "sidecar_harness.go", "sidecar_compute.go", "sidecar_models.go", "sidecar_enclaves.go",
	"sidecar_ssh.go", "sidecar_agents.go", "sidecar_calendar.go", "sidecar_connections.go", "sidecar_listeners.go",
	"sidecar_disclosures.go", "sidecar_auto_approvals.go",
}

// Each accessor's first call on a shell: the calls that answer an error answer ErrSidecarComputeNotBuilt, never a nil
// with no error, and the wrappers come back made, never nil, on both a bare and a running Sidecar.
func TestComputeAccessorsAnswerNotBuiltUntilFilled(t *testing.T) {
	running := newTestSidecarWorld(t).open(t)
	for _, sidecar := range []*Sidecar{{}, running} {
		harness, err := sidecar.NewHarness("{}")
		if harness != nil || !errors.Is(err, ErrSidecarComputeNotBuilt) {
			t.Errorf("NewHarness: %v, %v", harness, err)
		}
		session, err := sidecar.OpenSsh("laptop.ur.example", "agent", 80, 24, nil)
		if session != nil || !errors.Is(err, ErrSidecarComputeNotBuilt) {
			t.Errorf("OpenSsh: %v, %v", session, err)
		}
		disclosures, err := sidecar.Disclosures()
		if disclosures != "" || !errors.Is(err, ErrSidecarComputeNotBuilt) {
			t.Errorf("Disclosures: %q, %v", disclosures, err)
		}
		if err := sidecar.AckDisclosure("search:parallel"); !errors.Is(err, ErrSidecarComputeNotBuilt) {
			t.Errorf("AckDisclosure: %v", err)
		}
		if err := sidecar.AckNetworkDisclosure("search:parallel"); !errors.Is(err, ErrSidecarComputeNotBuilt) {
			t.Errorf("AckNetworkDisclosure: %v", err)
		}
		for name, made := range map[string]bool{
			"Compute":       sidecar.Compute() != nil,
			"Models":        sidecar.Models() != nil,
			"Enclaves":      sidecar.Enclaves() != nil,
			"Agents":        sidecar.Agents() != nil,
			"Calendar":      sidecar.Calendar() != nil,
			"Connections":   sidecar.Connections() != nil,
			"Listeners":     sidecar.Listeners() != nil,
			"AutoApprovals": sidecar.AutoApprovals() != nil,
		} {
			if !made {
				t.Errorf("%s answered nil", name)
			}
		}
	}
	// the running Sidecar's wrappers see its core
	if compute := running.Compute(); compute.args.local == nil || compute.args.local != running.Core() || compute.args.ctx == nil {
		t.Errorf("the compute wrapper holds %+v", compute.args)
	}
}

// Each accessor answers one wrapper at every call, and concurrent first calls held at a barrier get one: the first to
// take stateLock makes it while the rest wait on the lock or find it made.
func TestComputeWrappersAreMadeOnce(t *testing.T) {
	sidecar := &Sidecar{}
	accessors := map[string]func() any{
		"Compute":       func() any { return sidecar.Compute() },
		"Models":        func() any { return sidecar.Models() },
		"Enclaves":      func() any { return sidecar.Enclaves() },
		"Agents":        func() any { return sidecar.Agents() },
		"Calendar":      func() any { return sidecar.Calendar() },
		"Connections":   func() any { return sidecar.Connections() },
		"Listeners":     func() any { return sidecar.Listeners() },
		"AutoApprovals": func() any { return sidecar.AutoApprovals() },
	}
	for name, accessor := range accessors {
		if first, second := accessor(), accessor(); first != second {
			t.Errorf("%s answered %p, then %p", name, first, second)
		}
	}
	if made := sidecar.computeWrappersWithLock(); len(made) != len(accessors) {
		t.Errorf("%d wrappers made, want %d", len(made), len(accessors))
	}

	held := &Sidecar{}
	const callerCount = 8
	started := sync.WaitGroup{}
	started.Add(callerCount)
	release := make(chan struct{})
	constructionsLock := sync.Mutex{}
	constructions := 0
	construct := func(args *sidecarComputeArgs) *SidecarCompute {
		constructionsLock.Lock()
		constructions += 1
		constructionsLock.Unlock()
		// every caller has started before the first construction finishes
		started.Wait()
		<-release
		return newSidecarCompute(args)
	}
	answers := make(chan *SidecarCompute, callerCount)
	for range callerCount {
		go func() {
			started.Done()
			answers <- sidecarComputeWrapperOf(held, &held.compute, construct)
		}()
	}
	started.Wait()
	close(release)
	first := <-answers
	for range callerCount - 1 {
		if answer := <-answers; answer != first {
			t.Errorf("a concurrent first call got %p, another %p", answer, first)
		}
	}
	constructionsLock.Lock()
	constructionCount := constructions
	constructionsLock.Unlock()
	if first == nil || held.Compute() != first || constructionCount != 1 {
		t.Errorf("%d constructions, wrapper %p, then %p", constructionCount, first, held.Compute())
	}
}

// Every compute file of the sdk carries sidecar.go's constraint, parsed and evaluated: false on the js leg and under
// ios_extension, so sdk.wasm and the VPN component link none of compute (owner decision 197, DESIGN-COMPUTE §15.4),
// and true on the app legs.
func TestComputeShellsStayOffTheJsAndExtensionLegs(t *testing.T) {
	legs := []struct {
		name  string
		tags  map[string]bool
		wants bool
	}{
		{name: "js", tags: map[string]bool{"js": true, "wasm": true}, wants: false},
		{name: "the iOS extension", tags: map[string]bool{"ios": true, "ios_extension": true, "sdk_mobile_bind": true}, wants: false},
		{name: "the macOS extension", tags: map[string]bool{"darwin": true, "ios_extension": true, "sdk_mobile_bind": true}, wants: false},
		{name: "the iOS app", tags: map[string]bool{"ios": true, "sdk_mobile_bind": true}, wants: true},
		{name: "the Android app", tags: map[string]bool{"android": true, "linux": true, "sdk_mobile_bind": true}, wants: true},
		{name: "desktop", tags: map[string]bool{"darwin": true}, wants: true},
	}
	for _, fileName := range append(slices.Clone(testComputeSdkFiles), "sidecar.go") {
		syntax, err := parser.ParseFile(token.NewFileSet(), fileName, nil, parser.ParseComments|parser.PackageClauseOnly)
		if err != nil {
			t.Fatal(err)
		}
		var expression constraint.Expr
		for _, group := range syntax.Comments {
			for _, comment := range group.List {
				if constraint.IsGoBuild(comment.Text) {
					expression, err = constraint.Parse(comment.Text)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		if expression == nil {
			t.Errorf("%s has no build line", fileName)
			continue
		}
		for _, leg := range legs {
			if builds := expression.Eval(func(tag string) bool { return leg.tags[tag] }); builds != leg.wants {
				t.Errorf("%s builds on %s: %v", fileName, leg.name, builds)
			}
		}
	}
}

// The types gomobile binds in a signature: strings, the integers and floats it maps, bool, error, []byte, and this
// package's own exported structs by pointer and interfaces. Anything else, a map, another slice, a func, a channel or
// another package's type, it cannot bind.
func testGomobileSafeType(expression ast.Expr, declared map[string]string) bool {
	switch typed := expression.(type) {
	case *ast.Ident:
		switch typed.Name {
		case "string", "bool", "int", "int8", "int16", "int32", "int64", "float32", "float64", "byte", "rune", "error":
			return true
		}
		return declared[typed.Name] == "interface"
	case *ast.StarExpr:
		identifier, ok := typed.X.(*ast.Ident)
		return ok && declared[identifier.Name] == "struct"
	case *ast.ArrayType:
		element, ok := typed.Elt.(*ast.Ident)
		return typed.Len == nil && ok && element.Name == "byte"
	}
	return false
}

// Each exported signature the compute files add, as gomobile would bind it: the accessors on Sidecar, the hosts'
// and listeners' methods. A type the files do not declare reads as this package's own only if sdk declares it.
func testComputeSignatureFindings(t *testing.T, fileNames []string, extraDeclared map[string]string) []string {
	t.Helper()
	fileSet := token.NewFileSet()
	files := []*ast.File{}
	declared := map[string]string{}
	for name, kind := range extraDeclared {
		declared[name] = kind
	}
	for _, fileName := range fileNames {
		syntax, err := parser.ParseFile(fileSet, fileName, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, syntax)
		for _, decl := range syntax.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				continue
			}
			for _, spec := range genDecl.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				switch typeSpec.Type.(type) {
				case *ast.StructType:
					declared[typeSpec.Name.Name] = "struct"
				case *ast.InterfaceType:
					declared[typeSpec.Name.Name] = "interface"
				}
			}
		}
	}
	findings := []string{}
	for index, syntax := range files {
		checkFields := func(where string, fieldList *ast.FieldList) {
			if fieldList == nil {
				return
			}
			for _, field := range fieldList.List {
				if !testGomobileSafeType(field.Type, declared) {
					findings = append(findings, fileNames[index]+": "+where+" takes or answers a type gomobile cannot bind")
				}
			}
		}
		for _, decl := range syntax.Decls {
			switch typed := decl.(type) {
			case *ast.FuncDecl:
				if !typed.Name.IsExported() {
					continue
				}
				where := typed.Name.Name
				if typed.Recv != nil {
					receiver := typed.Recv.List[0].Type
					if star, ok := receiver.(*ast.StarExpr); ok {
						receiver = star.X
					}
					if identifier, ok := receiver.(*ast.Ident); !ok || !ast.IsExported(identifier.Name) {
						continue
					}
					where = receiver.(*ast.Ident).Name + "." + where
				}
				checkFields(where, typed.Type.Params)
				checkFields(where, typed.Type.Results)
			case *ast.GenDecl:
				for _, spec := range typed.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok || !typeSpec.Name.IsExported() {
						continue
					}
					interfaceType, ok := typeSpec.Type.(*ast.InterfaceType)
					if !ok {
						continue
					}
					for _, method := range interfaceType.Methods.List {
						functionType, ok := method.Type.(*ast.FuncType)
						if !ok || len(method.Names) == 0 {
							findings = append(findings, typeSpec.Name.Name+" embeds an interface")
							continue
						}
						where := typeSpec.Name.Name + "." + method.Names[0].Name
						checkFields(where, functionType.Params)
						checkFields(where, functionType.Results)
					}
				}
			}
		}
	}
	return findings
}

// The sdk's bind check over the new exported signatures: §15.3's accessors as sidecar.go declares them, and the hosts'
// and listeners' methods, each in types gomobile binds.
func TestComputeSurfaceSignaturesAreGomobileSafe(t *testing.T) {
	accessors := []string{
		"NewHarness", "Compute", "Models", "Enclaves", "OpenSsh", "Agents", "Calendar", "Connections", "SetConnectHost",
		"Listeners", "Disclosures", "AckDisclosure", "AckNetworkDisclosure", "SetLocalInferenceHost", "AutoApprovals",
		"SetSafetyClassifierHost",
	}
	sidecarFile, err := parser.ParseFile(token.NewFileSet(), "sidecar.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	found := []string{}
	for _, decl := range sidecarFile.Decls {
		if function, ok := decl.(*ast.FuncDecl); ok && function.Recv != nil && slices.Contains(accessors, function.Name.Name) {
			found = append(found, function.Name.Name)
		}
	}
	slices.Sort(found)
	sortedAccessors := slices.Sorted(slices.Values(accessors))
	if !slices.Equal(found, sortedAccessors) {
		t.Errorf("sidecar.go declares %v of the accessors %v", found, sortedAccessors)
	}
	// Sidecar itself is declared in sidecar.go, whose other calls are no compute surface's
	findings := testComputeSignatureFindings(t, append(slices.Clone(testComputeSdkFiles), "sidecar.go"), nil)
	for _, finding := range findings {
		accessor := slices.ContainsFunc(accessors, func(accessor string) bool {
			return strings.HasPrefix(finding, "sidecar.go: Sidecar."+accessor+" ")
		})
		if accessor || !strings.HasPrefix(finding, "sidecar.go: ") {
			t.Error(finding)
		}
	}
}

// The check sees what gomobile refuses: a map, a func, another package's type and a slice of strings.
func TestComputeSignatureCheckRefusesWhatGomobileCannotBind(t *testing.T) {
	directory := t.TempDir()
	planted := directory + "/planted.go"
	source := `package sdk

type PlantedHost interface {
	Settings() map[string]string
	OnDone(callback func())
	Names() []string
	Fine(text string, handle int64, data []byte) (bool, error)
}

type PlantedWrapper struct{}

func (self *PlantedWrapper) Core() *sidecar.Sidecar { return nil }
`
	if err := os.WriteFile(planted, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	findings := testComputeSignatureFindings(t, []string{planted}, nil)
	for _, want := range []string{"PlantedHost.Settings", "PlantedHost.OnDone", "PlantedHost.Names", "PlantedWrapper.Core"} {
		if !slices.ContainsFunc(findings, func(finding string) bool { return strings.HasPrefix(finding, planted+": "+want+" ") }) {
			t.Errorf("%s was not found: %v", want, findings)
		}
	}
	if slices.ContainsFunc(findings, func(finding string) bool { return strings.Contains(finding, "PlantedHost.Fine ") }) {
		t.Errorf("a bindable signature was refused: %v", findings)
	}
}
