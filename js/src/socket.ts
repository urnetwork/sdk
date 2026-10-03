export type SocketNetwork = "tcp" | "tcp4" | "tcp6" | "udp" | "udp4" | "udp6";
export interface SocketTLSOptions { serverName?: string; rootCAPEM?: string; nextProtos?: string[] }
export interface DialOptions { signal?: AbortSignal; timeoutMillis?: number }
export interface SocketDevice {
  dial(network: SocketNetwork, address: string, options?: DialOptions): Promise<Conn>;
  dialTls(network: SocketNetwork, address: string, tls?: SocketTLSOptions, options?: DialOptions): Promise<Conn>;
  readonly directSockets: DirectSockets;
}
/** Internal WASM bridge. Public devices are decorated by SDK factories. */
export interface SocketBridge { socketOperation(operation: string, handle: number, argument: unknown): Promise<any> }
interface Handle { id: number; localAddr?: string; remoteAddr?: string }
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
  /** Internal lifecycle notification used by the Direct Sockets adapter. */
  waitClosed(): Promise<void> { return this.call("socketClosed"); }
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
  const result = Object.assign(device, {
    dial: (network: SocketNetwork, address: string, options?: DialOptions) => dial("dial", network, address, undefined, options),
    dialTls: (network: SocketNetwork, address: string, tls?: SocketTLSOptions, options?: DialOptions) => dial("dialTls", network, address, tls, options),
  });
  return Object.assign(result, { directSockets: createDirectSockets(result) });
}
function validateDialOptions(options: DialOptions): void {
  if (options.timeoutMillis !== undefined && (!Number.isSafeInteger(options.timeoutMillis) || options.timeoutMillis < 0 || options.timeoutMillis > 2147483647)) throw new RangeError("timeoutMillis must be between 0 and 2147483647");
}


export type SocketDnsQueryType = "ipv4" | "ipv6";
export interface TCPSocketOptions {
  dnsQueryType?: SocketDnsQueryType;
  noDelay?: boolean;
  keepAliveDelay?: number;
  sendBufferSize?: number;
  receiveBufferSize?: number;
}
export interface UDPSocketOptions {
  remoteAddress?: string;
  remotePort?: number;
  localAddress?: string;
  localPort?: number;
  dnsQueryType?: SocketDnsQueryType;
  sendBufferSize?: number;
  receiveBufferSize?: number;
  ipv6Only?: boolean;
  multicastTimeToLive?: number;
  multicastLoopback?: boolean;
  multicastAllowAddressSharing?: boolean;
}
export interface UDPMessage {
  data: BufferSource;
  remoteAddress?: string;
  remotePort?: number;
  dnsQueryType?: SocketDnsQueryType;
}
export interface TCPSocketOpenInfo {
  readable: ReadableStream<Uint8Array>;
  writable: WritableStream<BufferSource>;
  remoteAddress: string;
  remotePort: number;
  localAddress: string;
  localPort: number;
}
export interface UDPSocketOpenInfo {
  readable: ReadableStream<UDPMessage & { data: Uint8Array }>;
  writable: WritableStream<UDPMessage>;
  remoteAddress?: string;
  remotePort?: number;
  localAddress: string;
  localPort: number;
}
export interface TCPSocket {
  readonly opened: Promise<TCPSocketOpenInfo>;
  readonly closed: Promise<void>;
  close(): Promise<void>;
}
export interface UDPSocket {
  readonly opened: Promise<UDPSocketOpenInfo>;
  readonly closed: Promise<void>;
  close(): Promise<void>;
}
export interface DirectSockets {
  readonly TCPSocket: new (remoteAddress: string, remotePort: number, options?: TCPSocketOptions) => TCPSocket;
  readonly UDPSocket: new (options?: UDPSocketOptions) => UDPSocket;
}

