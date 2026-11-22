package dash

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trick77/nui/backend/internal/demo"
	"github.com/trick77/nui/backend/internal/store"
)

// TestLargeTable fills 13 months of traffic (about 100k events, the feed's asset requests included), runs every
// endpoint over the longest range while a writer keeps inserting, and checks the plan of every statement the
// dashboard ran: none may scan the event table. Plans, not wall times: those are flaky under -race and on CI.
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
	// No actor_class rows: one transaction per actor-day is what makes generating slow; the reads do not need them.
	if _, err := demo.Generate(t.Context(), r.s.DB, demo.Options{SiteID: site.ID, Days: days, People: people,
		End: r.end, Seed: 3, Feed: true}); err != nil {
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
	seen := map[string]stmt{}
	traceSQL = func(q string, args []any) {
		mu.Lock()
		defer mu.Unlock()
		if _, ok := seen[q]; !ok {
			seen[q] = stmt{q, append([]any(nil), args...)}
		}
	}
	t.Cleanup(func() { traceSQL = nil })

	// The collector keeps writing meanwhile.
	ctx, cancel := context.WithCancel(t.Context())
	var written atomic.Int64
	var writeErr atomic.Value
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ctx.Err() == nil; i++ {
			batch := make([]store.Event, 50)
			for j := range batch {
				batch[j] = store.Event{SiteID: site.ID, TS: r.end.UnixMilli(), Source: "server", Kind: "request",
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

	long := func(kv ...string) map[string]string { return p(feedSite, append([]string{"range", "12mo"}, kv...)...) }
	calls := []struct {
		ep     string
		params map[string]string
	}{
		{"overview", long()}, {"overview", long("aud", "bots", "cmp", "yoy")}, {"noteworthy", long()},
		{"classes", long()}, {"status", long()}, {"clients", long()}, {"visitors", long()},
		{"visitors", long("sort", "refused", "class", "unknown")}, {"visitor", long("key", "203.0.113.5")},
		{"log", long()}, {"log", long("addr", "203.0.113.5", "status", "4xx")}, {"log", long("path", "/.env")},
		{"log", long("kind", "request", "asn", "15169")},
		{"domain", long()},
	}
	for dim := range dims {
		calls = append(calls, struct {
			ep     string
			params map[string]string
		}{"list", long("dim", dim)})
	}
	calls = append(calls, struct {
		ep     string
		params map[string]string
	}{"list", long("dim", "pages", "within", "/:text")})
	for _, c := range calls {
		start := time.Now()
		if code, body := r.get(t, c.ep, c.params); code != 200 {
			t.Errorf("%s %v: %d %v", c.ep, c.params, code, body)
		}
		t.Logf("%s %s %s: %v", c.ep, c.params["dim"], c.params["aud"], time.Since(start).Round(time.Millisecond))
	}
	cancel()
	<-done
	if err := writeErr.Load(); err != nil {
		t.Fatalf("the writer failed during the reads: %v", err)
	}
	if written.Load() == 0 {
		t.Error("the writer made no progress during the reads")
	}

	if len(seen) < 20 {
		t.Fatalf("only %d statements traced", len(seen))
	}
	for _, s := range seen {
		rows, err := r.s.DB.DB.Query("EXPLAIN QUERY PLAN "+s.q, s.args...)
		if err != nil {
			t.Fatalf("explain %q: %v", s.q, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		for _, d := range plan {
			if strings.HasPrefix(d, "SCAN event") {
				t.Errorf("full scan of event:\n%s\nplan: %s", strings.Join(strings.Fields(s.q), " "), strings.Join(plan, " | "))
			}
		}
	}
}
