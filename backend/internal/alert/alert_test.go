package alert

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var known = Known{Domains: []string{"example.com", "api.example.com"}, Products: []string{"Lens"}}

const good = `{
  "channels": {"ops": "ntfy://ntfy.example.com/topic-secret",
               "mail": {"url": "smtp://user:secret@mail.example.com:587/?to=b@example.com", "min_severity": "high", "max_per_hour": 3}},
  "defaults": {"channels": ["ops"], "cooldown": "2h", "severity": "low"},
  "rules": [
    {"name": "new-errors", "on": "error.new", "min": 5},
    {"name": "addr-flood", "on": "spike.addr", "min": 500, "share": 25, "factor": 3, "sites": ["Example.com"]},
    {"name": "feed-down", "on": "source.silent", "source": "server", "after": "30m", "channels": ["ops", "mail"], "severity": "high", "for": "5m", "repeat": "4h"},
    {"name": "refused", "on": "collector.refused", "min": 100, "window": "15m", "products": ["Lens"]},
    {"name": "jobs", "on": "job.failed", "jobs": ["agg", "writer"], "enabled": false},
    {"name": "slow", "on": "level.p95", "window": "15m", "factor": 3, "min": 800, "baseline": "weekday"},
    {"name": "quiet", "on": "level.requests", "window": "1h", "drop": 80, "channels": []}
  ]
}`

func TestParse_Good(t *testing.T) {
	c := Parse([]byte(good), known)
	if len(c.Faults) > 0 {
		t.Fatalf("faults %v", c.Faults)
	}
	if !c.Configured || !c.Enabled || c.Defaulted || !c.Sending() || len(c.Rules) != 7 || len(c.Channels) != 2 {
		t.Fatalf("config %+v", c)
	}
	mail, _ := c.Channel("mail")
	if mail.Service != "smtp" || mail.MinSeverity != High || mail.MaxPerHour != 3 || !strings.Contains(mail.URL(), "secret") {
		t.Errorf("mail %+v", mail)
	}
	ops, _ := c.Channel("ops")
	if ops.MinSeverity != Low || ops.MaxPerHour != DefaultMaxPerHour {
		t.Errorf("ops %+v", ops)
	}
	byName := map[string]Rule{}
	for _, r := range c.Rules {
		byName[r.Name] = r
	}
	if r := byName["new-errors"]; r.P.Min != 5 || r.Cooldown != 2*time.Hour || r.Severity != Low ||
		!slices.Equal(r.Channels, []string{"ops"}) || !r.Enabled {
		t.Errorf("new-errors %+v", r)
	}
	if r := byName["addr-flood"]; r.P.Share != 25 || r.P.Factor != 3 || !slices.Equal(r.Sites, []string{"example.com"}) {
		t.Errorf("addr-flood %+v", r)
	}
	if r := byName["feed-down"]; r.P.After != 30*time.Minute || r.P.Source != "server" || r.For != 5*time.Minute ||
		r.Repeat != 4*time.Hour || r.Severity != High || len(r.Channels) != 2 {
		t.Errorf("feed-down %+v", r)
	}
	if r := byName["jobs"]; r.Enabled || !slices.Equal(r.P.Jobs, []string{"agg", "writer"}) {
		t.Errorf("jobs %+v", r)
	}
	if r := byName["slow"]; !r.P.HasMin || !r.P.HasFactor || r.P.HasDrop || r.P.Baseline != "weekday" || r.P.Floor != 20 {
		t.Errorf("slow %+v", r)
	}
	if r := byName["quiet"]; len(r.Channels) != 0 || r.P.Drop != 80 || r.P.Baseline != "days" {
		t.Errorf("quiet %+v", r)
	}
	if r := byName["new-errors"]; r.P.Visitors != 1 || !slices.Equal(r.P.Origins, DefaultOrigins) ||
		slices.Contains(r.P.Origins, OriginExtension) {
		t.Errorf("error defaults %+v", r.P)
	}
	e := Parse([]byte(`{"rules":[{"name":"e","on":"error.new","visitors":3,"origins":["own","extension"]}]}`), known)
	if len(e.Faults) > 0 || e.Rules[0].P.Visitors != 3 || !slices.Equal(e.Rules[0].P.Origins, []string{"own", "extension"}) ||
		e.Rules[0].Sig == byName["new-errors"].Sig {
		t.Errorf("error parameters %v %+v", e.Faults, e.Rules)
	}
	// The defaults: Noteworthy's thresholds.
	d := Parse([]byte(`{"rules":[{"name":"a","on":"spike.net"},{"name":"b","on":"trend.failed"}]}`), known)
	if d.Rules[0].P.Min != 200 || d.Rules[0].P.Share != 40 || d.Rules[1].P.Min != 100 || d.Rules[1].P.Change != 5 ||
		d.Rules[1].P.Direction != "both" || d.Sending() {
		t.Errorf("defaults %+v", d.Rules)
	}
}

