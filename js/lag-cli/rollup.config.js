import ts from "typescript";
import { typescript } from "../rollup-typescript.js";

// One program, dist/cli.js, with the SDK's lag client (../src/lag) compiled in
// and the package's own dependencies left to node_modules.
export default {
  input: "src/cli.ts",
  output: {
    file: "dist/cli.js",
    format: "es",
    sourcemap: true,
    banner: "#!/usr/bin/env node",
  },
  external: [/^node:/, "undici", /^@modelcontextprotocol\/sdk/, "zod"],
  plugins: [typescript(ts, "./tsconfig.json")],
};
