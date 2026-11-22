// Package demo fills a database with synthetic, realistic traffic for one site: people with the script and the
// server feed (pages, assets, API calls), a search crawler, an uptime probe and scanners. The dashboard tests, the
// large-table test and local development (server -demo) use it. Deterministic for a seed.
package demo

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/trick77/ship/kit/ratelimit"

	"github.com/trick77/nui/backend/internal/store"
)

// Options shapes the traffic.
type Options struct {
	SiteID int64
	// Days of history before End, End's day included.
	Days int
	// People per day.
	People int
	// End is the newest event's time; zero is now.
	End  time.Time
	Seed uint64
	// Feed adds the server feed's requests.
	Feed bool
	// Classes also writes actor_class rows (slower: one short transaction per actor-day).
	Classes bool
}

var (
	pages     = []string{"/", "/8.8.8.8", "/AS15169", "/1.1.1.0%2F24", "/2001:db8::1", "/google", "/AS13335", "/cloudflare"}
	tmpls     = []string{"/", "/:ipv4", "/AS:n", "/:cidr", "/:ipv6", "/:text", "/AS:n", "/:text"}
	refs      = []string{"", "", "", "https://github.com/ipverse/as-ip-blocks", "https://www.google.com/", "https://news.ycombinator.com/"}
	uas       = []string{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0 Safari/537.36", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 Version/18.0 Mobile/15E148 Safari/604.1", "Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0"}
	countries = []string{"CH", "DE", "US", "US", "FR", "GB", "NL", "JP"}
	asns      = []int64{3303, 3320, 7922, 15169, 6830, 2856, 1136, 2516}
)

// Generate writes the traffic in batches and returns the events written.
func Generate(ctx context.Context, st *store.Store, o Options) (int, error) {
	if o.End.IsZero() {
		o.End = time.Now()
	}
	r := rand.New(rand.NewPCG(o.Seed, o.Seed^0x9e3779b97f4a7c15))
	g := &gen{st: st, o: o, r: r}
	start := o.End.Add(-time.Duration(o.Days) * 24 * time.Hour)
	for day := 0; day <= o.Days; day++ {
		d0 := start.Add(time.Duration(day) * 24 * time.Hour)
		at := func() int64 {
			t := d0.Add(time.Duration(r.Int64N(int64(24 * time.Hour))))
			if t.After(o.End) {
				t = o.End.Add(-time.Duration(r.Int64N(int64(time.Hour))))
			}
			return t.UnixMilli()
		}
		for p := range o.People {
			g.person(at(), day, p)
		}
		g.crawler(at())
		g.probe(d0, o.End)
		if o.Feed {
			g.scanner(at(), day)
		}
		if err := g.flush(ctx); err != nil {
			return g.n, err
		}
	}
	if o.Classes {
		for k, c := range g.classes {
			if err := st.SetActorClass(ctx, store.ActorClass{SiteID: o.SiteID, IPKey: k.key, Day: k.day,
				Class: c.class, Reasons: c.reasons}); err != nil {
				return g.n, err
			}
		}
	}
	return g.n, nil
}

type actorDay struct {
	key string
	day int64
}

type classed struct {
	class   string
	reasons []string
}

type gen struct {
	st      *store.Store
	o       Options
	r       *rand.Rand
	buf     []store.Event
	n       int
	classes map[actorDay]classed
}

func (g *gen) add(e store.Event, class string, reasons ...string) {
	e.SiteID = g.o.SiteID
	// A visit that started just before End does not run past it.
	e.TS = min(e.TS, g.o.End.UnixMilli())
	if e.IPKey == "" {
		e.IPKey = ratelimit.Key(e.IP)
	}
	e.Cls = store.ClassCode(class)
	if e.Props == "" {
		e.Props = "{}"
	}
	g.buf = append(g.buf, e)
	if g.o.Classes {
		if g.classes == nil {
			g.classes = map[actorDay]classed{}
		}
		g.classes[actorDay{e.IPKey, e.TS / (24 * 3600 * 1000)}] = classed{class, reasons}
	}
}

func (g *gen) flush(ctx context.Context) error {
	for len(g.buf) > 0 {
		n := min(len(g.buf), 2000)
		if err := g.st.InsertEvents(ctx, g.buf[:n]); err != nil {
			return err
		}
		g.n += n
		g.buf = g.buf[n:]
	}
	return nil
}

func (g *gen) person(ts int64, day, p int) {
	r := g.r
	ip := fmt.Sprintf("198.18.%d.%d", (day*7+p)%250, p%250+1)
	if p%5 == 0 {
		ip = fmt.Sprintf("2001:db8:%x:%x::%x", day%4000+1, p+1, r.IntN(0xffff)+1)
	}
	vid := fmt.Sprintf("v%d-%d", day%3, p) // some return across days
	sid := fmt.Sprintf("s%d-%d", day, p)
	ua := uas[p%len(uas)]
	i := r.IntN(len(countries))
	country, asn := countries[i], asns[i]
	ref := refs[r.IntN(len(refs))]
	refHost := ""
	if ref != "" {
		refHost = hostOf(ref)
	}
	views := 1 + r.IntN(4)
	for v := range views {
		t := ts + int64(v)*int64(20_000+r.IntN(60_000))
		pi := r.IntN(len(pages))
		path, tmpl := pages[pi], tmpls[pi]
		common := store.Event{TS: t, Visitor: vid, SID: sid, IP: ip, UA: ua, ASN: asn, Country: country, Path: path,
			Template: tmpl, URL: "https://lens.example.com" + path}
		if v == 0 {
			e := common
			e.Source, e.Kind, e.Ref, e.RefHost = "browser", "session_start", ref, refHost
			g.add(e, "human", "script beacon")
		}
		e := common
		e.Source, e.Kind = "browser", "page_view"
		if v == 0 {
			e.Ref, e.RefHost = ref, refHost
			e.Props = `{"ctx":{"webdriver":false,"lang":"de-CH"}}`
		}
		g.add(e, "human", "script beacon")
		eng := common
		eng.Source, eng.Kind, eng.TS = "browser", "engagement", t+15_000
		eng.Props = `{"p":{"ms":15000,"scroll":60}}`
		g.add(eng, "human")
		if r.IntN(10) == 0 {
			o := common
			o.Source, o.Kind, o.Name, o.TS = "browser", "outbound_click", "github.com", t+5_000
			g.add(o, "human")
		}
		if r.IntN(25) == 0 {
			o := common
			o.Source, o.Kind, o.Name, o.TS = "browser", "custom", "copy_prefix", t+6_000
			g.add(o, "human")
		}
		if r.IntN(60) == 0 {
			o := common
			o.Source, o.Kind, o.Name, o.TS = "browser", "js_error", "TypeError: cannot read properties of undefined", t+1_000
			g.add(o, "human")
		}
		if g.o.Feed {
			doc := common
			doc.Source, doc.Kind, doc.Name, doc.Method, doc.Status, doc.Template = "server", "request", "document", "GET", 200, "GET "+tmpl
			doc.Visitor, doc.SID, doc.TS = "", "", t-300
			doc.Props = `{"sf_mode":"navigate","lang":"de-CH"}`
			g.add(doc, "human", "assets loaded")
			for a, asset := range []string{"/assets/index.js", "/assets/index.css", "/favicon.svg"} {
				x := doc
				x.Name, x.Path, x.Template, x.TS = []string{"script", "style", "image"}[a], asset, "GET /assets/", t-200+int64(a)
				g.add(x, "human")
			}
			api := doc
			api.Name, api.Path, api.Template, api.TS, api.DurMS = "empty", "/api/v1/search", "GET /api/v1/search", t+100, 12
			g.add(api, "human")
		}
	}
}

func (g *gen) crawler(ts int64) {
	ip := fmt.Sprintf("66.249.66.%d", g.r.IntN(200)+1)
	for i := range 5 {
		pi := g.r.IntN(len(pages))
		e := store.Event{TS: ts + int64(i)*60_000, IP: ip, UA: "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
			ASN: 15169, Country: "US", Path: pages[pi], Source: "server", Kind: "request", Name: "document",
			Method: "GET", Status: 200, Template: "GET " + tmpls[pi], Visitor: "h:googlebot"}
		if g.o.Feed {
			g.add(e, "bot", "Googlebot address range")
		}
	}
}

// probe is an uptime check every 30 minutes, whole day.
func (g *gen) probe(d0, end time.Time) {
	if !g.o.Feed {
		return
	}
	for t := d0; t.Before(d0.Add(24*time.Hour)) && t.Before(end); t = t.Add(30 * time.Minute) {
		g.add(store.Event{TS: t.UnixMilli(), IP: "69.162.124.230", UA: "Mozilla/5.0+(compatible; UptimeRobot/2.0; http://www.uptimerobot.com/)",
			ASN: 46475, Country: "US", Path: "/", Source: "server", Kind: "request", Method: "HEAD", Status: 200,
			Template: "HEAD /", Visitor: "h:uptimerobot"}, "monitor", "UptimeRobot address range")
	}
}

// scanner is a client walking the lookup API with a bad key, and some days a path prober.
func (g *gen) scanner(ts int64, day int) {
	ip := fmt.Sprintf("203.0.113.%d", day%3+5)
	n := 50 + g.r.IntN(150)
	for i := range n {
		g.add(store.Event{TS: ts + int64(i)*800, IP: ip, UA: "python-requests/2.32.3", ASN: 14061, Country: "NL",
			Path: fmt.Sprintf("/api/v1/ip/10.0.%d.%d", i/250, i%250), Source: "server", Kind: "request", Method: "GET",
			Status: 401, Template: "GET /api/v1/ip/{addr}", Visitor: "h:scanner"}, "unknown", "client python-requests/2.32.3", "mostly refused")
	}
	if day%4 == 0 {
		for i, p := range []string{"/.env", "/wp-login.php", "/.git/config", "/admin", "/phpmyadmin/"} {
			g.add(store.Event{TS: ts + int64(i)*1500, IP: "192.0.2.66", UA: "Mozilla/5.0 zgrab/0.x", ASN: 63949,
				Country: "SG", Path: p, Source: "server", Kind: "request", Method: "GET", Status: 404,
				Template: "GET " + p, Visitor: "h:prober"}, "unknown", "client zgrab", "hosting network")
		}
	}
}

func hostOf(u string) string {
	const p = "https://"
	if len(u) <= len(p) {
		return ""
	}
	h := u[len(p):]
	for i := range len(h) {
		if h[i] == '/' {
			return h[:i]
		}
	}
	return h
}
