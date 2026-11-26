// Package sites reads the configured sites (the sites file, JSON) and answers the collector's questions about them:
// is a domain known, which site does a feed key belong to, which path template does a raw path fall under.
//
// Sites are declared, never self-registering: a browser can name any hostname, so only the operator's list decides
// what is stored. A feed key is configured as its SHA-256 (hex), so the file holds no secret.
package sites

import (
	"cmp"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/trick77/trakk/backend/internal/store"
)

// File is the sites file's shape.
type File struct {
	Sites []Config `json:"sites"`
}

// Config is one site as the file declares it.
type Config struct {
	// Domain is the hostname the pages run on and the script reports as site.
	Domain string `json:"domain"`
	// Name is what the dashboard and the messages call the site; left out, its domain. Nothing is matched or linked
	// by it.
	Name string `json:"name,omitempty"`
	// Product groups domains in the dashboard's menu ("Lens").
	Product string `json:"product"`
	// Order is the domain's place among its product's, from 1; 0 (or left out) is none: after the ordered ones, by
	// what they are called.
	Order int `json:"order,omitempty"`
	// Kind is site (pages) or api (with a feed, the dashboard shows calls instead of visits).
	Kind string `json:"kind"`
	// FeedKeySHA256 is the hex SHA-256 of the server feed's bearer key; empty means the site has no server feed.
	FeedKeySHA256 string `json:"feed_key_sha256"`
	// PathRules turn raw browser paths into templates, first match wins; unmatched paths fall to the heuristic.
	PathRules []Rule `json:"path_rules"`
}

// Rule is one path rule: Pattern matched against the whole path, decoded segment by segment (an encoded slash stays
// %2F inside its segment), Template what a match is listed as. Placeholders in a pattern: {ipv4}, {ipv6}, {cidr}
// (an address with /len or %2Flen), {int}, {seg} (one path segment), {text} (one segment that is no file name: not
// a dot file, no extension such as .php or .js, so free text without a scanner's probes) and {any} (anything,
// slashes included). Literal text matches case-insensitively.
type Rule struct {
	Pattern  string `json:"pattern"`
	Template string `json:"template"`
}

var (
	domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	hashRE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// A name goes into messages as it is: nothing a channel reads as markup, a mention or a new line.
	nameRE   = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{M}\p{N} .'()-]{0,63}$`)
	spacesRE = regexp.MustCompile(` +`)
)

// Load reads and checks the sites file. A missing file is no sites (the collector then refuses everything), not an
// error: a fresh deployment boots and says so in the log.
func Load(path string) ([]Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the operator's configured file
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse checks a sites file's content, every fault at once.
func Parse(b []byte) ([]Config, error) {
	var f File
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("sites file: %w", err)
	}
	var errs []error
	seen := map[string]bool{}
	keys := map[string]string{}  // feed key hash → domain
	taken := map[string]string{} // a site's domain and its name, in lower case → domain
	for i := range f.Sites {
		c := &f.Sites[i]
		c.Domain = strings.ToLower(strings.TrimSpace(c.Domain))
		c.Name = strings.Trim(spacesRE.ReplaceAllString(c.Name, " "), " ")
		c.FeedKeySHA256 = strings.ToLower(strings.TrimSpace(c.FeedKeySHA256))
		if c.Kind == "" {
			c.Kind = "site"
		}
		switch {
		case !domainRE.MatchString(c.Domain):
			errs = append(errs, fmt.Errorf("site %d: domain %q is not a hostname", i, c.Domain))
		case seen[c.Domain]:
			errs = append(errs, fmt.Errorf("site %s: listed twice", c.Domain))
		}
		seen[c.Domain] = true
		if c.Name != "" && !nameRE.MatchString(c.Name) {
			errs = append(errs, fmt.Errorf("site %s: name %q, want at most 64 letters, digits, spaces and .'()-, "+
				"a letter or digit first", c.Domain, c.Name))
		}
		// Not another's domain either, whatever that one is called: an alert would read as the wrong host's.
		for _, label := range []string{c.Domain, c.Name} {
			key := strings.ToLower(label)
			if d, ok := taken[key]; ok && d != c.Domain && label != "" {
				errs = append(errs, fmt.Errorf("site %s: %q is also %s's, every site needs its own name", c.Domain, label, d))
			}
			taken[key] = c.Domain
		}
		if c.Kind != "site" && c.Kind != "api" {
			errs = append(errs, fmt.Errorf("site %s: kind %q, want site or api", c.Domain, c.Kind))
		}
		if c.Order < 0 {
			errs = append(errs, fmt.Errorf("site %s: order %d, want 1 or more (0 or left out: none)", c.Domain, c.Order))
		}
		switch {
		case c.FeedKeySHA256 == "":
		case !hashRE.MatchString(c.FeedKeySHA256):
			errs = append(errs, fmt.Errorf("site %s: feed_key_sha256 is not 64 hex digits", c.Domain))
		case keys[c.FeedKeySHA256] != "":
			// The key alone says whose a batch is: with one for two sites both would be filed under one of them.
			errs = append(errs, fmt.Errorf("site %s: feed_key_sha256 is also %s's, every site needs its own key",
				c.Domain, keys[c.FeedKeySHA256]))
		default:
			keys[c.FeedKeySHA256] = c.Domain
		}
		if _, err := compileRules(c.PathRules); err != nil {
			errs = append(errs, fmt.Errorf("site %s: %w", c.Domain, err))
		}
	}
	return f.Sites, errors.Join(errs...)
}

