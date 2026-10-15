// A lag.dev measurement from wherever this code runs (lag.dev/PLAN.md
// amendment 12, item 17), on the SDK's lag client: the same guest, sample,
// track, sessions and loads a page uses, so it measures the network the
// machine running it is on.
//
// - latency: every server is pinged for a while, with no load. Each server
//   times its own pings. In a browser the network stack answers them, but
//   in Node undici answers each ping from its JavaScript frame parser on the
//   main thread, which also reads the downloads here. So whatever delays this
//   process's event loop delays the pongs and enters the round trips: little
//   when idle, but at multi-gigabit load rates enough to add to the loaded
//   increase.
// - bufferbloat: the same pings, and after an idle start each server in
//   turn runs one load: a download, then an upload, over four streams each.
//   How much the round trip to the loading server grows in each direction is
//   the bufferbloat. Caps on bytes and seconds stop it early, and never cut
//   a load past them by more than one 250 ms interval.

import { isURNetworkApiError } from "../../src/api_client";
import type { LagExtendersResult, LagMyIpInfoResult, LagTrackArgs, LagTrackResult, LagTrackTicket } from "../../src/lag/api";
import { LagSessionError, type LagLoadResult, type LagSession, type LagSessionEvent } from "../../src/lag/client";
import { LagGuestSessionError } from "../../src/lag/session";
import { percentiles } from "../../src/lag/stats";
import { rpmFor, type Rpm } from "./rpm";
import { selectServers, type Server, type Visitor } from "./select";
import { VERSION } from "./version";

/**
 * The header that tells the api which tool ran a measurement, on its track
 * request only, so the stats count the CLI's and the MCP server's runs apart
 * from lag.dev's page (which sends none). It names the tool, nothing else.
 */
export const LAG_CLIENT_HEADER = "X-Lag-Client";

/** The tool running a measurement: the command line or the MCP server. */
export type LagClient = "cli" | "mcp";

/** What a measurement needs from the outside, so tests can stand in for it. */
export interface MeasureContext {
  api: {
    lagExtenders(): Promise<LagExtendersResult>;
    lagTrack(args: LagTrackArgs, options?: { headers?: Record<string, string> }): Promise<LagTrackResult>;
    myIpInfo(): Promise<LagMyIpInfoResult>;
  };
  /** the guest whose jwt the api client sends */
  guest: { token(): Promise<string>; invalidate(jwt?: string): Promise<void> };
  openSession(options: { host: string; ticket: string; onEvent(event: LagSessionEvent): void }): LagSession;
  /** the api's origin, for the result */
  apiOrigin: string;
  /**
   * Bytes the measurement has moved so far, as they move. Without it a cap is
   * checked at each 250 ms interval a load completes.
   */
  meter?: { bytes(): number };
  /** progress for a person watching, one line each */
  onProgress?: (line: string) => void;
}

export interface LatencyOptions {
  /** how long to ping, ms (default 15 s) */
  durationMs?: number;
  /** servers to measure, 1 to 8 (default 8) */
  servers?: number;
  /** the tool running it, sent as X-Lag-Client with the track request */
  client?: LagClient;
  signal?: AbortSignal;
}

export interface BufferbloatOptions {
  /** stop before moving more than this many bytes */
  maxBytes: number;
  /** stop before running longer than this, ms */
  maxMs: number;
  /** servers to measure, 1 to 8 (default 8) */
  servers?: number;
  /** pings with no load before the first one, ms (default 5 s) */
  idleMs?: number;
  /** pause after each load before the next, ms (default 2 s) */
  settleMs?: number;
  /** the tool running it, sent as X-Lag-Client with the track request */
  client?: LagClient;
  signal?: AbortSignal;
}

export interface Percentiles {
  p50Ms: number;
  p95Ms: number;
}

