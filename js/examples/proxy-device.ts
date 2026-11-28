// Starts a hosted proxy device with the TypeScript API, prints its proxy URLs,
// and removes the device again on Ctrl-C (sdk#117). It needs only fetch: no
// wasm and no DOM, so it runs as is in Node 24, which runs TypeScript
// directly:
//
//   URNETWORK_USER_AUTH=you@example.com URNETWORK_PASSWORD=... node proxy-device.ts
//   URNETWORK_JWT=<network jwt> node proxy-device.ts
//
// A hosted proxy device is a client of your network that runs on a URnetwork
// proxy host. It connects out through the network's providers, and anything
// that speaks HTTP, HTTPS or SOCKS proxy sends through it. Each one counts
// toward the plan's active proxy limit until it is removed. See README.md.
import { URNetworkAPI } from "@urnetwork/sdk/client";

type AuthNetworkClientArgs = Parameters<URNetworkAPI["authNetworkClient"]>[0];

const api = new URNetworkAPI();

// A network token: the one given, or one from signing in with a password. An
// account that still has to verify its email or phone does that in the app
// first.
async function networkToken(): Promise<string> {
  const byJwt = process.env.URNETWORK_JWT;
  if (byJwt) {
    return byJwt;
  }
  const userAuth = process.env.URNETWORK_USER_AUTH;
  const password = process.env.URNETWORK_PASSWORD;
  if (!userAuth || !password) {
    throw new Error("Set URNETWORK_JWT, or URNETWORK_USER_AUTH and URNETWORK_PASSWORD.");
  }
  const login = await api.authLoginWithPassword({ user_auth: userAuth, password });
  if (login.error) {
    throw new Error(`Sign-in failed: ${login.error.message}`);
  }
  if (login.verification_required) {
    throw new Error(`Verify ${login.verification_required.user_auth} in the URnetwork app, then run this again.`);
  }
  const token = login.network?.by_jwt;
  if (!token) {
    throw new Error("Sign-in returned no network token.");
  }
  return token;
}

// The proxy device: HTTP, HTTPS and SOCKS, exiting at the best available
// location. initial_device_state.location picks another one (a location id
// from api.searchProviderLocations).
const proxyDeviceArgs: AuthNetworkClientArgs = {
  description: "sdk example",
  device_spec: "",
  proxy_config: {
    lock_caller_ip: false,
    lock_ip_list: [],
    enable_socks: true,
    enable_http: true,
    http_require_auth: false,
    https_require_auth: false,
    enable_wg: false,
    initial_device_state: {
      location: {
        connect_location_id: { best_available: true },
        stable: true,
        strong_privacy: true,
      },
      // null: the sdk's default connect options
      performance_profile: null,
    },
  },
};

const byJwt = await networkToken();
const result = await api.authNetworkClient(proxyDeviceArgs, byJwt);
if (result.error) {
  // a plan limit says so here, e.g. the active proxy limit
  console.error(`The proxy device was not created: ${result.error.message}`);
  process.exit(1);
}
const proxy = result.proxy_config_result;
const clientId = result.client_id ?? null;
if (!proxy) {
  console.error("The server answered without a proxy configuration.");
  process.exit(1);
}

console.log(`proxy device ${clientId ?? "(no client id)"}, until ${proxy.expiration_time}`);
console.log(`  https proxy: ${proxy.https_proxy_url ?? "(none)"}`);
console.log(`  http proxy:  ${proxy.http_proxy_url ?? "(none)"}`);
console.log(`  socks proxy: ${proxy.socks_proxy_url ?? "(none)"}`);
if (proxy.https_proxy_url) {
  console.log(`try it: curl --proxy '${proxy.https_proxy_url}' https://api.bringyour.com/my-ip-info`);
}
console.log("Ctrl-C removes the proxy device.");

// The proxy device stays until removed, or until the server reaps it after a
// long idle; remove it when done so it stops counting toward the plan's limit.
let removing = false;
const remove = async () => {
  if (removing) {
    return;
  }
  removing = true;
  const removed = await api.removeNetworkClient({ client_id: clientId }, byJwt);
  if (removed.error) {
    console.error(`Could not remove the proxy device: ${removed.error.message}`);
    process.exit(1);
  }
  console.log("proxy device removed");
  process.exit(0);
};
process.on("SIGINT", remove);
process.on("SIGTERM", remove);

// keeps the process running until a signal removes the device
setInterval(() => {}, 60 * 60 * 1000);
