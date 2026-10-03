#!/usr/bin/env zsh
set -euo pipefail

cd "${0:A:h}"
echo "cgo SDK smoke: building and loading the host C ABI"
make smoke
