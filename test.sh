#!/usr/bin/env zsh

# root sdk module
go test -timeout 0 -v -race "$@"
if [[ $? != 0 ]]; then
    exit 1
fi

# submodules with their own go.mod (cgo, js, build): run each module's go
# tests from inside the module, the way server/test.sh iterates its test
# dirs. Only modules that contain _test.go files are run — the js/build
# modules target wasm and do not build for the host. Notably cgo/gen holds
# the ABI baseline test, which must run from the cgo module.
for mod in `find . -mindepth 2 -maxdepth 2 -name go.mod | xargs -n 1 dirname | sort`; do
    if [[ -z `find $mod -name '*_test.go' -not -path '*/node_modules/*' -print -quit` ]]; then
        continue
    fi
    pushd $mod
    go test -timeout 0 -v -race "$@" ./...
    result=$?
    popd
    if [[ $result != 0 ]]; then
        exit $result
    fi
done

# js package tests (node --test via the package script): fetch_retry + the
# wasm surface guard
if [[ -f js/package.json ]]; then
    pushd js
    npm test
    result=$?
    popd
    if [[ $result != 0 ]]; then
        exit $result
    fi
fi

# ./test.sh -run 'pattern' (applies to the go modules; npm test ignores it)
