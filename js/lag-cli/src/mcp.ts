// lag.dev as an MCP server over stdio, for coding and desktop agents
// (lag.dev/PLAN.md amendment 12, item 17). It runs where the agent runs, so
// it measures that machine's network; a remote server would measure its own.
//
// Two tools: measure_latency, cheap and capped in time, and
// measure_bufferbloat, which moves real data and so takes explicit caps.
// One measurement runs at a time: two at once would load the same link and
// spoil each other.

import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";
import { measureBufferbloat, measureLatency, MeasureError, type MeasureContext, type Measurement } from "./measure";
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

/** The MCP server, measuring through `context`. */
export function createLagdevMcpServer(context: MeasureContext): McpServer {
  const server = new McpServer({ name: "lagdev", version: VERSION });
  let queue: Promise<unknown> = Promise.resolve();
  // one measurement at a time
  const serially = <T>(run: () => Promise<T>): Promise<T> => {
    const next = queue.then(run, run);
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
      annotations: { readOnlyHint: true, openWorldHint: true, idempotentHint: false },
    },
    async ({ duration_seconds, servers }) => {
      try {
        const result: Measurement = await serially(() =>
          measureLatency(context, { durationMs: (duration_seconds ?? 15) * 1000, servers }),
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
      annotations: { readOnlyHint: true, openWorldHint: true, idempotentHint: false },
    },
    async ({ max_bytes, max_seconds, servers }) => {
      try {
        const result = await serially(() =>
          measureBufferbloat(context, { maxBytes: max_bytes, maxMs: max_seconds * 1000, servers }),
        );
        return asText(result);
      } catch (error) {
        return failure(error);
      }
    },
  );

  return server;
}

/** Serve the tools on stdin and stdout until the client goes away. */
export async function serveMcp(context: MeasureContext): Promise<void> {
  const server = createLagdevMcpServer(context);
  const transport = new StdioServerTransport();
  const closed = new Promise<void>((resolve) => {
    transport.onclose = () => resolve();
  });
  await server.connect(transport);
  await closed;
}
