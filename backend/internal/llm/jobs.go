package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/trick77/nui/backend/internal/insight"
	"github.com/trick77/nui/backend/internal/store"
)

// Briefing answers from the cache: the stored briefing when its facts are unchanged, or when the range's cadence has
// not passed since it was written (a cached line whose fact is gone drops out on the dashboard). Otherwise it queues
// a new one and still answers with the old.
func (s *Service) Briefing(ctx context.Context, in insight.BriefingInput) *insight.Briefing {
	var cached *insight.Briefing
	b, ok, err := s.st.Briefing(ctx, in.SiteID, in.Range)
	if err == nil && ok {
		var br insight.Briefing
		if json.Unmarshal([]byte(b.Body), &br) == nil {
			cached = &br
		}
	}
	fresh := ok && (b.FactsHash == in.Hash || s.o.Now().Sub(time.UnixMilli(b.CreatedAt)) < Cadence[in.Range])
	if !fresh && len(in.Facts) > 0 {
		s.enqueue(job{key: fmt.Sprintf("brief:%d:%s", in.SiteID, in.Range), site: in.SiteID, briefing: &in}, false)
	}
	return cached
}

type briefOut struct {
	Headline string `json:"headline"`
	Lines    []struct {
		IDs  []string `json:"ids"`
		Text string   `json:"text"`
	} `json:"lines"`
}

var briefSchema = map[string]any{
	"type": "object", "additionalProperties": false, "required": []string{"headline", "lines"},
	"properties": map[string]any{
		"headline": map[string]any{"type": "string", "description": "about eight words"},
		"lines": map[string]any{"type": "array", "maxItems": 5, "items": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"ids", "text"},
			"properties": map[string]any{
				"ids":  map[string]any{"type": "array", "minItems": 1, "maxItems": 4, "items": map[string]any{"type": "string"}},
				"text": map[string]any{"type": "string", "description": "about nine words"},
			},
		}},
	},
}

const briefPrompt = `You brief the owner of a website about its analytics. You get the period's headline figures and ` +
	`a list of computed findings, each with an id. Choose the findings that matter most for someone who wants to ` +
	`act (at most five), most important first. You may merge findings that are clearly one thing (one operator, ` +
	`one cause) into one line by listing several ids. For each line write one short statement of about nine words ` +
	`saying what it means; the figure is shown beside it. Also write a headline of about eight words for the whole ` +
	`period. Use only ids from the list.`

func (s *Service) brief(ctx context.Context, in insight.BriefingInput) error {
	data := map[string]any{"site": in.Domain, "range": in.Range, "compared_with": in.PrevLabel, "figures": in.Tiles,
		"findings": in.Facts}
	raw := dataOf(data)
	obj, err := s.call(ctx, in.SiteID, "briefing", briefSchema, briefPrompt, data, 800)
	if err != nil {
		return err
	}
	var out briefOut
	if err := json.Unmarshal([]byte(obj), &out); err != nil {
		return fmt.Errorf("briefing: %w", err)
	}
	ids := map[string]bool{}
	for _, f := range in.Facts {
		ids[f.ID] = true
	}
	br := insight.Briefing{AsOf: in.AsOf, Lines: []insight.BriefLine{}}
	if h := clip(out.Headline, maxHeadline); grounded(h, raw) {
		br.Headline = h
	}
	for _, l := range out.Lines {
		if len(br.Lines) == 5 {
			break
		}
		var keep []string
		for _, id := range l.IDs {
			if ids[id] && !contains(keep, id) { // an id that was not offered is dropped, never shown
				keep = append(keep, id)
			}
		}
		text := clip(l.Text, maxLine)
		if len(keep) == 0 || text == "" || !grounded(text, raw) {
			continue
		}
		br.Lines = append(br.Lines, insight.BriefLine{IDs: keep, Text: text})
	}
	body, _ := json.Marshal(br)
	return s.st.PutBriefing(ctx, store.Briefing{SiteID: in.SiteID, Range: in.Range, FactsHash: in.Hash,
		AsOf: in.AsOf.UnixMilli(), Body: string(body), CreatedAt: s.o.Now().UnixMilli()})
}

