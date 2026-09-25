import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const source = (relative: string): string =>
  readFileSync(new URL(relative, import.meta.url), "utf8");

// Only SDK-owned exported closes return a join promise. The inbound transport
// callback remains synchronous and must not be confused with a WASM owner.
test("owned WASM close promises do not change caller-supplied transport callbacks", () => {
  const declarations = source("../src/types.ts");
  for (const name of ["ProxyDevice", "DeviceRemote", "ConnectViewController", "ContractDetailsViewController",
    "ContractViewController", "BlockActionViewController", "LocationsViewController", "DevicesViewController",
    "PointsLeaderboardViewController", "ProviderLocationsViewController", "PeerViewController",
    "AccountPreferencesViewController", "NetworkUserViewController", "FeedbackViewController", "ReferralCodeViewController",
    "SubscriptionBalanceViewController", "AccountHost"]) {
    const body = declarations.match(new RegExp(`export interface ${name}\\b[\\s\\S]*?\\n}`))?.[0];
    assert.match(body || "", /close\(\): Promise<void>/, name);
  }
  const transport = declarations.match(/export interface DeviceRpcTransportConnection[\s\S]*?\n}/)?.[0];
  assert.match(transport || "", /close\(\): void/);
  assert.match(source("../account_host.go"), /m\["close"\] = jsViewControllerClose\(/);
  assert.match(source("../device_remote.go"), /m\["close"\] = jsViewControllerClose\(device.Close, socketHandles.close, subprotocolHandles.close\)/);
  assert.match(source("../main.go"), /"close": jsViewControllerClose\(proxyDevice.Close\)/);
  assert.match(source("../main.go"), /m\["close"\] = jsViewControllerClose\(device.Close, handles.close\)/);
});

// The WASM bindings are authored in Go while the public declarations are
// authored in TypeScript. Keep a small explicit baseline for the
// contract-details surface so a rename cannot compile on one side and become
// undefined at runtime on the other.
test("contract-details declarations match WASM runtime keys", () => {
  const declarations = source("../src/types.ts");
  const deviceRuntime = source("../device_remote.go");
  const controllerRuntime = source("../view_controllers.go");

  for (const method of [
    "openContractDetailsViewController",
    "openClientContractDetailsViewController",
    "openProviderContractDetailsViewController",
  ]) {
    assert.match(declarations, new RegExp(`\\b${method}\\s*\\(`));
    assert.match(deviceRuntime, new RegExp(`m\\[\"${method}\"\\]`));
  }

  for (const method of [
    "getContractRows",
    "setAtTop",
    "pendingCount",
    "getClientContractRows",
    "getProviderContractRows",
    "addContractRowsListener",
  ]) {
    assert.match(declarations, new RegExp(`\\b${method}\\s*\\(`));
    assert.match(controllerRuntime, new RegExp(`m\\[\"${method}\"\\]`));
  }

  for (const field of [
    "sendContracts",
    "receiveContracts",
    "sendByteCount",
    "receiveByteCount",
    "lastActivityMillis",
    "closing",
  ]) {
    assert.match(declarations, new RegExp(`\\b${field}\\s*:`));
    assert.match(controllerRuntime, new RegExp(`\"${field}\"\\s*:`));
  }
});

// Same guard for the block-action surface, whose runtime lives in
// view_controllers2.go.
test("block-action declarations match WASM runtime keys", () => {
  const declarations = source("../src/types.ts");
  const controllerRuntime = source("../view_controllers2.go");

  for (const method of [
    "getBlockStats",
    "getBlockActions",
    "getWindowDurationSeconds",
    "setWindowDurationSeconds",
    "getMaxBlockActions",
    "setMaxBlockActions",
    "addBlockActionsListener",
    "addBlockActionStatsListener",
  ]) {
    assert.match(declarations, new RegExp(`\\b${method}\\s*\\(`));
    assert.match(controllerRuntime, new RegExp(`m\\[\"${method}\"\\]`));
  }

  // BlockAction feed row fields (jsBlockAction) and the BlockStats counters
  // (jsBlockStats)
  for (const field of [
    "time",
    "block",
    "local",
    "ips",
    "hosts",
    "matchedIps",
    "matchedHosts",
    "allowedCount",
    "blockedCount",
  ]) {
    assert.match(declarations, new RegExp(`\\b${field}\\s*:`));
    assert.match(controllerRuntime, new RegExp(`\"${field}\"\\s*:`));
  }
});

