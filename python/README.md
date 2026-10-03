# URnetwork SDK for Python

Default installation after the first registry publication:

```text
pip install urnetwork-sdk
```

Build a local package with `make -C sdk/python` and verify it with
`make -C sdk/python check-package`. Native desktop builds reuse the CGo module;
set `SDK_NATIVE_MANIFEST` to package an already built platform matrix.

The native package exposes the complete generated raw C ABI plus owned Device
and Conn wrappers where applicable. Devices must be initialized using the SDK
authentication and connection setup before dialing. Close Devices and connections;
raw callback users must retain callbacks until their subscriptions are removed.

See [the package plan](../PACKAGEMANAGERS.md), [socket design](../SOCKET.md), and
[language examples](https://github.com/urnetwork/examples/tree/main/python).

New package coordinates and distribution repositories are awaiting publication.
