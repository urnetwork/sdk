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
  /** the bare id, exactly one of these is set (a country/region/city, a
   * location group, or a device) */
  locationId?: string;
  locationGroupId?: string;
  clientId?: string;
  bestAvailable?: boolean;
  name: string;
  locationType: string;
  countryCode: string;
  providerCount: number;
  /** the dot color from the sdk palette (hex, no "#"); "" for best available */
  colorHex: string;
}

export interface NetworkPeerInfo {
  clientId?: string;
  provideEnabled: boolean;
  principal: string;
  deviceName: string;
  deviceSpec: string;
  roles: string[];
  /** the stable per-client dot color from the sdk palette (hex, no "#") */
  colorHex: string;
}

export interface NetworkPeersInfo {
  connected: NetworkPeerInfo[];
  disconnectedCount: number;
}

/**
 * One currently connected (routing-eligible) provider and where it is.
 *
 * `clientId` is the provider's EGRESS client id — the id that identifies it to
 * the user, and the one `removeConnectedProvider` takes.
 *
 * The `has*` flags are meaningful state, not just null-guards: `hasLocation` is
 * false for the user's own fixed peers and for restored window identities, and
 * 0,0 is a valid coordinate. Plot `city*` when `hasCityCoordinates`, else
 * `region*` when `hasRegionCoordinates`, else do not plot.
 *
 * `connectedSinceMillis` is an absolute unix-millis stamp taken on the device
 * hosting the connection (0 when unknown); derive the duration locally rather
 * than expecting it to tick.
 */
export interface ConnectedProviderLocationInfo {
  clientId?: string;
  country: string;
  countryCode: string;
  region: string;
  city: string;
  regionLat: number;
  regionLon: number;
  cityLat: number;
  cityLon: number;
  hasLocation: boolean;
  hasRegionCoordinates: boolean;
  hasCityCoordinates: boolean;
  connectedSinceMillis: number;
  /** the dot color from the sdk palette (hex, no "#"): the country's when the
   * location is known, else the stable per-client color */
  colorHex: string;
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
  /** Last explicit RPC sync refusal; empty while pending or after success. */
  getSyncError(): string;
  /** A random tag of 1–3 distinct emoji to prefill the emoji-tag editor with; count 0 or omitted picks the length at random. */
  suggestEmojiTag(count?: number): string;

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
  /**
   * The explicit "connect to this" action. Unlike setConnectLocation, this
   * rebuilds the connection even when the location is already the installed
   * destination — a new multi client and a fresh set of peers — so choosing the
   * location you are already on reconnects instead of doing nothing.
   */
  reconnect(location: ConnectLocationSpec | null): void;
  removeDestination(): void;
  shuffle(): void;
  getConnectEnabled(): boolean;

  // peers
  getNetworkPeers(): NetworkPeersInfo | null;

  // connected provider locations, sorted oldest-connected first. The
  // provider-locations screen renders ProviderLocationsViewController
  // .getProviderLocations() instead, which is the same window in display order;
  // this raw order is what an "oldest connected provider" consumer wants. While
  // the rpc is down the last readable list is retained rather than drained, so
  // pair an empty result with getRemoteConnected before showing "none".
  getConnectedProviderLocations(): ConnectedProviderLocationInfo[];
  // drop a provider and stop it being re-discovered for the rest of this
  // connection. Takes the egress client id
  removeConnectedProvider(clientId: string): void;

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
  /** signal only — re-read getConnectedProviderLocations */
  addConnectedProviderLocationChangeListener(cb: () => void): Unsubscribe;

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
  openPointsLeaderboardViewController(): PointsLeaderboardViewController;
  openPeerViewController(): PeerViewController;
  openProviderLocationsViewController(): ProviderLocationsViewController;
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
 * PointsLeaderboardRow — one ranked network on the all-time points
 * leaderboard (android/POINTSLEADERBOARD.md), in the server's snake_case with
 * the sdk's preformatted text beside the raw values. `display_name` is the
 * network name, or "" when `anonymous` — render your localized "Anonymous";
 * `emoji_tag` shows either way. Ranks are competition ranks (0 = not ranked).
 */
export interface PointsLeaderboardRow {
  network_id: string;
  network_name?: string;
  emoji_tag?: string;
  anonymous: boolean;
  total_points: number;
  blocks_with_points: number;
  streak: number;
  longest_streak: number;
  rank_points: number;
  rank_blocks: number;
  rank_streak: number;
  display_name?: string;
  total_points_text?: string;
  blocks_with_points_text?: string;
  streak_text?: string;
  longest_streak_text?: string;
  rank_points_text?: string;
  rank_blocks_text?: string;
  rank_streak_text?: string;
}

/** The caller's own row plus its opt-in state. */
export interface PointsLeaderboardMe extends PointsLeaderboardRow {
  points_leaderboard_public: boolean;
}

export type PointsLeaderboardSort = "points" | "blocks" | "streak";

/**
 * EmojiTagValidation — validateEmojiTag's verdict: `normalized` is the tag to
 * send; `reason` is "" | "empty" | "too_many" | "not_emoji" (localize by it,
 * `message` is the English fallback).
 */
export interface EmojiTagValidation {
  ok: boolean;
  count: number;
  normalized: string;
  reason: "" | "empty" | "too_many" | "not_emoji";
  message: string;
}

/**
 * PointsLeaderboardViewController — the all-time points leaderboard. It owns
 * the sort, the pages and the paging state; render `getRows()` in order and
 * call `loadMore()` when the list nears its end. The listener fires on every
 * state change (a page landed, loading toggled, the sort switched, an error,
 * `me` updated); read the state back through the getters. `refresh()` keeps
 * the rows until the new first page lands. Never sort, rank or page yourself.
 */
export interface PointsLeaderboardViewController {
  close(): void;
  start(): void;
  stop(): void;

