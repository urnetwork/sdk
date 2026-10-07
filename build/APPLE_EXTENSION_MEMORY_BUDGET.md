# Apple network-extension memory budget

Baseline: 2026-08-15, iPhone 16 Pro Max, iOS 26.6, signed Release build.

## Runtime

The packet tunnel samples Go runtime accounting and `phys_footprint` together
every five seconds. The same parseable `[memory]` line is sent to OSLog and the
extension diagnostic log.

| Gauge | Connected Release maximum (12 samples / 55 seconds) |
| --- | ---: |
| Go runtime total | 19.635 MiB |
| `phys_footprint` | 20.876 MiB |
| Go soft limit | 32 MiB |
| Per-`DeviceLocal` target | 20 MiB |

The provider load regression test separately measured 26.0–27.4 MiB across 12
fresh darwin/arm64 processes. Its hard ceiling is 27.75 MiB, leaving 256 KiB
below the 28 MiB product target.

Retrieve a device sample from the extension's data container and search it for
`[memory]`. The current log is under `Library/Caches/Logs/URnetworkVPN.INFO`.

## Compiled size

`check_apple_size.sh` enforces these byte-count ceilings:

| Artifact | Release baseline | Ceiling |
| --- | ---: | ---: |
| Full iOS arm64 SDK archive | 54.993 MiB (2026-08-18; 53.897 on 2026-08-15) | 96 MiB |
| Extension iOS arm64 SDK archive | 52.070 MiB (2026-08-18; 51.168 on 2026-08-15) | 92 MiB |
| Signed extension executable | 37.164 MiB (2026-08-18, unsigned Release; 36.598 signed on 2026-08-15) | 48 MiB |

The SDK archive ceilings were raised to 96/92 MiB on 2026-09-13 for the
extender-network additions. On 2026-09-14 the signed extension executable
ceiling was raised from 39 to 48 MiB as an explicitly reviewed release budget.
These compiled-artifact ceilings do not change the runtime-memory limits or
the FIPS build-metadata gate. The release builder uses this shared check.

2026-10-06 (Go 1.27.1): removed two unintended `runtime/pprof` roots from the
iOS binding. `WriteHeapProfileForDiag` follows `WriteHeapProfile`'s `!ios`
constraint; glog's iOS fatal fallback uses a complete `runtime.Stack` dump
instead of retaining every registered profiler through `pprof.Lookup`.
Android/native profile writers and the cross-platform memory-class counters
remain available. `TestMobileHeapProfileDependencies` guards all Apple binding
views, including gomobile's macOS slices with the `ios` tag.

Rebuilding release `2026.10.6-1065229510` with both fixes reduced the extension
SDK archive from 80,852,248 to 80,643,064 bytes and the signed extension from
50,448,992 to 50,284,368 bytes (47.955 MiB). The complete iOS archive, signature
verification, and existing size/FIPS gate passed with the unchanged
50,331,648-byte extension ceiling.

A later 2026-10-06 build with newer provider-state code reached 50,370,464
bytes. The license catalog was the only runtime importer of `gopkg.in/yaml.v3`;
parsing that generated static file retained the YAML parser and reflected
encoder methods. The license generator now also emits `license_data.json`,
which the SDK reads with its existing JSON decoder. The YAML catalog remains
reviewable, every license check enforces parity, and tests compare all fields,
verbatim texts, app filters and ordering. No public SDK API was removed.

With production SDK source through `7f596064`, both iOS frameworks rebuilt
and the complete signed archive passed: full SDK 85,431,152 bytes, extension
SDK 80,434,776 bytes, signed extension 50,058,320 bytes (47.739 MiB). This
removes 312,144 bytes from the failed extension and leaves 273,328 bytes below
the unchanged ceiling. The signature and FIPS checks also passed.

2026-08-18 bump (55 → 56 MiB, 52 → 53 MiB): intentional growth from the
transport settings work — the per-carrier packet stats breakdown, the
client/provider transport policy with its rpc plumbing and change listeners,
and the transport distribution view controller. The app-facing policy helpers
(`transport_settings_view.go`) are kept out of the `ios_extension` binding.

The `ios_extension` binding omits app-only view-controller implementations. It
removes 2.729 MiB from the arm64 SDK archive and about 1.3 MiB from the final
extension executable versus the former monolithic binding.

## FIPS policy

The extension build sets `GOFIPS140=off`. The artifact gate rejects FIPS-enabled
Go build metadata, and the provider rejects a runtime `fips140=on` override.
Enabling FIPS requires a separately reviewed resident-memory budget because its
entropy implementation can physically back an additional 32 MiB scratch area.
