# Blocker — design plan

An ad/tracker blocker for connect + sdk built around the existing sketch in
`connect/ip_blocker.go` (interface `Blocker { BlockHost, BlockIp, Enabled, SetEnabled }`;
peppered-SHA256 hostname hashes; IP subnets packed like `ip_security_cfaa_block.go`).
Hostnames are enforced at the UpgradeMux DNS layer; IPs and reverse-index hostnames are
enforced in the multi-client egress decision. The device exposes a single
`SetBlockerEnabled`/`GetBlockerEnabled` toggle across DeviceLocal, DeviceRemote, and the RPC.

Decisions resolved 2026-07-11 (license research verified live that day):
- **Default off** — `DeviceLocalSettings.DefaultBlockerEnabled = false`; apps expose the toggle.
- **Standard tier embedded** — ads + tracking, ~230–280k unique domains, ~2.0–2.3 MB rodata.
- **Security-list licensing remediation is a separate follow-up** (recorded below).

## Data sets (all verified permissive, active, clean provenance)

Every top-tier general list is unusable here: OISD and HaGeZi are GPL-3.0, AdGuard DNS
filter GPL-3.0, EasyList dual GPL/CC-BY-SA, Disconnect and DuckDuckGo CC-BY-NC-SA,
Peter Lowe non-commercial, 1Hosts' MPL label sits over 400+ contaminated upstreams
compiled in a private repo, Block List Project now syncs GPL upstreams, and the one big
Apache-2.0 list (Lightswitch05) is archived/dead. Competitor VPNs use the GPL lists by
filtering **server-side** (never distributing them); urnetwork's decentralized data plane
means we distribute the list in client binaries, so provenance must be clean.

Feeds (blocker/main.go), Standard tier:

| feed | url | license | ~entries | format |
|---|---|---|---|---|
| shadowwhisperer_ads | raw.githubusercontent.com/ShadowWhisperer/BlockLists/master/Lists/Ads | Unlicense | 27k | plain domains |
| shadowwhisperer_wild_ads | …/Lists/Wild_Ads | Unlicense | 16k | domain bases |
| shadowwhisperer_tracking | …/Lists/Tracking | Unlicense | 147k | plain domains |
| shadowwhisperer_wild_tracking | …/Lists/Wild_Tracking | Unlicense | 15k | domain bases |
| anudeepnd_adservers | raw.githubusercontent.com/anudeepND/blacklist/master/adservers.txt | MIT | 42.5k | hosts (0.0.0.0) |
| frogeye_firstparty | hostfiles.frogeye.fr/firstparty-trackers.txt | MIT | 14.7k | plain domains |
| frogeye_multiparty | hostfiles.frogeye.fr/multiparty-trackers.txt | MIT | 19k | plain domains |
| hostsvn | github.com/bigdargon/hostsVN (domains variant) | MIT | 18k | domains |

Allowlist subtraction: anudeepND/whitelist `whitelist.txt` (MIT, 191 domains — classic FP
set: CDN, OCSP, captcha, login endpoints) + in-house `blocker/allowlist.txt` that we own
(seed: `ur.network` + api/control-plane domains + the DoH resolver hostnames; grow from
breakage reports). Do not include anudeepND `referral-sites.txt`/`optional-list.txt`.

ShadowWhisperer is original research ("I will not merge other lists"), near-daily updates.
anudeepND is the lowest-FP-reputation curated list; updates are bursty (generator warns if
older than ~9 months). Frogeye first-party is the unique CNAME-cloaking coverage
(per-customer tracker FQDNs — exactly what DNS-level blocking can kill); regenerated weekly.
The generator also emits a Conservative composition (ads-only feeds, ~85k, ~0.7 MB) behind a
flag for a possible future aggressiveness setting; only Standard data is committed for now.

Attribution: generated-file header lists every feed with credit + license (like the
Spamhaus credit in `ip_security_cfaa_block.go`); MIT sources get their copyright lines.
Recommend also a public docs/attribution page (IVPN/ExpressVPN model). Note: hashing does
not launder license obligations (selection/arrangement copyright + EU database right), and
per RFC 9276 experience salted hashes of public domain names are not concealment — the
pepper is engineering (below), not secrecy, and we don't rely on it for licensing.

## Format — generated `ip_blocker_block.go`

Mirrors `ip_security_cfaa_block.go` (const strings in rodata, zero init alloc, no memory
budget interaction):

