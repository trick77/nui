package sites

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
)

// compiled is one rule ready to match.
type compiled struct {
	re       *regexp.Regexp
	kinds    []string // the placeholder of each capture group, for the checks a regexp cannot do
	template string
}

var placeholderRE = regexp.MustCompile(`\{([a-z0-9]+)\}`)

// placeholderRegexps are the shapes the regexp admits; typed ones are checked again after the match.
var placeholderRegexps = map[string]string{
	"ipv4": `([0-9.]+)`,
	"ipv6": `([0-9A-Fa-f:.]+)`,
	"cidr": `([0-9A-Fa-f:.]+/[0-9]{1,3})`,
	"int":  `([0-9]+)`,
	"seg":  `([^/]+)`,
	"any":  `(.+)`,
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
	decoded, err := url.PathUnescape(path)
	if err != nil {
		decoded = path
	}
	for _, r := range rules {
		if m := r.re.FindStringSubmatch(decoded); m != nil && typed(r.kinds, m[1:]) {
			return r.template
		}
	}
	return Heuristic(decoded)
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
			if _, err := netip.ParsePrefix(v); err != nil {
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
		t = t[:maxTemplate]
	}
	return t
}

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
	if len(s) > 64 {
		return ":long"
	}
	return s
}
