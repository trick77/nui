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
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	// maxCtx bounds the context as stored (encoded), maxHeader each stored browser header, maxHeaders all of them
	// (encoded): the tracker budgets a batch by these, a header it cannot see included.
	maxCtx     = 4 << 10
	maxHeader  = 256
	maxHeaders = 2 << 10
	// maxEventBytes and maxBatchBytes bound what one event and one batch store (store.Event.Size), whatever the
	// body: the page URL, user agent and ids repeat on every event, so a small body can name a large write. An
	// event's bound leaves out the context and headers its batch stores once, on the first row: they have bounds
	// of their own, and the first row would otherwise take less of an event than every other.
	maxEventBytes = 16 << 10
	maxBatchBytes = 2 * MaxBrowserBody
	// StoreUnit is the stored bytes one token pays for: a batch costs an event per event, or per StoreUnit stored
	// when that is more. maxBatchBytes / StoreUnit stays below the burst, so a byte charge is never a 413.
	StoreUnit = 2 << 10
	// Body read deadlines: a body trickled in holds a handler, and the connection, no longer than this.
	browserBodyTimeout = 10 * time.Second
	feedBodyTimeout    = 30 * time.Second
)

// browserHeaders are the request headers the collector keeps, lower-case: the client hints and fetch metadata a
// browser sends, and the agent a Web Bot Auth client names. Only these: the names are the caller's to choose, any
// number of them.
var browserHeaders = []string{"accept-language", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
	"sec-ch-ua-platform-version", "sec-ch-ua-model", "sec-ch-ua-arch", "sec-ch-ua-bitness",
	"sec-ch-ua-full-version-list", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "sec-fetch-user",
	"signature-agent"}

// The browser rate limit (contract: 60 events / min per address or /64, 600 per /48; RateRule states it). The /64
// burst admits the largest batch; the /48 tier (an IPv4 /24) bounds a caller rotating through its block.
const (
	BrowserRate      = 1.0 // events a second per address or /64
	BrowserBurst     = 60
	BrowserWideRate  = 10.0 // per /48
	BrowserWideBurst = 600
	// FeedRate and FeedBurst are per site key: a busy backend reports every request, assets included.
	FeedRate  = 200.0
	FeedBurst = 5000
)

// BrowserTiers is the browser endpoint's limit: per address or /64, and per /24 or /48 on top, keyed by the
// caller's address as the collector stores it (apix.ClientAddr): never a header's text.
func BrowserTiers(trustedHops int, now func() time.Time) *ratelimit.Tiers {
	return ratelimit.NewTiers(
		ratelimit.Tier{Limiter: ratelimit.New(ratelimit.Options{Rate: BrowserRate, Burst: BrowserBurst, Now: now}),
			Key: apix.ClientKey(trustedHops, 32, 64)},
		ratelimit.Tier{Limiter: ratelimit.New(ratelimit.Options{Rate: BrowserWideRate, Burst: BrowserWideBurst, Now: now}),
			Key: apix.ClientKey(trustedHops, 24, 48)},
	)
}

// FeedLimiter is the feed's limit, keyed by site.
func FeedLimiter(now func() time.Time) *ratelimit.Limiter {
	return ratelimit.New(ratelimit.Options{Rate: FeedRate, Burst: FeedBurst, Now: now})
}

// RateRule is the browser limit as the dashboard's footer states it.
func RateRule() string {
	return fmt.Sprintf("%d events / min per address or /64"+apix.Sep+"%d per /48"+apix.Sep+
		"%d KiB stored counts as an event", int(BrowserRate*60), int(BrowserWideRate*60), StoreUnit>>10)
}

// BatchRule is the browser batch limit as the footer states it.
func BatchRule() string {
	return fmt.Sprintf("%d KiB"+apix.Sep+"%d events", MaxBrowserBody>>10, MaxBrowserEvents)
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
	// BodyTimeout overrides both endpoints' body read deadline (tests); 0 keeps the defaults.
	BodyTimeout time.Duration
}

