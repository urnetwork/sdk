// Private gob messages for the companion-device subprotocol service.
package sdk

import "github.com/urnetwork/connect/v2026"

// Anonymous struct aliases keep the wire types private while net/rpc accepts
// their unnamed reflected types. The companion build exports aliases for Go
// and JavaScript callers; the mobile binding build exposes no wire structs.
type subprotocolRpcRequest = struct {
	ID, ClientID, InstanceID connect.Id
	Op                       string
	Protocol                 int32
	Destination              connect.Id
	Data                     []byte
	Start                    bool
	TimeoutMillis            int64
}

// Carries a companion RPC result with the same field names and gob wire format.
type subprotocolRpcResponse = struct {
	Pending, OK bool
	Source      connect.Id
	Data        []byte
	Protocols   []int32
	Error       string
}
