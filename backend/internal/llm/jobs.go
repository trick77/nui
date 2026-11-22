package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/nui/backend/internal/insight"
	"github.com/trick77/nui/backend/internal/store"
)

// Briefing answers from the cache, and only with a briefing written for the input's set of fact ids: the ids carry
// what a line may not contradict (a trend's direction), the lines carry no figure, so such a briefing fits however
// the numbers moved. For another set it answers nil (the dashboard shows the computed text) and queues a new one,
// at most every quarter of the range's cadence. No findings, no briefing.
func (s *Service) Briefing(ctx context.Context, in insight.BriefingInput) *insight.Briefing {
	if len(in.Facts) == 0 {
		return nil
	}
	b, ok, err := s.st.Briefing(ctx, in.SiteID, in.Range)
	if err != nil {
		return nil
	}
	if ok && b.FactsHash == in.Hash {
		var br insight.Briefing
		if json.Unmarshal([]byte(b.Body), &br) == nil {
			return &br
		}
	}
	if !ok || s.o.Now().Sub(time.UnixMilli(b.CreatedAt)) >= Cadence[in.Range]/4 {
		s.enqueue(job{key: fmt.Sprintf("brief:%d:%s", in.SiteID, in.Range), site: in.SiteID, briefing: &in}, false)
	}
	return nil
}

type briefOut struct {
	Headline string `json:"headline"`
	Lines    []struct {
		IDs  []string `json:"ids"`
		Text string   `json:"text"`
	} `json:"lines"`
}

// briefSchema is the briefing's answer, the citable finding ids as its ids' enum.
func briefSchema(ids []string) map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"headline", "lines"},
		"properties": map[string]any{
			"headline": map[string]any{"type": "string", "description": "about eight words, no figures"},
			"lines": map[string]any{"type": "array", "maxItems": 5, "items": map[string]any{
				"type": "object", "additionalProperties": false, "required": []string{"ids", "text"},
				"properties": map[string]any{
					// As many as are offered: a line covers every related finding of its kind (five probed
					// paths of one scanner are one line, not four and a stray).
					"ids": map[string]any{"type": "array", "minItems": 1, "maxItems": len(ids),
						"items": map[string]any{"type": "string", "enum": ids}},
					"text": map[string]any{"type": "string", "description": "about nine words, no figures"},
				},
			}},
		},
	}
}

const briefPrompt = `You brief the owner of a website about its analytics. The data holds the period's headline ` +
	`figures (for context only, they cannot be cited) and a list of findings, each with an id. Choose the findings ` +
	`that matter most for someone who wants to act (at most five), most important first. A line cites findings by ` +
	`their id, only ids from the findings list; findings of one kind that are clearly one thing (one operator, ` +
	`one cause) go into one line that lists all of their ids, however many; a finding of another kind gets a line ` +
	`of its own. For each line write one short statement of about nine words ` +
	`saying what it means. Also write a headline of about eight words for the whole period.`

