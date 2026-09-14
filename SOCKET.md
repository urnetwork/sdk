# Device sockets

Design, implementation, and WebTransport research. Reviewed September 14, 2026.

## Purpose and scope

An SDK Device can open application connections through its packet path without an application creating a kernel socket or installing an operating-system VPN interface. Go callers receive `net.Conn`-compatible connections; JavaScript, C, and mobile bindings expose the same behavior in their own types. The first release provides outbound TCP, UDP, TLS over TCP, DTLS over UDP, and an HTTP/3 WebTransport client. Server sockets, listeners, multicast, and unconnected UDP are future work.

The central UDP decision is explicit: **for a hostname with both A and AAAA records, race the initial application datagram and select the first replying address. The initial datagram can be delivered to both addresses.** Later datagrams wait for selection and use only the winner. This is application-visible behavior, not a transparent implementation detail.

The implementation is in [`socket.go`](socket.go), [`socket_udp.go`](socket_udp.go), [`socket_rpc.go`](socket_rpc.go), [`socket_mobile.go`](socket_mobile.go), [`socket_webtransport.go`](socket_webtransport.go), [`js/socket.go`](js/socket.go), [`js/src/socket.ts`](js/src/socket.ts), and [`cgo/exports_socket.go`](cgo/exports_socket.go). User-facing guidance belongs in the ur.io Developers → Socket page. Package distribution is covered separately in [PACKAGEMANAGERS.md](PACKAGEMANAGERS.md).

## Research findings: raw sockets and WebTransport

### Two different interfaces

WebTransport is a session API with reliable streams and unreliable datagrams. The current W3C document is the July 30, 2026 Candidate Recommendation snapshot; it is still evolving. It defines protocol mappings for HTTP/3 and HTTP/2, not an arbitrary TCP/UDP endpoint API. The SDK should expose its raw `dial`/`Conn` API separately from a WebTransport session facade.[^w3c]

The WICG **Direct Sockets** proposal describes `TCPSocket` and `UDPSocket`, which more closely match the requested raw socket semantics. It is a distinct browser capability with its own exposure and permission model. Using an SDK Device does not implement or grant that browser capability, and the SDK does not install replacements for those global constructors.[^direct]

The old W3C TCP and UDP Sockets API is a historical Working Group Note, not the current WebTransport standard. It is useful background, not a compatibility target.[^old-sockets]

### HTTP/3 transport requirements

WebTransport over HTTP/3 establishes a session using an extended HTTP CONNECT request and negotiated settings. The HTTPS endpoint must implement WebTransport; a UDP echo server or ordinary HTTPS handler is insufficient. The protocol associates streams and HTTP datagrams with a session and carries session closure information. The selected implementation speaks `draft-ietf-webtrans-http3-16`.[^http3-wt]

QUIC runs over UDP but uses TLS 1.3 integrated into its cryptographic handshake. It does **not** run TLS records or DTLS over the UDP socket. Consequently, WebTransport opens a plain connected UDP Device socket and places QUIC above it; it must not call `DialTls("udp", ...)`.[^quic-tls]

QUIC datagrams do not provide retransmission or ordered delivery. A successful send therefore cannot promise that an application received the datagram. Reliable streams are the appropriate channel for data requiring ordered delivery and retransmission.[^quic-datagrams]

### Implementation selection

| Option | Device routing | Protocol fit | Decision |
| --- | --- | --- | --- |
| Browser `globalThis.WebTransport` | Browser chooses the network path; no custom Device dialer | Browser implementation and version | Do not use for Device sessions |
| Direct Sockets globals | Browser-managed sockets/permissions | Raw TCP/UDP, not WebTransport sessions | Separate browser feature; do not depend on it |
| WebSocket frames called “datagrams” | Could use an existing browser transport | Does not provide HTTP/3 WebTransport interoperability | Not a WebTransport implementation |
| `quic-go` + `webtransport-go` above a Device UDP connection | Explicit PacketConn adapter over the Device | Real QUIC/HTTP/3 session and stream protocol | Implemented |

The SDK already depends on `quic-go` v0.61.0. `webtransport-go` v0.12.0 matches that dependency and identifies draft 16 in its release README. Pin these versions together and recheck protocol compatibility when upgrading. Some older WebTransport library documentation describes older wire drafts; the pinned source/release and the corresponding IETF document are the implementation reference.[^wt-go]

