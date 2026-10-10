//go:build !js

package sdk

// Every native build can carry a gossip node, so the role is decided by the
// memory policy and the persisted mode alone (D5).
const extenderFeedOnlyBuild = false
