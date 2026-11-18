package main

import (
	"errors"
	"go/types"
	"path/filepath"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"golang.org/x/tools/go/packages"
)

func testingMobileViewPackage() *packages.Package {
	return &packages.Package{
		PkgPath: "github.com/urnetwork/sdk/v2026",
		Types:   types.NewPackage("github.com/urnetwork/sdk/v2026", "sdk"),
	}
}

func TestMobileViewLoaderReusesEachView(t *testing.T) {
	calls := 0
	loader := mobileViewPackageLoader{loadPackages: func(*packages.Config, ...string) ([]*packages.Package, error) {
		calls++
		return []*packages.Package{testingMobileViewPackage()}, nil
	}}
	views := []struct{ goos, tags string }{
		{"android", "sdk_mobile_bind"},
		{"ios", "sdk_mobile_bind"},
		{"ios", "sdk_mobile_bind,ios_extension"},
	}
	first := make([]*packages.Package, len(views))
	// Affinity, logout, accessor and alias checks each inspect the same three
	// views. Their assertions must not cause twelve independent source loads.
	for consumer := range 4 {
		for index, view := range views {
			loaded, err := loader.load(testingMobileViewConfig(view.goos, view.tags))
			if err != nil {
				t.Fatal(err)
			}
			if consumer == 0 {
				first[index] = loaded
			} else if loaded != first[index] {
				t.Errorf("consumer %d view %d did not reuse the loaded graph", consumer, index)
			}
		}
	}
	if calls != 3 {
		t.Fatalf("four consumers of three mobile views loaded %d times, want 3", calls)
	}
}

func TestMobileViewLoaderSeparatesConfigurations(t *testing.T) {
	base := testingMobileViewConfig("android", "sdk_mobile_bind")
	base.dir = t.TempDir()
	base.env = append(base.env, "MOBILE_VIEW_TEST=first", "MOBILE_VIEW_TEST=last")
	cases := []struct {
		name   string
		change func(*mobileViewLoadConfig)
	}{
		{"goos", func(config *mobileViewLoadConfig) { config.env = append(config.env, "GOOS=ios") }},
		{"tags", func(config *mobileViewLoadConfig) {
			config.buildFlags = []string{"-tags=sdk_mobile_bind,ios_extension"}
		}},
		{"environment", func(config *mobileViewLoadConfig) { config.env = append(config.env, "GOGC=75") }},
		{"environment_order", func(config *mobileViewLoadConfig) {
			last := len(config.env) - 1
			config.env[last-1], config.env[last] = config.env[last], config.env[last-1]
		}},
		{"directory", func(config *mobileViewLoadConfig) { config.dir = t.TempDir() }},
		{"mode", func(config *mobileViewLoadConfig) { config.mode |= packages.NeedFiles }},
		{"build_flags", func(config *mobileViewLoadConfig) { config.buildFlags = append(config.buildFlags, "-trimpath") }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			loader := mobileViewPackageLoader{loadPackages: func(*packages.Config, ...string) ([]*packages.Package, error) {
				calls++
				return []*packages.Package{testingMobileViewPackage()}, nil
			}}
			changed := base
			changed.env = slices.Clone(base.env)
			changed.buildFlags = slices.Clone(base.buildFlags)
			test.change(&changed)
			first, err := loader.load(base)
			if err != nil {
				t.Fatal(err)
			}
			second, err := loader.load(changed)
			if err != nil {
				t.Fatal(err)
			}
			again, err := loader.load(base)
			if err != nil {
				t.Fatal(err)
			}
			if first == second || again != first || calls != 2 {
				t.Fatalf("distinct configuration identity was lost: calls=%d, want two independent graphs", calls)
			}
		})
	}
}

