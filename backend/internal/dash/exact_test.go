package dash

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/trick77/ship/kit/ratelimit"
	"github.com/trick77/stakeout/backend/internal/agg"
	"github.com/trick77/stakeout/backend/internal/insight"
	"github.com/trick77/stakeout/backend/internal/store"
)

// exactRig is an empty rig plus a way to write hand-made events for the feed site.
func exactRig(t *testing.T) (*rig, func(...store.Event)) {
	t.Helper()
	r := newRig(t, 0, 0, nil)
	site, _ := r.s.Sites.Lookup(feedSite)
	return r, func(evs ...store.Event) {
		t.Helper()
		for i := range evs {
			evs[i].SiteID = site.ID
			if evs[i].Props == "" {
				evs[i].Props = "{}"
			}
		}
		if err := r.s.DB.InsertEvents(t.Context(), evs); err != nil {
			t.Fatal(err)
		}
	}
}

func at(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UnixMilli()
}

// pageLoad is one page load seen by both sources: the script's page view and session start (the browser's own id)
// and the server's document request (a hash id), one address, one person.
func pageLoad(ts int64, vid, ip, country, ua string, asn int64) []store.Event {
	b := store.Event{TS: ts, Source: "browser", Visitor: vid, SID: vid + "-s", IP: ip, IPKey: ip, UA: ua,
		Path: "/a", Template: "/a", Country: country, ASN: asn, Cls: store.ClsHuman}
	pv, ss := b, b
	pv.Kind, ss.Kind = "page_view", "session_start"
	return []store.Event{pv, ss, {TS: ts, Source: "server", Kind: "request", Name: "document", Visitor: "h:" + vid,
		IP: ip, IPKey: ip, UA: ua, Path: "/a", Template: "GET /a", Method: "GET", Status: 200, Country: country,
		ASN: asn, Cls: store.ClsHuman}}
}

func num64(v any) float64 { f, _ := v.(float64); return f }

func changeOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The rig's clock is 2026-11-22 13:31 UTC, the viewer in Zurich: 7d is 16.-21.11. closed, the 22nd open. A tile runs
// to now and is compared with 9.-15.11. up to the same hour, 13:00 UTC; a list's change with the 14th and before.
func TestExact_BothSourcesOpenBucketPrevious(t *testing.T) {
	r, insert := exactRig(t)
	const chrome = "Mozilla/5.0 (Macintosh) Chrome/142 Safari/537.36"
	insert(pageLoad(at("2026-11-20T10:00:00Z"), "a", "192.0.2.1", "CH", chrome, 64500)...) // closed
	insert(pageLoad(at("2026-11-22T10:00:00Z"), "b", "192.0.2.2", "DE", chrome, 64501)...) // open bucket
	insert(pageLoad(at("2026-11-12T10:00:00Z"), "c", "192.0.2.3", "CH", chrome, 64500)...) // prior, closed
	insert(pageLoad(at("2026-11-15T10:00:00Z"), "d", "192.0.2.4", "CH", chrome, 64500)...) // prior, aligned to open
	insert(pageLoad(at("2026-11-15T20:00:00Z"), "e", "192.0.2.5", "CH", chrome, 64500)...) // prior, past the cut

	_, ov := r.get(t, "overview", p(feedSite))
	tl := tiles(t, ov)
	vis := tl["visitors"]
	if num64(vis["value"]) != 2 || num64(vis["prev"]) != 2 || changeOf(vis["change"]) != `{"pct":0}` ||
		vis["verdict"] != "flat" {
		t.Errorf("visitors tile %v", vis)
	}
	// The hours of 16.11. 00:00 to the 22nd's 14:00, the one filling, in Zurich.
	if s := vis["series"].([]any); len(s) != 6*24+15 || num64(s[4*24+11]) != 1 || num64(s[6*24+11]) != 1 || num64(s[0]) != 0 {
		t.Errorf("visitors series %v", s)
	}
	// Six hours' mean: one visitor in the three before or the two after, the last six before the open slot, which has
	// none.
	m := vis["mean"].([]any)
	for i, want := range map[int]float64{4*24 + 8: 0, 4*24 + 9: 0.17, 4*24 + 14: 0.17, 4*24 + 15: 0, 6*24 + 8: 0,
		6*24 + 9: 0.17, 6*24 + 13: 0.17} {
		if num64(m[i]) != want {
			t.Errorf("mean[%d] %v, want %v", i, m[i], want)
		}
	}
	if m[6*24+14] != nil {
		t.Errorf("the open slot has a mean: %v", m[6*24+14])
	}
	// The site's first figure is c's: no mean of the same six hours as this period's reaches back before it.
	if ps := vis["prev_mean"].([]any); ps[3*24+13] != nil || num64(ps[3*24+14]) != 0.17 || num64(ps[3*24+15]) != 0 || num64(ps[6*24+9]) != 0.17 || num64(ps[6*24+8]) != 0 {
		t.Errorf("visitors prior means %v", ps)
	}
	if v := tl["views"]; num64(v["value"]) != 2 || num64(v["prev"]) != 2 {
		t.Errorf("views tile %v", v)
	}
	if v := tl["requests"]; num64(v["value"]) != 2 || num64(v["prev"]) != 2 {
		t.Errorf("requests tile %v", v)
	}
	// The classes are compared as the tiles are.
	_, cs := r.get(t, "classes", p(feedSite))
	if row := listRows(cs)[0]; num64(row["requests"]) != 2 || changeOf(row["change"]) != `{"pct":0}` {
		t.Errorf("classes %v", row)
	}

	// Countries: one person per page load, whatever the sources. Over the whole range (the open bucket in), the
	// change on closed buckets only.
	_, cl := r.get(t, "list", p(feedSite, "dim", "countries"))
	rows := listRows(cl)
	got := map[string][3]float64{}
	for _, row := range rows {
		got[row["key"].(string)] = [3]float64{num64(row["value"]), num64(row["share"])}
	}
	if len(rows) != 2 || got["CH"] != [3]float64{1, 50} || got["DE"] != [3]float64{1, 50} {
		t.Errorf("countries %v", rows)
	}
	if changeOf(rows[0]["change"]) != `{"pct":0}` && changeOf(rows[1]["change"]) != `{"pct":0}` {
		t.Errorf("CH change: %v", rows)
	}
	for _, dim := range []string{"pages", "entry", "networks"} {
		_, l := r.get(t, "list", p(feedSite, "dim", dim))
		var sum float64
		for _, row := range listRows(l) {
			sum += num64(row["value"])
		}
		if sum != 2 {
			t.Errorf("%s: values sum %v, want 2 (two people)", dim, sum)
		}
	}
	_, c := r.get(t, "clients", p(feedSite))
	if b := c["browsers"].([]any); len(b) != 1 || num64(b[0].(map[string]any)["value"]) != 2 ||
		num64(b[0].(map[string]any)["share"]) != 100 {
		t.Errorf("browsers %v", b)
	}
	_, v := r.get(t, "visitors", p(feedSite, "q", "192.0.2.1"))
	if vr := v["rows"].([]any); len(vr) != 1 || num64(vr[0].(map[string]any)["views"]) != 1 ||
		num64(vr[0].(map[string]any)["requests"]) != 1 || num64(vr[0].(map[string]any)["visits"]) != 1 {
		t.Errorf("visitors %v", v)
	}
}

