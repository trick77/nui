package watch

import (
	"context"
	"database/sql"
	"encoding/json"
	"hash/fnv"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// The two statements a site-day is read with, each one day of the site's time index at most. The server rows in time
// order (the index's, no sort): an address's first answer on a template is its first row. bytes lives in props only.
const (
	serverRows = `SELECT ts, ip_key, method, template, status, unrouted, err,
		CAST(COALESCE(json_extract(props, '$.bytes'), 0) AS INTEGER) FROM event
		WHERE site_id = ? AND ts >= ? AND ts < ? AND source = 'server' ORDER BY ts`
	scriptRows = `SELECT ts, ip_key, props FROM event
		WHERE site_id = ? AND ts >= ? AND ts < ? AND kind = 'engagement' AND props LIKE '%"scripts":%'`
)

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
	addrs map[string]*addrSums
	tmpls map[string]*tmplDay
	// sizes are the unrouted 2xx GETs' sizes and the templates sharing each, up to shellTemplates.
	sizes  map[int64]map[string]bool
	routed bool
	hosts  map[string]*hostDay
	// addrHosts are the hosts each address key brought into hosts.
	addrHosts map[string]int
	// cutKeys are the templates and hosts past the day's bounds in memory (hashed, up to dayCutKeys), cut their number
	// once the day is read: a key left out counts once, however many rows it had.
	cutKeys map[uint64]bool
	cut     int64
}

type addrSums struct{ server, refused int64 }

// tmplDay is one template's rows of the day: its refusals summed, its answers kept (the first dayOKRows of them)
// until the day's shell sizes and scanners are known.
type tmplDay struct {
	refFirst, refLast int64
	// refSizes are the refusals' distinct sizes (addSize).
	refSizes []int64
	// refAddrs are the refused address keys and the first refusal of each, up to dayAddrs.
	refAddrs map[string]int64
	oks      []okRow
}

type okRow struct {
	ts, bytes int64
	addr      string
	unrouted  bool
	safe      bool // GET, HEAD or OPTIONS
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

// cutKey counts a template ("t") or host ("h") left out of the day.
func (d *day) cutKey(kind, key string) {
	if len(d.cutKeys) >= dayCutKeys {
		return
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(kind + "\x00" + key))
	d.cutKeys[h.Sum64()] = true
}

// finish counts the keys left out: a host one address could not bring in may have come with another.
func (d *day) finish() {
	for name := range d.hosts {
		h := fnv.New64a()
		_, _ = h.Write([]byte("h\x00" + name))
		delete(d.cutKeys, h.Sum64())
	}
	d.cut = int64(len(d.cutKeys))
}

// addSize adds a size to a sorted list of distinct sizes: none within the tolerance of one already there, at most
// maxRefusedSizes, the first come kept. Adding a size twice changes nothing.
func addSize(sizes []int64, s int64) []int64 {
	if s <= 0 || len(sizes) >= maxRefusedSizes {
		return sizes
	}
	for _, x := range sizes {
		if abs(x-s) <= SizeTolerance(max(x, s)) {
			return sizes
		}
	}
	i, _ := slices.BinarySearch(sizes, s)
	return slices.Insert(slices.Clip(sizes), i, s)
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// safeMethod: a method that only reads. Its routed answers never count, a route reads as it should.
func safeMethod(m string) bool { return m == "GET" || m == "HEAD" || m == "OPTIONS" }

// refusal: 404, or 401/403 without a route. A routed 401 is a login doing its work.
func refusal(status int, unrouted bool) bool {
	return status == 404 || (unrouted && (status == 401 || status == 403))
}

// answer: a 2xx without the product's error, unrouted or by a method that changes something.
func answer(status int, unrouted bool, errName, method string) bool {
	return status >= 200 && status < 300 && errName == "" && (unrouted || !safeMethod(method))
}

// scan reads a site's rows in [from, to), the day n or its part so far.
func scan(ctx context.Context, db *sql.DB, site *siteRef, n, from, to int64) (*day, error) {
	d := newDay(n)
	if err := d.readServer(ctx, db, site.id, from, to); err != nil {
		return nil, err
	}
	if err := d.readScripts(ctx, db, site, from, to); err != nil {
		return nil, err
	}
	d.finish()
	return d, nil
}

func newDay(n int64) *day {
	return &day{n: n, addrs: map[string]*addrSums{}, tmpls: map[string]*tmplDay{}, sizes: map[int64]map[string]bool{},
		hosts: map[string]*hostDay{}, addrHosts: map[string]int{}, cutKeys: map[uint64]bool{}}
}

func (d *day) readServer(ctx context.Context, db *sql.DB, site, from, to int64) error {
	rows, err := query(ctx, db, serverRows, site, from, to)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var ts, bytes int64
	var key, method, tmpl, errName string
	var status int
	var unrouted bool
	for rows.Next() {
		if err := rows.Scan(&ts, &key, &method, &tmpl, &status, &unrouted, &errName, &bytes); err != nil {
			return err
		}
		a := d.addrs[key]
		if a == nil {
			a = &addrSums{}
			d.addrs[key] = a
		}
		a.server++
		if status >= 400 && status < 500 {
			a.refused++
		}
		if !unrouted {
			d.routed = true
		}
		ok := answer(status, unrouted, errName, method)
		if ok && unrouted && method == "GET" && bytes > 0 {
			s := d.sizes[bytes]
			if s == nil {
				s = map[string]bool{}
				d.sizes[bytes] = s
			}
			if len(s) < shellTemplates {
				s[tmpl] = true
			}
		}
		ref := refusal(status, unrouted)
		if !ok && !ref {
			continue
		}
		t := d.tmpls[tmpl]
		if t == nil {
			if len(d.tmpls) >= dayTemplates {
				d.cutKey("t", tmpl)
				continue
			}
			t = &tmplDay{refAddrs: map[string]int64{}}
			d.tmpls[tmpl] = t
		}
		if ref {
			t.refFirst = minNZ(t.refFirst, ts)
			t.refLast = max(t.refLast, ts)
			t.refSizes = addSize(t.refSizes, bytes)
			if _, seen := t.refAddrs[key]; !seen && len(t.refAddrs) < dayAddrs {
				t.refAddrs[key] = ts
			}
			continue
		}
		// Past the first rows the template is answered often that day: its first answers are what is judged.
		if len(t.oks) < dayOKRows {
			t.oks = append(t.oks, okRow{ts: ts, bytes: bytes, addr: key, unrouted: unrouted, safe: safeMethod(method)})
		}
	}
	return rows.Err()
}

// readScripts reads the script hosts the pages reported, counted by the address keys and the networks that reported
// them. The list
// is the browser's word, so hostile: a host is kept only as the script sends it (scriptHost), maxScripts a report at
// most, dayHostsPerAddr new ones an address a day, the site's own host never.
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
					d.cutKey("h", host)
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
