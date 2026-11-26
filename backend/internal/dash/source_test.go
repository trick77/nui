package dash

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/agg"
	"github.com/trick77/trakk/backend/internal/store"
)

// skipRace skips a test -race makes too slow for CI's ten minutes a package: the gate runs it whole without (ship.yaml).
func skipRace(t *testing.T) {
	t.Helper()
	if raceOn {
		t.Skip("run without -race (ship.yaml's step)")
	}
}

// build builds every day of the rig's store up to its end, as BuildThrough does for a history.
func (r *rig) build(t *testing.T) {
	t.Helper()
	if _, err := agg.BuildThrough(t.Context(), r.s.DB, r.end, agg.TopK); err != nil {
		t.Fatal(err)
	}
}

// pieceCount counts the pieces read from each source: from the aggregates, raw on the tables' grain (pieces the
// aggregates could have served), raw off it.
type pieceCount struct{ agg, raw, off int }

// countPieces counts the pieces the requests f makes through its view of r read.
func countPieces(r *rig, f func(r *rig)) pieceCount {
	var mu sync.Mutex
	var c pieceCount
	v := *r
	v.trace = func(fromAgg, aligned bool) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case fromAgg:
			c.agg++
		case aligned:
			c.raw++
		default:
			c.off++
		}
	}
	f(&v)
	return c
}

// server is a server of its own over the rig's store reading from src: parallel subtests share the rig.
func (r *rig) server(src Source) *Server {
	s := r.s
	return &Server{DB: s.DB, Sites: s.Sites, Ref: s.Ref, Counters: s.Counters, Model: s.Model,
		RetentionMonths: s.RetentionMonths, Now: s.Now, Source: src, trace: r.trace}
}

// getFrom asks the endpoint of a server reading from src.
func (r *rig) getFrom(t *testing.T, src Source, endpoint string, params map[string]string) (int, map[string]any) {
	t.Helper()
	return serve(t, handler(r.server(src)), endpoint, params)
}

// both asks an endpoint from raw and from the aggregates, each answering 200: two equal failures never agree.
func (r *rig) both(t *testing.T, ep string, params map[string]string) (raw, got map[string]any) {
	t.Helper()
	code, raw := r.getFrom(t, SourceRaw, ep, params)
	gotCode, got := r.getFrom(t, SourceAgg, ep, params)
	if code != 200 || gotCode != 200 {
		t.Fatalf("%s %v: %d %v, %d %v", ep, params, code, raw, gotCode, got)
	}
	return raw, got
}

// sourceCase is a zone, range and comparison both sources are asked in.
type sourceCase struct{ tz, rng, cmp string }

func (c sourceCase) String() string {
	return strings.ReplaceAll(c.tz, "/", "-") + "_" + c.rng + "_" + c.cmp
}

// raceCases replace the full matrix under -race, where SQLite crawls: every zone, range and comparison at least
// once, and the pairs with a path of their own (24h and Kolkata's half hour at 7d, read raw, and at 30d, floored;
// a zone west of UTC at 30d and 90d; 24h against last year, which compares no list; 12mo in UTC).
var raceCases = []sourceCase{{"UTC", "24h", "prev"}, {"UTC", "7d", "yoy"}, {"UTC", "30d", "prev"},
	{"UTC", "90d", "yoy"}, {"UTC", "12mo", "prev"}, {"Europe/Zurich", "7d", "prev"}, {"Europe/Zurich", "30d", "yoy"},
	{"Asia/Kolkata", "24h", "yoy"}, {"Asia/Kolkata", "7d", "prev"}, {"Asia/Kolkata", "30d", "prev"},
	{"America/New_York", "24h", "prev"}, {"America/New_York", "30d", "prev"}, {"America/New_York", "90d", "yoy"}}

// sourceCases are the cases of the ranges given: every zone × range × comparison (12mo in UTC only: a year of
// pieces reads alike in every zone), under -race raceCases.
func sourceCases(ranges ...string) []sourceCase {
	var out []sourceCase
	if raceOn {
		for _, c := range raceCases {
			if slices.Contains(ranges, c.rng) {
				out = append(out, c)
			}
		}
		return out
	}
	for _, tz := range []string{"UTC", "Europe/Zurich", "Asia/Kolkata", "America/New_York"} {
		for _, rng := range ranges {
			for _, c := range []string{"prev", "yoy"} {
				if rng != "12mo" || tz == "UTC" {
					out = append(out, sourceCase{tz, rng, c})
				}
			}
		}
	}
	return out
}

// fullAuds: every audience is asked at 30d (against the prior period only, unless anyCmp), under -race in UTC
// against the prior period only.
func fullAuds(c sourceCase, anyCmp bool) []string {
	if c.rng == "30d" && (c.cmp == "prev" || anyCmp && !raceOn) && (!raceOn || c.tz == "UTC") {
		return []string{"all", "humans", "bots"}
	}
	return []string{"all"}
}

// distinctTol is how far a distinct count from sketches may be from the exact one: index collisions, rare in the
// sparse form; none below 50, where an off-by-one is a defect.
func distinctTol(a, b float64) bool {
	if math.Max(a, b) < 50 {
		return a == b
	}
	return math.Abs(a-b) <= math.Max(1, 0.001*math.Max(math.Abs(a), math.Abs(b)))
}

// rowPath is a list answer's row.
var rowPath = regexp.MustCompile(`^\.rows\[\d+\]$`)

// agree compares two answers field by field: equal, but inside a loose subtree (a distinct figure) numbers within
// distinctTol, and the change and verdict derived from them not at all, unless a list row's two values are equal.
func agree(t *testing.T, where string, raw, got any, loose func(path string, v any) bool) {
	t.Helper()
	var walk func(path string, a, b any, isLoose bool)
	walk = func(path string, a, b any, isLoose bool) {
		isLoose = isLoose || (loose != nil && loose(path, a))
		switch x := a.(type) {
		case map[string]any:
			y, ok := b.(map[string]any)
			if !ok || len(x) != len(y) {
				t.Errorf("%s %s: %v vs %v", where, path, a, b)
				return
			}
			same := rowPath.MatchString(path) && x["value"] == y["value"]
			for k, v := range x {
				if isLoose && (k == "change" || k == "verdict") && !same {
					continue
				}
				walk(path+"."+k, v, y[k], isLoose)
			}
		case []any:
			y, ok := b.([]any)
			if !ok || len(x) != len(y) {
				t.Errorf("%s %s: %d items vs %v", where, path, len(x), b)
				return
			}
			for i := range x {
				walk(fmt.Sprintf("%s[%d]", path, i), x[i], y[i], isLoose)
			}
		case float64:
			y, ok := b.(float64)
			if !ok || (x != y && !(isLoose && distinctTol(x, y))) {
				t.Errorf("%s %s: raw %v, aggregates %v", where, path, a, b)
			}
		default:
			if fmt.Sprint(a) != fmt.Sprint(b) {
				t.Errorf("%s %s: raw %v, aggregates %v", where, path, a, b)
			}
		}
	}
	walk("", raw, got, false)
}

// looseTiles marks the distinct figures of the overview and the classes: the visitors tile, addresses per class.
func looseTiles(path string, v any) bool {
	if m, ok := v.(map[string]any); ok && m["key"] == "visitors" {
		return true
	}
	return strings.HasSuffix(path, ".addresses")
}

// The tiles, the classes and Noteworthy's trends read the same from the hourly aggregates as from raw, in every zone
// and comparison; the aggregates were read, and no piece on their grain was read raw.
func TestSources_Hourly(t *testing.T) {
	t.Parallel()
	r := sharedRig(t, nil)
	for _, c := range sourceCases("24h", "7d", "30d", "90d", "12mo") {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			for _, aud := range fullAuds(c, true) {
				params := p(feedSite, "range", c.rng, "cmp", c.cmp, "aud", aud, "tz", c.tz)
				where := fmt.Sprint(params)
				for _, ep := range []string{"overview", "classes"} {
					code, raw := r.getFrom(t, SourceRaw, ep, params)
					var got map[string]any
					var gotCode int
					n := countPieces(r, func(r *rig) { gotCode, got = r.getFrom(t, SourceAgg, ep, params) })
					if code != 200 || gotCode != 200 {
						t.Fatalf("%s %s: %d, %d %v", ep, where, code, gotCode, got)
					}
					delete(raw, "live")
					delete(got, "live")
					agree(t, ep+" "+where, raw, got, looseTiles)
					// 24h begins and ends inside an hour, Kolkata's 7d on the half hour: read raw, under every source.
					offGrain := c.rng == "24h" || c.tz == "Asia/Kolkata" && c.rng == "7d"
					if (n.agg == 0 && !offGrain) || n.raw != 0 || (n.off != 0 && !offGrain) {
						t.Errorf("%s %s: pieces %+v", ep, where, n)
					}
				}
			}
		})
	}
}

