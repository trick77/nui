// Package ingest is the collector: POST /api/in (the browser script's batches) and POST /api/feed (a product
// backend's request records). Both validate, answer at once and hand the events to a bounded queue; one writer
// goroutine enriches them and inserts them in batches. A full queue drops and counts, never blocks a request: the
// collector must not slow a host page down, nor be made to hold memory by a flood.
package ingest

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trick77/ship/kit/logging"

	"github.com/trick77/stakeout/backend/internal/store"
)

// Writer limits.
const (
	// DefaultQueueEvents and DefaultQueueBytes bound what waits for the writer, events and memory: an event's
	// weight is its stored size (store.Event.Size) plus eventMemory for the row struct and its string headers.
	// 32 MiB of queue is about 50,000 typical events, or 2,000 at the collector's largest.
	DefaultQueueEvents = 20_000
	DefaultQueueBytes  = 32 << 20
	eventMemory        = 256
	// writeBatch is the most events one insert transaction takes; the writer also commits every flushEvery.
	writeBatch = 1000
	flushEvery = 250 * time.Millisecond
	// writeTimeout bounds one flush, its retries included; drainTimeout the last flush at shutdown. Neither is the
	// context shutdown cancels: a write under way finishes, and what was accepted is written.
	writeTimeout = 10 * time.Second
	drainTimeout = 5 * time.Second
	// StopTime is the longest Run takes to return once its context ends: the write under way, then the drain.
	StopTime = writeTimeout + drainTimeout
	// writeAttempts is how often a failed chunk is tried before its events are dropped and counted.
	writeAttempts = 3
)

// queuedBatch is one accepted batch and the hour its events were counted in: a failed write corrects that hour.
type queuedBatch struct {
	evs  []store.Event
	hour int64
}

// Writer is the bounded queue and its one writer goroutine.
type Writer struct {
	st          *store.Store
	ch          chan queuedBatch
	queued      atomic.Int64
	queuedBytes atomic.Int64
	max         int64
	maxBytes    int64
	enrich      func(*store.Event)
	counters    *Counters
	log         *slog.Logger
	done        chan struct{}
	// insert writes one chunk: the store's, a failing one in tests.
	insert func(context.Context, []store.Event) error
	// retryWait is the pause before the second attempt, doubled before each further one.
	retryWait time.Duration
}

// NewWriter returns a writer for st holding at most maxEvents queued (DefaultQueueEvents when 0) and
// DefaultQueueBytes of them. enrich, when not nil, runs on every event in the writer goroutine (network, country,
// first class), never in the request.
func NewWriter(st *store.Store, counters *Counters, maxEvents int, enrich func(*store.Event), log *slog.Logger) *Writer {
	if maxEvents <= 0 {
		maxEvents = DefaultQueueEvents
	}
	if log == nil {
		log = slog.Default()
	}
	return &Writer{st: st, ch: make(chan queuedBatch, maxEvents), max: int64(maxEvents),
		maxBytes: DefaultQueueBytes, enrich: enrich, counters: counters, log: log, done: make(chan struct{}),
		insert: st.InsertEvents, retryWait: 200 * time.Millisecond}
}

// weight is what a batch counts against the queue's byte bound.
func weight(evs []store.Event) int64 {
	var n int64
	for i := range evs {
		n += int64(evs[i].Size() + eventMemory)
	}
	return n
}

// Enqueue hands a batch counted in the current hour to the writer (EnqueueAt).
func (w *Writer) Enqueue(evs []store.Event) bool { return w.EnqueueAt(evs, w.counters.Hour()) }

// EnqueueAt hands a batch whose events were counted as accepted in hour to the writer, or reports false when the
// queue holds no room for all of it, in events or bytes: the caller counts the events as dropped.
func (w *Writer) EnqueueAt(evs []store.Event, hour int64) bool {
	n := int64(len(evs))
	if n == 0 {
		return true
	}
	b := weight(evs)
	// Both counters first, then the checks: a short-circuit would roll back bytes never added.
	events, bytes := w.queued.Add(n), w.queuedBytes.Add(b)
	if events > w.max || bytes > w.maxBytes {
		w.queued.Add(-n)
		w.queuedBytes.Add(-b)
		return false
	}
	select {
	case w.ch <- queuedBatch{evs: evs, hour: hour}:
		return true
	default:
		w.queued.Add(-n)
		w.queuedBytes.Add(-b)
		return false
	}
}

// Queued is the number of events waiting.
func (w *Writer) Queued() int64 { return w.queued.Load() }

// QueuedBytes is the weight of the events waiting.
func (w *Writer) QueuedBytes() int64 { return w.queuedBytes.Load() }

// Run writes until ctx ends, then drains what is queued (bounded by drainTimeout) and returns. Writes never use
// ctx itself: a flush under way when it ends completes instead of rolling back.
func (w *Writer) Run(ctx context.Context) {
	defer close(w.done)
	tick := time.NewTicker(flushEvery)
	defer tick.Stop()
	base := context.WithoutCancel(ctx)
	var pending []store.Event
	var hours []int64 // pending's counting hours, one per event
	take := func(b queuedBatch) {
		pending = append(pending, b.evs...)
		for range b.evs {
			hours = append(hours, b.hour)
		}
	}
	flush := func(limit time.Duration) {
		if len(pending) == 0 {
			return
		}
		fctx, cancel := context.WithTimeout(base, limit)
		w.write(fctx, pending, hours)
		cancel()
		pending, hours = pending[:0], hours[:0]
	}
	for {
		select {
		case <-ctx.Done():
		drain:
			for {
				select {
				case b := <-w.ch:
					take(b)
				default:
					break drain
				}
			}
			flush(drainTimeout)
			return
		default:
		}
		select {
		case b := <-w.ch:
			take(b)
			if len(pending) >= writeBatch {
				flush(writeTimeout)
			}
		case <-tick.C:
			flush(writeTimeout)
		case <-ctx.Done():
		}
	}
}

