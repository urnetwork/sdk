// The client info an app reports with its api calls (X-UR-ClientInfo, the
// ClientInfo schema of connect/api/bringyour.yml, parsed by connect
// client_info.go): its device type, version and distribution. The server keeps
// the device type and version with each session's last use, and reads the
// device type and the distribution for the account's onboarding platform at
// sign-up and when a device authenticates.
//
// The sdk's wasm sets the header itself on the requests it makes
// (setClientInfo on a device or an account host). This is for an app that
// calls the api through the generated client alone, as the extension's service
// worker does: pass clientInfoHeaders(...) as the client config's `headers`.

/**
 * How a build reached the device (connect ClientDistribution*; the Go sdk
 * exports the same values). Each build passes its own, stamped at build time.
 * The server reads a value only with the device type it pairs with: AppStore
 * with ios and macos; MacDirect with macos; Play, AndroidDirect, SolanaDapp
 * and EthosDapp with android; Windows with windows; Linux, LinuxAppimage and
 * LinuxFlatpak with linux; Web, ExtensionChrome and ExtensionFirefox with web.
 */
export const ClientDistribution = {
  AppStore: "app_store",
  MacDirect: "mac_direct",
  Play: "play",
  AndroidDirect: "android_direct",
  SolanaDapp: "solana_dapp",
  EthosDapp: "ethos_dapp",
  Windows: "windows",
  Linux: "linux",
  LinuxAppimage: "linux_appimage",
  LinuxFlatpak: "linux_flatpak",
  Web: "web",
  ExtensionChrome: "extension_chrome",
  ExtensionFirefox: "extension_firefox",
} as const;

export type ClientDistribution = (typeof ClientDistribution)[keyof typeof ClientDistribution];

export const CLIENT_INFO_HEADER = "X-UR-ClientInfo";

export interface ClientInfoOptions {
  /** one of the server's device types: android, ios, macos, windows, linux, web */
  deviceType: string;
  /** the app's build version, at most 64 UTF-8 bytes */
  appVersion: string;
  /** the build's distribution; leave it out when it is not known */
  distribution?: ClientDistribution;
}

/**
 * The X-UR-ClientInfo header for every request of a generated api client
 * (`createURNetworkApiClient({ headers: clientInfoHeaders(...) })`). An
 * unknown distribution is left out, as every client sent it before the field.
 */
export function clientInfoHeaders(options: ClientInfoOptions): Record<string, string> {
  const info: { v: number; device_type: string; app_version: string; distribution?: string } = {
    v: 1,
    device_type: options.deviceType,
    app_version: options.appVersion,
  };
  if (options.distribution) {
    info.distribution = options.distribution;
  }
  return { [CLIENT_INFO_HEADER]: JSON.stringify(info) };
}
