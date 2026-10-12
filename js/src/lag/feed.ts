// The extender feed over WSS, verified, into an in-memory directory
// (lag.dev/PLAN.md S3, M9; connect/EXTENDER.md D4, B5).
//
// An extender that serves the measurement API also serves the feed at
// wss://<measurement host>/ur/feed/v1: the browser's first binary message is
// an ExtenderFeedRequest, and every ExtenderFeedFrame arrives as one binary
// message. The directory takes a record only as connect's directory does
// (net_extender_directory.go ApplyRecord): the root signature over
// "ur-extender-record-v1" || Body verifies under a root key from hello, the
// body names an allowed network host, and it carries a key. The newest record
// of a key wins, a revocation at or after a record's issue time revokes it,
// and a record is expired five minutes after its ExpireTimeMs. Each entry
// exposes the record's MeasurementId (C10), the extender's id when it serves
// the measurement API, and the measurement host that id names (N1).
//
// Signatures are checked with WebCrypto Ed25519 where the runtime has it,
// proven once against an RFC 8032 vector, and otherwise with the vendored
// TweetNaCl (tweetnacl_verify.ts). Both refuse a non-canonical S, as Go's
// crypto/ed25519 does.

import {
  canonicalIp,
  DEFAULT_EXTENDER_MEASUREMENT_ZONE,
  extenderIdFromBytes,
  extenderMeasurementHost,
  trimSpaceGo,
} from "./names";
import {
  decodeExtenderFeedFrame,
  decodeExtenderRecordBody,
  decodeExtenderRevocationBody,
  encodeExtenderFeedRequest,
  type ExtenderFeedFrame,
  type ExtenderRecord,
  type ExtenderRecordBody,
  type ExtenderRevocation,
  type ExtenderRevocationBody,
} from "./protobuf";
import { tweetnaclVerifyDetached } from "./tweetnacl_verify";

/** The signature domain of a record body (connect extender_record.go). */
export const EXTENDER_RECORD_SIGNATURE_DOMAIN = "ur-extender-record-v1";
/** The signature domain of a revocation body (connect extender_record.go). */
export const EXTENDER_REVOCATION_SIGNATURE_DOMAIN = "ur-extender-revocation-v1";
/** The signature domain of a lag ticket body (connect lag_ticket.go, M0). */
export const LAG_TICKET_SIGNATURE_DOMAIN = "ur-lag-ticket-v1";

/** The sample size connect asks for by default, which is also the server's cap (D4). */
export const DEFAULT_EXTENDER_FEED_SAMPLE_COUNT = 8;
/** The largest feed frame (D4). */
export const EXTENDER_FEED_MAX_FRAME_BYTES = 64 * 1024;
/** How long past its ExpireTimeMs a record is still taken, for clock skew (connect RecordExpireSkew). */
export const DEFAULT_EXTENDER_RECORD_EXPIRE_SKEW_MS = 5 * 60 * 1000;

// RFC 8032 section 7.1, test 1: the empty message under the key of seed
// 9d61b1...7f60, which a runtime's Ed25519 must accept before it is trusted
const RFC8032_PUBLIC_KEY = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a";
const RFC8032_SIGNATURE =
  "e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b";

// the order of the Ed25519 group, little endian
const GROUP_ORDER = [
  0xed, 0xd3, 0xf5, 0x5c, 0x1a, 0x63, 0x12, 0x58, 0xd6, 0x9c, 0xf7, 0xa2, 0xde, 0xf9, 0xde, 0x14, 0, 0, 0, 0, 0, 0, 0,
  0, 0, 0, 0, 0, 0, 0, 0, 0x10,
];

function hexBytes(hex: string): Uint8Array<ArrayBuffer> {
  const text = hex.trim();
  if (text.length % 2 !== 0 || !/^[0-9a-fA-F]*$/.test(text)) {
    throw new TypeError("not hex");
  }
  const bytes = new Uint8Array(text.length / 2);
  for (let i = 0; i < bytes.length; i += 1) {
    bytes[i] = parseInt(text.slice(2 * i, 2 * i + 2), 16);
  }
  return bytes;
}

