package dash

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/agg"
	"github.com/trick77/trakk/backend/internal/classify"
	"github.com/trick77/trakk/backend/internal/plancheck"
	"github.com/trick77/trakk/backend/internal/sim"
	"github.com/trick77/trakk/backend/internal/store"
)

// writeBeside keeps inserting into a site, as the collector does, until stop, which checks that it went well.
func writeBeside(t *testing.T, r *rig, site int64) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var written int64
	var writeErr error
	done := make(chan struct{})
	first := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			batch := make([]store.Event, 50)
			for j := range batch {
				batch[j] = store.Event{SiteID: site, TS: r.end.UnixMilli(), Source: "server", Kind: "request",
					IP: "192.0.2.200", IPKey: "192.0.2.200", Template: "GET /", Status: 200}
			}
			if err := r.s.DB.InsertEvents(ctx, batch); err != nil {
				if ctx.Err() == nil {
					writeErr = err
				}
				return
			}
			if written == 0 {
				close(first)
			}
			written += int64(len(batch))
			time.Sleep(5 * time.Millisecond)
		}
	}()
	select { // the reads begin beside a writer under way
	case <-first:
	case <-done:
	}
	return func() {
		t.Helper()
		cancel()
		<-done
		if writeErr != nil {
			t.Fatalf("the writer failed during the reads: %v", writeErr)
		}
		if written == 0 {
			t.Error("the writer made no progress during the reads")
		}
	}
}

// Every endpoint reads beside a writer into its site, under -race too, where TestLargeTable does not run.
func TestReadsBesideWriter(t *testing.T) {
	r := newRig(t, 2, 4, nil)
	site, _ := r.s.Sites.Lookup(feedSite)
	stop := writeBeside(t, r, site.ID)
	for _, rng := range []string{"24h", "30d"} {
		for _, c := range [][]string{{"overview"}, {"noteworthy"}, {"classes"}, {"status"}, {"clients"}, {"visitors"},
			{"visitor", "key", "192.0.2.200"}, {"log"}, {"domain"}, {"error", "key", "Script error."},
			{"list", "dim", "pages"}} {
			code, body := r.getFrom(t, SourceAuto, c[0], p(feedSite, append([]string{"range", rng}, c[1:]...)...))
			if code != 200 {
				t.Errorf("%s %v: %d %v", rng, c, code, body)
			}
		}
	}
	stop()
}

