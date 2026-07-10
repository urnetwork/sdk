
all: clean build

clean:
	rm -rf build

# regenerate the c abi from the sdk surface
# updates exports_gen*.go, callbacks.h/.c, include/, and coverage_report.txt
generate:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	go run ./gen;

build: generate build_windows build_linux

# cross toolchains, assuming a macOS build host (see init):
#   windows/amd64: mingw-w64 (x86_64-w64-mingw32-gcc)
#   windows/arm64: llvm-mingw (aarch64-w64-mingw32-clang)
#   linux: zig cc, pinning the ubuntu 22.04 glibc 2.35 floor
WINDOWS_AMD64_CC ?= x86_64-w64-mingw32-gcc
WINDOWS_ARM64_CC ?= aarch64-w64-mingw32-clang
LINUX_AMD64_CC ?= zig cc -target x86_64-linux-gnu.2.35
LINUX_ARM64_CC ?= zig cc -target aarch64-linux-gnu.2.35

build_windows: build_windows_amd64 build_windows_arm64
	rm -f build/URnetworkSdkWindows.zip; \
	(cd build && zip -r URnetworkSdkWindows.zip windows);

build_windows_amd64:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	export GOEXPERIMENT=greenteagc; \
	mkdir -p build/windows/amd64; \
	CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC="$(WINDOWS_AMD64_CC)" \
	go build -trimpath -buildmode=c-shared \
		-ldflags "-s -w -X github.com/urnetwork/sdk.Version=$${WARP_VERSION} -buildid=" \
		-o build/windows/amd64/URnetworkSdk.dll .; \
	rm -f build/windows/amd64/URnetworkSdk.h; \
	cp include/urnetwork_sdk.h include/urnetwork_sdk.hpp include/urnetwork_sdk.def build/windows/amd64/;

build_windows_arm64:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	export GOEXPERIMENT=greenteagc; \
	mkdir -p build/windows/arm64; \
	CGO_ENABLED=1 GOOS=windows GOARCH=arm64 CC="$(WINDOWS_ARM64_CC)" \
	go build -trimpath -buildmode=c-shared \
		-ldflags "-s -w -X github.com/urnetwork/sdk.Version=$${WARP_VERSION} -buildid=" \
		-o build/windows/arm64/URnetworkSdk.dll .; \
	rm -f build/windows/arm64/URnetworkSdk.h; \
	cp include/urnetwork_sdk.h include/urnetwork_sdk.hpp include/urnetwork_sdk.def build/windows/arm64/;

build_linux: build_linux_amd64 build_linux_arm64
	rm -f build/URnetworkSdkLinux.zip; \
	(cd build && zip -r URnetworkSdkLinux.zip linux);

build_linux_amd64:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	export GOEXPERIMENT=greenteagc; \
	mkdir -p build/linux/amd64; \
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC="$(LINUX_AMD64_CC)" \
	go build -trimpath -buildmode=c-shared \
		-ldflags "-s -w -X github.com/urnetwork/sdk.Version=$${WARP_VERSION} -buildid=" \
		-o build/linux/amd64/libURnetworkSdk.so .; \
	rm -f build/linux/amd64/libURnetworkSdk.h; \
	cp include/urnetwork_sdk.h include/urnetwork_sdk.hpp build/linux/amd64/;

build_linux_arm64:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	export GOEXPERIMENT=greenteagc; \
	mkdir -p build/linux/arm64; \
	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC="$(LINUX_ARM64_CC)" \
	go build -trimpath -buildmode=c-shared \
		-ldflags "-s -w -X github.com/urnetwork/sdk.Version=$${WARP_VERSION} -buildid=" \
		-o build/linux/arm64/libURnetworkSdk.so .; \
	rm -f build/linux/arm64/libURnetworkSdk.h; \
	cp include/urnetwork_sdk.h include/urnetwork_sdk.hpp build/linux/arm64/;

# host (macOS) library, for local development and the smoke test
build_host:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	export GOEXPERIMENT=greenteagc; \
	mkdir -p build/host; \
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
		-ldflags "-X github.com/urnetwork/sdk.Version=$${WARP_VERSION}" \
		-o build/host/libURnetworkSdk.dylib .; \
	rm -f build/host/libURnetworkSdk.h;

# nlohmann/json for the c++ wrapper (header-only): brew install nlohmann-json
NLOHMANN_INCLUDE ?= /opt/homebrew/include

# abi smoke test against the host library
smoke: build_host
	clang++ -std=c++17 -Iinclude -o build/host/smoke smoke/smoke.cpp build/host/libURnetworkSdk.dylib && \
	DYLD_LIBRARY_PATH=build/host ./build/host/smoke;

# c++ wrapper (hpp) smoke test against the host library
smoke_hpp: build_host
	clang++ -std=c++17 -Iinclude -I$(NLOHMANN_INCLUDE) -o build/host/smoke_hpp smoke/smoke_hpp.cpp build/host/libURnetworkSdk.dylib && \
	DYLD_LIBRARY_PATH=build/host ./build/host/smoke_hpp;

init:
	brew install mingw-w64 llvm-mingw zig nlohmann-json
