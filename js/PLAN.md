# JS SDK remote device plan

> Architecture note (2026-07-10, as built): this plan originally routed device
> rpc through the connect service (browser → connect handler → resident →
> proxy host bridge). That was removed. The hosted `DeviceLocal` lives in the
> `server/proxy` process, so the browser `DeviceRemote` connects **directly to
> the proxy host** — there is no connect-service, resident, exchange, or bridge
> hop, and `server/connect` is not involved in device rpc at all. The sections
> below describe the final, single-leg architecture.


Implement the full surface area of `DeviceRemote` in the JS SDK, connected directly to
a hosted proxy `DeviceLocal`. This completes the design sketched in `sdk/proxy_device.go`
("remote device websocket transport with authentication", "compile SDK to wasm").

A JS client creates a `DeviceRemote` with the proxy host url
(`wss://<proxyHost>/device-rpc`) and the device's **signed proxy id**, gets events from
the hosted device, and configures it. The hosted device is the existing cloud proxy
`DeviceLocal` (`server/proxy` `ProxyDevice`), whose packet planes (socks, http, https,
wg) are unchanged.


## Architecture (direct)

The browser connects straight to the proxy host that runs the hosted device; the rpc
protocol terminates next to the `DeviceLocal` in the same process:

```
Browser DeviceRemote
  --wss /device-rpc (?proxy=<signed proxy id>)-->
    proxy api TLS listener --> deviceRpcHandler --> OpenProxyDevice(proxyId)
      --> hosted DeviceLocal's DeviceLocalRpc
```

- One websocket, one process. `deviceRpcHandler` (`server/proxy/device_rpc_handler.go`),
  a `GET /device-rpc` route on the proxy api TLS listener (`server/proxy/proxy_api.go`),
  terminates the ws, resolves the device by proxy id, and hands the ws to
  `ProxyDevice.PushDeviceRpc`, which feeds a `HostedDeviceRpcListener` → `DeviceLocalRpc`.
  The rpc protocol can only terminate here: `DeviceLocalRpc` holds an in-process
  reference to the `DeviceLocal`, and the proxy data planes need that same instance
  in-process.
- The endpoint shares the **proxy api listener**, not the forward-proxy https listener.
  Both terminate the same per-proxy SNI TLS and both authenticate by signed proxy id, and
  a control route belongs beside `POST /warmup` rather than beside the CONNECT /
  forward-proxy logic. Keeping it off the forward-proxy handler also avoids having to
  disambiguate an origin-form `/device-rpc` request from a forward-proxy request whose
  target path happens to be `/device-rpc`.
- wss only in production (the api listener is TLS-only). Tests wire the same handler into
  a plain http listener and dial ws — the way `server/connect` exposes its handler for
  tests — to avoid self-signed TLS trust setup.
- Wire format: the existing device rpc mux framing, every message `[streamTag][payload]`,
  tag 0 = forward (DeviceRemote is the net/rpc client of `DeviceLocalRpc`), tag 1 =
  reverse (`DeviceLocalRpc` is the net/rpc client of `DeviceRemoteRpc`). rpc writes are
  bufio-chunked (~4 KiB). There is **no auth frame** — auth happens at the http layer —
  so the mux starts immediately on both ends.


## Auth

