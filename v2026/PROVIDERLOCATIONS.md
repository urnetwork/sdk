# Provider Locations

Design document. Status: research complete, all decisions resolved, and implementation
landed (2026-08-04) — see "As built" at the end for what shipped and what deviates.
Companion reference:
`~/urnetwork/android/MOCKLOCATION.md` (full Android mock-location research with AOSP/doc
citations).

Thread per-provider location metadata from the server through `FindProviders2`, the connect
multi-client window, and the sdk `Device`, and surface it in the apps as a provider-locations
detail view (globe + list), with an Android-only option to sync the OS mock location to the
oldest connected provider.

## Requirements (as specified)

1. **Server/API**: `FindProviders2` results carry, per provider:
   `(country, region, city, region lat, region lon, city lat, city lon)`.
   Update the server and the spec at `connect/api/bringyour.yml`.
2. **SDK**: `Device` exposes `GetProviderLocations` returning, for each connected provider in
   the window: `(client id, country, region, city, region lat, region lon, city lat, city lon,
   duration connected in window)`. Add a change listener.
3. **Apps**: tapping the "Connected to X providers" label opens a provider-locations detail
   popup. Android first; port to other platforms after.
4. **Detail view**:
   - Top: white wireframe globe on dark fill, visually matching the globe on ur.io `/ip`.
   - Each provider is a dot in its country color. The selected provider gets a ring around its
     dot in the same color.
   - Below: providers listed descending by connected duration. Each row: client id on top,
     country-color dot at left, "city, region" and "lat, lon" stacked at right.
   - Tapping a row selects it in both list and globe and spins/centers the globe on that dot.
     Selection stays in sync between globe and list.
   - Tapping the client id copies it (existing app convention).
5. **Android only**: at the top of the detail view, a toggle "sync device location with oldest
   provider", with an info icon opening a guide for setting the app as the Android mock
   location provider (enable developer options → select app as mock location app). Add a
   preferences deep link in the app settings section that opens the relevant OS settings.
   *(Copy revised 2026-08-05, user request: label is now "Sync device location with
   provider" — key `sync_device_location_with_provider` — with a persistent note below it
   reading "Use most stable provider" (`use_most_stable_provider`) that doubles as the
   status line, e.g. "Waiting for a provider location" while the toggle is on with no
   located provider yet. Selection semantics unchanged — oldest-connected is the
   stability proxy. The old key `sync_device_location_with_oldest_provider` is retired.
   Applies to every platform that ships the toggle: Android, Linux, extension.)*
6. **Mock location behavior**: when the toggle is on, the device location is the oldest
   connected provider's lat/lon. When off, the real device location (excluding mock) flows
   through unchanged. *(Research note: "flows through" is achieved by removing the test
   providers — the OS restores real providers natively. Actively mirroring real location
   through the mock provider is structurally impossible; see D-MOCKOFF.)*

Out of scope for this phase: iOS / desktop UI ports (the sdk surface is designed so they only
need UI work), any non-Android mock-location analog.

## Research findings

### connect (transport library) — done

- `FindProviders2` structs live at `connect/api.go:330-357` (`FindProviders2Args`,
  `FindProviders2Result`, `FindProvidersProvider{ClientId, EstimatedBytesPerSecond,
  HasEstimatedBytesPerSecond, Tier, IntermediaryIds}`). Responses are decoded with plain
  `json.Unmarshal`, and the spec's `info.description` explicitly licenses additive response
  fields — adding a field is compatible in both directions.
- Spec: `connect/api/bringyour.yml:654` (path) and `:2902` (schemas). The result item is
  inlined. Existing vocabulary to reuse: `FindLocationsResult` (`city`, `city_location_id`,
  `region`, `region_location_id`, `country`, `country_location_id`, `country_code`) and
  `MyIPInfoResult` `coordinates {lat, lon}` — the only lat/lon in the spec today. Known
  drift: spec lists `force_count`/`force_minimum` that the Go args lack.
- **Single consumer**: `ip_remote_multi_client_api.go:256` collapses the response into
  `map[MultiHopId]DestinationStats` — `DestinationStats{EstimatedBytesPerSecond, Tier}`
  (`ip_remote_multi_client.go:61`) is the only surviving metadata carrier, already embedded
  in `multiClientChannelArgs` and reachable at every monitor emission point. It is only ever
  a map value → adding a pointer field does not break comparability anywhere.
- **Id semantics (critical)**: `ProviderEvent.ClientId` is the *local ephemeral window
  client id* minted per window client, NOT the provider's client id. The provider (egress)
  client id is `Destination.Tail()`. The detail view must display/copy the provider's id, so
  it must be threaded alongside location.
- Monitor surface (`ip_remote_multi_client_monitor.go:10-86`):
  `MonitorEventFunction(windowExpandEvent, providerEvents map[Id]*ProviderEvent, reset)`;
  states `InEvaluation → EvaluationFailed | Added → Removed` (`NotAdded` defined, never
  emitted). Both `EventTime` fields exist but are commented out. The monitor deletes entries
  on terminal states, so the live map is exactly the currently-connected set.
  `WindowExpandEvent` is `!=`-compared → must stay comparable (no new fields with slices);
  `ProviderEvent` is pointer-held and shallow-cloned → safe to extend with immutable pointer
  fields. `AddProviderEvent(clientId, state)` has 5 production call sites, all with
  `multiClientChannelArgs` (and thus `DestinationStats` + `Destination`) in hand, plus one
  stall test.