  getSort(): PointsLeaderboardSort;
  setSort(sort: PointsLeaderboardSort): void;
  loadMore(): void;
  refresh(): void;

  getRows(): PointsLeaderboardRow[];
  getRowCount(): number;
  isLoading(): boolean;
  isEndReached(): boolean;
  getMe(): PointsLeaderboardMe | null;
  getErrorMessage(): string;
  getTotalRanked(): number;
  getLatestEpoch(): number;
  getSnapshotTime(): string | null;

  addPointsLeaderboardListener(cb: () => void): Unsubscribe;
}

/**
 * ProviderLocationsViewController — the provider-locations screen's display
 * order, selection and scroll wheel, shared by every URnetwork app so they all
 * read and traverse the globe identically.
 *
 * `getProviderLocations` is the connected providers in DISPLAY ORDER: the ones
 * with coordinates west to east relative to their centroid — so a cluster
 * straddling the antimeridian stays contiguous — then the ones without. It is
 * the list to render, and it is the order `stepSelection` walks; re-read it on
 * the device's connectedProviderLocationsChanged. (The device's own
 * getConnectedProviderLocations is the same window sorted by connected
 * duration.)
 *
 * The wheel is the plottable head of that order, and `stepSelection` CLAMPS at
 * its ends: stepping past the extreme west or east sticks there rather than
 * cycling round the globe.
 *
 * The selection always points at a connected provider: the longest connected
 * one by default, and when the selected provider leaves the window (removed,
 * or rotated out) the NEAREST remaining one. `getSelectedClientId` is "" only
 * when no providers are connected.
 */
export interface ProviderLocationsViewController {
  close(): void;
  start(): void;
  stop(): void;

  /** the connected providers in display order (west to east, then unplottable) */
  getProviderLocations(): ConnectedProviderLocationInfo[];
  /** the selected provider's egress client id, "" when none are connected */
  getSelectedClientId(): string;
  /** select explicitly (a dot tap or a list row); "" falls back to the default */
  setSelectedClientId(clientId: string): void;
  /** move `steps` providers along the wheel, positive east, clamped at the ends */
  stepSelection(steps: number): void;
  /** drop the provider, moving the selection to the nearest one if it was selected */
  removeProvider(clientId: string): void;

