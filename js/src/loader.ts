import type { InitOptions } from "./types";

declare global {
  interface Window {
    Go: any;
    URnetworkNewProxyDeviceWithDefaults: any;
    URnetworkNewPlatformDeviceRemote: any;
    URnetworkNewExtensionDeviceRemote: any;
    URnetworkNewLocationsViewController: any;
    URnetworkNewAccountHost: any;
    URnetworkColorHex: any;
    URnetworkFilteredLocationsFromResult: any;
    URnetworkGetLicenses: any;
    URnetworkClose: any;
  }
}

let wasmInitialized = false;
let wasmInitPromise: Promise<void> | null = null;
const runtimeGlobal = globalThis as unknown as Window;
const isNode = () => Boolean((globalThis as any).process?.versions?.node);

async function readNodeFile(url: string): Promise<Uint8Array> {
  // Computed builtin specifier keeps browser bundles free of Node polyfills.
  const moduleName = "node:fs/promises";
  const fs = await import(/* @vite-ignore */ moduleName);
  return fs.readFile(new URL(url));
}

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
  if (typeof runtimeGlobal.Go !== "undefined") {
    return;
  }

  const wasmExecUrl = url || packagedUrl("wasm_exec.js");

  if (isNode()) {
    // The Go runtime glue registers Go on globalThis in both runtimes.
    if (wasmExecUrl.startsWith("file:")) {
      await import(/* @vite-ignore */ wasmExecUrl);
    } else {
      const response = await fetch(wasmExecUrl);
      if (!response.ok) throw new Error("Could not load wasm_exec.js: HTTP " + response.status);
      const moduleName = "node:vm";
      const vm = await import(/* @vite-ignore */ moduleName);
      vm.runInThisContext(await response.text(), {filename: wasmExecUrl});
    }
    return;
  }

  return new Promise((resolve, reject) => {
    const script = document.createElement("script");
    script.src = wasmExecUrl;
    script.onload = () => resolve();
    script.onerror = () =>
      reject(new Error(`Failed to load wasm_exec.js from ${wasmExecUrl}`));
    document.head.appendChild(script);
  });
}

// Fetch exactly once. A rejected fetch (notably document teardown) must not
// start a second request in a dying document. Only streaming compilation may
// fall back, using the same response bytes and preserving real fetch errors.
export async function instantiateWasm(
  wasmUrl: string,
  go: any,
): Promise<WebAssembly.Instance> {
  let result: WebAssembly.WebAssemblyInstantiatedSource;

  if (isNode() && wasmUrl.startsWith("file:")) {
    result = await WebAssembly.instantiate(await readNodeFile(wasmUrl) as BufferSource, go.importObject);
    return result.instance;
  }

  const response = await fetch(wasmUrl);
  if (!response.ok) throw new Error(`Could not load WASM: HTTP ${response.status}`);
  if (WebAssembly.instantiateStreaming) {
    try {
      result = await WebAssembly.instantiateStreaming(
        response.clone(),
        go.importObject,
      );
      return result.instance;
    } catch (e) {
      console.warn("Streaming instantiation failed, falling back to fetch:", e);
    }
  }

  const wasmBuffer = await response.arrayBuffer();
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
      const go = new runtimeGlobal.Go();
      const previousGlobals = new Set(Object.keys(globalThis));
      let registeredGlobals: string[] = [];
      const originalExit = go.exit;
      go.exit = (code: number) => {
        // wasm_exec.js is paired with our Go toolchain. Its timer table can
        // retain background wakeups after main exits; those must not resume
        // an exited runtime (or keep a Node process alive).
        for (const timer of go._scheduledTimeouts?.values() || []) clearTimeout(timer);
        go._scheduledTimeouts?.clear();
        wasmInitialized = false;
        wasmInitPromise = null;
        for (const key of registeredGlobals) delete (globalThis as any)[key];
        originalExit(code);
      };
      const wasmUrl = options.wasmUrl || packagedUrl("sdk.wasm");
      const wasmInstance = await instantiateWasm(wasmUrl, go);
      // Go package initialization can yield before main registers the bridge.
      // In Node this is observable even with a local, fully loaded WASM file.
      await new Promise<void>((resolve, reject) => {
        let ready = false;
        let timer: ReturnType<typeof setTimeout>;
        const expires = Date.now() + 30000;
        const check = () => {
          if (typeof runtimeGlobal.URnetworkNewPlatformDeviceRemote === "function" &&
              typeof runtimeGlobal.URnetworkClose === "function") {
            ready = true;
            registeredGlobals = Object.keys(globalThis).filter(key => key.startsWith("URnetwork") && !previousGlobals.has(key));
            resolve();
          } else if (Date.now() >= expires) {
            reject(new Error("Go runtime did not register its SDK exports"));
          } else {
            timer = setTimeout(check, 5);
          }
        };
        Promise.resolve(go.run(wasmInstance)).then(() => {
          clearTimeout(timer);
          if (!ready) reject(new Error("Go runtime exited before registering SDK exports"));
        }, error => { clearTimeout(timer); reject(error); });
        check();
      });
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
      runtimeGlobal.URnetworkNewProxyDeviceWithDefaults,
    // the DeviceRemote binding (sdk/js/device_remote.go) — a client's handle on
    // a hosted DeviceLocal, reached over the proxy host's device-rpc websocket
    URnetworkNewPlatformDeviceRemote: runtimeGlobal.URnetworkNewPlatformDeviceRemote,
    URnetworkNewExtensionDeviceRemote: runtimeGlobal.URnetworkNewExtensionDeviceRemote,
    URnetworkNewLocationsViewController: runtimeGlobal.URnetworkNewLocationsViewController,
    URnetworkNewAccountHost: runtimeGlobal.URnetworkNewAccountHost,
    URnetworkColorHex: runtimeGlobal.URnetworkColorHex,
    URnetworkFilteredLocationsFromResult: runtimeGlobal.URnetworkFilteredLocationsFromResult,
    URnetworkGetLicenses: runtimeGlobal.URnetworkGetLicenses,
    URnetworkClose: runtimeGlobal.URnetworkClose,
  };
}