export interface DirectionResult {
  /** p50 and p95 over the load's 250 ms intervals, Mbps */
  p50Mbps: number | null;
  p95Mbps: number | null;
  /** bytes moved: read here (download) or verified by the server (upload) */
  bytes: number;
  /** the round trip to this server during this direction, p50 */
  latencyP50Ms: number | null;
  /** that less the idle p50: how much this direction's load added */
  increaseMs: number | null;
}

export interface LoadResult {
  status: "complete" | "partial" | "refused" | "failed" | "skipped";
  /** the server's end reason, e.g. "complete"; or why it didn't run */
  reason: string | null;
  download: DirectionResult | null;
  upload: (DirectionResult & { verified: boolean }) | null;
  /** loaded RPM: from the round trip during this server's own load */
  rpm: Rpm | null;
}

export interface ServerResult {
  extenderId: string;
  city: string;
  region?: string;
  country: string;
  countryCode: string;
  /** from the visitor's place, km, when both are known */
  distanceKm: number | null;
  /** round trips seen, lost ones included */
  pings: number;
  /** pings with no answer within 5 s */
  lost: number;
  /** round trips with no load anywhere */
  idle: Percentiles | null;
  /** idle RPM, from the idle p50 */
  rpm: Rpm | null;
  load?: LoadResult;
  /** why the server couldn't be measured, when it couldn't */
  error?: string;
}

export interface Measurement {
  tool: "lagdev";
  version: string;
  mode: "latency" | "bufferbloat";
  /** ISO 8601 */
  startedAt: string;
  durationMs: number;
  api: string;
  /** where the api places this machine's network; never its address */
  visitor: Omit<Visitor, "latitude" | "longitude"> | null;
  servers: ServerResult[];
  overall: {
    pings: number;
    lost: number;
    /** every server's idle round trips together */
    idle: Percentiles | null;
    /** the server with the lowest idle p50 */
    best: { extenderId: string; city: string; country: string; idleP50Ms: number } | null;
    /** idle RPM from the best server's idle p50: the access network's, without far servers' distance */
    rpm: Rpm | null;
    /** the median over servers of each direction's increase */
    loadedIncreaseMs?: { download: number | null; upload: number | null };
    /** loaded RPM from the median over servers of the round trip during their own loads */
    loadedRpm?: Rpm | null;
    downloadMbps?: { best: number | null; median: number | null };
    uploadMbps?: { best: number | null; median: number | null };
  };
  bytes: { download: number; upload: number; total: number; maxBytes?: number };
  stoppedEarly: null | "max_bytes" | "max_seconds" | "interrupted";
  notes: string[];
}

/** Why a measurement couldn't run: the api's refusal, or no server. */
export class MeasureError extends Error {
  readonly code: string;
  readonly retryAfterMs?: number;

  constructor(code: string, message: string, retryAfterMs?: number) {
    super(message);
    this.name = "MeasureError";
    this.code = code;
    this.retryAfterMs = retryAfterMs;
  }
}

interface Ping {
  at: number;
  ms: number | null;
}

interface Dot {
  server: Server;
  session: LagSession | null;
  pings: Ping[];
  phases: { download?: number; upload?: number; done?: number };
  progress: { download: number; upload: number };
  onProgress?: () => void;
  /** what the session's load measured, worked out once every span is known */
  raw?: LagLoadResult;
  load?: LoadResult;
  error?: string;
}

const SETTLE_MS = 2000;
const IDLE_MS = 5000;
const LOAD_SETUP_MS = 2000;

const round = (value: number, digits: number): number => {
  const scale = 10 ** digits;
  return Math.round(value * scale) / scale;
};

const median = (values: number[]): number | null => {
  const p = percentiles(values);
  return p === undefined ? null : p.p50;
};

const asPercentiles = (values: number[]): Percentiles | null => {
  const p = percentiles(values);
  return p === undefined ? null : { p50Ms: round(p.p50, 1), p95Ms: round(p.p95, 1) };
};