// HashKey is a feed key's configured form: hex SHA-256.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Rows are the configs as site rows for store.SyncSites.
func Rows(cs []Config) []store.Site {
	out := make([]store.Site, 0, len(cs))
	for _, c := range cs {
		rules, _ := json.Marshal(nonNilRules(c.PathRules))
		out = append(out, store.Site{Domain: c.Domain, Name: c.Name, Kind: c.Kind, Product: c.Product,
			SortOrder: c.Order, FeedKeyHash: c.FeedKeySHA256, PathRules: string(rules), Active: true})
	}
	return out
}

func nonNilRules(r []Rule) []Rule {
	if r == nil {
		return []Rule{}
	}
	return r
}

// Site is a configured site ready for the collector.
type Site struct {
	ID     int64
	Domain string
	// Name is the configured one, "" for none (Label).
	Name    string
	Product string
	// Order is the place among the product's sites, 0 for none.
	Order int
	Kind  string
	// HasFeed is whether a feed key is configured.
	HasFeed bool
	keyHash []byte
	rules   []compiled
}

// Label is what the site is called: its name, else its domain.
func (s *Site) Label() string { return cmp.Or(s.Name, s.Domain) }

// Registry is the active sites, read-only after New.
type Registry struct {
	byDomain map[string]*Site
	list     []*Site
}

// New builds the registry from the synced rows (inactive ones left out).
func New(rows map[string]store.Site) (*Registry, error) {
	r := &Registry{byDomain: map[string]*Site{}}
	for _, row := range rows {
		if !row.Active {
			continue
		}
		var rules []Rule
		if err := json.Unmarshal([]byte(row.PathRules), &rules); err != nil {
			return nil, fmt.Errorf("site %s: path rules: %w", row.Domain, err)
		}
		cr, err := compileRules(rules)
		if err != nil {
			return nil, fmt.Errorf("site %s: %w", row.Domain, err)
		}
		s := &Site{ID: row.ID, Domain: row.Domain, Name: row.Name, Product: row.Product, Order: row.SortOrder,
			Kind: row.Kind, HasFeed: row.FeedKeyHash != "", rules: cr}
		if s.HasFeed {
			s.keyHash, _ = hex.DecodeString(row.FeedKeyHash)
		}
		r.byDomain[s.Domain] = s
		r.list = append(r.list, s)
	}
	slices.SortFunc(r.list, func(a, b *Site) int { return cmp.Compare(a.ID, b.ID) })
	return r, nil
}

// Lookup is the active site for a hostname.
func (r *Registry) Lookup(domain string) (*Site, bool) {
	s, ok := r.byDomain[strings.ToLower(domain)]
	return s, ok
}

// ByID is the active site with id.
func (r *Registry) ByID(id int64) (*Site, bool) {
	for _, s := range r.list {
		if s.ID == id {
			return s, true
		}
	}
	return nil, false
}

// All is every active site, in id order.
func (r *Registry) All() []*Site { return slices.Clone(r.list) }

// ByKey is the site a feed key belongs to. Every configured hash is compared, in constant time each, so the answer's
// timing does not depend on which site (if any) matched.
func (r *Registry) ByKey(key string) (*Site, bool) {
	if key == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(key))
	var found *Site
	for _, s := range r.list {
		if s.HasFeed && subtle.ConstantTimeCompare(sum[:], s.keyHash) == 1 {
			found = s
		}
	}
	return found, found != nil
}
