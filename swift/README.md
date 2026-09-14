# URnetwork SDK for Swift

Add `https://github.com/urnetwork/sdk-swift` in Xcode or a SwiftPM manifest, selecting a socket-capable release. The distribution repository's first publication is pending.

Swift uses the existing **gomobile XCFramework**, with `SdkDeviceLocal`, `SdkSocket` and `SdkSocketTLSOptions`. It does not use the desktop CGo/JNA ABI. `openSocket` with nil TLS options opens plain TCP/UDP; nonnil options select TLS/DTLS. Perform these blocking operations on a worker queue and close the Socket before its Device.

Build the Apple SDK first, then package its output:

```sh
make -C sdk/swift package check-package SDK_XCFRAMEWORK_ZIP=/path/to/URnetworkSdk.xcframework.zip
```

The Go helper generates an immutable-URL/checksum SwiftPM manifest, CocoaPods spec, and Carthage JSON. SwiftPM/CocoaPods include the static runtime's `resolv` dependency. Direct XCFramework/Carthage consumers must link `libresolv`. The SDK baseline is iOS 16 and macOS 13.5; the URLSession proxy example uses macOS 14 APIs.

`make publish` skips without `SDK_SWIFT_GIT_TOKEN`. `make publish-cocoapods` skips without `COCOAPODS_TRUNK_TOKEN`. The pipeline waits for the exact public XCFramework asset before publishing either distribution.

See the [executable Swift socket example](https://github.com/urnetwork/examples/tree/main/swift/socket), [socket design](../SOCKET.md), and [package plan](../PACKAGEMANAGERS.md).
