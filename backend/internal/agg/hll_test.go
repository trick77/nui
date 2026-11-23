package agg

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
)

// The hash and the encoding are permanent once raw events are purged: these pin both.
func TestHash_Pinned(t *testing.T) {
	for in, want := range map[string]uint64{
		"":                                 0xefd01f60ba992926,
		"192.0.2.1":                        0x8fc433616f773343,
		"2001:db8:1:2::/64":                0x07c35b4ee9bf5c0c,
		"0123456789abcdef0123456789abcdef": 0x26d521498b2056f3,
	} {
		if got := Hash(in); got != want {
			t.Errorf("Hash(%q) = %#x, want %#x", in, got, want)
		}
	}
}

func TestMarshal_Pinned(t *testing.T) {
	s := NewSketch()
	for _, id := range []string{"192.0.2.1", "192.0.2.2", "2001:db8::/64", "0123456789abcdef0123456789abcdef"} {
		s.AddString(id)
	}
	if got, want := hex.EncodeToString(s.Marshal()), "0181a1aa9b018092dea303c1bba8aa0180eaebbb01"; got != want {
		t.Errorf("Marshal = %s, want %s", got, want)
	}
	s.toDense()
	b := s.Marshal()
	if len(b) != 1+denseM || b[0] != encDense {
		t.Fatalf("dense marshal: %d bytes, version %d", len(b), b[0])
	}
	got := ""
	for i, r := range b[1:] {
		if r != 0 {
			got += fmt.Sprintf("%d:%d ", i, r)
		}
	}
	if want := "2485:2 9201:5 11926:4 14931:1 "; got != want {
		t.Errorf("dense registers = %q, want %q", got, want)
	}
}

func randomIDs(r *rand.Rand, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%016x%016x", r.Uint64(), r.Uint64())
	}
	return out
}

func relErr(got int64, n int) float64 { return math.Abs(float64(got)-float64(n)) / float64(n) }

// Sparse counts are exact but for index collisions (n²/2^26); dense ones about 1 % off.
func TestCount_ErrorBounds(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, n := range []int{1, 10, 1_000, 10_000, 100_000} {
		s := NewSketch()
		for _, id := range randomIDs(r, n) {
			s.AddString(id)
			s.AddString(id) // a repeat changes nothing
		}
		if s.Dense() {
			t.Fatalf("n=%d: dense", n)
		}
		if got := s.Count(); math.Abs(float64(got-int64(n))) > math.Max(1, 0.001*float64(n)) {
			t.Errorf("sparse n=%d: count %d", n, got)
		}
	}
	for _, n := range []int{5_000, 50_000, 200_000} {
		s := &Sketch{}
		s.toDense()
		for i := 0; i < n; i++ {
			s.Add(r.Uint64())
		}
		if got := s.Count(); relErr(got, n) > 0.03 {
			t.Errorf("dense n=%d: count %d", n, got)
		}
	}
	if got := NewSketch().Count(); got != 0 {
		t.Errorf("empty count = %d", got)
	}
}

// Collisions on the sparse index follow n²/2^26 for every id shape the tables count: browser ids, sequential IPv4
// addresses, sequential /64s.
func TestCount_Collisions(t *testing.T) {
	const n, trials = 10_000, 120
	want := float64(n) * float64(n) / float64(1<<26) // about 1.49 per trial
	r := rand.New(rand.NewPCG(3, 4))
	for _, shape := range []string{"hex32", "ipv4", "ipv6"} {
		var lost float64
		for range trials {
			s := NewSketch()
			base := r.Uint32()
			for i := 0; i < n; i++ {
				v := base + uint32(i)
				switch shape {
				case "hex32":
					s.AddString(fmt.Sprintf("%016x%016x", r.Uint64(), r.Uint64()))
				case "ipv4":
					s.AddString(fmt.Sprintf("%d.%d.%d.%d", v>>24, v>>16&255, v>>8&255, v&255))
				default:
					s.AddString(fmt.Sprintf("2001:db8:%x:%x::/64", v>>16, v&0xffff))
				}
			}
			lost += float64(n - len(s.sparse))
		}
		if got := lost / trials; got < want*0.6 || got > want*1.4 {
			t.Errorf("%s: %.2f collisions per trial, want about %.2f", shape, got, want)
		}
	}
}

