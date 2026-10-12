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

import { importLagUploadKey, LagKeystream, LAG_UPLOAD_MESSAGE_BYTES } from "./keystream";
import { bytesToMbps, LagIntervalSeries, LagPingWindow, type LagPercentiles } from "./stats";

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
  /** bytes per interval from the direction's first byte, as M8 aligns them */
  intervalBytes: number[];
  /** Mbps per interval */
  mbps: number[];
  /** p50 and p95 of `mbps` by nearest rank; undefined without an interval */
  percentiles?: LagPercentiles;
  /** bytes moved: read by the browser, or verified by the extender for the upload */
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
  download: LagDirectionResult;
  /** the extender's verified counts (M4 `uploaded`) */
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
  /** Date.now() when the hello arrived: the session's time origin for `rtt.at` */
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
  /** the send time on the browser's clock, as Date.now() (the open event's `at` plus `t`) */
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

/** A load interval completed in one direction. */
export interface LagProgressEvent {
  type: "progress";
  direction: LagLoadDirection;
  /** the interval that completed, counted from the direction's first byte */
  interval: number;
  /** its rate */
  bytesPerSecond: number;
  mbps: number;
  /** ms since the direction's first byte */
  ms: number;
  /** the direction's length (hello's downloadMs or uploadMs) */
  durationMs: number;
  /** bytes so far in this direction */
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
  /** Date.now() when the session opened, the origin of the rtt events' `at` */
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

// a load direction with no first byte by then has failed
const FIRST_BYTE_TIMEOUT_MS = 10_000;
// download streams still open this long past downloadMs from the first byte are cut
const DOWNLOAD_GRACE_MS = 2_000;
// how long the upload waits for the extender's report that closes its window
const UPLOAD_SETTLE_MS = 2_000;
// how long the load waits for the extender's loadResult after done
const RESULT_TIMEOUT_MS = 5_000;
// a stream that fails to open (it can race the load message) is retried
const OPEN_RETRIES = 3;
// S5: backpressure by bufferedAmount, at most 8 MiB per stream. Below that,
// a stream buffers about half a second of what it drains, so a slow uplink
// is not left with seconds of upload queued when the load ends.
const BUFFER_MIN_BYTES = 1 << 20;
const BUFFER_MAX_BYTES = 8 << 20;
const BUFFER_SECONDS = 0.5;
const BUFFER_POLL_MS = 10;
// A socket must count a message in bufferedAmount within the task that sent
// it. Where one does not, bufferedAmount gives no backpressure, so the
// streams are held by what the extender has verified: together they may run
// this far ahead of it, plus a second of its verified rate.
const UNCOUNTED_AHEAD_BYTES = 8 << 20;
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

// The flow of one upload stream: what it has sent, and its buffer cap.
interface UploadFlow {
  sent: number;
  drained: number;
  measuredAt: number;
  rate: number;
  cap: number;
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
  private result: { uploadVerified: boolean; endReason: string } | null = null;
  private readonly resultArrived = deferred<void>();
  private readonly downloadAbort = new AbortController();
  private readonly sockets = new Set<WebSocket>();
  private readonly timers = new Set<ReturnType<typeof setTimeout>>();
  private downloadTick?: ReturnType<typeof setTimeout>;
  private downloadFirst?: number;
  private downloadStartedAt?: number;
  private downloadBytes = 0;
  private downloadStreams = 0;
  private downloadReported = 0;
  private uploadFirst?: number;
  private uploadStartedAt?: number;
  private uploadStopped = false;
  private uploadStreams = 0;
  // bytes handed to the upload sockets, across streams
  private uploadSent = 0;
  // the extender's latest verified rate, bytes per second
  private uploadVerifiedRate = 0;
  // false once a socket failed to count a message in bufferedAmount
  private bufferedAmountCounts = true;
  private lastUploaded = { bytes: 0, ms: 0 };
  private readonly uploadWindowReported = deferred<void>();

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
      this.download = new LagIntervalSeries({ intervalMs: hello.intervalMs, maxIntervals: intervals(hello.downloadMs) });
      this.upload = new LagIntervalSeries({ intervalMs: hello.intervalMs, maxIntervals: intervals(hello.uploadMs) });
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

