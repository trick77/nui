package sim

import (
	"bytes"
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/trick77/trakk/backend/internal/classify"
	"github.com/trick77/trakk/backend/internal/ingest"
	"github.com/trick77/trakk/backend/internal/sites"
	"github.com/trick77/trakk/backend/internal/store"
	"github.com/trick77/trakk/backend/internal/watch"
)

// Stats is what a delivery did.
type Stats struct {
	// Requests sent (retries included) and the events the collector accepted.
	Requests, Events int
	// Refused counts the answers other than 204 by status; Retried the requests sent again after a 429.
	Refused map[int]int
	Retried int
	// Skipped counts the requests a live delivery left out at its time budget; Failed the sends over HTTP that got
	// no answer or a 5xx (also counted in Refused).
	Skipped, Failed int
	Elapsed         time.Duration
}

// Rate is the accepted events per second of wall time.
func (s Stats) Rate() float64 {
	if s.Elapsed <= 0 {
		return 0
	}
	return float64(s.Events) / s.Elapsed.Seconds()
}

func (s Stats) String() string {
	out := fmt.Sprintf("%d events in %d requests, %s (%.0f events/s), refused %v, retried %d", s.Events, s.Requests,
		s.Elapsed.Round(time.Millisecond), s.Rate(), s.Refused, s.Retried)
	if s.Skipped > 0 {
		out += fmt.Sprintf(", skipped %d", s.Skipped)
	}
	if s.Failed > 0 {
		out += fmt.Sprintf(", failed %d", s.Failed)
	}
	return out
}

// SitesFile is the world's sites as a sites file declares them (feed keys as their hashes).
func (w *World) SitesFile() sites.File {
	var f sites.File
	for _, s := range w.Sites {
		c := sites.Config{Domain: s.Domain, Product: s.Product, Order: s.Order, Kind: s.Kind, PathRules: s.Rules}
		if s.Feed {
			c.FeedKeySHA256 = sites.HashKey(s.FeedKey)
		}
		f.Sites = append(f.Sites, c)
	}
	return f
}

