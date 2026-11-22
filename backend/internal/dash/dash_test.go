package dash

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/nui/backend/internal/apix"
	"github.com/trick77/nui/backend/internal/demo"
	"github.com/trick77/nui/backend/internal/ingest"
	"github.com/trick77/nui/backend/internal/insight"
	"github.com/trick77/nui/backend/internal/sites"
	"github.com/trick77/nui/backend/internal/store"
)

const (
	feedSite  = "lens.example.com"
	plainSite = "blog.example.com"
)

// fakeModel answers from fixed results and records what it was asked.
type fakeModel struct {
	mu        sync.Mutex
	briefing  *insight.Briefing
	readings  map[string]insight.Reading
	labels    map[string]string
	briefIns  []insight.BriefingInput
	readIns   []insight.ReadingInput
	urgentSet bool
}

func (f *fakeModel) Configured() bool { return true }
func (f *fakeModel) Briefing(_ context.Context, in insight.BriefingInput) *insight.Briefing {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.briefIns = append(f.briefIns, in)
	return f.briefing
}
func (f *fakeModel) Readings(_ context.Context, ins []insight.ReadingInput, urgent bool) map[string]insight.Reading {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readIns = append(f.readIns, ins...)
	f.urgentSet = f.urgentSet || urgent
	out := map[string]insight.Reading{}
	for _, in := range ins {
		if r, ok := f.readings[in.Key]; ok {
			out[in.Key] = r
		}
	}
	return out
}
func (f *fakeModel) Labels(_ context.Context, _ int64, _ string, values []string) map[string]string {
	out := map[string]string{}
	for _, v := range values {
		if l, ok := f.labels[v]; ok {
			out[v] = l
		}
	}
	return out
}
func (f *fakeModel) Usage(context.Context, int64) (int64, int64) { return 3, 200 }

type rig struct {
	s   *Server
	h   http.Handler
	end time.Time
}

