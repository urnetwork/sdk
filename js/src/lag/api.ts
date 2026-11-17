// The api calls lag.dev makes (lag.dev/PLAN.md S6): the guest session (A1),
// the visitor's address with its network (A3), the extender sample (A4) and
// a track's tickets (A6). They are written by hand on the generated client's
// runtime, so auth, timeouts, GET retry and errors behave as the generated
// methods do, until connect/api/bringyour.yml carries the lag routes and
// gen_openapi.go generates them.

import {
  URNetworkApiClientBase,
  URNetworkApiError,
  type RequestOptions,
  type URNetworkApiClientConfig,
} from "../api_client";
import type { MyIPInfoResult } from "../generated/openapi";
import type { DriftVerdict, StopReason } from "./drift";

/**
 * A refusal the api answers in the body. The lag routes name it by `code`
 * (A6: limit, unavailable) with `retry_after_ms`; older routes carry a
 * `message`.
 */
export interface LagApiRefusal {
  code?: string;
  message?: string;
  retry_after_ms?: number;
}

/** A1 `POST /lag/session`: a lag guest network with its one client. */
export interface LagSessionResult {
  /** the client jwt for /lag/extenders and /lag/track */
  by_client_jwt?: string;
  network_id?: string;
  client_id?: string;
  /** RFC 3339; the jwt is good until then */
  expires_at?: string;
  error?: LagApiRefusal;
}

/** A4: the extender's derived location, else GeoLite2-City for its first address. */
export interface LagExtenderLocation {
  city?: string;
  region?: string;
  /** ISO 3166-1 alpha-2, lower case */
  country_code?: string;
  country_name?: string;
  latitude?: number;
  longitude?: number;
}

/**
 * A4: an extender that serves the measurement api now. There is one entry
 * per extender: its one name serves both address families (N3).
 */
export interface LagExtender {
  extender_id: string;
  /** the ed25519 identity key, hex */
  public_key: string;
  /**
   * What the browser connects to: the measurement name (N1),
   * `<extender_id>.extender.bringyour.com`. In a local test stack it may
   * carry a port.
   */
  host: string;
  /** the extender's active addresses, IPv4 first */
  ips: string[];
  location?: LagExtenderLocation;
}

/** A4 `GET /lag/extenders`: up to 64, randomized per request. */
export interface LagExtendersResult {
  extenders: LagExtender[];
  error?: LagApiRefusal;
}

/** One extender in an A6 `POST /lag/track` request. */
export interface LagTrackExtender {
  extender_id: string;
  /**
   * For a reissue (`trackId` set): the dot's place in that track. Without
   * `trackId` positions follow the request order and this is not sent.
   */
  position?: number;
}

/** A6 `POST /lag/track` arguments: the 8 dots, then up to 4 spares. */
export interface LagTrackArgs {
  extenders: LagTrackExtender[];
  /**
   * Reissue (A6): fresh tickets in this track, one the caller was issued
   * within the last hour, for dots it already holds, so a dropped session
   * reconnects without leaving its track. Each extender then needs its
   * `position`. Sent as `track_id`.
   */
  trackId?: string;
}

/** A6: one dot's ticket. */
export interface LagTrackTicket {
  extender_id: string;
  /** what the browser connects to, as in A4; it may carry a port */
  host: string;
  /** the request order: 0-7 for the dots, 8-11 for the spares */
  position: number;
  /** the ticket text for the M4 session url */
  ticket: string;
}

export type LagTrackRefusalCode = "limit" | "unavailable";

/**
 * A6: a track's tickets, or why there are none. A ticket whose extender is
 * unavailable is left out.
 */
export interface LagTrackResult {
  track_id?: string;
  tickets: LagTrackTicket[];
  /** RFC 3339; every session must open before this */
  expires_at?: string;
  error?: LagApiRefusal & { code?: LagTrackRefusalCode | (string & {}) };
}

/** A3: the network an address belongs to, from GeoLite2-ASN. */
export interface LagAsn {
  number: number;
  organization: string;
}

/** `GET /my-ip-info`, with A3's `info.asn` (omitted when unknown). */
export type LagMyIpInfoResult = MyIPInfoResult & {
  info: { asn?: LagAsn };
};

// ---- the drift test's widget routes (lag.dev/DRIFTTEST.md P5)

/** W8: the token that runs a real measurement and stores nothing. */
export const DRIFT_TEST_DEMO_TOKEN = "demo";

/** P4: a drift test's state. */
export type DriftTestState = "created" | "active" | "closed" | "expired" | "failed";

/** P6: a run's state. */
export type DriftTestRunState = "started" | "processing" | "complete";

/**
 * P5: the IP place the radar centers on, as `/my-ip-info` gives it: the
 * address that started the run.
 */
