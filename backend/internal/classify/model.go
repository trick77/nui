package classify

// actorDay is one actor's day as the pass weighs it, sums over its events.
//
// Table: event
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