function invalidState(message: string): DOMException { return new DOMException(message, "InvalidStateError"); }
function unsupported(message: string): never { throw new DOMException(message, "NotSupportedError"); }
function networkError(cause: unknown): DOMException {
  return new DOMException(cause instanceof Error ? cause.message : String(cause), "NetworkError");
}
function uint(value: number, max: number, field: string): number {
  const n = Math.trunc(Number(value));
  if (!Number.isFinite(n) || n < 0 || n > max) throw new TypeError(field + " is out of range");
  return n;
}
function destination(host: string, port: number): string {
  if (typeof host !== "string" || !host || /[\s/\[\]?#@]/.test(host)) throw new TypeError("Expected a hostname or unbracketed IP address");
  const n = uint(port, 65535, "remotePort");
  if (!n) throw new TypeError("remotePort must be nonzero");
  if (/^(?:22[4-9]|23\d)\./.test(host) || /^ff[\da-f]{0,2}:/i.test(host)) unsupported("Multicast sockets are not supported");
  return (host.includes(":") ? "[" + host + "]" : host) + ":" + n;
}
function network(protocol: "tcp" | "udp", options: TCPSocketOptions | UDPSocketOptions): SocketNetwork {
  if (options.dnsQueryType !== undefined && options.dnsQueryType !== "ipv4" && options.dnsQueryType !== "ipv6") throw new TypeError("Invalid dnsQueryType");
  for (const field of ["sendBufferSize", "receiveBufferSize"] as const) {
    if (options[field] !== undefined) {
      if (!uint(options[field]!, 0xffffffff, field)) throw new TypeError(field + " must be nonzero");
      unsupported("Per-socket " + field + " is not supported by the Device");
    }
  }
  return (protocol + (options.dnsQueryType === "ipv4" ? "4" : options.dnsQueryType === "ipv6" ? "6" : "")) as SocketNetwork;
}
function copyBuffer(value: BufferSource): Uint8Array {
  if (!(value instanceof ArrayBuffer) && !ArrayBuffer.isView(value)) throw new TypeError("Expected BufferSource");
  const view = ArrayBuffer.isView(value) ? new Uint8Array(value.buffer, value.byteOffset, value.byteLength) : new Uint8Array(value);
  if (!(view.buffer instanceof ArrayBuffer)) throw new TypeError("Shared buffers are not supported");
  return view.slice();
}
function endpoint(address: string): { address: string; port: number } {
  const split = address.lastIndexOf(":");
  let host = address.slice(0, split);
  if (host.startsWith("[") && host.endsWith("]")) host = host.slice(1, -1);
  const port = Number(address.slice(split + 1));
  if (split < 1 || !host || !Number.isInteger(port) || port < 0 || port > 65535) throw new Error("Device returned an invalid socket address");
  return { address: host, port };
}

type OpenInfo = TCPSocketOpenInfo | UDPSocketOpenInfo;
type ReadController = ReadableByteStreamController | ReadableStreamDefaultController<UDPMessage & { data: Uint8Array }>;

/** Lifecycle shared by the connected TCP and UDP profiles. */
class DirectSocket<I extends OpenInfo> {
  readonly opened: Promise<I>;
  readonly closed: Promise<void>;
  private resolveClosed!: () => void;
  private rejectClosed!: (error: unknown) => void;
  private conn?: Conn;
  private info?: I;
  private readController?: ReadController;
  private writeController?: WritableStreamDefaultController;
  private readDone = false;
  private writeDone = false;
  private readStopped = false;
  private writeStopped = false;
  private reading = false;
  private writing = false;
  private readStop?: Promise<void>;
  private writeStop?: Promise<void>;
  private finishing?: Promise<void>;
  private failed = false;
  private failure: unknown;
  private settled = false;
  private udp: boolean;

  constructor(device: Pick<SocketDevice, "dial">, protocol: SocketNetwork, address: string, udp: boolean) {
    this.udp = udp;
    this.closed = new Promise((resolve, reject) => { this.resolveClosed = resolve; this.rejectClosed = reject; });
    void this.closed.catch(() => {});
    this.opened = Promise.resolve().then(() => device.dial(protocol, address)).then(async conn => {
      this.conn = conn;
      try {
        this.info = this.streams();
        // Device shutdown must settle even an idle socket with no pending I/O.
        void conn.waitClosed().then(() => {
          if (!this.finishing) this.fail(networkError("Socket closed by its Device"));
        }, error => { if (!this.finishing) this.fail(networkError(error)); });
        return this.info;
      } catch (error) { await conn.close(); throw error; }
    }).catch(error => {
      const failure = networkError(error);
      this.settled = true;
      this.rejectClosed(failure);
      throw failure;
    });
    void this.opened.catch(() => {});
  }

  private streams(): I {
    const conn = this.conn!;
    const readable = this.udp
      ? new ReadableStream<UDPMessage & { data: Uint8Array }>({
        start: c => { this.readController = c; },
        pull: c => this.pull(c), cancel: () => this.stopRead(),
      }, { highWaterMark: 0 })
      : new ReadableStream({
        type: "bytes", autoAllocateChunkSize: 65535,
        start: c => { this.readController = c; },
        pull: c => this.pull(c), cancel: () => this.stopRead(),
      }, { highWaterMark: 0 });
    const writable = new WritableStream<BufferSource | UDPMessage>({
      start: c => {
        this.writeController = c;
        c.signal.addEventListener("abort", () => { void this.stopWrite().catch(error => this.fail(networkError(error))); }, { once: true });
      },
      write: value => this.write(value),
      close: () => this.stopWrite(),
      abort: () => this.stopWrite(),
    });
    // UDP Happy Eyeballs chooses its definitive addresses after the first
    // reply. Getters keep an already-resolved opened object up to date.
    return {
      readable, writable,
      get remoteAddress() { return endpoint(conn.remoteAddr).address; },
      get remotePort() { return endpoint(conn.remoteAddr).port; },
      get localAddress() { return endpoint(conn.localAddr).address; },
      get localPort() { return endpoint(conn.localAddr).port; },
    } as I;
  }

  private async pull(controller: ReadController): Promise<void> {
    if (this.readDone) return;
    this.reading = true;
    try {
      const request = "byobRequest" in controller ? controller.byobRequest : null;
      const data = await this.conn!.read(Math.min(request?.view?.byteLength ?? 65535, 65535));
      if (this.readDone) return;
      if (data === null) {
        this.readDone = true;
        this.readStopped = true;
        controller.close();
        if (request) request.respond(0);
        await this.finish();
      } else if ("byobRequest" in controller) {
        if (!data.length) return; // A TCP zero-byte read is not a byte-stream chunk.
        if (request?.view) {
          new Uint8Array(request.view.buffer, request.view.byteOffset, request.view.byteLength).set(data);
          request.respond(data.length);
        } else controller.enqueue(data.slice());
      } else controller.enqueue({ data: data.slice() });
    } catch (error) { if (!this.readDone) this.fail(networkError(error)); }
    finally { this.reading = false; }
  }

  private async write(value: BufferSource | UDPMessage): Promise<void> {
    let data: Uint8Array;
    try {
      if (this.udp) {
        const message = value as UDPMessage;
        if (!message || typeof message !== "object") throw new TypeError("Expected UDPMessage");
        if (message.remoteAddress !== undefined || message.remotePort !== undefined || message.dnsQueryType !== undefined) throw new TypeError("Connected UDP messages must not specify a destination");
        data = copyBuffer(message.data);
      } else data = copyBuffer(value as BufferSource);
    } catch (error) {
      this.failed = true; this.failure = error;
      await this.stopWrite();
      throw error;
    }
    this.writing = true;
    try { await this.conn!.write(data); }
    catch (error) {
      if (this.writeController!.signal.aborted) throw this.writeController!.signal.reason;
      const failure = networkError(error);
      if (error && typeof error === "object" && "bytesWritten" in error) Object.assign(failure, { bytesWritten: error.bytesWritten });
      this.fail(failure);
      throw failure;
    } finally { this.writing = false; }
  }

  private stopRead(): Promise<void> {
    if (this.readStop) return this.readStop;
    if (this.readDone) return this.finish();
    this.readDone = true;
    this.readStop = (async () => {
      if (!this.udp) await this.conn!.closeRead();
      else if (this.reading) await this.conn!.setReadDeadline(1);
    })().then(() => { this.readStopped = true; return this.finish(); }, error => this.fail(networkError(error)));
    return this.readStop;
  }
  private stopWrite(): Promise<void> {
    if (this.writeStop) return this.writeStop;
    if (this.writeDone) return this.finish();
    this.writeDone = true;
    this.writeStop = (async () => {
      if (!this.udp) await this.conn!.closeWrite();
      else if (this.writing) await this.conn!.setWriteDeadline(1);
    })().then(() => { this.writeStopped = true; return this.finish(); }, error => this.fail(networkError(error)));
    return this.writeStop;
  }
  private fail(error: unknown): void {
    if (this.finishing) return;
    this.failed = true; this.failure = error;
    this.readDone = true; this.writeDone = true;
    this.readStopped = true; this.writeStopped = true;
    this.readController?.error(error);
    this.writeController?.error(error);
    void this.finish();
  }
  private finish(): Promise<void> {
    if (!this.readDone || !this.writeDone || !this.readStopped || !this.writeStopped) return Promise.resolve();
    if (!this.finishing) {
      this.finishing = Promise.resolve().then(() => this.conn!.close()).then(() => {
        this.settled = true;
        if (this.failed) this.rejectClosed(this.failure); else this.resolveClosed();
      }, error => { this.settled = true; this.rejectClosed(networkError(error)); });
    }
    return this.finishing;
  }
  close(): Promise<void> {
    if (!this.info) return Promise.reject(invalidState("Socket has not opened"));
    if (this.settled) return this.closed;
    if (this.info.readable.locked || this.info.writable.locked) return Promise.reject(invalidState("Release the reader and writer locks before closing the socket"));
    void this.info.readable.cancel().catch(() => {});
    void this.info.writable.abort().catch(() => {});
    return this.closed;
  }
}

/** Bind the standard Direct Sockets constructor signatures to one UR Device. */
export function createDirectSockets(device: Pick<SocketDevice, "dial">): DirectSockets {
  return Object.freeze({
    TCPSocket: class TCPSocket extends DirectSocket<TCPSocketOpenInfo> {
      constructor(remoteAddress: string, remotePort: number, options: TCPSocketOptions = {}) {
        const protocol = network("tcp", options);
        if (options.keepAliveDelay !== undefined) {
          if (uint(options.keepAliveDelay, 0xffffffff, "keepAliveDelay") < 1000) throw new TypeError("keepAliveDelay must be at least 1000 ms");
          unsupported("Per-socket TCP keep-alive is not supported by the Device");
        }
        if (options.noDelay !== undefined) unsupported("Per-socket noDelay is not supported by the Device");
        super(device, protocol, destination(remoteAddress, remotePort), false);
      }
    },
    UDPSocket: class UDPSocket extends DirectSocket<UDPSocketOpenInfo> {
      constructor(options: UDPSocketOptions = {}) {
        const protocol = network("udp", options);
        if ((options.remoteAddress === undefined) !== (options.remotePort === undefined)) throw new TypeError("remoteAddress and remotePort must be specified together");
        if (options.localPort !== undefined && (options.localAddress === undefined || !uint(options.localPort, 65535, "localPort"))) throw new TypeError("localPort requires localAddress and must be nonzero");
        if (options.localAddress !== undefined) {
          if (options.remoteAddress !== undefined) throw new TypeError("Local and remote binding options cannot be combined");
          unsupported("Bound UDP sockets are reserved for the future listener API");
        }
        if (options.remoteAddress === undefined) throw new TypeError("Connected UDP requires remoteAddress and remotePort");
        if (options.ipv6Only !== undefined) throw new TypeError("ipv6Only is only valid for bound UDP");
        if (options.multicastTimeToLive !== undefined || options.multicastLoopback !== undefined || options.multicastAllowAddressSharing !== undefined) unsupported("Multicast sockets are not supported");
        super(device, protocol, destination(options.remoteAddress, options.remotePort!), true);
      }
    },
  });
}
