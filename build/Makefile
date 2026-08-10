
# build_windows is not in `all`: the shipped Windows SDK (amd64+arm64) builds
# inside the Windows VM (build/all/build-windows.sh), which provisions Go +
# llvm-mingw and copies URnetworkSdkWindows.zip back to ../cgo/build. The
# host-side target below stays for local dev (needs the ../cgo init toolchains;
# arm64 additionally needs llvm-mingw, which has no brew formula).
all: init clean build_android build_apple build_js build_linux

clean:
	rm -rf android*
	rm -rf apple*
	rm -rf ios*

# to reduce binary file size:
# -s omit symbol table and debug info
# -w omit DWARF symbol table
# see https://go.dev/doc/gdb
# see https://github.com/xaionaro/documentation/blob/master/golang/reduce-binary-size.md

build_android:
	# *important* gradle does not handle symbolic links consistently
	# the build dir swap is non-atomic
	# note android/amd64 is needed for chromebook devices
	# FIXME remove this GODEBUG setting per https://github.com/golang/go/issues/71827; see https://pkg.go.dev/go/types#Alias
	# FIXME edit the built aar to remove platform-specific comments, for reproducibility across platforms
	# validate that all types could be exported
	# note the device_rpc types (DeviceLocalRpc, DeviceRemote*, DeviceRpcDialer/Listener, their
	# Websocket* transports, and the *Rpc gob payload types) should not be included in gomobile
	# but due to limitations they are and should be ignored
	# the same applies to the DeviceLocalSettings struct fields, the DeviceLocal security-policy
	# generator setters, and testing-only constructors whose unsupported field/param types gomobile skips
	# the Sim* simulation scaffolding (sim_device.go) is a go-embedder surface whose configs
	# deliberately carry connect types, and CollapseHostNames is the go-side ([]string) variant
	# of CollapseHostNamesList — both are expected skips, not part of the mobile surface
	# note the .comment section is stripped from the build library for cross-platform build reproducibility, see
	# https://f-droid.org/en/docs/Reproducible_Builds/#ndk-clang-version-string-in-comment-section
	# note the 1.26 linker on android needs to use an additional -checklinkname=0 parameter, which is fixed in a pending PR to the anet lib
	# see https://github.com/wlynxg/anet/pull/9
	# see https://github.com/pion/webrtc/issues/2640
	set -e; \
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	export GODEBUG=gotypesalias=0; \
	export GOEXPERIMENT=greenteagc; \
	OBJCOPY=`find "$$ANDROID_NDK_HOME" -type f -iname 'llvm-objcopy' -print -quit`; \
	test -n "$$OBJCOPY"; \
	BUILD_DIR=android.`date +%s`; \
	test ! -e "$$BUILD_DIR"; \
	mkdir "$$BUILD_DIR"; \
	gomobile bind \
		-target android/arm64,android/arm,android/amd64 -androidapi 24 \
		-javapkg com.bringyour \
		-trimpath \
		-gcflags "-dwarf=false" \
		-ldflags "-s -w -X github.com/urnetwork/sdk.Version=$${WARP_VERSION} -compressdwarf=false -buildid= -checklinkname=0" \
		-o "$$BUILD_DIR/URnetworkSdk.aar" \
		"github.com/urnetwork/sdk"; \
	test -s "$$BUILD_DIR/URnetworkSdk.aar"; \
	test -s "$$BUILD_DIR/URnetworkSdk-sources.jar"; \
	unzip "$$BUILD_DIR/URnetworkSdk.aar" -d "$$BUILD_DIR/edit"; \
	find "$$BUILD_DIR/edit" -iname '*.so' -exec "$$OBJCOPY" --remove-section .comment {} \; ; \
	find "$$BUILD_DIR/edit" -iname '*.so' -exec checksec file {} --output json \; ; \
	jar cvf "$$BUILD_DIR/URnetworkSdk.aar" -C "$$BUILD_DIR/edit" .; \
	(cd "$$BUILD_DIR"; \
		if [ -e validate ]; then mv validate validate.$$(date +%s); fi; \
		mkdir validate; \
		cp URnetworkSdk-sources.jar validate/; \
		cd validate; \
			jar -xf URnetworkSdk-sources.jar; \
			: 'gobind omits anything it cannot bind and only leaves a'; \
			: '"// skipped" comment, so this gate fails the build on an'; \
			: 'UNEXPECTED omission. Everything below is a deliberate accept;'; \
			: 'add an entry only with a reason, or the gate stops catching'; \
			: 'real binding breaks.'; \
			: '  rpc/proxy/platform/test surface: not part of the app api'; \
			: '  *SyncWithContext, NewApi, NewNetworkSpaceWithUrls: take a'; \
			: '    context.Context; apps use the NewPlatform*/non-ctx forms'; \
			: '  Sn* (uint64, [][]byte) + VerifyKeysResult.Keys ([]*struct):'; \
			: '    gomobile binds none of those. NOTE these fields are then'; \
			: '    INVISIBLE to android/apple — if an app ever needs one it'; \
			: '    needs an exportedList[T] wrapper or an int64/string form,'; \
			: '    not another entry here.'; \
			allow_skipped='DeviceLocalRpc'; \
			allow_skipped="$$allow_skipped|DeviceRemote[A-Z]|DeviceRpcDialer|DeviceRpcListener|[A-Za-z]Rpc"; \
			allow_skipped="$$allow_skipped|NewPlatformDeviceLocal|NewPlatformNetworkSpace|Proxy"; \
			allow_skipped="$$allow_skipped|DeviceLocal\.AddReceivePacketCallback|DeviceLocal\.Ctx"; \
			allow_skipped="$$allow_skipped|DeviceLocal\.Set[A-Za-z]*SecurityPolicyGenerator"; \
			allow_skipped="$$allow_skipped|SetUpgradeMuxSettings|DeviceLocalSettings"; \
			allow_skipped="$$allow_skipped|Testing_NewNetworkSpaceWithUrls|Sim[A-Z]|CollapseHostNames"; \
			allow_skipped="$$allow_skipped|SyncWithContext|NewApi|NewNetworkSpaceWithUrls"; \
			allow_skipped="$$allow_skipped|SnPoolClaim(Result|Args)|SnEpochResult|VerifyKeysResult\.Keys"; \
			bad_exports=`grep -Ri '// skipped' . | grep -Ev "$$allow_skipped" || true`; \
			if [ "$$bad_exports" ]; then \
				echo "Some types could not be exported:"; \
				echo "$$bad_exports"; \
				exit 1; \
			fi;); \
	rm -rf "$$BUILD_DIR/edit"; \
	rm -rf "$$BUILD_DIR/validate"; \
	if [ -e "android" ]; then mv android android.old.`date +%s`; fi; \
	mv "$$BUILD_DIR" android;

