//go:build !ios_extension && !js

// The tls component on sdk hosts, and an app's tls/ listens (§13.17's app listeners, §18.4, §18.5; TLS-M8-01; R-TLS-4,
// D-TLS-5, D-TLS-7, X1).
//
// The component is the core's (sidecar/tls_component.go): the device's certificate manager on its own P-256 key, which
// the core seals in the network's keychain under U, the key the app's SidecarKeyStore wraps (§5.9), so the key never
// leaves the device and the component's store holds certificates alone. Orders go through certs' CSR proxy under a
// service token for aud certs (sidecar_host.go names its url beside the other services'), so no ACME client links
// into an app (D-TLS-7), and neither does miekg/dns: the tests hold sdk's mobile legs to both.
//
// The host class decides D-TLS-8's CT watch, which runs only on a host that stays up. ios and android run the sidecar
// role only while the app is open (owner decision 197), so their component runs no watch and leaves it to the
// network's other listening devices; a desktop class's runs it while it listens.
//
// SidecarSockets.Listen on a tls/ name is served here: the core's ListenTls registers the name through §5.4's gate and
// runs §13.7's front on it, so the visitor's TLS ends on this device's key and Accept answers a visitor's connection
// only after its audience check (FI-11), as plain HTTP/1.1 requests for the app's own server, the visitor's Host kept.
// Listen names no app mode, so an app's registration takes §9.5's default, http; raw is a desktop publish's.
//
// Listening follows §18.4's table: in the foreground on iOS and Android alike, the sidecar role running only while the
// app is open (owner decision 197, which supersedes the plan's Android-with-the-VPN). The OS suspending the app stops
// its listeners with the role, and their registrations lapse by the sockets service's 45 s lease, never by a timer
// here or in the core: nothing closes or unlistens on time. On resume the core's registrar sends its whole set again,
// which registers each name anew, and the listener's Accept goes on as before. Until SDK-M10-02 reports the foreground,
// a Listen in an OS-granted window or a wake is not refused here.
//
// The component needs the core's identity: before enrollment, and at a restart until the keychain opens, its factory
// waits (errSidecarComponentNotEnrolled), and the Sidecar builds it once the core holds an identity (sidecar.go).
// Addresses are the person's own and go only between the app and its core: nothing here logs one (I-13). Safe for
// concurrent use.

package sdk

import (
	"context"
	"fmt"

	"github.com/urnetwork/sidecar"
)

func init() {
	registerSidecarComponentFactory(SidecarComponentTls, newSidecarTlsComponent)
}

// The tls component's factory: the core's TlsComponent on its host class's args, or errSidecarComponentNotEnrolled
// while the core has no network or identity to make one for.
func newSidecarTlsComponent(ctx context.Context, local *sidecar.Sidecar, settings *SidecarSettings) (sidecar.SidecarComponent, error) {
	return newSidecarTlsComponentWith(ctx, settings, func(foreground bool) (*sidecar.TlsComponentArgs, error) {
		if foreground {
			return local.TlsForegroundComponentArgs()
		}
		return local.TlsComponentArgs()
	})
}

// The factory over argsOf, which makes a foreground host's args, with no CT watch, or a host's that stays up.
func newSidecarTlsComponentWith(ctx context.Context, settings *SidecarSettings, argsOf func(foreground bool) (*sidecar.TlsComponentArgs, error)) (sidecar.SidecarComponent, error) {
	hostClass, err := sidecarHostClassOf(settings.HostClass)
	if err != nil {
		return nil, err
	}
	args, err := argsOf(hostClass.foregroundOnly)
	if err != nil {
		// no network, identity or open keychain yet: it waits for the enrollment, or for the keychain at a restart
		return nil, fmt.Errorf("%w: %w", errSidecarComponentNotEnrolled, err)
	}
	return sidecar.NewTlsComponent(ctx, args)
}

// Whether address names tls/<port>/<host>, which listenSidecarTls serves rather than a plain listen.
func sidecarTlsAddress(address string) bool {
	parsed, err := sidecar.ParseSocketAddress(address)
	if err != nil {
		return false
	}
	_, namespace, err := sidecar.ParseSocketName(parsed.Name)
	return err == nil && namespace == sidecar.SocketNamespaceTls
}

// A tls/ name's listen on the core's ListenTls at weight, 0 for 100, bounded by ctx. The certificate it ensures is
// ordered on the first listen and may outlast ctx, in which case the order runs on and a later Listen finds it.
func listenSidecarTls(ctx context.Context, local *sidecar.Sidecar, address string, weight int) (sidecarSocketListenerBackend, error) {
	listener, err := local.ListenTls(ctx, address, &sidecar.TlsListenSettings{Weight: weight})
	if err != nil {
		return nil, err
	}
	return listener, nil
}
