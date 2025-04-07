module github.com/urnetwork/sdk/build

go 1.24.0

require (
	github.com/urnetwork/connect v0.0.0
	github.com/urnetwork/sdk v0.0.0
)

require (
	golang.org/x/mobile v0.0.0-20250305212854-3a7bc9f8a4de // indirect
	golang.org/x/mod v0.24.0 // indirect
	golang.org/x/sync v0.13.0 // indirect
	golang.org/x/tools v0.31.0 // indirect
)

replace github.com/urnetwork/sdk => ..

replace github.com/urnetwork/connect => ../../connect
