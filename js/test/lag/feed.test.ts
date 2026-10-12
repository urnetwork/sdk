import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { createServer, type IncomingMessage } from "node:http";
import type { AddressInfo, Socket } from "node:net";
import { registerTsResolve } from "../ts-resolve.ts";

registerTsResolve();
const {
  EXTENDER_RECORD_SIGNATURE_DOMAIN,
  EXTENDER_REVOCATION_SIGNATURE_DOMAIN,
  LAG_TICKET_SIGNATURE_DOMAIN,
  ExtenderDirectory,
  ExtenderFeedClient,
  ExtenderFeedError,
  ExtenderRootKeys,
  verifyEd25519,
} = await import("../../src/lag/feed.ts");
const { decodeExtenderRecord, decodeExtenderRevocation, decodeLagTicket } = await import("../../src/lag/protobuf.ts");
const { tweetnaclVerifyDetached } = await import("../../src/lag/tweetnacl_verify.ts");

// lag.dev/PLAN.md S3, M9: the feed, verified as connect verifies it, into a
// directory that exposes MeasurementApi
const vectors = JSON.parse(readFileSync(new URL("./vectors.json", import.meta.url), "utf8"));

const base64 = (text: string) => new Uint8Array(Buffer.from(text, "base64"));
const fromHex = (text: string) => new Uint8Array(Buffer.from(text, "hex"));
const concat = (a: Uint8Array, b: Uint8Array) => {
  const out = new Uint8Array(a.length + b.length);
  out.set(a, 0);
  out.set(b, a.length);
  return out;
};

const rootPublicKey = fromHex(vectors.rootPublicKeyHex);
const otherPublicKey = fromHex(vectors.otherPublicKeyHex);
const [measurementVector, canaryVector, unusualVector] = vectors.records;
const measurementRecord = decodeExtenderRecord(base64(measurementVector.recordBase64));
const canaryRecord = decodeExtenderRecord(base64(canaryVector.recordBase64));
const unusualRecord = decodeExtenderRecord(base64(unusualVector.recordBase64));
const revocation = decodeExtenderRevocation(base64(vectors.revocation.revocationBase64));
const extenderKey: string = measurementVector.body.publicKeyHex;

// inside every vector record's lifetime: an hour after issue
const now = measurementVector.body.issueTimeMs + 60 * 60 * 1000;
const expireTimeMs: number = measurementVector.body.expireTimeMs;

const frames = Object.fromEntries(
  vectors.feedFrames.map((vector: { name: string; frameBase64: string }) => [vector.name, base64(vector.frameBase64)]),
) as Record<string, Uint8Array>;

function directory(options: Partial<ConstructorParameters<typeof ExtenderDirectory>[0]> = {}) {
  return new ExtenderDirectory({
    rootPublicKeys: [vectors.rootPublicKeyHex],
    networkHosts: ["bringyour.com", "ur.network"],
    now: () => now,
    ...options,
  });
}

function tampered(bytes: Uint8Array, at = 0) {
  const copy = bytes.slice();
  copy[at] ^= 1;
  return copy;
}

