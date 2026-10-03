package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestXCFrameworkVersionedLinks(t *testing.T) {
	for _, target := range []string{"Versions/Current/Headers", "../../../../outside", "/tmp/outside"} {
		t.Run(target, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "sdk.zip")
			f, err := os.Create(archive)
			must(err)
			z := zip.NewWriter(f)
			root := "URnetworkSdk.xcframework/macos-arm64/URnetworkSdk.framework/"
			w, err := z.Create(root + "Versions/A/Headers/Sdk.objc.h")
			must(err)
			_, err = w.Write([]byte("- (SdkSocket*)openSocket:(NSString*)network;"))
			must(err)
			h := &zip.FileHeader{Name: root + "Headers"}
			h.SetMode(os.ModeSymlink | 0755)
			w, err = z.CreateHeader(h)
			must(err)
			_, err = w.Write([]byte(target))
			must(err)
			must(z.Close())
			must(f.Close())
			valid := target == "Versions/Current/Headers"
			defer func() {
				if rejected := recover() != nil; rejected == valid {
					t.Fatalf("target %q: rejected=%v, want %v", target, rejected, !valid)
				}
			}()
			validateXCFramework(archive)
		})
	}
}
