//go:build !ios_extension && !js

package sdk

// DESIGN-COMPUTE §15.3's sdk surface held to the sdk's sources (CSURF-M11-02): every method §15.3 writes, by its
// receiver, name and signature, is declared in this package for the apps' legs, read with go/ast, else it is listed
// unbuilt, so a method two plans each left to the other fails here rather than shipping missing. A declared method is
// held to §15.3's rule, gomobile-safe kinds alone (strings, int64, int, bool, error, this package's listener and host
// interfaces and its own structs by pointer, JSON for structures), and, where §15.3 writes its signature, to those
// kinds, whether or not it is still listed. The unbuilt list is the areas' to shrink: no compute area edits this file
// (CONTRACTS C-29), so a method declared and still listed passes, and the review phase prunes the list. A declared
// signature §15.3 does not write is a finding unless sdkComputeMethodDiffers records it, with where the difference is
// raised.
//
// Signatures are §15.3's, in Go's syntax: where §15.3 writes a JSON answer the method answers (string, error), as its
// written signatures do; `listener` stands for any of this package's listener interfaces, where §15.3 names a listener
// without its type; `_` for any kind gomobile binds, where it names a parameter without its kind; "" where it names a
// method without a signature. RemoteHarnesses() and its methods are §15.3's as CO-48 adds them (CSURF-M14-01, HARN-M14-09),
// and Support() as CO-45 does.

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// One §15.3 method: its receiver, its name, its signature as §15.3 writes it without func, and the task that builds it.
type sdkComputeMethod struct {
	receiver  string
	name      string
	signature string
	owner     string
}

