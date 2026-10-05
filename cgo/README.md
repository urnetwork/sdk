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

## the messaging abi

`exports_message.go` + `include/urnetwork_message.h` are the messaging surface:
**38** exports over `sdk/urmessage` and `sdk`'s own message client, hand-written
rather than generated. The query, so the number is checkable rather than quoted:
`bash ctest/run.sh` prints it, and it is
`grep -cE '^extern .*urnet_message_' build/host/URnetworkSdk.h`. The reason
is at the top of `exports_message.go` and in short it is that the generator
walks package `sdk` and the messenger is not in it. The header is independent of
`urnetwork_sdk.h`; both ship, and the cgo-emitted header beside the library
declares both.

Three decisions a reader should not have to reverse-engineer, each argued at the
line in `exports_message.go`:

- **blocking, on your own thread, with a cancel handle.** `Connect`, `Open`,
  `Send` and `Receive` block. `Connect`'s budget is **90 seconds**, because a
  reconnecting client is not routed to by the operator for about sixty. Pass a
  `urnet_message_context` handle and cancel it from any thread.
- **a body is counted octets, never a `char*` and never inside json.** A Go
  string here is not text: the seal path takes `[]byte` and the open path hands
  `[]byte` back, unvalidated. On the C test's own 21-octet body a `char*` loses
  9 octets and json turns it into 27 carrying three U+FFFD — measured in
  `exports_message_test.go`.
- **nothing is stubbed.** No receipts, reactions, replies, edit, delete or
  media. They are not built underneath this, and an export answering a plausible
  empty result would be worse than no export.

`urnet_message_transport_new` takes a connect client handle, and **this abi now
produces one**: `urnet_message_client_new` dials `wss://connect.<host>` with an
operator-minted `by_client_jwt` and sets the provide modes the message server's
replies need. Until it existed, a C caller could reach an in-process loopback
server and nothing else. What is still open of S2-7 is the **credential** and
only the credential — minting a `network_client` `ByJwt` is an operator-admin
action nothing in connect, sdk or this binding performs.

**That path is unexercised against a real operator.** No test here or in `sdk`
can reach one, and a `ByJwt` cannot be forged. What is under test is the shape:
which arguments are refused, what the platform url derives to, that the client
carries the credential's own `client_id`, that the provide modes are set, and
that a transport binds over it. Not that a frame crossed.

`include/urnetwork_sdk.def` names every hand-written export that ships, and
`gen.TestTheDefNamesEveryHandWrittenExportThatShips` is what keeps it that way —
it used to name 0 of the messaging surface while a test logged the gap and
passed. Nothing at runtime reads the def; it builds MSVC import libraries.

## testing

`make smoke` builds a host (macOS) library and runs `smoke/smoke.cpp` against
it: strings, ids, buffer-out, json, handle lifecycle, and async callbacks.

`make ctest` (or `bash ctest/run.sh`) is the messaging one, and it is a **C**
program: `ctest/message_abi_test.c` opens two devices, founds a group, joins it,
sends octets, reads them back one at a time, holds `urnet_live_handle_count`
across the whole conversation, and cancels a blocking `Connect` from a second OS
thread. It builds the library **twice** — once as it ships, once with
`-tags urnet_message_loopback -modfile=loopback.go.mod`, which adds
`loopback_test_world.go`: a real in-process message server, wired the way
`sdk/cp3b` wires one, because there is no operator here for the shipping
`urnet_message_client_new` to dial and the conversation has to be a real one.
The script **fails if the shipping header declares one
`urnet_message_loopback_*` symbol**, and the harness's dependency lives only in
`loopback.go.mod`, so deleting its build tag breaks the build rather than
shipping it.

It also fails on the word `panicked` anywhere in the run. `cgoGuard` recovers a
panic so it cannot unwind into C, which means an out-of-range index and a
deliberate refusal are indistinguishable from the C side — both answer false —
so a bounds check can be deleted with every assertion still green. The panic
line is the only thing that sees it.

`go test ./... ` in this module holds the rest, including `-race` over the
handle registry. On Windows the `-race` **c-shared** library builds but cannot
load — ThreadSanitizer cannot map its shadow memory into an already-running
process — and `ctest/run.sh` prints that where it tries rather than skipping it.

## regenerating

After changing the sdk surface, run `make generate` and commit the regenerated
files. Review the `coverage_report.txt` diff to confirm surface changes are
intentional. The generator fails on c name collisions and warns on data types
with empty json shapes (usually a type that should be classified behavioral).

The Python, Java, C#, Rust and Ruby packages bind `include/urnetwork_sdk.h`
through `../packaging/generate.go`. When the header changes, also run
`go -C ../packaging run . generate` and commit those five files. `go test ./gen`
fails until both the header and the bindings are current.

`manualExports()` publishes the hand-written `//export`s into the `.def`. It
skips files the default build excludes (`inDefaultBuild`) — a `.def` naming a
symbol the dll lacks is a link error at the consumer — and its scan pattern
tolerates CRLF, which every Windows clone of this repo has and which used to
make it find nothing at all.
## testing

`make smoke` builds a host (macOS) library and runs `smoke/smoke.cpp` against
it: strings, ids, buffer-out, json, handle lifecycle, and async callbacks.

`make smoke_memory_usage_json` checks the shipped C++ memory snapshot's JSON
round-trip and legacy defaults without linking the SDK library. It also runs
as part of `make smoke_hpp`.
