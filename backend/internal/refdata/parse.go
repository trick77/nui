package refdata

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Bounds on what a (compromised or broken) upstream can make the loader read.
const (
	maxEntryBytes        = 16 << 20 // one JSON file inside a tarball
	maxDecompressedBytes = 2 << 30  // one whole tarball, decompressed
)

// The shortest prefixes a list may name, per family. One broader entry would swallow everything: 0.0.0.0/0 in the
// bot list made every address that bot. A bot operator's block is at most a /8 (IPv4), its IPv6 ranges stay within
// an organisation's allocation (/20 and longer). AS and country data aggregate adjacent blocks: the real files hold
// IPv4 /7s (AS749 28.0.0.0/7; US 6.0.0.0/7, 20.0.0.0/7, 214.0.0.0/7, measured 2026-11), so /6 there, and IPv6 within
// an RIR's allocation (/12). An IPv4-mapped IPv6 prefix is refused too: it would stand for IPv4 space under
// another name.
const (
	botMinV4, botMinV6 = 8, 20
	netMinV4, netMinV6 = 6, 12
)

func plausible(p netip.Prefix, v4, v6 int) bool {
	switch {
	case p.Addr().Is4():
		return p.Bits() >= v4
	case p.Addr().Is4In6():
		return false
	}
	return p.Bits() >= v6
}

// Rejected is what a load refused: how many entries, the first few for the log.
type Rejected struct {
	Count int
	First []string
}

func (r *Rejected) add(s string) {
	r.Count++
	if len(r.First) < 5 {
		r.First = append(r.First, s)
	}
}

// botFile mirrors ipverse/bot-ip-blocks crawlers.json and monitoring.json. ip_list_authoritative is a pointer so a
// feed that leaves it out reads as non-authoritative, the safe side (verify by rDNS). The services stay raw so one
// entry that does not parse costs that service, not every service added since.
type botFile struct {
	Services map[string]json.RawMessage `json:"services"`
}

type botEntry struct {
	IPv4                []string `json:"ipv4"`
	IPv6                []string `json:"ipv6"`
	IPListAuthoritative *bool    `json:"ip_list_authoritative"`
	RdnsPatterns        []string `json:"rdns_patterns"`
	UserAgentPatterns   []string `json:"user_agent_patterns"`
	// Agents stays raw: it is read on its own, so a value of an unexpected shape costs the service its agents, never
	// the service.
	Agents json.RawMessage `json:"agents"`
}

// agentEntry is one element of a service's agents. Each field stays raw and is read on its own: a name or a pattern of
// the wrong type still leaves the purpose, a purpose that is no string blanks the purpose, not the agent.
type agentEntry struct {
	Name    json.RawMessage `json:"name"`
	Pattern json.RawMessage `json:"pattern"`
	Purpose json.RawMessage `json:"purpose"`
}

// agentDecl is one agent as the list declares it, before the names are settled. named is false for a name that is
// none, ua.re nil for a pattern that is none.
type agentDecl struct {
	name, purpose string
	named         bool
	ua            uaPattern
}

// maxAgentName bounds an agent's name, a token out of a user agent. A longer one is no token.
const maxAgentName = 64

var purposeRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// rawString is a raw JSON value as a trimmed string, false for one that is no string.
func rawString(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return strings.TrimSpace(s), true
}

// uaGlob compiles a user-agent glob of the list, false for one that does not compile or is only wildcards: a bare *
// would match every user agent.
func uaGlob(g string) (uaPattern, bool) {
	if strings.Trim(g, "*") == "" {
		return uaPattern{}, false
	}
	re, err := globRE(g)
	if err != nil {
		return uaPattern{}, false
	}
	return uaPattern{re: re, length: len(strings.ReplaceAll(g, "*", ""))}, true
}

