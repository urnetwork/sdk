//go:build ignore

/**
 * Generate the TypeScript API client from the OpenAPI spec of record.
 *
 *   go run gen_openapi.go            # regenerate src/generated/openapi.ts + client.ts
 *   go run gen_openapi.go -check     # exit 1 when the committed output is stale
 *   go run gen_openapi.go -spec PATH # read another copy of the spec
 *
 * The spec of record is connect/api/bringyour.yml, read relative to the sdk
 * repo in the monoroot (../../connect/api/bringyour.yml from sdk/js). The
 * output is committed so npm consumers build without connect.
 *
 * The output is a pure function of the spec bytes: document order is kept
 * (yaml.Node, never a Go map), nothing depends on time or the environment.
 */

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultSpecPath = "../../connect/api/bringyour.yml"

const typesOutPath = "src/generated/openapi.ts"
const clientOutPath = "src/generated/client.ts"

func main() {
	specPath := flag.String("spec", defaultSpecPath, "path to the OpenAPI spec (bringyour.yml)")
	check := flag.Bool("check", false, "compare against the committed output instead of writing it")
	flag.Parse()

	specBytes, err := os.ReadFile(*specPath)
	if err != nil {
		abs, _ := filepath.Abs(*specPath)
		fail("cannot read the OpenAPI spec at %s (%s): %v\n"+
			"  the spec of record lives in the connect repo (connect/api/bringyour.yml);\n"+
			"  check out connect beside sdk in the monoroot, or pass -spec PATH / OPENAPI_SPEC=PATH",
			*specPath, abs, err)
	}

	g, err := newGenerator(specBytes)
	if err != nil {
		fail("%v", err)
	}
	types, client, err := g.generate()
	if err != nil {
		fail("%v", err)
	}

	outputs := []struct {
		path    string
		content []byte
	}{
		{typesOutPath, types},
		{clientOutPath, client},
	}

	if *check {
		stale := []string{}
		for _, out := range outputs {
			current, err := os.ReadFile(out.path)
			if err != nil || !bytes.Equal(current, out.content) {
				stale = append(stale, out.path)
			}
		}
		if 0 < len(stale) {
			fail("generated OpenAPI client is stale: %s\n  regenerate with `make generate_openapi` (go run gen_openapi.go) and commit the result",
				strings.Join(stale, ", "))
		}
		fmt.Printf("OpenAPI client is up to date with %s (%d operations)\n", *specPath, g.operationCount)
		return
	}

	for _, out := range outputs {
		if err := os.MkdirAll(filepath.Dir(out.path), 0755); err != nil {
			fail("%v", err)
		}
		if err := os.WriteFile(out.path, out.content, 0644); err != nil {
			fail("%v", err)
		}
	}
	fmt.Printf("Generated the OpenAPI client from %s (%d operations, %d schemas)\n", *specPath, g.operationCount, g.schemaCount)
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gen_openapi: "+format+"\n", args...)
	os.Exit(1)
}

/*
 * yaml.Node helpers — the node tree keeps document order
 */

func deref(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n != nil && n.Kind == yaml.DocumentNode && 0 < len(n.Content) {
		return deref(n.Content[0])
	}
	return n
}

type pair struct {
	key   string
	value *yaml.Node
}

func pairs(n *yaml.Node) []pair {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]pair, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, pair{deref(n.Content[i]).Value, deref(n.Content[i+1])})
	}
	return out
}

func get(n *yaml.Node, key string) *yaml.Node {
	for _, p := range pairs(n) {
		if p.key == key {
			return p.value
		}
	}
	return nil
}

func items(n *yaml.Node) []*yaml.Node {
	n = deref(n)
	if n == nil || n.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]*yaml.Node, 0, len(n.Content))
	for _, c := range n.Content {
		out = append(out, deref(c))
	}
	return out
}

