
all: clean build_android build_ios

clean:
	rm -rf build

build_android:
	# *important* gradle does not handle symbolic links consistently
	# the build dir swap is non-atomic
	# note android/amd64 is needed for chromebook devices
	BUILD_DIR=android.`date +%s`; \
	WARP_VERSION=`warpctl ls version`; \
	mkdir -p "build/$$BUILD_DIR"; \
	gomobile bind \
		-target android/arm64,android/arm,android/amd64 -androidapi 24 \
		-javapkg com.bringyour \
		-trimpath \
		-gcflags "-dwarf=true" \
		-ldflags "-X client.Version=$$WARP_VERSION -compressdwarf=false -B gobuildid" \
		-o "build/$$BUILD_DIR/URnetworkSdk.aar" \
		github.com/urnetwork/sdk; \
	if [[ -e "build/android" ]]; then mv build/android build/android.old.`date +%s`; fi; \
	mv "build/$$BUILD_DIR/" build/android

	# validate that all types could be exported
	# note the device_rpc types (DeviceLocalRpc, DeviceRemote*) should not be included in gomobile
	# but due to limitations they are and should be ignored
	cd build/android; \
	    if [[ -e validate ]]; then mv validate validate.$$(date +%s); fi; \
		mkdir validate; \
		cp URnetworkSdk-sources.jar validate/; \
		cd validate; \
			jar -xf URnetworkSdk-sources.jar; \
			bad_exports=`grep -Ri '// skipped' . | grep -v DeviceLocalRpc | grep -v DeviceRemote`; \
			if [[ "$$bad_exports" ]]; then \
				echo "Some types could not be exported:"; \
				echo "$$bad_exports"; \
				exit 1; \
			fi;

build_ios:
	# *important* Xcode does not handle symbolic links consistently
	# the build dir swap is non-atomic
	BUILD_DIR=ios.`date +%s`; \
	WARP_VERSION=`warpctl ls version`; \
	mkdir -p "build/$$BUILD_DIR"; \
	gomobile bind \
		-ldflags "-X client.Version=$$WARP_VERSION" \
		-target ios/arm64,iossimulator/arm64 -iosversion 16.0 \
		-bundleid com.bringyour \
		-trimpath \
		-gcflags "-dwarf=true" \
		-ldflags "-X client.Version=$$WARP_VERSION -compressdwarf=false -B gobuildid" \
		-o "build/$$BUILD_DIR/URnetworkSdk.xcframework" \
		github.com/urnetwork/sdk; \
	if [[ -e "build/ios" ]]; then mv build/ios build/ios.old.`date +%s`; fi; \
	mv "build/$$BUILD_DIR/" build/ios

init:
	go install golang.org/x/mobile/cmd/gomobile@latest
	gomobile init
