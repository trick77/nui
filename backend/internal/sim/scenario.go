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
	{Name: "growth", Story: "The product takes off: its new visitors double every quarter over the history; " +
		"live time holds the start's level.",
		MinDays: 180, expect: expectGrowth},
	{Name: "hammer", Story: "A client posts tracker batches four times a second for half an hour and ignores 429.",
		From: -10 * h, To: -9*h - 30*time.Minute, expect: expectHammer},
	{Name: "headless", Story: "A headless browser crawls pages twice a day; the script runs and reports " +
		"navigator.webdriver.", MinDays: 2, expect: expectHeadless},
	{Name: "cred-prober", Story: "An address tries logins on the API every two seconds for an hour, an account " +
		"list in turn.", From: -14 * h, To: -13 * h, expect: expectProber},
	// The watch's stories need its learning window behind them: a week from the history's first day.
	{Name: "path-flip", Story: "Scanners probe a dropped PHP file for two days and are refused; then it answers, " +
		"a webshell in place, and its owner calls it.", From: -50 * h, To: -9 * h, MinDays: 9, expect: expectFlip},
	{Name: "scanner-hit", Story: "A scanner walks two dozen paths, refused at every one but a database dump " +
		"nobody asked for before.", From: -30 * h, To: -29 * h, MinDays: 9, expect: expectHit},
	{Name: "post-target", Story: "A PHP path nobody posted to before takes POSTs and answers them.", From: -23 * h,
		To: -22 * h, MinDays: 9, expect: expectPostTarget},
	{Name: "script-inject", Story: "A host the pages never loaded scripts from shows up on every page: a skimmer " +
		"injected into the site.", From: -40 * h, To: 1000 * 24 * h, MinDays: 9, expect: expectInject},
	{Name: "soft-404", Story: "The blog starts answering every unknown path with a page of its own, 200: probes " +
		"refused before are answered now, as is a scanner's spray of new paths.", From: -40 * h, To: 1000 * 24 * h,
		Domain: "blog.example.org", MinDays: 9, expect: expectSoft},
}

// Catalogue lists the scenarios.
func Catalogue() []Scenario { return slices.Clone(catalogue) }

// DefaultScenarios is what server -demo and make sim play: the stories that sit well together.
const DefaultScenarios = "baseline,referrer-spike,scan-wave,api-walker,deploy-error,headless,cred-prober,growth," +
	"path-flip,scanner-hit,post-target,script-inject"

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

// Around a scenario whose expectations read alerts, the alert engine steps from alertLead before its start (that
// step a baseline the scenario is not part of yet) to alertClear after its end (a condition clears with the window
// after it): every hour near the start and the end, every AlertEvery hours between and after. A fact the scenario
// makes later than its first hours (a few visitors at scale 0.3) still meets a facts pass while it is new.
const (
	alertLead = h
	alertTail = 2 * h
	// AlertEvery is the hours between two steps away from a scenario's edges: short of the engine's six idle hours,
	// which make a rule's next pass a baseline again.
	AlertEvery = 5
	// alertClear is how long after the end the steps go on: one of them, whatever the hour, is past the longest window
	// of a simulated rule (errors-up's 12 hours).
	alertClear = (12 + AlertEvery) * h
)

