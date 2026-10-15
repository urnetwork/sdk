import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { registerTsResolve } from "../../test/ts-resolve.ts";
import { startMockApi, type MockApi, type MockApiOptions } from "./mock_api.ts";

registerTsResolve();
const { main } = await import("../src/cli.ts");

// The command line's exit codes and output: a run in which no server opened
// is 1, and the process exits as soon as it has printed.

const io = (signal?: AbortSignal) => {
  const out: string[] = [];
  const err: string[] = [];
  return {
    out,
    err,
    io: {
      stdout: { write: (text: string) => out.push(text) },
      stderr: { write: (text: string) => err.push(text) },
      env: {},
      signal,
    },
  };
};

const withApi = async (options: MockApiOptions, body: (api: MockApi) => Promise<void>) => {
  const api = await startMockApi(options);
  try {
    await body(api);
  } finally {
    await api.close();
  }
};

// The CLI as a program, from its sources, with the test's mock api.
const CLI = fileURLToPath(new URL("../src/cli.ts", import.meta.url));
const REGISTER = fileURLToPath(new URL("./register_ts.ts", import.meta.url));
const EXIT_GAP = fileURLToPath(new URL("./exit_gap.ts", import.meta.url));

const startCli = (args: string[], imports: string[] = []) => {
  const child = spawn(
    process.execPath,
    [...[REGISTER, ...imports].flatMap((path) => ["--import", path]), CLI, ...args],
    { stdio: ["ignore", "pipe", "pipe"] },
  );
  let stdout = "";
  let stderr = "";
  child.stdout.on("data", (chunk) => {
    stdout += chunk;
  });
  child.stderr.on("data", (chunk) => {
    stderr += chunk;
  });
  const closed = new Promise<{ code: number | null; stdout: string; stderr: string }>((resolve) => {
    child.on("close", (code) => resolve({ code, stdout, stderr }));
  });
  return { child, closed };
};

test("the program exits as soon as it has printed its result", { timeout: 120_000 }, async () => {
  await withApi({ places: ["houston"] }, async (api) => {
    const { closed } = startCli(["--json", "--api", api.url, "--duration", "1", "--servers", "1", "--no-save", "-q"], [EXIT_GAP]);
    const { code, stdout, stderr } = await closed;
    assert.equal(code, 0, stderr);
    assert.equal(JSON.parse(stdout).mode, "latency");
    const gap = Number(/lagdev-test-exit-gap (\d+)/.exec(stderr)?.[1]);
    // the sessions' close used to leave a 3 s timer that held the process
    assert.ok(gap < 1500, `exited ${gap} ms after printing`);
  });
});
