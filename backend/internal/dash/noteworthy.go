package dash

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"maps"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/stakeout/backend/internal/apix"
	"github.com/trick77/stakeout/backend/internal/insight"
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
	// IDs are the findings the item covers: its own, and on a briefing line the related ones merged into it.
	IDs []string `json:"ids"`
}

// summable are the units whose figures add up across findings: counts of events. Visitors from several referrers
// may be the same people, shares and changes do not add at all.
var summable = map[string]bool{"requests from": true, "probes of": true, "times": true, "events refused": true}

// mergedItem is one briefing line over several findings of one kind: the figure describes the group (the sum when
// the findings count the same events, none otherwise), the detail names every finding with its own figure.
func mergedItem(group []insight.Fact, gen string) noteItem {
	lead := group[0]
	it := noteItem{ID: lead.ID, Kind: lead.Kind, Gen: &gen, Target: lead.Target, Detail: []string{},
		Text: fmt.Sprintf("%s, and %d related", lead.Text, len(group)-1)}
	var sum float64
	same := summable[lead.Unit]
	for _, f := range group {
		it.IDs = append(it.IDs, f.ID)
		it.Detail = append(it.Detail, strings.TrimSpace(f.Figure+" "+f.Unit+" "+f.Text))
		v, err := strconv.ParseFloat(f.Figure, 64)
		same = same && err == nil && f.Unit == lead.Unit
		sum += v
	}
	if same {
		it.Figure, it.Unit = num(sum), lead.Unit
	}
	return it
}

