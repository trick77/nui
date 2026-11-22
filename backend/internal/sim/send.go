package sim

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SendOptions shape a live run against a collector over HTTP.
type SendOptions struct {
	// Target is the collector's base URL (http://127.0.0.1:8080).
	Target string
	Client *http.Client
	// Workers send in parallel; 8 when 0.
	Workers int
	// For bounds the run; 0 runs until the context ends.
	For time.Duration
	// Every is the loop's beat; 100 ms when 0.
	Every time.Duration
	// Backoff is the wait before a request that failed (no answer, or a 5xx) is sent again, by the tries so far;
	// 1 s doubling to 30 s when nil.
	Backoff func(tries int) time.Duration
	// GiveUp is how long the target may fail every request (no answer, or a 5xx) before the run stops with an
	// error; 30 s when 0.
	GiveUp time.Duration
	// Refusals is how many batches of one site and endpoint the target may refuse for good (400, 401, 403, 404,
	// 413) with none taken before the run stops with an error: a wrong key or an unknown site; 5 when 0.
	Refusals int
	Log      *slog.Logger
}

// backoff is the default: a second, doubling, at most half a minute.
func backoff(tries int) time.Duration {
	return min(time.Second<<min(max(tries-1, 0), 5), 30*time.Second)
}

// stream is what the target answered one site's batches to one endpoint.
type stream struct {
	accepted int
	refused  map[int]int
}

// sendState is what the workers report to the loop.
type sendState struct {
	mu      sync.Mutex
	st      Stats
	streams map[string]*stream
	// failingSince is when the target began failing every request (zero while it answers); lastErr the last such
	// failure.
	failingSince time.Time
	lastErr      string
}

// Send runs l against a collector over HTTP: every beat it takes the requests due and posts them, each browser
// batch from its client's address (the one X-Forwarded-For entry, which the server reads with one trusted hop, as
// server -sim and BACKEND_TRUSTED_HOPS' default have it: the target must be the server itself, not a proxy that
// adds its own entry), each feed batch with its site's key. As a real client does, a 429 is retried after its
// Retry-After (unless the client is one that ignores it), a request without an answer or with a 5xx after a
// growing back-off. The run stops with an error when the target fails every request for GiveUp, or refuses one
// site's batches for good (a wrong key, an unknown site), and when it ends having taken nothing.
func Send(ctx context.Context, l *Live, o SendOptions) (Stats, error) {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if o.Workers <= 0 {
		o.Workers = 8
	}
	if o.Every <= 0 {
		o.Every = 100 * time.Millisecond
	}
	if o.Backoff == nil {
		o.Backoff = backoff
	}
	if o.GiveUp <= 0 {
		o.GiveUp = 30 * time.Second
	}
	if o.Refusals <= 0 {
		o.Refusals = 5
	}
	if o.Log == nil {
		o.Log = quietLog()
	}
	target := strings.TrimSuffix(o.Target, "/")
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		return Stats{}, fmt.Errorf("target %q is no http(s) URL", o.Target)
	}
	if o.For > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.For)
		defer cancel()
	}
	start := time.Now()
	s := &sendState{st: Stats{Refused: map[int]int{}}, streams: map[string]*stream{}}
	jobs := make(chan *Request)
	var wg sync.WaitGroup
	clock := l.Clock()
	for range o.Workers {
		wg.Go(func() {
			for q := range jobs {
				code, wait, err := post(ctx, o.Client, target, q, clock)
				if err != nil && ctx.Err() != nil {
					return // the run ended under the request
				}
				s.answer(l, q, code, wait, err, o.Backoff)
			}
		})
	}
	t := time.NewTicker(o.Every)
	defer t.Stop()
	last := time.Now()
	var failed error
loop:
	for {
		for _, q := range l.Tick(time.Now()) {
			select {
			case jobs <- q:
			case <-ctx.Done():
				break loop
			}
		}
		s.mu.Lock()
		failed = s.verdict(o.GiveUp, o.Refusals)
		if time.Since(last) >= 10*time.Second {
			last = time.Now()
			o.Log.Info("Live traffic", "world_time", l.WorldTime(last).Format(time.DateTime), "events", s.st.Events,
				"requests", s.st.Requests, "refused", s.st.Refused, "failed", s.st.Failed)
		}
		s.mu.Unlock()
		if failed != nil {
			break
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			break loop
		}
	}
	close(jobs)
	wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Elapsed = time.Since(start)
	if failed == nil && s.st.Events == 0 && (s.st.Failed > 0 || len(s.st.Refused) > 0) {
		failed = fmt.Errorf("the target took nothing: %s", s.why())
	}
	return s.st, failed
}

