package main

// sdk.Sidecar's C ABI (SDK-M8-06; sidecar §18.5's Gate table, R-SDK-5, D-SDK-2, D-SDK-15, D-SDK-30): its handle types
// cross as opaque handles with every exported method but the Go-only ones, which are skipped by name; its data types
// cross as json, its lists as json arrays; its listeners and the app's interfaces as function pointers; and
// SidecarSettings decodes over DefaultSidecarSettings. M10's SidecarSearch and SidecarSearchHandle (SDK-M10-01), the
// desktop's SidecarDiscovery and DESIGN-COMPUTE §15.3's wrappers have their rows in sidecarHandleTypes, and a later
// handle adds its own, this test failing until behavioralTypes has it; a Sidecar type with no json shape fails
// TestNoSidecarTypeCrossesAsAnEmptyJsonShape before that. The app's compute hosts are skipped by name, with the reason:
// the desktop apps this abi serves run those engines in ur daemon (DESIGN-COMPUTE §6.5, §25.5, §27.5).

import (
	"encoding/json"
	"go/types"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sdk"
)

// The handles §18.5's Gate table names, and DESIGN-COMPUTE §15.3's, that the sdk declares now.
var sidecarHandleTypes = []string{
	"Sidecar",
	"SidecarEvs",
	"SidecarMail",
	"SidecarSockets",
	"SidecarSocketListener",
	"SidecarApprovals",
	"SidecarDirectory",
	"SidecarSearch",
	"SidecarSearchHandle",
	// a desktop app's view of service discovery (§17.7.1, owner decision 778)
	"SidecarDiscovery",
	// DESIGN-COMPUTE §15.3's (CSURF-M11-01) and the support conversation's (§28.4)
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

// The Go-only members, each skipped by name with its reason.
var sidecarGoOnlyMethods = []string{
	"Sidecar.Core",
	"Sidecar.CloseAndWait",
	"SidecarSockets.ListenContext",
	"DeviceLocal.NewBridgeLink",
}

// The app's compute hosts' setters (DESIGN-COMPUTE §15.3), each skipped by name with its reason, and the hosts'
// interfaces, which no export takes.
var sidecarAppHostMethods = []string{
	"Sidecar.SetConnectHost",
	"Sidecar.SetLocalInferenceHost",
	"Sidecar.SetSafetyClassifierHost",
}

var sidecarAppHostTypes = []string{
	"SidecarConnectHost",
	"SidecarLocalInferenceHost",
	"SidecarLocalInferenceListener",
	"SidecarSafetyClassifierHost",
}

// Every handle crosses as a handle and emits each of its exported methods but the Go-only ones, which the generator
// skips by name, never as unmappable: a new method that does not map fails here rather than vanishing.
func TestSidecarHandlesCrossWithEveryMethod(t *testing.T) {
	g := testingPreferenceGenerator(t)
	for _, name := range sidecarHandleTypes {
		object := testingPreferenceType(t, g, name)
		if !behavioralTypes[name] {
			t.Errorf("%s is not a handle", name)
			continue
		}
		if info := g.classify(types.NewPointer(object.Type())); info.kind != kindHandle {
			t.Errorf("%s crosses as kind %d", name, info.kind)
		}
		g.emitType(object)
	}
	for _, qualified := range append(slices.Clone(sidecarGoOnlyMethods), sidecarAppHostMethods...) {
		if skipMethods[qualified] == "" {
			t.Errorf("%s is not skipped by name", qualified)
		}
	}
	for _, name := range sidecarHandleTypes {
		methods := types.NewMethodSet(types.NewPointer(testingPreferenceType(t, g, name).Type()))
		for i := 0; i < methods.Len(); i += 1 {
			method := methods.At(i).Obj()
			if !method.Exported() || skipMethods[name+"."+method.Name()] != "" {
				continue
			}
			_ = testingPreferenceExport(t, g, name, method.Name())
		}
	}
	for _, skipped := range g.skipped {
		if strings.HasPrefix(skipped.symbol, "Sidecar") && strings.HasPrefix(skipped.reason, "unmappable") {
			t.Errorf("%s does not map: %s", skipped.symbol, skipped.reason)
		}
	}
}

// §18.5's data types cross as json, its lists as json arrays, and the constructors and the device revocation calls
// are exported: NewSidecarLocal and NewSidecarRemote with their settings decoded over DefaultSidecarSettings.
func TestSidecarDataCrossesAsJson(t *testing.T) {
	g := testingPreferenceGenerator(t)
	for _, name := range []string{
		"SidecarSettings", "SidecarResult", "SidecarSecret", "SidecarEvent", "SidecarDevice", "SidecarDeviceList",
		"SidecarRevokePlan", "SidecarRevokeItem", "SidecarRevokeItemList", "SidecarApprovalItem", "SidecarApprovalList",
		"SidecarCleanupList", "SidecarAccessRequest",
		// SidecarMail's send capability (SDK-M9-01)
		"SidecarSendCheck", "SidecarSendResult", "SidecarSendDomain", "SidecarSendDomainList",
		// SidecarSearch's (SDK-M10-01), and its indexer list (SDK-M10-03)
		"SidecarSearchQuery", "SidecarSearchHit", "SidecarSearchHitList", "SidecarSearchProgress", "SidecarSearchStatus", "SidecarIndexer",
		"SidecarIndexerList",
		// SidecarEvs's format 2 answers (SDK-M10-05): a version's row, the dry run and its lineages, and the drops
		"SidecarBytes", "SidecarHistory", "SidecarHistoryVersion", "SidecarHistoryVersionList",
		"SidecarDropPreview", "SidecarDropLineage", "SidecarDropLineageList", "SidecarDrop", "SidecarDropList", "SidecarDrops",
		// the authorization features' prompt (CACL-M11-02, CACL-M11-03)
		"SidecarAuthFeature", "SidecarAuthFeatures",
	} {
		if info := g.classify(types.NewPointer(testingPreferenceType(t, g, name).Type())); info.kind != kindJson {
			t.Errorf("%s crosses as kind %d", name, info.kind)
		}
	}
	for name, list := range map[string]any{
		"SidecarDeviceList":     sdk.NewSidecarDeviceList(),
		"SidecarRevokeItemList": sdk.NewSidecarRevokeItemList(),
		"SidecarApprovalList":   sdk.NewSidecarApprovalList(),
		"SidecarSendDomainList": sdk.NewSidecarSendDomainList(),
		"SidecarSearchHitList":  sdk.NewSidecarSearchHitList(),
		"SidecarIndexerList":    sdk.NewSidecarIndexerList(),
		// SDK-M10-05's
		"SidecarDropLineageList":    sdk.NewSidecarDropLineageList(),
		"SidecarDropList":           sdk.NewSidecarDropList(),
		"SidecarHistoryVersionList": sdk.NewSidecarHistoryVersionList(),
		// a list whose name lacks the List suffix
		"SidecarAuthFeatures": sdk.NewSidecarAuthFeatures(),
	} {
		encoded, err := json.Marshal(list)
		if err != nil || !strings.HasPrefix(string(encoded), "[") {
			t.Errorf("%s is no json array: %s %v", name, encoded, err)
		}
	}
	// a list constructor is skipped, by the pattern or, where the name lacks the List suffix, by name
	g.emitFunc(g.scope.Lookup("NewSidecarAuthFeatures").(*types.Func))
	for _, export := range g.exports {
		if export.sig.goName == "NewSidecarAuthFeatures" {
			t.Error("NewSidecarAuthFeatures, a list constructor, is exported")
		}
	}
	if jsonParamDefaults["SidecarSettings"] != "DefaultSidecarSettings" {
		t.Error("SidecarSettings does not decode over its defaults")
	}
	for _, name := range []string{"NewSidecarLocal", "NewSidecarRemote", "DefaultSidecarSettings"} {
		g.emitFunc(g.scope.Lookup(name).(*types.Func))
	}
	for _, export := range g.exports {
		if export.sig.goName == "NewSidecarLocal" || export.sig.goName == "NewSidecarRemote" {
			if !strings.Contains(export.goCode, "settings_ := sdk.DefaultSidecarSettings()") {
				t.Errorf("%s does not decode its settings over their defaults", export.sig.goName)
			}
		}
	}
	g.emitType(testingPreferenceType(t, g, "SidecarApprovals"))
	for _, method := range []string{"GetPending", "Decide", "GetDevices", "PreviewRevokeDevice", "RevokeDevice", "AddPendingListener"} {
		_ = testingPreferenceExport(t, g, "SidecarApprovals", method)
	}
}

// The app's interfaces and every listener and callback cross as function pointers, the key store's statuses and its
// secret among them (D-SDK-15).
func TestSidecarInterfacesCrossAsFunctionPointers(t *testing.T) {
	g := testingPreferenceGenerator(t)
	for _, name := range []string{
		"SidecarKeyStore", "SidecarPresenceSigner", "SidecarEventListener", "SidecarApprovalListener",
		"SidecarResultCallback", "SidecarRevokePlanCallback", "SidecarForwardKeyStore", "SidecarDirectoryListener",
		"SidecarSendCheckCallback", "SidecarSendResultCallback", "SidecarSearchListener",
		// SidecarEvs's reads and drops (SDK-M10-05)
		"SidecarBytesCallback", "SidecarHistoryCallback", "SidecarDropPreviewCallback", "SidecarDropsCallback",
		// DESIGN-COMPUTE §15.3's listeners: a Harness's events, a terminal session's and the support conversation's
		"SidecarHarnessListener", "SidecarSshListener", "SidecarSupportListener",
	} {
		object := testingPreferenceType(t, g, name)
		if info := g.classify(object.Type()); info.kind != kindCallback {
			t.Errorf("%s crosses as kind %d", name, info.kind)
			continue
		}
		if _, err := g.callback(object.Type().(*types.Named)); err != nil {
			t.Errorf("%s has no function pointers: %v", name, err)
		}
	}
}

// The app's compute hosts do not cross: each setter is skipped by name with its reason, never as unmappable, and each
// host's interface is skipped too, so no export takes one and the report does not say it crosses.
func TestSidecarAppHostsAreSkippedByName(t *testing.T) {
	g := testingPreferenceGenerator(t)
	g.emitType(testingPreferenceType(t, g, "Sidecar"))
	for _, qualified := range sidecarAppHostMethods {
		reason := skipMethods[qualified]
		if !strings.Contains(reason, "DESIGN-COMPUTE §") {
			t.Errorf("%s is not skipped by name with the design's reason: %q", qualified, reason)
		}
		found := false
		for _, skipped := range g.skipped {
			if skipped.symbol == qualified {
				found = true
				if skipped.reason != "skipped: "+reason {
					t.Errorf("%s is skipped as %q", qualified, skipped.reason)
				}
			}
		}
		if !found {
			t.Errorf("%s is not in the generator's skips", qualified)
		}
		_, method, _ := strings.Cut(qualified, ".")
		for _, export := range g.exports {
			if export.sig.recvName == "Sidecar" && export.sig.goName == method {
				t.Errorf("%s is exported", qualified)
			}
		}
	}
	for _, name := range sidecarAppHostTypes {
		if skipTypes[name] == "" {
			t.Errorf("%s is not skipped by name", name)
		}
		if info := g.classify(testingPreferenceType(t, g, name).Type()); info.kind != kindBad {
			t.Errorf("%s crosses as kind %d", name, info.kind)
		}
	}
}

// No Sidecar type crosses as json with no json shape: a struct whose fields are all unexported is behavioral, so it is a
// handle or skipped by name. Every such wrapper DESIGN-COMPUTE §15.3 added crossed as "{}" until it was a handle, its
// accessors answering json that held nothing, and a later one fails here first.
func TestNoSidecarTypeCrossesAsAnEmptyJsonShape(t *testing.T) {
	g := testingPreferenceGenerator(t)
	for _, name := range g.scope.Names() {
		object, ok := g.scope.Lookup(name).(*types.TypeName)
		if !ok || !object.Exported() || !strings.HasPrefix(name, "Sidecar") {
			continue
		}
		structure, ok := object.Type().Underlying().(*types.Struct)
		if !ok || structure.NumFields() == 0 {
			continue
		}
		shaped := false
		for i := 0; i < structure.NumFields(); i += 1 {
			if structure.Field(i).Exported() || structure.Field(i).Embedded() {
				shaped = true
			}
		}
		if shaped {
			continue
		}
		if _, skipped := g.typeSkipReason(name); !behavioralTypes[name] && !skipped {
			t.Errorf("%s has no json shape and is not a handle", name)
		}
	}
	output := t.TempDir()
	t.Chdir(output)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	for _, skipped := range g.skipped {
		if strings.HasPrefix(skipped.symbol, "Sidecar") && strings.Contains(skipped.reason, "empty json shape") {
			t.Errorf("%s: %s", skipped.symbol, skipped.reason)
		}
	}
}

// Nothing of sdk.Sidecar waits for this task any more: no symbol in the coverage report is skipped as pending, and
// every exported handle method or constructor is in it, exported or skipped by name.
func TestNoSidecarSymbolIsPending(t *testing.T) {
	exported, accounted := testingCoverageReport(t)
	report := testingGeneratedFile(t, "..", "coverage_report.txt")
	if strings.Contains(report, "SDK-M8-06") {
		t.Error("the coverage report still names a symbol pending SDK-M8-06")
	}
	for _, identifier := range []string{
		"NewSidecarLocal", "NewSidecarRemote", "Sidecar.GetApprovals", "Sidecar.ConfirmForwardingSetup", "Sidecar.SetForwardingDevice",
		"SidecarApprovals.RevokeDevice", "SidecarApprovals.PreviewRevokeDevice", "SidecarApprovals.Decide",
		"SidecarMail.CheckSend", "SidecarMail.GetSendResult",
		"Sidecar.GetSearch", "SidecarSearch.Search", "SidecarSearch.GetStatus", "SidecarSearch.DropCache", "SidecarSearch.SetPolicy",
		"SidecarSearch.SetConditions", "SidecarSearch.SetForeground", "SidecarSearch.RunBackgroundSync", "SidecarSearchHandle.More",
		"SidecarSearchHandle.Cancel",
		// remote search's requester seams and the directory's repo id (SDK-M10-03, QD-1087)
		"SidecarSearch.GetIndexers", "SidecarSearch.SetRemote", "SidecarSearch.AcknowledgeIndexer", "SidecarDirectory.GetRepoId",
		// format 2's reads and the drop calls (SDK-M10-05)
		"SidecarEvs.ReadVersion", "SidecarEvs.Write", "SidecarEvs.History", "SidecarEvs.DropHistoryPreview", "SidecarEvs.DropHistory",
		"SidecarEvs.CancelDrop", "SidecarEvs.Drops",
		// a listen's grant lifetime and its end (owner decision 687, QD-1419)
		"SidecarSockets.ListenExpires", "SidecarSocketListener.GetGrantExpiresMillis",
		// the authorization features' prompt (CACL-M11-03) and a desktop app's discovery (owner decision 778)
		"Sidecar.GetAuthFeatures", "Sidecar.TurnOnAuthFeature", "Sidecar.GetDiscovery", "SidecarDiscovery.List",
		"SidecarDiscovery.Share", "SidecarDiscovery.Set",
		// DESIGN-COMPUTE §15.3's surface (CSURF-M11-01): its disclosures, its wrappers and their methods so far
		"Sidecar.Disclosures", "Sidecar.AckDisclosure", "Sidecar.AckNetworkDisclosure", "Sidecar.NewHarness",
		"Sidecar.Compute", "Sidecar.Models", "Sidecar.Enclaves", "Sidecar.OpenSsh", "Sidecar.Agents", "Sidecar.Calendar",
		"Sidecar.Connections", "Sidecar.Listeners", "Sidecar.AutoApprovals", "Sidecar.Support",
		"SidecarCompute.Balance", "SidecarCompute.Packs", "SidecarCompute.Charges", "SidecarCompute.Checkout",
		"SidecarCompute.StorePurchase", "SidecarCompute.Redeem", "SidecarCompute.AutoRenew", "SidecarCompute.RaiseBudget",
		"SidecarCompute.Spend",
		"SidecarSupport.Conversation", "SidecarSupport.Send", "SidecarSupport.Delete", "SidecarSupport.Logs",
		"SidecarSupport.Settings", "SidecarSupport.SetSettings", "SidecarSupport.AddListener",
	} {
		if _, ok := exported[identifier]; !ok {
			t.Errorf("%s is not exported", identifier)
		}
	}
	// the committed report holds every handle method the sdk declares, exported or skipped by name
	g := testingPreferenceGenerator(t)
	for _, name := range sidecarHandleTypes {
		methods := types.NewMethodSet(types.NewPointer(testingPreferenceType(t, g, name).Type()))
		for i := 0; i < methods.Len(); i += 1 {
			method := methods.At(i).Obj()
			qualified := name + "." + method.Name()
			if !method.Exported() || skipMethods[qualified] != "" {
				continue
			}
			if _, ok := exported[qualified]; !ok {
				t.Errorf("%s is not exported", qualified)
			}
		}
	}
	for _, identifier := range slices.Concat(sidecarGoOnlyMethods, sidecarAppHostMethods, sidecarAppHostTypes,
		[]string{"NewSidecarAuthFeatures"}) {
		if !accounted[identifier] {
			t.Errorf("%s is not in the report", identifier)
		}
		if _, ok := exported[identifier]; ok {
			t.Errorf("%s is exported", identifier)
		}
	}
	for _, name := range sidecarAppHostTypes {
		if !strings.Contains(report, "\n"+name+": skipped: ") {
			t.Errorf("the report does not skip %s by name", name)
		}
	}
}
