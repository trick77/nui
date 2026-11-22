package sim

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/netip"
	"time"
)

// record is one request a product's backend served, as its feed reports it.
type record struct {
	t                       time.Time
	ip                      netip.Addr
	ua, method, path, route string
	status                  int
	dur, bytes              int64
	ref, lang               string
	sfSite, sfMode, sfDest  string
	chUA, account           string
}

// event is one tracker event.
type event struct {
	t          time.Time
	k, n       string
	p          map[string]any
	url, title string
}

// client is one browser (or a script posing as one) sending tracker batches.
type client struct {
	addr netip.Addr
	ua   string
	hdr  map[string]string
}

// batch is one tracker batch: a session's events and the page context they were sent from.
type batch struct {
	at       time.Time
	site     int
	client   *client
	vid, sid string
	ctx      map[string]any
	events   []event
	actor    string
	// hammer: the sender ignores 429 and sends on.
	hammer bool
}

// Request is one POST a simulated client makes: a tracker batch to /api/in or a feed batch to /api/feed.
type Request struct {
	// At is when it is sent, in the world's time.
	At   time.Time
	Site *Site
	// Feed: a feed batch; otherwise a tracker batch.
	Feed bool
	// Actor names who sent it (feed: the product's emitter).
	Actor   string
	batch   *batch
	records []record
	// Tries counts the sends: a refused request is sent again after its Retry-After.
	Tries int
	seq   uint64
}

// Path is the endpoint the request goes to.
func (q *Request) Path() string {
	if q.Feed {
		return "/api/feed"
	}
	return "/api/in"
}

// Events is how many events the request carries.
func (q *Request) Events() int {
	if q.Feed {
		return len(q.records)
	}
	return len(q.batch.events)
}

// Hammer reports whether the sender ignores 429.
func (q *Request) Hammer() bool { return q.batch != nil && q.batch.hammer }

// Addr is the address the request comes from: the browser's, or for a feed batch the product's backend (any).
func (q *Request) Addr() netip.Addr {
	if q.Feed {
		return netip.MustParseAddr("192.0.2.10")
	}
	return q.batch.client.addr
}

// Clock maps the world's time to the milliseconds a request states: the identity for a backfill, the wall clock for
// a live run whose world time runs faster.
type Clock func(time.Time) int64

// Body is the request's JSON, its times stated through clock.
func (q *Request) Body(clock Clock) []byte {
	if clock == nil {
		clock = func(t time.Time) int64 { return t.UnixMilli() }
	}
	var v any
	if q.Feed {
		evs := make([]map[string]any, 0, len(q.records))
		for _, r := range q.records {
			e := map[string]any{"t": clock(r.t), "ip": r.ip.String(), "ua": r.ua, "method": r.method, "path": r.path,
				"status": r.status, "dur_ms": r.dur, "bytes": r.bytes}
			for k, s := range map[string]string{"route": r.route, "ref": r.ref, "lang": r.lang, "sf_site": r.sfSite,
				"sf_mode": r.sfMode, "sf_dest": r.sfDest, "ch_ua": r.chUA, "account": r.account} {
				if s != "" {
					e[k] = s
				}
			}
			evs = append(evs, e)
		}
		v = map[string]any{"v": 1, "site": q.Site.Domain, "events": evs}
	} else {
		b := q.batch
		evs := make([]map[string]any, 0, len(b.events))
		for _, e := range b.events {
			m := map[string]any{"t": clock(e.t), "k": e.k, "p": nonNilMap(e.p)}
			if e.n != "" {
				m["n"] = e.n
			}
			if e.url != "" {
				m["url"], m["title"] = e.url, e.title
			}
			evs = append(evs, m)
		}
		v = map[string]any{"v": 1, "site": q.Site.Domain, "vid": b.vid, "sid": b.sid, "sent": clock(q.At),
			"ctx": b.ctx, "events": evs}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// Header is what the client sends besides the body: the browser's user agent, Origin, client hints and fetch
// metadata, or the feed's bearer key.
func (q *Request) Header() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if q.Feed {
		h.Set("Authorization", "Bearer "+q.Site.FeedKey)
		h.Set("User-Agent", "example-lens-feed/1.3")
		return h
	}
	// fetch() with a string body, as the tracker sends it.
	h.Set("Content-Type", "text/plain;charset=UTF-8")
	h.Set("User-Agent", q.batch.client.ua)
	h.Set("Origin", "https://"+q.Site.Domain)
	for k, v := range q.batch.client.hdr {
		h.Set(k, v)
	}
	return h
}

func nonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
