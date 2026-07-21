package main

/*
#include <stdint.h>
#include <stdbool.h>
*/
import "C"

import (
	"unsafe"

	"github.com/urnetwork/sdk/v2026"
)

// hand-written exports for surface the generator cannot express:
// byte-buffer results use the buffer-out pattern.
// keep the declarations in gen/gen.go manualHeaderSection in sync.

// copyOut implements the buffer-out pattern:
// *inout_len is always set to the needed size. the copy happens and true is
// returned only when out is non-null and the passed capacity is sufficient.
func copyOut(out *C.uint8_t, inoutLen *C.int32_t, data []byte) C.bool {
	if inoutLen == nil {
		return C.bool(false)
	}
	capacity := *inoutLen
	needed := C.int32_t(len(data))
	*inoutLen = needed
	if out == nil || capacity < needed {
		return C.bool(false)
	}
	if 0 < len(data) {
		copy(unsafe.Slice((*byte)(unsafe.Pointer(out)), int(needed)), data)
	}
	return C.bool(true)
}

//export urnet_decode_base58
func urnet_decode_base58(s *C.char, out *C.uint8_t, inoutLen *C.int32_t) C.bool {
	defer cgoGuard("urnet_decode_base58")
	data, err := sdk.DecodeBase58(goString(s))
	if err != nil {
		if inoutLen != nil {
			*inoutLen = 0
		}
		return C.bool(false)
	}
	return copyOut(out, inoutLen, data)
}

//export urnet_decrypt_data
func urnet_decrypt_data(encryptedDataBase58 *C.char, nonceBase58 *C.char, sharedSecretBase58 *C.char, out *C.uint8_t, inoutLen *C.int32_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_decrypt_data")
	data, err := sdk.DecryptData(goString(encryptedDataBase58), goString(nonceBase58), goString(sharedSecretBase58))
	if err != nil {
		setErrorOut(outError, err)
		if inoutLen != nil {
			*inoutLen = 0
		}
		return C.bool(false)
	}
	return copyOut(out, inoutLen, data)
}

//export urnet_generate_shared_secret
func urnet_generate_shared_secret(privateKey *C.uint8_t, privateKeyLen C.int32_t, publicKey *C.uint8_t, publicKeyLen C.int32_t, out *C.uint8_t, inoutLen *C.int32_t, outError **C.char) C.bool {
	defer cgoGuard("urnet_generate_shared_secret")
	data, err := sdk.GenerateSharedSecret(goBytes(privateKey, privateKeyLen), goBytes(publicKey, publicKeyLen))
	if err != nil {
		setErrorOut(outError, err)
		if inoutLen != nil {
			*inoutLen = 0
		}
		return C.bool(false)
	}
	return copyOut(out, inoutLen, data)
}

// device local key material, for stable provider identity across process starts
// (see DeviceLocal.GetKeyMaterial and NewDeviceLocalWithKeyMaterial)

//export urnet_device_local_get_client_key_seed
func urnet_device_local_get_client_key_seed(self C.uint64_t, out *C.uint8_t, inoutLen *C.int32_t) C.bool {
	defer cgoGuard("urnet_device_local_get_client_key_seed")
	self_, ok := resolveHandle[*sdk.DeviceLocal](uint64(self), "urnet_device_local_get_client_key_seed")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	return copyOut(out, inoutLen, self_.GetClientKeySeed())
}

//export urnet_device_local_get_provide_tls_certificate_pem
func urnet_device_local_get_provide_tls_certificate_pem(self C.uint64_t, out *C.uint8_t, inoutLen *C.int32_t) C.bool {
	defer cgoGuard("urnet_device_local_get_provide_tls_certificate_pem")
	self_, ok := resolveHandle[*sdk.DeviceLocal](uint64(self), "urnet_device_local_get_provide_tls_certificate_pem")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	return copyOut(out, inoutLen, self_.GetProvideTlsCertificatePem())
}

//export urnet_device_local_get_provide_tls_private_key_pem
func urnet_device_local_get_provide_tls_private_key_pem(self C.uint64_t, out *C.uint8_t, inoutLen *C.int32_t) C.bool {
	defer cgoGuard("urnet_device_local_get_provide_tls_private_key_pem")
	self_, ok := resolveHandle[*sdk.DeviceLocal](uint64(self), "urnet_device_local_get_provide_tls_private_key_pem")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	return copyOut(out, inoutLen, self_.GetProvideTlsPrivateKeyPem())
}

//export urnet_device_local_key_material_get_client_key_seed
func urnet_device_local_key_material_get_client_key_seed(self C.uint64_t, out *C.uint8_t, inoutLen *C.int32_t) C.bool {
	defer cgoGuard("urnet_device_local_key_material_get_client_key_seed")
	self_, ok := resolveHandle[*sdk.DeviceLocalKeyMaterial](uint64(self), "urnet_device_local_key_material_get_client_key_seed")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	return copyOut(out, inoutLen, self_.GetClientKeySeed())
}

//export urnet_device_local_key_material_get_provide_tls_certificate_pem
func urnet_device_local_key_material_get_provide_tls_certificate_pem(self C.uint64_t, out *C.uint8_t, inoutLen *C.int32_t) C.bool {
	defer cgoGuard("urnet_device_local_key_material_get_provide_tls_certificate_pem")
	self_, ok := resolveHandle[*sdk.DeviceLocalKeyMaterial](uint64(self), "urnet_device_local_key_material_get_provide_tls_certificate_pem")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	return copyOut(out, inoutLen, self_.GetProvideTlsCertificatePem())
}

//export urnet_device_local_key_material_get_provide_tls_private_key_pem
func urnet_device_local_key_material_get_provide_tls_private_key_pem(self C.uint64_t, out *C.uint8_t, inoutLen *C.int32_t) C.bool {
	defer cgoGuard("urnet_device_local_key_material_get_provide_tls_private_key_pem")
	self_, ok := resolveHandle[*sdk.DeviceLocalKeyMaterial](uint64(self), "urnet_device_local_key_material_get_provide_tls_private_key_pem")
	if !ok || self_ == nil {
		return C.bool(false)
	}
	return copyOut(out, inoutLen, self_.GetProvideTlsPrivateKeyPem())
}
