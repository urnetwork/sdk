// A minimal protobuf reader for the extender feed and the lag ticket
// (lag.dev/PLAN.md S3, M0, M9; connect protocol/extender.proto, lag.proto).
//
// It reads ExtenderFeedFrame, ExtenderRecord, ExtenderRecordBody (with
// MeasurementId, field 17), ExtenderRevocation and its body, and LagTicket
// and its body, and writes ExtenderFeedRequest. It reads what Go's
// proto.Unmarshal reads the way it reads it:
// - an unknown field, or a known one with an unexpected wire type, is skipped;
// - a scalar written twice keeps the last value, a repeated field appends, and
//   a packed or unpacked repeated number is accepted either way;
// - a uint32 keeps the low 32 bits of its varint, and a bool is any non-zero;
// - a string must be valid UTF-8;
// - a oneof message written twice in the same case merges, and another case
//   replaces it.
// A uint64 is returned as a number: exact to 2^53, which every time and byte
// count here is far below. Malformed input throws a ProtobufError.

export class ProtobufError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ProtobufError";
  }
}

const VARINT = 0;
const FIXED64 = 1;
const BYTES = 2;
const START_GROUP = 3;
const END_GROUP = 4;
const FIXED32 = 5;

// unknown groups nest; past this depth the input is refused
const MAX_GROUP_DEPTH = 64;

const utf8Decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });

class Reader {
  bytes: Uint8Array;
  pos: number;
  end: number;
  // the last varint, as two unsigned 32 bit halves
  lo = 0;
  hi = 0;

  constructor(bytes: Uint8Array, pos = 0, end = bytes.length) {
    this.bytes = bytes;
    this.pos = pos;
    this.end = end;
  }

  done(): boolean {
    return this.end <= this.pos;
  }

  // protowire.ConsumeVarint: at most ten bytes, the tenth at most 1
  varint(): void {
    let lo = 0;
    let hi = 0;
    for (let i = 0; i < 10; i += 1) {
      if (this.end <= this.pos) {
        throw new ProtobufError("truncated varint");
      }
      const b = this.bytes[this.pos];
      this.pos += 1;
      if (i < 4) {
        lo |= (b & 0x7f) << (7 * i);
      } else if (i === 4) {
        lo |= (b & 0x0f) << 28;
        hi |= (b & 0x7f) >>> 4;
      } else if (i < 9) {
        hi |= (b & 0x7f) << (7 * i - 32);
      } else {
        if (1 < b) {
          throw new ProtobufError("varint overflows 64 bits");
        }
        hi |= b << 31;
      }
      if (b < 0x80) {
        this.lo = lo >>> 0;
        this.hi = hi >>> 0;
        return;
      }
    }
    throw new ProtobufError("varint overflows 64 bits");
  }

  uint32(): number {
    this.varint();
    return this.lo;
  }

  uint64(): number {
    this.varint();
    return this.hi * 0x100000000 + this.lo;
  }

  bool(): boolean {
    this.varint();
    return (this.lo | this.hi) !== 0;
  }

  // the bounds of a length-delimited value, consumed
  delimited(): [number, number] {
    this.varint();
    const length = this.hi * 0x100000000 + this.lo;
    if (this.end - this.pos < length) {
      throw new ProtobufError("truncated length-delimited field");
    }
    const start = this.pos;
    this.pos += length;
    return [start, this.pos];
  }

  bytesField(): Uint8Array {
    const [start, end] = this.delimited();
    return this.bytes.slice(start, end);
  }

  stringField(): string {
    const [start, end] = this.delimited();
    try {
      return utf8Decoder.decode(this.bytes.subarray(start, end));
    } catch {
      throw new ProtobufError("string field is not valid UTF-8");
    }
  }

  // a field's number and wire type; the end of a group is the caller's
  tag(): [number, number] {
    this.varint();
    if (this.hi !== 0) {
      throw new ProtobufError("invalid field number");
    }
    const number = this.lo >>> 3;
    if (number === 0) {
      throw new ProtobufError("invalid field number");
    }
    return [number, this.lo & 7];
  }

