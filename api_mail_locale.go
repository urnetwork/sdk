package sdk

// The reader's time zone and languages, which the api sends with sign-up,
// sign-in and auth-client so the server can place the campaign emails in the
// reader's day and write them in their language (server
// controller/mail_context.go; the campaign review of 2026-11-13, item B).
// Until then only auth-client carried them, one locale at a time, and a
// sign-up carried neither, so a reader was mailed at 10:00 UTC in English
// until their app's next auth-client.
//
// Each app sets them once at start, and again when the system's zone or
// languages change (SetMailLocale), from the platform's own APIs: iOS and
// macOS Locale.preferredLanguages and TimeZone.current, Android's LocaleList
// and TimeZone.getDefault(), Windows' GlobalizationPreferences.Languages and
// its ICU zone, Linux's LANGUAGE, LC_ALL, LC_MESSAGES and LANG with the
// system zone. The api copies them into each request's args that the caller
// left empty, so the flows stay the same on every platform; a value the
// caller set wins. A derived client's auth-client (a window client minted
// from a source client) is transport maintenance and carries none.

import (
	"slices"
	"strings"
)

// apiMailLocaleSettings bound what the api sends.
type apiMailLocaleSettings struct {
	// How many of the device's languages are sent, most preferred first.
	// Default 8: an OS lists a handful; the server keeps as many. Non-positive
	// sends none.
	MaxLocales int
	// The longest language tag sent, in bytes; a longer one is no tag.
	// Default 35, BCP 47's practical bound for a language, script, region
	// and variant.
	MaxLocaleBytes int
}

func defaultApiMailLocaleSettings() *apiMailLocaleSettings {
	return &apiMailLocaleSettings{
		MaxLocales:     8,
		MaxLocaleBytes: 35,
	}
}

// SetMailLocale sets the device's IANA time zone ("America/Chicago"; "" when
// it cannot be read) and its preferred languages, most preferred first, as
// BCP 47 tags ("pt-BR"; "_" is read as "-"). A language the device cannot
// name is left out, never replaced by a default such as "en": an unknown
// language lets the server use the reader's country instead.
func (self *Api) SetMailLocale(timeZone string, locales *StringList) {
	settings := defaultApiMailLocaleSettings()
	values := []string{}
	if locales != nil {
		for i := 0; i < locales.Len() && len(values) < settings.MaxLocales; i += 1 {
			locale := strings.ReplaceAll(strings.TrimSpace(locales.Get(i)), "_", "-")
			if locale == "" || settings.MaxLocaleBytes < len(locale) || slices.Contains(values, locale) {
				continue
			}
			values = append(values, locale)
		}
	}
	timeZone = strings.TrimSpace(timeZone)
	self.setMailLocale(timeZone, values)
}

func (self *Api) setMailLocale(timeZone string, locales []string) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.mailTimeZone = timeZone
	self.mailLocales = slices.Clone(locales)
}

// GetMailLocaleTimeZone is the zone SetMailLocale set, "" when none.
func (self *Api) GetMailLocaleTimeZone() string {
	timeZone, _ := self.getMailLocale()
	return timeZone
}

// GetMailLocales are the languages SetMailLocale set, most preferred first.
func (self *Api) GetMailLocales() *StringList {
	_, locales := self.getMailLocale()
	list := NewStringList()
	for _, locale := range locales {
		list.Add(locale)
	}
	return list
}

func (self *Api) getMailLocale() (timeZone string, locales []string) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.mailTimeZone, slices.Clone(self.mailLocales)
}

// fillMailLocale fills a request's zone and languages from the device's,
// where the caller left them empty.
func (self *Api) fillMailLocale(timeZone *string, locales **StringList) {
	deviceTimeZone, deviceLocales := self.getMailLocale()
	if *timeZone == "" {
		*timeZone = deviceTimeZone
	}
	if (*locales == nil || (*locales).Len() == 0) && 0 < len(deviceLocales) {
		list := NewStringList()
		for _, locale := range deviceLocales {
			list.Add(locale)
		}
		*locales = list
	}
}

// The args of each request that carries the mail locale, filled in a copy so
// the caller's args are not changed.

func (self *Api) networkCreateArgsWithMailLocale(args *NetworkCreateArgs) *NetworkCreateArgs {
	if args == nil {
		return nil
	}
	filled := *args
	self.fillMailLocale(&filled.TimeZone, &filled.Locales)
	return &filled
}

func (self *Api) authLoginArgsWithMailLocale(args *AuthLoginArgs) *AuthLoginArgs {
	if args == nil {
		return nil
	}
	filled := *args
	self.fillMailLocale(&filled.TimeZone, &filled.Locales)
	return &filled
}

func (self *Api) authLoginWithPasswordArgsWithMailLocale(args *AuthLoginWithPasswordArgs) *AuthLoginWithPasswordArgs {
	if args == nil {
		return nil
	}
	filled := *args
	self.fillMailLocale(&filled.TimeZone, &filled.Locales)
	return &filled
}

func (self *Api) authVerifyArgsWithMailLocale(args *AuthVerifyArgs) *AuthVerifyArgs {
	if args == nil {
		return nil
	}
	filled := *args
	self.fillMailLocale(&filled.TimeZone, &filled.Locales)
	return &filled
}

// A derived client's mint is left as it is; a device's also takes its single
// locale, the first of its languages, when the caller set none.
func (self *Api) authNetworkClientArgsWithMailLocale(args *AuthNetworkClientArgs) *AuthNetworkClientArgs {
	if args == nil || args.SourceClientId != nil {
		return args
	}
	filled := *args
	self.fillMailLocale(&filled.TimeZone, &filled.Locales)
	if filled.Locale == "" && filled.Locales != nil && 0 < filled.Locales.Len() {
		filled.Locale = filled.Locales.Get(0)
	}
	return &filled
}
