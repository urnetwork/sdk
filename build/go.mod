module github.com/urnetwork/sdk/build

go 1.24.4

require (
	github.com/urnetwork/connect v0.0.0
	github.com/urnetwork/sdk v0.0.0
)

toolchain go1.24.5

replace github.com/urnetwork/sdk => ..

replace github.com/urnetwork/connect => ../../connect

replace github.com/golang/glog => ../../glog

require (
	golang.org/x/mobile v0.0.0-20251021151156-188f512ec823 // indirect
	golang.org/x/mod v0.29.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/tools v0.38.0 // indirect
)