- `blockerPepper` — 32 random bytes (crypto/rand), regenerated on every generator run.
- `blockerBlockedHostCount` / `blockerBlockedHostData` — sorted unique **6-byte** records:
  the leading 6 bytes (big-endian) of `SHA256(pepper || name)`. Hashes rather than
  strings serve two goals: fixed-size binary-searchable records, and the generated
  source / shipped binary carries no plaintext domain list (source obfuscation — though
  not cryptographic concealment; see the attribution note above).
  **Record width is a size/collision tradeoff** (`blockerHostRecordLen`, mirrored by the
  generator's `hostRecordLen`): at 6 bytes and ~160k entries a random hostname collides
  with p ≈ 160e3/2^48 ≈ 6e-10 per probe (a few probes per lookup), and the pepper is
  regenerated every build so even that vanishing chance is transient rather than a
  permanently mis-blocked domain. 6 bytes keeps the full Standard tier inside the 1 MiB
  budget below; 8 bytes would not (1.21 MiB).

## Size budget

Both generators enforce a hard `-max-bytes` budget (default **1 MiB each**) and abort
before writing if exceeded. The budget is measured on the **embedded** table bytes — the
packed string constants that land in the binary's read-only data — **not** the generated
`.go` source, which is ~4x larger because each byte is written as a 4-char `\xNN` escape
and is compiled away. Only the embedded bytes cost SDK size.

| generated file | source on disk | embedded (ships in the SDK) | of 1 MiB budget |
|---|---|---|---|
| `ip_blocker_block.go` | 3.73 MiB | **0.910 MiB** (159,062 hosts × 6B + 32B pepper) | 91% |
| `ip_security_cfaa_block.go` | 1.69 MiB | **0.412 MiB** (49,441×8B v4 + v6) | 41% |
| combined | | **1.323 MiB** | |

The blocker sits at 91% of budget, so adding feeds will trip the gate — the levers are
`-tier conservative` (~85k hosts), dropping a feed, narrowing the record further, or
raising `-max-bytes`. `TestBlockerGeneratedTables` also asserts the 1 MiB cap, so an
over-budget table fails the test suite even if someone bypasses the generator.
- `blockerBlockedPrefixCount/Data` (v4, 8 B lo/hi) and `blockerBlockedPrefix6Count/Data`
  (v6, 32 B lo/hi) — same packing as the cfaa tables. v1 population: only literal IP
  entries appearing natively in the hostname feeds (research: effectively none — the sole
  maintained ad-IP list, Peter Lowe's, is non-commercial and is itself just his hostname
  list resolved). The mechanism ships and is tested with synthetic data; do NOT resolve
  hostnames into IPs (shared-CDN false positives).

Name canonicalization (identical in generator and runtime): lowercase, strip trailing dot,
IDNA/punycode with the exact `connect.Punycode` options (`MapForLookup`,
`Transitional(true)`, `StrictDomainName(false)` — net_http_doh.go:1115), reject > 253 chars.

Truncation math: at N=280k, a random hostname false-positive is N/2^64 ≈ 1.5e-14 per
lookup (Chromium-style exact sorted table, not a bloom filter; we have no online
confirmation channel, so 4-byte Safe-Browsing prefixes are not an option). The pepper
defeats adversarial collisions: an attacker cannot craft an upstream-list entry whose
truncated hash collides with a victim domain because the pepper is regenerated after the
feeds are fetched, at every release; rotation also clears any accidental collision.

Semantics: **a blocked name blocks itself and all subdomains** (single suffix set, per the
ip_blocker.go sketch). Exact-hostname feeds are ingested with the same semantics —
children of a tracker FQDN are trackers; the safety valve is the generator's PSL guard.

## Generator — `connect/blocker/main.go`

`//go:generate go run .`, package main, self-contained (like `security/main.go` — shares
no code with it; the small range-merge helpers are duplicated by design). Features carried
over from the security generator: concurrent fetch (sem 8, UA, 3 retries), per-feed
`minCount` floors that hard-fail the build (calibrated ~50–60% of observed volume),
`deprecated` tolerance, reserved-space subtraction for IP tables, `-allow-stale`, `-out`,
`resolveOut` by module root, `guardOutput` DO-NOT-EDIT check, gofmt + attribution header.

New, blocker-specific:
- Parsers: hosts format (`0.0.0.0 name` / `127.0.0.1 name`), plain-domain lines, and the
  domain-only subset of adblock syntax (`||name^` only; skip `@@` exceptions, cosmetic,
  regex, path rules). Literal IP/CIDR entries route to the IP tables.
- Normalize (as above) → drop invalid, `localhost`/`*.localhost`/`local`, ip-literals-as-names.
- **PSL poison guard**: reject any entry that IS a public suffix (ICANN + private sections
  — rejects `com` and also `cloudfront.net`, `github.io`, `s3.amazonaws.com`-class
  platform suffixes that would nuke shared infrastructure under suffix semantics).
- Suffix compression: drop any entry that has an ancestor entry (parent already blocks it).
- Allowlist subtraction: remove exact entries matching the allowlists (documented
  limitation: cannot punch a hole under a blocked parent; per-user unblock arrives later
  via the BlockActionOverride path).
- Resolver protection: subtract `DefaultDnsResolverSettings` endpoint IPs from the IP
  tables and their hostnames from the host set (mirror of `defaultRemoteDohIgnoreAddrs`).
- Poison ceilings: abort if total unique hosts > 1.5M or any single feed > 3× its
  historical size; staleness warnings from list headers (anudeepND exception ~9 months).
- Emit pepper + tables + header credits; print per-feed report like the security generator.

## Runtime — complete `connect/ip_blocker.go`

- Fix the missing `net/netip` import; keep the `Blocker` interface exactly as sketched.
- `NewBlockerWithDefaults() Blocker` — impl backed by the generated tables;
  `enabled atomic.Bool` (constructed disabled; DeviceLocal seeds from settings).
- `BlockHost(host)`: false when disabled. Normalize (alloc-free fast path for
  already-lowercase ASCII; stack buffer `[32+253]byte` for `SHA256(pepper||name)` via
  `sha256.Sum256` — zero heap). Walk per the sketch: exact hash first, then label suffixes
  from 2 labels upward until back at the full name (1-label suffixes skipped — the PSL
  guard keeps TLDs out of the table). Cost: ≤ labels−1 SHA256 ops, only at DNS-query /
  flow-decision time, never per packet.
- `BlockIp(addr)`: false when disabled; `Unmap()`; binary search the packed tables.
  Generalize `cfaaBlockedIp4` into `searchRange4(data, count, ip)` (cfaa delegates;
  `cfaaSearch6`/`cfaaBe64` are already generic — reuse directly).
- Nil-blocker is valid everywhere (proxy/server pass none).

## Integration — UpgradeMux (DNS)

The mux is the primary enforcement point (hostnames are only visible here: no SNI parsing
exists, TCP/443 is never claimed, and TCP/80 is pass-through/block without Host parsing).

- `UpgradeMux.SetBlocker(Blocker)` (atomic field), wired by DeviceLocal beside
  `SetUpstream`/`SetServerNameLookup` (device_local.go:2244–2246) — deliberately not part
  of the swappable `UpgradeMuxSettings` so `SetUpgradeMuxSettings` can't clear it.
- `handleDns` (ip_mux_upgrade.go:441): after `domain :=` (line 460), if
  `blocker.BlockHost(domain)`:
  - A → answer `0.0.0.0`, AAAA → `::` (industry default null-blocking; connects fail
    instantly and locally), TTL = the settings `ResponseTtl`.
  - **Claim every other qtype for blocked names too** (currently non-A/AAAA pass through
    unclaimed) and answer empty NOERROR/NODATA — otherwise browsers reconnect via
    HTTPS/SVCB (type 65) `ipv4hint`/`ipv6hint` records.
  - Reply via the existing `buildDnsResponse` + `deliverDownstream(ipOosPacket(...))`
    path (:566–570); extend `buildDnsResponse` to support zero answers (and an RCode
    param for completeness). Do NOT call `recordServerNames` (no reverse-index pollution).
- Blocked queries never enter `attachDnsResponder`/DohCache, so the check is ahead of the
  cache and toggling on takes effect immediately (modulo OS-level DNS TTLs).
- Scope notes: DNS enforcement requires the mux DNS to be active (`Dns != nil`; the proxy
  disables the mux) and a remote client (VPN connected) — consistent with NetShield-class
  features. Apps doing their own DoH to third parties on :443 bypass the DNS layer; the
  multi-client backstop below partially covers them. CNAME-chain inspection (blocking by
  intermediate CNAME targets) needs resolver plumbing that doesn't exist — future work;
  the Frogeye first-party feed covers the known CNAME-cloaked trackers by FQDN.

## Integration — multi-client (IP + reverse-index backstop)

- `multiClientConfig` (ip_remote_multi_client.go:435) gains `blocker Blocker`;
  `SetBlocker` mirrors `SetServerNameLookup` (:674–691) including the
  `blockActionCache` clear.
- The check fuses into the existing per-destination block-action decision (computed with
  `serverNames(addr)` — destination-scoped, NOT the IpAssoc cluster, to avoid
  over-blocking co-clustered api/cdn hosts): blocker hit =
  `BlockIp(dst) || any(BlockHost(serverName))`.
- `blockActionApply` (ip_block_action.go:219) gains the blocker input. Precedence:
  incident > user override > **blocker** > security default. An override that un-blocks a
  blocker hit egresses **remotely** as normal (unlike un-blocked security drops, which
  stay local-only). Destinations matched by the ignore set (`blockActionIgnored`, resolver
  endpoints) skip the blocker like they skip overrides.
- Cached decisions (`blockActionDecision`) record the blocker-enabled snapshot at compute
  time; a cache hit revalidates against `blocker.Enabled()` (one atomic load) so toggling
  invalidates instantly without an interface change.
- Blocked flows use the existing drop path (SendPacket :1250–1254) and count in the
  existing `PacketStats` block counters. `BlockAction` gains a `Blocker bool` attribution
  field (+ key bit in the collector) so the UI can later say "blocked as ad/tracker".
  No new stats surfaces in v1.

## SDK — device toggle (template: RouteLocal, the full ~31-site checklist)

- `device.go`: `SetBlockerEnabled(bool)` / `GetBlockerEnabled() bool` on `Device`
  (:418/:420 pattern) + `BlockerEnabledChangeListener` (:47–49 pattern) +
  `AddBlockerEnabledChangeListener` (:486 pattern). Plain bools bind cleanly in gomobile.
- `device_local.go`: **stable field** `blocker connect.Blocker` created once in the
  constructor via `NewBlockerWithDefaults()` (the mux + multi-client are torn down and
  rebuilt on every `SetDestination`/`Shuffle`, so the blocker must live on DeviceLocal —
  model: `upgradeMuxSettings` :285). Seed enabled from new
  `DeviceLocalSettings.DefaultBlockerEnabled` (**default false**; :163/:102 pattern).
  Wire on every rebuild beside :2244–2246 (`mux.SetBlocker`, `multi.SetBlocker`).
  Setter/getter/listener/emit mirror SetRouteLocal (:1288–1310, :1312–1317, :1525–1530,
  :1683–1690) — setter just calls `self.blocker.SetEnabled` (survives rebuilds for free).
- `local_state.go`: `SetBlockerEnabled`/`GetBlockerEnabled` pair (mirror :210–225, new
  `.blocker_enabled` file). Unlike RouteLocal, the device owns persistence (the
  DnsResolverSettings model): `SetBlockerEnabled` persists on set and the DeviceLocal
  constructor restores from LocalState, so the apps only expose the toggle.
- `device_rpc.go`: full RouteLocal plumbing — DeviceRemote cached
  `deviceRemoteValue[bool]` + offline fallback to `settings.DefaultBlockerEnabled`
  (:4103/:1193–1246 patterns), sync-request listener ids (:4235/:482), DeviceLocalRpc
  set/get methods (reflection-registered, no dispatch table; :7575–7586), listener
  add/remove + reverse-notify (:7952–7993), state snapshot + Sync apply + initial listener
  fire (:5840, :5919–5921, :6032–6034, :6182–6184), DeviceRemoteRpc reverse handler
  (:8680–8686), teardown (:5588–5590).
- Bindings: cgo — nothing to classify, run `make generate` in `sdk/cgo` (emits
  `urnet_device_get/set_blocker_enabled`); js — two `js.FuncOf` entries in
  `js/device_remote.go` (mirror :133–140) + listener wrapper if desired; regenerate the
  gomobile .aar / XCFramework.
- Binary size: ~2.2 MB rodata in the aar/xcframework (acceptable). Verify the js/wasm
  build drops the tables via dead-code elimination (js constructs only DeviceRemote); if
  not, build-tag the data out of wasm.

## Tests

The four load-bearing properties each get direct unit coverage plus a reference-model
fuzz, then every enforcement seam gets end-to-end coverage: (1) hash storage — fixed-size
records, no plaintext in source; (2) suffix semantics — a blocked domain blocks all its
subdomains; (3) the walk — exact, then base, enumerating subdomains back up to exact;
(4) IP ranges in the security_block packed format.

**Hash table + generated data (`ip_blocker_test.go`, mirroring `ip_security_cfaa_test.go`)**
- Structural invariants: `len(blockerBlockedHostData) == count*8`; records strictly
  increasing (sorted + unique); `len(blockerPepper) == 32`; counts above generator floors.
- Obfuscation property: the generated source contains no plaintext feed domain (assert a
  sample of known-blocked names never appears as a substring of `ip_blocker_block.go`).
- Membership round-trip on a fixture-built table: every fixture entry hits; a 1M-random-
  name probe against a small test table yields 0 false hits (statistical FP check).
- Golden hash vectors: shared `blocker/testdata/hash_vectors.txt` (fixed test pepper →
  name → expected 8-byte record) asserted by BOTH the generator tests and the runtime
  tests — pins generator/runtime normalization+hash agreement without cross-imports.

**Walk + suffix semantics (table-driven on a hand-built table)**
- Hits: exact; direct subdomain; deep subdomain (entry `ads.example.com` blocks
  `a.b.c.ads.example.com`); base-domain entry blocks every depth; hit at any walk
  position (exact hit, base hit, mid-suffix hit).
- Misses that define correctness: parent of an entry (`example.com` free when only
  `ads.example.com` is listed); sibling (`cdn.example.com`); **label-boundary attacks** —
  `evilads.example.com` / `xads.example.com` must NOT match entry `ads.example.com`,
  `ample.com` must not match `example.com` (string-suffix vs label-suffix); bare TLD /
  1-label suffixes never probed or matched.
- Normalization equivalences: mixed case (DNS 0x20 randomization), trailing dot, IDNA
  (`bücher.example` ≡ `xn--bcher-kva.example`); and inputs that must return false
  without panicking: empty, `.`, `..`, leading dot, embedded whitespace, >253 chars,
  single label, raw IP literal passed as host.
- Walk order / short-circuit: a base hit stops without hashing deeper suffixes
  (hash-count instrumentation hook or benchmark delta).
- `FuzzBlockHost` (mirroring `ip_security_fuzz_test.go`): naive string-set +
  label-walk reference vs `BlockHost` over fixture entries, random subdomain expansions,
  mutations (char flips, dot insert/remove), and unicode garbage — agreement + no panic.

**Enabled gating + concurrency**
- Disabled → false for known-blocked host and IP; enable/disable flips take effect
  immediately (no caching inside the Blocker itself).
- `-race`: parallel BlockHost/BlockIp with a concurrent SetEnabled toggler.

**Zero-alloc + perf** (blocking runs per DNS query / flow decision, never per packet)
- `testing.AllocsPerRun` == 0: BlockHost ASCII fast path (hit and miss) and BlockIp
  v4 + v6, mirroring `TestCfaaBlockedIp4ZeroAlloc`.
- Benchmarks: BlockHost at 2/5/10/127 labels (cost must stay linear in labels), BlockIp.

**IP ranges (the full cfaa assertion suite, pointed at the blocker tables)**
- Invariants: record sizes (8 B v4 / 32 B v6), strictly sorted, pairwise-disjoint, lo ≤ hi.
- Boundary probes lo−1/lo/hi/hi+1 per sampled range + LCG brute-force sweep vs an
  independent linear reference; hand-packed `cfaaSearch6`-style test for v6.
- `Unmap` normalization: `::ffff:1.2.3.4` ≡ `1.2.3.4`.
- Empty-table behavior (v1 ships ~empty IP tables): always false, no panic, zero alloc.
- Refactor regression: existing cfaa tests pass unchanged with `cfaaBlockedIp4`
  delegating to the shared `searchRange4` (same results, still zero-alloc).

**Generator** (`blocker/main.go` structured as testable pure stages — fetch | parse |
normalize | guard | compress | subtract | hash | emit — with the pepper injectable and
feeds served from fixtures/httptest; no network in tests)
- Parsers: hosts (`0.0.0.0`/`127.0.0.1`), plain domains, `||domain^` (only that adblock
  form; `@@` exceptions, cosmetic, regex, path rules skipped and counted), whole-line and
  inline comments, CRLF, BOM; IP/CIDR literals routed to the range tables.
- Normalization identical to runtime (golden vectors above); invalid entries counted.
- PSL poison guard: `com`, `co.uk`, `github.io`, `cloudfront.net`, `s3.amazonaws.com`
  rejected; entries beneath them accepted.
- Suffix compression: child dropped when any ancestor entry exists, input-order
  independent; duplicate hash records deduped (harmless collisions).
- Allowlist subtraction: exact removal; assert the documented limitation (an allowlisted
  subdomain of a blocked parent stays blocked) so the behavior is pinned, not accidental.
- Resolver protection: `DefaultDnsResolverSettings` IPs subtracted from ranges and
  hostnames excluded from the host set.
- Safety rails abort: per-feed `minCount` floor, total-entry ceiling, v4 prefixes broader
  than /8 and v6 broader than /16 rejected as poison (security-generator rules).
- Pepper: 32 bytes; two runs differ in pepper and table bytes yet agree on membership;
  fixed pepper + fixed fixtures → byte-identical output (determinism).
- Emitted file: gofmt-clean, DO-NOT-EDIT marker, per-feed credits, compiles.

**Mux DNS e2e (dnsClientHarness from `ip_mux_upgrade_test.go`)**
- Blocked A → `0.0.0.0`, AAAA → `::`, TTL from settings, on the reversed path.
- Blocked name with qtype HTTPS/65 (and TXT): claimed, empty NOERROR.
- Mixed-case and trailing-dot query forms of a blocked name still block.
- Unblocked names resolve exactly as before (regression); nil blocker and disabled
  blocker are byte-identical pass-through.
- Reverse index unpolluted: no `ServerNames` entry from blocked answers.
- Block-beats-cache: resolve with blocker off (DohCache warm), toggle on, re-query →
  blocked immediately.
- Toggle mid-test flips behavior both ways without a mux rebuild.

**Multi-client e2e (`ip_block_action_test.go` harness + fake `ServerNameLookup`)**
- Blocked dest IP: dropped, `PacketStats` block counters increment,
  `BlockAction{Blocker:true}` emitted with the destination.
- ServerName-driven block: dest IP not in tables, reverse index maps it to a blocked host.
- Dest-scoped matching guard: a destination whose IpAssoc cluster contains a blocked
  host but whose own serverNames/IP are clean is NOT blocked (over-block regression).
- Override interplay: a user un-block override on a blocker hit egresses REMOTELY
  (assert not local); the blocker never un-blocks a security drop; incidents stay
  terminal and non-overridable.
- Ignore set: resolver destinations skip the blocker.
- Decision-cache invalidation: a cached blocked decision flips on `SetEnabled(false)`
  without waiting out the TTL (and the reverse).

**sdk (RouteLocal test quartet + packet path + leak)**
- `TestDeviceRemoteFull` / `FullSync` / `LastKnownValues(+Listeners)` extended with
  BlockerEnabled round-trip, sync-on-connect, offline set/get caching, and listener
  replay (copy `testing_routeLocalChangeListener`).
- DeviceLocal packet path (mux-security/block harnesses): enabled → crafted DNS query
  via `device.SendPacket` answered `0.0.0.0`, blocked-IP flow dropped with stats;
  disabled → passes.
- Rebuild survival: enable → `SetDestination`/`Shuffle` → still enabled and still
  enforced (the stable blocker field re-wired into the fresh mux/multi-client).
- LocalState pair round-trip; toggle churn (×1000) under the goroutine/pool/fd leak
  helpers (`device_leak_helpers_test.go`).

## Sequencing (each step builds, tests, ships independently)

1. **connect core** — finish `ip_blocker.go` (imports, impl, `searchRange4` refactor with
   cfaa delegation) + deep unit tests against a small hand-built table. **DONE**
2. **Generator** — `blocker/main.go` + committed `ip_blocker_block.go` (Standard tier) +
   generator tests + attribution header. **DONE**
3. **Mux DNS enforcement** — `SetBlocker`, handleDns hook, `buildDnsResponse` extension,
   qtype-65 claiming, e2e tests. **DONE**
4. **Multi-client enforcement** — config snapshot + decision fusion + BlockAction
   attribution + cache invalidation + e2e tests. **DONE**
5. **SDK toggle** — device interface, DeviceLocal, LocalState, DeviceRemote + RPC, tests;
   `sdk/cgo make generate`; js entries; regenerate gomobile bindings. **DONE** (gomobile
   .aar/XCFramework regen rides the release flow)
6. **Apps** — the toggle UI (next step, out of scope here).

## Status — implemented (2026-07-11)

All five sequencing steps are implemented and tested; the full connect and sdk test
suites pass.

- **connect core**: `ip_blocker.go` (interface unchanged; `dataBlocker` impl,
  zero-alloc stack-buffer hashing, label-suffix walk) + shared packed-table searches
  moved to `ip_util.go` (`be64`, `searchRange4`, `searchRange6`, `searchHash64`; the
  cfaa lookups delegate). Benchmarks: base-suffix hit 258 ns / 0 allocs, ip4 lookup
  5.3 ns. Tests: walk/label-boundary/normalization tables, IDNA equivalence,
  golden hash vectors (`blocker/testdata/hash_vectors.txt`, asserted by both the
  generator and runtime suites), 1M-name false-positive probe (0 hits), zero-alloc
  assertions, `-race` toggle churn, `FuzzBlockHost` vs an independent reference model,
  generated-table invariants, evergreen-domain smoke on the real data.
- **Generated data (live run 2026-07-11)**: all 9 feeds fetched with **zero invalid
  entries**; 272,515 raw → **159,062 unique host records** (6 PSL-poison rejected,
  14 allowlisted, **53,226 dropped as covered by a parent entry**) ≈ **1.27 MB**
  rodata (5.2 MB generated source). IP tables: **empty**, as researched (no literal
  IPs in the permissive feeds; the mechanism ships and is tested with synthetic data).
  Observed feed sizes vs the research table: SW Tracking 115k (not 147k), Wild_Ads
  18.6k; floors hold comfortably.
- **Mux DNS**: `UpgradeMux.SetBlocker` (atomic, outside the swappable settings);
  `handleDns` checks the blocker ahead of resolution/caching for EVERY query type —
  A→`0.0.0.0`, AAAA→`::`, others (HTTPS/SVCB 65, TXT, …) claimed with empty NOERROR;
  no reverse-index pollution. e2e: synthesized-reply assertions per type,
  block-beats-cache (resolve with blocker off, enable, re-query → blocked), toggle
  both ways and nil-blocker without a mux rebuild.
- **Multi-client**: `SetBlocker` on the `multiClientConfig` snapshot (clears the
  decision cache); destination-scoped `blockerCheck` (ip + own server names, never
  the IpAssoc cluster) fused into `blockActionDecision` (cached with a
  blocker-enabled snapshot revalidated per hit); `blockActionApply` precedence
  incident > override > blocker > security, un-blocked blocker hits egress remotely;
  `BlockAction.Blocker` attribution. e2e: ip-block + stats + attributed action,
  server-name backstop, dest-scope over-block guard, override-unblock → remote,
  immediate toggle across the cache, `-race`.
- **SDK**: `SetBlockerEnabled`/`GetBlockerEnabled` + `BlockerEnabledChangeListener` on
  `Device`, both implementations; stable `DeviceLocal.blocker` field created in the
  constructor, seeded from `DefaultBlockerEnabled` (false), re-wired into the fresh
  mux/multi client on every rebuild (`SetDestination`/`Shuffle`/`RemoveDestination`
  survival tested); `LocalState` pair (`.blocker_enabled`, app-managed); full RPC
  plumbing (DeviceRemote cached value + offline fallback, reflection-registered
  DeviceLocalRpc set/get, listener add/remove + reverse-notify, state snapshot +
  Sync apply + initial listener fire + teardown). Tests: local toggle+listener,
  RPC round-trip both directions, offline set → Sync apply, LocalState round-trip,
  leak-checked ×1000 toggle churn.
- **Bindings**: cgo regenerated (`urnet_device_get/set_blocker_enabled` + listener
  callback surface; 501 functions) and builds; js `getBlockerEnabled`/
  `setBlockerEnabled` map entries added and the wasm builds (the data table adds
  ~1.3 MB to a ~41 MB wasm — acceptable; a wasm build tag remains an option).
  gomobile .aar/XCFramework: regenerate via the release flow. **Bind surface
  verified 2026-07-11** — a single-target `android/arm64` `gomobile bind` passed
  the Makefile's `// skipped`-vs-allowlist validation gate (no un-allowlisted
  skips; the Blocker RPC-layer skips are allowlisted like RouteLocal's), exported
  `get/setBlockerEnabled` + `addBlockerEnabledChangeListener` on
  Device/DeviceLocal/DeviceRemote, and generated a `BlockerEnabledChangeListener`
  interface structurally identical to `RouteLocalChangeListener`. iOS uses the
  same binding logic on the same plain-bool + single-method API.

## Follow-ups (recorded, not in scope)

- **Security-list re-sourcing — DONE (2026-07-11).** `security/main.go` was re-sourced
  under a permissive-first policy (maximize surface; treat openly-published no-license
  feeds as usable; exclude only feeds whose terms explicitly forbid commercial use or
  redistribution). Live regen: **49,601 IPv4 ranges + 1,145 IPv6** (was 64,131 / 214),
  14.8M addresses — modest v4 drop, 5× v6 growth, cleanly sourced. Full connect suite
  green.
  - **Removed:** AlienVault OTX (EULA forbids distribution), Binary Defense
    (non-commercial), ThreatFox (its CC0 grant was replaced 2025-11-04 by
    Spamhaus-managed terms — auth-gated, commercial "may require a subscription,"
    revocable; re-add only under an abuse.ch/Spamhaus agreement), ipsum + FireHOL
    level1 (contaminated aggregates re-bundling DShield CC-BY-NC-SA / Team Cymru /
    AbuseIPDB / GreenSnow), and the dead sources CruzIt (frozen 2023), NiX Spam
    (ceased 2025), SSLBL + darklist (empty).
  - **Kept:** Spamhaus DROP/DROPv6 (free for product use, © credit captured verbatim
    in the header), ET Open compromised-ips (BSD), Feodo main (CC0, `deprecated` since
    it's usually empty post-takedown), blocklist.de / CINS / BruteForceBlocker
    (openly published, no stated license → usable per policy).
  - **Added:** Feodo aggressive (CC0, 7,607 — note: abuse.ch warns it is FP-prone and
    it is frozen at 2026-03-04, so it's the first candidate to drop on FP reports),
    TweetFeed IP rows (CC0-1.0, ~627, via a new CSV type-column filter in the
    generator), ViriBack C2 Tracker (no stated license, ~13,052 current C2 IPs — the
    main egress-relevant addition; new `kindCSV` col=2 parse).
  - **Open items:** switch the Spamhaus fetch to the JSON endpoints
    (`drop_v4.json`) before the .txt files are eventually deprecated (Spamhaus will
    give "ample notice"); FP-watch the churny/frozen feeds (blocklist.de, CINS,
    Feodo-aggressive, TweetFeed) since false egress blocks are user-visible (mitigated
    by `routeLocal`, which routes security-drops locally instead of blocking).

## Sourcing policy (decided 2026-07-11 — applies to both generators)

A feed is included if, and only if, **either**:

1. it carries a license that permits commercial use, modification and
   redistribution (CC0, MIT, BSD, Apache-2.0, MPL-2.0, Unlicense, public domain,
   or an explicit written grant to the same effect — e.g. Spamhaus DROP, which is
   free for product use provided the © credit travels with the data); **or**
2. it states **no license at all** and is openly published, in which case it is
   treated as public domain.

Anything else is dropped. We do not weigh risk, argue that a transform launders an
upstream term, or negotiate for rights: **no paid data agreements, no permission
letters, no "contact sales".** If it does not clearly satisfy (1) or (2), it is out.

Consequences, so they are not revisited:

- **abuse.ch ThreatFox — out.** Its CC0 grant was replaced (2025-11-04) by
  Spamhaus-managed terms: auth-gated, commercial use "may require a paid
  subscription", revocable. That is a stated-but-restrictive license, so it is
  neither (1) nor (2). We do **not** pursue the agreement, however good the data is.
- **`romainmarcoux/malicious-outgoing-ip` — out.** MIT label over re-bundled
  CC-BY-NC-SA data (drb-ra). The upstream term is explicit and a permissive label on
  the aggregate does not satisfy (1).
- **`drb-ra/C2IntelFeeds` — out.** CC-BY-NC-SA outright. (Best C2 data found; still out.)
- **Peter Lowe — out.** Publishes an explicitly non-commercial license, so it is not
  "no license". No permission email.
- **DShield — out.** CC BY-NC-SA. No permission email.
- **ET compromised-ips — in.** The file states no license, and ET's own rules are
  BSD; either way it satisfies (1) or (2). No vendor confirmation needed.
- **Binary Defense, AlienVault OTX, AbuseIPDB, GreenSnow, ipsum, FireHOL level1 — out.**
  Explicit prohibitions, or aggregates re-bundling them.

The commercial-licensing research is retained only as the record of *why* buying is
not a path: every affordable subscription (AbuseIPDB ~$228/yr, Spamhaus DQS ~$600/yr,
ET Pro ~€749/yr) is an **internal-use** license that still forbids shipping derived
data in client binaries, and genuine embedding/OEM rights are contact-sales at five
figures. Under this policy none of that is pursued.
- **Future**: aggressiveness tiers as a runtime setting (Conservative build already in the
  generator); DNS-level blocked-query counter for UI; mux-level consult of user overrides
  (unblock-at-DNS); CNAME-chain blocking; SNI sniffing on 443; revisit Perflyst SmartTV
  (MIT, stale — unique coverage if revived) and WindowsSpyBlocker telemetry IPs (MIT,
  frozen 2022) for the Windows port.
