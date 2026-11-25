package dispatch

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/dash"
	"github.com/trick77/trakk/backend/internal/insight"
	"github.com/trick77/trakk/backend/internal/sites"
	"github.com/trick77/trakk/backend/internal/store"
	"github.com/trick77/trakk/backend/internal/watch"
)

// evalRig is a store with a website with a feed and an API, the evaluator reading it.
type evalRig struct {
	t        *testing.T
	st       *store.Store
	reg      *sites.Registry
	web, api *sites.Site
	now      time.Time
}

func newEvalRig(t *testing.T) *evalRig {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rows, err := st.SyncSites(t.Context(), []store.Site{
		{Domain: "example.com", Kind: "site", Product: "Lens", FeedKeyHash: sites.HashKey("k"), PathRules: "[]", Active: true},
		{Domain: "api.example.com", Kind: "api", Product: "Lens", FeedKeyHash: sites.HashKey("a"), PathRules: "[]",
			Active: true}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := sites.New(rows)
	if err != nil {
		t.Fatal(err)
	}
	r := &evalRig{t: t, st: st, reg: reg, now: time.Date(2026, 11, 22, 12, 30, 0, 0, time.UTC)}
	r.web, _ = reg.Lookup("example.com")
	r.api, _ = reg.Lookup("api.example.com")
	return r
}

// requests inserts n server requests on site from addr, ago before now, a second apart.
func (r *evalRig) requests(site *sites.Site, ago time.Duration, n int, addr string, mod func(*store.Event)) {
	r.t.Helper()
	evs := make([]store.Event, n)
	for i := range evs {
		evs[i] = store.Event{SiteID: site.ID, TS: r.now.Add(-ago).UnixMilli() + int64(i)*1000, Source: "server",
			Kind: "request", IP: addr, IPKey: addr, Template: "GET /", Path: "/", Method: "GET", Status: 200, DurMS: 20,
			Cls: store.ClsUnknown}
		if mod != nil {
			mod(&evs[i])
		}
	}
	if err := r.st.InsertEvents(r.t.Context(), evs); err != nil {
		r.t.Fatal(err)
	}
}

func (r *evalRig) eval(rules string, started time.Time, model insight.Model) (*Eval, alert.Config) {
	r.t.Helper()
	c := alert.Parse([]byte(`{"rules": [`+rules+`]}`), alert.Known{Domains: []string{"example.com", "api.example.com"},
		Products: []string{"Lens"}})
	if len(c.Faults) > 0 {
		r.t.Fatal(c.Faults)
	}
	d := &dash.Server{DB: r.st, Sites: r.reg, RetentionMonths: 25}
	return NewEval(d, c.Rules, started, model), c
}

func (r *evalRig) occ(v *Eval, rule alert.Rule, site *sites.Site) ([]alert.Occurrence, bool) {
	r.t.Helper()
	occ, ok, err := v.Occurrences(r.t.Context(), r.now, rule, site, true)
	if err != nil {
		r.t.Fatal(err)
	}
	return occ, ok
}

func keys(occ []alert.Occurrence) string {
	var out []string
	for _, o := range occ {
		k := o.Key
		if o.Hold {
			k += "(hold)"
		}
		if o.Quiet {
			k += "(quiet)"
		}
		out = append(out, k)
	}
	return strings.Join(out, ",")
}

// Facts: computed once at the loosest thresholds, each rule filtering by its own; hostile text defused.
func TestEvalFacts(t *testing.T) {
	r := newEvalRig(t)
	for i, msg := range []string{"TypeError: a", "TypeError: a", "TypeError: a", "see https://evil.example.com/@x"} {
		r.requests(r.web, 3*time.Hour+time.Duration(i)*time.Minute, 1, "192.0.2.9", func(e *store.Event) {
			e.Source, e.Kind, e.Name, e.UA = "browser", "js_error", msg, "Mozilla/5.0"
		})
	}
	r.requests(r.web, 30*time.Hour, 20, "192.0.2.1", nil)    // the comparison: a little
	r.requests(r.web, 3*time.Hour, 400, "198.51.100.7", nil) // one address, most of the requests
	r.requests(r.web, 2*time.Hour, 50, "192.0.2.1", nil)
	v, c := r.eval(`{"name": "errs", "on": "error.new", "min": 3}, {"name": "any-err", "on": "error.new"},
		{"name": "flood", "on": "spike.addr", "min": 300, "share": 50, "factor": 2},
		{"name": "huge", "on": "spike.addr", "share": 95}`, time.Time{}, nil)
	if occ, _ := r.occ(v, c.Rules[0], r.web); keys(occ) != "TypeError: a" || *occ[0].Figure != 3 || occ[0].Unit != "times" {
		t.Errorf("min 3: %s %+v", keys(occ), occ)
	}
	occ, _ := r.occ(v, c.Rules[1], r.web)
	if len(occ) != 2 {
		t.Fatalf("min 1: %s", keys(occ))
	}
	// Stored as it came, on one line: the message defuses it (TestFacts), the dashboard shows it.
	for _, o := range occ {
		if strings.Contains(o.Text, "\n") || (strings.Contains(o.Key, "evil") && !strings.Contains(o.Text, "https://")) {
			t.Errorf("text %q", o.Text)
		}
	}
	if occ, _ := r.occ(v, c.Rules[2], r.web); keys(occ) != "198.51.100.7" || occ[0].Target.Panel != "visitors" ||
		!strings.HasPrefix(occ[0].Text, "A spike of requests from 198.51.100.7") {
		t.Errorf("spike: %s %+v", keys(occ), occ)
	}
	if occ, _ := r.occ(v, c.Rules[3], r.web); len(occ) != 0 {
		t.Errorf("share 95: %s", keys(occ))
	}
	// Fact rules are not evaluated inside an hour.
	if _, ok, _ := v.Occurrences(t.Context(), r.now, c.Rules[0], r.web, false); ok {
		t.Error("facts evaluated outside a facts pass")
	}
}

// The watch's findings (an answered probe, a new target, a new script host) are no rule's: a website's new target
// shares its id's prefix with an API's new endpoint and still never reads as one, at any min.
func TestEvalWatchFindings(t *testing.T) {
	r := newEvalRig(t)
	req := func(ago time.Duration, addr, tmpl string, status int, bytes int) {
		r.requests(r.web, ago, 1, addr, func(e *store.Event) {
			e.Template, e.Method, e.Status, e.Unrouted = tmpl, strings.Fields(tmpl)[0], status, true
			e.Props = fmt.Sprintf(`{"bytes":%d}`, bytes)
		})
	}
	engage := func(ago time.Duration, addr, hosts string) {
		r.requests(r.web, ago, 1, addr, func(e *store.Event) {
			e.Source, e.Kind, e.Visitor, e.Props = "browser", "engagement", "v-"+addr, `{"p":{"scripts":[`+hosts+`]}}`
		})
	}
	req(240*time.Hour, "192.0.2.1", "GET /", 200, 900)
	engage(240*time.Hour, "192.0.2.1", `"cdn.example.org"`)
	// Caught up on its first day: a backfill finds nothing.
	if err := watch.Pass(t.Context(), r.st, r.now.Add(-236*time.Hour)); err != nil {
		t.Fatal(err)
	}
	req(72*time.Hour, "198.51.100.7", "GET /cache.php", 404, 153)
	req(20*time.Hour, "203.0.113.5", "GET /cache.php", 200, 48_000)
	req(19*time.Hour, "192.0.2.11", "POST /xmlrpc.php", 200, 700)
	for i, addr := range []string{"192.0.2.10", "198.51.100.11", "203.0.113.12"} {
		engage(18*time.Hour+time.Duration(i)*time.Minute, addr, `"cdn.example.org","pay.example.net"`)
	}
	if err := watch.Pass(t.Context(), r.st, r.now); err != nil {
		t.Fatal(err)
	}
	fs, err := watch.Findings(t.Context(), r.st.DB, r.web.ID, r.now.Add(-24*time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, f := range fs {
		if !f.Provisional {
			found[f.Kind+":"+f.Key] = true
		}
	}
	for _, id := range []string{"flip:GET /cache.php", "endpoint:POST /xmlrpc.php", "script:pay.example.net"} {
		if !found[id] {
			t.Fatalf("the watch found %v, no %s", found, id)
		}
	}
	v, c := r.eval(`{"name": "e", "on": "endpoint.new", "min": 0}, {"name": "p", "on": "probe.new", "min": 0},
		{"name": "r", "on": "referrer.new", "min": 0}, {"name": "x", "on": "error.new", "min": 0}`, time.Time{}, nil)
	for _, rule := range c.Rules {
		occ, ok := r.occ(v, rule, r.web)
		if !ok {
			t.Errorf("%s: no judgement", rule.Name)
		}
		for _, o := range occ {
			if o.Key == "GET /cache.php" || o.Key == "POST /xmlrpc.php" || o.Key == "pay.example.net" {
				t.Errorf("%s matched the watch's %s", rule.Name, o.Key)
			}
		}
	}
}

// error.new: visitors beside min, an extension's error left out by default, the place and share in the detail, the
// log's own rows as the target; a server error by its endpoint, never filtered by origin.
func TestEvalErrors(t *testing.T) {
	r := newEvalRig(t)
	jsErr := func(msg, src string, visitors ...int) {
		for _, v := range visitors {
			r.requests(r.web, 3*time.Hour, 1, "192.0.2.9", func(e *store.Event) {
				e.Source, e.Kind, e.Name, e.UA, e.Visitor = "browser", "js_error", msg, "Mozilla/5.0", fmt.Sprint("v", v)
				e.Props = `{"p":{"src":"` + src + `","line":1,"col":7,"type":"TypeError","stack":["at x (a.js:1:7)"]}}`
			})
			r.requests(r.web, 3*time.Hour, 1, "192.0.2.9", func(e *store.Event) {
				e.Source, e.Kind, e.UA, e.Visitor = "browser", "page_view", "Mozilla/5.0", fmt.Sprint("v", v)
			})
		}
	}
	jsErr("TypeError: own *x*", "https://example.com/assets/a.js", 1, 2)
	jsErr("TypeError: ext", "chrome-extension://abc/content.js", 3, 4, 5)
	jsErr("TypeError: lone", "https://example.com/assets/a.js", 6)
	r.requests(r.web, 2*time.Hour, 3, "192.0.2.20", func(e *store.Event) { e.Status, e.Template = 502, "GET /api/x" })
	v, c := r.eval(`{"name": "def", "on": "error.new"}, {"name": "two", "on": "error.new", "visitors": 2},
		{"name": "ext", "on": "error.new", "origins": ["extension"]}`, time.Time{}, nil)
	occ, _ := r.occ(v, c.Rules[0], r.web)
	byKey := map[string]alert.Occurrence{}
	for _, o := range occ {
		byKey[o.Key] = o
	}
	if _, ok := byKey["TypeError: ext"]; ok || len(occ) != 3 {
		t.Fatalf("default origins: %s", keys(occ))
	}
	own := byKey["TypeError: own *x*"]
	if own.Target.Panel != "log" || own.Target.Filter["kind"] != "js_error" || own.Target.Filter["name"] != "TypeError: own *x*" ||
		!strings.Contains(own.Detail, "TypeError at own /assets/a.js:1:7") || !strings.Contains(own.Detail, "2 visitors") ||
		!strings.Contains(own.Detail, "% of 6 who saw a page") || strings.Contains(own.Detail, "at x (") ||
		own.Text != "New error: TypeError: own *x*" {
		t.Errorf("own %+v", own)
	}
	srv := byKey["GET /api/x"]
	if srv.Target.Filter["path"] != "GET /api/x" || srv.Target.Filter["status"] != "5xx" || srv.Detail != "" {
		t.Errorf("server %+v", srv)
	}
	if occ, _ := r.occ(v, c.Rules[1], r.web); keys(occ) != "TypeError: own *x*" && keys(occ) != "TypeError: own *x*,GET /api/x" {
		t.Errorf("two visitors: %s", keys(occ))
	}
	if occ, _ := r.occ(v, c.Rules[2], r.web); keys(occ) != "TypeError: ext,GET /api/x" &&
		keys(occ) != "GET /api/x,TypeError: ext" {
		t.Errorf("extension: %s", keys(occ))
	}
	if len(v.errors) != 3 {
		t.Errorf("%d script errors looked at, want each new one once", len(v.errors))
	}
}

// The rules see every new error, past Noteworthy's ten; the script errors are looked at ten a site and pass, the
// rest held until the next.
func TestEvalManyErrors(t *testing.T) {
	r := newEvalRig(t)
	for i := range 12 {
		msg := fmt.Sprintf("TypeError: e%02d", i)
		r.requests(r.web, 3*time.Hour, 1+i, "192.0.2.9", func(e *store.Event) {
			e.Source, e.Kind, e.Name, e.UA, e.Visitor = "browser", "js_error", msg, "Mozilla/5.0", "v1"
		})
	}
	v, c := r.eval(`{"name": "def", "on": "error.new"}`, time.Time{}, nil)
	count := func(occ []alert.Occurrence) (seen, held int) {
		for _, o := range occ {
			if o.Hold {
				held++
			} else {
				seen++
			}
		}
		return seen, held
	}
	occ, _ := r.occ(v, c.Rules[0], r.web)
	if seen, held := count(occ); seen != 10 || held != 2 {
		t.Fatalf("first pass: %d seen, %d held (%s)", seen, held, keys(occ))
	}
	r.now = r.now.Add(time.Minute) // the next pass: the two left are looked at, the ten known not again
	occ, _ = r.occ(v, c.Rules[0], r.web)
	if seen, held := count(occ); seen != 12 || held != 0 || v.looked[r.web.ID] != 2 {
		t.Fatalf("second pass: %d seen, %d held, %d looked at", seen, held, v.looked[r.web.ID])
	}
}

// A gap in a list's reads is no judgement: the rule is not evaluated, nothing left.
func TestEvalGap(t *testing.T) {
	r := newEvalRig(t)
	r.requests(r.web, 3*time.Hour, 1, "192.0.2.9", func(e *store.Event) {
		e.Source, e.Kind, e.Name, e.UA = "browser", "js_error", "TypeError: a", "Mozilla/5.0"
	})
	v, c := r.eval(`{"name": "def", "on": "error.new"}, {"name": "p", "on": "probe.new"}`, time.Time{}, nil)
	if _, ok := r.occ(v, c.Rules[0], r.web); !ok {
		t.Fatal("no gap: not evaluated")
	}
	// Retention cut yesterday's raw, nothing built from it: the comparison has a gap.
	day := r.now.UnixMilli()/86_400_000 - 1
	if _, err := r.st.DB.Exec(`INSERT INTO agg_day (site_id, day, raw, version) VALUES (?, ?, 0, 1)`, r.web.ID,
		day); err != nil {
		t.Fatal(err)
	}
	v, c = r.eval(`{"name": "def", "on": "error.new"}`, time.Time{}, nil)
	if occ, ok := r.occ(v, c.Rules[0], r.web); ok {
		t.Fatalf("a gap judged: %s", keys(occ))
	}
}

// The loosest thresholds per site: a rule loosens only the sites it selects.
func TestLoosen(t *testing.T) {
	r := newEvalRig(t)
	v, _ := r.eval(`{"name": "a", "on": "spike.addr", "min": 10, "share": 5, "factor": 1.5, "sites": ["api.example.com"]},
		{"name": "b", "on": "trend.visitors", "min": 5, "change": 10}, {"name": "c", "on": "probe.new", "min": 1},
		{"name": "d", "on": "trend.failed", "min": 50, "change": 2}, {"name": "e", "on": "trend.auto_share", "change": 1},
		{"name": "f", "on": "failure.new", "min": 1}, {"name": "g", "on": "endpoint.new", "min": 1},
		{"name": "h", "on": "referrer.new", "min": 1}, {"name": "i", "on": "spike.net", "min": 10},
		{"name": "j", "on": "trend.calls", "min": 10, "change": 5}, {"name": "k", "on": "error.new", "min": 0},
		{"name": "off", "on": "probe.new", "min": 0, "enabled": false}`, time.Time{}, nil)
	web, api := v.loosest[r.web.ID], v.loosest[r.api.ID]
	def := dash.DefaultThresholds()
	if web.SpikeAddr != def.SpikeAddr || api.SpikeAddr != (dash.Spike{Min: 10, Share: 0.05, Growth: 1.5}) {
		t.Errorf("spike: web %+v, api %+v", web.SpikeAddr, api.SpikeAddr)
	}
	if web.TrendVisitors != 5 || web.TrendVisitorsPct != 10 || web.NewProbes != 1 || web.FailedCalls != 50 ||
		web.FailedPP != 2 || web.AutoSharePP != 1 || web.NewFailures != 1 || web.NewEndpoints != 1 ||
		web.NewReferrers != 1 || web.SpikeNet.Min != 10 || web.TrendCalls != 10 || web.TrendCallsPct != 5 ||
		web.NewErrors != 0 {
		t.Errorf("web %+v", web)
	}
}

// factOccurrences filters each kind by its own parameters.
func TestFactFilter(t *testing.T) {
	rule := func(on, extra string) alert.Rule {
		c := alert.Parse([]byte(`{"rules": [{"name": "x", "on": "`+on+`"`+extra+`}]}`), alert.Known{})
		if len(c.Faults) > 0 {
			t.Fatal(c.Faults)
		}
		return c.Rules[0]
	}
	fs := []insight.Fact{
		{ID: "trend:visitors:up", Text: "Visitors up", Value: 30, Change: 25},
		{ID: "trend:failed:down", Text: "Failed down", Value: 3, Change: -6, Volume: 150},
		{ID: "trend:auto_share:up", Text: "Unknown up", Value: 40, Change: 7},
		{ID: "spike:net:64500", Text: "AS64500", Value: 900, Share: 0.5, Growth: math.Inf(1)},
		{ID: "probe:GET /.env", Text: "New probed path: GET /.env", Value: 4, Unit: "probes of"},
	}
	for _, tc := range []struct {
		r    alert.Rule
		want string
	}{
		{rule("trend.visitors", ""), "up"},
		{rule("trend.visitors", `, "direction": "down"`), ""},
		{rule("trend.visitors", `, "change": 30`), ""},
		{rule("trend.failed", `, "direction": "down"`), "down"},
		{rule("trend.failed", `, "min": 200`), ""},
		{rule("trend.auto_share", `, "change": 8`), ""},
		{rule("trend.auto_share", ""), "up"},
		{rule("spike.net", ""), "64500"},
		{rule("spike.net", `, "share": 60`), ""},
		{rule("probe.new", `, "min": 5`), ""},
		{rule("probe.new", ""), "GET /.env"},
	} {
		occ := factOccurrences(tc.r, fs)
		if got := keys(occ); got != tc.want {
			t.Errorf("%s %+v: %q, want %q", tc.r.Spec.ID, tc.r.P, got, tc.want)
		}
	}
	occ := factOccurrences(rule("trend.failed", ""), fs)
	if occ[0].Unit != "pp" || *occ[0].Figure != -6 {
		t.Errorf("trend figure %+v", occ[0])
	}
	if occ := factOccurrences(rule("probe.new", ""), fs); occ[0].Unit != "probes" || *occ[0].Figure != 4 {
		t.Errorf("probe figure %+v", occ[0])
	}
}

// source.silent: a source that delivered before and not for after; within after of the start it holds.
func TestEvalSilent(t *testing.T) {
	r := newEvalRig(t)
	r.requests(r.web, 3*time.Hour, 5, "192.0.2.1", nil)
	r.requests(r.web, 10*time.Minute, 1, "192.0.2.1", func(e *store.Event) { e.Source, e.Kind = "browser", "page_view" })
	hour := func(ago time.Duration) int64 { return r.now.Add(-ago).Unix() / 3600 }
	if err := r.st.AddStats(t.Context(), []store.IngestStat{
		{SiteID: r.web.ID, Hour: hour(3 * time.Hour), Accepted: 5, AcceptedServer: 5},
		{SiteID: r.web.ID, Hour: hour(10 * time.Minute), Accepted: 1}}); err != nil {
		t.Fatal(err)
	}
	v, c := r.eval(`{"name": "s", "on": "source.silent", "after": "1h"},
		{"name": "b", "on": "source.silent", "after": "1h", "source": "browser"}`, time.Time{}, nil)
	occ, _ := r.occ(v, c.Rules[0], r.web)
	if keys(occ) != "server" || !strings.Contains(occ[0].Text, "feed for 2 h 59 min") || occ[0].Unit != "min" {
		t.Errorf("silent %s %+v", keys(occ), occ)
	}
	if occ, _ := r.occ(v, c.Rules[1], r.web); len(occ) != 0 {
		t.Errorf("browser %s", keys(occ))
	}
	if occ, _ := r.occ(v, c.Rules[0], r.api); len(occ) != 0 { // never delivered: no source to fall silent
		t.Errorf("api %s", keys(occ))
	}
	v, c = r.eval(`{"name": "s", "on": "source.silent", "after": "1h"}`, r.now.Add(-30*time.Minute), nil)
	if occ, _ := r.occ(v, c.Rules[0], r.web); keys(occ) != "browser(hold),server(hold)" {
		t.Errorf("within the grace %s", keys(occ))
	}
}

// The collector's counts against min over the window, unknown domains across the instance.
func TestEvalCollector(t *testing.T) {
	r := newEvalRig(t)
	h := r.now.Unix() / 3600
	if err := r.st.AddStats(t.Context(), []store.IngestStat{{SiteID: r.web.ID, Hour: h, RateLimited: 40, Rejected: 2},
		{SiteID: r.web.ID, Hour: h - 5, Dropped: 9}, {SiteID: 0, Hour: h, UnknownDomain: 3}}); err != nil {
		t.Fatal(err)
	}
	v, c := r.eval(`{"name": "r", "on": "collector.refused", "min": 40}, {"name": "d", "on": "collector.dropped"},
		{"name": "u", "on": "collector.unknown_domain", "window": "15m"}`, time.Time{}, nil)
	if occ, _ := r.occ(v, c.Rules[0], r.web); len(occ) != 1 || *occ[0].Figure != 42 || occ[0].Unit != "events" {
		t.Errorf("refused %+v", occ)
	}
	if occ, _ := r.occ(v, c.Rules[1], r.web); len(occ) != 0 {
		t.Errorf("dropped outside the window %+v", occ)
	}
	occ, ok, err := v.Instance(t.Context(), r.now, c.Rules[2])
	if err != nil || !ok || len(occ) != 1 || *occ[0].Figure != 3 || occ[0].Unit != "requests" {
		t.Errorf("unknown domains %+v %v %v", occ, ok, err)
	}
	if _, ok, _ := v.Instance(t.Context(), r.now, c.Rules[0]); ok {
		t.Error("a site rule as the instance's")
	}
}

// Levels: absolute and against the median of the days before, all the rule sets together; too few days to compare
// is a quiet note and a hold, never an alert.
func TestEvalLevel(t *testing.T) {
	r := newEvalRig(t)
	// The site's first figure two days back: two comparison days, one too few (a quiet hour is a figure, 0).
	r.requests(r.web, 50*time.Hour, 1, "192.0.2.1", nil)
	r.requests(r.web, 30*time.Minute, 120, "192.0.2.1", nil)
	v, c := r.eval(`{"name": "f", "on": "level.requests", "window": "1h", "factor": 3},
		{"name": "abs", "on": "level.requests", "window": "1h", "min": 100},
		{"name": "max", "on": "level.requests", "window": "1h", "max": 100}`, time.Time{}, nil)
	if occ, _ := r.occ(v, c.Rules[0], r.web); keys(occ) != "(hold),no-comparison(quiet)" {
		t.Errorf("no comparison %s", keys(occ))
	}
	if occ, _ := r.occ(v, c.Rules[1], r.web); keys(occ) != "" || len(occ) != 1 || *occ[0].Figure != 120 {
		t.Errorf("min %s %+v", keys(occ), occ)
	}
	if occ, _ := r.occ(v, c.Rules[2], r.web); len(occ) != 0 {
		t.Errorf("max %+v", occ)
	}
	for d := 1; d <= 7; d++ {
		r.requests(r.web, time.Duration(d)*24*time.Hour+30*time.Minute, 30, "192.0.2.1", nil)
	}
	v, c = r.eval(`{"name": "f", "on": "level.requests", "window": "1h", "factor": 3},
		{"name": "f5", "on": "level.requests", "window": "1h", "factor": 5},
		{"name": "drop", "on": "level.requests", "window": "1h", "drop": 50},
		{"name": "floor", "on": "level.requests", "window": "1h", "factor": 3, "floor": 50},
		{"name": "week", "on": "level.requests", "window": "1h", "factor": 3, "baseline": "weekday"}`, time.Time{}, nil)
	occ, _ := r.occ(v, c.Rules[0], r.web)
	if len(occ) != 1 || occ[0].Hold || *occ[0].Figure != 120 || !strings.Contains(occ[0].Detail, "Usually 30 requests") {
		t.Errorf("factor 3 %+v", occ)
	}
	for i := 1; i <= 3; i++ {
		if occ, _ := r.occ(v, c.Rules[i], r.web); len(occ) != 0 {
			t.Errorf("%s %+v", c.Rules[i].Name, occ)
		}
	}
	// The same weekday of the 4 weeks before: one with figures, too few.
	if occ, _ := r.occ(v, c.Rules[4], r.web); keys(occ) != "(hold),no-comparison(quiet)" {
		t.Errorf("weekday %s", keys(occ))
	}
	// Visitors on an API is no figure: it holds.
	v, c = r.eval(`{"name": "v", "on": "level.visitors", "window": "1h", "min": 1}`, time.Time{}, nil)
	if occ, _ := r.occ(v, c.Rules[0], r.api); keys(occ) != "(hold)" {
		t.Errorf("visitors on an API %s", keys(occ))
	}
}

// An API's failed share and p95, the failed share only with enough calls.
func TestEvalLevelAPI(t *testing.T) {
	r := newEvalRig(t)
	r.requests(r.api, 40*24*time.Hour, 1, "192.0.2.1", nil)
	r.requests(r.api, 20*time.Minute, 30, "192.0.2.1", func(e *store.Event) { e.Status, e.DurMS = 502, 3000 })
	r.requests(r.api, 10*time.Minute, 10, "192.0.2.1", nil)
	v, c := r.eval(`{"name": "f", "on": "level.failed", "window": "1h", "min": 50, "calls": 20},
		{"name": "few", "on": "level.failed", "window": "1h", "min": 50},
		{"name": "slow", "on": "level.p95", "window": "1h", "min": 2000}`, time.Time{}, nil)
	if occ, _ := r.occ(v, c.Rules[0], r.api); len(occ) != 1 || *occ[0].Figure != 75 || occ[0].Unit != "%" {
		t.Errorf("failed %+v", occ)
	}
	if occ, _ := r.occ(v, c.Rules[1], r.api); keys(occ) != "(hold)" {
		t.Errorf("too few calls %s", keys(occ))
	}
	if occ, _ := r.occ(v, c.Rules[2], r.api); len(occ) != 1 || *occ[0].Figure < 2000 {
		t.Errorf("p95 %+v", occ)
	}
}

type budgetModel struct{ calls, budget int64 }

func (budgetModel) Configured() bool { return true }
func (budgetModel) Briefing(context.Context, insight.BriefingInput) *insight.Briefing {
	return nil
}
func (budgetModel) Readings(context.Context, []insight.ReadingInput, bool) map[string]insight.Reading {
	return nil
}
func (budgetModel) Labels(context.Context, int64, string, []string) map[string]string { return nil }
func (m budgetModel) Usage(context.Context, int64) (int64, int64)                     { return m.calls, m.budget }

func TestEvalBudget(t *testing.T) {
	r := newEvalRig(t)
	for _, tc := range []struct {
		m    insight.Model
		want int
	}{{nil, 0}, {budgetModel{10, 200}, 0}, {budgetModel{200, 200}, 1}, {budgetModel{5, 0}, 0}} {
		v, c := r.eval(`{"name": "b", "on": "llm.budget"}`, time.Time{}, tc.m)
		if occ, ok := r.occ(v, c.Rules[0], r.web); !ok || len(occ) != tc.want {
			t.Errorf("%+v: %+v", tc.m, occ)
		}
	}
}

func TestSpan(t *testing.T) {
	for d, want := range map[time.Duration]string{time.Hour: "1 h", 90 * time.Minute: "1 h 30 min",
		15 * time.Minute: "15 min", 0: "0 min"} {
		if got := span(d); got != want {
			t.Errorf("span(%v) = %q", d, got)
		}
	}
	if m := median([]float64{3, 1, 2, 10}); m != 2.5 {
		t.Errorf("median %v", fmt.Sprint(m))
	}
}
