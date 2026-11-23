package dash

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/trick77/stakeout/backend/internal/agg"
	"github.com/trick77/stakeout/backend/internal/app"
	"github.com/trick77/stakeout/backend/internal/store"
)

// purge runs the raw purge on the rig's store at its now, the grace passed, keeping raw days.
func (r *rig) purge(t *testing.T, days int) (int64, []store.PurgeWait) {
	t.Helper()
	if _, err := r.s.DB.ArmRawPurge(t.Context(), r.s.now().Add(-store.PurgeGrace-time.Hour)); err != nil {
		t.Fatal(err)
	}
	n, waits, err := r.s.DB.PurgeRaw(t.Context(), store.RetentionOptions{RawDays: days, Months: 25,
		Builder: agg.Version, Log: slog.New(slog.DiscardHandler)}, r.s.now())
	if err != nil {
		t.Fatal(err)
	}
	return n, waits
}

// A backfill cut off midway, then the purge, then the backfill resumed and the purge again: the 90d and 12mo
// answers stay those of the whole raw (counters exact, distinct counts within the sketches' tolerance). The first
// purge stops a day short of the first unbuilt day (that day's build needs the raw before it). Afterwards the
// raw-only answers reach back to where raw begins (detail_since), the figures before it come from the aggregates,
// and the footer says how long raw is kept.
func TestSources_PurgeBackfill(t *testing.T) {
	t.Parallel()
	r := copyRig(t, nil)
	const keep = 3 // raw days: the fewest, so the backfill cut off late rebuilds few days
	r.s.RawDays = keep
	asks := []struct {
		ep     string
		params map[string]string
		loose  func(string, any) bool
	}{
		{"overview", p(feedSite, "range", "90d", "tz", "UTC"), looseTiles},
		{"overview", p(feedSite, "range", "12mo", "cmp", "yoy", "tz", "UTC"), looseTiles},
		{"list", p(feedSite, "range", "90d", "tz", "UTC", "dim", "pages", "limit", "500"), looseRows("pages")},
		{"list", p(plainSite, "range", "12mo", "tz", "Europe/Zurich", "dim", "referrers", "limit", "500"),
			looseRows("referrers")},
		{"visitors", p(feedSite, "range", "90d", "tz", "UTC", "limit", "7"), nil},
	}
	answers := func(r *rig, stage string) []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, a := range asks {
			code, body := r.getFrom(t, SourceAuto, a.ep, a.params)
			if code != http.StatusOK {
				t.Fatalf("%s %s %v: %d %v", stage, a.ep, a.params, code, body)
			}
			// Raw-only parts: where raw begins, and what an address does over the raw span.
			delete(body, "detail_since")
			if a.ep == "visitors" {
				for _, row := range listRows(body) {
					delete(row, "does")
				}
			}
			out = append(out, body)
		}
		return out
	}
	whole := answers(r, "whole")
	same := func(stage string, got []map[string]any) {
		t.Helper()
		for i, a := range asks {
			agree(t, fmt.Sprint(stage, " ", a.ep, a.params), whole[i], got[i], a.loose)
		}
	}

	// The backfill cut off: days from mid on unbuilt, mid itself left mid-write.
	today := r.s.now().UnixMilli() / dayMS
	mid := today - 4
	for _, q := range []string{`DELETE FROM agg_hour WHERE hour >= ?`, `DELETE FROM agg_list WHERE day >= ?`,
		`DELETE FROM agg_addr WHERE day >= ?`, `DELETE FROM agg_day WHERE day >= ?`} {
		arg := mid
		if strings.Contains(q, "hour >=") {
			arg = mid * 24
		}
		if _, err := r.s.DB.DB.Exec(q, arg); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.s.DB.DB.Exec(`INSERT INTO agg_day (site_id, day, frozen, dirty) SELECT id, ?, 1, ? FROM site`, mid,
		store.AggWriting); err != nil {
		t.Fatal(err)
	}
	n, waits := r.purge(t, keep)
	if n == 0 || len(waits) != 2 {
		t.Fatalf("first purge: %d events, waits %v", n, waits)
	}
	for _, w := range waits {
		if w.Day != mid-1 || w.Cut {
			t.Errorf("wait %+v, want the day before %d", w, mid)
		}
	}
	same("cut off, purged", answers(r, "cut off, purged"))

	// Resumed: the job builds the rest; the purge goes on to the raw days.
	r.build(t)
	if n, waits = r.purge(t, keep); n == 0 || len(waits) != 0 {
		t.Fatalf("second purge: %d events, waits %v", n, waits)
	}
	// The purged days come from the aggregates.
	if pieces := countPieces(r, func(r *rig) { same("purged", answers(r, "purged")) }); pieces.agg == 0 {
		t.Errorf("pieces %+v", pieces)
	}

	// Raw begins at the cut: every raw-only answer says so, the trail starts there, the footer agrees.
	var oldest int64
	if err := r.s.DB.DB.QueryRow(`SELECT MIN(ts) FROM event`).Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	if oldest/dayMS != store.RawCutDay(r.s.now(), keep) {
		t.Fatalf("oldest raw on day %d, the cut is %d", oldest/dayMS, store.RawCutDay(r.s.now(), keep))
	}
	since := iso(time.UnixMilli(oldest))
	_, vis := r.get(t, "visitors", p(feedSite, "range", "30d", "tz", "UTC"))
	key := listRows(vis)[0]["key"].(string)
	_, pages := r.get(t, "list", p(feedSite, "range", "30d", "tz", "UTC", "dim", "pages"))
	for _, ask := range [][]string{{"visitors"}, {"log"}, {"visitor", "key", key},
		{"list", "dim", "pages", "within", listRows(pages)[0]["key"].(string)}} {
		code, body := r.get(t, ask[0], p(feedSite, append([]string{"range", "30d", "tz", "UTC"}, ask[1:]...)...))
		if code != 200 || body["detail_since"] != since {
			t.Errorf("%v: %d, detail_since %v, want %v", ask, code, body["detail_since"], since)
		}
		if ask[0] != "visitor" {
			continue
		}
		for _, x := range body["trail"].([]any) {
			if from := x.(map[string]any)["from"].(string); from < since.(string) {
				t.Errorf("trail before the raw: %v", x)
			}
		}
	}
	_, dom := r.get(t, "domain", p(feedSite, "range", "30d", "tz", "UTC"))
	st := dom["storage"].(map[string]any)
	if st["oldest"] != since || num64(st["raw_days"]) != keep || st["purge_from"] != nil ||
		st["figures_since"].(string) >= since.(string) {
		t.Errorf("storage %v", st)
	}

	// A stray event left on a purged day (a cut stopped part-way) moves neither where raw begins nor the footer's
	// "since": the purged gap stays visible.
	stray := oldest - 5*dayMS
	var cutRaw bool
	if err := r.s.DB.DB.QueryRow(`SELECT raw FROM agg_day d JOIN site s ON s.id = d.site_id WHERE s.domain = ?
		AND d.day = ?`, feedSite, stray/dayMS).Scan(&cutRaw); err != nil || cutRaw {
		t.Fatalf("the stray's day not purged: raw %v, %v", cutRaw, err)
	}
	if _, err := r.s.DB.DB.Exec(`INSERT INTO event (site_id, ts, source, kind, ip, ip_key) SELECT id, ?, 'server',
		'request', '192.0.2.200', '192.0.2.200' FROM site WHERE domain = ?`, stray, feedSite); err != nil {
		t.Fatal(err)
	}
	for _, ask := range []string{"visitors", "log"} {
		if _, body := r.get(t, ask, p(feedSite, "range", "30d", "tz", "UTC")); body["detail_since"] != since {
			t.Errorf("%s with a stray older event: detail_since %v, want %v", ask, body["detail_since"], since)
		}
	}
	if _, dom := r.get(t, "domain", p(feedSite, "range", "30d", "tz", "UTC")); dom["storage"].(map[string]any)["oldest"] != since {
		t.Errorf("footer with a stray older event: %v", dom["storage"])
	}
}

