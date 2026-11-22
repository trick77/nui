package sim

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/trick77/ship/kit/ratelimit"
)

// Scenario is one named story on top of the world, active in a window relative to the anchor (negative: in the
// history). Its expectations say what the dashboard shows for it, so a backfill of it is a test fixture.
type Scenario struct {
	Name  string
	Story string
	// From and To bound the window, relative to the anchor; both zero: the whole time.
	From, To time.Duration
	// Domain is the site it acts on, "" for the scenario's own choice.
	Domain string
	// MinDays is the history the expectations need.
	MinDays int
	expect  func(w *World, sc *Scenario) []Expectation
}

func (s *Scenario) always() bool { return s.From == 0 && s.To == 0 }

func (s *Scenario) start(anchor time.Time) time.Time { return anchor.Add(s.From) }

func (s *Scenario) end(anchor time.Time) time.Time { return anchor.Add(s.To) }

func (s *Scenario) covers(anchor, t time.Time) bool {
	return s.always() || (!t.Before(s.start(anchor)) && t.Before(s.end(anchor)))
}

// site reports whether the scenario acts on s.
func (s *Scenario) site(w *World, site *Site) bool {
	if s.Domain != "" {
		return site.Domain == s.Domain
	}
	switch s.Name {
	case "late-feed":
		// The last site with a feed: a product added later.
		for i := len(w.Sites) - 1; i >= 0; i-- {
			if w.Sites[i].Feed && w.Sites[i].Kind != "api" && !w.Sites[i].Silent {
				return w.Sites[i] == site
			}
		}
		return false
	}
	return site.idx == w.firstFeed()
}

// Expectation is one thing the dashboard shows for a scenario: the request that shows it and a check of the answer.
type Expectation struct {
	Scenario string
	// Says is the expectation in words.
	Says     string
	Endpoint string
	Params   map[string]string
	// Check inspects the endpoint's JSON answer.
	Check func(body map[string]any) error
}

const h = time.Hour

// catalogue is every scenario, with its default window.
var catalogue = []Scenario{
	{Name: "baseline", Story: "A quiet product: people on their daily and weekly rhythm, a crawler, a crawler " +
		"rendering pages, an uptime probe. Always on.", expect: expectBaseline},
	{Name: "referrer-spike", Story: "A post on a news site sends a wave of first-time visitors to one lookup page.",
		From: -30 * h, To: -20 * h, expect: expectReferrer},
	{Name: "scan-wave", Story: "A dozen hosting addresses probe for secrets, admin pages and backups.",
		From: -40 * h, To: -28 * h, expect: expectScan},
	{Name: "api-walker", Story: "A script walks the lookup API address by address with a wrong key, every day, " +
		"a request each 0.8 s.", expect: expectWalker},
	{Name: "deploy-error", Story: "A release breaks the lookup page (a script error, some 502s from the API); " +
		"a fix ships three days later.", From: -5*24*h + 14*h, To: -2*24*h + 10*h, MinDays: 7, expect: expectDeploy},
	{Name: "flood-addr", Story: "One address floods the start page for an hour.", From: -6 * h, To: -5 * h,
		expect: expectFlood},
	{Name: "flood-64", Story: "One IPv6 /64 floods the start page for an hour, every request from another address.",
		From: -9 * h, To: -8 * h, expect: expectFlood},
	{Name: "flood-48", Story: "A /48 floods the start page for an hour from 200 /64s, none of them large alone.",
		From: -12 * h, To: -11 * h, expect: expectFlood},
	{Name: "feed-outage", Story: "The product's feed emitter is down for six hours, then recovers; its records " +
		"of that time are lost.", From: -20 * h, To: -14 * h, expect: expectOutage},
	{Name: "late-feed", Story: "A site's feed is set up three days before the anchor: its script delivered all " +
		"along.", From: -3 * 24 * h, To: 1000 * 24 * h, MinDays: 7, expect: expectLateFeed},
	{Name: "growth", Story: "The product takes off: its new visitors double every quarter, steadily.",
		MinDays: 180, expect: expectGrowth},
	{Name: "hammer", Story: "A client posts tracker batches four times a second for half an hour and ignores 429.",
		From: -10 * h, To: -9*h - 30*time.Minute, expect: expectHammer},
	{Name: "headless", Story: "A headless browser crawls pages twice a day; the script runs and reports " +
		"navigator.webdriver.", MinDays: 2, expect: expectHeadless},
	{Name: "cred-prober", Story: "An address tries logins on the API every two seconds for an hour, an account " +
		"list in turn.", From: -14 * h, To: -13 * h, expect: expectProber},
}