// looseRows marks a list's rows as loose when a figure of theirs is a distinct count; count dimensions agree exactly.
func looseRows(dim string) func(string, any) bool {
	d, ok := dims[dim]
	distinct := ok && d.distinct != ""
	return func(path string, _ any) bool {
		return distinct && (strings.HasPrefix(path, ".rows[") || path == ".other")
	}
}

// The lists, status, the clients split and Noteworthy's findings read the same from the daily aggregates as from
// raw, in every zone and comparison.
func TestSources_Lists(t *testing.T) {
	t.Parallel()
	r := sharedRig(t, nil)
	ask := func(t *testing.T, ep, where string, params map[string]string, loose func(string, any) bool) {
		t.Helper()
		code, raw := r.getFrom(t, SourceRaw, ep, params)
		var got map[string]any
		var gotCode int
		n := countPieces(r, func(r *rig) { gotCode, got = r.getFrom(t, SourceAgg, ep, params) })
		if code != 200 || gotCode != 200 {
			t.Fatalf("%s %s: %d, %d %v", ep, where, code, gotCode, got)
		}
		agree(t, ep+" "+where, raw, got, loose)
		if n.raw != 0 || (n.agg == 0 && params["range"] != "7d" && params["within"] == "") {
			t.Errorf("%s %s: pieces %+v", ep, where, n)
		}
	}
	names := slices.Sorted(maps.Keys(dims))
	for i, c := range sourceCases("7d", "30d", "90d", "12mo") {
		t.Run(c.String(), func(t *testing.T) {
			t.Parallel()
			// Under -race every dimension in UTC at 30d, and two of them in each other case, in turn.
			ds := names
			if raceOn && c != (sourceCase{"UTC", "30d", "prev"}) {
				ds = []string{names[(2*i)%len(names)], names[(2*i+1)%len(names)]}
			}
			for _, aud := range fullAuds(c, false) {
				base := p(feedSite, "range", c.rng, "cmp", c.cmp, "aud", aud, "tz", c.tz)
				where := fmt.Sprint(base)
				for _, dim := range ds {
					params := p(feedSite, "range", c.rng, "cmp", c.cmp, "aud", aud, "tz", c.tz, "dim", dim, "limit", "500")
					ask(t, "list", where+" "+dim, params, looseRows(dim))
				}
				ask(t, "status", where, base, nil)
				ask(t, "clients", where, base, func(string, any) bool { return c.rng != "7d" })
				if aud == "all" {
					ask(t, "noteworthy", where, p(feedSite, "range", c.rng, "cmp", c.cmp, "tz", c.tz, "findings", "all"), nil)
				}
			}
		})
	}
	// The other parameters: a sort, a search, a sub, the paths inside a template (raw only).
	t.Run("params", func(t *testing.T) {
		t.Parallel()
		for _, kv := range [][]string{{"dim", "pages", "sort", "change"}, {"dim", "pages", "limit", "2"},
			{"dim", "countries", "q", "ch"}, {"dim", "events", "sub", "custom"}, {"dim", "events", "sub", "auto"},
			{"dim", "pages", "within", "/:cidr"}, {"dim", "entry", "limit", "3"}} {
			params := p(feedSite, append([]string{"range", "30d", "tz", "UTC"}, kv...)...)
			ask(t, "list", fmt.Sprint(kv), params, looseRows(kv[1]))
		}
	})
}

// The visitors table and a visitor's sums read the same from agg_addr as from raw. In each case every row of the
// table is compared before its page's per-row reads (what it does, the reading: raw under every source, and most of
// the cost); the whole answer at the UI's limits in UTC, and the details of the keys listed first.
func TestSources_Addresses(t *testing.T) {
	t.Parallel()
	r := sharedRig(t, &fakeModel{})
	site, _ := r.s.Sites.Lookup(feedSite)
	type pair struct{ aud, sort string }
	var pairs []pair
	for _, aud := range []string{"all", "humans", "bots"} {
		for _, sortBy := range []string{"requests", "views"} {
			pairs = append(pairs, pair{aud, sortBy})
		}
	}
	seen := map[[2]string]bool{} // the visitors table has no comparison: one case per zone and range
	for i, c := range sourceCases("7d", "30d", "90d", "12mo") {
		if seen[[2]string{c.tz, c.rng}] {
			continue
		}
		seen[[2]string{c.tz, c.rng}] = true
		t.Run(c.tz+"_"+c.rng, func(t *testing.T) {
			t.Parallel()
			loc, _ := time.LoadLocation(c.tz)
			// Under -race every audience and order in UTC at 30d, one pair in each other case, in turn.
			ps := pairs
			if raceOn && (c.tz != "UTC" || c.rng != "30d") {
				ps = pairs[i%len(pairs) : i%len(pairs)+1]
			}
			for _, pr := range ps {
				table := func(r *rig, src Source) []any {
					q := &query{site: site, rng: c.rng, loc: loc, tz: c.tz, aud: pr.aud, cmp: "off",
						p: newPeriod(c.rng, "off", r.s.now(), loc)}
					rows, err := r.server(src).visitorRows(t.Context(), q, visitorCursor{From: ms(q.p.win.From),
						To: ms(q.p.Now)}, pr.sort, "", "")
					if err != nil {
						t.Fatal(err)
					}
					b, _ := json.Marshal(rows)
					var out []any
					_ = json.Unmarshal(b, &out)
					return out
				}
				raw := table(r, SourceRaw)
				var got []any
				n := countPieces(r, func(r *rig) { got = table(r, SourceAgg) })
				where := fmt.Sprint(c, pr)
				agree(t, "visitors "+where, raw, got, nil)
				if len(raw) == 0 || n.raw != 0 || (n.agg == 0 && c.rng != "7d") {
					t.Errorf("visitors %s: %d rows, pieces %+v", where, len(raw), n)
				}
			}
		})
	}
	both := r.both
	t.Run("answers", func(t *testing.T) {
		t.Parallel()
		limit := "50"
		if raceOn {
			limit = "7"
		}
		for _, params := range []map[string]string{p(feedSite, "range", "30d", "tz", "UTC", "limit", limit),
			p(feedSite, "range", "12mo", "tz", "UTC", "aud", "bots", "sort", "views", "limit", "7")} {
			raw, got := both(t, "visitors", params)
			agree(t, "visitors "+fmt.Sprint(params), raw, got, nil)
			if len(listRows(got)) == 0 {
				t.Errorf("visitors %v: no rows", params)
			}
		}
	})
	t.Run("details", func(t *testing.T) {
		t.Parallel()
		// The keys the tables list first (every audience's), under -race three of the whole table's.
		keys := map[string]bool{}
		auds := []string{"all", "humans", "bots"}
		if raceOn {
			auds = auds[:1]
		}
		for _, rng := range []string{"7d", "30d", "12mo"} {
			for _, aud := range auds {
				_, body := r.getFrom(t, SourceRaw, "visitors", p(feedSite, "range", rng, "tz", "UTC", "aud", aud,
					"limit", "3"))
				for _, row := range listRows(body) {
					keys[row["key"].(string)] = true
				}
			}
		}
		asked := 0
		for _, key := range slices.Sorted(maps.Keys(keys)) {
			if raceOn && asked == 3 {
				break
			}
			asked++
			for _, rng := range []string{"30d", "12mo"} {
				params := p(feedSite, "range", rng, "tz", "UTC", "key", key)
				raw, got := both(t, "visitor", params)
				agree(t, "visitor "+fmt.Sprint(params), raw, got, nil)
			}
		}
	})
}

