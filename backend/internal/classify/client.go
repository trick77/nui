package classify

import (
	"regexp"
	"strings"
)

func looksLikeBrowser(ua string) bool {
	return strings.HasPrefix(ua, "Mozilla/")
}

var productRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9._-]*)(?:/([0-9][0-9A-Za-z._-]*))?`)

// Client is a short client name for a user agent: browser and OS for a browser, the product token for anything
// else ("curl/8.4.0").
func Client(ua string) string {
	if ua == "" {
		return "no user agent"
	}
	if looksLikeBrowser(ua) && !automationUA.MatchString(ua) {
		b, o, _ := ParseUA(ua)
		return b + " · " + o
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
