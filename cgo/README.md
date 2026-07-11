# URnetwork SDK c abi

A c abi around the full sdk surface, embedded into native Windows and Linux
desktop apps as a single shared library. Targets Windows 10+ (amd64, arm64) and
Ubuntu 22.04+ (amd64, arm64; glibc >= 2.35).

Most of the abi is generated from the sdk surface — see `gen/gen.go` for the
mapping model and the curated classification lists, and `coverage_report.txt`
for exactly what is exported and what is skipped with the reason.

Two headers ship with the libraries:
- `include/urnetwork_sdk.h` — the raw c abi.
- `include/urnetwork_sdk.hpp` — a generated header-only c++17 wrapper
  (requires nlohmann/json): raii handle classes (`urnet::DeviceLocal`,
  `urnet::DeviceRemote`, ...; `urnet::Sub` closes on destruction), typed
  structs with json serialization for every data type, `std::function`
  callbacks, and `urnet::Error` exceptions. This is the intended api for the
  c++ apps; the c header is the stable abi underneath. The exported
surface tracks what the macOS app uses (the gomobile surface), including the
device rpc control flow (`urnet_generate_device_rpc_key_material`,
`urnet_device_local_set_rpc_server`, `urnet_new_device_remote_with_defaults`).

## abi contract

See the header comment in `include/urnetwork_sdk.h`. In short:

- objects are opaque `uint64_t` handles; release every returned handle with
  `urnet_release`. Releasing does not close/stop the object — call the object's
  `*_close`/`*_stop` first where one exists.
- returned `char*` are owned by the caller; free with `urnet_free_string`.
- structured data crosses as utf-8 json. Ids are uuid strings; times are unix
  epoch milliseconds (0 = none).
- callbacks fire on arbitrary threads and their arguments are only valid during
  the call; handles passed to callbacks are owned by the receiver.
- `urnet_live_handle_count` supports leak checks in app tests.

## building

The Makefile assumes a macOS build host with cross toolchains (`make init`):
mingw-w64 (windows/amd64), llvm-mingw (windows/arm64), and zig (linux, pinning
the ubuntu 22.04 glibc floor). `make` produces:

- `build/windows/{amd64,arm64}/URnetworkSdk.dll` + header + `urnetwork_sdk.def`,
  zipped as `build/URnetworkSdkWindows.zip`
- `build/linux/{amd64,arm64}/libURnetworkSdk.so` + header, zipped as
  `build/URnetworkSdkLinux.zip`

`make -C ../build build_windows` / `build_linux` delegate here, like `build_js`.

To consume from MSVC, generate an import library from the def file:
`lib /def:urnetwork_sdk.def /machine:x64 /out:URnetworkSdk.lib` (or link the
dll directly with lld). On linux, link with `-lURnetworkSdk`.

## regenerating

After changing the sdk surface, run `make generate` and commit the regenerated
files. Review the `coverage_report.txt` diff to confirm surface changes are
intentional. The generator fails on c name collisions and warns on data types
with empty json shapes (usually a type that should be classified behavioral).

## testing

`make smoke` builds a host (macOS) library and runs `smoke/smoke.cpp` against
it: strings, ids, buffer-out, json, handle lifecycle, and async callbacks.
