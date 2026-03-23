import type { InitOptions } from "./types";

declare global {
  interface Window {
    Go: any;
    URnetworkNewProxyDeviceWithDefaults: any;
    URnetworkClose: any;
  }
}

let wasmInitialized = false;
let wasmInitPromise: Promise<void> | null = null;

async function loadWasmExec(url?: string): Promise<void> {
  if (typeof window.Go !== "undefined") {
    return;
  }

  const wasmExecUrl =
    url || new URL("../wasm/wasm_exec.js", import.meta.url).href;

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
      const wasmUrl =
        options.wasmUrl || new URL("../wasm/sdk.wasm", import.meta.url).href;
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
    URnetworkClose: window.URnetworkClose,
  };
}
