#!/usr/bin/env bash

# Fail loudly. Without this the script exits 0 even when make fails, which is how a
# broken build ("No rule to make target `init'") looked like a successful one and the
# stale aar/framework kept shipping.
set -euo pipefail

export BRINGYOUR_HOME=`realpath ..`
export WARP_HOME="$BRINGYOUR_HOME"
export WARP_VERSION_HOME="$BRINGYOUR_HOME/version"
export PATH="$PATH:$BRINGYOUR_HOME/warp/warpctl/build/darwin/arm64"
export PATH="$PATH:/usr/local/go/bin:$HOME/go/bin"

# The Makefile lives in build/, not here. `make` alone ran in the sdk root and found
# no Makefile at all.
make -C "$(dirname "$0")/build" init build_android
