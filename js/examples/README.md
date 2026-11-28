# JavaScript SDK examples

## Start a proxy device

[`proxy-device.ts`](proxy-device.ts) starts a hosted proxy device with the
TypeScript API, prints its proxy URLs, and removes the device again on Ctrl-C.
A hosted proxy device is a client of your network that runs on a URnetwork proxy
host: it connects out through the network's providers, and any program that can
use an HTTP, HTTPS or SOCKS proxy sends through it.

It uses only the api client (`@urnetwork/sdk/client`: fetch, no wasm, no DOM),
so it runs in Node 24, which runs TypeScript directly. From a project with the
package installed (`npm install @urnetwork/sdk`, or the tarball
`make package` writes to `release/artifacts` before the first publication):

```sh
URNETWORK_USER_AUTH=you@example.com URNETWORK_PASSWORD=... node proxy-device.ts
```

or, with a network token you already have:

```sh
URNETWORK_JWT=<network jwt> node proxy-device.ts
```

It prints something like:

```
proxy device <client id>, until <expiration time>
  https proxy: https://<token>.<proxy host>:<port>
  http proxy:  http://<token>.<proxy host>:<port>
  socks proxy: socks5h://<token>.<proxy host>:<port>
try it: curl --proxy 'https://...' https://api.bringyour.com/my-ip-info
Ctrl-C removes the proxy device.
```

What it does:

1. **Signs in.** `URNetworkAPI.authLoginWithPassword` returns the network
   token (`by_jwt`). An account that still has to verify its email or phone
   number does that in the URnetwork app first.
2. **Starts the proxy device.** `URNetworkAPI.authNetworkClient` with a
   `proxy_config` asks the server for a hosted proxy device. Its
   `initial_device_state.location` picks where it exits: best available here,
   or a location id from `searchProviderLocations`. The answer's
   `proxy_config_result` holds the proxy URLs, their expiration, and the
   device's `api_base_url`, `auth_token` and `instance_id`.
3. **Removes it.** `URNetworkAPI.removeNetworkClient` with the device's client id.
   A proxy device counts toward the plan's active proxy limit until it is
   removed, or until the server reaps it after a long idle. A refusal at the
   limit comes back as `result.error`.

To steer the running device (its location, the blocker, and the other device
settings), create a `DeviceRemote` for it with the wasm SDK:
`sdk.createPlatformDeviceRemote({ apiUrl, platformUrl, byJwt, proxyUrl:
api_base_url, signedProxyId: auth_token, instanceId: instance_id })` (see
`createPlatformDeviceRemote` in `src/index.ts`).

The URLs carry the device's token, so treat them like a password.

## Type check

```sh
npm run typecheck:examples
```

checks the examples against the SDK's source (`tsconfig.json` here maps the
package names to `../src`). `node-globals.d.ts` stands in for `@types/node`
in this check only; leave it out of a project that has `@types/node`.