// readBody reads at most limit bytes within timeout. status is 0 on success, else the answer: 413 over the limit,
// 408 for a body not delivered in time, 400 for any other read failure.
func readBody(w http.ResponseWriter, r *http.Request, limit int64, timeout time.Duration) ([]byte, int) {
	rc := http.NewResponseController(w)
	// Not every ResponseWriter can (a test recorder cannot); the real server's can.
	_ = rc.SetReadDeadline(time.Now().Add(timeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err == nil {
		_ = rc.SetReadDeadline(time.Time{})
		return body, 0
	}
	// The rest of a failed body is never read: the connection closes after the answer. Without this the server
	// would wait, deadline lifted, to discard what a slow sender still owes.
	w.Header().Set("Connection", "close")
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		return nil, http.StatusRequestEntityTooLarge
	case errors.Is(err, os.ErrDeadlineExceeded):
		return nil, http.StatusRequestTimeout
	}
	return nil, http.StatusBadRequest
}

func (c *Collector) bodyTimeout(def time.Duration) time.Duration {
	if c.BodyTimeout > 0 {
		return c.BodyTimeout
	}
	return def
}

// encodeJSON is json.Marshal without HTML escaping: <, > and & stay one byte each instead of six.
func encodeJSON(v any) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(b.String(), "\n")
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// errTooLarge marks a limit breach: 413. errNoAddr: the request's client address does not parse (400).
var (
	errTooLarge = errors.New("too large")
	errNoAddr   = errors.New("no client address")
)

// storedAddr parses a client address as it is stored and keyed: an IPv6 zone dropped (netip takes any text after
// "%", unbounded, and no public address has one), an IPv4-mapped address unmapped. Its text is at most 39 bytes.
func storedAddr(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.WithZone("").Unmap(), true
}

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
	// Every request pays one token before any work: a caller without one is refused before its body is read or
	// parsed, so a flood of junk or of unknown domains is throttled like a flood of events.
	if ok, wait := c.Browser.Take(r, 1); !ok {
		c.count(c.siteOf(r), func(s *store.IngestStat) { s.RateLimited++ })
		apix.TooMany(w, wait)
		return
	}
	body, status := readBody(w, r, MaxBrowserBody, c.bodyTimeout(browserBodyTimeout))
	if status != 0 {
		c.refuse(w, c.siteOf(r), status, false)
		return
	}
	var b browserBatch
	if err := json.Unmarshal(body, &b); err != nil || b.V != 1 || b.Site == "" {
		c.refuse(w, c.siteOf(r), http.StatusBadRequest, false)
		return
	}
	site, ok := c.Sites.Lookup(b.Site)
	if !ok {
		c.refuse(w, c.siteOf(r), http.StatusForbidden, true)
		return
	}
	// A browser on another site naming this one (the script copied onto a foreign page) is refused: Origin is the
	// browser's, not the page's, to set.
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || !strings.EqualFold(u.Hostname(), site.Domain) {
			c.refuse(w, site.ID, http.StatusForbidden, false)
			return
		}
	}
	evs, stored, err := c.browserEvents(site, &b, r, recv)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		c.refuse(w, site.ID, status, false)
		return
	}
	// The rest of the charge, the first token paid above.
	if cost := browserCost(len(evs), stored); cost > 1 {
		if ok, wait := c.Browser.Take(r, float64(cost-1)); !ok {
			if wait == 0 { // more than any burst admits: a limit breach, not a pace one
				c.reject(w, site.ID, http.StatusRequestEntityTooLarge)
				return
			}
			c.count(site.ID, func(s *store.IngestStat) { s.RateLimited += int64(len(evs)) })
			// The wait is for the rest of the charge; the batch sent again pays its token on arrival first, so
			// it needs one token more, or every retry is refused once more.
			apix.TooMany(w, wait+time.Duration(float64(time.Second)/BrowserRate))
			return
		}
	}
	c.accept(site.ID, "browser", evs)
	w.WriteHeader(http.StatusNoContent)
}

