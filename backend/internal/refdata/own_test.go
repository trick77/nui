package refdata

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOwn(t *testing.T) {
	own, err := ParseOwn([]byte(`{"services":{"Home Kuma":{"ipv4":["198.51.100.50"],"ipv6":["2001:db8::/64"],
	 "user_agent_patterns":["Uptime-Kuma/*"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	b := (&Bots{}).withOwn(own)
	for _, c := range []struct {
		ip, ua string
		ok     bool
	}{
		{"198.51.100.50", "Uptime-Kuma/2.5.3", true},
		{"::ffff:198.51.100.50", "Uptime-Kuma/2.5.3", true},
		{"2001:db8::7", "Uptime-Kuma/1.23.0", true},
		{"198.51.100.50", "curl/8.4.0", false},
		{"198.51.100.51", "Uptime-Kuma/2.5.3", false},
	} {
		if _, ok := b.Own(netip.MustParseAddr(c.ip), c.ua); ok != c.ok {
			t.Errorf("Own(%s, %s) = %v", c.ip, c.ua, ok)
		}
	}
	// Kept out of the list's own lookups.
	if _, ok := b.ByUA("Uptime-Kuma/2.5.3"); ok || len(b.ByIP(netip.MustParseAddr("198.51.100.50"))) > 0 {
		t.Error("own probe in the list's lookups")
	}
	var nilBots *Bots
	if _, ok := nilBots.Own(netip.MustParseAddr("198.51.100.50"), "Uptime-Kuma/2.5.3"); ok {
		t.Error("nil list has a probe")
	}
}

func TestParseOwn_Faults(t *testing.T) {
	_, err := ParseOwn([]byte(`{"services":{
	 "A":{"user_agent_patterns":["Uptime-Kuma/*"]},
	 "B":{"ipv4":["198.51.100.1"]},
	 "C":{"ipv4":["198.51.100.1"],"user_agent_patterns":["*"]},
	 "D":{"ipv4":["nope"],"user_agent_patterns":["x/*"]},
	 "E":{"ipv4":["10.0.0.0/7"],"user_agent_patterns":["x/*"]},
	 "F":{"ipv4":["198.51.100.1"],"user_agent_patterns":["x/*"],"color":"red"},
	 " G":{"ipv4":["198.51.100.1"],"user_agent_patterns":["x/*"]}}}`))
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"probe A: no address", "probe B: no user agent pattern", `probe C: user agent pattern "*"`,
		`probe D: "nope" is no address`, "probe E: range 10.0.0.0/7 is too broad", `probe "F"`, `probe " G"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v lacks %q", err, want)
		}
	}
	if _, err := ParseOwn([]byte(`{"probes":{}}`)); err == nil {
		t.Error("unknown top-level field taken")
	}
}

// A missing file is none; the probes stay through every load of the list.
func TestLoadOwn(t *testing.T) {
	dir := t.TempDir()
	if own, err := LoadOwn(filepath.Join(dir, "probes.json")); err != nil || own != nil {
		t.Fatalf("missing: %v, %v", own, err)
	}
	p := filepath.Join(dir, "probes.json")
	if err := os.WriteFile(p, []byte(`{"services":{"UptimeRobot":{"ipv4":["198.51.100.50"],
	 "user_agent_patterns":["Uptime-Kuma/*"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	own, err := LoadOwn(p)
	if err != nil {
		t.Fatal(err)
	}
	d := New(Options{Dir: dir, Own: own})
	for range 2 {
		b, err := ParseBots([]byte(`{"services":{"X":{"ipv4":["192.0.2.0/24"],"user_agent_patterns":["*X*"]}}}`),
			[]byte(`{"services":{"UptimeRobot":{"ipv4":["69.162.124.224/28"],"user_agent_patterns":["*UptimeRobot*"]}}}`))
		if err != nil {
			t.Fatal(err)
		}
		d.installBots(b)
		got := d.Bots()
		if p, ok := got.Own(netip.MustParseAddr("198.51.100.50"), "Uptime-Kuma/2.5.3"); !ok || p.Name != "UptimeRobot" {
			t.Fatalf("own probe lost: %v %v", p, ok)
		}
		if len(got.clashed) != 1 {
			t.Errorf("clashed = %v", got.clashed)
		}
		if _, ok := b.ByName("UptimeRobot"); !ok {
			t.Error("the list's own name index changed")
		}
	}
}

// A probe works before any list has loaded; a name the list holds (an agent's too) keeps the list's meaning.
func TestOwn_NoListAndNames(t *testing.T) {
	own, err := ParseOwn([]byte(`{"services":{"Home Kuma":{"ipv4":["198.51.100.50"],"user_agent_patterns":["Uptime-Kuma/*"]},
	 "GPTBot":{"ipv4":["198.51.100.60"],"user_agent_patterns":["Uptime-Kuma/*"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	d := New(Options{Dir: t.TempDir(), Own: own})
	b := d.Bots()
	if b == nil || len(b.Providers) != 0 {
		t.Fatalf("before the list: %+v", b)
	}
	if _, ok := b.Own(netip.MustParseAddr("198.51.100.50"), "Uptime-Kuma/2.5.3"); !ok {
		t.Error("probe needs a list")
	}
	if New(Options{Dir: t.TempDir()}).Bots() != nil {
		t.Error("no list, no probes: not nil")
	}
	list, err := ParseBots([]byte(`{"services":{"OpenAI":{"ipv4":["192.0.2.0/24"],"user_agent_patterns":["*GPTBot*"],
	 "agents":[{"name":"GPTBot","pattern":"*GPTBot*","purpose":"ai-training"}]}}}`),
		[]byte(`{"services":{"UptimeRobot":{"ipv4":["69.162.124.224/28"],"user_agent_patterns":["*UptimeRobot*"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	d.SetBots(list)
	b = d.Bots()
	if n, _ := b.ByName("GPTBot"); n.Service != "OpenAI" || !n.Agent || n.Purpose != "ai-training" {
		t.Errorf("the list's agent lost its meaning: %+v", n)
	}
	if n, _ := b.ByName("Home Kuma"); n.Class != "monitor" {
		t.Errorf("Home Kuma = %+v", n)
	}
	if len(b.clashed) != 1 || b.clashed[0] != "GPTBot" {
		t.Errorf("clashed = %v", b.clashed)
	}
}

func TestParseOwn_Shape(t *testing.T) {
	for _, c := range []struct{ file, want string }{
		{`{"services":{"A":{"ipv4":["198.51.100.1"],"user_agent_patterns":["x/*"]}}}{"services":{}}`,
			"more than one JSON value"},
		{`{"services":{"A":{"ipv4":["198.51.100.1"],"user_agent_patterns":["x/*"]},
		  "A":{"ipv4":["198.51.100.2"],"user_agent_patterns":["x/*"]}}}`, `probe "A" listed twice`},
		{`{"services":{"A":{"ipv6":["::ffff:198.51.100.1"],"user_agent_patterns":["x/*"]}}}`, "write it as IPv4"},
		{`{"services":{"A":{"ipv6":["fe80::1%en0"],"user_agent_patterns":["x/*"]}}}`, "is no address or range"},
	} {
		if _, err := ParseOwn([]byte(c.file)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.file, err, c.want)
		}
	}
	if own, err := ParseOwn([]byte(`{"services":null}`)); err != nil || len(own) != 0 {
		t.Errorf("null services: %v %v", own, err)
	}
}
