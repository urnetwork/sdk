package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"golang.org/x/tools/go/packages"
)

// These are all the configuration fields used by the source-based mobile view
// checks. Keeping the configuration explicit also makes its identity testable.
type mobileViewLoadConfig struct {
	mode       packages.LoadMode
	dir        string
	env        []string
	buildFlags []string
}

func testingMobileViewConfig(goos string, tags string) mobileViewLoadConfig {
	return mobileViewLoadConfig{
		mode: packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps,
		dir:  "../..",
		// The sdk package has no cgo, so its declarations do not depend on cgo.
		env: append(
			os.Environ(),
			"GOOS="+goos,
			"GOARCH=arm64",
			"CGO_ENABLED=0",
			"GOEXPERIMENT=greenteagc",
			"GOPROXY=off",
			"GOSUMDB=off",
			"GOWORK=off",
		),
		buildFlags: []string{"-tags=" + tags},
	}
}

type mobileViewPackageLoader struct {
	loadPackages func(*packages.Config, ...string) ([]*packages.Package, error)
	mutex        sync.Mutex
	views        map[string]*mobileViewLoad
}

type mobileViewLoad struct {
	done chan struct{}
	pkg  *packages.Package
	err  error
}

func (loader *mobileViewPackageLoader) load(config mobileViewLoadConfig) (*packages.Package, error) {
	directory, err := filepath.Abs(config.dir)
	if err != nil {
		return nil, err
	}
	// Environment and flag order matter: duplicate variables and flags use
	// the last value. Include the complete configuration, not just the tags.
	identity, err := json.Marshal(struct {
		Mode       packages.LoadMode
		Directory  string
		Env        []string
		BuildFlags []string
	}{config.mode, directory, config.env, config.buildFlags})
	if err != nil {
		return nil, err
	}
	key := string(identity)
	loader.mutex.Lock()
	if previous := loader.views[key]; previous != nil {
		loader.mutex.Unlock()
		<-previous.done
		return previous.pkg, previous.err
	}
	if loader.views == nil {
		loader.views = map[string]*mobileViewLoad{}
	}
	current := &mobileViewLoad{done: make(chan struct{})}
	loader.views[key] = current
	loader.mutex.Unlock()

	// All consumers inspect the graph without changing it. A successful
	// source load can therefore serve every check of this exact mobile view.
	// Keep the mutex free while loading so distinct configurations and
	// callers waiting for this result do not block each other's bookkeeping.
	current.pkg, current.err = loader.loadSource(config)
	loader.mutex.Lock()
	if current.err != nil {
		// Existing waiters receive this attempt's error. A later request must
		// perform a fresh load instead of retaining a transient failure.
		delete(loader.views, key)
	}
	close(current.done)
	loader.mutex.Unlock()
	return current.pkg, current.err
}

func (loader *mobileViewPackageLoader) loadSource(config mobileViewLoadConfig) (*packages.Package, error) {
	loaded, err := loader.loadPackages(&packages.Config{
		Mode:       config.mode,
		Dir:        config.dir,
		Env:        slices.Clone(config.env),
		BuildFlags: slices.Clone(config.buildFlags),
	}, "github.com/urnetwork/sdk")
	if err != nil {
		return nil, err
	}
	if len(loaded) != 1 || loaded[0] == nil {
		return nil, fmt.Errorf("loaded %d packages, want the sdk", len(loaded))
	}
	if len(loaded[0].Errors) != 0 {
		return nil, fmt.Errorf("package errors: %v", loaded[0].Errors)
	}
	return loaded[0], nil
}

var testingMobileViews = mobileViewPackageLoader{loadPackages: packages.Load}

// Type-checks one gomobile view of the sdk from source. Loading the
// dependencies from source too keeps the check independent of the export data
// format of the toolchain that runs it.
func testingLoadMobileView(t *testing.T, goos string, tags string) *packages.Package {
	t.Helper()
	loaded, err := testingMobileViews.load(testingMobileViewConfig(goos, tags))
	if err != nil {
		t.Fatalf("load the %s %s view: %v", goos, tags, err)
	}
	return loaded
}