// §15.3's methods, by its listing.
var sdkComputeMethodDesign = []sdkComputeMethod{
	{"Sidecar", "NewHarness", "(settingsJson string) (*SidecarHarness, error)", "CSURF-M11-01"},
	{"SidecarHarness", "NewAgent", "(specJson string) (string, error)", "HARN-M11-10"},
	{"SidecarHarness", "Prompt", "(agentId string, text string) error", "HARN-M11-10"},
	{"SidecarHarness", "Steer", "(agentId string, text string) error", "HARN-M11-10"},
	{"SidecarHarness", "FollowUp", "(agentId string, text string) error", "HARN-M11-10"},
	{"SidecarHarness", "Abort", "(agentId string)", "HARN-M11-10"},
	{"SidecarHarness", "SetModel", "(agentId string, model string) error", "HARN-M11-10"},
	{"SidecarHarness", "Close", "(agentId string) error", "HARN-M11-10"},
	{"SidecarHarness", "Attach", "(harness string, sessionId string) (string, error)", "HARN-M14-03"},
	{"SidecarHarness", "Harnesses", "(optionsJson string) (string, error)", "HARN-M14-04"},
	{"SidecarHarness", "Manage", "(harness string) error", "HARN-M14-03"},
	{"SidecarHarness", "Launch", "(harness string, specJson string) (string, error)", "HARN-M14-03"},
	{"SidecarHarness", "Watch", "(harness string, optionsJson string) error", "HARN-M14-03"},
	{"SidecarHarness", "Unmanage", "(harness string)", "HARN-M14-03"},
	{"SidecarHarness", "AddListener", "(listener SidecarHarnessListener) Sub", "HARN-M11-10"},
	{"SidecarHarnessListener", "OnEvent", "(json string)", "HARN-M11-10"},
	{"SidecarHarness", "RespondUi", "(requestId string, responseJson string) error", "HARN-M11-10"},
	{"SidecarHarness", "Runs", "(filterJson string) (string, error)", "CTXH-M11-06"},
	{"SidecarHarness", "Continue", "(specJson string) (string, error)", "CTXH-M11-05"},

	// the standard view controller every app binds (CO-48): List, AddListener, and Attach and Manage as SidecarHarness's
	{"Sidecar", "RemoteHarnesses", "() *SidecarRemoteHarnesses", "CSURF-M14-01"},
	{"SidecarRemoteHarnesses", "List", "() (string, error)", "HARN-M14-09"},
	{"SidecarRemoteHarnesses", "AddListener", "(listener listener) Sub", "HARN-M14-09"},
	{"SidecarRemoteHarnesses", "Attach", "(harness string, sessionId string) (string, error)", "HARN-M14-09"},
	{"SidecarRemoteHarnesses", "Manage", "(harness string) error", "HARN-M14-09"},

	{"Sidecar", "Compute", "() *SidecarCompute", "CSURF-M11-01"},
	{"SidecarCompute", "Balance", "() (string, error)", "BAL-M12-19"},
	{"SidecarCompute", "Packs", "(market string) (string, error)", "BAL-M12-19"},
	{"SidecarCompute", "Charges", "(json string) (string, error)", "BAL-M12-19"},
	// a url, as text
	{"SidecarCompute", "Checkout", "(pack string) (string, error)", "BAL-M12-19"},
	{"SidecarCompute", "StorePurchase", "(market string, json string) (string, error)", "BAL-M12-19"},
	{"SidecarCompute", "Redeem", "(code string) (string, error)", "BAL-M12-19"},
	{"SidecarCompute", "AutoRenew", "(json string) error", "BAL-M12-19"},
	{"SidecarCompute", "RaiseBudget", "(json string) (string, error)", "BAL-M12-19"},
	{"SidecarCompute", "Spend", "(json string) (string, error)", "BAL-M12-19"},
	{"SidecarCompute", "Terms", "() (string, error)", "BAL-M13-03"},
	{"SidecarCompute", "AcceptTerms", "(version string) error", "BAL-M13-03"},

	{"Sidecar", "Models", "() *SidecarModels", "CSURF-M11-01"},
	{"SidecarModels", "Catalog", "() (string, error)", "INFER-M11-09"},
	{"SidecarModels", "Recommended", "() (string, error)", "INFER-M11-09"},
	{"SidecarModels", "Pull", "(name string, listener listener) error", "INFER-M11-09"},
	{"SidecarModels", "Remove", "(name string) error", "INFER-M11-09"},
	{"SidecarModels", "Settings", "() (string, error)", "INFER-M11-09"},
	{"SidecarModels", "SetSettings", "(json string) error", "INFER-M11-09"},

	{"Sidecar", "Enclaves", "() *SidecarEnclaves", "CSURF-M11-01"},
	{"SidecarEnclaves", "List", "() (string, error)", "ENC-M14-28"},
	{"SidecarEnclaves", "Create", "(json string) (string, error)", "ENC-M14-28"},
	{"SidecarEnclaves", "Stop", "", "ENC-M14-28"},
	{"SidecarEnclaves", "Start", "", "ENC-M14-28"},
	{"SidecarEnclaves", "Delete", "", "ENC-M14-28"},
	{"SidecarEnclaves", "Egress", "(name string, mode string) error", "ENC-M14-28"},
	{"SidecarEnclaves", "Keeper", "(name string, sidecarId string) error", "ENC-M14-28"},
	{"SidecarEnclaves", "GrantMailbox", "(on bool) error", "ENC-M14-28"},
	{"SidecarEnclaves", "Held", "(name string) (string, error)", "ENC-M14-28"},
	{"SidecarEnclaves", "Release", "(name string, refsJson string) error", "ENC-M14-28"},
	// ENC.md reports it, unplanned: §20.4, which it cites, defines no such setting since CO-43 made the agents'
	// enclave its sponsor's device
	{"SidecarEnclaves", "SetAgentsRead", "(name string, summaries bool) error", "none (ENC.md)"},

	{"Sidecar", "OpenSsh", "(host string, user string, cols int, rows int, listener SidecarSshListener) (*SidecarSshSession, error)", "CSURF-M11-01"},

	{"Sidecar", "Agents", "() *SidecarAgents", "CSURF-M11-01"},
	{"SidecarAgents", "List", "() (string, error)", "SKILL-M11-08"},
	{"SidecarAgents", "Send", "(json string) (string, error)", "SKILL-M11-08"},
	{"SidecarAgents", "Broadcast", "(json string) (string, error)", "SKILL-M11-08"},
	{"SidecarAgents", "Inbox", "(json string) (string, error)", "SKILL-M11-08"},
	{"SidecarAgents", "Graph", "(json string) (string, error)", "SKILL-M11-08"},
	{"SidecarAgents", "AddListener", "", "SKILL-M11-08"},

	{"Sidecar", "Calendar", "() *SidecarCalendar", "CSURF-M11-01"},
	{"SidecarCalendar", "Links", "() (string, error)", "MAILCAL-M13-04"},
	{"SidecarCalendar", "SetLink", "(json string) error", "MAILCAL-M13-04"},
	{"SidecarCalendar", "Connect", "(provider string, listener listener) error", "MAILCAL-M13-12"},
	{"SidecarCalendar", "RemoveLink", "(label string) error", "MAILCAL-M13-04"},
	{"SidecarCalendar", "List", "(json string) (string, error)", "MAILCAL-M13-10"},
	{"SidecarCalendar", "Undo", "(entryId string) error", "MAILCAL-M13-10"},
	{"SidecarCalendar", "Reconcile", "(approvalId string, index _, applied bool) error", "MAILCAL-M13-07"},

	{"Sidecar", "Connections", "() *SidecarConnections", "CSURF-M11-01"},
	{"SidecarConnections", "Services", "() (string, error)", "CONN-M13-10"},
	{"SidecarConnections", "List", "() (string, error)", "CONN-M13-10"},
	{"SidecarConnections", "Connect", "(json string, listener listener) error", "CONN-M13-10"},
	{"SidecarConnections", "AddFeature", "(label string, feature string, listener listener) error", "CONN-M13-10"},
	{"SidecarConnections", "Disconnect", "(label string) error", "CONN-M13-10"},
	{"SidecarConnections", "Answer", "(requestId string, json string) error", "CONN-M13-10"},
	{"SidecarConnections", "UseHere", "(label string, listener listener) error", "CONN-M13-10"},
	{"Sidecar", "SetConnectHost", "(host SidecarConnectHost)", "CSURF-M11-01"},
	{"SidecarConnectHost", "Open", "(json string) (string, error)", "CSURF-M11-01"},
	{"SidecarConnectHost", "Authorize", "(json string) (string, error)", "CSURF-M11-01"},

	{"Sidecar", "Listeners", "() *SidecarListeners", "CSURF-M11-01"},
	{"SidecarListeners", "List", "(optionsJson string) (string, error)", "LSTN-M13-14"},
	{"SidecarListeners", "Refresh", "(sidecarId string) error", "LSTN-M13-14"},
	{"SidecarListeners", "Label", "(port _, proto string, text string, serviceType string) error", "LSTN-M13-14"},
	{"SidecarListeners", "Hide", "(port _, proto string, on bool) error", "LSTN-M13-14"},
	{"SidecarListeners", "HideDevice", "(on bool) error", "LSTN-M13-14"},
	{"SidecarListeners", "Map", "(sidecarId string, port _, proto string) error", "LSTN-M14-01"},
	{"SidecarListeners", "AddListener", "", "LSTN-M13-14"},
	{"SidecarListeners", "Confirm", "() error", "LSTN-M13-14"},

	{"Sidecar", "Disclosures", "() (string, error)", "CSURF-M11-01"},
	{"Sidecar", "AckDisclosure", "(id string) error", "CSURF-M11-01"},
	{"Sidecar", "AckNetworkDisclosure", "(id string) error", "CSURF-M11-01"},
	{"Sidecar", "SetLocalInferenceHost", "(host SidecarLocalInferenceHost)", "CSURF-M11-01"},

	{"Sidecar", "AutoApprovals", "() *SidecarAutoApprovals", "CSURF-M11-01"},
	{"SidecarAutoApprovals", "Settings", "() (string, error)", "SAFE-M13-10"},
	{"SidecarAutoApprovals", "SetSettings", "(json string) error", "SAFE-M13-10"},
	{"SidecarAutoApprovals", "Digest", "(dayJson string) (string, error)", "SAFE-M13-11"},

	{"Sidecar", "Support", "() *SidecarSupport", "support-skill"},
	{"SidecarSupport", "Conversation", "(json string) (string, error)", "support-skill"},
	{"SidecarSupport", "Send", "(json string) (string, error)", "support-skill"},
	{"SidecarSupport", "Delete", "(entry string) error", "support-skill"},
	{"SidecarSupport", "Logs", "(json string) (string, error)", "support-skill"},
	{"SidecarSupport", "Settings", "() (string, error)", "support-skill"},
	{"SidecarSupport", "SetSettings", "(json string) error", "support-skill"},
	{"SidecarSupport", "AddListener", "", "support-skill"},

	{"Sidecar", "SetSafetyClassifierHost", "(host SidecarSafetyClassifierHost)", "CSURF-M11-01"},
	{"SidecarSafetyClassifierHost", "Load", "(modelPath string) (int64, error)", "CSURF-M11-01"},
	{"SidecarSafetyClassifierHost", "Classify", "(handle int64, idsJson string) (string, error)", "CSURF-M11-01"},
	{"SidecarSafetyClassifierHost", "Unload", "(handle int64)", "CSURF-M11-01"},
}