// 24h is the 24 hours before now (13:31 UTC) to the minute, compared with the 24 before them: tiles, classes and lists.
func TestExact_Last24Hours(t *testing.T) {
	r, insert := exactRig(t)
	const chrome = "Mozilla/5.0 (Macintosh) Chrome/142 Safari/537.36"
	insert(pageLoad(at("2026-11-22T13:26:00Z"), "a", "192.0.2.1", "CH", chrome, 64500)...) // five minutes ago
	insert(pageLoad(at("2026-11-21T13:41:00Z"), "b", "192.0.2.2", "CH", chrome, 64500)...) // 23 h 50 min ago
	insert(pageLoad(at("2026-11-21T13:21:00Z"), "c", "192.0.2.3", "CH", chrome, 64500)...) // 24 h 10 min ago: prior
	insert(pageLoad(at("2026-11-20T13:21:00Z"), "d", "192.0.2.4", "CH", chrome, 64500)...) // 48 h 10 min ago: neither

	_, ov := r.get(t, "overview", p(feedSite, "range", "24h"))
	period := ov["period"].(map[string]any)
	if labels := period["labels"].([]any); period["open"] != false || labels[143] != "14:21–14:31" ||
		labels[0] != "14:31–14:41" || period["from"] != "2026-11-21T13:31:00Z" {
		t.Errorf("period %v", period)
	}
	tl := tiles(t, ov)
	for _, key := range []string{"visitors", "views", "requests"} {
		v := tl[key]
		if num64(v["value"]) != 2 || num64(v["prev"]) != 1 || changeOf(v["change"]) != `{"pct":100}` {
			t.Errorf("%s tile %v", key, v)
		}
		// b came 10 minutes into the range, in the second slot; a and c five and ten minutes before an end.
		if s, ps := v["series"].([]any), v["prev_mean"].([]any); num64(s[0]) != 0 || num64(s[1]) != 1 || num64(s[143]) != 1 ||
			num64(ps[140]) != 0 || num64(ps[141]) != 0.17 || num64(ps[143]) != 0.17 {
			t.Errorf("%s series %v, prior %v", key, s, ps)
		}
	}
	_, cs := r.get(t, "classes", p(feedSite, "range", "24h"))
	if row := listRows(cs)[0]; num64(row["requests"]) != 2 || changeOf(row["change"]) != `{"pct":100}` {
		t.Errorf("classes %v", row)
	}
	_, cl := r.get(t, "list", p(feedSite, "range", "24h", "dim", "countries"))
	if rows := listRows(cl); len(rows) != 1 || num64(rows[0]["value"]) != 2 ||
		changeOf(rows[0]["change"]) != `{"pct":100}` {
		t.Errorf("countries %v", rows)
	}
}

// 24h rolls on: a visitor shown opens, and the log's next page ends, where the page loaded (from), at most a day back.
func TestExact_HoldFrom(t *testing.T) {
	r, insert := exactRig(t)
	insert(pageLoad(at("2026-11-21T13:35:00Z"), "a", "192.0.2.1", "CH", "Mozilla/5.0", 64500)...)
	_, table := r.get(t, "visitors", p(feedSite, "range", "24h"))
	_, log := r.get(t, "log", p(feedSite, "range", "24h"))
	from := strconv.FormatInt(int64(num64(table["from"])), 10)
	if len(listRows(table)) != 1 || log["total"] != 3.0 || table["from"] != log["from"] ||
		int64(num64(table["from"])) != at("2026-11-21T13:31:00Z") {
		t.Fatalf("table %v, log %v", table, log)
	}
	r.s.Now = func() time.Time { return r.end.Add(11 * time.Minute) } // 13:41: the event is 24 h 6 min old
	for _, c := range []struct {
		from string
		code int
		log  float64
	}{{"", 404, 0}, {from, 200, 3}, {strconv.FormatInt(at("2026-11-20T13:00:00Z"), 10), 404, 0}, {"x", 400, 0}} {
		code, _ := r.get(t, "visitor", p(feedSite, "range", "24h", "key", "192.0.2.1", "from", c.from))
		logCode, l := r.get(t, "log", p(feedSite, "range", "24h", "from", c.from))
		if code != c.code || (c.code != 400 && num64(l["total"]) != c.log) || (logCode == 400) != (c.code == 400) {
			t.Errorf("from %q: visitor %d, log %d %v", c.from, code, logCode, l)
		}
	}
	// Another range starts at a midnight: from changes nothing.
	if _, l := r.get(t, "log", p(feedSite, "range", "7d", "from", "1")); num64(l["total"]) != 3 {
		t.Errorf("7d %v", l)
	}
}

// Noteworthy's 24h is the full hours of the 24 before now (21st 14:00 to 22nd 13:00): its facts, and the briefing
// written for them, hold until the next, and each is inside the 24h the other panels show.
func TestExact_NoteworthyHour(t *testing.T) {
	r, insert := exactRig(t)
	jsErr := func(ts, name string) store.Event {
		return store.Event{TS: at(ts), Source: "browser", Kind: "js_error", Name: name, Visitor: "v", SID: "s",
			IP: "192.0.2.1", IPKey: "192.0.2.1", Cls: store.ClsHuman}
	}
	insert(jsErr("2026-11-21T14:05:00Z", "first"), jsErr("2026-11-22T13:05:00Z", "this hour"),
		jsErr("2026-11-21T13:05:00Z", "a day ago"), jsErr("2026-11-21T13:45:00Z", "its first hour"))
	_, b := r.get(t, "noteworthy", p(feedSite, "range", "24h"))
	items := b["items"].([]any)
	if b["as_of"] != "2026-11-22T13:00:00Z" || len(items) != 1 || items[0].(map[string]any)["id"] != "err:first" {
		t.Errorf("noteworthy %v", b)
	}
	// On the hour every one of the 24 is full.
	p := hoursPeriod(time.UnixMilli(at("2026-11-22T13:00:00Z")).UTC(), time.UTC)
	if p.open || p.ClosedEnd().UnixMilli() != at("2026-11-22T13:00:00Z") ||
		p.win.From.UnixMilli() != at("2026-11-21T13:00:00Z") || !p.prevWin.Closed.Equal(p.win.From) {
		t.Errorf("on the hour %+v", p)
	}
}

