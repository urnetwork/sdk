package sdk

import (
	"context"

	"github.com/urnetwork/connect/v2026"
)

// extender_seed_mirror_pass.go — the mirror part of a seed pass: connect's
// bootstrap pass over the space's mirrors (EXTENDER-UPDATE2.md §2.3).
//
// connect runs the pass: a shared budget of attempts, dials and bytes, one
// timeout per attempt and a deadline for the whole pass, every mirror on its
// own attempts so a stalled one holds up nothing but itself, redirects only to
// configured destinations, and nothing used until it verifies under the root
// keys and network hosts the directory already holds. What it accepts it
// commits through the space's acceptor and applies to the directory.
//
// The space supplies how a mirror is dialed and verified. A mirror is dialed
// the way the space dials everything direct -- its connect settings' dialer,
// with its proxy and its resolver, so a platform that protects or routes the
// app's own sockets does so for the mirrors too. A mirror with an address is
// dialed there and never resolved; one without is resolved by that dialer's
// resolver, the system's unless an embedder configured another. Its
// certificate is verified for the url's host against the roots of the space's
// tls settings, the roots the control route is verified against: by default
// connect's pinned Let's Encrypt set and any roots the embedder added. Nothing
// here reads or changes a resolver setting of the user's.

func runExtenderSeedMirrorPass(
	ctx context.Context,
	space *NetworkSpace,
	settings *extenderSeedSettings,
	request *extenderSeedMirrorRequest,
) *extenderSeedMirrorResult {
	bootstrapSettings := connect.DefaultExtenderSeedBootstrapSettings()
	for _, mirror := range request.mirrors {
		bootstrapSettings.Mirrors = append(bootstrapSettings.Mirrors, &connect.ExtenderSeedMirror{
			Url:     mirror.url,
			Address: mirror.addressText(),
		})
	}
	// the seed pass's deadline bounds connect's own
	bootstrapSettings.Timeout = settings.PassTimeout
	if 0 < settings.MirrorAttemptTimeout {
		bootstrapSettings.AttemptTimeout = settings.MirrorAttemptTimeout
	}
	connectSettings := &space.clientStrategySettings.ConnectSettings
	bootstrapSettings.DialContext = connectSettings.DialContext
	if connectSettings.TlsConfig != nil {
		bootstrapSettings.RootCAs = connectSettings.TlsConfig.RootCAs
	}
	bootstrapSettings.Now = request.now

	bootstrapResult, err := connect.ExtenderSeedBootstrap(ctx, request.directory, request.acceptor, bootstrapSettings)
	if bootstrapResult == nil {
		bootstrapResult = &connect.ExtenderSeedBootstrapResult{}
	}
	if log := space.logger().V(1); log.Enabled() {
		// the outcome codes are a fixed set and name no mirror
		outcomes := map[string]int{}
		for _, mirrorResult := range bootstrapResult.Mirrors {
			outcomes[mirrorResult.Outcome] += 1
		}
		log.Infof(
			"[extender]seed mirrors: %v attempts=%d dials=%d bytes=%d err=%s\n",
			outcomes,
			bootstrapResult.AttemptCount,
			bootstrapResult.DialCount,
			bootstrapResult.ByteCount,
			connect.ExtenderSeedErrorCode(err),
		)
	}
	result := &extenderSeedMirrorResult{
		bundle: bootstrapResult.Bundle,
		apply:  bootstrapResult.Apply,
		pass:   bootstrapResult,
	}
	if err != nil {
		result.code = connect.ExtenderSeedErrorCode(err)
		if result.code == "" {
			result.code = connect.ExtenderSeedCodeFetch
		}
	}
	return result
}
