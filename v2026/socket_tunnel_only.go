package sdk

// socket_tunnel_only.go — sockets that leave the device only through the
// tunnel. The developer sockets (socket.go) follow the device's route, so a
// device with local routing on and no destination sends them out of the
// host's own network. A tunnel-only socket never does: it has its own private
// TUN, whose pump sends through the remote route alone (the upgrade mux or the
// remote user nat client, see sendPacketsWithRoutePolicy). The hotspot proxy
// (sdk/hotspot) dials with it, so a device that shares its connection
// can only share the tunnel.
//
// The route is read from the same immutable snapshot the packet path reads.
// A dial refuses at once while the snapshot has no remote route. A route that
// turns local after a connection opened drops the connection's packets, which
// the device counts in tunnelOnlyDropPacketCount, until a remote route
// returns or the connection times out.

import (
	"context"
	"errors"
	"net"

	"github.com/urnetwork/connect/v2026"
)

// The error of a tunnel-only dial on a device with no remote route: it is not
// connected to a destination, so nothing may leave it.
var ErrTunnelNotConnected = errors.New("not connected: the tunnel has no remote route")

// Dials tcp or udp like DialContext, through the remote route only. Returns
// ErrTunnelNotConnected while the device has no remote route. The resolver is
// the same private resolver as the developer sockets', which also resolves
// only through this socket's TUN.
//
//gomobile:noexport
func (self *DeviceLocal) DialTunnelOnlyContext(ctx context.Context, network string, address string) (net.Conn, error) {
	if !self.tunnelRouteConnected() {
		return nil, ErrTunnelNotConnected
	}
	return self.dialSocket(ctx, &self.tunnelOnlySockets, self.tunnelOnlySocketTun, network, address)
}

// Whether the route snapshot has a remote route, the mux or the remote user
// nat client. Local routing alone does not count.
func (self *DeviceLocal) tunnelRouteConnected() bool {
	route := self.sendRoute.Load()
	return route != nil && (route.upgradeMux != nil || route.remoteUserNatClient != nil)
}

// The private TUN of the tunnel-only sockets.
func (self *DeviceLocal) tunnelOnlySocketTun() (*connect.Tun, error) {
	return self.privateSocketTun(&self.tunnelOnlySockets, self.sendPacketsTunnelOnly)
}

// Takes every pooled packet. The pump sends like the developer sockets' pump
// (sendPacketsNoCopy), restricted to the remote route; see
// sendPacketsWithRoutePolicy.
func (self *DeviceLocal) sendPacketsTunnelOnly(packets [][]byte) int {
	return self.sendPacketsWithRoutePolicy(packets, true, true)
}
