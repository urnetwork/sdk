// SPDX-License-Identifier: MPL-2.0
// Command packaging builds, checks, and publishes SDK language packages.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

var root = func() string { _, f, _, _ := runtime.Caller(0); return filepath.Dir(filepath.Dir(f)) }()
var versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)
var files = map[string]string{"darwin": "libURnetworkSdk.dylib", "linux": "libURnetworkSdk.so", "windows": "URnetworkSdk.dll"}
var nativePaths = map[string]string{"python": "src/urnetwork/native", "ruby": "lib/urnetwork/native", "java": "src/main/resources/native", "rust": "native", "csharp": "runtimes"}

type library struct {
	Platform      string `json:"platform"`
	Path          string `json:"path,omitempty"`
	SHA256        string `json:"sha256"`
	MinimumOS     string `json:"minimum_os"`
	Filename      string `json:"filename,omitempty"`
	Archive       string `json:"archive,omitempty"`
	ArchiveSHA256 string `json:"archive_sha256,omitempty"`
	URL           string `json:"url,omitempty"`
}
type nativeManifest struct {
	ABI            int       `json:"abi"`
	Version        string    `json:"version"`
	SourceRevision string    `json:"source_revision,omitempty"`
	Libraries      []library `json:"libraries"`
}
type artifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type remoteAsset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}
type inventory struct {
	Version     string          `json:"version"`
	Native      *nativeManifest `json:"native,omitempty"`
	XCFramework *remoteAsset    `json:"xcframework,omitempty"`
	Artifacts   []artifact      `json:"artifacts"`
}
type checked struct {
	ManifestSHA256 string `json:"manifest_sha256"`
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func require(ok bool, format string, args ...any) {
	if !ok {
		panic(fmt.Errorf(format, args...))
	}
}
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func packageVersion() string {
	v := env("SDK_PACKAGE_VERSION", env("EXTERNAL_WARP_VERSION", "0.0.1-dev.0"))
	require(versionPattern.MatchString(v), "invalid SDK_PACKAGE_VERSION")
	return v
}
func path(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }
func exists(p string) bool        { _, e := os.Stat(p); return e == nil }
func read(p string) []byte        { b, e := os.ReadFile(p); must(e); return b }
func mkdir(p string)              { must(os.MkdirAll(p, 0755)) }
func write(p string, b []byte) {
	if old, e := os.ReadFile(p); e == nil && slices.Equal(old, b) {
		return
	}
	mkdir(filepath.Dir(p))
	f, e := os.CreateTemp(filepath.Dir(p), ".sdk-write-")
	must(e)
	tmp := f.Name()
	defer os.Remove(tmp)
	_, e = f.Write(b)
	must(e)
	must(f.Close())
	must(os.Chmod(tmp, 0644))
	must(os.Rename(tmp, p))
}
func textFile(p, s string)     { write(p, []byte(s)) }
func jsonRead(p string, v any) { must(json.Unmarshal(read(p), v)) }
func jsonWrite(p string, v any) {
	b, e := json.MarshalIndent(v, "", "  ")
	must(e)
	write(p, append(b, '\n'))
}
func hash(p string) string {
	f, e := os.Open(p)
	must(e)
	defer f.Close()
	h := sha256.New()
	_, e = io.Copy(h, f)
	must(e)
	return hex.EncodeToString(h.Sum(nil))
}
func copyFile(src, dst string) {
	mkdir(filepath.Dir(dst))
	in, e := os.Open(src)
	must(e)
	defer in.Close()
	out, e := os.Create(dst)
	must(e)
	_, e = io.Copy(out, in)
	must(e)
	must(out.Close())
}
func copyTree(src, dst string, skip map[string]bool) {
	must(filepath.WalkDir(src, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p != src && (skip[d.Name()] || strings.HasSuffix(d.Name(), ".egg-info")) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, e := filepath.Rel(src, p)
		if e != nil {
			return e
		}
		require(d.Type()&os.ModeSymlink == 0, "unexpected symlink in package: %s", p)
		if d.IsDir() {
			mkdir(filepath.Join(dst, rel))
		} else {
			copyFile(p, filepath.Join(dst, rel))
		}
		return nil
	}))
}
func glob(pattern string) []string { p, e := filepath.Glob(pattern); must(e); return p }
func temporary(prefix string) (string, func()) {
	p, e := os.MkdirTemp("", prefix)
	must(e)
	return p, func() { _ = os.RemoveAll(p) }
}
func withEnv(overrides map[string]string) []string {
	out := []string{}
	for _, entry := range os.Environ() {
		k, _, _ := strings.Cut(entry, "=")
		if _, ok := overrides[k]; !ok {
			out = append(out, entry)
		}
	}
	for k, v := range overrides {
		if v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// Run tools directly: no shell interpolation, secret arguments, or interactive stdin.
func command(dir string, overrides map[string]string, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = withEnv(overrides)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	must(cmd.Run())
}
func output(dir string, overrides map[string]string, name string, args ...string) []byte {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = withEnv(overrides)
	cmd.Stderr = os.Stderr
	b, e := cmd.Output()
	must(e)
	return b
}
func gzipFile(src, dst string) {
	mkdir(filepath.Dir(dst))
	in, e := os.Open(src)
	must(e)
	defer in.Close()
	f, e := os.Create(dst)
	must(e)
	g := gzip.NewWriter(f)
	_, e = io.Copy(g, in)
	must(e)
	must(g.Close())
	must(f.Close())
}
func zipTree(src, dst string) {
	mkdir(filepath.Dir(dst))
	f, e := os.Create(dst)
	must(e)
	z := zip.NewWriter(f)
	must(filepath.WalkDir(src, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(src, p)
		if e != nil {
			return e
		}
		w, e := z.Create(filepath.ToSlash(rel))
		if e != nil {
			return e
		}
		_, e = w.Write(read(p))
		return e
	}))
	must(z.Close())
	must(f.Close())
}
func zipFiles(p string) map[string][]byte {
	z, e := zip.OpenReader(p)
	must(e)
	defer z.Close()
	out := map[string][]byte{}
	for _, f := range z.File {
		if f.FileInfo().IsDir() {
			continue
		}
		require(filepath.IsLocal(f.Name) && f.Mode()&os.ModeSymlink == 0, "unsafe archive path: %s", f.Name)
		r, e := f.Open()
		must(e)
		b, e := io.ReadAll(r)
		must(e)
		must(r.Close())
		out[f.Name] = b
	}
	return out
}
func tarFiles(p string) map[string][]byte {
	f, e := os.Open(p)
	must(e)
	defer f.Close()
	g, e := gzip.NewReader(f)
	must(e)
	defer g.Close()
	t := tar.NewReader(g)
	out := map[string][]byte{}
	for {
		h, e := t.Next()
		if e == io.EOF {
			break
		}
		must(e)
		require(filepath.IsLocal(h.Name), "unsafe archive path: %s", h.Name)
		if h.Typeflag == tar.TypeDir {
			continue
		}
		require(h.Typeflag == tar.TypeReg, "unexpected archive entry: %s", h.Name)
		b, e := io.ReadAll(t)
		must(e)
		out[h.Name] = b
	}
	return out
}
func artifactsIn(dir string) []artifact {
	entries, e := os.ReadDir(dir)
	must(e)
	result := []artifact{}
	for _, entry := range entries {
		require(!entry.IsDir() && entry.Type()&os.ModeSymlink == 0, "invalid package artifact")
		p := filepath.Join(dir, entry.Name())
		stat, e := os.Stat(p)
		must(e)
		require(stat.Size() > 0, "empty package artifact: %s", p)
		result = append(result, artifact{entry.Name(), hash(p), stat.Size()})
	}
	return result
}
func markChecked(dir string) {
	jsonWrite(filepath.Join(dir, "checked.json"), checked{hash(filepath.Join(dir, "manifest.json"))})
}
func packageOut(language string) string {
	p, e := filepath.Abs(env("SDK_PACKAGE_OUT", path(language, "dist")))
	must(e)
	return p
}

func main() {
	if os.Getenv("SDK_GIT_ASKPASS") == "1" {
		require(len(os.Args) == 2, "invalid Git credential prompt")
		if strings.Contains(os.Args[1], "Username") {
			fmt.Println("x-access-token")
		} else {
			name := env("SDK_GIT_TOKEN_VARIABLE", "SDK_SWIFT_GIT_TOKEN")
			require(name == "SDK_SWIFT_GIT_TOKEN" || name == "SDK_VCPKG_GIT_TOKEN", "invalid Git token variable")
			fmt.Println(os.Getenv(name))
		}
		return
	}
	defer func() {
		if e := recover(); e != nil {
			fmt.Fprintln(os.Stderr, "SDK packaging:", e)
			os.Exit(1)
		}
	}()
	require(len(os.Args) >= 2, "usage: packaging generate | native/package/check LANGUAGE | init/check-tools csharp | npm package/check | mobile android/swift | publish REGISTRY | release PHASE")
	action := os.Args[1]
	arg := ""
	if len(os.Args) > 2 {
		arg = os.Args[2]
	}
	switch action {
	case "init":
		initTools(arg)
	case "check-tools":
		checkTools(arg)
	case "credential":
		if hasCredential(arg) {
			fmt.Println("yes")
		} else {
			fmt.Println("no")
		}
	case "generate":
		generateBindings()
	case "native":
		stageNative(arg, path(arg), loadNative())
	case "c":
		if arg == "package" {
			packageCNative()
		} else {
			require(arg == "check", "unknown C package action")
			checkCNative()
		}
	case "package":
		if arg == "csharp" {
			requireDotnet() // Fail before compiling the native library.
		}
		buildPackage(arg, loadNative(), packageOut(arg))
	case "check":
		checkPackage(arg, packageOut(arg))
	case "npm":
		if arg == "package" {
			packageNpm()
		} else {
			require(arg == "check", "unknown npm action")
			checkNpm()
		}
	case "mobile":
		packageMobile(arg)
	case "publish":
		dir := ""
		if len(os.Args) > 3 {
			dir = os.Args[3]
		}
		version := packageVersion()
		if arg == "npm" {
			version = npmPackageVersion()
		}
		publishRegistry(arg, dir, version)
	case "release":
		releaseStage(arg)
	default:
		panic("unknown packaging action")
	}
}
