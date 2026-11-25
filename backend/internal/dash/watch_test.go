package dash

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/insight"
	"github.com/trick77/trakk/backend/internal/sites"
	"github.com/trick77/trakk/backend/internal/store"
	"github.com/trick77/trakk/backend/internal/watch"
)

// watchEvents builds a feed site's (and a plain site's) events for the watch job and runs its passes.
type watchEvents struct {
	t  *testing.T
	st *store.Store
	id int64
}

func newWatchEvents(t *testing.T) *watchEvents {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rows, err := st.SyncSites(t.Context(), []store.Site{
		{Domain: feedSite, Kind: "site", FeedKeyHash: sites.HashKey("k"), PathRules: "[]"},
		{Domain: plainSite, Kind: "site", PathRules: "[]"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &watchEvents{t: t, st: st, id: rows[feedSite].ID}
}

// hourAt is hour h of day d, unix ms.
func hourAt(d int64, h int) int64 { return d*dayMS + int64(h)*3_600_000 }

func (w *watchEvents) req(ts int64, addr string) {
	w.insert(store.Event{SiteID: w.id, TS: ts, Source: "server", Kind: "request", IP: addr, IPKey: addr,
		Method: "GET", Template: "GET /", Status: 200, Props: `{"bytes":900}`})
}

// engage is an address's engagement reporting script hosts (hosts: their JSON strings, comma-separated).
func (w *watchEvents) engage(ts int64, addr, hosts string) {
	w.insert(store.Event{SiteID: w.id, TS: ts, Source: "browser", Kind: "engagement", Visitor: "v-" + addr,
		IP: addr, IPKey: addr, Props: `{"p":{"scripts":[` + hosts + `]}}`})
}

func (w *watchEvents) insert(evs ...store.Event) {
	if err := w.st.InsertEvents(w.t.Context(), evs); err != nil {
		w.t.Fatal(err)
	}
}

func (w *watchEvents) pass(now int64) {
	if err := watch.Pass(w.t.Context(), w.st, time.UnixMilli(now)); err != nil {
		w.t.Fatal(err)
	}
}

// nets are addresses of three networks each: three addresses of one /24 are one.
var nets = []string{"192.0.2.", "198.51.100.", "203.0.113."}

// watchRig is the feed site with a new script host the watch job found, and today's hosts: a known one seen again,
// a new one, and one seen before by a single address that three report today.
func watchRig(t *testing.T) (*rig, *store.Store) {
	t.Helper()
	w := newWatchEvents(t)
	end := rigEnd.UnixMilli() / dayMS
	first := end - 12
	w.req(hourAt(first, 9), "192.0.2.1")
	w.engage(hourAt(first, 9), "192.0.2.1", `"cdn.example.org"`)
	w.pass(hourAt(first, 12))
	w.engage(hourAt(end-1, 10), "192.0.2.9", `"slow.example.net"`)
	for i := range 3 {
		w.engage(hourAt(end-1, 11)+int64(i), fmt.Sprint(nets[i], 10+i), `"cdn.example.org","pay.example.net"`)
	}
	for i := range 3 {
		w.engage(hourAt(end, 10)+int64(i), fmt.Sprint(nets[i], 20+i),
			`"cdn.example.org","today.example.net","slow.example.net"`)
	}
	w.pass(rigEnd.UnixMilli())
	return rigOver(t, w.st, nil, nil), w.st
}

// Noteworthy words the watch's findings and opens the card on them; the card lists the hosts with their state.
func TestWatchFindings(t *testing.T) {
	r, _ := watchRig(t)
	_, body := r.get(t, "noteworthy", p(feedSite, "range", "7d", "tz", "UTC", "findings", "all"))
	byID := map[string]map[string]any{}
	for _, it := range listRowsOf(body, "items") {
		byID[it["id"].(string)] = it
	}
	script := byID["script:pay.example.net"]
	if script == nil {
		t.Fatalf("items %v", body["items"])
	}
	if script["figure"] != "3" || script["unit"] != "addresses loaded" || script["kind"] != "problem" ||
		fmt.Sprint(script["target"]) != "map[filter:map[key:pay.example.net] panel:scripts]" {
		t.Errorf("script %v", script)
	}
	if today := byID["script:today.example.net"]; today == nil ||
		!slices.Contains(today["detail"].([]any), any("Provisional: judged on the day so far")) {
		t.Errorf("today %v", today)
	}

	code, w := r.get(t, "watch", p(feedSite, "range", "7d", "tz", "UTC"))
	if code != 200 || w["ready"] != true || w["cut"] != 0.0 || w["scripts_learning_until"] == nil {
		t.Fatalf("watch %d %v", code, w)
	}
	for _, k := range []string{"paths", "feed", "learning_until", "soft_day"} {
		if _, ok := w[k]; ok {
			t.Errorf("watch answers %s", k)
		}
	}
	// New ones first: the closed finding, then today's provisional ones, the one listed before with the finding's
	// addresses and last sight.
	var got []string
	for _, h := range listRowsOf(w, "hosts") {
		got = append(got, fmt.Sprint(h["host"], " ", h["addrs"], " ", h["new"], " ", h["provisional"], " ", h["last_seen"]))
	}
	if fmt.Sprint(got) != "[pay.example.net 3 true false 2026-11-21T11:00:00Z "+
		"slow.example.net 3 true true 2026-11-22T10:00:00Z today.example.net 3 true true 2026-11-22T10:00:00Z "+
		"cdn.example.org 3 false false 2026-11-22T10:00:00Z]" {
		t.Errorf("hosts %v", got)
	}
	// The day so far is listed before it closes: the 24 hours hold today's hosts. Yesterday's finding is not of them:
	// it happened at its first sight that day, not when the job merged the day.
	_, w = r.get(t, "watch", p(feedSite, "range", "24h", "tz", "UTC"))
	got = nil
	for _, h := range listRowsOf(w, "hosts") {
		got = append(got, fmt.Sprint(h["host"]))
	}
	if fmt.Sprint(got) != "[slow.example.net today.example.net cdn.example.org]" {
		t.Errorf("24h hosts %v", got)
	}
	// A site without a feed has its script hosts all the same.
	if _, w := r.get(t, "watch", p(plainSite)); w["ready"] != true || len(listRowsOf(w, "hosts")) != 0 {
		t.Errorf("plain %v", w)
	}
}

// Past ten the briefing gets the weightiest ten; findings=all lists every one, and the briefing's set stays what it
// was. A provisional finding is shown and counted, never in the briefing's set.
func TestWatchFindingsPastTheBound(t *testing.T) {
	w := newWatchEvents(t)
	end := rigEnd.UnixMilli() / dayMS
	first := end - 12
	w.req(hourAt(first, 9), "192.0.2.1")
	w.engage(hourAt(first, 9), "192.0.2.1", `"cdn.example.org"`)
	w.pass(hourAt(first, 12))
	// Six a day: the job finds ten a day at most. The later ones from more addresses: the weightiest.
	var heaviest []string
	for i := range 12 {
		host := fmt.Sprintf("h%02d.example.net", i)
		for n := range 3 + i {
			w.engage(hourAt(end-3+int64(i/6), 1+i%6)+int64(n), fmt.Sprint(nets[n%3], 10+n), `"`+host+`"`)
		}
		heaviest = append([]string{"script:" + host}, heaviest...)
	}
	w.pass(hourAt(end, 3))
	model := &fakeModel{}
	r := rigOver(t, w.st, nil, model)
	scripts := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			if strings.HasPrefix(id, "script:") {
				out = append(out, id)
			}
		}
		return out
	}
	_, body := r.get(t, "noteworthy", p(feedSite, "range", "7d", "tz", "UTC", "findings", "all"))
	var ids []string
	for _, it := range listRowsOf(body, "items") {
		ids = append(ids, it["id"].(string))
	}
	if got := scripts(ids); !slices.Equal(got, heaviest) {
		t.Errorf("listed %v\nwant   %v", got, heaviest)
	}
	_, _ = r.get(t, "noteworthy", p(feedSite, "range", "7d", "tz", "UTC"))
	if len(model.briefIns) != 2 || model.briefIns[0].Hash != model.briefIns[1].Hash {
		t.Fatalf("briefing inputs %d", len(model.briefIns))
	}
	var briefed []string
	for _, f := range model.briefIns[0].Facts {
		briefed = append(briefed, f.ID)
	}
	if got := scripts(briefed); !slices.Equal(got, heaviest[:maxWatchFindings]) {
		t.Errorf("briefed %v", got)
	}

	// A provisional finding may go again on the next pass, and the briefing would be written again each time. The
	// headline and more count those past the bound too.
	for n := range 3 {
		w.engage(hourAt(end, 4)+int64(n), fmt.Sprint(nets[n], 50+n), `"today.example.net"`)
	}
	w.pass(rigEnd.UnixMilli())
	_, body = r.get(t, "noteworthy", p(feedSite, "range", "7d", "tz", "UTC"))
	in := model.briefIns[len(model.briefIns)-1]
	if in.Hash != model.briefIns[0].Hash || slices.ContainsFunc(in.Facts, func(f insight.Fact) bool {
		return f.ID == "script:today.example.net"
	}) {
		t.Errorf("briefed with the provisional finding: %d facts", len(in.Facts))
	}
	items := listRowsOf(body, "items")
	n := len(in.Facts) + len(heaviest) - maxWatchFindings + 1
	if len(items)+int(body["more"].(float64)) != n ||
		body["headline"].(map[string]any)["text"] != fmt.Sprint(n, " findings against the prior 7 d") {
		t.Errorf("limited: %d items, more %v, headline %v, want %d", len(items), body["more"], body["headline"], n)
	}
	_, body = r.get(t, "noteworthy", p(feedSite, "range", "7d", "tz", "UTC", "findings", "all"))
	all := listRowsOf(body, "items")
	if len(all) != n || body["more"] != 0.0 ||
		!slices.ContainsFunc(all, func(it map[string]any) bool { return it["id"] == "script:today.example.net" }) {
		t.Errorf("all: %d items, more %v, want %d", len(all), body["more"], n)
	}
	// The alert rules take what the job made in their 24 hours, whenever it happened: the twelve merged at 03:00, past
	// the bound too, and today's provisional one. Their time names its zone; Noteworthy's is the viewer's.
	site, _ := r.s.Sites.Lookup(feedSite)
	fs, _, err := r.s.Findings(t.Context(), site, rigEnd, DefaultThresholds())
	if err != nil {
		t.Fatal(err)
	}
	alerted := map[string]insight.Fact{}
	for _, f := range fs {
		if strings.HasPrefix(f.ID, "script:") {
			alerted[f.ID] = f
		}
	}
	if _, ok := alerted["script:today.example.net"]; len(alerted) != len(heaviest)+1 || !ok {
		t.Errorf("alert facts %v", slices.Sorted(maps.Keys(alerted)))
	}
	const detail = ", pages load scripts from it"
	if d := alerted["script:h00.example.net"].Detail; len(d) == 0 || d[0] != "First seen 2026-11-19 01:00 UTC"+detail {
		t.Errorf("alert detail %q", d)
	}
	for _, it := range all {
		if it["id"] == "script:h00.example.net" && fmt.Sprint(it["detail"]) != "[First seen 19 Nov 01:00"+detail+"]" {
			t.Errorf("noteworthy detail %v", it["detail"])
		}
	}
}

func listRowsOf(body map[string]any, key string) []map[string]any {
	xs, _ := body[key].([]any)
	out := make([]map[string]any, 0, len(xs))
	for _, x := range xs {
		out = append(out, x.(map[string]any))
	}
	return out
}
