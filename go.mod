module github.com/urnetwork/sdk

go 1.24rc2

require (
	github.com/btcsuite/btcutil v1.0.2
	github.com/go-playground/assert/v2 v2.2.0
	github.com/golang-jwt/jwt/v5 v5.2.1
	github.com/golang/glog v1.2.4
	github.com/urnetwork/connect v0.1.12
	github.com/urnetwork/protocol v0.1.4
	golang.org/x/crypto v0.32.0
	golang.org/x/exp v0.0.0-20250128182459-e0ece0dbea4c
)

require (
	github.com/go-task/slim-sprig v0.0.0-20230315185526-52ccab3ef572 // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/google/pprof v0.0.0-20210407192527-94a9f03dee38 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/oklog/ulid/v2 v2.1.0 // indirect
	github.com/onsi/ginkgo/v2 v2.9.5 // indirect
	github.com/quic-go/quic-go v0.49.0 // indirect
	go.uber.org/mock v0.5.0 // indirect
	golang.org/x/mod v0.22.0 // indirect
	golang.org/x/net v0.34.0 // indirect
	golang.org/x/sync v0.11.0 // indirect
	golang.org/x/sys v0.30.0 // indirect
	golang.org/x/text v0.22.0 // indirect
	golang.org/x/tools v0.29.0 // indirect
	google.golang.org/protobuf v1.36.5 // indirect
	src.agwa.name/tlshacks v0.0.0-20231008131857-90d701ba3225 // indirect
)

replace github.com/urnetwork/connect => ../connect

replace github.com/urnetwork/protocol => ../protocol
