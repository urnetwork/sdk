package sdk

// Client credentials and idempotent creation replies have finite, unambiguous
// JSON grammar before any response can select identity or trigger logout.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/urnetwork/connect"
)

// A complete bad response is never physical unavailability or confirmed
// credential revocation. The original credential remains owned by its caller.
type ClientControlResponseError struct{ detail string }

func (self *ClientControlResponseError) Error() string {
	return "invalid client control response: " + self.detail
}

// Only a complete all-leaf HTTP401 verdict may revoke a captured credential.
// A timeout or malformed response joined to401 is not confirmed revocation.
//
//gomobile:noexport
func ConfirmedClientRefreshRejection(err error) bool {
	if status, ok := err.(*connect.HttpStatusError); ok {
		return status.StatusCode == http.StatusUnauthorized
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !ConfirmedClientRefreshRejection(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return ConfirmedClientRefreshRejection(wrapped.Unwrap())
	}
	return false
}

// Validate duplicate keys recursively before normal typed decoding. Unknown
// refresh fields retain compatibility; duplicate ownership/error fields do not.
func decodeClientControlJson(raw []byte, target any) error {
	if len(raw) == 0 || len(raw) > 64*1024 {
		return &ClientControlResponseError{detail: "response size is outside its bound"}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var consume func(int) error
	consume = func(depth int) error {
		if depth > 16 {
			return errors.New("response nesting exceeds its bound")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("response contains a duplicate object field")
				}
				seen[name] = true
				if err := consume(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := consume(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected response delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := consume(0); err != nil {
		return &ClientControlResponseError{detail: "malformed or ambiguous JSON"}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return &ClientControlResponseError{detail: "trailing JSON"}
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return &ClientControlResponseError{detail: "response fields have invalid types"}
	}
	return nil
}
