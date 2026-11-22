package dash

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/trick77/nui/backend/internal/apix"
	"github.com/trick77/nui/backend/internal/insight"
)

// maxItems is how many findings Noteworthy shows; the rest is "n more".
const maxItems = 5

// Thresholds of the computed findings: below them a change is noise.
const (
	minVisitorsForTrend = 20
	trendPct            = 20.0
	minSpikeRequests    = 100
	spikeShare          = 0.25
	minNetworkRequests  = 200
	networkShare        = 0.4
	minProbes           = 3
	minReferrerVisitors = 2
	autoShareSwingPP    = 5.0
)

type noteItem struct {
	ID     string         `json:"id"`
	Kind   string         `json:"kind"`
	Figure string         `json:"figure"`
	Unit   string         `json:"unit"`
	Gen    *string        `json:"gen"`
	Text   string         `json:"text"`
	Detail []string       `json:"detail"`
	Target insight.Target `json:"target"`
}

func (s *Server) handleNoteworthy(w http.ResponseWriter, r *http.Request, q *query) {
	ctx := r.Context()
	// Always all classes against the prior period, on closed buckets (contract: ignores aud).
	fq := *q
	fq.aud = "all"
	fq.p = newPeriod(q.rng, "prev", q.p.Now, q.loc)
	in, err := s.briefingInput(ctx, &fq)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The briefing is the model's only for the very set of fact ids it was written for (the model answers nil
	// otherwise, and for no findings): the computed text stands in until a new one is written.
	var b *insight.Briefing
	if s.Model != nil && s.Model.Configured() && len(in.Facts) > 0 {
		b = s.Model.Briefing(ctx, in)
	}
	items, more := noteItems(in.Facts, b)
	headline := map[string]any{"gen": nil, "text": fmt.Sprintf("%d findings against the %s", len(in.Facts), fq.p.prevL)}
	if len(in.Facts) == 1 {
		headline["text"] = "1 finding against the " + fq.p.prevL
	}
	asOf := in.AsOf
	if b != nil {
		if b.Headline != "" {
			headline["gen"] = b.Headline
		}
		// The briefing's own time: its words were written for the facts as of then.
		asOf = b.AsOf
	}
	apix.JSON(w, http.StatusOK, map[string]any{"headline": headline, "as_of": iso(asOf), "items": items, "more": more})
}

