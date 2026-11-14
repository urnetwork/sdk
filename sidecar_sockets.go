//go:build !ios_extension && !js

// SidecarSockets (§18.5, §9.9; SDK-M8-03): an app's dial and listen on its network's §9 sockets, each a thin mapping
// onto the core's Sidecar.Dial and Sidecar.Listen, whose checks are §9.6's and stand behind every call: the listener's
// claim under the identity key Hello verified, its listen grant in this device's own evaluation of its config (owner
// decision 3, I-5), every stream Required (I-3), and on a tls/ or smtp/ name §5.4's gate first, whose pending answer
// comes back as approval_pending:<id>. Nothing here adds a check the core lacks or skips one it makes.
//
// Dial answers the sdk's *Socket, the connection apps already read and write on worker threads, and Accept is for a
// worker thread too, waiting at most its timeout. ListenContext is Go only: a net.Listener for an app's own Go server,
// which gobind skips and mobileexports names (SDK-M8-06).
//
// Listening follows §18.4's table: on iOS and Android in the foreground, the sidecar role running only while the app
// is open (owner decision 197), and never on a Worker, whose build links no sdk.Sidecar (this file is !js). An app
// reports its foreground through SDK-M10-02's host calls; until those land this file rests on the role's own rule,
// that a phone's sidecar runs only while its app is open, and refuses nothing more by it.
//
// The sockets component is the core's (socket_component.go). Its factory builds it once the core is enrolled, and
// before then waits for EnrollWithDevice, Dial and Listen answering unavailable meanwhile. Its key ring, network
// labels and policy come from the verified config head, which no host installs yet, so until one does the core
// answers unavailable or denied: closed.
//
// Addresses are the person's own and go only between the app and its core on this device: nothing here logs one
// (I-13). Safe for concurrent use.

package sdk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"
)

func init() {
	registerSidecarComponentFactory(SidecarComponentSockets, newSidecarSocketsComponent)
}

// What an app's socket calls are held to.
type sidecarSocketsSettings struct {
	// How long Listen waits for the service's first verdict on a registration (§9.5), and how long the gate may hold
	// a tls/ or smtp/ listen: 30 s. Past it the listen is withdrawn and Listen answers its context's error.
	ListenTimeout time.Duration
	// The longest timeout a Dial or Accept takes, in milliseconds: 2,147,483,647, the int32 range every binding
	// passes. A larger or negative one is refused.
	TimeoutMaxMillis int64
}

// The defaults, a fresh value each call.
func defaultSidecarSocketsSettings() *sidecarSocketsSettings {
	return &sidecarSocketsSettings{
		ListenTimeout:    30 * time.Second,
		TimeoutMaxMillis: 2147483647,
	}
}

var (
	// A Dial or Accept timeout out of range.
	errSidecarSocketTimeout = errors.New("a socket timeout is 0 or a positive count of milliseconds within the int32 range")
	// The Sidecar closed.
	errSidecarSocketsClosed = errors.New("the sidecar's sockets closed")
	// An Accept whose timeout passed with no stream.
	errSidecarSocketAcceptTimeout = errors.New("no stream arrived within the accept's timeout")
)

// What SidecarSockets asks of the core: *sidecar.Sidecar through sidecarSocketsCoreBackend, or a test's.
type sidecarSocketsBackend interface {
	// sidecar.Sidecar.Dial with no filter.
	Dial(ctx context.Context, address string) (net.Conn, error)
	// sidecar.Sidecar.Listen at weight, 0 taking the core's 100.
	Listen(ctx context.Context, address string, weight int) (sidecarSocketListenerBackend, error)
}

// A listener of the core's: its net.Listener, and an accept a context ends.
type sidecarSocketListenerBackend interface {
	net.Listener
	AcceptContext(ctx context.Context) (net.Conn, error)
}

// sidecarSocketsBackend over the core.
type sidecarSocketsCoreBackend struct {
	local *sidecar.Sidecar
}