// WriteSitesFile writes the world's sites file to path.
func (w *World) WriteSitesFile(path string) error {
	b, err := json.MarshalIndent(w.SitesFile(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// SyncSites makes the store's sites the world's, as a server started on its sites file would.
func (w *World) SyncSites(ctx context.Context, st *store.Store) (*sites.Registry, error) {
	rows, err := st.SyncSites(ctx, sites.Rows(w.SitesFile().Sites))
	if err != nil {
		return nil, err
	}
	return sites.New(rows)
}

// SitesFrom makes a world's sites of configured ones (server -demo): the script on every site that is no API, a
// feed where one is configured. Their feed keys are the simulator's: backfill files them under the configured site.
func SitesFrom(ctx context.Context, st *store.Store) ([]*Site, error) {
	rows, err := st.Sites(ctx)
	if err != nil {
		return nil, err
	}
	var active []store.Site
	for _, r := range rows {
		if r.Active {
			active = append(active, r)
		}
	}
	slices.SortFunc(active, func(a, b store.Site) int { return int(a.ID - b.ID) })
	out := make([]*Site, 0, len(active))
	for _, r := range active {
		var rules []sites.Rule
		if err := json.Unmarshal([]byte(r.PathRules), &rules); err != nil {
			return nil, fmt.Errorf("site %s: %w", r.Domain, err)
		}
		out = append(out, &Site{Domain: r.Domain, Product: r.Product, Order: r.SortOrder, Kind: r.Kind, Script: r.Kind != "api",
			Feed: r.FeedKeyHash != "", SPA: true, Rules: rules})
	}
	return out, nil
}

// backfill drives an in-process collector on the world's virtual clock.
type backfill struct {
	ctx   context.Context
	st    *store.Store
	now   time.Time
	col   *ingest.Collector
	cls   *classify.Classifier
	stats Stats
	log   *slog.Logger
}

func (b *backfill) clock() time.Time { return b.now }

// Drain thresholds: the writer's queue is written out before it nears its bounds (a full queue drops).
const (
	// A drain has the writer's drainTimeout of wall time: 1000 events took longer under -race on a busy CI runner
	// (150 events/s), and the writer dropped them.
	drainEvents = 200
	drainBytes  = 4 << 20
	queueEvents = 4000 // the writer's bound, far past the threshold and the largest batch
)

// BackfillOptions shape a backfill.
type BackfillOptions struct {
	// Days of history before the world's anchor.
	Days int
	// Classify runs the classifier's pass after every simulated day (one transaction per actor-day: off for load
	// runs, whose events keep the instant class the writer gave them).
	Classify bool
	// ClassifyLast, above 0, runs that pass on the last that many days only: the older ones keep the writer's
	// instant class (a long history whose old days nothing reads by class costs half as much).
	ClassifyLast int
	// Log reports each day's progress; nil is quiet.
	Log *slog.Logger
}

// Backfill writes the world's last o.Days days before its anchor into st through the real collector: the
// requests the clients would have sent, in time order, answered by the collector's own handlers on a virtual clock,
// its writer enriching with the world's reference data, its counters flushed into the hourly rows, the classifier's
// pass run at the end of every day. The world's sites must be in st (SyncSites, or a sites file).
func Backfill(ctx context.Context, st *store.Store, w *World, o BackfillOptions) (_ Stats, err error) {
	start := time.Now()
	b := &backfill{ctx: ctx, st: st, log: o.Log, stats: Stats{Refused: map[int]int{}}}
	if b.log == nil {
		b.log = quietLog()
	}
	rows, err := st.Sites(ctx)
	if err != nil {
		return b.stats, err
	}
	real, err := sites.New(rows)
	if err != nil {
		return b.stats, err
	}
	// The collector's own registry knows the simulator's feed keys for the configured sites.
	for _, s := range w.Sites {
		row, ok := rows[s.Domain]
		if !ok || !row.Active {
			return b.stats, fmt.Errorf("site %s is not configured", s.Domain)
		}
		if s.Feed {
			row.FeedKeyHash = sites.HashKey(s.FeedKey)
		}
		rows[s.Domain] = row
	}
	reg, err := sites.New(rows)
	if err != nil {
		return b.stats, err
	}
	counters := ingest.NewCounters(b.clock)
	b.cls = &classify.Classifier{DB: st, Sites: real, Ref: w.Ref.Data, Now: b.clock, Log: b.log}
	b.col = &ingest.Collector{Sites: reg, Counters: counters, Browser: ingest.BrowserTiers(0, b.clock),
		Feed: ingest.FeedLimiter(b.clock), Now: b.clock}
	b.col.Writer = b.writer()
	dropped := func() (int64, error) {
		var n int64
		err := st.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(dropped), 0) FROM ingest_stat`).Scan(&n)
		return n, err
	}
	before, err := dropped()
	if err != nil {
		return b.stats, err
	}
	defer func() {
		// A write the writer gave up on is only counted: a history with a hole is no fixture.
		after, derr := dropped()
		switch {
		case err != nil:
		case derr != nil:
			err = derr
		case after > before:
			err = fmt.Errorf("the writer dropped %d events", after-before)
		}
	}()
	from := w.Anchor.Add(-time.Duration(o.Days) * day)
	var retries reqHeap
	for t := from; t.Before(w.Anchor); {
		next := earlier(dayStart(dayOf(t)+1), w.Anchor)
		if err := b.window(w, t, next, &retries); err != nil {
			return b.stats, err
		}
		b.now = next.Add(-time.Millisecond)
		classify := o.Classify && (o.ClassifyLast <= 0 || !next.Before(w.Anchor.Add(-time.Duration(o.ClassifyLast)*day)))
		if err := b.settle(classify); err != nil {
			return b.stats, err
		}
		b.log.Info("Simulated day written", "day", t.Format(time.DateOnly), "events", b.stats.Events)
		t = next
	}
	b.stats.Elapsed = time.Since(start)
	return b.stats, nil
}

// window sends the requests of [from, to), and the retries due in it, in time order.
func (b *backfill) window(w *World, from, to time.Time, retries *reqHeap) error {
	// An hour at a time: a day of a large world is millions of requests.
	for t := from; t.Before(to); t = t.Add(time.Hour) {
		end := earlier(t.Add(time.Hour), to)
		for _, q := range w.Requests(t, end) {
			heap.Push(retries, q)
		}
		for retries.Len() > 0 && (*retries)[0].At.Before(end) {
			q := heap.Pop(retries).(*Request)
			// Before a batch that would pass the threshold, not after: a drain is that many events, or one batch.
			if n := b.col.Writer.Queued(); n > 0 && n+int64(q.Events()) > drainEvents {
				b.drain()
			}
			b.now = q.At
			status, wait := b.send(q)
			if status == http.StatusTooManyRequests && !q.Hammer() && q.Tries < 5 {
				q.At = q.At.Add(wait)
				b.stats.Retried++
				heap.Push(retries, q)
			}
			if b.col.Writer.QueuedBytes() > drainBytes {
				b.drain()
			}
		}
	}
	return b.ctx.Err()
}

// send hands one request to the collector's handler and books the answer.
func (b *backfill) send(q *Request) (int, time.Duration) {
	r := httptest.NewRequestWithContext(b.ctx, http.MethodPost, q.Path(), bytes.NewReader(q.Body(nil)))
	r.Header = q.Header()
	r.RemoteAddr = netip.AddrPortFrom(q.Addr(), 40000).String()
	rec := httptest.NewRecorder()
	if q.Feed {
		b.col.HandleFeed(rec, r)
	} else {
		b.col.HandleBrowser(rec, r)
	}
	q.Tries++
	b.stats.Requests++
	if rec.Code == http.StatusNoContent {
		b.stats.Events += q.Events()
		return rec.Code, 0
	}
	b.stats.Refused[rec.Code]++
	return rec.Code, retryAfter(rec.Header().Get("Retry-After"))
}

// retryAfter reads a Retry-After in seconds, a second when there is none, an hour at most.
func retryAfter(v string) time.Duration {
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		return time.Duration(min(s, 3600)) * time.Second
	}
	return time.Second
}

// writer is a fresh batch writer: it runs only when drained.
func (b *backfill) writer() *ingest.Writer {
	return ingest.NewWriter(b.st, b.col.Counters, queueEvents, b.cls.Enrich, b.log)
}

// drain writes what the writer holds now: Run on an ended context drains, flushes and returns.
func (b *backfill) drain() {
	ctx, cancel := context.WithCancel(b.ctx)
	cancel()
	b.col.Writer.Run(ctx)
	b.col.Writer = b.writer()
}

// settle ends a simulated day: the queue written, the counters flushed, the classifier's pass run, then the watch's,
// as a live server's jobs do at the day's end. The watch's runs every day, classified or not: it walks from the
// site's first day on, as a job that ran all along (a first pass over a whole history is a backfill and finds
// nothing).
func (b *backfill) settle(classify bool) error {
	b.drain()
	if err := b.col.Counters.Flush(b.ctx, b.st); err != nil {
		return err
	}
	if classify {
		if err := b.cls.Pass(b.ctx); err != nil {
			return err
		}
	}
	return watch.Pass(b.ctx, b.st, b.now)
}

// reqHeap orders requests by time, the earlier sent first at a tie.
type reqHeap []*Request

func (h reqHeap) Len() int { return len(h) }
func (h reqHeap) Less(i, j int) bool {
	if c := h[i].At.Compare(h[j].At); c != 0 {
		return c < 0
	}
	return h[i].seq < h[j].seq
}
func (h reqHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *reqHeap) Push(x any) { *h = append(*h, x.(*Request)) }
func (h *reqHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func sortedKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }
