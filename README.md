# Client development guidelines

This is the same concurrency guidelines as the `connect` package.

- Non-view objects should be written to be concurrently accessed, except:
  -- Read-only data objects
  -- View object (e.g. view controllers) that assume a single main thread access
  -- Any object that assumes a single thread ownership, which must be clearly documented

## When changing your version of Go

```
$BRINGYOUR_HOME/bringyour/clientgo install golang.org/x/mobile/cmd/gomobile@latest
$BRINGYOUR_HOME/bringyour/clientgo gomobile init
```

# Debugging and common issues

## go.mod empty

If you're seeing an error similar to `go: error reading go.mod: missing module declaration`, then check the project modules with the command `go list -m -json all`. Likely there is some module that can't be resolved correctly.

# Go setup

Due to a bug in recent versions of Go wrt gomobile, we need to use a patched version of go to build the sdk. See https://github.com/golang/go/issues/68760

```
git clone https://go.googlesource.com/go
cd go
git checkout release-branch.go1.23
git revert 3560cf0afb3c29300a6c88ccd98256949ca7a6f6
cd src
./make.bash
cd ../..
sudo mv /usr/local/go /usr/local/go.bak
sudo cp -r go /usr/local/go
# now `which go` should point to the patched go
```

