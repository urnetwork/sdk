#!/usr/bin/env bash
# T4 (sidecar DESIGN.md §22.4; SDK-M8-15, and from M10 SDK-M10-03 and SDK-M10-06): the sdk's binding gates, the
# surrogate cells with §18.4's budgets as assertions, the app interfaces through every adapter this host runs (the C
# ABI, js in Node, the generated Swift and Kotlin names), FS-124's presence vectors byte-equal on each leg here, the
# DeviceRemote mirror and receiver hosting through the bound API, and from M10 the phone cells with search on (a phone
# that searches, downloads and asks within §18.4's working set), T4's requester surrogate, the page Worker's search in
# wasm under Node within its 8 MiB, and testdata/index's vectors on T4's test legs (the iOS settings, gomobile
# android/arm64 and js/wasm in Node; D-TEST-15). App-host search cases run at the later of M10's and M8's exits, so
# this script reports which ran: every one below runs on this host, the surrogate devices' runs are external, and so
# are search targets on real surrogates and in Safari's wasm engine. What this host cannot run says "not run", which
# a gate lists as open and never as a pass: the Swift harness needs URnetworkSdk.xcframework and a signed test
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

echo "== the search index's vectors under the sdk's module graph, on darwin/arm64 and, under Rosetta, darwin/amd64"
"$go_command" test -count=1 -run '^(TestSdkLinksNormalizerOne|TestSdkLinksNormalizerOneFailsOnAnotherTextModule)$' .
"$go_command" test -count=1 -tags sidecar_testhooks -run '^TestSdkReproducesTheIndexVectors$' .
if [ "$(uname -s)" = Darwin ] && arch -x86_64 /usr/bin/true 2>/dev/null; then
	GOARCH=amd64 "$go_command" test -count=1 -tags sidecar_testhooks -run '^(TestSdkLinksNormalizerOne|TestSdkReproducesTheIndexVectors)$' .
else
	echo "not run: no second architecture on this host"
fi

echo "== the search index's vectors on T4's test legs (D-TEST-15): the iOS settings here, gomobile android/arm64 by the cells"
# the iOS settings: darwin/arm64 under the ios32 profile's 32 MiB Go memory limit, as §22.9's memory row reads them;
# the sdk's tests do not build with the bind's sdk_mobile_bind tag on darwin, whose daemon-faking tests that tag drops
GOMEMLIMIT=32MiB "$go_command" test -count=1 -tags sidecar_testhooks -run '^(TestSdkLinksNormalizerOne|TestSdkReproducesTheIndexVectors)$' .
echo "gomobile android/arm64: the cells' binary above holds TestSdkReproducesTheIndexVectors; it runs where they run (T4_ANDROID_SERIAL)"
echo "js/wasm in Node: the page Worker's leg, below (test/sidecarwasm_budget.test.ts)"

echo "== the DeviceRemote mirror and receiver hosting through the bound API"
"$go_command" test -count=1 -run '^(TestRpcMirrorBridgeStatusComplete|TestBridgeStatusMirrorFollowsAResync|TestBridgeStatusListenerLeavesWithItsConnection|TestSetupRunsAfterEnrollment|TestForeignRevocationWaitsForConfirm|TestHandOffQueuedWhileDisconnected|TestDesktopPathThroughControl|TestForwardingChannelCarriesNoPrivateKey|TestUnsignedResponseOverRpcIgnored|TestBridgeStatusCarriesTheForwardingFields|TestMailVaultReadsMapAndRefuse|TestMailCallsAwaitingTheCoreAnswerUnavailable|TestMailCoverageValues)$' .

echo "== sdk.Sidecar's search, host classes and the iOS extension's leg"
"$go_command" test -count=1 -run '^(TestPagesReplace|TestMoreNextPage|TestCancelStopsLocallyOnly|TestNilWithoutEvs|TestRemoteModeStreams|TestQueryCanaryInNoLog|TestUnreportedConditionCountsMetered|TestHostClassPolicies|TestBudgetMapsToDeadline|TestOverrideSurvivesDropCache|TestCapFromFreeSpace|TestForegroundForwarded|TestIosExtensionLinksNoIndexCode)$' .

echo "== remote search's requester seams and T4's requester surrogate, on the ios32 and Android profiles (SDK-M10-03)"
"$go_command" test -count=1 -run '^(TestRemoteModeListsNoIndexers|TestSetRemoteRefusedOnRemoteHost|TestAcknowledgeFalseSetsOwn|TestResetNoticesClears|TestModeNeverThroughMcp|TestRemoteStatesMapOneForOne|TestRemoteModeSidecarAsksNoIndexerOfItsDaemon|TestNoControlMethodCarriesTheMode|TestDirectoryRepoIdScopesSearchMore|TestT4SurrogateBootsAgainstAFakeIndexer|TestT4SurrogateDirectorySearchMoreAsksAnIndexer|TestT4SurrogateFakesHoldTheirRules)$' .

echo "== the phone cells with search on: a phone that searches, downloads and asks within §18.4's working set (SDK-M10-06)"
"$go_command" test -count=1 -tags sidecar_testhooks -run '^(TestIos32CellSearchesDownloadsAndAsks|TestAndroidCellSearchesDownloadsAndAsks|TestT4SearchBudgetCatchesAPerSearchKeyCollection|TestIos32CellSearchesWithinItsWorkingSet|TestAndroidCellSearchesWithinItsWorkingSet)$' .

echo "== the binding gates and the adapter harnesses' bindings"
(cd cgo && "$go_command" test -count=1 -run '^(TestSidecar|TestNoSidecarSymbolIsPending|TestGeneratedArtifactsMatchCurrentSDKSurface)' ./gen)
(cd build && "$go_command" test -count=1 -run '^(TestMobileSidecar|TestSidecarAdapterHarnessesStayBound)' . ./cmd/mobileexports)

echo "== the app interfaces through the C ABI, and as a C++ host against the built library"
(cd cgo && "$go_command" test -count=1 -run '^(TestSidecarKeyStoreAnswersCrossTheAbi|TestNewSidecarLocalThroughTheAbiKeepsTheStoreRules|TestSidecarPresenceSignerAnswersCrossTheAbi|TestAMissingHostFunctionAnswersNothing|TestReceiverHostingAnswersOnceThroughTheAbi)$' .)
if command -v clang++ >/dev/null 2>&1 && [ "$(uname -s)" = Darwin ]; then
	(cd cgo && make smoke_sidecar smoke_sidecar_keystore)
else
	echo "not run: the C++ smokes build the host library with clang++ on macOS"
fi

echo "== the js adapter and the page Worker's search, in wasm under Node, within its 8 MiB, and the index vectors on its leg"
(cd js && GO="$go_command" node --test test/sidecar_surface.test.ts test/sidecar_adapter.test.ts)
(cd js && "$go_command" test -count=1 ./sidecarworker/)
(cd js && GO="$go_command" node --test test/sidecarwasm_search.test.ts)
(cd js && GO="$go_command" node --test test/sidecarwasm_budget.test.ts)
echo "not run: a page search over a real index in wasm (the search world does not build on js/wasm), and Safari's wasm engine"

echo "== the Swift and Kotlin harnesses"
echo "not run: adapters/swift needs URnetworkSdk.xcframework and a signed test host; adapters/kotlin an AVD or surrogate"
