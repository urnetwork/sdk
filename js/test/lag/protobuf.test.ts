import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { registerTsResolve } from "../ts-resolve.ts";

registerTsResolve();
const {
  ProtobufError,
  decodeExtenderFeedFrame,
  decodeExtenderRecord,
  decodeExtenderRecordBody,
  decodeExtenderRevocation,
  decodeExtenderRevocationBody,
  decodeLagTicket,
  decodeLagTicketBody,
  encodeExtenderFeedRequest,
} = await import("../../src/lag/protobuf.ts");

// lag.dev/PLAN.md S3, T3: protobuf decoding of real records, signed and
// marshaled by connect, read as Go's proto.Unmarshal reads them
const vectors = JSON.parse(readFileSync(new URL("./vectors.json", import.meta.url), "utf8"));

const base64 = (text: string) => new Uint8Array(Buffer.from(text, "base64"));
const hex = (bytes: Uint8Array) => Buffer.from(bytes).toString("hex");

function bodyJson(body: ReturnType<typeof decodeExtenderRecordBody>) {
  return {
    publicKeyHex: hex(body.publicKey),
    addresses: body.addresses,
    tcpPort: body.tcpPort,
    udpPort: body.udpPort,
    dnsPort: body.dnsPort,
    dnsTld: body.dnsTld,
    countryCode: body.countryCode,
    issueTimeMs: body.issueTimeMs,
    expireTimeMs: body.expireTimeMs,
    networkHost: body.networkHost,
    dnsPorts: body.dnsPorts,
    continentCode: body.continentCode,
    directoryTier: body.directoryTier,
    webRtcClientIdHex: hex(body.webRtcClientId),
    realityPublicKeyHex: hex(body.realityPublicKey),
    canaryChannel: body.canaryChannel,
    measurementApi: body.measurementApi,
  };
}

test("records and their bodies decode as Go decodes them", () => {
  assert.deepEqual(
    vectors.records.map((vector: { name: string }) => vector.name),
    ["measurement", "canary", "unusual"],
  );
  for (const vector of vectors.records) {
    const record = decodeExtenderRecord(base64(vector.recordBase64));
    assert.equal(Buffer.from(record.body).toString("base64"), vector.bodyBase64, vector.name);
    assert.equal(hex(record.rootSignature), vector.rootSignatureHex, vector.name);
    assert.equal(hex(record.rootKeyId), vector.rootKeyIdHex, vector.name);
    assert.deepEqual(bodyJson(decodeExtenderRecordBody(record.body)), vector.body, vector.name);
  }
});

test("MeasurementApi, field 17, is read", () => {
  const [measurement, canary] = vectors.records;
  assert.equal(decodeExtenderRecordBody(base64(measurement.bodyBase64)).measurementApi, true);
  assert.equal(decodeExtenderRecordBody(base64(canary.bodyBase64)).measurementApi, false);
});

test("an unusual body: unknown fields skipped, ports packed and not, uint32 truncated, wrong wire types unknown", () => {
  const unusual = vectors.records[2];
  const body = decodeExtenderRecordBody(base64(unusual.bodyBase64));
  assert.deepEqual(body.dnsPorts, [53, 4053, 5353, 8053]);
  assert.equal(body.tcpPort, 8443);
  assert.equal(body.measurementApi, true);
  assert.equal(body.countryCode, "");
});

test("the revocation decodes", () => {
  const vector = vectors.revocation;
  const revocation = decodeExtenderRevocation(base64(vector.revocationBase64));
  assert.equal(Buffer.from(revocation.body).toString("base64"), vector.bodyBase64);
  assert.equal(hex(revocation.rootSignature), vector.rootSignatureHex);
  assert.equal(hex(revocation.rootKeyId), vector.rootKeyIdHex);
  const body = decodeExtenderRevocationBody(revocation.body);
  assert.deepEqual(
    { publicKeyHex: hex(body.publicKey), issueTimeMs: body.issueTimeMs, networkHost: body.networkHost },
    { publicKeyHex: vector.publicKeyHex, issueTimeMs: vector.issueTimeMs, networkHost: vector.networkHost },
  );
});

test("feed frames read as connect's feed client reads them, oneof merges included", () => {
  assert.ok(9 <= vectors.feedFrames.length);
  for (const vector of vectors.feedFrames) {
    const frame = decodeExtenderFeedFrame(base64(vector.frameBase64));
    assert.equal(frame.kind, vector.kind, vector.name);
    if (frame.kind === "record") {
      assert.deepEqual(frame.record, decodeExtenderRecord(base64(vector.messageBase64)), vector.name);
    } else if (frame.kind === "revocation") {
      assert.deepEqual(frame.revocation, decodeExtenderRevocation(base64(vector.messageBase64)), vector.name);
    }
  }
});

test("the feed request is written as Go's proto.Marshal writes it", () => {
  for (const vector of vectors.feedRequests) {
    const request = encodeExtenderFeedRequest({ sampleCount: vector.sampleCount, subscribe: vector.subscribe });
    assert.equal(Buffer.from(request).toString("base64"), vector.requestBase64, JSON.stringify(vector));
  }
  for (const sampleCount of [-1, 1.5, 2 ** 32]) {
    assert.throws(() => encodeExtenderFeedRequest({ sampleCount, subscribe: false }), RangeError);
  }
});

