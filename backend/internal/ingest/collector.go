package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/ship/kit/httpx"
	"github.com/trick77/ship/kit/ratelimit"

	"github.com/trick77/nui/backend/internal/apix"
	"github.com/trick77/nui/backend/internal/sites"
	"github.com/trick77/nui/backend/internal/store"
)

// Limits of the browser endpoint (contract §1) and the feed (§2).
const (
	MaxBrowserBody   = 32 << 10
	MaxBrowserEvents = 50
	MaxFeedBody      = 256 << 10
	MaxFeedEvents    = 500
	maxURL           = 2048
	maxString        = 512
	maxProps         = 4 << 10
	maxDepth         = 8
	// maxClientSkew is how far the client's own clock may place an event before the batch was received: a batch is
	// sent within seconds of its events, a hidden tab's flush within minutes.
	maxClientSkew = 30 * time.Minute
)

// The browser rate limit (contract: "60 events / min per address or /64 · 600 per /48"). The /64 burst admits the
// largest batch; the /48 tier (an IPv4 /24) bounds a caller rotating through its block.
const (
	BrowserRate      = 1.0 // events a second per address or /64
	BrowserBurst     = 60
	BrowserWideRate  = 10.0 // per /48
	BrowserWideBurst = 600
	// FeedRate and FeedBurst are per site key: a busy backend reports every request, assets included.
	FeedRate  = 200.0
	FeedBurst = 5000
)

// BrowserTiers is the browser endpoint's limit: per address or /64, and per /24 or /48 on top.
func BrowserTiers(trustedHops int, now func() time.Time) *ratelimit.Tiers {
	return ratelimit.NewTiers(
		ratelimit.Tier{Limiter: ratelimit.New(ratelimit.Options{Rate: BrowserRate, Burst: BrowserBurst, Now: now}),
			Key: ratelimit.ClientKey(trustedHops)},
		ratelimit.Tier{Limiter: ratelimit.New(ratelimit.Options{Rate: BrowserWideRate, Burst: BrowserWideBurst, Now: now}),
			Key: ratelimit.ClientPrefixKey(trustedHops, 24, 48)},
	)
}

// FeedLimiter is the feed's limit, keyed by site.
func FeedLimiter(now func() time.Time) *ratelimit.Limiter {
	return ratelimit.New(ratelimit.Options{Rate: FeedRate, Burst: FeedBurst, Now: now})
}

// RateRule is the browser limit as the dashboard's footer states it.
func RateRule() string {
	return fmt.Sprintf("%d events / min per address or /64 · %d per /48", int(BrowserRate*60), int(BrowserWideRate*60))
}

// BatchRule is the browser batch limit as the footer states it.
func BatchRule() string {
	return fmt.Sprintf("%d KiB · %d events", MaxBrowserBody>>10, MaxBrowserEvents)
}

// Collector serves the two ingest endpoints.
type Collector struct {
	Sites       *sites.Registry
	Writer      *Writer
	Counters    *Counters
	Browser     *ratelimit.Tiers
	Feed        *ratelimit.Limiter
	TrustedHops int
	Now         func() time.Time
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// errTooLarge marks a limit breach: 413.
var errTooLarge = errors.New("too large")

// browserBatch is POST /api/in's body.
type browserBatch struct {
	V      int                        `json:"v"`
	Site   string                     `json:"site"`
	VID    string                     `json:"vid"`
	SID    string                     `json:"sid"`
	Sent   int64                      `json:"sent"`
	Ctx    map[string]json.RawMessage `json:"ctx"`
	Events []browserEvent             `json:"events"`
}

type browserEvent struct {
	T     int64           `json:"t"`
	K     string          `json:"k"`
	N     string          `json:"n"`
	P     json.RawMessage `json:"p"`
	URL   string          `json:"url"`
	Title string          `json:"title"`
}

var browserKinds = map[string]bool{
	"page_view": true, "session_start": true, "engagement": true, "outbound_click": true, "download": true,
	"form": true, "search": true, "js_error": true, "resource_error": true, "vital": true, "custom": true,
}

// siteOf is the site a refused request is counted for: its Origin's host when that is a configured site (a browser
// sets Origin and a page cannot forge it), else 0. Never the body's site field: a refused body is never trusted.
func (c *Collector) siteOf(r *http.Request) int64 {
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err == nil {
			if s, ok := c.Sites.Lookup(u.Hostname()); ok {
				return s.ID
			}
		}
	}
	return 0
}

func (c *Collector) count(site int64, f func(*store.IngestStat)) { c.Counters.Add(site, f) }

