package classify

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trick77/nui/backend/internal/refdata"
	"github.com/trick77/nui/backend/internal/sites"
	"github.com/trick77/nui/backend/internal/store"
)

const crawlers = `{"services":{
 "Googlebot":{"ipv4":["66.249.64.0/19"],"ip_list_authoritative":true,"user_agent_patterns":["*Googlebot*"]},
 "Bingbot":{"ipv4":["157.55.39.0/24"],"ip_list_authoritative":false,"user_agent_patterns":["*bingbot*"],
   "rdns_patterns":["*.search.msn.com"]},
 "UAOnly":{"user_agent_patterns":["*SomeCrawler*"]}}}`

const monitoring = `{"services":{"UptimeRobot":{"ipv4":["69.162.124.224/28"],"ip_list_authoritative":true,
 "user_agent_patterns":["*UptimeRobot*"]}}}`

type resolver struct{}

func (resolver) LookupAddr(_ context.Context, a string) ([]string, error) {
	if a == "157.55.39.1" {
		return []string{"msnbot-1.search.msn.com."}, nil
	}
	return nil, errors.New("nx")
}

func (resolver) LookupHost(_ context.Context, h string) ([]string, error) {
	if h == "msnbot-1.search.msn.com." {
		return []string{"157.55.39.1"}, nil
	}
	return nil, errors.New("nx")
}

type rig struct {
	st   *store.Store
	c    *Classifier
	site *sites.Site
	now  time.Time
}

func newRig(t *testing.T, feed bool) *rig {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "nui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hash := ""
	if feed {
		hash = sites.HashKey("k")
	}
	rows, err := st.SyncSites(t.Context(), []store.Site{{Domain: "app.example.com", Kind: "site",
		FeedKeyHash: hash, PathRules: "[]"}})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := sites.New(rows)
	if err != nil {
		t.Fatal(err)
	}
	ref := refdata.New(refdata.Options{Dir: t.TempDir()})
	b, err := refdata.ParseBots([]byte(crawlers), []byte(monitoring))
	if err != nil {
		t.Fatal(err)
	}
	ref.SetBots(b)
	now := time.Now().UTC()
	r := &rig{st: st, now: now}
	r.site, _ = reg.Lookup("app.example.com")
	r.c = &Classifier{DB: st, Sites: reg, Ref: ref, Verifier: refdata.NewVerifier(resolver{}, time.Second),
		Now: func() time.Time { return r.now }}
	return r
}

func (r *rig) insert(t *testing.T, evs ...store.Event) {
	t.Helper()
	for i := range evs {
		evs[i].SiteID = r.site.ID
		if evs[i].TS == 0 {
			evs[i].TS = r.now.UnixMilli() - int64(i)
		}
		if evs[i].IPKey == "" {
			evs[i].IPKey = evs[i].IP
		}
		r.c.Enrich(&evs[i])
	}
	if err := r.st.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
}

const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/18.0 Safari/605.1.15"

func req(ip, ua, name, path string, status int, props string) store.Event {
	if props == "" {
		props = `{"sf_mode":"navigate","lang":"de"}`
	}
	return store.Event{Source: "server", Kind: "request", IP: ip, UA: ua, Name: name, Path: path, Status: status,
		Props: props}
}

