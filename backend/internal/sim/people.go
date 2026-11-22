package sim

import (
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// uaProfile is one browser on one device, with what its tracker context and request headers say.
type uaProfile struct {
	ua               string
	chromium         bool
	brand, platform  string
	mobile           bool
	screen, viewport [2]int
	dpr              float64
	touch, hc        int
	mem              float64
	weight           float64
}

var profiles = []uaProfile{
	{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36",
		true, "Google Chrome", "macOS", false, [2]int{1512, 982}, [2]int{1440, 820}, 2, 0, 10, 8, 18},
	{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36",
		true, "Google Chrome", "Windows", false, [2]int{1920, 1080}, [2]int{1903, 961}, 1, 0, 8, 8, 20},
	{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36 Edg/142.0.0.0",
		true, "Microsoft Edge", "Windows", false, [2]int{1536, 864}, [2]int{1519, 730}, 1.25, 0, 8, 8, 7},
	{"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:145.0) Gecko/20100101 Firefox/145.0",
		false, "", "", false, [2]int{1920, 1080}, [2]int{1903, 948}, 1, 0, 12, 0, 8},
	{"Mozilla/5.0 (X11; Linux x86_64; rv:145.0) Gecko/20100101 Firefox/145.0",
		false, "", "", false, [2]int{2560, 1440}, [2]int{2543, 1300}, 1, 0, 16, 0, 5},
	{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.1 Safari/605.1.15",
		false, "", "", false, [2]int{1728, 1117}, [2]int{1728, 980}, 2, 0, 10, 0, 10},
	{"Mozilla/5.0 (iPhone; CPU iPhone OS 26_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.1 Mobile/15E148 Safari/604.1",
		false, "", "", true, [2]int{393, 852}, [2]int{393, 659}, 3, 5, 6, 0, 16},
	{"Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Mobile Safari/537.36",
		true, "Google Chrome", "Android", true, [2]int{412, 915}, [2]int{412, 785}, 2.625, 5, 8, 4, 10},
	{"Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/28.0 Chrome/130.0.0.0 Mobile Safari/537.36",
		true, "Samsung Internet", "Android", true, [2]int{384, 832}, [2]int{384, 700}, 2.8125, 5, 8, 4, 3},
	{"Mozilla/5.0 (iPad; CPU OS 26_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.1 Mobile/15E148 Safari/604.1",
		false, "", "", false, [2]int{820, 1180}, [2]int{820, 1106}, 2, 5, 6, 0, 3},
}

// locale is how people of a country set up their browsers: languages, zone and its offset (getTimezoneOffset).
type locale struct {
	langs []string
	tz    string
	tzo   int
}

var locales = map[string][]locale{
	"CH": {{[]string{"de-CH", "de", "en"}, "Europe/Zurich", -60}, {[]string{"fr-CH", "fr", "en"}, "Europe/Zurich", -60},
		{[]string{"en-US", "en"}, "Europe/Zurich", -60}},
	"DE": {{[]string{"de-DE", "de", "en"}, "Europe/Berlin", -60}},
	"US": {{[]string{"en-US", "en"}, "America/New_York", 300}, {[]string{"en-US"}, "America/Los_Angeles", 480},
		{[]string{"es-US", "es", "en"}, "America/Chicago", 360}},
	"FR": {{[]string{"fr-FR", "fr", "en"}, "Europe/Paris", -60}},
	"GB": {{[]string{"en-GB", "en"}, "Europe/London", 0}},
	"NL": {{[]string{"nl-NL", "nl", "en"}, "Europe/Amsterdam", -60}},
	"JP": {{[]string{"ja-JP", "ja"}, "Asia/Tokyo", -540}},
}

// person is one visitor: a home address, a browser, a language, a visitor id that survives between visits.
type person struct {
	addr    netip.Addr
	prof    *uaProfile
	loc     locale
	vid     string
	blocked bool    // the tracker is blocked: only the feed sees the visits
	rate    float64 // visits a day, for one who comes back
}

// sharedPool is how many addresses a carrier's NAT or an office gives all its people: they share them.
const sharedPool = 6

// newPerson draws a person from r.
func (w *World) newPerson(r rng) *person {
	weights := make([]float64, len(networks))
	for i, n := range networks {
		weights[i] = n.people
	}
	ni := r.weighted(weights)
	n := networks[ni]
	pw := make([]float64, len(profiles))
	for i, p := range profiles {
		pw[i] = p.weight
	}
	p := &person{prof: &profiles[r.weighted(pw)], loc: pick(r, locales[n.Country]), vid: r.hex32(),
		blocked: r.chance(0.08), rate: 0.15 + r.Float64()*0.5}
	shared := n.Category == "business" || n.ASN == 64504
	switch {
	case shared:
		p.addr = hostIn(netip.MustParsePrefix(n.V4[0]), 1+r.IntN(sharedPool))
	case r.chance(0.3):
		b := v6Block(ni).Addr().As16()
		r.fill(b[5:]) // a /64 in the network's /40, and an interface id
		p.addr = netip.AddrFrom16(b)
	default:
		pf := netip.MustParsePrefix(n.V4[0])
		p.addr = hostIn(pf, 1+r.IntN(1<<(32-pf.Bits())-2))
	}
	return p
}

// hostIn is the n-th address of a prefix (n ≥ 0, counted in its last 64 bits).
func hostIn(p netip.Prefix, n int) netip.Addr {
	b := p.Masked().Addr().As16()
	carry := 0
	for i := 15; i >= 8 && (n > 0 || carry > 0); i-- {
		s := int(b[i]) + n&0xff + carry
		b[i] = byte(s & 0xff) //nolint:gosec // masked to a byte
		n, carry = n>>8, s>>8
	}
	a := netip.AddrFrom16(b)
	if p.Addr().Is4() {
		return a.Unmap()
	}
	return a
}

// population is a site's regulars: people who come back over the whole history.
func (w *World) population(s *Site) []*person {
	if s.Kind == "api" || (!s.Script && !s.Feed) {
		return nil
	}
	n := int(math.Round(30 * w.Scale))
	out := make([]*person, 0, n)
	for i := range n {
		out = append(out, w.newPerson(newRng(w.Seed, 1, int64(s.idx), int64(i))))
	}
	return out
}

// hourWeights is the people's day, in their own time: a working day with an evening.
var hourWeights = []float64{0.2, 0.1, 0.05, 0.05, 0.05, 0.1, 0.3, 0.6, 1, 1.3, 1.4, 1.3, 1.1, 1.2, 1.4, 1.4, 1.3, 1.1,
	1, 1.1, 1.2, 1, 0.7, 0.4}

// volume is the people's level on day d: weekends quieter, growth over the history.
func (w *World) volume(d int64) float64 {
	v := 1.0
	switch dayStart(d).Weekday() {
	case time.Saturday, time.Sunday:
		v = 0.6
	}
	if sc := w.scenario("growth"); sc != nil {
		// The newcomers double every quarter, steadily: a quarter back the site had half of today's. Noteworthy
		// compares 90 days with the 90 before, so the visitors rise by some 90 %, the regulars' fixed number and a
		// seed's luck far from pulling it under the 20 % a trend needs (2.5 times a year rose by 14 to 35 %).
		// The growth is the history's: past the anchor the level stays the anchor's. Live time runs on for as long
		// as the server serves (at the largest speed 150 days a wall hour), and a level doubling on with it would
		// make a day's plan outgrow memory whatever the scale.
		quarters := min(dayStart(d).Add(12*time.Hour).Sub(w.Anchor).Hours()/(growthQuarter*24), 0)
		v *= math.Pow(2, quarters)
	}
	return v
}

// growthQuarter is the days the growth scenario's visitors take to double.
const growthQuarter = 90

// startOn is a visit's start on day d for someone in loc, by the hour weights of their own day.
func startOn(r rng, d int64, loc locale) time.Time {
	h := r.weighted(hourWeights)
	off := time.Duration(h)*time.Hour + time.Duration(loc.tzo)*time.Minute + time.Duration(r.Int64N(int64(time.Hour)))
	off = ((off % day) + day) % day
	return dayStart(d).Add(off)
}

// Referrers people arrive from, and their weights; "" is direct.
var (
	referrers   = []string{"", "https://search.example.net/", "https://forum.example.org/t/ip-lookup-tools/112", "https://code.example.org/example/lens"}
	refWeights  = []float64{50, 35, 8, 7}
	spikeRef    = "https://news.example.com/item?id=4242"
	outbound    = []string{"https://code.example.org/example/lens", "https://docs.example.net/rdap", "https://rdap.example.net/ip/192.0.2.1"}
	searchTerms = []string{"alpine fibre", "nimbus cloud", "AS64496", "198.51.100.0/24", "kestrel hosting", "polder"}
)

// people plans site s's visits on day d: its regulars who come that day, and newcomers.
func (w *World) people(p *dayPlan, s *Site, d int64) {
	if s.Kind == "api" {
		return
	}
	v := w.volume(d)
	for i, per := range w.regulars[s.idx] {
		r := newRng(w.Seed, 2, int64(s.idx), int64(i), d)
		for range r.poisson(per.rate * v) {
			w.visit(p, s, per, startOn(r, d, per.loc), pick(r, referrers[:2]), "", r)
		}
	}
	r := newRng(w.Seed, 3, int64(s.idx), d)
	for i := range r.poisson(14 * w.Scale * v) {
		pr := newRng(w.Seed, 4, int64(s.idx), d, int64(i))
		per := w.newPerson(pr)
		w.visit(p, s, per, startOn(pr, d, per.loc), referrers[pr.weighted(refWeights)], "", pr)
	}
	// A post on a news site: a wave of first-time visitors arriving from it, landing on the page it links. Some
	// three times a week's visitors from search, so it leads the week's referrers however long the history.
	if sc := w.scenario("referrer-spike"); sc != nil && s.idx == 0 {
		from, to := sc.start(w.Anchor), sc.end(w.Anchor)
		for i := range newRng(w.Seed, 6).poisson(200 * w.Scale) {
			pr := newRng(w.Seed, 5, int64(i))
			at := from.Add(time.Duration(pr.Int64N(int64(to.Sub(from)))))
			if dayOf(at) != d {
				continue
			}
			w.visit(p, s, w.newPerson(pr), at, spikeRef, s.paths.spikePage(), pr)
		}
	}
}

// page is one page of a visit.
type page struct {
	path, url, title string
	at, leave        time.Time
}

// visit plans one visit of per to s from start: the page loads the backend reports, the tracker's batches.
func (w *World) visit(p *dayPlan, s *Site, per *person, start time.Time, ref, entry string, r rng) {
	n := 1
	for n < 8 && r.chance(0.5) {
		n++
	}
	pages := make([]page, n)
	t := start
	for i := range pages {
		path := entry
		if i > 0 || path == "" {
			path = s.paths.page(r)
		}
		dwell := time.Duration(math.Exp(2.5+r.NormFloat64()*0.9)*1000) * time.Millisecond
		dwell = min(max(dwell, minDwell), 20*time.Minute)
		pages[i] = page{path: path, url: "https://" + s.Domain + path, title: s.paths.title(path, s.Product), at: t,
			leave: t.Add(dwell)}
		t = pages[i].leave.Add(time.Duration(200+r.IntN(800)) * time.Millisecond)
	}
	pages = endBy(pages, start.Add(maxVisit))
	if s.Feed {
		w.loads(p, s, per, pages, ref, r)
	}
	if s.Script && !per.blocked {
		w.tracker(p, s, per, pages, ref, r, "person", false)
	}
}

// maxVisit bounds a visit, its first page to its last event, and so how far a day's plan reaches past midnight. A
// person's visit is eight pages at most, each left within twenty minutes (well inside the product's thirty-minute
// session timeout) and the next shown within a second: under 2 h 40 min 7 s however it is drawn. The automation's
// runs take minutes, the renderer's seconds. minDwell is the shortest a page is shown.
const (
	maxVisit = 3 * time.Hour
	minDwell = 3 * time.Second
)

// endBy cuts a visit's pages to end by end: the pages it would show too late are left out, the last is left at end.
func endBy(pages []page, end time.Time) []page {
	n := len(pages)
	for n > 1 && pages[n-1].at.Add(minDwell).After(end) {
		n--
	}
	pages = pages[:n]
	pages[n-1].leave = earlier(pages[n-1].leave, end)
	return pages
}

// browserHeaders are the request headers a browser sends with the tracker's POST and the product's requests alike.
func browserHeaders(prof *uaProfile, loc locale) map[string]string {
	h := map[string]string{"Accept-Language": acceptLanguage(loc.langs), "Sec-Fetch-Site": "cross-site",
		"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}
	if prof.chromium {
		h["Sec-Ch-Ua"] = chUA(prof)
		h["Sec-Ch-Ua-Mobile"] = map[bool]string{true: "?1", false: "?0"}[prof.mobile]
		h["Sec-Ch-Ua-Platform"] = `"` + prof.platform + `"`
	}
	return h
}

func chUA(prof *uaProfile) string {
	if !prof.chromium {
		return ""
	}
	return fmt.Sprintf(`"Chromium";v="142", "%s";v="142", "Not_A Brand";v="99"`, prof.brand)
}

func acceptLanguage(langs []string) string {
	parts := make([]string, len(langs))
	for i, l := range langs {
		parts[i] = l
		if i > 0 {
			parts[i] = fmt.Sprintf("%s;q=0.%d", l, 9-i)
		}
	}
	return strings.Join(parts, ",")
}

// loads are the requests the product's backend serves for a visit: the document, its assets and the page's API
// call on the first page; on later pages a single-page app only calls its API, a classic site loads the document.
func (w *World) loads(p *dayPlan, s *Site, per *person, pages []page, ref string, r rng) {
	lang, ch := acceptLanguage(per.loc.langs), chUA(per.prof)
	var from string // the Referer: the arrival's for the first document, the page itself after
	add := func(t time.Time, method, path, route, mode, dest, site string, status int, size int64) {
		// The broken release's API answers some lookups with a 502 until the fix.
		if strings.HasPrefix(path, "/api/") && w.in("deploy-error", t) && r.chance(0.3) {
			status = 502
		}
		p.records[s.idx] = append(p.records[s.idx], record{t: t, ip: per.addr, ua: per.prof.ua, method: method,
			path: path, route: route, status: status, dur: int64(4 + r.IntN(60)), bytes: size, lang: lang,
			sfSite: site, sfMode: mode, sfDest: dest, chUA: ch, ref: from})
	}
	prev := ""
	for i, pg := range pages {
		from = ref
		if i > 0 {
			from = prev
		}
		prev = pg.url
		if i == 0 || !s.SPA {
			site := "none"
			if ref != "" {
				site = "cross-site"
			}
			if i > 0 {
				site = "same-origin"
			}
			add(pg.at.Add(-150*time.Millisecond), "GET", pg.path, "", "navigate", "document", site, 200, 4200)
			from = pg.url
			if i == 0 || r.chance(0.2) {
				for j, a := range s.paths.assets {
					add(pg.at.Add(time.Duration(j*20-100)*time.Millisecond), "GET", a.path, "", "no-cors", a.dest,
						"same-origin", 200, a.size)
				}
			}
		}
		if api, route := s.paths.api(pg.path); api != "" {
			from = pg.url
			add(pg.at.Add(300*time.Millisecond), "GET", api, route, "cors", "empty", "same-origin", 200, 1800)
		}
	}
}

// batchWait is how long the script holds its first queued event before it sends the batch (nui.ts).
const batchWait = 5 * time.Second

// tracker plans the script's batches for a visit: events queued as they happen, a batch sent batchWait after the
// first queued event, at twenty events, or at once when the page is hidden (nui.ts).
func (w *World) tracker(p *dayPlan, s *Site, per *person, pages []page, ref string, r rng, actor string, webdriver bool) {
	type queued struct {
		e     event
		page  int
		final bool
	}
	var evs []queued
	add := func(i int, t time.Time, k, n string, props map[string]any) {
		evs = append(evs, queued{e: event{t: t, k: k, n: n, p: props}, page: i})
	}
	errorProb := 1.0 / 60
	for i, pg := range pages {
		if i == 0 {
			add(i, pg.at, "session_start", "", nil)
		} else {
			// The engagement of the page being left, then the new view (an in-page navigation).
			add(i-1, pg.at, "engagement", "", engagement(r, pages[i-1]))
		}
		add(i, pg.at, "page_view", "", nil)
		if i == 0 {
			add(i, pg.at.Add(300*time.Millisecond), "vital", "", map[string]any{"name": "TTFB", "value": float64(80 + r.IntN(700))})
			add(i, pg.at.Add(900*time.Millisecond), "vital", "", map[string]any{"name": "FCP", "value": float64(300 + r.IntN(2000))})
		}
		span := pg.leave.Sub(pg.at)
		at := func() time.Time { return pg.at.Add(time.Duration(r.Int64N(int64(span)))) }
		if r.chance(0.08) {
			u := pick(r, outbound)
			add(i, at(), "outbound_click", "", map[string]any{"url": u, "text": hostOfURL(u)})
		}
		if s.idx == 0 && r.chance(0.04) {
			add(i, at(), "custom", "copy_prefix", map[string]any{"from": "detail"})
		}
		if s.paths.lookup && r.chance(0.06) {
			add(i, at(), "search", "", map[string]any{"term": pick(r, searchTerms)})
		}
		if s.paths.lookup && r.chance(0.02) {
			add(i, at(), "download", "", map[string]any{"url": "https://" + s.Domain + "/downloads/prefixes.csv"})
		}
		if !s.paths.lookup && r.chance(0.03) {
			add(i, at(), "form", "", map[string]any{"phase": "start", "id": "subscribe", "name": "", "action": "/subscribe"})
		}
		if r.chance(errorProb) {
			add(i, at(), "js_error", "", map[string]any{"msg": "ResizeObserver loop completed with undelivered notifications",
				"src": "https://" + s.Domain + "/assets/index.js", "line": 1, "col": 18211})
		}
		if s.paths.lookup && pg.path != "/" && w.in("deploy-error", pg.at) && r.chance(0.5) {
			add(i, pg.at.Add(700*time.Millisecond), "js_error", "", map[string]any{"msg": DeployError,
				"src": "https://" + s.Domain + "/assets/index-C4v1rT2s.js", "line": 1, "col": 48113})
		}
	}
	last := pages[len(pages)-1]
	add(len(pages)-1, last.leave, "engagement", "", engagement(r, last))
	add(len(pages)-1, last.leave, "vital", "", map[string]any{"name": "LCP", "value": float64(500 + r.IntN(3500))})
	if r.chance(0.6) {
		add(len(pages)-1, last.leave, "vital", "", map[string]any{"name": "INP", "value": float64(40 + r.IntN(360))})
	}
	add(len(pages)-1, last.leave, "vital", "", map[string]any{"name": "CLS", "value": math.Round(r.Float64()*0.3*1000) / 1000})
	evs[len(evs)-1].final = true
	// Stable by time: events of one moment keep their order.
	for i := 1; i < len(evs); i++ {
		for j := i; j > 0 && evs[j].e.t.Before(evs[j-1].e.t); j-- {
			evs[j], evs[j-1] = evs[j-1], evs[j]
		}
	}
	cl := &client{addr: per.addr, ua: per.prof.ua, hdr: browserHeaders(per.prof, per.loc)}
	sid := r.hex32()
	inputs := map[string]int{"pointer": 0, "key": 0, "touch": 0, "scroll": 0}
	current := func(t time.Time) int {
		c := 0
		for i, pg := range pages {
			if !pg.at.After(t) {
				c = i
			}
		}
		return c
	}
	send := func(at time.Time, q []queued) {
		cur := pages[current(at)]
		for _, x := range q {
			if x.e.k == "engagement" {
				for k, v := range x.e.p["inputs"].(map[string]int) {
					inputs[k] += v
				}
			}
		}
		ctx := w.context(per, cur, ref, inputs, webdriver)
		b := &batch{at: at, site: s.idx, client: cl, vid: per.vid, sid: sid, ctx: ctx, actor: actor}
		for _, x := range q {
			e := x.e
			if pg := pages[x.page]; pg.url != cur.url {
				e.url, e.title = pg.url, pg.title
			}
			b.events = append(b.events, e)
		}
		p.batches = append(p.batches, b)
	}
	var q []queued
	var first time.Time
	for _, x := range evs {
		if len(q) > 0 && x.e.t.After(first.Add(batchWait)) {
			send(first.Add(batchWait), q)
			q = nil
		}
		if len(q) == 0 {
			first = x.e.t
		}
		q = append(q, x)
		if len(q) >= 20 || x.final {
			send(x.e.t.Add(20*time.Millisecond), q)
			q = nil
		}
	}
	if len(q) > 0 {
		send(first.Add(batchWait), q)
	}
}

// DeployError is the script error the deploy-error scenario's release throws on lookup pages.
const DeployError = "TypeError: Cannot read properties of undefined (reading 'asn')"

// engagement is the props of a page's engagement: time visible, scroll depth, input counts.
func engagement(r rng, pg page) map[string]any {
	ms := pg.leave.Sub(pg.at).Milliseconds() * int64(60+r.IntN(40)) / 100
	in := map[string]int{"pointer": r.IntN(12), "key": r.IntN(6), "touch": 0, "scroll": r.IntN(30)}
	return map[string]any{"ms": ms, "scroll": 20 + r.IntN(81), "inputs": in}
}

// context is the tracker's ctx for a batch sent from page cur.
func (w *World) context(per *person, cur page, ref string, inputs map[string]int, webdriver bool) map[string]any {
	pr := per.prof
	c := map[string]any{"url": cur.url, "title": cur.title, "lang": per.loc.langs[0], "langs": per.loc.langs,
		"screen": pr.screen[:], "viewport": pr.viewport[:], "dpr": pr.dpr, "tz": per.loc.tz, "tzo": per.loc.tzo,
		"touch": pr.touch, "hc": pr.hc, "dark": strHash(per.vid)%3 == 0, "reduced_motion": false,
		"webdriver": webdriver, "vis": "visible", "nav": "navigate", "cookies": true,
		"inputs": map[string]int{"pointer": inputs["pointer"], "key": inputs["key"], "touch": inputs["touch"],
			"scroll": inputs["scroll"]}}
	if ref != "" {
		c["ref"] = ref
	}
	if pr.chromium {
		c["uach"] = map[string]any{"mobile": pr.mobile, "platform": pr.platform,
			"brands": [][]string{{"Chromium", "142"}, {pr.brand, "142"}, {"Not_A Brand", "99"}}}
		c["conn"] = map[string]any{"type": "4g", "downlink": 10, "rtt": 50, "save_data": false}
		c["mem"] = pr.mem
	}
	return c
}

func hostOfURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
