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

	"github.com/trick77/trakk/backend/internal/refdata"
	"github.com/trick77/trakk/backend/internal/sites"
	"github.com/trick77/trakk/backend/internal/store"
)

const crawlers = `{"services":{
 "Googlebot":{"ipv4":["66.249.64.0/19"],"ip_list_authoritative":true,"user_agent_patterns":["*Googlebot*"]},
 "Bingbot":{"ipv4":["157.55.39.0/24"],"ip_list_authoritative":false,"user_agent_patterns":["*bingbot*"],
   "rdns_patterns":["*.search.msn.com"]},
 "UAOnly":{"user_agent_patterns":["*SomeCrawler*"]}}}`

const monitoring = `{"services":{"UptimeRobot":{"ipv4":["69.162.124.224/28"],"ip_list_authoritative":true,
 "user_agent_patterns":["*UptimeRobot*"]}}}`

// resolver names every Bingbot address msnbot-a-b-c-d.search.msn.com, forward-confirmed, but 157.55.39.2 (another's
// name) and 157.55.39.4 (none); a lookup of 157.55.39.3 times out.
type resolver struct{}

func (resolver) LookupAddr(_ context.Context, a string) ([]string, error) {
	switch {
	case a == "157.55.39.2":
		return []string{"host.example.net."}, nil
	case a == "157.55.39.3":
		return nil, &net.DNSError{Err: "i/o timeout", IsTimeout: true}
	case strings.HasPrefix(a, "157.55.39.") && a != "157.55.39.4":
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

// engagedProps is an engagement with input.
const engagedProps = `{"p":{"ms":4000,"inputs":{"pointer":1,"key":0,"touch":0,"scroll":2,"move":20,"mdown":1}}}`

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
		store.Event{Source: "browser", Kind: "engagement", IP: "192.0.2.1", UA: browserUA, Props: engagedProps},
		// Googlebot from its range.
		req("66.249.66.1", "Mozilla/5.0 (compatible; Googlebot/2.1)", "document", "/", 200, ""),
		// Someone claiming Googlebot from elsewhere.
		req("203.0.113.5", "Mozilla/5.0 (compatible; Googlebot/2.1)", "document", "/", 200, ""),
		// Bingbot, rDNS confirmed; one in the range that is not; one there without a name; one elsewhere without.
		req("157.55.39.1", "Mozilla/5.0 (compatible; bingbot/2.0)", "document", "/", 200, ""),
		req("157.55.39.2", "Mozilla/5.0 (compatible; bingbot/2.0)", "document", "/", 200, ""),
		req("157.55.39.4", "Mozilla/5.0 (compatible; bingbot/2.0)", "document", "/", 200, ""),
		req("203.0.113.6", "Mozilla/5.0 (compatible; bingbot/2.0)", "document", "/", 200, ""),
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
		"157.55.39.4":    "bot|Bingbot address range, no rDNS name",
		"203.0.113.6":    "unknown|claims Bingbot, no rDNS name,no script beacon,no assets",
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
			store.Event{Source: "browser", Kind: "engagement", IP: nat, UA: ua, Props: engagedProps})
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
		"192.0.2.87": "unknown|API only, no page loaded,rapid requests",
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

// A person whose page reaches the product over one path and whose beacon reaches Trakk over another (dual stack, a
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

// A feed batch without usable times is dated to its arrival: its calls share one millisecond, which says nothing of
// their pace. The same calls a millisecond apart are a machine's.
func TestPass_MachinePaceNeedsSpan(t *testing.T) {
	r := newRig(t, true)
	at := r.now.UnixMilli()
	var evs []store.Event
	for i := range machineRequests {
		same := req("192.0.2.130", browserUA, "", fmt.Sprintf("/api/items/%d", i), 200, "")
		same.Method, same.TS = "GET", at
		spread := req("192.0.2.131", browserUA, "", fmt.Sprintf("/api/items/%d", i), 200, "")
		spread.Method, spread.TS = "GET", at-int64(i)
		evs = append(evs, same, spread)
	}
	r.insert(t, evs...)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, at/dayMS)
	if a := classes["192.0.2.130"]; slices.Contains(a.Reasons, reasonMachinePace) ||
		slices.Contains(a.Reasons, "rapid requests") {
		t.Errorf("one millisecond: %+v", a)
	}
	if a := classes["192.0.2.131"]; a.Class != "unknown" || len(a.Reasons) == 0 || a.Reasons[0] != reasonMachinePace {
		t.Errorf("a millisecond apart: %+v", a)
	}
}

