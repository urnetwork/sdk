// SPDX-License-Identifier: MPL-2.0
package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

func packageMobile(language string) {
	v := packageVersion()
	switch language {
	case "android":
		aar := env("SDK_ANDROID_AAR", path("build/android/URnetworkSdk.aar"))
		sources := env("SDK_ANDROID_SOURCES", path("build/android/URnetworkSdk-sources.jar"))
		require(exists(aar) && exists(sources), "build the Android SDK first, or set SDK_ANDROID_AAR and SDK_ANDROID_SOURCES")
		out := path("java/dist/artifacts")
		mkdir(out)
		base := "urnetwork-sdk-android-" + v
		copyFile(aar, filepath.Join(out, base+".aar"))
		copyFile(sources, filepath.Join(out, base+"-sources.jar"))
		temp, cleanup := temporary("urnetwork-android-javadoc-")
		defer cleanup()
		sourcePaths := []string{}
		for name, b := range zipFiles(sources) {
			p := filepath.Join(temp, "src", name)
			write(p, b)
			if strings.HasSuffix(name, ".java") {
				sourcePaths = append(sourcePaths, strconv.Quote(p))
			}
		}
		write(filepath.Join(temp, "classes.jar"), zipFiles(aar)["classes.jar"])
		androidJar := os.Getenv("ANDROID_JAR")
		if androidJar == "" {
			home, e := os.UserHomeDir()
			must(e)
			jars := glob(filepath.Join(env("ANDROID_HOME", filepath.Join(home, "Library/Android/sdk")), "platforms/android-*/android.jar"))
			latest := -1
			for _, p := range jars {
				match := regexp.MustCompile(`android-(\d+)$`).FindStringSubmatch(filepath.Base(filepath.Dir(p)))
				if match == nil {
					continue
				}
				level, e := strconv.Atoi(match[1])
				must(e)
				if level > latest {
					latest = level
					androidJar = p
				}
			}
		}
		require(exists(androidJar), "Android platform android.jar is required for the Maven documentation artifact")
		textFile(filepath.Join(temp, "sources.txt"), strings.Join(sourcePaths, "\n")+"\n")
		command(temp, nil, "javadoc", "-quiet", "-Xdoclint:none", "-classpath", filepath.Join(temp, "classes.jar")+string(os.PathListSeparator)+androidJar,
			"-d", filepath.Join(temp, "docs"), "@"+filepath.Join(temp, "sources.txt"))
		zipTree(filepath.Join(temp, "docs"), filepath.Join(out, base+"-javadoc.jar"))
		validateJavadoc(filepath.Join(out, base+"-javadoc.jar"))
		textFile(filepath.Join(out, base+".pom"), fmt.Sprintf(`<project xmlns="http://maven.apache.org/POM/4.0.0">
<modelVersion>4.0.0</modelVersion><groupId>io.ur</groupId><artifactId>urnetwork-sdk-android</artifactId>
<version>%s</version><packaging>aar</packaging><name>URnetwork Android SDK</name>
<description>URnetwork gomobile Android binding</description><url>https://ur.io</url>
<licenses><license><name>MPL-2.0</name><url>https://mozilla.org/MPL/2.0/</url></license></licenses>
<scm><url>https://github.com/urnetwork/sdk</url></scm>
<developers><developer><id>urnetwork</id><name>URnetwork</name><email>support@ur.io</email></developer></developers>
</project>
`, v))
		manifestFile := path("java/dist/manifest.json")
		var inv inventory
		jsonRead(manifestFile, &inv)
		require(inv.Version == v, "Android and desktop JVM package versions differ")
		inv.Artifacts = artifactsIn(out)
		jsonWrite(manifestFile, inv)
	case "swift":
		artifact := env("SDK_XCFRAMEWORK_ZIP", path("build/apple/URnetworkSdk.xcframework.zip"))
		require(exists(artifact), "build the Apple SDK first, or set SDK_XCFRAMEWORK_ZIP")
		url := env("SDK_XCFRAMEWORK_URL", "https://github.com/urnetwork/build/releases/download/v"+v+"/URnetworkSdk-"+v+".xcframework.zip")
		require(strings.HasPrefix(url, "https://") && !strings.ContainsAny(url, "'\"\\\r\n"), "invalid immutable XCFramework HTTPS URL")
		out := path("swift/dist/package")
		must(os.RemoveAll(out))
		mkdir(out)
		digest := hash(artifact)
		textFile(filepath.Join(out, "Package.swift"), fmt.Sprintf(`// swift-tools-version: 5.9
import PackageDescription
let package = Package(
    name: "URnetworkSdk",
    platforms: [.iOS(.v16), .macOS("13.5")],
    products: [.library(name: "URnetworkSdk", targets: ["URnetworkSdk", "URnetworkSdkSupport"])],
    targets: [
        .binaryTarget(name: "URnetworkSdk", url: %q, checksum: %q),
        .target(name: "URnetworkSdkSupport", path: ".", sources: ["URnetworkSdkSupport.swift"], linkerSettings: [.linkedLibrary("resolv")])
    ]
)
`, url, digest))
		textFile(filepath.Join(out, "URnetworkSdkSupport.swift"), "// Propagates the static Go runtime's system resolver link dependency.\npublic enum URnetworkSdkSupport {}\n")
		textFile(filepath.Join(out, "URnetworkSdk.podspec"), fmt.Sprintf(`Pod::Spec.new do |s|
  s.name = 'URnetworkSdk'
  s.version = '%s'
  s.summary = 'URnetwork userspace networking SDK'
  s.homepage = 'https://ur.io'
  s.license = { :type => 'MPL-2.0' }
  s.author = { 'URnetwork' => 'support@ur.io' }
  s.source = { :http => '%s', :sha256 => '%s' }
  s.ios.deployment_target = '16.0'
  s.osx.deployment_target = '13.5'
  s.vendored_frameworks = 'URnetworkSdk.xcframework'
  s.libraries = 'resolv'
end
`, v, url, digest))
		jsonWrite(filepath.Join(out, "URnetworkSdk.json"), map[string]string{v: url})
		copyFile(path("LICENSE"), filepath.Join(out, "LICENSE"))
		copyFile(path("swift/README.md"), filepath.Join(out, "README.md"))
		artifacts := path("swift/dist/artifacts")
		must(os.RemoveAll(artifacts))
		copyTree(out, artifacts, nil)
		jsonWrite(path("swift/dist/manifest.json"), inventory{Version: v, XCFramework: &remoteAsset{url, digest}, Artifacts: artifactsIn(artifacts)})
	default:
		panic("unknown mobile package")
	}
}
func checkSwift() {
	out := path("swift/dist")
	inv, artifacts := verifiedFiles(out, packageVersion(), false)
	require(inv.XCFramework != nil, "missing XCFramework manifest")
	artifact := env("SDK_XCFRAMEWORK_ZIP", path("build/apple/URnetworkSdk.xcframework.zip"))
	require(hash(artifact) == inv.XCFramework.SHA256, "XCFramework changed after packaging")
	validateXCFramework(artifact)
	if runtime.GOOS == "darwin" {
		// Exercise the packaged product, including its resolver link dependency,
		// using the exact archive before its immutable release URL is published.
		temp, cleanup := temporary("urnetwork-swift-check-")
		defer cleanup()
		pkg := filepath.Join(temp, "package")
		for _, artifact := range artifacts {
			copyFile(artifact, filepath.Join(pkg, filepath.Base(artifact)))
		}
		manifest := filepath.Join(pkg, "Package.swift")
		binary := regexp.MustCompile(`url: "[^"]+", checksum: "[^"]+"`)
		textFile(manifest, binary.ReplaceAllString(string(read(manifest)), `path: "URnetworkSdk.xcframework"`))
		command(temp, nil, "ditto", "-x", "-k", artifact, pkg)
		consumer := filepath.Join(temp, "consumer")
		textFile(filepath.Join(consumer, "Package.swift"), `// swift-tools-version: 5.9
import PackageDescription
let package = Package(name: "Smoke", platforms: [.macOS("13.5")],
    dependencies: [.package(path: "../package")],
    targets: [.executableTarget(name: "Smoke", dependencies: [.product(name: "URnetworkSdk", package: "package")])])
`)
		textFile(filepath.Join(consumer, "Sources/Smoke/main.swift"), "import URnetworkSdk\nprint(Sdk.version())\nlet _: SdkSocket? = nil\n")
		command(consumer, nil, "swift", "run", "Smoke")
	}
	markChecked(out)
}