  // protowire.ConsumeFieldValue: skips one value of an unknown field
  skip(number: number, wireType: number, depth = 0): void {
    switch (wireType) {
      case VARINT:
        this.varint();
        return;
      case FIXED64:
        this.advance(8);
        return;
      case BYTES:
        this.delimited();
        return;
      case FIXED32:
        this.advance(4);
        return;
      case START_GROUP: {
        if (MAX_GROUP_DEPTH <= depth) {
          throw new ProtobufError("groups nest too deeply");
        }
        for (;;) {
          if (this.done()) {
            throw new ProtobufError("missing end group marker");
          }
          const [innerNumber, innerWireType] = this.tag();
          if (innerWireType === END_GROUP) {
            if (innerNumber !== number) {
              throw new ProtobufError("mismatching end group marker");
            }
            return;
          }
          this.skip(innerNumber, innerWireType, depth + 1);
        }
      }
      case END_GROUP:
        throw new ProtobufError("mismatching end group marker");
      default:
        throw new ProtobufError(`reserved wire type ${wireType}`);
    }
  }

  advance(n: number): void {
    if (this.end - this.pos < n) {
      throw new ProtobufError("truncated fixed-width field");
    }
    this.pos += n;
  }
}

// Reads every field of a message, calling `field` for each. `field` consumes
// the value of a field it knows with the wire type it expects and returns
// true; for anything else it returns false and the value is skipped.
function readMessage(
  bytes: Uint8Array,
  field: (reader: Reader, number: number, wireType: number) => boolean,
): void {
  const reader = new Reader(bytes);
  while (!reader.done()) {
    const [number, wireType] = reader.tag();
    if (wireType === END_GROUP) {
      throw new ProtobufError("mismatching end group marker");
    }
    if (!field(reader, number, wireType)) {
      reader.skip(number, wireType);
    }
  }
}

// a repeated uint32, packed or not
function readUint32s(reader: Reader, wireType: number, out: number[]): boolean {
  if (wireType === VARINT) {
    out.push(reader.uint32());
    return true;
  }
  if (wireType === BYTES) {
    const [start, end] = reader.delimited();
    const packed = new Reader(reader.bytes, start, end);
    while (!packed.done()) {
      out.push(packed.uint32());
    }
    return true;
  }
  return false;
}

/** One address of an extender record. */
export interface ExtenderAddress {
  ip: string;
  ipVersion: number;
  /** tcp, quic, dns, webrtc */
  carriers: string[];
}

/** What the operator signs about one extender (connect protocol/extender.proto). */
export interface ExtenderRecordBody {
  /** the extender's ed25519 identity key */
  publicKey: Uint8Array;
  addresses: ExtenderAddress[];
  tcpPort: number;
  udpPort: number;
  dnsPort: number;
  dnsTld: string;
  countryCode: string;
  issueTimeMs: number;
  /** 0 when the record does not expire */
  expireTimeMs: number;
  networkHost: string;
  dnsPorts: number[];
  continentCode: string;
  /** 0 open, 1 gated (connect/EXTENDER.md Q1) */
  directoryTier: number;
  webRtcClientId: Uint8Array;
  realityPublicKey: Uint8Array;
  canaryChannel: string;
  /**
   * the extender's 16-byte id when it serves the lag.dev measurement API, which
   * is then at the measurement name of that id; empty when it does not (C10)
   */
  measurementId: Uint8Array;
}

/** A signed record: the body as the exact bytes that were signed. */
export interface ExtenderRecord {
  body: Uint8Array;
  /** ed25519 over "ur-extender-record-v1" || body */
  rootSignature: Uint8Array;
  /** the first 8 bytes of the sha256 of the signing key, a hint */
  rootKeyId: Uint8Array;
}

export interface ExtenderRevocationBody {
  publicKey: Uint8Array;
  issueTimeMs: number;
  networkHost: string;
}

/** A signed revocation: the body as the exact bytes that were signed. */
export interface ExtenderRevocation {
  body: Uint8Array;
  /** ed25519 over "ur-extender-revocation-v1" || body */
  rootSignature: Uint8Array;
  rootKeyId: Uint8Array;
}