// loomUA is an MCP client naming its repository: a contact URL, which marks automation in any other client.
const loomUA = "loom/0.14.8 (+https://github.com/example/loom)"

func account(e store.Event) store.Event {
	e.Account = "acct_1"
	return e
}

// A program the product signed in is an API client, whatever its address or user agent says: the account is the
// product's word. A prober with one good key is none, and without an account nothing changes.
func TestPass_APIClient(t *testing.T) {
	r := newRig(t, true)
	const listed = "66.249.66.7" // in Googlebot's authoritative range
	mcp := func(ip, ua string, status int) store.Event { return req(ip, ua, "", "/mcp", status, `{}`) }
	var evs []store.Event
	for range 6 {
		evs = append(evs, account(mcp(listed, loomUA, 200)), // a bot list's mistake
			account(mcp("192.0.2.140", "claude-code/2.1 (cli)", 200)), // no contact URL
			mcp("192.0.2.141", loomUA, 200),
			mcp("66.249.66.8", "some-agent/1", 200))
	}
	// One good key, then refusals: their account is known, the request was not answered.
	evs = append(evs, account(mcp("192.0.2.142", "prober/1", 200)))
	for range 20 {
		evs = append(evs, account(mcp("192.0.2.142", "prober/1", 429)))
	}
	// Lookups that found nothing, and one asset: an API client's all the same.
	for range 6 {
		evs = append(evs, account(mcp("66.249.66.10", loomUA, 404)))
	}
	evs = append(evs, account(req("66.249.66.10", loomUA, "", "/assets/x", 200, `{}`)))
	// The account's client with a Googlebot beside it on the listed address: each its own.
	evs = append(evs, account(mcp("66.249.66.9", loomUA, 200)), account(mcp("66.249.66.9", loomUA, 200)),
		mcp("66.249.66.9", "Mozilla/5.0 (compatible; Googlebot/2.1)", 200))
	// A browser's user agent on the API, no Sec-Fetch header: a script wearing it. With the headers: a browser.
	evs = append(evs, account(mcp("192.0.2.143", browserUA, 200)),
		account(req("192.0.2.144", browserUA, "empty", "/v1/x", 200, "")),
		account(req("192.0.2.145", browserUA, "document", "/", 200, `{}`)))
	// The docs read in a browser from the program's own address, on a site without the script.
	evs = append(evs, account(mcp("192.0.2.146", loomUA, 200)), account(mcp("192.0.2.146", loomUA, 200)),
		req("192.0.2.146", browserUA, "document", "/docs", 200, ""))
	r.insert(t, evs...)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	for key, want := range map[string]string{
		listed:         "api|signed-in API client",
		"192.0.2.140":  "api|signed-in API client",
		"192.0.2.141":  "unknown|client loom/0.14.8",
		"66.249.66.8":  "bot|Googlebot address range",
		"66.249.66.9":  "api|signed-in API client,Googlebot address range",
		"192.0.2.143":  "api|signed-in API client",
		"192.0.2.146":  "api|signed-in API client",
		"66.249.66.10": "api|signed-in API client",
	} {
		a := classes[key]
		if got := a.Class + "|" + strings.Join(a.Reasons, ","); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for key, not := range map[string]string{"192.0.2.142": "api", "192.0.2.144": "api", "192.0.2.145": "api"} {
		if classes[key].Class == not {
			t.Errorf("%s = %+v", key, classes[key])
		}
	}
	for _, tt := range []struct {
		ip, ua string
		want   int
	}{
		{"66.249.66.9", "Mozilla/5.0 (compatible; Googlebot/2.1)", store.ClsBot},
		{"66.249.66.9", loomUA, store.ClsAPI},
		{"192.0.2.146", browserUA, store.ClsHuman},
	} {
		var cls int
		if err := r.st.DB.QueryRow(`SELECT MAX(cls) FROM event WHERE ip = ? AND ua = ?`, tt.ip, tt.ua).Scan(&cls); err != nil ||
			cls != tt.want {
			t.Errorf("%s %q: cls %d, want %d, %v", tt.ip, tt.ua, cls, tt.want, err)
		}
	}
}

// A user agent that is no browser's is a program's on any site, however plain its name and whatever it fetched; only a
// script's beacon shows a browser behind one, and then its requests stay the person's between passes.
func TestPass_ProgramIsNoPerson(t *testing.T) {
	for _, script := range []bool{false, true} {
		r := newRig(t, true)
		if script {
			if err := r.st.AddStats(t.Context(), []store.IngestStat{{SiteID: r.site.ID, Hour: r.now.Unix() / 3600,
				Accepted: 5}}); err != nil {
				t.Fatal(err)
			}
		}
		const odd = "Opera/9.80 (Windows NT 6.1) Presto/2.12 Version/12.16"
		evs := []store.Event{
			req("192.0.2.210", "claude-code/2.1 (cli)", "", "/mcp", 401, `{}`),
			req("192.0.2.211", "some-agent/1", "", "/v1/x", 200, `{}`),
			req("192.0.2.212", "some-agent/1", "image", "/favicon.ico", 200, `{}`),
			req("192.0.2.213", "Dalvik/2.1.0 (Linux; U; Android 14)", "", "/v1/x", 200, ""),
			req("192.0.2.214", odd, "document", "/", 200, ""),
			{Source: "browser", Kind: "page_view", IP: "192.0.2.214", UA: odd},
			account(req("192.0.2.217", odd, "document", "/", 200, "")),
			account(req("192.0.2.217", odd, "empty", "/api/me", 200, "")),
		}
		// A person, and a program beside them.
		evs = append(evs, req("192.0.2.215", browserUA, "script", "/assets/a.js", 200, ""),
			req("192.0.2.215", "some-agent/1", "", "/v1/x", 200, `{}`))
		for range 5 {
			evs = append(evs, req("192.0.2.215", browserUA, "document", "/", 200, ""))
		}
		r.insert(t, evs...)
		if err := r.c.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
		for key, want := range map[string]string{
			"192.0.2.210": "unknown|client claude-code/2.1",
			"192.0.2.211": "unknown|client some-agent/1",
			"192.0.2.212": "unknown|client some-agent/1",
			"192.0.2.213": "unknown|client Dalvik/2.1.0",
			"192.0.2.214": "human|script beacon,no assets",
			"192.0.2.215": "human|assets loaded",
			// A document loaded with an account: a person's browser, however odd its user agent.
			"192.0.2.217": "human|signed in,no assets",
		} {
			if script && key == "192.0.2.215" {
				want += ",no script beacon"
			}
			if script && key == "192.0.2.217" {
				want = "unknown|no sign of a browser" // the script would have shown it
			}
			a := classes[key]
			if got := a.Class + "|" + strings.Join(a.Reasons, ","); got != want {
				t.Errorf("script %v, %s = %q, want %q", script, key, got, want)
			}
		}
		for _, tt := range []struct {
			ip, ua string
			want   int
		}{
			{"192.0.2.214", odd, store.ClsHuman},
			{"192.0.2.215", "some-agent/1", store.ClsUnknown},
			{"192.0.2.215", "other-agent/1", store.ClsUnknown}, // new behind the person
			{"192.0.2.216", "other-agent/1", store.ClsUnknown}, // new altogether
			{"192.0.2.215", browserUA, store.ClsHuman},
		} {
			e := req(tt.ip, tt.ua, "", "/v1/x", 200, `{}`)
			e.SiteID, e.IPKey, e.TS = r.site.ID, e.IP, r.now.UnixMilli()
			r.c.Enrich(&e)
			if e.Cls != tt.want {
				t.Errorf("script %v, new request of %s %q: %d, want %d", script, tt.ip, tt.ua, e.Cls, tt.want)
			}
		}
	}
}

// On a site that cannot show a browser a person's browser stays one beside a program that calls more; a rotating
// program's names are few reasons; behind people with more clients than the pass weighs the programs stay programs.
func TestPass_ProgramsBesidePeople(t *testing.T) {
	r := newRig(t, true)
	evs := []store.Event{req("192.0.2.220", browserUA, "document", "/", 200, ""),
		req("192.0.2.222", browserUA, "document", "/docs", 200, ""),
		account(req("192.0.2.222", "sdk/1", "", "/v1/x", 200, `{}`))}
	for range 3 {
		evs = append(evs, req("192.0.2.220", "some-agent/1", "", "/v1/x", 200, `{}`),
			req("192.0.2.222", "some-agent/1", "", "/v1/x", 200, `{}`))
	}
	for i := range 10 {
		evs = append(evs, req("192.0.2.221", fmt.Sprintf("agent%c/1", 'a'+i), "", "/v1/x", 200, `{}`))
	}
	for range 300 {
		evs = append(evs, req("192.0.2.223", browserUA, "document", "/", 200, ""))
	}
	for i := range 2 * maxUAs {
		evs = append(evs, req("192.0.2.223", fmt.Sprintf("agent/1.%d", i), "", "/v1/x", 200, `{}`))
	}
	r.insert(t, evs...)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	for key, want := range map[string]string{
		"192.0.2.220": "unknown|client some-agent/1",
		"192.0.2.221": "unknown|client agenta/1,client agentb/1,client agentc/1,more clients",
	} {
		a := classes[key]
		if got := a.Class + "|" + strings.Join(a.Reasons, ","); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for _, tt := range []struct {
		ip    string
		cls   int
		count int
	}{{"192.0.2.220", store.ClsHuman, 1}, {"192.0.2.222", store.ClsHuman, 1}, {"192.0.2.222", store.ClsAPI, 1},
		{"192.0.2.223", store.ClsHuman, 300}, {"192.0.2.223", store.ClsUnknown, 2 * maxUAs}} {
		var n int
		if err := r.st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE ip = ? AND cls = ?`, tt.ip, tt.cls).Scan(&n); err != nil ||
			n != tt.count {
			t.Errorf("%s: %d events of class %d, want %d, %v", tt.ip, n, tt.cls, tt.count, err)
		}
	}
}

// A GET of an API outside /api/ is no page load: a key-holding script wearing a browser's user agent is a program on a
// site with the script and on one without; without Sec-Fetch but with Accept-Language nobody's browser, never a person.
// A headless browser rendering pages with a session is no program of the API's, nor is a program without an account
// beside one with it a person or the API client.
func TestPass_APIClientPages(t *testing.T) {
	for _, script := range []bool{false, true} {
		r := newRig(t, true)
		if script {
			if err := r.st.AddStats(t.Context(), []store.IngestStat{{SiteID: r.site.ID, Hour: r.now.Unix() / 3600,
				Accepted: 5}}); err != nil {
				t.Fatal(err)
			}
		}
		get := func(ip, ua, props string, status int) store.Event {
			e := req(ip, ua, "", "/v1/x", status, props)
			e.Method = "GET"
			return e
		}
		const headless = "Mozilla/5.0 (X11; Linux x86_64) HeadlessChrome/141 Safari/537.36"
		// The feed carries the fetch headers: somebody sends them.
		evs := []store.Event{req("192.0.2.180", browserUA, "document", "/", 200, "")}
		for range 20 {
			evs = append(evs, account(get("192.0.2.181", browserUA, `{}`, 200)),
				account(get("192.0.2.182", browserUA, `{"lang":"en"}`, 200)),
				account(get("192.0.2.184", loomUA, `{}`, 200)))
		}
		for range 5 {
			evs = append(evs, account(req("66.249.66.11", headless, "document", "/a", 200, "")),
				account(req("66.249.66.11", headless, "script", "/assets/a.js", 200, "")),
				get("192.0.2.184", "some-agent/1", `{}`, 401))
		}
		r.insert(t, evs...)
		if err := r.c.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
		want := map[string]string{"192.0.2.181": "api", "66.249.66.11": "bot", "192.0.2.184": "api"}
		if script {
			want["192.0.2.182"] = "unknown"
		}
		for key, class := range want {
			if a := classes[key]; a.Class != class {
				t.Errorf("script %v, %s = %+v, want %s", script, key, a, class)
			}
		}
		var cls int
		if err := r.st.DB.QueryRow(`SELECT MAX(cls) FROM event WHERE ip = '192.0.2.184' AND ua = 'some-agent/1'`).
			Scan(&cls); err != nil || cls != store.ClsUnknown {
			t.Errorf("script %v: the program without an account: cls %d, %v", script, cls, err)
		}
	}
}

// Behind an API client's address only the clients the pass weighed are it: a scanner's many user agents past maxUAs are
// unknown, on the pass and between passes.
func TestPass_APIClientOverflow(t *testing.T) {
	r := newRig(t, true)
	var evs []store.Event
	for range 300 {
		evs = append(evs, account(req("192.0.2.190", loomUA, "", "/mcp", 200, `{}`)))
	}
	for i := range 2 * maxUAs {
		evs = append(evs, req("192.0.2.190", fmt.Sprintf("curl/8.%d", i), "", "/.env", 404, `{}`))
	}
	r.insert(t, evs...)
	for range 2 { // and the second pass changes nothing
		if err := r.c.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		var api, unknown int
		if err := r.st.DB.QueryRow(`SELECT SUM(cls = ?), SUM(cls = ?) FROM event WHERE ip = '192.0.2.190'`,
			store.ClsAPI, store.ClsUnknown).Scan(&api, &unknown); err != nil || api != 300 || unknown != 2*maxUAs {
			t.Errorf("%d API client, %d unknown, %v", api, unknown, err)
		}
		r.insert(t)
	}
	if a, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS); a["192.0.2.190"].Class != "api" {
		t.Errorf("actor %+v", a["192.0.2.190"])
	}
}

// An API client's builds each with their own user agent, more than the pass weighs: every signed-in request keeps its
// class. Behind a bot list's address the clients nobody weighed stay the list's, and a signed-in person's new events
// are the person's between passes.
func TestPass_SignedInRest(t *testing.T) {
	r := newRig(t, true)
	var evs []store.Event
	for i := range 2 * maxUAs {
		evs = append(evs, account(req("192.0.2.195", fmt.Sprintf("sdk/1.0.%d", i), "", "/mcp", 200, `{}`)))
	}
	const listed = "66.249.66.20"
	for range 300 {
		evs = append(evs, account(req(listed, browserUA, "document", "/", 200, "")))
	}
	for i := range 2 * maxUAs {
		evs = append(evs, req(listed, fmt.Sprintf("Mozilla/5.0 (compatible; Googlebot/2.%d)", i), "", "/", 200, ""))
	}
	r.insert(t, evs...)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		ip    string
		cls   int
		count int
	}{{"192.0.2.195", store.ClsAPI, 2 * maxUAs}, {listed, store.ClsHuman, 300}, {listed, store.ClsBot, 2 * maxUAs}} {
		var n int
		if err := r.st.DB.QueryRow(`SELECT COUNT(*) FROM event WHERE ip = ? AND cls = ?`, tt.ip, tt.cls).Scan(&n); err != nil ||
			n != tt.count {
			t.Errorf("%s: %d events of class %d, want %d, %v", tt.ip, n, tt.cls, tt.count, err)
		}
	}
	e := account(req(listed, browserUA, "document", "/b", 200, ""))
	e.SiteID, e.IPKey, e.TS = r.site.ID, e.IP, r.now.UnixMilli()
	r.c.Enrich(&e)
	if e.Cls != store.ClsHuman {
		t.Errorf("the person's next request: %d", e.Cls)
	}
}

// Small clients go unlisted only beside a person and as a small part; a page with a dot in its name is a page; Node's
// fetch, which sends Sec-Fetch-Mode, is no signed-in browser; an API client beside a scanner is no reason of the
// scanner's, nor is its pace.
func TestPass_SmallParts(t *testing.T) {
	r := newRig(t, true)
	if err := r.st.AddStats(t.Context(), []store.IngestStat{{SiteID: r.site.ID, Hour: r.now.Unix() / 3600,
		Accepted: 5}}); err != nil {
		t.Fatal(err)
	}
	page := func(ip, path string) store.Event {
		e := req(ip, "Mozilla/5.0 (X11; Linux x86_64) Chrome/120.0 Safari/537.36", "", path, 200, `{"lang":"en"}`)
		e.Method = "GET"
		return e
	}
	evs := []store.Event{
		{Source: "browser", Kind: "page_view", IP: "192.0.2.200", UA: browserUA},
		req("192.0.2.200", browserUA, "document", "/", 200, ""),
		req("192.0.2.200", browserUA, "document", "/b", 200, ""),
		req("192.0.2.200", "curl/8.4.0", "", "/.env", 404, ""), req("192.0.2.200", "curl/8.4.0", "", "/x", 404, ""),
		page("192.0.2.201", "/index.html"),
	}
	for range 5 {
		evs = append(evs, store.Event{Source: "browser", Kind: "page_view", IP: "192.0.2.201", UA: browserUA})
	}
	for i := range 15 {
		e := account(req("192.0.2.202", browserUA, "empty", "/v1/x", 200, `{"sf_mode":"cors","lang":"*"}`))
		slow := req("192.0.2.203", "curl/8.4.0", "", "/v1/x", 200, `{}`)
		e.TS, slow.TS = r.now.UnixMilli()-int64(i)*2000, r.now.UnixMilli()-int64(i)*2000
		evs = append(evs, e, slow)
	}
	for range 14 {
		evs = append(evs, account(req("192.0.2.203", "sdk/1", "", "/v1/x", 200, `{}`)))
	}
	r.insert(t, evs...)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	for key, want := range map[string]string{
		"192.0.2.200": "human|script beacon,also unknown automated: curl/8.4.0, 2 of 5 events (client curl/8.4.0)",
		"192.0.2.201": "human|script beacon",
		"192.0.2.202": "unknown|no page loaded",
		"192.0.2.203": "unknown|client curl/8.4.0,also API client: sdk/1, 14 of 29 events (signed-in API client)",
	} {
		a := classes[key]
		if got := a.Class + "|" + strings.Join(a.Reasons, ","); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	var cls int
	if err := r.st.DB.QueryRow(`SELECT cls FROM event WHERE path = '/index.html'`).Scan(&cls); err != nil || cls != store.ClsUnknown {
		t.Errorf("a page delivered without a beacon: cls %d, %v", cls, err)
	}
}

// Ten SDKs of two requests each beside a person are too much to go unlisted.
func TestPass_SmallAPIClients(t *testing.T) {
	r := newRig(t, true)
	evs := []store.Event{{Source: "browser", Kind: "page_view", IP: "192.0.2.191", UA: browserUA}}
	for range 40 {
		evs = append(evs, req("192.0.2.191", browserUA, "document", "/", 200, ""))
	}
	for i := range 10 {
		for range 2 {
			evs = append(evs, account(req("192.0.2.191", fmt.Sprintf("sdk-%d/1", i), "", "/v1/x", 200, `{}`)))
		}
	}
	r.insert(t, evs...)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	a := classes["192.0.2.191"]
	if a.Class != "human" || !slices.Contains(a.Reasons, "also API client: sdk-0/1, 2 of 61 events (signed-in API client)") {
		t.Errorf("actor %+v", a)
	}
}

// A feed without the fetch headers says nothing about a browser by their absence: a signed-in person's single-page app
// on a site without the script stays one.
func TestPass_APIClientNeedsFetchMeta(t *testing.T) {
	r := newRig(t, true)
	var evs []store.Event
	for range 10 {
		evs = append(evs, account(req("192.0.2.170", browserUA, "", "/api/x", 200, `{}`)))
	}
	r.insert(t, evs...)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	if a := classes["192.0.2.170"]; a.Class != "human" {
		t.Errorf("signed-in browser on a feed without fetch headers: %+v", a)
	}
}

// A signed-in person's machine asks for things beside the browser: a sign-in callback fetched again without its cookie
// by a link preview and by a second Safari user agent, two events of 403 (seen in production 2026-11-23). They make no
// "also" line and the browser's twin goes with the person; three such fetches are an actor of their own.
func TestPass_SideFetches(t *testing.T) {
	r := newRig(t, true)
	if err := r.st.AddStats(t.Context(), []store.IngestStat{{SiteID: r.site.ID, Hour: r.now.Unix() / 3600,
		Accepted: 5}}); err != nil {
		t.Fatal(err)
	}
	const (
		previewUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_11_1) AppleWebKit/601.2.4 (KHTML, like Gecko) " +
			"Version/9.0.1 Safari/601.2.4 facebookexternalhit/1.1 Facebot Twitterbot/1.0"
		oldUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_11_1) AppleWebKit/601.2.4 Version/9.0.1 Safari/601.2.4"
	)
	callback := func(ip, ua string) store.Event { return req(ip, ua, "", "/auth/github/callback", 403, "") }
	person := func(ip string) []store.Event {
		evs := []store.Event{req(ip, browserUA, "script", "/assets/a.js", 200, ""),
			account(req(ip, browserUA, "empty", "/api/me", 200, "")), req(ip, browserUA, "empty", "/api/me", 401, ""),
			{Source: "browser", Kind: "engagement", IP: ip, UA: browserUA, Props: engagedProps}}
		for _, status := range []int{200, 200, 304} {
			evs = append(evs, req(ip, browserUA, "document", "/", status, ""),
				store.Event{Source: "browser", Kind: "page_view", IP: ip, UA: browserUA})
		}
		return evs
	}
	r.insert(t, append(person("192.0.2.150"), callback("192.0.2.150", previewUA), callback("192.0.2.150", oldUA))...)
	// Three: no side fetch. A scanner with a browser's user agent does not hide behind the person.
	r.insert(t, append(person("192.0.2.151"), callback("192.0.2.151", oldUA), callback("192.0.2.151", oldUA),
		callback("192.0.2.151", oldUA))...)
	// The same fetch with nobody beside it is nobody's.
	r.insert(t, callback("192.0.2.152", oldUA))
	// A page delivered and no beacon: the rule as it was.
	r.insert(t, req("192.0.2.153", browserUA, "document", "/", 304, ""))
	// Signed in, the tracker blocked and the assets cached: the account shows the person.
	r.insert(t, req("192.0.2.154", browserUA, "document", "/", 304, ""),
		account(req("192.0.2.154", browserUA, "empty", "/api/me", 200, "")))
	// A scanner rotating browsers' user agents, two probes each, behind a person: the same size one by one.
	evs := person("192.0.2.155")
	for i := range 20 {
		ua := fmt.Sprintf("Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/%d.0 Safari/537.36", 100+i)
		evs = append(evs, req("192.0.2.155", ua, "", "/.env", 404, ""), req("192.0.2.155", ua, "", "/wp-login.php", 404, ""))
	}
	// Three tools of two events each behind a person: too much to be side fetches, so listed.
	evs = append(evs, person("192.0.2.156")...)
	for _, ua := range []string{"curl/8.4.0", "python-requests/2.32", "Wget/1.21"} {
		evs = append(evs, req("192.0.2.156", ua, "", "/x", 404, ""), req("192.0.2.156", ua, "", "/y", 404, ""))
	}
	// A key-holding script wearing a browser's user agent on a site with the script: a program, or, sending
	// Accept-Language, nobody's browser; never a person.
	for range 20 {
		evs = append(evs, account(req("192.0.2.157", browserUA, "", "/v1/x", 200, `{}`)),
			account(req("192.0.2.158", browserUA, "", "/v1/x", 200, `{"lang":"en"}`)))
	}
	r.insert(t, evs...)
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	classes, _ := r.st.ActorClasses(t.Context(), r.site.ID, r.now.UnixMilli()/dayMS)
	for key, want := range map[string]string{
		"192.0.2.150": "human|script beacon,engaged,assets loaded,signed in",
		"192.0.2.151": "human|script beacon,engaged,assets loaded,signed in,also unknown automated: " +
			"Safari" + ensp + "·" + ensp + "macOS, 3 of 13 events (no page loaded)",
		"192.0.2.152": "unknown|no page loaded",
		"192.0.2.153": "unknown|no sign of a browser",
		"192.0.2.154": "human|signed in,no script beacon,no assets",
		"192.0.2.155": "unknown|no page loaded,mostly refused",
		"192.0.2.156": "human|script beacon,engaged,assets loaded,signed in," +
			"also unknown automated: Wget/1.21, 2 of 16 events (client Wget/1.21)," +
			"also unknown automated: curl/8.4.0, 2 of 16 events (client curl/8.4.0)," +
			"also unknown automated: python-requests/2.32, 2 of 16 events (client python-requests/2.32)",
		"192.0.2.157": "api|signed-in API client",
		"192.0.2.158": "unknown|API only, no page loaded,rapid requests",
	} {
		a := classes[key]
		if got := a.Class + "|" + strings.Join(a.Reasons, ","); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	for ua, want := range map[string]int{previewUA: store.ClsUnknown, oldUA: store.ClsHuman} {
		var cls int
		if err := r.st.DB.QueryRow(`SELECT cls FROM event WHERE ip = '192.0.2.150' AND ua = ?`, ua).Scan(&cls); err != nil ||
			cls != want {
			t.Errorf("%q: cls %d, want %d, %v", ua, cls, want, err)
		}
	}
}

// A request answered with an account is an API client's when written, over the lists and over a pass that saw the
// client without one.
func TestEnrich_APIClient(t *testing.T) {
	r := newRig(t, true)
	r.insert(t, req("192.0.2.160", loomUA, "", "/mcp", 401, `{}`),
		account(req("192.0.2.164", loomUA, "", "/mcp", 200, `{}`)), account(req("192.0.2.164", loomUA, "", "/mcp", 200, `{}`)),
		// One good answer among refusals: the pass saw the account and said no.
		account(req("192.0.2.166", loomUA, "", "/mcp", 200, `{}`)), req("192.0.2.166", loomUA, "", "/mcp", 401, `{}`),
		req("192.0.2.166", loomUA, "", "/mcp", 401, `{}`))
	if err := r.c.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		e    store.Event
		want int
	}{
		{"after a pass", account(req("192.0.2.160", loomUA, "", "/mcp", 200, `{}`)), store.ClsAPI},
		{"listed address", account(req("66.249.66.1", loomUA, "", "/mcp", 200, `{}`)), store.ClsAPI},
		{"refused", account(req("192.0.2.161", loomUA, "", "/mcp", 429, `{}`)), store.ClsUnknown},
		{"a browser", account(req("192.0.2.162", browserUA, "empty", "/api/me", 200, "")), store.ClsPending},
		{"no account", req("192.0.2.163", loomUA, "", "/mcp", 200, `{}`), store.ClsUnknown},
		{"decided against", account(req("192.0.2.166", loomUA, "", "/mcp", 200, `{}`)), store.ClsUnknown},
		{"a 404 lookup", account(req("192.0.2.165", loomUA, "", "/mcp", 404, `{}`)), store.ClsAPI},
		// Behind an address the pass classed API client: its own refused request stays its own, a newcomer is not it.
		{"its own refusal", req("192.0.2.164", loomUA, "", "/mcp", 401, `{}`), store.ClsAPI},
		{"a scanner beside it", req("192.0.2.164", "curl/8.4.0", "", "/.env", 404, `{}`), store.ClsUnknown},
		{"a page beside it", req("192.0.2.164", browserUA, "document", "/", 200, ""), store.ClsPending},
		{"a beacon beside it", store.Event{Source: "browser", Kind: "page_view", IP: "192.0.2.164", UA: browserUA,
			Props: `{"ctx":{"webdriver":false}}`}, store.ClsHuman},
	} {
		tt.e.SiteID, tt.e.IPKey, tt.e.TS = r.site.ID, tt.e.IP, r.now.UnixMilli()
		r.c.Enrich(&tt.e)
		if tt.e.Cls != tt.want {
			t.Errorf("%s: %d, want %d", tt.name, tt.e.Cls, tt.want)
		}
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