for (const fallback of [false, true]) {
  const path = fallback ? "the vendored TweetNaCl" : "WebCrypto";

  test(`Ed25519 with ${path}: the root signature of every record vector verifies`, async () => {
    for (const vector of vectors.records) {
      const record = decodeExtenderRecord(base64(vector.recordBase64));
      const signed = concat(new TextEncoder().encode(EXTENDER_RECORD_SIGNATURE_DOMAIN), record.body);
      assert.equal(await verifyEd25519(rootPublicKey, signed, record.rootSignature, { fallback }), true, vector.name);
      assert.equal(await verifyEd25519(rootPublicKey, tampered(signed, signed.length - 1), record.rootSignature, { fallback }), false);
      assert.equal(await verifyEd25519(rootPublicKey, signed, tampered(record.rootSignature, 5), { fallback }), false);
      assert.equal(await verifyEd25519(otherPublicKey, signed, record.rootSignature, { fallback }), false);
    }
  });

  test(`Ed25519 with ${path}: wrong lengths and a non-canonical S do not verify`, async () => {
    const signed = concat(new TextEncoder().encode(EXTENDER_RECORD_SIGNATURE_DOMAIN), measurementRecord.body);
    const signature = measurementRecord.rootSignature;
    assert.equal(await verifyEd25519(rootPublicKey.subarray(1), signed, signature, { fallback }), false);
    assert.equal(await verifyEd25519(rootPublicKey, signed, signature.subarray(1), { fallback }), false);
    // S + L is the same scalar, which TweetNaCl alone would take and Go refuses
    const order = fromHex("edd3f55c1a631258d69cf7a2def9de1400000000000000000000000000000010");
    const malleated = signature.slice();
    let carry = 0;
    for (let i = 0; i < 32; i += 1) {
      const sum = malleated[32 + i] + order[i] + carry;
      malleated[32 + i] = sum & 0xff;
      carry = sum >> 8;
    }
    assert.equal(carry, 0);
    assert.equal(tweetnaclVerifyDetached(signed, malleated, rootPublicKey), true);
    assert.equal(await verifyEd25519(rootPublicKey, signed, malleated, { fallback }), false);
  });

  test(`directory with ${path}: a record is taken, with its MeasurementApi flag and hosts`, async () => {
    const extenders = directory({ verifyFallback: fallback });
    const result = await extenders.applyRecord(measurementRecord);
    assert.equal(result.status, "applied");
    const [entry] = extenders.entries();
    assert.equal(entry.publicKey, extenderKey);
    assert.equal(entry.measurementApi, true);
    assert.deepEqual(entry.ips, ["203.0.113.7", "2001:db8::7"]);
    assert.deepEqual(entry.measurementHosts, [
      "203-0-113-7.extender.bringyour.com",
      "2001-db8--7.extender.bringyour.com",
    ]);
    assert.equal(entry.body.countryCode, "de");
    assert.equal(extenders.get(extenderKey.toUpperCase()), entry);
    assert.deepEqual(extenders.measurementEntries(), [entry]);
    assert.equal((await extenders.applyRecord({ ...measurementRecord, body: tampered(measurementRecord.body, 3) })).status, "refused");
  });
}

test("root keys: domains are separate, and keys are read as connect reads them", async () => {
  const keys = new ExtenderRootKeys(["", ` ${vectors.rootPublicKeyHex}\n`]);
  assert.equal(keys.publicKeys.length, 1);
  const ticket = decodeLagTicket(vectors.ticket.text);
  assert.equal(await keys.verify(LAG_TICKET_SIGNATURE_DOMAIN, ticket.body, ticket.rootSignature), true);
  assert.equal(await keys.verify(EXTENDER_RECORD_SIGNATURE_DOMAIN, ticket.body, ticket.rootSignature), false);
  assert.equal(await keys.verify(LAG_TICKET_SIGNATURE_DOMAIN, measurementRecord.body, measurementRecord.rootSignature), false);
  assert.equal(await keys.verify(EXTENDER_REVOCATION_SIGNATURE_DOMAIN, revocation.body, revocation.rootSignature), true);
  assert.equal(await new ExtenderRootKeys([vectors.otherPublicKeyHex]).verify(LAG_TICKET_SIGNATURE_DOMAIN, ticket.body, ticket.rootSignature), false);
  assert.throws(() => new ExtenderRootKeys(["not hex"]), TypeError);
  assert.throws(() => new ExtenderRootKeys(["abcd"]), TypeError);
  assert.equal(new ExtenderRootKeys([new Uint8Array(31)]).publicKeys.length, 0);
});

test("directory: network hosts are matched as connect matches them", async () => {
  const urNetwork = directory({ networkHosts: [" UR.Network. "] });
  const refused = await urNetwork.applyRecord(measurementRecord);
  assert.deepEqual(refused.status === "refused" && refused.reason, "networkHost");
  assert.equal((await urNetwork.applyRecord(canaryRecord)).status, "applied");
  assert.equal(urNetwork.entries()[0].body.canaryChannel, "canary-a");
  assert.equal(urNetwork.entries()[0].measurementApi, false);
  assert.deepEqual(urNetwork.entries()[0].measurementHosts, []);
  assert.equal((await directory({ networkHosts: [] }).applyRecord(canaryRecord)).status, "refused");
});

test("directory: a record expires five minutes after its ExpireTimeMs", async () => {
  let clock = expireTimeMs + 5 * 60 * 1000;
  const extenders = directory({ now: () => clock });
  assert.equal((await extenders.applyRecord(measurementRecord)).status, "applied");
  assert.equal(extenders.entries().length, 1);
  clock += 1;
  assert.deepEqual(extenders.entries(), []);
  assert.equal(extenders.get(extenderKey), undefined);
  const late = await directory({ now: () => clock }).applyRecord(measurementRecord);
  assert.deepEqual(late.status === "refused" && late.reason, "expired");
});

