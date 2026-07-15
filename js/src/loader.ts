import type { InitOptions } from "./types";

declare global {
  interface Window {
    Go: any;
    URnetworkNewProxyDeviceWithDefaults: any;
    URnetworkNewPlatformDeviceRemote: any;
    URnetworkClose: any;
  }
}

let wasmInitialized = false;
let wasmInitPromise: Promise<void> | null = null;

// Resolve a packaged artifact relative to this module AT RUNTIME.
//
// The specifier is computed rather than a static literal on purpose. A literal
// `new URL("../wasm/sdk.wasm", import.meta.url)` makes bundlers (vite/rollup)
// statically emit the ~40 MB wasm as a bundle asset — even for consumers that
// pass explicit wasmUrl/wasmExecUrl, and even for consumers that never call
// init() at all (the browser extension was shipping ~19 MB of wasm it never
// loads for exactly this reason). Runtime resolution is unchanged; consumers who
// want a bundler-managed URL should pass wasmUrl/wasmExecUrl explicitly.
function packagedUrl(name: string): string {
  const rel = `../wasm/${name}`;
  return new URL(rel, import.meta.url).href;
}

async function loadWasmExec(url?: string): Promise<void> {
  if (typeof window.Go !== "undefined") {
    return;
  }

  const wasmExecUrl = url || packagedUrl("wasm_exec.js");

  return new Promise((resolve, reject) => {
    const script = document.createElement("script");
    script.src = wasmExecUrl;
    script.onload = () => resolve();
    script.onerror = () =>
      reject(new Error(`Failed to load wasm_exec.js from ${wasmExecUrl}`));
    document.head.appendChild(script);
  });
}

async function instantiateWasm(
  wasmUrl: string,
  go: any,
): Promise<WebAssembly.Instance> {
  let result: WebAssembly.WebAssemblyInstantiatedSource;

  if (WebAssembly.instantiateStreaming) {
    try {
      result = await WebAssembly.instantiateStreaming(
        fetch(wasmUrl),
        go.importObject,
      );
      return result.instance;
    } catch (e) {
      console.warn("Streaming instantiation failed, falling back to fetch:", e);
    }
  }

  const wasmBuffer = await fetch(wasmUrl).then((response) =>
    response.arrayBuffer(),
  );
  result = await WebAssembly.instantiate(wasmBuffer, go.importObject);
  return result.instance;
}

export async function initWasm(options: InitOptions = {}): Promise<void> {
  if (wasmInitPromise) {
    return wasmInitPromise;
  }

  if (wasmInitialized) {
    return;
  }

  wasmInitPromise = (async () => {
    try {
      await loadWasmExec(options.wasmExecUrl);
      const go = new window.Go();
      const wasmUrl = options.wasmUrl || packagedUrl("sdk.wasm");
      const wasmInstance = await instantiateWasm(wasmUrl, go);
      go.run(wasmInstance);
      wasmInitialized = true;
    } catch (error) {
      wasmInitPromise = null;
      throw new Error(`Failed to initialize URnetwork WASM: ${error}`);
    }
  })();

  return wasmInitPromise;
}

export function isWasmInitialized(): boolean {
  return wasmInitialized;
}

export function getWasmGlobals() {
  if (!wasmInitialized) {
    throw new Error("WASM not initialized. Call initWasm() first.");
  }

  return {
    URnetworkNewProxyDeviceWithDefaults:
      window.URnetworkNewProxyDeviceWithDefaults,
    // the DeviceRemote binding (sdk/js/device_remote.go) — a client's handle on
    // a hosted DeviceLocal, reached over the proxy host's device-rpc websocket
    URnetworkNewPlatformDeviceRemote: window.URnetworkNewPlatformDeviceRemote,
    URnetworkClose: window.URnetworkClose,
  };
}