const sleep = (ms: number, signal?: AbortSignal): Promise<void> =>
  new Promise((resolve) => {
    if (signal?.aborted || ms <= 0) {
      resolve();
      return;
    }
    const done = () => {
      clearTimeout(timer);
      signal?.removeEventListener("abort", done);
      resolve();
    };
    const timer = setTimeout(done, ms);
    signal?.addEventListener("abort", done, { once: true });
  });

const placeOf = (server: Server): string =>
  [server.city, server.countryCode.toUpperCase()].filter((part) => part !== "").join(", ") || server.extenderId;

// The visitor's place and network from my-ip-info. Its address is read past
// and never kept.
const visitorOf = (info: LagMyIpInfoResult): Visitor => {
  const location = info.info?.location;
  const coordinates = location?.coordinates;
  const visitor: Visitor = {
    city: location?.city,
    region: location?.region,
    countryCode: location?.country?.code?.toLowerCase(),
    countryName: location?.country?.name,
    latitude: coordinates?.lat,
    longitude: coordinates?.lon,
  };
  const asn = info.info?.asn;
  if (asn && typeof asn.number === "number") {
    visitor.asn = { number: asn.number, organization: asn.organization };
  }
  return visitor;
};

const refusalError = (what: string, refusal: { code?: string; message?: string; retry_after_ms?: number }): MeasureError =>
  new MeasureError(
    refusal.code ?? "refused",
    `${what}: ${refusal.message ?? refusal.code ?? "refused"}${
      refusal.retry_after_ms ? ` (try again in ${Math.ceil(refusal.retry_after_ms / 1000)} s)` : ""
    }`,
    refusal.retry_after_ms,
  );

/** Run an api call with the guest's jwt; a refused jwt makes a new guest, once. */
async function withGuest<T>(context: MeasureContext, call: () => Promise<T>, retried = false): Promise<T> {
  let jwt: string;
  try {
    jwt = await context.guest.token();
  } catch (error) {
    if (error instanceof LagGuestSessionError) {
      throw refusalError("lag.dev couldn't start a guest session", {
        code: error.code ?? "guest",
        message: error.message,
        retry_after_ms: error.retryAfterMs,
      });
    }
    throw error;
  }
  try {
    return await call();
  } catch (error) {
    if (!retried && isURNetworkApiError(error) && error.kind === "http" && (error.status === 401 || error.status === 403)) {
      await context.guest.invalidate(jwt);
      return await withGuest(context, call, true);
    }
    throw error;
  }
}

interface Prepared {
  startedAt: number;
  visitor: Visitor;
  dots: Dot[];
}

// The guest, the sample, the track and an open session on each server, a
// spare standing in for a server whose session won't open.
async function prepare(
  context: MeasureContext,
  count: number,
  client: LagClient | undefined,
  signal?: AbortSignal,
): Promise<Prepared> {
  const startedAt = Date.now();
  let visitor: Visitor = {};
  try {
    visitor = visitorOf(await context.api.myIpInfo());
  } catch {
    // without a place every server is chosen by spread alone
  }
  const sample = await withGuest(context, () => context.api.lagExtenders());
  if (sample.error) {
    throw refusalError("lag.dev couldn't list its servers", sample.error);
  }
  const selection = selectServers(visitor, sample.extenders, { count });
  if (selection.servers.length === 0) {
    throw new MeasureError("unavailable", "no lag.dev server can be measured right now");
  }
  const ordered = [...selection.servers, ...selection.spares];
  const track = await withGuest(context, () =>
    context.api.lagTrack(
      { extenders: ordered.map((server) => ({ extender_id: server.extenderId })) },
      client === undefined ? undefined : { headers: { [LAG_CLIENT_HEADER]: client } },
    ),
  );
  if (track.error) {
    throw refusalError("lag.dev couldn't start a measurement", track.error);
  }
  const byPosition = new Map<number, LagTrackTicket>();
  for (const ticket of track.tickets) {
    byPosition.set(ticket.position, ticket);
  }
  const spares: Array<{ server: Server; ticket: LagTrackTicket }> = [];
  for (let position = selection.servers.length; position < ordered.length; position += 1) {
    const ticket = byPosition.get(position);
    if (ticket) {
      spares.push({ server: ordered[position], ticket });
    }
  }

  context.onProgress?.(`connecting to ${selection.servers.length} servers`);
  const dots = await Promise.all(
    selection.servers.map(async (server, position) => {
      const dot: Dot = { server, session: null, pings: [], phases: {}, progress: { download: 0, upload: 0 } };
      let candidate: { server: Server; ticket: LagTrackTicket } | undefined = byPosition.has(position)
        ? { server, ticket: byPosition.get(position)! }
        : spares.shift();
      while (candidate && !signal?.aborted) {
        dot.server = candidate.server;
        const session = openDot(context, dot, candidate.ticket);
        try {
          await session.ready;
          dot.session = session;
          return dot;
        } catch (error) {
          session.close();
          dot.error = error instanceof Error ? error.message : String(error);
          dot.pings = [];
          candidate = spares.shift();
        }
      }
      dot.error ??= "no ticket for this server";
      return dot;
    }),
  );
  return { startedAt, visitor, dots };
}

