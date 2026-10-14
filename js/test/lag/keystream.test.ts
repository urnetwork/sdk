import { test } from "node:test";
import assert from "node:assert/strict";
import { createCipheriv, randomBytes } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import {
  importLagUploadKey,
  lagCounterBlock,
  lagKeystream,
  LagKeystream,
  LAG_UPLOAD_MESSAGE_BYTES,
} from "../../src/lag/keystream.ts";

// The reference: AES-128-CTR as OpenSSL (and Go's cipher.NewCTR) runs it, a
// full 128-bit big-endian counter from the M6 counter block.
const reference = (key: Uint8Array, stream: number, offset: number, length: number): Uint8Array => {
  const cipher = createCipheriv("aes-128-ctr", key, lagCounterBlock(stream, offset));
  return new Uint8Array(cipher.update(Buffer.alloc(length)));
};

const hex = (bytes: Uint8Array) => Buffer.from(bytes).toString("hex");

const base64url = (bytes: Uint8Array) => Buffer.from(bytes).toString("base64url");

test("counter block: stream, then offset/16, each a big-endian uint64", () => {
  assert.equal(hex(lagCounterBlock(0, 0)), "00".repeat(16));
  assert.equal(hex(lagCounterBlock(3, 65536)), "0000000000000003" + "0000000000001000");
  assert.equal(
    hex(lagCounterBlock(2 ** 40 + 5, 16 * (2 ** 40 + 7))),
    "0000010000000005" + "0000010000000007",
  );
  assert.equal(
    hex(lagCounterBlock(Number.MAX_SAFE_INTEGER, 0)),
    "001fffffffffffff" + "0000000000000000",
  );
  assert.throws(() => lagCounterBlock(0, 8), RangeError);
  assert.throws(() => lagCounterBlock(-1, 0), RangeError);
  assert.throws(() => lagCounterBlock(1.5, 0), RangeError);
  assert.throws(() => lagCounterBlock(0, -16), RangeError);
});

test("keystream equals AES-128-CTR from the counter block", async () => {
  for (let trial = 0; trial < 8; trial += 1) {
    const raw = new Uint8Array(randomBytes(16));
    const key = await importLagUploadKey(raw);
    for (const [stream, offset, length] of [
      [0, 0, 16],
      [trial % 4, 0, LAG_UPLOAD_MESSAGE_BYTES],
      [1, 16, 1],
      [2, 65536, 15],
      [3, 65536 * 17, 17],
      [0, (1 << 20) + 48, 100_000],
      [2 ** 33 + trial, 16 * 2 ** 35, 4096],
    ] as const) {
      assert.equal(
        hex(await lagKeystream(key, stream, offset, length)),
        hex(reference(raw, stream, offset, length)),
        `stream ${stream} offset ${offset} length ${length}`,
      );
    }
  }
});

test("keystream: a later offset continues the same stream", async () => {
  const raw = new Uint8Array(randomBytes(16));
  const key = await importLagUploadKey(raw);
  const whole = await lagKeystream(key, 1, 0, 3 * 65536);
  const second = await lagKeystream(key, 1, 65536, 65536);
  assert.equal(hex(second), hex(whole.subarray(65536, 2 * 65536)));
  assert.equal((await lagKeystream(key, 1, 0, 0)).length, 0);
  // streams differ from each other
  const other = await lagKeystream(key, 2, 0, 64);
  assert.notEqual(hex(other), hex(whole.subarray(0, 64)));
});

test("upload key: base64url text and bytes import the same key; a wrong size is refused", async () => {
  const raw = new Uint8Array(randomBytes(16));
  const fromBytes = await importLagUploadKey(raw);
  const fromText = await importLagUploadKey(base64url(raw));
  assert.equal(
    hex(await lagKeystream(fromText, 0, 0, 64)),
    hex(await lagKeystream(fromBytes, 0, 0, 64)),
  );
  assert.equal(
    hex(await lagKeystream(await importLagUploadKey(base64url(raw) + "=="), 0, 0, 64)),
    hex(await lagKeystream(fromBytes, 0, 0, 64)),
  );
  await assert.rejects(importLagUploadKey(new Uint8Array(15)), RangeError);
  await assert.rejects(importLagUploadKey(base64url(new Uint8Array(32))), RangeError);
  await assert.rejects(importLagUploadKey("not base64url!"), TypeError);
});

test("ring: sequential reads of any size are the stream from offset 0", async () => {
  const raw = new Uint8Array(randomBytes(16));
  const key = await importLagUploadKey(raw);
  const ring = new LagKeystream(key, 3, { chunkBytes: 4096, depth: 3 });
  const sizes = [100, 4000, 0, 65536, 16, 4096, 1, 9000];
  const reads = [];
  for (const size of sizes) {
    reads.push(await ring.next(size));
  }
  const total = sizes.reduce((a, b) => a + b, 0);
  assert.equal(ring.offset, total);
  assert.equal(hex(Buffer.concat(reads)), hex(reference(raw, 3, 0, total)));
});

test("ring: default upload messages across 1 MiB chunks, without copies inside a chunk", async () => {
  const raw = new Uint8Array(randomBytes(16));
  const key = await importLagUploadKey(raw);
  const ring = new LagKeystream(key, 0);
  const messages = [];
  for (let i = 0; i < 40; i += 1) {
    messages.push(await ring.next());
  }
  assert.ok(messages.every((message) => message.length === LAG_UPLOAD_MESSAGE_BYTES));
  assert.equal(hex(Buffer.concat(messages)), hex(reference(raw, 0, 0, 40 * LAG_UPLOAD_MESSAGE_BYTES)));
  // messages 0-15 are views of the first chunk
  assert.equal(messages[1].buffer, messages[0].buffer);
  assert.notEqual(messages[16].buffer, messages[15].buffer);
});

