// Provider secrets stay raw bytes in their existing Go string and gob fields.
// JSON uses a distinct binary representation only when UTF-8 would lose bytes.
package sdk

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// The alias retains legacy field names without recursively invoking the codec.
type provideSecretKeyJSON ProvideSecretKey

// Valid UTF-8 retains the legacy literal form, including prefix-like strings.
// Binary keys never pass through encoding/json's replacement-rune conversion.
func (self ProvideSecretKey) MarshalJSON() ([]byte, error) {
	if utf8.ValidString(self.ProvideSecretKey) {
		return json.Marshal(provideSecretKeyJSON(self))
	}
	return json.Marshal(struct {
		ProvideMode            ProvideMode `json:"provide_mode"`
		ProvideSecretKeyBase64 string      `json:"provide_secret_key_base64"`
	}{
		ProvideMode:            self.ProvideMode,
		ProvideSecretKeyBase64: base64.StdEncoding.EncodeToString([]byte(self.ProvideSecretKey)),
	})
}

// Absent binary metadata always means a literal legacy string. Malformed or
// competing representations fail without changing the receiver or guessing
// missing bytes from an already-corrupted legacy record.
func (self *ProvideSecretKey) UnmarshalJSON(data []byte) error {
	if self == nil {
		return errors.New("decode provide secret key")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return errors.New("decode provide secret key")
	}
	key := provideSecretKeyJSON(*self)
	if err := json.Unmarshal(data, &key); err != nil {
		return errors.New("decode provide secret key")
	}
	if encoded, present := fields["provide_secret_key_base64"]; present {
		var encodedKey *string
		if err := json.Unmarshal(encoded, &encodedKey); err != nil || encodedKey == nil {
			return errors.New("decode provide secret key")
		}
		if plain, present := fields["provide_secret_key"]; present {
			var plainKey string
			if err := json.Unmarshal(plain, &plainKey); err != nil || plainKey != "" {
				return errors.New("decode provide secret key")
			}
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(*encodedKey)
		if err != nil || base64.StdEncoding.EncodeToString(decoded) != *encodedKey {
			return errors.New("decode provide secret key")
		}
		key.ProvideSecretKey = string(decoded)
	}
	*self = ProvideSecretKey(key)
	return nil
}
