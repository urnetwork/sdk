// Checked identity reads distinguish a missing or legacy-empty record from
// observation failure. The compatibility getter and storage format stay unchanged.
package sdk

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Loads optional identity material without repairing or replacing its file.
// Callers must stop key-preserving startup/cleanup on error, not retry with nil.
// Readable symlinks and successful legacy-empty JSON retain their old meaning;
// this is not a cross-process snapshot or validation of seed/PEM cryptography.
func (self *LocalState) LoadDeviceLocalKeyMaterial() (*DeviceLocalKeyMaterial, error) {
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	return self.loadDeviceLocalKeyMaterialWithLock()
}

// Called inside the paired storage transaction so loading and restoring known
// material cannot interleave with this LocalState's checked reads/key saves.
func (self *LocalState) loadDeviceLocalKeyMaterialWithLock() (*DeviceLocalKeyMaterial, error) {
	path := filepath.Join(self.localStorageDir, ".device_local_key_material")
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, localStorageStageError("inspect device key material", err)
		}
		// LocalState creates this parent. Losing access to it is not evidence
		// that an established identity is absent.
		parent, parentErr := os.Stat(self.localStorageDir)
		if parentErr != nil {
			return nil, localStorageStageError("inspect device key directory", parentErr)
		}
		if !parent.IsDir() {
			return nil, errors.New("device key parent is not a directory")
		}
		return nil, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(path)
		if err != nil {
			return nil, localStorageStageError("resolve device key material", err)
		}
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("device key material is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, localStorageStageError("read device key material", err)
	}
	var stored deviceLocalKeyMaterialStorage
	if err := json.Unmarshal(data, &stored); err != nil {
		// Some decoder errors include the rejected value. Public errors must
		// identify the stage without exposing stored identity bytes.
		return nil, errors.New("decode device key material")
	}
	material := NewDeviceLocalKeyMaterial(
		stored.ClientKeySeed,
		stored.ProvideTlsCertificatePem,
		stored.ProvideTlsPrivateKeyPem,
	)
	if material.IsEmpty() {
		return nil, nil
	}
	return material, nil
}
