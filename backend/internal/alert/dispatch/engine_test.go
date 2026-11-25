package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/sites"
	"github.com/trick77/trakk/backend/internal/store"
)

// fakeSource answers each rule's occurrences from a map the test sets between steps.
type fakeSource struct {
	occ      map[string][]alert.Occurrence
	instance map[string][]alert.Occurrence
	// fail fails that many fact passes.
	fail int
}

func (f *fakeSource) Occurrences(_ context.Context, _ time.Time, r alert.Rule, _ *sites.Site, facts bool) (
	[]alert.Occurrence, bool, error) {
	if !r.Spec.Condition && !facts {
		return nil, false, nil
	}
	if !r.Spec.Condition && f.fail > 0 {
		f.fail--
		return nil, false, errors.New("database is locked")
	}
	return f.occ[r.Name], true, nil
}

func (f *fakeSource) Instance(_ context.Context, _ time.Time, r alert.Rule) ([]alert.Occurrence, bool, error) {
	return f.instance[r.Name], true, nil
}

type rig struct {
	t     *testing.T
	st    *store.Store
	e     *Engine
	src   *fakeSource
	rec   *Recorder
	board *alert.Board
	now   time.Time
	site  int64
}

var t0 = time.Date(2026, 11, 22, 10, 30, 0, 0, time.UTC)