export interface DriftTestPlace {
  /** ISO 3166-1 alpha-2, lower case */
  country_code?: string;
  country_name?: string;
  region?: string;
  city?: string;
  latitude?: number;
  longitude?: number;
}

/** P5: one of a run's extenders, with its ticket, host and place. */
export interface DriftTestTicket {
  extender_id: string;
  /** what the browser connects to (N1); it may carry a port */
  host: string;
  /** E2: 0-11 for the 12 extenders, 12-15 for the spares */
  position: number;
  /** the ticket text for the M4 session url */
  ticket: string;
  /** A4: the extender's place */
  location?: LagExtenderLocation;
  /** V3: the place's error, km (s_ext); the model takes 50 km when absent */
  place_error_km?: number;
}

/** P5 `POST /lag/drift-test/start` arguments. */
export interface DriftTestStartArgs {
  /** the token from `?t=` */
  token: string;
  /** W5: the parent's origin, when the frame knows it; sent as `parent_origin` */
  parentOrigin?: string | null;
}

/**
 * P5: why a start was refused: the token unknown, expired, closed or out of
 * runs, another of its runs still measuring, the origin not allowed, or a
 * limit (P7, W8).
 */
export type DriftTestStartRefusalCode =
  | "unknown"
  | "expired"
  | "closed"
  | "no_runs_left"
  | "measuring"
  | "origin"
  | "limit"
  | "unavailable";

/**
 * P5: a run started: its track and tickets, the IP place and how long it may
 * measure; or why not. The demo token's (W8) carries `demo: true` and no
 * `drift_test_id`, and its tickets carry `NoReport`.
 */
export interface DriftTestStartResult {
  /** the drift test; absent for the demo token */
  drift_test_id?: string;
  /** W8: the demo token; `finish` and `result` refuse it */
  demo?: boolean;
  /** the run started, from 1 */
  run?: number;
  /** runs the token has left after this one */
  runs_left?: number;
  track_id?: string;
  tickets: DriftTestTicket[];
  /** the IP place */
  ip?: DriftTestPlace;
  /** V10's cap, 12000 */
  measure_ms?: number;
  /** RFC 3339; every session must open before this */
  expires_at?: string;
  error?: LagApiRefusal & { code?: DriftTestStartRefusalCode | (string & {}) };
}

/** P5 `POST /lag/drift-test/finish` arguments. */
export interface DriftTestFinishArgs {
  token: string;
  run: number;
  /** V10: why the frame ended the run */
  stop: StopReason;
}

/** P5: `finish` is idempotent. */
export interface DriftTestFinishResult {
  run?: number;
  run_state?: DriftTestRunState;
  error?: LagApiRefusal;
}

/** P5 `POST /lag/drift-test/result` arguments. */
export interface DriftTestResultArgs {
  token: string;
}

/**
 * P5: only what the widget shows, never the drift country, a distance or
 * the fit (decision 7).
 */
export interface DriftTestResultResult {
  state?: DriftTestState;
  /** the token's latest run */
  run?: number;
  run_state?: DriftTestRunState;
  /** null until a run has completed */
  verdict?: DriftVerdict | null;
  ip_country_name?: string;
  /** the runs the verdict pools (V7), which the frame keeps on its radar (V9) */
  runs_in_verdict: number[];
  retest_allowed?: boolean;
  runs_left?: number;
  error?: LagApiRefusal;
}

const DRIFT_STOP_REASONS: readonly string[] = ["converged", "clear", "interrupted", "cap"];

const driftToken = (call: string, token: unknown): string => {
  if (typeof token !== "string" || token === "") {
    throw new TypeError(`${call}: a token is required`);
  }
  return token;
};

// The api may answer a refusal with a non-2xx status and the refusal in the
// body; either way the caller gets it as a result, as the routes that answer
// 200 with `error` already do.
const isRefusalBody = (body: unknown): body is { error: LagApiRefusal } => {
  if (typeof body !== "object" || body === null) {
    return false;
  }
  const error = (body as { error?: unknown }).error;
  return typeof error === "object" && error !== null && typeof (error as { code?: unknown }).code === "string";
};

const refusalAsResult = async <R>(call: Promise<R>): Promise<R> => {
  try {
    return await call;
  } catch (error) {
    if (error instanceof URNetworkApiError && error.kind === "http" && isRefusalBody(error.body)) {
      return error.body as R;
    }
    throw error;
  }
};

/**
 * The lag routes on the URnetwork api (default https://api.bringyour.com, or
 * `baseURL`). `token` is the guest's client jwt, e.g.
 * `() => guestSession.token()`; /lag/session and /my-ip-info send none.
 */
export class LagApiClient extends URNetworkApiClientBase {
  /** A1: create a lag guest network and client (no auth). Never retried. */
  lagSession(options?: RequestOptions): Promise<LagSessionResult> {
    return refusalAsResult(
      this.request<LagSessionResult>(
        { method: "POST", path: "/lag/session", auth: "none", body: {} },
        options,
      ),
    );
  }

