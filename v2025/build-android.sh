#!/usr/bin/env bash

export BRINGYOUR_HOME=`realpath ..`
export WARP_HOME="$BRINGYOUR_HOME"
export WARP_VERSION_HOME="$BRINGYOUR_HOME/version"
export PATH="$PATH:$BRINGYOUR_HOME/warp/warpctl/build/darwin/arm64"
export PATH="$PATH:/usr/local/go/bin:$HOME/go/bin"
make init build_android
