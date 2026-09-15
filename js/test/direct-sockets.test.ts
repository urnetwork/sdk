import { test } from "node:test";
import assert from "node:assert/strict";
import { attachSocketAPI, createDirectSockets } from "../src/socket.ts";
import type { SocketBridge, SocketDevice } from "../src/socket.ts";

const deferred = <T>() => {
  let resolve!: (value: T) => void, reject!: (reason: unknown) => void;
  const promise = new Promise<T>((a, b) => { resolve = a; reject = b; });
  return { promise, resolve, reject };
};
const tick = () => new Promise(resolve => setImmediate(resolve));
type Read = { data: Uint8Array; eof: boolean; error?: string };
function fixture(reads: Read[] = []) {
  const closed = deferred<void>();
  const calls: [string, number, any][] = [];
  const pendingReads: ReturnType<typeof deferred<Read>>[] = [];
  let remoteAddr = "[2001:db8::1]:7";
  let override: SocketBridge["socketOperation"] | undefined;
  const bridge: SocketBridge = { socketOperation: async (op, id, arg) => {
    calls.push([op, id, arg]);
    if (override) return override(op, id, arg);
    switch (op) {
      case "dial": return { id: 1, localAddr: "[fd00::2]:50000", remoteAddr };
      case "socketClosed": return closed.promise;
      case "read": {
        const read = reads.shift();
        if (read) {
          if (read.data.length > arg) {
            reads.unshift({ ...read, data: read.data.slice(arg) });
            return { data: read.data.slice(0, arg), eof: false };
          }
          return read;
        }
        const pending = deferred<Read>(); pendingReads.push(pending); return pending.promise;
      }
      case "write": return arg.length;
      case "addresses": return { localAddr: "[fd00::2]:50000", remoteAddr };
      case "closeRead": case "readDeadline":
        for (const read of pendingReads.splice(0)) read.reject(new Error("read stopped"));
        return;
      case "release": closed.resolve(); return;
    }
  } };
  const device = attachSocketAPI(bridge);
  return { device, calls, closed, pendingReads, setRemote: (s: string) => { remoteAddr = s; }, override: (f: SocketBridge["socketOperation"]) => { override = f; } };
}
const dom = (name: string) => (error: any) => error instanceof DOMException && error.name === name;

test("Device supplies standard constructor signatures without changing browser globals", async () => {
  const nativeTCP = (globalThis as any).TCPSocket, nativeUDP = (globalThis as any).UDPSocket;
  const f = fixture();
  assert.equal("webTransport" in f.device, false);
  assert.equal(Object.isFrozen(f.device.directSockets), true);
  const { TCPSocket } = createDirectSockets(f.device);
  const socket = new TCPSocket("socket.test", 7);
  assert.ok(socket instanceof TCPSocket);
  const opened = await socket.opened;
  assert.deepEqual(f.calls[0][2], { network: "tcp", address: "socket.test:7", tls: undefined });
  assert.equal(opened.remoteAddress, "2001:db8::1");
  assert.equal(opened.remotePort, 7);
  assert.equal(opened.localAddress, "fd00::2");
  assert.equal(opened.localPort, 50000);
  await socket.close(); await socket.closed;
  assert.equal((globalThis as any).TCPSocket, nativeTCP);
  assert.equal((globalThis as any).UDPSocket, nativeUDP);
});

test("DNS family options and IPv6 destinations reach the Device unchanged", async () => {
  for (const [dnsQueryType, suffix] of [[undefined, ""], ["ipv4", "4"], ["ipv6", "6"]] as const) {
    const f = fixture();
    const tcp = new f.device.directSockets.TCPSocket("socket.test", 7, { dnsQueryType });
    await tcp.opened; assert.equal(f.calls[0][2].network, "tcp" + suffix); await tcp.close();
    const g = fixture();
    const udp = new g.device.directSockets.UDPSocket({ remoteAddress: "2001:db8::1", remotePort: 7, dnsQueryType });
    await udp.opened;
    assert.equal(g.calls[0][2].network, "udp" + suffix);
    assert.equal(g.calls[0][2].address, "[2001:db8::1]:7");
    await udp.close();
  }
});

