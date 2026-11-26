package dispatch

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"slices"
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

// script.new: the watch's new script host, provisional while its day is open and then closed, one key throughout;
// the addresses as its figure, the script hosts card as its target. No other rule reads it, and a website's new POST
// target is no new endpoint: those are an API's.
func TestEvalScriptHosts(t *testing.T) {
	r := newEvalRig(t)
	engage := func(ago time.Duration, addr, hosts string) {
		r.requests(r.web, ago, 1, addr, func(e *store.Event) {
			e.Source, e.Kind, e.Visitor, e.Props = "browser", "engagement", "v-"+addr, `{"p":{"scripts":[`+hosts+`]}}`
		})
	}
	engage(240*time.Hour, "192.0.2.1", `"cdn.example.org"`)
	// Caught up on its first day: a backfill finds nothing.
	if err := watch.Pass(t.Context(), r.st, r.now.Add(-236*time.Hour)); err != nil {
		t.Fatal(err)
	}
	r.requests(r.web, 3*time.Hour, 1, "192.0.2.11", func(e *store.Event) {
		e.Template, e.Method, e.Path, e.Unrouted = "POST /xmlrpc.php", "POST", "/xmlrpc.php", true
	})
	for i, addr := range []string{"192.0.2.10", "198.51.100.11", "203.0.113.12"} {
		engage(2*time.Hour+time.Duration(i)*time.Minute, addr, `"cdn.example.org","pay.example.net"`)
	}
	if err := watch.Pass(t.Context(), r.st, r.now); err != nil {
		t.Fatal(err)
	}
	rules := `{"name": "s", "on": "script.new"}, {"name": "e", "on": "endpoint.new", "min": 0},
		{"name": "p", "on": "probe.new", "min": 0}, {"name": "r", "on": "referrer.new", "min": 0},
		{"name": "x", "on": "error.new", "min": 0}`
	v, c := r.eval(rules, time.Time{}, nil)
	occ, ok := r.occ(v, c.Rules[0], r.web)
	if !ok || keys(occ) != "pay.example.net" || *occ[0].Figure != 3 || occ[0].Unit != "addresses" ||
		occ[0].Text != "New script host: pay.example.net" || !strings.Contains(occ[0].Detail, "Provisional") ||
		occ[0].Target.Panel != "scripts" || occ[0].Target.Filter["key"] != "pay.example.net" {
		t.Fatalf("provisional: %v %+v", ok, occ)
	}
	for _, rule := range c.Rules[1:] {
		occ, ok := r.occ(v, rule, r.web)
		if !ok || len(occ) != 0 {
			t.Errorf("%s: %v %s", rule.Name, ok, keys(occ))
		}
	}
	// The day closed: the same key, no longer provisional.
	r.now = r.now.Add(15 * time.Hour)
	if err := watch.Pass(t.Context(), r.st, r.now); err != nil {
		t.Fatal(err)
	}
	v, c = r.eval(rules, time.Time{}, nil)
	if occ, _ := r.occ(v, c.Rules[0], r.web); keys(occ) != "pay.example.net" || strings.Contains(occ[0].Detail,
		"Provisional") {
		t.Errorf("closed: %+v", occ)
	}
}

// scriptSteps is the alert engine over the evaluator, a script.new rule, the watch's pass and the engine's step
// each hour as the server runs them, from day d.
type scriptSteps struct {
	*evalRig
	d int64
	e *Engine
}

const dayMS = 86_400_000

func newScriptSteps(t *testing.T) *scriptSteps {
	t.Helper()
	r := newEvalRig(t)
	s := &scriptSteps{evalRig: r, d: r.now.UnixMilli() / dayMS}
	c := alert.Parse([]byte(`{`+ops+`, "rules": [{"name": "s", "on": "script.new"}]}`),
		alert.Known{Domains: []string{"example.com", "api.example.com"}, Products: []string{"Lens"}})
	if len(c.Faults) > 0 {
		t.Fatal(c.Faults)
	}
	rec := &Recorder{}
	d := &dash.Server{DB: r.st, Sites: r.reg, RetentionMonths: 25}
	s.e = New(Options{Store: r.st, Sites: r.reg, Config: c, Senders: rec.Senders(c), Board: alert.NewBoard(nil),
		Source: NewEval(d, c.Rules, s.at(-10, 0), nil), PublicFQDN: "trakk.example.com",
		Log: slog.New(slog.DiscardHandler)})
	// A known host ten days back, walked that day: no backfill, the learning window long over.
	s.engage(s.at(-10, 9), "192.0.2.1", `"cdn.example.org"`)
	r.now = s.at(-10, 12)
	if err := watch.Pass(t.Context(), r.st, r.now); err != nil {
		t.Fatal(err)
	}
	r.now = s.at(0, 0).Add(5 * time.Minute)
	s.step() // the rule's baseline: nothing yet
	return s
}

