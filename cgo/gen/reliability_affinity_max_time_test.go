package main

import (
	"go/types"
	"strings"
	"testing"
)

// Inspect the actual SDK model through the sanctioned C/C++ generator. This
// test does not edit checked-in generated files.
func TestReliabilityAffinityMaxTimeGeneratedSchema(t *testing.T) {
	g := testingPreferenceGenerator(t)
	named, ok := types.Unalias(testingPreferenceType(t, g, "ReliabilitySettings").Type()).(*types.Named)
	if !ok {
		t.Fatal("ReliabilitySettings is not a named SDK model")
	}
	if got := g.classify(types.NewPointer(named)).kind; got != kindJson {
		t.Fatalf("ReliabilitySettings C representation = %v, want JSON", got)
	}
	fields, listElement := g.hppStructModel(named)
	if listElement != "" {
		t.Fatal("ReliabilitySettings unexpectedly became a list")
	}
	found := 0
	for _, field := range fields {
		if field.jsonName == "FreshFlowAffinity" || field.cppName == "FreshFlowAffinity" {
			t.Fatal("obsolete boolean remains in the generated C++ model")
		}
		if field.jsonName == "AffinityMaxTimeMillis" {
			found++
			if field != (hppField{cppName: "AffinityMaxTimeMillis", jsonName: "AffinityMaxTimeMillis", cppType: "int64_t"}) {
				t.Fatalf("affinity C++ field = %+v", field)
			}
		}
	}
	if found != 1 {
		t.Fatalf("affinity field count = %d", found)
	}
	doc := g.dataDoc("ReliabilitySettings", named)
	if strings.Count(doc, "AffinityMaxTimeMillis: number") != 1 ||
		strings.Contains(doc, "AffinityMaxTimeMillis: boolean") ||
		strings.Contains(doc, "FreshFlowAffinity") {
		t.Fatalf("generated JSON schema did not migrate: %s", doc)
	}
	g.dataTypes = map[string]*types.Named{"ReliabilitySettings": named}
	var output strings.Builder
	if err := g.hppDataTypes(&output); err != nil {
		t.Fatal(err)
	}
	cpp := output.String()
	for _, required := range []string{
		"int64_t AffinityMaxTimeMillis{};",
		"j[\"AffinityMaxTimeMillis\"] = v.AffinityMaxTimeMillis;",
		"it->get_to(v.AffinityMaxTimeMillis);",
	} {
		if strings.Count(cpp, required) != 1 {
			t.Fatalf("generated C++ codec missing or duplicating %q", required)
		}
	}
	if strings.Contains(cpp, "FreshFlowAffinity") {
		t.Fatal("obsolete boolean remains in the generated C++ codec")
	}
}
