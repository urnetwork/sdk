// Package subprotocolrpc contains the private gob messages used by the SDK's
// companion-device subprotocol service.
package subprotocolrpc

import "github.com/urnetwork/connect/v2026"

// Request and Response must remain exported: net/rpc rejects methods whose
// argument or reply base type is unexported. Keeping them in an internal
// package lets the service retain that requirement without adding its wire
// structs to the SDK's public mobile binding surface.
type Request struct {
	ID, ClientID, InstanceID connect.Id
	Op                       string
	Protocol                 int32
	Destination              connect.Id
	Data                     []byte
	Start                    bool
	TimeoutMillis            int64
}

type Response struct {
	Pending, OK bool
	Source      connect.Id
	Data        []byte
	Protocols   []int32
	Error       string
}
