//go:build js

package sdk

// The js build has no gossip node at all: go-libp2p is not part of the wasm
// binary (H), so the feed role is the only role this build can have and the
// platform rule says so before any memory policy is consulted (D5).
const extenderFeedOnlyBuild = true
