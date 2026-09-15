#!/usr/bin/env zsh

sdk_dir=${0:A:h}
workspace_root=${URNETWORK_ROOT:-${WARP_HOME:-${sdk_dir:h}}}
network_test_gate="$workspace_root/tests/network-intensive-suite-lock.sh"
if [[ ! -x "$network_test_gate" ]]; then
    echo "SDK test suite gate is missing or not executable: $network_test_gate" >&2
    exit 127
fi
if [[ "${URNETWORK_NETWORK_TEST_LOCK_HELD:-}" != 1 ]]; then
    exec "$network_test_gate" run-all run-all-sdk -- "$sdk_dir/test.sh" "$@"
fi
if ! "$network_test_gate" --verify-held run-all; then
    echo "SDK test suite inherited an invalid network-intensive lock" >&2
    exit 70
fi

# root sdk module
# Run the public-surface smoke on its own so a load/constructor regression is
# reported before the longer race-enabled suite. The full command below runs it
# again as part of the complete package test.
go test -timeout 30s -v -race -run '^TestSDKSmoke$'
if [[ $? != 0 ]]; then
    exit 1
fi
go test -timeout 0 -v -race "$@"
if [[ $? != 0 ]]; then
    exit 1
fi

# Match the test command's package-loading flags. Go applies GOFLAGS itself,
# including test-only flags that go list correctly ignores. Consume test flag
# values so a pattern such as `-run -tags` is not mistaken for a build flag.
list_args=(-race)
for ((arg_index = 1; arg_index <= $#; arg_index++)); do
    argument=$argv[arg_index]
    option=${argument%%=*}
    option=${option/#--/-}
    option=${option/#-test./-}
    case "$option" in
        -race|-msan|-asan)
            list_args+=("$argument")
            ;;
        -tags|-mod|-modfile|-overlay|-compiler|-buildmode|-installsuffix)
            list_args+=("$argument")
            if [[ "$argument" != *=* ]]; then
                ((arg_index++))
                list_args+=("$argv[arg_index]")
            fi
            ;;
        -args) break ;;
        -run|-skip|-bench|-benchtime|-count|-cpu|-parallel|-timeout|-shuffle|\
        -fuzz|-fuzztime|-fuzzminimizetime|\
        -blockprofile|-blockprofilerate|-coverprofile|-covermode|-coverpkg|\
        -cpuprofile|-memprofile|-memprofilerate|-mutexprofile|-mutexprofilefraction|\
        -outputdir|-trace|-vet|-o|-p|-asmflags|-gcflags|-gccgoflags|-ldflags|-pgo|-toolexec)
            if [[ "$argument" != *=* ]]; then
                ((arg_index++))
            fi
            ;;
    esac
done

# A filename alone does not make a test runnable for this target. Go's package
# metadata honors platform suffixes, build/race tags, and nested module
# boundaries. Keep discovery errors fatal and run every package in an admitted
# module, including build's host contracts and cgo/gen's ABI baseline.
for mod in "$sdk_dir"/*(N/); do
    [[ -f "$mod/go.mod" ]] || continue
    (
        cd "$mod" || exit $?
        host_tests=$(go list "${list_args[@]}" \
            -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./...) || exit $?
        if [[ -z "$host_tests" ]]; then
            printf 'SDK Go module %s: no tests for the active Go target\n' "${mod:t}"
            exit 0
        fi
        go test -timeout 0 -v -race "$@" ./...
    ) || exit $?
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
