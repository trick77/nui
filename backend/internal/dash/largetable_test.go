package dash

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trick77/stakeout/backend/internal/agg"
	"github.com/trick77/stakeout/backend/internal/plancheck"
	"github.com/trick77/stakeout/backend/internal/sim"
	"github.com/trick77/stakeout/backend/internal/store"
)

// TestLargeTable fills 13 months of traffic (about 100k events, the feed's asset requests included), builds its
// aggregates, runs every endpoint over the longest range from each source (auto while a writer keeps inserting), and
// checks the plan of every statement the dashboard and the builder ran: none may scan the event table or an
// aggregate table. Plans, not wall times: those are flaky under -race and on CI.
func TestLargeTable(t *testing.T) {
	if testing.Short() {
		t.Skip("large table")
	}
	// Under -race the data is smaller (the plans are the same, the race detector makes SQLite crawl); a plain
	// go test runs the full size.
	days, people, want := 395, 4, int64(50_000)
	if raceOn {
		days, people, want = 45, 1, 1_500
	}
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
	trace := func(into map[string]stmt) func(string, []any) {
		return func(q string, args []any) {
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
		{"log", long("kind", "request", "asn", "15169")},
		{"domain", long()},
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
		ctx, cancel := context.WithCancel(t.Context())
		var written atomic.Int64
		var writeErr atomic.Value
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; ctx.Err() == nil; i++ {
				batch := make([]store.Event, 50)
				for j := range batch {
					batch[j] = store.Event{SiteID: into, TS: r.end.UnixMilli(), Source: "server", Kind: "request",
						IP: "192.0.2.200", IPKey: "192.0.2.200", Template: "GET /", Status: 200}
				}
				if err := r.s.DB.InsertEvents(ctx, batch); err != nil {
					if ctx.Err() == nil {
						writeErr.Store(err)
					}
					return
				}
				written.Add(int64(len(batch)))
				time.Sleep(5 * time.Millisecond)
			}
		}()
		var took time.Duration
		asked := calls
		if src == SourceRaw {
			asked = append(slices.Clone(calls), weekCalls...)
		}
		for _, c := range asked {
			start := time.Now()
			if code, body := r.getFrom(t, src, c.ep, c.params); code != 200 {
				t.Errorf("%v %s %v: %d %v", src, c.ep, c.params, code, body)
			}
			d := time.Since(start)
			took += d
			t.Logf("%s %s %s %s: %v", srcName[src], c.ep, c.params["dim"], c.params["aud"], d.Round(time.Millisecond))
		}
		t.Logf("%s: every call in %v", srcName[src], took.Round(time.Millisecond))
		cancel()
		<-done
		if err := writeErr.Load(); err != nil {
			t.Fatalf("the writer failed during the reads: %v", err)
		}
		if written.Load() == 0 {
			t.Error("the writer made no progress during the reads")
		}
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

// addrKeySearch is a plan step searching agg_addr_key by site and key.
var addrKeySearch = regexp.MustCompile(`INDEX agg_addr_key \(site_id=\? AND ip_key=\?`)
