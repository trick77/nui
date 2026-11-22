// Package refdata holds ipverse's reference data in memory: the bot list (crawlers and uptime probes, with their
// address ranges, user-agent patterns and rDNS patterns), AS ownership of every routed prefix with the AS's name and
// category, and country of every prefix. Refreshed daily from GitHub, cached on disk so a restart without network
// still has the last good copy, and swapped atomically: readers always see one consistent snapshot per source.
//
// The approach follows lens (internal/ingest/botblocks.go, internal/handler/search.go), which cannot be imported.
package refdata

import (
	"net/netip"
	"regexp"
	"strings"
	"time"
)

// Provider is one service of the bot list.
type Provider struct {
	Name string
	// Class is "bot" (crawlers.json) or "monitor" (monitoring.json).
	Class string
	// Authoritative false means the address list is incomplete: an address hit is only a candidate until the
	// address's rDNS matches RdnsPatterns and resolves back (forward-confirmed reverse DNS).
	Authoritative bool
	RdnsPatterns  []string
	// HasRanges is whether the list carries addresses for it at all.
	HasRanges bool
	ua        []*regexp.Regexp
}

// MatchesUA reports whether ua matches one of the provider's user-agent patterns.
func (p *Provider) MatchesUA(ua string) bool {
	for _, re := range p.ua {
		if re.MatchString(ua) {
			return true
		}
	}
	return false
}

// Bots is one load of the bot list.
type Bots struct {
	Providers []*Provider
	table     PrefixTable
	Updated   time.Time
	// Rejected are the ranges left out as implausibly broad.
	Rejected Rejected
}

// ByIP is the provider whose ranges hold a.
func (b *Bots) ByIP(a netip.Addr) (*Provider, bool) {
	if b == nil {
		return nil, false
	}
	i, ok := b.table.Lookup(a)
	if !ok {
		return nil, false
	}
	return b.Providers[i], true
}

// ByUA is the first provider whose user-agent patterns match ua.
func (b *Bots) ByUA(ua string) (*Provider, bool) {
	if b == nil || ua == "" {
		return nil, false
	}
	for _, p := range b.Providers {
		if p.MatchesUA(ua) {
			return p, true
		}
	}
	return nil, false
}

// AS is one autonomous system's metadata.
type AS struct {
	ASN      int64
	Handle   string
	Name     string
	Country  string
	Category string // hosting, isp, business, education_research, government_admin, or ""
}

// Networks is one load of the AS data.
type Networks struct {
	ASes     []AS
	table    PrefixTable
	Updated  time.Time
	Rejected Rejected
}

// Lookup is the AS announcing a.
func (n *Networks) Lookup(a netip.Addr) (AS, bool) {
	if n == nil {
		return AS{}, false
	}
	i, ok := n.table.Lookup(a)
	if !ok {
		return AS{}, false
	}
	return n.ASes[i], true
}

// ByASN is an AS by number.
func (n *Networks) ByASN(asn int64) (AS, bool) {
	if n == nil {
		return AS{}, false
	}
	// ASes is sorted by number at load.
	lo, hi := 0, len(n.ASes)
	for lo < hi {
		m := (lo + hi) / 2
		if n.ASes[m].ASN < asn {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo < len(n.ASes) && n.ASes[lo].ASN == asn {
		return n.ASes[lo], true
	}
	return AS{}, false
}

// Countries is one load of the country data.
type Countries struct {
	Codes    []string
	Names    map[string]string
	table    PrefixTable
	Updated  time.Time
	Rejected Rejected
}

// Lookup is the ISO code of a's country, "" when unknown.
func (c *Countries) Lookup(a netip.Addr) string {
	if c == nil {
		return ""
	}
	i, ok := c.table.Lookup(a)
	if !ok {
		return ""
	}
	return c.Codes[i]
}

// Name is a country's English name, the code itself when unknown.
func (c *Countries) Name(code string) string {
	if c != nil {
		if n, ok := c.Names[code]; ok {
			return n
		}
	}
	return code
}

// NetworkKind maps an AS category to the dashboard's network kind.
func NetworkKind(category string) string {
	switch category {
	case "hosting":
		return "hosting"
	case "isp":
		return "isp"
	case "business":
		return "business"
	case "education_research":
		return "education"
	case "government_admin":
		return "government"
	}
	return ""
}

// globRE compiles a user-agent glob ("*AhrefsBot*"): * is any run, the rest literal, case-insensitive.
func globRE(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("(?i)^")
	for _, part := range strings.Split(pattern, "*") {
		b.WriteString(regexp.QuoteMeta(part))
		b.WriteString(".*")
	}
	s := strings.TrimSuffix(b.String(), ".*") + "$"
	return regexp.Compile(s)
}

// MatchRdnsGlob reports whether host matches a provider's rDNS glob: * is one DNS label (no dots), as in lens, so
// "*.googlebot.com" never matches "x.evil.example.googlebot.com.attacker.net".
func MatchRdnsGlob(host string, patterns []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	for _, p := range patterns {
		if p == "" {
			continue
		}
		var b strings.Builder
		b.WriteString("(?i)^")
		for i, part := range strings.Split(p, "*") {
			if i > 0 {
				b.WriteString(`[^.]*`)
			}
			b.WriteString(regexp.QuoteMeta(part))
		}
		b.WriteString("$")
		if re, err := regexp.Compile(b.String()); err == nil && re.MatchString(host) {
			return true
		}
	}
	return false
}
