// Public policy and status routes must survive schema edits with their own
// response contracts in both committed OpenAPI outputs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import ts from "typescript";
import { registerTsResolve } from "./ts-resolve.ts";

registerTsResolve();
const { createURNetworkApiClient } = await import("../src/client.ts");

function recordingClient(response: Response) {
  const calls: Array<{ url: string; method: string; authorization: string | null; body: unknown }> = [];
  const client = createURNetworkApiClient({
    baseURL: "https://api.example",
    token: "synthetic-token",
    retry: false,
    fetch: async (input, init) => {
      calls.push({
        url: String(input),
        method: init?.method ?? "GET",
        authorization: new Headers(init?.headers).get("authorization"),
        body: init?.body,
      });
      return response;
    },
  });
  return { client, calls };
}

for (const [method, path] of [
  ["privacyTxt", "/privacy.txt"],
  ["termsTxt", "/terms.txt"],
  ["vdpTxt", "/vdp.txt"],
] as const) {
  test(`public policy ${method} keeps its bodyless response contract`, async () => {
    // Fetch follows the redirect; the resulting document is not status JSON.
    const { client, calls } = recordingClient(new Response("Synthetic public policy.\n", {
      headers: { "Content-Type": "text/plain" },
    }));
    assert.equal(typeof client[method], "function", `${method} is missing from the generated client`);
    assert.equal(await client[method](), undefined);
    assert.deepEqual(calls, [{ url: `https://api.example${path}`, method: "GET", authorization: null, body: undefined }]);
  });
}

test("public status returns its status JSON from the status route", async () => {
  const status = {
    status: "ok", client_address: "192.0.2.1", host: "synthetic-host",
    service: "synthetic-service", block: "synthetic-block", version: "synthetic-version",
  };
  const { client, calls } = recordingClient(new Response(JSON.stringify(status), {
    headers: { "Content-Type": "application/json" },
  }));
  assert.equal(typeof client.warpStatus, "function", "warpStatus is missing from the generated client");
  assert.deepEqual(await client.warpStatus(), status);
  assert.deepEqual(calls, [{ url: "https://api.example/status", method: "GET", authorization: null, body: undefined }]);
});

test("public operation declarations distinguish policy redirects from status JSON", () => {
  const filename = fileURLToPath(new URL("openapi_public_routes.fixture.ts", import.meta.url));
  const source = `
    import type { URNetworkApiClient } from "../src/client";
    import type { Operations, WarpStatusResult } from "../src/generated/openapi";
    declare const client: URNetworkApiClient;
    declare const operations: Operations;
    const privacy: Promise<void> = client.privacyTxt();
    const terms: Promise<void> = client.termsTxt();
    const vdp: Promise<void> = client.vdpTxt();
    const status: Promise<WarpStatusResult> = client.warpStatus();
    const policyResponses: void[] = [
      operations.privacyTxt.response, operations.termsTxt.response, operations.vdpTxt.response,
    ];
    const statusResponse: WarpStatusResult = operations.warpStatus.response;
  `;
  const options: ts.CompilerOptions = {
    strict: true, noEmit: true, skipLibCheck: true,
    target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext,
    moduleResolution: ts.ModuleResolutionKind.Bundler,
  };
  const host = ts.createCompilerHost(options);
  const read = host.getSourceFile.bind(host);
  host.getSourceFile = (name, languageVersion, onError, shouldCreateNewSourceFile) =>
    name === filename ? ts.createSourceFile(name, source, languageVersion) :
      read(name, languageVersion, onError, shouldCreateNewSourceFile);
  const program = ts.createProgram([filename], options, host);
  const diagnostics = ts.getPreEmitDiagnostics(program);
  assert.deepEqual(diagnostics.map(d => ts.flattenDiagnosticMessageText(d.messageText, "\n")), []);
});
