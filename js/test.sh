#!/usr/bin/env zsh
set -euo pipefail

cd "${0:A:h}"
echo "JavaScript SDK smoke: building the package, testing JS APIs, and exercising native companion RPC"
make smoke
npm test
UR_SUBPROTOCOL_WASM_TEST=1 go -C .. test . -run '^TestSubprotocolWasmCompanionRoundTrip$' -count=1
