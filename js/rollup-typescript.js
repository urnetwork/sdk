import { readFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";

// Our bundles have local TypeScript sources and explicit external packages.
// TypeScript can resolve those local imports and compile isolated modules
// directly, without a glob matcher in the build. Each package type-checks
// with tsc before bundling.
export function typescript(ts, tsconfig) {
  const configPath = resolve(tsconfig);
  const format = (diagnostics) => ts.formatDiagnostics(diagnostics, {
    getCanonicalFileName: (file) => file,
    getCurrentDirectory: ts.sys.getCurrentDirectory,
    getNewLine: () => ts.sys.newLine,
  }).trim();
  let options;

  return {
    name: "sdk-typescript",
    buildStart() {
      this.addWatchFile(configPath);
      const { config, error } = ts.readConfigFile(configPath, ts.sys.readFile);
      if (error) this.error(format([error]));
      const parsed = ts.parseJsonConfigFileContent(config, ts.sys, dirname(configPath), {}, configPath);
      if (parsed.errors.length) this.error(format(parsed.errors));
      options = { ...parsed.options, declaration: false, declarationMap: false };
    },
    resolveId(source, importer) {
      if (!importer || !/^\.{1,2}\//.test(source)) return null;
      const { resolvedModule } = ts.resolveModuleName(source, importer, options, ts.sys);
      const file = resolvedModule?.resolvedFileName;
      return file && !/\.d\.[cm]?ts$/.test(file) ? file : null;
    },
    async load(id) {
      if (!/\.tsx?$/.test(id)) return null;
      const source = await readFile(id, "utf8");
      const output = ts.transpileModule(source, {
        compilerOptions: options,
        fileName: id,
        reportDiagnostics: true,
      });
      if (output.diagnostics?.length) this.error(format(output.diagnostics));
      return { code: output.outputText, map: output.sourceMapText };
    },
  };
}
