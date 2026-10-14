// Where a load's download streams are read (lag.dev/PLAN.md S5). By default a
// dedicated Worker reads them (downloadWorker.ts), so a page busy drawing
// cannot delay a chunk's timestamp and push its bytes into a later 250 ms
// interval. The page reads them itself when no Worker is available, when the
// caller brings its own fetch (it cannot cross into a Worker), when asked to,
// or when the Worker fails to start. Either way the load gets the same
// chunks, timed on the page's own monotonic clock, performance.now().

import {
  LagDownloadStreams,
  type LagDownloadStart,
  type LagDownloadToPage,
  type LagDownloadToWorker,
} from "./download";

/** What a source hands the load. */
export interface LagDownloadHandlers {
  /** `at` is on the page's monotonic clock, performance.now() */
  chunk(stream: number, at: number, bytes: number): void;
  /** every chunk timed up to `through` (page clock) has been handed over */
  delivered(through: number): void;
  ended(stream: number): void;
}

/** One load's download streams, wherever they are read. */
export interface LagDownloadSource {
  start(start: LagDownloadStart, handlers: LagDownloadHandlers): void;
  /** resolves once every chunk that had arrived is handed over */
  flush(): Promise<void>;
  /** cut the streams; nothing is handed over after this */
  close(): void;
}

export type LagDownloadSourceFactory = () => LagDownloadSource;

// how long a new Worker gets to say it is ready before the page reads the
// streams itself
const WORKER_READY_MS = 2_000;
// how long a flush waits for the Worker before the load goes on without it
const WORKER_FLUSH_MS = 500;

// the shared clock (download.ts) on the page's monotonic clock
const onPage = (at: number) => at - performance.timeOrigin;

/** The streams read on the page: every chunk is handed over as it is timed. */
class LagInlineDownloadSource implements LagDownloadSource {
  private readonly fetchImpl: typeof fetch;
  private streams?: LagDownloadStreams;

  constructor(fetchImpl: typeof fetch) {
    this.fetchImpl = fetchImpl;
  }

  start(start: LagDownloadStart, handlers: LagDownloadHandlers): void {
    handlers.delivered(Number.POSITIVE_INFINITY);
    this.streams = new LagDownloadStreams(start, this.fetchImpl, {
      chunk: (stream, at, bytes) => handlers.chunk(stream, onPage(at), bytes),
      ended: (stream) => handlers.ended(stream),
    });
    this.streams.run();
  }

  flush(): Promise<void> {
    return Promise.resolve();
  }

  close(): void {
    this.streams?.close();
  }
}

interface HubRun {
  readonly handlers: LagDownloadHandlers;
  readonly streams: number;
  readonly ended: Set<number>;
  flushes: Array<() => void>;
}

/**
 * One Worker shared by every download on the page, made on first use. A
 * Worker that fails to load, errors or never says it is ready is given up:
 * its downloads end with what they had, and later ones are read on the page.
 */
class LagDownloadWorkerHub {
  private readonly factory: () => Worker;
  private worker: Worker | null = null;
  private state: "idle" | "starting" | "ready" | "broken" = "idle";
  private readyPromise: Promise<boolean> | null = null;
  private readonly runs = new Map<number, HubRun>();
  private nextId = 1;

  constructor(factory: () => Worker) {
    this.factory = factory;
  }

  ready(): Promise<boolean> {
    if (this.readyPromise !== null) {
      return this.readyPromise;
    }
    this.readyPromise = new Promise<boolean>((resolve) => {
      let worker: Worker;
      try {
        worker = this.factory();
      } catch {
        this.state = "broken";
        resolve(false);
        return;
      }
      this.worker = worker;
      this.state = "starting";
      const timer = setTimeout(() => {
        if (this.state === "starting") {
          this.breakDown();
          resolve(false);
        }
      }, WORKER_READY_MS);
      worker.addEventListener("message", (event: MessageEvent<LagDownloadToPage>) => {
        const message = event.data;
        if (message.type === "ready") {
          if (this.state === "starting") {
            this.state = "ready";
            clearTimeout(timer);
            resolve(true);
          }
          return;
        }
        this.receive(message);
      });
      const fail = () => {
        clearTimeout(timer);
        this.breakDown();
        resolve(false);
      };
      worker.addEventListener("error", fail);
      worker.addEventListener("messageerror", fail);
    });
    return this.readyPromise;
  }

  /** Start a download in the Worker, or null when the Worker is not ready. */
  open(start: LagDownloadStart, handlers: LagDownloadHandlers): number | null {
    if (this.state !== "ready" || this.worker === null) {
      return null;
    }
    const id = this.nextId;
    this.nextId += 1;
    this.runs.set(id, { handlers, streams: start.streams, ended: new Set(), flushes: [] });
    this.post({ type: "start", id, start });
    return id;
  }

