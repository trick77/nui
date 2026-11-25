package alert

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// FileName is the notify file's name in the conf directory.
const FileName = "notify.json"

// DefaultMaxPerHour is the most messages a channel is sent an hour unless it says otherwise: a storm queues, it
// does not flood a phone.
const DefaultMaxPerHour = 10

// DefaultCooldown is how long after a key left its set a re-entry stays quiet unless the rule says otherwise.
// Two hours, safely past the facts' hourly pass: a key missing from one pass and back at the next stays quiet.
const DefaultCooldown = 2 * time.Hour

// Channel is one named delivery target. Its URL holds the service's token: unexported, never logged, never shown.
type Channel struct {
	Name string
	// Service is the URL's scheme (ntfy, smtp, …): what the dashboard and the log name.
	Service     string
	MinSeverity Severity
	MaxPerHour  int
	url         string
}

// URL is the channel's service URL, token and all: for the sender only.
func (c Channel) URL() string { return c.url }

// LogValue keeps the URL out of every log line.
func (c Channel) LogValue() slog.Value {
	return slog.GroupValue(slog.String("name", c.Name), slog.String("service", c.Service))
}

// Rule is one rule as the file declares it, its defaults filled in.
type Rule struct {
	// Name is the rule's identity: baseline, cooldown and the alerts' rule column go by it, not by its place.
	Name     string
	Spec     Spec
	Enabled  bool
	Sites    []string
	Products []string
	// Channels are the channels it sends to; empty records without sending.
	Channels []string
	Cooldown time.Duration
	Severity Severity
	// For is how long a condition holds before it is reported, Repeat how often an open one is reported again (0:
	// never).
	For, Repeat time.Duration
	P           Params
	// Sig identifies the trigger and its parameters: a rule whose Sig changed starts a new baseline.
	Sig string
}

// Config is the notify file as read: never an error, its faults inside. A file that is missing, has faults or has
// no rule records the catalogue's defaults and sends nothing.
type Config struct {
	// Configured: the file exists.
	Configured bool
	// Enabled is the top-level switch: off, the rules still record, nothing is sent.
	Enabled  bool
	Channels []Channel
	Rules    []Rule
	// Faults are every fault the file has, all at once; any of them and nothing is sent.
	Faults []string
	// Defaulted: Rules are the catalogue's defaults, recorded only.
	Defaulted bool
}

// Sending reports whether anything may be sent: a file without faults, switched on, with a channel.
func (c *Config) Sending() bool {
	return c.Configured && c.Enabled && len(c.Faults) == 0 && !c.Defaulted && len(c.Channels) > 0
}

// WithFaults is c with faults found beyond the file's own reading (a channel the sender refuses): like any other,
// they leave the defaults recording and nothing sent.
func (c Config) WithFaults(faults ...string) Config {
	if len(faults) == 0 {
		return c
	}
	c.Faults = append(slices.Clone(c.Faults), faults...)
	return defaulted(c)
}

// Channel is the named channel.
func (c *Config) Channel(name string) (Channel, bool) {
	i := slices.IndexFunc(c.Channels, func(ch Channel) bool { return ch.Name == name })
	if i < 0 {
		return Channel{}, false
	}
	return c.Channels[i], true
}

// Status is the notifications' state as the dashboard's footer shows it. No URL: a channel is its name and service.
type Status struct {
	Configured, Enabled bool
	Faults              []string
	// Active and Off count the rules switched on and off.
	Active, Off int
	Channels    []ChannelStatus
}

// ChannelStatus is one channel's last delivery and last failure (zero: none since its first use).
type ChannelStatus struct {
	Name, Service string
	MinSeverity   Severity
	LastOK        time.Time
	ErrorAt       time.Time
	// Error is the last failure's text, scrubbed.
	Error string
}

// Known is what the rules' selectors are checked against: the sites file's domains and products.
type Known struct {
	Domains, Products []string
}