test("TCP BYOB reads preserve a split payload and finish the pending BYOB read at EOF", async () => {
  const f = fixture([{ data: Uint8Array.of(1, 2, 3, 4, 5), eof: true }]);
  const socket = new f.device.directSockets.TCPSocket("socket.test", 7);
  const { readable, writable } = await socket.opened;
  assert.equal(f.calls.filter(c => c[0] === "read").length, 0);
  const reader = readable.getReader({ mode: "byob" });
  assert.deepEqual((await reader.read(new Uint8Array(3))).value, Uint8Array.of(1, 2, 3));
  assert.deepEqual((await reader.read(new Uint8Array(3))).value, Uint8Array.of(4, 5));
  assert.equal((await reader.read(new Uint8Array(3))).done, true);
  reader.releaseLock();
  await writable.close();
  await socket.closed;
  assert.equal(f.calls.filter(c => c[0] === "release").length, 1);
});

test("TCP accepts BufferSource views, applies backpressure and preserves the read half after FIN", async () => {
  const f = fixture([{ data: Uint8Array.of(9), eof: true }]);
  const socket = new f.device.directSockets.TCPSocket("socket.test", 7);
  const { readable, writable } = await socket.opened;
  const writer = writable.getWriter();
  await writer.write(new DataView(Uint8Array.of(8, 1, 2, 8).buffer, 1, 2));
  await writer.write(Uint8Array.of(3).buffer);
  await writer.write(new Uint8Array(140000));
  assert.deepEqual(f.calls.filter(c => c[0] === "write").map(c => c[2].length), [2, 1, 65535, 65535, 8930]);
  assert.deepEqual(f.calls.find(c => c[0] === "write")![2], Uint8Array.of(1, 2));
  await writer.close(); writer.releaseLock();
  assert.equal(f.calls.filter(c => c[0] === "release").length, 0);
  const reader = readable.getReader();
  assert.deepEqual((await reader.read()).value, Uint8Array.of(9));
  assert.equal((await reader.read()).done, true);
  reader.releaseLock(); await socket.closed;
});

test("connected UDP uses message objects, preserves empty datagrams and updates the race winner", async () => {
  const f = fixture([{ data: new Uint8Array(), eof: false }, { data: Uint8Array.of(4, 5), eof: false }]);
  const socket = new f.device.directSockets.UDPSocket({ remoteAddress: "socket.test", remotePort: 7 });
  const opened = await socket.opened;
  assert.throws(() => opened.readable.getReader({ mode: "byob" }), TypeError);
  const reader = opened.readable.getReader(), writer = opened.writable.getWriter();
  await writer.write({ data: new Uint8Array() });
  assert.equal(f.calls.find(c => c[0] === "write")![2].length, 0);
  f.setRemote("192.0.2.1:7");
  assert.deepEqual((await reader.read()).value, { data: new Uint8Array() });
  assert.equal(opened.remoteAddress, "192.0.2.1");
  assert.deepEqual((await reader.read()).value, { data: Uint8Array.of(4, 5) });
  await writer.close(); writer.releaseLock();
  assert.equal(f.calls.filter(c => c[0] === "release").length, 0);
  await reader.cancel(); reader.releaseLock(); await socket.closed;
});

test("close rejects pending-open and locked-stream states without closing a working socket", async () => {
  const f = fixture();
  const socket = new f.device.directSockets.TCPSocket("socket.test", 7);
  await assert.rejects(socket.close(), dom("InvalidStateError"));
  const { readable, writable } = await socket.opened;
  const reader = readable.getReader(), writer = writable.getWriter();
  await assert.rejects(socket.close(), dom("InvalidStateError"));
  assert.equal(f.calls.filter(c => c[0] === "release").length, 0);
  reader.releaseLock();
  await assert.rejects(socket.close(), dom("InvalidStateError"));
  writer.releaseLock();
  await Promise.all([socket.close(), socket.close()]); await socket.close();
  assert.equal(f.calls.filter(c => c[0] === "release").length, 1);
});

