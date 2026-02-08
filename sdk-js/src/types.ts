/**
 * Configuration for proxy behavior
 */
export interface ProxyConfig {
  lockCallerIp?: boolean;
  lockIpList?: string[];
  enableSocks?: boolean;
  enableHttp?: boolean;
  httpRequireAuth?: boolean;
}

/**
 * Proxy authentication credentials
 */
export interface ProxyAuthResult {
  username: string;
  password: string;
}

/**
 * Result of proxy configuration
 */
export interface ProxyConfigResult {
  expirationTime: number;
  keepaliveSeconds: number;
  httpProxyUrl: string;
  socksProxyUrl: string;
  httpProxyAuth?: ProxyAuthResult;
  socksProxyAuth?: ProxyAuthResult;
}

/**
 * Device interface - placeholder for future device methods
 */
export interface Device {
  // Device methods will be added as needed
}

/**
 * Callback invoked when a new device is set up
 */
export type SetupDeviceCallback = (
  device: Device,
  proxyConfigResult: ProxyConfigResult,
) => boolean | void;

/**
 * Proxy device - handles proxy connections
 */
export interface ProxyDevice {
  getDevice(): Device;
  getProxyConfigResult(): ProxyConfigResult;
  cancel(): void;
  close(): void;
  isDone(): boolean;
}

/**
 * SDK initialization options
 */
export interface InitOptions {
  wasmUrl?: string;
  wasmExecUrl?: string;
}
