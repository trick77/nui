// Package alert is what Trakk reports about the traffic it watches and about itself: the fixed catalogue of
// triggers, the rules an operator picks from it (notify.json), the conditions the background jobs report and the
// plain text a message is made of. A leaf, the standard library only: every job can take a Reporter, and the store,
// the dashboard and the dispatcher all import it.
package alert

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// Severity orders alerts: a channel takes those at or above its min_severity, a message is titled with its highest.
type Severity int

// The severities, low to high; the zero value is none.
const (
	Low Severity = iota + 1
	Normal
	High
)

var severityNames = map[Severity]string{Low: "low", Normal: "normal", High: "high"}

func (s Severity) String() string { return severityNames[s] }

// ParseSeverity reads low, normal or high.
func ParseSeverity(s string) (Severity, bool) {
	for v, n := range severityNames {
		if n == s {
			return v, true
		}
	}
	return 0, false
}

// The jobs that report job.failed, by the names a rule's jobs parameter takes.
const (
	JobAgg       = "agg"       // the aggregate builder
	JobRetention = "retention" // age retention, the size cap, the vacuum
	JobPurge     = "purge"     // the raw purge
	JobRefdata   = "refdata"   // the reference data refresh
	JobClassify  = "classify"  // the classifier's pass
	JobWriter    = "writer"    // the event writer
)

// Jobs are the job names, in a fixed order.
var Jobs = []string{JobAgg, JobRetention, JobPurge, JobRefdata, JobClassify, JobWriter}

// The instance's own condition kinds the jobs report (their catalogue ids).
const (
	KindJobFailed = "job.failed"
	KindSizeCap   = "db.size_cap"
)

// Condition is one state a job reports: open while it holds, closed (Open false) once the job got through.
type Condition struct {
	Kind string // KindJobFailed or KindSizeCap
	Key  string // the job, "" for the size cap
	Open bool
	// Text says what happened, plain; it is defused before it is shown or sent.
	Text string
	At   time.Time
}

// Reporter takes the jobs' conditions. It never blocks and never fails the job: the writer reports a failed write,
// and the database that refused it is no place to keep that.
type Reporter interface {
	Report(c Condition)
}

// Note reports c to r, nothing when r is nil (a job without one: tests, the simulator's own runs).
func Note(r Reporter, c Condition) {
	if r != nil {
		r.Report(c)
	}
}

// Fail reports a job's run: failed with err's text, through when err is nil. A cancelled run says nothing: the
// shutdown is no failure, nor a success.
func Fail(r Reporter, job string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	c := Condition{Kind: KindJobFailed, Key: job, Open: err != nil}
	if err != nil {
		c.Text = err.Error()
	}
	Note(r, c)
}

// Board holds the conditions reported since start, in memory: the dispatcher's step reads them. One not reported
// since start is unknown, never closed: a restart is no success.
type Board struct {
	mu  sync.Mutex
	m   map[[2]string]Condition
	now func() time.Time
}

// NewBoard returns an empty board on now (time.Now when nil).
func NewBoard(now func() time.Time) *Board {
	if now == nil {
		now = time.Now
	}
	return &Board{m: map[[2]string]Condition{}, now: now}
}

// Report records c, stamped now when it has no time.
func (b *Board) Report(c Condition) {
	if c.At.IsZero() {
		c.At = b.now()
	}
	b.mu.Lock()
	b.m[[2]string{c.Kind, c.Key}] = c
	b.mu.Unlock()
}

// Conditions is every condition reported since start, by kind and key.
func (b *Board) Conditions() []Condition {
	b.mu.Lock()
	out := make([]Condition, 0, len(b.m))
	for _, c := range b.m {
		out = append(out, c)
	}
	b.mu.Unlock()
	slices.SortFunc(out, func(a, b Condition) int { return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Key, b.Key)) })
	return out
}

// Target is where an alert's row jumps to in the dashboard: a panel and its filter, as Noteworthy's findings.
type Target struct {
	Panel  string            `json:"panel"`
	Filter map[string]string `json:"filter"`
}

// Occurrence is one key a rule matched in a pass: a fact in its set, a condition that holds.
type Occurrence struct {
	// Key is the subject within the kind (an error's message, an address, a job), "" for the kind as a whole.
	Key string
	// Figure is the number the alert is about, nil for none; Unit what it counts.
	Figure *float64
	Unit   string
	// Text and Detail are one line each (Line), the outside world's text as it came: a message defuses them.
	Text, Detail string
	Target       *Target
	// Quiet is recorded and shown, never sent: a level rule's "no comparison".
	Quiet bool
	// Hold: the state is not known in this pass (a job not run since start, a level without its comparison): the
	// key's row stays as it is, nothing opens or clears (one pending its for starts it over).
	Hold bool
	// Wait, with Hold: the state is not judged yet but will be (a source within its after of the start, a level
	// still gathering its comparison or its calls, a script error not looked at yet). A rule's baseline waits for it:
	// what is there once it is judged was there at the rule's start, and is no news. A job not run since start holds
	// without waiting (when it runs and fails, that is news), so does the errors' level with too few visitors affected
	// (when it spreads to enough of them, that is).
	Wait bool
}