test("canceling a pending TCP or UDP read unblocks it and keeps write ownership until close", async () => {
  for (const udp of [false, true]) {
    const f = fixture();
    const socket = udp ? new f.device.directSockets.UDPSocket({ remoteAddress: "socket.test", remotePort: 7 }) : new f.device.directSockets.TCPSocket("socket.test", 7);
    const { readable, writable } = await socket.opened;
    const reader = readable.getReader();
    const pending = reader.read(); await tick();
    await reader.cancel(); assert.equal((await pending).done, true); reader.releaseLock();
    assert.equal(f.calls.filter(c => c[0] === "release").length, 0);
    await writable.close(); await socket.closed;
  }
});

test("opening failures reject both promises with NetworkError", async () => {
  const f = fixture(); f.override(async () => { throw new Error("connection refused"); });
  const socket = new f.device.directSockets.TCPSocket("socket.test", 7);
  await assert.rejects(socket.opened, dom("NetworkError"));
  await assert.rejects(socket.closed, dom("NetworkError"));
  await assert.rejects(socket.close(), dom("InvalidStateError"));
});

test("Device closure errors both streams and closed, including an idle socket", async () => {
  const f = fixture();
  const socket = new f.device.directSockets.TCPSocket("socket.test", 7);
  const { readable, writable } = await socket.opened;
  f.closed.reject(new Error("Device disconnected"));
  await assert.rejects(socket.closed, dom("NetworkError"));
  await assert.rejects(readable.getReader().read(), dom("NetworkError"));
  await assert.rejects(writable.getWriter().write(Uint8Array.of(1)), dom("NetworkError"));
  assert.equal(f.calls.filter(c => c[0] === "release").length, 1);
});

test("partial read errors deliver the data before reporting NetworkError", async () => {
  const f = fixture([{ data: Uint8Array.of(7), eof: false, error: "read failure" }]);
  const socket = new f.device.directSockets.TCPSocket("socket.test", 7);
  const { readable } = await socket.opened;
  const reader = readable.getReader();
  assert.deepEqual((await reader.read()).value, Uint8Array.of(7));
  await assert.rejects(reader.read(), dom("NetworkError"));
  await assert.rejects(socket.closed, dom("NetworkError"));
});

test("connected UDP rejects per-message destinations and invalid data", async () => {
  for (const message of [{ data: Uint8Array.of(1), remoteAddress: "other.test" }, { data: Uint8Array.of(1), remotePort: 9 }, { data: Uint8Array.of(1), dnsQueryType: "ipv4" }, {}, new Uint8Array()]) {
    const f = fixture();
    const socket = new f.device.directSockets.UDPSocket({ remoteAddress: "socket.test", remotePort: 7 });
    const { readable, writable } = await socket.opened;
    await assert.rejects(writable.getWriter().write(message as any), TypeError);
    await readable.cancel(); await assert.rejects(socket.closed, TypeError);
    assert.equal(f.calls.filter(c => c[0] === "write").length, 0);
  }
});

test("close waits for both directions to stop before releasing the connection", async () => {
  const readStop = deferred<void>(), writeStop = deferred<void>(), closed = deferred<void>();
  let released = false;
  const device = attachSocketAPI({ socketOperation: async (op: string) => {
    if (op === "dial") return { id: 1, localAddr: "127.0.0.1:1000", remoteAddr: "127.0.0.2:7" };
    if (op === "socketClosed") return closed.promise;
    if (op === "closeRead") return readStop.promise;
    if (op === "closeWrite") return writeStop.promise;
    if (op === "release") { released = true; closed.resolve(); }
  } });
  const socket = new device.directSockets.TCPSocket("host", 7);
  await socket.opened;
  const closing = socket.close(); await tick();
  readStop.resolve(); await tick(); assert.equal(released, false);
  writeStop.resolve(); await closing; assert.equal(released, true);
});

