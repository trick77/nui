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

// An open condition whose opening never got through is sent again: the channel down while it opened, it cleared
// before it was sent, came back within the cooldown and stays.
func TestUnheardOpenSentAgain(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "jobs", "on": "job.failed"}]}`)
	r.step(0)
	r.rec.Fail = errors.New("down")
	alert.Fail(r.board, alert.JobAgg, errors.New("disk I/O error"))
	r.step(5 * time.Minute) // opens, its send fails
	alert.Fail(r.board, alert.JobAgg, nil)
	r.step(5 * time.Minute) // clears before anyone heard: quiet
	r.rec.Fail = nil
	alert.Fail(r.board, alert.JobAgg, errors.New("disk I/O error"))
	r.step(5 * time.Minute) // back within the cooldown, and staying
	msgs := r.rec.Messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Title, "The aggregate build failed") ||
		strings.Contains(msgs[0].Title, "Cleared") {
		t.Fatalf("messages %+v", msgs)
	}
	if rows := r.rows("jobs"); len(rows) != 1 || rows[0].State != store.AlertOpen || rows[0].Told != "open" {
		t.Errorf("rows %+v", rows)
	}
}

// An opening given up after six hours is sent again while the condition lasts.
func TestGivenUpOpenSentAgain(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "jobs", "on": "job.failed"}]}`)
	r.step(0)
	r.rec.Fail = errors.New("down")
	alert.Fail(r.board, alert.JobAgg, errors.New("disk I/O error"))
	r.step(5 * time.Minute)
	r.step(7 * time.Hour) // given up
	if rows := r.rows("jobs"); rows[0].Due != "" || rows[0].Told != "" {
		t.Fatalf("given up %+v", rows[0])
	}
	r.rec.Fail = nil
	r.step(5 * time.Minute)
	if msgs := r.rec.Messages(); len(msgs) != 1 {
		t.Fatalf("messages %+v", msgs)
	}
}

// A clearing not sent yet is withdrawn when the condition is back within the cooldown: no "Cleared" while it is
// open, and its real clearing later is sent.
func TestReopenWithdrawsClear(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "jobs", "on": "job.failed"}]}`)
	r.step(0)
	alert.Fail(r.board, alert.JobAgg, errors.New("disk I/O error"))
	r.step(5 * time.Minute) // opened, sent
	r.rec.Fail = errors.New("down")
	alert.Fail(r.board, alert.JobAgg, nil)
	r.step(5 * time.Minute) // cleared, its send fails
	alert.Fail(r.board, alert.JobAgg, errors.New("disk I/O error"))
	r.step(5 * time.Minute) // back within the cooldown
	r.rec.Fail = nil
	r.step(5 * time.Minute)
	if msgs := r.rec.Messages(); len(msgs) != 1 {
		t.Fatalf("messages while it is open again %+v", msgs)
	}
	if rows := r.rows("jobs"); rows[0].State != store.AlertOpen || rows[0].Due != "" || rows[0].Told != "open" {
		t.Fatalf("row %+v", rows[0])
	}
	alert.Fail(r.board, alert.JobAgg, nil)
	r.step(5 * time.Minute)
	if msgs := r.rec.Messages(); len(msgs) != 2 || !strings.Contains(msgs[1].Title, "Cleared") {
		t.Fatalf("messages %+v", msgs)
	}
}

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

// The rows of a rule gone, switched off or no longer about a site are closed at the start, in silence, and pruned
// with the rest.
func TestRetire(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "errs", "on": "error.new"},
		{"name": "off", "on": "probe.new", "enabled": false}]}`)
	ctx := t.Context()
	at := r.now.UnixMilli()
	for _, rule := range []string{"gone", "off", "errs"} {
		a := store.Alert{SiteID: r.site, Rule: rule, Kind: "error.new", Key: "k", State: store.AlertOnce, Severity: "normal",
			Text: "t", FirstAt: at, LastAt: at, Told: "open"}
		if rule == "gone" {
			a.State, a.Kind, a.Due, a.DueAt, a.Channels = store.AlertOpen, "source.silent", "remind", &at, []string{"ops"}
		}
		if err := r.st.InsertAlert(ctx, &a); err != nil {
			t.Fatal(err)
		}
		if err := r.st.SetAlertRule(ctx, store.AlertRule{Rule: rule, SiteID: r.site, Sig: "s", CheckedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	// A site no longer configured: its rows of a live rule go too.
	gone := store.Alert{SiteID: 999, Rule: "errs", Kind: "error.new", Key: "k", State: store.AlertOnce, Severity: "normal",
		Text: "t", FirstAt: at, LastAt: at}
	if err := r.st.InsertAlert(ctx, &gone); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetAlertRule(ctx, store.AlertRule{Rule: "errs", SiteID: 999, Sig: "s", CheckedAt: at}); err != nil {
		t.Fatal(err)
	}
	r.src.occ["errs"] = occ("k")
	r.step(time.Minute)
	for _, rule := range []string{"gone", "off"} {
		if in, _ := r.st.InSet(ctx, rule, r.site); len(in) != 0 {
			t.Errorf("%s: still in its set %+v", rule, in)
		}
	}
	if in, _ := r.st.InSet(ctx, "errs", 999); len(in) != 0 {
		t.Errorf("a site gone: %+v", in)
	}
	if in, _ := r.st.InSet(ctx, "errs", r.site); len(in) != 1 {
		t.Errorf("a live rule's row closed: %+v", in)
	}
	if msgs := r.rec.Messages(); len(msgs) != 0 {
		t.Errorf("messages about rules gone %+v", msgs)
	}
	if n, err := r.st.PruneAlerts(ctx, r.now.Add(time.Hour)); err != nil || n != 3 {
		t.Errorf("pruned %d, %v", n, err)
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

// A facts pass that failed is run again at the next step, not an hour later.
func TestFailedFactsRetried(t *testing.T) {
	r := newRig(t, `{`+ops+`, "rules": [{"name": "errs", "on": "error.new"}]}`)
	r.step(0)
	r.src.occ["errs"], r.src.fail = occ("a"), 1
	r.now = r.now.Add(time.Hour)
	if err := r.e.Step(t.Context(), r.now); err == nil {
		t.Fatal("the failed pass went through")
	}
	r.step(time.Minute)
	if msgs := r.rec.Messages(); len(msgs) != 1 {
		t.Fatalf("messages %+v", msgs)
	}
	// A pass that keeps failing stops being retried within its hour.
	r.src.occ["errs"], r.src.fail = occ("a", "b"), 10
	r.now = r.now.Add(time.Hour)
	for range 5 {
		_ = r.e.Step(t.Context(), r.now)
		r.now = r.now.Add(time.Minute)
	}
	if r.src.fail != 10-factTries {
		t.Errorf("%d failed passes in an hour, want %d", 10-r.src.fail, factTries)
	}
}
