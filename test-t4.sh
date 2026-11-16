#!/usr/bin/env bash
# T4 (sidecar DESIGN.md §22.4; SDK-M8-15): the sdk's binding gates, the surrogate cells with §18.4's budgets as
# assertions, the app interfaces through every adapter this host runs (the C ABI, js in Node, the generated Swift and
# Kotlin names), FS-124's presence vectors byte-equal on each leg here, the DeviceRemote mirror and receiver hosting
# through the bound API, and the page Worker's search in wasm under Node. What this host cannot run says "not run",
# which a gate lists as open and never as a pass: the Swift harness needs URnetworkSdk.xcframework and a signed test
# host, the Kotlin harness an AVD or surrogate, the iOS surrogate's cells a device, and an Android surrogate's cells the
# serial of a device lent to this run (T4_ANDROID_SERIAL; a device merely attached may be another campaign's, so none is
# picked).
#
# GO names the go command, which must not add -trimpath: cgo/gen's and packaging's tests find their files through the
# source paths it would strip.
#
#   GO=go ./test-t4.sh
set -euo pipefail
cd "$(dirname "$0")"
go_command="${GO:-go}"

echo "== the sdk compiles for every target the binds name"
"$go_command" test -count=1 -run '^TestSdkCompilesForEveryBoundTarget$' .

echo "== the surrogate cells, on this host"
(cd build && "$go_command" run ./cmd/t4cell -target host)

echo "== the surrogate cells, on an Android surrogate"
(cd build && "$go_command" test -count=1 ./cmd/t4cell)
if [ -n "${T4_ANDROID_SERIAL:-}" ]; then
	(cd build && "$go_command" run ./cmd/t4cell -target android -serial "$T4_ANDROID_SERIAL")
else
	echo "not run: T4_ANDROID_SERIAL names no surrogate (the cells built for android/arm64 above)"
fi

echo "== FS-124's presence vectors, on darwin/arm64 and, under Rosetta, darwin/amd64"
"$go_command" test -count=1 -run '^(TestPresenceVectorsCrossTheAppSignerByteEqual|TestPresenceAnswerThatIsNoEvidenceRefused|TestPresenceVectorsAreTheSidecarModules)$' .
if [ "$(uname -s)" = Darwin ] && arch -x86_64 /usr/bin/true 2>/dev/null; then
	GOARCH=amd64 "$go_command" test -count=1 -run '^(TestPresenceVectorsCrossTheAppSignerByteEqual|TestPresenceAnswerThatIsNoEvidenceRefused)$' .
else
	echo "not run: no second architecture on this host"
fi

echo "== the DeviceRemote mirror and receiver hosting through the bound API"
"$go_command" test -count=1 -run '^(TestRpcMirrorBridgeStatusComplete|TestBridgeStatusMirrorFollowsAResync|TestBridgeStatusListenerLeavesWithItsConnection|TestSetupRunsAfterEnrollment|TestForeignRevocationWaitsForConfirm|TestHandOffQueuedWhileDisconnected|TestDesktopPathThroughControl|TestForwardingChannelCarriesNoPrivateKey|TestUnsignedResponseOverRpcIgnored|TestBridgeStatusCarriesTheForwardingFields|TestMailVaultReadsMapAndRefuse|TestMailCallsAwaitingTheCoreAnswerUnavailable|TestMailCoverageValues)$' .

echo "== sdk.Sidecar's search, host classes and the iOS extension's leg"
"$go_command" test -count=1 -run '^(TestPagesReplace|TestMoreNextPage|TestCancelStopsLocallyOnly|TestNilWithoutEvs|TestRemoteModeStreams|TestQueryCanaryInNoLog|TestUnreportedConditionCountsMetered|TestHostClassPolicies|TestBudgetMapsToDeadline|TestOverrideSurvivesDropCache|TestCapFromFreeSpace|TestForegroundForwarded|TestIosExtensionLinksNoIndexCode)$' .

echo "== the binding gates and the adapter harnesses' bindings"
(cd cgo && "$go_command" test -count=1 -run '^(TestSidecar|TestNoSidecarSymbolIsPending|TestGeneratedArtifactsMatchCurrentSDKSurface)' ./gen)
(cd build && "$go_command" test -count=1 -run '^(TestMobileSidecar|TestSidecarAdapterHarnessesStayBound)' . ./cmd/mobileexports)

echo "== the app interfaces through the C ABI, and as a C++ host against the built library"
(cd cgo && "$go_command" test -count=1 -run '^(TestSidecarKeyStoreAnswersCrossTheAbi|TestNewSidecarLocalThroughTheAbiKeepsTheStoreRules|TestSidecarPresenceSignerAnswersCrossTheAbi|TestAMissingHostFunctionAnswersNothing)$' .)
if command -v clang++ >/dev/null 2>&1 && [ "$(uname -s)" = Darwin ]; then
	(cd cgo && make smoke_sidecar smoke_sidecar_keystore)
else
	echo "not run: the C++ smokes build the host library with clang++ on macOS"
fi

echo "== the js adapter and the page Worker's search, in wasm under Node"
(cd js && GO="$go_command" node --test test/sidecar_surface.test.ts test/sidecar_adapter.test.ts)
(cd js && "$go_command" test -count=1 ./sidecarworker/)
(cd js && GO="$go_command" node --test test/sidecarwasm_search.test.ts)

echo "== the Swift and Kotlin harnesses"
echo "not run: adapters/swift needs URnetworkSdk.xcframework and a signed test host; adapters/kotlin an AVD or surrogate"
