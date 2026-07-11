all: clean build

clean:
	rm -rf wasm
	rm -rf dist
	rm -rf build

# Generate TypeScript types from Go structs
generate_types:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	go run gen_types.go;

# Build the WASM binary and the Go JS runtime support file.
#
# wasm_exec.js is ABI-paired with the Go toolchain that compiled the wasm, so
# it is always copied from the building toolchain (never vendored) and sourced
# from `go env GOROOT` (which honors the toolchain directive in go.mod), not a
# hardcoded path. Go >= 1.24 keeps it under lib/wasm; older toolchains under
# misc/wasm. `check_wasm` then gates on the pairing.
build_wasm:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	export GOEXPERIMENT=greenteagc; \
	mkdir -p wasm; \
	GOOS=js GOARCH=wasm go build -o wasm/sdk.wasm .; \
	GOROOT="$$(go env GOROOT)"; \
	if [ -f "$$GOROOT/lib/wasm/wasm_exec.js" ]; then \
		cp "$$GOROOT/lib/wasm/wasm_exec.js" wasm/; \
	else \
		cp "$$GOROOT/misc/wasm/wasm_exec.js" wasm/; \
	fi; \
	$(MAKE) check_wasm

# Verify the packaged wasm_exec.js is byte-identical to the building toolchain's
# copy, so a committed or published glue can never skew from the wasm it loads.
# The wasm binary itself is paired by construction (built and copied in the same
# build_wasm target from one toolchain); `go version <wasm>` cannot read js/wasm
# build info, so the glue byte-check is the reliable gate. Run this standalone in
# CI (against the committed artifacts) to catch a stale wasm_exec.js.
check_wasm:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	GOROOT="$$(go env GOROOT)"; \
	toolchain_version="$$(go version | awk '{print $$3}')"; \
	glue="$$GOROOT/lib/wasm/wasm_exec.js"; \
	if [ ! -f "$$glue" ]; then glue="$$GOROOT/misc/wasm/wasm_exec.js"; fi; \
	if ! cmp -s "$$glue" wasm/wasm_exec.js; then \
		echo "packaged wasm_exec.js does not match the toolchain ($$toolchain_version)"; \
		exit 1; \
	fi; \
	echo "wasm_exec.js paired with $$toolchain_version"

# Package the standalone browser bundle (wasm + sample loader) for release
build_bundle: build_wasm
	rm -rf build; \
	mkdir -p build/URnetworkSdkJs; \
	cp wasm/sdk.wasm wasm/wasm_exec.js index.js index.html build/URnetworkSdkJs/; \
	(cd build && zip -r URnetworkSdkJs.zip URnetworkSdkJs);

# Build the npm package (dist) from the wasm artifacts
build: generate_types build_bundle
	npm ci && \
	npm run build;
