import { initWasm, isWasmInitialized, getWasmGlobals } from "./loader";
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
} from "./types";

export * from "./types";
export * from "./api";
export * from "./utils";

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
    return URnetworkNewProxyDeviceWithDefaults(config, setupCallback);
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
   *   apiUrl: 'api.bringyour.com',
   *   platformUrl: 'connect.bringyour.com',
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
    return device as DeviceRemote;
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
    return device as DeviceRemote;
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