// HandleBrowser is POST /api/in.
func (c *Collector) HandleBrowser(w http.ResponseWriter, r *http.Request) {
	// The script reads nothing but the status; a 429's Retry-After must be readable cross-origin.
	w.Header().Set("Access-Control-Expose-Headers", "Retry-After")
	w.Header().Set("Cache-Control", "no-store")
	recv := c.now()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBrowserBody))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		c.reject(w, c.siteOf(r), http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		c.reject(w, c.siteOf(r), http.StatusBadRequest)
		return
	}
	var b browserBatch
	if err := json.Unmarshal(body, &b); err != nil || b.V != 1 || b.Site == "" {
		c.reject(w, c.siteOf(r), http.StatusBadRequest)
		return
	}
	site, ok := c.Sites.Lookup(b.Site)
	if !ok {
		c.count(c.siteOf(r), func(s *store.IngestStat) { s.UnknownDomain++ })
		w.WriteHeader(http.StatusForbidden)
		return
	}
	// A browser on another site naming this one (the script copied onto a foreign page) is refused: Origin is the
	// browser's, not the page's, to set.
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || !strings.EqualFold(u.Hostname(), site.Domain) {
			c.count(site.ID, func(s *store.IngestStat) { s.Rejected++ })
			w.WriteHeader(http.StatusForbidden)
			return
		}
	}
	evs, err := c.browserEvents(site, &b, r, recv)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		c.reject(w, site.ID, status)
		return
	}
	n := float64(len(evs))
	if ok, wait := c.Browser.Take(r, n); !ok {
		if wait == 0 { // more than any burst admits: a limit breach, not a pace one
			c.reject(w, site.ID, http.StatusRequestEntityTooLarge)
			return
		}
		c.count(site.ID, func(s *store.IngestStat) { s.RateLimited += int64(len(evs)) })
		apix.TooMany(w, wait)
		return
	}
	c.accept(site.ID, "browser", evs)
	w.WriteHeader(http.StatusNoContent)
}

func (c *Collector) reject(w http.ResponseWriter, site int64, status int) {
	c.count(site, func(s *store.IngestStat) { s.Rejected++ })
	w.WriteHeader(status)
}

func (c *Collector) accept(site int64, source string, evs []store.Event) {
	if len(evs) == 0 {
		return
	}
	n := int64(len(evs))
	if !c.Writer.Enqueue(evs) {
		c.count(site, func(s *store.IngestStat) { s.Dropped += n })
		return
	}
	c.count(site, func(s *store.IngestStat) {
		s.Accepted += n
		if source == "server" {
			s.AcceptedServer += n
		}
	})
	c.Counters.Seen(site, source)
}

// browserEvents validates a batch and turns it into rows. Every string and the props are bounded; anything over a
// bound refuses the whole batch (errTooLarge), anything malformed too.
func (c *Collector) browserEvents(site *sites.Site, b *browserBatch, r *http.Request, recv time.Time) ([]store.Event, error) {
	if len(b.Events) == 0 {
		return nil, errors.New("no events")
	}
	if len(b.Events) > MaxBrowserEvents {
		return nil, errTooLarge
	}
	if err := checkString(b.VID, maxString); err != nil {
		return nil, err
	}
	if err := checkString(b.SID, maxString); err != nil {
		return nil, err
	}
	ctx := map[string]any{}
	for k, raw := range b.Ctx {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		if err := checkValue(k, v, 0); err != nil {
			return nil, err
		}
		ctx[k] = v
	}
	pageURL, _ := ctx["url"].(string)
	ref, _ := ctx["ref"].(string)
	ip := httpx.ClientIP(r, c.TrustedHops)
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return nil, err
	}
	ipKey := ratelimit.Key(ip)
	ua := truncate(r.UserAgent(), maxString)
	visitor := b.VID
	if visitor == "" {
		visitor = anonVisitor(ipKey, ua)
	}
	hdr := map[string]string{}
	for name, vals := range r.Header {
		lname := strings.ToLower(name)
		if lname == "accept-language" || strings.HasPrefix(lname, "sec-ch-ua") || strings.HasPrefix(lname, "sec-fetch-") {
			hdr[lname] = truncate(strings.Join(vals, ", "), maxString)
		}
	}
	refHost := hostOf(ref)
	if strings.EqualFold(refHost, site.Domain) {
		refHost = ""
	}
	out := make([]store.Event, 0, len(b.Events))
	for i, e := range b.Events {
		if !browserKinds[e.K] {
			return nil, fmt.Errorf("kind %q", e.K)
		}
		if len(e.P) > maxProps {
			return nil, errTooLarge
		}
		if err := checkString(e.URL, maxURL); err != nil {
			return nil, err
		}
		if err := checkString(e.Title, maxString); err != nil {
			return nil, err
		}
		if err := checkString(e.N, maxString); err != nil {
			return nil, err
		}
		if e.K == "custom" && e.N == "" {
			return nil, errors.New("custom event without a name")
		}
		var p map[string]any
		if len(e.P) > 0 && string(e.P) != "null" {
			if err := json.Unmarshal(e.P, &p); err != nil {
				return nil, err
			}
			if err := checkValue("p", p, 0); err != nil {
				return nil, err
			}
		}
		evURL := pageURL
		if e.URL != "" {
			evURL = e.URL
		}
		u, err := url.Parse(evURL)
		if evURL != "" && (err != nil || !strings.EqualFold(u.Hostname(), site.Domain)) {
			return nil, errors.New("url outside the site")
		}
		path := "/"
		if u != nil && u.EscapedPath() != "" {
			path = u.EscapedPath()
		}
		props := map[string]any{}
		if p != nil {
			props["p"] = p
		}
		if e.Title != "" {
			props["title"] = e.Title
		}
		// The context and the browser headers once per batch: they are the same for every event in it.
		if i == 0 {
			props["ctx"] = ctx
			if len(hdr) > 0 {
				props["hdr"] = hdr
			}
		}
		pj, _ := json.Marshal(props)
		ev := store.Event{
			SiteID: site.ID, TS: eventTime(recv, b.Sent, e.T), Source: "browser", Kind: e.K, Name: eventName(e, p),
			Visitor: visitor, SID: b.SID, IP: addr.Unmap().String(), IPKey: ipKey, UA: ua, Path: path,
			Template: site.Template(path), URL: evURL, Props: string(pj),
		}
		if e.K == "page_view" || e.K == "session_start" {
			// The page's own URL and referrer are kept whole (campaign tags live in the query); the feed's request
			// paths are not, since an API query string can carry a key.
			ev.Ref, ev.RefHost = ref, refHost
		}
		out = append(out, ev)
	}
	return out, nil
}

