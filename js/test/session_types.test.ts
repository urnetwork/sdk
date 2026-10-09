import { test } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import ts from "typescript";

test("Go and OpenAPI auth-code arguments accept the same nullable request identity", () => {
  const filename = fileURLToPath(new URL("session_contract.fixture.ts", import.meta.url));
  const source = `
    import type { AuthCodeLoginArgs as GoArgs } from "../src/generated/types.js";
    import type { AuthCodeLoginArgs as WireArgs } from "../src/generated/openapi.js";
    const nullable: GoArgs = { auth_code: "code", request_id: null };
    const wire: WireArgs = nullable;
    const reverse: GoArgs = wire;
  `;
  const options: ts.CompilerOptions = {
    strict: true, noEmit: true, skipLibCheck: true,
    target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.NodeNext,
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