// Load reads the notify file at path. A missing file is no fault: nothing is sent, the defaults record.
func Load(path string, k Known) Config {
	b, err := os.ReadFile(path) //nolint:gosec // the operator's configured file
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return defaulted(Config{Enabled: true})
	case err != nil:
		return defaulted(Config{Configured: true, Faults: []string{"cannot read the file: " + errText(err)}})
	}
	return Parse(b, k)
}

// errText is an error's text with any URL in it cut out.
func errText(err error) string { return Scrub(err.Error()) }

// Parse reads a notify file's content, every fault at once.
func Parse(b []byte, k Known) Config {
	c := Config{Configured: true, Enabled: true}
	var f struct {
		Enabled  *bool                      `json:"enabled"`
		Channels map[string]json.RawMessage `json:"channels"`
		Defaults json.RawMessage            `json:"defaults"`
		Rules    []json.RawMessage          `json:"rules"`
	}
	if err := strict(b, &f); err != nil {
		c.Faults = append(c.Faults, "notify file: "+jsonFault(err))
		return defaulted(c)
	}
	if f.Enabled != nil {
		c.Enabled = *f.Enabled
	}
	fault := func(format string, a ...any) { c.Faults = append(c.Faults, fmt.Sprintf(format, a...)) }
	names := make([]string, 0, len(f.Channels))
	for n := range f.Channels {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		ch, err := parseChannel(n, f.Channels[n])
		if err != nil {
			fault("channel %q: %s", n, err)
			continue
		}
		c.Channels = append(c.Channels, ch)
	}
	def := ruleDefaults{Cooldown: DefaultCooldown, Severity: Normal}
	if len(f.Defaults) > 0 {
		if err := def.parse(f.Defaults, &c); err != nil {
			fault("defaults: %s", err)
		}
	}
	seen := map[string]bool{}
	for i, raw := range f.Rules {
		r, errs := parseRule(raw, def, &c, k)
		label := fmt.Sprintf("rule %d", i+1)
		if r.Name != "" {
			label += fmt.Sprintf(" (%s)", r.Name)
		}
		for _, e := range errs {
			fault("%s: %s", label, e)
		}
		if r.Name != "" && seen[r.Name] {
			fault("%s: the name is taken by an earlier rule", label)
		}
		seen[r.Name] = true
		c.Rules = append(c.Rules, r)
	}
	if len(c.Faults) > 0 || len(c.Rules) == 0 {
		return defaulted(c)
	}
	return c
}

// defaulted is c recording the catalogue's defaults, sending nothing.
func defaulted(c Config) Config {
	c.Rules, c.Defaulted = DefaultRules(), true
	return c
}

// DefaultRules are one rule per trigger that needs no parameter of its own, named by its id: what Trakk records
// without a notify file.
func DefaultRules() []Rule {
	var out []Rule
	for _, s := range Catalogue {
		if len(s.Required) > 0 {
			continue
		}
		r := Rule{Name: s.ID, Spec: s, Enabled: true, Cooldown: DefaultCooldown, Severity: Normal, P: s.Defaults}
		r.P.Jobs, r.P.Origins = slices.Clone(r.P.Jobs), slices.Clone(r.P.Origins)
		r.Sig = signature(r)
		out = append(out, r)
	}
	return out
}

// strict decodes b into v refusing unknown fields and anything after the value.
func strict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}

// jsonFault is a decoding error as a fault: Go's messages name fields and types, never a value.
func jsonFault(err error) string {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Sprintf("not JSON at byte %d", se.Offset)
	}
	return strings.TrimPrefix(Scrub(err.Error()), "json: ")
}

