package classify

// uaDay is one user agent's part of an actor's day, sums over its events: the pass weighs every client of a shared
// address on its own.
//
// Table: event
type uaDay struct {
	ua                string
	n                 int64
	browser, server   int64
	docs, assets      int64
	refused           int64
	webdriver         bool
	noFetch, noLang   int64
	engaged, pageView int64
	// first and last are the server requests' first and last time (unix ms), 0 without any.
	first, last int64
}

// actorKey is one actor-day of a site: a pass's unit of work, and what an rDNS answer still pending marks.
//
// Table: event
type actorKey struct {
	site int64
	key  string
	day  int64
}

// actorDay is one actor's day as the pass weighs it: its address, network and its most frequent user agents.
//
// Table: event
type actorDay struct {
	key, ip string
	asn     int64
	uas     []uaDay
}
