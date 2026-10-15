import { test } from "node:test";
import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { registerTsResolve } from "../../test/ts-resolve.ts";
import { startMockApi, type MockApi, type MockApiOptions } from "./mock_api.ts";

registerTsResolve();
const { createContext } = await import("../src/context.ts");
const { createLagdevMcpServer, serveMcp } = await import("../src/mcp.ts");
const { Client } = await import("@modelcontextprotocol/sdk/client/index.js");
const { InMemoryTransport } = await import("@modelcontextprotocol/sdk/inMemory.js");
const { LATEST_PROTOCOL_VERSION } = await import("@modelcontextprotocol/sdk/types.js");

// The MCP server's tools as a client sees them: what they declare, how a
// cancelled call or a client that goes away stops the measurement, the
// progress a call can ask for, and a run with no server to measure.

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

// A client connected to the server, measuring the mock api's extenders.
const withServer = async (
  options: MockApiOptions,
  body: (client: InstanceType<typeof Client>, api: MockApi) => Promise<void>,
) => {
  const api = await startMockApi(options);
  const local = createContext({ apiURL: api.url, save: false });
  const server = createLagdevMcpServer(local.context);
  const client = new Client({ name: "test", version: "0" });
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
  try {
    await Promise.all([server.connect(serverTransport), client.connect(clientTransport)]);
    await body(client, api);
  } finally {
    await client.close();
    await server.close();
    await local.close();
    await api.close();
  }
};

const sessionsOf = (api: MockApi) => [...api.extenders.values()].flatMap((extender) => extender.sessions);
const loadsOf = (api: MockApi) => sessionsOf(api).filter((session) => session.loadRequests > 0).length;
const openSessionsOf = (api: MockApi) => [...api.extenders.values()].reduce((open, extender) => open + extender.openSessions(), 0);

test("neither tool says it only reads or that it destroys", async () => {
  await withServer({ places: ["houston"] }, async (client) => {
    const { tools } = await client.listTools();
    assert.equal(tools.length, 2);
    for (const tool of tools) {
      // each makes a guest and a track, and bufferbloat moves real data
      assert.equal(tool.annotations?.readOnlyHint, false, tool.name);
      assert.equal(tool.annotations?.destructiveHint, false, tool.name);
      assert.equal(tool.annotations?.openWorldHint, true, tool.name);
      assert.equal(tool.annotations?.idempotentHint, false, tool.name);
    }
  });
});

test("a call the client gives up on stops its measurement, and the next call doesn't wait behind it (the reviewer's demo)", { timeout: 120_000 }, async () => {
  await withServer({ places: ["houston", "london"] }, async (client, api) => {
    // the client gives up after 3 s, as one with a short request timeout
    // would, and sends notifications/cancelled; the run is then pinging
    // before its loads, or still setting up
    await assert.rejects(
      client.callTool({ name: "measure_bufferbloat", arguments: { max_bytes: 20_000_000_000, max_seconds: 600 } }, undefined, {
        timeout: 3000,
      }),
      /timed out|timeout/i,
    );
    await waitFor(() => openSessionsOf(api) === 0, "the cancelled run to close its sessions", 10_000);
    const tracks = api.trackRequests.length;
    const started = performance.now();
    const next = await client.callTool({ name: "measure_latency", arguments: { duration_seconds: 3, servers: 1 } });
    const waited = performance.now() - started;
    assert.notEqual(next.isError, true, JSON.stringify(next.content));
    // a full run behind it would have been some 15 s more
    assert.ok(waited < 12_000, `the next 3 s call took ${Math.round(waited)} ms`);
    assert.equal(api.trackRequests.length, tracks + 1);
    // the cancelled run never loaded either server, then or since
    assert.equal(loadsOf(api), 0);
  });
});

test("a call cancelled during a load cuts that load and loads no other server", { timeout: 120_000 }, async () => {
  await withServer({ places: ["houston", "london"] }, async (client, api) => {
    const controller = new AbortController();
    const call = client.callTool(
      { name: "measure_bufferbloat", arguments: { max_bytes: 20_000_000_000, max_seconds: 600, servers: 2 } },
      undefined,
      { signal: controller.signal, timeout: 120_000 },
    );
    call.catch(() => undefined);
    await waitFor(() => loadsOf(api) === 1, "the first load to start", 60_000);
    controller.abort();
    await assert.rejects(call);
    await waitFor(() => openSessionsOf(api) === 0, "the cancelled run to close its sessions", 10_000);
    // what a full run would have done by now: its second server's load
    await sleep(6000);
    assert.equal(loadsOf(api), 1, "a load ran after the call was cancelled");
  });
});

