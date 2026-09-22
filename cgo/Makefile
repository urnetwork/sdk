
.PHONY: all clean generate build build_windows build_windows_amd64 \
	build_windows_arm64 build_linux build_linux_amd64 build_linux_arm64 \
	build_host smoke smoke_hpp smoke_memory_usage_json smoke_transport_stats_json init

all: clean build

# Keep the module boundary present even while concurrent discovery sees cleanup.
clean:
	@if [ -L build ]; then \
		echo "refusing to clean symbolic cgo build directory" >&2; exit 1; \
	elif [ -d build ]; then \
		find -P build -mindepth 1 -maxdepth 1 ! -name go.mod -exec rm -rf -- {} +; \
	elif [ -e build ]; then \
		echo "cgo build path is not a directory" >&2; exit 1; \
	fi

# regenerate the c abi from the sdk surface
# updates exports_gen*.go, callbacks.h/.c, include/, and coverage_report.txt
generate:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin" && \
	go run ./gen;

build: generate build_windows build_linux

# cross toolchains, assuming a macOS build host (see init):
#   windows/amd64: mingw-w64 (x86_64-w64-mingw32-gcc)
#   windows/arm64: llvm-mingw (aarch64-w64-mingw32-clang)
#   linux: zig cc, pinning the ubuntu 22.04 glibc 2.35 floor
WINDOWS_AMD64_CC ?= x86_64-w64-mingw32-gcc
SDK_GO_MODULE ?= $(shell awk '$$1 ~ /^github.com\/urnetwork\/sdk(\/v[0-9]+)?$$/ { print $$1; exit }' go.mod)
WINDOWS_ARM64_CC ?= aarch64-w64-mingw32-clang
LINUX_AMD64_CC ?= zig cc -target x86_64-linux-gnu.2.35
LINUX_ARM64_CC ?= zig cc -target aarch64-linux-gnu.2.35

.PHONY: build_darwin build_darwin_amd64 build_darwin_arm64
.PHONY: package check-package publish publish-conan publish-vcpkg
package:
	go -C ../packaging run . c package
check-package:
	go -C ../packaging run . c check
publish: publish-conan publish-vcpkg
publish-conan:
	go -C ../packaging run . publish conan
publish-vcpkg:
	go -C ../packaging run . publish vcpkg
build_darwin: build_darwin_amd64 build_darwin_arm64
build_darwin_amd64 build_darwin_arm64:
	@arch="$(@:build_darwin_%=%)"; \
	mkdir -p "build/darwin/$$arch" && \
	rm -f "build/darwin/$$arch/libURnetworkSdk.dylib" && \
	CGO_ENABLED=1 GOOS=darwin GOARCH="$$arch" MACOSX_DEPLOYMENT_TARGET=13.5 \
	go build -trimpath -buildmode=c-shared -ldflags "-s -w -X $(SDK_GO_MODULE).Version=$${WARP_VERSION:-0.0.1-dev.0} -buildid=" \
		-o "build/darwin/$$arch/libURnetworkSdk.dylib" . && \
	install_name_tool -id @rpath/libURnetworkSdk.dylib "build/darwin/$$arch/libURnetworkSdk.dylib" && \
	codesign --force --sign - "build/darwin/$$arch/libURnetworkSdk.dylib" && \
	cp include/urnetwork_sdk.h include/urnetwork_sdk.hpp "build/darwin/$$arch/"

build_windows: build_windows_amd64 build_windows_arm64
	rm -f build/URnetworkSdkWindows.zip && \
	(cd build && zip -r URnetworkSdkWindows.zip windows);

build_windows_amd64:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin" && \
	export GOEXPERIMENT=greenteagc && \
	mkdir -p build/windows/amd64 && \
	rm -f build/windows/amd64/URnetworkSdk.dll && \
	CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC="$(WINDOWS_AMD64_CC)" \
	go build -trimpath -buildmode=c-shared \
		-ldflags "-s -w -X $(SDK_GO_MODULE).Version=$${WARP_VERSION} -buildid=" \
		-o build/windows/amd64/URnetworkSdk.dll . && \
	rm -f build/windows/amd64/URnetworkSdk.h && \
	cp include/urnetwork_sdk.h include/urnetwork_sdk.hpp include/urnetwork_sdk.def build/windows/amd64/;