test("ring: overlapping calls are served in order", async () => {
  const raw = new Uint8Array(randomBytes(16));
  const key = await importLagUploadKey(raw);
  const ring = new LagKeystream(key, 1, { chunkBytes: 1024, depth: 2 });
  const reads = await Promise.all([ring.next(700), ring.next(700), ring.next(700), ring.next(10)]);
  assert.equal(hex(Buffer.concat(reads)), hex(reference(raw, 1, 0, 2110)));
  await assert.rejects(ring.next(-1), RangeError);
  assert.equal(ring.offset, 2110);
});

test("ring: a failed chunk fails the stream from there on", async () => {
  const raw = new Uint8Array(randomBytes(16));
  const key = await importLagUploadKey(raw);
  let calls = 0;
  const subtle = {
    encrypt: (...args: Parameters<SubtleCrypto["encrypt"]>) => {
      calls += 1;
      return calls === 2 ? Promise.reject(new Error("no crypto")) : crypto.subtle.encrypt(...args);
    },
  } as SubtleCrypto;
  const ring = new LagKeystream(key, 0, { chunkBytes: 64, depth: 2, subtle });
  assert.equal(hex(await ring.next(64)), hex(reference(raw, 0, 0, 64)));
  await assert.rejects(ring.next(64), /no crypto/);
  await assert.rejects(ring.next(64), /no crypto/);
});

test("ring: one WebCrypto call in flight at a time, filling and refilling", async () => {
  const raw = new Uint8Array(randomBytes(16));
  const key = await importLagUploadKey(raw);
  let inFlight = 0;
  let most = 0;
  let calls = 0;
  const subtle = {
    encrypt: async (...args: Parameters<SubtleCrypto["encrypt"]>) => {
      calls += 1;
      inFlight += 1;
      most = Math.max(most, inFlight);
      try {
        await new Promise((resolve) => setTimeout(resolve, 2));
        return await crypto.subtle.encrypt(...args);
      } finally {
        inFlight -= 1;
      }
    },
  } as SubtleCrypto;
  const ring = new LagKeystream(key, 5, { chunkBytes: 1024, depth: 4, subtle });
  const reads = [];
  for (let i = 0; i < 12; i += 1) {
    reads.push(await ring.next(512));
  }
  // the ring filled, then refilled as chunks were read, one call at a time
  assert.equal(most, 1);
  assert.ok(6 <= calls);
  assert.equal(hex(Buffer.concat(reads)), hex(reference(raw, 5, 0, 12 * 512)));
});

test("ring: primed waits for the next chunk without taking it", async () => {
  const raw = new Uint8Array(randomBytes(16));
  const key = await importLagUploadKey(raw);
  let release!: () => void;
  const held = new Promise<void>((resolve) => {
    release = resolve;
  });
  const subtle = {
    encrypt: async (...args: Parameters<SubtleCrypto["encrypt"]>) => {
      await held;
      return crypto.subtle.encrypt(...args);
    },
  } as SubtleCrypto;
  const ring = new LagKeystream(key, 2, { chunkBytes: 1024, depth: 2, subtle });
  let primed = false;
  const priming = ring.primed().then(() => {
    primed = true;
  });
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(primed, false, "primed before WebCrypto answered");
  release();
  await priming;
  // the chunk is still the stream's first
  assert.equal(ring.offset, 0);
  assert.equal(hex(await ring.next(1024)), hex(reference(raw, 2, 0, 1024)));
  // a stream part way through a chunk is primed already
  await ring.next(10);
  await ring.primed();
  assert.equal(hex(await ring.next(10)), hex(reference(raw, 2, 0, 1044).subarray(1034)));
});

// The S1 vectors from Go (gen_lag.go): key, stream and offset give the
// counter block and the keystream cipher.NewCTR produced.
const vectorsPath = new URL("./vectors.json", import.meta.url);
test("keystream matches the Go vectors", { skip: !existsSync(vectorsPath) && "test/lag/vectors.json not generated yet" }, async () => {
  const vectors = JSON.parse(readFileSync(vectorsPath, "utf8"));
  const samples = vectors.keystream as Array<{
    key: string;
    uploadKey: string;
    stream: number;
    offset: number;
    counter: string;
    bytes: string;
  }>;
  assert.ok(Array.isArray(samples) && 0 < samples.length, "vectors.json has keystream samples");
  for (const sample of samples) {
    const what = `stream ${sample.stream} offset ${sample.offset}`;
    assert.equal(hex(lagCounterBlock(sample.stream, sample.offset)), sample.counter, what);
    const raw = new Uint8Array(Buffer.from(sample.key, "hex"));
    assert.equal(base64url(raw), sample.uploadKey, what);
    const length = sample.bytes.length / 2;
    for (const key of [await importLagUploadKey(raw), await importLagUploadKey(sample.uploadKey)]) {
      assert.equal(hex(await lagKeystream(key, sample.stream, sample.offset, length)), sample.bytes, what);
    }
    // the ring reaches the same bytes reading from offset 0
    if (sample.offset + length <= 1 << 21) {
      const ring = new LagKeystream(await importLagUploadKey(raw), sample.stream, { chunkBytes: 4096 });
      if (0 < sample.offset) {
        await ring.next(sample.offset);
      }
      assert.equal(hex(await ring.next(length)), sample.bytes, `${what}, through the ring`);
    }
  }
});
