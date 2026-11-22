// Package classify decides what kind of actor an address key is on a day: human, known bot, uptime probe or unknown
// automated, with the reasons the dashboard shows. Two steps: an instant guess when an event is written (the bot
// list and the client string, nothing that needs history), and a periodic pass over the recent window that weighs
// the actor's whole day (script beacon, assets, headers, network) and stamps the result on its events.
package classify

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"slices"
	"strings"
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

	mu       sync.Mutex
	today    int64
	known    map[int64]map[string]int // site → ip_key → cls, today's
	lastPass time.Time
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
	c.mu.Lock()
	cls, ok := c.known[e.SiteID][e.IPKey]
	sameDay := e.TS/dayMS == c.today
	c.mu.Unlock()
	if ok && sameDay {
		e.Cls = cls
		return
	}
	e.Cls = c.instant(a, e.UA)
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

// Pass classifies every actor with events since the last pass (since the start of yesterday on the first), for
// each day it was active, and stamps the class on that day's events.
func (c *Classifier) Pass(ctx context.Context) error {
	now := c.now()
	c.mu.Lock()
	since := c.lastPass
	c.mu.Unlock()
	if since.IsZero() {
		since = time.UnixMilli((now.UnixMilli()/dayMS - 1) * dayMS)
	}
	// Overlap a little: an event the writer had queued at the last pass lands with an earlier ts.
	since = since.Add(-time.Minute)
	today := now.UnixMilli() / dayMS
	known := map[int64]map[string]int{}
	for _, s := range c.Sites.All() {
		got, err := c.passSite(ctx, s, since.UnixMilli(), today)
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
		c.known = map[int64]map[string]int{}
	}
	for site, m := range known {
		if c.known[site] == nil {
			c.known[site] = map[string]int{}
		}
		for k, v := range m {
			c.known[site][k] = v
		}
	}
	c.lastPass = now
	c.mu.Unlock()
	return nil
}

// actorDay is one actor's day as the pass weighs it.
type actorDay struct {
	key, ip, ua       string
	browser, server   int64
	docs, assets      int64
	refused           int64
	webdriver         bool
	noFetch, noLang   int64
	uas               int64
	asn               int64
	engaged, pageView int64
}