  onUploaded(message: Record<string, unknown>): void {
    const bytes = finiteAtLeastZero(message.bytes);
    const ms = finiteAtLeastZero(message.ms);
    if (bytes === undefined || ms === undefined || this.upload === undefined || this.stopError !== null) {
      return;
    }
    const { intervalMs, uploadMs } = this.hello;
    const previous = this.lastUploaded;
    this.upload.addCumulative(ms, bytes);
    this.lastUploaded = { bytes: Math.max(previous.bytes, bytes), ms: Math.max(previous.ms, ms) };
    const elapsed = ms - previous.ms;
    const bytesPerSecond = 0 < elapsed ? (Math.max(0, bytes - previous.bytes) * 1000) / elapsed : 0;
    this.uploadVerifiedRate = bytesPerSecond;
    this.session.emit({
      type: "progress",
      direction: "upload",
      interval: Math.max(0, Math.round(ms / intervalMs) - 1),
      bytesPerSecond,
      mbps: (bytesPerSecond * 8) / 1e6,
      ms,
      durationMs: uploadMs,
      bytes: this.lastUploaded.bytes,
      startedAt: this.uploadStartedAt ?? epochNow() - ms,
    });
    if (uploadMs - intervalMs / 2 <= ms) {
      this.uploadWindowReported.resolve();
    }
  }

  onLoadResult(message: Record<string, unknown>): void {
    if (this.result !== null || this.stopError !== null) {
      return;
    }
    this.result = {
      uploadVerified: message.uploadVerified === true,
      endReason: typeof message.endReason === "string" ? message.endReason : "unknown",
    };
    this.resultArrived.resolve();
    if (!this.doneSent) {
      // the extender ended the load first, e.g. on an upload mismatch
      this.ended = true;
      this.uploadStopped = true;
      this.downloadAbort.abort();
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
    this.downloadAbort.abort();
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

  // M5: maxStreams fetches at once, chunks counted and dropped, `received`
  // every interval from the first byte.
  private async runDownload(): Promise<void> {
    const { maxStreams } = this.hello;
    this.session.send({ type: "load", phase: "download" });
    const firstByteGuard = this.timeout(FIRST_BYTE_TIMEOUT_MS, () => {
      if (this.downloadFirst === undefined) {
        this.downloadAbort.abort();
      }
    });
    const streams: Promise<void>[] = [];
    for (let k = 0; k < maxStreams; k += 1) {
      streams.push(this.downloadStream(k));
    }
    const outcome = await this.until(Promise.all(streams));
    this.clearTimer(firstByteGuard);
    this.downloadAbort.abort();
    clearTimeout(this.downloadTick);
    if (this.downloadFirst !== undefined) {
      const ms = monotonicNow() - this.downloadFirst;
      this.session.send({ type: "received", bytes: this.downloadBytes, ms: Math.round(ms) });
      this.reportDownload(ms, true);
    }
    if (outcome === EARLY) {
      return;
    }
    if (this.downloadBytes === 0) {
      this.sendDone();
      throw new LagSessionError("download", "no download stream delivered data");
    }
  }

  private async downloadStream(k: number): Promise<void> {
    const url =
      `${this.session.httpOrigin}/lag/v1/down` +
      `?session=${encodeURIComponent(this.hello.session)}&stream=${k}`;
    const signal = this.downloadAbort.signal;
    for (let attempt = 0; ; attempt += 1) {
      let response: Response | undefined;
      try {
        response = await this.session.fetchImpl(url, { signal, cache: "no-store", credentials: "omit" });
      } catch {
        response = undefined;
      }
      if (response?.ok && response.body) {
        await this.readDownload(response.body);
        return;
      }
      try {
        await response?.body?.cancel();
      } catch {
        // nothing to release
      }
      // a stream can arrive before the extender has the load message: try
      // again while the download can still use it
      const late = this.downloadFirst !== undefined && this.hello.downloadMs <= monotonicNow() - this.downloadFirst;
      if (signal.aborted || OPEN_RETRIES <= attempt || late) {
        return;
      }
      await sleep(100 * 2 ** attempt);
    }
  }

  private async readDownload(body: ReadableStream<Uint8Array>): Promise<void> {
    const reader = body.getReader();
    let counted = false;
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) {
          return;
        }
        if (!counted) {
          counted = true;
          this.downloadStreams += 1;
        }
        this.onDownloadChunk(value.byteLength);
      }
    } catch {
      // aborted, or the stream broke: what arrived counts
    } finally {
      try {
        reader.releaseLock();
      } catch {
        // a pending read on an aborted body
      }
    }
  }