// zoneAsk is one 7d answer against its prior period in a zone.
type zoneAsk struct {
	tz, name, ep string
	params       map[string]string
	loose        func(string, any) bool
}

// zoneRig is a rig with five page loads a day from the 4th to the 22nd, built, and the 7d asks (tiles, pages,
// visitors) against the prior period in zones whose local midnights fall inside a UTC day: a half-hour one and the
// farthest from UTC either way included. get answers them, the raw-only parts left out.
func zoneRig(t *testing.T, zones ...string) (*rig, []zoneAsk, func() []map[string]any) {
	t.Helper()
	r, insert := exactRig(t)
	const chrome = "Mozilla/5.0 (Macintosh) Chrome/142 Safari/537.36"
	n := 0
	for d := 4; d <= 22; d++ {
		for _, h := range []string{"02", "06", "10", "15", "21"} {
			n++
			insert(pageLoad(at(fmt.Sprintf("2026-11-%02dT%s:00:00Z", d, h)), fmt.Sprint("v", n),
				fmt.Sprintf("192.0.2.%d", n%250), "CH", chrome, 64500)...)
		}
	}
	r.build(t)
	var asks []zoneAsk
	for _, tz := range zones {
		asks = append(asks,
			zoneAsk{tz, tz + " overview", "overview", p(feedSite, "range", "7d", "cmp", "prev", "tz", tz), looseTiles},
			zoneAsk{tz, tz + " pages", "list", p(feedSite, "range", "7d", "cmp", "prev", "tz", tz, "dim", "pages"),
				looseRows("pages")},
			zoneAsk{tz, tz + " visitors", "visitors", p(feedSite, "range", "7d", "cmp", "prev", "tz", tz), nil})
	}
	return r, asks, func() []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, a := range asks {
			code, body := r.getFrom(t, SourceAuto, a.ep, a.params)
			if code != http.StatusOK {
				t.Fatalf("%s: %d %v", a.name, code, body)
			}
			delete(body, "detail_since")
			if a.ep == "visitors" {
				for _, row := range listRows(body) {
					delete(row, "does")
				}
			}
			out = append(out, body)
		}
		return out
	}
}

