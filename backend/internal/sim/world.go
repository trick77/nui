// Package sim is nui's traffic simulator: one seeded model of a world (sites, the people visiting them, known bots,
// uptime probes and unknown automation, and named scenarios on top) that delivers its traffic two ways. Backfill
// drives the real collector in-process on a virtual clock, so a history goes through the same validation,
// templating, enrichment, counters and classifier as real traffic; live sends the same requests to a running
// collector over HTTP, or into the server's own collector as the dashboard is asked (server -sim).
//
// The world is a function of its seed and anchor: every actor-day draws from its own random stream, so the same seed
// and anchor give the same requests whether the world is advanced a day or a second at a time.
package sim

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/trick77/nui/backend/internal/sites"
)

// Site is one simulated site: what delivers its events and the shape of its paths.
type Site struct {
	Domain  string
	Product string
	// Kind is the sites file's kind: site, or api for a server-only product.
	Kind string
	// Script: the browser script runs on its pages. Feed: the product's backend reports its requests.
	Script, Feed bool
	// FeedKey is the feed's bearer key; its hash goes into the sites file.
	FeedKey string
	// SPA: later page views are in-page navigations (no document request, an API call).
	SPA   bool
	Rules []sites.Rule
	// Silent: configured, never delivers anything (an empty site).
	Silent bool

	idx   int
	paths *pathGen
}

// LookupRules are the example lookup site's path rules (docs/sites.example.json).
var LookupRules = []sites.Rule{
	{Pattern: "/", Template: "/"},
	{Pattern: "/assets/{any}", Template: "/assets/*"},
	{Pattern: "/favicon.ico", Template: "/favicon.ico"},
	{Pattern: "/favicon.svg", Template: "/favicon.svg"},
	{Pattern: "/apple-touch-icon.png", Template: "/apple-touch-icon.png"},
	{Pattern: "/robots.txt", Template: "/robots.txt"},
	{Pattern: "/sitemap.xml", Template: "/sitemap.xml"},
	{Pattern: "/manifest.webmanifest", Template: "/manifest.webmanifest"},
	{Pattern: "/{ipv4}", Template: "/:ipv4"},
	{Pattern: "/{ipv6}", Template: "/:ipv6"},
	{Pattern: "/{cidr}", Template: "/:cidr"},
	{Pattern: "/AS{int}", Template: "/AS:n"},
	{Pattern: "/{text}", Template: "/:text"},
}

// DefaultSites are the world's sites when none are configured: one of each delivery shape, the lookup site first.
func DefaultSites() []*Site {
	return []*Site{
		{Domain: "lens.example.com", Product: "Example Lens", Kind: "site", Script: true, Feed: true, SPA: true,
			Rules: LookupRules},
		{Domain: "console.example.com", Product: "Example Lens", Kind: "site", Script: true, SPA: true,
			Rules: []sites.Rule{{Pattern: "/", Template: "/"}, {Pattern: "/projects/{int}", Template: "/projects/:n"},
				{Pattern: "/projects/{int}/{seg}", Template: "/projects/:n/:tab"}, {Pattern: "/settings", Template: "/settings"}}},
		{Domain: "api.example.com", Product: "Example Lens", Kind: "api", Feed: true},
		{Domain: "blog.example.org", Product: "Blog", Kind: "site", Script: true, Feed: true},
	}
}

// EmptySite is the configured site that never delivers (NUI_SIM_EMPTY_SITE).
func EmptySite() *Site {
	return &Site{Domain: "new.example.net", Product: "Blog", Kind: "site", Script: true, Silent: true}
}

// Options make a world.
type Options struct {
	Seed uint64
	// Anchor is where the history ends and live time begins; scenario windows are relative to it.
	Anchor time.Time
	// Scale multiplies every actor's volume: 1 is a small product (a few dozen visitors a day per site).
	Scale     float64
	Sites     []*Site
	Scenarios []*Scenario
	Ref       *Reference
}

// World is the seeded model: its sites, people and bots, and the day plans built from them.
type World struct {
	Seed      uint64
	Anchor    time.Time
	Scale     float64
	Sites     []*Site
	Scenarios []*Scenario
	Ref       *Reference

	regulars [][]*person // per site, the people who come back
	plans    map[int64]*dayPlan
	most     int    // the most day plans held at once (TestPlansHeld)
	seq      uint64 // numbers the requests handed out, their order at one instant
}

