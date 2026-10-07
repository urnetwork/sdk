// Decodes the server's decimal-string operator id without changing sdk fields.
package sdk

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Preserves the int64 decoder's missing/null behavior and numeric encoding.
// A malformed supplied id fails before any partially decoded schedule is kept.
func (self *SnEpochResult) UnmarshalJSON(data []byte) error {
	type epochResult SnEpochResult
	result := epochResult(*self)
	wire := struct {
		*epochResult
		NoId snEpochNoId `json:"no_id"`
	}{
		epochResult: &result,
		NoId:        snEpochNoId(self.NoId),
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	result.NoId = int64(wire.NoId)
	*self = SnEpochResult(result)
	return nil
}

// Accepts canonical decimal strings and legacy integer tokens with exact
// signed range checks. Null retains the previous value, like a plain int64.
type snEpochNoId int64

// Decodes each supplied field occurrence, so a later duplicate cannot hide an
// earlier malformed id. No numeric value passes through floating point.
func (self *snEpochNoId) UnmarshalJSON(data []byte) error {
	value := int64(*self)
	if len(data) != 0 && data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return fmt.Errorf("decode epoch no_id: %w", err)
		}
		var err error
		value, err = strconv.ParseInt(text, 10, 64)
		if err != nil {
			return fmt.Errorf("decode epoch no_id: %w", err)
		}
		if strconv.FormatInt(value, 10) != text {
			return fmt.Errorf("decode epoch no_id: expected canonical decimal integer")
		}
	} else if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode epoch no_id: %w", err)
	}
	*self = snEpochNoId(value)
	return nil
}
