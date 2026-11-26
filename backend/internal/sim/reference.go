package sim

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/trick77/trakk/backend/internal/refdata"
)

// The world's reference data. Documentation address ranges are announced by nobody, so the real AS and country
// data know nothing of them: the simulator brings its own, in ipverse's file formats, written into a reference
// directory and loaded through refdata's own loader. Every network, country and bot below is made up; their
// addresses are documentation ranges only (RFC 5737, RFC 2544's benchmark /15, RFC 3849).

// network is one made-up autonomous system and where its people come from.
type network struct {
	ASN      int64
	Name     string
	Country  string
	Category string
	V4       []string
	// people is the network's weight among the people's home networks (0: none live there).
	people float64
}

// networks are the world's ASes. Their IPv6 block is 2001:db8:<index>00::/40 (index from 1).
var networks = []network{
	{64496, "Alpine Fibre AG", "CH", "isp", []string{"198.18.0.0/19"}, 30},
	{64497, "Rheinwelle Telekom", "DE", "isp", []string{"198.18.32.0/19"}, 14},
	{64498, "Prairie Cable Co", "US", "isp", []string{"198.18.64.0/18"}, 14},
	{64499, "Hexagone Telecom", "FR", "isp", []string{"198.18.128.0/19"}, 8},
	{64500, "Thames Broadband", "GB", "isp", []string{"198.18.160.0/19"}, 8},
	{64501, "Polder Net BV", "NL", "isp", []string{"198.18.192.0/19"}, 6},
	{64502, "Sakura Mobile KK", "JP", "isp", []string{"198.18.224.0/19"}, 4},
	{64503, "Lakeside University", "CH", "education_research", []string{"198.19.0.0/20"}, 4},
	{64504, "Matterhorn Mobile", "CH", "isp", []string{"198.19.16.0/20"}, 9},
	{64505, "Nimbus Cloud Inc", "US", "hosting", []string{"203.0.113.0/24", "198.19.128.0/18"}, 0},
	{64506, "Kestrel Hosting BV", "NL", "hosting", []string{"192.0.2.0/24", "198.19.192.0/19"}, 0},
	{64507, "Example Search LLC", "US", "business", []string{"198.51.100.0/25"}, 0},
	{64508, "Uptimely Ltd", "GB", "hosting", []string{"198.51.100.128/25"}, 0},
	{64509, "Harbour Office Networks", "DE", "business", []string{"198.19.32.0/20"}, 3},
	{64510, "Example AI Inc", "US", "hosting", []string{"198.19.64.0/20"}, 0},
}

var countryNames = map[string]string{"CH": "Switzerland", "DE": "Germany", "US": "United States", "FR": "France",
	"GB": "United Kingdom", "NL": "Netherlands", "JP": "Japan"}

// v6Block is a network's IPv6 /40.
func v6Block(i int) netip.Prefix {
	var b [16]byte
	copy(b[:], netip.MustParseAddr("2001:db8::").AsSlice())
	b[4] = byte(i + 1) //nolint:gosec // a dozen networks
	return netip.PrefixFrom(netip.AddrFrom16(b), 40)
}

// botService is one entry of the made-up bot list, in bot-ip-blocks' shape.
type botService struct {
	IPv4          []string `json:"ipv4"`
	IPv6          []string `json:"ipv6"`
	Authoritative bool     `json:"ip_list_authoritative"`
	Rdns          []string `json:"rdns_patterns"`
	UA            []string `json:"user_agent_patterns"`
	// Agents is missing from an older list's entry.
	Agents []botAgent `json:"agents,omitempty"`
}

// botAgent is one user-agent token a service declares, with what the vendor says it does.
type botAgent struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Purpose string `json:"purpose"`
}

// The made-up bots: a crawler with an authoritative list, one rendering pages whose list needs rDNS, an uptime
// probe whose entry is an older list's (no agents), and an AI vendor publishing one address list for two purposes:
// its crawler's and the fetches its assistant makes for a user. Their user agents follow the real ones' pattern.
var (
	worldCrawlers = map[string]botService{
		"Examplebot": {IPv4: []string{"198.51.100.0/26"}, IPv6: []string{"2001:db8:c00::/48"}, Authoritative: true,
			Rdns: []string{"*.crawl.example.com"}, UA: []string{"*Examplebot*"},
			Agents: []botAgent{{"Examplebot", "*Examplebot*", "search"}}},
		"Samplebot": {IPv4: []string{"198.51.100.64/26"}, Rdns: []string{"*.render.example.net"},
			UA: []string{"*Samplebot*"}, Agents: []botAgent{{"Samplebot", "*Samplebot*", "seo"}}},
		AIService: {IPv4: []string{"198.19.64.0/24"}, Authoritative: true, Rdns: []string{"*.ai.example.com"},
			UA: []string{"*ExampleAI*"}, Agents: []botAgent{{AICrawler, "*ExampleAI-Crawler*", "ai-training"},
				{AIFetcher, "*ExampleAI-User*", "ai-assistant"}}},
	}
	worldMonitors = map[string]botService{
		"Uptimely": {IPv4: []string{"198.51.100.128/26"}, Authoritative: true, UA: []string{"*Uptimely*"}},
	}
)