Tests establish interoperability with a local `webtransport-go` v0.12.0 server through both local and RPC Devices. They do not establish compatibility with every deployed WebTransport server or browser version, nor complete W3C conformance.

## Go API and socket semantics

`Device` embeds these method interfaces. `net.Dialer` itself is a struct; its `Dial` and `DialContext` signatures are preserved, with two SDK secure-dial extensions in a separate interface:

```go
type Conn = net.Conn

type Dialer interface {
    Dial(network, address string) (net.Conn, error)
    DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type TLSDialer interface {
    DialTls(network, address string, config *tls.Config) (net.Conn, error)
    DialTlsContext(ctx context.Context, network, address string, config *tls.Config) (net.Conn, error)
}
```

Both `DeviceLocal` and `DeviceRemote` implement both interfaces. A standard `net.Dialer` satisfies `Dialer` but does not itself implement the additional SDK secure methods. Existing integrations can use `device.DialContext` directly as an HTTP transport or other client's dial function; they do not need to type-assert a concrete SDK connection.

| Aspect | Contract |
| --- | --- |
| Networks | `tcp`, `tcp4`, `tcp6`, `udp`, `udp4`, `udp6` |
| Address | `host:port`; bracket IPv6 literals, such as `[2001:db8::1]:443` |
| Establishment | Caller context plus a default 30-second dial/secure-handshake bound; earlier caller cancellation wins |
| Post-dial cancellation | The dial context controls establishment. Canceling it after success does not close the connection. |
| Ownership | Caller closes connections; canceling/closing the owning Device also closes them. |
| TCP | Byte stream; reads/writes can be partial. EOF is separate from data. Concurrent operations are supported. |
| UDP | Connected peer; each write is one datagram, each read consumes one datagram. An undersized read discards the remainder. Zero-byte datagrams are valid. |
| Deadlines | Absolute read/write deadlines, including pending I/O; `time.Time{}` clears them. Timeouts implement `net.Error`. |
| Half-close | Optional `CloseRead`/`CloseWrite` for connections that support them; not an unconnected-UDP control. |
| Addresses | Virtual local endpoint and selected remote endpoint. The local address is not the provider's public exit address. |
| Kernel-specific APIs | No file descriptor, `SyscallConn`, socket options, Unix-domain socket, packet capture socket, or arbitrary bind/listen operation |

Example: replace only an HTTP client's connection factory:

```go
transport := &http.Transport{DialContext: device.DialContext}
client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
// Standard net/http handles HTTPS TLS above the Device connection.
defer transport.CloseIdleConnections()
```

For direct TLS use `device.DialTlsContext(ctx, "tcp", "example.com:443", nil)`. `nil` selects normal certificate verification. Go TLS configuration is cloned before SDK changes, and SNI is inferred from the original hostname rather than the winning IP.

## Local Device packet path

```mermaid
flowchart LR
    A[Application Conn] --> B[connect.Tun / gVisor]
    B --> C[Device SendPacketsNoCopy]
    C --> D[Configured Device route / provider]
    D --> E[Destination]
    D --> F[Device receive callbacks]
    F --> B
```

Each Device lazily creates one private `connect.Tun`, sharing it among its sockets. This is an in-process IP/TCP/UDP stack, not an OS TUN file descriptor. It allocates distinct virtual IPv4/IPv6 addresses from connect's address pools. Ordinary Device construction remains cheap until the first socket is opened.

The outbound worker reads packet batches from the TUN and gives ownership to `SendPacketsNoCopy`. It clears the consumed batch references. Inbound callbacks inspect the packet's destination IP and inject only packets addressed to that private TUN. Matching uses the IP header rather than a transport-port parser: later IP fragments contain no TCP/UDP ports and must still reach gVisor reassembly. The source receive buffer is borrowed only for the synchronous injection.

Closing the Device prevents further opens, closes tracked connections and the TUN, removes the receive subscription, and joins the packet pump as part of the Device's close boundary. A connection closes at most once in the tracking wrapper. Cached endpoint addresses remain available after the underlying netstack connection closes.