// 24h against last year reads the comparison from the hourly aggregates, raw long gone.
func TestExact_24hLastYear(t *testing.T) {
	r, insert := exactRig(t)
	insert(pageLoad(at("2025-11-22T10:00:00Z"), "a", "192.0.2.1", "CH", "Mozilla/5.0", 64500)...)
	insert(pageLoad(at("2026-11-22T10:00:00Z"), "b", "192.0.2.2", "CH", "Mozilla/5.0", 64500)...)
	through := r.end
	if raceOn { // last year's days alone: a build of the year between takes seconds
		through = r.end.AddDate(-1, 0, 0)
	}
	if _, err := agg.BuildThrough(t.Context(), r.s.DB, through, agg.TopK); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	n := countPieces(r, func(r *rig) {
		_, body = r.getFrom(t, SourceAuto, "overview", p(feedSite, "range", "24h", "cmp", "yoy"))
	})
	if v := tiles(t, body)["views"]; num64(v["value"]) != 1 || num64(v["prev"]) != 1 || n.agg == 0 || n.raw != 0 {
		t.Errorf("views %v, pieces %+v", v, n)
	}
}

// API clients, known bots and uptime probes rising is neutral; unknown automation rising is bad, people rising good.
func TestExact_ClassVerdicts(t *testing.T) {
	r, insert := exactRig(t)
	req := func(ts int64, ip string, cls int) store.Event {
		return store.Event{TS: ts, Source: "server", Kind: "request", IP: ip, IPKey: ip, Status: 200, Cls: cls}
	}
	for cls, n := range map[int]int{store.ClsHuman: 4, store.ClsAPI: 4, store.ClsBot: 4, store.ClsMonitor: 4,
		store.ClsUnknown: 4} {
		insert(req(at("2026-11-12T10:00:00Z"), "198.51.100.1", cls))
		for i := range n {
			insert(req(at("2026-11-20T10:00:00Z")+int64(i), "198.51.100.2", cls))
		}
	}
	_, body := r.get(t, "classes", p(feedSite))
	want := map[string]string{"human": "good", "api": "flat", "bot": "flat", "monitor": "flat", "unknown": "bad"}
	for _, row := range body["rows"].([]any) {
		m := row.(map[string]any)
		if changeOf(m["change"]) != `{"pct":300}` || m["verdict"] != want[m["class"].(string)] ||
			num64(m["share"]) != 20 {
			t.Errorf("class row %v", m)
		}
	}
}

func serverReq(ts int64, ip, tmpl string, status int) store.Event {
	return store.Event{TS: ts, Source: "server", Kind: "request", IP: ip, IPKey: ip, UA: "curl/8.4.0",
		Template: tmpl, Method: "GET", Status: status, Cls: store.ClsUnknown}
}

