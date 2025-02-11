
all: clean build_android build_apple

clean:
	rm -rf build

# to reduce binary file size:
# -s omit symbol table and debug info
# -w omit DWARF symbol table
# see https://go.dev/doc/gdb
# see https://github.com/xaionaro/documentation/blob/master/golang/reduce-binary-size.md

build_android:
	# *important* gradle does not handle symbolic links consistently
	# the build dir swap is non-atomic
	# note android/amd64 is needed for chromebook devices
	BUILD_DIR=build/android.`date +%s`; \
	WARP_VERSION=`warpctl ls version`; \
	mkdir -p "$$BUILD_DIR"; \
	gomobile bind \
		-target android/arm64,android/arm,android/amd64 -androidapi 24 \
		-javapkg com.bringyour \
		-trimpath \
		-gcflags "-dwarf=false" \
		-ldflags "-w -X client.Version=$$WARP_VERSION -compressdwarf=false -B gobuildid" \
		-o "$$BUILD_DIR/URnetworkSdk.aar" \
		github.com/urnetwork/sdk; \
	if [[ -e "build/android" ]]; then mv build/android build/android.old.`date +%s`; fi; \
	mv "$$BUILD_DIR" build/android;

	# validate that all types could be exported
	# note the device_rpc types (DeviceLocalRpc, DeviceRemote*) should not be included in gomobile
	# but due to limitations they are and should be ignored
	cd build/android; \
		if [[ -e validate ]]; then mv validate validate.$$(date +%s); fi; \
		mkdir validate; \
		cp URnetworkSdk-sources.jar validate/; \
		cd validate; \
			jar -xf URnetworkSdk-sources.jar; \
			bad_exports=`grep -Ri '// skipped' . | grep -v DeviceLocalRpc | grep -ve 'DeviceRemote[A-Z]'`; \
			if [[ "$$bad_exports" ]]; then \
				echo "Some types could not be exported:"; \
				echo "$$bad_exports"; \
				exit 1; \
			fi;

build_ios:
	$(MAKE) build_apple

build_apple:
	# *important* Xcode does not handle symbolic links consistently
	# the build dir swap is non-atomic
	BUILD_DIR=build/apple.`date +%s`; \
	WARP_VERSION=`warpctl ls version`; \
	mkdir -p "$$BUILD_DIR"; \
	gomobile bind \
		-ldflags "-X client.Version=$$WARP_VERSION" \
		-target ios/arm64,iossimulator/arm64,macos/arm64,macos/amd64 -iosversion 16.0 \
		-bundleid network.ur \
		-trimpath \
		-gcflags "-dwarf=false" \
		-ldflags "-s -w -X client.Version=$$WARP_VERSION -compressdwarf=false -B gobuildid" \
		-o "$$BUILD_DIR/URnetworkSdk.xcframework" \
		github.com/urnetwork/sdk; \
	if [[ -e "build/ios" ]]; then mv build/ios build/ios.old.`date +%s`; fi; \
	cp -r "$$BUILD_DIR" build/ios; \
	if [[ -e "build/apple" ]]; then mv build/apple build/apple.old.`date +%s`; fi; \
	mv "$$BUILD_DIR" build/apple;

# this must be run on Windows
# use mingw64 for gcc (choco install mingw) 
build_windows:
	cd windows && set "GOOS=windows" && set "GOARCH=amd64" && set "CGO_ENABLED=1" && go build -buildmode=c-shared -o "../build/windows/amd64/URnetworkSdk.dll"
	rem FIXME mingw64 does not appear to support arm64 currently
	rem cd windows && set "GOOS=windows" && set "GOARCH=arm64" && set "CGO_ENABLED=1" && go build -buildmode=c-shared -o "../build/windows/arm64/URnetworkSdk.dll"

init:
	go install golang.org/x/mobile/cmd/gomobile@latest
	go get golang.org/x/mobile/bind@latest
	gomobile init