// Done is closed once Run has drained and returned.
func (w *Writer) Done() <-chan struct{} { return w.done }

// write inserts evs, hours[i] the hour evs[i] was counted in (nil: the current one). A chunk that keeps failing is
// dropped and moved from accepted to dropped in the counters it was counted in.
func (w *Writer) write(ctx context.Context, evs []store.Event, hours []int64) {
	// The weight as enqueued, before enrich adds a country: the queue gives back exactly what it took.
	b := weight(evs)
	defer func() {
		w.queued.Add(-int64(len(evs)))
		w.queuedBytes.Add(-b)
	}()
	if w.enrich != nil {
		for i := range evs {
			w.enrich(&evs[i])
		}
	}
	for start := 0; start < len(evs); start += writeBatch {
		end := min(start+writeBatch, len(evs))
		chunk := evs[start:end]
		if err := w.insertRetry(ctx, chunk); err != nil {
			w.log.Error("Event batch write failed, events dropped", "events", len(chunk), logging.Err(err))
			type key struct{ site, hour int64 }
			lost := map[key]*store.IngestStat{}
			for i, e := range chunk {
				k := key{e.SiteID, w.counters.Hour()}
				if hours != nil {
					k.hour = hours[start+i]
				}
				s := lost[k]
				if s == nil {
					s = &store.IngestStat{}
					lost[k] = s
				}
				s.Accepted++
				if e.Source == "server" {
					s.AcceptedServer++
				}
			}
			for k, s := range lost {
				w.counters.AddAt(k.site, k.hour, func(t *store.IngestStat) {
					t.Dropped += s.Accepted
					t.Accepted -= s.Accepted
					t.AcceptedServer -= s.AcceptedServer
				})
			}
		}
	}
}

// insertRetry writes a chunk, trying again after a failure (a lock held past the busy timeout, a full disk that
// retention is freeing) up to writeAttempts times within ctx.
func (w *Writer) insertRetry(ctx context.Context, chunk []store.Event) error {
	wait := w.retryWait
	var err error
	for attempt := 1; ; attempt++ {
		if err = w.insert(ctx, chunk); err == nil || attempt == writeAttempts {
			return err
		}
		w.log.Warn("Event batch write failed, retrying", "events", len(chunk), "attempt", attempt, logging.Err(err))
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
		wait *= 2
	}
}

// Counters are the collector's counters per site and hour, flushed to ingest_stat periodically, plus when each
// site's sources last delivered (since start; the dashboard falls back to the hourly rows for older times).
type Counters struct {
	mu   sync.Mutex
	m    map[[2]int64]*store.IngestStat
	last map[lastKey]time.Time
	now  func() time.Time
}

type lastKey struct {
	site   int64
	source string
}

// NewCounters returns empty counters on now (time.Now when nil).
func NewCounters(now func() time.Time) *Counters {
	if now == nil {
		now = time.Now
	}
	return &Counters{m: map[[2]int64]*store.IngestStat{}, last: map[lastKey]time.Time{}, now: now}
}

// Hour is the current hour, the counters' key.
func (c *Counters) Hour() int64 {
	if c == nil {
		return time.Now().Unix() / 3600
	}
	return c.now().Unix() / 3600
}

// Add applies f to site's counters for the current hour.
func (c *Counters) Add(site int64, f func(*store.IngestStat)) { c.AddAt(site, c.Hour(), f) }

// AddAt applies f to site's counters for hour. The counters are deltas to the stored hour: a correction may make
// one negative here, the stored sum never goes below zero (store.AddStats).
func (c *Counters) AddAt(site, hour int64, f func(*store.IngestStat)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := [2]int64{site, hour}
	s, ok := c.m[k]
	if !ok {
		s = &store.IngestStat{SiteID: site, Hour: hour}
		c.m[k] = s
	}
	f(s)
}

// Seen records that site's source delivered now.
func (c *Counters) Seen(site int64, source string) {
	now := c.now()
	c.mu.Lock()
	c.last[lastKey{site, source}] = now
	c.mu.Unlock()
}

// Last is when site's source last delivered since start, zero when it has not.
func (c *Counters) Last(site int64, source string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last[lastKey{site, source}]
}

// Pending is a copy of the counters not yet flushed, for the dashboard's current hour.
func (c *Counters) Pending() []store.IngestStat {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]store.IngestStat, 0, len(c.m))
	for _, s := range c.m {
		out = append(out, *s)
	}
	return out
}

// Flush writes the counters and resets them; on failure they are added back for the next flush.
func (c *Counters) Flush(ctx context.Context, st *store.Store) error {
	c.mu.Lock()
	taken := c.m
	c.m = map[[2]int64]*store.IngestStat{}
	c.mu.Unlock()
	stats := make([]store.IngestStat, 0, len(taken))
	for _, s := range taken {
		stats = append(stats, *s)
	}
	err := st.AddStats(ctx, stats)
	if err != nil {
		c.mu.Lock()
		for k, s := range taken {
			if t, ok := c.m[k]; ok {
				addStat(t, *s)
			} else {
				c.m[k] = s
			}
		}
		c.mu.Unlock()
	}
	return err
}

func addStat(t *store.IngestStat, s store.IngestStat) {
	t.Accepted += s.Accepted
	t.AcceptedServer += s.AcceptedServer
	t.RateLimited += s.RateLimited
	t.Rejected += s.Rejected
	t.Dropped += s.Dropped
	t.UnknownDomain += s.UnknownDomain
}
