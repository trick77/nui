// Package refdata holds ipverse's reference data in memory: the bot list (crawlers and uptime probes, with their
// address ranges, user-agent patterns and rDNS patterns), AS ownership of every routed prefix with the AS's name and
// category, and country of every prefix. Refreshed daily from GitHub, cached on disk so a restart without network
// still has the last good copy, and swapped atomically: readers always see one consistent snapshot per source.
//
// The approach follows lens (internal/ingest/botblocks.go, internal/handler/search.go), which cannot be imported.
package refdata

import (
	"cmp"
	"net/netip"
	"regexp"
	"slices"
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
	// RdnsPatterns keeps the refused globs too, which match nothing: a service without a usable one is not one that
	// names none.
	RdnsPatterns []string
	// HasRanges is whether the list carries addresses for it at all, refused ones included: a service whose ranges
	// cannot be used is not one known by its user agent alone.
	HasRanges bool
	// Ranges are the addresses the list carries, the refused ones left out.
	Ranges []netip.Prefix
	// Agents are the user-agent tokens the vendor declares within the service, each with the purpose it declares for
	// it. They tell apart what one service's addresses do; which service a user agent names is user_agent_patterns'.
	// nil when there are none, or none was kept.
	Agents []Agent
	ua     []uaPattern
	// agentUA are the agents' patterns in list order, those that name none included.
	agentUA []agentUA
}

// agentUA is one agent pattern and its agent's index in Agents, -1 for one that names none: its agent's left out, or
// it is a repeat of the agent's name declaring another purpose.
type agentUA struct {
	ua    uaPattern
	agent int
}

// Agent is one user-agent token a service declares.
type Agent struct {
	Name string
	// Purpose is the vendor's word for what the agent does ("ai-training", "ai-search"), as the list spells it: no
	// closed list, "" when it declares none usable.
	Purpose string
}

// Agent is the agent whose pattern matches ua, the longest pattern's, as ByUA decides between services; of two kept
// agents' patterns of one length the first listed. None when the longest is a pattern that names none, also when a
// kept agent's pattern of that length matches too, whatever the order: a shorter pattern's or a sibling's agent would
// be a guess.
func (p *Provider) Agent(ua string) (Agent, bool) {
	if p == nil {
		return Agent{}, false
	}
	best, length := -1, 0
	for _, u := range p.agentUA {
		if u.ua.length < length || (u.ua.length == length && (best < 0 || u.agent >= 0)) || !u.ua.re.MatchString(ua) {
			continue
		}
		best, length = u.agent, u.ua.length
	}
	if best < 0 {
		return Agent{}, false
	}
	return p.Agents[best], true
}

// uaPattern is one user-agent glob and how much of a user agent it states: its length without the wildcards.
type uaPattern struct {
	re     *regexp.Regexp
	length int
}

// uaLength is the length of the provider's longest pattern matching ua, 0 when none does.
func (p *Provider) uaLength(ua string) int {
	n := 0
	for _, u := range p.ua {
		if u.length > n && u.re.MatchString(ua) {
			n = u.length
		}
	}
	return n
}

// Bots is one load of the bot list.
type Bots struct {
	Providers []*Provider
	// table maps a prefix to its owners: the providers listing it, then those listing a wider one around it.
	table   PrefixTable
	owners  [][]*Provider
	Updated time.Time
	// Rejected is what was left out as implausibly broad or unreadable.
	Rejected Rejected
	// RejectedAgents is what of the agents was left out: of no use, of a name taken, a repeat of another purpose, of a
	// service's second listing. Apart from Rejected: agents never crowd a refused range out of its first few.
	RejectedAgents Rejected
	names          map[string]Named
	// own are the operator's probes (Own), apart from Providers and the table.
	own []*Provider
	// clashed are the probes named like a name of the list's, which keeps its meaning (withOwn).
	clashed []string
}

// Named is what a service's or an agent's name stands for.
type Named struct {
	// Service is the service's name, the agent's service for an agent.
	Service string
	Class   string
	// Purpose is the agent's, for a service the one all of its agents in both listings share, "" when they differ or
	// there are none.
	Purpose string
	// Declared is whether the service's agents differ in purpose, those of both listings and those left out counted:
	// the purpose then rests on the user agent alone, the address list cannot tell them apart.
	Declared bool
	// Group is the purpose's group (PurposeGroup), for a service the one all its purposes share, those of both
	// listings and those left out counted: "" when they differ or one has none.
	Group string
	// Agent is whether the name is an agent's. An agent named like its own service is the agent.
	Agent bool
}

// PurposeGroup is a purpose's group, its part before the first "-" ("ai" of "ai-training"), "" without one.
func PurposeGroup(purpose string) string {
	g, _, _ := strings.Cut(purpose, "-")
	return g
}

// sharedGroup is the group every purpose of a set has, "" when they differ, one has none or there are none.
func sharedGroup(purposes map[string]bool) string {
	shared := ""
	for pu := range purposes {
		g := PurposeGroup(pu)
		if g == "" || (shared != "" && g != shared) {
			return ""
		}
		shared = g
	}
	return shared
}