// The methods whose owners have not declared them, as receiver.name. Each area's sdk task fills its file; these lines
// are the review phase's to prune (C-29).
var sdkComputeMethodUnbuilt = []string{
	"SidecarHarness.Attach", "SidecarHarness.Harnesses", "SidecarHarness.Manage", "SidecarHarness.Launch",
	"SidecarHarness.Watch", "SidecarHarness.Unmanage",
	"Sidecar.RemoteHarnesses", "SidecarRemoteHarnesses.List", "SidecarRemoteHarnesses.AddListener",
	"SidecarRemoteHarnesses.Attach", "SidecarRemoteHarnesses.Manage",
	"SidecarCompute.Terms", "SidecarCompute.AcceptTerms",
	"SidecarEnclaves.List", "SidecarEnclaves.Create", "SidecarEnclaves.Stop", "SidecarEnclaves.Start",
	"SidecarEnclaves.Delete", "SidecarEnclaves.Egress", "SidecarEnclaves.Keeper", "SidecarEnclaves.GrantMailbox",
	"SidecarEnclaves.Held", "SidecarEnclaves.Release", "SidecarEnclaves.SetAgentsRead",
	"SidecarCalendar.Links", "SidecarCalendar.SetLink", "SidecarCalendar.Connect", "SidecarCalendar.RemoveLink",
	"SidecarCalendar.List", "SidecarCalendar.Undo", "SidecarCalendar.Reconcile",
	"SidecarConnections.Services", "SidecarConnections.List", "SidecarConnections.Connect",
	"SidecarConnections.AddFeature", "SidecarConnections.Disconnect", "SidecarConnections.Answer",
	"SidecarConnections.UseHere",
	"SidecarListeners.List", "SidecarListeners.Refresh", "SidecarListeners.Label", "SidecarListeners.Hide",
	"SidecarListeners.HideDevice", "SidecarListeners.Map", "SidecarListeners.AddListener", "SidecarListeners.Confirm",
	"SidecarAutoApprovals.Settings", "SidecarAutoApprovals.SetSettings", "SidecarAutoApprovals.Digest",
}