// eventTime is the receive time moved back by how much earlier than the batch the client says the event happened,
// bounded: the client's clock only orders events, it never dates them.
func eventTime(recv time.Time, sent, t int64) int64 {
	skew := sent - t
	if sent == 0 || t == 0 || skew < 0 {
		skew = 0
	}
	skew = min(skew, maxClientSkew.Milliseconds())
	return recv.UnixMilli() - skew
}

// eventName is the event's list key: the custom name, the outbound host, the error message, the vital's name.
func eventName(e browserEvent, p map[string]any) string {
	str := func(k string) string { s, _ := p[k].(string); return s }
	switch e.K {
	case "custom":
		return e.N
	case "outbound_click", "download":
		if h := hostOf(str("url")); h != "" {
			return h
		}
		return truncate(str("url"), 200)
	case "js_error":
		return truncate(str("msg"), 200)
	case "resource_error":
		return truncate(stripQuery(str("url")), 200)
	case "vital":
		return truncate(str("name"), 16)
	case "form":
		return truncate(str("phase"), 16)
	}
	return ""
}

// checkValue bounds a decoded JSON value: string lengths (URL-like keys longer), depth, array and object sizes.
func checkValue(key string, v any, depth int) error {
	if depth > maxDepth {
		return errTooLarge
	}
	switch t := v.(type) {
	case string:
		limit := maxString
		if urlKey(key) {
			limit = maxURL
		}
		return checkString(t, limit)
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			return errors.New("number")
		}
	case []any:
		if len(t) > MaxBrowserEvents {
			return errTooLarge
		}
		for _, x := range t {
			if err := checkValue(key, x, depth+1); err != nil {
				return err
			}
		}
	case map[string]any:
		if len(t) > 64 {
			return errTooLarge
		}
		for k, x := range t {
			if err := checkString(k, 64); err != nil {
				return err
			}
			if err := checkValue(k, x, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func urlKey(k string) bool {
	switch k {
	case "url", "ref", "src", "href":
		return true
	}
	return false
}

func checkString(s string, limit int) error {
	if len(s) > limit {
		return errTooLarge
	}
	return nil
}

// HandleFeed is POST /api/feed.
func (c *Collector) HandleFeed(w http.ResponseWriter, r *http.Request) {
	key, _ := httpx.Bearer(r)
	site, ok := c.Sites.ByKey(key)
	if !ok {
		c.count(0, func(s *store.IngestStat) { s.Rejected++ })
		apix.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	fail := func(status int, msg string) {
		c.count(site.ID, func(s *store.IngestStat) { s.Rejected++ })
		apix.Error(w, status, msg)
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxFeedBody))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		fail(http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var b feedBatch
	if err != nil || json.Unmarshal(body, &b) != nil {
		fail(http.StatusBadRequest, "malformed request")
		return
	}
	if b.V != 1 || !strings.EqualFold(b.Site, site.Domain) {
		fail(http.StatusBadRequest, "v must be 1 and site the key's site")
		return
	}
	if len(b.Events) > MaxFeedEvents {
		fail(http.StatusRequestEntityTooLarge, "too many events")
		return
	}
	evs, err := c.feedEvents(site, b.Events)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			fail(http.StatusRequestEntityTooLarge, "a field is over its limit")
		} else {
			fail(http.StatusBadRequest, "malformed event: "+err.Error())
		}
		return
	}
	if ok, wait := c.Feed.Take(strconv.FormatInt(site.ID, 10), float64(len(evs)), 0); !ok {
		c.count(site.ID, func(s *store.IngestStat) { s.RateLimited += int64(len(evs)) })
		apix.TooMany(w, wait)
		return
	}
	c.accept(site.ID, "server", evs)
	w.WriteHeader(http.StatusNoContent)
}

type feedBatch struct {
	V      int         `json:"v"`
	Site   string      `json:"site"`
	Events []feedEvent `json:"events"`
}

type feedEvent struct {
	T       int64  `json:"t"`
	IP      string `json:"ip"`
	UA      string `json:"ua"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Route   string `json:"route"`
	Status  int    `json:"status"`
	DurMS   int64  `json:"dur_ms"`
	Bytes   int64  `json:"bytes"`
	Ref     string `json:"ref"`
	Lang    string `json:"lang"`
	SFSite  string `json:"sf_site"`
	SFMode  string `json:"sf_mode"`
	SFDest  string `json:"sf_dest"`
	ChUA    string `json:"ch_ua"`
	Account string `json:"account"`
}

func (c *Collector) feedEvents(site *sites.Site, in []feedEvent) ([]store.Event, error) {
	now := c.now().UnixMilli()
	out := make([]store.Event, 0, len(in))
	for _, e := range in {
		for _, f := range []struct {
			v     string
			limit int
		}{{e.UA, maxString}, {e.Method, 16}, {e.Path, maxURL}, {e.Route, 200}, {e.Ref, maxURL}, {e.Lang, maxString},
			{e.SFSite, 32}, {e.SFMode, 32}, {e.SFDest, 32}, {e.ChUA, maxString}, {e.Account, 128}} {
			if err := checkString(f.v, f.limit); err != nil {
				return nil, err
			}
		}
		addr, err := netip.ParseAddr(e.IP)
		if err != nil {
			return nil, errors.New("ip")
		}
		if e.Status < 0 || e.Status > 999 || e.DurMS < 0 || e.Bytes < 0 {
			return nil, errors.New("status, dur_ms or bytes")
		}
		ts := e.T
		// The backend's clock dates its own records; one far off is the receive time instead.
		if ts < now-int64(time.Hour/time.Millisecond) || ts > now+int64(time.Minute/time.Millisecond) {
			ts = now
		}
		path := stripQuery(e.Path)
		if path == "" || path[0] != '/' {
			path = "/" + path
		}
		method := strings.ToUpper(e.Method)
		template := e.Route
		// No route (an unmatched 404, the probing case) or the app's catch-all: the path itself is the clue, so
		// the site's rules or the heuristic name it.
		if template == "" || e.Status == http.StatusNotFound || strings.HasSuffix(template, " /") || template == "/" {
			template = strings.TrimSpace(method + " " + site.Template(path))
		}
		props := map[string]any{"bytes": e.Bytes}
		for k, v := range map[string]string{"lang": e.Lang, "sf_site": e.SFSite, "sf_mode": e.SFMode,
			"sf_dest": e.SFDest, "ch_ua": e.ChUA} {
			if v != "" {
				props[k] = v
			}
		}
		pj, _ := json.Marshal(props)
		ipKey := ratelimit.Key(e.IP)
		refHost := hostOf(e.Ref)
		if strings.EqualFold(refHost, site.Domain) {
			refHost = ""
		}
		out = append(out, store.Event{
			SiteID: site.ID, TS: ts, Source: "server", Kind: "request", Name: e.SFDest,
			Visitor: anonVisitor(ipKey, e.UA), IP: addr.Unmap().String(), IPKey: ipKey, UA: e.UA, Path: path,
			Template: template, Ref: stripQuery(e.Ref), RefHost: refHost, Method: method, Status: e.Status,
			DurMS: e.DurMS, Account: e.Account, Props: string(pj),
		})
	}
	return out, nil
}

// anonVisitor is the visitor id of a client without the script's own: address key and user agent.
func anonVisitor(ipKey, ua string) string {
	sum := sha256.Sum256([]byte(ipKey + "\x00" + ua))
	return "h:" + hex.EncodeToString(sum[:8])
}

func hostOf(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// stripQuery drops a URL's query and fragment: they carry tokens and search terms nobody asked to keep.
func stripQuery(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		return raw[:i]
	}
	return raw
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
