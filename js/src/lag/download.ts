// The download's streams (lag.dev/PLAN.md M5, S5): maxStreams fetches of the
// extender's bytes, each chunk counted when it arrived and dropped. This code
// runs in a dedicated Worker by default (downloadWorker.ts), so a page busy
// drawing cannot delay a chunk's timestamp and skew the 250 ms intervals it
// lands in. It runs on the page itself where a Worker is not available
// (downloadSource.ts). It touches nothing but fetch, streams and timers, so
// it is safe in either.

/**
 * The clock every context shares: performance.timeOrigin plus
 * performance.now(), on the user agent's shared monotonic clock (HR-Time 3).
 * A Worker's and its page's readings compare directly.
 */
export const lagSharedNow = (): number => performance.timeOrigin + performance.now();

/** What a download's streams need to start. */
export interface LagDownloadStart {
  /** the download's url up to its session, `.../lag/v1/down?session=<id>`; each stream appends `&stream=<k>` */
  url: string;
  /** how many streams at once (hello's maxStreams) */
  streams: number;
  /** the window from the first chunk on any stream; a stream that fails to open is not retried past it */
  downloadMs: number;
  /** how many times a stream that fails to open is tried again */
  openRetries: number;
}

/** Where a download's chunks go. */
export interface LagDownloadSink {
  /** `at` is on the shared clock (lagSharedNow) */
  chunk(stream: number, at: number, bytes: number): void;
  /** the stream is over: its body ended or broke, it was cut, or it never opened */
  ended(stream: number): void;
}

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

/**
 * One download's streams. A stream can arrive before the extender has the
 * load message, so one that fails to open is retried after 100, 200 and
 * 400 ms, while the download can still use it (M4).
 */
export class LagDownloadStreams {
  private readonly start: LagDownloadStart;
  private readonly fetchImpl: typeof fetch;
  private readonly sink: LagDownloadSink;
  private readonly now: () => number;
  private readonly abort = new AbortController();
  private readonly readers = new Set<ReadableStreamDefaultReader<Uint8Array>>();
  private closed = false;
  private first?: number;

  constructor(start: LagDownloadStart, fetchImpl: typeof fetch, sink: LagDownloadSink, now: () => number = lagSharedNow) {
    this.start = start;
    this.fetchImpl = fetchImpl;
    this.sink = sink;
    this.now = now;
  }

  /** Start every stream. */
  run(): void {
    for (let k = 0; k < this.start.streams; k += 1) {
      void this.stream(k).finally(() => this.sink.ended(k));
    }
  }

  /** Cut every stream: readers cancelled, fetches aborted, and no chunk after this. */
  close(): void {
    if (this.closed) {
      return;
    }
    this.closed = true;
    for (const reader of this.readers) {
      reader.cancel().catch(() => undefined);
    }
    this.readers.clear();
    this.abort.abort();
  }

  private async stream(k: number): Promise<void> {
    const url = `${this.start.url}&stream=${k}`;
    const signal = this.abort.signal;
    for (let attempt = 0; ; attempt += 1) {
      let response: Response | undefined;
      try {
        response = await this.fetchImpl(url, { signal, cache: "no-store", credentials: "omit" });
      } catch {
        response = undefined;
      }
      if (response?.ok && response.body) {
        await this.read(k, response.body);
        return;
      }
      try {
        await response?.body?.cancel();
      } catch {
        // nothing to release
      }
      const late = this.first !== undefined && this.start.downloadMs <= this.now() - this.first;
      if (signal.aborted || this.start.openRetries <= attempt || late) {
        return;
      }
      await sleep(100 * 2 ** attempt);
    }
  }

  private async read(k: number, body: ReadableStream<Uint8Array>): Promise<void> {
    const reader = body.getReader();
    if (this.closed) {
      reader.cancel().catch(() => undefined);
      return;
    }
    this.readers.add(reader);
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done || this.closed) {
          return;
        }
        const at = this.now();
        if (this.first === undefined) {
          this.first = at;
        }
        this.sink.chunk(k, at, value.byteLength);
      }
    } catch {
      // aborted, or the stream broke: what arrived counts
    } finally {
      this.readers.delete(reader);
      try {
        reader.releaseLock();
      } catch {
        // a pending read on an aborted body
      }
    }
  }
}

