// The runtime under the generated URnetwork api client
// (src/generated/client.ts, from connect/api/bringyour.yml by gen_openapi.go).
//
// Dependency-free and DOM-free: it needs only fetch, AbortController,
// URLSearchParams and setTimeout, so it runs in a page, a MV3 service
// worker, a web worker or node >= 18.

import { fetchWithGetRetry, type FetchLike, type GetRetryOptions } from "./utils/fetch_retry";

export type { FetchLike, GetRetryOptions } from "./utils/fetch_retry";

export const DEFAULT_API_BASE_URL = "https://api.bringyour.com";

/**
 * How an operation authenticates, from its OpenAPI `security`:
 * - `bearer`: the network JWT (BearerAuth) — the configured token is attached
 * - `admin`: an admin bearer (AdminBearerAuth), never the network JWT — pass
 *   it per call as `options.token`
 * - `operator`: the probe fleet's shared secret — pass
 *   `options.headers["X-UR-Operator-Secret"]`
 * - `basic`: a webhook's HTTP Basic credential — pass `options.headers`
 * - `none`: public; no Authorization is sent unless `options.token` is given
 */
// "optional": the configured bearer is sent when there is one, and the call
// works without it (e.g. /stats/points-leaderboard adds the caller's own row)
export type ApiAuthScheme = "bearer" | "optional" | "admin" | "operator" | "basic" | "none";

export type ApiBodyType = "json" | "form" | "ndjson" | "binary";
export type ApiResponseType = "json" | "text" | "blob" | "none";

/** An operation as the generated client describes it to the runtime. */
export interface ApiOperation {
  /** the spec operationId */
  id: string;
  method: string;
  /** the path template, e.g. `/key/{clientId}` */
  path: string;
  auth: ApiAuthScheme;
  body?: ApiBodyType;
  response: ApiResponseType;
  /** parameter names by location (keys of the params argument); path
   * parameter names are read from the `{name}` segments of `path` */
  query?: readonly string[];
  header?: readonly string[];
}

interface NormalizedOperation {
  id: string;
  method: string;
  path: string;
  auth: ApiAuthScheme;
  body?: ApiBodyType;
  response: ApiResponseType;
  pathParams: readonly string[];
  queryParams: readonly string[];
  headerParams: readonly string[];
}

/** A token, or a function returning the current one (sync or async). */
export type ApiTokenSource =
  | string
  | null
  | undefined
  | (() => string | null | undefined | Promise<string | null | undefined>);

export interface URNetworkApiClientConfig {
  /** api origin; default https://api.bringyour.com. A trailing slash is ignored. */
  baseURL?: string;
  /**
   * The network JWT (`by_jwt`, or a client JWT) attached as
   * `Authorization: Bearer` to every operation whose security is BearerAuth.
   * A function is called per request, so a refreshed token is picked up
   * without rebuilding the client.
   */
  token?: ApiTokenSource;
  /** default: globalThis.fetch, looked up per request */
  fetch?: FetchLike;
  /** headers added to every request (e.g. X-Client-Version) */
  headers?: Record<string, string> | (() => Record<string, string>);
  /**
   * Abort a request that has not completed after this many milliseconds and
   * reject with URNetworkApiError kind "timeout". Default: no timeout.
   */
  timeoutMs?: number;
  /**
   * GET retry on 502/503 and network failures (fetchWithGetRetry); non-GET
   * requests are never replayed. `false` disables. Default: one jittered retry.
   */
  retry?: Omit<GetRetryOptions, "fetchImpl"> | false;
}

export interface RequestOptions {
  /**
   * Overrides the configured token for this call. A string is sent as
   * `Authorization: Bearer` whatever the operation's scheme (use it for
   * admin bearers, or to authenticate a public route); `null` sends none.
   */
  token?: string | null;
  /** headers for this call; they override config.headers */
  headers?: Record<string, string>;
  signal?: AbortSignal;
  /** overrides config.timeoutMs for this call */
  timeoutMs?: number;
}

