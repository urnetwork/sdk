#!/usr/bin/env zsh
set -euo pipefail

cd "${0:A:h}"

# Only the runner-owned test binary changes its Go temp directory. Go's driver
# keeps the inherited compiler scratch/cache; native consumers inherit TMPDIR.
sdk_test_runtime_args=()
if [[ -n "${URNETWORK_SDK_TEST_RUNTIME_DIR:-}" ]]; then
    sdk_test_runtime="$URNETWORK_SDK_TEST_RUNTIME_DIR"
    if [[ "$sdk_test_runtime" != /* || ! -d "$sdk_test_runtime" ||
          "$sdk_test_runtime" == *\'* || "$sdk_test_runtime" == *$'\n'* ||
          "$sdk_test_runtime" == *$'\r'* ]]; then
        echo "SDK test runtime must be an absolute existing launcher-safe directory" >&2
        exit 64
    fi
    # Go splits -exec itself: quote each complete assignment, not just its value.
    sdk_test_runtime_args=("-exec=/usr/bin/env 'GOTMPDIR=$sdk_test_runtime' 'TMPDIR=$sdk_test_runtime'")
fi
echo "JavaScript SDK smoke: building the package, testing JS APIs, and exercising native companion RPC"
make smoke
npm test
UR_SUBPROTOCOL_WASM_TEST=1 go -C .. test "${sdk_test_runtime_args[@]}" . -run '^TestSubprotocolWasmCompanionRoundTrip$' -count=1
