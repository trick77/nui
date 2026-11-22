package store

// Class codes of event.cls. Pending events wait for the classifier's next pass; the dashboard counts them with the
// humans until then (the pass runs every few minutes).
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
	TS       int64 // unix ms
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

// IngestStat is one site's collector counters for one hour.
//
// Table: ingest_stat
type IngestStat struct {
	SiteID        int64
	Hour          int64
	Accepted      int64
	RateLimited   int64
	Rejected      int64
	Dropped       int64
	UnknownDomain int64
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
	BasisFrom int64
	BasisTo   int64
	CreatedAt int64
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
