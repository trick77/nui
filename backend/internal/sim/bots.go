package sim

import (
	"fmt"
	"math"
	"net/netip"
	"time"
)

// Addresses of the scenario actors. All documentation addresses, in the world's hosting networks; the floods all in
// Kestrel's (64506), so the /48's network spike holds beside the others.
var (
	WalkerAddr      = netip.MustParseAddr("203.0.113.5")
	FloodAddr       = netip.MustParseAddr("192.0.2.77")
	ProberAddr      = netip.MustParseAddr("192.0.2.66")
	HammerAddr      = netip.MustParseAddr("192.0.2.99")
	HeadlessAddr    = netip.MustParseAddr("192.0.2.140")
	Flood64         = netip.MustParsePrefix("2001:db8:b07:7::/64")
	Flood48         = netip.MustParsePrefix("2001:db8:b03::/48")
	scannerPrefixes = []netip.Prefix{netip.MustParsePrefix("198.19.128.0/18"), netip.MustParsePrefix("198.19.192.0/19")}
)

// User agents of the unknown automation.
const (
	WalkerUA   = "python-requests/2.32.3"
	ProberUA   = "Go-http-client/1.1"
	HammerUA   = "python-httpx/0.28.1"
	HeadlessUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/142.0.0.0 Safari/537.36"
	FloodUA    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64)"
)

var scannerUAs = []string{"Mozilla/5.0 zgrab/0.x", "curl/8.7.1", "Go-http-client/1.1"}

// ProbedPaths are what the scan wave asks for: secrets, admin pages, backups.
var ProbedPaths = []string{"/.env", "/.git/config", "/wp-login.php", "/admin", "/phpmyadmin/", "/config.json",
	"/.aws/credentials", "/server-status", "/backup.zip", "/.DS_Store"}

var probedAccounts = []string{"admin", "root", "test", "info", "support", "admin@example.com", "demo", "guest"}

// BotAddr is the address a bot role sends from to site idx, ok false when the reference data has no such service.
func (w *World) BotAddr(role string, site int) (netip.Addr, bool) {
	b, ok := w.Ref.role(role)
	if !ok {
		return netip.Addr{}, false
	}
	return hostIn(b.V4[0], 10+site), true
}

// Bot is the address a bot role ("crawler", "renderer", "monitor") sends from to the site with domain.
func (w *World) Bot(role, domain string) (netip.Addr, bool) {
	s, ok := w.Site(domain)
	if !ok {
		return netip.Addr{}, false
	}
	return w.BotAddr(role, s.idx)
}

// bots plans the known bots' day on s: a crawler fetching pages on a steady beat, one that renders them (its
// browser runs the script), an uptime probe every few minutes.
func (w *World) bots(p *dayPlan, s *Site, d int64) {
	r := newRng(w.Seed, 10, int64(s.idx), d)
	d0 := dayStart(d)
	if b, ok := w.Ref.role("crawler"); ok && s.Feed && s.Kind != "api" {
		addr, _ := w.BotAddr("crawler", s.idx)
		n := r.poisson(48 * w.Scale)
		for k := range n {
			at := d0.Add(time.Duration((float64(k) + 0.5 + 0.3*(r.Float64()-0.5)) * float64(day) / float64(n)))
			path := s.paths.page(r)
			if k == 0 {
				path = "/robots.txt"
			}
			p.records[s.idx] = append(p.records[s.idx], record{t: at, ip: addr, ua: b.UA, method: "GET", path: path,
				status: 200, dur: int64(5 + r.IntN(30)), bytes: 4200})
		}
	}
	if b, ok := w.Ref.role("renderer"); ok && s.Kind != "api" {
		addr, _ := w.BotAddr("renderer", s.idx)
		bot := &person{addr: addr, prof: &uaProfile{ua: b.UA, chromium: true, brand: "Chromium", platform: "Linux",
			screen: [2]int{1024, 1024}, viewport: [2]int{1024, 1024}, dpr: 1, hc: 4, mem: 8},
			loc: locale{[]string{"en-US"}, "UTC", 0}, vid: r.hex32(), scripted: true}
		for range r.poisson(4 * w.Scale) {
			start := d0.Add(time.Duration(r.Int64N(int64(day))))
			pg := s.paths.page(r)
			pages := []page{{path: pg, url: "https://" + s.Domain + pg, title: s.paths.title(pg, s.Product), at: start,
				leave: start.Add(5 * time.Second)}}
			if s.Feed {
				w.loads(p, s, bot, pages, "", r)
			}
			if s.Script {
				w.tracker(p, s, bot, pages, "", r, "renderer", false)
			}
		}
	}
	if b, ok := w.Ref.role("monitor"); ok && s.Feed && w.Scale > 0 {
		addr, _ := w.BotAddr("monitor", s.idx)
		every := min(max(time.Duration(float64(5*time.Minute)/w.Scale), 5*time.Minute), 2*day)
		method, path := "HEAD", "/"
		if s.Kind == "api" {
			method, path = "GET", "/health"
		}
		off := time.Duration(newRng(w.Seed, 11, int64(s.idx)).Int64N(int64(every)))
		for t := d0.Add(off); t.Before(d0.Add(day)); t = t.Add(every) {
			p.records[s.idx] = append(p.records[s.idx], record{t: t, ip: addr, ua: b.UA, method: method, path: path,
				status: 200, dur: 3, bytes: 0})
		}
	}
}

