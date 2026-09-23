// SPDX-License-Identifier: MPL-2.0
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func verifiedFiles(dir, version string, needReceipt bool) (inventory, []string) {
	var inv inventory
	jsonRead(filepath.Join(dir, "manifest.json"), &inv)
	require(inv.Version == version, "package version does not match this release")
	if needReceipt {
		var c checked
		jsonRead(filepath.Join(dir, "checked.json"), &c)
		require(c.ManifestSHA256 == hash(filepath.Join(dir, "manifest.json")), "package manifest changed after its consumer check")
	}
	result := []string{}
	for _, entry := range inv.Artifacts {
		require(filepath.Base(entry.Name) == entry.Name && entry.Name != ".", "invalid artifact filename")
		p := filepath.Join(dir, "artifacts", entry.Name)
		s, e := os.Lstat(p)
		must(e)
		require(s.Mode().IsRegular() && s.Size() > 0 && s.Size() == entry.Size && hash(p) == entry.SHA256, "artifact changed after build: %s", entry.Name)
		result = append(result, p)
	}
	require(len(result) > 0, "empty package artifact set")
	return inv, result
}

func rubyFFIInstallArgs(system, arch string) []string {
	args := []string{"install", "ffi", "-v", "1.17.2", "--no-document"}
	if system == "darwin" {
		// Apple's universal RubyGems platform accepts both arm64 and x86_64
		// gems. Restrict native FFI to the same CPU as this host's SDK library.
		cpu := map[string]string{"arm64": "arm64", "amd64": "x86_64"}[arch]
		require(cpu != "", "unsupported Darwin Ruby consumer architecture: %s", arch)
		args = append(args, "--platform", cpu+"-darwin")
	}
	return args
}

func checkPackage(language, out string) {
	var manifest inventory
	jsonRead(filepath.Join(out, "manifest.json"), &manifest)
	_, artifacts := verifiedFiles(out, manifest.Version, false)
	temp, cleanup := temporary("urnetwork-package-check-")
	defer cleanup()
	e := map[string]string{"URNETWORK_SDK_LIBRARY": "", "PYTHONPATH": ""}
	switch language {
	case "python":
		command(temp, e, env("PYTHON", "python3"), "-m", "venv", filepath.Join(temp, "venv"))
		python := filepath.Join(temp, "venv/bin/python")
		if runtime.GOOS == "windows" {
			python = filepath.Join(temp, "venv/Scripts/python.exe")
		}
		command(temp, e, python, "-m", "pip", "install", "--no-index", "--find-links", filepath.Join(out, "artifacts"), "--pre", "urnetwork-sdk")
		command(temp, e, python, path("packaging/smoke_python.py"))
	case "java":
		cp := filepath.Join(temp, "classpath")
		command(filepath.Join(out, "project"), e, "mvn", "-q", "dependency:build-classpath", "-Dmdep.outputFile="+cp)
		jar := filepath.Join(out, "artifacts", "urnetwork-sdk-"+manifest.Version+".jar")
		command(temp, e, "java", "-cp", jar+string(os.PathListSeparator)+strings.TrimSpace(string(read(cp))), path("packaging/Smoke.java"))
	case "csharp":
		d := requireDotnet()
		e = d.environment(e)
		e["NUGET_PACKAGES"] = filepath.Join(temp, "nuget-cache")
		copyFile(path("csharp/global.json"), filepath.Join(temp, "global.json"))
		command(temp, e, d.executable, "new", "console", "--framework", "net8.0", "--output", temp)
		command(temp, e, d.executable, "add", "package", "URnetwork.SDK", "--version", manifest.Version, "--source", filepath.Join(out, "artifacts"))
		textFile(filepath.Join(temp, "Program.cs"), `using URnetwork.SDK;
Console.WriteLine(Sdk.Version);
if (Raw.urnet_abi_version() != 1) throw new Exception("ABI");
var id = Sdk.TakeString(Raw.urnet_new_id());
if (id?.Length != 36) throw new Exception("string");
if (!Sdk.TakeString(Raw.urnet_new_network_space_key("héllo", "main"))!.Contains("héllo")) throw new Exception("UTF-8");
using var handle = new Handle(Raw.urnet_new_network_space_manager_no_storage());
Raw.urnet_network_space_manager_close(handle.Value);
`)
		// Keep compiler/build processes owned by the consumer check; no shared
		// server may survive its temporary project or require host-wide shutdown.
		command(temp, e, d.executable, "run", "--disable-build-servers", "--no-restore")
	case "ruby":
		e["GEM_HOME"] = filepath.Join(temp, "gems")
		e["GEM_PATH"] = e["GEM_HOME"]
		e["SDK_GEM_VERSION"] = languageVersion("ruby", manifest.Version)
		command(temp, e, "gem", rubyFFIInstallArgs(runtime.GOOS, runtime.GOARCH)...)
		for _, p := range artifacts {
			if strings.HasSuffix(p, ".gem") {
				command(temp, e, "gem", "install", "--local", "--ignore-dependencies", "--no-document", p)
			}
		}
		command(temp, e, "ruby", path("packaging/smoke_ruby.rb"))
	case "rust":
		if assets := os.Getenv("SDK_RUST_RELEASE_ASSETS"); assets != "" {
			e["SDK_RUST_NATIVE_CACHE"] = assets
		}
		archive := ""
		for _, p := range artifacts {
			if strings.HasSuffix(p, ".crate") {
				archive = p
				break
			}
		}
		require(archive != "", "missing crate")
		for name, b := range tarFiles(archive) {
			write(filepath.Join(temp, name), b)
		}
		source := filepath.Join(temp, "urnetwork-sdk-"+manifest.Version)
		require(exists(source), "unexpected crate root")
		consumer := filepath.Join(temp, "consumer")
		mkdir(filepath.Join(consumer, "src"))
		textFile(filepath.Join(consumer, "Cargo.toml"), fmt.Sprintf("[package]\nname=\"package-check\"\nversion=\"0.1.0\"\nedition=\"2021\"\n[dependencies]\nurnetwork-sdk={path=%q}\n", filepath.ToSlash(source)))
		textFile(filepath.Join(consumer, "src/main.rs"), `fn main() -> std::io::Result<()> {
    println!("{}", urnetwork_sdk::version()?);
    let raw=urnetwork_sdk::native()?;
    assert_eq!(unsafe{(raw.urnet_abi_version)()},1);
    let id=unsafe{urnetwork_sdk::take_string((raw.urnet_new_id)())}.unwrap();
    assert_eq!(id.len(),36);
    Ok(())
}
`)
		command(consumer, e, "cargo", "run", "--quiet")
	default:
		panic("unknown package check")
	}
	markChecked(out)
	fmt.Println("Packaged", language, "runtime passed")
}
