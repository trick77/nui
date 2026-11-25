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

// cancelSender cuts the step's context as it is asked to send: the shutdown in mid-send.
type cancelSender struct{ cancel context.CancelFunc }

func (s cancelSender) Deliver(ctx context.Context, _, _ string, _ alert.Severity) []Delivery {
	s.cancel()
	return []Delivery{{Service: "ntfy", Err: ctx.Err()}}
}

// A shutdown in mid-send keeps what the channels before it took: they do not get it again after the restart.
func TestShutdownKeepsDelivered(t *testing.T) {
	r := newRig(t, `{"channels": {"a": "ntfy://ntfy.example.com/a", "b": "ntfy://ntfy.example.com/b"},
		"defaults": {"channels": ["a", "b"]}, "rules": [{"name": "errs", "on": "error.new"}]}`)
	r.step(0)
	r.src.occ["errs"] = occ("x")
	ctx, cancel := context.WithCancel(t.Context())
	r.e.o.Senders["b"] = cancelSender{cancel}
	r.now = r.now.Add(time.Hour)
	if err := r.e.Step(ctx, r.now); err == nil {
		t.Fatal("the cut step went through")
	}
	if row := r.rows("errs")[0]; len(row.Delivered) != 1 || row.Delivered[0] != "a" || row.Due != "open" {
		t.Fatalf("row %+v", row)
	}
	r.e.o.Senders["b"] = r.rec.Senders(r.e.o.Config)["b"]
	r.step(3 * time.Minute)
	got := map[string]int{}
	for _, m := range r.rec.Messages() {
		got[m.Channel]++
	}
	if got["a"] != 1 || got["b"] != 1 {
		t.Fatalf("messages by channel %v", got)
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

// A channel that is down holds no slot of the outbox: the healthy one gets every row owed to it at once, and a row
// due later goes out at the next step.
func TestOutboxNotStarved(t *testing.T) {
	r := newRig(t, `{"channels": {"ops": {"url": "ntfy://ntfy.example.com/o", "max_per_hour": 100},
		"mail": {"url": "smtp://mail.example.com:587/?to=ops@example.com", "max_per_hour": 100}},
		"defaults": {"channels": ["ops", "mail"]}, "rules": [{"name": "errs", "on": "error.new", "severity": "high"}]}`)
	down := &Recorder{Fail: errors.New("refused")}
	r.e.o.Senders["mail"] = down.Senders(r.e.o.Config)["mail"]
	ctx := t.Context()
	at := r.now.UnixMilli()
	for i := range 300 {
		a := store.Alert{SiteID: r.site, Rule: "errs", Kind: "error.new", Key: string(rune('a' + i%26)), State: store.AlertOnce,
			Severity: "normal", Text: "t", FirstAt: at, LastAt: at, Due: "open", DueAt: &at,
			Channels: []string{"ops", "mail"}}
		if err := r.st.InsertAlert(ctx, &a); err != nil {
			t.Fatal(err)
		}
	}
	r.e.retired = true // nothing to retire: the rows were written by hand
	for range 3 {
		if err := r.e.dispatch(ctx, r.now); err != nil {
			t.Fatal(err)
		}
		r.now = r.now.Add(time.Minute)
	}
	var owed int
	if err := r.st.DB.QueryRow(`SELECT COUNT(*) FROM alert WHERE delivered NOT LIKE '%"ops"%'`).Scan(&owed); err != nil ||
		owed != 0 {
		t.Fatalf("%d rows ops still owes, %v", owed, err)
	}
	later := store.Alert{SiteID: r.site, Rule: "errs", Kind: "error.new", Key: "late", State: store.AlertOnce,
		Severity: "high", Text: "late", FirstAt: at, LastAt: at, Due: "open", DueAt: new(r.now.UnixMilli()),
		Channels: []string{"ops", "mail"}}
	if err := r.st.InsertAlert(ctx, &later); err != nil {
		t.Fatal(err)
	}
	if err := r.e.dispatch(ctx, r.now); err != nil {
		t.Fatal(err)
	}
	last, _, _ := r.st.LastAlert(ctx, "errs", r.site, "late")
	if len(last.Delivered) != 1 || last.Delivered[0] != "ops" {
		t.Fatalf("the late row %+v", last)
	}
}