// parseAgents reads a service's agents, each element that is an object: its purpose counts towards the service's even
// when the agent is of no use. A value that is no array is none, an element that is no object is skipped. A name that
// is no string, empty, over-long or holding a rune that does not print (a control, zero-width or bidi format
// character, a space other than U+0020: two names that render alike would be two names) is none, so is a pattern that
// is no usable glob; a purpose that is no string or not a plain lower-case word is "". What is of no use lands in rej,
// never failing the service.
func parseAgents(service string, raw json.RawMessage, rej *Rejected) []agentDecl {
	var elems []json.RawMessage
	if len(raw) == 0 {
		return nil
	}
	if json.Unmarshal(raw, &elems) != nil {
		rej.add(service + " (unreadable agents)")
		return nil
	}
	var agents []agentDecl
	for _, e := range elems {
		var a agentEntry
		if string(e) == "null" || json.Unmarshal(e, &a) != nil {
			rej.add(service + " (unreadable agent)")
			continue
		}
		var d agentDecl
		if purpose, ok := rawString(a.Purpose); ok && purposeRE.MatchString(strings.ToLower(purpose)) {
			d.purpose = strings.ToLower(purpose)
		}
		name, ok := rawString(a.Name)
		d.named = ok && name != "" && len(name) <= maxAgentName && !strings.ContainsFunc(name, notPrint)
		if d.named {
			d.name = name
		} else {
			if !ok {
				name = string(a.Name)
			}
			rej.add(fmt.Sprintf("%s %.50q (agent name)", service, name))
		}
		if g, ok := rawString(a.Pattern); ok {
			d.ua, _ = uaGlob(g) // a blank one is none
		}
		if d.ua.re == nil && d.named {
			rej.add(fmt.Sprintf("%s %.50q (agent pattern)", service, d.name))
		}
		agents = append(agents, d)
	}
	return agents
}

func notPrint(r rune) bool { return !unicode.IsPrint(r) }

// botPrefix reads a range of the bot list, a bare address as itself.
func botPrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if a, err := netip.ParseAddr(s); err == nil {
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	return p.Masked(), err
}

// ParseBots reads both bot lists, for a caller holding the files (tests, a demo).
func ParseBots(crawlers, monitoring []byte) (*Bots, error) { return parseBots(crawlers, monitoring) }

// maxBroken is the largest part of a list's services that may be unusable, as one in maxBroken: above it the list is
// broken, not a service. So is one that mostly has no addresses (a renamed field): it would trust every user agent.
// A choice, not measured.
const maxBroken = 4

