// Package classify decides what kind of actor an address key is on a day: human, known bot, uptime probe or unknown
// automated, with the reasons the dashboard shows. Two steps: an instant guess when an event is written (the bot
// list and the client string, nothing that needs history), and a periodic pass over the recent window that weighs
// the actor's whole day (script beacon, assets, headers, network) and stamps the result on its events.
//
// An address is often shared (an office, a carrier's NAT): the pass weighs each of its user agents on its own. The
// actor's class follows the majority of its events; the clients that differ keep their own class on their own events,
// so one curl request behind a NAT of people is counted as automated without making the people automated, and a
// script cannot hide behind one browser request.
package classify

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"

	"github.com/trick77/nui/backend/internal/refdata"
	"github.com/trick77/nui/backend/internal/sites"
	"github.com/trick77/nui/backend/internal/store"
)

// Every is how often the pass runs.
const Every = 5 * time.Minute

const dayMS = int64(24 * time.Hour / time.Millisecond)

// maxUAs is how many of an actor-day's user agents the pass weighs one by one, the most frequent; the rest (a
// client rotating its user agent) is stamped with the actor's class.
const maxUAs = 64

// Thresholds of the pace signals: an actor with at least apiOnlyRequests requests and no page, asset or beacon only
// calls the API; one whose requests average under rapidGap apart is no person reading.
const (
	apiOnlyRequests = 20
	rapidGap        = 500 * time.Millisecond
)

// maxRecheck bounds the actor-days waiting for an rDNS answer.
const maxRecheck = 10_000

// automationUA matches clients that are no browser: HTTP libraries, command-line tools, headless and scripted
// browsers, scanners, and the generic bot words a crawler not on the list still uses.
var automationUA = regexp.MustCompile(`(?i)(curl|wget|python|go-http-client|okhttp|java/|apache-httpclient|libwww|` +
	`scrapy|httpx|aiohttp|axios|node-fetch|undici|headlesschrome|phantomjs|puppeteer|playwright|selenium|` +
	`\bbot\b|bot/|crawler|spider|scanner|nikto|nmap|masscan|zgrab|nuclei|sqlmap|postman|insomnia|guzzle|` +
	`ruby|perl|php/|powershell|winhttp|dataprovider|censys|shodan)`)

// Classifier holds the reference data and the classes of the current day's actors.
type Classifier struct {
	DB       *store.Store
	Sites    *sites.Registry
	Ref      *refdata.Data
	Verifier *refdata.Verifier
	Log      *slog.Logger
	Now      func() time.Time

	mu    sync.Mutex
	today int64
	known map[int64]map[string]knownActor // site → ip_key → today's class
	// lastID is the newest event id the last pass covered. By id, not time: the feed may date a record up to an hour
	// back and the browser's events are back-dated by their client offset, and a time window would miss them.
	lastID int64
	// recheck are the actor-days decided while an rDNS answer was pending: the next pass decides them again even
	// without new events.
	recheck map[actorKey]bool
}

// knownActor is an actor's class today and the classes of its clients that differ from it.
type knownActor struct {
	cls int
	ua  map[string]int
}

func (c *Classifier) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Classifier) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// Enrich fills an event's network, country and first class, in the batch writer before the insert.
func (c *Classifier) Enrich(e *store.Event) {
	a, err := netip.ParseAddr(e.IP)
	if err != nil {
		return
	}
	if c.Ref != nil {
		if as, ok := c.Ref.Networks().Lookup(a); ok {
			e.ASN = as.ASN
		}
		e.Country = c.Ref.Countries().Lookup(a)
	}
	inst := c.instant(a, e.UA)
	c.mu.Lock()
	k, ok := c.known[e.SiteID][e.IPKey]
	sameDay := e.TS/dayMS == c.today
	c.mu.Unlock()
	switch {
	case !ok || !sameDay:
		e.Cls = inst
	case k.ua[e.UA] != 0:
		e.Cls = k.ua[e.UA]
	case k.cls == store.ClsHuman && inst != store.ClsPending:
		// A new automated client behind an address of people: its own class, not theirs.
		e.Cls = inst
	default:
		e.Cls = k.cls
	}
}