func str(n *yaml.Node) string {
	n = deref(n)
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

func isTrue(n *yaml.Node) bool {
	return str(n) == "true"
}

// literal renders a scalar as a TypeScript literal type
func literal(n *yaml.Node) (string, error) {
	n = deref(n)
	if n == nil || n.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("const/enum value at line %d is not a scalar", lineOf(n))
	}
	switch n.Tag {
	case "!!int", "!!float":
		return n.Value, nil
	case "!!bool":
		return n.Value, nil
	case "!!null":
		return "null", nil
	default:
		return jsonString(n.Value), nil
	}
}

func lineOf(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	return n.Line
}

func jsonString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimRight(buf.String(), "\n")
}

/*
 * naming
 */

var identRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
var wordRe = regexp.MustCompile(`[A-Za-z0-9]+`)

func propKey(name string) string {
	if identRe.MatchString(name) {
		return name
	}
	return jsonString(name)
}

// methodName normalizes an operationId to camelCase. The spec mixes styles
// ("Auth Login", "Stats Providers Last N hours", "authNetworkCreate",
// "My IP Info"); words are split on non-alphanumerics, an all-caps word is
// treated as an acronym ("IP" -> "Ip"), and a word already in camelCase keeps
// its inner capitals.
func methodName(operationId string) string {
	words := wordRe.FindAllString(operationId, -1)
	var sb strings.Builder
	for i, w := range words {
		if 1 < len(w) && w == strings.ToUpper(w) {
			w = w[:1] + strings.ToLower(w[1:])
		}
		if i == 0 {
			sb.WriteString(strings.ToLower(w[:1]) + w[1:])
		} else {
			sb.WriteString(strings.ToUpper(w[:1]) + w[1:])
		}
	}
	return sb.String()
}

// members of the hand-written base class a generated method must not shadow
var reservedMethodNames = map[string]bool{
	"constructor": true,
	"request":     true,
	"call":        true,
	"setToken":    true,
	"baseURL":     true,
	"config":      true,
}

/*
 * the generator
 */

type generator struct {
	root           *yaml.Node
	specSha        string
	specVersion    string
	schemaNames    map[string]bool
	schemaCount    int
	operationCount int
	globalSecurity *yaml.Node
}

func newGenerator(specBytes []byte) (*generator, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(specBytes, &doc); err != nil {
		return nil, fmt.Errorf("the OpenAPI spec is not valid yaml: %w", err)
	}
	root := deref(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("the OpenAPI spec has no top-level mapping")
	}
	if v := str(get(root, "openapi")); !strings.HasPrefix(v, "3.") {
		return nil, fmt.Errorf("unsupported OpenAPI version %q (want 3.x)", v)
	}
	sum := sha256.Sum256(specBytes)
	g := &generator{
		root:           root,
		specSha:        hex.EncodeToString(sum[:]),
		specVersion:    str(get(get(root, "info"), "version")),
		schemaNames:    map[string]bool{},
		globalSecurity: get(root, "security"),
	}
	for _, p := range pairs(get(get(root, "components"), "schemas")) {
		if !identRe.MatchString(p.key) {
			return nil, fmt.Errorf("schema name %q is not a TypeScript identifier", p.key)
		}
		g.schemaNames[p.key] = true
	}
	return g, nil
}

func (g *generator) header(sb *strings.Builder) {
	sb.WriteString("// Auto-generated from the URnetwork OpenAPI spec (connect/api/bringyour.yml)\n")
	sb.WriteString("// DO NOT EDIT - Generated by: go run gen_openapi.go (make generate_openapi)\n")
	sb.WriteString(fmt.Sprintf("// spec version: %s\n", g.specVersion))
	sb.WriteString(fmt.Sprintf("// spec sha256: %s\n", g.specSha))
	sb.WriteString("\n")
	sb.WriteString("/* eslint-disable */\n\n")
}

