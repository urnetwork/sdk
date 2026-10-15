// The lag.dev session client (lag.dev/PLAN.md S5): the browser side of an
// extender's measurement api (M4-M6).
//
// A session opens a control WebSocket with its ticket and stays open until
// close(). From hello on, the extender pings it, one ping in flight, about
// four a second, and reports every round trip. The browser's network stack
// answers the pings without page script, so jank never enters them. Later,
// on demand, the session runs its one load: a download over maxStreams
// fetches, then an upload over maxStreams WebSockets, each direction counted
// from its first byte, while the pings go on and show what the load does.

import { lagDownloadSources, type LagDownloadSource, type LagDownloadSourceFactory } from "./downloadSource";
import { importLagUploadKey, LagKeystream, LAG_UPLOAD_MESSAGE_BYTES } from "./keystream";
import {
  bytesToMbps,
  LAG_SPREAD_MAX_MS,
  LagIntervalSeries,
  LagPingWindow,
  percentiles,
  type LagPercentiles,
} from "./stats";

/** The session's parameters, from the extender's hello (M4). */
export interface LagHello {
  /** base64url, 16 bytes: names the session in the download and upload urls */
  session: string;
  /** base64url, 16 bytes: the AES-128 key of the upload keystream (M6) */
  uploadKey: string;
  /** the extender's target ping interval */
  pingIntervalMs: number;
  /** the progress interval, and the load series' resolution */
  intervalMs: number;
  downloadMs: number;
  uploadMs: number;
  /** streams per direction */
  maxStreams: number;
}

export type LagSessionState = "connecting" | "open" | "closed";

export type LagLoadDirection = "download" | "upload";

export type LagSessionErrorCode =
  // from the extender, ending the session (M4)
  | "ticket"
  | "reused"
  | "sessions"
  | "origin"
  | "internal"
  // from the extender, refusing a load; the session goes on pinging (M4)
  | "busy"
  | "budget"
  // the browser's own
  /** the control connection failed, or no hello came, before the session opened */
  | "connect"
  /** the control connection closed under an open session */
  | "dropped"
  /** nothing came from the extender for `silenceMs`, so the session was closed */
  | "silent"
  /** the extender sent what M4 does not allow */
  | "protocol"
  /** the session closed, or was closed, while the load ran */
  | "closed"
  /** the session's one load is running or has run */
  | "used"
  /** a direction of the load moved no data */
  | "download"
  | "upload";

/**
 * Why a session ended or a load failed. A refused load (`busy`, `budget`)
 * carries the extender's `retryAfterMs`; a load that failed after moving
 * data carries what it measured as `partial`.
 */
export class LagSessionError extends Error {
  readonly code: LagSessionErrorCode | (string & {});
  readonly retryAfterMs: number;
  /** the control connection's close code, when it closed */
  closeCode?: number;
  /** what the failed load measured before it failed */
  partial?: LagLoadResult;

  constructor(
    code: LagSessionErrorCode | (string & {}),
    message: string,
    init: { retryAfterMs?: number; closeCode?: number; partial?: LagLoadResult; cause?: unknown } = {},
  ) {
    super(message);
    this.name = "LagSessionError";
    this.code = code;
    this.retryAfterMs = init.retryAfterMs ?? 0;
    this.closeCode = init.closeCode;
    this.partial = init.partial;
    if (init.cause !== undefined) {
      Object.defineProperty(this, "cause", { value: init.cause, enumerable: false, writable: true, configurable: true });
    }
  }
}

/** One direction of a load (S5). */
export interface LagDirectionResult {
  /** whole bytes per interval from the direction's first byte, as M8 aligns them */
  intervalBytes: number[];
  /** Mbps per interval */
  mbps: number[];
  /** p50 and p95 of `mbps` by nearest rank; undefined without an interval */
  percentiles?: LagPercentiles;
  /** bytes moved: read by the browser, or counted by the extender for the upload */
  bytes: number;
  /** streams that carried data (download) or opened (upload) */
  streams: number;
  /** when the browser saw the direction's first byte, as Date.now() */
  startedAt?: number;
  /** from the first byte to the last, ms */
  durationMs: number;
}

/** What the session's load measured (S5). */
export interface LagLoadResult {
  intervalMs: number;
  /** what the browser read, its series as its `received` reports gave it to the extender (M4) */
  download: LagDirectionResult;
  /**
   * The extender's counts: its own series, as its report signs it and the
   * API stores it (M4 `loadResult` `uploadBytes`), or from an extender that
   * sends none, the series of its `uploaded` reports
   */
  upload: LagDirectionResult;
  /** the extender found every upload message it checked to be the expected keystream */
  uploadVerified: boolean;
  /** the extender's endReason (M4 `loadResult`), e.g. "complete"; null when none came */
  endReason: string | null;
}

/** The session opened: the extender's hello arrived. */
export interface LagOpenEvent {
  type: "open";
  hello: LagHello;
  /**
   * Date.now() when the hello was handled: the session's first estimate of
   * its time origin. `LagSession.openedAt` holds the refined one.
   */
  at: number;
}

/** One ping's result (M4). */
export interface LagRttEvent {
  type: "rtt";
  /** the ping's send time, ms since the session opened, on the extender's clock */
  t: number;
  /** the round trip in ms, or null when the ping was lost (no pong within 5 s) */
  ms: number | null;
  lost: boolean;
  /**
   * The send time on the browser's clock, as Date.now(): the session's
   * origin plus `t`, as the origin stood when the event fired. A later round
   * trip can move the origin a little earlier; `LagSession.openedAt` and
   * `LagSession.pings` follow it.
   */
  at: number;
}

/**
 * The load moved to a phase, at the moment data began to move, so that load
 * windows leave out connection setup: "download" at the download's first
 * byte, "upload" at the upload's first message, and "done" once the load is
 * over, however it ended. A load that never moved data (refused, or failed
 * to connect) has none of them.
 */
export interface LagLoadEvent {
  type: "load";
  phase: LagLoadDirection | "done";
  /** Date.now() */
  at: number;
}

/**
 * A load interval in one direction, once its value is final (S5): a
 * download interval when it settles and its `received` report goes out
 * (M4), an upload interval when the live series' latest point is at or past
 * its end, or the window is over. One per interval, in order.
 */
export interface LagProgressEvent {
  type: "progress";
  direction: LagLoadDirection;
  /** the interval, counted from the direction's first byte */
  interval: number;
  /** its rate */
  bytesPerSecond: number;
  mbps: number;
  /**
   * How far into the direction the load is as the event goes out, in ms
   * since its first byte: for the download, the page's clock, up to
   * downloadMs, which it is once the window has closed; for the upload, the
   * extender's latest `uploaded` ms. It never decreases.
   */
  ms: number;
  /** the direction's length (hello's downloadMs or uploadMs) */
  durationMs: number;
  /** whole bytes in this direction through the interval; it never decreases */
  bytes: number;
  /** when the browser saw the direction's first byte, as Date.now() */
  startedAt: number;
}

export interface LagLoadResultEvent {
  type: "loadResult";
  result: LagLoadResult;
}

/**
 * An error: `fatal` ones end the session (a closed event follows); a load
 * refusal does not.
 */
export interface LagErrorEvent {
  type: "error";
  error: LagSessionError;
  fatal: boolean;
}

/** The session ended. */
export interface LagClosedEvent {
  type: "closed";
  /** the control connection's close code */
  code: number;
  reason: string;
  /** close() ended it */
  byClient: boolean;
  /** why, when it was not close() */
  error?: LagSessionError;
  /** Date.now() */
  at: number;
}

export type LagSessionEvent =
  | LagOpenEvent
  | LagRttEvent
  | LagLoadEvent
  | LagProgressEvent
  | LagLoadResultEvent
  | LagErrorEvent
  | LagClosedEvent;