/**
 * One feed frame as connect's feed client reads it: a record, a revocation,
 * the end of the sample, a keepalive, or nothing (a frame with no case set,
 * or a bool case set false).
 */
export type ExtenderFeedFrame =
  | { kind: "record"; record: ExtenderRecord }
  | { kind: "revocation"; revocation: ExtenderRevocation }
  | { kind: "endOfSample" }
  | { kind: "keepalive" }
  | { kind: "empty" };

export interface ExtenderFeedRequest {
  sampleCount: number;
  subscribe: boolean;
}

function mergeExtenderAddress(target: ExtenderAddress, bytes: Uint8Array): ExtenderAddress {
  readMessage(bytes, (reader, number, wireType) => {
    switch (number) {
      case 1:
        if (wireType !== BYTES) return false;
        target.ip = reader.stringField();
        return true;
      case 2:
        if (wireType !== VARINT) return false;
        target.ipVersion = reader.uint32();
        return true;
      case 3:
        if (wireType !== BYTES) return false;
        target.carriers.push(reader.stringField());
        return true;
    }
    return false;
  });
  return target;
}

/** Decodes a serialized ExtenderRecordBody. */
export function decodeExtenderRecordBody(bytes: Uint8Array): ExtenderRecordBody {
  const body: ExtenderRecordBody = {
    publicKey: new Uint8Array(0),
    addresses: [],
    tcpPort: 0,
    udpPort: 0,
    dnsPort: 0,
    dnsTld: "",
    countryCode: "",
    issueTimeMs: 0,
    expireTimeMs: 0,
    networkHost: "",
    dnsPorts: [],
    continentCode: "",
    directoryTier: 0,
    webRtcClientId: new Uint8Array(0),
    realityPublicKey: new Uint8Array(0),
    canaryChannel: "",
    measurementId: new Uint8Array(0),
  };
  readMessage(bytes, (reader, number, wireType) => {
    switch (number) {
      case 1:
        if (wireType !== BYTES) return false;
        body.publicKey = reader.bytesField();
        return true;
      case 2: {
        if (wireType !== BYTES) return false;
        const [start, end] = reader.delimited();
        body.addresses.push(
          mergeExtenderAddress({ ip: "", ipVersion: 0, carriers: [] }, reader.bytes.subarray(start, end)),
        );
        return true;
      }
      case 3:
        if (wireType !== VARINT) return false;
        body.tcpPort = reader.uint32();
        return true;
      case 4:
        if (wireType !== VARINT) return false;
        body.udpPort = reader.uint32();
        return true;
      case 5:
        if (wireType !== VARINT) return false;
        body.dnsPort = reader.uint32();
        return true;
      case 6:
        if (wireType !== BYTES) return false;
        body.dnsTld = reader.stringField();
        return true;
      case 7:
        if (wireType !== BYTES) return false;
        body.countryCode = reader.stringField();
        return true;
      case 8:
        if (wireType !== VARINT) return false;
        body.issueTimeMs = reader.uint64();
        return true;
      case 9:
        if (wireType !== VARINT) return false;
        body.expireTimeMs = reader.uint64();
        return true;
      case 10:
        if (wireType !== BYTES) return false;
        body.networkHost = reader.stringField();
        return true;
      case 11:
        return readUint32s(reader, wireType, body.dnsPorts);
      case 12:
        if (wireType !== BYTES) return false;
        body.continentCode = reader.stringField();
        return true;
      case 13:
        if (wireType !== VARINT) return false;
        body.directoryTier = reader.uint32();
        return true;
      case 14:
        if (wireType !== BYTES) return false;
        body.webRtcClientId = reader.bytesField();
        return true;
      case 15:
        if (wireType !== BYTES) return false;
        body.realityPublicKey = reader.bytesField();
        return true;
      case 16:
        if (wireType !== BYTES) return false;
        body.canaryChannel = reader.stringField();
        return true;
      case 17:
        if (wireType !== BYTES) return false;
        body.measurementId = reader.bytesField();
        return true;
    }
    return false;
  });
  return body;
}

// ExtenderRecord and ExtenderRevocation share their three fields
interface Signed {
  body: Uint8Array;
  rootSignature: Uint8Array;
  rootKeyId: Uint8Array;
}

