package agg

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// The classes are stored for good: their number and a few starts are pinned.
func TestHistClassesArePermanent(t *testing.T) {
	if n := len(histStarts); n != 167 {
		t.Errorf("%d classes", n)
	}
	for i, want := range map[int]int64{0: 0, 1: 1, 12: 12, 22: 22, 23: 23, 24: 25} {
		if histStarts[i] != want {
			t.Errorf("class %d starts at %d, want %d", i, histStarts[i], want)
		}
	}
	if !slices.IsSorted(histStarts) || histStarts[len(histStarts)-1] != histMax {
		t.Errorf("starts %v", histStarts)
	}
}

// Percentiles are within 5 % of the exact nearest-rank value, whatever the shape.
func TestHistPercentile(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: test data
	for name, gen := range map[string]func() int64{
		"uniform":   func() int64 { return r.Int64N(5000) },
		"lognormal": func() int64 { return int64(math.Exp(r.NormFloat64()*1.5 + 4)) },
		"constant":  func() int64 { return 37 },
		"fast":      func() int64 { return r.Int64N(4) },
	} {
		var h Hist
		vals := make([]int64, 4000)
		for i := range vals {
			vals[i] = gen()
			h.Add(vals[i], 1)
		}
		slices.Sort(vals)
		for _, p := range []float64{1, 50, 95, 99, 100} {
			exact := vals[max(1, int(math.Ceil(float64(len(vals))*p/100)))-1]
			got, ok := h.Percentile(p)
			if !ok || math.Abs(float64(got-exact)) > max(1, 0.05*float64(exact)) {
				t.Errorf("%s p%v = %d, exact %d", name, p, got, exact)
			}
		}
	}
}

func TestHistEdges(t *testing.T) {
	var h Hist
	if _, ok := h.Percentile(50); ok || h.Marshal() != nil {
		t.Error("an empty histogram answers")
	}
	h.Add(-5, 1)
	h.Add(histMax*10, 1)
	h.Add(3, 0)
	if h.Count() != 2 {
		t.Fatalf("count %d", h.Count())
	}
	if p, _ := h.Percentile(50); p != 0 {
		t.Errorf("p50 %d, want 0", p)
	}
	if p, _ := h.Percentile(100); p != histMax {
		t.Errorf("p100 %d, want the cap", p)
	}
}

// Days merged from their encodings are the whole range's histogram.
func TestHistRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // G404: test data
	var all, merged Hist
	for range 5 {
		var day Hist
		for range 500 {
			v := r.Int64N(histMax * 2)
			day.Add(v, 2)
			all.Add(v, 2)
		}
		if err := merged.AddEncoded(day.Marshal()); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(all.classes, merged.classes) || all.n != merged.n {
		t.Error("merged days differ from the whole")
	}
	if err := merged.AddEncoded(nil); err != nil {
		t.Error(err)
	}
	before := merged.n
	for _, b := range [][]byte{{2, 1, 1}, {1, 0xff}, {1, 200, 200, 200, 1}, {1, 1}, {1, 1, 0}} {
		if merged.AddEncoded(b) == nil {
			t.Errorf("% x accepted", b)
		}
	}
	if merged.n != before {
		t.Error("a malformed histogram changed the counts")
	}
}