// at is hour h of day d+day.
func (s *scriptSteps) at(day int64, h int) time.Time {
	return time.UnixMilli((s.d+day)*dayMS + int64(h)*3_600_000).UTC()
}

func (s *scriptSteps) engage(at time.Time, addr, hosts string) {
	s.t.Helper()
	s.requests(s.web, s.now.Sub(at), 1, addr, func(e *store.Event) {
		e.Source, e.Kind, e.Visitor, e.Props = "browser", "engagement", "v-"+addr, `{"p":{"scripts":[`+hosts+`]}}`
	})
}

func (s *scriptSteps) step() {
	s.t.Helper()
	if err := watch.Pass(s.t.Context(), s.st, s.now); err != nil {
		s.t.Fatal(err)
	}
	if err := s.e.Step(s.t.Context(), s.now); err != nil {
		s.t.Fatal(err)
	}
}

// until steps hourly up to end.
func (s *scriptSteps) until(end time.Time) {
	s.t.Helper()
	for s.now.Add(time.Hour).Before(end.Add(time.Second)) {
		s.now = s.now.Add(time.Hour)
		s.step()
	}
}

// scriptAlert is a script.new row as the test reads it.
type scriptAlert struct {
	id      int64
	key     string
	state   string
	told    string
	left    bool
	firstAt int64
	detail  string
}

func (s *scriptSteps) alerts() map[string]scriptAlert {
	s.t.Helper()
	rows, err := s.st.DB.QueryContext(s.t.Context(), `SELECT id, key, state, told, left_at IS NOT NULL, first_at, detail
		FROM alert WHERE rule = 's'`)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]scriptAlert{}
	for rows.Next() {
		var a scriptAlert
		if err := rows.Scan(&a.id, &a.key, &a.state, &a.told, &a.left, &a.firstAt, &a.detail); err != nil {
			s.t.Fatal(err)
		}
		if _, ok := out[a.key]; ok {
			s.t.Errorf("%s alerted twice", a.key)
		}
		out[a.key] = a
	}
	return out
}

// A third network late in the day finds a host first seen at its start: the alert comes with the finding, though the
// 24 hours read then no longer hold its first sight. Provisional, then closed, one alert throughout; it leaves a day
// after the job made it and never comes back.
func TestScriptNewLateNetwork(t *testing.T) {
	s := newScriptSteps(t)
	hosts := `"cdn.example.org","late.example.net"`
	s.engage(s.at(0, 0).Add(10*time.Minute), "192.0.2.10", hosts)
	s.engage(s.at(0, 23).Add(30*time.Minute), "198.51.100.11", hosts)
	s.engage(s.at(0, 23).Add(31*time.Minute), "203.0.113.12", hosts)
	s.until(s.at(0, 23).Add(5 * time.Minute))
	if as := s.alerts(); len(as) != 0 {
		t.Fatalf("before the third network %v", as)
	}
	s.until(s.at(1, 0).Add(5 * time.Minute))
	first := s.alerts()["late.example.net"]
	if len(s.alerts()) != 1 || first.state != store.AlertOnce || first.told != "open" ||
		!strings.Contains(first.detail, "Provisional") || !strings.Contains(first.detail, "First seen "+
		s.at(0, 0).Format("2006-01-02")+" 00:10 UTC") {
		t.Fatalf("provisional %+v", s.alerts())
	}
	// Its day closes at 02:00: the same row in the set every pass, never left (a re-entry within the cooldown would
	// look the same at the end), its finding no longer provisional.
	for range 3 {
		s.until(s.now.Add(time.Hour))
		if a := s.alerts()["late.example.net"]; a.id != first.id || a.left || a.told != "open" {
			t.Fatalf("%s: %+v", s.now, a)
		}
	}
	if a := s.alerts()["late.example.net"]; strings.Contains(a.detail, "Provisional") {
		t.Fatalf("closed %+v", a)
	}
	if st, _, _ := watch.SiteState(t.Context(), s.st.DB, s.web.ID); st.DoneDay != s.d {
		t.Fatalf("day not closed: %+v", st)
	}
	s.until(s.at(3, 6).Add(5 * time.Minute))
	if as := s.alerts(); len(as) != 1 || !as["late.example.net"].left || as["late.example.net"].id != first.id {
		t.Errorf("a day later %+v", as)
	}
}

