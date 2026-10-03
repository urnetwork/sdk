package sdk

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The browser sdk's TypeScript types are generated from these Go structs by
// `make generate_types` in js, and the generated file is committed. Nothing in
// a Go build reads it, so a field added or removed here and not regenerated
// leaves every browser embedder compiling against a shape the wasm no longer
// has -- and the failure surfaces as a runtime undefined, in someone else's
// repository.
//
// This pins the network space values, which the extender settings extended
// (EXTENDER.md F1, K6), and the shapes beside them, by rebuilding the emitted
// block from the live struct and comparing it to the committed file.

const jsGeneratedTypesPath = "js/src/generated/types.ts"

// The interfaces this pins, in the order the generator emits them.
func TestJsGeneratedTypesMatchTheGoStructs(t *testing.T) {
	generated, err := os.ReadFile(jsGeneratedTypesPath)
	if err != nil {
		t.Fatal(err)
	}
	generatedText := string(generated)

	for _, value := range []any{
		NetworkSpaceKey{},
		NetworkSpaceValues{},
		ExportNetworkSpace{},
		NetExtender{},
	} {
		expected := jsGeneratedInterface(t, value)
		if !strings.Contains(generatedText, expected) {
			name := reflect.TypeOf(value).Name()
			t.Errorf(
				"%s is stale in %s; run `make generate_types` in js. expected:\n%s\ngot:\n%s",
				name,
				jsGeneratedTypesPath,
				expected,
				jsGeneratedInterfaceText(generatedText, name),
			)
		}
	}

	// the removed auto-configure shape is gone from both sides (F1): the
	// generator no longer names it, so a leftover interface is a file that was
	// hand edited rather than regenerated
	if strings.Contains(generatedText, "NetExtenderAutoConfigure") {
		t.Errorf("%s still declares NetExtenderAutoConfigure", jsGeneratedTypesPath)
	}
}

// The interface the js generator emits for one struct. Mirrors
// js/gen_types.go: the json tag names the field, `omitempty` makes it
// optional, and the type mapping below covers every kind these structs use.
func jsGeneratedInterface(t *testing.T, value any) string {
	t.Helper()
	valueType := reflect.TypeOf(value)
	var text strings.Builder
	fmt.Fprintf(&text, "export interface %s {\n", valueType.Name())
	for i := range valueType.NumField() {
		field := valueType.Field(i)
		if !field.IsExported() {
			continue
		}
		jsonTag := field.Tag.Get("json")
		if jsonTag == "-" {
			continue
		}
		parts := strings.Split(jsonTag, ",")
		name := parts[0]
		if name == "" {
			t.Fatalf("%s.%s has no json name", valueType.Name(), field.Name)
		}
		optional := ""
		for _, part := range parts[1:] {
			if part == "omitempty" {
				optional = "?"
			}
		}
		fmt.Fprintf(&text, "  %s%s: %s;\n", name, optional, jsGeneratedType(t, field.Type))
	}
	text.WriteString("}")
	return text.String()
}

func jsGeneratedType(t *testing.T, fieldType reflect.Type) string {
	t.Helper()
	switch fieldType.Kind() {
	case reflect.Pointer:
		return jsGeneratedType(t, fieldType.Elem()) + " | null"
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice:
		return jsGeneratedType(t, fieldType.Elem()) + "[]"
	case reflect.Struct:
		return fieldType.Name()
	}
	// a kind the generator maps by a rule this mirror does not carry; extend
	// both together rather than guessing here
	t.Fatalf("no generated TypeScript type for %s", fieldType)
	return ""
}

// The committed block for one interface, for the failure message.
func jsGeneratedInterfaceText(generatedText string, name string) string {
	_, after, ok := strings.Cut(generatedText, "export interface "+name+" {")
	if !ok {
		return "(the file declares no " + name + ")"
	}
	body, _, _ := strings.Cut(after, "}")
	return "export interface " + name + " {" + body + "}"
}
