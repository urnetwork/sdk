module github.com/urnetwork/sdk/build

go 1.24.0

require (
	github.com/urnetwork/connect v2025.4.3-58948734+incompatible
	github.com/urnetwork/connect/protocol v0.0.0
	github.com/urnetwork/sdk v0.0.0
)

replace github.com/urnetwork/sdk => ..

replace github.com/urnetwork/connect => ../../connect

replace github.com/urnetwork/connect/protocol => ../../connect/protocol
