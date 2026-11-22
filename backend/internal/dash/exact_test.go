package dash

import (
	"encoding/json"
	"testing"
	"time"

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