// Raw kept the shortest allowed: 7d against its prior period answers as the whole raw did in every zone.
func TestSources_PurgeMinZones(t *testing.T) {
	r, asks, get := zoneRig(t, "America/Los_Angeles", "Etc/GMT+12", "Asia/Kolkata", "Pacific/Kiritimati",
		"Europe/Zurich")
	before := get()
	if n, _ := r.purge(t, app.MinRawRetentionDays); n == 0 {
		t.Fatal("nothing purged")
	}
	after := get()
	for i, a := range asks {
		agree(t, a.name, before[i], after[i], a.loose)
	}
}

// The size cap cutting raw younger than the raw days (all but its last three days): where the aggregates' grain
// allows (7d in UTC; the tiles' hours in a zone on whole hours) the cut days come from them, as the whole raw
// answered. A piece off the grain (a local midnight inside a UTC day for the lists, a half-hour zone's for the
// tiles) is raw only, and gone: the figures are short, so no change is claimed (null, flat) and the tiles' prior
// period shows no figures, never a percentage against a part of it.
func TestSources_CapCutRecentRaw(t *testing.T) {
	r, asks, get := zoneRig(t, "UTC", "Asia/Kolkata", "Europe/Zurich")
	before := get()
	used, err := r.s.DB.UsedBytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.DB.EnforceCap(t.Context(), store.RetentionOptions{MaxBytes: used / 100, Months: 25,
		Builder: agg.Version, Log: slog.New(slog.DiscardHandler)}, r.s.now()); err != nil {
		t.Fatal(err)
	}
	var oldest int64
	if err := r.s.DB.DB.QueryRow(`SELECT MIN(ts) FROM event`).Scan(&oldest); err != nil ||
		oldest/dayMS != r.s.now().UnixMilli()/dayMS-(store.CapTail-1) {
		t.Fatalf("the cap left raw from day %d, %v", oldest/dayMS-r.s.now().UnixMilli()/dayMS, err)
	}
	after := get()
	for i, a := range asks {
		if a.tz == "UTC" || a.tz == "Europe/Zurich" && a.ep == "overview" {
			agree(t, a.name, before[i], after[i], a.loose)
			continue
		}
		switch a.ep {
		case "overview":
			for _, x := range after[i]["tiles"].([]any) {
				tl := x.(map[string]any)
				if tl["change"] != nil || tl["verdict"] != "flat" || tl["prev"] != nil || len(tl["prev_series"].([]any)) != 0 {
					t.Errorf("%s: tile %v claims a change over cut raw: change %v, prev %v, prev_series %v", a.name,
						tl["key"], tl["change"], tl["prev"], tl["prev_series"])
				}
			}
		case "list":
			for _, row := range listRows(after[i]) {
				if row["change"] != nil {
					t.Errorf("%s: row %v changed %v over cut raw", a.name, row["key"], row["change"])
				}
			}
		}
	}
}