// Declared signatures §15.3 does not write, as receiver.name to the declared kinds, each raised where it is decided:
// StorePurchase takes the app's own Api, which holds the network's credential, since that credential never enters the
// Sidecar (I-6); BAL-M12-19's draft left it for the design records, and csurf-m11-02-store-purchase-api asks it.
var sdkComputeMethodDiffers = map[string]string{
	"SidecarCompute.StorePurchase": "(*Api, string, string) (string, error)",
}

// The kinds §15.3's gomobile rule allows: strings, int64, int, bool and error, beside this package's interfaces and
// its own structs by pointer.
var sdkComputeBasicKinds = []string{"string", "int64", "int", "bool", "error"}

// The wildcards a §15.3 signature may hold: any listener interface of this package, and any kind gomobile binds.
const (
	sdkComputeAnyListener = "listener"
	sdkComputeAnyKind     = "_"
)

// What this package declares for the apps' legs: each method by receiver.name, interfaces' included, and each type's
// kind, struct or interface.
type sdkComputeDeclarations struct {
	methods map[string]*ast.FuncType
	types   map[string]string
}

// The contexts of the apps' legs: a file any of them compiles declares for the apps.
func sdkComputeAppContexts() []build.Context {
	contexts := []build.Context{}
	for _, leg := range []struct {
		goos   string
		goarch string
		tags   []string
		cgo    bool
	}{
		{goos: "darwin", goarch: "arm64"},
		{goos: "linux", goarch: "amd64"},
		{goos: "windows", goarch: "amd64"},
		{goos: "ios", goarch: "arm64", tags: []string{"sdk_mobile_bind", "ios"}, cgo: true},
		{goos: "android", goarch: "arm64", tags: []string{"sdk_mobile_bind"}, cgo: true},
	} {
		context := build.Default
		context.GOOS = leg.goos
		context.GOARCH = leg.goarch
		context.BuildTags = leg.tags
		context.CgoEnabled = leg.cgo
		contexts = append(contexts, context)
	}
	return contexts
}

