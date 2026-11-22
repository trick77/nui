package sim

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/nui/backend/internal/ingest"
	"github.com/trick77/nui/backend/internal/sites"
	"github.com/trick77/nui/backend/internal/store"
)

func TestKnobs(t *testing.T) {
	var warned []string
	warn := func(s string) { warned = append(warned, s) }
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if k := ReadKnobs(env(nil), warn); k != DefaultKnobs() || len(warned) != 0 {
		t.Errorf("defaults %+v, warned %v", k, warned)
	}
	k := ReadKnobs(env(map[string]string{"NUI_SIM_SEED": "42", "NUI_SIM_SCALE": "2.5", "NUI_SIM_SPEED": "60",
		"NUI_SIM_PORT": "9091", "NUI_SIM_DAYS": "7", "NUI_SIM_SCENARIOS": "hammer, nope", "NUI_SIM_START": "2026-11-22T13:30:00Z",
		"NUI_SIM_CLASSIFY": "false", "NUI_SIM_FEED_OUTAGE": "15", "NUI_SIM_FLOOD": "48", "NUI_SIM_LATE_FEED": "never",
		"NUI_SIM_SLOW_MS": "800", "NUI_SIM_EMPTY_SITE": "1"}), warn)
	if k.Seed != 42 || k.Scale != 2.5 || k.Speed != 60 || k.Port != 9091 || k.Days != 7 || k.Scenarios != "hammer" ||
		!k.Start.Equal(anchor) || k.Classify || k.FeedOutage != 15*time.Minute || k.Flood != "48" || k.LateFeed != LateFeedNever ||
		k.SlowMS != 800 || !k.EmptySite || len(warned) != 1 || !strings.Contains(warned[0], "nope") {
		t.Errorf("set %+v, warned %v", k, warned)
	}
	warned = nil
	k = ReadKnobs(env(map[string]string{"NUI_SIM_SEED": "x", "NUI_SIM_SCALE": "-1", "NUI_SIM_SPEED": "NaN",
		"NUI_SIM_PORT": "70000", "NUI_SIM_DAYS": "1.5", "NUI_SIM_START": "yesterday", "NUI_SIM_CLASSIFY": "maybe",
		"NUI_SIM_FLOOD": "huge", "NUI_SIM_LATE_FEED": "soon"}), warn)
	if d := DefaultKnobs(); k.Seed != d.Seed || k.Scale != d.Scale || k.Speed != d.Speed || k.Port != d.Port ||
		k.Days != d.Days || !k.Start.IsZero() || !k.Classify || k.Flood != "" || k.LateFeed != LateFeedOff || len(warned) != 9 {
		t.Errorf("bad values %+v, warned %d: %v", k, len(warned), warned)
	}
	// The variables' ranges are the flags' too: Validate refuses what ReadKnobs would have ignored.
	if err := DefaultKnobs().Validate(); err != nil {
		t.Errorf("defaults invalid: %v", err)
	}
	for _, f := range []func(*Knobs){
		func(k *Knobs) { k.Scale = math.NaN() }, func(k *Knobs) { k.Scale = math.Inf(1) }, func(k *Knobs) { k.Scale = 0 },
		func(k *Knobs) { k.Scale = 1e9 }, func(k *Knobs) { k.Speed = 0 }, func(k *Knobs) { k.Speed = math.NaN() },
		func(k *Knobs) { k.Speed = math.Inf(1) }, func(k *Knobs) { k.Speed = -1 }, func(k *Knobs) { k.Days = -1 },
		func(k *Knobs) { k.Days = 100_000 }, func(k *Knobs) { k.Port = 0 }, func(k *Knobs) { k.SlowMS = -1 },
		func(k *Knobs) { k.FeedOutage = -time.Minute }, func(k *Knobs) { k.Flood = "huge" },
		func(k *Knobs) { k.Scenarios = "nope" },
	} {
		k := DefaultKnobs()
		f(&k)
		if k.Validate() == nil {
			t.Errorf("accepted %+v", k)
		}
	}
	// Nothing out of range reaches a world or a live run.
	if _, err := NewWorld(Options{Scale: math.NaN(), Ref: &Reference{}}); err == nil {
		t.Error("a NaN scale made a world")
	}
	if r := newRng(1); r.poisson(math.NaN()) != 0 || r.poisson(math.Inf(1)) != 0 {
		t.Error("poisson of no number")
	}
	// The awkward behaviours become live windows; an empty site joins the default ones.
	k = ReadKnobs(env(map[string]string{"NUI_SIM_FEED_OUTAGE": "15", "NUI_SIM_FLOOD": "addr", "NUI_SIM_LATE_FEED": "5",
		"NUI_SIM_EMPTY_SITE": "true"}), warn)
	ref, err := OpenReference(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := k.World(ref, anchor)
	if err != nil {
		t.Fatal(err)
	}
	lens, _ := w.Site("lens.example.com")
	blog, _ := w.Site("blog.example.org")
	if w.FeedDelivers(lens, anchor.Add(5*time.Minute)) || !w.FeedDelivers(lens, anchor.Add(20*time.Minute)) ||
		w.FeedDelivers(blog, anchor.Add(time.Minute)) || !w.FeedDelivers(blog, anchor.Add(6*time.Minute)) ||
		!w.in("flood-addr", anchor.Add(2*time.Minute)) || len(w.Sites) != 5 {
		t.Errorf("live windows wrong: %+v", w.Scenarios)
	}
	// A late feed of 0 minutes starts at the start, not never; "never" never.
	for raw, delivers := range map[string]bool{"0": true, "never": false} {
		k = ReadKnobs(env(map[string]string{"NUI_SIM_LATE_FEED": raw}), warn)
		if w, err = k.World(ref, anchor); err != nil {
			t.Fatal(err)
		}
		blog, _ = w.Site("blog.example.org")
		if w.FeedDelivers(blog, anchor.Add(-time.Minute)) || w.FeedDelivers(blog, anchor.Add(time.Minute)) != delivers ||
			k.Validate() != nil {
			t.Errorf("NUI_SIM_LATE_FEED=%s: %v, delivering after the start %v", raw, k.LateFeed, !delivers)
		}
	}
}

// A rate means what it says: the scale it sets sends that many events a second of wall time over the live week, the
// actors that do not scale included; one out of reach says so.
func TestScaleFor(t *testing.T) {
	ref, err := OpenReference(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ rate, speed float64 }{{0.05, 1}, {2, 60}, {50, 1200}, {1, 60}}
	if !raceOn {
		cases = append(cases, struct{ rate, speed float64 }{20, 60})
	}
	for _, c := range cases {
		k := DefaultKnobs()
		k.Speed = c.speed
		s, err := k.ScaleFor(ref, anchor, c.rate)
		if err != nil {
			t.Fatalf("rate %v at speed %v: %v", c.rate, c.speed, err)
		}
		k.Scale = s
		w, err := k.World(ref, anchor)
		if err != nil {
			t.Fatal(err)
		}
		got := float64(w.LiveEvents()) / (liveWeek.Seconds() / c.speed)
		if math.Abs(got-c.rate) > 0.1*c.rate {
			t.Errorf("rate %v at speed %v: scale %.3g sends %.3g events a second", c.rate, c.speed, s, got)
		}
	}
	k := DefaultKnobs()
	k.Speed = 3600
	if _, err := k.ScaleFor(ref, anchor, 0.01); err == nil || !strings.Contains(err.Error(), "fixed actors") {
		t.Errorf("a rate below the fixed actors': %v", err)
	}
	k.Speed = 1
	if _, err := k.ScaleFor(ref, anchor, 100); err == nil || !strings.Contains(err.Error(), "-speed") {
		t.Errorf("a rate beyond the largest scale: %v", err)
	}
}

func TestLiveTick(t *testing.T) {
	w := testWorld(t, 1, "baseline")
	start := time.Now()
	l := NewLive(w, start, 60)
	if got := l.Tick(start); len(got) != 0 {
		t.Errorf("at the start: %d requests", len(got))
	}
	// A minute of wall time is an hour of the world's, handed out once.
	a := l.Tick(start.Add(30 * time.Second))
	b := l.Tick(start.Add(time.Minute))
	all := w.Requests(anchor, anchor.Add(time.Hour))
	if len(a)+len(b) != len(all) || len(a) == 0 || len(b) == 0 {
		t.Errorf("%d + %d requests, the hour has %d", len(a), len(b), len(all))
	}
	if !l.WorldTime(start.Add(time.Minute)).Equal(anchor.Add(time.Hour)) ||
		l.Clock()(anchor.Add(time.Hour)) != start.Add(time.Minute).UnixMilli() {
		t.Error("world time and wall time do not map")
	}
	// After an idle spell the world skips ahead: only the last MaxGap's traffic, at most MaxSpan of it.
	c := l.Tick(start.Add(time.Hour))
	if want := w.Requests(l.WorldTime(start.Add(time.Hour)).Add(-time.Hour), l.WorldTime(start.Add(time.Hour))); len(c) != len(want) {
		t.Errorf("after idling %d requests, want the gap's %d", len(c), len(want))
	}
	// A refused request comes again after its wait; a hammer's never.
	q := c[0]
	if !l.Retry(q, time.Second) || len(l.Tick(start.Add(time.Hour+2*time.Second))) == 0 {
		t.Error("retry not due")
	}
	if l.Retry(&Request{batch: &batch{hammer: true}}, time.Second) {
		t.Error("a hammer retried")
	}
	for _, s := range []float64{0, -1, math.NaN(), math.Inf(1), 1e12} {
		if NewLive(w, start, s).Speed != 1 {
			t.Errorf("speed %v not defaulted", s)
		}
	}
}

// At the largest speed one tick stays bounded: an hour of world time after any idle spell, a delivery cut at its
// time budget, and world time never overflowing however long the run.
func TestLiveBounded(t *testing.T) {
	w := testWorld(t, 2, "baseline")
	start := time.Now()
	l := NewLive(w, start, SpeedLimit.Max())
	l.Tick(start.Add(time.Second))
	reqs := l.Tick(start.Add(10 * time.Minute)) // ten idle minutes: 10 days of the world
	to := l.WorldTime(start.Add(10 * time.Minute))
	if len(reqs) == 0 {
		t.Fatal("nothing after the idle spell")
	}
	for _, q := range reqs {
		if q.At.Before(to.Add(-l.MaxSpan)) || q.At.After(to) {
			t.Fatalf("a request at %v, the tick spans %v to %v", q.At, to.Add(-l.MaxSpan), to)
		}
	}
	if len(w.plans) > maxPlans {
		t.Errorf("%d day plans held", len(w.plans))
	}
	// A delivery stops at its budget and counts what it left.
	slow := http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Millisecond)
		rw.WriteHeader(http.StatusNoContent)
	})
	l.Budget = 20 * time.Millisecond
	began := time.Now()
	s := l.Deliver(t.Context(), Handlers{Browser: slow, Feed: slow}, reqs)
	if took := time.Since(began); took > 200*time.Millisecond || s.Skipped == 0 || s.Requests+s.Skipped != len(reqs) {
		t.Errorf("delivery of %d took %v: %v", len(reqs), took, s)
	}
	// A month of wall time at the largest speed is beyond a time.Duration; world time still only moves forward.
	prev := l.WorldTime(start)
	for d := range 400 {
		at := l.WorldTime(start.Add(time.Duration(d) * day))
		if at.Before(prev) || at.Sub(w.Anchor) > maxWorld {
			t.Fatalf("day %d of wall time: world time %v after %v", d, at, prev)
		}
		prev = at
	}
	if l.Clock()(prev) < start.UnixMilli() {
		t.Error("the clock maps the far world time before the start")
	}
}