function openDot(context: MeasureContext, dot: Dot, ticket: LagTrackTicket): LagSession {
  return context.openSession({
    host: ticket.host,
    ticket: ticket.ticket,
    onEvent: (event) => {
      switch (event.type) {
        case "rtt":
          dot.pings.push({ at: event.at, ms: event.ms });
          break;
        case "load":
          dot.phases[event.phase] = event.at;
          break;
        case "progress":
          dot.progress[event.direction] = event.bytes;
          dot.onProgress?.();
          break;
        default:
          break;
      }
    },
  });
}

async function closeAll(dots: Dot[]): Promise<void> {
  const closing: Promise<unknown>[] = [];
  for (const dot of dots) {
    if (dot.session) {
      dot.session.close();
      closing.push(dot.session.closed);
    }
  }
  await Promise.race([Promise.allSettled(closing), sleep(3000)]);
}

interface Span {
  start: number;
  end: number;
}

// A ping is idle when its whole round trip falls outside every load's span
// (lag.dev/PLAN.md L4); a lost one's round trip counts as 5 s.
const isIdle = (ping: Ping, spans: readonly Span[]): boolean =>
  spans.every((span) => ping.at + (ping.ms ?? 5000) <= span.start || span.end <= ping.at);

const roundTripsIn = (pings: readonly Ping[], from: number | undefined, to: number | undefined): number[] => {
  if (from === undefined || to === undefined) {
    return [];
  }
  return pings.filter((ping) => ping.ms !== null && from <= ping.at && ping.at < to).map((ping) => ping.ms!);
};

function serverResult(dot: Dot, spans: readonly Span[]): ServerResult {
  const server = dot.server;
  const idle = asPercentiles(
    dot.pings.filter((ping) => ping.ms !== null && isIdle(ping, spans)).map((ping) => ping.ms!),
  );
  const result: ServerResult = {
    extenderId: server.extenderId,
    city: server.city,
    region: server.region,
    country: server.countryName,
    countryCode: server.countryCode,
    distanceKm: Number.isFinite(server.distanceKm) ? Math.round(server.distanceKm) : null,
    pings: dot.pings.length,
    lost: dot.pings.filter((ping) => ping.ms === null).length,
    idle,
    rpm: rpmFor(idle?.p50Ms),
  };
  if (dot.error && !dot.session) {
    result.error = dot.error;
  }
  if (dot.load) {
    result.load = dot.load;
  }
  return result;
}

