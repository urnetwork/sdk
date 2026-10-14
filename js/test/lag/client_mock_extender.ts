// A local mock of an extender's measurement api (lag.dev/PLAN.md M3-M6),
// for the session client's tests: an http server with a small RFC 6455
// WebSocket server of its own, so the tests need no dependency.
//
// The session model is the amended one: pings from hello until the browser
// closes, one in flight, the next at the later of pingIntervalMs after the
// last send and the last pong; a ping without a pong in lostAfterMs is lost;
// noPongCloseMs without any pong ends the session. A session runs at most
// one load, and a load can be refused as busy or budget while the session
// goes on pinging.

import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import { createCipheriv, createHash, randomBytes, type Cipher } from "node:crypto";
import type { Duplex } from "node:stream";
import type { AddressInfo } from "node:net";

const WEBSOCKET_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";

// The server side of one WebSocket connection: just enough of RFC 6455.
export class MockWebSocket {
  onText?: (text: string) => void;
  onBinary?: (data: Buffer) => void;
  onPong?: (data: Buffer) => void;
  onClose?: (code: number, reason: string) => void;
  closed = false;
  private pending: Buffer = Buffer.alloc(0);
  private fragments: Buffer[] = [];
  private fragmentOpcode = 0;
  private closeSent = false;
  private readonly socket: Duplex;

  constructor(socket: Duplex, head: Buffer) {
    this.socket = socket;
    socket.on("data", (chunk: Buffer) => this.onData(chunk));
    socket.on("close", () => this.finish(1006, ""));
    socket.on("error", () => undefined);
    if (head.length > 0) {
      this.onData(head);
    }
  }

  get paused(): boolean {
    return this.socket.isPaused();
  }

  pause(): void {
    this.socket.pause();
  }

  resume(): void {
    this.socket.resume();
  }

  sendText(text: string): void {
    this.sendFrame(0x1, Buffer.from(text, "utf8"));
  }

  ping(payload: Buffer): void {
    this.sendFrame(0x9, payload);
  }

  close(code: number, reason = ""): void {
    if (this.closeSent || this.closed) {
      return;
    }
    const payload = Buffer.alloc(2 + Buffer.byteLength(reason));
    payload.writeUInt16BE(code, 0);
    payload.write(reason, 2);
    this.sendFrame(0x8, payload);
    this.closeSent = true;
    // the client answers with its own close frame; don't wait forever
    setTimeout(() => this.socket.destroy(), 1000).unref();
  }

  terminate(): void {
    this.socket.destroy();
  }

  private sendFrame(opcode: number, payload: Buffer): void {
    if (this.closeSent || this.closed || this.socket.destroyed) {
      return;
    }
    let header: Buffer;
    if (payload.length < 126) {
      header = Buffer.from([0x80 | opcode, payload.length]);
    } else if (payload.length < 65536) {
      header = Buffer.alloc(4);
      header[0] = 0x80 | opcode;
      header[1] = 126;
      header.writeUInt16BE(payload.length, 2);
    } else {
      header = Buffer.alloc(10);
      header[0] = 0x80 | opcode;
      header[1] = 127;
      header.writeBigUInt64BE(BigInt(payload.length), 2);
    }
    this.socket.write(Buffer.concat([header, payload]));
  }

  private onData(chunk: Buffer): void {
    this.pending = this.pending.length === 0 ? chunk : Buffer.concat([this.pending, chunk]);
    for (;;) {
      const buffer = this.pending;
      if (buffer.length < 2) {
        return;
      }
      const fin = (buffer[0] & 0x80) !== 0;
      const opcode = buffer[0] & 0x0f;
      const masked = (buffer[1] & 0x80) !== 0;
      let length = buffer[1] & 0x7f;
      let offset = 2;
      if (length === 126) {
        if (buffer.length < 4) {
          return;
        }
        length = buffer.readUInt16BE(2);
        offset = 4;
      } else if (length === 127) {
        if (buffer.length < 10) {
          return;
        }
        length = Number(buffer.readBigUInt64BE(2));
        offset = 10;
      }
      const maskAt = offset;
      if (masked) {
        offset += 4;
      }
      if (buffer.length < offset + length) {
        return;
      }
      const payload = Buffer.from(buffer.subarray(offset, offset + length));
      if (masked) {
        unmask(payload, buffer.subarray(maskAt, maskAt + 4));
      }
      this.pending = buffer.subarray(offset + length);
      this.onFrame(fin, opcode, payload);
      if (this.closed) {
        return;
      }
    }
  }

