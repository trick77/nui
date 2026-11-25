package watch

import (
	"context"
	"database/sql"
	"encoding/json"
	"hash/fnv"
	"net/netip"
	"regexp"
	"strings"
	"unicode/utf8"
)

// scriptRows is the statement a site-day is read with, one day of the site's time index at most: the engagement
// events that report script hosts.
const scriptRows = `SELECT ts, ip_key, props FROM event
	WHERE site_id = ? AND ts >= ? AND ts < ? AND kind = 'engagement' AND props LIKE '%"scripts":%'`

// TraceSQL sees, when set (tests), every statement the job reads events with: the tests check their plans.
var TraceSQL func(query string, args []any)

func query(ctx context.Context, db *sql.DB, q string, args ...any) (*sql.Rows, error) {
	if TraceSQL != nil {
		TraceSQL(q, args)
	}
	return db.QueryContext(ctx, q, args...)
}

// day is what one site-day's raw rows say, before the state judges it.
type day struct {
	n     int64
	hosts map[string]*hostDay
	// addrHosts are the hosts each address key brought into hosts.
	addrHosts map[string]int
	// cutKeys are the hosts past the day's bounds in memory (hashed, up to dayCutKeys), cut their number once the day
	// is read: a host left out counts once, however many rows it had.
	cutKeys map[uint64]bool
	cut     int64
}

type hostDay struct {
	first, last int64
	// addrs are the address keys that reported the host, up to hostAddrs; nets their networks (netOf), up to
	// hostAddrs too: one /48 holds more /64 keys than addrs keeps.
	addrs, nets map[string]bool
}

// netOf is an address key's network: an IPv4 address's /24, an IPv6 /64's /48 (a /48 is one site's, its 65,536
// /64s cost the holder nothing). A key that is no address is its own.
func netOf(key string) string {
	a, err := netip.ParseAddr(strings.TrimSuffix(key, "/64"))
	if err != nil {
		return key
	}
	bits := 48
	if a.Is4() {
		bits = 24
	}
	p, err := a.Prefix(bits)
	if err != nil {
		return key
	}
	return p.String()
}

func hostHash(name string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return h.Sum64()
}

// cutKey counts a host left out of the day.
func (d *day) cutKey(name string) {
	if len(d.cutKeys) < dayCutKeys {
		d.cutKeys[hostHash(name)] = true
	}
}

// finish counts the hosts left out: one an address could not bring in may have come with another.
func (d *day) finish() {
	for name := range d.hosts {
		delete(d.cutKeys, hostHash(name))
	}
	d.cut = int64(len(d.cutKeys))
}

// scan reads a site's rows in [from, to), the day n or its part so far.
func scan(ctx context.Context, db *sql.DB, site *siteRef, n, from, to int64) (*day, error) {
	d := newDay(n)
	if err := d.readScripts(ctx, db, site, from, to); err != nil {
		return nil, err
	}
	d.finish()
	return d, nil
}

func newDay(n int64) *day {
	return &day{n: n, hosts: map[string]*hostDay{}, addrHosts: map[string]int{}, cutKeys: map[uint64]bool{}}
}

// readScripts reads the script hosts the pages reported, counted by the address keys and the networks that reported
// them. The list is the browser's word, so hostile: a host is kept only as the script sends it (scriptHost),
// maxScripts a report at most, dayHostsPerAddr new ones an address a day, the site's own host never.
func (d *day) readScripts(ctx context.Context, db *sql.DB, site *siteRef, from, to int64) error {
	rows, err := query(ctx, db, scriptRows, site.id, from, to)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var ts int64
	var addr, props string
	for rows.Next() {
		if err := rows.Scan(&ts, &addr, &props); err != nil {
			return err
		}
		// The event's own props are p (the collector's envelope beside title and ctx).
		var p struct {
			P struct {
				Scripts []json.RawMessage `json:"scripts"`
			} `json:"p"`
		}
		if json.Unmarshal([]byte(props), &p) != nil {
			continue
		}
		net := netOf(addr)
		for _, raw := range p.P.Scripts[:min(len(p.P.Scripts), maxScripts)] {
			host, ok := scriptHost(raw)
			if !ok || host == site.domain {
				continue
			}
			h := d.hosts[host]
			if h == nil {
				if len(d.hosts) >= dayHosts || d.addrHosts[addr] >= dayHostsPerAddr {
					d.cutKey(host)
					continue
				}
				d.addrHosts[addr]++
				h = &hostDay{addrs: map[string]bool{}, nets: map[string]bool{}}
				d.hosts[host] = h
			}
			h.first, h.last = minNZ(h.first, ts), max(h.last, ts)
			if len(h.addrs) < hostAddrs {
				h.addrs[addr] = true
			}
			if len(h.nets) < hostAddrs {
				h.nets[net] = true
			}
		}
	}
	return rows.Err()
}

// hostName is a host name as a browser's URL parser gives it, lower case: dot-separated labels of letters, digits,
// inner hyphens and underscores (hosts do carry them), a single label too. The script's HOSTNAME.
var hostName = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?(\.[a-z0-9_]([a-z0-9_-]*[a-z0-9_])?)*$`)

// maxHostRaw bounds a reported value before it is decoded: a name a URL parser could give, quoted.
const maxHostRaw = 255 + 2

// scriptHost is one reported host as the script sends it, ok false when it is none. JSON's decoder turns bytes that
// are no UTF-8 into U+FFFD; the raw value is checked before it decodes. An IPv6 address comes in brackets and is kept
// in its canonical form; a name past maxHostLen is cut to its last labels, as the script cuts it.
func scriptHost(raw json.RawMessage) (string, bool) {
	if !utf8.Valid(raw) || len(raw) > maxHostRaw {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	s = strings.TrimSuffix(strings.ToLower(s), ".")
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		a, err := netip.ParseAddr(s[1 : len(s)-1])
		if err != nil || !a.Is6() || a.Zone() != "" {
			return "", false
		}
		return "[" + a.String() + "]", true
	}
	if !hostName.MatchString(s) {
		return "", false
	}
	return clipHost(s)
}

// clipHost cuts a name past maxHostLen to its last labels: the registered domain stays, a name is still a name. ok
// false when no label fits.
func clipHost(s string) (string, bool) {
	for len(s) > maxHostLen {
		i := strings.IndexByte(s, '.')
		if i < 0 {
			return "", false
		}
		s = s[i+1:]
	}
	return s, true
}
