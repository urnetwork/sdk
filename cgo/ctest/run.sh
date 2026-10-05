#!/usr/bin/env bash
# Build the messaging c abi twice and run a C program against it.
#
#   ./ctest/run.sh            from sdk/cgo, with go and a c compiler on PATH
#
# TWO LIBRARIES, AND THE DIFFERENCE BETWEEN THEM IS THE POINT:
#
#   build/host/URnetworkSdk.dll   what SHIPS. no build tag, no alternate modfile, no harness.
#   build/ctest/URnetworkSdk.dll  the same code plus loopback_test_world.go, which is behind
#                                 `//go:build urnet_message_loopback` and pulls in the real
#                                 message server through loopback.go.mod. NOTHING SHIPS IT.
#
# The script FAILS if the shipping header declares a single urnet_message_loopback_* symbol,
# because a harness that leaked into the product is worse than no harness. It also prints the
# export counts of both, which is the number to quote.
set -u

cd "$(dirname "$0")/.."
here=$(pwd)
fail=0

note() { printf '\n== %s\n' "$*"; }

note "the shipping library: no tag, cgo/go.mod untouched"
rm -rf build/host && mkdir -p build/host
CGO_ENABLED=1 go build -buildmode=c-shared -o build/host/URnetworkSdk.dll . || exit 1

note "the test library: -tags urnet_message_loopback, -modfile=loopback.go.mod"
rm -rf build/ctest && mkdir -p build/ctest
CGO_ENABLED=1 go build -modfile=loopback.go.mod \
  -tags urnet_message_loopback -buildmode=c-shared \
  -o build/ctest/URnetworkSdk.dll . || exit 1

note "what each library exports"
ship_all=$(grep -cE '^extern .*urnet_' build/host/URnetworkSdk.h)
ship_msg=$(grep -cE '^extern .*urnet_message_' build/host/URnetworkSdk.h)
ship_loop=$(grep -cE 'urnet_message_loopback' build/host/URnetworkSdk.h)
test_loop=$(grep -cE '^extern .*urnet_message_loopback_' build/ctest/URnetworkSdk.h)
printf '  shipping : %s exports, %s of them messaging, %s loopback\n' "$ship_all" "$ship_msg" "$ship_loop"
printf '  test     : %s loopback exports\n' "$test_loop"
printf '  shipping dll : %s bytes\n' "$(stat -c %s build/host/URnetworkSdk.dll 2>/dev/null || wc -c < build/host/URnetworkSdk.dll)"
printf '  shipping hdr : %s bytes\n' "$(stat -c %s build/host/URnetworkSdk.h 2>/dev/null || wc -c < build/host/URnetworkSdk.h)"

if [ "$ship_loop" != "0" ]; then
  printf '  FAIL: the SHIPPING header declares %s loopback symbols. the harness has leaked.\n' "$ship_loop"
  fail=1
fi
if [ "$test_loop" = "0" ]; then
  printf '  FAIL: the test library has no loopback symbols, so the tag did not apply.\n'
  fail=1
fi

note "compiling the C consumer against include/urnetwork_message.h"
"${CC:-gcc}" -std=c11 -Wall -Wextra -Werror -O1 \
  -I"$here/include" \
  -o build/ctest/message_abi_test.exe ctest/message_abi_test.c \
  build/ctest/URnetworkSdk.dll || exit 1
printf '  %s bytes\n' "$(stat -c %s build/ctest/message_abi_test.exe 2>/dev/null || wc -c < build/ctest/message_abi_test.exe)"

note "running it"
run_log=build/ctest/run.log
( cd build/ctest && ./message_abi_test.exe ) 2>&1 | tee "$run_log"
# the whole line, anchored. `grep '0 FAILED'` matches "10 FAILED" too, which would have read a
# ten-failure run as a pass. The exit status is not available here because tee is the last stage
# of the pipe, so this line IS the verdict and it has to be exact.
if ! grep -qE '^=== [0-9]+ STEPS, [0-9]+ ASSERTIONS, 0 FAILED ===$' "$run_log"; then
  printf '  FAIL: the consumer did not report a clean run\n'
  fail=1
fi

# NO EXPORT MAY REACH C BY WAY OF A RECOVERED PANIC. cgoGuard catches a panic so it cannot unwind
# into C and abort the host, and that is right -- but it also means an out-of-range index and an
# explicit refusal look identical from C: both answer false. So the bounds checks in the list
# accessors are invisible to any assertion the consumer can make, and deleting one of them leaves
# this whole suite green. This is the gate that sees it: a panic, recovered or not, is a defect.
if grep -q 'panicked' "$run_log"; then
  printf '  FAIL: an export panicked and cgoGuard recovered it; grep panicked %s\n' "$run_log"
  grep 'panicked' "$run_log" | head -3
  fail=1
else
  printf '  no export panicked\n'
fi

# THE SAME PROGRAM UNDER -race, BECAUSE THIS TEST IS CONCURRENCY-BEARING. The cancellation case
# calls urnet_message_context_cancel from a second OS thread the C side created, while a Go
# goroutine is blocked inside Device.Connect; the connect-attempt callback runs on the Go side of
# that same call and writes into a C struct the main thread reads afterwards.
#
# ON WINDOWS THIS DOES NOT RUN, AND THE SCRIPT SAYS SO OUT LOUD RATHER THAN SKIPPING QUIETLY.
# The -race dll BUILDS; it is loading it that fails. ThreadSanitizer needs to map a large fixed
# shadow region and cannot once the host process has already laid out its address space, so the
# consumer dies before main with "ThreadSanitizer failed to allocate ... (error code: 87)". Where
# that happens the concurrency property is held instead by `go test -race` over the same handle
# registry and the same context handles -- see exports_message_test.go, which names this.
note "the same program again, against a -race build"
rm -rf build/ctest_race && mkdir -p build/ctest_race
CGO_ENABLED=1 go build -race -modfile=loopback.go.mod \
  -tags urnet_message_loopback -buildmode=c-shared \
  -o build/ctest_race/URnetworkSdk.dll . || exit 1
"${CC:-gcc}" -std=c11 -Wall -Wextra -Werror -O1 \
  -I"$here/include" \
  -o build/ctest_race/message_abi_test.exe ctest/message_abi_test.c \
  build/ctest_race/URnetworkSdk.dll || exit 1
race_log=build/ctest_race/race.log
( cd build/ctest_race && ./message_abi_test.exe ) > "$race_log" 2>&1
race_status=$?
if grep -q 'ThreadSanitizer failed to allocate' "$race_log"; then
  printf '  >>> RACE PASS DID NOT RUN ON THIS HOST <<<\n'
  printf '      ThreadSanitizer could not map its shadow memory into a loaded c-shared dll.\n'
  printf '      `go test -race ./... ` in this module is what holds the property here.\n'
  sed -n '1p' "$race_log"
elif grep -q 'WARNING: DATA RACE' "$race_log"; then
  printf '  FAIL: the race detector reported a data race (see %s)\n' "$race_log"
  fail=1
elif [ "$race_status" != "0" ]; then
  printf '  FAIL: the -race build of the consumer exited %s (see %s)\n' "$race_status" "$race_log"
  fail=1
else
  printf '  ran clean, no data race reported\n'
fi

# and the Go-level race pass, which DOES run everywhere
note "go test -race over the handle registry and the context handles"
go test -count=1 -race -timeout 300s . ./gen || fail=1

if [ "$fail" != "0" ]; then
  printf '\nFAILED\n'
  exit 1
fi
printf '\nOK\n'
