use sha2::{Digest, Sha256};
use std::{env, fs, io::Read, path::PathBuf};

fn main() {
    println!("cargo:rerun-if-changed=native/manifest.json");
    println!("cargo:rerun-if-env-changed=SDK_RUST_NATIVE_CACHE");
    let manifest = PathBuf::from("native/manifest.json");
    let output = PathBuf::from(env::var_os("OUT_DIR").unwrap());
    if !manifest.exists() {
        // A source checkout can use URNETWORK_SDK_LIBRARY at runtime.
        fs::write(output.join("runtime.rs"), "pub fn packaged() -> Option<(&'static [u8], &'static str, &'static str)> { None }\n").unwrap();
        return;
    }
    let manifest: serde_json::Value = serde_json::from_slice(&fs::read(manifest).unwrap()).unwrap();
    // The local embedded package path needs no download or generated code.
    if manifest["libraries"][0].get("url").is_none() { return; }
    let os = match env::var("CARGO_CFG_TARGET_OS").unwrap().as_str() {
        "macos" => "darwin", "linux" => "linux", "windows" => "windows",
        _ => panic!("URnetwork release has no native runtime for this OS"),
    };
    let arch = match env::var("CARGO_CFG_TARGET_ARCH").unwrap().as_str() {
        "aarch64" => "arm64", "x86_64" => "amd64",
        _ => panic!("URnetwork release has no native runtime for this architecture"),
    };
    if os == "linux" && env::var("CARGO_CFG_TARGET_ENV").unwrap_or_default() != "gnu" {
        panic!("URnetwork release requires glibc; musl is not supported");
    }
    let platform = format!("{os}-{arch}");
    let entry = manifest["libraries"].as_array().unwrap().iter()
        .find(|e| e["platform"] == platform).expect("target is absent from this release");
    let archive = entry["archive"].as_str().unwrap();
    let url = entry["url"].as_str().unwrap();
    assert!(url.starts_with("https://github.com/urnetwork/build/releases/download/v"));
    let bytes = if let Some(cache) = env::var_os("SDK_RUST_NATIVE_CACHE") {
        fs::read(PathBuf::from(cache).join(archive)).expect("pinned runtime is missing from SDK_RUST_NATIVE_CACHE")
    } else {
        let mut bytes = Vec::new();
        ureq::get(url).call().expect("download pinned URnetwork runtime")
            .into_body().into_reader().take(128 * 1024 * 1024)
            .read_to_end(&mut bytes).expect("read pinned runtime");
        bytes
    };
    assert_eq!(format!("{:x}", Sha256::digest(&bytes)), entry["archive_sha256"].as_str().unwrap(),
        "URnetwork runtime archive checksum mismatch");
    fs::write(output.join("runtime.gz"), bytes).unwrap();
    let code = format!(
        "pub fn packaged() -> Option<(&'static [u8], &'static str, &'static str)> {{ Some((include_bytes!(concat!(env!(\"OUT_DIR\"), \"/runtime.gz\")), {:?}, {:?})) }}\n",
        entry["sha256"].as_str().unwrap(), entry["filename"].as_str().unwrap());
    fs::write(output.join("runtime.rs"), code).unwrap();
}
