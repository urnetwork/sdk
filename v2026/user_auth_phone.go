// The login's email or phone number field, checked the same way on every app
// before it is sent (apple#193, android#298).
//
// A phone number is valid when, with its whitespace removed, it is an optional
// "+" and then 2 to 15 digits, the first of them not 0: the length E.164
// allows, and the rule the Apple app has kept (ValidationUtils.isValidPhone).
// The Android app accepted anything its platform's phone pattern took, dashes
// and parentheses included, and the Apple app kept Continue disabled for such
// a number with no reason given. A field that holds a phone number in another
// format (dashes, dots, parentheses, a leading 0, a "+" inside, too few or too
// many digits) now says to check the phone number's format
// (IsInvalidUserAuthPhone), and is not sent.
//
// A valid number's digits are the ASCII ones, as both apps' patterns have read
// them; one typed in other digits (Arabic-Indic, Persian) says to check the
// format too, where it used to say nothing.
package sdk

import (
	"strings"
	"unicode"
)

// the user auth with its whitespace removed, as both apps' fields remove it
func userAuthWithoutWhitespace(userAuth string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, userAuth)
}

// Whether `userAuth` is a phone number the login sends: an optional "+", then
// 2 to 15 digits, the first not 0, whitespace ignored.
// Exported to gomobile and the c abi.
func IsValidUserAuthPhone(userAuth string) bool {
	digits := strings.TrimPrefix(userAuthWithoutWhitespace(userAuth), "+")
	if len(digits) < 2 || 15 < len(digits) || digits[0] == '0' {
		return false
	}
	for i := 0; i < len(digits); i += 1 {
		if digits[i] < '0' || '9' < digits[i] {
			return false
		}
	}
	return true
}

// Whether `userAuth` looks like a phone number and is not one the login sends
// (IsValidUserAuthPhone): digits of any script, with nothing but "+", "-",
// ".", "(", ")" and whitespace among them. The login then says to check the
// phone number's format. An email address, or any other text, is not a phone
// number.
// Exported to gomobile and the c abi.
func IsInvalidUserAuthPhone(userAuth string) bool {
	phone := userAuthWithoutWhitespace(userAuth)
	hasDigit := false
	for _, r := range phone {
		switch {
		case unicode.IsDigit(r):
			hasDigit = true
		case strings.ContainsRune("+-.()", r):
		default:
			return false
		}
	}
	return hasDigit && !IsValidUserAuthPhone(phone)
}
