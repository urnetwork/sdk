// How the CLI reaches the api and the extenders from Node (lag.dev/PLAN.md
// amendment 12, item 17).
//
// The SDK's lag client is written for a page: it takes `fetch` and
// `WebSocket` and calls them as a browser would. Node 22 has both, but a
// page's requests carry its origin, and an extender serves only the pages it
// allows (M2: lag.dev and ur.io), refusing a request with no Origin. So the
// measurement requests here carry lag.dev's origin, the one client this CLI
// stands for; the api's own requests carry none, as a page's requests to a
// CORS route are judged by it and not refused for lacking one.
//
// Everything goes through one undici agent, so a local test stack can be
// reached under the production names: `connectTo` sends every connection to
// one address (the stack's front) while TLS keeps the real server name, and
// `caPem` adds the stack's root to the trusted ones.

import { rootCertificates } from "node:tls";
import { Agent, buildConnector, fetch as undiciFetch, WebSocket as UndiciWebSocket } from "undici";

/** lag.dev's origin, which the extenders allow (M2). */
export const LAGDEV_ORIGIN = "https://lag.dev";

export interface NetOptions {
  /** the Origin measurement requests carry (default https://lag.dev) */
  origin?: string;
  /** extra trusted roots, PEM: a test stack's CA */
  caPem?: string;
  /** send every connection here, keeping each request's TLS name */
  connectTo?: { host: string; port: number };
  /** give up on a connection that takes longer (default 10 s) */
  connectTimeoutMs?: number;
}

export interface Net {
  /** fetch for the api: no Origin */
  apiFetch: typeof fetch;
  /** fetch for an extender's download streams: with the origin */
  measureFetch: typeof fetch;
  /** WebSocket for an extender's session and upload streams: with the origin */
  WebSocket: typeof WebSocket;
  /**
   * Bytes the measurement requests have moved so far: download bodies as they
   * are read, and upload messages as they are handed to their sockets. A cap
   * can follow these as they happen, without waiting for a load's 250 ms
   * intervals to complete.
   */
  meter: { bytes(): number };
  /** close every connection the agent keeps */
  close(): Promise<void>;
}

/** "host:port" into its parts; the host may be a bracketed IPv6 address. */
export function parseHostPort(text: string): { host: string; port: number } {
  const match = /^(?:\[([^\]]+)\]|([^:\[\]]+)):(\d{1,5})$/.exec(text.trim());
  const port = match ? Number(match[3]) : Number.NaN;
  if (!match || !(0 < port && port < 65536)) {
    throw new Error(`expected host:port, got ${JSON.stringify(text)}`);
  }
  return { host: match[1] ?? match[2], port };
}

// The TLS name for a connection: the request's host without its port, and
// none for an address, as undici's own connector decides it.
const serverName = (host: string | undefined): string | undefined => {
  if (!host) {
    return undefined;
  }
  const name = host.startsWith("[") ? host.slice(1, host.indexOf("]")) : host.replace(/:\d+$/, "");
  return /^[\d.]+$/.test(name) || name.includes(":") ? undefined : name;
};

const withOrigin = (headers: HeadersInit | undefined, origin: string): Headers => {
  const merged = new Headers(headers);
  merged.set("Origin", origin);
  return merged;
};

export function createNet(options: NetOptions = {}): Net {
  const origin = options.origin ?? LAGDEV_ORIGIN;
  const connectTo = options.connectTo;
  const base = buildConnector({
    ...(options.caPem ? { ca: [...rootCertificates, options.caPem] } : {}),
    timeout: options.connectTimeoutMs ?? 10_000,
  });
  const connect: buildConnector.connector = connectTo
    ? (connectOptions, callback) =>
        base(
          {
            ...connectOptions,
            hostname: connectTo.host,
            port: String(connectTo.port),
            servername: connectOptions.servername ?? serverName(connectOptions.host) ?? serverName(connectOptions.hostname),
          },
          callback,
        )
    : base;
  const dispatcher = new Agent({ connect });

  // undici's fetch takes the agent as `dispatcher`; its types are its own,
  // so the wrappers are typed as the platform's
  const apiFetch = ((input: RequestInfo | URL, init?: RequestInit) =>
    undiciFetch(input as never, { ...(init as object), dispatcher } as never)) as unknown as typeof fetch;
  let moved = 0;
  const counted = (): TransformStream<Uint8Array, Uint8Array> =>
    new TransformStream({
      transform(chunk, controller) {
        moved += chunk.byteLength;
        controller.enqueue(chunk);
      },
    });
  const measureFetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const response = (await undiciFetch(input as never, {
      ...(init as object),
      headers: withOrigin(init?.headers, origin),
      dispatcher,
    } as never)) as unknown as Response;
    if (response.body === null) {
      return response;
    }
    return new Response(response.body.pipeThrough(counted()), {
      status: response.status,
      statusText: response.statusText,
      headers: response.headers,
    });
  }) as unknown as typeof fetch;

  class MeasureWebSocket extends UndiciWebSocket {
    constructor(url: string | URL, protocols?: string | string[]) {
      super(url, { protocols, dispatcher, headers: { Origin: origin } });
    }

    send(data: Parameters<UndiciWebSocket["send"]>[0]): void {
      moved += typeof data === "string" ? Buffer.byteLength(data) : "byteLength" in data ? data.byteLength : data.size;
      super.send(data);
    }
  }

  return {
    apiFetch,
    measureFetch,
    WebSocket: MeasureWebSocket as unknown as typeof WebSocket,
    meter: { bytes: () => moved },
    close: () => dispatcher.close(),
  };
}
