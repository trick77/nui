package sim

import (
	"encoding/hex"
	"math"
	"math/rand/v2"
)

// rng is the seeded randomness of one part of the world: every actor-day draws from its own stream, derived from
// the world's seed and the part's coordinates, so what one actor does never depends on how many others there are or
// on how often the world is ticked.
type rng struct{ *rand.Rand }

// mix is splitmix64's finaliser: it spreads one coordinate over all 64 bits.
func mix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// newRng is the stream for seed and coordinates.
func newRng(seed uint64, parts ...int64) rng {
	h := mix(seed)
	for _, p := range parts {
		h = mix(h ^ bits(p))
	}
	return rng{rand.New(rand.NewPCG(h, mix(h)))} //nolint:gosec // synthetic traffic, reproducible by seed on purpose
}

// bits is a coordinate's bit pattern: a hash input, its sign is just another bit.
func bits(x int64) uint64 { return uint64(x) } //nolint:gosec // reinterpreted, never compared as a number

// chance is true with probability p.
func (r rng) chance(p float64) bool { return r.Float64() < p }

// fill sets b to random bytes.
func (r rng) fill(b []byte) {
	for i := range b {
		b[i] = byte(r.Uint32() >> 24) //nolint:gosec // the top 8 bits, in range by the shift
	}
}

// poisson draws a count with mean m (Knuth; the means here are small).
func (r rng) poisson(m float64) int {
	if m <= 0 {
		return 0
	}
	if m > 30 {
		return max(0, int(math.Round(m+math.Sqrt(m)*r.NormFloat64())))
	}
	l, k, p := math.Exp(-m), 0, 1.0
	for {
		p *= r.Float64()
		if p <= l {
			return k
		}
		k++
	}
}

// hex32 is a 32-hex-digit id, the tracker's visitor and session id shape.
func (r rng) hex32() string {
	var b [16]byte
	r.fill(b[:])
	return hex.EncodeToString(b[:])
}

// weighted picks an index by weight.
func (r rng) weighted(w []float64) int {
	var sum float64
	for _, x := range w {
		sum += x
	}
	x := r.Float64() * sum
	for i, v := range w {
		if x < v {
			return i
		}
		x -= v
	}
	return len(w) - 1
}

func pick[T any](r rng, xs []T) T { return xs[r.IntN(len(xs))] }

// strHash is a stable 64-bit hash of a string (FNV-1a), for coordinates named by text.
func strHash(s string) uint64 {
	h := uint64(0xcbf29ce484222325)
	for i := range len(s) {
		h ^= uint64(s[i])
		h *= 0x100000001b3
	}
	return h
}