// instant is the class an event gets before the pass sees its actor: the bot list by address (authoritative lists
// only, rDNS waits for the pass), a bot list's client string for a service without ranges, or an automation client.
func (c *Classifier) instant(a netip.Addr, ua string) int {
	var bots *refdata.Bots
	if c.Ref != nil {
		bots = c.Ref.Bots()
	}
	if p, ok := bots.ByIP(a); ok && p.Authoritative {
		return store.ClassCode(p.Class)
	}
	if p, ok := bots.ByUA(ua); ok && !p.HasRanges {
		return store.ClassCode(p.Class)
	}
	if ua == "" || automationUA.MatchString(ua) {
		return store.ClsUnknown
	}
	return store.ClsPending
}

// Run passes every Every until ctx ends.
func (c *Classifier) Run(ctx context.Context) {
	for {
		if err := c.Pass(ctx); err != nil && ctx.Err() == nil {
			c.log().Warn("Classification pass failed", logging.Err(err))
		}
		if !run.Sleep(ctx, Every) {
			return
		}
	}
}

// Pass classifies every actor with events written since the last pass (on the first, every actor of yesterday and
// today), for each day it was active, and stamps the class on that day's events.
func (c *Classifier) Pass(ctx context.Context) error {
	now := c.now()
	if c.Verifier != nil {
		c.Verifier.StartPass()
	}
	c.mu.Lock()
	after := c.lastID
	c.mu.Unlock()
	var upto int64
	if err := c.DB.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM event`).Scan(&upto); err != nil {
		return err
	}
	since := (now.UnixMilli()/dayMS - 1) * dayMS
	today := now.UnixMilli() / dayMS
	known := map[int64]map[string]knownActor{}
	for _, s := range c.Sites.All() {
		got, err := c.passSite(ctx, s, after, upto, since, today)
		if err != nil {
			return fmt.Errorf("site %s: %w", s.Domain, err)
		}
		known[s.ID] = got
	}
	c.mu.Lock()
	if c.today != today {
		c.known = nil
	}
	c.today = today
	if c.known == nil {
		c.known = map[int64]map[string]knownActor{}
	}
	for site, m := range known {
		if c.known[site] == nil {
			c.known[site] = map[string]knownActor{}
		}
		for k, v := range m {
			c.known[site][k] = v
		}
	}
	c.lastID = upto
	c.mu.Unlock()
	return nil
}

// siteFacts is what the pass knows about a site beyond one actor.
type siteFacts struct {
	// feed: the site has a server feed. script: its script delivered in the last week, so a missing beacon is a
	// signal. fetchMeta: its feed carries the fetch metadata a browser always sends, so a request without it is
	// none of a browser's.
	feed, script, fetchMeta bool
}

func (c *Classifier) passSite(ctx context.Context, s *sites.Site, after, upto, since, today int64) (map[string]knownActor, error) {
	// Which actors moved since the last pass, on which days: the ids written since (a rowid range), or on the
	// first pass yesterday and today; and the ones waiting for an rDNS answer.
	q, args := `SELECT DISTINCT ip_key, ts / ? FROM event WHERE id > ? AND id <= ? AND site_id = ?`,
		[]any{dayMS, after, upto, s.ID}
	if after == 0 {
		q, args = `SELECT DISTINCT ip_key, ts / ? FROM event WHERE site_id = ? AND ts >= ? AND id <= ?`,
			[]any{dayMS, s.ID, since, upto}
	}
	rows, err := c.DB.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	seen := map[actorKey]bool{}
	var moved []actorKey
	for rows.Next() {
		x := actorKey{site: s.ID}
		if err := rows.Scan(&x.key, &x.day); err != nil {
			_ = rows.Close()
			return nil, err
		}
		seen[x] = true
		moved = append(moved, x)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	for k := range c.recheck {
		if k.site == s.ID && !seen[k] {
			moved = append(moved, k)
		}
	}
	c.mu.Unlock()
	f := siteFacts{feed: s.HasFeed}
	if f.script, err = c.hasBrowser(ctx, s.ID); err != nil {
		return nil, err
	}
	if s.HasFeed {
		if f.fetchMeta, err = c.hasFetchMeta(ctx, s.ID); err != nil {
			return nil, err
		}
	}
	out := map[string]knownActor{}
	for _, m := range moved {
		a, err := c.actorDay(ctx, s.ID, m.key, m.day)
		if err != nil {
			return nil, err
		}
		if len(a.uas) == 0 {
			c.setRecheck(m, false)
			continue
		}
		d := c.Decide(a, f)
		c.setRecheck(m, d.pending)
		if err := c.DB.SetActorClass(ctx, store.ActorClass{SiteID: s.ID, IPKey: m.key, Day: m.day, Class: d.class,
			Reasons: d.reasons}, d.byUA); err != nil {
			return nil, err
		}
		if m.day == today {
			k := knownActor{cls: store.ClassCode(d.class), ua: map[string]int{}}
			for ua, cl := range d.byUA {
				k.ua[ua] = store.ClassCode(cl)
			}
			out[m.key] = k
		}
	}
	return out, nil
}

func (c *Classifier) setRecheck(k actorKey, pending bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case !pending:
		delete(c.recheck, k)
	case c.recheck == nil:
		c.recheck = map[actorKey]bool{k: true}
	case len(c.recheck) < maxRecheck:
		c.recheck[k] = true
	}
}

// hasBrowser reports whether the site's script delivered in the last week: only then is a missing beacon a signal.
func (c *Classifier) hasBrowser(ctx context.Context, site int64) (bool, error) {
	var ok bool
	err := c.DB.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM ingest_stat WHERE site_id = ? AND hour >= ?
		AND accepted > accepted_server)`, site, c.now().Add(-7*24*time.Hour).Unix()/3600).Scan(&ok)
	return ok, err
}

