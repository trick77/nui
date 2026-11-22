package simcli

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/nui/backend/internal/sim"
	"github.com/trick77/nui/backend/internal/store"
)

// The parameter sets the UI asks for (ui/src/api.ts and its callers): every range; the audiences and comparisons
// on the overview, the audiences on the default range; the lists at the limits the panels use.
var (
	ranges    = []string{"24h", "7d", "30d", "90d", "12mo"}
	audiences = []string{"all", "humans", "bots"}
	compares  = []string{"prev", "yoy", "off"}
	listDims  = []string{"pages", "entry", "events", "outbound", "referrers", "networks", "countries", "probed",
		"refused", "errors"}
	visitorSorts = []string{"requests", "refused", "events", "views"}
)

// ExportIndex is index.json: which file answers which request.
type ExportIndex struct {
	// World is what the answers were computed over.
	World map[string]any `json:"world"`
	// Responses maps a request (Key) to its answer.
	Responses map[string]ExportEntry `json:"responses"`
}

// ExportEntry is one answer: its file under the export directory and its status.
type ExportEntry struct {
	File   string `json:"file"`
	Status int    `json:"status"`
}

// Key is a request's index key: the endpoint and its parameters sorted, the viewer's zone left out (the answers are
// computed in UTC).
func Key(endpoint string, params map[string]string) string {
	v := url.Values{}
	for k, x := range params {
		if k != "tz" && x != "" {
			v.Set(k, x)
		}
	}
	if q := v.Encode(); q != "" {
		return endpoint + "?" + q
	}
	return endpoint
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9.=-]+`)

// fileName is where an answer is written: endpoint/site/the other parameters.
func fileName(endpoint string, params map[string]string) string {
	site := params["site"]
	if site == "" {
		site = "_"
	}
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(params)) {
		if k != "site" && k != "tz" && params[k] != "" {
			parts = append(parts, k+"="+params[k])
		}
	}
	name := unsafeName.ReplaceAllString(strings.Join(parts, "_"), "-")
	if name == "" {
		name = "index"
	}
	return filepath.Join(endpoint, site, name+".json")
}

// Export writes the dashboard's answers over st for every request the UI's fixture mode needs into dir, with
// index.json naming them: the ranges given (nil: all). The answers are the real computations' over the world's
// history, as of a minute after its anchor.
func Export(ctx context.Context, st *store.Store, w *sim.World, dir string, about map[string]any, only []string) (int, error) {
	rngs := ranges
	if only != nil {
		rngs = only
	}
	d, err := NewDashboard(ctx, st, w)
	if err != nil {
		return 0, err
	}
	idx := ExportIndex{World: about, Responses: map[string]ExportEntry{}}
	get := func(endpoint string, params map[string]string) (map[string]any, error) {
		k := Key(endpoint, params)
		code, b := d.Get(ctx, endpoint, params)
		var body map[string]any
		if err := json.Unmarshal(b, &body); err != nil {
			return nil, fmt.Errorf("%s: not JSON: %s", k, b)
		}
		if _, seen := idx.Responses[k]; seen {
			return body, nil
		}
		name := fileName(endpoint, params)
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, err
		}
		pretty, err := json.MarshalIndent(body, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, append(pretty, '\n'), 0o600); err != nil {
			return nil, err
		}
		idx.Responses[k] = ExportEntry{File: filepath.ToSlash(name), Status: code}
		return body, nil
	}
	if _, err := get("sites", nil); err != nil {
		return 0, err
	}
	for _, s := range w.Sites {
		for _, rng := range rngs {
			p := func(aud, cmp string, kv ...string) map[string]string {
				m := map[string]string{"site": s.Domain, "range": rng, "aud": aud, "cmp": cmp, "tz": "UTC"}
				for i := 0; i+1 < len(kv); i += 2 {
					m[kv[i]] = kv[i+1]
				}
				return m
			}
			auds := []string{"all"}
			if rng == "7d" {
				auds = audiences
			}
			for _, aud := range audiences {
				for _, cmp := range compares {
					if _, err := get("overview", p(aud, cmp)); err != nil {
						return 0, err
					}
				}
			}
			for _, aud := range auds {
				var reqs [][]string
				for _, ep := range []string{"noteworthy", "classes", "status", "clients", "domain", "log"} {
					reqs = append(reqs, []string{ep})
				}
				for _, dim := range listDims {
					for _, sort := range []string{"volume", "change"} {
						reqs = append(reqs, []string{"list", "dim", dim, "sort", sort, "limit", "6"},
							[]string{"list", "dim", dim, "sort", sort, "limit", "500"})
					}
				}
				reqs = append(reqs, []string{"list", "dim", "countries", "sort", "volume", "limit", "300"},
					[]string{"list", "dim", "countries", "sort", "volume", "limit", "5"},
					[]string{"list", "dim", "events", "sort", "volume", "limit", "6", "sub", "custom"})
				for _, sort := range visitorSorts {
					for _, limit := range []string{"7", "50"} {
						reqs = append(reqs, []string{"visitors", "sort", sort, "limit", limit})
					}
				}
				for _, r := range reqs {
					body, err := get(r[0], p(aud, "prev", r[1:]...))
					if err != nil {
						return 0, err
					}
					// The expansions of the top pages, and the details of the visitors listed first.
					switch {
					case aud == "all" && r[0] == "list" && r[2] == "pages" && r[6] == "6" && r[4] == "volume":
						for _, row := range rows(body) {
							if row["expandable"] == true {
								if _, err := get("list", p(aud, "prev", "dim", "pages", "sort", "volume", "limit", "6",
									"within", fmt.Sprint(row["key"]))); err != nil {
									return 0, err
								}
							}
						}
					case aud == "all" && r[0] == "visitors" && r[4] == "7" && (rng == "7d" || rng == "24h"):
						for _, row := range rows(body) {
							if _, err := get("visitor", p(aud, "prev", "key", fmt.Sprint(row["key"]))); err != nil {
								return 0, err
							}
						}
					}
				}
			}
		}
	}
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return 0, err
	}
	return len(idx.Responses), os.WriteFile(filepath.Join(dir, "index.json"), append(b, '\n'), 0o600)
}

func rows(body map[string]any) []map[string]any {
	xs, _ := body["rows"].([]any)
	var out []map[string]any
	for _, x := range xs {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// about describes a world for index.json.
func about(w *sim.World, days int, scenarios string) map[string]any {
	return map[string]any{"seed": strconv.FormatUint(w.Seed, 10), "anchor": w.Anchor.Format(time.RFC3339),
		"now": w.Anchor.Add(time.Minute).Format(time.RFC3339), "scale": w.Scale, "days": days,
		"scenarios": scenarios}
}
