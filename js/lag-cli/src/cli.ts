// lagdev: lag.dev from the command line (lag.dev/PLAN.md amendment 12, item 17).
//
//   lagdev                      latency to up to 8 servers, 15 s, no load
//   lagdev bufferbloat          loads each server in turn, within caps
//   lagdev mcp                  the same as MCP tools over stdio
//
// It talks to production (https://api.bringyour.com) unless told otherwise;
// --stack points it at a local test stack under the production names.

import { readFileSync, realpathSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";
import { DEFAULT_API_BASE_URL } from "../../src/api_client";
import { createContext } from "./context";
import { formatMeasurement } from "./format";
import { MeasureError, measureBufferbloat, measureLatency, type Measurement } from "./measure";
import { serveMcp } from "./mcp";
import { LAGDEV_ORIGIN, parseHostPort } from "./net";
import { VERSION } from "./version";

export interface Io {
  stdout: { write(text: string): unknown };
  stderr: { write(text: string): unknown; isTTY?: boolean };
  env: NodeJS.ProcessEnv;
  /** aborted on Ctrl-C (default: wired to SIGINT here) */
  signal?: AbortSignal;
}

export const USAGE = `lagdev ${VERSION}: measure the latency, bufferbloat and stability of the network this machine is on, with lag.dev's servers.

Usage:
  lagdev [latency] [options]      ping up to 8 servers with no load (default 15 s)
  lagdev bufferbloat [options]    then load each in turn: a 5 s download, a 5 s upload
  lagdev mcp [options]            serve measure_latency and measure_bufferbloat over MCP stdio

Options:
  --json                 print the result as JSON
  --duration <s>         latency: how long to ping (default 15)
  --servers <n>          how many servers, 1 to 8 (default 8)
  --max-bytes <size>     bufferbloat: stop before moving more, e.g. 500MB, 2GB (default 500MB)
  --max-seconds <s>      bufferbloat: stop before running longer (default 180)
  --api <url>            the api (default ${DEFAULT_API_BASE_URL})
  --stack <stack.json>   a local lag.dev test stack (lag.dev/e2e/stack): its api, front and CA
  --ca <file>            also trust this root, PEM
  --connect-to <h:p>     send every connection to this address, keeping TLS names
  --origin <origin>      the page origin measurement requests carry (default ${LAGDEV_ORIGIN})
  --no-save              don't keep the guest session between runs
  --config-dir <dir>     where the guest session is kept (default ~/.config/lagdev)
  -q, --quiet            no progress on stderr
  -h, --help             this help
  -v, --version          the version

A bufferbloat run moves real data: about 5 MB per Mbps of your combined download
and upload speed for 8 servers (about 600 MB at 100/20 Mbps). Nothing about this
machine's address is stored; the guest session keeps only its network and city.
`;

class UsageError extends Error {}

/** "500MB", "2 GB", "1.5GiB" or a byte count, as bytes. MB and GB are decimal. */
export function parseBytes(text: string): number {
  const match = /^\s*(\d+(?:\.\d+)?)\s*(b|kb|mb|gb|tb|kib|mib|gib|tib)?\s*$/i.exec(text);
  if (!match) {
    throw new UsageError(`not a size: ${JSON.stringify(text)} (try 500MB or 2GB)`);
  }
  const unit = (match[2] ?? "b").toLowerCase();
  const scale: Record<string, number> = {
    b: 1,
    kb: 1e3,
    mb: 1e6,
    gb: 1e9,
    tb: 1e12,
    kib: 2 ** 10,
    mib: 2 ** 20,
    gib: 2 ** 30,
    tib: 2 ** 40,
  };
  const bytes = Math.round(Number(match[1]) * scale[unit]);
  if (!(0 < bytes)) {
    throw new UsageError(`a size must be more than 0: ${JSON.stringify(text)}`);
  }
  return bytes;
}

const parsePositive = (name: string, text: string | undefined, fallback: number, max?: number): number => {
  if (text === undefined) {
    return fallback;
  }
  const value = Number(text);
  if (!(Number.isFinite(value) && 0 < value) || (max !== undefined && max < value)) {
    throw new UsageError(`--${name} must be a number above 0${max === undefined ? "" : ` and at most ${max}`}, got ${JSON.stringify(text)}`);
  }
  return value;
};

interface StackFile {
  api?: { url?: string };
  front?: string;
  ca_file?: string;
}

/** Run the CLI with `argv` (without node and the script); the exit code. */
export async function main(argv: string[], io: Io): Promise<number> {
  let parsed;
  try {
    parsed = parseArgs({
      args: argv,
      allowPositionals: true,
      options: {
        json: { type: "boolean" },
        duration: { type: "string" },
        servers: { type: "string" },
        "max-bytes": { type: "string" },
        "max-seconds": { type: "string" },
        api: { type: "string" },
        stack: { type: "string" },
        ca: { type: "string" },
        "connect-to": { type: "string" },
        origin: { type: "string" },
        "no-save": { type: "boolean" },
        "config-dir": { type: "string" },
        quiet: { type: "boolean", short: "q" },
        help: { type: "boolean", short: "h" },
        version: { type: "boolean", short: "v" },
      },
    });
  } catch (error) {
    io.stderr.write(`lagdev: ${error instanceof Error ? error.message : String(error)}\n\n${USAGE}`);
    return 2;
  }
  const { values, positionals } = parsed;
  if (values.help) {
    io.stdout.write(USAGE);
    return 0;
  }
  if (values.version) {
    io.stdout.write(`${VERSION}\n`);
    return 0;
  }
  const command = positionals[0] ?? "latency";
  if (positionals.length > 1 || !["latency", "bufferbloat", "mcp", "help"].includes(command)) {
    io.stderr.write(`lagdev: unknown command ${JSON.stringify(positionals.join(" "))}\n\n${USAGE}`);
    return 2;
  }
  if (command === "help") {
    io.stdout.write(USAGE);
    return 0;
  }

  let local;
  let durationMs = 15_000;
  let servers = 8;
  let maxBytes = 500e6;
  let maxMs = 180_000;
  try {
    durationMs = parsePositive("duration", values.duration, 15, 3600) * 1000;
    servers = Math.floor(parsePositive("servers", values.servers, 8, 8));
    maxBytes = values["max-bytes"] === undefined ? 500e6 : parseBytes(values["max-bytes"]);
    maxMs = parsePositive("max-seconds", values["max-seconds"], 180, 3600) * 1000;

    let apiURL = values.api;
    let caPem = values.ca === undefined ? undefined : readFileSync(values.ca, "utf8");
    let connectTo = values["connect-to"] === undefined ? undefined : parseHostPort(values["connect-to"]);
    if (values.stack !== undefined) {
      const stack = JSON.parse(readFileSync(values.stack, "utf8")) as StackFile;
      if (!stack.api?.url || !stack.front || !stack.ca_file) {
        throw new UsageError(`${values.stack} has no api.url, front or ca_file: is it a lag.dev stack.json?`);
      }
      apiURL ??= stack.api.url;
      caPem ??= readFileSync(stack.ca_file, "utf8");
      connectTo ??= parseHostPort(stack.front);
    }
    const progress = values.quiet || command === "mcp" ? undefined : (line: string) => io.stderr.write(`lagdev: ${line}\n`);
    local = createContext({
      apiURL,
      caPem,
      connectTo,
      origin: values.origin,
      save: !values["no-save"],
      configDir: values["config-dir"],
      onProgress: progress,
    });
  } catch (error) {
    io.stderr.write(`lagdev: ${error instanceof Error ? error.message : String(error)}\n`);
    return error instanceof UsageError ? 2 : 1;
  }

  try {
    if (command === "mcp") {
      await serveMcp(local.context);
      return 0;
    }
    let result: Measurement;
    if (command === "bufferbloat") {
      result = await measureBufferbloat(local.context, { maxBytes, maxMs, servers, signal: io.signal });
    } else {
      result = await measureLatency(local.context, { durationMs, servers, signal: io.signal });
    }
    io.stdout.write(values.json ? `${JSON.stringify(result, null, 2)}\n` : formatMeasurement(result));
    return 0;
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    if (values.json) {
      const failure = {
        error: error instanceof MeasureError ? error.code : "failed",
        message,
        ...(error instanceof MeasureError && error.retryAfterMs ? { retryAfterMs: error.retryAfterMs } : {}),
      };
      io.stdout.write(`${JSON.stringify(failure, null, 2)}\n`);
    }
    io.stderr.write(`lagdev: ${message}\n`);
    return 1;
  } finally {
    await local.close();
  }
}

// Run when this file is the program (not when a test imports it); the
// installed `lagdev` is a link to it.
const isProgram = (): boolean => {
  try {
    return process.argv[1] !== undefined && realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url));
  } catch {
    return false;
  }
};
if (isProgram()) {
  const controller = new AbortController();
  let interrupts = 0;
  process.on("SIGINT", () => {
    interrupts += 1;
    if (interrupts > 1) {
      process.exit(130);
    }
    controller.abort();
  });
  main(process.argv.slice(2), { stdout: process.stdout, stderr: process.stderr, env: process.env, signal: controller.signal }).then(
    (code) => {
      process.exitCode = code;
    },
  );
}
