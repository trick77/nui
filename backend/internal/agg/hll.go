// Package agg builds the aggregate tables (agg_hour, agg_list, agg_addr) from a site's raw events, one UTC day at a
// time, and holds the distinct-count sketch they store.
package agg

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
)

// The sketch is a HyperLogLog with an HLL++ sparse form. Its hash and both encodings are permanent once raw events
// are purged: a stored sketch is never rebuilt, so a change here needs a new encoding version byte and must still
// read the old one.
const (
	// sparseP is the sparse form's index bits: 2^25 slots, so a set of thousands is near-exact (collisions about
	// n²/2^26).
	sparseP = 25
	// denseP is the dense form's index bits: 16,384 one-byte registers, about 0.8 % error.
	denseP = 14
	denseM = 1 << denseP
	// maxRank bounds a register: 64 - denseP + 1.
	maxRank = 64 - denseP + 1
	// memSparseMax is the entries a sketch holds in memory before it turns dense (a merge over a long range).
	memSparseMax = 1 << 20
	// storedDenseAt is the sparse encoding's size from which a stored sketch is dense instead: only flood hours get
	// there, and they stay near-exact up to about 20k ids.
	storedDenseAt = 64 << 10

	encSparse = 1
	encDense  = 2
)

// Hash is the 64-bit hash every stored sketch and key hash is built on: FNV-1a, then murmur3's fmix64 (FNV alone
// leaves sequential addresses' high bits too alike for the index).
func Hash(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

// Sketch counts distinct hashes. The zero value is an empty sketch.
type Sketch struct {
	sparse map[uint32]uint8 // sparse index -> rank, while dense is nil
	dense  []uint8          // denseM registers once dense
}

// NewSketch returns an empty sketch.
func NewSketch() *Sketch { return &Sketch{} }

// Add counts one hash.
func (s *Sketch) Add(h uint64) {
	if s.dense != nil {
		i := h >> (64 - denseP)
		// The set bit below the shifted hash bounds the rank: at most 64 - denseP + 1.
		r := rank(bits.LeadingZeros64(h<<denseP | 1<<(denseP-1)))
		s.dense[i] = max(s.dense[i], r)
		return
	}
	idx := uint32(h >> (64 - sparseP))
	s.addSparse(idx, rank(bits.LeadingZeros64(h<<sparseP|1<<(sparseP-1))))
}

// rank is a register value: the leading zeros plus one.
func rank(zeros int) uint8 {
	return uint8(zeros + 1) //nolint:gosec // a bit count, at most 64
}

// AddString counts one id.
func (s *Sketch) AddString(id string) { s.Add(Hash(id)) }

func (s *Sketch) addSparse(idx uint32, r uint8) {
	if s.sparse == nil {
		s.sparse = map[uint32]uint8{}
	}
	if old, ok := s.sparse[idx]; !ok || r > old {
		s.sparse[idx] = r
	}
	if len(s.sparse) > memSparseMax {
		s.toDense()
	}
}

// toDenseEntry maps a sparse entry onto its dense register (HLL++): the dense index is the sparse index's top bits;
// the rank is counted in the index bits below them, or past them when those are all zero.
func toDenseEntry(idx uint32, r uint8) (uint32, uint8) {
	const low = sparseP - denseP
	i := idx >> low
	if rest := idx & (1<<low - 1); rest != 0 {
		return i, rank(bits.LeadingZeros32(rest) - (32 - low))
	}
	return i, low + r
}

func (s *Sketch) toDense() {
	s.dense = make([]uint8, denseM)
	for idx, r := range s.sparse {
		i, rr := toDenseEntry(idx, r)
		s.dense[i] = max(s.dense[i], rr)
	}
	s.sparse = nil
}

// Dense reports whether the sketch is in its dense form, where counts are about 1 % off instead of near-exact.
func (s *Sketch) Dense() bool { return s.dense != nil }

// Merge adds o's hashes to s. A dense side makes the result dense.
func (s *Sketch) Merge(o *Sketch) {
	if o == nil {
		return
	}
	if o.dense != nil {
		if s.dense == nil {
			s.toDense()
		}
		for i, r := range o.dense {
			s.dense[i] = max(s.dense[i], r)
		}
		return
	}
	for idx, r := range o.sparse {
		if s.dense != nil {
			i, rr := toDenseEntry(idx, r)
			s.dense[i] = max(s.dense[i], rr)
			continue
		}
		s.addSparse(idx, r)
	}
}

// Count is the estimated number of distinct hashes: linear counting over the sparse slots (exact but for index
// collisions), Ertl's improved estimator once dense.
func (s *Sketch) Count() int64 {
	if s.dense != nil {
		return int64(math.Round(ertl(s.dense)))
	}
	m, v := float64(1<<sparseP), float64(len(s.sparse))
	return int64(math.Round(m * math.Log(m/(m-v))))
}

// ertl is Ertl's improved raw estimator (2017): no bias tables, good from a handful of ids on.
func ertl(reg []uint8) float64 {
	const q = 64 - denseP
	var c [q + 2]float64
	for _, r := range reg {
		c[min(int(r), q+1)]++
	}
	m := float64(len(reg))
	z := m * tau(1-c[q+1]/m)
	for k := q; k >= 1; k-- {
		z = 0.5 * (z + c[k])
	}
	z += m * sigma(c[0]/m)
	return m * m / (2 * math.Ln2) / z
}

func sigma(x float64) float64 {
	if x == 1 {
		return math.Inf(1)
	}
	y, z := 1.0, x
	for {
		x *= x
		zo := z
		z += x * y
		y += y
		if z == zo {
			return z
		}
	}
}

func tau(x float64) float64 {
	if x == 0 || x == 1 {
		return 0
	}
	y, z := 1.0, 1-x
	for {
		x = math.Sqrt(x)
		zo := z
		y *= 0.5
		z -= (1 - x) * (1 - x) * y
		if z == zo {
			return z / 3
		}
	}
}

// Marshal encodes the sketch: nil when empty; a version byte 1 and the sorted sparse entries (index<<6 | rank) as
// uvarint deltas; or, dense or with a sparse encoding past storedDenseAt, a version byte 2 and the registers.
func (s *Sketch) Marshal() []byte {
	if s == nil || (s.dense == nil && len(s.sparse) == 0) {
		return nil
	}
	if s.dense != nil {
		return append([]byte{encDense}, s.dense...)
	}
	vals := make([]uint32, 0, len(s.sparse))
	for idx, r := range s.sparse {
		vals = append(vals, idx<<6|uint32(r))
	}
	slices.Sort(vals)
	out := make([]byte, 1, 1+3*len(vals))
	out[0] = encSparse
	prev := uint32(0)
	for _, v := range vals {
		out = binary.AppendUvarint(out, uint64(v-prev))
		prev = v
		if len(out) > storedDenseAt {
			d := &Sketch{sparse: s.sparse}
			d.toDense()
			return append([]byte{encDense}, d.dense...)
		}
	}
	return out
}

// ErrSketch is a stored sketch that does not decode.
var ErrSketch = errors.New("bad sketch encoding")

// Unmarshal decodes what Marshal wrote; nil or empty is an empty sketch.
func Unmarshal(b []byte) (*Sketch, error) {
	s := &Sketch{}
	if len(b) == 0 {
		return s, nil
	}
	switch b[0] {
	case encDense:
		if len(b) != 1+denseM {
			return nil, fmt.Errorf("%w: dense length %d", ErrSketch, len(b)-1)
		}
		for _, r := range b[1:] {
			if r > maxRank {
				return nil, fmt.Errorf("%w: dense register %d", ErrSketch, r)
			}
		}
		s.dense = append([]uint8(nil), b[1:]...)
		return s, nil
	case encSparse:
	default:
		return nil, fmt.Errorf("%w: version %d", ErrSketch, b[0])
	}
	s.sparse = make(map[uint32]uint8, len(b)/3)
	var v uint64
	for i := 1; i < len(b); {
		d, n := binary.Uvarint(b[i:])
		if n <= 0 || d == 0 {
			return nil, fmt.Errorf("%w: sparse entry at byte %d", ErrSketch, i)
		}
		i += n
		v += d
		idx, r := v>>6, uint8(v&63)
		if idx >= 1<<sparseP || r == 0 || r > 64-sparseP+1 {
			return nil, fmt.Errorf("%w: sparse entry %d", ErrSketch, v)
		}
		s.addSparse(uint32(idx), r)
	}
	return s, nil
}