func parseChannel(name string, raw json.RawMessage) (Channel, error) {
	if !nameRE.MatchString(name) {
		return Channel{}, errors.New("a name is 1 to 64 of a-z, 0-9, '.', '_' and '-', starting with a letter or digit")
	}
	ch := Channel{Name: name, MinSeverity: Low, MaxPerHour: DefaultMaxPerHour}
	var u string
	if err := json.Unmarshal(raw, &u); err != nil {
		var o struct {
			URL         string `json:"url"`
			MinSeverity string `json:"min_severity"`
			MaxPerHour  *int   `json:"max_per_hour"`
		}
		if err := strict(raw, &o); err != nil {
			return ch, fmt.Errorf("a URL or {\"url\", \"min_severity\", \"max_per_hour\"}: %s", jsonFault(err))
		}
		u = o.URL
		if o.MinSeverity != "" {
			s, ok := ParseSeverity(o.MinSeverity)
			if !ok {
				return ch, errors.New("min_severity is low, normal or high")
			}
			ch.MinSeverity = s
		}
		if o.MaxPerHour != nil {
			if *o.MaxPerHour < 1 {
				return ch, errors.New("max_per_hour is 1 or more")
			}
			ch.MaxPerHour = *o.MaxPerHour
		}
	}
	// url.Parse's error quotes the URL, token and all: never passed on.
	p, err := url.Parse(u)
	if err != nil || p.Scheme == "" || !strings.Contains(u, "://") {
		return ch, errors.New("the URL does not parse as service://… (shoutrrr's format)")
	}
	ch.Service, ch.url = strings.ToLower(p.Scheme), u
	return ch, nil
}

// nameRE is a rule's or a channel's name: shown in the dashboard and the log as it is.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ruleDefaults are the file's defaults block.
type ruleDefaults struct {
	Channels []string
	Cooldown time.Duration
	Severity Severity
}

func (d *ruleDefaults) parse(raw json.RawMessage, c *Config) error {
	var o struct {
		Channels []string `json:"channels"`
		Cooldown *string  `json:"cooldown"`
		Severity *string  `json:"severity"`
	}
	if err := strict(raw, &o); err != nil {
		return errors.New(jsonFault(err))
	}
	var errs []error
	for _, n := range o.Channels {
		if _, ok := c.Channel(n); !ok {
			errs = append(errs, fmt.Errorf("channel %q is not among the channels", n))
		}
	}
	d.Channels = o.Channels
	if o.Cooldown != nil {
		v, err := duration(*o.Cooldown, true)
		if err != nil {
			errs = append(errs, fmt.Errorf("cooldown: %w", err))
		}
		d.Cooldown = v
	}
	if o.Severity != nil {
		s, ok := ParseSeverity(*o.Severity)
		if !ok {
			errs = append(errs, errors.New("severity is low, normal or high"))
		}
		d.Severity = s
	}
	return errors.Join(errs...)
}

// ruleJSON is one rule as written: pointers tell a value left out from a zero.
type ruleJSON struct {
	Name     string   `json:"name"`
	On       string   `json:"on"`
	Enabled  *bool    `json:"enabled"`
	Sites    []string `json:"sites"`
	Products []string `json:"products"`
	Channels []string `json:"channels"`
	Cooldown *string  `json:"cooldown"`
	Severity *string  `json:"severity"`
	For      *string  `json:"for"`
	Repeat   *string  `json:"repeat"`

	Min       *float64 `json:"min"`
	Max       *float64 `json:"max"`
	Share     *float64 `json:"share"`
	Factor    *float64 `json:"factor"`
	Change    *float64 `json:"change"`
	Drop      *float64 `json:"drop"`
	Floor     *float64 `json:"floor"`
	Calls     *float64 `json:"calls"`
	Direction *string  `json:"direction"`
	Source    *string  `json:"source"`
	Baseline  *string  `json:"baseline"`
	After     *string  `json:"after"`
	Window    *string  `json:"window"`
	Jobs      []string `json:"jobs"`
	Visitors  *float64 `json:"visitors"`
	Origins   []string `json:"origins"`
}

// set is the trigger parameters the rule wrote.
func (j *ruleJSON) set() map[string]bool {
	m := map[string]bool{pMin: j.Min != nil, pMax: j.Max != nil, pShare: j.Share != nil, pFactor: j.Factor != nil,
		pChange: j.Change != nil, pDrop: j.Drop != nil, pFloor: j.Floor != nil, pCalls: j.Calls != nil,
		pDirection: j.Direction != nil, pSource: j.Source != nil, pBaseline: j.Baseline != nil, pAfter: j.After != nil,
		pWindow: j.Window != nil, pJobs: j.Jobs != nil, pVisitors: j.Visitors != nil, pOrigins: j.Origins != nil}
	for k, v := range m {
		if !v {
			delete(m, k)
		}
	}
	return m
}

