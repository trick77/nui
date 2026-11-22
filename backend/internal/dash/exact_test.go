package dash

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/trick77/nui/backend/internal/insight"
	"github.com/trick77/nui/backend/internal/store"
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

// The rig's clock is 2026-11-22 13:31 UTC, the viewer in Zurich: 7d is 16.-21.11. closed, the 22nd open, compared
// with 9.-14.11. (the 15th is the prior period's open-aligned bucket and is left out like the 22nd).
func TestExact_BothSourcesOpenBucketPrevious(t *testing.T) {
	r, insert := exactRig(t)
	const chrome = "Mozilla/5.0 (Macintosh) Chrome/142 Safari/537.36"
	insert(pageLoad(at("2026-11-20T10:00:00Z"), "a", "192.0.2.1", "CH", chrome, 64500)...) // closed
	insert(pageLoad(at("2026-11-22T10:00:00Z"), "b", "192.0.2.2", "DE", chrome, 64501)...) // open bucket
	insert(pageLoad(at("2026-11-12T10:00:00Z"), "c", "192.0.2.3", "CH", chrome, 64500)...) // prior, closed
	insert(pageLoad(at("2026-11-15T10:00:00Z"), "d", "192.0.2.4", "CH", chrome, 64500)...) // prior, aligned to open

	_, ov := r.get(t, "overview", p(feedSite))
	tl := tiles(t, ov)
	vis := tl["visitors"]
	if num64(vis["value"]) != 1 || num64(vis["prev"]) != 1 || changeOf(vis["change"]) != `{"pct":0}` ||
		vis["verdict"] != "flat" || num64(vis["median"]) != 0 {
		t.Errorf("visitors tile %v", vis)
	}
	if s := vis["series"].([]any); len(s) != 7 || num64(s[4]) != 1 || num64(s[6]) != 1 || num64(s[0]) != 0 {
		t.Errorf("visitors series %v", s)
	}
	if ps := vis["prev_series"].([]any); num64(ps[3]) != 1 || num64(ps[6]) != 1 {
		t.Errorf("visitors prev series %v", ps)
	}
	if v := tl["views"]; num64(v["value"]) != 1 || num64(v["prev"]) != 1 {
		t.Errorf("views tile %v", v)
	}
	if v := tl["requests"]; num64(v["value"]) != 1 || num64(v["prev"]) != 1 {
		t.Errorf("requests tile %v", v)
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

// Known bots and uptime probes rising is neutral; unknown automation rising is bad, people rising good.
func TestExact_ClassVerdicts(t *testing.T) {
	r, insert := exactRig(t)
	req := func(ts int64, ip string, cls int) store.Event {
		return store.Event{TS: ts, Source: "server", Kind: "request", IP: ip, IPKey: ip, Status: 200, Cls: cls}
	}
	for cls, n := range map[int]int{store.ClsHuman: 4, store.ClsBot: 4, store.ClsMonitor: 4, store.ClsUnknown: 4} {
		insert(req(at("2026-11-12T10:00:00Z"), "198.51.100.1", cls))
		for i := range n {
			insert(req(at("2026-11-20T10:00:00Z")+int64(i), "198.51.100.2", cls))
		}
	}
	_, body := r.get(t, "classes", p(feedSite))
	want := map[string]string{"human": "good", "bot": "flat", "monitor": "flat", "unknown": "bad"}
	for _, row := range body["rows"].([]any) {
		m := row.(map[string]any)
		if changeOf(m["change"]) != `{"pct":300}` || m["verdict"] != want[m["class"].(string)] ||
			num64(m["share"]) != 25 {
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
	if row["client"] != "Firefox · Windows" {
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