func TestMarshal_RoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	if b := NewSketch().Marshal(); b != nil {
		t.Errorf("empty marshal = %x", b)
	}
	if s, err := Unmarshal(nil); err != nil || s.Count() != 0 {
		t.Errorf("unmarshal nil = %v, %v", s, err)
	}
	for _, n := range []int{1, 300, 20_000, 30_000} {
		s := NewSketch()
		for _, id := range randomIDs(r, n) {
			s.AddString(id)
		}
		b := s.Marshal()
		back, err := Unmarshal(b)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		// 20k ids stay sparse under the 64 KB bound (about 3 bytes each), 30k pass it and are stored dense.
		wantDense := n > 20_000
		if back.Dense() != wantDense || (b[0] == encDense) != wantDense {
			t.Errorf("n=%d: dense %v, %d bytes", n, back.Dense(), len(b))
		}
		if !wantDense {
			if back.Count() != s.Count() || len(back.sparse) != len(s.sparse) {
				t.Errorf("n=%d: count %d after round trip, %d before", n, back.Count(), s.Count())
			}
			for idx, rk := range s.sparse {
				if back.sparse[idx] != rk {
					t.Fatalf("n=%d: entry %d rank %d, want %d", n, idx, back.sparse[idx], rk)
				}
			}
		} else if relErr(back.Count(), n) > 0.03 {
			t.Errorf("n=%d: stored dense count %d", n, back.Count())
		}
		if again := back.Marshal(); string(again) != string(b) {
			t.Errorf("n=%d: second marshal differs", n)
		}
	}
}

func TestUnmarshal_Bad(t *testing.T) {
	for name, b := range map[string][]byte{
		"version":      {9, 1},
		"dense short":  append([]byte{encDense}, make([]byte, 10)...),
		"dense rank":   append(append([]byte{encDense}, make([]byte, denseM-1)...), maxRank+1),
		"truncated":    {encSparse, 0x80},
		"zero delta":   {encSparse, 0x41, 0x00},
		"zero rank":    {encSparse, 0x40},
		"rank past 40": {encSparse, 41},
		"index range":  {encSparse, 0x81, 0x80, 0x80, 0x80, 0x10},
	} {
		if _, err := Unmarshal(b); !errors.Is(err, ErrSketch) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A sparse sketch's entries land in the dense registers a dense sketch would have given the same hashes, so a
// merge of both forms counts the union.
func TestSparseToDense(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	hashes := make([]uint64, 60_000)
	for i := range hashes {
		hashes[i] = r.Uint64()
	}
	direct, viaSparse := &Sketch{}, NewSketch()
	direct.toDense()
	for _, h := range hashes {
		direct.Add(h)
		viaSparse.Add(h)
	}
	viaSparse.toDense()
	for i := range direct.dense {
		if direct.dense[i] != viaSparse.dense[i] {
			t.Fatalf("register %d: %d direct, %d via sparse", i, direct.dense[i], viaSparse.dense[i])
		}
	}

	// sparse a (0..40k) merged with dense b (20k..60k), both orders: the union is 60k.
	a, b := NewSketch(), &Sketch{}
	b.toDense()
	for _, h := range hashes[:40_000] {
		a.Add(h)
	}
	for _, h := range hashes[20_000:] {
		b.Add(h)
	}
	ab, ba := NewSketch(), NewSketch()
	ab.Merge(a)
	ab.Merge(b)
	ba.Merge(b)
	ba.Merge(a)
	for name, m := range map[string]*Sketch{"sparse+dense": ab, "dense+sparse": ba} {
		if !m.Dense() || relErr(m.Count(), 60_000) > 0.03 {
			t.Errorf("%s: dense %v, count %d", name, m.Dense(), m.Count())
		}
	}
	if string(ab.Marshal()) != string(ba.Marshal()) {
		t.Error("merge order changes the registers")
	}

	// Two sparse sketches stay sparse and count their union exactly; nil merges nothing.
	c, d := NewSketch(), NewSketch()
	for _, h := range hashes[:600] {
		c.Add(h)
	}
	for _, h := range hashes[400:1000] {
		d.Add(h)
	}
	c.Merge(d)
	c.Merge(nil)
	if c.Dense() || c.Count() != 1000 {
		t.Errorf("sparse union: dense %v, count %d", c.Dense(), c.Count())
	}
}

// Past memSparseMax entries (not ids: about 1.6 % of a million collide) a sketch turns dense in memory.
func TestSparseLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("a million adds")
	}
	r := rand.New(rand.NewPCG(9, 10))
	s := NewSketch()
	n := 0
	for ; n < 2*memSparseMax && !s.Dense(); n++ {
		if n == memSparseMax {
			// Linear counting still corrects the collisions just below the switch.
			if relErr(s.Count(), n) > 0.0005 {
				t.Errorf("sparse count %d of %d", s.Count(), n)
			}
		}
		s.Add(r.Uint64())
	}
	if !s.Dense() || n < memSparseMax || relErr(s.Count(), n) > 0.03 {
		t.Errorf("dense %v after %d ids, count %d", s.Dense(), n, s.Count())
	}
}