test("directory: the newest record of a key wins", async () => {
  const extenders = directory();
  assert.equal((await extenders.applyRecord(measurementRecord)).status, "applied");
  // the unusual record is the same extender, issued a millisecond later
  assert.equal((await extenders.applyRecord(unusualRecord)).status, "applied");
  assert.deepEqual(extenders.entries().map((entry) => entry.ips), [["198.51.100.20"]]);
  assert.equal((await extenders.applyRecord(measurementRecord)).status, "unchanged");
  assert.equal((await extenders.applyRecord(unusualRecord)).status, "unchanged");
  assert.deepEqual(extenders.entries().map((entry) => entry.ips), [["198.51.100.20"]]);
});

test("directory: a revocation revokes the records issued at or before it, even those that come later", async () => {
  const extenders = directory();
  await extenders.applyRecord(measurementRecord);
  await extenders.applyRecord(canaryRecord);
  assert.equal((await extenders.applyRevocation(revocation)).status, "applied");
  assert.deepEqual(extenders.entries().map((entry) => entry.body.canaryChannel), ["canary-a"]);
  assert.equal(extenders.get(extenderKey), undefined);
  assert.equal((await extenders.applyRevocation(revocation)).status, "unchanged");

  const revokedFirst = directory();
  assert.equal((await revokedFirst.applyRevocation(revocation)).status, "applied");
  const late = await revokedFirst.applyRecord(unusualRecord);
  assert.deepEqual(late, { status: "applied", publicKey: extenderKey, entry: null });
  assert.deepEqual(revokedFirst.entries(), []);
  const forged = await revokedFirst.applyRevocation({ ...revocation, rootSignature: tampered(revocation.rootSignature) });
  assert.deepEqual(forged.status === "refused" && forged.reason, "signature");
});

test("directory: the earliest key goes past maxKeys", async () => {
  const extenders = directory({ maxKeys: 1 });
  await extenders.applyRecord(measurementRecord);
  await extenders.applyRecord(canaryRecord);
  assert.deepEqual(extenders.entries().map((entry) => entry.body.canaryChannel), ["canary-a"]);
});

