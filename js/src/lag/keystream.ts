// The upload keystream (lag.dev/PLAN.md M6). The browser uploads AES-128-CTR
// keystream under the session's uploadKey, and the extender regenerates it to
// check that what arrived is the stream it expects.
//
// Counter block for stream k at byte offset o: bytes 0-7 are k and bytes 8-15
// are o/16, each a big-endian uint64. WebCrypto increments only the low 64
// bits (`length: 64`) and Go's cipher.NewCTR the whole block; they agree
// because the low half never wraps within a stream.

/** The size of every upload message but possibly the last (M6). */
export const LAG_UPLOAD_MESSAGE_BYTES = 65536;
/** Keystream generated per WebCrypto call (S5: 4 x 1 MiB per stream). */
export const LAG_KEYSTREAM_CHUNK_BYTES = 1 << 20;
/** Chunks a stream keeps generated ahead of what it has handed out. */
export const LAG_KEYSTREAM_DEPTH = 4;

const writeU64 = (view: DataView, at: number, value: number) => {
  view.setUint32(at, Math.floor(value / 0x100000000));
  view.setUint32(at + 4, value >>> 0);
};

/** The 16-byte AES-CTR counter block for `stream` at byte `offset` (a multiple of 16). */
export function lagCounterBlock(stream: number, offset: number) {
  if (!(Number.isSafeInteger(stream) && 0 <= stream)) {
    throw new RangeError(`stream must be a non-negative safe integer, got ${stream}`);
  }
  if (!(Number.isSafeInteger(offset) && 0 <= offset && offset % 16 === 0)) {
    throw new RangeError(`offset must be a non-negative multiple of 16, got ${offset}`);
  }
  const block = new Uint8Array(16);
  const view = new DataView(block.buffer);
  writeU64(view, 0, stream);
  writeU64(view, 8, offset / 16);
  return block;
}

const subtleOf = (subtle?: SubtleCrypto): SubtleCrypto => {
  const resolved = subtle ?? globalThis.crypto?.subtle;
  if (!resolved) {
    throw new Error("WebCrypto (crypto.subtle) is not available; it needs a secure context");
  }
  return resolved;
};

/** Decode base64url (with or without padding) to bytes. */
const decodeBase64Url = (text: string) => {
  if (!/^[A-Za-z0-9_-]*={0,2}$/.test(text)) {
    throw new TypeError("not base64url");
  }
  const base64 = text.replace(/=+$/, "").replace(/-/g, "+").replace(/_/g, "/");
  const binary = atob(base64 + "=".repeat((4 - (base64.length % 4)) % 4));
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return bytes;
};

/**
 * Import the session's 16-byte upload key (hello.uploadKey), given as bytes or
 * as its base64url text, as a non-extractable AES-CTR key.
 */
export async function importLagUploadKey(
  key: Uint8Array | string,
  subtle?: SubtleCrypto,
): Promise<CryptoKey> {
  // a copy: WebCrypto wants a plain ArrayBuffer behind the view
  const raw = typeof key === "string" ? decodeBase64Url(key) : new Uint8Array(key);
  if (raw.length !== 16) {
    throw new RangeError(`the upload key must be 16 bytes, got ${raw.length}`);
  }
  return subtleOf(subtle).importKey("raw", raw, { name: "AES-CTR" }, false, ["encrypt"]);
}

// One shared block of zeros, never written: encrypting zeros yields the
// keystream, and WebCrypto copies its input when called. It grows to the
// largest length asked for, up to a bound past which a call gets its own.
const MAX_SHARED_ZEROS = 8 << 20;
let sharedZeros = new Uint8Array(0);
const zeros = (length: number) => {
  if (MAX_SHARED_ZEROS < length) {
    return new Uint8Array(length);
  }
  if (sharedZeros.length < length) {
    sharedZeros = new Uint8Array(length);
  }
  return sharedZeros.subarray(0, length);
};

/** `length` bytes of stream `stream`'s keystream from byte `offset` (a multiple of 16). */
export async function lagKeystream(
  key: CryptoKey,
  stream: number,
  offset: number,
  length: number,
  subtle?: SubtleCrypto,
): Promise<Uint8Array> {
  if (!(Number.isSafeInteger(length) && 0 <= length)) {
    throw new RangeError(`length must be a non-negative integer, got ${length}`);
  }
  const counter = lagCounterBlock(stream, offset);
  if (length === 0) {
    return new Uint8Array(0);
  }
  const out = await subtleOf(subtle).encrypt(
    { name: "AES-CTR", counter, length: 64 },
    key,
    zeros(length),
  );
  return new Uint8Array(out);
}

export interface LagKeystreamOptions {
  /** bytes per generated chunk (default 1 MiB; a multiple of 16) */
  chunkBytes?: number;
  /** chunks held at once, the one being read included (default 4) */
  depth?: number;
  subtle?: SubtleCrypto;
}