// collector is an in-process collector on the real clock, its writer running.
func collector(t *testing.T, w *World) (*store.Store, *ingest.Collector) {
	t.Helper()
	st := testStore(t, w)
	rows, err := st.Sites(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	reg, err := sites.New(rows)
	if err != nil {
		t.Fatal(err)
	}
	counters := ingest.NewCounters(nil)
	c := &ingest.Collector{Sites: reg, Counters: counters, Writer: ingest.NewWriter(st, counters, 0, nil, quietLog()),
		Browser: ingest.BrowserTiers(1, nil), Feed: ingest.FeedLimiter(nil), TrustedHops: 1}
	ctx, cancel := context.WithCancel(context.Background())
	go c.Writer.Run(ctx)
	t.Cleanup(func() { cancel(); <-c.Writer.Done() })
	return st, c
}

// Live traffic over HTTP: accepted, the hammer refused and never retried, everyone else within the limits.
func TestSend(t *testing.T) {
	w := testWorld(t, 1, "hammer")
	w.Scenarios[1].From, w.Scenarios[1].To = 0, 10*time.Minute
	st, c := collector(t, w)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/in", c.HandleBrowser)
	mux.HandleFunc("POST /api/feed", c.HandleFeed)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	l := NewLive(w, time.Now(), 1200) // 20 minutes of the world a second
	s, err := Send(t.Context(), l, SendOptions{Target: srv.URL, For: 1500 * time.Millisecond, Every: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if s.Events == 0 || s.Refused[http.StatusTooManyRequests] == 0 || len(s.Refused) != 1 {
		t.Errorf("stats %v", s)
	}
	time.Sleep(400 * time.Millisecond) // the writer's beat
	var hammer, people int
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE ip = ?`, HammerAddr.String()).Scan(&hammer)
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE source = 'browser' AND ip != ?`, HammerAddr.String()).Scan(&people)
	if hammer == 0 || people == 0 || hammer > 20*80 {
		t.Errorf("hammer events %d, people's %d", hammer, people)
	}
	if _, err := Send(t.Context(), l, SendOptions{Target: "ftp://x"}); err == nil {
		t.Error("a non-HTTP target accepted")
	}
	if _, err := Send(t.Context(), l, SendOptions{Target: "http://127.0.0.1:1", For: time.Second}); err == nil {
		t.Error("an unreachable target is no error")
	}
}

// A 429 is honoured: the refused batch comes again, whole, no earlier than its Retry-After.
func TestSendBacksOff(t *testing.T) {
	w := testWorld(t, 1, "baseline")
	var mu sync.Mutex
	refused := map[string]time.Time{} // the first event of each refused batch → when
	var retried []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var b struct {
			Events []json.RawMessage `json:"events"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &b)
		key := r.URL.Path + string(b.Events[0])
		mu.Lock()
		defer mu.Unlock()
		if at, ok := refused[key]; ok {
			retried = append(retried, time.Since(at))
			rw.WriteHeader(http.StatusNoContent)
			return
		}
		if len(refused) < 3 {
			refused[key] = time.Now()
			rw.Header().Set("Retry-After", "1")
			rw.WriteHeader(http.StatusTooManyRequests)
			return
		}
		rw.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	s, err := Send(t.Context(), NewLive(w, time.Now(), 3600), SendOptions{Target: srv.URL, For: 2500 * time.Millisecond,
		Every: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if s.Retried != 3 || s.Refused[http.StatusTooManyRequests] != 3 || len(retried) != 3 {
		t.Fatalf("stats %v, retried %v", s, retried)
	}
	for _, d := range retried {
		if d < time.Second {
			t.Errorf("sent again after %v, Retry-After said 1 s", d)
		}
	}
}

// Live traffic into the server's own collector as the dashboard is asked, the answers slowed.
func TestWrap(t *testing.T) {
	w := testWorld(t, 1, "baseline")
	st, c := collector(t, w)
	l := NewLive(w, time.Now().Add(-time.Second), 3600)
	dash := l.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }),
		Handlers{Browser: http.HandlerFunc(c.HandleBrowser), Feed: http.HandlerFunc(c.HandleFeed)}, 10*time.Millisecond)
	rec := httptest.NewRecorder()
	start := time.Now()
	dash.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/dash/overview", nil))
	if rec.Code != 204 || time.Since(start) < 10*time.Millisecond {
		t.Errorf("answer %d after %v", rec.Code, time.Since(start))
	}
	time.Sleep(400 * time.Millisecond)
	var n int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM event`).Scan(&n); err != nil || n == 0 {
		t.Errorf("events after a dashboard request: %d, %v", n, err)
	}
}