- Two destination sources bypass FindProviders2 and will have **nil location**: fixed
  client-id specs (`ip_remote_multi_client_api.go:223` — user's own peers) and restored
  persisted identities (`:253`). Optional follow-up: persist location in
  `WindowClientIdentity` for restore continuity.
- sdk consumption today: `device_local.go:3393` subscribes the merged monitor and reduces to
  a comparable `WindowStatus` count struct (`device.go:293` — must stay comparable; do not
  hang the list off it). Per-provider UI surface is `ConnectGrid` /
  `ProviderGridPoint{X, Y, ClientId, State, EndTime, Active}`
  (`connect_view_controller.go:408`).
- `DeviceRemote` bridges the *same monitor events* over net/rpc with gob
  (`device_rpc.go:4701`, `:5967`). Gob tolerates added exported struct fields in both
  directions → additive `ProviderEvent` fields (location, event time, provider client id)
  ride the existing envelope with **zero rpc protocol changes**.
- Window semantics: "connected duration" = now − the `ProviderStateAdded` emission for that
  window client (ping-acked and routing-eligible; matches what the sdk already counts as
  connected). `multiClientChannel.createTime` is pre-ping (wrong), traffic-based
  `clientDuration` starts at first packet (wrong). Destination change = full window+monitor
  teardown (durations reset). `NetworkChanged()` re-dials transports in place — the window
  and durations survive brief network changes. `MaxClientLifetime` defaults to 60 min, so
  durations top out there. Same-id replacement re-emits `Added` → duration correctly
  restarts.

### ur.io /ip globe — done

- Implementation: **D3 + SVG orthographic projection** (not three.js/cobe), component
  `mmm/ur.io/react/src/components/ip/Globe.jsx`, mounted from `pages/MyIpInfo.jsx`. Flat
  fills, no lighting/atmosphere on /ip. The "wireframe" look = dark `#101010` sphere disc,
  **filled** `#F8F8F8` land polygons with 0.3px `#101010` country-border strokes, and a 10°
  graticule (`#CCCCCC` at alpha 0.376, 0.5px, drawn on top of land, lat lines clipped ±80°).
  Web wraps it in a white card; the Android sheet will sit it on the app dark theme instead.
- Geometry: 600×600 viewBox; mount scale 300 (sphere touches edges) → settles at 420
  (1.4×, crops at frame). Orthographic with `clipAngle 90`; backface cull at
  `geoDistance(p, center) ≥ π/2`. Drag sensitivity `k = 600/scale/(3π)` **degrees** per
  virtual px = 0.2122°/px at scale 300 (an earlier note in this doc said 0.707°/px —
  wrong; the value was re-derived from `Globe.jsx` and verified in the port's tests). No
  roll axis; **no auto-rotate** — only the recenter animation.
- Recenter animation: rotate lerp to `[-lon, -lat]`, 2000 ms cubic-in-out on mount then
  2000 ms zoom; 1000 ms variant on pointer-leave. (Web lerps naively; Android should add
  shortest-path longitude wrapping + a no-op guard, as `CountryGlobe.jsx` does.)
- Provider dots: solid country color, **no stroke/alpha/glow**, radius 4–9 (in 600-space),
  normalized per-country by provider count, drawn sorted descending by radius so small dots
  stay on top. Web fallback color for unknown country: `#0099FF`. "You" marker: `#87FB67`
  r6 with 1px `#101010` stroke. Selection-ring precedent (CountryGlobe): r12 ring stroke in
  the dot color at ~1px + solid dot.
- **Country → color already lives in this sdk**: `locations_view_controller.go:381`
  `GetColorHex(code)` — fixed 67-entry palette keyed by lowercase ISO-2; miss ⇒ mix two
  palette entries chosen by `md5(code)[0] % 67` / `md5(code+"salt")[0] % 67` over sorted
  keys, integer-average RGB. The web mirrors it (`locationColor.js`) and the Android app
  already calls the sdk version (`LocationsListViewModel.kt`, `ConnectStatsSections.kt`).
  **No port needed — the app calls `GetColorHex(countryCode)`.**
- Land data: vendored `world-110m.json` (~100 KB quantized TopoJSON, Natural Earth 110m,
  public domain; 177 country polygons + a single `land` object). Android can ship it as an
  asset with a small decoder (dequantize + delta-decode arcs, `~i` reversal), or pre-bake to
  packed floats at build time.
- Bonus fact: the web globe's data feed `GET /stats/providers-map` returns
  `{countryCode: {region: {provider_count, lat, lon}}}` — the server already computes
  **region-level lat/lon** somewhere in the stats pipeline.

### server — done

- Route `POST /network/find-providers2` (`server/api/api.go:64`) →
  `model.FindProviders2` (`model/network_client_location_model.go:2924`). **The request
  path issues zero SQL**: mmdb lookup for the caller's country (cache-key only) + redis
  GETs of gob-encoded `[]*ClientScore` samples + weighted select + a `HIncrBy` stats
  pipeline. It's a hot path with a staleness canary and warmup gating — do not add a
  Postgres query to it.
- The score pool is written by the `UpdateClientScores` batch task (TTL 5h, ~2,940
  serialized copies of each provider's `ClientScore` across cache key permutations, ×~4 via
  nested lookback scores). **Its SQL already selects
  `city/region/country_location_id` per client from `network_client_location_reliability`
  (`:2383-2410`) and discards them** (map keys only). `network_client_location_reliability`
  guarantees each valid provider has exactly one unambiguous (city, region, country) —
  ambiguous clients are excluded from the pool.
- `location` table has **no lat/lon columns** (`db_migrations.go:974`). The Go `Location`
  struct has `Latitude/Longitude` filled from the mmdb (`IpInfo`) at connect time
  (`controller/network_client_location_controller.go:30`), and `CreateLocation` silently
  drops them — city-centroid lat/lon reaches the server on every connect and is thrown
  away.
- **Region lat/lon already solved**: embedded `model/region_centroids.json` (358 KB — 244
  country centroids, 12,354 region keys) with `centroidFor(countryCode, regionName)`
  (`model/providers_map_model.go:40`); it's what powers `/stats/providers-map` today. No
  equivalent city dataset exists.
- No location API exposes lat/lon today; `LocationResult`/`ConnectLocation` are the
  naming precedent (`country_code`, `*_location_id`, names). `/stats/providers-map` and
  `/my-ip-info` are the only lat/lon emitters (`lat`/`lon` field naming).
- Cheapest correct server plan (per findings):
  1. Add `CityLocationId/RegionLocationId/CountryLocationId server.Id` to `ClientScore`,
     populated in `loadClientScore` (values already scanned); set only on the top-level
     score, not the nested lookback copies (halves the redis cost; ~48 bytes each). **No
     strings or floats in `ClientScore`** — redis amplification is ~2,940×4 copies per
     provider. Gob tolerates old blobs (zero ids → "no location" for ≤5h during rollout).
  2. Process-local location directory `map[locationId]→{name, type, country_code, parent
     ids, lat/lon}` via `sync.OnceValue` + `server.OnWarmup`/`OnReset`, mirroring the
     existing `countryCodeLocationIds()` pattern; resolve ids → names at response build
     (~4 map lookups per returned provider, zero I/O).
  3. Region lat/lon from `centroidFor(...)`. City lat/lon: `ALTER TABLE location ADD
     latitude double precision NULL, longitude double precision NULL`, populate in the
     city INSERT of `CreateLocation` (source already on the struct) + self-heal UPDATE on
     the existing-row path when NULL; no backfill job strictly needed (connect churn
     fills it); region centroid as fallback while NULL.
  4. Extend `FindProvidersProvider` with flat `omitempty` fields: `country`, `region`,
     `city`, `country_code`, `region_lat`, `region_lon`, `city_lat`, `city_lon` (+
     optionally the three `*_location_id`s, free and consistent with other APIs). Spec
     conformance test tolerates impl-extra fields; update `bringyour.yml` to stay honest.
- Watch item: `sdk/api.go:706` keeps a *narrower* `FindProvidersProvider` mirror
  ({client_id, estimated_bytes_per_second}) — extend when the sdk needs to read the new
  fields directly. The anonymized stats stream (`FindProviders2Sample`) does not need the
  new fields.

### android app — done

- Label chain: `ConnectStatusIndicator.kt:44` renders plural `connected_provider_count`
  ("Connected to %d providers") inside an inert `Row` — the whole row becomes the tap
  target. Count comes from `ConnectViewModel.windowCurrentSize` ← `ConnectGrid
  .windowCurrentSize` via `ConnectViewController.addGridListener`. The VM dedupes grid
  callbacks with a signature string (sdk re-emits fresh proxies every event) — new
  provider-locations state must follow the same discipline.
- Architecture: Hilt (constructor-injection only, no modules), `@Singleton DeviceManager`
  holds `DeviceLocal` in the app process; **single process** (no `android:process`
  anywhere) — a mock-location engine can be an app-scoped singleton visible to both UI and
  `MainService` (VpnService, FGS type `dataSync`). Lifecycle helpers to mirror:
  `ForegroundDeviceControllerOwner`, `ForegroundWorkOwner`, `DeviceManager
  .addDeviceChangeListener`.
- Settings: hand-built `Column` sections in `SettingsScreen.kt`; the **Connections**
  section (kill switch at `:1012`, blocked-regions nav row at `:1037`) is the natural home
  for the mock-location row. `BatteryOptimizationToggle` (`:1562`) shows the
  re-check-on-`ON_RESUME` pattern — exactly what "is the app currently the selected mock
  app?" needs (no OS callback exists). Per-flavor source-set split (battery optimization
  play no-op) is available if Play needs different behavior. No
  `Settings.ACTION_APPLICATION_DEVELOPMENT_SETTINGS` usage yet.
- Popups: 7 `ModalBottomSheet` precedents — `AddBlockedLocationSheet.kt` is the best model
  (sheet with `LazyColumn` of `CircleImage(backgroundColor = getLocationColor(...))` rows,
  `containerColor = Black`). Sheets are screen-local state, not nav routes.
  `InfoIconWithOverlay.kt` is the existing info-icon popup for requirement 5. Guide-screen
  templates: `IntroductionSettings.kt` (steps + tinted action cards + bottom URButton) or
  `DnsSettingsScreen.kt` (settings sub-screen with top bar); `BulletPoint.kt` for steps.
  New screens = `@Serializable` route in `MainNavViewModel.kt` + `composable<Route.X>`
  block with `NavigationAnimations`.
- Canvas precedent: `ConnectButton.kt` `GridCanvas` (Animatable radius/color per point,
  diff-and-animate against sdk map); ring-around-dot precedent in
  `ContractStatsScreen.kt:685`. Project convention: factor pure logic (projection math,
  oldest-provider selection) into plain Kotlin files with JVM tests (like
  `ConnectSheetGeometry.kt`).
- **Location posture: zero location code, and the manifest actively removes
  `ACCESS_FINE_LOCATION`/`ACCESS_COARSE_LOCATION` with `tools:node="remove"`** to keep
  dependencies from injecting them. `play-services-location` exists only as a CVE pin on
  play/solana/ethos flavors — the github/ungoogle flavor lacks it, so any mock engine must
  use platform `LocationManager` only. A design that never reads real location preserves
  the app's no-location-permission privacy stance.
- Build: minSdk 26, target/compileSdk 36, Kotlin 2.3.21, Compose BOM 2026.05.01, M3 1.4,
  nav 2.9.8. Sdk consumed as prebuilt `sdk/build/android/URnetworkSdk.aar`; **rebuilding
  the aar after Go changes is manual** (`make init build_android`, gomobile
  `-androidapi 24`).
- Colors/theme: dark-only theme, `Black #101010`, `OffWhite #F8F8F8`, `SheetBlack`;
  country color via `LocationsListViewModel.getLocationColor` lambda →
  `locationsVc.getColorHex(...)` (sdk). `CircleImage` renders the color dots.
- i18n: `strings.xml` is **generated** from the `~/urnetwork/localizations` repo
  (`keys/*.yaml` → `npm run gen`); all new strings (guide steps, toggle, sheet title) must
  be added there, 19 locales. Plurals: `pluralStringResource(id, count, count)`.
- Clipboard convention (requirement: tap client id → copy): `ProviderIdentitiesScreen
  .kt:194-207` — monospace text, `.clickable { clipboardManager.setText(...); Toast
  client_id_copied }`; the `client_id_copied` string already exists.
- Secondary entry-point candidate: `OpenProviderListButton` in `ConnectActions.kt:508`
  already shows a globe icon + provider count in the connect drawer.

### sdk — done

- `Device` interface: `device.go:399-650`, implemented by `DeviceLocal` and `DeviceRemote`
  (`device_rpc.go`). Canonical precedent for this feature's shape:
  `GetProviderIdentities() *ProviderIdentityList` + `AddProviderIdentityChangeListener`
  (no-payload listener; consumers re-read the getter). Listeners are single-method
  interfaces stored in `connect.CallbackList`, fired outside the state lock wrapped in
  `connect.HandleError`; `Sub` unsubscribes. List getters return empty, never nil.
- Window plumbing: unexported `device` capability `windowMonitor()` returns the connect
  monitor (multi), a fixed monitor (single client), or empty; comment contract: "get the
  window monitor each time the destination changes". `windowMonitorWithAvailability`
  adds `EventsWithAvailability()` — `ConnectGrid` **freezes rather than drains** when the
  remote rpc is down, and reconciles every 30 s against retained events. `DeviceLocal`
  already maintains an internal monitor subscription per destination
  (`device_local.go:3393`) to derive comparable `WindowStatus` counts.
- **Key consequence**: the connect monitor's retained-events map (after `ProviderEvent`
  gains `Location` + `EventTime`) contains everything `GetProviderLocations` needs — the
  getter can be a **pure read over `windowMonitor().Events()`** (filter `Added`, map,
  compute duration from `EventTime`), and the change listener an adapter over
  `AddMonitorEventCallback`. Because `DeviceRemote` already bridges the same monitor
  events over gob rpc (additive fields ride along), **both device implementations share
  the same derivation with zero new rpc methods** — versus ~17 touchpoints in
  `device_rpc.go` + js/wasm mirrors + mirror tests for a conventional rpc-mirrored
  getter/listener pair.
