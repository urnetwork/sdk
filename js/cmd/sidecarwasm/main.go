//go:build js && wasm && sidecar_http

// The page Worker's main, sidecar.wasm (§18.10, D-SDK-19): the sidecar on the browser's leg, which the
// key origin https://sidecar.ur.io/v1/ runs as a Worker for ur.io's pages (§18.8).
//
// It links the sidecar root and protocol and nothing of sdk or connect: a connect-only js/wasm main
// is already 55.0 MB unstripped, since Go links every imported package's init, connect's IP stack
// included. The module requires both, and sdk/dependency_boundary_test.go's leg for this main fails
// on any connect, gVisor, sdk, net/rpc or template package in its closure.
//
// Build it unstripped with make build_sidecar_wasm:
//
//	CGO_ENABLED=0 GOOS=js GOARCH=wasm go build -tags sidecar_http -trimpath -o wasm/sidecar.wasm ./cmd/sidecarwasm
//
// Until the Worker's host and operations land (SDK-M4-04) this boots and publishes its version and
// no more, so its own size says nothing yet about the leg: make size's sidecar_http row, which keeps
// the root's whole exported surface alive, is the figure to hold against §18.10's goal.
package main

import (
	"syscall/js"

	"github.com/urnetwork/sidecar"
	_ "github.com/urnetwork/sidecar/protocol"
)

// The global the loader reads once the program has started: the release version, empty in a
// development build. Set before main first blocks, so it is there when the loader's go.run returns.
const sidecarWasmGlobalName = "urnetworkSidecar"

func main() {
	js.Global().Set(sidecarWasmGlobalName, js.ValueOf(map[string]any{
		"version": sidecar.Version,
	}))
	// the Worker lives as long as its page: nothing here returns, so later callbacks find Go running
	select {}
}
