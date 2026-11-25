package watch

// Site is the walk's state of one site: its cursor and what a day is judged against.
//
// Table: watch_site
type Site struct {
	SiteID int64
	// FromDay is the first day walked (unix day), DoneDay the last closed day merged.
	FromDay, DoneDay int64
	// ReadyAt is when the walk first caught up (unix ms); 0 while it backfills, which finds nothing.
	ReadyAt int64
	// ScriptsFrom is the first day a page reported its script hosts (0: none yet): the learning window starts there.
	ScriptsFrom int64
	// Cut counts the hosts the caps left out, each once a day it was.
	Cut int64
}

// Host is one host a site's pages loaded scripts from.
//
// Table: script_host
type Host struct {
	Host      string `json:"host"`
	FirstSeen int64  `json:"first_seen"`
	LastSeen  int64  `json:"last_seen"`
	// Addrs is the most distinct address keys of one day that reported it: a visitor id is the browser's word. Nets
	// is the most distinct networks of one day (IPv4 /24, IPv6 /48): what makes it new.
	Addrs int64 `json:"addrs"`
	Nets  int64 `json:"nets"`
	// FoundAt is when the job found the host new (0 never): the bookkeeping's time. FoundFirst is its first sight on
	// the day it passed as new: the finding's time, the same before and after that day closes (set without FoundAt:
	// past its day's bound, found on a later day).
	FoundAt    int64 `json:"found_at"`
	FoundFirst int64 `json:"found_first"`
}

// Open is one provisional finding of a day not closed yet (kind script): its host, its first sight that day and its
// state as of the pass (Detail, JSON of a Host). Kind host is no finding: a script host the open days reported, its
// state merged with theirs.
//
// Table: watch_open
type Open struct {
	Kind      string
	Key       string
	FirstSeen int64
	Detail    string
}
