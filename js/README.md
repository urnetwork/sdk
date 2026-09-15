# URnetwork JavaScript SDK

The Go/WASM SDK runs in modern browsers and Node 24+. Both use a configured
hosted DeviceRemote. The canonical npm name is `@urnetwork/sdk`; its first
publication is pending. `@urnetwork/sdk-js` is the compatibility import.

## Install

```sh
npm install @urnetwork/sdk
```

The unqualified command selects `latest`. Preview builds use
`npm install @urnetwork/sdk@nightly`. ESM, CommonJS, TypeScript declarations,
React bindings, WASM and its matching Go runtime glue are included.

## Sockets and Direct Sockets

An initialized Device exposes `dial`, `dialTls` and `directSockets`. Conn has
async reads/writes, deadlines, close, and Web Streams adapters. TCP/UDP support
IPv4, IPv6 and hostname Happy Eyeballs. Plain UDP races the initial datagram
and selects the first reply; that datagram may be delivered to both addresses.

Use the Direct Sockets constructor signatures with a UR Device:

```js
const {TCPSocket, UDPSocket} = device.directSockets;
const socket = new TCPSocket("echo.example", 9000);
const {readable, writable} = await socket.opened;
const writer = writable.getWriter();
await writer.write(new TextEncoder().encode("hello"));
await writer.close();
writer.releaseLock();
const reader = readable.getReader({mode: "byob"});
console.log(await reader.read(new Uint8Array(1024)));
await reader.cancel();
reader.releaseLock();
await socket.close();
await socket.closed;
```

`createDirectSockets(device)` also returns bound constructors. TCP accepts
`BufferSource` writes and supports default/BYOB readers; connected UDP uses
`UDPMessage` objects (`{data}`). Both offer `opened`, `closed`, and asynchronous
`close()`. Release reader/writer locks before calling `close()`.

The SDK implementation runs in ordinary browsers and Node over the Device's
packet path; it does not use or replace browser globals. Chrome's native API
is restricted to [Isolated Web Apps](https://developer.chrome.com/docs/iwa/direct-sockets).
This client profile supports `dnsQueryType` and rejects bound UDP, multicast,
and per-socket buffer/no-delay/keep-alive tuning with `NotSupportedError`.
Listener sockets remain future work. Use `dialTls` for TLS/DTLS. See the
[socket design and support profile](https://github.com/urnetwork/sdk/blob/main/SOCKET.md).

Full programs and run guides are in the
[Node and browser socket examples](https://github.com/urnetwork/examples/tree/main/javascript/socket).
They include hosted Device setup, Undici/Axios socket integration for Node,
a browser Axios request adapter, and Direct Sockets TCP/UDP echo. Browser fetch and XHR
do not expose a custom socket factory.

## Build, check and publish

From this directory:

```sh
make package check-package
```

This builds paired WASM/glue and JS/types, creates canonical and compatibility
tarballs in `release/artifacts`, and tests real Node runtime initialization,
ESM/CommonJS imports and TypeScript consumption. Install the canonical tarball
locally before the first registry publication.

`make publish` uses the Go publisher to upload the checked artifacts; it skips
when npm credentials are absent. Set `SDK_PACKAGE_VERSION` for the release
identity and `SDK_PACKAGE_CHANNEL` for its npm tag (default `nightly`). The
release pipeline calls these targets. See the
[package-manager plan](https://github.com/urnetwork/sdk/blob/main/PACKAGEMANAGERS.md).
