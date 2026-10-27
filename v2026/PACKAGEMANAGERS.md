# SDK package managers and publishing

Research and implementation review: September 14, 2026. The source inventory is `../examples`: C, C++, C#, Go, Java, JavaScript, Kotlin, Python, Ruby, Rust, Swift, and TypeScript.

The installation goal is **`pip install urnetwork-sdk`**, **`npm install @urnetwork/sdk`**, and the equivalent native package command in each language. Package creation, consumer checks, and credential-gated publishing are implemented here. New public names and distribution repositories still need their first publication; this document does not claim that a registry install of an unpublished coordinate works.

## Distribution map

These are established ecosystem installation methods, not a numerical popularity ranking. `<version>` and `<commit>` are placeholders for a socket-capable release and a pinned source revision.

| Examples | Native package / common installers | GitHub installation | Owner |
| --- | --- | --- | --- |
| C, C++ | `urnetwork-sdk` in Conan 2 and vcpkg; CMake binary archive | Conan recipe or vcpkg registry/overlay selects a checked release archive; arbitrary Git source is not a prebuilt C library | `cgo/Makefile` |
| C# | `dotnet add package URnetwork.SDK`; NuGet, Visual Studio, Paket | Clone and build a local NuGet feed/project reference; NuGet has no native Git dependency syntax | `csharp/Makefile` |
| Go | `go get github.com/urnetwork/sdk/v2026@<version>`; Go modules | Native module tag/commit; `GOPROXY=direct` selects VCS fetching | Existing Go release stage |
| Java, Kotlin JVM | `io.ur:urnetwork-sdk:<version>` through Maven or Gradle | Clone and install the checked JAR/POM in Maven Local; JitPack is a separate build service, not Maven Git resolution | `java/Makefile`; `kotlin/Makefile` delegates |
| Java, Kotlin Android | `io.ur:urnetwork-sdk-android:<version>` through Maven or Gradle | Build the gomobile AAR from the checkout | `java/Makefile package-android` |
| JavaScript, TypeScript | `npm install @urnetwork/sdk`; pnpm, Yarn, Bun use the same npm registry | Install the built npm tarball; this monorepo's `js/` directory is not selected by npm's `repository.directory` field | `js/Makefile` |
| Python | `pip install urnetwork-sdk`; `uv add urnetwork-sdk`; `poetry add urnetwork-sdk` | pip VCS supports `git+https://github.com/urnetwork/sdk.git@<commit>#subdirectory=python`; source builds need the Go/C toolchain and SDK dependencies | `python/Makefile` |
| Ruby | `gem install urnetwork-sdk`; Bundler `gem "urnetwork-sdk"` | Bundler supports `git`, `ref` and `glob: "ruby/*.gemspec"`; prepare a native runtime for source development | `ruby/Makefile` |
| Rust | `cargo add urnetwork-sdk`; crates.io | Cargo Git dependencies locate the named crate; a source checkout needs `make -C rust native` or an explicit existing native library | `rust/Makefile` |
| Swift | SwiftPM/Xcode: `https://github.com/urnetwork/sdk-swift`; CocoaPods `URnetworkSdk`; Carthage binary JSON | SwiftPM uses the small Git distribution repository with a root manifest and immutable XCFramework URL | `swift/Makefile` |

Primary installation references: npm, pnpm, Yarn and Bun [1]; Go modules [2]; Python installers [3]; Bundler [4]; Cargo [5]; NuGet [6]; Maven/Gradle/JitPack [7]; SwiftPM [8]; Conan/vcpkg [9].

Secondary installers which consume the same registry need no second publisher: npm/pnpm/Yarn/Bun, pip/uv/Poetry, Maven/Gradle, RubyGems/Bundler, NuGet/Paket. Conda-forge is an optional future Python distribution: it requires a separate feedstock and maintainer/CI acceptance, not another PyPI upload. The normal Python package implemented here is a wheel on PyPI.