test("the amended ticket decodes from its text form", () => {
  const vector = vectors.ticket;
  const ticket = decodeLagTicket(vector.text);
  assert.equal(Buffer.from(ticket.body).toString("base64"), vector.bodyBase64);
  assert.equal(hex(ticket.rootSignature), vector.rootSignatureHex);
  assert.equal(hex(ticket.rootKeyId), vector.rootKeyIdHex);
  const body = decodeLagTicketBody(ticket.body);
  assert.deepEqual(
    {
      ticketIdHex: hex(body.ticketId),
      clientIdHex: hex(body.clientId),
      extenderPublicKeyHex: hex(body.extenderPublicKey),
      networkHost: body.networkHost,
      issueTimeMs: body.issueTimeMs,
      expireTimeMs: body.expireTimeMs,
      trackIdHex: hex(body.trackId),
      trackPosition: body.trackPosition,
      pingIntervalMs: body.pingIntervalMs,
      downloadMs: body.downloadMs,
      uploadMs: body.uploadMs,
      maxStreams: body.maxStreams,
      maxDownloadBytes: body.maxDownloadBytes,
      maxUploadBytes: body.maxUploadBytes,
    },
    vector.body,
  );
  // line breaks are ignored, as Go's base64 ignores them
  const broken = `${vector.text.slice(0, 40)}\r\n${vector.text.slice(40)}\n`;
  assert.deepEqual(decodeLagTicket(broken), ticket);
});

test("the ticket text refuses padding, the standard alphabet and impossible lengths", () => {
  const text: string = vectors.ticket.text;
  for (const refused of ["", `${text}=`, `${text.slice(0, -2)}+/`, `${text}A`.slice(0, text.length - (text.length % 4) + 1), "a b"]) {
    assert.throws(() => decodeLagTicket(refused), ProtobufError, JSON.stringify(refused));
  }
});

test("malformed input is refused as Go refuses it", () => {
  const refusals: Array<[string, number[]]> = [
    ["a truncated varint", [0x08, 0x80]],
    ["a varint past 64 bits", [0x08, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02]],
    ["field number 0", [0x00, 0x00]],
    ["a field number past 2^29", [0x80, 0x80, 0x80, 0x80, 0x10, 0x00]],
    ["wire type 6", [0x0e]],
    ["wire type 7", [0x0f]],
    ["an end group at the top", [0x0c]],
    ["a group never ended", [0x0b, 0x08, 0x01]],
    ["a group ended by another number", [0x0b, 0x14]],
    ["a length past the end", [0x0a, 0x05, 0x01]],
    ["a fixed64 past the end", [0x09, 0x01, 0x02]],
    ["a fixed32 past the end", [0x0d, 0x01]],
    ["a string that is not UTF-8", [0x32, 0x01, 0xff]],
    ["a surrogate in a string", [0x32, 0x03, 0xed, 0xa0, 0x80]],
    ["a truncated packed varint", [0x5a, 0x01, 0x80]],
  ];
  for (const [name, bytes] of refusals) {
    assert.throws(() => decodeExtenderRecordBody(Uint8Array.from(bytes)), ProtobufError, name);
  }
  // a record field holding a malformed record fails the frame
  assert.throws(() => decodeExtenderFeedFrame(Uint8Array.from([0x0a, 0x02, 0x0a])), ProtobufError);
});

test("unknown groups nested past 64 are refused, where Go allows 10000", () => {
  const deep: number[] = [];
  for (let i = 0; i < 100; i += 1) deep.push(0xa3, 0x06); // start group 100
  for (let i = 0; i < 100; i += 1) deep.push(0xa4, 0x06); // end group 100
  assert.throws(() => decodeExtenderRecordBody(Uint8Array.from(deep)), ProtobufError);
});

test("values at the edges: a BOM is kept, uint64 is exact to 2^53, unknown groups nest", () => {
  assert.equal(decodeExtenderRecordBody(Uint8Array.from([0x32, 0x03, 0xef, 0xbb, 0xbf])).dnsTld, "﻿");
  // ExpireTimeMs (9) = 2^53 - 1, then IssueTimeMs (8) = 2^64 - 1
  const body = decodeExtenderRecordBody(
    Uint8Array.from([
      0x48, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x0f, 0x40, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
      0x01,
    ]),
  );
  assert.equal(body.expireTimeMs, Number.MAX_SAFE_INTEGER);
  assert.equal(body.issueTimeMs, 2 ** 64);
  const nested: number[] = [];
  for (let i = 0; i < 10; i += 1) nested.push(0xa3, 0x06);
  for (let i = 0; i < 10; i += 1) nested.push(0xa4, 0x06);
  assert.equal(decodeExtenderRecordBody(Uint8Array.from([...nested, 0x18, 0x07])).tcpPort, 7);
  assert.deepEqual(decodeExtenderFeedFrame(new Uint8Array(0)), { kind: "empty" });
});