// Reads this package's files that an app leg compiles, tests aside.
func sdkComputeReadPackage(t *testing.T, directory string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	contexts := sdkComputeAppContexts()
	files := []*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		compiled := false
		for _, context := range contexts {
			match, err := context.MatchFile(directory, name)
			if err != nil {
				t.Fatal(err)
			}
			compiled = compiled || match
		}
		if !compiled {
			continue
		}
		syntax, err := parser.ParseFile(token.NewFileSet(), filepath.Join(directory, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, syntax)
	}
	return files
}

// The methods and types files declare.
func sdkComputeDeclared(files []*ast.File) *sdkComputeDeclarations {
	declared := &sdkComputeDeclarations{methods: map[string]*ast.FuncType{}, types: map[string]string{}}
	for _, syntax := range files {
		for _, declaration := range syntax.Decls {
			switch typed := declaration.(type) {
			case *ast.FuncDecl:
				if typed.Recv == nil || len(typed.Recv.List) == 0 {
					continue
				}
				receiver := typed.Recv.List[0].Type
				if star, ok := receiver.(*ast.StarExpr); ok {
					receiver = star.X
				}
				if identifier, ok := receiver.(*ast.Ident); ok {
					key := identifier.Name + "." + typed.Name.Name
					if _, ok := declared.methods[key]; !ok {
						declared.methods[key] = typed.Type
					}
				}
			case *ast.GenDecl:
				for _, spec := range typed.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					switch typeType := typeSpec.Type.(type) {
					case *ast.StructType:
						declared.types[typeSpec.Name.Name] = "struct"
					case *ast.InterfaceType:
						declared.types[typeSpec.Name.Name] = "interface"
						for _, method := range typeType.Methods.List {
							functionType, ok := method.Type.(*ast.FuncType)
							if !ok {
								continue
							}
							for _, name := range method.Names {
								declared.methods[typeSpec.Name.Name+"."+name.Name] = functionType
							}
						}
					}
				}
			}
		}
	}
	return declared
}