## Three base SDKs

No additional networking engine is needed. Go imports the root SDK, JS/TS wrap the existing Go/WASM SDK, and desktop C/C++/Java/Kotlin/C#/Python/Ruby/Rust share the CGo module's C ABI. Android keeps gomobile's AAR and Swift keeps its XCFramework.

The desktop wrappers generate the full raw C ABI from `cgo/include/urnetwork_sdk.h` and add owned Handle, Device and Conn objects. Python uses ctypes, Java JNA, C# P/Invoke/SafeHandle, Ruby FFI, and Rust dynamically loads the shared engine. They do not fork routing, DNS, socket behavior or TLS implementations.

`urnet_abi_version()` is checked before loading wrapper APIs. FFI declarations retain one-byte C booleans, fixed-width integers, UTF-8 ownership, callbacks, binary data, partial read/write results, and explicit EOF. The generator respects Windows-only omissions such as the Unix I/O-loop entry point. Raw callback users must keep callback objects alive until their subscriptions are removed. A handle belongs to the runtime which created it.

Native package manifests record ABI, source/native version, OS/architecture, minimum OS/libc and SHA-256. Desktop release targets are macOS, Linux/glibc and Windows on arm64/amd64; the configured Windows architecture list may restrict that release. Only actual manifest entries are packaged. Host checks validate the current platform; full cross-platform release qualification remains necessary before expanding support claims. Mobile uses the existing iOS 16, macOS 13.5 and Android API 24 build targets.

## Go preparation and Makefile contract

Shared helper programs are Go in `packaging/`, a standard-library-only Go module. They build manifests, generate bindings, create archives, validate consumer packages, and publish registries. Python is retained only where the ecosystem requires it: setuptools' Python backend, Conan's Python recipe, and Python-language examples/tests. Native language compiler/package-manager commands remain their official tools.

| Target | Action |
| --- | --- |
| `make package` | Build local artifacts and a manifest. Does not publish. |
| `make check-package` | Install/load the artifacts in a clean consumer and record the checked manifest digest. |
| `make publish` | Skip if credentials are missing; otherwise publish those checked artifacts. Does not rebuild them. |
| `make native` | Prepare the local native runtime for source-development wrappers. |
| `make generate` | Regenerate raw FFI declarations. |

`cgo` uses `publish-conan` and `publish-vcpkg`; Swift additionally uses `publish-cocoapods`. Kotlin delegates to Java, and JS/TS share one owner. There is no duplicate publication per example language.

For C#, `make -C csharp init` installs the host .NET 8 SDK and runtime through Homebrew on macOS, or reuses an existing compatible installation. `make -C csharp check-tools` validates setup without installing anything. Both targets use the shared Go helper. On other platforms, install .NET 8 for the host architecture before building. Go, Make, and the native compiler toolchain are prerequisites.

`build/all/run.sh` checks required host commands before repository pulls, signing setup, or builds. Additional registry tools are required only when that registry's publishing credentials are set. NuGet uses the C# helper to check SDK version, runtime availability, and architecture, including Homebrew installations outside `PATH`. Missing tools fail the release early; setup belongs in `make init`.

Common inputs:

- `SDK_PACKAGE_VERSION`: external SemVer release identity; local default `0.0.1-dev.0`.
- `WARP_VERSION`: the SDK's embedded native version, retained separately from package-manager spelling.
- `SDK_NATIVE_MANIFEST`: reuse already built, checksum-checked platform libraries. Without it, desktop package builds compile the current host through `sdk/cgo`.
- `SDK_PACKAGE_OUT`: optional desktop package output directory.
- `SDK_DOTNET`: optional explicit .NET executable for C# builds and tool checks; otherwise discover the host installation.
- `SDK_PACKAGE_CHANNEL`: npm tag, default `nightly`.
- `SDK_XCFRAMEWORK_ZIP` / `SDK_XCFRAMEWORK_URL`: checked Apple artifact and immutable public URL.
- `SDK_RUST_RELEASE_ASSETS` / `SDK_C_RELEASE_ASSETS`: release asset staging destinations.

