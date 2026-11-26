package dash

// bucketStats is one bucket's (or one of its slots') overview figures, sums over events.
//
// Table: -
type bucketStats struct {
	missing  bool // part of it holds no figures (plan.older)
	gap      bool // part of it was read as nothing (plan.gaps)
	visitors uniq // visitor ids (aud bots: address keys)
	views    float64
	starts   float64 // session starts: visits
	events   float64
	requests float64
	unknown  float64 // unknown automated requests, over all requests whatever aud
	allReq   float64
	errors   float64
	api      apiStats // a site of kind api only
}

// live counts the address keys seen in the last five minutes, people apart from the automated.
//
// Table: -
type live struct {
	Humans    int64 `json:"humans"`
	Automated int64 `json:"automated"`
}

// Collected are the collector's counts of a site (0: the requests without one) over whole hours, the hour not
// flushed yet included.
//
// Table: ingest_stat
type Collected struct {
	// Refused are the events refused (the rate) and the requests refused whole (rejected); Dropped the events lost
	// after they were taken (a full queue, a failed write); UnknownDomain the requests naming no configured site.
	Refused, Dropped, UnknownDomain int64
}

// dayState is one agg_day row as the dashboard needs it.
//
// Table: agg_day
type dayState struct {
	frozen, clean, raw bool
	builder            int64
}

// classInfo is one address key's stored class and its reasons for a day.
//
// Table: actor_class
type classInfo struct {
	class   string
	signals []string
}

// actor is one address key's events summed over a range.
//
// Table: event
type actor struct {
	key                     string
	visits, views, requests int64
	refused, events         int64
	first, last             int64 // first is only read for one actor (the visitor detail)
	ua                      string
	asn                     int64
	country                 string
	cls                     int
	browser                 bool
	// human: an event of the key is stamped human (a beacon is at once); its pending events are then a person's.
	human bool
}