// Catalogue lists the scenarios.
func Catalogue() []Scenario { return slices.Clone(catalogue) }

// DefaultScenarios is what server -demo and make sim play: the stories that sit well together.
const DefaultScenarios = "baseline,referrer-spike,scan-wave,api-walker,deploy-error,headless,cred-prober,growth"

// Scenarios parses a comma-separated list of names ("all" for every one), baseline always included.
func Scenarios(list string) ([]*Scenario, error) {
	names := map[string]bool{"baseline": true}
	for _, n := range strings.Split(list, ",") {
		n = strings.TrimSpace(n)
		switch n {
		case "":
		case "all":
			for _, c := range catalogue {
				names[c.Name] = true
			}
		default:
			if !slices.ContainsFunc(catalogue, func(c Scenario) bool { return c.Name == n }) {
				return nil, fmt.Errorf("unknown scenario %q", n)
			}
			names[n] = true
		}
	}
	var out []*Scenario
	for _, c := range catalogue {
		if names[c.Name] {
			sc := c
			out = append(out, &sc)
		}
	}
	return out, nil
}

// Expectations are the active scenarios' expectations for this world.
func (w *World) Expectations() []Expectation {
	var out []Expectation
	for _, sc := range w.Scenarios {
		if sc.expect == nil {
			continue
		}
		for _, e := range sc.expect(w, sc) {
			e.Scenario = sc.Name
			out = append(out, e)
		}
	}
	return out
}