function bytesHex(bytes: Uint8Array): string {
  let hex = "";
  for (let i = 0; i < bytes.length; i += 1) {
    hex += bytes[i].toString(16).padStart(2, "0");
  }
  return hex;
}

// whether the S half of a signature is below the group order, which Go's
// crypto/ed25519 requires and TweetNaCl does not check
function canonicalScalar(signature: Uint8Array): boolean {
  for (let i = 31; 0 <= i; i -= 1) {
    const s = signature[32 + i];
    if (s !== GROUP_ORDER[i]) {
      return s < GROUP_ORDER[i];
    }
  }
  return false;
}

let webCryptoEd25519: Promise<SubtleCrypto | null> | undefined;

// The runtime's WebCrypto when its Ed25519 verifies the RFC 8032 vector and
// refuses it for another message; null when it has no Ed25519 or gets either
// wrong. Probed once.
function ed25519Subtle(): Promise<SubtleCrypto | null> {
  if (webCryptoEd25519 === undefined) {
    webCryptoEd25519 = (async () => {
      const subtle = globalThis.crypto?.subtle;
      if (!subtle) {
        return null;
      }
      try {
        const key = await subtle.importKey("raw", hexBytes(RFC8032_PUBLIC_KEY), { name: "Ed25519" }, false, ["verify"]);
        const signature = hexBytes(RFC8032_SIGNATURE);
        const valid = await subtle.verify({ name: "Ed25519" }, key, signature, new Uint8Array(0));
        const forged = await subtle.verify({ name: "Ed25519" }, key, signature, new Uint8Array(1));
        return valid && !forged ? subtle : null;
      } catch {
        return null;
      }
    })();
  }
  return webCryptoEd25519;
}

/**
 * Verifies an Ed25519 signature with WebCrypto where the runtime has Ed25519,
 * otherwise with the vendored TweetNaCl. A signature or key of the wrong
 * length, or a non-canonical S, does not verify. `fallback: true` skips
 * WebCrypto.
 */
export async function verifyEd25519(
  publicKey: Uint8Array,
  message: Uint8Array,
  signature: Uint8Array,
  options: { fallback?: boolean } = {},
): Promise<boolean> {
  if (publicKey.length !== 32 || signature.length !== 64 || !canonicalScalar(signature)) {
    return false;
  }
  const subtle = options.fallback ? null : await ed25519Subtle();
  if (subtle !== null) {
    try {
      const key = await subtle.importKey("raw", new Uint8Array(publicKey), { name: "Ed25519" }, false, ["verify"]);
      return await subtle.verify({ name: "Ed25519" }, key, new Uint8Array(signature), new Uint8Array(message));
    } catch {
      // a key WebCrypto will not import is no key
      return false;
    }
  }
  return tweetnaclVerifyDetached(message, signature, publicKey);
}

/** Why the directory did not take a record or revocation. */
export type ExtenderRefusal = "signature" | "malformed" | "networkHost" | "publicKey" | "expired";

export type ExtenderApplyResult =
  | { status: "applied"; publicKey: string; entry: ExtenderDirectoryEntry | null }
  | { status: "unchanged"; publicKey: string }
  | { status: "refused"; reason: ExtenderRefusal; message: string };

/**
 * The root keys records are signed by, from hello's
 * `extender_root_public_keys`. As in connect, a hex key that is empty is
 * skipped and one that is not 32 bytes of hex is an error, and a raw key that
 * is not 32 bytes is skipped. A signature is tried under each key; the key id
 * in a record is only a hint in connect and is not needed for the few keys a
 * space has.
 */
export class ExtenderRootKeys {
  readonly publicKeys: ReadonlyArray<Uint8Array>;

