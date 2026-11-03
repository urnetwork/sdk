#!/bin/sh
# Enforce the shipped iOS SDK and network-extension executable budgets.
set -eu

# These are artifact-size regression ceilings, not runtime-memory limits.
# Override them only for an explicitly reviewed budget change.
sdk_max_bytes="${URNETWORK_IOS_SDK_MAX_BYTE_COUNT:-100663296}" # 96 MiB (raised from 64 on 2026-09-13: the extender network added libp2p, the extender directory and the alt carriers, measured at 72.0 MiB)
extension_sdk_max_bytes="${URNETWORK_IOS_EXTENSION_SDK_MAX_BYTE_COUNT:-96468992}" # 92 MiB (raised from 60 on 2026-09-13 with the sdk ceiling: the extender network, measured at 67.7 MiB)
extension_max_bytes="${URNETWORK_IOS_EXTENSION_MAX_BYTE_COUNT:-52428800}" # 50 MiB

sdk_path=""
extension_sdk_path=""
extension_path=""

usage() {
    echo "usage: $0 [--sdk <xcframework-or-arm64-archive>] [--extension-sdk <xcframework-or-arm64-archive>] [--extension <appex-or-executable>]" >&2
    exit 2
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --sdk)
            [ "$#" -ge 2 ] || usage
            sdk_path="$2"
            shift 2
            ;;
        --extension)
            [ "$#" -ge 2 ] || usage
            extension_path="$2"
            shift 2
            ;;
        --extension-sdk)
            [ "$#" -ge 2 ] || usage
            extension_sdk_path="$2"
            shift 2
            ;;
        -h|--help)
            usage
            ;;
        *)
            usage
            ;;
    esac
done

[ -n "$sdk_path" ] || [ -n "$extension_sdk_path" ] || [ -n "$extension_path" ] || usage

if [ -d "$sdk_path" ]; then
    if [ -f "$sdk_path/ios-arm64/URnetworkSdk.framework/URnetworkSdk" ]; then
        sdk_path="$sdk_path/ios-arm64/URnetworkSdk.framework/URnetworkSdk"
    elif [ -f "$sdk_path/URnetworkSdk" ]; then
        sdk_path="$sdk_path/URnetworkSdk"
    fi
fi

if [ -d "$extension_sdk_path" ]; then
    if [ -f "$extension_sdk_path/ios-arm64/URnetworkExtensionSdk.framework/URnetworkExtensionSdk" ]; then
        extension_sdk_path="$extension_sdk_path/ios-arm64/URnetworkExtensionSdk.framework/URnetworkExtensionSdk"
    elif [ -f "$extension_sdk_path/URnetworkExtensionSdk" ]; then
        extension_sdk_path="$extension_sdk_path/URnetworkExtensionSdk"
    fi
fi

if [ -d "$extension_path" ]; then
    extension_path="$extension_path/URnetworkVPN"
fi

# Go's linker drops the exported methods that nothing can reach, about 7 MiB of the
# extension, until a reachable function calls reflect's Method or MethodByName with
# a name it can't read. Then it keeps all of them, and with them
# reflect.(*rtype).Methods, which nothing here calls and which makes such a call
# itself. So that name is in a binary exactly when pruning is off, and it stays in
# the function table whatever -s and strip do to the symbols.
check_method_pruning() {
    label="$1"
    path="$2"
    status=0
    LC_ALL=C grep -a -q -F 'reflect.(*rtype).Methods' "$path" || status=$?
    case "$status" in
        0)
            echo "[apple-size] $label keeps every exported method of every type, because a reachable function calls reflect's Method or MethodByName with a computed name or names a method Methods" >&2
            echo "[apple-size] to find it, add -dumpdep to the bind's -ldflags and read the <ReflectMethod> lines (build/APPLE_EXTENSION_MEMORY_BUDGET.md)" >&2
            exit 1
            ;;
        1)
            echo "[apple-size] $label linker method pruning=on"
            ;;
        *)
            echo "[apple-size] could not read $label: $path" >&2
            exit 1
            ;;
    esac
}

check_size() {
    label="$1"
    path="$2"
    max_bytes="$3"

    [ -f "$path" ] || {
        echo "[apple-size] missing $label: $path" >&2
        exit 1
    }
    bytes="$(wc -c <"$path" | tr -d '[:space:]')"
    case "$bytes" in
        ''|*[!0-9]*)
            echo "[apple-size] could not measure $label: $path" >&2
            exit 1
            ;;
    esac
    mib="$(awk -v bytes="$bytes" 'BEGIN { printf "%.3f", bytes / 1048576 }')"
    max_mib="$(awk -v bytes="$max_bytes" 'BEGIN { printf "%.3f", bytes / 1048576 }')"
    echo "[apple-size] $label=$bytes bytes (${mib} MiB), ceiling=$max_bytes bytes (${max_mib} MiB)"
    check_method_pruning "$label" "$path"
    if [ "$bytes" -gt "$max_bytes" ]; then
        echo "[apple-size] $label exceeds its compiled-size budget" >&2
        exit 1
    fi
}

if [ -n "$sdk_path" ]; then
    check_size "ios-arm64-sdk" "$sdk_path" "$sdk_max_bytes"
fi

if [ -n "$extension_sdk_path" ]; then
    check_size "ios-arm64-extension-sdk" "$extension_sdk_path" "$extension_sdk_max_bytes"
fi

if [ -n "$extension_path" ]; then
    check_size "ios-release-extension" "$extension_path" "$extension_max_bytes"

    # GOFIPS140=latest or a baked-in fips140=on default would make the Go
    # entropy source physically back a 32 MiB BSS scratch buffer. Runtime also
    # rejects an environment override; this catches build-time mistakes before
    # the extension is shipped.
    command -v go >/dev/null 2>&1 || {
        echo "[apple-size] Go is required to verify the extension's FIPS build metadata" >&2
        exit 1
    }
    build_info="$(go version -m "$extension_path" 2>/dev/null)" || {
        echo "[apple-size] could not read Go build metadata from the extension" >&2
        exit 1
    }
    if ! printf '%s\n' "$build_info" | grep -Eq 'build[[:space:]]+GOOS=ios'; then
        echo "[apple-size] extension has no verifiable iOS Go build metadata" >&2
        exit 1
    fi
    if printf '%s\n' "$build_info" | grep -Eq 'GOFIPS140=(latest|inprogress|v[0-9])|DefaultGODEBUG=.*fips140=(on|only)'; then
        echo "[apple-size] iOS extension was built with FIPS 140 enabled" >&2
        exit 1
    fi
    echo "[apple-size] fips140 build default=off"
fi
