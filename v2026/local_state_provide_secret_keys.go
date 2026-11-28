// Checked provider-secret observation preserves legacy successful values while
// keeping unreadable state distinct from absence and fresh-key initialization.
package sdk

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Nil without error means an absent leaf in an accessible directory. Successful
// legacy null/empty arrays return an empty nonnil list, just like the getter;
// readable symlinks and optional fields retain their meaning. No cryptographic
// validation or repair is performed. Callers must not regenerate on error.
func (self *LocalState) LoadProvideSecretKeys() (*ProvideSecretKeyList, error) {
	self.authStateLock.Lock()
	defer self.authStateLock.Unlock()
	if self.ctx.Err() != nil {
		return nil, errors.New("provide secret storage owner is closed")
	}
	path := filepath.Join(self.localStorageDir, ".provide_secret_keys")
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("inspect provide secret keys")
		}
		parent, parentErr := os.Stat(self.localStorageDir)
		if parentErr != nil || !parent.IsDir() {
			return nil, errors.New("provide secret directory is unavailable")
		}
		return nil, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(path)
		if err != nil {
			return nil, errors.New("resolve provide secret keys")
		}
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("provide secret keys are not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("read provide secret keys")
	}
	secrets := NewProvideSecretKeyList()
	if err := json.Unmarshal(data, secrets); err != nil {
		return nil, errors.New("decode provide secret keys")
	}
	for _, secret := range secrets.values {
		if secret == nil {
			// The old decoder admits this but LoadProvideSecretKeys dereferences
			// it. Reject the unusable element rather than panic or regenerate.
			return nil, errors.New("decode provide secret keys")
		}
	}
	return secrets, nil
}
