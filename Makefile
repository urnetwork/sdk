
all: clean build_android build_ios

clean:
	rm -rf build

build_android:
	mkdir -p build/android
# 	gomobile bind -target=android -androidapi 19 -javapkg com.bringyour.network -o build/android/client.aar
	WARP_VERSION=`warpctl ls version`; \
	gomobile bind \
		-target=android/arm64,android/arm,android/amd64 -androidapi 24 \
		-javapkg com.bringyour \
		-trimpath \
		-gcflags="-dwarf=true" \
		-ldflags="-X client.Version=$$WARP_VERSION -compressdwarf=false -B gobuildid" \
		-o build/android/URnetworkSdk.aar \
		github.com/urnetwork/sdk

	# validate that all types could be exported
	cd build/android; \
	    if [[ -e validate ]]; then mv validate validate.$$(date +%s); fi; \
		mkdir validate; \
		cp URnetworkSdk-sources.jar validate/; \
		cd validate; \
			jar -xf URnetworkSdk-sources.jar; \
			bad_exports=`grep -Ri '// skipped' .`; \
			if [[ "$$bad_exports" ]]; then \
				echo "Some types could not be exported:"; \
				echo "$$bad_exports"; \
				exit 1; \
			fi;

build_ios:
	mkdir -p build/ios
	# -prefix com.bringyour.network.client
# 	gomobile bind -target=ios -iosversion 14.0 -o build/ios/Client.xcframework
# 	gomobile bind -target=ios -iosversion 14.0 -o build/ios/Client.xcframework bringyour.com/client bringyour.com/client/device bringyour.com/client/vc
	WARP_VERSION=`warpctl ls version`; \
	gomobile bind \
		-ldflags "-X client.Version=$$WARP_VERSION" \
		-target=ios -iosversion 14.0 \
		-bundleid com.bringyour \
		-trimpath \
		-o build/ios/URnetworkSdk.xcframework \
		github.com/urnetwork/sdk

init:
	go install golang.org/x/mobile/cmd/gomobile@latest
	gomobile init