  private onFrame(fin: boolean, opcode: number, payload: Buffer): void {
    switch (opcode) {
      case 0x0:
        this.fragments.push(payload);
        if (fin) {
          this.deliver(this.fragmentOpcode, Buffer.concat(this.fragments));
          this.fragments = [];
        }
        return;
      case 0x1:
      case 0x2:
        if (fin) {
          this.deliver(opcode, payload);
        } else {
          this.fragmentOpcode = opcode;
          this.fragments = [payload];
        }
        return;
      case 0x8: {
        const code = payload.length >= 2 ? payload.readUInt16BE(0) : 1005;
        const reason = payload.length > 2 ? payload.subarray(2).toString("utf8") : "";
        if (!this.closeSent) {
          this.sendFrame(0x8, payload.subarray(0, 2));
          this.closeSent = true;
        }
        this.socket.end();
        this.finish(code, reason);
        return;
      }
      case 0x9:
        this.sendFrame(0xa, payload);
        return;
      case 0xa:
        this.onPong?.(payload);
        return;
    }
  }

  private deliver(opcode: number, payload: Buffer): void {
    if (opcode === 0x1) {
      this.onText?.(payload.toString("utf8"));
    } else {
      this.onBinary?.(payload);
    }
  }

  private finish(code: number, reason: string): void {
    if (this.closed) {
      return;
    }
    this.closed = true;
    this.onClose?.(code, reason);
  }
}

const unmask = (payload: Buffer, mask: Buffer): void => {
  const length = payload.length;
  let i = 0;
  if (payload.byteOffset % 4 === 0) {
    const words = new Uint32Array(payload.buffer, payload.byteOffset, length >>> 2);
    const word = mask.readUInt32LE(0);
    for (; i < words.length; i += 1) {
      words[i] ^= word;
    }
    i = words.length * 4;
  }
  for (; i < length; i += 1) {
    payload[i] ^= mask[i & 3];
  }
};

const acceptWebSocket = (req: IncomingMessage, socket: Duplex, head: Buffer): MockWebSocket => {
  const accept = createHash("sha1")
    .update(`${req.headers["sec-websocket-key"]}${WEBSOCKET_GUID}`)
    .digest("base64");
  socket.write(
    "HTTP/1.1 101 Switching Protocols\r\n" +
      "Upgrade: websocket\r\n" +
      "Connection: Upgrade\r\n" +
      `Sec-WebSocket-Accept: ${accept}\r\n\r\n`,
  );
  return new MockWebSocket(socket, head);
};

const refuseUpgrade = (socket: Duplex, status: number): void => {
  // the client may reset a refused upgrade
  socket.on("error", () => undefined);
  socket.end(`HTTP/1.1 ${status} Refused\r\nConnection: close\r\nContent-Length: 0\r\n\r\n`);
};

// M6's counter block: stream, then offset/16, each a big-endian uint64
const counterBlock = (stream: number, offset: number): Buffer => {
  const block = Buffer.alloc(16);
  block.writeBigUInt64BE(BigInt(stream), 0);
  block.writeBigUInt64BE(BigInt(offset / 16), 8);
  return block;
};

const ZEROS = Buffer.alloc(65536);

export interface MockHello {
  pingIntervalMs: number;
  intervalMs: number;
  downloadMs: number;
  uploadMs: number;
  maxStreams: number;
}

