// Package simcli runs the real dashboard over a simulated history: it checks the scenarios' expectations against
// it, exports its answers as fixture files, and is the command line of server sim.
package simcli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"

	"github.com/trick77/stakeout/backend/internal/agg"
	"github.com/trick77/stakeout/backend/internal/dash"
	"github.com/trick77/stakeout/backend/internal/ingest"
	"github.com/trick77/stakeout/backend/internal/sim"
	"github.com/trick77/stakeout/backend/internal/sites"
	"github.com/trick77/stakeout/backend/internal/store"
)

// Dashboard is the dashboard API over st as of a minute after the world's anchor, its reference data the world's.
type Dashboard struct {
	srv *dash.Server
	mux *http.ServeMux
}

// NewDashboard serves the dashboard over st.
func NewDashboard(ctx context.Context, st *store.Store, w *sim.World) (*Dashboard, error) {
	return newDashboard(ctx, st, w, w.Anchor.Add(time.Minute), dash.SourceAuto)
}

// newDashboard serves the dashboard over st as of now, its figures from src.
func newDashboard(ctx context.Context, st *store.Store, w *sim.World, now time.Time, src dash.Source) (*Dashboard,
	error) {
	rows, err := st.Sites(ctx)
	if err != nil {
		return nil, err
	}
	reg, err := sites.New(rows)
	if err != nil {
		return nil, err
	}
	clock := func() time.Time { return now }
	counters := ingest.NewCounters(clock)
	for _, s := range w.Sites {
		// The sources delivered as the history did: the last hour's state of a live collector.
		if site, ok := reg.Lookup(s.Domain); ok && !s.Silent {
			if s.Script {
				counters.Seen(site.ID, "browser")
			}
			if s.Feed && w.FeedDelivers(s, w.Anchor) {
				counters.Seen(site.ID, "server")
			}
		}
	}
	srv := &dash.Server{DB: st, Sites: reg, Ref: w.Ref.Data, Counters: counters, RetentionMonths: 25,
		Build: "sim", Now: clock, Source: src}
	mux := http.NewServeMux()
	mux.Handle("GET /api/dash/{endpoint}", srv)
	return &Dashboard{srv: srv, mux: mux}, nil
}

// Get asks an endpoint and returns the status and the answer's bytes.
func (d *Dashboard) Get(ctx context.Context, endpoint string, params map[string]string) (int, []byte) {
	v := url.Values{}
	for k, x := range params {
		v.Set(k, x)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/dash/"+endpoint+"?"+v.Encode(), nil)
	d.mux.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

// Failure is an expectation the dashboard did not meet, reading its figures from Source.
type Failure struct {
	Expectation sim.Expectation
	Source      string
	Err         error
}

func (f Failure) Error() string {
	return fmt.Sprintf("%s: %s (%s %v, %s): %v", f.Expectation.Scenario, f.Expectation.Says,
		f.Expectation.Endpoint, f.Expectation.Params, f.Source, f.Err)
}

// checkSources are the sources Check asks every expectation from.
var checkSources = []struct {
	name string
	src  dash.Source
}{{"raw", dash.SourceRaw}, {"aggregates", dash.SourceAgg}}

// rawOnly: 24h begins and ends inside an hour, so it is read raw under every source, but for Noteworthy, whose 24h is
// whole hours (agg_hour's, its lists raw): asked raw only.
func rawOnly(e sim.Expectation) bool {
	return e.Params["range"] == "24h" && e.Endpoint != "noteworthy"
}

// buildMu serializes the aggregate builds: tests check one stored history from several goroutines, and a second
// build of a store finds nothing left to build.
var buildMu sync.Mutex

// buildAggregates builds every day of the world's history up to its anchor's (the history ends there), as the job
// would have once each day was due.
func buildAggregates(ctx context.Context, st *store.Store, w *sim.World) error {
	buildMu.Lock()
	defer buildMu.Unlock()
	_, err := agg.BuildThrough(ctx, st, w.Anchor, agg.TopK)
	return err
}

// Check builds the aggregates, asks the dashboard for every expectation of the world's scenarios from raw events and
// again from the aggregates (raw only where they have no answer), and returns those it did not meet.
func Check(ctx context.Context, st *store.Store, w *sim.World) ([]Failure, error) {
	if err := buildAggregates(ctx, st, w); err != nil {
		return nil, err
	}
	var out []Failure
	for _, s := range checkSources {
		d, err := newDashboard(ctx, st, w, w.Anchor.Add(time.Minute), s.src)
		if err != nil {
			return nil, err
		}
		var exps []sim.Expectation
		for _, e := range w.Expectations() {
			if s.src == dash.SourceRaw || !rawOnly(e) {
				exps = append(exps, e)
			}
		}
		out = append(out, check(ctx, d, s.name, exps)...)
	}
	return out, nil
}

// check asks d for the expectations and returns those it did not meet. A request several expectations make is
// asked once: the answers are the store's at a fixed time, and Noteworthy, which most scenarios ask the same way,
// computes every finding of a range at each request. The requests are asked at once, a few at a time: the dashboard
// serves them side by side as it does its viewers.
func check(ctx context.Context, d *Dashboard, source string, exps []sim.Expectation) []Failure {
	type answer struct {
		code int
		body []byte
	}
	keys := make([]string, len(exps))
	asked := map[string]*answer{}
	var wg sync.WaitGroup
	slots := make(chan struct{}, checkWorkers)
	for i, e := range exps {
		v := url.Values{}
		for k, x := range e.Params {
			v.Set(k, x)
		}
		keys[i] = e.Endpoint + "?" + v.Encode()
		if asked[keys[i]] != nil {
			continue
		}
		a := &answer{}
		asked[keys[i]] = a
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			a.code, a.body = d.Get(ctx, e.Endpoint, e.Params)
		})
	}
	wg.Wait()
	var out []Failure
	for i, e := range exps {
		code, b := asked[keys[i]].code, asked[keys[i]].body
		var body map[string]any
		switch {
		case code != http.StatusOK:
			out = append(out, Failure{e, source, fmt.Errorf("status %d: %s", code, b)})
		case json.Unmarshal(b, &body) != nil:
			out = append(out, Failure{e, source, fmt.Errorf("not JSON: %s", b)})
		default:
			if err := e.Check(body); err != nil {
				out = append(out, Failure{e, source, err})
			}
		}
	}
	return out
}

// checkWorkers is how many of Check's requests run at once.
const checkWorkers = 4