// A WebSocket the test drives from the server's side.
class FakeWebSocket {
  static last: FakeWebSocket | null = null;
  url: string;
  binaryType = "blob";
  readyState = 0;
  onopen: ((event: unknown) => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onerror: ((event: unknown) => void) | null = null;
  onclose: ((event: { code: number; reason: string }) => void) | null = null;
  sent: Uint8Array[] = [];
  closedWith: number | null = null;

  constructor(url: string) {
    this.url = url;
    FakeWebSocket.last = this;
  }

  send(data: Uint8Array) {
    this.sent.push(new Uint8Array(data));
  }

  close(code?: number) {
    this.closedWith = code ?? 1005;
    this.readyState = 3;
  }

  // the server's side
  open() {
    this.readyState = 1;
    this.onopen?.({});
  }

  frame(bytes: Uint8Array | string) {
    this.onmessage?.({ data: typeof bytes === "string" ? bytes : bytes.slice().buffer });
  }

  serverClose(code = 1000) {
    this.readyState = 3;
    this.onclose?.({ code, reason: "" });
  }
}

const fake = () => {
  const socket = FakeWebSocket.last;
  assert.ok(socket);
  return socket;
};

function feedClient(options: Record<string, unknown> = {}) {
  const events: Array<{ type: string } & Record<string, unknown>> = [];
  const client = new ExtenderFeedClient({
    host: "203-0-113-7.extender.bringyour.com",
    rootPublicKeys: [vectors.rootPublicKeyHex],
    networkHosts: ["bringyour.com", "ur.network"],
    now: () => now,
    WebSocket: FakeWebSocket as unknown as typeof WebSocket,
    onEvent: (event) => events.push(event as { type: string }),
    ...options,
  });
  return { client, events };
}

const until = async (condition: () => boolean) => {
  for (let i = 0; i < 1000 && !condition(); i += 1) {
    await new Promise((resolve) => setTimeout(resolve, 1));
  }
  assert.ok(condition());
};

test("feed: a sample asks as connect asks, applies in order, and closes at its end", async () => {
  const { client, events } = feedClient();
  assert.equal(client.url, "wss://203-0-113-7.extender.bringyour.com/ur/feed/v1");
  const sampled = client.sample(8);
  const socket = fake();
  assert.equal(socket.url, client.url);
  assert.equal(socket.binaryType, "arraybuffer");
  socket.open();
  const request = vectors.feedRequests.find((vector: { sampleCount: number; subscribe: boolean }) => vector.sampleCount === 8 && !vector.subscribe);
  assert.deepEqual(socket.sent, [base64(request.requestBase64)]);
  socket.frame(frames.record);
  socket.frame(frames.keepalive);
  socket.frame(frames["keepalive then record"]);
  socket.frame(frames.endOfSample);
  // the server ends a sample without a subscription at once
  socket.serverClose();
  const entries = await sampled;
  assert.deepEqual(entries.map((entry) => [entry.measurementApi, entry.body.countryCode]), [[true, "de"], [false, "jp"]]);
  assert.equal(socket.closedWith, 1000);
  assert.equal(client.open, false);
  assert.deepEqual(events.map((event) => event.type), ["open", "record", "record", "endOfSample", "closed"]);
  assert.equal(events.at(-1)?.error, undefined);
});

test("feed: a subscription applies what follows the sample until the server ends it", async () => {
  const { client, events } = feedClient();
  const sampled = client.sample(8, { subscribe: true });
  const socket = fake();
  socket.open();
  const request = vectors.feedRequests.find((vector: { sampleCount: number; subscribe: boolean }) => vector.sampleCount === 8 && vector.subscribe);
  assert.deepEqual(socket.sent, [base64(request.requestBase64)]);
  socket.frame(frames.record);
  socket.frame(frames.endOfSample);
  assert.equal((await sampled).length, 1);
  assert.equal(client.open, true);
  socket.frame(frames.revocation);
  socket.frame(frames.keepalive);
  socket.frame(frames["keepalive then record"]);
  await until(() => events.filter((event) => event.type === "record").length === 2);
  assert.deepEqual(client.directory.entries().map((entry) => entry.body.countryCode), ["jp"]);
  socket.serverClose();
  await until(() => events.at(-1)?.type === "closed");
  assert.equal(events.at(-1)?.error, undefined);
  assert.equal(client.open, false);
  assert.deepEqual(events.map((event) => event.type), ["open", "record", "endOfSample", "revocation", "record", "closed"]);
});

test("feed: refused records and frames are reported and the stream goes on", async () => {
  const { client, events } = feedClient({ networkHosts: ["ur.network"] });
  const sampled = client.sample(8);
  const socket = fake();
  socket.open();
  socket.frame(frames.record);
  socket.frame(Uint8Array.from([0x0a, 0x02, 0x0a]));
  socket.frame(frames["keepalive then record"]);
  socket.frame(frames.endOfSample);
  assert.deepEqual((await sampled).map((entry) => entry.body.canaryChannel), ["canary-a"]);
  assert.deepEqual(
    events.filter((event) => event.type === "refused").map((event) => event.reason),
    ["networkHost", "frame"],
  );
});

test("feed: a sample that does not finish rejects", async () => {
  const cases: Array<[string, (socket: FakeWebSocket, client: InstanceType<typeof ExtenderFeedClient>) => void, RegExp]> = [
    ["a close before the end", (socket) => { socket.frame(frames.record); socket.serverClose(1006); }, /closed before the sample \(1006\)/],
    ["a connection error", (socket) => { socket.onerror?.({}); socket.serverClose(1006); }, /connection failed/],
    ["a text message", (socket) => socket.frame("hello"), /not binary/],
    ["a frame over 64 KiB", (socket) => socket.frame(new Uint8Array(64 * 1024 + 1)), /at most 65536/],
    ["the client closing", (_socket, client) => client.close(), /closed/],
  ];
  for (const [name, act, message] of cases) {
    const { client, events } = feedClient();
    const sampled = client.sample();
    const socket = fake();
    socket.open();
    act(socket, client);
    await assert.rejects(sampled, (error: Error) => error instanceof ExtenderFeedError && message.test(error.message), name);
    assert.equal(client.open, false, name);
    assert.equal(events.at(-1)?.type, "closed", name);
  }
});

test("feed: the sample timeout, the idle timeout and an abort end the stream", async () => {
  const slow = feedClient({ sampleTimeoutMs: 20 });
  const sampled = slow.client.sample();
  fake().open();
  await assert.rejects(sampled, /the sample timed out/);

  const quiet = feedClient({ idleTimeoutMs: 20 });
  const subscribed = quiet.client.sample(8, { subscribe: true });
  const socket = fake();
  socket.open();
  socket.frame(frames.endOfSample);
  assert.deepEqual(await subscribed, []);
  await until(() => quiet.events.at(-1)?.type === "closed");
  assert.match(String(quiet.events.at(-1)?.error), /went silent/);

  const controller = new AbortController();
  const aborted = feedClient().client.sample(8, { signal: controller.signal });
  fake().open();
  controller.abort();
  await assert.rejects(aborted, /aborted/);
  const abortedEarly = new AbortController();
  abortedEarly.abort();
  await assert.rejects(feedClient().client.sample(8, { signal: abortedEarly.signal }), /aborted/);
});

test("feed: one stream at a time, and a bad count is refused", async () => {
  const { client } = feedClient();
  const sampled = client.sample();
  await assert.rejects(client.sample(), /already open/);
  client.close();
  await assert.rejects(sampled, /closed/);
  await assert.rejects(client.sample(-1), RangeError);
});

// A minimal RFC 6455 server, so the client runs against a real WebSocket:
// the runtime's own, which delivers binary messages as a Blob unless asked
// for an ArrayBuffer.
function websocketServer(onClient: (socket: Socket, received: Promise<Buffer>) => void) {
  const server = createServer();
  server.on("upgrade", (request: IncomingMessage, socket: Socket) => {
    const key = request.headers["sec-websocket-key"];
    const accept = createHash("sha1").update(`${key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest("base64");
    socket.write(
      `HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`,
    );
    let buffered = Buffer.alloc(0);
    // the first client message, unmasked
    const received = new Promise<Buffer>((resolve) => {
      socket.on("data", (chunk: Buffer) => {
        buffered = Buffer.concat([buffered, chunk]);
        if (buffered.length < 2) return;
        let length = buffered[1] & 0x7f;
        let offset = 2;
        if (length === 126) {
          length = buffered.readUInt16BE(2);
          offset = 4;
        }
        if (buffered.length < offset + 4 + length) return;
        const mask = buffered.subarray(offset, offset + 4);
        const payload = Buffer.from(buffered.subarray(offset + 4, offset + 4 + length));
        for (let i = 0; i < payload.length; i += 1) payload[i] ^= mask[i % 4];
        resolve(payload);
      });
    });
    onClient(socket, received);
  });
  return server;
}

function binaryMessage(payload: Uint8Array): Buffer {
  const header = payload.length < 126
    ? Buffer.from([0x82, payload.length])
    : Buffer.from([0x82, 126, payload.length >> 8, payload.length & 0xff]);
  return Buffer.concat([header, Buffer.from(payload)]);
}

test("feed: a real WebSocket, the runtime's own", { skip: typeof WebSocket === "undefined" && "no WebSocket in this runtime" }, async () => {
  let request: Buffer | null = null;
  const server = websocketServer(async (socket, received) => {
    request = await received;
    socket.write(binaryMessage(frames.record));
    socket.write(binaryMessage(frames.revocation));
    socket.write(binaryMessage(frames["keepalive then record"]));
    socket.write(binaryMessage(frames.endOfSample));
    // a close frame, as the server ends a sample without a subscription
    socket.end(Buffer.from([0x88, 0x02, 0x03, 0xe8]));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    const { port } = server.address() as AddressInfo;
    const client = new ExtenderFeedClient({
      host: "ignored",
      url: `ws://127.0.0.1:${port}/ur/feed/v1`,
      rootPublicKeys: [vectors.rootPublicKeyHex],
      networkHosts: ["bringyour.com", "ur.network"],
      now: () => now,
    });
    const entries = await client.sample(8);
    const expected = vectors.feedRequests.find((vector: { sampleCount: number; subscribe: boolean }) => vector.sampleCount === 8 && !vector.subscribe);
    assert.equal(request && (request as Buffer).toString("base64"), expected.requestBase64);
    // the measurement extender is revoked; the canary remains
    assert.deepEqual(entries.map((entry) => entry.body.countryCode), ["jp"]);
  } finally {
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
  }
});