  addSelectedProviderLocationChangeListener(cb: () => void): Unsubscribe;
}

/**
 * Custom DNS resolver settings — DoH/plain-DNS toggles and the per-family server
 * lists, matching the native DNS editor. `enableFallback` races a handicapped
 * host-network resolver during tunnel startup. `dnsUpgradeMaskAddress` is the
 * plain-DNS stand-in advertised while UpgradeMux intercepts port 53; it is not
 * an upstream resolver.
 */
export interface DnsResolverSettings {
  enableRemoteDoh: boolean;
  enableLocalDoh: boolean;
  enableRemoteDns: boolean;
  enableLocalDns: boolean;
  enableFallback: boolean;
  dnsUpgradeMaskAddress: string;

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
 * network member jwt used for the network-space api. `instanceId` must be the
 * hosted DeviceLocal instance returned as `instance_id` by /network/auth-client.
 */
export interface PlatformDeviceRemoteOptions {
  apiUrl: string;
  platformUrl: string;
  byJwt: string;
  proxyUrl: string;
  signedProxyId: string;
  instanceId: string;
}

/** Callbacks consumed by an SDK device-rpc byte transport. */
export interface DeviceRpcTransportCallbacks {
  opened(): void;
  message(frame: Uint8Array): void;
  closed(reason?: string): void;
}

/** One logical connection returned by a device-rpc transport. */
export interface DeviceRpcTransportConnection {
  send(frame: Uint8Array): void;
  close(): void;
}

/**
 * Opaque byte transport used by DeviceRemote. The SDK continues to own RPC
 * framing and all Device behavior; the implementation owns the actual socket.
 */
export interface DeviceRpcTransport {
  open(callbacks: DeviceRpcTransportCallbacks): DeviceRpcTransportConnection;
}

/** Options for the extension-routed DeviceRemote used by ur.io. */
/** api-only LocationsViewController: the same browse controller a device
 * exposes, opened over the network space api for a host with no device yet */
export interface LocationsViewControllerOptions {
  apiUrl: string;
  platformUrl: string;
  byJwt: string;
}

/**
 * PeerViewController — the connectable peers: ONLY the connected peers that
 * provide (the sdk's filter, shared by every app). `getConnectedCount` is all
 * connected peers, providing or not ("N devices online"). The listener
 * delivers the current connectable list.
 */
export interface PeerViewController {
  close(): void;
  start(): void;
  stop(): void;

  getPeers(): NetworkPeerInfo[];
  getPeerCount(): number;
  getConnectedCount(): number;

  addPeersListener(cb: (peers: NetworkPeerInfo[]) => void): Unsubscribe;
}

/** AccountPreferencesViewController — the product-updates preference. */
export interface AccountPreferencesViewController {
  close(): void;
  start(): void;
  stop(): void;

  getAllowProductUpdates(): boolean;
  updateAllowProductUpdates(allow: boolean): void;
  addAllowProductUpdatesListener(cb: (allow: boolean) => void): Unsubscribe;
}

/** the profile, through the sdk NetworkUser's json tags */
export interface NetworkUserInfo {
  userId?: string;
  user_name: string;
  user_auth?: string;
  verified: boolean;
  auth_type: string;
  network_name: string;
  wallet_address?: string;
  auth_types?: string[];
}

/**
 * NetworkUserViewController — the profile: fetch, the cached user, rename with
 * success / error / in-flight listeners.
 */
export interface NetworkUserViewController {
  close(): void;
  start(): void;
  stop(): void;

  fetchNetworkUser(): void;
  getNetworkUser(): NetworkUserInfo | null;
  updateNetworkUser(networkName: string): void;

  addNetworkUserListener(cb: () => void): Unsubscribe;
  addIsLoadingListener(cb: (loading: boolean) => void): Unsubscribe;
  addNetworkUserUpdateErrorListener(cb: (message: string) => void): Unsubscribe;
  addNetworkUserUpdateSuccessListener(cb: () => void): Unsubscribe;
  addIsUpdatingListener(cb: (updating: boolean) => void): Unsubscribe;
}

/** FeedbackViewController — send feedback (message + star count). */
export interface FeedbackViewController {
  close(): void;
  start(): void;
  stop(): void;

  sendFeedback(message: string, starCount: number): void;
  addIsSendingFeedbackListener(cb: (sending: boolean) => void): Unsubscribe;
}

/** the referral code result through its json tags */
export interface ReferralCodeInfo {
  referral_code?: string;
  total_referrals: number;
  max_referrals: number;
  bonus_per_referral_bytes: number;
  referred_bonus_bytes: number;
  bonus_period_seconds: number;
  error?: { message: string };
}

/**
 * ReferralCodeViewController — the network's referral code and its terms.
 * `getReferralCode` is null until the first fetch lands; the listener carries
 * the code string.
 */
export interface ReferralCodeViewController {
  close(): void;
  start(): void;
  stop(): void;

  getReferralCode(): ReferralCodeInfo | null;
  addReferralCodeListener(cb: (code: string) => void): Unsubscribe;
}

export interface SubscriptionInfo {
  subscription_id?: string;
  store: string;
  plan: string;
}

export type PurchaseConfirmationState =
  | "idle"
  | "waiting_for_confirmation"
  | "confirmed"
  | "confirmation_gave_up";

/**
 * SubscriptionBalanceViewController — balance, plan and the purchase
 * confirmation state machine (background poll, confirmation poll with a
 * budget that pauses while backgrounded, jwt reconciliation). Byte counts
 * are numbers; the platform owns the jwt refresh and calls `jwtRefreshed`.
 */
export interface SubscriptionBalanceViewController {
  close(): void;
  start(): void;
  stop(): void;

