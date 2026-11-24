package simcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/trick77/trakk/backend/internal/sim"
	"github.com/trick77/trakk/backend/internal/sites"
	"github.com/trick77/trakk/backend/internal/store"
)

// exportRanges limits the export's ranges (tests); nil is every range.
var exportRanges []string

// maxRate bounds -rate, events a second: the scale it sets is bounded anyway, this keeps the arithmetic finite.
const maxRate = 1e6

// FixtureAnchor is where an export's history ends unless BACKEND_SIM_START says otherwise: the UI fixtures' "now".
var FixtureAnchor = time.Date(2026, 11, 22, 13, 20, 0, 0, time.UTC)

const usage = `usage: server sim <command> [flags]

Commands:
  scenarios   list the scenarios, their windows and what the dashboard shows for each
  backfill    write a simulated history into a data directory (-data)
  live        send live traffic to a running collector (-target)
  export      write the dashboard's answers over a simulated history as fixture files (-out)
  check       backfill each scenario on its own and check its expectations
  port        print the port server -sim listens on (BACKEND_SIM_PORT, its default when it does not parse)

The BACKEND_SIM_* environment knobs (README) set the world; the flags below override them.
`

// Main is server sim: args after "sim", the environment through getenv. It returns the exit code.
func Main(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	k := sim.ReadKnobs(getenv, func(s string) { log.Warn(s) })
	fl := flag.NewFlagSet("sim "+args[0], flag.ContinueOnError)
	fl.SetOutput(stderr)
	seed := fl.Uint64("seed", k.Seed, "the world's seed (BACKEND_SIM_SEED)")
	scale := fl.Float64("scale", k.Scale, "volume, 1 a small product (BACKEND_SIM_SCALE)")
	days := fl.Int("days", k.Days, "days of history (BACKEND_SIM_DAYS)")
	scenarios := fl.String("scenarios", k.Scenarios, "comma-separated scenarios, all for every one (BACKEND_SIM_SCENARIOS)")
	classify := fl.Bool("classify", k.Classify, "run the classifier over the history (BACKEND_SIM_CLASSIFY)")
	data := fl.String("data", "", "backfill: the data directory (app.db, sites.json, ref/): empty, absent or one the simulator wrote")
	wipe := fl.Bool("wipe", false, "backfill: empty a data directory the simulator wrote first, its history written anew")
	out := fl.String("out", "", "export: the directory to write the answers to")
	target := fl.String("target", "", "live: the collector's base URL, the server itself with one trusted hop (server -sim)")
	speed := fl.Float64("speed", k.Speed, "live: world time per wall time (BACKEND_SIM_SPEED)")
	rate := fl.Float64("rate", 0, "live: events a second to aim for (sets the scale)")
	dur := fl.Duration("for", 0, "live: how long to run (0: until interrupted)")
	var keys keyFlag
	fl.Var(&keys, "key", "live: a site's feed key, domain=key (repeatable; default: the simulator's own keys)")
	if err := fl.Parse(args[1:]); err != nil {
		return 2
	}
	k.Seed, k.Scale, k.Days, k.Scenarios, k.Classify, k.Speed = *seed, *scale, *days, *scenarios, *classify, *speed
	// The flags pass the knobs' own ranges; -rate, when given, a positive rate the scale it sets must hold.
	rateSet := false
	fl.Visit(func(f *flag.Flag) { rateSet = rateSet || f.Name == "rate" })
	err := k.Validate()
	if err == nil && rateSet && !(*rate > 0 && *rate <= maxRate) {
		err = fmt.Errorf("rate %v: not a number above 0 and at most %v", *rate, maxRate)
	}
	if err == nil && *dur < 0 {
		err = fmt.Errorf("for %v: below 0", *dur)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sim:", err)
		return 2
	}
	switch args[0] {
	case "scenarios":
		err = listScenarios(stdout)
	case "port":
		// The port server -sim listens on, BACKEND_SIM_PORT as read (make sim points the UI there).
		_, _ = fmt.Fprintln(stdout, k.Port)
	case "backfill":
		err = backfillCmd(ctx, k, *data, *wipe, getenv, log, stdout)
	case "live":
		err = liveCmd(ctx, k, *target, *rate, *dur, keys, log, stdout)
	case "export":
		err = exportCmd(ctx, k, *out, log, stdout)
	case "check":
		err = checkCmd(ctx, k, log, stdout)
	default:
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "sim:", err)
		return 1
	}
	return 0
}