  constructor(rootPublicKeys: ReadonlyArray<string | Uint8Array>) {
    const publicKeys: Uint8Array[] = [];
    for (const rootPublicKey of rootPublicKeys) {
      if (typeof rootPublicKey === "string") {
        const text = trimSpaceGo(rootPublicKey);
        if (text === "") {
          continue;
        }
        let key: Uint8Array;
        try {
          key = hexBytes(text);
        } catch {
          throw new TypeError(`extender root key is not hex: ${JSON.stringify(rootPublicKey)}`);
        }
        if (key.length !== 32) {
          throw new TypeError(`extender root key must be 32 bytes, got ${key.length}`);
        }
        publicKeys.push(key);
      } else if (rootPublicKey.length === 32) {
        publicKeys.push(new Uint8Array(rootPublicKey));
      }
    }
    this.publicKeys = publicKeys;
  }

  /**
   * Whether `signature` is a signature over `domain` || `body` by one of the
   * keys, as connect's ExtenderRootKeySet.Verify checks it.
   */
  async verify(domain: string, body: Uint8Array, signature: Uint8Array, options: { fallback?: boolean } = {}): Promise<boolean> {
    const domainBytes = new TextEncoder().encode(domain);
    const signingBytes = new Uint8Array(domainBytes.length + body.length);
    signingBytes.set(domainBytes, 0);
    signingBytes.set(body, domainBytes.length);
    for (const publicKey of this.publicKeys) {
      if (await verifyEd25519(publicKey, signingBytes, signature, options)) {
        return true;
      }
    }
    return false;
  }
}

/** One extender the directory holds an active record of. */
export interface ExtenderDirectoryEntry {
  /** the extender's identity key, lower case hex */
  publicKey: string;
  /** the record as it was signed */
  record: ExtenderRecord;
  body: ExtenderRecordBody;
  /** each address's canonical text, in record order, once each */
  ips: string[];
  /**
   * the extender's id, from the record's MeasurementId, when it serves the
   * lag.dev measurement API; null when it does not (C10)
   */
  measurementId: string | null;
  /** the host it serves the measurement API on, the name of that id; null when it does not (N1) */
  measurementHost: string | null;
}

export interface ExtenderDirectoryOptions {
  /** hello's `extender_root_public_keys`; a record signed by no other key is taken */
  rootPublicKeys: ReadonlyArray<string | Uint8Array>;
  /** the network hosts whose records are taken, e.g. ["bringyour.com", "ur.network"] (connect B2) */
  networkHosts: ReadonlyArray<string>;
  /** the zone of the measurement hosts; extender.bringyour.com by default */
  measurementZone?: string;
  expireSkewMs?: number;
  /** the most keys held; past it the earliest taken is dropped (default 4096) */
  maxKeys?: number;
  /** ms since the epoch, Date.now by default */
  now?: () => number;
  /** verify with the vendored implementation only, as a runtime without WebCrypto Ed25519 would */
  verifyFallback?: boolean;
}

interface DirectoryKey {
  record: ExtenderRecord | null;
  body: ExtenderRecordBody | null;
  revocationIssueTimeMs: number | null;
  entry: ExtenderDirectoryEntry | null;
}

// a host as connect's ExtenderNetworkHostAllowed compares it
function normalizeNetworkHost(host: string): string {
  let normalized = trimSpaceGo(host);
  if (normalized.endsWith(".")) {
    normalized = normalized.slice(0, -1);
  }
  return normalized.toLowerCase();
}

/**
 * Verified extender records, in memory. Records and revocations from any
 * source -- the feed, or a record an api answer carries -- are applied with
 * `applyRecord` and `applyRevocation`; `entries()` lists the active ones.
 */
export class ExtenderDirectory {
  readonly rootKeys: ExtenderRootKeys;
  private readonly networkHosts: Set<string>;
  private readonly measurementZone: string;
  private readonly expireSkewMs: number;
  private readonly maxKeys: number;
  private readonly now: () => number;
  private readonly verifyFallback: boolean;
  private readonly keys = new Map<string, DirectoryKey>();

