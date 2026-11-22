package sites

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// compiled is one rule ready to match.
type compiled struct {
	re       *regexp.Regexp
	kinds    []string // the placeholder of each capture group, for the checks a regexp cannot do
	template string
}

var placeholderRE = regexp.MustCompile(`\{([a-z0-9]+)\}`)

// placeholderRegexps are the shapes the regexp admits; typed ones are checked again after the match. A path is
// matched decoded segment by segment, an encoded slash kept as %2F inside its segment: {cidr} takes either form.
var placeholderRegexps = map[string]string{
	"ipv4": `([0-9.]+)`,
	"ipv6": `([0-9A-Fa-f:.]+)`,
	"cidr": `([0-9A-Fa-f:.]+(?:/|%2F)[0-9]{1,3})`,
	"int":  `([0-9]+)`,
	"seg":  `([^/]+)`,
	"text": `([^/]+)`,
	"any":  `(.+)`,
}

// fileExts are the extensions that make a segment a file name, not free text: what scanners probe for and what a
// site serves statically.
var fileExts = map[string]bool{
	"php": true, "asp": true, "aspx": true, "jsp": true, "cgi": true, "pl": true, "env": true, "sql": true,
	"bak": true, "old": true, "zip": true, "gz": true, "tgz": true, "tar": true, "rar": true, "7z": true,
	"xml": true, "json": true, "txt": true, "ini": true, "conf": true, "cfg": true, "yml": true, "yaml": true,
	"log": true, "html": true, "htm": true, "js": true, "mjs": true, "css": true, "map": true, "ico": true,
	"png": true, "jpg": true, "jpeg": true, "gif": true, "svg": true, "webp": true, "woff": true, "woff2": true,
	"ttf": true, "webmanifest": true, "git": true, "svn": true, "ds_store": true, "action": true, "do": true,
}

// isFileName reports whether a segment names a file: a dot file (.env, .git) or a known extension.
func isFileName(s string) bool {
	if strings.HasPrefix(s, ".") {
		return true
	}
	i := strings.LastIndexByte(s, '.')
	return i >= 0 && fileExts[strings.ToLower(s[i+1:])]
}

// maxTemplate bounds a template, the dashboard's list key.
const maxTemplate = 200

func compileRules(rules []Rule) ([]compiled, error) {
	out := make([]compiled, 0, len(rules))
	for i, r := range rules {
		if !strings.HasPrefix(r.Pattern, "/") || r.Template == "" || len(r.Template) > maxTemplate {
			return nil, fmt.Errorf("path rule %d: a pattern starts with / and a template is 1-%d bytes", i, maxTemplate)
		}
		var b strings.Builder
		b.WriteString(`(?i)^`)
		var kinds []string
		rest := r.Pattern
		for {
			loc := placeholderRE.FindStringSubmatchIndex(rest)
			if loc == nil {
				b.WriteString(regexp.QuoteMeta(rest))
				break
			}
			b.WriteString(regexp.QuoteMeta(rest[:loc[0]]))
			kind := rest[loc[2]:loc[3]]
			re, ok := placeholderRegexps[kind]
			if !ok {
				return nil, fmt.Errorf("path rule %d: unknown placeholder {%s}", i, kind)
			}
			b.WriteString(re)
			kinds = append(kinds, kind)
			rest = rest[loc[1]:]
		}
		b.WriteString(`$`)
		re, err := regexp.Compile(b.String())
		if err != nil {
			return nil, fmt.Errorf("path rule %d: %w", i, err)
		}
		out = append(out, compiled{re: re, kinds: kinds, template: r.Template})
	}
	return out, nil
}

// Template is the template a raw browser path is listed under: the first matching rule's, else the heuristic's.
func (s *Site) Template(path string) string {
	return templateOf(s.rules, path)
}

func templateOf(rules []compiled, path string) string {
	if path == "" {
		path = "/"
	}
	decoded := decodePath(path)
	for _, r := range rules {
		if m := r.re.FindStringSubmatch(decoded); m != nil && typed(r.kinds, m[1:]) {
			return r.template
		}
	}
	return Heuristic(decoded)
}

// decodePath unescapes a raw path segment by segment. An encoded slash stays %2F inside its segment, so a search
// term holding a slash is one segment, not two; a segment with a bad escape stays as it came. Every byte that is no
// UTF-8 (%FF) becomes U+FFFD: a template is a list key the dashboard sends back, and only valid UTF-8 survives the
// round trip through JSON unchanged.
func decodePath(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if d, err := url.PathUnescape(s); err == nil {
			segs[i] = strings.ReplaceAll(d, "/", "%2F")
		}
		segs[i] = strings.ToValidUTF8(segs[i], "�")
	}
	return strings.Join(segs, "/")
}

// typed checks what the regexp only shaped: that an {ipv4} is an IPv4 address, and so on.
func typed(kinds, values []string) bool {
	for i, k := range kinds {
		v := values[i]
		switch k {
		case "ipv4":
			if a, err := netip.ParseAddr(v); err != nil || !a.Is4() {
				return false
			}
		case "ipv6":
			if a, err := netip.ParseAddr(v); err != nil || !a.Is6() {
				return false
			}
		case "cidr":
			if _, err := netip.ParsePrefix(strings.Replace(v, "%2F", "/", 1)); err != nil {
				return false
			}
		case "text":
			if isFileName(v) {
				return false
			}
		}
	}
	return true
}

var (
	uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexRE  = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	intRE  = regexp.MustCompile(`^[0-9]+$`)
	// tokenRE is a long run of letters and digits with digits in it: an opaque id, a base64 token.
	tokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{24,}$`)
)

// maxSegments bounds how much of a path a template keeps; the rest becomes one placeholder.
const maxSegments = 8

// Heuristic turns the segments of a path that look like values (an address, a number, a UUID, a hash) into
// placeholders, so one route does not become thousands of list rows.
func Heuristic(path string) string {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	out := make([]string, 0, len(segs))
	for i, s := range segs {
		if i == maxSegments {
			out = append(out, ":rest")
			break
		}
		out = append(out, segment(s))
	}
	t := "/" + strings.Join(out, "/")
	if len(t) > maxTemplate {
		// Cut on a character boundary: a split character would not survive the dashboard's round trip.
		n := maxTemplate
		for n > 0 && !utf8.RuneStart(t[n]) {
			n--
		}
		t = t[:n]
	}
	return t
}

// hashedFileRE is a build tool's file name: a name, a content hash, an extension (index-a41cB3xZ.js).
var hashedFileRE = regexp.MustCompile(`^(.+)-([A-Za-z0-9_]{4,})(\.[A-Za-z0-9]+)$`)

// segment keeps a literal segment and replaces one that looks like a value: an address, a number, a UUID, a hash,
// an opaque id, a build hash inside a file name.
func segment(s string) string {
	if s == "" {
		return s
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return ":ip"
	}
	switch {
	case intRE.MatchString(s):
		return ":n"
	case uuidRE.MatchString(s):
		return ":uuid"
	case hexRE.MatchString(s):
		return ":hash"
	case tokenRE.MatchString(s) && strings.ContainsAny(s, "0123456789"):
		return ":id"
	}
	if m := hashedFileRE.FindStringSubmatch(s); m != nil && strings.ContainsAny(m[2], "0123456789") &&
		strings.ContainsFunc(m[2], unicode.IsLetter) {
		return m[1] + "-:hash" + m[3]
	}
	if len(s) > 64 {
		return ":long"
	}
	return s
}