func params(site, rng string, kv ...string) map[string]string {
	m := map[string]string{"site": site, "range": rng, "tz": "UTC"}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// The checks read the dashboard's JSON as decoded into maps.

func items(body map[string]any, key string) []map[string]any {
	xs, _ := body[key].([]any)
	out := make([]map[string]any, 0, len(xs))
	for _, x := range xs {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// findings is the Noteworthy request of every finding (findings=all), the ones the panel holds back past its five
// included: a world playing several scenarios has more findings than the panel shows.
func findings(site, rng string) map[string]string { return params(site, rng, "findings", "all") }

// hasFinding checks a Noteworthy answer for a finding id, among every item's findings (a briefing line merges
// several). Asked with findings(), the answer holds them all.
func hasFinding(id string) func(map[string]any) error {
	return func(body map[string]any) error {
		var ids []string
		for _, it := range items(body, "items") {
			xs, _ := it["ids"].([]any)
			for _, x := range xs {
				ids = append(ids, fmt.Sprint(x))
			}
		}
		if slices.Contains(ids, id) {
			return nil
		}
		return fmt.Errorf("no finding %s among %v (and %v more)", id, ids, body["more"])
	}
}

// rowWith checks a list for a row whose key is key, then check on it.
func rowWith(key string, check func(map[string]any) error) func(map[string]any) error {
	return func(body map[string]any) error {
		for _, r := range items(body, "rows") {
			if r["key"] == key {
				if check == nil {
					return nil
				}
				return check(r)
			}
		}
		return fmt.Errorf("no row %q", key)
	}
}

// classed checks a visitors row: its class, and reasons among its signals.
func classed(class string, reasons ...string) func(map[string]any) error {
	return func(r map[string]any) error {
		if r["class"] != class {
			return fmt.Errorf("class %v, want %s (signals %v)", r["class"], class, r["signals"])
		}
		sig := fmt.Sprint(r["signals"])
		for _, want := range reasons {
			if !strings.Contains(sig, want) {
				return fmt.Errorf("signals %v lack %q", r["signals"], want)
			}
		}
		return nil
	}
}

// visitor is the expectation that the visitors table classes key so.
func visitor(says, site, rng, key, class string, reasons ...string) Expectation {
	return Expectation{Says: says, Endpoint: "visitors", Params: params(site, rng, "q", key, "limit", "50"),
		Check: rowWith(key, classed(class, reasons...))}
}

func tileSeries(body map[string]any, key string) []float64 {
	for _, t := range items(body, "tiles") {
		if t["key"] == key {
			xs, _ := t["series"].([]any)
			out := make([]float64, len(xs))
			for i, x := range xs {
				out[i], _ = x.(float64)
			}
			return out
		}
	}
	return nil
}

func expectBaseline(w *World, _ *Scenario) []Expectation {
	var out []Expectation
	for _, s := range w.Sites {
		if s.Silent || s.Kind == "api" || !s.Script {
			continue
		}
		out = append(out, Expectation{Says: s.Domain + ": Overview shows visitors and page views",
			Endpoint: "overview", Params: params(s.Domain, "7d"), Check: func(b map[string]any) error {
				for _, k := range []string{"visitors", "views"} {
					var sum float64
					for _, v := range tileSeries(b, k) {
						sum += v
					}
					if sum == 0 {
						return fmt.Errorf("no %s", k)
					}
				}
				return nil
			}})
	}
	if f := w.firstFeed(); f >= 0 {
		s := w.Sites[f]
		if a, ok := w.BotAddr("monitor", f); ok {
			out = append(out, visitor("the probe is an uptime probe, by its address range", s.Domain, "24h",
				a.String(), "monitor", "address range"))
		}
		if a, ok := w.BotAddr("crawler", f); ok {
			out = append(out, visitor("the crawler is a known bot, by its address range", s.Domain, "7d",
				a.String(), "bot", "address range"))
		}
	}
	return out
}

func expectReferrer(w *World, _ *Scenario) []Expectation {
	s := w.Sites[0].Domain
	return []Expectation{
		{Says: "Noteworthy lists the new referrer news.example.com", Endpoint: "noteworthy", Params: findings(s, "7d"),
			Check: hasFinding("ref:news.example.com")},
		{Says: "the referrers list leads with news.example.com", Endpoint: "list",
			Params: params(s, "7d", "dim", "referrers"), Check: func(b map[string]any) error {
				if rows := items(b, "rows"); len(rows) == 0 || rows[0]["key"] != "news.example.com" {
					return fmt.Errorf("top referrer %v", rows)
				}
				return nil
			}},
	}
}

func expectScan(w *World, _ *Scenario) []Expectation {
	s := w.Sites[w.firstFeed()].Domain
	return []Expectation{
		{Says: "Noteworthy lists the new probed path GET /.env", Endpoint: "noteworthy", Params: findings(s, "7d"),
			Check: hasFinding("probe:GET /.env")},
		{Says: "the probed list has GET /.env answered 404", Endpoint: "list",
			Params: params(s, "7d", "dim", "probed", "limit", "50"), Check: rowWith("GET /.env", func(r map[string]any) error {
				if r["sub"] != "404" {
					return fmt.Errorf("sub %v", r["sub"])
				}
				return nil
			})},
	}
}

func expectWalker(w *World, _ *Scenario) []Expectation {
	s := w.Sites[w.firstFeed()].Domain
	key := WalkerAddr.String()
	return []Expectation{
		visitor("the walker is unknown automated: its client, mostly refused, API only", s, "24h", key, "unknown",
			"client "+WalkerUA, "mostly refused", "API only, no page loaded"),
		{Says: "its trail is runs of GET /api/v1/ip/{addr} → 401, 0.8 s apart", Endpoint: "visitor",
			Params: params(s, "24h", "key", key), Check: func(b map[string]any) error {
				tr := items(b, "trail")
				if len(tr) == 0 || tr[0]["what"] != "GET /api/v1/ip/{addr} → 401" || tr[0]["interval_s"] != 0.8 {
					return fmt.Errorf("trail %v", tr)
				}
				return nil
			}},
	}
}

func expectDeploy(w *World, _ *Scenario) []Expectation {
	s := w.Sites[0].Domain
	return []Expectation{
		{Says: "Noteworthy lists the release's script error as new", Endpoint: "noteworthy", Params: findings(s, "7d"),
			Check: hasFinding("err:" + DeployError)},
		{Says: "the status breakdown has 5xx answers", Endpoint: "status", Params: params(s, "7d"),
			Check: func(b map[string]any) error {
				for _, r := range items(b, "rows") {
					if r["class"] == "5xx" {
						return nil
					}
				}
				return fmt.Errorf("no 5xx in %v", b["rows"])
			}},
		{Says: "the last day, after the fix, has no such error", Endpoint: "list",
			Params: params(s, "24h", "dim", "errors", "limit", "50"), Check: func(b map[string]any) error {
				if rowWith(DeployError, nil)(b) == nil {
					return fmt.Errorf("the fixed error is still listed")
				}
				return nil
			}},
	}
}

func expectFlood(w *World, sc *Scenario) []Expectation {
	s := w.Sites[w.firstFeed()].Domain
	id := map[string]string{"flood-addr": "spike:addr:" + FloodAddr.String(),
		"flood-64": "spike:addr:" + ratelimit.Key(hostIn(Flood64, 1).String()), "flood-48": "spike:net:64506"}[sc.Name]
	return []Expectation{{Says: "Noteworthy lists the spike " + id, Endpoint: "noteworthy", Params: findings(s, "24h"),
		Check: hasFinding(id)}}
}

func expectOutage(w *World, sc *Scenario) []Expectation {
	var s string
	for _, x := range w.Sites {
		if sc.site(w, x) {
			s = x.Domain
		}
	}
	return []Expectation{{Says: "the requests series drops to zero for the outage's hours and comes back",
		Endpoint: "overview", Params: params(s, "24h"), Check: func(b map[string]any) error {
			// The longest run of empty hours with requests on both sides of it.
			ser := tileSeries(b, "requests")
			best, run, seen := 0, 0, false
			for _, v := range ser {
				switch {
				case v == 0 && seen:
					run++
				case v != 0:
					if seen {
						best = max(best, run)
					}
					run, seen = 0, true
				}
			}
			if best < 5 {
				return fmt.Errorf("no outage then recovery in %v", ser)
			}
			return nil
		}}}
}

func expectLateFeed(w *World, sc *Scenario) []Expectation {
	var s string
	for _, x := range w.Sites {
		if sc.site(w, x) {
			s = x.Domain
		}
	}
	return []Expectation{{Says: s + ": no requests before the feed's start, page views all along",
		Endpoint: "overview", Params: params(s, "7d"), Check: func(b map[string]any) error {
			req, views := tileSeries(b, "requests"), tileSeries(b, "views")
			if len(req) != 7 || req[0] != 0 || req[1] != 0 || req[5] == 0 || views[0] == 0 {
				return fmt.Errorf("requests %v, views %v", req, views)
			}
			return nil
		}}}
}

func expectGrowth(w *World, _ *Scenario) []Expectation {
	s := w.Sites[0].Domain
	return []Expectation{{Says: "Noteworthy has visitors up against the prior 90 days", Endpoint: "noteworthy",
		Params: findings(s, "90d"), Check: hasFinding("trend:visitors:up")}}
}

func expectHammer(w *World, _ *Scenario) []Expectation {
	s := w.Sites[w.firstScript()].Domain
	return []Expectation{
		{Says: "Noteworthy has the collector's refusals as a problem", Endpoint: "noteworthy", Params: findings(s, "24h"),
			Check: hasFinding("collector:refused")},
		{Says: "the footer counts rate-limited events", Endpoint: "domain", Params: params(s, "24h"),
			Check: func(b map[string]any) error {
				c, _ := b["counters"].(map[string]any)
				if v, _ := c["rate_limited"].(float64); v <= 0 {
					return fmt.Errorf("counters %v", c)
				}
				return nil
			}},
		visitor("the hammer is unknown automated by its client", s, "24h", HammerAddr.String(), "unknown",
			"client "+HammerUA),
	}
}

func expectHeadless(w *World, _ *Scenario) []Expectation {
	s := w.Sites[w.firstScript()].Domain
	return []Expectation{visitor("the headless browser is unknown automated: navigator.webdriver set", s, "7d",
		HeadlessAddr.String(), "unknown", "navigator.webdriver set")}
}

func expectProber(w *World, _ *Scenario) []Expectation {
	s := w.Sites[w.firstFeed()].Domain
	return []Expectation{
		{Says: "Noteworthy lists the new probed path POST /api/v1/login", Endpoint: "noteworthy",
			Params: findings(s, "24h"), Check: hasFinding("probe:POST /api/v1/login")},
		visitor("the prober is unknown automated: its client, mostly refused", s, "24h", ProberAddr.String(),
			"unknown", "client "+ProberUA, "mostly refused"),
	}
}