// parseBots reads both bot lists (crawlers as bot, monitoring as monitor) into one Bots. A list that does not parse,
// or only in part, is an error: the caller keeps the last good load.
func parseBots(crawlers, monitoring []byte) (*Bots, error) {
	b := &Bots{}
	listing := map[netip.Prefix][]*Provider{}
	agents := map[*Provider][]agentDecl{}
	for _, src := range []struct {
		class string
		data  []byte
	}{{"bot", crawlers}, {"monitor", monitoring}} {
		var f botFile
		if err := json.Unmarshal(src.data, &f); err != nil {
			return nil, fmt.Errorf("%s list: %w", src.class, err)
		}
		if len(f.Services) == 0 {
			return nil, fmt.Errorf("%s list: no services", src.class)
		}
		broken, rangeless := 0, 0
		names := make([]string, 0, len(f.Services))
		for n := range f.Services {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, name := range names {
			var s botEntry
			if json.Unmarshal(f.Services[name], &s) != nil {
				b.Rejected.add(name + " (unreadable entry)")
				broken++
				continue
			}
			p := &Provider{Name: name, Class: src.class, HasRanges: len(s.IPv4)+len(s.IPv6) > 0,
				Authoritative: s.IPListAuthoritative != nil && *s.IPListAuthoritative}
			for _, g := range s.RdnsPatterns {
				g = strings.TrimSuffix(strings.TrimSpace(g), ".")
				p.RdnsPatterns = append(p.RdnsPatterns, g)
				if !plausibleRdns(g) {
					b.Rejected.add(fmt.Sprintf("%s %.50q (rDNS)", name, g))
				}
			}
			for _, g := range s.UserAgentPatterns {
				if ua, ok := uaGlob(g); ok {
					p.ua = append(p.ua, ua)
				}
			}
			// Agents name no service: only user_agent_patterns decide which one a user agent names.
			agents[p] = parseAgents(name, s.Agents, &b.RejectedAgents)
			for _, c := range slices.Concat(s.IPv4, s.IPv6) {
				pfx, err := botPrefix(c)
				switch {
				case err != nil:
					b.Rejected.add(fmt.Sprintf("%s %.50q (unreadable range)", name, c))
				case !plausible(pfx, botMinV4, botMinV6):
					b.Rejected.add(name + " " + pfx.String())
				default:
					listing[pfx] = append(listing[pfx], p)
					p.Ranges = append(p.Ranges, pfx)
				}
			}
			switch {
			case len(p.Ranges) > 0:
			case p.HasRanges || len(p.ua) == 0:
				broken++
			default:
				rangeless++
			}
			b.Providers = append(b.Providers, p)
		}
		if broken*maxBroken > len(names) || (broken+rangeless)*2 > len(names) {
			return nil, fmt.Errorf("%s list: of %d services %d unusable, %d without addresses", src.class, len(names),
				broken, rangeless)
		}
	}
	b.index(listing)
	b.nameAgents(agents)
	return b, nil
}

// nameAgents settles the agents and fills the name index. A name stands for one thing only, its case aside, the first
// spelling's: an agent named like another service, or like an agent of a service before it (crawlers, then monitors,
// each by service name), is left out of its service. One named like its own service is that name's entry, spelled as
// the service. One name twice within a service is one agent, settled at its first named declaration: that one's
// spelling and purpose, kept when any of its declarations has a usable pattern, even when that one has none. A
// repeat's pattern names the agent when the repeat declares the agent's purpose, else none: the user agent then says
// another purpose than the agent's. A service listed twice keeps its first listing's agents and class, the second's
// agents are left out. A service's purposes are those of every agent it declares, in either listing, the ones left
// out included: what its addresses do does not depend on which names survived. A left-out agent's pattern still
// counts in Agent: a user agent it matches best names none rather than a sibling.
func (b *Bots) nameAgents(declared map[*Provider][]agentDecl) {
	services, folded := map[string]*Provider{}, map[string]bool{}
	purposes := map[string]map[string]bool{}
	for _, p := range b.Providers {
		if _, ok := services[p.Name]; !ok {
			services[p.Name] = p
			purposes[p.Name] = map[string]bool{}
		}
		folded[strings.ToLower(p.Name)] = true
		for _, d := range declared[p] {
			purposes[p.Name][d.purpose] = true
		}
	}
	b.names = map[string]Named{}
	claimed := map[string]bool{}
	for _, p := range b.Providers {
		if services[p.Name] != p {
			if len(declared[p]) > 0 {
				b.RejectedAgents.add(p.Name + " (agents of a second listing)")
			}
			continue
		}
		self := strings.ToLower(p.Name)
		patterned := map[string]bool{}
		for _, d := range declared[p] {
			if d.named && d.ua.re != nil {
				patterned[strings.ToLower(d.name)] = true
			}
		}
		own := map[string]int{} // a name's index in Agents once settled, -1 for one left out; "" is no name's
		for _, d := range declared[p] {
			key := strings.ToLower(d.name)
			i, settled := own[key]
			if !settled {
				i = -1
				if d.named && patterned[key] && !claimed[key] && (!folded[key] || key == self) {
					name := d.name
					if key == self {
						name = p.Name
					}
					claimed[key] = true
					i = len(p.Agents)
					p.Agents = append(p.Agents, Agent{Name: name, Purpose: d.purpose})
				}
				own[key] = i
			}
			if d.ua.re == nil {
				continue
			}
			switch {
			case !d.named:
			case i < 0:
				b.RejectedAgents.add(fmt.Sprintf("%s %.50q (agent name taken)", p.Name, d.name))
			case p.Agents[i].Purpose != d.purpose:
				b.RejectedAgents.add(fmt.Sprintf("%s %.50q (agent purpose differs)", p.Name, d.name))
				i = -1
			}
			p.agentUA = append(p.agentUA, agentUA{ua: d.ua, agent: i})
		}
		if p.Agents == nil {
			p.agentUA = nil
		}
		shared, differ := "", len(purposes[p.Name]) > 1
		if len(purposes[p.Name]) == 1 {
			for pu := range purposes[p.Name] {
				shared = pu
			}
		}
		for _, a := range p.Agents {
			b.names[a.Name] = Named{Service: p.Name, Class: p.Class, Purpose: a.Purpose, Declared: differ, Agent: true}
		}
		if _, ok := b.names[p.Name]; !ok {
			b.names[p.Name] = Named{Service: p.Name, Class: p.Class, Purpose: shared, Declared: differ}
		}
	}
}

// index fills the table: each listed prefix stands for its own providers, then those of the prefixes around it.
func (b *Bots) index(listing map[netip.Prefix][]*Provider) {
	for pfx := range listing {
		var owners []*Provider
		for bits := pfx.Bits(); bits > 0; bits-- {
			wide, _ := pfx.Addr().Prefix(bits)
			for _, p := range slices.Backward(listing[wide]) {
				if !slices.Contains(owners, p) {
					owners = append(owners, p)
				}
			}
		}
		b.table.Insert(pfx, int32(len(b.owners))) //nolint:gosec // a few thousand prefixes
		b.owners = append(b.owners, owners)
	}
	b.table.Build()
}

var (
	asEntryRE  = regexp.MustCompile(`^(?:\./)?(?:[^/]+/)*as/([0-9]+)/aggregated\.json$`)
	geoEntryRE = regexp.MustCompile(`^(?:\./)?(?:[^/]+/)*country/([a-z]{2})/([a-z]{2})\.json$`)
)

type asEntry struct {
	ASN      int64 `json:"asn"`
	Metadata struct {
		Handle      string `json:"handle"`
		Description string `json:"description"`
		CountryCode string `json:"countryCode"`
		Category    string `json:"category"`
	} `json:"metadata"`
	Prefixes struct {
		IPv4 []string `json:"ipv4"`
		IPv6 []string `json:"ipv6"`
	} `json:"prefixes"`
}

// parseNetworks streams as-ip-blocks.tar.gz (as/<asn>/aggregated.json per AS) into a Networks, never extracting.
func parseNetworks(r io.Reader) (*Networks, error) {
	n := &Networks{}
	var entries []asEntry
	err := eachTarEntry(r, func(name string, body io.Reader) error {
		if !asEntryRE.MatchString(name) {
			return nil
		}
		var e asEntry
		if json.NewDecoder(body).Decode(&e) != nil {
			return nil //nolint:nilerr // one broken file does not sink the load
		}
		if e.ASN == 0 {
			if m := asEntryRE.FindStringSubmatch(name); m != nil {
				e.ASN, _ = strconv.ParseInt(m[1], 10, 64)
			}
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("as-ip-blocks: no AS files")
	}
	slices.SortFunc(entries, func(a, b asEntry) int { return int(a.ASN - b.ASN) })
	for i, e := range entries {
		n.AS = append(n.AS, AS{ASN: e.ASN, Handle: e.Metadata.Handle, Name: e.Metadata.Description,
			Country: e.Metadata.CountryCode, Category: e.Metadata.Category})
		for _, c := range slices.Concat(e.Prefixes.IPv4, e.Prefixes.IPv6) {
			pfx, err := netip.ParsePrefix(c)
			switch {
			case err != nil:
			case !plausible(pfx, netMinV4, netMinV6):
				n.Rejected.add(fmt.Sprintf("AS%d %s", e.ASN, pfx))
			default:
				n.table.Insert(pfx, int32(i))
			}
		}
	}
	n.table.Build()
	return n, nil
}

type geoEntry struct {
	CountryCode string `json:"countryCode"`
	Country     string `json:"country"`
	Prefixes    struct {
		IPv4 []string `json:"ipv4"`
		IPv6 []string `json:"ipv6"`
	} `json:"prefixes"`
}

// parseCountries streams geo-ip-blocks.tar.gz (country/<cc>/<cc>.json) into a Countries.
func parseCountries(r io.Reader) (*Countries, error) {
	c := &Countries{Names: map[string]string{}}
	err := eachTarEntry(r, func(name string, body io.Reader) error {
		if !geoEntryRE.MatchString(name) {
			return nil
		}
		var e geoEntry
		if json.NewDecoder(body).Decode(&e) != nil || len(e.CountryCode) != 2 {
			return nil //nolint:nilerr // one broken file does not sink the load
		}
		code := strings.ToUpper(e.CountryCode)
		idx := int32(len(c.Codes)) //nolint:gosec // a few hundred countries
		c.Codes = append(c.Codes, code)
		if e.Country != "" {
			c.Names[code] = e.Country
		}
		for _, p := range slices.Concat(e.Prefixes.IPv4, e.Prefixes.IPv6) {
			pfx, err := netip.ParsePrefix(p)
			switch {
			case err != nil:
			case !plausible(pfx, netMinV4, netMinV6):
				c.Rejected.add(code + " " + pfx.String())
			default:
				c.table.Insert(pfx, idx)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(c.Codes) == 0 {
		return nil, errors.New("geo-ip-blocks: no country files")
	}
	c.table.Build()
	return c, nil
}

// eachTarEntry calls fn with every regular file of a gzipped tar, bounded per file and in total.
func eachTarEntry(r io.Reader, fn func(name string, body io.Reader) error) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	total := &limited{r: gz, left: maxDecompressedBytes}
	tr := tar.NewReader(total)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg || h.Size > maxEntryBytes {
			continue
		}
		if err := fn(path.Clean(h.Name), io.LimitReader(tr, maxEntryBytes)); err != nil {
			return err
		}
		if total.left <= 0 {
			return errors.New("tar: decompressed size over the bound")
		}
	}
}

// limited is io.LimitReader that reports the bound as an error instead of a clean EOF, so a decompression bomb
// fails the load rather than ending it early as if complete.
type limited struct {
	r    io.Reader
	left int64
}

func (l *limited) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, errors.New("decompressed size over the bound")
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}