The socket follows the Device's routing, provider selection, security policy, and connection lifecycle. Underlying carriers and an exit NAT may use kernel sockets. The claim is that the **application's connection and protocol stack** are in user space and follow the Device; it is not a claim that no process in the network uses an operating-system socket. Selecting a Device mode with local routing continues to mean local routing.

### DNS and isolation

Socket resolution uses the private TUN's resolver. Configured local and remote resolver endpoints are projected onto its remote path, so their traffic travels through the same Device. The host resolver fallback is disabled for this socket resolver. A missing/unavailable provider does not cause an application hostname to be resolved or connected directly through the host as a fallback.

The resolver configuration is derived from Device DNS settings. Updating the live settings replaces the private TUN resolver/cache for subsequent queries and retires the old cache; existing established sockets retain their peers. DNS override/interception behavior elsewhere in the Device remains its own layer.

## Happy Eyeballs

RFC 8305 provides the basis for staggered family attempts: prefer IPv6 when available, start the other family promptly, and dispose of unsuccessful attempts. Its full algorithm also addresses asynchronous DNS timing and address ordering. The SDK uses connect's TCP resolver/racing behavior and an explicit 250 ms family delay for secure handshakes and raw UDP first-datagram selection. The new UDP wrapper waits for the two family resolutions; it is not a claim to implement every RFC 8305 DNS timing recommendation.[^happy]

Explicit `tcp4`/`udp4` or `tcp6`/`udp6` restrict the family. A literal IP selects its family without a hostname race. For generic names with only one usable family, use that family. Errors and caller cancellation clean up attempts; a successful late loser is closed even if a lower dialer returns after cancellation.

### TCP and secure handshakes

Plain TCP uses `connect.Tun.DialContext`'s connection racing. A winner has completed a TCP connection. TLS races family-specific TCP **plus TLS handshakes**, allowing a family with a stalled or failing TLS handshake to lose to a working family. DTLS similarly races completed DTLS handshakes. QUIC races completed QUIC handshakes before HTTP/3 WebTransport session establishment. A successful bare UDP `connect` cannot be used as evidence that DTLS or QUIC works on that path.

Only handshake traffic is raced for secure connections. Application writes begin after a winner is returned; the raw-UDP duplicate-initial-application-datagram policy does not apply to TLS, DTLS, or QUIC application data. Multiple connection attempts can nevertheless be observed by servers.

### Plain UDP: initial datagram and first reply

1. Resolve/dial IPv6 and IPv4 candidates through the Device. If both exist, return an undecided connected-UDP wrapper. `Dial` has not established reachability.
2. The first `Write` snapshots its bytes and sends them to IPv6. If that write fails, try IPv4 immediately. Otherwise, send the same initial datagram to IPv4 after 250 ms without a reply.
3. Read one initial response from each candidate. The first response, including a zero-length datagram, permanently selects that candidate. Preserve it for the application's first `Read` and close the losing socket.
4. Subsequent writes wait until a winner exists, then send once to that peer. Later replies from the loser cannot change the connection. The fallback write cannot block processing a reply arriving on the other family.
5. Deadline expiration unblocks waiting application operations. Clearing/extending the deadline allows recovery while the family selection is pending. Closing the connection terminates candidate reads/writes and selection.

**Duplicate delivery is possible even when one server never replies.** The first write succeeding means the datagram was accepted for sending, not that it was delivered or a peer was selected. Application protocols should use request IDs/idempotence where appropriate. A send-only protocol or a server that waits for multiple requests before replying cannot select a winner with this policy: use a literal address or `udp4`/`udp6`, or arrange a first-message response. Set deadlines instead of allowing an undecided connection to wait indefinitely.

No arbitrary probe datagram is invented. No additional application datagrams are queued for replay to multiple peers. No winner is changed automatically after selection if the route later fails.

## TLS and DTLS

TCP uses Go `crypto/tls`; UDP uses Pion DTLS v3 with DTLS 1.2. `connectedPacketConn` adapts the Device `net.Conn` to `net.PacketConn` while rejecting writes to any address other than its connected peer. Pion's handshake is explicitly driven with the dial context before returning the connection.[^dtls]

The portable `SocketTLSOptions` contains `serverName`, `rootCAPEM`, and `nextProtos`. Supplying root PEM replaces the default trust pool; invalid PEM fails. JS/C/mobile APIs keep verification enabled. Native Go can supply `tls.Config`, including an explicit verification policy for TCP.