/** A message from the page to the download Worker. */
export type LagDownloadToWorker =
  | { type: "start"; id: number; start: LagDownloadStart }
  /** hand over every chunk that has arrived */
  | { type: "flush"; id: number }
  /** cut the download's streams and hand over the rest */
  | { type: "close"; id: number };

/**
 * A message from the download Worker to the page. Chunks travel as flat
 * triples, [stream, at, bytes, ...], with `at` on the shared clock; `through`
 * says every chunk timed up to then has been sent, in this message or before.
 */
export type LagDownloadToPage =
  | { type: "ready" }
  | { type: "chunks" | "flushed" | "closed"; id: number; items: number[]; through: number }
  | { type: "ended"; id: number; stream: number };

/** How often the Worker hands chunks over while a download runs. */
export const LAG_DOWNLOAD_BATCH_MS = 16;

interface HostedRun {
  streams: LagDownloadStreams;
  items: number[];
  sentFirst: boolean;
}

/**
 * The download Worker's side (downloadWorker.ts wires it to the Worker's
 * messages): it runs each download's streams and hands their chunks to the
 * page in batches, every LAG_DOWNLOAD_BATCH_MS and at once for a download's
 * first chunk, so the page can start the window without waiting.
 */
export class LagDownloadWorkerHost {
  private readonly post: (message: LagDownloadToPage) => void;
  private readonly fetchImpl: typeof fetch;
  private readonly now: () => number;
  private readonly runs = new Map<number, HostedRun>();
  private ticker?: ReturnType<typeof setInterval>;

  constructor(
    post: (message: LagDownloadToPage) => void,
    fetchImpl: typeof fetch = (input, init) => globalThis.fetch(input, init),
    now: () => number = lagSharedNow,
  ) {
    this.post = post;
    this.fetchImpl = fetchImpl;
    this.now = now;
  }

  receive(message: LagDownloadToWorker): void {
    switch (message.type) {
      case "start":
        this.startRun(message.id, message.start);
        break;
      case "flush": {
        const run = this.runs.get(message.id);
        this.send(message.id, run, "flushed");
        break;
      }
      case "close": {
        const run = this.runs.get(message.id);
        run?.streams.close();
        this.runs.delete(message.id);
        this.send(message.id, run, "closed");
        this.idle();
        break;
      }
    }
  }

  private startRun(id: number, start: LagDownloadStart): void {
    // the sink is only called once run() has started, after `run` is set
    let run: HostedRun;
    const streams = new LagDownloadStreams(
      start,
      this.fetchImpl,
      {
        chunk: (stream, at, bytes) => {
          if (this.runs.get(id) !== run) {
            return;
          }
          run.items.push(stream, at, bytes);
          if (!run.sentFirst) {
            run.sentFirst = true;
            this.send(id, run, "chunks");
          }
        },
        ended: (stream) => {
          if (this.runs.get(id) !== run) {
            return;
          }
          // the stream's chunks before its end
          this.send(id, run, "chunks");
          this.post({ type: "ended", id, stream });
        },
      },
      this.now,
    );
    run = { streams, items: [], sentFirst: false };
    this.runs.set(id, run);
    if (this.ticker === undefined) {
      this.ticker = setInterval(() => {
        for (const [runId, hosted] of this.runs) {
          this.send(runId, hosted, "chunks");
        }
      }, LAG_DOWNLOAD_BATCH_MS);
    }
    streams.run();
  }

  private send(id: number, run: HostedRun | undefined, type: "chunks" | "flushed" | "closed"): void {
    const items = run ? run.items : [];
    if (run) {
      run.items = [];
    }
    this.post({ type, id, items, through: this.now() });
  }

  private idle(): void {
    if (this.runs.size === 0 && this.ticker !== undefined) {
      clearInterval(this.ticker);
      this.ticker = undefined;
    }
  }
}