// answer books one answer and queues the retry it calls for.
func (s *sendState) answer(l *Live, q *Request, code int, wait time.Duration, err error, back func(int) time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q.Tries++
	s.st.Requests++
	key := q.Site.Domain + " " + q.Path()
	sm := s.streams[key]
	if sm == nil {
		sm = &stream{refused: map[int]int{}}
		s.streams[key] = sm
	}
	switch {
	case err != nil || code >= 500:
		s.st.Failed++
		if err != nil {
			s.lastErr = err.Error()
		} else {
			s.st.Refused[code]++
			s.lastErr = fmt.Sprintf("%s answered %d", q.Path(), code)
		}
		if s.failingSince.IsZero() {
			s.failingSince = time.Now()
		}
		if l.Retry(q, back(q.Tries)) {
			s.st.Retried++
		}
		return
	case code == http.StatusNoContent:
		s.st.Events += q.Events()
		sm.accepted++
	case code == http.StatusTooManyRequests:
		s.st.Refused[code]++
		if l.Retry(q, wait) {
			s.st.Retried++
		}
	default:
		s.st.Refused[code]++
		sm.refused[code]++
	}
	s.failingSince = time.Time{}
}

// verdict is the error that stops the run: the target failing everything for giveUp, or refusing one stream's
// batches for good, refusals of them with none taken.
func (s *sendState) verdict(giveUp time.Duration, refusals int) error {
	if !s.failingSince.IsZero() && time.Since(s.failingSince) >= giveUp {
		return fmt.Errorf("the target failed every request for %v, giving up: %s", giveUp.Round(time.Second), s.lastErr)
	}
	for key, sm := range s.streams {
		n := 0
		for code, c := range sm.refused {
			if permanent(code) {
				n += c
			}
		}
		if sm.accepted == 0 && n >= refusals {
			return fmt.Errorf("the target refuses every batch of %s: %s", key, refusalText(sm.refused))
		}
	}
	return nil
}

// why says what the target answered, for a run that delivered nothing.
func (s *sendState) why() string {
	if s.lastErr != "" {
		return s.lastErr
	}
	all := map[int]int{}
	for _, sm := range s.streams {
		for code, c := range sm.refused {
			all[code] += c
		}
	}
	return refusalText(all)
}

// permanent reports whether a refusal comes again whatever the wait: the batch, its key or its site is wrong.
func permanent(code int) bool {
	switch code {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusRequestEntityTooLarge:
		return true
	}
	return false
}

// refusalText names the refusals and what they mean.
func refusalText(refused map[int]int) string {
	var parts []string
	for _, c := range []struct {
		code int
		what string
	}{
		{http.StatusUnauthorized, "401: the feed key is wrong (-key, the sites file's feed_key_sha256)"},
		{http.StatusForbidden, "403: the site is unknown to the target (its sites file)"},
		{http.StatusNotFound, "404: no collector at this URL (-target)"},
		{http.StatusBadRequest, "400: the target takes no such batch"},
		{http.StatusRequestEntityTooLarge, "413: the batch is over the target's limits"},
	} {
		if n := refused[c.code]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d times %s", n, c.what))
		}
	}
	if len(parts) == 0 {
		return fmt.Sprint(refused)
	}
	return strings.Join(parts, "; ")
}

// post sends one request and returns the status and, for a 429, the wait it names.
func post(ctx context.Context, c *http.Client, target string, q *Request, clock Clock) (int, time.Duration, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, target+q.Path(), bytes.NewReader(q.Body(clock)))
	if err != nil {
		return 0, 0, err
	}
	r.Header = q.Header()
	if !q.Feed {
		r.Header.Set("X-Forwarded-For", q.Addr().String())
	}
	res, err := c.Do(r)
	if err != nil {
		return 0, 0, err
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return res.StatusCode, retryAfter(res.Header.Get("Retry-After")), nil
}
