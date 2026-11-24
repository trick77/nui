package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/trakk/backend/internal/app"
	"github.com/trick77/trakk/backend/internal/sim"
	"github.com/trick77/trakk/backend/internal/store"
)

// What the simulator leaves in a data directory changes nothing in a normal run: its reference data is discarded,
// its directory named, and refused for a deployment. Its own modes keep both; rDNS verification follows the mode
// alone.
func TestCheckDataDir(t *testing.T) {
	if !verifyBots(normalRun) || verifyBots(simRun) || verifyBots(demoRun) {
		t.Error("rDNS verification not by the mode")
	}
	dir := t.TempDir()
	ref := filepath.Join(dir, "ref")
	if err := sim.WriteReference(ref); err != nil {
		t.Fatal(err)
	}
	if err := sim.Mark(dir); err != nil {
		t.Fatal(err)
	}
	cfg := app.Config{DataDir: dir, RefDataDisabled: true}
	if err := checkDataDir(cfg, demoRun); err != nil || !sim.SimulatedReference(ref) {
		t.Fatalf("-demo: %v, reference kept %v", err, sim.SimulatedReference(ref))
	}
	pub := cfg
	pub.PublicFQDN = "trakk.example.com"
	if err := checkDataDir(pub, normalRun); err == nil || !strings.Contains(err.Error(), sim.OwnerFile) {
		t.Errorf("a deployment on simulated data: %v", err)
	}
	if err := checkDataDir(cfg, normalRun); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(ref); len(entries) != 0 {
		t.Errorf("the made-up reference data left: %v", entries)
	}
	// Real reference data, unmarked, is never touched.
	if err := os.WriteFile(filepath.Join(ref, "crawlers.json"), []byte(`{"services":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkDataDir(cfg, normalRun); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ref, "crawlers.json")); err != nil {
		t.Error(err)
	}
}

// -demo fills an empty database and marks the directory, serves its own history again, and refuses one with
// events it did not write.
func TestSeedDemo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sites.json"), []byte(`{"sites":[{"domain":"lens.example.com",`+
		`"product":"Lens","kind":"site"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := app.Config{DataDir: dir, SitesFile: filepath.Join(dir, "sites.json"), RefDataDisabled: true}
	st, err := store.Open(t.Context(), filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if _, err := loadSites(t.Context(), cfg, st); err != nil {
		t.Fatal(err)
	}
	if err := seedDemo(t.Context(), st, cfg, 1); err != nil || !sim.Owned(dir) {
		t.Fatalf("an empty database: %v, marked %v", err, sim.Owned(dir))
	}
	var n int
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM event`).Scan(&n)
	if err := seedDemo(t.Context(), st, cfg, 1); err != nil {
		t.Fatalf("its own history again: %v", err)
	}
	var again int
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM event`).Scan(&again)
	if n == 0 || again != n {
		t.Errorf("events %d, after a second start %d", n, again)
	}
	if err := os.Remove(filepath.Join(dir, sim.OwnerFile)); err != nil {
		t.Fatal(err)
	}
	if err := seedDemo(t.Context(), st, cfg, 1); err == nil || !strings.Contains(err.Error(), "refuses") {
		t.Errorf("events the simulator did not write: %v", err)
	}
}
