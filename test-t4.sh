#!/usr/bin/env bash
# T4 (sidecar DESIGN.md §22.4; SDK-M8-15): the sdk's binding gates, the surrogate cells with §18.4's budgets as
# assertions, the adapter harnesses' bindings, and the page Worker's search in wasm under Node. What this host cannot
# run says "not run", which a gate lists as open and never as a pass: the Swift harness needs URnetworkSdk.xcframework
# and a signed test host, the Kotlin harness an AVD or surrogate, the iOS surrogate's cells a device, and an Android
# surrogate's cells the serial of a device lent to this run (T4_ANDROID_SERIAL; a device merely attached may be another
# campaign's, so none is picked).
#
# GO names the go command, which must not add -trimpath: cgo/gen's and packaging's tests find their files through the
# source paths it would strip.
#
#   GO=go ./test-t4.sh
set -euo pipefail
cd "$(dirname "$0")"
go_command="${GO:-go}"

echo "== the surrogate cells, on this host"
(cd build && "$go_command" run ./cmd/t4cell -target host)

echo "== the surrogate cells, on an Android surrogate"
if [ -n "${T4_ANDROID_SERIAL:-}" ]; then
	(cd build && "$go_command" run ./cmd/t4cell -target android -serial "$T4_ANDROID_SERIAL")
else
	echo "not run: T4_ANDROID_SERIAL names no surrogate"
fi

echo "== sdk.Sidecar's search, host classes and the iOS extension's leg"
"$go_command" test -count=1 -run '^(TestPagesReplace|TestMoreNextPage|TestCancelStopsLocallyOnly|TestNilWithoutEvs|TestRemoteModeStreams|TestQueryCanaryInNoLog|TestUnreportedConditionCountsMetered|TestHostClassPolicies|TestBudgetMapsToDeadline|TestOverrideSurvivesDropCache|TestCapFromFreeSpace|TestForegroundForwarded|TestIosExtensionLinksNoIndexCode)$' .

echo "== the binding gates and the adapter harnesses' bindings"
(cd cgo && "$go_command" test -count=1 -run '^(TestSidecar|TestNoSidecarSymbolIsPending|TestGeneratedArtifactsMatchCurrentSDKSurface)' ./gen)
(cd build && "$go_command" test -count=1 -run '^(TestMobileSidecar|TestSidecarAdapterHarnessesStayBound)' . ./cmd/mobileexports)

echo "== the page Worker's search, natively and in wasm under Node"
(cd js && "$go_command" test -count=1 ./sidecarworker/)
(cd js && GO="$go_command" node --test test/sidecarwasm_search.test.ts)

echo "== the Swift and Kotlin harnesses"
echo "not run: adapters/swift needs URnetworkSdk.xcframework and a signed test host; adapters/kotlin an AVD or surrogate"