test("hosted DeviceRemote requires the server instance and surfaces sync refusals", () => {
  const declarations = source("../src/types.ts");
  const publicWrapper = source("../src/index.ts");
  const runtime = source("../device_remote.go");

  assert.match(declarations, /\binstanceId\s*:\s*string/);
  assert.match(declarations, /\bgetSyncError\s*\(/);
  assert.match(publicWrapper, /options\.instanceId/);
  assert.match(runtime, /len\(args\) < 6/);
  assert.match(runtime, /sdk\.ParseId\(args\[5\]\.String\(\)\)/);
  assert.doesNotMatch(runtime, /instanceId := sdk\.NewId\(\)/);
  assert.match(runtime, /m\["getSyncError"\]/);
});

test("extension DeviceRemote exposes an opaque transport without page credentials", () => {
  const declarations = source("../src/types.ts");
  const publicWrapper = source("../src/index.ts");
  const loader = source("../src/loader.ts");
  const runtime = source("../device_remote.go");
  const main = source("../main.go");

  assert.match(declarations, /interface DeviceRpcTransport\b/);
  assert.match(declarations, /open\(callbacks: DeviceRpcTransportCallbacks\)/);
  assert.match(declarations, /interface ExtensionDeviceRemoteOptions\b/);
  assert.match(publicWrapper, /createExtensionDeviceRemote/);
  assert.match(publicWrapper, /options\.transport/);
  const extensionOptions =
    declarations.match(/interface ExtensionDeviceRemoteOptions[\s\S]*?\n}/)?.[0] ?? "";
  assert.doesNotMatch(extensionOptions, /(proxyUrl|signedProxyId|apiBaseUrl)/);
  assert.match(runtime, /sdk\.NewExtensionDeviceRemote/);
  assert.match(main, /URnetworkNewExtensionDeviceRemote/);
  assert.match(loader, /URnetworkNewExtensionDeviceRemote/);
});

