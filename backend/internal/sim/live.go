package sim

import (
	"bytes"
	"container/heap"
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"time"
)

// Live runs a world on the wall clock: world time starts at the anchor when the run starts and runs speed times as
// fast. Tick hands out the requests due by now; nothing runs between ticks, so an idle live world costs nothing.
type Live struct {
	World *World
	Speed float64
	// MaxGap is the longest wall-clock gap a tick catches up on: after a longer idle spell the world skips ahead
	// instead of sending the missed time's traffic at once (the collector would date it all now).
	MaxGap time.Duration
	// MaxSpan is the most world time one tick hands out, whatever the speed: beyond it the world skips ahead.
	MaxSpan time.Duration
	// MaxEvents is the most events one tick hands out: an hour of the largest world, a live flood in it, can be
	// several times that. The span's earlier requests beyond it are skipped (Skipped counts them).
	MaxEvents int
	// Skipped counts the requests ticks left out at MaxEvents.
	Skipped int
	// Budget is the wall time one delivery may take: what is left after it is skipped, so a dashboard request that
	// advances the world never waits long.
	Budget time.Duration

	mu      sync.Mutex
	start   time.Time // the wall time the run began
	cursor  time.Time // world time up to which requests were handed out
	retries reqHeap
}

// NewLive starts w live at wall time start. A speed outside SpeedLimit is 1.
func NewLive(w *World, start time.Time, speed float64) *Live {
	if SpeedLimit.Check(speed) != nil {
		speed = 1
	}
	return &Live{World: w, Speed: speed, MaxGap: 2 * time.Minute, MaxSpan: time.Hour, MaxEvents: 20_000,
		Budget: time.Second, start: start, cursor: w.Anchor}
}

// maxWorld bounds world time's distance from the anchor, within a time.Duration's range: at the largest speed a
// month of wall time would overflow it, and world time would run backwards.
const maxWorld = 200 * 365 * day

// scaled is d times f, saturated at ±maxWorld.
func scaled(d time.Duration, f float64) time.Duration {
	x := float64(d) * f
	switch {
	case !(x < float64(maxWorld)):
		return maxWorld
	case x < -float64(maxWorld):
		return -maxWorld
	}
	return time.Duration(x)
}

// WorldTime is the world's time at wall time t. It stops at maxWorld past the anchor.
func (l *Live) WorldTime(t time.Time) time.Time {
	return l.World.Anchor.Add(scaled(t.Sub(l.start), l.Speed))
}

// Clock states world times as the wall clock's milliseconds: what a request says when it is sent live.
func (l *Live) Clock() Clock {
	return func(t time.Time) int64 {
		return l.start.Add(scaled(t.Sub(l.World.Anchor), 1/l.Speed)).UnixMilli()
	}
}

// Tick returns the requests due at wall time now, retries included, in time order.
func (l *Live) Tick(now time.Time) []*Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	to := l.WorldTime(now)
	if !to.After(l.cursor) {
		return nil
	}
	from := l.cursor
	if gap := min(scaled(l.MaxGap, l.Speed), l.MaxSpan); to.Sub(from) > gap {
		from = to.Add(-gap)
	}
	l.cursor = to
	reqs := l.World.Requests(from, to)
	if l.MaxEvents > 0 {
		// The latest requests up to the bound, as a skip ahead keeps the latest time.
		n, i := 0, len(reqs)
		for i > 0 && n+reqs[i-1].Events() <= l.MaxEvents {
			i--
			n += reqs[i].Events()
		}
		l.Skipped += i
		reqs = reqs[i:]
	}
	for _, q := range reqs {
		heap.Push(&l.retries, q)
	}
	var out []*Request
	for l.retries.Len() > 0 && !l.retries[0].At.After(to) {
		out = append(out, heap.Pop(&l.retries).(*Request))
	}
	return out
}

// Retry queues a refused request again after wait of wall time, as a client honouring Retry-After does; one that
// ignores 429 or was tried five times is dropped.
func (l *Live) Retry(q *Request, wait time.Duration) bool {
	if q.Hammer() || q.Tries >= 5 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// From now, not from the last tick: the wait is the server's, counted from its answer.
	q.At = l.WorldTime(time.Now()).Add(scaled(wait, l.Speed))
	heap.Push(&l.retries, q)
	return true
}

// Handlers are the collector's two endpoints, for a live world driven in-process.
type Handlers struct {
	Browser, Feed http.Handler
}

// Deliver sends requests to the collector's handlers in-process (the server's own collector, on its own clock),
// queueing a 429 for its retry. It returns the stats of the delivery.
func (l *Live) Deliver(ctx context.Context, h Handlers, reqs []*Request) Stats {
	st := Stats{Refused: map[int]int{}}
	clock := l.Clock()
	deadline := time.Now().Add(l.Budget)
	for i, q := range reqs {
		if l.Budget > 0 && time.Now().After(deadline) {
			st.Skipped = len(reqs) - i
			break
		}
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, q.Path(), bytes.NewReader(q.Body(clock)))
		r.Header = q.Header()
		r.RemoteAddr = netip.AddrPortFrom(q.Addr(), 40000).String()
		rec := httptest.NewRecorder()
		if q.Feed {
			h.Feed.ServeHTTP(rec, r)
		} else {
			h.Browser.ServeHTTP(rec, r)
		}
		q.Tries++
		st.Requests++
		if rec.Code == http.StatusNoContent {
			st.Events += q.Events()
			continue
		}
		st.Refused[rec.Code]++
		if rec.Code == http.StatusTooManyRequests && l.Retry(q, retryAfter(rec.Header().Get("Retry-After"))) {
			st.Retried++
		}
	}
	return st
}

// Wrap advances the world at the top of every request to next and delivers what is due into the collector's
// handlers: the dashboard moves while one watches it, and only then. slow delays every answer (a slow server).
func (l *Live) Wrap(next http.Handler, h Handlers, slow time.Duration) http.Handler {
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		l.Deliver(context.WithoutCancel(r.Context()), h, l.Tick(time.Now()))
		mu.Unlock()
		if slow > 0 {
			t := time.NewTimer(slow)
			select {
			case <-t.C:
			case <-r.Context().Done():
				t.Stop()
			}
		}
		next.ServeHTTP(w, r)
	})
}
