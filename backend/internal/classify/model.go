package classify

// uaDay is one user agent's part of an actor's day, sums over its events: the pass weighs every client of a shared
// address on its own.
//
// Table: event
type uaDay struct {
	ua              string
	n               int64
	browser, server int64
	// docs are the page loads Sec-Fetch-Dest names; pages adds those of a client without it (a crawler).
	docs, pages, assets int64
	// delivered are the pages answered 200 or 304: only those carry the script. authed are the requests answered with
	// the product's account on them (signedRequest).
	delivered, authed int64
	// beaconElsewhere: a beacon with the client's user agent for one of its pages came from another address key
	// (dual stack, a relay that leaves per host).
	beaconElsewhere   bool
	refused           int64
	webdriver         bool
	noFetch, noLang   int64
	noSite            int64
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
