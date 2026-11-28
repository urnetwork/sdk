package sdk

import (
	"testing"
)

// The phone rule the Apple app has kept (ValidationUtils.isValidPhone,
// ^\+?[1-9]\d{1,14}$), with whitespace ignored as both apps' fields remove
// it. The Android app took any number its platform pattern matched, dashes
// and parentheses included.
func TestIsValidUserAuthPhone(t *testing.T) {
	cases := []struct {
		userAuth string
		want     bool
	}{
		{userAuth: "+15551234567", want: true},
		{userAuth: "15551234567", want: true},
		{userAuth: "5551234567", want: true},
		{userAuth: "+44 7911 123456", want: true},
		{userAuth: " +15551234567 ", want: true},
		{userAuth: "+12", want: true},
		// 15 digits, E.164's most
		{userAuth: "+123456789012345", want: true},
		{userAuth: "+1234567890123456", want: false},
		{userAuth: "+1", want: false},
		{userAuth: "1", want: false},
		{userAuth: "+", want: false},
		{userAuth: "", want: false},
		{userAuth: "+1 (555) 123-4567", want: false},
		{userAuth: "555-123-4567", want: false},
		{userAuth: "555.123.4567", want: false},
		{userAuth: "07911123456", want: false},
		{userAuth: "+07911123456", want: false},
		{userAuth: "1+5551234567", want: false},
		{userAuth: "++15551234567", want: false},
		// the Arabic-Indic digits are not ASCII digits
		{userAuth: "+١٥٥٥١٢٣٤٥٦٧", want: false},
		{userAuth: "someone@example.com", want: false},
	}
	for _, c := range cases {
		if got := IsValidUserAuthPhone(c.userAuth); got != c.want {
			t.Errorf("IsValidUserAuthPhone(%q) = %t, want %t", c.userAuth, got, c.want)
		}
	}
}

// A field that holds a phone number in a format the login does not send says
// to check the format. Before, the Apple app kept Continue disabled with no
// reason, and the Android app sent the number as typed.
func TestIsInvalidUserAuthPhone(t *testing.T) {
	cases := []struct {
		userAuth string
		want     bool
	}{
		{userAuth: "+1 (555) 123-4567", want: true},
		{userAuth: "555-123-4567", want: true},
		{userAuth: "555.123.4567", want: true},
		{userAuth: "(555)1234567", want: true},
		// a national number's trunk 0, where the country code belongs
		{userAuth: "07911123456", want: true},
		{userAuth: "+07911123456", want: true},
		{userAuth: "1+5551234567", want: true},
		{userAuth: "+1234567890123456", want: true},
		{userAuth: "1", want: true},
		{userAuth: "+1", want: true},
		// valid: nothing to say
		{userAuth: "+15551234567", want: false},
		{userAuth: "+44 7911 123456", want: false},
		// not a phone number at all: no digits, or text that is not a phone's
		{userAuth: "", want: false},
		{userAuth: "+", want: false},
		{userAuth: "()-.", want: false},
		{userAuth: "someone@example.com", want: false},
		{userAuth: "1234@example.com", want: false},
		{userAuth: "someone", want: false},
		{userAuth: "555-CALL-NOW", want: false},
		// typed in other digits: a phone number, though not one the login sends
		{userAuth: "+١٥٥٥١٢٣٤٥٦٧", want: true},
		{userAuth: "۰۹۱۲۳۴۵۶۷۸۹", want: true},
	}
	for _, c := range cases {
		if got := IsInvalidUserAuthPhone(c.userAuth); got != c.want {
			t.Errorf("IsInvalidUserAuthPhone(%q) = %t, want %t", c.userAuth, got, c.want)
		}
	}
}
