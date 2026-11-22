package simcli

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/nui/backend/internal/app"
	"github.com/trick77/nui/backend/internal/sim"
	"github.com/trick77/nui/backend/internal/store"
)

// Server is server -sim's world: written into a temporary data directory (its sites file, the world's reference
// data), backfilled before the server serves, then advanced by the dashboard's requests.
type Server struct {
	Knobs sim.Knobs
	Dir   string
	World *sim.World

	live *sim.Live
}

// NewServer prepares a simulated server's data directory for the knobs, the world anchored at now.
func NewServer(k sim.Knobs, now time.Time) (*Server, error) {
	dir, err := os.MkdirTemp("", "nui-sim-")
	if err != nil {
		return nil, err
	}
	s := &Server{Knobs: k, Dir: dir}
	fail := func(err error) (*Server, error) { s.Close(); return nil, err }
	if err := sim.Mark(dir); err != nil {
		return fail(err)
	}
	ref, err := sim.OpenReference(filepath.Join(dir, "ref"))
	if err != nil {
		return fail(err)
	}
	if s.World, err = k.World(ref, now); err != nil {
		return fail(err)
	}
	if err := s.World.WriteSitesFile(filepath.Join(dir, "sites.json")); err != nil {
		return fail(err)
	}
	return s, nil
}

// RefusePublic refuses a simulator mode (what) where a public host is configured: the simulator never runs as, or
// writes into, a deployment.
func RefusePublic(publicFQDN, what string) error {
	if strings.TrimSpace(publicFQDN) != "" {
		return fmt.Errorf("%s refuses to run with PUBLIC_FQDN set: the simulator never runs as a deployment", what)
	}
	return nil
}

// Configure points the server's configuration at the simulated world: its data directory, sign-in off, the
// reference data kept as written, the knobs' port on the loopback. A configuration naming a public host is refused:
// the simulator never runs as a deployment.
//
// One trusted hop, whatever BACKEND_TRUSTED_HOPS says: sim live sends each browser's address as the one
// X-Forwarded-For entry, which a server set to 0 ignores (every visitor would be the loopback, one rate-limit key).
// Only here: the server listens on the loopback alone, its data is thrown away, and sign-in is off anyway. The
// in-process delivery sets the connection's address and no header, which every hop count reads the same.
func (s *Server) Configure(cfg *app.Config) error {
	if err := RefusePublic(cfg.PublicFQDN, "server -sim"); err != nil {
		return err
	}
	cfg.DataDir, cfg.SitesFile = s.Dir, filepath.Join(s.Dir, "sites.json")
	cfg.AuthDisabled, cfg.RefDataDisabled = true, true
	cfg.Addr = fmt.Sprintf("127.0.0.1:%d", s.Knobs.Port)
	cfg.TrustedHops = 1
	return nil
}

// Backfill writes the history before the anchor into st (the sites synced from the sites file).
func (s *Server) Backfill(ctx context.Context, st *store.Store) error {
	stats, err := sim.Backfill(ctx, st, s.World, sim.BackfillOptions{Days: s.Knobs.Days, Classify: s.Knobs.Classify})
	if err == nil {
		slog.Info("Simulated history written", "days", s.Knobs.Days, "seed", s.Knobs.Seed, "scale", s.Knobs.Scale,
			"stats", stats.String())
	}
	return err
}

// Wrap advances the world live at every request to dash, into the collector's handlers: world time is the anchor
// when Wrap is called (serving begins, the history written) and runs on from there, whatever wall time the anchor
// names (NUI_SIM_START in the past or the future).
func (s *Server) Wrap(dash http.Handler, h sim.Handlers) http.Handler {
	s.live = sim.NewLive(s.World, time.Now(), s.Knobs.Speed)
	next := s.live.Wrap(dash, h, time.Duration(s.Knobs.SlowMS)*time.Millisecond)
	pid := strconv.Itoa(os.Getpid())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// make sim waits for this process's answer before it starts the UI: not another server on the port.
		w.Header().Set(PIDHeader, pid)
		next.ServeHTTP(w, r)
	})
}

// PIDHeader names the simulated server's process on every dashboard answer.
const PIDHeader = "X-Nui-Sim-Pid"

// Close removes the data directory.
func (s *Server) Close() { _ = os.RemoveAll(s.Dir) }

// Demo is server -demo: days of the default scenarios' traffic before now on the configured sites, written into
// st. refDir holds the reference data the backfill enriches with: the world's is written there when it holds none.
func Demo(ctx context.Context, st *store.Store, days int, refDir string, log *slog.Logger) (sim.Stats, error) {
	ref, err := sim.OpenReference(refDir)
	if err != nil {
		return sim.Stats{}, err
	}
	ss, err := sim.SitesFrom(ctx, st)
	if err != nil {
		return sim.Stats{}, err
	}
	sc, err := sim.Scenarios(sim.DefaultScenarios)
	if err != nil {
		return sim.Stats{}, err
	}
	w, err := sim.NewWorld(sim.Options{Seed: 1, Anchor: time.Now(), Scale: 1, Sites: ss, Scenarios: sc, Ref: ref})
	if err != nil {
		return sim.Stats{}, err
	}
	return sim.Backfill(ctx, st, w, sim.BackfillOptions{Days: days, Classify: true, Log: log})
}