/**
 * One upload stream's keystream, read sequentially from offset 0 and
 * generated ahead in a small ring: `depth` chunks of `chunkBytes` are in
 * hand or on their way, generated one at a time, and a chunk is replaced as
 * soon as it has been read, so a sender rarely waits on WebCrypto, no task
 * copies more than a chunk, and memory stays at depth x chunkBytes.
 *
 * The views next() returns are never written again, but the caller should
 * hand each one off (WebSocket.send copies it) rather than keep it.
 */
export class LagKeystream {
  readonly stream: number;
  readonly chunkBytes: number;
  readonly depth: number;
  private readonly key: CryptoKey;
  private readonly subtle: SubtleCrypto;
  private readonly ahead: Promise<Uint8Array>[] = [];
  // settles when the last chunk asked for is generated (generateAhead)
  private generated: Promise<unknown> = Promise.resolve();
  private nextChunk = 0;
  private current: Uint8Array | null = null;
  private position = 0;
  private handedOut = 0;
  private reading: Promise<unknown> = Promise.resolve();
  private failed = false;
  private failure: unknown;

  constructor(key: CryptoKey, stream: number, options: LagKeystreamOptions = {}) {
    const chunkBytes = options.chunkBytes ?? LAG_KEYSTREAM_CHUNK_BYTES;
    if (!(Number.isSafeInteger(chunkBytes) && 16 <= chunkBytes && chunkBytes % 16 === 0)) {
      throw new RangeError(`chunkBytes must be a positive multiple of 16, got ${chunkBytes}`);
    }
    const depth = options.depth ?? LAG_KEYSTREAM_DEPTH;
    if (!(Number.isInteger(depth) && 1 <= depth)) {
      throw new RangeError(`depth must be a positive integer, got ${depth}`);
    }
    lagCounterBlock(stream, 0);
    this.key = key;
    this.stream = stream;
    this.chunkBytes = chunkBytes;
    this.depth = depth;
    this.subtle = subtleOf(options.subtle);
    for (let i = 0; i < depth; i += 1) {
      this.generateAhead();
    }
  }

  /** Bytes handed out so far: the stream offset of the next byte. */
  get offset(): number {
    return this.handedOut;
  }

  /**
   * Resolves once the next byte is generated, so next() would not wait on
   * WebCrypto for it. A chunk that failed resolves it too; next() reports
   * the failure.
   */
  primed(): Promise<void> {
    const chunk = this.current === null && !this.failed ? this.ahead[0] : undefined;
    if (chunk === undefined) {
      return Promise.resolve();
    }
    return chunk.then(
      () => undefined,
      () => undefined,
    );
  }

  /**
   * The next `length` bytes of the keystream (default one upload message).
   * Calls are served in order, even when they overlap.
   */
  next(length: number = LAG_UPLOAD_MESSAGE_BYTES): Promise<Uint8Array> {
    if (!(Number.isSafeInteger(length) && 0 <= length)) {
      return Promise.reject(new RangeError(`length must be a non-negative integer, got ${length}`));
    }
    const read = this.reading.then(() => this.read(length));
    // a failed read fails its own caller; the queue moves on
    this.reading = read.catch(() => undefined);
    return read;
  }

  private async read(length: number): Promise<Uint8Array> {
    if (this.failed) {
      throw this.failure;
    }
    if (length === 0) {
      return new Uint8Array(0);
    }
    if (this.current === null) {
      await this.take();
    }
    const first = this.current!;
    if (this.position + length <= first.length) {
      // the common case: one message inside one chunk, no copy
      const view = first.subarray(this.position, this.position + length);
      this.consumed(length);
      return view;
    }
    // the request straddles chunks
    const out = new Uint8Array(length);
    let filled = 0;
    while (filled < length) {
      if (this.current === null) {
        await this.take();
      }
      const current = this.current!;
      const take = Math.min(length - filled, current.length - this.position);
      out.set(current.subarray(this.position, this.position + take), filled);
      filled += take;
      this.consumed(take);
    }
    return out;
  }

  private async take(): Promise<void> {
    try {
      this.current = await this.ahead.shift()!;
      this.position = 0;
    } catch (error) {
      // the chunk is gone, so every later offset would be wrong
      this.failed = true;
      this.failure = error;
      throw error;
    }
  }

  private consumed(bytes: number): void {
    this.position += bytes;
    this.handedOut += bytes;
    if (this.current !== null && this.position === this.current.length) {
      this.current = null;
      this.position = 0;
      this.generateAhead();
    }
  }

  // Asks for the next chunk. Each starts once the one before it is generated,
  // so a stream has one WebCrypto call in flight: filling the ring is a chunk
  // per task rather than depth chunks' copying in one long task.
  private generateAhead(): void {
    const offset = this.nextChunk * this.chunkBytes;
    this.nextChunk += 1;
    const generate = () => lagKeystream(this.key, this.stream, offset, this.chunkBytes, this.subtle);
    const chunk = this.generated.then(generate, generate);
    // an abandoned stream must not leave an unhandled rejection behind; a
    // reader still sees the failure when it reaches this chunk
    chunk.catch(() => undefined);
    this.generated = chunk;
    this.ahead.push(chunk);
  }
}
