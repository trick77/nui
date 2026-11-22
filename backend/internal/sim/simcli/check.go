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
	"time"

	"github.com/trick77/nui/backend/internal/dash"
	"github.com/trick77/nui/backend/internal/ingest"
	"github.com/trick77/nui/backend/internal/sim"
	"github.com/trick77/nui/backend/internal/sites"
	"github.com/trick77/nui/backend/internal/store"
)

// Dashboard is the dashboard API over st as of a minute after the world's anchor, its reference data the world's.
type Dashboard struct {
	srv *dash.Server
	mux *http.ServeMux
}

// NewDashboard serves the dashboard over st.
func NewDashboard(ctx context.Context, st *store.Store, w *sim.World) (*Dashboard, error) {
	rows, err := st.Sites(ctx)
	if err != nil {
		return nil, err
	}
	reg, err := sites.New(rows)
	if err != nil {
		return nil, err
	}
	now := w.Anchor.Add(time.Minute)
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
		Build: "sim", Now: clock}
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

// Failure is an expectation the dashboard did not meet.
type Failure struct {
	Expectation sim.Expectation
	Err         error
}

func (f Failure) Error() string {
	return fmt.Sprintf("%s: %s (%s %v): %v", f.Expectation.Scenario, f.Expectation.Says, f.Expectation.Endpoint,
		f.Expectation.Params, f.Err)
}

// Check asks the dashboard for every expectation of the world's scenarios and returns those it did not meet.
func Check(ctx context.Context, st *store.Store, w *sim.World) ([]Failure, error) {
	d, err := NewDashboard(ctx, st, w)
	if err != nil {
		return nil, err
	}
	var out []Failure
	for _, e := range w.Expectations() {
		code, b := d.Get(ctx, e.Endpoint, e.Params)
		var body map[string]any
		switch {
		case code != http.StatusOK:
			out = append(out, Failure{e, fmt.Errorf("status %d: %s", code, b)})
		case json.Unmarshal(b, &body) != nil:
			out = append(out, Failure{e, fmt.Errorf("not JSON: %s", b)})
		default:
			if err := e.Check(body); err != nil {
				out = append(out, Failure{e, err})
			}
		}
	}
	return out, nil
}