  private onDownloadChunk(bytes: number): void {
    const now = monotonicNow();
    if (this.downloadFirst === undefined) {
      this.downloadFirst = now;
      this.downloadStartedAt = epochNow();
      this.timeout(this.hello.downloadMs + DOWNLOAD_GRACE_MS, () => this.downloadAbort.abort());
      this.scheduleDownloadTick(1);
      this.emitPhase("download", this.downloadStartedAt);
    }
    this.download.addAt(now - this.downloadFirst, bytes);
    this.downloadBytes += bytes;
  }

  // Ticks fall on whole intervals from the first byte, without drift.
  private scheduleDownloadTick(k: number): void {
    const due = this.downloadFirst! + k * this.hello.intervalMs;
    this.downloadTick = setTimeout(() => {
      const ms = monotonicNow() - this.downloadFirst!;
      this.session.send({ type: "received", bytes: this.downloadBytes, ms: Math.round(ms) });
      this.reportDownload(ms, false);
      this.scheduleDownloadTick(Math.floor(ms / this.hello.intervalMs) + 1);
    }, Math.max(0, due - monotonicNow()));
  }

  private reportDownload(ms: number, final: boolean): void {
    const { intervalMs, downloadMs } = this.hello;
    const upTo = final
      ? this.download.count
      : Math.min(this.download.maxIntervals, Math.floor(ms / intervalMs));
    while (this.downloadReported < upTo) {
      const interval = this.downloadReported;
      this.downloadReported += 1;
      const bytesPerSecond = (this.download.intervalBytes(interval) * 1000) / intervalMs;
      this.session.emit({
        type: "progress",
        direction: "download",
        interval,
        bytesPerSecond,
        mbps: bytesToMbps(this.download.intervalBytes(interval), intervalMs),
        ms: Math.round(ms),
        durationMs: downloadMs,
        bytes: this.downloadBytes,
        startedAt: this.downloadStartedAt!,
      });
    }
  }

  // M6: maxStreams WebSockets of keystream for uploadMs from the first
  // message, with backpressure by bufferedAmount.
  private async runUpload(): Promise<void> {
    const { maxStreams } = this.hello;
    let key: CryptoKey;
    try {
      key = await importLagUploadKey(this.hello.uploadKey);
    } catch (error) {
      this.sendDone();
      throw new LagSessionError("protocol", "the extender's upload key is not 16 bytes", { cause: error });
    }
    this.session.send({ type: "load", phase: "upload" });
    const firstSendGuard = this.timeout(FIRST_BYTE_TIMEOUT_MS, () => {
      if (this.uploadFirst === undefined) {
        this.uploadStopped = true;
        this.closeSockets();
      }
    });
    const streams: Promise<void>[] = [];
    for (let k = 0; k < maxStreams; k += 1) {
      streams.push(this.uploadStream(k, key));
    }
    const outcome = await this.until(Promise.all(streams));
    this.clearTimer(firstSendGuard);
    if (outcome === EARLY) {
      return;
    }
    if (this.uploadFirst === undefined) {
      this.closeSockets();
      this.sendDone();
      throw new LagSessionError("upload", "no upload stream opened");
    }
    // the extender's window started when the first message arrived, a
    // little after it was sent: give its last report a moment
    await this.until(Promise.race([this.uploadWindowReported.promise, this.delay(UPLOAD_SETTLE_MS)]));
  }