// See sidecarSocketsBackend.
func (self *sidecarSocketsCoreBackend) Dial(ctx context.Context, address string) (net.Conn, error) {
	conn, err := self.local.Dial(ctx, address, nil)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// See sidecarSocketsBackend.
func (self *sidecarSocketsCoreBackend) Listen(ctx context.Context, address string, weight int) (sidecarSocketListenerBackend, error) {
	listener, err := self.local.Listen(ctx, address, &sidecar.SocketListenSettings{Weight: weight})
	if err != nil {
		return nil, err
	}
	return &sidecarSocketListenerCore{SocketListener: listener}, nil
}

// sidecarSocketListenerBackend over a listener of the core's.
type sidecarSocketListenerCore struct {
	*sidecar.SocketListener
}

// See sidecarSocketListenerBackend.
func (self *sidecarSocketListenerCore) AcceptContext(ctx context.Context) (net.Conn, error) {
	conn, err := self.AcceptSocket(ctx)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// The sockets component's factory: the core's SocketComponent for the network the core is enrolled in, its registrar
// running until the Sidecar closes, or errSidecarComponentNotEnrolled before the core has a network.
func newSidecarSocketsComponent(ctx context.Context, local *sidecar.Sidecar, settings *SidecarSettings) (sidecar.SidecarComponent, error) {
	args, err := local.SocketComponentArgs()
	if err != nil {
		// its one refusal: no enrolled network
		return nil, fmt.Errorf("%w: %w", errSidecarComponentNotEnrolled, err)
	}
	return sidecar.NewSocketComponent(ctx, args)
}

// One network's sockets for an app. Safe for concurrent use.
type SidecarSockets struct {
	ctx      context.Context
	backend  sidecarSocketsBackend
	settings *sidecarSocketsSettings
}

// A SidecarSockets on backend until ctx ends.
func newSidecarSockets(ctx context.Context, backend sidecarSocketsBackend, settings *sidecarSocketsSettings) *SidecarSockets {
	return &SidecarSockets{ctx: ctx, backend: backend, settings: settings}
}

// This Sidecar's sockets (§18.5): nil unless the settings named sockets, whether or not the core's component is built
// yet (Dial and Listen answer unavailable until it is). The remote backend's are its daemon's, and nil here until
// SDK-M8-02's control channel says how they reach an app.
func (self *Sidecar) GetSockets() *SidecarSockets {
	sockets, err := traceWithReturn(func() *SidecarSockets {
		if !self.settings.Components.Contains(SidecarComponentSockets) {
			return nil
		}
		local, _ := self.backends()
		if local == nil {
			return nil
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil
		}
		if self.sockets == nil {
			self.sockets = newSidecarSockets(self.ctx, &sidecarSocketsCoreBackend{local: local}, self.socketsSettings)
		}
		return self.sockets
	})
	if err != nil {
		return nil
	}
	return sockets
}

// A context ending with the Sidecar and, for a positive timeoutMillis, after it; 0 waits as long as the call's own
// bounds allow.
func (self *SidecarSockets) timeoutCtx(timeoutMillis int64) (context.Context, context.CancelFunc, error) {
	if timeoutMillis < 0 || self.settings.TimeoutMaxMillis < timeoutMillis {
		return nil, nil, errSidecarSocketTimeout
	}
	if self.ctx.Err() != nil {
		return nil, nil, errSidecarSocketsClosed
	}
	if timeoutMillis == 0 {
		ctx, cancel := context.WithCancel(self.ctx)
		return ctx, cancel, nil
	}
	ctx, cancel := context.WithTimeout(self.ctx, time.Duration(timeoutMillis)*time.Millisecond)
	return ctx, cancel, nil
}

// A stream to one listener of address in this sidecar's network (§9.6, §9.9), waiting at most timeoutMillis, 0 for
// the dial's own bounds. Errors are the core's *net.OpError around §9.9's values, which never repeat the address.
func (self *SidecarSockets) Dial(address string, timeoutMillis int64) (*Socket, error) {
	return traceWithReturnError(func() (*Socket, error) {
		ctx, cancel, err := self.timeoutCtx(timeoutMillis)
		if err != nil {
			return nil, err
		}
		defer cancel()
		conn, err := self.backend.Dial(ctx, address)
		if err != nil {
			return nil, err
		}
		return &Socket{conn: conn}, nil
	})
}

// A listen on address in this sidecar's network at weight, 1 to 1000 and 0 for 100 (§9.5), live once the service's
// first verdict takes it; on tls/ and smtp/ §5.4's gate first, whose pending answer is an error naming
// approval_pending:<id>. Accept its streams on a worker thread, and Close it when done.
func (self *SidecarSockets) Listen(address string, weight int32) (*SidecarSocketListener, error) {
	return traceWithReturnError(func() (*SidecarSocketListener, error) {
		listener, err := self.listen(self.ctx, address, int(weight))
		if err != nil {
			return nil, err
		}
		return &SidecarSocketListener{sockets: self, listener: listener}, nil
	})
}

// A listen bounded by ctx and ListenTimeout, the gate's pending answer named as SidecarResult names it.
func (self *SidecarSockets) listen(ctx context.Context, address string, weight int) (sidecarSocketListenerBackend, error) {
	if self.ctx.Err() != nil {
		return nil, errSidecarSocketsClosed
	}
	listenCtx, cancel := context.WithTimeout(ctx, self.settings.ListenTimeout)
	defer cancel()
	// a listen outlives no Sidecar
	stop := context.AfterFunc(self.ctx, cancel)
	defer stop()
	listener, err := self.backend.Listen(listenCtx, address, weight)
	if err != nil {
		var pending *sidecar.ApprovalPendingError
		if errors.As(err, &pending) {
			return nil, fmt.Errorf("%s%s: %w", sidecarApprovalPendingPrefix, pending.ApprovalId, err)
		}
		return nil, err
	}
	return listener, nil
}

// Go only: a listen on address as a net.Listener, at the core's default weight, for an app's own Go server. ctx bounds
// the listen alone, as net.ListenConfig's does; the listener lives until its Close or the Sidecar's.
func (self *SidecarSockets) ListenContext(ctx context.Context, address string) (net.Listener, error) {
	listener, err := self.listen(ctx, address, 0)
	if err != nil {
		return nil, err
	}
	return listener, nil
}

// One listen of an app's. Safe for concurrent use.
type SidecarSocketListener struct {
	sockets  *SidecarSockets
	listener sidecarSocketListenerBackend

	closeOnce sync.Once
	closeErr  error
}

// The next stream of the listen, waiting at most timeoutMillis, 0 until one comes or the listener closes. Blocks: call
// it on a worker thread.
func (self *SidecarSocketListener) Accept(timeoutMillis int64) (*Socket, error) {
	return traceWithReturnError(func() (*Socket, error) {
		ctx, cancel, err := self.sockets.timeoutCtx(timeoutMillis)
		if err != nil {
			return nil, err
		}
		defer cancel()
		conn, err := self.listener.AcceptContext(ctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, errSidecarSocketAcceptTimeout
			}
			if self.sockets.ctx.Err() != nil {
				return nil, errSidecarSocketsClosed
			}
			return nil, err
		}
		return &Socket{conn: conn}, nil
	})
}

// The listen's network and client, as ids (I-13).
func (self *SidecarSocketListener) GetAddr() string {
	addr, err := traceWithReturn(func() string {
		return self.listener.Addr().String()
	})
	if err != nil {
		return ""
	}
	return addr
}

// Unlistens at once (§9.9). A second Close does nothing.
func (self *SidecarSocketListener) Close() error {
	return traceWithError(func() error {
		self.closeOnce.Do(func() {
			self.closeErr = self.listener.Close()
		})
		return self.closeErr
	})
}