// keyFlag collects -key domain=key.
type keyFlag map[string]string

func (k *keyFlag) String() string { return fmt.Sprint(map[string]string(*k)) }

func (k *keyFlag) Set(v string) error {
	d, key, ok := strings.Cut(v, "=")
	if !ok || d == "" || key == "" {
		return errors.New("want domain=key")
	}
	if *k == nil {
		*k = keyFlag{}
	}
	(*k)[d] = key
	return nil
}

func listScenarios(out io.Writer) error {
	dir, err := os.MkdirTemp("", "trakk-sim-ref-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ref, err := sim.OpenReference(dir)
	if err != nil {
		return err
	}
	for _, c := range sim.Catalogue() {
		window := "always"
		if c.From != 0 || c.To != 0 {
			window = fmt.Sprintf("%v to %v from the anchor", c.From, c.To)
		}
		_, _ = fmt.Fprintf(out, "%s (%s)\n  %s\n", c.Name, window, c.Story)
		sc, _ := sim.Scenarios(c.Name)
		w, err := sim.NewWorld(sim.Options{Seed: 1, Anchor: FixtureAnchor, Scale: 1, Scenarios: sc, Ref: ref})
		if err != nil {
			return err
		}
		for _, e := range w.Expectations() {
			if e.Scenario == c.Name {
				_, _ = fmt.Fprintf(out, "  - %s (%s, range %s)\n", e.Says, e.Endpoint, e.Params["range"])
			}
		}
	}
	return nil
}

// backfillCmd writes the history into a data directory: its sites file and reference data written first when it
// has none, so the server started on it sees what the backfill saw. Only into a directory the simulator may claim
// (empty, absent, or its own), never where a deployment is configured.
func backfillCmd(ctx context.Context, k sim.Knobs, dir string, wipe bool, getenv func(string) string,
	log *slog.Logger, out io.Writer) error {
	if dir == "" {
		return errors.New("backfill needs -data")
	}
	if err := RefusePublic(getenv("PUBLIC_FQDN"), "sim backfill"); err != nil {
		return err
	}
	if err := sim.Claim(dir, wipe); err != nil {
		return err
	}
	st, err := store.Open(ctx, filepath.Join(dir, store.FileName))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	ref, err := sim.OpenReference(filepath.Join(dir, "ref"))
	if err != nil {
		return err
	}
	w, err := worldFor(ctx, k, ref, st, filepath.Join(dir, "sites.json"))
	if err != nil {
		return err
	}
	s, err := sim.Backfill(ctx, st, w, sim.BackfillOptions{Days: k.Days, Classify: k.Classify, Log: log})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "backfilled %d days before %s: %s\n", k.Days, w.Anchor.Format(time.RFC3339), s)
	return nil
}

// worldFor is the knobs' world on the sites of the sites file at path: written from the world when missing, its
// sites taken (feed keys the simulator's) when present.
func worldFor(ctx context.Context, k sim.Knobs, ref *sim.Reference, st *store.Store, path string) (*sim.World, error) {
	w, err := k.World(ref, time.Now())
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := w.WriteSitesFile(path); err != nil {
			return nil, err
		}
		_, err := w.SyncSites(ctx, st)
		return w, err
	}
	cs, err := sites.Load(path)
	if err != nil {
		return nil, err
	}
	if _, err := st.SyncSites(ctx, sites.Rows(cs)); err != nil {
		return nil, err
	}
	if w.Sites, err = sim.SitesFrom(ctx, st); err != nil {
		return nil, err
	}
	return sim.NewWorld(sim.Options{Seed: w.Seed, Anchor: w.Anchor, Scale: w.Scale, Sites: w.Sites,
		Scenarios: w.Scenarios, Ref: ref})
}

