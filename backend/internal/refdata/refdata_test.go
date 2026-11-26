package refdata

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trick77/trakk/backend/internal/alert"
)

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestPrefixTable_NestedOwners(t *testing.T) {
	var tb PrefixTable
	tb.Insert(netip.MustParsePrefix("10.0.0.0/8"), 1)
	tb.Insert(netip.MustParsePrefix("10.1.0.0/16"), 2)
	tb.Insert(netip.MustParsePrefix("10.1.2.0/24"), 3)
	tb.Insert(netip.MustParsePrefix("10.1.2.0/24"), 4) // a later insert of the same prefix wins
	tb.Insert(netip.MustParsePrefix("192.0.2.7/32"), 5)
	tb.Insert(netip.MustParsePrefix("2001:db8::/32"), 6)
	tb.Insert(netip.MustParsePrefix("2001:db8:1::/48"), 7)
	tb.Insert(netip.MustParsePrefix("2001:db8:1:2:3::/80"), 8)
	tb.Insert(netip.MustParsePrefix("2001:db8:1:2:3:4:5:6/128"), 9)
	tb.Insert(netip.MustParsePrefix("::/0"), 10)
	tb.Insert(netip.Prefix{}, 11)
	tb.Build()
	if tb.Len() != 9 {
		t.Errorf("Len = %d", tb.Len())
	}
	for a, want := range map[string]int32{
		"10.9.9.9": 1, "10.1.9.9": 2, "10.1.2.200": 4, "192.0.2.7": 5, "::ffff:10.1.2.3": 4,
		"2001:db8:ffff::1": 6, "2001:db8:1:ffff::1": 7, "2001:db8:1:2:3:ffff::1": 8, "2001:db8:1:2:3:4:5:6": 9,
		"2a00::1": 10,
	} {
		if got, ok := tb.Lookup(addr(a)); !ok || got != want {
			t.Errorf("Lookup(%s) = %d %v, want %d", a, got, ok, want)
		}
	}
	for _, a := range []string{"11.0.0.1", "192.0.2.8"} {
		if _, ok := tb.Lookup(addr(a)); ok {
			t.Errorf("Lookup(%s) matched", a)
		}
	}
	if _, ok := tb.Lookup(netip.Addr{}); ok {
		t.Error("zero Addr matched")
	}
	if mask32(0) != 0 || mask128(u128{1, 1}, 0) != (u128{}) || cmpU32(2, 1) != 1 || cmp128(u128{2, 0}, u128{1, 0}) != 1 {
		t.Error("helpers")
	}
}

const crawlers = `{"services":{
 "Googlebot":{"ipv4":["66.249.64.0/19"],"ipv6":["2001:4860:4801::/48"],"ip_list_authoritative":true,
   "user_agent_patterns":["*Googlebot*"],"rdns_patterns":["*.googlebot.com"]},
 "Bingbot":{"ipv4":["157.55.39.0/24", "junk"],"ip_list_authoritative":false,
   "user_agent_patterns":["*bingbot*","*"],"rdns_patterns":["*.search.msn.com"]},
 "UAOnly":{"user_agent_patterns":["SomeCrawler/*"]}}}`

const monitoring = `{"services":{"UptimeRobot":{"ipv4":["69.162.124.224/28"],"ip_list_authoritative":true,
 "user_agent_patterns":["*UptimeRobot*"]}}}`

// ranged is a service with a usable range and nothing else.
const ranged = `{"ipv4":["203.0.113.0/24"],"ip_list_authoritative":true}`

func TestParseBots(t *testing.T) {
	b, err := parseBots([]byte(crawlers), []byte(monitoring))
	if err != nil {
		t.Fatal(err)
	}
	if ps := b.ByIP(addr("66.249.66.1")); len(ps) != 1 || ps[0].Name != "Googlebot" || ps[0].Class != "bot" ||
		!ps[0].Authoritative {
		t.Fatalf("ByIP = %+v", ps)
	}
	if ps := b.ByIP(addr("69.162.124.230")); len(ps) != 1 || ps[0].Class != "monitor" {
		t.Fatalf("monitor = %+v", ps)
	}
	if ps := b.ByIP(addr("157.55.39.1")); len(ps) != 1 || ps[0].Authoritative || ps[0].Name != "Bingbot" {
		t.Fatalf("bing = %+v", ps)
	}
	if b.Rejected.Count != 1 || b.Rejected.First[0] != `Bingbot "junk" (unreadable range)` {
		t.Errorf("rejected = %+v", b.Rejected)
	}
	if p, ok := b.ByUA("Mozilla/5.0 (compatible; Googlebot/2.1)"); !ok || p.Name != "Googlebot" {
		t.Fatalf("ByUA = %+v", p)
	}
	if p, ok := b.ByUA("somecrawler/1.0"); !ok || p.Name != "UAOnly" || p.HasRanges {
		t.Fatalf("UA only = %+v", p)
	}
	// The bare * pattern is dropped: it would make every client Bingbot.
	if _, ok := b.ByUA("Mozilla/5.0 Firefox"); ok {
		t.Error("a plain browser matched a bot")
	}
	var nilBots *Bots
	if ps := nilBots.ByIP(addr("66.249.66.1")); ps != nil {
		t.Error("nil bots")
	}
	if _, ok := nilBots.ByUA("x"); ok {
		t.Error("nil bots ua")
	}
	if _, ok := nilBots.Claimed(addr("66.249.66.1"), "x"); ok {
		t.Error("nil bots claim")
	}
	for _, bad := range [][2]string{{"{", monitoring}, {crawlers, `{"services":{}}`},
		{crawlers, `{"services":{"A":[],"B":7}}`},
		// More than a service in four unusable, by its shape or its ranges: the list is broken.
		{crawlers, `{"services":{"A":[],"B":{"ipv4":["192.0.2.0-192.0.2.9"]},"C":` + ranged + `,"D":` + ranged +
			`,"E":` + ranged + `,"F":` + ranged + `,"G":` + ranged + `}}`},
		// Renamed fields: entries without anything, and mostly without addresses.
		{crawlers, `{"services":{"A":{"prefixes":["192.0.2.0/24"]},"B":` + ranged + `}}`},
		{crawlers, `{"services":{"A":{"user_agent_patterns":["*A*"]},"B":{"user_agent_patterns":["*B*"]},"C":` + ranged + `}}`},
	} {
		if _, err := parseBots([]byte(bad[0]), []byte(bad[1])); err == nil {
			t.Error("bad list parsed")
		}
	}
	// Without agents (today's lists) a service has none, and its name is the service alone.
	for _, p := range b.Providers {
		if p.Agents != nil {
			t.Errorf("%s agents = %+v", p.Name, p.Agents)
		}
		if a, ok := p.Agent("Mozilla/5.0 (compatible; Googlebot/2.1)"); ok {
			t.Errorf("%s agent = %+v", p.Name, a)
		}
		if n, ok := b.ByName(p.Name); !ok || n != (Named{Service: p.Name, Class: p.Class}) {
			t.Errorf("ByName(%s) = %+v %v", p.Name, n, ok)
		}
	}
}

// name64 is the longest agent name kept.
const name64 = "a123456789012345678901234567890123456789012345678901234567890123"