// maxWindow bounds window: a level's window is read in one statement, which spans at most a day.
const maxWindow = 24 * time.Hour

func parseRule(raw json.RawMessage, def ruleDefaults, c *Config, k Known) (Rule, []error) {
	var j ruleJSON
	if err := strict(raw, &j); err != nil {
		return Rule{}, []error{errors.New(jsonFault(err))}
	}
	r := Rule{Name: j.Name, Enabled: j.Enabled == nil || *j.Enabled, Sites: j.Sites, Products: j.Products,
		Channels: def.Channels, Cooldown: def.Cooldown, Severity: def.Severity}
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !nameRE.MatchString(j.Name) {
		bad("name: 1 to 64 of a-z, 0-9, '.', '_' and '-', starting with a letter or digit")
	}
	spec, ok := Lookup(j.On)
	if !ok {
		bad("on %q is no trigger of the catalogue (README, Notifications)", Clip(j.On, 40))
		return r, errs
	}
	r.Spec, r.P = spec, spec.Defaults
	r.P.Jobs, r.P.Origins = slices.Clone(r.P.Jobs), slices.Clone(r.P.Origins)
	given := j.set()
	for p := range given {
		if !spec.takes(p) {
			bad("%s takes no %s", spec.ID, p)
		}
	}
	for _, p := range spec.Required {
		if !given[p] {
			bad("%s needs %s", spec.ID, p)
		}
	}
	if spec.Instance && (j.Sites != nil || j.Products != nil) {
		bad("%s is about the instance: no sites or products", spec.ID)
	}
	if !spec.Condition && (j.For != nil || j.Repeat != nil) {
		bad("%s is reported once: no for or repeat", spec.ID)
	}
	for _, s := range j.Sites {
		if !slices.Contains(k.Domains, strings.ToLower(s)) {
			bad("site %q is not in the sites file", Clip(s, 80))
		}
	}
	for i, s := range r.Sites {
		r.Sites[i] = strings.ToLower(s)
	}
	for _, p := range j.Products {
		if !slices.Contains(k.Products, p) {
			bad("product %q is no product of the sites file", Clip(p, 80))
		}
	}
	if j.Channels != nil {
		r.Channels = j.Channels
	}
	for _, n := range r.Channels {
		if _, ok := c.Channel(n); !ok {
			bad("channel %q is not among the channels", Clip(n, 64))
		}
	}
	durs := []struct {
		name string
		v    *string
		into *time.Duration
		zero bool
	}{{"cooldown", j.Cooldown, &r.Cooldown, true}, {"for", j.For, &r.For, true}, {"repeat", j.Repeat, &r.Repeat, true},
		{pAfter, j.After, &r.P.After, false}, {pWindow, j.Window, &r.P.Window, false}}
	for _, d := range durs {
		if d.v == nil {
			continue
		}
		v, err := duration(*d.v, d.zero)
		if err != nil {
			bad("%s: %v", d.name, err)
		}
		*d.into = v
	}
	if r.P.Window > maxWindow {
		bad("window: at most 24h")
	}
	if j.Severity != nil {
		s, ok := ParseSeverity(*j.Severity)
		if !ok {
			bad("severity is low, normal or high")
		}
		r.Severity = s
	}
	errs = append(errs, numbers(&j, &r)...)
	if j.Direction != nil {
		r.P.Direction = *j.Direction
		if !slices.Contains([]string{"up", "down", "both"}, r.P.Direction) {
			bad("direction is up, down or both")
		}
	}
	if j.Source != nil {
		r.P.Source = *j.Source
		if r.P.Source != "browser" && r.P.Source != "server" {
			bad("source is browser or server (left out: both)")
		}
	}
	if j.Baseline != nil {
		r.P.Baseline = *j.Baseline
		if r.P.Baseline != "days" && r.P.Baseline != "weekday" {
			bad("baseline is days (the 7 days before) or weekday (the same weekday of the 4 weeks before)")
		}
	}
	if j.Jobs != nil {
		r.P.Jobs = j.Jobs
		for _, n := range j.Jobs {
			if !slices.Contains(Jobs, n) {
				bad("job %q is none of %s", Clip(n, 40), strings.Join(Jobs, ", "))
			}
		}
	}
	if j.Origins != nil {
		r.P.Origins = j.Origins
		if len(j.Origins) == 0 {
			bad("origins lists at least one of %s", strings.Join(Origins, ", "))
		}
		for _, o := range j.Origins {
			if !slices.Contains(Origins, o) {
				bad("origin %q is none of %s", Clip(o, 40), strings.Join(Origins, ", "))
			}
		}
	}
	if spec.Group == "level" && !r.P.HasMin && !r.P.HasMax && !r.P.HasFactor && !r.P.HasDrop {
		bad("%s needs min, max, factor or drop", spec.ID)
	}
	r.Sig = signature(r)
	return r, errs
}

