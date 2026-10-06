import typescript from "@rollup/plugin-typescript";
import resolve from "@rollup/plugin-node-resolve";
import { readFile } from "node:fs/promises";

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