// agentList declares agents: names and purposes the vendor gives each user-agent token of a service.
const agentList = `{"services":{
 "ClaudeBot":{"ipv4":["192.0.2.0/24"],"ip_list_authoritative":true,
   "user_agent_patterns":["*ClaudeBot*","*Claude-User*","*Claude-Search*"],
   "agents":[{"name":"ClaudeBot","pattern":"*ClaudeBot*","purpose":"ai-training"},
     {"name":" Claude-User ","pattern":"*Claude-User*","purpose":" AI-Assistant "},
     {"name":"Claude-Search","pattern":"*Claude-Search*","purpose":"ai-search"},
     {"name":"Claude-SearchBot","pattern":"*Claude-SearchBot*","purpose":"quantum-crawl"},
     {"name":"Claude-Typed","pattern":"*Claude-Typed*","purpose":7},
     {"name":"Claude-Under","pattern":"*Claude-Under*","purpose":"ai_training"},
     {"name":"Claude-Spaced","pattern":"*Claude-Spaced*","purpose":"AI Assistant"},
     {"name":"Claude-Long","pattern":"*Claude-Long*","purpose":"a123456789012345678901234567890123"},
     {"name":"Claude-Hidden","pattern":"*Claude-Hidden*"},
     {"pattern":"*NoName*"},{"name":"  ","pattern":"*Claude-Searcher*"},{"name":"NoPattern","purpose":"x"},
     {"name":"Star","pattern":"**"},{"name":7,"pattern":"*Seven*"},{"name":"Bad","pattern":["*Bad*"]},
     "Claude-String",null,7,
     {"name":"` + name64 + `x","pattern":"*TooLong*"},{"name":"` + name64 + `","pattern":"*Claude-Max*"},
     {"name":"Claude\u0007Bell","pattern":"*Bell*"},{"name":"Claude-Blank","pattern":"   "},
     {"name":"Claude-Padded","pattern":" *Claude-Padded* ","purpose":"ai-search"},
     {"name":"Claude Dot.Bot","pattern":"*Dot.Bot*"}]},
 "Plain":{"ipv4":["198.51.100.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Plain*"],
   "agents":{"name":"Plain","pattern":"*Plain*"}},
 "Stringy":{"ipv4":["198.51.101.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Stringy*"],
   "agents":"Stringy"},
 "Numeric":{"ipv4":["198.51.102.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Numeric*"],"agents":7},
 "Null":{"ipv4":["198.51.103.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Null*"],"agents":null},
 "Empty":{"ipv4":["198.51.104.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Empty*"],"agents":[]}}}`

func TestParseBots_Agents(t *testing.T) {
	b, err := parseBots([]byte(agentList), []byte(monitoring))
	if err != nil {
		t.Fatal(err)
	}
	// Each agent of no use and each agents value of the wrong shape, none for null or []; the first ones in order.
	if r := b.RejectedAgents; r.Count != 15 || strings.Join(r.First, ", ") != `ClaudeBot "" (agent name), `+
		`ClaudeBot "" (agent name), ClaudeBot "NoPattern" (agent pattern), ClaudeBot "Star" (agent pattern), `+
		`ClaudeBot "7" (agent name)` {
		t.Errorf("rejected agents = %+v", r)
	}
	if b.Rejected.Count != 0 {
		t.Errorf("rejected = %+v", b.Rejected)
	}
	p, ok := b.ByUA("Claude-User/1.0")
	if !ok || p.Name != "ClaudeBot" {
		t.Fatalf("ByUA = %+v", p)
	}
	var got []string
	for _, a := range p.Agents {
		got = append(got, a.Name+"="+a.Purpose)
	}
	// Trimmed and lower-cased, kept when unknown but well-formed, "" when not a word; no name, no usable pattern
	// or a wrong shape skips the agent. A name may hold spaces and dots.
	if want := "ClaudeBot=ai-training Claude-User=ai-assistant Claude-Search=ai-search " +
		"Claude-SearchBot=quantum-crawl Claude-Typed= Claude-Under= Claude-Spaced= Claude-Long= Claude-Hidden= " +
		name64 + "= Claude-Padded=ai-search Claude Dot.Bot="; strings.Join(got, " ") != want {
		t.Errorf("agents = %q, want %q", strings.Join(got, " "), want)
	}
	for ua, want := range map[string]string{
		"Mozilla/5.0 (compatible; ClaudeBot/1.0)": "ClaudeBot",
		"claude-user/1.0":                         "Claude-User",
		// The longest pattern wins: *Claude-SearchBot* over *Claude-Search*.
		"Claude-SearchBot/1.0": "Claude-SearchBot",
		"Claude-Search/1.0":    "Claude-Search",
		"Mozilla/5.0 Firefox":  "",
		"Seven/1 NoName/1":     "",
		// The pattern of an agent without a name still decides: *Claude-Searcher* names none, not *Claude-Search*.
		"Claude-Searcher/1": "",
		"TooLong/1 Bell/1":  "",
		// A pattern is trimmed.
		"Claude-Padded/1": "Claude-Padded",
		"Dot.Bot/1":       "Claude Dot.Bot",
	} {
		a, ok := p.Agent(ua)
		if a.Name != want || ok != (want != "") {
			t.Errorf("Agent(%q) = %+v %v, want %q", ua, a, ok, want)
		}
	}
	// A pattern only an agent carries names no service.
	if q, ok := b.ByUA("Claude-Hidden/1"); ok {
		t.Errorf("agent pattern named %+v", q)
	}
	for _, name := range []string{"Plain", "Stringy", "Numeric", "Null", "Empty"} {
		q, ok := b.ByUA(name + "/1")
		if !ok || q.Name != name || q.Agents != nil || len(q.Ranges) != 1 {
			t.Errorf("%s = %+v", name, q)
		}
	}
	var nilProvider *Provider
	if _, ok := nilProvider.Agent("ClaudeBot"); ok {
		t.Error("nil provider agent")
	}
}

// What an agent of no use leaves behind: one trace each, its purpose still read.
func TestParseAgents_Rejected(t *testing.T) {
	for _, c := range [][2]string{
		{``, ""},
		{`null`, ""},
		{`[]`, ""},
		{`{"name":"A","pattern":"*A*"}`, "S (unreadable agents)"},
		{`[7]`, "S (unreadable agent)"},
		{`[null]`, "S (unreadable agent)"},
		{`[{"name":"A\u0000B","pattern":"*A*"}]`, `S "A\x00B" (agent name)`},
		{`[{"name":"A\tB","pattern":"*A*","purpose":"search"}]`, `S "A\tB" (agent name)`},
		{`[{"name":"Agent é/2","pattern":"*A*"}]`, ""},
		{`[{"name":[1],"pattern":"*A*","purpose":"search"}]`, `S "[1]" (agent name)`},
		{`[{"name":"A","pattern":"  ","purpose":"search"}]`, `S "A" (agent pattern)`},
		{`[{"name":"A","pattern":7,"purpose":"search"}]`, `S "A" (agent pattern)`},
		{`[{"name":"A","pattern":" *A* ","purpose":"search"}]`, ""},
		{`[{"name":"Agent Dot.Bot/2","pattern":"*A*"}]`, ""},
	} {
		raw, want := c[0], c[1]
		var r Rejected
		agents := parseAgents("S", json.RawMessage(raw), &r)
		if got := strings.Join(r.First, ", "); got != want || r.Count != len(r.First) {
			t.Errorf("%s: rejected = %+v, want %q", raw, r, want)
		}
		if strings.Contains(raw, "search") && (len(agents) != 1 || agents[0].purpose != "search") {
			t.Errorf("%s: agents = %+v", raw, agents)
		}
	}
	// What does not print renders like another name: a zero-width space, a bidi override, a no-break space.
	for _, c := range []rune{0x200b, 0x202e, 0xa0} {
		name := "A" + string(c) + "B"
		raw, err := json.Marshal([]map[string]string{{"name": name, "pattern": "*A*", "purpose": "search"}})
		if err != nil {
			t.Fatal(err)
		}
		var r Rejected
		agents := parseAgents("S", raw, &r)
		if want := fmt.Sprintf("S %q (agent name)", name); strings.Join(r.First, ", ") != want || r.Count != 1 {
			t.Errorf("%U: rejected = %+v, want %q", c, r, want)
		}
		if len(agents) != 1 || agents[0].named || agents[0].purpose != "search" {
			t.Errorf("%U: agents = %+v", c, agents)
		}
	}
}

