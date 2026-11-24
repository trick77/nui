package classify

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/trick77/stakeout/backend/internal/refdata"
	"github.com/trick77/stakeout/backend/internal/store"
)

// anyCrawlers and anyMonitors are made-up services, one of each kind the list has: the classifier knows no service by
// name, its entry is all it takes.
const anyCrawlers = `{"services":{
 "Ranged":{"ipv4":["192.0.2.0/27"],"ipv6":["2001:db8:1::/48"],"ip_list_authoritative":true,
   "user_agent_patterns":["*Ranged*","*Ranged-Images*"],"rdns_patterns":["*.ranged.example.com"]},
 "Shared (user-triggered)":{"ipv4":["192.0.2.0/28"],"ip_list_authoritative":true,
   "user_agent_patterns":["*SharedFetcher*"]},
 "Labelled":{"ipv4":["192.0.2.64/27"],"ip_list_authoritative":false,"user_agent_patterns":["*Labelled*"],
   "rdns_patterns":["*.labelled.example.com"]},
 "Nameless":{"ipv4":["192.0.2.96/27"],"ip_list_authoritative":false,"user_agent_patterns":["*Nameless*"],
   "rdns_patterns":["*.nameless.example.com"]},
 "Unpatterned":{"ipv4":["192.0.2.128/27"],"ip_list_authoritative":false,"user_agent_patterns":["*Unpatterned*"],
   "rdns_patterns":[]},
 "Refused":{"ipv4":["192.0.2.0-192.0.2.9"],"ip_list_authoritative":true,"user_agent_patterns":["*Refused*"],
   "rdns_patterns":["*.example.net"]},
 "Rangeless":{"ipv4":[],"ipv6":[],"ip_list_authoritative":true,"user_agent_patterns":["*Rangeless*"]}}}`

const anyMonitors = `{"services":{
 "Probe":{"ipv4":["192.0.2.16/28"],"ip_list_authoritative":true,"user_agent_patterns":["*Probe*"]},
 "Silent":{"ipv4":["192.0.2.192/27"],"ip_list_authoritative":true,"user_agent_patterns":[]}}}`

// anyResolver names 192.0.2.64/27 with two labels under the pattern (Yandex does), 192.0.2.96/27 not at all (Meta's
// 57.141.0.0/16), and everything else by another's name.
type anyResolver struct{}

func (anyResolver) LookupAddr(_ context.Context, a string) ([]string, error) {
	switch ip := netip.MustParseAddr(a); {
	case netip.MustParsePrefix("192.0.2.64/27").Contains(ip), a == "203.0.113.66":
		return []string{strings.ReplaceAll(a, ".", "-") + ".spider.labelled.example.com."}, nil
	case netip.MustParsePrefix("192.0.2.96/27").Contains(ip), a == "203.0.113.65":
		return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
	}
	return []string{"host.example.net."}, nil
}

func (anyResolver) LookupHost(_ context.Context, h string) ([]string, error) {
	if h == "host.example.net." {
		return []string{"203.0.113.64"}, nil
	}
	if a, ok := strings.CutSuffix(h, ".spider.labelled.example.com."); ok {
		return []string{strings.ReplaceAll(a, "-", ".")}, nil
	}
	return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
}

func botListRig(t *testing.T) *Classifier {
	t.Helper()
	ref := refdata.New(refdata.Options{Dir: t.TempDir()})
	b, err := refdata.ParseBots([]byte(anyCrawlers), []byte(anyMonitors))
	if err != nil {
		t.Fatal(err)
	}
	ref.SetBots(b)
	return &Classifier{Ref: ref, Verifier: refdata.NewVerifier(anyResolver{}, time.Second)}
}

// decide is the verdict on one client once its rDNS answer is in.
func decide(t *testing.T, c *Classifier, ip string, uas ...string) decision {
	t.Helper()
	a := actorDay{ip: ip}
	for i, ua := range uas {
		n := int64(len(uas) - i)
		a.uas = append(a.uas, uaDay{ua: ua, n: n, server: n})
	}
	c.Verifier.StartPass()
	c.Decide(a, siteFacts{feed: true})
	c.Verifier.Work(t.Context())
	d := c.Decide(a, siteFacts{feed: true})
	if d.pending {
		t.Errorf("%s %v still pending", ip, uas)
	}
	return d
}