For DTLS the shared configuration supports roots, server name, client certificates, application protocols, `VerifyPeerCertificate`, explicit Go verification bypass, and key logging. Options without an equivalent supported mapping—such as TLS `VerifyConnection`, TLS client-certificate callback, custom cipher/curve lists, ECH, custom time/random sources, TLS session cache, or a version range excluding DTLS 1.2—fail rather than being silently treated as enforced. DTLS 1.3 is not implemented.

The WASM executable includes `golang.org/x/crypto/x509roots/fallback` because a browser-hosted Go executable cannot assume a native system certificate store. This dependency embeds trust roots and must be kept current with SDK rebuilds. Custom root PEM and WebTransport certificate hashes are explicit alternate trust inputs.[^roots]

## Remote Device and C/mobile bindings

### Remote sockets

The existing Device RPC dispatcher serves requests sequentially. Blocking `Dial` or `Read` inside an RPC method would prevent another request from setting a deadline or closing the socket. `DeviceLocalRpc.Socket` therefore implements start/poll operations: starting an open/read/write queues work and immediately returns; later polls consume the completed result once. Deadline/close controls stay responsive.

An RPC session owns a random-ID registry limited to 128 sockets, including pending opens. Each entry permits one pending read and one pending write, with a maximum 65,535-byte operation buffer. The client serializes reads and writes independently and chunks large TCP writes. It never splits a UDP datagram into separate writes. EOF, cancellation, closed-state, and timeout errors are encoded separately from byte counts so partial results are retained.

`DeviceRemote` captures the active ordinary or browser service at dial time. A connection remains bound to that service; it is not rebound or replayed after reconnect. Session shutdown cancels pending dials, closes sockets, and joins workers. IDs cannot be used from another RPC session. An older peer without the optional method returns an error; the client does not fall back to a kernel connection.

TLS/DTLS and QUIC can run in the client above this raw RPC connection. In the JS SDK that keeps the secure-protocol state in WASM while the owning remote Device supplies the packet path. Polling and buffer copies add overhead; these APIs emulate socket behavior, not the performance of a local file descriptor.

### C ABI

`urnet_device_dial` and `urnet_device_dial_tls` return owned connection handles. `urnet_conn_read`/`urnet_conn_write` perform actual I/O and report transferred bytes; reading is not a size-query/fill protocol. A separate `eof` output distinguishes EOF from a zero-byte UDP datagram. Partial byte counts can accompany an allocated error string. Clear/read/write deadline functions take epoch milliseconds; zero clears the deadline.

`urnet_conn_close` closes while retaining the handle; `urnet_release` releases it and closes its connection. Free returned error/address strings with `urnet_free_string`. Calls can block, so invoke them on worker threads. The C boundary validates pointer/length combinations and copies outgoing bytes before work may outlive the caller. The generated header documents buffer limits and ownership.

### Android and Apple bindings

`Device.OpenSocket(network, address, timeoutMillis, tlsOptions)` returns the portable `Socket` class. Nil TLS options request a plain socket; a nonnil options object requests TLS/DTLS. `Socket.Read(maxBytes)` returns `SocketRead{Data, Eof}`; deadline methods take epoch milliseconds. This avoids exporting `context.Context`, `net.Addr`, and `time.Time` through gomobile. Native-only methods are explicitly excluded by the export validator; portable methods remain subject to validation.

All three gomobile binds select the internal `sdk_mobile_bind` build tag (combined with `ios_extension` for the reduced Apple binding). Only that binding view removes `Dialer` and `TLSDialer` from the `Device` interface; every default/native Go target, including native Go on Android and Apple, retains both interfaces. Concrete Device dial implementations and `OpenSocket` are unchanged. Do not use this internal tag for Go embedding. An export-omission allowlist alone cannot make gobind's incomplete foreign proxy implement native-only method signatures.

## JavaScript SDK

SDK factories attach `dial`, `dialTls`, and `webTransport` to Device wrappers, including proxy callbacks and returned Devices. The WASM bridge uses one asynchronous dispatcher per Device and monotonically allocated resource handles, capped at 512 sockets/streams/sessions. Releasing a WebTransport session also releases its child handles, including streams arriving after closure. Device cancellation/closure tears down its registry.