Examples:

```sh
make -C python package check-package
make -C java package check-package
make -C csharp init
make -C csharp package check-package
make -C ruby package check-package
make -C rust package check-package
make -C cgo package check-package
make -C js package check-package
make -C swift package check-package SDK_XCFRAMEWORK_ZIP=/path/to/URnetworkSdk.xcframework.zip
go -C packaging test -race ./...
```

Desktop outputs are `<language>/dist`, npm outputs are `js/release`. Manifests inventory exact artifact bytes. A successful check writes `checked.json`; publication fails if a filename, size, hash, manifest, or version differs. Package output/cache directories are ignored by Git.

## Package formats and publication

### npm: canonical name and compatibility

The canonical package is **`@urnetwork/sdk`**, including ESM/CJS, declarations, React entry point, WASM, and the matching Go runtime glue. The staged declarations use valid NodeNext relative imports. The loader supports browsers and Node; examples use Node 24+.

The release also creates **`@urnetwork/sdk-js`** as a thin compatibility package with an exact dependency on the canonical version. It re-exports the same modules and class identities. Publish canonical first, compatibility second; do not ship a second WASM runtime under the alias. `check-package` verifies installed ESM/CJS identity, TypeScript imports and asset paths. The release retains the existing `nightly` channel unless explicitly overridden.

The npm release stage always builds/checks packages. When npm publishing credentials are absent, it skips upload; the extension keeps its existing published SDK/localization dependencies, since no new registry version was created.

The simple unqualified install commands target the registry's default release. This pipeline's numeric external versions are prereleases, and npm uses `nightly` by default: preview consumers use `npm install @urnetwork/sdk@nightly`, `pip install --pre urnetwork-sdk`, `dotnet add package URnetwork.SDK --prerelease`, `gem install urnetwork-sdk --pre`, or pin an exact prerelease with Cargo/Maven/SwiftPM. Publishing npm with `SDK_PACKAGE_CHANNEL=latest` makes that version the unqualified npm install. Stable native versions need an explicit release/promotion decision; do not silently turn every nightly build into a stable release.

### Python: platform wheels

`python/Makefile` builds platform-specific `py3-none-<platform>` wheels with the native library included. The import is `import urnetwork`. Normal wheel consumers need no Go compiler, sibling checkout, or manual DLL download. The macOS wheel tag rounds the runtime's minimum OS upward when needed; Linux tags state the glibc floor. Musl is not advertised as a glibc wheel.

The Python build backend allows explicit source/Git development, where the native Go/C toolchain and core SDK dependencies are required. The release publisher uploads **wheels only**, so a missing platform cannot silently fall back to an incomplete source install. Twine checks the selected wheels, then uploads the exact files. [3][10]

### Java/Kotlin and Android: Maven Central

The desktop JAR contains JNA declarations, lifetime/Conn wrappers, and platform runtimes. Maven metadata declares JNA transitively and includes a POM, sources and Javadocs. Desktop Java targets Java 17+. Kotlin/JVM consumes that artifact directly.

The Android artifact is a separate gomobile AAR with sources, real generated Javadocs and its own POM. `package-android` adds the AAR artifacts to the matching desktop Java manifest. Do not substitute desktop JNA for Android's native binding. Android documentation generation needs the Android SDK's `android.jar`.

The Go publisher signs artifacts with GPG, creates the Maven repository bundle and checksums, uploads it to Central's Portal API, records its deployment ID, and polls for publication with a bounded deadline. Namespace ownership and signing setup are prerequisites. [7][11]

### C#: NuGet

The .NET 8 wrapper uses UTF-8 P/Invoke declarations and SafeHandle ownership. The NuGet package contains `runtimes/<rid>/native` assets selected from the manifest. Its isolated consumer restores from the built feed and loads the SDK.

