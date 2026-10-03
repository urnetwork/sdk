import { initWasm, isWasmInitialized, getWasmGlobals } from "./loader";
import { attachSocketAPI } from "./socket";
import { attachSubprotocolAPI } from "./subprotocol";
export * from "./socket";
export * from "./subprotocol";
import type {
  InitOptions,
  ProxyDevice,
  ProxyConfig,
  SetupDeviceCallback,
  DeviceRemote,
  PlatformDeviceRemoteOptions,
  ExtensionDeviceRemoteOptions,
  LocationsViewControllerOptions,
  LocationsViewController,
  AccountHostOptions,
  AccountHost,
  LicenseApp,
  LicenseInfo,
  Device,
  FilteredLocations,
} from "./types";
import type { FindLocationsResult } from "./generated";
import type * as OpenAPI from "./generated/openapi";
import type { SocketDevice } from "./socket";

export * from "./types";
export * from "./client";
export * from "./utils";

// a device as the wasm hands it over: its own methods, before the socket API
// is attached
type WasmDevice = Omit<Device, keyof SocketDevice>;

export class URNetwork {
  private static instance: URNetwork | null = null;

  private constructor() {}

  /**
   * Initialize the SDK
   * @example
   * const sdk = await URNetwork.init({
   *   wasmUrl: '/wasm/sdk.wasm',
   *   wasmExecUrl: '/wasm/wasm_exec.js'
   * });
   */
  static async init(options: InitOptions = {}): Promise<URNetwork> {
    if (URNetwork.instance) {
      return URNetwork.instance;
    }

    // Initialize WASM
    await initWasm(options);

    const instance = new URNetwork();
    URNetwork.instance = instance;
    return instance;
  }

  /**
   * Get the existing SDK instance
   */
  static getInstance(): URNetwork {
    if (!URNetwork.instance) {
      throw new Error("SDK not initialized. Call URNetwork.init() first.");
    }
    return URNetwork.instance;
  }

  /**
   * Create a proxy device
   * @example
   * const proxyDevice = sdk.createProxyDevice(
   *   { enableHttp: true },
   *   (device, proxyConfig) => {
   *     console.log('Proxy URL:', proxyConfig.httpProxyUrl);
   *     return true;
   *   }
   * );
   */
  createProxyDevice(
    config?: ProxyConfig,
    setupCallback?: SetupDeviceCallback,
  ): ProxyDevice {
    const { URnetworkNewProxyDeviceWithDefaults } = getWasmGlobals();
    const proxy = URnetworkNewProxyDeviceWithDefaults(config, setupCallback
      ? (device: WasmDevice, result: Parameters<SetupDeviceCallback>[1]) => setupCallback(attachSocketAPI(device), result)
      : undefined);
    const getDevice = proxy.getDevice.bind(proxy);
    proxy.getDevice = () => attachSocketAPI(getDevice() as WasmDevice);
    return proxy;
  }

  /**
   * Create a DeviceRemote controlling a hosted DeviceLocal on the proxy host.
   *
   * This is the web equivalent of the app process controlling the device in the
   * native apps: the client connects to wss://<proxyUrl>/device-rpc, authenticated
   * with the device's signed proxy id (the `auth_token` the platform returns from
   * /network/auth-client), and drives the hosted device — connect location,
   * blocker, peers, and every other device setting — over that rpc.
   *
   * @example
   * const sdk = await URNetwork.init({ wasmUrl: '/wasm/sdk.wasm', wasmExecUrl: '/wasm/wasm_exec.js' });
   * const device = sdk.createPlatformDeviceRemote({
   *   apiUrl: 'https://api.bringyour.com',
   *   platformUrl: 'wss://connect.bringyour.com',
   *   byJwt,
   *   proxyUrl: proxyConfigResult.api_base_url,
   *   signedProxyId: proxyConfigResult.auth_token,
   *   instanceId: proxyConfigResult.instance_id,
   * });
   * device.addConnectLocationChangeListener((loc) => console.log(loc?.name));
   * device.setConnectLocation({ bestAvailable: true });
   */
  createPlatformDeviceRemote(options: PlatformDeviceRemoteOptions): DeviceRemote {
    const { URnetworkNewPlatformDeviceRemote } = getWasmGlobals();
    if (typeof URnetworkNewPlatformDeviceRemote !== "function") {
      // the wasm predates the DeviceRemote binding (sdk/js/device_remote.go) —
      // rebuild it (`make -C sdk/js build_wasm`) rather than failing silently
      throw new Error(
        "URnetworkNewPlatformDeviceRemote is not exported by the loaded wasm. Rebuild the sdk wasm.",
      );
    }
    const device = URnetworkNewPlatformDeviceRemote(
      options.apiUrl,
      options.platformUrl,
      options.byJwt,
      options.proxyUrl,
      options.signedProxyId,
      options.instanceId,
    );
    if (!device) {
      throw new Error("Could not create the device remote.");
    }
    if (device.error) {
      throw new Error(String(device.error));
    }
    return attachSubprotocolAPI(attachSocketAPI(device)) as DeviceRemote;
  }

