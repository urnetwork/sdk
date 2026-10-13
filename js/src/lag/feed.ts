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
// TweetNaCl (tweetnacl_verify.ts), run a slice at a time so a check never
// holds the event loop. Both refuse a non-canonical S, as Go's
// crypto/ed25519 does.
//
// An extender is a volunteer's machine, so the feed client bounds what one
// can make it hold (S3 Bounds): a sample may run at most a few frames past the
// count it asked for before it ends, and no more than 64 frames wait to be
// verified at once, the most the feed server lets a subscriber fall behind
// (D4). Past either, the stream fails.

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
import { tweetnaclPrimitives } from "./tweetnacl_verify";

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
/**
 * How many frames past the count it asked for a sample may carry before its
 * end (S3 Bounds). The server sends at most the count, its own record among
 * them, so the margin is only slack.
 */
export const EXTENDER_FEED_SAMPLE_MARGIN = 4;
/**
 * The most frames that may wait to be verified at once (S3 Bounds): as many as
 * the feed server lets a subscriber fall behind before it drops it (D4).
 */
export const EXTENDER_FEED_MAX_PENDING_FRAMES = 64;
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
 * otherwise with the vendored TweetNaCl, run in slices so a check never holds
 * the event loop (`verifyEd25519Sliced`). A signature or key of the wrong
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
  return verifyEd25519Sliced(publicKey, message, signature);
}

/** How long the fallback verification works before it gives the event loop a turn. */
export const ED25519_SLICE_MS = 8;

// lets the event loop run: other tasks, input and rendering in a page. A
// MessageChannel turn is not clamped to 4 ms the way nested timers are.
function yieldToEventLoop(): Promise<void> {
  return new Promise((resolve) => {
    if (typeof MessageChannel === "function") {
      const channel = new MessageChannel();
      channel.port1.onmessage = () => {
        channel.port1.close();
        resolve();
      };
      channel.port2.postMessage(null);
    } else {
      setTimeout(resolve, 0);
    }
  });
}

const clock = () => (typeof performance !== "undefined" ? performance.now() : Date.now());

export interface Ed25519SliceOptions {
  /** the work between turns of the event loop, ED25519_SLICE_MS by default; 0 yields at every step */
  sliceMs?: number;
  /** hash with the vendored SHA-512 even where WebCrypto's is available */
  vendoredHash?: boolean;
  /** how to give the event loop a turn */
  yieldNow?: () => Promise<void>;
}

type TweetnaclPoint = [Float64Array, Float64Array, Float64Array, Float64Array];

/**
 * TweetNaCl's detached verification (crypto_sign_open) taken a slice at a
 * time: the same primitives in the same order, with a turn of the event loop
 * whenever a slice has run `sliceMs`. The SHA-512 of R || A || M is
 * WebCrypto's where the runtime has it, and otherwise TweetNaCl's, a block
 * range at a time. A 64 KiB record holds the event loop for a slice, not for
 * the whole check. Like TweetNaCl it does not refuse a non-canonical S;
 * `verifyEd25519` checks that first.
 */
export async function verifyEd25519Sliced(
  publicKey: Uint8Array,
  message: Uint8Array,
  signature: Uint8Array,
  options: Ed25519SliceOptions = {},
): Promise<boolean> {
  if (publicKey.length !== 32 || signature.length !== 64) {
    return false;
  }
  const nacl = tweetnaclPrimitives();
  const sliceMs = options.sliceMs ?? ED25519_SLICE_MS;
  const yieldNow = options.yieldNow ?? yieldToEventLoop;
  let sliceStart = clock();
  const pace = async () => {
    if (sliceMs <= clock() - sliceStart) {
      await yieldNow();
      sliceStart = clock();
    }
  };
  const point = (): TweetnaclPoint => [nacl.gf(), nacl.gf(), nacl.gf(), nacl.gf()];
  // TweetNaCl's scalarmult, pacing every 4 of its 256 steps
  const scalarmult = async (p: TweetnaclPoint, q: TweetnaclPoint, s: Uint8Array) => {
    nacl.set25519(p[0], nacl.gf0);
    nacl.set25519(p[1], nacl.gf1);
    nacl.set25519(p[2], nacl.gf1);
    nacl.set25519(p[3], nacl.gf0);
    for (let i = 255; 0 <= i; i -= 1) {
      const b = (s[(i / 8) | 0] >> (i & 7)) & 1;
      nacl.cswap(p, q, b);
      nacl.add(q, p);
      nacl.add(p, p);
      nacl.cswap(p, q, b);
      if (i % 4 === 0) {
        await pace();
      }
    }
  };

  // as crypto_sign_open: -A from the key
  const q = point();
  if (nacl.unpackneg(q, publicKey)) {
    return false;
  }
  await pace();
  // h = SHA-512(R || A || M), reduced mod L
  const hashed = new Uint8Array(64 + message.length);
  hashed.set(signature.subarray(0, 32), 0);
  hashed.set(publicKey, 32);
  hashed.set(message, 64);
  const subtle = options.vendoredHash ? undefined : globalThis.crypto?.subtle;
  const h = new Uint8Array(64);
  h.set(subtle ? new Uint8Array(await subtle.digest("SHA-512", hashed)) : await sha512Sliced(hashed, pace));
  nacl.reduce(h);
  // [h](-A) + [S]B, which is R when the signature is good (scalarbase is
  // scalarmult from the base point)
  const p = point();
  await scalarmult(p, q, h);
  const base = point();
  nacl.set25519(base[0], nacl.X);
  nacl.set25519(base[1], nacl.Y);
  nacl.set25519(base[2], nacl.gf1);
  nacl.M(base[3], nacl.X, nacl.Y);
  await scalarmult(q, base, signature.subarray(32));
  nacl.add(p, q);
  const t = new Uint8Array(32);
  nacl.pack(t, p);
  return nacl.crypto_verify_32(signature, 0, t, 0) === 0;
}

