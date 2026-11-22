package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/nui/backend/internal/sites"
	"github.com/trick77/nui/backend/internal/store"
)

const (
	domain  = "app.example.com"
	feedKey = "feed-key-for-tests"
)

type rig struct {
	st  *store.Store
	c   *Collector
	reg *sites.Registry
	mu  sync.Mutex
	now time.Time
}

func (r *rig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newRig is a collector on a fresh database; the writer runs until the test ends unless queue is 0 (not started).
func newRig(t *testing.T, queue int, start bool) *rig {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "nui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cs, err := sites.Parse([]byte(`{"sites":[{"domain":"` + domain + `","feed_key_sha256":"` +
		sites.HashKey(feedKey) + `","path_rules":[{"pattern":"/AS{int}","template":"/AS:n"}]},
		{"domain":"other.example.com"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.SyncSites(t.Context(), sites.Rows(cs))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := sites.New(rows)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{st: st, reg: reg, now: time.Now()}
	counters := NewCounters(r.clock)
	w := NewWriter(st, counters, queue, func(e *store.Event) { e.Country = "CH" }, quiet())
	if start {
		ctx, cancel := context.WithCancel(context.Background())
		go w.Run(ctx)
		t.Cleanup(func() { cancel(); <-w.Done() })
	}
	r.c = &Collector{Sites: reg, Writer: w, Counters: counters, Browser: BrowserTiers(1, r.clock),
		Feed: FeedLimiter(r.clock), TrustedHops: 1, Now: r.clock}
	return r
}

func (r *rig) browser(body, ip, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/in", strings.NewReader(body))
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Safari/605.1.15")
	req.Header.Set("Accept-Language", "de-CH")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("X-Forwarded-For", ip)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	r.c.HandleBrowser(w, req)
	return w
}

func (r *rig) feed(body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/feed", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	r.c.HandleFeed(w, req)
	return w
}

func batch(site string, n int, extra string) string {
	evs := make([]string, n)
	for i := range evs {
		evs[i] = fmt.Sprintf(`{"t":%d,"k":"page_view","p":{}%s}`, 1_000_000+i, extra)
	}
	return `{"v":1,"site":"` + site + `","vid":"v1","sid":"s1","sent":1000100,"ctx":{"url":"https://` + site +
		`/AS15169?x=1","ref":"https://github.com/ipverse","title":"t","webdriver":false,"screen":[1,2],"new_key":{"a":1}},
		"events":[` + strings.Join(evs, ",") + `]}`
}

func (r *rig) stat(t *testing.T, site int64) store.IngestStat {
	t.Helper()
	var sum store.IngestStat
	for _, s := range r.c.Counters.Pending() {
		if s.SiteID == site {
			addStat(&sum, s)
		}
	}
	return sum
}

func (r *rig) siteID(d string) int64 {
	s, _ := r.reg.Lookup(d)
	return s.ID
}

func waitEvents(t *testing.T, st *store.Store, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		_ = st.DB.QueryRow(`SELECT COUNT(*) FROM event`).Scan(&n)
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("events = %d, want %d", n, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBrowser_Accepts(t *testing.T) {
	r := newRig(t, 0, true)
	body := batch(domain, 2, "")
	body = strings.Replace(body, `{"t":1000001,"k":"page_view","p":{}}`,
		`{"t":1000001,"k":"outbound_click","p":{"url":"https://github.com/ipverse/x","text":"x"}}`, 1)
	if w := r.browser(body, "203.0.113.9", "https://"+domain); w.Code != http.StatusNoContent ||
		w.Header().Get("Access-Control-Expose-Headers") != "Retry-After" {
		t.Fatalf("status %d %v", w.Code, w.Header())
	}
	waitEvents(t, r.st, 2)
	rows, err := r.st.DB.Query(`SELECT kind, name, template, path, visitor, ip_key, ref_host, country, props, ts, url
		FROM event ORDER BY ts`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	var props []string
	var ts []int64
	for rows.Next() {
		var kind, name, tmpl, path, vis, key, refHost, country, p, u string
		var t0 int64
		if err := rows.Scan(&kind, &name, &tmpl, &path, &vis, &key, &refHost, &country, &p, &t0, &u); err != nil {
			t.Fatal(err)
		}
		got = append(got, strings.Join([]string{kind, name, tmpl, path, vis, key, refHost, country, u}, "|"))
		props = append(props, p)
		ts = append(ts, t0)
	}
	want := []string{
		"page_view||/AS:n|/AS15169|v1|203.0.113.9|github.com|CH|https://" + domain + "/AS15169?x=1",
		"outbound_click|github.com|/AS:n|/AS15169|v1|203.0.113.9||CH|https://" + domain + "/AS15169?x=1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The context and headers ride on the first event only; unknown ctx keys are kept.
	if !strings.Contains(props[0], `"new_key"`) || !strings.Contains(props[0], `"accept-language":"de-CH"`) ||
		strings.Contains(props[1], `"ctx"`) {
		t.Errorf("props: %v", props)
	}
	// Ordered by the client offset, dated by the server: 100 ms and 99 ms before receipt.
	if ts[1]-ts[0] != 1 {
		t.Errorf("ts %v", ts)
	}
	if s := r.stat(t, r.siteID(domain)); s.Accepted != 2 || s.AcceptedServer != 0 {
		t.Errorf("stat %+v", s)
	}
	if r.c.Counters.Last(r.siteID(domain), "browser").IsZero() {
		t.Error("browser last not recorded")
	}
}

func TestBrowser_Hostile(t *testing.T) {
	r := newRig(t, 0, true)
	site := r.siteID(domain)
	big := `{"v":1,"site":"` + domain + `","events":[{"k":"custom","n":"` + strings.Repeat("x", MaxBrowserBody) + `"}]}`
	for _, tt := range []struct {
		name, body, origin string
		want               int
	}{
		{"oversized body", big, "", http.StatusRequestEntityTooLarge},
		{"not json", `{"v":1,`, "", http.StatusBadRequest},
		{"wrong version", strings.Replace(batch(domain, 1, ""), `"v":1`, `"v":2`, 1), "", http.StatusBadRequest},
		{"no site", `{"v":1,"events":[]}`, "", http.StatusBadRequest},
		{"no events", `{"v":1,"site":"` + domain + `","events":[]}`, "", http.StatusBadRequest},
		{"too many events", batch(domain, MaxBrowserEvents+1, ""), "", http.StatusRequestEntityTooLarge},
		{"long vid", strings.Replace(batch(domain, 1, ""), `"vid":"v1"`, `"vid":"`+strings.Repeat("v", 600)+`"`, 1), "", http.StatusRequestEntityTooLarge},
		{"long title", batch(domain, 1, `,"title":"`+strings.Repeat("t", 600)+`"`), "", http.StatusRequestEntityTooLarge},
		{"long url", batch(domain, 1, `,"url":"https://`+domain+`/`+strings.Repeat("u", 2100)+`"`), "", http.StatusRequestEntityTooLarge},
		{"big props", strings.Replace(batch(domain, 1, ""), `"p":{}`, `"p":{"a":"`+strings.Repeat("x", 500)+`","b":"`+strings.Repeat("x", 500)+`","c":"`+strings.Repeat("x", 500)+`","d":"`+strings.Repeat("x", 500)+`","e":"`+strings.Repeat("x", 500)+`","f":"`+strings.Repeat("x", 500)+`","g":"`+strings.Repeat("x", 500)+`","h":"`+strings.Repeat("x", 500)+`","i":"`+strings.Repeat("x", 500)+`"}`, 1), "", http.StatusRequestEntityTooLarge},
		{"long ctx string", strings.Replace(batch(domain, 1, ""), `"title":"t"`, `"title":"`+strings.Repeat("t", 600)+`"`, 1), "", http.StatusRequestEntityTooLarge},
		{"deep ctx", strings.Replace(batch(domain, 1, ""), `"new_key":{"a":1}`, `"new_key":`+strings.Repeat(`{"a":`, 12)+`1`+strings.Repeat(`}`, 12), 1), "", http.StatusRequestEntityTooLarge},
		{"wide ctx array", strings.Replace(batch(domain, 1, ""), `"screen":[1,2]`, `"screen":[`+strings.TrimSuffix(strings.Repeat("1,", 60), ",")+`]`, 1), "", http.StatusRequestEntityTooLarge},
		{"bad kind", strings.Replace(batch(domain, 1, ""), `page_view`, `drop_table`, 1), "", http.StatusBadRequest},
		{"nameless custom", strings.Replace(batch(domain, 1, ""), `"k":"page_view"`, `"k":"custom"`, 1), "", http.StatusBadRequest},
		{"props not object", strings.Replace(batch(domain, 1, ""), `"p":{}`, `"p":[1]`, 1), "", http.StatusBadRequest},
		{"url elsewhere", strings.Replace(batch(domain, 1, ""), `https://`+domain+`/AS`, `https://evil.example.net/AS`, 1), "", http.StatusBadRequest},
		{"unknown site", batch("unknown.example.net", 1, ""), "", http.StatusForbidden},
		{"foreign origin", batch(domain, 1, ""), "https://evil.example.net", http.StatusForbidden},
		{"sibling origin", batch(domain, 1, ""), "https://other.example.com", http.StatusForbidden},
	} {
		if w := r.browser(tt.body, "198.51.100.1", tt.origin); w.Code != tt.want {
			t.Errorf("%s: status %d, want %d", tt.name, w.Code, tt.want)
		}
	}
	s := r.stat(t, site)
	if s.Accepted != 0 || s.Rejected < 10 {
		t.Errorf("site counters %+v", s)
	}
	if g := r.stat(t, 0); g.UnknownDomain != 1 || g.Rejected == 0 {
		t.Errorf("siteless counters %+v", g)
	}
	// A refused request with the site's Origin is counted for the site, not as siteless.
	before := r.stat(t, site).Rejected
	r.browser(`{nope`, "198.51.100.1", "https://"+domain)
	if r.stat(t, site).Rejected != before+1 {
		t.Error("origin attribution")
	}
}

func TestBrowser_Flooding(t *testing.T) {
	r := newRig(t, 0, true)
	site := r.siteID(domain)
	// One address: a 50-event batch passes, the next exceeds the /64's 60-event burst.
	if w := r.browser(batch(domain, 50, ""), "2001:db8:1:1::1", ""); w.Code != http.StatusNoContent {
		t.Fatalf("first batch %d", w.Code)
	}
	w := r.browser(batch(domain, 50, ""), "2001:db8:1:1::2", "") // same /64, another address
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("same /64: %d %q", w.Code, w.Header().Get("Retry-After"))
	}
	// Rotating /64s within one /48 passes the narrow tier each time until the /48's 600 run out.
	refused := 0
	for i := range 20 {
		if w := r.browser(batch(domain, 50, ""), fmt.Sprintf("2001:db8:1:%x::1", 0x100+i), ""); w.Code == http.StatusTooManyRequests {
			refused++
		}
	}
	if refused == 0 {
		t.Error("a /48 rotating /64s was never limited")
	}
	// Another /48 is unaffected, and so is IPv4.
	if w := r.browser(batch(domain, 10, ""), "2001:db8:2::1", ""); w.Code != http.StatusNoContent {
		t.Errorf("other /48: %d", w.Code)
	}
	if w := r.browser(batch(domain, 10, ""), "192.0.2.10", ""); w.Code != http.StatusNoContent {
		t.Errorf("ipv4: %d", w.Code)
	}
	// Time refills the bucket.
	r.advance(2 * time.Minute)
	if w := r.browser(batch(domain, 50, ""), "2001:db8:1:1::3", ""); w.Code != http.StatusNoContent {
		t.Errorf("after refill: %d", w.Code)
	}
	if s := r.stat(t, site); s.RateLimited < 50 {
		t.Errorf("rate_limited = %d", s.RateLimited)
	}
	if !strings.Contains(RateRule(), "60 events / min") || !strings.Contains(BatchRule(), "32 KiB") {
		t.Error("rules text")
	}
}

// A flood of junk is metered too: one token a request, then 429.
func TestBrowser_JunkFlood(t *testing.T) {
	r := newRig(t, 0, false)
	codes := map[int]int{}
	for range BrowserBurst + 5 {
		codes[r.browser(`{nope`, "203.0.113.200", "").Code]++
	}
	if codes[http.StatusBadRequest] != BrowserBurst || codes[http.StatusTooManyRequests] != 5 {
		t.Errorf("codes %v", codes)
	}
	if s := r.stat(t, 0); s.RateLimited != 5 || s.Rejected != BrowserBurst {
		t.Errorf("stat %+v", s)
	}
}

func TestBrowser_QueueFullDrops(t *testing.T) {
	r := newRig(t, 5, false) // writer not running: the queue fills
	site := r.siteID(domain)
	if w := r.browser(batch(domain, 4, ""), "192.0.2.1", ""); w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
	if w := r.browser(batch(domain, 4, ""), "192.0.2.2", ""); w.Code != http.StatusNoContent {
		t.Fatalf("a dropped batch still answers 204: %d", w.Code)
	}
	if s := r.stat(t, site); s.Accepted != 4 || s.Dropped != 4 {
		t.Errorf("stat %+v", s)
	}
	if r.c.Writer.Queued() != 4 {
		t.Errorf("queued %d", r.c.Writer.Queued())
	}
	if !r.c.Writer.Enqueue(nil) {
		t.Error("empty enqueue")
	}
	// Stopping the writer drains what is queued.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.c.Writer.Run(ctx)
	waitEvents(t, r.st, 4)
}

func feedBody(site string, n int, ev string) string {
	if ev == "" {
		ev = `{"t":%d,"ip":"203.0.113.42","ua":"curl/8.0","method":"get","path":"/api/v1/as/15169?key=secret",
			"route":"GET /api/v1/as/{asn}","status":200,"dur_ms":12,"bytes":5120,"sf_dest":"empty"}`
	}
	evs := make([]string, n)
	for i := range evs {
		evs[i] = strings.ReplaceAll(ev, "%d", fmt.Sprint(time.Now().UnixMilli()))
	}
	return `{"v":1,"site":"` + site + `","events":[` + strings.Join(evs, ",") + `]}`
}

func TestFeed(t *testing.T) {
	r := newRig(t, 0, true)
	site := r.siteID(domain)
	if w := r.feed(feedBody(domain, 2, ""), feedKey); w.Code != http.StatusNoContent {
		t.Fatalf("feed %d %s", w.Code, w.Body)
	}
	probe := `{"t":%d,"ip":"2001:db8::7","ua":"x","method":"GET","path":"/.env","route":"","status":404}`
	page := `{"t":1,"ip":"192.0.2.1","method":"GET","path":"AS1","route":"GET /","status":200,"sf_dest":"document"}`
	if w := r.feed(feedBody(domain, 1, probe), feedKey); w.Code != http.StatusNoContent {
		t.Fatalf("probe %d %s", w.Code, w.Body)
	}
	if w := r.feed(feedBody(domain, 1, page), feedKey); w.Code != http.StatusNoContent {
		t.Fatalf("page %d %s", w.Code, w.Body)
	}
	waitEvents(t, r.st, 4)
	var tmpls []string
	rows, _ := r.st.DB.Query(`SELECT template || ' ' || path || ' ' || ip_key || ' ' || method FROM event ORDER BY id`)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		tmpls = append(tmpls, s)
	}
	_ = rows.Close()
	want := []string{
		"GET /api/v1/as/{asn} /api/v1/as/15169 203.0.113.42 GET",
		"GET /api/v1/as/{asn} /api/v1/as/15169 203.0.113.42 GET",
		"GET /.env /.env 2001:db8::/64 GET",
		"GET /AS:n /AS1 192.0.2.1 GET",
	}
	if strings.Join(tmpls, "\n") != strings.Join(want, "\n") {
		t.Errorf("templates:\n%s", strings.Join(tmpls, "\n"))
	}
	if s := r.stat(t, site); s.Accepted != 4 || s.AcceptedServer != 4 {
		t.Errorf("stat %+v", s)
	}
}

func TestFeed_Hostile(t *testing.T) {
	r := newRig(t, 0, true)
	site := r.siteID(domain)
	big := `{"v":1,"site":"` + domain + `","events":[{"ip":"192.0.2.1","ua":"` + strings.Repeat("x", MaxFeedBody) + `"}]}`
	for _, tt := range []struct {
		name, body, key string
		want            int
	}{
		{"no key", feedBody(domain, 1, ""), "", http.StatusUnauthorized},
		{"wrong key", feedBody(domain, 1, ""), "nope", http.StatusUnauthorized},
		{"other site", feedBody("other.example.com", 1, ""), feedKey, http.StatusBadRequest},
		{"version", strings.Replace(feedBody(domain, 1, ""), `"v":1`, `"v":0`, 1), feedKey, http.StatusBadRequest},
		{"not json", `{"v":`, feedKey, http.StatusBadRequest},
		{"oversized", big, feedKey, http.StatusRequestEntityTooLarge},
		{"too many", feedBody(domain, MaxFeedEvents+1, `{"t":1,"ip":"192.0.2.1"}`), feedKey, http.StatusRequestEntityTooLarge},
		{"bad ip", feedBody(domain, 1, `{"ip":"not-an-ip"}`), feedKey, http.StatusBadRequest},
		{"bad status", feedBody(domain, 1, `{"ip":"192.0.2.1","status":1000}`), feedKey, http.StatusBadRequest},
		{"long route", feedBody(domain, 1, `{"ip":"192.0.2.1","route":"`+strings.Repeat("r", 300)+`"}`), feedKey, http.StatusRequestEntityTooLarge},
	} {
		if w := r.feed(tt.body, tt.key); w.Code != tt.want {
			t.Errorf("%s: %d, want %d (%s)", tt.name, w.Code, tt.want, w.Body)
		} else if w.Code >= 400 {
			var e map[string]string
			if json.Unmarshal(w.Body.Bytes(), &e) != nil || e["detail"] == "" {
				t.Errorf("%s: body %q is not {detail}", tt.name, w.Body)
			}
		}
	}
	if s := r.stat(t, site); s.Accepted != 0 || s.Rejected < 7 {
		t.Errorf("stat %+v", s)
	}
	// Flooding one key: the burst passes, then 429 with Retry-After.
	var last *httptest.ResponseRecorder
	for range FeedBurst/MaxFeedEvents + 2 {
		last = r.feed(feedBody(domain, MaxFeedEvents, `{"t":1,"ip":"192.0.2.1"}`), feedKey)
	}
	if last.Code != http.StatusTooManyRequests || last.Header().Get("Retry-After") == "" {
		t.Errorf("flood: %d", last.Code)
	}
	if s := r.stat(t, site); s.RateLimited == 0 {
		t.Errorf("stat %+v", s)
	}
}

func TestCounters_Flush(t *testing.T) {
	r := newRig(t, 0, false)
	r.c.Counters.Add(1, func(s *store.IngestStat) { s.Accepted += 3 })
	if err := r.c.Counters.Flush(t.Context(), r.st); err != nil {
		t.Fatal(err)
	}
	if len(r.c.Counters.Pending()) != 0 {
		t.Error("not reset")
	}
	// A failed flush keeps the counts for the next one.
	r.c.Counters.Add(1, func(s *store.IngestStat) { s.Dropped += 2 })
	_ = r.st.Close()
	if err := r.c.Counters.Flush(t.Context(), r.st); err == nil {
		t.Fatal("flush on a closed db")
	}
	r.c.Counters.Add(1, func(s *store.IngestStat) { s.Dropped += 1 })
	if p := r.c.Counters.Pending(); len(p) != 1 || p[0].Dropped != 3 {
		t.Errorf("pending %+v", p)
	}
}

func TestEventTimeAndNames(t *testing.T) {
	recv := time.UnixMilli(10_000_000)
	for _, tt := range []struct {
		sent, t int64
		want    int64
	}{{0, 5, 10_000_000}, {100, 200, 10_000_000}, {1000, 900, 9_999_900}, {maxClientSkew.Milliseconds() * 3, 1, 10_000_000 - maxClientSkew.Milliseconds()}} {
		if got := eventTime(recv, tt.sent, tt.t); got != tt.want {
			t.Errorf("eventTime(%d,%d) = %d, want %d", tt.sent, tt.t, got, tt.want)
		}
	}
	p := map[string]any{"url": "https://x.example.org/a?b", "msg": "TypeError", "name": "LCP", "phase": "submit"}
	for k, want := range map[string]string{"js_error": "TypeError", "resource_error": "https://x.example.org/a",
		"vital": "LCP", "form": "submit", "download": "x.example.org", "search": ""} {
		if got := eventName(browserEvent{K: k}, p); got != want {
			t.Errorf("eventName(%s) = %q", k, got)
		}
	}
	if eventName(browserEvent{K: "outbound_click"}, map[string]any{"url": "mailto"}) != "mailto" {
		t.Error("hostless outbound")
	}
	if hostOf("%zz") != "" || truncate("abc", 2) != "ab" {
		t.Error("helpers")
	}
}
