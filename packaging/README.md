# Language packages

The Go SDK, Go/WASM JS SDK, and CGo C ABI are the three implementations.
Python, Ruby, desktop Java/Kotlin, C#, and Rust wrap the C ABI. Android uses
the existing gomobile AAR; Swift uses the existing Apple XCFramework.

Run `make -C python` (or another language directory) to build a local package.
`make check-package` tests the packaged runtime. No default target publishes.
Set `SDK_PACKAGE_VERSION` for release builds; the development default is
`0.0.1-dev.0`. Set `SDK_NATIVE_MANIFEST` to reuse the complete native matrix.
Without it the build uses the local `sdk/cgo` Go module for the host platform.

For C# on the macOS build host, run `make -C csharp init` from the SDK root
to install .NET 8 through Homebrew. `make -C csharp check-tools` checks the
SDK, runtime, and architecture without installing tools. Normal C# builds and
the release runner use the same Go discovery helper, including Homebrew's
keg-only installation. `SDK_DOTNET` selects an explicit executable if needed.

The native manifest is JSON with `abi: 1`, `version`, and `libraries`, each with
`platform`, absolute `path`, `sha256`, and `minimum_os`. Platforms are
`darwin-arm64`, `darwin-amd64`, `linux-arm64`, `linux-amd64`,
`windows-arm64`, and `windows-amd64`. Linux artifacts must state their glibc
floor; the current release toolchain uses 2.35. Packaging never relabels these
as musl or an older manylinux baseline.

The Go generator in `generate.go` reads the generated C header and emits the complete raw
FFI declarations for every C ABI function and callback. The small handwritten
layers own Devices and sockets, preserve partial I/O and empty datagrams, and
load the packaged library. Raw users must retain callback objects until the
native subscription is removed, copy borrowed callback buffers, and close
objects before releasing their handles. See `cgo/include/urnetwork_sdk.h`.

All shared build, generation and publishing helpers are Go programs in this
module. Run `go -C packaging test -race ./...` from the SDK root to test the
release helpers. Python remains only for the Python SDK/setuptools backend and
Conan's required recipe format. Each publisher skips when its credentials are
absent and otherwise uploads the exact artifacts recorded by `check-package`.

Registry names and first publications still require registry setup. Build
artifacts are local and reviewable; see `../PACKAGEMANAGERS.md` for publishing,
Git installation, platform qualification, and secondary package managers.
