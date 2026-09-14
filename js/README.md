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

## Sockets and WebTransport

An initialized Device exposes `dial`, `dialTls` and `webTransport`. Conn has
async reads/writes, deadlines, close, and Web Streams adapters. TCP/UDP support
IPv4, IPv6 and hostname Happy Eyeballs. Plain UDP races the initial datagram
and selects the first reply; that datagram may be delivered to both addresses.

`webTransport` opens an HTTP/3 session over a UR UDP socket. It needs a
WebTransport server; use `dial` for arbitrary raw TCP/UDP destinations. See the
[socket design and support profile](https://github.com/urnetwork/sdk/blob/main/SOCKET.md).

Full programs and run guides are in the
[Node and browser socket examples](https://github.com/urnetwork/examples/tree/main/javascript/socket).
They include hosted Device setup, Undici/Axios socket integration for Node,
a browser Axios request adapter, and WebTransport echo. Browser fetch and XHR
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
