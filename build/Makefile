
all: init clean build_js build_android build_apple

clean:
	rm -rf android*
	rm -rf apple*
	rm -rf ios*
	rm -rf js*

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
	# note the device_rpc types (DeviceLocalRpc, DeviceRemote*) should not be included in gomobile
	# but due to limitations they are and should be ignored
	# note the .comment section is stripped from the build library for cross-platform build reproducibility, see
	# https://f-droid.org/en/docs/Reproducible_Builds/#ndk-clang-version-string-in-comment-section
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	export GODEBUG=gotypesalias=0; \
	export GOEXPERIMENT=greenteagc; \
	OBJCOPY=`find "$$ANDROID_NDK_HOME" -iname 'llvm-objcopy'`; \
	BUILD_DIR=android.`date +%s`; \
	mkdir -p "$$BUILD_DIR"; \
	gomobile bind \
		-target android/arm64,android/arm,android/amd64 -androidapi 24 \
		-javapkg com.bringyour \
		-trimpath \
		-gcflags "-dwarf=false" \
		-ldflags "-s -w -X sdk.Version=$${WARP_VERSION} -compressdwarf=false -buildid=" \
		-o "$$BUILD_DIR/URnetworkSdk.aar" \
		"github.com/urnetwork/sdk"; \
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
			bad_exports=`grep -Ri '// skipped' . | grep -v DeviceLocalRpc | grep -ve 'DeviceRemote[A-Z]' | grep -ve 'NewPlatformDeviceLocal' | grep -ve 'Proxy'`; \
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
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	export GODEBUG=gotypesalias=0; \
	export GOEXPERIMENT=greenteagc; \
	BUILD_DIR=apple.`date +%s`; \
	mkdir -p "$$BUILD_DIR"; \
	gomobile bind \
		-target ios/arm64,iossimulator/arm64,macos/arm64,macos/amd64 -iosversion 16.0 \
		-bundleid network.ur \
		-trimpath \
		-gcflags "-dwarf=false" \
		-ldflags "-s -w -X sdk.Version=$${WARP_VERSION} -compressdwarf=false -buildid=" \
		-o "$$BUILD_DIR/URnetworkSdk.xcframework" \
		"github.com/urnetwork/sdk"; \
	(cd "$$BUILD_DIR" && zip -r URnetworkSdk.xcframework.zip URnetworkSdk.xcframework); \
	if [ -e "ios" ]; then mv ios ios.old.`date +%s`; fi; \
	cp -r "$$BUILD_DIR" ios; \
	if [ -e "apple" ]; then mv apple apple.old.`date +%s`; fi; \
	mv "$$BUILD_DIR" apple;

build_js:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	export GODEBUG=gotypesalias=0; \
	export GOEXPERIMENT=greenteagc; \
	BUILD_DIR=js.`date +%s`; \
	mkdir -p "$$BUILD_DIR"; \
	mkdir -p "$$BUILD_DIR/URnetworkSdkJs"; \
	GOOS=js GOARCH=wasm go build -o "$$BUILD_DIR/URnetworkSdkJs/sdk.wasm"; \
	cp /usr/local/go/lib/wasm/wasm_exec.js "$$BUILD_DIR/URnetworkSdkJs"; \
	cp index.js "$$BUILD_DIR/URnetworkSdkJs"; \
	cp index.html "$$BUILD_DIR/URnetworkSdkJs"; \
	(cd "$$BUILD_DIR" && zip -r URnetworkSdkJs.zip URnetworkSdkJs); \
	if [ -e "js" ]; then mv js js.old.`date +%s`; fi; \
	mv "$$BUILD_DIR" js;

init:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin" && \
	GOMOBILE_VERSION=v0.0.0-20251209145715-2553ed8ce294 && \
	go install golang.org/x/mobile/cmd/gomobile@$$GOMOBILE_VERSION && \
	go get golang.org/x/mobile/bind@$$GOMOBILE_VERSION && \
	gomobile init && \
	CHECKSEC_VERSION=v0.0.0-20250809193834-71012bf5cee9 && \
	go install github.com/slimm609/checksec@$$CHECKSEC_VERSION && \
	go mod download "github.com/urnetwork/sdk" && \
	go mod download "github.com/urnetwork/connect" && \
	go clean -cache && \
	go clean -modcache;
