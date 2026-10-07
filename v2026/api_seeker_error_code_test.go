package sdk

import (
	"encoding/json"
	"strings"
	"testing"
)

// The server's Seeker verify error carries a stable code so the apps can show
// a localized reason. Before, the type had only the English message, so the
// json decode dropped the code.
func TestVerifySeekerNftHolderDecodesErrorCode(t *testing.T) {
	serverBody := `{"success":false,"error":{"code":"seeker_token_not_found","message":"Wallet is not a holder of the Seeker or Saga Genesis tokens"}}`
	result := &VerifySeekerNftHolderResult{}
	if err := json.Unmarshal([]byte(serverBody), result); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"code":"seeker_token_not_found"`) {
		t.Fatalf("seeker verify dropped the error code: %s", body)
	}
}