func docComment(sb *strings.Builder, indent string, lines ...string) {
	text := strings.TrimSpace(strings.Join(lines, "\n\n"))
	if text == "" {
		return
	}
	text = strings.ReplaceAll(text, "*/", "*\\/")
	sb.WriteString(indent + "/**\n")
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			sb.WriteString(indent + " *\n")
		} else {
			sb.WriteString(indent + " * " + line + "\n")
		}
	}
	sb.WriteString(indent + " */\n")
}

// refName resolves a local schema $ref
func (g *generator) refName(ref string) (string, error) {
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("unsupported $ref %q (only %s* is supported)", ref, prefix)
	}
	name := strings.TrimPrefix(ref, prefix)
	if !g.schemaNames[name] {
		return "", fmt.Errorf("dangling $ref %q", ref)
	}
	return name, nil
}

func needsParens(t string) bool {
	depth := 0
	for _, r := range t {
		switch r {
		case '{', '(', '<', '[':
			depth += 1
		case '}', ')', '>', ']':
			depth -= 1
		case '|', '&':
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

func wrap(t string) string {
	if needsParens(t) {
		return "(" + t + ")"
	}
	return t
}

func joinTypes(types []string, sep string) string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range types {
		if sep == " & " {
			t = wrap(t)
		} else if strings.Contains(t, " & ") && needsParens(t) {
			t = "(" + t + ")"
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if len(out) == 1 {
		return out[0]
	}
	return strings.Join(out, sep)
}

// tsType renders a schema as a TypeScript type expression. indent is the
// indentation of the line the expression starts on.
func (g *generator) tsType(s *yaml.Node, indent string) (string, error) {
	s = deref(s)
	if s == nil {
		return "unknown", nil
	}
	if s.Kind == yaml.ScalarNode {
		// boolean schemas (3.1): true = anything, false = nothing
		if s.Value == "false" {
			return "never", nil
		}
		return "unknown", nil
	}
	if s.Kind != yaml.MappingNode {
		return "", fmt.Errorf("schema at line %d is not a mapping", s.Line)
	}

	if ref := get(s, "$ref"); ref != nil {
		// 3.1 allows sibling keywords (description); the ref is the type
		return g.refName(str(ref))
	}

	if c := get(s, "const"); c != nil {
		return literal(c)
	}
	if e := get(s, "enum"); e != nil {
		values := []string{}
		for _, v := range items(e) {
			lit, err := literal(v)
			if err != nil {
				return "", err
			}
			values = append(values, lit)
		}
		if len(values) == 0 {
			return "never", nil
		}
		return joinTypes(values, " | "), nil
	}

	// the object/array/primitive shape the schema itself declares
	base, err := g.baseType(s, indent)
	if err != nil {
		return "", err
	}
	hasOwnShape := get(s, "properties") != nil || get(s, "additionalProperties") != nil

	for _, combinator := range []string{"oneOf", "anyOf"} {
		if members := get(s, combinator); members != nil {
			union := []string{}
			for _, m := range items(members) {
				t, err := g.tsType(m, indent)
				if err != nil {
					return "", err
				}
				union = append(union, t)
			}
			u := joinTypes(union, " | ")
			if hasOwnShape {
				return joinTypes([]string{base, u}, " & "), nil
			}
			return u, nil
		}
	}
	if members := get(s, "allOf"); members != nil {
		parts := []string{}
		for _, m := range items(members) {
			t, err := g.tsType(m, indent)
			if err != nil {
				return "", err
			}
			parts = append(parts, t)
		}
		if hasOwnShape {
			parts = append(parts, base)
		}
		return joinTypes(parts, " & "), nil
	}
	return base, nil
}

func (g *generator) baseType(s *yaml.Node, indent string) (string, error) {
	typeNode := get(s, "type")
	var typeNames []string
	if typeNode != nil && typeNode.Kind == yaml.SequenceNode {
		for _, t := range items(typeNode) {
			typeNames = append(typeNames, str(t))
		}
	} else if typeNode != nil {
		typeNames = []string{str(typeNode)}
	} else if get(s, "properties") != nil || get(s, "additionalProperties") != nil {
		typeNames = []string{"object"}
	} else if get(s, "items") != nil {
		typeNames = []string{"array"}
	}
	if len(typeNames) == 0 {
		return "unknown", nil
	}
	out := []string{}
	for _, t := range typeNames {
		switch t {
		case "string":
			if str(get(s, "format")) == "binary" {
				out = append(out, "Blob")
			} else {
				out = append(out, "string")
			}
		case "integer", "number":
			out = append(out, "number")
		case "boolean":
			out = append(out, "boolean")
		case "null":
			out = append(out, "null")
		case "array":
			itemType, err := g.tsType(get(s, "items"), indent)
			if err != nil {
				return "", err
			}
			if identRe.MatchString(itemType) {
				out = append(out, itemType+"[]")
			} else {
				out = append(out, "Array<"+itemType+">")
			}
		case "object":
			t, err := g.objectType(s, indent)
			if err != nil {
				return "", err
			}
			out = append(out, t)
		default:
			return "", fmt.Errorf("unsupported schema type %q at line %d", t, s.Line)
		}
	}
	return joinTypes(out, " | "), nil
}

func (g *generator) objectType(s *yaml.Node, indent string) (string, error) {
	props := pairs(get(s, "properties"))
	ap := get(s, "additionalProperties")

	var apType string
	if ap != nil {
		if ap.Kind == yaml.ScalarNode {
			if isTrue(ap) {
				apType = "unknown"
			}
		} else {
			t, err := g.tsType(ap, indent)
			if err != nil {
				return "", err
			}
			apType = t
		}
	}

	if len(props) == 0 {
		if apType == "" {
			if ap != nil && !isTrue(ap) {
				// additionalProperties: false and no properties
				return "Record<string, never>", nil
			}
			return "Record<string, unknown>", nil
		}
		return "Record<string, " + apType + ">", nil
	}

	body, err := g.objectBody(s, indent)
	if err != nil {
		return "", err
	}
	t := "{\n" + body + indent + "}"
	if apType != "" {
		t = t + " & Record<string, " + apType + ">"
	}
	return t, nil
}

// objectBody renders the property lines of an object schema (without braces)
func (g *generator) objectBody(s *yaml.Node, indent string) (string, error) {
	required := map[string]bool{}
	for _, r := range items(get(s, "required")) {
		required[str(r)] = true
	}
	inner := indent + "  "
	var sb strings.Builder
	for _, p := range pairs(get(s, "properties")) {
		t, err := g.tsType(p.value, inner)
		if err != nil {
			return "", fmt.Errorf("property %q: %w", p.key, err)
		}
		docComment(&sb, inner, str(get(p.value, "description")))
		optional := "?"
		if required[p.key] {
			optional = ""
		}
		sb.WriteString(fmt.Sprintf("%s%s%s: %s;\n", inner, propKey(p.key), optional, t))
	}
	return sb.String(), nil
}

func isPlainObject(s *yaml.Node) bool {
	s = deref(s)
	if s == nil || s.Kind != yaml.MappingNode {
		return false
	}
	for _, k := range []string{"$ref", "oneOf", "anyOf", "allOf", "enum", "const"} {
		if get(s, k) != nil {
			return false
		}
	}
	if t := get(s, "type"); t != nil && (t.Kind != yaml.ScalarNode || str(t) != "object") {
		return false
	}
	ap := get(s, "additionalProperties")
	if ap != nil && !(ap.Kind == yaml.ScalarNode && !isTrue(ap)) {
		return false
	}
	return 0 < len(pairs(get(s, "properties")))
}

/*
 * operations
 */

type param struct {
	name        string
	in          string
	required    bool
	tsType      string
	description string
}

type operation struct {
	operationId string
	method      string
	name        string
	httpMethod  string
	path        string
	summary     string
	description string
	auth        string
	deprecated  bool
	tags        []string

	params []param

	bodyType     string // "", json, form, ndjson, binary
	bodyTS       string
	bodyRequired bool
	bodyInline   bool

	responseType   string // none, json, text, blob
	responseTS     string
	responseInline bool
}

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

func (g *generator) authScheme(security *yaml.Node) (string, error) {
	if security == nil {
		security = g.globalSecurity
	}
	reqs := items(security)
	if len(reqs) == 0 {
		return "none", nil
	}
	// an empty requirement ({}) next to BearerAuth makes the bearer optional:
	// anonymous callers are served, and a token adds caller-specific fields
	optional := false
	for _, req := range reqs {
		if len(pairs(req)) == 0 {
			optional = true
		}
	}
	// the first requirement that names a scheme decides
	for _, req := range reqs {
		for _, p := range pairs(req) {
			switch p.key {
			case "BearerAuth":
				if optional {
					return "optional", nil
				}
				return "bearer", nil
			case "AdminBearerAuth":
				return "admin", nil
			case "OperatorSecret":
				return "operator", nil
			case "BrevoWebhookAuth":
				return "basic", nil
			default:
				return "", fmt.Errorf("unknown security scheme %q", p.key)
			}
		}
	}
	return "none", nil
}

func (g *generator) operations() ([]*operation, error) {
	ops := []*operation{}
	names := map[string]string{}
	for _, pathPair := range pairs(get(g.root, "paths")) {
		path := pathPair.key
		pathItem := pathPair.value
		pathParams := items(get(pathItem, "parameters"))
		for _, m := range httpMethods {
			opNode := get(pathItem, m)
			if opNode == nil {
				continue
			}
			op, err := g.operation(path, m, opNode, pathParams)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", strings.ToUpper(m), path, err)
			}
			if prev, ok := names[op.name]; ok {
				return nil, fmt.Errorf("operationId %q and %q both normalize to %s", prev, op.operationId, op.name)
			}
			if reservedMethodNames[op.name] {
				return nil, fmt.Errorf("operationId %q normalizes to the reserved client member %s", op.operationId, op.name)
			}
			names[op.name] = op.operationId
			ops = append(ops, op)
		}
	}
	return ops, nil
}

var pathParamRe = regexp.MustCompile(`\{([^}]+)\}`)

func (g *generator) operation(path string, m string, opNode *yaml.Node, pathParams []*yaml.Node) (*operation, error) {
	op := &operation{
		operationId: str(get(opNode, "operationId")),
		httpMethod:  strings.ToUpper(m),
		path:        path,
		summary:     strings.TrimSpace(str(get(opNode, "summary"))),
		description: strings.TrimSpace(str(get(opNode, "description"))),
		deprecated:  isTrue(get(opNode, "deprecated")),
	}
	for _, t := range items(get(opNode, "tags")) {
		op.tags = append(op.tags, str(t))
	}
	if op.operationId == "" {
		return nil, fmt.Errorf("missing operationId")
	}
	op.name = methodName(op.operationId)
	if op.name == "" || !identRe.MatchString(op.name) {
		return nil, fmt.Errorf("operationId %q does not normalize to an identifier", op.operationId)
	}

	auth, err := g.authScheme(get(opNode, "security"))
	if err != nil {
		return nil, err
	}
	op.auth = auth

	// parameters: operation-level override path-level by (name, in)
	merged := []*yaml.Node{}
	index := map[string]int{}
	for _, list := range [][]*yaml.Node{pathParams, items(get(opNode, "parameters"))} {
		for _, p := range list {
			if ref := get(p, "$ref"); ref != nil {
				return nil, fmt.Errorf("parameter $ref %q is not supported", str(ref))
			}
			key := str(get(p, "in")) + ":" + str(get(p, "name"))
			if i, ok := index[key]; ok {
				merged[i] = p
			} else {
				index[key] = len(merged)
				merged = append(merged, p)
			}
		}
	}
	declaredPath := map[string]bool{}
	for _, p := range merged {
		in := str(get(p, "in"))
		name := str(get(p, "name"))
		switch in {
		case "path", "query", "header":
		default:
			return nil, fmt.Errorf("parameter %q in %q is not supported", name, in)
		}
		t, err := g.tsType(get(p, "schema"), "      ")
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", name, err)
		}
		required := isTrue(get(p, "required")) || in == "path"
		if in == "path" {
			declaredPath[name] = true
		}
		op.params = append(op.params, param{
			name:        name,
			in:          in,
			required:    required,
			tsType:      t,
			description: strings.TrimSpace(str(get(p, "description"))),
		})
	}
	for _, match := range pathParamRe.FindAllStringSubmatch(path, -1) {
		if !declaredPath[match[1]] {
			return nil, fmt.Errorf("path parameter {%s} is not declared", match[1])
		}
	}

	// request body: prefer json, then form, ndjson, binary
	if rb := get(opNode, "requestBody"); rb != nil {
		if ref := get(rb, "$ref"); ref != nil {
			return nil, fmt.Errorf("requestBody $ref %q is not supported", str(ref))
		}
		content := get(rb, "content")
		op.bodyRequired = isTrue(get(rb, "required"))
		for _, candidate := range []struct {
			contentType string
			bodyType    string
		}{
			{"application/json", "json"},
			{"application/x-www-form-urlencoded", "form"},
			{"application/x-ndjson", "ndjson"},
			{"application/octet-stream", "binary"},
		} {
			media := get(content, candidate.contentType)
			if media == nil {
				continue
			}
			op.bodyType = candidate.bodyType
			schema := get(media, "schema")
			switch candidate.bodyType {
			case "binary":
				op.bodyTS = "BodyInit"
			case "ndjson":
				op.bodyTS = "string"
			default:
				t, err := g.tsType(schema, "    ")
				if err != nil {
					return nil, fmt.Errorf("requestBody: %w", err)
				}
				op.bodyTS = t
				op.bodyInline = get(schema, "$ref") == nil
			}
			break
		}
		if op.bodyType == "" {
			ct := []string{}
			for _, p := range pairs(content) {
				ct = append(ct, p.key)
			}
			return nil, fmt.Errorf("requestBody content types %v are not supported", ct)
		}
	}

	// response: the first 2xx in document order
	op.responseType = "none"
	op.responseTS = "void"
	for _, rp := range pairs(get(opNode, "responses")) {
		if !strings.HasPrefix(rp.key, "2") {
			continue
		}
		if ref := get(rp.value, "$ref"); ref != nil {
			return nil, fmt.Errorf("response $ref %q is not supported", str(ref))
		}
		content := get(rp.value, "content")
		if media := get(content, "application/json"); media != nil {
			schema := get(media, "schema")
			t, err := g.tsType(schema, "    ")
			if err != nil {
				return nil, fmt.Errorf("response %s: %w", rp.key, err)
			}
			op.responseType = "json"
			op.responseTS = t
			op.responseInline = get(schema, "$ref") == nil
		} else {
			for _, p := range pairs(content) {
				switch {
				case p.key == "text/plain" || p.key == "application/x-ndjson":
					op.responseType = "text"
					op.responseTS = "string"
				case strings.HasPrefix(p.key, "image/") || p.key == "application/octet-stream":
					op.responseType = "blob"
					op.responseTS = "Blob"
				default:
					return nil, fmt.Errorf("response %s content type %q is not supported", rp.key, p.key)
				}
				break
			}
		}
		break
	}
	return op, nil
}

func (op *operation) paramsType() string {
	if len(op.params) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("{\n")
	for _, p := range op.params {
		docComment(&sb, "      ", strings.TrimSpace(p.in+" parameter. "+p.description))
		optional := "?"
		if p.required {
			optional = ""
		}
		sb.WriteString(fmt.Sprintf("      %s%s: %s;\n", propKey(p.name), optional, p.tsType))
	}
	sb.WriteString("    }")
	return sb.String()
}

func (op *operation) paramsRequired() bool {
	for _, p := range op.params {
		if p.required {
			return true
		}
	}
	// a params object followed by a body argument is positional, so it is
	// always passed (possibly as {})
	return op.bodyType != ""
}

func (op *operation) docLines() []string {
	lines := []string{}
	if op.summary != "" {
		lines = append(lines, op.summary)
	}
	if op.description != "" {
		lines = append(lines, op.description)
	}
	meta := fmt.Sprintf("`%s %s` — operationId `%s`, auth: %s", op.httpMethod, op.path, op.operationId, op.auth)
	lines = append(lines, meta)
	if op.deprecated {
		lines = append(lines, "@deprecated")
	}
	return lines
}

func (g *generator) generate() ([]byte, []byte, error) {
	ops, err := g.operations()
	if err != nil {
		return nil, nil, err
	}
	g.operationCount = len(ops)

	/*
	 * openapi.ts: component schemas + the Operations map
	 */
	var types strings.Builder
	g.header(&types)
	types.WriteString("// Component schemas are emitted under their spec names. The package exports\n")
	types.WriteString("// this module as the `OpenAPI` namespace (import type { OpenAPI } from\n")
	types.WriteString("// \"@urnetwork/sdk\"), so a spec name never collides with a hand-written or\n")
	types.WriteString("// Go-reflected type of the same name.\n\n")

	for _, p := range pairs(get(get(g.root, "components"), "schemas")) {
		g.schemaCount += 1
		docComment(&types, "", str(get(p.value, "title")), str(get(p.value, "description")))
		if isPlainObject(p.value) {
			body, err := g.objectBody(p.value, "")
			if err != nil {
				return nil, nil, fmt.Errorf("schema %s: %w", p.key, err)
			}
			types.WriteString(fmt.Sprintf("export interface %s {\n%s}\n\n", p.key, body))
		} else {
			t, err := g.tsType(p.value, "")
			if err != nil {
				return nil, nil, fmt.Errorf("schema %s: %w", p.key, err)
			}
			types.WriteString(fmt.Sprintf("export type %s = %s;\n\n", p.key, t))
		}
	}

	types.WriteString("/**\n")
	types.WriteString(" * Every operation by client method name: its parameters (path, query and\n")
	types.WriteString(" * header, by their spec names), its request body and its success response.\n")
	types.WriteString(" * `never` marks an operation without parameters or without a body.\n")
	types.WriteString(" */\n")
	types.WriteString("export interface Operations {\n")
	for _, op := range ops {
		docComment(&types, "  ", fmt.Sprintf("`%s %s` — operationId `%s`", op.httpMethod, op.path, op.operationId))
		types.WriteString(fmt.Sprintf("  %s: {\n", op.name))
		if pt := op.paramsType(); pt != "" {
			types.WriteString(fmt.Sprintf("    params: %s;\n", pt))
		} else {
			types.WriteString("    params: never;\n")
		}
		if op.bodyType != "" {
			types.WriteString(fmt.Sprintf("    requestBody: %s;\n", op.bodyTS))
		} else {
			types.WriteString("    requestBody: never;\n")
		}
		types.WriteString(fmt.Sprintf("    response: %s;\n", op.responseTS))
		types.WriteString("  };\n")
	}
	types.WriteString("}\n\n")
	types.WriteString("export type OperationName = keyof Operations;\n")

	/*
	 * client.ts: one method per operation
	 */
	var client strings.Builder
	g.header(&client)
	client.WriteString("import type * as T from \"./openapi\";\n")
	client.WriteString("import {\n")
	client.WriteString("  URNetworkApiClientBase,\n")
	client.WriteString("  type URNetworkApiClientConfig,\n")
	client.WriteString("  type RequestOptions,\n")
	client.WriteString("} from \"../api_client\";\n\n")
	client.WriteString("/**\n")
	client.WriteString(" * The URnetwork api, one method per OpenAPI operation. Method names are the\n")
	client.WriteString(" * operationIds in camelCase. Arguments are positional: path/query/header\n")
	client.WriteString(" * params (when the operation has any), the request body (when it has one),\n")
	client.WriteString(" * then per-call RequestOptions. A non-2xx response rejects with\n")
	client.WriteString(" * URNetworkApiError; a 2xx body that carries its own `error` field resolves.\n")
	client.WriteString(" */\n")
	client.WriteString("export class URNetworkApiClient extends URNetworkApiClientBase {\n")
	for i, op := range ops {
		if 0 < i {
			client.WriteString("\n")
		}
		docComment(&client, "  ", op.docLines()...)

		args := []string{}
		callParams := "undefined"
		callBody := "undefined"
		if 0 < len(op.params) {
			pType := fmt.Sprintf("T.Operations[%s][\"params\"]", jsonString(op.name))
			if op.paramsRequired() {
				args = append(args, "params: "+pType)
			} else {
				args = append(args, "params?: "+pType)
			}
			callParams = "params"
		}
		if op.bodyType != "" {
			bType := op.bodyTS
			if op.bodyInline || !identRe.MatchString(bType) {
				bType = fmt.Sprintf("T.Operations[%s][\"requestBody\"]", jsonString(op.name))
			} else if g.schemaNames[bType] {
				bType = "T." + bType
			}
			if op.bodyRequired {
				args = append(args, "body: "+bType)
			} else {
				args = append(args, "body?: "+bType)
			}
			callBody = "body"
		}
		args = append(args, "options?: RequestOptions")

		rType := op.responseTS
		if op.responseInline || !identRe.MatchString(rType) {
			rType = fmt.Sprintf("T.Operations[%s][\"response\"]", jsonString(op.name))
		} else if g.schemaNames[rType] {
			rType = "T." + rType
		}

		meta := []string{
			"id: " + jsonString(op.operationId),
			"method: " + jsonString(op.httpMethod),
			"path: " + jsonString(op.path),
			"auth: " + jsonString(op.auth),
		}
		if op.bodyType != "" {
			meta = append(meta, "body: "+jsonString(op.bodyType))
		}
		meta = append(meta, "response: "+jsonString(op.responseType))
		// path param names are read from the path template at runtime
		for _, in := range []string{"query", "header"} {
			names := []string{}
			for _, p := range op.params {
				if p.in == in {
					names = append(names, jsonString(p.name))
				}
			}
			if 0 < len(names) {
				meta = append(meta, in+": ["+strings.Join(names, ", ")+"]")
			}
		}

		client.WriteString(fmt.Sprintf("  %s(%s): Promise<%s> {\n", op.name, strings.Join(args, ", "), rType))
		client.WriteString(fmt.Sprintf("    return this.call<%s>(\n", rType))
		client.WriteString("      { " + strings.Join(meta, ", ") + " },\n")
		client.WriteString(fmt.Sprintf("      %s,\n      %s,\n      options,\n    );\n", callParams, callBody))
		client.WriteString("  }\n")
	}
	client.WriteString("}\n\n")
	client.WriteString("/** Create a client for the URnetwork api (see URNetworkApiClientConfig). */\n")
	client.WriteString("export function createURNetworkApiClient(\n  config?: URNetworkApiClientConfig,\n): URNetworkApiClient {\n")
	client.WriteString("  return new URNetworkApiClient(config);\n}\n\n")
	client.WriteString(fmt.Sprintf("/** The spec the client was generated from. */\nexport const OPENAPI_SPEC_VERSION = %s;\n", jsonString(g.specVersion)))
	client.WriteString(fmt.Sprintf("export const OPENAPI_SPEC_SHA256 = %s;\n", jsonString(g.specSha)))

	return []byte(types.String()), []byte(client.String()), nil
}
