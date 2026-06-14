all: clean build

clean:
	rm -rf wasm
	rm -rf dist
	$(MAKE) -C ../build clean

build:
	mkdir wasm && \
	$(MAKE) -C ../build build_js && \
	cp ../build/js/URnetworkSdkJs/sdk.wasm wasm/ && \
	cp ../build/js/URnetworkSdkJs/wasm_exec.js wasm/ && \
	npm ci && \
	npm run build;