function overallOf(dots: readonly Dot[], servers: readonly ServerResult[], spans: readonly Span[]): Measurement["overall"] {
  const idleTrips = dots.flatMap((dot) =>
    dot.pings.filter((ping) => ping.ms !== null && isIdle(ping, spans)).map((ping) => ping.ms!),
  );
  let best: Measurement["overall"]["best"] = null;
  for (const server of servers) {
    if (server.idle && (best === null || server.idle.p50Ms < best.idleP50Ms)) {
      best = { extenderId: server.extenderId, city: server.city, country: server.country, idleP50Ms: server.idle.p50Ms };
    }
  }
  return {
    pings: servers.reduce((sum, server) => sum + server.pings, 0),
    lost: servers.reduce((sum, server) => sum + server.lost, 0),
    idle: asPercentiles(idleTrips),
    best,
    rpm: rpmFor(best?.idleP50Ms),
  };
}

// What a load measured, against the server's idle p50.
function loadResult(
  dot: Dot,
  result: LagLoadResult | undefined,
  status: LoadResult["status"],
  reason: string | null,
  spans: readonly Span[],
): LoadResult {
  if (result === undefined) {
    return { status, reason, download: null, upload: null, rpm: null };
  }
  const idle = percentiles(
    dot.pings.filter((ping) => ping.ms !== null && isIdle(ping, spans)).map((ping) => ping.ms!),
  );
  const { download: downloadAt, upload: uploadAt, done: doneAt } = dot.phases;
  const downloadTrips = roundTripsIn(dot.pings, downloadAt, uploadAt ?? doneAt);
  const uploadTrips = roundTripsIn(dot.pings, uploadAt, doneAt);
  const direction = (
    measured: LagLoadResult["download"],
    trips: number[],
  ): DirectionResult => {
    const latency = percentiles(trips);
    return {
      p50Mbps: measured.percentiles ? round(measured.percentiles.p50, 2) : null,
      p95Mbps: measured.percentiles ? round(measured.percentiles.p95, 2) : null,
      bytes: measured.bytes,
      latencyP50Ms: latency ? round(latency.p50, 1) : null,
      increaseMs: latency && idle ? round(latency.p50 - idle.p50, 1) : null,
    };
  };
  const own = percentiles([...downloadTrips, ...uploadTrips]);
  return {
    status,
    reason: reason ?? result.endReason ?? null,
    download: direction(result.download, downloadTrips),
    upload: { ...direction(result.upload, uploadTrips), verified: result.uploadVerified },
    rpm: rpmFor(own?.p50),
  };
}

function measurement(
  mode: Measurement["mode"],
  context: MeasureContext,
  prepared: Prepared,
  spans: readonly Span[],
  stoppedEarly: Measurement["stoppedEarly"],
  notes: string[],
  maxBytes?: number,
): Measurement {
  const { visitor, dots } = prepared;
  const servers = dots.map((dot) => serverResult(dot, spans));
  const overall = overallOf(dots, servers, spans);
  let download = 0;
  let upload = 0;
  for (const dot of dots) {
    download += dot.load?.download?.bytes ?? 0;
    upload += dot.load?.upload?.bytes ?? 0;
  }
  if (mode === "bufferbloat") {
    const loads = servers.map((server) => server.load).filter((load): load is LoadResult => load !== undefined);
    const increases = (pick: (load: LoadResult) => number | null | undefined) =>
      median(loads.map(pick).filter((value): value is number => typeof value === "number"));
    const rates = (pick: (load: LoadResult) => number | null | undefined) =>
      loads.map(pick).filter((value): value is number => typeof value === "number");
    const ownP50s = dots
      .map((dot) => median([...roundTripsIn(dot.pings, dot.phases.download, dot.phases.done)]))
      .filter((value): value is number => value !== null);
    const downloadRates = rates((load) => load.download?.p50Mbps);
    const uploadRates = rates((load) => load.upload?.p50Mbps);
    const roundOrNull = (value: number | null, digits: number) => (value === null ? null : round(value, digits));
    overall.loadedIncreaseMs = {
      download: roundOrNull(increases((load) => load.download?.increaseMs), 1),
      upload: roundOrNull(increases((load) => load.upload?.increaseMs), 1),
    };
    overall.loadedRpm = rpmFor(median(ownP50s));
    overall.downloadMbps = {
      best: downloadRates.length ? Math.max(...downloadRates) : null,
      median: roundOrNull(median(downloadRates), 2),
    };
    overall.uploadMbps = {
      best: uploadRates.length ? Math.max(...uploadRates) : null,
      median: roundOrNull(median(uploadRates), 2),
    };
  }
  const visitorOut: Measurement["visitor"] =
    visitor.city || visitor.countryCode || visitor.asn
      ? {
          city: visitor.city,
          region: visitor.region,
          countryCode: visitor.countryCode,
          countryName: visitor.countryName,
          asn: visitor.asn,
        }
      : null;
  return {
    tool: "lagdev",
    version: VERSION,
    mode,
    startedAt: new Date(prepared.startedAt).toISOString(),
    durationMs: Date.now() - prepared.startedAt,
    api: context.apiOrigin,
    visitor: visitorOut,
    servers,
    overall,
    bytes: { download, upload, total: download + upload, ...(maxBytes === undefined ? {} : { maxBytes }) },
    stoppedEarly,
    notes,
  };
}