// One entry of an unexpected shape costs that service, not the list: the services added since still load. A range
// without its length is the one address.
func TestParseBots_UnreadableEntry(t *testing.T) {
	b, err := parseBots([]byte(`{"services":{"Odd":{"ipv4":"192.0.2.0/24"},"A":`+ranged+`,"B":`+ranged+`,"C":`+ranged+`,
		"D":`+ranged+`,"E":`+ranged+`,"F":`+ranged+`,
		"Ranges":{"ipv4":["192.0.2.0-192.0.2.9"],"ip_list_authoritative":true,"user_agent_patterns":["*Ranges*"],
		"rdns_patterns":["*","*.com","crawl-*.example.*","*example.com","*..com"," *.example.com. ","host.example.com",
		"*-bot.example.com"]},
		"Newbot":{"ipv4":["192.0.2.7"," 198.51.100.9/24 "],"ipv6":["2001:db8::7"],"ip_list_authoritative":true,
		"a_field_of_tomorrow":{"x":1}}}}`), []byte(monitoring))
	if err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]int{"192.0.2.7": 1, "192.0.2.8": 0, "2001:db8::7": 1, "2001:db8::8": 0,
		"198.51.100.200": 1} {
		if ps := b.ByIP(addr(ip)); len(ps) != want {
			t.Errorf("%s = %+v", ip, ps)
		}
	}
	// Odd, the five globs that name no domain of their own, the one range.
	if b.Rejected.Count != 7 || b.Rejected.First[0] != "Odd (unreadable entry)" {
		t.Errorf("rejected = %+v", b.Rejected)
	}
	// A service whose ranges are all refused is not one the list knows by its user agent alone.
	p, _ := b.ByUA("Ranges/1")
	if p == nil || !p.HasRanges || len(p.Ranges) != 0 {
		t.Fatalf("refused ranges: %+v", p)
	}
	for host, want := range map[string]bool{"a.example.com": true, "1-bot.example.com": true, "host.example.com": true,
		"a.com": false, "evilexample.com": false, "crawl-1.example.attacker.net": false} {
		if got := matchRdns(host, p.RdnsPatterns, anyRun); got != want {
			t.Errorf("%s = %v", host, got)
		}
	}
	// Agents of an unexpected shape are no unreadable entry: at the broken bound already, the list still loads.
	b, err = parseBots([]byte(`{"services":{"Odd":{"ipv4":"192.0.2.0/24"},
		"A":{"ipv4":["192.0.2.0/24"],"ip_list_authoritative":true,"agents":{"name":"A"}},
		"B":{"ipv4":["198.51.100.0/24"],"ip_list_authoritative":true,"agents":"B"},
		"C":{"ipv4":["203.0.113.0/24","0.0.0.0/0"],"ip_list_authoritative":true,
		  "agents":[7,"x",{"name":[1]},{"purpose":{}}]}}}`),
		[]byte(monitoring))
	if err != nil {
		t.Fatal(err)
	}
	// Each traced all the same, on its own: A's and B's values, C's four elements. More than the first few, they
	// crowd out neither C's refused range nor Odd.
	if r := b.RejectedAgents; r.Count != 6 || strings.Join(r.First, ", ") != `A (unreadable agents), `+
		`B (unreadable agents), C (unreadable agent), C (unreadable agent), C "[1]" (agent name)` {
		t.Errorf("rejected agents = %+v", r)
	}
	if b.Rejected.Count != 2 || strings.Join(b.Rejected.First, ", ") != "C 0.0.0.0/0, Odd (unreadable entry)" {
		t.Errorf("rejected = %+v", b.Rejected)
	}
	for _, ip := range []string{"192.0.2.1", "198.51.100.1", "203.0.113.1"} {
		if ps := b.ByIP(addr(ip)); len(ps) != 1 || ps[0].Agents != nil {
			t.Errorf("%s = %+v", ip, ps)
		}
	}
}