  constructor(options: ExtenderDirectoryOptions) {
    this.rootKeys = new ExtenderRootKeys(options.rootPublicKeys);
    this.networkHosts = new Set(options.networkHosts.map(normalizeNetworkHost).filter((host) => host !== ""));
    this.measurementZone = options.measurementZone ?? DEFAULT_EXTENDER_MEASUREMENT_ZONE;
    this.expireSkewMs = options.expireSkewMs ?? DEFAULT_EXTENDER_RECORD_EXPIRE_SKEW_MS;
    this.maxKeys = options.maxKeys ?? 4096;
    this.now = options.now ?? Date.now;
    this.verifyFallback = options.verifyFallback ?? false;
  }

  private networkHostAllowed(networkHost: string): boolean {
    const candidate = normalizeNetworkHost(networkHost);
    return candidate !== "" && this.networkHosts.has(candidate);
  }

  private expired(body: ExtenderRecordBody, now: number): boolean {
    return body.expireTimeMs !== 0 && body.expireTimeMs + this.expireSkewMs < now;
  }

  private key(publicKey: string): DirectoryKey {
    let key = this.keys.get(publicKey);
    if (key === undefined) {
      key = { record: null, body: null, revocationIssueTimeMs: null, entry: null };
      this.keys.set(publicKey, key);
      if (this.maxKeys < this.keys.size) {
        const earliest = this.keys.keys().next().value;
        if (earliest !== undefined) {
          this.keys.delete(earliest);
        }
      }
    }
    return key;
  }

  // the entry of a key whose record is held and not revoked; expiry is judged
  // when the entries are read
  private refresh(key: DirectoryKey, publicKey: string): void {
    const { record, body, revocationIssueTimeMs } = key;
    if (record === null || body === null || (revocationIssueTimeMs !== null && body.issueTimeMs <= revocationIssueTimeMs)) {
      key.entry = null;
      return;
    }
    const ips: string[] = [];
    for (const address of body.addresses) {
      const ip = canonicalIp(address.ip);
      if (ip !== null && !ips.includes(ip)) {
        ips.push(ip);
      }
    }
    // an extender's id is 16 bytes that are not all zero; the field is empty
    // for an extender that does not serve the measurement API, and anything
    // else is no id and names no host
    const measurementId = extenderIdFromBytes(body.measurementId);
    const measurementHost = measurementId === null ? null : extenderMeasurementHost(measurementId, this.measurementZone);
    key.entry = { publicKey, record, body, ips, measurementId, measurementHost };
  }

  /**
   * Verifies a signed record and takes it when it is newer than the one held
   * for its key. A record that does not verify, names a network host not
   * allowed, carries no key or has expired is refused.
   */
  async applyRecord(record: ExtenderRecord): Promise<ExtenderApplyResult> {
    if (!(await this.rootKeys.verify(EXTENDER_RECORD_SIGNATURE_DOMAIN, record.body, record.rootSignature, { fallback: this.verifyFallback }))) {
      return { status: "refused", reason: "signature", message: "the record does not verify under any root key" };
    }
    let body: ExtenderRecordBody;
    try {
      body = decodeExtenderRecordBody(record.body);
    } catch (error) {
      return { status: "refused", reason: "malformed", message: String(error) };
    }
    if (!this.networkHostAllowed(body.networkHost)) {
      return { status: "refused", reason: "networkHost", message: `the record is for network host "${body.networkHost}"` };
    }
    if (body.publicKey.length === 0) {
      return { status: "refused", reason: "publicKey", message: "the record carries no public key" };
    }
    if (this.expired(body, this.now())) {
      return { status: "refused", reason: "expired", message: `the record expired at ${body.expireTimeMs} ms` };
    }
    const publicKey = bytesHex(body.publicKey);
    const key = this.key(publicKey);
    if (key.body !== null && body.issueTimeMs <= key.body.issueTimeMs) {
      // the newest record held wins (B5)
      return { status: "unchanged", publicKey };
    }
    key.record = record;
    key.body = body;
    this.refresh(key, publicKey);
    return { status: "applied", publicKey, entry: key.entry };
  }