// NewWorld builds the world for o.
func NewWorld(o Options) (*World, error) {
	// Zero is allowed here (a world of its fixed actors alone); the knobs start at ScaleLimit's lower bound.
	if !(o.Scale >= 0 && o.Scale <= ScaleLimit.Max()) {
		return nil, fmt.Errorf("scale %v: not from 0 to %v", o.Scale, ScaleLimit.Max())
	}
	if o.Ref == nil {
		return nil, fmt.Errorf("no reference data")
	}
	if o.Sites == nil {
		o.Sites = DefaultSites()
	}
	w := &World{Seed: o.Seed, Anchor: o.Anchor.UTC().Truncate(time.Millisecond), Scale: o.Scale, Sites: o.Sites,
		Scenarios: o.Scenarios, Ref: o.Ref, plans: map[int64]*dayPlan{}}
	for i, s := range w.Sites {
		s.idx = i
		if s.Feed && s.FeedKey == "" {
			s.FeedKey = FeedKey(o.Seed, s.Domain)
		}
		s.paths = newPathGen(s)
		w.regulars = append(w.regulars, w.population(s))
	}
	return w, nil
}

// FeedKey is a simulated site's feed key: derived from seed and domain, so a backfill and a later live run agree.
func FeedKey(seed uint64, domain string) string {
	return fmt.Sprintf("sim-%016x%016x", mix(seed^strHash(domain)), mix(strHash(domain)))
}

// Site is the site with domain.
func (w *World) Site(domain string) (*Site, bool) {
	for _, s := range w.Sites {
		if s.Domain == domain {
			return s, true
		}
	}
	return nil, false
}

