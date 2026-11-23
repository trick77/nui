package dash

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trick77/stakeout/backend/internal/agg"
	"github.com/trick77/stakeout/backend/internal/sites"
	"github.com/trick77/stakeout/backend/internal/store"
)

const apiSite = "gateway.example.com"

// call is one server request of the API test site.
type call struct {
	ago           time.Duration
	tmpl, op, err string
	status        int
	dur           int64
	ip, account   string
	unrouted      bool
}

// ua is the client the request came with: two builds of one program by its account.
func (c call) ua() string {
	if c.account == "a2" {
		return "lens-cli/2.0 (darwin)"
	}
	return "lens-cli/1.4 (" + c.op + ")"
}

// apiRig is a dashboard over one API site with the given requests, its closed days built.
func apiRig(t *testing.T, calls []call) *rig {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rows, err := st.SyncSites(t.Context(), []store.Site{
		{Domain: feedSite, Kind: "site", FeedKeyHash: sites.HashKey("k"), PathRules: "[]"},
		{Domain: plainSite, Kind: "site", PathRules: "[]"},
		{Domain: apiSite, Kind: "api", FeedKeyHash: sites.HashKey("a"), PathRules: "[]"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var evs []store.Event
	for _, c := range calls {
		evs = append(evs, store.Event{SiteID: rows[apiSite].ID, TS: rigEnd.Add(-c.ago).UnixMilli(), Source: "server",
			Kind: "request", IP: c.ip, IPKey: c.ip, UA: c.ua(), Path: "/mcp", Template: c.tmpl, Method: "POST",
			Status: c.status, DurMS: c.dur, Account: c.account, Op: c.op, Err: c.err, Unrouted: c.unrouted, Cls: 5})
	}
	if err := st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	if _, err := agg.BuildThrough(t.Context(), st, rigEnd, agg.TopK); err != nil {
		t.Fatal(err)
	}
	return rigOver(t, st, nil, nil)
}

const day1 = 24 * time.Hour

// mcpCalls is a week of calls: failures answered 200, a refusal, and what is no call (a probe, an account's 404, a
// refusal before any route, a client asking for a stream).
func mcpCalls() []call {
	var out []call
	for d := range 6 {
		ago := time.Duration(d)*day1 + time.Hour
		for i := range 20 {
			out = append(out, call{ago: ago + time.Duration(i)*time.Minute, tmpl: "POST /mcp", op: "whois", status: 200,
				dur: int64(10 + i), ip: fmt.Sprintf("192.0.2.%d", 1+i%3), account: "a1"})
		}
		out = append(out,
			call{ago: ago, tmpl: "POST /mcp", op: "whois", status: 200, err: "invalid_params", dur: 2, ip: "192.0.2.1", account: "a1"},
			call{ago: ago, tmpl: "POST /mcp", op: "search", status: 200, dur: 900, ip: "192.0.2.9", account: "a2"},
			call{ago: ago, tmpl: "POST /mcp", op: "search", status: 429, dur: 1, ip: "192.0.2.9", account: "a2"},
			call{ago: ago, tmpl: "POST /mcp", status: 401, dur: 1, ip: "198.51.100.7"},
			call{ago: ago, tmpl: "GET /.env", status: 404, dur: 1, ip: "198.51.100.8", unrouted: true},
			call{ago: ago, tmpl: "GET /x", status: 404, dur: 1, ip: "192.0.2.9", account: "a2"},
			call{ago: ago, tmpl: "GET /admin", status: 401, dur: 1, ip: "198.51.100.8", unrouted: true},
			call{ago: ago, tmpl: "GET /mcp", err: "method_not_allowed", status: 405, dur: 1, ip: "192.0.2.9", unrouted: true})
	}
	return out
}

func rowsByKey(body map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, r := range listRows(body) {
		out[r["key"].(string)] = r
	}
	return out
}

// Every figure the same from raw and from the aggregates, and what it should be.
func TestAPIFigures(t *testing.T) {
	t.Parallel()
	r := apiRig(t, mcpCalls())
	for _, src := range []Source{SourceRaw, SourceAuto} {
		r.s.Source = src
		for _, rng := range []string{"7d", "30d"} {
			code, body := r.get(t, "overview", p(apiSite, "range", rng, "tz", "UTC"))
			if code != 200 {
				t.Fatalf("overview %d %v", code, body)
			}
			got := tiles(t, body)
			if len(got) != 5 {
				t.Fatalf("tiles %v", got)
			}
			// 24 calls a day: 20 and 1 whois, 2 search, one without a key; the 404 is none.
			for k, want := range map[string]float64{"calls": 144, "accounts": 2, "callers": 5, "failed": 12.5} {
				if v := num64(got[k]["value"]); v != want {
					t.Errorf("%v %s %s = %v, want %v", src, rng, k, v, want)
				}
			}
			if v := num64(got["p95"]["value"]); v < 27 || v > 30 {
				t.Errorf("%v %s p95 %v", src, rng, v)
			}
			_, body = r.get(t, "list", p(apiSite, "range", rng, "tz", "UTC", "dim", "endpoints", "limit", "50"))
			eps := rowsByKey(body)
			if len(eps) != 3 {
				t.Fatalf("endpoints %v", eps)
			}
			w, se, bare := eps["POST /mcp whois"], eps["POST /mcp search"], eps["POST /mcp"]
			if num64(w["value"]) != 126 || num64(w["second"]) != 3 || w["sub"] != "whois" || num64(w["failed"]) != 4.8 ||
				num64(w["p50"]) < 17 || num64(w["p50"]) > 20 {
				t.Errorf("%v whois %v", src, w)
			}
			if num64(se["value"]) != 12 || num64(se["failed"]) != 50 || num64(se["p95"]) < 855 || num64(se["p95"]) > 945 {
				t.Errorf("%v search %v", src, se)
			}
			if num64(bare["value"]) != 6 || num64(bare["failed"]) != 100 {
				t.Errorf("%v bare %v", src, bare)
			}
			_, body = r.get(t, "list", p(apiSite, "range", rng, "tz", "UTC", "dim", "accounts"))
			acc := rowsByKey(body)
			if len(acc) != 2 || num64(acc["a1"]["value"]) != 126 || num64(acc["a2"]["failed"]) != 50 || acc["a1"]["p50"] != nil {
				t.Errorf("%v accounts %v", src, acc)
			}
			// A client by its product, whatever else its user agent says.
			_, body = r.get(t, "list", p(apiSite, "range", rng, "tz", "UTC", "dim", "agents"))
			ag := rowsByKey(body)
			if len(ag) != 2 || num64(ag["lens-cli/1.4"]["value"]) != 132 || num64(ag["lens-cli/1.4"]["second"]) != 4 ||
				num64(ag["lens-cli/2.0"]["failed"]) != 50 {
				t.Errorf("%v agents %v", src, ag)
			}
			_, body = r.get(t, "list", p(apiSite, "range", rng, "tz", "UTC", "dim", "failures"))
			fl := rowsByKey(body)
			if len(fl) != 3 || num64(fl["invalid_params"]["value"]) != 6 || num64(fl["HTTP 429"]["value"]) != 6 ||
				num64(fl["HTTP 401"]["value"]) != 6 || fl["invalid_params"]["failed"] != nil {
				t.Errorf("%v failures %v", src, fl)
			}
		}
	}
}

// A day an older builder made holds none of these figures: it is read raw, never as nothing.
func TestAPIOlderBuild(t *testing.T) {
	t.Parallel()
	r := apiRig(t, mcpCalls())
	for _, q := range []string{`UPDATE agg_day SET builder = 2`, `DELETE FROM agg_list WHERE dim = 'endpoints'`,
		`UPDATE agg_hour SET failed = 0, durations = NULL, callers = NULL, accounts = NULL`} {
		if _, err := r.s.DB.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	r.s.Source = SourceAuto
	_, body := r.get(t, "overview", p(apiSite, "range", "30d", "tz", "UTC"))
	if v := num64(tiles(t, body)["calls"]["value"]); v != 144 {
		t.Errorf("calls %v over days an older builder made", v)
	}
	_, body = r.get(t, "list", p(apiSite, "range", "30d", "tz", "UTC", "dim", "endpoints"))
	if n := len(listRows(body)); n != 3 {
		t.Errorf("%d endpoints over days an older builder made", n)
	}
}

// A website keeps its tiles, whether or not it has a feed; without one it has no endpoints.
func TestAPIOnlyForAPISites(t *testing.T) {
	t.Parallel()
	r := apiRig(t, mcpCalls())
	_, body := r.get(t, "overview", p(feedSite))
	if got := tiles(t, body); got["visitors"] == nil || got["calls"] != nil {
		t.Errorf("website tiles %v", got)
	}
	if code, _ := r.get(t, "list", p(plainSite, "dim", "endpoints")); code != 404 {
		t.Errorf("endpoints without a feed: %d", code)
	}
}

// The log answers what a click on a row asks: its count is the row's.
func TestAPILog(t *testing.T) {
	t.Parallel()
	r := apiRig(t, mcpCalls())
	for _, tc := range []struct {
		k, v string
		want float64
	}{{"endpoint", "POST /mcp whois", 126}, {"endpoint", "POST /mcp", 6}, {"account", "a2", 18}, {"op", "search", 12},
		{"failure", "invalid_params", 6}, {"failure", "HTTP 429", 6}, {"failure", "nope", 0},
		{"endpoint", "x' OR '1'='1", 0}} {
		code, body := r.get(t, "log", p(apiSite, "range", "7d", "tz", "UTC", tc.k, tc.v))
		if code != 200 || num64(body["total"]) != tc.want {
			t.Errorf("%s=%s: %d, total %v, want %v", tc.k, tc.v, code, body["total"], tc.want)
		}
	}
	// An account's row counts its calls, and opens them.
	_, body := r.get(t, "log", p(apiSite, "range", "7d", "tz", "UTC", "account", "a2", "calls", "1"))
	if num64(body["total"]) != 12 {
		t.Errorf("a2's calls: %v", body["total"])
	}
	_, body = r.get(t, "log", p(apiSite, "range", "7d", "tz", "UTC", "failure", "invalid_params"))
	row := listRows(body)[0]
	if row["what"] != "POST /mcp whois → 200, invalid_params" || row["account"] != "a1" || num64(row["dur_ms"]) != 2 {
		t.Errorf("log row %v", row)
	}
}

// Noteworthy speaks of calls: a new endpoint, a new reason to fail, a failed share that moved; never of visitors.
func TestAPINoteworthy(t *testing.T) {
	t.Parallel()
	var calls []call
	for d := range 14 {
		for i := range 30 {
			c := call{ago: time.Duration(d)*day1 + time.Duration(i)*time.Minute, tmpl: "POST /mcp", op: "whois", status: 200,
				dur: 5, ip: "192.0.2.1", account: "a1"}
			if d > 0 && d < 7 && i < 10 {
				c.op, c.err = "search", "upstream_error"
			}
			calls = append(calls, c)
			if d > 0 && d < 7 && i < 5 { // somebody tries a path
				calls = append(calls, call{ago: c.ago, tmpl: "POST /admin", status: 401, dur: 1, ip: "198.51.100.9"})
			}
			if d == 0 && i < 20 {
				calls = append(calls, call{ago: c.ago + 14*day1, tmpl: "POST /mcp", op: "whois", status: 200, dur: 5, ip: "192.0.2.1"})
			}
		}
	}
	r := apiRig(t, calls)
	_, body := r.get(t, "noteworthy", p(apiSite, "range", "7d", "tz", "UTC", "findings", "all"))
	var ids []string
	for _, x := range body["items"].([]any) {
		ids = append(ids, x.(map[string]any)["id"].(string))
	}
	for _, want := range []string{"endpoint:POST /mcp search", "failure:upstream_error", "trend:failed:up"} {
		if !slices.Contains(ids, want) {
			t.Errorf("no finding %s in %v", want, ids)
		}
	}
	if slices.Contains(ids, "endpoint:POST /admin") {
		t.Error("a path that only ever was refused is a new endpoint")
	}
	for _, id := range ids {
		if strings.HasPrefix(id, "trend:visitors") || strings.HasPrefix(id, "trend:auto_share") || strings.HasPrefix(id, "ref:") {
			t.Errorf("a website's finding on an API: %s", id)
		}
	}
}

// A few calls say nothing of the failed share, and a falling one is no problem.
func TestAPINoteworthyFailedShare(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		n            int
		was, is      int // of n a day fail
		want, forbid string
		kind         string
	}{
		"few":  {1, 0, 1, "", "trend:failed:up", ""},
		"rise": {30, 0, 10, "trend:failed:up", "", "problem"},
		"fall": {30, 10, 0, "trend:failed:down", "", "automation"},
	} {
		var calls []call
		for d := range 14 {
			for i := range tc.n {
				c := call{ago: time.Duration(d)*day1 + time.Duration(i)*time.Minute, tmpl: "POST /mcp", op: "whois", status: 200,
					dur: 5, ip: "192.0.2.1"}
				if d < 7 && i < tc.is || d >= 7 && i < tc.was {
					c.err = "upstream_error"
				}
				calls = append(calls, c)
			}
		}
		_, body := apiRig(t, calls).get(t, "noteworthy", p(apiSite, "range", "7d", "tz", "UTC", "findings", "all"))
		kinds := map[string]string{}
		for _, x := range body["items"].([]any) {
			kinds[x.(map[string]any)["id"].(string)] = x.(map[string]any)["kind"].(string)
		}
		if tc.want != "" && kinds[tc.want] != tc.kind || tc.forbid != "" && kinds[tc.forbid] != "" {
			t.Errorf("%s: %v", name, kinds)
		}
	}
}

// No call took no time: no percentile, no median of one, no change against one.
func TestAPINoCalls(t *testing.T) {
	t.Parallel()
	r := apiRig(t, []call{{ago: 3 * day1, tmpl: "POST /mcp", status: 200, dur: 40, ip: "192.0.2.1"}})
	_, body := r.get(t, "overview", p(apiSite, "range", "7d", "tz", "UTC"))
	p95 := tiles(t, body)["p95"]
	if v := num64(p95["value"]); v < 38 || v > 42 || p95["median"] != p95["value"] || p95["prev"] != nil || p95["change"] != nil {
		t.Errorf("one call: %v", p95)
	}
	nulls := 0
	for _, x := range p95["series"].([]any) {
		if x == nil {
			nulls++
		}
	}
	if nulls != 6 {
		t.Errorf("series %v, want the six days without a call null", p95["series"])
	}
	_, body = r.get(t, "overview", p(apiSite, "range", "24h", "tz", "UTC"))
	if got := tiles(t, body); got["p95"]["value"] != nil || got["p95"]["change"] != nil || got["p95"]["median"] != nil ||
		got["failed"]["value"] != nil || got["failed"]["change"] != nil || got["failed"]["median"] != nil || num64(got["calls"]["value"]) != 0 {
		t.Errorf("no call: %v", got)
	}
}

// Days an older builder made whose raw is gone hold no such figures: they are missing, not quiet, and the answer says
// from when on there are some.
func TestAPIFiguresSince(t *testing.T) {
	t.Parallel()
	r := apiRig(t, mcpCalls())
	cut := rigEnd.Truncate(day1).Add(-3 * day1)
	for _, q := range []string{`UPDATE agg_day SET builder = 2, raw = 0 WHERE day < ?1`, `DELETE FROM event WHERE ts < ?1 * 86400000`,
		`UPDATE agg_hour SET failed = 0, durations = NULL, callers = NULL, accounts = NULL WHERE hour < ?1 * 24`} {
		if _, err := r.s.DB.DB.Exec(q, cut.UnixMilli()/dayMS); err != nil {
			t.Fatal(err)
		}
	}
	r.s.Source = SourceAuto
	_, body := r.get(t, "overview", p(apiSite, "range", "7d", "tz", "UTC"))
	if since := body["period"].(map[string]any)["since"]; since != cut.Format(time.RFC3339) {
		t.Errorf("since %v, want %s", since, cut.Format(time.RFC3339))
	}
	calls := tiles(t, body)["calls"]
	series := calls["series"].([]any)
	if series[0] == nil || series[1] != nil || series[len(series)-1] == nil || num64(calls["value"]) != 96 || num64(calls["median"]) != 24 || calls["change"] != nil {
		t.Errorf("calls %v", calls)
	}
	_, body = r.get(t, "overview", p(feedSite, "range", "7d", "tz", "UTC"))
	if since := body["period"].(map[string]any)["since"]; since != nil {
		t.Errorf("a website's figures begin %v", since)
	}
}

// The sites list says which have a feed, delivered or not: an API is shown as one from the start.
func TestSitesSayFeed(t *testing.T) {
	t.Parallel()
	r := apiRig(t, nil)
	_, body := r.get(t, "sites", nil)
	got := map[string]any{}
	for _, x := range body["sites"].([]any) {
		m := x.(map[string]any)
		got[m["site"].(string)] = m["feed"]
		if m["server"] != nil {
			t.Errorf("%v delivered", m)
		}
	}
	if got[apiSite] != true || got[feedSite] != true || got[plainSite] != false {
		t.Errorf("feeds %v", got)
	}
}

// What a visitor does most names the operation: two clients of one route that do different things are no one.
func TestAPIVisitorOperation(t *testing.T) {
	t.Parallel()
	r := apiRig(t, mcpCalls())
	_, body := r.get(t, "visitors", p(apiSite, "range", "7d", "tz", "UTC", "sort", "requests"))
	for _, v := range listRows(body) {
		if v["key"] == "192.0.2.1" && !strings.Contains(fmt.Sprint(v["does"]), "POST /mcp whois") {
			t.Errorf("does %v", v["does"])
		}
	}
}