// A field list's kinds, one for each name, the field's type as written.
func sdkComputeKinds(fields *ast.FieldList) []string {
	kinds := []string{}
	if fields == nil {
		return kinds
	}
	for _, field := range fields.List {
		kind := types.ExprString(field.Type)
		for range max(len(field.Names), 1) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// A signature's kinds as text: (parameters) (results).
func sdkComputeSignatureText(signature *ast.FuncType) string {
	return "(" + strings.Join(sdkComputeKinds(signature.Params), ", ") + ") (" + strings.Join(sdkComputeKinds(signature.Results), ", ") + ")"
}

// Whether a kind is one §15.3's gomobile rule allows: a basic kind, an interface of this package's, or a pointer to a
// struct of its own.
func (self *sdkComputeDeclarations) gomobileSafe(kind string) bool {
	switch {
	case slices.Contains(sdkComputeBasicKinds, kind):
		return true
	case strings.HasPrefix(kind, "*"):
		return self.types[strings.TrimPrefix(kind, "*")] == "struct"
	}
	return self.types[kind] == "interface"
}

// Whether a declared kind is what §15.3 writes, its wildcards read.
func (self *sdkComputeDeclarations) kindMatches(written string, declared string) bool {
	switch written {
	case sdkComputeAnyListener:
		return strings.HasSuffix(declared, "Listener") && self.types[declared] == "interface"
	case sdkComputeAnyKind:
		return self.gomobileSafe(declared)
	}
	return written == declared
}

// Whether a declared signature has §15.3's written kinds.
func (self *sdkComputeDeclarations) signatureMatches(written *ast.FuncType, declared *ast.FuncType) bool {
	for _, pair := range [][2][]string{
		{sdkComputeKinds(written.Params), sdkComputeKinds(declared.Params)},
		{sdkComputeKinds(written.Results), sdkComputeKinds(declared.Results)},
	} {
		if len(pair[0]) != len(pair[1]) {
			return false
		}
		for index := range pair[0] {
			if !self.kindMatches(pair[0][index], pair[1][index]) {
				return false
			}
		}
	}
	return true
}

// Where the declarations and the tables differ: a method neither declared nor listed unbuilt, a declared one of a
// kind gomobile cannot bind or of other kinds than §15.3 writes and no recorded difference, a listed or recorded entry
// naming no method, and a recorded difference that no longer holds. A method declared and still listed passes (C-29).
func sdkComputeMethodFindings(declared *sdkComputeDeclarations, design []sdkComputeMethod, unbuilt []string, differs map[string]string) []string {
	findings := []string{}
	keys := []string{}
	for _, method := range design {
		key := method.receiver + "." + method.name
		if slices.Contains(keys, key) {
			findings = append(findings, key+" is in the table twice")
		}
		keys = append(keys, key)
		if method.owner == "" {
			findings = append(findings, key+" has no owner")
		}
		signature, isDeclared := declared.methods[key]
		if !isDeclared {
			if !slices.Contains(unbuilt, key) {
				findings = append(findings, key+" ("+method.owner+") is neither declared nor listed unbuilt")
			}
			continue
		}
		declaredText := sdkComputeSignatureText(signature)
		for _, kind := range slices.Concat(sdkComputeKinds(signature.Params), sdkComputeKinds(signature.Results)) {
			if !declared.gomobileSafe(kind) {
				findings = append(findings, key+" takes or answers "+kind+", which §15.3's gomobile rule does not bind")
			}
		}
		if method.signature == "" {
			continue
		}
		written, err := parser.ParseExpr("func" + method.signature)
		if err != nil {
			findings = append(findings, key+"'s signature in the table does not parse: "+err.Error())
			continue
		}
		matches := declared.signatureMatches(written.(*ast.FuncType), signature)
		recorded, isRecorded := differs[key]
		switch {
		case matches && isRecorded:
			findings = append(findings, key+" now has §15.3's kinds, and a difference is still recorded")
		case !matches && !isRecorded:
			findings = append(findings, key+" is declared "+declaredText+", and §15.3 writes "+method.signature)
		case !matches && recorded != declaredText:
			findings = append(findings, key+" is declared "+declaredText+", not as its recorded difference "+recorded)
		}
	}
	for _, key := range unbuilt {
		if !slices.Contains(keys, key) {
			findings = append(findings, key+" is listed unbuilt and is no §15.3 method")
		}
	}
	for key := range differs {
		if !slices.Contains(keys, key) {
			findings = append(findings, key+"'s difference is recorded and it is no §15.3 method")
		} else if _, isDeclared := declared.methods[key]; !isDeclared {
			findings = append(findings, key+"'s difference is recorded and it is not declared")
		}
	}
	slices.Sort(findings)
	return findings
}

// Every §15.3 method is declared for the apps or listed unbuilt, each declared one at §15.3's kinds, against this
// package's own sources.
func TestComputeSdkMethodsAreDeclaredOrUnbuilt(t *testing.T) {
	declared := sdkComputeDeclared(sdkComputeReadPackage(t, "."))
	for _, finding := range sdkComputeMethodFindings(declared, sdkComputeMethodDesign, sdkComputeMethodUnbuilt, sdkComputeMethodDiffers) {
		t.Error(finding)
	}
	for _, key := range sdkComputeMethodUnbuilt {
		if _, isDeclared := declared.methods[key]; isDeclared {
			t.Logf("%s is declared and still listed unbuilt, for the review phase to prune", key)
		}
	}
	for key, recorded := range sdkComputeMethodDiffers {
		t.Logf("%s is declared %s, which §15.3 does not write: raised in csurf-m11-02-store-purchase-api", key, recorded)
	}
}

// Planted declarations: a method of the wrong result kind fails, as does one answering a map, one missing and not
// listed, a listed name that is no method and a recorded difference that no longer holds; a listener wildcard, a kind
// wildcard, a method declared and still listed, an interface's method and a method §15.3 writes no signature for pass.
func TestComputeSdkMethodsFailAPlantedSignature(t *testing.T) {
	source := `package sdk

type Sub interface{ Close() }
type SidecarModelsPullListener interface{ OnProgress(json string) }
type SidecarCompute struct{}
type SidecarModels struct{}
type SidecarCalendar struct{}
type SidecarConnectHost interface {
	Open(requestJson string) (string, error)
}

func (self *SidecarCompute) Balance() string { return "" }
func (self *SidecarCompute) Charges(json string) (map[string]string, error) { return nil, nil }
func (self *SidecarCompute) Redeem(code string) (string, error) { return "", nil }
func (self *SidecarModels) Pull(name string, listener SidecarModelsPullListener) error { return nil }
func (self *SidecarModels) Remove(name string) error { return nil }
func (self *SidecarCalendar) Reconcile(approvalId string, index int64, applied bool) error { return nil }
func (self *SidecarCalendar) Undo(entryId string) error { return nil }
`
	syntax, err := parser.ParseFile(token.NewFileSet(), "planted.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	declared := sdkComputeDeclared([]*ast.File{syntax})
	design := []sdkComputeMethod{
		{"SidecarCompute", "Balance", "() (string, error)", "PLANTED-1"},
		{"SidecarCompute", "Charges", "", "PLANTED-2"},
		{"SidecarCompute", "Redeem", "(code string) (string, error)", "PLANTED-3"},
		{"SidecarCompute", "Spend", "(json string) (string, error)", "PLANTED-4"},
		{"SidecarModels", "Pull", "(name string, listener listener) error", "PLANTED-5"},
		{"SidecarModels", "Remove", "", "PLANTED-6"},
		{"SidecarCalendar", "Reconcile", "(approvalId string, index _, applied bool) error", "PLANTED-7"},
		{"SidecarCalendar", "Undo", "(entryId string) error", "PLANTED-8"},
		{"SidecarConnectHost", "Open", "(json string) (string, error)", "PLANTED-9"},
	}
	unbuilt := []string{"SidecarCalendar.Undo", "SidecarCalendar.Links"}
	differs := map[string]string{"SidecarCompute.Redeem": "(string) (string, error)"}
	wants := []string{
		"SidecarCalendar.Links is listed unbuilt and is no §15.3 method",
		"SidecarCompute.Balance is declared () (string), and §15.3 writes () (string, error)",
		"SidecarCompute.Charges takes or answers map[string]string, which §15.3's gomobile rule does not bind",
		"SidecarCompute.Redeem now has §15.3's kinds, and a difference is still recorded",
		"SidecarCompute.Spend (PLANTED-4) is neither declared nor listed unbuilt",
	}
	slices.Sort(wants)
	if findings := sdkComputeMethodFindings(declared, design, unbuilt, differs); !slices.Equal(findings, wants) {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(findings, "\n"), strings.Join(wants, "\n"))
	}
	// a difference recorded as declared passes; recorded otherwise, it fails
	design = []sdkComputeMethod{{"SidecarCompute", "Balance", "() (string, error)", "PLANTED-1"}}
	if findings := sdkComputeMethodFindings(declared, design, nil, map[string]string{"SidecarCompute.Balance": "() (string)"}); len(findings) != 0 {
		t.Errorf("a recorded difference: %v", findings)
	}
	findings := sdkComputeMethodFindings(declared, design, nil, map[string]string{"SidecarCompute.Balance": "() (string, error)"})
	if len(findings) != 1 || !strings.Contains(findings[0], "not as its recorded difference") {
		t.Errorf("a stale recorded difference: %v", findings)
	}
}
