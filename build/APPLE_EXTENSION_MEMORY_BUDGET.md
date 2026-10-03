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