  /**
   * Verifies a signed revocation and takes it when it is newer than the one
   * held for its key. It revokes the key's records issued at or before it,
   * including any that arrive later.
   */
  async applyRevocation(revocation: ExtenderRevocation): Promise<ExtenderApplyResult> {
    if (
      !(await this.rootKeys.verify(EXTENDER_REVOCATION_SIGNATURE_DOMAIN, revocation.body, revocation.rootSignature, {
        fallback: this.verifyFallback,
      }))
    ) {
      return { status: "refused", reason: "signature", message: "the revocation does not verify under any root key" };
    }
    let body: ExtenderRevocationBody;
    try {
      body = decodeExtenderRevocationBody(revocation.body);
    } catch (error) {
      return { status: "refused", reason: "malformed", message: String(error) };
    }
    if (!this.networkHostAllowed(body.networkHost)) {
      return { status: "refused", reason: "networkHost", message: `the revocation is for network host "${body.networkHost}"` };
    }
    if (body.publicKey.length === 0) {
      return { status: "refused", reason: "publicKey", message: "the revocation carries no public key" };
    }
    const publicKey = bytesHex(body.publicKey);
    const key = this.key(publicKey);
    if (key.revocationIssueTimeMs !== null && body.issueTimeMs <= key.revocationIssueTimeMs) {
      return { status: "unchanged", publicKey };
    }
    key.revocationIssueTimeMs = body.issueTimeMs;
    this.refresh(key, publicKey);
    return { status: "applied", publicKey, entry: key.entry };
  }

  /** The entry of an extender's identity key (hex), when its record is active. */
  get(publicKey: string): ExtenderDirectoryEntry | undefined {
    const entry = this.keys.get(publicKey.toLowerCase())?.entry;
    return entry && !this.expired(entry.body, this.now()) ? entry : undefined;
  }

  /** Every active entry: verified, not revoked and not expired, in the order taken. */
  entries(): ExtenderDirectoryEntry[] {
    const now = this.now();
    const entries: ExtenderDirectoryEntry[] = [];
    for (const key of this.keys.values()) {
      if (key.entry !== null && !this.expired(key.entry.body, now)) {
        entries.push(key.entry);
      }
    }
    return entries;
  }

  /** The active entries that serve the measurement API: those whose record carries a MeasurementId (C10). */
  measurementEntries(): ExtenderDirectoryEntry[] {
    return this.entries().filter((entry) => entry.measurementHost !== null);
  }
}

/** What the feed client reports as it reads. */
export type ExtenderFeedEvent =
  | { type: "open" }
  | { type: "record"; publicKey: string; entry: ExtenderDirectoryEntry | null }
  | { type: "revocation"; publicKey: string }
  | { type: "refused"; reason: ExtenderRefusal | "frame"; message: string }
  | { type: "endOfSample" }
  | { type: "closed"; error?: Error };

export interface ExtenderFeedTransportOptions {
  /** the extender's measurement host; the feed is read at wss://<host>/ur/feed/v1 */
  host: string;
  /** the whole url instead, for a test server */
  url?: string;
  /** a WebSocket constructor; the runtime's global WebSocket by default */
  WebSocket?: new (url: string) => WebSocket;
  /** how long the sample may take until its end (default 30 s, connect's dial timeout) */
  sampleTimeoutMs?: number;
  /** how long a subscription may stay silent; the server keeps it alive every 30 s (default 90 s) */
  idleTimeoutMs?: number;
  onEvent?: (event: ExtenderFeedEvent) => void;
}

export type ExtenderFeedClientOptions = ExtenderFeedTransportOptions &
  ({ directory: ExtenderDirectory } | ExtenderDirectoryOptions);

export interface ExtenderFeedSampleOptions {
  /** keep the stream open after the sample and apply every record and revocation that follows */
  subscribe?: boolean;
  signal?: AbortSignal;
}

