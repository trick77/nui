// Package insight holds what the dashboard computes for the model and what the model answers: facts with stable
// ids, the briefing that picks and words them, and the reading of one actor. Go computes every number; the model
// only selects facts by id and writes short text, so nothing here carries a number the model made.
package insight

import (
	"context"
	"time"
)

// Fact is one computed finding. ID is stable across recomputations of the same finding (its type and key), so a
// cached briefing still points at the right fact after the numbers moved.
type Fact struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"` // problem, automation, people
	Figure string   `json:"figure"`
	Unit   string   `json:"unit"`
	Text   string   `json:"text"`
	Detail []string `json:"detail"`
	Target Target   `json:"target"`
	// Score orders facts without a briefing, most important first.
	Score float64 `json:"-"`
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
	// Hash identifies the facts: a briefing is regenerated only when it changed.
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

// Evidence is one computed statement about an actor the reading may cite by id.
type Evidence struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// ReadingInput is one actor's behaviour as the model is shown it: the collapsed trail (at most about 50 lines),
// the signals and the evidence it may cite.
type ReadingInput struct {
	SiteID    int64
	Key       string
	Signature string
	Class     string
	Client    string
	Network   string
	Signals   []string
	Evidence  []Evidence
	Trail     []string
	From, To  time.Time
}

// Verdicts are the readings' fixed choices.
var Verdicts = []string{
	"Person", "Search crawler", "Uptime check", "Feed reader", "API client", "Scraping attempt",
	"Vulnerability scan", "Credential probing", "Unclear",
}

// Suspicious is whether a verdict is one the dashboard flags.
func Suspicious(verdict string) bool {
	switch verdict {
	case "Scraping attempt", "Vulnerability scan", "Credential probing":
		return true
	}
	return false
}

// Reading is the model's validated reading of a behaviour signature.
type Reading struct {
	Verdict  string    `json:"verdict"`
	Summary  string    `json:"summary"`
	Evidence []string  `json:"evidence"` // evidence ids
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
}

// Model is the background writer of briefings, readings and labels. Every method answers from the cache at once
// and only queues work; none waits for the model. A nil Model, or one not configured, answers nothing.
type Model interface {
	Configured() bool
	// Briefing is the cached briefing for the input's site and range, nil when none; it queues a new one when the
	// cadence allows and the facts changed.
	Briefing(ctx context.Context, in BriefingInput) *Briefing
	// Readings are the cached readings of the inputs' signatures, keyed by actor key; missing ones are queued.
	Readings(ctx context.Context, ins []ReadingInput, urgent bool) map[string]Reading
	// Labels are the stored labels of values ("path" templates, "ua" strings); missing ones are queued.
	Labels(ctx context.Context, site int64, kind string, values []string) map[string]string
	// Usage is a site's calls today and the daily call budget.
	Usage(ctx context.Context, site int64) (calls, budget int64)
}