const COMMON_NOTES = [
  "Each server times its own pings, about four a second. This process answers them on its main thread, which also reads the downloads, so its own delays add to a round trip: little when idle, but at multi-gigabit load rates enough to add to the loaded increase. A browser's network stack answers them instead.",
  "A ping is lost when no answer comes within 5 s. That is not a packet-loss measurement.",
  "RPM is 60,000 divided by the round trip in ms, banded as in draft-ietf-ippm-responsiveness-09: poor below 300, fair to 1,000, good to 6,000, excellent above.",
  "Nothing about this machine's address is stored: the guest session and its results keep only the network (ASN) and city.",
];

/** Ping up to 8 lag.dev servers for a while, with no load. */
export async function measureLatency(context: MeasureContext, options: LatencyOptions = {}): Promise<Measurement> {
  const durationMs = Math.max(1000, options.durationMs ?? 15_000);
  const prepared = await prepare(context, options.servers ?? 8, options.client, options.signal);
  const open = prepared.dots.filter((dot) => dot.session !== null).length;
  context.onProgress?.(`pinging ${open} servers for ${Math.round(durationMs / 1000)} s`);
  try {
    await sleep(durationMs, options.signal);
  } finally {
    await closeAll(prepared.dots);
  }
  return measurement(
    "latency",
    context,
    prepared,
    [],
    options.signal?.aborted ? "interrupted" : null,
    [...COMMON_NOTES, "No load ran, so every round trip is idle; RPM here is the idle RPM of the server with the lowest p50."],
  );
}

/**
 * Ping up to 8 lag.dev servers and load each in turn, within `maxBytes` and
 * `maxMs`, to measure how much a busy link adds to the round trip.
 */
