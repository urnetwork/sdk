// lag.dev as an MCP server over stdio, for coding and desktop agents
// (lag.dev/PLAN.md amendment 12, item 17). It runs where the agent runs, so
// it measures that machine's network; a remote server would measure its own.
//
// Two tools: measure_latency, cheap and capped in time, and
// measure_bufferbloat, which moves real data and so takes explicit caps.
// One measurement runs at a time: two at once would load the same link and
// spoil each other.
//
// A measurement stops when its request is cancelled, which is also what a
// client does when it gives up waiting, and when the client goes away. Each
// tool reports its progress to a client that asks for it, so a client can
// show the run, and one that resets its timeout on progress needn't give up
// on a long one.

import type { Readable, Writable } from "node:stream";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import type { RequestHandlerExtra } from "@modelcontextprotocol/sdk/shared/protocol.js";
import type { ServerNotification, ServerRequest } from "@modelcontextprotocol/sdk/types.js";
import { z } from "zod";
import {
  measureBufferbloat,
  measureLatency,
  MeasureError,
  type MeasureContext,
  type MeasureProgress,
  type Measurement,
} from "./measure";
import { VERSION } from "./version";

const LATENCY_DESCRIPTION =
  "Measure the latency of the network this machine is on: ping up to 8 lag.dev servers around the world " +
  "(3 near, the rest far) for a few seconds, with no load. Uses well under 1 MB. Returns, per server and overall, " +
  "the idle round trip p50 and p95 in ms, lost pings, and RPM (round trips per minute) with its band from the " +
  "IETF responsiveness draft. Nothing about this machine's address is stored.";

const BUFFERBLOAT_DESCRIPTION =
  "Measure bufferbloat (latency under load) of the network this machine is on: after 5 s of idle pings to up to " +
  "8 lag.dev servers, load each in turn with a 5 s download and then a 5 s upload over 4 streams, while pinging. " +
  "This moves real data: about 5 MB per Mbps of the link's combined download and upload speed for a full run of " +
  "8 servers (about 600 MB on a 100/20 Mbps line), and about 100 s. max_bytes and max_seconds cap it and it stops " +
  "early at either. Returns per server the idle p50, download and upload p50 in Mbps, how much each direction's " +
  "load raised the round trip (ms), and loaded RPM, plus overall medians and the bytes moved.";

// Neither tool only reads: each makes a lag.dev guest and a track, and the
// servers keep what they measured (A1, A5, A6), and measure_bufferbloat moves
// up to max_bytes over the line. Neither destroys anything: what they make is
// new, so each call adds to what was there.
const ANNOTATIONS = { readOnlyHint: false, destructiveHint: false, openWorldHint: true, idempotentHint: false };

const asText = (value: unknown) => ({ content: [{ type: "text" as const, text: JSON.stringify(value, null, 2) }] });

const failure = (error: unknown) => ({
  isError: true,
  content: [
    {
      type: "text" as const,
      text: JSON.stringify({
        error: error instanceof MeasureError ? error.code : "failed",
        message: error instanceof Error ? error.message : String(error),
        ...(error instanceof MeasureError && error.retryAfterMs ? { retryAfterMs: error.retryAfterMs } : {}),
      }),
    },
  ],
});

type Extra = RequestHandlerExtra<ServerRequest, ServerNotification>;

// notifications/progress for a request that asked for them by carrying a
// progressToken, one for each step of the run
const progressFor = (extra: Extra): ((progress: MeasureProgress) => void) | undefined => {
  const progressToken = extra._meta?.progressToken;
  if (progressToken === undefined) {
    return undefined;
  }
  return ({ progress, total, message }) => {
    extra
      .sendNotification({
        method: "notifications/progress",
        params: { progressToken, progress, ...(total === undefined ? {} : { total }), message },
      })
      .catch(() => undefined);
  };
};