// A session over midnight from the last purged day into the first raw one is one visit: read across the raw
// horizon (7d reads the raw days raw, the purged day from its aggregates) and over aggregates alone (30d), in the
// table and the detail.
func TestSources_PurgeStitch(t *testing.T) {
	r, insert := exactRig(t)
	ev := func(ts int64, sid string) store.Event {
		return store.Event{TS: ts, Source: "browser", Kind: "page_view", Visitor: "v1", SID: sid, IP: "192.0.2.64",
			IPKey: "192.0.2.64", Template: "/", Path: "/", Cls: store.ClsHuman}
	}
	insert(ev(at("2026-11-17T23:50:00Z"), "s1"), ev(at("2026-11-18T00:10:00Z"), "s1"),
		ev(at("2026-11-18T10:00:00Z"), "s2"))
	r.build(t)
	if n, waits := r.purge(t, 4); n != 1 || len(waits) != 0 {
		t.Fatalf("purged %d, waits %v; want the 17th's one event", n, waits)
	}
	site, _ := r.s.Sites.Lookup(feedSite)
	for _, rng := range []string{"7d", "30d"} {
		var table float64 = -1
		n := countPieces(r, func(r *rig) {
			_, v := r.getFrom(t, SourceAuto, "visitors", p(feedSite, "range", rng, "tz", "UTC"))
			for _, row := range listRows(v) {
				if row["key"] == "192.0.2.64" {
					table = num64(row["visits"])
				}
			}
		})
		q := &query{site: site, aud: "all", p: newPeriod(rng, "off", r.s.now(), time.UTC)}
		a, err := r.server(SourceAuto).actorOne(t.Context(), q, "192.0.2.64")
		if err != nil || a == nil || table != 2 || a.visits != 2 || n.agg == 0 || (rng == "7d") != (n.raw > 0) {
			t.Errorf("%s: %v visits in the table, detail %+v %v, pieces %+v; want 2", rng, table, a, err, n)
		}
	}
}

