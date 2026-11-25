package sites

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/trick77/trakk/backend/internal/store"
)

func exampleRegistry(t *testing.T, key string) *Registry {
	t.Helper()
	cs, err := Load(filepath.Join("..", "..", "..", "docs", "sites.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		cs[0].FeedKeySHA256 = HashKey(key)
	}
	rows := map[string]store.Site{}
	for i, r := range Rows(cs) {
		r.ID = int64(i + 1)
		rows[r.Domain] = r
	}
	rows["gone.example.com"] = store.Site{ID: 9, Domain: "gone.example.com", PathRules: "[]"}
	reg, err := New(rows)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestExample_LensTemplates(t *testing.T) {
	reg := exampleRegistry(t, "")
	s, ok := reg.Lookup("LENS.example.com")
	if !ok || s.Product != "Lens" || s.HasFeed {
		t.Fatalf("lookup = %+v %v", s, ok)
	}
	for path, want := range map[string]string{
		"/":                  "/",
		"":                   "/",
		"/8.8.8.8":           "/:ipv4",
		"/2001:db8::1":       "/:ipv6",
		"/2001%3Adb8%3A%3A1": "/:ipv6",
		"/1.2.3.0/24":        "/:cidr",
		"/1.2.3.0%2F24":      "/:cidr",
		"/2001:db8::/32":     "/:cidr",
		"/AS15169":           "/AS:n",
		"/as15169":           "/AS:n",
		"/google":            "/:text",
		"/999.1.1.1":         "/:text", // shaped like an address, is none
		"/Google%20LLC":      "/:text",
		"/example.com":       "/:text", // a dot, but no file extension
		"/%zz":               "/:text", // bad escape: matched raw
		// A free-text search holding a slash (%2F) is one segment: text, not a deeper path.
		"/foo%2Fbar": "/:text",
		// What lens serves statically has its own rules.
		"/assets/index-a41c.js": "/assets/*",
		"/favicon.ico":          "/favicon.ico",
		"/robots.txt":           "/robots.txt",
		// Scanners and unknown paths fall through to the heuristic: literal segments stay readable.
		"/.env":                  "/.env",
		"/wp-login.php":          "/wp-login.php",
		"/wp-admin/setup.php":    "/wp-admin/setup.php",
		"/api/v1/nope":           "/api/v1/nope",
		"/1.2.3.4/99":            "/:ip/:n", // no such prefix length
		"/some/deep/path":        "/some/deep/path",
		"/AS15169/extra":         "/AS15169/extra",
		"/2001:db8::1/64/x/y/zz": "/:ip/:n/x/y/zz",
		// Bytes that are no UTF-8 become U+FFFD: the key survives the dashboard's round trip.
		"/%FF%FE/x": "/�/x",
	} {
		if got := s.Template(path); got != want {
			t.Errorf("Template(%q) = %q, want %q", path, got, want)
		}
	}
	if _, ok := reg.Lookup("gone.example.com"); ok {
		t.Error("an inactive site is looked up")
	}
	if all := reg.All(); len(all) != 1 {
		t.Errorf("All = %d", len(all))
	}
	if _, ok := reg.ByID(s.ID); !ok {
		t.Error("ByID")
	}
	if _, ok := reg.ByID(42); ok {
		t.Error("ByID 42")
	}
}

func TestByKey(t *testing.T) {
	reg := exampleRegistry(t, "s3cret-key")
	if s, ok := reg.ByKey("s3cret-key"); !ok || s.Domain != "lens.example.com" || !s.HasFeed {
		t.Fatalf("ByKey = %+v %v", s, ok)
	}
	for _, k := range []string{"", "wrong", "s3cret-key "} {
		if _, ok := reg.ByKey(k); ok {
			t.Errorf("ByKey(%q) matched", k)
		}
	}
}

func TestHeuristic(t *testing.T) {
	for in, want := range map[string]string{
		"/":                      "/",
		"/api/v1/ip/203.0.113.9": "/api/v1/ip/:ip",
		"/users/42/posts":        "/users/:n/posts",
		"/o/3f2a91c0-1234-4abc-8def-0123456789ab":    "/o/:uuid",
		"/blob/deadbeefdeadbeefdeadbeef":             "/blob/:hash",
		"/t/AbCdEfGhIjKlMnOpQrStUvWx12":              "/t/:id",
		"/x/" + strings.Repeat("z", 70):              "/x/:long",
		"/a/b/c/d/e/f/g/h/i/j":                       "/a/b/c/d/e/f/g/h/:rest",
		"/wp-login.php":                              "/wp-login.php",
		"/.env":                                      "/.env",
		"/wp-admin/setup.php":                        "/wp-admin/setup.php",
		"/api/v1/nope":                               "/api/v1/nope",
		"/assets/index-abc.js":                       "/assets/index-abc.js",
		"/assets/index-a41c.js":                      "/assets/index-:hash.js",
		"/assets/vendor-B3xZ9kQa.css":                "/assets/vendor-:hash.css",
		"/assets/jquery-3.7.1.min.js":                "/assets/jquery-3.7.1.min.js",
		"/" + strings.Repeat("segment/", 7) + "last": "/segment/segment/segment/segment/segment/segment/segment/last",
	} {
		if got := Heuristic(in); got != want {
			t.Errorf("Heuristic(%q) = %q, want %q", in, got, want)
		}
	}
	long := "/" + strings.Repeat("abcdefghij/", 7) + strings.Repeat("y", 60)
	if got := Heuristic(long); len(got) > maxTemplate {
		t.Errorf("Heuristic too long: %d", len(got))
	}
	// The cut falls on a character boundary.
	wide := "/" + strings.Repeat("é", 50) + "/" + strings.Repeat("ü", 60)
	if got := Heuristic(wide); len(got) > maxTemplate || !utf8.ValidString(got) {
		t.Errorf("Heuristic(%d bytes) = %d bytes, valid %v", len(wide), len(got), utf8.ValidString(got))
	}
}

func TestParse_Faults(t *testing.T) {
	for name, in := range map[string]string{
		"not json":      `{`,
		"unknown field": `{"sites":[{"domain":"a.example.com","x":1}]}`,
		"bad domain":    `{"sites":[{"domain":"not a host"}]}`,
		"twice":         `{"sites":[{"domain":"a.example.com"},{"domain":"A.example.com"}]}`,
		"kind":          `{"sites":[{"domain":"a.example.com","kind":"blog"}]}`,
		"hash":          `{"sites":[{"domain":"a.example.com","feed_key_sha256":"abc"}]}`,
		"placeholder":   `{"sites":[{"domain":"a.example.com","path_rules":[{"pattern":"/{nope}","template":"/x"}]}]}`,
		"no slash":      `{"sites":[{"domain":"a.example.com","path_rules":[{"pattern":"x","template":"/x"}]}]}`,
		"no template":   `{"sites":[{"domain":"a.example.com","path_rules":[{"pattern":"/x","template":""}]}]}`,
		"below zero":    `{"sites":[{"domain":"a.example.com","order":-1}]}`,
		"no integer":    `{"sites":[{"domain":"a.example.com","order":1.5}]}`,
		"name twice":    `{"sites":[{"domain":"a.example.com","name":"Blog"},{"domain":"b.example.com","name":" blog "}]}`,
		"name a domain": `{"sites":[{"domain":"a.example.com"},{"domain":"b.example.com","name":"A.example.com"}]}`,
		"domain a name": `{"sites":[{"domain":"b.example.com","name":"A.example.com"},{"domain":"a.example.com"}]}`,
		"name spaced":   `{"sites":[{"domain":"a.example.com","name":"My Blog"},{"domain":"b.example.com","name":"My  Blog"}]}`,
		"name of named": `{"sites":[{"domain":"a.example.com","name":"b.example.com"},{"domain":"b.example.com","name":"B"}]}`,
		"name a line":   `{"sites":[{"domain":"a.example.com","name":"a\nb"}]}`,
		"name a markup": `{"sites":[{"domain":"a.example.com","name":"*a*"}]}`,
		"name reversed": `{"sites":[{"domain":"a.example.com","name":"a` + string(rune(0x202e)) + `b"}]}`,
		"name long":     `{"sites":[{"domain":"a.example.com","name":"` + strings.Repeat("ü", 65) + `"}]}`,
	} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// One key for two sites is refused, naming both.
	h := HashKey("shared")
	_, err := Parse([]byte(`{"sites":[{"domain":"a.example.com","feed_key_sha256":"` + h +
		`"},{"domain":"b.example.com","feed_key_sha256":"` + strings.ToUpper(h) + `"}]}`))
	if err == nil || !strings.Contains(err.Error(), "b.example.com") || !strings.Contains(err.Error(), "a.example.com") {
		t.Errorf("shared feed key: %v", err)
	}
	cs, err := Parse([]byte(`{"sites":[{"domain":" A.Example.com ","feed_key_sha256":"` +
		strings.Repeat("AB", 32) + `"}]}`))
	if err != nil || cs[0].Domain != "a.example.com" || cs[0].Kind != "site" || cs[0].FeedKeySHA256 != strings.Repeat("ab", 32) {
		t.Fatalf("normalised = %+v, %v", cs, err)
	}
	// Every fault is named at once, the order's among them.
	_, err = Parse([]byte(`{"sites":[{"domain":"a.example.com","order":-1},{"domain":"b.example.com","kind":"blog"}]}`))
	if err == nil || !strings.Contains(err.Error(), "order -1") || !strings.Contains(err.Error(), `kind "blog"`) {
		t.Errorf("two faults: %v", err)
	}
}

func TestOrder(t *testing.T) {
	cs, err := Parse([]byte(`{"sites":[{"domain":"a.example.com","order":2},{"domain":"b.example.com","order":0},
		{"domain":"c.example.com"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]store.Site{}
	for i, r := range Rows(cs) {
		r.ID = int64(i + 1)
		rows[r.Domain] = r
	}
	reg, err := New(rows)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{2, 0, 0} {
		if s := reg.All()[i]; s.Order != want {
			t.Errorf("%s: order %d, want %d", s.Domain, s.Order, want)
		}
	}
}

// A name is what the site is called and nothing else: it is looked up by its domain alone, and a name may be its own
// domain or a host that is not listed.
func TestName(t *testing.T) {
	cs, err := Parse([]byte(`{"sites":[{"domain":"www.example.com","name":" example.com "},{"domain":"b.example.com"},
		{"domain":"c.example.com","name":"c.example.com"},{"domain":"d.example.com","name":"Zoë's (d) 2-1"},
		{"domain":"e.example.com","name":"x.example.com"},{"domain":"f.example.com","name":"F"},
		{"domain":"g.example.com","name":"` + strings.Repeat("ü", 64) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]store.Site{}
	for i, r := range Rows(cs) {
		r.ID = int64(i + 1)
		rows[r.Domain] = r
	}
	reg, err := New(rows)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"example.com", "b.example.com", "c.example.com", "Zoë's (d) 2-1", "x.example.com", "F"} {
		if got := reg.All()[i].Label(); got != want {
			t.Errorf("site %d is called %q, want %q", i, got, want)
		}
	}
	if _, ok := reg.Lookup("example.com"); ok {
		t.Error("a site is looked up by its name")
	}
	if s, ok := reg.Lookup("f.example.com"); !ok || s.Name != "F" {
		t.Errorf("lookup by domain = %+v %v", s, ok)
	}
}

func TestLoad_MissingAndBroken(t *testing.T) {
	dir := t.TempDir()
	if cs, err := Load(filepath.Join(dir, "none.json")); err != nil || cs != nil {
		t.Errorf("missing file: %v %v", cs, err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("a directory loads")
	}
	p := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(p, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("a broken file loads")
	}
	if _, err := New(map[string]store.Site{"x": {Domain: "x", Active: true, PathRules: "nope"}}); err == nil {
		t.Error("broken stored rules accepted")
	}
	if _, err := New(map[string]store.Site{"x": {Domain: "x", Active: true,
		PathRules: `[{"pattern":"/{nope}","template":"/"}]`}}); err == nil {
		t.Error("bad stored rule accepted")
	}
}