// scenario is the active scenario named name, nil when it is not.
func (w *World) scenario(name string) *Scenario {
	for _, s := range w.Scenarios {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// in reports whether t is inside the named scenario's window (false when the scenario is not active).
func (w *World) in(name string, t time.Time) bool {
	s := w.scenario(name)
	return s != nil && s.covers(w.Anchor, t)
}

const day = 24 * time.Hour

// dayOf is t's UTC day number.
func dayOf(t time.Time) int64 { return t.Unix() / 86400 }

func dayStart(d int64) time.Time { return time.Unix(d*86400, 0).UTC() }

// dayPlan is everything that starts on one UTC day: tracker batches and feed records, each sorted by time. A
// visit started late in the day spills past midnight, by planReach at most; a page's document is requested a moment
// before the page shows, so a record may come a moment before the day, its feed batch sent after midnight.
type dayPlan struct {
	batches []*batch
	records [][]record // per site
}

// planReach is how far past its day a plan reaches: a visit begun just before midnight, its last batch sent
// batchWait after its last event.
const planReach = maxVisit + batchWait

// plan builds (or returns the cached) plan of day d.
func (w *World) plan(d int64) *dayPlan {
	if p, ok := w.plans[d]; ok {
		return p
	}
	// Beside the day built, only the day before is kept (its visits reach past midnight), and dropped before the
	// build: a plan is a whole day's requests.
	w.keep(d-1, d-1)
	p := &dayPlan{records: make([][]record, len(w.Sites))}
	for _, s := range w.Sites {
		if s.Silent {
			continue
		}
		w.people(p, s, d)
		w.bots(p, s, d)
		w.automation(p, s, d)
	}
	for i, s := range w.Sites {
		recs := p.records[i][:0]
		for _, r := range p.records[i] {
			if w.feedDown(s, r.t) {
				continue
			}
			recs = append(recs, r)
		}
		sort.SliceStable(recs, func(a, b int) bool { return recs[a].t.Before(recs[b].t) })
		p.records[i] = recs
	}
	sort.SliceStable(p.batches, func(a, b int) bool { return p.batches[a].at.Before(p.batches[b].at) })
	w.plans[d] = p
	w.most = max(w.most, len(w.plans))
	return p
}

// keep drops the plans of the days outside [first, last].
func (w *World) keep(first, last int64) {
	for k := range w.plans {
		if k < first || k > last {
			delete(w.plans, k)
		}
	}
}

// The day plans a world holds, and what they may take at the largest scale: a plan is the whole day's requests, two
// are held only in the hours past midnight a visit begun the day before can last. At scale 50 the referrer wave's
// day and the one before it took 205 MB together, the most over a week on each side of the anchor (TestPlanMemory).
const (
	maxPlans   = 2
	planBudget = 256 << 20
)

// liveWeek is the span a live rate is averaged over: a week after the anchor, its weekend and every hour of its days.
const liveWeek = 7 * day

// LiveEvents counts the events the world's clients send in the week after its anchor: what a live run plays, the
// scenarios' windows in the history left out.
func (w *World) LiveEvents() int {
	n := 0
	for from := w.Anchor; from.Before(w.Anchor.Add(liveWeek)); from = from.Add(day) {
		for _, q := range w.Requests(from, from.Add(day)) {
			n += q.Events()
		}
	}
	return n
}

// FeedDelivers reports whether site's feed delivers at t.
func (w *World) FeedDelivers(s *Site, t time.Time) bool {
	return s.Feed && !s.Silent && !w.feedDown(s, t)
}

// feedDown reports whether site's feed loses records at t: an outage, or before a late feed's start.
func (w *World) feedDown(s *Site, t time.Time) bool {
	if sc := w.scenario("feed-outage"); sc != nil && sc.site(w, s) && sc.covers(w.Anchor, t) {
		return true
	}
	if sc := w.scenario("late-feed"); sc != nil && sc.site(w, s) && t.Before(sc.start(w.Anchor)) {
		return true
	}
	return false
}

// feedWindow is how long the product's feed emitter collects records before it sends them; feedDelay how long
// after a window closes the batch arrives.
const (
	feedWindow = 10 * time.Second
	feedDelay  = 2 * time.Second
)

// Requests are the requests the world's clients send in [from, to), in time order: tracker batches as they leave
// the browsers, and each site's feed batches as its backend sends them.
func (w *World) Requests(from, to time.Time) []*Request {
	from, to = from.UTC(), to.UTC()
	if !to.After(from) {
		return nil
	}
	var out []*Request
	// The feed's windows whose batches are sent in [from, to).
	fFrom := from.Add(-feedDelay).Truncate(feedWindow).Add(-feedWindow)
	// The days whose plans hold any of it: the day before only for the hours past midnight a visit begun on it can
	// reach into. A record before its day's start is sent after it, so a span ending at midnight does not need the
	// next day. Other days' plans are dropped, and each plan is read once: a span of several days holds two at a time.
	first, last := dayOf(fFrom.Add(-planReach)), dayOf(to.Add(-time.Nanosecond))
	w.keep(first, last)
	recs := make([][]record, len(w.Sites))
	for d := first; d <= last; d++ {
		p := w.plan(d)
		i := sort.Search(len(p.batches), func(i int) bool { return !p.batches[i].at.Before(from) })
		for ; i < len(p.batches) && p.batches[i].at.Before(to); i++ {
			b := p.batches[i]
			out = append(out, &Request{At: b.at, Site: w.Sites[b.site], batch: b, Actor: b.actor})
		}
		for _, s := range w.Sites {
			if !s.Feed {
				continue
			}
			rs := p.records[s.idx]
			i := sort.Search(len(rs), func(i int) bool { return !rs[i].t.Before(fFrom) })
			for ; i < len(rs) && rs[i].t.Before(to); i++ {
				recs[s.idx] = append(recs[s.idx], rs[i])
			}
		}
	}
	for _, s := range w.Sites {
		if !s.Feed {
			continue
		}
		rs := recs[s.idx]
		sort.SliceStable(rs, func(a, b int) bool { return rs[a].t.Before(rs[b].t) })
		out = append(out, feedBatches(s, rs, from, to)...)
	}
	slices.SortStableFunc(out, func(a, b *Request) int { return a.At.Compare(b.At) })
	for _, q := range out {
		w.seq++
		q.seq = w.seq
	}
	return out
}

// feedBatches groups a site's records into the emitter's windows and returns the batches sent in [from, to): at
// most MaxFeedEvents records each.
func feedBatches(s *Site, recs []record, from, to time.Time) []*Request {
	var out []*Request
	for i := 0; i < len(recs); {
		win := recs[i].t.Truncate(feedWindow)
		j := i
		for j < len(recs) && recs[j].t.Truncate(feedWindow).Equal(win) {
			j++
		}
		at := win.Add(feedWindow + feedDelay)
		if !at.Before(from) && at.Before(to) {
			for k := i; k < j; k += maxFeedEvents {
				out = append(out, &Request{At: at, Site: s, Feed: true, records: recs[k:min(j, k+maxFeedEvents)],
					Actor: "feed"})
			}
		}
		i = j
	}
	return out
}

// maxFeedEvents is what the emitter puts in one batch: under the collector's 500 and its 256 KiB.
const maxFeedEvents = 400