// A day finds ten hosts at most: the eleventh, first seen early that day, is found the next and alerted then; each
// host once, provisional or closed.
func TestScriptNewPastTheBound(t *testing.T) {
	s := newScriptSteps(t)
	var names []string
	for i := range 11 {
		names = append(names, fmt.Sprintf(`"p%02d.example.net"`, i))
	}
	for i, addr := range []string{"192.0.2.10", "198.51.100.11", "203.0.113.12"} {
		s.engage(s.at(0, 0).Add(time.Duration(20+i)*time.Minute), addr, strings.Join(names, ","))
	}
	s.until(s.at(0, 23).Add(5 * time.Minute))
	if as := s.alerts(); len(as) != 10 {
		t.Fatalf("day 0: %d alerts", len(as))
	}
	if _, ok := s.alerts()["p10.example.net"]; ok {
		t.Fatal("the eleventh found on its own day")
	}
	s.until(s.at(2, 6).Add(5 * time.Minute))
	as := s.alerts()
	last, ok := as["p10.example.net"]
	if len(as) != 11 || !ok || last.told != "open" || last.firstAt < s.at(1, 0).UnixMilli() {
		t.Fatalf("the eleventh %v %+v", ok, last)
	}
	for k, a := range as {
		if a.state != store.AlertOnce || a.told != "open" {
			t.Errorf("%s %+v", k, a)
		}
	}
	if h, err := watch.Hosts(t.Context(), s.st.DB, s.web.ID, 0); err != nil || len(h) != 12 {
		t.Fatalf("hosts %v %v", h, err)
	}
	fs, err := watch.Findings(t.Context(), s.st.DB, s.web.ID, 0)
	if err != nil || len(fs) != 11 || slices.ContainsFunc(fs, func(f watch.Finding) bool { return f.Provisional }) {
		t.Errorf("findings %d %v", len(fs), err)
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

// An error first seen from an extension and then thrown by the site's own code is looked at again once it grew.
func TestEvalErrorLookedAgain(t *testing.T) {
	r := newEvalRig(t)
	throw := func(src string, n int) {
		r.requests(r.web, 2*time.Hour, n, "192.0.2.9", func(e *store.Event) {
			e.Source, e.Kind, e.Name, e.UA, e.Visitor = "browser", "js_error", "TypeError: z", "Mozilla/5.0", "v1"
			e.Props = `{"p":{"src":"` + src + `","line":1,"col":1}}`
		})
	}
	throw("chrome-extension://abc/x.js", 1)
	v, c := r.eval(`{"name": "def", "on": "error.new"}`, time.Time{}, nil)
	if occ, _ := r.occ(v, c.Rules[0], r.web); len(occ) != 0 {
		t.Fatalf("an extension's error: %s", keys(occ))
	}
	throw("https://example.com/app.js", 50)
	r.now = r.now.Add(time.Minute)
	if occ, _ := r.occ(v, c.Rules[0], r.web); keys(occ) != "TypeError: z" {
		t.Fatalf("the site's own, later: %s", keys(occ))
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

// people inserts n people who saw a page on the site, ago before now, the first hit of them with a script error msg.
func (r *evalRig) people(ago time.Duration, n, hit int, msg string) {
	r.t.Helper()
	r.requests(r.web, ago, n+hit, "192.0.2.9", func(e *store.Event) {
		i := int(e.TS-r.now.Add(-ago).UnixMilli()) / 1000
		e.Source, e.Kind, e.UA, e.Cls, e.Visitor = "browser", "page_view", "Mozilla/5.0", store.ClsHuman, fmt.Sprint("v", i)
		if i >= n {
			e.Kind, e.Name, e.Visitor = "js_error", msg, fmt.Sprint("v", i-n)
		}
	})
}

// waits: the one key is held and the rule's baseline waits for it.
func waits(occ []alert.Occurrence) bool { return len(occ) > 0 && occ[0].Hold && occ[0].Wait }

// The script errors' level: min, and the usual by factor; too few days to compare, too few affected or nobody there
// holds; the detail ends with the most frequent message.
func TestEvalErrorsLevel(t *testing.T) {
	const on = `"on": "level.errors", "window": "1h"`
	r := newEvalRig(t)
	r.people(50*time.Hour, 1, 0, "") // raw begins: two days to compare
	r.people(30*time.Minute, 100, 12, "TypeError: x")
	v, c := r.eval(`{"name": "min", `+on+`, "min": 10}, {"name": "under", `+on+`, "min": 12.5},
		{"name": "few", `+on+`, "min": 10, "visitors": 13}, {"name": "quiet", `+on+`, "min": 12.5, "visitors": 13},
		{"name": "factor", `+on+`, "min": 5, "factor": 3},
		{"name": "other", `+on+`, "min": 5, "origins": ["extension"]},
		{"name": "low", `+on+`, "min": 12.5, "factor": 3}`, time.Time{}, nil)
	occ, ok := r.occ(v, c.Rules[0], r.web)
	if !ok || keys(occ) != "" || len(occ) != 1 || *occ[0].Figure != 12 || occ[0].Unit != "%" || occ[0].Hold ||
		occ[0].Text != "Visitors with a script error over the last 1 h: 12 %" ||
		occ[0].Detail != "12 of 100 visitors; most frequent (12 visitors): TypeError: x" ||
		occ[0].Target.Panel != "errors" || len(occ[0].Target.Filter) != 0 {
		t.Fatalf("min %+v", occ)
	}
	if occ, ok := r.occ(v, c.Rules[1], r.web); !ok || len(occ) != 0 {
		t.Errorf("under min %+v", occ)
	}
	if occ, _ := r.occ(v, c.Rules[2], r.web); keys(occ) != "(hold)" || waits(occ) {
		t.Errorf("too few affected %+v", occ)
	}
	// Under min it clears, however few are affected.
	if occ, ok := r.occ(v, c.Rules[3], r.web); !ok || len(occ) != 0 {
		t.Errorf("under min, too few affected %+v", occ)
	}
	if occ, _ := r.occ(v, c.Rules[4], r.web); keys(occ) != "(hold),no-comparison(quiet)" || !waits(occ) {
		t.Errorf("too few days %+v", occ)
	}
	if occ, ok := r.occ(v, c.Rules[5], r.web); !ok || len(occ) != 0 {
		t.Errorf("another origin %+v", occ)
	}
	// Under min it is judged without days to compare with.
	if occ, ok := r.occ(v, c.Rules[6], r.web); !ok || len(occ) != 0 {
		t.Errorf("under min, too few days %+v", occ)
	}
	// At the bound: 29 of 100 is 29 %, three times a usual of 7 of 100 is 21 %.
	r = newEvalRig(t)
	r.people(9*24*time.Hour, 1, 0, "")
	for d := 1; d <= 7; d++ {
		r.people(time.Duration(d)*24*time.Hour+30*time.Minute, 100, 7, "TypeError: old")
	}
	r.people(30*time.Minute, 100, 29, "TypeError: x")
	v, c = r.eval(`{"name": "min", `+on+`, "min": 29}`, time.Time{}, nil)
	if occ, _ := r.occ(v, c.Rules[0], r.web); len(occ) != 1 || occ[0].Hold {
		t.Errorf("at min %+v", occ)
	}
	r = newEvalRig(t)
	r.people(9*24*time.Hour, 1, 0, "")
	for d := 1; d <= 7; d++ {
		r.people(time.Duration(d)*24*time.Hour+30*time.Minute, 100, 7, "TypeError: old")
	}
	r.people(30*time.Minute, 100, 21, "TypeError: x")
	v, c = r.eval(`{"name": "factor", `+on+`, "min": 5, "factor": 3}`, time.Time{}, nil)
	if occ, _ := r.occ(v, c.Rules[0], r.web); len(occ) != 1 || occ[0].Hold {
		t.Errorf("at the factor %+v", occ)
	}

	// Two of two at night neither open nor clear; nobody there is no figure.
	r = newEvalRig(t)
	r.people(26*time.Hour, 1, 0, "")
	v, c = r.eval(`{"name": "def", `+on+`, "min": 10}, {"name": "one", `+on+`, "min": 10, "visitors": 1},
		{"name": "fast", "on": "level.errors", "window": "1m", "min": 10}`, time.Time{}, nil)
	r.people(30*time.Minute, 2, 2, "TypeError: x")
	if occ, _ := r.occ(v, c.Rules[0], r.web); keys(occ) != "(hold)" || waits(occ) {
		t.Errorf("two of two %+v", occ)
	}
	if occ, _ := r.occ(v, c.Rules[1], r.web); len(occ) != 1 || occ[0].Hold {
		t.Errorf("visitors 1 %+v", occ)
	}
	if occ, _ := r.occ(v, c.Rules[2], r.web); keys(occ) != "(hold)" || !waits(occ) {
		t.Errorf("nobody there %+v", occ)
	}

	// Against the usual; a message of another line, invisible characters and any length stays one clipped line.
	for _, usual := range []int{1, 0} {
		r := newEvalRig(t)
		r.people(9*24*time.Hour, 1, 0, "")
		for d := 1; d <= 7; d++ {
			r.people(time.Duration(d)*24*time.Hour+30*time.Minute, 50, usual, "TypeError: old")
		}
		hostile := "Error:\n" + string(rune(0x202e)) + "x; https://evil.example.com " + strings.Repeat("y", 400)
		r.people(30*time.Minute, 100, 12, hostile)
		v, c := r.eval(`{"name": "f3", `+on+`, "min": 5, "factor": 3}, {"name": "f7", `+on+`, "min": 5, "factor": 7},
			{"name": "min", `+on+`, "min": 12.5, "factor": 3}`, time.Time{}, nil)
		occ, _ := r.occ(v, c.Rules[0], r.web)
		want := fmt.Sprintf("Usually %d %% (median of 7 days); 12 of 100 visitors; most frequent (12 visitors): "+
			"Error: x; https://evil.example.com yyy", 2*usual)
		if len(occ) != 1 || occ[0].Hold || *occ[0].Figure != 12 || !strings.HasPrefix(occ[0].Detail, want) ||
			!strings.HasSuffix(occ[0].Detail, "y…") || len([]rune(occ[0].Detail)) != 300 {
			t.Errorf("usual %d %%, factor 3: %+v", 2*usual, occ)
		}
		// 7 times the usual 2 % is above it; a usual of 0 % leaves it to min.
		if occ, _ := r.occ(v, c.Rules[1], r.web); len(occ) != 1-usual {
			t.Errorf("usual %d %%, factor 7: %+v", 2*usual, occ)
		}
		if occ, _ := r.occ(v, c.Rules[2], r.web); len(occ) != 0 {
			t.Errorf("usual %d %%, under min: %+v", 2*usual, occ)
		}
	}

	r = newEvalRig(t)
	r.people(26*time.Hour, 1, 0, "")
	r.people(30*time.Minute, 1, 1, "TypeError: x")
	v, c = r.eval(`{"name": "one", `+on+`, "min": 10, "visitors": 1}`, time.Time{}, nil)
	if occ, _ := r.occ(v, c.Rules[0], r.web); len(occ) != 1 ||
		occ[0].Detail != "1 of 1 visitor; most frequent (1 visitor): TypeError: x" {
		t.Errorf("one visitor %+v", occ)
	}
}

type budgetModel struct {
	calls, budget int64
	spent         bool
}

func (budgetModel) Configured() bool { return true }
func (budgetModel) Briefing(context.Context, insight.BriefingInput) *insight.Briefing {
	return nil
}
func (budgetModel) Readings(context.Context, []insight.ReadingInput, bool) map[string]insight.Reading {
	return nil
}
func (budgetModel) Labels(context.Context, int64, string, []string) map[string]string { return nil }
func (m budgetModel) Usage(context.Context, int64) (int64, int64, bool) {
	return m.calls, m.budget, m.spent
}

// The model says when its budget is spent: tokens run out with calls to spare.
func TestEvalBudget(t *testing.T) {
	r := newEvalRig(t)
	for _, tc := range []struct {
		m    insight.Model
		want int
	}{{nil, 0}, {budgetModel{10, 200, false}, 0}, {budgetModel{200, 200, true}, 1},
		{budgetModel{150, 200, true}, 1}} {
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

// One visitor is not "1 visitors": the line goes out in a message.
func TestScriptErrorTextCounts(t *testing.T) {
	share := 4.5
	for _, c := range []struct {
		e    dash.ScriptError
		want string
	}{
		{dash.ScriptError{Visitors: 1, Viewers: 22, Share: &share}, "1 visitor, 4.5 % of 22 who saw a page"},
		{dash.ScriptError{Visitors: 2, Viewers: 22, Share: &share}, "2 visitors, 4.5 % of 22 who saw a page"},
		{dash.ScriptError{Visitors: 1}, "1 visitor"},
		{dash.ScriptError{Visitors: 3}, "3 visitors"},
	} {
		if got := scriptErrorText(c.e); got != c.want {
			t.Errorf("%+v: %q, want %q", c.e, got, c.want)
		}
	}
}
