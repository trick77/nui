package demo

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/trick77/nui/backend/internal/store"
)

func TestGenerate(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "nui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rows, err := st.SyncSites(t.Context(), []store.Site{{Domain: "a.example.com", Kind: "site", PathRules: "[]"}})
	if err != nil {
		t.Fatal(err)
	}
	o := Options{SiteID: rows["a.example.com"].ID, Days: 2, People: 3, Seed: 1, Feed: true, Classes: true}
	n, err := Generate(t.Context(), st, o)
	if err != nil || n < 100 {
		t.Fatalf("generated %d, %v", n, err)
	}
	var newest int64
	_ = st.DB.QueryRow(`SELECT MAX(ts) FROM event`).Scan(&newest)
	if time.UnixMilli(newest).After(time.Now()) {
		t.Error("an event in the future")
	}
	var classes, kinds int
	_ = st.DB.QueryRow(`SELECT COUNT(DISTINCT class) FROM actor_class`).Scan(&classes)
	_ = st.DB.QueryRow(`SELECT COUNT(DISTINCT kind) FROM event`).Scan(&kinds)
	if classes != 4 || kinds < 4 {
		t.Errorf("classes %d, kinds %d", classes, kinds)
	}
	// Same seed, same traffic.
	o.Classes, o.End = false, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a, _ := Generate(t.Context(), st, o)
	b, _ := Generate(t.Context(), st, o)
	if a != b {
		t.Errorf("not deterministic: %d vs %d", a, b)
	}
	if hostOf("https://x") != "x" || hostOf("x") != "" {
		t.Error("hostOf")
	}
}
