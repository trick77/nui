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
	Log   *slog.Logger
}

// Send runs l against a collector over HTTP: every beat it takes the requests due and posts them, each browser
// batch from its client's address (the one X-Forwarded-For entry, which the server reads with one trusted hop, as
// server -sim and BACKEND_TRUSTED_HOPS' default have it: the target must be the server itself, not a proxy that
// adds its own entry), each feed batch with its site's key. A 429 is retried after its Retry-After, as the tracker
// and a well-behaved emitter do, unless the client is one that ignores it.
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
	st := Stats{Refused: map[int]int{}}
	var mu sync.Mutex
	var failed error
	jobs := make(chan *Request)
	var wg sync.WaitGroup
	clock := l.Clock()
	for range o.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for q := range jobs {
				code, wait, err := post(ctx, o.Client, target, q, clock)
				mu.Lock()
				q.Tries++
				st.Requests++
				switch {
				case err != nil:
					if ctx.Err() == nil && failed == nil {
						failed = err
					}
				case code == http.StatusNoContent:
					st.Events += q.Events()
				default:
					st.Refused[code]++
					if code == http.StatusTooManyRequests && l.Retry(q, wait) {
						st.Retried++
					}
				}
				mu.Unlock()
			}
		}()
	}
	t := time.NewTicker(o.Every)
	defer t.Stop()
	last := time.Now()
loop:
	for {
		for _, q := range l.Tick(time.Now()) {
			select {
			case jobs <- q:
			case <-ctx.Done():
				break loop
			}
		}
		mu.Lock()
		err := failed
		if time.Since(last) >= 10*time.Second {
			last = time.Now()
			o.Log.Info("Live traffic", "world_time", l.WorldTime(last).Format(time.DateTime), "events", st.Events,
				"requests", st.Requests, "refused", st.Refused)
		}
		mu.Unlock()
		if err != nil {
			close(jobs)
			wg.Wait()
			return st, err
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			break loop
		}
	}
	close(jobs)
	wg.Wait()
	st.Elapsed = time.Since(start)
	return st, nil
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
