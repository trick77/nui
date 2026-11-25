package dash

import (
	"context"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/plancheck"
	"github.com/trick77/trakk/backend/internal/store"
)

// GET /api/dash/alerts: the wire contract, newest first, a page at a time, baseline and pending rows left out.
func TestAlertsEndpoint(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	feed, _ := r.s.Sites.Lookup(feedSite)
	ctx := t.Context()
	at := r.end.UnixMilli()
	add := func(a store.Alert) int64 {
		t.Helper()
		a.FirstAt, a.LastAt = at, at+1000
		if a.Severity == "" {
			a.Severity = "normal"
		}
		if err := r.s.DB.InsertAlert(ctx, &a); err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	fig := 42.0
	once := add(store.Alert{SiteID: feed.ID, Rule: "new-errors", Kind: "error.new", Key: "TypeError: x", State: store.AlertOnce,
		Severity: "high", Figure: &fig, Unit: "times", Text: "New error: TypeError: x", Panel: "errors",
		Channels: []string{"ops", "mail"}, Delivered: []string{"ops"}, Failed: []string{"mail"}, NotifiedAt: &at})
	add(store.Alert{SiteID: feed.ID, Rule: "new-errors", Kind: "error.new", Key: "old", State: store.AlertBaseline, Text: "b"})
	cleared := add(store.Alert{SiteID: 0, Rule: "jobs", Kind: "job.failed", Key: "agg", State: store.AlertCleared,
		Text: "The aggregate build failed", Detail: "disk I/O error", LeftAt: new(at + 5000), Panel: "domain",
		Filter: map[string]string{}, Channels: []string{"ops"}, Due: "clear", DueAt: &at})
	add(store.Alert{SiteID: feed.ID, Rule: "silent", Kind: "source.silent", Key: "server", State: store.AlertPending, Text: "p"})
	none := add(store.Alert{SiteID: feed.ID, Rule: "trend", Kind: "trend.visitors", Key: "up", State: store.AlertOnce, Text: "t"})

	code, body := r.get(t, "alerts", map[string]string{})
	if code != 200 {
		t.Fatal(code, body)
	}
	noNullLists(t, "alerts", body)
	rows := body["alerts"].([]any)
	if len(rows) != 3 || body["next"] != nil {
		t.Fatalf("alerts %v", body)
	}
	byID := map[float64]map[string]any{}
	for _, x := range rows {
		m := x.(map[string]any)
		byID[m["id"].(float64)] = m
	}
	o := byID[float64(once)]
	if o["site"] != feedSite || o["rule"] != "new-errors" || o["kind"] != "error.new" || o["key"] != "TypeError: x" ||
		o["severity"] != "high" || o["state"] != "once" || o["figure"] != 42.0 || o["unit"] != "times" ||
		o["detail"] != "" || o["cleared_at"] != nil || o["first_at"] != iso(time.UnixMilli(at)) {
		t.Errorf("once %v", o)
	}
	d := o["delivery"].(map[string]any)
	chans := d["channels"].([]any)
	if d["state"] != "partial" || d["at"] == nil || len(chans) != 2 || chans[0].(map[string]any)["ok"] != true ||
		chans[1].(map[string]any)["name"] != "mail" {
		t.Errorf("delivery %v", d)
	}
	if tg := o["target"].(map[string]any); tg["panel"] != "errors" || tg["filter"] == nil {
		t.Errorf("target %v", tg)
	}
	c := byID[float64(cleared)]
	if c["site"] != "" || c["state"] != "cleared" || c["cleared_at"] != iso(time.UnixMilli(at+5000)) ||
		c["delivery"].(map[string]any)["state"] != "pending" || c["detail"] != "disk I/O error" {
		t.Errorf("cleared %v", c)
	}
	n := byID[float64(none)]
	if n["delivery"].(map[string]any)["state"] != "none" || n["target"] != nil || n["figure"] != nil {
		t.Errorf("none %v", n)
	}
	// One site's, a page at a time.
	_, body = r.get(t, "alerts", map[string]string{"site": feedSite, "limit": "1"})
	page := body["alerts"].([]any)
	if len(page) != 1 || page[0].(map[string]any)["id"] != float64(none) || body["next"] == nil {
		t.Fatalf("first page %v", body)
	}
	_, body = r.get(t, "alerts", map[string]string{"site": feedSite, "limit": "1", "before": body["next"].(string)})
	if page := body["alerts"].([]any); len(page) != 1 || page[0].(map[string]any)["id"] != float64(once) {
		t.Fatalf("second page %v", body)
	}
	for _, bad := range []map[string]string{{"site": "nope.example.com"}, {"limit": "0"}, {"limit": "201"},
		{"limit": "x"}, {"before": "x"}, {"before": "-1"}} {
		if code, _ := r.get(t, "alerts", bad); code != 400 {
			t.Errorf("%v: %d", bad, code)
		}
	}
}

type fakeNotify struct{ st alert.Status }

func (f fakeNotify) Status(context.Context) (alert.Status, error) { return f.st, nil }

// The footer's notify block: never a URL, an empty one without notifications.
func TestDomainNotify(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	_, body := r.get(t, "domain", p(feedSite))
	noNullLists(t, "domain", body)
	nb := body["notify"].(map[string]any)
	if nb["configured"] != false || len(nb["channels"].([]any)) != 0 || len(nb["faults"].([]any)) != 0 {
		t.Errorf("no notifications %v", nb)
	}
	at := r.end
	r.s.Notify = fakeNotify{alert.Status{Configured: true, Enabled: true, Faults: []string{"rule 1: x"}, Active: 4, Off: 1,
		Channels: []alert.ChannelStatus{{Name: "ops", Service: "ntfy", MinSeverity: alert.High, LastOK: at, ErrorAt: at,
			Error: `ntfy: Post "https://ntfy.example.com/t?auth=hunter2": refused`}, {Name: "mail", Service: "smtp",
			MinSeverity: alert.Low}}}}
	_, body = r.get(t, "domain", p(feedSite))
	nb = body["notify"].(map[string]any)
	chans := nb["channels"].([]any)
	ops, mail := chans[0].(map[string]any), chans[1].(map[string]any)
	if nb["enabled"] != true || nb["rules"].(map[string]any)["active"] != 4.0 || nb["faults"].([]any)[0] != "rule 1: x" ||
		ops["min_severity"] != "high" || ops["last_ok"] != iso(at) || mail["last_ok"] != nil || mail["last_error"] != nil {
		t.Errorf("notify %v", nb)
	}
	if e := ops["last_error"].(map[string]any); strings.Contains(e["text"].(string), "hunter2") || e["at"] != iso(at) {
		t.Errorf("last error %v", e)
	}
}

// A new script error as its alert says it: origins, place, type and share over the closed hours of the last day,
// never its stack; each statement within a day, on an index.
func TestScriptErrorOf(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	site, _ := r.s.Sites.Lookup(feedSite)
	at := r.end.Add(-3 * time.Hour).UnixMilli()
	var evs []store.Event
	for i, v := range []string{"v1", "v2", "v1", "v3", "v4"} {
		e := store.Event{SiteID: site.ID, TS: at + int64(i)*1000, Source: "browser", Kind: "page_view", Visitor: v,
			IP: "192.0.2.70", IPKey: "192.0.2.70", UA: "Mozilla/5.0", Template: "/", Path: "/"}
		if i < 3 {
			e.Kind, e.Name = "js_error", "TypeError: a"
			e.Props = `{"p":{"src":"https://` + feedSite + `/assets/a.js","line":3,"col":9,"type":"TypeError",
				"stack":["at f (a.js:3:9)"]}}`
		}
		evs = append(evs, e)
	}
	if err := r.s.DB.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := map[string][]any{}
	traceSQL = func(q string, args []any) {
		mu.Lock()
		defer mu.Unlock()
		seen[q] = append([]any(nil), args...)
	}
	t.Cleanup(func() { traceSQL = nil })
	now := r.end.Add(time.Minute)
	e, err := r.s.ScriptErrorOf(t.Context(), site, "TypeError: a", now)
	if err != nil || !slices.Equal(e.Origins, []string{"own"}) || e.Place != "own /assets/a.js:3:9" ||
		e.Type != "TypeError" || e.Visitors != 2 || e.Viewers != 2 || e.Share == nil || *e.Share != 100 {
		t.Errorf("script error %+v %v", e, err)
	}
	if e, err := r.s.ScriptErrorOf(t.Context(), site, "no such error", now); err != nil || len(e.Origins) != 0 ||
		e.Share != nil {
		t.Errorf("none: %+v %v", e, err)
	}
	traceSQL = nil
	if len(seen) < 2 {
		t.Fatalf("%d statements", len(seen))
	}
	for q, args := range seen {
		plan, err := plancheck.Explain(t.Context(), r.s.DB.DB, q, args...)
		if err != nil {
			t.Fatal(err)
		}
		if p := plancheck.Problem(q, plan); p != "" {
			t.Errorf("%s: %s", p, strings.Join(strings.Fields(q), " "))
		}
		if lo, hi := args[1].(int64), args[2].(int64); hi-lo > dayMS {
			t.Errorf("a statement over %d ms: %s", hi-lo, strings.Join(strings.Fields(q), " "))
		}
	}
}

// The script errors' level: people decided by visitor (any row of one is the evidence, a batch's other rows still
// pending), everyone there in the denominator, the origins a rule counts, NaN without people or raw; a statement a
// window, at most a day, on the site's time index.
func TestErrorLevels(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	ctx := t.Context()
	site, _ := r.s.Sites.Lookup(feedSite)
	blog, _ := r.s.Sites.Lookup(plainSite)
	now := r.end.Add(time.Minute)
	const own, ext = `{"p":{"src":"https://lens.example.com/a.js"}}`, `{"p":{"src":"chrome-extension://abc/c.js"}}`
	type row struct {
		visitor, kind string
		cls           int
		name, props   string
	}
	insert := func(site int64, at time.Time, rows ...row) {
		t.Helper()
		var evs []store.Event
		for i, x := range rows {
			if x.props == "" {
				x.props = "{}"
			}
			evs = append(evs, store.Event{SiteID: site, TS: at.UnixMilli() + int64(i), Source: "browser", Kind: x.kind,
				Name: x.name, Visitor: x.visitor, IP: "192.0.2.70", IPKey: "192.0.2.70", UA: "Mozilla/5.0", Template: "/",
				Path: "/", Cls: x.cls, Props: x.props})
		}
		if err := r.s.DB.InsertEvents(ctx, evs); err != nil {
			t.Fatal(err)
		}
	}
	const a, b = "TypeError: a", "TypeError: b"
	insert(site.ID, now.Add(-30*time.Minute),
		// A session's first batch before the classifier's pass: its first event alone is human.
		row{"first", "session_start", 1, "", ""}, row{"first", "page_view", 0, "", ""}, row{"first", "js_error", 0, a, own},
		// Nothing says a person yet: nobody, nor do its errors make b the most frequent.
		row{"pending", "session_start", 0, "", ""}, row{"pending", "page_view", 0, "", ""},
		row{"pending", "js_error", 0, b, own},
		// The evidence after the error it counts.
		row{"late", "page_view", 0, "", ""}, row{"late", "js_error", 0, b, own}, row{"late", "engagement", 1, "", ""},
		row{"late", "js_error", 0, b, `{"p":{"src":"https://lens.example.com/b.js"}}`},
		// A tab open from before, no page view and no error: there.
		row{"open", "engagement", 1, "", ""},
		// A row classed automated outweighs a person's: the pass judged it, its next batch is human again until the next.
		row{"mixed", "session_start", 1, "", ""}, row{"mixed", "page_view", 4, "", ""}, row{"mixed", "js_error", 2, b, own},
		row{"again", "page_view", 4, "", ""}, row{"again", "js_error", 1, b, own},
		// An error without a message is one, but names nothing.
		row{"mute1", "js_error", 1, "", own}, row{"mute2", "js_error", 1, "", own},
		row{"bot", "page_view", 2, "", ""}, row{"bot", "js_error", 2, b, own},
		row{"monitor", "page_view", 3, "", ""}, row{"unknown", "page_view", 4, "", ""}, row{"unknown", "js_error", 4, b, own},
		row{"api", "page_view", 5, "", ""},
		// A tab left open: an error, no page view.
		row{"tab", "js_error", 1, a, own},
		// Another JSON type for the file reads as none: an unknown origin, no failure.
		row{"json", "page_view", 1, "", ""}, row{"json", "js_error", 1, b, `{"p":{"src":{"a":1}}}`},
		// An extension's error: there, not affected.
		row{"ext", "page_view", 1, "", ""}, row{"ext", "js_error", 1, "TypeError: ext", ext},
		row{"seen", "page_view", 1, "", ""}, row{"seen", "page_view", 1, "", ""},
		row{"", "page_view", 1, "", ""}, row{"", "js_error", 1, b, own})
	// The days before: one person, unaffected; nobody; a window that begins before raw does.
	insert(site.ID, now.AddDate(0, 0, -1).Add(-30*time.Minute), row{"p", "page_view", 1, "", ""})
	insert(site.ID, now.AddDate(0, 0, -3).Add(-10*time.Minute), row{"p", "page_view", 1, "", ""},
		row{"p", "js_error", 1, a, own})

	var mu sync.Mutex
	type stmt struct {
		q    string
		args []any
	}
	var seen []stmt
	traceSQL = func(q string, args []any) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, stmt{q, append([]any(nil), args...)})
	}
	t.Cleanup(func() { traceSQL = nil })
	cur, prev, err := r.s.ErrorLevels(ctx, site, alert.DefaultOrigins, time.Hour, now, []int{1, 2, 3}, 50)
	if err != nil {
		t.Fatal(err)
	}
	// first, late, tab, json, mute1 and mute2 of them and open, ext and seen; a and b hit two each, a by its name.
	if cur.Affected != 6 || cur.Visitors != 9 || cur.Value != 600.0/9 || cur.Message != a || cur.MessageVisitors != 2 {
		t.Errorf("level %+v", cur)
	}
	// Under the least share nothing is compared.
	if _, prev, err := r.s.ErrorLevels(ctx, site, alert.DefaultOrigins, time.Hour, now, []int{1, 2}, 67); err != nil ||
		len(prev) != 0 {
		t.Errorf("under the least %+v %v", prev, err)
	}
	if len(prev) != 3 || prev[0].Value != 0 || !math.IsNaN(prev[1].Value) || !math.IsNaN(prev[2].Value) {
		t.Errorf("days before %+v", prev)
	}
	if e, _, err := r.s.ErrorLevels(ctx, site, []string{alert.OriginExtension}, time.Hour, now, nil, 0); err != nil ||
		e.Affected != 1 || e.Visitors != 9 || e.Message != "TypeError: ext" || e.MessageVisitors != 1 {
		t.Errorf("an extension's: %+v %v", e, err)
	}
	if e, prev, err := r.s.ErrorLevels(ctx, blog, alert.DefaultOrigins, time.Hour, now, []int{1}, 0); err != nil ||
		!math.IsNaN(e.Value) || len(prev) != 0 {
		t.Errorf("a site without events: %+v %+v %v", e, prev, err)
	}
	// Over the end of summer time a day by the date is 25 hours.
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatal(err)
	}
	insert(blog.ID, time.Date(2026, 10, 20, 12, 0, 0, 0, zurich), row{"p", "page_view", 1, "", ""})
	insert(blog.ID, time.Date(2026, 10, 26, 11, 0, 0, 0, zurich), row{"p", "page_view", 1, "", ""})
	if _, _, err := r.s.ErrorLevels(ctx, blog, alert.DefaultOrigins, 24*time.Hour,
		time.Date(2026, 10, 26, 12, 0, 0, 0, zurich), []int{1}, 0); err != nil {
		t.Fatal(err)
	}
	traceSQL = nil
	reads := 0
	for _, x := range seen {
		plan, err := plancheck.Explain(ctx, r.s.DB.DB, x.q, x.args...)
		if err != nil {
			t.Fatal(err)
		}
		if p := plancheck.Problem(x.q, plan); p != "" {
			t.Errorf("%s: %s", p, strings.Join(strings.Fields(x.q), " "))
		}
		if !strings.Contains(x.q, "js_error") {
			continue
		}
		reads++
		if lo, hi := x.args[1].(int64), x.args[2].(int64); hi-lo > dayMS {
			t.Errorf("a statement over %d ms: %s", hi-lo, strings.Join(strings.Fields(x.q), " "))
		}
		if len(plan) == 0 || plan[0] != "SEARCH event USING INDEX event_site_ts (site_id=? AND ts>? AND ts<?)" {
			t.Errorf("plan %q", plan)
		}
	}
	// The window and the two days raw covers, the one under the least, the extension's, summer time's two; none for
	// what raw does not cover.
	if reads != 7 {
		t.Errorf("%d statements, want 7", reads)
	}
}

