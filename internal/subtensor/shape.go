package subtensor

import (
	"errors"
	"fmt"
)

// What the runtime has to look like before anything is signed. Metadata is
// read at run time so a runtime upgrade that leaves these shapes alone needs
// no release. A change this client does not know refuses, with
// RuntimeChangedError, rather than signing something it cannot vouch for.

// RuntimeChangedError says the runtime's call or transaction format is not
// the one this client knows. Nothing was signed.
type RuntimeChangedError struct {
	Detail string
}

func (self *RuntimeChangedError) Error() string {
	return "runtime changed: " + self.Detail
}

func runtimeChanged(format string, args ...any) error {
	return &RuntimeChangedError{Detail: fmt.Sprintf(format, args...)}
}

// IsRuntimeChanged reports whether an error is a RuntimeChangedError.
func IsRuntimeChanged(err error) bool {
	var changed *RuntimeChangedError
	return errors.As(err, &changed)
}

type extensionStep uint8

const (
	// nothing in the transaction or the signing payload
	extensionEmpty extensionStep = iota
	extensionSpecVersion
	extensionTransactionVersion
	extensionGenesis
	extensionMortality
	extensionNonce
	extensionTip
	extensionMetadataHash
)

// signingPlan checks every signed extension of the runtime and says how each
// is filled, in the runtime's order. One whose encoding is empty needs nothing
// from this client, whatever it is called, so a new one of those does not stop
// a release. One this client cannot fill refuses. The ones that make a
// transaction safe to sign have to be there.
func (self *Metadata) signingPlan() ([]extensionStep, error) {
	if self.extrinsicVersion != 4 {
		return nil, runtimeChanged("extrinsic version %d, this client signs 4", self.extrinsicVersion)
	}
	if err := self.checkExtrinsicEnvelope(); err != nil {
		return nil, err
	}
	seen := map[extensionStep]int{}
	plan := make([]extensionStep, 0, len(self.extensions))
	for _, extension := range self.extensions {
		step, err := self.planExtension(extension)
		if err != nil {
			return nil, err
		}
		seen[step] += 1
		plan = append(plan, step)
	}
	for step, name := range map[extensionStep]string{
		extensionSpecVersion:        "CheckSpecVersion",
		extensionTransactionVersion: "CheckTxVersion",
		extensionGenesis:            "CheckGenesis",
		extensionMortality:          "CheckMortality",
		extensionNonce:              "CheckNonce",
		extensionTip:                "ChargeTransactionPayment",
	} {
		if seen[step] != 1 {
			return nil, runtimeChanged("signed extension %s appears %d times", name, seen[step])
		}
	}
	if 1 < seen[extensionMetadataHash] {
		return nil, runtimeChanged("signed extension CheckMetadataHash appears %d times", seen[extensionMetadataHash])
	}
	return plan, nil
}

func (self *Metadata) planExtension(extension metadataExtension) (extensionStep, error) {
	extraEmpty, err := self.isZeroSized(extension.ty)
	if err != nil {
		return 0, runtimeChanged("signed extension %s: %v", extension.identifier, err)
	}
	additionalEmpty, err := self.isZeroSized(extension.additional)
	if err != nil {
		return 0, runtimeChanged("signed extension %s: %v", extension.identifier, err)
	}
	refuse := func() (extensionStep, error) {
		return 0, runtimeChanged("signed extension %s has a shape this client does not know", extension.identifier)
	}
	switch extension.identifier {
	case "CheckSpecVersion":
		if !extraEmpty || !self.isPrimitive(extension.additional, primitiveU32) {
			return refuse()
		}
		return extensionSpecVersion, nil
	case "CheckTxVersion":
		if !extraEmpty || !self.isPrimitive(extension.additional, primitiveU32) {
			return refuse()
		}
		return extensionTransactionVersion, nil
	case "CheckGenesis":
		if !extraEmpty || !self.isBytes32(extension.additional) {
			return refuse()
		}
		return extensionGenesis, nil
	case "CheckMortality", "CheckEra":
		era, err := self.resolve(extension.ty)
		if err != nil || era.kind != typeDefVariant || era.pathName() != "Era" || !self.isBytes32(extension.additional) {
			return refuse()
		}
		return extensionMortality, nil
	case "CheckNonce":
		if !self.isCompactOf(extension.ty, primitiveU32) || !additionalEmpty {
			return refuse()
		}
		return extensionNonce, nil
	case "ChargeTransactionPayment":
		if !self.isCompactOf(extension.ty, primitiveU64) || !additionalEmpty {
			return refuse()
		}
		return extensionTip, nil
	case "CheckMetadataHash":
		mode, err := self.resolve(extension.ty)
		if err != nil || mode.kind != typeDefVariant || !self.isOptionOfBytes32(extension.additional) {
			return refuse()
		}
		disabled := mode.variantByName("Disabled")
		if disabled == nil || disabled.index != metadataHashModeDisabled || len(disabled.fields) != 0 {
			return refuse()
		}
		return extensionMetadataHash, nil
	default:
		if !extraEmpty || !additionalEmpty {
			return refuse()
		}
		return extensionEmpty, nil
	}
}

