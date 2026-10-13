import typescript from "@rollup/plugin-typescript";
import resolve from "@rollup/plugin-node-resolve";
import { readFile } from "node:fs/promises";

// The lag SDK starts its download Worker from its own module,
// `new Worker(new URL("./downloadWorker.ts", import.meta.url))`, which a
// bundler like vite builds from the sources. dist has no .ts, so the lag
// entry names the downloadWorker.js built beside it instead; a build that
// cannot find the expression fails rather than ship a Worker that never
// loads.
const DOWNLOAD_WORKER_SOURCE = 'new URL("./downloadWorker.ts", import.meta.url)';
const downloadWorkerUrl = {
  name: "lag-download-worker-url",
  transform(code, id) {
    if (!id.endsWith("/src/lag/downloadSource.ts")) {
      return null;
    }
    if (!code.includes(DOWNLOAD_WORKER_SOURCE)) {
      this.error(`src/lag/downloadSource.ts no longer starts its Worker with ${DOWNLOAD_WORKER_SOURCE}`);
    }
    return { code: code.replace(DOWNLOAD_WORKER_SOURCE, 'new URL("./downloadWorker.js", import.meta.url)'), map: null };
  },
};

export default [
  // Main entry point (core SDK)
  {
    input: "src/index.ts",
    output: [
      {
        file: "dist/index.js",
        format: "es",
        sourcemap: true,
      },
      {
        file: "dist/index.cjs",
        format: "cjs",
        sourcemap: true,
      },
    ],
    external: ["react"],
    plugins: [
      resolve(),
      typescript({
        tsconfig: "./tsconfig.json",
        declaration: false, // handled by build:types script
      }),
      {
        name: "wasm-assets",
        async buildStart() {
          // The runtime is a fixed pair; no glob expansion is needed.
          for (const name of ["sdk.wasm", "wasm_exec.js"]) {
            const fileName = `wasm/${name}`;
            this.addWatchFile(fileName);
            this.emitFile({ type: "asset", fileName, source: await readFile(fileName) });
          }
        },
      },
    ],
  },
  // The api client alone (no wasm, no DOM): for service workers and other
  // fetch-only runtimes
  {
    input: "src/client.ts",
    output: [
      {
        file: "dist/client.js",
        format: "es",
        sourcemap: true,
      },
      {
        file: "dist/client.cjs",
        format: "cjs",
        sourcemap: true,
      },
    ],
    plugins: [
      resolve(),
      typescript({
        tsconfig: "./tsconfig.json",
        declaration: false,
      }),
    ],
  },
  // The lag.dev SDK (lag.dev/PLAN.md S): plain TypeScript that never loads
  // the wasm, so a page that imports only it never downloads sdk.wasm
  {
    input: "src/lag/index.ts",
    output: [
      {
        file: "dist/lag/index.js",
        format: "es",
        sourcemap: true,
      },
      {
        file: "dist/lag/index.cjs",
        format: "cjs",
        sourcemap: true,
      },
    ],
    plugins: [
      downloadWorkerUrl,
      resolve(),
      typescript({
        tsconfig: "./tsconfig.json",
        declaration: false,
      }),
    ],
  },
  // The lag SDK's download Worker (src/lag/downloadWorker.ts), a module of
  // its own beside the entry that starts it
  {
    input: "src/lag/downloadWorker.ts",
    output: {
      file: "dist/lag/downloadWorker.js",
      format: "es",
      sourcemap: true,
    },
    plugins: [
      resolve(),
      typescript({
        tsconfig: "./tsconfig.json",
        declaration: false,
      }),
    ],
  },
  // React entry point
  {
    input: "src/react/index.ts",
    output: [
      {
        file: "dist/react/index.js",
        format: "es",
        sourcemap: true,
      },
      {
        file: "dist/react/index.cjs",
        format: "cjs",
        sourcemap: true,
      },
    ],
    external: [
      "react",
      "react/jsx-runtime",
      "@tanstack/react-query",
      "@urnetwork/sdk",
    ],
    plugins: [
      resolve(),
      typescript({
        tsconfig: "./tsconfig.json",
        declaration: false,
      }),
    ],
  },
];