  private async uploadStream(k: number, key: CryptoKey): Promise<void> {
    const socket = await this.openUploadSocket(k);
    if (socket === null) {
      return;
    }
    this.uploadStreams += 1;
    const keystream = new LagKeystream(key, k);
    const flow: UploadFlow = {
      sent: 0,
      drained: 0,
      measuredAt: monotonicNow(),
      rate: 0,
      cap: BUFFER_MIN_BYTES,
    };
    let sliceStart = monotonicNow();
    while (!this.uploadStopped && socket.readyState === WEBSOCKET_OPEN) {
      this.adjustCap(socket, flow);
      if (flow.cap < socket.bufferedAmount + LAG_UPLOAD_MESSAGE_BYTES || this.aheadOfVerified()) {
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
        message = await keystream.next(LAG_UPLOAD_MESSAGE_BYTES);
      } catch {
        return;
      }
      if (this.uploadStopped || socket.readyState !== WEBSOCKET_OPEN) {
        return;
      }
      try {
        socket.send(message);
      } catch {
        return;
      }
      flow.sent += message.length;
      this.uploadSent += message.length;
      if (socket.bufferedAmount < message.length) {
        this.bufferedAmountCounts = false;
      }
      if (this.uploadFirst === undefined) {
        this.uploadFirst = monotonicNow();
        this.uploadStartedAt = epochNow();
        this.timeout(this.hello.uploadMs, () => {
          this.uploadStopped = true;
          // a stream still connecting has missed the window; don't wait on it
          for (const pending of this.sockets) {
            if (pending.readyState === WEBSOCKET_CONNECTING) {
              pending.close(1000);
            }
          }
        });
        this.emitPhase("upload", this.uploadStartedAt);
      }
    }
  }

  // Backpressure where bufferedAmount gives none (UNCOUNTED_AHEAD_BYTES).
  private aheadOfVerified(): boolean {
    return (
      !this.bufferedAmountCounts &&
      UNCOUNTED_AHEAD_BYTES + this.uploadVerifiedRate < this.uploadSent - this.lastUploaded.bytes
    );
  }

  // The cap follows about BUFFER_SECONDS of what the stream drains, within
  // [BUFFER_MIN_BYTES, BUFFER_MAX_BYTES].
  private adjustCap(socket: WebSocket, flow: UploadFlow): void {
    const now = monotonicNow();
    const elapsed = now - flow.measuredAt;
    if (elapsed < 100) {
      return;
    }
    const drained = flow.sent - socket.bufferedAmount;
    const rate = ((drained - flow.drained) * 1000) / elapsed;
    flow.rate = flow.rate === 0 ? rate : (flow.rate + rate) / 2;
    flow.drained = drained;
    flow.measuredAt = now;
    flow.cap = Math.min(BUFFER_MAX_BYTES, Math.max(BUFFER_MIN_BYTES, flow.rate * BUFFER_SECONDS));
  }

  private async openUploadSocket(k: number): Promise<WebSocket | null> {
    const url =
      `${this.session.wsOrigin}/lag/v1/up` +
      `?session=${encodeURIComponent(this.hello.session)}&stream=${k}`;
    for (let attempt = 0; ; attempt += 1) {
      if (this.uploadStopped) {
        return null;
      }
      const socket = await this.connect(url);
      if (socket !== null) {
        return socket;
      }
      // the socket can arrive before the extender has the load message
      if (this.uploadStopped || OPEN_RETRIES <= attempt) {
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
        if (this.uploadStopped) {
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
    clearTimeout(this.downloadTick);
    this.uploadStopped = true;
    this.downloadAbort.abort();
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
      upload: direction(this.upload, this.uploadStreams, this.uploadStartedAt),
      uploadVerified: this.result?.uploadVerified ?? false,
      endReason: this.result?.endReason ?? null,
    };
  }
}
