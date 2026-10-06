# URnetwork JavaScript SDK

The Go/WASM SDK runs in modern browsers and Node 24+. It controls a native
Device through `DeviceRemote`, using either a configured hosted device or an
extension/companion transport. The canonical npm name is `@urnetwork/sdk`;
its first publication is pending. `@urnetwork/sdk-js` is the compatibility
import.

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

## Peer messages

A provider-capable native Device can expose raw application subprotocols to
JavaScript through an extension/companion `DeviceRemote`:

```js
const messages = await device.enableSubprotocol(4097, async message => {
  console.log(message.sourceClientId, message.bytes);
});
const supported = await messages.querySubprotocols(peerClientId, 10_000);
if (supported?.includes(4097)) {
  await messages.send(peerClientId, new TextEncoder().encode("hello"));
}
await messages.close();
await messages.closed;
```

Each callback receives one complete owned `Uint8Array` and the authenticated
source client ID. `send()` reports local queue acceptance, so application
protocols should acknowledge delivery when needed. A subscription is bound to
one RPC connection and closes on reconnect. Hosted proxy devices reject this
API because they do not have a visible peer identity. The executable
[JavaScript messages example](https://github.com/urnetwork/examples/tree/main/javascript/messages)
includes live peer updates, support queries, TEXT/ACK framing, and an
authenticated numeric-loopback native companion.

## Provider status

A `DeviceRemote` of a providing native Device (an extension/companion
transport) reads the provider's state directly. The device pushes it to the
remote after every change, so the getters stay current without a listener:

```js
const status = {
  provideMode: device.getProvideMode(), // 3 is public
  providePaused: device.getProvidePaused(),
  provideEnabled: device.getProvideEnabled(),
  providerConnected: device.getProviderConnected(),
  clientLimitStatus: device.getClientLimitStatus(), // {status, retryTime}
};
const stats = device.getProviderPacketStats(); // null without a provider
const dataProvided = stats
  ? stats.remoteEgressByteCount + stats.remoteIngressByteCount
  : 0;
const rows = [
  ...(device.getProviderIngressContractDetails() ?? []),
  ...(device.getProviderEgressContractDetails() ?? []),
];
```

Each contract row carries its `contractTransferPath` (`sourceId`,
`destinationId`, `streamId`); the peer of an ingress contract is its source,
of an egress contract its destination. The `add...ChangeListener` methods
report the same values as they change, one contract row per call, and the
provider contract details controller's entries carry the `streamId` too.

## Hosted device configuration

A hosted Device starts from the settings its session was provisioned with,
and the host can recreate it under the remote (an idle reap, an egress death,
a host restart). `addDeviceConfigurationChangedListener` fires when the Device
may not hold the settings the app applied: after the remote's first sync with
it (treat a first connect as a change), after a sync that reaches a recreated
Device, and after every sync with a Device that reports no generation. A
reconnect to the same Device does not fire. Apply your own settings again
there:

```js
const device = sdk.createExtensionDeviceRemote(options);
device.addDeviceConfigurationChangedListener(() => {
  const want = mySettings();
  if (device.getBlockerEnabled() !== want.blockerEnabled) {
    device.setBlockerEnabled(want.blockerEnabled);
  }
});
```

Add the listener right after creating the remote. The getters already read the
synced Device when it fires. Write only the values that differ: every write
resyncs the remote, and a Device without a generation fires on each sync.

## Build, check and publish

`make smoke` and `make build_checked` first validate the committed Go-derived
types and OpenAPI client without rewriting them. Stale output fails the test
build before WASM or npm work. Use `make generate_types` to refresh both generated
surfaces deliberately, then commit the changes. `make check_generated` checks
them without building; developer `make build` still generates before building.

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
