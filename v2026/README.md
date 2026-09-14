# Style

This package is written to compile with gomobile to supported platforms. 

Some exported signatures are slightly different than idiomatic Go, since they are meant to be called from linked languages.

`sdk.go` and `gomobile.go` documents most of the significant signature elements.

`errors.go` is an implementation of better error logging proposal X. We follow this error pattern in all exported types and goroutine roots to make error and crash logging easier on linked platforms.


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

# Xcode setup

If you see this error, run the following command.

```
# gomobile: -target="ios/arm64,iossimulator/arm64,macos/arm64,macos/amd64" requires Xcode
sudo xcode-select -s /Applications/Xcode.app/Contents/Developer/
```

# Subprotocols

An application can speak its own protocol between two devices' clients through the subprotocol frame (`connect/SUBPROTOCOL.md`, §8.9 for the sdk boundary). The device exposes the raw-bytes surface only; the application owns its codec.

- `DeviceLocal.EnableSubprotocol(id, listener) (Sub, error)` enables a 16-bit id for a listener and returns the `Sub` that removes it. Ids below `SubprotocolReservedLimit` (1024) belong to the network and are refused. Any number of listeners may share an id; the client registration goes with the last one. `DisableSubprotocol(id)` removes every listener of the id at once, and `EnabledSubprotocols()` lists the enabled ids.
- `SubprotocolListener.SubprotocolMessage(id, sourceClientId, messageBytes)` is called inline on the client's receive path with the listener's own copy of the bytes, so it must not block.
- `SendSubprotocolBytes(id, destinationClientId, messageBytes) bool` sends one message, fire and forget; the bytes are copied once into the frame. False means it was not enqueued (no client, invalid id, control destination, or a full queue).
- `QuerySubprotocols(destinationClientId, timeoutMillis, callback)` asks a peer which ids it supports and answers on the callback from a worker; `ok` is false on timeout, and a peer older than the frame never answers.
- `SubprotocolStats()` reads the client's counters.

The registrations live on the device: they are applied to the device's own client when the device starts and re-applied when that client is replaced.
