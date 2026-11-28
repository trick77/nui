package refdata

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strings"
)

// The operator's own probes (conf/probes.json): an uptime monitor of their own that no list carries. A probe is
// matched by its address and its user agent together: another program from the same address stays what it is, and the
// same monitor software elsewhere (anyone's Uptime Kuma) is no claim on the operator's name. So they stay out of the
// list's address table and user-agent search (ByIP, Rank, ByUA, Claimed) and answer only Own.

// ownFile is the probes file's shape, a subset of monitoring.json's: an entry copied from ipverse's list keeps only
// these.
type ownFile struct {
	Services map[string]json.RawMessage `json:"services"`
}

type ownEntry struct {
	IPv4              []string `json:"ipv4"`
	IPv6              []string `json:"ipv6"`
	UserAgentPatterns []string `json:"user_agent_patterns"`
}

// LoadOwn reads and checks the probes file. A missing file is none, not an error.
func LoadOwn(path string) ([]*Provider, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the operator's configured file
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseOwn(b)
}

// ParseOwn checks a probes file's content, every fault at once. Each probe needs an address and a user-agent pattern:
// the match takes both.
func ParseOwn(b []byte) ([]*Provider, error) {
	var f ownFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("probes file: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("probes file: more than one JSON value")
	}
	if dup := duplicateKey(b); dup != "" {
		return nil, fmt.Errorf("probes file: probe %q listed twice", dup)
	}
	var errs []error
	var own []*Provider
	for _, name := range slices.Sorted(maps.Keys(f.Services)) {
		var e ownEntry
		d := json.NewDecoder(bytes.NewReader(f.Services[name]))
		d.DisallowUnknownFields()
		if err := d.Decode(&e); err != nil {
			errs = append(errs, fmt.Errorf("probe %q: %w", name, err))
			continue
		}
		if strings.TrimSpace(name) != name || name == "" || len(name) > maxAgentName || strings.ContainsFunc(name, notPrint) {
			errs = append(errs, fmt.Errorf("probe %q: want a name of at most %d printable characters, no outer spaces",
				name, maxAgentName))
		}
		p := &Provider{Name: name, Class: "monitor", Authoritative: true, HasRanges: true}
		for _, c := range slices.Concat(e.IPv4, e.IPv6) {
			pfx, err := botPrefix(c)
			switch {
			case err != nil || !pfx.IsValid() || strings.Contains(c, "%"):
				errs = append(errs, fmt.Errorf("probe %s: %q is no address or range", name, c))
			case pfx.Addr().Is4In6():
				errs = append(errs, fmt.Errorf("probe %s: %q is an IPv4 address written as IPv6, write it as IPv4", name, c))
			case !plausible(pfx, botMinV4, botMinV6):
				errs = append(errs, fmt.Errorf("probe %s: range %s is too broad", name, pfx))
			default:
				p.Ranges = append(p.Ranges, pfx)
			}
		}
		for _, g := range e.UserAgentPatterns {
			ua, ok := uaGlob(g)
			if !ok {
				errs = append(errs, fmt.Errorf("probe %s: user agent pattern %q matches nothing or everything", name, g))
				continue
			}
			p.ua = append(p.ua, ua)
		}
		if len(e.IPv4)+len(e.IPv6) == 0 {
			errs = append(errs, fmt.Errorf("probe %s: no address", name))
		}
		if len(e.UserAgentPatterns) == 0 {
			errs = append(errs, fmt.Errorf("probe %s: no user agent pattern", name))
		}
		own = append(own, p)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return own, nil
}

// duplicateKey is a probe name the services object lists twice, "" for none: encoding/json keeps the last silently.
// b decoded already, so its shape is known good.
func duplicateKey(b []byte) string {
	dec := json.NewDecoder(bytes.NewReader(b))
	if _, err := dec.Token(); err != nil { // {
		return ""
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return ""
		}
		if k != "services" {
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return ""
			}
			continue
		}
		if t, err := dec.Token(); err != nil || t != json.Delim('{') {
			return "" // null: no services
		}
		seen := map[string]bool{}
		for dec.More() {
			t, err := dec.Token()
			name, ok := t.(string)
			if err != nil || !ok {
				return ""
			}
			if seen[name] {
				return name
			}
			seen[name] = true
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return ""
			}
		}
		if _, err := dec.Token(); err != nil { // }
			return ""
		}
	}
	return ""
}

// withOwn is b with the operator's probes: a copy, b itself is shared. A probe's name joins the name index where it is
// free; a name the list already holds (a service's or an agent's) keeps the list's meaning, the probe is listed in
// clashed: the list's purpose and group are not the operator's to take away.
func (b *Bots) withOwn(own []*Provider) *Bots {
	if b == nil || len(own) == 0 {
		return b
	}
	c := *b
	c.own = own
	c.clashed = nil
	c.names = maps.Clone(b.names)
	if c.names == nil {
		c.names = map[string]Named{}
	}
	for _, p := range own {
		if _, taken := c.names[p.Name]; taken {
			c.clashed = append(c.clashed, p.Name)
			continue
		}
		c.names[p.Name] = Named{Service: p.Name, Class: p.Class}
	}
	return &c
}

// Own is the operator's probe at a whose user-agent pattern matches ua, the longest pattern's; of two of one length
// the first by name. On the write path for every event: no allocation.
func (b *Bots) Own(a netip.Addr, ua string) (*Provider, bool) {
	if b == nil || len(b.own) == 0 {
		return nil, false
	}
	a = a.Unmap()
	var best *Provider
	length := 0
	for _, p := range b.own {
		if !slices.ContainsFunc(p.Ranges, func(r netip.Prefix) bool { return r.Contains(a) }) {
			continue
		}
		if n := p.uaLength(ua); n > length {
			best, length = p, n
		}
	}
	return best, best != nil
}