build_ios:
	$(MAKE) build_apple

build_apple:
	# *important* Xcode does not handle symbolic links consistently
	# the build dir swap is non-atomic
	# FIXME remove this GODEBUG setting per https://github.com/golang/go/issues/71827; see https://pkg.go.dev/go/types#Alias
	set -e; \
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	export GODEBUG=gotypesalias=0; \
	export GOEXPERIMENT=greenteagc; \
	export MACOSX_DEPLOYMENT_TARGET=13.5; \
	BUILD_DIR=apple.`date +%s`; \
	test ! -e "$$BUILD_DIR"; \
	mkdir "$$BUILD_DIR"; \
	gomobile bind \
		-target ios/arm64,iossimulator/arm64,macos/arm64,macos/amd64 -iosversion 16.0 \
		-bundleid network.ur \
		-trimpath \
		-gcflags "-dwarf=false" \
		-ldflags "-s -w -X github.com/urnetwork/sdk.Version=$${WARP_VERSION} -compressdwarf=false -buildid=" \
		-o "$$BUILD_DIR/URnetworkSdk.xcframework" \
		"github.com/urnetwork/sdk"; \
	test -d "$$BUILD_DIR/URnetworkSdk.xcframework"; \
	(cd "$$BUILD_DIR" && zip -r URnetworkSdk.xcframework.zip URnetworkSdk.xcframework); \
	test -s "$$BUILD_DIR/URnetworkSdk.xcframework.zip"; \
	if [ -e "ios" ]; then mv ios ios.old.`date +%s`; fi; \
	cp -r "$$BUILD_DIR" ios; \
	if [ -e "apple" ]; then mv apple apple.old.`date +%s`; fi; \
	mv "$$BUILD_DIR" apple;

# Build the JavaScript/WASM version of the SDK in the js dir
build_js:
	$(MAKE) -C ../js

# Build the c abi libraries for desktop apps in the cgo dir
# note these need the cross toolchains from `make -C ../cgo init` on the build host
build_windows:
	$(MAKE) -C ../cgo build_windows

build_linux:
	$(MAKE) -C ../cgo build_linux


GOMOBILE_VERSION ?= v0.0.0-20260611195102-4dd8f1dbf5d2
CHECKSEC_VERSION ?= v3.1.0

# Install only the pinned build tools. Platform setup scripts use this target
# so acceptance builds do not install tools or clean shared Go caches.
# gomobile init installs gobind@latest, so restore the matching version after it.
.PHONY: init_tools init
init_tools:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin" && \
	go install golang.org/x/mobile/cmd/gomobile@$(GOMOBILE_VERSION) && \
	gomobile init && \
	go install golang.org/x/mobile/cmd/gobind@$(GOMOBILE_VERSION) && \
	go install github.com/slimm609/checksec/v3@$(CHECKSEC_VERSION)

init: init_tools
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin" && \
	go get golang.org/x/mobile/bind@$(GOMOBILE_VERSION) && \
	go mod download "github.com/urnetwork/sdk" && \
	go mod download "github.com/urnetwork/connect" && \
	go clean -cache && \
	go clean -modcache;