export interface LagSessionOptions {
  /**
   * What the track's ticket says to connect to (A6): the extender's
   * measurement name, `<extender_id>.extender.bringyour.com` (N1), with the
   * port a local test stack may add. It is reached over https and wss. An
   * origin such as http://127.0.0.1:8443 is used as given (tests, dev).
   */
  host: string;
  /** the ticket text (A6) */
  ticket: string;
  onEvent?: (event: LagSessionEvent) => void;
  /** default globalThis.WebSocket */
  WebSocket?: typeof WebSocket;
  /** default globalThis.fetch */
  fetch?: typeof fetch;
  /**
   * Where the download's streams are read (S5). By default a dedicated
   * Worker reads them, so a chunk is timed when it arrives however busy the
   * page is; the page reads them itself when there is no Worker, when it
   * cannot start, or when `fetch` is given (it cannot cross into a Worker).
   * false reads them on the page; a function supplies the Worker, for a
   * bundler that builds it some other way.
   */
  downloadWorker?: false | (() => Worker);
  /**
   * Close the session when nothing has come from the extender for this long
   * (default 15 s). Pings report at least every 5 s, lost or not.
   */
  silenceMs?: number;
  /** fail the session when no hello comes within this (default 10 s) */
  connectTimeoutMs?: number;
}

/** A session on one extender (S5); openLagSession() returns it. */
export interface LagSession {
  readonly host: string;
  readonly state: LagSessionState;
  /** the extender's hello, once open */
  readonly hello: LagHello | null;
  /**
   * When the session opened, as Date.now(): the origin of the rtt events'
   * `at`. It starts as the hello's handling time and moves earlier when a
   * round trip shows the session opened sooner (S5).
   */
  readonly openedAt: number | null;
  /** the last minute of pings, by send time on the browser's clock (`at`) */
  readonly pings: LagPingWindow;
  /** resolves with the hello; rejects with a LagSessionError when the session ends first */
  readonly ready: Promise<LagHello>;
  /** resolves with the closed event when the session ends */
  readonly closed: Promise<LagClosedEvent>;
  /**
   * Run the session's one load: the download, then the upload. A refused
   * load rejects with code `busy` or `budget` and `retryAfterMs`, and may be
   * started again later; the session goes on pinging either way.
   */
  startLoad(): Promise<LagLoadResult>;
  /** End the session: stop the pings, cut any load. */
  close(): void;
}

/** Open a session on one extender (lag.dev/PLAN.md S5, M4). */
export function openLagSession(options: LagSessionOptions): LagSession {
  return new LagSessionClient(options);
}

// a download with no first byte by then has failed
const FIRST_BYTE_TIMEOUT_MS = 10_000;
// how long the upload sockets get to open before the streams start without
// the ones still connecting (the extender waits 10 s for them)
const UPLOAD_OPEN_WAIT_MS = 3_000;
// The upload's first socket reaches the extender about this long before
// the download's window ends: the sockets start to open this long and two of
// the session's lowest round trips before it, for a socket's TCP and TLS
// handshakes before its request goes. The extender holds a stream that comes
// before its phase for up to 2 s (M4), and the phase opens once the
// download's window is final, at most LAG_SPREAD_MAX_MS after its end (M6,
// amendment 15). So the first socket waits there at most this long and
// LAG_SPREAD_MAX_MS, and a busy page's flush, well short of being refused;
// and when the window is final sooner, it is already waiting. Before
// amendment 15 the sockets opened 1 s before the end, which a window final
// a second late would hold into a refusal. The rest still connect after the
// first: a browser has one WebSocket handshake to a host in progress at a
// time (RFC 6455 4.1), and the extender holds the first until the phase
// opens. The keystreams start to generate then too.
const UPLOAD_EARLY_MS = 250;
// The upload's streams close when the extender's report says its window is
// over, which comes once its window is final, at most LAG_SPREAD_MAX_MS after
// its end (M4, M6, amendment 15). Without one they close this long after the
// window's estimated end, and done goes out.
const UPLOAD_SETTLE_MS = 2_000;
// how long the load waits for the extender's loadResult after done
const RESULT_TIMEOUT_MS = 5_000;
// a stream that fails to open (it can race the load message) is retried
const OPEN_RETRIES = 3;
// S5: the upload's backlog. bufferedAmount counts only what the page has not
// yet handed to the network stack. The kernel's send buffers hold far more
// (and on the local stack, its front too), so a backlog that bufferedAmount
// sized left tens of MB queued past the window. The streams are held instead,
// together, by what the extender has counted. Bytes sent but not yet
// counted may cover the path in flight (the base round trip at the counted
// rate), what has arrived since the extender's last report, and a backlog of
// UPLOAD_BACKLOG_MS at that rate, within its floor and ceiling, which keeps
// the bottleneck busy. That rate is the higher of the extender's last two
// reports, so a short dip does not starve the streams. They may also hold no
// more than the extender can still count by its window's end, as its reports
// place that on this page's clock (amendment 15), at its rate over the last
// UPLOAD_STEADY_MS, so they stop queueing in time to drain by then. Until its
// first report, the streams may send UPLOAD_START_BYTES.
const UPLOAD_BACKLOG_MS = 250;
const UPLOAD_BACKLOG_MIN_BYTES = 4 * LAG_UPLOAD_MESSAGE_BYTES;
const UPLOAD_BACKLOG_MAX_BYTES = 16 << 20;
const UPLOAD_STEADY_MS = 1000;
// Enough for the first report's round trip on a path of a few tens of Mbps,
// and to fill the window on one of 1.7 Mbps
const UPLOAD_START_BYTES = 16 * LAG_UPLOAD_MESSAGE_BYTES;
// what the streams may still hold when the extender's reports show no rate
const UPLOAD_MIN_UNVERIFIED_BYTES = 2 * LAG_UPLOAD_MESSAGE_BYTES;
const UPLOAD_MAX_UNVERIFIED_BYTES = 32 << 20;
// the base round trip when the session has no ping yet
const UPLOAD_DEFAULT_RTT_MS = 100;
const BUFFER_POLL_MS = 10;
// A stream that sends this long without a pause yields to the event loop, so
// timers and the page stay live whatever a socket's buffer reports.
const SEND_SLICE_MS = 8;
const WEBSOCKET_CONNECTING = 0;
const WEBSOCKET_OPEN = 1;

// Durations come from the monotonic clock. Event times are Date.now(), the
// clock a page keeps its own times in, so the two compare directly.
const monotonicNow = () => performance.now();
const epochNow = () => Date.now();
const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

interface Deferred<T> {
  promise: Promise<T>;
  resolve(value: T): void;
  reject(reason: unknown): void;
}

const deferred = <T>(): Deferred<T> => {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((a, b) => {
    resolve = a;
    reject = b;
  });
  // a rejection nobody waits for is not an error here
  promise.catch(() => undefined);
  return { promise, resolve, reject };
};

