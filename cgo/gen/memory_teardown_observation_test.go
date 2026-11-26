// A device's memory teardown observation crosses the c abi as a handle (QD-1423). DeviceLocal's
// BeginMemoryTeardownObservation answered json, which was always "{}" because the observation has no exported field,
// while the observer it started kept sampling until its lifetime ended and nothing on the c side could finish it. Now
// the constructor export answers a uint64_t handle and the observation's four methods bind, in the header, the def,
// the c++ wrapper and the report. The cgo package's tests run the handle's lifecycle through the exports.
package main

import (
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each exported method of the observation and its c declaration.
var memoryTeardownObservationDeclarations = map[string]string{
	"GetObserverId":      "char* urnet_memory_teardown_observation_get_observer_id(uint64_t self);",
	"WaitForDeviceClose": "bool urnet_memory_teardown_observation_wait_for_device_close(uint64_t self, int64_t timeout_milliseconds);",
	"FinishAndTakeJson":  "char* urnet_memory_teardown_observation_finish_and_take_json(uint64_t self, int64_t timeout_milliseconds);",
	"AbortAndTakeJson":   "char* urnet_memory_teardown_observation_abort_and_take_json(uint64_t self, int64_t timeout_milliseconds);",
}

const memoryTeardownObservationBeginDeclaration = "uint64_t urnet_device_local_begin_memory_teardown_observation(uint64_t self, char** out_error);"

// The type is a handle, every exported method it declares binds with the declaration above, none is skipped, and the
// constructor registers what it answers as a handle rather than encoding it.
func TestMemoryTeardownObservationCrossesAsAHandle(t *testing.T) {
	g := testingPreferenceGenerator(t)
	object := testingPreferenceType(t, g, "MemoryTeardownObservation")
	if !behavioralTypes["MemoryTeardownObservation"] {
		t.Fatal("MemoryTeardownObservation is not a handle")
	}
	info := g.classify(types.NewPointer(object.Type()))
	if info.kind != kindHandle || info.named == nil || info.named.Obj().Name() != "MemoryTeardownObservation" {
		t.Fatalf("MemoryTeardownObservation crosses as kind %d", info.kind)
	}
	g.emitType(object)
	methods := types.NewMethodSet(types.NewPointer(object.Type()))
	boundCount := 0
	for i := 0; i < methods.Len(); i += 1 {
		method := methods.At(i).Obj()
		if !method.Exported() {
			continue
		}
		declaration, ok := memoryTeardownObservationDeclarations[method.Name()]
		if !ok {
			t.Errorf("MemoryTeardownObservation.%s has no declaration in this test", method.Name())
			continue
		}
		if actual := testingPreferenceExport(t, g, "MemoryTeardownObservation", method.Name()).cDecl; actual != declaration {
			t.Errorf("MemoryTeardownObservation.%s binds as %q", method.Name(), actual)
		}
		boundCount += 1
	}
	if boundCount != len(memoryTeardownObservationDeclarations) {
		t.Errorf("%d of the observation's %d methods bind", boundCount, len(memoryTeardownObservationDeclarations))
	}
	for _, skipped := range g.skipped {
		if strings.HasPrefix(skipped.symbol, "MemoryTeardownObservation") {
			t.Errorf("%s is skipped: %s", skipped.symbol, skipped.reason)
		}
	}

	g.emitType(testingPreferenceType(t, g, "DeviceLocal"))
	begin := testingPreferenceExport(t, g, "DeviceLocal", "BeginMemoryTeardownObservation")
	if begin.sig.result == nil || begin.sig.result.kind != kindHandle || !begin.sig.hasError {
		t.Fatal("the constructor does not answer a handle and its error")
	}
	if begin.cDecl != memoryTeardownObservationBeginDeclaration {
		t.Errorf("the constructor binds as %q", begin.cDecl)
	}
	if !strings.Contains(begin.goCode, "return C.uint64_t(newHandle(r0))") || strings.Contains(begin.goCode, "cJson(") {
		t.Error("the constructor does not register the observation as a handle")
	}
}

// A full run carries the handle through every generated file: the header declares the constructor's handle result and
// the four methods, the def names all five, the c++ wrapper has a handle class in place of the empty struct, and the
// report exports the four methods and no longer warns of the type.
func TestMemoryTeardownObservationHandleInTheGeneratedFiles(t *testing.T) {
	g := testingPreferenceGenerator(t)
	output := t.TempDir()
	t.Chdir(output)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	read := func(path string) string {
		t.Helper()
		fileBytes, err := os.ReadFile(filepath.Join(output, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(fileBytes)
	}
	header := read(filepath.Join("include", "urnetwork_sdk.h"))
	definition := "\n" + read(filepath.Join("include", "urnetwork_sdk.def")) + "\n"
	cpp := read(filepath.Join("include", "urnetwork_sdk.hpp"))
	coverage := read("coverage_report.txt")

	declarations := []string{memoryTeardownObservationBeginDeclaration}
	for _, declaration := range memoryTeardownObservationDeclarations {
		declarations = append(declarations, declaration)
	}
	for _, declaration := range declarations {
		if !strings.Contains(header, "\n"+declaration+"\n") {
			t.Errorf("the header does not declare %s", declaration)
		}
		_, afterResult, _ := strings.Cut(declaration, " ")
		symbol, _, _ := strings.Cut(afterResult, "(")
		if !strings.Contains(definition, "\n\t"+symbol+"\n") {
			t.Errorf("the def does not name %s", symbol)
		}
	}
	if strings.Contains(header, "char* urnet_device_local_begin_memory_teardown_observation(") ||
		strings.Contains(header, "MemoryTeardownObservation (json)") {
		t.Error("the header still carries the observation as json")
	}

	if !strings.Contains(cpp, "class MemoryTeardownObservation final : public detail::Handle {") ||
		strings.Contains(cpp, "struct MemoryTeardownObservation {") {
		t.Error("the c++ wrapper has no handle class for the observation")
	}
	for _, declaration := range []string{
		"MemoryTeardownObservation beginMemoryTeardownObservation() const;",
		"std::string getObserverId() const;",
		"bool waitForDeviceClose(int64_t timeout_milliseconds) const;",
		"std::string finishAndTakeJson(int64_t timeout_milliseconds) const;",
		"std::string abortAndTakeJson(int64_t timeout_milliseconds) const;",
	} {
		if !strings.Contains(cpp, declaration) {
			t.Errorf("the c++ wrapper does not declare %s", declaration)
		}
	}
	_, beginTail, found := strings.Cut(cpp, "inline MemoryTeardownObservation DeviceLocal::beginMemoryTeardownObservation() const {\n")
	beginBody, _, ended := strings.Cut(beginTail, "\n}\n")
	if !found || !ended ||
		!strings.Contains(beginBody, "MemoryTeardownObservation r(urnet_device_local_begin_memory_teardown_observation(handle(), &err_c));") ||
		!strings.Contains(beginBody, "detail::throwError(err_c);") {
		t.Error("the c++ constructor does not wrap the handle or throw the error")
	}

	for method := range memoryTeardownObservationDeclarations {
		if !strings.Contains(coverage, " <- MemoryTeardownObservation."+method+"\n") {
			t.Errorf("the report does not export MemoryTeardownObservation.%s", method)
		}
	}
	if strings.Contains(coverage, "MemoryTeardownObservation: data: crosses as json") ||
		strings.Contains(coverage, "MemoryTeardownObservation: (!)") {
		t.Error("the report still carries the observation as json")
	}
}

// A full run warns of no data type with an empty json shape. Such a type crosses as "{}", carrying nothing, and is a
// handle misclassified as data: the generator's own rule, which made MemoryTeardownObservation a handle (QD-1423), the
// last type the warning named. A later one fails here rather than only on the generator's standard error.
func TestNoDataTypeCrossesWithAnEmptyJsonShape(t *testing.T) {
	g := testingPreferenceGenerator(t)
	output := t.TempDir()
	t.Chdir(output)
	if err := g.run(); err != nil {
		t.Fatal(err)
	}
	for _, skipped := range g.skipped {
		if strings.Contains(skipped.reason, "empty json shape") {
			t.Errorf("%s: %s", skipped.symbol, skipped.reason)
		}
	}
	if strings.Contains(testingGeneratedFile(t, "..", "coverage_report.txt"), "empty json shape") {
		t.Error("the committed report warns of a data type with an empty json shape")
	}
}