function emptySigned(): Signed {
  return { body: new Uint8Array(0), rootSignature: new Uint8Array(0), rootKeyId: new Uint8Array(0) };
}

function mergeSigned(target: Signed, bytes: Uint8Array): Signed {
  readMessage(bytes, (reader, number, wireType) => {
    if (wireType !== BYTES) {
      return false;
    }
    switch (number) {
      case 1:
        target.body = reader.bytesField();
        return true;
      case 2:
        target.rootSignature = reader.bytesField();
        return true;
      case 3:
        target.rootKeyId = reader.bytesField();
        return true;
    }
    return false;
  });
  return target;
}

/** Decodes a serialized ExtenderRecord. The body is not decoded or verified. */
export function decodeExtenderRecord(bytes: Uint8Array): ExtenderRecord {
  return mergeSigned(emptySigned(), bytes);
}

/** Decodes a serialized ExtenderRevocation. The body is not decoded or verified. */
export function decodeExtenderRevocation(bytes: Uint8Array): ExtenderRevocation {
  return mergeSigned(emptySigned(), bytes);
}

/** Decodes a serialized ExtenderRevocationBody. */
export function decodeExtenderRevocationBody(bytes: Uint8Array): ExtenderRevocationBody {
  const body: ExtenderRevocationBody = { publicKey: new Uint8Array(0), issueTimeMs: 0, networkHost: "" };
  readMessage(bytes, (reader, number, wireType) => {
    switch (number) {
      case 1:
        if (wireType !== BYTES) return false;
        body.publicKey = reader.bytesField();
        return true;
      case 2:
        if (wireType !== VARINT) return false;
        body.issueTimeMs = reader.uint64();
        return true;
      case 3:
        if (wireType !== BYTES) return false;
        body.networkHost = reader.stringField();
        return true;
    }
    return false;
  });
  return body;
}

/** Decodes one ExtenderFeedFrame, one binary message of the feed (M9). */
export function decodeExtenderFeedFrame(bytes: Uint8Array): ExtenderFeedFrame {
  // the oneof: the case written last, and its value
  const oneof: { number: number; signed: Signed | null; flag: boolean } = { number: 0, signed: null, flag: false };
  readMessage(bytes, (reader, number, wireType) => {
    switch (number) {
      case 1:
      case 2: {
        if (wireType !== BYTES) return false;
        const [start, end] = reader.delimited();
        // the same case merges into the message it holds; another starts over
        const target = oneof.number === number && oneof.signed !== null ? oneof.signed : emptySigned();
        oneof.signed = mergeSigned(target, reader.bytes.subarray(start, end));
        oneof.number = number;
        return true;
      }
      case 3:
      case 4:
        if (wireType !== VARINT) return false;
        oneof.flag = reader.bool();
        oneof.signed = null;
        oneof.number = number;
        return true;
    }
    return false;
  });
  switch (oneof.number) {
    case 1:
      return { kind: "record", record: oneof.signed as Signed };
    case 2:
      return { kind: "revocation", revocation: oneof.signed as Signed };
    case 3:
      // as connect reads it, a false end of sample or keepalive is nothing
      return oneof.flag ? { kind: "endOfSample" } : { kind: "empty" };
    case 4:
      return oneof.flag ? { kind: "keepalive" } : { kind: "empty" };
    default:
      return { kind: "empty" };
  }
}

function writeVarint(out: number[], value: number): void {
  let v = value;
  while (0x80 <= v) {
    out.push((v & 0x7f) | 0x80);
    v = Math.floor(v / 128);
  }
  out.push(v);
}

/**
 * Serializes an ExtenderFeedRequest as Go's proto.Marshal does: the browser's
 * first message on the feed (M9).
 */
export function encodeExtenderFeedRequest(request: ExtenderFeedRequest): Uint8Array<ArrayBuffer> {
  const { sampleCount, subscribe } = request;
  if (!(Number.isInteger(sampleCount) && 0 <= sampleCount && sampleCount <= 0xffffffff)) {
    throw new RangeError(`sampleCount must be a uint32, got ${sampleCount}`);
  }
  const out: number[] = [];
  if (sampleCount !== 0) {
    out.push((1 << 3) | VARINT);
    writeVarint(out, sampleCount);
  }
  if (subscribe) {
    out.push((2 << 3) | VARINT, 1);
  }
  return Uint8Array.from(out);
}

