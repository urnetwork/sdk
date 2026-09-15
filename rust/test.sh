#!/usr/bin/env zsh
set -euo pipefail

cd "${0:A:h}"
echo "Rust SDK smoke: compiling and running a clean Cargo consumer"
make smoke
