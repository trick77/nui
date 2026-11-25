package dash

import (
	"fmt"
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

func (w *watchEvents) req(ts int64, addr, tmpl string, status int, bytes int64) {
	w.insert(store.Event{SiteID: w.id, TS: ts, Source: "server", Kind: "request", IP: addr, IPKey: addr,
		Method: "GET", Template: tmpl, Status: status, Unrouted: true, Props: fmt.Sprintf(`{"bytes":%d}`, bytes)})
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

// watchRig is the feed site with a flipped path and a new script host, as the watch job found them, and today's
// hosts: a known one seen again, a new one, and one seen before by a single address that three report today.
func watchRig(t *testing.T) (*rig, *store.Store) {
	t.Helper()
	w := newWatchEvents(t)
	end := rigEnd.UnixMilli() / dayMS
	first := end - 12
	w.req(hourAt(first, 9), "192.0.2.1", "GET /", 200, 900)
	w.engage(hourAt(first, 9), "192.0.2.1", `"cdn.example.org"`)
	w.pass(hourAt(first, 12))
	w.req(hourAt(end-3, 4), "198.51.100.7", "GET /cache.php", 404, 153)
	w.req(hourAt(end-1, 10), "203.0.113.5", "GET /cache.php", 200, 48_000)
	w.engage(hourAt(end-1, 10), "192.0.2.9", `"slow.example.net"`)
	// Three networks each: three addresses of one /24 are one.
	nets := []string{"192.0.2.", "198.51.100.", "203.0.113."}
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

// Noteworthy words the watch's findings and opens their requests; the watch's cards list them with their state.
// Once the raw is gone the text says so and the row opens the card.
func TestWatchFindings(t *testing.T) {
	r, st := watchRig(t)
	_, body := r.get(t, "noteworthy", p(feedSite, "range", "7d", "tz", "UTC", "findings", "all"))
	byID := map[string]map[string]any{}
	for _, it := range listRowsOf(body, "items") {
		byID[it["id"].(string)] = it
	}
	flip, script := byID["flip:GET /cache.php"], byID["script:pay.example.net"]
	if flip == nil || script == nil {
		t.Fatalf("items %v", body["items"])
	}
	if flip["kind"] != "problem" || flip["text"] != "Refused path now answered: GET /cache.php" ||
		fmt.Sprint(flip["target"]) != "map[filter:map[kind:request path:GET /cache.php] panel:log]" {
		t.Errorf("flip %v", flip)
	}
	if fmt.Sprint(flip["detail"]) != "[Refused since 19 Nov 04:00, answered 21 Nov 10:00 Answered 48 kB, refused 153 B]" {
		t.Errorf("flip detail %v", flip["detail"])
	}
	if script["figure"] != "3" || script["unit"] != "addresses loaded" ||
		fmt.Sprint(script["target"]) != "map[filter:map[key:pay.example.net] panel:scripts]" {
		t.Errorf("script %v", script)
	}

	code, w := r.get(t, "watch", p(feedSite, "range", "7d", "tz", "UTC"))
	if code != 200 || w["ready"] != true || w["soft_day"] != nil {
		t.Fatalf("watch %d %v", code, w)
	}
	paths, hosts := listRowsOf(w, "paths"), listRowsOf(w, "hosts")
	if len(paths) != 1 || paths[0]["id"] != "flip:GET /cache.php" || paths[0]["raw"] != true ||
		paths[0]["refused_first"] != "2026-11-19T04:00:00Z" || fmt.Sprint(paths[0]["ok_bytes"]) != "[48000 48000]" ||
		fmt.Sprint(paths[0]["refused_bytes"]) != "[153]" {
		t.Errorf("paths %v", paths)
	}
	// New ones first: the closed finding, then today's provisional ones, the one listed before with the finding's
	// addresses and last sight.
	var got []string
	for _, h := range hosts {
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
	// The plain site has no feed: no paths, its scripts all the same.
	if _, w := r.get(t, "watch", p(plainSite)); len(listRowsOf(w, "paths")) != 0 || w["feed"] != false {
		t.Errorf("plain %v", w)
	}

	if _, err := st.DB.ExecContext(t.Context(), `DELETE FROM event`); err != nil {
		t.Fatal(err)
	}
	_, body = r.get(t, "noteworthy", p(feedSite, "range", "7d", "tz", "UTC", "findings", "all"))
	for _, it := range listRowsOf(body, "items") {
		if it["id"] == "flip:GET /cache.php" {
			if it["text"] != "Refused path now answered: GET /cache.php (its requests are no longer kept)" ||
				fmt.Sprint(it["target"]) != "map[filter:map[key:GET /cache.php] panel:answered]" {
				t.Errorf("without raw %v", it)
			}
			return
		}
	}
	t.Errorf("the flip went with its raw: %v", body["items"])
}

// Past ten of a kind the briefing gets the latest ten (a flip's score is every flip's); findings=all lists every one,
// the latest first, and the briefing's set stays what it was.
func TestWatchFindingsPastTheBound(t *testing.T) {
	w := newWatchEvents(t)
	end := rigEnd.UnixMilli() / dayMS
	first := end - 12
	w.req(hourAt(first, 9), "192.0.2.1", "GET /", 200, 900)
	w.pass(hourAt(first, 12))
	var latest []string
	for i := range 12 {
		tmpl := fmt.Sprintf("GET /f%02d.php", i)
		w.req(hourAt(end-5, 4), "198.51.100.7", tmpl, 404, 153)
		// Six a day: the job finds ten a kind a day at most.
		w.req(hourAt(end-2+int64(i/6), 1+i%6), "203.0.113.5", tmpl, 200, 48_000)
		latest = append([]string{"flip:" + tmpl}, latest...)
	}
	// Refused without a size reported, the earliest (and without the size's weight, the last): the card's
	// refused_bytes is null.
	w.req(hourAt(end-5, 4), "198.51.100.7", "GET /nosize.php", 404, 0)
	w.req(hourAt(end-2, 0), "203.0.113.5", "GET /nosize.php", 200, 48_000)
	latest = append(latest, "flip:GET /nosize.php")
	w.pass(rigEnd.UnixMilli())
	model := &fakeModel{}
	r := rigOver(t, w.st, nil, model)
	flips := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			if strings.HasPrefix(id, "flip:") {
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
	if got := flips(ids); !slices.Equal(got, latest) {
		t.Errorf("listed %v\nwant   %v", got, latest)
	}
	_, _ = r.get(t, "noteworthy", p(feedSite, "range", "7d", "tz", "UTC"))
	if len(model.briefIns) != 2 || model.briefIns[0].Hash != model.briefIns[1].Hash {
		t.Fatalf("briefing inputs %d", len(model.briefIns))
	}
	var briefed []string
	for _, f := range model.briefIns[0].Facts {
		briefed = append(briefed, f.ID)
	}
	if got := flips(briefed); !slices.Equal(got, latest[:maxWatchFindings]) {
		t.Errorf("briefed %v", got)
	}
	_, wb := r.get(t, "watch", p(feedSite, "range", "7d", "tz", "UTC"))
	rows := listRowsOf(wb, "paths")
	last := len(rows) - 1
	if len(rows) != len(latest) || rows[last]["key"] != "GET /nosize.php" || rows[last]["refused_bytes"] != nil ||
		fmt.Sprint(rows[0]["refused_bytes"]) != "[153]" {
		t.Errorf("card rows %d: %v", len(rows), rows)
	}

	// A provisional finding is shown and counted, never in the briefing's set: it may go again on the next pass, and
	// the briefing would be written again each time. The headline and more count those past the bound too.
	for i := range 3 {
		w.req(hourAt(end, 4)+int64(i), fmt.Sprint("198.51.100.", 1+i), "GET /today.php", 404, 153)
	}
	w.req(hourAt(end, 10), "203.0.113.5", "GET /today.php", 200, 48_000)
	w.pass(rigEnd.UnixMilli())
	_, body = r.get(t, "noteworthy", p(feedSite, "range", "7d", "tz", "UTC"))
	in := model.briefIns[len(model.briefIns)-1]
	if in.Hash != model.briefIns[0].Hash || slices.ContainsFunc(in.Facts, func(f insight.Fact) bool {
		return f.ID == "flip:GET /today.php"
	}) {
		t.Errorf("briefed with the provisional finding: %d facts", len(in.Facts))
	}
	items := listRowsOf(body, "items")
	n := len(in.Facts) + len(latest) - maxWatchFindings + 1
	if items[0]["id"] != "flip:GET /today.php" || len(items)+int(body["more"].(float64)) != n ||
		body["headline"].(map[string]any)["text"] != fmt.Sprint(n, " findings against the prior 7 d") {
		t.Errorf("limited: %d items, more %v, headline %v, want %d", len(items), body["more"], body["headline"], n)
	}
	_, body = r.get(t, "noteworthy", p(feedSite, "range", "7d", "tz", "UTC", "findings", "all"))
	if got := len(listRowsOf(body, "items")); got != n || body["more"] != 0.0 {
		t.Errorf("all: %d items, more %v, want %d", got, body["more"], n)
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
