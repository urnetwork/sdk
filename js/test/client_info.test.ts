import { test } from "node:test";
import assert from "node:assert/strict";
import { registerTsResolve } from "./ts-resolve.ts";

registerTsResolve();
const { createURNetworkApiClient, clientInfoHeaders, ClientDistribution, CLIENT_INFO_HEADER } = await import(
  "../src/client.ts"
);

// the client info header an app on the generated client alone sends
// (src/client_info.ts), as connect's ParseClientInfo reads it

test("the header names the device type, version and distribution", () => {
  const headers = clientInfoHeaders({
    deviceType: "web",
    appVersion: "0.1.8",
    distribution: ClientDistribution.ExtensionFirefox,
  });
  assert.deepEqual(Object.keys(headers), [CLIENT_INFO_HEADER]);
  assert.equal(CLIENT_INFO_HEADER, "X-UR-ClientInfo");
  assert.deepEqual(JSON.parse(headers[CLIENT_INFO_HEADER]), {
    v: 1,
    device_type: "web",
    app_version: "0.1.8",
    distribution: "extension_firefox",
  });
});

test("an unknown distribution is left out, as before the field", () => {
  const headers = clientInfoHeaders({ deviceType: "web", appVersion: "0.1.8" });
  assert.equal(headers[CLIENT_INFO_HEADER], '{"v":1,"device_type":"web","app_version":"0.1.8"}');
});

test("every request of a client configured with it carries the header", async () => {
  const sent: Array<string | null> = [];
  const client = createURNetworkApiClient({
    baseURL: "https://api.example.test",
    token: "jwt-1",
    headers: clientInfoHeaders({
      deviceType: "web",
      appVersion: "0.1.8",
      distribution: ClientDistribution.ExtensionChrome,
    }),
    fetch: async (_input: string | URL | Request, init?: RequestInit) => {
      sent.push(new Headers(init?.headers).get("x-ur-clientinfo"));
      return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } });
    },
  });
  await client.networkProxies();
  await client.removeNetworkClient({ client_id: "00000000-0000-0000-0000-000000000001" });
  assert.equal(sent.length, 2);
  for (const value of sent) {
    assert.deepEqual(JSON.parse(value ?? "null"), {
      v: 1,
      device_type: "web",
      app_version: "0.1.8",
      distribution: "extension_chrome",
    });
  }
});