func newRig(t *testing.T, file string) *rig {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rows, err := st.SyncSites(t.Context(), []store.Site{{Domain: "example.com", Kind: "site", Product: "Lens",
		PathRules: "[]", Active: true}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := sites.New(rows)
	if err != nil {
		t.Fatal(err)
	}
	c := alert.Parse([]byte(file), alert.Known{Domains: []string{"example.com"}, Products: []string{"Lens"}})
	if len(c.Faults) > 0 {
		t.Fatal(c.Faults)
	}
	r := &rig{t: t, st: st, src: &fakeSource{occ: map[string][]alert.Occurrence{},
		instance: map[string][]alert.Occurrence{}}, rec: &Recorder{}, board: alert.NewBoard(nil), now: t0,
		site: rows["example.com"].ID}
	r.e = New(Options{Store: st, Sites: reg, Config: c, Senders: r.rec.Senders(c), Board: r.board, Source: r.src,
		PublicFQDN: "trakk.example.com", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return r
}

// step advances the clock by d and steps.
func (r *rig) step(d time.Duration) {
	r.t.Helper()
	r.now = r.now.Add(d)
	if err := r.e.Step(r.t.Context(), r.now); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) rows(rule string) []store.Alert {
	r.t.Helper()
	var out []store.Alert
	for _, site := range []int64{0, r.site} {
		as, err := r.st.DB.QueryContext(r.t.Context(), `SELECT id FROM alert WHERE rule = ? AND site_id = ? ORDER BY id`,
			rule, site)
		if err != nil {
			r.t.Fatal(err)
		}
		var ids []int64
		for as.Next() {
			var id int64
			_ = as.Scan(&id)
			ids = append(ids, id)
		}
		_ = as.Close()
		for _, id := range ids {
			a, err := r.st.AlertPage(r.t.Context(), nil, id+1, 1)
			if err != nil {
				r.t.Fatal(err)
			}
			if len(a) == 1 && a[0].ID == id {
				out = append(out, a[0])
				continue
			}
			// baseline and pending rows are no page's: read them by their key.
			var key string
			_ = r.st.DB.QueryRowContext(r.t.Context(), `SELECT key FROM alert WHERE id = ?`, id).Scan(&key)
			last, _, _ := r.st.LastAlert(r.t.Context(), rule, site, key)
			out = append(out, last)
		}
	}
	return out
}

func occ(keys ...string) []alert.Occurrence {
	var out []alert.Occurrence
	for _, k := range keys {
		out = append(out, alert.Occurrence{Key: k, Text: "New error: " + alert.Line(k, 120), Unit: "times",
			Figure: new(3.0), Target: &alert.Target{Panel: "errors", Filter: map[string]string{}}})
	}
	return out
}

const ops = `"channels": {"ops": "ntfy://ntfy.example.com/topic"}, "defaults": {"channels": ["ops"], "cooldown": "2h"}`

// A fact rule: a baseline first, then each key entering the set once, a re-entry within the cooldown quiet.
func TestFacts(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "errs", "on": "error.new"}]}`)
	r.src.occ["errs"] = occ("old")
	r.step(0)
	if rows := r.rows("errs"); len(rows) != 1 || rows[0].State != store.AlertBaseline || rows[0].Due != "" {
		t.Fatalf("baseline %+v", rows)
	}
	if n := len(r.rec.Messages()); n != 0 {
		t.Fatalf("%d messages from a baseline", n)
	}
	// The next hour: two new keys, one message with both. Hostile text arrives defused, the link is the
	// dashboard's own.
	r.src.occ["errs"] = occ("old", "TypeError: x", "see https://evil.example.com/@me `x`")
	r.step(time.Hour)
	msgs := r.rec.Messages()
	if len(msgs) != 1 || msgs[0].Channel != "ops" || !strings.Contains(msgs[0].Title, "2 alerts") ||
		!strings.Contains(msgs[0].Body, "TypeError∶ x (3 times)") ||
		!strings.Contains(msgs[0].Body, "https://trakk.example.com/dashboard/alerts?site=example.com") {
		t.Fatalf("messages %+v", msgs)
	}
	if strings.Contains(msgs[0].Body, "https://evil") || strings.Contains(msgs[0].Body, "@me") ||
		strings.Contains(msgs[0].Body, "`") {
		t.Errorf("hostile text not defused: %s", msgs[0].Body)
	}
	rows := r.rows("errs")
	if len(rows) != 3 || rows[1].State != store.AlertOnce || rows[1].Told != "open" || rows[1].Due != "" ||
		rows[1].NotifiedAt == nil || rows[1].Delivered[0] != "ops" {
		t.Fatalf("rows %+v", rows)
	}
	// Still in the set: nothing new, however often it is evaluated.
	r.step(time.Hour)
	r.step(5 * time.Minute)
	if n := len(r.rec.Messages()); n != 1 {
		t.Fatalf("%d messages for keys still in the set", n)
	}
	// One leaves and comes back within the cooldown: its row reopens, nothing is sent.
	r.src.occ["errs"] = occ("old", "TypeError: x")
	r.step(time.Hour)
	if rows := r.rows("errs"); rows[2].LeftAt == nil {
		t.Fatalf("left %+v", rows[2])
	}
	r.src.occ["errs"] = occ("old", "TypeError: x", "see https://evil.example.com/@me `x`")
	r.step(time.Hour)
	if rows := r.rows("errs"); len(rows) != 3 || rows[2].LeftAt != nil {
		t.Fatalf("reopened %+v", rows)
	}
	if n := len(r.rec.Messages()); n != 1 {
		t.Fatalf("%d messages after a re-entry within the cooldown", n)
	}
	// Past the cooldown it is news again.
	r.src.occ["errs"] = occ("old", "TypeError: x")
	r.step(time.Hour)
	r.step(3 * time.Hour)
	r.src.occ["errs"] = occ("old", "TypeError: x", "see https://evil.example.com/@me `x`")
	r.step(time.Hour)
	if msgs := r.rec.Messages(); len(msgs) != 2 || !strings.HasPrefix(msgs[1].Title, "[normal] example.com: New error") {
		t.Fatalf("messages after the cooldown %+v", msgs)
	}
	// Fact rules wait for the next hour: a step inside it evaluates no facts.
	r.src.occ["errs"] = occ("old", "TypeError: x", "see https://evil.example.com/@me `x`", "late")
	r.step(10 * time.Minute)
	if n := len(r.rows("errs")); n != 4 {
		t.Errorf("%d rows: a fact evaluated inside the hour", n)
	}
}