func TestMobileViewLoaderUsesResolvedDirectory(t *testing.T) {
	calls := 0
	loader := mobileViewPackageLoader{loadPackages: func(*packages.Config, ...string) ([]*packages.Package, error) {
		calls++
		return []*packages.Package{testingMobileViewPackage()}, nil
	}}
	firstConfig := testingMobileViewConfig("ios", "sdk_mobile_bind")
	secondConfig := firstConfig
	var err error
	secondConfig.dir, err = filepath.Abs(firstConfig.dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := loader.load(firstConfig)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loader.load(secondConfig)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first != second {
		t.Fatalf("equivalent resolved source directory loaded %d times, want 1", calls)
	}
}

func TestMobileViewLoaderConcurrentRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const requests = 32
		var calls atomic.Int32
		release := make(chan struct{})
		loader := mobileViewPackageLoader{loadPackages: func(*packages.Config, ...string) ([]*packages.Package, error) {
			calls.Add(1)
			<-release
			return []*packages.Package{testingMobileViewPackage()}, nil
		}}
		config := testingMobileViewConfig("android", "sdk_mobile_bind")
		type result struct {
			pkg *packages.Package
			err error
		}
		results := make(chan result, requests)
		for range requests {
			go func() {
				pkg, err := loader.load(config)
				results <- result{pkg, err}
			}()
		}
		// Every caller must reach the blocked backend or the shared in-flight
		// result before the backend can complete. No scheduler timing is assumed.
		synctest.Wait()
		if calls.Load() != 1 {
			t.Errorf("blocked concurrent requests started %d loads, want 1", calls.Load())
		}
		close(release)
		var first *packages.Package
		for index := range requests {
			got := <-results
			if got.err != nil {
				t.Error(got.err)
			}
			if index == 0 {
				first = got.pkg
			} else if got.pkg != first {
				t.Error("concurrent requests did not share the successful graph")
			}
		}
		if calls.Load() != 1 {
			t.Fatalf("concurrent requests loaded %d times, want 1", calls.Load())
		}
	})
}

func TestMobileViewLoaderFailuresAreNotCached(t *testing.T) {
	cases := []struct {
		name   string
		loaded []*packages.Package
		err    error
	}{
		{"loader_error", nil, errors.New("source loader failed")},
		{"no_package", nil, nil},
		{"multiple_packages", []*packages.Package{testingMobileViewPackage(), testingMobileViewPackage()}, nil},
		{"nil_package", []*packages.Package{nil}, nil},
		{"package_errors", []*packages.Package{{Errors: []packages.Error{{Msg: "source type check failed"}}}}, nil},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			success := testingMobileViewPackage()
			loader := mobileViewPackageLoader{loadPackages: func(*packages.Config, ...string) ([]*packages.Package, error) {
				calls++
				if calls == 1 {
					return test.loaded, test.err
				}
				return []*packages.Package{success}, nil
			}}
			config := testingMobileViewConfig("ios", "sdk_mobile_bind")
			if loaded, err := loader.load(config); err == nil || loaded != nil {
				t.Fatal("failed load was accepted")
			}
			for range 2 {
				loaded, err := loader.load(config)
				if err != nil || loaded != success {
					t.Fatal("successful load did not recover from the failure")
				}
			}
			if calls != 2 {
				t.Fatalf("failed load recovery called loader %d times, want 2", calls)
			}
		})
	}
}

func TestMobileViewLoaderPreservesSourceLoadConfiguration(t *testing.T) {
	config := testingMobileViewConfig("ios", "sdk_mobile_bind,ios_extension")
	loader := mobileViewPackageLoader{loadPackages: func(actual *packages.Config, patterns ...string) ([]*packages.Package, error) {
		if actual.Mode != packages.NeedName|packages.NeedTypes|packages.NeedImports|packages.NeedDeps ||
			actual.Dir != "../.." ||
			!reflect.DeepEqual(actual.Env, config.env) ||
			!reflect.DeepEqual(actual.BuildFlags, []string{"-tags=sdk_mobile_bind,ios_extension"}) ||
			!reflect.DeepEqual(patterns, []string{"github.com/urnetwork/sdk/v2026"}) {
			t.Fatal("mobile source-load configuration changed")
		}
		wantTail := []string{
			"GOOS=ios", "GOARCH=arm64", "CGO_ENABLED=0", "GOEXPERIMENT=greenteagc",
			"GOPROXY=off", "GOSUMDB=off", "GOWORK=off",
		}
		if !reflect.DeepEqual(actual.Env[len(actual.Env)-len(wantTail):], wantTail) {
			t.Fatal("mobile source-load environment changed")
		}
		return []*packages.Package{testingMobileViewPackage()}, nil
	}}
	if _, err := loader.load(config); err != nil {
		t.Fatal(err)
	}
}