// numbers checks and takes the rule's numeric parameters.
func numbers(j *ruleJSON, r *Rule) []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	whole := r.Spec.Group == "traffic" || r.Spec.Group == "collector"
	for _, n := range []struct {
		name   string
		v      *float64
		into   *float64
		has    *bool
		lo, hi float64
		open   bool // lo itself is refused
		whole  bool
	}{
		{pMin, j.Min, &r.P.Min, &r.P.HasMin, 0, math.Inf(1), false, whole},
		{pMax, j.Max, &r.P.Max, &r.P.HasMax, 0, math.Inf(1), false, false},
		{pShare, j.Share, &r.P.Share, nil, 0, 100, true, false},
		{pFactor, j.Factor, &r.P.Factor, &r.P.HasFactor, 1, math.Inf(1), r.Spec.Group == "level", false},
		{pChange, j.Change, &r.P.Change, nil, 0, math.Inf(1), true, false},
		{pDrop, j.Drop, &r.P.Drop, &r.P.HasDrop, 0, 100, true, false},
		{pFloor, j.Floor, &r.P.Floor, nil, 0, math.Inf(1), false, false},
		{pCalls, j.Calls, &r.P.Calls, nil, 0, math.Inf(1), false, true},
		{pVisitors, j.Visitors, &r.P.Visitors, nil, 1, math.Inf(1), false, true},
	} {
		if n.v == nil {
			continue
		}
		v := *n.v
		switch {
		case math.IsNaN(v) || v < n.lo || (n.open && v == n.lo) || v > n.hi:
			bad("%s %v is out of range", n.name, v)
		case n.whole && v != math.Trunc(v):
			bad("%s %v is no whole number", n.name, v)
		}
		*n.into = v
		if n.has != nil {
			*n.has = true
		}
	}
	if r.Spec.ID == "level.failed" && ((r.P.HasMin && r.P.Min > 100) || (r.P.HasMax && r.P.Max > 100)) {
		bad("level.failed's min and max are percent, at most 100")
	}
	return errs
}

// duration reads a Go duration (90s, 30m, 1h30m); zero only where zeroOK.
func duration(s string, zeroOK bool) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	switch {
	case err != nil:
		return 0, errors.New("not a duration such as 30m or 2h")
	case d < 0 || (d == 0 && !zeroOK):
		return 0, errors.New("must be above 0")
	}
	return d, nil
}

// signature identifies a rule's trigger and parameters, not its selectors, channels, severity or timing: a stricter
// or looser threshold under the same name starts a new baseline, or the loosened rule would report everything
// already there at once. A site that joins the selectors gets a baseline of its own anyway.
func signature(r Rule) string {
	b, _ := json.Marshal(struct {
		On string
		P  Params
	}{r.Spec.ID, r.P})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