export type URNetworkApiErrorKind = "http" | "network" | "timeout" | "parse";

/**
 * Every failure of a generated client call, except a caller's own abort,
 * which rejects with the signal's reason (an AbortError) untouched.
 *
 * - `http`: a non-2xx response. `status`, `statusText` and `body` (the parsed
 *   JSON when it parses, else the text) are set. `message` is the server's
 *   `error.message` when the body carries one.
 * - `network`: fetch itself failed (`status` 0, `cause` is the fetch error)
 * - `timeout`: the request outlived `timeoutMs` and was aborted (`status` 0).
 *   For a non-GET the server may still have committed it.
 * - `parse`: a 2xx whose body is not the JSON the operation promises
 *
 * Note: most URnetwork routes answer a handled failure as 200 with an
 * `error` field in the body; that is a resolved result, not this error.
 */
export class URNetworkApiError extends Error {
  readonly kind: URNetworkApiErrorKind;
  readonly status: number;
  readonly statusText: string;
  readonly body: unknown;
  /** the raw response text of an `http` or `parse` failure ("" otherwise) */
  readonly bodyText: string;
  readonly operationId: string;
  readonly method: string;
  readonly url: string;
  declare readonly cause?: unknown;

  constructor(init: {
    kind: URNetworkApiErrorKind;
    message: string;
    status?: number;
    statusText?: string;
    body?: unknown;
    bodyText?: string;
    operationId: string;
    method: string;
    url: string;
    cause?: unknown;
  }) {
    super(init.message);
    this.name = "URNetworkApiError";
    this.kind = init.kind;
    this.status = init.status ?? 0;
    this.statusText = init.statusText ?? "";
    this.body = init.body;
    this.bodyText = init.bodyText ?? "";
    this.operationId = init.operationId;
    this.method = init.method;
    this.url = init.url;
    if (init.cause !== undefined) {
      Object.defineProperty(this, "cause", {
        value: init.cause,
        enumerable: false,
        writable: true,
        configurable: true,
      });
    }
  }

  get isTimeout(): boolean {
    return this.kind === "timeout";
  }
}

export function isURNetworkApiError(error: unknown): error is URNetworkApiError {
  return error instanceof URNetworkApiError;
}

const isAbortError = (error: unknown): boolean =>
  typeof error === "object" &&
  error !== null &&
  (error as { name?: unknown }).name === "AbortError";

const serverMessage = (body: unknown): string | undefined => {
  if (typeof body !== "object" || body === null) {
    return undefined;
  }
  const error = (body as { error?: unknown }).error;
  if (typeof error === "string" && error !== "") {
    return error;
  }
  if (typeof error === "object" && error !== null) {
    const message = (error as { message?: unknown }).message;
    if (typeof message === "string" && message !== "") {
      return message;
    }
  }
  const message = (body as { message?: unknown }).message;
  if (typeof message === "string" && message !== "") {
    return message;
  }
  return undefined;
};

const appendValue = (search: URLSearchParams, name: string, value: unknown) => {
  if (value === undefined || value === null) {
    return;
  }
  if (Array.isArray(value)) {
    for (const v of value) {
      appendValue(search, name, v);
    }
    return;
  }
  search.append(name, typeof value === "object" ? JSON.stringify(value) : String(value));
};

const normalize = (op: ApiOperation): NormalizedOperation => {
  const pathParams: string[] = [];
  op.path.replace(/\{([^}]+)\}/g, (_m, name: string) => {
    pathParams.push(name);
    return "";
  });
  return {
    id: op.id,
    method: op.method.toUpperCase(),
    path: op.path,
    auth: op.auth,
    body: op.body,
    response: op.response,
    pathParams,
    queryParams: op.query ?? [],
    headerParams: op.header ?? [],
  };
};