  /**
   * Open the sdk LocationsViewController over the network space api alone —
   * the grouped/promoted location browse every app's chooser renders — for a
   * signed-in host that has no device plane (no extension attached). Same
   * shape as device.openLocationsViewController(); close() releases it.
   */
  createLocationsViewController(options: LocationsViewControllerOptions): LocationsViewController {
    const { URnetworkNewLocationsViewController } = getWasmGlobals();
    if (typeof URnetworkNewLocationsViewController !== "function") {
      throw new Error(
        "URnetworkNewLocationsViewController is not exported by the loaded wasm. Rebuild the sdk wasm.",
      );
    }
    const vc = URnetworkNewLocationsViewController(options.apiUrl, options.platformUrl, options.byJwt);
    if (!vc) {
      throw new Error("Could not open the locations view controller.");
    }
    if (vc.error) {
      throw new Error(String(vc.error));
    }
    return vc as LocationsViewController;
  }

  /**
   * Open the account host: the network space api plus the api-only view
   * controllers (locations, devices, preferences, profile, feedback, referral
   * code, subscription balance) for a signed-in page with no device, so the
   * account screens render the same sdk controllers as the apps. close()
   * releases it.
   */
  createAccountHost(options: AccountHostOptions): AccountHost {
    const { URnetworkNewAccountHost } = getWasmGlobals();
    if (typeof URnetworkNewAccountHost !== "function") {
      throw new Error(
        "URnetworkNewAccountHost is not exported by the loaded wasm. Rebuild the sdk wasm.",
      );
    }
    const host = URnetworkNewAccountHost(options.apiUrl, options.platformUrl, options.byJwt);
    if (!host) {
      throw new Error("Could not open the account host.");
    }
    if (host.error) {
      throw new Error(String(host.error));
    }
    return host as AccountHost;
  }

  /**
   * The sdk palette color (hex, no "#") for a code the page already holds: a
   * country code, or a bare location / client id. Locations the sdk hands out
   * already carry `colorHex`; this is for ids persisted before that.
   */
  colorHex(code: string): string {
    const { URnetworkColorHex } = getWasmGlobals();
    if (typeof URnetworkColorHex !== "function") {
      return "";
    }
    return String(URnetworkColorHex(code) || "");
  }

  /**
   * Group and order a raw /network/provider-locations or
   * /network/find-provider-locations result the way every app's location
   * chooser renders it (best matches, promoted, countries, regions, cities,
   * devices, and regions with their cities nested as regionGroups) — the
   * sdk's own GetFilteredLocationsFromResult, run in the wasm, so a page
   * with a result but no device orders it exactly like android/apple.
   *
   * `filter` is the search text the result answers: a non-empty filter puts
   * exact matches (match distance 0) in bestMatches and fills
   * regions/cities/regionGroups; an empty filter is the unsearched browse.
   *
   * Returns null when the result cannot be read as a FindLocationsResult.
   *
   * @example
   * const result = await client.networkFindProviderLocations({ query: "ger" });
   * const grouped = sdk.filteredLocations(result, "ger");
   */
  filteredLocations(
    result: OpenAPI.FindLocationsResult | FindLocationsResult | string,
    filter: string = "",
  ): FilteredLocations | null {
    const { URnetworkFilteredLocationsFromResult } = getWasmGlobals();
    if (typeof URnetworkFilteredLocationsFromResult !== "function") {
      throw new Error(
        "URnetworkFilteredLocationsFromResult is not exported by the loaded wasm. Rebuild the sdk wasm.",
      );
    }
    const json = typeof result === "string" ? result : JSON.stringify(result);
    return (URnetworkFilteredLocationsFromResult(json, filter) ?? null) as FilteredLocations | null;
  }

  /**
   * The open source licenses and data attributions `app` publishes under
   * Settings -> Licenses, from the license list embedded in the sdk. Data
   * attributions come first; an entry's `notice`, when set, must be shown.
   */
  licenses(app: LicenseApp): LicenseInfo[] {
    const { URnetworkGetLicenses } = getWasmGlobals();
    if (typeof URnetworkGetLicenses !== "function") {
      throw new Error(
        "URnetworkGetLicenses is not exported by the loaded wasm. Rebuild the sdk wasm.",
      );
    }
    return (URnetworkGetLicenses(app) || []) as LicenseInfo[];
  }

  /**
   * Create the SDK DeviceRemote with an extension-owned device-rpc socket.
   * Endpoint and proxy credentials never enter the page-side SDK.
   */
  createExtensionDeviceRemote(options: ExtensionDeviceRemoteOptions): DeviceRemote {
    const { URnetworkNewExtensionDeviceRemote } = getWasmGlobals();
    if (typeof URnetworkNewExtensionDeviceRemote !== "function") {
      throw new Error(
        "URnetworkNewExtensionDeviceRemote is not exported by the loaded wasm. Rebuild the sdk wasm.",
      );
    }
    const device = URnetworkNewExtensionDeviceRemote(
      options.apiUrl,
      options.platformUrl,
      options.byJwt,
      options.instanceId,
      options.transport,
    );
    if (!device) {
      throw new Error("Could not create the extension device remote.");
    }
    if (device.error) {
      throw new Error(String(device.error));
    }
    return attachSubprotocolAPI(attachSocketAPI(device)) as DeviceRemote;
  }

  /**
   * Close the SDK and clean up resources
   */
  close(): void {
    const { URnetworkClose } = getWasmGlobals();
    URnetworkClose();
    URNetwork.instance = null;
  }

  /**
   * Check if the SDK is initialized
   */
  isInitialized(): boolean {
    return isWasmInitialized();
  }
}

export default URNetwork;
