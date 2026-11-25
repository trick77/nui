package alert

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Line is s on one line of at most n runes: whitespace runs one space, control and invisible format characters out
// (a zero-width space, a direction override reads otherwise than it is). What an alert stores and the dashboard
// shows as text: the outside world's text as it came, readable and copyable. A message defuses it on its way out
// (toolkit/notify's Plain, internal/alert/dispatch): a chat or a mail client would act on it.
func Line(s string, n int) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == utf8.RuneError:
			continue
		case unicode.IsSpace(r) || unicode.IsControl(r):
			space = b.Len() > 0
			continue
		case unicode.Is(unicode.Cf, r):
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return Clip(b.String(), n)
}

// Clip cuts s to at most n runes, an ellipsis for what is cut.
func Clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:max(n-1, 0)])) + "…"
}

// ClipKey cuts a key (raw: it is the alert's identity and the dashboard's filter) to at most n bytes on a rune
// boundary, deterministically, so the same key always clips alike.
func ClipKey(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// urlRE is anything that looks like a URL, with or without its scheme's slashes (mailto:, a token's host).
var urlRE = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://\S*|\b[a-z][a-z0-9+.-]*:[^\s@/]*@\S*`)

// Scrub cuts every URL out of an error's text: a service's error quotes its URL, token included.
func Scrub(s string) string {
	return urlRE.ReplaceAllString(s, "[url]")
}
