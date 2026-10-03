# URnetwork SDK for C#

Default installation after the first registry publication:

```text
dotnet add package URnetwork.SDK
```

From the SDK checkout, prepare the build tools and build a local package:

```sh
make -C csharp init
make -C csharp
make -C csharp check-package
```

On macOS, `init` reuses a compatible .NET 8 SDK or installs `dotnet@8` with
Homebrew. Go, Make, Homebrew, and the Xcode command-line tools must already be
installed. An Apple Silicon build uses the ARM64 SDK and runtime. The Go helper
finds Homebrew's installation without requiring shell profile or `PATH` edits.
See the [Homebrew formula](https://formulae.brew.sh/formula/dotnet@8).

On other hosts, install the .NET 8 SDK for the host architecture using
[Microsoft's installation instructions](https://learn.microsoft.com/en-us/dotnet/core/install/).
The helper checks `PATH`, `DOTNET_ROOT`, and standard installation locations.
Set `SDK_DOTNET` to a specific `dotnet` executable to override discovery. Both
the .NET 8 SDK and its runtime are required; `global.json` selects the latest
installed stable 8.0 SDK. A newer major SDK alone does not satisfy this build.

`make -C csharp check-tools` checks the SDK, runtime, and host architecture
without installing tools. Normal builds also check before compiling CGo and
report `make init` when setup is missing. The release runner performs the same
check near the start when `NUGET_API_KEY` enables NuGet publication.

Native desktop builds reuse the CGo module;
set `SDK_NATIVE_MANIFEST` to package an already built platform matrix.

The native package exposes the complete generated raw C ABI plus owned Device
and Conn wrappers where applicable. Devices must be initialized using the SDK
authentication and connection setup before dialing. Close Devices and connections;
raw callback users must retain callbacks until their subscriptions are removed.

See [the package plan](../PACKAGEMANAGERS.md), [socket design](../SOCKET.md), and
[language examples](https://github.com/urnetwork/examples/tree/main/csharp).

New package coordinates and distribution repositories are awaiting publication.