// Readings answers the cached readings by actor key and queues the missing signatures.
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
			in := in
			s.enqueue(job{key: "read:" + in.Signature, site: in.SiteID, reading: &in}, urgent)
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

var readSchema = map[string]any{
	"type": "object", "additionalProperties": false, "required": []string{"verdict", "summary", "evidence"},
	"properties": map[string]any{
		"verdict":  map[string]any{"type": "string", "enum": insight.Verdicts},
		"summary":  map[string]any{"type": "string", "description": "one sentence, about fourteen words"},
		"evidence": map[string]any{"type": "array", "maxItems": 3, "items": map[string]any{"type": "string"}},
	},
}

const readPrompt = `You read what one visitor of a website did, from its server and browser trail (repeats collapsed), ` +
	`its signals and computed evidence. Choose the verdict that fits best from the list. Write one sentence of ` +
	`about fourteen words saying what it does and, if it is automated, what it is probably after. Pick up to three ` +
	`evidence ids that support the verdict best. Use only ids from the evidence list.`

func (s *Service) read(ctx context.Context, in insight.ReadingInput) error {
	data := map[string]any{"class": in.Class, "client": in.Client, "network": in.Network, "signals": in.Signals,
		"evidence": in.Evidence, "trail_newest_first": in.Trail}
	raw := dataOf(data)
	obj, err := s.call(ctx, in.SiteID, "reading", readSchema, readPrompt, data, 400)
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
	rd := insight.Reading{Verdict: out.Verdict, Evidence: []string{}, From: in.From, To: in.To}
	if sum := clip(out.Summary, maxSummary); grounded(sum, raw) {
		rd.Summary = sum
	}
	for _, id := range out.Evidence {
		for _, e := range in.Evidence {
			if e.ID == id && !contains(rd.Evidence, id) && len(rd.Evidence) < 3 {
				rd.Evidence = append(rd.Evidence, id)
			}
		}
	}
	body, _ := json.Marshal(rd)
	return s.st.PutReading(ctx, store.Reading{Signature: in.Signature, Body: string(body),
		BasisFrom: in.From.UnixMilli(), BasisTo: in.To.UnixMilli(), CreatedAt: s.o.Now().UnixMilli()})
}

// Labels answers the stored labels and queues the missing values (at most a batch per call; the rest is asked
// again on the next view). A label is written once per value and kept.
func (s *Service) Labels(ctx context.Context, site int64, kind string, values []string) map[string]string {
	got, err := s.st.Labels(ctx, kind, values)
	if err != nil {
		return map[string]string{}
	}
	var missing []string
	for _, v := range values {
		if _, ok := got[v]; !ok && v != "" && !contains(missing, v) {
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
			"label": map[string]any{"type": "string", "description": "about four words"},
		},
	}}},
}

var labelPrompts = map[string]string{
	"path": `Each value is a URL path a client requested on a website and got 401, 403 or 404. Label what someone ` +
		`requesting it is probably looking for, in about four words (for example "looking for leaked secrets", ` +
		`"WordPress login probe"). Answer with each value's index i.`,
	"ua": `Each value is the user agent string of a client that is not a browser. Label what kind of client it ` +
		`is, in about four words (for example "Python scraping script", "SEO crawler"). Answer with each value's index i.`,
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
	raw := dataOf(data)
	for _, l := range out.Labels {
		if l.I < 0 || l.I >= len(j.values) {
			continue
		}
		label := clip(l.Label, maxLabel)
		if label == "" || !grounded(label, raw) {
			continue
		}
		if err := s.st.PutLabel(ctx, j.kind, j.values[l.I], label); err != nil {
			return err
		}
	}
	return nil
}