// TestLargeTable fills 13 months of traffic (about 100k events, the feed's asset requests included), builds its
// aggregates, runs every endpoint over the longest range from each source (auto while a writer keeps inserting), and
// checks the plan of every statement the dashboard and the builder ran: none may scan the event table or an
// aggregate table. Plans, not wall times: those are flaky on CI.
func TestLargeTable(t *testing.T) {
	skipRace(t)
	if testing.Short() {
		t.Skip("large table")
	}
	days, people, want := 395, 4, int64(50_000)
	r := newRig(t, 0, 0, &fakeModel{})
	site, _ := r.s.Sites.Lookup(feedSite)
	other, _ := r.s.Sites.Lookup(plainSite)
	// No classifier pass: one transaction per actor-day is what makes a history slow; the reads do not need it.
	if _, _, err := sim.History(t.Context(), r.s.DB, sim.HistoryOptions{Seed: 3, Anchor: r.end, Days: days,
		Scale: float64(people) / 25, Scenarios: "api-walker,scan-wave"}); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := r.s.DB.DB.QueryRow(`SELECT COUNT(*) FROM event`).Scan(&n); err != nil || n < want {
		t.Fatalf("events = %d, %v", n, err)
	}

	type stmt struct {
		q    string
		args []any
	}
	var mu sync.Mutex
	seen, built := map[string]stmt{}, map[string]stmt{}
	var wide []string // runs of days on agg_day read in one statement past a month
	var count atomic.Int64
	trace := func(into map[string]stmt) func(string, []any) {
		return func(q string, args []any) {
			count.Add(1)
			mu.Lock()
			defer mu.Unlock()
			if strings.Contains(q, "FROM agg_day d") && strings.HasSuffix(strings.TrimSpace(q), "d.day <= ?") {
				lo, ok1 := args[len(args)-2].(int64)
				hi, ok2 := args[len(args)-1].(int64)
				if !ok1 || !ok2 || hi-lo >= spanDays {
					wide = append(wide, fmt.Sprintf("%d days: %s", hi-lo+1, strings.Join(strings.Fields(q), " ")))
				}
			}
			if _, ok := into[q]; !ok {
				into[q] = stmt{q, append([]any(nil), args...)}
			}
		}
	}
	agg.TraceSQL = trace(built)
	start := time.Now()
	if _, err := agg.BuildThrough(t.Context(), r.s.DB, r.end, agg.TopK); err != nil {
		t.Fatal(err)
	}
	agg.TraceSQL = nil
	t.Logf("aggregates of %d events built in %v", n, time.Since(start).Round(time.Millisecond))
	traceSQL = trace(seen)
	t.Cleanup(func() { traceSQL, agg.TraceSQL = nil, nil })

	long := func(kv ...string) map[string]string { return p(feedSite, append([]string{"range", "12mo"}, kv...)...) }
	type call struct {
		ep     string
		params map[string]string
	}
	calls := []call{
		{"overview", long()}, {"overview", long("aud", "bots", "cmp", "yoy")}, {"noteworthy", long()},
		{"classes", long()}, {"status", long()}, {"clients", long()}, {"visitors", long()},
		{"visitors", long("sort", "refused", "class", "unknown")}, {"visitor", long("key", "203.0.113.5")},
		{"log", long()}, {"log", long("addr", "203.0.113.5", "status", "4xx")}, {"log", long("path", "/.env")},
		{"log", long("kind", "request", "asn", "15169")}, {"log", long("kind", "js_error", "name", "Script error.")},
		{"log", long("endpoint", "POST /mcp whois")}, {"log", long("failure", "HTTP 429", "calls", "1")},
		{"domain", long()}, {"error", long("key", "Script error.")},
	}
	for dim := range dims {
		calls = append(calls, call{"list", long("dim", dim)})
	}
	calls = append(calls, call{"list", long("dim", "pages", "within", "/:text")})
	// 7d reads raw where 12mo reads the aggregates (the latest-client split, the hours, a list, the table), the
	// same under every source: asked once, raw.
	week := func(kv ...string) map[string]string { return p(feedSite, append([]string{"range", "7d"}, kv...)...) }
	weekCalls := []call{{"clients", week()}, {"clients", week("aud", "bots")}, {"overview", week()},
		{"list", week("dim", "pages")}, {"visitors", week()}}

	// The collector keeps writing meanwhile: under auto into the site read (its day goes dirty and is read raw), else
	// into the other site (SourceAgg fails on a dirty day by design; and the site's last day would grow by every
	// pass's writes).
	for _, src := range []Source{SourceAgg, SourceRaw, SourceAuto} {
		into := other.ID
		if src == SourceAuto {
			into = site.ID
		}
		stop := writeBeside(t, r, into)
		var took time.Duration
		asked := calls
		if src == SourceRaw {
			asked = append(slices.Clone(calls), weekCalls...)
		}
		for _, c := range asked {
			start, before := time.Now(), count.Load()
			uas := map[string]int{}
			clientName = func(ua string) string {
				mu.Lock()
				uas[ua]++
				mu.Unlock()
				return classify.Client(ua)
			}
			code, body := r.getFrom(t, src, c.ep, c.params)
			clientName = classify.Client
			if code != 200 {
				t.Errorf("%v %s %v: %d %v", src, c.ep, c.params, code, body)
			}
			if c.ep == "visitors" {
				clientNames(t, c.params, body, uas)
				if src == SourceAgg {
					visitorsBounds(t, r.s.DB.DB, site.ID, c.params, body, count.Load()-before)
				}
			}
			d := time.Since(start)
			took += d
			t.Logf("%s %s %s %s: %v", srcName[src], c.ep, c.params["dim"], c.params["aud"], d.Round(time.Millisecond))
		}
		t.Logf("%s: every call in %v", srcName[src], took.Round(time.Millisecond))
		stop()
	}

	if len(seen) < 20 || len(built) < 2 {
		t.Fatalf("only %d statements traced, %d of the builder's", len(seen), len(built))
	}
	for _, w := range wide {
		t.Errorf("one statement over %s", w)
	}
	explain := func(s stmt) []string {
		t.Helper()
		plan, err := plancheck.Explain(t.Context(), r.s.DB.DB, s.q, s.args...)
		if err != nil {
			t.Fatalf("explain %q: %v", s.q, err)
		}
		return plan
	}
	keyDays := 0
	for name, set := range map[string]map[string]stmt{"dashboard": seen, "builder": built} {
		for _, s := range set {
			plan := strings.Join(explain(s), " | ")
			if p := plancheck.Problem(s.q, explain(s)); p != "" {
				t.Errorf("%s: %s:\n%s\nplan: %s", name, p, strings.Join(strings.Fields(s.q), " "), plan)
			}
			// One key's days over a range: unpinned, the planner takes the primary key's (site_id, day) range, every
			// address of the range. Pinned, the index must be searched by the key: a named index alone would also
			// be walked whole.
			if strings.Contains(s.q, "INDEXED BY agg_addr_key") && !addrKeySearch.MatchString(plan) {
				t.Errorf("not on agg_addr_key by its key: %s\nplan: %s", strings.Join(strings.Fields(s.q), " "), plan)
			}
			if strings.Contains(s.q, "LEFT JOIN agg_addr t INDEXED BY agg_addr_key ON t.site_id = d.site_id AND t.ip_key") {
				keyDays++
			}
		}
	}
	if keyDays == 0 {
		t.Error("no read of one key's days pinned to agg_addr_key")
	}
}