// AlertHours reports the hours, by their end, a backfill steps the alert engine through for the expectations that
// read alerts: around each such scenario, every hour for one that is always on. Nil when no expectation reads
// alerts: no step at all. A history that steps every hour of days takes minutes, under -race more than CI's ten a
// package.
func (w *World) AlertHours() func(end time.Time) bool {
	type span struct{ start, end time.Time }
	var spans []span
	for _, sc := range w.Scenarios {
		if sc.expect == nil || !slices.ContainsFunc(sc.expect(w, sc), func(e Expectation) bool {
			return e.Endpoint == "alerts"
		}) {
			continue
		}
		if sc.always() {
			return func(time.Time) bool { return true }
		}
		spans = append(spans, span{sc.start(w.Anchor), sc.end(w.Anchor)})
	}
	if spans == nil {
		return nil
	}
	return func(t time.Time) bool {
		return slices.ContainsFunc(spans, func(s span) bool {
			if t.Before(s.start.Add(-alertLead)) || t.After(s.end.Add(alertClear)) {
				return false
			}
			return !t.After(s.start.Add(alertTail)) || (!t.Before(s.end.Add(-h)) && !t.After(s.end.Add(alertTail))) ||
				t.Unix()/3600%AlertEvery == 0
		})
	}
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

// findingIDs are a Noteworthy answer's finding ids, every item's (a briefing line merges several).
func findingIDs(body map[string]any) []string {
	var ids []string
	for _, it := range items(body, "items") {
		xs, _ := it["ids"].([]any)
		for _, x := range xs {
			ids = append(ids, fmt.Sprint(x))
		}
	}
	return ids
}

// hasFinding checks a Noteworthy answer for a finding id. Asked with findings(), the answer holds them all.
func hasFinding(id string) func(map[string]any) error {
	return func(body map[string]any) error {
		ids := findingIDs(body)
		if slices.Contains(ids, id) {
			return nil
		}
		return fmt.Errorf("no finding %s among %v (and %v more)", id, ids, body["more"])
	}
}

// errorAt finds a script error's only place.
func errorAt(origin, place string) func(map[string]any) error {
	return func(body map[string]any) error {
		places, _ := body["places"].(map[string]any)
		rows := items(places, "rows")
		if len(rows) != 1 {
			return fmt.Errorf("%d places of %v occurrences", len(rows), body["count"])
		}
		if w, _ := rows[0]["where"].(map[string]any); w["origin"] != origin || w["place"] != place {
			return fmt.Errorf("place %v", w)
		}
		return nil
	}
}

// noFinding checks a Noteworthy answer for no finding whose id starts with prefix. Asked with findings().
func noFinding(prefix string) func(map[string]any) error {
	return func(body map[string]any) error {
		for _, id := range findingIDs(body) {
			if strings.HasPrefix(id, prefix) {
				return fmt.Errorf("finding %s", id)
			}
		}
		return nil
	}
}

// watchRow checks the watch's answer for a row of list (paths, hosts) whose field is value, then check on it.
func watchRow(list, field, value string, check func(map[string]any) error) func(map[string]any) error {
	return func(body map[string]any) error {
		for _, r := range items(body, list) {
			if r[field] == value {
				if check == nil {
					return nil
				}
				return check(r)
			}
		}
		return fmt.Errorf("no %s row %q in %v", list, value, body[list])
	}
}

// alerts is the alerts request of a site's history, as many as one page holds.
func alerts(site string) map[string]string { return map[string]string{"site": site, "limit": "200"} }

// hasAlert checks an alerts answer for an alert of kind on key in state whose last message was sent (the
// simulator's recording channel takes every one).
func hasAlert(kind, key, state string) func(map[string]any) error {
	return alertWith(kind, key, state, nil)
}

// alertWith is hasAlert, then check on the alert.
func alertWith(kind, key, state string, check func(map[string]any) error) func(map[string]any) error {
	return func(body map[string]any) error {
		var seen []string
		for _, a := range items(body, "alerts") {
			seen = append(seen, fmt.Sprintf("%v %q %v", a["kind"], a["key"], a["state"]))
			if a["kind"] != kind || a["key"] != key || a["state"] != state {
				continue
			}
			if d, _ := a["delivery"].(map[string]any); d["state"] != "sent" {
				return fmt.Errorf("%s %q %s delivered %v", kind, key, state, d)
			}
			if check != nil {
				return check(a)
			}
			return nil
		}
		return fmt.Errorf("no %s %q %s among %v", kind, key, state, seen)
	}
}

// errorsUp checks an alerts answer for the errors' level alert of a release from start to end: opened in it, cleared
// after it, its clearing sent (and so its opening, engine.leave), then its words and its figure, at least the simulated
// rule's min of 30 %. A quiet site's level may open and clear more than once within the release: one such alert is
// enough.
func errorsUp(start, end time.Time) func(map[string]any) error {
	at := func(v any) time.Time {
		t, _ := time.Parse(time.RFC3339, fmt.Sprint(v))
		return t
	}
	return func(body map[string]any) error {
		var seen []string
		for _, a := range items(body, "alerts") {
			if a["kind"] != "level.errors" {
				continue
			}
			d, _ := a["delivery"].(map[string]any)
			seen = append(seen, fmt.Sprintf("%v %q %v to %v, %v", a["state"], a["key"], a["first_at"], a["cleared_at"],
				d["state"]))
			opened := at(a["first_at"])
			if a["key"] != "" || a["state"] != "cleared" || d["state"] != "sent" || opened.Before(start) ||
				!opened.Before(end) || !at(a["cleared_at"]).After(end) {
				continue
			}
			tg, _ := a["target"].(map[string]any)
			detail := fmt.Sprint(a["detail"])
			if f, _ := a["figure"].(float64); f < 30 || f > 100 || a["unit"] != "%" || tg["panel"] != "errors" ||
				!strings.HasPrefix(fmt.Sprint(a["text"]), "Visitors with a script error over the last ") ||
				(!strings.HasSuffix(detail, "): "+DeployError) && !strings.HasSuffix(detail, "): "+MaskedError)) {
				return fmt.Errorf("alert %v", a)
			}
			return nil
		}
		return fmt.Errorf("no level.errors opened from %v on and cleared after %v among %v", start, end, seen)
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

// perDay sums 7d's hours (UTC: 24 a day).
func perDay(hours []float64) []float64 {
	out := make([]float64, (len(hours)+23)/24)
	for i, v := range hours {
		out[i/24] += v
	}
	return out
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
	for _, s := range w.Sites {
		if s.Kind != "api" || !s.Feed || s.Silent {
			continue
		}
		out = append(out, Expectation{Says: s.Domain + ": the programs calling with an account are API clients, not people",
			Endpoint: "classes", Params: params(s.Domain, "7d"), Check: func(b map[string]any) error {
				got := map[string]any{}
				for _, r := range items(b, "rows") {
					class, _ := r["class"].(string)
					got[class] = r["requests"]
				}
				if n, _ := got["api"].(float64); n == 0 {
					return fmt.Errorf("no API client requests: %v", got)
				}
				if n, _ := got["human"].(float64); n != 0 {
					return fmt.Errorf("%v requests of people", n)
				}
				return nil
			}})
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
		{Says: "an alert reports the new probed path GET /.env", Endpoint: "alerts", Params: alerts(s),
			Check: hasAlert("probe.new", "GET /.env", "once")},
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
		// A route refusing a key is answered to whoever holds one: no probe flipped, no scanner's hit.
		{Says: "Noteworthy has no flipped path on the walked route", Endpoint: "noteworthy", Params: findings(s, "7d"),
			Check: noFinding("flip:GET /api/v1/ip/")},
		{Says: "Noteworthy has no scanner's hit on the walked route", Endpoint: "noteworthy", Params: findings(s, "7d"),
			Check: noFinding("hit:GET /api/v1/ip/")},
	}
}

func expectDeploy(w *World, sc *Scenario) []Expectation {
	s := w.Sites[0].Domain
	return []Expectation{
		{Says: "Noteworthy lists the release's script error as new", Endpoint: "noteworthy", Params: findings(s, "7d"),
			Check: hasFinding("err:" + DeployError)},
		{Says: "the release's error is the site's own, with file and line", Endpoint: "error",
			Params: params(s, "7d", "key", DeployError), Check: errorAt("own", "/assets/index-C4v1rT2s.js:1:48113")},
		{Says: "the release's error has its stack", Endpoint: "error", Params: params(s, "7d", "key", DeployError),
			Check: func(b map[string]any) error {
				if st, _ := b["stack"].([]any); len(st) != 2 || b["type"] != "TypeError" {
					return fmt.Errorf("type %v, stack %v", b["type"], b["stack"])
				}
				return nil
			}},
		{Says: "an error the browser hides says so, without a place", Endpoint: "error",
			Params: params(s, "7d", "key", MaskedError), Check: errorAt("withheld", "")},
		{Says: "an alert reports the release's script error once, its place, and leads to its rows in the log",
			Endpoint: "alerts", Params: alerts(s), Check: alertWith("error.new", DeployError, "once",
				func(a map[string]any) error {
					tg, _ := a["target"].(map[string]any)
					f, _ := tg["filter"].(map[string]any)
					if tg["panel"] != "log" || f["kind"] != "js_error" || f["name"] != DeployError ||
						!strings.Contains(fmt.Sprint(a["detail"]), "/assets/index-C4v1rT2s.js:1:48113") {
						return fmt.Errorf("target %v, detail %v", tg, a["detail"])
					}
					return nil
				})},
		{Says: "the errors' level rule opened for the release and cleared after its fix", Endpoint: "alerts",
			Params: alerts(s), Check: errorsUp(sc.start(w.Anchor), sc.end(w.Anchor))},
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
	out := []Expectation{{Says: "Noteworthy lists the spike " + id, Endpoint: "noteworthy", Params: findings(s, "24h"),
		Check: hasFinding(id)}}
	if sc.Name == "flood-addr" {
		out = append(out,
			Expectation{Says: "an alert reports the address's spike", Endpoint: "alerts", Params: alerts(s),
				Check: hasAlert("spike.addr", FloodAddr.String(), "once")},
			Expectation{Says: "the requests' level rule opened for the flood's hour and cleared after it",
				Endpoint: "alerts", Params: alerts(s), Check: hasAlert("level.requests", "", "cleared")})
	}
	return out
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
			// The longest run of empty slots with requests on both sides of it.
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
			if best < 5*len(ser)/24 { // five hours, of ten minutes or of thirty
				return fmt.Errorf("no outage then recovery in %v", ser)
			}
			return nil
		}}, {Says: "an alert reported the silent feed and cleared it when it came back", Endpoint: "alerts",
		Params: alerts(s), Check: hasAlert("source.silent", "server", "cleared")}}
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
			req, views := perDay(tileSeries(b, "requests")), perDay(tileSeries(b, "views"))
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
		{Says: "an alert reported the refusals and cleared once they stopped", Endpoint: "alerts", Params: alerts(s),
			Check: hasAlert("collector.refused", "", "cleared")},
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
		// A login refusing the prober (a route's 401) and answering people is neither a flip nor a scanner's hit.
		{Says: "Noteworthy has no flipped login", Endpoint: "noteworthy", Params: findings(s, "7d"),
			Check: noFinding("flip:POST /api/v1/login")},
		{Says: "Noteworthy has no scanner's hit on the login", Endpoint: "noteworthy", Params: findings(s, "7d"),
			Check: noFinding("hit:POST /api/v1/login")},
	}
}