// Pages of the visitors table come from one snapshot of one frozen span: rows that change between two pages
// neither move nor repeat; once the snapshot is gone, the cursor resumes over the same span by its sort value.
func TestExact_VisitorsPaging(t *testing.T) {
	r, insert := exactRig(t)
	base := at("2026-11-21T10:00:00Z")
	for i, n := range []int{50, 40, 30, 20, 10} {
		ip := "198.51.100." + string(rune('1'+i))
		for j := range n {
			insert(serverReq(base+int64(j), ip, "GET /x", 200))
		}
	}
	_, p1 := r.get(t, "visitors", p(feedSite, "limit", "2"))
	keys := func(b map[string]any) []string {
		var out []string
		for _, row := range listRows(b) {
			out = append(out, row["key"].(string))
		}
		return out
	}
	if got := keys(p1); len(got) != 2 || got[0] != "198.51.100.1" || got[1] != "198.51.100.2" || p1["total"] != 5.0 {
		t.Fatalf("page 1 %v", p1)
	}
	// The fifth actor leaps to the top before page 2 is read (a late, back-dated batch).
	for j := range 100 {
		insert(serverReq(base+1000+int64(j), "198.51.100.5", "GET /x", 200))
	}
	_, p2 := r.get(t, "visitors", p(feedSite, "limit", "2", "cursor", p1["next"].(string)))
	if got := keys(p2); len(got) != 2 || got[0] != "198.51.100.3" || got[1] != "198.51.100.4" {
		t.Errorf("page 2 from the snapshot: %v", got)
	}
	// The snapshot expired: the cursor's span and sort value carry on, nothing repeats.
	r.s.snaps = snapshots{}
	_, p3 := r.get(t, "visitors", p(feedSite, "limit", "2", "cursor", p2["next"].(string)))
	if got := keys(p3); len(got) != 0 || p3["next"] != "" {
		t.Errorf("page 3 after the snapshot: %v (the leaped actor sorts before the cursor now)", got)
	}
}

// "What it does" and the evidence count the whole range, not the trail's newest 2000 events.
func TestExact_WholeRangeCounts(t *testing.T) {
	r, insert := exactRig(t)
	base := at("2026-11-21T10:00:00Z")
	var evs []store.Event
	for i := range 2100 {
		evs = append(evs, serverReq(base+int64(i)*100, "198.51.100.9", "GET /a", 200))
	}
	for i := range 1500 {
		evs = append(evs, serverReq(base+300_000+int64(i)*100, "198.51.100.9", "GET /b", 200))
	}
	insert(evs...)
	_, v := r.get(t, "visitors", p(feedSite))
	row := listRows(v)[0]
	if d := row["does"].(map[string]any)["text"]; d != "Unknown automated, mostly GET /a → 200 (2100×)" {
		t.Errorf("does %q", d)
	}
}

// One run of an action spanning days shows its own pace, the median between its events, as the pace fact does.
func TestExact_RunInterval(t *testing.T) {
	r, insert := exactRig(t)
	for _, day := range []string{"2026-11-20T10:00:00Z", "2026-11-21T10:00:00Z"} {
		for i := range 10 {
			insert(serverReq(at(day)+int64(i)*800, "198.51.100.10", "GET /a", 401))
		}
	}
	_, d := r.get(t, "visitor", p(feedSite, "key", "198.51.100.10"))
	line := d["trail"].([]any)[0].(map[string]any)
	if line["count"] != 20.0 || line["interval_s"] != 0.8 {
		t.Errorf("run %v", line)
	}
}

// A reading cached under a behaviour's signature fits every actor with it, each with its own figures and network;
// an actor whose statements differ gets another signature, and a reading citing what it lacks does not resolve.
func TestExact_SharedReading(t *testing.T) {
	r, _ := exactRig(t)
	row := func(key, network string, requests, refused int64) visitorRow {
		share := round1(pct(float64(refused), float64(requests)))
		return visitorRow{Key: key, Class: "unknown", Client: "curl/8.4.0", Network: network, NetworkKind: "hosting",
			Requests: &requests, Refused: &refused, RefusedShare: &share, Signals: []string{"client curl/8.4.0"}}
	}
	counts := map[string]int64{"GET /api → 401": 2300}
	tr := trailResult{gaps: []float64{0.8, 0.8, 0.8, 0.8}}
	a, factsA := r.s.readingInput(1, row("198.51.100.1", "AS64500 Alpha Hosting", 2340, 2338), counts, tr)
	counts2 := map[string]int64{"GET /api → 401": 3000}
	b, factsB := r.s.readingInput(1, row("203.0.113.7", "AS64511 Beta Cloud", 3010, 3000), counts2, tr)
	if a.Signature != b.Signature {
		t.Fatal("alike behaviour, different signatures")
	}
	for _, in := range []insight.ReadingInput{a, b} {
		raw, _ := json.Marshal(in.Evidence)
		if strings.Contains(string(raw), "Alpha") || strings.Contains(string(raw), "Beta") || in.Key == "" {
			t.Errorf("the model sees a network name: %s", raw)
		}
	}
	rd := insight.Reading{Verdict: "Credential probing", Evidence: []string{"requests:refused-most", "network:hosting"}}
	evA, okA := resolve(rd, factsA)
	evB, okB := resolve(rd, factsB)
	if !okA || !okB || !strings.HasPrefix(evA[0], "2340 requests") || !strings.HasPrefix(evB[0], "3010 requests") ||
		!strings.Contains(evB[1], "Beta Cloud") {
		t.Errorf("resolved %v / %v", evA, evB)
	}
	// Mostly answered instead of refused: another signature, and the reading's fact is missing.
	c, factsC := r.s.readingInput(1, row("192.0.2.5", "AS64500 Alpha Hosting", 2340, 3), counts, tr)
	if c.Signature == a.Signature {
		t.Error("a refused and an answered actor share a signature")
	}
	if _, ok := resolve(rd, factsC); ok {
		t.Error("a reading resolved against facts the actor does not have")
	}
}

