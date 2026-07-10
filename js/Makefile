all: clean build

clean:
	rm -rf wasm
	rm -rf dist
	rm -rf build

# Generate TypeScript types from Go structs
generate_types:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	go run gen_types.go;

# Build the WASM binary and the Go JS runtime support file
build_wasm:
	export PATH="$$PATH:/usr/local/go/bin:$$HOME/go/bin"; \
	export GOEXPERIMENT=greenteagc; \
	mkdir -p wasm; \
	GOOS=js GOARCH=wasm go build -o wasm/sdk.wasm .; \
	cp /usr/local/go/lib/wasm/wasm_exec.js wasm/;

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
