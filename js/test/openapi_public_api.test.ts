import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";

// The committed client is the public api of the spec of record,
// connect/api/bringyour.yml, as ur.io publishes it: no operation the spec
// marks x-internal, and exactly the component schemas the rest of the
// document reaches. This reads the spec by its block layout (top-level keys
// at column 0, paths and component sections at 2, methods, path-level keys
// and component entries at 4, operation keys at 6), apart from the
// generator's yaml walk, so the two have to agree. Without connect beside
// sdk there is nothing to compare to and the checks are skipped.

const specUrl = new URL("../../../connect/api/bringyour.yml", import.meta.url);
const skip = existsSync(specUrl) ? false : "connect/api/bringyour.yml is not checked out beside sdk";
const read = (relative: string) => readFileSync(new URL(relative, import.meta.url), "utf8");

const METHODS = new Set(["get", "put", "post", "delete", "options", "head", "patch", "trace"]);
// a component pointer in any string, the pattern publishable-spec.mjs reads
const POINTER = /#\/components\/([^/\s"'`#]+)\/([A-Za-z0-9._-]*[A-Za-z0-9_-])/g;

const indentOf = (line: string) => line.search(/\S/);
const isComment = (line: string) => line.trimStart().startsWith("#");
const unquote = (value: string) => value.replace(/^(["'])(.*)\1$/, "$2");

function keyAt(line: string, indent: number) {
  if (indentOf(line) !== indent || isComment(line)) return null;
  const key = line.slice(indent).match(/^(.+?):(?:\s|$)/);
  return key ? unquote(key[1]) : null;
}

// the mapping entries `indent` columns in between two lines, each up to the next
function entries(lines: string[], start: number, end: number, indent: number) {
  const out: Array<{ key: string; start: number; end: number }> = [];
  for (let i = start; i < end; i++) {
    const key = keyAt(lines[i], indent);
    if (key === null) continue;
    if (out.length) out[out.length - 1].end = i;
    out.push({ key, start: i, end });
  }
  return out;
}

function publicApi(spec: string) {
  const lines = spec.split("\n");
  // a YAML comment is not in the parse, so it reaches nothing
  const text = (start: number, end: number, left: Set<number> = new Set()) =>
    lines.slice(start, end).filter((line, i) => !left.has(start + i) && !isComment(line)).join("\n");
  const top = entries(lines, 0, lines.length, 0);
  const paths = top.find(entry => entry.key === "paths");
  const components = top.find(entry => entry.key === "components");
  assert.ok(paths && components, "the spec has top-level paths and components");

  const published = top.filter(entry => entry !== paths && entry !== components).map(entry => text(entry.start, entry.end));
  const internal: Array<{ operation: string; operationId: string }> = [];
  for (const path of entries(lines, paths.start + 1, paths.end, 2)) {
    const keys = entries(lines, path.start + 1, path.end, 4);
    const pathInternal = keys.some(key => key.key === "x-internal" && /^ {4}x-internal: true\s*$/.test(lines[key.start]));
    const operations = keys.filter(key => METHODS.has(key.key));
    const hidden = operations.filter(op =>
      pathInternal || lines.slice(op.start, op.end).some(line => /^ {6}x-internal: true\s*$/.test(line)));
    for (const op of hidden) {
      const id = lines.slice(op.start, op.end).map(line => line.match(/^ {6}operationId: (.+?)\s*$/)?.[1]).find(Boolean);
      assert.ok(id, `${op.key.toUpperCase()} ${path.key} has no operationId line`);
      internal.push({ operation: `${op.key.toUpperCase()} ${path.key}`, operationId: unquote(id) });
    }
    // a path whose every operation is x-internal goes whole, with its path-level keys
    if (0 < hidden.length && hidden.length === operations.length) continue;
    const left = new Set(hidden.flatMap(op => Array.from({ length: op.end - op.start }, (_, i) => op.start + i)));
    published.push(text(path.start, path.end, left));
  }

  const component = new Map<string, string>();
  for (const section of entries(lines, components.start + 1, components.end, 2)) {
    for (const entry of entries(lines, section.start + 1, section.end, 4)) {
      component.set(`${section.key}/${entry.key}`, text(entry.start, entry.end));
    }
  }
  const reached = new Set<string>();
  const queue = published.flatMap(t => [...t.matchAll(POINTER)].map(match => `${match[1]}/${match[2]}`));
  while (queue.length) {
    const next = queue.pop()!;
    if (reached.has(next)) continue;
    reached.add(next);
    for (const match of (component.get(next) || "").matchAll(POINTER)) queue.push(`${match[1]}/${match[2]}`);
  }
  const schemas = [...component.keys()].filter(c => c.startsWith("schemas/")).map(c => c.slice("schemas/".length));
  return { internal, schemas: schemas.filter(name => reached.has(`schemas/${name}`)) };
}

test("the committed client has no operation the spec of record marks x-internal", { skip }, () => {
  const { internal } = publicApi(readFileSync(specUrl, "utf8"));
  assert.ok(0 < internal.length, "the layout read found no x-internal operation: the spec's layout changed, update this test");
  const client = read("../src/generated/client.ts");
  const types = read("../src/generated/openapi.ts");
  const shipped = internal.filter(op =>
    client.includes(`id: ${JSON.stringify(op.operationId)}`) || types.includes(`operationId \`${op.operationId}\``));
  assert.deepEqual(shipped.map(op => op.operation), [], `${shipped.length} of ${internal.length} x-internal operations ship in the client`);
});

test("the committed client declares exactly the schemas the public api reaches", { skip }, () => {
  const { schemas } = publicApi(readFileSync(specUrl, "utf8"));
  const types = read("../src/generated/openapi.ts");
  const declared = [...types.matchAll(/^export (?:interface|type) (\w+)/gm)].map(match => match[1])
    .filter(name => name !== "Operations" && name !== "OperationName");
  const published = new Set(schemas);
  const extra = declared.filter(name => !published.has(name));
  const missing = schemas.filter(name => !declared.includes(name));
  assert.deepEqual({ extra: extra.length, missing }, { extra: 0, missing: [] },
    `${extra.length} declared schemas nothing published reaches`);
  assert.deepEqual(declared, schemas, "in the spec's order");
});