// Noteworthy: a rise and a fall are different findings; the briefing's own time is as_of; no findings, no
// briefing asked.
func TestExact_Noteworthy(t *testing.T) {
	r, insert := exactRig(t)
	model := &fakeModel{}
	r.s.Model = model
	_, empty := r.get(t, "noteworthy", p(feedSite))
	if len(model.briefIns) != 0 || empty["headline"].(map[string]any)["gen"] != nil {
		t.Errorf("no findings, yet the briefing was asked: %v", empty)
	}
	const chrome = "Mozilla/5.0 (Macintosh) Chrome/142 Safari/537.36"
	for i := range 25 {
		insert(pageLoad(at("2026-11-20T10:00:00Z")+int64(i), "n"+string(rune('a'+i)), "192.0.2.1", "CH", chrome, 0)...)
	}
	for i := range 10 {
		insert(pageLoad(at("2026-11-12T10:00:00Z")+int64(i), "o"+string(rune('a'+i)), "192.0.2.1", "CH", chrome, 0)...)
	}
	asOf := time.Date(2026, 11, 21, 23, 0, 0, 0, time.UTC)
	model.briefing = &insight.Briefing{Headline: "More people than before", AsOf: asOf}
	_, body := r.get(t, "noteworthy", p(feedSite))
	in := model.briefIns[0]
	ids := map[string]bool{}
	for _, f := range in.Facts {
		ids[f.ID] = true
	}
	if !ids["trend:visitors:up"] || body["as_of"] != asOf.Format(time.RFC3339) {
		t.Errorf("facts %v, as_of %v", ids, body["as_of"])
	}
	// The hash is the id set's, in any order.
	if in.Hash != factsHash(append([]insight.Fact{in.Facts[len(in.Facts)-1]}, in.Facts[:len(in.Facts)-1]...)) {
		t.Error("the facts hash depends on their order")
	}
}

// The snapshots stay within their row bound and their time: the oldest go first, an expired one is gone, one too
// large is never kept.
func TestSnapshots(t *testing.T) {
	var c snapshots
	now := time.Now()
	rows := make([]visitorRow, snapshotMaxRows/2)
	c.put("a", rows, now)
	c.put("b", rows, now.Add(time.Second))
	c.put("c", rows[:10], now.Add(2*time.Second)) // over the bound: a, the oldest, goes
	if _, ok := c.get("a", now.Add(3*time.Second)); ok {
		t.Error("the oldest snapshot kept past the bound")
	}
	if _, ok := c.get("b", now.Add(3*time.Second)); !ok {
		t.Error("b evicted")
	}
	if _, ok := c.get("c", now.Add(snapshotTTL+3*time.Second)); ok {
		t.Error("an expired snapshot answered")
	}
	c.put("d", rows[:1], now.Add(snapshotTTL+time.Hour)) // b and c expired: dropped on the way
	if len(c.m) != 1 {
		t.Errorf("%d snapshots kept", len(c.m))
	}
	c.put("huge", make([]visitorRow, snapshotMaxRows+1), now)
	if _, ok := c.get("huge", now); ok {
		t.Error("a snapshot over the bound kept")
	}
}

