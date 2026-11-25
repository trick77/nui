package watch

// Site is the walk's state of one site: its cursor and what a day is judged against.
//
// Table: watch_site
type Site struct {
	SiteID int64
	// FromDay is the first day walked (unix day): the learning window starts there. DoneDay is the last closed day
	// merged.
	FromDay, DoneDay int64
	// ReadyAt is when the walk first caught up (unix ms); 0 while it backfills, which finds nothing.
	ReadyAt int64
	// RoutedDay is the last day a server row named a route, SoftDay the last soft day (0: none).
	RoutedDay, SoftDay int64
	// ShellBytes is the last shell size a day showed (0: none).
	ShellBytes int64
	// ScriptsFrom is the first day a page reported its script hosts (0: none yet).
	ScriptsFrom int64
	// Cut counts the templates and hosts the caps left out, each once a day it was.
	Cut int64
}

// Path is one request template's state: its refusals, its answers, and the finding its first answer made. Times are
// unix ms, sizes bytes (0: none reported), addrs the most distinct address keys of one day.
//
// Table: path_state
type Path struct {
	Template     string `json:"template"`
	RefusedFirst int64  `json:"refused_first"`
	RefusedLast  int64  `json:"refused_last"`
	RefusedAddrs int64  `json:"refused_addrs"`
	// RefusedSizes are the refusals' distinct sizes, ascending, at most maxRefusedSizes (none: never reported).
	RefusedSizes []int64 `json:"refused_sizes"`
	OKFirst      int64   `json:"ok_first"`
	OKLast       int64   `json:"ok_last"`
	OKAddrs      int64   `json:"ok_addrs"`
	OKLo         int64   `json:"ok_lo"`
	OKHi         int64   `json:"ok_hi"`
	// PlainFirst is the first answer to an address not mostly refused that day; HitFirst and HitAddr the first to one
	// that was.
	PlainFirst int64  `json:"plain_first"`
	HitFirst   int64  `json:"hit_first"`
	HitAddr    string `json:"hit_addr"`
	// Found is the finding the first answer made (flip, hit, endpoint; "" none), FoundAt when the job made it (0 with
	// Found set: past its day's bound, made on a later day).
	Found   string `json:"found"`
	FoundAt int64  `json:"found_at"`
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

// Open is one provisional finding of a day not closed yet: its kind, template or host, when it was first answered
// or seen, and its state as of the pass (Detail, JSON of a Path or a Host). Kind host is no finding: a script host
// the open days reported, its state merged with theirs.
//
// Table: watch_open
type Open struct {
	Kind      string
	Key       string
	FirstSeen int64
	Detail    string
}
