// Package classify decides what kind of actor an address key is on a day: human, known bot, uptime probe or unknown
// automated, with the reasons the dashboard shows. Two steps: an instant guess when an event is written (the bot
// list, the client string, a script beacon, nothing that needs history), and a periodic pass over the recent window
// that weighs the actor's whole day (script beacon, assets, headers, network) and stamps the result on its events.
//
// A person is shown, never assumed: on a site whose script or feed can show a browser, a client is human only with a
// browser's evidence (a script beacon, engagement or assets loaded). A browser user agent alone proves nothing: a
// crawler wearing one (PetalBot reads "Safari · Android") once counted as a person for want of a contrary sign.
//
// An address is often shared (an office, a carrier's NAT): the pass weighs each of its user agents on its own. The
// actor's class follows the majority of its events; the clients that differ keep their own class on their own events,
// so one curl request behind a NAT of people is counted as automated without making the people automated, and a
// script cannot hide behind one browser request.
package classify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"

	"github.com/trick77/stakeout/backend/internal/refdata"
	"github.com/trick77/stakeout/backend/internal/sites"
	"github.com/trick77/stakeout/backend/internal/store"
)

// Every is how often the pass runs.
const Every = 5 * time.Minute

const dayMS = int64(24 * time.Hour / time.Millisecond)

// maxUAs is how many of an actor-day's user agents the pass weighs one by one, the most frequent; the rest (a
// client rotating its user agent) is stamped with the actor's class.
const maxUAs = 64

// Thresholds of the pace signals: a client with at least apiOnlyRequests requests and no page, asset or beacon only
// calls the API, and its requests averaging under rapidGap apart are rapid; both are weak, a single-page app's start
// or a service worker does that. At least machineRequests averaging under machineGap apart is no person's browser.
const (
	apiOnlyRequests = 20
	rapidGap        = 500 * time.Millisecond
	machineRequests = 100
	machineGap      = 100 * time.Millisecond
)

// reasonMachinePace is the conclusive pace verdict's reason; it implies the weak "rapid requests".
const reasonMachinePace = "machine pace"

// maxRecheck bounds the actor-days waiting for an rDNS answer.
const maxRecheck = 10_000

// automationUA matches clients that are no browser: HTTP libraries, command-line tools, headless and scripted
// browsers, scanners, and the generic bot words a crawler not on the list still uses: a contact URL ("+https://"; no
// browser has one) and a bot named in the compatible part ("(compatible; PetalBot;"). A bare "…bot" word is not
// enough: phones are named CUBOT.
var automationUA = regexp.MustCompile(`(?i)(curl|wget|python|go-http-client|okhttp|java/|apache-httpclient|libwww|` +
	`scrapy|httpx|aiohttp|axios|node-fetch|undici|headlesschrome|phantomjs|puppeteer|playwright|selenium|` +
	`\bbot\b|bot/|crawler|spider|scanner|nikto|nmap|masscan|zgrab|nuclei|sqlmap|postman|insomnia|guzzle|` +
	`ruby|perl|php/|powershell|winhttp|dataprovider|censys|shodan|\+https?://|compatible;[^)]*bot)`)

