package main

// The app's sidecar role's clock check across the C ABI (owner decision 803, QD-1427; sidecar §18.5's API list):
// Sidecar.CheckClock crosses as urnet_sidecar_check_clock, a call on the Sidecar's handle that answers nothing, as the
// VPN component's DeviceLocal.CheckClock crosses as urnet_device_local_check_clock, and every generated file and each
// of the five language bindings carries it.

import (
	"strings"
	"testing"
)

// The C declarations the two checks cross as.
const (
	sidecarCheckClockDeclaration     = "void urnet_sidecar_check_clock(uint64_t self);"
	deviceLocalCheckClockDeclaration = "void urnet_device_local_check_clock(uint64_t self);"
)

// The generator binds each check as a call on its handle that takes and answers nothing: the export resolves the
// handle as the receiver's type and calls the method through.
func TestSidecarCheckClockBindsAsAHandleCall(t *testing.T) {
	g := testingPreferenceGenerator(t)
	for _, c := range []struct {
		receiver    string
		declaration string
	}{
		{receiver: "Sidecar", declaration: sidecarCheckClockDeclaration},
		{receiver: "DeviceLocal", declaration: deviceLocalCheckClockDeclaration},
	} {
		g.emitType(testingPreferenceType(t, g, c.receiver))
		check := testingPreferenceExport(t, g, c.receiver, "CheckClock")
		if check.cDecl != c.declaration {
			t.Errorf("%s.CheckClock binds as %q", c.receiver, check.cDecl)
		}
		if check.sig.result != nil || check.sig.hasError || len(check.sig.params) != 0 {
			t.Errorf("%s.CheckClock takes or answers something across the abi", c.receiver)
		}
		if !strings.Contains(check.goCode, "resolveHandle[*sdk."+c.receiver+"](uint64(self), ") ||
			!strings.Contains(check.goCode, "\tself_.CheckClock()\n") {
			t.Errorf("%s.CheckClock's export does not call through its handle:\n%s", c.receiver, check.goCode)
		}
	}
}

// The committed files carry the Sidecar's check: the header declares it, the def names it, the c++ wrapper's Sidecar
// calls it on its handle, the coverage report exports it from Sidecar.CheckClock, and each language binding binds it
// as a call taking the handle and answering nothing.
func TestSidecarCheckClockIsInEveryGeneratedFile(t *testing.T) {
	header := testingGeneratedFile(t, "..", "include", "urnetwork_sdk.h")
	if !strings.Contains(header, "\n"+sidecarCheckClockDeclaration+"\n") {
		t.Error("the header does not declare urnet_sidecar_check_clock")
	}
	definition := "\n" + strings.ReplaceAll(testingGeneratedFile(t, "..", "include", "urnetwork_sdk.def"), "\r\n", "\n") + "\n"
	if !strings.Contains(definition, "\n\turnet_sidecar_check_clock\n") {
		t.Error("the def does not name urnet_sidecar_check_clock")
	}
	cpp := testingGeneratedFile(t, "..", "include", "urnetwork_sdk.hpp")
	if !strings.Contains(cpp, "inline void Sidecar::checkClock() const {\n\turnet_sidecar_check_clock(handle());\n}\n") {
		t.Error("the c++ wrapper's Sidecar does not call urnet_sidecar_check_clock on its handle")
	}
	exported, _ := testingCoverageReport(t)
	if cName := exported["Sidecar.CheckClock"]; cName != "urnet_sidecar_check_clock" {
		t.Errorf("the coverage report exports Sidecar.CheckClock as %q", cName)
	}
	for _, binding := range []struct {
		path []string
		line string
	}{
		{path: []string{"..", "..", "python", "src", "urnetwork", "_raw.py"}, line: "lib.urnet_sidecar_check_clock.argtypes = [c_uint64]"},
		{path: []string{"..", "..", "python", "src", "urnetwork", "_raw.py"}, line: "lib.urnet_sidecar_check_clock.restype = None"},
		{path: []string{"..", "..", "java", "src", "main", "java", "io", "ur", "sdk", "Raw.java"}, line: "void urnet_sidecar_check_clock(long a0);"},
		{path: []string{"..", "..", "csharp", "Raw.g.cs"}, line: "public static extern void urnet_sidecar_check_clock(ulong a0);"},
		{path: []string{"..", "..", "rust", "src", "raw.rs"}, line: "pub urnet_sidecar_check_clock: unsafe extern \"C\" fn(u64) -> (),"},
		{path: []string{"..", "..", "ruby", "lib", "urnetwork", "raw.rb"}, line: "attach_function :urnet_sidecar_check_clock, [:uint64], :void"},
	} {
		if !strings.Contains(testingGeneratedFile(t, binding.path...), binding.line) {
			t.Errorf("%s does not bind %s", strings.Join(binding.path[2:], "/"), binding.line)
		}
	}
}
