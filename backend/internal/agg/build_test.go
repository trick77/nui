package agg

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/classify"
	"github.com/trick77/trakk/backend/internal/plancheck"
	"github.com/trick77/trakk/backend/internal/sim"
	"github.com/trick77/trakk/backend/internal/store"
)

var anchor = time.Date(2026, 11, 22, 13, 30, 0, 0, time.UTC)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "trakk.db"))
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
	path := filepath.Join(dir, "trakk.db")
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
	d, err := os.MkdirTemp("", "trakk-agg-")
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
			insertHostile(t, st)
			sds := buildAll(t, st, k)
			var others, parts2, parts1 int
			classes := map[int]bool{}
			var exp experience
			for _, sd := range sds {
				checkHours(t, st, sd, classes)
				o, p1, p2 := checkLists(t, st, sd, k)
				others, parts1, parts2 = others+o, parts1+p1, parts2+p2
				checkAddrs(t, st, sd)
				checkDay(t, st, sd)
				exp.add(checkExperience(t, st, sd))
			}
			// Not vacuous: people and automation, several classes, and at K=3 entries cut into Other rows.
			if parts1 == 0 || parts2 == 0 || len(classes) < 3 {
				t.Errorf("history too narrow: %d part-1 rows, %d part-2 rows, classes %v", parts1, parts2, classes)
			}
			// Every vital sampled, engaged time, vitals and engagements left out (old scripts, hostile rows, props
			// that are no JSON), named services.
			if slices.Contains(exp.samples[:], 0) || exp.engaged == 0 || exp.left < 7 || exp.zero < 2 ||
				exp.clamped == 0 || exp.invalid != 3 {
				t.Errorf("experience too narrow: %+v", exp)
			}
			var services int
			if err := st.DB.QueryRow(`SELECT COUNT(*) FROM agg_list WHERE dim = 'services' AND sub = 'bot'`).
				Scan(&services); err != nil || services == 0 {
				t.Errorf("%d services rows, %v", services, err)
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

// insertHostile adds a person's vitals and engagements of every shape a row stored before /api/in checked them may
// hold, beside a page view of the history's.
func insertHostile(t *testing.T, st *store.Store) {
	t.Helper()
	var e store.Event
	if err := st.DB.QueryRow(`SELECT site_id, ts, template, ip, ip_key, visitor, ua FROM event
		WHERE cls = 1 AND kind = 'page_view' ORDER BY ts LIMIT 1 OFFSET 50`).
		Scan(&e.SiteID, &e.TS, &e.Template, &e.IP, &e.IPKey, &e.Visitor, &e.UA); err != nil {
		t.Fatal(err)
	}
	e.Source, e.Cls, e.Path = "browser", store.ClsHuman, e.Template
	var evs []store.Event
	for i, row := range [][3]string{
		{"vital", "LCP", `{"name":"LCP","value":"1200","v":2}`},    // a string value
		{"vital", "LCP", `{"name":"LCP","value":1200,"v":"2"}`},    // a string version
		{"vital", "LCP", `{"name":"LCP","value":1200,"v":2.5}`},    // a fraction
		{"vital", "LCP", `{"name":"LCP","value":1200,"v":2.0}`},    // a real
		{"vital", "LCP", `{"name":"LCP","value":true,"v":2}`},      // no number
		{"vital", "INP", `{"name":"INP","value":-5,"v":2}`},        // negative
		{"vital", "FOO", `{"name":"FOO","value":10,"v":2}`},        // no vital
		{"vital", "CLS", `{"name":"CLS","value":0.0425}`},          // the old script's
		{"vital", "TTFB", `{"name":"TTFB","value":1e300,"v":2}`},   // past the last class
		{"vital", "CLS", `{"name":"CLS","value":0.0425,"v":2}`},    // ×1000, rounded
		{"vital", "LCP", `{"name":"LCP","value":1234.5678,"v":2}`}, // rounded
		{"engagement", "", `{"ms":1500.5}`},
		{"engagement", "", `{"ms":-300}`},
		{"engagement", "", `{"ms":"900"}`},
		{"engagement", "", `{"ms":100000000000000000000}`},
		{"engagement", "", `{"ms":2500,"scripts":["cdn.example.org"]}`},
	} {
		x := e
		x.TS += int64(i + 1)
		x.Kind, x.Name, x.Props = row[0], row[1], `{"p":`+row[2]+`}`
		evs = append(evs, x)
	}
	// Props that are no JSON: they count as nothing, the day's read goes on.
	for _, row := range [][3]string{
		{"vital", "LCP", `{"p":{"name":"LCP","value":1200,"v":2}`}, // cut short
		{"engagement", "", `{"p":{"ms":700`},
		{"engagement", "", `{"p":{"ms":700}}x`}, // trailing text
	} {
		x := e
		x.TS += int64(len(evs) + 1)
		x.Kind, x.Name, x.Props = row[0], row[1], row[2]
		evs = append(evs, x)
	}
	insert(t, st, evs...)
}

// experience counts what a site-day's raw adds to the experience figures: samples per vital, engaged ms, the vitals
// left out, the engagements that add nothing though they carry ms, those clamped, and the rows whose props are no
// JSON.
type experience struct {
	samples                      [len(Vitals)]int64
	engaged                      int64
	left, zero, clamped, invalid int64
}

func (e *experience) add(o experience) {
	for i, n := range o.samples {
		e.samples[i] += n
	}
	e.engaged, e.left, e.zero, e.clamped = e.engaged+o.engaged, e.left+o.left, e.zero+o.zero, e.clamped+o.clamped
	e.invalid += o.invalid
}

// checkExperience compares a site-day's hourly engaged time and vitals and its vital lists' histograms with its raw
// rows' props, parsed here: the builder's SQL fragments are not used, so a wrong one shows.
func checkExperience(t *testing.T, st *store.Store, sd siteDay) experience {
	t.Helper()
	var out experience
	type hc [2]int64
	engaged := map[hc]int64{}
	hours := map[hc]*[len(Vitals)]Hist{}
	lists := map[[2]string]*Hist{} // (dim, template)
	rows, err := st.DB.Query(`SELECT ts / 3600000, CASE cls WHEN 0 THEN 4 ELSE cls END, cls, kind, name, template, props
		FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND kind IN ('vital', 'engagement')`,
		sd.site, sd.day*dayMS, (sd.day+1)*dayMS)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k hc
		var cls int
		var kind, name, tmpl, props string
		if err := rows.Scan(&k[0], &k[1], &cls, &kind, &name, &tmpl, &props); err != nil {
			t.Fatal(err)
		}
		// No JSON (json_valid; Decode would take a value with text after it): nothing.
		if !json.Valid([]byte(props)) {
			out.invalid++
			continue
		}
		var p struct {
			P map[string]any `json:"p"`
		}
		dec := json.NewDecoder(strings.NewReader(props))
		dec.UseNumber()
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if kind == "engagement" {
			raw, ok := p.P["ms"]
			if !ok {
				continue
			}
			n, _ := raw.(json.Number)
			var ms int64
			if n != "" && !strings.ContainsAny(string(n), ".eE") {
				v, err := strconv.ParseInt(string(n), 10, 64)
				switch {
				case err != nil && strings.HasPrefix(string(n), "-"):
				case err != nil || v > 3_600_000:
					ms = 3_600_000
					out.clamped++
				default:
					ms = max(v, 0)
				}
			}
			if ms == 0 && n != "0" {
				out.zero++
			}
			engaged[k] += ms
			out.engaged += ms
			continue
		}
		i := slices.Index(Vitals[:], name)
		v, isNum := p.P["v"].(json.Number)
		val, valNum := p.P["value"].(json.Number)
		f, ferr := val.Float64()
		if i < 0 || !isNum || v != "2" || !valNum || ferr != nil || f < 0 {
			out.left++
			continue
		}
		if name == "CLS" {
			f *= 1000
		}
		u := int64(math.Round(math.Min(f, 3_600_000)))
		if hours[k] == nil {
			hours[k] = &[len(Vitals)]Hist{}
		}
		hours[k][i].Add(u, 1)
		out.samples[i]++
		if dim := map[string]string{"LCP": "vital_lcp", "INP": "vital_inp", "CLS": "vital_cls"}[name]; dim != "" &&
			cls == store.ClsHuman {
			lk := [2]string{dim, tmpl}
			if lists[lk] == nil {
				lists[lk] = &Hist{}
			}
			lists[lk].Add(u, 1)
		}
	}
	_ = rows.Close()

	rows, err = st.DB.Query(`SELECT hour, cls, engaged, v_lcp, v_inp, v_cls, v_fcp, v_ttfb FROM agg_hour
		WHERE site_id = ? AND hour >= ? AND hour < ?`, sd.site, sd.day*24, sd.day*24+24)
	if err != nil {
		t.Fatal(err)
	}
	got := map[hc]bool{}
	for rows.Next() {
		var k hc
		var h store.AggHour
		if err := rows.Scan(&k[0], &k[1], &h.Engaged, &h.Vitals[0], &h.Vitals[1], &h.Vitals[2], &h.Vitals[3],
			&h.Vitals[4]); err != nil {
			t.Fatal(err)
		}
		got[k] = true
		if h.Engaged != engaged[k] {
			t.Errorf("%v hour %v: engaged %d, want %d", sd, k, h.Engaged, engaged[k])
		}
		want := hours[k]
		if want == nil {
			want = &[len(Vitals)]Hist{}
		}
		for i := range Vitals {
			if w := want[i].Marshal(); !bytes.Equal(h.Vitals[i], w) {
				t.Errorf("%v hour %v %s: %x, want %x", sd, k, Vitals[i], h.Vitals[i], w)
			}
		}
	}
	_ = rows.Close()
	for k := range engaged {
		if !got[k] {
			t.Errorf("%v hour %v: no row", sd, k)
		}
	}
	for k := range hours {
		if !got[k] {
			t.Errorf("%v hour %v: no row", sd, k)
		}
	}

	rows, err = st.DB.Query(`SELECT dim, key, durations FROM agg_list WHERE site_id = ? AND dim IN ('vital_lcp',
		'vital_inp', 'vital_cls') AND day = ? AND other = 0`, sd.site, sd.day)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var lk [2]string
		var durations []byte
		if err := rows.Scan(&lk[0], &lk[1], &durations); err != nil {
			t.Fatal(err)
		}
		want := lists[lk]
		if want == nil {
			t.Errorf("%v %v: stored, no sample", sd, lk)
			continue
		}
		if !bytes.Equal(durations, want.Marshal()) {
			t.Errorf("%v %v: %x, want %x", sd, lk, durations, want.Marshal())
		}
	}
	return out
}

