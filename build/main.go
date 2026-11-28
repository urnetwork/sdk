package main

// The packages the mobile bindings bind, so this module's requirements carry
// everything they build. The hotspot proxy is bound for android only.
import (
	_ "github.com/urnetwork/sdk"
	_ "github.com/urnetwork/sdk/hotspot"
)
