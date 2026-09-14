# URnetwork SDK for Rust

Default installation after the first registry publication:

```text
cargo add urnetwork-sdk
```

Build a local package with `make -C sdk/rust` and verify it with
`make -C sdk/rust check-package`. Native desktop builds reuse the CGo module;
set `SDK_NATIVE_MANIFEST` to package an already built platform matrix.

The native package exposes the complete generated raw C ABI plus owned Device
and Conn wrappers where applicable. Devices must be initialized using the SDK
authentication and connection setup before dialing. Close Devices and connections;
raw callback users must retain callbacks until their subscriptions are removed.

See [the package plan](../PACKAGEMANAGERS.md), [socket design](../SOCKET.md), and
[language examples](https://github.com/urnetwork/examples/tree/main/rust).

New package coordinates and distribution repositories are awaiting publication.
