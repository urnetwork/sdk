import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

// gen_openapi.go against small fixture specs: shared parameters referenced as
// `#/components/parameters/<name>` resolve into the operation, and every
// other parameter $ref is refused with a message naming it.

const jsDir = fileURLToPath(new URL("..", import.meta.url));
const env = { ...process.env, GOWORK: "off", GOMAXPROCS: "2" };
let root = "";
let generator = "";
let goMissing = false;

before(() => {
  root = mkdtempSync(join(tmpdir(), "urnetwork-js-gen-openapi-"));
  generator = join(root, "gen-openapi");
  const result = spawnSync("go", ["build", "-mod=readonly", "-o", generator, "gen_openapi.go"], {
    cwd: jsDir, env, encoding: "utf8", timeout: 180000,
  });
  if ((result.error as NodeJS.ErrnoException | undefined)?.code === "ENOENT") {
    goMissing = true;
    return;
  }
  assert.equal(result.status, 0, `gen_openapi.go: ${result.error || ""}${result.stderr}`);
});
after(() => { if (root) rmSync(root, { recursive: true, force: true }); });

function spec(parameters: string, components: string) {
  return `openapi: "3.1.0"
info:
  title: fixture
  version: "1.0.0"
paths:
  /things:
    get:
      operationId: listThings
      security: []
      parameters:
${parameters}
      responses:
        "200":
          description: OK.
          content:
            application/json:
              schema:
                type: object
components:
  parameters:
${components}
  schemas: {}
`;
}

const domain = `    Domain:
      in: query
      name: domain
      required: true
      schema:
        type: string`;

function generate(t: { skip: (message: string) => void }, source: string) {
  if (goMissing) {
    t.skip("no go toolchain");
    return null;
  }
  const dir = mkdtempSync(join(root, "case-"));
  mkdirSync(join(dir, "src/generated"), { recursive: true });
  writeFileSync(join(dir, "spec.yml"), source);
  const result = spawnSync(generator, ["-spec", "spec.yml"], { cwd: dir, env, encoding: "utf8", timeout: 10000 });
  return { dir, result };
}

test("a components/parameters $ref resolves into the operation", t => {
  const run = generate(t, spec(`        - $ref: "#/components/parameters/Domain"`, domain));
  if (!run) return;
  assert.equal(run.result.status, 0, run.result.stderr);
  const client = readFileSync(join(run.dir, "src/generated/client.ts"), "utf8");
  assert.match(client, /path: "\/things", auth: "none", response: "json", query: \["domain"\]/);
  const types = readFileSync(join(run.dir, "src/generated/openapi.ts"), "utf8");
  assert.match(types, /^\s+domain: string;$/m, "a required shared query parameter stays required");
});

test("a JSON pointer escape in the component name resolves", t => {
  const slashed = `    a/b:
      in: query
      name: slashed
      schema:
        type: integer`;
  const run = generate(t, spec(`        - $ref: "#/components/parameters/a~1b"`, slashed));
  if (!run) return;
  assert.equal(run.result.status, 0, run.result.stderr);
  assert.match(readFileSync(join(run.dir, "src/generated/client.ts"), "utf8"), /query: \["slashed"\]/);
  assert.match(readFileSync(join(run.dir, "src/generated/openapi.ts"), "utf8"), /^\s+slashed\?: number;$/m);
});

test("an operation parameter overrides a referenced one with the same (in, name)", t => {
  const run = generate(t, spec(`        - $ref: "#/components/parameters/Domain"
        - in: query
          name: domain
          schema:
            type: integer`, domain));
  if (!run) return;
  assert.equal(run.result.status, 0, run.result.stderr);
  const types = readFileSync(join(run.dir, "src/generated/openapi.ts"), "utf8");
  assert.match(types, /^\s+domain\?: number;$/m);
  assert.doesNotMatch(types, /^\s+domain: string;$/m);
});

const refused: Array<[string, string, string, RegExp]> = [
  ["a $ref outside components/parameters", `        - $ref: "#/components/schemas/Domain"`, domain,
    /parameter \$ref "#\/components\/schemas\/Domain" is not supported/],
  ["a $ref that does not resolve", `        - $ref: "#/components/parameters/Missing"`, domain,
    /parameter \$ref "#\/components\/parameters\/Missing" does not resolve/],
  ["a $ref to another $ref", `        - $ref: "#/components/parameters/Alias"`, `${domain}
    Alias:
      $ref: "#/components/parameters/Domain"`,
    /parameter \$ref "#\/components\/parameters\/Alias" resolves to another \$ref/],
];

for (const [name, parameters, components, message] of refused) {
  test(`${name} is refused`, t => {
    const run = generate(t, spec(parameters, components));
    if (!run) return;
    assert.notEqual(run.result.status, 0);
    assert.match(run.result.stderr + run.result.stdout, message);
    assert.match(run.result.stderr + run.result.stdout, /GET \/things/, "the error names the operation");
  });
}

// The client is the public api, as ur.io publishes the spec: an operation
// marked x-internal (on itself or on its path item) is left out, and so is
// every schema nothing published reaches. A schema a published operation
// reaches in any way stays: through another schema, a component response or
// parameter, or a pointer spelled out in its prose.
function jsonContent(ref: string) {
  return `          content:
            application/json:
              schema:
                $ref: "#/components/schemas/${ref}"`;
}

