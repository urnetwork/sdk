import { initWasm, isWasmInitialized, getWasmGlobals } from "./loader";
import type {
  InitOptions,
  ProxyDevice,
  ProxyConfig,
  SetupDeviceCallback,
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