export class ExtenderFeedError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ExtenderFeedError";
  }
}

/**
 * Reads the extender feed of one extender over WSS (M9) into a directory.
 *
 * @example
 * const feed = new ExtenderFeedClient({
 *   host: "0192e4a1-7b3c-7d2e-9f10-5a6b7c8d9e0f.extender.bringyour.com",
 *   rootPublicKeys: hello.extender_root_public_keys,
 *   networkHosts: ["bringyour.com", "ur.network"],
 * });
 * await feed.sample(8);
 * const hosts = feed.directory.measurementEntries().map((entry) => entry.measurementHost);
 */
export class ExtenderFeedClient {
  readonly directory: ExtenderDirectory;
  private readonly options: ExtenderFeedTransportOptions;
  // ends the open stream, if any
  private stop: (() => void) | null = null;

  constructor(options: ExtenderFeedClientOptions) {
    this.directory = "directory" in options ? options.directory : new ExtenderDirectory(options);
    this.options = options;
  }

  /** The feed's url. */
  get url(): string {
    return this.options.url ?? `wss://${this.options.host}/ur/feed/v1`;
  }

  /** Whether a stream is open, sampling or subscribed. */
  get open(): boolean {
    return this.stop !== null;
  }

  private emit(event: ExtenderFeedEvent): void {
    try {
      this.options.onEvent?.(event);
    } catch {
      // a listener's error is the listener's
    }
  }

