package alert

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// defuser maps what a chat, a mail client or a Markdown renderer would act on to look-alikes that do nothing: a
// path, a referrer or an error message is the outside world's text, and it lands in Slack and mail. No link but
// the dashboard's own, no mention, no formatting.
var defuser = strings.NewReplacer(
	"://", "∶//",
	"@", "＠",
	"`", "'",
	"*", "∗",
	"_", "＿",
	"~", "～",
	"[", "［",
	"]", "］",
	"<", "‹",
	">", "›",
	"|", "｜",
	"#", "＃",
	"\\", "＼",
)

// Defuse is s as safe plain text of at most n runes: on one line, control and format characters out, what a
// renderer acts on defused, clipped with an ellipsis. Every text from outside goes through it before it is stored
// or sent.
func Defuse(s string, n int) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == utf8.RuneError:
			continue
		case unicode.IsSpace(r) || unicode.IsControl(r):
			space = b.Len() > 0
			continue
		case unicode.Is(unicode.Cf, r): // zero-width and direction marks: text that reads otherwise than it is
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return Clip(defuser.Replace(b.String()), n)
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