// Every service of the list is recognized by its entry alone: at each of its ranges, with each of its user agents.
func TestBotList_EveryService(t *testing.T) {
	c := botListRig(t)
	for class, list := range map[string]string{"bot": anyCrawlers, "monitor": anyMonitors} {
		var f struct {
			Services map[string]struct {
				IPv4, IPv6 []string
				UA         []string `json:"user_agent_patterns"`
				Auth       bool     `json:"ip_list_authoritative"`
			}
		}
		if err := json.Unmarshal([]byte(list), &f); err != nil {
			t.Fatal(err)
		}
		for name, s := range f.Services {
			if name == "Refused" {
				continue // its ranges cannot be used: TestBotList_Verdicts
			}
			ranges := append(s.IPv4, s.IPv6...)
			uas := []string{}
			for _, g := range s.UA {
				uas = append(uas, "Mozilla/5.0 (compatible; "+strings.Trim(g, "*")+"/1.0; +https://example.com/about)")
			}
			for _, r := range ranges {
				ip := netip.MustParsePrefix(r).Addr()
				for _, ua := range uas {
					d := decide(t, c, ip.String(), ua)
					if d.class != class || !strings.HasPrefix(d.reasons[0], name) {
						t.Errorf("%s at %s = %s %v", name, ip, d.class, d.reasons)
					}
					if got := c.instant(ip, ua); s.Auth && got != store.ClassCode(class) {
						t.Errorf("%s at %s at once = %d", name, ip, got)
					}
				}
				// An authoritative list needs no user agent.
				if d := decide(t, c, ip.String(), browserUA); s.Auth && d.class != class {
					t.Errorf("%s's address = %s %v", name, d.class, d.reasons)
				}
			}
			for _, ua := range uas {
				d := decide(t, c, "203.0.113.9", ua)
				if len(ranges) == 0 {
					if d.class != class || d.reasons[0] != "client is "+name {
						t.Errorf("%s by its user agent = %s %v", name, d.class, d.reasons)
					}
				} else if d.class != "unknown" || !strings.HasPrefix(d.reasons[0], "claims "+name+", ") {
					t.Errorf("a claim to be %s = %s %v", name, d.class, d.reasons)
				}
			}
		}
	}
}

func TestBotList_Verdicts(t *testing.T) {
	c := botListRig(t)
	for _, tt := range []struct {
		ip   string
		uas  []string
		want string
	}{
		// Two services' shared range: the most frequent user agent says whose, crawler or probe; else the narrowest.
		{"192.0.2.1", []string{"Ranged/1", "SharedFetcher/1"}, "bot|Ranged address range"},
		{"192.0.2.2", []string{"SharedFetcher/1", "Ranged/1"}, "bot|Shared (user-triggered) address range"},
		{"192.0.2.17", []string{"Ranged/1"}, "bot|Ranged address range"},
		{"192.0.2.17", []string{"Probe/1"}, "monitor|Probe address range"},
		{"192.0.2.17", []string{"curl/8"}, "monitor|Probe address range"},
		// A name deeper under the pattern, or none, only with the service's own user agent: the company's people share
		// its network and its domain.
		{"192.0.2.65", []string{"Labelled/1"}, "bot|Labelled, rDNS confirmed"},
		{"192.0.2.65", []string{"curl/8"}, "unknown|client curl/8"},
		{"192.0.2.97", []string{"Nameless/1"}, "bot|Nameless address range, no rDNS name"},
		{"192.0.2.97", []string{"curl/8"}, "unknown|client curl/8"},
		{"192.0.2.97", []string{"Labelled/1"}, "unknown|claims Labelled, no rDNS name"},
		// Nothing to check it by.
		{"192.0.2.129", []string{"Unpatterned/1"}, "bot|Unpatterned address range, rDNS not verified"},
		// A name that is another's refutes inside the range too.
		{"192.0.2.66", []string{"Nameless/1"}, "unknown|claims Nameless, address is not Nameless's"},
		{"192.0.2.129", []string{"Nameless/1"}, "unknown|claims Nameless, address is not Nameless's"},
		{"192.0.2.193", []string{"anything/1"}, "monitor|Silent address range"},
		// Refused ranges compare with nothing: rDNS alone tells, and without an answer nobody is called an impostor.
		{"203.0.113.64", []string{"Refused/1"}, "bot|Refused, rDNS confirmed"},
		{"203.0.113.65", []string{"Refused/1"}, "unknown|claims Refused, no rDNS name"},
		// Outside the range a name deeper under the pattern is anybody's on the company's domain.
		{"203.0.113.66", []string{"Labelled/1"}, "unknown|claims Labelled, address is not Labelled's"},
	} {
		d := decide(t, c, tt.ip, tt.uas...)
		if got := d.class + "|" + strings.Join(d.reasons, ","); got != tt.want {
			t.Errorf("%s %v = %q, want %q", tt.ip, tt.uas, got, tt.want)
		}
	}
	// A person on the company's network, its host named under its domain, stays one.
	a := actorDay{ip: "192.0.2.65", uas: []uaDay{{ua: browserUA, n: 9, browser: 5, server: 4, docs: 1, engaged: 2}}}
	if d := c.Decide(a, siteFacts{feed: true, script: true}); d.class != "human" {
		t.Errorf("a person behind a deeper name = %s %v", d.class, d.reasons)
	}
	// A probe that signs in from its own range is the probe, not an impostor.
	a = actorDay{ip: "192.0.2.17", uas: []uaDay{{ua: browserUA + " Probe", n: 2, server: 2, docs: 1, authed: 2}}}
	if d := c.Decide(a, siteFacts{feed: true}); d.class != "monitor" || d.reasons[0] != "Probe address range" {
		t.Errorf("a signed-in probe = %s %v", d.class, d.reasons)
	}
}
