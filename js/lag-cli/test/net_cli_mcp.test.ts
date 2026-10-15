import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer, type IncomingHttpHeaders } from "node:http";
import type { AddressInfo } from "node:net";
import { registerTsResolve } from "../../test/ts-resolve.ts";
import { startMockApi } from "./mock_api.ts";

registerTsResolve();
const { createNet } = await import("../src/net.ts");
const { createContext } = await import("../src/context.ts");
const { createLagdevMcpServer } = await import("../src/mcp.ts");
const { main } = await import("../src/cli.ts");
const { measureBufferbloat } = await import("../src/measure.ts");
const { Client } = await import("@modelcontextprotocol/sdk/client/index.js");
const { InMemoryTransport } = await import("@modelcontextprotocol/sdk/inMemory.js");

// One local server that records what reached it.
const recorder = async () => {
  const seen: Array<{ url: string; headers: IncomingHttpHeaders; upgrade: boolean }> = [];
  const server = createServer((request, response) => {
    seen.push({ url: request.url ?? "", headers: request.headers, upgrade: false });
    response.end("ok");
  });
  server.on("upgrade", (request, socket) => {
    seen.push({ url: request.url ?? "", headers: request.headers, upgrade: true });
    socket.destroy();
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as AddressInfo).port;
  return {
    seen,
    port,
    close: () =>
      new Promise<void>((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
      }),
  };
};

test("measurement requests carry lag.dev's origin, the api's none, and connect-to keeps the name", async () => {
  const server = await recorder();
  // the names don't resolve; connect-to sends every connection to the recorder
  const net = createNet({ connectTo: { host: "127.0.0.1", port: server.port } });
  try {
    assert.equal(await (await net.apiFetch("http://api.example.invalid/my-ip-info")).text(), "ok");
    await (await net.measureFetch("http://x1.extender.example.invalid/lag/v1/down?stream=0")).text();
    await new Promise<void>((resolve) => {
      const socket = new net.WebSocket("ws://x1.extender.example.invalid/lag/v1/session?ticket=t");
      socket.addEventListener("close", () => resolve());
      socket.addEventListener("error", () => undefined);
    });
    const [api, download, session] = server.seen;
    assert.equal(api.headers.host, "api.example.invalid");
    assert.equal(api.headers.origin, undefined);
    assert.equal(download.headers.host, "x1.extender.example.invalid");
    assert.equal(download.headers.origin, "https://lag.dev");
    assert.equal(session.upgrade, true);
    assert.equal(session.headers.origin, "https://lag.dev");
    assert.equal(session.url, "/lag/v1/session?ticket=t");
  } finally {
    await net.close();
    await server.close();
  }
});

const io = () => {
  const out: string[] = [];
  const err: string[] = [];
  return {
    out,
    err,
    io: {
      stdout: { write: (text: string) => out.push(text) },
      stderr: { write: (text: string) => err.push(text) },
      env: {},
    },
  };
};

test("the cli prints a latency run as JSON and refuses bad arguments", async () => {
  const api = await startMockApi({ places: ["houston", "london"] });
  try {
    const run = io();
    const code = await main(["latency", "--json", "--api", api.url, "--duration", "1", "--servers", "2", "--no-save", "-q"], run.io);
    assert.equal(code, 0, run.err.join(""));
    const result = JSON.parse(run.out.join(""));
    assert.equal(result.tool, "lagdev");
    assert.equal(result.mode, "latency");
    assert.equal(result.servers.length, 2);
    assert.deepEqual(run.err, [], "quiet means no progress");

    const text = io();
    assert.equal(await main(["--api", api.url, "--duration", "1", "--servers", "1", "--no-save"], text.io), 0);
    assert.match(text.out.join(""), /^lag\.dev latency · from Austin/);
    assert.ok(text.err.some((line) => line.startsWith("lagdev: pinging")), "progress goes to stderr");
  } finally {
    await api.close();
  }
  const refusing = await startMockApi({
    places: ["houston"],
    refuseSession: { code: "limit", message: "too many guests from this network", retry_after_ms: 60_000 },
  });
  try {
    const run = io();
    assert.equal(await main(["--json", "--api", refusing.url, "--no-save", "-q"], run.io), 1);
    assert.deepEqual(JSON.parse(run.out.join("")), {
      error: "limit",
      message: "lag.dev couldn't start a guest session: too many guests from this network (try again in 60 s)",
      retryAfterMs: 60_000,
    });
  } finally {
    await refusing.close();
  }
  for (const args of [["--servers", "9"], ["bufferbloat", "--max-bytes", "lots"], ["frobnicate"], ["--duration", "-1"], ["--stack", "/nonexistent.json"]]) {
    const run = io();
    const code = await main(args, run.io);
    assert.equal(code, 2, args.join(" "));
    assert.ok(run.err.join("").startsWith("lagdev: "));
  }
});

test("the mcp server offers both tools, and bufferbloat requires its caps", async () => {
  const api = await startMockApi({ places: ["houston", "london"] });
  const local = createContext({ apiURL: api.url, save: false });
  const server = createLagdevMcpServer(local.context);
  const client = new Client({ name: "test", version: "0" });
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
  try {
    await Promise.all([server.connect(serverTransport), client.connect(clientTransport)]);
    const { tools } = await client.listTools();
    const byName = new Map(tools.map((tool) => [tool.name, tool]));
    assert.deepEqual([...byName.keys()].sort(), ["measure_bufferbloat", "measure_latency"]);
    assert.deepEqual(byName.get("measure_bufferbloat")!.inputSchema.required?.sort(), ["max_bytes", "max_seconds"]);
    assert.equal(byName.get("measure_latency")!.inputSchema.required, undefined);
    assert.match(byName.get("measure_bufferbloat")!.description ?? "", /5 MB per Mbps/);

    const call = await client.callTool({ name: "measure_latency", arguments: { duration_seconds: 3, servers: 2 } });
    assert.notEqual(call.isError, true);
    const result = JSON.parse((call.content as Array<{ text: string }>)[0].text);
    assert.equal(result.mode, "latency");
    assert.equal(result.servers.length, 2);
    assert.ok(result.servers.every((s: { pings: number }) => s.pings > 0));

    const refused = await client.callTool({ name: "measure_bufferbloat", arguments: { max_seconds: 60 } });
    assert.equal(refused.isError, true);
  } finally {
    await client.close();
    await server.close();
    await local.close();
    await api.close();
  }
});

test("the track request names the tool that ran it, cli or mcp, and nothing else does", async () => {
  const api = await startMockApi({ places: ["houston"], relay: true });
  const local = createContext({ apiURL: api.url, save: false });
  const server = createLagdevMcpServer(local.context);
  const client = new Client({ name: "test", version: "0" });
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
  try {
    const run = io();
    const code = await main(["--json", "--api", api.url, "--duration", "1", "--servers", "1", "--no-save", "-q"], run.io);
    assert.equal(code, 0, run.err.join(""));
    await Promise.all([server.connect(serverTransport), client.connect(clientTransport)]);
    const call = await client.callTool({ name: "measure_latency", arguments: { duration_seconds: 3, servers: 1 } });
    assert.notEqual(call.isError, true);
    // a load too, so downloads and uploads go through the relay
    const loaded = await measureBufferbloat(local.context, {
      maxBytes: 20e9,
      maxMs: 60_000,
      servers: 1,
      idleMs: 300,
      settleMs: 100,
      client: "cli",
    });
    assert.equal(loaded.servers[0].load?.status, "complete");

    const named = api.requests.filter((request) => request.client !== undefined);
    assert.deepEqual(
      named.map((request) => `${request.method} ${request.path} ${request.client}`),
      ["POST /lag/track cli", "POST /lag/track mcp", "POST /lag/track cli"],
    );
    assert.equal(api.requests.filter((request) => request.path === "/lag/track").length, 3);
    // the guest, the sample and my-ip-info carry none; nor does anything sent
    // to the extender: its sessions, downloads and uploads
    assert.ok(api.extenderTraffic.requests >= 3 + 4 + 4, `${api.extenderTraffic.requests} requests reached the extender`);
    assert.equal(api.extenderTraffic.lagClient, 0);
  } finally {
    await client.close();
    await server.close();
    await local.close();
    await api.close();
  }
});