// Both sources' last delivery is their newest stored event, right after a restart (counters on disk only) as
// while running (this process's record).
func TestExact_LastSeen(t *testing.T) {
	r, insert := exactRig(t)
	site, _ := r.s.Sites.Lookup(feedSite)
	b := pageLoad(at("2026-11-22T12:07:28Z"), "a", "192.0.2.1", "CH", "Mozilla/5.0", 0)
	insert(b...)
	insert(serverReq(at("2026-11-22T13:20:05Z"), "192.0.2.2", "GET /x", 200))
	// After a restart: the hourly rows only (12:00 and 13:00), no record of this process for either source.
	if err := r.s.DB.AddStats(t.Context(), []store.IngestStat{
		{SiteID: site.ID, Hour: at("2026-11-22T12:00:00Z") / 3_600_000, Accepted: 3},
		{SiteID: site.ID, Hour: at("2026-11-22T13:00:00Z") / 3_600_000, Accepted: 1, AcceptedServer: 1}}); err != nil {
		t.Fatal(err)
	}
	r.s.Counters = nil
	_, body := r.get(t, "sites", nil)
	var row map[string]any
	for _, x := range body["sites"].([]any) {
		if x.(map[string]any)["site"] == feedSite {
			row = x.(map[string]any)
		}
	}
	if row["browser"].(map[string]any)["last"] != "2026-11-22T12:07:28Z" ||
		row["server"].(map[string]any)["last"] != "2026-11-22T13:20:05Z" {
		t.Errorf("after a restart: %v", row)
	}
}

// The client shown is the latest event's, in the table, the detail and the clients split alike.
func TestExact_LatestClient(t *testing.T) {
	r, insert := exactRig(t)
	early := pageLoad(at("2026-11-19T10:00:00Z"), "a", "2001:db8::1", "CH", "Zeta/9.0", 0)
	late := pageLoad(at("2026-11-20T10:00:00Z"), "a", "192.0.2.9", "CH", "Mozilla/5.0 (Windows) Firefox/140", 0)
	for i := range early {
		early[i].IPKey = "192.0.2.9" // one key, so the table row is one actor with two clients
	}
	insert(early...)
	insert(late...)
	_, v := r.get(t, "visitors", p(feedSite))
	row := v["rows"].([]any)[0].(map[string]any)
	if row["client"] != "Firefox\xe2\x80\x82·\xe2\x80\x82Windows" {
		t.Errorf("table client %v", row["client"])
	}
	_, d := r.get(t, "visitor", p(feedSite, "key", "192.0.2.9"))
	if d["client"] != row["client"] {
		t.Errorf("detail client %v, table %v", d["client"], row["client"])
	}
	_, c := r.get(t, "clients", p(feedSite))
	if ipv := c["ip_version"].([]any); len(ipv) != 1 || ipv[0].(map[string]any)["key"] != "IPv4" {
		t.Errorf("ip_version %v", ipv)
	}
}

// The detail's addresses are a key's distinct addresses over its raw span, read a month window at a time: a /64's
// addresses recur from window to window and count once; a plain address is one, whatever windows it falls in.
func TestExact_VisitorAddresses(t *testing.T) {
	r, insert := exactRig(t)
	const ua = "Mozilla/5.0 (Windows) Firefox/140"
	a, b, c, d := "2001:db8:1:1::a", "2001:db8:1:1::b", "2001:db8:1:1::c", "2001:db8:1:1::d"
	net := ratelimit.Key(a)
	// Over a month apart, each day in a window of its own: per window 2 + 2 + 3 = 7, together 4.
	days := []string{"2026-07-10", "2026-09-01", "2026-11-20"}
	for i, ips := range [][]string{{a, b}, {b, c}, {a, c, d}} {
		for _, ip := range ips {
			evs := pageLoad(at(days[i]+"T10:00:00Z"), "v"+ip, ip, "CH", ua, 0)
			for j := range evs {
				evs[j].IPKey = net
			}
			insert(evs...)
		}
		insert(pageLoad(at(days[i]+"T11:00:00Z"), "w", "192.0.2.9", "CH", ua, 0)...)
	}
	for key, want := range map[string]float64{net: 4, "192.0.2.9": 1} {
		code, v := r.get(t, "visitor", p(feedSite, "range", "12mo", "key", key))
		if code != 200 || v["addresses"] != want {
			t.Errorf("%s: %d, addresses %v, want %v", key, code, v["addresses"], want)
		}
	}
}