func (s *Service) brief(ctx context.Context, in insight.BriefingInput) error {
	figures := make([]map[string]string, 0, len(in.Tiles))
	for _, t := range in.Tiles {
		figures = append(figures, map[string]string{"figure": t.Label, "value": t.Value, "before": t.Prev,
			"change": t.Change})
	}
	ids := make([]string, 0, len(in.Facts))
	findings := make([]map[string]any, 0, len(in.Facts))
	for _, f := range in.Facts {
		ids = append(ids, f.ID)
		findings = append(findings, map[string]any{"id": f.ID, "kind": f.Kind, "figure": f.Figure + " " + f.Unit,
			"text": f.Text, "detail": f.Detail})
	}
	data := map[string]any{"site": in.Domain, "range": in.Range, "compared_with": in.PrevLabel,
		"figures_not_citable": figures, "findings": findings}
	obj, err := s.call(ctx, in.SiteID, "briefing", briefSchema(ids), briefPrompt, data, 800)
	if err != nil {
		return err
	}
	var out briefOut
	if err := json.Unmarshal([]byte(obj), &out); err != nil {
		return fmt.Errorf("briefing: %w", err)
	}
	br := insight.Briefing{AsOf: in.AsOf, Lines: []insight.BriefLine{}}
	if h := clip(out.Headline, maxHeadline); noFigures(h) {
		br.Headline = h
	}
	for _, l := range out.Lines {
		if len(br.Lines) == 5 {
			break
		}
		var keep []string
		for _, id := range l.IDs {
			if contains(ids, id) && !contains(keep, id) { // an id that was not offered is dropped, never shown
				keep = append(keep, id)
			}
		}
		text := clip(l.Text, maxLine)
		if len(keep) == 0 || text == "" || !noFigures(text) {
			continue // the finding keeps its computed text
		}
		br.Lines = append(br.Lines, insight.BriefLine{IDs: keep, Text: text})
	}
	body, _ := json.Marshal(br)
	return s.st.PutBriefing(ctx, store.Briefing{SiteID: in.SiteID, Range: in.Range, FactsHash: in.Hash,
		AsOf: in.AsOf.UnixMilli(), Body: string(body), CreatedAt: s.o.Now().UnixMilli()})
}

// Readings answers the cached readings by actor key and queues the missing signatures of the inputs that ask.
func (s *Service) Readings(ctx context.Context, ins []insight.ReadingInput, urgent bool) map[string]insight.Reading {
	out := map[string]insight.Reading{}
	sigs := make([]string, 0, len(ins))
	for _, in := range ins {
		sigs = append(sigs, in.Signature)
	}
	got, err := s.st.Readings(ctx, sigs)
	if err != nil {
		return out
	}
	for _, in := range ins {
		r, ok := got[in.Signature]
		if !ok {
			if in.Queue {
				in := in
				s.enqueue(job{key: "read:" + in.Signature, site: in.SiteID, reading: &in}, urgent)
			}
			continue
		}
		var rd insight.Reading
		if json.Unmarshal([]byte(r.Body), &rd) == nil {
			out[in.Key] = rd
		}
	}
	return out
}

type readOut struct {
	Verdict  string   `json:"verdict"`
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence"`
}

func readSchema(ids []string) map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"verdict", "summary", "evidence"},
		"properties": map[string]any{
			"verdict":  map[string]any{"type": "string", "enum": insight.Verdicts},
			"summary":  map[string]any{"type": "string", "description": "one sentence, about fourteen words, no figures"},
			"evidence": map[string]any{"type": "array", "maxItems": 3, "items": map[string]any{"type": "string", "enum": ids}},
		},
	}
}

// readPrompt lists every verdict with its meaning: the schema's enum alone never reaches the model's reading.
var readPrompt = func() string {
	var b strings.Builder
	b.WriteString(`You read what one visitor of a website did, from its server and browser trail (repeats ` +
		`collapsed), its signals and computed evidence. Choose the one verdict from this list that fits best:` + "\n")
	for _, v := range insight.Verdicts {
		b.WriteString("- " + v + ": " + insight.VerdictMeanings[v] + "\n")
	}
	b.WriteString(`Write one sentence of about fourteen words saying what it does and, if it is automated, what ` +
		`it is probably after; it must agree with the verdict. Pick up to three evidence ids that support the ` +
		`verdict best, only ids from the evidence list. Write no address, network name or date: the reading is ` +
		`shown for every visitor that behaves alike, with that visitor's own figures beside it.`)
	return b.String()
}()

