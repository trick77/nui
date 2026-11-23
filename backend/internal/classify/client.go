package classify

import (
	"regexp"
	"slices"
	"strings"

	"github.com/trick77/stakeout/backend/internal/apix"
)

func looksLikeBrowser(ua string) bool {
	return strings.HasPrefix(ua, "Mozilla/")
}

// isBrowserUA: a browser's user agent that names no tool behind the Mozilla prefix.
func isBrowserUA(ua string) bool { return looksLikeBrowser(ua) && !automationUA.MatchString(ua) }

var productRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9._-]*)(?:/([0-9][0-9A-Za-z._-]*))?`)

// botToken is a product token naming a bot: "PetalBot", "PetalBot/1.0".
var botToken = regexp.MustCompile(`(?i)^[a-z][a-z0-9._-]*bot(/[0-9][0-9a-z._-]*)?$`)

// Client is a short client name for a user agent: browser and OS for a browser, the product token for anything
// else ("curl/8.4.0").
func Client(ua string) string {
	if ua == "" {
		return "no user agent"
	}
	if isBrowserUA(ua) {
		b, o, _ := ParseUA(ua)
		return b + apix.Sep + o
	}
	// A tool behind the Mozilla/5.0 compatibility prefix (Mozilla/5.0 zgrab/0.x, HeadlessChrome, Googlebot): the
	// token naming the tool, not the prefix.
	if looksLikeBrowser(ua) {
		toks := strings.FieldsFunc(ua, func(r rune) bool { return strings.ContainsRune(" ();,+", r) })
		for _, tok := range toks {
			// A contact URL names no tool ("https://babbar.tech/crawler" would read "https").
			if automationUA.MatchString(tok) && !strings.Contains(tok, "://") {
				if m := productRE.FindStringSubmatch(tok); m != nil {
					return strings.TrimSuffix(m[0], "/")
				}
			}
		}
		// A crawler known only by its compatible part or its contact URL ("(compatible; PetalBot;+https://…"): the
		// token naming the bot, else the product after "compatible".
		for _, tok := range toks {
			if botToken.MatchString(tok) {
				return tok
			}
		}
		if i := slices.Index(toks, "compatible"); i >= 0 && i+1 < len(toks) && !strings.Contains(toks[i+1], "://") {
			if m := productRE.FindStringSubmatch(toks[i+1]); m != nil {
				return strings.TrimSuffix(m[0], "/")
			}
		}
	}
	if m := productRE.FindStringSubmatch(ua); m != nil {
		if m[2] != "" {
			return m[1] + "/" + m[2]
		}
		return m[1]
	}
	if len(ua) > 40 {
		return ua[:40]
	}
	return ua
}

// ParseUA reads browser, operating system and device class from a user agent, coarse on purpose.
func ParseUA(ua string) (browser, os, device string) {
	l := strings.ToLower(ua)
	switch {
	case strings.Contains(l, "edg/"):
		browser = "Edge"
	case strings.Contains(l, "opr/") || strings.Contains(l, "opera"):
		browser = "Opera"
	case strings.Contains(l, "samsungbrowser"):
		browser = "Samsung Internet"
	case strings.Contains(l, "firefox/") || strings.Contains(l, "fxios"):
		browser = "Firefox"
	case strings.Contains(l, "chrome/") || strings.Contains(l, "crios"):
		browser = "Chrome"
	case strings.Contains(l, "safari/"):
		browser = "Safari"
	default:
		browser = "Other"
	}
	switch {
	case strings.Contains(l, "iphone") || strings.Contains(l, "ipad") || strings.Contains(l, "ipod"):
		os = "iOS"
	case strings.Contains(l, "android"):
		os = "Android"
	case strings.Contains(l, "windows"):
		os = "Windows"
	case strings.Contains(l, "cros"):
		os = "ChromeOS"
	case strings.Contains(l, "mac os x") || strings.Contains(l, "macintosh"):
		os = "macOS"
	case strings.Contains(l, "linux"):
		os = "Linux"
	default:
		os = "Other"
	}
	switch {
	case strings.Contains(l, "ipad") || strings.Contains(l, "tablet"):
		device = "Tablet"
	case strings.Contains(l, "mobi") || strings.Contains(l, "iphone") || strings.Contains(l, "android"):
		device = "Mobile"
	case browser == "Other" && os == "Other":
		device = "Other"
	default:
		device = "Desktop"
	}
	return browser, os, device
}