// A message calls a site as the dashboard does; its link goes by the domain.
func TestNamedSite(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "errs", "on": "error.new", "sites": ["example.com"]}]}`)
	s, _ := r.e.o.Sites.Lookup("example.com")
	s.Name = "shop.example"
	r.step(0)
	r.src.occ["errs"] = occ("x")
	r.step(time.Hour)
	msgs := r.rec.Messages()
	if len(msgs) != 1 || !strings.HasPrefix(msgs[0].Title, "[normal] shop.example: New error") ||
		!strings.Contains(msgs[0].Body, "/dashboard/alerts?site=example.com") {
		t.Fatalf("messages %+v", msgs)
	}
}

// A condition: for before it opens, its clearing sent once its opening was, repeat while open.
func TestConditions(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "silent", "on": "source.silent", "after": "30m", "for": "10m",
		"repeat": "1h", "severity": "high"}]}`)
	r.step(0) // the baseline pass: nothing holds
	r.src.occ["silent"] = []alert.Occurrence{{Key: "server", Text: "No server events"}}
	r.step(5 * time.Minute)
	if rows := r.rows("silent"); len(rows) != 1 || rows[0].State != store.AlertPending {
		t.Fatalf("pending %+v", rows)
	}
	r.step(5 * time.Minute)
	r.step(5 * time.Minute)
	msgs := r.rec.Messages()
	if len(msgs) != 1 || msgs[0].Severity != alert.High || msgs[0].Title != "[high] example.com: No server events" {
		t.Fatalf("opened %+v", msgs)
	}
	for range 13 {
		r.step(5 * time.Minute)
	}
	if msgs := r.rec.Messages(); len(msgs) != 2 || !strings.Contains(msgs[1].Title, "Still open: ") {
		t.Fatalf("reminded %+v", msgs)
	}
	r.src.occ["silent"] = nil
	r.step(5 * time.Minute)
	msgs = r.rec.Messages()
	if len(msgs) != 3 || !strings.Contains(msgs[2].Title, "Cleared: example.com: No server events") {
		t.Fatalf("cleared %+v", msgs)
	}
	if rows := r.rows("silent"); rows[0].State != store.AlertCleared || rows[0].LeftAt == nil || rows[0].Told != "clear" {
		t.Errorf("row %+v", rows[0])
	}
	// A condition that clears within its for was never one: its row goes, nothing is sent.
	r.src.occ["silent"] = []alert.Occurrence{{Key: "browser", Text: "No browser events"}}
	r.step(5 * time.Minute)
	r.src.occ["silent"] = nil
	r.step(5 * time.Minute)
	if rows := r.rows("silent"); len(rows) != 1 || len(r.rec.Messages()) != 3 {
		t.Errorf("pending cleared: %+v, %d messages", rows, len(r.rec.Messages()))
	}
}

// A pass that does not know the state is none a condition held in: its for starts over, an open one stays open.
func TestHoldRestartsFor(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "errs", "on": "level.errors", "window": "1h", "min": 10, "for": "10m"}]}`)
	met := []alert.Occurrence{{Text: "Visitors with a script error over the last 1 h: 12 %"}}
	held := []alert.Occurrence{{Hold: true}}
	pass := func(o []alert.Occurrence) {
		r.src.occ["errs"] = o
		r.step(5 * time.Minute)
	}
	r.step(0)
	pass(met)
	for range 6 {
		pass(held)
	}
	pass(met)
	pass(met)
	if rows := r.rows("errs"); len(rows) != 1 || rows[0].State != store.AlertPending || len(r.rec.Messages()) != 0 {
		t.Fatalf("opened on held passes: %+v", rows)
	}
	pass(met)
	pass(held)
	if rows := r.rows("errs"); len(rows) != 1 || rows[0].State != store.AlertOpen || len(r.rec.Messages()) != 1 {
		t.Fatalf("open %+v", rows)
	}
}

// Too few affected visitors at a rule's first pass wait for nothing: the spread after it is news.
func TestHoldWithoutWaitIsBaseline(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "errs", "on": "level.errors", "window": "1h", "min": 10}]}`)
	r.src.occ["errs"] = []alert.Occurrence{{Hold: true}}
	r.step(0)
	r.src.occ["errs"] = []alert.Occurrence{{Text: "Visitors with a script error over the last 1 h: 60 %"}}
	r.step(5 * time.Minute)
	if rows := r.rows("errs"); len(rows) != 1 || rows[0].State != store.AlertOpen || len(r.rec.Messages()) != 1 {
		t.Fatalf("rows %+v", rows)
	}
}