test("aborting a pending TCP or UDP write interrupts it without waiting for network progress", async () => {
  for (const udp of [false, true]) {
    const pending = deferred<number>(), closed = deferred<void>();
    const device = attachSocketAPI({ socketOperation: async (op: string) => {
      if (op === "dial") return { id: 1, localAddr: "127.0.0.1:1000", remoteAddr: "127.0.0.2:7" };
      if (op === "socketClosed") return closed.promise;
      if (op === "write") return pending.promise;
      if (op === "closeWrite" || op === "writeDeadline") pending.reject(new Error("write stopped"));
      if (op === "release") closed.resolve();
    } });
    const socket = udp ? new device.directSockets.UDPSocket({ remoteAddress: "host", remotePort: 7 }) : new device.directSockets.TCPSocket("host", 7);
    const { readable, writable } = await socket.opened;
    const writer = writable.getWriter();
    const write = writer.write((udp ? { data: Uint8Array.of(1) } : Uint8Array.of(1)) as any);
    await tick();
    const reason = new Error("application aborted");
    const rejected = assert.rejects(write, error => error === reason);
    await writer.abort(reason); await rejected; writer.releaseLock();
    await readable.cancel(); await socket.closed;
  }
});

test("partial write failures carry their byte count and reject closed", async () => {
  const closed = deferred<void>();
  const device = attachSocketAPI({ socketOperation: async (op: string) => {
    if (op === "dial") return { id: 1, localAddr: "127.0.0.1:1000", remoteAddr: "127.0.0.2:7" };
    if (op === "socketClosed") return closed.promise;
    if (op === "write") return { bytesWritten: 2, error: "network failed" };
    if (op === "release") closed.resolve();
  } });
  const socket = new device.directSockets.TCPSocket("host", 7);
  const { writable } = await socket.opened;
  await assert.rejects(writable.getWriter().write(Uint8Array.of(1, 2, 3)), (error: any) => dom("NetworkError")(error) && error.bytesWritten === 2);
  await assert.rejects(socket.closed, dom("NetworkError"));
});

test("constructors validate supported options and reject unimplemented features before dialing", () => {
  const f = fixture(); const { TCPSocket, UDPSocket } = f.device.directSockets;
  for (const port of [-1, 0, 65536, NaN, Infinity]) assert.throws(() => new TCPSocket("host", port), TypeError);
  for (const host of ["", "[::1]", "host/path", "host name"]) assert.throws(() => new TCPSocket(host, 7), TypeError);
  for (const options of [{ dnsQueryType: "all" }, { keepAliveDelay: 999 }, { sendBufferSize: 0 }, { receiveBufferSize: -1 }]) assert.throws(() => new TCPSocket("host", 7, options as any), TypeError);
  for (const options of [{ noDelay: true }, { keepAliveDelay: 1000 }, { sendBufferSize: 1024 }, { receiveBufferSize: 1024 }]) assert.throws(() => new TCPSocket("host", 7, options), dom("NotSupportedError"));
  for (const options of [{}, { remoteAddress: "host" }, { remotePort: 7 }, { remoteAddress: "host", remotePort: 7, localAddress: "::" }, { remoteAddress: "host", remotePort: 7, ipv6Only: false }]) assert.throws(() => new UDPSocket(options), TypeError);
  for (const options of [{ localAddress: "::" }, { remoteAddress: "224.0.0.1", remotePort: 7 }, { remoteAddress: "host", remotePort: 7, multicastLoopback: false }]) assert.throws(() => new UDPSocket(options), dom("NotSupportedError"));
  assert.equal(f.calls.length, 0);
});

// This type-level fixture also ensures callers need only the public dialer.
const acceptsDialer = (device: Pick<SocketDevice, "dial">) => createDirectSockets(device);
void acceptsDialer;
