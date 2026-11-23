package agg

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/stakeout/backend/internal/classify"
	"github.com/trick77/stakeout/backend/internal/plancheck"
	"github.com/trick77/stakeout/backend/internal/sim"
	"github.com/trick77/stakeout/backend/internal/store"
)

var anchor = time.Date(2026, 11, 22, 13, 30, 0, 0, time.UTC)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "stakeout.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// history is a classified simulator history on the default sites: people, crawlers, the API walker and a scan wave,
// so every class, both parts and the server dimensions have rows.
var history = sync.OnceValues(func() (string, error) {
	dir, err := tempDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "stakeout.db")
	st, err := store.Open(ctxBackground(), path)
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()
	_, _, err = sim.History(ctxBackground(), st, sim.HistoryOptions{Seed: 7, Anchor: anchor, Days: 3, Scale: 1.5,
		Scenarios: "api-walker,scan-wave", Classify: true})
	return path, err
})

// historyCopy is a fresh copy of the history for one test.
func historyCopy(t *testing.T) *store.Store {
	t.Helper()
	src, err := history()
	if err != nil {
		t.Fatal(err)
	}
	st := openStore(t)
	// ATTACH holds for one connection only.
	conn, err := st.DB.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, q := range []string{`ATTACH DATABASE '` + src + `' AS src`, `INSERT INTO site SELECT * FROM src.site`,
		`INSERT INTO event SELECT * FROM src.event`, `INSERT INTO actor_class SELECT * FROM src.actor_class`,
		`DETACH DATABASE src`} {
		if _, err := conn.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

var historyDir string

func tempDir() (string, error) {
	d, err := os.MkdirTemp("", "stakeout-agg-")
	historyDir = d
	return d, err
}

func ctxBackground() context.Context { return context.Background() }

func TestMain(m *testing.M) {
	code := m.Run()
	if historyDir != "" {
		_ = os.RemoveAll(historyDir)
	}
	os.Exit(code)
}

type siteDay struct{ site, day int64 }

// siteDays are every site-day with events, oldest first.
func siteDays(t *testing.T, st *store.Store) []siteDay {
	t.Helper()
	rows, err := st.DB.QueryContext(t.Context(), `SELECT DISTINCT site_id, ts / 86400000 FROM event ORDER BY 2, 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []siteDay
	for rows.Next() {
		var sd siteDay
		if err := rows.Scan(&sd.site, &sd.day); err != nil {
			t.Fatal(err)
		}
		out = append(out, sd)
	}
	return out
}

func buildAll(t *testing.T, st *store.Store, k int) []siteDay {
	t.Helper()
	b := &Builder{Store: st, K: k}
	sds := siteDays(t, st)
	for _, sd := range sds {
		if clean, err := b.BuildDay(t.Context(), sd.site, sd.day); err != nil || !clean {
			t.Fatalf("build %v: clean %v, %v", sd, clean, err)
		}
	}
	return sds
}

// near is a distinct count's tolerance: sketches are exact but for hash collisions while sparse.
func near(got, want int64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= max(1, want/1000)
}

func sketchCount(t *testing.T, b []byte) int64 {
	t.Helper()
	s, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return s.Count()
}

const partSQL = `CASE WHEN cls = 1 THEN 1 ELSE 2 END`

func TestBuild_AgreesWithRaw(t *testing.T) {
	if testing.Short() {
		t.Skip("simulator history")
	}
	for _, k := range []int{TopK, 3} {
		t.Run(fmt.Sprint("K=", k), func(t *testing.T) {
			st := historyCopy(t)
			sds := buildAll(t, st, k)
			var others, parts2, parts1 int
			classes := map[int]bool{}
			for _, sd := range sds {
				checkHours(t, st, sd, classes)
				o, p1, p2 := checkLists(t, st, sd, k)
				others, parts1, parts2 = others+o, parts1+p1, parts2+p2
				checkAddrs(t, st, sd)
				checkDay(t, st, sd)
			}
			// Not vacuous: people and automation, several classes, and at K=3 entries cut into Other rows.
			if parts1 == 0 || parts2 == 0 || len(classes) < 3 {
				t.Errorf("history too narrow: %d part-1 rows, %d part-2 rows, classes %v", parts1, parts2, classes)
			}
			if (k == 3) != (others > 0) {
				t.Errorf("K=%d: %d Other rows", k, others)
			}
		})
	}
}

func checkDay(t *testing.T, st *store.Store, sd siteDay) {
	t.Helper()
	days, err := st.AggDays(t.Context(), sd.site, sd.day, sd.day)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE site_id = ? AND ts >= ? AND ts < ?`, sd.site,
		sd.day*dayMS, (sd.day+1)*dayMS).Scan(&n); err != nil {
		t.Fatal(err)
	}
	d := days[sd.day]
	if !d.Frozen || d.Dirty != 0 || d.Builder != Version || !d.Raw || d.Events != n || d.BuiltAt == 0 {
		t.Errorf("%v: agg_day %+v, %d events", sd, d, n)
	}
}

func checkHours(t *testing.T, st *store.Store, sd siteDay, classes map[int]bool) {
	t.Helper()
	got := map[[2]int64]store.AggHour{}
	rows, err := st.DB.Query(`SELECT hour, cls, events, views, actions, starts, requests, refused, errors, browser_ids,
		addrs, server_addrs FROM agg_hour WHERE site_id = ? AND hour >= ? AND hour < ?`, sd.site, sd.day*24, sd.day*24+24)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var h store.AggHour
		if err := rows.Scan(&h.Hour, &h.Cls, &h.Events, &h.Views, &h.Actions, &h.Starts, &h.Requests, &h.Refused,
			&h.Errors, &h.BrowserIDs, &h.Addrs, &h.ServerAddrs); err != nil {
			t.Fatal(err)
		}
		got[[2]int64{h.Hour, int64(h.Cls)}] = h
	}
	_ = rows.Close()
	rows, err = st.DB.Query(`SELECT ts / 3600000 AS hour, CASE cls WHEN 0 THEN 4 ELSE cls END AS c, COUNT(*),
		SUM(kind = 'page_view'), SUM(source = 'browser' AND kind NOT IN ('page_view', 'session_start', 'engagement', 'vital')),
		SUM(kind = 'session_start'), SUM(source = 'server'), SUM(source = 'server' AND status >= 400),
		SUM(kind = 'js_error' OR (source = 'server' AND status >= 500)),
		COUNT(DISTINCT CASE WHEN source = 'browser' THEN visitor END), COUNT(DISTINCT ip_key),
		COUNT(DISTINCT CASE WHEN source = 'server' THEN ip_key END)
		FROM event WHERE site_id = ? AND ts >= ? AND ts < ? GROUP BY hour, c`, sd.site, sd.day*dayMS, (sd.day+1)*dayMS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	want := 0
	for rows.Next() {
		var w store.AggHour
		var ids, addrs, srv int64
		if err := rows.Scan(&w.Hour, &w.Cls, &w.Events, &w.Views, &w.Actions, &w.Starts, &w.Requests, &w.Refused,
			&w.Errors, &ids, &addrs, &srv); err != nil {
			t.Fatal(err)
		}
		want++
		classes[w.Cls] = true
		g, ok := got[[2]int64{w.Hour, int64(w.Cls)}]
		if !ok {
			t.Errorf("%v: hour %d cls %d missing", sd, w.Hour, w.Cls)
			continue
		}
		if g.Events != w.Events || g.Views != w.Views || g.Actions != w.Actions || g.Starts != w.Starts ||
			g.Requests != w.Requests || g.Refused != w.Refused || g.Errors != w.Errors {
			t.Errorf("%v hour %d cls %d: %+v, want %+v", sd, w.Hour, w.Cls, g, w)
		}
		if c := sketchCount(t, g.BrowserIDs); !near(c, ids) {
			t.Errorf("%v hour %d cls %d: browser ids %d, want %d", sd, w.Hour, w.Cls, c, ids)
		}
		if c := sketchCount(t, g.Addrs); !near(c, addrs) {
			t.Errorf("%v hour %d cls %d: addrs %d, want %d", sd, w.Hour, w.Cls, c, addrs)
		}
		if c := sketchCount(t, g.ServerAddrs); !near(c, srv) {
			t.Errorf("%v hour %d cls %d: server addrs %d, want %d", sd, w.Hour, w.Cls, c, srv)
		}
	}
	if len(got) != want {
		t.Errorf("%v: %d hour rows, want %d", sd, len(got), want)
	}
}

// dimSQL is each list dimension as direct SQL: the dashboard's definitions (dash/lists.go), with id the distinct
// identity for audiences all and humans (” is no id) and addr the bots audience's.
var dimSQL = map[string]struct{ where, key, sub, a, b, id, addr string }{
	"pages": {"kind = 'page_view'", "template", "''", "COUNT(*)", "0",
		"CASE WHEN source = 'browser' THEN visitor ELSE '' END", "ip_key"},
	"entry": {"kind = 'session_start'", "template", "''", "COUNT(*)", "0",
		"CASE WHEN source = 'browser' THEN visitor ELSE '' END", "ip_key"},
	"events": {"source = 'browser' AND kind NOT IN ('page_view', 'session_start', 'engagement', 'vital')",
		"CASE WHEN kind = 'custom' THEN name ELSE kind END",
		"MAX(CASE WHEN kind = 'custom' THEN 'custom' ELSE 'auto' END)", "COUNT(*)", "0", "visitor", "ip_key"},
	"outbound": {"kind = 'outbound_click'", "name", "''", "COUNT(*)", "0",
		"CASE WHEN source = 'browser' THEN visitor ELSE '' END", "ip_key"},
	"referrers": {"kind IN ('page_view', 'session_start') AND ref_host != ''", "ref_host", "''",
		"SUM(kind = 'page_view')", "0", "CASE WHEN source = 'browser' THEN visitor ELSE '' END", "ip_key"},
	"networks": {"asn != 0", "CAST(asn AS TEXT)", "''", "COUNT(*)", "SUM(source = 'server')", "ip_key", "''"},
	"countries": {"country != ''", "country", "''", "COUNT(*)", "0",
		"CASE WHEN source = 'browser' THEN visitor ELSE '' END", "ip_key"},
	"probed": {"source = 'server' AND status IN (401, 403, 404)", "template", "MAX(CAST(status AS TEXT))",
		"COUNT(*)", "0", "ip_key", "''"},
	"errors": {"(kind = 'js_error' OR (source = 'server' AND status >= 500))",
		"CASE WHEN kind = 'js_error' THEN name ELSE template END",
		"MAX(CASE WHEN kind = 'js_error' THEN 'js' ELSE 'server ' || status END)", "COUNT(*)", "0",
		"CASE WHEN source = 'browser' THEN visitor ELSE ip_key END", "ip_key"},
	"status": {"source = 'server' AND status > 0", "CAST(status / 100 AS TEXT) || 'xx'", "''", "COUNT(*)", "0", "''",
		"''"},
}

// want is one expected list entry: its sums, exact distinct count and the bots audience's address count.
type want struct {
	sub      string
	a, b, n  int64
	addrs    int64
	part     int
	parent   string
	key      string
	hasAddrs bool
}

func wantLists(t *testing.T, st *store.Store, sd siteDay, dim string) []want {
	t.Helper()
	if dim == "clients" {
		return wantClients(t, st, sd)
	}
	d := dimSQL[dim]
	//nolint:gosec // the test's own SQL fragments
	rows, err := st.DB.Query(`SELECT `+partSQL+` AS part, `+d.key+` AS k, `+d.sub+`, `+d.a+`, `+d.b+`,
		COUNT(DISTINCT NULLIF(`+d.id+`, '')), COUNT(DISTINCT NULLIF(`+d.addr+`, ''))
		FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND `+d.where+` GROUP BY part, k`,
		sd.site, sd.day*dayMS, (sd.day+1)*dayMS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []want
	for rows.Next() {
		var w want
		if err := rows.Scan(&w.part, &w.key, &w.sub, &w.a, &w.b, &w.n, &w.addrs); err != nil {
			t.Fatal(err)
		}
		w.hasAddrs = d.addr != "''"
		out = append(out, w)
	}
	return out
}

// wantClients is the clients dimension from plain rows: ParseUA has no SQL.
func wantClients(t *testing.T, st *store.Store, sd siteDay) []want {
	t.Helper()
	rows, err := st.DB.Query(`SELECT `+partSQL+`, source, visitor, ip_key, ip, ua FROM event
		WHERE site_id = ? AND ts >= ? AND ts < ?`, sd.site, sd.day*dayMS, (sd.day+1)*dayMS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	type k struct {
		part        int
		parent, key string
	}
	ids, addrs := map[k]map[string]bool{}, map[k]map[string]bool{}
	for rows.Next() {
		var part int
		var source, vid, key, ip, ua string
		if err := rows.Scan(&part, &source, &vid, &key, &ip, &ua); err != nil {
			t.Fatal(err)
		}
		if source != "browser" {
			vid = ""
		}
		if vid == "" && part == 1 {
			continue
		}
		br, os, dev := classify.ParseUA(ua)
		v := "IPv4"
		if strings.Contains(ip, ":") {
			v = "IPv6"
		}
		for _, x := range []k{{part, "browsers", br}, {part, "os", os}, {part, "devices", dev}, {part, "ip_version", v}} {
			if ids[x] == nil {
				ids[x], addrs[x] = map[string]bool{}, map[string]bool{}
			}
			if vid != "" {
				ids[x][vid] = true
			}
			if part == 2 {
				addrs[x][key] = true
			}
		}
	}
	var out []want
	for x := range ids {
		out = append(out, want{part: x.part, parent: x.parent, key: x.key, n: int64(len(ids[x])),
			addrs: int64(len(addrs[x])), hasAddrs: true})
	}
	return out
}

// kept is the union of a part's top k by n, by a, by addresses (automated part) and (networks) by b, ties by key;
// every entry of a full dimension.
func kept(dim string, part int, ws []want, k int) map[[2]string]bool {
	out := map[[2]string]bool{}
	byKey := func(x, y want) int { return cmp.Or(strings.Compare(x.parent, y.parent), strings.Compare(x.key, y.key)) }
	measures := []func(want) int64{func(w want) int64 { return w.n }, func(w want) int64 { return w.a }}
	if part == 2 && dimSQL[dim].addr != "''" {
		measures = append(measures, func(w want) int64 { return w.addrs })
	}
	if dim == "networks" {
		measures = append(measures, func(w want) int64 { return w.b })
	}
	for _, m := range measures {
		s := slices.Clone(ws)
		slices.SortFunc(s, func(x, y want) int { return cmp.Or(cmp.Compare(m(y), m(x)), byKey(x, y)) })
		if !fullDims[dim] && len(s) > k {
			s = s[:k]
		}
		for _, w := range s {
			out[[2]string{w.parent, w.key}] = true
		}
	}
	return out
}

func checkLists(t *testing.T, st *store.Store, sd siteDay, k int) (others, part1, part2 int) {
	t.Helper()
	for _, dim := range Dims {
		rows, err := st.DB.Query(`SELECT part, other, parent, key, sub, a, b, n, ids, addrs, entries, keys FROM agg_list
			WHERE site_id = ? AND dim = ? AND day = ?`, sd.site, dim, sd.day)
		if err != nil {
			t.Fatal(err)
		}
		got := map[[3]string]store.AggList{}
		for rows.Next() {
			var l store.AggList
			if err := rows.Scan(&l.Part, &l.Other, &l.Parent, &l.Key, &l.Sub, &l.A, &l.B, &l.N, &l.IDs, &l.Addrs,
				&l.Entries, &l.Keys); err != nil {
				t.Fatal(err)
			}
			got[[3]string{fmt.Sprint(l.Part, l.Other), l.Parent, l.Key}] = l
		}
		_ = rows.Close()
		byPart := map[int][]want{}
		for _, w := range wantLists(t, st, sd, dim) {
			byPart[w.part] = append(byPart[w.part], w)
		}
		matched := 0
		for part, ws := range byPart {
			if part == 1 {
				part1 += len(ws)
			} else {
				part2 += len(ws)
			}
			keep := kept(dim, part, ws, k)
			type rest struct {
				a, b, n, entries int64
				hashes           []uint64
			}
			rs := map[string]*rest{}
			for _, w := range ws {
				if !keep[[2]string{w.parent, w.key}] {
					r := rs[w.parent]
					if r == nil {
						r = &rest{}
						rs[w.parent] = r
					}
					r.a, r.b, r.n, r.entries = r.a+w.a, r.b+w.b, r.n+w.n, r.entries+1
					r.hashes = append(r.hashes, Hash(w.key))
					continue
				}
				g, ok := got[[3]string{fmt.Sprint(part, false), w.parent, w.key}]
				if !ok {
					t.Errorf("%v %s part %d: %q/%q missing", sd, dim, part, w.parent, w.key)
					continue
				}
				matched++
				if g.Sub != w.sub || g.A != w.a || g.B != w.b || g.N != w.n {
					t.Errorf("%v %s part %d %q: sub %q a %d b %d n %d, want %q %d %d %d", sd, dim, part, w.key, g.Sub,
						g.A, g.B, g.N, w.sub, w.a, w.b, w.n)
				}
				if c := sketchCount(t, g.IDs); !near(c, w.n) {
					t.Errorf("%v %s part %d %q: ids sketch %d, want %d", sd, dim, part, w.key, c, w.n)
				}
				switch c := sketchCount(t, g.Addrs); {
				case part == 1 || !w.hasAddrs:
					if g.Addrs != nil {
						t.Errorf("%v %s part %d %q: addrs stored", sd, dim, part, w.key)
					}
				case !near(c, w.addrs):
					t.Errorf("%v %s part %d %q: addrs sketch %d, want %d", sd, dim, part, w.key, c, w.addrs)
				}
			}
			for parent, r := range rs {
				g, ok := got[[3]string{fmt.Sprint(part, true), parent, ""}]
				if !ok {
					t.Errorf("%v %s part %d: Other of %q missing", sd, dim, part, parent)
					continue
				}
				matched++
				others++
				slices.Sort(r.hashes)
				if g.A != r.a || g.B != r.b || g.N != r.n || g.Entries != r.entries ||
					!slices.Equal(OtherKeys(g.Keys), slices.Compact(r.hashes)) || g.IDs != nil {
					t.Errorf("%v %s part %d Other: %+v, want %+v", sd, dim, part, g, r)
				}
			}
		}
		if matched != len(got) {
			t.Errorf("%v %s: %d rows stored, %d expected: %v", sd, dim, len(got), matched, slices.Collect(maps.Keys(got)))
		}
	}
	return others, part1, part2
}

func checkAddrs(t *testing.T, st *store.Store, sd siteDay) {
	t.Helper()
	got := map[store.AggAddrKey]store.AggAddr{}
	rows, err := st.DB.Query(`SELECT ip_key, part, events, views, actions, requests, refused, sessions, carried,
		browser, human, first_ts, last_ts, ua, asn, country, cls FROM agg_addr WHERE site_id = ? AND day = ?`,
		sd.site, sd.day)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a store.AggAddr
		if err := rows.Scan(&a.IPKey, &a.Part, &a.Events, &a.Views, &a.Actions, &a.Requests, &a.Refused, &a.Sessions,
			&a.Carried, &a.Browser, &a.Human, &a.FirstTS, &a.LastTS, &a.UA, &a.ASN, &a.Country, &a.Cls); err != nil {
			t.Fatal(err)
		}
		got[store.AggAddrKey{IPKey: a.IPKey, Part: a.Part}] = a
	}
	_ = rows.Close()

	carried := map[store.AggAddrKey]int64{}
	rows, err = st.DB.Query(`SELECT ip_key, part, COUNT(*) FROM (SELECT DISTINCT ip_key, `+partSQL+` AS part, sid
		FROM event WHERE site_id = ?1 AND ts >= ?2 AND ts < ?3 AND sid != '') AS t
		WHERE EXISTS (SELECT 1 FROM event p WHERE p.site_id = ?1 AND p.ip_key = t.ip_key AND p.sid = t.sid
			AND p.ts >= ?2 - 86400000 AND p.ts < ?2 AND CASE WHEN p.cls = 1 THEN 1 ELSE 2 END = t.part)
		GROUP BY ip_key, part`, sd.site, sd.day*dayMS, (sd.day+1)*dayMS)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k store.AggAddrKey
		var n int64
		if err := rows.Scan(&k.IPKey, &k.Part, &n); err != nil {
			t.Fatal(err)
		}
		carried[k] = n
	}
	_ = rows.Close()

	rows, err = st.DB.Query(`SELECT ip_key, `+partSQL+` AS part, COUNT(*), SUM(kind = 'page_view'),
		SUM(source = 'browser' AND kind NOT IN ('page_view', 'session_start', 'engagement', 'vital')),
		SUM(source = 'server'), SUM(source = 'server' AND status >= 400), COUNT(DISTINCT NULLIF(sid, '')),
		MAX(source = 'browser'), MAX(cls = 1), MIN(ts), MAX(ts)
		FROM event WHERE site_id = ? AND ts >= ? AND ts < ? GROUP BY ip_key, part`,
		sd.site, sd.day*dayMS, (sd.day+1)*dayMS)
	if err != nil {
		t.Fatal(err)
	}
	var ws []store.AggAddr
	for rows.Next() {
		var w store.AggAddr
		if err := rows.Scan(&w.IPKey, &w.Part, &w.Events, &w.Views, &w.Actions, &w.Requests, &w.Refused, &w.Sessions,
			&w.Browser, &w.Human, &w.FirstTS, &w.LastTS); err != nil {
			t.Fatal(err)
		}
		ws = append(ws, w)
	}
	_ = rows.Close()
	for _, w := range ws {
		// The latest row: by time, then the row written last.
		if err := st.DB.QueryRow(`SELECT ua, asn, country, cls FROM event WHERE site_id = ? AND ip_key = ?
			AND ts >= ? AND ts < ? AND `+partSQL+` = ? ORDER BY ts DESC, id DESC LIMIT 1`, sd.site, w.IPKey,
			sd.day*dayMS, (sd.day+1)*dayMS, w.Part).Scan(&w.UA, &w.ASN, &w.Country, &w.Cls); err != nil {
			t.Fatal(err)
		}
		k := store.AggAddrKey{IPKey: w.IPKey, Part: w.Part}
		w.SiteID, w.Day, w.Carried = sd.site, sd.day, carried[k]
		g, ok := got[k]
		g.SiteID, g.Day = sd.site, sd.day
		if !ok || g != w {
			t.Errorf("%v %v:\n got %+v\nwant %+v", sd, k, g, w)
		}
	}
	if len(got) != len(ws) {
		t.Errorf("%v: %d address rows, want %d", sd, len(got), len(ws))
	}
}

// insert writes events through the store, as the batch writer does.
func insert(t *testing.T, st *store.Store, evs ...store.Event) {
	t.Helper()
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
}

func site(t *testing.T, st *store.Store) int64 {
	t.Helper()
	rows, err := st.SyncSites(t.Context(), []store.Site{{Domain: "a.example.com", Kind: "site", PathRules: "[]"}})
	if err != nil {
		t.Fatal(err)
	}
	return rows["a.example.com"].ID
}

// A key with people's and automated rows on one day is a row per part; a session in both parts counts in each, and a
// session crossing midnight is carried within its part.
func TestBuild_MixedParts(t *testing.T) {
	st := openStore(t)
	id := site(t, st)
	day := anchor.UnixMilli()/dayMS - 5
	t0 := day * dayMS
	ev := func(ts int64, kind string, cls int, sid, src string) store.Event {
		return store.Event{SiteID: id, TS: ts, Source: src, Kind: kind, Visitor: "v1", SID: sid, IP: "192.0.2.7",
			IPKey: "192.0.2.7", UA: "Mozilla/5.0 Firefox/140.0", Cls: cls, Template: "/", Country: "CH", ASN: 64500}
	}
	insert(t, st,
		ev(t0-1000, "page_view", 1, "s0", "browser"), // the day before, carried into day
		ev(t0+1000, "page_view", 1, "s0", "browser"),
		ev(t0+2000, "page_view", 1, "s1", "browser"),
		ev(t0+3000, "custom", 2, "s1", "browser"), // the same session, stamped bot
		ev(t0+4000, "request", 0, "", "server"),   // pending: automated part, cls 4 in agg_hour
	)
	b := &Builder{Store: st}
	for _, d := range []int64{day - 1, day} {
		if _, err := b.BuildDay(t.Context(), id, d); err != nil {
			t.Fatal(err)
		}
	}
	got := map[int]store.AggAddr{}
	rows, err := st.DB.Query(`SELECT part, events, sessions, carried, human, browser, cls FROM agg_addr
		WHERE site_id = ? AND day = ?`, id, day)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a store.AggAddr
		if err := rows.Scan(&a.Part, &a.Events, &a.Sessions, &a.Carried, &a.Human, &a.Browser, &a.Cls); err != nil {
			t.Fatal(err)
		}
		got[a.Part] = a
	}
	_ = rows.Close()
	if p := got[1]; p.Events != 2 || p.Sessions != 2 || p.Carried != 1 || !p.Human || !p.Browser || p.Cls != 1 {
		t.Errorf("people's part = %+v", p)
	}
	if p := got[2]; p.Events != 2 || p.Sessions != 1 || p.Carried != 0 || p.Human || !p.Browser || p.Cls != 0 {
		t.Errorf("automated part = %+v", p)
	}
	var cls4, pages1, pages2 int64
	if err := st.DB.QueryRow(`SELECT requests FROM agg_hour WHERE site_id = ? AND hour = ? AND cls = 4`, id,
		t0/hourMS).Scan(&cls4); err != nil || cls4 != 1 {
		t.Errorf("pending request in cls 4: %d, %v", cls4, err)
	}
	if err := st.DB.QueryRow(`SELECT SUM(part = 1), SUM(part = 2) FROM agg_list WHERE site_id = ? AND dim = 'events'
		AND day = ?`, id, day).Scan(&pages1, &pages2); err != nil || pages1 != 0 || pages2 != 1 {
		t.Errorf("events rows per part: %d, %d, %v", pages1, pages2, err)
	}
}

// A day whose raw is gone keeps its rows, and the day after it keeps its stored carried sessions.
func TestBuild_NoRaw(t *testing.T) {
	st := openStore(t)
	id := site(t, st)
	day := anchor.UnixMilli()/dayMS - 5
	t0 := day * dayMS
	insert(t, st,
		store.Event{SiteID: id, TS: t0 - 1000, Source: "browser", Kind: "page_view", Visitor: "v", SID: "s",
			IP: "192.0.2.8", IPKey: "192.0.2.8", Cls: 1},
		store.Event{SiteID: id, TS: t0 + 1000, Source: "browser", Kind: "page_view", Visitor: "v", SID: "s",
			IP: "192.0.2.8", IPKey: "192.0.2.8", Cls: 1})
	b := &Builder{Store: st}
	for _, d := range []int64{day - 1, day} {
		if _, err := b.BuildDay(t.Context(), id, d); err != nil {
			t.Fatal(err)
		}
	}
	// The purge, by hand: the day before's raw goes.
	if _, err := st.DB.Exec(`DELETE FROM event WHERE ts < ?`, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(`UPDATE agg_day SET raw = 0, dirty = 1 WHERE site_id = ? AND day = ?`, id, day-1); err != nil {
		t.Fatal(err)
	}
	for _, d := range []int64{day - 1, day} {
		if clean, err := b.BuildDay(t.Context(), id, d); err != nil || !clean {
			t.Fatalf("day %d: clean %v, %v", d, clean, err)
		}
	}
	var views, carried int64
	var dirty int
	if err := st.DB.QueryRow(`SELECT views FROM agg_addr WHERE site_id = ? AND day = ?`, id, day-1).
		Scan(&views); err != nil || views != 1 {
		t.Errorf("purged day's row: %d views, %v", views, err)
	}
	if err := st.DB.QueryRow(`SELECT dirty FROM agg_day WHERE site_id = ? AND day = ?`, id, day-1).
		Scan(&dirty); err != nil || dirty != 0 {
		t.Errorf("purged day dirty = %d, %v", dirty, err)
	}
	if err := st.DB.QueryRow(`SELECT carried FROM agg_addr WHERE site_id = ? AND day = ?`, id, day).
		Scan(&carried); err != nil || carried != 1 {
		t.Errorf("carried after the day before's purge = %d, %v", carried, err)
	}
}

// TestBuild_Plans checks every statement the builder reads with: none may scan event or an aggregate table, and the
// day's read comes in index order, without a sort.
func TestBuild_Plans(t *testing.T) {
	st := historyOrEmpty(t)
	var mu sync.Mutex
	seen := map[string][]any{}
	TraceSQL = func(q string, args []any) {
		mu.Lock()
		defer mu.Unlock()
		seen[q] = append([]any(nil), args...)
	}
	t.Cleanup(func() { TraceSQL = nil })
	buildAll(t, st, TopK)
	if len(seen) < 2 {
		t.Fatalf("%d statements traced", len(seen))
	}
	for q, args := range seen {
		plan, err := plancheck.Explain(t.Context(), st.DB, q, args...)
		if err != nil {
			t.Fatal(err)
		}
		if p := plancheck.Problem(q, plan); p != "" || strings.Contains(strings.Join(plan, "\n"), "TEMP B-TREE") {
			t.Errorf("plan of %s: %s\n%s", q, p, strings.Join(plan, "\n"))
		}
	}
}

// historyOrEmpty is the simulator history, or under -short a handful of rows.
func historyOrEmpty(t *testing.T) *store.Store {
	t.Helper()
	if !testing.Short() {
		return historyCopy(t)
	}
	st := openStore(t)
	id := site(t, st)
	insert(t, st, store.Event{SiteID: id, TS: anchor.UnixMilli(), Source: "browser", Kind: "page_view",
		Visitor: "v", SID: "s", IP: "192.0.2.9", IPKey: "192.0.2.9", Cls: 1})
	return st
}