export async function measureBufferbloat(context: MeasureContext, options: BufferbloatOptions): Promise<Measurement> {
  if (!(Number.isFinite(options.maxBytes) && 0 < options.maxBytes)) {
    throw new RangeError("maxBytes must be a positive number of bytes");
  }
  if (!(Number.isFinite(options.maxMs) && 0 < options.maxMs)) {
    throw new RangeError("maxMs must be a positive number of ms");
  }
  const idleMs = options.idleMs ?? IDLE_MS;
  const settleMs = options.settleMs ?? SETTLE_MS;
  const signal = options.signal;
  const prepared = await prepare(context, options.servers ?? 8, options.client, signal);
  const elapsed = () => Date.now() - prepared.startedAt;
  const spans: Span[] = [];
  let used = 0;
  let largest = 0;
  let stoppedEarly: Measurement["stoppedEarly"] = null;

  try {
    context.onProgress?.(`pinging for ${Math.round(idleMs / 1000)} s before the loads`);
    await sleep(idleMs, signal);
    for (const [index, dot] of prepared.dots.entries()) {
      if (signal?.aborted) {
        stoppedEarly = "interrupted";
        break;
      }
      const session = dot.session;
      if (session === null || session.state !== "open" || session.hello === null) {
        dot.load = { status: "skipped", reason: dot.error ?? "no session", download: null, upload: null, rpm: null };
        continue;
      }
      const loadMs = session.hello.downloadMs + session.hello.uploadMs + LOAD_SETUP_MS;
      if (options.maxMs < elapsed() + loadMs + settleMs) {
        stoppedEarly = "max_seconds";
        break;
      }
      if (0 < largest && options.maxBytes < used + largest) {
        stoppedEarly = "max_bytes";
        break;
      }
      context.onProgress?.(`loading ${placeOf(dot.server)} (${index + 1} of ${prepared.dots.length})`);
      const requested = Date.now();
      const meter = context.meter;
      const meterStart = meter?.bytes() ?? 0;
      const movedNow = () => Math.max(meter ? meter.bytes() - meterStart : 0, dot.progress.download + dot.progress.upload);
      let cut: "max_bytes" | "max_seconds" | null = null;
      const checkBytes = () => {
        if (cut === null && options.maxBytes <= used + movedNow()) {
          cut = "max_bytes";
          session.close();
        }
      };
      dot.onProgress = checkBytes;
      // a load moves its bytes far faster than its intervals complete
      const poll = meter ? setInterval(checkBytes, 20) : undefined;
      const timer = setTimeout(() => {
        cut ??= "max_seconds";
        session.close();
      }, Math.max(0, options.maxMs - elapsed()));
      const onAbort = () => session.close();
      signal?.addEventListener("abort", onAbort, { once: true });
      let result: LagLoadResult | undefined;
      let status: LoadResult["status"] = "complete";
      let reason: string | null = null;
      try {
        result = await session.startLoad();
      } catch (error) {
        if (error instanceof LagSessionError) {
          result = error.partial;
          reason = error.code;
          status = error.code === "busy" || error.code === "budget" ? "refused" : result ? "partial" : "failed";
        } else {
          status = "failed";
          reason = error instanceof Error ? error.message : String(error);
        }
      } finally {
        clearTimeout(timer);
        clearInterval(poll);
        signal?.removeEventListener("abort", onAbort);
        dot.onProgress = undefined;
      }
      // what the load cost: the meter's count, sent and not only verified
      const moved = Math.max(movedNow(), result ? result.download.bytes + result.upload.bytes : 0);
      used += moved;
      largest = Math.max(largest, moved);
      const end = dot.phases.done ?? Date.now();
      spans.push({ start: requested, end: end + settleMs });
      dot.load = { status, reason: cut ?? reason, download: null, upload: null, rpm: null };
      dot.raw = result;
      if (cut !== null) {
        stoppedEarly = cut;
        break;
      }
      if (signal?.aborted) {
        stoppedEarly = "interrupted";
        break;
      }
      await sleep(settleMs, signal);
    }
  } finally {
    await closeAll(prepared.dots);
  }
  for (const dot of prepared.dots) {
    if (dot.load && dot.load.status !== "skipped") {
      dot.load = loadResult(dot, dot.raw, dot.load.status, dot.load.reason, spans);
    }
  }
  for (const dot of prepared.dots) {
    dot.load ??= {
      status: "skipped",
      reason: stoppedEarly ?? dot.error ?? "not run",
      download: null,
      upload: null,
      rpm: null,
    };
  }
  return measurement("bufferbloat", context, prepared, spans, stoppedEarly, [
    ...COMMON_NOTES,
    "Loads ran one server at a time: a download, then an upload, over four streams each. The increase is the round trip to the loading server during each direction, less its idle p50.",
    "Idle round trips leave out every ping whose round trip overlapped a load or the 2 s after it.",
  ], options.maxBytes);
}
