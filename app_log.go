// App diagnostic lines in the sdk's own glog files, which are the logs a user
// uploads with feedback.
package sdk

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/urnetwork/glog"
)

// The bounds of one app log line (see LogAppInfo), in bytes.
const (
	appLogTagMaxLength     = 32
	appLogMessageMaxLength = 1024
)

// Writes one line from the app into this process's glog INFO log.
//
// The apps log to the platform log (android logcat, apple os_log), but the log
// files the sdk uploads when the user sends feedback with logs
// (DeviceLocal.UploadLogs) are only glog's. A diagnostic line the app wants in
// those files goes through here as well.
//
// The line is "[app][<tag>] <message>", at the default verbosity, so it is
// written at every level. The [app] prefix keeps app lines apart from the sdk's
// own. The line is bounded and sanitized so that a caller can neither split it
// nor forge other lines: line breaks and other control characters in the
// message become spaces, invalid utf-8 is replaced, the message is cut to 1024
// bytes (ending in "…" when cut), and the tag keeps only ascii letters, digits,
// '.', '_' and '-', at most 32 of them. Write a multi-line block one line per
// call.
//
// It writes only what the caller passes, and it is meant for occasional
// diagnostic lines, not as an app's logger. Safe to call from any thread.
// Exported to gomobile and the c abi.
func LogAppInfo(tag string, message string) {
	glog.Info(appLogLine(tag, message))
}

// The line LogAppInfo writes.
func appLogLine(tag string, message string) string {
	// the tag keeps only ascii letters, digits, '.', '_' and '-', at most
	// appLogTagMaxLength of them
	var tagBuilder strings.Builder
	for i := 0; i < len(tag) && tagBuilder.Len() < appLogTagMaxLength; i += 1 {
		c := tag[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
			c == '.' || c == '_' || c == '-' {
			tagBuilder.WriteByte(c)
		}
	}
	tag = tagBuilder.String()

	message = strings.Map(func(r rune) rune {
		// U+2028 and U+2029 are not control characters, but viewers break
		// lines on them
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(message, string(utf8.RuneError)))
	if appLogMessageMaxLength < len(message) {
		// cut on a rune boundary, leaving room for the marker
		cut := appLogMessageMaxLength - len("…")
		for 0 < cut && !utf8.RuneStart(message[cut]) {
			cut -= 1
		}
		message = message[:cut] + "…"
	}

	if tag == "" {
		return "[app] " + message
	}
	return "[app][" + tag + "] " + message
}
