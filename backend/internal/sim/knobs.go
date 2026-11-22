package sim

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Knobs are the simulator's settings, read from NUI_SIM_* environment variables. A value that does not parse or is
// out of range falls back to its default with a warning: a typo must not poison the world.
type Knobs struct {
	Seed  uint64  // NUI_SIM_SEED: the world's seed
	Scale float64 // NUI_SIM_SCALE: volume, 1 a small product
	Speed float64 // NUI_SIM_SPEED: world time per wall time, live
	Port  int     // NUI_SIM_PORT: the simulated server's port
	Days  int     // NUI_SIM_DAYS: history backfilled before live time starts
	// Scenarios (NUI_SIM_SCENARIOS) is a comma-separated list, "all" for every one.
	Scenarios string
	// Start (NUI_SIM_START, RFC 3339) is the anchor: the end of the history. Zero: now.
	Start time.Time
	// Classify (NUI_SIM_CLASSIFY) runs the classifier's pass over the backfill.
	Classify bool
	// The awkward behaviours, live from a minute after the start. FeedOutage (NUI_SIM_FEED_OUTAGE, minutes): the
	// first feed site's emitter is down that long. Flood (NUI_SIM_FLOOD: addr, 64 or 48): a flood for ten minutes.
	// LateFeed (NUI_SIM_LATE_FEED, minutes or "never"): the last feed site's feed has not delivered yet and starts
	// then; -1 is off. SlowMS (NUI_SIM_SLOW_MS): every dashboard answer waits that long. EmptySite
	// (NUI_SIM_EMPTY_SITE): one more site, configured, that never delivers.
	FeedOutage time.Duration
	Flood      string
	LateFeed   time.Duration
	SlowMS     int
	EmptySite  bool
}

// DefaultKnobs are the knobs without any environment.
func DefaultKnobs() Knobs {
	return Knobs{Seed: 1, Scale: 1, Speed: 1, Port: 8080, Days: 30, Scenarios: DefaultScenarios, Classify: true,
		LateFeed: -1}
}

// ReadKnobs reads the knobs from getenv; warn gets one line per value it had to ignore.
func ReadKnobs(getenv func(string) string, warn func(string)) Knobs {
	k := DefaultKnobs()
	bad := func(name, raw, want string, fallback any) {
		warn(fmt.Sprintf("%s=%s is not %s; using %v", name, raw, want, fallback))
	}
	num := func(name string, fallback, lo, hi float64) float64 {
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			return fallback
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || v < lo || v > hi {
			bad(name, raw, "a number from "+strconv.FormatFloat(lo, 'f', -1, 64)+" to "+
				strconv.FormatFloat(hi, 'f', -1, 64), fallback)
			return fallback
		}
		return v
	}
	flag := func(name string, fallback bool) bool {
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			return fallback
		}
		v, err := strconv.ParseBool(raw)
		if err != nil {
			bad(name, raw, "true or false", fallback)
			return fallback
		}
		return v
	}
	whole := func(name string, fallback, lo, hi int) int {
		v := num(name, float64(fallback), float64(lo), float64(hi))
		if v != math.Trunc(v) {
			bad(name, getenv(name), "a whole number", fallback)
			return fallback
		}
		return int(v)
	}
	if raw := strings.TrimSpace(getenv("NUI_SIM_SEED")); raw != "" {
		if s, err := strconv.ParseUint(raw, 10, 64); err == nil {
			k.Seed = s
		} else {
			bad("NUI_SIM_SEED", raw, "a whole number from 0", k.Seed)
		}
	}
	k.Scale = num("NUI_SIM_SCALE", k.Scale, 0, 10_000)
	k.Speed = num("NUI_SIM_SPEED", k.Speed, 0.01, 100_000)
	k.Port = whole("NUI_SIM_PORT", k.Port, 1, 65535)
	k.Days = whole("NUI_SIM_DAYS", k.Days, 0, 3650)
	if raw := strings.TrimSpace(getenv("NUI_SIM_SCENARIOS")); raw != "" {
		var keep []string
		for _, n := range strings.Split(raw, ",") {
			if _, err := Scenarios(n); err != nil {
				warn(fmt.Sprintf("NUI_SIM_SCENARIOS: %v; left out", err))
				continue
			}
			keep = append(keep, strings.TrimSpace(n))
		}
		k.Scenarios = strings.Join(keep, ",")
	}
	if raw := strings.TrimSpace(getenv("NUI_SIM_START")); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			k.Start = t
		} else {
			bad("NUI_SIM_START", raw, "an RFC 3339 time", "now")
		}
	}
	k.Classify = flag("NUI_SIM_CLASSIFY", k.Classify)
	k.FeedOutage = time.Duration(num("NUI_SIM_FEED_OUTAGE", 0, 0, 7*24*60) * float64(time.Minute))
	switch raw := strings.TrimSpace(getenv("NUI_SIM_FLOOD")); raw {
	case "", "addr", "64", "48":
		k.Flood = raw
	default:
		bad("NUI_SIM_FLOOD", raw, "addr, 64 or 48", "no flood")
	}
	switch raw := strings.TrimSpace(getenv("NUI_SIM_LATE_FEED")); raw {
	case "":
	case "never":
		k.LateFeed = 0
	default:
		if m := num("NUI_SIM_LATE_FEED", -1, 0, 7*24*60); m >= 0 {
			k.LateFeed = time.Duration(m * float64(time.Minute))
		}
	}
	k.SlowMS = whole("NUI_SIM_SLOW_MS", 0, 0, 60_000)
	k.EmptySite = flag("NUI_SIM_EMPTY_SITE", false)
	return k
}

// World builds the knobs' world on ref, its anchor the knobs' start or now.
func (k Knobs) World(ref *Reference, now time.Time) (*World, error) {
	sc, err := Scenarios(k.Scenarios)
	if err != nil {
		return nil, err
	}
	live := func(name string, from, to time.Duration) {
		for _, s := range sc {
			if s.Name == name {
				s.From, s.To = from, to
				return
			}
		}
		for _, c := range catalogue {
			if c.Name == name {
				c.From, c.To = from, to
				sc = append(sc, &c)
			}
		}
	}
	if k.FeedOutage > 0 {
		live("feed-outage", time.Minute, time.Minute+k.FeedOutage)
	}
	if k.Flood != "" {
		live("flood-"+k.Flood, time.Minute, 11*time.Minute)
	}
	switch {
	case k.LateFeed == 0:
		live("late-feed", 1000*day, 1001*day)
	case k.LateFeed > 0:
		live("late-feed", k.LateFeed, 1000*day)
	}
	sites := DefaultSites()
	if k.EmptySite {
		sites = append(sites, EmptySite())
	}
	anchor := k.Start
	if anchor.IsZero() {
		anchor = now
	}
	return NewWorld(Options{Seed: k.Seed, Anchor: anchor, Scale: k.Scale, Sites: sites, Scenarios: sc, Ref: ref})
}