func expectFlip(w *World, _ *Scenario) []Expectation {
	s := w.Sites[w.firstFeed()].Domain
	id := "flip:GET " + FlipPath
	return []Expectation{
		{Says: "Noteworthy lists the refused path now answered", Endpoint: "noteworthy", Params: findings(s, "7d"),
			Check: hasFinding(id)},
		{Says: "the answered probes card has it, refused on the days before", Endpoint: "watch",
			Params: params(s, "7d"), Check: watchRow("paths", "id", id, func(r map[string]any) error {
				if n, _ := r["refused_addrs"].(float64); n < 1 || r["refused_first"] == nil {
					return fmt.Errorf("row %v", r)
				}
				return nil
			})},
	}
}

func expectHit(w *World, _ *Scenario) []Expectation {
	s := w.Sites[w.firstFeed()].Domain
	id := "hit:GET " + DumpPath
	return []Expectation{
		{Says: "Noteworthy lists the scanner's hit", Endpoint: "noteworthy", Params: findings(s, "7d"),
			Check: hasFinding(id)},
		{Says: "the answered probes card names the scanner", Endpoint: "watch", Params: params(s, "7d"),
			Check: watchRow("paths", "id", id, func(r map[string]any) error {
				if r["hit_addr"] != HitScannerAddr.String() {
					return fmt.Errorf("row %v", r)
				}
				return nil
			})},
	}
}