// browserCost is a batch's tokens: one per event, or one per StoreUnit stored when that is more.
func browserCost(events, stored int) int {
	return max(events, (stored+StoreUnit-1)/StoreUnit)
}

func (c *Collector) reject(w http.ResponseWriter, site int64, status int) {
	c.count(site, func(s *store.IngestStat) { s.Rejected++ })
	w.WriteHeader(status)
}

// refuse answers a browser request the collector will not store (its token is paid on arrival).
func (c *Collector) refuse(w http.ResponseWriter, site int64, status int, unknownDomain bool) {
	if unknownDomain {
		c.count(site, func(s *store.IngestStat) { s.UnknownDomain++ })
		w.WriteHeader(status)
		return
	}
	c.reject(w, site, status)
}

func (c *Collector) accept(site int64, source string, evs []store.Event) {
	if len(evs) == 0 {
		return
	}
	n, ns := int64(len(evs)), int64(0)
	if source == "server" {
		ns = n
	}
	// Counted before the writer can see them, in the hour the writer is told: a failed write takes them back from
	// that hour, never from a later one or before they were added.
	hour := c.Counters.Hour()
	c.Counters.AddAt(site, hour, func(s *store.IngestStat) { s.Accepted += n; s.AcceptedServer += ns })
	if !c.Writer.EnqueueAt(evs, hour) {
		c.Counters.AddAt(site, hour, func(s *store.IngestStat) { s.Accepted -= n; s.AcceptedServer -= ns; s.Dropped += n })
		return
	}
	c.Counters.Seen(site, source)
}