// ByName is what a service's or an agent's name stands for, spelled exactly as the list's first spelling: services
// are known by their exact name throughout. An agent named like a name before it but for case is no other name.
func (b *Bots) ByName(name string) (Named, bool) {
	if b == nil {
		return Named{}, false
	}
	n, ok := b.names[name]
	return n, ok
}

// ByIP are the providers whose ranges hold a, the narrowest range first, for the same range the one listed last (a
// monitor before a crawler). Services share addresses (GPTBot and OAI-SearchBot do), so the address alone need not
// name one: Rank decides with the user agent.
func (b *Bots) ByIP(a netip.Addr) []*Provider {
	if b == nil {
		return nil
	}
	i, ok := b.table.Lookup(a)
	if !ok {
		return nil
	}
	return b.owners[i]
}

// ByUA is the provider whose user-agent patterns match ua, the longest pattern's: one service's "*Synthetics*" also
// matches another's "DatadogSynthetics".
func (b *Bots) ByUA(ua string) (*Provider, bool) {
	if b == nil {
		return nil, false
	}
	return longestUA(b.Providers, ua)
}

// Claimed is the provider ua names for a client at a: one whose ranges hold a before any other.
func (b *Bots) Claimed(a netip.Addr, ua string) (*Provider, bool) {
	if p, ok := longestUA(b.ByIP(a), ua); ok {
		return p, true
	}
	return b.ByUA(ua)
}

func longestUA(ps []*Provider, ua string) (*Provider, bool) {
	var best *Provider
	length := 0
	for _, p := range ps {
		if n := p.uaLength(ua); n > length {
			best, length = p, n
		}
	}
	return best, best != nil
}

// Rank orders an address's owners by how far they name who is behind it: an authoritative list first, then the one
// the earliest of uas names, then the narrowest range.
func Rank(owners []*Provider, uas ...string) []*Provider {
	if len(owners) < 2 {
		return owners
	}
	ranked := make([]*Provider, 0, len(owners))
	for _, authoritative := range []bool{true, false} {
		// Each kind on its own: a pattern of the other kind must not keep one of this kind from being named.
		kind := slices.DeleteFunc(slices.Clone(owners), func(p *Provider) bool { return p.Authoritative != authoritative })
		named := map[*Provider]int{}
		for i, ua := range uas {
			if p, ok := longestUA(kind, ua); ok {
				named[p] = max(named[p], len(uas)-i)
			}
		}
		slices.SortStableFunc(kind, func(a, b *Provider) int { return cmp.Compare(named[b], named[a]) })
		ranked = append(ranked, kind...)
	}
	return ranked
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
	AS       []AS
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
	return n.AS[i], true
}

// ByASN is an AS by number.
func (n *Networks) ByASN(asn int64) (AS, bool) {
	if n == nil {
		return AS{}, false
	}
	// AS is sorted by number at load.
	lo, hi := 0, len(n.AS)
	for lo < hi {
		m := (lo + hi) / 2
		if n.AS[m].ASN < asn {
			lo = m + 1
		} else {
			hi = m
		}
	}
	if lo < len(n.AS) && n.AS[lo].ASN == asn {
		return n.AS[lo], true
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
	return regexp.Compile("(?i)^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$")
}

// The two readings of an rDNS glob's *. Any run, as in any glob, for a client that claims to be the provider inside its
// ranges: the list says "*.yandex.com", Yandex's crawlers are "5-255-231-1.spider.yandex.com". One label for any other:
// deeper names also are the company's offices, its customers' machines and mail.
const (
	anyRun   = ".*"
	oneLabel = "[^.]*"
)

// matchRdns reports whether host matches one of a provider's rDNS globs, an implausible one never. The glob's end is
// the name's end, so "*.googlebot.com" never matches "x.googlebot.com.attacker.net".
func matchRdns(host string, globs []string, star string) bool {
	host = strings.TrimSuffix(host, ".")
	for _, g := range globs {
		if !plausibleRdns(g) {
			continue
		}
		re, err := regexp.Compile("(?i)^" + strings.ReplaceAll(regexp.QuoteMeta(g), `\*`, star) + "$")
		if err == nil && re.MatchString(host) {
			return true
		}
	}
	return false
}

// plausibleRdns: the glob ends in a whole domain of its own, two labels after its last * at least ("*.googlebot.com",
// "*-bot.example.com"). "*.com", "*example.com" or "crawler.*" would match names anybody can register.
func plausibleRdns(glob string) bool {
	tail := "." + glob
	if i := strings.LastIndex(glob, "*"); i >= 0 {
		tail = glob[i+1:]
	}
	_, domain, _ := strings.Cut(tail, ".")
	labels := strings.Split(domain, ".")
	return len(labels) >= 2 && !slices.Contains(labels, "")
}
