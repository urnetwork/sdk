import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const source = (relative: string): string =>
  readFileSync(new URL(relative, import.meta.url), "utf8");

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