// The AI vendor's service and agents, and the key directory its fetcher names in Signature-Agent (an RFC 8941 string).
const (
	AIService  = "ExampleAI"
	AICrawler  = "ExampleAI-Crawler"
	AIFetcher  = "ExampleAI-User"
	AISigAgent = `"https://ai.example.com"`
)

// botRole is a bot actor's identity: its user agent, the ranges it sends from, and whether the service is the world's
// own (the expectations that name a purpose hold for its list only).
type botRole struct {
	UA  string
	V4  []netip.Prefix
	own bool
}

// roleChoices are the candidates for each bot role, the world's own last: with a real bot list loaded the actors
// take a real service's ranges from it (never hard-coded here) and its user agent.
var roleChoices = map[string][]struct{ name, ua string }{
	"crawler": {
		{"Googlebot", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"},
		{"Examplebot", "Mozilla/5.0 (compatible; Examplebot/2.1; +https://crawl.example.com/bot)"},
	},
	"renderer": {
		{"Bingbot", "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm) Chrome/116.0.1938.76 Safari/537.36"},
		{"Samplebot", "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Samplebot/1.4; +https://render.example.net/) Chrome/142.0.0.0 Safari/537.36"},
	},
	"monitor": {
		{"UptimeRobot", "Mozilla/5.0+(compatible; UptimeRobot/2.0; http://www.uptimerobot.com/)"},
		{"Uptimely", "Mozilla/5.0 (compatible; Uptimely/3.2; +https://uptimely.example.com/)"},
	},
	// One service's two agents: its crawler and the fetcher a user's question sends.
	"ai-crawler": {
		{"ClaudeBot", "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)"},
		{AIService, "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ExampleAI-Crawler/1.0; +https://ai.example.com/bots)"},
	},
	"ai-fetcher": {
		{"ClaudeBot", "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-User/1.0; +Claude-User@anthropic.com)"},
		{AIService, "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ExampleAI-User/1.0; +https://ai.example.com/bots)"},
	},
}

// hostPlan places a role's hosts in its range: the first, then one every step per site.
type hostPlan struct{ first, step int }

// roleHost is the plan of a role sharing its service's range with another, every other role's host 10 plus the site:
// the two interleave, another visitor on every site however many sites there are (blocks side by side met from the
// ninth site on).
var roleHost = map[string]hostPlan{"ai-crawler": {10, 2}, "ai-fetcher": {11, 2}}

// The reference files, as refdata caches them.
const (
	crawlersFile  = "crawlers.json"
	monitorFile   = "monitoring.json"
	networksFile  = "as-ip-blocks.tar.gz"
	countriesFile = "geo-ip-blocks.tar.gz"
)

// Reference is the reference data a world runs against: loaded from a directory through refdata, and the bot roles
// resolved against its bot list.
type Reference struct {
	Dir   string
	Data  *refdata.Data
	roles map[string]botRole
}

// OpenReference loads the reference data in dir, writing the world's own first when dir holds no bot list or an
// earlier build's world (staleWorld).
func OpenReference(dir string) (*Reference, error) {
	_, err := os.Stat(filepath.Join(dir, crawlersFile))
	stale, serr := staleWorld(dir)
	if serr != nil {
		return nil, serr
	}
	if errors.Is(err, fs.ErrNotExist) || stale {
		if err := WriteReference(dir); err != nil {
			return nil, err
		}
	}
	d := refdata.New(refdata.Options{Dir: dir, Log: quietLog()})
	d.LoadCached()
	ref := &Reference{Dir: dir, Data: d, roles: map[string]botRole{}}
	// Read again: the loader keeps quiet about a list it cannot use.
	crawlers, err := os.ReadFile(filepath.Join(dir, crawlersFile)) //nolint:gosec // the directory the caller names
	if err != nil {
		return nil, err
	}
	monitors, err := os.ReadFile(filepath.Join(dir, monitorFile)) //nolint:gosec // the directory the caller names
	if err != nil {
		return nil, err
	}
	bots, err := refdata.ParseBots(crawlers, monitors)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	for role, choices := range roleChoices {
		for _, c := range choices {
			// The list must take the user agent for this service, or the actor would be an impostor.
			p, ok := bots.ByUA(c.ua)
			if !ok || p.Name != c.name {
				continue
			}
			_, crawler := worldCrawlers[c.name]
			_, monitor := worldMonitors[c.name]
			r := botRole{UA: c.ua, own: crawler || monitor}
			for _, x := range p.Ranges {
				if x.Addr().Is4() {
					r.V4 = append(r.V4, x)
				}
			}
			if len(r.V4) > 0 {
				ref.roles[role] = r
				break
			}
		}
	}
	return ref, nil
}

// Stamp dates the reference files at t and loads them again: the dashboard states when its reference data was
// updated (the files' time), and an export must not carry the wall clock.
func (r *Reference) Stamp(t time.Time) error {
	for _, f := range []string{crawlersFile, monitorFile, networksFile, countriesFile} {
		if err := os.Chtimes(filepath.Join(r.Dir, f), t, t); err != nil {
			return err
		}
	}
	r.Data.LoadCached()
	return nil
}

// role is a bot role, ok false when the loaded list has no service for it (the actor then stays away).
func (r *Reference) role(name string) (botRole, bool) {
	b, ok := r.roles[name]
	return b, ok
}

// WriteReference writes the world's reference data into dir, in the files refdata caches, and marks it
// (RefMarkerFile): a server in a simulator mode on that directory enriches the simulated addresses as backfill did,
// a normal one discards it.
func WriteReference(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := mark(filepath.Join(dir, RefMarkerFile), "the simulator's made-up reference data: a normal server "+
		"discards it and fetches the real data"); err != nil {
		return err
	}
	for name, list := range worldLists {
		b, err := botList(list)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return err
		}
	}
	as := map[string]any{}
	geo := map[string]map[string]any{}
	for i, n := range networks {
		v6 := []string{v6Block(i).String()}
		as[fmt.Sprintf("as/%d/aggregated.json", n.ASN)] = map[string]any{"asn": n.ASN,
			"metadata": map[string]string{"handle": strings.ToUpper(strings.Fields(n.Name)[0]), "description": n.Name,
				"countryCode": n.Country, "category": n.Category},
			"prefixes": map[string][]string{"ipv4": n.V4, "ipv6": v6}}
		cc := strings.ToLower(n.Country)
		key := "country/" + cc + "/" + cc + ".json"
		g := geo[key]
		if g == nil {
			g = map[string]any{"countryCode": n.Country, "country": countryNames[n.Country],
				"prefixes": map[string][]string{"ipv4": nil, "ipv6": nil}}
			geo[key] = g
		}
		p := g["prefixes"].(map[string][]string)
		p["ipv4"] = append(p["ipv4"], n.V4...)
		p["ipv6"] = append(p["ipv6"], v6...)
	}
	geoAny := map[string]any{}
	for k, v := range geo {
		geoAny[k] = v
	}
	for name, files := range map[string]map[string]any{networksFile: as, countriesFile: geoAny} {
		b, err := tarball(files)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// worldLists are the world's bot lists by file.
var worldLists = map[string]map[string]botService{crawlersFile: worldCrawlers, monitorFile: worldMonitors}

// botList is a bot list file's content, byte for byte the same for the same list (a map marshals sorted).
func botList(list map[string]botService) ([]byte, error) {
	return json.MarshalIndent(map[string]any{"services": list}, "", " ")
}

// staleWorld reports whether dir holds an earlier build's world reference: marked, its bot lists other than the
// world's, and naming none but the world's services. A directory without the marker, or whose list names another
// service (real data fetched into it since), is never rewritten. An earlier build's list lacked the AI vendor: its
// roles would stay away, unseen.
func staleWorld(dir string) (bool, error) {
	if !SimulatedReference(dir) {
		return false, nil
	}
	stale := false
	for name, list := range worldLists {
		got, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // the directory the caller names
		if errors.Is(err, fs.ErrNotExist) {
			stale = true
			continue
		}
		if err != nil {
			return false, err
		}
		want, err := botList(list)
		if err != nil {
			return false, err
		}
		if bytes.Equal(got, want) {
			continue
		}
		if !worldOnly(got) {
			return false, nil
		}
		stale = true
	}
	return stale, nil
}

// worldOnly reports whether a bot list file names none but the world's services.
func worldOnly(list []byte) bool {
	var f struct {
		Services map[string]json.RawMessage `json:"services"`
	}
	if json.Unmarshal(list, &f) != nil {
		return false
	}
	for s := range f.Services {
		_, crawler := worldCrawlers[s]
		_, monitor := worldMonitors[s]
		if !crawler && !monitor {
			return false
		}
	}
	return true
}

// tarball packs JSON documents into a gzipped tar, ipverse's release format.
func tarball(files map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range sortedKeys(files) {
		b, err := json.Marshal(files[name])
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(b); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
