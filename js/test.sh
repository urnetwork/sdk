#!/usr/bin/env zsh
set -euo pipefail

cd "${0:A:h}"
echo "JavaScript SDK smoke: building the package and loading WASM in Node"
make smoke