// The server, and a promise of the end of the measurements it has started.
function lagdevMcpServer(context: MeasureContext): { server: McpServer; settled(): Promise<void> } {
  const server = new McpServer({ name: "lagdev", version: VERSION });
  let queue: Promise<unknown> = Promise.resolve();
  // One measurement at a time. A request cancelled while it waited never
  // starts, so a client's retries can't queue up runs nobody waits for.
  const serially = <T>(signal: AbortSignal, run: () => Promise<T>): Promise<T> => {
    const start = () =>
      signal.aborted ? Promise.reject(new MeasureError("interrupted", "the request was cancelled before it ran")) : run();
    const next = queue.then(start, start);
    queue = next.catch(() => undefined);
    return next;
  };

  server.registerTool(
    "measure_latency",
    {
      title: "Measure latency (lag.dev)",
      description: LATENCY_DESCRIPTION,
      inputSchema: {
        duration_seconds: z.number().int().min(3).max(60).optional().describe("how long to ping, 3 to 60 s (default 15)"),
        servers: z.number().int().min(1).max(8).optional().describe("how many servers, 1 to 8 (default 8)"),
      },
      annotations: ANNOTATIONS,
    },
    async ({ duration_seconds, servers }, extra) => {
      try {
        const result: Measurement = await serially(extra.signal, () =>
          measureLatency(context, {
            durationMs: (duration_seconds ?? 15) * 1000,
            servers,
            client: "mcp",
            signal: extra.signal,
            onProgress: progressFor(extra),
          }),
        );
        return asText(result);
      } catch (error) {
        return failure(error);
      }
    },
  );

  server.registerTool(
    "measure_bufferbloat",
    {
      title: "Measure bufferbloat (lag.dev)",
      description: BUFFERBLOAT_DESCRIPTION,
      inputSchema: {
        max_bytes: z
          .number()
          .int()
          .min(1_000_000)
          .max(20_000_000_000)
          .describe("stop before moving more than this many bytes, e.g. 500000000 for 500 MB"),
        max_seconds: z.number().int().min(20).max(600).describe("stop before running longer than this, 20 to 600 s"),
        servers: z.number().int().min(1).max(8).optional().describe("how many servers to load, 1 to 8 (default 8)"),
      },
      annotations: ANNOTATIONS,
    },
    async ({ max_bytes, max_seconds, servers }, extra) => {
      try {
        const result = await serially(extra.signal, () =>
          measureBufferbloat(context, {
            maxBytes: max_bytes,
            maxMs: max_seconds * 1000,
            servers,
            client: "mcp",
            signal: extra.signal,
            onProgress: progressFor(extra),
          }),
        );
        return asText(result);
      } catch (error) {
        return failure(error);
      }
    },
  );

  return { server, settled: () => queue.then(() => undefined) };
}

/** The MCP server, measuring through `context`. */
export function createLagdevMcpServer(context: MeasureContext): McpServer {
  return lagdevMcpServer(context).server;
}

/**
 * Serve the tools on stdin and stdout until the client goes away: it closes
 * the connection, or its end of stdin. A measurement still running then stops
 * as a cancelled one does, and this returns once it has closed its sessions.
 */
export async function serveMcp(
  context: MeasureContext,
  stdio: { stdin?: Readable; stdout?: Writable } = {},
): Promise<void> {
  const stdin = stdio.stdin ?? process.stdin;
  const { server, settled } = lagdevMcpServer(context);
  const transport = new StdioServerTransport(stdin, stdio.stdout ?? process.stdout);
  const closed = new Promise<void>((resolve) => {
    transport.onclose = () => resolve();
  });
  // The transport doesn't notice stdin ending, which is how a stdio client
  // goes away. Closed then, it aborts every request in flight, so a
  // measurement stops rather than load the line for nobody.
  let closing = false;
  const close = () => {
    if (!closing) {
      closing = true;
      transport.close().catch(() => undefined);
    }
  };
  stdin.once("end", close);
  stdin.once("close", close);
  try {
    await server.connect(transport);
    await closed;
  } finally {
    closing = true;
    stdin.off("end", close);
    stdin.off("close", close);
  }
  await settled();
}
