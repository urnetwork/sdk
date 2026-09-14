export type SocketNetwork = "tcp" | "tcp4" | "tcp6" | "udp" | "udp4" | "udp6";
export interface SocketTLSOptions { serverName?: string; rootCAPEM?: string; nextProtos?: string[] }
export interface DialOptions { signal?: AbortSignal; timeoutMillis?: number }
export interface SocketDevice {
  dial(network: SocketNetwork, address: string, options?: DialOptions): Promise<Conn>;
  dialTls(network: SocketNetwork, address: string, tls?: SocketTLSOptions, options?: DialOptions): Promise<Conn>;
  webTransport(url: string, options?: WebTransportOptions): WebTransport;
}
/** Internal WASM bridge. Public devices are decorated by SDK factories. */
export interface SocketBridge { socketOperation(operation: string, handle: number, argument: unknown): Promise<any> }
interface Handle { id: number; localAddr?: string; remoteAddr?: string; protocol?: string }
function bytes(value: Uint8Array): Uint8Array {
  if (!(value instanceof Uint8Array)) throw new TypeError("Expected Uint8Array");
  return value.slice();
}
function deadline(value: number | Date | null): number {
  const millis = value === null ? 0 : Number(value);
  if (!Number.isSafeInteger(millis)) throw new RangeError("Expected epoch milliseconds or null");
  return millis;
}
/** User space connection. Each UDP read consumes one datagram; null is EOF. */
export class Conn {
  readonly readable: ReadableStream<Uint8Array>;
  readonly writable: WritableStream<Uint8Array>;
  private released = false;
  private eof = false;
  private readError?: Error;
  private writes: Promise<unknown> = Promise.resolve();
  private bridge: SocketBridge;
  private handle: Handle;
  readonly network: SocketNetwork;
  constructor(bridge: SocketBridge, handle: Handle, network: SocketNetwork) {
    this.bridge = bridge; this.handle = handle; this.network = network;
    this.readable = new ReadableStream({
      pull: async controller => { const data = await this.read(); if (data === null) controller.close(); else controller.enqueue(data); },
      cancel: async () => { if (network.startsWith("udp")) await this.close(); else await this.closeRead(); },
    }, { highWaterMark: 0 });
    this.writable = new WritableStream({
      write: async data => { await this.write(data); },
      close: async () => { if (network.startsWith("udp")) await this.close(); else await this.closeWrite(); },
      abort: async () => { await this.close(); },
    });
  }
  private call(op: string, arg: unknown = null): Promise<any> {
    if (this.released) return Promise.reject(new Error("Socket is closed"));
    return this.bridge.socketOperation(op, this.handle.id, arg);
  }
  get localAddr(): string { return this.handle.localAddr ?? ""; }
  get remoteAddr(): string { return this.handle.remoteAddr ?? ""; }
  async read(maxBytes = 65535): Promise<Uint8Array | null> {
    if (!Number.isInteger(maxBytes) || maxBytes < 1 || maxBytes > 65535) throw new RangeError("read size must be between 1 and 65535");
    if (this.eof) return null;
    if (this.readError) { const error = this.readError; this.readError = undefined; throw error; }
    const result = await this.call("read", maxBytes);
    this.eof = result.eof;
    if (result.error) this.readError = new Error(result.error);
    if (this.network === "udp") Object.assign(this.handle, await this.call("addresses"));
    return result.eof && result.data.length === 0 ? null : result.data;
  }
  async write(value: Uint8Array): Promise<number> {
    const data = bytes(value);
    if (this.network.startsWith("udp") && data.length > 65507) throw new RangeError("UDP datagram exceeds 65507 bytes");
    const operation = this.writes.then(() => this.writeBytes(data));
    this.writes = operation.catch(() => {});
    return operation;
  }
  private async writeBytes(data: Uint8Array): Promise<number> {
    let written = 0;
    do {
      const chunk = data.subarray(written, written + 65535);
      let result;
      try { result = await this.call("write", chunk); }
      catch (error) { throw Object.assign(error instanceof Error ? error : new Error(String(error)), { bytesWritten: written }); }
      const n = typeof result === "number" ? result : result.bytesWritten;
      written += n;
      if (result.error) throw Object.assign(new Error(result.error), { bytesWritten: written });
      if (n !== chunk.length) throw Object.assign(new Error("Short socket write"), { bytesWritten: written });
    } while (written < data.length);
    return written;
  }
  setDeadline(t: number | Date | null): Promise<void> { return this.call("deadline", deadline(t)); }
  setReadDeadline(t: number | Date | null): Promise<void> { return this.call("readDeadline", deadline(t)); }
  setWriteDeadline(t: number | Date | null): Promise<void> { return this.call("writeDeadline", deadline(t)); }
  closeRead(): Promise<void> { return this.call("closeRead"); }
  closeWrite(): Promise<void> { return this.call("closeWrite"); }
  async close(): Promise<void> {
    if (this.released) return;
    this.released = true;
    await this.bridge.socketOperation("release", this.handle.id, null);
  }
}
export interface WebTransportOptions extends DialOptions {
  tls?: SocketTLSOptions;
  serverCertificateHashes?: { algorithm: "sha-256"; value: ArrayBuffer | ArrayBufferView }[];
  protocols?: string[]; allowPooling?: boolean; requireUnreliable?: boolean; congestionControl?: "default";
}
export interface WebTransportCloseInfo { closeCode?: number; reason?: string }
export interface WebTransportBidirectionalStream { readable: ReadableStream<Uint8Array>; writable: WritableStream<Uint8Array> }
const buffer = (v: ArrayBuffer | ArrayBufferView): Uint8Array => ArrayBuffer.isView(v) ? new Uint8Array(v.buffer, v.byteOffset, v.byteLength) : new Uint8Array(v);
/** HTTP/3 WebTransport over a Device socket, with QUIC and TLS in WASM. */
export class WebTransport {
  readonly ready: Promise<void>;
  readonly closed: Promise<Required<WebTransportCloseInfo>>;
  readonly incomingBidirectionalStreams: ReadableStream<WebTransportBidirectionalStream>;
  readonly incomingUnidirectionalStreams: ReadableStream<ReadableStream<Uint8Array>>;
  readonly datagrams: { readable: ReadableStream<Uint8Array>; writable: WritableStream<Uint8Array>; createWritable(): WritableStream<Uint8Array>; readonly maxDatagramSize: number };
  readonly reliability = "supports-unreliable";
  readonly congestionControl = "default";
  private selectedProtocol = "";
  get protocol(): string { return this.selectedProtocol; }
  private bridge: SocketBridge;
  private id = 0;
  private stopping = false;
  private normalClose = false;
  private abort = new AbortController();
  private resolveClosed!: (value: Required<WebTransportCloseInfo>) => void;
  private rejectClosed!: (error: unknown) => void;
  constructor(device: SocketDevice | SocketBridge, url: string, options: WebTransportOptions = {}) {
    this.bridge = device as SocketBridge;
    validateDialOptions(options);
    const parsed = new URL(url);
    if (parsed.protocol !== "https:" || parsed.username || parsed.password || url.includes("#")) throw new TypeError("Expected HTTPS URL without credentials or fragment");
    if (options.allowPooling) throw new DOMException("Connection pooling is unsupported", "NotSupportedError");
    if (options.congestionControl && options.congestionControl !== "default") throw new DOMException("Unsupported congestion control", "NotSupportedError");
    const hashes = (options.serverCertificateHashes ?? []).map(hash => {
      if (hash.algorithm !== "sha-256" || buffer(hash.value).length !== 32) throw new TypeError("Expected SHA-256 certificate hash");
      return btoa(String.fromCharCode(...buffer(hash.value)));
    });
    const protocols = options.protocols ?? [];
    if (new Set(protocols).size !== protocols.length || protocols.some(p => !p || !/^[\x21-\x7e]+$/.test(p))) throw new TypeError("Invalid or duplicate WebTransport protocol");
    this.closed = new Promise((resolve, reject) => { this.resolveClosed = resolve; this.rejectClosed = reject; });
    void this.closed.catch(() => {});
    const abort = () => this.abort.abort(options.signal?.reason);
    options.signal?.addEventListener("abort", abort, { once: true });
    if (options.signal?.aborted) abort();
    this.ready = this.bridge.socketOperation("webTransport", 0, {
      address: parsed.href, tls: options.tls, hashes, protocols, signal: this.abort.signal, timeoutMillis: options.timeoutMillis,
    }).then(async (handle: Handle) => {
      this.id = handle.id; this.selectedProtocol = handle.protocol ?? "";
      if (this.stopping) {
        await this.bridge.socketOperation("release", this.id, null);
        throw new DOMException("Closed while connecting", "AbortError");
      }
      void this.bridge.socketOperation("sessionClosed", this.id, null).then(
        info => { this.stopping = true; this.normalClose = true; this.resolveClosed(info); },
        error => { this.stopping = true; this.rejectClosed(error); },
      ).finally(() => this.bridge.socketOperation("release", this.id, null)).catch(() => {});
    }).catch(error => { this.stopping = true; this.rejectClosed(error); throw error; })
      .finally(() => options.signal?.removeEventListener("abort", abort));
    void this.ready.catch(() => {});
    this.incomingBidirectionalStreams = this.incoming("acceptBi", h => this.stream(h, true, true) as WebTransportBidirectionalStream);
    this.incomingUnidirectionalStreams = this.incoming("acceptUni", h => this.stream(h, true, false).readable!);
    const createWritable = () => new WritableStream<Uint8Array>({
      write: async value => { const data = bytes(value); if (data.length <= 1024) await this.call("sendDatagram", data); },
    });
    this.datagrams = {
      maxDatagramSize: 1024,
      readable: this.incoming("receiveDatagram", value => value as Uint8Array),
      writable: createWritable(), createWritable,
    };
  }
  private incoming<T>(operation: string, convert: (value: any) => T): ReadableStream<T> {
    let ended = false;
    return new ReadableStream<T>({
      start: controller => {
        void this.closed.then(() => { if (!ended) { ended = true; controller.close(); } }, error => {
          if (!ended) { ended = true; controller.error(error); }
        });
      },
      pull: async controller => {
        try {
          const value = await this.call(operation);
          if (ended) {
            if (operation !== "receiveDatagram" && value?.id) await this.bridge.socketOperation("release", value.id, null);
          } else if (value?.closed) { ended = true; controller.close(); }
          else controller.enqueue(convert(value));
        } catch (error) { if (!ended && !this.normalClose) { ended = true; controller.error(error); } }
      },
      cancel: () => { ended = true; },
    }, { highWaterMark: 0 });
  }
  private async call(op: string, arg: unknown = null): Promise<any> {
    await this.ready;
    if (this.stopping) throw new DOMException("WebTransport is closed", "InvalidStateError");
    return this.bridge.socketOperation(op, this.id, arg);
  }
  private stream(handle: Handle, read: boolean, write: boolean): Partial<WebTransportBidirectionalStream> {
    let readDone = !read, writeDone = !write;
    let pendingReadError: Error | undefined;
    const call = (op: string, arg: unknown = null) => this.bridge.socketOperation(op, handle.id, arg);
    const release = async () => { if (readDone && writeDone) await call("release", "finished"); };
    const result: Partial<WebTransportBidirectionalStream> = {};
    if (read) result.readable = new ReadableStream({
      pull: async controller => {
        try {
          if (pendingReadError) throw pendingReadError;
          const r = await call("read", 65535);
          if (r.data.length) controller.enqueue(r.data);
          if (r.error) pendingReadError = new Error(r.error);
          if (r.eof) { readDone = true; controller.close(); await release(); }
        } catch (err) { readDone = true; await release(); throw err; }
      },
      cancel: async reason => { readDone = true; await call("cancelRead", streamErrorCode(reason)); await release(); },
    }, { highWaterMark: 0 });
    if (write) result.writable = new WritableStream({
      write: async value => {
        const data = bytes(value);
        for (let pos = 0; pos < data.length;) {
          const chunk = data.subarray(pos, pos + 65535), result = await call("write", chunk);
          const n = typeof result === "number" ? result : result.bytesWritten;
          if (result.error) throw Object.assign(new Error(result.error), { bytesWritten: pos + n });
          if (n !== chunk.length) throw new Error("Short WebTransport stream write");
          pos += n;
        }
      },
      close: async () => { await call("closeWrite"); writeDone = true; await release(); },
      abort: async reason => { await call("cancelWrite", streamErrorCode(reason)); writeDone = true; await release(); },
    });
    return result;
  }
  async createBidirectionalStream(): Promise<WebTransportBidirectionalStream> { return this.stream(await this.call("openBi"), true, true) as WebTransportBidirectionalStream; }
  async createUnidirectionalStream(): Promise<WritableStream<Uint8Array>> { return this.stream(await this.call("openUni"), false, true).writable!; }
  close(info: WebTransportCloseInfo = {}): void {
    const closeCode = info.closeCode ?? 0, reason = info.reason ?? "";
    if (!Number.isInteger(closeCode) || closeCode < 0 || closeCode > 0xffffffff) throw new RangeError("Invalid close code");
    if (new TextEncoder().encode(reason).length > 1024) throw new RangeError("Close reason exceeds 1024 bytes");
    if (this.stopping) return;
    this.stopping = true;
    if (!this.id) { this.abort.abort(); return; }
    void this.bridge.socketOperation("sessionClose", this.id, { closeCode, reason }).catch(this.rejectClosed);
  }
}
function streamErrorCode(reason: unknown): number {
  const code = (reason as { streamErrorCode?: number } | null)?.streamErrorCode ?? 0;
  if (!Number.isInteger(code) || code < 0 || code > 0xffffffff) throw new RangeError("Invalid stream error code");
  return code;
}
/** Called by all SDK device factories, including proxy setup callbacks. */
export function attachSocketAPI<T extends object>(device: T): T & SocketDevice {
  const bridge = device as unknown as SocketBridge;
  const dial = async (op: string, network: SocketNetwork, address: string, tls: SocketTLSOptions | undefined, options: DialOptions = {}): Promise<Conn> => {
    if (!/^(tcp|udp)[46]?$/.test(network)) throw new TypeError("Unsupported socket network");
    validateDialOptions(options);
    options.signal?.throwIfAborted();
    const handle = await bridge.socketOperation(op, 0, { network, address, tls, ...options });
    if (options.signal?.aborted) { await bridge.socketOperation("release", handle.id, null); options.signal.throwIfAborted(); }
    return new Conn(bridge, handle, network);
  };
  return Object.assign(device, {
    dial: (network: SocketNetwork, address: string, options?: DialOptions) => dial("dial", network, address, undefined, options),
    dialTls: (network: SocketNetwork, address: string, tls?: SocketTLSOptions, options?: DialOptions) => dial("dialTls", network, address, tls, options),
    webTransport: (url: string, options?: WebTransportOptions) => new WebTransport(bridge, url, options),
  });
}
function validateDialOptions(options: DialOptions): void {
  if (options.timeoutMillis !== undefined && (!Number.isSafeInteger(options.timeoutMillis) || options.timeoutMillis < 0 || options.timeoutMillis > 2147483647)) throw new RangeError("timeoutMillis must be between 0 and 2147483647");
}
