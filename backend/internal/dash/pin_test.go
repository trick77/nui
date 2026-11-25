package dash

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/store"
)

// factsDigest asks Noteworthy for each site and range and digests the facts the model is given and their hash.
func factsDigest(t *testing.T, r *rig, model *fakeModel, sites ...string) (string, int) {
	t.Helper()
	r.s.Model = model
	for _, site := range sites {
		for _, rng := range []string{"24h", "7d", "30d"} {
			if code, body := r.get(t, "noteworthy", p(site, "range", rng, "tz", "UTC")); code != 200 {
				t.Fatal(code, body)
			}
		}
	}
	sum := sha256.New()
	for _, in := range model.briefIns {
		b, err := json.Marshal(in.Facts)
		if err != nil {
			t.Fatal(err)
		}
		sum.Write([]byte(in.Hash))
		sum.Write(b)
		var ids []string
		for _, f := range in.Facts {
			ids = append(ids, f.ID)
		}
		t.Logf("%s %s: %v", in.Domain, in.Range, ids)
	}
	return hex.EncodeToString(sum.Sum(nil)), len(model.briefIns)
}

// The alert rules' thresholds never touch the dashboard: Noteworthy's facts, as the model is given them, and their
// hash (the briefing cache's key) stay byte for byte what they were before the thresholds became a parameter. The
// digests were recorded on the code before it.
func TestFactsPinned(t *testing.T) {
	got, n := factsDigest(t, sharedRig(t, nil), &fakeModel{}, feedSite, plainSite)
	if want := "20c072a0b3518354790c91665762805b2cd020955dae333f8bded56b0c5da546"; got != want || n != 5 {
		t.Errorf("sites: %d inputs, digest %s, want 5, %s", n, got, want)
	}
	calls := mcpCalls()
	for i := range 6 { // a new operation and a new failure in the last day
		calls = append(calls, call{ago: 2*time.Hour + time.Duration(i)*time.Minute, tmpl: "POST /mcp", op: "lookup",
			status: 200, dur: 30, ip: "192.0.2.4", account: "a1"},
			call{ago: 3*time.Hour + time.Duration(i)*time.Minute, tmpl: "POST /mcp", op: "whois", status: 200,
				err: "timeout", dur: 3000, ip: "192.0.2.4", account: "a1"})
	}
	got, n = factsDigest(t, apiRig(t, calls), &fakeModel{}, apiSite)
	if want := "496a91e804de872efd42018d7e0d1611f775d8e29f9f7b1a06941de96b8e4c76"; got != want || n != 3 {
		t.Errorf("API: %d inputs, digest %s, want 3, %s", n, got, want)
	}
	// Script errors: one hidden error from three visitors (a finding), another from two (none), a plain one.
	r := newRig(t, 0, 0, nil)
	site, _ := r.s.Sites.Lookup(feedSite)
	var evs []store.Event
	for i, e := range []struct {
		name    string
		visitor int
	}{{"Script error.", 1}, {"Script error.", 2}, {"Script error.", 3}, {"script error", 4}, {"script error", 5},
		{"script error", 4}, {"TypeError: z", 6}} {
		evs = append(evs, store.Event{SiteID: site.ID, TS: r.end.Add(-2*time.Hour).UnixMilli() + int64(i)*1000,
			Source: "browser", Kind: "js_error", Name: e.name, Visitor: fmt.Sprint("v", e.visitor), IP: "192.0.2.70",
			IPKey: "192.0.2.70", UA: "Mozilla/5.0", Template: "/", Path: "/"})
	}
	if err := r.s.DB.InsertEvents(t.Context(), evs); err != nil {
		t.Fatal(err)
	}
	got, n = factsDigest(t, r, &fakeModel{}, feedSite)
	if want := "6b8fe77b0c09e51aa3cf019570a69d2c140d5a48fb12bc97cf74bc560c1d42e6"; got != want || n != 1 {
		t.Errorf("masked: %d inputs, digest %s, want 1, %s", n, got, want)
	}
	// The watch's: a flipped path and a new script host.
	wr, _ := watchRig(t)
	got, n = factsDigest(t, wr, &fakeModel{}, feedSite, plainSite)
	if want := "f17ad3c789bd30ddc8c837a069d8fcce67fef424574f34ce73e7a744057fc7ad"; got != want || n != 3 {
		t.Errorf("watch: %d inputs, digest %s, want 3, %s", n, got, want)
	}
}