  /** A4: extenders that serve the measurement api now (client jwt). */
  async lagExtenders(options?: RequestOptions): Promise<LagExtendersResult> {
    const result = await refusalAsResult(
      this.request<Partial<LagExtendersResult>>(
        { method: "GET", path: "/lag/extenders", auth: "bearer" },
        options,
      ),
    );
    const extenders = Array.isArray(result.extenders) ? result.extenders : [];
    return {
      ...result,
      // `ips` is always a list, even when the api sent none
      extenders: extenders.map((extender) => (Array.isArray(extender.ips) ? extender : { ...extender, ips: [] })),
    };
  }

  /**
   * A6: tickets for a track's dots and spares, in request order; or, with
   * `trackId`, fresh tickets at the given positions in that track (client
   * jwt).
   */
  async lagTrack(args: LagTrackArgs, options?: RequestOptions): Promise<LagTrackResult> {
    // an extender is named by its id, and in a reissue by its position too:
    // nothing else a caller holds about it, such as an address, goes in
    let body: object;
    if (args.trackId === undefined) {
      body = { extenders: args.extenders.map(({ extender_id }) => ({ extender_id })) };
    } else {
      if (typeof args.trackId !== "string" || args.trackId === "") {
        throw new TypeError("lagTrack: a reissue's trackId must be a track id");
      }
      body = {
        track_id: args.trackId,
        extenders: args.extenders.map(({ extender_id, position }) => {
          if (!(Number.isSafeInteger(position) && 0 <= position!)) {
            throw new TypeError(`lagTrack: a reissue needs each extender's position (${extender_id})`);
          }
          return { extender_id, position };
        }),
      };
    }
    const result = await refusalAsResult(
      this.request<Partial<LagTrackResult>>(
        { method: "POST", path: "/lag/track", auth: "bearer", body },
        options,
      ),
    );
    return { ...result, tickets: Array.isArray(result.tickets) ? result.tickets : [] };
  }

  /** The caller's address, location and (A3) network (no auth). */
  myIpInfo(options?: RequestOptions): Promise<LagMyIpInfoResult> {
    return this.request<LagMyIpInfoResult>(
      { method: "GET", path: "/my-ip-info", auth: "none" },
      options,
    );
  }

  /**
   * P5: start the token's next run (the first, or a retest), with its
   * tickets. The token is the only credential, and it travels in the body,
   * never the url. Never retried.
   */
  async driftTestStart(args: DriftTestStartArgs, options?: RequestOptions): Promise<DriftTestStartResult> {
    const body: { token: string; parent_origin?: string } = { token: driftToken("driftTestStart", args.token) };
    if (typeof args.parentOrigin === "string" && args.parentOrigin !== "") {
      body.parent_origin = args.parentOrigin;
    }
    const result = await refusalAsResult(
      this.request<Partial<DriftTestStartResult>>(
        { method: "POST", path: "/lag/drift-test/start", auth: "none", body },
        options,
      ),
    );
    return { ...result, tickets: Array.isArray(result.tickets) ? result.tickets : [] };
  }

  /**
   * P5: the frame closed a run's sessions, and why (V10). Idempotent. The
   * api refuses the demo token.
   */
  driftTestFinish(args: DriftTestFinishArgs, options?: RequestOptions): Promise<DriftTestFinishResult> {
    const token = driftToken("driftTestFinish", args.token);
    if (!(Number.isSafeInteger(args.run) && args.run >= 1)) {
      throw new TypeError(`driftTestFinish: run must be a run number, got ${args.run}`);
    }
    if (!DRIFT_STOP_REASONS.includes(args.stop)) {
      throw new TypeError(`driftTestFinish: stop must be one of ${DRIFT_STOP_REASONS.join(", ")}, got ${args.stop}`);
    }
    return refusalAsResult(
      this.request<DriftTestFinishResult>(
        {
          method: "POST",
          path: "/lag/drift-test/finish",
          auth: "none",
          body: { token, run: args.run, stop: args.stop },
        },
        options,
      ),
    );
  }

  /** P5: what the widget shows. The api refuses the demo token. */
  async driftTestResult(args: DriftTestResultArgs, options?: RequestOptions): Promise<DriftTestResultResult> {
    const token = driftToken("driftTestResult", args.token);
    const result = await refusalAsResult(
      this.request<Partial<DriftTestResultResult>>(
        { method: "POST", path: "/lag/drift-test/result", auth: "none", body: { token } },
        options,
      ),
    );
    return { ...result, runs_in_verdict: Array.isArray(result.runs_in_verdict) ? result.runs_in_verdict : [] };
  }
}

export function createLagApiClient(config: URNetworkApiClientConfig = {}): LagApiClient {
  return new LagApiClient(config);
}