// TweetNaCl's crypto_hash, pacing every 8 whole blocks
async function sha512Sliced(m: Uint8Array, pace: () => Promise<void>): Promise<Uint8Array> {
  const nacl = tweetnaclPrimitives();
  const h = new Uint8Array(64);
  h.set(nacl.iv);
  const b = m.length;
  const wholeBlocks = b - (b % 128);
  for (let offset = 0; offset < wholeBlocks; ) {
    const length = Math.min(128 * 8, wholeBlocks - offset);
    nacl.crypto_hashblocks(h, m.subarray(offset, offset + length), length);
    offset += length;
    await pace();
  }
  // then the last partial block, padded, and the length, as crypto_hash ends
  const x = new Uint8Array(256);
  let n = b % 128;
  for (let i = 0; i < n; i += 1) {
    x[i] = m[b - n + i];
  }
  x[n] = 128;
  n = 256 - 128 * (n < 112 ? 1 : 0);
  x[n - 9] = 0;
  nacl.ts64(x, n - 8, new nacl.u64((b / 0x20000000) | 0, b << 3));
  nacl.crypto_hashblocks(h, x, n);
  return h;
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
      // what one extender can make the client hold (S3 Bounds): the frames a
      // sample may carry before its end, and the frames waiting at once
      const sampleAllowance = count + EXTENDER_FEED_SAMPLE_MARGIN;
      let sampleFrames = 0;
      let endReceived = false;
      let pending = 0;

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
      // queues a frame's work behind the frames before it; past the cap the
      // stream fails rather than hold more. The end of the sample, which
      // comes once and holds nothing, does not count.
      const enqueue = (work: () => Promise<void> | void, frame = true) => {
        if (frame && EXTENDER_FEED_MAX_PENDING_FRAMES <= pending) {
          end(new ExtenderFeedError(`more than ${EXTENDER_FEED_MAX_PENDING_FRAMES} feed frames are waiting to be verified`));
          return;
        }
        if (frame) {
          pending += 1;
        }
        applying = applying.then(async () => {
          try {
            if (!ended) {
              await work();
            }
          } catch (error) {
            this.emit({ type: "refused", reason: "frame", message: String(error) });
          } finally {
            if (frame) {
              pending -= 1;
            }
          }
        });
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
        // decoding is cheap and synchronous; only verifying waits its turn
        let frame: ExtenderFeedFrame | null = null;
        let malformed = "";
        try {
          frame = decodeExtenderFeedFrame(new Uint8Array(data));
        } catch (error) {
          malformed = String(error);
        }
        // a sample carries at most what it asked for, and then its end;
        // every other frame before the end counts, whatever it holds
        if (!endReceived && frame?.kind !== "endOfSample") {
          sampleFrames += 1;
          if (sampleAllowance < sampleFrames) {
            end(new ExtenderFeedError(`the sample sent more than ${sampleAllowance} frames for the ${count} records it asked for`));
            return;
          }
        }
        if (frame === null) {
          enqueue(() => this.emit({ type: "refused", reason: "frame", message: malformed }));
          return;
        }
        switch (frame.kind) {
          case "record": {
            const record = frame.record;
            enqueue(() => this.applyRecord(record));
            return;
          }
          case "revocation": {
            const revocation = frame.revocation;
            enqueue(() => this.applyRevocation(revocation));
            return;
          }
          case "endOfSample":
            if (!endReceived) {
              endReceived = true;
              enqueue(endOfSample, false);
            }
            return;
          default:
            // a keepalive, or nothing: it only shows the stream is alive
            return;
        }
      };
    });
  }

  private async applyRecord(record: ExtenderRecord): Promise<void> {
    const result = await this.directory.applyRecord(record);
    if (result.status === "applied") {
      this.emit({ type: "record", publicKey: result.publicKey, entry: result.entry });
    } else if (result.status === "refused") {
      this.emit({ type: "refused", reason: result.reason, message: result.message });
    }
  }

  private async applyRevocation(revocation: ExtenderRevocation): Promise<void> {
    const result = await this.directory.applyRevocation(revocation);
    if (result.status === "applied") {
      this.emit({ type: "revocation", publicKey: result.publicKey });
    } else if (result.status === "refused") {
      this.emit({ type: "refused", reason: result.reason, message: result.message });
    }
  }

  /** Closes the open stream, if any. A sample still pending rejects. */
  close(): void {
    this.stop?.();
  }
}
