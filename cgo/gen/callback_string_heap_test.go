package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A string a callback returns is freed by the library (urnet_free_string, the
// library's own free). windows/urnetwork#17: the generated wrapper allocated it
// with std::malloc, inlined into the host, so a host and library linked to
// different C runtimes freed a block on a heap that never allocated it, and the
// service died in RtlFreeHeap. The buffer must come from the library itself.

// The generated trampolines and the c abi must agree on one allocator.
func TestCallbackStringResultIsAllocatedByTheLibrary(t *testing.T) {
	outputDirectory := testingCallbackStringGenerate(t)
	header := testingCallbackStringRead(t, outputDirectory, "include", "urnetwork_sdk.h")
	definition := testingCallbackStringRead(t, outputDirectory, "include", "urnetwork_sdk.def")
	wrapper := testingCallbackStringRead(t, outputDirectory, "include", "urnetwork_sdk.hpp")

	if !strings.Contains(header, "\nchar* urnet_copy_string(const char* s);\n") {
		t.Error("urnetwork_sdk.h does not declare urnet_copy_string, the library allocator a callback's string result must come from")
	}
	if !strings.Contains(strings.ReplaceAll(definition, "\r\n", "\n"), "\n\turnet_copy_string\n") {
		t.Error("urnetwork_sdk.def does not export urnet_copy_string")
	}

	dupCString := regexp.MustCompile(`(?s)inline char\* dupCString\(const std::string& s\) \{.*?\n\}\n`).FindString(wrapper)
	if dupCString == "" {
		t.Fatal("urnetwork_sdk.hpp has no detail::dupCString")
	}
	if strings.Contains(dupCString, "malloc") {
		t.Errorf("dupCString allocates in the host (the library frees it with its own free):\n%s", dupCString)
	}
	if !strings.Contains(dupCString, "urnet_copy_string(") {
		t.Errorf("dupCString does not allocate through urnet_copy_string:\n%s", dupCString)
	}

	// every trampoline that hands a string back to the library goes through it
	trampolineReturns := regexp.MustCompile(`(?s)inline char\* (?:retained|oneshot)_\w+\(void\* user_data[^)]*\) \{.*?\n\}\n`).FindAllString(wrapper, -1)
	if len(trampolineReturns) == 0 {
		t.Fatal("no string-returning callback trampoline was generated (FlowOwnerLookup)")
	}
	for _, trampoline := range trampolineReturns {
		if !strings.Contains(trampoline, "return dupCString(r);") {
			t.Errorf("string-returning trampoline does not return a library-allocated copy:\n%s", trampoline)
		}
	}
}

// Build the generated wrapper against an instrumented library and run the
// FlowOwnerLookup trampoline: the pointer it returns must be one the library
// allocated, so the library's urnet_free_string releases its own block.
func TestCallbackStringResultFreesOnTheLibraryHeap(t *testing.T) {
	compiler, err := exec.LookPath("clang++")
	if err != nil {
		t.Skip("clang++ is not installed")
	}
	jsonInclude := os.Getenv("NLOHMANN_INCLUDE")
	if jsonInclude == "" {
		jsonInclude = "/opt/homebrew/include"
	}
	if _, err := os.Stat(filepath.Join(jsonInclude, "nlohmann", "json.hpp")); err != nil {
		t.Skipf("nlohmann/json.hpp is not under %s (set NLOHMANN_INCLUDE)", jsonInclude)
	}

	outputDirectory := testingCallbackStringGenerate(t)
	program := filepath.Join(outputDirectory, "callback_string_heap.cpp")
	if err := os.WriteFile(program, []byte(callbackStringHeapProgram), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(outputDirectory, "callback_string_heap")
	build := exec.Command(compiler, "-std=c++17",
		"-I"+filepath.Join(outputDirectory, "include"), "-I"+jsonInclude,
		"-o", binary, program)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile the generated wrapper: %v\n%s", err, output)
	}
	if output, err := exec.Command(binary).CombinedOutput(); err != nil {
		t.Fatalf("callback string crossed the module boundary: %v\n%s", err, output)
	}
}

func testingCallbackStringGenerate(t *testing.T) string {
	t.Helper()
	g := testingPreferenceGenerator(t)
	outputDirectory := t.TempDir()
	t.Chdir(outputDirectory)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	return outputDirectory
}

func testingCallbackStringRead(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The program stands in for the library: it defines the two core string
// exports with a tracked allocator, so a block the host allocated itself is
// caught before it reaches free(). Only the wrapper's trampoline is linked; no
// other export is referenced.
const callbackStringHeapProgram = `#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <set>

#include "urnetwork_sdk.hpp"

static std::set<void*>& libraryBlocks() {
	static std::set<void*> blocks;
	return blocks;
}

extern "C" char* urnet_copy_string(const char* s) {
	if (!s) {
		return nullptr;
	}
	size_t n = std::strlen(s) + 1;
	char* p = static_cast<char*>(std::malloc(n));
	std::memcpy(p, s, n);
	libraryBlocks().insert(p);
	return p;
}

extern "C" void urnet_free_string(char* s) {
	if (!s) {
		return;
	}
	if (libraryBlocks().erase(s) == 0) {
		std::fprintf(stderr, "urnet_free_string: %p was not allocated by the library\n", static_cast<void*>(s));
		std::exit(2);
	}
	std::free(s);
}

int main() {
	urnet::FlowOwnerLookup lookup = [](int64_t, int64_t, std::string, int64_t, std::string, int64_t) {
		return std::string("C:\\Program Files\\App\\app.exe");
	};
	char* r = urnet::detail::retained_flow_owner_lookup(&lookup, 4, 6, "10.0.0.2", 50000, "1.1.1.1", 443);
	if (!r || std::strcmp(r, "C:\\Program Files\\App\\app.exe") != 0) {
		std::fprintf(stderr, "trampoline returned %s\n", r ? r : "(null)");
		return 1;
	}
	// what the library does with the result (exports_gen.go, cAdapterFlowOwnerLookup)
	urnet_free_string(r);

	auto* once = new urnet::FlowOwnerLookup([](int64_t, int64_t, std::string, int64_t, std::string, int64_t) {
		return std::string();
	});
	urnet_free_string(urnet::detail::oneshot_flow_owner_lookup(once, 6, 17, "::1", 1, "::2", 2));
	return 0;
}
`