```ts
// `device` is an initialized SDK Device with an available packet path.
const conn = await device.dial("tcp", "example.com:80", { timeoutMillis: 5000 });
try {
  await conn.setDeadline(Date.now() + 5000);
  await conn.write(new TextEncoder().encode("GET / HTTP/1.0\r\nHost: example.com\r\n\r\n"));
  for (let chunk; (chunk = await conn.read()) !== null;) {
    console.log(new TextDecoder().decode(chunk));
  }
} finally {
  await conn.close();
}
```

`Conn.read()` returns bytes or `null` for EOF; an empty `Uint8Array` is data. It also provides `ReadableStream`/`WritableStream` adapters. Do not mix direct reads/writes with their stream adapters concurrently. TCP logical writes are serialized across their bridge chunks; UDP boundaries are preserved. Inputs are copied before asynchronous work. Partial writes reject with `bytesWritten`; partial read data is returned before its accompanying error on the next read. Read deadlines can be cleared and retried through direct `read`; an errored Web Stream follows normal Web Streams terminal-error semantics.

`AbortSignal` and `timeoutMillis` control opening/handshakes. Established connections use deadlines and explicit close. WASM I/O runs in Go goroutines and resolves Promises without blocking the JS event loop.

### Implemented WebTransport profile

Use `device.webTransport(url, options)`, or import the SDK `WebTransport` constructor and pass `(device, url, options)`. This extra Device argument is intentional; the SDK does not replace the browser constructor.

```ts
const session = device.webTransport("https://transport.example/session");
await session.ready;
try {
  const stream = await session.createBidirectionalStream();
  const writer = stream.writable.getWriter();
  await writer.write(new TextEncoder().encode("hello"));
  await writer.close(); // sends FIN; receiving remains possible
  const reader = stream.readable.getReader();
  console.log(await reader.read());

  const datagrams = session.datagrams.writable.getWriter();
  await datagrams.write(new Uint8Array([1, 2, 3]));
} finally {
  session.close({ closeCode: 0, reason: "done" });
  await session.closed;
}
```

| Feature | SDK behavior |
| --- | --- |
| Wire transport | HTTP/3, QUIC, TLS 1.3, draft-16 WebTransport; HTTP/2 fallback absent |
| Lifecycle | `ready`, `closed`, `close({closeCode, reason})`; handshake failure rejects both promises; normal peer closure preserves its code/reason |
| Reliable streams | Outgoing/incoming unidirectional and bidirectional streams, reads/writes, FIN, cancellation/reset |
| Incoming queues | Demand-driven stream acceptance and datagram reads; graceful session closure ends queues |
| Unreliable data | `datagrams.readable`, compatibility `datagrams.writable`, and `datagrams.createWritable()` |
| Datagram size | Conservative 1,024-byte send limit; oversize writes are dropped. No promise of delivery. |
| Negotiation | HTTPS URL, optional application protocols, page-derived Origin, `h3` ALPN |
| Certificates | Normal PKI/custom root PEM, or explicit SHA-256 certificate pins with current lifetime ≤14 days and ECDSA P-256 |
| Pooling/congestion | No pooling; nondefault congestion choices rejected; `reliability` reports unreliable support |
| Not implemented | HTTP/2 mapping, BYOB readers, transferability, send groups/order, datagram age/queue tuning, per-stream/session statistics, draining/key export, and complete WebIDL/browser conformance |

Certificate pins match the full DER leaf certificate, not only its public key, and are copied before use. Pin mode replaces normal chain/name validation with pin/lifetime/key checks. Browser cookies and HTTP authorization headers are not implicitly inherited from `fetch`. Servers should validate Origin and apply application authentication appropriate to their session endpoint. Server protocol/version mismatch and certificate failures are reported; there is no silent switch to browser-native WebTransport.

The profile deliberately exposes a working protocol implementation plus a familiar JS stream/session surface. It is not advertised as a complete replacement for every feature in the current W3C interface. Missing advanced members must be implemented and validated before expanding that compatibility claim.

## Platforms, validation, and future listeners

The Go implementation builds on the SDK's existing Go targets. Android and Apple use portable gomobile bindings; C clients use the desktop C ABI. The JS package loads its WASM runtime in Node 24+ and modern browsers. Both environments use a configured reachable Device RPC endpoint with socket support. The browser does not require native WebTransport or Direct Sockets globals. Bun and other JS runtimes have not been qualified.

