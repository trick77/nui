package dash

// bucketStats is one bucket's overview figures, sums over events.
//
// Table: -
type bucketStats struct {
	visitors map[string]struct{} // visitor ids (aud bots: address keys)
	views    float64
	events   float64
	requests float64
	unknown  float64 // unknown automated requests, over all requests whatever aud
	allReq   float64
	errors   float64
}

// live counts the address keys seen in the last five minutes, people apart from the automated.
//
// Table: -
type live struct {
	Humans    int64 `json:"humans"`
	Automated int64 `json:"automated"`
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