// A changed threshold under the same name starts a new baseline; so does a rule not run for six hours.
func TestBaselineAgain(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "errs", "on": "error.new"}]}`)
	r.src.occ["errs"] = occ("a")
	r.step(0)
	c := alert.Parse([]byte(`{`+ops+`, "rules": [{"name": "errs", "on": "error.new", "min": 4}]}`),
		alert.Known{Domains: []string{"example.com"}})
	r.e.o.Config = c
	r.src.occ["errs"] = occ("a", "b")
	r.step(time.Hour)
	if n := len(r.rec.Messages()); n != 0 {
		t.Fatalf("%d messages after a new signature", n)
	}
	r.src.occ["errs"] = occ("a", "b", "c")
	r.step(7 * time.Hour)
	if n := len(r.rec.Messages()); n != 0 {
		t.Fatalf("%d messages after six hours without a pass", n)
	}
	r.src.occ["errs"] = occ("a", "b", "c", "d")
	r.step(time.Hour)
	if n := len(r.rec.Messages()); n != 1 {
		t.Fatalf("%d messages for a key new after the baseline", n)
	}
}

// The outbox: the hourly cap queues, a failure is retried with a growing wait, a message older than six hours goes
// nowhere, a send the shutdown cut is no attempt.
func TestOutbox(t *testing.T) {
	r := newRig(t, `{"channels": {"ops": {"url": "ntfy://ntfy.example.com/t", "max_per_hour": 1}},
		"defaults": {"channels": ["ops"]}, "rules": [{"name": "errs", "on": "error.new"}]}`)
	r.step(0)
	r.src.occ["errs"] = occ("a")
	r.step(time.Hour)
	r.src.occ["errs"] = occ("a", "b")
	r.step(time.Hour - time.Minute) // within the hour of the first message
	if n := len(r.rec.Messages()); n != 1 {
		t.Fatalf("%d messages past the cap", n)
	}
	r.step(time.Minute)
	if n := len(r.rec.Messages()); n != 2 {
		t.Fatalf("%d messages after the hour", n)
	}

	r.rec.Fail = errors.New(`Post "https://ntfy.example.com/t?auth=hunter2": refused`)
	r.src.occ["errs"] = occ("a", "b", "c")
	r.step(time.Hour)
	row := r.rows("errs")[2]
	if row.Due != "open" || row.Attempts != 1 || row.Failed[0] != "ops" {
		t.Fatalf("failed row %+v", row)
	}
	st, err := r.e.Status(t.Context())
	if err != nil || len(st.Channels) != 1 || st.Channels[0].Error == "" || strings.Contains(st.Channels[0].Error, "hunter2") ||
		st.Channels[0].LastOK.IsZero() || st.Active != 1 || !st.Configured {
		t.Fatalf("status %+v %v", st, err)
	}
	r.step(time.Minute) // within the backoff: not tried
	if row := r.rows("errs")[2]; row.Attempts != 1 {
		t.Fatalf("tried within the backoff: %+v", row)
	}
	r.rec.Fail = nil
	r.step(2 * time.Minute)
	if row := r.rows("errs")[2]; row.Due != "" || row.Attempts != 2 || len(row.Failed) != 0 || row.Delivered[0] != "ops" {
		t.Fatalf("retried row %+v", row)
	}

	// The shutdown cuts a send: no attempt, still due.
	r.src.occ["errs"] = occ("a", "b", "c", "d")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.now = r.now.Add(time.Hour)
	if err := r.e.Step(ctx, r.now); err == nil {
		t.Fatal("a cancelled step went through")
	}
	// The cancelled step's evaluation may have stopped before it recorded the key: a fresh step records it.
	r.e.condAt, r.e.factHour = time.Time{}, 0
	r.rec.Fail = errors.New("down")
	r.step(0)
	rows := r.rows("errs")
	d := rows[len(rows)-1]
	if d.Due != "open" || d.Attempts != 1 {
		t.Fatalf("row %+v", d)
	}
	// Down for longer than six hours: given up, failed.
	r.step(7 * time.Hour)
	rows = r.rows("errs")
	if d := rows[len(rows)-1]; d.Due != "" || d.Failed[0] != "ops" {
		t.Fatalf("stale row %+v", d)
	}
}

// Severity below a channel's minimum, sending switched off and a channel without a sender: recorded, not sent.
func TestNotSent(t *testing.T) {
	for name, file := range map[string]string{
		"severity": `{"channels": {"ops": {"url": "ntfy://ntfy.example.com/t", "min_severity": "high"}},
			"defaults": {"channels": ["ops"]}, "rules": [{"name": "errs", "on": "error.new"}]}`,
		"off":     `{"enabled": false, ` + ops + `, "rules": [{"name": "errs", "on": "error.new"}]}`,
		"no rule": `{` + ops + `, "rules": []}`,
	} {
		r := newRig(t, file)
		rule := "errs"
		if name == "no rule" {
			rule = "error.new"
		}
		r.step(0)
		r.src.occ[rule] = occ("a")
		r.step(time.Hour)
		rows := r.rows(rule)
		if len(rows) != 1 || rows[0].State != store.AlertOnce || rows[0].Due != "" || len(rows[0].Channels) != 0 ||
			len(r.rec.Messages()) != 0 {
			t.Errorf("%s: %+v, %d messages", name, rows, len(r.rec.Messages()))
		}
	}
	r := newRig(t, `{`+ops+`, "rules": [{"name": "errs", "on": "error.new"}]}`)
	r.e.o.Senders = nil
	r.step(0)
	r.src.occ["errs"] = occ("a")
	r.step(time.Hour)
	if rows := r.rows("errs"); rows[0].Due != "" || len(r.rec.Messages()) != 0 {
		t.Errorf("no sender: %+v", rows)
	}
}

// The board's conditions: a job not heard from since start holds its row, a failure opens, the next run clears.
func TestJobs(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "jobs", "on": "job.failed", "jobs": ["agg", "writer"]},
		{"name": "cap", "on": "db.size_cap"}]}`)
	r.step(0)
	alert.Fail(r.board, alert.JobAgg, errors.New("site 1 day 5: disk I/O error"))
	alert.Note(r.board, alert.Condition{Kind: alert.KindSizeCap, Open: true, Text: "over by 3 MiB"})
	r.step(5 * time.Minute)
	msgs := r.rec.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, "Trakk: The aggregate build failed") ||
		!strings.Contains(msgs[0].Body, "disk I/O error") || !strings.Contains(msgs[0].Body, "size cap") ||
		!strings.HasSuffix(msgs[0].Body, "https://trakk.example.com/dashboard/alerts?all=1") { // not one site's: all
		t.Fatalf("messages %+v", msgs)
	}
	// A restart: the board forgets, the row holds.
	r.e.o.Board = alert.NewBoard(nil)
	r.step(5 * time.Minute)
	if rows := r.rows("jobs"); len(rows) != 1 || rows[0].State != store.AlertOpen {
		t.Fatalf("held %+v", rows)
	}
	alert.Fail(r.e.o.Board, alert.JobAgg, nil)
	r.step(5 * time.Minute)
	if rows := r.rows("jobs"); rows[0].State != store.AlertCleared {
		t.Fatalf("cleared %+v", rows)
	}
	if msgs := r.rec.Messages(); len(msgs) != 2 || !strings.Contains(msgs[1].Title, "Cleared") {
		t.Fatalf("messages %+v", msgs)
	}
}