build_windows_arm64:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin" && \
	export GOEXPERIMENT=greenteagc && \
	mkdir -p build/windows/arm64 && \
	rm -f build/windows/arm64/URnetworkSdk.dll && \
	CGO_ENABLED=1 GOOS=windows GOARCH=arm64 CC="$(WINDOWS_ARM64_CC)" \
	go build -trimpath -buildmode=c-shared \
		-ldflags "-s -w -X $(SDK_GO_MODULE).Version=$${WARP_VERSION} -buildid=" \
		-o build/windows/arm64/URnetworkSdk.dll . && \
	rm -f build/windows/arm64/URnetworkSdk.h && \
	cp include/urnetwork_sdk.h include/urnetwork_sdk.hpp include/urnetwork_sdk.def build/windows/arm64/;

build_linux: build_linux_amd64 build_linux_arm64
	rm -f build/URnetworkSdkLinux.zip && \
	(cd build && zip -r URnetworkSdkLinux.zip linux);

build_linux_amd64:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin" && \
	export GOEXPERIMENT=greenteagc && \
	mkdir -p build/linux/amd64 && \
	rm -f build/linux/amd64/libURnetworkSdk.so && \
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC="$(LINUX_AMD64_CC)" \
	go build -trimpath -buildmode=c-shared \
		-ldflags "-s -w -X $(SDK_GO_MODULE).Version=$${WARP_VERSION} -buildid=" \
		-o build/linux/amd64/libURnetworkSdk.so . && \
	rm -f build/linux/amd64/libURnetworkSdk.h && \
	cp include/urnetwork_sdk.h include/urnetwork_sdk.hpp build/linux/amd64/;

build_linux_arm64:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin" && \
	export GOEXPERIMENT=greenteagc && \
	mkdir -p build/linux/arm64 && \
	rm -f build/linux/arm64/libURnetworkSdk.so && \
	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC="$(LINUX_ARM64_CC)" \
	go build -trimpath -buildmode=c-shared \
		-ldflags "-s -w -X $(SDK_GO_MODULE).Version=$${WARP_VERSION} -buildid=" \
		-o build/linux/arm64/libURnetworkSdk.so . && \
	rm -f build/linux/arm64/libURnetworkSdk.h && \
	cp include/urnetwork_sdk.h include/urnetwork_sdk.hpp build/linux/arm64/;

# host (macOS) library, for local development and the smoke test
build_host:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin" && \
	export GOEXPERIMENT=greenteagc && \
	mkdir -p build/host && \
	rm -f build/host/libURnetworkSdk.dylib && \
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
		-ldflags "-X $(SDK_GO_MODULE).Version=$${WARP_VERSION}" \
		-o build/host/libURnetworkSdk.dylib . && \
	rm -f build/host/libURnetworkSdk.h;

# nlohmann/json for the c++ wrapper (header-only): brew install nlohmann-json
NLOHMANN_INCLUDE ?= /opt/homebrew/include

# abi smoke test against the host library
smoke: build_host
	clang++ -std=c++17 -Iinclude -o build/host/smoke smoke/smoke.cpp build/host/libURnetworkSdk.dylib && \
	DYLD_LIBRARY_PATH=build/host ./build/host/smoke;

# c++ wrapper (hpp) smoke test against the host library
smoke_hpp: build_host smoke_memory_usage_json smoke_transport_stats_json
	clang++ -std=c++17 -Iinclude -I$(NLOHMANN_INCLUDE) -o build/host/smoke_hpp smoke/smoke_hpp.cpp build/host/libURnetworkSdk.dylib && \
	DYLD_LIBRARY_PATH=build/host ./build/host/smoke_hpp;

# Shipped header JSON conversions, without constructing devices or linking Go.
smoke_memory_usage_json:
	mkdir -p build/host && \
	clang++ -std=c++17 -Iinclude -I$(NLOHMANN_INCLUDE) -o build/host/memory_usage_json smoke/memory_usage_json.cpp && \
	./build/host/memory_usage_json;

# Shipped desktop transport stats, including old-library JSON defaults.
smoke_transport_stats_json:
	mkdir -p build/host && \
	clang++ -std=c++17 -Iinclude -I$(NLOHMANN_INCLUDE) -o build/host/transport_stats_json smoke/transport_stats_json.cpp && \
	./build/host/transport_stats_json;

# Toolchains for the mac-host cgo cross-builds:
#   zig            -> linux/{amd64,arm64} SDK (glibc-pinned for portability)
#   nlohmann-json  -> host hpp smoke test (smoke_hpp)
#   mingw-w64      -> optional local windows/amd64 dev builds
# The shipped Windows SDK (amd64+arm64) now builds INSIDE the Windows VM, where
# Go + llvm-mingw are provisioned (see build/all/build-windows.sh). There is no
# `llvm-mingw` Homebrew formula — it was removed from this list.
init:
	brew install mingw-w64 zig nlohmann-json
