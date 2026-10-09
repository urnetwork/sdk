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