// automation plans the day's scenario actors on s.
func (w *World) automation(p *dayPlan, s *Site, d int64) {
	d0 := dayStart(d)
	add := func(rec record) { p.records[s.idx] = append(p.records[s.idx], rec) }
	if s.Kind == "api" {
		w.integrations(p, s, d)
	}
	if sc := w.scenario("api-walker"); sc != nil && s.Feed && s.idx == w.firstFeed() {
		r := newRng(w.Seed, 20, d)
		n := 50 + r.IntN(150)
		run := time.Duration(n) * 800 * time.Millisecond
		start := d0.Add(time.Duration(r.Int64N(int64(day - run))))
		// On the anchor's day the run happens before it: the history ends with it.
		if d == dayOf(w.Anchor) && start.Add(run).After(w.Anchor.Add(-time.Hour)) && w.Anchor.Sub(d0) > run+2*time.Hour {
			start = d0.Add(time.Duration(r.Int64N(int64(w.Anchor.Sub(d0) - run - time.Hour))))
		}
		for i := range n {
			add(record{t: start.Add(time.Duration(i) * 800 * time.Millisecond), ip: WalkerAddr, ua: WalkerUA,
				method: "GET", path: fmt.Sprintf("/api/v1/ip/192.0.2.%d", i%256), route: "GET /api/v1/ip/{addr}",
				status: 401, dur: 2, bytes: 31})
		}
	}
	if sc := w.scenario("scan-wave"); sc != nil && s.Feed {
		from, to := sc.start(w.Anchor), sc.end(w.Anchor)
		for i := range 12 {
			r := newRng(w.Seed, 21, int64(s.idx), int64(i))
			at := from.Add(time.Duration(r.Int64N(int64(to.Sub(from)))))
			if dayOf(at) != d {
				continue
			}
			addr := hostIn(scannerPrefixes[i%2], 1+r.IntN(4000))
			ua := pick(r, scannerUAs)
			n := 6 + r.IntN(5)
			for j, path := range ProbedPaths[:n] {
				add(record{t: at.Add(time.Duration(j*1500) * time.Millisecond), ip: addr, ua: ua, method: "GET",
					path: path, status: 404, dur: 1, bytes: 153})
			}
		}
	}
	if sc := w.scenario("cred-prober"); sc != nil && s.Feed && s.idx == w.firstFeed() {
		from, to := sc.start(w.Anchor), sc.end(w.Anchor)
		for i := 0; ; i++ {
			at := from.Add(time.Duration(i) * 2 * time.Second)
			if !at.Before(to) {
				break
			}
			if dayOf(at) == d {
				// The names tried as account, against INTEGRATION.md: a careless emitter's 401 must sign nobody in.
				add(record{t: at, ip: ProberAddr, ua: ProberUA, method: "POST", path: "/api/v1/login",
					route: "POST /api/v1/login", status: 401, dur: 40, bytes: 44,
					account: probedAccounts[i%len(probedAccounts)]})
			}
		}
	}
	// A flood outweighs the rest of the site's day: twice the prober's hour and the people's and bots' requests
	// together (some 450 fixed and 650 per unit of scale with the referrer wave, 800 and 700 allowed), so each
	// address flood keeps over a quarter of the day's requests, a spike, with the other two floods and the prober
	// beside it.
	floodSize := int(math.Round(2 * (1800 + 800 + 700*w.Scale)))
	for k, f := range []struct {
		name string
		addr func(r rng, i int) netip.Addr
	}{
		{"flood-addr", func(rng, int) netip.Addr { return FloodAddr }},
		{"flood-64", func(r rng, _ int) netip.Addr {
			b := Flood64.Addr().As16()
			r.fill(b[8:])
			return netip.AddrFrom16(b)
		}},
		{"flood-48", func(r rng, i int) netip.Addr {
			// 200 /64s, the 7th and 8th byte numbering them.
			b := hostIn(netip.PrefixFrom(Flood48.Addr(), 64), 0).As16()
			b[7] = byte(i % 200) //nolint:gosec // below 200
			r.fill(b[8:])
			return netip.AddrFrom16(b)
		}},
	} {
		sc := w.scenario(f.name)
		if sc == nil || !s.Feed || s.idx != w.firstFeed() {
			continue
		}
		from, to := sc.start(w.Anchor), sc.end(w.Anchor)
		n := floodSize
		r := newRng(w.Seed, 22, int64(k))
		for i := range n {
			at := from.Add(to.Sub(from) * time.Duration(i) / time.Duration(n))
			addr := f.addr(r, i)
			if dayOf(at) == d {
				add(record{t: at, ip: addr, ua: FloodUA, method: "GET", path: "/", status: 200, dur: 12, bytes: 4200})
			}
		}
	}
	if sc := w.scenario("headless"); sc != nil && s.Script && s.Kind != "api" {
		r := newRng(w.Seed, 23, int64(s.idx), d)
		bot := &person{addr: HeadlessAddr, prof: &uaProfile{ua: HeadlessUA, chromium: true, brand: "HeadlessChrome",
			platform: "Linux", screen: [2]int{800, 600}, viewport: [2]int{800, 600}, dpr: 1, hc: 2, mem: 2},
			loc: locale{[]string{"en-US"}, "UTC", 0}, vid: r.hex32(), scripted: true}
		for range r.poisson(2) {
			start := d0.Add(time.Duration(r.Int64N(int64(day - time.Hour))))
			var pages []page
			for k := range 5 + r.IntN(20) {
				pg := s.paths.page(r)
				at := start.Add(time.Duration(k) * 4 * time.Second)
				pages = append(pages, page{path: pg, url: "https://" + s.Domain + pg, title: s.paths.title(pg, s.Product),
					at: at, leave: at.Add(3 * time.Second)})
			}
			if s.Feed {
				w.loads(p, s, bot, pages, "", r)
			}
			w.tracker(p, s, bot, pages, "", r, "headless", true)
		}
	}
	if sc := w.scenario("hammer"); sc != nil && s.Script && s.idx == w.firstScript() {
		from, to := sc.start(w.Anchor), sc.end(w.Anchor)
		cl := &client{addr: HammerAddr, ua: HammerUA, hdr: map[string]string{}}
		r := newRng(w.Seed, 24)
		vid, sid := r.hex32(), r.hex32()
		for at := from; at.Before(to); at = at.Add(250 * time.Millisecond) {
			if dayOf(at) != d {
				continue
			}
			b := &batch{at: at, site: s.idx, client: cl, vid: vid, sid: sid, actor: "hammer", hammer: true,
				ctx: map[string]any{"url": "https://" + s.Domain + "/"}}
			for range 20 {
				b.events = append(b.events, event{t: at, k: "custom", n: "ping"})
			}
			p.batches = append(p.batches, b)
		}
	}
}