// A session crossing midnight from an aggregate day into a raw one is one visit, in the table and the detail. One
// session and one address-day with rows of both parts (people and automated) count once per part under aud=all from
// the aggregates, once from raw: the aggregates keep the parts apart.
func TestSources_Sessions(t *testing.T) {
	r, insert := exactRig(t)
	view := func(ts int64, sid string, src string, cls int) store.Event {
		return store.Event{TS: ts, Source: src, Kind: "page_view", Visitor: "v1", SID: sid, IP: "192.0.2.40",
			IPKey: "192.0.2.40", Template: "/", Cls: cls}
	}
	insert(view(at("2026-11-12T23:50:00Z"), "s1", "browser", store.ClsHuman),
		view(at("2026-11-13T00:10:00Z"), "s1", "browser", store.ClsHuman),
		view(at("2026-11-13T10:00:00Z"), "s2", "browser", store.ClsHuman),
		view(at("2026-11-14T09:00:00Z"), "s3", "browser", store.ClsHuman),
		view(at("2026-11-14T09:01:00Z"), "s3", "server", store.ClsUnknown))
	r.build(t)
	site, _ := r.s.Sites.Lookup(feedSite)
	// The 13th is raw again (unbuilt), the 12th stays built.
	if _, err := r.s.DB.DB.Exec(`DELETE FROM agg_day WHERE site_id = ? AND day = ?`, site.ID,
		at("2026-11-13T00:00:00Z")/dayMS); err != nil {
		t.Fatal(err)
	}
	visitsVia := func(r *rig, src Source, aud string) (float64, float64) {
		t.Helper()
		_, v := r.getFrom(t, src, "visitors", p(feedSite, "range", "30d", "tz", "UTC", "aud", aud))
		var table float64
		for _, row := range listRows(v) {
			if row["key"] == "192.0.2.40" {
				table = num64(row["visits"])
			}
		}
		_, d := r.getFrom(t, src, "visitor", p(feedSite, "range", "30d", "tz", "UTC", "key", "192.0.2.40"))
		return table, num64(d["visits"])
	}
	visits := func(src Source, aud string) (float64, float64) { return visitsVia(r, src, aud) }
	var table float64
	n := countPieces(r, func(r *rig) { table, _ = visitsVia(r, SourceAuto, "humans") })
	if table != 3 || n.agg == 0 || n.raw == 0 {
		t.Errorf("humans across the raw boundary: %v visits, pieces %+v", table, n)
	}
	if table, _ := visits(SourceRaw, "all"); table != 3 {
		t.Errorf("raw, all: %v visits", table)
	}
	if table, _ := visits(SourceAuto, "all"); table != 4 {
		t.Errorf("aggregates, all: %v visits, want s3 once per part", table)
	}
	a, err := r.s.actorIn(t.Context(), &query{site: site, aud: "all", p: newPeriod("30d", "off", r.end, time.UTC)},
		"192.0.2.40", time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC), false)
	if err != nil || a.visits != 2 || a.views != 3 {
		t.Errorf("detail across the boundary: %+v, %v", a, err)
	}
}

// A span starting off a UTC midnight (7d in Zurich starts 23:00 UTC) reads its first whole day raw under every
// source: a session carried into that day from before the span is one visit, not none. 24h never holds a whole
// UTC day, so its visitors are raw under every source.
func TestSources_HeadCut(t *testing.T) {
	t.Parallel()
	r, insert := exactRig(t)
	view := func(ts int64, sid string) store.Event {
		return store.Event{TS: ts, Source: "browser", Kind: "page_view", Visitor: "v1", SID: sid, IP: "192.0.2.41",
			IPKey: "192.0.2.41", Template: "/", Cls: store.ClsHuman}
	}
	insert(view(at("2026-11-15T22:30:00Z"), "s1"), view(at("2026-11-16T00:10:00Z"), "s1"),
		view(at("2026-11-17T10:00:00Z"), "s2"), view(at("2026-11-22T12:00:00Z"), "s3"))
	r.build(t)
	site, _ := r.s.Sites.Lookup(feedSite)
	for _, tc := range []struct {
		rng, tz string
		visits  float64
	}{{"7d", "Europe/Zurich", 3}, {"7d", "UTC", 3}, {"24h", "UTC", 1}, {"24h", "Europe/Zurich", 1}} {
		for _, src := range []Source{SourceRaw, SourceAgg} {
			var table, detail float64
			n := countPieces(r, func(r *rig) {
				_, v := r.getFrom(t, src, "visitors", p(feedSite, "range", tc.rng, "tz", tc.tz))
				for _, row := range listRows(v) {
					if row["key"] == "192.0.2.41" {
						table = num64(row["visits"])
					}
				}
				// The detail's sums (its answer shows no visits).
				loc, _ := time.LoadLocation(tc.tz)
				q := &query{site: site, aud: "all", p: newPeriod(tc.rng, "off", r.s.now(), loc)}
				if a, err := r.server(src).actorOne(t.Context(), q, "192.0.2.41"); err != nil || a == nil {
					t.Errorf("%s %s %v: detail %v, %v", tc.rng, tc.tz, src, a, err)
				} else {
					detail = float64(a.visits)
				}
			})
			if table != tc.visits || detail != tc.visits {
				t.Errorf("%s %s %v: %v visits in the table, %v in the detail, want %v", tc.rng, tc.tz, src, table,
					detail, tc.visits)
			}
			if src == SourceAgg && (n.raw != 0 || (tc.rng == "24h") != (n.agg == 0)) {
				t.Errorf("%s %s: pieces %+v", tc.rng, tc.tz, n)
			}
		}
	}
}

// Where raw is gone (the size cap cut it, the aggregates kept), the sums still come from the aggregates, while the
// trail, the log and a reading reach back to detail_since only; the footer says since when figures go back. (Age
// retention takes the per-address rows with the raw.)
func TestSources_RawSpan(t *testing.T) {
	t.Parallel()
	r := copyRig(t, &fakeModel{})
	code, before := r.get(t, "visitors", p(feedSite, "range", "30d", "tz", "UTC", "limit", "500"))
	if code != 200 {
		t.Fatalf("visitors before the cut: %d %v", code, before)
	}
	addrRows := func() (n int) {
		_ = r.s.DB.DB.QueryRow(`SELECT COUNT(*) FROM agg_addr`).Scan(&n)
		return n
	}
	addrs := addrRows()
	used, _ := r.s.DB.UsedBytes(t.Context())
	if _, err := r.s.DB.EnforceCap(t.Context(), store.RetentionOptions{MaxBytes: used * 95 / 100, Builder: agg.Version,
		Log: slog.New(slog.DiscardHandler)}, r.end); err != nil {
		t.Fatal(err)
	}
	if addrRows() != addrs {
		t.Fatal("the cap cut per-address rows: the history is too small for the test")
	}
	// A fresh server: the first answer's table is cached for its cursor.
	r = rigOver(t, r.s.DB, r.world, &fakeModel{})
	code, after := r.get(t, "visitors", p(feedSite, "range", "30d", "tz", "UTC", "limit", "500"))
	if code != 200 {
		t.Fatalf("visitors after the cut: %d %v", code, after)
	}
	detailSince := after["detail_since"]
	since, _ := time.Parse(time.RFC3339, detailSince.(string))
	if !since.After(r.end.AddDate(0, 0, -19)) {
		t.Fatalf("the cap cut no raw: detail_since %v", detailSince)
	}
	delete(before, "detail_since")
	delete(after, "detail_since")
	rows := func(b map[string]any) []map[string]any {
		out := listRows(b)
		for _, row := range out {
			delete(row, "does") // what it does reads the raw span
		}
		return out
	}
	agree(t, "visitors after the cut", rows(before), rows(after), nil)
	key := listRows(after)[0]["key"].(string)
	_, d := r.get(t, "visitor", p(feedSite, "range", "30d", "tz", "UTC", "key", key))
	for _, x := range d["trail"].([]any) {
		if ts, _ := time.Parse(time.RFC3339, x.(map[string]any)["from"].(string)); ts.Before(since) {
			t.Errorf("trail before detail_since: %v", x)
		}
	}
	if d["detail_since"] != detailSince || d["requests"] != listRows(after)[0]["requests"] {
		t.Errorf("detail %v %v", d["detail_since"], d["requests"])
	}
	_, l := r.get(t, "log", p(feedSite, "range", "30d", "tz", "UTC"))
	if l["detail_since"] != detailSince {
		t.Errorf("log detail_since %v", l["detail_since"])
	}
	// A template's paths are raw: their answer says since when, the template list itself covers the range.
	_, pages := r.get(t, "list", p(feedSite, "range", "30d", "tz", "UTC", "dim", "pages"))
	if _, ok := pages["detail_since"]; ok || len(listRows(pages)) == 0 {
		t.Fatalf("pages %v", pages)
	}
	_, in := r.get(t, "list", p(feedSite, "range", "30d", "tz", "UTC", "dim", "pages", "within",
		listRows(pages)[0]["key"].(string)))
	if in["detail_since"] != detailSince || len(listRows(in)) == 0 {
		t.Errorf("within detail_since %v, %d rows", in["detail_since"], len(listRows(in)))
	}
	_, dom := r.get(t, "domain", p(feedSite, "range", "30d", "tz", "UTC"))
	st, src := dom["storage"].(map[string]any), dom["sources"].(map[string]any)
	if st["oldest"] != detailSince || st["figures_since"] == nil ||
		st["figures_since"].(string) >= st["oldest"].(string) || src["first"] != st["figures_since"] ||
		num64(st["raw_days"]) < 750 {
		t.Errorf("storage %v, first %v", st, src["first"])
	}
}

