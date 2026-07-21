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
 * The proxy config reported by the wasm proxy-device binding
 * (`ProxyDevice.getProxyConfigResult()` and the setup callback). This is the
 * camelCase wasm projection (js/main.go jsProxyConfigResult) — the REST
 * /network/auth-client response carries the snake_case generated shape instead.
 */
export interface ProxyConfigResult {
  /** unix epoch milliseconds */
  expirationTime: number;
  keepaliveSeconds: number;
  httpProxyUrl: string;
  socksProxyUrl: string;
  httpProxyAuth: ProxyAuthResult | null;
  socksProxyAuth: ProxyAuthResult | null;
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
  getProxyConfigResult(): ProxyConfigResult | null;
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

/**
 * A connect location: either a specific location id, or best-available.
 */
export interface ConnectLocationSpec {
  connectLocationId?: string;
  bestAvailable?: boolean;
  name?: string;
}

export interface ConnectLocationInfo {
  connectLocationId?: string;
  name: string;
  locationType: string;
  countryCode: string;
  providerCount: number;
}

export interface NetworkPeerInfo {
  clientId?: string;
  provideEnabled: boolean;
  principal: string;
  deviceName: string;
  deviceSpec: string;
  roles: string[];
}

export interface NetworkPeersInfo {
  connected: NetworkPeerInfo[];
  disconnectedCount: number;
}

/** Listener adders return an unsubscribe function. */
export type Unsubscribe = () => void;

/**
 * DeviceRemote — the client's handle on a hosted DeviceLocal. It reaches the
 * device over the proxy host's device-rpc websocket (authenticated with the
 * device's signed proxy id) and controls it exactly as the app process controls
 * the device in the native apps.
 *
 * Mirrors the bindings in sdk/js/device_remote.go. Hosted-incompatible setters
 * (route local, provide settings) are accepted but no-op on the hosted device;
 * the getters and listeners still reflect real device state.
 */
export interface DeviceRemote {
  // lifecycle
  close(): void;
  cancel(): void;
  getRemoteConnected(): boolean;

  // offline / tunnel
  getOffline(): boolean;
  setOffline(offline: boolean): void;
  getVpnInterfaceWhileOffline(): boolean;
  setVpnInterfaceWhileOffline(v: boolean): void;
  getTunnelStarted(): boolean;

  // routing / blocker
  getRouteLocal(): boolean;
  setRouteLocal(v: boolean): void;
  getBlockerEnabled(): boolean;
  setBlockerEnabled(v: boolean): void;

  // provide
  getProvidePaused(): boolean;
  setProvidePaused(v: boolean): void;
  getProvideEnabled(): boolean;

  // connect location / destination
  getConnectLocation(): ConnectLocationInfo | null;
  setConnectLocation(location: ConnectLocationSpec | null): void;
  removeDestination(): void;
  shuffle(): void;
  getConnectEnabled(): boolean;

  // peers
  getNetworkPeers(): NetworkPeersInfo | null;

  // listeners
  addRemoteChangeListener(cb: (remoteConnected: boolean) => void): Unsubscribe;
  addDeviceRecreatedListener(cb: () => void): Unsubscribe;
  addConnectChangeListener(cb: (connectEnabled: boolean) => void): Unsubscribe;
  addOfflineChangeListener(
    cb: (offline: boolean, vpnInterfaceWhileOffline: boolean) => void,
  ): Unsubscribe;
  addConnectLocationChangeListener(
    cb: (location: ConnectLocationInfo | null) => void,
  ): Unsubscribe;
  addNetworkPeersChangeListener(cb: (peers: NetworkPeersInfo | null) => void): Unsubscribe;

  // custom DNS resolver settings (over the device-rpc)
  getDnsResolverSettings(): DnsResolverSettings | null;
  setDnsResolverSettings(settings: DnsResolverSettings): void;
  getDefaultDnsResolverSettings(): DnsResolverSettings | null;
  addDnsResolverSettingsChangeListener(
    cb: (settings: DnsResolverSettings | null) => void,
  ): Unsubscribe;