// The reads the alert rules make: Noteworthy's own facts at the default thresholds, more at looser ones, the levels
// with their comparison, the collector's counts and the sources' last delivery. Every statement on an index.
func TestRuleReads(t *testing.T) {
	r := sharedRig(t, nil)
	ctx := t.Context()
	feed, _ := r.s.Sites.Lookup(feedSite)
	now := r.end.Add(time.Minute)
	var mu sync.Mutex
	seen := map[string][]any{}
	traceSQL = func(q string, args []any) {
		mu.Lock()
		defer mu.Unlock()
		if _, ok := seen[q]; !ok {
			seen[q] = append([]any(nil), args...)
		}
	}
	t.Cleanup(func() { traceSQL = nil })

	facts, unknown, err := r.s.Findings(ctx, feed, now, DefaultThresholds())
	if len(unknown) != 0 {
		t.Errorf("a whole history read with gaps: %v", unknown)
	}
	if err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{}
	r.s.Model = model
	r.get(t, "noteworthy", p(feedSite, "range", "24h", "tz", "UTC"))
	r.s.Model = nil
	var want, got []string
	if len(model.briefIns) == 1 {
		for _, f := range model.briefIns[0].Facts {
			want = append(want, f.ID)
		}
	}
	for _, f := range facts {
		got = append(got, f.ID)
	}
	if !slices.Equal(got, want) {
		t.Errorf("findings %v, Noteworthy's 24h %v", got, want)
	}
	loose := DefaultThresholds()
	loose.AutoSharePP, loose.TrendVisitors, loose.TrendVisitorsPct, loose.NewProbes = 0, 0, 0, 1
	loose.SpikeAddr = Spike{Min: 1, Share: 0.01, Growth: 1}
	more, _, err := r.s.Findings(ctx, feed, now, loose)
	if err != nil {
		t.Fatal(err)
	}
	if len(more) <= len(facts) {
		t.Errorf("looser thresholds found %d facts, the defaults %d", len(more), len(facts))
	}
	for _, f := range more {
		if strings.HasPrefix(f.ID, "spike:addr:") && (f.Share <= 0 || f.Share > 1 || f.Growth < 1 || f.Value < 1) {
			t.Errorf("spike %+v", f)
		}
	}

	cur, prev, err := r.s.Levels(ctx, feed, LevelRequests, time.Hour, now, []int{1, 2, 3, 7, 14, 21})
	if err != nil {
		t.Fatal(err)
	}
	if math.IsNaN(cur.Value) || cur.Value <= 0 || len(prev) != 6 || math.IsNaN(prev[0].Value) {
		t.Errorf("levels %+v %+v", cur, prev)
	}
	// Before the history's first figure there is none.
	if !math.IsNaN(prev[len(prev)-1].Value) {
		t.Errorf("21 days back of a 20-day history: %+v", prev[len(prev)-1])
	}
	if v, _, _ := r.s.Levels(ctx, feed, LevelP95, time.Hour, now, nil); !math.IsNaN(v.Value) {
		t.Errorf("an API's p95 on a website: %v", v)
	}
	if v, _, _ := r.s.Levels(ctx, feed, LevelVisitors, 24*time.Hour, now, nil); math.IsNaN(v.Value) || v.Value == 0 {
		t.Errorf("visitors %v", v)
	}

	c, err := r.s.Collector(ctx, feed.ID, r.end.Add(-31*time.Hour), now)
	if err != nil || c.Refused != 4 {
		t.Errorf("collected %+v %v", c, err)
	}
	if c, _ := r.s.Collector(ctx, 0, r.end.Add(-time.Hour), now); c.UnknownDomain != 2 {
		t.Errorf("unknown domains %+v", c)
	}
	if c, _ := r.s.Collector(ctx, feed.ID, r.end.Add(-time.Hour), now); c.Refused != 0 {
		t.Errorf("an hour without refusals %+v", c)
	}
	if b, sv, err := r.s.LastSeen(ctx, feed.ID); err != nil || b.IsZero() || sv.IsZero() {
		t.Errorf("last seen %v %v %v", b, sv, err)
	}

	traceSQL = nil
	if len(seen) < 10 {
		t.Fatalf("only %d statements traced", len(seen))
	}
	for q, args := range seen {
		plan, err := plancheck.Explain(ctx, r.s.DB.DB, q, args...)
		if err != nil {
			t.Fatalf("explain %q: %v", q, err)
		}
		if p := plancheck.Problem(q, plan); p != "" {
			t.Errorf("%s: %s", p, strings.Join(strings.Fields(q), " "))
		}
	}
}