// noteItems orders the findings: the briefing's lines first (its order, its words, related facts merged), then
// the computed order. At most maxItems; more is what is left.
func noteItems(facts []insight.Fact, b *insight.Briefing) ([]noteItem, int) {
	byID := map[string]insight.Fact{}
	for _, f := range facts {
		byID[f.ID] = f
	}
	used := map[string]bool{}
	items := []noteItem{}
	if b != nil {
		for _, l := range b.Lines {
			var lead *insight.Fact
			detail := []string{}
			for _, id := range l.IDs {
				f, ok := byID[id]
				if !ok || used[id] {
					continue
				}
				used[id] = true
				if lead == nil {
					lead = &f
					detail = append(detail, f.Detail...)
					continue
				}
				detail = append(detail, f.Text)
			}
			if lead == nil {
				continue
			}
			gen := l.Text
			items = append(items, noteItem{ID: lead.ID, Kind: lead.Kind, Figure: lead.Figure, Unit: lead.Unit,
				Gen: &gen, Text: lead.Text, Detail: detail, Target: lead.Target})
		}
	}
	for _, f := range facts {
		if used[f.ID] {
			continue
		}
		items = append(items, noteItem{ID: f.ID, Kind: f.Kind, Figure: f.Figure, Unit: f.Unit, Text: f.Text,
			Detail: nonNil(f.Detail), Target: f.Target})
	}
	more := 0
	if len(items) > maxItems {
		more = len(items) - maxItems
		items = items[:maxItems]
	}
	return items, more
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// briefingInput computes the findings and the tile summary of a range against its prior period.
func (s *Server) briefingInput(ctx context.Context, q *query) (insight.BriefingInput, error) {
	in := insight.BriefingInput{SiteID: q.site.ID, Domain: q.site.Domain, Range: q.rng, PrevLabel: q.p.prevL,
		AsOf: q.p.ClosedEnd()}
	cur, err := s.buckets(ctx, q.site.ID, "all", q.p.Bounds)
	if err != nil {
		return in, err
	}
	prev, err := s.buckets(ctx, q.site.ID, "all", q.p.Prev)
	if err != nil {
		return in, err
	}
	tiles := s.tiles(q, cur, prev)
	for _, t := range tiles {
		pv := ""
		if t.Prev != nil {
			pv = num(*t.Prev) + t.Unit
		}
		in.Tiles = append(in.Tiles, insight.TileFact{Key: t.Key, Label: t.Label, Value: num(t.Value) + t.Unit,
			Prev: pv, Change: changeText(t.Change)})
	}
	facts, err := s.findings(ctx, q, tiles)
	if err != nil {
		return in, err
	}
	in.Facts = facts
	in.Hash = factsHash(facts)
	return in, nil
}

// factsHash identifies a set of fact ids, in any order: a briefing written for it fits it whatever the figures.
func factsHash(facts []insight.Fact) string {
	ids := make([]string, 0, len(facts))
	for _, f := range facts {
		ids = append(ids, f.ID)
	}
	slices.Sort(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return hex.EncodeToString(sum[:])
}

// closedQuery restricts q to its closed buckets and its comparison's: the bounds a finding is computed on.
func closedQuery(q *query) *query {
	c := *q
	n := q.p.n()
	c.p.Bounds = q.p.Bounds[:n]
	c.p.Prev = q.p.Prev[:n]
	return &c
}

// findings computes Noteworthy's facts. Stable ids: the finding's type and key.
func (s *Server) findings(ctx context.Context, q *query, tiles []tile) ([]insight.Fact, error) {
	var facts []insight.Fact
	add := func(f insight.Fact) {
		if f.Detail == nil {
			f.Detail = []string{}
		}
		if f.Target.Filter == nil {
			f.Target.Filter = map[string]string{}
		}
		facts = append(facts, f)
	}
	for _, t := range tiles {
		switch {
		case t.Key == "visitors" && t.Change != nil && t.Change["pct"] != nil && t.Value >= minVisitorsForTrend:
			p := t.Change["pct"].(float64)
			if math.Abs(p) >= trendPct {
				// The direction is part of the id: a line written for a rise never sits beside a fall.
				add(insight.Fact{ID: "trend:visitors:" + upDown(p), Kind: "people", Figure: signed(p) + " %", Unit: "visitors",
					Text:  fmt.Sprintf("Visitors %s against the %s (%s, was %s)", upDown(p), q.p.prevL, num(t.Value), num(*t.Prev)),
					Score: 100 + math.Abs(p), Target: insight.Target{Panel: "log",
						Filter: map[string]string{"kind": "page_view"}}})
			}
		case t.Key == "auto_share" && t.Change != nil && t.Change["pp"] != nil:
			pp := t.Change["pp"].(float64)
			if math.Abs(pp) >= autoShareSwingPP {
				add(insight.Fact{ID: "trend:auto_share:" + upDown(pp), Kind: "automation", Figure: num(t.Value) + " %",
					Unit: "unknown automated", Text: fmt.Sprintf("Unknown automated share %s by %s pp", upDown(pp),
						num(math.Abs(pp))), Score: 400 + math.Abs(pp), Target: insight.Target{Panel: "log",
						Filter: map[string]string{"class": "unknown"}}})
			}
		}
	}
	cq := closedQuery(q)
	if err := s.newKeys(ctx, cq, "referrers", minReferrerVisitors, func(k string, v int64) {
		add(insight.Fact{ID: "ref:" + k, Kind: "people", Figure: num(float64(v)), Unit: "visitors from",
			Text: "New referrer: " + k, Score: 150 + float64(v), Target: insight.Target{Panel: "referrers"}})
	}); err != nil {
		return nil, err
	}
	if err := s.newKeys(ctx, cq, "errors", 1, func(k string, v int64) {
		add(insight.Fact{ID: "err:" + k, Kind: "problem", Figure: num(float64(v)), Unit: "times",
			Text: "New error: " + truncate(k, 120), Score: 1000 + float64(v), Target: insight.Target{Panel: "errors"}})
	}); err != nil {
		return nil, err
	}
	if q.site.HasFeed {
		if err := s.newKeys(ctx, cq, "probed", minProbes, func(k string, v int64) {
			add(insight.Fact{ID: "probe:" + k, Kind: "automation", Figure: num(float64(v)), Unit: "probes of",
				Text: "New probed path: " + k, Score: 500 + float64(v), Target: insight.Target{Panel: "probed"}})
		}); err != nil {
			return nil, err
		}
		if err := s.spikes(ctx, cq, add); err != nil {
			return nil, err
		}
	}
	// Collector throttling over the closed range.
	var refused int64
	if err := s.row(ctx, `SELECT COALESCE(SUM(rate_limited + dropped), 0) FROM ingest_stat
		WHERE site_id = ? AND hour >= ? AND hour < ?`, q.site.ID, q.p.From().Unix()/3600,
		q.p.ClosedEnd().Unix()/3600).Scan(&refused); err != nil {
		return nil, err
	}
	if refused > 0 {
		add(insight.Fact{ID: "collector:refused", Kind: "problem", Figure: num(float64(refused)), Unit: "events refused",
			Text: "The collector refused events (rate limit or full queue)", Score: 900 + float64(refused),
			Target: insight.Target{Panel: "domain"}})
	}
	slices.SortStableFunc(facts, func(a, b insight.Fact) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if facts == nil {
		facts = []insight.Fact{}
	}
	return facts, nil
}

// newKeys reports the keys of a list dimension that have at least min now and nothing in the comparison.
func (s *Server) newKeys(ctx context.Context, q *query, dim string, minValue int64, fn func(string, int64)) error {
	d := dims[dim]
	cur, prev := aggMap{}, aggMap{}
	if err := s.scanDim(ctx, q, d, "", nil, q.p.Bounds, cur); err != nil {
		return err
	}
	if err := s.scanDim(ctx, q, d, "", nil, q.p.Prev, prev); err != nil {
		return err
	}
	type kv struct {
		k string
		v int64
	}
	var out []kv
	for k, g := range cur {
		v := d.value(g.a, g.b, int64(len(g.d)))
		if _, seen := prev[k]; !seen && v >= minValue && k != "" {
			out = append(out, kv{k, v})
		}
	}
	slices.SortFunc(out, func(a, b kv) int {
		if c := cmp.Compare(b.v, a.v); c != 0 {
			return c
		}
		return cmp.Compare(a.k, b.k)
	})
	for _, x := range out[:min(len(out), 10)] {
		fn(x.k, x.v)
	}
	return nil
}

// spikes finds one address or one network taking a large and grown share of the requests.
func (s *Server) spikes(ctx context.Context, q *query, add func(insight.Fact)) error {
	for _, sp := range []struct {
		key, id     string
		minRequests int64
		share       float64
	}{{"ip_key", "spike:addr:", minSpikeRequests, spikeShare}, {"CAST(asn AS TEXT)", "spike:net:", minNetworkRequests, networkShare}} {
		d := dimSpec{where: "source = 'server'", key: sp.key, a: "COUNT(*)", b: "0", value: countA, second: zeroSecond}
		cur, prev := aggMap{}, aggMap{}
		if err := s.scanDim(ctx, q, d, "", nil, q.p.Bounds, cur); err != nil {
			return err
		}
		if err := s.scanDim(ctx, q, d, "", nil, q.p.Prev, prev); err != nil {
			return err
		}
		var total int64
		for _, g := range cur {
			total += g.a
		}
		for k, g := range cur {
			if k == "" || k == "0" || g.a < sp.minRequests || float64(g.a) < sp.share*float64(total) {
				continue
			}
			was := int64(0)
			if p, ok := prev[k]; ok {
				was = p.a
			}
			if was*3 > g.a {
				continue
			}
			label := k
			target := insight.Target{Panel: "visitors", Filter: map[string]string{"key": k}}
			if sp.id == "spike:net:" {
				asn, _ := strconv.ParseInt(k, 10, 64)
				label, _ = s.networkName(asn)
				target = insight.Target{Panel: "log", Filter: map[string]string{"kind": "request"}}
			} else if asns := s.asnOf(ctx, q.site.ID, []string{k}); asns[k] != 0 {
				name, _ := s.networkName(asns[k])
				label += " (" + name + ")"
			}
			add(insight.Fact{ID: sp.id + k, Kind: "automation", Figure: num(float64(g.a)), Unit: "requests from",
				Text:   label,
				Detail: []string{fmt.Sprintf("%s %% of all requests, %s in the %s", num(round1(pct(float64(g.a), float64(total)))), num(float64(was)), q.p.prevL)},
				Score:  600 + float64(g.a)/float64(max(total, 1))*100, Target: target})
		}
	}
	return nil
}

func upDown(f float64) string {
	if f >= 0 {
		return "up"
	}
	return "down"
}

func signed(f float64) string {
	if f > 0 {
		return "+" + num(f)
	}
	return num(f)
}

// num formats a figure for text: no decimals for whole numbers, one otherwise.
func num(f float64) string {
	if f == math.Trunc(f) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(round1(f), 'f', 1, 64)
}

func changeText(c change) string {
	switch {
	case c == nil:
		return ""
	case c["new"] == true:
		return "new"
	case c["pct"] != nil:
		return signed(c["pct"].(float64)) + " %"
	case c["pp"] != nil:
		return signed(c["pp"].(float64)) + " pp"
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}
