import typescript from "@rollup/plugin-typescript";
import resolve from "@rollup/plugin-node-resolve";
import copy from "rollup-plugin-copy";

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
      copy({
        targets: [{ src: "wasm/*", dest: "dist/wasm" }],
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
      "@urnetwork/sdk-js",
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
