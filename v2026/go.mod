module github.com/urnetwork/sdk/v2026

go 1.26.3

// pin the toolchain so every machine and CI builds the wasm with the same
// compiler; wasm_exec.js is copied from this toolchain's GOROOT (see js/Makefile)
// so the glue stays paired with the binary regardless of the locally installed go
toolchain go1.26.4

require (
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/gopacket v1.1.19
	github.com/gorilla/websocket v1.5.3
	golang.org/x/crypto v0.53.0
)

require (
	github.com/google/btree v1.1.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/pion/datachannel v1.6.2 // indirect
	github.com/pion/dtls/v3 v3.1.4 // indirect
	github.com/pion/ice/v4 v4.2.7 // indirect
	github.com/pion/interceptor v0.1.45 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.1.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.16 // indirect
	github.com/pion/rtp v1.10.2 // indirect
	github.com/pion/sctp v1.10.0 // indirect
	github.com/pion/sdp/v3 v3.0.18 // indirect
	github.com/pion/srtp/v3 v3.0.11 // indirect
	github.com/pion/stun/v3 v3.1.5 // indirect
	github.com/pion/transport/v4 v4.0.2 // indirect
	github.com/pion/turn/v5 v5.0.9 // indirect
	github.com/pion/webrtc/v4 v4.2.15 // indirect
	github.com/quic-go/quic-go v0.60.0 // indirect
	github.com/urnetwork/connect/v2026 v2026.7.21-998430760
	github.com/urnetwork/glog/v2026 v2026.7.21-998430760
	github.com/wlynxg/anet v0.0.5 // indirect
	golang.org/x/exp v0.0.0-20260611194520-c48552f49976 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gvisor.dev/gvisor v0.0.0-20260624000029-d10071d63566 // indirect
	src.agwa.name/tlshacks v0.0.3 // indirect
)
