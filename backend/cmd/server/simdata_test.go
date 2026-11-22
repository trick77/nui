package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/nui/backend/internal/app"
	"github.com/trick77/nui/backend/internal/sim"
	"github.com/trick77/nui/backend/internal/store"
)

// -demo fills an empty database and marks the directory, serves its own history again, and refuses one with
// events it did not write.
func TestSeedDemo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sites.json"), []byte(`{"sites":[{"domain":"lens.example.com",`+
		`"product":"Lens","kind":"site"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := app.Config{DataDir: dir, SitesFile: filepath.Join(dir, "sites.json"), RefDataDisabled: true}
	st, err := store.Open(t.Context(), filepath.Join(dir, "nui.db"))
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