- Remote availability: three existing getter policies (nil for live stats; state
  fallback; private last-known cache). For a window view the `ConnectGrid`
  freeze-on-unavailable policy is the right match; `GetRemoteConnected` +
  `RemoteChangeListener` is the app-facing gray-out signal (existing pattern).
- Clock note: `EventTime` is stamped on the device hosting the window. A remote viewer
  computing `now − EventTime` inherits cross-device clock skew — acceptable for a coarse
  duration display; document it. Precedent favors **absolute unix-millis int64** fields
  (`ThroughputPoint.Time`, `LastActivityMillis`) over `*Time` — expose
  `ConnectedSinceMillis int64` and let the UI tick duration locally.
- Gomobile: per-type `XList` wrappers embedding `exportedList[T]`
  (`ProviderGridPointList` is the template); no struct composition; pointer-to-struct for
  nullable; `Get*` naming. Constraints doc at `sdk.go:28-42`.
- **Naming collision**: `Api.GetProviderLocations` already exists (`api.go:608`, GET
  `/network/provider-locations` → browseable `FindLocationsResult`). A
  `Device.GetProviderLocations` is technically fine (different receiver) but semantically
  confusable — candidate alternates: `GetConnectedProviderLocations` /
  `GetWindowProviderLocations`.
- cgo: fully generated (`cgo/gen/gen.go`); a new data struct + list auto-classifies as
  json shape, a new listener auto-generates a callback typedef — **no gen.go edits**;
  run `make generate`, review `coverage_report.txt`, `make smoke`. `*Rpc`-named types are
  auto-excluded. Android build gate greps the sources jar for unbindable skips.