func expectPostTarget(w *World, _ *Scenario) []Expectation {
	s := w.Sites[w.firstFeed()].Domain
	return []Expectation{{Says: "Noteworthy lists the new target POST " + PostPath, Endpoint: "noteworthy",
		Params: findings(s, "7d"), Check: hasFinding("endpoint:POST " + PostPath)}}
}

func expectInject(w *World, _ *Scenario) []Expectation {
	s := w.Sites[w.firstScript()].Domain
	return []Expectation{
		{Says: "Noteworthy lists the new script host", Endpoint: "noteworthy", Params: findings(s, "7d"),
			Check: hasFinding("script:" + InjectedHost)},
		{Says: "the script hosts card marks it new beside the known one", Endpoint: "watch", Params: params(s, "7d"),
			Check: func(b map[string]any) error {
				if err := watchRow("hosts", "host", InjectedHost, func(r map[string]any) error {
					if r["new"] != true {
						return fmt.Errorf("row %v", r)
					}
					return nil
				})(b); err != nil {
					return err
				}
				return watchRow("hosts", "host", BaseScriptHost, nil)(b)
			}},
	}
}

func expectSoft(_ *World, sc *Scenario) []Expectation {
	s := sc.Domain
	return []Expectation{
		{Says: "the watch notes the soft day", Endpoint: "watch", Params: params(s, "7d"),
			Check: func(b map[string]any) error {
				if b["soft_day"] == nil {
					return fmt.Errorf("no soft day: %v", b)
				}
				return nil
			}},
		{Says: "Noteworthy has no flipped path: a soft 404 answers every probe", Endpoint: "noteworthy",
			Params: findings(s, "7d"), Check: noFinding("flip:")},
		{Says: "Noteworthy has no scanner's hit", Endpoint: "noteworthy", Params: findings(s, "7d"),
			Check: noFinding("hit:")},
	}
}