// The size cap cutting one day's per-address rows (all of them) leaves the day's tiles and lists readable; only its
// addresses are gone, as after age retention. The last three days keep their raw.
func TestSources_CapCutAddrs(t *testing.T) {
	r, insert := exactRig(t)
	ua := "Mozilla/5.0 " + strings.Repeat("x", 500)
	var evs []store.Event
	for i := range 1500 {
		ip := fmt.Sprintf("198.51.100.%d", i%250)
		if i >= 250 {
			ip = fmt.Sprintf("2001:db8:%x::1", i)
		}
		evs = append(evs, store.Event{TS: at("2026-11-10T10:00:00Z") + int64(i)*1000, Source: "browser",
			Kind: "page_view", Visitor: fmt.Sprint("v", i), SID: fmt.Sprint("s", i), IP: ip, IPKey: ip, UA: ua,
			Template: "/", Path: "/", Country: "CH", Cls: store.ClsHuman})
	}
	for d := 11; d <= 22; d++ {
		evs = append(evs, store.Event{TS: at(fmt.Sprintf("2026-11-%02dT09:00:00Z", d)), Source: "browser",
			Kind: "page_view", Visitor: "w", SID: fmt.Sprint("w", d), IP: "192.0.2.65", IPKey: "192.0.2.65",
			Template: "/", Path: "/", Cls: store.ClsHuman})
	}
	insert(evs...)
	r.build(t)
	figures := func() []map[string]any {
		t.Helper()
		var out []map[string]any
		for _, ask := range [][]string{{"overview"}, {"list", "dim", "pages"}, {"list", "dim", "countries"},
			{"classes"}} {
			code, body := r.getFrom(t, SourceAuto, ask[0], p(feedSite, append([]string{"range", "30d", "tz", "UTC"},
				ask[1:]...)...))
			if code != 200 {
				t.Fatalf("%v: %d %v", ask, code, body)
			}
			delete(body, "detail_since")
			out = append(out, body)
		}
		return out
	}
	before := figures()
	if n, _ := r.purge(t, 3); n == 0 {
		t.Fatal("nothing purged")
	}
	used, err := r.s.DB.UsedBytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// A tenth over the cap's target: the 10th's per-address rows are most of what the rows take.
	if _, err := r.s.DB.EnforceCap(t.Context(), store.RetentionOptions{MaxBytes: used - 1, Months: 25,
		Builder: agg.Version, Log: slog.New(slog.DiscardHandler)}, r.s.now()); err != nil {
		t.Fatal(err)
	}
	day := at("2026-11-10T00:00:00Z") / dayMS
	count := func(q string, args ...any) (n int) {
		t.Helper()
		if err := r.s.DB.DB.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if a, l, other := count(`SELECT COUNT(*) FROM agg_addr WHERE day = ?`, day),
		count(`SELECT COUNT(*) FROM agg_list WHERE day = ?`, day),
		count(`SELECT COUNT(*) FROM agg_addr WHERE day > ?`, day); a != 0 || l == 0 || other == 0 {
		t.Fatalf("after the cap: %d per-address rows of the 10th, %d list rows, %d per-address rows after", a, l, other)
	}
	if n := count(`SELECT COUNT(*) FROM agg_day WHERE day = ? AND frozen = 1 AND dirty = 0`, day); n != 1 {
		t.Errorf("the 10th not clean after its per-address rows went")
	}
	if n := count(`SELECT COUNT(*) FROM event WHERE ts >= ?`, at("2026-11-20T00:00:00Z")); n != 3 {
		t.Errorf("the last three days' raw: %d events", n)
	}
	after := figures()
	for i := range before {
		agree(t, fmt.Sprint("figures after the per-address cut ", i), before[i], after[i], nil)
	}
	_, vis := r.get(t, "visitors", p(feedSite, "range", "30d", "tz", "UTC", "limit", "500"))
	if rows := listRows(vis); len(rows) != 1 || rows[0]["key"] != "192.0.2.65" {
		t.Errorf("visitors after the per-address cut: %d rows, first %v", len(rows), rows[0]["key"])
	}
}

// The footer's purge_from: the instant raw older than the raw days starts to go, during the grace only and only
// for a site holding such raw; null before a first start is recorded, after the grace, and on a young site. Until
// the first start is recorded, or while it cannot be read, nothing is purged: raw_days are the months', no purge
// claimed.
func TestDomain_PurgeFrom(t *testing.T) {
	r, insert := exactRig(t)
	r.s.RawDays = 10
	storage := func(site string) map[string]any {
		t.Helper()
		code, body := r.get(t, "domain", p(site, "range", "30d", "tz", "UTC"))
		if code != 200 {
			t.Fatalf("%d %v", code, body)
		}
		return body["storage"].(map[string]any)
	}
	purgeFrom := func(site string) any {
		t.Helper()
		st := storage(site)
		if num64(st["raw_days"]) != 10 {
			t.Errorf("raw_days %v", st["raw_days"])
		}
		return st["purge_from"]
	}
	insert(pageLoad(at("2026-11-01T10:00:00Z"), "a", "192.0.2.1", "CH", "Mozilla/5.0", 0)...)
	months := float64(int(r.s.now().Sub(r.s.now().AddDate(0, -r.s.RetentionMonths, 0)).Hours() / 24))
	if st := storage(feedSite); num64(st["raw_days"]) != months || st["purge_from"] != nil {
		t.Errorf("before a first start: %v, want raw_days %v", st, months)
	}
	if _, err := r.s.DB.DB.Exec(`INSERT INTO agg_meta (key, value) VALUES ('raw_purge_armed_at', 'soon')`); err != nil {
		t.Fatal(err)
	}
	if st := storage(feedSite); num64(st["raw_days"]) != months || st["purge_from"] != nil {
		t.Errorf("first start unreadable: %v, want raw_days %v", st, months)
	}
	if _, err := r.s.DB.DB.Exec(`DELETE FROM agg_meta`); err != nil {
		t.Fatal(err)
	}
	armed := r.s.now().Add(-24 * time.Hour)
	if _, err := r.s.DB.ArmRawPurge(t.Context(), armed); err != nil {
		t.Fatal(err)
	}
	if f, want := purgeFrom(feedSite), iso(armed.Add(store.PurgeGrace)); f != want {
		t.Errorf("during the grace: %v, want %v", f, want)
	}
	if f := purgeFrom(plainSite); f != nil {
		t.Errorf("a site without raw: %v", f)
	}
	// A site whose raw is all younger than the raw days: nothing to say.
	if _, err := r.s.DB.DB.Exec(`DELETE FROM event`); err != nil {
		t.Fatal(err)
	}
	insert(pageLoad(at("2026-11-20T10:00:00Z"), "a", "192.0.2.1", "CH", "Mozilla/5.0", 0)...)
	if f := purgeFrom(feedSite); f != nil {
		t.Errorf("young site: %v", f)
	}
	// After the grace.
	if _, err := r.s.DB.DB.Exec(`UPDATE agg_meta SET value = ?`, fmt.Sprint(r.s.now().Add(-8*24*time.Hour).UnixMilli())); err != nil {
		t.Fatal(err)
	}
	insert(pageLoad(at("2026-11-01T10:00:00Z"), "a", "192.0.2.1", "CH", "Mozilla/5.0", 0)...)
	if f := purgeFrom(feedSite); f != nil {
		t.Errorf("after the grace: %v", f)
	}
}
