// Request-local discovery diagnostics retain only typed, fixed categories.
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"

	"github.com/urnetwork/connect"
)

// Never inspect error text, response bodies, identities, or authentication data.
func authDiscoveryResultCategory(result *AuthLoginResult, err error) string {
	if err != nil {
		var statusErr *connect.HttpStatusError
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		var networkErr net.Error
		switch {
		case errors.Is(err, context.Canceled):
			return "canceled"
		case errors.Is(err, context.DeadlineExceeded):
			return "timeout"
		case errors.As(err, &statusErr):
			switch {
			case statusErr.StatusCode == http.StatusUnauthorized || statusErr.StatusCode == http.StatusForbidden:
				return "http_auth"
			case statusErr.StatusCode == http.StatusTooManyRequests:
				return "http_rate"
			case 400 <= statusErr.StatusCode && statusErr.StatusCode < 500:
				return "http_client"
			case 500 <= statusErr.StatusCode && statusErr.StatusCode < 600:
				return "http_server"
			default:
				return "http_other"
			}
		case errors.As(err, &syntaxErr), errors.As(err, &typeErr):
			return "decode"
		case errors.Is(err, errApiRequestFailed):
			return "request_panic"
		case errors.Is(err, errApiRequestReturnedWithoutCallback):
			return "request_no_callback"
		case errors.As(err, &networkErr):
			if networkErr.Timeout() {
				return "timeout"
			}
			return "network"
		default:
			return "request_unknown"
		}
	}
	if result == nil {
		return "result_invalid"
	}
	if result.Error != nil {
		return "api_refusal"
	}
	if result.AuthAllowed != nil {
		if result.AuthAllowed.Contains("password") {
			return "password_allowed"
		}
		return "password_not_allowed"
	}
	return "no_auth_methods"
}
