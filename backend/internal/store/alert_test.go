package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/plancheck"
)

func TestAlertRows(t *testing.T) {
	st := open(t)
	ctx := t.Context()
	fig := 42.0
	a := Alert{SiteID: 1, Rule: "new-errors", Kind: "error.new", Key: "TypeError: x", State: AlertOnce, Severity: "high",
		Figure: &fig, Unit: "times", Text: "New error", Panel: "errors", Filter: map[string]string{"k": "v"},
		FirstAt: 1000, LastAt: 1000, Due: "open", DueAt: new(int64(1000)), Channels: []string{"ops"}}
	if err := st.InsertAlert(ctx, &a); err != nil || a.ID == 0 {
		t.Fatal(a.ID, err)
	}
	b := Alert{SiteID: 1, Rule: "new-errors", Kind: "error.new", Key: "other", State: AlertBaseline, Severity: "normal",
		Text: "New error", FirstAt: 1000, LastAt: 1000}
	c := Alert{SiteID: 0, Rule: "jobs", Kind: "job.failed", Key: "agg", State: AlertPending, Severity: "normal",
		Text: "The aggregate build failed", FirstAt: 1000, LastAt: 1000}
	for _, x := range []*Alert{&b, &c} {
		if err := st.InsertAlert(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	in, err := st.InSet(ctx, "new-errors", 1)
	if err != nil || len(in) != 2 {
		t.Fatalf("in set %+v %v", in, err)
	}
	got := in[0]
	if *got.Figure != 42 || got.Filter["k"] != "v" || !slices.Equal(got.Channels, []string{"ops"}) || len(got.Delivered) != 0 ||
		got.LeftAt != nil || *got.DueAt != 1000 {
		t.Errorf("row %+v", got)
	}
	got.LeftAt, got.Delivered = new(int64(2000)), []string{"ops"}
	if err := st.UpdateAlert(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if in, _ := st.InSet(ctx, "new-errors", 1); len(in) != 1 || in[0].Key != "other" {
		t.Errorf("after leaving %+v", in)
	}
	last, ok, err := st.LastAlert(ctx, "new-errors", 1, "TypeError: x")
	if err != nil || !ok || *last.LeftAt != 2000 || !slices.Equal(last.Delivered, []string{"ops"}) {
		t.Errorf("last %+v %v %v", last, ok, err)
	}
	if _, ok, _ := st.LastAlert(ctx, "new-errors", 2, "TypeError: x"); ok {
		t.Error("another site's row")
	}
	due, err := st.DueAlerts(ctx, 0, 0, 10)
	if err != nil || len(due) != 1 || due[0].ID != a.ID {
		t.Errorf("due %+v %v", due, err)
	}
	// The history leaves baseline and pending rows out.
	page, err := st.AlertPage(ctx, nil, 0, 10)
	if err != nil || len(page) != 1 || page[0].ID != a.ID {
		t.Errorf("page %+v %v", page, err)
	}
	site := int64(0)
	if page, _ := st.AlertPage(ctx, &site, 0, 10); len(page) != 0 {
		t.Errorf("the instance's %+v", page)
	}
	if err := st.DeleteAlert(ctx, c.ID); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := st.AlertRuleOf(ctx, "jobs", 0); ok || err != nil {
		t.Errorf("rule before its pass: %v %v", ok, err)
	}
	for _, at := range []int64{5, 6} {
		if err := st.SetAlertRule(ctx, AlertRule{Rule: "jobs", SiteID: 0, Sig: "s", CheckedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	if r, ok, err := st.AlertRuleOf(ctx, "jobs", 0); !ok || err != nil || r.CheckedAt != 6 || r.Sig != "s" {
		t.Errorf("rule %+v %v %v", r, ok, err)
	}

	at := time.UnixMilli(7000)
	if err := st.ChannelDelivered(ctx, "ops", at, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.ChannelDelivered(ctx, "ops", at.Add(time.Second), "ntfy: refused"); err != nil {
		t.Fatal(err)
	}
	chs, err := st.AlertChannels(ctx)
	if ch := chs["ops"]; err != nil || *ch.LastOK != 7000 || *ch.ErrorAt != 8000 || ch.Error != "ntfy: refused" {
		t.Errorf("channel %+v %v", ch, err)
	}

	// Pruned: rows that left their set before the cut; one still in it stays, however old.
	n, err := st.PruneAlerts(ctx, time.UnixMilli(5000))
	if err != nil || n != 1 {
		t.Errorf("pruned %d %v", n, err)
	}
	if in, _ := st.InSet(ctx, "new-errors", 1); len(in) != 1 {
		t.Errorf("the baseline row went: %+v", in)
	}
}

// TestAlertPlans pins every read of the alert table to an index: the history grows for 90 days.
func TestAlertPlans(t *testing.T) {
	st := open(t)
	for name, q := range map[string]struct {
		q    string
		args []any
	}{
		"in set":    {qInSet, []any{"r", 1}},
		"last":      {qLastAlert, []any{"r", 1, "k"}},
		"due":       {qDue, []any{1000, 5, 10}},
		"page site": {qPageSite, []any{1, 100, 10}},
		"page all":  {qPageAll, []any{100, 10}},
		"prune":     {qPrune, []any{1, 10}},
	} {
		plan, err := plancheck.Explain(t.Context(), st.DB, q.q, q.args...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, step := range plan {
			// The outbox's partial index holds the due rows only: scanning it is reading the outbox.
			whole := strings.HasPrefix(step, "SCAN alert") && step != "SCAN alert USING INDEX alert_due"
			if whole || strings.Contains(step, "TEMP B-TREE") {
				t.Errorf("%s: %s (plan %v)", name, step, plan)
			}
		}
	}
}