// From 30 days on, the split counts every client a visitor used (days do not add up to "the latest"), shares of the
// sum; 7d keeps the latest.
func TestExact_EveryClient(t *testing.T) {
	r, insert := exactRig(t)
	const chrome, firefox = "Mozilla/5.0 (Macintosh) Chrome/142 Safari/537.36", "Mozilla/5.0 (Windows) Firefox/140"
	insert(pageLoad(at("2026-11-19T10:00:00Z"), "a", "192.0.2.9", "CH", chrome, 0)...)
	insert(pageLoad(at("2026-11-20T10:00:00Z"), "a", "192.0.2.9", "CH", firefox, 0)...)
	insert(pageLoad(at("2026-11-20T11:00:00Z"), "b", "192.0.2.8", "CH", chrome, 0)...)
	browsers := func(rng string) map[string][2]float64 {
		_, c := r.get(t, "clients", p(feedSite, "range", rng))
		out := map[string][2]float64{}
		for _, x := range c["browsers"].([]any) {
			m := x.(map[string]any)
			out[m["key"].(string)] = [2]float64{num64(m["value"]), num64(m["share"])}
		}
		return out
	}
	if got := browsers("30d"); len(got) != 2 || got["Chrome"] != [2]float64{2, 67} || got["Firefox"] != [2]float64{1, 33} {
		t.Errorf("30d browsers %v", got)
	}
	if got := browsers("7d"); len(got) != 2 || got["Chrome"] != [2]float64{1, 50} || got["Firefox"] != [2]float64{1, 50} {
		t.Errorf("7d browsers %v", got)
	}
}

// A person types a client with ordinary spaces, the table shows it with en spaces: any spacing finds it.
func TestExact_SearchFoldsSpaces(t *testing.T) {
	r, insert := exactRig(t)
	insert(pageLoad(at("2026-11-20T10:00:00Z"), "a", "192.0.2.9", "CH", "Mozilla/5.0 (Windows) Firefox/140", 0)...)
	for _, q := range []string{"firefox · windows", "Firefox\xe2\x80\x82·\xe2\x80\x82Windows", " firefox  ·\twindows"} {
		_, v := r.get(t, "visitors", p(feedSite, "q", q))
		if n := len(v["rows"].([]any)); n != 1 {
			t.Errorf("q %q: %d rows", q, n)
		}
	}
	// A model's label holds the separator with en spaces too.
	r.s.Model = &fakeModel{}
	if err := r.s.DB.PutLabel(t.Context(), "ua", "x/1", "Script\xe2\x80\x82·\xe2\x80\x82walker"); err != nil {
		t.Fatal(err)
	}
	if got, err := r.s.labelMatches(t.Context(), foldSpaces("script · walker")); err != nil || !got["x/1"] {
		t.Errorf("label match %v %v", got, err)
	}
}

// A day classified before the en spaces shows its stored reasons with them, in the table and the detail.
func TestExact_StoredReasonsRespaced(t *testing.T) {
	r, insert := exactRig(t)
	insert(pageLoad(at("2026-11-20T10:00:00Z"), "a", "192.0.2.9", "CH", "Mozilla/5.0 (Windows) Firefox/140", 0)...)
	site, _ := r.s.Sites.Lookup(feedSite)
	old := "also unknown automated: Chrome · macOS, 3 of 9 events (client curl/8.4.0)"
	if err := r.s.DB.SetActorClass(t.Context(), store.ActorClass{SiteID: site.ID, IPKey: "192.0.2.9",
		Day: at("2026-11-20T00:00:00Z") / dayMS, Class: "human", Reasons: []string{old}}, nil); err != nil {
		t.Fatal(err)
	}
	want := "also unknown automated: Chrome\xe2\x80\x82·\xe2\x80\x82macOS, 3 of 9 events (client curl/8.4.0)"
	_, v := r.get(t, "visitors", p(feedSite))
	_, d := r.get(t, "visitor", p(feedSite, "key", "192.0.2.9"))
	for name, sig := range map[string]any{"table": v["rows"].([]any)[0].(map[string]any)["signals"],
		"detail": d["signals"]} {
		if s, _ := sig.([]any); len(s) != 1 || s[0] != want {
			t.Errorf("%s signals %q", name, sig)
		}
	}
}