/** The operator's admission of one lag.dev session on one extender (M0, M3). */
export interface LagTicketBody {
  /** 16 bytes */
  ticketId: Uint8Array;
  /** 16 bytes, the lag guest client */
  clientId: Uint8Array;
  /** the extender's ed25519 identity key */
  extenderPublicKey: Uint8Array;
  networkHost: string;
  issueTimeMs: number;
  /** the session must open before this */
  expireTimeMs: number;
  /** 16 bytes, shared by the track's sessions */
  trackId: Uint8Array;
  /** the dot's place: 0-7, spares 8-11 */
  trackPosition: number;
  pingIntervalMs: number;
  downloadMs: number;
  uploadMs: number;
  /** per direction */
  maxStreams: number;
  maxDownloadBytes: number;
  maxUploadBytes: number;
}

/** A signed LagTicket: the body as the exact bytes that were signed. */
export interface SignedLagTicket {
  body: Uint8Array;
  /** ed25519 over "ur-lag-ticket-v1" || body, by the key that signs records */
  rootSignature: Uint8Array;
  rootKeyId: Uint8Array;
}

/** Decodes a serialized LagTicketBody (M0, as amended). */
export function decodeLagTicketBody(bytes: Uint8Array): LagTicketBody {
  const body: LagTicketBody = {
    ticketId: new Uint8Array(0),
    clientId: new Uint8Array(0),
    extenderPublicKey: new Uint8Array(0),
    networkHost: "",
    issueTimeMs: 0,
    expireTimeMs: 0,
    trackId: new Uint8Array(0),
    trackPosition: 0,
    pingIntervalMs: 0,
    downloadMs: 0,
    uploadMs: 0,
    maxStreams: 0,
    maxDownloadBytes: 0,
    maxUploadBytes: 0,
  };
  readMessage(bytes, (reader, number, wireType) => {
    switch (number) {
      case 1:
      case 2:
      case 3:
      case 7: {
        if (wireType !== BYTES) return false;
        const value = reader.bytesField();
        if (number === 1) body.ticketId = value;
        else if (number === 2) body.clientId = value;
        else if (number === 3) body.extenderPublicKey = value;
        else body.trackId = value;
        return true;
      }
      case 4:
        if (wireType !== BYTES) return false;
        body.networkHost = reader.stringField();
        return true;
      case 5:
      case 6:
      case 13:
      case 14: {
        if (wireType !== VARINT) return false;
        const value = reader.uint64();
        if (number === 5) body.issueTimeMs = value;
        else if (number === 6) body.expireTimeMs = value;
        else if (number === 13) body.maxDownloadBytes = value;
        else body.maxUploadBytes = value;
        return true;
      }
      case 8:
      case 9:
      case 10:
      case 11:
      case 12: {
        if (wireType !== VARINT) return false;
        const value = reader.uint32();
        if (number === 8) body.trackPosition = value;
        else if (number === 9) body.pingIntervalMs = value;
        else if (number === 10) body.downloadMs = value;
        else if (number === 11) body.uploadMs = value;
        else body.maxStreams = value;
        return true;
      }
    }
    return false;
  });
  return body;
}

/**
 * Parses the text form of a ticket (M0 `DecodeLagTicket`): base64url without
 * padding of the serialized LagTicket. As in Go, padding and the standard
 * alphabet are refused and line breaks are ignored. The ticket is not
 * verified.
 */
export function decodeLagTicket(text: string): SignedLagTicket {
  const compact = text.replace(/[\r\n]/g, "");
  if (compact === "" || !/^[A-Za-z0-9_-]*$/.test(compact) || compact.length % 4 === 1) {
    throw new ProtobufError("lag ticket is not unpadded base64url");
  }
  const base64 = compact.replace(/-/g, "+").replace(/_/g, "/");
  const binary = atob(base64 + "=".repeat((4 - (base64.length % 4)) % 4));
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i);
  }
  return mergeSigned(emptySigned(), bytes);
}