// A new error's message links to its rows in the dashboard's log, its message encoded in the link.
func TestLogLink(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "errs", "on": "error.new"}]}`)
	r.step(0)
	r.src.occ["errs"] = []alert.Occurrence{{Key: "TypeError: a b", Text: "New error: TypeError: a b",
		Target: &alert.Target{Panel: "log", Filter: map[string]string{"kind": "js_error", "name": "TypeError: a b"}}}}
	r.step(time.Hour)
	msgs := r.rec.Messages()
	want := "https://trakk.example.com/dashboard/log?kind=js_error&name=TypeError%3A+a+b&range=24h&site=example.com"
	if len(msgs) != 1 || !strings.Contains(msgs[0].Body, want) {
		t.Fatalf("messages %+v", msgs)
	}
	if l := r.e.logLink(&store.Alert{SiteID: r.site, Panel: "errors"}); l != "" {
		t.Errorf("no log target: %q", l)
	}
	if l := r.e.logLink(&store.Alert{SiteID: 0, Panel: "log"}); l != "" {
		t.Errorf("the instance: %q", l)
	}
}

func TestTest(t *testing.T) {
	c := alert.Parse([]byte(`{"channels": {"a": "ntfy://ntfy.example.com/t", "b": "gotify://g.example.com/t"}}`),
		alert.Known{})
	rec := &Recorder{}
	var out strings.Builder
	if err := Test(t.Context(), c, rec.Senders(c), &out); err != nil || !strings.Contains(out.String(), "a (ntfy): sent") {
		t.Errorf("%v: %s", err, out.String())
	}
	out.Reset()
	if err := Test(t.Context(), c, nil, &out); !errors.Is(err, ErrNotSent) || !strings.Contains(out.String(), "no sender") {
		t.Errorf("%v: %s", err, out.String())
	}
	out.Reset()
	rec.Fail = errors.New("smtp://user:pw@mail.example.com refused")
	if err := Test(t.Context(), c, rec.Senders(c), &out); err == nil || strings.Contains(out.String(), "pw@") {
		t.Errorf("%v: %s", err, out.String())
	}
	out.Reset()
	if err := Test(t.Context(), alert.Config{}, nil, &out); err == nil {
		t.Error("no channel passed")
	}
}

// An open condition whose opening never got through is sent again: the channel down while it opened, it cleared
// before it was sent, came back within the cooldown and stays.
func TestUnheardOpenSentAgain(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "jobs", "on": "job.failed"}]}`)
	r.step(0)
	r.rec.Fail = errors.New("down")
	alert.Fail(r.board, alert.JobAgg, errors.New("disk I/O error"))
	r.step(5 * time.Minute) // opens, its send fails
	alert.Fail(r.board, alert.JobAgg, nil)
	r.step(5 * time.Minute) // clears before anyone heard: quiet
	r.rec.Fail = nil
	alert.Fail(r.board, alert.JobAgg, errors.New("disk I/O error"))
	r.step(5 * time.Minute) // back within the cooldown, and staying
	msgs := r.rec.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Title, "The aggregate build failed") ||
		strings.Contains(msgs[0].Title, "Cleared") {
		t.Fatalf("messages %+v", msgs)
	}
	if rows := r.rows("jobs"); len(rows) != 1 || rows[0].State != store.AlertOpen || rows[0].Told != "open" {
		t.Errorf("rows %+v", rows)
	}
}

