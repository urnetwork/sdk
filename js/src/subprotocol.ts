export interface SubprotocolMessage {
  subprotocolId: number;
  sourceClientId: string;
  /** Owned complete message, copied across the WASM boundary. */
  bytes: Uint8Array;
}

export interface SubprotocolDevice {
  /** Requires a provider-capable native companion; hosted proxies reject it.
   * Register mirrored peer/state listeners before the first RPC sync. Adding
   * them later can reconnect the browser RPC and invalidate subscriptions. */
  enableSubprotocol(id: number, listener: (message: SubprotocolMessage) => void | Promise<void>): Promise<SubprotocolSubscription>;
}

/** Internal WASM bridge. */
export interface SubprotocolBridge {
  subprotocolOperation(operation: string, handle: number, argument: unknown): Promise<any>;
}

/** A registration on one RPC session. Reopen explicitly after disconnection.
 * Up to 16 registrations/session, 65535 bytes/frame, and 64 queued messages or
 * 1 MiB/registration. Overflow rejects `closed`; frames are never coalesced. */
export class SubprotocolSubscription {
  readonly subprotocolId: number;
  /** Resolves after unsubscribe; rejects on transport, queue, or listener failure. */
  readonly closed: Promise<void>;
  private active = true;
  private release?: Promise<void>;
  private bridge: SubprotocolBridge;
  private handle: number;
  constructor(bridge: SubprotocolBridge, handle: number, id: number, listener: (message: SubprotocolMessage) => void | Promise<void>) {
    this.bridge = bridge;
    this.handle = handle;
    this.subprotocolId = id;
    this.closed = this.receive(listener);
    // Applications may inspect .closed later without an unhandled rejection.
    void this.closed.catch(() => {});
  }
  private async receive(listener: (message: SubprotocolMessage) => void | Promise<void>): Promise<void> {
    try {
      while (this.active) {
        const message = await this.bridge.subprotocolOperation("receive", this.handle, null);
        if (!this.active) break;
        if (!(message.bytes instanceof Uint8Array)) throw new TypeError("Invalid subprotocol frame from WASM");
        await listener({subprotocolId: this.subprotocolId, sourceClientId: message.sourceClientId, bytes: message.bytes.slice()});
      }
    } catch (error) {
      if (this.active) throw error;
    } finally {
      await this.close();
    }
  }
  private requireActive(): void {
    if (!this.active) throw new Error("Subprotocol subscription is closed");
  }
  /** true means enqueued. Use an application ACK to establish receipt. */
  async send(destinationClientId: string, bytes: Uint8Array): Promise<boolean> {
    this.requireActive();
    if (!(bytes instanceof Uint8Array)) throw new TypeError("Expected Uint8Array");
    if (bytes.length > 65535) throw new RangeError("Subprotocol frame exceeds 65535 bytes");
    return this.bridge.subprotocolOperation("send", this.handle, {destinationClientId, bytes: bytes.slice()});
  }
  /** null means unanswered/unavailable; [] means the peer advertised no protocols. */
  async querySubprotocols(destinationClientId: string, timeoutMillis = 10000): Promise<number[] | null> {
    this.requireActive();
    if (!Number.isSafeInteger(timeoutMillis) || timeoutMillis < 1 || timeoutMillis > 60000) throw new RangeError("timeoutMillis must be between 1 and 60000");
    return this.bridge.subprotocolOperation("query", this.handle, {destinationClientId, timeoutMillis});
  }
  /** Stops delivery immediately and releases the native registration once. */
  close(): Promise<void> {
    this.active = false;
    this.release ??= this.bridge.subprotocolOperation("release", this.handle, null);
    return this.release;
  }
}

export function attachSubprotocolAPI<T extends object>(device: T): T & SubprotocolDevice {
  const bridge = device as unknown as SubprotocolBridge;
  return Object.assign(device, {
    async enableSubprotocol(id: number, listener: (message: SubprotocolMessage) => void | Promise<void>): Promise<SubprotocolSubscription> {
      if (!Number.isInteger(id) || id < 1024 || id > 65535) throw new RangeError("Application subprotocol id must be between 1024 and 65535");
      if (typeof listener !== "function") throw new TypeError("Expected subprotocol listener");
      if (typeof bridge.subprotocolOperation !== "function") throw new Error("Loaded WASM lacks subprotocol RPC support; rebuild the SDK");
      const handle = await bridge.subprotocolOperation("open", 0, id);
      return new SubprotocolSubscription(bridge, handle.id, id, listener);
    },
  });
}
