package store

// Class codes of event.cls. Pending events wait for the classifier's next pass (every few minutes); the dashboard
// counts them as unknown automated until then, a person being shown, never assumed. A key with a human event (a
// beacon is human at once) counts its pending events as the person's where a figure is per key (live, the visitor
// table).
const (
	ClsPending = 0
	ClsHuman   = 1
	ClsBot     = 2
	ClsMonitor = 3
	ClsUnknown = 4
)

// ClassNames maps an event.cls code to the class name the API and actor_class use.
var ClassNames = map[int]string{ClsHuman: "human", ClsBot: "bot", ClsMonitor: "monitor", ClsUnknown: "unknown"}

// ClassCode is the event.cls code of a class name, ClsPending for an unknown one.
func ClassCode(name string) int {
	for c, n := range ClassNames {
		if n == name {
			return c
		}
	}
	return ClsPending
}

// Site is one configured domain as synced from the sites file.
//
// Table: site
type Site struct {
	ID          int64
	Domain      string
	Kind        string
	Product     string
	FeedKeyHash string
	PathRules   string
	Active      bool
}

// Event is one row the batch writer inserts: a browser event or a server request.
//
// Table: event
type Event struct {
	SiteID   int64
	TS       int64 // unix ms: when it happened, bounded by the receive time (0001_init.sql says how)
	Source   string
	Kind     string
	Name     string
	Visitor  string
	SID      string
	IP       string
	IPKey    string
	UA       string
	Path     string
	Template string
	URL      string
	Ref      string
	RefHost  string
	Method   string
	Status   int
	DurMS    int64
	ASN      int64
	Country  string
	Account  string
	Cls      int
	Props    string
}

// rowOverhead approximates what a row takes beyond its text: the integer columns, the record header and the two
// index entries (each holds site_id and ts, the actor index ip_key too, counted with the text).
const rowOverhead = 96

// Size is about the bytes the row takes on disk: its text plus a fixed overhead. The collector bounds and charges
// by it, the batch writer's queue counts it.
func (e *Event) Size() int {
	return rowOverhead + len(e.Source) + len(e.Kind) + len(e.Name) + len(e.Visitor) + len(e.SID) + len(e.IP) +
		2*len(e.IPKey) + len(e.UA) + len(e.Path) + len(e.Template) + len(e.URL) + len(e.Ref) + len(e.RefHost) +
		len(e.Method) + len(e.Country) + len(e.Account) + len(e.Props)
}

// IngestStat is one site's collector counters for one hour.
//
// Table: ingest_stat
type IngestStat struct {
	SiteID         int64
	Hour           int64
	Accepted       int64
	AcceptedServer int64
	RateLimited    int64
	Rejected       int64
	Dropped        int64
	UnknownDomain  int64
}

// ActorClass is one actor's class on one day, with its reasons.
//
// Table: actor_class
type ActorClass struct {
	SiteID  int64
	IPKey   string
	Day     int64
	Class   string
	Reasons []string
}

// Briefing is the model's cached briefing for a site and range.
//
// Table: llm_briefing
type Briefing struct {
	SiteID    int64
	Range     string
	FactsHash string
	AsOf      int64
	Body      string
	CreatedAt int64
}

// Reading is the model's cached reading of a behaviour signature.
//
// Table: llm_reading
type Reading struct {
	Signature string
	Body      string
	CreatedAt int64
}

// AggDay is one site-day's aggregate build state: the dashboard reads the day's aggregates only while it is frozen
// and clean, raw otherwise.
//
// Table: agg_day
type AggDay struct {
	SiteID  int64
	Day     int64 // unix day, UTC
	Frozen  bool
	Dirty   int   // AggClean, AggRawChanged, AggRequeued or AggWriting
	Version int64 // raw changes counted, the build's handshake
	Builder int64
	Raw     bool
	Events  int64
	BuiltAt int64
	// AddrsCut: the size cap deleted the day's per-address rows, all of them in the transaction that set it.
	AddrsCut bool
}

// AggHour is one site's sums and sketches for one UTC hour and class.
//
// Table: agg_hour
type AggHour struct {
	SiteID      int64
	Hour        int64 // unix hour
	Cls         int   // 1-4, pending as 4
	Events      int64
	Views       int64
	Actions     int64
	Starts      int64
	Requests    int64
	Refused     int64
	Errors      int64
	BrowserIDs  []byte
	Addrs       []byte
	ServerAddrs []byte
}

// AggList is one list entry, or a part's Other row, for one site, dimension and UTC day.
//
// Table: agg_list
type AggList struct {
	SiteID  int64
	Dim     string
	Day     int64
	Part    int // 1 people, 2 automated
	Other   bool
	Parent  string
	Key     string
	Sub     string
	A       int64
	B       int64
	N       int64
	AddrN   int64 // Other, part 2: its entries' address counts summed (the bots audience's N)
	IDs     []byte
	Addrs   []byte
	Entries int64
	Keys    []byte // Other: the collapsed keys' sorted 64-bit hashes, big-endian; nil past 50,000
}

// AggAddr is one address key's sums for one site, UTC day and part.
//
// Table: agg_addr
type AggAddr struct {
	SiteID   int64
	Day      int64
	IPKey    string
	Part     int
	Events   int64
	Views    int64
	Actions  int64
	Requests int64
	Refused  int64
	Sessions int64
	Carried  int64 // sessions the key also had the day before, in the same part
	Browser  bool
	Human    bool
	FirstTS  int64
	LastTS   int64
	UA       string
	ASN      int64
	Country  string
	Cls      int
}

// AggAddrKey is an agg_addr row's key within its site-day.
//
// Table: agg_addr
type AggAddrKey struct {
	IPKey string
	Part  int
}

// AggCarried is one key's carried sessions a rebuild saved before rewriting a day whose day before has no raw
// (IPKey "" and Part 0: the copy is whole).
//
// Table: agg_carried
type AggCarried struct {
	SiteID  int64
	Day     int64
	IPKey   string
	Part    int
	Carried int64
}

// AggMeta is one marker of the background jobs: raw_purge_armed_at, the first start of the release that purges raw.
//
// Table: agg_meta
type AggMeta struct {
	Key   string
	Value string
}

// Usage is one site's model calls and tokens on one day.
//
// Table: llm_usage
type Usage struct {
	SiteID       int64
	Day          int64
	Calls        int64
	InputTokens  int64
	OutputTokens int64
}
