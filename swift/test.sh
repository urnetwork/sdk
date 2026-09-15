#!/usr/bin/env zsh
set -euo pipefail

script_dir="${0:A:h}"
cd "$script_dir"

if [[ -n "${SDK_XCFRAMEWORK_ZIP:-}" ]]; then
    artifact="${SDK_XCFRAMEWORK_ZIP:A}"
else
    artifact="$script_dir/../build/apple/URnetworkSdk.xcframework.zip"
fi
needs_build=0
if [[ ! -s "$artifact" ]]; then
    needs_build=1
elif ! unzip -p "$artifact" '*/Headers/Sdk.objc.h' 2>/dev/null | grep -F 'openSocket:' >/dev/null; then
    # A retained archive from before the Socket API was added is just as
    # unusable as a missing archive. Rebuild it instead of reporting a stale
    # package as a passing Swift smoke.
    needs_build=1
fi
if (( needs_build )); then
    echo "Swift SDK smoke: Apple XCFramework is missing or stale; building it first"
    make -C "$script_dir/../build" build_apple
    artifact="$script_dir/../build/apple/URnetworkSdk.xcframework.zip"
fi
if [[ ! -s "$artifact" ]]; then
    echo "Swift SDK smoke: missing XCFramework: $artifact" >&2
    exit 1
fi

echo "Swift SDK smoke: compiling and running a Swift Package consumer"
SDK_XCFRAMEWORK_ZIP="$artifact" make package check-package
