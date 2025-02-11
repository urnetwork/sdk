module main

go 1.24rc2

require github.com/urnetwork/sdk v0.0.0

require (
	github.com/btcsuite/btcutil v1.0.2 // indirect
	github.com/golang-jwt/jwt/v5 v5.2.1 // indirect
	github.com/golang/glog v1.2.4 // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/oklog/ulid/v2 v2.1.0 // indirect
	github.com/urnetwork/connect v0.1.12 // indirect
	github.com/urnetwork/connect/protocol v0.0.0 // indirect
	golang.org/x/crypto v0.33.0 // indirect
	golang.org/x/exp v0.0.0-20250210185358-939b2ce775ac // indirect
	golang.org/x/net v0.35.0 // indirect
	golang.org/x/sys v0.30.0 // indirect
	golang.org/x/text v0.22.0 // indirect
	google.golang.org/protobuf v1.36.5 // indirect
	src.agwa.name/tlshacks v0.0.0-20231008131857-90d701ba3225 // indirect
)

replace github.com/urnetwork/sdk => ..

replace github.com/urnetwork/connect => ../../connect

replace github.com/urnetwork/connect/protocol => ../../connect/protocol