func (s *Service) read(ctx context.Context, in insight.ReadingInput) error {
	data := map[string]any{"class": in.Class, "client": in.Client, "network_kind": in.NetworkKind,
		"signals": in.Signals, "evidence": in.Evidence, "trail_newest_first": in.Trail}
	ids := make([]string, 0, len(in.Evidence))
	for _, e := range in.Evidence {
		ids = append(ids, e.ID)
	}
	obj, err := s.call(ctx, in.SiteID, "reading", readSchema(ids), readPrompt, data, 400)
	if err != nil {
		return err
	}
	var out readOut
	if err := json.Unmarshal([]byte(obj), &out); err != nil {
		return fmt.Errorf("reading: %w", err)
	}
	if !contains(insight.Verdicts, out.Verdict) {
		out.Verdict = "Unclear"
	}
	rd := insight.Reading{Verdict: out.Verdict, Evidence: []string{}}
	if sum := clip(out.Summary, maxSummary); noFigures(sum) {
		rd.Summary = sum
	}
	for _, id := range out.Evidence {
		if contains(ids, id) && !contains(rd.Evidence, id) && len(rd.Evidence) < 3 {
			rd.Evidence = append(rd.Evidence, id)
		}
	}
	body, _ := json.Marshal(rd)
	return s.st.PutReading(ctx, store.Reading{Signature: in.Signature, Body: string(body),
		CreatedAt: s.o.Now().UnixMilli()})
}

// Labels answers the stored labels and queues the missing values (at most a batch per call; the rest is asked
// again on the next view). A label is written once per value and kept; a value that got none usable is not asked
// again before labelRetry.
func (s *Service) Labels(ctx context.Context, site int64, kind string, values []string) map[string]string {
	got, err := s.st.Labels(ctx, kind, values)
	if err != nil {
		return map[string]string{}
	}
	var missing []string
	for _, v := range values {
		if _, ok := got[v]; !ok && v != "" && !contains(missing, v) && !s.labelMissed(kind, v) {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		missing = missing[:min(len(missing), maxLabelsIn)]
		s.enqueue(job{key: "label:" + kind + ":" + missing[0] + ":" + strconv.Itoa(len(missing)), site: site,
			labels: &labelJob{kind: kind, values: missing}}, false)
	}
	return got
}

type labelOut struct {
	Labels []struct {
		I     int    `json:"i"`
		Label string `json:"label"`
	} `json:"labels"`
}

var labelSchema = map[string]any{
	"type": "object", "additionalProperties": false, "required": []string{"labels"},
	"properties": map[string]any{"labels": map[string]any{"type": "array", "items": map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"i", "label"},
		"properties": map[string]any{
			"i":     map[string]any{"type": "integer"},
			"label": map[string]any{"type": "string", "description": "about four words, no figures"},
		},
	}}},
}

var labelPrompts = map[string]string{
	"path": `Each value is a URL path a client requested on a website and got 401, 403 or 404. Label what someone ` +
		`requesting it is probably looking for, in about four words (for example "looking for leaked secrets", ` +
		`"WordPress login probe"). Answer with each value's index i.`,
	"ua": `Each value is the user agent string of a client that is not a browser. Label what kind of client it ` +
		`is, in about four words (for example "Python scraping script", "SEO crawler"). Answer with each value's ` +
		`index i. Write no version numbers.`,
}

func (s *Service) label(ctx context.Context, site int64, j labelJob) error {
	items := make([]map[string]any, len(j.values))
	for i, v := range j.values {
		items[i] = map[string]any{"i": i, "value": v}
	}
	data := map[string]any{"values": items}
	obj, err := s.call(ctx, site, "labels", labelSchema, labelPrompts[j.kind], data, 600)
	if err != nil {
		return err
	}
	var out labelOut
	if err := json.Unmarshal([]byte(obj), &out); err != nil {
		return fmt.Errorf("labels: %w", err)
	}
	labelled := map[int]bool{}
	for _, l := range out.Labels {
		if l.I < 0 || l.I >= len(j.values) {
			continue
		}
		label := clip(l.Label, maxLabel)
		if label == "" || !noFigures(label) {
			continue
		}
		if err := s.st.PutLabel(ctx, j.kind, j.values[l.I], label); err != nil {
			return err
		}
		labelled[l.I] = true
	}
	// Asked, nothing usable: remembered, so the next views do not spend the budget on it again at once.
	for i, v := range j.values {
		if !labelled[i] {
			s.missLabel(j.kind, v)
		}
	}
	return nil
}