export interface MockExtenderOptions {
  hello?: Partial<MockHello>;
  /** at most this many open sessions; another gets `sessions` (default 512) */
  maxSessions?: number;
  /** a ping without a pong this long is lost (M4: 5 s) */
  lostAfterMs?: number;
  /** a session without a pong this long ends with 4408 (M4: 30 s) */
  noPongCloseMs?: number;
  /** pretend no pong arrives, as when the page is gone */
  ignorePongs?: boolean;
  /** send no pings at all, so the session goes silent */
  silent?: boolean;
  /**
   * Block this process this long right after sending the hello, so the
   * browser handles the hello late, as a page busy when it arrives would.
   */
  stallAfterHelloMs?: number;
  /** refusals for the next load requests, in order */
  refuseLoads?: Array<{ code: "busy" | "budget"; retryAfterMs: number }>;
  /** verify uploads under this key instead of the session's, to force a mismatch */
  verifyKey?: Buffer;
  /** pause between 64 KiB download writes, per stream (default 5 ms) */
  downloadPauseMs?: number;
  /** upload read rate per stream, bytes per second (default unlimited) */
  uploadBytesPerSecond?: number;
  /** answer the first download request of each stream with this status, once */
  firstDownloadStatus?: number;
  /**
   * Keep writing each download stream this long past the window, whatever
   * the browser's load phase, as an extender whose bytes are still in
   * flight would deliver them.
   */
  downloadOverrunMs?: number;
  /** never answer the upgrade of this upload stream, as if its packets were lost */
  holdUploadStream?: number;
  /**
   * Answer upload upgrades one at a time, this far apart, as a browser that
   * opens one WebSocket handshake to a host at a time makes them complete.
   */
  uploadUpgradeSpacingMs?: number;
}

export interface MockSessionRecord {
  id?: string;
  ticket: string;
  /** every control message the browser sent */
  messages: Array<Record<string, unknown>>;
  /** the browser's download reports */
  received: Array<{ bytes: number; ms: number }>;
  loadRequests: number;
  refused: number;
  pingsSent: number;
  pongs: number;
  lost: number;
  downloadRequests: number;
  downloadRefused: number;
  downloadStreams: number;
  /** download streams the browser closed while the extender was still writing */
  downloadCutByBrowser: number;
  maxConcurrentDownloads: number;
  downloadBytes: number;
  uploadStreams: number;
  /** upload streams that came while the download ran, and waited for their phase */
  uploadEarly: number;
  uploadMessages: number;
  /** sizes of upload messages that were not 65536 bytes, but the last of a stream */
  oddUploadMessages: number[];
  uploadVerifiedBytes: number;
  /** per upload stream: when its socket opened, and when its first message came */
  uploadOpenedAt: number[];
  uploadFirstMessageAt: number[];
  uploadReports: Array<{ bytes: number; ms: number }>;
  mismatch: boolean;
  /** Date.now() when the session opened and its hello went out */
  openedEpoch?: number;
  /** Date.now() at each ping's send */
  pingSentEpoch: number[];
  loadResult?: { uploadVerified: boolean; endReason: string };
  closeCode?: number;
  closedBy?: "browser" | "extender";
}

interface SessionState {
  record: MockSessionRecord;
  ws: MockWebSocket;
  uploadKey: Buffer;
  opened: number;
  phase: "idle" | "download" | "upload" | "ended";
  loadUsed: boolean;
  downloadFirst?: number;
  downloads: Set<ServerResponse>;
  uploadFirst?: number;
  uploads: Set<MockWebSocket>;
  uploadReporter?: ReturnType<typeof setInterval>;
  timers: Set<ReturnType<typeof setTimeout>>;
  // upload streams that came before the upload message, waiting for it (M4)
  phaseWaiters: Set<() => void>;
  closed: boolean;
}

// M4: a stream that comes before the control message of its phase waits
// this long for it, as the extender's streamWait
const STREAM_WAIT_MS = 2000;

export interface MockExtender {
  /** e.g. http://127.0.0.1:12345, for openLagSession's host */
  origin: string;
  sessions: MockSessionRecord[];
  /** download responses and upload sockets still open, across sessions */
  activeStreams(): number;
  /** control sockets still open */
  openSessions(): number;
  close(): Promise<void>;
}