// An opening given up after six hours is sent again while the condition lasts.
func TestGivenUpOpenSentAgain(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "jobs", "on": "job.failed"}]}`)
	r.step(0)
	r.rec.Fail = errors.New("down")
	alert.Fail(r.board, alert.JobAgg, errors.New("disk I/O error"))
	r.step(5 * time.Minute)
	r.step(7 * time.Hour) // given up
	if rows := r.rows("jobs"); rows[0].Due != "" || rows[0].Told != "" {
		t.Fatalf("given up %+v", rows[0])
	}
	r.rec.Fail = nil
	r.step(5 * time.Minute)
	if msgs := r.rec.Messages(); len(msgs) != 1 {
		t.Fatalf("messages %+v", msgs)
	}
}

// A clearing not sent yet is withdrawn when the condition is back within the cooldown: no "Cleared" while it is
// open, and its real clearing later is sent.
func TestReopenWithdrawsClear(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "jobs", "on": "job.failed"}]}`)
	r.step(0)
	alert.Fail(r.board, alert.JobAgg, errors.New("disk I/O error"))
	r.step(5 * time.Minute) // opened, sent
	r.rec.Fail = errors.New("down")
	alert.Fail(r.board, alert.JobAgg, nil)
	r.step(5 * time.Minute) // cleared, its send fails
	alert.Fail(r.board, alert.JobAgg, errors.New("disk I/O error"))
	r.step(5 * time.Minute) // back within the cooldown
	r.rec.Fail = nil
	r.step(5 * time.Minute)
	if msgs := r.rec.Messages(); len(msgs) != 1 {
		t.Fatalf("messages while it is open again %+v", msgs)
	}
	if rows := r.rows("jobs"); rows[0].State != store.AlertOpen || rows[0].Due != "" || rows[0].Told != "open" {
		t.Fatalf("row %+v", rows[0])
	}
	alert.Fail(r.board, alert.JobAgg, nil)
	r.step(5 * time.Minute)
	if msgs := r.rec.Messages(); len(msgs) != 2 || !strings.Contains(msgs[1].Title, "Cleared") {
		t.Fatalf("messages %+v", msgs)
	}
}

