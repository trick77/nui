// Package insight holds what the dashboard computes for the model and what the model answers: facts with stable
// ids, the briefing that picks and words them, and the reading of one actor. Go computes every number; the model
// only selects facts by id and writes short text, so nothing here carries a number the model made.
package insight

import (
	"context"
	"time"
)

// Fact is one computed finding. ID is stable across recomputations of the same finding (its type, key and what a
// line about it may not contradict, such as a trend's direction), so a cached briefing written for one set of ids
// still fits after the numbers moved, and never once a direction flipped.
type Fact struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"` // problem, automation, people
	Figure string   `json:"figure"`
	Unit   string   `json:"unit"`
	Text   string   `json:"text"`
	Detail []string `json:"detail"`
	Target Target   `json:"target"`
	// Score orders facts without a briefing, most important first; At (unix ms, 0 none) those of one score, the
	// latest first.
	Score float64 `json:"-"`
	At    int64   `json:"-"`
	// What an alert rule filters by, beside the id (internal: neither the model nor the briefing cache sees them):
	// Value the figure (a count, a tile's value), Change a trend's percent or points, Share the part of all requests
	// (0 to 1) and Growth the factor over the comparison (+Inf from none) of a spike, Volume the calls a failed share
	// is of in the smaller of both periods, Visitors the distinct visitors (addresses on the server) a new error hit.
	Value, Change, Share, Growth, Volume, Visitors float64 `json:"-"`
	// Server: a new error the server answered (5xx, its key the endpoint), not the script's.
	Server bool `json:"-"`
}

// Target is where a finding's row jumps to.
type Target struct {
	Panel  string            `json:"panel"`
	Filter map[string]string `json:"filter"`
}

// TileFact is one overview figure as the briefing is told it, already formatted.
type TileFact struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Value  string `json:"value"`
	Prev   string `json:"prev"`
	Change string `json:"change"`
}

// BriefingInput is everything one briefing is written from: a few KB.
type BriefingInput struct {
	SiteID    int64
	Domain    string
	Range     string
	PrevLabel string
	AsOf      time.Time
	Tiles     []TileFact
	Facts     []Fact
	// Hash identifies the set of fact ids: a briefing is shown only for the set it was written for.
	Hash string
}

// Briefing is the model's answer, validated: every id is one of the input's facts.
type Briefing struct {
	Headline string      `json:"headline"`
	Lines    []BriefLine `json:"lines"`
	AsOf     time.Time   `json:"as_of"`
}

// BriefLine is one line: the facts it covers (related ones merged) and its wording.
type BriefLine struct {
	IDs  []string `json:"ids"`
	Text string   `json:"text"`
}

// Evidence is one computed statement about an actor the reading may cite by id. The id is stable for what the
// statement says (requests:refused-most, pace:fast), so a reading cached for one actor resolves against another
// actor's own statement, with that actor's figures, or not at all.
type Evidence struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// ReadingInput is one actor's behaviour as the model is shown it: the collapsed trail (at most about 50 lines),
// the signals and the evidence it may cite. No address, network name or date: the reading is cached by Signature
// and shown for every actor that behaves alike.
type ReadingInput struct {
	SiteID      int64
	Key         string
	Signature   string
	Class       string
	Client      string
	NetworkKind string
	Signals     []string
	Evidence    []Evidence
	Trail       []string
	// Queue asks for a reading when none is cached; without it the cache only answers.
	Queue bool
}

// Verdicts are the readings' fixed choices, each with what it means (the model is told both).
var Verdicts = []string{
	"Person", "Search crawler", "Uptime check", "Feed reader", "API client", "Scraping attempt",
	"Vulnerability scan", "Credential probing", "Unclear",
}

// VerdictMeanings say what each verdict is for.
var VerdictMeanings = map[string]string{
	"Person":             "a human using a browser: pages, their assets, the script's beacon, irregular pace",
	"Search crawler":     "a search engine or SEO crawler fetching pages to index them",
	"Uptime check":       "a monitor fetching the same page or two at a fixed interval to see whether the site is up",
	"Feed reader":        "fetches feeds or a few pages on a schedule for a reader or preview app",
	"API client":         "a program using the site's API as intended: valid requests that are answered",
	"Scraping attempt":   "walks many pages or API entries to collect the data, often fast, often refused or limited",
	"Vulnerability scan": "requests paths the site does not have (admin panels, .env, .git, wp-login) to find weaknesses",
	"Credential probing": "repeats requests that need a key or a login and are refused, trying keys or passwords",
	"Unclear":            "none of the above fits the evidence",
}

// Suspicious is whether a verdict is one the dashboard flags.
func Suspicious(verdict string) bool {
	switch verdict {
	case "Scraping attempt", "Vulnerability scan", "Credential probing":
		return true
	}
	return false
}

// Reading is the model's validated reading of a behaviour signature: no figure, address, network name or date of
// its own, so it fits every actor with the signature. The dashboard resolves the evidence ids against the actor
// shown and leaves the reading out when one of them does not exist there.
type Reading struct {
	Verdict  string   `json:"verdict"`
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence"` // evidence ids
}

// Model is the background writer of briefings, readings and labels. Every method answers from the cache at once
// and only queues work; none waits for the model. A nil Model, or one not configured, answers nothing.
type Model interface {
	Configured() bool
	// Briefing is the cached briefing for the input's site and range, nil when none; it queues a new one when the
	// cadence allows and the facts changed.
	Briefing(ctx context.Context, in BriefingInput) *Briefing
	// Readings are the cached readings of the inputs' signatures, keyed by actor key; missing ones are queued for
	// the inputs that ask (Queue), urgent ones first.
	Readings(ctx context.Context, ins []ReadingInput, urgent bool) map[string]Reading
	// Labels are the stored labels of values ("path" templates, "ua" strings); missing ones are queued.
	Labels(ctx context.Context, site int64, kind string, values []string) map[string]string
	// Usage is a site's calls today, the daily call budget and whether the day's budget (calls or tokens) is spent.
	Usage(ctx context.Context, site int64) (calls, budget int64, spent bool)
}
