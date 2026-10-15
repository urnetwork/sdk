import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { registerTsResolve } from "../../test/ts-resolve.ts";
import { startMockApi, type MockApi, type MockApiOptions } from "./mock_api.ts";

registerTsResolve();
const { main } = await import("../src/cli.ts");

// The command line's exit codes and output: a run in which no server opened
// is 1; Ctrl-C before any server was measured is 130; and the process exits
// as soon as it has printed.

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

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

const waitFor = async (predicate: () => boolean, what: string, timeoutMs = 30_000) => {
  const deadline = performance.now() + timeoutMs;
  while (!predicate()) {
    if (deadline < performance.now()) {
      throw new Error(`timed out waiting for ${what}`);
    }
    await sleep(20);
  }
};

test("a run in which no server opened exits 1, with the reason as JSON, and doesn't wait out its duration", async () => {
  // the one server's ticket points at a dead port, and there is no spare
  await withApi({ places: ["houston"], deadFirstTicket: true }, async (api) => {
    for (const command of ["latency", "bufferbloat"]) {
      const run = io();
      const started = performance.now();
      const code = await main([command, "--json", "--api", api.url, "--duration", "30", "--servers", "1", "--no-save", "-q"], run.io);
      assert.equal(code, 1, run.err.join(""));
      const failure = JSON.parse(run.out.join(""));
      assert.equal(failure.error, "unavailable");
      assert.match(failure.message, /^none of the 1 lag\.dev servers chosen could be reached: /);
      assert.ok(performance.now() - started < 20_000, `${command} took ${Math.round(performance.now() - started)} ms`);
    }
  });
});

test("Ctrl-C while the run is being set up asks for no track, and exits 130 as interrupted", async () => {
  // my-ip-info, the run's first call, takes a minute to answer
  await withApi({ places: ["houston"], myIpInfoDelayMs: 60_000 }, async (api) => {
    const controller = new AbortController();
    const run = io(controller.signal);
    const exited = main(["--json", "--api", api.url, "--duration", "1", "--servers", "1", "--no-save", "-q"], run.io);
    await waitFor(() => api.requests.some((request) => request.path === "/my-ip-info"), "the run to ask for my-ip-info");
    const interruptedAt = performance.now();
    controller.abort();
    assert.equal(await exited, 130);
    assert.ok(performance.now() - interruptedAt < 10_000, "it stopped waiting for my-ip-info");
    assert.deepEqual(JSON.parse(run.out.join("")), {
      error: "interrupted",
      message: "interrupted before any server was measured",
    });
    assert.deepEqual(api.requests.map((request) => request.path), ["/my-ip-info"], "nothing asked of the api after the interrupt");
    assert.equal(api.trackRequests.length, 0);
  });
});

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

test("the program's Ctrl-C during setup exits 130 with the interruption as JSON", { timeout: 120_000 }, async () => {
  await withApi({ places: ["houston"], myIpInfoDelayMs: 60_000 }, async (api) => {
    const { child, closed } = startCli(["--json", "--api", api.url, "--duration", "1", "--servers", "1", "--no-save", "-q"]);
    await waitFor(() => api.requests.some((request) => request.path === "/my-ip-info"), "the program to ask for my-ip-info", 60_000);
    const interruptedAt = performance.now();
    child.kill("SIGINT");
    const { code, stdout, stderr } = await closed;
    assert.equal(code, 130, stderr);
    assert.ok(performance.now() - interruptedAt < 30_000, "it stopped waiting for my-ip-info");
    assert.equal(JSON.parse(stdout).error, "interrupted");
    assert.match(stderr, /^lagdev: interrupted before any server was measured$/m);
    assert.equal(api.trackRequests.length, 0);
  });
});