  // view controllers — the same layer the native app screens use. The caller
  // owns each returned controller and must close() it.
  openConnectViewController(): ConnectViewController;
  /** @deprecated Use the split client/provider methods. */
  openContractDetailsViewController(): ContractDetailsViewController;
  openClientContractDetailsViewController(): ContractDetailsViewController;
  openProviderContractDetailsViewController(): ContractDetailsViewController;
  openContractViewController(): ContractViewController;
  openBlockActionViewController(): BlockActionViewController;
  openLocationsViewController(): LocationsViewController;
  openDevicesViewController(): DevicesViewController;
}

// ── view controllers ─────────────────────────────────────────────────────────
// The same layer the native app screens are built on. Opened from a device
// (openConnectViewController / openContractDetailsViewController); the caller
// owns the returned controller and MUST close() it.

/** DISCONNECTED | CONNECTING | DESTINATION_SET | CONNECTED */
export type ConnectionStatus =
  | "DISCONNECTED"
  | "CONNECTING"
  | "DESTINATION_SET"
  | "CONNECTED";

/** InEvaluation | EvaluationFailed | NotAdded | Added | Removed */
export type ProviderState =
  | "InEvaluation"
  | "EvaluationFailed"
  | "NotAdded"
  | "Added"
  | "Removed";

export interface ProviderGridPoint {
  x: number;
  y: number;
  clientId?: string;
  state: ProviderState;
  active: boolean;
  /** absolute time the point is removed */
  endTimeUnixMillis?: number;
  /** relative time until removal — what an exit animation wants */
  endTimeMillisUntil?: number;
}

export interface ConnectGrid {
  width: number;
  height: number;
  /** the provider window the device is filling toward */
  windowTargetSize: number;
  windowCurrentSize: number;
  points: ProviderGridPoint[];
}

/**
 * ConnectViewController — the connect state machine the app screens render:
 * connection status, selected location, the provider grid, connect/disconnect.
 */
export interface ConnectViewController {
  close(): void;
  start(): void;
  stop(): void;

  getConnected(): boolean;
  getConnectionStatus(): ConnectionStatus;
  getSelectedLocation(): ConnectLocationInfo | null;
  getGrid(): ConnectGrid | null;

  connect(location: ConnectLocationSpec): void;
  connectBestAvailable(): void;
  disconnect(): void;

  addConnectionStatusListener(cb: () => void): Unsubscribe;
  addSelectedLocationListener(cb: (location: ConnectLocationInfo | null) => void): Unsubscribe;
  addGridListener(cb: () => void): Unsubscribe;
}

/** @deprecated Aggregate compatibility projection; use ContractPeerRow. */
export interface ContractClientRow {
  clientId: string;

  contractId: string;
  companionContractId: string;

  contractUsedByteCount: number;
  contractByteCount: number;
  contractBitRate: number;

  companionContractUsedByteCount: number;
  companionContractByteCount: number;
  companionContractBitRate: number;

  pairCount: number;
  /** the client's last contract closed and the row is ejecting */
  closing: boolean;
}

/**
 * One individual contract in a peer row's newest-first direction stack.
 */
export interface ContractEntry {
  contractId: string;
  usedByteCount: number;
  totalByteCount: number;
  bitRate: number;
  hasStream: boolean;
}

/** The runtime row returned by getContractRows(). */
export interface ContractPeerRow {
  clientId: string;
  sendContracts: ContractEntry[];
  receiveContracts: ContractEntry[];
  sendByteCount: number;
  receiveByteCount: number;
  lastActivityMillis: number;
  closing: boolean;
}

/**
 * One client- or provider-feed contract-details controller. It groups
 * individual contracts by peer, owns the closing lifecycle and at-top
 * ordering, and reports rows that exactly match the WASM runtime object.
 */
export interface ContractDetailsViewController {
  close(): void;
  start(): void;
  stop(): void;

  getContractRows(): ContractPeerRow[];
  setAtTop(atTop: boolean): void;
  pendingCount(): number;

  /** @deprecated Available on the combined compatibility controller. */
  getClientContractRows(): ContractClientRow[];
  /** @deprecated Available on the combined compatibility controller. */
  getProviderContractRows(): ContractClientRow[];

  addContractRowsListener(cb: () => void): Unsubscribe;
}

/** egress/ingress throughput sample */
export interface ThroughputSample {
  egressByteCount: number;
  ingressByteCount: number;
  egressPacketCount: number;
  ingressPacketCount: number;
  egressBitRate: number;
  ingressBitRate: number;
}

/** one throughput time point, split remote/local/block */
export interface ThroughputPoint {
  time: number;
  remote: ThroughputSample | null;
  local: ThroughputSample | null;
  block: ThroughputSample | null;
}

export interface PacketStats {
  remoteEgressByteCount: number;
  remoteIngressByteCount: number;
  localEgressByteCount: number;
  localIngressByteCount: number;
  blockEgressByteCount: number;
  blockIngressByteCount: number;
}

/**
 * ContractViewController — throughput over the window, for the client feed and
 * the PROVIDER feed (the account's provider-statistics surface). Listener is
 * signal-only.
 */
export interface ContractViewController {
  close(): void;
  start(): void;
  stop(): void;

  getThroughputPoints(): ThroughputPoint[];
  getProviderThroughputPoints(): ThroughputPoint[];
  getPacketStats(): PacketStats | null;
  getProviderPacketStats(): PacketStats | null;
  getWindowDurationSeconds(): number;
  setWindowDurationSeconds(seconds: number): void;