func liveCmd(ctx context.Context, k sim.Knobs, target string, rate float64, dur time.Duration, keys keyFlag,
	log *slog.Logger, out io.Writer) error {
	if target == "" {
		return errors.New("live needs -target")
	}
	dir, err := os.MkdirTemp("", "trakk-sim-ref-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ref, err := sim.OpenReference(dir)
	if err != nil {
		return err
	}
	now := time.Now()
	if rate > 0 {
		if k.Scale, err = k.ScaleFor(ref, now, rate); err != nil {
			return err
		}
	}
	w, err := k.World(ref, now)
	if err != nil {
		return err
	}
	for d, key := range keys {
		s, ok := w.Site(d)
		if !ok {
			return fmt.Errorf("-key: no simulated site %s", d)
		}
		s.FeedKey = key
	}
	l := sim.NewLive(w, time.Now(), k.Speed)
	log.Info("Live traffic starts", "target", target, "scale", w.Scale, "speed", k.Speed)
	s, err := sim.Send(ctx, l, sim.SendOptions{Target: target, For: dur, Log: log})
	_, _ = fmt.Fprintln(out, "live:", s)
	return err
}

func exportCmd(ctx context.Context, k sim.Knobs, dir string, log *slog.Logger, out io.Writer) error {
	if dir == "" {
		return errors.New("export needs -out")
	}
	if k.Start.IsZero() {
		k.Start = FixtureAnchor
	}
	st, w, cleanup, err := scratchHistory(ctx, k, log)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	n, err := Export(ctx, st, w, dir, about(w, k.Days, k.Scenarios), exportRanges)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "exported %d answers to %s\n", n, dir)
	return nil
}

// scratchHistory backfills the knobs' world into a temporary directory.
func scratchHistory(ctx context.Context, k sim.Knobs, log *slog.Logger) (*store.Store, *sim.World, func(), error) {
	tmp, err := os.MkdirTemp("", "trakk-sim-")
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	fail := func(err error) (*store.Store, *sim.World, func(), error) { cleanup(); return nil, nil, nil, err }
	ref, err := sim.OpenReference(filepath.Join(tmp, "ref"))
	if err != nil {
		return fail(err)
	}
	w, err := k.World(ref, time.Now())
	if err != nil {
		return fail(err)
	}
	if err := ref.Stamp(w.Anchor); err != nil {
		return fail(err)
	}
	st, err := store.Open(ctx, filepath.Join(tmp, store.FileName))
	if err != nil {
		return fail(err)
	}
	cleanup = func() { _ = st.Close(); _ = os.RemoveAll(tmp) }
	if _, err := w.SyncSites(ctx, st); err != nil {
		return fail(err)
	}
	s, err := sim.Backfill(ctx, st, w, sim.BackfillOptions{Days: k.Days, Classify: k.Classify})
	if err != nil {
		return fail(err)
	}
	log.Info("History backfilled", "days", k.Days, "stats", s.String())
	return st, w, cleanup, nil
}

// checkWorld is Check (tests replace it).
var checkWorld = Check

// checkCmd plays every selected scenario with the baseline alone and checks what it says the dashboard shows.
func checkCmd(ctx context.Context, k sim.Knobs, log *slog.Logger, out io.Writer) error {
	sc, err := sim.Scenarios(k.Scenarios)
	if err != nil {
		return err
	}
	failed := 0
	for _, s := range sc {
		one := k
		one.Scenarios, one.Days = s.Name, max(k.Days, s.MinDays, 3)
		if one.Start.IsZero() {
			one.Start = FixtureAnchor
		}
		st, w, cleanup, err := scratchHistory(ctx, one, log)
		if err != nil {
			return err
		}
		fails, err := checkWorld(ctx, st, w)
		cleanup()
		if err != nil {
			return err
		}
		// Every failure counts, the baseline's in this run included: its expectations were checked too.
		for _, f := range fails {
			_, _ = fmt.Fprintln(out, "FAIL", f)
		}
		failed += len(fails)
		own := 0
		for _, e := range w.Expectations() {
			if e.Scenario == s.Name {
				own++
			}
		}
		_, _ = fmt.Fprintf(out, "%s: %d expectations checked (%d its own, %d the baseline's), %d failed\n", s.Name,
			len(w.Expectations()), own, len(w.Expectations())-own, len(fails))
	}
	if failed > 0 {
		return fmt.Errorf("%d expectations not met", failed)
	}
	return nil
}