func (s *Server) handleNoteworthy(w http.ResponseWriter, r *http.Request, q *query) {
	ctx := r.Context()
	// findings=all lists every finding, the ones past maxItems included (the simulator's checks; the UI shows five).
	limit := maxItems
	switch r.URL.Query().Get("findings") {
	case "":
	case "all":
		limit = -1
	default:
		apix.Error(w, http.StatusBadRequest, "findings must be all or left out")
		return
	}
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
	items, more := noteItems(in.Facts, b, limit)
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
// the computed order. At most limit (below 0: all); more is what is left.
func noteItems(facts []insight.Fact, b *insight.Briefing, limit int) ([]noteItem, int) {
	byID := map[string]insight.Fact{}
	for _, f := range facts {
		byID[f.ID] = f
	}
	used := map[string]bool{}
	items := []noteItem{}
	if b != nil {
		for _, l := range b.Lines {
			// The findings of the lead's kind the line names; one of another kind keeps its own item.
			var group []insight.Fact
			for _, id := range l.IDs {
				f, ok := byID[id]
				if !ok || used[id] || (len(group) > 0 && f.Kind != group[0].Kind) {
					continue
				}
				used[id] = true
				group = append(group, f)
			}
			gen := l.Text
			switch len(group) {
			case 0:
			case 1:
				f := group[0]
				items = append(items, noteItem{ID: f.ID, Kind: f.Kind, Figure: f.Figure, Unit: f.Unit, Gen: &gen,
					Text: f.Text, Detail: nonNil(f.Detail), Target: f.Target, IDs: []string{f.ID}})
			default:
				items = append(items, mergedItem(group, gen))
			}
		}
	}
	for _, f := range facts {
		if used[f.ID] {
			continue
		}
		items = append(items, noteItem{ID: f.ID, Kind: f.Kind, Figure: f.Figure, Unit: f.Unit, Text: f.Text,
			Detail: nonNil(f.Detail), Target: f.Target, IDs: []string{f.ID}})
	}
	more := 0
	if limit >= 0 && len(items) > limit {
		more = len(items) - limit
		items = items[:limit]
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
	pl, err := s.planOf(ctx, q)
	if err != nil {
		return in, err
	}
	cur, err := s.buckets(ctx, pl, "all", q.p.Bounds, !q.p.agg)
	if err != nil {
		return in, err
	}
	prev, err := s.buckets(ctx, pl, "all", q.p.Prev, !q.p.prevAgg)
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
	// The lists' findings: the closed part of the daily span against the comparison's.
	if err := s.newKeys(ctx, q, "referrers", minReferrerVisitors, func(k string, v int64) {
		add(insight.Fact{ID: "ref:" + k, Kind: "people", Figure: num(float64(v)), Unit: "visitors from",
			Text: "New referrer: " + k, Score: 150 + float64(v), Target: insight.Target{Panel: "referrers"}})
	}); err != nil {
		return nil, err
	}
	if err := s.newKeys(ctx, q, "errors", 1, func(k string, v int64) {
		add(insight.Fact{ID: "err:" + k, Kind: "problem", Figure: num(float64(v)), Unit: "times",
			Text: "New error: " + truncate(k, 120), Score: 1000 + float64(v), Target: insight.Target{Panel: "errors"}})
	}); err != nil {
		return nil, err
	}
	if q.site.HasFeed {
		if err := s.newKeys(ctx, q, "probed", minProbes, func(k string, v int64) {
			add(insight.Fact{ID: "probe:" + k, Kind: "automation", Figure: num(float64(v)), Unit: "probes of",
				Text: "New probed path: " + k, Score: 500 + float64(v), Target: insight.Target{Panel: "probed"}})
		}); err != nil {
			return nil, err
		}
		if err := s.spikes(ctx, q, add); err != nil {
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

// closedPart is a window's closed part.
func closedPart(w window) window { return closedWin(w.From, w.Closed) }

// newKeys reports the keys of a list dimension that have at least min in the closed part and nothing in the
// comparison: not among its rows, and not maybe collapsed into one of its days' Other rows.
func (s *Server) newKeys(ctx context.Context, q *query, dim string, minValue int64, fn func(string, int64)) error {
	sc := scanOf(dim)
	cur, prev := newSums(), newSums()
	if err := s.scanDim(ctx, q, sc, closedPart(q.p.win), !q.p.agg, cur); err != nil {
		return err
	}
	if err := s.scanDim(ctx, q, sc, *q.p.prevWin, !q.p.prevAgg, prev); err != nil {
		return err
	}
	type kv struct {
		k string
		v int64
	}
	var out []kv
	for k, g := range cur.m {
		v := sc.figure(g)
		if _, seen := prev.m[k]; !seen && !prev.other.hides(k) && v >= minValue && k != "" {
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

// spikes finds one address or one network taking a large and grown share of the requests. A network's requests
// are its networks list's server rows (b); one collapsed into a day's Other row counts as nothing that day, and one
// that may sit in a prior day's Other is no spike: its growth is not known.
func (s *Server) spikes(ctx context.Context, q *query, add func(insight.Fact)) error {
	total, err := s.requestsIn(ctx, q, closedPart(q.p.win), !q.p.agg)
	if err != nil {
		return err
	}
	nets := func(w window, short bool, _ map[string]bool) (map[string]int64, *otherSums, error) {
		sc := scan{dimSpec: dimSpec{where: "source = 'server'", key: "CAST(asn AS TEXT)", a: "COUNT(*)", b: "0",
			value: countA, second: zeroSecond}, dim: "networks", fromB: true}
		sums := newSums()
		if err := s.scanDim(ctx, q, sc, w, short, sums); err != nil {
			return nil, nil, err
		}
		out := map[string]int64{}
		for k, g := range sums.m {
			out[k] = g.a
		}
		return out, &sums.other, nil
	}
	addrs := func(w window, short bool, keys map[string]bool) (map[string]int64, *otherSums, error) {
		out, err := s.addrRequests(ctx, q, w, short, keys)
		return out, &otherSums{}, err
	}
	for _, sp := range []struct {
		id          string
		sums        func(w window, short bool, keys map[string]bool) (map[string]int64, *otherSums, error)
		minRequests int64
		share       float64
	}{{"spike:addr:", addrs, minSpikeRequests, spikeShare}, {"spike:net:", nets, minNetworkRequests, networkShare}} {
		cur, _, err := sp.sums(closedPart(q.p.win), !q.p.agg, nil)
		if err != nil {
			return err
		}
		spiking := map[string]bool{}
		for k, n := range cur {
			if k != "" && k != "0" && n >= sp.minRequests && float64(n) >= sp.share*float64(total) {
				spiking[k] = true
			}
		}
		if len(spiking) == 0 {
			continue
		}
		prev, prevOther, err := sp.sums(*q.p.prevWin, !q.p.prevAgg, spiking)
		if err != nil {
			return err
		}
		for k := range spiking {
			n, was := cur[k], prev[k]
			if was*3 > n || prevOther.hides(k) {
				continue
			}
			label := k
			target := insight.Target{Panel: "visitors", Filter: map[string]string{"key": k}}
			if sp.id == "spike:net:" {
				asn, _ := strconv.ParseInt(k, 10, 64)
				label, _ = s.networkName(asn)
				// The log on that network's requests: what the spike counted, nothing else.
				target = insight.Target{Panel: "log", Filter: map[string]string{"kind": "request", "asn": k}}
			} else if asns := s.asnOf(ctx, q.site.ID, []string{k}); asns[k] != 0 {
				name, _ := s.networkName(asns[k])
				label += " (" + name + ")"
			}
			add(insight.Fact{ID: sp.id + k, Kind: "automation", Figure: num(float64(n)), Unit: "requests from",
				Text:   label,
				Detail: []string{fmt.Sprintf("%s %% of all requests, %s in the %s", num(round1(pct(float64(n), float64(total)))), num(float64(was)), q.p.prevL)},
				Score:  600 + float64(n)/float64(max(total, 1))*100, Target: target})
		}
	}
	return nil
}

// spikeTop is the rows of an aggregate day an address spike is looked for in: an address with a quarter of a span's
// requests has a quarter of some day's, so one of its parts (people, automated) an eighth: among the day's top 8.
const spikeTop = 8

// addrRequests sums the server requests of addresses over w. Raw days list every address; an aggregate day names
// its spikeTop rows by requests (agg_addr_requests). The candidates (keys, or when nil those names and the addresses
// with a quarter of a raw day's requests) then get their aggregate days' sums on agg_addr_key: exact, and every
// address with a quarter of w's requests is among them.
func (s *Server) addrRequests(ctx context.Context, q *query, w window, short bool, keys map[string]bool) (map[string]int64, error) {
	pl, err := s.planOf(ctx, q)
	if err != nil {
		return nil, err
	}
	raw := map[string]int64{}
	cands := map[string]bool{}
	var aggDays []int64
	for _, pc := range pieces(w.From, w.End) {
		day := ms(pc[0]) / dayMS
		if err := pl.read(ctx, pc[0], pc[1], readSpan, short, func() (bool, error) {
			rows, err := s.query(ctx, `SELECT d.ok, t.ip_key FROM (SELECT frozen = 1 AND dirty = 0 AS ok FROM agg_day
				WHERE site_id = ?1 AND day = ?2) AS d LEFT JOIN agg_addr t INDEXED BY agg_addr_requests
				ON t.site_id = ?1 AND t.day = ?2 ORDER BY t.requests DESC LIMIT `+strconv.Itoa(spikeTop), q.site.ID, day)
			if err != nil {
				return false, err
			}
			defer func() { _ = rows.Close() }()
			ok, top := false, []string{}
			for rows.Next() {
				var key sql.NullString
				if err := rows.Scan(&ok, &key); err != nil {
					return false, err
				}
				if key.Valid {
					top = append(top, key.String)
				}
			}
			if err := rows.Err(); err != nil || !ok {
				return false, err
			}
			for _, k := range top {
				cands[k] = true
			}
			aggDays = append(aggDays, day)
			return true, nil
		}, func() error {
			rows, err := s.query(ctx, `SELECT ip_key, COUNT(*) FROM event WHERE site_id = ? AND ts >= ? AND ts < ?
				AND source = 'server' GROUP BY ip_key`, q.site.ID, ms(pc[0]), ms(pc[1]))
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			piece := map[string]int64{}
			var total int64
			for rows.Next() {
				var k string
				var n int64
				if err := rows.Scan(&k, &n); err != nil {
					return err
				}
				piece[k] += n
				raw[k] += n
				total += n
			}
			for k, n := range piece {
				if n*4 >= total {
					cands[k] = true
				}
			}
			return rows.Err()
		}); err != nil {
			return nil, err
		}
	}
	if len(aggDays) == 0 {
		return raw, nil
	}
	if keys != nil {
		cands = keys
	}
	agg, err := s.addrAggRequests(ctx, q, slices.Sorted(maps.Keys(cands)), aggDays)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for k := range cands {
		out[k] = raw[k] + agg[k]
	}
	return out, nil
}

// spanDays is the most days one statement over a run of aggregate days spans: a month, as classDays.
const spanDays = 31

// addrAggRequests is the addresses' server requests on the given aggregate days (oldest first): every address in one
// statement on the key index per month of days, each day's state read with it. A day rebuilt since the plan is
// counted raw (SourceAgg: an error).
func (s *Server) addrAggRequests(ctx context.Context, q *query, keys []string, days []int64) (map[string]int64, error) {
	out := map[string]int64{}
	if len(keys) == 0 {
		return out, nil
	}
	want := map[int64]bool{}
	for _, d := range days {
		want[d] = true
	}
	in := strings.TrimSuffix(strings.Repeat("?, ", len(keys)), ", ")
	stale := map[int64]bool{}
	for i := 0; i < len(days); {
		lo, hi := days[i], days[i]
		for i < len(days) && days[i] < lo+spanDays {
			hi = days[i]
			i++
		}
		args := make([]any, 0, len(keys)+3)
		for _, k := range keys {
			args = append(args, k)
		}
		rows, err := s.query(ctx, `SELECT d.day, d.frozen = 1 AND d.dirty = 0, t.ip_key, COALESCE(t.requests, 0)
			FROM agg_day d LEFT JOIN agg_addr t INDEXED BY agg_addr_key ON t.site_id = d.site_id AND t.ip_key IN (`+in+`)
			AND t.day = d.day WHERE d.site_id = ? AND d.day >= ? AND d.day <= ?`, append(args, q.site.ID, lo, hi)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var day, n int64
			var ok bool
			var key sql.NullString
			if err := rows.Scan(&day, &ok, &key, &n); err != nil {
				_ = rows.Close()
				return nil, err
			}
			switch {
			case !want[day]:
			case !ok:
				stale[day] = true
			case key.Valid:
				out[key.String] += n
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	pl, _ := s.planOf(ctx, q)
	for day := range stale {
		if pl.src == SourceAgg {
			return nil, fmt.Errorf("%w: site %d, %s rebuilt while read", ErrNoAggregates, q.site.ID,
				time.UnixMilli(day*dayMS).UTC().Format(time.DateOnly))
		}
		for _, key := range keys {
			var n int64
			if err := s.row(ctx, `SELECT COUNT(*) FROM event WHERE site_id = ? AND ip_key = ? AND ts >= ? AND ts < ?
				AND source = 'server'`, q.site.ID, key, day*dayMS, (day+1)*dayMS).Scan(&n); err != nil {
				return nil, err
			}
			out[key] += n
		}
	}
	return out, nil
}

// requestsIn is the server requests over w, a piece at a time from its source.
func (s *Server) requestsIn(ctx context.Context, q *query, w window, short bool) (int64, error) {
	pl, err := s.planOf(ctx, q)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, pc := range pieces(w.From, w.End) {
		if err := pl.read(ctx, pc[0], pc[1], time.Hour, short, func() (bool, error) {
			var ok sql.NullBool
			var n int64
			if err := s.row(ctx, `SELECT d.ok, COALESCE(SUM(t.requests), 0)`+
				aggFrom("agg_hour", "t.hour >= ?3 AND t.hour < ?4"), q.site.ID, ms(pc[0])/dayMS, ms(pc[0])/hourMS,
				ms(pc[1])/hourMS).Scan(&ok, &n); err != nil || !ok.Bool {
				return false, err
			}
			total += n
			return true, nil
		}, func() error {
			var n int64
			err := s.row(ctx, `SELECT COUNT(*) FROM event WHERE site_id = ? AND ts >= ? AND ts < ? AND source = 'server'`,
				q.site.ID, ms(pc[0]), ms(pc[1])).Scan(&n)
			total += n
			return err
		}); err != nil {
			return 0, err
		}
	}
	return total, nil
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
