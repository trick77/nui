package ingest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/sites"
	"github.com/trick77/trakk/backend/internal/store"
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
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "app.db"))
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
	// Refused before the site is known (size, JSON, version, no site) counts siteless; every refusal after the
	// lookup counts for the site.
	s := r.stat(t, site)
	if s.Accepted != 0 || s.Rejected != 15 {
		t.Errorf("site counters %+v, want 15 rejected", s)
	}
	if g := r.stat(t, 0); g.UnknownDomain != 1 || g.Rejected != 4 {
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
	// Rotating /64s within one /48 passes the narrow tier each time until the /48's 600 run out: 50 + 1 are spent
	// (the refused request paid its arrival token), 549 admit ten more batches of 50, the rest is refused.
	var codes []int
	for i := range 20 {
		codes = append(codes, r.browser(batch(domain, 50, ""), fmt.Sprintf("2001:db8:1:%x::1", 0x100+i), "").Code)
	}
	for i, c := range codes {
		want := http.StatusNoContent
		if i >= 10 {
			want = http.StatusTooManyRequests
		}
		if c != want {
			t.Errorf("rotating /64 %d: %d, want %d", i, c, want)
		}
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
	// The same-/64 batch and ten rotated ones, 50 events each.
	if s := r.stat(t, site); s.RateLimited != 550 || s.Accepted != 50+500+10+10+50 {
		t.Errorf("stat %+v, want 550 rate-limited", s)
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

// The agent a Web Bot Auth client names is kept: from the script's request as a header, from the feed as sig_agent,
// clipped there like every feed string.
func TestSignatureAgentKept(t *testing.T) {
	r := newRig(t, 0, true)
	req := httptest.NewRequest(http.MethodPost, "/api/in", strings.NewReader(batch(domain, 1, "")))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) Safari/605.1.15")
	req.Header.Set("Signature-Agent", `"https://agent.example.com"`)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("Origin", "https://"+domain)
	w := httptest.NewRecorder()
	r.c.HandleBrowser(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("browser %d %s", w.Code, w.Body)
	}
	long := strings.Repeat("a", 400)
	ev := `{"t":1,"ip":"192.0.2.1","method":"GET","path":"/","status":200,"sig_agent":"` + long + `"}`
	if w := r.feed(feedBody(domain, 1, ev), feedKey); w.Code != http.StatusNoContent {
		t.Fatalf("feed %d %s", w.Code, w.Body)
	}
	waitEvents(t, r.st, 2)
	var browser, server string
	_ = r.st.DB.QueryRow(`SELECT props FROM event WHERE source = 'browser'`).Scan(&browser)
	_ = r.st.DB.QueryRow(`SELECT props FROM event WHERE source = 'server'`).Scan(&server)
	if !strings.Contains(browser, `"signature-agent":"\"https://agent.example.com\""`) {
		t.Errorf("browser props %s", browser)
	}
	if !strings.Contains(server, `"sig_agent":"`+long[:feedSigAgent]+`"`) || strings.Contains(server, long[:feedSigAgent+1]) {
		t.Errorf("server props %s", server)
	}
}

// A beacon's Origin is kept with its headers; one without is still taken, and stores none.
func TestOriginKept(t *testing.T) {
	r := newRig(t, 0, true)
	for i, origin := range []string{"https://" + domain, ""} {
		if w := r.browser(batch(domain, 1, ""), fmt.Sprintf("203.0.113.%d", 20+i), origin); w.Code != http.StatusNoContent {
			t.Fatalf("origin %q: %d %s", origin, w.Code, w.Body)
		}
		waitEvents(t, r.st, i+1)
	}
	_, props := storedRows(t, r.st)
	if len(props) != 2 || !strings.Contains(props[0], `"origin":"https://`+domain+`"`) || strings.Contains(props[1], `"origin"`) {
		t.Errorf("props %q", props)
	}
}

// op and err are names: cleaned and clipped, never refused, and a field Trakk does not know (an emitter newer than
// it) is ignored.
func TestFeed_OperationAndFailure(t *testing.T) {
	r := newRig(t, 0, true)
	for _, ev := range []string{
		`{"t":1,"ip":"192.0.2.1","method":"POST","path":"/mcp","route":"POST /mcp","status":200,"op":"whois",` +
			`"err":"invalid_params","field_of_tomorrow":{"x":[1]}}`,
		`{"t":1,"ip":"192.0.2.1","method":"POST","path":"/mcp","route":"POST /mcp","status":200,` +
			`"op":"ig nore <b>\u0000é","err":"` + strings.Repeat("é", 100) + `"}`,
		`{"t":1,"ip":"192.0.2.1","method":"POST","path":"/mcp","route":"POST /mcp","status":200}`,
	} {
		if w := r.feed(feedBody(domain, 1, ev), feedKey); w.Code != http.StatusNoContent {
			t.Fatalf("feed %d %s", w.Code, w.Body)
		}
	}
	waitEvents(t, r.st, 3)
	var got []string
	rows, err := r.st.DB.Query(`SELECT op || '|' || err FROM event ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		got = append(got, s)
	}
	want := []string{"whois|invalid_params", "ig_nore__b___|" + strings.Repeat("_", feedName/2), "|"}
	if !slices.Equal(got, want) {
		t.Errorf("stored %q, want %q", got, want)
	}
}

// A request no route took is marked so: no route, or the catch-all's answer for another path. "/" asked for itself is a
// route.
func TestFeed_Unrouted(t *testing.T) {
	r := newRig(t, 0, true)
	for _, ev := range []string{
		`{"t":1,"ip":"192.0.2.1","method":"POST","path":"/","route":"POST /","status":401}`,
		`{"t":1,"ip":"192.0.2.1","method":"GET","path":"/admin","route":"GET /","status":401}`,
		`{"t":1,"ip":"192.0.2.1","method":"GET","path":"/mcp","route":"","status":405}`,
		`{"t":1,"ip":"192.0.2.1","method":"GET","path":"/users/1","route":"GET /users/{id}","status":401}`,
		`{"t":1,"ip":"192.0.2.1","method":"GET","path":"/users/2","route":"GET /users/{id}","status":404}`,
	} {
		if w := r.feed(feedBody(domain, 1, ev), feedKey); w.Code != http.StatusNoContent {
			t.Fatalf("feed %d %s", w.Code, w.Body)
		}
	}
	waitEvents(t, r.st, 5)
	var got string
	if err := r.st.DB.QueryRow(`SELECT group_concat(unrouted, '') FROM (SELECT unrouted FROM event ORDER BY id)`).Scan(&got); err != nil ||
		got != "01101" {
		t.Errorf("unrouted %s, %v, want 01101", got, err)
	}
}

// One crafted request of the feeding product's public clients must not blind the feed: over-long fields are clipped
// on a rune boundary, unusable events dropped and counted, the rest of the batch stored.
func TestFeed_PoisonedBatch(t *testing.T) {
	r := newRig(t, 0, true)
	site := r.siteID(domain)
	good := `{"t":%d,"ip":"192.0.2.10","ua":"Mozilla/5.0","method":"GET","path":"/","route":"GET /","status":200}`
	events := []string{
		good, good,
		// A method of 40 bytes, a 60-character Sec-Fetch-Site, a route of 300.
		`{"ip":"192.0.2.11","method":"` + strings.Repeat("M", 40) + `","sf_site":"` + strings.Repeat("s", 60) +
			`","route":"GET /` + strings.Repeat("r", 300) + `","status":200}`,
		// A user agent of invalid UTF-8: every byte decodes to U+FFFD, three bytes each.
		`{"ip":"192.0.2.12","ua":"` + strings.Repeat("\xff", 400) + `","status":200}`,
		// Two-byte runes across the 512-byte cut, and a 3000-byte path.
		`{"ip":"192.0.2.13","ua":"` + strings.Repeat("é", 300) + `","path":"/` + strings.Repeat("p", 3000) + `"}`,
		// Unusable: no address, a bad address, an impossible status, a negative duration.
		`{"ua":"x"}`, `{"ip":"not-an-ip"}`, `{"ip":"192.0.2.14","status":1000}`, `{"ip":"192.0.2.15","dur_ms":-1}`,
	}
	for i := range events {
		events[i] = strings.ReplaceAll(events[i], "%d", fmt.Sprint(time.Now().UnixMilli()))
	}
	body := `{"v":1,"site":"` + domain + `","events":[` + strings.Join(events, ",") + `]}`
	if w := r.feed(body, feedKey); w.Code != http.StatusNoContent {
		t.Fatalf("poisoned batch: %d %s", w.Code, w.Body)
	}
	waitEvents(t, r.st, 5)
	rows, err := r.st.DB.Query(`SELECT ip, method, ua, path, template, props FROM event ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var ip, method, ua, path, tmpl, props string
		if err := rows.Scan(&ip, &method, &ua, &path, &tmpl, &props); err != nil {
			t.Fatal(err)
		}
		n++
		if len(method) > feedMethod || len(ua) > maxString || len(path) > maxURL+1 || !utf8.ValidString(ua) {
			t.Errorf("%s: method %d, ua %d (valid %v), path %d bytes", ip, len(method), len(ua), utf8.ValidString(ua),
				len(path))
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(props), &p); err != nil {
			t.Errorf("%s: props %q", ip, props)
		}
		if s, _ := p["sf_site"].(string); len(s) > feedSF {
			t.Errorf("%s: sf_site %d bytes", ip, len(s))
		}
		switch ip {
		case "192.0.2.12":
			if ua != strings.Repeat("�", maxString/3) {
				t.Errorf("invalid UTF-8 ua clipped to %q", ua)
			}
		case "192.0.2.13":
			if ua != strings.Repeat("é", maxString/2) {
				t.Errorf("two-byte ua clipped to %d bytes", len(ua))
			}
		}
	}
	if n != 5 {
		t.Errorf("stored %d events, want 5", n)
	}
	if s := r.stat(t, site); s.Accepted != 5 || s.Rejected != 4 {
		t.Errorf("stat %+v, want 5 accepted and 4 rejected", s)
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
	if s := r.stat(t, site); s.Accepted != 0 || s.Rejected != 5 {
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

// The feed's Retry-After covers the retry: the refused batch, sent again once it has passed, is taken.
func TestFeed_RetryAfterCoversTheRetry(t *testing.T) {
	r := newRig(t, 0, true)
	body := feedBody(domain, MaxFeedEvents, `{"t":1,"ip":"192.0.2.1"}`)
	var w *httptest.ResponseRecorder
	for w = r.feed(body, feedKey); w.Code != http.StatusTooManyRequests; w = r.feed(body, feedKey) {
	}
	secs, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || secs < 1 {
		t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
	}
	r.advance(time.Duration(secs) * time.Second)
	if w := r.feed(body, feedKey); w.Code != http.StatusNoContent {
		t.Errorf("retry after Retry-After: %d", w.Code)
	}
}

// A batch refused for the rate is sent again whole: its dropped events are counted when it is taken, once.
func TestFeed_DroppedCountedOnce(t *testing.T) {
	r := newRig(t, 0, true)
	site := r.siteID(domain)
	// Spend the key's burst.
	for r.feed(feedBody(domain, MaxFeedEvents, `{"t":1,"ip":"192.0.2.1"}`), feedKey).Code != http.StatusTooManyRequests {
	}
	before := r.stat(t, site).Rejected
	body := `{"v":1,"site":"` + domain + `","events":[{"ip":"192.0.2.2"},{"ip":"bad"},{"ua":"no address"}]}`
	for range 3 {
		if w := r.feed(body, feedKey); w.Code != http.StatusTooManyRequests {
			t.Fatalf("retry: %d", w.Code)
		}
	}
	if got := r.stat(t, site).Rejected - before; got != 0 {
		t.Errorf("refused retries counted %d rejected", got)
	}
	r.advance(time.Hour)
	if w := r.feed(body, feedKey); w.Code != http.StatusNoContent {
		t.Fatalf("taken: %d", w.Code)
	}
	if got := r.stat(t, site).Rejected - before; got != 2 {
		t.Errorf("rejected %d, want the batch's 2 once", got)
	}
}

// An address's IPv6 zone is free text netip accepts: dropped on both paths, never stored or keyed.
func TestAddressZoneDropped(t *testing.T) {
	r := newRig(t, 0, true)
	long := `{"t":%d,"ip":"2001:db8::9%` + strings.Repeat("z", 100_000) + `","status":200}`
	short := `{"t":%d,"ip":"::ffff:192.0.2.9%eth0","status":200}`
	if w := r.feed(`{"v":1,"site":"`+domain+`","events":[`+
		strings.ReplaceAll(long, "%d", fmt.Sprint(time.Now().UnixMilli()))+","+
		strings.ReplaceAll(short, "%d", fmt.Sprint(time.Now().UnixMilli()))+`]}`, feedKey); w.Code != http.StatusNoContent {
		t.Fatalf("feed: %d %s", w.Code, w.Body)
	}
	if w := r.browser(batch(domain, 1, ""), "2001:db8::5%eth0", ""); w.Code != http.StatusNoContent {
		t.Fatalf("browser: %d %s", w.Code, w.Body)
	}
	waitEvents(t, r.st, 3)
	rows, err := r.st.DB.Query(`SELECT ip, ip_key FROM event ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var ip, key string
		_ = rows.Scan(&ip, &key)
		got = append(got, ip+" "+key)
	}
	want := []string{"2001:db8::9 2001:db8::/64", "192.0.2.9 192.0.2.9", "2001:db8::5 2001:db8::/64"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("stored:\n%s", strings.Join(got, "\n"))
	}
}

// storedRows is every stored row's size (store.Event.Size over the columns) and props, in id order.
func storedRows(t *testing.T, st *store.Store) ([]int, []string) {
	t.Helper()
	rows, err := st.DB.Query(`SELECT source, kind, name, visitor, sid, ip, ip_key, ua, path, template, url, ref,
		ref_host, method, country, account, props FROM event ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var sizes []int
	var props []string
	for rows.Next() {
		var e store.Event
		if err := rows.Scan(&e.Source, &e.Kind, &e.Name, &e.Visitor, &e.SID, &e.IP, &e.IPKey, &e.UA, &e.Path,
			&e.Template, &e.URL, &e.Ref, &e.RefHost, &e.Method, &e.Country, &e.Account, &e.Props); err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, e.Size())
		props = append(props, e.Props)
	}
	return sizes, props
}

// The caller's address follows the configured proxy hops: with none the header is the caller's own text and
// ignored; behind one, an entry that is no address falls back to the peer, never refusing or keying on the text.
func TestBrowser_ClientAddressHops(t *testing.T) {
	r := newRig(t, 0, true)
	post := func(peer, xff string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/in", strings.NewReader(batch(domain, 1, "")))
		req.RemoteAddr = peer
		req.Header.Set("X-Forwarded-For", xff)
		w := httptest.NewRecorder()
		r.c.HandleBrowser(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("%s via %s: %d", xff, peer, w.Code)
		}
	}
	post("10.0.0.2:1234", "not-an-address")
	r.c.TrustedHops, r.c.Browser = 0, BrowserTiers(0, r.clock)
	post("192.0.2.77:1234", "203.0.113.9")
	waitEvents(t, r.st, 2)
	rows, err := r.st.DB.Query(`SELECT ip, ip_key FROM event ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var ip, key string
		if err := rows.Scan(&ip, &key); err != nil {
			t.Fatal(err)
		}
		got = append(got, ip+"|"+key)
	}
	if want := []string{"10.0.0.2|10.0.0.2", "192.0.2.77|192.0.2.77"}; !slices.Equal(got, want) {
		t.Errorf("stored %v, want %v", got, want)
	}
}

// What one token stores is bounded whatever the request: caller-named headers are not kept, the encoding does not
// inflate <, > and &, and a batch whose rows would outgrow the bounds is refused.
func TestBrowser_StoredBytesPerToken(t *testing.T) {
	r := newRig(t, 0, true)
	// 490 headers named like client hints, 512 bytes each, on a one-event batch.
	req := httptest.NewRequest(http.MethodPost, "/api/in", strings.NewReader(batch(domain, 1, "")))
	req.Header.Set("X-Forwarded-For", "198.51.100.30")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	for i := range 490 {
		req.Header.Set(fmt.Sprintf("Sec-Ch-Ua-X%d", i), strings.Repeat("h", 512))
	}
	w := httptest.NewRecorder()
	r.c.HandleBrowser(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("header flood: %d", w.Code)
	}
	// A batch of < characters: 8 events, 3.5 KB of them in each one's props.
	var p []string
	for i := range 7 {
		p = append(p, fmt.Sprintf(`"k%d":"%s"`, i, strings.Repeat("<", 500)))
	}
	lt := strings.ReplaceAll(batch(domain, 8, ""), `"p":{}`, `"p":{`+strings.Join(p, ",")+`}`)
	if w := r.browser(lt, "198.51.100.31", ""); w.Code != http.StatusNoContent {
		t.Fatalf("< batch: %d", w.Code)
	}
	waitEvents(t, r.st, 9)
	sizes, props := storedRows(t, r.st)
	if sizes[0] > 2*StoreUnit || strings.Contains(props[0], "sec-ch-ua-x") || !strings.Contains(props[0], `"sec-fetch-mode":"cors"`) {
		t.Errorf("header flood stored %d bytes: %s", sizes[0], props[0])
	}
	total := 0
	for i, s := range sizes[1:] {
		total += s
		if s > 4<<10 || strings.Contains(props[i+1], `\`+`u003c`) || !strings.Contains(props[i+1], "<<<") {
			t.Errorf("< event %d stored %d bytes", i, s)
		}
	}
	// The batch was charged for what it stores: the bytes per token stay within StoreUnit.
	if cost := browserCost(8, total); total > cost*StoreUnit || cost <= 8 {
		t.Errorf("< batch stored %d bytes for %d tokens", total, cost)
	}
	// 12 events naming a 1.9 KB URL each store twice the body: charged for the bytes, so the second such batch
	// passes and the third does not (by the event count alone 36 of 60 tokens would be spent).
	long := `,"url":"https://` + domain + `/` + strings.Repeat("u", 1900) + `"`
	body := `{"v":1,"site":"` + domain + `","events":[` + strings.TrimSuffix(strings.Repeat(`{"k":"engagement"`+long+`},`, 12), ",") + `]}`
	for i, want := range []int{http.StatusNoContent, http.StatusNoContent, http.StatusTooManyRequests} {
		if w := r.browser(body, "198.51.100.32", ""); w.Code != want {
			t.Errorf("long-URL batch %d: %d, want %d", i, w.Code, want)
		}
	}
	// 50 events of 17 bytes each, under a 2 KB page URL and referrer: every page view stores both, 300 KB from a
	// 5 KB body. Over the batch bound: refused.
	ctxLong := `"url":"https://` + domain + `/` + strings.Repeat("u", 2000) + `","ref":"https://ref.example.org/` +
		strings.Repeat("r", 2000) + `"`
	body = `{"v":1,"site":"` + domain + `","ctx":{` + ctxLong + `},"events":[` +
		strings.TrimSuffix(strings.Repeat(`{"k":"page_view"},`, 50), ",") + `]}`
	if w := r.browser(body, "198.51.100.33", ""); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("amplifying batch: %d", w.Code)
	}
	// The bounds keep the largest charge under the burst: a byte charge never becomes a 413.
	if browserCost(1, maxBatchBytes) >= BrowserBurst {
		t.Errorf("max byte charge %d", browserCost(1, maxBatchBytes))
	}
}

// The largest event the tracker sends may come first in its batch, under the largest context and every header a
// browser could send: the event's own bound leaves out what the batch stores once, and the headers have a bound of
// their own, so the tracker can budget a batch it cannot see all of.
func TestBrowser_FirstRowBounds(t *testing.T) {
	r := newRig(t, 0, true)
	var langs []string
	for range 7 {
		langs = append(langs, `"`+strings.Repeat("l", 500)+`"`)
	}
	var p []string
	for i := range 7 {
		p = append(p, fmt.Sprintf(`"k%d":"%s"`, i, strings.Repeat(" ", 170)))
	}
	page := `https://` + domain + `/` + strings.Repeat("p", 2000)
	body := `{"v":1,"site":"` + domain + `","vid":"` + strings.Repeat("a", 32) + `","sid":"` + strings.Repeat("b", 32) +
		`","ctx":{"url":"` + page + `","title":"t","langs":[` + strings.Join(langs, ",") + `]},"events":[` +
		`{"k":"custom","n":"` + strings.Repeat("n", 128) + `","url":"` + page + `x","title":"` +
		strings.Repeat(`\u0001`, 500) + `","p":{` + strings.Join(p, ",") + `}},{"k":"engagement","p":{}}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/in", strings.NewReader(body))
	req.Header.Set("X-Forwarded-For", "198.51.100.40")
	req.Header.Set("User-Agent", strings.Repeat("u", 600))
	for _, name := range browserHeaders {
		req.Header.Set(name, strings.Repeat(`"`, 300))
	}
	req.Header.Set("Origin", "https://"+domain) // any other is refused
	w := httptest.NewRecorder()
	r.c.HandleBrowser(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("largest first event: %d", w.Code)
	}
	waitEvents(t, r.st, 2)
	sizes, props := storedRows(t, r.st)
	var stored struct {
		Ctx json.RawMessage   `json:"ctx"`
		Hdr map[string]string `json:"hdr"`
	}
	if err := json.Unmarshal([]byte(props[0]), &stored); err != nil {
		t.Fatal(err)
	}
	hdr := encodeJSON(stored.Hdr)
	if len(hdr) > maxHeaders || len(stored.Hdr) == 0 || len(stored.Hdr) == len(browserHeaders) {
		t.Errorf("headers stored %d bytes, %d of them", len(hdr), len(stored.Hdr))
	}
	if _, ok := stored.Hdr["accept-language"]; !ok {
		t.Error("the first header in the list is kept first")
	}
	// The row holds more than an event's bound: its own part stays within it.
	if own := sizes[0] - len(stored.Ctx) - len(hdr); sizes[0] <= maxEventBytes || own > maxEventBytes {
		t.Errorf("first row %d bytes, its own %d", sizes[0], own)
	}
}

// A batch refused for the rest of its charge passes when sent again once its Retry-After has passed: the wait
// covers the token the retry pays on arrival too.
func TestBrowser_RetryAfterCoversTheRetry(t *testing.T) {
	r := newRig(t, 0, true)
	long := `,"url":"https://` + domain + `/` + strings.Repeat("u", 1900) + `"`
	body := `{"v":1,"site":"` + domain + `","events":[` +
		strings.TrimSuffix(strings.Repeat(`{"k":"engagement"`+long+`},`, 12), ",") + `]}`
	var w *httptest.ResponseRecorder
	for range 3 {
		if w = r.browser(body, "198.51.100.50", ""); w.Code == http.StatusTooManyRequests {
			break
		}
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("no 429: %d", w.Code)
	}
	ra, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || ra < 1 {
		t.Fatalf("Retry-After %q", w.Header().Get("Retry-After"))
	}
	r.advance(time.Duration(ra) * time.Second)
	if w := r.browser(body, "198.51.100.50", ""); w.Code != http.StatusNoContent {
		t.Errorf("sent again after %d s: %d", ra, w.Code)
	}
}

// readFlag is a body that records whether anyone read it.
type readFlag struct {
	r    io.Reader
	read bool
}

func (f *readFlag) Read(p []byte) (int, error) { f.read = true; return f.r.Read(p) }

// A caller without tokens is refused before its body is read: refusing is cheap, and every request still pays.
func TestRefusedBeforeParsing(t *testing.T) {
	r := newRig(t, 0, false)
	for range BrowserBurst {
		r.browser(`{nope`, "203.0.113.77", "")
	}
	body := &readFlag{r: strings.NewReader(batch(domain, 1, ""))}
	req := httptest.NewRequest(http.MethodPost, "/api/in", body)
	req.Header.Set("X-Forwarded-For", "203.0.113.77")
	w := httptest.NewRecorder()
	r.c.HandleBrowser(w, req)
	if w.Code != http.StatusTooManyRequests || body.read {
		t.Errorf("browser: %d, body read %v", w.Code, body.read)
	}
	// The feed: a key without tokens left is refused unread too.
	for range FeedBurst/MaxFeedEvents + 1 {
		r.feed(feedBody(domain, MaxFeedEvents, `{"t":1,"ip":"192.0.2.1"}`), feedKey)
	}
	body = &readFlag{r: strings.NewReader(feedBody(domain, 1, ""))}
	req = httptest.NewRequest(http.MethodPost, "/api/feed", body)
	req.Header.Set("Authorization", "Bearer "+feedKey)
	w = httptest.NewRecorder()
	r.c.HandleFeed(w, req)
	if w.Code != http.StatusTooManyRequests || body.read || w.Header().Get("Retry-After") == "" {
		t.Errorf("feed: %d, body read %v", w.Code, body.read)
	}
}

// A body trickled in holds its handler no longer than the read deadline, and is counted.
func TestSlowBody(t *testing.T) {
	r := newRig(t, 0, false)
	r.c.BodyTimeout = 200 * time.Millisecond
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/in", r.c.HandleBrowser)
	mux.HandleFunc("POST /api/feed", r.c.HandleFeed)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	for _, tt := range []struct{ path, hdr string }{
		{"/api/in", "Origin: https://" + domain + "\r\n"},
		{"/api/feed", "Authorization: Bearer " + feedKey + "\r\n"},
	} {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, _ = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: x\r\n%sContent-Length: 1000\r\n\r\n{\"v\":1,", tt.path, tt.hdr)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		_ = conn.Close()
		if err != nil {
			t.Fatalf("%s: %v", tt.path, err)
		}
		if resp.StatusCode != http.StatusRequestTimeout || time.Since(start) > 3*time.Second {
			t.Errorf("%s: %d after %v", tt.path, resp.StatusCode, time.Since(start))
		}
	}
	if s := r.stat(t, r.siteID(domain)); s.Rejected != 2 {
		t.Errorf("stat %+v, want both slow bodies counted", s)
	}
}

// One event of the wrong JSON shape is dropped and counted; the batch around it is stored. A wrong key is counted
// where every footer shows it.
func TestFeed_PerEvent(t *testing.T) {
	r := newRig(t, 0, true)
	site := r.siteID(domain)
	good := `{"t":%d,"ip":"192.0.2.10","ua":"Mozilla/5.0","method":"GET","path":"/","route":"GET /","status":200}`
	evs := []string{good, `{"ip":"192.0.2.11","status":"200"}`, `1`, `{"ip":["192.0.2.12"]}`, good}
	for i := range evs {
		evs[i] = strings.ReplaceAll(evs[i], "%d", fmt.Sprint(time.Now().UnixMilli()))
	}
	if w := r.feed(`{"v":1,"site":"`+domain+`","events":[`+strings.Join(evs, ",")+`]}`, feedKey); w.Code != http.StatusNoContent {
		t.Fatalf("batch: %d %s", w.Code, w.Body)
	}
	waitEvents(t, r.st, 2)
	if s := r.stat(t, site); s.Accepted != 2 || s.Rejected != 3 {
		t.Errorf("stat %+v, want 2 accepted, 3 rejected", s)
	}
	// The envelope itself still refuses: events that are no array.
	if w := r.feed(`{"v":1,"site":"`+domain+`","events":{}}`, feedKey); w.Code != http.StatusBadRequest {
		t.Errorf("events object: %d", w.Code)
	}
	r.feed(feedBody(domain, 1, ""), "wrong")
	if g := r.stat(t, 0); g.UnknownDomain != 1 || g.Rejected != 0 {
		t.Errorf("wrong key counted as %+v", g)
	}
}

// The queue is bounded in bytes as well as events: a few large events fill it.
func TestWriter_QueueBytes(t *testing.T) {
	r := newRig(t, 0, false)
	w := r.c.Writer
	big := store.Event{SiteID: 1, Props: strings.Repeat("x", 1<<20)}
	n := 0
	for w.Enqueue([]store.Event{big}) {
		n++
		if n > 100 {
			t.Fatal("no byte bound")
		}
	}
	if want := DefaultQueueBytes / weight([]store.Event{big}); int64(n) != want {
		t.Errorf("queued %d 1 MiB events, want %d", n, want)
	}
	if w.QueuedBytes() > DefaultQueueBytes || w.Queued() != int64(n) {
		t.Errorf("queued %d events, %d bytes", w.Queued(), w.QueuedBytes())
	}
	// A refused batch leaves nothing behind.
	before := w.QueuedBytes()
	if w.Enqueue([]store.Event{big}) || w.QueuedBytes() != before {
		t.Error("refused batch counted")
	}
	// Refused by the event bound: the bytes stay exact too.
	small := newRig(t, 2, false).c.Writer
	ev := store.Event{SiteID: 1, Props: "{}"}
	small.Enqueue([]store.Event{ev, ev})
	for range 3 {
		if small.Enqueue([]store.Event{ev}) {
			t.Fatal("past the event bound")
		}
	}
	if small.QueuedBytes() != weight([]store.Event{ev, ev}) {
		t.Errorf("bytes %d after event-bound refusals, want %d", small.QueuedBytes(), weight([]store.Event{ev, ev}))
	}
}

// What the writer writes, enriched, it gives back to the queue exactly.
func TestWriter_BytesBalance(t *testing.T) {
	r := newRig(t, 0, true) // the rig's enrich adds a country
	if w := r.browser(batch(domain, 3, ""), "192.0.2.70", ""); w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
	waitEvents(t, r.st, 3)
	deadline := time.Now().Add(5 * time.Second)
	for r.c.Writer.Queued() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.c.Writer.QueuedBytes() != 0 || r.c.Writer.Queued() != 0 {
		t.Errorf("after the write: %d events, %d bytes queued", r.c.Writer.Queued(), r.c.Writer.QueuedBytes())
	}
}

// Shutdown during a write: the write under way completes and the queue is drained; nothing is dropped.
func TestWriter_ShutdownDuringWrite(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rows, err := st.SyncSites(t.Context(), []store.Site{{Domain: domain, Kind: "site", PathRules: "[]"}})
	if err != nil {
		t.Fatal(err)
	}
	site := rows[domain].ID
	ctx, cancel := context.WithCancel(context.Background())
	inWrite := make(chan struct{})
	var once sync.Once
	counters := NewCounters(nil)
	// The first enriched event stops the world: shutdown begins while its write is under way.
	w := NewWriter(st, counters, 0, func(*store.Event) {
		once.Do(func() { close(inWrite); cancel(); time.Sleep(50 * time.Millisecond) })
	}, quiet())
	go w.Run(ctx)
	ev := store.Event{SiteID: site, TS: time.Now().UnixMilli(), Source: "browser", Kind: "page_view", IP: "192.0.2.1",
		IPKey: "192.0.2.1"}
	w.Enqueue([]store.Event{ev, ev, ev})
	<-inWrite
	w.Enqueue([]store.Event{ev, ev})
	<-w.Done()
	var n int
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM event`).Scan(&n)
	if n != 5 {
		t.Errorf("stored %d of 5 events", n)
	}
	for _, s := range counters.Pending() {
		if s.Dropped != 0 {
			t.Errorf("dropped %+v", s)
		}
	}
}

// A failed chunk is retried; only one that keeps failing is dropped and counted.
func TestWriter_Retry(t *testing.T) {
	r := newRig(t, 0, false)
	w := r.c.Writer
	w.retryWait = time.Millisecond
	fails := 2
	var calls int
	w.insert = func(ctx context.Context, evs []store.Event) error {
		calls++
		if calls <= fails {
			return fmt.Errorf("database is locked")
		}
		return r.st.InsertEvents(ctx, evs)
	}
	ev := store.Event{SiteID: r.siteID(domain), TS: 1, Source: "browser", Kind: "page_view", IP: "192.0.2.1",
		IPKey: "192.0.2.1"}
	w.Enqueue([]store.Event{ev})
	w.write(t.Context(), []store.Event{ev}, nil)
	if calls != 3 {
		t.Errorf("%d attempts", calls)
	}
	waitEvents(t, r.st, 1)
	// Failing every attempt: dropped and counted after writeAttempts.
	calls, fails = 0, 100
	w.write(t.Context(), []store.Event{ev, ev}, nil)
	if calls != writeAttempts {
		t.Errorf("%d attempts, want %d", calls, writeAttempts)
	}
	if s := r.stat(t, r.siteID(domain)); s.Dropped != 2 {
		t.Errorf("stat %+v", s)
	}
}

// A write that fails for good takes its events back from the counters they were counted in: the hour of the count,
// the server's share too, after a flush as well. Stored, no counter goes below zero or the server's above the whole.
func TestWriter_FailureCorrectsItsHour(t *testing.T) {
	r := newRig(t, 0, false)
	w := r.c.Writer
	w.retryWait = time.Millisecond
	w.insert = func(context.Context, []store.Event) error { return fmt.Errorf("disk I/O error") }
	site := r.siteID(domain)
	hour := r.clock().Unix() / 3600
	if c := r.feed(feedBody(domain, 2, ""), feedKey); c.Code != http.StatusNoContent {
		t.Fatalf("feed: %d", c.Code)
	}
	if err := r.c.Counters.Flush(t.Context(), r.st); err != nil { // the count is stored before the write fails
		t.Fatal(err)
	}
	r.advance(time.Hour)
	b := <-w.ch
	board := alert.NewBoard(nil)
	w.Report = board
	w.write(t.Context(), b.evs, []int64{b.hour, b.hour})
	if cs := board.Conditions(); len(cs) != 1 || cs[0].Key != alert.JobWriter || !cs[0].Open ||
		!strings.Contains(cs[0].Text, "disk I/O error") {
		t.Errorf("reported %+v", cs)
	}
	if err := r.c.Counters.Flush(t.Context(), r.st); err != nil {
		t.Fatal(err)
	}
	rows, err := r.st.DB.Query(`SELECT hour, accepted, accepted_server, dropped FROM ingest_stat WHERE site_id = ?
		ORDER BY hour`, site)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var h, acc, srv, drop int64
		_ = rows.Scan(&h, &acc, &srv, &drop)
		got = append(got, fmt.Sprintf("%d %d %d %d", h-hour, acc, srv, drop))
	}
	if strings.Join(got, ",") != "0 0 0 2" {
		t.Errorf("hour, accepted, server, dropped = %v, want the count's hour corrected", got)
	}
	// Stored deltas are clamped: never below zero, the server's share never above the whole.
	if err := r.st.AddStats(t.Context(), []store.IngestStat{{SiteID: site, Hour: hour + 5, Accepted: -3,
		AcceptedServer: 4, Dropped: -1}, {SiteID: site, Hour: hour, Accepted: 1, AcceptedServer: 3}}); err != nil {
		t.Fatal(err)
	}
	var acc, srv, drop int64
	_ = r.st.DB.QueryRow(`SELECT accepted, accepted_server, dropped FROM ingest_stat WHERE site_id = ? AND hour = ?`,
		site, hour+5).Scan(&acc, &srv, &drop)
	if acc != 0 || srv != 0 || drop != 0 {
		t.Errorf("negative deltas stored as %d %d %d", acc, srv, drop)
	}
	_ = r.st.DB.QueryRow(`SELECT accepted, accepted_server FROM ingest_stat WHERE site_id = ? AND hour = ?`,
		site, hour).Scan(&acc, &srv)
	if acc != 1 || srv != 1 {
		t.Errorf("server share %d of %d", srv, acc)
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
	// A name is at most 200 bytes, whatever host a link names: the tracker budgets a row by it.
	long := map[string]any{"url": "https://" + strings.Repeat("h", 1000) + ".example.org/"}
	if n := eventName(browserEvent{K: "outbound_click"}, long); len(n) != 200 {
		t.Errorf("long host: %d bytes", len(n))
	}
	if hostOf("%zz") != "" || truncate("abc", 2) != "ab" {
		t.Error("helpers")
	}
}

// The oldest time the collector dates an event (the feed's hour, the browser's skew, from receipt) lies on today or
// yesterday (UTC): among the days the raw purge and the size cap never touch (store.CapTail), so no write lands in a
// day being purged.
func TestBackdate_WithinRawTail(t *testing.T) {
	if back := max(maxFeedAge, maxClientSkew); back >= 24*time.Hour || store.CapTail < 2 {
		t.Errorf("events dated up to %v back can land before yesterday, the purge's tail is %d days", back,
			store.CapTail)
	}
}
