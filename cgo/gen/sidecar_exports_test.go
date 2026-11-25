package main

// sdk.Sidecar's C ABI (SDK-M8-06; sidecar §18.5's Gate table, R-SDK-5, D-SDK-2, D-SDK-15, D-SDK-30): its seven handle
// types cross as opaque handles with every exported method but the Go-only ones, which are skipped by name; its data
// types cross as json, its lists as json arrays; its listeners and the app's interfaces as function pointers; and
// SidecarSettings decodes over DefaultSidecarSettings. M10's SidecarSearch and SidecarSearchHandle (SDK-M10-01) have their
// rows in sidecarHandleTypes, and a later handle adds its own, this test failing until behavioralTypes has it.

import (
	"encoding/json"
	"go/types"
	"strings"
	"testing"

	"github.com/urnetwork/sdk"
)

// The handles §18.5's Gate table names that the sdk declares now.
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
}

// The Go-only members, each skipped by name with its reason.
var sidecarGoOnlyMethods = []string{
	"Sidecar.Core",
	"Sidecar.CloseAndWait",
	"SidecarSockets.ListenContext",
	"DeviceLocal.NewBridgeLink",
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
	for _, qualified := range sidecarGoOnlyMethods {
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
	} {
		encoded, err := json.Marshal(list)
		if err != nil || !strings.HasPrefix(string(encoded), "[") {
			t.Errorf("%s is no json array: %s %v", name, encoded, err)
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
	} {
		if _, ok := exported[identifier]; !ok {
			t.Errorf("%s is not exported", identifier)
		}
	}
	for _, identifier := range sidecarGoOnlyMethods {
		if !accounted[identifier] {
			t.Errorf("%s is not in the report", identifier)
		}
		if _, ok := exported[identifier]; ok {
			t.Errorf("%s is exported", identifier)
		}
	}
}
