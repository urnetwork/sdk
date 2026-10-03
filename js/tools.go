//go:build tools

// Build-time tool dependencies of the js module, kept required under
// `go mod tidy` (which never reads //go:build ignore files such as
// gen_openapi.go). Never compiled into the wasm.
package main

import (
	_ "gopkg.in/yaml.v3" // gen_openapi.go reads the OpenAPI spec
)