// Where a session connects. A ticket's host goes into the urls as it is,
// port and all (A4, A6), over https and wss; it must be a host with an
// optional port and nothing else. An http(s) origin is used as given.
const lagOrigins = (host: string): { http: string; ws: string } => {
  if (/^https?:\/\//i.test(host)) {
    const origin = new URL(host).origin;
    return { http: origin, ws: origin.replace(/^http/i, "ws") };
  }
  let parsed: URL | undefined;
  if (!/[\s/?#@\\]/.test(host)) {
    try {
      parsed = new URL(`https://${host}`);
    } catch {
      parsed = undefined;
    }
  }
  if (
    parsed === undefined ||
    parsed.hostname === "" ||
    parsed.pathname !== "/" ||
    parsed.search !== "" ||
    parsed.hash !== "" ||
    parsed.username !== "" ||
    parsed.password !== ""
  ) {
    throw new TypeError(`not an extender host: ${host}`);
  }
  return { http: `https://${host}`, ws: `wss://${host}` };
};

const closeCodeError = (code: number): LagSessionErrorCode | undefined => {
  switch (code) {
    case 4403:
      return "ticket";
    case 4429:
      return "sessions";
    case 4500:
      return "internal";
    default:
      return undefined;
  }
};

const isBase64Url = (value: unknown): value is string =>
  typeof value === "string" && /^[A-Za-z0-9_-]+={0,2}$/.test(value) && value.length <= 64;

// a whole number in [min, max], or `fallback` when the field is absent
const integerField = (value: unknown, fallback: number, min: number, max: number): number => {
  if (value === undefined) {
    return fallback;
  }
  return typeof value === "number" && Number.isInteger(value) && min <= value && value <= max ? value : NaN;
};

const parseHello = (message: Record<string, unknown>): LagHello | null => {
  if (!isBase64Url(message.session) || !isBase64Url(message.uploadKey)) {
    return null;
  }
  const hello: LagHello = {
    session: message.session,
    uploadKey: message.uploadKey,
    pingIntervalMs: integerField(message.pingIntervalMs, 250, 1, 60_000),
    intervalMs: integerField(message.intervalMs, 250, 10, 10_000),
    downloadMs: integerField(message.downloadMs, 5000, 1, 120_000),
    uploadMs: integerField(message.uploadMs, 5000, 1, 120_000),
    maxStreams: integerField(message.maxStreams, 4, 1, 16),
  };
  const numbers = [hello.pingIntervalMs, hello.intervalMs, hello.downloadMs, hello.uploadMs, hello.maxStreams];
  return numbers.every((n) => !Number.isNaN(n)) ? hello : null;
};

const finiteAtLeastZero = (value: unknown): number | undefined =>
  typeof value === "number" && Number.isFinite(value) && 0 <= value ? value : undefined;

const reportError = (error: unknown) => {
  const report = (globalThis as { reportError?: (error: unknown) => void }).reportError;
  if (typeof report === "function") {
    report(error);
  } else {
    console.error(error);
  }
};

class LagSessionClient implements LagSession {
  readonly host: string;
  readonly pings = new LagPingWindow();
  readonly ready: Promise<LagHello>;
  readonly closed: Promise<LagClosedEvent>;
  readonly httpOrigin: string;
  readonly wsOrigin: string;
  readonly fetchImpl: typeof fetch;
  readonly downloadSources: LagDownloadSourceFactory;
  readonly WebSocketImpl: typeof WebSocket;
  private stateValue: LagSessionState = "connecting";
  private helloValue: LagHello | null = null;
  private openedAtValue: number | null = null;
  private readonly readyDeferred = deferred<LagHello>();
  private readonly closedDeferred = deferred<LagClosedEvent>();
  private readonly onEvent?: (event: LagSessionEvent) => void;
  private readonly socket: WebSocket;
  private readonly silenceMs: number;
  private lastMessageAt = monotonicNow();
  private connectTimer?: ReturnType<typeof setTimeout>;
  private silenceTimer?: ReturnType<typeof setInterval>;
  private fatalTimer?: ReturnType<typeof setTimeout>;
  private fatal: LagSessionError | null = null;
  private loadState: "none" | "running" | "spent" = "none";
  private load: LagLoadRun | null = null;

  constructor(options: LagSessionOptions) {
    const origins = lagOrigins(options.host);
    const WebSocketImpl = options.WebSocket ?? globalThis.WebSocket;
    if (typeof WebSocketImpl !== "function") {
      throw new Error("WebSocket is not available");
    }
    this.host = options.host;
    this.httpOrigin = origins.http;
    this.wsOrigin = origins.ws;
    this.WebSocketImpl = WebSocketImpl;
    this.fetchImpl = options.fetch ?? ((input, init) => globalThis.fetch(input, init));
    this.downloadSources = lagDownloadSources({ fetch: options.fetch, worker: options.downloadWorker });
    this.onEvent = options.onEvent;
    this.silenceMs = options.silenceMs ?? 15_000;
    this.ready = this.readyDeferred.promise;
    this.closed = this.closedDeferred.promise;

    const socket = new WebSocketImpl(
      `${this.wsOrigin}/lag/v1/session?ticket=${encodeURIComponent(options.ticket)}`,
    );
    socket.binaryType = "arraybuffer";
    socket.addEventListener("message", (event) => this.onMessage(event.data));
    socket.addEventListener("close", (event) => this.onClose(event.code, event.reason));
    // an error event is always followed by a close event
    socket.addEventListener("error", () => undefined);
    this.socket = socket;
    this.connectTimer = setTimeout(
      () => this.fail(new LagSessionError("connect", "no hello came from the extender")),
      options.connectTimeoutMs ?? 10_000,
    );
  }

  get state(): LagSessionState {
    return this.stateValue;
  }

  get hello(): LagHello | null {
    return this.helloValue;
  }

  get openedAt(): number | null {
    return this.openedAtValue;
  }

  startLoad(): Promise<LagLoadResult> {
    if (this.stateValue === "closed") {
      return Promise.reject(new LagSessionError("closed", "the session is closed"));
    }
    if (this.loadState !== "none") {
      return Promise.reject(new LagSessionError("used", "the session's one load is running or has run"));
    }
    this.loadState = "running";
    const run = new LagLoadRun(this);
    this.load = run;
    return run.run().then(
      (result) => {
        this.loadState = "spent";
        this.load = null;
        return result;
      },
      (error: unknown) => {
        // a refused load never started, so it may be asked for again
        const refused = error instanceof LagSessionError && (error.code === "busy" || error.code === "budget");
        this.loadState = refused ? "none" : "spent";
        this.load = null;
        throw error;
      },
    );
  }

  close(): void {
    if (this.stateValue === "closed") {
      return;
    }
    try {
      this.socket.close(1000);
    } catch {
      // already closing
    }
    this.finish(1000, "", true, undefined);
  }

  /** Send a control message; false when the session is not open. */
  send(message: object): boolean {
    if (this.stateValue !== "open" || this.socket.readyState !== WEBSOCKET_OPEN) {
      return false;
    }
    try {
      this.socket.send(JSON.stringify(message));
      return true;
    } catch {
      return false;
    }
  }

  emit(event: LagSessionEvent): void {
    if (this.onEvent === undefined) {
      return;
    }
    try {
      this.onEvent(event);
    } catch (error) {
      // a broken handler must not break the measurement
      reportError(error);
    }
  }

  private onMessage(data: unknown): void {
    if (this.stateValue === "closed") {
      return;
    }
    this.lastMessageAt = monotonicNow();
    if (typeof data !== "string") {
      return;
    }
    let message: Record<string, unknown>;
    try {
      const parsed: unknown = JSON.parse(data);
      if (typeof parsed !== "object" || parsed === null) {
        return;
      }
      message = parsed as Record<string, unknown>;
    } catch {
      return;
    }
    // unknown types are ignored, so the extender can add some
    switch (message.type) {
      case "hello":
        this.onHello(message);
        return;
      case "rtt":
        this.onRtt(message);
        return;
      case "error":
        this.onError(message);
        return;
      case "uploaded":
        this.load?.onUploaded(message);
        return;
      case "loadResult":
        this.load?.onLoadResult(message);
        return;
    }
  }

  private onHello(message: Record<string, unknown>): void {
    if (this.helloValue !== null) {
      return;
    }
    const hello = parseHello(message);
    if (hello === null) {
      this.fail(new LagSessionError("protocol", "the extender's hello is malformed"));
      return;
    }
    clearTimeout(this.connectTimer);
    this.helloValue = hello;
    this.openedAtValue = epochNow();
    this.stateValue = "open";
    this.silenceTimer = setInterval(
      () => {
        if (this.silenceMs < monotonicNow() - this.lastMessageAt) {
          this.fail(new LagSessionError("silent", `nothing came from the extender for ${this.silenceMs} ms`));
        }
      },
      Math.max(50, Math.min(1000, this.silenceMs / 4)),
    );
    this.readyDeferred.resolve(hello);
    this.emit({ type: "open", hello, at: this.openedAtValue });
  }

  private onRtt(message: Record<string, unknown>): void {
    if (this.openedAtValue === null) {
      return;
    }
    const t = message.t;
    if (typeof t !== "number" || !Number.isFinite(t)) {
      return;
    }
    const lost = message.lost === true;
    const ms = lost ? null : finiteAtLeastZero(message.ms);
    if (ms === undefined) {
      return;
    }
    if (ms !== null) {
      // The extender had this pong t + ms after the session opened and
      // reported it at once, so the session opened no later than now less
      // t + ms. The earliest such bound is the origin: a stall while the
      // hello was handled made openedAt late, and would shift every ping.
      const opened = epochNow() - (t + ms);
      if (opened < this.openedAtValue) {
        this.pings.shift(opened - this.openedAtValue);
        this.openedAtValue = opened;
      }
    }
    const at = this.openedAtValue + t;
    this.pings.add({ t: at, ms });
    this.emit({ type: "rtt", t, ms, lost, at });
  }

  private onError(message: Record<string, unknown>): void {
    const code = typeof message.code === "string" && message.code !== "" ? message.code : "internal";
    const retryAfterMs = finiteAtLeastZero(message.retryAfterMs) ?? 0;
    if (code === "busy" || code === "budget") {
      const error = new LagSessionError(code, `the extender refused the load (${code})`, { retryAfterMs });
      this.emit({ type: "error", error, fatal: false });
      this.load?.onRefused(error);
      return;
    }
    if (this.fatal !== null) {
      return;
    }
    this.fatal = new LagSessionError(code, `the extender ended the session (${code})`, { retryAfterMs });
    this.emit({ type: "error", error: this.fatal, fatal: true });
    // the extender closes next; if it does not, this side does
    this.fatalTimer = setTimeout(() => this.fail(this.fatal!), 2000);
  }

  private onClose(code: number, reason: string): void {
    if (this.stateValue === "closed") {
      return;
    }
    let error = this.fatal;
    if (error === null) {
      const fallback = this.helloValue === null ? "connect" : "dropped";
      const name = closeCodeError(code) ?? fallback;
      error = new LagSessionError(name, `the extender's connection closed (${code}${reason ? ` ${reason}` : ""})`);
    }
    error.closeCode = code;
    this.finish(code, reason, false, error);
  }

  // A failure seen by this side: close the connection and end the session.
  private fail(error: LagSessionError): void {
    if (this.stateValue === "closed") {
      return;
    }
    try {
      this.socket.close(1000, error.code);
    } catch {
      // already closing
    }
    this.finish(1000, error.code, false, error);
  }

  private finish(code: number, reason: string, byClient: boolean, error: LagSessionError | undefined): void {
    if (this.stateValue === "closed") {
      return;
    }
    this.stateValue = "closed";
    clearTimeout(this.connectTimer);
    clearInterval(this.silenceTimer);
    clearTimeout(this.fatalTimer);
    if (error !== undefined && error !== this.fatal) {
      this.emit({ type: "error", error, fatal: true });
    }
    this.load?.onSessionClosed(error);
    if (this.helloValue === null) {
      this.readyDeferred.reject(error ?? new LagSessionError("closed", "the session closed before it opened"));
    }
    const event: LagClosedEvent = { type: "closed", code, reason, byClient, error, at: epochNow() };
    this.emit(event);
    this.closedDeferred.resolve(event);
  }
}

const EARLY = Symbol("early");

// One run of a session's load (M4-M6): the download, the upload, then the
// extender's loadResult.
class LagLoadRun {
  private readonly session: LagSessionClient;
  private hello!: LagHello;
  private download!: LagIntervalSeries;
  private upload!: LagIntervalSeries;
  // rejects when the load must stop: refused, or the session closed
  private readonly stopped = deferred<never>();
  private stopError: LagSessionError | null = null;
  // resolves when the extender ends the load before done (e.g. a mismatch)
  private readonly early = deferred<typeof EARLY>();
  private ended = false;
  private doneSent = false;
  private started = false;
  private doneEmitted = false;
  // the extender's loadResult; uploadBytes as it came, checked when used
  private result: { uploadVerified: boolean; endReason: string; uploadBytes: unknown } | null = null;
  private readonly resultArrived = deferred<void>();
  // imported while the download runs, so the upload need not wait for it
  private uploadKey?: Promise<CryptoKey>;
  private downloadSource: LagDownloadSource | null = null;
  // every chunk timed up to then (monotonic) has been handed over: always on
  // the page, a batch behind in the download's Worker (downloadSource.ts)
  private downloadDelivered = Number.NEGATIVE_INFINITY;
  private readonly downloadStreamsWithData = new Set<number>();
  // the streams that have ended
  private readonly downloadEnded = new Set<number>();
  // resolves when every stream has ended, or the download is cut
  private readonly downloadStreamsEnded = deferred<void>();
  // resolves when the download's window is final (M6, amendment 15): every
  // stream still open is done, or LAG_SPREAD_MAX_MS has passed since the
  // window's end
  private readonly downloadFinal = deferred<void>();
  // downloadMs has passed since the download's first byte
  private downloadWindowEnded = false;
  // no byte counts once the download is over
  private downloadClosed = false;
  private readonly sockets = new Set<WebSocket>();
  private readonly timers = new Set<ReturnType<typeof setTimeout>>();
  private downloadTick?: ReturnType<typeof setTimeout>;
  private downloadFirst?: number;
  private downloadStartedAt?: number;
  private downloadBytes = 0;
  private downloadStreams = 0;
  // the download intervals reported so far, and the whole bytes through them
  private downloadReported = 0;
  private downloadReportedBytes = 0;
  private uploadFirst?: number;
  private uploadStartedAt?: number;
  // the upload's sockets, from when they began to open (openUploadSockets),
  // and its streams' keystreams, from when they began to generate
  private uploadSockets: { opened: Array<WebSocket | null>; opening: Promise<void>[] } | null = null;
  private keystreams: LagKeystream[] | null = null;
  private uploadStopped = false;
  // the streams have started together; a socket opening later is not used
  private uploadStarted = false;
  private uploadStreams = 0;
  // bytes handed to the upload sockets, across streams
  private uploadSent = 0;
  // the extender's last reports in the window (ms since its first byte, and
  // bytes counted), and when the last came (monotonic)
  private reports: Array<{ ms: number; bytes: number }> = [];
  private lastReportAt?: number;
  // the earliest any report's ms could have been the extender's time as it
  // arrived (monotonic): the least, over the reports, of when each arrived
  // less its ms
  private reportsOrigin = Number.POSITIVE_INFINITY;
  // the session's lowest round trip when the upload started
  private baseRttMs = UPLOAD_DEFAULT_RTT_MS;
  private lastUploaded = { bytes: 0, ms: 0 };
  // a report has said the extender's window is over
  private uploadWindowOver = false;
  // resolves when the upload's streams close: on that report, or
  // UPLOAD_SETTLE_MS after the window's estimated end
  private readonly uploadClosed = deferred<void>();
  private uploadSettleTimer?: ReturnType<typeof setTimeout>;
  // the upload intervals that have had their progress event, and the whole
  // bytes through them
  private uploadReported = 0;
  private uploadReportedBytes = 0;

  constructor(session: LagSessionClient) {
    this.session = session;
  }

  async run(): Promise<LagLoadResult> {
    try {
      const hello = await this.until(this.session.ready);
      if (hello === EARLY) {
        throw new LagSessionError("protocol", "the extender ended a load it never started");
      }
      this.hello = hello;
      const intervals = (ms: number) => Math.max(1, Math.ceil(ms / hello.intervalMs));
      this.download = new LagIntervalSeries({
        intervalMs: hello.intervalMs,
        maxIntervals: intervals(hello.downloadMs),
        windowMs: hello.downloadMs,
      });
      this.upload = new LagIntervalSeries({ intervalMs: hello.intervalMs, maxIntervals: intervals(hello.uploadMs) });
      this.uploadKey = importLagUploadKey(hello.uploadKey);
      // a bad key fails the upload when it starts, not before
      this.uploadKey.catch(() => undefined);
      await this.runDownload();
      if (!this.ended) {
        await this.runUpload();
      }
      if (!this.ended) {
        this.sendDone();
        this.closeSockets();
        await this.until(Promise.race([this.resultArrived.promise, this.delay(RESULT_TIMEOUT_MS)]));
      }
      const result = this.measured();
      this.session.emit({ type: "loadResult", result });
      return result;
    } catch (error) {
      if (error instanceof LagSessionError && error.partial === undefined && this.moved()) {
        error.partial = this.measured();
      }
      throw error;
    } finally {
      this.emitDone();
      this.cleanup();
    }
  }

  onRefused(error: LagSessionError): void {
    this.stop(error);
  }

  onSessionClosed(cause: LagSessionError | undefined): void {
    this.stop(
      new LagSessionError("closed", "the session closed during the load", {
        closeCode: cause?.closeCode,
        cause,
      }),
    );
  }

  // M4 `uploaded`: the extender's count so far (`bytes`), at the report's
  // time (`ms`), with when the last counted byte arrived (`at`).
  onUploaded(message: Record<string, unknown>): void {
    const bytes = finiteAtLeastZero(message.bytes);
    const ms = finiteAtLeastZero(message.ms);
    if (bytes === undefined || ms === undefined || this.upload === undefined || this.stopError !== null) {
      return;
    }
    const { intervalMs, uploadMs } = this.hello;
    const now = monotonicNow();
    // The live series (S5): the bytes a report adds arrived since the point
    // before, so they spread evenly back to it, from `at`; from `ms` where
    // an older extender sends no `at`, though the report's time can be well
    // after the bytes came.
    const at = finiteAtLeastZero(message.at);
    this.upload.addCumulative(at === undefined ? ms : Math.min(at, ms), bytes);
    // flow control and the window's end go by `ms` and `bytes` alone
    const previous = this.lastUploaded;
    this.lastUploaded = { bytes: Math.max(previous.bytes, bytes), ms: Math.max(previous.ms, ms) };
    if (0 < ms - previous.ms) {
      this.reports = [...this.reports.slice(-7), { ms, bytes: Math.max(previous.bytes, bytes) }];
      this.lastReportAt = now;
    }
    // the report that came soonest after its time places the extender's
    // window on this page's clock (uploadWindowEnd)
    if (now - ms < this.reportsOrigin) {
      this.reportsOrigin = now - ms;
      this.armUploadSettle();
    }
    const over = uploadMs - intervalMs / 2 <= ms;
    if (over) {
      // the window's last intervals count, even where no byte came
      this.upload.extendTo(uploadMs);
    }
    this.reportUpload(over);
    if (over) {
      this.uploadWindowOver = true;
      if (this.uploadFirst !== undefined) {
        this.closeUpload();
      }
    }
  }

  onLoadResult(message: Record<string, unknown>): void {
    if (this.result !== null || this.stopError !== null) {
      return;
    }
    this.result = {
      uploadVerified: message.uploadVerified === true,
      endReason: typeof message.endReason === "string" ? message.endReason : "unknown",
      uploadBytes: message.uploadBytes,
    };
    this.resultArrived.resolve();
    if (!this.doneSent) {
      // the extender ended the load first, e.g. on an upload mismatch
      this.ended = true;
      this.uploadStopped = true;
      this.closeDownload();
      this.closeSockets();
      this.emitDone();
      this.early.resolve(EARLY);
    }
  }

  // Await `promise`, or stop: rejects when the load is stopped, and gives
  // EARLY when the extender ended it.
  private until<T>(promise: Promise<T>): Promise<T | typeof EARLY> {
    return Promise.race([promise, this.stopped.promise, this.early.promise]);
  }

  private stop(error: LagSessionError): void {
    if (this.stopError !== null) {
      return;
    }
    this.stopError = error;
    this.uploadStopped = true;
    this.closeDownload();
    this.closeSockets();
    // before the session's closed event, when that is what stopped it
    this.emitDone();
    this.stopped.reject(error);
  }

  private moved(): boolean {
    return 0 < this.downloadBytes || this.uploadFirst !== undefined;
  }

  private timeout(ms: number, action: () => void): ReturnType<typeof setTimeout> {
    const timer = setTimeout(() => {
      this.timers.delete(timer);
      action();
    }, ms);
    this.timers.add(timer);
    return timer;
  }

  // a wait that cleanup() cancels
  private delay(ms: number): Promise<void> {
    return new Promise((resolve) => {
      this.timeout(ms, resolve);
    });
  }

  private clearTimer(timer: ReturnType<typeof setTimeout> | undefined): void {
    if (timer !== undefined) {
      clearTimeout(timer);
      this.timers.delete(timer);
    }
  }

  private sendDone(): void {
    if (this.doneSent) {
      return;
    }
    this.doneSent = true;
    this.session.send({ type: "load", phase: "done" });
    this.emitDone();
  }

  private emitPhase(phase: LagLoadDirection, at: number): void {
    this.started = true;
    this.session.emit({ type: "load", phase, at });
  }

  // "done" follows every load that started, once
  private emitDone(): void {
    if (this.started && !this.doneEmitted) {
      this.doneEmitted = true;
      this.session.emit({ type: "load", phase: "done", at: epochNow() });
    }
  }

  // M5: maxStreams fetches at once, chunks counted by rule S as amendment 15
  // extends it (M6) and dropped, `received` for each interval from the first
  // byte once it has settled (M4). The window ends downloadMs after the
  // first byte, but the streams are read on until it is final, since a chunk
  // that arrives after the end still counts what it carried before it: until
  // every stream still open is done, or LAG_SPREAD_MAX_MS after the end. The
  // download ends then, or sooner when every stream has ended (a byte cap):
  // the final `received` reports go out, streams still delivering are cut,
  // and the upload starts at once. Waiting out trailing bytes beyond that
  // would push every later load in the track back (S5). The streams are read
  // in a Worker where one is available (downloadSource.ts), so a chunk's
  // time is when it arrived, not when a busy page got to it.
  private async runDownload(): Promise<void> {
    this.session.send({ type: "load", phase: "download" });
    const firstByteGuard = this.timeout(FIRST_BYTE_TIMEOUT_MS, () => {
      if (this.downloadFirst === undefined) {
        this.closeDownload();
      }
    });
    const source = this.session.downloadSources();
    this.downloadSource = source;
    source.start(
      {
        url: `${this.session.httpOrigin}/lag/v1/down?session=${encodeURIComponent(this.hello.session)}`,
        streams: this.hello.maxStreams,
        downloadMs: this.hello.downloadMs,
        openRetries: OPEN_RETRIES,
      },
      {
        chunk: (stream, at, bytes) => this.onDownloadChunk(stream, at, bytes),
        delivered: (through) => {
          this.downloadDelivered = Math.max(this.downloadDelivered, through);
          this.catchUpDownload();
        },
        ended: (stream) => {
          // an ended stream holds no interval back, and a first group it
          // still holds counts now (M6)
          this.downloadEnded.add(stream);
          this.download.endStream(stream);
          if (this.hello.maxStreams <= this.downloadEnded.size) {
            this.downloadStreamsEnded.resolve();
          }
          this.checkDownloadFinal();
          this.catchUpDownload();
        },
      },
    );
    if (this.downloadClosed) {
      // the load stopped before the streams began
      source.close();
    }
    const outcome = await this.until(Promise.race([this.downloadStreamsEnded.promise, this.downloadFinal.promise]));
    this.clearTimer(firstByteGuard);
    if (outcome !== EARLY) {
      // what arrived before now, still on its way from the Worker
      await this.until(source.flush());
    }
    this.closeDownload();
    if (this.downloadFirst !== undefined) {
      // the final reports: once the download is closed, its window is final
      // and every interval has settled, through the window's last, or
      // through the last that a download that ended early counts
      this.reportDownload(this.download.count);
    }
    if (outcome === EARLY) {
      return;
    }
    if (this.downloadBytes === 0) {
      this.sendDone();
      throw new LagSessionError("download", "no download stream delivered data");
    }
  }

  // A chunk of stream `stream`, timed `at` on the monotonic clock where it
  // arrived. It counts by rule S as amendment 15 extends it (M6,
  // LagIntervalSeries.addRead): spread over the time since its stream's
  // previous chunk, or over its group's span when it came back to back with
  // the group's first; a stream's first group at the rate of its second, and
  // only from the window's first byte on; and only the part before the
  // window's end, though it arrives after it.
  private onDownloadChunk(stream: number, at: number, bytes: number): void {
    if (this.downloadClosed) {
      return;
    }
    if (this.downloadFirst === undefined) {
      this.downloadFirst = at;
      this.downloadStartedAt = epochNow() - Math.max(0, monotonicNow() - at);
      const sinceFirst = monotonicNow() - at;
      this.timeout(Math.max(0, this.hello.downloadMs - sinceFirst), () => {
        this.downloadWindowEnded = true;
      });
      // no chunk that arrives later can reach back before the window's end,
      // so its window is final; the chunks that arrived by then are flushed
      // from the Worker before the download closes
      this.timeout(Math.max(0, this.hello.downloadMs + LAG_SPREAD_MAX_MS - sinceFirst), () => this.downloadFinal.resolve());
      const roundTrips = this.session.pings.roundTrips();
      const rttMs = 0 < roundTrips.length ? Math.min(...roundTrips) : UPLOAD_DEFAULT_RTT_MS;
      this.timeout(Math.max(0, this.hello.downloadMs - UPLOAD_EARLY_MS - 2 * rttMs - sinceFirst), () => this.prepareUpload());
      this.scheduleDownloadTick(1);
      this.emitPhase("download", this.downloadStartedAt);
    }
    const t = Math.max(0, at - this.downloadFirst);
    if (!this.downloadStreamsWithData.has(stream)) {
      this.downloadStreamsWithData.add(stream);
      this.downloadStreams += 1;
    }
    this.download.addRead(stream, t, bytes);
    this.downloadBytes += bytes;
    this.checkDownloadFinal();
    // a chunk past the next interval's end can settle it before the next tick
    if ((this.downloadReported + 1) * this.hello.intervalMs <= t) {
      this.catchUpDownload();
    }
  }

  // The download's window is final (M6, amendment 15) once every stream that
  // has not ended is done, so nothing it reads later can count, or once a
  // chunk came too late to count: more than LAG_SPREAD_MAX_MS after the end.
  private checkDownloadFinal(): void {
    if (!this.download.finished) {
      for (let k = 0; k < this.hello.maxStreams; k += 1) {
        if (!this.downloadEnded.has(k) && !this.download.streamDone(k)) {
          return;
        }
      }
    }
    this.downloadFinal.resolve();
  }

  // End the download: its window is final, so nothing more counts and a
  // first group still held counts now (M6); every stream still delivering
  // is cut (its reader cancelled, its fetch aborted); and a window that ran
  // to its end keeps its last intervals even if they stayed empty.
  private closeDownload(): void {
    if (this.downloadClosed) {
      return;
    }
    this.downloadClosed = true;
    clearTimeout(this.downloadTick);
    this.downloadSource?.close();
    // a cut stream may never say it ended (the Worker drops a closed download)
    this.downloadStreamsEnded.resolve();
    if (this.downloadFirst !== undefined && this.hello.downloadMs <= monotonicNow() - this.downloadFirst) {
      // its timer may not have run yet, behind a batch of chunks from past it
      this.downloadWindowEnded = true;
    }
    if (this.download !== undefined) {
      this.download.finish();
      if (this.downloadWindowEnded) {
        this.download.extendTo(this.hello.downloadMs);
      }
    }
  }

  // Ticks fall on whole intervals from the first byte, without drift. A
  // tick's own events can end the download (a handler may close the
  // session), so a closed download schedules no more.
  private scheduleDownloadTick(k: number): void {
    if (this.downloadClosed) {
      return;
    }
    const { intervalMs } = this.hello;
    const due = this.downloadFirst! + k * intervalMs;
    this.downloadTick = setTimeout(() => {
      if (this.downloadClosed) {
        return;
      }
      this.catchUpDownload();
      const completed = Math.floor((monotonicNow() - this.downloadFirst!) / intervalMs);
      this.scheduleDownloadTick(completed + 1);
    }, Math.max(0, due - monotonicNow()));
  }

  // Report each interval that has settled (M4).
  private catchUpDownload(): void {
    if (this.downloadClosed || this.downloadFirst === undefined) {
      return;
    }
    this.reportDownload(this.settledDownload());
  }

  // How many intervals have settled, so that no more bytes can reach them
  // (M4): each has ended by the clock, every chunk timed before now has
  // been handed over (the Worker hands them over a batch behind), and no
  // stream can still count bytes in it (M6, amendment 15). A stream's later
  // chunks reach back to its previous chunk, or to its group's span while a
  // chunk may still join the group, and never more than LAG_SPREAD_MAX_MS;
  // a first group it holds reaches back LAG_SPREAD_MAX_MS from its last
  // chunk; and a stream that is done, so that nothing it reads later counts,
  // reaches nothing (LagIntervalSeries.reach). One that has not opened yet
  // holds intervals back like any other, since its first group reaches back
  // too; one that has ended holds nothing back. Until the window is final,
  // chunks after its end can still count in its last intervals.
  private settledDownload(): number {
    const { intervalMs, maxStreams } = this.hello;
    const seen = Math.min(monotonicNow(), this.downloadDelivered) - this.downloadFirst!;
    let settled = Math.floor(seen / intervalMs);
    for (let k = 0; k < maxStreams; k += 1) {
      if (!this.downloadEnded.has(k)) {
        settled = Math.min(settled, Math.floor(this.download.reach(k, seen) / intervalMs));
      }
    }
    if (maxStreams <= this.downloadEnded.size) {
      // every stream has ended, so the series is whole: an interval it does
      // not count is not reported, though the clock passes it
      settled = Math.min(settled, this.download.count);
    }
    return Math.max(0, settled);
  }

  // One `received` report and one progress event per settled interval, up
  // to `completed`, from the browser's own series (M4): interval i is
  // reported at ms (i+1) x intervalMs with the whole bytes through it, the
  // floor of the series' running sum, so a late tick catches up interval by
  // interval instead of reading a running total at the wrong time, and what
  // the extender signs is what the browser shows. Nothing past the window
  // is reported.
  private reportDownload(completed: number): void {
    const { intervalMs, downloadMs } = this.hello;
    // the progress events' time: the clock since the first byte, which an
    // interval waiting to settle does not hold back, and once the window
    // has closed, its whole length
    const ms = this.downloadWindowEnded
      ? downloadMs
      : Math.min(downloadMs, monotonicNow() - this.downloadFirst!);
    const upTo = Math.min(completed, this.download.maxIntervals);
    while (this.downloadReported < upTo) {
      const interval = this.downloadReported;
      this.downloadReported += 1;
      const through = this.download.wholeBytesThrough(interval);
      const intervalBytes = through - this.downloadReportedBytes;
      this.downloadReportedBytes = through;
      this.session.send({ type: "received", bytes: through, ms: (interval + 1) * intervalMs });
      this.emitProgress("download", interval, intervalBytes, through, ms, this.downloadStartedAt!);
    }
  }

  // An interval's progress event, once its value is final (S5), at `ms`
  // into the direction.
  private emitProgress(
    direction: LagLoadDirection,
    interval: number,
    intervalBytes: number,
    through: number,
    ms: number,
    startedAt: number,
  ): void {
    const { intervalMs, downloadMs, uploadMs } = this.hello;
    this.session.emit({
      type: "progress",
      direction,
      interval,
      bytesPerSecond: (intervalBytes * 1000) / intervalMs,
      mbps: bytesToMbps(intervalBytes, intervalMs),
      ms,
      durationMs: direction === "download" ? downloadMs : uploadMs,
      bytes: through,
      startedAt,
    });
  }

  // One progress event per completed interval of the live upload series
  // (S5): interval i is complete once the series' latest point is at or
  // past its end, or the window is over, and nothing reaches it after. A
  // report that adds nothing completes none. The events' time is the
  // extender's, its latest report's ms.
  private reportUpload(over: boolean): void {
    const { intervalMs } = this.hello;
    const series = this.upload;
    const { ms } = this.lastUploaded;
    const completed = over ? series.count : Math.min(series.maxIntervals, Math.floor(series.durationMs / intervalMs));
    while (this.uploadReported < completed) {
      const interval = this.uploadReported;
      this.uploadReported += 1;
      const through = series.wholeBytesThrough(interval);
      const intervalBytes = through - this.uploadReportedBytes;
      this.uploadReportedBytes = through;
      this.emitProgress("upload", interval, intervalBytes, through, ms, this.uploadStartedAt ?? epochNow() - ms);
    }
  }

  // M6: maxStreams WebSockets of keystream until the extender's report says
  // its window is over, held back by what it has counted (S5). A browser
  // opens WebSockets to one host one at a time, so streams started as their
  // sockets opened would join the window late: every socket is opened first,
  // from shortly before the download's window ends (UPLOAD_EARLY_MS), and the
  // streams start together once each has opened or failed and has its first
  // keystream, waiting at most UPLOAD_OPEN_WAIT_MS after the phase (S5). A
  // stream that waited for its keystream would join the extender's window,
  // which opens at the first message, late.
  private async runUpload(): Promise<void> {
    let key: CryptoKey;
    try {
      key = await this.uploadKey!;
    } catch (error) {
      this.sendDone();
      throw new LagSessionError("protocol", "the extender's upload key is not 16 bytes", { cause: error });
    }
    this.session.send({ type: "load", phase: "upload" });
    // a download that ended before its window did has not prepared them yet
    const { opened, opening } = this.openUploadSockets();
    const keystreams = this.keystreamsFor(key);
    const outcome = await this.until(
      Promise.race([
        Promise.all([...opening, ...keystreams.map((keystream) => keystream.primed())]),
        this.delay(UPLOAD_OPEN_WAIT_MS),
      ]),
    );
    // the common start: a socket still connecting has missed it
    this.uploadStarted = true;
    for (const socket of this.sockets) {
      if (socket.readyState === WEBSOCKET_CONNECTING) {
        socket.close(1000);
      }
    }
    if (outcome === EARLY) {
      return;
    }
    const ready: Array<{ socket: WebSocket; keystream: LagKeystream }> = [];
    opened.forEach((socket, k) => {
      if (socket !== null && socket.readyState === WEBSOCKET_OPEN) {
        ready.push({ socket, keystream: keystreams[k] });
      }
    });
    if (ready.length === 0) {
      this.closeSockets();
      this.sendDone();
      throw new LagSessionError("upload", "no upload stream opened");
    }
    this.uploadStreams = ready.length;
    const roundTrips = this.session.pings.roundTrips();
    if (0 < roundTrips.length) {
      this.baseRttMs = Math.min(...roundTrips);
    }
    this.uploadFirst = monotonicNow();
    this.uploadStartedAt = epochNow();
    // The extender's window opens when the first message arrives, a little
    // after it went, so the streams send until the extender's report says
    // its window is over, rather than for uploadMs by this page's clock
    // (M4, amendment 15); and failing that, until UPLOAD_SETTLE_MS after its
    // estimated end
    this.armUploadSettle();
    if (this.uploadWindowOver) {
      this.closeUpload();
    }
    this.emitPhase("upload", this.uploadStartedAt);
    const pumped = await this.until(this.pumpUploads(ready));
    if (pumped === EARLY) {
      return;
    }
    // streams the extender closed first leave that report still to wait for
    await this.until(this.uploadClosed.promise);
  }

  // The extender's upload window's end on this page's monotonic clock (M4,
  // amendment 15): uploadMs after its first byte, which a report's arrival
  // less its ms places no earlier than half a round trip after it. The
  // report that came soonest after its time places it best, and the
  // session's lowest round trip stands in for the way here. Before the
  // first report, uploadMs after the streams started.
  private uploadWindowEnd(): number {
    const { uploadMs } = this.hello;
    return Number.isFinite(this.reportsOrigin)
      ? this.reportsOrigin - this.baseRttMs / 2 + uploadMs
      : this.uploadFirst! + uploadMs;
  }

  // The streams close UPLOAD_SETTLE_MS after the window's estimated end
  // unless a report says it is over first; a report that places the end
  // sooner moves the deadline.
  private armUploadSettle(): void {
    if (this.uploadFirst === undefined || this.uploadStopped) {
      return;
    }
    this.clearTimer(this.uploadSettleTimer);
    this.uploadSettleTimer = this.timeout(
      Math.max(0, this.uploadWindowEnd() + UPLOAD_SETTLE_MS - monotonicNow()),
      () => this.closeUpload(),
    );
  }

  // The extender's window is over, or should long since have been: the
  // streams close at once, and what is left in their backlog is not the
  // extender's to count.
  private closeUpload(): void {
    this.clearTimer(this.uploadSettleTimer);
    this.uploadStopped = true;
    this.closeSockets();
    this.uploadClosed.resolve();
  }

  // The streams' messages, from the common start until they close (on the
  // extender's report that its window is over, closeUpload): a
  // message whenever the streams' unverified bytes leave room for it, dealt
  // to the streams in turn, so the backlog spreads across them rather than
  // filling one stream's buffers while the others idle. A stream whose
  // buffer in the page already holds its share of the room waits its next
  // turn; one that closes or whose keystream fails drops out.
  private async pumpUploads(streams: Array<{ socket: WebSocket; keystream: LagKeystream }>): Promise<void> {
    let live = streams.slice();
    let turn = 0;
    let sliceStart = monotonicNow();
    while (!this.uploadStopped) {
      live = live.filter(({ socket }) => socket.readyState === WEBSOCKET_OPEN);
      if (live.length === 0) {
        return;
      }
      const allowed = this.unverifiedAllowance(monotonicNow());
      const share = Math.max(LAG_UPLOAD_MESSAGE_BYTES, allowed / live.length);
      let next: { socket: WebSocket; keystream: LagKeystream } | undefined;
      if (this.uploadSent - this.lastUploaded.bytes + LAG_UPLOAD_MESSAGE_BYTES <= allowed) {
        for (let i = 0; i < live.length && next === undefined; i += 1) {
          const stream = live[(turn + i) % live.length];
          if (stream.socket.bufferedAmount + LAG_UPLOAD_MESSAGE_BYTES <= share) {
            next = stream;
            turn = (turn + i + 1) % live.length;
          }
        }
      }
      if (next === undefined) {
        await sleep(BUFFER_POLL_MS);
        sliceStart = monotonicNow();
        continue;
      }
      if (SEND_SLICE_MS < monotonicNow() - sliceStart) {
        await sleep(0);
        sliceStart = monotonicNow();
        continue;
      }
      let message: Uint8Array;
      try {
        message = await next.keystream.next(LAG_UPLOAD_MESSAGE_BYTES);
      } catch {
        live = live.filter((stream) => stream !== next);
        continue;
      }
      if (this.uploadStopped || next.socket.readyState !== WEBSOCKET_OPEN) {
        continue;
      }
      try {
        next.socket.send(message);
      } catch {
        live = live.filter((stream) => stream !== next);
        continue;
      }
      this.uploadSent += message.length;
    }
  }

  // The bytes the streams may have sent that the extender has not verified
  // yet (UPLOAD_BACKLOG_MS): enough to keep the bottleneck busy at its recent
  // rate, and no more than it can still count by the window's end at its
  // steady rate. The end is the extender's, as this page's clock places it
  // (uploadWindowEnd, amendment 15).
  private unverifiedAllowance(now: number): number {
    if (this.lastReportAt === undefined) {
      return UPLOAD_START_BYTES;
    }
    const sinceReportMs = now - this.lastReportAt;
    const timeLeftMs = Math.max(0, this.uploadWindowEnd() - now);
    const recent = this.recentRate();
    const steady = this.steadyRate();
    const backlog = Math.min(UPLOAD_BACKLOG_MAX_BYTES, Math.max(UPLOAD_BACKLOG_MIN_BYTES, (recent * UPLOAD_BACKLOG_MS) / 1000));
    const busy = (recent * (this.baseRttMs + sinceReportMs)) / 1000 + backlog;
    const room = (steady * (this.baseRttMs + sinceReportMs + timeLeftMs)) / 1000;
    const allowed = Math.min(UPLOAD_MAX_UNVERIFIED_BYTES, busy, room);
    // reports that show no rate yet must not leave nothing in flight
    return steady === 0 ? Math.max(UPLOAD_MIN_UNVERIFIED_BYTES, allowed) : allowed;
  }

  // The higher of the rates over the extender's last two report intervals,
  // bytes per second.
  private recentRate(): number {
    let rate = 0;
    for (let i = Math.max(0, this.reports.length - 2); i < this.reports.length; i += 1) {
      const before = i === 0 ? { ms: 0, bytes: 0 } : this.reports[i - 1];
      const report = this.reports[i];
      rate = Math.max(rate, ((report.bytes - before.bytes) * 1000) / (report.ms - before.ms));
    }
    return rate;
  }

  // The extender's rate over its last UPLOAD_STEADY_MS of reports, or since
  // the window began, bytes per second.
  private steadyRate(): number {
    const last = this.reports[this.reports.length - 1];
    let since = { ms: 0, bytes: 0 };
    for (const report of this.reports) {
      if (last.ms - report.ms < UPLOAD_STEADY_MS) {
        break;
      }
      since = report;
    }
    return ((last.bytes - since.bytes) * 1000) / (last.ms - since.ms);
  }

  // UPLOAD_EARLY_MS and two round trips before the download's window ends:
  // the upload's sockets start to open, and its keystreams to generate once
  // the key is in.
  private prepareUpload(): void {
    if (this.uploadStopped || this.ended) {
      return;
    }
    this.openUploadSockets();
    this.uploadKey!.then(
      (key) => {
        if (!this.uploadStopped && !this.ended) {
          this.keystreamsFor(key);
        }
      },
      // runUpload reports a bad key
      () => undefined,
    );
  }

  // The streams' keystreams, made once; each starts generating at once.
  private keystreamsFor(key: CryptoKey): LagKeystream[] {
    if (this.keystreams === null) {
      this.keystreams = [];
      for (let k = 0; k < this.hello.maxStreams; k += 1) {
        this.keystreams.push(new LagKeystream(key, k));
      }
    }
    return this.keystreams;
  }

  // Starts opening the upload's sockets, once: before the download's window
  // ends (prepareUpload), or when the upload starts if that is sooner. A
  // socket that arrives before the upload message waits for it at the
  // extender (M4).
  private openUploadSockets(): { opened: Array<WebSocket | null>; opening: Promise<void>[] } {
    if (this.uploadSockets === null) {
      const opened: Array<WebSocket | null> = [];
      const opening: Promise<void>[] = [];
      for (let k = 0; k < this.hello.maxStreams; k += 1) {
        opened.push(null);
        opening.push(
          this.openUploadSocket(k).then((socket) => {
            opened[k] = socket;
          }),
        );
      }
      this.uploadSockets = { opened, opening };
    }
    return this.uploadSockets;
  }

  private async openUploadSocket(k: number): Promise<WebSocket | null> {
    const url =
      `${this.session.wsOrigin}/lag/v1/up` +
      `?session=${encodeURIComponent(this.hello.session)}&stream=${k}`;
    for (let attempt = 0; ; attempt += 1) {
      if (this.uploadStopped || this.uploadStarted) {
        return null;
      }
      const socket = await this.connect(url);
      if (socket !== null) {
        return socket;
      }
      // the socket can arrive before the extender has the load message
      if (this.uploadStopped || this.uploadStarted || OPEN_RETRIES <= attempt) {
        return null;
      }
      await sleep(100 * 2 ** attempt);
    }
  }

  private connect(url: string): Promise<WebSocket | null> {
    return new Promise((resolve) => {
      let socket: WebSocket;
      try {
        socket = new this.session.WebSocketImpl(url);
      } catch {
        resolve(null);
        return;
      }
      socket.binaryType = "arraybuffer";
      this.sockets.add(socket);
      const onOpen = () => {
        socket.removeEventListener("close", onClose);
        if (this.uploadStopped || this.uploadStarted) {
          socket.close(1000);
          resolve(null);
          return;
        }
        resolve(socket);
      };
      const onClose = () => {
        socket.removeEventListener("open", onOpen);
        this.sockets.delete(socket);
        resolve(null);
      };
      socket.addEventListener("open", onOpen, { once: true });
      socket.addEventListener("close", onClose, { once: true });
      socket.addEventListener("error", () => undefined);
    });
  }

  private closeSockets(): void {
    for (const socket of this.sockets) {
      try {
        socket.close(1000);
      } catch {
        // already closed
      }
    }
    this.sockets.clear();
  }

  private cleanup(): void {
    for (const timer of this.timers) {
      clearTimeout(timer);
    }
    this.timers.clear();
    this.uploadStopped = true;
    this.closeDownload();
    this.closeSockets();
  }

  private measured(): LagLoadResult {
    const intervalMs = this.hello?.intervalMs ?? 250;
    const direction = (
      series: LagIntervalSeries | undefined,
      streams: number,
      startedAt: number | undefined,
    ): LagDirectionResult => ({
      intervalBytes: series?.bytes ?? [],
      mbps: series?.mbps ?? [],
      percentiles: series?.percentiles(),
      bytes: series?.totalBytes ?? 0,
      streams,
      startedAt,
      durationMs: series?.durationMs ?? 0,
    });
    return {
      intervalMs,
      download: direction(this.download, this.downloadStreams, this.downloadStartedAt),
      upload: this.extenderUpload() ?? direction(this.upload, this.uploadStreams, this.uploadStartedAt),
      uploadVerified: this.result?.uploadVerified ?? false,
      endReason: this.result?.endReason ?? null,
    };
  }

  // The upload as the extender's report signs it (M4 `loadResult`
  // `uploadBytes`), cut as the API cuts it (A5): to at most its first
  // ceil(window / intervalMs) intervals, the window being the last
  // `uploaded` report's ms. The extender sends every interval from the
  // upload's first byte through its end, quiet ones as zeros (M8); the API
  // counts only the ones it sent, so a shorter series is never padded. Its
  // p50 and p95 are then the ones the API stores. Undefined when the extender sent none, or sent anything but a
  // series of finite counts at least 0 that the window's intervals hold,
  // and the browser's own series stands.
  private extenderUpload(): LagDirectionResult | undefined {
    const values = this.result?.uploadBytes;
    if (
      this.upload === undefined ||
      !Array.isArray(values) ||
      this.upload.maxIntervals < values.length ||
      !values.every((value) => finiteAtLeastZero(value) !== undefined)
    ) {
      return undefined;
    }
    const { intervalMs } = this.hello;
    const windowMs = this.lastUploaded.ms;
    const intervalBytes = (values as number[]).slice(0, Math.ceil(windowMs / intervalMs));
    const mbps = intervalBytes.map((bytes) => bytesToMbps(bytes, intervalMs));
    return {
      intervalBytes,
      mbps,
      percentiles: percentiles(mbps),
      bytes: this.upload.totalBytes,
      streams: this.uploadStreams,
      startedAt: this.uploadStartedAt,
      durationMs: windowMs,
    };
  }
}
