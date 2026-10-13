// A local stand-in for the api's lag routes (lag.dev/PLAN.md A1, A3, A4, A6)
// in front of the SDK's mock extenders, for the CLI's tests. Nothing here
// reaches the network beyond 127.0.0.1.

import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { connect, createServer as createTcpServer, type AddressInfo, type Socket } from "node:net";
import { startMockExtender, type MockExtender, type MockExtenderOptions } from "../../test/lag/client_mock_extender.ts";

export interface Place {
  city: string;
  region?: string;
  country_code: string;
  country_name: string;
  latitude: number;
  longitude: number;
}

export const PLACES: Record<string, Place> = {
  houston: { city: "Houston", region: "Texas", country_code: "us", country_name: "United States", latitude: 29.7604, longitude: -95.3698 },
  dallas: { city: "Dallas", region: "Texas", country_code: "us", country_name: "United States", latitude: 32.7767, longitude: -96.797 },
  mexico: { city: "Mexico City", country_code: "mx", country_name: "Mexico", latitude: 19.4326, longitude: -99.1332 },
  denver: { city: "Denver", region: "Colorado", country_code: "us", country_name: "United States", latitude: 39.7392, longitude: -104.9903 },
  toronto: { city: "Toronto", country_code: "ca", country_name: "Canada", latitude: 43.6532, longitude: -79.3832 },
  london: { city: "London", country_code: "gb", country_name: "United Kingdom", latitude: 51.5074, longitude: -0.1278 },
  frankfurt: { city: "Frankfurt am Main", country_code: "de", country_name: "Germany", latitude: 50.1109, longitude: 8.6821 },
  saopaulo: { city: "São Paulo", country_code: "br", country_name: "Brazil", latitude: -23.5505, longitude: -46.6333 },
  tokyo: { city: "Tokyo", country_code: "jp", country_name: "Japan", latitude: 35.6762, longitude: 139.6503 },
  sydney: { city: "Sydney", country_code: "au", country_name: "Australia", latitude: -33.8688, longitude: 151.2093 },
};

/** Austin, as the stack and lag.dev's tests place the visitor. */
export const VISITOR_IP = "203.0.113.9";

export interface MockApiOptions {
  /** the places to run extenders in (default houston, dallas, london) */
  places?: string[];
  extender?: MockExtenderOptions;
  /** refuse guest creation with this */
  refuseSession?: { code: string; message: string; retry_after_ms?: number };
  /** answer this jwt (or every one matching) with 401 on the lag routes */
  rejectJwt?: string | RegExp;
  /** a dead origin to give position 0's ticket, so its session can't open */
  deadFirstTicket?: boolean;
  /** put a relay in front of each extender that reads what the client sends it */
  relay?: boolean;
}

export interface MockApi {
  url: string;
  extenders: Map<string, MockExtender>;
  sessionCreates: number;
  trackRequests: Array<{ extenders: Array<{ extender_id: string }> }>;
  /** every request to the api, with its Origin and X-Lag-Client headers */
  requests: Array<{ method: string; path: string; origin: string | undefined; client: string | undefined }>;
  /** with `relay`: the requests the extenders were sent, and how many named an X-Lag-Client */
  extenderTraffic: { requests: number; lagClient: number };
  close(): Promise<void>;
}

// A TCP relay to an extender that counts the request lines it is sent and
// any X-Lag-Client header among them. Upload frames are masked random bytes,
// so neither turns up in them by chance.
const REQUEST_LINE = /(?:^|\r\n)(?:GET|POST) \/lag\//g;
const LAG_CLIENT = /\r\nx-lag-client:/gi;
const count = (text: string, pattern: RegExp) => (text.match(pattern) ?? []).length;

const startRelay = async (target: string, traffic: MockApi["extenderTraffic"]) => {
  const sockets = new Set<Socket>();
  const relay = createTcpServer((client) => {
    const upstream = connect(Number(new URL(target).port), "127.0.0.1");
    for (const socket of [client, upstream]) {
      sockets.add(socket);
      socket.on("error", () => undefined);
      socket.on("close", () => {
        sockets.delete(socket);
        client.destroy();
        upstream.destroy();
      });
    }
    let tail = "";
    client.on("data", (chunk: Buffer) => {
      // a match across two chunks counts once: those wholly in the tail
      // were counted with the chunk before
      const text = tail + chunk.toString("latin1");
      traffic.requests += count(text, REQUEST_LINE) - count(tail, REQUEST_LINE);
      traffic.lagClient += count(text, LAG_CLIENT) - count(tail, LAG_CLIENT);
      tail = text.slice(-16);
    });
    client.pipe(upstream).pipe(client);
  });
  await new Promise<void>((resolve) => relay.listen(0, "127.0.0.1", resolve));
  return {
    origin: `http://127.0.0.1:${(relay.address() as AddressInfo).port}`,
    close: () =>
      new Promise<void>((resolve) => {
        for (const socket of sockets) {
          socket.destroy();
        }
        relay.close(() => resolve());
      }),
  };
};