// Same guard for the account-plane and peer controllers (view_controllers3.go)
// and the account host (account_host.go).
test("account host and controller declarations match WASM runtime keys", () => {
  const declarations = source("../src/types.ts");
  const controllerRuntime = source("../view_controllers3.go");
  const hostRuntime = source("../account_host.go");
  const deviceRuntime = source("../device_remote.go");

  assert.match(deviceRuntime, /m\["openPeerViewController"\]/);
  assert.match(declarations, /\bopenPeerViewController\s*\(/);

  for (const method of [
    "getPeers",
    "getPeerCount",
    "getConnectedCount",
    "addPeersListener",
    "getAllowProductUpdates",
    "updateAllowProductUpdates",
    "addAllowProductUpdatesListener",
    "fetchNetworkUser",
    "getNetworkUser",
    "updateNetworkUser",
    "addNetworkUserUpdateErrorListener",
    "addNetworkUserUpdateSuccessListener",
    "addIsUpdatingListener",
    "sendFeedback",
    "addIsSendingFeedbackListener",
    "getReferralCode",
    "addReferralCodeListener",
    "getIsPro",
    "getAvailableByteCount",
    "getCurrentSubscription",
    "getPurchaseConfirmationState",
    "startPurchaseConfirmation",
    "jwtRefreshed",
    "addSubscriptionBalanceChangeListener",
    "addPurchaseConfirmationListener",
  ]) {
    assert.match(declarations, new RegExp(`\\b${method}\\s*\\(`));
    assert.match(controllerRuntime, new RegExp(`m\\[\"${method}\"\\]`));
  }

  for (const method of [
    "openLocationsViewController",
    "openDevicesViewController",
    "openAccountPreferencesViewController",
    "openNetworkUserViewController",
    "openFeedbackViewController",
    "openReferralCodeViewController",
    "openSubscriptionBalanceViewController",
    "getNetworkClients",
    "removeNetworkClient",
    "getNetworkReferralCode",
    "validateReferralCode",
    "setNetworkReferral",
    "getReferralNetwork",
    "unlinkReferralNetwork",
    "authCodeCreate",
    "networkDelete",
    "getLeaderboard",
    "getNetworkLeaderboardRanking",
    "setNetworkLeaderboardPublic",
    "getNetworkReliability",
    "getNetworkRedeemedBalanceCodes",
    "redeemBalanceCode",
    "checkBalanceCode",
    "subscriptionBalance",
    "getNetworkUser",
  ]) {
    assert.match(declarations, new RegExp(`\\b${method}\\s*\\(`));
    assert.match(hostRuntime, new RegExp(`m\\[\"${method}\"\\]`));
  }

  for (const field of ["colorHex"]) {
    assert.match(declarations, new RegExp(`\\b${field}\\s*:`));
    assert.match(deviceRuntime, new RegExp(`\"${field}\"\\s*:`));
  }
});

test("suggestEmojiTag declarations match WASM runtime keys", () => {
  const declarations = source("../src/types.ts");
  const main = source("../main.go");
  assert.match(declarations, /\bsuggestEmojiTag\s*\(/);
  assert.match(source("../account_host.go"), /m\["suggestEmojiTag"\]/);
  assert.match(source("../device_remote.go"), /m\["suggestEmojiTag"\]/);
  assert.match(main, /URnetworkSuggestEmojiTag/);
});

test("license declarations match WASM runtime keys", () => {
  const declarations = source("../src/types.ts");
  const device = declarations.match(/export interface Device extends[\s\S]*?\n}/)?.[0] || "";
  const deviceRemote = declarations.match(/export interface DeviceRemote extends[\s\S]*?\n}/)?.[0] || "";
  assert.match(device, /getLicenses\(app: LicenseApp\): LicenseInfo\[\]/);
  assert.match(deviceRemote, /getLicenses\(app: LicenseApp\): LicenseInfo\[\]/);
  assert.match(source("../main.go"), /m\["getLicenses"\] = js.FuncOf\(/);
  assert.match(source("../main.go"), /js.Global\(\).Set\("URnetworkGetLicenses", js.FuncOf\(jsGetLicenses\)\)/);
  assert.match(source("../device_remote.go"), /m\["getLicenses"\] = js.FuncOf\(/);
  const info = declarations.match(/export interface LicenseInfo \{[\s\S]*?\n}/)?.[0] || "";
  const runtime = source("../license.go");
  assert.match(info, /\bkind: "data" \| "software" \| "font";/);
  assert.match(runtime, /"kind":/);
  for (const field of ["name", "version", "origin", "url", "spdx", "copyright", "notice", "text"]) {
    assert.match(info, new RegExp(`\\b${field}: string;`), field);
    assert.match(runtime, new RegExp(`"${field}":`), field);
  }
});

test("filteredLocations declarations match the WASM runtime", () => {
  const declarations = source("../src/types.ts");
  const index = source("../src/index.ts");
  const loader = source("../src/loader.ts");
  const main = source("../main.go");
  const runtime = source("../view_controllers2.go");

  // the global, registered by main.go and surfaced by the loader and URNetwork
  assert.match(main, /js.Global\(\).Set\("URnetworkFilteredLocationsFromResult", js.FuncOf\(FilteredLocationsFromResult\)\)/);
  assert.match(main, /return jsFilteredLocations\(sdk.GetFilteredLocationsFromResult\(&result, filter\)\)/);
  assert.match(loader, /URnetworkFilteredLocationsFromResult: any;/);
  assert.match(loader, /URnetworkFilteredLocationsFromResult: runtimeGlobal.URnetworkFilteredLocationsFromResult/);
  assert.match(index, /filteredLocations\(\s*result: [^)]*string,\s*filter: string = "",?\s*\): FilteredLocations \| null/);
  assert.match(index, /URnetworkFilteredLocationsFromResult\(json, filter\)/);

  // the go side takes (resultJson string, filter string)
  assert.match(main, /args\[0\].Type\(\) != js.TypeString/);

  // every group key jsFilteredLocations emits is declared, and vice versa
  const filtered = declarations.match(/export interface FilteredLocations \{[\s\S]*?\n}/)?.[0] || "";
  const emitted = runtime.match(/func jsFilteredLocations[\s\S]*?\n}/)?.[0] || "";
  const runtimeKeys = [...emitted.matchAll(/"(\w+)":/g)].map((m) => m[1]).sort();
  const declaredKeys = [...filtered.matchAll(/^\s{2}(\w+): /gm)].map((m) => m[1]).sort();
  assert.deepEqual(declaredKeys, runtimeKeys);
  assert.deepEqual(runtimeKeys, ["bestMatches", "cities", "countries", "devices", "promoted", "regionGroups", "regions"]);
  assert.match(filtered, /regionGroups: RegionGroupInfo\[\];/);

  const group = declarations.match(/export interface RegionGroupInfo \{[\s\S]*?\n}/)?.[0] || "";
  const groupRuntime = runtime.match(/func jsRegionGroupList[\s\S]*?\n}/)?.[0] || "";
  assert.match(group, /region: ConnectLocationInfo \| null;/);
  assert.match(group, /cities: ConnectLocationInfo\[\];/);
  assert.match(groupRuntime, /"region": region,/);
  assert.match(groupRuntime, /"cities": jsConnectLocationList\(group.Cities\),/);

  // each location is the ConnectLocationInfo shape jsConnectLocation emits
  const info = declarations.match(/export interface ConnectLocationInfo \{[\s\S]*?\n}/)?.[0] || "";
  const location = source("../device_remote.go").match(/func jsConnectLocation\([\s\S]*?\n}/)?.[0] || "";
  for (const key of ["name", "locationType", "countryCode", "providerCount", "colorHex",
    "connectLocationId", "locationId", "locationGroupId", "clientId", "bestAvailable"]) {
    assert.match(location, new RegExp(`"${key}"`), key);
    assert.match(info, new RegExp(`\\b${key}\\??: `), key);
  }
});