/** A request for an operation (or a route) the generated client does not cover. */
export interface ApiRequest {
  method: string;
  /** path template; `{name}` segments are filled from `params` */
  path: string;
  /** default "bearer" */
  auth?: ApiAuthScheme;
  params?: Record<string, unknown>;
  /** param names sent as query (default: every param not in the path) */
  query?: readonly string[];
  header?: readonly string[];
  body?: unknown;
  /** default "json" when a body is given */
  bodyType?: ApiBodyType;
  /** default "json" */
  responseType?: ApiResponseType;
}

export class URNetworkApiClientBase {
  readonly baseURL: string;
  private token: ApiTokenSource;
  private readonly config: URNetworkApiClientConfig;

  constructor(config: URNetworkApiClientConfig = {}) {
    this.config = config;
    this.baseURL = (config.baseURL || DEFAULT_API_BASE_URL).replace(/\/+$/, "");
    this.token = config.token;
  }

  /** Replace the network JWT (or token function) used for BearerAuth routes. */
  setToken(token: ApiTokenSource): void {
    this.token = token;
  }

  /**
   * Call a route by hand — for a path/content type the generated methods do
   * not cover. Same auth, retry, timeout and error behavior as the methods.
   */
  request<R = unknown>(request: ApiRequest, options?: RequestOptions): Promise<R> {
    const pathParams = new Set<string>();
    request.path.replace(/\{([^}]+)\}/g, (_m, name: string) => {
      pathParams.add(name);
      return "";
    });
    const header = request.header ?? [];
    const query =
      request.query ??
      Object.keys(request.params ?? {}).filter((k) => !pathParams.has(k) && !header.includes(k));
    return this.call<R>(
      {
        id: `${request.method.toUpperCase()} ${request.path}`,
        method: request.method,
        path: request.path,
        auth: request.auth ?? "bearer",
        body: request.body === undefined ? undefined : (request.bodyType ?? "json"),
        response: request.responseType ?? "json",
        query,
        header,
      },
      request.params,
      request.body,
      options,
    );
  }

  protected async call<R>(
    operation: ApiOperation,
    params: object | undefined,
    body: unknown,
    options?: RequestOptions,
  ): Promise<R> {
    const op = normalize(operation);
    const values = (params ?? {}) as Record<string, unknown>;

    // url
    let path = op.path;
    for (const name of op.pathParams) {
      const value = values[name];
      if (value === undefined || value === null || value === "") {
        throw new TypeError(`${op.id}: missing path parameter "${name}"`);
      }
      path = path.replace(`{${name}}`, encodeURIComponent(String(value)));
    }
    const search = new URLSearchParams();
    for (const name of op.queryParams) {
      appendValue(search, name, values[name]);
    }
    const qs = search.toString();
    const url = `${this.baseURL}${path}${qs ? `?${qs}` : ""}`;

    // headers
    const headers: Record<string, string> = {};
    const configHeaders =
      typeof this.config.headers === "function" ? this.config.headers() : this.config.headers;
    Object.assign(headers, configHeaders);
    for (const name of op.headerParams) {
      const value = values[name];
      if (value !== undefined && value !== null) {
        headers[name] = String(value);
      }
    }

    let token: string | null | undefined;
    if (options && "token" in options) {
      token = options.token;
    } else if (op.auth === "bearer" || op.auth === "optional") {
      token = typeof this.token === "function" ? await this.token() : this.token;
    }
    if (token) {
      headers["Authorization"] = `Bearer ${token}`;
    }

    // body
    let requestBody: BodyInit | undefined;
    if (op.body && body !== undefined) {
      switch (op.body) {
        case "json":
          headers["Content-Type"] = "application/json";
          requestBody = JSON.stringify(body);
          break;
        case "form": {
          const form = new URLSearchParams();
          for (const [name, value] of Object.entries(body as Record<string, unknown>)) {
            appendValue(form, name, value);
          }
          requestBody = form;
          break;
        }
        case "ndjson":
          headers["Content-Type"] = "application/x-ndjson";
          requestBody = body as BodyInit;
          break;
        case "binary":
          headers["Content-Type"] = "application/octet-stream";
          requestBody = body as BodyInit;
          break;
      }
    }
    Object.assign(headers, options?.headers);

    // cancellation: the caller's signal and the timeout share one controller
    const timeoutMs = options?.timeoutMs ?? this.config.timeoutMs;
    const callerSignal = options?.signal;
    let signal = callerSignal;
    let timedOut = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let unlink: (() => void) | undefined;
    if (timeoutMs !== undefined && 0 < timeoutMs) {
      const controller = new AbortController();
      if (callerSignal) {
        if (callerSignal.aborted) {
          controller.abort(callerSignal.reason);
        } else {
          const onAbort = () => controller.abort(callerSignal.reason);
          callerSignal.addEventListener("abort", onAbort, { once: true });
          unlink = () => callerSignal.removeEventListener("abort", onAbort);
        }
      }
      timer = setTimeout(() => {
        if (!controller.signal.aborted) {
          timedOut = true;
          controller.abort();
        }
      }, timeoutMs);
      signal = controller.signal;
    }

    const fail = (init: Omit<ConstructorParameters<typeof URNetworkApiError>[0], "operationId" | "method" | "url">) =>
      new URNetworkApiError({ ...init, operationId: op.id, method: op.method, url });

    const fetchImpl: FetchLike =
      this.config.fetch ?? ((input, init) => globalThis.fetch(input, init));
    const retry = this.config.retry;

    try {
      let response: Response;
      try {
        response = await fetchWithGetRetry(
          url,
          { method: op.method, headers, body: requestBody, signal },
          retry === false ? { retryCount: 0, fetchImpl } : { ...retry, fetchImpl },
        );
      } catch (error) {
        if (timedOut) {
          throw fail({
            kind: "timeout",
            message: `${op.method} ${op.path} timed out after ${timeoutMs}ms`,
            cause: error,
          });
        }
        if (isAbortError(error) || callerSignal?.aborted) {
          throw error;
        }
        throw fail({
          kind: "network",
          message: error instanceof Error ? error.message : `${op.method} ${op.path} failed`,
          cause: error,
        });
      }

      if (!response.ok) {
        let text = "";
        try {
          text = await response.text();
        } catch {
          // the body is gone; the status is the error
        }
        let parsed: unknown = text;
        try {
          parsed = text ? JSON.parse(text) : undefined;
        } catch {
          // not json (a gateway html page, a text/plain 429)
        }
        throw fail({
          kind: "http",
          message:
            serverMessage(parsed) ??
            `${op.method} ${op.path} failed: HTTP ${response.status}${response.statusText ? ` ${response.statusText}` : ""}`,
          status: response.status,
          statusText: response.statusText,
          body: parsed,
          bodyText: text,
        });
      }

      switch (op.response) {
        case "none":
          try {
            await response.body?.cancel();
          } catch {
            // best-effort release
          }
          return undefined as R;
        case "text":
          return (await response.text()) as R;
        case "blob":
          return (await response.blob()) as R;
        case "json":
        default: {
          const text = await response.text();
          if (response.status === 204 || text.trim() === "") {
            // an empty 2xx: the legacy contract returned {} here
            return {} as R;
          }
          try {
            return JSON.parse(text) as R;
          } catch (error) {
            throw fail({
              kind: "parse",
              message: `${op.method} ${op.path}: expected a JSON response but got: ${text.substring(0, 100)}`,
              status: response.status,
              statusText: response.statusText,
              body: text,
              bodyText: text,
              cause: error,
            });
          }
        }
      }
    } catch (error) {
      if (timedOut && !(error instanceof URNetworkApiError)) {
        // the timeout fired while the body was being read
        throw fail({
          kind: "timeout",
          message: `${op.method} ${op.path} timed out after ${timeoutMs}ms`,
          cause: error,
        });
      }
      throw error;
    } finally {
      if (timer !== undefined) {
        clearTimeout(timer);
      }
      unlink?.();
    }
  }
}