The macOS M1 build host uses the ARM64 .NET 8 SDK installed by `make -C csharp init`. The package build and isolated consumer share SDK selection through `global.json` and runtime discovery through the Go helper. See [C# build setup](csharp/README.md).

The Go publisher uploads the exact checked `.nupkg` using NuGet's package-publish protocol. Mobile RIDs, Native AOT, trimming and single-file deployment are separate qualification tasks; the presence of a generic desktop RID does not establish those modes. [6][12]

### Ruby: platform gems

`ruby/Makefile` builds platform gems over FFI, with the matching native engine. RubyGems/Bundler select the platform variant. The wrapper provides explicit close/block ownership and structured partial-I/O errors. The checked host consumer uses CRuby; alternate engines need their own validation.

The Go publisher posts each checked gem to RubyGems' API. Source/Bundler Git development requires a prepared native runtime. [4][13]

### Rust: small crate, pinned build-time native asset

A host runtime made the original crate approximately 15 MiB, beyond the ordinary crates.io upload limit. The implemented release crate instead contains generated raw bindings, safe ownership/I/O wrappers and a `build.rs` manifest of immutable native assets. Cargo's build step downloads the **exact versioned gzip**, verifies both compressed and uncompressed SHA-256, and embeds the runtime bytes in the executable. This is not a download of “latest” and requires no Go toolchain for the consumer.

`SDK_RUST_NATIVE_CACHE` supports checked offline/pre-publication assets. `URNETWORK_SDK_LIBRARY` is an explicit runtime override for source/system-library development. Unsupported OS/CPU/libc targets fail with an explanation. Unix unlinks the temporary loaded image after mapping; Windows retains its file for the process lifetime.

The public release waits until every referenced GitHub asset is accessible and matches its checksum. The Go publisher reads metadata from the checked `.crate`, verifies its manifest and sends that exact archive through Cargo's documented publish protocol, preserving normal/build dependencies and renamed package names. [5][14]

### SwiftPM, CocoaPods and Carthage

`swift/Makefile` consumes the existing gomobile XCFramework ZIP. It creates a SwiftPM manifest with the immutable URL and checksum, a CocoaPods spec and a Carthage binary JSON manifest. SwiftPM/CocoaPods propagate the static Go runtime's `resolv` link dependency; direct XCFramework/Carthage clients must link it.

Swift publication pushes a new immutable version tag to `urnetwork/sdk-swift` (or the configured distribution repository) only after the public asset hash matches. It never moves an existing tag. Carthage consumes the versioned JSON in that repository, so it needs no separate registry credential. CocoaPods is a separate credential-gated trunk upload. Its announced registry transition makes SwiftPM the long-term default; retain CocoaPods as compatibility while supported. [8][15]

### C/C++: archives, Conan and vcpkg

`cgo/Makefile` produces one archive per native target containing the library, C/C++ headers, export definition, license, and relocatable CMake imported targets `urnetwork::sdk` and `urnetwork::sdk-cpp`. macOS install names use `@rpath`; Windows archives include an LLVM/COFF import library generated with `zig dlltool`. The C++ generated facade uses nlohmann/json; vcpkg declares it and Conan's `cpp=True` option adds it.

The Conan recipe is Python because Conan requires that format. All recipe rendering, native archiving and publication orchestration is Go. When Conan is enabled, package creation assembles recipe/binary cache artifacts for the matrix from checked local ZIPs. The checks restore that cache and validate Conan integrity. Publication restores and uploads the checked cache/list; it does not rebuild a runtime.

The vcpkg port selects an immutable archive with SHA-512. Publication updates a first-party Git registry's port tree, version database and baseline in a fast-forward push; it does not overwrite an existing version. Consumers pin a registry baseline.

**One-time registry configuration is still required** until these recipes are accepted into upstream Conan Center / Microsoft's vcpkg registry. Their maintainers control acceptance; this pipeline cannot publish directly into those upstream collections like PyPI. Configure the first-party remote/registry explicitly:

```sh
conan remote add urnetwork https://<your-public-conan-remote>
conan install --requires=urnetwork-sdk/<version> --remote=urnetwork --build=missing

# vcpkg: add urnetwork/vcpkg-registry and its immutable baseline to
# vcpkg-configuration.json, then:
vcpkg add port urnetwork-sdk
vcpkg install
```

A local CMake consumer check loads the packaged ABI; Conan checks validate cache integrity. Windows import-library linking and all cross-platform registry consumers require release-platform qualification. [9][16]

## build/all/run.sh integration

The script's existing SDK/npm stage now prepares the registry plan and calls `make package check-package publish` next to the JS SDK. New wrapper directories and the Go packaging module are preserved by `go_mod_fork` so source subdirectory installation continues to work.

1. Record release start time and enabled/skipped registries. Build/check npm and upload only when npm credentials exist.
2. After mobile artifacts are built, prepare/check Swift packages when their publisher is enabled.
3. After Linux/Windows outputs exist, build the macOS C ABI matrix and assemble a native manifest. Require fresh output from this release; unpack the freshly retrieved Windows ZIP rather than using stale host-side DLLs.
4. Build/check each selected native owner once. Publish PyPI, NuGet, RubyGems and Maven artifacts. Rust, Conan and vcpkg wait for their public binary URLs.
5. Upload staged Rust/C binary assets into the existing GitHub release, then finalize it.
6. Verify public asset checksums and publish Rust, Conan, vcpkg, SwiftPM/Carthage and CocoaPods.

Missing registry credentials skip that registry before reading its artifact directory or invoking its publishing tool. A configured registry which rejects an upload fails the stage through `error_trap`. A missing build tool or incomplete required artifact is an error when its registry is enabled, not a reason to silently skip.

| Registry | Credentials / configuration |
| --- | --- |
| npm | `NPM_TOKEN` or `NODE_AUTH_TOKEN`; existing npmjs login in the project/user npmrc is also recognized |
| PyPI | `PYPI_TOKEN` or `TWINE_PASSWORD` (token username `__token__`) |
| NuGet | `NUGET_API_KEY` |
| RubyGems | `GEM_HOST_API_KEY` |
| crates.io | `CARGO_REGISTRY_TOKEN` |
| Maven Central | All of `MAVEN_CENTRAL_USERNAME`, `MAVEN_CENTRAL_PASSWORD`, `SDK_GPG_KEY_ID`; GPG key must be available locally |
| SwiftPM/Carthage | `SDK_SWIFT_GIT_TOKEN`; optional `SDK_SWIFT_REPOSITORY=owner/repo` |
| CocoaPods | `COCOAPODS_TRUNK_TOKEN` |
| Conan | `CONAN_LOGIN_USERNAME` and `CONAN_PASSWORD`; configured `SDK_CONAN_REMOTE_URL` |
| vcpkg first-party registry | `SDK_VCPKG_GIT_TOKEN`; optional `SDK_VCPKG_REPOSITORY=owner/repo` |

Go module publishing remains the script's existing Git/tag operation. Its credentials and failure handling are unchanged. No new third-party registry is involved.

Versions derive from the existing release identity. Numeric external prereleases map to Python `.dev<code>` and Ruby `.pre.<code>`; ordinary SemVer prereleases have explicit Python/Ruby mappings. Native `WARP_VERSION` stays distinct. Version conversion, digest validation, absent/partial credentials, exact upload bytes and error handling have Go tests.

The multi-registry release is not atomic. Publishers record manifests and successful registry receipts; Maven records its deployment ID before polling. A failure after an earlier successful publication leaves a partial release. Reconcile existing immutable versions explicitly before retrying; do not use force-overwrite or blanket “skip existing.” Automatic public-registry install/promotion across the entire platform matrix remains a release qualification step.

**Do not execute `build/all/run.sh` merely to validate packaging changes.** It commits, tags, publishes assets/apps and sends release notifications; `WARP_SKIP_DEPLOY` does not make those other effects read-only. This implementation was checked with package targets, isolated consumers and release-contract tests, without publishing a registry release.

## Executable examples and documentation

Each `examples/<language>/README.md` has its native installation guide. Each `examples/<language>/socket/` contains executable sources and a `README.md` with setup, run commands, socket semantics, the chosen HTTP integration hooks and primary research sources. JavaScript has both Node and browser programs.

The Developer SDK documentation includes Install, Socket and an Examples mini site. Its Go generator snapshots all twelve language source trees, including nested browser/Node programs and build manifests, while excluding generated outputs, dependencies and local credentials. Source links point back to the examples repository.

Socket semantics, TLS/DTLS, Happy Eyeballs and the JavaScript Direct Sockets client profile are specified in [SOCKET.md](SOCKET.md). Plain UDP races the initial datagram and accepts the first reply; the documentation explicitly covers possible duplicate initial delivery.

## Sources

[1] [npm install](https://docs.npmjs.com/cli/v11/commands/npm-install/), [pnpm sources](https://pnpm.io/package-sources), [Yarn Git](https://yarnpkg.com/protocol/git), [Bun add](https://bun.com/docs/pm/cli/add).

[2] [Go publishing modules](https://go.dev/doc/modules/publishing).

[3] [pip VCS](https://pip.pypa.io/en/stable/topics/vcs-support/), [uv dependencies](https://docs.astral.sh/uv/concepts/projects/dependencies/), [Poetry dependencies](https://python-poetry.org/docs/dependency-specification/), [Python version specification](https://packaging.python.org/en/latest/specifications/version-specifiers/).

[4] [Bundler Git dependencies](https://bundler.io/guides/git.html), [RubyGems publication](https://guides.rubygems.org/publishing/).

[5] [Cargo dependencies](https://doc.rust-lang.org/cargo/reference/specifying-dependencies.html), [Cargo publication](https://doc.rust-lang.org/cargo/reference/publishing.html).

[6] [NuGet CLI installation](https://learn.microsoft.com/en-us/nuget/consume-packages/install-use-packages-dotnet-cli), [NuGet native assets](https://learn.microsoft.com/en-us/nuget/create-packages/native-files-in-net-packages).

[7] [Gradle Maven publishing](https://docs.gradle.org/current/userguide/publishing_maven.html), [JitPack](https://docs.jitpack.io/).

[8] [Swift PackageDescription](https://docs.swift.org/package-manager/PackageDescription/PackageDescription.html).

[9] [Conan package creation](https://docs.conan.io/2/tutorial/creating_packages/create_your_first_package.html), [vcpkg Git registries](https://learn.microsoft.com/en-us/vcpkg/consume/git-registries).

[10] [PyPI upload API](https://docs.pypi.org/api/upload/).

[11] [Maven Central Portal API](https://central.sonatype.org/publish/publish-portal-api/).

[12] [NuGet package-publish API](https://learn.microsoft.com/en-us/nuget/api/package-publish-resource).

[13] [RubyGems API](https://guides.rubygems.org/rubygems-org-api/).

[14] [Cargo publish wire protocol](https://doc.rust-lang.org/cargo/reference/registry-web-api.html#publish).

[15] [CocoaPods trunk](https://guides.cocoapods.org/making/getting-setup-with-trunk.html), [CocoaPods registry transition](https://blog.cocoapods.org/CocoaPods-Specs-Repo/).

[16] [Conan credentials](https://docs.conan.io/2/reference/config_files/credentials.html), [Conan upload](https://docs.conan.io/2.12/reference/commands/upload.html), [vcpkg registry database](https://learn.microsoft.com/en-us/vcpkg/maintainers/registries), [vcpkg checksum downloads](https://learn.microsoft.com/en-us/vcpkg/maintainers/functions/vcpkg_download_distfile).
