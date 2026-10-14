import typescript from "@rollup/plugin-typescript";
import resolve from "@rollup/plugin-node-resolve";

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
  plugins: [
    resolve({ preferBuiltins: true, exportConditions: ["node"] }),
    typescript({ tsconfig: "./tsconfig.json" }),
  ],
};