The device's **signed proxy id** (`model.SignProxyId`, an HMAC-SHA1 bearer token, ~160
bits) is the credential — the same one the wg/https/socks data planes already use. The
browser passes it as the `proxy` query parameter (a browser `WebSocket` cannot set request
headers, but can set query params), so no jwt and no first-frame handshake are needed;
non-browser callers may instead use `Authorization: Bearer <signed>`. The proxy id
resolves the hosted device directly. The `DeviceRemote`'s network-member jwt is used only
for the network-space api (and to supply the remote's displayed client id), never for the
rpc endpoint.


## Verified constraints (these shaped the design)

- The browser `WebSocket` API cannot set request headers. Carrying the signed proxy id in
  the url query sidesteps that entirely — unlike a jwt in an `Authorization` header, it
  needs no first-frame handshake.
- The wasm build exists and ships (`sdk/js`, npm `@urnetwork/sdk-js`), but the Go
  networking paths are runtime-inert in a browser: the connect `ClientStrategy` sets
  `DialTLSContext` on its `http.Transport`, which defeats Go's automatic Fetch fallback
  under js/wasm, and gorilla/websocket has no browser backend. Today's JS SDK works only
  through its hand-written TypeScript fetch layer. The plan therefore adds a js-tagged
  browser-websocket rpc dialer and keeps api/jwt in the TS layer.
- Hosted devices live only in `server/proxy` (`ProxyDevice` wraps `sdk.DeviceLocal` via
  `NewPlatformDeviceLocal`), lazily opened per proxy id by `ProxyDeviceManager`,
  idle-reaped (90 min default), recreated on egress death.
- `newDeviceLocalRpcManager` already accepts any `DeviceRpcListener`, so the hosted attach
  point is a listener implementation (`HostedDeviceRpcListener`), not a refactor.


## Locked decisions

- Route: `GET /device-rpc` on the proxy api TLS listener (`server/proxy/proxy_api.go`),
  beside `POST /warmup`. wss only.
- Auth: the device's signed proxy id (`proxy` query param, or `Authorization: Bearer`).
  No jwt at the rpc endpoint.
- Close conventions: the rpc session closes when the mux closes and vice versa;
  `PushDeviceRpc` blocks for the session's lifetime and closes the websocket when it ends.
- Device recreate: client-driven setup, with notification. The rpc sync handshake carries
  a device generation id; `DeviceRemote` fires a `DeviceRecreatedListener` when the
  generation changes across reconnects; the JS client re-configures. Initial device state
  always comes from `proxy_device_config`. No write-through persistence of rpc-set state.
- `HostedIncompatible`: under no situation can the hosted device change route local or
  provide settings. Guarded at BOTH layers: a `DeviceLocalSettings` hard guard in
  `DeviceLocal`, and `DeviceLocalRpc.DisableHostedIncompatible = true` which noops the rpc
  setters while getters and listeners keep working. The set: `SetRouteLocal`,
  `SetProvideMode`, `SetProvidePaused`, `SetProvideControlMode`, `SetProvideNetworkMode`,
  `SetTunnelStarted`, `SetVpnInterfaceWhileOffline`, `SetRpcServer`, `SetByJwt`,
  `SetKeyMaterial`. `SetDestination` / `SetConnectLocation` are allowed (the point of a
  proxy device).
- Peers and quota: proxy clients count toward the network's client quota but never appear
  in peers. `peer_model` meta carries a category (`client` | `proxy`);
  `GetNetworkPeerProfile` detects proxy clients via a `proxy_device_config` existence
  check; connected proxy clients are tracked in a separate `connected_proxy` zset
  (`GetNetworkConnectedCount`) and the peers wire replay + `GET /network/peers` filter out
  the proxy category. The creation cap is already combined since proxy clients are
  top-level creates under `LimitTopLevelClientIdsPerNetwork`. (The hosted proxy device has
  no resident of its own — it reaches providers via its multi-client — so the live
  connected-count registration is driven by the transient proxy-client residents, not the
  hosted device.)
- JS bindings: hand-written `syscall/js` wrappers in the `sdk/js/main.go` style for the
  `Device` surface, with TypeScript definitions in `sdk/js/src`. The TS fetch layer keeps
  owning api calls and jwt refresh; the Go token manager is disabled under js.

Defaults (flag if these should change):

- Connected clients+proxy accounting: expose the combined count from the categorized
  registry; no enforcement of a concurrent quota yet.
- http-over-rpc stays disabled for platform-path connections (the JS client has native
  fetch).


## Wasm enablement (how the blockers are handled)

No sidecar JS is needed beyond the standard `wasm_exec.js` runtime glue the package
already bundles and loads. `syscall/js` reaches the browser `WebSocket` and `fetch`
directly.

- Websocket: `deviceRpcMux` has a small ws interface seam (`DeviceRpcWs`: write message,
  write control, next reader, deadlines, close), implemented by gorilla natively and by a
  hand-written `syscall/js` browser shim under `//go:build js` (`WebSocket`,
  `binaryType = "arraybuffer"`, `js.FuncOf` handlers feeding Go channels). Browsers cannot
  send ws protocol pings (they only auto-answer them), so keepalive in **both** directions
  is a zero-length binary mux frame, sent every ~5s; the mux read loop discards `len < 1`
  messages after resetting the read deadline. The read deadline is
  `KeepAliveTimeout*(KeepAliveRetryCount+1)` = 30s.
- Fetch: routed around rather than patched. The browser path makes no Go http calls: the
  TS fetch layer owns api calls and jwt refresh, the Go token manager is disabled under
  js, and the JS bindings expose the `Device` surface only (the view controller layer,
  which pulls in the Go `Api`, stays un-exported). Optional follow-up if Go http in wasm
  is ever wanted: a `//go:build js` ClientStrategy variant whose `http.Transport` omits
  `DialTLSContext`, re-enabling Go's automatic Fetch roundtripper.
- Browser origins: the `deviceRpcHandler` upgrader's `CheckOrigin` allows all origins —
  the signed proxy id, not the origin, is the security boundary — so browsers (ur.io, the
  extension) can upgrade regardless of the Origin header they send.
- `wasm_exec.js` currency (DONE): the glue is ABI-paired with the Go toolchain that built
  the wasm, so it is always copied from the building toolchain, never vendored. The
  Makefile `build_wasm` sources it from `$(go env GOROOT)/lib/wasm/wasm_exec.js` (fallback
  `misc/wasm` for Go < 1.24) instead of a hardcoded path; `sdk/go.mod` pins the version via
  a `toolchain` directive so every machine builds with the same compiler; and a
  `check_wasm` target gates on the packaged `wasm_exec.js` being byte-identical (`cmp`) to
  the toolchain's copy. Note: `go version <wasm>` cannot read js/wasm build info, so the
  wasm binary is paired by construction (built + glue-copied in the same target from one
  toolchain) and the glue byte-check is the gate.


## Implementation map (as built, 2026-07-10)

- **sdk**: `DeviceRpcWs` interface seam (`device_rpc_transport.go`);
  `PlatformDeviceRpcDialer` (`device_rpc_platform.go` + native/js variants) dials
  `wss://<proxyHost>/device-rpc?proxy=<signed>` with **no auth frame**; browser websocket
  shim (`device_rpc_platform_js.go`); `HostedDeviceRpcListener` (`device_rpc_hosted.go`);
  `DeviceLocalSettings.HostedIncompatible` + `DeviceLocalRpc.DisableHostedIncompatible`
  guards on the agreed setter set; device generation id in the sync handshake +
  `DeviceRecreatedListener`; zero-length binary mux keepalive.
  `NewPlatformDeviceRemote(networkSpace, byJwt, proxyUrl, signedProxyId, instanceId)`.
- **server/proxy**: `deviceRpcHandler` (`device_rpc_handler.go`) wired as a `GET
  /device-rpc` route on the api TLS listener (`proxy_api.go`), signed-proxy-id auth;
  `ProxyDevice.PushDeviceRpc` feeds the `HostedDeviceRpcListener` via
  `DeviceLocal.StartHostedRpc`; rpc attach keeps the device non-idle for the session.
- **server/connect**: unchanged — not in the device-rpc path. The peers/quota work is kept
  (`peer_model` proxy category via the `connected_proxy` zset; proxy clients counted but
  never in the peer list or events; residents register proxy clients with no peer
  subscription).
- **sdk/js**: `URnetworkNewPlatformDeviceRemote(apiUrl, platformUrl, byJwt, proxyUrl,
  signedProxyId)` + the core Device surface as hand-written `syscall/js` wrappers; listener
  bridging Go callbacks → JS functions; Makefile `build_wasm` sources `wasm_exec.js` from
  `go env GOROOT` + `check_wasm` byte-parity gate; `go.mod` toolchain pin.
- **tests**: `server/proxy/TestProxyDeviceRpcE2E` — a real `DeviceRemote` connecting
  directly to the proxy host device-rpc endpoint (plain http + ws in-test) against a hosted
  proxy `DeviceLocal`: auth + sync, getter/setter round-trips, `HostedIncompatible` no-op,
  reverse-channel listeners (offline), keepalive survives the 30s window, device recreate
  (generation change), peer exclusion. Plus sdk `TestDeviceRemoteNetworkPeers` /
  epoch-coalesce and model proxy-peer units.

Open follow-ups (not blocking):

- The core Device surface is bound to JS; the long tail of view-controller methods can be
  added with the same pattern as needed.
- The hosted proxy device has no resident, so the live connected-count registration path
  is unit-tested (`model.TestNetworkProxyPeer`) but not exercised by a hosted device in the
  e2e. If proxy clients should count toward a live concurrent quota, the proxy device (or
  the device-rpc attach) would need to register the proxy peer explicitly.