func (c *Classifier) passSite(ctx context.Context, s *sites.Site, since, today int64) (map[string]int, error) {
	// Which actors moved since the last pass, on which days.
	rows, err := c.DB.DB.QueryContext(ctx, `SELECT DISTINCT ip_key, ts / ? FROM event WHERE site_id = ? AND ts >= ?`,
		dayMS, s.ID, since)
	if err != nil {
		return nil, err
	}
	type kd struct {
		key string
		day int64
	}
	var moved []kd
	for rows.Next() {
		var x kd
		if err := rows.Scan(&x.key, &x.day); err != nil {
			_ = rows.Close()
			return nil, err
		}
		moved = append(moved, x)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	scriptSite, err := c.hasBrowser(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, m := range moved {
		a, err := c.actorDay(ctx, s.ID, m.key, m.day)
		if err != nil {
			return nil, err
		}
		class, reasons := c.Decide(ctx, a, s.HasFeed, scriptSite)
		if err := c.DB.SetActorClass(ctx, store.ActorClass{SiteID: s.ID, IPKey: m.key, Day: m.day, Class: class,
			Reasons: reasons}); err != nil {
			return nil, err
		}
		if m.day == today {
			out[m.key] = store.ClassCode(class)
		}
	}
	return out, nil
}

// hasBrowser reports whether the site's script delivered in the last week: only then is a missing beacon a signal.
func (c *Classifier) hasBrowser(ctx context.Context, site int64) (bool, error) {
	var ok bool
	err := c.DB.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM ingest_stat WHERE site_id = ? AND hour >= ?
		AND accepted > accepted_server)`, site, c.now().Add(-7*24*time.Hour).Unix()/3600).Scan(&ok)
	return ok, err
}

func (c *Classifier) actorDay(ctx context.Context, site int64, key string, day int64) (actorDay, error) {
	a := actorDay{key: key}
	var webdriver sql.NullInt64
	err := c.DB.DB.QueryRowContext(ctx, `SELECT MIN(ip), COALESCE(MAX(ua), ''),
		SUM(source = 'browser'), SUM(source = 'server'),
		SUM(source = 'server' AND name = 'document'),
		SUM(source = 'server' AND (name IN ('script', 'style', 'image', 'font') OR path LIKE '/assets/%')),
		SUM(source = 'server' AND status >= 400 AND status < 500),
		MAX(source = 'browser' AND props LIKE '%"webdriver":true%'),
		SUM(source = 'server' AND props NOT LIKE '%"sf_mode"%'),
		SUM(source = 'server' AND props NOT LIKE '%"lang"%'),
		COUNT(DISTINCT ua), MAX(asn), SUM(kind = 'engagement'), SUM(kind = 'page_view')
		FROM event WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ?`,
		site, key, day*dayMS, (day+1)*dayMS).Scan(&a.ip, &a.ua, &a.browser, &a.server, &a.docs, &a.assets,
		&a.refused, &webdriver, &a.noFetch, &a.noLang, &a.uas, &a.asn, &a.engaged, &a.pageView)
	a.webdriver = webdriver.Int64 == 1
	return a, err
}

// Decide weighs an actor-day: the bot list first (address, confirmed by rDNS where the list is not
// authoritative, or the client string), then strong automation signals (one suffices) and weak ones (two do).
func (c *Classifier) Decide(ctx context.Context, a actorDay, feed, script bool) (string, []string) {
	addr, _ := netip.ParseAddr(a.ip)
	var bots *refdata.Bots
	var nets *refdata.Networks
	if c.Ref != nil {
		bots, nets = c.Ref.Bots(), c.Ref.Networks()
	}
	confirm := func(p *refdata.Provider) bool {
		return c.Verifier != nil && c.Verifier.Confirm(ctx, a.ip, p.RdnsPatterns)
	}
	if p, ok := bots.ByIP(addr); ok {
		if p.Authoritative {
			return p.Class, []string{p.Name + " address range"}
		}
		if confirm(p) {
			return p.Class, []string{p.Name + ", rDNS confirmed"}
		}
	}
	if p, ok := bots.ByUA(a.ua); ok {
		switch {
		case !p.HasRanges:
			return p.Class, []string{"client is " + p.Name}
		case !p.Authoritative && confirm(p):
			return p.Class, []string{p.Name + ", rDNS confirmed"}
		default:
			return "unknown", []string{"claims " + p.Name + ", address is not " + p.Name + "'s"}
		}
	}
	var strong, weak, good []string
	switch {
	case a.webdriver:
		strong = append(strong, "navigator.webdriver set")
	case a.ua == "":
		strong = append(strong, "no user agent")
	case automationUA.MatchString(a.ua):
		strong = append(strong, "client "+Client(a.ua))
	}
	if feed {
		if script && a.docs > 0 && a.browser == 0 {
			weak = append(weak, "no script beacon")
		}
		if a.docs > 0 && a.assets == 0 {
			weak = append(weak, "no assets")
		}
		if a.server >= 3 && a.noFetch == a.server && a.noLang == a.server && looksLikeBrowser(a.ua) {
			weak = append(weak, "missing browser headers")
		}
		if a.server >= 10 && a.refused*2 >= a.server {
			weak = append(weak, "mostly refused")
		}
	}
	if as, ok := nets.Lookup(addr); ok && refdata.NetworkKind(as.Category) == "hosting" {
		weak = append(weak, "hosting network")
	}
	if a.browser > 0 {
		good = append(good, "script beacon")
	}
	if a.engaged > 0 {
		good = append(good, "engaged")
	}
	if a.assets > 0 {
		good = append(good, "assets loaded")
	}
	switch {
	case len(strong) > 0:
		return "unknown", slices.Concat(strong, weak)
	case len(weak) >= 2 && a.engaged == 0:
		return "unknown", weak
	}
	return "human", slices.Concat(good, weak)
}

func looksLikeBrowser(ua string) bool {
	return strings.HasPrefix(ua, "Mozilla/")
}

var productRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9._-]*)(?:/([0-9][0-9A-Za-z._-]*))?`)

// Client is a short client name for a user agent: browser and OS for a browser, the product token for anything
// else ("curl/8.4.0").
func Client(ua string) string {
	if ua == "" {
		return "no user agent"
	}
	if looksLikeBrowser(ua) && !automationUA.MatchString(ua) {
		b, o, _ := ParseUA(ua)
		return b + " · " + o
	}
	if m := productRE.FindStringSubmatch(ua); m != nil {
		if m[2] != "" {
			return m[1] + "/" + m[2]
		}
		return m[1]
	}
	if len(ua) > 40 {
		return ua[:40]
	}
	return ua
}

// ParseUA reads browser, operating system and device class from a user agent, coarse on purpose.
func ParseUA(ua string) (browser, os, device string) {
	l := strings.ToLower(ua)
	switch {
	case strings.Contains(l, "edg/"):
		browser = "Edge"
	case strings.Contains(l, "opr/") || strings.Contains(l, "opera"):
		browser = "Opera"
	case strings.Contains(l, "samsungbrowser"):
		browser = "Samsung Internet"
	case strings.Contains(l, "firefox/") || strings.Contains(l, "fxios"):
		browser = "Firefox"
	case strings.Contains(l, "chrome/") || strings.Contains(l, "crios"):
		browser = "Chrome"
	case strings.Contains(l, "safari/"):
		browser = "Safari"
	default:
		browser = "Other"
	}
	switch {
	case strings.Contains(l, "iphone") || strings.Contains(l, "ipad") || strings.Contains(l, "ipod"):
		os = "iOS"
	case strings.Contains(l, "android"):
		os = "Android"
	case strings.Contains(l, "windows"):
		os = "Windows"
	case strings.Contains(l, "cros"):
		os = "ChromeOS"
	case strings.Contains(l, "mac os x") || strings.Contains(l, "macintosh"):
		os = "macOS"
	case strings.Contains(l, "linux"):
		os = "Linux"
	default:
		os = "Other"
	}
	switch {
	case strings.Contains(l, "ipad") || strings.Contains(l, "tablet"):
		device = "Tablet"
	case strings.Contains(l, "mobi") || strings.Contains(l, "iphone") || strings.Contains(l, "android"):
		device = "Mobile"
	case browser == "Other" && os == "Other":
		device = "Other"
	default:
		device = "Desktop"
	}
	return browser, os, device
}