// The rows of a rule gone, switched off or no longer about a site are closed at the start, in silence, and pruned
// with the rest.
func TestRetire(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "errs", "on": "error.new"},
		{"name": "off", "on": "probe.new", "enabled": false}]}`)
	ctx := t.Context()
	at := r.now.UnixMilli()
	for _, rule := range []string{"gone", "off", "errs"} {
		a := store.Alert{SiteID: r.site, Rule: rule, Kind: "error.new", Key: "k", State: store.AlertOnce, Severity: "normal",
			Text: "t", FirstAt: at, LastAt: at, Told: "open"}
		if rule == "gone" {
			a.State, a.Kind, a.Due, a.DueAt, a.Channels = store.AlertOpen, "source.silent", "remind", &at, []string{"ops"}
		}
		if err := r.st.InsertAlert(ctx, &a); err != nil {
			t.Fatal(err)
		}
		if err := r.st.SetAlertRule(ctx, store.AlertRule{Rule: rule, SiteID: r.site, Sig: "s", CheckedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	// A site no longer configured: its rows of a live rule go too.
	gone := store.Alert{SiteID: 999, Rule: "errs", Kind: "error.new", Key: "k", State: store.AlertOnce, Severity: "normal",
		Text: "t", FirstAt: at, LastAt: at}
	if err := r.st.InsertAlert(ctx, &gone); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetAlertRule(ctx, store.AlertRule{Rule: "errs", SiteID: 999, Sig: "s", CheckedAt: at}); err != nil {
		t.Fatal(err)
	}
	r.src.occ["errs"] = occ("k")
	r.step(time.Minute)
	for _, rule := range []string{"gone", "off"} {
		if in, _ := r.st.InSet(ctx, rule, r.site); len(in) != 0 {
			t.Errorf("%s: still in its set %+v", rule, in)
		}
	}
	if in, _ := r.st.InSet(ctx, "errs", 999); len(in) != 0 {
		t.Errorf("a site gone: %+v", in)
	}
	if in, _ := r.st.InSet(ctx, "errs", r.site); len(in) != 1 {
		t.Errorf("a live rule's row closed: %+v", in)
	}
	if msgs := r.rec.Messages(); len(msgs) != 0 {
		t.Errorf("messages about rules gone %+v", msgs)
	}
	if n, err := r.st.PruneAlerts(ctx, r.now.Add(time.Hour)); err != nil || n != 3 {
		t.Errorf("pruned %d, %v", n, err)
	}
}

// A facts pass that failed is run again at the next step, not an hour later.
func TestFailedFactsRetried(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "errs", "on": "error.new"}]}`)
	r.step(0)
	r.src.occ["errs"], r.src.fail = occ("a"), 1
	r.now = r.now.Add(time.Hour)
	if err := r.e.Step(t.Context(), r.now); err == nil {
		t.Fatal("the failed pass went through")
	}
	r.step(time.Minute)
	if msgs := r.rec.Messages(); len(msgs) != 1 {
		t.Fatalf("messages %+v", msgs)
	}
	// A pass that keeps failing stops being retried within its hour.
	r.src.occ["errs"], r.src.fail = occ("a", "b"), 10
	r.now = r.now.Add(time.Hour)
	for range 5 {
		_ = r.e.Step(t.Context(), r.now)
		r.now = r.now.Add(time.Minute)
	}
	if r.src.fail != 10-factTries {
		t.Errorf("%d failed passes in an hour, want %d", 10-r.src.fail, factTries)
	}
}

// A rule's baseline waits for what it cannot judge yet: a source silent since before the start, judged once its
// after has passed, is part of the baseline, not news.
func TestBaselineWaitsForHeld(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "silent", "on": "source.silent", "after": "30m"}]}`)
	r.src.occ["silent"] = []alert.Occurrence{{Key: "server", Hold: true, Wait: true}}
	r.step(0)
	r.step(5 * time.Minute)
	r.src.occ["silent"] = []alert.Occurrence{{Key: "server", Text: "No events from the feed for 3 h"}}
	r.step(30 * time.Minute)
	r.step(5 * time.Minute)
	if msgs := r.rec.Messages(); len(msgs) != 0 {
		t.Fatalf("a source long silent sent as news: %+v", msgs)
	}
	if rows := r.rows("silent"); len(rows) != 1 || rows[0].State != store.AlertBaseline {
		t.Fatalf("rows %+v", rows)
	}
	// After the baseline, a source falling silent is news.
	r.src.occ["silent"] = append(r.src.occ["silent"], alert.Occurrence{Key: "browser", Text: "No events from the script"})
	r.step(5 * time.Minute)
	if msgs := r.rec.Messages(); len(msgs) != 1 || !strings.Contains(msgs[0].Title, "script") {
		t.Fatalf("messages %+v", msgs)
	}
}