export async function startMockExtender(options: MockExtenderOptions = {}): Promise<MockExtender> {
  const hello: MockHello = {
    pingIntervalMs: 250,
    intervalMs: 250,
    downloadMs: 1000,
    uploadMs: 1000,
    maxStreams: 4,
    ...options.hello,
  };
  const lostAfterMs = options.lostAfterMs ?? 5000;
  const noPongCloseMs = options.noPongCloseMs ?? 30_000;
  const refusals = [...(options.refuseLoads ?? [])];
  const sessions: MockSessionRecord[] = [];
  const byId = new Map<string, SessionState>();
  const usedTickets = new Set<string>();
  const firstDownloadAnswered = new Set<string>();
  const held = new Set<Duplex>();
  // when the last spaced upload upgrade was answered
  let lastUpgradeAnswer = -Infinity;
  const closedByMock = new WeakSet<ServerResponse>();
  let open = 0;

  const later = (state: SessionState, ms: number, action: () => void) => {
    const timer = setTimeout(() => {
      state.timers.delete(timer);
      action();
    }, ms);
    state.timers.add(timer);
  };

  const wakePhaseWaiters = (state: SessionState) => {
    for (const wake of [...state.phaseWaiters]) {
      wake();
    }
  };

  const sendJson = (state: SessionState, message: object) => {
    if (!state.closed) {
      state.ws.sendText(JSON.stringify(message));
    }
  };

  const endLoad = (state: SessionState, endReason: string) => {
    if (state.phase === "ended") {
      return;
    }
    state.phase = "ended";
    wakePhaseWaiters(state);
    clearInterval(state.uploadReporter);
    for (const response of state.downloads) {
      closedByMock.add(response);
      response.destroy();
    }
    for (const upload of state.uploads) {
      upload.close(1000);
    }
    const result = { uploadVerified: !state.record.mismatch, endReason };
    state.record.loadResult = result;
    sendJson(state, { type: "loadResult", ...result });
  };

  const endSession = (state: SessionState, closedBy: "browser" | "extender", code: number) => {
    if (state.closed) {
      return;
    }
    state.closed = true;
    open -= 1;
    state.record.closeCode = code;
    state.record.closedBy = closedBy;
    wakePhaseWaiters(state);
    for (const timer of state.timers) {
      clearTimeout(timer);
    }
    clearInterval(state.uploadReporter);
    for (const response of state.downloads) {
      closedByMock.add(response);
      response.destroy();
    }
    for (const upload of state.uploads) {
      upload.terminate();
    }
  };

  // M4: one ping in flight; the next at the later of pingIntervalMs after
  // the last send and the pong
  const startPings = (state: SessionState) => {
    let sequence = 0;
    let inFlight: { sequence: number; sentAt: number } | null = null;
    let lastSend = -Infinity;
    let lastPong = performance.now();
    const next = () => {
      const delay = Math.max(0, lastSend + hello.pingIntervalMs - performance.now());
      later(state, delay, send);
    };
    const send = () => {
      if (state.closed) {
        return;
      }
      sequence += 1;
      const ping = { sequence, sentAt: performance.now() };
      inFlight = ping;
      lastSend = ping.sentAt;
      const payload = Buffer.alloc(4);
      payload.writeUInt32BE(sequence);
      state.ws.ping(payload);
      state.record.pingsSent += 1;
      state.record.pingSentEpoch.push(Date.now());
      later(state, lostAfterMs, () => {
        if (inFlight !== ping) {
          return;
        }
        inFlight = null;
        state.record.lost += 1;
        sendJson(state, { type: "rtt", t: Math.round(ping.sentAt - state.opened), lost: true });
        next();
      });
    };
    state.ws.onPong = (payload) => {
      if (options.ignorePongs) {
        return;
      }
      lastPong = performance.now();
      if (inFlight === null || payload.length !== 4 || payload.readUInt32BE(0) !== inFlight.sequence) {
        return;
      }
      const ping = inFlight;
      inFlight = null;
      state.record.pongs += 1;
      const ms = Math.round((performance.now() - ping.sentAt) * 100) / 100;
      sendJson(state, { type: "rtt", t: Math.round(ping.sentAt - state.opened), ms });
      next();
    };
    const watch = () => {
      if (state.closed) {
        return;
      }
      if (noPongCloseMs < performance.now() - lastPong) {
        // M4: the page is gone; 1001 is for shutting down and a withdrawn
        // certificate
        endSession(state, "extender", 4408);
        state.ws.close(4408, "no pong");
        return;
      }
      later(state, Math.min(1000, noPongCloseMs / 4), watch);
    };
    watch();
    send();
  };

  const onControlMessage = (state: SessionState, message: Record<string, unknown>) => {
    state.record.messages.push(message);
    if (message.type === "received") {
      state.record.received.push({ bytes: Number(message.bytes), ms: Number(message.ms) });
      return;
    }
    if (message.type !== "load") {
      return;
    }
    switch (message.phase) {
      case "download": {
        state.record.loadRequests += 1;
        const refusal = state.loadUsed ? { code: "busy", retryAfterMs: 0 } : refusals.shift();
        if (refusal !== undefined) {
          state.record.refused += 1;
          sendJson(state, { type: "error", ...refusal });
          return;
        }
        state.loadUsed = true;
        state.phase = "download";
        return;
      }
      case "upload":
        if (state.phase === "download") {
          state.phase = "upload";
          wakePhaseWaiters(state);
        }
        return;
      case "done":
        if (state.phase === "download" || state.phase === "upload") {
          endLoad(state, "complete");
        }
        return;
    }
  };

  const onSession = (req: IncomingMessage, socket: Duplex, head: Buffer, url: URL) => {
    const ticket = url.searchParams.get("ticket") ?? "";
    const ws = acceptWebSocket(req, socket, head);
    const record: MockSessionRecord = {
      ticket,
      messages: [],
      received: [],
      loadRequests: 0,
      refused: 0,
      pingsSent: 0,
      pongs: 0,
      lost: 0,
      downloadRequests: 0,
      downloadRefused: 0,
      downloadCutByBrowser: 0,
      downloadStreams: 0,
      maxConcurrentDownloads: 0,
      downloadBytes: 0,
      uploadStreams: 0,
      uploadEarly: 0,
      uploadMessages: 0,
      oddUploadMessages: [],
      uploadVerifiedBytes: 0,
      uploadOpenedAt: [],
      uploadFirstMessageAt: [],
      uploadReports: [],
      mismatch: false,
      pingSentEpoch: [],
    };
    sessions.push(record);
    // M3: admission
    const refuse = (code: string, closeCode: number) => {
      ws.sendText(JSON.stringify({ type: "error", code }));
      ws.close(closeCode, code);
      record.closeCode = closeCode;
      record.closedBy = "extender";
    };
    if (ticket === "" || ticket.startsWith("bad")) {
      refuse("ticket", 4403);
      return;
    }
    if (usedTickets.has(ticket)) {
      refuse("reused", 4403);
      return;
    }
    usedTickets.add(ticket);
    if ((options.maxSessions ?? 512) <= open) {
      refuse("sessions", 4429);
      return;
    }
    open += 1;
    const id = randomBytes(16).toString("base64url");
    const uploadKey = randomBytes(16);
    record.id = id;
    const state: SessionState = {
      record,
      ws,
      uploadKey,
      opened: performance.now(),
      phase: "idle",
      loadUsed: false,
      downloads: new Set(),
      uploads: new Set(),
      timers: new Set(),
      phaseWaiters: new Set(),
      closed: false,
    };
    byId.set(id, state);
    ws.onText = (text) => onControlMessage(state, JSON.parse(text));
    ws.onClose = (code) => endSession(state, "browser", code);
    record.openedEpoch = Date.now();
    ws.sendText(
      JSON.stringify({ type: "hello", session: id, uploadKey: uploadKey.toString("base64url"), ...hello }),
    );
    if (options.stallAfterHelloMs !== undefined) {
      const until = performance.now() + options.stallAfterHelloMs;
      while (performance.now() < until) {
        // busy, like a page in a long task
      }
    }
    if (!options.silent) {
      startPings(state);
    }
  };

  // M5: keystream under a fresh key per stream, for downloadMs after the
  // phase's first byte
  const onDownload = (req: IncomingMessage, res: ServerResponse, url: URL) => {
    const state = byId.get(url.searchParams.get("session") ?? "");
    const stream = Number(url.searchParams.get("stream"));
    if (state !== undefined) {
      state.record.downloadRequests += 1;
    }
    const key = `${url.searchParams.get("session")}/${stream}`;
    const forced = options.firstDownloadStatus !== undefined && !firstDownloadAnswered.has(key);
    if (
      state === undefined ||
      state.closed ||
      state.phase !== "download" ||
      !(Number.isInteger(stream) && 0 <= stream && stream < hello.maxStreams) ||
      forced
    ) {
      firstDownloadAnswered.add(key);
      if (state !== undefined) {
        state.record.downloadRefused += 1;
      }
      res.writeHead(forced ? options.firstDownloadStatus! : 409, { "Cache-Control": "no-store" });
      res.end();
      return;
    }
    const record = state.record;
    record.downloadStreams += 1;
    state.downloads.add(res);
    record.maxConcurrentDownloads = Math.max(record.maxConcurrentDownloads, state.downloads.size);
    res.writeHead(200, { "Content-Type": "application/octet-stream", "Cache-Control": "no-store" });
    const cipher: Cipher = createCipheriv("aes-128-ctr", randomBytes(16), Buffer.alloc(16));
    const overrunMs = options.downloadOverrunMs;
    let done = false;
    const finish = () => {
      done = true;
      state.downloads.delete(res);
    };
    const endHere = () => {
      closedByMock.add(res);
      res.end();
      finish();
    };
    res.on("close", () => {
      if (!done && !res.writableEnded && !closedByMock.has(res)) {
        record.downloadCutByBrowser += 1;
      }
      finish();
    });
    req.on("close", () => {
      if (!res.writableEnded) {
        finish();
      }
    });
    const write = () => {
      if (done) {
        return;
      }
      const over = overrunMs === undefined ? state.phase !== "download" : state.phase === "ended" || state.closed;
      if (over) {
        endHere();
        return;
      }
      const now = performance.now();
      state.downloadFirst ??= now;
      if (hello.downloadMs + (overrunMs ?? 0) <= now - state.downloadFirst) {
        endHere();
        return;
      }
      record.downloadBytes += ZEROS.length;
      if (res.write(cipher.update(ZEROS))) {
        setTimeout(write, options.downloadPauseMs ?? 5);
      } else {
        res.once("drain", write);
      }
    };
    write();
  };

  // M6: verify each message against the keystream from its offset; report
  // verified bytes every interval from the first message, for uploadMs
  const onUpload = (req: IncomingMessage, socket: Duplex, head: Buffer, url: URL) => {
    const state = byId.get(url.searchParams.get("session") ?? "");
    const stream = Number(url.searchParams.get("stream"));
    // M4: a stream can overtake the upload message, so one that comes while
    // the download runs waits for it, up to STREAM_WAIT_MS
    if (state !== undefined && !state.closed && state.phase === "download") {
      socket.on("error", () => undefined);
      state.record.uploadEarly += 1;
      let woken = false;
      const wake = () => {
        if (woken) {
          return;
        }
        woken = true;
        clearTimeout(timer);
        state.phaseWaiters.delete(wake);
        if (socket.destroyed) {
          return;
        }
        onUploadInPhase(req, socket, head, state, stream);
      };
      const timer = setTimeout(wake, STREAM_WAIT_MS);
      state.phaseWaiters.add(wake);
      return;
    }
    onUploadInPhase(req, socket, head, state, stream);
  };

  const onUploadInPhase = (
    req: IncomingMessage,
    socket: Duplex,
    head: Buffer,
    state: SessionState | undefined,
    stream: number,
  ) => {
    if (
      state === undefined ||
      state.closed ||
      state.phase !== "upload" ||
      !(Number.isInteger(stream) && 0 <= stream && stream < hello.maxStreams)
    ) {
      refuseUpgrade(socket, 409);
      return;
    }
    if (stream === options.holdUploadStream) {
      socket.on("error", () => undefined);
      held.add(socket);
      return;
    }
    const spacing = options.uploadUpgradeSpacingMs;
    if (spacing !== undefined) {
      socket.on("error", () => undefined);
      const answerAt = Math.max(performance.now(), lastUpgradeAnswer + spacing);
      lastUpgradeAnswer = answerAt;
      setTimeout(() => {
        if (!socket.destroyed && !state.closed && state.phase === "upload") {
          acceptUpload(state, stream, req, socket, head);
        } else {
          socket.destroy();
        }
      }, answerAt - performance.now());
      return;
    }
    acceptUpload(state, stream, req, socket, head);
  };

  const acceptUpload = (state: SessionState, stream: number, req: IncomingMessage, socket: Duplex, head: Buffer) => {
    const record = state.record;
    const ws = acceptWebSocket(req, socket, head);
    record.uploadOpenedAt[stream] = performance.now();
    record.uploadStreams += 1;
    state.uploads.add(ws);
    ws.onClose = () => state.uploads.delete(ws);
    const verifier = createCipheriv("aes-128-ctr", options.verifyKey ?? state.uploadKey, counterBlock(stream, 0));
    let lastSize = 65536;
    ws.onBinary = (data) => {
      if (state.phase !== "upload") {
        return;
      }
      record.uploadMessages += 1;
      if (lastSize !== 65536) {
        record.oddUploadMessages.push(lastSize);
      }
      lastSize = data.length;
      const now = performance.now();
      record.uploadFirstMessageAt[stream] ??= now;
      if (state.uploadFirst === undefined) {
        state.uploadFirst = now;
        state.uploadReporter = setInterval(() => {
          const ms = Math.round(performance.now() - state.uploadFirst!);
          const report = { bytes: record.uploadVerifiedBytes, ms: Math.min(ms, hello.uploadMs) };
          record.uploadReports.push(report);
          sendJson(state, { type: "uploaded", ...report });
          if (hello.uploadMs <= ms) {
            clearInterval(state.uploadReporter);
          }
        }, hello.intervalMs);
      }
      const expected = verifier.update(ZEROS.subarray(0, data.length));
      if (!expected.equals(data)) {
        record.mismatch = true;
        endLoad(state, "upload_mismatch");
        return;
      }
      if (now - state.uploadFirst <= hello.uploadMs) {
        record.uploadVerifiedBytes += data.length;
      }
      const rate = options.uploadBytesPerSecond;
      if (rate !== undefined && !ws.paused) {
        ws.pause();
        setTimeout(() => ws.resume(), (data.length / rate) * 1000);
      }
    };
  };

  const server = createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://mock");
    if (req.method === "GET" && url.pathname === "/lag/v1/down") {
      onDownload(req, res, url);
      return;
    }
    res.writeHead(404);
    res.end();
  });
  server.on("upgrade", (req: IncomingMessage, socket: Duplex, head: Buffer) => {
    const url = new URL(req.url ?? "/", "http://mock");
    if (url.pathname === "/lag/v1/session") {
      onSession(req, socket, head, url);
    } else if (url.pathname === "/lag/v1/up") {
      onUpload(req, socket, head, url);
    } else {
      refuseUpgrade(socket, 404);
    }
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as AddressInfo;

  return {
    origin: `http://127.0.0.1:${port}`,
    sessions,
    activeStreams: () => {
      let active = 0;
      for (const state of byId.values()) {
        active += state.downloads.size + state.uploads.size;
      }
      return active;
    },
    openSessions: () => open,
    close: async () => {
      for (const socket of held) {
        socket.destroy();
      }
      for (const state of byId.values()) {
        endSession(state, "extender", 1001);
        state.ws.terminate();
      }
      server.closeAllConnections();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
}
