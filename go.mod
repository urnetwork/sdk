module github.com/urnetwork/sdk

go 1.23.0

require (
	github.com/btcsuite/btcutil v1.0.2
	github.com/go-playground/assert/v2 v2.2.0
	github.com/golang-jwt/jwt/v5 v5.2.1
	github.com/golang/glog v1.2.4
	github.com/urnetwork/connect v0.0.0
	github.com/urnetwork/connect/protocol v0.0.0
	golang.org/x/crypto v0.33.0
	golang.org/x/exp v0.0.0-20250210185358-939b2ce775ac
)

require (
	github.com/google/gopacket v1.1.19 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/oklog/ulid/v2 v2.1.0 // indirect
	golang.org/x/mobile v0.0.0-20250210185054-b38b8813d607 // indirect
	golang.org/x/mod v0.23.0 // indirect
	golang.org/x/net v0.35.0 // indirect
	golang.org/x/sync v0.11.0 // indirect
	golang.org/x/sys v0.30.0 // indirect
	golang.org/x/text v0.22.0 // indirect
	golang.org/x/tools v0.30.0 // indirect
	google.golang.org/protobuf v1.36.5 // indirect
	src.agwa.name/tlshacks v0.0.0-20231008131857-90d701ba3225 // indirect
)

replace github.com/urnetwork/connect => ../connect

replace github.com/urnetwork/connect/protocol => ../connect/protocol

replace github.com/urnetwork/userwireguard => ../userwireguard
