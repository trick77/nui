package agg

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"

	"github.com/trick77/stakeout/backend/internal/store"
)

const (
	// freezeLag is how long after its end a day is built: beyond the collector's back-dating (an hour for the feed,
	// half an hour for the browser), its queue and a classifier pass. Today, yesterday and, until 02:00 UTC, the
	// day before stay raw.
	freezeLag = 2 * time.Hour
	// yield is the pause between two site-days: the writer's turn at the lock, as retention's.
	yield = 20 * time.Millisecond
	// progressEvery spaces the progress lines of a long backfill.
	progressEvery = 30 * time.Second
	// maxPasses bounds the passes of one wake: a pass after one that built something takes the next days its
	// rebuilds requeued; a day a writer keeps racing waits for the next wake.
	maxPasses = 3
)

// Options shape Run.
type Options struct {
	// Every is the pause between two wakes; five minutes by default. Run also wakes once at start.
	Every time.Duration
	// K is the entries a list part keeps per day; 0 is TopK.
	K int
	// Months is how long age retention keeps raw (0: no limit). A day on or before the UTC day holding the cutoff is
	// never built: retention deletes it within a day, and its raw may be partial already (0.1 pruned to the
	// millisecond), so a build would freeze an undercount for good.
	Months int
	Now    func() time.Time
	Log    *slog.Logger
}

// ageFloor is the oldest day the job builds at now: the day after the one holding the age cutoff, 0 without one.
func ageFloor(now time.Time, months int) int64 {
	if months <= 0 {
		return 0
	}
	return now.AddDate(0, -months, 0).UnixMilli()/dayMS + 1
}

// lastDue is the newest day due at now: a day is built once freezeLag has passed since the end of the day after it.
func lastDue(now time.Time) int64 {
	return (now.UnixMilli()-freezeLag.Milliseconds())/dayMS - 2
}

// Run builds every due site-day until ctx ends: the backfill of every day with raw after an upgrade, then each day as
// it falls due, and the rebuilds raw writes and a new builder ask for.
func Run(ctx context.Context, st *store.Store, o Options) {
	if o.Every <= 0 {
		o.Every = 5 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	j := &job{b: &Builder{Store: st, K: o.K}, log: o.Log, yield: yield}
	for wait := time.Duration(0); run.Sleep(ctx, wait); wait = o.Every {
		start, now := time.Now(), o.Now()
		j.floor = ageFloor(now, o.Months)
		built, err := j.passes(ctx, lastDue(now), maxPasses)
		switch {
		case err != nil && ctx.Err() == nil:
			o.Log.Warn("Aggregates failed, retried at the next wake", "built", built, logging.Err(err))
		case built > 0:
			o.Log.Info("Aggregates built", "site_days", built, "took", time.Since(start).Round(time.Millisecond).String())
		}
	}
}

// BuildThrough builds every site-day up to and including now's day, today too, until nothing is left to build, and
// returns how many it built. Tests use it: a history ends at its anchor, so even its last day is complete. It does
// not yield between site-days: nothing else writes meanwhile.
func BuildThrough(ctx context.Context, st *store.Store, now time.Time, k int) (int, error) {
	j := &job{b: &Builder{Store: st, K: k}, log: slog.New(slog.DiscardHandler)}
	return j.passes(ctx, now.UnixMilli()/dayMS, 0)
}

type job struct {
	b     *Builder
	log   *slog.Logger
	yield time.Duration // the pause between two site-days
	floor int64         // the oldest day built (ageFloor)
}

// passes runs passes until one builds nothing, at most limit (0: no limit; then a day that stays dirty is an error).
func (j *job) passes(ctx context.Context, last int64, limit int) (int, error) {
	total := 0
	for i := 0; limit == 0 || i < limit; i++ {
		n, err := j.pass(ctx, last)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
		if limit == 0 && i == 100 {
			return total, fmt.Errorf("aggregates still dirty after %d passes", i)
		}
	}
	return total, nil
}

type work struct{ site, day int64 }

// pass builds, oldest day first, every site-day from the site's oldest raw day (the floor when later) to last without
// a clean row of this builder. Days before keep what they hold.
func (j *job) pass(ctx context.Context, last int64) (int, error) {
	todo, err := j.todo(ctx, last)
	if err != nil || len(todo) == 0 {
		return 0, err
	}
	built := 0
	logged := time.Now()
	for _, w := range todo {
		if _, err := j.b.BuildDay(ctx, w.site, w.day); err != nil {
			return built, fmt.Errorf("site %d day %d: %w", w.site, w.day, err)
		}
		built++
		if time.Since(logged) >= progressEvery {
			logged = time.Now()
			j.log.Info("Aggregates building", "done", built, "of", len(todo),
				"day", time.UnixMilli(w.day*dayMS).UTC().Format(time.DateOnly))
		}
		if !run.Sleep(ctx, j.yield) {
			return built, ctx.Err()
		}
	}
	return built, nil
}

// todo lists the site-days to build, oldest first. A dirty day without raw has nothing to build from: its flag is
// cleared and its complete rows stand; one left mid-write stays out of reading.
func (j *job) todo(ctx context.Context, last int64) ([]work, error) {
	st := j.b.Store
	sites, err := st.Sites(ctx)
	if err != nil {
		return nil, err
	}
	var out []work
	for _, s := range sites {
		first, ok, err := st.OldestEventDay(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			first = last + 1
		}
		first = max(first, j.floor)
		if err := st.ClearAggDirtyBefore(ctx, s.ID, min(first, last+1)); err != nil {
			return nil, err
		}
		if first > last {
			continue
		}
		days, err := st.AggDays(ctx, s.ID, first, last)
		if err != nil {
			return nil, err
		}
		for d := first; d <= last; d++ {
			r, ok := days[d]
			switch {
			case ok && !r.Raw:
				if r.Dirty == store.AggRawChanged || r.Dirty == store.AggRequeued {
					if err := st.ClearAggDirty(ctx, s.ID, d); err != nil {
						return nil, err
					}
				}
			case ok && r.Frozen && r.Dirty == 0 && r.Builder >= j.b.builder():
			default:
				out = append(out, work{s.ID, d})
			}
		}
	}
	slices.SortFunc(out, func(a, b work) int { return cmp.Or(cmp.Compare(a.day, b.day), cmp.Compare(a.site, b.site)) })
	return out, nil
}