// hasFetchMeta reports whether the site's feed carried Sec-Fetch-Mode in the last day: only then is a request
// without it and without Accept-Language evidence (a product's emitter may not send these fields at all).
func (c *Classifier) hasFetchMeta(ctx context.Context, site int64) (bool, error) {
	var ok bool
	err := c.DB.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM event WHERE site_id = ? AND ts >= ?
		AND source = 'server' AND props LIKE '%"sf_mode"%')`, site, c.now().Add(-24*time.Hour).UnixMilli()).Scan(&ok)
	return ok, err
}

// actorDay reads an actor's day per user agent, the most frequent first.
func (c *Classifier) actorDay(ctx context.Context, site int64, key string, day int64) (actorDay, error) {
	a := actorDay{key: key}
	rows, err := c.DB.DB.QueryContext(ctx, `SELECT ua, COUNT(*),
		SUM(source = 'browser'), SUM(source = 'server'),
		SUM(source = 'server' AND name = 'document'),
		SUM(source = 'server' AND (name IN ('script', 'style', 'image', 'font') OR path LIKE '/assets/%')),
		SUM(source = 'server' AND status >= 400 AND status < 500),
		MAX(source = 'browser' AND props LIKE '%"webdriver":true%'),
		SUM(source = 'server' AND props NOT LIKE '%"sf_mode"%'),
		SUM(source = 'server' AND props NOT LIKE '%"lang"%'),
		SUM(kind = 'engagement'), SUM(kind = 'page_view'),
		COALESCE(MIN(CASE WHEN source = 'server' THEN ts END), 0),
		COALESCE(MAX(CASE WHEN source = 'server' THEN ts END), 0),
		MIN(ip), MAX(asn)
		FROM event WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ?
		GROUP BY ua ORDER BY COUNT(*) DESC, ua LIMIT ?`,
		site, key, day*dayMS, (day+1)*dayMS, maxUAs)
	if err != nil {
		return a, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var g uaDay
		var ip string
		var asn int64
		if err := rows.Scan(&g.ua, &g.n, &g.browser, &g.server, &g.docs, &g.assets, &g.refused, &g.webdriver,
			&g.noFetch, &g.noLang, &g.engaged, &g.pageView, &g.first, &g.last, &ip, &asn); err != nil {
			return a, err
		}
		if a.ip == "" {
			a.ip = ip
		}
		a.asn = max(a.asn, asn)
		a.uas = append(a.uas, g)
	}
	return a, rows.Err()
}

// decision is the pass's verdict on an actor-day.
type decision struct {
	class   string
	reasons []string
	// byUA are the clients whose class differs from the actor's, stamped on their own events.
	byUA map[string]string
	// pending: an rDNS answer was not known yet; the next pass decides again.
	pending bool
}

// clientVerdict is one user agent's own evidence: a class when it alone is conclusive ("" when it looks like a
// person's browser), and the reason.
type clientVerdict struct {
	class, reason string
	pending       bool
}

// Decide weighs an actor-day: the bot list by address first (authoritative, or confirmed by rDNS), then every
// client on its own (a bot list's client string, an automation client, a scripted browser, a browser without the
// headers every browser sends), then the majority. People in the majority are weighed further by weak signals
// (two make them automated too); clients that differ from the majority keep their own class.
func (c *Classifier) Decide(a actorDay, f siteFacts) decision {
	addr, _ := netip.ParseAddr(a.ip)
	var bots *refdata.Bots
	var nets *refdata.Networks
	if c.Ref != nil {
		bots, nets = c.Ref.Bots(), c.Ref.Networks()
	}
	pending := false
	if p, ok := bots.ByIP(addr); ok {
		if p.Authoritative {
			return decision{class: p.Class, reasons: []string{p.Name + " address range"}}
		}
		confirmed, wait := c.check(a.ip, p)
		if confirmed {
			return decision{class: p.Class, reasons: []string{p.Name + ", rDNS confirmed"}}
		}
		pending = wait
	}
	var people uaDay // the people's clients summed
	verdicts := make([]clientVerdict, len(a.uas))
	var total, automated int64
	for i, g := range a.uas {
		v := c.clientVerdict(a.ip, g, f, bots)
		verdicts[i] = v
		pending = pending || v.pending
		total += g.n
		if v.class != "" {
			automated += g.n
			continue
		}
		people = addDay(people, g)
	}
	weak := c.weakSignals(people, f, nets, addr)
	d := decision{byUA: map[string]string{}, pending: pending}
	if automated*2 < total || (automated*2 == total && people.engaged > 0) {
		// Mostly people: human unless their own traffic looks automated too.
		d.class = "human"
		if len(weak) >= 2 && people.engaged == 0 {
			d.class = "unknown"
		}
		if d.class == "human" {
			d.reasons = append(goodSignals(people), weak...)
		} else {
			d.reasons = weak
		}
		for i, v := range verdicts {
			if v.class != "" && v.class != d.class {
				d.byUA[a.uas[i].ua] = v.class
			}
			if v.class != "" {
				// The automated part stays visible on the people's row: what, which client, how much.
				d.reasons = append(d.reasons, fmt.Sprintf("also %s: %s, %d of %d events (%s)", classPhrase[v.class],
					Client(a.uas[i].ua), a.uas[i].n, total, v.reason))
			}
		}
		return d
	}
	// Mostly automated: the class of the largest automated part, its reasons first.
	byClass := map[string]int64{}
	for i, v := range verdicts {
		if v.class != "" {
			byClass[v.class] += a.uas[i].n
		}
	}
	for cl, n := range byClass {
		if d.class == "" || n > byClass[d.class] || (n == byClass[d.class] && cl > d.class) {
			d.class = cl
		}
	}
	for i, v := range verdicts {
		switch {
		case v.class != "" && !slices.Contains(d.reasons, v.reason):
			d.reasons = append(d.reasons, v.reason)
		case v.class == "" && (a.uas[i].browser > 0 || a.uas[i].assets > 0 || a.uas[i].engaged > 0):
			// A person's browser behind an automated address keeps its own class.
			d.byUA[a.uas[i].ua] = "human"
		}
		if v.class != "" && v.class != d.class {
			d.byUA[a.uas[i].ua] = v.class
		}
	}
	if d.class == "unknown" {
		d.reasons = append(d.reasons, c.weakSignals(sumDays(a.uas), f, nets, addr)...)
	}
	return d
}

// check asks the verifier for a provider's rDNS, from its cache: confirmed, or pending an answer.
func (c *Classifier) check(ip string, p *refdata.Provider) (confirmed, pending bool) {
	if c.Verifier == nil {
		return false, false
	}
	return c.Verifier.Check(ip, p.RdnsPatterns)
}

// clientVerdict is one user agent's own, conclusive evidence.
func (c *Classifier) clientVerdict(ip string, g uaDay, f siteFacts, bots *refdata.Bots) clientVerdict {
	if p, ok := bots.ByUA(g.ua); ok {
		switch {
		case !p.HasRanges:
			return clientVerdict{class: p.Class, reason: "client is " + p.Name}
		case !p.Authoritative:
			confirmed, pending := c.check(ip, p)
			switch {
			case confirmed:
				return clientVerdict{class: p.Class, reason: p.Name + ", rDNS confirmed"}
			case pending:
				return clientVerdict{class: "unknown", reason: "claims " + p.Name + ", rDNS check pending", pending: true}
			}
		}
		return clientVerdict{class: "unknown", reason: "claims " + p.Name + ", address is not " + p.Name + "'s"}
	}
	switch {
	case g.webdriver:
		return clientVerdict{class: "unknown", reason: "navigator.webdriver set"}
	case g.ua == "":
		return clientVerdict{class: "unknown", reason: "no user agent"}
	case automationUA.MatchString(g.ua):
		return clientVerdict{class: "unknown", reason: "client " + Client(g.ua)}
	case f.fetchMeta && g.server > 0 && g.browser == 0 && g.noFetch == g.server && g.noLang == g.server &&
		looksLikeBrowser(g.ua):
		// Every browser sends Accept-Language, and every current one Sec-Fetch-Mode; this feed carries both.
		return clientVerdict{class: "unknown", reason: "missing browser headers"}
	}
	return clientVerdict{}
}

// weakSignals are the hints that a person's browser is none: each alone is common, two together are not.
func (c *Classifier) weakSignals(g uaDay, f siteFacts, nets *refdata.Networks, addr netip.Addr) []string {
	var weak []string
	if f.feed {
		if f.script && g.docs > 0 && g.browser == 0 {
			weak = append(weak, "no script beacon")
		}
		if g.docs > 0 && g.assets == 0 {
			weak = append(weak, "no assets")
		}
		if !f.fetchMeta && g.server >= 3 && g.noFetch == g.server && g.noLang == g.server && looksLikeBrowser(g.ua) {
			weak = append(weak, "missing browser headers")
		}
		if g.server >= 10 && g.refused*2 >= g.server {
			weak = append(weak, "mostly refused")
		}
		// A page load fires its assets and API calls in a burst: pace says something only without any page.
		if apiOnly := g.server >= apiOnlyRequests && g.docs == 0 && g.assets == 0 && g.browser == 0; apiOnly {
			weak = append(weak, "API only, no page loaded")
			if g.last > g.first && time.Duration(g.last-g.first)*time.Millisecond/time.Duration(g.server-1) < rapidGap {
				weak = append(weak, "rapid requests")
			}
		}
	}
	if as, ok := nets.Lookup(addr); ok && refdata.NetworkKind(as.Category) == "hosting" {
		weak = append(weak, "hosting network")
	}
	return weak
}

func goodSignals(g uaDay) []string {
	var good []string
	if g.browser > 0 {
		good = append(good, "script beacon")
	}
	if g.engaged > 0 {
		good = append(good, "engaged")
	}
	if g.assets > 0 {
		good = append(good, "assets loaded")
	}
	return good
}

// addDay sums two user agents' days; the user agent is the larger one's (a browser test looks at it).
func addDay(a, b uaDay) uaDay {
	if b.n > a.n {
		a.ua = b.ua
	}
	a.n += b.n
	a.browser += b.browser
	a.server += b.server
	a.docs += b.docs
	a.assets += b.assets
	a.refused += b.refused
	a.webdriver = a.webdriver || b.webdriver
	a.noFetch += b.noFetch
	a.noLang += b.noLang
	a.engaged += b.engaged
	a.pageView += b.pageView
	if a.first == 0 || (b.first != 0 && b.first < a.first) {
		a.first = b.first
	}
	a.last = max(a.last, b.last)
	return a
}

func sumDays(gs []uaDay) uaDay {
	var s uaDay
	for _, g := range gs {
		s = addDay(s, g)
	}
	return s
}

// classPhrase names a class in a reason.
var classPhrase = map[string]string{"unknown": "unknown automated", "bot": "known bot", "monitor": "uptime probe"}