  getIsPro(): boolean;
  getIsGuest(): boolean;
  getIsLoaded(): boolean;
  getStartBalanceByteCount(): number;
  getAvailableByteCount(): number;
  getPendingByteCount(): number;
  getUsedBalanceByteCount(): number;
  getCurrentSubscription(): SubscriptionInfo | null;
  getSubscriptions(): SubscriptionInfo[];
  getCurrentStore(): string;
  getPurchaseConfirmationState(): PurchaseConfirmationState;
  getConfirmationBudgetRemainingMillis(): number;

  refresh(): void;
  setForeground(foreground: boolean): void;
  startPurchaseConfirmation(): void;
  clearPurchaseConfirmation(): void;
  jwtRefreshed(): void;

  getBackgroundPollIntervalMillis(): number;
  setBackgroundPollIntervalMillis(millis: number): void;
  getConfirmationPollIntervalMillis(): number;
  setConfirmationPollIntervalMillis(millis: number): void;
  getConfirmationBudgetMillis(): number;
  setConfirmationBudgetMillis(millis: number): void;

  addSubscriptionBalanceChangeListener(cb: () => void): Unsubscribe;
  addSubscriptionJwtOutOfSyncListener(cb: (serverIsPro: boolean) => void): Unsubscribe;
  addPurchaseConfirmationListener(cb: (state: PurchaseConfirmationState) => void): Unsubscribe;
}

export interface AccountHostOptions {
  apiUrl: string;
  platformUrl: string;
  byJwt: string;
}

/**
 * AccountHost — the sdk for a signed-in page with no device: the network space
 * api plus the api-only view controllers the account screens are built on.
 * Openers return the same objects a DeviceRemote's openers return. The api
 * methods resolve with the sdk result through its json tags (the API's own
 * snake_case field names) and reject with an Error carrying the sdk message.
 * `close()` releases the host; close the controllers it opened first.
 */
export interface AccountHost {
  setByJwt(byJwt: string): void;
  getByJwt(): string;
  close(): void;

  openLocationsViewController(): LocationsViewController;
  openDevicesViewController(): DevicesViewController;
  openAccountPreferencesViewController(): AccountPreferencesViewController;
  openNetworkUserViewController(): NetworkUserViewController;
  openFeedbackViewController(): FeedbackViewController;
  openReferralCodeViewController(): ReferralCodeViewController;
  openSubscriptionBalanceViewController(): SubscriptionBalanceViewController;
  openPointsLeaderboardViewController(): PointsLeaderboardViewController;

  getNetworkClients(): Promise<any>;
  removeNetworkClient(clientId: string): Promise<any>;
  getNetworkReferralCode(): Promise<ReferralCodeInfo>;
  validateReferralCode(code: string): Promise<any>;
  setNetworkReferral(code: string): Promise<any>;
  getReferralNetwork(): Promise<any>;
  unlinkReferralNetwork(): Promise<any>;
  authCodeCreate(uses: number, durationMinutes: number): Promise<any>;
  networkDelete(): Promise<any>;
  getLeaderboard(): Promise<any>;
  /** One page of the all-time points leaderboard (public; the jwt only adds `me`). */
  getPointsLeaderboard(sort: PointsLeaderboardSort, cursor?: string, limit?: number): Promise<any>;
  setPointsLeaderboardPublic(isPublic: boolean): Promise<any>;
  /** Validate with validateEmojiTag first and send `normalized`; "" clears the tag. */
  setEmojiTag(emojiTag: string): Promise<any>;
  validateEmojiTag(emojiTag: string): EmojiTagValidation;
  /** A random tag of 1–3 distinct emoji to prefill the editor with; count 0 or omitted picks the length at random. */
  suggestEmojiTag(count?: number): string;
  getNetworkLeaderboardRanking(): Promise<any>;
  setNetworkLeaderboardPublic(isPublic: boolean): Promise<any>;
  getNetworkReliability(): Promise<any>;
  getNetworkRedeemedBalanceCodes(): Promise<any>;
  redeemBalanceCode(secret: string): Promise<any>;
  checkBalanceCode(secret: string): Promise<any>;
  subscriptionBalance(): Promise<any>;
  getNetworkUser(): Promise<any>;
}

export interface ExtensionDeviceRemoteOptions {
  apiUrl: string;
  platformUrl: string;
  byJwt: string;
  instanceId: string;
  transport: DeviceRpcTransport;
}
