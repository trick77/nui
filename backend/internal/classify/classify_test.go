package classify

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trick77/stakeout/backend/internal/refdata"
	"github.com/trick77/stakeout/backend/internal/sites"
	"github.com/trick77/stakeout/backend/internal/store"
)

const crawlers = `{"services":{
 "Googlebot":{"ipv4":["66.249.64.0/19"],"ip_list_authoritative":true,"user_agent_patterns":["*Googlebot*"]},
 "Bingbot":{"ipv4":["157.55.39.0/24"],"ip_list_authoritative":false,"user_agent_patterns":["*bingbot*"],
   "rdns_patterns":["*.search.msn.com"]},
 "UAOnly":{"user_agent_patterns":["*SomeCrawler*"]}}}`

const monitoring = `{"services":{"UptimeRobot":{"ipv4":["69.162.124.224/28"],"ip_list_authoritative":true,
 "user_agent_patterns":["*UptimeRobot*"]}}}`

// resolver names every Bingbot address but 157.55.39.2 msnbot-a-b-c-d.search.msn.com, forward-confirmed; a
// lookup of 157.55.39.3 times out.
type resolver struct{}

func (resolver) LookupAddr(_ context.Context, a string) ([]string, error) {
	switch {
	case a == "157.55.39.3":
		return nil, &net.DNSError{Err: "i/o timeout", IsTimeout: true}
	case strings.HasPrefix(a, "157.55.39.") && a != "157.55.39.2":
		return []string{"msnbot-" + strings.ReplaceAll(a, ".", "-") + ".search.msn.com."}, nil
	}
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

func (resolver) LookupHost(_ context.Context, h string) ([]string, error) {
	if a, ok := strings.CutPrefix(h, "msnbot-"); ok {
		return []string{strings.ReplaceAll(strings.TrimSuffix(a, ".search.msn.com."), "-", ".")}, nil
	}
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

type rig struct {
	st   *store.Store
	c    *Classifier
	site *sites.Site
	now  time.Time
}

func newRig(t *testing.T, feed bool) *rig {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "app.db"))
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
	// The rDNS checks are pending after the first pass (the pass never waits for DNS); once answered, the next pass
	// decides those actors again without new events.
	classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	if got := classes["157.55.39.1"]; got.Class != "bot" || got.Reasons[0] != "Bingbot address range, rDNS check pending" {
		t.Errorf("before the rDNS answer: %+v", got)
	}
	r.c.Verifier.Work(t.Context())
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
		"203.0.113.5":    "unknown|claims Googlebot, address is not Googlebot's,no script beacon,no assets",
		"157.55.39.1":    "bot|Bingbot, rDNS confirmed",
		"157.55.39.2":    "unknown|claims Bingbot, address is not Bingbot's,no script beacon,no assets",
		"69.162.124.230": "monitor|UptimeRobot address range",
		"198.51.100.20":  "bot|client is UAOnly",
		"198.51.100.7":   "unknown|client curl/8.4.0",
		// This feed carries the fetch metadata: a browser without it is none.
		"198.51.100.8":  "unknown|missing browser headers,no script beacon,no assets",
		"198.51.100.9":  "unknown|navigator.webdriver set",
		"198.51.100.10": "unknown|no user agent",
	} {
		a := classes[key]
		if got := a.Class + "|" + strings.Join(a.Reasons, ","); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if len(r.c.recheck) != 0 {
		t.Errorf("rechecks left: %v", r.c.recheck)
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

// Only a completed lookup makes a crawler's claim false. A real crawler inside its listed range past the network's
// lookup budget, or behind a DNS timeout, is the crawler marked unverified, not an impostor; outside the range the
// claim stays unknown with the true reason. A failed lookup is asked again soon.
func TestPass_UncheckedClaims(t *testing.T) {
	r := newRig(t, true)
	const bing = "Mozilla/5.0 (compatible; bingbot/2.0)"
	pass := func() {
		t.Helper()
		if err := r.c.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		r.c.Verifier.Work(t.Context())
	}
	r.insert(t, req("157.55.39.3", bing, "document", "/", 200, ""))
	pass()
	var evs []store.Event
	for i := range 21 { // with the one above, more than one /24's daily lookups
		evs = append(evs, req(fmt.Sprintf("157.55.39.%d", 10+i), bing, "document", "/", 200, ""))
	}
	r.insert(t, evs...)
	pass()
	pass()
	classes, err := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for key, a := range classes {
		got[a.Class+"|"+strings.Join(a.Reasons, ",")]++
		if strings.Contains(strings.Join(a.Reasons, ","), "address is not") {
			t.Errorf("%s called an impostor without an answer: %v", key, a.Reasons)
		}
	}
	if got["bot|Bingbot, rDNS confirmed"] != 15 || got["bot|Bingbot address range, rDNS not verified"] != 6 ||
		got["bot|Bingbot address range, rDNS lookup failed"] != 1 {
		t.Errorf("classes = %v", got)
	}
	// The timed-out address waits for its retry; the one past the budget does not.
	if !r.c.recheck[actorKey{site: r.site.ID, key: "157.55.39.3", day: r.now.UnixMilli() / dayMS}] ||
		len(r.c.recheck) != 1 {
		t.Errorf("rechecks: %v", r.c.recheck)
	}
	// A claim from outside the listed range past its network's budget is unknown, and says why.
	var out []store.Event
	for i := range 17 {
		out = append(out, req(fmt.Sprintf("198.51.100.%d", 100+i), bing, "document", "/", 200, ""))
	}
	r.insert(t, out...)
	r.c.Verifier.StartPass()
	for i := range 17 {
		r.c.Verifier.Check(fmt.Sprintf("198.51.100.%d", 100+i), []string{"*.search.msn.com"})
	}
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ = r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	if a := classes["198.51.100.116"]; a.Class != "unknown" || a.Reasons[0] != "claims Bingbot, rDNS not verified" {
		t.Errorf("outside the range past the budget: %+v", a)
	}
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

// A shared address is weighed per client: people behind a NAT stay human with one curl among them, the curl part
// counted as automated on its own events; a script does not turn human by adding one browser request.
func TestPass_SharedAddressAndPace(t *testing.T) {
	r := newRig(t, true)
	if err := r.st.AddStats(t.Context(), []store.IngestStat{{SiteID: r.site.ID, Hour: r.now.Unix() / 3600,
		Accepted: 5}}); err != nil {
		t.Fatal(err)
	}
	const nat = "192.0.2.80"
	var evs []store.Event
	for _, ua := range []string{browserUA, "Mozilla/5.0 (Windows NT 10.0) Chrome/142 Safari/537.36",
		"Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"} {
		evs = append(evs, req(nat, ua, "document", "/", 200, ""), req(nat, ua, "script", "/assets/a.js", 200, ""),
			store.Event{Source: "browser", Kind: "page_view", IP: nat, UA: ua},
			store.Event{Source: "browser", Kind: "engagement", IP: nat, UA: ua})
	}
	for range 3 {
		evs = append(evs, req(nat, "curl/8.4.0", "", "/api/v1/ip/1", 200, ""))
	}
	// A script with one browser request, its user agent sorting after the script's.
	for range 40 {
		evs = append(evs, req("192.0.2.81", "Go-http-client/1.1", "", "/api/v1/ip/1", 200, ""))
	}
	evs = append(evs, req("192.0.2.81", browserUA, "document", "/", 200, ""))
	// 150 API requests 10 ms apart with a browser's user agent and headers, no page, asset or beacon.
	for i := range 150 {
		e := req("192.0.2.82", browserUA, "empty", "/api/v1/ip/1", 200, "")
		e.TS = r.now.UnixMilli() - int64(i)*10
		evs = append(evs, e)
	}
	// One request with a browser's user agent and none of the headers every browser sends.
	evs = append(evs, req("192.0.2.83", browserUA, "document", "/", 200, `{}`))
	// A real browser blocking the tracker: page and assets with its headers, no beacon. A person.
	evs = append(evs, req("192.0.2.84", browserUA, "document", "/", 200, ""),
		req("192.0.2.84", browserUA, "style", "/assets/a.css", 200, ""))
	// The same, loading one page with 25 assets and API calls within 3 s: a burst, still a person.
	burst := req("192.0.2.85", browserUA, "document", "/", 200, "")
	burst.TS = r.now.UnixMilli() - 3000
	evs = append(evs, burst)
	for i := range 25 {
		e := req("192.0.2.85", browserUA, "script", "/assets/c.js", 200, "")
		if i%2 == 0 {
			e = req("192.0.2.85", browserUA, "empty", "/api/v1/ip/1", 200, "")
		}
		e.TS = r.now.UnixMilli() - 3000 + int64(i)*100
		evs = append(evs, e)
	}
	// A browser with its headers, the tracker blocked and the assets cached: documents only, no engagement. Nothing
	// shows a browser, so no person: a crawler wearing a browser's user agent looks the same.
	for _, p := range []string{"/", "/a", "/b"} {
		evs = append(evs, req("192.0.2.86", browserUA, "document", p, 200, ""))
	}
	// API calls only: 40 calls 20 ms apart with a browser's headers, no page, asset or beacon that day. A person's
	// single-page app loads its page and assets first; without them, no person.
	for i := range 40 {
		e := req("192.0.2.87", browserUA, "empty", "/api/v1/ip/1", 200, "")
		e.TS = r.now.UnixMilli() - int64(i)*20
		evs = append(evs, e)
	}
	r.insert(t, evs...)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	for key, want := range map[string]string{
		nat: "human|script beacon,engaged,assets loaded," +
			"also unknown automated: curl/8.4.0, 3 of 15 events (client curl/8.4.0)",
		"192.0.2.81": "unknown|client Go-http-client/1.1,no sign of a browser",
		"192.0.2.82": "unknown|machine pace,API only, no page loaded",
		"192.0.2.83": "unknown|missing browser headers,no script beacon,no assets",
		"192.0.2.84": "human|assets loaded,no script beacon",
		"192.0.2.85": "human|assets loaded,no script beacon",
		"192.0.2.86": "unknown|no sign of a browser",
		"192.0.2.87": "unknown|no sign of a browser,API only, no page loaded,rapid requests",
	} {
		a := classes[key]
		if got := a.Class + "|" + strings.Join(a.Reasons, ","); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// On the events: the people human, the curl part automated.
	var people, auto int
	if err := r.st.DB.QueryRow(`SELECT SUM(cls = ?), SUM(cls = ?) FROM event WHERE ip_key = ?`,
		store.ClsHuman, store.ClsUnknown, nat).Scan(&people, &auto); err != nil || people != 12 || auto != 3 {
		t.Errorf("NAT events: %d human, %d unknown, %v", people, auto, err)
	}
	// New events from the NAT: a new automation client is not stamped human, a browser is.
	for ua, want := range map[string]int{"python-requests/2.32": store.ClsUnknown, "curl/8.4.0": store.ClsUnknown,
		"Mozilla/5.0 (iPhone) Mobile Safari/604.1": store.ClsHuman} {
		e := store.Event{SiteID: r.site.ID, TS: r.now.UnixMilli(), IP: nat, IPKey: nat, UA: ua}
		r.c.Enrich(&e)
		if e.Cls != want {
			t.Errorf("new %q from the NAT: %d, want %d", ua, e.Cls, want)
		}
	}
}

// "No assets" is a signal only on a site whose feed reports asset requests that day: behind a CDN or the browser's
// cache no visitor has any.
func TestPass_NoAssetsNeedsAssets(t *testing.T) {
	r := newRig(t, true)
	r.insert(t, req("192.0.2.90", browserUA, "document", "/", 200, ""),
		req("192.0.2.91", browserUA, "document", "/", 200, ""))
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	if a := classes["192.0.2.90"]; a.Class != "human" || slices.Contains(a.Reasons, "no assets") {
		t.Errorf("no asset reported on the site: %+v", a)
	}
	// Another visitor's asset that day makes it one; a day without any (yesterday) does not. 192.0.2.91's beacon shows
	// a browser: a person without assets.
	r.insert(t, req("192.0.2.92", browserUA, "script", "/assets/a.js", 200, ""),
		req("192.0.2.91", browserUA, "document", "/b", 200, ""),
		store.Event{Source: "browser", Kind: "page_view", IP: "192.0.2.91", UA: browserUA})
	old := req("192.0.2.93", browserUA, "document", "/", 200, "")
	old.TS = r.now.Add(-24 * time.Hour).UnixMilli()
	r.insert(t, old)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ = r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	if a := classes["192.0.2.91"]; a.Class != "human" || !slices.Contains(a.Reasons, "no assets") {
		t.Errorf("with assets reported: %+v", a)
	}
	yesterday, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS-1)
	if a := yesterday["192.0.2.93"]; a.Class != "human" || slices.Contains(a.Reasons, "no assets") {
		t.Errorf("yesterday, no asset reported: %+v", a)
	}
}

const petalUA = "Mozilla/5.0 (Linux; Android 7.0;) AppleWebKit/537.36 (KHTML, like Gecko) Mobile Safari/537.36 " +
	"(compatible; PetalBot;+https://webmaster.petalsearch.com/site/petalbot)"

// A person is shown by a browser's evidence, never assumed from a browser's user agent: on a site whose script
// delivers, a client without beacon, engagement or assets is unknown. A page request is one without Sec-Fetch-Dest
// too: crawlers do not send it.
func TestPass_BrowserEvidence(t *testing.T) {
	r := newRig(t, true)
	if err := r.st.AddStats(t.Context(), []store.IngestStat{{SiteID: r.site.ID, Hour: r.now.Unix() / 3600,
		Accepted: 5}}); err != nil {
		t.Fatal(err)
	}
	get := func(ip, ua, path string, props string) store.Event {
		e := req(ip, ua, "", path, 200, props)
		e.Method = "GET"
		return e
	}
	r.insert(t,
		// PetalBot wearing a phone's Safari: three pages, no Sec-Fetch-Dest.
		get("192.0.2.100", petalUA, "/AS1", `{"lang":"en"}`),
		get("192.0.2.100", petalUA, "/AS2", `{"lang":"en"}`),
		get("192.0.2.100", petalUA, "/AS3", `{"lang":"en"}`),
		// One page with a clean Chrome user agent and Accept-Language, nothing else.
		get("192.0.2.101", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/142.0 Safari/537.36", "/AS1",
			`{"lang":"en-GB"}`),
		// A person: the beacon shows the browser; a page without Sec-Fetch-Dest still counts as one, so on a day
		// with assets reported the missing assets are a reason.
		get("192.0.2.102", browserUA, "/AS1", ""),
		store.Event{Source: "browser", Kind: "page_view", IP: "192.0.2.102", UA: browserUA},
		req("192.0.2.103", browserUA, "script", "/assets/a.js", 200, ""),
		store.Event{Source: "browser", Kind: "page_view", IP: "192.0.2.103", UA: browserUA},
		// Neither a page nor a file: an API call, an asset by its extension.
		get("192.0.2.102", browserUA, "/api/v1/x", ""),
		get("192.0.2.102", browserUA, "/robots.txt", ""),
	)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	for key, want := range map[string]string{
		"192.0.2.100": "unknown|client PetalBot,no script beacon,no assets",
		"192.0.2.101": "unknown|no sign of a browser",
		"192.0.2.102": "human|script beacon,no assets",
		"192.0.2.103": "human|script beacon,assets loaded",
	} {
		a := classes[key]
		if got := a.Class + "|" + strings.Join(a.Reasons, ","); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	var docs int
	if err := r.st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE ip = '192.0.2.102' AND source = 'server' AND ` +
		pageRequest).Scan(&docs); err != nil || docs != 1 {
		t.Errorf("page requests of 192.0.2.102 = %d, %v", docs, err)
	}
}

// A person whose page reaches the product over one path and whose beacon reaches Stakeout over another (dual stack, a
// relay) is one by the beacon for the same page with the same user agent within a minute; a beacon too late, or for
// another page, is no evidence.
func TestPass_BeaconFromAnotherAddress(t *testing.T) {
	r := newRig(t, true)
	if err := r.st.AddStats(t.Context(), []store.IngestStat{{SiteID: r.site.ID, Hour: r.now.Unix() / 3600,
		Accepted: 5}}); err != nil {
		t.Fatal(err)
	}
	at := r.now.UnixMilli()
	page := func(ip, path string, ts int64) store.Event {
		e := req(ip, browserUA, "document", path, 200, "")
		e.TS = ts
		return e
	}
	beacon := func(ip, path string, ts int64) store.Event {
		return store.Event{Source: "browser", Kind: "page_view", IP: ip, UA: browserUA, Path: path, TS: ts}
	}
	r.insert(t,
		page("2001:db8::1", "/AS1", at-1000), beacon("192.0.2.110", "/AS1", at),
		page("2001:db8:1::1", "/AS2", at-1000), beacon("192.0.2.111", "/AS2", at+2*beaconWindow),
		page("2001:db8:2::1", "/AS3", at-1000), beacon("192.0.2.112", "/AS4", at),
	)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	for key, want := range map[string]string{
		"2001:db8::1":   "human|script beacon from another address",
		"2001:db8:1::1": "unknown|no sign of a browser",
		"2001:db8:2::1": "unknown|no sign of a browser",
	} {
		a := classes[key]
		if got := a.Class + "|" + strings.Join(a.Reasons, ","); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// Without the script only a beacon could show a browser: one cached favicon on a feed-only site makes no visitor
// automated, and a page scraper's pace is still a machine's though its pages carry no Sec-Fetch-Dest.
func TestPass_FeedOnlyAndPageScraper(t *testing.T) {
	r := newRig(t, true)
	var evs []store.Event
	evs = append(evs, req("192.0.2.120", browserUA, "image", "/favicon.ico", 200, ""),
		req("192.0.2.121", browserUA, "document", "/", 200, ""))
	for i := range 150 {
		e := req("192.0.2.122", browserUA, "", fmt.Sprintf("/page/%d", i), 200, `{"lang":"en"}`)
		e.Method = "GET"
		e.TS = r.now.UnixMilli() - int64(i)*10
		evs = append(evs, e)
	}
	r.insert(t, evs...)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	if a := classes["192.0.2.121"]; a.Class != "human" {
		t.Errorf("visitor on a feed-only site: %+v", a)
	}
	if a := classes["192.0.2.122"]; a.Class != "unknown" || a.Reasons[0] != reasonMachinePace {
		t.Errorf("page scraper: %+v", a)
	}
}

// A beacon after a pass that saw only the page request is the person's evidence at once, not the pass's stale verdict.
func TestEnrich_BeaconAfterPass(t *testing.T) {
	r := newRig(t, true)
	if err := r.st.AddStats(t.Context(), []store.IngestStat{{SiteID: r.site.ID, Hour: r.now.Unix() / 3600,
		Accepted: 5}}); err != nil {
		t.Fatal(err)
	}
	r.insert(t, req("192.0.2.130", browserUA, "document", "/", 200, ""))
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	e := store.Event{SiteID: r.site.ID, Source: "browser", Kind: "page_view", IP: "192.0.2.130", IPKey: "192.0.2.130",
		UA: browserUA, TS: r.now.UnixMilli(), Props: `{"ctx":{"webdriver":false}}`}
	r.c.Enrich(&e)
	if e.Cls != store.ClsHuman {
		t.Errorf("beacon after the pass: %d", e.Cls)
	}
	// A request of the same client still has the pass's class.
	e = req("192.0.2.130", browserUA, "document", "/a", 200, "")
	e.SiteID, e.IPKey, e.TS = r.site.ID, e.IP, r.now.UnixMilli()
	r.c.Enrich(&e)
	if e.Cls != store.ClsUnknown {
		t.Errorf("request after the pass: %d", e.Cls)
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
	// A beacon from a browser shows a person at once, unless the browser is scripted or a list names the client.
	for _, tt := range []struct {
		ua, props string
		want      int
	}{
		{browserUA, `{"ctx":{"webdriver":false}}`, store.ClsHuman},
		{browserUA, `{"ctx":{"webdriver":true}}`, store.ClsUnknown},
		// A page's own property named webdriver says nothing about the browser.
		{browserUA, `{"p":{"webdriver":true},"ctx":{}}`, store.ClsHuman},
		// A batch's later event carries no context: the pass decides.
		{browserUA, `{"p":{"x":1}}`, store.ClsPending},
		// No browser's user agent: the pass decides.
		{"Dalvik/2.1.0", `{"ctx":{}}`, store.ClsPending},
		{"SomeCrawler/2", "", store.ClsBot},
	} {
		e := store.Event{Source: "browser", IP: "192.0.2.1", UA: tt.ua, IPKey: "192.0.2.1", Props: tt.props}
		r.c.Enrich(&e)
		if e.Cls != tt.want {
			t.Errorf("beacon %q %s = %d, want %d", tt.ua, tt.props, e.Cls, tt.want)
		}
	}
	bad := store.Event{IP: "nope"}
	r.c.Enrich(&bad)
	var empty Classifier
	if empty.instant(netipMust("192.0.2.1"), "curl/1") != store.ClsUnknown {
		t.Error("no ref data")
	}
}

// ensp is U+2002, spelled out: the expected strings must not lean on apix.Sep.
const ensp = "\xe2\x80\x82"

func TestClientAndUA(t *testing.T) {
	for ua, want := range map[string]string{
		"":                     "no user agent",
		"curl/8.4.0":           "curl/8.4.0",
		"python-requests/2.32": "python-requests/2.32",
		"Go-http-client/1.1":   "Go-http-client/1.1",
		browserUA:              "Safari" + ensp + "·" + ensp + "macOS",
		"Mozilla/5.0 (X11; Linux x86_64) HeadlessChrome/120":                       "HeadlessChrome/120",
		"Mozilla/5.0 zgrab/0.x":                                                    "zgrab/0.x",
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)": "Googlebot/2.1",
		"(weird)":                     "(weird)",
		"!" + strings.Repeat("x", 60): "!" + strings.Repeat("x", 39),
		"UptimeRobot":                 "UptimeRobot",
		petalUA:                       "PetalBot",
		"Mozilla/5.0 (compatible; Foo-Preview/1.0; +https://foo.example/info)":   "Foo-Preview/1.0",
		"Mozilla/5.0 (compatible; Barkrowler/0.9; +https://babbar.tech/crawler)": "Barkrowler/0.9",
		// A phone named CUBOT is a browser, not a bot.
		"Mozilla/5.0 (Linux; Android 9; CUBOT X19) AppleWebKit/537.36 Chrome/120 Mobile Safari/537.36": "Chrome" + ensp + "·" + ensp + "Android",
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