func checkHours(t *testing.T, st *store.Store, sd siteDay, classes map[int]bool) {
	t.Helper()
	got := map[[2]int64]store.AggHour{}
	rows, err := st.DB.Query(`SELECT hour, cls, events, views, actions, starts, requests, refused, errors, browser_ids,
		addrs, server_addrs, failed, durations, callers, accounts FROM agg_hour WHERE site_id = ? AND hour >= ? AND hour < ?`,
		sd.site, sd.day*24, sd.day*24+24)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var h store.AggHour
		if err := rows.Scan(&h.Hour, &h.Cls, &h.Events, &h.Views, &h.Actions, &h.Starts, &h.Requests, &h.Refused,
			&h.Errors, &h.BrowserIDs, &h.Addrs, &h.ServerAddrs, &h.Failed, &h.Durations, &h.Callers, &h.Accounts); err != nil {
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
		COUNT(DISTINCT CASE WHEN source = 'server' THEN ip_key END),
		SUM(source = 'server' AND status != 404 AND NOT (unrouted = 1 AND status IN (401, 403, 405)) AND (err != '' OR status >= 400)),
		SUM(source = 'server' AND status != 404 AND NOT (unrouted = 1 AND status IN (401, 403, 405))),
		COUNT(DISTINCT CASE WHEN source = 'server' AND status != 404 AND NOT (unrouted = 1 AND status IN (401, 403, 405)) THEN ip_key END),
		COUNT(DISTINCT CASE WHEN source = 'server' AND status != 404 AND NOT (unrouted = 1 AND status IN (401, 403, 405)) AND account != '' THEN account END)
		FROM event WHERE site_id = ? AND ts >= ? AND ts < ? GROUP BY hour, c`, sd.site, sd.day*dayMS, (sd.day+1)*dayMS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	want := 0
	for rows.Next() {
		var w store.AggHour
		var ids, addrs, srv, calls, callers, accounts int64
		if err := rows.Scan(&w.Hour, &w.Cls, &w.Events, &w.Views, &w.Actions, &w.Starts, &w.Requests, &w.Refused,
			&w.Errors, &ids, &addrs, &srv, &w.Failed, &calls, &callers, &accounts); err != nil {
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
			g.Requests != w.Requests || g.Refused != w.Refused || g.Errors != w.Errors || g.Failed != w.Failed {
			t.Errorf("%v hour %d cls %d: %+v, want %+v", sd, w.Hour, w.Cls, g, w)
		}
		var durations Hist
		if err := durations.AddEncoded(g.Durations); err != nil || durations.Count() != calls {
			t.Errorf("%v hour %d cls %d: %d durations (%v), want %d", sd, w.Hour, w.Cls, durations.Count(), err, calls)
		}
		if c := sketchCount(t, g.Callers); !near(c, callers) {
			t.Errorf("%v hour %d cls %d: callers %d, want %d", sd, w.Hour, w.Cls, c, callers)
		}
		if c := sketchCount(t, g.Accounts); !near(c, accounts) {
			t.Errorf("%v hour %d cls %d: accounts %d, want %d", sd, w.Hour, w.Cls, c, accounts)
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
	"endpoints": {"source = 'server' AND status != 404 AND NOT (unrouted = 1 AND status IN (401, 403, 405))", "CASE WHEN op = '' THEN template ELSE template || ' ' || op END",
		"MAX(op)", "COUNT(*)", "SUM(err != '' OR status >= 400)", "ip_key", "''"},
	"accounts": {"source = 'server' AND status != 404 AND NOT (unrouted = 1 AND status IN (401, 403, 405)) AND account != ''", "account", "''", "COUNT(*)",
		"SUM(err != '' OR status >= 400)", "ip_key", "''"},
	"agents": {addr: "''"}, // Client has no SQL (wantAgents)
	"failures": {"source = 'server' AND status != 404 AND NOT (unrouted = 1 AND status IN (401, 403, 405)) AND (err != '' OR status >= 400)",
		"CASE WHEN err != '' THEN err ELSE 'HTTP ' || status END", "''", "COUNT(*)", "0", "ip_key", "''"},
	"services": {"source = 'server' AND svc != ''", "svc", "MAX(CASE WHEN cls = 3 THEN 'monitor' ELSE 'bot' END)",
		"COUNT(*)", "SUM(status >= 400)", "ip_key", "''"},
	"engaged": {"cls = 1 AND kind IN ('page_view', 'engagement')", "template", "''",
		"SUM(CASE WHEN kind != 'engagement' OR NOT json_valid(props) THEN 0 ELSE CASE WHEN json_type(props, '$.p.ms') = 'integer' THEN MAX(0, MIN(3600000, json_extract(props, '$.p.ms'))) ELSE 0 END END)",
		"SUM(kind = 'page_view')", "visitor", "''"},
	"vital_lcp": vitalSQL("LCP"),
	"vital_inp": vitalSQL("INP"),
	"vital_cls": vitalSQL("CLS"),
}

// vitalSQL is a vital's dimension: people's vitals of version 2 with a number ≥ 0 for a value, by template; props
// that are no JSON none.
func vitalSQL(name string) struct{ where, key, sub, a, b, id, addr string } {
	return struct{ where, key, sub, a, b, id, addr string }{"cls = 1 AND kind = 'vital' AND name = '" + name + `'
		AND CASE WHEN json_valid(props) THEN json_type(props, '$.p.v') = 'integer' AND json_extract(props, '$.p.v') = 2
		AND json_type(props, '$.p.value') IN ('integer', 'real') AND json_extract(props, '$.p.value') >= 0 END`,
		"template", "''", "COUNT(*)", "0", "visitor", "''"}
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
	if dim == "agents" {
		return wantAgents(t, st, sd)
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

// wantAgents is the agents dimension from plain rows.
func wantAgents(t *testing.T, st *store.Store, sd siteDay) []want {
	t.Helper()
	rows, err := st.DB.Query(`SELECT `+partSQL+`, ua, ip_key, err != '' OR status >= 400 FROM event
		WHERE site_id = ? AND ts >= ? AND ts < ? AND source = 'server' AND status != 404
		AND NOT (unrouted = 1 AND status IN (401, 403, 405))`, sd.site, sd.day*dayMS, (sd.day+1)*dayMS)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	type k struct {
		part int
		key  string
	}
	sums, ids := map[k]*want{}, map[k]map[string]bool{}
	for rows.Next() {
		var part int
		var ua, ip string
		var failed int64
		if err := rows.Scan(&part, &ua, &ip, &failed); err != nil {
			t.Fatal(err)
		}
		x := k{part, classify.Client(ua)}
		if sums[x] == nil {
			sums[x], ids[x] = &want{part: part, key: x.key}, map[string]bool{}
		}
		sums[x].a++
		sums[x].b += failed
		ids[x][ip] = true
	}
	var out []want
	for x, w := range sums {
		w.n = int64(len(ids[x]))
		out = append(out, *w)
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

	// What people experienced: the hours keep every class's, the lists people's only; a named bot's requests in the
	// automated part.
	exp := func(ts int64, kind, name string, cls int, props string) store.Event {
		return store.Event{SiteID: id, TS: ts, Source: "browser", Kind: kind, Name: name, Visitor: "v2", SID: "s2",
			IP: "192.0.2.17", IPKey: "192.0.2.17", Cls: cls, Template: "/", Props: props}
	}
	insert(t, st,
		exp(t0+5000, "engagement", "", 1, `{"p":{"ms":1200}}`),
		exp(t0+5001, "engagement", "", 2, `{"p":{"ms":800}}`),
		exp(t0+5002, "vital", "LCP", 1, `{"p":{"name":"LCP","value":1500,"v":2}}`),
		exp(t0+5003, "vital", "LCP", 2, `{"p":{"name":"LCP","value":2500,"v":2}}`),
		store.Event{SiteID: id, TS: t0 + 5004, Source: "server", Kind: "request", IP: "192.0.2.18", IPKey: "192.0.2.18",
			Template: "/", Status: 403, Cls: store.ClsBot, Svc: "Examplebot"},
	)
	if clean, err := b.BuildDay(t.Context(), id, day); err != nil || !clean {
		t.Fatalf("rebuild: clean %v, %v", clean, err)
	}
	hourly := map[int][2]int64{}
	rows, err = st.DB.Query(`SELECT cls, engaged, v_lcp FROM agg_hour WHERE site_id = ? AND hour = ?`, id, t0/hourMS)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cls int
		var engaged int64
		var lcp []byte
		if err := rows.Scan(&cls, &engaged, &lcp); err != nil {
			t.Fatal(err)
		}
		var h Hist
		if err := h.AddEncoded(lcp); err != nil {
			t.Fatal(err)
		}
		hourly[cls] = [2]int64{engaged, h.Count()}
	}
	_ = rows.Close()
	if hourly[1] != [2]int64{1200, 1} || hourly[2] != [2]int64{800, 1} {
		t.Errorf("hourly engaged and LCP samples by class: %v", hourly)
	}
	var lists []string
	rows, err = st.DB.Query(`SELECT dim, part, key, sub, a, b FROM agg_list WHERE site_id = ? AND day = ?
		AND dim IN ('engaged', 'vital_lcp', 'services') ORDER BY dim, part`, id, day)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var dim, key, sub string
		var part int
		var sumA, sumB int64
		if err := rows.Scan(&dim, &part, &key, &sub, &sumA, &sumB); err != nil {
			t.Fatal(err)
		}
		lists = append(lists, fmt.Sprint(dim, " ", part, " ", key, " ", sub, " ", sumA, " ", sumB))
	}
	_ = rows.Close()
	expected := []string{"engaged 1 /  1200 2", "services 2 Examplebot bot 1 1", "vital_lcp 1 /  1 0"}
	if !slices.Equal(lists, expected) {
		t.Errorf("lists %q, want %q", lists, expected)
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
			IP: "192.0.2.8", IPKey: "192.0.2.8", Cls: 1},
		store.Event{SiteID: id, TS: t0 - 500, Source: "browser", Kind: "engagement", Visitor: "v", SID: "s",
			IP: "192.0.2.8", IPKey: "192.0.2.8", Cls: 1, Props: `{"p":{"ms":400}}`},
		store.Event{SiteID: id, TS: t0 - 400, Source: "browser", Kind: "vital", Name: "INP", Visitor: "v", SID: "s",
			IP: "192.0.2.8", IPKey: "192.0.2.8", Cls: 1, Props: `{"p":{"name":"INP","value":80,"v":2}}`})
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
	// Its experience stands: the hour's and the lists'.
	var engaged, listed, samples int64
	var inp []byte
	if err := st.DB.QueryRow(`SELECT engaged, v_inp FROM agg_hour WHERE site_id = ? AND hour = ?`, id,
		(t0-500)/hourMS).Scan(&engaged, &inp); err != nil || engaged != 400 || inp == nil {
		t.Errorf("purged day's hour: engaged %d, INP %x, %v", engaged, inp, err)
	}
	if err := st.DB.QueryRow(`SELECT SUM(CASE dim WHEN 'engaged' THEN a END), SUM(CASE dim WHEN 'vital_inp' THEN a END)
		FROM agg_list WHERE site_id = ? AND day = ?`, id, day-1).Scan(&listed, &samples); err != nil || listed != 400 ||
		samples != 1 {
		t.Errorf("purged day's lists: engaged %d, INP samples %d, %v", listed, samples, err)
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
	// The day's read takes the experience's fields, never props whole: a day's props are most of its bytes.
	read := false
	for q := range seen {
		read = read || strings.Contains(q, EngagedSQL) && strings.Contains(q, VitalValueSQL) &&
			strings.Contains(q, VitalVerSQL) && strings.Contains(q, " svc,")
		if strings.Count(q, "props") != strings.Count(q, "(props, ")+strings.Count(q, "json_valid(props)") {
			t.Errorf("props read whole: %s", q)
		}
	}
	if !read {
		t.Error("no read of the experience's fields traced")
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

// A day's names are given before every build reads it: the build names a bot without one and takes a stray name off a
// person and comes out clean in the same call. A day named already moves no version, one whose names the list changed
// takes the new ones without requeueing the next day, a bot the list names nobody asks once a build, and the job's
// passes end. A nil hook builds as stored.
func TestBuild_NamesBeforeFreezing(t *testing.T) {
	day := anchor.UnixMilli()/dayMS - 5
	t0 := day * dayMS
	req := func(id, ts int64, ip string, cls, status int, svc string) store.Event {
		return store.Event{SiteID: id, TS: ts, Source: "server", Kind: "request", IP: ip, IPKey: ip, UA: "Examplebot/1",
			Template: "/", Status: status, Cls: cls, Svc: svc}
	}
	rows := func(id int64) []store.Event {
		return []store.Event{
			req(id, t0+1000, "192.0.2.50", store.ClsBot, 200, ""),
			req(id, t0+2000, "192.0.2.50", store.ClsBot, 404, ""),
			req(id, t0+3000, "192.0.2.60", store.ClsMonitor, 200, ""),
			req(id, t0+4000, "192.0.2.70", store.ClsHuman, 200, "Stale"),
		}
	}
	names := map[string]string{"192.0.2.50": "Examplebot", "192.0.2.60": "Examplemonitor"}
	services := func(st *store.Store, id int64) string {
		t.Helper()
		r, err := st.DB.Query(`SELECT key, sub, a, b FROM agg_list WHERE site_id = ? AND dim = 'services' AND day = ?
			ORDER BY key`, id, day)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Close() }()
		var out []string
		for r.Next() {
			var key, sub string
			var a, b int64
			if err := r.Scan(&key, &sub, &a, &b); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprint(key, " ", sub, " ", a, " ", b))
		}
		return strings.Join(out, ", ")
	}
	version := func(st *store.Store, id int64) int64 {
		t.Helper()
		days, err := st.AggDays(t.Context(), id, day, day)
		if err != nil {
			t.Fatal(err)
		}
		return days[day].Version
	}

	st := openStore(t)
	id := site(t, st)
	insert(t, st, rows(id)...)
	var calls int
	b := &Builder{Store: st, Name: func(ctx context.Context, site, day int64) (int64, error) {
		calls++
		return st.NameSvcDay(ctx, site, day, func(ip, _ string, _ int) string { return names[ip] })
	}}
	if clean, err := b.BuildDay(t.Context(), id, day); err != nil || !clean || calls != 1 {
		t.Fatalf("named build: clean %v, %d calls, %v", clean, calls, err)
	}
	const want = "Examplebot bot 2 1, Examplemonitor monitor 1 0"
	if got := services(st, id); got != want {
		t.Errorf("services %q, want %q", got, want)
	}
	v := version(st, id)
	if clean, err := b.BuildDay(t.Context(), id, day); err != nil || !clean || calls != 2 || version(st, id) != v {
		t.Errorf("named day rebuilt: clean %v, %d calls, version %d → %d, %v", clean, calls, v, version(st, id), err)
	}

	// A list that renames a service: a day whose bots all carry the old name takes the new one, and the next day,
	// whose carried sessions a name does not change, stays clean.
	insert(t, st, store.Event{SiteID: id, TS: t0 + dayMS + 1000, Source: "browser", Kind: "page_view", Visitor: "v",
		SID: "s", IP: "192.0.2.9", IPKey: "192.0.2.9", Cls: store.ClsHuman})
	if clean, err := (&Builder{Store: st}).BuildDay(t.Context(), id, day+1); err != nil || !clean {
		t.Fatalf("next day: clean %v, %v", clean, err)
	}
	names["192.0.2.50"] = "Renamedbot"
	if clean, err := b.BuildDay(t.Context(), id, day); err != nil || !clean || calls != 3 {
		t.Errorf("renamed build: clean %v, %d calls, %v", clean, calls, err)
	}
	const renamed = "Examplemonitor monitor 1 0, Renamedbot bot 2 1"
	if got := services(st, id); got != renamed {
		t.Errorf("services %q, want %q", got, renamed)
	}
	if days, err := st.AggDays(t.Context(), id, day+1, day+1); err != nil || !days[day+1].BuiltBy(Version) {
		t.Errorf("next day after a renaming: %+v, %v", days[day+1], err)
	}

	// A bot the list names nobody: the hook once a build, nothing changed, nothing read again.
	insert(t, st, req(id, t0+5000, "192.0.2.80", store.ClsBot, 200, ""))
	v = version(st, id)
	for i := range 2 {
		if clean, err := b.BuildDay(t.Context(), id, day); err != nil || !clean || calls != 4+i || version(st, id) != v {
			t.Errorf("build %d: clean %v, %d calls, version %d → %d, %v", i, clean, calls, v, version(st, id), err)
		}
	}
	if got := services(st, id); got != renamed {
		t.Errorf("services %q, want %q", got, renamed)
	}
	// The job: every day of it built, then nothing left.
	if _, err := st.DB.Exec(`UPDATE agg_day SET dirty = 1`); err != nil {
		t.Fatal(err)
	}
	j := &job{b: b, log: slog.New(slog.DiscardHandler)}
	if _, err := j.passes(t.Context(), anchor.UnixMilli()/dayMS, 0); err != nil {
		t.Fatal(err)
	}
	if n, err := j.pass(t.Context(), anchor.UnixMilli()/dayMS); err != nil || n != 0 {
		t.Errorf("a pass after: %d built, %v", n, err)
	}

	// No hook: the names as stored.
	bare := openStore(t)
	bid := site(t, bare)
	insert(t, bare, rows(bid)...)
	if clean, err := (&Builder{Store: bare}).BuildDay(t.Context(), bid, day); err != nil || !clean {
		t.Fatalf("bare build: clean %v, %v", clean, err)
	}
	if got := services(bare, bid); got != "Stale bot 1 0" {
		t.Errorf("services without naming %q", got)
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