func TestPass(t *testing.T) {
	r := newRig(t, true)
	// The script has delivered this week: a missing beacon counts.
	if err := r.st.AddStats(t.Context(), []store.IngestStat{{SiteID: r.site.ID, Hour: r.now.Unix() / 3600,
		Accepted: 5}}); err != nil {
		t.Fatal(err)
	}
	r.insert(t,
		// A person: page, assets, beacon, engagement.
		req("192.0.2.1", browserUA, "document", "/", 200, ""),
		req("192.0.2.1", browserUA, "script", "/assets/a.js", 200, ""),
		store.Event{Source: "browser", Kind: "page_view", IP: "192.0.2.1", UA: browserUA, Props: `{"ctx":{"webdriver":false}}`},
		store.Event{Source: "browser", Kind: "engagement", IP: "192.0.2.1", UA: browserUA},
		// Googlebot from its range.
		req("66.249.66.1", "Mozilla/5.0 (compatible; Googlebot/2.1)", "document", "/", 200, ""),
		// Someone claiming Googlebot from elsewhere.
		req("203.0.113.5", "Mozilla/5.0 (compatible; Googlebot/2.1)", "document", "/", 200, ""),
		// Bingbot, rDNS confirmed; and one in the range that is not.
		req("157.55.39.1", "Mozilla/5.0 (compatible; bingbot/2.0)", "document", "/", 200, ""),
		req("157.55.39.2", "Mozilla/5.0 (compatible; bingbot/2.0)", "document", "/", 200, ""),
		// UptimeRobot.
		req("69.162.124.230", "Mozilla/5.0+(compatible; UptimeRobot/2.0)", "", "/", 200, ""),
		// A crawler known by its client string only.
		req("198.51.100.20", "SomeCrawler/1.0", "", "/", 200, ""),
		// curl.
		req("198.51.100.7", "curl/8.4.0", "", "/api/v1/ip/1", 401, ""),
		// A browser-looking client fetching pages without assets, beacon or browser headers.
		req("198.51.100.8", browserUA, "document", "/a", 200, `{}`),
		req("198.51.100.8", browserUA, "document", "/b", 200, `{}`),
		req("198.51.100.8", browserUA, "document", "/c", 200, `{}`),
		// A scripted browser.
		store.Event{Source: "browser", Kind: "page_view", IP: "198.51.100.9", UA: browserUA,
			Props: `{"ctx":{"webdriver":true}}`},
		// No user agent at all.
		req("198.51.100.10", "", "", "/", 200, ""),
	)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, err := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"192.0.2.1":      "human|script beacon,engaged,assets loaded",
		"66.249.66.1":    "bot|Googlebot address range",
		"203.0.113.5":    "unknown|claims Googlebot, address is not Googlebot's",
		"157.55.39.1":    "bot|Bingbot, rDNS confirmed",
		"157.55.39.2":    "unknown|claims Bingbot, address is not Bingbot's",
		"69.162.124.230": "monitor|UptimeRobot address range",
		"198.51.100.20":  "bot|client is UAOnly",
		"198.51.100.7":   "unknown|client curl/8.4.0",
		"198.51.100.8":   "unknown|no script beacon,no assets,missing browser headers",
		"198.51.100.9":   "unknown|navigator.webdriver set",
		"198.51.100.10":  "unknown|no user agent",
	} {
		a := classes[key]
		if got := a.Class + "|" + strings.Join(a.Reasons, ","); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// The pass stamped the events, and new events of a known actor get the class at once.
	var pending int
	_ = r.st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE cls = 0`).Scan(&pending)
	if pending != 0 {
		t.Errorf("%d events still pending", pending)
	}
	e := store.Event{SiteID: r.site.ID, TS: r.now.UnixMilli(), IP: "198.51.100.8", IPKey: "198.51.100.8", UA: browserUA}
	r.c.Enrich(&e)
	if e.Cls != store.ClsUnknown {
		t.Errorf("known actor enriched as %d", e.Cls)
	}
	// A second pass with nothing new is quiet.
	r.now = r.now.Add(time.Minute)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The next day forgets yesterday's map.
	r.now = r.now.Add(24 * time.Hour)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	e = store.Event{SiteID: r.site.ID, TS: r.now.UnixMilli(), IP: "198.51.100.8", IPKey: "198.51.100.8", UA: browserUA}
	r.c.Enrich(&e)
	if e.Cls != store.ClsPending {
		t.Errorf("yesterday's class used today: %d", e.Cls)
	}
	// Run stops with its context.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r.c.Run(ctx)
}

// An event written after a pass but dated before it (the feed's own clock, the browser's back-dating) is still
// classified by the next pass.
func TestPass_LateEvent(t *testing.T) {
	r := newRig(t, true)
	r.insert(t, req("192.0.2.50", "curl/8", "", "/", 200, ""))
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(10 * time.Minute)
	late := req("198.51.100.77", browserUA, "document", "/", 200, `{}`)
	late.TS = r.now.Add(-50 * time.Minute).UnixMilli()
	late.IPKey = late.IP
	late.SiteID = r.site.ID
	if err := r.st.InsertEvents(t.Context(), []store.Event{late}); err != nil {
		t.Fatal(err)
	}
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	var cls int
	if err := r.st.DB.QueryRow(`SELECT cls FROM event WHERE ip = '198.51.100.77'`).Scan(&cls); err != nil || cls == store.ClsPending {
		t.Errorf("late event cls = %d, %v", cls, err)
	}
}

func TestPass_NoFeed(t *testing.T) {
	r := newRig(t, false)
	r.insert(t, store.Event{Source: "browser", Kind: "page_view", IP: "192.0.2.1", UA: browserUA})
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	if classes["192.0.2.1"].Class != "human" {
		t.Errorf("classes = %+v", classes)
	}
}

func TestInstant(t *testing.T) {
	r := newRig(t, true)
	for _, tt := range []struct {
		ip, ua string
		want   int
	}{
		{"66.249.66.1", browserUA, store.ClsBot},
		{"69.162.124.230", "x", store.ClsMonitor},
		{"157.55.39.2", browserUA, store.ClsPending}, // non-authoritative: the pass decides
		{"192.0.2.1", "SomeCrawler/2", store.ClsBot},
		{"192.0.2.1", "python-requests/2.32", store.ClsUnknown},
		{"192.0.2.1", "", store.ClsUnknown},
		{"192.0.2.1", browserUA, store.ClsPending},
	} {
		e := store.Event{IP: tt.ip, UA: tt.ua, IPKey: tt.ip}
		r.c.Enrich(&e)
		if e.Cls != tt.want {
			t.Errorf("%s %q = %d, want %d", tt.ip, tt.ua, e.Cls, tt.want)
		}
	}
	bad := store.Event{IP: "nope"}
	r.c.Enrich(&bad)
	var empty Classifier
	if empty.instant(netipMust("192.0.2.1"), "curl/1") != store.ClsUnknown {
		t.Error("no ref data")
	}
}

func TestClientAndUA(t *testing.T) {
	for ua, want := range map[string]string{
		"":                     "no user agent",
		"curl/8.4.0":           "curl/8.4.0",
		"python-requests/2.32": "python-requests/2.32",
		"Go-http-client/1.1":   "Go-http-client/1.1",
		browserUA:              "Safari · macOS",
		"Mozilla/5.0 (X11; Linux x86_64) HeadlessChrome/120": "Mozilla/5.0",
		"(weird)":                     "(weird)",
		"!" + strings.Repeat("x", 60): "!" + strings.Repeat("x", 39),
		"UptimeRobot":                 "UptimeRobot",
	} {
		if got := Client(ua); got != want {
			t.Errorf("Client(%q) = %q, want %q", ua, got, want)
		}
	}
	for ua, want := range map[string]string{
		"Mozilla/5.0 (Windows NT 10.0) Chrome/120 Edg/120":                       "Edge Windows Desktop",
		"Mozilla/5.0 (Linux; Android 14) Chrome/120 Mobile Safari/537.36":        "Chrome Android Mobile",
		"Mozilla/5.0 (iPad; CPU OS 17_0) Version/17.0 Safari/604.1":              "Safari iOS Tablet",
		"Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0": "Firefox Linux Desktop",
		"Mozilla/5.0 (X11; CrOS x86_64) Chrome/120 OPR/100":                      "Opera ChromeOS Desktop",
		"Mozilla/5.0 (Linux; Android 13) SamsungBrowser/23.0 Mobile":             "Samsung Internet Android Mobile",
		"curl/8":                                "Other Other Other",
		"Mozilla/5.0 (iPhone) CriOS/120 Mobile": "Chrome iOS Mobile",
	} {
		b, o, d := ParseUA(ua)
		if got := b + " " + o + " " + d; got != want {
			t.Errorf("ParseUA(%q) = %q, want %q", ua, got, want)
		}
	}
}

func netipMust(s string) netip.Addr { return netip.MustParseAddr(s) }