The network tests use two real connect/gVisor TUNs with an in-memory Device packet route, local dual-family DNS servers, and actual TCP/UDP/TLS/DTLS/HTTP3 peers. No public DNS, provider account, or Internet echo service is needed for the socket tests. They cover:

- Network/family matrix; names with working or blackholed IPv4/IPv6; TLS/DTLS handshake racing and inferred SNI.
- TCP partial/large I/O, deadlines and recovery, cancellation, half-close, EOF, and close unblocking reads.
- UDP empty datagrams, truncation, boundaries, IPv4/IPv6 fragmentation, initial duplicate delivery, delayed loser replies, no reply/deadline recovery, and fixed winner routing.
- RPC responsiveness with blocked reads/dials, session isolation, limits, disconnects, and no replay.
- Real HTTP/3 datagrams and both stream directions through local and remote Devices, protocol/Origin negotiation, certificate verification, pins, and session closure.
- C ABI byte counts/errors, EOF, bad buffers, deadline reset, release during reads, and stale handles.
- TypeScript stream backpressure, lifecycle/error propagation, byte snapshots, large concurrent writes, and Go/WASM dispatcher/lifetime tests.
- Real WASM initialization and closure in Node and Chrome, ESM/CommonJS package consumers, and executable Node/browser examples. Node's Undici/Axios adapters and Python's HTTPX/Requests adapters are tested against local HTTP and verified HTTPS peers.

Useful commands:

```sh
go test -race . -run '^TestSocket' -count=1 -timeout=90s
go test -short ./... -timeout=180s
(cd cgo && go test -race . -run '^TestSocketABI' -count=1)
(cd js && npm run build && npm test)
(cd js && GOOS=js GOARCH=wasm go test -exec="$(go env GOROOT)/lib/wasm/go_js_wasm_exec" . -run '^TestSocketWasm')
```

Generated C/C++ exports and Java gomobile exports are also checked. A macOS arm64 XCFramework is built and linked into the Swift example and an isolated SwiftPM consumer. An Android arm64 AAR and its sources/Javadocs are built and packaged. Full Apple/Android device coverage and cross-platform live-provider/browser interoperability remain release qualification work; passing host/Go/WASM tests does not substitute for them. The repository's long-running leak/soak tests need their own time budget rather than the short unit-test timeout.

Future server work should add a separate listener/packet-listener surface with explicit bind semantics, provider reachability, accept cancellation, ownership, and authorization. It must not imply that a virtual local address is publicly reachable. Preserve the outbound `Conn` contract and avoid changing established UDP winner semantics when listener support arrives.

## Sources

Primary sources were consulted for protocol/API definitions; repository code and tests establish SDK-specific behavior. Research status is dated above because the WebTransport API and wire protocol remain active work.

[^w3c]: [W3C WebTransport Candidate Recommendation, July 30, 2026](https://www.w3.org/TR/2026/CR-webtransport-20260730/), including the session/stream model, lifecycle, datagrams, and certificate-hash mode.
[^direct]: [WICG Direct Sockets API](https://wicg.github.io/direct-sockets/).
[^old-sockets]: [W3C TCP and UDP Socket API Working Group Note](https://www.w3.org/TR/tcp-udp-sockets/).
[^http3-wt]: [IETF WebTransport over HTTP/3, draft 16](https://datatracker.ietf.org/doc/html/draft-ietf-webtrans-http3-16).
[^quic-tls]: [RFC 9001: Using TLS to Secure QUIC](https://www.rfc-editor.org/rfc/rfc9001.html).
[^quic-datagrams]: [RFC 9221: An Unreliable Datagram Extension to QUIC](https://www.rfc-editor.org/rfc/rfc9221.html).
[^wt-go]: [webtransport-go v0.12.0 release README](https://github.com/quic-go/webtransport-go/blob/v0.12.0/README.md).
[^happy]: [RFC 8305: Happy Eyeballs Version 2](https://www.rfc-editor.org/rfc/rfc8305.html).
[^dtls]: [Pion DTLS v3 API](https://pkg.go.dev/github.com/pion/dtls/v3).
[^roots]: [Go x509roots fallback package](https://pkg.go.dev/golang.org/x/crypto/x509roots/fallback).
