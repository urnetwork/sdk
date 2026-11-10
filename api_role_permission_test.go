package sdk

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/urnetwork/connect"
)

// The server's role refusal is a 403 with its own message, recognized through
// wrapping, and never a sign-out cause; a 401, another 403 and another error
// are not it.
func TestRolePermissionRefusedIsAForbiddenWithItsMessage(t *testing.T) {
	refusal := &connect.HttpStatusError{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: []byte(RolePermissionRefusedMessage + "\n")}
	connect.AssertEqual(t, RolePermissionRefused(refusal), true)
	connect.AssertEqual(t, RolePermissionRefused(fmt.Errorf("network members: %w", refusal)), true)
	connect.AssertEqual(t, confirmedRejectionCause(refusal), "")

	for _, err := range []error{
		&connect.HttpStatusError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Body: []byte(RolePermissionRefusedMessage)},
		&connect.HttpStatusError{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: []byte("A sidecar token cannot call this route.")},
		errors.New(RolePermissionRefusedMessage),
		nil,
	} {
		if RolePermissionRefused(err) {
			t.Errorf("%v read as the role refusal", err)
		}
	}
}
