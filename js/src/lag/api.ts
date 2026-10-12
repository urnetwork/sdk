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
}

/** A6 `POST /lag/track` arguments: the 8 dots, then up to 4 spares. */
export interface LagTrackArgs {
  extenders: LagTrackExtender[];
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

  /** A6: tickets for a track's dots and spares, in request order (client jwt). */
  async lagTrack(args: LagTrackArgs, options?: RequestOptions): Promise<LagTrackResult> {
    // an extender is named by its id alone: nothing else a caller holds
    // about it, such as an address, goes into the request
    const body: LagTrackArgs = {
      extenders: args.extenders.map(({ extender_id }) => ({ extender_id })),
    };
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
}

export function createLagApiClient(config: URNetworkApiClientConfig = {}): LagApiClient {
  return new LagApiClient(config);
}
