module github.com/urnetwork/sdk/build

go 1.24.4

toolchain go1.24.5

require (
	github.com/urnetwork/connect v0.0.0
	github.com/urnetwork/glog v0.0.0
	github.com/urnetwork/sdk v0.0.0
)

replace github.com/urnetwork/sdk => ..

replace github.com/urnetwork/connect => ../../connect

replace github.com/urnetwork/glog => ../../glog

require (
	golang.org/x/mobile v0.0.0-20251209145715-2553ed8ce294 // indirect
	golang.org/x/mod v0.31.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	golang.org/x/tools v0.40.0 // indirect
)