func newRig(t *testing.T, days, people int, model insight.Model) *rig {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "nui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rows, err := st.SyncSites(t.Context(), []store.Site{
		{Domain: feedSite, Kind: "site", Product: "Lens", FeedKeyHash: sites.HashKey("k"), PathRules: "[]"},
		{Domain: plainSite, Kind: "site", Product: "Blog", PathRules: "[]"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := sites.New(rows)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Date(2026, 11, 22, 13, 30, 0, 0, time.UTC)
	if days > 0 {
		for _, d := range []struct {
			id   int64
			feed bool
		}{{rows[feedSite].ID, true}, {rows[plainSite].ID, false}} {
			if _, err := demo.Generate(t.Context(), st, demo.Options{SiteID: d.id, Days: days, People: people, End: end,
				Seed: 7, Feed: d.feed, Classes: true}); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.AddStats(t.Context(), []store.IngestStat{{SiteID: rows[feedSite].ID, Hour: end.Unix()/3600 - 30,
			Accepted: 100, AcceptedServer: 60, RateLimited: 4}, {SiteID: 0, Hour: end.Unix() / 3600, UnknownDomain: 2}}); err != nil {
			t.Fatal(err)
		}
	}
	counters := ingest.NewCounters(func() time.Time { return end })
	counters.Add(rows[feedSite].ID, func(s *store.IngestStat) { s.Accepted += 5 })
	counters.Seen(rows[feedSite].ID, "browser")
	s := &Server{DB: st, Sites: reg, Counters: counters, Model: model, RetentionMonths: 25,
		Now: func() time.Time { return end.Add(time.Minute) }}
	mux := http.NewServeMux()
	mux.Handle("GET /api/dash/{endpoint}", s)
	return &rig{s: s, h: mux, end: end}
}

func (r *rig) get(t *testing.T, endpoint string, params map[string]string) (int, map[string]any) {
	t.Helper()
	v := url.Values{}
	for k, x := range params {
		v.Set(k, x)
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/dash/"+endpoint+"?"+v.Encode(), nil))
	plainDots(t, endpoint, w.Body.String())
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: not JSON: %q", endpoint, w.Body)
	}
	return w.Code, out
}

// plainDots fails on a separating dot with a plain space beside it in an answer: the UI shows these texts verbatim,
// and apix.Sep's en spaces are the only spacing.
func plainDots(t *testing.T, endpoint, body string) {
	t.Helper()
	for _, bad := range []string{" ·", "· "} {
		if i := strings.Index(body, bad); i >= 0 {
			t.Errorf("%s: plain space beside a dot: %q", endpoint, body[max(0, i-30):min(len(body), i+30)])
		}
	}
}

func TestSeparatorSpacing(t *testing.T) {
	r := newRig(t, 3, 30, nil)
	sep := "\xe2\x80\x82·\xe2\x80\x82"
	// Composed texts the existing answers carry: the footer's rules, a browser's client in the log and the table.
	_, d := r.get(t, "domain", p(feedSite))
	rules := d["rules"].(map[string]any)
	for _, k := range []string{"rate", "batch"} {
		if !strings.Contains(rules[k].(string), sep) {
			t.Errorf("rules.%s %q", k, rules[k])
		}
	}
	for _, ep := range []string{"log", "visitors"} {
		_, body := r.get(t, ep, p(feedSite))
		b, _ := json.Marshal(body)
		if !strings.Contains(string(b), sep) {
			t.Errorf("%s: no browser client with the separator", ep)
		}
	}
	// A day classified before the en spaces reads with them.
	if got := respace([]string{"also unknown automated: Chrome · macOS, 3 of 9 events"})[0]; got !=
		"also unknown automated: Chrome"+sep+"macOS, 3 of 9 events" {
		t.Errorf("respace %q", got)
	}
}

// A client's family names its evidence id and the reading signature, the keys of cached readings.
func TestClientFamily(t *testing.T) {
	// A name without the separator keeps the family it always had, byte for byte: the version cut at the first "/".
	before := func(c string) string {
		if i := strings.IndexByte(c, '/'); i > 0 {
			return c[:i]
		}
		return c
	}
	for _, c := range []string{"curl/8.4.0", "CURL/8", "Googlebot/2.1", "UptimeRobot", "Go-http-client/1.1",
		"go_http_client/2", "HeadlessChrome/120", "MJ12bot/v1.4.8", "MJ13bot", "python-requests/2.32.3",
		"no user agent", "12345", "(weird)", "/x/y", "Chrome-macOS/1.0", "Chrome · macOS"} {
		if got := clientFamily(c); got != before(c) {
			t.Errorf("clientFamily(%q) = %q, was %q", c, got, before(c))
		}
	}
	// A browser's parts are joined by browserJoin, not the shown separator: respacing it changes no family.
	browser := "Chrome" + apix.Sep + "macOS"
	if got := clientFamily(browser); got != "Chrome+macOS" {
		t.Errorf("browser family %q", got)
	}
	if got := clientFamily("Samsung Internet" + apix.Sep + "Android"); got != "Samsung Internet+Android" {
		t.Errorf("two-word browser family %q", got)
	}
	// Distinct names stay apart; an empty one gets a family of its own.
	for _, pair := range [][2]string{{"MJ12bot", "MJ13bot"}, {"CURL", "curl"}, {"go_http_client", "Go-http-client"},
		{"123", "456"}, {"Chrome-macOS/1.0", browser}, {"", "no user agent"}} {
		a, b := clientFamily(pair[0]), clientFamily(pair[1])
		if a == b || a == "" || b == "" {
			t.Errorf("%q and %q: families %q and %q", pair[0], pair[1], a, b)
		}
	}
	if clientFamily("") != clientFamily(" ") || clientFamily("") == "" {
		t.Errorf("empty family %q", clientFamily(""))
	}
	// The reading input carries it as the client's evidence id.
	var s Server
	in, facts := s.readingInput(1, visitorRow{Class: "human", Client: browser}, nil, trailResult{})
	if _, ok := facts["client:Chrome+macOS"]; !ok || in.Signature == "" {
		t.Errorf("client id %v", facts)
	}
}

func p(site string, kv ...string) map[string]string {
	m := map[string]string{"site": site, "range": "7d", "tz": "Europe/Zurich"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// noNullLists fails on a JSON null where a list belongs: the UI maps over them.
func noNullLists(t *testing.T, where string, v any) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			if c == nil && listKey(k) {
				t.Errorf("%s: %s is null", where, k)
			}
			noNullLists(t, where+"."+k, c)
		}
	case []any:
		for _, c := range x {
			noNullLists(t, where, c)
		}
	}
}

func listKey(k string) bool {
	switch k {
	case "rows", "items", "tiles", "series", "prev_series", "labels", "signals", "detail", "trail", "hours",
		"sites", "evidence", "related", "ip_version", "browsers", "os", "devices", "ids":
		return true
	}
	return false
}

func TestEmptyDatabase(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	for _, site := range []string{feedSite, plainSite} {
		for _, ep := range []struct {
			name   string
			params map[string]string
		}{
			{"overview", nil}, {"noteworthy", nil}, {"classes", nil}, {"list", map[string]string{"dim": "pages"}},
			{"list", map[string]string{"dim": "probed"}}, {"status", nil}, {"clients", nil}, {"visitors", nil},
			{"log", nil}, {"domain", nil},
		} {
			params := p(site)
			for k, v := range ep.params {
				params[k] = v
			}
			code, body := r.get(t, ep.name, params)
			serverOnly := ep.name == "classes" || ep.name == "status" || ep.params["dim"] == "probed"
			want := http.StatusOK
			if serverOnly && site == plainSite {
				want = http.StatusNotFound
			}
			if code != want {
				t.Errorf("%s %s: %d %v", site, ep.name, code, body)
			}
			noNullLists(t, site+" "+ep.name, body)
		}
	}
	code, body := r.get(t, "visitor", p(feedSite, "key", "192.0.2.1"))
	if code != http.StatusNotFound || body["detail"] == nil {
		t.Errorf("visitor without events: %d %v", code, body)
	}
	if code, body := r.get(t, "sites", nil); code != 200 || len(body["sites"].([]any)) != 2 {
		t.Errorf("sites: %d %v", code, body)
	}
}

func TestBadParameters(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	for _, tt := range []struct {
		ep     string
		params map[string]string
		want   int
	}{
		{"nope", p(feedSite), 404},
		{"overview", p("unknown.example.com"), 400},
		{"overview", p(feedSite, "range", "1y"), 400},
		{"overview", p(feedSite, "aud", "aliens"), 400},
		{"overview", p(feedSite, "cmp", "next"), 400},
		{"overview", p(feedSite, "tz", "Mars/Olympus"), 400},
		{"overview", p(feedSite, "tz", "../../etc/passwd"), 400},
		{"list", p(feedSite, "dim", "secrets"), 400},
		{"list", p(feedSite, "dim", "pages", "limit", "0"), 400},
		{"list", p(feedSite, "dim", "pages", "sort", "random"), 400},
		{"list", p(feedSite, "dim", "events", "within", "/"), 400},
		{"visitors", p(feedSite, "sort", "x"), 400},
		{"visitors", p(feedSite, "class", "alien"), 400},
		{"visitors", p(feedSite, "limit", "x"), 400},
		{"visitors", p(feedSite, "cursor", "-1"), 400},
		{"visitor", p(feedSite), 400},
		{"visitor", p(feedSite, "key", "192.0.2.1", "cursor", "x"), 400},
		{"log", p(feedSite, "addr", "not-an-address"), 400},
		{"log", p(feedSite, "addr", "2001:db8:1::/48"), 400},
		{"log", p(feedSite, "addr", "192.0.2.0/24"), 400},
		{"log", p(feedSite, "class", "x"), 400},
		{"log", p(feedSite, "asn", "x"), 400},
		{"log", p(feedSite, "asn", "0"), 400},
		{"log", p(feedSite, "asn", "4294967296"), 400},
		{"log", p(feedSite, "status", "6xx"), 400},
		{"log", p(feedSite, "cursor", "1"), 400},
	} {
		if code, body := r.get(t, tt.ep, tt.params); code != tt.want || body["detail"] == nil {
			t.Errorf("%s %v: %d %v, want %d with detail", tt.ep, tt.params, code, body, tt.want)
		}
	}
}

func tiles(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, x := range body["tiles"].([]any) {
		m := x.(map[string]any)
		out[m["key"].(string)] = m
	}
	return out
}

func TestOverview(t *testing.T) {
	r := newRig(t, 20, 12, nil)
	for _, rng := range []string{"24h", "7d", "30d", "90d", "12mo"} {
		for _, aud := range []string{"all", "humans", "bots"} {
			for _, c := range []string{"prev", "yoy", "off"} {
				// Every range once; the audience and comparison matrix on the short ones (SQLite under -race is slow).
				if (aud != "all" || c != "prev") && (rng == "90d" || rng == "12mo" || rng == "30d") {
					continue
				}
				code, body := r.get(t, "overview", p(feedSite, "range", rng, "aud", aud, "cmp", c))
				if code != 200 {
					t.Fatalf("%s %s %s: %d %v", rng, aud, c, code, body)
				}
				noNullLists(t, "overview", body)
				period := body["period"].(map[string]any)
				n := map[string]int{"24h": 24, "7d": 7, "30d": 30, "90d": 90, "12mo": 12}[rng]
				if len(period["labels"].([]any)) != n || period["last_bucket_open"] != true {
					t.Errorf("%s period %v", rng, period)
				}
				ts := tiles(t, body)
				if len(ts) != 6 {
					t.Errorf("%s: %d tiles", rng, len(ts))
				}
				if len(ts["views"]["series"].([]any)) != n {
					t.Errorf("%s: series length", rng)
				}
				if c == "off" && (ts["views"]["change"] != nil || len(ts["views"]["prev_series"].([]any)) != 0) {
					t.Errorf("cmp off still compares: %v", ts["views"])
				}
				if aud == "bots" && ts["visitors"]["label"] != "Addresses" {
					t.Errorf("bots label %v", ts["visitors"]["label"])
				}
			}
		}
	}
	_, body := r.get(t, "overview", p(feedSite))
	ts := tiles(t, body)
	if ts["visitors"]["value"].(float64) == 0 || ts["views"]["value"].(float64) == 0 ||
		ts["requests"]["value"].(float64) == 0 || ts["auto_share"]["value"].(float64) == 0 {
		t.Errorf("empty tiles: %v", ts)
	}
	// The visitors value is distinct over the closed buckets, never a sum of buckets.
	var sum float64
	series := ts["visitors"]["series"].([]any)
	for _, x := range series[:len(series)-1] {
		sum += x.(float64)
	}
	if ts["visitors"]["value"].(float64) > sum {
		t.Errorf("visitors %v above the bucket sum %v", ts["visitors"]["value"], sum)
	}
	if ts["auto_share"]["change"] != nil {
		if _, ok := ts["auto_share"]["change"].(map[string]any)["pp"]; !ok {
			t.Errorf("rate change not pp: %v", ts["auto_share"]["change"])
		}
	}
	// Without a feed: no server tiles.
	_, body = r.get(t, "overview", p(plainSite))
	ts = tiles(t, body)
	if len(ts) != 4 || ts["requests"] != nil || ts["auto_share"] != nil {
		t.Errorf("plain site tiles %v", ts)
	}
}

func listRows(body map[string]any) []map[string]any {
	var out []map[string]any
	for _, x := range body["rows"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func TestLists(t *testing.T) {
	model := &fakeModel{labels: map[string]string{"GET /.env": "looking for leaked secrets"}}
	r := newRig(t, 10, 15, model)
	for dim := range dims {
		for _, sortBy := range []string{"volume", "change"} {
			code, body := r.get(t, "list", p(feedSite, "dim", dim, "sort", sortBy, "range", "30d"))
			if code != 200 {
				t.Fatalf("%s: %d %v", dim, code, body)
			}
			noNullLists(t, dim, body)
			if len(body["rows"].([]any)) > 6 {
				t.Errorf("%s: more than 6 rows", dim)
			}
		}
	}
	_, body := r.get(t, "list", p(feedSite, "dim", "pages", "range", "30d", "limit", "4"))
	rows := listRows(body)
	if len(rows) != 4 || rows[0]["expandable"] != true || body["other"].(map[string]any)["count"].(float64) == 0 ||
		body["total"].(float64) != 4+body["other"].(map[string]any)["count"].(float64) {
		t.Errorf("pages %v", body)
	}
	var entries float64
	for _, row := range rows {
		entries += row["entry"].(float64)
	}
	if entries <= 0 || entries > 100 {
		t.Errorf("entry shares sum to %v", entries)
	}
	_, body = r.get(t, "list", p(feedSite, "dim", "pages", "range", "30d", "within", "/:cidr"))
	if rows := listRows(body); len(rows) == 0 || rows[0]["label"] != "/1.1.1.0/24" || rows[0]["expandable"] == true ||
		rows[0]["entry"] == nil {
		t.Errorf("within %v", body)
	}
	// Search before the top-N, on key and label.
	_, body = r.get(t, "list", p(feedSite, "dim", "pages", "range", "30d", "q", "IPV"))
	if rows := listRows(body); len(rows) != 2 || body["total"].(float64) != 2 {
		t.Errorf("q %v", body)
	}
	_, body = r.get(t, "list", p(feedSite, "dim", "countries", "range", "30d", "limit", "300", "q", "ch"))
	if rows := listRows(body); len(rows) != 1 || rows[0]["key"] != "CH" || rows[0]["level"].(float64) != 4 {
		t.Errorf("countries %v", body)
	}
	_, body = r.get(t, "list", p(feedSite, "dim", "events", "range", "30d", "sub", "custom"))
	if rows := listRows(body); len(rows) != 1 || rows[0]["key"] != "copy_prefix" || rows[0]["sub"] != "custom" {
		t.Errorf("custom events %v", body)
	}
	_, body = r.get(t, "list", p(feedSite, "dim", "events", "range", "30d", "sub", "auto"))
	for _, row := range listRows(body) {
		if row["sub"] != "auto" {
			t.Errorf("auto events %v", row)
		}
	}
	if code, _ := r.get(t, "list", p(feedSite, "dim", "pages", "sub", "custom")); code != 400 {
		t.Errorf("sub on pages: %d", code)
	}
	if _, body = r.get(t, "list", p(feedSite, "dim", "referrers")); listRows(body) != nil && listRows(body)[0]["entry"] != nil {
		t.Error("entry outside pages")
	}
	_, body = r.get(t, "list", p(feedSite, "dim", "probed", "range", "30d", "limit", "50"))
	found := false
	for _, row := range listRows(body) {
		if row["key"] == "GET /.env" {
			found = row["gen"] == "looking for leaked secrets" && row["sub"] == "404"
		}
	}
	if !found {
		t.Errorf("probed %v", body)
	}
	_, body = r.get(t, "list", p(feedSite, "dim", "refused", "limit", "500"))
	if rows := listRows(body); len(rows) == 0 || !strings.HasPrefix(rows[0]["key"].(string), "203.0.113.") {
		t.Errorf("refused %v", body)
	}
	for _, row := range listRows(body) {
		if row["value"].(float64) == 0 {
			t.Errorf("refused lists an address without refusals: %v", row)
		}
	}
	_, body = r.get(t, "list", p(feedSite, "dim", "events"))
	subs := map[string]bool{}
	for _, row := range listRows(body) {
		subs[row["sub"].(string)] = true
	}
	if !subs["auto"] {
		t.Errorf("events subs %v", body)
	}
	_, body = r.get(t, "list", p(feedSite, "dim", "networks", "range", "30d"))
	if rows := listRows(body); len(rows) == 0 || !strings.HasPrefix(rows[0]["label"].(string), "AS") {
		t.Errorf("networks %v", body)
	}
	if code, _ := r.get(t, "list", p(plainSite, "dim", "refused")); code != 404 {
		t.Errorf("refused without a feed: %d", code)
	}
	// Status, clients, classes.
	_, body = r.get(t, "status", p(feedSite))
	if rows := listRows(body); len(rows) != 2 || rows[0]["class"] != "2xx" || rows[1]["class"] != "4xx" {
		t.Errorf("status %v", body)
	}
	_, body = r.get(t, "clients", p(feedSite))
	for _, k := range []string{"ip_version", "browsers", "os", "devices"} {
		if len(body[k].([]any)) == 0 || len(body[k].([]any)) > 5 {
			t.Errorf("clients %s %v", k, body[k])
		}
	}
	_, body = r.get(t, "clients", p(feedSite, "aud", "bots"))
	if len(body["browsers"].([]any)) == 0 {
		t.Errorf("bot clients %v", body)
	}
	_, body = r.get(t, "classes", p(feedSite, "aud", "humans"))
	rows = listRows(body)
	if len(rows) != 4 || rows[0]["class"] != "human" || rows[0]["requests"].(float64) == 0 ||
		len(body["series"].(map[string]any)["unknown"].([]any)) != 7 {
		t.Errorf("classes %v", body)
	}
}

func TestVisitorsAndDetail(t *testing.T) {
	model := &fakeModel{readings: map[string]insight.Reading{}, labels: map[string]string{
		"python-requests/2.32.3": "Python scraping script"}}
	r := newRig(t, 3, 8, model)
	_, body := r.get(t, "visitors", p(feedSite, "sort", "refused", "range", "24h"))
	rows := listRows(body)
	if len(rows) == 0 || rows[0]["class"] != "unknown" || rows[0]["refused_share"].(float64) < 99 ||
		rows[0]["client"] != "python-requests/2.32.3" || rows[0]["does"].(map[string]any)["text"] == "" {
		t.Fatalf("visitors %v", rows)
	}
	scanner := rows[0]["key"].(string)
	if rows[0]["client_gen"] != "Python scraping script" {
		t.Errorf("client_gen %v", rows[0]["client_gen"])
	}
	if rows[0]["visits"] != nil {
		t.Errorf("a server-only actor has visits: %v", rows[0]["visits"])
	}
	if body["total"].(float64) < 5 {
		t.Errorf("total %v", body["total"])
	}
	// Paging (an opaque keyset cursor), class filter, search.
	_, page1 := r.get(t, "visitors", p(feedSite, "limit", "2"))
	if page1["next"] == "" || len(page1["rows"].([]any)) != 2 {
		t.Errorf("page 1 %v", page1)
	}
	_, page2 := r.get(t, "visitors", p(feedSite, "limit", "2", "cursor", page1["next"].(string)))
	if listRows(page2)[0]["key"] == listRows(page1)[0]["key"] || listRows(page2)[0]["key"] == listRows(page1)[1]["key"] {
		t.Error("page 2 repeats page 1")
	}
	if code, _ := r.get(t, "visitors", p(feedSite, "cursor", "100000")); code != http.StatusBadRequest {
		t.Errorf("a foreign cursor: %d", code)
	}
	_, monitors := r.get(t, "visitors", p(feedSite, "class", "monitor"))
	if rows := listRows(monitors); len(rows) != 1 || rows[0]["key"] != "69.162.124.230" {
		t.Errorf("monitor filter %v", monitors)
	}
	// The search matches the model's client label the table shows, as stored.
	if err := r.s.DB.PutLabel(t.Context(), "ua", "python-requests/2.32.3", "Python scraping script"); err != nil {
		t.Fatal(err)
	}
	_, found := r.get(t, "visitors", p(feedSite, "q", "Scraping SCRIPT", "range", "24h"))
	if rows := listRows(found); len(rows) == 0 || rows[0]["client_gen"] != "Python scraping script" {
		t.Errorf("search by client label: %v", found)
	} else {
		for _, row := range rows {
			if row["client"] != "python-requests/2.32.3" {
				t.Errorf("search by client label matched %v", row["client"])
			}
		}
	}
	// Without a feed: requests and refused are null, sorted by views.
	_, plain := r.get(t, "visitors", p(plainSite))
	if rows := listRows(plain); len(rows) == 0 || rows[0]["requests"] != nil || rows[0]["refused"] != nil {
		t.Errorf("plain visitors %v", plain)
	}
	// The table asked the model about its rows, not urgently, and queued readings for a few of them only.
	queued := 0
	for _, in := range model.readIns {
		if in.Queue {
			queued++
		}
	}
	if len(model.readIns) == 0 || model.urgentSet || model.readIns[0].Signature == "" || queued == 0 ||
		queued > readQueueMax*4 {
		t.Errorf("readings asked %d, queued %d", len(model.readIns), queued)
	}
	// A cached reading citing a fact this actor does not have is shown nowhere.
	model.readings[scanner] = insight.Reading{Verdict: "Scraping attempt", Summary: "Walks the lookup API.",
		Evidence: []string{"requests:refused-most", "nope"}}
	_, body = r.get(t, "visitors", p(feedSite, "sort", "refused", "range", "24h"))
	if g := listRows(body)[0]["does"].(map[string]any)["gen"]; g != nil {
		t.Errorf("unresolvable reading shown: %v", g)
	}
	if _, d := r.get(t, "visitor", p(feedSite, "key", scanner, "range", "24h")); d["reading"] != nil ||
		d["does"].(map[string]any)["gen"] != nil || d["does"].(map[string]any)["text"] == "" {
		t.Errorf("unresolvable reading in the detail: %v %v", d["reading"], d["does"])
	}
	// Detail: a cached reading for the scanner, evidence resolved to the actor's own figures and span.
	model.readings[scanner] = insight.Reading{Verdict: "Scraping attempt", Summary: "Walks the lookup API.",
		Evidence: []string{"requests:refused-most", "pace:fast"}}
	_, body = r.get(t, "visitors", p(feedSite, "sort", "refused", "range", "24h"))
	if g := listRows(body)[0]["does"].(map[string]any)["gen"]; g != "Walks the lookup API." {
		t.Errorf("does.gen %v", g)
	}
	code, d := r.get(t, "visitor", p(feedSite, "key", scanner, "range", "24h"))
	if code != 200 {
		t.Fatalf("visitor %d %v", code, d)
	}
	noNullLists(t, "visitor", d)
	// "What it does" reads as in the table, both sentences.
	tableDoes := listRows(body)[0]["does"].(map[string]any)
	if dd := d["does"].(map[string]any); dd["gen"] != "Walks the lookup API." || dd["text"] != tableDoes["text"] {
		t.Errorf("detail does %v, table %v", dd, tableDoes)
	}
	// The client reads as in the table.
	if d["client_gen"] != "Python scraping script" || d["client"] != "python-requests/2.32.3" {
		t.Errorf("detail client %v / %v", d["client_gen"], d["client"])
	}
	reading := d["reading"].(map[string]any)
	ev := reading["evidence"].([]any)
	if reading["verdict"] != "Scraping attempt" || reading["suspicious"] != true || len(ev) != 2 ||
		!strings.Contains(ev[0].(string), "refused") || !strings.HasPrefix(ev[1].(string), "Median 0.8 s") ||
		!model.urgentSet {
		t.Errorf("reading %v", reading)
	}
	if b := reading["basis"].(map[string]any); b["from"] == nil || b["to"] == nil || b["from"] == b["to"] {
		t.Errorf("basis %v", b)
	}
	if len(d["hours"].([]any)) != 24 {
		t.Errorf("hours %v", d["hours"])
	}
	trail := d["trail"].([]any)
	first := trail[0].(map[string]any)
	if first["what"] != "GET /api/v1/ip/{addr} → 401" || first["count"].(float64) < 50 || first["interval_s"].(float64) != 0.8 {
		t.Errorf("trail %v", first)
	}
	// An IPv6 visitor by its /64.
	_, v6 := r.get(t, "visitors", p(feedSite, "q", "2001:db8", "range", "24h"))
	if rows := listRows(v6); len(rows) == 0 || !strings.HasSuffix(rows[0]["key"].(string), "/64") {
		t.Errorf("v6 %v", v6)
	} else if code, _ := r.get(t, "visitor", p(feedSite, "key", rows[0]["key"].(string), "range", "24h")); code != 200 {
		t.Errorf("v6 detail %d", code)
	}
}

// The model's reading gets a statement on the raw values behind the top template: a script walking a range of
// addresses reads apart from one repeating a single request, though both show the same template.
func TestReadingValueVariety(t *testing.T) {
	model := &fakeModel{readings: map[string]insight.Reading{}}
	r := newRig(t, 0, 0, model)
	site, _ := r.s.Sites.Lookup(feedSite)
	var evs []store.Event
	for i := range 60 {
		at := r.end.UnixMilli() - int64(60-i)*1000 // oldest first: the walk goes up
		evs = append(evs, store.Event{SiteID: site.ID, TS: at, Source: "server", Kind: "request", IP: "192.0.2.60",
			IPKey: "192.0.2.60", UA: "curl/8", Template: "GET /api/v1/ip/{addr}",
			Path: fmt.Sprintf("/api/v1/ip/10.0.%d.%d", i/250, i%250+1), Status: 401},
			store.Event{SiteID: site.ID, TS: at, Source: "server", Kind: "request", IP: "192.0.2.61",
				IPKey: "192.0.2.61", UA: "curl/8", Template: "GET /api/v1/ip/{addr}", Path: "/api/v1/ip/10.0.0.1",
				Status: 401})
	}
	if err := r.s.DB.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	evidence := func(key string) []string {
		t.Helper()
		model.readIns = nil
		if code, _ := r.get(t, "visitor", p(feedSite, "key", key, "range", "24h")); code != 200 {
			t.Fatalf("visitor %s: %d", key, code)
		}
		var ids []string
		for _, e := range model.readIns[0].Evidence {
			ids = append(ids, e.ID)
		}
		return ids
	}
	// A person's browser: engagements on varying pages, its top action no request. No value statement.
	var person []store.Event
	for i := range 10 {
		person = append(person, store.Event{SiteID: site.ID, TS: r.end.UnixMilli() - int64(i)*1000, Source: "browser",
			Kind: "engagement", IP: "192.0.2.62", IPKey: "192.0.2.62", UA: "Mozilla/5.0", Template: "/AS:n",
			Path: fmt.Sprintf("/AS%d", i)})
	}
	if err := r.s.DB.InsertEvents(t.Context(), person); err != nil {
		t.Fatal(err)
	}
	walk, same, browser := evidence("192.0.2.60"), evidence("192.0.2.61"), evidence("192.0.2.62")
	if !slices.Contains(walk, "values:walk") || !slices.Contains(same, "values:same") {
		t.Errorf("walker %v, repeater %v", walk, same)
	}
	for _, id := range browser {
		if strings.HasPrefix(id, "values:") {
			t.Errorf("a browser's engagements got %s", id)
		}
	}
	if model.readIns[0].Signature == "" {
		t.Error("no signature")
	}
	for paths, want := range map[string]string{
		"/AS3,/AS2,/AS1,/AS0":                                         "values:walk",
		"/AS9,/AS2,/AS7,/AS1":                                         "values:distinct",
		"/x/a,/x/b,/x/c,/x/a,/x/b,/x/c,/x/a,/x/b":                     "values:mixed",
		"/:text,/:text,/:text":                                        "", // the template itself: no value
		"/x/1,/x/1,/x/1,/x/1,/x/1,/x/1,/x/2,/x/1,/x/1,/x/1,/x/1,/x/2": "values:few",
		"/q/b,/q/a":         "", // too few
		"/q/zz,/q/zy,/q/zx": "values:distinct",
	} {
		if got := varietyOf(strings.Split(paths, ","), "/:text"); got.id != want {
			t.Errorf("%s: %q, want %q", paths, got.id, want)
		}
	}
}

// A network spike opens the log on that network's requests: the log filters by AS number.
func TestNetworkSpikeOpensItsLog(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	site, _ := r.s.Sites.Lookup(feedSite)
	var evs []store.Event
	// In a closed bucket (findings read those only): ten addresses of one network, none a spike of its own.
	at := r.end.Add(-2 * time.Hour).UnixMilli()
	for i := range 250 {
		ip := fmt.Sprintf("198.51.100.%d", i%10)
		evs = append(evs, store.Event{SiteID: site.ID, TS: at - int64(i)*1000, Source: "server",
			Kind: "request", IP: ip, IPKey: ip, ASN: 64500, Template: "GET /x", Status: 200})
	}
	for i := range 20 {
		evs = append(evs, store.Event{SiteID: site.ID, TS: at - int64(i)*1000, Source: "server",
			Kind: "request", IP: "192.0.2.5", IPKey: "192.0.2.5", ASN: 64501, Template: "GET /y", Status: 200})
	}
	if err := r.s.DB.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	_, body := r.get(t, "noteworthy", p(feedSite, "range", "24h"))
	var target map[string]any
	for _, x := range body["items"].([]any) {
		if m := x.(map[string]any); m["id"] == "spike:net:64500" {
			target = m["target"].(map[string]any)
		}
	}
	if target == nil || target["panel"] != "log" {
		t.Fatalf("no network spike to the log: %v", body["items"])
	}
	params := p(feedSite, "range", "24h")
	for k, v := range target["filter"].(map[string]any) {
		params[k] = v.(string)
	}
	if params["asn"] != "64500" {
		t.Fatalf("filter %v", target["filter"])
	}
	_, log := r.get(t, "log", params)
	if log["total"].(float64) != 250 {
		t.Errorf("the network's log holds %v events, want its 250", log["total"])
	}
	for _, row := range listRows(log) {
		if !strings.HasPrefix(row["addr"].(string), "198.51.100.") {
			t.Errorf("another network's event: %v", row)
		}
	}
	if _, log := r.get(t, "log", p(feedSite, "range", "24h", "asn", "AS64501")); log["total"].(float64) != 20 {
		t.Errorf("AS64501: %v", log["total"])
	}
}

// A run longer than a page's event bound is split across pages: each part says so, so no part reads as the whole.
func TestTrailRunAcrossPages(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	site, _ := r.s.Sites.Lookup(feedSite)
	insert := func(key string, n int) {
		evs := make([]store.Event, n)
		for i := range evs {
			evs[i] = store.Event{SiteID: site.ID, TS: r.end.UnixMilli() - int64(i)*10, Source: "server",
				Kind: "request", IP: key, IPKey: key, Template: "GET /a", Status: 200}
		}
		if err := r.s.DB.InsertEvents(t.Context(), evs); err != nil {
			t.Fatal(err)
		}
	}
	line := func(d map[string]any, i int) map[string]any { return d["trail"].([]any)[i].(map[string]any) }
	insert("192.0.2.7", maxTrailEvents+100)
	_, d := r.get(t, "visitor", p(feedSite, "key", "192.0.2.7", "range", "24h"))
	if l := line(d, 0); len(d["trail"].([]any)) != 1 || l["count"].(float64) != maxTrailEvents ||
		l["continues"] != true || l["continued"] != false || d["trail_next"] == "" {
		t.Fatalf("page 1: %v next %v", l, d["trail_next"])
	}
	_, d2 := r.get(t, "visitor", p(feedSite, "key", "192.0.2.7", "range", "24h", "cursor", d["trail_next"].(string)))
	if l := line(d2, 0); len(d2["trail"].([]any)) != 1 || l["count"].(float64) != 100 || l["continued"] != true ||
		l["continues"] != false || d2["trail_next"] != "" {
		t.Errorf("page 2: %v next %v", l, d2["trail_next"])
	}
	// Exactly a page of events: one whole run, no next page.
	insert("192.0.2.8", maxTrailEvents)
	_, d = r.get(t, "visitor", p(feedSite, "key", "192.0.2.8", "range", "24h"))
	if l := line(d, 0); l["count"].(float64) != maxTrailEvents || l["continues"] != false || d["trail_next"] != "" {
		t.Errorf("a full page: %v next %v", l, d["trail_next"])
	}
}

// Opening one visitor reads only its own class rows: never the site's whole range of them.
func TestVisitorReadsOwnClass(t *testing.T) {
	r := newRig(t, 3, 8, nil)
	var stmts []string
	var mu sync.Mutex
	traceSQL = func(q string, _ []any) {
		mu.Lock()
		stmts = append(stmts, strings.Join(strings.Fields(q), " "))
		mu.Unlock()
	}
	t.Cleanup(func() { traceSQL = nil })
	code, d := r.get(t, "visitor", p(feedSite, "key", "69.162.124.230", "range", "24h"))
	if code != http.StatusOK || d["class"] != "monitor" {
		t.Fatalf("visitor %d %v", code, d)
	}
	for _, q := range stmts {
		if strings.Contains(q, "FROM actor_class") && !strings.Contains(q, "ip_key = ?") {
			t.Errorf("the detail read every actor's class: %s", q)
		}
	}
	// A key without a class row reads as its events' class.
	site, _ := r.s.Sites.Lookup(feedSite)
	if err := r.s.DB.InsertEvents(t.Context(), []store.Event{{SiteID: site.ID, TS: r.end.UnixMilli(), Source: "server",
		Kind: "request", IP: "192.0.2.250", IPKey: "192.0.2.250", UA: "curl/8", Template: "GET /", Status: 200,
		Cls: store.ClsUnknown}}); err != nil {
		t.Fatal(err)
	}
	if code, d := r.get(t, "visitor", p(feedSite, "key", "192.0.2.250", "range", "24h")); code != 200 ||
		d["class"] != "unknown" {
		t.Errorf("visitor without a class row: %d %v", code, d["class"])
	}
}

// A pending event is no person's until the pass shows one: it counts as unknown automated, and a visitor whose page
// request waits while its beacon is human is one person, not also an automated address.
func TestPendingCountsAutomated(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	site, _ := r.s.Sites.Lookup(feedSite)
	at := r.end.UnixMilli()
	ev := func(ip, src string, cls int) store.Event {
		return store.Event{SiteID: site.ID, TS: at, Source: src, Kind: "request", IP: ip, IPKey: ip,
			UA: "Mozilla/5.0", Template: "GET /", Status: 200, Cls: cls}
	}
	// A visitor: the beacon first, the page's later API request still pending.
	later := ev("192.0.2.1", "server", store.ClsPending)
	later.TS++
	if err := r.s.DB.InsertEvents(t.Context(), []store.Event{
		later, ev("192.0.2.1", "browser", store.ClsHuman),
		ev("192.0.2.2", "server", store.ClsPending),                                            // a crawler before the pass
		ev("192.0.2.3", "server", store.ClsHuman), ev("192.0.2.3", "server", store.ClsUnknown), // people and a curl
	}); err != nil {
		t.Fatal(err)
	}
	_, ov := r.get(t, "overview", p(feedSite, "range", "24h"))
	if l := ov["live"].(map[string]any); l["humans"] != 2.0 || l["automated"] != 2.0 {
		t.Errorf("live = %v, want 2 humans, 2 automated", l)
	}
	for key, want := range map[string]string{"192.0.2.1": "human", "192.0.2.2": "unknown"} {
		if _, v := r.get(t, "visitor", p(feedSite, "key", key, "range", "24h")); v["class"] != want {
			t.Errorf("visitor %s class = %v, want %s", key, v["class"], want)
		}
	}
	_, vs := r.get(t, "visitors", p(feedSite, "range", "24h"))
	var cls any
	for _, row := range listRows(vs) {
		if row["key"] == "192.0.2.1" {
			cls = row["class"]
		}
	}
	if cls != "human" {
		t.Errorf("visitors row of the visitor: %v", cls)
	}
	for cls, want := range map[string]float64{"human": 2, "unknown": 3} {
		if _, log := r.get(t, "log", p(feedSite, "range", "24h", "class", cls)); log["total"] != want {
			t.Errorf("log class %s: %v events, want %v", cls, log["total"], want)
		}
	}
}

func TestTrailPaging(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	site, _ := r.s.Sites.Lookup(feedSite)
	var evs []store.Event
	for i := range 130 {
		path := "/a"
		if i%2 == 0 {
			path = "/b"
		}
		evs = append(evs, store.Event{SiteID: site.ID, TS: r.end.UnixMilli() - int64(i)*1000, Source: "server",
			Kind: "request", IP: "192.0.2.9", IPKey: "192.0.2.9", Template: "GET " + path, Status: 200})
	}
	if err := r.s.DB.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	_, d := r.get(t, "visitor", p(feedSite, "key", "192.0.2.9", "range", "24h"))
	if len(d["trail"].([]any)) != maxTrailLines || d["trail_next"] == "" {
		t.Fatalf("trail page 1: %d lines, next %v", len(d["trail"].([]any)), d["trail_next"])
	}
	_, d2 := r.get(t, "visitor", p(feedSite, "key", "192.0.2.9", "range", "24h", "cursor", d["trail_next"].(string)))
	if len(d2["trail"].([]any)) != maxTrailLines {
		t.Errorf("trail page 2: %d", len(d2["trail"].([]any)))
	}
	if d2["trail"].([]any)[0].(map[string]any)["t"] == d["trail"].([]any)[0].(map[string]any)["t"] {
		t.Error("page 2 starts where page 1 did")
	}
}

// A session crossing a bucket boundary is one visit, not one per bucket it touches.
func TestVisitsAcrossBuckets(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	site, _ := r.s.Sites.Lookup(plainSite)
	ev := func(ago time.Duration, sid string) store.Event {
		return store.Event{SiteID: site.ID, TS: r.end.Add(-ago).UnixMilli(), Source: "browser", Kind: "page_view",
			Visitor: "v1", SID: sid, IP: "192.0.2.40", IPKey: "192.0.2.40", Template: "/", Cls: store.ClsHuman}
	}
	// s1 runs from 12:50 to 13:10 UTC, across the 13:00 hour; s2 is a second visit; s3 crosses midnight in Zurich
	// (23:00 UTC), where the 12-month range's day pieces meet.
	evs := []store.Event{ev(40*time.Minute, "s1"), ev(20*time.Minute, "s1"), ev(5*time.Minute, "s2"),
		ev(14*time.Hour+40*time.Minute, "s3"), ev(14*time.Hour+20*time.Minute, "s3")}
	if err := r.s.DB.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	for _, rng := range []string{"24h", "7d", "12mo"} {
		_, body := r.get(t, "visitors", p(plainSite, "range", rng))
		rows := body["rows"].([]any)
		if len(rows) != 1 {
			t.Fatalf("%s: %d rows", rng, len(rows))
		}
		row := rows[0].(map[string]any)
		if row["visits"] != 3.0 || row["views"] != 5.0 {
			t.Errorf("%s: visits %v views %v, want 3 and 5", rng, row["visits"], row["views"])
		}
	}
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	ps := pieces(from, from.Add(36*time.Hour))
	if len(ps) != 2 || !ps[0][1].Equal(from.Add(24*time.Hour)) || !ps[1][1].Equal(from.Add(36*time.Hour)) {
		t.Errorf("pieces: %v", ps)
	}
}

// The log reads a day at a time: pages that span windows, and windows with no match, lose and repeat nothing.
func TestLogAcrossWindows(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	site, _ := r.s.Sites.Lookup(feedSite)
	var evs []store.Event
	// 250 matching events over five days, two of them empty, with non-matching ones between.
	for i := range 250 {
		day := []int{0, 1, 4}[i%3]
		ts := r.end.Add(-time.Duration(day)*24*time.Hour - time.Duration(i)*time.Minute).UnixMilli()
		evs = append(evs, store.Event{SiteID: site.ID, TS: ts, Source: "server", Kind: "request", IP: "192.0.2.7",
			IPKey: "192.0.2.7", Template: "GET /rare", Status: 200},
			store.Event{SiteID: site.ID, TS: ts - 1, Source: "server", Kind: "request", IP: "192.0.2.8",
				IPKey: "192.0.2.8", Template: "GET /other", Status: 200})
	}
	if err := r.s.DB.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var prev string
	cur := ""
	for page := 0; ; page++ {
		args := p(feedSite, "path", "rare")
		if cur != "" {
			args["cursor"] = cur
		}
		code, body := r.get(t, "log", args)
		if code != http.StatusOK || body["total"] != 250.0 {
			t.Fatalf("page %d: %d, total %v", page, code, body["total"])
		}
		for _, x := range body["rows"].([]any) {
			row := x.(map[string]any)
			key := row["t"].(string)
			if prev != "" && key > prev {
				t.Fatalf("page %d: %s after %s, not newest first", page, key, prev)
			}
			prev = key
			seen[key] = true
		}
		cur, _ = body["next"].(string)
		if cur == "" {
			break
		}
	}
	if len(seen) != 250 {
		t.Errorf("paged %d distinct rows, want 250", len(seen))
	}
	if got := windows(0, 10, 4*time.Millisecond); len(got) != 3 || got[0] != [2]int64{6, 10} || got[2] != [2]int64{0, 2} {
		t.Errorf("windows: %v", got)
	}
}

func TestLog(t *testing.T) {
	r := newRig(t, 2, 6, nil)
	code, body := r.get(t, "log", p(feedSite, "range", "24h"))
	if code != 200 || len(body["rows"].([]any)) != logPage || body["next"] == "" || body["total"].(float64) <= logPage {
		t.Fatalf("log %d %v", code, body["total"])
	}
	_, page2 := r.get(t, "log", p(feedSite, "range", "24h", "cursor", body["next"].(string)))
	r1, r2 := listRows(body), listRows(page2)
	if r2[0]["t"].(string) > r1[len(r1)-1]["t"].(string) {
		t.Error("page 2 is not older")
	}
	for _, tt := range []struct {
		params []string
		check  func(map[string]any) bool
	}{
		{[]string{"addr", "69.162.124.230"}, func(m map[string]any) bool { return m["class"] == "monitor" }},
		{[]string{"addr", "2001:db8:1:1::/64"}, func(map[string]any) bool { return true }},
		{[]string{"status", "4xx"}, func(m map[string]any) bool { return strings.Contains(m["what"].(string), "→ 4") }},
		{[]string{"status", "401"}, func(m map[string]any) bool { return strings.HasSuffix(m["what"].(string), "→ 401") }},
		{[]string{"class", "unknown"}, func(m map[string]any) bool { return m["class"] == "unknown" }},
		{[]string{"class", "human"}, func(m map[string]any) bool { return m["class"] == "human" }},
		{[]string{"kind", "page_view"}, func(m map[string]any) bool { return m["kind"] == "page_view" }},
		{[]string{"kind", "browser"}, func(m map[string]any) bool { return m["source"] == "browser" }},
		{[]string{"path", "/api/v1/ip/"}, func(m map[string]any) bool { return m["client"] == "python-requests/2.32.3" }},
		{[]string{"path", "cannot read"}, func(m map[string]any) bool { return m["kind"] == "js_error" }},
		{[]string{"kind", "request", "status", "2xx"}, func(m map[string]any) bool { return m["source"] == "server" }},
	} {
		code, body := r.get(t, "log", p(feedSite, append([]string{"range", "24h"}, tt.params...)...))
		if code != 200 {
			t.Errorf("%v: %d", tt.params, code)
			continue
		}
		for _, row := range listRows(body) {
			if !tt.check(row) {
				t.Errorf("%v: row %v", tt.params, row)
				break
			}
		}
	}
}

func TestDomainAndSites(t *testing.T) {
	r := newRig(t, 2, 3, &fakeModel{})
	_, body := r.get(t, "domain", p(feedSite, "range", "7d"))
	c := body["counters"].(map[string]any)
	// The demo's events are counted as ingest would have; the rig adds 100 stored and 5 pending.
	site, _ := r.s.Sites.Lookup(feedSite)
	var demoEvents float64
	_ = r.s.DB.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE site_id = ? AND ts >= ?`, site.ID,
		r.end.Add(-7*24*time.Hour).Truncate(time.Hour).UnixMilli()).Scan(&demoEvents)
	if c["accepted"].(float64) != 105+demoEvents || c["rate_limited"].(float64) != 4 || c["unknown_domain"].(float64) != 2 {
		t.Errorf("counters %v, demo events %v", c, demoEvents)
	}
	src := body["sources"].(map[string]any)
	if src["browser"].(map[string]any)["state"] != "receiving" || src["server"].(map[string]any)["state"] != "receiving" {
		t.Errorf("sources %v", src)
	}
	st := body["storage"].(map[string]any)
	if st["events"].(float64) == 0 || st["bytes"].(float64) == 0 || st["retention_months"].(float64) != 25 {
		t.Errorf("storage %v", st)
	}
	rules := body["rules"].(map[string]any)
	if rules["llm"].(map[string]any)["budget"].(float64) != 200 || rules["tz"] != "Europe/Zurich" {
		t.Errorf("rules %v", rules)
	}
	if src["feed"] != true {
		t.Errorf("feed site's sources say no feed: %v", src)
	}
	_, body = r.get(t, "domain", p(plainSite))
	if src := body["sources"].(map[string]any); src["server"] != nil || src["feed"] != false {
		t.Errorf("plain site's sources %v", src)
	}
	_, body = r.get(t, "sites", nil)
	sts := body["sites"].([]any)
	first := sts[0].(map[string]any) // "Blog" sorts before "Lens"
	if first["site"] != plainSite || first["server"] != nil || sts[1].(map[string]any)["server"] == nil {
		t.Errorf("sites %v", sts)
	}
	if source(time.Time{}, time.Now()) != nil {
		t.Error("a source that never delivered is not null")
	}
}

// A source that never delivered is null in the sites list and the domain footer alike: both, browser only, server
// only and none yet read apart.
func TestSourcesNeverDelivered(t *testing.T) {
	r := newRig(t, 0, 0, nil)
	feed, _ := r.s.Sites.Lookup(feedSite)
	check := func(name string, browser, server bool) {
		t.Helper()
		_, body := r.get(t, "sites", nil)
		var row map[string]any
		for _, s := range body["sites"].([]any) {
			if s.(map[string]any)["site"] == feedSite {
				row = s.(map[string]any)
			}
		}
		_, dom := r.get(t, "domain", p(feedSite))
		src := dom["sources"].(map[string]any)
		for _, got := range []struct {
			where string
			v     any
			want  bool
		}{{"sites browser", row["browser"], browser}, {"sites server", row["server"], server},
			{"domain browser", src["browser"], browser}, {"domain server", src["server"], server}} {
			if (got.v != nil) != got.want {
				t.Errorf("%s: %s = %v", name, got.where, got.v)
			}
		}
		// The configured feed is stated whether or not it delivered: a null server reads as "not yet".
		if src["feed"] != true {
			t.Errorf("%s: domain feed = %v", name, src["feed"])
		}
	}
	r.s.Counters = ingest.NewCounters(func() time.Time { return r.end })
	check("none yet", false, false)
	r.s.Counters.Seen(feed.ID, "server")
	check("server only", false, true)
	r.s.Counters.Seen(feed.ID, "browser")
	check("both", true, true)
	r.s.Counters = ingest.NewCounters(func() time.Time { return r.end })
	r.s.Counters.Seen(feed.ID, "browser")
	check("browser only", true, false)
}

func TestNoteworthy(t *testing.T) {
	r := newRig(t, 16, 10, nil)
	code, body := r.get(t, "noteworthy", p(feedSite, "aud", "bots"))
	if code != 200 {
		t.Fatal(code, body)
	}
	noNullLists(t, "noteworthy", body)
	items := body["items"].([]any)
	if len(items) == 0 || len(items) > maxItems || body["as_of"] == nil {
		t.Fatalf("items %v", body)
	}
	kinds := map[string]bool{}
	ids := []string{}
	for _, x := range items {
		m := x.(map[string]any)
		kinds[m["kind"].(string)] = true
		ids = append(ids, m["id"].(string))
		if m["gen"] != nil {
			t.Errorf("gen without a model: %v", m)
		}
	}
	if !kinds["problem"] {
		t.Errorf("no problem finding (rate-limited events): %v", ids)
	}
	if body["headline"].(map[string]any)["gen"] != nil {
		t.Error("headline gen without a model")
	}

	// With a briefing: its order and words, merged facts in the detail, unknown ids dropped.
	model := &fakeModel{}
	r.s.Model = model
	r.get(t, "noteworthy", p(feedSite))
	if len(model.briefIns) != 1 || model.briefIns[0].Hash == "" || len(model.briefIns[0].Facts) == 0 {
		t.Fatalf("briefing input %+v", model.briefIns)
	}
	facts := model.briefIns[0].Facts
	if len(facts) < 2 {
		t.Skip("demo produced too few facts for a merge")
	}
	model.briefing = &insight.Briefing{Headline: "Quiet week, one noisy script.", Lines: []insight.BriefLine{
		{IDs: []string{"made-up"}, Text: "invented"},
		{IDs: []string{facts[1].ID, facts[0].ID}, Text: "one operator"},
	}}
	_, body = r.get(t, "noteworthy", p(feedSite))
	noNullLists(t, "noteworthy", body)
	first := body["items"].([]any)[0].(map[string]any)
	if first["id"] != facts[1].ID || first["gen"] != "one operator" || body["headline"].(map[string]any)["gen"] != "Quiet week, one noisy script." {
		t.Errorf("briefed %v", body)
	}
	// Merged when of one kind (TestNoteItemsMerge has the figures), else apart.
	covered := first["ids"].([]any)
	if merged := len(covered) == 2 && covered[1] == facts[0].ID; merged != (facts[0].Kind == facts[1].Kind) {
		t.Errorf("ids %v for kinds %s, %s", covered, facts[1].Kind, facts[0].Kind)
	}
	for _, x := range body["items"].([]any)[1:] {
		if x.(map[string]any)["id"] == facts[0].ID && len(covered) == 2 {
			t.Error("a merged fact shows twice")
		}
	}
}

// A briefing line covers every related finding of one kind it names; its figure describes the group (the sum of
// counted events) or is left out, never the first finding's alone.
func TestNoteItemsMerge(t *testing.T) {
	var facts []insight.Fact
	for i, path := range []string{"/.env", "/.git/config", "/wp-login.php", "/.aws/credentials", "/phpinfo.php"} {
		facts = append(facts, insight.Fact{ID: "probe:GET " + path, Kind: "automation", Figure: num(float64(10 + i)),
			Unit: "probes of", Text: "New probed path: GET " + path})
	}
	facts = append(facts,
		insight.Fact{ID: "ref:a.example", Kind: "people", Figure: "4", Unit: "visitors from", Text: "a.example"},
		insight.Fact{ID: "ref:b.example", Kind: "people", Figure: "3", Unit: "visitors from", Text: "b.example"},
		insight.Fact{ID: "spike:addr:192.0.2.1", Kind: "automation", Figure: "900", Unit: "requests from",
			Text: "192.0.2.1"})
	ids := func(fs ...insight.Fact) []string {
		var out []string
		for _, f := range fs {
			out = append(out, f.ID)
		}
		return out
	}
	items, _ := noteItems(facts, &insight.Briefing{Lines: []insight.BriefLine{
		{IDs: ids(facts[:5]...), Text: "Someone hunts for leaked config"},
		{IDs: append(ids(facts[5:7]...), facts[0].ID), Text: "Two new referrers"}, // a used id is not taken twice
		{IDs: []string{facts[7].ID, facts[5].ID}, Text: "One busy script"},        // another kind stays apart
	}})
	probes, refs, spike := items[0], items[1], items[2]
	if probes.Figure != "60" || probes.Unit != "probes of" || len(probes.IDs) != 5 || len(probes.Detail) != 5 ||
		probes.Detail[4] != "14 probes of New probed path: GET /phpinfo.php" ||
		probes.Text != "New probed path: GET /.env, and 4 related" {
		t.Errorf("probes %+v", probes)
	}
	if refs.Figure != "" || refs.Unit != "" || len(refs.IDs) != 2 {
		t.Errorf("visitors do not add up: %+v", refs)
	}
	if spike.Figure != "900" || len(spike.IDs) != 1 || *spike.Gen != "One busy script" {
		t.Errorf("single %+v", spike)
	}
}

func TestPeriod(t *testing.T) {
	zh, _ := time.LoadLocation("Europe/Zurich")
	now := time.Date(2026, 3, 29, 12, 30, 0, 0, time.UTC) // DST change day in Zurich
	p := newPeriod("7d", "prev", now, zh)
	if p.n() != 7 || p.Bounds[7].Sub(p.Bounds[6]) != 23*time.Hour || p.labels[6] != "29.03." || p.prevL != "prior 7 d" {
		t.Errorf("7d %v %v", p.Bounds, p.labels)
	}
	if !p.Prev[7].Equal(p.Bounds[0]) {
		t.Error("prev does not end where the range starts")
	}
	p = newPeriod("24h", "yoy", now, time.UTC)
	if p.n() != 24 || p.labels[23] != "12:00" || p.prevL != "last year" || p.Prev[0].Year() != 2025 {
		t.Errorf("24h %v", p.labels)
	}
	p = newPeriod("12mo", "prev", now, time.UTC)
	if p.labels[11] != "Mar" || p.Bounds[0].Month() != time.April || p.prevL != "prior 12 mo" {
		t.Errorf("12mo %v", p.labels)
	}
	if newPeriod("30d", "off", now, time.UTC).Prev != nil {
		t.Error("off has a prev")
	}
	for _, tt := range []struct {
		c     change
		upBad bool
		want  string
	}{{nil, false, "flat"}, {change{"new": true}, false, "good"}, {change{"new": true}, true, "bad"},
		{change{"pct": 1.0}, false, "flat"}, {change{"pct": -10.0}, false, "bad"}, {change{"pp": 0.2}, true, "flat"},
		{change{"pp": 3.0}, true, "bad"}} {
		if got := verdict(tt.c, tt.upBad); got != tt.want {
			t.Errorf("verdict(%v, %v) = %s", tt.c, tt.upBad, got)
		}
	}
	if median([]float64{3, 1, 2, 4}) != 2.5 || median(nil) != 0 {
		t.Error("median")
	}
	if num(2) != "2" || num(2.25) != "2.3" || signed(-1) != "-1" || changeText(change{"pp": 1.5}) != "+1.5 pp" ||
		changeText(nil) != "" || changeText(change{"new": true}) != "new" || truncate("abcdef", 3) != "abc…" {
		t.Error("formatting")
	}
	if _, ok := blockOf("nope"); ok {
		t.Error("blockOf")
	}
	if clientFamily("curl/8") != "curl" || deref(nil) != 0 {
		t.Error("helpers")
	}
	if rateChange(1, 2, true, false) != nil || sumChange(0, 0, true) != nil {
		t.Error("changes")
	}
	if topShares(map[string]int64{"a": 5, "b": 4, "c": 3, "d": 2, "e": 1, "Other": 1}, 4, 16)[4].Value != 2 {
		t.Error("topShares")
	}
}