  addThroughputListener(cb: () => void): Unsubscribe;
}

/** ad/tracker blocking counters over the current window */
export interface BlockStats {
  allowedCount: number;
  blockedCount: number;
}

/** one aggregated routing decision — a cluster of ips/hosts and whether blocked */
export interface BlockAction {
  blockActionId?: string;
  time: number;
  block: boolean;
  local: boolean;
  /** cluster ips/hosts that did NOT match an override (disjoint from matchedIps/matchedHosts) */
  ips: string[];
  hosts: string[];
  /**
   * the exact ips and hosts that matched an override rule, disjoint from
   * ips/hosts so nothing is shown or counted twice (empty when no override
   * matched). The UI renders these distinctly at the front of the row.
   */
  matchedIps: string[];
  matchedHosts: string[];
}

/**
 * BlockActionViewController — ad/tracker blocking. Live allow/block stats and
 * the recent block-action feed, over a configurable window. Both listeners are
 * signal-only: re-read the getters on notify.
 */
export interface BlockActionViewController {
  close(): void;
  start(): void;
  stop(): void;

  getBlockStats(): BlockStats | null;
  getBlockActions(): BlockAction[];
  getWindowDurationSeconds(): number;
  setWindowDurationSeconds(seconds: number): void;
  /** the retained block-action feed length cap */
  getMaxBlockActions(): number;
  setMaxBlockActions(maxBlockActions: number): void;

  addBlockActionsListener(cb: () => void): Unsubscribe;
  addBlockActionStatsListener(cb: () => void): Unsubscribe;
}

/** LOCATIONS_LOADING | LOCATIONS_LOADED | LOCATIONS_ERROR */
export type FilterLocationsState =
  | "LOCATIONS_LOADING"
  | "LOCATIONS_LOADED"
  | "LOCATIONS_ERROR";

/** locations grouped the way the browse screen renders them */
export interface FilteredLocations {
  bestMatches: ConnectLocationInfo[];
  promoted: ConnectLocationInfo[];
  countries: ConnectLocationInfo[];
  regions: ConnectLocationInfo[];
  cities: ConnectLocationInfo[];
  devices: ConnectLocationInfo[];
}

/**
 * LocationsViewController — the grouped/promoted location browse with a live
 * filter and load state.
 */
export interface LocationsViewController {
  close(): void;
  start(): void;
  stop(): void;

  getFilteredLocations(): FilteredLocations | null;
  getFilteredLocationState(): FilterLocationsState;
  filterLocations(filter: string): void;

  addFilteredLocationsListener(
    cb: (locations: FilteredLocations | null, state: FilterLocationsState) => void,
  ): Unsubscribe;
}

/** one client/device on the network, with live connection state */
export interface NetworkClientInfo {
  clientId?: string;
  deviceId?: string;
  deviceName: string;
  deviceSpec: string;
  deviceDescription: string;
  provideMode: number;
  connectionCount: number;
  connected: boolean;
  createTimeUnixMillis?: number;
}

/**
 * DevicesViewController — the network's clients/devices, live. The listener is
 * fired with the current list.
 */
export interface DevicesViewController {
  close(): void;
  start(): void;
  stop(): void;

  addNetworkClientsListener(cb: (clients: NetworkClientInfo[]) => void): Unsubscribe;
}

/**
 * Custom DNS resolver settings — DoH/plain-DNS toggles and the per-family server
 * lists, matching the native DNS editor. `enableFallback` races a handicapped
 * host-network resolver during tunnel startup.
 */
export interface DnsResolverSettings {
  enableRemoteDoh: boolean;
  enableLocalDoh: boolean;
  enableRemoteDns: boolean;
  enableLocalDns: boolean;
  enableFallback: boolean;

  remoteDohUrlsIpv4: string[];
  remoteDohUrlsIpv6: string[];
  localDohUrlsIpv4: string[];
  localDohUrlsIpv6: string[];
  remoteDnsIpv4: string[];
  remoteDnsIpv6: string[];
  localDnsIpv4: string[];
  localDnsIpv6: string[];
}

/**
 * Options for building a DeviceRemote against a hosted device.
 *
 * `proxyUrl` is the proxy host's api base (e.g. https://api.<proxyHost>:<port>);
 * the sdk converts it to wss and appends /device-rpc. `signedProxyId` is the
 * device's signed proxy id — the device-rpc credential (NOT a jwt), which the
 * platform returns as `auth_token` from /network/auth-client. `byJwt` is the
 * network member jwt used for the network-space api.
 */
export interface PlatformDeviceRemoteOptions {
  apiUrl: string;
  platformUrl: string;
  byJwt: string;
  proxyUrl: string;
  signedProxyId: string;
}