  flush(id: number): Promise<void> {
    const run = this.runs.get(id);
    if (run === undefined) {
      return Promise.resolve();
    }
    return new Promise<void>((resolve) => {
      const timer = setTimeout(done, WORKER_FLUSH_MS);
      function done() {
        clearTimeout(timer);
        resolve();
      }
      run.flushes.push(done);
      this.post({ type: "flush", id });
    });
  }

  close(id: number): void {
    const run = this.runs.get(id);
    if (run === undefined) {
      return;
    }
    this.runs.delete(id);
    for (const done of run.flushes) {
      done();
    }
    this.post({ type: "close", id });
  }

  private post(message: LagDownloadToWorker): void {
    try {
      this.worker?.postMessage(message);
    } catch {
      this.breakDown();
    }
  }

  private receive(message: LagDownloadToPage): void {
    if (message.type === "ready") {
      return;
    }
    const run = this.runs.get(message.id);
    if (run === undefined) {
      return;
    }
    if (message.type === "ended") {
      run.ended.add(message.stream);
      run.handlers.ended(message.stream);
      return;
    }
    const { items } = message;
    for (let i = 0; i + 2 < items.length; i += 3) {
      run.handlers.chunk(items[i], onPage(items[i + 1]), items[i + 2]);
    }
    run.handlers.delivered(onPage(message.through));
    if (message.type === "flushed") {
      const flushes = run.flushes;
      run.flushes = [];
      for (const done of flushes) {
        done();
      }
    }
  }

  // Give the Worker up. Its downloads end with what they had: every chunk
  // already handed over counts, and no interval waits for more.
  private breakDown(): void {
    this.state = "broken";
    try {
      this.worker?.terminate();
    } catch {
      // already gone
    }
    this.worker = null;
    const runs = [...this.runs.values()];
    this.runs.clear();
    for (const run of runs) {
      run.handlers.delivered(Number.POSITIVE_INFINITY);
      for (let k = 0; k < run.streams; k += 1) {
        if (!run.ended.has(k)) {
          run.handlers.ended(k);
        }
      }
      for (const done of run.flushes) {
        done();
      }
    }
  }
}

/** The streams read in the hub's Worker, or on the page if it cannot start. */
class LagWorkerDownloadSource implements LagDownloadSource {
  private readonly hub: LagDownloadWorkerHub;
  private readonly fetchImpl: typeof fetch;
  private id: number | null = null;
  private fallback: LagInlineDownloadSource | null = null;
  private starting: Promise<void> = Promise.resolve();
  private closed = false;

  constructor(hub: LagDownloadWorkerHub, fetchImpl: typeof fetch) {
    this.hub = hub;
    this.fetchImpl = fetchImpl;
  }

  start(start: LagDownloadStart, handlers: LagDownloadHandlers): void {
    this.starting = this.hub.ready().then((ok) => {
      if (this.closed) {
        return;
      }
      const id = ok ? this.hub.open(start, handlers) : null;
      if (id !== null) {
        this.id = id;
        return;
      }
      this.fallback = new LagInlineDownloadSource(this.fetchImpl);
      this.fallback.start(start, handlers);
    });
  }

  async flush(): Promise<void> {
    await this.starting;
    if (this.fallback !== null) {
      return this.fallback.flush();
    }
    if (this.id !== null) {
      return this.hub.flush(this.id);
    }
  }

  close(): void {
    this.closed = true;
    this.fallback?.close();
    if (this.id !== null) {
      this.hub.close(this.id);
    }
  }
}

// The Worker's own module, which a bundler (vite) builds from this
// expression; the sdk's rollup build rewrites the name to the .js it emits.
const defaultDownloadWorker = (): Worker =>
  new Worker(new URL("./downloadWorker.ts", import.meta.url), { type: "module", name: "lagdev-download" });

const hubs = new WeakMap<() => Worker, LagDownloadWorkerHub>();

/**
 * How a session's loads read their downloads: in a Worker, unless the caller
 * brings its own fetch, says false, or the page has no Worker.
 */
export function lagDownloadSources(options: {
  fetch?: typeof fetch;
  worker?: false | (() => Worker);
}): LagDownloadSourceFactory {
  const fetchImpl = options.fetch ?? ((input, init) => globalThis.fetch(input, init));
  const factory =
    options.fetch !== undefined || options.worker === false
      ? undefined
      : (options.worker ?? (typeof Worker === "function" ? defaultDownloadWorker : undefined));
  if (factory === undefined) {
    return () => new LagInlineDownloadSource(fetchImpl);
  }
  let hub = hubs.get(factory);
  if (hub === undefined) {
    hub = new LagDownloadWorkerHub(factory);
    hubs.set(factory, hub);
  }
  const shared = hub;
  return () => new LagWorkerDownloadSource(shared, fetchImpl);
}
