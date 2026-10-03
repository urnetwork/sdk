// The sdk sources import each other without extensions (bundler resolution).
// Node's type stripping runs .ts directly but resolves like ESM, so a test that
// imports a source module with relative imports registers this hook first and
// then imports the module dynamically.
import { registerHooks } from "node:module";

let registered = false;

export function registerTsResolve(): void {
  if (registered) {
    return;
  }
  registered = true;
  registerHooks({
    resolve(specifier, context, nextResolve) {
      try {
        return nextResolve(specifier, context);
      } catch (error) {
        if (specifier.startsWith(".") && !/\.[cm]?[jt]sx?$/.test(specifier)) {
          try {
            return nextResolve(`${specifier}.ts`, context);
          } catch {
            return nextResolve(`${specifier}/index.ts`, context);
          }
        }
        throw error;
      }
    },
  });
}
