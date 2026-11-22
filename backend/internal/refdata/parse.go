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
// feed that leaves it out reads as non-authoritative, the safe side (verify by rDNS).
type botFile struct {
	Services map[string]struct {
		IPv4                []string `json:"ipv4"`
		IPv6                []string `json:"ipv6"`
		IPListAuthoritative *bool    `json:"ip_list_authoritative"`
		RdnsPatterns        []string `json:"rdns_patterns"`
		UserAgentPatterns   []string `json:"user_agent_patterns"`
	} `json:"services"`
}

// ParseBots reads both bot lists, for a caller holding the files (tests, a demo).
func ParseBots(crawlers, monitoring []byte) (*Bots, error) { return parseBots(crawlers, monitoring) }

// parseBots reads both bot lists (crawlers as bot, monitoring as monitor) into one Bots. A list that does not parse
// is an error: the caller keeps the last good load.
func parseBots(crawlers, monitoring []byte) (*Bots, error) {
	b := &Bots{}
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
		names := make([]string, 0, len(f.Services))
		for n := range f.Services {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, name := range names {
			s := f.Services[name]
			p := &Provider{Name: name, Class: src.class, RdnsPatterns: s.RdnsPatterns,
				Authoritative: s.IPListAuthoritative != nil && *s.IPListAuthoritative}
			for _, g := range s.UserAgentPatterns {
				if strings.Trim(g, "*") == "" {
					continue // a bare * would make every user agent this provider
				}
				if re, err := globRE(g); err == nil {
					p.ua = append(p.ua, re)
				}
			}
			idx := int32(len(b.Providers)) //nolint:gosec // a few dozen services
			for _, c := range slices.Concat(s.IPv4, s.IPv6) {
				pfx, err := netip.ParsePrefix(strings.TrimSpace(c))
				switch {
				case err != nil:
				case !plausible(pfx, botMinV4, botMinV6):
					b.Rejected.add(name + " " + pfx.String())
				default:
					b.table.Insert(pfx, idx)
					p.HasRanges = true
				}
			}
			b.Providers = append(b.Providers, p)
		}
	}
	b.table.Build()
	return b, nil
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