// browserEvents validates a batch and turns it into rows, returning them and the bytes they store. Every string and
// the props are bounded, and so is what each event and the batch store; anything over a bound refuses the whole
// batch (errTooLarge), anything malformed too.
func (c *Collector) browserEvents(site *sites.Site, b *browserBatch, r *http.Request, recv time.Time) ([]store.Event, int, error) {
	if len(b.Events) == 0 {
		return nil, 0, errors.New("no events")
	}
	if len(b.Events) > MaxBrowserEvents {
		return nil, 0, errTooLarge
	}
	if err := checkString(b.VID, maxString); err != nil {
		return nil, 0, err
	}
	if err := checkString(b.SID, maxString); err != nil {
		return nil, 0, err
	}
	var pageURL, ref string
	ctx := map[string]any{}
	for k, raw := range b.Ctx {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, 0, err
		}
		if err := checkValue(k, v, 0); err != nil {
			return nil, 0, err
		}
		// The page's URL and referrer have columns of their own: stored once there, not again in the context.
		switch k {
		case "url":
			pageURL, _ = v.(string)
		case "ref":
			ref, _ = v.(string)
		default:
			ctx[k] = v
		}
	}
	ctxJSON := encodeJSON(ctx)
	if len(ctxJSON) > maxCtx {
		return nil, 0, errTooLarge
	}
	addr, ok := apix.ClientAddr(r, c.TrustedHops)
	if !ok {
		return nil, 0, errNoAddr
	}
	ipKey := ratelimit.Key(addr.String())
	ua := truncate(r.UserAgent(), maxString)
	visitor := b.VID
	if visitor == "" {
		visitor = anonVisitor(ipKey, ua)
	}
	hdr := map[string]string{}
	hdrLen := len("{}")
	for _, name := range browserHeaders {
		if vals := r.Header.Values(name); len(vals) > 0 {
			v := truncate(strings.Join(vals, ", "), maxHeader)
			// Kept in the list's order while the encoded whole stays within its bound.
			if n := len(encodeJSON(name)) + len(encodeJSON(v)) + len(":,"); hdrLen+n <= maxHeaders {
				hdr[name] = v
				hdrLen += n
			}
		}
	}
	refHost := hostOf(ref)
	if strings.EqualFold(refHost, site.Domain) {
		refHost = ""
	}
	out := make([]store.Event, 0, len(b.Events))
	stored := 0
	for i, e := range b.Events {
		if !browserKinds[e.K] {
			return nil, 0, fmt.Errorf("kind %q", e.K)
		}
		if len(e.P) > maxProps {
			return nil, 0, errTooLarge
		}
		if err := checkString(e.URL, maxURL); err != nil {
			return nil, 0, err
		}
		if err := checkString(e.Title, maxString); err != nil {
			return nil, 0, err
		}
		if err := checkString(e.N, maxString); err != nil {
			return nil, 0, err
		}
		if e.K == "custom" && e.N == "" {
			return nil, 0, errors.New("custom event without a name")
		}
		var p map[string]any
		if len(e.P) > 0 && string(e.P) != "null" {
			if err := json.Unmarshal(e.P, &p); err != nil {
				return nil, 0, err
			}
			if err := checkValue("p", p, 0); err != nil {
				return nil, 0, err
			}
		}
		evURL := pageURL
		if e.URL != "" {
			evURL = e.URL
		}
		u, err := url.Parse(evURL)
		if evURL != "" && (err != nil || !strings.EqualFold(u.Hostname(), site.Domain)) {
			return nil, 0, errors.New("url outside the site")
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
		ev := store.Event{
			SiteID: site.ID, TS: eventTime(recv, b.Sent, e.T), Source: "browser", Kind: e.K, Name: eventName(e, p),
			Visitor: visitor, SID: b.SID, IP: addr.String(), IPKey: ipKey, UA: ua, Path: path,
			Template: site.Template(path), URL: evURL, Props: encodeJSON(props),
		}
		if e.K == "page_view" || e.K == "session_start" {
			// The page's own URL and referrer are kept whole (campaign tags live in the query); the feed's request
			// paths are not, since an API query string can carry a key.
			ev.Ref, ev.RefHost = ref, refHost
		}
		if ev.Size() > maxEventBytes {
			return nil, 0, errTooLarge
		}
		// The context and the browser headers once per batch: they are the same for every event in it.
		if i == 0 {
			props["ctx"] = json.RawMessage(ctxJSON)
			if len(hdr) > 0 {
				props["hdr"] = hdr
			}
			ev.Props = encodeJSON(props)
		}
		stored += ev.Size()
		if stored > maxBatchBytes {
			return nil, 0, errTooLarge
		}
		out = append(out, ev)
	}
	return out, stored, nil
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
			return truncate(h, 200)
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
		// A key no site has names no site the collector can trust (the body is never read): counted with the
		// collector-wide refusals every footer shows, beside the unknown domains.
		c.count(0, func(s *store.IngestStat) { s.UnknownDomain++ })
		apix.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	fail := func(status int, msg string) {
		c.count(site.ID, func(s *store.IngestStat) { s.Rejected++ })
		apix.Error(w, status, msg)
	}
	limitKey := strconv.FormatInt(site.ID, 10)
	// A key without a token left is refused before its body is read or parsed.
	if !c.Feed.Permitted(limitKey) {
		c.count(site.ID, func(s *store.IngestStat) { s.RateLimited++ })
		apix.TooMany(w, c.Feed.RetryAfter(limitKey))
		return
	}
	body, status := readBody(w, r, MaxFeedBody, c.bodyTimeout(feedBodyTimeout))
	switch status {
	case 0:
	case http.StatusRequestEntityTooLarge:
		fail(status, "request body too large")
		return
	case http.StatusRequestTimeout:
		fail(status, "request body not received in time")
		return
	default:
		fail(http.StatusBadRequest, "malformed request")
		return
	}
	var b feedBatch
	if json.Unmarshal(body, &b) != nil {
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
	// One event's content never refuses the batch: the sender is a trusted backend, but the fields are its public
	// clients' (a crafted method or user agent would otherwise blind the feed). Each event is decoded on its own;
	// unusable ones (a wrong JSON type included) are dropped and counted as rejected, the rest is taken.
	evs, dropped := c.feedEvents(site, b.Events)
	if ok, wait := c.Feed.Take(limitKey, float64(len(evs)), 0); !ok {
		c.count(site.ID, func(s *store.IngestStat) { s.RateLimited += int64(len(evs)) })
		apix.TooMany(w, wait)
		return
	}
	// Counted once the batch is taken: a refused one comes again, its dropped events with it.
	if dropped > 0 {
		c.count(site.ID, func(s *store.IngestStat) { s.Rejected += int64(dropped) })
	}
	c.accept(site.ID, "server", evs)
	w.WriteHeader(http.StatusNoContent)
}

// feedBatch is the envelope; its events are decoded one at a time (feedEvents).
type feedBatch struct {
	V      int               `json:"v"`
	Site   string            `json:"site"`
	Events []json.RawMessage `json:"events"`
}

type feedEvent struct {
	T      int64  `json:"t"`
	IP     string `json:"ip"`
	UA     string `json:"ua"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Route  string `json:"route"`
	Status int    `json:"status"`
	DurMS  int64  `json:"dur_ms"`
	Bytes  int64  `json:"bytes"`
	Ref    string `json:"ref"`
	Lang   string `json:"lang"`
	SFSite string `json:"sf_site"`
	SFMode string `json:"sf_mode"`
	SFDest string `json:"sf_dest"`
	ChUA   string `json:"ch_ua"`
	// SigAgent is the Signature-Agent header (Web Bot Auth): the agent a client says it is.
	SigAgent string `json:"sig_agent"`
	Account  string `json:"account"`
}

// Per-field limits of a feed event in bytes (README, "Server feed"): longer values are clipped, never refused.
const (
	feedMethod   = 16
	feedRoute    = 200
	feedSF       = 32
	feedSigAgent = 256
	feedAccount  = 128
)

// feedEvents turns a feed batch into rows: strings clipped to their limits, events without a usable address or
// with impossible numbers dropped (the count is returned).
func (c *Collector) feedEvents(site *sites.Site, in []json.RawMessage) ([]store.Event, int) {
	now := c.now().UnixMilli()
	out := make([]store.Event, 0, len(in))
	dropped := 0
	for _, raw := range in {
		var e feedEvent
		// An event that is no object or has a field of the wrong type is unusable: decoding goes on past a type
		// error, so a half-filled event must not be kept.
		if json.Unmarshal(raw, &e) != nil {
			dropped++
			continue
		}
		for _, f := range []struct {
			v     *string
			limit int
		}{{&e.UA, maxString}, {&e.Method, feedMethod}, {&e.Path, maxURL}, {&e.Route, feedRoute}, {&e.Ref, maxURL},
			{&e.Lang, maxString}, {&e.SFSite, feedSF}, {&e.SFMode, feedSF}, {&e.SFDest, feedSF},
			{&e.ChUA, maxString}, {&e.SigAgent, feedSigAgent}, {&e.Account, feedAccount}} {
			*f.v = truncate(*f.v, f.limit)
		}
		addr, ok := storedAddr(e.IP)
		if !ok || e.Status < 0 || e.Status > 999 || e.DurMS < 0 || e.Bytes < 0 {
			dropped++
			continue
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
			"sf_dest": e.SFDest, "ch_ua": e.ChUA, "sig_agent": e.SigAgent} {
			if v != "" {
				props[k] = v
			}
		}
		pj := encodeJSON(props)
		ipKey := ratelimit.Key(addr.String())
		refHost := hostOf(e.Ref)
		if strings.EqualFold(refHost, site.Domain) {
			refHost = ""
		}
		out = append(out, store.Event{
			SiteID: site.ID, TS: ts, Source: "server", Kind: "request", Name: e.SFDest,
			Visitor: anonVisitor(ipKey, e.UA), IP: addr.String(), IPKey: ipKey, UA: e.UA, Path: path,
			Template: template, Ref: stripQuery(e.Ref), RefHost: refHost, Method: method, Status: e.Status,
			DurMS: e.DurMS, Account: e.Account, Props: pj,
		})
	}
	return out, dropped
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

// truncate cuts s to at most n bytes on a rune boundary: a clipped string stays valid UTF-8.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
