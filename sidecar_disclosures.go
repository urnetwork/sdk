//go:build !ios_extension && !js

// Disclosures for the apps (DESIGN-COMPUTE §15.3, §2.8, D-CSURF-1): the functions behind Sidecar.Disclosures,
// AckDisclosure and AckNetworkDisclosure. Created as shells by CSURF-M11-01; CTRUST's tasks fill them here, none of
// them editing sidecar.go. An acknowledgment is the person's act in the app's UI, after the app showed the whole text,
// and no tool, MCP call or RPC reaches it; in remote mode it goes by DESIGN §5.8's Confirm method
// compute.disclosure.ack with confirmed set. Until CTRUST builds them, each answers ErrSidecarComputeNotBuilt: no
// disclosure list, and no acknowledgment recorded.

package sdk

// Every disclosure of the newest verified policy, as JSON: ErrSidecarComputeNotBuilt until CTRUST builds it.
func sidecarDisclosures(args *sidecarComputeArgs) (string, error) {
	return "", ErrSidecarComputeNotBuilt
}

// The person's acknowledgment of disclosure id: ErrSidecarComputeNotBuilt, recording nothing, until CTRUST builds it.
func ackSidecarDisclosure(args *sidecarComputeArgs, id string) error {
	return ErrSidecarComputeNotBuilt
}

// An Owner's or Admin's acknowledgment of disclosure id for the network: ErrSidecarComputeNotBuilt, signing and saving
// nothing, until CTRUST builds it.
func ackSidecarNetworkDisclosure(args *sidecarComputeArgs, id string) error {
	return ErrSidecarComputeNotBuilt
}