// Cut to three entries a day, the aggregates still add up: rows and Other hold the raw total of a counted list, a
// key never collapsed has its raw figures and change, and a key that may hide in a day's Other has no change, never
// new. Four days of the history come again 30, 90 and 365 days earlier: the prior periods of 30d and 90d and the
// year before hold data, some of it collapsed.
func TestSources_ListsTopThree(t *testing.T) {
	t.Parallel()
	skipRace(t)
	r := copyRig(t, nil)
	for _, tbl := range []string{"agg_day", "agg_hour", "agg_list", "agg_addr"} {
		if _, err := r.s.DB.DB.Exec(`DELETE FROM ` + tbl); err != nil { //nolint:gosec // the test's own names
			t.Fatal(err)
		}
	}
	from, to := r.end.AddDate(0, 0, -8).Truncate(24*time.Hour), r.end.AddDate(0, 0, -4).Truncate(24*time.Hour)
	for _, back := range []int{30, 90, 365} {
		if _, err := r.s.DB.DB.Exec(`INSERT INTO event (site_id, ts, source, kind, name, visitor, sid, ip, ip_key, ua,
			path, template, url, ref, ref_host, method, status, dur_ms, asn, country, account, op, err, unrouted, cls, props)
			SELECT site_id, ts - ?, source, kind, name, visitor, sid, ip, ip_key, ua, path, template, url, ref, ref_host,
			method, status, dur_ms, asn, country, account, op, err, unrouted, cls, props FROM event WHERE ts >= ? AND ts < ?`,
			int64(back)*dayMS, from.UnixMilli(), to.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := agg.BuildThrough(t.Context(), r.s.DB, r.end, 3); err != nil {
		t.Fatal(err)
	}
	site, _ := r.s.Sites.Lookup(feedSite)
	// The hashes of every key some day collapsed, by dimension.
	collapsed := map[string]map[uint64]bool{}
	rows, err := r.s.DB.DB.Query(`SELECT dim, keys FROM agg_list WHERE site_id = ? AND other = 1`, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	truncated := 0
	for rows.Next() {
		var dim string
		var keys []byte
		if err := rows.Scan(&dim, &keys); err != nil {
			t.Fatal(err)
		}
		if collapsed[dim] == nil {
			collapsed[dim] = map[uint64]bool{}
		}
		for _, h := range agg.OtherKeys(keys) {
			collapsed[dim][h] = true
			truncated++
		}
	}
	_ = rows.Close()
	if truncated == 0 {
		t.Fatal("nothing collapsed at three entries a day")
	}
	byKey := func(body map[string]any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, row := range listRows(body) {
			out[row["key"].(string)] = row
		}
		return out
	}
	var priorOthers int
	if err := r.s.DB.DB.QueryRow(`SELECT COUNT(*) FROM agg_list WHERE site_id = ? AND other = 1 AND day < ?`,
		site.ID, r.end.AddDate(0, 0, -30).UnixMilli()/dayMS).Scan(&priorOthers); err != nil || priorOthers == 0 {
		t.Fatalf("no Other row in a prior period: %d, %v", priorOthers, err)
	}
	compared := 0 // changes from the aggregates against prior data
	combos := [][2]string{{"30d", "prev"}, {"90d", "prev"}, {"30d", "yoy"}, {"90d", "yoy"}}
	for _, rc := range combos {
		rng := rc[0] + " " + rc[1]
		for dim, d := range dims {
			if dim == "refused" {
				continue
			}
			params := p(feedSite, "range", rc[0], "cmp", rc[1], "tz", "UTC", "dim", dim, "limit", "500")
			raw, got := r.both(t, "list", params)
			rawRows, gotRows := byKey(raw), byKey(got)
			for k, g := range gotRows {
				w, ok := rawRows[k]
				cut := collapsed[dim][agg.Hash(k)]
				switch {
				case !ok:
					t.Errorf("%s %s: %s is no raw key", rng, dim, k)
				case !cut && (num64(g["value"]) != num64(w["value"]) || num64(g["second"]) != num64(w["second"])):
					t.Errorf("%s %s %s, never collapsed: %v, raw %v", rng, dim, k, g, w)
				case !cut && (changeOf(g["change"]) != changeOf(w["change"]) || g["verdict"] != w["verdict"]):
					t.Errorf("%s %s %s, never collapsed: change %v %v, raw %v %v", rng, dim, k, g["change"],
						g["verdict"], w["change"], w["verdict"])
				case cut && g["change"] != nil && changeOf(g["change"]) != changeOf(w["change"]):
					t.Errorf("%s %s %s, collapsed: change %v, raw %v", rng, dim, k, g["change"], w["change"])
				}
				if c, ok := g["change"].(map[string]any); ok && c["pct"] != nil {
					compared++
				}
				if changeOf(g["change"]) == `{"new":true}` && changeOf(w["change"]) != `{"new":true}` {
					t.Errorf("%s %s %s: new from the aggregates, raw %v", rng, dim, k, w["change"])
				}
			}
			total := func(body map[string]any) float64 {
				sum := num64(body["other"].(map[string]any)["value"])
				for _, row := range listRows(body) {
					sum += num64(row["value"])
				}
				return sum
			}
			if d.distinct == "" || d.value(1, 0, 0) == 1 { // value is the counted a
				if total(raw) != total(got) {
					t.Errorf("%s %s: rows and Other %v, raw %v", rng, dim, total(got), total(raw))
				}
			}
			// Every key, shown or only in an Other's listed hashes: raw's count (no day past the listing bound).
			if num64(got["total"]) != num64(raw["total"]) {
				t.Errorf("%s %s: total %v, raw %v", rng, dim, got["total"], raw["total"])
			}
		}
	}
	if compared == 0 {
		t.Error("no change from the aggregates compared prior data")
	}
	// No finding of a new key the aggregates cannot vouch for.
	for _, rc := range combos {
		rng := rc[0] + " " + rc[1]
		params := p(feedSite, "range", rc[0], "cmp", rc[1], "tz", "UTC", "findings", "all")
		raw, got := r.both(t, "noteworthy", params)
		rawIDs := map[string]bool{}
		for _, x := range raw["items"].([]any) {
			rawIDs[x.(map[string]any)["id"].(string)] = true
		}
		for _, x := range got["items"].([]any) {
			if id := x.(map[string]any)["id"].(string); !rawIDs[id] && !strings.HasPrefix(id, "trend:") {
				t.Errorf("%s: finding %s only from the aggregates", rng, id)
			}
		}
	}
}

// A backfill cut off midway (the oldest site-days built, one left under way) and then resumed changes no answer at
// 90d or 12mo: auto reads raw wherever a day is not built and clean.
func TestSources_Backfill(t *testing.T) {
	t.Parallel()
	r := copyRig(t, nil)
	for _, tbl := range []string{"agg_day", "agg_hour", "agg_list", "agg_addr"} {
		if _, err := r.s.DB.DB.Exec(`DELETE FROM ` + tbl); err != nil { //nolint:gosec // the test's own names
			t.Fatal(err)
		}
	}
	asks := []struct {
		ep     string
		params map[string]string
		loose  func(string, any) bool
	}{
		{"overview", p(feedSite, "range", "90d", "tz", "UTC"), looseTiles},
		{"overview", p(feedSite, "range", "12mo", "cmp", "yoy", "tz", "UTC"), looseTiles},
		{"classes", p(feedSite, "range", "12mo", "tz", "UTC"), looseTiles},
		{"list", p(feedSite, "range", "90d", "tz", "UTC", "dim", "pages", "limit", "500"), looseRows("pages")},
		{"list", p(feedSite, "range", "12mo", "tz", "UTC", "dim", "probed", "limit", "500"), looseRows("probed")},
		{"visitors", p(feedSite, "range", "90d", "tz", "UTC", "limit", "7"), nil},
	}
	answers := func(stage string) ([]map[string]any, pieceCount) {
		var out []map[string]any
		n := countPieces(r, func(r *rig) {
			for _, a := range asks {
				code, body := r.getFrom(t, SourceAuto, a.ep, a.params)
				if code != http.StatusOK {
					t.Fatalf("%s %s %v: %d %v", stage, a.ep, a.params, code, body)
				}
				out = append(out, body)
			}
		})
		return out, n
	}
	before, n := answers("unbuilt")
	if n.agg != 0 {
		t.Fatalf("unbuilt: pieces %+v", n)
	}
	// The oldest half of the site-days built, the next one cut off between its first and last transaction.
	rows, err := r.s.DB.DB.Query(`SELECT DISTINCT site_id, ts / 86400000 FROM event ORDER BY 2, 1`)
	if err != nil {
		t.Fatal(err)
	}
	var sds [][2]int64
	for rows.Next() {
		var sd [2]int64
		if err := rows.Scan(&sd[0], &sd[1]); err != nil {
			t.Fatal(err)
		}
		sds = append(sds, sd)
	}
	_ = rows.Close()
	b := &agg.Builder{Store: r.s.DB}
	for _, sd := range sds[:len(sds)/2] {
		if _, err := b.BuildDay(t.Context(), sd[0], sd[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.s.DB.BeginAggDay(t.Context(), sds[len(sds)/2][0], sds[len(sds)/2][1]); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.DB.DB.Exec(`UPDATE agg_day SET dirty = ? WHERE site_id = ? AND day = ?`, store.AggWriting,
		sds[len(sds)/2][0], sds[len(sds)/2][1]); err != nil {
		t.Fatal(err)
	}
	cut, n := answers("cut off")
	if n.agg == 0 || n.raw == 0 {
		t.Errorf("cut off: pieces %+v", n)
	}
	r.build(t)
	after, n := answers("resumed")
	if n.agg == 0 {
		t.Errorf("resumed: pieces %+v", n)
	}
	for i, a := range asks {
		where := fmt.Sprint(a.ep, a.params)
		agree(t, "cut off "+where, before[i], cut[i], a.loose)
		agree(t, "resumed "+where, before[i], after[i], a.loose)
	}
}

// The source of each day: auto reads 24h and 7d raw, every clean day of a longer range from the aggregates, a dirty
// or unbuilt day raw, and nothing before the first event; SourceAgg fails on a day with raw but no clean build.
func TestSources_Plan(t *testing.T) {
	r, insert := exactRig(t)
	insert(pageLoad(at("2026-11-02T10:00:00Z"), "a", "192.0.2.1", "CH", "Mozilla/5.0", 0)...)
	insert(pageLoad(at("2026-11-12T10:00:00Z"), "b", "192.0.2.2", "CH", "Mozilla/5.0", 0)...)
	r.build(t)
	visitors := func(src Source, rng string) (float64, pieceCount) {
		t.Helper()
		var body map[string]any
		var code int
		n := countPieces(r, func(r *rig) {
			code, body = r.getFrom(t, src, "overview", p(feedSite, "range", rng, "tz", "UTC"))
		})
		if code != http.StatusOK {
			t.Fatalf("%v %s: %d %v", src, rng, code, body)
		}
		return num64(tiles(t, body)["visitors"]["value"]), n
	}
	if v, n := visitors(SourceAuto, "7d"); v != 0 || n.agg != 0 || n.raw == 0 {
		t.Errorf("auto 7d: %v visitors, pieces %+v", v, n)
	}
	v, n := visitors(SourceAuto, "30d")
	// The 21 built days from 2 to 22 November; 24 October to 1 November, before the first event, are not read.
	if v != 2 || n.agg != 21 || n.raw != 0 {
		t.Errorf("auto 30d: %v visitors, pieces %+v", v, n)
	}
	// A write to a built day shows at once, read raw, and again once rebuilt.
	insert(pageLoad(at("2026-11-12T11:00:00Z"), "c", "192.0.2.3", "CH", "Mozilla/5.0", 0)...)
	if v, n := visitors(SourceAuto, "30d"); v != 3 || n.raw != 1 {
		t.Errorf("after the write: %v visitors, pieces %+v", v, n)
	}
	if code, body := r.getFrom(t, SourceAgg, "overview", p(feedSite, "range", "30d", "tz", "UTC")); code != 500 {
		t.Errorf("SourceAgg on a dirty day: %d %v", code, body)
	}
	r.build(t)
	if v, n := visitors(SourceAuto, "30d"); v != 3 || n.raw != 0 {
		t.Errorf("after the rebuild: %v visitors, pieces %+v", v, n)
	}
	// A day whose raw retention cut (raw = 0) is read from its build, or as nothing when it has none.
	site, _ := r.s.Sites.Lookup(feedSite)
	if _, err := r.s.DB.DB.Exec(`UPDATE agg_day SET raw = 0 WHERE site_id = ? AND day = ?`, site.ID,
		at("2026-11-12T00:00:00Z")/dayMS); err != nil {
		t.Fatal(err)
	}
	if v, _ := visitors(SourceRaw, "30d"); v != 3 {
		t.Errorf("raw source: %v", v)
	}
	if _, err := r.s.DB.DB.Exec(`UPDATE agg_day SET frozen = 0 WHERE site_id = ? AND day = ?`, site.ID,
		at("2026-11-12T00:00:00Z")/dayMS); err != nil {
		t.Fatal(err)
	}
	if v, _ := visitors(SourceAuto, "30d"); v != 1 {
		t.Errorf("a cut day without a build: %v visitors, want only a's", v)
	}
	if v, _ := visitors(SourceAgg, "30d"); v != 1 {
		t.Errorf("SourceAgg on a cut day without a build: %v", v)
	}
}

// A rebuild under way (AggWriting between its first and last transaction) is not read: the statement's own snapshot
// sees the day out of reading and the piece goes raw.
func TestSources_RebuildWhileRead(t *testing.T) {
	r, insert := exactRig(t)
	insert(pageLoad(at("2026-11-12T10:00:00Z"), "a", "192.0.2.1", "CH", "Mozilla/5.0", 0)...)
	r.build(t)
	site, _ := r.s.Sites.Lookup(feedSite)
	day := at("2026-11-12T00:00:00Z") / dayMS
	q := &query{site: site, aud: "all", p: newPeriod("30d", "off", r.end, time.UTC)}
	pl, err := r.s.planOf(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	// The plan says built and clean; the job takes the day out and deletes its rows before the read.
	if _, err := r.s.DB.DB.Exec(`UPDATE agg_day SET dirty = ? WHERE site_id = ? AND day = ?`, store.AggWriting,
		site.ID, day); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.DB.DB.Exec(`DELETE FROM agg_hour WHERE site_id = ?`, site.ID); err != nil {
		t.Fatal(err)
	}
	// In two slots, the event in the first.
	bounds := []time.Time{time.UnixMilli(day * dayMS), time.UnixMilli(day*dayMS + dayMS/2), time.UnixMilli((day + 1) * dayMS)}
	b, err := r.s.slots(t.Context(), pl, "all", bounds, false)
	if err != nil || b[0].views != 1 || b[0].visitors.count() != 1 || b[1].views != 0 {
		t.Errorf("a day rebuilt while read: %+v, %v", b, err)
	}
	pl.src = SourceAgg
	if _, err := r.s.slots(t.Context(), pl, "all", bounds, false); !errors.Is(err, ErrNoAggregates) {
		t.Errorf("SourceAgg on a day rebuilt while read: %v", err)
	}
}

// uniq is exact while it holds ids only, and merges a stored sketch.
func TestUniq(t *testing.T) {
	var u uniq
	for i := range 100 {
		u.add(fmt.Sprint("v", i%60))
	}
	if u.count() != 60 || u.sk != nil {
		t.Errorf("exact: %d", u.count())
	}
	s := agg.NewSketch()
	for i := 40; i < 140; i++ {
		s.AddString(fmt.Sprint("v", i))
	}
	if err := u.addSketch(s.Marshal()); err != nil || u.count() != 140 {
		t.Errorf("with a sketch: %d, %v", u.count(), err)
	}
	var w uniq
	w.add("v1")
	w.merge(&u)
	if w.count() != 140 || w.sk == nil {
		t.Errorf("merged: %d", w.count())
	}
	if err := w.addSketch([]byte{9}); err == nil {
		t.Error("a bad sketch merged")
	}
}

// detail_since says where raw begins only when something is missing, decided on the reads' UTC days: a site whose
// figures begin where its raw does says nothing, nor a range whose first UTC day raw holds (Kolkata, its local
// start the evening before in UTC); a raw start after that day is said (New York, though it is still the range's
// first local date).
func TestSources_DetailSince(t *testing.T) {
	r, insert := exactRig(t)
	view := func(ts int64) store.Event {
		return store.Event{TS: ts, Source: "browser", Kind: "page_view", Visitor: "v1", SID: "s1",
			IP: "192.0.2.90", IPKey: "192.0.2.90", Template: "/", Path: "/", Cls: store.ClsHuman}
	}
	insert(view(at("2026-10-20T10:00:00Z")), view(at("2026-10-24T05:00:00Z")), view(at("2026-10-25T00:30:00Z")),
		view(at("2026-11-10T10:00:00Z")), view(at("2026-11-20T10:00:00Z")))
	r.build(t)
	since := func(rng, tz string) any {
		t.Helper()
		var out []any
		for _, ask := range [][]string{{"visitors"}, {"log"}, {"visitor", "key", "192.0.2.90"},
			{"list", "dim", "pages", "within", "/"}} {
			code, body := r.get(t, ask[0], p(feedSite, append([]string{"range", rng, "tz", tz}, ask[1:]...)...))
			if code != 200 {
				t.Fatalf("%s %s %v: %d %v", rng, tz, ask, code, body)
			}
			out = append(out, body["detail_since"])
		}
		for _, x := range out[1:] {
			if x != out[0] {
				t.Errorf("%s %s: detail_since differs between endpoints: %v", rng, tz, out)
			}
		}
		return out[0]
	}
	if s := since("90d", "UTC"); s != nil {
		t.Errorf("young site: detail_since %v", s)
	}
	cut := func(day string) {
		t.Helper()
		if _, err := r.s.DB.PruneAge(t.Context(), time.UnixMilli(at(day))); err != nil {
			t.Fatal(err)
		}
	}
	cut("2026-10-24T00:00:00Z")
	if s := since("90d", "UTC"); s != "2026-10-24T05:00:00Z" {
		t.Errorf("cut, 90d: detail_since %v", s)
	}
	if s := since("30d", "Asia/Kolkata"); s != nil {
		t.Errorf("Kolkata 30d, raw from its first UTC day: detail_since %v", s)
	}
	cut("2026-10-25T00:00:00Z")
	if s := since("30d", "America/New_York"); s != "2026-10-25T00:30:00Z" {
		t.Errorf("New York 30d, raw from its second UTC day: detail_since %v", s)
	}
	if s := since("7d", "UTC"); s != nil {
		t.Errorf("7d after the cut: detail_since %v", s)
	}
}

// A day the size cap cut aggregate rows of (marked unbuilt with partial rows) has no figure anywhere: not its hours
// left in the tiles, not an address in the table; the footer's figures begin on the next day. A site without raw
// says raw begins now.
func TestSources_CapCutDay(t *testing.T) {
	r, insert := exactRig(t)
	view := func(ts int64, ip string) store.Event {
		return store.Event{TS: ts, Source: "browser", Kind: "page_view", Visitor: ip, SID: ip, IP: ip, IPKey: ip,
			Template: "/", Path: "/", Cls: store.ClsHuman}
	}
	insert(view(at("2026-11-10T08:00:00Z"), "192.0.2.80"), view(at("2026-11-11T10:00:00Z"), "192.0.2.81"))
	r.build(t)
	// As the cap leaves them: raw gone from both days, the 10th's per-address rows cut, its hours and lists not yet.
	for _, q := range []string{`DELETE FROM event`, `UPDATE agg_day SET raw = 0`,
		`UPDATE agg_day SET frozen = 0, dirty = 3 WHERE day = ` + fmt.Sprint(at("2026-11-10T00:00:00Z")/dayMS),
		`DELETE FROM agg_addr WHERE day = ` + fmt.Sprint(at("2026-11-10T00:00:00Z")/dayMS)} {
		if _, err := r.s.DB.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	params := p(feedSite, "range", "30d", "tz", "UTC")
	for _, src := range []Source{SourceAgg, SourceAuto} {
		code, ov := r.getFrom(t, src, "overview", params)
		var views float64 = -1
		for _, x := range ov["tiles"].([]any) {
			if m := x.(map[string]any); m["key"] == "views" {
				views = num64(m["value"])
			}
		}
		_, vis := r.getFrom(t, src, "visitors", params)
		var keys []string
		for _, row := range listRows(vis) {
			keys = append(keys, row["key"].(string))
		}
		if code != 200 || views != 1 || !slices.Equal(keys, []string{"192.0.2.81"}) {
			t.Errorf("%v: %d, %v views, visitors %v; want the 11th's only", src, code, views, keys)
		}
		for _, ask := range [][]string{{"visitors"}, {"log"}, {"visitor", "key", "192.0.2.81"},
			{"list", "dim", "pages", "within", "/"}} {
			code, body := r.getFrom(t, src, ask[0], p(feedSite, append([]string{"range", "30d", "tz", "UTC"}, ask[1:]...)...))
			if code != 200 || body["detail_since"] != iso(r.s.now()) {
				t.Errorf("%v %v: %d, detail_since %v without raw, want now", src, ask, code, body["detail_since"])
			}
		}
	}
	_, dom := r.get(t, "domain", params)
	if st := dom["storage"].(map[string]any); st["figures_since"] != "2026-11-11T10:00:00Z" || st["oldest"] != nil {
		t.Errorf("storage %v", st)
	}
}

// A built day age retention cut (raw and per-address rows gone) keeps its tiles, classes, lists and the footer's
// figures from the aggregates; it stays frozen and clean, and the job builds nothing. Its addresses are simply gone:
// no row, no error, no raw read in their place. A session crossing into the next day is a visit there: the cut day
// counted no address.
func TestSources_AgeCutDay(t *testing.T) {
	r, insert := exactRig(t)
	ev := func(ts int64, ip, sid string) store.Event {
		return store.Event{TS: ts, Source: "browser", Kind: "page_view", Visitor: ip, SID: sid, IP: ip, IPKey: ip,
			Template: "/", Path: "/", Country: "CH", Cls: store.ClsHuman}
	}
	insert(ev(at("2026-10-10T10:00:00Z"), "192.0.2.70", "a1"), ev(at("2026-10-10T23:50:00Z"), "192.0.2.71", "b1"),
		ev(at("2026-10-11T00:10:00Z"), "192.0.2.71", "b1"), ev(at("2026-11-10T10:00:00Z"), "192.0.2.72", "c1"))
	r.build(t)
	site, _ := r.s.Sites.Lookup(feedSite)
	day := at("2026-10-10T00:00:00Z") / dayMS
	params := p(feedSite, "range", "90d", "tz", "UTC")
	figures := func() ([]map[string]any, pieceCount) {
		t.Helper()
		var out []map[string]any
		n := countPieces(r, func(r *rig) {
			for _, ask := range [][]string{{"overview"}, {"classes"}, {"list", "dim", "pages"},
				{"list", "dim", "countries"}} {
				code, body := r.getFrom(t, SourceAuto, ask[0], p(feedSite, append([]string{"range", "90d", "tz", "UTC"},
					ask[1:]...)...))
				if code != 200 {
					t.Fatalf("%v: %d %v", ask, code, body)
				}
				delete(body, "detail_since")
				out = append(out, body)
			}
		})
		_, dom := r.get(t, "domain", params)
		out = append(out, map[string]any{"figures_since": dom["storage"].(map[string]any)["figures_since"]})
		return out, n
	}
	before, _ := figures()
	if _, err := r.s.DB.PruneAge(t.Context(), time.UnixMilli(at("2026-10-11T00:00:00Z"))); err != nil {
		t.Fatal(err)
	}
	var addrs int
	if err := r.s.DB.DB.QueryRow(`SELECT COUNT(*) FROM agg_addr WHERE day = ?`, day).Scan(&addrs); err != nil ||
		addrs != 0 {
		t.Fatalf("per-address rows of the cut day: %d, %v", addrs, err)
	}
	state := func() store.AggDay {
		t.Helper()
		days, err := r.s.DB.AggDays(t.Context(), site.ID, day, day)
		if err != nil {
			t.Fatal(err)
		}
		return days[day]
	}
	if d := state(); !d.Frozen || d.Dirty != store.AggClean || d.Raw {
		t.Fatalf("cut day: %+v, want frozen, clean, without raw", d)
	}
	if n, err := agg.BuildThrough(t.Context(), r.s.DB, r.end, agg.TopK); err != nil || n != 0 {
		t.Errorf("job after the cut built %d, %v", n, err)
	}
	if d := state(); !d.Frozen || d.Dirty != store.AggClean {
		t.Errorf("cut day after the job: %+v", d)
	}
	after, n := figures()
	if n.agg == 0 {
		t.Errorf("pieces %+v: nothing from the aggregates", n)
	}
	if before[len(before)-1]["figures_since"] != "2026-10-10T10:00:00Z" {
		t.Errorf("figures_since before the cut: %v", before[len(before)-1])
	}
	for i := range before {
		agree(t, fmt.Sprint("after the cut ", i), before[i], after[i], nil)
	}
	for _, src := range []Source{SourceAuto, SourceAgg} {
		for _, aud := range []string{"all", "humans"} {
			code, vis := r.getFrom(t, src, "visitors", p(feedSite, "range", "90d", "tz", "UTC", "aud", aud))
			got := map[string]float64{}
			for _, row := range listRows(vis) {
				got[row["key"].(string)] = num64(row["visits"])
			}
			if code != 200 || len(got) != 2 || got["192.0.2.71"] != 1 || got["192.0.2.72"] != 1 {
				t.Errorf("%v %s visitors: %d %v, want .71 and .72 with a visit each", src, aud, code, got)
			}
			q := &query{site: site, aud: aud, p: newPeriod("90d", "off", r.s.now(), time.UTC)}
			if a, err := r.server(src).actorOne(t.Context(), q, "192.0.2.71"); err != nil || a == nil || a.visits != 1 {
				t.Errorf("%v %s detail of .71: %+v %v, want 1 visit", src, aud, a, err)
			}
		}
		for _, key := range []string{"192.0.2.70", "192.0.2.99"} {
			if code, body := r.getFrom(t, src, "visitor", p(feedSite, "range", "90d", "tz", "UTC", "key", key)); code !=
				http.StatusNotFound {
				t.Errorf("%v visitor %s: %d %v, want 404", src, key, code, body)
			}
		}
		code, ref := r.getFrom(t, src, "list", p(feedSite, "range", "90d", "tz", "UTC", "dim", "refused"))
		for _, row := range listRows(ref) {
			if row["key"] == "192.0.2.70" {
				t.Errorf("%v refused lists the cut day's address", src)
			}
		}
		if code != 200 {
			t.Errorf("%v refused: %d %v", src, code, ref)
		}
		if code, body := r.getFrom(t, src, "noteworthy", p(feedSite, "range", "90d", "tz", "UTC", "findings", "all")); code != 200 ||
			strings.Contains(fmt.Sprint(body), "192.0.2.70") {
			t.Errorf("%v noteworthy: %d %v", src, code, body)
		}
	}
	// A raw write makes the day after dirty: read raw, it subtracts no session carried from the cut day either.
	insert(ev(at("2026-10-11T12:00:00Z"), "192.0.2.73", "d1"))
	_, vis := r.getFrom(t, SourceAuto, "visitors", params)
	q := &query{site: site, aud: "all", p: newPeriod("90d", "off", r.s.now(), time.UTC)}
	for _, row := range listRows(vis) {
		if row["key"] == "192.0.2.71" && num64(row["visits"]) != 1 {
			t.Errorf("raw day after the cut: .71 has %v visits in the table, want 1", row["visits"])
		}
	}
	if a, err := r.server(SourceAuto).actorOne(t.Context(), q, "192.0.2.71"); err != nil || a == nil || a.visits != 1 {
		t.Errorf("raw day after the cut: detail of .71 %+v %v, want 1 visit", a, err)
	}
}

// buildK rebuilds every day of the rig at k entries a list part.
func (r *rig) buildK(t *testing.T, k int) {
	t.Helper()
	if _, err := agg.BuildThrough(t.Context(), r.s.DB, r.end, k); err != nil {
		t.Fatal(err)
	}
}

// otherOf is a list answer's Other.
func otherOf(body map[string]any) (count, value float64) {
	o := body["other"].(map[string]any)
	return num64(o["count"]), num64(o["value"])
}

// Other rows at one entry a day: the bots audience counts an Other's addresses, not its browser ids; a sub= filter
// keeps its own Other; a key that may sit in a closed day's Other has no change; past the listing bound Other's
// count is a lower bound that never counts a shown key.
func TestSources_OtherRows(t *testing.T) {
	r, insert := exactRig(t)
	view := func(ts int64, tmpl, vid, ip string, cls int) store.Event {
		return store.Event{TS: ts, Source: "browser", Kind: "page_view", Visitor: vid, SID: vid + "-s", IP: ip,
			IPKey: ip, Path: tmpl, Template: tmpl, Cls: cls}
	}
	act := func(ts int64, kind, name, vid string) store.Event {
		return store.Event{TS: ts, Source: "browser", Kind: kind, Name: name, Visitor: vid, SID: vid + "-s",
			IP: "192.0.2.70", IPKey: "192.0.2.70", Cls: store.ClsHuman}
	}
	nov := func(day, i int) int64 { return at(fmt.Sprintf("2026-11-%02dT10:00:00Z", day)) + int64(i)*1000 }
	var evs []store.Event
	// Bots on the 10th: /a by one address under five ids, /b by three addresses under one id (both kept), /c by one
	// address under four ids, collapsed.
	for i := range 5 {
		evs = append(evs, view(nov(10, i), "/a", fmt.Sprint("a", i), "198.51.100.1", store.ClsBot))
	}
	for i := range 3 {
		evs = append(evs, view(nov(10, 10+i), "/b", "w", fmt.Sprint("198.51.100.", 2+i), store.ClsBot))
	}
	for i := range 4 {
		evs = append(evs, view(nov(10, 20+i), "/c", fmt.Sprint("c", i), "198.51.100.5", store.ClsBot))
	}
	// Events on the 11th: signup kept, share (custom) and two automatic kinds collapsed.
	for i := range 3 {
		evs = append(evs, act(nov(11, i), "custom", "signup", fmt.Sprint("p", i)))
	}
	evs = append(evs, act(nov(11, 5), "custom", "share", "p9"), act(nov(11, 6), "rage_click", "", "p1"),
		act(nov(11, 7), "rage_click", "", "p2"), act(nov(11, 8), "download", "", "p3"))
	// People: /y collapsed on the 12th (/x kept), kept on the 13th, seen before in the prior period.
	for i := range 3 {
		evs = append(evs, view(nov(12, i), "/x", fmt.Sprint("x", i), fmt.Sprint("192.0.2.", 10+i), store.ClsHuman))
	}
	evs = append(evs, view(nov(12, 9), "/y", "y0", "192.0.2.20", store.ClsHuman))
	for i := range 5 {
		evs = append(evs, view(nov(13, i), "/y", fmt.Sprint("y", 1+i), fmt.Sprint("192.0.2.", 21+i), store.ClsHuman))
	}
	for i := range 2 {
		evs = append(evs, view(at("2026-10-01T10:00:00Z")+int64(i), "/y", fmt.Sprint("o", i),
			fmt.Sprint("192.0.2.", 30+i), store.ClsHuman))
	}
	// The 14th: /top kept, five keys collapsed.
	for i := range 3 {
		evs = append(evs, view(nov(14, i), "/top", fmt.Sprint("t", i), fmt.Sprint("192.0.2.", 40+i), store.ClsHuman))
	}
	for i := range 5 {
		evs = append(evs, view(nov(14, 10+i), fmt.Sprint("/k", i), fmt.Sprint("k", i), "192.0.2.50", store.ClsHuman))
	}
	insert(evs...)
	r.buildK(t, 1)
	ask := func(src Source, kv ...string) map[string]any {
		t.Helper()
		code, body := r.getFrom(t, src, "list", p(feedSite, append([]string{"range", "30d", "tz", "UTC",
			"limit", "500"}, kv...)...))
		if code != 200 {
			t.Fatalf("%v: %d %v", kv, code, body)
		}
		return body
	}
	total := func(body map[string]any) float64 {
		_, v := otherOf(body)
		for _, row := range listRows(body) {
			v += num64(row["value"])
		}
		return v
	}

	// The bots audience: /c's one address, not its four ids.
	if c, v := otherOf(ask(SourceAgg, "dim", "pages", "aud", "bots")); c != 1 || v != 1 {
		t.Errorf("bots' Other: %v keys, %v addresses, want 1 and 1", c, v)
	}
	// sub=: each keeps its own Other, the totals are raw's.
	for sub, want := range map[string]float64{"custom": 1, "auto": 2} {
		got, raw := ask(SourceAgg, "dim", "events", "sub", sub), ask(SourceRaw, "dim", "events", "sub", sub)
		if c, _ := otherOf(got); c != want || total(got) != total(raw) {
			t.Errorf("sub=%s: Other %v keys, total %v, raw %v", sub, c, total(got), total(raw))
		}
	}
	// A search: a row's share is of the whole list, the Other rows' value included, as raw has it (signup's 3 of 7).
	got, raw := ask(SourceAgg, "dim", "events", "q", "sign"), ask(SourceRaw, "dim", "events", "q", "sign")
	agree(t, "events q=sign", raw, got, nil)
	if rows := listRows(got); len(rows) != 1 || num64(rows[0]["share"]) != 43 {
		t.Errorf("events q=sign: rows %v, want signup at 43 %%", rows)
	}
	// /y sits in the 12th's Other: its change is not known.
	for _, row := range listRows(ask(SourceAgg, "dim", "pages", "aud", "humans")) {
		if row["key"] == "/y" && row["change"] != nil {
			t.Errorf("/y, collapsed on a closed day: change %v", row["change"])
		}
	}
	// Past the listing bound (keys NULL): the 14th held five keys, three rows are shown, so at least two are hidden;
	// /y, shown, is not counted although the 12th's Other held it.
	if _, err := r.s.DB.DB.Exec(`UPDATE agg_list SET keys = NULL WHERE dim = 'pages' AND other = 1 AND part = 1`); err != nil {
		t.Fatal(err)
	}
	if c, _ := otherOf(ask(SourceAgg, "dim", "pages", "aud", "humans")); c != 2 {
		t.Errorf("unlisted Other: %v keys, want the lower bound 2", c)
	}
}

// A template's paths are raw: with raw cut inside the comparison (7d), no path has a change (never "new"); a
// comparison before the first event (30d) never held any. A path's entry share is of the visits raw still holds.
func TestSources_WithinCut(t *testing.T) {
	t.Parallel()
	r := copyRig(t, nil)
	cut := r.end.AddDate(0, 0, -10)
	if _, err := r.s.DB.PruneAge(t.Context(), cut); err != nil {
		t.Fatal(err)
	}
	site, _ := r.s.Sites.Lookup(feedSite)
	_, pages := r.get(t, "list", p(feedSite, "range", "30d", "tz", "UTC", "dim", "entry"))
	tmpl := listRows(pages)[0]["key"].(string)
	for _, rng := range []string{"7d", "30d"} {
		_, in := r.get(t, "list", p(feedSite, "range", rng, "tz", "UTC", "dim", "entry", "within", tmpl,
			"limit", "500"))
		rows := listRows(in)
		if len(rows) == 0 {
			t.Fatalf("%s: no paths in %s", rng, tmpl)
		}
		news := 0
		for _, row := range rows {
			if changeOf(row["change"]) == `{"new":true}` {
				news++
			}
			if rng == "7d" && row["change"] != nil {
				t.Errorf("%s %s: change %v with the comparison's raw cut", rng, row["key"], row["change"])
			}
		}
		if rng == "7d" {
			continue
		}
		if news == 0 {
			t.Errorf("30d: no path new against a comparison before the first event")
		}
		var all, path float64
		from, to := cut.Truncate(24*time.Hour).UnixMilli(), r.end.Truncate(24*time.Hour).AddDate(0, 0, 1).UnixMilli()
		if err := r.s.DB.DB.QueryRow(`SELECT COUNT(*), SUM(template = ? AND path = ?) FROM event WHERE site_id = ?
			AND ts >= ? AND ts < ? AND kind = 'session_start'`, tmpl, rows[0]["key"], site.ID, from, to).
			Scan(&all, &path); err != nil {
			t.Fatal(err)
		}
		if want := math.Round(path/all*1000) / 10; num64(rows[0]["entry"]) != want {
			t.Errorf("entry share of %s: %v, raw %v", rows[0]["key"], rows[0]["entry"], want)
		}
	}
}

// A session crossing midnight from an aggregate day whose raw is gone into a day read raw (7d reads raw where it is
// whole) is one visit: the raw day's stored rows say which of its sessions the day before had, in every audience.
func TestSources_StitchRawGone(t *testing.T) {
	r, insert := exactRig(t)
	ev := func(ts int64, sid string) store.Event {
		return store.Event{TS: ts, Source: "browser", Kind: "page_view", Visitor: "v1", SID: sid, IP: "192.0.2.62",
			IPKey: "192.0.2.62", Template: "/", Cls: store.ClsHuman}
	}
	insert(ev(at("2026-11-17T23:50:00Z"), "s1"), ev(at("2026-11-18T00:10:00Z"), "s1"),
		ev(at("2026-11-18T10:00:00Z"), "s2"))
	r.build(t)
	site, _ := r.s.Sites.Lookup(feedSite)
	// The 17th's raw cut as retention cuts it: deleted, the day marked without raw, its rows kept.
	for _, q := range []string{`DELETE FROM event WHERE ts < ` + fmt.Sprint(at("2026-11-18T00:00:00Z")),
		`UPDATE agg_day SET raw = 0 WHERE day = ` + fmt.Sprint(at("2026-11-17T00:00:00Z")/dayMS)} {
		if _, err := r.s.DB.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, aud := range []string{"all", "humans"} {
		var table float64 = -1
		n := countPieces(r, func(r *rig) {
			_, v := r.getFrom(t, SourceAuto, "visitors", p(feedSite, "range", "7d", "tz", "UTC", "aud", aud))
			for _, row := range listRows(v) {
				if row["key"] == "192.0.2.62" {
					table = num64(row["visits"])
				}
			}
		})
		q := &query{site: site, aud: aud, p: newPeriod("7d", "off", r.s.now(), time.UTC)}
		a, err := r.server(SourceAuto).actorOne(t.Context(), q, "192.0.2.62")
		if err != nil || a == nil || table != 2 || a.visits != 2 || n.agg == 0 || n.raw == 0 {
			t.Errorf("%s: %v visits in the table, detail %+v %v, pieces %+v; want 2", aud, table, a, err, n)
		}
	}
}

// The detail sums a key over every audience: a bot's session crossing midnight from an aggregate day whose raw is
// gone into a raw day is one visit with the people's audience too.
func TestSources_StitchRawGoneAud(t *testing.T) {
	r, insert := exactRig(t)
	ev := func(ts int64, sid string, cls int) store.Event {
		return store.Event{TS: ts, Source: "browser", Kind: "page_view", Visitor: "v1", SID: sid, IP: "192.0.2.63",
			IPKey: "192.0.2.63", Template: "/", Cls: cls}
	}
	insert(ev(at("2026-11-17T23:50:00Z"), "s1", store.ClsBot), ev(at("2026-11-18T00:10:00Z"), "s1", store.ClsBot),
		ev(at("2026-11-18T10:00:00Z"), "s2", store.ClsHuman))
	r.build(t)
	site, _ := r.s.Sites.Lookup(feedSite)
	for _, q := range []string{`DELETE FROM event WHERE ts < ` + fmt.Sprint(at("2026-11-18T00:00:00Z")),
		`UPDATE agg_day SET raw = 0 WHERE day = ` + fmt.Sprint(at("2026-11-17T00:00:00Z")/dayMS)} {
		if _, err := r.s.DB.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, aud := range []string{"all", "humans"} {
		q := &query{site: site, aud: aud, p: newPeriod("7d", "off", r.s.now(), time.UTC)}
		a, err := r.server(SourceAuto).actorOne(t.Context(), q, "192.0.2.63")
		if err != nil || a == nil || a.visits != 2 {
			t.Errorf("%s: detail %+v %v; want 2 visits", aud, a, err)
		}
	}
}

// A session carried into an aggregate day from a day read from nowhere (its raw cut, never built) is a visit: the
// table and the detail stitch only onto a day they counted.
func TestSources_StitchAfterGap(t *testing.T) {
	r, insert := exactRig(t)
	ev := func(ts int64) store.Event {
		return store.Event{TS: ts, Source: "browser", Kind: "page_view", Visitor: "v1", SID: "s1", IP: "192.0.2.60",
			IPKey: "192.0.2.60", Template: "/", Cls: store.ClsHuman}
	}
	insert(ev(at("2026-11-10T23:50:00Z")), ev(at("2026-11-11T00:10:00Z")))
	r.build(t)
	site, _ := r.s.Sites.Lookup(feedSite)
	for _, q := range []string{`DELETE FROM event WHERE ts < ` + fmt.Sprint(at("2026-11-11T00:00:00Z")),
		`UPDATE agg_day SET raw = 0, frozen = 0 WHERE day = ` + fmt.Sprint(at("2026-11-10T00:00:00Z")/dayMS)} {
		if _, err := r.s.DB.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, src := range []Source{SourceAgg, SourceAuto} {
		_, v := r.getFrom(t, src, "visitors", p(feedSite, "range", "30d", "tz", "UTC"))
		var table float64 = -1
		for _, row := range listRows(v) {
			if row["key"] == "192.0.2.60" {
				table = num64(row["visits"])
			}
		}
		q := &query{site: site, aud: "all", p: newPeriod("30d", "off", r.s.now(), time.UTC)}
		a, err := r.server(src).actorOne(t.Context(), q, "192.0.2.60")
		if err != nil || a == nil || table != 1 || a.visits != 1 {
			t.Errorf("%v: %v visits in the table, detail %+v %v; want 1", src, table, a, err)
		}
	}
}

// A network that may sit in a prior day's Other is no spike: what it had then is not known.
func TestSources_NetSpikePriorOther(t *testing.T) {
	r, insert := exactRig(t)
	req := func(ts int64, ip string, asn int64) store.Event {
		return store.Event{TS: ts, Source: "server", Kind: "request", IP: ip, IPKey: ip, Template: "GET /",
			Method: "GET", Status: 200, ASN: asn, Cls: store.ClsBot}
	}
	var evs []store.Event
	// The prior period: AS64600 100 requests, AS64601 150 from two addresses (kept at one entry a day).
	for i := range 100 {
		evs = append(evs, req(at("2026-10-10T10:00:00Z")+int64(i)*1000, "198.51.100.80", 64600))
	}
	for i := range 150 {
		evs = append(evs, req(at("2026-10-10T11:00:00Z")+int64(i)*1000, fmt.Sprint("198.51.100.", 81+i%2), 64601))
	}
	// Now AS64600 sends 250: raw compares them with its 100 (no threefold growth).
	for i := range 250 {
		evs = append(evs, req(at("2026-11-15T10:00:00Z")+int64(i)*1000, "198.51.100.80", 64600))
	}
	insert(evs...)
	r.buildK(t, 1)
	for _, src := range []Source{SourceRaw, SourceAgg} {
		_, body := r.getFrom(t, src, "noteworthy", p(feedSite, "range", "30d", "tz", "UTC", "findings", "all"))
		for _, x := range body["items"].([]any) {
			if id := x.(map[string]any)["id"].(string); strings.HasPrefix(id, "spike:net:") {
				t.Errorf("%v: %s", src, id)
			}
		}
	}
}