test("a call that asks for progress hears of each step: connecting, the idle start and each load", { timeout: 120_000 }, async () => {
  await withServer({ places: ["houston"] }, async (client) => {
    const heard: Array<{ progress: number; total?: number; message?: string }> = [];
    const result = await client.callTool(
      { name: "measure_bufferbloat", arguments: { max_bytes: 20_000_000_000, max_seconds: 600, servers: 1 } },
      undefined,
      { onprogress: (progress) => heard.push(progress), timeout: 120_000 },
    );
    assert.notEqual(result.isError, true, JSON.stringify(result.content));
    assert.deepEqual(heard, [
      { progress: 1, message: "connecting to 1 servers" },
      { progress: 2, total: 3, message: "pinging for 5 s before the loads" },
      { progress: 3, total: 3, message: "loading Houston, US (1 of 1)" },
    ]);

    const latency: typeof heard = [];
    const pinged = await client.callTool(
      { name: "measure_latency", arguments: { duration_seconds: 6, servers: 1 } },
      undefined,
      { onprogress: (progress) => latency.push(progress), timeout: 120_000 },
    );
    assert.notEqual(pinged.isError, true, JSON.stringify(pinged.content));
    assert.deepEqual(latency, [
      { progress: 1, message: "connecting to 1 servers" },
      { progress: 2, total: 3, message: "pinging 1 servers for 6 s" },
      { progress: 3, total: 3, message: "pinged 1 servers for 5 of 6 s" },
    ]);
  });
});

test("a run in which no server opened is an error result", async () => {
  await withServer({ places: ["houston"], deadFirstTicket: true }, async (client) => {
    const result = await client.callTool({ name: "measure_latency", arguments: { duration_seconds: 30, servers: 1 } });
    assert.equal(result.isError, true);
    const failure = JSON.parse((result.content as Array<{ text: string }>)[0].text);
    assert.equal(failure.error, "unavailable");
    assert.match(failure.message, /^none of the 1 lag\.dev servers chosen could be reached: /);
  });
});

test("a client that closes stdin stops the measurement, and the server returns", { timeout: 120_000 }, async () => {
  const api = await startMockApi({ places: ["houston", "london"] });
  const local = createContext({ apiURL: api.url, save: false });
  const stdin = new PassThrough();
  const stdout = new PassThrough();
  const lines: Array<Record<string, unknown>> = [];
  let buffered = "";
  stdout.on("data", (chunk: Buffer) => {
    buffered += chunk.toString("utf8");
    for (let end = buffered.indexOf("\n"); end !== -1; end = buffered.indexOf("\n")) {
      lines.push(JSON.parse(buffered.slice(0, end)));
      buffered = buffered.slice(end + 1);
    }
  });
  const send = (message: object) => stdin.write(`${JSON.stringify({ jsonrpc: "2.0", ...message })}\n`);
  try {
    const served = serveMcp(local.context, { stdin, stdout });
    send({ id: 1, method: "initialize", params: { protocolVersion: LATEST_PROTOCOL_VERSION, capabilities: {}, clientInfo: { name: "test", version: "0" } } });
    await waitFor(() => lines.some((line) => line.id === 1), "the answer to initialize");
    send({ method: "notifications/initialized" });
    send({ id: 2, method: "tools/call", params: { name: "measure_bufferbloat", arguments: { max_bytes: 20_000_000_000, max_seconds: 600 } } });
    // the run is pinging both servers before its loads
    await waitFor(() => [...api.extenders.values()].every((extender) => extender.openSessions() === 1), "the run's sessions to open", 60_000);
    stdin.end();
    await served;
    // closed on this side when it returns; the extenders hear of it a moment later
    await waitFor(() => openSessionsOf(api) === 0, "the extenders to see the run's sessions close", 10_000);
    await sleep(6000);
    assert.equal(loadsOf(api), 0, "a load ran after the client went away");
    assert.ok(!lines.some((line) => line.id === 2), "a call for a client that went away isn't answered");
  } finally {
    await local.close();
    await api.close();
  }
});