  /**
   * Opens the feed, asks for `count` records, and resolves with the
   * directory's active entries once the sample has been applied. Without
   * `subscribe` the stream then closes; with it the stream stays open and
   * applies what follows until `close()`, a silence of `idleTimeoutMs`, or
   * the server ends it. One stream at a time.
   */
  sample(count: number = DEFAULT_EXTENDER_FEED_SAMPLE_COUNT, options: ExtenderFeedSampleOptions = {}): Promise<ExtenderDirectoryEntry[]> {
    if (this.stop !== null) {
      return Promise.reject(new ExtenderFeedError("the feed is already open"));
    }
    const subscribe = options.subscribe ?? false;
    let request: Uint8Array<ArrayBuffer>;
    try {
      request = encodeExtenderFeedRequest({ sampleCount: count, subscribe });
    } catch (error) {
      return Promise.reject(error);
    }
    const WebSocketImpl = this.options.WebSocket ?? globalThis.WebSocket;
    if (WebSocketImpl === undefined) {
      return Promise.reject(new ExtenderFeedError("this runtime has no WebSocket; pass one as options.WebSocket"));
    }
    if (options.signal?.aborted) {
      return Promise.reject(new ExtenderFeedError("aborted"));
    }
    const sampleTimeoutMs = this.options.sampleTimeoutMs ?? 30_000;
    const idleTimeoutMs = this.options.idleTimeoutMs ?? 90_000;

    return new Promise<ExtenderDirectoryEntry[]>((resolve, reject) => {
      let socket: WebSocket;
      try {
        socket = new WebSocketImpl(this.url);
      } catch (error) {
        reject(error);
        return;
      }
      socket.binaryType = "arraybuffer";
      let sampled = false;
      let ended = false;
      // frames are applied one at a time and in order, though verifying is async
      let applying: Promise<void> = Promise.resolve();
      let timer: ReturnType<typeof setTimeout> | undefined;

      // Ends the stream once. A sample still pending rejects with the error;
      // after the sample an error ends a subscription and is reported, and
      // no error is an orderly end.
      const end = (error?: Error, byClient = false) => {
        if (ended) {
          return;
        }
        ended = true;
        if (timer !== undefined) {
          clearTimeout(timer);
        }
        options.signal?.removeEventListener("abort", onAbort);
        socket.onopen = null;
        socket.onmessage = null;
        socket.onerror = null;
        socket.onclose = null;
        try {
          socket.close(1000);
        } catch {
          // already closed
        }
        this.stop = null;
        if (!sampled) {
          reject(error ?? new ExtenderFeedError("the feed closed before the sample"));
        }
        this.emit(error && !byClient ? { type: "closed", error } : { type: "closed" });
      };
      const arm = (ms: number) => {
        if (timer !== undefined) {
          clearTimeout(timer);
        }
        timer = setTimeout(() => {
          end(new ExtenderFeedError(sampled ? "the subscription went silent" : "the sample timed out"));
        }, ms);
      };
      const onAbort = () => end(new ExtenderFeedError("aborted"), true);
      // a close or failure ends the stream after the frames already received
      // are applied, so a sample whose end arrived just before the close
      // still resolves; `error` gives the error once that is known
      const endAfterApplying = (error: () => Error | undefined) => {
        void applying.then(() => end(error()));
      };
      const endOfSample = () => {
        if (ended) {
          return;
        }
        sampled = true;
        this.emit({ type: "endOfSample" });
        resolve(this.directory.entries());
        if (subscribe) {
          arm(idleTimeoutMs);
        } else {
          end();
        }
      };

      this.stop = () => end(new ExtenderFeedError("closed"), true);
      options.signal?.addEventListener("abort", onAbort);
      arm(sampleTimeoutMs);

      socket.onopen = () => {
        this.emit({ type: "open" });
        try {
          socket.send(request);
        } catch (error) {
          end(error instanceof Error ? error : new ExtenderFeedError(String(error)));
        }
      };
      socket.onerror = () => {
        endAfterApplying(() => new ExtenderFeedError("the feed connection failed"));
      };
      socket.onclose = (event) => {
        // the server ends a feed with a normal close; after the sample that
        // is an orderly end, and anything else is a failure
        endAfterApplying(() => {
          if (!sampled) {
            return new ExtenderFeedError(`the feed closed before the sample (${event.code})`);
          }
          return event.code === 1000 ? undefined : new ExtenderFeedError(`the feed closed (${event.code})`);
        });
      };
      socket.onmessage = (event) => {
        const data: unknown = event.data;
        if (!(data instanceof ArrayBuffer)) {
          // the feed is binary; text is not a frame
          end(new ExtenderFeedError("the feed sent a message that is not binary"));
          return;
        }
        if (EXTENDER_FEED_MAX_FRAME_BYTES < data.byteLength) {
          end(new ExtenderFeedError(`a feed frame is ${data.byteLength} bytes, at most ${EXTENDER_FEED_MAX_FRAME_BYTES}`));
          return;
        }
        if (sampled) {
          arm(idleTimeoutMs);
        }
        const bytes = new Uint8Array(data);
        applying = applying.then(() => (ended ? undefined : this.applyFrame(bytes, endOfSample)));
      };
    });
  }

  private async applyFrame(bytes: Uint8Array, endOfSample: () => void): Promise<void> {
    let frame: ExtenderFeedFrame;
    try {
      frame = decodeExtenderFeedFrame(bytes);
    } catch (error) {
      this.emit({ type: "refused", reason: "frame", message: String(error) });
      return;
    }
    switch (frame.kind) {
      case "record": {
        const result = await this.directory.applyRecord(frame.record);
        if (result.status === "applied") {
          this.emit({ type: "record", publicKey: result.publicKey, entry: result.entry });
        } else if (result.status === "refused") {
          this.emit({ type: "refused", reason: result.reason, message: result.message });
        }
        return;
      }
      case "revocation": {
        const result = await this.directory.applyRevocation(frame.revocation);
        if (result.status === "applied") {
          this.emit({ type: "revocation", publicKey: result.publicKey });
        } else if (result.status === "refused") {
          this.emit({ type: "refused", reason: result.reason, message: result.message });
        }
        return;
      }
      case "endOfSample":
        endOfSample();
        return;
      default:
        // a keepalive, or nothing
        return;
    }
  }

  /** Closes the open stream, if any. A sample still pending rejects. */
  close(): void {
    this.stop?.();
  }
}
