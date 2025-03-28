
all: clean build_android build_apple

clean:
	rm -rf build

# to reduce binary file size:
# -s omit symbol table and debug info
# -w omit DWARF symbol table
# see https://go.dev/doc/gdb
# see https://github.com/xaionaro/documentation/blob/master/golang/reduce-binary-size.md

# on macos, `brew install binutils` and `brew link binutils --force`
build_android:
	# *important* gradle does not handle symbolic links consistently
	# the build dir swap is non-atomic
	# note android/amd64 is needed for chromebook devices
	# FIXME remove this GODEBUG setting per https://github.com/golang/go/issues/71827; see https://pkg.go.dev/go/types#Alias
	# FIXME edit the built aar to remove platform-specific comments, for reproducibility across platforms
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	export GODEBUG=gotypesalias=0; \
	BUILD_DIR=build/android.`date +%s`; \
	mkdir -p "$$BUILD_DIR"; \
	gomobile bind \
		-target android/arm64,android/arm,android/amd64 -androidapi 24 \
		-javapkg com.bringyour \
		-trimpath \
		-gcflags "-dwarf=false" \
		-ldflags "-w -X client.Version=$$WARP_VERSION -compressdwarf=false" \
		-o "$$BUILD_DIR/URnetworkSdk.aar" \
		github.com/urnetwork/sdk; \
	if [ -e "build/android" ]; then mv build/android build/android.old.`date +%s`; fi; \
	unzip "$$BUILD_DIR/URnetworkSdk.aar" -d "$$BUILD_DIR/edit"; \
	find "$$BUILD_DIR/edit" -iname '*.so' -exec objcopy --remove-section .comment {} \; ; \
	jar cvf "$$BUILD_DIR/URnetworkSdk.aar" -C "$$BUILD_DIR/edit" .; \
	mv "$$BUILD_DIR" build/android;

	# validate that all types could be exported
	# note the device_rpc types (DeviceLocalRpc, DeviceRemote*) should not be included in gomobile
	# but due to limitations they are and should be ignored
	(cd build/android; \
		if [ -e validate ]; then mv validate validate.$$(date +%s); fi; \
		mkdir validate; \
		cp URnetworkSdk-sources.jar validate/; \
		cd validate; \
			jar -xf URnetworkSdk-sources.jar; \
			bad_exports=`grep -Ri '// skipped' . | grep -v DeviceLocalRpc | grep -ve 'DeviceRemote[A-Z]'`; \
			if [ "$$bad_exports" ]; then \
				echo "Some types could not be exported:"; \
				echo "$$bad_exports"; \
				exit 1; \
			fi;)

build_ios:
	$(MAKE) build_apple

build_apple:
	# *important* Xcode does not handle symbolic links consistently
	# the build dir swap is non-atomic
	# FIXME remove this GODEBUG setting per https://github.com/golang/go/issues/71827; see https://pkg.go.dev/go/types#Alias
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	export GODEBUG=gotypesalias=0; \
	BUILD_DIR=build/apple.`date +%s`; \
	mkdir -p "$$BUILD_DIR"; \
	gomobile bind \
		-target ios/arm64,iossimulator/arm64,macos/arm64,macos/amd64 -iosversion 16.0 \
		-bundleid network.ur \
		-trimpath \
		-gcflags "-dwarf=false" \
		-ldflags "-s -w -X client.Version=$$WARP_VERSION -compressdwarf=false" \
		-o "$$BUILD_DIR/URnetworkSdk.xcframework" \
		github.com/urnetwork/sdk; \
	if [ -e "build/ios" ]; then mv build/ios build/ios.old.`date +%s`; fi; \
	cp -r "$$BUILD_DIR" build/ios; \
	if [ -e "build/apple" ]; then mv build/apple build/apple.old.`date +%s`; fi; \
	mv "$$BUILD_DIR" build/apple;

# this must be run on Windows
# use mingw64 for gcc (choco install mingw) 
build_windows:
	cd windows && set "GOOS=windows" && set "GOARCH=amd64" && set "CGO_ENABLED=1" && go build -buildmode=c-shared -o "../build/windows/amd64/URnetworkSdk.dll"
	rem FIXME mingw64 does not appear to support arm64 currently
	rem cd windows && set "GOOS=windows" && set "GOARCH=arm64" && set "CGO_ENABLED=1" && go build -buildmode=c-shared -o "../build/windows/arm64/URnetworkSdk.dll"

init:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	GOMOBILE_VERSION=v0.0.0-20250305212854-3a7bc9f8a4de; \
	go install golang.org/x/mobile/cmd/gomobile@$$GOMOBILE_VERSION; \
	go get golang.org/x/mobile/bind@$$GOMOBILE_VERSION; \
	gomobile init