var srcName = map[Source]string{SourceAuto: "auto", SourceRaw: "raw", SourceAgg: "aggregates"}

// visitorsBounds checks what one 12-month visitors table from the aggregates costs, by counts, not wall time (a year
// of 23 M events once took 5.4 s warm, 26 s cold), from what the seeded history holds (more than the range): a
// statement per built day, a month's piece of the classes over the range, and per page row a statement per month of
// its raw span, its values and trail, and its reading's raw sums when raw begins inside the range. The months are counted here, not from the code's spans: shorter windows must fail.
func visitorsBounds(t *testing.T, db *sql.DB, site int64, params map[string]string, body map[string]any, stmts int64) {
	t.Helper()
	loc, err := time.LoadLocation(cmp.Or(params["tz"], "UTC"))
	if err != nil {
		t.Fatal(err)
	}
	p := newPeriod(params["range"], "prev", rigEnd.Add(time.Minute), loc)
	from, to := ms(p.win.From), ms(p.Now)
	var days, oldest int64
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM agg_day WHERE site_id = ? AND day >= ? AND day < ?
		AND frozen = 1`, site, from/dayMS, to/dayMS).Scan(&days); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), `SELECT MIN(ts) FROM event WHERE site_id = ?`, site).Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	month := int64(31 * 24 * time.Hour / time.Millisecond)
	raw := max(from, oldest)
	perRow := (to-raw+month-1)/month + 2 // the raw span's months, the values, the trail
	if raw > from {
		perRow += 3 // the reading's raw sums, sessions and latest event
	}
	classes := (to/dayMS - from/dayMS + 31) / 31
	rows := int64(len(body["rows"].([]any)))
	// Besides: the plan's two, today's raw piece (two), detail_since's (up to three).
	limit := days + classes + rows*perRow + 7
	t.Logf("visitors %v: %d statements, limit %d: %d rows of %v, %d built days, %d per row", params, stmts, limit,
		rows, body["total"], days, perRow)
	if stmts > limit || rows == 0 {
		t.Errorf("visitors %v: %d statements for %d rows over %d built days, want at most %d", params, stmts, rows, days,
			limit)
	}
}

// clientNames checks a visitors call derives a client name once per distinct user agent, not per actor (59k rows
// parsed theirs one by one, 2.3 s), plus at most one per page row (a reading's own sums).
func clientNames(t *testing.T, params map[string]string, body map[string]any, uas map[string]int) {
	t.Helper()
	calls := 0
	for _, n := range uas {
		calls += n
	}
	if rows := len(body["rows"].([]any)); calls > len(uas)+rows {
		t.Errorf("visitors %v: %d client names derived for %d user agents, %d rows of %v", params, calls, len(uas), rows,
			body["total"])
	}
}

// addrKeySearch is a plan step searching agg_addr_key by site and key.
var addrKeySearch = regexp.MustCompile(`INDEX agg_addr_key \(site_id=\? AND ip_key=\?`)