// A name stands for one thing: a service, or an agent with its service.
func TestBots_ByName(t *testing.T) {
	b, err := parseBots([]byte(`{"services":{
		"Alpha":{"ipv4":["192.0.2.0/26"],"ip_list_authoritative":true,"user_agent_patterns":["*Alpha*"],
		  "agents":[{"name":"Alpha-Index","pattern":"*Alpha-Index*","purpose":"search"},
		    {"name":"Alpha-News","pattern":"*Alpha-News*","purpose":"search"},
		    {"name":"Zeta","pattern":"*Zeta*","purpose":"ai-training"},
		    {"name":"Shared","pattern":"*Shared*","purpose":"search"},
		    {"name":"Alpha-News","pattern":"*Alpha-Newer*","purpose":"ai-training"},
		    {"name":"alpha-index","pattern":"*Alpha-Idx*","purpose":"ai-assistant"},
		    {"name":"Alpha-News","pattern":"*Alpha-Daily*","purpose":"search"}]},
		"Beta":{"ipv4":["192.0.2.64/26"],"ip_list_authoritative":true,"user_agent_patterns":["*Beta*"],
		  "agents":[{"name":"Shared","pattern":"*Beta-Shared*","purpose":"ai-training"},
		    {"name":"Beta-Any","pattern":"*Beta*","purpose":"ai-training"},
		    {"name":"Beta-Train","pattern":"*Beta-Train*","purpose":"ai-training"},
		    {"name":"Beta-Ask","pattern":"*Beta-Ask*","purpose":"ai-assistant"},
		    {"name":"alpha-news","pattern":"*Beta-News*","purpose":"search"},
		    {"name":"zeta","pattern":"*Train*","purpose":"search"}]},
		"ClaudeBot":{"ipv4":["192.0.2.128/26"],"ip_list_authoritative":true,"user_agent_patterns":["*Claude*"],
		  "agents":[{"name":"ClaudeBot","pattern":"*ClaudeBot*","purpose":"ai-training"},
		    {"name":"Claude-User","pattern":"*Claude-User*","purpose":"ai-assistant"}]},
		"Delta":{"ipv4":["198.51.101.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Delta*"],
		  "agents":[{"name":"DELTA","pattern":"*Delta*","purpose":"search"}]},
		"Eta":{"ipv4":["198.51.102.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Eta*"],
		  "agents":[{"name":"Shared","pattern":"*Eta-Shared*","purpose":"search"},
		    {"name":"ZETA","pattern":"*Eta-Zeta*","purpose":"ai-training"}]},
		"Gamma":{"ipv4":["192.0.2.192/26"],"ip_list_authoritative":true,"user_agent_patterns":["*Gamma*"],
		  "agents":[{"name":"Gamma-Bot","pattern":"*Gamma-Bot*"}]},
		"Iota":{"ipv4":["198.51.103.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Iota*"],
		  "agents":[{"name":"Iota-Bot","pattern":"*Iota-Bot*","purpose":"search"},
		    {"name":"Iota-Odd","purpose":"ai-training"},
		    {"name":"`+name64+`x","pattern":"*Iota-Long*","purpose":"ai-assistant"}]},
		"Theta":{"ipv4":["198.51.104.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Theta*"],
		  "agents":[{"name":"Theta-Bot","pattern":"*Theta-Bot*","purpose":"search"}]},
		"Zeta":{"ipv4":["198.51.100.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Zeta*"]}}}`),
		[]byte(`{"services":{"Probe":{"ipv4":["203.0.113.0/24"],"ip_list_authoritative":true,
		  "user_agent_patterns":["*Probe*"],
		  "agents":[{"name":"Alpha","pattern":"*Alpha*","purpose":"uptime"},
		    {"name":"Beta-Ask","pattern":"*Beta-Ask*","purpose":"uptime"},
		    {"name":"Probe-Check","pattern":"*Probe-Check*","purpose":"uptime"}]},
		  "Theta":{"ipv4":["198.51.105.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Theta*"],
		  "agents":[{"name":"Theta-Ping","pattern":"*Theta-Ping*","purpose":"uptime"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]Named{
		// Purposes differ, counting the agents left out: only the user agent tells them apart.
		"Alpha":       {Service: "Alpha", Class: "bot", Declared: true},
		"Alpha-Index": {Service: "Alpha", Class: "bot", Purpose: "search", Declared: true, Group: "search", Agent: true},
		// The first of two agents of one name.
		"Alpha-News": {Service: "Alpha", Class: "bot", Purpose: "search", Declared: true, Group: "search", Agent: true},
		"Beta":       {Service: "Beta", Class: "bot", Declared: true},
		"Beta-Train": {Service: "Beta", Class: "bot", Purpose: "ai-training", Declared: true, Group: "ai", Agent: true},
		"Beta-Ask":   {Service: "Beta", Class: "bot", Purpose: "ai-assistant", Declared: true, Group: "ai", Agent: true},
		// An agent named like its own service is the agent, spelled as the service.
		"ClaudeBot": {Service: "ClaudeBot", Class: "bot", Purpose: "ai-training", Declared: true, Group: "ai",
			Agent: true},
		"Claude-User": {Service: "ClaudeBot", Class: "bot", Purpose: "ai-assistant", Declared: true, Group: "ai",
			Agent: true},
		"Delta": {Service: "Delta", Class: "bot", Purpose: "search", Group: "search", Agent: true},
		// Every agent left out, their purposes still the service's.
		"Eta": {Service: "Eta", Class: "bot", Declared: true},
		// An agent without a purpose: the service shares the "".
		"Gamma":     {Service: "Gamma", Class: "bot"},
		"Gamma-Bot": {Service: "Gamma", Class: "bot", Agent: true},
		// Agents of no use still declare their purpose.
		"Iota":     {Service: "Iota", Class: "bot", Declared: true},
		"Iota-Bot": {Service: "Iota", Class: "bot", Purpose: "search", Declared: true, Group: "search", Agent: true},
		// Listed twice: the first listing's class and agents, the purposes of both.
		"Theta":     {Service: "Theta", Class: "bot", Declared: true},
		"Theta-Bot": {Service: "Theta", Class: "bot", Purpose: "search", Declared: true, Group: "search", Agent: true},
		// Named like another service: the service, the agent dropped; named like an earlier agent: that one.
		"Zeta":   {Service: "Zeta", Class: "bot"},
		"Shared": {Service: "Alpha", Class: "bot", Purpose: "search", Declared: true, Group: "search", Agent: true},
		// The agents left out share the kept one's purpose.
		"Probe":       {Service: "Probe", Class: "monitor", Purpose: "uptime", Group: "uptime"},
		"Probe-Check": {Service: "Probe", Class: "monitor", Purpose: "uptime", Group: "uptime", Agent: true},
	} {
		if got, ok := b.ByName(name); !ok || got != want {
			t.Errorf("ByName(%s) = %+v %v, want %+v", name, got, ok, want)
		}
	}
	// Iota's two agents of no use, then each pattern of a name taken or of a repeat of another purpose, and Theta's
	// second listing.
	if r := b.RejectedAgents; r.Count != 13 || strings.Join(r.First, ", ") != `Iota "Iota-Odd" (agent pattern), `+
		`Iota "`+name64[:50]+`" (agent name), Alpha "Zeta" (agent name taken), `+
		`Alpha "Alpha-News" (agent purpose differs), Alpha "alpha-index" (agent purpose differs)` {
		t.Errorf("rejected agents = %+v", r)
	}
	if b.Rejected.Count != 0 {
		t.Errorf("rejected = %+v", b.Rejected)
	}
	// A dropped agent is gone from its service too: Agent never names what ByName resolves elsewhere.
	for _, ua := range []string{"Zeta/1", "Shared/1", "Alpha/1", "Alpha-Newer/1", "Beta-Ask/1", "Probe-Check/1",
		"Beta-Shared/1", "Beta-News/1", "Train/1", "Eta-Zeta/1", "Theta-Ping/1", "Delta/1"} {
		for _, p := range b.Providers {
			if a, ok := p.Agent(ua); ok {
				if n, _ := b.ByName(a.Name); n.Service != p.Name || n.Class != p.Class || !n.Agent {
					t.Errorf("%s: Agent(%s) = %+v, named %+v", p.Name, ua, a, n)
				}
			}
		}
	}
	for svc, uas := range map[string]map[string]string{
		// One name twice, its case aside: a repeat's pattern names the agent when it declares the agent's purpose,
		// else none.
		"Alpha/1": {"Alpha-Daily/1": "Alpha-News", "Alpha-Newer/1": "", "Alpha-Idx/1": "", "Alpha-News/1": "Alpha-News",
			"Zeta/1": ""},
		// A dropped agent's longer pattern names none, a kept one's longer pattern still names its agent.
		"Beta/1": {"Beta-Shared/1": "", "Beta-News/1": "", "Beta/1": "Beta-Any", "Beta-Train/1": "Beta-Train",
			"Train/1": ""},
		"Delta/1": {"Delta/1": "Delta"},
		"Eta/1":   {"Eta-Shared/1": "", "Eta-Zeta/1": ""},
	} {
		p, _ := b.ByUA(svc)
		for ua, want := range uas {
			if a, ok := p.Agent(ua); a.Name != want || ok != (want != "") {
				t.Errorf("%s: Agent(%s) = %+v %v, want %q", p.Name, ua, a, ok, want)
			}
		}
	}
	if p, _ := b.ByUA("Alpha/1"); p == nil || len(p.Agents) != 3 {
		t.Errorf("Alpha agents = %+v", p)
	}
	if p, _ := b.ByUA("Beta/1"); p == nil || len(p.Agents) != 3 {
		t.Errorf("Beta agents = %+v", p)
	}
	// All dropped, none at all; a second listing has none.
	for _, ip := range []string{"198.51.102.1", "198.51.105.1"} {
		if ps := b.ByIP(addr(ip)); len(ps) != 1 || ps[0].Agents != nil || ps[0].agentUA != nil {
			t.Errorf("%s = %+v", ip, ps)
		}
	}
	for _, name := range []string{"Nobody", "alpha", "", "DELTA", "alpha-index", "Theta-Ping"} {
		if got, ok := b.ByName(name); ok {
			t.Errorf("ByName(%q) = %+v", name, got)
		}
	}
	var nilBots *Bots
	if _, ok := nilBots.ByName("Alpha"); ok {
		t.Error("nil bots name")
	}
}

// A service's purposes are every agent's it declares: one of no use without a purpose is the unknown one, one name
// twice with two purposes is both. Purposes that differ can share their group.
func TestBots_AgentPurposes(t *testing.T) {
	b, err := parseBots([]byte(`{"services":{
		"Kappa":{"ipv4":["192.0.2.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Kappa*"],
		  "agents":[{"name":"Kappa-Bot","pattern":"*Kappa-Bot*","purpose":"search"},{"name":"Kappa-Odd"}]},
		"Lambda":{"ipv4":["198.51.100.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Lambda*"],
		  "agents":[{"name":"Lambda-Bot","pattern":"*Lambda-Bot*","purpose":"search"},
		    {"name":"Lambda-Bot","pattern":"*Lambda-Ask*","purpose":"ai-assistant"}]},
		"Mu":{"ipv4":["198.51.101.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Mu*"],
		  "agents":[{"name":"Mu-Train","pattern":"*Mu-Train*","purpose":"ai-training"},
		    {"name":"Mu-Ask","pattern":"*Mu-Ask*","purpose":"ai-assistant"},
		    {"name":"Mu-Odd","purpose":"ai-search"}]},
		"Nu":{"ipv4":["198.51.102.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Nu*"],
		  "agents":[{"name":"Nu-Train","pattern":"*Nu-Train*","purpose":"ai-training"},{"name":"Nu-Odd"}]}}}`),
		[]byte(monitoring))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]Named{
		"Kappa":      {Service: "Kappa", Class: "bot", Declared: true},
		"Kappa-Bot":  {Service: "Kappa", Class: "bot", Purpose: "search", Declared: true, Group: "search", Agent: true},
		"Lambda":     {Service: "Lambda", Class: "bot", Declared: true},
		"Lambda-Bot": {Service: "Lambda", Class: "bot", Purpose: "search", Declared: true, Group: "search", Agent: true},
		// No purpose, one group: the one left out counted too.
		"Mu":       {Service: "Mu", Class: "bot", Declared: true, Group: "ai"},
		"Mu-Train": {Service: "Mu", Class: "bot", Purpose: "ai-training", Declared: true, Group: "ai", Agent: true},
		// One agent without a purpose: no group.
		"Nu": {Service: "Nu", Class: "bot", Declared: true},
	} {
		if got, ok := b.ByName(name); !ok || got != want {
			t.Errorf("ByName(%s) = %+v %v, want %+v", name, got, ok, want)
		}
	}
	// The repeat's pattern says the other purpose: it names none, also when the first's pattern of its length matches
	// too.
	p, _ := b.ByUA("Lambda/1")
	if len(p.Agents) != 1 {
		t.Fatalf("Lambda agents = %+v", p.Agents)
	}
	for ua, want := range map[string]bool{"Lambda-Bot/1": true, "Lambda-Ask/1": false, "Lambda-Bot Lambda-Ask/1": false,
		"Lambda-Ask Lambda-Bot/1": false} {
		if a, ok := p.Agent(ua); ok != want || ok && a != (Agent{Name: "Lambda-Bot", Purpose: "search"}) {
			t.Errorf("Agent(%s) = %+v %v", ua, a, ok)
		}
	}
}

// Of patterns of one length, one that names none wins whatever the order; of two kept agents' the first listed.
func TestProvider_AgentTie(t *testing.T) {
	b, err := parseBots([]byte(`{"services":{
		"Mu":{"ipv4":["192.0.2.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Mu*"],
		  "agents":[{"name":"Mu-Kept","pattern":"*Mu-AAAA*","purpose":"search"},{"pattern":"*Mu-BBBB*"},
		    {"name":"Zeta","pattern":"*Mu-CCCC*"},{"name":"Mu-Late","pattern":"*Mu-DDDD*","purpose":"search"}]},
		"Zeta":{"ipv4":["198.51.100.0/24"],"ip_list_authoritative":true,"user_agent_patterns":["*Zeta*"]}}}`),
		[]byte(monitoring))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := b.ByUA("Mu/1")
	for ua, want := range map[string]string{
		"Mu-AAAA/1": "Mu-Kept", "Mu-DDDD/1": "Mu-Late", "Mu-BBBB/1": "",
		// Kept, then left out for want of a name; left out for a name taken, then kept.
		"Mu-AAAA Mu-BBBB/1": "", "Mu-CCCC Mu-DDDD/1": "",
		"Mu-AAAA Mu-DDDD/1": "Mu-Kept",
	} {
		if a, ok := p.Agent(ua); a.Name != want || ok != (want != "") {
			t.Errorf("Agent(%s) = %+v %v, want %q", ua, a, ok, want)
		}
	}
}

// One name declared twice is settled at its first named declaration, with or without a pattern: that one's spelling
// and purpose. A name without a usable pattern in any declaration is no agent and takes no name.
func TestBots_AgentFirstDeclaration(t *testing.T) {
	b, err := parseBots([]byte(`{"services":{
		"Omicron":{"ipv4":["192.0.2.0/26"],"ip_list_authoritative":true,"user_agent_patterns":["*Omicron*"],
		  "agents":[{"name":"Omicron-Bot","pattern":"*Omicron-Bot*","purpose":"search"}]},
		"Xi":{"ipv4":["192.0.2.64/26"],"ip_list_authoritative":true,"user_agent_patterns":["*Xi*"],
		  "agents":[{"name":"Xi-Bot","purpose":"search"},{"name":"xi-bot","pattern":"*Xi-Bot*","purpose":"search"},
		    {"name":"Xi-Odd","purpose":"ai-training"},{"name":"xi-odd","pattern":"*Xi-Odd*","purpose":"ai-assistant"},
		    {"name":"Xi-None","purpose":"search"},{"name":"XI-NONE","pattern":7,"purpose":"search"}]},
		"Ypsilon":{"ipv4":["192.0.2.128/26"],"ip_list_authoritative":true,"user_agent_patterns":["*Ypsilon*"],
		  "agents":[{"name":"xi-none","pattern":"*Ypsilon-None*","purpose":"search"}]}}}`),
		[]byte(`{"services":{"Omicron":{"ipv4":["203.0.113.0/24"],"ip_list_authoritative":true,
		  "user_agent_patterns":["*Omicron*"],
		  "agents":[{"name":"Omicron-Ping","pattern":"*Omicron-Ping*","purpose":"search"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]Named{
		"Xi-Bot": {Service: "Xi", Class: "bot", Purpose: "search", Declared: true, Group: "search", Agent: true},
		// Its only pattern declares another purpose: the agent stands, no user agent names it.
		"Xi-Odd":  {Service: "Xi", Class: "bot", Purpose: "ai-training", Declared: true, Group: "ai", Agent: true},
		"xi-none": {Service: "Ypsilon", Class: "bot", Purpose: "search", Group: "search", Agent: true},
		// Listed twice, one purpose across both: shared.
		"Omicron":     {Service: "Omicron", Class: "bot", Purpose: "search", Group: "search"},
		"Omicron-Bot": {Service: "Omicron", Class: "bot", Purpose: "search", Group: "search", Agent: true},
	} {
		if got, ok := b.ByName(name); !ok || got != want {
			t.Errorf("ByName(%s) = %+v %v, want %+v", name, got, ok, want)
		}
	}
	for _, name := range []string{"xi-bot", "xi-odd", "Xi-None", "XI-NONE", "Omicron-Ping"} {
		if got, ok := b.ByName(name); ok {
			t.Errorf("ByName(%q) = %+v", name, got)
		}
	}
	p, _ := b.ByUA("Xi/1")
	if len(p.Agents) != 2 {
		t.Errorf("Xi agents = %+v", p.Agents)
	}
	for ua, want := range map[string]string{"Xi-Bot/1": "Xi-Bot", "Xi-Odd/1": ""} {
		if a, ok := p.Agent(ua); a.Name != want || ok != (want != "") {
			t.Errorf("Agent(%s) = %+v %v, want %q", ua, a, ok, want)
		}
	}
}

// Agents left out are logged apart from refused ranges, a line each, below the range warning.
func TestData_LogsRejectedAgents(t *testing.T) {
	b, err := parseBots([]byte(`{"services":{
		"A":{"ipv4":["192.0.2.0/24","0.0.0.0/0"],"ip_list_authoritative":true,"agents":[7,7,7,7,7,7]}}}`),
		[]byte(monitoring))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	d := New(Options{Log: slog.New(slog.NewTextHandler(&buf, nil))})
	d.installBots(b)
	var ranges, agents []string
	for line := range strings.Lines(buf.String()) {
		switch {
		case strings.Contains(line, "agents left out"):
			agents = append(agents, line)
		case strings.Contains(line, "rejected"):
			ranges = append(ranges, line)
		}
	}
	if len(ranges) != 1 || !strings.Contains(ranges[0], "level=WARN") || !strings.Contains(ranges[0], "A 0.0.0.0/0") ||
		strings.Contains(ranges[0], "agent") {
		t.Errorf("ranges logged %q", ranges)
	}
	if len(agents) != 1 || !strings.Contains(agents[0], "level=INFO") || !strings.Contains(agents[0], "count=6") {
		t.Errorf("agents logged %q", agents)
	}
}

// Services share addresses: an address has every service whose range holds it, and the user agent tells which.
func TestBots_Overlap(t *testing.T) {
	b, err := parseBots([]byte(`{"services":{
		"Index":{"ipv4":["192.0.2.0/25"],"ip_list_authoritative":true,"user_agent_patterns":["*Index*"]},
		"Search":{"ipv4":["192.0.2.0/25"],"ip_list_authoritative":true,"user_agent_patterns":["*SearchIndex*"]},
		"Wide":{"ipv4":["192.0.0.0/16"],"ip_list_authoritative":true,"user_agent_patterns":["*Wide*"]},
		"Network":{"ipv4":["192.0.2.0/28"],"user_agent_patterns":["*WideNetwork*"],"rdns_patterns":["*.example.net"]}}}`),
		[]byte(`{"services":{"Probe":{"ipv4":["192.0.2.64/26"],"ip_list_authoritative":true,
		"user_agent_patterns":["*Probe*"]},"Check":{"ipv4":["192.0.0.0/16"],"ip_list_authoritative":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	names := func(ps []*Provider) string {
		var s []string
		for _, p := range ps {
			s = append(s, p.Name)
		}
		return strings.Join(s, " ")
	}
	for _, tt := range []struct {
		ip   string
		uas  []string
		want string
	}{
		// The narrowest range first, an authoritative list before one that needs rDNS, a monitor before a crawler.
		{"192.0.2.1", nil, "Search Index Check Wide Network"},
		{"192.0.2.70", nil, "Probe Search Index Check Wide"},
		{"192.0.9.1", nil, "Check Wide"},
		{"203.0.113.1", nil, ""},
		// The user agent names its service among them, the longest pattern's.
		{"192.0.2.70", []string{"SearchIndex/1"}, "Search Probe Index Check Wide"},
		{"192.0.2.70", []string{"Wide/1"}, "Wide Probe Search Index Check"},
		// A longer pattern of a list that needs rDNS does not unname the authoritative one.
		{"192.0.2.1", []string{"WideNetwork/1"}, "Wide Search Index Check Network"},
		// The earliest (most frequent) user agent counts most, one that names nobody not at all.
		{"192.0.2.70", []string{"x", "Index/1", "Wide/1", "Index/2"}, "Index Wide Probe Search Check"},
	} {
		if got := names(Rank(b.ByIP(addr(tt.ip)), tt.uas...)); got != tt.want {
			t.Errorf("%s %v = %q, want %q", tt.ip, tt.uas, got, tt.want)
		}
	}
	// Rank leaves the table's own order alone.
	if got := names(b.ByIP(addr("192.0.2.70"))); got != "Probe Search Index Check Wide" {
		t.Errorf("after ranking: %q", got)
	}
	if p, _ := b.ByUA("SearchIndex/1"); p == nil || p.Name != "Search" {
		t.Errorf("longest pattern: %+v", p)
	}
	// A claim is the service's at the address before a longer pattern's elsewhere.
	for ip, want := range map[string]string{"192.0.2.200": "Wide", "203.0.113.1": "Search"} {
		if p, ok := b.Claimed(addr(ip), "Wide SearchIndex/1"); !ok || p.Name != want {
			t.Errorf("%s claimed %+v, want %s", ip, p, want)
		}
	}
}

func TestMatchRdns(t *testing.T) {
	pats := []string{"*.googlebot.com", "crawl-*.Googlebot.com"}
	for host, want := range map[string][2]bool{ // a claim's, another client's
		"crawl-66-249-66-1.googlebot.com.": {true, true},
		"x.googlebot.com":                  {true, true},
		"a.b.GOOGLEBOT.com":                {true, false}, // Yandex: N-N-N-N.spider.yandex.com for *.yandex.com
		"googlebot.com":                    {false, false},
		"evilgooglebot.com":                {false, false},
		"googlebot.com.evil.net":           {false, false},
		"x.googlebot.com.evil.net":         {false, false},
		"":                                 {false, false},
	} {
		if got := [2]bool{matchRdns(host, pats, anyRun), matchRdns(host, pats, oneLabel)}; got != want {
			t.Errorf("matchRdns(%q) = %v", host, got)
		}
	}
}

// tarball builds a .tar.gz of name → content.
func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "as/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var asTar = map[string]string{
	"as/15169/aggregated.json": `{"asn":15169,"metadata":{"handle":"GOOGLE","description":"Google LLC",
		"countryCode":"US","category":"hosting"},"prefixes":{"ipv4":["8.8.8.0/24"],"ipv6":["2001:4860::/32"]}}`,
	"as/64500/aggregated.json": `{"metadata":{"handle":"EX","description":"Example ISP","category":"isp"},
		"prefixes":{"ipv4":["8.0.0.0/9"]}}`,
	"as/1/aggregated.json":    `{broken`,
	"as/15169/ipv4.txt":       `8.8.8.0/24`,
	"country/ch/ch.json":      `{"countryCode":"CH","country":"Switzerland","prefixes":{"ipv4":["2.56.40.0/22"]}}`,
	"country/us/us.json":      `{"countryCode":"US","country":"United States","prefixes":{"ipv4":["8.0.0.0/8"],"ipv6":["2001:4860::/32"]}}`,
	"country/xx/xx.json":      `{broken`,
	"country/ch/ch-ipv4.txt":  `2.56.40.0/22`,
	"./country/de/de.json":    `{"countryCode":"DE","prefixes":{"ipv4":["5.0.0.0/8"]}}`,
	"geo/country/fr/fr.json":  `{"countryCode":"FR","country":"France","prefixes":{"ipv4":["2.0.0.0/12"]}}`,
	"README.md":               `# not data`,
	"as/abc/aggregated.json":  `{}`,
	"country/zz/zz-ipv6.json": `{}`,
}

func TestParseTarballs(t *testing.T) {
	b := tarball(t, asTar)
	n, err := parseNetworks(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := n.Lookup(addr("8.8.8.8")); !ok || a.ASN != 15169 || a.Name != "Google LLC" || NetworkKind(a.Category) != "hosting" {
		t.Fatalf("8.8.8.8 = %+v", a)
	}
	if a, ok := n.Lookup(addr("8.1.1.1")); !ok || a.ASN != 64500 || NetworkKind(a.Category) != "isp" {
		t.Fatalf("8.1.1.1 = %+v (nested owner)", a)
	}
	if a, ok := n.ByASN(64500); !ok || a.Handle != "EX" {
		t.Fatalf("ByASN = %+v", a)
	}
	if _, ok := n.ByASN(7); ok {
		t.Error("ByASN 7")
	}
	c, err := parseCountries(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	for a, want := range map[string]string{"2.56.41.1": "CH", "8.8.8.8": "US", "5.1.1.1": "DE", "2.1.1.1": "FR",
		"2001:4860::1": "US", "9.9.9.9": ""} {
		if got := c.Lookup(addr(a)); got != want {
			t.Errorf("country(%s) = %q, want %q", a, got, want)
		}
	}
	if c.Name("CH") != "Switzerland" || c.Name("DE") != "DE" {
		t.Errorf("names %v", c.Names)
	}
	var nilN *Networks
	var nilC *Countries
	if _, ok := nilN.Lookup(addr("8.8.8.8")); ok || nilC.Lookup(addr("8.8.8.8")) != "" || nilC.Name("CH") != "CH" {
		t.Error("nil data answers")
	}
	if _, ok := nilN.ByASN(1); ok {
		t.Error("nil ByASN")
	}
	for _, k := range []string{"business", "education_research", "government_admin", "x"} {
		_ = NetworkKind(k)
	}
	// Not gzip, an empty tarball, a tarball without the wanted files.
	if _, err := parseNetworks(strings.NewReader("nope")); err == nil {
		t.Error("not gzip parsed")
	}
	empty := tarball(t, map[string]string{"x.txt": "1"})
	if _, err := parseNetworks(bytes.NewReader(empty)); err == nil {
		t.Error("no AS files parsed")
	}
	if _, err := parseCountries(bytes.NewReader(empty)); err == nil {
		t.Error("no country files parsed")
	}
	if _, err := parseCountries(bytes.NewReader(b[:len(b)/2])); err == nil {
		t.Error("truncated tarball parsed")
	}
}

func TestLimited(t *testing.T) {
	l := &limited{r: strings.NewReader("abcdef"), left: 3}
	b, err := io.ReadAll(l)
	if err == nil || string(b) != "abc" {
		t.Errorf("limited = %q, %v", b, err)
	}
}

func TestData_RefreshCacheAndFailure(t *testing.T) {
	tb := tarball(t, asTar)
	var fail atomic.Bool
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		switch r.URL.Path {
		case "/crawlers.json":
			_, _ = io.WriteString(w, crawlers)
		case "/monitoring.json":
			_, _ = io.WriteString(w, monitoring)
		case "/as.tar.gz", "/geo.tar.gz":
			_, _ = w.Write(tb)
		case "/broken.tar.gz":
			_, _ = io.WriteString(w, "not a tarball")
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	board := alert.NewBoard(nil)
	o := Options{Dir: dir, Crawlers: srv.URL + "/crawlers.json", Monitoring: srv.URL + "/monitoring.json",
		Networks: srv.URL + "/as.tar.gz", Countries: srv.URL + "/geo.tar.gz", Log: quiet, Report: board}
	refdataOpen := func() bool {
		for _, c := range board.Conditions() {
			if c.Key == alert.JobRefdata {
				return c.Open
			}
		}
		t.Fatal("no refresh reported")
		return false
	}
	d := New(o)
	if d.Bots() != nil || d.Networks() != nil || d.Countries() != nil {
		t.Fatal("data before load")
	}
	d.Refresh(t.Context())
	if d.Bots() == nil || d.Networks() == nil || d.Countries() == nil || d.Bots().Updated.IsZero() {
		t.Fatal("refresh loaded nothing")
	}
	if refdataOpen() {
		t.Error("a refresh that got through reported a failure")
	}
	// Fresh caches: a second refresh fetches nothing.
	before := hits.Load()
	d.Refresh(t.Context())
	if hits.Load() != before {
		t.Errorf("fresh cache refetched: %d requests", hits.Load()-before)
	}
	// A restart with the network down still has the last good copy.
	fail.Store(true)
	d2 := New(o)
	d2.LoadCached()
	d2.Refresh(t.Context()) // stale check passes (fresh files), nothing fetched
	if d2.Bots() == nil || d2.Networks() == nil || d2.Countries() == nil {
		t.Fatal("cache not loaded")
	}
	// Stale caches and a failing network: every refresh fails, the data stays.
	old := time.Now().Add(-48 * time.Hour)
	for _, f := range []string{crawlersFile, monitorFile, networksFile, countriesFile} {
		_ = os.Chtimes(filepath.Join(dir, f), old, old)
	}
	d2.Refresh(t.Context())
	if d2.Networks() == nil || d2.Bots() == nil {
		t.Fatal("failed refresh dropped the data")
	}
	if !refdataOpen() {
		t.Error("a failed refresh reported nothing")
	}
	// A download that does not parse is rolled back: the previous file stays in place.
	fail.Store(false)
	o.Networks = srv.URL + "/broken.tar.gz"
	d3 := New(o)
	d3.Refresh(t.Context())
	if d3.Networks() != nil {
		t.Error("a broken tarball was installed")
	}
	if n, err := loadNetworks(d3.path(networksFile)); err != nil || len(n.AS) == 0 {
		t.Errorf("previous cache file lost: %v", err)
	}
	// Run returns when its context ends.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	New(Options{Dir: t.TempDir(), Crawlers: "http://127.0.0.1:1/x", Monitoring: "http://127.0.0.1:1/y",
		Networks: "http://127.0.0.1:1/z", Countries: "http://127.0.0.1:1/w", Log: quiet}).Run(ctx)
}

// A refresh that failed is tried again within the interval, not a whole one later.
func TestData_RunRetriesAFailedRefresh(t *testing.T) {
	tb := tarball(t, asTar)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 3 { // the first refresh: the bot list and both tarballs
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		switch r.URL.Path {
		case "/crawlers.json":
			_, _ = io.WriteString(w, crawlers)
		case "/monitoring.json":
			_, _ = io.WriteString(w, monitoring)
		default:
			_, _ = w.Write(tb)
		}
	}))
	defer srv.Close()
	d := New(Options{Dir: t.TempDir(), Crawlers: srv.URL + "/crawlers.json", Monitoring: srv.URL + "/monitoring.json",
		Networks: srv.URL + "/as.tar.gz", Countries: srv.URL + "/geo.tar.gz", Every: time.Minute,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); d.Run(ctx) }()
	deadline := time.Now().Add(20 * time.Second)
	for d.Bots() == nil || d.Networks() == nil || d.Countries() == nil {
		if time.Now().After(deadline) {
			t.Fatal("a failed refresh was not tried again within the interval")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}

// A bot list that does not parse (an error page served with 200) never replaces the good cached pair: the files on
// disk keep their content and age (so the next refresh tries again), and a restart without network still has them.
func TestData_BotListRollback(t *testing.T) {
	var html atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case html.Load() && r.URL.Path == "/monitoring.json":
			_, _ = io.WriteString(w, "<html>rate limited</html>")
		case r.URL.Path == "/crawlers.json":
			_, _ = io.WriteString(w, crawlers)
		case r.URL.Path == "/monitoring.json":
			_, _ = io.WriteString(w, monitoring)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	o := Options{Dir: dir, Crawlers: srv.URL + "/crawlers.json", Monitoring: srv.URL + "/monitoring.json",
		Networks: srv.URL + "/none", Countries: srv.URL + "/none", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	d := New(o)
	d.Refresh(t.Context())
	if d.Bots() == nil {
		t.Fatal("bot list not loaded")
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, f := range []string{crawlersFile, monitorFile} {
		_ = os.Chtimes(filepath.Join(dir, f), old, old)
	}
	html.Store(true)
	d.Refresh(t.Context())
	if d.Bots() == nil {
		t.Fatal("bot list dropped from memory")
	}
	for f, want := range map[string]string{crawlersFile: crawlers, monitorFile: monitoring} {
		got, err := os.ReadFile(filepath.Join(dir, f))
		st, _ := os.Stat(filepath.Join(dir, f))
		if err != nil || string(got) != want || !st.ModTime().Equal(old) {
			t.Errorf("%s replaced by the failed refresh: %q", f, got)
		}
	}
	if !d.stale(crawlersFile) {
		t.Error("a failed refresh marked the cache fresh")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("leftover files: %v", entries)
	}
	d2 := New(o)
	d2.LoadCached()
	if d2.Bots() == nil || len(d2.Bots().Providers) == 0 {
		t.Error("restart lost the bot list")
	}
}

// Prefixes broad enough to swallow every address are left out of every list, and counted.
func TestParse_OverbroadPrefixes(t *testing.T) {
	b, err := parseBots([]byte(`{"services":{"Evil":{"ipv4":["0.0.0.0/0","10.0.0.0/7","66.249.64.0/19"],
		"ipv6":["::/0","2001:db8::/19","::ffff:0.0.0.0/96","2001:4860::/32"],"ip_list_authoritative":true}}}`),
		[]byte(monitoring))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"203.0.113.1", "2001:db9::1", "11.1.1.1"} {
		if len(b.ByIP(addr(a))) > 0 {
			t.Errorf("%s matched an overbroad range", a)
		}
	}
	for _, a := range []string{"66.249.66.1", "2001:4860::1"} {
		if len(b.ByIP(addr(a))) == 0 {
			t.Errorf("%s lost with the plausible ranges", a)
		}
	}
	if b.Rejected.Count != 5 || len(b.Rejected.First) != 5 || b.Rejected.First[0] != "Evil 0.0.0.0/0" {
		t.Errorf("rejected %+v", b.Rejected)
	}
	n, err := parseNetworks(bytes.NewReader(tarball(t, map[string]string{
		"as/64500/aggregated.json": `{"asn":64500,"prefixes":{"ipv4":["0.0.0.0/0","192.0.2.0/24","28.0.0.0/7"],"ipv6":["::/8"]}}`})))
	if err != nil {
		t.Fatal(err)
	}
	// A /7 is real (adjacent /8s of one owner): kept.
	if _, ok := n.Lookup(addr("198.51.100.1")); ok || n.Rejected.Count != 2 {
		t.Errorf("networks: rejected %+v", n.Rejected)
	}
	if _, ok := n.Lookup(addr("29.1.1.1")); !ok {
		t.Error("a real /7 rejected")
	}
	c, err := parseCountries(bytes.NewReader(tarball(t, map[string]string{
		"country/ch/ch.json": `{"countryCode":"ch","prefixes":{"ipv4":["0.0.0.0/1","192.0.2.0/24"]}}`})))
	if err != nil {
		t.Fatal(err)
	}
	if c.Lookup(addr("10.0.0.1")) != "" || c.Lookup(addr("192.0.2.1")) != "CH" || c.Rejected.Count != 1 {
		t.Errorf("countries: rejected %+v", c.Rejected)
	}
}

type fakeResolver struct {
	ptr  map[string][]string
	host map[string][]string
	// fail are the names (addresses or hosts) whose lookup times out.
	fail map[string]bool
	// slow are the hosts whose lookup ends only with its context.
	slow  map[string]bool
	calls int
}

var errTimeout = &net.DNSError{Err: "i/o timeout", IsTimeout: true}

func (f *fakeResolver) LookupAddr(_ context.Context, a string) ([]string, error) {
	f.calls++
	if f.fail[a] {
		return nil, errTimeout
	}
	if n, ok := f.ptr[a]; ok {
		return n, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: a, IsNotFound: true}
}

func (f *fakeResolver) LookupHost(ctx context.Context, h string) ([]string, error) {
	if f.slow[h] {
		<-ctx.Done()
	}
	if f.fail[h] || ctx.Err() != nil {
		return nil, errTimeout
	}
	if a, ok := f.host[h]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: h, IsNotFound: true}
}

func TestVerifier(t *testing.T) {
	r := &fakeResolver{
		ptr: map[string][]string{
			"157.55.39.1": {"msnbot-157-55-39-1.search.msn.com."},
			"157.55.39.2": {"spoof.search.msn.com."}, // PTR claims msn, forward does not come back
			"bad":         {"x"},
		},
		host: map[string][]string{
			"msnbot-157-55-39-1.search.msn.com.": {"157.55.39.1"},
			"spoof.search.msn.com.":              {"203.0.113.9"},
		},
	}
	v := NewVerifier(r, time.Second)
	pats := []string{"*.search.msn.com"}
	// The first ask never waits for DNS: it is pending, the lookup runs off the caller's path.
	for _, ip := range []string{"157.55.39.1", "157.55.39.2", "198.51.100.1"} {
		if got := v.Check(ip, pats); got != Pending {
			t.Errorf("%s: first check %v, want pending", ip, got)
		}
	}
	if r.calls != 0 {
		t.Error("Check looked up on the caller's path")
	}
	v.Work(t.Context())
	for _, tt := range []struct {
		ip   string
		pats []string
		want Verdict
	}{
		{"157.55.39.1", pats, Confirmed},
		{"157.55.39.2", pats, Refuted},  // spoofed PTR
		{"198.51.100.1", pats, Unnamed}, // no PTR at all: an answer, but no name against the claim
		{"157.55.39.1", []string{"*.googlebot.com"}, Refuted},
		{"bad", pats, Unverified},
		{"157.55.39.1", nil, Unverified}, // a provider without rDNS patterns cannot be checked
	} {
		if got := v.Check(tt.ip, tt.pats); got != tt.want {
			t.Errorf("%s %v = %v, want %v", tt.ip, tt.pats, got, tt.want)
		}
	}
	calls := r.calls
	v.Check("157.55.39.1", pats)
	if r.calls != calls {
		t.Error("verdict not cached")
	}
	if NewVerifier(nil, time.Second).r == nil {
		t.Error("default resolver")
	}
}

// An address may have several names: any confirmed one that matches will do, also beside one whose lookup takes all
// the time there is. Names left unchecked refute nothing, and with one confirmed they are not asked again. A name
// deeper under the pattern confirms a claim only.
func TestVerifier_Names(t *testing.T) {
	r := &fakeResolver{
		ptr: map[string][]string{
			"192.0.2.1": {"slow.example.net.", "host.example.net.", "a.spider.example.com."},
			"192.0.2.2": {"slow.example.net.", "host.example.net."},
			"192.0.2.4": {"slow.example.net.", "none.example.net."},
			"192.0.2.5": {"host.example.net."},
			"192.0.2.3": {"1.", "2.", "3.", "4.", "5.", "6.", "7.", "8.", "a.example.com."},
		},
		host: map[string][]string{
			"host.example.net.":     {"192.0.2.1", "192.0.2.2"},
			"a.spider.example.com.": {"192.0.2.1"},
			"a.example.com.":        {"192.0.2.3"},
		},
		slow: map[string]bool{"slow.example.net.": true},
	}
	v := NewVerifier(r, 50*time.Millisecond)
	pats := []string{"*.example.com"}
	for _, tt := range []struct {
		ip            string
		claim, others Verdict
	}{
		{"192.0.2.1", Confirmed, Unverified},
		{"192.0.2.2", Unverified, Unverified},
		{"192.0.2.3", Unverified, Unverified},
		{"192.0.2.4", Failed, Failed},
		{"192.0.2.5", Refuted, Refuted},
	} {
		v.Check(tt.ip, pats)
		v.Work(t.Context())
		if got := v.Check(tt.ip, pats); got != tt.claim {
			t.Errorf("%s claimed = %v, want %v", tt.ip, got, tt.claim)
		}
		if got := v.CheckClose(tt.ip, pats); got != tt.others {
			t.Errorf("%s close = %v, want %v", tt.ip, got, tt.others)
		}
	}
}

// A failed lookup is no answer: the claim reads Failed, never Refuted, and is asked again after failRetry, not a day.
func TestVerifier_Failure(t *testing.T) {
	r := &fakeResolver{
		ptr:  map[string][]string{"157.55.39.3": {"msnbot-3.search.msn.com."}},
		host: map[string][]string{"msnbot-3.search.msn.com.": {"157.55.39.3"}},
		fail: map[string]bool{"157.55.39.3": true, "157.55.39.4": true, "fwd.search.msn.com.": true},
	}
	r.ptr["157.55.39.5"] = []string{"fwd.search.msn.com."}
	now := time.Unix(1_800_000_000, 0)
	v := NewVerifier(r, time.Second)
	v.now = func() time.Time { return now }
	pats := []string{"*.search.msn.com"}
	for _, ip := range []string{"157.55.39.3", "157.55.39.5"} {
		v.Check(ip, pats)
	}
	v.Work(t.Context())
	for _, ip := range []string{"157.55.39.3", "157.55.39.5"} { // reverse fails; forward fails
		if got := v.Check(ip, pats); got != Failed {
			t.Errorf("%s after a timeout = %v, want failed", ip, got)
		}
	}
	// Retried after failRetry: queued again, and answered this time.
	now = now.Add(failRetry)
	delete(r.fail, "157.55.39.3")
	v.StartPass()
	if got := v.Check("157.55.39.3", pats); got != Pending {
		t.Errorf("retry = %v, want pending", got)
	}
	v.Work(t.Context())
	if got := v.Check("157.55.39.3", pats); got != Confirmed {
		t.Errorf("after the retry = %v, want confirmed", got)
	}
	// A failure past the network's budget is unverified, not refuted.
	v.budget[networkOf(netip.MustParseAddr("157.55.39.5"))] = lookupsPerNetwork
	if got := v.Check("157.55.39.5", pats); got != Unverified {
		t.Errorf("failed past the budget = %v, want unverified", got)
	}
	// A cancelled lookup is not cached at all.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	v.StartPass()
	v.Check("157.55.39.4", pats)
	v.Work(ctx)
	if _, ok := v.cache["157.55.39.4"]; ok {
		t.Error("cut-short lookup cached")
	}
}

// A claimant rotating addresses inside one network gets that network's daily budget of lookups, no more; a pass
// queues at most LookupsPerPass; other networks are unaffected.
func TestVerifier_Budgets(t *testing.T) {
	r := &fakeResolver{}
	v := NewVerifier(r, time.Second)
	pats := []string{"*.googlebot.com"}
	pending := 0
	for i := range 200 {
		v.StartPass()
		switch v.Check(fmt.Sprintf("2001:db8:7:%x::1", i), pats) {
		case Pending:
			pending++
		case Unverified:
		default:
			t.Fatal("past the budget: neither pending nor unverified")
		}
		v.Work(t.Context())
	}
	if pending != lookupsPerNetwork || r.calls != lookupsPerNetwork {
		t.Errorf("one /48: %d pending, %d lookups, want %d", pending, r.calls, lookupsPerNetwork)
	}
	if v.Check("2001:db8:8::1", pats) != Pending {
		t.Error("another /48 refused")
	}
	// One pass: at most LookupsPerPass queued, the rest pending for a later pass.
	v2 := NewVerifier(r, time.Second)
	queued := 0
	for i := range 100 {
		v2.Check(fmt.Sprintf("10.%d.0.1", i), pats)
	}
	queued = len(v2.queue)
	if queued != LookupsPerPass {
		t.Errorf("one pass queued %d", queued)
	}
	// Run answers until its context ends.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { v2.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(v2.queue) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(v2.queue) != 0 {
		t.Error("Run left lookups queued")
	}
}
