package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/alert"
	"github.com/trick77/trakk/backend/internal/store"
)

// A rule's baseline waits for what it cannot judge yet: a source silent since before the start, judged once its
// after has passed, is part of the baseline, not news.
func TestBaselineWaitsForHeld(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "silent", "on": "source.silent", "after": "30m"}]}`)
	r.src.occ["silent"] = []alert.Occurrence{{Key: "server", Hold: true, Wait: true}}
	r.step(0)
	r.step(5 * time.Minute)
	r.src.occ["silent"] = []alert.Occurrence{{Key: "server", Text: "No events from the feed for 3 h"}}
	r.step(30 * time.Minute)
	r.step(5 * time.Minute)
	if msgs := r.rec.Messages(); len(msgs) != 0 {
		t.Fatalf("a source long silent sent as news: %+v", msgs)
	}
	if rows := r.rows("silent"); len(rows) != 1 || rows[0].State != store.AlertBaseline {
		t.Fatalf("rows %+v", rows)
	}
	// After the baseline, a source falling silent is news.
	r.src.occ["silent"] = append(r.src.occ["silent"], alert.Occurrence{Key: "browser", Text: "No events from the script"})
	r.step(5 * time.Minute)
	if msgs := r.rec.Messages(); len(msgs) != 1 || !strings.Contains(msgs[0].Title, "script") {
		t.Fatalf("messages %+v", msgs)
	}
}

// A clearing of what the recipients know open waits out a channel outage of any length.
func TestClearOutlivesOutage(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "jobs", "on": "job.failed"}]}`)
	r.step(0)
	alert.Fail(r.board, alert.JobAgg, errors.New("disk I/O error"))
	r.step(5 * time.Minute) // opened, sent
	r.rec.Fail = errors.New("down")
	alert.Fail(r.board, alert.JobAgg, nil)
	r.step(5 * time.Minute) // cleared, not sent
	r.step(7 * time.Hour)
	if row := r.rows("jobs")[0]; row.Due != "clear" {
		t.Fatalf("the clearing aged out: %+v", row)
	}
	r.rec.Fail = nil
	r.step(time.Hour)
	if msgs := r.rec.Messages(); len(msgs) != 2 || !strings.Contains(msgs[1].Title, "Cleared") {
		t.Fatalf("messages %+v", msgs)
	}
}

// tookThenCut delivers, and the shutdown ends the step's context as it returns.
type tookThenCut struct{ cancel context.CancelFunc }

func (s tookThenCut) Deliver(context.Context, string, string, alert.Severity) []Delivery {
	s.cancel()
	return []Delivery{{Service: "ntfy"}}
}

// A channel that took its message as the shutdown cut the step keeps it: not sent again after the restart.
func TestDeliveredAtTheCutKept(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "errs", "on": "error.new"}]}`)
	r.step(0)
	r.src.occ["errs"] = occ("x")
	ctx, cancel := context.WithCancel(t.Context())
	r.e.o.Senders["ops"] = tookThenCut{cancel}
	r.now = r.now.Add(time.Hour)
	if err := r.e.Step(ctx, r.now); err == nil {
		t.Fatal("the cut step went through")
	}
	if row := r.rows("errs")[0]; row.Due != "" || len(row.Delivered) != 1 || row.Told != "open" {
		t.Fatalf("row %+v", row)
	}
	r.e.o.Senders["ops"] = r.rec.Senders(r.e.o.Config)["ops"]
	r.step(time.Minute)
	if msgs := r.rec.Messages(); len(msgs) != 0 {
		t.Fatalf("sent again %+v", msgs)
	}
}
