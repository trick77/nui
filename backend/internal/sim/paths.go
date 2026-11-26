package sim

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// asset is a static file a page load fetches.
type asset struct {
	path, dest string
	size       int64
}

// pathGen makes a site's raw paths from its path rules: every rule that names a page becomes pages, its
// placeholders filled with plausible values; a site without rules gets a small blog's paths.
type pathGen struct {
	pages   []pageShape
	weights []float64
	assets  []asset
	// lookup: the rules have address placeholders, the lookup site's shape.
	lookup bool
}

// pageShape is one rule's pattern, filled on demand; re matches the paths it fills, by the values fill puts in: a
// path is its own rule's, not another's of one segment as well ("/{ipv4}" and "/{text}").
type pageShape struct {
	pattern string
	re      *regexp.Regexp
}

func newPageShape(pattern string) pageShape {
	var b strings.Builder
	last := 0
	for _, m := range placeholderRE.FindAllStringSubmatchIndex(pattern, -1) {
		b.WriteString(regexp.QuoteMeta(pattern[last:m[0]]))
		switch kind := pattern[m[2]:m[3]]; {
		case kind == "any":
			b.WriteString(`.+`)
		case kind == "int":
			b.WriteString(`[0-9]+`)
		case len(fills[kind]) == 0:
			b.WriteString(`x`)
		default:
			vals := make([]string, len(fills[kind]))
			for i, v := range fills[kind] {
				vals[i] = regexp.QuoteMeta(v)
			}
			b.WriteString(`(?:` + strings.Join(vals, `|`) + `)`)
		}
		last = m[1]
	}
	b.WriteString(regexp.QuoteMeta(pattern[last:]))
	return pageShape{pattern: pattern, re: regexp.MustCompile(`^` + b.String() + `$`)}
}

// Placeholder values. Lookup targets are documentation addresses and networks too; the first of each list is the
// most asked for.
var fills = map[string][]string{
	"ipv4": {"192.0.2.44", "198.51.100.23", "203.0.113.9", "192.0.2.200", "198.51.100.7"},
	"ipv6": {"2001:db8::1", "2001:db8:85a3::8a2e:370:7334", "2001:db8:1f::44"},
	"cidr": {"198.51.100.0%2F24", "198.51.100.0%2F24", "198.51.100.0%2F24", "203.0.113.0%2F24", "192.0.2.0%2F25",
		"2001:db8::%2F32"},
	"text": {"alpine-fibre", "nimbus-cloud", "example-search", "polder-net", "kestrel-hosting"},
	"seg":  {"overview", "members", "billing", "logs"},
	"any":  {"docs/getting-started"},
}

var placeholderRE = regexp.MustCompile(`\{([a-z0-9]+)\}`)

// blogPages are the paths of a site without rules.
var blogPages = []string{"/", "/about", "/blog/hello-world", "/blog/quiet-analytics", "/blog/sqlite-at-scale",
	"/blog/release-notes-1-4", "/blog/why-we-built-it", "/docs/1", "/docs/2", "/pricing"}

func newPathGen(s *Site) *pathGen {
	g := &pathGen{}
	for _, r := range s.Rules {
		switch {
		case strings.HasPrefix(r.Pattern, "/assets/"):
			g.assets = append(g.assets, asset{"/assets/index-B7xk2c9Q.js", "script", 182_000},
				asset{"/assets/index-Dq3m81aF.css", "style", 24_000})
		case r.Pattern == "/favicon.svg" || r.Pattern == "/favicon.ico":
			if !hasAsset(g.assets, "image") {
				g.assets = append(g.assets, asset{r.Pattern, "image", 1_200})
			}
		case fileLike(r.Pattern):
		default:
			g.pages = append(g.pages, newPageShape(r.Pattern))
			w := 2.0
			switch {
			case r.Pattern == "/":
				w = 1.2
			case strings.Contains(r.Pattern, "{ipv4}"):
				w = 3
				g.lookup = true
			case strings.Contains(r.Pattern, "{text}"):
				w = 1
			}
			g.weights = append(g.weights, w)
		}
	}
	if len(g.pages) == 0 {
		for i, p := range blogPages {
			g.pages = append(g.pages, newPageShape(p))
			g.weights = append(g.weights, map[bool]float64{true: 2, false: 1}[i == 0 || strings.HasPrefix(p, "/blog/")])
		}
	}
	if len(g.assets) == 0 {
		g.assets = []asset{{"/static/app-4f9a1c2e.js", "script", 96_000}, {"/static/style-77b0e3d1.css", "style", 18_000},
			{"/favicon.ico", "image", 4_300}}
	}
	return g
}

func hasAsset(as []asset, dest string) bool {
	for _, a := range as {
		if a.dest == dest {
			return true
		}
	}
	return false
}

// fileLike is a pattern naming a file (robots.txt, a manifest): fetched by machines and browsers, never a page.
func fileLike(p string) bool {
	last := p[strings.LastIndexByte(p, '/')+1:]
	return strings.Contains(last, ".") && !strings.Contains(last, "{")
}

// page draws a page path.
func (g *pathGen) page(r rng) string {
	return g.fill(g.pages[r.weighted(g.weights)].pattern, r)
}

// speed is how slow a page of path loads, by the rule it was made from: a factor from 0.6 to 2 on the vitals.
func (g *pathGen) speed(path string) float64 { return shapeSpeed(g.shape(path)) }

// shapeSpeed is the speed of the pages of a rule's pattern.
func shapeSpeed(shape string) float64 { return 0.6 + float64(strHash(shape)%1000)/1000*1.4 }

// shape is the rule's pattern a page of path was made from, the path itself when none.
func (g *pathGen) shape(path string) string {
	for _, p := range g.pages {
		if p.re.MatchString(path) {
			return p.pattern
		}
	}
	return path
}

// spikePage is the page a news post links: the lookup site's most asked address, or the first page.
func (g *pathGen) spikePage() string {
	for _, p := range g.pages {
		if strings.Contains(p.pattern, "{ipv4}") {
			return strings.Replace(p.pattern, "{ipv4}", fills["ipv4"][0], 1)
		}
	}
	return g.fill(g.pages[len(g.pages)-1].pattern, newRng(0))
}

func (g *pathGen) fill(pattern string, r rng) string {
	return placeholderRE.ReplaceAllStringFunc(pattern, func(m string) string {
		kind := m[1 : len(m)-1]
		if kind == "int" {
			if strings.Contains(pattern, "AS{int}") {
				return strconv.Itoa(64496 + r.IntN(14))
			}
			return strconv.Itoa(1 + r.IntN(400))
		}
		vals := fills[kind]
		if len(vals) == 0 {
			return "x"
		}
		// The first values are asked for more often: a lookup site's top pages.
		i := min(r.IntN(len(vals)), r.IntN(len(vals)))
		return vals[i]
	})
}

// title is a page's document title.
func (g *pathGen) title(path, product string) string {
	if path == "/" {
		return product
	}
	p, err := url.PathUnescape(strings.TrimPrefix(path, "/"))
	if err != nil {
		p = path
	}
	return p + " · " + product
}

// api is the call a page makes to its site's API and the route the backend names it by; none for the start page
// and for sites that are no single-page app.
func (g *pathGen) api(path string) (string, string) {
	if path == "/" || !g.lookup {
		return "", ""
	}
	return "/api/v1/lookup" + path, "GET /api/v1/lookup/{q}"
}