// checkExtrinsicEnvelope checks the signed extrinsic's address and signature
// are the ones this client writes.
func (self *Metadata) checkExtrinsicEnvelope() error {
	extrinsic, err := self.lookup(self.extrinsicType)
	if err != nil {
		return runtimeChanged("the extrinsic type is gone")
	}
	addressId, hasAddress := extrinsic.param("Address")
	signatureId, hasSignature := extrinsic.param("Signature")
	if !hasAddress || !hasSignature {
		return runtimeChanged("the extrinsic type does not name its address and signature")
	}
	address, err := self.resolve(addressId)
	if err != nil || address.kind != typeDefVariant {
		return runtimeChanged("the extrinsic address is not an enum")
	}
	id := address.variantByName("Id")
	if id == nil || id.index != multiAddressId || len(id.fields) != 1 || !self.isBytes32(id.fields[0].ty) {
		return runtimeChanged("the extrinsic address has no 32-byte Id at index %d", multiAddressId)
	}
	signature, err := self.resolve(signatureId)
	if err != nil || signature.kind != typeDefVariant {
		return runtimeChanged("the extrinsic signature is not an enum")
	}
	sr25519 := signature.variantByName("Sr25519")
	if sr25519 == nil || sr25519.index != multiSignatureSr25519 || len(sr25519.fields) != 1 {
		return runtimeChanged("the extrinsic signature has no Sr25519 at index %d", multiSignatureSr25519)
	}
	bytes64, err := self.resolve(sr25519.fields[0].ty)
	if err != nil || bytes64.kind != typeDefArray || bytes64.length != 64 || !self.isPrimitive(bytes64.elem, primitiveU8) {
		return runtimeChanged("the Sr25519 signature is not 64 bytes")
	}
	return nil
}

// checkCall checks the runtime has this exact call: the same pallet and call
// indices, and the same arguments with the same encodings.
func (self *Metadata) checkCall(call *Call) error {
	pallet := self.pallets[call.pallet]
	if pallet == nil {
		return runtimeChanged("pallet %s is gone", call.pallet)
	}
	if pallet.index != call.palletIndex {
		return runtimeChanged("pallet %s is at index %d, this client knows %d", call.pallet, pallet.index, call.palletIndex)
	}
	if !pallet.hasCalls {
		return runtimeChanged("pallet %s has no calls", call.pallet)
	}
	calls, err := self.lookup(pallet.calls)
	if err != nil || calls.kind != typeDefVariant {
		return runtimeChanged("pallet %s's calls are not an enum", call.pallet)
	}
	variant := calls.variantByName(call.name)
	if variant == nil {
		return runtimeChanged("call %s.%s is gone", call.pallet, call.name)
	}
	if variant.index != call.callIndex {
		return runtimeChanged("call %s.%s is at index %d, this client knows %d", call.pallet, call.name, variant.index, call.callIndex)
	}
	if len(variant.fields) != len(call.fields) {
		return runtimeChanged("call %s.%s takes %d arguments, this client knows %d", call.pallet, call.name, len(variant.fields), len(call.fields))
	}
	for i, field := range variant.fields {
		expected := call.fields[i]
		if field.name != expected.name || !expected.shape(self, field.ty) {
			return runtimeChanged("call %s.%s argument %d is %q with a shape this client does not know", call.pallet, call.name, i, field.name)
		}
	}
	return nil
}

// CheckRuntime is the whole signing shape for a call, for hosts that want to
// know before they ask for anything else. Prepare runs the same checks.
func (self *Metadata) CheckRuntime(call *Call) error {
	if _, err := self.signingPlan(); err != nil {
		return err
	}
	return self.checkCall(call)
}
