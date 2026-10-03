package main

import (
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The notice carries a private API owner and generation. JSON erases both and
// cannot implement its guarded close; the callback must transfer the actual
// object through the same owned-handle contract as other private observations.
func TestClientRefreshIntegrityBindingPreservesNoticeOwnership(t *testing.T) {
	g := testingPreferenceGenerator(t)
	output := t.TempDir()
	t.Chdir(output)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	listener, ok := testingPreferenceType(t, g, "ClientRefreshIntegrityListener").Type().(*types.Named)
	if !ok {
		t.Fatal("integrity listener is not a named SDK interface")
	}
	callback, err := g.callback(listener)
	if err != nil {
		t.Fatal(err)
	}
	if len(callback.methods) != 1 || callback.methods[0].name != "ClientRefreshInvalid" {
		t.Fatal("integrity listener shape changed")
	}
	method := callback.methods[0]
	if len(method.params) != 1 || method.params[0].info.kind != kindHandle ||
		method.params[0].info.named.Obj().Name() != "ClientRefreshIntegrityNotice" ||
		!strings.Contains(method.typedef, "uint64_t notice") ||
		!strings.Contains(method.adapter, "newHandle(notice)") ||
		strings.Contains(method.adapter, "cJson(notice") {
		t.Fatal("integrity callback serialized away its private owner and guarded close")
	}
	close := testingPreferenceExport(t, g, "ClientRefreshIntegrityNotice", "CloseApiIfCurrent")
	if close.cDecl != "bool urnet_client_refresh_integrity_notice_close_api_if_current(uint64_t self);" ||
		!strings.Contains(close.goCode, "resolveHandle[*sdk.ClientRefreshIntegrityNotice]") ||
		!strings.Contains(close.goCode, "self_.CloseApiIfCurrent()") {
		t.Fatal("integrity action no longer operates on the original notice")
	}
	read := func(name string) string {
		t.Helper()
		value, err := os.ReadFile(filepath.Join(output, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(value)
	}
	if !strings.Contains(read("include/urnetwork_sdk.h"), close.cDecl) ||
		!strings.Contains(read("include/urnetwork_sdk.def"), "\n\turnet_client_refresh_integrity_notice_close_api_if_current\n") {
		t.Fatal("generated C surface omitted the guarded notice action")
	}
	cpp := read("include/urnetwork_sdk.hpp")
	for _, declaration := range []string{
		"class ClientRefreshIntegrityNotice final : public detail::Handle",
		"using ClientRefreshIntegrityListener = std::function<void(ClientRefreshIntegrityNotice notice)>;",
		"bool closeApiIfCurrent() const;",
		"urnet_client_refresh_integrity_notice_close_api_if_current(handle())",
	} {
		if !strings.Contains(cpp, declaration) {
			t.Fatalf("generated C++ lost notice ownership: %s", declaration)
		}
	}
	if strings.Contains(cpp, "struct ClientRefreshIntegrityNotice {") ||
		strings.Contains(read("callbacks.h"), "notice_json") {
		t.Fatal("generated callback still reduces a private notice to JSON")
	}
}