// Every fault is reported, all at once, and never a URL.
func TestParse_Faults(t *testing.T) {
	for name, tc := range map[string]struct{ file, want string }{
		"syntax":           {`{"rules": [}`, "not JSON"},
		"top unknown":      {`{"rule": []}`, `unknown field "rule"`},
		"two values":       {`{} {}`, "more than one"},
		"channel unknown":  {`{"channels": {"a": {"url": "ntfy://x.example.com/t", "token": "s"}}}`, `unknown field "token"`},
		"channel url":      {`{"channels": {"a": "no url here"}}`, "does not parse"},
		"channel name":     {`{"channels": {"A B": "ntfy://x.example.com/t"}}`, "a name is"},
		"channel severity": {`{"channels": {"a": {"url": "ntfy://x.example.com/t", "min_severity": "urgent"}}}`, "min_severity"},
		"channel cap":      {`{"channels": {"a": {"url": "ntfy://x.example.com/t", "max_per_hour": 0}}}`, "max_per_hour"},
		"defaults unknown": {`{"defaults": {"cooldwn": "1h"}}`, `unknown field "cooldwn"`},
		"defaults channel": {`{"defaults": {"channels": ["nope"]}}`, `channel "nope"`},
		"defaults dur":     {`{"defaults": {"cooldown": "soon"}}`, "cooldown"},
		"defaults sev":     {`{"defaults": {"severity": "loud"}}`, "severity"},
		"rule unknown":     {`{"rules": [{"name": "a", "on": "error.new", "threshold": 3}]}`, `unknown field "threshold"`},
		"rule name":        {`{"rules": [{"name": "Bad Name", "on": "error.new"}]}`, "name:"},
		"rule twice":       {`{"rules": [{"name": "a", "on": "error.new"}, {"name": "a", "on": "probe.new"}]}`, "taken"},
		"no trigger":       {`{"rules": [{"name": "a", "on": "error.old"}]}`, "no trigger"},
		"foreign param":    {`{"rules": [{"name": "a", "on": "error.new", "share": 3}]}`, "takes no share"},
		"auto share min":   {`{"rules": [{"name": "a", "on": "trend.auto_share", "min": 3}]}`, "takes no min"},
		"after needed":     {`{"rules": [{"name": "a", "on": "source.silent"}]}`, "needs after"},
		"window needed":    {`{"rules": [{"name": "a", "on": "level.requests", "min": 3}]}`, "needs window"},
		"level judge":      {`{"rules": [{"name": "a", "on": "level.requests", "window": "1h"}]}`, "min, max, factor or drop"},
		"instance sites":   {`{"rules": [{"name": "a", "on": "job.failed", "sites": ["example.com"]}]}`, "about the instance"},
		"fact for":         {`{"rules": [{"name": "a", "on": "error.new", "for": "1h"}]}`, "no for or repeat"},
		"site":             {`{"rules": [{"name": "a", "on": "error.new", "sites": ["other.example.org"]}]}`, "not in the sites file"},
		"product":          {`{"rules": [{"name": "a", "on": "error.new", "products": ["Nope"]}]}`, "no product"},
		"channel":          {`{"rules": [{"name": "a", "on": "error.new", "channels": ["nope"]}]}`, `channel "nope"`},
		"duration":         {`{"rules": [{"name": "a", "on": "source.silent", "after": "0s"}]}`, "above 0"},
		"window":           {`{"rules": [{"name": "a", "on": "level.requests", "window": "48h", "min": 1}]}`, "at most 24h"},
		"severity":         {`{"rules": [{"name": "a", "on": "error.new", "severity": "urgent"}]}`, "severity"},
		"share":            {`{"rules": [{"name": "a", "on": "spike.addr", "share": 120}]}`, "share 120"},
		"whole":            {`{"rules": [{"name": "a", "on": "error.new", "min": 1.5}]}`, "no whole number"},
		"factor":           {`{"rules": [{"name": "a", "on": "level.requests", "window": "1h", "factor": 1}]}`, "factor 1"},
		"failed pct":       {`{"rules": [{"name": "a", "on": "level.failed", "window": "1h", "min": 120}]}`, "percent"},
		"direction":        {`{"rules": [{"name": "a", "on": "trend.calls", "direction": "sideways"}]}`, "direction"},
		"source":           {`{"rules": [{"name": "a", "on": "source.silent", "after": "1h", "source": "mail"}]}`, "source is"},
		"baseline":         {`{"rules": [{"name": "a", "on": "level.p95", "window": "1h", "min": 1, "baseline": "year"}]}`, "baseline"},
		"jobs":             {`{"rules": [{"name": "a", "on": "job.failed", "jobs": ["backup"]}]}`, `job "backup"`},
		"visitors":         {`{"rules": [{"name": "a", "on": "error.new", "visitors": 0}]}`, "visitors 0"},
		"visitors whole":   {`{"rules": [{"name": "a", "on": "error.new", "visitors": 2.5}]}`, "no whole number"},
		"visitors foreign": {`{"rules": [{"name": "a", "on": "probe.new", "visitors": 2}]}`, "takes no visitors"},
		"origin":           {`{"rules": [{"name": "a", "on": "error.new", "origins": ["mine"]}]}`, `origin "mine"`},
		"no origin":        {`{"rules": [{"name": "a", "on": "error.new", "origins": []}]}`, "at least one"},
		"origins foreign":  {`{"rules": [{"name": "a", "on": "failure.new", "origins": ["own"]}]}`, "takes no origins"},
	} {
		c := Parse([]byte(tc.file), known)
		joined := strings.Join(c.Faults, "\n")
		if !strings.Contains(joined, tc.want) {
			t.Errorf("%s: faults %q lack %q", name, c.Faults, tc.want)
		}
		if c.Sending() || !c.Defaulted || len(c.Rules) == 0 {
			t.Errorf("%s: a file with faults sends or records nothing: %+v", name, c)
		}
	}
	// Several at once, and no URL even when one is mistyped.
	c := Parse([]byte(`{"channels": {"a": "ntfy//user:hunter2@x.example.com"}, "rules": [{"name": "a", "on": "nope"},
		{"name": "b", "on": "error.new", "min": -1}]}`), known)
	if len(c.Faults) != 3 {
		t.Errorf("faults %q", c.Faults)
	}
	if s := strings.Join(c.Faults, " "); strings.Contains(s, "hunter2") || strings.Contains(s, "x.example.com") {
		t.Errorf("a URL in the faults: %s", s)
	}
}

