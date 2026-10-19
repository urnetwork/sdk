package main

import (
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Run the actual generator: origin and cause must cross the same callback,
// and the application must be able to validate that origin on the same API.
func TestAuthLogoutEventBindingPreservesOriginAndCause(t *testing.T) {
	g := testingPreferenceGenerator(t)
	output := t.TempDir()
	t.Chdir(output)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	listener := testingPreferenceType(t, g, "AuthLogoutEventListener").Type().(*types.Named)
	callback, err := g.callback(listener)
	if err != nil {
		t.Fatal(err)
	}
	if len(callback.methods) != 1 || len(callback.methods[0].params) != 2 {
		t.Fatal("logout event lost its origin or cause")
	}
	method := callback.methods[0]
	if method.typedef != "typedef void (*urnet_auth_logout_event_cb)(void* user_data, int64_t generation, const char* cause);" ||
		!strings.Contains(method.adapter, "C.int64_t(int64(generation)), cause_") {
		t.Fatal("logout event callback does not pass its immutable values")
	}
	current := testingPreferenceExport(t, g, "Api", "IsAuthLogoutCurrent")
	if current.cDecl != "bool urnet_api_is_auth_logout_current(uint64_t self, int64_t generation);" ||
		!strings.Contains(current.goCode, "self_.IsAuthLogoutCurrent(int64(generation))") {
		t.Fatal("logout currentness check lost its originating API or generation")
	}
	cpp, err := os.ReadFile(filepath.Join(output, "include", "urnetwork_sdk.hpp"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"using AuthLogoutEventListener = std::function<void(int64_t generation, std::string cause)>;",
		"Sub addAuthLogoutEventListener(AuthLogoutEventListener listener) const;",
		"bool isAuthLogoutCurrent(int64_t generation) const;",
		"inline void retained_auth_logout_event(void* user_data, int64_t generation, const char* cause)",
		"urnet_api_is_auth_logout_current(handle(), generation)",
	} {
		if !strings.Contains(string(cpp), expected) {
			t.Fatalf("generated C++ lost event origin: %s", expected)
		}
	}
}