const readBody = (request: IncomingMessage): Promise<string> =>
  new Promise((resolve) => {
    let body = "";
    request.on("data", (chunk) => {
      body += chunk;
    });
    request.on("end", () => resolve(body));
  });

const json = (response: ServerResponse, status: number, value: unknown) => {
  response.writeHead(status, { "content-type": "application/json" });
  response.end(JSON.stringify(value));
};

export async function startMockApi(options: MockApiOptions = {}): Promise<MockApi> {
  const places = options.places ?? ["houston", "dallas", "london"];
  const extenders = new Map<string, MockExtender>();
  const ids: Array<{ id: string; place: Place }> = [];
  const extenderTraffic = { requests: 0, lagClient: 0 };
  const relays = new Map<string, Awaited<ReturnType<typeof startRelay>>>();
  for (const [index, name] of places.entries()) {
    const id = `00000000-0000-4000-8000-${String(index + 1).padStart(12, "0")}`;
    const extender = await startMockExtender(options.extender);
    extenders.set(id, extender);
    if (options.relay) {
      relays.set(id, await startRelay(extender.origin, extenderTraffic));
    }
    ids.push({ id, place: PLACES[name] });
  }
  // where the client reaches an extender: through its relay, if it has one
  const hostOf = (id: string) => relays.get(id)?.origin ?? extenders.get(id)!.origin;
  let jwtCount = 0;
  let ticketCount = 0;
  const api: MockApi = {
    url: "",
    extenders,
    sessionCreates: 0,
    trackRequests: [],
    requests: [],
    extenderTraffic,
    close: async () => {
      await new Promise<void>((resolve) => server.close(() => resolve()));
      server.closeAllConnections();
      await Promise.all([...relays.values()].map((relay) => relay.close()));
      await Promise.all([...extenders.values()].map((extender) => extender.close()));
    },
  };
  const authorized = (request: IncomingMessage): string | null => {
    const header = request.headers.authorization ?? "";
    return header.startsWith("Bearer ") ? header.slice(7) : null;
  };
  const server = createServer(async (request, response) => {
    const path = new URL(request.url ?? "/", "http://localhost").pathname;
    const client = request.headers["x-lag-client"];
    api.requests.push({
      method: request.method ?? "",
      path,
      origin: request.headers.origin,
      client: Array.isArray(client) ? client.join(", ") : client,
    });
    if (request.method === "GET" && path === "/my-ip-info") {
      json(response, 200, {
        info: {
          ip: VISITOR_IP,
          location: {
            coordinates: { lat: 30.2672, lon: -97.7431 },
            city: "Austin",
            region: "Texas",
            country: { code: "us", name: "United States" },
          },
          asn: { number: 64500, organization: "Example Networks" },
        },
        connected_to_network: false,
      });
      return;
    }
    if (request.method === "POST" && path === "/lag/session") {
      await readBody(request);
      if (options.refuseSession) {
        json(response, 200, { error: options.refuseSession });
        return;
      }
      api.sessionCreates += 1;
      jwtCount += 1;
      json(response, 200, {
        by_client_jwt: `jwt-${jwtCount}`,
        network_id: `network-${jwtCount}`,
        client_id: `client-${jwtCount}`,
        expires_at: new Date(Date.now() + 24 * 3600 * 1000).toISOString(),
      });
      return;
    }
    const jwt = authorized(request);
    const rejected =
      jwt !== null && (typeof options.rejectJwt === "string" ? jwt === options.rejectJwt : options.rejectJwt?.test(jwt));
    if (path.startsWith("/lag/") && (jwt === null || rejected)) {
      json(response, 401, { message: "unauthorized" });
      return;
    }
    if (request.method === "GET" && path === "/lag/extenders") {
      json(response, 200, {
        extenders: ids.map(({ id, place }) => ({
          extender_id: id,
          public_key: "00",
          host: hostOf(id),
          ips: [],
          location: place,
        })),
      });
      return;
    }
    if (request.method === "POST" && path === "/lag/track") {
      const body = JSON.parse(await readBody(request)) as { extenders: Array<{ extender_id: string }> };
      api.trackRequests.push(body);
      json(response, 200, {
        track_id: `track-${api.trackRequests.length}`,
        tickets: body.extenders
          .filter((extender) => extenders.has(extender.extender_id))
          .map((extender, position) => {
            ticketCount += 1;
            return {
              extender_id: extender.extender_id,
              host: options.deadFirstTicket && position === 0 ? "http://127.0.0.1:1" : hostOf(extender.extender_id),
              position,
              ticket: `ticket-${ticketCount}`,
            };
          }),
        expires_at: new Date(Date.now() + 120_000).toISOString(),
      });
      return;
    }
    json(response, 404, { message: "not found" });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  api.url = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  return api;
}