// integrations are the programs calling a server-only API with their accounts: a few runs a day each.
func (w *World) integrations(p *dayPlan, s *Site, d int64) {
	r := newRng(w.Seed, 25, int64(s.idx), d)
	for i := range r.poisson(20 * w.Scale) {
		// The same integrations every day: as many as half the day's runs.
		pr := newRng(w.Seed, 26, int64(s.idx), int64(i%int(max(10*w.Scale, 1))))
		per := w.newPerson(pr)
		account := fmt.Sprintf("team-%04d", 1+pr.IntN(400))
		ua := pick(pr, []string{"example-lens-cli/2.3.1", "example-lens-sdk-js/1.8.0", "example-lens-sdk-py/0.9.2"})
		start := dayStart(d).Add(time.Duration(r.Int64N(int64(day))))
		for k := range 3 + r.IntN(30) {
			v := pick(r, fills["ipv4"])
			p.records[s.idx] = append(p.records[s.idx], record{t: start.Add(time.Duration(k*(400+r.IntN(4000))) * time.Millisecond),
				ip: per.addr, ua: ua, method: "GET", path: "/v1/prefix/" + v, route: "GET /v1/prefix/{addr}",
				status: 200, dur: int64(8 + r.IntN(80)), bytes: 900, account: account})
		}
		// Its tool calls over one route: the operation is the product's name for them, and a failed call is answered
		// 200 with the reason beside it. A generator of its own: the draws above stay what they were.
		tr := newRng(w.Seed, 27, int64(s.idx), d, int64(i))
		for k := range 2 + tr.IntN(12) {
			op := pick(tr, []string{"whois", "whois", "whois", "search", "search", "tools/list"})
			rec := record{t: start.Add(time.Duration(k*(900+tr.IntN(5000))) * time.Millisecond), ip: per.addr, ua: ua,
				method: "POST", path: "/mcp", route: "POST /mcp", op: op, status: 200, bytes: 1800, account: account,
				dur: int64(20 + tr.IntN(60) + tr.IntN(8)/7*400)}
			switch n := tr.IntN(40); {
			case n < 2:
				rec.err = "invalid_params"
			case n == 2:
				rec.err = "upstream_error"
			case n == 3:
				rec.status = 429
			}
			p.records[s.idx] = append(p.records[s.idx], rec)
		}
	}
}

// firstFeed and firstScript are the sites the single-target scenarios act on: the first with a feed, the first with
// the script.
func (w *World) firstFeed() int {
	for _, s := range w.Sites {
		if s.Feed && s.Kind != "api" && !s.Silent {
			return s.idx
		}
	}
	return -1
}

func (w *World) firstScript() int {
	for _, s := range w.Sites {
		if s.Script && !s.Silent {
			return s.idx
		}
	}
	return -1
}