function surfaceSpec(marker = "true", pathMarker = "true") {
  return `openapi: "3.1.0"
info:
  title: fixture
  version: "1.0.0"
paths:
  /public:
    get:
      operationId: getPublic
      description: "Its error is a #/components/schemas/ReachedThroughProse."
      security: []
      parameters:
        - $ref: "#/components/parameters/Cursor"
      responses:
        "200":
          description: OK.
${jsonContent("PublicResult")}
        "403":
          $ref: "#/components/responses/Refused"
  /mixed:
    get:
      operationId: getMixed
      security: []
      responses:
        "200":
          description: OK.
${jsonContent("Shared")}
    post:
      operationId: postMixed
      x-internal: ${marker}
      security: []
      requestBody:
        required: true
${jsonContent("InternalArgs").replace(/^ {2}/gm, "")}
      responses:
        "200":
          description: OK.
${jsonContent("InternalResult")}
  /internal-path:
    x-internal: ${pathMarker}
    parameters:
      - name: hidden
        in: query
        schema:
          $ref: "#/components/schemas/InternalPathParam"
    get:
      operationId: getInternalPath
      security: []
      responses:
        "200":
          description: OK.
${jsonContent("InternalPathResult")}
    post:
      operationId: postInternalPath
      security: []
      responses:
        "204":
          description: Done.
components:
  parameters:
    Cursor:
      in: query
      name: cursor
      schema:
        $ref: "#/components/schemas/CursorValue"
  responses:
    Refused:
      description: Refused.
${jsonContent("ReachedThroughResponse").replace(/^ {4}/gm, "")}
  schemas:
    PublicResult:
      type: object
      properties:
        shared:
          $ref: "#/components/schemas/Shared"
    Shared:
      type: object
      properties:
        nested:
          $ref: "#/components/schemas/SharedNested"
    SharedNested:
      type: object
      properties:
        a:
          type: string
    CursorValue:
      type: string
    ReachedThroughResponse:
      type: object
      properties:
        reason:
          type: string
    ReachedThroughProse:
      type: object
      properties:
        reason:
          type: string
    InternalArgs:
      type: object
      properties:
        nested:
          $ref: "#/components/schemas/InternalNested"
        shared:
          $ref: "#/components/schemas/Shared"
    InternalNested:
      type: object
      properties:
        secret_counter:
          type: integer
    InternalResult:
      type: object
      properties:
        ok:
          type: boolean
    InternalPathParam:
      type: string
    InternalPathResult:
      type: object
      properties:
        ok:
          type: boolean
    Unused:
      type: object
      properties:
        never:
          type: string
`;
}

const clientMethods = (client: string) => [...client.matchAll(/^ {2}(\w+)\(.*\): Promise</gm)].map(match => match[1]);
const declared = (types: string) =>
  [...types.matchAll(/^export (?:interface|type) (\w+)/gm)].map(match => match[1]).filter(name => !name.startsWith("Operation"));

test("x-internal operations, and the schemas only they reach, are left out of the client", t => {
  const run = generate(t, surfaceSpec());
  if (!run) return;
  assert.equal(run.result.status, 0, run.result.stderr);
  const client = readFileSync(join(run.dir, "src/generated/client.ts"), "utf8");
  const types = readFileSync(join(run.dir, "src/generated/openapi.ts"), "utf8");
  assert.deepEqual(clientMethods(client), ["getPublic", "getMixed"]);
  for (const hidden of ["postMixed", "getInternalPath", "postInternalPath", "/internal-path", "InternalArgs", "InternalResult", "InternalPathResult"]) {
    assert.ok(!client.includes(hidden), `client.ts names ${hidden}`);
    assert.ok(!types.includes(hidden), `openapi.ts names ${hidden}`);
  }
  assert.ok(!types.includes("secret_counter"), "a schema only an x-internal operation reaches, through another, is left out");
  assert.ok(!types.includes("hidden"), "a path-level parameter of an x-internal path is left out");
  assert.deepEqual(declared(types), [
    "PublicResult", "Shared", "SharedNested", "CursorValue", "ReachedThroughResponse", "ReachedThroughProse",
  ]);
  assert.match(run.result.stdout, /2 operations, 6 schemas; left out 3 x-internal operations and 6 schemas nothing published reaches/);
});

test("an x-internal: false operation is published, and a schema nothing reaches is not", t => {
  const run = generate(t, surfaceSpec("false", "false"));
  if (!run) return;
  assert.equal(run.result.status, 0, run.result.stderr);
  const client = readFileSync(join(run.dir, "src/generated/client.ts"), "utf8");
  assert.deepEqual(clientMethods(client), ["getPublic", "getMixed", "postMixed", "getInternalPath", "postInternalPath"]);
  const types = readFileSync(join(run.dir, "src/generated/openapi.ts"), "utf8");
  assert.ok(!declared(types).includes("Unused"), "a schema nothing reaches is left out");
  assert.ok(declared(types).includes("InternalPathParam"), "a published path's path-level parameter reaches its schema");
  assert.ok(declared(types).includes("InternalNested"), "a published operation reaches a schema through another");
});

for (const [where, marker, pathMarker, message] of [
  ["an operation", `"true"`, "true", /POST \/mixed: x-internal at line \d+ is not a boolean/],
  ["an operation", "yes", "true", /POST \/mixed: x-internal at line \d+ is not a boolean/],
  ["a path item", "true", `"true"`, /\/internal-path: x-internal at line \d+ is not a boolean/],
  ["a path item", "true", "[true]", /\/internal-path: x-internal at line \d+ is not a boolean/],
] as const) {
  test(`a non-boolean x-internal (${where === "a path item" ? pathMarker : marker}) on ${where} is refused`, t => {
    const run = generate(t, surfaceSpec(marker, pathMarker));
    if (!run) return;
    assert.notEqual(run.result.status, 0, "an unreadable marker must not publish the route");
    assert.match(run.result.stderr + run.result.stdout, message);
  });
}
