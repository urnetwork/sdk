module github.com/urnetwork/sdk

go 1.24.4

toolchain go1.24.5

require (
	github.com/btcsuite/btcutil v1.0.2
	github.com/go-playground/assert/v2 v2.2.0
	github.com/golang-jwt/jwt/v5 v5.3.0
	github.com/urnetwork/connect v0.0.0
	github.com/urnetwork/glog v0.0.0
	golang.org/x/crypto v0.45.0
	golang.org/x/exp v0.0.0-20251113190631-e25ba8c21ef6
)

require (
	github.com/google/gopacket v1.1.19 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/oklog/ulid/v2 v2.1.1 // indirect
	github.com/quic-go/quic-go v0.57.0 // indirect
	golang.org/x/net v0.47.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/text v0.31.0 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
	src.agwa.name/tlshacks v0.0.0-20250628001001-c92050511ef4 // indirect
)

replace github.com/urnetwork/connect => ../connect

replace github.com/urnetwork/glog => ../glog