func validateXCFramework(artifact string) {
	z, e := zip.OpenReader(artifact)
	must(e)
	defer z.Close()
	headers := 0
	for _, f := range z.File {
		require(filepath.IsLocal(f.Name), "unsafe archive path: %s", f.Name)
		if f.FileInfo().IsDir() {
			continue
		}
		isLink := f.Mode()&os.ModeSymlink != 0
		isHeader := strings.HasSuffix(f.Name, "/Headers/Sdk.objc.h")
		if !isLink && !isHeader {
			continue
		}
		r, e := f.Open()
		must(e)
		b, e := io.ReadAll(r)
		must(e)
		must(r.Close())
		if isLink {
			// Versioned macOS frameworks contain standard Headers, Resources,
			// Modules and Versions/Current links. Keep every target in the bundle.
			target := filepath.Clean(filepath.Join(filepath.Dir(f.Name), string(b)))
			require(!filepath.IsAbs(string(b)) && filepath.IsLocal(target) && strings.HasPrefix(target, "URnetworkSdk.xcframework/"), "unsafe framework symlink: %s", f.Name)
		} else if isHeader {
			headers++
			require(strings.Contains(string(b), "openSocket:"), "XCFramework lacks the SDK Socket API")
		}
	}
	require(headers > 0, "XCFramework lacks SDK headers")
}

// Ensure packaged Java documentation really contains generated API pages.
func validateJavadoc(jar string) {
	found := false
	for name := range zipFiles(jar) {
		if strings.HasSuffix(name, "/Sdk.html") {
			found = true
		}
	}
	require(found, "documentation artifact lacks the SDK API")
}
