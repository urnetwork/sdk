// SPDX-License-Identifier: MPL-2.0
package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func npmPackageVersion() string {
	var source struct{ Version string }
	jsonRead(path("js/package.json"), &source)
	version := env("SDK_PACKAGE_VERSION", env("EXTERNAL_WARP_VERSION", source.Version))
	require(versionPattern.MatchString(version), "invalid npm SDK version")
	return version
}

func packageNpm() {
	var source map[string]any
	jsonRead(path("js/package.json"), &source)
	version := npmPackageVersion()
	out := path("js/release")
	must(os.RemoveAll(out))
	mkdir(filepath.Join(out, "artifacts"))
	temp, cleanup := temporary("urnetwork-npm-package-")
	defer cleanup()
	canonical := filepath.Join(temp, "sdk")
	legacy := filepath.Join(temp, "legacy")
	mkdir(canonical)
	mkdir(legacy)
	for _, name := range []string{"dist", "wasm"} {
		copyTree(path("js", name), filepath.Join(canonical, name), nil)
	}
	re := regexp.MustCompile(`(from\s+["'])(\.[^"']+)(["'])`)
	must(filepath.WalkDir(filepath.Join(canonical, "dist"), func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() || !strings.HasSuffix(p, ".d.ts") {
			return nil
		}
		content := re.ReplaceAllStringFunc(string(read(p)), func(s string) string {
			m := re.FindStringSubmatch(s)
			target := m[2]
			if exists(filepath.Join(filepath.Dir(p), target+".d.ts")) {
				target += ".js"
			} else if exists(filepath.Join(filepath.Dir(p), target, "index.d.ts")) {
				target += "/index.js"
			}
			return m[1] + target + m[3]
		})
		textFile(p, content)
		return nil
	}))
	for _, name := range []string{"sdk.wasm", "wasm_exec.js"} {
		stat, e := os.Stat(filepath.Join(canonical, "wasm", name))
		must(e)
		require(stat.Size() > 0, "missing WASM runtime")
	}
	copyFile(path("js/README.md"), filepath.Join(canonical, "README.md"))
	source["name"] = "@urnetwork/sdk"
	source["version"] = version
	delete(source, "scripts")
	delete(source, "devDependencies")
	source["exports"].(map[string]any)["./wasm/*"] = "./wasm/*"
	jsonWrite(filepath.Join(canonical, "package.json"), source)
	exports := map[string]any{}
	for _, entry := range []string{".", "./react"} {
		dir := legacy
		target := "@urnetwork/sdk"
		prefix := "."
		if entry == "./react" {
			dir = filepath.Join(legacy, "react")
			target += "/react"
			prefix = "./react"
		}
		js := "export * from \"" + target + "\";\n"
		if entry == "." {
			js += "export {default} from \"" + target + "\";\n"
		}
		textFile(filepath.Join(dir, "index.js"), js)
		textFile(filepath.Join(dir, "index.d.ts"), js)
		textFile(filepath.Join(dir, "index.cjs"), "module.exports = require(\""+target+"\");\n")
		exports[entry] = map[string]string{"types": prefix + "/index.d.ts", "import": prefix + "/index.js", "require": prefix + "/index.cjs"}
	}
	jsonWrite(filepath.Join(legacy, "package.json"), map[string]any{"name": "@urnetwork/sdk-js", "version": version, "type": "module",
		"description": "Compatibility imports for @urnetwork/sdk", "license": "MPL-2.0", "main": "./index.cjs", "types": "./index.d.ts",
		"dependencies": map[string]string{"@urnetwork/sdk": version}, "exports": exports})
	inv := inventory{Version: version}
	for _, dir := range []string{canonical, legacy} {
		var packed []struct {
			Filename string `json:"filename"`
		}
		must(json.Unmarshal(output(dir, nil, "npm", "pack", "--ignore-scripts", "--json", "--pack-destination", filepath.Join(out, "artifacts")), &packed))
		require(len(packed) == 1, "npm pack did not produce one artifact")
		p := filepath.Join(out, "artifacts", packed[0].Filename)
		stat, e := os.Stat(p)
		must(e)
		inv.Artifacts = append(inv.Artifacts, artifact{packed[0].Filename, hash(p), stat.Size()})
	}
	jsonWrite(filepath.Join(out, "manifest.json"), inv)
}
func checkNpm() {
	out := path("js/release")
	var manifest inventory
	jsonRead(filepath.Join(out, "manifest.json"), &manifest)
	_, artifacts := verifiedFiles(out, manifest.Version, false)
	temp, cleanup := temporary("urnetwork-npm-consumer-")
	defer cleanup()
	textFile(filepath.Join(temp, "package.json"), "{\"private\":true,\"type\":\"module\"}\n")
	command(temp, nil, "npm", append([]string{"install", "--ignore-scripts", "--no-audit", "--no-fund", "--omit=peer"}, artifacts...)...)
	textFile(filepath.Join(temp, "check.mjs"), `import assert from "node:assert/strict";
import canonical, * as sdk from "@urnetwork/sdk";
import legacy from "@urnetwork/sdk-js";
import {createRequire} from "node:module";
const require = createRequire(import.meta.url);
assert.equal(canonical, legacy);
assert.equal(require("@urnetwork/sdk").URNetwork, require("@urnetwork/sdk-js").URNetwork);
assert.equal(typeof sdk.Conn, "function");
assert.equal(typeof sdk.WebTransport, "function");
assert.ok(require.resolve("@urnetwork/sdk/wasm/sdk.wasm"));
const initialized = await sdk.URNetwork.init();
assert.equal(initialized.isInitialized(), true);
initialized.close();
await new Promise(resolve => setTimeout(resolve, 30));
// Exercise the emitted CommonJS asset URL logic in a separate module instance.
const cjs = await require("@urnetwork/sdk").URNetwork.init();
assert.equal(cjs.isInitialized(), true);
cjs.close();
await new Promise(resolve => setTimeout(resolve, 30));
`)
	command(temp, nil, "node", "check.mjs")
	textFile(filepath.Join(temp, "check.ts"), "import {Conn, URNetwork, WebTransport} from \"@urnetwork/sdk\";\nimport Legacy from \"@urnetwork/sdk-js\";\nconst same: typeof URNetwork = Legacy;\nlet conn: Conn; let transport: WebTransport;\n")
	command(temp, nil, path("js/node_modules/.bin/tsc"), "--noEmit", "--skipLibCheck", "--target", "ES2022", "--module", "NodeNext", "--moduleResolution", "NodeNext", "check.ts")
	markChecked(out)
}
