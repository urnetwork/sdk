

// JS sample to load the SDK and initiate a proxy device
// **important** only one instance of the SDK can be loaded at a time,
//               since it exports global function names
//               unlimited proxy devices can be created from the single SDK instance


// from https://wasmbyexample.dev/examples/hello-world/hello-world.go.en-us

// https://github.com/torch2424/wasm-by-example/blob/master/demo-util/
export const wasmBrowserInstantiate = async (wasmModuleUrl, importObject) => {
  let response = undefined;

  // Check if the browser supports streaming instantiation
  if (WebAssembly.instantiateStreaming) {
    // Fetch the module, and instantiate it as it is downloading
    response = await WebAssembly.instantiateStreaming(
      fetch(wasmModuleUrl),
      importObject
    );
  } else {
    // Fallback to using fetch to download the entire module
    // And then instantiate the module
    const fetchAndInstantiateTask = async () => {
      const wasmArrayBuffer = await fetch(wasmModuleUrl).then(response =>
        response.arrayBuffer()
      );
      return WebAssembly.instantiate(wasmArrayBuffer, importObject);
    };

    response = await fetchAndInstantiateTask();
  }

  return response;
};


const loadURnetwork = async () => {
  // defined in wasm_exec.js
  const go = new Go();

  const wasmModule = await wasmBrowserInstantiate("./sdk.wasm", go.importObject);

  go.run(wasmModule.instance);

  let proxyDevice = URnetworkNewProxyDeviceWithDefaults(
    {
      enableHttp: true
    },
    (device, proxyConfigResult) => {

      /*
      r = device.search("United States")
      if (0 < r.results.length) {
        let locationId = r.results[0].locationId
        device.setDestination({
          locationId: locationId
        })
      }
      */

      /*
      device.setPerformanceProfile("speed", 1, 1)
      */

      /*
      proxyConfigResult.httpProxyUrl
      */
    }
  );

  // proxyDevice.Close()
  // URnetworkClose()
};
loadURnetwork();
