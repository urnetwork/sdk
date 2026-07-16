import { initWasm, isWasmInitialized, getWasmGlobals } from "./loader";
import type {
  InitOptions,
  ProxyDevice,
  ProxyConfig,
  SetupDeviceCallback,
  DeviceRemote,
  PlatformDeviceRemoteOptions,
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