// The documented example holds no fault against the example sites file.
func TestExampleFile(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "sites.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sf struct {
		Sites []struct{ Domain, Product string } `json:"sites"`
	}
	if err := json.Unmarshal(b, &sf); err != nil {
		t.Fatal(err)
	}
	var k Known
	for _, s := range sf.Sites {
		k.Domains, k.Products = append(k.Domains, s.Domain), append(k.Products, s.Product)
	}
	c := Load(filepath.Join("..", "..", "..", "docs", "notify.example.json"), k)
	if len(c.Faults) > 0 || !c.Sending() || len(c.Rules) < 8 {
		t.Errorf("example: %v %+v", c.Faults, c)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	c := Load(filepath.Join(dir, FileName), known)
	if c.Configured || !c.Defaulted || c.Sending() || len(c.Rules) != len(DefaultRules()) {
		t.Errorf("missing file: %+v", c)
	}
	for _, r := range c.Rules {
		if len(r.Spec.Required) > 0 || r.Name != r.Spec.ID || r.Sig == "" {
			t.Errorf("default rule %+v", r)
		}
	}
	p := filepath.Join(dir, "notify.json")
	if err := os.WriteFile(p, []byte(`{"rules": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := Load(p, known); !c.Configured || !c.Defaulted || c.Sending() || len(c.Faults) > 0 {
		t.Errorf("no rule: %+v", c)
	}
	if c := Load(dir, known); !c.Configured || len(c.Faults) != 1 {
		t.Errorf("unreadable: %+v", c)
	}
	if err := os.WriteFile(p, []byte(`{"enabled": false, "channels": {"a": "ntfy://x.example.com/t"},
		"rules": [{"name": "a", "on": "error.new", "channels": ["a"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := Load(p, known); c.Enabled || c.Sending() || c.Defaulted || len(c.Rules) != 1 {
		t.Errorf("switched off: %+v", c)
	}
}

// A changed threshold is a new signature; a changed selector, channel or severity is not.
func TestSignature(t *testing.T) {
	sig := func(rule string) string {
		t.Helper()
		c := Parse([]byte(`{"channels": {"a": "ntfy://x.example.com/t"}, "rules": [`+rule+`]}`), known)
		if len(c.Faults) > 0 {
			t.Fatal(c.Faults)
		}
		return c.Rules[0].Sig
	}
	base := sig(`{"name": "x", "on": "error.new", "min": 2}`)
	if base != sig(`{"name": "y", "on": "error.new", "min": 2, "sites": ["example.com"], "channels": ["a"], "severity": "high"}`) {
		t.Error("a selector changed the signature")
	}
	if base == sig(`{"name": "x", "on": "error.new", "min": 3}`) || base == sig(`{"name": "x", "on": "probe.new", "min": 2}`) {
		t.Error("a threshold or trigger kept the signature")
	}
}

func TestChannelLogValue(t *testing.T) {
	c := Parse([]byte(`{"channels": {"a": "ntfy://user:hunter2@x.example.com/t"}}`), known)
	ch, _ := c.Channel("a")
	if s := ch.LogValue().String(); strings.Contains(s, "hunter2") || !strings.Contains(s, "ntfy") {
		t.Errorf("log value %s", s)
	}
	if s := fmt.Sprintf("%v", ch.LogValue()); strings.Contains(s, "hunter2") {
		t.Errorf("formatted %s", s)
	}
}

// A zero-width space and a right-to-left override, made of their code points: never literal in the source.
var zwsp, rlo = string(rune(0x200b)), string(rune(0x202e))

func TestLine(t *testing.T) {
	for in, want := range map[string]string{
		"line\nbreak\t\x00 and  spaces ":     "line break and spaces",
		"zero" + zwsp + "width" + rlo:        "zerowidth",
		"see https://evil.example.com/@me *": "see https://evil.example.com/@me *", // defused on the way out only
	} {
		if got := Line(in, 200); got != want {
			t.Errorf("Line(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClip(t *testing.T) {
	if got := Clip(strings.Repeat("é", 50), 10); got != strings.Repeat("é", 9)+"…" {
		t.Errorf("clip %q", got)
	}
	if got := Clip("short", 10); got != "short" {
		t.Errorf("clip %q", got)
	}
	if got := ClipKey("aé", 2); got != "a" {
		t.Errorf("ClipKey %q", got)
	}
	if got := ClipKey("abc", 5); got != "abc" {
		t.Errorf("ClipKey %q", got)
	}
}

func TestScrub(t *testing.T) {
	for in, want := range map[string]string{
		`Post "https://api.example.com/x?token=s": EOF`: `Post "[url]`,
		"smtp://user:pass@mail.example.com:587 refused": "[url] refused",
		"dial user:secret@host.example.com failed":      "dial [url] failed",
	} {
		got := Scrub(in)
		if !strings.HasPrefix(got, want) || strings.Contains(got, "secret") || strings.Contains(got, "token") ||
			strings.Contains(got, "pass") {
			t.Errorf("Scrub(%q) = %q", in, got)
		}
	}
}

func TestBoard(t *testing.T) {
	at := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	b := NewBoard(func() time.Time { return at })
	Fail(b, JobAgg, errors.New("disk full"))
	Fail(b, JobWriter, nil)
	Fail(b, JobClassify, fmt.Errorf("pass: %w", context.Canceled))
	Note(nil, Condition{Kind: KindSizeCap}) // no reporter: nothing, no panic
	Fail(nil, JobAgg, nil)
	cs := b.Conditions()
	if len(cs) != 2 || cs[0].Key != JobAgg || !cs[0].Open || cs[0].Text != "disk full" || !cs[0].At.Equal(at) ||
		cs[1].Key != JobWriter || cs[1].Open {
		t.Errorf("conditions %+v", cs)
	}
	Fail(b, JobAgg, nil)
	if cs := b.Conditions(); cs[0].Open {
		t.Errorf("agg still open: %+v", cs)
	}
}

func TestSeverity(t *testing.T) {
	for _, s := range []Severity{Low, Normal, High} {
		if v, ok := ParseSeverity(s.String()); !ok || v != s {
			t.Errorf("%v", s)
		}
	}
	if _, ok := ParseSeverity("urgent"); ok {
		t.Error("urgent")
	}
}