// reasonNoBrowser is the verdict on a client that showed none of a browser's evidence where the site would show it.
const reasonNoBrowser = "no sign of a browser"

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
	inst := c.instantEvent(a, e)
	c.mu.Lock()
	k, ok := c.known[e.SiteID][e.IPKey]
	sameDay := e.TS/dayMS == c.today
	c.mu.Unlock()
	switch {
	case !ok || !sameDay:
		e.Cls = inst
	case inst == store.ClsHuman && (k.ua[e.UA] == store.ClsUnknown || k.ua[e.UA] == 0 && k.cls == store.ClsUnknown):
		// A beacon after a pass that saw only the page request: the person's evidence, not yet weighed.
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

// instantEvent is instant for one event: a script beacon from a browser user agent is a person's evidence itself
// (the pass then confirms it), unless the browser says it is scripted. Only a batch's first event carries the
// browser's context (navigator.webdriver): the others wait for the pass, counted with the key's person meanwhile.
func (c *Classifier) instantEvent(a netip.Addr, e *store.Event) int {
	inst := c.instant(a, e.UA)
	if inst != store.ClsPending || e.Source != "browser" || !looksLikeBrowser(e.UA) {
		return inst
	}
	var p struct {
		Ctx *struct {
			Webdriver bool `json:"webdriver"`
		} `json:"ctx"`
	}
	if json.Unmarshal([]byte(e.Props), &p) != nil || p.Ctx == nil {
		return inst
	}
	if p.Ctx.Webdriver {
		return store.ClsUnknown
	}
	return store.ClsHuman
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
	// none of a browser's. assets: its feed reported asset requests on the actor's day, so a visitor without any is
	// a signal (a site whose assets come from a CDN or the browser's cache never reports them).
	feed, script, fetchMeta, assets bool
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
	assetDays := map[int64]bool{}
	for _, m := range moved {
		a, err := c.actorDay(ctx, s.ID, m.key, m.day)
		if err != nil {
			return nil, err
		}
		if len(a.uas) == 0 {
			c.setRecheck(m, false)
			continue
		}
		fd := f
		if s.HasFeed {
			seen, ok := assetDays[m.day]
			if !ok {
				if seen, err = c.hasAssets(ctx, s.ID, m.day); err != nil {
					return nil, err
				}
				assetDays[m.day] = seen
			}
			fd.assets = seen
		}
		if err := c.beaconsElsewhere(ctx, s.ID, m, a.uas, fd); err != nil {
			return nil, err
		}
		d := c.Decide(a, fd)
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

// beaconWindow is how far a page request and its beacon may lie apart.
const beaconWindow = int64(time.Minute / time.Millisecond)

// beaconsElsewhere marks the clients without a browser's evidence on their own address whose page drew a beacon
// from another: the page reaches the product and the beacon Stakeout over different paths (dual stack, a relay that
// leaves per host), so their address keys differ. The beacon must name the same page with the same user agent within
// a minute: a crawler wearing a browser's user agent rarely meets a person's beacon for the same page that closely.
func (c *Classifier) beaconsElsewhere(ctx context.Context, site int64, m actorKey, uas []uaDay, f siteFacts) error {
	for i, g := range uas {
		if !f.script || g.browser > 0 || g.engaged > 0 || g.assets > 0 || g.pages == 0 {
			continue
		}
		err := c.DB.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM (SELECT site_id, ts, ua, path, ip_key
				FROM event WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ? AND ua = ? AND source = 'server'
				AND `+pageRequest+`) s
			JOIN event b ON b.site_id = s.site_id AND b.ts >= s.ts - ? AND b.ts <= s.ts + ? AND b.source = 'browser'
			AND b.kind = 'page_view' AND b.ua = s.ua AND b.path = s.path AND b.ip_key != s.ip_key)`,
			site, m.key, m.day*dayMS, (m.day+1)*dayMS, g.ua, beaconWindow, beaconWindow).Scan(&uas[i].beaconElsewhere)
		if err != nil {
			return err
		}
	}
	return nil
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

// hasAssets reports whether the site's feed reported an asset request on day: only then is a visitor without any a
// signal.
func (c *Classifier) hasAssets(ctx context.Context, site, day int64) (bool, error) {
	var ok bool
	err := c.DB.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM event WHERE site_id = ? AND ts >= ? AND ts < ?
		AND source = 'server' AND `+assetRequest+`)`, site, day*dayMS, (day+1)*dayMS).Scan(&ok)
	return ok, err
}

// assetRequest is a feed record that fetched an asset.
const assetRequest = `(name IN ('script', 'style', 'image', 'font') OR path LIKE '/assets/%')`

// pageRequest is a feed record that loaded a page. Sec-Fetch-Dest says so when the client sends it, and a crawler
// does not: then a GET answered below 400 that is no asset, no API call and no file (no dot in the last segment).
const pageRequest = `(name = 'document' OR (name = '' AND method = 'GET' AND status < 400 AND NOT ` + assetRequest +
	` AND path NOT LIKE '/api/%' AND instr(substr(path, length(rtrim(path, replace(path, '/', ''))) + 1), '.') = 0))`

// actorDay reads an actor's day per user agent, the most frequent first.
func (c *Classifier) actorDay(ctx context.Context, site int64, key string, day int64) (actorDay, error) {
	a := actorDay{key: key}
	rows, err := c.DB.DB.QueryContext(ctx, `SELECT ua, COUNT(*),
		SUM(source = 'browser'), SUM(source = 'server'),
		SUM(source = 'server' AND name = 'document'), SUM(source = 'server' AND `+pageRequest+`),
		SUM(source = 'server' AND `+assetRequest+`),
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
		if err := rows.Scan(&g.ua, &g.n, &g.browser, &g.server, &g.docs, &g.pages, &g.assets, &g.refused, &g.webdriver,
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
// headers every browser sends, a machine's pace, no browser's evidence where the site would show it), then the
// majority. Weak signals are only reasons: each fits an ordinary visitor (a blocked tracker, cached assets, a
// single-page app's API calls, a VPN). Clients that differ from the majority keep their own class.
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
		switch c.check(a.ip, p) {
		case refdata.Confirmed:
			return decision{class: p.Class, reasons: []string{p.Name + ", rDNS confirmed"}}
		case refdata.Pending, refdata.Failed:
			pending = true
		}
	}
	var people uaDay // the people's clients summed
	verdicts := make([]clientVerdict, len(a.uas))
	var total, automated int64
	for i, g := range a.uas {
		v := c.clientVerdict(a.ip, addr, g, f, bots)
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
		// Mostly people: human, the weak signals listed with the good ones.
		d.class = "human"
		d.reasons = append(goodSignals(people), weak...)
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
		case v.class == "" && (a.uas[i].browser > 0 || a.uas[i].assets > 0 || a.uas[i].engaged > 0 ||
			a.uas[i].beaconElsewhere):
			// A person's browser behind an automated address keeps its own class.
			d.byUA[a.uas[i].ua] = "human"
		}
		if v.class != "" && v.class != d.class {
			d.byUA[a.uas[i].ua] = v.class
		}
	}
	if d.class == "unknown" {
		for _, w := range c.weakSignals(sumDays(a.uas), f, nets, addr) {
			if w == "rapid requests" && slices.Contains(d.reasons, reasonMachinePace) ||
				(w == "no script beacon" || w == "no assets") && slices.Contains(d.reasons, reasonNoBrowser) {
				continue
			}
			d.reasons = append(d.reasons, w)
		}
	}
	return d
}

// check asks the verifier for a provider's rDNS, from its cache.
func (c *Classifier) check(ip string, p *refdata.Provider) refdata.Verdict {
	if c.Verifier == nil {
		return refdata.Unverified
	}
	return c.Verifier.Check(ip, p.RdnsPatterns)
}

// claimVerdict weighs a client's claim to be p, a provider whose address list is not authoritative, by what rDNS
// knows. Only a completed lookup refutes the claim. Unchecked (the network's lookup budget spent, a DNS timeout, the
// lookup still queued), an address inside p's listed range is taken as p's and marked so: a real crawler is no
// impostor for want of an answer. One outside the range stays unknown, its reason naming the missing check.
func claimVerdict(p *refdata.Provider, v refdata.Verdict, bots *refdata.Bots, addr netip.Addr) clientVerdict {
	var state string
	switch v {
	case refdata.Confirmed:
		return clientVerdict{class: p.Class, reason: p.Name + ", rDNS confirmed"}
	case refdata.Refuted:
		return clientVerdict{class: "unknown", reason: "claims " + p.Name + ", address is not " + p.Name + "'s"}
	case refdata.Pending:
		state = "rDNS check pending"
	case refdata.Failed:
		state = "rDNS lookup failed"
	default:
		state = "rDNS not verified"
	}
	// A pending or failed lookup is asked again soon: the pass decides again once it answers.
	retry := v == refdata.Pending || v == refdata.Failed
	if r, ok := bots.ByIP(addr); ok && r.Name == p.Name {
		return clientVerdict{class: p.Class, reason: p.Name + " address range, " + state, pending: retry}
	}
	return clientVerdict{class: "unknown", reason: "claims " + p.Name + ", " + state, pending: retry}
}

// clientVerdict is one user agent's own, conclusive evidence.
func (c *Classifier) clientVerdict(ip string, addr netip.Addr, g uaDay, f siteFacts, bots *refdata.Bots) clientVerdict {
	if p, ok := bots.ByUA(g.ua); ok {
		switch {
		case !p.HasRanges:
			return clientVerdict{class: p.Class, reason: "client is " + p.Name}
		case !p.Authoritative:
			return claimVerdict(p, c.check(ip, p), bots, addr)
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
	case g.server >= machineRequests && apiOnly(g) && avgGap(g) < machineGap:
		return clientVerdict{class: "unknown", reason: reasonMachinePace}
	case f.script && g.browser == 0 && g.engaged == 0 && g.assets == 0 && !g.beaconElsewhere:
		// The site's script shows a browser by its beacon (its assets too, but on a feed alone one cached visit would
		// be no person), and this client showed neither.
		return clientVerdict{class: "unknown", reason: reasonNoBrowser}
	}
	return clientVerdict{}
}

// apiOnly: the client's requests loaded no page or asset and its script sent no beacon.
func apiOnly(g uaDay) bool { return g.docs == 0 && g.assets == 0 && g.browser == 0 }

// avgGap is the average time between the client's server requests, 0 for fewer than two or no span.
func avgGap(g uaDay) time.Duration {
	if g.server < 2 || g.last <= g.first {
		return 0
	}
	return time.Duration(g.last-g.first) * time.Millisecond / time.Duration(g.server-1)
}

// weakSignals are the hints that a person's browser is none, shown as reasons; none of them decides.
func (c *Classifier) weakSignals(g uaDay, f siteFacts, nets *refdata.Networks, addr netip.Addr) []string {
	var weak []string
	if f.feed {
		if f.script && g.pages > 0 && g.browser == 0 && !g.beaconElsewhere {
			weak = append(weak, "no script beacon")
		}
		if f.assets && g.pages > 0 && g.assets == 0 {
			weak = append(weak, "no assets")
		}
		if !f.fetchMeta && g.server >= 3 && g.noFetch == g.server && g.noLang == g.server && looksLikeBrowser(g.ua) {
			weak = append(weak, "missing browser headers")
		}
		if g.server >= 10 && g.refused*2 >= g.server {
			weak = append(weak, "mostly refused")
		}
		// A page load fires its assets and API calls in a burst: pace says something only without any page.
		if g.server >= apiOnlyRequests && apiOnly(g) {
			weak = append(weak, "API only, no page loaded")
			if g.last > g.first && avgGap(g) < rapidGap {
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
	} else if g.beaconElsewhere {
		good = append(good, "script beacon from another address")
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
	a.pages += b.pages
	a.assets += b.assets
	a.beaconElsewhere = a.beaconElsewhere || b.beaconElsewhere
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