- sdk's own `api.go` FindProviders2 mirror is narrower ({client_id,
  estimated_bytes_per_second}) and is *not* the copy the window uses (connect's is) — no
  sdk api change strictly needed.
- Tests to extend: `connect_grid_reconcile_test.go` ships `testing_gridWindowMonitor`
  (fake monitor with availability control) — the right harness for the new derivation;
  `connect_grid_rpc_leak_test.go` (boundedness over rpc), `device_rpc_test.go`
  (`TestDeviceRemoteFull*`, window-monitor merge, last-known patterns),
  `view_controller_lifecycle_test.go` (monitor-subscription leak counting).
- The apple app composes the "Connected to N providers" string itself from
  `grid.getWindowCurrentSize()` — same entry-point change will apply there when porting.

### Android mock-location — done

Full cited report: `~/urnetwork/android/MOCKLOCATION.md`. Load-bearing facts:

- **Eligibility**: declare `android.permission.ACCESS_MOCK_LOCATION` in the manifest — a
  signature-level permission that is never granted; it exists purely as the marker that
  lists the app under Developer options → "Select mock location app". Non-debuggable
  release builds qualify (Surfshark ships exactly this). The real gate is the AppOps op
  `OPSTR_MOCK_LOCATION` (MODE_ALLOWED when selected); self-check with `checkOpNoThrow`,
  watch changes with `startWatchingMode`. No Play policy prohibits mock-location apps.
  Posting mock fixes needs **no location runtime permission and no location FGS type** —
  the existing `dataSync` VPN foreground service suffices.
- **APIs**: `addTestProvider` (`ProviderProperties` on 31+, legacy 10-arg on 26–30;
  always `removeTestProvider` first — pre-S duplicate add throws),
  `setTestProviderEnabled(true)` is mandatory (providers start disallowed — skipping it
  breaks device location instead of faking it), `setTestProviderLocation` with a complete
  Location: provider-name match, lat/lon, accuracy (~8 m), `time`, and
  `elapsedRealtimeNanos` **fresh on every post** (future-dated → rejected).
- **Provider set**: `gps` + `network` everywhere, plus `fused` on API 31+ (platform
  default for `getCurrentLocation`/geofencing since 12). Never `passive` (pre-S throws;
  passive receives forwarded mock fixes automatically). A test provider fully replaces
  the same-named real provider device-wide, and the real GNSS engine is *stopped* while
  mocked — battery cost ≈ zero.
- **Cadence**: re-post ~1 Hz (`getCurrentLocation` has a 30 s freshness rule; GMS FLP
  expects monotonically increasing timestamps). Single handler tick in the VPN process.
- **Mirroring is impossible**: while gps/network/fused are mocked there is no un-mocked
  source left to read — a "pass-through" mirror has nothing to mirror. It would also
  require FINE+BACKGROUND location permissions, the `location` FGS type, Play's location
  policy review, and would stamp `isMock=true` on genuinely-real fixes. Toggle-off =
  `removeTestProvider` ×N: the platform swaps the real providers back in immediately and
  purges mock entries from last-known-location caches.
- **Nothing ever auto-removes test providers** — not process death, not force-stop, not
  uninstall (in-memory in system_server until reboot). A crash while active leaves
  device location frozen at the mock fix. ⇒ mandatory defensive startup cleaner, and an
  ORPHANED state: if the user deselects the app (or disables Developer options) while
  active, the op is revoked *before* we can clean up — `removeTestProvider` throws and
  the providers linger. Recovery: instruct re-select + toggle off (or reboot); auto-clean
  via the op watcher the moment the op returns.
- **Location master switch** (Settings → Location off) silently gates all delivery while
  every API call still succeeds — check `isLocationEnabled()` (28+; `isProviderEnabled`
  fallback on 26–27) and guide to `ACTION_LOCATION_SOURCE_SETTINGS`.
- **Deep links**: there is no public intent for the mock-app picker. Deepest:
  `Settings.ACTION_APPLICATION_DEVELOPMENT_SETTINGS`, plus the undocumented best-effort
  extra `:settings:fragment_args_key = "mock_location_app"` which scrolls-and-highlights
  the row on stock Android (harmless elsewhere); guard with `resolveActivity`. If
  Developer options are disabled (`Settings.Global.DEVELOPMENT_SETTINGS_ENABLED == 0`):
  `ACTION_DEVICE_INFO_SETTINGS` + "tap Build number 7×" instructions (not automatable).
- **GMS FusedLocationProviderClient**: the platform path usually moves it (real gps
  engine stopped; the GMS-registered fused proxy is replaced on 31+), but guaranteed
  coverage uses `FusedLocationProviderClient.setMockMode(true)`/`setMockLocation()` —
  same eligibility, no extra user step, device-global (must `setMockMode(false)` on
  teardown). Only viable on flavors already shipping `play-services-location`
  (play/solana/ethos have it pinned); github/ungoogle stays platform-only.
- **Detectability**: mock fixes are always `isMock=true` by design; banking/rideshare/
  game apps may reject them; Google Maps does not (it's the standard verification step).
  Some detector SDKs flag any app *declaring* `ACCESS_MOCK_LOCATION` — worth a line in
  the disclosure. Android 12 was the only mock-related behavior change; 13–16 changed
  nothing.
- Surfshark's guide structure (settings toggle → explainer popup → open settings →
  select app → verify in Maps) is the UX template; we additionally cover the 7-taps
  step, the master switch, and the orphan-recovery warnings their guide omits.

## Architecture

```
server   network_client_location_reliability ──> UpdateClientScores (batch, 5h TTL)
             adds City/Region/CountryLocationId to ClientScore (redis, ids only)
         FindProviders2 (redis-only hot path)
             + process-local location directory (id -> name/code/parents/lat-lon)
             + region centroids (embedded json)  + city lat/lon (new location columns)
             => providers[].location {country, country_code, region, city,
                                      region_coordinates{lat,lon}, city_coordinates{lat,lon},
                                      *_location_id}
connect  api.FindProvidersProvider.Location  ─┐
         NextDestinations => DestinationStats.Location (single choke point)
         multiClientChannelArgs ──> AddProviderEvent(...)
         ProviderEvent {ClientId, EgressClientId(new), State, Location(new), EventTime(new)}
             retained-events map == currently-connected set
             └── gob rpc bridge to DeviceRemote (additive fields ride existing envelope)
sdk      Device.GetConnectedProviderLocations() = derivation over windowMonitor().Events()
         ConnectedProviderLocationChangeListener = adapter over AddMonitorEventCallback
app      "Connected to N providers" tap -> ProviderLocationsSheet (globe + list [+ toggle])
         MockLocationController (Android only) <- oldest provider with coordinates
```

### 1. Server + spec

- `ClientScore` gains `CityLocationId/RegionLocationId/CountryLocationId server.Id`,
  populated in `loadClientScore` from columns the query already scans. Set only on the
  top-level score (not nested lookback copies). No strings/floats in `ClientScore` — redis
  amplification (~2,940 copies × ~4 nesting per provider) forbids it.
- New process-local location directory (`sync.OnceValue` + `OnWarmup`/`OnReset`, modeled
  on `countryCodeLocationIds()`): `map[locationId] -> {name, type, country_code, parent
  ids, lat/lon}` from one full `location` scan. Response build resolves ids → names with
  ~4 map lookups per returned provider; zero request-path I/O added.
- Region coordinates: existing `centroidFor(countryCode, regionName)` (embedded
  `region_centroids.json`, country-centroid fallback). Region coords are effectively
  always present for valid providers (pool validity requires a country).
- City coordinates: migration `ALTER TABLE location ADD latitude double precision NULL,
  longitude double precision NULL`; populate in the city INSERT of `CreateLocation`
  (values already on `model.Location` from the mmdb) and self-heal existing rows (UPDATE
  when NULL on the existing-row path). No backfill job: connect churn fills it. While
  NULL, fall back to the region centroid for `city_coordinates` — or omit; see D-CITYFB.
- Response: `FindProvidersProvider` gains `Location *ProviderLocation
  json:"location,omitempty"` with a **named schema** (reusable), shaped:
  `{country, country_code, region, city, country_location_id, region_location_id,
  city_location_id, region_coordinates {lat, lon}, city_coordinates {lat, lon}}`.
  Coordinate **sub-objects omitted when unknown** — flat `omitempty` floats would drop
  legitimate 0.0 coordinates. Naming mirrors `MyIPInfoResult.coordinates` + the
  `LocationResult` vocabulary.
- Spec: add the `ProviderLocation` schema + reference from the `FindProviders2Result`
  inlined item in `connect/api/bringyour.yml`; `info.description` already licenses
  additive fields; spec-conformance test tolerates them (update keeps the spec honest).
- Rollout: old redis score blobs (≤5h) decode with zero location ids → providers appear
  location-less until the next `UpdateClientScores` pass; clients must render unknown
  gracefully anyway (fixed/peer destinations), so no cache key bump is needed.

### 2. connect

- New `type ProviderLocation struct` next to `ProviderSpec` in `api.go`, mirroring the
  wire schema (pointer field on `FindProvidersProvider`; nil = server didn't send).
- `DestinationStats` gains `Location *ProviderLocation` — the single choke point already
  embedded in `multiClientChannelArgs`, reachable at every monitor emission point, and
  only ever a map value (comparability unaffected). Populated at the one FindProviders2
  consumption site; the fixed-spec and restored-identity sites stay `DestinationStats{}`
  (nil location). Optional later: persist location in `WindowClientIdentity`.
- `ProviderEvent` gains three fields, all sourced from `multiClientChannelArgs` at the 5
  emission sites (+1 stall test):
  - `Location *ProviderLocation` — treated as immutable after construction (events are
    shallow-cloned).
  - `EventTime time.Time` — un-comment the existing field; stamped in
    `AddProviderEvent`. The `Added` stamp is the connect time; a re-`Added` (same-id
    replacement) correctly restarts it.
  - `EgressClientId Id` — `Destination.Tail()`, the **provider's** client id.
    `ProviderEvent.ClientId` is the local ephemeral window client id and is the wrong id
    to display/copy.
- `AddProviderEvent(clientId, state)` → takes a small args struct (CODESTYLE) or extra
  params. `WindowExpandEvent` untouched (must stay comparable).
- No rpc change: the sdk's `DeviceRemote` monitor bridge gobs `connect.ProviderEvent`
  directly; additive exported fields are tolerated in both directions (old peer ⇒ zero
  values ⇒ unknown location / zero time, handled as below).

### 3. sdk

- Exported element struct (gomobile-flat, no composition):

  ```go
  type ConnectedProviderLocation struct {
      ClientId             *Id     // egress provider client id (Destination tail)
      Country              string
      CountryCode          string  // lowercase; feeds GetColorHex
      Region               string
      City                 string
      RegionLat, RegionLon float64
      CityLat, CityLon     float64
      HasLocation          bool    // false: unknown (fixed/peer dest, old server, rollout gap)
      HasCityCoordinates   bool    // false: city coords not yet known (pre-migration rows)
      ConnectedSinceMillis int64   // unix millis of ProviderStateAdded; UI ticks duration
  }
  ```

  plus `ConnectedProviderLocationList` (exportedList wrapper) and
  `ConnectedProviderLocationChangeListener { ConnectedProviderLocationsChanged() }` —
  no-payload, consumers re-read (ProviderIdentity precedent).
- `Device` interface gains `GetConnectedProviderLocations()` +
  `AddConnectedProviderLocationChangeListener(...) Sub`.
  Getter is a pure derivation over `windowMonitor().Events()`: filter
  `ProviderStateAdded`, map event → struct, sort ascending `ConnectedSinceMillis`
  (= descending duration), return empty-never-nil when disconnected.
- `DeviceLocal`: derive on read; fire the change listeners from the existing internal
  window-monitor callback (`device_local.go:3393`), which already re-subscribes per
  destination change. `DeviceRemote`: same derivation over its bridged
  `windowMonitor()`, using `EventsWithAvailability` — **freeze rather than drain** when
  the rpc is down (ConnectGrid policy); apps gray out via `GetRemoteConnected` (existing
  pattern). Zero new rpc methods/mirrors.
- Duration semantics documented on the getter: measured from ping-acked routing
  eligibility (`Added`); survives network blips (`NetworkChanged` re-dials in place);
  resets on destination change (full window teardown) and on same-id replacement;
  providers rotate out at `MaxClientLifetime` (default 60 min), so durations top out
  around an hour. Remote-viewer clock skew affects displayed durations (absolute stamp,
  viewer clock) — acceptable and documented.
- cgo: `make generate` + coverage-report review + smoke; auto-classification handles the
  new struct/list/listener. gomobile aar: manual `make init build_android` after Go
  changes.

### 4. Android UI

- Entry: make the `ConnectStatusIndicator` row clickable when status == CONNECTED →
  opens the sheet. (Secondary entry later if wanted: connect drawer's provider-list
  button.)
- Container: `ModalBottomSheet` (AddBlockedLocationSheet pattern: `containerColor =
  Black`, top inset padding), screen-local state on the connect screen. Content top →
  bottom: [Android-only sync-toggle row + info icon] → globe card → provider list
  (`LazyColumn`, `indexedLazyListKey`).
- Globe: custom Compose `Canvas` port of the /ip d3 globe per the replication spec
  (dark `#101010` disc, `#F8F8F8` land fill + 0.3 strokes, 10° graticule at
  `#CCCCCC`×0.376, orthographic clipAngle 90, mount scale 300→420 in the 600-unit
  space). Land from `world-110m.json` asset (~100 KB) with a small TopoJSON decoder
  (dequantize + delta-decode + `~i` stitching), decoded once and cached. Projection +
  hit-testing + shortest-path rotation math in a plain Kotlin file with JVM tests
  (project convention). Provider dots: country color via existing
  `getLocationColor(countryCode)`; selected dot gets a ring (r≈12 in 600-space, stroke
  in the same color — CountryGlobe marker precedent). Dot position: city coords when
  `HasCityCoordinates`, else region coords. Providers with `HasLocation == false` are
  listed but not plotted.
- Tap a dot (hit-test projected positions, nearest within touch slop) or a row →
  single selection state in the ViewModel; globe animates rotation to
  `[-lon, -lat]` (1000 ms, cubic in-out, shortest-path wrapping, no-op guard). Drag to
  rotate freely; no auto-spin.
- List rows: `CircleImage` country-color dot left; client id top in monospace —
  tap-to-copy with `client_id_copied` toast (ProviderIdentitiesScreen precedent);
  right column stacked "City, Region" and "lat, lon" (city coords, else region coords,
  else em-dash); sorted by the sdk order (desc duration); duration text ticks from a
  1 s `LaunchedEffect` clock against `ConnectedSinceMillis` (no listener spam).
- ViewModel: subscribe `AddConnectedProviderLocationChangeListener` → re-read
  `GetConnectedProviderLocations` (dedupe by signature like the grid listener); expose
  `StateFlow<List<ProviderLocationRow>>` + selection.
- Strings: all new text lands in `~/urnetwork/localizations` `keys/*.yaml` → `npm run
  gen` (never hand-edit strings.xml).

### 5. Android mock location engine

- **`MockLocationController`** — app-scoped `@Singleton` (single process makes this
  visible to UI and `MainService`), sole owner of all `LocationManager` test-provider
  calls; single `Handler` thread, no locking; exposes `StateFlow<MockLocationState>`.
  State machine:

  ```
  DISABLED → NEEDS_DEV_OPTIONS → NEEDS_SELECTION → NEEDS_LOCATION_ON → ELIGIBLE → ACTIVE
                                                                  (+ ORPHANED, ERROR_TRANSIENT)
  ```

  Eligibility is recomputed on the settings/sheet `ON_RESUME`, on the AppOps
  `startWatchingMode` callback, and on `ACTION_LOCATION_MODE_CHANGED` /
  `PROVIDERS_CHANGED` broadcasts (the BatteryOptimizationToggle re-check pattern).
- **Inputs**: persisted toggle (app preference store); tunnel/connect state (via
  `DeviceManager`); provider locations (Device change listener → pick the **oldest entry
  with coordinates** — first of the sorted list with `HasLocation`, city coords else
  region coords); eligibility signals above.
- **ACTIVE**: register `gps` + `network` (+ `fused` on 31+) with low-power/fine
  `ProviderProperties` (legacy overload on 26–30, remove-before-add always), enable each,
  then 1 Hz post loop: accuracy 8 m, speed/bearing 0, fresh `time` +
  `elapsedRealtimeNanos` each tick. Oldest-provider change = **teleport** (no
  interpolation — expected of a VPN; reset speed/bearing, briefly widen accuracy so
  consumers treat it as a new fix). GMS FLP layer ships in v1 on the play/solana/ethos
  flavors (guarded by `GoogleApiAvailability`; github/ungoogle platform-only):
  `setMockMode(true)` + per-tick `setMockLocation`, `setMockMode(false)` on every
  teardown path including the startup cleaner.
- **Teardown** (toggle off, tunnel down, service destroy, or no located provider →
  back to ELIGIBLE): stop ticker, `removeTestProvider` per name in try/catch. **Startup
  cleaner**: on every process start with the toggle off, best-effort
  `removeTestProvider` ×N to clear leftovers from a killed process. ORPHANED: surface
  recovery instructions ("re-select URnetwork under Developer options and turn the
  feature off, or restart the device"), auto-clean when the op watcher reports the op
  restored.
- **UI**: toggle row at the top of the provider-locations sheet (Android flavor only) +
  `InfoIconWithOverlay` info icon; a row in Settings → Connections (nav to the guide);
  guide screen `Route.MockLocationGuide` (IntroductionSettings/DnsSettingsScreen
  template, `BulletPoint` steps) rendering live state with per-state actions: About
  phone (dev options off) → Developer options intent (+ highlight extra) → Location
  settings (master switch off) → "Ready". Disclosure bullets: device-wide effect; apps
  can detect simulated location and some may refuse to work; turn the feature off before
  deselecting the app, disabling Developer options, or uninstalling.
- **Manifest**: add `ACCESS_MOCK_LOCATION` (all flavors; the existing
  `tools:node="remove"` lines for FINE/COARSE stay — this design never reads real
  location).
- **Testing**: state machine + oldest-provider selection as plain Kotlin with JVM tests
  (project convention); manual matrix on emulators (API 26/30/31/36): activate, verify
  in Maps, provider rotation teleport, toggle-off resume, force-stop → startup cleaner,
  deselect-while-active → orphan recovery, master-switch off.

## Design decisions

Resolved (rationale inline above unless noted):

- **D-WIRE** — nested named `location` schema with coordinate sub-objects (no 0.0
  ambiguity), location ids included for future joins.
- **D-REDIS** — ids-only in `ClientScore`, top-level only; names/coords resolved from a
  process-local directory at response build.
- **D-CITYMIG** — do the 2-column `location` migration now (city coords are an explicit
  requirement); self-heal via connect churn, no backfill job.
- **D-EGRESS** — the displayed/copied id is the **egress provider client id**
  (`Destination.Tail()`), not the window-local client id.
- **D-DURATION** — duration = since `ProviderStateAdded`, stamped in connect
  (`EventTime`), exposed as absolute `ConnectedSinceMillis`; UI ticks locally.
  Consequence: `MaxClientLifetime` (60 min) rotates providers, so the oldest provider —
  and therefore the synced mock location — naturally changes roughly hourly.
- **D-DERIVE** — sdk surface is a pure derivation over monitor events; no new rpc
  methods; DeviceRemote freezes on unavailability (gray-out via `GetRemoteConnected`).
- **D-UNKNOWN** — providers without location (user's own peers, restored identities,
  rollout gap) are listed with a placeholder, omitted from the globe, and skipped by the
  mock-location sync (fall through to the next-oldest with coordinates).
- **D-COLOR** — country color = existing sdk `GetColorHex(country_code)`; unknown-country
  fallback on the globe `#0099FF` (web parity).
- **D-I18N** — all strings via the localizations repo pipeline.
- **D-CITYFB** — when city coords are unknown, server omits `city_coordinates` (honest)
  and clients fall back to region coords for display/plotting; server does not silently
  substitute.

- **D-MOCKOFF** — toggle off (or any teardown) = remove the test providers; the OS
  restores real providers immediately and purges mock last-known fixes — this *is* the
  "pass the actual location through" requirement. Active mirroring is structurally
  impossible (the real providers are stopped while mocked), would require
  FINE+BACKGROUND location + `location` FGS + Play location-policy review, and would
  stamp real fixes as mock. This design keeps the app's zero-location-permission
  posture.
- **D-PROVIDERS** — mock `gps`+`network` (+`fused` on 31+), never `passive`;
  `ProviderProperties` path on 31+, legacy overload on 26–30; remove-before-add always;
  `setTestProviderEnabled(true)` after add; 1 Hz repost with fresh timestamps.
- **D-ORPHAN** — mandatory defensive startup cleaner + ORPHANED state with recovery
  instructions + auto-clean via the AppOps watcher (test providers are never
  auto-removed by the OS).
- **D-TUNNEL** — mock is active only while: toggle on ∧ tunnel up ∧ a located provider
  exists. Tunnel down or no located provider → tear down (never report a city we are
  not exiting through). Oldest-provider change = teleport.
- **D-MANIFEST** — declare `ACCESS_MOCK_LOCATION` on all flavors (never granted; picker
  marker only); disclosure notes that some detector apps flag its mere presence.

Resolved with the user (2026-08-04):

- **D-NAME** — `Device.GetConnectedProviderLocations`, with `ConnectedProviderLocation`,
  `ConnectedProviderLocationList`, and `ConnectedProviderLocationChangeListener` —
  avoids the semantic collision with the existing `Api.GetProviderLocations`
  (browse-able locations).
- **D-SHEET** — modal bottom sheet (AddBlockedLocationSheet pattern), screen-local to
  the connect screen.
- **D-FLP** — v1 includes the GMS `FusedLocationProviderClient` mock layer on the
  play/solana/ethos flavors (artifact already pinned there; guarded by
  `GoogleApiAvailability`); github/ungoogle stays platform-only. Guarantees the
  Google-Maps verification step works on GMS devices.

## Platform ports (apple, web + extension, windows, linux)

**Decided 2026-08-05 (user call): mock location ships on Linux and the extension only.**
Apple, Windows and the ur.io web page get the provider-locations view with no toggle,
no setup guide and none of their strings — the research below shows the OS gives us no
honest mechanism on those three. The two targets that ship it are the two where a real
mechanism exists: Linux via GeoClue's static source (now viable because the Linux
target is a **privileged deb daemon** + AppImage GUI, which settles the open question
flagged at the end of the Linux section), and the extension via an MV3 `world: "MAIN"`
content script (see D-EXTOVERRIDE). Both are opt-in and off by default, and both
disclose honestly what they do and do not cover.

Research in progress. The server/connect/sdk layers are platform-agnostic and already
shipped, so the ports are UI-only — plus a per-platform decision on mock location.
`GetConnectedProviderLocations` / `RemoveConnectedProvider` are on the `Device`
interface, so they are reachable through gomobile (apple), the generated C ABI
(windows, linux) and the wasm bridge (web; the two methods may still need adding to
its marshaller).

### Apple (iOS + macOS) — researched, verdict: mock location NOT SUPPORTED

**The toggle, guide, settings row and all their strings are omitted on Apple.** The
provider-locations view ships as title + globe + list only. No counterpart to
`MockLocationController` / `MockLocationSection` / `MockLocationGuideScreen` is created.

Evidence (not an inference from absence):
- Core Location exposes **no** setter/injection/provider-registration symbol; every API
  is a read-only consumer. Apple's own docs name the only sanctioned simulation channel
  — "You can simulate locations by loading GPX files using the Xcode debugger"
  (`CLLocationSourceInformation.isSimulatedBySoftware`).
- Every simulation mechanism (Simulator, `.gpx`, scheme default location, `Debug >
  Simulate Location` on device) requires an active Xcode debug session and a mounted
  Developer Disk Image — a desktop-tethered developer workflow an app cannot invoke on
  itself.
- The VPN surface grants nothing: `NEPacketTunnelProvider` confers no location
  capability, and Apple DTS states a system extension "is effectively a `launchd`
  daemon and daemons can't get the Core Location privilege" — daemons cannot even
  *read* location, let alone supply it.
- macOS `locationd` has write-side entitlements (`com.apple.locationd.mock_testing`,
  …) but they are **Apple-private**: unsignable by third parties, and using them needs
  SIP/AMFI disabled. Not distributable.
- Precedent: Surfshark ships this feature **Android-only** in their own words; the
  ExpressVPN analogue is a browser extension spoofing the HTML5 API, not the OS.
- **Rejected on purpose:** a VPN *can* shift network-derived location by MITMing
  `gs-loc.apple.com` and rewriting the Wi-Fi-positioning response. It has been built
  and Apple rejected it from TestFlight; it needs a user-installed root CA, doesn't
  affect GNSS, and is precisely the behavior a privacy VPN exists to prevent. Do not
  build this.

Apple port notes: the sdk surface is **already bound and entirely unused in Swift**
(`SdkConnectedProviderLocation`, `…List`, `…ChangeListener`, plus the getter/listener
on both `SdkDeviceRemote` and `SdkDeviceLocal` in the shipped xcframework) — so the
port is pure UI. There is no map/globe anywhere in the Apple app and no MapKit usage,
so the Compose globe is ported to SwiftUI `Canvas`; the pure geometry (projection,
wheel stepping, TopoJSON decode) translates almost line-for-line and should be
XCTest-covered before any view code. Swipe-to-delete already behaves the desired way
on Apple: use `.swipeActions` with `allowsFullSwipe: false` (the existing
`BlockedLocationsView` pattern), which is the same "swipe alone never deletes" rule
Android needed a custom component for.

**Prerequisite, not part of the port:** the committed `Localizable.xcstrings` carries
~61 English-only Xcode-extracted strings that were never imported into the
localizations store, so the first `npm run gen:apple` silently drops them. This is the
same drift class that broke the Android build earlier in this work. Import them before
regenerating — check `git diff …/Localizable.xcstrings | grep '^-'`.

### Linux — researched, verdict: SUPPORTED via the static source (decision made 2026-08-05)

> **Resolved.** The recommendation below was written while the tun-privilege question
> was still open, and it explicitly deferred to that decision ("the Linux
> mock-location decision should be made *after* the tun privilege decision"). That
> decision is now made — Linux ships a **privileged daemon as a deb** alongside the
> AppImage GUI — so route 1 (static source) is the shipping mechanism and route 2
> (NMEA over mDNS) is rejected. Rationale: with a root daemon already present, the
> static source costs almost nothing extra, and unlike NMEA it does **not** advertise
> a fake GPS fix to the whole LAN and it does serve CITY-level consumers. The
> "v1 omits the toggle" recommendation is superseded; the toggle ships on Linux.
> The remaining caveats below are still true and belong in the disclosure copy:
> GeoClue is not installed by default everywhere, KDE has historically bypassed it,
> and consumers that do their own IP geolocation are unaffected.

GeoClue has **no** setter, no writable property, and no plugin/provider interface
(2.x dropped the out-of-process provider model; sources are internal C classes). Its
D-Bus name can only be owned by the `geoclue` user or root. So there are exactly two
mechanisms:

1. **Static source** — `/etc/geolocation` (lat/lon/alt/accuracy), live-monitored via
   `GFileMonitor`, `[static-source] enable=true` already the shipped default. Root
   write. Config paths are compile-time constants with **no user-level override**.
2. **NMEA over mDNS** — publish `_nmea-0183._tcp` via Avahi and serve synthetic
   `$GPGGA`/`$GPRMC`. **No root**: Avahi's system bus policy lets any unprivileged
   process register a service. `[network-nmea] enable=true` is the default. A GGA with
   HDOP ≤ 1 reports 0 m accuracy, which sets GeoClue's *priority lock* — every
   non-priority source is ignored for 30 s, hard-overriding Wi-Fi, IP and static.

**Why v1 should omit the toggle** (recommendation, open for the user's call): the
root-free route (2) **advertises the fake GPS fix to the entire LAN**, where any other
machine can discover and adopt it. For a privacy VPN that is a poor default and a
product decision, not an engineering one. It also misses CITY-only consumers (e.g.
automatic timezone, which never requests EXACT so never starts the source), needs
`avahi-daemon`, and does nothing where GeoClue is absent (not default on Ubuntu) or on
KDE, which historically bypasses GeoClue.

**Measure the free win first.** Mozilla Location Service shut down in June 2024 and
GeoClue 2.7.x ships no default Wi-Fi URL, so on a stock modern system **IP
geolocation is often the only working source** — and that request egresses through the
tunnel. GNOME Maps may already follow the VPN exit with zero code. Test before
building anything.

**Cross-cutting note the Linux research could not make** (it assumed AppImage implies
no install): per `linux/APPIMAGE.md` §1, a VPN AppImage **must** obtain privilege for
`/dev/net/tun` from somewhere — a polkit helper, a setcap'd helper, or a root daemon.
If such a helper exists anyway, the static-source route costs almost nothing extra
(the helper writes `/etc/geolocation` on each provider change with no further
prompting) and is strictly better behaved than the LAN broadcast: no mDNS, no
LAN visibility, and it serves CITY-level consumers too. **So the Linux mock-location
decision should be made *after* the tun privilege decision, not before it.**

### Windows — researched, verdict: mock location NOT SUPPORTED (omit the toggle)

Windows 11 contains an **exact** functional analog of Android's mock-location app —
`Windows.Devices.Geolocation.Provider.GeolocationProvider.SetOverridePosition`, which
Microsoft documents as applying "across all app types that consume Windows geolocation
services". It is unreachable for us, on four independent counts:

1. It is a **Limited Access Feature** — it needs a Microsoft-issued unlock token, and
   LAFs exist precisely to gate features that "may be abused by malicious apps".
2. The token binds to a **Package Family Name**; our app ships unpackaged
   (`WindowsPackageType=None`), so it would first need a sparse package.
3. **Windows 11 24H2-era only** (introduced in build 10.0.23504) — no Windows 10.
4. It requires the user to enable "Allow location override", which ships **off**.

Even granted, `Geocoordinate.IsRemoteSource` lets any app detect the override — the
same detectability as Android's `isFromMockProvider`. And the feature's documented
purpose is the *inverse* of ours: RDP/AVD pushing a remote user's **true** location
into a session. A grant for "report the VPN exit city instead" is implausible.

The consolation prize, `IDefaultLocation::SetReport` ("Default location"), is a real,
writable, documented API — but it is **fallback-only**: Windows consults it only when
it cannot determine a better position, so it silently does nothing on any
Wi-Fi-connected laptop. It also needs admin, is deprecated, and is per-user in a way
that conflicts with our LocalSystem service. **Shipping a toggle that no-ops for most
users is worse than shipping none.**

Windows port notes: the app **already renders "Connected to N providers"**
(`ProviderCountText`, fed by `getGrid().getWindowCurrentSize()`), and
`ConnectedProviderLocation` + the getter/listener are **already in the generated C ABI
and entirely unused by the app** — so this is UI-only work. House pattern is a
`ContentDialog` with imperatively-built `StackPanel` rows (no XAML binding, no
`ItemsSource`); make `ProviderCountText` tappable as the entry point. There is no map
infrastructure and the existing convention is explicitly "solid colors, no flags", so
a globe is not required for parity — coordinates are in the struct if one is wanted
later. Unlike Apple/Linux, the Windows build **is** verifiable from this Mac via the
existing QEMU Windows VM.

### Web (ur.io) + extension — researched

**Web page: ship the view, ship NO toggle — a hard no, not a "later".** A page can
only patch its own realm, which already knows the provider location from the sdk; a
toggle there would be a self-lie with a switch on it. Origin isolation, per-origin
permission grants stored in browser chrome, and Permissions Policy (whose default
`self` can only *deny*, never forge) each independently prevent affecting other sites.

**Extension: MAIN-world only, and defer it.** Use the declarative MV3
`world: "MAIN"` content script (Chrome 111+/Firefox 128+ — our
`strict_min_version` is already 128), never `chrome.debugger` (see D-EXTOVERRIDE).
**The blocker is not the spoof, it's the data**: the extension has no device plane at
all — it depends on the published `@urnetwork/sdk-js` for REST hooks only, the wasm
loader is commented out in `popup/App.tsx`, and `URNetwork.init()` is never called.
It cannot learn the oldest provider's coordinates today; that needs a new bridge verb
or a popup-side `DeviceRemote`, which is a bigger lift than the override itself.
Be honest in any disclosure: Android's is an OS-mediated, device-wide, all-apps
replacement; this is a per-page JS patch whose fabricated `GeolocationPosition` any
site can detect (there is no constructor — `Object.getOwnPropertyNames` on a genuine
one returns `[]`).

**Measure before building either.** The extension's PAC bypasses only localhost,
`*.local` and `api.bringyour.com` — Chrome's network location provider calls
`googleapis.com`, which is **not** bypassed. Browser geolocation may already resolve
near the exit with zero code. Same shape as the Linux IP-source question.

Web port notes: **ur.io already has the globe** — `components/ip/Globe.jsx` is the d3
original the Android port was made from. **Extend it, do not fork it**: add `id` to
provider dots plus optional `selectedId` / `onSelectProvider` props, a selection ring
in `drawProviders()`, and a spin-to-selected target in `animateToCentral()`; `/ip`
is unaffected because both props stay optional. Entry point is `Connect.jsx` — the
status line already renders `Connected · N` from `cvc.grid.windowCurrentSize` — and
the established pattern is a sub-screen route (`/app/provider-locations`), matching
the Android screen-not-sheet decision.

⚠️ **Perf trap to design around:** `Globe.jsx`'s effect deps include the `providers`
array and `/ip` rebuilds that array every render with no `useMemo`. Harmless there;
**fatal** for a live view with a per-second duration clock — the whole SVG would tear
down and rebuild 1×/s. Memoize the dots and keep the clock strictly below the globe
(Android solved it the same way).

The Go core is complete; only the **js/wasm bridge** needs work — a marshaller, list
walker, getter, signal-only listener adapter, and the `removeConnectedProvider`
action in `sdk/js/device_remote.go`, plus the hand-written `DeviceRemote` interface in
`sdk/js/src/types.ts`. All five follow existing patterns in those files
(`jsNetworkPeers`, `addNetworkPeersChangeListener`, `shuffle`). **No swipe on web** —
there are no swipe gestures anywhere in the app; use the existing inline remove-button
idiom with an optimistic trim. Localization is nearly free: the six needed keys
already exist and are translated; they only need adding to `panel-keys.mjs`.

Decisions resolved so far:

- **D-LINUXPKG** — the Linux distribution target is **AppImage, not Snap**
  (`linux/PLAN.md` still says Snap; that intent is superseded). This matters here:
  an AppImage runs unconfined as the invoking user, so the snapd
  `location-observe`/`location-control` interface question stops being the deciding
  factor for mock location. The GeoClue question becomes whether the override can be
  done without root (a user-level config or a runtime D-Bus method) or needs a polkit
  prompt to edit `/etc/geoclue/geoclue.conf` plus a service restart — and whether the
  static source actually outranks GPS/Wi-Fi rather than only filling in when nothing
  better exists.
- **D-EXTBANNER (SUPERSEDED — the premise did not hold)** — the accepted cost was
  the "started debugging this browser" banner, on the understanding that *the feature
  is opt-in, so the user chooses whether to pay it*. **Research showed it is not
  opt-in.** Chromium marks the `debugger` permission `kFlagCannotBeOptional`, so it
  **cannot** go in `optional_permissions` — there is no `chrome.permissions.request()`
  runtime prompt. Every install shows *"Access the page debugger backend"* **and**
  *"Read and change all your data on all websites"* (the permission also carries
  `kFlagImpliesFullURLAccess`), whether or not the user ever enables location sync.
  The cost therefore lands on **all** extension users, not the ones who opt in — which
  is not what was agreed. Two further strikes: the banner is a single global infobar
  reading `"URnetwork" started debugging this browser` whose **Cancel button silently
  disables the feature** (`onDetach` → `canceled_by_user`) and which only enterprise
  policy or a launch switch can suppress; and Chromium already ships a dormant flag
  (`kDebuggerAPIRestrictedToDevMode`) that can restrict `chrome.debugger` to
  Developer-Mode users via a config flip, with no new code.
  → **D-EXTOVERRIDE** below replaces it.
- **D-EXTOVERRIDE** — use the **MV3 `world: "MAIN"` content-script override**
  (declarative registration, `match_origin_as_fallback: true`, prototype patching
  behind a `Proxy`, plus `Permissions.prototype.query`). It needs no `debugger`
  permission, so it can be a genuine opt-in, and it carries no banner. Honest
  caveats to disclose: the fabricated `GeolocationPosition` is permanently
  detectable by a determined page, and it is a per-page shim rather than a browser
  guarantee. **Do not ship the CDP path at all** — not even as a stricter tier.
  Chromium-only regardless: Firefox marked CDP geolocation override WONTFIX and
  removed CDP in Firefox 141, and WebKit has no `Emulation` domain.

## Implementation plan

Phased so each layer lands testable; apps consume prebuilt artifacts.

1. **server**: ClientScore ids + location directory + centroids + migration + response
   fields; extend `network_client_location_model_test.go` cases; spec update in
   `bringyour.yml` (+ redocly build).
2. **connect**: `ProviderLocation`, `DestinationStats.Location`, `ProviderEvent`
   {Location, EventTime, EgressClientId}, `AddProviderEvent` signature; update stall
   test; new monitor-event assertions.
3. **sdk**: exported struct/list/listener + Device methods + both implementations;
   extend `testing_gridWindowMonitor` harness tests, rpc-bridge test (additive fields),
   lifecycle/leak tests; cgo `make generate` + smoke; rebuild aar.
4. **android**: sheet + globe (asset, decoder, projection JVM tests) + list + selection
   sync + entry point; localizations keys.
5. **android mock location**: manifest permission; `MockLocationController` + state
   machine (JVM tests) + startup cleaner; sheet toggle + info overlay; guide screen +
   Settings → Connections row + developer-settings intent (with highlight extra);
   localizations keys; manual device matrix (26/30/31/36, orphan recovery, force-stop
   cleanup, master-switch off).
6. Later ports: apple/web reuse steps 1–3 unchanged (apple composes its own status
   string; web already has the globe).

## As built

Implemented 2026-08-04. Deviations and details worth knowing:

### connect
- `ProviderLocation` + `LocationCoordinates` in `api.go`, `FindProvidersProvider.Location`
  (nil = unknown). `DestinationStats.Location` carries it to the window.
- `ProviderEvent` gained `EventTime` (uncommented and stamped in `AddProviderEvent`),
  `EgressClientId`, and `Location`. `AddProviderEvent(clientId, state, egressClientId,
  location)` — 5 production call sites pass `args.Destination.Tail()` and
  `args.Location`; `removeClients` uses `client.args`.
- New test `TestMultiClientMonitorProviderEventCarriesDetails` asserts the stamp window,
  egress id, shared location pointer, callback delivery, and terminal deletion.
- Spec: named `ProviderLocation` + `LocationCoordinates` schemas in `bringyour.yml`,
  referenced from the FindProviders2 result item.

### sdk
- `device_provider_locations.go`: `ConnectedProviderLocation`,
  `ConnectedProviderLocationList`, `ConnectedProviderLocationChangeListener`, and the
  shared `deriveConnectedProviderLocations` used by both device implementations.
- The exported struct flattens coordinates into `RegionLat/RegionLon/CityLat/CityLon`
  with `HasLocation`/`HasRegionCoordinates`/`HasCityCoordinates` flags (gomobile has no
  nullable scalars), plus `ConnectedSinceMillis`.
- Sort: ascending `ConnectedSinceMillis` (= descending duration), unknown stamps last,
  egress client id as the tiebreak so the order is stable.
- `DeviceLocal` fires the change listeners from its existing internal window-monitor
  callback and once per destination change. `DeviceRemote` lazily creates **one**
  internal window monitor (`ensureConnectedProviderLocationsMonitor`) — registering it
  is what makes the events readable over rpc, and its subscription drives the local
  listeners; it retains the last readable list when the rpc is down and is unsubscribed
  in `Close`. **Zero new rpc methods**, as designed.
- `fixedWindowMonitor` now stamps `createTime` and sets `EgressClientId` so fixed
  destinations (the user's own peers) show a connected-since and a copyable id.
- Tests: derivation ordering/mapping, fixed-monitor stamps, and a gob round trip of
  `DeviceRemoteWindowMonitorEvent` proving the additive fields survive the rpc bridge.
- cgo regenerated: `urnet_device_get_connected_provider_locations` +
  `urnet_device_add_connected_provider_location_change_listener`, structs auto-classified
  as json, listener as a callback typedef. `make smoke` passes.

### server
- Migration appends `latitude`/`longitude` (nullable) to `location`; `CreateLocation`
  writes them on the city insert and self-heals existing rows when they are NULL and the
  incoming mmdb fix has coordinates (0,0 is treated as unknown).
- Process-local location directory with a 30-minute staleness bound and single-flight
  background reload; the query is bounded to location ids actually referenced by
  `network_client_location_reliability`, never the full seeded city list. Registered on
  `OnWarmup`/`OnReset` next to `countryCodeLocationIds`. Never blocks the request path —
  a cold directory yields nil locations rather than waiting.
- **Deviation (important)**: `ClientScore`'s three location ids are `*server.Id`, not
  value `server.Id`. gob does *not* omit zero-valued arrays, so value fields would have
  cost ~279 bytes per provider per copy even when unset — exactly the redis
  amplification the design forbids. Pointers cost nothing on the lookback copies.
  Old↔new blob compatibility was verified in both directions.
- Group-only specs still return `location: null` (only the location-keyed score query
  carries ids) — a known, tested gap; extending the group query is a follow-up.
- `resolveProviderLocation` omits any part it cannot resolve; city coordinates come only
  from the stored columns and are never silently replaced by the region centroid.

### android
- `ui/connect/providerlocations/`: `ProviderLocationsViewModel` (listener + value-compare
  dedupe + selection), `ProviderGlobe` (Compose Canvas), `ProviderLocationsSheet`,
  `MockLocationSection`, `MockLocationViewModel`, `MockLocationGuideScreen`, plus
  `GlobeGeometry`/`WorldTopology` (pure Kotlin, JVM-tested) and the `world-110m.json`
  asset.
- Entry point: `ConnectStatusIndicator` takes an optional `onShowProviderLocations` and
  makes the row clickable **only** while genuinely connected (not while reconnecting,
  polling a balance, or showing an insufficient-balance state).
- The globe centers once on the first provider and thereafter only on an explicit
  selection — recentering on every window turnover would fight the user's drag.
- `Route.MockLocationGuide` added; settings gains a "Device location sync" row in the
  Connections section next to blocked locations.
- Mock location: `location/MockLocationController` is a `@Singleton` confined to one
  `HandlerThread`, started from `MainApplication.onCreate` so the mandatory startup
  cleanup runs even when the feature UI is never opened. Persists the toggle *and* the
  registered provider-name set (written **before** registering, so a crash mid-add still
  leaves an exact cleanup list). GMS FLP mirroring ships as a per-flavor
  `FusedMockLocationSupport` (real on play/solana/ethos, no-op on github/ungoogle),
  matching the existing `BatteryOptimizationSupport` split.
- The globe decodes the TopoJSON off the main thread (`produceState` +
  `Dispatchers.Default`); the sphere, graticule and dots render while land loads.
- Sheet layout: the title, toggle and globe are fixed; only the list scrolls,
  in a `LazyColumn` taking the remaining height. Because the list is an ordinary
  scrollable child of the sheet, the sheet's nested scroll still takes over at the
  list's top edge, so pulling down there drags the sheet away as usual.
- Globe: **full bleed** (spans the sheet width, outside the header's horizontal
  padding) and square. It does **not** zoom to the web's cropping rest scale —
  `GLOBE_SCALE = CENTER − DOT_RADIUS − ring` sizes the sphere to fit inside the box
  with room for a selected dot's ring at the limb, so nothing paints outside the
  component. `clipToBounds()` remains as a backstop (a Canvas does not clip).
- Selection ring (globe): solid dot plus an outline ring 4 units outside its edge
  (`DOT_RADIUS + SELECTED_RING_GAP + stroke/2`).
- **Globe interaction is a scroll wheel** when providers are present. The wheel is
  ordered by **longitude** (west→east), independent of the list's duration order.
  A horizontal drag accumulates travel; crossing `WHEEL_STEP_WIDTH_FRACTION` of the
  globe's width steps the selection, and the step consumes exactly one threshold of
  travel — that leftover is the hysteresis, so a finger resting on the boundary
  cannot flicker between two providers. A fast drag crosses several steps at once.
  Swiping left advances (the globe spins east under the finger). The wheel wraps at
  both ends, which is correct *because* longitude is cyclic: stepping east past the
  last provider lands on the westernmost, which is also the shortest way round.
  Each step recenters the globe on the new selection (the existing selection-centering
  effect), so free rotation is disabled in this mode — it would fight the animation.
  With **no** plottable providers there is nothing to traverse, so the globe falls
  back to free-form drag. The gesture handler reads the selection through
  `rememberUpdatedState` rather than a `pointerInput` key, or every step would cancel
  the in-flight drag. Recenter timing dropped from the web's 1000 ms to 450 ms:
  recentering is now a per-step interaction, not an occasional one.
  `wheelStep`/`wrapIndex` live in the tested geometry module.
- Row layout: a fixed-size dot column on the left (24 dp box, so the width never
  changes with selection; the 12 dp dot gains a ring with a 4 dp gap when selected),
  top-aligned, and a right column of four stacked, right-aligned rows — client id
  (tap to copy), "city, region, country", "lat, lon", duration.
- **`MockLocationState` carries the raw setup signals** (`devOptionsEnabled`,
  `mockAppSelected`, `locationServicesEnabled`, plus a `setupComplete` derived value)
  in addition to `status`. This is load-bearing: `status` collapses to `DISABLED`
  whenever the toggle is off, so it cannot answer "is the device set up?" — the
  question both the toggle and the guide must answer *while the feature is still off*.
  Turning the toggle on opens the guide only when setup is incomplete, and the guide
  marks each step from its own signal. Both surfaces refresh the signals on first
  composition as well as on `ON_RESUME`, since neither entry path pauses the activity.
- Localization note: regenerating `strings.xml` from the store surfaced a **pre-existing
  drift** — `edit_network_name`, `claim_network_name_hint` and `change_network_name_hint`
  were in the committed `strings.xml` but had no key files, so the regeneration dropped
  them and broke `ProfileScreen.kt`. They were restored as proper store keys (and now
  exist in all 19 locale dirs, where HEAD had English only). Anyone regenerating should
  check `git diff .../values/strings.xml | grep '^-'` for the same class of drift.

### Provider removal (swipe to remove)

- `Device.RemoveConnectedProvider(clientId)` drops a provider by its **egress**
  client id. Backed by `RemoteUserNatMultiClient.RemoveProvider`, which does two
  things — and both are required:
  1. cancels every window client routing to that destination tail, and
  2. adds the id to the generator's discovery exclusion set.

  Removal alone is not enough: the resize loop wakes the moment a client dies
  (`ip_remote_multi_client.go`, the client-`Done()` watcher notifies
  `resizeMonitor`), re-expands, and the next `find-providers2` can return the
  same provider — the row would reappear within seconds.
- **Exclusion lifetime: the current connection** (decided with the user). This
  needs no explicit clearing: a destination change builds a new generator and
  multi client, so reconnecting gives every provider a clean slate.
  `ApiMultiClientGenerator.ExcludeClientId` is mutex-guarded — discovery reads
  the set on the enumerator goroutine while the app appends from the ui thread.
- **Fixed-destination connections are never excluded.** When every spec is an
  explicit client id (a chosen network peer), there is nothing to replace the
  provider with, so excluding it would leave the tunnel with no destination at
  all. There the call drops the client and lets the window redial it.
- Unlike the read-only locations surface (which rides the bridged monitor),
  this is an action, so `DeviceRemote` mirrors it with a real rpc call —
  the `Shuffle` pattern.
- Android: the shared `ui/components/SwipeToRevealRow` — swipe reveals a small
  centered capsule that grows with the drag and must be **tapped** to confirm,
  the iOS-style behavior. The view model trims the row locally before the sdk
  round trip so the swipe does not snap back.
  While wiring this up, that component was moved out of `ui/stats/` into
  `ui/components/` and the two lists still on Material's `SwipeToDismissBox`
  (blocked locations, and this screen's first cut) were converted to it, so
  every swipe-to-delete list in the app now shares one implementation and
  swiping alone can never delete.

### Verification (2026-08-04)
- connect: `go build ./...`, monitor + multi-client suites pass (incl. the new
  `TestMultiClientMonitorProviderEventCarriesDetails`).
- sdk: **full `go test ./...` suite passes** (433 s), including the heavy DeviceRemote rpc
  end-to-end tests; cgo `make generate` + `make smoke` pass; wasm/js builds.
- gomobile aar rebuilt and the export-validation gate passes; the bound Java surface has
  `getConnectedProviderLocations()` / `addConnectedProviderLocationChangeListener()` and
  the flat `ConnectedProviderLocation` getters, with no skipped exports.
- android: `:app:compileGithubDebugKotlin` **BUILD SUCCESSFUL**; new JVM unit tests pass
  — GlobeGeometry 14, WorldTopology 7, MockLocationState 11, ProviderLocations labels 5
  (37 total, 0 failures).
- server: `go build ./...`, `go vet`, and the model tests pass (including the two new
  tests for city-coordinate persistence and the FindProviders2 location payload).
- spec: `bringyour.yml` parses; `location` `$ref`s `ProviderLocation` with all 9 fields.

### Known gaps / follow-ups
- Providers selected via a **group-only** spec return `location: null` (only the
  location-keyed score query carries ids). Tested and intentional; extending the group
  query is a follow-up.
- The mock-location device matrix (API 26/30/31/36: activate, verify in Maps, provider
  rotation teleport, toggle-off resume, force-stop → startup cleaner, deselect-while-
  active → orphan recovery, master-switch off) has **not** been run on hardware.
- Unrelated pre-existing drift found while regenerating: the apple `Localizable.xcstrings`
  carries ~61 English-only Xcode-extracted strings that were never imported into the
  localizations store, so any `npm run gen` drops them from the catalog (SwiftUI falls
  back to the literal, so no compile break). Needs a separate store import; not touched
  here.
- Apple/web UI ports remain; they consume the server/connect/sdk layers unchanged.

